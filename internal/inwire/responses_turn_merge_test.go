package inwire

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cn-maul/rosetta"
)

// Responses 的 input 把一轮 assistant 拆成多个 output item（reasoning /
// message / function_call 各一条）。rosetta 的语义是「一个 Message = 一轮」，
// 所以入站必须把它们合并成**一条** RoleAssistant 多 Block。
//
// 这一层修好之后，chat / anthropic / responses 三个出站协议同时正确；
// 修复前是「一个 item 一条 Message」，再靠出站层重写编码器合并回来
// （responses_chat_replay.go，已删）。
func TestResponsesToRosetta_MergesAssistantTurn(t *testing.T) {
	got := decodeResponses(t, `{
		"model":"m",
		"input":[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"weather?"}]},
			{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"need weather"}]},
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"checking"}]},
			{"type":"function_call","call_id":"call_1","name":"weather","arguments":"{\"city\":\"Paris\"}"},
			{"type":"function_call_output","call_id":"call_1","output":"sunny"},
			{"type":"reasoning","id":"rs_2","summary":[{"type":"summary_text","text":"again"}]},
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}
		]
	}`)

	req := got.ToRosetta()
	if len(req.Messages) != 4 {
		t.Fatalf("message count = %d, want 4: %+v", len(req.Messages), req.Messages)
	}

	// 第一轮：reasoning + 正文 + tool_call 同一条。
	first := req.Messages[1]
	if first.Role != rosetta.RoleAssistant || len(first.Blocks) != 3 {
		t.Fatalf("first assistant turn: %+v", first)
	}
	if first.Blocks[0].Type != rosetta.BlockThinking || first.Blocks[0].Thinking != "need weather" {
		t.Fatalf("thinking block: %+v", first.Blocks[0])
	}
	if first.Blocks[1].Type != rosetta.BlockText || first.Blocks[1].Text != "checking" {
		t.Fatalf("text block: %+v", first.Blocks[1])
	}
	if first.Blocks[2].Type != rosetta.BlockToolCall || first.Blocks[2].ToolCallID != "call_1" {
		t.Fatalf("tool_call block: %+v", first.Blocks[2])
	}

	// 工具结果断开本轮。
	if req.Messages[2].Role != rosetta.RoleTool || req.Messages[2].Blocks[0].Content != "sunny" {
		t.Fatalf("tool result: %+v", req.Messages[2])
	}

	// 工具结果之后的 reasoning + 正文属于**下一轮**，不能被并进上一轮。
	second := req.Messages[3]
	if second.Role != rosetta.RoleAssistant || len(second.Blocks) != 2 {
		t.Fatalf("second assistant turn: %+v", second)
	}
	if second.Blocks[0].Thinking != "again" || second.Blocks[1].Text != "done" {
		t.Fatalf("second turn blocks: %+v", second.Blocks)
	}
}

// user / system 是轮次边界：它们前后的 assistant 段不能合并。
func TestResponsesToRosetta_UserBoundaryFlushes(t *testing.T) {
	got := decodeResponses(t, `{"model":"m","input":[
		{"type":"message","role":"assistant","content":[{"type":"output_text","text":"a1"}]},
		{"type":"message","role":"user","content":[{"type":"input_text","text":"u1"}]},
		{"type":"message","role":"assistant","content":[{"type":"output_text","text":"a2"}]}
	]}`)

	req := got.ToRosetta()
	if len(req.Messages) != 3 {
		t.Fatalf("message count = %d, want 3: %+v", len(req.Messages), req.Messages)
	}
	if req.Messages[0].Role != rosetta.RoleAssistant || req.Messages[0].Blocks[0].Text != "a1" {
		t.Fatalf("first assistant: %+v", req.Messages[0])
	}
	if req.Messages[1].Role != rosetta.RoleUser {
		t.Fatalf("user boundary: %+v", req.Messages[1])
	}
	if req.Messages[2].Role != rosetta.RoleAssistant || req.Messages[2].Blocks[0].Text != "a2" {
		t.Fatalf("second assistant: %+v", req.Messages[2])
	}
}

// 出站验证（chat 上游）：合并好的一轮经 SDK 编码器出来，reasoning_content、
// content、tool_calls 必须落在**同一条** assistant 消息上，且网关不装任何
// Extra 覆盖开关 —— 全部靠入站建模正确。
func TestResponsesToChat_OutboundMergedTurn(t *testing.T) {
	var captured []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"c1","object":"chat.completion","created":1,"model":"m",
			"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}))
	defer srv.Close()

	req := decodeResponses(t, `{
		"model":"deepseek-chat",
		"instructions":"be brief",
		"input":[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"weather?"}]},
			{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"need weather"}]},
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"checking"}]},
			{"type":"function_call","call_id":"call_1","name":"weather","arguments":"{\"city\":\"Paris\"}"},
			{"type":"function_call_output","call_id":"call_1","output":"sunny"}
		]
	}`).ToRosetta()

	client, err := rosetta.NewClient(
		rosetta.WithProtocol(rosetta.ProtoOpenAIChat),
		rosetta.WithEndpoint(srv.URL),
		rosetta.WithAPIKey("k"),
		// 与 upstream.buildClient 一致：不装 Extra 覆盖开关。
	)
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	if _, err := client.Chat(context.Background(), req); err != nil {
		t.Fatalf("chat: %v", err)
	}

	var body struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(captured, &body); err != nil {
		t.Fatalf("decode body: %v (%s)", err, captured)
	}
	if len(body.Messages) != 4 {
		t.Fatalf("outbound messages = %d, want 4 (system/user/assistant/tool): %s", len(body.Messages), captured)
	}
	assistant := body.Messages[2]
	if assistant["role"] != "assistant" {
		t.Fatalf("messages[2] should be assistant: %s", captured)
	}
	if assistant["reasoning_content"] != "need weather" {
		t.Fatalf("reasoning_content 未回放：%s", captured)
	}
	if assistant["content"] != "checking" {
		t.Fatalf("正文与思考不在同一条消息：%s", captured)
	}
	calls, ok := assistant["tool_calls"].([]any)
	if !ok || len(calls) != 1 {
		t.Fatalf("tool_calls 未落在同一条 assistant 消息：%s", captured)
	}
	if tc, _ := calls[0].(map[string]any); tc["id"] != "call_1" {
		t.Fatalf("tool_call id: %s", captured)
	}
	if !strings.Contains(string(captured), `"role":"tool"`) {
		t.Fatalf("tool 结果消息缺失：%s", captured)
	}
}

// 出站验证（responses 上游）：合并成一条 Message 多 Block 之后，SDK 的
// Responses 编码器仍要按 Block 顺序拆回 reasoning / message / function_call
// 三个 item —— 合并不能把 item 结构吃掉。
//
// 这里不校验响应体解码（只要出站 payload），所以 Chat 的返回错误被忽略。
func TestResponsesToResponses_OutboundSplitsBlocksBackToItems(t *testing.T) {
	var captured []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"resp_1","object":"response","model":"m","status":"completed","output":[]}`)
	}))
	defer srv.Close()

	req := decodeResponses(t, `{
		"model":"m",
		"input":[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"weather?"}]},
			{"type":"reasoning","id":"rs_1","encrypted_content":"sealed-1",
			 "summary":[{"type":"summary_text","text":"need weather"}]},
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"checking"}]},
			{"type":"function_call","call_id":"call_1","name":"weather","arguments":"{\"city\":\"Paris\"}"},
			{"type":"function_call_output","call_id":"call_1","output":"sunny"}
		]
	}`).ToRosetta()

	client, err := rosetta.NewClient(
		rosetta.WithProtocol(rosetta.ProtoOpenAIResponses),
		rosetta.WithEndpoint(srv.URL),
		rosetta.WithAPIKey("k"),
	)
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	_, _ = client.Chat(context.Background(), req) // 只看出站 payload

	var body struct {
		Input []map[string]any `json:"input"`
	}
	if err := json.Unmarshal(captured, &body); err != nil {
		t.Fatalf("decode body: %v (%s)", err, captured)
	}

	var kinds []string
	for _, it := range body.Input {
		kind, _ := it["type"].(string)
		if kind == "message" {
			role, _ := it["role"].(string)
			kind += ":" + role
		}
		kinds = append(kinds, kind)
	}
	want := []string{
		"message:user", "reasoning", "message:assistant",
		"function_call", "function_call_output",
	}
	if strings.Join(kinds, ",") != strings.Join(want, ",") {
		t.Fatalf("item 顺序/形状 = %v, want %v\n%s", kinds, want, captured)
	}
	reasoning := body.Input[1]
	if reasoning["encrypted_content"] != "sealed-1" || reasoning["id"] != "rs_1" {
		t.Fatalf("sealed reasoning item 被合并弄丢：%s", captured)
	}
}
