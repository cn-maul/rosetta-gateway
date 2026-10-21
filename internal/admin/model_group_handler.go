package admin

import (
	"errors"
	"net/http"
	"strings"

	"github.com/cn-maul/rosetta-gateway/internal/store"
)

// ModelGroupHandler 管理「跨上游的同一个模型」的归组。
//
// 归组是纯展示/统计层概念：把 deepseek 的 deepseek-chat 和某聚合站的
// ds-chat 标成一组「DeepSeek V3」，用量统计就能按组合并显示。
// 数据面（路由、转发、思考挡位）完全不感知归组 —— 它不参与任何请求判定。
type ModelGroupHandler struct {
	store *store.Store
}

func NewModelGroupHandler(st *store.Store) *ModelGroupHandler {
	return &ModelGroupHandler{store: st}
}

type modelGroupResponse struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
	// MemberCount 是组内模型数。列表页要能一眼看出「这组管着几个模型」
	// 以及删组的影响面，前端再逐组查成员是 N+1。
	MemberCount int   `json:"member_count"`
	CreatedAt   int64 `json:"created_at"`
}

// groupMemberCounts 一次查询拿全部组的成员数（List 的富化用）。
func (h *ModelGroupHandler) groupMemberCounts() (map[string]int, error) {
	rows, err := h.store.Reader().Query(
		`SELECT group_id, COUNT(*) FROM upstream_models WHERE group_id IS NOT NULL GROUP BY group_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var id string
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			return nil, err
		}
		out[id] = n
	}
	return out, rows.Err()
}

func (h *ModelGroupHandler) List(w http.ResponseWriter, r *http.Request) {
	groups, err := h.store.ListModelGroups(r.Context())
	if err != nil {
		writeServerError(w, "list model groups", err)
		return
	}
	counts, err := h.groupMemberCounts()
	if err != nil {
		writeServerError(w, "count model group members", err)
		return
	}
	result := make([]modelGroupResponse, 0, len(groups))
	for _, g := range groups {
		result = append(result, modelGroupResponse{
			ID: g.ID, DisplayName: g.DisplayName,
			MemberCount: counts[g.ID], CreatedAt: g.CreatedAt,
		})
	}
	writeJSON(w, http.StatusOK, result)
}

type modelGroupRequest struct {
	DisplayName *string `json:"display_name"`
}

func (h *ModelGroupHandler) Create(w http.ResponseWriter, r *http.Request) {
	var req modelGroupRequest
	if err := decodeJSON(w, r, &req); err != nil {
		if errors.Is(err, errUnsupportedMediaType) {
			return
		}
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	name := strings.TrimSpace(derefStr(req.DisplayName))
	if name == "" {
		writeError(w, http.StatusBadRequest, "display_name 不能为空")
		return
	}
	g := &store.ModelGroup{ID: generateID(), DisplayName: name}
	if err := h.store.CreateModelGroup(r.Context(), g); err != nil {
		if store.IsUniqueViolation(err) {
			writeError(w, http.StatusConflict, "已存在同名的模型组")
			return
		}
		writeServerError(w, "create model group", err)
		return
	}
	writeJSON(w, http.StatusCreated, modelGroupResponse{ID: g.ID, DisplayName: g.DisplayName, CreatedAt: g.CreatedAt})
}

func (h *ModelGroupHandler) Update(w http.ResponseWriter, r *http.Request, id string) {
	var req modelGroupRequest
	if err := decodeJSON(w, r, &req); err != nil {
		if errors.Is(err, errUnsupportedMediaType) {
			return
		}
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	// PATCH 语义（2026-10-21 修 A7-11）：指针 nil = 未提供。
	//
	// 此前无条件 deref 后把空串判为非法，于是没带 display_name 的请求一律
	// 400 —— 名义上是 PATCH，实际只支持「整条重发」。前端当前总发全字段
	// （ModelGroups.vue 与 api.ts 的调用），所以还没在 UI 上暴露，但契约
	// 与其余 PATCH 不一致；且将来给这个请求体加任何字段（备注、排序等）
	// 时都会立刻踩到它。
	//
	// 显式传空串仍然是 400：模型组没有显示名就在界面上无法指认。
	if req.DisplayName == nil {
		writeError(w, http.StatusBadRequest, "display_name 必填")
		return
	}
	name := strings.TrimSpace(*req.DisplayName)
	if name == "" {
		writeError(w, http.StatusBadRequest, "display_name 不能为空")
		return
	}
	if err := h.store.UpdateModelGroup(r.Context(), id, name); err != nil {
		if store.IsUniqueViolation(err) {
			writeError(w, http.StatusConflict, "已存在同名的模型组")
			return
		}
		writeNotFoundOrError(w, "update model group", "模型组不存在（可能已被并发删除）", err)
		return
	}
	writeJSON(w, http.StatusOK, modelGroupResponse{ID: id, DisplayName: name})
}

// Delete 删除归组。组内模型**不会**被删除，它们回到「未归组」
// （group_id 由 FK 的 ON DELETE SET NULL 清掉）。
func (h *ModelGroupHandler) Delete(w http.ResponseWriter, r *http.Request, id string) {
	if err := h.store.DeleteModelGroup(r.Context(), id); err != nil {
		writeDeleteError(w, "delete model group", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// assignGroupRequest 是批量指派归组的输入。
//
// GroupID 用指针：区分「没提供」与「显式提供空串」。空串是合法值且语义
// 明确 = 把模型移出组（未归组）。省略该字段则报错而不是「什么都不做」——
// 这个端点唯一的存在理由就是指派/取消指派，两者都没发生就是调用方写错了。
type assignGroupRequest struct {
	ModelIDs []string `json:"model_ids"`
	GroupID  *string  `json:"group_id"`
}

// AssignModels 把多个模型指派到某个组（group_id 传空 = 移出组）。
//
// 整体替换语义：组内旧成员不动（新指派的加进来，这是「加入」而不是
// 「重定义成员集合」）—— 重定义一个组应该显式清空后重新指派，
// 界面上是两次有意识的操作，防止「勾选了 3 个模型加入组」被误解成
// 「组里只该有这 3 个」。
//
// 全部校验后才逐条写，任一 id 不存在则整笔拒绝（与批量改价同一立场：
// 静默部分生效会让运维以为「都标好了」，实则漏了几个）。
func (h *ModelGroupHandler) AssignModels(w http.ResponseWriter, r *http.Request, id string) {
	var req assignGroupRequest
	if err := decodeJSON(w, r, &req); err != nil {
		if errors.Is(err, errUnsupportedMediaType) {
			return
		}
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	seen := make(map[string]bool, len(req.ModelIDs))
	ids := make([]string, 0, len(req.ModelIDs))
	for _, mid := range req.ModelIDs {
		mid = strings.TrimSpace(mid)
		if mid == "" || seen[mid] {
			continue
		}
		seen[mid] = true
		ids = append(ids, mid)
	}
	if len(ids) == 0 {
		writeError(w, http.StatusBadRequest, "model_ids 不能为空")
		return
	}
	if len(ids) > 500 {
		writeError(w, http.StatusBadRequest, "一次最多指派 500 个模型")
		return
	}
	// 目标组必须存在。用 GetModelGroup 而不是依赖 FK 报错：
	// SQLite 的 FK 约束错误是英文原文（"FOREIGN KEY constraint failed"），
	// 运维拿它排查半天，不如入口一句「模型组不存在」。
	g, err := h.store.GetModelGroup(r.Context(), id)
	if err != nil {
		writeServerError(w, "get model group", err)
		return
	}
	if g == nil {
		writeError(w, http.StatusNotFound, "模型组不存在")
		return
	}

	missing := make([]string, 0)
	for _, mid := range ids {
		m, err := h.store.GetUpstreamModel(r.Context(), mid)
		if err != nil {
			writeServerError(w, "verify model", err)
			return
		}
		if m == nil {
			missing = append(missing, mid)
		}
	}
	if len(missing) > 0 {
		writeError(w, http.StatusNotFound,
			"部分模型不存在，归组未写入："+strings.Join(missing, ", "))
		return
	}

	assigned := 0
	for _, mid := range ids {
		if err := h.store.SetModelGroup(r.Context(), mid, id); err != nil {
			writeServerError(w, "assign model group", err)
			return
		}
		assigned++
	}
	writeJSON(w, http.StatusOK, map[string]any{"assigned": assigned, "group_id": id})
}

// UnassignModels 把模型移出归组（POST /admin/api/model-groups/unassign）。
//
// 单独一个端点而不是 AssignModels 的 group_id=""：指派是「对某个组」的
// 操作（路径里有 {id}），移出组不属于任何组，塞进同一个语义会让路径说谎。
func (h *ModelGroupHandler) UnassignModels(w http.ResponseWriter, r *http.Request) {
	var req assignGroupRequest
	if err := decodeJSON(w, r, &req); err != nil {
		if errors.Is(err, errUnsupportedMediaType) {
			return
		}
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	seen := make(map[string]bool, len(req.ModelIDs))
	ids := make([]string, 0, len(req.ModelIDs))
	for _, mid := range req.ModelIDs {
		mid = strings.TrimSpace(mid)
		if mid == "" || seen[mid] {
			continue
		}
		seen[mid] = true
		ids = append(ids, mid)
	}
	if len(ids) == 0 {
		writeError(w, http.StatusBadRequest, "model_ids 不能为空")
		return
	}
	// 移出组对「模型不存在」与「本来就没在组里」都是无害操作，
	// 所以不预先校验存在性：SetModelGroup 命中 0 行时跳过即可。
	// 整笔拒绝的立场只适用于「部分写入」危险的场景（改价/指派），
	// 这里没有那种风险。
	unassigned := 0
	for _, mid := range ids {
		if err := h.store.SetModelGroup(r.Context(), mid, ""); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				continue
			}
			writeServerError(w, "unassign model group", err)
			return
		}
		unassigned++
	}
	writeJSON(w, http.StatusOK, map[string]any{"unassigned": unassigned})
}

// GroupMembers 列出一个组内的全部模型（组详情页用）。
func (h *ModelGroupHandler) GroupMembers(w http.ResponseWriter, r *http.Request, id string) {
	models, err := h.store.ListModelsInGroup(r.Context(), id)
	if err != nil {
		writeServerError(w, "list group members", err)
		return
	}
	result := make([]modelResponse, 0, len(models))
	for _, m := range models {
		result = append(result, toModelResponse(m))
	}
	writeJSON(w, http.StatusOK, result)
}
