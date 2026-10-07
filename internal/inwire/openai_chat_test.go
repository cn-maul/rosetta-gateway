package inwire

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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

// chat → openai-responses：能等价翻译的字段必须翻译，不得整个静默丢弃（P1-8）。
func TestApplyProtocolPrivateExtra_ResponsesTranslation(t *testing.T) {
	got := decodeChat(t, `{"model":"m","messages":[{"role":"user","content":"hi"}],
		"response_format":{"type":"json_schema","json_schema":{"name":"out","strict":true,"schema":{"type":"object"}}},
		"tool_choice":{"type":"function","function":{"name":"weather"}},
		"parallel_tool_calls":false,
		"presence_penalty":0.5,"frequency_penalty":0.5,"seed":42,"user":"u-1"}`)

	req := got.ToRosetta()
	got.ApplyProtocolPrivateExtra(req, "openai-responses")

	// response_format json_schema → text.format 平铺外壳（与正向翻译互为镜像）。
	textObj, ok := req.Extra["text"].(map[string]any)
	if !ok {
		t.Fatalf("extra[text] 缺失，response_format 未翻译：%+v", req.Extra)
	}
	format, ok := textObj["format"].(map[string]any)
	if !ok || format["type"] != "json_schema" || format["name"] != "out" || format["strict"] != true {
		t.Fatalf("text.format 摊平外壳失败：%+v", textObj)
	}
	if _, doubled := format["json_schema"]; doubled {
		t.Fatalf("text.format 不该保留 chat 的嵌套外壳：%+v", format)
	}
	// tool_choice：嵌套 function 摊平为 {"type":"function","name"}。
	tc, ok := req.Extra["tool_choice"].(map[string]any)
	if !ok || tc["type"] != "function" || tc["name"] != "weather" {
		t.Fatalf("tool_choice 未摊平：%+v", req.Extra["tool_choice"])
	}
	// parallel_tool_calls 直传。
	if v, ok := req.Extra["parallel_tool_calls"].(bool); !ok || v != false {
		t.Fatalf("parallel_tool_calls 未透传：%+v", req.Extra["parallel_tool_calls"])
	}
	// penalty / seed / user：Responses 无对应物，丢弃（软偏好，注释留痕即可）。
	for _, k := range []string{"presence_penalty", "frequency_penalty", "seed", "user"} {
		if _, exists := req.Extra[k]; exists {
			t.Fatalf("字段 %s 应被丢弃（无对应物），却出现在 Extra 里", k)
		}
	}
}

// chat → openai-responses：json_object 直映。
func TestApplyProtocolPrivateExtra_ResponsesJsonObject(t *testing.T) {
	got := decodeChat(t, `{"model":"m","messages":[{"role":"user","content":"hi"}],
		"response_format":{"type":"json_object"},"tool_choice":"none"}`)
	req := got.ToRosetta()
	got.ApplyProtocolPrivateExtra(req, "openai-responses")

	textObj, ok := req.Extra["text"].(map[string]any)
	if !ok {
		t.Fatalf("extra[text] 缺失：%+v", req.Extra)
	}
	format, ok := textObj["format"].(map[string]any)
	if !ok || format["type"] != "json_object" {
		t.Fatalf("json_object 应直映为 text.format：%+v", textObj)
	}
	// 字符串三档两协议同名："none" 原样直取。
	if req.Extra["tool_choice"] != "none" {
		t.Fatalf("tool_choice none 应原样直取：%+v", req.Extra["tool_choice"])
	}
}

// chat → anthropic：tool_choice 等价翻译；其余字段无对应物丢弃。
func TestApplyProtocolPrivateExtra_AnthropicTranslation(t *testing.T) {
	cases := []struct {
		name       string
		toolChoice string
		want       any    // 期望的 Extra["tool_choice"]；nil = 丢弃
		wantJSON   string // want 非 nil 时用于深比较
	}{
		{"auto→auto", `"auto"`, map[string]any{"type": "auto"}, `{"type":"auto"}`},
		{"required→any", `"required"`, map[string]any{"type": "any"}, `{"type":"any"}`},
		// json.Marshal 对 map 按键名排序输出，这里按同规则写期望串。
		{"named fn→tool", `{"type":"function","function":{"name":"weather"}}`, map[string]any{"type": "tool", "name": "weather"}, `{"name":"weather","type":"tool"}`},
		// "none" 在 Anthropic 无对应物（无法表达「工具照常声明但本轮禁用」），丢弃。
		{"none dropped", `"none"`, nil, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := decodeChat(t, `{"model":"m","messages":[{"role":"user","content":"hi"}],
				"tool_choice":`+c.toolChoice+`}`)
			req := got.ToRosetta()
			got.ApplyProtocolPrivateExtra(req, "anthropic")
			if c.want == nil {
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

	// response_format / penalty / seed / user / parallel_tool_calls：anthropic 无
	// 对应物，全部丢弃（response_format 的硬约束在网关层拒绝，不放行后丢失）。
	got := decodeChat(t, `{"model":"m","messages":[{"role":"user","content":"hi"}],
		"response_format":{"type":"json_object"},"presence_penalty":0.5,
		"seed":1,"user":"u","parallel_tool_calls":true}`)
	req := got.ToRosetta()
	got.ApplyProtocolPrivateExtra(req, "anthropic")
	if len(req.Extra) != 0 {
		t.Fatalf("anthropic 上游不应收到这些字段：%+v", req.Extra)
	}
}

// openai-chat 直传路径不受影响：""/auto 与显式 openai-chat 同组。
func TestApplyProtocolPrivateExtra_OpenAIChatPassthrough(t *testing.T) {
	for _, proto := range []string{"", "openai-chat", "auto"} {
		got := decodeChat(t, `{"model":"m","messages":[{"role":"user","content":"hi"}],
			"response_format":{"type":"json_object"},"tool_choice":"auto","seed":7}`)
		req := got.ToRosetta()
		got.ApplyProtocolPrivateExtra(req, proto)
		if _, ok := req.Extra["response_format"]; !ok {
			t.Fatalf("protocol=%q: response_format 应原样直传", proto)
		}
		if req.Extra["tool_choice"] != "auto" {
			t.Fatalf("protocol=%q: tool_choice 应原样直传：%+v", proto, req.Extra["tool_choice"])
		}
		// seed 存的是 *int 解引用后的 int（不是 JSON round-trip 的 float64）。
		if v, ok := req.Extra["seed"].(int); !ok || v != 7 {
			t.Fatalf("protocol=%q: seed 应直传为 int 7：%+v", proto, req.Extra["seed"])
		}
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
