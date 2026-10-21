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
	// responses 上游拿翻译后的形状（P1-8：不再整个跳过、静默丢 tool_choice）。
	req3 := anyChoice.ToRosetta()
	anyChoice.ApplyUpstreamExtras(req3, "openai-responses")
	if req3.Extra["tool_choice"] != "required" {
		t.Fatalf("responses tool_choice translation (any→required): %+v", req3.Extra)
	}
	// top_k 不跟随：responses 上游没有对应物，丢弃。
	req4 := decodeAnthropic(t, `{"model":"m","max_tokens":8,"top_k":40,
		"messages":[{"role":"user","content":"hi"}]}`)
	req4Ros := req4.ToRosetta()
	req4.ApplyUpstreamExtras(req4Ros, "openai-responses")
	if len(req4Ros.Extra) != 0 {
		t.Fatalf("responses upstream must not receive top_k, got %+v", req4Ros.Extra)
	}

	bad := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"m","max_tokens":8,"tool_choice":{"type":"tool"},"messages":[{"role":"user","content":"hi"}]}`))
	if _, err := DecodeAnthropicMessagesRequest(bad, 1<<20); err == nil {
		t.Fatalf("tool_choice.type=tool without name should fail")
	}
}

// anthropic → openai-responses：tool_choice 等价翻译矩阵（P1-8 补齐的方向）。
func TestAnthropic_ResponsesToolChoiceMatrix(t *testing.T) {
	cases := []struct {
		name       string
		toolChoice string
		wantJSON   string // "" = 应被丢弃
	}{
		{"auto→auto", `{"type":"auto"}`, `"auto"`},
		{"any→required", `{"type":"any"}`, `"required"`},
		{"tool→function", `{"type":"tool","name":"weather"}`, `{"name":"weather","type":"function"}`},
		// "none" 在 Responses 无对齐语义：丢弃。
		{"none dropped", `{"type":"none"}`, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := decodeAnthropic(t, `{"model":"m","max_tokens":8,
				"tool_choice":`+c.toolChoice+`,"messages":[{"role":"user","content":"hi"}]}`)
			req := got.ToRosetta()
			got.ApplyUpstreamExtras(req, "openai-responses")
			if c.wantJSON == "" {
				if _, exists := req.Extra["tool_choice"]; exists {
					t.Fatalf("tool_choice 应被丢弃，实际 %v", req.Extra["tool_choice"])
				}
				return
			}
			raw, _ := json.Marshal(req.Extra["tool_choice"])
			if string(raw) != c.wantJSON {
				t.Fatalf("tool_choice = %s, want %s", raw, c.wantJSON)
			}
		})
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

// decodeErr 解析一个**应当失败**的 body，返回错误。
func decodeErr(t *testing.T, body string) error {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	_, err := DecodeAnthropicMessagesRequest(req, 1<<20)
	if err == nil {
		t.Fatalf("body 解析竟成功：%s", body)
	}
	return err
}

// content 为**单个对象**时必须 400，不得静默变成空消息（2026-10-12 修复 P2-16）。
//
// Anthropic 官方协议里 content 只能是 string 或块**数组**。但客户端常会发
// 单对象（尤其从 OpenAI 格式手改过来的脚本）：
//
//	{"role":"user","content":{"type":"text","text":"hi"}}
//
// 旧实现把它 Unmarshal 到 []anthropicBlock 上失败 → 返回 nil；调用方
// convertUserMessage 见 blocks 为空，又试按纯字符串解析、也失败，最后落到
// `rosetta.User("")` —— **用户输入被静默丢弃且请求照常 200**。
// 客户端以为发成功了，上游却没收到任何内容，这是最难排查的一类问题。
//
// 现在在 DecodeAnthropicMessagesRequest 里挡掉，报明确的 400。
func TestDecodeAnthropicMessages_SingleObjectContentRejected(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"user 单对象", `{"model":"m","max_tokens":8,"messages":[
			{"role":"user","content":{"type":"text","text":"hi"}}]}`},
		{"assistant 单对象", `{"model":"m","max_tokens":8,"messages":[
			{"role":"assistant","content":{"type":"text","text":"之前"}}]}`},
		{"多条中夹一条单对象", `{"model":"m","max_tokens":8,"messages":[
			{"role":"user","content":"正常"},
			{"role":"user","content":{"type":"text","text":"单对象"}}]}`},
		{"system 单对象", `{"model":"m","max_tokens":8,
			"system":{"type":"text","text":"指令"},
			"messages":[{"role":"user","content":"hi"}]}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := decodeErr(t, c.body)
			if !errors.Is(err, ErrContentSingleObject) {
				t.Fatalf("错误不是 ErrContentSingleObject：%v", err)
			}
		})
	}
}

// 合法形态不得被新校验误伤（2026-10-12）。
//
// P2-16 的修复加了一道 content 形态校验，这里守住它的边界：数组、纯字符串、
// 空缺省、空数组都必须照常通过。误伤比漏报更危险 —— 会让合法客户端全线 400。
func TestDecodeAnthropicMessages_LegalContentShapesStillPass(t *testing.T) {
	bodies := []struct{ name, body string }{
		{"纯字符串", `{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`},
		{"块数组", `{"model":"m","max_tokens":8,"messages":[
			{"role":"user","content":[{"type":"text","text":"hi"}]}]}`},
		{"块数组含多种类型", `{"model":"m","max_tokens":8,"messages":[
			{"role":"user","content":[
				{"type":"text","text":"看这张图"},
				{"type":"image","source":{"type":"url","url":"http://x/y.png"}},
				{"type":"tool_result","tool_use_id":"t1","content":"结果"}]}]}`},
		{"空 content 数组", `{"model":"m","max_tokens":8,"messages":[{"role":"user","content":[]}]}`},
		{"缺省 content", `{"model":"m","max_tokens":8,"messages":[{"role":"user"}]}`},
		{"system 为数组", `{"model":"m","max_tokens":8,
			"system":[{"type":"text","text":"指令"}],
			"messages":[{"role":"user","content":"hi"}]}`},
		{"system 为字符串", `{"model":"m","max_tokens":8,
			"system":"指令",
			"messages":[{"role":"user","content":"hi"}]}`},
	}
	for _, b := range bodies {
		t.Run(b.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(b.body))
			if _, err := DecodeAnthropicMessagesRequest(req, 1<<20); err != nil {
				t.Fatalf("合法形态被误拒：%v", err)
			}
		})
	}
}

// tool_result 里的**媒体块**不得被静默丢弃（2026-10-21 修 A2-P1-4）。
//
// 回归风险：agent 场景下「工具返回了一张截图，模型应该看这张截图」是极常见
// 的用法。修复前 toolResultText 只抽 text 块，image/document 被静默丢掉 ——
// 请求 200、回答正常，客户端与模型都察觉不到（内容就这么没了）。
//
// 处置与 Responses 入口同构：媒体不能塞进 tool_result（它的 Content 是纯
// 文本语义），而是搬进紧随其后的 user 消息，并留一行说明它被搬去了哪。
func TestAnthropicToolResult_MediaNotDropped(t *testing.T) {
	got := decodeAnthropic(t, `{
		"model":"claude-sonnet-4-5",
		"max_tokens":2048,
		"messages":[{
			"role":"user",
			"content":[{
				"type":"tool_result",
				"tool_use_id":"t1",
				"content":[
					{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aGk="}},
					{"type":"text","text":"caption"}
				]
			}]
		}]
	}`)
	req := got.ToRosetta()

	var toolMsg, mediaMsg *rosetta.Message
	for i := range req.Messages {
		m := &req.Messages[i]
		switch m.Role {
		case rosetta.RoleTool:
			toolMsg = m
		case rosetta.RoleUser:
			if mediaMsg == nil {
				mediaMsg = m
			}
		}
	}
	if toolMsg == nil {
		t.Fatalf("没有 tool 消息：%+v", req.Messages)
	}
	if toolMsg.Blocks[0].Content != "caption" {
		t.Fatalf("tool_result 文本 = %q，期望 \"caption\"", toolMsg.Blocks[0].Content)
	}
	if mediaMsg == nil {
		t.Fatalf("媒体被静默丢弃了（没有紧随其后的 user 消息）：%+v", req.Messages)
	}
	var sawImage bool
	for _, b := range mediaMsg.Blocks {
		if b.Type == rosetta.BlockImage {
			sawImage = true
		}
	}
	if !sawImage {
		t.Fatalf("搬过去的 user 消息里没有 image 块：%+v", mediaMsg.Blocks)
	}
	// 留痕：模型要知道「有东西被搬走了」。
	if len(mediaMsg.Blocks) == 0 || mediaMsg.Blocks[0].Type != rosetta.BlockText ||
		!strings.Contains(mediaMsg.Blocks[0].Text, "附件") {
		t.Fatalf("缺少「媒体已移至下一条消息」的说明：%+v", mediaMsg.Blocks)
	}
}
