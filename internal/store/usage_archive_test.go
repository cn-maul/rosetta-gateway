package store

import (
	"context"
	"math"
	"testing"
	"time"
)

// archiveFixture 造一份「可剪枝」的用量数据：跨 3 个维度（用户/密钥/上游模型）
// 与 2 个 provider，并混入 ok / error / canceled 三种状态。
//
// 维度必须**真的不同**：所有记录若维度相同，剪枝丢一维也看不出来 ——
// 归档表少一列的典型症状是「数字对但按模型/按密钥拆不开」，同维度数据测不出来。
// canceled 状态同理：它是 error_count 口径的边界（'ok'/'canceled' 之外才算错），
// 全是 ok 的数据无法证明归档层排除了它。
//
// 记录分两批：batch=0 落在保留窗口之前（会被剪掉），batch=1 落在窗口内（保留）。
// 返回 [老记录数, 保留记录数]。
func archiveFixture(t *testing.T, st *Store, base time.Time, keepDays int) (old, kept int) {
	t.Helper()
	ctx := context.Background()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("fixture: %v", err)
		}
	}
	must(st.CreateProvider(ctx, &Provider{
		ID: "p1", Slug: "p1", Name: "P1", Endpoint: "http://x", Protocol: "chat", Enabled: true}))
	must(st.CreateProvider(ctx, &Provider{
		ID: "p2", Slug: "p2", Name: "P2", Endpoint: "http://y", Protocol: "anthropic", Enabled: true}))
	// 两个模型分别定价，才能让 cost_total 在归档后仍可复算。
	must(st.CreateUpstreamModel(ctx, &UpstreamModel{
		ID: "m1", ProviderID: "p1", ModelID: "sonnet", PriceInput: 0.2, PriceCacheHit: 0.02, PriceOutput: 0.8,
	}))
	must(st.CreateUpstreamModel(ctx, &UpstreamModel{
		ID: "m2", ProviderID: "p2", ModelID: "haiku", PriceInput: 0.5, PriceOutput: 2,
	}))

	// 保留窗口的切分点与 PruneOldUsage 内部一致：本地午夜往前 keepDays 天。
	local := base.In(time.Local)
	cutoff := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, time.Local).
		AddDate(0, 0, -keepDays)

	statuses := []string{"ok", "error", "canceled"}
	// 三组维度：换 user/key/upstream/provider，每组 2 天 × 3 种状态。
	dims := []struct{ user, key, model, prov string }{
		{"u1", "k1", "sonnet", "p1"},
		{"u2", "k2", "haiku", "p2"},
		{"", "k3", "sonnet", "p1"}, // 无归属（user_id 为 NULL）的一批
	}
	seq := 0
	for _, d := range dims {
		for dayOffset := -keepDays - 3; dayOffset <= 1; dayOffset++ {
			for si, status := range statuses {
				ts := cutoff.AddDate(0, 0, dayOffset).Add(9*time.Hour + time.Duration(si)*time.Hour).UnixMilli()
				seq++
				in := int64(1000 + si*100)
				cached := int64(200 * si)
				out := int64(500 + si*50)
				must(st.CreateUsageRecord(ctx, &UsageRecord{
					ID:              seqName(seq),
					Ts:              ts,
					UserID:          d.user,
					AccessKeyID:     d.key,
					PublicModel:     d.model,
					ProviderID:      d.prov,
					UpstreamModel:   d.model,
					IngressProtocol: protocolFor(d.prov),
					Stream:          si%2 == 0,
					InputTokens:     in, OutputTokens: out, TotalTokens: in + out,
					CachedTokens: cached, ReasoningTokens: int64(si) * 10,
					UsageState: "reported", Status: status, HTTPStatus: 200,
					LatencyMs: 100 + int64(si)*50, TTFBMs: 10 + int64(si)*5,
				}))
				if dayOffset < 0 {
					old++
				} else {
					kept++
				}
			}
		}
	}
	return old, kept
}

func seqName(n int) string {
	return "u" + itoa(n)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func protocolFor(providerID string) string {
	if providerID == "p1" {
		return "chat"
	}
	return "anthropic"
}

// freezeNow 把剪枝的「今天」固定住，让切分日与 fixture 用的基准同一天。
// 不冻结的话测试会在 23:59 前后跨午夜，用例变成间歇性失败。
func freezeNow(t *testing.T, at time.Time) {
	t.Helper()
	prev := nowLocal
	nowLocal = func() time.Time { return at }
	t.Cleanup(func() { nowLocal = prev })
}

// TestPrune_StatsUnchangedAcrossPrune 是 P1.5 的核心验收：
// 同一时间窗口的统计在剪枝前后**逐字段相等**。
//
// 这条断言守的是「明细保留 30 天、累计永久」这条硬需求（设计 §4.8）。剪枝把
// usage_records 的老行聚合进表 A 并删除，若任何一个聚合入口漏了归档那张表，
// 这里就会失败 —— 而且是「数字变小」的方向，即静默的数据丢失。
//
// # 必须用非零 from 的窗口，不能只测 from=0
//
// from=0（全部历史）时 UsageSource 根本不下发下界条件，归档支与明细支
// 都没被时间谓词区分，于是「归档支接没接上」这条线在这一档**测不出来** ——
// 改坏归档支的过滤逻辑，这一档仍然全绿（已用变异测试确认）。
// 真正能把归档支压出来的是「窗口下界落在归档区内」的查询（总览的
// 「近 1 年」在有归档数据后走的就是这条），所以下面主断言用它。
func TestPrune_StatsUnchangedAcrossPrune(t *testing.T) {
	ctx := context.Background()
	st := testStore(t, t.TempDir()+"/gw.db")
	base := time.Now()
	freezeNow(t, base)

	oldN, keptN := archiveFixture(t, st, base, DefaultRetentionDays)
	if oldN == 0 || keptN == 0 {
		t.Fatalf("fixture must span both sides of the cutoff, got old=%d kept=%d", oldN, keptN)
	}

	// 窗口下界落在归档区，且**对齐本地午夜**。
	//
	// 对齐午夜不是随手写的：归档支的边界规则是「整日包含」（day_start >= from
	// 且 day_end <= to）。若 from 落在某天的 00:05，那一天的午夜早于 from，
	// 整天会被正确地排除 —— 于是「剪枝前计入、剪枝后（改由归档提供时）排除」，
	// 断言就会看到数字变小。UI 的时间范围下界都是 24h 整倍数、实际对齐午夜，
	// 所以对齐午夜才是真实查询的形状。
	local := base.In(time.Local)
	midnight := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, time.Local)
	winFrom := midnight.AddDate(0, 0, -(DefaultRetentionDays + 5)).UnixMilli()
	winTo := midnight.AddDate(0, 0, 2).UnixMilli()

	before, err := st.GetUsageStats(ctx, winFrom, winTo, "")
	if err != nil {
		t.Fatalf("stats before prune: %v", err)
	}
	beforeScoped, err := st.GetUsageStats(ctx, winFrom, winTo, "u1")
	if err != nil {
		t.Fatalf("scoped stats before prune: %v", err)
	}
	beforeSum, err := st.SumUserUsedTokens(ctx, "u1")
	if err != nil {
		t.Fatalf("user sum before prune: %v", err)
	}
	// 窗口必须真的覆盖到记录，否则本例会退化成「全在明细里」的平凡通过。
	if before.TotalRequests == 0 {
		t.Fatal("window must cover records; got 0 requests")
	}

	res, err := st.PruneOldUsage(ctx, DefaultRetentionDays)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if res.Skipped {
		t.Fatalf("prune skipped: %s", res.Reason)
	}
	if res.DeletedRows != int64(oldN) {
		t.Fatalf("deleted %d rows, want %d (fixture old records)", res.DeletedRows, oldN)
	}

	// 明细里必须真的少了老行 —— 否则「数字不变」可能只是因为根本没剪。
	var remaining int64
	if err := st.read.QueryRow(`SELECT COUNT(*) FROM usage_records`).Scan(&remaining); err != nil {
		t.Fatalf("count details: %v", err)
	}
	if remaining != int64(keptN) {
		t.Fatalf("details after prune = %d, want %d", remaining, keptN)
	}

	after, err := st.GetUsageStats(ctx, winFrom, winTo, "")
	if err != nil {
		t.Fatalf("stats after prune: %v", err)
	}
	assertStatsEqual(t, before, after)

	afterScoped, err := st.GetUsageStats(ctx, winFrom, winTo, "u1")
	if err != nil {
		t.Fatalf("scoped stats after prune: %v", err)
	}
	assertStatsEqual(t, beforeScoped, afterScoped)

	// 用户配额预检走的是同一个 SUM。剪枝后它变小 = 用户配额被清零，
	// 这条断言是「剪枝不会悄悄放宽配额」的唯一守门。
	afterSum, err := st.SumUserUsedTokens(ctx, "u1")
	if err != nil {
		t.Fatalf("user sum after prune: %v", err)
	}
	if afterSum != beforeSum {
		t.Fatalf("user used tokens changed across prune: %d -> %d", beforeSum, afterSum)
	}

	// 归档必须真的进了统计路径：剪枝后表 A 非空，而该窗口里老明细已被删，
	// 所以上面那几个「相等」只可能由表 A 提供。两条合起来才是「接上了」的证据。
	if rows, _ := rollupTotals(t, st); rows == 0 {
		t.Error("archive table is empty after pruning; nothing was archived")
	}
}

// TestPrune_KeyQuotaUnchangedAcrossPrune 是 key 级的对称守门
// （2026-10-12 新增；用户侧由 TestPrune_StatsUnchangedAcrossPrune 的
// SumUserUsedTokens 断言守着，key 级此前没有）。
//
// access_keys.used_tokens 是**终身累计**，与 users 那条同口径：
// DESIGN.md §4「quota_tokens ... 终身累计 input+output 上限」、
// ReserveQuota 的注释「配额限的是终身累计，没有『窗口结束』这回事」，
// 且 RecomputeUsedTokens 的数据源是 UsageSource（明细 + 归档）。
//
// 所以剪枝（把老明细聚合进表 A 后删除）**不得**改动这一列。
// 曾有一段「配对回收」UPDATE 想按被剪量扣回它，因放在 DELETE 之后而恒为空操作
// （2026-10-12 实证后删除，见 usage_archive.go 里的说明）；若有人把它
// 「修好」——即真的减掉被剪量——本测试会当场变红：key 的终身配额会被剪枝
// 悄悄放宽，用户 30 天前的用量一过期就凭空多出一份额度。
func TestPrune_KeyQuotaUnchangedAcrossPrune(t *testing.T) {
	st := newArchiveStore(t)
	ctx := context.Background()
	base := time.Now()
	freezeNow(t, base)

	if err := st.CreateUser(ctx, &User{ID: "u1", Username: "u1", Role: "user", Status: "active"}); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if err := st.CreateAccessKey(ctx, &AccessKey{
		ID: "k1", KeyHash: "hash-k1", KeyPrefix: "sk-gw-1", Name: "k1",
		UserID: "u1", Enabled: true}); err != nil {
		t.Fatalf("CreateAccessKey: %v", err)
	}
	old, kept := archiveFixture(t, st, base, DefaultRetentionDays)
	if old == 0 || kept == 0 {
		t.Fatalf("fixture must span both sides of the cutoff, got old=%d kept=%d", old, kept)
	}

	beforeQuota, beforeUsed, ok, err := st.GetKeyQuota(ctx, "k1")
	if err != nil || !ok {
		t.Fatalf("GetKeyQuota before prune: ok=%v err=%v", ok, err)
	}

	if _, err := st.PruneOldUsage(ctx, DefaultRetentionDays); err != nil {
		t.Fatalf("prune: %v", err)
	}
	// 明细确实少了老行，否则「不变」可能只是因为根本没剪。
	var remaining int64
	if err := st.read.QueryRow(`SELECT COUNT(*) FROM usage_records`).Scan(&remaining); err != nil {
		t.Fatalf("count details: %v", err)
	}
	if remaining != int64(kept) {
		t.Fatalf("details after prune = %d, want %d", remaining, kept)
	}

	afterQuota, afterUsed, ok, err := st.GetKeyQuota(ctx, "k1")
	if err != nil || !ok {
		t.Fatalf("GetKeyQuota after prune: ok=%v err=%v", ok, err)
	}
	if afterUsed != beforeUsed {
		t.Fatalf("★ key 终身配额被剪枝放宽：used_tokens %d -> %d（差 %d）\n"+
			"  这部分量已归档进表 A，仍属于该 key 的终身累计，不得回收",
			beforeUsed, afterUsed, beforeUsed-afterUsed)
	}
	if afterQuota != beforeQuota {
		t.Fatalf("quota_tokens 不该被剪枝改动：%d -> %d", beforeQuota, afterQuota)
	}
	// 与 RecomputeUsedTokens 对账：它走 UsageSource（明细+归档），是权威值。
	// 两者必须一致 —— 不一致说明 used_tokens 与「明细+归档」分叉了。
	recomputed, err := st.RecomputeUsedTokens(ctx, "k1")
	if err != nil {
		t.Fatalf("RecomputeUsedTokens: %v", err)
	}
	if recomputed != afterUsed {
		t.Fatalf("★ used_tokens(%d) 与 RecomputeUsedTokens 的权威值(%d) 分叉", afterUsed, recomputed)
	}
}

func assertStatsEqual(t *testing.T, before, after *UsageStats) {
	t.Helper()
	if before.TotalRequests != after.TotalRequests {
		t.Errorf("total_requests: %d -> %d", before.TotalRequests, after.TotalRequests)
	}
	if before.TotalTokens != after.TotalTokens {
		t.Errorf("total_tokens: %d -> %d", before.TotalTokens, after.TotalTokens)
	}
	if before.InputTokens != after.InputTokens {
		t.Errorf("input_tokens: %d -> %d", before.InputTokens, after.InputTokens)
	}
	if before.OutputTokens != after.OutputTokens {
		t.Errorf("output_tokens: %d -> %d", before.OutputTokens, after.OutputTokens)
	}
	if before.CachedTokens != after.CachedTokens {
		t.Errorf("cached_tokens: %d -> %d", before.CachedTokens, after.CachedTokens)
	}
	if before.ErrorCount != after.ErrorCount {
		t.Errorf("error_count: %d -> %d", before.ErrorCount, after.ErrorCount)
	}
	if before.SuccessCount != after.SuccessCount {
		t.Errorf("success_count: %d -> %d", before.SuccessCount, after.SuccessCount)
	}
	if before.CanceledCount != after.CanceledCount {
		t.Errorf("canceled_count: %d -> %d", before.CanceledCount, after.CanceledCount)
	}
	if before.CostPendingCount != after.CostPendingCount {
		t.Errorf("cost_pending_count: %d -> %d", before.CostPendingCount, after.CostPendingCount)
	}
	if before.CacheHitRate != after.CacheHitRate {
		t.Errorf("cache_hit_rate: %v -> %v", before.CacheHitRate, after.CacheHitRate)
	}
	if before.TtfbSamples != after.TtfbSamples || math.Abs(before.TtfbMs-after.TtfbMs) > 1e-9 {
		t.Errorf("ttfb: %v/%d -> %v/%d", before.TtfbMs, before.TtfbSamples, after.TtfbMs, after.TtfbSamples)
	}
	if before.ThroughputSamples != after.ThroughputSamples || math.Abs(before.TokensPerSec-after.TokensPerSec) > 1e-9 {
		t.Errorf("throughput: %v/%d -> %v/%d",
			before.TokensPerSec, before.ThroughputSamples, after.TokensPerSec, after.ThroughputSamples)
	}
	if diff := before.Cost - after.Cost; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("cost: %v -> %v", before.Cost, after.Cost)
	}
}

// TestPrune_PreservesUpstreamModelDimension 守住表 A 的第 8 个维度。
//
// upstream_model 是 /admin/api/usage/by-model 的分组键。归档表少这一列的后果
// 不是报错，而是**剪枝后按模型拆分失真**：多个上游模型被并进同一行，而用户
// 看到的 by-model 排行从此不再对应真实的模型消耗 —— 没有任何报错。
func TestPrune_PreservesUpstreamModelDimension(t *testing.T) {
	ctx := context.Background()
	st := testStore(t, t.TempDir()+"/gw.db")
	base := time.Now()
	freezeNow(t, base)
	archiveFixture(t, st, base, DefaultRetentionDays)

	if _, err := st.PruneOldUsage(ctx, DefaultRetentionDays); err != nil {
		t.Fatalf("prune: %v", err)
	}

	// 归档后仍应能按 upstream_model 拆出两个模型。
	rows, err := st.read.Query(
		`SELECT upstream_model, SUM(request_count) FROM usage_daily_rollups
		  GROUP BY upstream_model ORDER BY upstream_model`)
	if err != nil {
		t.Fatalf("group rollups by upstream model: %v", err)
	}
	defer rows.Close()
	got := map[string]int64{}
	for rows.Next() {
		var m string
		var n int64
		if err := rows.Scan(&m, &n); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got[m] = n
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	// fixture 用了 sonnet 与 haiku 两个上游模型，各 1/3 的记录。
	for _, m := range []string{"sonnet", "haiku"} {
		if got[m] == 0 {
			t.Errorf("upstream_model %q lost in archive (got %v)", m, got)
		}
	}
	if len(got) != 2 {
		t.Errorf("expected exactly 2 archived upstream models, got %v", got)
	}
}

// TestPrune_BillingEndpointsStillWork 守住剪枝后仍能出数的客户端端点。
//
// SumTokensByDayForKey / SumUsageBuckets / SumCostBuckets 是
// /v1/dashboard/billing/usage 与 /v1/organization/* 的数据源，都是外部客户端
// 的对账入口。它们改用归一化来源后若拼错（例如派生表漏了别名，`u.` 前缀
// 在派生表内部不可解析），症状是**端点直接报错**而不是数字偏差 ——
// 这条用例就是为此写的回归（曾经真出过一次）。
func TestPrune_BillingEndpointsStillWork(t *testing.T) {
	ctx := context.Background()
	st := testStore(t, t.TempDir()+"/gw.db")
	base := time.Now()
	freezeNow(t, base)
	archiveFixture(t, st, base, DefaultRetentionDays)

	local := base.In(time.Local)
	midnight := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, time.Local)
	from := midnight.AddDate(0, 0, -(DefaultRetentionDays + 5)).UnixMilli()
	to := midnight.AddDate(0, 0, 2).UnixMilli()

	beforeDay, err := st.SumTokensByDayForKey(ctx, "k1", from, to)
	if err != nil {
		t.Fatalf("day-for-key before prune: %v", err)
	}
	beforeBuckets, err := st.SumUsageBuckets(ctx, from, to, 86400)
	if err != nil {
		t.Fatalf("usage buckets before prune: %v", err)
	}
	beforeCost, err := st.SumCostBuckets(ctx, from, to, 86400)
	if err != nil {
		t.Fatalf("cost buckets before prune: %v", err)
	}

	if _, err := st.PruneOldUsage(ctx, DefaultRetentionDays); err != nil {
		t.Fatalf("prune: %v", err)
	}

	afterDay, err := st.SumTokensByDayForKey(ctx, "k1", from, to)
	if err != nil {
		t.Fatalf("day-for-key after prune: %v", err)
	}
	if len(afterDay) == 0 {
		t.Fatal("day-for-key returned nothing after pruning")
	}
	// 总量必须不变（按天分桶可能因为 UTC/本地日界两套口径而重分布，
	// 但合计是可比的 —— 见 SumTokensByDayForKey 的日界注释）。
	var beforeSum, afterSum int64
	for _, d := range beforeDay {
		beforeSum += d.Total
	}
	for _, d := range afterDay {
		afterSum += d.Total
	}
	if beforeSum != afterSum {
		t.Errorf("day-for-key total changed across prune: %d -> %d", beforeSum, afterSum)
	}

	afterBuckets, err := st.SumUsageBuckets(ctx, from, to, 86400)
	if err != nil {
		t.Fatalf("usage buckets after prune: %v", err)
	}
	if len(afterBuckets) != len(beforeBuckets) {
		t.Errorf("usage bucket count changed: %d -> %d", len(beforeBuckets), len(afterBuckets))
	}
	var bReq, aReq int64
	for _, b := range beforeBuckets {
		bReq += b.Requests
	}
	for _, b := range afterBuckets {
		aReq += b.Requests
	}
	if bReq != aReq {
		t.Errorf("usage bucket requests changed across prune: %d -> %d", bReq, aReq)
	}

	afterCost, err := st.SumCostBuckets(ctx, from, to, 86400)
	if err != nil {
		t.Fatalf("cost buckets after prune: %v", err)
	}
	var bCost, aCost float64
	for _, b := range beforeCost {
		bCost += b.Cost
	}
	for _, b := range afterCost {
		aCost += b.Cost
	}
	if diff := bCost - aCost; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("cost bucket total changed across prune: %v -> %v", bCost, aCost)
	}
}

// TestPrune_Idempotent 守水位幂等：重复剪枝不产生第二份归档。
//
// 若 ON CONFLICT 的累加或水位判断任一失效，第二次剪枝会把归档数翻倍 ——
// 而翻倍后的数字「看起来完全正常」，正是这类缺陷最难被发现的原因。
func TestPrune_Idempotent(t *testing.T) {
	ctx := context.Background()
	st := testStore(t, t.TempDir()+"/gw.db")
	base := time.Now()
	freezeNow(t, base)
	archiveFixture(t, st, base, DefaultRetentionDays)

	first, err := st.PruneOldUsage(ctx, DefaultRetentionDays)
	if err != nil {
		t.Fatalf("first prune: %v", err)
	}
	if first.Skipped {
		t.Fatalf("first prune should do work, got skipped: %s", first.Reason)
	}
	rollups, tokens := rollupTotals(t, st)

	second, err := st.PruneOldUsage(ctx, DefaultRetentionDays)
	if err != nil {
		t.Fatalf("second prune: %v", err)
	}
	if !second.Skipped {
		t.Errorf("second prune should be skipped by the watermark, got %+v", second)
	}
	rollups2, tokens2 := rollupTotals(t, st)
	if rollups2 != rollups || tokens2 != tokens {
		t.Errorf("archive changed on repeat prune: rows %d->%d tokens %d->%d",
			rollups, rollups2, tokens, tokens2)
	}
}

// TestPrune_LargeBacklogAggregatesOnce 是 P0-3 的回归：积压超过单轮删除上限
// （maxPruneDelete = 20000）时，聚合与删除必须覆盖**严格同一批行**。
//
// 旧实现在同一事务里先「无界聚合全部到期行」、再「只删最老 2 万行」：积压
// 3 万行时，第一轮把 3 万行全部聚合进表 A 却只删掉 2 万，剩下的 1 万行在
// 下一轮被**再聚合一次** —— 而 ON CONFLICT 是累加语义，归档数字凭空变大
// （实测 3 万行 × 1 token：两轮剪枝后 rollup 总数 4 万，多 33%）。偏差不报
// 任何错、不告警，只有对账才能发现，这正是它危险的地方。
//
// 复现的关键是**不许**放大单轮上限来绕过：30000 行全部早于 cutoff，第一轮
// 必须恰好删 20000 行、留下 10000 行逼出第二轮。两轮之后归档总数必须恒等于
// 30000 —— 这条断言同时堵住另一种「修法」（把 ON CONFLICT 改成覆盖）：
// 覆盖会让第二轮把第一轮的量抹掉，同样在这里失败。
func TestPrune_LargeBacklogAggregatesOnce(t *testing.T) {
	ctx := context.Background()
	st := testStore(t, t.TempDir()+"/gw.db")
	base := time.Now()
	freezeNow(t, base)

	// 与 PruneOldUsage 内部同口径的切分点：本地午夜 - keepDays。
	local := base.In(time.Local)
	cutoff := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, time.Local).
		AddDate(0, 0, -DefaultRetentionDays)

	// 30000 行同一维度、同一天、各 1 token：归档后应恰好聚成 1 行，
	// SUM(total_tokens) 每一轮都严格等于「已归档的明细行数」。
	// 只要有任何一批行被聚合两次，这个数就会越过 30000。
	const total = 30000
	ts := cutoff.AddDate(0, 0, -5).Add(9 * time.Hour).UnixMilli()
	for i := 1; i <= total; i++ {
		if err := st.CreateUsageRecord(ctx, &UsageRecord{
			ID:              seqName(i),
			Ts:              ts,
			AccessKeyID:     "k-bulk",
			PublicModel:     "sonnet",
			ProviderID:      "p-bulk",
			UpstreamModel:   "sonnet",
			IngressProtocol: "chat",
			TotalTokens:     1,
			UsageState:      "reported",
			Status:          "ok",
			HTTPStatus:      200,
		}); err != nil {
			t.Fatalf("seed record %d: %v", i, err)
		}
	}

	first, err := st.PruneOldUsage(ctx, DefaultRetentionDays)
	if err != nil {
		t.Fatalf("first prune: %v", err)
	}
	if first.Skipped {
		t.Fatalf("first prune should do work, got skipped: %s", first.Reason)
	}
	// 单轮上限必须仍然生效：只删 20000，留下 10000 行逼出第二轮。
	// （刻意写字面量而不是引用常量：要钉死的就是「上限是 20000、没被放大」。）
	if first.DeletedRows != 20000 {
		t.Fatalf("first prune deleted %d rows, want 20000 (maxPruneDelete cap)", first.DeletedRows)
	}
	// 第一轮只允许归档它删掉的那批：20000。旧实现在这里就已经聚合了全部 30000。
	if _, tokens := rollupTotals(t, st); tokens != 20000 {
		t.Fatalf("rollup total_tokens after first prune = %d, want 20000 (over-aggregation)", tokens)
	}

	second, err := st.PruneOldUsage(ctx, DefaultRetentionDays)
	if err != nil {
		t.Fatalf("second prune: %v", err)
	}
	if second.Skipped {
		t.Fatalf("second prune must not be skipped: watermark must wait until the cutoff is clean, got %s", second.Reason)
	}
	if second.DeletedRows != 10000 {
		t.Fatalf("second prune deleted %d rows, want 10000", second.DeletedRows)
	}
	// 核心断言：两轮之后归档总数恒等于 30000 —— 第二轮只补上自己那批的量，
	// 不把第一轮没删掉的行再聚合一遍。
	if _, tokens := rollupTotals(t, st); tokens != total {
		t.Fatalf("rollup total_tokens after both prunes = %d, want exactly %d (double aggregation)", tokens, total)
	}
	// 水位推进的前提是明细真的清空：cutoff 之前一行不剩。
	var due int64
	if err := st.read.QueryRow(
		`SELECT COUNT(*) FROM usage_records WHERE ts < ?`, cutoff.UnixMilli()).Scan(&due); err != nil {
		t.Fatalf("count due details: %v", err)
	}
	if due != 0 {
		t.Fatalf("%d detail rows remain before cutoff after both prunes", due)
	}
	// 水位已推进：第三轮应直接跳过（批量改造不得破坏幂等）。
	third, err := st.PruneOldUsage(ctx, DefaultRetentionDays)
	if err != nil {
		t.Fatalf("third prune: %v", err)
	}
	if !third.Skipped {
		t.Errorf("third prune should be skipped by the watermark, got %+v", third)
	}
}

func rollupTotals(t *testing.T, st *Store) (rows, tokens int64) {
	t.Helper()
	if err := st.read.QueryRow(
		`SELECT COUNT(*), COALESCE(SUM(total_tokens), 0) FROM usage_daily_rollups`).
		Scan(&rows, &tokens); err != nil {
		t.Fatalf("rollup totals: %v", err)
	}
	return rows, tokens
}

// TestPrune_RejectsInvalidRetention 守参数边界。
//
// keepDays<=0 会让切分点落在今天之前、**当天数据也被删**；超大值等价于清空明细。
// 宁可拒绝也不动数据 —— 所以这里是 skipped=true 而不是报错，理由回给调用方。
func TestPrune_RejectsInvalidRetention(t *testing.T) {
	ctx := context.Background()
	st := testStore(t, t.TempDir()+"/gw.db")
	base := time.Now()
	freezeNow(t, base)
	archiveFixture(t, st, base, DefaultRetentionDays)

	for _, keep := range []int{0, -1, 99999} {
		res, err := st.PruneOldUsage(ctx, keep)
		if err != nil {
			t.Fatalf("keepDays=%d: %v", keep, err)
		}
		if !res.Skipped {
			t.Errorf("keepDays=%d should be rejected, got %+v", keep, res)
		}
	}
	var n int64
	if err := st.read.QueryRow(`SELECT COUNT(*) FROM usage_records`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n == 0 {
		t.Fatal("invalid keep_days must not delete any usage rows")
	}
}

// TestUsageTotals_TriggerAccumulates 守终身累计表的触发器口径。
//
// 三个易错点都在这里：
//   - first_record_at 取 min，且初始 0 必须能被第一次写入置进去
//     （写 MIN(first_record_at, NEW.ts) 的话，初始 0 会让它永远停在 0）；
//   - error_count 排除 canceled；
//   - cost_total 累加的是落库时固化的金额。
func TestUsageTotals_TriggerAccumulates(t *testing.T) {
	ctx := context.Background()
	st := testStore(t, t.TempDir()+"/gw.db")
	base := time.Now()
	freezeNow(t, base)
	archiveFixture(t, st, base, DefaultRetentionDays)

	lt, err := st.GetUsageLifetime(ctx)
	if err != nil {
		t.Fatalf("lifetime: %v", err)
	}
	if lt == nil {
		t.Fatal("lifetime must exist (the (id=1) placeholder row)")
	}
	stats, err := st.GetUsageStats(ctx, 0, 0, "")
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if lt.RequestCount != stats.TotalRequests {
		t.Errorf("lifetime request_count %d != detail %d", lt.RequestCount, stats.TotalRequests)
	}
	if lt.TotalTokens != stats.TotalTokens {
		t.Errorf("lifetime total_tokens %d != detail %d", lt.TotalTokens, stats.TotalTokens)
	}
	if lt.ErrorCount != stats.ErrorCount {
		t.Errorf("lifetime error_count %d != detail %d", lt.ErrorCount, stats.ErrorCount)
	}
	if diff := lt.CostTotal - stats.Cost; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("lifetime cost %v != detail %v", lt.CostTotal, stats.Cost)
	}
	if lt.FirstRecordAt == 0 {
		t.Error("first_record_at must be set (initial 0 must be replaced, not MIN-ed away)")
	}
	// canceled 存在 → error_count 必须严格小于 request_count - success_count。
	if lt.ErrorCount == 0 {
		t.Error("fixture includes error/canceled rows; error_count should be non-zero")
	}
	if lt.SuccessCount >= lt.RequestCount {
		t.Errorf("success %d must be < requests %d (canceled must be excluded from both)",
			lt.SuccessCount, lt.RequestCount)
	}

	// 剪枝**不得**改动表 B：它与明细清理完全解耦，这正是它存在的理由。
	beforeCount, beforeTokens := lt.RequestCount, lt.TotalTokens
	if _, err := st.PruneOldUsage(ctx, DefaultRetentionDays); err != nil {
		t.Fatalf("prune: %v", err)
	}
	lt2, err := st.GetUsageLifetime(ctx)
	if err != nil {
		t.Fatalf("lifetime after prune: %v", err)
	}
	if lt2.RequestCount != beforeCount || lt2.TotalTokens != beforeTokens {
		t.Errorf("lifetime must survive pruning unchanged: %d/%d -> %d/%d",
			beforeCount, beforeTokens, lt2.RequestCount, lt2.TotalTokens)
	}
	if lt2.FirstRecordAt != lt.FirstRecordAt {
		t.Errorf("first_record_at changed across prune: %d -> %d", lt.FirstRecordAt, lt2.FirstRecordAt)
	}
}
