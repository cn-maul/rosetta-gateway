package store

import (
	"context"
	"testing"
	"time"
)

// TestProbe_RecordAndSummary 落几条探测 → 健康摘要算对（成功率/连续失败/最近错误）。
func TestProbe_RecordAndSummary(t *testing.T) {
	st := testStore(t, t.TempDir()+"/p.db")
	ctx := context.Background()
	seedProvider(t, st, "p1")
	if err := st.CreateUpstreamModel(ctx, &UpstreamModel{ID: "m1", ProviderID: "p1", ModelID: "x"}); err != nil {
		t.Fatalf("model: %v", err)
	}

	now := time.Now().UnixMilli()
	// 3 次失败（连续），然后 2 次成功 → 连续失败应为 0（最近成功重置），
	// 窗口 5 次成功 2 次，成功率 40%。
	for i := 0; i < 3; i++ {
		if err := st.RecordProbe(ctx, ProbeRecord{ModelID: "m1", ProviderID: "p1", Ts: now - int64(6-i)*1000, OK: false, ErrorKind: "timeout"}); err != nil {
			t.Fatalf("record fail %d: %v", i, err)
		}
	}
	for i := 0; i < 2; i++ {
		if err := st.RecordProbe(ctx, ProbeRecord{ModelID: "m1", ProviderID: "p1", Ts: now - int64(2-i)*1000, OK: true, LatencyMs: 500}); err != nil {
			t.Fatalf("record ok %d: %v", i, err)
		}
	}

	healths, err := st.ProbeHealthSummary(ctx, 3600*1000)
	if err != nil {
		t.Fatalf("summary: %v", err)
	}
	if len(healths) != 1 {
		t.Fatalf("应 1 个模型，实际 %d", len(healths))
	}
	h := healths[0]
	if h.WindowCount != 5 || h.WindowOK != 2 {
		t.Fatalf("窗口计数错误: count=%d ok=%d", h.WindowCount, h.WindowOK)
	}
	if h.ConsecutiveFail != 0 {
		t.Fatalf("最近成功后连续失败应为 0，实际 %d", h.ConsecutiveFail)
	}
	if !h.LastOK {
		t.Fatal("最近一次应成功")
	}
	if h.LastLatencyMs != 500 {
		t.Fatalf("最近延迟应为 500，实际 %d", h.LastLatencyMs)
	}
}

// TestProbe_ConsecutiveFail 连续失败计数：最近一次失败时，从最近成功之后数。
func TestProbe_ConsecutiveFail(t *testing.T) {
	st := testStore(t, t.TempDir()+"/p.db")
	ctx := context.Background()
	seedProvider(t, st, "p1")
	if err := st.CreateUpstreamModel(ctx, &UpstreamModel{ID: "m1", ProviderID: "p1", ModelID: "x"}); err != nil {
		t.Fatalf("model: %v", err)
	}

	now := time.Now().UnixMilli()
	// 成功 → 失败 → 失败 → 失败（最近 3 次失败）
	if err := st.RecordProbe(ctx, ProbeRecord{ModelID: "m1", ProviderID: "p1", Ts: now - 4000, OK: true}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := st.RecordProbe(ctx, ProbeRecord{ModelID: "m1", ProviderID: "p1", Ts: now - int64(3-i)*1000, OK: false, ErrorKind: "auth"}); err != nil {
			t.Fatal(err)
		}
	}

	healths, _ := st.ProbeHealthSummary(ctx, 3600*1000)
	h := healths[0]
	if h.ConsecutiveFail != 3 {
		t.Fatalf("连续失败应为 3，实际 %d", h.ConsecutiveFail)
	}
	if h.LastOK {
		t.Fatal("最近一次应失败")
	}
	if h.LastErrorKind != "auth" {
		t.Fatalf("最近错误应为 auth，实际 %q", h.LastErrorKind)
	}
}

// TestProbe_Timeline 分桶：按小时聚合，桶内无记录的行不返回。
func TestProbe_Timeline(t *testing.T) {
	st := testStore(t, t.TempDir()+"/p.db")
	ctx := context.Background()
	seedProvider(t, st, "p1")
	if err := st.CreateUpstreamModel(ctx, &UpstreamModel{ID: "m1", ProviderID: "p1", ModelID: "x"}); err != nil {
		t.Fatalf("model: %v", err)
	}

	now := time.Now().UnixMilli()
	hour := int64(3600 * 1000)
	// 两个不同小时的探测
	if err := st.RecordProbe(ctx, ProbeRecord{ModelID: "m1", ProviderID: "p1", Ts: now, OK: true, LatencyMs: 100}); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordProbe(ctx, ProbeRecord{ModelID: "m1", ProviderID: "p1", Ts: now, OK: true, LatencyMs: 300}); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordProbe(ctx, ProbeRecord{ModelID: "m1", ProviderID: "p1", Ts: now - hour, OK: false, ErrorKind: "timeout"}); err != nil {
		t.Fatal(err)
	}

	buckets, err := st.ProbeTimeline(ctx, []string{"m1"}, now-2*hour, now, hour)
	if err != nil {
		t.Fatalf("timeline: %v", err)
	}
	if len(buckets) != 2 {
		t.Fatalf("应 2 个桶，实际 %d: %+v", len(buckets), buckets)
	}
	// 每个桶都带回 model_id —— 多模型查询时前端靠它把桶分流到各自的卡片。
	for _, b := range buckets {
		if b.ModelID != "m1" {
			t.Fatalf("桶应带 model_id=m1，实际 %q", b.ModelID)
		}
	}
	// 最近的桶：2 次成功，平均延迟 200
	latest := buckets[0]
	if latest.Ts > buckets[1].Ts {
		latest = buckets[1]
	}
	_ = latest
	// 找到 now 所在桶
	var cur, prev *ProbeTimelineBucket
	for i := range buckets {
		if buckets[i].Ts <= now && now < buckets[i].Ts+hour {
			cur = &buckets[i]
		}
		if buckets[i].Ts <= now-hour && now-hour < buckets[i].Ts+hour {
			prev = &buckets[i]
		}
	}
	if cur == nil || cur.Count != 2 || cur.OK != 2 || cur.Latency != 200 {
		t.Fatalf("当前桶错误: %+v", cur)
	}
	if prev == nil || prev.Count != 1 || prev.OK != 0 {
		t.Fatalf("前一桶错误: %+v", prev)
	}
}

// TestProbe_TimelineMultiModel 多模型一次查询：两个模型的桶互不混淆，
// 且各自带回 model_id（前端靠它把桶分流到对应卡片）。
//
// 这条守住「一次请求覆盖多模型」这个设计：退化成循环逐个查不会报错，
// 只是慢 —— 而慢正是这个改动要消除的东西，所以必须用「返回里同时有
// 两个模型的桶」来钉住单次查询确实发生了。
func TestProbe_TimelineMultiModel(t *testing.T) {
	st := testStore(t, t.TempDir()+"/p.db")
	ctx := context.Background()
	seedProvider(t, st, "p1")
	for _, id := range []string{"m1", "m2"} {
		if err := st.CreateUpstreamModel(ctx, &UpstreamModel{ID: id, ProviderID: "p1", ModelID: id}); err != nil {
			t.Fatalf("model %s: %v", id, err)
		}
	}

	now := time.Now().UnixMilli()
	hour := int64(3600 * 1000)
	if err := st.RecordProbe(ctx, ProbeRecord{ModelID: "m1", ProviderID: "p1", Ts: now, OK: true, LatencyMs: 100}); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordProbe(ctx, ProbeRecord{ModelID: "m2", ProviderID: "p1", Ts: now, OK: false, ErrorKind: "timeout"}); err != nil {
		t.Fatal(err)
	}

	buckets, err := st.ProbeTimeline(ctx, []string{"m1", "m2"}, now-hour, now, hour)
	if err != nil {
		t.Fatalf("multi timeline: %v", err)
	}
	if len(buckets) != 2 {
		t.Fatalf("应返回 2 个模型各 1 桶，实际 %d: %+v", len(buckets), buckets)
	}
	byModel := map[string]ProbeTimelineBucket{}
	for _, b := range buckets {
		byModel[b.ModelID] = b
	}
	if m1, ok := byModel["m1"]; !ok || m1.OK != 1 {
		t.Fatalf("m1 应有 1 次成功: %+v", byModel)
	}
	if m2, ok := byModel["m2"]; !ok || m2.OK != 0 || m2.Count != 1 {
		t.Fatalf("m2 应有 1 次失败: %+v", byModel)
	}
}

// TestProbe_TimelineEmptyIDs 空 id 列表直接返回空，不打库。
func TestProbe_TimelineEmptyIDs(t *testing.T) {
	st := testStore(t, t.TempDir()+"/p.db")
	buckets, err := st.ProbeTimeline(context.Background(), nil, 0, 1, 3600000)
	if err != nil || len(buckets) != 0 {
		t.Fatalf("空 ids 应返回空切片: buckets=%v err=%v", buckets, err)
	}
}
func TestProbe_Prune(t *testing.T) {
	st := testStore(t, t.TempDir()+"/p.db")
	ctx := context.Background()
	seedProvider(t, st, "p1")
	if err := st.CreateUpstreamModel(ctx, &UpstreamModel{ID: "m1", ProviderID: "p1", ModelID: "x"}); err != nil {
		t.Fatalf("model: %v", err)
	}

	now := time.Now().UnixMilli()
	// 一条 40 天前（超期），一条现在
	if err := st.RecordProbe(ctx, ProbeRecord{ModelID: "m1", ProviderID: "p1", Ts: now - 40*24*3600*1000, OK: true}); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordProbe(ctx, ProbeRecord{ModelID: "m1", ProviderID: "p1", Ts: now, OK: true}); err != nil {
		t.Fatal(err)
	}

	n, err := st.PruneOldProbes(ctx, 30)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if n != 1 {
		t.Fatalf("应删 1 条，实际 %d", n)
	}
	// 剩下的那条还在
	healths, _ := st.ProbeHealthSummary(ctx, 3600*1000)
	if healths[0].WindowCount != 1 {
		t.Fatalf("剪枝后应剩 1 条，实际 %d", healths[0].WindowCount)
	}
}
