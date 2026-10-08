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
	// reload 是运行时快照重建的注入点。**重建的唯一所有者是外层
	// server.AutoReload**（写操作成功后触发；失败置脏，交 cmd/gateway 的
	// 后台重试兜底）—— 本 handler 的端点不再自行调用重建：handler 内的
	// 同步重建一旦失败，响应变 500，恰好绕过 AutoReload 的审计与置脏路径。
	// 字段与 WithReload 仅为兼容 cmd/gateway 的构造注入面保留，留 nil 无副作用。
	reload func(context.Context) error
	// thr 惰性初始化（构造时不建，登录路径上才用）。
	// mu 保护它：登录端点是并发入口，裸读裸写会造出多个限速器，
	// 等于把限速打散成「每请求一个」—— 那就等于没有限速。
	mu  sync.Mutex
	thr *server.FailureThrottle
	// pwThr 是**改密专用**的限速器，按用户 ID 而非 IP 归键。
	//
	// 为什么与登录限速分开：登录限速按 IP，保护的是「猜密码」；
	// 改密限速保护的是「已窃获会话后在线爆破旧密码」—— 攻击者换 IP
	// 就该被挡住，按用户归键才做得到。共用一个会让「改密失败几次」
	// 把该IP 的正常登录一起锁掉。
	//
	// 不用另一个 mu：pwThr 只在 ChangePassword 里用，那是低频端点，
	// 但仍可能并发（前端重复提交），故复用同一把mu 取惰性初始化。
	pwThr *server.FailureThrottle
}

// passwordChangeThrottleLimit / Cooldown：改密的失败阈值与冷却。
//
// 5 次 / 5 分钟：PBKDF2 210k 次迭代把单次校验压到几十毫秒，对交互无感，
// 够挡住在线爆破又不至于让「手抖连点」被锁。冷却取5 分钟而非登录的 1 分钟，
// 因为这里的一「次」是已登录用户的主动操作，代价更高。
const (
	passwordChangeThrottleLimit    = 5
	passwordChangeThrottleCooldown = 5 * time.Minute
)

// throttleForPassword 按用户 ID 取改密限速器（惰性构造）。
func (h *UserHandler) throttleForPassword() *server.FailureThrottle {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.pwThr == nil {
		h.pwThr = server.NewFailureThrottle(
			passwordChangeThrottleLimit, passwordChangeThrottleCooldown)
	}
	return h.pwThr
}

func NewUserHandler(st *store.Store, mgr *userauth.Manager) *UserHandler {
	return &UserHandler{store: st, mgr: mgr}
}

// WithReload 注入运行时重建函数（仅为兼容 cmd/gateway 的构造注入面保留；
// 重建统一由外层 server.AutoReload 负责，见 reload 字段的注释）。
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
	// BalanceCents 是我的账户余额（**分**）。unlimited=true 才是不限额；
	// 0 分是「真的一分钱都没有」（会被 402 拒绝）。前端据此显示「不限」或
	// 具体金额，绝不能把不限额显示成「0.00 元」。
	BalanceCents int64 `json:"balance_cents"`
	Unlimited    bool  `json:"balance_unlimited"`
	// BalanceRemainder 是**不足一分**的待结算余数（微元）。余额按分扣减，
	// 而单价可能远低于一分，所以「余额没变」不等于「没消费」—— 前端要把
	// 它显示出来，否则用户会以为没扣钱。语义见 store.User.BalanceRemainder。
	BalanceRemainder int64 `json:"balance_remainder"`
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
	// 安全性：只在「是**引导账号**」时才回不同文案，判定条件与
	// FindUninitializedAdmin 完全一致 —— role='admin' **且** password_hash 为空。
	//
	// 原实现只判 password_hash == ''，于是管理员为别人建的空密码账号
	// 也拿到这条文案。它泄露的不只是「没初始化」，而是「**这个账号存在**」——
	// 文案覆盖了所有空密码账号，而注释声称的范围只有引导admin。
	// 一个可用的枚举 oracle：拿到用户名列表即可确认哪些账号存在且未设密。
	//
	// 收紧后：非 admin 的空密码账号走下面的通用失败路径（与「用户不存在」
	// 和「密码错误」同一条），泄露面回到零。代价是那种账号的本人看到的是
	// 「密码错误」而非引导指引 —— 但那种账号本来就该由管理员重置密码，
	// 不该让本人自助设密（否则绕过管理员）。
	if u != nil && u.PasswordHash == "" && u.Role == store.RoleAdmin {
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
//
// # 窗口的判定依据是一次性标记，不是「当前是否存在空密码 admin」
//
// 引导窗口挂在免鉴权的 publicAdminMux 上（首次部署别无选择），它开放与否
// 必须绑定「本安装是否完成过引导」（app_settings 的 bootstrap_completed 标记，
// 与设密同事务写入，见 store.SetInitialAdminPassword）。若改用「存在空密码
// admin」判定：管理员一旦建出/升级出空密码 admin，任何人都能 POST bootstrap
// 给该账号设上自己的密码并直接拿到 admin 会话 —— 免鉴权窗口**永久**重开。
// 标记只前进一次，窗口因此最多开到第一次设密成功为止。
//
// 兼容性：老库已完成引导但没有标记时，行为不变（无空密码 admin，窗口本来就
// 是关的）；只有 bug 期间恰好留下空密码 admin 的库会再开**最后**一次窗口，
// 设完即永久关闭。

// bootstrapStatusResponse 是首次登录引导的状态。
type bootstrapStatusResponse struct {
	// NeedsSetup 为真表示引导窗口仍然开放（本安装尚未完成过引导，且当前
	// 存在待初始化的管理员），前端应把登录页换成「设置密码」表单。
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
//
// 判定分两层：bootstrap_completed 标记已置 → 直接报「无需引导」，**不再看**
// 是否存在空密码 admin —— 那个条件只是窗口曾经开过的原因（见文件顶部的
// 引导注释），把它当开关会让 bug 期间建出的空密码 admin 把免鉴权窗口永久
// 重开。标记未置（新库或老库升级）才回落到 FindUninitializedAdmin 的旧判定。
func (h *UserHandler) BootstrapStatus(w http.ResponseWriter, r *http.Request) {
	done, err := h.store.BootstrapCompleted(r.Context())
	if err != nil {
		// 查不到状态不该让界面显示「不需要设密码」——那会把用户送去
		// 一个必然失败的登录框。报 500，前端显示「无法连接」。
		writeServerError(w, "bootstrap status: read completed marker", err)
		return
	}
	if done {
		writeJSON(w, http.StatusOK, bootstrapStatusResponse{
			NeedsSetup:     false,
			SessionEnabled: h.mgr.Enabled(),
		})
		return
	}
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
//  1. 一次性标记（bootstrap_completed）先决：本安装完成过引导 → 409，
//     此端点即失效 —— 哪怕库里恰好还存在空密码 admin（bug 期间建出的），
//     也不能经这里被设上密码。标记与设密同事务写入，见 store.SetInitialAdminPassword。
//  2. 只对「role=admin 且 password_hash 为空」的那个账号有效，且条件 UPDATE
//     保证只设一次：引导完成后再调 → 409，不是覆盖。
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

	// 一次性标记先决：本安装完成过引导，窗口就必须关死 —— 不再看「是否存在
	// 空密码 admin」。后者在修复前的用户管理面可能被建出/升级出来，若拿它当
	// 开关，任何人都能给那个账号设上自己的密码并直接拿到 admin 会话。放最前：
	// 窗口已关时连限速与查库都不必做。
	done, err := h.store.BootstrapCompleted(r.Context())
	if err != nil {
		writeServerError(w, "bootstrap setup: read completed marker", err)
		return
	}
	if done {
		// 与「重复初始化」同一条 409 路径：对调用方来说语义相同 —— 管理员
		// 密码已设置，这个免鉴权入口不再接受任何写入。
		writeError(w, http.StatusConflict, "管理员密码已设置，无需重复初始化")
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
	// 写入命中时**同一个事务**还会置 bootstrap_completed 标记：设密成功与
	// 窗口关闭原子生效（见 store.SetInitialAdminPassword 的注释）。
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
// Logout 作废当前会话。
//
// **服务端吊销**（此前只清 cookie，P2-29）：会话是自包含 JWT，Manager 里没有
// 任何服务端状态，所以清 cookie 只让**这一个浏览器**不再发送令牌 —— 已经
// 复制出去的令牌（另一个标签页、另一台机器、或已被 XSS 偷走）仍能用到自然
// 过期（最长 8 小时）。用户点了登出却发现在别处仍能操作，就是这个缺口。
//
// 递增 auth_version 作废该用户的**全部**会话。这是**有界**的吊销：一个
// 数字让全量作废，不必维护逐令牌的吊销表（那要处理表增长、清理，以及
// JWT 无状态带来的存储需求）。Verify 每次都比对当前版本号，版本一变
// 旧令牌立刻全部失效。
//
// 代价：同一账号的所有设备都被踢下线。对管理后台这是期望行为 —— 用户
// 显式点了「登出」，语义就该是「这个身份不再可用」。
func (h *UserHandler) Logout(w http.ResponseWriter, r *http.Request) {
	server.ClearSessionCookie(w)
	if u := server.UserFromContext(r.Context()); u != nil && u.ID != "" {
		if err := h.store.BumpAuthVersion(r.Context(), u.ID); err != nil {
			// 吊销失败必须让用户知道：此时令牌在自然过期前仍可用，
			// 而界面上却显示「已登出」。属降级路径，记 ERROR 但仍回 200 ——
			// cookie 已清，本浏览器确实登出了。
			slog.Error("logout: revoke sessions failed",
				"user_id", u.ID, "error", err)
			writeJSON(w, http.StatusOK, map[string]string{
				"status":  "ok",
				"message": "已登出，但未能作废其他设备上的会话",
			})
			return
		}
	}
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
		// Unlimited 必须**显式**给 true，不能吃 Go 零值：false + 0 分在界面上
		// 渲染成「0.00 元」加一个「已用尽」红标 —— 而这个账号连余额这回事
		// 都没有，那是一次凭空捏造的账目告警。
		writeJSON(w, http.StatusOK, meResponse{
			Username: "(setup)", Role: u.Role, Status: u.Status,
			Unlimited:      true,
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

	// 管理员**不读余额**（2026-10-11）：它不建 key、不调 API，不产生扣费，
	// 也不允许被充值（AdjustBalance 已拦下给自己充值），所以库里那一列对
	// 管理员永远是残留值。统一回「不限额 + 0 分 + 无余数」。
	//
	// 直接短路而不是读了再丢弃，有两个理由：
	//  ① 省掉一次 BalanceOf 查询，而 /me 是每个页面都会打的基础请求；
	//  ② 更重要的是**不留可被误读的数字**：读出来再判断，传给前端的仍是
	//     一个 0/真实值，漏判一次就是一次「我还欠着钱」的假象。
	//
	// 前端据此把管理员的钱包页渲染成「用户消费 + 充值记录」，而不是自己的
	// 余额（见 web/src/views/Wallet.vue）。
	if u.IsAdmin() {
		writeJSON(w, http.StatusOK, meResponse{
			Username:        u.Username,
			DisplayName:     u.DisplayName,
			Role:            u.Role,
			Status:          u.Status,
			QuotaTokens:     u.QuotaTokens,
			UsedTokens:      used,
			BalanceCents:    0,
			Unlimited:       true,
			MustSetPassword: u.PasswordHash == "",
			IsAdmin:         true,
			SessionEnabled:  h.mgr.Enabled(),
		})
		return
	}

	// 余额要**回库读**，不能从上下文里的 u 拿：session.UserFromContext 装的是
	// 鉴权用的那份身份（见 server 包），它不带余额字段。读库也正是余额语义的
	// 唯一权威来源 —— 预检与扣费都走 store.BalanceOf，界面若显示另一份数字，
	// 就会出现「界面还有钱、请求却被 402」。
	//
	// 查不到时**降级成不限额**而不是 0：这与 quota 的「0 = 不限」恰好相反，
	// 但降级方向是对的 —— 显示「0 元」会让用户以为自己没钱，而查库失败
	// 并不代表账户真的空了。fail-open 只发生在展示层，真实拦截仍由
	// 扣费路径上的数据库读决定，不受这里的降级影响。
	//
	// 注意 BalanceOf 在读失败时返回 (0, limited=true, err)：即「有限额且为 0」。
	// 所以降级必须以 err != nil 为判据，不能直接用 limited —— 那样读失败会
	// 被渲染成「0 元」，正是这里要避开的那个误读。
	balance, limited, berr := h.store.BalanceOf(r.Context(), u.ID)
	unlimited := true
	remainder := int64(0)
	if berr != nil {
		slog.Warn("me: balance lookup failed; degrading balance to unlimited", "error", berr)
	} else {
		unlimited = !limited
		// 余数只对有限额用户有意义；不限额用户本来就不攒（见 ChargeBalance）。
		if !unlimited {
			remainder = u.BalanceRemainder
		}
	}

	writeJSON(w, http.StatusOK, meResponse{
		Username:         u.Username,
		DisplayName:      u.DisplayName,
		Role:             u.Role,
		Status:           u.Status,
		QuotaTokens:      u.QuotaTokens,
		UsedTokens:       used,
		BalanceCents:     balance,
		Unlimited:        unlimited,
		BalanceRemainder: remainder,
		MustSetPassword:  u.PasswordHash == "",
		IsAdmin:          u.IsAdmin(),
		SessionEnabled:   h.mgr.Enabled(),
	})
}
