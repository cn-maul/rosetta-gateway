// Package gatewayapp owns the gateway's process lifecycle.
package gatewayapp

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/cn-maul/rosetta-gateway/internal/config"
	"github.com/cn-maul/rosetta-gateway/internal/probe"
	"github.com/cn-maul/rosetta-gateway/internal/runtime"
	"github.com/cn-maul/rosetta-gateway/internal/store"
	"github.com/cn-maul/rosetta-gateway/internal/usage"
)

// ServeDeps contains the initialized dependencies needed to serve traffic and
// run the periodic maintenance loops.
type ServeDeps struct {
	DB        *store.Store
	Config    *config.Config
	Logger    *slog.Logger
	MasterKey []byte
	Reloader  *runtime.Reloader
	Usage     *usage.Recorder
	Handler   http.Handler
}

// Serve starts the HTTP server and maintenance loops, then blocks until a
// termination signal or a server error. The shutdown order is intentional:
// stop writers before closing the store, then drain pending usage.
func Serve(deps ServeDeps) error {
	db, cfg, logger := deps.DB, deps.Config, deps.Logger
	shutdown := make(chan struct{})

	probeCtx, stopProbe := context.WithCancel(context.Background())
	defer stopProbe()

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
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
	}()

	pruneDone := startPruner(db, cfg, logger, shutdown)
	recomputeDone := startRecompute(db, logger, shutdown)
	reconcileDone := startReconcile(db, logger, shutdown)
	probeDone := startProbe(db, cfg, deps.MasterKey, logger, probeCtx)

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(quit)

	var runErr error
	select {
	case <-quit:
	case runErr = <-serverErr:
		logger.Error("server error", "error", runErr)
	}

	// Stop every background writer before closing the store.
	close(shutdown)
	stopProbe()
	waitLoop(pruneDone, 5*time.Second,
		logger, "usage prune still running at shutdown; exiting anyway")
	waitLoop(recomputeDone, 2*time.Second,
		logger, "cost recompute still running at shutdown; exiting anyway")
	waitLoop(reconcileDone, 2*time.Second,
		logger, "charge reconciliation still running at shutdown; exiting anyway")
	waitLoop(probeDone, 2*time.Second,
		logger, "probe runner still running at shutdown; exiting anyway")

	logger.Info("shutting down...")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		logger.Error("graceful shutdown incomplete; forcing close of in-flight connections",
			"error", err, "note", "streams longer than the shutdown budget are terminated")
		if cerr := srv.Close(); cerr != nil {
			logger.Error("forced close failed", "error", cerr)
		}
	}

	drainCtx, drainCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer drainCancel()
	if !deps.Usage.Wait(drainCtx) {
		deps.Usage.Abort()
		logger.Error("usage drain incomplete: records were lost during shutdown",
			"hint", "check earlier 'usage record lost' / 'usage recorder aborted' errors for request_id")
	}
	logger.Info("server stopped")
	return runErr
}

func waitLoop(done <-chan struct{}, timeout time.Duration, logger *slog.Logger, message string) {
	select {
	case <-done:
	case <-time.After(timeout):
		logger.Warn(message)
	}
}

func startPruner(db *store.Store, cfg *config.Config, logger *slog.Logger, shutdown <-chan struct{}) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		pruneAudit := func(reason string) {
			if cfg.AuditLogKeepRows <= 0 {
				return
			}
			n, err := db.PruneAuditLog(context.Background(), cfg.AuditLogKeepRows)
			if err != nil {
				logger.Error("audit log prune failed", "error", err, "trigger", reason)
				return
			}
			if n > 0 {
				logger.Info("audit log pruned", "trigger", reason,
					"deleted_rows", n, "keep_rows", cfg.AuditLogKeepRows)
			}
		}
		runPrune := func(reason string) {
			res, err := db.PruneOldUsage(context.Background(), cfg.UsageRetentionDays)
			switch {
			case err != nil:
				logger.Error("usage prune failed", "error", err, "trigger", reason)
			case res.Skipped:
				logger.Debug("usage prune skipped", "reason", res.Reason, "trigger", reason)
			default:
				logger.Info("usage pruned into daily rollups", "trigger", reason,
					"deleted_rows", res.DeletedRows, "rollup_rows", res.RollupRows,
					"pruned_through_day", res.PrunedThrough)
			}
			pruneAudit(reason)
		}
		runPrune("startup")
		t := time.NewTicker(24 * time.Hour)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				runPrune("daily")
			case <-shutdown:
				return
			}
		}
	}()
	return done
}

func startRecompute(db *store.Store, logger *slog.Logger, shutdown <-chan struct{}) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		runRecompute := func(reason string) {
			for round := 0; round < 10; round++ {
				fixed, remaining, err := db.RecomputePendingCosts(context.Background(), 500)
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
		runRecompute("startup")
		tk := time.NewTicker(5 * time.Minute)
		defer tk.Stop()
		for {
			select {
			case <-tk.C:
				runRecompute("periodic")
			case <-shutdown:
				return
			}
		}
	}()
	return done
}

func startReconcile(db *store.Store, logger *slog.Logger, shutdown <-chan struct{}) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		runReconcile := func(reason string) {
			found, err := db.FindUnchargedUsage(context.Background(), 500)
			if err != nil {
				logger.Error("charge reconciliation scan failed", "error", err, "trigger", reason)
				return
			}
			if len(found) == 0 {
				return
			}
			var charged, failed int
			for _, u := range found {
				if cerr := db.ChargeUnchargedUsage(context.Background(), u); cerr != nil {
					failed++
					logger.Error("charge reconciliation failed to charge",
						"error", cerr, "record_id", u.RecordID,
						"user_id", u.UserID, "request_id", u.RequestID,
						"cost_yuan", u.CostYuan)
					continue
				}
				charged++
			}
			logger.Warn("reconciled uncharged usage records",
				"trigger", reason, "found", len(found), "charged", charged, "failed", failed)
		}
		runReconcile("startup")
		tk := time.NewTicker(10 * time.Minute)
		defer tk.Stop()
		for {
			select {
			case <-tk.C:
				runReconcile("periodic")
			case <-shutdown:
				return
			}
		}
	}()
	return done
}

func startProbe(db *store.Store, cfg *config.Config, masterKey []byte, logger *slog.Logger, ctx context.Context) <-chan struct{} {
	done := make(chan struct{})
	if !cfg.Probes.Enabled {
		close(done)
		return done
	}
	go func() {
		defer close(done)
		probe.NewRunner(db, cfg, masterKey, logger).Run(ctx)
	}()
	return done
}
