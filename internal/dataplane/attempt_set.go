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
	if d.PenalizeUpstream {
		pool.RecordTargetFailure(targetID, token, threshold)
		return
	}
	pool.RecordTargetSuccess(targetID, token)
	pool.RecordCredentialSuccess(credID)
}

func committedKindForStatus(status string) OutcomeKind {
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
		return OutcomeCommittedPartial
	default:
		return OutcomeCommittedPartial
	}
}
