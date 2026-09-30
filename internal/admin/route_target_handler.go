package admin

import (
	"net/http"
	"strconv"

	"github.com/cn-maul/rosetta-gateway/internal/store"
)

// RouteTargetHandler 管理一条 route 的有序故障转移目标链。
//
// 交互方式与前端「编辑链」对齐：一次 PUT 整体替换（按数组顺序重排 position），
// 而非逐条增删改 —— 有序列表的插入/删除/上下移在客户端算好新顺序后一把提交最自然。
// 落库后由前端照旧调 POST /admin/api/reload 重建快照，运行时才看得到新链。
type RouteTargetHandler struct {
	store *store.Store
}

func NewRouteTargetHandler(st *store.Store) *RouteTargetHandler {
	return &RouteTargetHandler{store: st}
}

type routeTargetResponse struct {
	ID              string `json:"id"`
	RouteID         string `json:"route_id"`
	ProviderID      string `json:"provider_id"`
	ProviderName    string `json:"provider_name"`
	UpstreamModelID string `json:"upstream_model_id"`
	ModelID         string `json:"model_id"`
	Position        int    `json:"position"`
	Enabled         bool   `json:"enabled"`
}

// routeTargetInput 是整体替换时数组里的一个元素。
// position 由数组下标决定，不接受客户端传入；enabled 省略即 true。
type routeTargetInput struct {
	ProviderID      string `json:"provider_id"`
	UpstreamModelID string `json:"upstream_model_id"`
	Enabled         *bool  `json:"enabled"`
}

type replaceTargetsRequest struct {
	Targets []routeTargetInput `json:"targets"`
}

func (h *RouteTargetHandler) List(w http.ResponseWriter, r *http.Request, routeID string) {
	route, err := h.store.GetRoute(r.Context(), routeID)
	if err != nil {
		writeServerError(w, "get route for targets", err)
		return
	}
	if route == nil {
		writeError(w, http.StatusNotFound, "route not found")
		return
	}

	targets, err := h.store.ListRouteTargets(r.Context(), routeID)
	if err != nil {
		writeServerError(w, "list route targets", err)
		return
	}
	writeJSON(w, http.StatusOK, h.buildResponses(r, targets))
}

func (h *RouteTargetHandler) Replace(w http.ResponseWriter, r *http.Request, routeID string) {
	route, err := h.store.GetRoute(r.Context(), routeID)
	if err != nil {
		writeServerError(w, "get route for targets", err)
		return
	}
	if route == nil {
		writeError(w, http.StatusNotFound, "route not found")
		return
	}

	var req replaceTargetsRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if len(req.Targets) == 0 {
		writeError(w, http.StatusBadRequest, "至少需要一个上游目标；要停用整条 route 请把 route.enabled 置 false")
		return
	}

	built := make([]store.RouteTarget, 0, len(req.Targets))
	for i, in := range req.Targets {
		if err := h.validateTarget(r, in); err != nil {
			writeError(w, http.StatusBadRequest, "targets["+strconv.Itoa(i)+"]: "+err.Error())
			return
		}
		enabled := true
		if in.Enabled != nil {
			enabled = *in.Enabled
		}
		built = append(built, store.RouteTarget{
			RouteID:         routeID,
			ProviderID:      in.ProviderID,
			UpstreamModelID: in.UpstreamModelID,
			Position:        i,
			Enabled:         enabled,
		})
	}

	if err := h.store.ReplaceRouteTargets(r.Context(), routeID, built); err != nil {
		writeServerError(w, "replace route targets", err)
		return
	}

	targets, err := h.store.ListRouteTargets(r.Context(), routeID)
	if err != nil {
		writeServerError(w, "list route targets", err)
		return
	}
	writeJSON(w, http.StatusOK, h.buildResponses(r, targets))
}

// validateTarget 复用 store 层的配对校验：provider 存在、上游模型存在且确属该 provider。
// 三者任一不符都会在运行时被 buildCandidates 静默过滤掉，与其到请求时才 404，
// 不如此处在配置入口就明确报错。
//
// 与 POST /routes、PATCH /routes/{id} 共用**同一份**判定 —— 两条写入路径各写一套
// 校验是本站出过问题的根源：链路上严格、另一条只查非空串，于是能建出恒 404 的路由。
func (h *RouteTargetHandler) validateTarget(r *http.Request, in routeTargetInput) error {
	return h.store.ValidateRouteTargetRef(r.Context(), in.ProviderID, in.UpstreamModelID)
}

// buildResponses 补齐展示字段（provider 名、模型 id），供前端列表直接渲染。
func (h *RouteTargetHandler) buildResponses(r *http.Request, targets []store.RouteTarget) []routeTargetResponse {
	out := make([]routeTargetResponse, 0, len(targets))
	for _, t := range targets {
		resp := routeTargetResponse{
			ID:              t.ID,
			RouteID:         t.RouteID,
			ProviderID:      t.ProviderID,
			UpstreamModelID: t.UpstreamModelID,
			Position:        t.Position,
			Enabled:         t.Enabled,
		}
		if p, _ := h.store.GetProvider(r.Context(), t.ProviderID); p != nil {
			resp.ProviderName = p.Name
		}
		if m, _ := h.store.GetUpstreamModel(r.Context(), t.UpstreamModelID); m != nil {
			resp.ModelID = m.ModelID
		}
		out = append(out, resp)
	}
	return out
}
