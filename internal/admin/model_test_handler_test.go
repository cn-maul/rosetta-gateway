package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/cn-maul/rosetta-gateway/internal/config"
	"github.com/cn-maul/rosetta-gateway/internal/store"
)

// ---------------------------------------------------------------------------
// 单模型可用性探测（ModelHandler.Test）
//
// # 这批测试要守住的三件事
//
//  1. **真的发了推理请求，且是给对的那个模型**。这是本端点的全部价值：
//     它存在的理由就是 ListModels 证明不了「某个 model_id 能推理」。
//     所以 fake 上游必须断言收到的 model 与 max_tokens —— 只断言响应体的话，
//     一个「压根没打上游、直接回 ok」的实现也能全绿（这正是 provider 级
//     测试当年踩过的假信号形态）。
//  2. **不存在的模型是 404，不是 200 + error**。两者对调用方的含义不同：
//     前者是「你调错了」，后者是「模型不可用」。
//  3. **绝不碰外网**。全部走 httptest 假上游；缺凭据的用例连假上游都不该被
//     碰到（用计数器断言 0 次请求）。
// ---------------------------------------------------------------------------

// probeUpstream 是一个记录请求的假上游，只实现探测会用到的那一个端点。
type probeUpstream struct {
	srv *httptest.Server

	mu       sync.Mutex
	requests []map[string]any
	paths    []string
	hits     int
}

// recordingProbeUpstream 起一个假上游：记录收到的 JSON body 与路径，
// 并按 status/body 回应。status=0 表示按 200 + 一个合法的最小补全回应。
func recordingProbeUpstream(t *testing.T, status int, body string) *probeUpstream {
	t.Helper()
	u := &probeUpstream{}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(raw, &m)

		u.mu.Lock()
		u.hits++
		u.requests = append(u.requests, m)
		u.paths = append(u.paths, r.URL.Path)
		u.mu.Unlock()

		if status != 0 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		// 最小的合法 OpenAI Chat 补全：一个 token 的输出 + usage。
		// usage 刻意给输出 1，与 max_tokens=1 的意图一致。
		_, _ = w.Write([]byte(`{"id":"c1","object":"chat.completion","model":"m",
			"choices":[{"index":0,"message":{"role":"assistant","content":"h"},"finish_reason":"length"}],
			"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}}`))
	}))
	t.Cleanup(u.srv.Close)
	return u
}

func (u *probeUpstream) hitCount() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.hits
}

func (u *probeUpstream) lastRequest(t *testing.T) map[string]any {
	t.Helper()
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.requests) == 0 {
		t.Fatal("假上游一次请求都没收到")
	}
	return u.requests[len(u.requests)-1]
}

func (u *probeUpstream) firstPath(t *testing.T) string {
	t.Helper()
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.paths) == 0 {
		t.Fatal("假上游一次请求都没收到")
	}
	return u.paths[0]
}

// probeTestStore 建一个指向给定 endpoint 的 provider + 一条凭据 + 一个模型。
//
// masterKey 传 nil：加密走 encryptSecret 的明文分支，decryptCredentialKey 的
// DecryptWithFallback 会原样读出 —— 测试不关心加密，只关心「凭据能不能解出
// 一把非空 key」。加密路径本身由 crypto 包的测试覆盖。
func probeTestStore(t *testing.T, endpoint string, withCredential bool) *store.Store {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	st, err := store.Open(t.TempDir()+"/probe.db", logger)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	ctx := context.Background()
	if err := st.CreateProvider(ctx, &store.Provider{
		ID: "p1", Slug: "probe", Name: "Probe",
		Endpoint: endpoint, Protocol: "openai-chat", Enabled: true,
	}); err != nil {
		t.Fatalf("create provider: %v", err)
	}
	if withCredential {
		if err := st.CreateCredential(ctx, &store.Credential{
			ID: "c1", ProviderID: "p1", Label: "default",
			APIKeyEnc: []byte("sk-test"), Enabled: true, Weight: 1, Status: "healthy",
		}); err != nil {
			t.Fatalf("create credential: %v", err)
		}
	}
	if err := st.CreateUpstreamModel(ctx, &store.UpstreamModel{
		ID: "m1", ProviderID: "p1", ModelID: "probe-model", Enabled: true,
	}); err != nil {
		t.Fatalf("create model: %v", err)
	}
	return st
}

func probeHandler(t *testing.T, st *store.Store) *ModelHandler {
	t.Helper()
	return NewModelHandler(st, nil, config.Default())
}

// callTest 调 handler.Test 并解出响应体。
func callTest(t *testing.T, h *ModelHandler, id string) (*httptest.ResponseRecorder, modelTestResponse) {
	t.Helper()
	req := jsonRequest(http.MethodPost, "/admin/api/models/"+id+"/test", bytes.NewBufferString(`{}`))
	rec := httptest.NewRecorder()
	h.Test(rec, req, id)
	var resp modelTestResponse
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("unmarshal: %v body=%s", err, rec.Body.String())
		}
	}
	return rec, resp
}

// 不存在的模型必须 404，而不是 200 + status="error"。
// 两者对调用方的含义不同：「你调错了 id」vs「这个模型不可用」。
// 混在一起会让前端把拼错的 id 显示成「模型测试失败」，
// 运维于是去查上游，而真正的问题是路径。
func TestModelTest_UnknownIDIs404(t *testing.T) {
	st := probeTestStore(t, "http://127.0.0.1:1", true)
	h := probeHandler(t, st)

	rec, _ := callTest(t, h, "no-such-model")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("want 404 for unknown id, got %d body=%s", rec.Code, rec.Body.String())
	}
}

// 缺凭据：必须在**建客户端阶段**就失败，而不是把请求发出去再报 401。
// 用假上游的命中计数器证明「一次都没打出去」—— 否则这条测试只证明了
// 「返回了错误」，而错误可能来自上游，那就完全没测到 resolveCredential。
func TestModelTest_NoCredentialFailsWithoutTouchingUpstream(t *testing.T) {
	up := recordingProbeUpstream(t, 0, "")
	st := probeTestStore(t, up.srv.URL, false)
	h := probeHandler(t, st)

	rec, resp := callTest(t, h, "m1")
	if rec.Code != http.StatusOK {
		t.Fatalf("probe failure must still be HTTP 200 (it is a result, not an API error), got %d", rec.Code)
	}
	if resp.Status != "error" {
		t.Fatalf("want status=error, got %q", resp.Status)
	}
	if !strings.Contains(resp.Message, "没有可用凭据") {
		t.Fatalf("message should name the real cause (no credential), got %q", resp.Message)
	}
	if n := up.hitCount(); n != 0 {
		t.Fatalf("无凭据时不该发出任何上游请求，实际发了 %d 次", n)
	}
}

// 这是本端点的核心断言：成功路径必须真的对**该模型**发出一次**最小**推理请求。
//
// 具体断言三件事，缺一不可：
//   - 请求打到了 /chat/completions（证明是推理调用，不是 /models）；
//   - body 里的 model == 配置的 model_id（证明测的是这一行，不是别的模型）；
//   - max_tokens（或 max_completion_tokens）== 1（证明成本被压到最低 ——
//     这是「真实推理」方案在经济上可接受的全部前提，回归时会最先被破坏）；
//   - 没有 stream=true（证明走的是非流式：探测不需要 SSE 那套失败路径）。
func TestModelTest_SendsMinimalRealInferenceToTheRightModel(t *testing.T) {
	up := recordingProbeUpstream(t, 0, "")
	st := probeTestStore(t, up.srv.URL, true)
	h := probeHandler(t, st)

	rec, resp := callTest(t, h, "m1")
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	if resp.Status != "ok" {
		t.Fatalf("want status=ok, got %q message=%q", resp.Status, resp.Message)
	}
	// 回显：前端据此把结果与行对上，避免「点了 A 行、结果是 B 行」的错觉。
	if resp.ModelID != "probe-model" {
		t.Fatalf("response must echo the probed model_id, got %q", resp.ModelID)
	}
	if resp.OutputTokens != 1 {
		t.Fatalf("上游回报的输出 token 应原样带出，got %d", resp.OutputTokens)
	}

	if !strings.HasSuffix(up.firstPath(t), "/chat/completions") {
		t.Fatalf("探测必须打推理端点，实际打到 %q", up.firstPath(t))
	}

	body := up.lastRequest(t)
	if body["model"] != "probe-model" {
		t.Fatalf("必须用该行配置的 model_id 探测，实际 model=%v", body["model"])
	}
	// SDK 默认发 max_completion_tokens，部分上游要求 max_tokens；
	// 两种拼写都接受，但**必须恰好有一个且等于 1**。
	cap1, ok1 := body["max_completion_tokens"]
	cap2, ok2 := body["max_tokens"]
	if !ok1 && !ok2 {
		t.Fatalf("探测请求必须带输出上限（max_tokens=1），实际 body=%v", body)
	}
	got := cap1
	if ok2 {
		got = cap2
	}
	if v, isNum := got.(float64); !isNum || v != 1 {
		t.Fatalf("输出上限必须是 1（成本下限），实际 %v", got)
	}
	if s, ok := body["stream"]; ok && s == true {
		t.Fatalf("探测走非流式，不该带 stream=true；body=%v", body)
	}
}

// 上游明确报错（模型下架 / 无权限 / 名字写错）→ status=error 且带上上游原因。
//
// 原因必须透传：那是管理员唯一能据以行动的信息（404 改模型名、403 开权限）。
// 这是「为什么不能用 ListModels 代替」的直接体现 —— 同一把凭据的
// /models 会照常返回 200。
func TestModelTest_UpstreamModelNotFoundSurfacesReason(t *testing.T) {
	up := recordingProbeUpstream(t, http.StatusNotFound,
		`{"error":{"message":"The model `+"`nope`"+` does not exist","type":"invalid_request_error","code":"model_not_found"}}`)
	st := probeTestStore(t, up.srv.URL, true)
	h := probeHandler(t, st)

	rec, resp := callTest(t, h, "m1")
	if rec.Code != http.StatusOK {
		t.Fatalf("上游报错是探测结论，管理接口仍应 200；got %d", rec.Code)
	}
	if resp.Status != "error" {
		t.Fatalf("want status=error, got %q", resp.Status)
	}
	if !strings.Contains(resp.Message, "does not exist") {
		t.Fatalf("上游原因必须透传给管理员，got %q", resp.Message)
	}
	if resp.InBand {
		t.Fatalf("404 是带外错误，InBand 应为 false，got true")
	}
}

// 某些中转网关用 HTTP 200 + 错误体回绝请求。此时只看状态码会把
// 「200 但没推理」判成可用 —— 这正是 InBand 字段存在的理由。
// 断言 InBand=true 且 status=error，让前端能把这种形态说明白。
func TestModelTest_InBandErrorIsNotSuccess(t *testing.T) {
	// 显式 200：下面这个 body 是错误体，但 HTTP 状态是成功。
	// 假上游必须能表达「状态码与语义不一致」这种形态，否则测不到它。
	up := recordingProbeUpstream(t, http.StatusOK,
		`{"error":{"message":"insufficient quota","type":"insufficient_quota"}}`)
	st := probeTestStore(t, up.srv.URL, true)
	h := probeHandler(t, st)

	rec, resp := callTest(t, h, "m1")
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d", rec.Code)
	}
	if resp.Status != "error" {
		t.Fatalf("HTTP 200 + 错误体不是成功，want status=error got %q", resp.Status)
	}
	if !resp.InBand {
		t.Fatalf("InBand 必须为 true，好让前端解释「HTTP 成功却报错」")
	}
	if !strings.Contains(resp.Message, "insufficient quota") {
		t.Fatalf("原因应透传，got %q", resp.Message)
	}
}

// 停用的模型也必须能测。
//
// 这是刻意的：管理员在「启用」之前最需要知道的就是「它到底能不能用」。
// 若这里加 enabled 前置判定，唯一的效果是把探测推后到启用之后 ——
// 那时坏模型已经进入候选池，一次真实流量才会撞上它。
func TestModelTest_DisabledModelIsStillProbed(t *testing.T) {
	up := recordingProbeUpstream(t, 0, "")
	st := probeTestStore(t, up.srv.URL, true)
	ctx := context.Background()
	if err := st.UpdateUpstreamModel(ctx, "m1", &store.UpstreamModel{
		ID: "m1", ProviderID: "p1", ModelID: "probe-model", Enabled: false,
	}); err != nil {
		t.Fatalf("disable model: %v", err)
	}
	h := probeHandler(t, st)

	_, resp := callTest(t, h, "m1")
	if resp.Status != "ok" {
		t.Fatalf("停用的模型仍应可探测，got %q message=%q", resp.Status, resp.Message)
	}
	if n := up.hitCount(); n != 1 {
		t.Fatalf("应恰好发出一次探测请求，实际 %d 次", n)
	}
}

// cfg 为 nil 时不能 panic 在请求路径上。
//
// 这条不是假想的：ModelHandler 的其它端点（Discover 之外的 Create/Update/List）
// 都不碰 h.cfg，所以既有测试与调用点一直允许传 nil
// （model_handler_test.go 里就是 NewModelHandler(st, nil, nil)）。
// Test 是第一个真正依赖 cfg 的端点（算超时 + 传给 NewProviderClient），
// 不兜底的话 nil deref 会把一个 500 变成 panic。
func TestModelTest_NilConfigDoesNotPanic(t *testing.T) {
	up := recordingProbeUpstream(t, 0, "")
	st := probeTestStore(t, up.srv.URL, true)
	h := NewModelHandler(st, nil, nil) // 刻意 nil cfg

	rec, resp := callTest(t, h, "m1")
	if rec.Code != http.StatusOK {
		t.Fatalf("nil cfg 下仍应正常返回，code=%d body=%s", rec.Code, rec.Body.String())
	}
	if resp.Status != "ok" {
		t.Fatalf("nil cfg 应回落到默认配置并完成探测，got %q message=%q", resp.Status, resp.Message)
	}
}
