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
	//
	// {"type":"reasoning"} 项不计入消息数：它不带 role 也不带 content，
	// 按 F17 规则（user 侧只采信 input_text）产不出任何消息，在 dispatch
	// 里就是空操作 —— 这与修复前一致。
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
	// anthropic 上游拿翻译后的形状（P1-8：不再整个跳过、静默丢 tool_choice）。
	req3 := got.ToRosetta()
	got.ApplyUpstreamExtras(req3, "anthropic")
	if tc, ok := req3.Extra["tool_choice"].(map[string]any); !ok || tc["type"] != "tool" || tc["name"] != "weather" {
		t.Fatalf("anthropic tool_choice translation: %+v", req3.Extra)
	}
	// text.format 不跟随：anthropic 上游没有对应物（结构化输出这类硬约束由
	// 网关层 requiresStructuredOutput 拦截，不放行后丢失）。
	got2 := decodeResponses(t, `{"model":"m","input":"hi",
		"text":{"format":{"type":"json_object"}}}`)
	req4 := got2.ToRosetta()
	got2.ApplyUpstreamExtras(req4, "anthropic")
	if len(req4.Extra) != 0 {
		t.Fatalf("anthropic upstream must not receive text.format, got %+v", req4.Extra)
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

// responses → anthropic：tool_choice 等价翻译矩阵（P1-8 补齐的方向）。
func TestResponses_AnthropicToolChoiceMatrix(t *testing.T) {
	cases := []struct {
		name       string
		toolChoice string
		wantJSON   string // "" = 应被丢弃
	}{
		{"auto→auto", `"auto"`, `{"type":"auto"}`},
		{"required→any", `"required"`, `{"type":"any"}`},
		{"object auto→auto", `{"type":"auto"}`, `{"type":"auto"}`},
		{"object required→any", `{"type":"required"}`, `{"type":"any"}`},
		{"function→tool", `{"type":"function","name":"weather"}`, `{"name":"weather","type":"tool"}`},
		// "none" 在 Anthropic 无对应物：丢弃。
		{"none dropped", `"none"`, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := decodeResponses(t, `{"model":"m","input":"hi","tool_choice":`+c.toolChoice+`}`)
			req := got.ToRosetta()
			got.ApplyUpstreamExtras(req, "anthropic")
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

// 硬约束判定（Responses 入口）：text.format 为 json_object / json_schema 才算。
func TestResponses_RequiresStructuredOutput(t *testing.T) {
	cases := []struct {
		body string
		want bool
	}{
		{`{"model":"m","input":"hi","text":{"format":{"type":"json_object"}}}`, true},
		{`{"model":"m","input":"hi","text":{"format":{"type":"json_schema","name":"o","schema":{}}}}`, true},
		{`{"model":"m","input":"hi","text":{"format":{"type":"text"}}}`, false},
		{`{"model":"m","input":"hi"}`, false},
	}
	for i, c := range cases {
		got := decodeResponses(t, c.body)
		if got.RequiresStructuredOutput() != c.want {
			t.Fatalf("case %d: RequiresStructuredOutput = %v, want %v", i, !c.want, c.want)
		}
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

// TestResponses_TextFormatNotDoubleWrapped 钉住一条 P1 的修复。
//
// 修复前把整个 a.Text（已是 {"format":…} 的外壳）当成 format 的值放回去，
// 出站变成 "text":{"format":{"format":{…}}}：上游看到的 format 里没有 type、
// schema 埋在重复的 format 键下，于是结构化输出静默不生效，而客户端以为
// 它生效了。同函数上面的 openai-chat 分支用 textFormatMap() 正确拆包。
func TestResponses_TextFormatNotDoubleWrapped(t *testing.T) {
	a := decodeResponses(t, `{"model":"m","input":"hi",
		"text":{"format":{"type":"json_schema","name":"t","strict":true,
			"schema":{"type":"object","properties":{"a":{"type":"string"}}}}}}`)

	req := a.ToRosetta()
	a.ApplyUpstreamExtras(req, "openai-responses")

	textObj, ok := req.Extra["text"].(map[string]any)
	if !ok {
		t.Fatalf("extra[text] 应是 map，实际 %T（%v）", req.Extra["text"], req.Extra["text"])
	}
	format, ok := textObj["format"].(map[string]any)
	if !ok {
		t.Fatalf("text.format 应是 map，实际 %T —— 说明整包被当成 format 的值塞进去了",
			textObj["format"])
	}
	if format["type"] != "json_schema" {
		t.Fatalf("text.format.type = %v，期望 json_schema", format["type"])
	}
	if _, doubled := format["format"]; doubled {
		t.Fatalf("text.format 里不该再出现一层 format（双层包裹）")
	}
	if _, hasName := format["name"]; !hasName {
		t.Fatalf("text.format 丢了 schema 名：%v", format)
	}
}

// text 里除 format 之外的键不能因为修 format 而被一起弄丢。
func TestResponses_TextOtherKeysPassThrough(t *testing.T) {
	a := decodeResponses(t, `{"model":"m","input":"hi",
		"text":{"format":{"type":"json_object"},"verbosity":"low"}}`)

	req := a.ToRosetta()
	a.ApplyUpstreamExtras(req, "openai-responses")

	textObj, ok := req.Extra["text"].(map[string]any)
	if !ok {
		t.Fatalf("extra[text] 应是 map，实际 %T", req.Extra["text"])
	}
	if textObj["verbosity"] != "low" {
		t.Fatalf("text.verbosity 被丢了：%v", textObj)
	}
	if _, ok := textObj["format"]; !ok {
		t.Fatalf("text.format 丢了：%v", textObj)
	}
}

// previous_response_id / store 解析后必须送到 openai-responses 上游，
// 否则客户端以为会话续上了，实际模型只看到本次 input。
func TestResponses_PreviousResponseIDForwarded(t *testing.T) {
	a := decodeResponses(t, `{"model":"m","input":"hi",
		"previous_response_id":"resp_abc123","store":false}`)

	req := a.ToRosetta()
	a.ApplyUpstreamExtras(req, "openai-responses")

	if req.Extra["previous_response_id"] != "resp_abc123" {
		t.Fatalf("previous_response_id 未透传：%v", req.Extra["previous_response_id"])
	}
	if req.Extra["store"] != false {
		t.Fatalf("显式 store:false 未透传：%v", req.Extra["store"])
	}
}

// role 缺失的 message 项里，output_text 不得被当成用户输入。
//
// 修复前 default 分支取 input_text 与 output_text 的并集，于是模型自己的
// 上一句回答被伪装成用户的新指令。实测上游曾收到
// {role:"user", content:"SECRET-ASSISTANT-OUTPUT"}。
func TestResponses_RolelessOutputTextNotTreatedAsUser(t *testing.T) {
	a := decodeResponses(t, `{"model":"m","input":[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"QUESTION"}]},
		{"type":"message","content":[{"type":"output_text","text":"SECRET-ASSISTANT-OUTPUT"}]}
	]}`)

	req := a.ToRosetta()
	// role 缺失且只带 output_text 的那一项被**整条丢弃**：既不能变成
	// user 轮（那是把模型输出伪装成用户指令），也不能留下「空内容消息」
	//（rosetta 校验会判 no content blocks → 整个请求 400）。
	if len(req.Messages) != 1 {
		t.Fatalf("期望只剩 1 条消息，实际 %d：%+v", len(req.Messages), req.Messages)
	}
	// 前一条显式 user 的内容不能受影响
	first := req.Messages[0]
	if first.Role != rosetta.RoleUser {
		t.Fatalf("显式 user 轮角色被改：%q", first.Role)
	}
	if len(first.Blocks) == 0 || first.Blocks[0].Text != "QUESTION" {
		t.Fatalf("显式 user 轮的内容被误伤：%+v", first.Blocks)
	}
}

// TestResponses_OutputTextOnlyItemIsDroppedNotEmitted 钉住 F17 修复的一个
// 连带问题：role 缺失且只带 output_text 的项，丢弃后**不能**塞空文本块。
//
// rosetta 的校验判「空文本不算内容」（message has no content blocks），
// 于是整个请求 400 —— 那比「把 assistant 输出当用户输入」更糟：后者至少
// 请求还能通。实测本条曾把请求打成
// 400 rosetta: invalid request: Messages[1]: message has no content blocks。
func TestResponses_OutputTextOnlyItemIsDroppedNotEmitted(t *testing.T) {
	a := decodeResponses(t, `{"model":"m","input":[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"QUESTION"}]},
		{"type":"message","content":[{"type":"output_text","text":"ASSISTANT-ONLY"}]}
	]}`)

	req := a.ToRosetta()
	// 只剩那条显式的 user 消息；被丢的那条不能变成「空内容消息」
	if len(req.Messages) != 1 {
		t.Fatalf("期望只剩 1 条消息，实际 %d：%+v", len(req.Messages), req.Messages)
	}
	for _, m := range req.Messages {
		for _, b := range m.Blocks {
			if strings.Contains(b.Text, "ASSISTANT-ONLY") {
				t.Fatalf("assistant 的 output_text 仍被转发：%q", b.Text)
			}
		}
	}
	if len(req.Messages[0].Blocks) == 0 || req.Messages[0].Blocks[0].Text != "QUESTION" {
		t.Fatalf("显式 user 轮内容被误伤：%+v", req.Messages[0].Blocks)
	}

	// 顺带确认：input 全是「不可采信的 role 缺失项」时，转发出去会是
	// 一条空消息（SDK 判 no content blocks → 400）。decode 侧不拦这种情况
	// （客户端可以只发 reasoning / function_call 项），故这里只断言
	// 「不会把 assistant 文本当成 user 输入」。
}

// 显式 assistant 轮仍然要正常读到 output_text（别把上一条修复过头）。
func TestResponses_AssistantRoleStillReadsOutputText(t *testing.T) {
	a := decodeResponses(t, `{"model":"m","input":[
		{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ANSWER"}]}
	]}`)

	req := a.ToRosetta()
	if len(req.Messages) != 1 || req.Messages[0].Role != rosetta.RoleAssistant {
		t.Fatalf("期望 1 条 assistant 消息，实际 %+v", req.Messages)
	}
	if len(req.Messages[0].Blocks) == 0 || req.Messages[0].Blocks[0].Text != "ANSWER" {
		t.Fatalf("assistant 的 output_text 丢了：%+v", req.Messages[0].Blocks)
	}
}
