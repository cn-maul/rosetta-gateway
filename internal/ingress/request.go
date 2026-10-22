// Package ingress defines the protocol-independent request view and the
// downstream protocol codecs used by the data plane.
package ingress

import (
	"log/slog"

	"github.com/cn-maul/rosetta"
	"github.com/cn-maul/rosetta-gateway/internal/effort"
	"github.com/cn-maul/rosetta-gateway/internal/routing"
)

// Request is the decoded, protocol-independent request view.
//
// BuildRosetta is a factory called once per attempt rather than a prepared
// request: targets in a failover chain can use different upstream protocols,
// and ApplyUpstreamExtras attaches protocol-private fields to req.Extra.
// Reusing one instance across attempts would leak target A's Extra into target
// B's request.
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

	ApplyUpstreamExtras func(req *rosetta.ChatRequest, upstreamProtocol string)

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
