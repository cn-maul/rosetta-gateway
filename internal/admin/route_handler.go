package admin

import (
	"errors"
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
//   - 非nil → 显式赋新值
//
// 旧实现用 `!= ""` / `!= 0` 判断，导致「清空兜底路由」在界面上无效。
//
// 这里没有 priority / fallback_route_id / 各类故障转移数值：
//   - priority 运行时从不读，且 public_name 的 UNIQUE 让「同名择优」在数据层不可能；
//   - fallback_route_id 的单跳兜底已被 route_targets 有序链取代（DESIGN §10）；
//   - 故障转移策略参数（尝试预算/熔断阈值/超时）是全局的，统一在「设置」页配置。
//
// 「一个公开名后面挂多个上游、失败自动切换」由 route_targets 链承担。
type routeRequest struct {
	PublicName      *string `json:"public_name"`
	ProviderID      *string `json:"provider_id"`
	UpstreamModelID *string `json:"upstream_model_id"`
	Enabled         *bool   `json:"enabled"`
	FailoverEnabled *bool   `json:"failover_enabled"`
}

// 没有 extra_json：它存了但没有任何地方拿它构造请求，具体理由见
// internal/store/store.go 的 dropDeadColumns 注释。
type routeResponse struct {
	ID              string `json:"id"`
	PublicName      string `json:"public_name"`
	ProviderID      string `json:"provider_id"`
	UpstreamModelID string `json:"upstream_model_id"`
	Enabled         bool   `json:"enabled"`
	CreatedAt       int64  `json:"created_at"`

	FailoverEnabled bool `json:"failover_enabled"`
}

func (h *RouteHandler) List(w http.ResponseWriter, r *http.Request) {
	routes, err := h.store.ListRoutes(r.Context())
	if err != nil {
		writeServerError(w, "list routes", err)
		return
	}
	result := make([]routeResponse, 0, len(routes))
	for _, route := range routes {
		result = append(result, toRouteResponse(route))
	}
	writeJSON(w, http.StatusOK, result)
}

// writeRouteWriteError 把 route 写操作的失败映射成合适的状态码。
//
// 重名（public_name 带 UNIQUE 约束）此前返回 500 + 裸的
// "UNIQUE constraint failed: routes.public_name" —— 一个 4xx 级别的问题
// 披着 500 的皮，前端只能提示「保存失败」，用户重试必然还是同一个 500。
func writeRouteWriteError(w http.ResponseWriter, action string, err error) {
	if store.IsUniqueViolation(err) {
		writeError(w, http.StatusConflict, "该公开模型名已被另一条路由占用，请换一个")
		return
	}
	writeServerError(w, action, err)
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

	// 配对校验：provider 与上游模型必须真实存在且互属。
	// 缺了它就能建出一条「有目标行、但该目标查不到」的路由 —— 运行时得到空候选、
	// 又因为「有目标行」不回落主目标列，直接终止为 404；而接口刚才回的是 201，
	// 界面显示保存成功。同一个校验目标链接口一直在做，这里漏了。
	if err := h.store.ValidateRouteTargetRef(r.Context(), providerID, upstreamModelID); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
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
		FailoverEnabled: derefBool(req.FailoverEnabled),
	}

	// 新建 route 与「种一条 position 0 的目标（=主目标）」必须原子完成，
	// 否则第二步失败就留下一条没有链的 route —— 让链从诞生起就可被后台管理，
	// 不必等下一次冷启动迁移回填。
	if err := h.store.CreateRouteWithHeadTarget(r.Context(), route); err != nil {
		writeRouteWriteError(w, "create route", err)
		return
	}

	writeJSON(w, http.StatusCreated, toRouteResponse(*route))
}

func (h *RouteHandler) Update(w http.ResponseWriter, r *http.Request, id string) {
	existing, err := h.store.GetRoute(r.Context(), id)
	if err != nil {
		// DB 故障不是「资源不存在」。报 404 会让运维去删库重建配置，
		// 真正的问题（磁盘/锁）反而被掩盖。
		writeServerError(w, "get route", err)
		return
	}
	if existing == nil {
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
	if req.FailoverEnabled != nil {
		existing.FailoverEnabled = *req.FailoverEnabled
	}

	// 配对校验放在所有字段赋值**之后**：只看最终要落库的那一对是否合法，
	// 不要求中间态合法（同一个请求里既换 provider 又换 model 是常见操作）。
	if err := h.store.ValidateRouteTargetRef(r.Context(), existing.ProviderID, existing.UpstreamModelID); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// 更新 route 与「把链首对齐到新的主目标」必须原子完成：运行时以链为准，
	// 第二步失败就会留下「主目标列=新值、链首=旧值」的永久分叉 ——
	// 响应回显新值、界面显示成功，实际流量仍打旧目标，且没有自愈路径。
	if err := h.store.UpdateRouteWithHeadTarget(r.Context(), id, existing); err != nil {
		writeRouteWriteError(w, "update route", err)
		return
	}

	writeJSON(w, http.StatusOK, toRouteResponse(*existing))
}

func (h *RouteHandler) Delete(w http.ResponseWriter, r *http.Request, id string) {
	switch err := h.store.DeleteRoute(r.Context(), id); {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "route not found")
	case err != nil:
		writeServerError(w, "delete route", err)
	default:
		writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
	}
}

func toRouteResponse(r store.Route) routeResponse {
	return routeResponse{
		ID:              r.ID,
		PublicName:      r.PublicName,
		ProviderID:      r.ProviderID,
		UpstreamModelID: r.UpstreamModelID,
		Enabled:         r.Enabled,
		CreatedAt:       r.CreatedAt,

		FailoverEnabled: r.FailoverEnabled,
	}
}
