package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/cn-maul/rosetta-gateway/internal/config"
	"github.com/cn-maul/rosetta-gateway/internal/protocol"
	"github.com/cn-maul/rosetta-gateway/internal/store"
)

// provider_probe_test.go 覆盖「上游协议探测」端点。
//
// 这个端点的价值全在**判定口径**上，所以测试的主体是「状态码 → ok 结论」的
// 映射表，而不是响应结构：把 400/401/403/404/405/422/429/500 钉死，
// 才能防止有人把判据从「不是 404/405」悄悄改成「== 200」（那会把所有协议
// 都判成不支持）或「4xx 即不支持」（那会把「路径对了只差 key」判死）。
//
// 另一组测试盯的是安全边界：请求体必须是不带 model 的 `{}`（否则探测会
// 真的产生推理计费），api_key 只许出现在发往用户填的那个 endpoint 的
// 请求头里，绝不回显在响应体或任何 message 里。

// probeRecorder 记录假上游收到的请求，供事后断言 path / header / body。
type probeRecorder struct {
	mu   sync.Mutex
	reqs []recordedProbe
}

type recordedProbe struct {
	Path   string
	Host   string
	Header http.Header
	Body   string
}

func (p *probeRecorder) add(r *http.Request, body string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.reqs = append(p.reqs, recordedProbe{Path: r.URL.Path, Host: r.Host, Header: r.Header.Clone(), Body: body})
}

func (p *probeRecorder) all() []recordedProbe {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]recordedProbe, len(p.reqs))
	copy(out, p.reqs)
	return out
}

// findByPath 按路径取记录；假上游的回调是并发的，断言前统一在这里取。
func findByPath(recs []recordedProbe, path string) *recordedProbe {
	for i := range recs {
		if recs[i].Path == path {
			return &recs[i]
		}
	}
	return nil
}

// newProbeUpstream 起一个按「路径 → 状态码」分流的假上游。
//
// 分流而不是一律回同一个码，是因为测的正是「三条路径的结论互不相同」：
// 只有这样，「结果按下标错位」「三条都读同一个响应」这类实现错误才会红。
func newProbeUpstream(t *testing.T, byPath map[string]int) (*httptest.Server, *probeRecorder) {
	t.Helper()
	rec := &probeRecorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := make([]byte, r.ContentLength)
		if r.ContentLength > 0 {
			_, _ = r.Body.Read(body)
		}
		rec.add(r, string(body))
		status, ok := byPath[r.URL.Path]
		if !ok {
			// 未声明的路径一律 404：让「拼错路径」立刻表现为 ok=false，
			// 而不是被默认分支的 200 掩盖。
			status = http.StatusNotFound
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"error":"upstream says no"}`))
	}))
	t.Cleanup(srv.Close)
	return srv, rec
}

func probeProtocols(t *testing.T, h *ProviderHandler, body map[string]string) (*httptest.ResponseRecorder, protocolProbeResult) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ProbeProtocols(rec, asAdmin(jsonRequest(http.MethodPost, "/admin/api/providers/probe-protocols", jsonBody(t, body))))
	// 被拒绝（400）时响应体是错误信封，解成 probe 结果会失败，交给调用方判断。
	var resp protocolProbeResult
	if rec.Code == http.StatusOK {
		decodeBody(t, rec, &resp)
	}
	return rec, resp
}

// 三条路径分别回 400 / 404 / 401：ok 必须是 true / false / true，
// 且 status 原样透出（前端靠 status 区分「不支持」与「key 不对」）。
func TestProbeProtocolsJudgesEachPathIndependently(t *testing.T) {
	srv, recs := newProbeUpstream(t, map[string]int{
		"/v1/chat/completions": http.StatusBadRequest,
		"/v1/responses":        http.StatusNotFound,
		"/v1/messages":         http.StatusUnauthorized,
	})
	st := newTestStore(t)
	h := NewProviderHandler(st, nil, config.Default())

	const key = "sk-probe-secret-value-123456"
	rec, resp := probeProtocols(t, h, map[string]string{
		"endpoint": srv.URL,
		"api_key":  key,
	})

	if rec.Code != http.StatusOK {
		t.Fatalf("探测结论不是 HTTP 错误，应恒 200，实际 %d body=%s", rec.Code, rec.Body.String())
	}
	if len(resp.Results) != 3 {
		t.Fatalf("应恒回三条结论，实际 %d: %s", len(resp.Results), rec.Body.String())
	}

	// 顺序固定 chat / response / anthropic：前端按下标渲染。
	wantOrder := []string{protocol.Chat, protocol.Response, protocol.Anthropic}
	for i, want := range wantOrder {
		if resp.Results[i].Protocol != want {
			t.Fatalf("第 %d 条协议应为 %q，实际 %+v", i, want, resp.Results)
		}
	}

	cases := []struct {
		idx      int
		ok       bool
		status   int
		contains string
	}{
		{0, true, http.StatusBadRequest, "路径存在"},
		{1, false, http.StatusNotFound, "路径不存在"},
		{2, true, http.StatusUnauthorized, "鉴权未通过"},
	}
	for _, c := range cases {
		got := resp.Results[c.idx]
		if got.OK != c.ok || got.Status != c.status {
			t.Fatalf("%s 结论应为 ok=%v status=%d，实际 %+v", got.Protocol, c.ok, c.status, got)
		}
		if !strings.Contains(got.Message, c.contains) {
			t.Fatalf("%s 的 message 应包含 %q，实际 %q", got.Protocol, c.contains, got.Message)
		}
	}

	// 请求体必须是 `{}`：带 model 就会真的产生推理计费，而且探测会退化成
	// 「你得先知道这个上游认哪个模型名」。
	for _, want := range []string{"/v1/chat/completions", "/v1/responses", "/v1/messages"} {
		got := findByPath(recs.all(), want)
		if got == nil {
			t.Fatalf("假上游没有收到 %s", want)
		}
		if strings.TrimSpace(got.Body) != "{}" {
			t.Fatalf("%s 的请求体应是空对象 {}，实际 %q", want, got.Body)
		}
	}

	// 凭据只许出现发往上游的请求头里，且按协议分头。
	chat := findByPath(recs.all(), "/v1/chat/completions")
	if got := chat.Header.Get("Authorization"); got != "Bearer "+key {
		t.Fatalf("chat 应带 Authorization: Bearer <key>，实际 %q", got)
	}
	messages := findByPath(recs.all(), "/v1/messages")
	if got := messages.Header.Get("x-api-key"); got != key {
		t.Fatalf("anthropic 应带 x-api-key，实际 %q", got)
	}
	if got := messages.Header.Get("Authorization"); got != "" {
		t.Fatalf("anthropic 不该同时带 Authorization，实际 %q", got)
	}
	if got := messages.Header.Get("anthropic-version"); got != anthropicVersionHeader {
		t.Fatalf("anthropic 应带 anthropic-version=%s，实际 %q", anthropicVersionHeader, got)
	}

	// 响应体里绝不能出现 api_key。
	if strings.Contains(rec.Body.String(), key) {
		t.Fatalf("响应体泄漏了 api_key: %s", rec.Body.String())
	}
}

// 三条路径全 404：三条结论都必须是「不存在」。
func TestProbeProtocolsAllMissing(t *testing.T) {
	srv, _ := newProbeUpstream(t, map[string]int{}) // 全部落到默认 404
	st := newTestStore(t)
	h := NewProviderHandler(st, nil, config.Default())

	rec, resp := probeProtocols(t, h, map[string]string{"endpoint": srv.URL})
	if rec.Code != http.StatusOK {
		t.Fatalf("全 404 也是探测结论，应 200，实际 %d body=%s", rec.Code, rec.Body.String())
	}
	for _, r := range resp.Results {
		if r.OK {
			t.Fatalf("%s path 404 应判 ok=false，实际 %+v", r.Protocol, r)
		}
		if r.Status != http.StatusNotFound {
			t.Fatalf("%s 应透出 404，实际 %+v", r.Protocol, r)
		}
	}
}

// 405（方法不对）等同上一条路径不存在。
//
// 单独一条：判据是「不是 404 且不是 405」，漏掉 405 的症状是某些只实现
// GET 的网关会被判成「支持该协议」，之后转发必然失败。
func TestProbeProtocolsTreatsMethodNotAllowedAsMissing(t *testing.T) {
	srv, _ := newProbeUpstream(t, map[string]int{
		"/v1/chat/completions": http.StatusMethodNotAllowed,
		"/v1/responses":        http.StatusMethodNotAllowed,
		"/v1/messages":         http.StatusMethodNotAllowed,
	})
	st := newTestStore(t)
	h := NewProviderHandler(st, nil, config.Default())

	_, resp := probeProtocols(t, h, map[string]string{"endpoint": srv.URL})
	for _, r := range resp.Results {
		if r.OK {
			t.Fatalf("405 应判 ok=false，实际 %+v", r)
		}
	}
}

// 400/401/403/422/429/500 都算「路径存在」——把这些码钉死，
// 防止判据被改成「== 200」。
func TestProbeProtocolsAnyNonMissingStatusMeansExists(t *testing.T) {
	for _, status := range []int{
		http.StatusBadRequest,
		http.StatusUnauthorized,
		http.StatusForbidden,
		http.StatusUnprocessableEntity,
		http.StatusTooManyRequests,
		http.StatusInternalServerError,
	} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			srv, _ := newProbeUpstream(t, map[string]int{
				"/v1/chat/completions": status,
				"/v1/responses":        status,
				"/v1/messages":         status,
			})
			st := newTestStore(t)
			h := NewProviderHandler(st, nil, config.Default())

			_, resp := probeProtocols(t, h, map[string]string{"endpoint": srv.URL})
			for _, r := range resp.Results {
				if !r.OK || r.Status != status {
					t.Fatalf("HTTP %d 应判 ok=true，实际 %+v", status, r)
				}
			}
		})
	}
}

// endpoint 已带 /v1 时不能再补一层版本：否则会打 /v1/v1/chat/completions，
// 三条全 404，而管理员的配置其实是好的。
func TestProbeProtocolsKeepsExistingVersionPath(t *testing.T) {
	srv, recs := newProbeUpstream(t, map[string]int{
		"/v1/chat/completions": http.StatusBadRequest,
		"/v1/responses":        http.StatusBadRequest,
		"/v1/messages":         http.StatusBadRequest,
	})
	st := newTestStore(t)
	h := NewProviderHandler(st, nil, config.Default())

	_, resp := probeProtocols(t, h, map[string]string{"endpoint": srv.URL + "/v1"})
	for _, r := range resp.Results {
		if !r.OK {
			t.Fatalf("endpoint 带 /v1 时不该拼出 /v1/v1：%+v", r)
		}
	}
	for _, got := range recs.all() {
		if strings.Contains(got.Path, "/v1/v1/") {
			t.Fatalf("路径被重复补了版本层：%s", got.Path)
		}
	}
}

// 参数非法才 400：endpoint 缺失 / 不合法 / 网络不存在 / 代理地址损坏。
func TestProbeProtocolsRejectsBadParamsOnly(t *testing.T) {
	st := newTestStore(t)
	h := NewProviderHandler(st, nil, config.Default())

	cases := []struct {
		name string
		body map[string]string
		want string
	}{
		{"缺 endpoint", map[string]string{}, "endpoint is required"},
		{"endpoint 只有空白", map[string]string{"endpoint": "   "}, "endpoint is required"},
		{"endpoint 不是 URL", map[string]string{"endpoint": "api.example.com"}, "合法的 URL"},
		{"endpoint 带查询串", map[string]string{"endpoint": "https://x.com/api?key=1"}, "查询串"},
		{"endpoint 带 userinfo", map[string]string{"endpoint": "https://user:pw@x.com"}, "合法的 URL"},
		{"endpoint 非 http", map[string]string{"endpoint": "ftp://x.com"}, "http/https"},
		{"网络不存在", map[string]string{"endpoint": "https://x.com", "network_id": "net-nope"}, "不存在"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec, _ := probeProtocols(t, h, c.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("应为 400，实际 %d body=%s", rec.Code, rec.Body.String())
			}
			assertSingleErrorEnvelope(t, rec, http.StatusBadRequest)
			if !strings.Contains(rec.Body.String(), c.want) {
				t.Fatalf("错误文案应包含 %q，实际 %s", c.want, rec.Body.String())
			}
		})
	}
}

// 网络条目里的 proxy_url 坏了必须回 400，不能伪装成「三条路径都不存在」。
//
// 失效模式：三条 ok=false 会把管理员指向「上游不支持」这个错误方向，
// 而真正的原因在网络配置里。
func TestProbeProtocolsReportsBrokenProxyAsParamError(t *testing.T) {
	st := newTestStore(t)
	ctx := t.Context()
	if err := st.CreateNetwork(ctx, &store.Network{ID: "net-bad", Name: "坏代理", ProxyURL: "://not-a-url"}); err != nil {
		t.Fatalf("create network: %v", err)
	}
	h := NewProviderHandler(st, nil, config.Default())

	rec, _ := probeProtocols(t, h, map[string]string{
		"endpoint":   "https://api.example.com",
		"network_id": "net-bad",
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("代理地址损坏应 400，实际 %d body=%s", rec.Code, rec.Body.String())
	}
	assertSingleErrorEnvelope(t, rec, http.StatusBadRequest)
}

// network_id 生效：请求必须从代理出去，而不是直连。
//
// 用一个假代理（httptest 服务器）验证 —— 它既是代理又什么都不转发，
// 所以只要请求到达这里就证明走了代理；同时上游域名是 .invalid，
// 直连必然 DNS 失败，结论会截然不同。
func TestProbeProtocolsUsesNetworkProxy(t *testing.T) {
	proxy, recs := newProbeUpstream(t, map[string]int{
		"/v1/chat/completions": http.StatusBadRequest,
		"/v1/responses":        http.StatusNotFound,
		"/v1/messages":         http.StatusBadRequest,
	})
	st := newTestStore(t)
	ctx := t.Context()
	if err := st.CreateNetwork(ctx, &store.Network{ID: "net-1", Name: "公司代理", ProxyURL: proxy.URL}); err != nil {
		t.Fatalf("create network: %v", err)
	}
	h := NewProviderHandler(st, nil, config.Default())

	_, resp := probeProtocols(t, h, map[string]string{
		"endpoint":   "http://upstream.invalid",
		"network_id": "net-1",
	})
	if len(recs.all()) != 3 {
		t.Fatalf("三条探测都应经过代理，代理实际收到 %d 条", len(recs.all()))
	}
	for _, r := range resp.Results {
		if r.Message == "" {
			t.Fatalf("探测结论缺少 message：%+v", r)
		}
	}
	// 结论必须仍按「看状态码」得出：代理回 400 的 chat 是存在，回 404 的
	// response 是不存在。若代理没生效（直连 .invalid 必然 DNS 失败），
	// 这里会是 status=0 的传输失败。
	if resp.Results[0].OK != true || resp.Results[1].OK != false {
		t.Fatalf("代理生效后应看到 chat ok=true / response ok=false，实际 %+v", resp.Results)
	}
	// 通过代理时请求行是 absolute-form，Host 仍是上游域名 —— 证明没有被
	// 悄悄改成直连（直连会 DNS 失败、代理收不到请求）。
	if got := recs.all()[0].Host; got != "upstream.invalid" {
		t.Fatalf("代理收到的 Host 应为 upstream.invalid，实际 %q", got)
	}
}

// 传输层失败（连不上）时 status 为 0、ok 为 false，且 message 不含 key。
func TestProbeProtocolsTransportFailureIsConclusion(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.Close() // 立刻关掉：拿到一个「拒绝连接」的地址

	st := newTestStore(t)
	h := NewProviderHandler(st, nil, config.Default())
	const key = "sk-should-never-appear"
	rec, resp := probeProtocols(t, h, map[string]string{"endpoint": srv.URL, "api_key": key})

	if rec.Code != http.StatusOK {
		t.Fatalf("传输层失败也是探测结论，应 200，实际 %d body=%s", rec.Code, rec.Body.String())
	}
	for _, r := range resp.Results {
		if r.OK || r.Status != 0 {
			t.Fatalf("连不上应 ok=false status=0，实际 %+v", r)
		}
		if !strings.Contains(r.Message, "请求失败") {
			t.Fatalf("message 应说明请求失败，实际 %q", r.Message)
		}
	}
	if strings.Contains(rec.Body.String(), key) {
		t.Fatalf("响应体泄漏了 api_key: %s", rec.Body.String())
	}
}

// 没给 api_key 时 anthropic 的 401 文案要引导「填 key」而不是「key 不对」，
// 否则管理员会去翻一个自己从没填过的字段。
func TestProbeProtocolsHintMissingKeySeparately(t *testing.T) {
	srv, _ := newProbeUpstream(t, map[string]int{"/v1/messages": http.StatusUnauthorized})
	st := newTestStore(t)
	h := NewProviderHandler(st, nil, config.Default())

	_, resp := probeProtocols(t, h, map[string]string{"endpoint": srv.URL})
	if len(resp.Results) != 3 {
		t.Fatalf("应回三条结论，实际 %+v", resp.Results)
	}
	msg := resp.Results[2].Message
	if !strings.Contains(msg, "请填写 api_key") {
		t.Fatalf("缺 key 的 401 应提示填写 api_key，实际 %q", msg)
	}
	if strings.Contains(msg, "是否属于这个上游") {
		t.Fatalf("缺 key 时不该提示「检查 key 是否属于该上游」，实际 %q", msg)
	}
}

// 响应形状契约：results[].{protocol,ok,status,message} 四个字段齐全且
// message 恒非空（前端直接把它当一行说明渲染，空串会留下一块空白）。
func TestProbeProtocolsResponseShape(t *testing.T) {
	srv, _ := newProbeUpstream(t, map[string]int{
		"/v1/chat/completions": http.StatusBadRequest,
		"/v1/responses":        http.StatusBadRequest,
		"/v1/messages":         http.StatusBadRequest,
	})
	st := newTestStore(t)
	h := NewProviderHandler(st, nil, config.Default())

	rec, _ := probeProtocols(t, h, map[string]string{"endpoint": srv.URL})
	var raw struct {
		Results []map[string]any `json:"results"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("响应不是合法 JSON: %v body=%s", err, rec.Body.String())
	}
	for i, item := range raw.Results {
		for _, field := range []string{"protocol", "ok", "status", "message"} {
			if _, ok := item[field]; !ok {
				t.Fatalf("第 %d 条缺少字段 %q: %s", i, field, rec.Body.String())
			}
		}
		if msg, _ := item["message"].(string); msg == "" {
			t.Fatalf("第 %d 条 message 不应为空: %s", i, rec.Body.String())
		}
	}
}
