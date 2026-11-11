package protocol

import (
	"testing"

	"github.com/cn-maul/rosetta"
)

// 映射必须是一个双射：任何一侧新增协议却漏改另一侧，这里就会红。
func TestProtocolNamesRoundTrip(t *testing.T) {
	for _, name := range []string{Chat, Response, Anthropic} {
		sdk, ok := ToSDK(name)
		if !ok {
			t.Fatalf("ToSDK(%q) 不认识这个已知协议", name)
		}
		if got := FromSDK(sdk); got != name {
			t.Errorf("FromSDK(ToSDK(%q)) = %q，不是双射", name, got)
		}
	}
}

// 历史别名必须和当前取值等价，否则旧配置在升级后会被当成未知协议。
func TestToSDKAcceptsLegacyNames(t *testing.T) {
	for legacy, want := range map[string]rosetta.Protocol{
		LegacyChat:     rosetta.ProtoOpenAIChat,
		LegacyResponse: rosetta.ProtoOpenAIResponses,
		" chat ":       rosetta.ProtoOpenAIChat, // 首尾空白也归一到同一取值
	} {
		got, ok := ToSDK(legacy)
		if !ok || got != want {
			t.Errorf("ToSDK(%q) = %q, %v；want %q, true", legacy, got, ok, want)
		}
	}
}

// 认不出来时必须明确失败，不能给一个「看起来合理」的默认值：
// 拼错的 protocol 被静默当成 chat，请求会打到错误的端点形态上。
func TestToSDKRejectsUnknown(t *testing.T) {
	for _, bad := range []string{"", Auto, "openai", "gpt", "chatt"} {
		if got, ok := ToSDK(bad); ok {
			t.Errorf("ToSDK(%q) 不该被接受（得到 %q）", bad, got)
		}
	}
}

// 未知协议原样返回：日志里出现一个陌生名字，本身就是要看的信息。
func TestFromSDKKeepsUnknownVerbatim(t *testing.T) {
	if got := FromSDK(rosetta.Protocol("bedrock")); got != "bedrock" {
		t.Errorf("FromSDK 未知协议 = %q，应原样返回", got)
	}
}

// Valid 只认当前取值；历史别名要先过 Normalize —— 管理接口的入参校验
// 就是这个顺序，写成「Valid 顺带认别名」会让两处口径悄悄分叉。
func TestValidAcceptsOnlyCurrentNames(t *testing.T) {
	for _, name := range []string{Auto, Chat, Response, Anthropic} {
		if !Valid(name) {
			t.Errorf("Valid(%q) = false", name)
		}
	}
	for _, name := range []string{"", LegacyChat, LegacyResponse, "openai", "chatt"} {
		if Valid(name) {
			t.Errorf("Valid(%q) = true；别名必须先 Normalize", name)
		}
	}
}
