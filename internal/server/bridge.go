package server

// HTTP 边界能力的薄委托层。
//
// 实现已移到 internal/httpx，server 只保留原有导出名，供 cmd/gateway 的
// 中间件链继续使用。internal/admin 不允许再经过 server 取这些能力。

import (
	"net/http"
	"net/netip"

	"github.com/cn-maul/rosetta-gateway/internal/httpx"
)

func WithPublicOrigin(next http.Handler, origin string) http.Handler {
	return httpx.WithPublicOrigin(next, origin)
}

func WithTrustedProxies(next http.Handler, prefixes []netip.Prefix) http.Handler {
	return httpx.WithTrustedProxies(next, prefixes)
}
