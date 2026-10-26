package upstream

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/cn-maul/rosetta-gateway/internal/store"
)

// R6-M06-01: a known hard limit does not make the used amount known. A failed
// usage lookup must be exposed as unknown, never as an exact "hard limit - 0".
func TestR6_OpenAIBalanceDoesNotInventZeroUsed(t *testing.T) {
	cases := []struct {
		name      string
		usage     func(http.ResponseWriter)
		timeout   time.Duration
		wantInMsg string
	}{
		{
			name: "usage non-200",
			usage: func(w http.ResponseWriter) {
				w.WriteHeader(http.StatusServiceUnavailable)
			},
			wantInMsg: "HTTP 503",
		},
		{
			name: "usage malformed JSON",
			usage: func(w http.ResponseWriter) {
				fmt.Fprint(w, `{"total_usage":`)
			},
			wantInMsg: "解析",
		},
		{
			name: "usage missing total_usage",
			usage: func(w http.ResponseWriter) {
				fmt.Fprint(w, `{"object":"list"}`)
			},
			wantInMsg: "total_usage",
		},
		{
			name: "usage timeout",
			usage: func(w http.ResponseWriter) {
				time.Sleep(2 * time.Second)
			},
			timeout:   40 * time.Millisecond,
			wantInMsg: "deadline",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/dashboard/billing/subscription":
					fmt.Fprint(w, `{"hard_limit_usd":100}`)
				case "/dashboard/billing/usage":
					tc.usage(w)
				default:
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer srv.Close()

			st, pid, master := balanceFixture(t)
			setEndpoint(t, st, pid, srv.URL)
			asDialect(t, func(ctx context.Context, p *store.Provider, key string) (balanceReading, error) {
				return fetchOpenAIBalance(ctx, p, key)
			})

			ctx := context.Background()
			if tc.timeout > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, tc.timeout)
				defer cancel()
			}
			res, err := QueryProviderBalance(ctx, st, pid, master)
			if err != nil {
				t.Fatalf("query: %v", err)
			}
			if len(res.Results) != 1 {
				t.Fatalf("results = %+v", res.Results)
			}
			r := res.Results[0]
			if r.Status != "unknown" {
				t.Fatalf("status = %q, want unknown (amount=%v detail=%q)", r.Status, r.Amount, r.Detail)
			}
			if r.Amount != 0 {
				t.Fatalf("unknown usage exposed a precise amount: %v", r.Amount)
			}
			if r.FetchedAt == 0 {
				t.Fatalf("unknown result lacks fetch time: %+v", r)
			}
			if !containsAny(r.Message, tc.wantInMsg) && !containsAny(r.Detail, tc.wantInMsg) {
				t.Fatalf("failure reason %q missing from message=%q detail=%q", tc.wantInMsg, r.Message, r.Detail)
			}
			if res.Status != "unknown" {
				t.Fatalf("provider status = %q, want unknown", res.Status)
			}
		})
	}
}

// R6-M06-02: discovery must authenticate with the protocol selected by the same
// auto-detection path used by the data plane.
func TestR6_AnthropicDiscoveryUsesResolvedProtocolAuth(t *testing.T) {
	type requestAuth struct {
		path          string
		apiKey        string
		authorization string
		version       string
	}

	for _, configured := range []string{"auto", "anthropic"} {
		t.Run(configured, func(t *testing.T) {
			var mu sync.Mutex
			var requests []requestAuth
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				requests = append(requests, requestAuth{
					path:          r.URL.Path,
					apiKey:        r.Header.Get("x-api-key"),
					authorization: r.Header.Get("Authorization"),
					version:       r.Header.Get("anthropic-version"),
				})
				mu.Unlock()
				if r.Header.Get("x-api-key") != "sk-secret" {
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, `{"data":[{"type":"model","id":"claude-test"}]}`)
			}))
			defer srv.Close()

			st, pid, _ := balanceFixture(t)
			setEndpoint(t, st, pid, srv.URL+"/v1")
			p, err := st.GetProvider(context.Background(), pid)
			if err != nil || p == nil {
				t.Fatalf("get provider: %v", err)
			}
			p.Protocol = configured
			if err := st.UpdateProvider(context.Background(), pid, p); err != nil {
				t.Fatalf("update provider protocol: %v", err)
			}

			cands, err := DiscoverUpstreamModels(
				context.Background(), st, pid,
				[]byte("0123456789abcdef0123456789abcdef"),
				newTestConfig(),
			)
			if err != nil {
				t.Fatalf("discover: %v", err)
			}
			if len(cands) != 1 || cands[0].ID != "claude-test" {
				t.Fatalf("candidates = %+v", cands)
			}

			mu.Lock()
			defer mu.Unlock()
			if len(requests) == 0 {
				t.Fatal("no discovery requests recorded")
			}
			last := requests[len(requests)-1]
			if last.apiKey != "sk-secret" || last.authorization != "" {
				t.Fatalf("discovery auth = x-api-key %q / Authorization %q; want x-api-key only", last.apiKey, last.authorization)
			}
			if last.version != "2023-06-01" {
				t.Fatalf("anthropic-version = %q, want 2023-06-01", last.version)
			}
		})
	}
}

func containsAny(s, want string) bool {
	return len(want) == 0 || (len(s) >= len(want) && containsFold(s, want))
}

func containsFold(s, want string) bool {
	// This test only needs ASCII diagnostics and avoids importing strings solely
	// for case-insensitive failure matching.
	for i := 0; i+len(want) <= len(s); i++ {
		ok := true
		for j := range want {
			a, b := s[i+j], want[j]
			if a >= 'A' && a <= 'Z' {
				a += 'a' - 'A'
			}
			if b >= 'A' && b <= 'Z' {
				b += 'a' - 'A'
			}
			if a != b {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}
