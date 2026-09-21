package admin

import (
	"net/http"
	"strings"

	"github.com/cn-maul/rosetta-gateway/internal/store"
)

type RouteHandler struct {
	store *store.Store
}

func NewRouteHandler(st *store.Store) *RouteHandler {
	return &RouteHandler{store: st}
}

// routeRequest 是路由的创建 / PATCH 输入。
//
// PATCH 语义（2026-09-21 重构）：所有字段都是指针，
//   - nil  → 未提供，保持原值
//   - 非nil → 显式赋新值；Priority=0、FallbackRouteID=""、ExtraJSON="" 均为合法值
//
// 旧实现用 `!= ""` / `!= 0` 判断，导致「优先级改回 0」「清空兜底路由」在界面上无效。
type routeRequest struct {
	PublicName      *string `json:"public_name"`
	ProviderID      *string `json:"provider_id"`
	UpstreamModelID *string `json:"upstream_model_id"`
	Enabled         *bool   `json:"enabled"`
	Priority        *int    `json:"priority"`
	FallbackRouteID *string `json:"fallback_route_id"`
	ExtraJSON       *string `json:"extra_json"`
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

	publicName := strings.TrimSpace(derefStr(req.PublicName))
	providerID := strings.TrimSpace(derefStr(req.ProviderID))
	upstreamModelID := strings.TrimSpace(derefStr(req.UpstreamModelID))
	if publicName == "" || providerID == "" || upstreamModelID == "" {
		writeError(w, http.StatusBadRequest, "public_name, provider_id, and upstream_model_id are required")
		return
	}

	priority := derefInt(req.Priority)
	if priority < 0 {
		writeError(w, http.StatusBadRequest, "priority cannot be negative")
		return
	}

	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}

	route := &store.Route{
		ID:              generateID(),
		PublicName:      publicName,
		ProviderID:      providerID,
		UpstreamModelID: upstreamModelID,
		Enabled:         enabled,
		Priority:        priority,
		FallbackRouteID: strings.TrimSpace(derefStr(req.FallbackRouteID)),
		ExtraJSON:       derefStr(req.ExtraJSON),
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

	// 三个必填字段：显式提供时不允许置空（旧的「静默忽略空串」会让人以为改成功了）
	if req.PublicName != nil {
		v := strings.TrimSpace(*req.PublicName)
		if v == "" {
			writeError(w, http.StatusBadRequest, "public_name cannot be empty")
			return
		}
		existing.PublicName = v
	}
	if req.ProviderID != nil {
		v := strings.TrimSpace(*req.ProviderID)
		if v == "" {
			writeError(w, http.StatusBadRequest, "provider_id cannot be empty")
			return
		}
		existing.ProviderID = v
	}
	if req.UpstreamModelID != nil {
		v := strings.TrimSpace(*req.UpstreamModelID)
		if v == "" {
			writeError(w, http.StatusBadRequest, "upstream_model_id cannot be empty")
			return
		}
		existing.UpstreamModelID = v
	}
	if req.Enabled != nil {
		existing.Enabled = *req.Enabled
	}
	if req.Priority != nil {
		if *req.Priority < 0 {
			writeError(w, http.StatusBadRequest, "priority cannot be negative")
			return
		}
		existing.Priority = *req.Priority
	}
	// 可清空字段：传空串即落 NULL（清空兜底路由 / 清空透传参数）
	if req.FallbackRouteID != nil {
		existing.FallbackRouteID = strings.TrimSpace(*req.FallbackRouteID)
	}
	if req.ExtraJSON != nil {
		existing.ExtraJSON = *req.ExtraJSON
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
