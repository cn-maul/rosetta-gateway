// Package httpx 提供与具体业务无关的 HTTP 边界能力：客户端 IP、同源判定、
// Cookie 安全属性与统一的错误信封。
//
// 这些能力同时被数据面中间件、管理面登录和审计使用。放在独立包里，
// 是为了让 internal/admin 不必反向依赖 internal/server。
package httpx

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
)

type contextKey string

const publicOriginKey contextKey = "public_origin"
const trustedProxiesKey contextKey = "trusted_proxies"
const resolvedClientIPKey contextKey = "resolved_client_ip"

// WithPublicOrigin 把配置里的公开源注入请求 context，供同源判定与
// 会话 Cookie 的 Secure 属性共同使用。
//
// 只在配置非空时注入。空串表示「未声明反代后的公开源」，此时同源判定
// 回落到进程实际观察到的 TLS 状态。
//
// 这里刻意不读取 X-Forwarded-*：反向代理后面的客户端可以伪造这些头，
// 而同源判定同时保护免鉴权的 bootstrap 写入口。公开源只能来自部署配置，
// 不能来自请求本身。
func WithPublicOrigin(next http.Handler, origin string) http.Handler {
	origin = strings.TrimSpace(origin)
	if origin == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := context.WithValue(r.Context(), publicOriginKey, origin)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// PublicOriginFromContext 返回配置的公开源；未配置时为空串。
func PublicOriginFromContext(ctx context.Context) string {
	v, _ := ctx.Value(publicOriginKey).(string)
	return v
}

// WithTrustedProxies 把可信反向代理网段注入请求 context。
//
// 只有这个显式配置存在时，ClientIPOf 才会解析 Forwarded / X-Forwarded-For。
// 空配置保持旧行为：转发头一律不可信。
func WithTrustedProxies(next http.Handler, prefixes []netip.Prefix) http.Handler {
	if len(prefixes) == 0 {
		return next
	}
	copied := make([]netip.Prefix, len(prefixes))
	copy(copied, prefixes)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := context.WithValue(r.Context(), trustedProxiesKey, copied)
		resolved := resolveClientIP(r, copied)
		ctx = context.WithValue(ctx, resolvedClientIPKey, resolved)
		// 数据面 auth 包读取 RemoteAddr 做 key 级 IP 白名单。把解析后的地址
		// 回写到这里，保证管理限速、审计、数据面白名单使用同一来源；原始
		// 对端地址不再参与后续判定。
		if addr, err := netip.ParseAddr(resolved); err == nil {
			r.RemoteAddr = net.JoinHostPort(addr.String(), "0")
		}
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// SameOrigin 报告请求是否来自同源。
//
// 判据优先级：
//  1. Sec-Fetch-Site —— 浏览器强制发送，页面 JS 无法伪造（属于
//     forbidden header name）。跨站导航/表单/fetch 一律是 cross-site。
//  2. Origin —— 浏览器对 CORS 相关请求必发。同样不可伪造。取其 scheme+host
//     与请求的 Host 比对；反向代理后两者可能不等，此时保守判为跨源。
//  3. 都没有 —— 非浏览器客户端（curl、SDK、服务间调用）。它不受浏览器
//     同源策略约束，因此不是 CSRF 的攻击面，放行。
func SameOrigin(r *http.Request) bool {
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" {
		return site == "same-origin" || site == "none"
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" || u.User != nil ||
		(u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return false
	}
	// 审计 A7-13：原先只比 Host，所以 http://gw.example 与 https://gw.example
	// 被当成同源。该函数同时守卫免鉴权的 bootstrap 写端点与 Cookie 写请求，
	// 只比 Host 就漏掉了协议降级这一维。
	originScheme := strings.ToLower(u.Scheme)
	// 配置了 public_origin 时，它就是浏览器侧 scheme+host 的唯一基准。
	// 网关自身只监听明文 HTTP；若前面有 TLS 反代，浏览器看到的 scheme 是
	// https，而 r.TLS 在进程内仍为 nil。用 X-Forwarded-Proto 会引入可伪造头，
	// 所以反代部署必须显式配置这个值，而不是相信请求头。
	if publicOrigin := PublicOriginFromContext(r.Context()); publicOrigin != "" {
		base, err := url.Parse(publicOrigin)
		if err != nil || base.Host == "" {
			return false
		}
		return originScheme == strings.ToLower(base.Scheme) &&
			strings.EqualFold(u.Host, base.Host)
	}
	// 未配置公开源：只接受进程实际观察到的 TLS 状态。这样纯 HTTP 局域网
	// 部署保持原行为，反代部署若忘记配置则保守拒绝（而不是静默放行）。
	requestScheme := "http"
	if r.TLS != nil {
		requestScheme = "https"
	}
	if originScheme != requestScheme {
		return false
	}
	return strings.EqualFold(u.Host, r.Host)
}

// SessionCookieSecure 报告当前请求对应的会话 Cookie 是否应带 Secure。
//
// 配置了 https 的 public_origin 时必须带；未配置时按进程实际是否终止 TLS
// 判断。两者都不看 X-Forwarded-Proto。
func SessionCookieSecure(r *http.Request) bool {
	if origin := PublicOriginFromContext(r.Context()); origin != "" {
		return strings.HasPrefix(strings.ToLower(origin), "https://")
	}
	return r.TLS != nil
}

// WriteForbidden 写统一的跨站拒绝响应。
func WriteForbidden(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	//nolint:errcheck // 响应写入失败已无补救手段
	w.Write([]byte(`{"error":{"message":"拒绝跨站请求","type":"invalid_request_error"}}`))
}

// WriteAuthError 写管理面的统一鉴权错误信封。
func WriteAuthError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]string{"message": message, "type": "auth_error"},
	})
}

// ClientIPOf 取请求来源地址（已按 trusted_proxy_cidrs 解析可信转发链）。
//
// 鉴权、登录限速、审计与数据面 IP 白名单必须共用这一口径；各处自行解析
// 转发头会让同一个请求在不同安全边界上被识别成不同客户端。
func ClientIPOf(r *http.Request) string {
	if resolved, ok := r.Context().Value(resolvedClientIPKey).(string); ok && resolved != "" {
		return resolved
	}
	return resolveClientIP(r, trustedProxiesFromContext(r.Context()))
}

func resolveClientIP(r *http.Request, trusted []netip.Prefix) string {
	remote := remoteHost(r.RemoteAddr)
	if len(trusted) == 0 {
		return remote
	}
	remoteAddr, ok := parseProxyAddr(remote)
	if !ok || !isTrustedProxy(remoteAddr, trusted) {
		return remote
	}

	chain, ok := forwardedChain(r)
	if !ok || len(chain) == 0 {
		return remote
	}
	for i := len(chain) - 1; i >= 0; i-- {
		if !isTrustedProxy(chain[i], trusted) {
			return chain[i].String()
		}
	}
	return chain[0].String()
}

func trustedProxiesFromContext(ctx context.Context) []netip.Prefix {
	prefixes, _ := ctx.Value(trustedProxiesKey).([]netip.Prefix)
	return prefixes
}

func remoteHost(remoteAddr string) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err == nil {
		return host
	}
	return strings.Trim(remoteAddr, "[]")
}

func forwardedChain(r *http.Request) ([]netip.Addr, bool) {
	if values := r.Header.Values("Forwarded"); len(values) > 0 {
		return parseForwarded(values)
	}
	if values := r.Header.Values("X-Forwarded-For"); len(values) > 0 {
		return parseXForwardedFor(values)
	}
	return nil, false
}

func parseForwarded(values []string) ([]netip.Addr, bool) {
	var out []netip.Addr
	for _, value := range values {
		for _, element := range strings.Split(value, ",") {
			var forValue string
			for _, part := range strings.Split(element, ";") {
				name, raw, ok := strings.Cut(strings.TrimSpace(part), "=")
				if ok && strings.EqualFold(strings.TrimSpace(name), "for") {
					forValue = strings.TrimSpace(raw)
					break
				}
			}
			if forValue == "" {
				return nil, false
			}
			addr, ok := parseProxyAddr(forValue)
			if !ok {
				return nil, false
			}
			out = append(out, addr)
		}
	}
	return out, len(out) > 0
}

func parseXForwardedFor(values []string) ([]netip.Addr, bool) {
	var out []netip.Addr
	for _, value := range values {
		for _, raw := range strings.Split(value, ",") {
			addr, ok := parseProxyAddr(strings.TrimSpace(raw))
			if !ok {
				return nil, false
			}
			out = append(out, addr)
		}
	}
	return out, len(out) > 0
}

func parseProxyAddr(raw string) (netip.Addr, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.EqualFold(raw, "unknown") || strings.HasPrefix(raw, "_") {
		return netip.Addr{}, false
	}
	if strings.HasPrefix(raw, `"`) {
		unquoted, err := strconv.Unquote(raw)
		if err != nil {
			return netip.Addr{}, false
		}
		raw = strings.TrimSpace(unquoted)
	}

	if addr, err := netip.ParseAddr(raw); err == nil {
		return normalizeProxyAddr(addr), true
	}
	if host, _, err := net.SplitHostPort(raw); err == nil {
		raw = host
	} else if strings.HasPrefix(raw, "[") && strings.HasSuffix(raw, "]") {
		raw = raw[1 : len(raw)-1]
	}
	addr, err := netip.ParseAddr(raw)
	if err != nil {
		return netip.Addr{}, false
	}
	return normalizeProxyAddr(addr), true
}

func normalizeProxyAddr(addr netip.Addr) netip.Addr {
	if addr.Is4In6() {
		return addr.Unmap()
	}
	return addr
}

func isTrustedProxy(addr netip.Addr, prefixes []netip.Prefix) bool {
	addr = normalizeProxyAddr(addr)
	for _, prefix := range prefixes {
		p := prefix
		if p.Addr().Is4In6() {
			bits := p.Bits() - 96
			if bits < 0 {
				continue
			}
			p = netip.PrefixFrom(p.Addr().Unmap(), bits)
		}
		if p.Contains(addr) {
			return true
		}
	}
	return false
}
