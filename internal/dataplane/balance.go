package dataplane

import (
	"net/http"

	"github.com/cn-maul/rosetta-gateway/internal/auth"
	"github.com/cn-maul/rosetta-gateway/internal/billing"
	"github.com/cn-maul/rosetta-gateway/internal/httpx"
	"github.com/cn-maul/rosetta-gateway/internal/ingress"
	"github.com/cn-maul/rosetta-gateway/internal/routing"
)

// Balance handling has two deliberately different accounting meanings:
//
//   - usage_records.cost_total records upstream cost for every call, including
//     failures and interrupted streams;
//   - balance_cents is charged only for successful calls and represents what
//     the user owes.
//
// The admission estimate answers only "can this call start?". The actual charge
// comes from the cost_total fixed when the usage row is persisted. Admins are
// exempt from balance checks as defence in depth, although data-plane auth
// already rejects admin API keys.
//
// The estimate must read the balance even when it rounds to zero if any model
// in the chain has a price; otherwise an exhausted account could keep sending
// sub-cent requests. "No model in the chain is priced" is the only unconditional
// allow path.

// precheckBalance performs the balance admission check before touching any
// upstream. A false result means the error response has already been written.
//
// The check is deliberately fail-closed: a failed balance lookup rejects the
// request. Token quota checks fail open because their worst case is bounded by
// the quota itself, while a missed balance check spends real money and cannot
// be detected afterwards. Keep the asymmetry unless the product decision
// changes.
func (h *Handler) precheckBalance(
	w http.ResponseWriter,
	r *http.Request,
	authCtx *auth.Context,
	ing *ingress.Request,
	cands []routing.Candidate,
	codec ingress.Codec,
) bool {
	if h.balanceExempt(authCtx.UserID) {
		return true
	}

	est := billing.EstimateBalanceCostChain(ing.BuildRosetta(), cands)
	chainPriced := billing.ChainHasPricedModel(cands)
	if est <= 0 && !chainPriced {
		return true
	}

	cents, limited, err := h.db.BalanceOf(r.Context(), authCtx.UserID)
	if err != nil {
		h.logger.Error("balance lookup failed, rejecting (fail-closed)",
			"error", err, "user_id", authCtx.UserID, "key_id", authCtx.KeyID,
			"estimated_cents", est, "request_id", httpx.RequestIDFromContext(r.Context()))
		codec.WriteError(w, http.StatusInternalServerError, "internal_error",
			"balance check unavailable")
		return false
	}

	switch billing.Preflight(est, cents, !limited, false, chainPriced) {
	case billing.Reject:
		h.logger.Warn("insufficient balance",
			"user_id", authCtx.UserID, "key_id", authCtx.KeyID,
			"estimated_cents", est, "balance_cents", cents,
			"request_id", httpx.RequestIDFromContext(r.Context()))
		codec.WriteError(w, http.StatusPaymentRequired, "insufficient_balance",
			"insufficient balance: this request would cost more than your remaining balance, "+
				"please top up your account")
		return false
	default:
		return true
	}
}

func (h *Handler) balanceExempt(userID string) bool {
	u := h.snapshot().UsersByID[userID]
	return u != nil && billing.IsBalanceExempt(u.Role)
}
