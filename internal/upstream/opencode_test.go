package upstream

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

type recordingRoundTripper struct {
	header http.Header
}

func (r *recordingRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	r.header = req.Header.Clone()
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader("")),
		Request:    req,
	}, nil
}

func TestOpenCodeSessionTransportInjectsContextSession(t *testing.T) {
	base := &recordingRoundTripper{}
	client := &http.Client{Transport: &conversationAffinityTransport{base: base, openCode: true}}
	ctx := WithOpenCodeSession(context.Background(), "conversation-42")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://opencode.ai/zen/go/v1/chat/completions", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}

	if _, err := client.Do(req); err != nil {
		t.Fatalf("do request: %v", err)
	}
	if got := base.header.Get(OpenCodeSessionHeader); got != "conversation-42" {
		t.Fatalf("%s = %q, want conversation-42", OpenCodeSessionHeader, got)
	}
	if got := base.header.Get("User-Agent"); got != openCodeUserAgent {
		t.Fatalf("User-Agent = %q, want %q", got, openCodeUserAgent)
	}
	if got := req.Header.Get(OpenCodeSessionHeader); got != "" {
		t.Fatalf("transport mutated caller request header: %q", got)
	}
}

func TestOpenCodeSessionTransportPreservesExistingHeader(t *testing.T) {
	base := &recordingRoundTripper{}
	client := &http.Client{Transport: &conversationAffinityTransport{base: base, openCode: true}}
	ctx := WithOpenCodeSession(context.Background(), "context-session")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://opencode.ai/zen/go/v1/chat/completions", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set(OpenCodeSessionHeader, "caller-session")

	if _, err := client.Do(req); err != nil {
		t.Fatalf("do request: %v", err)
	}
	if got := base.header.Get(OpenCodeSessionHeader); got != "caller-session" {
		t.Fatalf("%s = %q, want caller-session", OpenCodeSessionHeader, got)
	}
}

func TestOpenCodeSessionTransportDoesNotLeakToOtherHosts(t *testing.T) {
	base := &recordingRoundTripper{}
	client := &http.Client{Transport: &conversationAffinityTransport{base: base, openCode: true}}
	ctx := WithOpenCodeSession(context.Background(), "conversation-42")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://example.com/v1/chat/completions", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}

	if _, err := client.Do(req); err != nil {
		t.Fatalf("do request: %v", err)
	}
	if got := base.header.Get(OpenCodeSessionHeader); got != "" {
		t.Fatalf("%s leaked to non-OpenCode host: %q", OpenCodeSessionHeader, got)
	}
	if got := base.header.Get("User-Agent"); got != "" {
		t.Fatalf("User-Agent changed for non-OpenCode host: %q", got)
	}
}

func TestConversationAffinityTransportAdvertisesOpenCodeOnlyForOpenCodeEndpoints(t *testing.T) {
	base := &http.Client{}
	for _, tc := range []struct{ endpoint, slug string }{
		{"https://opencode.ai/zen/go/v1", ""},
		{"https://api.opencode.ai/v1", ""},
		// 自建反代不会用 opencode.ai 域名，靠 slug 认出来。
		{"https://gateway.internal/v1", "opencode-go"},
	} {
		got := withConversationAffinityTransport(base, tc.endpoint, "chat", tc.slug)
		if got == base {
			t.Fatalf("endpoint %q was not wrapped", tc.endpoint)
		}
		transport, ok := got.Transport.(*conversationAffinityTransport)
		if !ok {
			t.Fatalf("endpoint %q transport = %T", tc.endpoint, got.Transport)
		}
		if !transport.openCode {
			t.Fatalf("endpoint %q was not marked as OpenCode", tc.endpoint)
		}
	}

	for _, tc := range []struct{ endpoint, slug string }{
		{"https://example.com/v1", ""},
		{"https://opencode.ai.example.com/v1", ""},
		{"://invalid", ""},
		{"https://gateway.internal/v1", "deepseek"},
	} {
		got := withConversationAffinityTransport(base, tc.endpoint, "response", tc.slug)
		transport, ok := got.Transport.(*conversationAffinityTransport)
		if !ok || transport.openCode {
			t.Fatalf("endpoint %q should use non-OpenCode affinity transport, got %#v", tc.endpoint, got.Transport)
		}
	}
}

func TestConversationAffinityTransportSendsCacheAffinityToOpenAIHosts(t *testing.T) {
	base := &recordingRoundTripper{}
	client := &http.Client{Transport: &conversationAffinityTransport{base: base}}
	ctx := WithPromptCacheKey(context.Background(), "session-42")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://example.com/v1/responses", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}

	if _, err := client.Do(req); err != nil {
		t.Fatalf("do request: %v", err)
	}
	if got := base.header.Get("x-session-affinity"); got != "session-42" {
		t.Fatalf("x-session-affinity = %q, want session-42", got)
	}
	if got := base.header.Get("x-client-request-id"); got != "session-42" {
		t.Fatalf("x-client-request-id = %q, want session-42", got)
	}
	if got := base.header.Get(OpenCodeSessionHeader); got != "" {
		t.Fatalf("%s leaked to non-OpenCode host: %q", OpenCodeSessionHeader, got)
	}
	if got := req.Header.Get("x-session-affinity"); got != "" {
		t.Fatalf("transport mutated caller request header: %q", got)
	}
}

func TestConversationAffinityTransportKeepsCallerAffinityHeaders(t *testing.T) {
	base := &recordingRoundTripper{}
	client := &http.Client{Transport: &conversationAffinityTransport{base: base}}
	ctx := WithPromptCacheKey(context.Background(), "gateway-key")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://example.com/v1/responses", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("x-session-affinity", "caller-affinity")
	req.Header.Set("x-client-request-id", "caller-request")

	if _, err := client.Do(req); err != nil {
		t.Fatalf("do request: %v", err)
	}
	if got := base.header.Get("x-session-affinity"); got != "caller-affinity" {
		t.Fatalf("x-session-affinity = %q, want caller-affinity", got)
	}
	if got := base.header.Get("x-client-request-id"); got != "caller-request" {
		t.Fatalf("x-client-request-id = %q, want caller-request", got)
	}
}

func TestConversationAffinityTransportSkipsAnthropic(t *testing.T) {
	base := &http.Client{}
	if got := withConversationAffinityTransport(base, "https://api.anthropic.com/v1", "anthropic", "anthropic"); got != base {
		t.Fatalf("Anthropic client should keep the original transport")
	}
}
