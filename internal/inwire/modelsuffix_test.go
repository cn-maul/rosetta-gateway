package inwire

import (
	"testing"

	"github.com/cn-maul/rosetta-gateway/internal/effort"
)

func TestParseModelSuffix_NoSuffixLeavesNameAlone(t *testing.T) {
	for _, name := range []string{
		"deepseek-chat",
		"qwen-max", // 以 -max 结尾但不是后缀
		"gpt-5.1-codex-max",
		"org/model@2024-08",         // 未知 @ 片段不是后缀
		"org/model@sha256:deadbeef", // 同上
	} {
		base, sfx, err := ParseModelSuffix(name)
		if err != nil {
			t.Fatalf("%s: unexpected error: %v", name, err)
		}
		if base != name || sfx.Set {
			t.Fatalf("%s: got base=%q set=%v, want unchanged", name, base, sfx.Set)
		}
	}
}

func TestParseModelSuffix_Thinking(t *testing.T) {
	cases := []struct {
		model    string
		wantBase string
		off      bool
		on       bool
		budget   int
		adaptive bool
		level    string
	}{
		// 关思考时 Effort 清空、Level 固定为 none —— 下游靠这两个字段
		// 区分「没说强度」与「明确不思考」。
		{"deepseek-chat@thinking:off", "deepseek-chat", true, false, 0, false, ""},
		{"glm-4.7@thinking:disabled", "glm-4.7", true, false, 0, false, ""},
		{"deepseek-chat@thinking:0", "deepseek-chat", true, false, 0, false, ""},
		{"deepseek-chat@thinking:on", "deepseek-chat", false, true, 0, false, ""},
		// 预算不是显式档位：Effort 留空，折算结果只在 Level 上，
		// 这样日志里能区分「客户端给了 8192 token」与「客户端要 medium」。
		{"deepseek-chat@thinking:8192", "deepseek-chat", false, true, 8192, false, ""},
		{"claude-sonnet-4-6@thinking:adaptive", "claude-sonnet-4-6", false, true, 0, true, ""},
	}
	for _, tc := range cases {
		base, sfx, err := ParseModelSuffix(tc.model)
		if err != nil {
			t.Fatalf("%s: %v", tc.model, err)
		}
		if base != tc.wantBase {
			t.Fatalf("%s: base=%q want %q", tc.model, base, tc.wantBase)
		}
		if sfx.ThinkingOff != tc.off || sfx.ThinkingOn != tc.on ||
			sfx.Budget != tc.budget || sfx.Adaptive != tc.adaptive ||
			sfx.Effort != tc.level {
			t.Fatalf("%s: got off=%v on=%v budget=%d adaptive=%v level=%q",
				tc.model, sfx.ThinkingOff, sfx.ThinkingOn, sfx.Budget, sfx.Adaptive, sfx.Effort)
		}
		if tc.off && sfx.Level != effort.None {
			t.Fatalf("%s: off suffix left level %q", tc.model, sfx.Level)
		}
	}
}

func TestParseModelSuffix_EffortAndDialect(t *testing.T) {
	base, sfx, err := ParseModelSuffix("m@effort:high@dialect:glm")
	if err != nil {
		t.Fatal(err)
	}
	if base != "m" || sfx.Effort != "high" || sfx.Dialect != "glm" {
		t.Fatalf("got base=%q effort=%q dialect=%q", base, sfx.Effort, sfx.Dialect)
	}

	// 关思考压过强度：同时写了两者时按关处理，且不残留强度。
	_, sfx, err = ParseModelSuffix("m@effort:high@thinking:off")
	if err != nil {
		t.Fatal(err)
	}
	if !sfx.ThinkingOff || sfx.Effort != "" || sfx.ThinkingOn {
		t.Fatalf("off should win: %+v", sfx)
	}
}

func TestParseModelSuffix_RejectsBadValues(t *testing.T) {
	for _, model := range []string{
		"m@thinking:maybe",
		"m@thinking:-5",
		"m@effort:turbo",
		"m@dialect:openai-ish",
	} {
		if _, _, err := ParseModelSuffix(model); err == nil {
			t.Fatalf("%s: expected error", model)
		}
	}
}

func TestParseModelSuffix_OnlyTrailingRunIsStripped(t *testing.T) {
	base, sfx, err := ParseModelSuffix("org/m@2024-08@thinking:off")
	if err != nil {
		t.Fatal(err)
	}
	if base != "org/m@2024-08" || !sfx.ThinkingOff {
		t.Fatalf("got base=%q off=%v", base, sfx.ThinkingOff)
	}
}
