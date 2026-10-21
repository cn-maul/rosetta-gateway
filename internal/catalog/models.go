// Package catalog serves the public model directory endpoints.
package catalog

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/cn-maul/rosetta-gateway/internal/auth"
	"github.com/cn-maul/rosetta-gateway/internal/authhttp"
	"github.com/cn-maul/rosetta-gateway/internal/dataplane"
	"github.com/cn-maul/rosetta-gateway/internal/outwire"
	"github.com/cn-maul/rosetta-gateway/internal/routing"
	"github.com/cn-maul/rosetta-gateway/internal/snapshot"
)

// List returns GET /v1/models and its protocol-specific aliases.
func List(forceShape string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		shape := responseShape(r, forceShape)
		writeErr := authhttp.ErrorWriter(outwire.WriteOpenAIError)
		if shape == "anthropic" {
			writeErr = outwire.WriteAnthropicError
		}

		authCtx, err := auth.Authenticate(r)
		if err != nil {
			authhttp.WriteAuthError(w, err, writeErr)
			return
		}

		snap := snapshot.Get()
		ids := make([]string, 0, 16)
		seen := make(map[string]bool)
		for _, route := range snap.Routes.ListRoutes() {
			if route.Enabled && authCtx.AllowsModel(route.PublicName) && !seen[route.PublicName] {
				seen[route.PublicName] = true
				ids = append(ids, route.PublicName)
			}
		}
		if r.URL.Query().Get("include") == "upstream" {
			for _, m := range snap.Routes.ListUpstreamModels() {
				id := m.ProviderSlug + "/" + m.ModelID
				if m.Enabled && !seen[id] && authCtx.AllowsModel(id) {
					seen[id] = true
					ids = append(ids, id)
				}
			}
		}

		w.Header().Set("Content-Type", "application/json")
		if shape == "anthropic" {
			entries := make([]map[string]any, 0, len(ids))
			for _, id := range ids {
				entry := map[string]any{
					"type": "model", "id": id, "display_name": id,
					"created_at": "1970-01-01T00:00:00Z",
				}
				appendModelMetadata(entry, snap, id)
				entries = append(entries, entry)
			}
			var first, last any
			if len(entries) > 0 {
				first, last = entries[0]["id"], entries[len(entries)-1]["id"]
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": entries, "has_more": false, "first_id": first, "last_id": last,
			})
			return
		}

		entries := make([]map[string]any, 0, len(ids))
		for _, id := range ids {
			entry := map[string]any{
				"id": id, "object": "model", "created": 0, "owned_by": "gateway",
			}
			appendModelMetadata(entry, snap, id)
			entries = append(entries, entry)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"object": "list",
			"data":   entries,
		})
	}
}

// Get returns GET /v1/models/{model} and its protocol-specific aliases.
func Get(forceShape string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		shape := responseShape(r, forceShape)
		writeErr := authhttp.ErrorWriter(outwire.WriteOpenAIError)
		if shape == "anthropic" {
			writeErr = outwire.WriteAnthropicError
		}
		authCtx, err := auth.Authenticate(r)
		if err != nil {
			authhttp.WriteAuthError(w, err, writeErr)
			return
		}

		modelID := r.PathValue("model")
		snap := snapshot.Get()
		res, rerr := snap.Routes.Resolve(modelID)
		if rerr != nil {
			writeErr(w, http.StatusNotFound, "model_not_found", dataplane.ModelNotFoundMessage(snap, modelID))
			return
		}
		if !authCtx.AllowsModel(res.Route.PublicName) {
			writeErr(w, http.StatusForbidden, "model_not_allowed",
				fmt.Sprintf("model %q is not available for this API key", modelID))
			return
		}

		w.Header().Set("Content-Type", "application/json")
		if shape == "anthropic" {
			entry := map[string]any{
				"type": "model", "id": modelID, "display_name": modelID,
				"created_at": "1970-01-01T00:00:00Z",
			}
			appendModelMetadata(entry, snap, modelID)
			_ = json.NewEncoder(w).Encode(entry)
			return
		}
		entry := map[string]any{
			"id": modelID, "object": "model", "created": 0, "owned_by": "gateway",
		}
		appendModelMetadata(entry, snap, modelID)
		_ = json.NewEncoder(w).Encode(entry)
	}
}

func responseShape(r *http.Request, forceShape string) string {
	if forceShape != "" {
		return forceShape
	}
	if r.Header.Get("X-Api-Key") != "" && !hasBearer(r) {
		return "anthropic"
	}
	return "openai"
}

func appendModelMetadata(entry map[string]any, snap *snapshot.Snapshot, modelID string) {
	cw, mo := modelCapacity(snap, modelID)
	if cw > 0 {
		entry["context_length"] = cw
		entry["max_input_tokens"] = cw
	}
	if mo > 0 {
		entry["max_output_tokens"] = mo
	}
	if m := modelThinkingCapability(snap, modelID); m != nil {
		supports := true
		if m.SupportsThinking != nil {
			supports = *m.SupportsThinking
		}
		entry["supports_thinking"] = supports
		if len(m.EffortLevels) > 0 {
			out := make([]string, 0, len(m.EffortLevels))
			for _, level := range m.EffortLevels {
				out = append(out, string(level))
			}
			entry["supported_reasoning"] = out
		}
	}
}

func modelThinkingCapability(snap *snapshot.Snapshot, modelID string) *routing.UpstreamModel {
	res, err := snap.Routes.Resolve(modelID)
	if err != nil || len(res.Candidates) == 0 {
		return nil
	}
	return res.Candidates[0].UpstreamModel
}

func modelCapacity(snap *snapshot.Snapshot, modelID string) (contextWindow, maxOutput int) {
	res, err := snap.Routes.Resolve(modelID)
	if err != nil || len(res.Candidates) == 0 {
		return 0, 0
	}
	m := res.Candidates[0].UpstreamModel
	if m == nil {
		return 0, 0
	}
	cw, mo := m.ContextWindow, m.MaxOutputTokens
	if cw <= 0 {
		cw = snap.Runtime.DefaultContextWindow
	}
	if mo <= 0 {
		mo = snap.Runtime.DefaultMaxOutputTokens
	}
	return cw, mo
}

func hasBearer(r *http.Request) bool {
	const prefix = "Bearer "
	authorization := r.Header.Get("Authorization")
	return len(authorization) > len(prefix) &&
		strings.EqualFold(authorization[:len(prefix)], prefix)
}
