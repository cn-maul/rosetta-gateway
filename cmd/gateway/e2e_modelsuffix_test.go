package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cn-maul/rosetta-gateway/internal/ingress"
)

// upstreamCapture 记录上游真正收到的 body，用来断言网关发出去的字段。
func suffixHarness(t *testing.T) (http.HandlerFunc, *map[string]any) {
	t.Helper()
	got := map[string]any{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		for k, v := range body {
			got[k] = v
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c1","object":"chat.completion","created":0,
			"model":"up-model",
			"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	t.Cleanup(srv.Close)

	h, _, _ := buildHarnessFull(t, []harnessChain{{slug: "ds", url: srv.URL}}, false, ingress.NewOpenAIChat())
	return h, &got
}

func postChatBody(h http.HandlerFunc, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testAccessKey)
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec
}

// 带后缀的模型名必须仍然命中同名路由 —— 后缀是在解码阶段剥掉的，
// 若漏了这一步，客户端会拿到 404 model_not_found。
func TestE2E_ModelSuffixStillRoutes(t *testing.T) {
	h, got := suffixHarness(t)
	rec := postChatBody(h, `{"model":"flash@thinking:off","messages":[{"role":"user","content":"hi"}]}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	// 上游看到的是剥掉后缀的真实模型名。
	if (*got)["model"] != "ds-model" {
		t.Fatalf("upstream model = %v", (*got)["model"])
	}
}

// 没有配置方言时（这里推不出厂商），后缀仍要生效：
// @thinking:off 走兜底的 reasoning_effort=none，而不是静默失败。
func TestE2E_ModelSuffixThinkingOffReachesUpstream(t *testing.T) {
	h, got := suffixHarness(t)
	rec := postChatBody(h, `{"model":"flash@thinking:off","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if (*got)["reasoning_effort"] != "none" {
		t.Fatalf("reasoning_effort = %v, want none (body=%v)", (*got)["reasoning_effort"], *got)
	}
}

// 后缀非法时必须 400，而不是把 @... 原样当模型名发出去。
func TestE2E_ModelSuffixInvalidIsRejected(t *testing.T) {
	h, _ := suffixHarness(t)
	rec := postChatBody(h, `{"model":"flash@thinking:maybe","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400: %s", rec.Code, rec.Body.String())
	}
}
