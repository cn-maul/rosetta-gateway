package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeUpstream 复刻真实上游最关键的两种行为：
//   - 收到 @thinking:off 时真的不思考（无 reasoning_content）
//   - 其余情况思考并返回 reasoning_content
//
// 用它验证 livecheck 自己的判定：不能把「还在思考」误判成通过。
func fakeUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		model, _ := body["model"].(string)
		w.Header().Set("Content-Type", "application/json")

		if strings.Contains(model, "@thinking:off") {
			_, _ = w.Write([]byte(`{"id":"c1","object":"chat.completion","model":"m",
				"choices":[{"index":0,"message":{"role":"assistant","content":"210"},"finish_reason":"stop"}],
				"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3,
				         "completion_tokens_details":{"reasoning_tokens":0}}}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"c1","object":"chat.completion","model":"m",
			"choices":[{"index":0,"message":{"role":"assistant","content":"210","reasoning_content":"1.5h + 2h = 3.5h = 210min"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":1,"completion_tokens":20,"total_tokens":21,
			         "completion_tokens_details":{"reasoning_tokens":18}}}`))
	}))
}

func TestLivecheckDetectsOffAndOn(t *testing.T) {
	srv := fakeUpstream(t)
	defer srv.Close()

	c := config{base: srv.URL, key: "k", model: "demo", protocol: "chat"}
	var results []result
	for _, tc := range buildCases("demo", false) {
		results = append(results, runOne(c, tc))
	}

	if !allOK(results) {
		for _, r := range results {
			t.Logf("%s: status=%d reasoning=%v err=%s", r.tc.name, r.status, r.reasoning, r.err)
		}
		t.Fatal("expected all cases to pass against the fake upstream")
	}
}

// 上游「假装接受、实际没关」时，livecheck 必须报失败 —— 这是它存在的意义。
func TestLivecheckCatchesIgnoredOffSwitch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c1","object":"chat.completion","model":"m",
			"choices":[{"index":0,"message":{"role":"assistant","content":"210","reasoning_content":"still thinking"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":1,"completion_tokens":20,"total_tokens":21}}`))
	}))
	defer srv.Close()

	c := config{base: srv.URL, key: "k", model: "demo", protocol: "chat"}
	var results []result
	for _, tc := range buildCases("demo", false) {
		results = append(results, runOne(c, tc))
	}
	if allOK(results) {
		t.Fatal("ignored off-switch must be reported as a failure")
	}
}

// 流式用例拿到整包 JSON 时必须报错：那说明流式分支根本没被走到。
func TestLivecheckRejectsNonSSEForStreamCase(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c1","object":"chat.completion","model":"m",
			"choices":[{"index":0,"message":{"role":"assistant","content":"210"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`))
	}))
	defer srv.Close()

	c := config{base: srv.URL, key: "k", model: "demo", protocol: "chat"}
	r := runOne(c, testCase{name: "流式", model: "demo@thinking:off", body: map[string]any{"stream": true}, wantOff: true})
	if r.err == "" {
		t.Fatal("non-SSE body for a stream case must be reported as an error")
	}
}

func TestBuildCasesIncludesSuffixVariants(t *testing.T) {
	cases := buildCases("m", true)
	joined := ""
	for _, c := range cases {
		joined += c.model + "|"
	}
	for _, want := range []string{"m@thinking:off", "m@effort:low", "m@effort:high"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing case %q in %v", want, joined)
		}
	}
	if len(cases) != 6 {
		t.Fatalf("expected 6 cases (baseline + 3 effort + dialect + stream), got %d", len(cases))
	}
}
