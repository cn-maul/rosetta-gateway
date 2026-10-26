package usage

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cn-maul/rosetta-gateway/internal/store"
)

// R3 / M09-01 failure-first: abandonment logging samples request IDs, but
// reservation callbacks are lifecycle work and must run once for every job.
func TestR3_M09_01AbandonedBatchReleasesEveryQuotaCallback(t *testing.T) {
	db, err := store.Open(t.TempDir()+"/gw.db", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	rec := New(Config{
		DB:     db,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		BatchInsert: func(context.Context, []*store.UsageRecord) ([]float64, error) {
			return nil, errors.New("injected persistent write failure")
		},
	})
	if err := db.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	var callbacks atomic.Int32
	const n = 25
	for i := 0; i < n; i++ {
		rec.RecordWithCallback(&store.UsageRecord{
			ID: "m09-" + itoa(i), AccessKeyID: "k1", UserID: "u1",
			PublicModel: "m", ProviderID: "p1", UpstreamModel: "m",
			IngressProtocol: "chat", UsageState: store.UsageStateReported,
			Status: store.UsageStatusOK, HTTPStatus: 200,
			RequestID: "req-m09-" + itoa(i),
		}, func() { callbacks.Add(1) })
	}

	deadline := time.Now().Add(5 * time.Second)
	for callbacks.Load() != 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	// All writes fail permanently; wait until the worker owns the full batch.
	for !rec.HasPendingUsage() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if rec.Wait(cancelled) {
		t.Fatal("wait reported a clean drain for permanently failed writes")
	}
	rec.Abort()
	select {
	case <-rec.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("worker did not exit after Abort")
	}
	if got := callbacks.Load(); got != n {
		t.Fatalf("quota callbacks = %d, want %d", got, n)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
