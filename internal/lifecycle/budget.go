package lifecycle

import "time"

// ShutdownBudget is the single source for graceful shutdown budgets. Waits
// are sequential. The nominal total excludes abort/join work that only runs
// after a dependency fails to join within its own deadline.
type ShutdownBudget struct {
	UsagePrune       time.Duration
	CostRecompute    time.Duration
	ChargeReconcile  time.Duration
	ProbeJoin        time.Duration
	HTTPDrain        time.Duration
	UsageDrain       time.Duration
	UsageAbortJoin   time.Duration
	RuntimeRetryJoin time.Duration
}

// DefaultShutdownBudget returns the process shutdown budget.
func DefaultShutdownBudget() ShutdownBudget {
	return ShutdownBudget{
		UsagePrune:       5 * time.Second,
		CostRecompute:    2 * time.Second,
		ChargeReconcile:  2 * time.Second,
		ProbeJoin:        2 * time.Second,
		HTTPDrain:        10 * time.Second,
		UsageDrain:       5 * time.Second,
		UsageAbortJoin:   2 * time.Second,
		RuntimeRetryJoin: 2 * time.Second,
	}
}

// ComposeGracePeriod is the complete outer process budget. It is intentionally
// greater than UpperBound so Docker does not SIGKILL during the abort/join
// path; callers that need more time must change this constant, not only the
// individual waits.
func (b ShutdownBudget) ComposeGracePeriod() time.Duration {
	return b.UpperBound() + time.Second
}

// Nominal is the sum of waits that run on every normal shutdown.
func (b ShutdownBudget) Nominal() time.Duration {
	return b.UsagePrune + b.CostRecompute + b.ChargeReconcile + b.ProbeJoin +
		b.HTTPDrain + b.UsageDrain
}

// UpperBound includes the two join deadlines that run only after an earlier
// deadline expires.
func (b ShutdownBudget) UpperBound() time.Duration {
	return b.Nominal() + b.UsageAbortJoin + b.RuntimeRetryJoin
}
