package admin

import (
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/cn-maul/rosetta-gateway/internal/server"
	"github.com/cn-maul/rosetta-gateway/internal/snapshot"
	"github.com/cn-maul/rosetta-gateway/internal/store"
)

// ---- 分组与模型白名单（多用户改造 P1）----
//
// # 权限模型
//
// 分组本身**只影响模型可见性**，不影响配额与限速（那两项在 P2）。
// 这条边界是刻意的：把「看得见什么」与「能用多少」混在一个配置项里，
// 会让「我只是想给这组人换个模型清单」顺手改掉额度。
//
// 全部端点都是 admin-only（由 server.AdminGateGuard 的前缀白名单挡住普通用户）。

// groupResponse 是分组对外的表示。
type groupResponse struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	// Models 是该组的公开模型名白名单。**空数组 = 该组不限制可见范围**，
	// 不是「什么都看不到」—— 前端必须把这点显式提示给管理员，
	// 否则很容易出现「建了组、忘了配模型、以为收紧了权限」。
	Models      []string `json:"models"`
	MemberCount int      `json:"member_count"`
	CreatedAt   int64    `json:"created_at"`
}

func (h *GroupHandler) toGroupResponse(id, name, desc string, createdAt int64, models []string, members int) groupResponse {
	if models == nil {
		models = []string{}
	}
	return groupResponse{
		ID: id, Name: name, Description: desc,
		Models: models, MemberCount: members, CreatedAt: createdAt,
	}
}

// GroupHandler 提供分组的增删改查与白名单维护。
//
// # 谁负责把改动刷到数据面
//
// 模型可见性随快照下发（snapshot.UserSnapshot.AllowedModels），
// 而**所有 /admin/api 下的成功写操作都会经 server.AutoReload 重建快照**
// —— 所以本文件刻意**不**再显式调 reload：那会变成一次请求重建两遍。
//
// 这条依赖不是「碰巧如此」：`access_keys.enabled` / `rpm_limit` 等列早就
// 靠同一条路径生效。但它确实是一条**隐式**依赖，所以由端到端脚本
// （`.workbuddy/verify_multiuser.sh`）以真实 HTTP 覆盖「改白名单 → 立刻生效」，
// 避免将来有人把分组路由挪出 adminAuto 而无人发现。
type GroupHandler struct {
	store *store.Store
}

// NewGroupHandler 构造分组管理 handler。
func NewGroupHandler(st *store.Store) *GroupHandler {
	return &GroupHandler{store: st}
}

// List 返回全部分组（含白名单与成员数）。
func (h *GroupHandler) List(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireAdmin(w, r); !ok {
		return
	}
	ctx := r.Context()
	groups, err := h.store.ListGroups(ctx)
	if err != nil {
		writeServerError(w, "list groups", err)
		return
	}
	models, err := h.store.ListGroupModels(ctx)
	if err != nil {
		writeServerError(w, "list group models", err)
		return
	}
	users, err := h.store.ListUsers(ctx)
	if err != nil {
		writeServerError(w, "list users", err)
		return
	}
	members := make(map[string]int, len(groups))
	for i := range users {
		if users[i].GroupID != "" {
			members[users[i].GroupID]++
		}
	}

	out := make([]groupResponse, 0, len(groups))
	for i := range groups {
		g := &groups[i]
		out = append(out, h.toGroupResponse(g.ID, g.Name, g.Description, g.CreatedAt,
			models[g.ID], members[g.ID]))
	}
	writeJSON(w, http.StatusOK, out)
}

type groupRequest struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// Create 新建分组。新组的白名单为空 = 不限制 —— 这是既有语义，
// 但**新建即不限制**对管理员是个陷阱（以为建组就收紧了权限）。
// 前端必须在创建成功后提示去配置模型白名单。
func (h *GroupHandler) Create(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireAdmin(w, r); !ok {
		return
	}
	var req groupRequest
	if err := decodeJSON(w, r, &req); err != nil {
		if errors.Is(err, errUnsupportedMediaType) {
			return
		}
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		writeError(w, http.StatusBadRequest, "组名必填")
		return
	}
	if len(name) > 64 {
		writeError(w, http.StatusBadRequest, "组名不能超过 64 个字符")
		return
	}

	g := &store.Group{
		ID:          generateID(),
		Name:        name,
		Description: strings.TrimSpace(req.Description),
	}
	if err := h.store.CreateGroup(r.Context(), g); err != nil {
		if store.IsUniqueViolation(err) {
			writeError(w, http.StatusConflict, "组名已存在")
			return
		}
		writeServerError(w, "create group", err)
		return
	}
	writeJSON(w, http.StatusCreated, h.toGroupResponse(g.ID, g.Name, g.Description, g.CreatedAt, nil, 0))
}

// Update 改名/改描述。白名单走 ReplaceModels，不在这里改。
func (h *GroupHandler) Update(w http.ResponseWriter, r *http.Request, id string) {
	if _, ok := requireAdmin(w, r); !ok {
		return
	}
	ctx := r.Context()
	g, err := h.store.GetGroup(ctx, id)
	if err != nil {
		writeServerError(w, "get group", err)
		return
	}
	if g == nil {
		writeError(w, http.StatusNotFound, "分组不存在")
		return
	}
	var req groupRequest
	if err := decodeJSON(w, r, &req); err != nil {
		if errors.Is(err, errUnsupportedMediaType) {
			return
		}
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if name := strings.TrimSpace(req.Name); name != "" {
		if len(name) > 64 {
			writeError(w, http.StatusBadRequest, "组名不能超过 64 个字符")
			return
		}
		g.Name = name
	}
	g.Description = strings.TrimSpace(req.Description)

	if err := h.store.UpdateGroup(ctx, g); err != nil {
		if store.IsUniqueViolation(err) {
			writeError(w, http.StatusConflict, "组名已存在")
			return
		}
		writeNotFoundOrError(w, "update group", "分组不存在", err)
		return
	}
	models, _ := h.store.ListGroupModelsByGroup(ctx, id)
	writeJSON(w, http.StatusOK, h.toGroupResponse(g.ID, g.Name, g.Description, g.CreatedAt, models, 0))
}

// Delete 删除分组。组内还有成员时拒绝（见 store.ErrGroupNotEmpty）——
// 靠外键 SET NULL 放行等于悄悄把那一组人的权限从「受限」放大到「不受限」。
func (h *GroupHandler) Delete(w http.ResponseWriter, r *http.Request, id string) {
	if _, ok := requireAdmin(w, r); !ok {
		return
	}
	if err := h.store.DeleteGroup(r.Context(), id); err != nil {
		if errors.Is(err, store.ErrGroupNotEmpty) {
			// 把人数一起告诉管理员：只说「删不掉」会让人去查库，
			// 而真正该做的是把这些人迁走 —— 迁移工作量取决于人数。
			msg := "该分组下还有账号，请先把他们移到别的组或移出分组（否则他们的模型权限会被放大）"
			if n, cerr := h.store.CountGroupMembers(r.Context(), id); cerr == nil && n > 0 {
				msg = fmt.Sprintf("该分组下还有 %d 个账号，请先把他们移到别的组或移出分组（否则他们的模型权限会被放大）", n)
			}
			writeError(w, http.StatusConflict, msg)
			return
		}
		writeNotFoundOrError(w, "delete group", "分组不存在", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

type groupModelsRequest struct {
	Models []string `json:"models"`
}

// SetModels 整体替换某组的模型白名单。
//
// 传空数组 = 清空白名单 = 该组不限制可见范围（不是拒绝全部）。
// 这个语义必须在响应里讲明，否则「清空」看起来像「禁用全部」。
func (h *GroupHandler) SetModels(w http.ResponseWriter, r *http.Request, id string) {
	if _, ok := requireAdmin(w, r); !ok {
		return
	}
	var req groupModelsRequest
	if err := decodeJSON(w, r, &req); err != nil {
		if errors.Is(err, errUnsupportedMediaType) {
			return
		}
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if !validateModelAllowlist(w, req.Models) {
		return
	}
	if err := h.store.ReplaceGroupModels(r.Context(), id, req.Models); err != nil {
		writeNotFoundOrError(w, "replace group models", "分组不存在", err)
		return
	}
	// 快照重建由 server.AutoReload 在本请求返回后完成（见 GroupHandler 注释）。
	models, _ := h.store.ListGroupModelsByGroup(r.Context(), id)
	if models == nil {
		models = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"models": models,
		"note":   "白名单为空表示该组不限制模型可见范围",
	})
}

// ModelNames 返回「当前调用者可以选用的公开模型名清单」，供前端的白名单选择器用。
//
// # 为什么单独开一个端点，而不是让前端复用 /admin/api/routes
//
// /admin/api/routes 是 admin-only（它会暴露全部路由拓扑）。而 key 级模型白名单
// 的设计意图是**用户可以自己收紧**—— 如果普通用户读不到模型清单，这个功能
// 对他们就等于不存在，只能让管理员代配。
//
// # 返回值按身份收窄
//
//   - 管理员 / 运维凭据：全部路由名（前端校验也接受这些名字，包括暂时停用的
//     路由——允许为新模型预先配好名单）。
//   - 普通用户：与**其所属组**的白名单求交。这正是「他用任何一把 key 最多能用到
//     什么」；key 那一层要等具体选中某把 key 才能收窄，而 key 的收窄只能更紧，
//     所以这里给全集不会让他越权。
//
// restricted 字段告诉前端「这份清单是全集还是已经被组收窄过」，
// 避免界面把过滤后的清单当成全集展示。
func (h *GroupHandler) ModelNames(w http.ResponseWriter, r *http.Request) {
	me := server.UserFromContext(r.Context())
	if me == nil {
		writeError(w, http.StatusUnauthorized, "需要登录")
		return
	}

	all := make([]string, 0, 32)
	seen := make(map[string]struct{}, 32)
	for _, route := range snapshot.Get().Routes.ListRoutes() {
		if _, dup := seen[route.PublicName]; dup {
			continue
		}
		seen[route.PublicName] = struct{}{}
		all = append(all, route.PublicName)
	}
	sort.Strings(all)

	// 管理员不受分组白名单约束。
	//
	// 空 ID **不**当admin（fail-closed）：统一认证后所有会话都对应 users 行，
	// 该状态不可达；原实现把它当admin 是静默提权，方向全错。
	// 代价：若真出现空 ID，用户会看到「模型列表受限」而非全量——这是
	// 可见且可修的失败方向，远好于「拿不到身份 = 拿到最高权限」。
	if me.ID == "" {
		writeError(w, http.StatusUnauthorized, "需要登录")
		return
	}
	if me.IsAdmin() {
		writeJSON(w, http.StatusOK, map[string]any{"models": all, "restricted": false})
		return
	}

	u := snapshot.Get().UsersByID[me.ID]
	if u == nil || u.AllowedModels.Unrestricted {
		writeJSON(w, http.StatusOK, map[string]any{"models": all, "restricted": false})
		return
	}
	// 组白名单生效：只给他组内那部分。交集为空时返回空数组而不是全集 ——
	// 「全都看不到」和「什么都能看到」是两回事，返回全集会静默放宽。
	filtered := make([]string, 0, len(u.AllowedModels.Models))
	for _, m := range all {
		if u.AllowedModels.Allows(m) {
			filtered = append(filtered, m)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"models": filtered, "restricted": true})
}

// unknownModels 返回白名单里「当前不存在」的公开模型名。
//
// # 为什么要校验
//
// 白名单是精确匹配。拼错一个字符的结果是「这个模型谁都看不到」，
// 而界面上看不出任何异常 —— 管理员会以为配对了。更糟的是全组都拼错时，
// 该组**所有模型都不可见**，现象是「组内用户全部 403」，排查要从
// 权限链一路查到拼写。在写入点挡住，把静默故障变成一条 400 错误信息。
//
// 校验用的是当前快照里的公开模型名，与数据面判定同源 —— 用别的来源
// （比如查库）会出现「校验通过但转发时判不通过」的不一致。
func unknownModels(models []string) (unknown []string, blanks bool) {
	if len(models) == 0 {
		return nil, false
	}
	known := make(map[string]struct{}, 32)
	for _, route := range snapshot.Get().Routes.ListRoutes() {
		known[route.PublicName] = struct{}{}
	}
	seen := make(map[string]struct{}, len(models))
	for _, m := range models {
		m = strings.TrimSpace(m)
		if m == "" {
			// 空白条目**不能**静默跳过。
			//
			// 修复前这里 continue，于是 {"allowed_models":["  "]} 校验通过，
			// 落库时被 store.NormalizeModelNames 归一成空 → 写 NULL → 读回 nil
			// → keyModelAllow(nil) = AllowAll()：一把**不限制**的 key。
			// 本意「收紧到某个模型」的结果是「完全不限制」，且界面无任何提示。
			// 全空白（["  "] / [""]）等价于清空白名单，那要用 [] 显式表达。
			blanks = true
			continue
		}
		if _, dup := seen[m]; dup {
			continue
		}
		seen[m] = struct{}{}
		if _, ok := known[m]; !ok {
			unknown = append(unknown, m)
		}
	}
	return unknown, blanks
}

// validateModelAllowlist 统一校验 key/组两处共用的模型白名单入参。
func validateModelAllowlist(w http.ResponseWriter, models []string) bool {
	unknown, blanks := unknownModels(models)
	if len(unknown) > 0 {
		writeError(w, http.StatusBadRequest,
			"以下模型名不存在，请从当前模型列表中选择："+strings.Join(unknown, ", "))
		return false
	}
	if blanks {
		writeError(w, http.StatusBadRequest,
			"模型白名单里不能有空白条目（会被当成「不限制」）；清空请传空数组 []")
		return false
	}
	return true
}
