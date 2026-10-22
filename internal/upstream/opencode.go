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
type promptCacheKeyContextKey struct{}

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

// WithPromptCacheKey attaches the client's prompt-cache affinity key. The
// transport emits it only to OpenAI-family upstreams, where it is routing
// metadata rather than conversation content.
func WithPromptCacheKey(ctx context.Context, key string) context.Context {
	key = strings.TrimSpace(key)
	if key == "" {
		return ctx
	}
	return context.WithValue(ctx, promptCacheKeyContextKey{}, key)
}

// PromptCacheKeyFromContext returns the prompt-cache affinity key, if any.
func PromptCacheKeyFromContext(ctx context.Context) string {
	key, _ := ctx.Value(promptCacheKeyContextKey{}).(string)
	return strings.TrimSpace(key)
}

// withConversationAffinityTransport clones the client so proxy settings,
// timeout, redirect policy and the underlying connection pool stay intact.
// Anthropic has its own cache_control contract and must not receive OpenAI's
// routing headers. An empty or "auto" protocol is treated as OpenAI-family.
func withConversationAffinityTransport(client *http.Client, endpoint, protocol, providerSlug string) *http.Client {
	if client == nil || protocol == "anthropic" {
		return client
	}

	base := client.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	cloned := *client
	cloned.Transport = &conversationAffinityTransport{
		base:     base,
		openCode: isOpenCodeEndpoint(endpoint) || strings.Contains(strings.ToLower(providerSlug), "opencode"),
	}
	return &cloned
}

type conversationAffinityTransport struct {
	base     http.RoundTripper
	openCode bool
}

func (t *conversationAffinityTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req == nil || req.URL == nil {
		return t.base.RoundTrip(req)
	}

	// RoundTrippers must not mutate the caller's request. Clone also gives us
	// an independent Header map before adding affinity metadata.
	cloned := req.Clone(req.Context())
	if cloned.Header == nil {
		cloned.Header = make(http.Header)
	}
	isOpenCode := t.openCode && isOpenCodeHost(req.URL.Hostname())
	if isOpenCode {
		if cloned.Header.Get(OpenCodeSessionHeader) == "" {
			if sessionID := OpenCodeSessionFromContext(req.Context()); sessionID != "" {
				cloned.Header.Set(OpenCodeSessionHeader, sessionID)
			}
		}
		cloned.Header.Set("User-Agent", openCodeUserAgent)
	}
	if cacheKey := PromptCacheKeyFromContext(req.Context()); cacheKey != "" {
		// Keep caller-provided headers intact; these are fallbacks for clients
		// that identify a conversation by header rather than prompt_cache_key.
		if cloned.Header.Get("x-session-affinity") == "" {
			cloned.Header.Set("x-session-affinity", cacheKey)
		}
		if cloned.Header.Get("x-client-request-id") == "" {
			cloned.Header.Set("x-client-request-id", cacheKey)
		}
	}
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
