package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Responses 全链路：reasoning item 的 encrypted_content 必须原样往返。
//
// 这是 responses 相对 chat 的核心增量。修复前两侧都断：
// inwire 把 reasoning item 整个剥除、outwire 流式侧不出 thinking 事件、
// 非流式侧跳过 BlockThinking。客户端第二轮带着 rs_ id 回放时，
// 上游报 "Item with id 'rs_…' not found"，多轮 reasoning 会话断链。
//
// 假上游同时充当「上一轮的响应方」与「这一轮的校验方」：它把收到的
// input 原样记下来，断言第二轮的 input 里带回了它自己发过的 sealed 载荷。
func TestE2E_Responses_ReasoningSealedRoundTrip(t *testing.T) {
	const sealed = "gAAAAA-opaque-blob"
	const reasoningID = "rs_abc123"

	var gotInputs []json.RawMessage
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Input json.RawMessage `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotInputs = append(gotInputs, body.Input)

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"resp_up_1","object":"response","created_at":1,"model":"good-model",
			"status":"completed",
			"output":[
				{"type":"reasoning","id":"` + reasoningID + `",
				 "summary":[{"type":"summary_text","text":"先想一下"}],
				 "encrypted_content":"` + sealed + `"},
				{"type":"message","id":"msg_up_1","status":"completed","role":"assistant",
				 "content":[{"type":"output_text","text":"答案是 42"}]}
			],
			"usage":{"input_tokens":5,"output_tokens":3,"total_tokens":8}
		}`))
	}))
	defer up.Close()

	h := buildHarnessProtocols(t, []harnessChain{
		{slug: "good", url: up.URL, protocol: "openai-responses"},
	}, false, openaiResponsesCodec{})

	// --- 第一轮：拿到 reasoning item ---
	rec := e2ePost(h, "/v1/responses", testAccessKey, `{
		"model":"flash","stream":false,
		"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"1+1=?"}]}]
	}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("第一轮期望 200，实际 %d，body=%s", rec.Code, rec.Body.String())
	}

	var first struct {
		ID     string `json:"id"`
		Status string `json:"status"`
		Output []struct {
			Type             string `json:"type"`
			ID               string `json:"id"`
			EncryptedContent string `json:"encrypted_content"`
			Summary          []struct {
				Text string `json:"text"`
			} `json:"summary"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
		Usage struct {
			TotalTokens int64 `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &first); err != nil {
		t.Fatalf("第一轮响应不是合法 JSON: %v body=%s", err, rec.Body.String())
	}
	if len(first.Output) != 2 {
		t.Fatalf("第一轮 output 数量 = %d，期望 2（reasoning + message）: %+v",
			len(first.Output), first.Output)
	}
	r := first.Output[0]
	if r.Type != "reasoning" || r.EncryptedContent != sealed || r.ID != reasoningID {
		t.Fatalf("reasoning item 未透传: %+v", r)
	}
	if len(r.Summary) != 1 || r.Summary[0].Text != "先想一下" {
		t.Fatalf("reasoning summary 未透传: %+v", r.Summary)
	}
	if first.Usage.TotalTokens != 8 {
		t.Errorf("第一轮 usage.total_tokens=%d，期望 8", first.Usage.TotalTokens)
	}

	// --- 第二轮：客户端回传它拿到的 reasoning item ---
	//
	// 客户端只能用**它看到的** id 与 encrypted_content —— 这正是真实
	// Codex CLI 的行为。网关必须原样搬回上游，否则上游找不到该 item。
	rec2 := e2ePost(h, "/v1/responses", testAccessKey, `{
		"model":"flash","stream":false,
		"input":[
			{"type":"reasoning","id":"`+reasoningID+`",
			 "summary":[{"type":"summary_text","text":"先想一下"}],
			 "encrypted_content":"`+sealed+`"},
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"答案是 42"}]},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"那 2+2 呢"}]}
		]
	}`)
	if rec2.Code != http.StatusOK {
		t.Fatalf("第二轮期望 200，实际 %d，body=%s", rec2.Code, rec2.Body.String())
	}

	// --- 上游第二轮收到的 input 必须含 sealed 载荷 ---
	if len(gotInputs) != 2 {
		t.Fatalf("上游应被调用 2 次，实际 %d 次", len(gotInputs))
	}
	second := string(gotInputs[1])
	if !strings.Contains(second, sealed) {
		t.Errorf("上游第二轮未收到 encrypted_content，多轮 reasoning 会话会断链:\n%s", second)
	}
	if !strings.Contains(second, reasoningID) {
		t.Errorf("上游第二轮未收到 reasoning item id:\n%s", second)
	}
}

// Responses 流式侧：thinking 增量必须出事件，且 sealed 载荷随
// output_item.done 送达。客户端若只消费 response.completed，那里的
// output 也要含 reasoning item。
func TestE2E_Responses_ReasoningSealedStream(t *testing.T) {
	const sealed = "gAAAAA-stream-blob"

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl, _ := w.(http.Flusher)
		events := []string{
			`{"type":"response.created","response":{"id":"resp_up_s","object":"response","created_at":1,"model":"good-model","status":"in_progress","output":[]}}`,
			`{"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs_stream1"}}`,
			`{"type":"response.reasoning_summary_text.delta","item_id":"rs_stream1","output_index":0,"summary_index":0,"delta":"先想一下"}`,
			`{"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning","id":"rs_stream1","summary":[{"type":"summary_text","text":"先想一下"}],"encrypted_content":"` + sealed + `"}}`,
			`{"type":"response.output_item.added","output_index":1,"item":{"type":"message","id":"msg_s","status":"in_progress","role":"assistant","content":[]}}`,
			`{"type":"response.content_part.added","item_id":"msg_s","output_index":1,"content_index":0,"part":{"type":"output_text","text":"","annotations":[]}}`,
			`{"type":"response.output_text.delta","item_id":"msg_s","output_index":1,"content_index":0,"delta":"答案是 42"}`,
			`{"type":"response.content_part.done","item_id":"msg_s","output_index":1,"content_index":0,"part":{"type":"output_text","text":"答案是 42","annotations":[]}}`,
			`{"type":"response.output_item.done","output_index":1,"item":{"type":"message","id":"msg_s","status":"completed","role":"assistant","content":[{"type":"output_text","text":"答案是 42"}]}}`,
			`{"type":"response.completed","response":{"id":"resp_up_s","object":"response","created_at":1,"model":"good-model","status":"completed","output":[],"usage":{"input_tokens":5,"output_tokens":3,"total_tokens":8}}}`,
		}
		for _, e := range events {
			_, _ = w.Write([]byte("data: " + e + "\n\n"))
			if fl != nil {
				fl.Flush()
			}
		}
	}))
	defer up.Close()

	h := buildHarnessProtocols(t, []harnessChain{
		{slug: "good", url: up.URL, protocol: "openai-responses"},
	}, false, openaiResponsesCodec{})

	rec := e2ePost(h, "/v1/responses", testAccessKey, `{
		"model":"flash","stream":true,
		"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"1+1=?"}]}]
	}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("流式期望 200，实际 %d，body=%s", rec.Code, rec.Body.String())
	}

	raw := rec.Body.String()
	if !strings.Contains(raw, sealed) {
		t.Errorf("流式响应未含 encrypted_content，客户端拿不到续接状态:\n%s", raw)
	}
	if !strings.Contains(raw, "response.reasoning_summary_text.delta") {
		t.Errorf("流式响应未含 reasoning summary delta:\n%s", raw)
	}

	// response.completed 内嵌的 output 也要有 reasoning item。
	var done struct {
		Response struct {
			Output []struct {
				Type             string `json:"type"`
				EncryptedContent string `json:"encrypted_content"`
			} `json:"output"`
		} `json:"response"`
	}
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimRight(line, "\r")
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if !strings.Contains(payload, "response.completed") {
			continue
		}
		if err := json.Unmarshal([]byte(payload), &done); err != nil {
			t.Fatalf("解析 completed 失败: %v", err)
		}
	}
	var found bool
	for _, item := range done.Response.Output {
		if item.Type == "reasoning" && item.EncryptedContent == sealed {
			found = true
		}
	}
	if !found {
		t.Errorf("response.completed 的 output 未含 sealed reasoning item: %+v", done.Response.Output)
	}
}
