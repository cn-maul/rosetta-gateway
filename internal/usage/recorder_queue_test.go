package usage

// 队列满时的行为回归：先等空位（背压），等不到才同步写。
//
// 这两条测试锁的是一个**曾经把吞吐打掉 3.7 倍**的形状（2026-11）：
// 队列一满就在请求线程做单行事务，抢走批量 worker 的唯一写连接，
// worker 发不出批 → 队列更满 → 更多同步写。正反馈一旦触发就回不来。

import (
	"context"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cn-maul/rosetta-gateway/internal/store"
)

func queueTestRecord(i int) *store.UsageRecord {
	return &store.UsageRecord{
		ID: "q-" + itoa(i), AccessKeyID: "k1", UserID: "u1",
		PublicModel: "m", ProviderID: "p1", UpstreamModel: "m",
		IngressProtocol: "chat", UsageState: store.UsageStateReported,
		Status: store.UsageStatusOK, HTTPStatus: 200,
		RequestID: "req-q-" + itoa(i),
	}
}

func newQueueTestRecorder(t *testing.T, wait time.Duration) (*Recorder, chan struct{}, chan struct{}) {
	t.Helper()
	db, err := store.Open(t.TempDir()+"/gw.db", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	// entered 在 worker 第一次进入批量写入时关闭；release 放它通过。
	entered := make(chan struct{})
	release := make(chan struct{})
	var once atomic.Bool
	rec := New(Config{
		DB:          db,
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		EnqueueWait: wait,
		BatchInsert: func(ctx context.Context, recs []*store.UsageRecord) ([]float64, error) {
			if once.CompareAndSwap(false, true) {
				close(entered)
				<-release
			}
			costs := make([]float64, len(recs))
			return costs, nil
		},
	})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
		rec.CloseQueue()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		rec.Wait(ctx)
	})
	return rec, entered, release
}

// 队列满时第 N+1 条必须**阻塞等待**，不得立刻做同步写。
func TestRecorder_QueueFullBlocksInsteadOfSyncWrite(t *testing.T) {
	rec, entered, release := newQueueTestRecorder(t, time.Second)

	var done atomic.Int32
	finish := func() { done.Add(1) }

	// 第一条把 worker 送进批量写入并卡住。
	rec.RecordWithCallback(queueTestRecord(0), finish)
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("worker never entered batch insert")
	}

	// 灌满队列：这些都必须立刻入队成功（队列还有空位）。
	for i := 1; i <= QueueSize; i++ {
		rec.RecordWithCallback(queueTestRecord(i), finish)
	}
	if got := len(rec.queue); got != QueueSize {
		t.Fatalf("queue length = %d, want %d", got, QueueSize)
	}

	// 再来一条：队列已满，它必须停在这里等，而不是自己写库。
	returned := make(chan struct{})
	go func() {
		rec.RecordWithCallback(queueTestRecord(QueueSize+1), finish)
		close(returned)
	}()
	select {
	case <-returned:
		t.Fatal("queue-full record returned immediately: it took the synchronous-write path instead of waiting")
	case <-time.After(150 * time.Millisecond):
		// 期望：仍在等空位。
	}
	if n := rec.syncFallbacks.Load(); n != 0 {
		t.Fatalf("sync fallbacks = %d during the wait window, want 0", n)
	}

	// 放行 worker，队列开始排空，等待中的那条应当入队并最终落库。
	close(release)
	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		t.Fatal("queue-full record never returned after the worker resumed")
	}
	deadline := time.Now().Add(5 * time.Second)
	for done.Load() != int32(QueueSize+2) && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := done.Load(); got != int32(QueueSize+2) {
		t.Fatalf("callbacks = %d, want %d", got, QueueSize+2)
	}
	if n := rec.syncFallbacks.Load(); n != 0 {
		t.Fatalf("sync fallbacks = %d, want 0 (no synchronous write should happen)", n)
	}
}

// 整段等待窗口都没有空位（worker 停摆）才允许同步写兜底，且必须计数。
func TestRecorder_EnqueueTimeoutFallsBackToSyncWrite(t *testing.T) {
	rec, entered, release := newQueueTestRecorder(t, 30*time.Millisecond)

	rec.RecordWithCallback(queueTestRecord(0), func() {})
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("worker never entered batch insert")
	}
	for i := 1; i <= QueueSize; i++ {
		rec.RecordWithCallback(queueTestRecord(i), func() {})
	}

	start := time.Now()
	rec.RecordWithCallback(queueTestRecord(QueueSize+1), func() {})
	waited := time.Since(start)

	if waited < 25*time.Millisecond {
		t.Fatalf("returned after %v: expected it to wait ~EnqueueWait (30ms) first", waited)
	}
	if n := rec.syncFallbacks.Load(); n != 1 {
		t.Fatalf("sync fallbacks = %d, want 1", n)
	}
	close(release)
}
