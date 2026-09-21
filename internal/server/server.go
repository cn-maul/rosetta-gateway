package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

type contextKey string

const requestIDKey contextKey = "request_id"

func RequestIDFromContext(ctx context.Context) string {
	if id, ok := ctx.Value(requestIDKey).(string); ok {
		return id
	}
	return ""
}

func generateRequestID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func Middleware(next http.Handler, logger *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		reqID := generateRequestID()
		ctx := context.WithValue(r.Context(), requestIDKey, reqID)
		r = r.WithContext(ctx)

		w.Header().Set("X-Request-Id", reqID)

		wrapped := &statusResponseWriter{ResponseWriter: w, statusCode: http.StatusOK}
		next.ServeHTTP(wrapped, r)

		logger.Info("request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", wrapped.statusCode,
			"duration_ms", time.Since(start).Milliseconds(),
			"request_id", reqID,
			"remote", r.RemoteAddr,
		)
	})
}

type statusResponseWriter struct {
	http.ResponseWriter
	statusCode int
	written    bool
}

func (w *statusResponseWriter) WriteHeader(code int) {
	if !w.written {
		w.statusCode = code
		w.written = true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusResponseWriter) Write(b []byte) (int, error) {
	if !w.written {
		w.written = true
	}
	return w.ResponseWriter.Write(b)
}

// Flush 必须显式转发给底层 ResponseWriter：中间件把 w 包进 statusResponseWriter 后，
// 被提升的只有 Write/WriteHeader/Header，并不包含 http.Flusher。缺了这个方法，
// 下游 handler 的 `w.(http.Flusher)` 断言会得到 nil，SSE 每块 Flush 变成空操作，
// 数据全压在 net/http 的 bufio 缓冲里直到写满或 handler 返回 —— 流式吞吐骤降、首字延迟暴涨。
func (w *statusResponseWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap 让更外层的中间件/handler 能穿透本包装拿到原始 ResponseWriter（Go 1.20+ 约定）。
func (w *statusResponseWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

func RequestSizeLimit(maxBytes int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
			next.ServeHTTP(w, r)
		})
	}
}

func CORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PATCH, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Api-Key")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// AdminCredentials 是管理后台凭据的运行时视图，由 internal/adminauth.Store 实现。
//
// 这里声明接口而不是直接依赖具体类型，是为了让 server 包保持纯粹：
// 它只负责「HTTP 中间件该不该放行」，不关心凭据存在哪、怎么哈希。
type AdminCredentials interface {
	// Verify 报告一个明文令牌是否有效。
	Verify(token string) bool
	// HasCredential 报告是否已配置任何凭据（用户密码或 config 的 admin_token）。
	HasCredential() bool
}

// AdminAuth 保护管理后台的 API。
//
// 放行规则：
//   - GET  /admin/api/password/check —— 恒放行。前端靠它决定弹「设置密码」还是
//     「输入密码」，响应里只有布尔值，不含敏感信息。
//   - POST /admin/api/password/set   —— 仅在「尚未配置任何凭据」时放行。
//     那是一次性引导窗口：还没有密码可被绕过，且不放行的话全新部署根本
//     无法设置密码。一旦存在凭据，改密码必须带上旧凭据 ——
//     否则局域网内任何人都能把管理员锁在门外。
//   - 其余一律要求 Authorization: Bearer <密码或 admin_token>。
//
// 注意这里**没有**「admin_token 为空就一律放行」的分支：凭据为空时除上述两个
// 引导端点外全部拒绝，前端因此会停在「设置密码」对话框，形成一条明确的
// 初始化路径。旧实现在凭据为空时无条件 401（和 config.go 里
// 「admin_token 留空 = 后台免鉴权」的注释正好相反），导致首次部署只能改配置文件。
func AdminAuth(next http.Handler, creds AdminCredentials) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path

		if path == "/admin/api/password/check" {
			next.ServeHTTP(w, r)
			return
		}
		if path == "/admin/api/password/set" && !creds.HasCredential() {
			next.ServeHTTP(w, r)
			return
		}

		if creds.Verify(bearerToken(r)) {
			next.ServeHTTP(w, r)
			return
		}

		writeAuthError(w)
	})
}

// bearerToken 取出 Authorization 头里的 Bearer 令牌（大小写不敏感）。
func bearerToken(r *http.Request) string {
	const prefix = "Bearer "
	auth := r.Header.Get("Authorization")
	if len(auth) > len(prefix) && strings.EqualFold(auth[:len(prefix)], prefix) {
		return auth[len(prefix):]
	}
	return ""
}

func writeAuthError(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	// 与内部 admin 包的 writeError 保持同样的错误信封，前端只需一套解析逻辑。
	w.Write([]byte(`{"error":{"message":"需要管理员密码","type":"authentication_error"}}`))
}

func Recovery(next http.Handler, logger *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				logger.Error("panic recovered",
					"error", fmt.Sprintf("%v", rec),
					"path", r.URL.Path,
					"request_id", RequestIDFromContext(r.Context()),
				)
				http.Error(w, `{"error":{"message":"internal error","type":"internal_error"}}`, http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}
