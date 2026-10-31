package store

import (
	"context"
	"testing"
	"time"
)

// TestProbe_TimelineDayBucketsAlignToLocalDay 守住「天桶按本地日界切分」。
//
// 2026-10-31 修（后端接口审计 #4）：分桶原先是 `(ts / bucketMs) * bucketMs`，
// 即按 UTC epoch 的整倍数切。天桶的边界因此是 UTC 午夜＝本地 08:00，与全仓
// 别的日界口径（dayExpr / usage_daily_rollups.day / 前端 localDayKey）分叉。
//
// 这条用例取本地同一天的 00:30 与 23:30 两条探测：
//   - 本地日界：两者同一个桶，桶起点＝当天本地午夜；
//   - UTC 整倍数：两者分属两个桶，桶起点是本地 08:00。
//
// 两种实现的结果不可能同时通过，所以它钉得住这次修复。
func TestProbe_TimelineDayBucketsAlignToLocalDay(t *testing.T) {
	st := testStore(t, t.TempDir()+"/p.db")
	ctx := context.Background()
	seedProvider(t, st, "p1")
	if err := st.CreateUpstreamModel(ctx, &UpstreamModel{ID: "m1", ProviderID: "p1", ModelID: "x"}); err != nil {
		t.Fatalf("model: %v", err)
	}

	// 用「昨天」而不是「今天」，避免依赖此刻在当天的哪个位置。
	y := time.Now().AddDate(0, 0, -1)
	midnight := time.Date(y.Year(), y.Month(), y.Day(), 0, 0, 0, 0, time.Local)
	early := midnight.Add(30 * time.Minute)
	late := midnight.Add(23*time.Hour + 30*time.Minute)

	if err := st.RecordProbe(ctx, ProbeRecord{ModelID: "m1", ProviderID: "p1", Ts: early.UnixMilli(), OK: true, LatencyMs: 120}); err != nil {
		t.Fatalf("record early: %v", err)
	}
	if err := st.RecordProbe(ctx, ProbeRecord{ModelID: "m1", ProviderID: "p1", Ts: late.UnixMilli(), OK: false, ErrorKind: "timeout"}); err != nil {
		t.Fatalf("record late: %v", err)
	}

	buckets, err := st.ProbeTimeline(ctx, []string{"m1"},
		midnight.Add(-time.Hour).UnixMilli(), late.Add(time.Hour).UnixMilli(), 24*3600*1000)
	if err != nil {
		t.Fatalf("timeline: %v", err)
	}
	if len(buckets) != 1 {
		t.Fatalf("本地同一天应合成 1 个桶，实际 %d 个: %+v", len(buckets), buckets)
	}
	b := buckets[0]
	if b.Ts != midnight.UnixMilli() {
		got := time.UnixMilli(b.Ts).In(time.Local)
		t.Fatalf("桶起点应为当天本地午夜 %s，实际 %s（ts=%d）",
			midnight.Format(time.RFC3339), got.Format(time.RFC3339), b.Ts)
	}
	if b.Count != 2 || b.OK != 1 {
		t.Fatalf("桶计数应为 2 次 / 1 成功，实际 %d / %d", b.Count, b.OK)
	}
	// 平均延迟是 AVG(latency_ms) 覆盖桶内**全部**记录（失败那条没填延迟，
	// 按 0 计入），所以 120 与 0 的平均是 60。这是既有口径，不在本次修复
	// 范围内，但顺手钉住 —— 免得将来有人以为它只统计成功的请求。
	if b.Latency != 60 {
		t.Fatalf("平均延迟应为 60（120 与失败那条的 0 取平均），实际 %d", b.Latency)
	}
}

// TestProbe_TimelineHourBucketsAlignToLocalHour 小时桶按**本地整点**切，
// 返回的 ts 就是那个整点的毫秒值。
//
// 东八区偏移是整小时，本地整点恰好也落在 UTC 整倍数上 —— 所以这条用例在
// 本机时区下对两种实现都成立，它守的是「本地整点」这个契约本身（换到
// 半小时偏移的时区跑，它就会挑出只按 UTC 整倍数切的那种实现）。
func TestProbe_TimelineHourBucketsAlignToLocalHour(t *testing.T) {
	st := testStore(t, t.TempDir()+"/p.db")
	ctx := context.Background()
	seedProvider(t, st, "p1")
	if err := st.CreateUpstreamModel(ctx, &UpstreamModel{ID: "m1", ProviderID: "p1", ModelID: "x"}); err != nil {
		t.Fatalf("model: %v", err)
	}

	y := time.Now().AddDate(0, 0, -1)
	hourStart := time.Date(y.Year(), y.Month(), y.Day(), 9, 0, 0, 0, time.Local)
	if err := st.RecordProbe(ctx, ProbeRecord{ModelID: "m1", ProviderID: "p1",
		Ts: hourStart.Add(30 * time.Minute).UnixMilli(), OK: true, LatencyMs: 80}); err != nil {
		t.Fatalf("record: %v", err)
	}

	buckets, err := st.ProbeTimeline(ctx, []string{"m1"},
		hourStart.Add(-time.Hour).UnixMilli(), hourStart.Add(2*time.Hour).UnixMilli(), 3600*1000)
	if err != nil {
		t.Fatalf("timeline: %v", err)
	}
	if len(buckets) != 1 {
		t.Fatalf("应 1 个桶，实际 %d: %+v", len(buckets), buckets)
	}
	if buckets[0].Ts != hourStart.UnixMilli() {
		t.Fatalf("桶起点应为本地整点 %s，实际 %s",
			hourStart.Format(time.RFC3339), time.UnixMilli(buckets[0].Ts).In(time.Local).Format(time.RFC3339))
	}
}

// TestProbe_TimelineRejectsUnsupportedBucket 只支持按小时 / 按天。
//
// 早先任何 bucketMs 都能跑，是因为分桶纯算术、退化成一个更细的 UTC 桶也
// 不报错。改成按本地日界取桶键后，"其它宽度"没有对应的本地语义，直接
// 报错比悄悄退回 UTC 整倍数（也就是这次要消除的偏差）更诚实。
func TestProbe_TimelineRejectsUnsupportedBucket(t *testing.T) {
	st := testStore(t, t.TempDir()+"/p.db")
	ctx := context.Background()
	if _, err := st.ProbeTimeline(ctx, []string{"m1"}, 0, time.Now().UnixMilli(), 60*1000); err == nil {
		t.Fatal("1 分钟桶应报错，实际通过了")
	}
}

// TestProbe_TimelineCrossesDSTBoundarySafely 桶键解析不因时区跳变炸掉。
//
// 这条不依赖本机时区是否有夏令时：它只断言「跨月/跨年连续的若干天各自
// 一个桶、ts 单调递增且等于各自本地午夜」，任何时区下都该成立。
func TestProbe_TimelineCrossesMonthBoundary(t *testing.T) {
	st := testStore(t, t.TempDir()+"/p.db")
	ctx := context.Background()
	seedProvider(t, st, "p1")
	if err := st.CreateUpstreamModel(ctx, &UpstreamModel{ID: "m1", ProviderID: "p1", ModelID: "x"}); err != nil {
		t.Fatalf("model: %v", err)
	}

	// 2026-01-31 / 02-01 / 02-02 三个本地日各一条（不与「今天」耦合）。
	days := []time.Time{
		time.Date(2026, 1, 31, 12, 0, 0, 0, time.Local),
		time.Date(2026, 2, 1, 12, 0, 0, 0, time.Local),
		time.Date(2026, 2, 2, 12, 0, 0, 0, time.Local),
	}
	for _, d := range days {
		if err := st.RecordProbe(ctx, ProbeRecord{ModelID: "m1", ProviderID: "p1", Ts: d.UnixMilli(), OK: true, LatencyMs: 50}); err != nil {
			t.Fatalf("record %s: %v", d, err)
		}
	}
	buckets, err := st.ProbeTimeline(ctx, []string{"m1"},
		time.Date(2026, 1, 30, 0, 0, 0, 0, time.Local).UnixMilli(),
		time.Date(2026, 2, 3, 0, 0, 0, 0, time.Local).UnixMilli(), 24*3600*1000)
	if err != nil {
		t.Fatalf("timeline: %v", err)
	}
	if len(buckets) != 3 {
		t.Fatalf("应 3 个桶，实际 %d: %+v", len(buckets), buckets)
	}
	for i, b := range buckets {
		want := time.Date(days[i].Year(), days[i].Month(), days[i].Day(), 0, 0, 0, 0, time.Local)
		if b.Ts != want.UnixMilli() {
			t.Fatalf("第 %d 桶起点应为 %s，实际 %s", i,
				want.Format(time.RFC3339), time.UnixMilli(b.Ts).In(time.Local).Format(time.RFC3339))
		}
		if i > 0 && b.Ts <= buckets[i-1].Ts {
			t.Fatalf("桶起点必须严格递增: %+v", buckets)
		}
	}
}
