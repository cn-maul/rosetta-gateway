package ingress

import (
	"testing"

	"github.com/cn-maul/rosetta"
	"github.com/cn-maul/rosetta-gateway/internal/effort"
	"github.com/cn-maul/rosetta-gateway/internal/routing"
)

// EffortLabel 是「落库的思考档位」的唯一来源，覆盖的是
// 「ApplyThinkingCapability 之后 upstreamReq 上到底能读出什么」这个问题。
//
// 它的三种可能来源按优先级排：EffortRaw（夹取后的原样档位）> Effort
// （anthropic 映射后的三档）> BudgetTokens（预算原样透传）。任何把顺序
// 弄反的改法都会在这里红：比如先读 Effort 再读 EffortRaw，就会把
// xhigh 记成 medium（因为非 anthropic 分支把 Effort 显式置成 Unset，
// 但若有人改成不置，Effort 会是 SDK 默认值）。
func TestEffortLabel_ReadsWhatWentOnTheWire(t *testing.T) {
	cases := []struct {
		name string
		// raw 是客户端请求的档位，空串 = 没请求思考。
		raw string
		// levels 是模型支持的档位集合，nil = 未配置（不夹取）。
		levels []effort.Level
		proto  string
		// budget 是 anthropic 入站给的原始预算。
		budget int
		want   string
		// why 记录这条用例钉住的是哪个具体失效模式。
		why string
	}{
		{
			name: "没请求思考时留空",
			want: "",
			why:  "空串不能写成 medium —— SDK 的默认值不是上游收到的显式指令",
		},
		{
			name: "原样送达的 xhigh 不被折成三档",
			raw:  "xhigh",
			want: "xhigh",
			why:  "非 anthropic 目标走 EffortRaw，xhigh 这种 SDK 枚举外的值必须原样保留",
		},
		{
			name:   "夹取后记夹取到的档位而不是客户端要的",
			raw:    "xhigh",
			levels: []effort.Level{effort.Low},
			want:   "low",
			why:    "客户端要 xhigh、模型只到 low，落库写 xhigh 就是在骗排障的人",
		},
		{
			name:   "支持集内的档位不夹取",
			raw:    "medium",
			levels: []effort.Level{effort.Low, effort.Medium, effort.High},
			want:   "medium",
			why:    "夹取只在不在集合内时发生，支持集内必须原样",
		},
		{
			name:   "anthropic 目标读映射后的三档",
			raw:    "xhigh",
			levels: []effort.Level{effort.Low, effort.Medium, effort.High},
			proto:  "anthropic",
			want:   "high",
			why:    "anthropic 没有 effort 字段，走 Effort（三档映射），EffortRaw 为空",
		},
		{
			name:   "anthropic 预算透传记成 budget:<n>",
			raw:    "high", // 有预算时 raw 不参与，走预算分支
			proto:  "anthropic",
			budget: 5000,
			want:   "budget:5000",
			why:    "预算与档位不可逆互转，记档位会丢掉真实预算",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := &rosetta.ChatRequest{
				Model:    "public",
				Thinking: &rosetta.ThinkingConfig{},
			}
			ing := &Request{RawEffort: c.raw, AnthropicBudget: c.budget}
			m := &routing.UpstreamModel{ModelID: "m", ProviderID: "p", EffortLevels: c.levels}
			ing.ApplyThinkingCapability(req, m, c.proto)

			if got := EffortLabel(req.Thinking); got != c.want {
				t.Fatalf("EffortLabel = %q，期望 %q（%s）", got, c.want, c.why)
			}
		})
	}
}

// Thinking 被模型能力剥掉时（supports_thinking=false），落库必须是空串：
// 请求里有 Thinking 但上游一个思考令牌都不会产生。
func TestEffortLabel_UnsupportedThinkingRecordsNothing(t *testing.T) {
	no := false
	req := &rosetta.ChatRequest{Model: "public", Thinking: &rosetta.ThinkingConfig{}}
	ing := &Request{RawEffort: "high"}
	ing.ApplyThinkingCapability(req, &routing.UpstreamModel{ModelID: "m", SupportsThinking: &no}, "chat")

	if req.Thinking != nil {
		t.Fatalf("Thinking 应被剥掉，实际 %+v", req.Thinking)
	}
	if got := EffortLabel(req.Thinking); got != "" {
		t.Fatalf("EffortLabel = %q，期望空串", got)
	}
}

// nil Thinking（没请求过思考）不能 panic，也不能编出一个档位。
func TestEffortLabel_NilIsEmpty(t *testing.T) {
	if got := EffortLabel(nil); got != "" {
		t.Fatalf("EffortLabel(nil) = %q，期望空串", got)
	}
}
