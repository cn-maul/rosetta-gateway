package dataplane

import (
	"io"
	"log/slog"
	"testing"

	"github.com/cn-maul/rosetta"
)

func newReplayGateHandler() *Handler {
	return &Handler{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

func blockTypes(m rosetta.Message) []rosetta.BlockType {
	out := make([]rosetta.BlockType, 0, len(m.Blocks))
	for _, b := range m.Blocks {
		out = append(out, b.Type)
	}
	return out
}

// chat 目标：v1.3.0 起 BlockThinking 会被编码成 reasoning_content，所以
// **没签名、有可读文本**的 thinking 必须保留 —— 这是 DeepSeek 系多轮会话
// 能续接的前提。签过名的（别的协议族签发）与无文本的仍然剥。
func TestApplyReplayGate_ChatKeepsReplayableThinking(t *testing.T) {
	h := newReplayGateHandler()

	req := &rosetta.ChatRequest{Messages: []rosetta.Message{
		rosetta.User("hi"),
		{Role: rosetta.RoleAssistant, Blocks: []rosetta.Block{
			{Type: rosetta.BlockThinking, Thinking: "need weather"},
			{Type: rosetta.BlockText, Text: "checking"},
			{Type: rosetta.BlockToolCall, ToolCallID: "c1", ToolName: "weather", Arguments: "{}"},
		}},
		rosetta.ToolResult("c1", "", "sunny"),
	}}

	h.applyReplayGate(req, "chat")

	if len(req.Messages) != 3 {
		t.Fatalf("messages = %d, want 3: %+v", len(req.Messages), req.Messages)
	}
	asst := req.Messages[1]
	types := blockTypes(asst)
	if len(types) != 3 || types[0] != rosetta.BlockThinking {
		t.Fatalf("chat 应保留可回放的 thinking，实际 %v", types)
	}
	if asst.Blocks[0].Thinking != "need weather" {
		t.Fatalf("thinking 文本被改：%+v", asst.Blocks[0])
	}
}

// chat 目标：签名 thinking 来自别的协议族，chat 写不出对应语义；无文本的
// 思考块也没有可回放内容。两种都剥，但轮次本身（有正文）要留下。
func TestApplyReplayGate_ChatStripsSignedAndTextlessThinking(t *testing.T) {
	h := newReplayGateHandler()

	req := &rosetta.ChatRequest{Messages: []rosetta.Message{
		{Role: rosetta.RoleAssistant, Blocks: []rosetta.Block{
			{Type: rosetta.BlockThinking, Thinking: "from anthropic", Signature: "sig-1"},
			{Type: rosetta.BlockThinking, Sealed: "sealed-only"},
			{Type: rosetta.BlockRedactedThinking},
			{Type: rosetta.BlockText, Text: "answer"},
		}},
	}}

	h.applyReplayGate(req, "chat")

	if len(req.Messages) != 1 {
		t.Fatalf("turn should survive: %+v", req.Messages)
	}
	types := blockTypes(req.Messages[0])
	if len(types) != 1 || types[0] != rosetta.BlockText {
		t.Fatalf("只剩正文，实际 %v", types)
	}
}

// chat 目标：整轮只有思考块时必须整条删掉 —— 留着会被 SDK 以
// 「assistant turn has only thinking blocks」拒掉整个请求（G6）。
func TestApplyReplayGate_ChatDropsThinkingOnlyTurn(t *testing.T) {
	h := newReplayGateHandler()

	req := &rosetta.ChatRequest{Messages: []rosetta.Message{
		rosetta.User("hi"),
		{Role: rosetta.RoleAssistant, Blocks: []rosetta.Block{
			{Type: rosetta.BlockThinking, Thinking: "thinking but no answer"},
		}},
	}}

	h.applyReplayGate(req, "chat")

	if len(req.Messages) != 1 || req.Messages[0].Role != rosetta.RoleUser {
		t.Fatalf("thinking-only turn must be dropped: %+v", req.Messages)
	}
}

// anthropic 目标：只保留有签名的 thinking（签名是上游校验的唯一凭据）。
func TestApplyReplayGate_AnthropicKeepsOnlySigned(t *testing.T) {
	h := newReplayGateHandler()

	req := &rosetta.ChatRequest{Messages: []rosetta.Message{
		{Role: rosetta.RoleAssistant, Blocks: []rosetta.Block{
			{Type: rosetta.BlockThinking, Thinking: "signed", Signature: "sig-1"},
			{Type: rosetta.BlockThinking, Thinking: "unsigned"},
			{Type: rosetta.BlockRedactedThinking, Signature: "sig-2"},
			{Type: rosetta.BlockText, Text: "answer"},
		}},
	}}

	h.applyReplayGate(req, "anthropic")

	types := blockTypes(req.Messages[0])
	want := []rosetta.BlockType{rosetta.BlockThinking, rosetta.BlockRedactedThinking, rosetta.BlockText}
	if len(types) != len(want) {
		t.Fatalf("blocks = %v, want %v", types, want)
	}
	for i := range want {
		if types[i] != want[i] {
			t.Fatalf("blocks = %v, want %v", types, want)
		}
	}
	if req.Messages[0].Blocks[0].Signature != "sig-1" {
		t.Fatalf("signed thinking 的签名被改：%+v", req.Messages[0].Blocks[0])
	}
}

// response 目标整层不动：外来封由 SDK 的 SealedBy 门闩自理。
func TestApplyReplayGate_ResponsesUntouched(t *testing.T) {
	h := newReplayGateHandler()

	original := []rosetta.Message{
		{Role: rosetta.RoleAssistant, Blocks: []rosetta.Block{
			{Type: rosetta.BlockThinking, Thinking: "x", Sealed: "sealed", SealedBy: "openai-responses"},
			{Type: rosetta.BlockRedactedThinking},
		}},
	}
	req := &rosetta.ChatRequest{Messages: append([]rosetta.Message(nil), original...)}

	h.applyReplayGate(req, "response")

	if len(req.Messages) != 1 || len(req.Messages[0].Blocks) != 2 {
		t.Fatalf("response 目标不该被改：%+v", req.Messages)
	}
}

// 非 assistant 消息（user / tool）永远原样保留。
func TestApplyReplayGate_LeavesNonAssistantUntouched(t *testing.T) {
	h := newReplayGateHandler()

	req := &rosetta.ChatRequest{Messages: []rosetta.Message{
		rosetta.User("hi"),
		rosetta.ToolResult("c1", "", "sunny"),
	}}

	h.applyReplayGate(req, "chat")

	if len(req.Messages) != 2 {
		t.Fatalf("messages = %d, want 2: %+v", len(req.Messages), req.Messages)
	}
}
