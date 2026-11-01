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

// TestProbe_LatencyByProvider 按供应商聚合延迟：只算成功样本、跨模型合并、
// 失败样本不进平均、窗口外的旧记录不算。
//
// 这四条断言各钉住一种「算错也不会报错」的失效模式：把失败样本算进平均会让
// 连不上的供应商显示成最快；按模型返回会让列上只能显示某一个模型的延迟；
// 不查窗口会拿上周的数当现在的网络状况。
func TestProbe_LatencyByProvider(t *testing.T) {
	st := testStore(t, t.TempDir()+"/lat.db")
	ctx := context.Background()
	seedProvider(t, st, "p1")
	seedProvider(t, st, "p2")
	for _, m := range []struct{ id, prov string }{{"m1", "p1"}, {"m2", "p1"}, {"m3", "p2"}} {
		if err := st.CreateUpstreamModel(ctx, &UpstreamModel{ID: m.id, ProviderID: m.prov, ModelID: m.id}); err != nil {
			t.Fatalf("model %s: %v", m.id, err)
		}
	}

	now := time.Now().UnixMilli()
	rec := func(model, prov string, ok bool, latency int64, ageMs int64) {
		t.Helper()
		if err := st.RecordProbe(ctx, ProbeRecord{
			ModelID: model, ProviderID: prov, Ts: now - ageMs, OK: ok, LatencyMs: latency,
		}); err != nil {
			t.Fatalf("record %s/%s: %v", prov, model, err)
		}
	}
	// p1：两个模型各两条成功（100/200 与 300/300），再加一次失败。
	// 合并后平均 = (100+200+300+300)/4 = 225；失败那条的 latency 若被算进去
	// （它是 9000ms 的超时等待）平均值会跳到 1980 —— 这正是要挡住的。
	rec("m1", "p1", true, 100, 5_000)
	rec("m1", "p1", true, 200, 4_000)
	rec("m2", "p1", true, 300, 3_000)
	rec("m2", "p1", true, 300, 2_000)
	rec("m1", "p1", false, 9_000, 1_000)
	// p2：只有一条成功
	rec("m3", "p2", true, 80, 1_000)
	// p2：窗口外（2 小时前）的一条成功，不该被算进当前窗口
	rec("m3", "p2", true, 7_000, 2*3600*1000+60_000)

	got, err := st.ProbeLatencyByProvider(ctx, 3600*1000)
	if err != nil {
		t.Fatalf("latency: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("应 2 个供应商有数据，实际 %d: %+v", len(got), got)
	}

	p1 := got["p1"]
	if p1.Samples != 4 {
		t.Fatalf("p1 成功样本应是 4（失败那条不算），实际 %d", p1.Samples)
	}
	if p1.AvgMs != 225 {
		t.Fatalf("p1 平均延迟应是 225ms（失败样本的 9000ms 不能算进去），实际 %d", p1.AvgMs)
	}
	if p1.MaxMs != 300 {
		t.Fatalf("p1 最大延迟应是 300ms，实际 %d", p1.MaxMs)
	}

	p2 := got["p2"]
	if p2.Samples != 1 {
		t.Fatalf("p2 窗口内只有 1 条成功，实际 %d（窗口外那条被算进来了）", p2.Samples)
	}
	if p2.AvgMs != 80 {
		t.Fatalf("p2 平均延迟应是 80ms，实际 %d", p2.AvgMs)
	}
	if p2.P95Ms != 80 {
		t.Fatalf("p2 只有 1 个样本时 P95 应等于该样本，实际 %d", p2.P95Ms)
	}
}

// TestProbe_LatencyByProvider_P95 用 20 条样本验证第 95 百分位取的是高位那条。
//
// 20 条 → ceil(0.95*20) = 19，即升序第 19 条（值 190），不是最大值 200。
// 写成「取最大」也能让简单用例通过，所以这条特意造了 20 条。
func TestProbe_LatencyByProvider_P95(t *testing.T) {
	st := testStore(t, t.TempDir()+"/p95.db")
	ctx := context.Background()
	seedProvider(t, st, "p1")
	if err := st.CreateUpstreamModel(ctx, &UpstreamModel{ID: "m1", ProviderID: "p1", ModelID: "x"}); err != nil {
		t.Fatalf("model: %v", err)
	}
	now := time.Now().UnixMilli()
	for i := 1; i <= 20; i++ {
		if err := st.RecordProbe(ctx, ProbeRecord{
			ModelID: "m1", ProviderID: "p1", Ts: now - int64(20-i)*1000, OK: true, LatencyMs: int64(i * 10),
		}); err != nil {
			t.Fatalf("record %d: %v", i, err)
		}
	}

	got, err := st.ProbeLatencyByProvider(ctx, 3600*1000)
	if err != nil {
		t.Fatalf("latency: %v", err)
	}
	if p := got["p1"]; p.P95Ms != 190 {
		t.Fatalf("20 条样本（10..200ms）的 P95 应是 190ms，实际 %d", p.P95Ms)
	}
}

// TestProbe_LatencyByProvider_EmptyIsAbsent 没有任何探测记录时不返回该供应商。
//
// 返回 Latency{ProviderID:"p1"} 会让前端无法区分「没探过」与「0 毫秒」，
// 而列表上那一格正是靠「键不存在」来显示「—」的。
func TestProbe_LatencyByProvider_EmptyIsAbsent(t *testing.T) {
	st := testStore(t, t.TempDir()+"/empty.db")
	ctx := context.Background()
	seedProvider(t, st, "p1")

	got, err := st.ProbeLatencyByProvider(ctx, 3600*1000)
	if err != nil {
		t.Fatalf("latency: %v", err)
	}
	if _, ok := got["p1"]; ok {
		t.Fatalf("没有探测记录时不该出现 p1 的条目，实际 %+v", got)
	}
}
