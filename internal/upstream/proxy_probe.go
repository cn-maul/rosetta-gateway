package upstream

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"
)

// proxyProbeURL 是经代理访问的轻量探测地址。
//
// 选 generate_204 而不是任意网站首页：正常路径没有正文，也不会因为页面改版
// 把一次健康检查变成假失败。非 204 会被判为失败，避免代理登录页或拦截页
// 用 HTTP 200 冒充连通。
const proxyProbeURL = "https://www.gstatic.com/generate_204"

// TestProxyConnectivity 用与真实上游请求相同的代理 transport 做一次短请求。
//
// 只测「代理服务器可达」不够：端口能连上但代理配置错误、认证失败或无法
// 转发流量时，上游请求仍会失败。这里让请求真正经过代理到达一个固定目标，
// 因此同时覆盖代理连接、协议协商与出站转发。
func TestProxyConnectivity(ctx context.Context, proxyURL string) (int64, error) {
	const timeout = 10 * time.Second
	client, err := probeHTTPClientFor(proxyURL, timeout)
	if err != nil {
		return 0, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, proxyProbeURL, nil)
	if err != nil {
		return 0, err
	}
	start := time.Now()
	resp, err := client.Do(req)
	latencyMs := time.Since(start).Milliseconds()
	if err != nil {
		return latencyMs, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))

	if resp.StatusCode != http.StatusNoContent {
		return latencyMs, fmt.Errorf("代理探测返回 HTTP %d", resp.StatusCode)
	}
	return latencyMs, nil
}
