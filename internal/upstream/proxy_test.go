package upstream

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cn-maul/rosetta"
)

// TestProxyHTTPClientFor_SharedWhenEmpty 守住「不配代理就共用全局 client」。
//
// 这条边界是性能的：给每个 provider 各建一个 client 会让连接池按 provider
// 数量分裂，MaxIdleConnsPerHost 被 N 等分，代理/上游侧同时维持 N 倍空闲连接。
// 而多数部署里绝大多数上游是直连的 —— 它们必须共享同一个池。
func TestProxyHTTPClientFor_SharedWhenEmpty(t *testing.T) {
	for _, in := range []string{"", "   "} {
		c, err := proxyHTTPClientFor(in)
		if err != nil {
			t.Fatalf("proxyHTTPClientFor(%q) error: %v", in, err)
		}
		if c != upstreamHTTPClient {
			t.Fatalf("proxyHTTPClientFor(%q) 应返回全局共享 client", in)
		}
	}
}

// TestProxyHTTPClientFor_DistinctPerURL 守住「不同代理 URL 得到不同 client」。
//
// Proxy 是 Transport 级属性，共用一个 client 就做不到 per-provider ——
// 这正是本功能存在的理由。
func TestProxyHTTPClientFor_DistinctPerURL(t *testing.T) {
	a := "http://127.0.0.1:7897"
	b := "http://127.0.0.1:8080"

	ca, err := proxyHTTPClientFor(a)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	cb, err := proxyHTTPClientFor(b)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if ca == cb {
		t.Fatalf("不同代理地址返回了同一个 client")
	}
	if ca == upstreamHTTPClient || cb == upstreamHTTPClient {
		t.Fatalf("配了代理却返回全局 client")
	}

	// 同一个 URL 必须拿到同一个 client：否则每把凭据各建一个，
	// 连接池按凭据数分裂（见 TestProxyHTTPClientFor_SharedWhenEmpty）。
	ca2, err := proxyHTTPClientFor(a)
	if err != nil {
		t.Fatalf("repeat: %v", err)
	}
	if ca2 != ca {
		t.Fatalf("同一代理地址返回了不同 client")
	}
}

// TestProxyHTTPClientFor_CachesAcrossProxyConfig 守住「同一个 client 被多次
// 使用」—— 建池时每把凭据都调一次 proxyHTTPClientFor，若不复用，
// N 把凭据就会建出 N 个 Transport。
func TestProxyHTTPClientFor_CachesAcrossProxyConfig(t *testing.T) {
	prov := &ProviderEntry{ProxyURL: "http://127.0.0.1:7897"}

	c1, err1 := proxyHTTPClientFor(prov.ProxyURL)
	c2, err2 := proxyHTTPClientFor(prov.ProxyURL)
	if err1 != nil || err2 != nil {
		t.Fatalf("errors: %v %v", err1, err2)
	}
	if c1 != c2 {
		t.Fatalf("同一 provider 多次建 client 未复用")
	}

	tr, ok := c1.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport 类型 = %T", c1.Transport)
	}
	if tr.Proxy == nil {
		t.Fatalf("代理 client 的 Transport.Proxy 为 nil")
	}
}

// TestProxyHTTPClientFor_InvalidURL 守住「配错代理不炸、只报错并回落」。
//
// 若让 buildClient 因代理配错而失败，症状会变成「路由配好了、请求莫名 500」，
// 而根因躺在建池日志里没人看。正确行为：回落全局 client 继续服务，
// 同时把错误交出去（记进 prov.ProxyConfigError，由建池循环打日志）。
func TestProxyHTTPClientFor_InvalidURL(t *testing.T) {
	for _, in := range []string{
		"://nohost",         // 空 scheme + 空 host
		"ftp://127.0.0.1:1", // 不支持的 scheme
		"http://",           // 有 scheme 无 host
		"127.0.0.1:7897",    // 缺 scheme
		"not a url at all",  // 含空格
	} {
		c, err := proxyHTTPClientFor(in)
		if err == nil {
			t.Fatalf("proxyHTTPClientFor(%q) 应报错", in)
		}
		if c != upstreamHTTPClient {
			t.Fatalf("proxyHTTPClientFor(%q) 报错时应回落全局 client，实际 %p", in, c)
		}
	}
}

// TestBuildClient_SetsProxyConfigError 守住代理配错时 client 仍能建成，
// 且错误被记到 provider 上供建池循环消费。
func TestBuildClient_SetsProxyConfigError(t *testing.T) {
	good := &ProviderEntry{
		Slug: "direct", Endpoint: "https://example.invalid/v1",
		Protocol: "openai-chat", MaxRetries: 0,
	}
	if _, err := buildClient(good, "sk-x", newTestConfig()); err != nil {
		t.Fatalf("无代理的 client 应建成: %v", err)
	}
	if good.ProxyConfigError != nil {
		t.Fatalf("无代理时不该有 ProxyConfigError: %v", good.ProxyConfigError)
	}

	bad := &ProviderEntry{
		Slug: "badproxy", Endpoint: "https://example.invalid/v1",
		Protocol: "openai-chat", MaxRetries: 0, ProxyURL: "ftp://127.0.0.1:1",
	}
	if _, err := buildClient(bad, "sk-x", newTestConfig()); err != nil {
		t.Fatalf("代理配错不该让 client 构建失败: %v", err)
	}
	if bad.ProxyConfigError == nil {
		t.Fatalf("代理配错时应记录 ProxyConfigError")
	}
}

// TestProxyHTTPClientFor_TrafficActuallyRouted 是唯一一条**真发请求**的用例：
// 起一个本地假代理 + 一个假上游，让 client 打上游，然后断言假代理收到了请求。
//
// 为什么值得单写一条：前面几条只证明「Transport.Proxy 被设上了」，而那是个
// 声明。真正要确认的是流量确实拐去了代理 —— http.ProxyURL 对 scheme 合法但
// 主机不存在的地址会静默失败（退回直连），这个失效模式只有真发请求才看得出来。
func TestProxyHTTPClientFor_TrafficActuallyRouted(t *testing.T) {
	var proxyHits int32
	var gotPath string
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&proxyHits, 1)
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"id":"cmpl-1","object":"chat.completion","created":0,
			"model":"upstream-model",
			"choices":[{"index":0,"message":{"role":"assistant","content":"pong"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}}`)
	}))
	defer proxy.Close()

	// 假上游一旦被直连，这条用例直接失败 —— 那说明代理没生效。
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("请求绕过代理、直连了上游（path=%s）", r.URL.Path)
		w.WriteHeader(http.StatusOK)
	}))
	defer up.Close()

	prov := &ProviderEntry{
		Slug: "proxied", Endpoint: up.URL + "/v1",
		Protocol: "openai-chat", MaxRetries: 0,
		ProxyURL: proxy.URL,
	}
	client, err := buildClient(prov, "sk-x", newTestConfig())
	if err != nil {
		t.Fatalf("build client: %v", err)
	}
	if prov.ProxyConfigError != nil {
		t.Fatalf("合法代理不该报错: %v", prov.ProxyConfigError)
	}

	resp, err := client.Chat(context.Background(), &rosetta.ChatRequest{
		Model:    "upstream-model",
		Messages: []rosetta.Message{rosetta.User("ping")},
	})
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	if resp.Text() != "pong" {
		t.Fatalf("response text = %q", resp.Text())
	}

	if n := atomic.LoadInt32(&proxyHits); n != 1 {
		t.Fatalf("代理收到 %d 次请求，期望 1 次 —— 流量没有走代理", n)
	}
	// 代理转发的 path 必须是上游的完整路径：证明它转发到了正确的目标。
	if !strings.Contains(gotPath, "/v1/chat/completions") {
		t.Fatalf("代理转发的 path = %q，期望含 /v1/chat/completions", gotPath)
	}
}
