package main

// This file keeps the data-plane balance preflight and the process-level
// loopback check. Pricing policy lives in internal/billing; read-only billing
// query endpoints live in internal/billinghttp.

import (
	"log/slog"
	"net"
	"net/http"
	"strings"

	"github.com/cn-maul/rosetta-gateway/internal/auth"
	"github.com/cn-maul/rosetta-gateway/internal/billing"
	"github.com/cn-maul/rosetta-gateway/internal/routing"
	"github.com/cn-maul/rosetta-gateway/internal/server"
	"github.com/cn-maul/rosetta-gateway/internal/snapshot"
	"github.com/cn-maul/rosetta-gateway/internal/store"
)

// isLoopbackListen 报告 listen 地址是否只绑定回环。
//
// 用途：判断「首次初始化窗口 + 绑非回环」这个组合是否构成
// **持有引导令牌的人能抢先成为第一个管理员**的暴露面。
// 解析失败时**保守返回 false** —— 宁可多报一次警，不可漏掉真实暴露。
func isLoopbackListen(listen string) bool {
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		// 可能是 ":8666"（空 host = 所有接口）或压根不是 host:port。
		if strings.HasPrefix(listen, ":") {
			return false
		}
		return false
	}
	host = strings.TrimSpace(host)
	// 空 host 意味着绑定全部接口（Go 的 net.Listen 语义）。
	if host == "" {
		return false
	}
	// 已经是 IP 字面量。
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	// 主机名：localhost 及常见别名视为回环。
	switch strings.ToLower(host) {
	case "localhost", "localhost.localname", "ip6-localhost", "ip6-loopback":
		return true
	}
	return false
}

// balanceExempt adapts the runtime snapshot to billing's pure policy.
func balanceExempt(userID string) bool {
	u := snapshot.Get().UsersByID[userID]
	return u != nil && billing.IsBalanceExempt(u.Role)
}

// =====================================================================
// 数据面余额：预检（准入）与扣费（记账）
// =====================================================================
//
// # 两套口径必须分清：cost_total 是「上游成本」，余额扣费是「用户应付」
//
// 这一节最容易被后来者搞混的地方，故先说清：
//
//	usage_records.cost_total —— **无论请求成败都会记**，只要触发了上游调用。
//	  它的含义是「这次调用让网关付给上游多少钱」，是网关的成本/审计口径，
//	  失败、超时、断流同样产生成本，所以必须记。
//
//	balance_cents 的扣减 —— **只对成功请求**（Lead 与用户确认的口径）。
//	  它的含义是「用户要为这次成功拿到的东西付多少钱」，是应收口径。
//	  失败请求没给用户产出任何东西，不该收钱。
//
// 两者数字相同（同一次调用的 cost_total 既是成本也是应收），但**触发条件
// 不同**，所以不是同一笔账。若把 cost_total 的「无论成败都记」直接搬过来
// 当扣费触发条件，用户会为上游 502 付费 —— 这与用户确认的口径相反。
//
// # 预检与扣费为什么是两个不同的数
//
//	预检（请求前）：按**模型单价 + 估算 token 数**估算本次量级，只回答
//	  「余额够不够做这一次」。它是准入闸门，**不负责确定扣多少**。
//	扣费（成功后）：金额取该条 usage 落库时固化的 cost_total。
//
// 为什么不干脆在预检时就按估算值扣掉：估不准。输出长度事前不可知（流式更是
// 边生成边计费），预扣多了要退款（余额逻辑没有「预占/退款」这一对操作，
// 与 token 配额的 ReserveQuota/ReleaseQuota 机制不同），预扣少了则拦不住
// 超支。**先按上界准入、后按实收**把「估不准」这件事消掉，且不需要找零。
//
// # 管理员完全跳过（纵深防御，见 balanceExempt）
//
// 这段注释描述的是**余额豁免**这一层。2026-10 控制面/数据面分离之后，
// 管理员**根本进不了 /v1**（internal/auth 的 ErrAdminCannotCallModel 已拦），
// 于是下面两处 balanceExempt 短路在数据面上都是**不可达**的：
//   - precheckBalance 的管理员 early-return（见该函数说明）；
//   - usage.Recorder.charge 的管理员短路（见该函数说明）。
//
// 它们保留作纵深防御：余额是「钱」，多一道零成本的判 admin 短路，将来若
// 某个入口绕过 auth.Authenticate 直达扣费/预检（重构、新的数据面端点、
// 内部复用），这条短路能立刻把管理员挡在「不读库、不计费」之外，而不依赖
// 「上游那一层一定会先拦住」这个跨文件假设。详见 balanceExempt 的注释。

// precheckBalance 执行余额预检，返回 false 表示**已经把错误写回客户端**、
// 调用方应当立即返回（绝不触碰上游）。
//
// # fail-closed 还是 fail-open：本函数选 fail-closed（读不到余额就拒绝）
//
// 这与本文件既有的两处查询抖动决策（token 配额预检、用户 token 配额预检都
// fail-open）**刻意相反**，理由如下，请勿"统一"成 fail-open：
//
//   - 配额是 **token**，超发的代价是"某个 key 多用了 N 个 token"。
//     有硬上限（quota），且下一次预检就会挡住继续超发。代价有界、可恢复。
//   - 余额是**钱**，漏放一次 = 一次上游调用白送，而用户**永远不会知道**
//     （没有对账单、没有报错、余额悄悄归零他也不确定是花掉的还是别的原因）。
//     且**没有兜底**：配额漏了，下次 used>quota 会被挡住；余额漏了，钱就没了。
//
// 更关键的是**不对称性**：fail-open 的风险是"偶发的可用性损失"（读库抖动时
// 短暂拒绝用户，可重试即恢复）；fail-closed 的风险是"偶发的资金损失"
// （读库抖动时放行，白送一次调用）。前者随抖动自愈，后者在网关重启前都不会
// 被任何人发现。因此宁可误拒（用户看到明确错误 + 日志，重试即可）不可漏扣。
//
// 代价与缓解：读库失败会误拒正常流量，所以这一路必须**记 ERROR**（fail-open
// 的教训是"记日志才看得见"；这里同理，只是反过来——记录是为了发现抖动）。
//
// # 管理员的短路在最前面（纵深防御，见 balanceExempt）
//
// balanceExempt 为真时立刻返回 true，**一次库都不查**。口径要求是管理员
// 完全不受余额约束；2026-10 后管理员在数据面已被 auth 拦截，这条短路实际
// 不可达，**保留**是为了纵深防御（万一某个入口绕过 auth 直达预检，管理员
// 也不会因余额查询被误拒或读库抖动而受影响）。详见 balanceExempt 的注释。
func precheckBalance(w http.ResponseWriter, r *http.Request, db *store.Store, logger *slog.Logger,
	authCtx *auth.Context, ing *ingressRequest, cands []routing.Candidate,
	codec ingressCodec) bool {

	if balanceExempt(authCtx.UserID) {
		return true // 纵深防御：管理员不读库、不估算、不判定（数据面上不可达，见 balanceExempt）
	}

	// 先估算，再决定要不要读库。顺序有讲究：未配价模型估算恒 0，而未配价
	// 是**放行**的（Lead 口径），那么这类用户根本不该因为「余额读不出来」
	// 而被拒 —— 不读库就不可能读失败。这顺带消掉了「没配价的部署被余额
	// 功能整体拖垮」的可能。
	//
	// 但「估算为 0」有**两种**含义，不能混为一谈：
	//
	//  1. 整条链都没配价 → 本次调用**真的不计费**（cost_total 恒 0），
	//     余额不变。这种才允许直接放行、不读库。
	//  2. 模型配了价，但这次请求小到四舍五入算成 0 分（输入两三个字、
	//     客户端没传 max_tokens、模型又没配 MaxOutputTokens）—— 扣费几乎
	//     为零，但**不能据此放行**：那等于「余额已经欠费的用户只要发足够小的
	//     请求就能一直用下去」，预检形同虚设。所以这种情况**照样读余额**，
	//     判定时按「余额是否达到地板线」而不是「够不够付本次」——
	//     地板线见 billing.MinSubcentSpendableCents 的说明。
	//
	// 区分方式：链上是否存在**已配价**的模型（而不是估算值是否大于 0）。
	est := billing.EstimateBalanceCostChain(ing.buildRosetta(), cands)
	chainPriced := billing.ChainHasPricedModel(cands)
	if est <= 0 && !chainPriced {
		return true // 整条链都没配价：本次真的不计费，不读库
	}

	cents, limited, err := db.BalanceOf(r.Context(), authCtx.UserID)
	if err != nil {
		// fail-closed（理由见本函数注释）。
		logger.Error("balance lookup failed, rejecting (fail-closed)",
			"error", err, "user_id", authCtx.UserID, "key_id", authCtx.KeyID,
			"estimated_cents", est, "request_id", server.RequestIDFromContext(r.Context()))
		codec.WriteError(w, http.StatusInternalServerError, "internal_error",
			"balance check unavailable")
		return false
	}

	switch billing.Preflight(est, cents, !limited, false, chainPriced) {
	case billing.Reject:
		logger.Warn("insufficient balance",
			"user_id", authCtx.UserID, "key_id", authCtx.KeyID,
			"estimated_cents", est, "balance_cents", cents,
			"request_id", server.RequestIDFromContext(r.Context()))
		codec.WriteError(w, http.StatusPaymentRequired, "insufficient_balance",
			"insufficient balance: this request would cost more than your remaining balance, "+
				"please top up your account")
		return false
	default:
		return true
	}
}
