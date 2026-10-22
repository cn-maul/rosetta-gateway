package dataplane

import (
	"context"
	"strings"

	"github.com/cn-maul/rosetta-gateway/internal/ingress"
	"github.com/cn-maul/rosetta-gateway/internal/routing"
	"github.com/cn-maul/rosetta-gateway/internal/upstream"
)

// withPromptCacheAffinity binds one cache-affinity key to the request context.
//
// The identity comes from the client's explicit prompt_cache_key when present;
// otherwise it is the same conversation ID already chosen for OpenCode. The
// client-provided value wins because it is the only cache key that can be
// shared with a client that sends cache hints independently of session IDs.
func withPromptCacheAffinity(
	ctx context.Context,
	ing *ingress.Request,
	cand routing.Candidate,
	stableSessionID string,
) context.Context {
	if ing == nil || cand.Provider.Protocol == "anthropic" {
		return ctx
	}
	key := strings.TrimSpace(ing.CacheKey)
	if key == "" {
		key = strings.TrimSpace(stableSessionID)
	}
	if key == "" {
		return ctx
	}
	return upstream.WithPromptCacheKey(ctx, key)
}
