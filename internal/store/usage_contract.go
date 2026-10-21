package store

// Usage record status and usage-state vocabulary.
//
// These values are persisted as text and are read by success-rate reports,
// quota reconciliation, and balance charging. Keeping the vocabulary here
// prevents the write path and the many SQL readers from drifting silently.
// This is not a schema change: the stored strings remain identical.
const (
	UsageStatusOK        = "ok"
	UsageStatusError     = "error"
	UsageStatusCanceled  = "canceled"
	UsageStatusTruncated = "truncated"
	UsageStatusOverflow  = "overflow"

	UsageStateNone        = "none"
	UsageStateMissing     = "missing"
	UsageStateReported    = "reported"
	UsageStateCostPending = "cost_pending"
)
