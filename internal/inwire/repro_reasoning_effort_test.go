package inwire

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cn-maul/rosetta"
)

// 本文件是「客户端带 reasoning_effort 就必然 400」这条 P0 的回归防线。
//
// 故障链：ToRosetta 把 reasoning_effort 归一到 Thinking.Effort，
// ApplyProtocolPrivateExtra 又把原始值塞进 Extra；rosetta 的
// openaiChatReservedPayloadKeys 含 reasoning_effort，mergeExtra 撞键即
// ErrInvalidRequest；网关 buildClient 未开 WithExtraOverrides，于是错误被
// outwire.MapUpstreamError 映射成 400 invalid_request_error 回显给客户端。
// 请求死在网关进程内，上游从未被触达。
//
// 修复：不再把 reasoning_effort 放进 Extra。下面每个用例都从不同角度
// 锁住「这条路通了」这个事实。

// upstreamRec 是记录请求体的假上游。
func upstreamRec(t *testing.T, got *map[string]any) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		*got = body
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c1","object":"chat.completion","created":1,
			"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},
			"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
}

// gwClient 复刻 internal/upstream.buildClient。刻意不传
// WithExtraOverrides(true) —— 那正是线上网关的行为，任何时候都不该靠它
// 让请求通过。
func gwClient(t *testing.T, endpoint string) *rosetta.Client {
	t.Helper()
	c, err := rosetta.NewClient(
		rosetta.WithEndpoint(endpoint),
		rosetta.WithAPIKey("k"),
		rosetta.WithProtocol(rosetta.ProtoOpenAIChat),
	)
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	return c
}

// decodeToRosetta 跑完「入站 decode → ToRosetta → ApplyProtocolPrivateExtra」，
// 与真实请求路径一致。
func decodeToRosetta(t *testing.T, body, protocol string) *rosetta.ChatRequest {
	t.Helper()
	req, err := DecodeOpenAIChatRequest(newReq(t, body), defaultMaxBodyBytes)
	if err != nil {
		t.Fatalf("入站 decode 应成功: %v", err)
	}
	ros := req.ToRosetta()
	req.ApplyProtocolPrivateExtra(ros, protocol)
	return ros
}

// TestRegression_ReasoningEffortNoLonger400 是核心回归用例：
// 带 reasoning_effort 的请求必须成功打到上游，且带上归一后的 effort。
func TestRegression_ReasoningEffortNoLonger400(t *testing.T) {
	var got map[string]any
	srv := upstreamRec(t, &got)
	defer srv.Close()

	ros := decodeToRosetta(t,
		`{"model":"m","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"high"}`,
		"openai-chat")

	if _, ok := ros.Extra["reasoning_effort"]; ok {
		t.Fatalf("reasoning_effort 不该出现在 Extra 里：%v", ros.Extra)
	}

	if _, err := gwClient(t, srv.URL).Chat(context.Background(), ros); err != nil {
		t.Fatalf("带 reasoning_effort 的请求不应再 400: %v", err)
	}
	if got["reasoning_effort"] != "high" {
		t.Fatalf("上游收到 reasoning_effort=%v，期望 \"high\"", got["reasoning_effort"])
	}
}

// TestRegression_AllEffortValuesPass 确认所有取值都不再触发 400 ——
// 修复前 minimal/low/medium/high/xhigh/HIGH 一律失败。
func TestRegression_AllEffortValuesPass(t *testing.T) {
	for _, eff := range []string{"minimal", "low", "medium", "high", "xhigh", "HIGH", " high "} {
		t.Run(eff, func(t *testing.T) {
			var got map[string]any
			srv := upstreamRec(t, &got)
			defer srv.Close()

			ros := decodeToRosetta(t,
				`{"model":"m","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"`+eff+`"}`,
				"openai-chat")

			if _, err := gwClient(t, srv.URL).Chat(context.Background(), ros); err != nil {
				t.Fatalf("effort=%s 不应失败: %v", eff, err)
			}
			if got["reasoning_effort"] == nil {
				t.Fatalf("effort=%s 应由 Thinking.Effort 写到上游，实际缺失", eff)
			}
		})
	}
}

// TestRegression_UnknownEffortStillNot400 确认修复没有把「未知值」从
// 「退回默认」变成别的行为 —— 它仍不该让请求失败。
func TestRegression_UnknownEffortStillNot400(t *testing.T) {
	var got map[string]any
	srv := upstreamRec(t, &got)
	defer srv.Close()

	ros := decodeToRosetta(t,
		`{"model":"m","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"turbo"}`,
		"openai-chat")

	if ros.Thinking != nil {
		t.Fatalf("未知 effort 不应设 Thinking，实际 %+v", ros.Thinking)
	}
	if _, err := gwClient(t, srv.URL).Chat(context.Background(), ros); err != nil {
		t.Fatalf("未知 effort 不应让请求失败: %v", err)
	}
	if _, ok := got["reasoning_effort"]; ok {
		t.Fatalf("未知 effort 不该写到上游，却收到 %v", got["reasoning_effort"])
	}
}

// TestRegression_OtherExtrasStillForwarded 确认修复没有误伤该函数的本职：
// 其余 OpenAI 私有字段仍需照原样直通上游。
func TestRegression_OtherExtrasStillForwarded(t *testing.T) {
	var got map[string]any
	srv := upstreamRec(t, &got)
	defer srv.Close()

	ros := decodeToRosetta(t, `{"model":"m","messages":[{"role":"user","content":"hi"}],
		"reasoning_effort":"high","seed":42,"user":"u1","parallel_tool_calls":true,
		"presence_penalty":0.1,"frequency_penalty":0.2,"tool_choice":"auto"}`, "openai-chat")

	if _, err := gwClient(t, srv.URL).Chat(context.Background(), ros); err != nil {
		t.Fatalf("请求不应失败: %v", err)
	}
	for k, want := range map[string]any{
		"seed":                float64(42),
		"user":                "u1",
		"parallel_tool_calls": true,
		"presence_penalty":    0.1,
		"frequency_penalty":   0.2,
	} {
		if got[k] != want {
			t.Fatalf("上游 %s=%v，期望 %v", k, got[k], want)
		}
	}
}

// TestRegression_ProtocolsWithoutPassthroughUnaffected 确认 anthropic /
// openai-responses 路径的既有行为没变：这些 OpenAI 私有字段一概不下发。
func TestRegression_ProtocolsWithoutPassthroughUnaffected(t *testing.T) {
	for _, p := range []string{"anthropic", "openai-responses"} {
		t.Run(p, func(t *testing.T) {
			ros := decodeToRosetta(t,
				`{"model":"m","messages":[{"role":"user","content":"hi"}],
				"reasoning_effort":"high","seed":42,"user":"u1"}`, p)
			if len(ros.Extra) != 0 {
				t.Fatalf("protocol=%s 不该产生任何 Extra，实际 %v", p, ros.Extra)
			}
			if ros.Thinking == nil {
				t.Fatalf("protocol=%s 仍应完成 Thinking 归一", p)
			}
		})
	}
}

// TestRegression_ErrorMessageNoLongerLeaksSDKInternals 确认那条把 SDK 内部
// 提示（WithExtraOverrides 等）原样回显给客户端的路径不会再被触发。
//
// 即便将来有人重新把保留键塞进 Extra，这条断言也会先于线上暴露问题。
func TestRegression_ErrorMessageNoLongerLeaksSDKInternals(t *testing.T) {
	var got map[string]any
	srv := upstreamRec(t, &got)
	defer srv.Close()

	ros := decodeToRosetta(t,
		`{"model":"m","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"high"}`,
		"openai-chat")

	_, err := gwClient(t, srv.URL).Chat(context.Background(), ros)
	if err != nil && strings.Contains(err.Error(), "collides with an SDK-managed field") {
		t.Fatalf("SDK 保留键提示泄漏到客户端: %v", err)
	}
	if err != nil {
		t.Fatalf("请求不应失败: %v", err)
	}
}
