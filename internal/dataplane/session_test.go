package dataplane

import (
	"net/http/httptest"
	"testing"

	"github.com/cn-maul/rosetta-gateway/internal/httpx"
	"github.com/cn-maul/rosetta-gateway/internal/upstream"
)

func TestWithOpenCodeSessionPrefersIncomingHeader(t *testing.T) {
	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	req = req.WithContext(httpx.WithRequestID(req.Context(), "request-42"))
	req.Header.Set(upstream.OpenCodeSessionHeader, "conversation-7")

	got := withOpenCodeSession(req)
	if session := upstream.OpenCodeSessionFromContext(got.Context()); session != "conversation-7" {
		t.Fatalf("session = %q, want conversation-7", session)
	}
}

func TestWithOpenCodeSessionFallsBackToRequestID(t *testing.T) {
	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	req = req.WithContext(httpx.WithRequestID(req.Context(), "request-42"))

	got := withOpenCodeSession(req)
	if session := upstream.OpenCodeSessionFromContext(got.Context()); session != "request-42" {
		t.Fatalf("session = %q, want request-42", session)
	}
}
