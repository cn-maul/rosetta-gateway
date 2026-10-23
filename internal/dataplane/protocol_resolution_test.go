package dataplane

import (
	"log/slog"
	"testing"

	"github.com/cn-maul/rosetta-gateway/internal/routing"
	"github.com/cn-maul/rosetta-gateway/internal/upstream"
)

func TestEffectiveProviderProtocolUsesResolvedAutoValue(t *testing.T) {
	pool := upstream.NewPool(slog.Default())
	pool.Install(map[string]*upstream.ProviderEntry{
		"detected": {Slug: "detected", Protocol: "anthropic"},
	}, upstream.LiveTargets{Known: true})
	h := &Handler{pool: pool}

	detected := routing.Candidate{Provider: &routing.ProviderRef{
		Slug: "detected", Protocol: "auto",
	}}
	if got := h.effectiveProviderProtocol(detected); got != "anthropic" {
		t.Fatalf("auto protocol = %q, want anthropic", got)
	}

	fallback := routing.Candidate{Provider: &routing.ProviderRef{
		Slug: "not-built", Protocol: "openai-chat",
	}}
	if got := h.effectiveProviderProtocol(fallback); got != "chat" {
		t.Fatalf("fallback protocol = %q, want chat", got)
	}

	if got := (&Handler{}).effectiveProviderProtocol(routing.Candidate{}); got != "" {
		t.Fatalf("nil provider protocol = %q, want empty", got)
	}
}
