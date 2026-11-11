package dialect

import (
	"testing"

	"github.com/cn-maul/rosetta-gateway/internal/effort"
)

func TestDetect(t *testing.T) {
	cases := []struct {
		model string
		want  Dialect
		ok    bool
	}{
		{"deepseek-chat", DeepSeek, true},
		{"deepseek/deepseek-v4-pro", DeepSeek, true},
		// 真实中转站上遇到的写法：点号版本号 + 日期后缀。
		// 名字必须能推成 DeepSeek，否则 `@thinking:off` 会退回「只发
		// 协议标准字段」，而这一家恰恰只认 thinking/effort。
		{"deepseek-v4.1-flash-1017", DeepSeek, true},
		{"deepseek-v4-1-flash", DeepSeek, true},
		{"glm-5.3", GLM, true},
		{"zhipu/glm-5.2", GLM, true},
		{"qwen3.8-max", Qwen, true},
		{"MiniMax-M3.1-Flash", MiniMax, true},
		{"kimi-k3.1-pro", Kimi, true},
		{"gpt-5.6", OpenAI, true},
		{"o3-mini", OpenAI, true},
		{"claude-sonnet-5-5", Anthropic, true},
		{"my-private-model", Auto, false},
		{"", Auto, false},
	}
	for _, c := range cases {
		got, ok := Detect(c.model)
		if got != c.want || ok != c.ok {
			t.Errorf("Detect(%q) = (%q,%v)，期望 (%q,%v)", c.model, got, ok, c.want, c.ok)
		}
	}
}

func TestResolveManualWins(t *testing.T) {
	// 手工配置必须压过推断：否则管理员改成 Qwen 之后，一个名字里带
	// deepseek 的模型会被悄悄改回 DeepSeek。
	got, src := Resolve(Qwen, "deepseek-chat")
	if got != Qwen || src != SourceManual {
		t.Fatalf("Resolve = (%q,%q)，期望 (qwen,manual)", got, src)
	}

	got, src = Resolve(Auto, "deepseek-chat")
	if got != DeepSeek || src != SourceDetected {
		t.Fatalf("Resolve = (%q,%q)，期望 (deepseek,detected)", got, src)
	}

	got, src = Resolve(Auto, "my-relay-model")
	if got != Passthrough || src != SourceFallback {
		t.Fatalf("Resolve = (%q,%q)，期望 (passthrough,fallback)", got, src)
	}
}

func TestCanDisable(t *testing.T) {
	cases := []struct {
		d     Dialect
		model string
		want  bool
	}{
		{DeepSeek, "deepseek-chat", true},
		{GLM, "glm-5.2", true},
		{GLM, "glm-5.3", false}, // 文档：传 disabled 直接报错
		{GLM, "GLM-5.3-FLASH", false},
		{MiniMax, "MiniMax-M3", true},
		{MiniMax, "MiniMax-M3.1-Flash", false},
		{Kimi, "kimi-k3.1-pro", false},
		{Qwen, "qwen3.8-max", true},
		{OpenAI, "gpt-5.6", true},
		{Passthrough, "deepseek-chat", false}, // 不确定 → 不冒险
	}
	for _, c := range cases {
		if got := CanDisable(c.d, c.model, nil); got != c.want {
			t.Errorf("CanDisable(%q,%q) = %v，期望 %v", c.d, c.model, got, c.want)
		}
	}
}

// 中转站会归一化各家写法，实测结果可能与厂商文档表相反。override 就是
// 管理员按实测纠正表的入口 —— 它必须两个方向都能压过文档表。
func TestCanDisableOverride(t *testing.T) {
	yes, no := true, false
	cases := []struct {
		name   string
		d      Dialect
		model  string
		over   *bool
		expect bool
	}{
		{"文档说不能关，实测能关", GLM, "glm-5.3", &yes, true},
		{"文档说不能关，实测仍不能关", Kimi, "kimi-k3.1-pro", &no, false},
		{"文档说能关，实测不能关", DeepSeek, "deepseek-chat", &no, false},
		{"方言未知但实测能关", Passthrough, "relay-thing", &yes, true},
		{"未纠正时沿用文档表", GLM, "glm-5.3", nil, false},
	}
	for _, c := range cases {
		if got := CanDisable(c.d, c.model, c.over); got != c.expect {
			t.Errorf("%s: CanDisable = %v，期望 %v", c.name, got, c.expect)
		}
	}
}

// 纠正成「能关」之后，关闭不能再被改写成 low。只改判断不改字节的话，
// 上游收到的仍然是 low，用户以为关了、实际一直在思考。
//
// 注意两家表达关闭的方式不同：
//   - DeepSeek 有 reasoning_effort:"none"，none 必须原样发；
//   - GLM-5.3 的 effort 没有 none，关闭只能靠 thinking.type，强度字段
//     应当整个不下发（发明一个上游不认的取值就是 400）。
func TestWireEffortOverrideKeepsOff(t *testing.T) {
	yes := true
	got, ok := WireEffort(DeepSeek, "deepseek-chat", &yes, effort.None)
	if !ok || got != "none" {
		t.Fatalf("WireEffort = (%q,%v)，期望 (none,true)", got, ok)
	}
	for _, c := range []struct {
		d     Dialect
		model string
	}{
		{GLM, "glm-5.3"},
		{MiniMax, "MiniMax-M3.1-Flash"},
	} {
		if got, ok := WireEffort(c.d, c.model, &yes, effort.None); ok {
			t.Fatalf("%s/%s 不应下发 effort none，实际 (%q,true)", c.d, c.model, got)
		}
	}
}

func TestWireEffortPerVendor(t *testing.T) {
	cases := []struct {
		d     Dialect
		model string
		in    effort.Level
		want  string
		ok    bool
	}{
		// OpenAI：7 档原样
		{OpenAI, "gpt-5.6", effort.XHigh, "xhigh", true},
		{OpenAI, "gpt-5.6", effort.Max, "max", true},
		// DeepSeek：只认 low/high/max，medium 映射成 high
		{DeepSeek, "deepseek-chat", effort.Medium, "high", true},
		{DeepSeek, "deepseek-chat", effort.XHigh, "high", true},
		{DeepSeek, "deepseek-chat", effort.Max, "max", true},
		// GLM-5.3：只认 max/high/low，xhigh 必须折成 max 而不是原样发
		{GLM, "glm-5.3", effort.XHigh, "max", true},
		{GLM, "glm-5.3", effort.Medium, "high", true},
		{GLM, "glm-5.3", effort.None, "low", true},
		// GLM-5.2：认全档
		{GLM, "glm-5.2", effort.XHigh, "xhigh", true},
		// MiniMax：没有 none（传了 400）
		{MiniMax, "MiniMax-M3", effort.None, "", false},
		{MiniMax, "MiniMax-M3.1-Flash", effort.Max, "max", true},
		// Kimi K3：low/high/max
		{Kimi, "kimi-k3.1-pro", effort.XHigh, "max", true},
		// 真 Claude 的 chat 兼容层没有 effort 字段
		{Anthropic, "claude-sonnet-5-5", effort.High, "", false},
	}
	for _, c := range cases {
		got, ok := WireEffort(c.d, c.model, nil, c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("WireEffort(%q,%q,%q) = (%q,%v)，期望 (%q,%v)",
				c.d, c.model, c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestParse(t *testing.T) {
	for _, s := range []string{"", "deepseek", "GLM", " qwen ", "passthrough"} {
		if _, ok := Parse(s); !ok {
			t.Errorf("Parse(%q) 应通过", s)
		}
	}
	for _, s := range []string{"gemini", "xxx", "deep seek"} {
		if _, ok := Parse(s); ok {
			t.Errorf("Parse(%q) 应被拒", s)
		}
	}
}
