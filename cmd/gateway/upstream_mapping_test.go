package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// recordingUpstream 把收到的请求体回显出来，供断言「网关到底往上��发了什么」。
func recordingUpstream(t *testing.T, ch chan<- map[string]any, stream bool) *httptest.Server {
	t.Helper()
	if stream {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw, _ := io.ReadAll(r.Body)
			var m map[string]any
			_ = json.Unmarshal(raw, &m)
			ch <- m
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			fl, _ := w.(http.Flusher)
			for _, c := range []string{
				`{"id":"c1","object":"chat.completion.chunk","created":0,"model":"m","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`,
				`{"id":"c1","object":"chat.completion.chunk","created":0,"model":"m","choices":[{"index":0,"delta":{"content":"pong"},"finish_reason":null}]}`,
				`{"id":"c1","object":"chat.completion.chunk","created":0,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
			} {
				_, _ = w.Write([]byte("data: " + c + "\n\n"))
				if fl != nil {
					fl.Flush()
				}
			}
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
			if fl != nil {
				fl.Flush()
			}
		}))
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(raw, &m)
		ch <- m
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c","object":"chat.completion","created":0,"model":"m",
			"choices":[{"index":0,"message":{"role":"assistant","content":"pong"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}}`))
	}))
}

func postRaw(h http.HandlerFunc, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testAccessKey)
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec
}

// 网关必须把**公共别名换成上游 model_id** 再发出去。buildHarness 里路由
// flash → 上游 "p1-model"，两者刻意不同。
//
// 这条断言是必需的：曾经main.go 在外层循环里造好 request 覆写了 Model 与
// Extra 却不传给 attempt*，而 attempt* 内部又自己调了一遍buildRosetta() ——
// 结果所有别名映射与协议私有字段的透传全部失效，而当时的 fake 上游不断言
// 收到的 model，测试全绿放行。
func TestUpstreamMapping_ReplacesPublicAliasWithModelID(t *testing.T) {
	ch := make(chan map[string]any, 4)
	up := recordingUpstream(t, ch, false)
	defer up.Close()

	h, _ := buildHarness(t, []struct {
		slug, url string
		fail      bool
	}{{"p1", up.URL, false}}, false)

	rec := postRaw(h, `{"model":"flash","messages":[{"role":"user","content":"hi"}],"stream":false}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := (<-ch)["model"]; got != "p1-model" {
		t.Fatalf("上游收到 model=%v，期望 p1-model（公共别名 flash 未被映射）", got)
	}
}

// 本版本新增的透传字段必须真的到达上游。此前它们被applyUpstreamExtras
// 写进一个从未被使用的请求对象，等于特性没接上线。
func TestUpstreamPassthrough_ExtraFieldsReachUpstream(t *testing.T) {
	ch := make(chan map[string]any, 4)
	up := recordingUpstream(t, ch, false)
	defer up.Close()

	h, _ := buildHarness(t, []struct {
		slug, url string
		fail      bool
	}{{"p1", up.URL, false}}, false)

	rec := postRaw(h, `{"model":"flash","messages":[{"role":"user","content":"hi"}],"stream":false,
		"response_format":{"type":"json_object"},"seed":42,
		"parallel_tool_calls":false,"user":"u-123","frequency_penalty":0.5}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	m := <-ch
	for _, k := range []string{"response_format", "seed", "parallel_tool_calls", "user", "frequency_penalty"} {
		if m[k] == nil {
			t.Errorf("字段 %s 未透传给上游，收到=%v", k, m)
		}
	}
}

// 流式路径走的是另一个 attempt*，必须同样映射别名 —— 修一处漏一处是最容易
// 发生的回归，所以两条路径都要钉住。
func TestUpstreamMapping_StreamAlsoReplacesPublicAlias(t *testing.T) {
	ch := make(chan map[string]any, 4)
	up := recordingUpstream(t, ch, true)
	defer up.Close()

	h, _ := buildHarness(t, []struct {
		slug, url string
		fail      bool
	}{{"p1", up.URL, false}}, false)

	rec := postRaw(h, `{"model":"flash","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := (<-ch)["model"]; got != "p1-model" {
		t.Fatalf("流式上游收到 model=%v，期望 p1-model", got)
	}
}

// 每个 attempt 必须造自己的请求实例：链上 A 目标（openai-chat）与 B 目标
// （anthropic）的 Extra 形状不同，复用同一实例会把 A 的字段泄漏给 B。
func TestUpstreamPassthrough_NoExtraLeakAcrossFailoverTargets(t *testing.T) {
	chB := make(chan map[string]any, 8)
	chA := make(chan map[string]any, 8)
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(raw, &m)
		chA <- m
		w.WriteHeader(http.StatusInternalServerError)
	}))
	good := recordingUpstream(t, chB, false)
	defer bad.Close()
	defer good.Close()

	h, _ := buildHarness(t, []struct {
		slug, url string
		fail      bool
	}{{"pA", bad.URL, true}, {"pB", good.URL, false}}, true)

	rec := postRaw(h, `{"model":"flash","messages":[{"role":"user","content":"hi"}],"stream":false,
		"presence_penalty":0.3}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	mA := <-chA
	mB := <-chB
	// A 失败后转B：B 的请求必须是**独立构造**的 —— 用 B 自己的 model_id，
	// 而不是沿用 A 那一轮改过的对象（共用实例会把 A 的 Extra 泄漏给 B）。
	if mB["model"] != "pB-model" {
		t.Errorf("B 目标收到 model=%v，期望 pB-model（A 的请求实例被复用了）", mB["model"])
	}
	if mA["model"] != "pA-model" {
		t.Errorf("A 目标收到 model=%v，期望 pA-model", mA["model"])
	}
	// 两个目标各自的 model 都对，才说明每次 attempt 都重新覆写过Model。
	if mA["model"] == mB["model"] {
		t.Errorf("两个目标收到同一个 model=%v，别名映射没按目标生效", mA["model"])
	}
}
