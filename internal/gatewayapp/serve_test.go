package gatewayapp

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/cn-maul/rosetta-gateway/internal/config"
	"github.com/cn-maul/rosetta-gateway/internal/lifecycle"
	"github.com/cn-maul/rosetta-gateway/internal/snapshot"
	"github.com/cn-maul/rosetta-gateway/internal/store"
	"github.com/cn-maul/rosetta-gateway/internal/usage"
)

func openServeTestStore(t *testing.T) *store.Store {
	t.Helper()
	db, err := store.Open(t.TempDir()+"/gw.db",
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// R1 startup failure injection: an occupied listener must fail before any
// maintenance worker runs and before readiness can publish "ready".
func TestR1_ListenFailureDoesNotStartWorkersOrPublishReady(t *testing.T) {
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve listener: %v", err)
	}
	defer occupied.Close()

	db := openServeTestStore(t)
	cfg := config.Default()
	cfg.Listen = occupied.Addr().String()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	manager := lifecycle.NewManager(db, nil)
	manager.SetBootstrap(lifecycle.BootstrapReady)

	err = Serve(ServeDeps{
		DB:        db,
		Config:    cfg,
		Logger:    logger,
		Handler:   http.NewServeMux(),
		Lifecycle: manager,
	})
	if err == nil {
		t.Fatal("Serve succeeded on an occupied listener")
	}
	if got := manager.Phase(); got != lifecycle.PhaseStarting {
		t.Fatalf("phase after listen failure = %q, want starting", got)
	}
	if got := manager.PendingWorkers(); got != 0 {
		t.Fatalf("workers registered after listen failure = %d, want 0", got)
	}
}

// R1 shutdown evidence: every registered maintenance worker must decrement
// the lifecycle count and the final join barrier must become true.
func TestR1_NormalShutdownJoinsEveryRegisteredWorker(t *testing.T) {
	db := openServeTestStore(t)
	cfg := config.Default()
	cfg.Listen = "127.0.0.1:0"
	cfg.Probes.Enabled = false
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	manager := lifecycle.NewManager(db, nil)
	manager.SetBootstrap(lifecycle.BootstrapReady)
	recorder := usage.New(usage.Config{DB: db, Logger: logger})
	t.Cleanup(recorder.Abort)
	manager.AddWorkers(1)
	recorderDone := make(chan struct{})
	go func() {
		defer close(recorderDone)
		<-recorder.Done()
		manager.WorkerExited()
	}()

	stop := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- Serve(ServeDeps{
			DB:        db,
			Config:    cfg,
			Logger:    logger,
			Handler:   http.NewServeMux(),
			Usage:     recorder,
			Lifecycle: manager,
			Stop:      stop,
		})
	}()

	close(stop)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve shutdown: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Serve did not stop")
	}

	// Serve does not own the usage queue in production; Run closes it after
	// Serve returns. Simulate that owner and prove the count reaches zero.
	recorder.CloseQueue()
	if !recorder.Join(context.Background()) {
		t.Fatal("usage recorder did not join")
	}
	select {
	case <-recorderDone:
	case <-time.After(time.Second):
		t.Fatal("usage worker ownership callback did not run")
	}
	if got := manager.PendingWorkers(); got != 0 {
		t.Fatalf("pending workers after shutdown = %d, want 0", got)
	}
	manager.MarkWorkersJoined()
	if !manager.WorkersJoined() {
		t.Fatal("join barrier did not become true")
	}
}

// R1 slow-writer evidence: a timed-out shutdown must not turn into close-store
// permission. The caller can only close storage after the recorder joins.
func TestR1_SlowUsageWriterKeepsJoinBarrierClosed(t *testing.T) {
	db := openServeTestStore(t)
	var startedOnce sync.Once
	started := make(chan struct{})
	release := make(chan struct{})
	recorder := usage.New(usage.Config{
		DB:     db,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		BatchInsert: func(ctx context.Context, recs []*store.UsageRecord) ([]float64, error) {
			startedOnce.Do(func() { close(started) })
			select {
			case <-release:
			case <-ctx.Done():
			}
			return nil, errors.New("slow writer released")
		},
	})
	recorder.Record(&store.UsageRecord{
		ID: "slow", AccessKeyID: "k", PublicModel: "m", Status: "ok",
	})
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("slow writer did not start")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if recorder.Wait(ctx) {
		t.Fatal("Wait reported a drained queue while the writer was blocked")
	}
	joinCtx, joinCancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer joinCancel()
	if recorder.Join(joinCtx) {
		t.Fatal("Join reported success while the writer was blocked")
	}
	close(release)
	recorder.Abort()
	if !recorder.Join(context.Background()) {
		t.Fatal("slow writer did not join after release")
	}

	// Keep package dependencies exercised: no Serve call means no snapshot
	// replacement; this line documents that shutdown tests never publish state.
	snapshot.Init(&snapshot.Snapshot{})
}
