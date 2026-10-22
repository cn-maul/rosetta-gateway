package upstream

import (
	"context"
	"net/http"
	"net/url"
	"strings"
)

// OpenCodeSessionHeader is required by OpenCode Go for chat requests. The
// value should identify one conversation and stay stable across its turns.
const OpenCodeSessionHeader = "x-opencode-session"

// rosetta-gateway is a real client identity; OpenCode explicitly asks callers
// not to identify as a generic HTTP library or SDK.
const openCodeUserAgent = "rosetta-gateway"

type openCodeSessionContextKey struct{}

// WithOpenCodeSession attaches the downstream conversation ID to contexts
// used by upstream requests. The transport only emits it for OpenCode hosts.
func WithOpenCodeSession(ctx context.Context, sessionID string) context.Context {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return ctx
	}
	return context.WithValue(ctx, openCodeSessionContextKey{}, sessionID)
}

// OpenCodeSessionFromContext returns the conversation ID attached for an
// OpenCode upstream request.
func OpenCodeSessionFromContext(ctx context.Context) string {
	sessionID, _ := ctx.Value(openCodeSessionContextKey{}).(string)
	return strings.TrimSpace(sessionID)
}

// withOpenCodeSessionTransport clones the client so proxy settings, timeout,
// redirect policy and the underlying connection pool stay intact. Requests to
// non-OpenCode endpoints are deliberately left untouched.
func withOpenCodeSessionTransport(client *http.Client, endpoint string) *http.Client {
	if client == nil || !isOpenCodeEndpoint(endpoint) {
		return client
	}

	base := client.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	cloned := *client
	cloned.Transport = &openCodeSessionTransport{base: base}
	return &cloned
}

type openCodeSessionTransport struct {
	base http.RoundTripper
}

func (t *openCodeSessionTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req == nil || req.URL == nil || !isOpenCodeHost(req.URL.Hostname()) {
		return t.base.RoundTrip(req)
	}

	// RoundTrippers must not mutate the caller's request. Clone also gives us
	// an independent Header map before adding the session.
	cloned := req.Clone(req.Context())
	if cloned.Header == nil {
		cloned.Header = make(http.Header)
	}
	if cloned.Header.Get(OpenCodeSessionHeader) == "" {
		if sessionID := OpenCodeSessionFromContext(req.Context()); sessionID != "" {
			cloned.Header.Set(OpenCodeSessionHeader, sessionID)
		}
	}
	cloned.Header.Set("User-Agent", openCodeUserAgent)
	return t.base.RoundTrip(cloned)
}

func isOpenCodeEndpoint(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return false
	}
	return isOpenCodeHost(u.Hostname())
}

func isOpenCodeHost(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
	return host == "opencode.ai" || strings.HasSuffix(host, ".opencode.ai")
}
