package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cn-maul/rosetta-gateway/internal/ingress"
)

// fakeAnthropic 起一个 Anthropic Messages 形状的假上游（SDK 会 POST
// <endpoint>/v1/messages），命中次数记入计数器供断言「未被命中」。
func fakeAnthropic(t *testing.T, hits *atomic.Int64) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","model":"claude",
			"content":[{"type":"text","text":"pong"}],"stop_reason":"end_turn",
			"usage":{"input_tokens":5,"output_tokens":3}}`))
	}))
}

// (a) 链上只有 anthropic 上游 + 带 json_schema 的 Chat 请求 → 400，
// 且文案点明结构化输出 —— 而不是 200 但约束静默丢失（P1-8）。
func TestStructuredOutput_AnthropicOnlyChainRejected(t *testing.T) {
	var hits atomic.Int64
	anth := fakeAnthropic(t, &hits)
	defer anth.Close()

	h := buildHarnessProtocols(t, []harnessChain{{slug: "claude", url: anth.URL, protocol: "anthropic"}}, false, ingress.NewOpenAIChat())

	rec := postRaw(h, `{"model":"flash","messages":[{"role":"user","content":"hi"}],"stream":false,
		"response_format":{"type":"json_schema","json_schema":{"name":"out","strict":true,"schema":{"type":"object"}}}}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "structured output") {
		t.Fatalf("错误文案必须点明结构化输出：%s", rec.Body.String())
	}
	if got := hits.Load(); got != 0 {
		t.Fatalf("anthropic 上游不应被命中，实际 %d 次", got)
	}
}

// (b) 链 [chat(可转移失败), anthropic] + json_schema → 最终错误，
// 而不是「转移到 anthropic 后 200 但约束丢失」。anthropic 假上游必须零命中。
func TestStructuredOutput_FailoverSkipsAnthropicCandidate(t *testing.T) {
	var hits atomic.Int64
	anth := fakeAnthropic(t, &hits)
	defer anth.Close()
	bad := fakeBad()
	defer bad.Close()

	h := buildHarnessProtocols(t, []harnessChain{
		{slug: "openai", url: bad.URL, protocol: "chat"},
		{slug: "claude", url: anth.URL, protocol: "anthropic"},
	}, true, ingress.NewOpenAIChat())

	rec := postRaw(h, `{"model":"flash","messages":[{"role":"user","content":"hi"}],"stream":false,
		"response_format":{"type":"json_schema","json_schema":{"name":"out","schema":{"type":"object"}}}}`)
	if rec.Code == http.StatusOK {
		t.Fatalf("结构化输出请求不得在 anthropic 上游拿到 200（约束会静默丢失）：%s", rec.Body.String())
	}
	if rec.Code < 400 {
		t.Fatalf("want 4xx/5xx, got %d body=%s", rec.Code, rec.Body.String())
	}
	if got := hits.Load(); got != 0 {
		t.Fatalf("anthropic 上游不应被命中，实际 %d 次", got)
	}
	// chat 目标确实被打过（失败是真实上游错误，不是过滤造成的空转）。
	if !strings.Contains(rec.Body.String(), "error") {
		t.Fatalf("应透出上游错误而非其它：%s", rec.Body.String())
	}
}

// 正向对照：openai 系候选不受过滤影响，json_schema 照常转发（约束仍生效）。
// 防止过滤器把「能服务的请求」也拦掉。
func TestStructuredOutput_OpenAIChatTargetStillServes(t *testing.T) {
	ch := make(chan map[string]any, 4)
	up := recordingUpstream(t, ch, false)
	defer up.Close()

	h := buildHarnessProtocols(t, []harnessChain{{slug: "openai", url: up.URL, protocol: "chat"}}, false, ingress.NewOpenAIChat())

	rec := postRaw(h, `{"model":"flash","messages":[{"role":"user","content":"hi"}],"stream":false,
		"response_format":{"type":"json_schema","json_schema":{"name":"out","schema":{"type":"object"}}}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	m := <-ch
	rf, ok := m["response_format"].(map[string]any)
	if !ok || rf["type"] != "json_schema" {
		t.Fatalf("response_format 未透传给 chat 上游：%v", m["response_format"])
	}
}

// Responses 入口同样受硬约束约束：text.format 为 json_schema 时，
// anthropic-only 链 → 400。
func TestStructuredOutput_ResponsesIngressAnthropicChainRejected(t *testing.T) {
	var hits atomic.Int64
	anth := fakeAnthropic(t, &hits)
	defer anth.Close()

	h := buildHarnessProtocols(t, []harnessChain{{slug: "claude", url: anth.URL, protocol: "anthropic"}}, false, ingress.NewResponses())

	body := `{"model":"flash","input":"hi","text":{"format":{"type":"json_schema","name":"out","schema":{"type":"object"}}}}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testAccessKey)
	rec := httptest.NewRecorder()
	h(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "structured output") {
		t.Fatalf("错误文案必须点明结构化输出：%s", rec.Body.String())
	}
	if got := hits.Load(); got != 0 {
		t.Fatalf("anthropic 上游不应被命中，实际 %d 次", got)
	}
}

// Anthropic 入站协议没有结构化输出概念，永不触发过滤：
// anthropic 入口 → anthropic 上游照常 200。
func TestStructuredOutput_AnthropicIngressNotFiltered(t *testing.T) {
	var hits atomic.Int64
	anth := fakeAnthropic(t, &hits)
	defer anth.Close()

	h := buildHarnessProtocols(t, []harnessChain{{slug: "claude", url: anth.URL, protocol: "anthropic"}}, false, ingress.NewAnthropicMessages())

	rec := postMessages(h, `{"model":"flash","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("anthropic 入口不受结构化输出过滤影响，want 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("anthropic 上游应被命中 1 次，实际 %d", got)
	}
	var out struct {
		Usage struct {
			Input  int64 `json:"input_tokens"`
			Output int64 `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, rec.Body.String())
	}
	if out.Usage.Input != 5 || out.Usage.Output != 3 {
		t.Fatalf("usage = %+v, want 5/3", out.Usage)
	}
}
