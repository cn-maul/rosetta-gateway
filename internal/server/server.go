package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cn-maul/rosetta-gateway/internal/guard"
	"github.com/cn-maul/rosetta-gateway/internal/httpx"
)

// FailureThrottle 与登录限速参数的实现已移到 internal/guard。
// server 保留这些薄别名，供仍在 HTTP 中间件层的调用方与测试使用。
type FailureThrottle = guard.FailureThrottle

const (
	LoginFailLimit = guard.LoginFailLimit
	LoginCooldown  = guard.LoginCooldown
)

func NewFailureThrottle(limit int, cooldown time.Duration) *FailureThrottle {
	return guard.NewFailureThrottle(limit, cooldown)
}

type contextKey string

const publicOriginKey contextKey = "public_origin"
const trustedProxiesKey contextKey = "trusted_proxies"
const resolvedClientIPKey contextKey = "resolved_client_ip"

func RequestIDFromContext(ctx context.Context) string {
	return httpx.RequestIDFromContext(ctx)
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
		ctx := httpx.WithRequestID(r.Context(), reqID)
		r = r.WithContext(ctx)

		w.Header().Set("X-Request-Id", reqID)

		wrapped := &statusResponseWriter{ResponseWriter: w, statusCode: http.StatusOK}
		next.ServeHTTP(wrapped, r)

		logger.Info("request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", wrapped.StatusCode(),
			"duration_ms", time.Since(start).Milliseconds(),
			"request_id", reqID,
			"remote", httpx.ClientIPOf(r),
		)
	})
}

type statusResponseWriter struct {
	http.ResponseWriter
	// mu 保护 statusCode/written。流式路径上心跳 goroutine 与主事件循环都会
	// 走到 Write，两个字段是共享可变状态。
	mu         sync.Mutex
	statusCode int
	written    bool
}

func (w *statusResponseWriter) WriteHeader(code int) {
	w.mu.Lock()
	if !w.written {
		w.statusCode = code
		w.written = true
	}
	w.mu.Unlock()
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusResponseWriter) Write(b []byte) (int, error) {
	w.mu.Lock()
	if !w.written {
		w.written = true
	}
	w.mu.Unlock()
	return w.ResponseWriter.Write(b)
}

// Committed 报告响应是否已开始下发（状态码或 body 写过至少一次）。
func (w *statusResponseWriter) Committed() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.written
}

// StatusCode 返回已下发的状态码（默认 200）。
func (w *statusResponseWriter) StatusCode() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.statusCode
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
		// /admin 管理面由同源 SPA 访问，不需要跨域放行；对管理面开
		// Access-Control-Allow-Origin: * 等于把已登录管理员的浏览器暴露给任意
		// 恶意站点（配合 Authorization 头可被 CSRF 式利用）。管理面一律不设
		// CORS 头，浏览器跨源请求自然被同源策略拦下。
		// /v1 数据面是给任意客户端跨域调用的，保持 * 放行。
		if !strings.HasPrefix(r.URL.Path, "/admin") {
			w.Header().Set("Access-Control-Allow-Origin", "*")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PATCH, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Api-Key, X-OpenCode-Session")

			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}
		}

		next.ServeHTTP(w, r)
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

// writeTooManyAttempts 在限速冷却期内拒绝请求。
func writeTooManyAttempts(w http.ResponseWriter, retryAfter time.Duration) {
	seconds := int(retryAfter.Seconds()) + 1
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Retry-After", strconv.Itoa(seconds))
	w.WriteHeader(http.StatusTooManyRequests)
	//nolint:errcheck // 响应写入失败已无补救手段
	w.Write([]byte(`{"error":{"message":"密码尝试过于频繁，请稍后再试","type":"rate_limit_error"}}`))
}

// SecurityHeaders 为管理面响应附加安全头，缓解点击劫持 / MIME 嗅探 / XSS。
// 只作用于 /admin 路径；/v1 数据面是纯 JSON API，不适用这些页面级头。
func SecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/admin") {
			h := w.Header()
			// 管理面板是自包含 SPA，无第三方脚本/样式，CSP 收紧到 self。
			//
			// 注意 script-src **不含** 'unsafe-inline'：主题防闪脚本曾经以
			// 内联 IIFE 写在 index.html 的 <head> 里，被这条 CSP 静默拦掉 ——
			// 功能直接坏掉（暗色用户首帧白闪回归），而浏览器只在控制台报一条
			// 违规，测试与构建全绿，看不出任何异常。该脚本已外置为
			// /admin/assets/theme-init.js（见 web/index.html）。
			h.Set("Content-Security-Policy",
				"default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; "+
					"img-src 'self' data:; connect-src 'self'; base-uri 'self'; "+
					// object-src 'none'：禁用 <object>/<embed>。管理页没有这类元素，
					// 而缺了它时 default-src 会放行插件内容 —— 一次被注入的
					// <object data="..."> 就能执行远端代码，绕开 script-src 'self'
					// 的全部限制。零成本，无副作用。
					"object-src 'none'; "+
					"form-action 'self'; frame-ancestors 'none'")
			// 禁止被嵌入 iframe（点击劫持防护）。
			h.Set("X-Frame-Options", "DENY")
			// 禁止 MIME 嗅探。
			h.Set("X-Content-Type-Options", "nosniff")
			// 现代浏览器的 XSS 过滤器（已废弃但无害，兼容旧浏览器）。
			h.Set("X-XSS-Protection", "1; mode=block")
			// 引用策略：不向任何跨源请求泄露 Referer。
			h.Set("Referrer-Policy", "no-referrer")
			// 管理面响应一律不缓存：凭据状态、路由配置、用量数据都是敏感且
			// 强时效的，浏览器 back-forward cache 或中间代理命中一次旧响应
			// 就会让「刚被禁用的 key 仍显示可用」这类误判持续存在。
			h.Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
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
				writeInternalError(w)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// writeInternalError 写一个 500 响应。
//
// 不能用 http.Error：它按纯文本处理 body 并补一个换行，Content-Type 变成
// text/plain —— 与错误信封的 JSON 自相矛盾，前端只能拿到一段解析不了的字符串。
//
// 更要紧的是**响应头可能已经发出**（流式路径在写 SSE 头之后 panic 正是这种情形，
// 也是本轮审计实测到的那条）。此时再 WriteHeader 只会被 net/http 记一条
// "superfluous response.WriteHeader call"，而这段 JSON 会被当成 SSE 数据
// 直接追加进流里 —— 客户端看到一坨没有 `data:` 前缀的字节。
// 所以先判断提交状态：已提交就什么都不写，让下游按流中断处理。
func writeInternalError(w http.ResponseWriter) {
	if ct, ok := w.(interface{ Committed() bool }); ok && ct.Committed() {
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusInternalServerError)
	_, _ = w.Write([]byte("{\"error\":{\"message\":\"internal error\",\"type\":\"internal_error\"}}\n"))
}
