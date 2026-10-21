// Package dataplane contains protocol-independent request forwarding policy.
package dataplane

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/cn-maul/rosetta"
	"github.com/cn-maul/rosetta-gateway/internal/outwire"
	"github.com/cn-maul/rosetta-gateway/internal/routing"
	"github.com/cn-maul/rosetta-gateway/internal/store"
)

// AppendRouteTrace 记录一次链上决策：第几个目标、谁、结果、原因。
//
// 格式固定为 "N:provider/model -> outcome[:reason]"，按发生顺序用 "; " 连接。
// 固定格式是为了让运维能在调用明细里直接读，也方便将来需要时用简单切分做统计；
// 不引入 JSON 是为了不为一次排障增加 schema 与查询复杂度。
func AppendRouteTrace(trace []string, attempt int, cand routing.Candidate, outcome, reason string) []string {
	provider, model := "?", "?"
	if cand.Provider != nil {
		provider = cand.Provider.Slug
	}
	if cand.UpstreamModel.ModelID != "" {
		model = cand.UpstreamModel.ModelID
	}
	entry := fmt.Sprintf("%d:%s/%s -> %s", attempt, provider, model, outcome)
	if reason != "" {
		entry += ":" + reason
	}
	return append(trace, entry)
}

// StreamCommitOutcome 把已提交流式的终态映射成轨迹里的结果词。
// 用 "ok" / "committed_fail" 与未提交路径保持同一套词汇，避免同一种
// 「流断了」在不同路径里出现两种叫法（status 本身只用于计费判读）。
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
	ClientGone         bool
	Eligible           bool
	CredentialCooldown time.Duration
	StatusCode         int
	Code               string
	Message            string
	// Detail 是上游失败详情（脱敏 + 截断），落进 usage_records.error_detail
	// 供调用历史排查。与 Message 不同：Message 是给客户端的固定文案，
	// Detail 是给运维的上游原文。空串 = 无正文可截（TransportError 等）。
	Detail string
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
			// 首批事件之前就断流 = 这次尝试什么也没产出，值得换目标再试。
			Eligible:           true,
			CredentialCooldown: 60 * time.Second,
			StatusCode:         http.StatusBadGateway,
			Code:               "upstream_error",
			Message:            "upstream stream failed before the first event",
		}
	}
	eligible := outwire.FailoverEligible(err)
	statusCode, code, message := outwire.MapUpstreamError(err)
	return Outcome{
		Eligible:           eligible,
		CredentialCooldown: outwire.CredentialCooldown(err),
		StatusCode:         statusCode,
		Code:               code,
		Message:            message,
		Detail:             outwire.UpstreamErrorDetail(err),
	}
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
