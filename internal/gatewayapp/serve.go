// Package gatewayapp owns the gateway's process lifecycle.
package gatewayapp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/cn-maul/rosetta-gateway/internal/config"
	"github.com/cn-maul/rosetta-gateway/internal/lifecycle"
	"github.com/cn-maul/rosetta-gateway/internal/probe"
	"github.com/cn-maul/rosetta-gateway/internal/runtime"
	"github.com/cn-maul/rosetta-gateway/internal/store"
	"github.com/cn-maul/rosetta-gateway/internal/usage"
)

// ServeDeps contains the initialized dependencies needed to serve traffic and
// run the periodic maintenance loops.
type ServeDeps struct {
	DB              *store.Store
	Config          *config.Config
	Logger          *slog.Logger
	MasterKey       []byte
	Reloader        *runtime.Reloader
	Usage           *usage.Recorder
	Handler         http.Handler
	Lifecycle       *lifecycle.Manager
	Budget          lifecycle.ShutdownBudget
	ReloadRetryDone <-chan struct{}
	// Stop lets tests drive the same shutdown path as a termination signal.
	// Production leaves it nil and listens for SIGINT/SIGTERM.
	Stop <-chan struct{}
}

// Serve starts the HTTP server and maintenance loops, then blocks until a
// termination signal or a server error. The shutdown order is intentional:
// stop writers before closing the store, then drain pending usage.
func Serve(deps ServeDeps) error {
	db, cfg, logger := deps.DB, deps.Config, deps.Logger
	budget := deps.Budget
	if budget.Nominal() == 0 {
		budget = lifecycle.DefaultShutdownBudget()
	}
	if deps.Lifecycle != nil {
		deps.Lifecycle.SetPhase(lifecycle.PhaseStarting)
	}
	shutdown := make(chan struct{})

	probeCtx, stopProbe := context.WithCancel(context.Background())
	defer stopProbe()

	// Bind before starting any maintenance worker or publishing ready. A
	// startup failure must not leave a ready window or untracked background
	// writes behind.
	listener, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return fmt.Errorf("listen %s: %w", cfg.Listen, err)
	}

	srv := &http.Server{
		Addr:        cfg.Listen,
		Handler:     deps.Handler,
		ReadTimeout: 30 * time.Second,
		// WriteTimeout must stay zero: net/http applies it to the whole response,
		// which would cut healthy long-running streams. Stream lifetime is bounded
		// by the TTFT and idle watchdogs instead.
		WriteTimeout:      0,
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}

	serverErr := make(chan error, 1)
	go func() {
		logger.Info("server starting", "addr", cfg.Listen)
		if err := srv.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
	}()

	pruneDone := startPruner(db, cfg, logger, shutdown, deps.Lifecycle)
	recomputeDone := startRecompute(db, logger, shutdown, deps.Lifecycle)
	reconcileDone := startReconcile(db, logger, shutdown, deps.Lifecycle)
	probeDone := startProbe(db, cfg, deps.MasterKey, logger, probeCtx, deps.Lifecycle)

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(quit)

	var runErr error
	if deps.Lifecycle != nil {
		deps.Lifecycle.SetPhase(lifecycle.PhaseReady)
	}
	select {
	case <-quit:
	case <-deps.Stop:
	case runErr = <-serverErr:
		logger.Error("server error", "error", runErr)
	}

	// Stop every background writer before closing the store.
	if deps.Lifecycle != nil {
		deps.Lifecycle.SetPhase(lifecycle.PhaseStopping)
	}
	close(shutdown)
	stopProbe()
	allJoined := waitLoop(pruneDone, budget.UsagePrune,
		logger, "usage prune still running at shutdown; exiting anyway")
	allJoined = waitLoop(recomputeDone, budget.CostRecompute,
		logger, "cost recompute still running at shutdown; exiting anyway") && allJoined
	allJoined = waitLoop(reconcileDone, budget.ChargeReconcile,
		logger, "charge reconciliation still running at shutdown; exiting anyway") && allJoined
	allJoined = waitLoop(probeDone, budget.ProbeJoin,
		logger, "probe runner still running at shutdown") && allJoined

	logger.Info("shutting down...")
	ctx, cancel := context.WithTimeout(context.Background(), budget.HTTPDrain)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		logger.Error("graceful shutdown incomplete; forcing close of in-flight connections",
			"error", err, "note", "streams longer than the shutdown budget are terminated")
		if cerr := srv.Close(); cerr != nil {
			logger.Error("forced close failed", "error", cerr)
		}
	}

	if !allJoined {
		runErr = errors.Join(runErr, ErrWorkersNotJoined)
	}
	logger.Info("server stopped")
	return runErr
}

// ErrWorkersNotJoined prevents the owning process from closing storage while
// a registered worker may still be inside database/sql.
var ErrWorkersNotJoined = errors.New("background workers did not join before shutdown deadline")

func waitLoop(done <-chan struct{}, timeout time.Duration, logger *slog.Logger, message string) bool {
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		logger.Warn(message)
		return false
	}
}

func maintenanceContext(shutdown <-chan struct{}) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		select {
		case <-shutdown:
			cancel()
		case <-ctx.Done():
		}
	}()
	return ctx, cancel
}

func startPruner(db *store.Store, cfg *config.Config, logger *slog.Logger,
	shutdown <-chan struct{}, lifecycle *lifecycle.Manager) <-chan struct{} {
	done := make(chan struct{})
	if lifecycle != nil {
		lifecycle.AddWorkers(1)
	}
	go func() {
		defer close(done)
		if lifecycle != nil {
			defer lifecycle.WorkerExited()
		}
		ctx, cancel := maintenanceContext(shutdown)
		defer cancel()
		pruneAudit := func(reason string) {
			// 0 and -1 both mean permanent retention. Treating 0 as an
			// omitted value here would silently prune a compliance setting.
			if cfg.AuditLogKeepRows == 0 || cfg.AuditLogKeepRows < 0 {
				return
			}
			n, err := db.PruneAuditLog(ctx, cfg.AuditLogKeepRows)
			if err != nil {
				logger.Error("audit log prune failed", "error", err, "trigger", reason)
				return
			}
			if n > 0 {
				logger.Info("audit log pruned", "trigger", reason,
					"deleted_rows", n, "keep_rows", cfg.AuditLogKeepRows)
			}
		}
		runPrune := func(reason string) bool {
			if n, err := db.SweepExpiredQuotaReservations(ctx, time.Now()); err != nil {
				logger.Warn("expired quota reservation sweep failed", "error", err, "trigger", reason)
			} else if n > 0 {
				logger.Warn("recovered expired quota reservation leases",
					"requests", n, "trigger", reason)
			}
			started := time.Now()
			var deleted, rollups int64
			for batch := 0; batch < 10 && time.Since(started) < 30*time.Second; batch++ {
				batchCtx, batchCancel := context.WithTimeout(ctx, 15*time.Second)
				res, err := db.PruneOldUsage(batchCtx, cfg.UsageRetentionDays)
				batchCancel()
				if err != nil {
					logger.Error("usage prune failed", "error", err, "trigger", reason,
						"batches_completed", batch, "deleted_rows", deleted)
					pruneAudit(reason)
					return false
				}
				if res.Skipped {
					if deleted > 0 {
						logger.Info("usage pruned into daily rollups", "trigger", reason,
							"deleted_rows", deleted, "rollup_rows", rollups,
							"pruned_through_day", res.PrunedThrough)
					} else {
						logger.Debug("usage prune skipped", "reason", res.Reason, "trigger", reason)
					}
					pruneAudit(reason)
					return true
				}
				deleted += res.DeletedRows
				rollups += res.RollupRows
			}
			logger.Warn("usage prune backlog remains; retrying sooner",
				"trigger", reason, "deleted_rows", deleted, "rollup_rows", rollups,
				"retry_in", 15*time.Minute)
			pruneAudit(reason)
			return false
		}

		// Do not compete with request-path quota reservations and usage writes
		// immediately after process start.
		timer := time.NewTimer(30 * time.Second)
		defer timer.Stop()
		for {
			select {
			case <-timer.C:
				complete := runPrune("scheduled")
				if complete {
					timer.Reset(24 * time.Hour)
				} else {
					timer.Reset(15 * time.Minute)
				}
			case <-shutdown:
				return
			}
		}
	}()
	return done
}

func startRecompute(db *store.Store, logger *slog.Logger, shutdown <-chan struct{},
	lifecycle *lifecycle.Manager) <-chan struct{} {
	done := make(chan struct{})
	if lifecycle != nil {
		lifecycle.AddWorkers(1)
	}
	go func() {
		defer close(done)
		if lifecycle != nil {
			defer lifecycle.WorkerExited()
		}
		ctx, cancel := maintenanceContext(shutdown)
		defer cancel()
		runRecompute := func(reason string) {
			for round := 0; round < 10; round++ {
				fixed, remaining, err := db.RecomputePendingCosts(ctx, 500)
				if err != nil {
					logger.Error("pending cost recompute failed", "error", err, "trigger", reason)
					return
				}
				if fixed > 0 {
					logger.Info("recomputed pending usage costs",
						"trigger", reason, "fixed", fixed, "remaining", remaining)
				}
				if !remaining {
					return
				}
			}
			logger.Warn("pending cost recompute backlog still large after 10 rounds",
				"trigger", reason, "batch_size", 500)
		}
		// Same reasoning as the pruner: give the first requests the single
		// write connection before a background batch takes it.
		timer := time.NewTimer(45 * time.Second)
		defer timer.Stop()
		for {
			select {
			case <-timer.C:
				runRecompute("periodic")
				timer.Reset(5 * time.Minute)
			case <-shutdown:
				return
			}
		}
	}()
	return done
}

func startReconcile(db *store.Store, logger *slog.Logger, shutdown <-chan struct{},
	lifecycle *lifecycle.Manager) <-chan struct{} {
	done := make(chan struct{})
	if lifecycle != nil {
		lifecycle.AddWorkers(1)
	}
	go func() {
		defer close(done)
		if lifecycle != nil {
			defer lifecycle.WorkerExited()
		}
		ctx, cancel := maintenanceContext(shutdown)
		defer cancel()
		runReconcile := func(reason string) {
			result, err := db.ReconcileUnchargedUsage(ctx, 500)
			if err != nil {
				logger.Error("charge reconciliation scan failed", "error", err, "trigger", reason)
				return
			}
			if result.Charged == 0 && result.Deferred == 0 && result.Failed == 0 {
				return
			}
			logger.Warn("reconciled uncharged usage records",
				"trigger", reason, "charged", result.Charged,
				"deferred", result.Deferred, "failed", result.Failed)
		}
		// Last of the three: reconciliation rewrites money, so let the pruner
		// and recompute finish their first pass first.
		timer := time.NewTimer(time.Minute)
		defer timer.Stop()
		for {
			select {
			case <-timer.C:
				runReconcile("periodic")
				timer.Reset(10 * time.Minute)
			case <-shutdown:
				return
			}
		}
	}()
	return done
}

func startProbe(db *store.Store, cfg *config.Config, masterKey []byte, logger *slog.Logger,
	ctx context.Context, lifecycle *lifecycle.Manager) <-chan struct{} {
	done := make(chan struct{})
	if !cfg.Probes.Enabled {
		close(done)
		return done
	}
	if lifecycle != nil {
		lifecycle.AddWorkers(1)
	}
	go func() {
		defer close(done)
		if lifecycle != nil {
			defer lifecycle.WorkerExited()
		}
		probe.NewRunner(db, cfg, masterKey, logger).Run(ctx)
	}()
	return done
}
