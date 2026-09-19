package admin

import (
	"net/http"

	"github.com/cn-maul/rosetta-gateway/internal/store"
)

type RouteHandler struct {
	store *store.Store
}

func NewRouteHandler(st *store.Store) *RouteHandler {
	return &RouteHandler{store: st}
}

type routeRequest struct {
	PublicName      string `json:"public_name"`
	ProviderID      string `json:"provider_id"`
	UpstreamModelID string `json:"upstream_model_id"`
	Enabled         *bool  `json:"enabled"`
	Priority        int    `json:"priority"`
	FallbackRouteID string `json:"fallback_route_id"`
	ExtraJSON       string `json:"extra_json"`
}

type routeResponse struct {
	ID              string `json:"id"`
	PublicName      string `json:"public_name"`
	ProviderID      string `json:"provider_id"`
	UpstreamModelID string `json:"upstream_model_id"`
	Enabled         bool   `json:"enabled"`
	Priority        int    `json:"priority"`
	FallbackRouteID string `json:"fallback_route_id"`
	ExtraJSON       string `json:"extra_json"`
	CreatedAt       int64  `json:"created_at"`
}

func (h *RouteHandler) List(w http.ResponseWriter, r *http.Request) {
	routes, err := h.store.ListRoutes(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	result := make([]routeResponse, 0, len(routes))
	for _, route := range routes {
		result = append(result, toRouteResponse(route))
	}
	writeJSON(w, http.StatusOK, result)
}

func (h *RouteHandler) Create(w http.ResponseWriter, r *http.Request) {
	var req routeRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}

	if req.PublicName == "" || req.ProviderID == "" || req.UpstreamModelID == "" {
		writeError(w, http.StatusBadRequest, "public_name, provider_id, and upstream_model_id are required")
		return
	}

	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}

	route := &store.Route{
		ID:              generateID(),
		PublicName:      req.PublicName,
		ProviderID:      req.ProviderID,
		UpstreamModelID: req.UpstreamModelID,
		Enabled:         enabled,
		Priority:        req.Priority,
		FallbackRouteID: req.FallbackRouteID,
		ExtraJSON:       req.ExtraJSON,
	}

	if err := h.store.CreateRoute(r.Context(), route); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusCreated, toRouteResponse(*route))
}

func (h *RouteHandler) Update(w http.ResponseWriter, r *http.Request, id string) {
	existing, err := h.store.GetRoute(r.Context(), id)
	if err != nil || existing == nil {
		writeError(w, http.StatusNotFound, "route not found")
		return
	}

	var req routeRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}

	if req.PublicName != "" {
		existing.PublicName = req.PublicName
	}
	if req.ProviderID != "" {
		existing.ProviderID = req.ProviderID
	}
	if req.UpstreamModelID != "" {
		existing.UpstreamModelID = req.UpstreamModelID
	}
	if req.Enabled != nil {
		existing.Enabled = *req.Enabled
	}
	if req.Priority != 0 {
		existing.Priority = req.Priority
	}
	if req.FallbackRouteID != "" {
		existing.FallbackRouteID = req.FallbackRouteID
	}
	if req.ExtraJSON != "" {
		existing.ExtraJSON = req.ExtraJSON
	}

	if err := h.store.UpdateRoute(r.Context(), id, existing); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, toRouteResponse(*existing))
}

func (h *RouteHandler) Delete(w http.ResponseWriter, r *http.Request, id string) {
	if err := h.store.DeleteRoute(r.Context(), id); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func toRouteResponse(r store.Route) routeResponse {
	return routeResponse{
		ID:              r.ID,
		PublicName:      r.PublicName,
		ProviderID:      r.ProviderID,
		UpstreamModelID: r.UpstreamModelID,
		Enabled:         r.Enabled,
		Priority:        r.Priority,
		FallbackRouteID: r.FallbackRouteID,
		ExtraJSON:       r.ExtraJSON,
		CreatedAt:       r.CreatedAt,
	}
}
