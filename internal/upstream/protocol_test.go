package upstream

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/cn-maul/rosetta"
)

func TestBuildClient_AutoDetectsAnthropicProtocol(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("probe path = %q, want /v1/models", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[{"type":"model","id":"claude-test"}]}`)
	}))
	defer srv.Close()

	prov := &ProviderEntry{
		Slug:       "auto-anthropic",
		Endpoint:   srv.URL + "/v1",
		Protocol:   "auto",
		MaxRetries: 0,
	}
	client, err := buildClient(context.Background(), prov, "sk-test", newTestConfig())
	if err != nil {
		t.Fatalf("buildClient: %v", err)
	}
	if client.Protocol() != rosetta.ProtoAnthropic {
		t.Fatalf("detected protocol = %q, want anthropic", client.Protocol())
	}
	if prov.Protocol != "anthropic" {
		t.Fatalf("provider protocol = %q, want anthropic", prov.Protocol)
	}
}

func TestBuildClient_AutoProbeFailureFallsBackToChat(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	prov := &ProviderEntry{
		Slug:       "auto-fallback",
		Endpoint:   "http://127.0.0.1:1/v1",
		Protocol:   "auto",
		MaxRetries: 0,
	}
	client, err := buildClient(ctx, prov, "sk-test", newTestConfig())
	if err != nil {
		t.Fatalf("probe failure must not fail client construction: %v", err)
	}
	if client.Protocol() != rosetta.ProtoOpenAIChat {
		t.Fatalf("fallback protocol = %q, want chat", client.Protocol())
	}
	if prov.Protocol != "chat" {
		t.Fatalf("provider protocol = %q, want chat", prov.Protocol)
	}
}

func TestBuildClient_AutoProbeHonorsProviderTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(2 * time.Second):
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"data":[]}`)
		case <-r.Context().Done():
			return
		}
	}))
	defer srv.Close()

	prov := &ProviderEntry{
		Slug:       "auto-timeout",
		Endpoint:   srv.URL + "/v1",
		Protocol:   "auto",
		Timeout:    40 * time.Millisecond,
		MaxRetries: 0,
	}
	start := time.Now()
	client, err := buildClient(context.Background(), prov, "sk-test", newTestConfig())
	if err != nil {
		t.Fatalf("buildClient: %v", err)
	}
	if elapsed := time.Since(start); elapsed >= time.Second {
		t.Fatalf("auto probe ignored provider timeout: elapsed %s", elapsed)
	}
	if client.Protocol() != rosetta.ProtoOpenAIChat || prov.Protocol != "chat" {
		t.Fatalf("timeout fallback = %q / provider %q, want chat", client.Protocol(), prov.Protocol)
	}
}

func TestPoolResolvedProtocol(t *testing.T) {
	pool := NewPool(slog.Default())
	pool.Install(map[string]*ProviderEntry{
		"detected": {Slug: "detected", Protocol: "anthropic"},
		"legacy":   {Slug: "legacy", Protocol: "openai-chat"},
	}, LiveTargets{Known: true})

	if got := pool.ResolvedProtocol("detected"); got != "anthropic" {
		t.Fatalf("detected protocol = %q, want anthropic", got)
	}
	if got := pool.ResolvedProtocol("legacy"); got != "chat" {
		t.Fatalf("legacy protocol = %q, want chat", got)
	}
	if got := pool.ResolvedProtocol("missing"); got != "" {
		t.Fatalf("missing protocol = %q, want empty", got)
	}
}

func TestBuildClient_ExplicitProtocolsAndLegacyAliases(t *testing.T) {
	tests := []struct {
		configured string
		want       rosetta.Protocol
	}{
		{"chat", rosetta.ProtoOpenAIChat},
		{"response", rosetta.ProtoOpenAIResponses},
		{"anthropic", rosetta.ProtoAnthropic},
		{"openai-chat", rosetta.ProtoOpenAIChat},
		{"openai-responses", rosetta.ProtoOpenAIResponses},
	}
	for _, tc := range tests {
		t.Run(tc.configured, func(t *testing.T) {
			prov := &ProviderEntry{
				Slug:       tc.configured,
				Endpoint:   "https://example.invalid/v1",
				Protocol:   tc.configured,
				MaxRetries: 0,
			}
			client, err := buildClient(context.Background(), prov, "sk-test", newTestConfig())
			if err != nil {
				t.Fatalf("buildClient: %v", err)
			}
			if got := client.Protocol(); got != tc.want {
				t.Fatalf("protocol = %q, want %q", got, tc.want)
			}
		})
	}
}
