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
	// UsageStatusTimeout 是网关自身总预算到期，不是上游故障。
	// 单独成值是为了让报表能把「网关没来得及跑完」与「上游真的坏了」
	// 分开，避免运维按错误率去追一个不存在的上游问题。
	UsageStatusTimeout = "timeout"

	UsageStateNone        = "none"
	UsageStateMissing     = "missing"
	UsageStateReported    = "reported"
	UsageStateCostPending = "cost_pending"
)
