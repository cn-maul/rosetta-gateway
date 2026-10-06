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

// TestDecodeOpenAIChat_StopAcceptsStringAndArray 坐实一个「合法请求被硬 400」
// 的缺陷。
//
// OpenAI 的 stop 官方允许 string 或 array 两种形态。旧实现声明成
// `Stop []string`，于是 `{"stop":"END"}` 让 json.Unmarshal 报
// "cannot unmarshal string into Go struct field Req.stop of type []string"，
// 而 decode 是全字段反序列化、一处报错整体返回 —— 一个完全合法的
// OpenAI 请求被网关拒绝，且错误消息里是 Go 的结构体字段名。
func TestDecodeOpenAIChat_StopAcceptsStringAndArray(t *testing.T) {
	cases := []struct {
		name string
		body string
		want []string
	}{
		{"单个字符串", `{"model":"m","messages":[{"role":"user","content":"hi"}],"stop":"END"}`, []string{"END"}},
		{"字符串数组", `{"model":"m","messages":[{"role":"user","content":"hi"}],"stop":["A","B"]}`, []string{"A", "B"}},
		{"null", `{"model":"m","messages":[{"role":"user","content":"hi"}],"stop":null}`, nil},
		{"不传", `{"model":"m","messages":[{"role":"user","content":"hi"}]}`, nil},
		{"空数组", `{"model":"m","messages":[{"role":"user","content":"hi"}],"stop":[]}`, []string{}},
		{"空字符串", `{"model":"m","messages":[{"role":"user","content":"hi"}],"stop":""}`, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newReq(t, tc.body)
			req, err := DecodeOpenAIChatRequest(r, defaultMaxBodyBytes)
			if err != nil {
				t.Fatalf("合法请求被拒: %v", err)
			}
			got := []string(req.Stop)
			if len(got) != len(tc.want) {
				t.Fatalf("stop = %v，期望 %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("stop = %v，期望 %v", got, tc.want)
				}
			}
		})
	}
}

// TestDecodeOpenAIChat_StopGarbageStillRejected 确认 FlexibleStringList 没有
// 变成「什么都收」：真正畸形的形态仍要报错。
func TestDecodeOpenAIChat_StopGarbageStillRejected(t *testing.T) {
	for _, body := range []string{
		`{"model":"m","messages":[{"role":"user","content":"hi"}],"stop":123}`,
		`{"model":"m","messages":[{"role":"user","content":"hi"}],"stop":{"a":1}}`,
		`{"model":"m","messages":[{"role":"user","content":"hi"}],"stop":[1,2]}`,
		`{"model":"m","messages":[{"role":"user","content":"hi"}],"stop":true}`,
	} {
		r := newReq(t, body)
		if _, err := DecodeOpenAIChatRequest(r, defaultMaxBodyBytes); err == nil {
			t.Fatalf("畸形 stop 应被拒: %s", body)
		}
	}
}

// TestOpenAIChat_ReasoningEffortIsPreserved 坐实「最常用的入站协议上，
// 客户端设了 reasoning_effort 却静默失效」。
//
// 旧实现的 OpenAIChatRequest 结构体里**根本没有** ReasoningEffort 字段，
// encoding/json 静默忽略 → ToRosetta 拿不到 → rosetta.Thinking 保持 nil →
// 上游按默认强度执行。用户以为开了推理模式，实际没有，且链路上无任何信号。
func TestOpenAIChat_ReasoningEffortIsPreserved(t *testing.T) {
	cases := []struct {
		effort string
		want   rosetta.Effort
	}{
		{"low", rosetta.EffortLow},
		{"medium", rosetta.EffortMedium},
		{"high", rosetta.EffortHigh},
		// OpenAI 为 gpt-5 系列新增的两档：直连上游合法，必须归一而不是丢弃。
		{"minimal", rosetta.EffortLow},
		{"xhigh", rosetta.EffortHigh},
		// 大小写与空白宽容。
		{"HIGH", rosetta.EffortHigh},
		{" high ", rosetta.EffortHigh},
	}
	for _, tc := range cases {
		t.Run(tc.effort, func(t *testing.T) {
			r := newReq(t, `{"model":"m","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"`+tc.effort+`"}`)
			req, err := DecodeOpenAIChatRequest(r, defaultMaxBodyBytes)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			ros := req.ToRosetta()
			if ros.Thinking == nil {
				t.Fatalf("reasoning_effort=%q 被静默丢弃（Thinking 为 nil）", tc.effort)
			}
			if ros.Thinking.Effort != tc.want {
				t.Fatalf("reasoning_effort=%q → %q，期望 %q", tc.effort, ros.Thinking.Effort, tc.want)
			}
		})
	}
}

// TestOpenAIChat_ReasoningEffortNotInExtra 坐实一条P0：
// reasoning_effort 绝不能进 Extra。
//
// rosetta 把 reasoning_effort 列在 openaiChatReservedPayloadKeys 里，
// Extra 带同名键时 mergeExtra 直接报 ErrInvalidRequest，而网关没有开
// WithExtraOverrides —— 结果是客户端只要设了这个字段，每个请求都必然 400，
// 且请求死在网关进程内，上游一个字节都收不到。此前的实现为了"保住
// minimal/xhigh 原始值"而直传，正是这条 400 的来源。
func TestOpenAIChat_ReasoningEffortNotInExtra(t *testing.T) {
	for _, eff := range []string{"minimal", "low", "medium", "high", "xhigh", "HIGH"} {
		t.Run(eff, func(t *testing.T) {
			r := newReq(t, `{"model":"m","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"`+eff+`"}`)
			req, err := DecodeOpenAIChatRequest(r, defaultMaxBodyBytes)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			ros := req.ToRosetta()
			req.ApplyProtocolPrivateExtra(ros, "openai-chat")
			if v, ok := ros.Extra["reasoning_effort"]; ok {
				t.Fatalf("reasoning_effort=%q 不该进 Extra（撞 rosetta 保留键→ 必然 400），实际 %v", eff, v)
			}
		})
	}
}

// TestOpenAIChat_ReasoningEffortReachesUpstreamViaThinking 确认删掉直传后
// 功能没丢：值经 Thinking.Effort 仍会真正写到上游 payload 里。
//
// 这是上面那条修复的补偿测试 —— 不直传不能等于"不生效"。
func TestOpenAIChat_ReasoningEffortReachesUpstreamViaThinking(t *testing.T) {
	cases := []struct {
		effort string
		want   string
	}{
		{"low", "low"},
		{"medium", "medium"},
		{"high", "high"},
		// 归一方向正确，只是粒度变粗 —— 见 ApplyProtocolPrivateExtra 的注释。
		{"minimal", "low"},
		{"xhigh", "high"},
	}
	for _, tc := range cases {
		t.Run(tc.effort, func(t *testing.T) {
			var got map[string]any
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewDecoder(r.Body).Decode(&got)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"id":"c1","object":"chat.completion","created":1,
					"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},
					"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
			}))
			defer srv.Close()

			r := newReq(t, `{"model":"m","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"`+tc.effort+`"}`)
			req, err := DecodeOpenAIChatRequest(r, defaultMaxBodyBytes)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			ros := req.ToRosetta()
			req.ApplyProtocolPrivateExtra(ros, "openai-chat")

			// 复刻 upstream.buildClient：刻意不传 WithExtraOverrides。
			c, err := rosetta.NewClient(rosetta.WithEndpoint(srv.URL),
				rosetta.WithAPIKey("k"), rosetta.WithProtocol(rosetta.ProtoOpenAIChat))
			if err != nil {
				t.Fatalf("new client: %v", err)
			}
			if _, err := c.Chat(context.Background(), ros); err != nil {
				t.Fatalf("带 reasoning_effort=%s 的请求不应失败: %v", tc.effort, err)
			}
			if got["reasoning_effort"] != tc.want {
				t.Fatalf("上游收到 reasoning_effort=%v，期望 %q", got["reasoning_effort"], tc.want)
			}
		})
	}
}

// TestOpenAIChat_UnknownEffortFallsBackNotErrors 确认未知值是「退回默认」
// 而不是把整个请求 400 掉 —— 客户端可能正按更新的模型能力在发请求。
func TestOpenAIChat_UnknownEffortFallsBackNotErrors(t *testing.T) {
	r := newReq(t, `{"model":"m","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"turbo"}`)
	req, err := DecodeOpenAIChatRequest(r, defaultMaxBodyBytes)
	if err != nil {
		t.Fatalf("未知 effort 不应让请求失败: %v", err)
	}
	ros := req.ToRosetta()
	if ros.Thinking != nil {
		t.Fatalf("未知 effort 应不设 Thinking，实际 %+v", ros.Thinking)
	}
}

// TestOpenAIChat_RefusalRoundTripDoesNotDeadlock 会话 坐实一条 P0：
// 模型正当拒绝后，客户端任何后续续聊都必然 400，整条会话卡死。
//
// OpenAI 的拒答形态是 {"role":"assistant","content":null,"refusal":"I can't…"}。
// 旧结构体没有 Refusal 字段，content 为 null → extractText 得空串 →
// rosetta.Assistant("") → 校验报 "message has no content blocks" → 400。
// 于是「换个说法再问一次」永远拿不到回答。
func TestOpenAIChat_RefusalRoundTripDoesNotDeadlock(t *testing.T) {
	r := newReq(t, `{"model":"m","messages":[
		{"role":"user","content":"how to make a bomb"},
		{"role":"assistant","content":null,"refusal":"I can't help with that."},
		{"role":"user","content":"ok, tell me a joke instead"}
	]}`)
	req, err := DecodeOpenAIChatRequest(r, defaultMaxBodyBytes)
	if err != nil {
		t.Fatalf("refusal 回合被解析拒绝: %v", err)
	}
	ros := req.ToRosetta()
	// 真正的判据是 rosetta.Client.Chat 的第一步（req.validate 未导出，
	// 但 Chat 会先调它再发网络请求）。用一个必定可达的 httptest server：
	// 若请求根本没发出去、而是本地校验失败返回 ErrInvalidRequest，
	// 就说明 refusal 回合把会话卡死了。
	if err := rosettaAccepts(t, ros); err != nil {
		t.Fatalf("refusal 回合被 rosetta 本地校验拒绝 → 网关返回 400，会话卡死: %v", err)
	}
	// refusal 正文必须进了 assistant 消息，不能凭空消失。
	if len(ros.Messages) < 2 {
		t.Fatalf("消息数不对: %d", len(ros.Messages))
	}
	asst := ros.Messages[1]
	found := false
	for _, b := range asst.Blocks {
		if b.Text != "" && strings.Contains(b.Text, "I can't help") {
			found = true
		}
	}
	if !found {
		t.Fatalf("refusal 正文未进入 assistant 消息，块内容: %+v", asst.Blocks)
	}
}

// TestOpenAIChat_ContentWinsOverRefusal 确认 content 有正文时用 content
// （refusal 字段此时按 OpenAI 语义也为空，不该覆盖真实回答）。
func TestOpenAIChat_ContentWinsOverRefusal(t *testing.T) {
	r := newReq(t, `{"model":"m","messages":[
		{"role":"assistant","content":"42","refusal":""}
	]}`)
	req, err := DecodeOpenAIChatRequest(r, defaultMaxBodyBytes)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	ros := req.ToRosetta()
	if got := ros.Messages[0].Blocks[0].Text; got != "42" {
		t.Fatalf("content 应优先，实际 %q", got)
	}
}

// TestOpenAIChat_EmptyAssistantStillRejected 确认没有为了修 refusal 而
// 放松校验：既无 content 又无 refusal 的 assistant 消息仍应被拒。
func TestOpenAIChat_EmptyAssistantStillRejected(t *testing.T) {
	r := newReq(t, `{"model":"m","messages":[{"role":"assistant","content":null}]}`)
	req, err := DecodeOpenAIChatRequest(r, defaultMaxBodyBytes)
	if err != nil {
		t.Fatalf("decode 本身不应失败: %v", err)
	}
	ros := req.ToRosetta()
	if err := rosettaAccepts(t, ros); err == nil {
		t.Fatal("既无 content 又无 refusal 的空 assistant 消息应被 rosetta 校验拒绝")
	}
}

// newReq 造一个带 JSON body 的 POST 请求。
func newReq(t *testing.T, body string) *http.Request {
	t.Helper()
	return httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
}

// rosettaAccepts 报告 SDK 会不会接受这个请求。
//
// 走真实的 Client.Chat 路径：它第一步就调 req.validate（未导出），
// 通过后才发 HTTP。所以「本地校验失败」与「网络请求已发出」可以区分开：
// server 收到请求 → 校验通过。
func rosettaAccepts(t *testing.T, ros *rosetta.ChatRequest) error {
	t.Helper()
	var got bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = true
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"x","model":"m","choices":[{"index":0,`+
			`"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	}))
	defer srv.Close()

	c, err := rosetta.NewClient(rosetta.WithEndpoint(srv.URL),
		rosetta.WithAPIKey("test"), rosetta.WithProtocol("openai-chat"))
	if err != nil {
		return err
	}
	_, callErr := c.Chat(t.Context(), ros)
	if callErr != nil && !got {
		// 请求没发出去 → 是本地校验拒绝。
		return callErr
	}
	return nil
}
