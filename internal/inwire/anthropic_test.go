package inwire

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cn-maul/rosetta"
)

func decodeAnthropic(t *testing.T, body string) *AnthropicMessagesRequest {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	got, err := DecodeAnthropicMessagesRequest(req, 1<<20)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	return got
}

// 全特性解码：system 块、工具、thinking 回放、tool_result 拆分、图像 data URI。
func TestDecodeAnthropicMessages_Full(t *testing.T) {
	got := decodeAnthropic(t, `{
		"model":"claude-sonnet-4-5",
		"max_tokens":2048,
		"temperature":0.5,
		"top_k":40,
		"stop_sequences":["END"],
		"stream":true,
		"system":[{"type":"text","text":"你是网关","cache_control":{"type":"ephemeral"}}],
		"tools":[{"name":"weather","description":"查天气","input_schema":{"type":"object"},"cache_control":{"type":"ephemeral"}}],
		"thinking":{"type":"enabled","budget_tokens":1024},
		"messages":[
			{"role":"user","content":"你好"},
			{"role":"assistant","content":[
				{"type":"thinking","thinking":"想一想","signature":"sig=="},
				{"type":"text","text":"我调个工具"},
				{"type":"tool_use","id":"t1","name":"weather","input":{"city":"北京"}}
			]},
			{"role":"user","content":[
				{"type":"tool_result","tool_use_id":"t1","content":"晴 25 度"},
				{"type":"text","text":"总结一下"},
				{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aGVsbG8="}}
			]}
		]
	}`)

	req := got.ToRosetta()

	if req.Model != "claude-sonnet-4-5" || req.MaxOutputTokens != 2048 {
		t.Fatalf("basic fields: %+v", req)
	}
	if req.Temperature == nil || *req.Temperature != 0.5 || len(req.StopSequences) != 1 {
		t.Fatalf("sampling fields: %+v", req)
	}
	if req.Thinking == nil || req.Thinking.BudgetTokens != 1024 {
		t.Fatalf("thinking config: %+v", req.Thinking)
	}
	if len(req.Tools) != 1 || req.Tools[0].Name != "weather" || string(req.Tools[0].Parameters) != `{"type":"object"}` {
		t.Fatalf("tools: %+v", req.Tools)
	}
	if req.Tools[0].CacheControl == nil {
		t.Fatalf("tool cache_control lost")
	}

	// system 块数组 → RoleSystem 消息（带缓存断点）。
	if len(req.Messages) != 5 {
		t.Fatalf("message count = %d, want 5 (system, user, assistant, tool, user)", len(req.Messages))
	}
	sys := req.Messages[0]
	if sys.Role != rosetta.RoleSystem || sys.Blocks[0].CacheControl == nil {
		t.Fatalf("system message/cache_control: %+v", sys)
	}

	user0 := req.Messages[1]
	if user0.Role != rosetta.RoleUser || user0.Blocks[0].Text != "你好" {
		t.Fatalf("first user message: %+v", user0)
	}

	asst := req.Messages[2]
	if asst.Role != rosetta.RoleAssistant || len(asst.Blocks) != 3 {
		t.Fatalf("assistant message: %+v", asst)
	}
	if asst.Blocks[0].Type != rosetta.BlockThinking || asst.Blocks[0].Signature != "sig==" {
		t.Fatalf("thinking replay lost: %+v", asst.Blocks[0])
	}
	if asst.Blocks[2].Type != rosetta.BlockToolCall || asst.Blocks[2].Arguments != `{"city":"北京"}` {
		t.Fatalf("tool_call: %+v", asst.Blocks[2])
	}

	// user 轮里的 tool_result 拆成 RoleTool；剩余文本+图组成 RoleUser。
	toolMsg := req.Messages[3]
	if toolMsg.Role != rosetta.RoleTool || toolMsg.Blocks[0].ToolCallID != "t1" || toolMsg.Blocks[0].Content != "晴 25 度" {
		t.Fatalf("tool_result message: %+v", toolMsg)
	}
	finalUser := req.Messages[4]
	if finalUser.Role != rosetta.RoleUser || len(finalUser.Blocks) != 2 ||
		finalUser.Blocks[1].Type != rosetta.BlockImage ||
		finalUser.Blocks[1].ImageURL != "data:image/png;base64,aGVsbG8=" {
		t.Fatalf("final user message: %+v", finalUser)
	}
}

// max_tokens 是 Anthropic 协议的必填项。
func TestDecodeAnthropicMessages_MaxTokensRequired(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	if _, err := DecodeAnthropicMessagesRequest(req, 1<<20); err == nil {
		t.Fatalf("missing max_tokens should fail")
	}
}

// tool_choice 类型校验与按上游协议的翻译/透传。
func TestDecodeAnthropicMessages_ToolChoice(t *testing.T) {
	good := decodeAnthropic(t, `{"model":"m","max_tokens":8,
		"tool_choice":{"type":"tool","name":"weather"},
		"messages":[{"role":"user","content":"hi"}]}`)
	req := good.ToRosetta()
	good.ApplyUpstreamExtras(req, "openai-chat")
	if tc, ok := req.Extra["tool_choice"].(map[string]any); !ok || tc["type"] != "function" {
		t.Fatalf("openai tool_choice translation: %+v", req.Extra)
	}

	anyChoice := decodeAnthropic(t, `{"model":"m","max_tokens":8,
		"tool_choice":{"type":"any"},"messages":[{"role":"user","content":"hi"}]}`)
	req2 := anyChoice.ToRosetta()
	anyChoice.ApplyUpstreamExtras(req2, "")
	if req2.Extra["tool_choice"] != "required" {
		t.Fatalf("any→required: %+v", req2.Extra)
	}
	// anthropic 上游拿原形状。
	anyChoice.ApplyUpstreamExtras(req2, "anthropic")
	if tc, ok := req2.Extra["tool_choice"].(map[string]any); !ok || tc["type"] != "any" {
		t.Fatalf("anthropic tool_choice passthrough: %+v", req2.Extra)
	}
	// responses 上游跳过：不挂任何 Extra。
	req3 := anyChoice.ToRosetta()
	anyChoice.ApplyUpstreamExtras(req3, "openai-responses")
	if len(req3.Extra) != 0 {
		t.Fatalf("responses upstream should skip extras, got %+v", req3.Extra)
	}

	bad := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"m","max_tokens":8,"tool_choice":{"type":"tool"},"messages":[{"role":"user","content":"hi"}]}`))
	if _, err := DecodeAnthropicMessagesRequest(bad, 1<<20); err == nil {
		t.Fatalf("tool_choice.type=tool without name should fail")
	}
}

// thinking.type 校验。
func TestDecodeAnthropicMessages_ThinkingValidation(t *testing.T) {
	bad := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"m","max_tokens":8,"thinking":{"type":"enabled"},"messages":[{"role":"user","content":"hi"}]}`))
	if _, err := DecodeAnthropicMessagesRequest(bad, 1<<20); err == nil {
		t.Fatalf("enabled without budget_tokens should fail")
	}
}

// 超限报 MaxBytesError（413 语义）。
func TestDecodeAnthropicMessages_BodyTooLarge(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"`+strings.Repeat("x", 256)+`"}]}`))
	_, err := DecodeAnthropicMessagesRequest(req, 16)
	var tooLarge *http.MaxBytesError
	if !errors.As(err, &tooLarge) {
		t.Fatalf("expected *http.MaxBytesError, got %v", err)
	}
}

// 助手消息 content 为纯字符串（非块数组）也应正常解码。
func TestDecodeAnthropicMessages_AssistantStringContent(t *testing.T) {
	got := decodeAnthropic(t, `{"model":"m","max_tokens":8,"messages":[
		{"role":"user","content":"hi"},
		{"role":"assistant","content":"之前说过"},
		{"role":"user","content":"继续"}]}`)
	req := got.ToRosetta()
	if len(req.Messages) != 3 || req.Messages[1].Role != rosetta.RoleAssistant {
		t.Fatalf("messages: %+v", req.Messages)
	}
	if len(req.Messages[1].Blocks) != 1 || req.Messages[1].Blocks[0].Text != "之前说过" {
		t.Fatalf("assistant string content: %+v", req.Messages[1].Blocks)
	}
}
