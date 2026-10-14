package upstream

import (
	"testing"
)

// TestRawModelEntry_思考字段多写法 上游各家 /models 的思考字段名不统一，
// 这里钉住「认得的那几种都能取到」，而不是只认一种 ——
// 猜错的后果是 effort_levels 永远为空，而空 = 网关不干预 = 静默。
//
// 用例里的形状取自真实上游：某个国产服务返回
// reasoning_supported_efforts=[low,medium,high,xhigh,max]、
// reasoning_default_effort=max、supports_reasoning=true。
func TestRawModelEntry_思考字段多写法(t *testing.T) {
	cases := []struct {
		name     string
		entry    rawModelEntry
		wantLvls []string
		wantBool *bool
	}{
		{
			name:     "reasoning_supported_efforts（实测那家）",
			entry:    rawModelEntry{ReasoningSupportedEfforts: []string{"low", "medium", "high", "xhigh", "max"}, SupportsReasoning: boolp(true)},
			wantLvls: []string{"low", "medium", "high", "xhigh", "max"},
			wantBool: boolp(true),
		},
		{
			name:     "supported_reasoning（OpenRouter 约定）",
			entry:    rawModelEntry{SupportedReasoning: []string{"low", "high"}},
			wantLvls: []string{"low", "high"},
		},
		{
			name:     "supported_efforts",
			entry:    rawModelEntry{SupportedEfforts: []string{"minimal", "high"}},
			wantLvls: []string{"minimal", "high"},
		},
		{
			name:     "effort_levels",
			entry:    rawModelEntry{EffortLevelsRaw: []string{"low"}},
			wantLvls: []string{"low"},
		},
		{
			name: "都没报 → 空（不是「不支持」）",
			// 关键：全空时返回 nil 而**不是**空切片之外的东西，
			// 更不能给出 false。admin 层据此判断「未配置，网关不干预」。
			entry:    rawModelEntry{ID: "x"},
			wantLvls: nil,
		},
		{
			name:     "多字段并存取第一个非空",
			entry:    rawModelEntry{ReasoningSupportedEfforts: []string{}, SupportedReasoning: []string{"low", "max"}},
			wantLvls: []string{"low", "max"},
		},
		{
			name:     "supports_thinking 变体",
			entry:    rawModelEntry{SupportsThinking: boolp(false)},
			wantBool: boolp(false),
		},
		{
			name: "supports_reasoning 优先于 supports_thinking",
			// 固定顺序而非「取第一个非 nil」：自相矛盾的上游不能靠猜，
			// 且顺序一旦漂移，同一个上游两次发现会给出不同结论。
			entry:    rawModelEntry{SupportsReasoning: boolp(true), SupportsThinking: boolp(false)},
			wantBool: boolp(true),
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := c.entry.effortLevelsFor()
			if len(got) != len(c.wantLvls) {
				t.Fatalf("effortLevelsFor() = %v，期望 %v", got, c.wantLvls)
			}
			for i := range c.wantLvls {
				if got[i] != c.wantLvls[i] {
					t.Errorf("挡位[%d] = %q，期望 %q", i, got[i], c.wantLvls[i])
				}
			}

			gotB := c.entry.supportsThinkingFor()
			switch {
			case c.wantBool == nil && gotB != nil:
				t.Errorf("supportsThinkingFor() = %v，期望 nil（nil 与 false 必须可区分）", *gotB)
			case c.wantBool != nil && gotB == nil:
				t.Errorf("supportsThinkingFor() = nil，期望 %v", *c.wantBool)
			case c.wantBool != nil && *gotB != *c.wantBool:
				t.Errorf("supportsThinkingFor() = %v，期望 %v", *gotB, *c.wantBool)
			}
		})
	}
}

func boolp(v bool) *bool { return &v }
