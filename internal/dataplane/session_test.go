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
	req.Header.Set("x-deepseek-harness-session-id", "harness-8")

	got := withOpenCodeSession(req)
	if session := upstream.OpenCodeSessionFromContext(got.Context()); session != "conversation-7" {
		t.Fatalf("session = %q, want conversation-7", session)
	}
}

func TestWithOpenCodeSessionUsesDeepSeekHarnessSession(t *testing.T) {
	req := httptest.NewRequest("POST", "/v1/messages", nil)
	req = req.WithContext(httpx.WithRequestID(req.Context(), "request-42"))
	req.Header.Set("x-deepseek-harness-session-id", "harness-8")

	got := withOpenCodeSession(req)
	if session := upstream.OpenCodeSessionFromContext(got.Context()); session != "harness-8" {
		t.Fatalf("session = %q, want harness-8", session)
	}
}

func TestWithOpenCodeSessionUsesOpenAISessionAffinityHeaders(t *testing.T) {
	for _, header := range []string{"x-session-affinity", "session_id", "x-client-request-id", "x-session-id", "session-id"} {
		t.Run(header, func(t *testing.T) {
			req := httptest.NewRequest("POST", "/v1/responses", nil)
			req = req.WithContext(httpx.WithRequestID(req.Context(), "request-42"))
			req.Header.Set(header, "conversation-9")

			got := withOpenCodeSession(req)
			if session := upstream.OpenCodeSessionFromContext(got.Context()); session != "conversation-9" {
				t.Fatalf("%s session = %q, want conversation-9", header, session)
			}
		})
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
