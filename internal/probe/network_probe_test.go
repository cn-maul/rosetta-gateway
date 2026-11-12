package probe

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cn-maul/rosetta-gateway/internal/config"
	"github.com/cn-maul/rosetta-gateway/internal/store"
)

// 代理测速跑在探针轮里（2026-11-01 起不再有「测试」按钮，改由后台定时测）。
//
// 这些用例钉的是「代理测速真的被调度了、失败也落库、取消时不写脏数据」，
// 而不是 generate_204 的具体实现 —— 后者在 upstream 包已有自己的用例。
// 所以测速函数走 Runner.proxyProbe 注入，不依赖外网。

func newProbeRunner(t *testing.T) (*Runner, *store.Store) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	st, err := store.Open(t.TempDir()+"/gw.db", logger)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	cfg := config.Default()
	cfg.Probes.Enabled = true
	cfg.Probes.Concurrency = 2
	cfg.Probes.TimeoutMs = 5000
	return NewRunner(st, cfg, logger), st
}

func seedProbeNetwork(t *testing.T, st *store.Store, id string) {
	t.Helper()
	if err := st.CreateNetwork(context.Background(), &store.Network{
		ID: id, Name: id, ProxyURL: "http://127.0.0.1:7897",
	}); err != nil {
		t.Fatalf("create network %s: %v", id, err)
	}
}

func seedProbeModel(t *testing.T, st *store.Store) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UnixMilli()
	if _, err := st.DB().ExecContext(ctx,
		`INSERT INTO providers (id, slug, name, protocol, endpoint, enabled, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, 1, ?, ?)`,
		"p1", "p1", "p1", "chat", "http://127.0.0.1:1/v1", now, now); err != nil {
		t.Fatalf("insert provider: %v", err)
	}
	if err := st.CreateUpstreamModel(ctx, &store.UpstreamModel{
		ID: "m1", ProviderID: "p1", ModelID: "model",
	}); err != nil {
		t.Fatalf("create model: %v", err)
	}
}

func TestRunOnce_ProbesEachNetworkExactlyOnce(t *testing.T) {
	r, st := newProbeRunner(t)
	seedProbeModel(t, st)
	seedProbeNetwork(t, st, "n1")

	var calls int32
	r.proxyProbe = func(_ context.Context, _ string) (int64, error) {
		atomic.AddInt32(&calls, 1)
		return 5, nil
	}

	r.runOnce(context.Background())

	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("一轮应只测一次代理，实际调用 %d 次", got)
	}
}

func TestProbeNetworks_RecordsSuccessAndLatency(t *testing.T) {
	r, st := newProbeRunner(t)
	ctx := context.Background()
	seedProbeNetwork(t, st, "n1")

	var calls int32
	r.proxyProbe = func(_ context.Context, _ string) (int64, error) {
		atomic.AddInt32(&calls, 1)
		return 42, nil
	}

	r.probeNetworks(ctx, 5*time.Second)

	if atomic.LoadInt32(&calls) == 0 {
		t.Fatal("代理测速函数根本没被调用 —— probeNetworks 没接上")
	}
	got, err := st.NetworkLatencyByID(ctx, 3600*1000)
	if err != nil {
		t.Fatalf("latency: %v", err)
	}
	n, ok := got["n1"]
	if !ok {
		t.Fatal("成功探测必须落库，否则界面永远显示「—」")
	}
	if n.Samples != 1 || !n.LastOK {
		t.Fatalf("应 1 条成功样本，实际 samples=%d lastOK=%v", n.Samples, n.LastOK)
	}
	if n.AvgMs != 42 {
		t.Fatalf("延迟应原样落库（42），实际 %d", n.AvgMs)
	}
}

func TestProbeNetworks_RecordsFailureWithReason(t *testing.T) {
	r, st := newProbeRunner(t)
	ctx := context.Background()
	seedProbeNetwork(t, st, "n1")

	r.proxyProbe = func(_ context.Context, _ string) (int64, error) {
		return 3000, errors.New("代理探测返回 HTTP 502")
	}

	r.probeNetworks(ctx, 2*time.Second)

	got, err := st.NetworkLatencyByID(ctx, 3600*1000)
	if err != nil {
		t.Fatalf("latency: %v", err)
	}
	n, ok := got["n1"]
	if !ok {
		t.Fatal("失败也必须落库 —— 否则界面把「代理挂了」显示成「没测过」")
	}
	if n.LastOK {
		t.Fatal("失败的代理 LastOK 必须是 false")
	}
	if n.LastError != "代理探测返回 HTTP 502" {
		t.Fatalf("失败原因必须原样落库，实际 %q", n.LastError)
	}
	if n.Samples != 0 {
		t.Fatalf("失败样本不能计入延迟平均（Samples 应 0），实际 %d", n.Samples)
	}
}

func TestProbeNetworks_SkipsWhenCancelled(t *testing.T) {
	r, st := newProbeRunner(t)
	seedProbeNetwork(t, st, "n1")

	var called int32
	r.proxyProbe = func(_ context.Context, _ string) (int64, error) {
		atomic.AddInt32(&called, 1)
		return 10, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r.probeNetworks(ctx, 5*time.Second)

	got, err := st.NetworkLatencyByID(context.Background(), 3600*1000)
	if err != nil {
		t.Fatalf("latency: %v", err)
	}
	if _, ok := got["n1"]; ok {
		t.Fatalf("取消后不该落库：这是生命周期停止，不是探测结论（与 probeOne 同一纪律）。called=%d", atomic.LoadInt32(&called))
	}
}

func TestProbeNetworks_NoNetworksIsFine(t *testing.T) {
	r, _ := newProbeRunner(t)
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.probeNetworks(context.Background(), time.Second)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("没有代理时 probeNetworks 卡住了")
	}
}

func TestProbeNetworks_ProbesEveryNetwork(t *testing.T) {
	r, st := newProbeRunner(t)
	seedProbeNetwork(t, st, "n1")
	seedProbeNetwork(t, st, "n2")
	seedProbeNetwork(t, st, "n3")

	var mu sync.Mutex
	seen := make(map[string]int)
	r.proxyProbe = func(_ context.Context, proxyURL string) (int64, error) {
		mu.Lock()
		seen[proxyURL]++
		mu.Unlock()
		return 5, nil
	}

	r.probeNetworks(context.Background(), 5*time.Second)

	mu.Lock()
	total := 0
	for _, c := range seen {
		total += c
	}
	mu.Unlock()
	if total != 3 {
		t.Fatalf("三条代理应各测一次，实际调用 %d 次", total)
	}
	got, err := st.NetworkLatencyByID(context.Background(), 3600*1000)
	if err != nil {
		t.Fatalf("latency: %v", err)
	}
	for _, id := range []string{"n1", "n2", "n3"} {
		if _, ok := got[id]; !ok {
			t.Fatalf("%s 没有落库：一轮探测必须覆盖全部代理", id)
		}
	}
}
