package main

// This file keeps the data-plane balance preflight and the process-level
// loopback check. Pricing policy lives in internal/billing; read-only billing
// query endpoints live in internal/billinghttp.

import (
	"net"
	"strings"

	"github.com/cn-maul/rosetta-gateway/internal/billing"
	"github.com/cn-maul/rosetta-gateway/internal/snapshot"
)

// isLoopbackListen 报告 listen 地址是否只绑定回环。
//
// 用途：判断「首次初始化窗口 + 绑非回环」这个组合是否构成
// **能访问该端口的人可以抢先成为第一个管理员**的暴露面。
// 解析失败时**保守返回 false** —— 宁可多报一次警，不可漏掉真实暴露。
func isLoopbackListen(listen string) bool {
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		// 可能是 ":8666"（空 host = 所有接口）或压根不是 host:port。
		if strings.HasPrefix(listen, ":") {
			return false
		}
		return false
	}
	host = strings.TrimSpace(host)
	// 空 host 意味着绑定全部接口（Go 的 net.Listen 语义）。
	if host == "" {
		return false
	}
	// 已经是 IP 字面量。
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	// 主机名：localhost 及常见别名视为回环。
	switch strings.ToLower(host) {
	case "localhost", "localhost.localname", "ip6-localhost", "ip6-loopback":
		return true
	}
	return false
}

// balanceExempt adapts the runtime snapshot to billing's pure policy.
func balanceExempt(userID string) bool {
	u := snapshot.Get().UsersByID[userID]
	return u != nil && billing.IsBalanceExempt(u.Role)
}
