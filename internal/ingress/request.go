// Package ingress defines the protocol-independent request view and the
// downstream protocol codecs used by the data plane.
package ingress

import (
	"encoding/json"
	"log/slog"
	"strconv"
	"strings"

	"github.com/cn-maul/rosetta"
	"github.com/cn-maul/rosetta-gateway/internal/dialect"
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

	// ThinkingOff records that the client explicitly asked for no thinking
	// (chat `thinking.type:"disabled"`, `reasoning_effort:"none"`, or the
	// Anthropic equivalent). It is separate from RawEffort because
	// rosetta.ThinkingConfig has no "off" value: nil only means "send
	// nothing", and many upstreams then reason by default — the opposite of
	// what the client asked for. ApplyProtocolThinking writes the explicit
	// off-switch each target protocol understands.
	ThinkingOff bool

	// ThinkingRaw is the client's verbatim `thinking` object, when one was
	// sent on a protocol that has the field (OpenAI-compatible chat dialects,
	// Anthropic Messages). Forwarded as-is to chat-dialect upstreams: the
	// extension fields differ per vendor and re-encoding loses them.
	ThinkingRaw json.RawMessage

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

	// Suffix 是客户端写在 model 尾部的单次覆盖（@thinking:off / @effort:high
	// / @dialect:glm）。它已经作用到上面的 ThinkingOff/RawEffort 上，这里保留
	// 一份是因为方言覆盖要等到选了目标模型之后才能决定怎么下发。
	Suffix ModelSuffixOverride
}

// ModelSuffixOverride 是 model 后缀里与「选哪个方言」有关的部分。
//
// 单独定义而不是直接用 inwire.ModelSuffix：ingress 是协议无关层，把它绑在
// 某个具体解码器的结构上会让第三个协议接入时又要改一次。
type ModelSuffixOverride struct {
	Dialect dialect.Dialect
	// Raw 是剥下来的后缀原文，用于日志：请求记录里必须能看出「这条为什么
	// 和别的请求发得不一样」。
	Raw string
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

// ApplyProtocolThinking writes the client's thinking switch in the target
// model's own dialect.
//
// # Why this cannot live in the codecs
//
// A codec only knows the protocol it decodes. "Turn thinking off" is not a
// protocol concept — it is a per-vendor spelling:
//
//	OpenAI      reasoning_effort:"none"      (no `thinking` field exists)
//	Anthropic   thinking:{type:"disabled"}   + output_config.effort
//	DeepSeek    thinking:{type:"disabled"}
//	GLM         thinking:{type:"disabled"}   (GLM-5.3 rejects it with a 400)
//	Qwen        enable_thinking:false
//	MiniMax     thinking:{type:"disabled"}   (M3.1-Flash rejects it with a 400)
//	Kimi        no off switch at all
//
// Sending one vendor's spelling to another is either silently ignored (the
// client thinks it disabled thinking while the upstream keeps reasoning and
// billing) or a hard 400. That is why the dialect is a per-model fact, and
// why this runs per attempt: failover targets can be different vendors.
//
// It must run after ApplyThinkingCapability, which clamps the requested level
// to what the model supports.
func (r *Request) ApplyProtocolThinking(req *rosetta.ChatRequest, m *routing.UpstreamModel, protocol string) {
	// A model declared not to support thinking needs no switch in either
	// direction, and forwarding a vendor field it may not know is a needless
	// 400 risk. This also covers `thinking:{type:"enabled"}` on such a model:
	// ApplyThinkingCapability already cleared the SDK-side config, and the
	// raw object must not sneak it back in through Extra.
	if m != nil && m.SupportsThinking != nil && !*m.SupportsThinking {
		req.Thinking = nil
		delete(req.Extra, "thinking")
		delete(req.Extra, "enable_thinking")
		return
	}

	modelID := ""
	pinned := dialect.Auto
	var canDisable *bool
	if m != nil {
		modelID = m.ModelID
		pinned = m.ThinkingDialect
		canDisable = m.CanDisableThinking
	}
	// 请求级覆盖优先于库里的配置：@dialect: 是调用方对「这次上游怎么说话」
	// 的明确声明，而且它是唯一能在不改配置的前提下试怪模型的手段。
	if r.Suffix.Dialect != dialect.Auto {
		pinned = r.Suffix.Dialect
	}
	d, source := dialect.Resolve(pinned, modelID)
	r.warnForcedToolChoiceWithThinking(req, d, modelID)
	r.wireThinking(req, d, source, modelID, canDisable, protocol)
}

// warnForcedToolChoiceWithThinking 在「开思考 + 强制调工具」同时出现时留痕。
//
// DeepSeek 文档写明：思考模式下不支持 tool_choice=required 与指定具体工具，
// 会返回 400。实测 tiantoken / opencode.ai 两家的 deepseek-v4.1-flash 都
// 放行了，说明这条约束是**按模型/版本**的，不能一刀切地本地拒绝 ——
// 那会把现在能跑的请求打成 400。
//
// 所以这里只记一条 WARN：真撞上 400 时，日志里能直接看到「是这两个参数
// 打架」，而不用去猜。上游错误原文按设计不回显给客户端（见 outwire/errors.go），
// 这条日志就是排障时唯一的线索。
func (r *Request) warnForcedToolChoiceWithThinking(req *rosetta.ChatRequest, d dialect.Dialect, modelID string) {
	if d != dialect.DeepSeek || req.Thinking == nil || req.ToolChoice == nil {
		return
	}
	if req.ToolChoice.Mode != rosetta.ToolChoiceRequired && req.ToolChoice.Mode != rosetta.ToolChoiceTool {
		return
	}
	slog.Warn("thinking mode with a forced tool_choice: DeepSeek documents this combination as 400 on some model versions",
		"model", modelID, "tool_choice", string(req.ToolChoice.Mode))
}

// wireThinking is the dialect dispatch itself, split out so the decision can
// be unit-tested without constructing a routing snapshot.
func (r *Request) wireThinking(req *rosetta.ChatRequest, d dialect.Dialect, source dialect.Source,
	modelID string, canDisable *bool, protocol string) {
	if d == dialect.Auto || d == dialect.Passthrough {
		r.wireFallback(req, source, modelID, protocol)
		return
	}

	// Anthropic and Responses have a stable, vendor-neutral way to express
	// both "on" and "off", so the dialect only decides whether to override
	// the SDK's own output for the vendors that change the meaning of it.
	if protocol == "response" {
		// 客户端可能用后缀/thinking 对象表达「关」，也可能直接发
		// reasoning.effort:"none"（Responses 的官方写法）。两条都要归到
		// 同一个判断上，否则「关不掉」的模型只挡住了前一条。
		off := r.ThinkingOff || r.wireLevel(req) == effort.None
		if off {
			// reasoning.effort:"none" is the documented off switch for every
			// OpenAI-compatible Responses endpoint we know of, including
			// DeepSeek, GLM and MiniMax. "none" is outside the SDK's enum,
			// hence EffortRaw.
			//
			// 与 chat / anthropic 两条入口同一口径：判定为「关不掉」的模型
			// （GLM-5.3 / MiniMax-M3.1-Flash / Kimi K3）收到 none 是 400，
			// 不能因为换了协议就硬发。退到它能接受的最低档，并记一条日志。
			if !dialect.CanDisable(d, modelID, canDisable) {
				if wire, ok := dialect.WireEffort(d, modelID, canDisable, dialect.MinEffort(d, modelID)); ok {
					r.setResponseThinking(req, wire)
					slog.Warn("model cannot disable thinking; downgraded to its lowest level",
						"model", modelID, "dialect", string(d), "effort", wire, "protocol", protocol)
				} else {
					req.Thinking = nil
				}
				return
			}
			r.setResponseThinking(req, "none")
			return
		}
		// 强度也要按方言翻译：GLM-5.3 只认 low/high/max，收到 medium 直接
		// 400。chat 入口一直有这一步（wireChat），Responses 入口此前漏了，
		// 于是同一条 effort 在 chat 上能跑、在 responses 上被拒。
		if req.Thinking != nil {
			if lvl := r.wireLevel(req); lvl != effort.Unset && lvl != effort.None {
				if wire, ok := dialect.WireEffort(d, modelID, canDisable, lvl); ok && wire != string(lvl) {
					req.Thinking.EffortRaw = wire
					req.Thinking.Effort = rosetta.EffortUnset
					req.Thinking.BudgetTokens = 0
				}
			}
		}
		return
	}

	if protocol == "anthropic" {
		r.wireAnthropic(req, d, modelID, canDisable)
		return
	}

	r.wireChat(req, d, modelID, canDisable)
}

// wireChat puts the switch on an OpenAI-compatible Chat payload.
func (r *Request) wireChat(req *rosetta.ChatRequest, d dialect.Dialect, modelID string, canDisable *bool) {
	// Qwen is the only vendor here that does not use a `thinking` object.
	if d == dialect.Qwen {
		if r.ThinkingOff {
			req.Thinking = nil
			delete(req.Extra, "thinking")
			setExtra(req, "enable_thinking", false)
			return
		}
		if len(r.ThinkingRaw) > 0 {
			// The client already wrote a `thinking` object. Qwen ignores it,
			// but forwarding it costs nothing and preserves any field this
			// gateway does not model.
			setExtra(req, "thinking", r.ThinkingRaw)
		}
		if req.Thinking != nil {
			setExtra(req, "enable_thinking", true)
		}
		return
	}

	// Kimi K3 系列和其它厂商不是一个请求 schema：官方 API 参考里 K3 只有
	// 顶层 reasoning_effort，没有 thinking 字段（K2.x 才有 thinking）。客户端
	// 若按 K2.x 的写法发了 thinking，必须替它去掉，只留强度。
	if d == dialect.Kimi && dialect.IsKimiK3(modelID) {
		delete(req.Extra, "thinking")
		if r.ThinkingOff {
			req.Thinking = nil
			if allowed := dialect.CanDisable(d, modelID, canDisable); allowed {
				req.Thinking = &rosetta.ThinkingConfig{EffortRaw: "none"}
				return
			}
			if wire, ok := dialect.WireEffort(d, modelID, canDisable, dialect.MinEffort(d, modelID)); ok {
				req.Thinking = &rosetta.ThinkingConfig{EffortRaw: wire}
				slog.Warn("model cannot disable thinking; downgraded to its lowest level",
					"model", modelID, "dialect", string(d), "effort", wire)
			}
			return
		}
		if req.Thinking != nil {
			if lvl := r.wireLevel(req); lvl != effort.Unset && lvl != effort.None {
				if wire, ok := dialect.WireEffort(d, modelID, canDisable, lvl); ok && wire != string(lvl) {
					req.Thinking.EffortRaw = wire
					req.Thinking.Effort = rosetta.EffortUnset
					req.Thinking.BudgetTokens = 0
				}
			}
		}
		return
	}

	if r.ThinkingOff {
		req.Thinking = nil
		if !dialect.CanDisable(d, modelID, canDisable) {
			// GLM-5.3 / MiniMax-M3.1-Flash / Kimi K3 and friends return 400
			// for `disabled`. Saying nothing would leave them reasoning at
			// their default (highest) level with no trace of why; asking for
			// the lowest level they do accept is the closest honest answer.
			if wire, ok := dialect.WireEffort(d, modelID, canDisable, dialect.MinEffort(d, modelID)); ok {
				req.Thinking = &rosetta.ThinkingConfig{EffortRaw: wire}
				slog.Warn("model cannot disable thinking; downgraded to its lowest level",
					"model", modelID, "dialect", string(d), "effort", wire)
			}
			return
		}
		if d == dialect.OpenAI {
			// The official field is reasoning_effort, never a thinking object.
			req.Thinking = &rosetta.ThinkingConfig{EffortRaw: "none"}
			return
		}
		setExtra(req, "thinking", map[string]any{"type": "disabled"})
		return
	}

	// Thinking was requested (or the client said `enabled`). Forward the
	// client's own object verbatim: the extension fields differ per vendor
	// (`clear_thinking` on GLM, `keep` on Kimi) and re-encoding loses them.
	if len(r.ThinkingRaw) > 0 {
		setExtra(req, "thinking", normalizeThinkingRaw(d, r.ThinkingRaw))
	} else if d == dialect.MiniMax && req.Thinking != nil {
		// MiniMax 的 schema 没有 `enabled`：开启态就是 adaptive（默认值）。
		// 不补的话就只能指望上游把缺失的 thinking 当默认值，而 M3.1-Flash
		// 的唯一合法取值是 adaptive。
		setExtra(req, "thinking", map[string]any{"type": thinkingOnType(d)})
		if lvl := r.wireLevel(req); lvl != effort.Unset && lvl != effort.None {
			if wire, ok := dialect.WireEffort(d, modelID, canDisable, lvl); ok && wire != string(lvl) {
				req.Thinking.EffortRaw = wire
				req.Thinking.Effort = rosetta.EffortUnset
				req.Thinking.BudgetTokens = 0
			}
		}
	} else if req.Thinking != nil {
		// A client that only sent `reasoning_effort` gets no thinking object:
		// every dialect here accepts a bare effort, and inventing an `enabled`
		// object would add a field some of them do not know.
		//
		// The level still has to be spelled the way this vendor spells it:
		// GLM-5.3 rejects `medium` outright, and DeepSeek only implements
		// low/high/max. WireEffort is the single table for that; when it has
		// no mapping (Kimi K2, Anthropic) the SDK value is left untouched.
		if lvl := r.wireLevel(req); lvl != effort.Unset && lvl != effort.None {
			if wire, ok := dialect.WireEffort(d, modelID, canDisable, lvl); ok && wire != string(lvl) {
				req.Thinking.EffortRaw = wire
				req.Thinking.Effort = rosetta.EffortUnset
				req.Thinking.BudgetTokens = 0
			}
		}
		// Kimi K2.x 官方对照表把 reasoning_effort 标为「不支持」，只认 thinking
		// 对象。清空强度不够：SDK 在 Thinking 非 nil 但没档位时会补一个默认
		// effort，最终仍会写出 reasoning_effort。整块丢掉 —— K2.6 默认就思考，
		// 只是这次无法表达「要更强」；这比发一个上游标记为不支持的字段安全。
		if !dialect.AcceptsReasoningEffort(d, modelID) {
			req.Thinking = nil
			slog.Debug("model does not accept reasoning_effort; the requested level cannot be expressed",
				"model", modelID, "dialect", string(d))
		}
	}
}

// wireAnthropic puts the switch on an Anthropic Messages payload.
//
// Two families live here and they disagree about how to express effort:
//   - real Claude: `thinking:{type:"enabled",budget_tokens:N}`; there is no
//     effort field, and budget_tokens is mandatory for type:"enabled".
//   - DeepSeek / GLM / Qwen / MiniMax Anthropic-compatible endpoints: they
//     accept `thinking` as an on/off switch but **ignore budget_tokens**, and
//     use `output_config.effort` for the level.
//
// Getting this backwards is why a "high effort" request to DeepSeek silently
// ran at its default level.
func (r *Request) wireAnthropic(req *rosetta.ChatRequest, d dialect.Dialect, modelID string, canDisable *bool) {
	vendorEffort := d != dialect.Anthropic
	// Kimi K3 走 Anthropic 兼容端点时，它的原生 schema 没有 thinking；能否
	// 用 Anthropic 的 thinking 对象取决于该端点自己的兼容层。官方只保证
	// 原生 Chat 的字段表，因此这里不为 K3 发明 thinking 开关，只发强度。
	k3Anthropic := d == dialect.Kimi && dialect.IsKimiK3(modelID)

	if r.ThinkingOff {
		req.Thinking = nil
		if !dialect.CanDisable(d, modelID, canDisable) {
			if wire, ok := dialect.WireEffort(d, modelID, canDisable, dialect.MinEffort(d, modelID)); ok {
				if !k3Anthropic {
					setExtra(req, "thinking", map[string]any{"type": "adaptive"})
				}
				r.setOutputEffort(req, wire)
				slog.Warn("model cannot disable thinking; downgraded to its lowest level",
					"model", modelID, "dialect", string(d), "effort", wire)
			}
			return
		}
		// Real Claude, DeepSeek's /anthropic endpoint, and GLM's Claude-compatible
		// endpoint all take the same {"thinking":{"type":"disabled"}} switch.
		setExtra(req, "thinking", map[string]any{"type": "disabled"})
		return
	}

	if !vendorEffort {
		return // real Claude: the SDK already emitted budget_tokens
	}

	// Vendor endpoint: keep the on/off switch, replace the useless token
	// budget with the effort field it actually reads.
	if len(r.ThinkingRaw) > 0 {
		if !k3Anthropic {
			setExtra(req, "thinking", r.ThinkingRaw)
		}
	} else if req.Thinking != nil {
		// Adaptive has no vendor-wide spelling. Keep the on/off switch and
		// let the vendor run at its own default level instead of inventing
		// an effort value it may reject.
		if k3Anthropic {
			// 不写 thinking 开关；强度照常按方言翻译（见下面的 WireEffort）。
		} else if level := r.wireLevel(req); level == effort.Unset {
			setExtra(req, "thinking", map[string]any{"type": "enabled"})
			return
		}
		// The ingress sent only a budget (Anthropic clients have no effort
		// field). Translate the level the gateway derived from it.
		if wire, ok := dialect.WireEffort(d, modelID, canDisable, r.wireLevel(req)); ok {
			if !k3Anthropic {
				setExtra(req, "thinking", map[string]any{"type": "enabled"})
			}
			r.setOutputEffort(req, wire)
		}
	}
	if req.Thinking != nil {
		// Stop the SDK from also sending an ignored budget_tokens.
		req.Thinking = nil
	}
}

// setOutputEffort writes `output_config.effort`, merging with the structured
// output format the SDK may have put in the same object.
func (r *Request) setOutputEffort(req *rosetta.ChatRequest, effortValue string) {
	cfg := map[string]any{"effort": effortValue}
	// output_config.format is how Anthropic expresses json_schema; replacing
	// the whole object would silently drop a structured-output constraint.
	if req.ResponseFormat != nil && req.ResponseFormat.Type == "json_schema" {
		cfg["format"] = map[string]any{"type": "json_schema", "schema": req.ResponseFormat.Schema}
	}
	setExtra(req, "output_config", cfg)
}

// wireLevel recovers the effort level the gateway derived for this attempt.
func (r *Request) wireLevel(req *rosetta.ChatRequest) effort.Level {
	if req.Thinking == nil {
		return effort.Unset
	}
	if req.Thinking.EffortRaw != "" {
		return effort.Parse(req.Thinking.EffortRaw)
	}
	if req.Thinking.Effort != rosetta.EffortUnset {
		return effort.Parse(string(req.Thinking.Effort))
	}
	if r.RawEffort != "" {
		return effort.Parse(r.RawEffort)
	}
	return effort.Unset
}

// setResponseThinking replaces the Responses-side thinking config while
// keeping the caller's `include` list.
//
// # 为什么单独一个函数
//
// `include` 在 rosetta 里挂在 ThinkingConfig 上（SDK 只在 Thinking 非 nil
// 时才把顶层 include 写进 payload）。关思考时直接覆写 `req.Thinking` 会把
// 客户端要的字段一起抹掉 —— 典型受害者是 Codex 一类客户端，它用
// `include:["reasoning.encrypted_content"]` 换回可离线续接的加密思考。
// 关的是这一次的思考，不是「以后别再返回思考包裹」。
func (r *Request) setResponseThinking(req *rosetta.ChatRequest, effortRaw string) {
	next := &rosetta.ThinkingConfig{EffortRaw: effortRaw}
	if req.Thinking != nil {
		next.Include = req.Thinking.Include
	}
	req.Thinking = next
}

// wireFallback is what happens when the dialect is unknown.
//
// The rule is: never invent a vendor field. Forward what the client itself
// sent (its spelling is the one the upstream was written against, since the
// client already talks to that endpoint) and log when a switch could not be
// expressed. This cannot break a request; it can only fail to take effect,
// and the log makes that visible instead of silent.
func (r *Request) wireFallback(req *rosetta.ChatRequest, source dialect.Source, modelID, protocol string) {
	// 厂商文档明说「始终思考」的模型，任何关闭写法都是 400。这条判断必须
	// 排在最前：三个协议分支的默认写法对它们全是非法值。
	if r.ThinkingOff && dialect.CannotDisableByDocs(modelID) {
		req.Thinking = nil
		delete(req.Extra, "thinking")
		slog.Info("model cannot disable thinking by vendor docs; "+
			"left the off-switch unset instead of risking a 400",
			"model", modelID, "source", string(source), "protocol", protocol)
		return
	}
	switch protocol {
	case "response":
		if r.ThinkingOff {
			r.setResponseThinking(req, "none")
		}
		return
	case "anthropic":
		if r.ThinkingOff {
			req.Thinking = nil
			setExtra(req, "thinking", map[string]any{"type": "disabled"})
			slog.Debug("thinking dialect unknown; sent the protocol-standard disabled switch",
				"model", modelID, "source", string(source))
		}
		return
	}

	if len(r.ThinkingRaw) > 0 {
		if r.ThinkingOff {
			req.Thinking = nil
		}
		setExtra(req, "thinking", r.ThinkingRaw)
		return
	}
	if r.ThinkingOff {
		// Honour the client's own spelling when it only used an effort value:
		// "none" is the one off-switch every OpenAI-compatible chat endpoint
		// shares, and it is also what the official API uses.
		req.Thinking = &rosetta.ThinkingConfig{EffortRaw: "none"}
		slog.Debug("thinking dialect unknown; sent reasoning_effort=none", "model", modelID)
	}
}

// setExtra merges one protocol-private payload field into req.Extra.
func setExtra(req *rosetta.ChatRequest, key string, value any) {
	if req.Extra == nil {
		req.Extra = map[string]any{}
	}
	req.Extra[key] = value
}

// thinkingOnType 是该方言表达「开启思考」时 thinking.type 的合法取值。
//
// MiniMax 官方 schema（text-chat-openai）把 thinking.type 的 enum 定为
// adaptive / disabled：adaptive 是默认值，也是 M3.1-Flash 唯一接受的取值；
// enabled 不在枚举里。其它方言用 enabled。
func thinkingOnType(d dialect.Dialect) string {
	if d == dialect.MiniMax {
		return "adaptive"
	}
	return "enabled"
}

// normalizeThinkingRaw 把客户端原样带来的 thinking 对象收敛到目标方言的合法形状。
//
// 只改「语义相同但拼写不在目标 schema 里」的情况，其余字段原样保留 ——
// GLM 的 clear_thinking、Kimi 的 keep 这类厂商扩展正是靠原样转发才不会丢。
func normalizeThinkingRaw(d dialect.Dialect, raw json.RawMessage) any {
	if d != dialect.MiniMax {
		return raw
	}
	// MiniMax 的 schema 不接受 enabled；把它换成等价的 adaptive。
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return raw
	}
	if t, _ := obj["type"].(string); strings.EqualFold(t, "enabled") {
		obj["type"] = "adaptive"
	}
	return obj
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
