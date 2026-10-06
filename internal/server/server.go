package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
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

// sameOrigin 报告请求是否来自同源。
//
// 判据优先级：
//  1. Sec-Fetch-Site —— 浏览器强制发送，页面 JS **无法**伪造（属于
//     forbidden header name）。跨站导航/表单/fetch 一律是 cross-site。
//  2. Origin —— 浏览器对 CORS 相关请求必发。同样不可伪造。取其 scheme+host
//     与请求的 Host 比对；反向代理后两者可能不等，此时保守判为跨源。
//  3. 都没有 —— 非浏览器客户端（curl、SDK、服务间调用）。它不受浏览器
//     同源策略约束，因此不是 CSRF 的攻击面，放行。
//
// SameOrigin 导出给 admin 包：首次登录引导的设密码端点是免鉴权写接口，
// 必须与中间件用同一套同源判据。两处各写一份，迟早会只改一处 —— 而漏改
// 的那处就是一个免鉴权的跨站写入口。
func SameOrigin(r *http.Request) bool { return sameOrigin(r) }

func sameOrigin(r *http.Request) bool {
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" {
		return site == "same-origin" || site == "none"
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	return strings.EqualFold(u.Host, r.Host)
}

// WriteForbidden 导出给 admin 包的引导端点复用：免鉴权写接口被跨站调用时，
// 必须与中间件用同一句拒绝文案，否则界面上的报错会因入口不同而不同。
func WriteForbidden(w http.ResponseWriter) { writeForbidden(w) }

func writeForbidden(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	//nolint:errcheck // 响应写入失败已无补救手段
	w.Write([]byte(`{"error":{"message":"拒绝跨站请求","type":"invalid_request_error"}}`))
}

// ClientIPOf 取请求来源地址。
//
// 导出给admin 包的登录限速用：它必须和鉴权中间件按同一口径计数，
// 否则两处限速各算各的，攻击者可以从较松的那处撞进来。
func ClientIPOf(r *http.Request) string { return clientIP(r) }

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

// LoginFailLimit / LoginCooldown 是登录失败的限速参数，导出给
// admin 包的登录端点复用，保证「登录失败」与「管理鉴权失败」同一套阈值。
const (
	LoginFailLimit = loginFailLimit
	LoginCooldown  = loginCooldown
)

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
	// inflight 是当前正在跑 KDF 的并发请求数。Allow 必须**先占用再放行**：
	// 只读计数的话，同 IP 的 N 个并发请求会在第一次 Fail 落地前全部通过，
	// 既击穿限速，又把 PBKDF2 的 CPU 消耗放大 N 倍。
	inflight int
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

// Allow 报告该 IP 当前是否允许尝试。返回 true 时同时**占用一个并发额度**，
// 调用方必须在验证结束后调用 Release 归还（Success/Fail 内部已各自归还，
// 只有中途 return 的分支需要手动调）。
func (t *FailureThrottle) Allow(ip string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	e, ok := t.fails[ip]
	if !ok {
		t.fails[ip] = &failEntry{inflight: 1}
		return true
	}
	if !e.until.IsZero() && time.Now().Before(e.until) {
		return false
	}
	// 并发上限：同一个 IP 最多同时有 maxConcurrent 个请求在跑 KDF。
	if e.inflight >= maxConcurrentAttempts {
		return false
	}
	e.inflight++
	return true
}

// maxConcurrentAttempts 是单个来源 IP 允许同时进行的鉴权尝试数。
// 超出的请求直接按限速处理（429），不给「并发抢先」留窗口。
const maxConcurrentAttempts = 4

// Release 归还 Allow 占用的并发额度。ip 没有条目时是 no-op
// （Success 删条目后迟到的 Release 不该凭空造出一个新条目）。
func (t *FailureThrottle) Release(ip string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if e, ok := t.fails[ip]; ok && e.inflight > 0 {
		e.inflight--
	}
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
	// 归还 Allow 占用的并发额度（Fail 是一次尝试的终点）。
	if e.inflight > 0 {
		e.inflight--
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
	e, ok := t.fails[ip]
	if !ok {
		return
	}
	// 仍有并发尝试在跑时不能直接删条目 —— 那些请求结束时会调用 Fail，
	// 删掉条目会让它们凭空造出一个 count=0 的新条目，把失败计数清零。
	// 正确做法是清零失败状态、保留并发计数，等最后一个请求离开时再删。
	e.count = 0
	e.until = time.Time{}
	if e.inflight <= 0 {
		delete(t.fails, ip)
	}
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
			//
			// 注意 script-src **不含** 'unsafe-inline'：主题防闪脚本曾经以
			// 内联 IIFE 写在 index.html 的 <head> 里，被这条 CSP 静默拦掉 ——
			// 功能直接坏掉（暗色用户首帧白闪回归），而浏览器只在控制台报一条
			// 违规，测试与构建全绿，看不出任何异常。该脚本已外置为
			// /admin/assets/theme-init.js（见 web/index.html）。
			h.Set("Content-Security-Policy",
				"default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; "+
					"img-src 'self' data:; connect-src 'self'; base-uri 'self'; "+
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
