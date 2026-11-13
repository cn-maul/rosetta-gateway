package upstream

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/cn-maul/rosetta"
	"github.com/cn-maul/rosetta-gateway/internal/crypto"
	"github.com/cn-maul/rosetta-gateway/internal/store"
)

func balanceFixture(t *testing.T) (*store.Store, string, []byte) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	st, err := store.Open(t.TempDir()+"/r6.db", logger)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	master := []byte("0123456789abcdef0123456789abcdef")
	p := &store.Provider{
		ID: "p1", Slug: "p1", Name: "P1", Protocol: "chat",
		Endpoint: "http://127.0.0.1:0", Enabled: true,
	}
	if err := st.CreateProvider(context.Background(), p); err != nil {
		t.Fatalf("create provider: %v", err)
	}
	enc, err := crypto.Encrypt([]byte("sk-secret"), master)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	cred := &store.Credential{
		ID: "c1", ProviderID: "p1", Label: "主号",
		APIKeyEnc: enc, Enabled: true, Weight: 1, Status: "healthy",
	}
	if err := st.CreateCredential(context.Background(), cred); err != nil {
		t.Fatalf("create credential: %v", err)
	}
	return st, p.ID, master
}

func setEndpoint(t *testing.T, st *store.Store, id, endpoint string) {
	t.Helper()
	p, err := st.GetProvider(context.Background(), id)
	if err != nil || p == nil {
		t.Fatalf("get provider: %v", err)
	}
	p.Endpoint = endpoint
	if err := st.UpdateProvider(context.Background(), id, p); err != nil {
		t.Fatalf("update provider: %v", err)
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
			if last.version != rosetta.AnthropicVersion {
				t.Fatalf("anthropic-version = %q, want %s", last.version, rosetta.AnthropicVersion)
			}
		})
	}
}
