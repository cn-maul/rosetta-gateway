package ingress

import (
	"testing"

	"github.com/cn-maul/rosetta"

	"github.com/cn-maul/rosetta-gateway/internal/dialect"
)

const anthropicBody = `{"model":"%s","max_tokens":1024,"messages":[{"role":"user","content":"hi"}]}`

// Anthropic 入口：@thinking:off 必须变成 protocol 标准的 disabled 开关。
func TestWire_AnthropicIngressSuffixOff(t *testing.T) {
	got := probeWireModel(t, NewAnthropicMessages(),
		replaceModel(anthropicBody, "m@thinking:off"),
		rosetta.ProtoAnthropic, "/messages", modelWith(dialect.DeepSeek, "deepseek-chat"))

	th, ok := got["thinking"].(map[string]any)
	if !ok || th["type"] != "disabled" {
		t.Fatalf("thinking.type=disabled expected, got %v", got["thinking"])
	}
}

// Anthropic 入口 + @effort:high：Anthropic 没有 effort 字段，必须落成预算，
// 且预算要够到 high 那一档（32768，与 SDK 一致）。
func TestWire_AnthropicIngressSuffixEffortBecomesBudget(t *testing.T) {
	got := probeWireModel(t, NewAnthropicMessages(),
		replaceModel(anthropicBody, "m@effort:high"),
		rosetta.ProtoAnthropic, "/messages", modelWith(dialect.Anthropic, "claude-sonnet-4-6"))

	th, ok := got["thinking"].(map[string]any)
	if !ok || th["type"] != "enabled" {
		t.Fatalf("thinking.type=enabled expected, got %v", got["thinking"])
	}
	if budget, _ := th["budget_tokens"].(float64); int(budget) != 32768 {
		t.Fatalf("budget_tokens = %v, want 32768 (thinking=%v)", th["budget_tokens"], th)
	}
}

// Responses 入口：@thinking:off 走 reasoning.effort=none。
func TestWire_ResponsesIngressSuffixOff(t *testing.T) {
	got := probeWireModel(t, NewResponses(),
		`{"model":"m@thinking:off","input":"hi"}`,
		rosetta.ProtoOpenAIResponses, "/responses", modelWith(dialect.OpenAI, "gpt-5"))

	reasoning, ok := got["reasoning"].(map[string]any)
	if !ok || reasoning["effort"] != "none" {
		t.Fatalf("reasoning.effort=none expected, got %v", got["reasoning"])
	}
}

func replaceModel(body, model string) string {
	out := make([]byte, 0, len(body))
	for i := 0; i < len(body); i++ {
		if body[i] == '%' && i+1 < len(body) && body[i+1] == 's' {
			out = append(out, model...)
			i++
			continue
		}
		out = append(out, body[i])
	}
	return string(out)
}
