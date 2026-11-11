package dataplane

import (
	"github.com/cn-maul/rosetta-gateway/internal/routing"
	"github.com/cn-maul/rosetta-gateway/internal/store"
	"github.com/cn-maul/rosetta-gateway/internal/upstream"
)

// preflightCandidates returns the exact candidate set the request lane may
// attempt. Balance and output reservations must use this set, not the full
// resolved route: a disabled failover or an attempt budget can make a more
// expensive tail target unreachable.
func preflightCandidates(failoverEnabled bool, limit int, cands []routing.Candidate) []routing.Candidate {
	if limit <= 0 || limit > len(cands) {
		limit = len(cands)
	}
	if !failoverEnabled && limit > 1 {
		limit = 1
	}
	return cands[:limit]
}

// recordCommittedOutcome applies the terminal attempt to target/credential
// health. It is intentionally the only place where a committed response
// chooses success, neutral completion, or upstream failure.
func recordCommittedOutcome(pool *upstream.Pool, d LaneDecision, targetID, credID string, token upstream.ProbeToken, threshold int) {
	switch {
	case d.PenalizeUpstream:
		pool.RecordTargetFailure(targetID, token, threshold)
	case d.NeutralUpstream:
		// 只归还半开探测名额，不动 consecutiveFails。
		//
		// 不能走下面的 RecordTargetSuccess：那会把这个目标既有的连续失败
		// 清零。上游返回脏数据时清零，等于「坏一次成功一次」的目标永远
		// 熔断不了，熔断器被自己的成功路径绕过了。
		// 也不能按失败记（见 OutcomeCommittedMalformed 的说明）。
		pool.ReleaseTargetProbe(targetID, token)
	default:
		pool.RecordTargetSuccess(targetID, token)
		pool.RecordCredentialSuccess(credID)
	}
}

// committedKindFor 决定一次已提交的流该归到哪一类终态。
//
// 主判据是落库的 status，只有一个例外需要看 code：同样落成 status=error 的
// 已提交失败里，「上游在终止事件上给出自相矛盾的数据」必须与「上游写到一半
// 断了/回了错误」分开——前者是上游实现缺陷（元数据脏），不构成目标不可用的
// 证据，按普通失败记账会让一家中转被三次请求熔断（见
// OutcomeCommittedMalformed）。
func committedKindFor(status, errorCode string) OutcomeKind {
	switch status {
	case store.UsageStatusOK:
		return OutcomeCommittedSuccess
	case store.UsageStatusOverflow:
		return OutcomeGatewayOverflow
	case store.UsageStatusTimeout:
		return OutcomeGatewayTimeout
	case store.UsageStatusCanceled:
		return OutcomeClientAborted
	case store.UsageStatusError, store.UsageStatusTruncated:
		if errorCode == errorCodeStreamMalformed {
			return OutcomeCommittedMalformed
		}
		return OutcomeCommittedPartial
	default:
		return OutcomeCommittedPartial
	}
}
