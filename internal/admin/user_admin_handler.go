package admin

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/cn-maul/rosetta-gateway/internal/server"
	"github.com/cn-maul/rosetta-gateway/internal/store"
	"github.com/cn-maul/rosetta-gateway/internal/userauth"
)

// ---- 用户管理（多用户改造 P0.5）----
//
// # 权限模型
//
// 角色只有两级（见 store.RoleAdmin/RoleUser），刻意不引Casbin ——
// new-api 自己已从 Casbin 收敛回静态矩阵（见 MULTIUSER.md §2.3）。
// 规则只有三条，全部写死在这个文件里，不做成可配置的策略表：
//
//	admin  → 用户管理全部端点 + 所有配置（上游/路由/设置）
//	user   → 只能读自己的 key 与用量；改自己的密码
//
// 「key 与用量按归属过滤」不是靠每个 handler 手写 if，
// 而是靠 DAO 方法强制带 user_id 参数（MULTIUSER.md §4.5）——
// 漏加一个的后果是数据泄露，编译期拦住比 review 可靠。

// userResponse 是用户对外的表示。
//
// 刻意**不含** password_hash：它进响应就等于把哈希交给浏览器，
// 哪怕是 PBKDF2 也是不该发生的信息泄漏。
type userResponse struct {
	ID          string `json:"id"`
	Username    string `json:"username"`
	DisplayName string `json:"display_name"`
	Role        string `json:"role"`
	Status      string `json:"status"`
	// GroupID 是所属分组（P1）。空串 = 不属于任何组 = **不受模型白名单约束**。
	// 前端必须把「未分组」与「分到组」的差别显示出来，否则会有人以为
	// 「没分组」是一种限制。
	GroupID     string `json:"group_id"`
	QuotaTokens int64  `json:"quota_tokens"`
	UsedTokens  int64  `json:"used_tokens"`
	AuthVersion int64  `json:"auth_version"`
	Remark      string `json:"remark,omitempty"`
	// HasPassword 为false 时前端应显示「未设置密码」并引导去设置。
	HasPassword bool  `json:"has_password"`
	KeyCount    int   `json:"key_count"`
	CreatedAt   int64 `json:"created_at"`
	LastLoginAt int64 `json:"last_login_at"`
	// IsSelf 标记这是不是当前登录者自己（前端据此禁用自删按钮）。
	IsSelf bool `json:"is_self"`
}

func toUserResponse(u *store.User, selfID string, keyCount int) userResponse {
	return userResponse{
		ID:          u.ID,
		Username:    u.Username,
		DisplayName: u.DisplayName,
		Role:        u.Role,
		Status:      u.Status,
		GroupID:     u.GroupID,
		QuotaTokens: u.QuotaTokens,
		UsedTokens:  u.UsedTokens,
		AuthVersion: u.AuthVersion,
		Remark:      u.Remark,
		HasPassword: u.PasswordHash != "",
		KeyCount:    keyCount,
		CreatedAt:   u.CreatedAt,
		LastLoginAt: u.LastLoginAt,
		IsSelf:      u.ID == selfID,
	}
}

// requireAdmin 返回当前用户，非管理员时已写出 403。
func requireAdmin(w http.ResponseWriter, r *http.Request) (*store.User, bool) {
	u := server.UserFromContext(r.Context())
	if u == nil {
		writeError(w, http.StatusUnauthorized, "需要登录")
		return nil, false
	}
	if u.ID == "" {
		// 引导态的合成 user：有admin 权限但没有真实身份。
		// 允许它建第一个账号 —— 这正是引导窗口存在的意义。
		return u, true
	}
	if !u.IsAdmin() {
		writeError(w, http.StatusForbidden, "需要管理员权限")
		return nil, false
	}
	return u, true
}

func (h *UserHandler) ListUsers(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireAdmin(w, r); !ok {
		return
	}
	ctx := r.Context()
	users, err := h.store.ListUsers(ctx)
	if err != nil {
		writeServerError(w, "list users", err)
		return
	}
	self := ""
	if u := server.UserFromContext(ctx); u != nil {
		self = u.ID
	}
	out := make([]userResponse, 0, len(users))
	for i := range users {
		// 已用量走 SUM 而非读库里的 used_tokens：后者是导入时的快照，
		// 与预检口径（SUM）不一致会让界面数字与实际拦截对不上。
		used, uerr := h.store.SumUserUsedTokens(ctx, users[i].ID)
		if uerr != nil {
			used = users[i].UsedTokens
		}
		keys, kerr := h.store.ListAccessKeysByUser(ctx, users[i].ID)
		count := 0
		if kerr == nil {
			count = len(keys)
		}
		resp := toUserResponse(&users[i], self, count)
		resp.UsedTokens = used
		out = append(out, resp)
	}
	writeJSON(w, http.StatusOK, out)
}

type createUserRequest struct {
	Username    string `json:"username"`
	Password    string `json:"password"`
	DisplayName string `json:"display_name"`
	Role        string `json:"role"`
	GroupID     string `json:"group_id"`
	QuotaTokens int64  `json:"quota_tokens"`
	Remark      string `json:"remark"`
}

// resolveGroupID 校验目标分组存在，返回规范化后的 id。
//
// 空串是合法值（= 不属于任何组，模型不受限）。非空时必须真实存在：
// 写进一个不存在的 id，快照重建时 groupModelAllow 会回落成「不限制」，
// 于是「以为把人放进了一个受限组，实际放得更开」—— 静默放宽，正是
// 要消除的那类失败。
func (h *UserHandler) resolveGroupID(ctx context.Context, raw string) (string, error) {
	id := strings.TrimSpace(raw)
	if id == "" {
		return "", nil
	}
	g, err := h.store.GetGroup(ctx, id)
	if err != nil {
		return "", err
	}
	if g == nil {
		return "", errGroupNotFound
	}
	return id, nil
}

// errGroupNotFound 由 handler 映射成 400（而不是 404）：
// 出问题的是**请求体里的字段**，不是被请求的那个资源。
var errGroupNotFound = errors.New("指定的分组不存在")

// validUsername 校验登录名。
//
// 只允许字母数字与 _ - .，长度 3~32。**刻意不允许中文与空格**：
// 用户名会出现在 URL 路径、日志、以及工单里，含空白或控制字符的
// 登录名能造成一连串难查的问题（复制粘贴带尾空格就登不上）。
func validUsername(s string) bool {
	if len(s) < 3 || len(s) > 32 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '_', r == '-', r == '.':
		default:
			return false
		}
	}
	return true
}

func (h *UserHandler) CreateUser(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireAdmin(w, r); !ok {
		return
	}
	var req createUserRequest
	if err := decodeJSON(w, r, &req); err != nil {
		if errors.Is(err, errUnsupportedMediaType) {
			return
		}
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}

	username := strings.TrimSpace(req.Username)
	if !validUsername(username) {
		writeError(w, http.StatusBadRequest,
			"用户名需为 3-32 位的字母、数字、下划线、连字符或点")
		return
	}
	role := req.Role
	if role == "" {
		role = store.RoleUser
	}
	if role != store.RoleAdmin && role != store.RoleUser {
		writeError(w, http.StatusBadRequest, "role 必须是 admin 或 user")
		return
	}
	if req.QuotaTokens < 0 {
		writeError(w, http.StatusBadRequest, "quota_tokens 不能为负")
		return
	}

	// 密码可以留空（建号后由本人设置，或管理员稍后重置）。
	hash := ""
	if req.Password != "" {
		var err error
		hash, err = userauth.HashPassword(req.Password)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}

	u := &store.User{
		ID:           generateID(),
		Username:     username,
		DisplayName:  strings.TrimSpace(req.DisplayName),
		PasswordHash: hash,
		Role:         role,
		Status:       store.UserStatusActive,
		QuotaTokens:  req.QuotaTokens,
		Remark:       strings.TrimSpace(req.Remark),
	}
	groupID, gerr := h.resolveGroupID(r.Context(), req.GroupID)
	if gerr != nil {
		if errors.Is(gerr, errGroupNotFound) {
			writeError(w, http.StatusBadRequest, gerr.Error())
			return
		}
		writeServerError(w, "resolve group", gerr)
		return
	}
	u.GroupID = groupID

	if err := h.store.CreateUser(r.Context(), u); err != nil {
		if store.IsUniqueViolation(err) {
			writeError(w, http.StatusConflict, "用户名已存在")
			return
		}
		writeServerError(w, "create user", err)
		return
	}
	// 新建用户若带分组，其模型可见性已被收窄，必须重建快照才对数据面生效
	// —— 否则「刚建好就能看到全部模型，过一会儿才变正常」。
	if u.GroupID != "" {
		if err := h.reloadNow(r.Context()); err != nil {
			writeServerError(w, "reload after user create", err)
			return
		}
	}
	writeJSON(w, http.StatusCreated, toUserResponse(u, "", 0))
}

// reloadNow 在注入过 reload 时重建运行时快照（测试里可能没注入）。
func (h *UserHandler) reloadNow(ctx context.Context) error {
	if h.reload == nil {
		return nil
	}
	return h.reload(ctx)
}

type updateUserRequest struct {
	DisplayName *string `json:"display_name"`
	Role        *string `json:"role"`
	Status      *string `json:"status"`
	// GroupID 传空串 = 移出分组（模型可见性变回不限制）；
	// 不传（nil）= 保持原组。两种意图必须能区分，所以用指针。
	GroupID     *string `json:"group_id"`
	QuotaTokens *int64  `json:"quota_tokens"`
	Remark      *string `json:"remark"`
}

// UpdateUser 修改用户。PATCH 语义：nil = 保持原值。
func (h *UserHandler) UpdateUser(w http.ResponseWriter, r *http.Request, id string) {
	if _, ok := requireAdmin(w, r); !ok {
		return
	}
	ctx := r.Context()
	u, err := h.store.GetUser(ctx, id)
	if err != nil {
		writeServerError(w, "get user", err)
		return
	}
	if u == nil {
		writeError(w, http.StatusNotFound, "用户不存在")
		return
	}

	var req updateUserRequest
	if err := decodeJSON(w, r, &req); err != nil {
		if errors.Is(err, errUnsupportedMediaType) {
			return
		}
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}

	// self 标记用于阻止「管理员把自己降级/禁用」这类把自己锁死的操作。
	// 唯一的自锁出口是另一个管理员 —— 局域网小团队里这够用。
	self := server.UserFromContext(ctx)
	isSelf := self != nil && self.ID == id

	bumpVersion := false
	// groupChanged 与 bumpVersion 分开：改分组**不需要**让会话失效
	// （用户的身份没变），但**必须**重建快照（模型可见性随之改变）。
	// 混用一个标志会导致「改个分组把所有人踢下线」这种不必要的副作用。
	groupChanged := false
	if req.DisplayName != nil {
		u.DisplayName = strings.TrimSpace(*req.DisplayName)
	}
	if req.GroupID != nil {
		groupID, gerr := h.resolveGroupID(ctx, *req.GroupID)
		if gerr != nil {
			if errors.Is(gerr, errGroupNotFound) {
				writeError(w, http.StatusBadRequest, gerr.Error())
				return
			}
			writeServerError(w, "resolve group", gerr)
			return
		}
		if groupID != u.GroupID {
			u.GroupID = groupID
			groupChanged = true
		}
	}
	if req.QuotaTokens != nil {
		if *req.QuotaTokens < 0 {
			writeError(w, http.StatusBadRequest, "quota_tokens 不能为负")
			return
		}
		u.QuotaTokens = *req.QuotaTokens
	}
	if req.Remark != nil {
		u.Remark = strings.TrimSpace(*req.Remark)
	}
	if req.Role != nil {
		role := *req.Role
		if role != store.RoleAdmin && role != store.RoleUser {
			writeError(w, http.StatusBadRequest, "role 必须是 admin 或 user")
			return
		}
		if isSelf && role != store.RoleAdmin {
			writeError(w, http.StatusBadRequest, "不能降低自己的权限（会把自己锁在门外）")
			return
		}
		if role != u.Role {
			u.Role = role
			bumpVersion = true // 角色变了，旧会话必须失效
		}
	}
	if req.Status != nil {
		st := *req.Status
		if st != store.UserStatusActive && st != store.UserStatusDisabled {
			writeError(w, http.StatusBadRequest, "status 必须是 active 或 disabled")
			return
		}
		if isSelf && st != store.UserStatusActive {
			writeError(w, http.StatusBadRequest, "不能禁用自己的账号")
			return
		}
		if st != u.Status {
			u.Status = st
			bumpVersion = true // 禁用必须让既有会话立刻失效
		}
	}

	if err := h.store.UpdateUser(ctx, u); err != nil {
		writeNotFoundOrError(w, "update user", "用户不存在", err)
		return
	}
	if bumpVersion {
		if err := h.store.BumpAuthVersion(ctx, id); err != nil {
			writeServerError(w, "bump auth version", err)
			return
		}
		u.AuthVersion++
	}
	// 角色/状态/分组变了都要立刻重建快照：
	//   - 角色/状态 → 被禁用用户的 key 在数据面还能用；
	//   - 分组     → 模型可见性（组白名单）在数据面还是旧的。
	if bumpVersion || groupChanged {
		if err := h.reloadNow(ctx); err != nil {
			writeServerError(w, "reload after user update", err)
			return
		}
	}

	keys, _ := h.store.ListAccessKeysByUser(ctx, id)
	writeJSON(w, http.StatusOK, toUserResponse(u, selfIDOf(self), len(keys)))
}

func (h *UserHandler) DeleteUser(w http.ResponseWriter, r *http.Request, id string) {
	if _, ok := requireAdmin(w, r); !ok {
		return
	}
	self := server.UserFromContext(r.Context())
	if self != nil && self.ID == id {
		writeError(w, http.StatusBadRequest, "不能删除自己的账号")
		return
	}
	if err := h.store.DeleteUser(r.Context(), id); err != nil {
		writeNotFoundOrError(w, "delete user", "用户不存在", err)
		return
	}
	// 名下 key 被外键级联删除，快照里的 KeysByHash 仍有旧条目
	// （虽然它们已被禁用逻辑挡下，但 map 会一直涨）。重建后清掉。
	if h.reload != nil {
		if err := h.reload(r.Context()); err != nil {
			writeServerError(w, "reload after user delete", err)
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

type resetPasswordRequest struct {
	Password string `json:"password"`
}

// ResetPassword 由管理员为某用户设置新密码。
//
// 递增 auth_version 是隐式的（SetUserPassword 内做了）—— 改密码
// 与「让所有旧会话失效」必须是同一个原子动作。
func (h *UserHandler) ResetPassword(w http.ResponseWriter, r *http.Request, id string) {
	if _, ok := requireAdmin(w, r); !ok {
		return
	}
	var req resetPasswordRequest
	if err := decodeJSON(w, r, &req); err != nil {
		if errors.Is(err, errUnsupportedMediaType) {
			return
		}
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if req.Password == "" {
		writeError(w, http.StatusBadRequest, "password 必填")
		return
	}
	hash, err := userauth.HashPassword(req.Password)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := h.store.SetUserPassword(r.Context(), id, hash); err != nil {
		writeNotFoundOrError(w, "reset password", "用户不存在", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"status":  "ok",
		"message": "密码已重置，该用户的所有既有会话已失效",
	})
}

// ---- 自助改密 ----

type changePasswordRequest struct {
	OldPassword string `json:"old_password"`
	NewPassword string `json:"new_password"`
}

// ChangePassword 允许登录用户改自己的密码。
//
// 必须验旧密码：否则一个拿到会话 cookie 的人（比如XSS 后的临时凭据）
// 可以把账号永久锁死 —— 受害者既不知道密码被改了，也没法改回来。
func (h *UserHandler) ChangePassword(w http.ResponseWriter, r *http.Request) {
	u := server.UserFromContext(r.Context())
	if u == nil || u.ID == "" {
		writeError(w, http.StatusUnauthorized, "需要登录")
		return
	}
	var req changePasswordRequest
	if err := decodeJSON(w, r, &req); err != nil {
		if errors.Is(err, errUnsupportedMediaType) {
			return
		}
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if !userauth.VerifyPassword(u.PasswordHash, req.OldPassword) {
		writeError(w, http.StatusUnauthorized, "原密码不正确")
		return
	}
	hash, err := userauth.HashPassword(req.NewPassword)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := h.store.SetUserPassword(r.Context(), u.ID, hash); err != nil {
		writeNotFoundOrError(w, "change password", "用户不存在", err)
		return
	}
	// 改密后自己这个会话也失效了（auth_version 已递增），
	// 前端应跳回登录页。
	server.ClearSessionCookie(w)
	writeJSON(w, http.StatusOK, map[string]string{
		"status":  "ok",
		"message": "密码已更新，请重新登录",
	})
}

func selfIDOf(u *store.User) string {
	if u == nil {
		return ""
	}
	return u.ID
}
