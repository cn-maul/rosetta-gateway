// Package dataplane contains protocol-independent request forwarding policy.
package dataplane

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/cn-maul/rosetta"
	"github.com/cn-maul/rosetta-gateway/internal/outwire"
	"github.com/cn-maul/rosetta-gateway/internal/routing"
	"github.com/cn-maul/rosetta-gateway/internal/store"
)

// AppendRouteTrace 记录一次链上决策：第几个目标、谁、结果、原因。
//
// 格式固定为 "N:provider/model -> outcome[:reason]"，按发生顺序用 "; " 连接。
// provider 优先用人类可读的 Name（例如「肥鱼中转」），只有旧快照/测试
// 没填 Name 时才退回 Slug。调用历史要回答「从谁切到了谁」，而 slug 往往是
// 部署者随手写的英文短名，光看它认不出是哪家上游。
// 固定格式是为了让运维能在调用明细里直接读，也方便将来需要时用简单切分做统计；
// 不引入 JSON 是为了不为一次排障增加 schema 与查询复杂度。
//
// provider 名和 model 名都可能含 "/"、";" 或 "\\"：provider 名里的 "/" 会
// 让解析器误把名字切成 provider/model，";" 又会被当成下一条轨迹。写入时
// 对这两个字段做最小反斜杠转义，前端解析时还原，既保留原名的可读性，
// 也不改变既有 "N:provider/model -> outcome:reason" 的格式。
func AppendRouteTrace(trace []string, attempt int, cand routing.Candidate, outcome, reason string) []string {
	provider, model := "?", "?"
	if cand.Provider != nil {
		provider = cand.Provider.Name
		if provider == "" {
			provider = cand.Provider.Slug
		}
	}
	if cand.UpstreamModel.ModelID != "" {
		model = cand.UpstreamModel.ModelID
	}
	entry := fmt.Sprintf("%d:%s/%s -> %s",
		attempt, escapeRouteTraceComponent(provider, true), escapeRouteTraceComponent(model, false), outcome)
	if reason != "" {
		entry += ":" + reason
	}
	return append(trace, entry)
}

// escapeRouteTraceComponent 只转义会破坏轨迹语法的字符，不做 URL 编码：
// 转义后的记录仍然一眼能看出是哪个上游/模型。模型名允许包含 "/"（例如
// "org/model"），所以只有 provider 需要把 "/" 转义成 "\/"；分号与反斜杠
// 对两者都必须转义，否则一条记录会被拆成多条或无法还原。
func escapeRouteTraceComponent(value string, escapeSlash bool) string {
	var b strings.Builder
	b.Grow(len(value))
	for _, r := range value {
		switch {
		case r == '\\':
			b.WriteString(`\\`)
		case r == ';':
			b.WriteString(`\;`)
		case r == '/' && escapeSlash:
			b.WriteString(`\/`)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// OutcomeKind is the single terminal classification for one upstream attempt.
// The compatibility booleans on Outcome are projections of this value; request
// orchestration must branch on the kind rather than infer meaning from a
// combination of independent flags.
type OutcomeKind uint8

const (
	// OutcomeUpstreamFailed is an uncommitted upstream/target failure. It may
	// fail over when Eligible is true.
	OutcomeUpstreamFailed OutcomeKind = iota
	OutcomeClientAborted
	OutcomeCommittedSuccess
	OutcomeCommittedPartial
	OutcomeGatewayOverflow
	OutcomeGatewayTimeout
	OutcomeDownstreamWriteFailed
	OutcomeEncodeFailed
)

func (k OutcomeKind) String() string {
	switch k {
	case OutcomeUpstreamFailed:
		return "upstream_failed"
	case OutcomeClientAborted:
		return "client_aborted"
	case OutcomeCommittedSuccess:
		return "committed_success"
	case OutcomeCommittedPartial:
		return "committed_partial"
	case OutcomeGatewayOverflow:
		return "gateway_overflow"
	case OutcomeGatewayTimeout:
		return "gateway_timeout"
	case OutcomeDownstreamWriteFailed:
		return "downstream_write_failed"
	case OutcomeEncodeFailed:
		return "encode_failed"
	default:
		return "unknown"
	}
}

// LaneDecision is the projection consumed by failover, diagnostics, route
// tracing, and accounting. Deriving all of them here keeps stream and
// non-stream commits on the same rules.
type LaneDecision struct {
	Kind             OutcomeKind
	Committed        bool
	Success          bool
	Eligible         bool
	PenalizeUpstream bool
	DownstreamClient bool
	GatewayTimeout   bool
	Billable         bool
	UsageStatus      string
	TraceOutcome     string
	TraceReason      string
	DiagnosticStatus string
	DiagnosticError  string
}

// StreamCommitOutcome maps a persisted terminal status into the trace
// vocabulary. It is retained for callers that only have the status value.
func StreamCommitOutcome(status string) string {
	if status == store.UsageStatusOK {
		return store.UsageStatusOK
	}
	return "committed_fail"
}

// Outcome 是一次「对某个链目标发起上游调用」的结果。
//
// Committed=true 表示已向下游写出内容（非流式的完整响应、或流式已发首字并写完整个流），
// 此时 usage 已由该次 attempt 落库，请求终结，绝不能回退去换目标（半条流收不回）。
// Committed=false 时 attempt 未碰过 ResponseWriter，由外层循环决定「换下一个目标」还是
// 「把这次错误写回客户端」。Eligible 告诉外层这次失败值不值得转移。
type Outcome struct {
	// Kind is authoritative. Constructors and all request-lane producers set
	// it; the boolean fields below remain as compatibility projections.
	Kind      OutcomeKind
	Committed bool
	// Success 表示本次 attempt 真的成功抵达上游（流式仅 status=="ok" 为真）。
	// Committed 只说明「字节已写出、不能回退」，断流/溢出同样是 Committed
	// 却是失败 —— 熔断与凭据健康必须看success，不能看 Committed。
	Success bool
	// ClientGone 标记「这次 attempt 是客户端自己走了」，不是上游故障。
	//
	// 必须显式带出来而不能靠外层看 r.Context().Err()：请求的 ctx 身上还挂着
	// TotalRequestBudget 的超时（见 handleIngress），所以预算到期时
	// ctx.Err() 也是 DeadlineExceeded —— 与用户点「停止」在那一刻无法区分。
	// 旧实现只按「Committed 且非 ok」一律记目标失败，于是连点三次停止就把
	// 健康目标熔断 60 秒；预算到期同样被误记成上游故障。
	ClientGone bool
	// NoUpstreamPenalty marks a committed failure caused by a gateway-local
	// limit rather than upstream behavior. The most important case is
	// ErrStreamOverflow: the SDK's in-memory accumulator hit its cap, so the
	// target must not be cooled down or circuit-broken for it.
	NoUpstreamPenalty bool
	// GatewayTimeout marks a failure caused by the gateway's own total request
	// budget expiring, not by the upstream. It must not cool a credential or
	// count toward target circuit-breaking, and it must not trigger failover:
	// the shared deadline is already gone, so trying another target can only
	// consume the remaining budget of a request that is already over.
	GatewayTimeout bool
	Eligible       bool
	// UsageStatus is the exact persisted usage status selected by the attempt.
	// Keeping it on the outcome prevents the outer lane from re-deriving it from
	// booleans and error text.
	UsageStatus        string
	CredentialCooldown time.Duration
	StatusCode         int
	Code               string
	Message            string
	// Detail 是上游失败详情（脱敏 + 截断），落进 usage_records.error_detail
	// 供调用历史排查。与 Message 不同：Message 是给客户端的固定文案，
	// Detail 是给运维的完整原因。APIError 保留 message + raw；TransportError
	// 保留底层 DNS/TCP/TLS/proxy/timeout 原因；流哨兵保留本地判定文案。
	// 空串 = 该错误确实没有可提供的细节。
	Detail string
}

// normalize fills the compatibility fields from Kind. Producers set Kind once;
// consumers can still use the old fields without each caller inventing a
// different truth table.
func (o Outcome) normalize() Outcome {
	kind := o.normalizedKind()
	o.Kind = kind
	d := LaneDecisionFor(kind, o.Committed, o.Eligible, o.Code)
	o.Committed = d.Committed
	o.Success = d.Success
	if kind == OutcomeUpstreamFailed {
		// Upstream failure eligibility comes from the error mapper; the
		// projection only reports it.
	} else {
		o.Eligible = d.Eligible
	}
	o.ClientGone = d.DownstreamClient
	o.NoUpstreamPenalty = kind == OutcomeGatewayOverflow
	o.GatewayTimeout = d.GatewayTimeout || kind == OutcomeGatewayTimeout
	if kind == OutcomeClientAborted && o.UsageStatus == store.UsageStatusError {
		o.UsageStatus = ""
	}
	if o.UsageStatus == "" {
		o.UsageStatus = d.UsageStatus
	}
	return o
}

func (o Outcome) normalizedKind() OutcomeKind {
	if o.Kind == OutcomeClientAborted && o.Committed && o.Code == "downstream_write_failed" {
		return OutcomeDownstreamWriteFailed
	}
	if o.Kind != OutcomeUpstreamFailed {
		// A committed client abort is a distinct terminal state from a request
		// abandoned before any downstream bytes.
		return o.Kind
	}
	// Legacy struct literals (notably diagnostic unit tests) only set fields.
	switch {
	case o.Committed && o.Success:
		return OutcomeCommittedSuccess
	case o.Committed && o.GatewayTimeout:
		return OutcomeGatewayTimeout
	case o.Committed && o.NoUpstreamPenalty:
		return OutcomeGatewayOverflow
	case o.Committed && o.ClientGone:
		return OutcomeClientAborted
	case o.Committed:
		return OutcomeCommittedPartial
	case o.ClientGone:
		return OutcomeClientAborted
	case o.GatewayTimeout:
		return OutcomeGatewayTimeout
	default:
		return OutcomeUpstreamFailed
	}
}

func usageStatusForOutcome(kind OutcomeKind, explicit, code string) string {
	if explicit != "" {
		return explicit
	}
	switch kind {
	case OutcomeCommittedSuccess:
		return store.UsageStatusOK
	case OutcomeClientAborted, OutcomeDownstreamWriteFailed:
		return store.UsageStatusCanceled
	case OutcomeGatewayTimeout:
		return store.UsageStatusTimeout
	case OutcomeGatewayOverflow:
		return store.UsageStatusOverflow
	case OutcomeCommittedPartial:
		switch code {
		case "stream_truncated", "stream_idle_timeout":
			return store.UsageStatusTruncated
		case "stream_overflow":
			return store.UsageStatusOverflow
		case "gateway_timeout":
			return store.UsageStatusTimeout
		default:
			return store.UsageStatusError
		}
	default:
		return store.UsageStatusError
	}
}

// LaneDecisionFor derives the common lane projection from a kind.
func LaneDecisionFor(kind OutcomeKind, committed, eligible bool, code string) LaneDecision {
	d := LaneDecision{Kind: kind, Committed: committed, Eligible: eligible}
	switch kind {
	case OutcomeCommittedSuccess:
		d.Committed, d.Success, d.Billable = true, true, true
		d.TraceOutcome = store.UsageStatusOK
		d.DiagnosticStatus = "ok"
	case OutcomeCommittedPartial:
		d.Committed, d.PenalizeUpstream = true, true
		d.TraceOutcome = "committed_fail"
		d.TraceReason = code
		d.DiagnosticStatus = "committed_fail"
		d.DiagnosticError = code
	case OutcomeGatewayOverflow:
		d.Committed = true
		d.TraceOutcome = "committed_fail"
		d.TraceReason = code
		d.DiagnosticStatus = "committed_fail"
		d.DiagnosticError = code
	case OutcomeGatewayTimeout:
		d.Committed, d.GatewayTimeout = true, true
		d.TraceOutcome = "committed_fail"
		d.TraceReason = "gateway_timeout"
		d.DiagnosticStatus = "timeout"
		d.DiagnosticError = "gateway_timeout"
	case OutcomeClientAborted:
		d.DownstreamClient = true
		d.TraceReason = "client_canceled"
		d.TraceOutcome = "fail_terminal"
		if d.Committed {
			d.TraceOutcome = "committed_fail"
		}
		d.DiagnosticStatus = "canceled"
	case OutcomeDownstreamWriteFailed:
		d.Committed = true
		d.DownstreamClient = true
		d.TraceOutcome = "committed_fail"
		d.TraceReason = "downstream_write_failed"
		d.DiagnosticStatus = "canceled"
		d.DiagnosticError = "downstream_write_failed"
	case OutcomeEncodeFailed:
		d.Committed = true
		d.TraceOutcome = "fail_terminal"
		d.TraceReason = code
		d.DiagnosticStatus = "committed_fail"
		d.DiagnosticError = code
	default:
		d.TraceOutcome = "fail_terminal"
		d.TraceReason = code
		d.DiagnosticStatus = "failed"
		d.DiagnosticError = code
	}
	d.UsageStatus = usageStatusForOutcome(kind, "", code)
	if d.TraceReason == "" {
		d.TraceReason = code
	}
	if d.DiagnosticError == "" && kind != OutcomeCommittedSuccess {
		d.DiagnosticError = code
	}
	return d
}

// LaneDecision is the only supported interpretation of an Outcome.
func (o Outcome) LaneDecision() LaneDecision {
	o = o.normalize()
	if o.Code == "downstream_write_failed" {
		return LaneDecisionFor(OutcomeDownstreamWriteFailed, true, false, o.Code)
	}
	d := LaneDecisionFor(o.Kind, o.Committed, o.Eligible, o.Code)
	if o.Kind == OutcomeClientAborted && o.Committed {
		d.Committed = true
		d.TraceOutcome = "committed_fail"
	}
	if o.Kind == OutcomeUpstreamFailed {
		d.Eligible = o.Eligible
		d.PenalizeUpstream = o.Eligible
		if o.Eligible {
			d.TraceOutcome = "fail_switch"
		}
	}
	if !o.Eligible && o.Kind == OutcomeUpstreamFailed {
		d.Eligible = false
		d.PenalizeUpstream = false
		d.TraceOutcome = "fail_terminal"
	}
	if o.UsageStatus != "" {
		d.UsageStatus = o.UsageStatus
	}
	return d
}

// WithRequestContext upgrades a generic uncommitted failure when the shared
// request context proves that the client or the gateway budget ended first.
func (o Outcome) WithRequestContext(ctx context.Context) Outcome {
	// An uncommitted attempt that failed while the caller was already gone is
	// a client abort regardless of which error surfaced first. The SDK can
	// wrap context cancellation as a transport/API error; the shared request
	// context is the higher-authority fact.
	if !o.Committed && IsClientGone(ctx, nil) {
		o.Kind = OutcomeClientAborted
		o.Eligible = false
		o.CredentialCooldown = 0
		return o.normalize()
	}
	if o.Committed {
		return o.normalize()
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		o.Kind = OutcomeGatewayTimeout
		o.Eligible = false
		o.CredentialCooldown = 0
		o.StatusCode = http.StatusGatewayTimeout
		o.Code = "gateway_timeout"
		o.Message = "gateway request budget exceeded"
	}
	return o.normalize()
}

// OutcomeFromErr 把一次上游 err 归类成「未提交」的结果。
//
// # 流式哨兵必须在**这里**单独处理（2026-10-10 修复的 P1）
//
// outwire.MapUpstreamError 把 ErrStreamTruncated / ErrStreamOverflow 映射成
// (200, "", "")，理由是「内容已经写到线上去了，没什么可补的」。这个前提
// **只在已提交时成立**，而本函数的每一个调用点都是**未提交**路径
// （见 attemptStream 的 !gotFirst 分支：SSE 头要等到第一个事件到达才写）。
//
// 于是修复前：上游在首个事件前硬失败 → 拿到 (200,"","") → StatusCode 非 0
// 所以 main 的 502 兜底不生效 → 客户端收到
// `{"error":{"message":"","type":"api_error"}}` 且 **HTTP 200**。
// 两个后果叠加：
//   - OpenAI/Anthropic SDK 只在 >=400 时抛错，于是调用方拿到一个空字符串和
//     「成功」；按状态码记账的监控把它记成一条成功调用。
//   - FailoverEligible 对这两个哨兵返回 false，故障转移链直接 break ——
//     一个本该被链吸收的失败，反而被伪造成成功返回。
//
// 所以在这里改判：既给出真实的状态码，也让链继续尝试下一个目标。
// MapUpstreamError 本身**保持原样** —— 它服务的已提交路径（「如实 truncated
// 收尾，不追加错误体」）那个语义是对的，不该为迁就未提交路径而改掉。
func OutcomeFromErr(err error) Outcome {
	if errors.Is(err, rosetta.ErrStreamTruncated) || errors.Is(err, rosetta.ErrStreamOverflow) {
		return Outcome{
			Kind: OutcomeUpstreamFailed,
			// 首批事件之前就断流 = 这次尝试什么也没产出，值得换目标再试。
			// 但这是目标/链路故障，不是凭据故障：不冷却这把 key，
			// 交给外层 RecordTargetFailure 熔断目标。
			Eligible:   true,
			StatusCode: http.StatusBadGateway,
			Code:       "upstream_error",
			Message:    "upstream stream failed before the first event",
			Detail:     outwire.UpstreamErrorDetail(err),
		}.normalize()
	}
	eligible := outwire.FailoverEligible(err)
	statusCode, code, message := outwire.MapUpstreamError(err)
	return Outcome{
		Kind:               OutcomeUpstreamFailed,
		Eligible:           eligible,
		CredentialCooldown: outwire.CredentialCooldown(err),
		StatusCode:         statusCode,
		Code:               code,
		Message:            message,
		Detail:             outwire.UpstreamErrorDetail(err),
	}.normalize()
}

// OutcomeFromErrCtx 在 OutcomeFromErr 之上区分「网关总预算到期」。
//
// 预算挂在请求 ctx 上（见 handleIngress 的 WithTimeout），到期时上游调用
// 同样会以 TransportError / context.DeadlineExceeded 的形式失败。只看错误
// 类型会把「网关自己没来得及跑完」记成「上游坏了」：一次总预算到期就让
// 凭据冷却 60s、目标熔断计数 +1，连续三次就把健康目标熔断 —— 而真正
// 需要调整的是网关的超时/负载。
func OutcomeFromErrCtx(ctx context.Context, err error) Outcome {
	out := OutcomeFromErr(err)
	return out.WithRequestContext(ctx)
}

// IsClientGone 判断一次流式中断是否源于**客户端**断开，而不是上游故障。
//
// 请求 context 被取消（用户点「停止生成」、客户端进程退出、网络切换）时，
// SDK 会把 context 错误从流里透出来。这类中断既不该算进上游的成功率，
// 也不该打成 ERROR —— 它跟路由、凭据、上游的健康状况无关。
//
// 只认 context.Canceled，**不认 context.DeadlineExceeded**：请求 ctx 上还挂着
// 网关自己的 TotalRequestBudget（handleIngress 里 WithTimeout 派生），
// 预算到期同样是 DeadlineExceeded，那是网关侧事件，不能记成用户主动离开。
func IsClientGone(ctx context.Context, err error) bool {
	if errors.Is(err, context.Canceled) {
		return true
	}
	// ctx.Err() 也只认 Canceled：DeadlineExceeded 属于预算到期，见上。
	return errors.Is(ctx.Err(), context.Canceled)
}
