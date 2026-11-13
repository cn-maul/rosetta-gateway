package server

import (
	"errors"
	"net/http"

	"github.com/cn-maul/rosetta-gateway/internal/access"
	"github.com/cn-maul/rosetta-gateway/internal/httpx"
	"github.com/cn-maul/rosetta-gateway/internal/identity"
	"github.com/cn-maul/rosetta-gateway/internal/userauth"
)

type SessionStore = identity.SessionStore

// UserAuthMiddleware 保护管理后台 API：只有用户会话一条通道。
type UserAuthMiddleware struct {
	mgr      *userauth.Manager
	store    SessionStore
	throttle *FailureThrottle
}

func NewUserAuth(mgr *userauth.Manager, st SessionStore) *UserAuthMiddleware {
	return &UserAuthMiddleware{
		mgr: mgr, store: st,
		// 会话鉴权不跑 KDF，并发上限只会误伤正常页面（2026-10-10 修复）。
		throttle: NewFailureThrottle(LoginFailLimit, LoginCooldown).WithoutConcurrencyCap(),
	}
}

func (a *UserAuthMiddleware) Guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.ServeHTTP(w, r, next)
	})
}

// AdminGateGuard 在会话鉴权之后做「非白名单即要求管理员」的判定。
// 具体白名单与判定已移到 internal/access。
func AdminGateGuard(routes *http.ServeMux, next http.Handler) http.Handler {
	return access.AdminGateGuard(routes, next)
}

func (a *UserAuthMiddleware) ServeHTTP(w http.ResponseWriter, r *http.Request, next http.Handler) {
	if !a.mgr.Enabled() {
		httpx.WriteAuthError(w, http.StatusServiceUnavailable, "管理后台未就绪：会话密钥不可用")
		return
	}

	ip := httpx.ClientIPOf(r)
	if !a.throttle.Allow(ip) {
		writeTooManyAttempts(w, a.throttle.RetryAfter(ip))
		return
	}
	defer a.throttle.Release(ip)

	token := sessionToken(r)
	if token == "" {
		// 匿名请求不计失败：它不消耗验证资源，计数只会制造零成本 DoS。
		httpx.WriteAuthError(w, http.StatusUnauthorized, "需要登录")
		return
	}

	probe, err := a.mgr.Peek(token)
	if err != nil || probe == nil {
		a.throttle.Fail(ip)
		httpx.WriteAuthError(w, http.StatusUnauthorized, sessionErrMessage(err))
		return
	}

	u, err := a.store.GetUser(r.Context(), probe.UserID)
	if err != nil {
		httpx.WriteAuthError(w, http.StatusInternalServerError, "服务内部错误")
		return
	}
	if u == nil {
		a.throttle.Fail(ip)
		httpx.WriteAuthError(w, http.StatusUnauthorized, "账号不存在")
		return
	}
	if !u.IsActive() {
		a.throttle.Fail(ip)
		httpx.WriteAuthError(w, http.StatusForbidden, "账号已被禁用")
		return
	}

	// 只做校验：会话内容（角色/用户名/到期时间）已由 *store.User 与
	// token 自身承载，请求处理链上没有第二个读者。
	if err := a.mgr.Verify(token, u.AuthVersion); err != nil {
		a.throttle.Fail(ip)
		httpx.WriteAuthError(w, http.StatusUnauthorized, sessionErrMessage(err))
		return
	}

	a.throttle.Success(ip)

	// 同源校验只拦「凭 Cookie 进来的跨站写请求」：Bearer 不受浏览器同源
	// 策略约束，跨源读又会被 CORS 挡住。
	if !safeMethods[r.Method] && cookieAuthed(r) && !httpx.SameOrigin(r) {
		httpx.WriteForbidden(w)
		return
	}

	// AutoReload sits outside this middleware and records the audit entry after
	// the handler returns. Persist the authenticated actor in that request's
	// slot; the derived context below is not visible to the outer middleware.
	recordAuditActor(r.Context(), u.Username)

	ctx := identity.WithUser(r.Context(), u)
	next.ServeHTTP(w, r.WithContext(ctx))
}

var safeMethods = map[string]bool{
	http.MethodGet:     true,
	http.MethodHead:    true,
	http.MethodOptions: true,
}

func cookieAuthed(r *http.Request) bool {
	c, err := r.Cookie(identity.SessionCookieName)
	return err == nil && c.Value != ""
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

func sessionToken(r *http.Request) string {
	if c, err := r.Cookie(identity.SessionCookieName); err == nil && c.Value != "" {
		return c.Value
	}
	return bearerToken(r)
}
