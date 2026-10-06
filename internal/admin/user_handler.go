package admin

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cn-maul/rosetta-gateway/internal/server"
	"github.com/cn-maul/rosetta-gateway/internal/store"
	"github.com/cn-maul/rosetta-gateway/internal/userauth"
)

// UserHandler 承载 users 表之上的全部认证入口：登录 / 登出 / 自身信息，
// 以及首次登录引导（BootstrapStatus / BootstrapSetup，见文件后段）。
// 统一认证后没有第二个 handler、也没有第二条通道 —— 它就是唯一入口。
type UserHandler struct {
	store *store.Store
	mgr   *userauth.Manager
	// reload 在**角色/状态变更后**必须被调用：被禁用用户的 key 在数据面
	// 仍会放行，直到快照重建。留 nil 时（测试）跳过重建。
	reload func(context.Context) error
	// thr 惰性初始化（构造时不建，登录路径上才用）。
	// mu 保护它：登录端点是并发入口，裸读裸写会造出多个限速器，
	// 等于把限速打散成「每请求一个」—— 那就等于没有限速。
	mu  sync.Mutex
	thr *server.FailureThrottle
}

func NewUserHandler(st *store.Store, mgr *userauth.Manager) *UserHandler {
	return &UserHandler{store: st, mgr: mgr}
}

// WithReload 注入运行时重建函数。
func (h *UserHandler) WithReload(fn func(context.Context) error) *UserHandler {
	h.reload = fn
	return h
}

type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type loginResponse struct {
	Token     string `json:"token"`
	ExpiresAt int64  `json:"expires_at"`
	Username  string `json:"username"`
	Role      string `json:"role"`
	// MustSetPassword 为真表示这是个引导出来、还没设过密码的账号，
	// 前端应直接弹「设置密码」而不是进主界面。
	MustSetPassword bool `json:"must_set_password"`
}

type meResponse struct {
	Username        string `json:"username"`
	DisplayName     string `json:"display_name"`
	Role            string `json:"role"`
	Status          string `json:"status"`
	QuotaTokens     int64  `json:"quota_tokens"`
	UsedTokens      int64  `json:"used_tokens"`
	MustSetPassword bool   `json:"must_set_password"`
	IsAdmin         bool   `json:"is_admin"`
	// SessionEnabled 告诉前端能否用密码登录（没配 secret 时为 false）。
	SessionEnabled bool `json:"session_enabled"`
}

// SessionStatus 报告用户会话是否已启用，供登录页决定显示什么。
//
// 该端点必须免鉴权：前端要先知道「这台网关有没有开多用户登录」才能决定
// 是显示登录框还是提示去配置环境变量。响应里只有��个布尔值。
func (h *UserHandler) SessionStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"enabled": h.mgr.Enabled(),
		"env":     userauth.SecretEnvName,
		"min_len": 32,
	})
}

// Login 校验用户名/口令并签发会话令牌。
//
// 失败原因**刻意不细分**（「用户不存在」vs「密码错误」都回同一句话）——
// 细分等于给攻击者一个枚举用户名的 oracle。限速按来源 IP 做（复用
// server.FailureThrottle），对策是拖慢而非完全阻止。
func (h *UserHandler) Login(w http.ResponseWriter, r *http.Request) {
	if !h.mgr.Enabled() {
		writeError(w, http.StatusServiceUnavailable,
			"用户登录未启用：请设置环境变量 "+userauth.SecretEnvName+"（至少 32 个字符）后重启")
		return
	}

	var req loginRequest
	if err := decodeJSON(w, r, &req); err != nil {
		if errors.Is(err, errUnsupportedMediaType) {
			return
		}
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}

	username := strings.TrimSpace(req.Username)
	if username == "" || req.Password == "" {
		writeError(w, http.StatusBadRequest, "用户名与密码必填")
		return
	}

	// 限速：连续失败进冷却。这是拖慢暴力破解，不替代它。
	th := h.throttle()
	if th != nil {
		ip := server.ClientIPOf(r)
		if !th.Allow(ip) {
			writeTooMany(w, th.RetryAfter(ip))
			return
		}
		defer th.Release(ip)
	}

	u, err := h.store.GetUserByUsername(r.Context(), username)
	if err != nil {
		// 查库故障要说 500 而不是「密码错误」—— 前者让运维来查，
		// 后者会让所有人以为是自己密码错了。
		writeServerError(w, "login: get user", err)
		return
	}
	// 引导账号还没设过密码。**必须放在哈希校验之前**：userauth.VerifyPassword
	// 对空哈希直接返回 false，若按原顺序放在后面就永远走不到 ——
	// 这类账号会拿到「用户名或密码错误」（实测确认），语义完全不对：
	// 不是密码错了，而是这个账号**根本没有密码可输**，用户会反复重试。
	//
	// 安全性不变：只有「用户名确实存在 **且** 该账号哈希为空」时才回不同文案，
	// 泄露的是「这个账号还没初始化」，不是「这个账号存在」——
	// 而引导账号的存在本来就是公开事实（启动日志就写了）。
	//
	// 代价：这多出一次可区分的失败分支。接受它 —— 与其让用户对着
	// 「密码错误」无限重试，不如给一条能照着做的指引。
	if u != nil && u.PasswordHash == "" {
		if th != nil {
			th.Fail(server.ClientIPOf(r))
		}
		writeError(w, http.StatusBadRequest,
			"该账号尚未设置密码。若是首次部署，请通过登录页的「首次设置密码」表单完成；"+
				"若管理员已重置过密码，请用新密码登录")
		return
	}

	// 用户不存在与密码错误走同一条路径，且**都做一次哈希校验** ——
	// 否则「用户不存在」会明显更快，响应时间就成了枚举 oracle。
	ok := false
	if u != nil {
		ok = userauth.VerifyPassword(u.PasswordHash, req.Password)
	} else {
		// 拿一个固定的假哈希跑一遍，把两者的耗时拉平。
		userauth.VerifyPassword(dummyHash, req.Password)
	}

	if !ok {
		if th != nil {
			th.Fail(server.ClientIPOf(r))
		}
		writeError(w, http.StatusUnauthorized, "用户名或密码错误")
		return
	}
	if !u.IsActive() {
		if th != nil {
			th.Fail(server.ClientIPOf(r))
		}
		writeError(w, http.StatusForbidden, "账号已被禁用，请联系管理员")
		return
	}

	token, exp, err := h.mgr.Issue(&userauth.UserClaims{
		UserID: u.ID, AuthVersion: u.AuthVersion, Role: u.Role, Username: u.Username,
	})
	if err != nil {
		writeServerError(w, "login: issue token", err)
		return
	}

	if th != nil {
		th.Success(server.ClientIPOf(r))
	}
	server.TouchLogin(h.store, r.Context(), u.ID)
	server.SetSessionCookie(w, token, exp)

	writeJSON(w, http.StatusOK, loginResponse{
		Token:           token,
		ExpiresAt:       exp.UnixMilli(),
		Username:        u.Username,
		Role:            u.Role,
		MustSetPassword: u.PasswordHash == "",
	})
}

// dummyHash 用于「用户不存在」时拉平响应耗时。
//
// 它是一个真实的 PBKDF2 编码（算法/迭代数与真哈希一致），
// 但不对应任何口令 —— 用它校验必然失败，而这正是我们要的。
// 若直接 return 跳过哈希，「用户不存在」会比「密码错误」快几十毫秒
// （21 万次迭代的成本），足够脚本据此枚举出有效用户名。
const dummyHash = "pbkdf2-sha256$210000$AAAAAAAAAAAAAAAAAAAAAA==$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="

// throttle 返回本 handler 持有的登录限速器（惰性建一次）。
//
// 限速状态必须**跨请求**保持，所以它挂在 handler 上而不是 context 上 ——
// 每个请求新建一个限速器等于没有限速。
func (h *UserHandler) throttle() *server.FailureThrottle {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.thr == nil {
		h.thr = server.NewFailureThrottle(server.LoginFailLimit, server.LoginCooldown)
	}
	return h.thr
}

func writeTooMany(w http.ResponseWriter, retryAfter time.Duration) {
	seconds := int(retryAfter.Seconds()) + 1
	w.Header().Set("Retry-After", strconv.Itoa(seconds))
	writeError(w, http.StatusTooManyRequests, "登录尝试过于频繁，请稍后再试")
}

// ---- 首次登录引导 ----
//
// 统一认证后（admin_token 与 admin_auth.json 两条通道都已删除），整个系统
// 只有一种身份来源：users 表。启动时若 users 表为空，会建出一个
// password_hash 为空的 admin 账号（见 cmd/gateway 的 ensureBootstrapAdmin）。
//
// 这个账号**登不进去**（空哈希过不了校验），但它让「第一个管理员」这件事
// 有了明确的落点。BootstrapStatus 告诉前端「现在该显示设密码表单」，
// BootstrapSetup 负责真正设置并直接签发会话 —— 设完即可用，无第二个步骤。

// bootstrapStatusResponse 是首次登录引导的状态。
type bootstrapStatusResponse struct {
	// NeedsSetup 为真表示存在一个已建出但未设密码的管理员，
	// 前端应把登录页换成「设置密码」表单。
	NeedsSetup bool `json:"needs_setup"`
	// Username 是那个待初始化的账号名，供前端显示「为 admin 设置密码」。
	Username string `json:"username,omitempty"`
	// SessionEnabled 恒为 true（会话密钥现在自动生成），保留字段是为了
	// 前端不必区分「没配 secret」这种已不存在的状态。
	SessionEnabled bool `json:"session_enabled"`
}

// BootstrapStatus 报告是否需要首次设置密码。该端点**免鉴权**：
// 它的用途恰恰是「还没有任何身份时」决定界面长什么样。
//
// 泄露面评估：只暴露「有没有一个待初始化管理员」与它的用户名。
// 两者都是公开事实（启动日志就写了账号名），且不提供任何可用于登录的信息。
// 与 password/check 的区别是语义完全不同 —— 后者已随双通道一起删除。
func (h *UserHandler) BootstrapStatus(w http.ResponseWriter, r *http.Request) {
	u, err := h.store.FindUninitializedAdmin(r.Context())
	if err != nil {
		// 查不到状态不该让界面显示「不需要设密码」——那会把用户送去
		// 一个必然失败的登录框。报 500，前端显示「无法连接」。
		writeServerError(w, "bootstrap status: find uninitialized admin", err)
		return
	}
	writeJSON(w, http.StatusOK, bootstrapStatusResponse{
		NeedsSetup:     u != nil,
		Username:       usernameOrEmpty(u),
		SessionEnabled: h.mgr.Enabled(),
	})
}

func usernameOrEmpty(u *store.User) string {
	if u == nil {
		return ""
	}
	return u.Username
}

type bootstrapSetupRequest struct {
	Password string `json:"password"`
}

// BootstrapSetup 为待初始化的管理员设置密码并**直接签发会话**。
//
// # 为什么设完就登录，而不是让用户再登一次
//
// 引导账号此前只有一条出路：用 admin_token 进后台 → 找到 admin → 重置密码。
// 三步，且中间那步需要知道「去用户管理里找谁」。统一认证后那条出路消失了，
// 引导必须自成闭环：**设完即可用**。
//
// # 安全性
//
// 免鉴权写接口，但受三重限制：
//  1. 只对「role=admin 且 password_hash 为空」的那个账号有效 ——
//     引导完成后 FindUninitializedAdmin 返回 nil，此端点即失效。
//  2. 一旦该账号设过密码，再次调用 → 409，不是覆盖。
//  3. decodeJSON 强制 Content-Type: application/json（CSRF 防线，
//     见 helpers.go 的说明），且上面叠一层同源判定。
//
// 残留风险要说清楚：**全新部署且监听非回环时，任何能连到端口的人
// 都能抢先成为第一个管理员**。这与旧实现「无凭据时 password/set 免鉴权」
// 的暴露面完全相同 —— 统一认证没有把它变大，只是把它挪了个位置。
// 真正的缓解是绑回环（默认 listen 就是），以及首次设密后立即失效。
func (h *UserHandler) BootstrapSetup(w http.ResponseWriter, r *http.Request) {
	// 引导窗口是「无凭据可校验」时开的，必须防跨站抢注 —— 与
	// password/set 当年的同源判定同理由。放在最前面：不同源的请求
	// 连查库都不该做。
	if !server.SameOrigin(r) {
		server.WriteForbidden(w)
		return
	}

	var req bootstrapSetupRequest
	if err := decodeJSON(w, r, &req); err != nil {
		if errors.Is(err, errUnsupportedMediaType) {
			return
		}
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}

	u, err := h.store.FindUninitializedAdmin(r.Context())
	if err != nil {
		writeServerError(w, "bootstrap setup: find uninitialized admin", err)
		return
	}
	if u == nil {
		// 已初始化：这是「重复提交」（用户连点两次、或脚本重试），
		// 不是「越权」—— 但也不能静默成功，否则调用方以为密码设上了。
		writeError(w, http.StatusConflict, "管理员密码已设置，无需重复初始化")
		return
	}

	// 限速与登录端点同源：引导接口也是无凭据的写入口。
	th := h.throttle()
	if th != nil {
		ip := server.ClientIPOf(r)
		if !th.Allow(ip) {
			writeTooMany(w, th.RetryAfter(ip))
			return
		}
		defer th.Release(ip)
	}

	hash, err := userauth.HashPassword(req.Password)
	if err != nil {
		// 弱口令在派生之前就被拒（HashPassword 内部先 validatePassword），
		// 不消耗 KDF。
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	// SetInitialAdminPassword 在 SQL 里带上 AND password_hash = ''：设过即不写。
	// 上面的 FindUninitializedAdmin 判定与这里的写入是两个时刻，「判定通过」
	// 不保证「写入时仍然未设」—— 并发下（或与本判定之后发生的正常设置竞跑）
	// 后写者会静默覆盖先写者。条件 UPDATE 让「只设一次」由数据库原子保证，
	// 0 行受影响即说明窗口已在两条指令之间关闭。
	ok, err := h.store.SetInitialAdminPassword(r.Context(), u.ID, hash)
	if err != nil {
		writeServerError(w, "bootstrap setup: set password", err)
		return
	}
	if !ok {
		writeError(w, http.StatusConflict, "管理员密码已设置，无需重复初始化")
		return
	}
	if th != nil {
		th.Success(server.ClientIPOf(r))
	}

	// 重新读一次：SetUserPassword 已递增 auth_version，签发的令牌
	// 必须带**新版本号**，否则前端刚拿到手就 401。
	fresh, err := h.store.GetUser(r.Context(), u.ID)
	if err != nil || fresh == nil {
		writeServerError(w, "bootstrap setup: reload user", err)
		return
	}
	token, exp, err := h.mgr.Issue(&userauth.UserClaims{
		UserID: fresh.ID, AuthVersion: fresh.AuthVersion,
		Role: fresh.Role, Username: fresh.Username,
	})
	if err != nil {
		writeServerError(w, "bootstrap setup: issue token", err)
		return
	}
	server.TouchLogin(h.store, r.Context(), fresh.ID)
	server.SetSessionCookie(w, token, exp)
	writeJSON(w, http.StatusOK, loginResponse{
		Token:     token,
		ExpiresAt: exp.UnixMilli(),
		Username:  fresh.Username,
		Role:      fresh.Role,
		// 设完了，不再需要「必须设密码」这个标记。
		MustSetPassword: false,
	})
}

// Logout 清除会话 cookie。
//
// 无状态 JWT 的登出天然是「客户端不再携带」—— 服务端不维护黑名单
// （见 userauth.Manager 的注释）。要真正作废某张令牌，改密码或禁用账号
// 即可让 auth_version 前进，所有旧令牌一次性失效。
func (h *UserHandler) Logout(w http.ResponseWriter, r *http.Request) {
	server.ClearSessionCookie(w)
	writeJSON(w, http.StatusOK, map[string]string{
		"status":  "ok",
		"message": "已登出",
	})
}

// Me 返回当前登录用户的信息。
func (h *UserHandler) Me(w http.ResponseWriter, r *http.Request) {
	u := server.UserFromContext(r.Context())
	if u == nil {
		writeError(w, http.StatusUnauthorized, "需要登录")
		return
	}
	// 引导态下的合成 user 没有 ID，不返回任何真实数据。
	if u.ID == "" {
		writeJSON(w, http.StatusOK, meResponse{
			Username: "(setup)", Role: u.Role, Status: u.Status,
			SessionEnabled: h.mgr.Enabled(), IsAdmin: true,
		})
		return
	}

	used, err := h.store.SumUserUsedTokens(r.Context(), u.ID)
	if err != nil {
		// 用量查不到不该让「我是谁」也查不出来 —— 用 0 顶上，日志留痕。
		// 注意降级**不能**走 writeServerError：那会把 500 状态头和错误体写出去，
		// 而函数继续往下走还会再写一份 200 响应 —— 客户端收到两段拼接的 JSON，
		// 解析必然失败。降级就是降级，只进日志，不进响应。
		slog.Warn("me: sum used tokens failed; degrading used_tokens to 0", "error", err)
		used = 0
	}

	writeJSON(w, http.StatusOK, meResponse{
		Username:        u.Username,
		DisplayName:     u.DisplayName,
		Role:            u.Role,
		Status:          u.Status,
		QuotaTokens:     u.QuotaTokens,
		UsedTokens:      used,
		MustSetPassword: u.PasswordHash == "",
		IsAdmin:         u.IsAdmin(),
		SessionEnabled:  h.mgr.Enabled(),
	})
}
