package inwire

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cn-maul/rosetta"
)

// 超限 body 必须以 *http.MaxBytesError 报出（调用方据此映射 413），而不是被
// LimitReader 静默截断后以「unexpected end of JSON input」报 400 —— 那会让
// 客户端分不清「body 太大该减内容」还是「body 格式错该改请求」。
func TestDecodeOpenAIChatRequest_BodyTooLarge(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"`+strings.Repeat("x", 128)+`"}]}`))

	_, err := DecodeOpenAIChatRequest(req, 16)
	var tooLarge *http.MaxBytesError
	if !errors.As(err, &tooLarge) {
		t.Fatalf("expected *http.MaxBytesError, got %v", err)
	}
	if tooLarge.Limit != 16 {
		t.Fatalf("limit = %d, want 16", tooLarge.Limit)
	}
}

// 恰好等于上限的 body 必须正常解码：「多读一个字节再判长度」不能误伤满额请求。
func TestDecodeOpenAIChatRequest_ExactlyAtLimit(t *testing.T) {
	body := `{"model":"m","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))

	got, err := DecodeOpenAIChatRequest(req, int64(len(body)))
	if err != nil {
		t.Fatalf("decode at limit: %v", err)
	}
	if got.Model != "m" || len(got.Messages) != 1 {
		t.Fatalf("unexpected decode result: %+v", got)
	}
}

// decodeChat 解析一段 chat 请求体（翻译矩阵测试的共用入口）。
func decodeChat(t *testing.T, body string) *OpenAIChatRequest {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	got, err := DecodeOpenAIChatRequest(req, 1<<20)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	return got
}

// n > 1 必须被拒绝（2026-10-14，交接文档 §7.2 第 1 条）。
//
// 本测试存在的全部理由是「**不得静默降级**」：rosetta 与三个上游协议都不
// 支持多候选，ToRosetta 也从不读 n。若不拒绝，客户端传 `{"n":3}` 会拿到
// 200 + 1 个候选 —— 请求被无声打了对折，调用方看到的是「模型就这样」，
// 而不是「我的请求没被完整执行」。
//
// 这组断言里最要紧的不是「返回了 error」，而是 **n=1 与 n=0 仍然放行**。
// 把拒绝写成 `req.N != nil && *req.N != 1` 之类的严格判定，会让按
// 「0 = 不指定」生成请求体的客户端集体 400 —— 那是把一个小修变成事故。
func TestDecodeOpenAIChatRequest_RejectsMultipleChoices(t *testing.T) {
	const base = `{"model":"m","messages":[{"role":"user","content":"hi"}],`

	for _, n := range []int{2, 3, 8, 128} {
		t.Run(fmt.Sprintf("n=%d", n), func(t *testing.T) {
			body := base + fmt.Sprintf(`"n":%d}`, n)
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
			_, err := DecodeOpenAIChatRequest(req, 1<<20)
			if err == nil {
				t.Fatalf("n=%d 被接受 —— 客户端会拿到 1 个候选却以为要了 %d 个", n, n)
			}
			// 错误消息必须指名 n：客户端要能据此改代码，而不是去猜
			// 到底是 model、messages 还是别的字段的问题。
			if !strings.Contains(err.Error(), "n=") {
				t.Errorf("错误消息没有指明是 n 的问题：%v", err)
			}
		})
	}
}

// n=1（OpenAI 默认）与 n=0（OpenAI 视同未指定）都必须放行。
//
// 这两条是上一组测试的对照：只测「n>1 被拒」的套件完全看不见
// 「拒绝范围写宽了」这种回归，而那正是本改动最容易犯的错。
func TestDecodeOpenAIChatRequest_AllowsSingleChoice(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"n=1", `{"model":"m","messages":[{"role":"user","content":"hi"}],"n":1}`},
		{"n=0", `{"model":"m","messages":[{"role":"user","content":"hi"}],"n":0}`},
		{"不传 n", `{"model":"m","messages":[{"role":"user","content":"hi"}]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(tc.body))
			got, err := DecodeOpenAIChatRequest(req, 1<<20)
			if err != nil {
				t.Fatalf("decode 失败：%v", err)
			}
			if got.Model != "m" {
				t.Errorf("model = %q, want m", got.Model)
			}
		})
	}
}

// 负数 n 必须被拒绝。
//
// 与 n>1 分开测而不是塞进同一组：两者虽然都走 400，但语义不同 ——
// 一个是「支持不了」，一个是「取值本身不合法」。合成一条会让将来
// 有人改动其中一支时看不出另一支的意图。
func TestDecodeOpenAIChatRequest_RejectsNegativeN(t *testing.T) {
	body := `{"model":"m","messages":[{"role":"user","content":"hi"}],"n":-1}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	if _, err := DecodeOpenAIChatRequest(req, 1<<20); err == nil {
		t.Fatal("n=-1 被接受 —— 一个没有意义的取值不该被静默接受")
	}
}

// ToRosetta 把 chat 的 tool_choice / response_format 翻译成 rosetta 的
// 类型化字段；跨协议翻译（Responses 的 text.format、Anthropic 的 any 等）
// 由 SDK 负责，网关不再手搓。
func TestToRosetta_ToolChoiceAndResponseFormat(t *testing.T) {
	got := decodeChat(t, `{"model":"m","messages":[{"role":"user","content":"hi"}],
		"response_format":{"type":"json_schema","json_schema":{"name":"out","strict":true,"schema":{"type":"object"}}},
		"tool_choice":{"type":"function","function":{"name":"weather"}},"parallel_tool_calls":false}`)
	req := got.ToRosetta()

	if req.ResponseFormat == nil {
		t.Fatal("response_format 未落到类型化字段")
	}
	if req.ResponseFormat.Type != "json_schema" || req.ResponseFormat.Name != "out" ||
		req.ResponseFormat.Strict == nil || !*req.ResponseFormat.Strict {
		t.Fatalf("ResponseFormat = %+v，期望 json_schema/out/strict", req.ResponseFormat)
	}
	if string(req.ResponseFormat.Schema) != `{"type":"object"}` {
		t.Fatalf("ResponseFormat.Schema = %s", req.ResponseFormat.Schema)
	}
	if req.ToolChoice == nil || req.ToolChoice.Mode != rosetta.ToolChoiceTool || req.ToolChoice.Name != "weather" {
		t.Fatalf("ToolChoice = %+v，期望 tool/weather", req.ToolChoice)
	}
	if req.ParallelToolCalls == nil || *req.ParallelToolCalls {
		t.Fatalf("ParallelToolCalls = %v，期望 false", req.ParallelToolCalls)
	}
}

// tool_choice 的 string / 命名函数两种形态都翻译成统一 ToolChoice。
func TestToRosetta_ToolChoiceModes(t *testing.T) {
	cases := []struct {
		name string
		json string
		want rosetta.ToolChoice
		drop bool
	}{
		{"auto", `"auto"`, rosetta.ToolChoice{Mode: rosetta.ToolChoiceAuto}, false},
		{"none", `"none"`, rosetta.ToolChoice{Mode: rosetta.ToolChoiceNone}, false},
		{"required", `"required"`, rosetta.ToolChoice{Mode: rosetta.ToolChoiceRequired}, false},
		{"named fn", `{"type":"function","function":{"name":"weather"}}`, rosetta.ToolChoice{Mode: rosetta.ToolChoiceTool, Name: "weather"}, false},
		{"unknown string dropped", `"bogus"`, rosetta.ToolChoice{}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := decodeChat(t, `{"model":"m","messages":[{"role":"user","content":"hi"}],"tool_choice":`+c.json+`}`)
			req := got.ToRosetta()
			if c.drop {
				if req.ToolChoice != nil {
					t.Fatalf("tool_choice 应被丢弃，实际 %+v", req.ToolChoice)
				}
				return
			}
			if req.ToolChoice == nil || *req.ToolChoice != c.want {
				t.Fatalf("ToolChoice = %+v，期望 %+v", req.ToolChoice, c.want)
			}
		})
	}
}

// response_format 的 text / json_object 两态。
func TestToRosetta_ResponseFormatSimple(t *testing.T) {
	for _, c := range []struct{ body, want string }{
		{`{"type":"text"}`, "text"},
		{`{"type":"json_object"}`, "json_object"},
	} {
		got := decodeChat(t, `{"model":"m","messages":[{"role":"user","content":"hi"}],"response_format":`+c.body+`}`)
		req := got.ToRosetta()
		if req.ResponseFormat == nil || req.ResponseFormat.Type != c.want {
			t.Fatalf("ResponseFormat = %+v，期望 %s", req.ResponseFormat, c.want)
		}
	}
}

// 直通采样字段落到类型化字段，不再塞 Extra。
func TestToRosetta_SamplingParamsPassThrough(t *testing.T) {
	got := decodeChat(t, `{"model":"m","messages":[{"role":"user","content":"hi"}],
		"seed":7,"user":"u","presence_penalty":0.5,"frequency_penalty":0.6,
		"parallel_tool_calls":true,"prompt_cache_key":"ck"}`)
	req := got.ToRosetta()
	if req.Seed == nil || *req.Seed != 7 {
		t.Fatalf("Seed = %v", req.Seed)
	}
	if req.User != "u" {
		t.Fatalf("User = %q", req.User)
	}
	if req.PresencePenalty == nil || *req.PresencePenalty != 0.5 {
		t.Fatalf("PresencePenalty = %v", req.PresencePenalty)
	}
	if req.FrequencyPenalty == nil || *req.FrequencyPenalty != 0.6 {
		t.Fatalf("FrequencyPenalty = %v", req.FrequencyPenalty)
	}
	if req.ParallelToolCalls == nil || !*req.ParallelToolCalls {
		t.Fatalf("ParallelToolCalls = %v", req.ParallelToolCalls)
	}
	if req.PromptCacheKey != "ck" {
		t.Fatalf("PromptCacheKey = %q", req.PromptCacheKey)
	}
	if len(req.Extra) != 0 {
		t.Fatalf("采样字段不该进 Extra：%+v", req.Extra)
	}
}

// 硬约束判定：json_object / json_schema 算结构化输出，text/缺失/畸形不算。
func TestRequiresStructuredOutput_Chat(t *testing.T) {
	cases := []struct {
		body string
		want bool
	}{
		{`{"model":"m","messages":[{"role":"user","content":"hi"}],"response_format":{"type":"json_object"}}`, true},
		{`{"model":"m","messages":[{"role":"user","content":"hi"}],"response_format":{"type":"json_schema","json_schema":{"name":"o","schema":{}}}}`, true},
		{`{"model":"m","messages":[{"role":"user","content":"hi"}],"response_format":{"type":"text"}}`, false},
		{`{"model":"m","messages":[{"role":"user","content":"hi"}]}`, false},
	}
	for i, c := range cases {
		got := decodeChat(t, c.body)
		if got.RequiresStructuredOutput() != c.want {
			t.Fatalf("case %d: RequiresStructuredOutput = %v, want %v", i, !c.want, c.want)
		}
	}
	// 畸形 response_format（decode 阶段 json.RawMessage 不会失败）不算约束，
	// 交给上游拒绝。
	got := decodeChat(t, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	got.ResponseFormat = json.RawMessage(`{"type":`)
	if got.RequiresStructuredOutput() {
		t.Fatalf("malformed response_format must not count as structured output")
	}
}

// chat 入口的 reasoning_content 回放现在由 rosetta 的 chat 编码器承担
// （v1.3.0 P1-1a：BlockThinking → 同一条 assistant 消息的
// reasoning_content）。网关自 v1.3.0 起删掉了「整份接管 messages」的
// 逃生舱，这条用例守住「接管删了、回放还在」，且思考与正文/工具调用
// 仍落在**同一条** assistant 消息上（DeepSeek 系硬要求）。
func TestOpenAIChat_ReasoningContentReplayedBySDK(t *testing.T) {
	var got map[string]any
	srv := upstreamRec(t, &got)
	defer srv.Close()

	ros := decodeChat(t, `{"model":"m","messages":[
		{"role":"user","content":"weather?"},
		{"role":"assistant","content":"let me check","reasoning_content":"need weather",
		 "tool_calls":[{"id":"call_1","type":"function",
		   "function":{"name":"weather","arguments":"{\"city\":\"SH\"}"}}]},
		{"role":"tool","tool_call_id":"call_1","content":"sunny"}
	]}`).ToRosetta()

	// 回放完全走 SDK 编码器：网关不往 Extra 塞 messages。
	if _, taken := ros.Extra["messages"]; taken {
		t.Fatalf("chat 入口不该再接管 messages：%+v", ros.Extra["messages"])
	}
	if _, err := gwClient(t, srv.URL).Chat(context.Background(), ros); err != nil {
		t.Fatalf("请求不应失败: %v", err)
	}

	msgs, ok := got["messages"].([]any)
	if !ok || len(msgs) != 3 {
		t.Fatalf("上游 messages 形状异常：%#v", got["messages"])
	}
	assistant, ok := msgs[1].(map[string]any)
	if !ok {
		t.Fatalf("第二条应是 assistant：%#v", msgs[1])
	}
	if assistant["role"] != "assistant" {
		t.Fatalf("role = %v, want assistant", assistant["role"])
	}
	if assistant["reasoning_content"] != "need weather" {
		t.Fatalf("reasoning_content 未回放：%#v", assistant)
	}
	if assistant["content"] != "let me check" {
		t.Fatalf("正文与思考不在同一条消息上：%#v", assistant)
	}
	tcs, ok := assistant["tool_calls"].([]any)
	if !ok || len(tcs) != 1 {
		t.Fatalf("tool_calls 丢失或拆分：%#v", assistant["tool_calls"])
	}
	if tc, _ := tcs[0].(map[string]any); tc["id"] != "call_1" {
		t.Fatalf("tool_call id 变了：%#v", tcs[0])
	}
	toolMsg, _ := msgs[2].(map[string]any)
	if toolMsg["role"] != "tool" || toolMsg["tool_call_id"] != "call_1" {
		t.Fatalf("工具结果未按协议回放：%#v", msgs[2])
	}
}
