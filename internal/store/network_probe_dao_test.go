package store

import (
	"context"
	"testing"
	"time"
)

// 代理测速的 DAO 口径。这些用例钉的是「界面上那一列数字到底怎么来的」，
// 而不是 SQL 的写法 —— 三种状态必须能在接口上区分开：
//   - 测到了（有成功样本 → 有平均/P95/样本数）；
//   - 最近一次失败（样本可能为 0，但必须能看到失败原因）；
//   - 从没测过（连 last_probe_at 都是 0）。

func seedNetwork(t *testing.T, st *Store, id, name, proxyURL string) {
	t.Helper()
	if err := st.CreateNetwork(context.Background(), &Network{ID: id, Name: name, ProxyURL: proxyURL}); err != nil {
		t.Fatalf("create network %s: %v", id, err)
	}
}

func TestNetworkProbe_LatencyByID(t *testing.T) {
	st := testStore(t, t.TempDir()+"/np.db")
	ctx := context.Background()
	seedNetwork(t, st, "n1", "公司代理", "http://127.0.0.1:7897")
	seedNetwork(t, st, "n2", "本机 Clash", "http://127.0.0.1:7890")

	now := time.Now().UnixMilli()
	// n1：三条成功 100/200/300，再加一条失败（9000ms 是「等到超时才放弃」
	// 的时长，不能被算进平均，否则一条连不通的代理会显示成最快的那条）。
	for i, ms := range []int64{100, 200, 300} {
		if err := st.RecordNetworkProbe(ctx, NetworkProbe{
			NetworkID: "n1", Ts: now - int64(3-i)*1000, OK: true, LatencyMs: ms,
		}); err != nil {
			t.Fatalf("record ok %d: %v", i, err)
		}
	}
	if err := st.RecordNetworkProbe(ctx, NetworkProbe{
		NetworkID: "n1", Ts: now, OK: false, LatencyMs: 9000, Error: "代理探测返回 HTTP 502",
	}); err != nil {
		t.Fatalf("record fail: %v", err)
	}
	// n2：窗口内只有一条成功 80ms。
	if err := st.RecordNetworkProbe(ctx, NetworkProbe{
		NetworkID: "n2", Ts: now, OK: true, LatencyMs: 80,
	}); err != nil {
		t.Fatalf("record n2: %v", err)
	}

	got, err := st.NetworkLatencyByID(ctx, 3600*1000)
	if err != nil {
		t.Fatalf("latency: %v", err)
	}

	n1, ok := got["n1"]
	if !ok {
		t.Fatal("n1 应出现在结果里")
	}
	if n1.Samples != 3 {
		t.Fatalf("n1 成功样本应是 3（失败那条不算），实际 %d", n1.Samples)
	}
	if n1.AvgMs != 200 {
		t.Fatalf("n1 平均应是 (100+200+300)/3=200，实际 %d", n1.AvgMs)
	}
	if n1.MaxMs != 300 {
		t.Fatalf("n1 最大应是 300（不含失败那条的 9000），实际 %d", n1.MaxMs)
	}
	if n1.P95Ms != 300 {
		t.Fatalf("n1 P95 应是 300，实际 %d", n1.P95Ms)
	}
	// 最近一次是失败：即使有成功样本，界面上也要能说出「现在这条挂了」。
	if n1.LastOK {
		t.Fatal("n1 最近一次是失败，LastOK 应为 false")
	}
	if n1.LastError != "代理探测返回 HTTP 502" {
		t.Fatalf("n1 最近错误应是原文，实际 %q", n1.LastError)
	}

	n2 := got["n2"]
	if n2.Samples != 1 || n2.AvgMs != 80 || n2.P95Ms != 80 {
		t.Fatalf("n2 应 1 样本 / 80ms，实际 samples=%d avg=%d p95=%d", n2.Samples, n2.AvgMs, n2.P95Ms)
	}
	if !n2.LastOK {
		t.Fatal("n2 最近一次成功，LastOK 应为 true")
	}
}

func TestNetworkProbe_P95(t *testing.T) {
	st := testStore(t, t.TempDir()+"/np95.db")
	ctx := context.Background()
	seedNetwork(t, st, "n1", "proxy", "http://127.0.0.1:7897")

	now := time.Now().UnixMilli()
	// 20 条 10..200ms：P95 应落在第 ceil(0.95*20)=19 名 = 190ms。
	for i := 1; i <= 20; i++ {
		if err := st.RecordNetworkProbe(ctx, NetworkProbe{
			NetworkID: "n1", Ts: now - int64(20-i)*100, OK: true, LatencyMs: int64(i * 10),
		}); err != nil {
			t.Fatalf("record %d: %v", i, err)
		}
	}
	got, err := st.NetworkLatencyByID(ctx, 3600*1000)
	if err != nil {
		t.Fatalf("latency: %v", err)
	}
	if p95 := got["n1"].P95Ms; p95 != 190 {
		t.Fatalf("P95 应是 190（第 19 名），实际 %d —— 取成最大值会让「偶尔慢」看起来像「一直慢」", p95)
	}
}

func TestNetworkProbe_OnlyFailureIsVisible(t *testing.T) {
	st := testStore(t, t.TempDir()+"/npfail.db")
	ctx := context.Background()
	seedNetwork(t, st, "n1", "proxy", "http://127.0.0.1:7897")

	if err := st.RecordNetworkProbe(ctx, NetworkProbe{
		NetworkID: "n1", Ts: time.Now().UnixMilli(), OK: false, LatencyMs: 10000, Error: "dial tcp: connection refused",
	}); err != nil {
		t.Fatalf("record: %v", err)
	}
	got, err := st.NetworkLatencyByID(ctx, 3600*1000)
	if err != nil {
		t.Fatalf("latency: %v", err)
	}
	n := got["n1"]
	if n.Samples != 0 {
		t.Fatalf("只有失败样本时 Samples 应为 0（前端显示「—」），实际 %d", n.Samples)
	}
	if n.LastOK {
		t.Fatal("LastOK 应为 false")
	}
	if n.LastError != "dial tcp: connection refused" {
		t.Fatalf("失败原因必须能读到，实际 %q —— 否则界面上「代理挂了」与「没测过」长得一样", n.LastError)
	}
	if n.LastTs == 0 {
		t.Fatal("LastTs 必须写，否则前端分不出「从没测过」与「刚失败」")
	}
}

func TestNetworkProbe_NeverProbedIsAbsent(t *testing.T) {
	st := testStore(t, t.TempDir()+"/npnone.db")
	ctx := context.Background()
	seedNetwork(t, st, "n1", "proxy", "http://127.0.0.1:7897")

	got, err := st.NetworkLatencyByID(ctx, 3600*1000)
	if err != nil {
		t.Fatalf("latency: %v", err)
	}
	if _, ok := got["n1"]; ok {
		t.Fatal("从没测过的代理不应出现在结果里（前端据此显示「—」而不是 0ms）")
	}
}

func TestNetworkProbe_WindowExcludesOldSamples(t *testing.T) {
	st := testStore(t, t.TempDir()+"/npwin.db")
	ctx := context.Background()
	seedNetwork(t, st, "n1", "proxy", "http://127.0.0.1:7897")

	now := time.Now().UnixMilli()
	// 窗口内 1 条 60ms，窗口外 1 条 7000ms（2 小时前，窗口 1 小时）。
	if err := st.RecordNetworkProbe(ctx, NetworkProbe{NetworkID: "n1", Ts: now, OK: true, LatencyMs: 60}); err != nil {
		t.Fatalf("record in: %v", err)
	}
	if err := st.RecordNetworkProbe(ctx, NetworkProbe{
		NetworkID: "n1", Ts: now - 2*3600*1000, OK: true, LatencyMs: 7000,
	}); err != nil {
		t.Fatalf("record out: %v", err)
	}
	got, err := st.NetworkLatencyByID(ctx, 3600*1000)
	if err != nil {
		t.Fatalf("latency: %v", err)
	}
	n := got["n1"]
	if n.Samples != 1 || n.AvgMs != 60 {
		t.Fatalf("窗口外样本不能计入平均：实际 samples=%d avg=%d", n.Samples, n.AvgMs)
	}
}

func TestPruneOldNetworkProbes(t *testing.T) {
	st := testStore(t, t.TempDir()+"/npprune.db")
	ctx := context.Background()
	seedNetwork(t, st, "n1", "proxy", "http://127.0.0.1:7897")

	now := time.Now().UnixMilli()
	if err := st.RecordNetworkProbe(ctx, NetworkProbe{NetworkID: "n1", Ts: now, OK: true, LatencyMs: 50}); err != nil {
		t.Fatalf("record fresh: %v", err)
	}
	if err := st.RecordNetworkProbe(ctx, NetworkProbe{
		NetworkID: "n1", Ts: now - 40*24*3600*1000, OK: true, LatencyMs: 50,
	}); err != nil {
		t.Fatalf("record old: %v", err)
	}
	n, err := st.PruneOldNetworkProbes(ctx, 30)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if n != 1 {
		t.Fatalf("应删 1 条超期记录，实际 %d", n)
	}
	// keepDays <= 0 时不动任何数据（与 PruneOldProbes 同一约定）。
	if n, err := st.PruneOldNetworkProbes(ctx, 0); err != nil || n != 0 {
		t.Fatalf("keepDays=0 应什么都不删，实际 n=%d err=%v", n, err)
	}
}
