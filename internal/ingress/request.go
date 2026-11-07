// Package ingress defines the protocol-independent request view and the
// downstream protocol codecs used by the data plane.
package ingress

import (
	"log/slog"
	"strconv"

	"github.com/cn-maul/rosetta"
	"github.com/cn-maul/rosetta-gateway/internal/effort"
	"github.com/cn-maul/rosetta-gateway/internal/routing"
)

// Request is the decoded, protocol-independent request view.
//
// BuildRosetta is a factory called once per attempt rather than a prepared
// request: targets in a failover chain can use different upstream protocols,
// so each attempt needs its own instance with its own upstream model id.
type Request struct {
	BuildRosetta func() *rosetta.ChatRequest
	Stream       bool
	Model        string // public model name, before route resolution

	WantsStreamUsage bool

	// CacheKey is the client's prompt-cache affinity key, when one was sent.
	// It is transport metadata, not conversation content: the upstream uses it
	// to choose a cache shard, while the model never sees it.
	CacheKey string

	// RequiresStructuredOutput marks a hard structured-output constraint
	// (chat response_format or responses text.format set to json_object /
	// json_schema). Such requests must not be sent to Anthropic upstreams.
	RequiresStructuredOutput bool

	// RawEffort is the client's original effort level before normalization.
	// An empty string means it was not specified.
	//
	// This is kept separately from Thinking.Effort because normalization
	// collapses minimal into low and xhigh into high, while a model's
	// configured set may only contain minimal or xhigh.
	RawEffort string

	// AnthropicBudget is the original thinking.budget_tokens supplied by an
	// Anthropic ingress client. Zero means thinking was not requested.
	//
	// Budget and effort level are not reversibly interchangeable. Only the
	// Anthropic-to-Anthropic path forwards this value verbatim; cross-protocol
	// routing uses RawEffort instead.
	AnthropicBudget int

	// DowngradedTools maps ingress tool names to native Responses types
	// ("custom" / "local_shell"). The Responses codec uses it to restore
	// custom_tool_call / local_shell_call items in the response.
	DowngradedTools map[string]string
}

// ApplyThinkingCapability adjusts a request's thinking configuration for the
// target model's configured capabilities.
//
// It is applied once per attempt because failover targets can have different
// models and capabilities. The order is significant:
//
//  1. Explicitly unsupported thinking removes Thinking entirely.
//  2. A requested effort outside the model's configured set is clamped to the
//     nearest supported level.
//  3. A supported or unconfigured effort is written back through EffortRaw so
//     values outside the SDK's three-level enum are preserved.
//
// An unset effort is not written back: it can mean either "unconfigured" or
// "thinking with no requested level", and the gateway should not choose one.
func (r *Request) ApplyThinkingCapability(req *rosetta.ChatRequest, m *routing.UpstreamModel, protocol string) {
	if m == nil || req.Thinking == nil {
		return
	}

	// nil means unknown, not unsupported. Only an explicit false removes
	// Thinking.
	if m.SupportsThinking != nil && !*m.SupportsThinking {
		req.Thinking = nil
		return
	}

	// Anthropic-to-Anthropic keeps the token budget verbatim. This must run
	// before effort clamping because budget and effort level are not
	// reversibly interchangeable.
	if protocol == "anthropic" && r.AnthropicBudget > 0 {
		req.Thinking.EffortRaw = ""
		req.Thinking.Effort = rosetta.EffortUnset
		req.Thinking.BudgetTokens = r.AnthropicBudget
		return
	}

	if r.RawEffort == "" {
		return
	}

	wanted := effort.Parse(r.RawEffort)
	if wanted == effort.Unset {
		return
	}
	applied := wanted
	if len(m.EffortLevels) > 0 && !effort.Supports(wanted, m.EffortLevels) {
		applied = effort.Clamp(wanted, m.EffortLevels)
		slog.Warn("clamped reasoning effort to the model's supported set",
			"model", m.ModelID, "provider", m.ProviderID,
			"requested", string(wanted), "applied", string(applied))
	}

	// Anthropic has no effort field. Cross-protocol requests let the SDK map
	// the clamped level back to a budget; the original budget is cleared so
	// it cannot override that mapping.
	if protocol == "anthropic" {
		req.Thinking.EffortRaw = ""
		req.Thinking.Effort = rosetta.Effort(effort.ToRosettaLevel(applied))
		req.Thinking.BudgetTokens = 0
		return
	}
	req.Thinking.EffortRaw = string(applied)
	req.Thinking.Effort = rosetta.EffortUnset
}

// EffortLabel renders the thinking level that was actually put on the wire.
//
// Call it after ApplyThinkingCapability and on the same *rosetta.ChatRequest
// that is about to be sent: the value it reports is the clamped one, not what
// the client asked for. That distinction is the whole point of recording it —
// 「客户端要 high、上游只给 medium」是排障时最想知道的一件事，而落库原始值
// 会让人以为上游收到了它其实没收到的强度。
//
// 空串表示这次调用没有思考，取值规则与 SDK 的 effortWire 保持一致：
//
//  1. EffortRaw 优先 —— ApplyThinkingCapability 把夹取后的档位写在这里，
//     好让 SDK 的三档枚举之外的值（minimal/xhigh/max）原样下发。
//  2. 否则用 Effort —— anthropic 目标走这条，档位已被映射成 low/medium/high。
//  3. 再看 BudgetTokens —— anthropic 预算原样透传的分支只写了预算，
//     记成 "budget:8192" 而不是猜一个档位（预算与档位不可逆互转）。
//  4. 都没有就留空：Thinking 非 nil 但没指定档位时 SDK 会默认 medium，
//     但那是 SDK 的默认值不是上游收到的显式指令，网关不替它编造。
func EffortLabel(t *rosetta.ThinkingConfig) string {
	if t == nil {
		return ""
	}
	if t.EffortRaw != "" {
		return t.EffortRaw
	}
	if t.Effort != rosetta.EffortUnset {
		return string(t.Effort)
	}
	if t.BudgetTokens > 0 {
		return "budget:" + strconv.Itoa(t.BudgetTokens)
	}
	return ""
}
