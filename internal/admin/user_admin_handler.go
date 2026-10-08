package admin

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
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
	// BalanceCents 是账户余额（**分**，人民币）。与 quota_tokens 的「0 = 不限」
	// 刻意相反：**unlimited=true 才是不限额**，0 分是「真的一分钱都没有」，
	// 两种状态一个放行一个 402，绝不能塌成同一个值。判定一律看 unlimited。
	//
	// 前端必须据此显示「不限」而不是「0.00 元」—— 后者会被读成「没钱了」，
	// 而实际含义是「不受余额限制」。
	BalanceCents int64 `json:"balance_cents"`
	Unlimited    bool  `json:"balance_unlimited"`
	// BalanceRemainder 是**不足一分**的待结算余数（微元）。
	//
	// 余额按分扣减，而单价可能远低于一分（实测 3 元/百万 token 时，一次
	// 一万 token 的调用只有 5 厘），于是会有相当长一段时间里余额纹丝不动。
	// 前端必须把它一起显示出来，否则用户看到「余额没变」会以为没扣钱 ——
	// 而实际上钱已消费，只是还没攒够一分。语义与 balance_cents 正交。
	BalanceRemainder int64  `json:"balance_remainder"`
	AuthVersion      int64  `json:"auth_version"`
	Remark           string `json:"remark,omitempty"`
	// HasPassword 为false 时前端应显示「未设置密码」并引导去设置。
	HasPassword bool  `json:"has_password"`
	KeyCount    int   `json:"key_count"`
	CreatedAt   int64 `json:"created_at"`
	LastLoginAt int64 `json:"last_login_at"`
	// IsSelf 标记这是不是当前登录者自己（前端据此禁用自删按钮）。
	IsSelf bool `json:"is_self"`
}

func toUserResponse(u *store.User, selfID string, keyCount int) userResponse {
	// 管理员账户**没有余额语义**（2026-10-11）。
	//
	// 管理员不建 key、不调 API，所以它既不会产生扣费，也不该被充值 ——
	// AdjustBalance 已经拒绝「给自己充值」。那库里那一列对 admin 行就是纯粹的
	// 历史残留：可能是升级前充值留下的，也可能压根是 NULL。
	//
	// 统一在这里归一成「不限额 + 0 分 + 无余数」，而不是把真实值透出去：
	// 透出去的话，管理员在用户列表里会看到一个自己从不消耗的数字，
	// 让人以为「我还有钱没花」或「我欠着钱」，而这两种状态都不存在。
	//
	// 收在这一处而不是各 handler 里手改：toUserResponse 是**唯一**的构造点
	// （ListUsers / AdjustBalance 共用），漏一处就会出现「列表里是 0、
	// 充值响应里是真余额」这种同一账号两个答案的矛盾。
	if u.Role == store.RoleAdmin {
		return userResponse{
			ID:               u.ID,
			Username:         u.Username,
			DisplayName:      u.DisplayName,
			Role:             u.Role,
			Status:           u.Status,
			GroupID:          u.GroupID,
			QuotaTokens:      u.QuotaTokens,
			UsedTokens:       u.UsedTokens,
			BalanceCents:     0,
			Unlimited:        true, // 「不限额」= 不参与余额计费
			BalanceRemainder: 0,
			AuthVersion:      u.AuthVersion,
			Remark:           u.Remark,
			HasPassword:      u.PasswordHash != "",
			KeyCount:         keyCount,
			CreatedAt:        u.CreatedAt,
			LastLoginAt:      u.LastLoginAt,
			IsSelf:           u.ID == selfID,
		}
	}
	return userResponse{
		ID:          u.ID,
		Username:    u.Username,
		DisplayName: u.DisplayName,
		Role:        u.Role,
		Status:      u.Status,
		GroupID:     u.GroupID,
		QuotaTokens: u.QuotaTokens,
		UsedTokens:  u.UsedTokens,
		// 余额直接取 store.User 的两个字段：Unlimited 是**列值为 NULL** 的
		// 表示（见 store.User.BalanceCents），不是「数值为 0」。手写 DTO 的
		// 好处正在这里 —— store.User 没有 json tag，漏填一个字段只会安静地
		// 在 JSON 里少一个键，前端于是读到 undefined 而显示成「0 元」。
		BalanceCents: u.BalanceCents,
		Unlimited:    u.Unlimited,
		// 余数同样是「直接取、不手写换算」：手写 DTO 的好处正在这里，
		// 漏填只会安静地少一个键，而单位换算写错则是**数字错**。
		BalanceRemainder: u.BalanceRemainder,
		AuthVersion:      u.AuthVersion,
		Remark:           u.Remark,
		HasPassword:      u.PasswordHash != "",
		KeyCount:         keyCount,
		CreatedAt:        u.CreatedAt,
		LastLoginAt:      u.LastLoginAt,
		IsSelf:           u.ID == selfID,
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

	// 密码对普通用户可以留空：空密码账号登不进去，由管理员稍后重置（普通
	// 用户没有自助设密通道，那等于绕过管理员）。**admin 没有留空的资格**：
	// 空密码 admin 会让引导判定（存在 role=admin 且 password_hash='' 的账号）
	// 重新命中，而免鉴权的 POST /admin/api/bootstrap 恰好认这个条件 ——
	// 任何能连到端口的人都能给该账号设上自己的密码并直接拿到 admin 会话。
	// 在产生的源头堵住（一次性 marker 是第二层防线，见 user_handler.go）。
	hash := ""
	if req.Password != "" {
		var err error
		hash, err = userauth.HashPassword(req.Password)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	} else if role == store.RoleAdmin {
		writeError(w, http.StatusBadRequest, "创建管理员必须设置初始密码（admin 不允许空密码账号）")
		return
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
		// 新建用户的余额一律是**不限额**（NULL），不是 0 分。
		//
		// 漏掉这一行会让 Unlimited 保持 Go 零值 false，于是 balanceValue
		// 把 0 写进库 —— 新账号一建出来就是「余额 0 分」，下一次调用立刻 402。
		// 而存量用户经迁移是 NULL（不限额），两者行为不一致：升级前的账号
		// 能正常用，升级后新建的账号全部欠费，且没有任何报错线索指向这里。
		//
		// 「建号时给个初始额度」是另一件事，由管理员随后调
		// PUT /users/{id}/balance 显式做，不该由创建路径偷偷决定。
		Unlimited: true,
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
	// 新建用户若带分组，其模型可见性已被收窄，需要重建快照才对数据面生效。
	// 重建统一由外层 server.AutoReload 负责，handler 不再自己调：此前这里的
	// 同步重建一旦失败就回 500，而 AutoReload 对 >=400 的响应直接返回 ——
	// 既不审计也不 MarkDirty，后台兜底完全不触发，库里的写却已提交，数据面
	// 继续按旧快照放行（这正是本缺陷的形态）。重建的唯一所有者是 AutoReload。
	writeJSON(w, http.StatusCreated, toUserResponse(u, "", 0))
}

// reloadNow 保留但已无调用点：运行时快照的重建统一由外层 server.AutoReload
// 负责（成功触发一次；失败置脏，交 cmd/gateway 的后台重试兜底收敛）。
// 此前各 handler 自己同步重建，失败即回 500，恰好绕过 AutoReload 的
// 兜底路径。函数与 reload 字段仅为兼容构造注入面保留（cmd/gateway/main.go
// 仍经 WithReload 注入），新代码不要再调用。
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
	// 改分组与改角色/状态的语义边界：分组变化**不需要** bumpVersion ——
	// 用户的身份没变，把会话踢下线是过度反应；它需要的是重建快照
	// （模型可见性随之改变），生效由外层 server.AutoReload 统一承担。
	// 这个边界此前由独立的 groupChanged 标志表达，重建收口到 AutoReload
	// 后 handler 不再需要该标志，但「不混用两种失效语义」的原则不变。
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
			// 只改分组：不 bumpVersion（既有会话仍有效），模型可见性的生效
			// 交给 AutoReload 的快照重建。
			u.GroupID = groupID
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
		// 升级成 admin 前必须已有密码：空密码账号不能当管理员 —— 它会让
		// 免鉴权引导端点（POST /admin/api/bootstrap）的判定重新命中，任何
		// 能连到端口的人都能借道给它设上自己的密码并拿到 admin 会话。
		// 只拦「user → admin」这一步；存量空密码 admin 改其它字段不受影响
		// （那样只会把人锁死，堵不住任何口子）。拒绝而非代设密码：设密码
		// 是管理员的显式动作（ResetPassword），顺手代劳会掩盖真实意图。
		if role == store.RoleAdmin && u.Role != store.RoleAdmin && u.PasswordHash == "" {
			writeError(w, http.StatusBadRequest,
				"该账号尚未设置密码，请先为其重置密码，再升级为管理员")
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

	// store.UpdateUser 刻意不写 auth_version（见 store.UpdateUser 注释）：
	// 这里的 u 是读库时的旧快照，若随 UPDATE 把快照里的旧版本号写回，
	// 读库与落库之间发生的改密（auth_version 已原子递增）会被覆盖回旧值，
	// 改密前被窃取的旧 JWT 重新通过校验。角色/状态变更需要的会话失效
	// 由下面的 BumpAuthVersion 承担 —— 它在 SQL 里相对递增，不依赖快照。
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
	// 角色/状态/分组变了都需要重建快照才能对数据面生效：
	//   - 角色/状态 → 被禁用用户的 key 在数据面还能用；
	//   - 分组     → 模型可见性（组白名单）在数据面还是旧的。
	// 重建统一由外层 server.AutoReload 负责：handler 内的同步重建一旦失败
	// 就回 500，AutoReload 对 >=400 的响应直接返回 —— 既不审计也不 MarkDirty，
	// 后台兜底完全不触发，而库里的写已提交（与 CreateUser 同一缺陷形态）。

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
	// 名下 key 被外键级联删除，快照里的 KeysByHash 仍有旧条目（虽然它们已被
	// 禁用逻辑挡下，但 map 会一直涨），重建后清掉。重建统一由外层
	// server.AutoReload 负责：handler 内的同步重建失败会回 500，恰好绕过
	// AutoReload 的审计与置脏兜底（与 CreateUser/UpdateUser 同一缺陷形态）。
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

// ---- 余额充值 ----

type adjustBalanceRequest struct {
	// DeltaCents 是**相对增减**（分）。正数=充值，负数=扣减。
	//
	// 用 delta 而不是「设置绝对值」是刻意的：绝对值接口上一次误操作
	// （比如把 5000 分打成 50 分）会直接把余额清零，而充值是**高频且
	// 意图明确**的操作，相对调整不可能产生这种结果 —— 打错也只是多充/少充。
	//
	// 用指针区分「没传」与「传 0」：传 0 是合法的「调平」动作，不传则是
	// 请求畸形，两者的处置完全不同（后者要 400，不能静默当成「调平」）。
	DeltaCents *int64 `json:"delta_cents"`
	// Remark 是可选备注（充值原因 / 工单号），写进充值流水供事后追溯。
	// 不参与任何计算，纯留痕。
	Remark string `json:"remark"`
}

// AdjustBalance 管理员给某用户充值/扣减余额。
//
// # 为什么是「相对调整」而不是「设置绝对值」
//
// 充值界面上填的是「充 100 元」，不是「把余额设成 100 元」。两者在界面上
// 只差一个字段的语义，在事故上的差别是决定性的：绝对值接口上一次手抖
// 就能把别人账上的钱清零，而且没有任何东西拦得住它。
//
// 底层的 store.AdjustBalance 用 `balance_cents = MAX(0, COALESCE(…) + ?)`
// 在 SQL 里原子自增，天然免疫「读-改-写」的丢失更新：两个管理员并发充值
// 都生效，不会后写者覆盖先写者。
//
// # 不限额用户的特殊行为（store.AdjustBalance 里有完整说明）
//
// COALESCE 把 NULL 当 0 起算，于是「给一个不限额用户充值 +100 元」会**把
// 不限额切成 100 元**。这是刻意的：给不限额用户充值的意图通常正是「给他设个
// 额度」，而静默无操作会让人以为充值失败。前端因此必须把这条后果写在
// 充值按钮旁边（见 Users.vue）。
//
// 余额变更**不**需要重建快照：预检与扣费都直接读库（store.BalanceOf），
// 不用快照。加了重建只会白白多一次全量 RebuildFromDB。
func (h *UserHandler) AdjustBalance(w http.ResponseWriter, r *http.Request, id string) {
	self, ok := requireAdmin(w, r)
	if !ok {
		return
	}
	// 管理员**不能给自己充值**（2026-10-11）。
	//
	// 管理员账户不再有余额语义：它不建 key、不调 API（见 main.go 的数据面
	// 拦截与 key_handler.Create），于是「给自己充钱」这件事在产品上没有任何
	// 用途 —— 充进去的钱既不会被扣，也没有界面会把它花掉。
	//
	// 但**必须在这里挡**，不能只靠前端不显示这个选项：
	//  ① `requireAdmin` 已经返回了当前管理员，self 就在手边，判定是免费的；
	//  ② 这是**权限边界**而不是界面偏好 —— 前端隐藏按钮对 curl / SDK / 旧版
	//     前端一律无效，而 PUT /users/{id}/balance 是公开的管理端点，
	//     一个管理员 id 完全可以自己构造出来；
	//  ③ 更实质的理由：一旦 admin 有了余额，它就会出现在「用户」列表里 ——
	//     那张表对所有管理员可见，于是界面上出现「给自己充值」这一格，
	//     而那笔账没有任何对应消费，是一笔纯噪声流水。
	//
	// 比较用的是 **id** 而不是用户名：同名账号不可能并存（users 表
	// username 是 COLLATE NOCASE UNIQUE），但 id 比较不依赖任何字符串口径。
	//
	// 空 id 的引导态合成 user（requireAdmin 对它直接放行，见其注释）
	// 同样被这条拦下：它没有真实身份，本来也不该有余额。
	if id == "" || self.ID == id {
		writeError(w, http.StatusBadRequest,
			"管理员账户不能充值：管理员不参与计费，请为普通用户账户充值")
		return
	}
	var req adjustBalanceRequest
	if err := decodeJSON(w, r, &req); err != nil {
		if errors.Is(err, errUnsupportedMediaType) {
			return
		}
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if req.DeltaCents == nil {
		writeError(w, http.StatusBadRequest, "delta_cents 必填（单位：分，正数充值 / 负数扣减）")
		return
	}
	delta := *req.DeltaCents
	if delta == 0 {
		// 0 是合法的「调平」，但作为一次 API 调用毫无意义，且更可能是
		// 前端把空输入框当成了 0。明确拒绝比默默成功更容易发现 bug。
		writeError(w, http.StatusBadRequest, "delta_cents 不能为 0")
		return
	}
	// 上限保护：负 delta 大于当前余额时，SQL 侧的 MAX(0, …) 会把它夹到 0，
	// 那是一次**部分成功**的扣减 —— 管理员以为扣了 5000 分、实际只扣到 0。
	// 这里先读一次余额把这种情况挡在门外（读-判-写不是原子的，但这条只是
	// 防止误操作，不是资金正确性的保证；真正的并发安全由 SQL 自增负责）。
	cents, limited, err := h.store.BalanceOf(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "用户不存在")
			return
		}
		writeServerError(w, "adjust balance: read", err)
		return
	}
	if delta < 0 && limited && cents+delta < 0 {
		writeError(w, http.StatusBadRequest,
			"扣减金额超过当前余额（余额 "+formatCents(cents)+" 元）")
		return
	}

	// 改余额**并记一条充值流水**。
	//
	// 用 AdjustBalanceWithLedger 而不是老的 AdjustBalance：后者只改余额、
	// 不留任何痕迹，于是「谁给谁充了多少钱」事后无从查起（audit_log 只存
	// 字段名不存值，说不出金额）。
	//
	// 操作者取 self（requireAdmin 的返回值，就是当前会话身份）。余额是钱，
	// 「谁给的」必须能追溯到具体账号。流水写失败时本方法**仍返回成功**
	// （余额确实已改，报错会诱导管理员重复充值 —— 那才是真的多充钱），
	// 失败已由 store 层记 ERROR，见 AdjustBalanceWithLedger 的注释。
	if _, err := h.store.AdjustBalanceWithLedger(r.Context(), id, delta,
		self.ID, self.Username, req.Remark); err != nil {
		writeNotFoundOrError(w, "adjust balance", "用户不存在", err)
		return
	}
	// 回读一次再返回：响应里的余额必须是**调整后**的值，而 AdjustBalance
	// 只返回 error。让前端自己减一下 delta 的话，扣减撞到 0 时界面会显示
	// 一个负数（等于把「已夹到 0」这件事藏起来了）。
	after, afterLimited, aerr := h.store.BalanceOf(r.Context(), id)
	if aerr != nil {
		writeServerError(w, "adjust balance: reread", aerr)
		return
	}
	u, uerr := h.store.GetUser(r.Context(), id)
	if uerr != nil || u == nil {
		writeNotFoundOrError(w, "adjust balance: get user", "用户不存在", uerr)
		return
	}
	keys, _ := h.store.ListAccessKeysByUser(r.Context(), id)
	// 复用开头 requireAdmin 返回的 self，不再从 context 里取第二次 ——
	// requireAdmin 读的就是同一个 context.UserFromContext，两处必然相等。
	resp := toUserResponse(u, selfIDOf(self), len(keys))
	// **以回读的值为准**，不用 UpdateUser 之前那次读到的 u.BalanceCents：
	// 充值动作本身（AdjustBalance）可能已把一个 NULL（不限额）切成有限额，
	// 而 u 仍是调整前读出来的旧快照。BalanceOf 的 limited 是「有限额」的
	// **正向**说法（store 层刻意如此，避免与 u.Unlimited 并排时读反），
	// 这里显式反转。
	resp.Unlimited = !afterLimited
	resp.BalanceCents = after
	writeJSON(w, http.StatusOK, resp)
}

// formatCents 把「分」渲染成「元」的中文错误文案用串。
//
// 刻意不复用前端的 fmtMoney：那是展示层格式化（带千分位、小额留 4 位），
// 而这里的数字要放进一句错误提示里，格式必须与账目口径一致（两位小数即可，
// 因为它只用于「余额不足 500.00 元」这类说明，不参与对账）。
func formatCents(cents int64) string {
	return strconv.FormatInt(cents/100, 10) + "." + fmt.Sprintf("%02d", cents%100)
}

// ---- 充值流水查询 ----

// 充值流水的分页口径：默认 50，上限 200。
//
// 默认值与前端 Wallet.vue 的 api.topups() 默认值一致（limit=50）。
// 上限比用量的 1000 小：充值记录是**低频**事件（一个月可能只有几笔），
// 上限开太大只是给了一个可被用来物化整表的入口。
const (
	defaultTopupLimit = 50
	maxTopupLimit     = 200
)

// topupRecordOut 是单条充值流水的对外形状。
//
// 相对 store.Topup 只多一个 username：被充值用户的**用户名**。
// 管理员看这一页时问的是「我给谁充了钱」，让他拿 user_id 去用户表里比对
// 是这一页独有的额外一步 —— 而这一页恰好是唯一会出现别人账号的地方。
// 用户名在写流水那一刻固化（与 operator_username 同一理由：账号可能被改名
// 或删除，流水不该因主体消失而失去可读性）。
type topupRecordOut struct {
	store.Topup
	Username string `json:"username"`
}

// ListTopups 查询充值流水（**唯一一个**查询端点）。
//
// # 作用域由会话身份决定，不接受请求参数指定查谁
//
//   - 普通用户 → 只能看到自己的（自己没流水就返回空数组）；
//   - 管理员   → 看**全站**（2026-10-10 改）。
//
// 管理员看全站是需求决定的：他的钱包页要回答「我给所有用户充过多少钱」，
// 而他**不能被充值**（自充值被本 handler 拒绝），所以「按 me.ID 查」得到
// 的一定是空列表 —— 那个视图恒为空，需求就落不了地。
//
// 为什么不读 ?user_id=：那等于任何普通用户传个别人的 id 就能看别人的钱。
// 作用域只能由**服务端**根据会话身份判定（与 usage_handler.callerScope 同一原则）。
//
// 判空身份时返回 401 而不是空数组：拿不到身份却返回「空列表」会被前端
// 渲染成「你没有充值记录」，把一次鉴权故障伪装成正常状态。引导态合成 user
// （ID 为空）同理 —— 它没有真实身份，它的「充值记录」无从谈起。
func (h *UserHandler) ListTopups(w http.ResponseWriter, r *http.Request) {
	me := server.UserFromContext(r.Context())
	if me == nil || me.ID == "" {
		writeError(w, http.StatusUnauthorized, "需要登录")
		return
	}
	q := r.URL.Query()
	limit := clampLimit(q.Get("limit"), defaultTopupLimit, maxTopupLimit)
	// offset 也要钳制：负数在 SQLite 的 LIMIT/OFFSET 里语义不友好，
	// 显式归零更稳（clampLimit 对 <=0 一律回落默认值 0）。
	offset := clampLimit(q.Get("offset"), 0, 1<<31)

	ctx := r.Context()
	adminScope := me.IsAdmin()

	var list []store.Topup
	var total int64
	var err error
	if adminScope {
		list, err = h.store.ListAllTopups(ctx, limit, offset)
		if err == nil {
			total, err = h.store.CountAllTopups(ctx)
		}
	} else {
		list, err = h.store.ListTopupsByUser(ctx, me.ID, limit, offset)
		if err == nil {
			total, err = h.store.CountTopupsByUser(ctx, me.ID)
		}
	}
	if err != nil {
		writeServerError(w, "list topups", err)
		return
	}

	// 补上被充值者的用户名。管理员视图是**跨用户**的，所以用户名必须
	// 逐条查；普通用户视图恒为本人，一次就够。
	//
	// 为什么不一次性 JOIN users：见 CountGroupMembers 的注释 —— 全表读会把
	// 所有用户的密码哈希扫进内存，那是一个展示字段不该付出的代价。
	// 这里只读**当页这 limit 条**涉及的少数用户，且带缓存，避免翻页时
	// 同一页里重复查同一个用户。
	//
	// 查询失败不阻断：拿不到用户名就退化成显示 id，列表仍然可读 ——
	// 一个辅助字段缺失不该让整页查不出来。
	nameCache := map[string]string{}
	nameOf := func(id string) string {
		if n, ok := nameCache[id]; ok {
			return n
		}
		n := id
		if u, gerr := h.store.GetUser(ctx, id); gerr == nil && u != nil && u.Username != "" {
			n = u.Username
		}
		nameCache[id] = n
		return n
	}
	if !adminScope {
		nameCache[me.ID] = nameOf(me.ID)
	}

	out := make([]topupRecordOut, 0, len(list))
	for _, tp := range list {
		out = append(out, topupRecordOut{Topup: tp, Username: nameOf(tp.UserID)})
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"records": out,
		"total":   total,
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

	// 旧密码校验必须限速，且按**用户 ID** 而非 IP 归键。
	//
	// 会话被临时窃取时（XSS 后的凭据），攻击者已能无限次调用本端点；
	// 没有限速时他可以对着旧密码做在线爆破，撞中即永久改掉密码、
	// 把受害者锁在门外。会话本身不携带失败计数，这是那条防护的缺口。
	//
	// 用与登录端点同族的 FailureThrottle（成功即释限），但独立实例 ——
	// 共用会让「改密输错几次」把该 IP 的正常登录一起锁掉。
	thr := h.throttleForPassword()
	if !thr.Allow(u.ID) {
		w.Header().Set("Retry-After", strconv.Itoa(int(thr.RetryAfter(u.ID).Seconds())+1))
		writeError(w, http.StatusTooManyRequests, "原密码尝试过于频繁，请稍后再试")
		return
	}
	// 必须与 Allow 配对（2026-10-10 修复的 P1）：Allow 占用的并发额度
	// 只有 Fail 与 Release 会归还，而 Success **刻意不动** inflight
	// （它要保留条目给仍在跑的并发请求）。所以缺了这一行，每成功校验一次
	// 旧密码就永久漏掉一个额度。
	//
	// 最容易触发的方式甚至不需要改密成功：旧密码校验通过 → Success →
	// 新密码太弱 → HashPassword 报错 → 400 返回，额度已经漏了。四次之后
	// inflight 触顶，该账号**永久**无法改密，报的却是「原密码尝试过于频繁」
	// —— 没有一次失败尝试发生过，这句提示纯属误导。
	//
	// 而限速按用户 ID 归键，这口「漏」跨登出、重登、乃至改密本身递增的
	// auth_version 都不会自愈（sweepLocked 也不回收：它只在条目数超阈值时
	// 清理，且这里 until 为零值）。
	defer thr.Release(u.ID)

	if !userauth.VerifyPassword(u.PasswordHash, req.OldPassword) {
		thr.Fail(u.ID)
		writeError(w, http.StatusUnauthorized, "原密码不正确")
		return
	}
	thr.Success(u.ID)
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
