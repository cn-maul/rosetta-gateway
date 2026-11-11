package ingress

import (
	"testing"

	"github.com/cn-maul/rosetta"

	"github.com/cn-maul/rosetta-gateway/internal/dialect"
)

// 这一组用例的期望值全部来自**厂商官方文档**，不是实测猜测。每条都注明了
// 出处，因为方言表最危险的失效方式就是「按记忆改」，改完没人能看出错在哪。

// Responses 入口的关闭意图必须被识别出来。
//
// 官方文档：OpenAI Responses 用 reasoning.effort；DeepSeek 的 Responses 格式
// 写的是 {"reasoning":{"effort":"none/low/high/max"}}，并注明 none 关闭思考。
// 只登记 RawEffort 不登记 ThinkingOff，跨协议时 chat/anthropic 目标就收不到
// 任何关闭字段 —— 上游照默认开思考，而客户端以为已经关了。
func TestWire_ResponsesNoneIsAnOffIntentOnChatTarget(t *testing.T) {
	got := probeWireModel(t, NewResponses(),
		`{"model":"m","input":"hi","reasoning":{"effort":"none"}}`,
		rosetta.ProtoOpenAIChat, "/chat", modelWith(dialect.Qwen, "qwen3.8-max"))

	// Qwen 的关闭写法是 enable_thinking=false（官方「深度思考」页），
	// 且不能同时下发 reasoning_effort=none。
	if got["enable_thinking"] != false {
		t.Fatalf("qwen 应收到 enable_thinking=false，实际 %v", got["enable_thinking"])
	}
	if got["reasoning_effort"] != nil {
		t.Fatalf("关闭时不该同时带 reasoning_effort，实际 %v", got["reasoning_effort"])
	}
	if got["thinking"] != nil {
		t.Fatalf("qwen 不该收到 thinking 对象，实际 %v", got["thinking"])
	}
}

// 同一条 none 落到 DeepSeek 的 chat 目标，要换成 thinking.type=disabled。
func TestWire_ResponsesNoneIsAnOffIntentOnDeepSeek(t *testing.T) {
	got := probeWireModel(t, NewResponses(),
		`{"model":"m","input":"hi","reasoning":{"effort":"none"}}`,
		rosetta.ProtoOpenAIChat, "/chat", modelWith(dialect.DeepSeek, "deepseek-chat"))

	th, _ := got["thinking"].(map[string]any)
	if th == nil || th["type"] != "disabled" {
		t.Fatalf("deepseek 应收到 thinking.type=disabled，实际 %v", got["thinking"])
	}
	if got["reasoning_effort"] != nil {
		t.Fatalf("关闭时不该同时带 reasoning_effort，实际 %v", got["reasoning_effort"])
	}
}

// GLM-5.3 收到 reasoning_effort=none 是 400（官方 thinking-mode 页：
// 「GLM-5.3 GLM-5.3-FLASH 不再支持关闭思考（API 请求中 thinking.type 传
// disabled 将会报错）」，且 5.3 的 effort 只有 max/high/low）。
func TestWire_ResponsesNoneDowngradesOnGLM53(t *testing.T) {
	got := probeWireModel(t, NewResponses(),
		`{"model":"m","input":"hi","reasoning":{"effort":"none"}}`,
		rosetta.ProtoOpenAIChat, "/chat", modelWith(dialect.GLM, "glm-5.3"))

	if got["reasoning_effort"] != "low" {
		t.Fatalf("glm-5.3 应退到 low，实际 %v", got["reasoning_effort"])
	}
}

// Kimi K3 系列的请求 schema 里没有 thinking，只有顶层 reasoning_effort。
//
// 官方 API 参考（platform.kimi.com/docs/api/chat）用 oneOf 区分 K3 与 K2.x：
// KimiK3ChatRequestBase 只声明 messages + reasoning_effort；thinking 在 K2.x
// 的 ChatRequestBase 里。给 K3 发 thinking 是给一个不接受的 schema 塞字段。
func TestWire_KimiK3DropsClientThinkingObject(t *testing.T) {
	got := probeWireModel(t, NewOpenAIChat(),
		`{"model":"m","messages":[{"role":"user","content":"hi"}],"thinking":{"type":"enabled","keep":"all"},"reasoning_effort":"high"}`,
		rosetta.ProtoOpenAIChat, "/chat", modelWith(dialect.Kimi, "kimi-k3.1-pro"))

	if got["thinking"] != nil {
		t.Fatalf("kimi-k3 不该收到 thinking，实际 %v", got["thinking"])
	}
	if got["reasoning_effort"] != "high" {
		t.Fatalf("kimi-k3 应保留 reasoning_effort=high，实际 %v", got["reasoning_effort"])
	}
}

// K2.x 反过来：只认 thinking，reasoning_effort 官方标注「不支持」。
func TestWire_KimiK26UsesThinkingNotEffort(t *testing.T) {
	got := probeWireModel(t, NewOpenAIChat(),
		`{"model":"m","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"high"}`,
		rosetta.ProtoOpenAIChat, "/chat", modelWith(dialect.Kimi, "kimi-k2.6"))

	if got["reasoning_effort"] != nil {
		t.Fatalf("kimi-k2.6 不该收到 reasoning_effort，实际 %v", got["reasoning_effort"])
	}
}

// K3 走 Anthropic 兼容端点时同样不发明 thinking：官方只保证原生 Chat 的
// 字段表，强度和开关都按 Anthropic 兼容层自己的口径表达。
func TestWire_KimiK3AnthropicTargetHasNoThinkingSwitch(t *testing.T) {
	got := probeWireModel(t, NewAnthropicMessages(),
		`{"model":"m","max_tokens":1024,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"disabled"}}`,
		rosetta.ProtoAnthropic, "/anth", modelWith(dialect.Kimi, "kimi-k3.1-pro"))

	if got["thinking"] != nil {
		t.Fatalf("kimi-k3 的 anthropic 目标不该收到 thinking，实际 %v", got["thinking"])
	}
	cfg, _ := got["output_config"].(map[string]any)
	if cfg == nil || cfg["effort"] != "low" {
		t.Fatalf("应退到最低档 output_config.effort=low，实际 %v", got["output_config"])
	}
}

// MiniMax 官方 schema 的 thinking.type 只有 adaptive / disabled；
// adaptive 是 M3.1-Flash 唯一接受的取值，enabled 不在枚举里。
func TestWire_MiniMaxEnabledBecomesAdaptive(t *testing.T) {
	// 客户端用 effort 表达「开启」时，网关补出的开关必须是 adaptive。
	got := probeWireModel(t, NewOpenAIChat(),
		`{"model":"m","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"high"}`,
		rosetta.ProtoOpenAIChat, "/chat", modelWith(dialect.MiniMax, "MiniMax-M3.1-Flash"))
	th, _ := got["thinking"].(map[string]any)
	if th == nil || th["type"] != "adaptive" {
		t.Fatalf("minimax 开启态应为 thinking.type=adaptive，实际 %v", got["thinking"])
	}
	// 客户端原样带来的 enabled 也要收敛，不能把枚举外的值发出去。
	got = probeWireModel(t, NewOpenAIChat(),
		`{"model":"m","messages":[{"role":"user","content":"hi"}],"thinking":{"type":"enabled"}}`,
		rosetta.ProtoOpenAIChat, "/chat", modelWith(dialect.MiniMax, "MiniMax-M3.1-Flash"))
	th, _ = got["thinking"].(map[string]any)
	if th == nil || th["type"] != "adaptive" {
		t.Fatalf("minimax 的 enabled 应收敛成 adaptive，实际 %v", got["thinking"])
	}
}

// DeepSeek 官方映射表把 ultra 列为最高一档并写明 ultra → max。
// 认不出 ultra 会让 Thinking 整个消失（客户端要最强档、上游完全不思考）。
func TestWire_UltraMapsToMax(t *testing.T) {
	got := probeWireModel(t, NewOpenAIChat(),
		`{"model":"m","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"ultra"}`,
		rosetta.ProtoOpenAIChat, "/chat", modelWith(dialect.DeepSeek, "deepseek-chat"))

	if got["reasoning_effort"] != "max" {
		t.Fatalf("ultra 应落成 max，实际 %v", got["reasoning_effort"])
	}
}
