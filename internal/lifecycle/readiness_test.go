package lifecycle_test

import (
	"context"
	"errors"
	"testing"

	"github.com/cn-maul/rosetta-gateway/internal/lifecycle"
)

type pingFunc func(context.Context) error

func (f pingFunc) Ping(ctx context.Context) error { return f(ctx) }

type runtimeFunc func() bool

func (f runtimeFunc) IsDirty() bool { return f() }

func TestR1_ReadinessReportsIndependentDimensions(t *testing.T) {
	dbErr := errors.New("write pool unavailable")
	dirty := false
	manager := lifecycle.NewManager(
		pingFunc(func(context.Context) error { return nil }),
		runtimeFunc(func() bool { return dirty }),
	)
	manager.SetBootstrap(lifecycle.BootstrapReady)

	got := manager.Readiness(context.Background())
	if got.Status != "ok" || got.Database != lifecycle.DimensionOK ||
		got.Runtime != lifecycle.DimensionOK || got.Bootstrap != lifecycle.DimensionOK {
		t.Fatalf("healthy readiness = %+v", got)
	}

	dirty = true
	got = manager.Readiness(context.Background())
	if got.Status != "degraded" || got.ErrorCode != "runtime_lags_behind_database" ||
		got.Runtime != lifecycle.DimensionPending {
		t.Fatalf("dirty readiness = %+v", got)
	}

	dirty = false
	manager = lifecycle.NewManager(
		pingFunc(func(context.Context) error { return dbErr }),
		runtimeFunc(func() bool { return true }),
	)
	manager.SetBootstrap(lifecycle.BootstrapFailed)
	got = manager.Readiness(context.Background())
	if got.Status != "unhealthy" || got.ErrorCode != "database_unavailable" ||
		got.Database != lifecycle.DimensionFailed ||
		got.Runtime != lifecycle.DimensionPending ||
		got.Bootstrap != lifecycle.DimensionFailed {
		t.Fatalf("multi-dimension failure = %+v", got)
	}
}

func TestR1_ReadinessDoesNotReportReadyWhileWorkersDrain(t *testing.T) {
	manager := lifecycle.NewManager(
		pingFunc(func(context.Context) error { return nil }),
		runtimeFunc(func() bool { return false }),
	)
	manager.SetBootstrap(lifecycle.BootstrapReady)
	manager.SetPhase(lifecycle.PhaseStopping)
	manager.AddWorkers(1)

	got := manager.Readiness(context.Background())
	if got.Status != "degraded" || got.ErrorCode != "workers_draining" {
		t.Fatalf("stopping readiness = %+v", got)
	}

	manager.WorkerExited()
	if got := manager.PendingWorkers(); got != 0 {
		t.Fatalf("pending workers after exit = %d, want 0", got)
	}
	manager.MarkWorkersJoined()
	got = manager.Readiness(context.Background())
	if got.ErrorCode != "service_stopping" {
		t.Fatalf("joined-but-not-stopped readiness = %+v", got)
	}
}

func TestR1_JoinBarrierRequiresBothExitEvidenceAndZeroCount(t *testing.T) {
	manager := lifecycle.NewManager(
		pingFunc(func(context.Context) error { return nil }),
		runtimeFunc(func() bool { return false }),
	)
	manager.AddWorkers(2)
	manager.MarkWorkersJoined()
	if manager.WorkersJoined() {
		t.Fatal("mark-joined cannot override two live workers")
	}
	manager.WorkerExited()
	if manager.WorkersJoined() {
		t.Fatal("join barrier became true with one worker still live")
	}
	manager.WorkerExited()
	if !manager.WorkersJoined() {
		t.Fatal("join barrier stayed closed after every worker exited")
	}
}

func TestR1_ShutdownBudgetCoversNominalAndUpperBound(t *testing.T) {
	budget := lifecycle.DefaultShutdownBudget()
	if got := budget.Nominal().Seconds(); got != 26 {
		t.Fatalf("nominal shutdown budget = %.0fs, want 26s", got)
	}
	if got := budget.UpperBound().Seconds(); got != 30 {
		t.Fatalf("shutdown upper bound = %.0fs, want 30s", got)
	}
	if budget.ComposeGracePeriod() <= budget.UpperBound() {
		t.Fatalf("compose grace %s must exceed upper bound %s",
			budget.ComposeGracePeriod(), budget.UpperBound())
	}
}
