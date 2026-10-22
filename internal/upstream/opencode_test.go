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
	client := &http.Client{Transport: &openCodeSessionTransport{base: base}}
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
	client := &http.Client{Transport: &openCodeSessionTransport{base: base}}
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
	client := &http.Client{Transport: &openCodeSessionTransport{base: base}}
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

func TestWithOpenCodeSessionTransportOnlyWrapsOpenCodeEndpoints(t *testing.T) {
	base := &http.Client{}
	for _, endpoint := range []string{
		"https://opencode.ai/zen/go/v1",
		"https://api.opencode.ai/v1",
	} {
		got := withOpenCodeSessionTransport(base, endpoint)
		if got == base {
			t.Fatalf("endpoint %q was not wrapped", endpoint)
		}
		if _, ok := got.Transport.(*openCodeSessionTransport); !ok {
			t.Fatalf("endpoint %q transport = %T", endpoint, got.Transport)
		}
	}

	for _, endpoint := range []string{
		"https://example.com/v1",
		"https://opencode.ai.example.com/v1",
		"://invalid",
	} {
		if got := withOpenCodeSessionTransport(base, endpoint); got != base {
			t.Fatalf("endpoint %q should keep the original client", endpoint)
		}
	}
}
