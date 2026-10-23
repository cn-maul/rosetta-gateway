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

// Responses → chat 上游：reasoning item 的 summary 必须回填成
// assistant 消息上的 reasoning_content，且同一轮的正文与 tool_calls
// 要合并在同一条消息里（DeepSeek 的 thinking 模式靠这个续接）。
func TestResponsesChatReplay_ReasoningContent(t *testing.T) {
	got := decodeResponses(t, `{
		"model":"deepseek",
		"instructions":"be brief",
		"input":[
			{"type":"message","role":"user","content":[
				{"type":"input_text","text":"weather?"},
				{"type":"input_image","image_url":"https://x/y.png"}
			]},
			{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"need weather"}]},
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"checking"}]},
			{"type":"function_call","call_id":"call_1","name":"weather","arguments":"{\"city\":\"Paris\"}"},
			{"type":"function_call_output","call_id":"call_1","output":"sunny"}
		]
	}`)

	req := got.ToRosetta()
	got.ApplyUpstreamExtras(req, "chat")

	raw, ok := req.Extra["messages"]
	if !ok {
		t.Fatalf("messages override missing: %+v", req.Extra)
	}
	msgs, ok := raw.([]map[string]any)
	if !ok {
		t.Fatalf("messages override type = %T", raw)
	}
	if len(msgs) != 4 {
		t.Fatalf("message count = %d, want 4: %+v", len(msgs), msgs)
	}
	if msgs[0]["role"] != "system" || msgs[0]["content"] != "be brief" {
		t.Fatalf("system message: %+v", msgs[0])
	}
	user, ok := msgs[1]["content"].([]map[string]any)
	if !ok || len(user) != 2 || user[1]["type"] != "image_url" {
		t.Fatalf("multimodal user message: %+v", msgs[1])
	}
	assistant := msgs[2]
	if assistant["role"] != "assistant" || assistant["content"] != "checking" {
		t.Fatalf("assistant message: %+v", assistant)
	}
	if assistant["reasoning_content"] != "need weather" {
		t.Fatalf("reasoning_content = %v, want need weather", assistant["reasoning_content"])
	}
	calls, ok := assistant["tool_calls"].([]map[string]any)
	if !ok || len(calls) != 1 || calls[0]["id"] != "call_1" {
		t.Fatalf("tool_calls: %+v", assistant["tool_calls"])
	}
	if msgs[3]["role"] != "tool" || msgs[3]["tool_call_id"] != "call_1" || msgs[3]["content"] != "sunny" {
		t.Fatalf("tool message: %+v", msgs[3])
	}
}

// 即使没有 reasoning，多 item 的 assistant 轮也必须合并成一条消息。
//
// SDK 的 chat 编码器逐条输出 assistant 消息，Responses 的
// message + function_call 于是变成两条连续的 assistant；DeepSeek 系上游
// 会判定工具调用记录不配对并返回 400。合并与 reasoning 无关。
func TestResponsesChatReplay_MergesToolTurnWithoutReasoning(t *testing.T) {
	got := decodeResponses(t, `{
		"model":"m",
		"input":[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"weather?"}]},
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"checking"}]},
			{"type":"function_call","call_id":"call_1","name":"weather","arguments":"{\"city\":\"Paris\"}"},
			{"type":"function_call_output","call_id":"call_1","output":"sunny"}
		]
	}`)
	req := got.ToRosetta()
	got.ApplyUpstreamExtras(req, "chat")

	raw, ok := req.Extra["messages"]
	if !ok {
		t.Fatalf("messages override missing: %+v", req.Extra)
	}
	msgs, ok := raw.([]map[string]any)
	if !ok {
		t.Fatalf("messages override type = %T", raw)
	}
	if len(msgs) != 3 {
		t.Fatalf("message count = %d, want 3: %+v", len(msgs), msgs)
	}
	assistant := msgs[1]
	if assistant["role"] != "assistant" || assistant["content"] != "checking" {
		t.Fatalf("assistant message: %+v", assistant)
	}
	calls, ok := assistant["tool_calls"].([]map[string]any)
	if !ok || len(calls) != 1 || calls[0]["id"] != "call_1" {
		t.Fatalf("tool_calls must live on the same assistant turn: %+v", assistant)
	}
	if _, has := assistant["reasoning_content"]; has {
		t.Fatalf("no reasoning item => no reasoning_content: %+v", assistant)
	}
	if msgs[2]["role"] != "tool" || msgs[2]["tool_call_id"] != "call_1" {
		t.Fatalf("tool message: %+v", msgs[2])
	}
}

// 纯文本请求也必须能走合并编码器（而不是只在有工具时特判）。
func TestResponsesChatReplay_SimpleText(t *testing.T) {
	got := decodeResponses(t, `{"model":"m","input":"hi"}`)
	req := got.ToRosetta()
	got.ApplyUpstreamExtras(req, "chat")
	msgs, ok := req.Extra["messages"].([]map[string]any)
	if !ok || len(msgs) != 1 || msgs[0]["role"] != "user" || msgs[0]["content"] != "hi" {
		t.Fatalf("simple text messages: %+v", req.Extra["messages"])
	}
}

// anthropic 上游拿不到 signature，不能伪造 thinking 块，因此不接管 messages。
func TestResponsesChatReplay_AnthropicUntouched(t *testing.T) {
	got := decodeResponses(t, `{"model":"m","input":[
		{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"think"}]},
		{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}
	]}`)
	req := got.ToRosetta()
	got.ApplyUpstreamExtras(req, "anthropic")
	if _, ok := req.Extra["messages"]; ok {
		t.Fatalf("anthropic must not receive chat messages override: %+v", req.Extra)
	}
}

// 只有思考、没有正文/工具调用的一轮不产出空 assistant 消息。
func TestResponsesChatReplay_SkipsThinkingOnlyTurn(t *testing.T) {
	got := decodeResponses(t, `{"model":"m","input":[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},
		{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"think"}]}
	]}`)
	req := got.ToRosetta()
	got.ApplyUpstreamExtras(req, "chat")
	msgs := req.Extra["messages"].([]map[string]any)
	if len(msgs) != 1 || msgs[0]["role"] != "user" {
		t.Fatalf("thinking-only turn must be skipped: %+v", msgs)
	}
}

// 出站验证：走真实 SDK 编码 + Extra 覆盖，确认上游收到的 body 里
// assistant 消息带 reasoning_content，且 messages 没有被 SDK 再追加一遍。
func TestResponsesChatReplay_OutboundPayload(t *testing.T) {
	var captured []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"id":"c1","object":"chat.completion","created":1,"model":"m",
			"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}
		}`)
	}))
	defer srv.Close()

	got := decodeResponses(t, `{
		"model":"deepseek",
		"instructions":"be brief",
		"input":[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"weather?"}]},
			{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"need weather"}]},
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"checking"}]},
			{"type":"function_call","call_id":"call_1","name":"weather","arguments":"{\"city\":\"Paris\"}"},
			{"type":"function_call_output","call_id":"call_1","output":"sunny"}
		]
	}`)
	req := got.ToRosetta()
	got.ApplyUpstreamExtras(req, "chat")
	req.Model = "deepseek-chat"
	// 生产顺序：ApplyUpstreamExtras 先拿走 messages，再由 applyReplayGate
	// 从 rosetta 消息里剥掉 chat 协议回放不了的 thinking 块。这里复刻后者，
	// 否则 SDK 的 validate 会先以「assistant 只有 thinking 块」拒绝请求。
	req.Messages = stripThinkingForChat(req.Messages)

	client, err := rosetta.NewClient(
		rosetta.WithProtocol(rosetta.ProtoOpenAIChat),
		rosetta.WithEndpoint(srv.URL),
		rosetta.WithAPIKey("k"),
		rosetta.WithExtraOverrides(true),
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
		t.Fatalf("outbound messages = %d, want 4: %s", len(body.Messages), captured)
	}
	if len(body.Messages) > 0 && body.Messages[0]["role"] != "system" {
		t.Fatalf("missing system message: %s", captured)
	}
	assistant := body.Messages[2]
	if assistant["reasoning_content"] != "need weather" {
		t.Fatalf("outbound reasoning_content missing: %s", captured)
	}
	if !strings.Contains(string(captured), `"tool_calls"`) {
		t.Fatalf("outbound tool_calls missing: %s", captured)
	}
}

func stripThinkingForChat(in []rosetta.Message) []rosetta.Message {
	out := make([]rosetta.Message, 0, len(in))
	for _, m := range in {
		if m.Role != rosetta.RoleAssistant {
			out = append(out, m)
			continue
		}
		blocks := make([]rosetta.Block, 0, len(m.Blocks))
		for _, b := range m.Blocks {
			if b.Type == rosetta.BlockThinking || b.Type == rosetta.BlockRedactedThinking {
				continue
			}
			blocks = append(blocks, b)
		}
		if len(blocks) == 0 {
			continue
		}
		m.Blocks = blocks
		out = append(out, m)
	}
	return out
}
