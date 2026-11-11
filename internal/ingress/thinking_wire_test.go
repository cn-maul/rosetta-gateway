package ingress

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cn-maul/rosetta"

	"github.com/cn-maul/rosetta-gateway/internal/dialect"
	"github.com/cn-maul/rosetta-gateway/internal/routing"
)

// probeWire decodes an ingress body, applies the same per-attempt steps the
// data plane does, and returns what the fake upstream actually received.
func probeWire(t *testing.T, codec Codec, body string, proto rosetta.Protocol, path string) map[string]any {
	t.Helper()
	return probeWireModel(t, codec, body, proto, path, &routing.UpstreamModel{ModelID: "m"})
}

func probeWireModel(t *testing.T, codec Codec, body string, proto rosetta.Protocol, path string, m *routing.UpstreamModel) map[string]any {
	t.Helper()
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = nil
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.Header().Set("Content-Type", "application/json")
		switch proto {
		case rosetta.ProtoAnthropic:
			_, _ = w.Write([]byte(`{"id":"m1","type":"message","role":"assistant","model":"m","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
		case rosetta.ProtoOpenAIResponses:
			_, _ = w.Write([]byte(`{"id":"r1","object":"response","created_at":1,"status":"completed","model":"m","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`))
		default:
			_, _ = w.Write([]byte(`{"id":"c1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
		}
	}))
	defer srv.Close()

	req, err := http.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	ing, err := codec.Decode(req, 0)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	up := ing.BuildRosetta()
	up.Model = "m"
	ing.ApplyThinkingCapability(up, m, upstreamProtocol(proto))
	ing.ApplyProtocolThinking(up, m, upstreamProtocol(proto))

	c, err := rosetta.NewClient(
		rosetta.WithEndpoint(srv.URL+path),
		rosetta.WithAPIKey("k"),
		rosetta.WithProtocol(proto),
		rosetta.WithExtraOverrideKeys("thinking", "output_config"),
	)
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	if _, err := c.Chat(context.Background(), up); err != nil {
		t.Fatalf("chat: %v (wire=%v)", err, got)
	}
	return got
}

func upstreamProtocol(p rosetta.Protocol) string {
	switch p {
	case rosetta.ProtoAnthropic:
		return "anthropic"
	case rosetta.ProtoOpenAIResponses:
		return "response"
	default:
		return "chat"
	}
}

func modelWith(d dialect.Dialect, id string) *routing.UpstreamModel {
	return &routing.UpstreamModel{ModelID: id, ThinkingDialect: d}
}

// DeepSeek 的关闭写法：thinking 对象，且不能同时带 reasoning_effort。
func TestWire_DeepSeekDisabled(t *testing.T) {
	got := probeWireModel(t, NewOpenAIChat(),
		`{"model":"m","messages":[{"role":"user","content":"hi"}],"thinking":{"type":"disabled"}}`,
		rosetta.ProtoOpenAIChat, "/chat", modelWith(dialect.DeepSeek, "deepseek-chat"))
	th, _ := got["thinking"].(map[string]any)
	if th == nil || th["type"] != "disabled" {
		t.Fatalf("DeepSeek 应收到 thinking.type=disabled，实际 %v", got["thinking"])
	}
	if got["reasoning_effort"] != nil {
		t.Fatalf("disabled 不该同时带 reasoning_effort：%v", got)
	}
}

// Qwen 的关闭写法是 enable_thinking:false，不是 thinking 对象。
func TestWire_QwenDisabledUsesEnableThinking(t *testing.T) {
	got := probeWireModel(t, NewOpenAIChat(),
		`{"model":"m","messages":[{"role":"user","content":"hi"}],"thinking":{"type":"disabled"}}`,
		rosetta.ProtoOpenAIChat, "/chat", modelWith(dialect.Qwen, "qwen3.8-max"))
	if got["enable_thinking"] != false {
		t.Fatalf("Qwen 应收到 enable_thinking=false，实际 %v", got)
	}
	if got["thinking"] != nil {
		t.Fatalf("Qwen 不该收到 thinking 对象：%v", got["thinking"])
	}
}

// GLM-5.3 不能关：绝不能下发 disabled（会 400），只能退到最低档。
func TestWire_GLM53CannotDisable(t *testing.T) {
	got := probeWireModel(t, NewOpenAIChat(),
		`{"model":"m","messages":[{"role":"user","content":"hi"}],"thinking":{"type":"disabled"}}`,
		rosetta.ProtoOpenAIChat, "/chat", modelWith(dialect.GLM, "glm-5.3"))
	if got["thinking"] != nil {
		t.Fatalf("GLM-5.3 不能收到 thinking 对象（disabled 会 400），实际 %v", got["thinking"])
	}
	if got["reasoning_effort"] != "low" {
		t.Fatalf("GLM-5.3 应退到最低档 low，实际 %v", got["reasoning_effort"])
	}
}

// MiniMax-M3.1-Flash 同上，而且它的最低档也是 low。
func TestWire_MiniMaxFlashCannotDisable(t *testing.T) {
	got := probeWireModel(t, NewOpenAIChat(),
		`{"model":"m","messages":[{"role":"user","content":"hi"}],"thinking":{"type":"disabled"}}`,
		rosetta.ProtoOpenAIChat, "/chat", modelWith(dialect.MiniMax, "MiniMax-M3.1-Flash"))
	if got["thinking"] != nil {
		t.Fatalf("MiniMax-M3.1-Flash 不能收到 disabled，实际 %v", got["thinking"])
	}
	if got["reasoning_effort"] != "low" {
		t.Fatalf("应退到 low，实际 %v", got["reasoning_effort"])
	}
}

// 中转站归一化写法后，文档表里「不能关」的模型其实能关。管理员按实测
// 把它纠正过来之后，必须下发真正的关闭字段 —— 而不是继续退到 low。
//
// 这是真实故障的回归：opencode.ai 上的 glm-5.3 / kimi-k3.1-pro 直连时
// 接受 disabled，网关却按文档表降级，用户点了关闭、上游照旧思考并计费。
func TestWire_GLM53ManualDisableOverride(t *testing.T) {
	yes := true
	m := modelWith(dialect.GLM, "glm-5.3")
	m.CanDisableThinking = &yes
	got := probeWireModel(t, NewOpenAIChat(),
		`{"model":"m","messages":[{"role":"user","content":"hi"}],"thinking":{"type":"disabled"}}`,
		rosetta.ProtoOpenAIChat, "/chat", m)
	th, _ := got["thinking"].(map[string]any)
	if th == nil || th["type"] != "disabled" {
		t.Fatalf("纠正成能关后应下发 thinking.type=disabled，实际 %v", got["thinking"])
	}
	if got["reasoning_effort"] != nil {
		t.Fatalf("关闭时不该同时带 reasoning_effort：%v", got["reasoning_effort"])
	}
}

// 反向也要成立：文档说能关、实测不能关时，必须退回降级而不是硬发 disabled。
func TestWire_ManualCannotDisableOverride(t *testing.T) {
	no := false
	m := modelWith(dialect.DeepSeek, "deepseek-chat")
	m.CanDisableThinking = &no
	got := probeWireModel(t, NewOpenAIChat(),
		`{"model":"m","messages":[{"role":"user","content":"hi"}],"thinking":{"type":"disabled"}}`,
		rosetta.ProtoOpenAIChat, "/chat", m)
	if got["thinking"] != nil {
		t.Fatalf("已声明不能关时不该下发 disabled，实际 %v", got["thinking"])
	}
	if got["reasoning_effort"] != "low" {
		t.Fatalf("应退到最低档 low，实际 %v", got["reasoning_effort"])
	}
}

// 真 GPT：没有 thinking 字段，关闭只能靠 reasoning_effort:none。
func TestWire_OpenAIDisabledUsesEffortNone(t *testing.T) {
	got := probeWireModel(t, NewOpenAIChat(),
		`{"model":"m","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"none"}`,
		rosetta.ProtoOpenAIChat, "/chat", modelWith(dialect.OpenAI, "gpt-5.6"))
	if got["thinking"] != nil {
		t.Fatalf("官方 OpenAI 没有 thinking 字段，实际 %v", got["thinking"])
	}
	if got["reasoning_effort"] != "none" {
		t.Fatalf("应下发 reasoning_effort=none，实际 %v", got["reasoning_effort"])
	}
}

// 开启时保留客户端自己的 thinkng 对象（GLM 的 clear_thinking 等扩展字段）。
func TestWire_ChatThinkingEnabledKeepsVendorFields(t *testing.T) {
	got := probeWireModel(t, NewOpenAIChat(),
		`{"model":"m","messages":[{"role":"user","content":"hi"}],"thinking":{"type":"enabled","clear_thinking":false},"reasoning_effort":"high"}`,
		rosetta.ProtoOpenAIChat, "/chat", modelWith(dialect.GLM, "glm-5.2"))
	th, _ := got["thinking"].(map[string]any)
	if th == nil || th["type"] != "enabled" {
		t.Fatalf("应原样转发 thinking，实际 %v", got["thinking"])
	}
	if th["clear_thinking"] != false {
		t.Fatalf("厂商扩展字段 clear_thinking 必须保留，实际 %v", th)
	}
	if got["reasoning_effort"] != "high" {
		t.Fatalf("强度应保留 high，实际 %v", got["reasoning_effort"])
	}
}

// Anthropic 目标 + 真 Claude：关闭走 thinking.type=disabled。
func TestWire_AnthropicDisabledExplicit(t *testing.T) {
	got := probeWireModel(t, NewAnthropicMessages(),
		`{"model":"m","max_tokens":1024,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"disabled"}}`,
		rosetta.ProtoAnthropic, "/anthropic", modelWith(dialect.Anthropic, "claude-sonnet-5-5"))
	th, _ := got["thinking"].(map[string]any)
	if th == nil || th["type"] != "disabled" {
		t.Fatalf("anthropic 目标应显式下发 thinking.type=disabled，实际 %v", got["thinking"])
	}
}

// Anthropic 目标 + DeepSeek：必须改用 output_config.effort，
// 因为它的 /anthropic 端点忽略 budget_tokens。
func TestWire_AnthropicVendorUsesOutputConfigEffort(t *testing.T) {
	got := probeWireModel(t, NewAnthropicMessages(),
		`{"model":"m","max_tokens":4096,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"enabled","budget_tokens":8192}}`,
		rosetta.ProtoAnthropic, "/anthropic", modelWith(dialect.DeepSeek, "deepseek-chat"))
	cfg, _ := got["output_config"].(map[string]any)
	if cfg == nil || cfg["effort"] == "" {
		t.Fatalf("DeepSeek 的 anthropic 端点应收到 output_config.effort，实际 %v", got["output_config"])
	}
	th, _ := got["thinking"].(map[string]any)
	if th != nil {
		if _, hasBudget := th["budget_tokens"]; hasBudget {
			t.Fatalf("不该再发被忽略的 budget_tokens，实际 %v", th)
		}
	}
}

// Responses 目标：关闭统一是 reasoning.effort=none。
func TestWire_ResponsesReasoningNone(t *testing.T) {
	got := probeWireModel(t, NewResponses(),
		`{"model":"m","input":"hi","reasoning":{"effort":"none"}}`,
		rosetta.ProtoOpenAIResponses, "/responses", modelWith(dialect.DeepSeek, "deepseek-chat"))
	rs, _ := got["reasoning"].(map[string]any)
	if rs == nil || rs["effort"] != "none" {
		t.Fatalf("responses 目标应下发 reasoning.effort=none，实际 %v", got["reasoning"])
	}
}

// 认不出方言时不能瞎发明厂商字段，但关闭意图仍要表达（用官方 none）。
func TestWire_UnknownDialectDoesNotInventVendorFields(t *testing.T) {
	got := probeWireModel(t, NewOpenAIChat(),
		`{"model":"m","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"none"}`,
		rosetta.ProtoOpenAIChat, "/chat", modelWith(dialect.Auto, "my-relay-model"))
	if got["thinking"] != nil {
		t.Fatalf("方言未知时不该发明 thinking 字段，实际 %v", got["thinking"])
	}
	if got["reasoning_effort"] != "none" {
		t.Fatalf("应使用官方 none，实际 %v", got["reasoning_effort"])
	}
}

// 方言未知 + 厂商标注「始终思考」：绝不能发 none。GLM 官方收到它会返回
// 1210「该模型始终思考，不支持关闭思考」，线上就是一次 400。
//
// 这是 passthrough「保证不把请求打挂」承诺的真实反例：它只保证不发明
// 厂商私有字段，但 none 本身对这类模型同样是非法值。
//
// 用「中转改名」的形态构造：名字里没有 glm/minimax/kimi 可供推断，但
// 仍命中 forcedThinking 的后缀规则 —— 这正是中转站改过名的真实场景。
func TestWire_UnknownDialectSkipsOffSwitchForForcedThinkingModel(t *testing.T) {
	for _, model := range []string{"relay-glm-5.3", "vendor-m3.1-flash", "x/kimi-k3"} {
		t.Run(model, func(t *testing.T) {
			m := modelWith(dialect.Passthrough, model)
			got := probeWireModel(t, NewOpenAIChat(),
				`{"model":"m","messages":[{"role":"user","content":"hi"}],"thinking":{"type":"disabled"}}`,
				rosetta.ProtoOpenAIChat, "/chat", m)
			if got["reasoning_effort"] != nil {
				t.Fatalf("%s 不该收到 reasoning_effort，实际 %v", model, got["reasoning_effort"])
			}
			if got["thinking"] != nil {
				t.Fatalf("%s 不该收到 thinking，实际 %v", model, got["thinking"])
			}
		})
	}
}

// 同名模型在 Anthropic 入口同样不能硬发 disabled。
func TestWire_UnknownDialectAnthropicSkipsDisabledForForcedThinkingModel(t *testing.T) {
	got := probeWireModel(t, NewAnthropicMessages(),
		`{"model":"m","max_tokens":1024,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"disabled"}}`,
		rosetta.ProtoAnthropic, "/anthropic", modelWith(dialect.Passthrough, "relay-glm-5.3"))
	if got["thinking"] != nil {
		t.Fatalf("relay-glm-5.3 的 anthropic 入口不该收到 disabled，实际 %v", got["thinking"])
	}
}

// 非法 thinking.type 必须在 decode 期 400，而不是静默 200。
func TestWire_ChatThinkingBogusRejected(t *testing.T) {
	req, _ := http.NewRequest(http.MethodPost, "/",
		strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}],"thinking":{"type":"bogus"}}`))
	if _, err := (NewOpenAIChat()).Decode(req, 0); err == nil {
		t.Fatal("thinking.type=bogus 应被拒")
	} else if !strings.Contains(err.Error(), "thinking") {
		t.Fatalf("错误应指向 thinking，实际 %v", err)
	}
}

// 关思考不能顺手把 include 抹掉。
//
// include 在 rosetta 里挂在 Thinking 上，所以「把 Thinking 换成 none」这种
// 直观写法会丢字段：Codex 一类客户端靠
// include:["reasoning.encrypted_content"] 拿回可离线续接的加密思考，
// 这里关掉的应该是「本轮思考」，不是「以后都别再返回思考包裹」。
func TestWire_ResponsesThinkingOffKeepsInclude(t *testing.T) {
	got := probeWireModel(t, NewResponses(),
		`{"model":"m@thinking:off","input":"hi",`+
			`"include":["reasoning.encrypted_content"],"reasoning":{"effort":"low"}}`,
		rosetta.ProtoOpenAIResponses, "/responses", modelWith(dialect.DeepSeek, "deepseek-chat"))
	if r, ok := got["reasoning"].(map[string]any); !ok || r["effort"] != "none" {
		t.Fatalf("reasoning.effort 应为 none，实际 %v", got["reasoning"])
	}
	inc, ok := got["include"].([]any)
	if !ok || len(inc) != 1 || inc[0] != "reasoning.encrypted_content" {
		t.Fatalf("include 被抹掉了：%v", got["include"])
	}
}

// Responses 入口同样不能对「关不掉」的模型硬发 none：GLM-5.3 收到它会 400。
// 应退到它能接受的最低档，而不是把请求打挂。
func TestWire_ResponsesCannotDisableDowngrades(t *testing.T) {
	got := probeWireModel(t, NewResponses(),
		`{"model":"m@thinking:off","input":"hi"}`,
		rosetta.ProtoOpenAIResponses, "/responses", modelWith(dialect.GLM, "glm-5.3"))
	r, ok := got["reasoning"].(map[string]any)
	if !ok {
		t.Fatalf("应下发 reasoning，实际 %v", got["reasoning"])
	}
	if r["effort"] != "low" {
		t.Fatalf("glm-5.3 应降级到 low，实际 %v", r["effort"])
	}
}

// Responses 入口的档位也要按方言翻译：GLM-5.3 收到 medium 直接 400。
func TestWire_ResponsesEffortTranslatedPerDialect(t *testing.T) {
	got := probeWireModel(t, NewResponses(),
		`{"model":"m","input":"hi","reasoning":{"effort":"medium"}}`,
		rosetta.ProtoOpenAIResponses, "/responses", modelWith(dialect.GLM, "glm-5.3"))
	r, ok := got["reasoning"].(map[string]any)
	if !ok || r["effort"] != "high" {
		t.Fatalf("glm-5.3 的 medium 应翻成 high，实际 %v", got["reasoning"])
	}
}
