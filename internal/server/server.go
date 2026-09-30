package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
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
			"status", wrapped.StatusCode(),
			"duration_ms", time.Since(start).Milliseconds(),
			"request_id", reqID,
			"remote", r.RemoteAddr,
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
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Api-Key")

			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}
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
	return AdminAuthThrottled(next, creds, NewFailureThrottle(loginFailLimit, loginCooldown))
}

// AdminAuthThrottled 是 AdminAuth 的可注入限速版本（测试与需要调参的部署用）。
func AdminAuthThrottled(next http.Handler, creds AdminCredentials, throttle *FailureThrottle) http.Handler {
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

		ip := clientIP(r)
		if !throttle.Allow(ip) {
			writeTooManyAttempts(w, throttle.RetryAfter(ip))
			return
		}

		if creds.Verify(bearerToken(r)) {
			throttle.Success(ip)
			next.ServeHTTP(w, r)
			return
		}

		throttle.Fail(ip)
		writeAuthError(w)
	})
}

// clientIP 取请求来源地址。
//
// 刻意不看 X-Forwarded-For：那是客户端可以随意伪造的头，采信它等于把限速交给攻击者
// 开关。代价是本网关若部署在反向代理之后，计数会退化成「按代理 IP 计」——
// 挡不住分布式爆破，但至少不会被伪造头绕过。
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

const (
	// loginFailLimit 是同一来源 IP 连续鉴权失败多少次后进入冷却。
	// 密码正确的用户一次成功即清零，正常使用不可能触及。
	loginFailLimit = 10
	// loginCooldown 是冷却时长。冷却期内连 KDF 都不做 ——
	// 顺带把爆破者本可以逼我们消耗的 CPU 也省掉。
	loginCooldown = time.Minute
)

// FailureThrottle 按来源 IP 记连续鉴权失败，给管理登录限速。
//
// 为什么需要它：管理密码下限只有 6 位，PBKDF2 21 万迭代把单次尝试压到几十毫秒
// （对交互无感），但并发下 6 位弱口令依然可爆破 —— 而且失败路径此前不留任何痕迹，
// 日志里看不出有人在猜密码。
type FailureThrottle struct {
	mu       sync.Mutex
	limit    int
	cooldown time.Duration
	fails    map[string]*failEntry
}

type failEntry struct {
	count int
	until time.Time
}

func NewFailureThrottle(limit int, cooldown time.Duration) *FailureThrottle {
	if limit <= 0 {
		limit = loginFailLimit
	}
	if cooldown <= 0 {
		cooldown = loginCooldown
	}
	return &FailureThrottle{limit: limit, cooldown: cooldown, fails: make(map[string]*failEntry)}
}

// Allow 报告该 IP 当前是否允许尝试。
func (t *FailureThrottle) Allow(ip string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	e, ok := t.fails[ip]
	if !ok {
		return true
	}
	return !time.Now().Before(e.until)
}

// RetryAfter 返回冷却剩余时长；未处于冷却期为 0。
func (t *FailureThrottle) RetryAfter(ip string) time.Duration {
	t.mu.Lock()
	defer t.mu.Unlock()
	e, ok := t.fails[ip]
	if !ok {
		return 0
	}
	if d := time.Until(e.until); d > 0 {
		return d
	}
	return 0
}

// Fail 记一次失败；达到阈值即进入冷却并把计数清零。
//
// 「是否处于冷却期」必须用 `until` 是否为零值来判定，不能直接写
// `!time.Now().Before(e.until)` —— 未进过冷却的条目 `until` 是零值，
// 而任何时间点都「不在零值之前」，于是那个条件对新条目恒为真：
// 每失败一次就新建条目、count 被清回 1，计数**永远涨不到阈值**，
// 整个限速静默失效（2026-09-24 实测：连打 26 次错误密码全是 401，无一 429）。
func (t *FailureThrottle) Fail(ip string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.sweepLocked()

	e, ok := t.fails[ip]
	if !ok {
		e = &failEntry{}
		t.fails[ip] = e
	} else if !e.until.IsZero() && !time.Now().Before(e.until) {
		// 上一轮冷却已结束：从零重新计数。
		// until 必须清回零值，否则下一次 Fail 又会命中这个分支把 count 清零。
		e.count = 0
		e.until = time.Time{}
	}

	e.count++
	if e.count >= t.limit {
		e.count = 0
		e.until = time.Now().Add(t.cooldown)
	}
}

// Success 清空该 IP 的失败记录。
func (t *FailureThrottle) Success(ip string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.fails, ip)
}

// sweepLocked 防止 map 被海量一次性 IP 撑爆：条目数超阈值时清掉所有不在冷却期的记录。
// 代价是攻击者换个 IP 就能重置计数 —— 可接受，这个限速的目标是挡住单个来源的
// 暴力枚举，不是分布式防护。
func (t *FailureThrottle) sweepLocked() {
	const maxEntries = 4096
	if len(t.fails) < maxEntries {
		return
	}
	now := time.Now()
	for ip, e := range t.fails {
		if !now.Before(e.until) {
			delete(t.fails, ip)
		}
	}
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
			h.Set("Content-Security-Policy",
				"default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; base-uri 'self'; form-action 'self'; frame-ancestors 'none'")
			// 禁止被嵌入 iframe（点击劫持防护）。
			h.Set("X-Frame-Options", "DENY")
			// 禁止 MIME 嗅探。
			h.Set("X-Content-Type-Options", "nosniff")
			// 现代浏览器的 XSS 过滤器（已废弃但无害，兼容旧浏览器）。
			h.Set("X-XSS-Protection", "1; mode=block")
			// 引用策略：不向任何跨源请求泄露 Referer。
			h.Set("Referrer-Policy", "no-referrer")
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
