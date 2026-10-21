// Package identity 管理管理面请求里的用户身份与会话凭据。
//
// 它不依赖 internal/server：登录处理器、鉴权中间件和管理 handler 都从这里
// 读取同一个 context key、写同一种 Cookie，避免 admin 反向依赖 HTTP 装配层。
package identity

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/cn-maul/rosetta-gateway/internal/store"
	"github.com/cn-maul/rosetta-gateway/internal/userauth"
)

type ctxKey string

const (
	sessionKey ctxKey = "admin_session"
	// userKey 存 *store.User，供 handler 少查一次库。
	userKey ctxKey = "admin_user"
)

// SessionCookieName 是会话 cookie 名。
const SessionCookieName = "rosetta_session"

// SessionStore 是会话鉴权需要的最小存储能力。
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

// UserFromContext 返回当前请求的用户；未登录时返回 nil。
func UserFromContext(ctx context.Context) *store.User {
	v, _ := ctx.Value(userKey).(*store.User)
	return v
}

// IsAdmin 报告当前请求是否带管理员身份。
func IsAdmin(ctx context.Context) bool {
	u := UserFromContext(ctx)
	return u != nil && u.IsAdmin()
}

// WithSession 把会话与用户同时注入 context。
func WithSession(ctx context.Context, sess *userauth.Session, u *store.User) context.Context {
	ctx = context.WithValue(ctx, sessionKey, sess)
	return context.WithValue(ctx, userKey, u)
}

// WithUser 只注入用户身份，主要用于 handler 测试。
func WithUser(ctx context.Context, u *store.User) context.Context {
	return context.WithValue(ctx, userKey, u)
}

// UserCtxKey 返回用户 context key，供必须手工构造 context 的测试使用。
func UserCtxKey() any { return userKey }

// SetSessionCookie 写出会话 cookie。
//
// secure 由调用方从 httpx.SessionCookieSecure(r) 推导：配置了 https 的
// public_origin 时浏览器实际走 HTTPS，Cookie 必须带 Secure；纯 HTTP
// 局域网部署则不能带，否则浏览器会直接丢弃它。
func SetSessionCookie(w http.ResponseWriter, token string, exp time.Time, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    token,
		Path:     "/admin",
		Expires:  exp,
		MaxAge:   int(time.Until(exp).Seconds()),
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteStrictMode,
	})
}

// ClearSessionCookie 清除会话 cookie。路径必须与写入时一致。
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
