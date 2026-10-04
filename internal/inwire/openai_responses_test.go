package inwire

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cn-maul/rosetta"
)

func decodeResponses(t *testing.T, body string) *ResponsesRequest {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	got, err := DecodeResponsesRequest(req, 1<<20)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	return got
}

// 全特性解码：instructions、字符串/数组 input、function_call 回放、
// reasoning 剥除、tools、thinking effort、图像。
func TestDecodeResponses_Full(t *testing.T) {
	got := decodeResponses(t, `{
		"model":"gpt-4o",
		"instructions":"你是网关",
		"max_output_tokens":2048,
		"temperature":0.5,
		"stream":true,
		"reasoning":{"effort":"low"},
		"tools":[{"type":"function","name":"weather","description":"查天气","parameters":{"type":"object"}}],
		"tool_choice":{"type":"function","name":"weather"},
		"input":[
			{"role":"user","content":"你好"},
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"我调个工具"}]},
			{"type":"function_call","call_id":"c1","name":"weather","arguments":"{\"city\":\"北京\"}"},
			{"type":"function_call_output","call_id":"c1","output":"晴"},
			{"type":"reasoning","summary":[]},
			{"type":"message","role":"user","content":[
				{"type":"input_text","text":"看这张图"},
				{"type":"input_image","image_url":"https://x/y.png"}
			]}
		]
	}`)

	req := got.ToRosetta()
	if req.Model != "gpt-4o" || req.MaxOutputTokens != 2048 || req.System != "你是网关" {
		t.Fatalf("basic fields: %+v", req)
	}
	if req.Thinking == nil || req.Thinking.Effort != rosetta.EffortLow {
		t.Fatalf("reasoning effort: %+v", req.Thinking)
	}
	if len(req.Tools) != 1 || req.Tools[0].Name != "weather" {
		t.Fatalf("tools: %+v", req.Tools)
	}

	// user → assistant(text) → assistant(tool_call) → tool → user(text+image)
	if len(req.Messages) != 5 {
		t.Fatalf("message count = %d, want 5: %+v", len(req.Messages), req.Messages)
	}
	if req.Messages[1].Role != rosetta.RoleAssistant || req.Messages[1].Blocks[0].Text != "我调个工具" {
		t.Fatalf("assistant text: %+v", req.Messages[1])
	}
	tc := req.Messages[2]
	if tc.Role != rosetta.RoleAssistant || tc.Blocks[0].Type != rosetta.BlockToolCall || tc.Blocks[0].ToolCallID != "c1" {
		t.Fatalf("function_call item: %+v", tc)
	}
	tr := req.Messages[3]
	if tr.Role != rosetta.RoleTool || tr.Blocks[0].Content != "晴" {
		t.Fatalf("function_call_output item: %+v", tr)
	}
	last := req.Messages[4]
	if last.Role != rosetta.RoleUser || len(last.Blocks) != 2 ||
		last.Blocks[0].Text != "看这张图" || last.Blocks[1].ImageURL != "https://x/y.png" {
		t.Fatalf("final user message: %+v", last)
	}
}

// tool_choice 翻译：openai-chat 上游 function→嵌套形状，responses 上游原样。
func TestDecodeResponses_ToolChoice(t *testing.T) {
	got := decodeResponses(t, `{"model":"m","input":"hi",
		"tool_choice":{"type":"function","name":"weather"}}`)
	req := got.ToRosetta()
	got.ApplyUpstreamExtras(req, "openai-chat")
	if tc, ok := req.Extra["tool_choice"].(map[string]any); !ok || tc["type"] != "function" {
		t.Fatalf("openai-chat tool_choice: %+v", req.Extra)
	}
	req2 := got.ToRosetta()
	got.ApplyUpstreamExtras(req2, "openai-responses")
	if _, ok := req2.Extra["tool_choice"].(json.RawMessage); !ok {
		t.Fatalf("responses tool_choice passthrough: %+v", req2.Extra)
	}
	// anthropic 上游跳过：不挂任何 Extra。
	req3 := got.ToRosetta()
	got.ApplyUpstreamExtras(req3, "anthropic")
	if len(req3.Extra) != 0 {
		t.Fatalf("anthropic upstream should skip extras, got %+v", req3.Extra)
	}
}

// text.format → openai-chat 的 response_format 翻译。
func TestDecodeResponses_TextFormat(t *testing.T) {
	got := decodeResponses(t, `{"model":"m","input":"hi",
		"text":{"format":{"type":"json_schema","name":"out","schema":{"type":"object"}}}}`)
	req := got.ToRosetta()
	got.ApplyUpstreamExtras(req, "openai-chat")
	rf, ok := req.Extra["response_format"].(map[string]any)
	if !ok || rf["type"] != "json_schema" {
		t.Fatalf("response_format: %+v", req.Extra)
	}
	inner := rf["json_schema"].(map[string]any)
	if inner["name"] != "out" {
		t.Fatalf("json_schema inner: %+v", inner)
	}

	// responses 上游拿原始 text 外壳。
	req2 := got.ToRosetta()
	got.ApplyUpstreamExtras(req2, "openai-responses")
	if _, ok := req2.Extra["text"].(map[string]any); !ok {
		t.Fatalf("responses text passthrough: %+v", req2.Extra)
	}
}

// 不支持的 item 类型不静默丢弃：转成显式的未知块，rosetta 在请求时以
// ErrInvalidRequest 拒绝（映射 400）—— 这里验证块被保留为可识别形态。
func TestDecodeResponses_UnknownItemRejected(t *testing.T) {
	got := decodeResponses(t, `{"model":"m","input":[
		{"role":"user","content":"hi"},
		{"type":"web_search_call","status":"completed"}]}`)
	req := got.ToRosetta()
	found := false
	for _, m := range req.Messages {
		for _, b := range m.Blocks {
			if b.Type == "responses_item:web_search_call" {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("unknown item should be preserved for upstream-side rejection")
	}
}

// 基础校验：model / input 必填，tools 类型白名单，413。
func TestDecodeResponses_Validation(t *testing.T) {
	if _, err := DecodeResponsesRequest(httptest.NewRequest(http.MethodPost, "/v1/responses",
		strings.NewReader(`{"input":"hi"}`)), 1<<20); err == nil {
		t.Fatalf("missing model should fail")
	}
	if _, err := DecodeResponsesRequest(httptest.NewRequest(http.MethodPost, "/v1/responses",
		strings.NewReader(`{"model":"m"}`)), 1<<20); err == nil {
		t.Fatalf("missing input should fail")
	}
	big := httptest.NewRequest(http.MethodPost, "/v1/responses",
		strings.NewReader(`{"model":"m","input":"`+strings.Repeat("x", 256)+`"}`))
	if _, err := DecodeResponsesRequest(big, 16); err == nil {
		t.Fatalf("oversized body should fail")
	} else {
		var tooLarge *http.MaxBytesError
		if !errors.As(err, &tooLarge) {
			t.Fatalf("expected MaxBytesError, got %v", err)
		}
	}
	if _, err := DecodeResponsesRequest(httptest.NewRequest(http.MethodPost, "/v1/responses",
		strings.NewReader(`{"model":"m","input":"hi","tools":[{"type":"web_search"}]}`)), 1<<20); err == nil {
		t.Fatalf("builtin tool should be rejected")
	}
}
