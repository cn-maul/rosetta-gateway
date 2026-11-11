package ingress

import (
	"testing"

	"github.com/cn-maul/rosetta"

	"github.com/cn-maul/rosetta-gateway/internal/dialect"
)

// 请求级 @dialect 必须压过库里的配置：这是「不改后台、只改这一次」的全部意义。
func TestWire_ModelSuffixDialectBeatsPinnedDialect(t *testing.T) {
	got := probeWireModel(t, NewOpenAIChat(),
		`{"model":"m@dialect:qwen","messages":[{"role":"user","content":"hi"}],"thinking":{"type":"disabled"}}`,
		rosetta.ProtoOpenAIChat, "/chat", modelWith(dialect.DeepSeek, "deepseek-chat"))

	if _, ok := got["thinking"]; ok {
		t.Fatalf("qwen override must not send a thinking object: %v", got)
	}
	if got["enable_thinking"] != false {
		t.Fatalf("enable_thinking=false expected, got %v", got["enable_thinking"])
	}
}

// 不带后缀时仍然走库里的配置，后缀不能改变默认行为。
func TestWire_NoSuffixKeepsPinnedDialect(t *testing.T) {
	got := probeWireModel(t, NewOpenAIChat(),
		`{"model":"m","messages":[{"role":"user","content":"hi"}],"thinking":{"type":"disabled"}}`,
		rosetta.ProtoOpenAIChat, "/chat", modelWith(dialect.DeepSeek, "deepseek-chat"))

	th, ok := got["thinking"].(map[string]any)
	if !ok || th["type"] != "disabled" {
		t.Fatalf("deepseek disabled switch expected, got %v", got["thinking"])
	}
}

// @thinking:off 优先于请求体里的 reasoning_effort:high。
func TestWire_ModelSuffixThinkingOffWins(t *testing.T) {
	got := probeWireModel(t, NewOpenAIChat(),
		`{"model":"m@thinking:off","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"high"}`,
		rosetta.ProtoOpenAIChat, "/chat", modelWith(dialect.DeepSeek, "deepseek-chat"))

	if got["reasoning_effort"] != nil {
		// DeepSeek 的关闭写法是 thinking 对象；同时带 reasoning_effort
		// 会自相矛盾，网关必须只留一个。
		t.Fatalf("off suffix must not leave reasoning_effort: %v", got)
	}
	th, ok := got["thinking"].(map[string]any)
	if !ok || th["type"] != "disabled" {
		t.Fatalf("thinking.type=disabled expected, got %v", got["thinking"])
	}
}

// @effort:max 在 GLM-5.3 上必须落成它认识的 max；
// 在 DeepSeek 上 medium 这类它没实现的档要换成 high，而不是原样发。
func TestWire_ModelSuffixEffortIsTranslatedPerDialect(t *testing.T) {
	got := probeWireModel(t, NewOpenAIChat(),
		`{"model":"m@effort:max","messages":[{"role":"user","content":"hi"}]}`,
		rosetta.ProtoOpenAIChat, "/chat", modelWith(dialect.GLM, "glm-5.3"))
	if got["reasoning_effort"] != "max" {
		t.Fatalf("glm-5.3 max expected, got %v", got["reasoning_effort"])
	}

	got = probeWireModel(t, NewOpenAIChat(),
		`{"model":"m@effort:medium","messages":[{"role":"user","content":"hi"}]}`,
		rosetta.ProtoOpenAIChat, "/chat", modelWith(dialect.DeepSeek, "deepseek-chat"))
	if got["reasoning_effort"] != "high" {
		t.Fatalf("deepseek medium should be sent as high, got %v", got["reasoning_effort"])
	}
}
