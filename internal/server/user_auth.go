package server

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/cn-maul/rosetta-gateway/internal/store"
	"github.com/cn-maul/rosetta-gateway/internal/userauth"
)

type ctxKey string

const (
	sessionKey ctxKey = "admin_session"
	// userKey 存*store.User，供handler 少查一次库。
	userKey ctxKey = "admin_user"
)

// SessionCookieName 是会话 cookie 名。
//
// 刻意**不用** __Host- 前缀：那要求 Secure 属性，而本项目跑在局域网 HTTP 上，
// 浏览器不会接受没有 Secure 的 __Host- cookie。改用普通名字 + SameSite=Strict。
const SessionCookieName = "rosetta_session"

// SessionTTL 与 userauth.Manager 的令牌有效期一致。
// cookie 的 MaxAge 必须与 JWT exp 对齐，否则会出现「cookie 还在但令牌已过期」
// 的窗口，前端表现为「明明有登录态却一直 401」。
const SessionTTL = 8 * time.Hour

// SessionStore 是会话中间件依赖的最小数据面（便于测试替身）。
type SessionStore interface {
	GetUser(ctx context.Context, id string) (*store.User, error)
	CountUsers(ctx context.Context) (int, error)
	TouchUserLogin(ctx context.Context, id string) error
}

// SessionFromContext 返回当前请求的会话；未登录时返回 nil。
func SessionFromContext(ctx context.Context) *userauth.Session {
	v, _ := ctx.Value(sessionKey).(*userauth.Session)
	return v
}

// UserCtxKey 是注入用户身份的 context key。
//
// 导出给测试包用：admin 包的测试需要构造「已登录」请求，
// 而 userKey 是私有的。
func UserCtxKey() any { return userKey }

// UserFromContext 返回当前请求的用户；未登录时返回 nil。
func UserFromContext(ctx context.Context) *store.User {
	v, _ := ctx.Value(userKey).(*store.User)
	return v
}

// IsAdmin 报告当前请求是否具备管理面权限。
func IsAdmin(ctx context.Context) bool {
	u := UserFromContext(ctx)
	return u != nil && u.IsAdmin()
}

// UserAuthMiddleware 保护管理后台 API：**只有用户会话一条通道**
// （users 表里的身份 + JWT 会话）。
//
// # 2026-10-06：移除了并行的「运维凭据」通道
//
// 此前有第二条通道（config 的 admin_token 或 admin_auth.json 的密码），
// 它的存在理由是「建第一个账号」与「忘记密码时应急」。统一认证后这两件事
// 都有了更合适的位置：
//
//   - 首次：GET/POST /admin/api/bootstrap（免鉴权，仅对「已建出但未设密码」
//     的那个 admin 有效，设完即失效）
//   - 忘记密码：管理员在「用户管理」里重置该用户密码（递增 auth_version，
//     旧会话一次性作废）
//
// 留着旁路通道的代价是实打实的：它绕过 users 表，因此不产生会话、
// 不受 auth_version 约束、也不留下任何可归属到具体用户的记录。
// 那是一条「谁改的」永远查不到的管理入口。
type UserAuthMiddleware struct {
	mgr      *userauth.Manager
	store    SessionStore
	throttle *FailureThrottle
}

func NewUserAuth(mgr *userauth.Manager, st SessionStore) *UserAuthMiddleware {
	return &UserAuthMiddleware{
		mgr: mgr, store: st,
		throttle: NewFailureThrottle(loginFailLimit, loginCooldown),
	}
}

// WithThrottle 注入自定义限速器（测试用；线上走 NewUserAuth 的默认值）。
func (a *UserAuthMiddleware) WithThrottle(th *FailureThrottle) *UserAuthMiddleware {
	a.throttle = th
	return a
}

// UserAuth 构造会话中间件并套在 next 前面。
//
// 签名是构造函数而非「中间件函数」是为了能作为一个整体挂到 mux 上。
func UserAuth(next http.Handler, mgr *userauth.Manager, st SessionStore) http.Handler {
	return NewUserAuth(mgr, st).Guard(next)
}

// Guard 把鉴权套在 next 前面。
func (a *UserAuthMiddleware) Guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.ServeHTTP(w, r, next)
	})
}

// userAccessiblePrefixes 是**普通用户**可访问的管理端点前缀（白名单）。
//
// # 为什么用白名单而不是逐个标 admin-only
//
// 黑名单（逐个给 admin 端点套 requireAdmin）漏标一条 = 越权，
// 而且是静默的：接口照常 200，没人发现。51 个路由靠人手标，
// 漏标的概率不低 —— 实测第一版就漏了 stats / settings / providers /
// routes 四个，普通用户全部能读。
//
// 白名单反过来：漏标一条 = 普通用户访问自己的功能被 403，
// 立刻会被发现并修掉。**把「静默越权」换成「显式不可用」**，
// 这是安全默认值该有的方向。
//
// 每条白名单端点内部都必须自己做作用域收窄（见
// internal/admin/key_handler.go 与 usage_handler.go 的 callerScope），
// 否则白名单本身就成了泄露入口。
var userAccessiblePrefixes = []string{
	"/admin/api/keys",  // 作用域收窄：只看自己的
	"/admin/api/usage", // 含 by-* 与 history，均按 user_id 收窄
	"/admin/api/stats", // 同上；费用对普通用户归零（见 stats_handler）
	"/admin/api/me",
	"/admin/api/logout",
	// 模型名清单：key 级白名单要能「用户自助收紧」，就得让普通用户读到
	// 可选模型。返回内容已按身份收窄（普通用户只拿到自己组内的），
	// 且只是一串公开模型名 —— 不含上游、凭据、路由拓扑。
	"/admin/api/model-names",
}

// AdminGateGuard 在会话鉴权之后再做一次「非白名单即要求管理员」的判定。
//
// 必须套在 UserAuth 之后 —— 它依赖 context 里的用户身份。
func AdminGateGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		for _, pre := range userAccessiblePrefixes {
			if strings.HasPrefix(p, pre) {
				next.ServeHTTP(w, r)
				return
			}
		}
		u := UserFromContext(r.Context())
		if u == nil || !u.IsAdmin() {
			writeAdminErr(w, http.StatusForbidden, "需要管理员权限")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ServeHTTP 实现标准中间件签名 (next 可继续传递请求)。
//
// 只有一条路径：解析会话令牌 → 查库比对 auth_version → 注入身份。
// 没有任何旁路。免鉴权的端点（登录、首次引导）挂在 publicAdminMux 上，
// 根本不经过这个中间件。
func (a *UserAuthMiddleware) ServeHTTP(w http.ResponseWriter, r *http.Request, next http.Handler) {
	// 会话密钥不可用属于部署故障。main.go 在启动时就会因取不到密钥而退出，
	// 走到这里只可能是测试构造。回 503 而不是 401：前者说「服务端没准备好」，
	// 后者说「你的凭据不对」—— 让用户去检查自己的 cookie 是白费功夫。
	if !a.mgr.Enabled() {
		writeAdminErr(w, http.StatusServiceUnavailable, "管理后台未就绪：会话密钥不可用")
		return
	}

	ip := clientIP(r)
	if !a.throttle.Allow(ip) {
		writeTooManyAttempts(w, a.throttle.RetryAfter(ip))
		return
	}
	defer a.throttle.Release(ip)

	token := sessionToken(r)
	if token == "" {
		a.throttle.Fail(ip)
		writeAdminErr(w, http.StatusUnauthorized, "需要登录")
		return
	}

	// 先解析令牌拿到 user id，再查库比 auth_version。
	// 顺序不能反：auth_version 在库里，不解析令牌无从得知该查谁。
	probe, err := a.mgr.Peek(token)
	if err != nil || probe == nil {
		a.throttle.Fail(ip)
		writeAdminErr(w, http.StatusUnauthorized, sessionErrMessage(err))
		return
	}

	u, err := a.store.GetUser(r.Context(), probe.UserID)
	if err != nil {
		// 数据库故障不该回 401（那是「你的凭据不对」），回 500。
		writeAdminErr(w, http.StatusInternalServerError, "服务内部错误")
		return
	}
	if u == nil {
		a.throttle.Fail(ip)
		writeAdminErr(w, http.StatusUnauthorized, "账号不存在")
		return
	}
	if !u.IsActive() {
		a.throttle.Fail(ip)
		writeAdminErr(w, http.StatusForbidden, "账号已被禁用")
		return
	}

	sess, err := a.mgr.Verify(token, u.AuthVersion)
	if err != nil {
		a.throttle.Fail(ip)
		writeAdminErr(w, http.StatusUnauthorized, sessionErrMessage(err))
		return
	}

	a.throttle.Success(ip)

	ctx := context.WithValue(r.Context(), sessionKey, sess)
	ctx = context.WithValue(ctx, userKey, u)
	next.ServeHTTP(w, r.WithContext(ctx))
}

func sessionErrMessage(err error) string {
	switch {
	case errors.Is(err, userauth.ErrTokenExpired):
		return "登录已过期，请重新登录"
	case errors.Is(err, userauth.ErrStaleVersion):
		return "凭据已失效（密码或账号状态有变更），请重新登录"
	default:
		return "需要登录"
	}
}

// sessionToken 从请求里取令牌：Cookie 优先，其次 Authorization 头。
//
// 支持 Authorization 是为了让 curl / SDK 能直接用 Bearer 调管理 API
// （脚本化运维、冒烟测试都需要）。浏览器走 Cookie。
func sessionToken(r *http.Request) string {
	if c, err := r.Cookie(SessionCookieName); err == nil && c.Value != "" {
		return c.Value
	}
	return bearerToken(r)
}

// SetSessionCookie 写出会话 cookie。
func SetSessionCookie(w http.ResponseWriter, token string, exp time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    token,
		Path:     "/admin",
		Expires:  exp,
		MaxAge:   int(time.Until(exp).Seconds()),
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		// 刻意不设 Secure：局域网是 HTTP，设了浏览器直接丢弃 cookie。
		// 这是「不上 HTTPS」的已知代价，§6.1 已记录。
	})
}

// ClearSessionCookie 清除会话 cookie。
//
// 路径与写入时一致，否则浏览器会当成另一个 cookie 留着不清。
func ClearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    "",
		Path:     "/admin",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	})
}

// TouchLogin 更新最后登录时间。失败只记日志 —— 它是辅助信息，
// 不该让登录失败。
func TouchLogin(st SessionStore, ctx context.Context, userID string) {
	if userID == "" {
		return
	}
	if err := st.TouchUserLogin(ctx, userID); err != nil {
		slog.Warn("admin api: touch login failed", "user_id", userID, "error", err)
	}
}

func writeAdminErr(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]string{"message": message, "type": "auth_error"},
	})
}
