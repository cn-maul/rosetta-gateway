package store

// 并发压测与账目完整性验证 —— 配额预占（场景 D）与归档剪枝（场景 E）。
//
// # 为什么配额与归档要和余额放在一起验证
//
// 三者共享同一批表（users / access_keys / usage_records），且都依赖
// **触发器**与**写入顺序**。单独看每一处都自洽，但真实的账目是三者叠加的
// 结果：一次请求会同时（1）预占配额、（2）落 usage、（3）触发 used_tokens
// 累加、（4）将来被剪枝归档。本文件验证这些环节叠加后**总数不变**。
//
// # 复用既有 fixture 而不是另造数据
//
// archiveFixture / freezeNow / seqName / protocolFor 来自 usage_archive_test.go。
// 复用它们有两个作用：一是维度与状态分布已经被设计成「漏一维就能看出来」；
// 二是如果 fixture 本身变了，本文件的断言会跟着一起动，不会两边悄悄分叉。

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// D. 配额预占
// ---------------------------------------------------------------------------

// reserveOutcome 汇总一次并发预占的结果。
type reserveOutcome struct {
	granted  int64 // ReserveQuota 返回 ok=true
	rejected int64 // ok=false（额度不够）
	otherErr int64
	errs     []error
	mu       sync.Mutex
	// grantedTokens 是所有成功预占的 est 之和 —— 必须 <= quota。
	grantedTokens int64
}

func (o *reserveOutcome) record(reserved int64, ok bool, err error) {
	switch {
	case err != nil:
		atomic.AddInt64(&o.otherErr, 1)
		o.mu.Lock()
		o.errs = append(o.errs, err)
		o.mu.Unlock()
	case ok:
		atomic.AddInt64(&o.granted, 1)
		atomic.AddInt64(&o.grantedTokens, reserved)
	default:
		atomic.AddInt64(&o.rejected, 1)
	}
}

// mustKeyWithQuota 造一个带终身配额上限的 key（quota>0 才会真正预占）。
func mustKeyWithQuota(t *testing.T, st *Store, id string, quota int64) {
	t.Helper()
	if err := st.CreateAccessKey(context.Background(), &AccessKey{
		ID: id, KeyHash: "hash-" + id, KeyPrefix: "sk-gw-", Name: id,
		Enabled: true, QuotaTokens: quota,
	}); err != nil {
		t.Fatalf("create key %s: %v", id, err)
	}
}

// readReserved 读某 key 当前的在途预占。
func readReserved(t *testing.T, st *Store, keyID string) int64 {
	t.Helper()
	var v int64
	if err := st.read.QueryRowContext(context.Background(),
		`SELECT reserved_tokens FROM access_keys WHERE id = ?`, keyID).Scan(&v); err != nil {
		t.Fatalf("read reserved(%s): %v", keyID, err)
	}
	return v
}

// readUsed 读某 key 的 used_tokens（由 usage 触发器维护）。
func readUsed(t *testing.T, st *Store, keyID string) int64 {
	t.Helper()
	var v int64
	if err := st.read.QueryRowContext(context.Background(),
		`SELECT used_tokens FROM access_keys WHERE id = ?`, keyID).Scan(&v); err != nil {
		t.Fatalf("read used(%s): %v", keyID, err)
	}
	return v
}

// D1：并发 ReserveQuota **不得超过总额度**。
//
// 这是预占机制存在的全部理由。修复前的形态是「纯查询（GetKeyQuota）：
// 读 used → 比较 → 放行」，并发下 N 个请求都读到同一个 used 并全部通过，
// 可超发数十倍。
//
// 构造：quota = 1000，200 个 goroutine 各预占 100。
// 期望：放行**恰好 10 次**（10×100 = 1000），其余全部 rejected；
// 且所有放行的 est 之和**恰好** 1000，reserved_tokens 恰好 1000。
func TestStress_ReserveQuota_NeverExceedsTotal(t *testing.T) {
	ctx := context.Background()
	st := testStore(t, t.TempDir()+"/gw.db")

	const (
		quota     = 1000
		est       = 100
		workers   = 200
		wantGrant = quota / est // 10
	)
	mustKeyWithQuota(t, st, "k1", quota)

	var out reserveOutcome
	runConcurrent(workers, func(i int) {
		reserved, ok, err := st.ReserveQuota(ctx, "k1", est)
		out.record(reserved, ok, err)
	})

	if out.otherErr != 0 {
		t.Fatalf("出现 %d 次非预期错误: %v", out.otherErr, out.errs)
	}
	if out.granted != wantGrant {
		t.Errorf("放行 %d 次, want **恰好** %d 次（quota %d / est %d）",
			out.granted, wantGrant, quota, est)
	}
	if out.granted+out.rejected != workers {
		t.Errorf("放行+拒绝 = %d, want %d", out.granted+out.rejected, workers)
	}
	// 核心不变量：放行的预占总量不得超过额度。这是「不超发」的定义。
	if out.grantedTokens > quota {
		t.Errorf("放行预占总量 = %d, want <= %d —— **超发**（预占机制失效）",
			out.grantedTokens, quota)
	}
	if out.grantedTokens != wantGrant*est {
		t.Errorf("放行预占总量 = %d, want %d", out.grantedTokens, wantGrant*est)
	}
	if reserved := readReserved(t, st, "k1"); reserved != wantGrant*est {
		t.Errorf("reserved_tokens = %d, want %d", reserved, wantGrant*est)
	}
	// used_tokens 必须仍是 0：预占**绝不能**写 used（那会让一次请求被记两次，
	// 正是 2026-10-07 的 P0-2）。
	if used := readUsed(t, st, "k1"); used != 0 {
		t.Errorf("used_tokens = %d, want 0（预占不得写 used_tokens —— P0-2 回归）", used)
	}
}

// D2：释放后 reserved_tokens 归零；**泄漏检测** —— N 次成功/失败/异常收尾后
// 必须回到 0。
//
// 预占泄漏是本模块最隐蔽的故障：reserved_tokens 单调偏高 → 后续请求
// `used+reserved+est > quota` 恒成立 → 这把 key **永久 429**，
// 而界面上看不出任何异常（used 与 quota 都是对的）。
//
// 构造：混合三种收尾形态各自跑一遍，每次都必须回到 0：
//  1. reserve → release（正常收尾，真实用量由触发器另外累加）；
//  2. reserve → 额度不足（0 预占，无需释放）；
//  3. 高并发 reserve/release 配对（最容易漏配对）。
func TestStress_ReserveQuota_NoLeakAfterMixedOutcomes(t *testing.T) {
	ctx := context.Background()
	st := testStore(t, t.TempDir()+"/gw.db")
	mustKeyWithQuota(t, st, "k1", 100_000)

	// 形态 1：reserve → release 配对。
	for i := 0; i < 50; i++ {
		reserved, ok, err := st.ReserveQuota(ctx, "k1", 100)
		if err != nil || !ok {
			t.Fatalf("第 %d 次预占失败: ok=%v err=%v", i, ok, err)
		}
		if err := st.ReleaseQuota(ctx, "k1", reserved); err != nil {
			t.Fatalf("第 %d 次释放失败: %v", i, err)
		}
	}
	if r := readReserved(t, st, "k1"); r != 0 {
		t.Fatalf("形态1 后 reserved = %d, want 0（预占泄漏）", r)
	}

	// 形态 2：reserve 被拒（est 超过额度）—— 必须不留下任何预占。
	if _, ok, err := st.ReserveQuota(ctx, "k1", 999_999); err != nil || ok {
		t.Fatalf("超额预占应被拒: ok=%v err=%v", ok, err)
	}
	if r := readReserved(t, st, "k1"); r != 0 {
		t.Fatalf("形态2 后 reserved = %d, want 0（被拒的预占留下了痕迹）", r)
	}

	// 形态 3：高并发 reserve/release 配对 —— 最可能漏配对的形态。
	const workers = 100
	var (
		grantedTotal int64
		leaks        []string
		mu           sync.Mutex
	)
	runConcurrent(workers, func(i int) {
		reserved, ok, err := st.ReserveQuota(ctx, "k1", 10)
		if err != nil {
			mu.Lock()
			leaks = append(leaks, fmt.Sprintf("worker %d reserve err: %v", i, err))
			mu.Unlock()
			return
		}
		if !ok {
			return // 额度不够，没有预占可释放
		}
		atomic.AddInt64(&grantedTotal, reserved)
		// 模拟收尾：真实用量由触发器累加，这里只退预占。
		if err := st.ReleaseQuota(ctx, "k1", reserved); err != nil {
			mu.Lock()
			leaks = append(leaks, fmt.Sprintf("worker %d release err: %v", i, err))
			mu.Unlock()
		}
	})
	if len(leaks) > 0 {
		t.Fatalf("高并发配对出现错误: %v", leaks)
	}

	if r := readReserved(t, st, "k1"); r != 0 {
		t.Errorf("高并发配对后 reserved = %d, want 0 —— **预占泄漏**，"+
			"这把 key 会永久 429（每次预检都看到假性额度不足）", r)
	}
	// 配额只在「预占时」判定，与 used 无关；这里 used 仍是 0（没落 usage）。
	if used := readUsed(t, st, "k1"); used != 0 {
		t.Errorf("used_tokens = %d, want 0（本用例没落 usage 记录）", used)
	}
}

// D3：预占 + 真实用量落库 + 释放之后，**净记账恰好等于真实用量**（不是 2 倍）。
//
// 这是 P0-2 的并发形态：修复前预占写入 used_tokens、收尾再按差值校正，
// 一次请求净记账 2×actual。本用例把它压到并发：N 个请求各自
// reserve → 落 usage → release，最后 used 必须**恰好**等于真实用量之和。
func TestStress_ReserveQuota_NetAccountingEqualsActualUsage(t *testing.T) {
	ctx := context.Background()
	st := testStore(t, t.TempDir()+"/gw.db")

	const (
		quota   = 1_000_000
		workers = 100
		estEach = 500
		actual  = 300 // 真实用量小于预估
	)
	mustKeyWithQuota(t, st, "k1", quota)

	var otherErr int64
	var mu sync.Mutex
	var errs []error
	runConcurrent(workers, func(i int) {
		reserved, ok, err := st.ReserveQuota(ctx, "k1", estEach)
		if err != nil || !ok {
			mu.Lock()
			errs = append(errs, fmt.Errorf("worker %d reserve: ok=%v err=%v", i, ok, err))
			mu.Unlock()
			atomic.AddInt64(&otherErr, 1)
			return
		}
		// 落一条真实用量（total_tokens=actual）→ 触发器累加 used_tokens。
		if err := st.CreateUsageRecord(ctx, &UsageRecord{
			ID: fmt.Sprintf("u-%d", i), AccessKeyID: "k1",
			PublicModel: "m", ProviderID: "p", UpstreamModel: "u",
			IngressProtocol: "openai-chat", TotalTokens: actual,
			UsageState: "reported", Status: "ok", HTTPStatus: 200,
		}); err != nil {
			mu.Lock()
			errs = append(errs, fmt.Errorf("worker %d usage: %w", i, err))
			mu.Unlock()
			atomic.AddInt64(&otherErr, 1)
			return
		}
		if err := st.ReleaseQuota(ctx, "k1", reserved); err != nil {
			mu.Lock()
			errs = append(errs, fmt.Errorf("worker %d release: %w", i, err))
			mu.Unlock()
			atomic.AddInt64(&otherErr, 1)
		}
	})

	if otherErr != 0 {
		t.Fatalf("出现 %d 次错误: %v", otherErr, errs)
	}

	wantUsed := int64(workers * actual)
	if used := readUsed(t, st, "k1"); used != wantUsed {
		t.Errorf("used_tokens = %d, want %d —— 差 %d；**2 倍记账回归**"+
			"（预占混写进 used_tokens 时这里会是 %d）",
			used, wantUsed, used-wantUsed, wantUsed*2)
	}
	if r := readReserved(t, st, "k1"); r != 0 {
		t.Errorf("reserved_tokens = %d, want 0（预占泄漏）", r)
	}
}

// ---------------------------------------------------------------------------
// E. 归档剪枝的账目一致性
// ---------------------------------------------------------------------------

// E1：跨多天用量 → 剪枝 → **总额不变**，且 GetUsageStats 剪枝前后逐字段相等。
//
// 这条守的是「明细 30 天、累计永久」这条硬需求。剪枝把老行聚合进表 A 并删除，
// 任何聚合入口漏接归档表都会表现为**数字变小**（静默数据丢失）。
//
// 断言用「全部历史」窗口（from=0）：剪枝的意义就是让这个窗口的数字不变。
func TestStress_Prune_StatsIdenticalBeforeAndAfter(t *testing.T) {
	ctx := context.Background()
	st := testStore(t, t.TempDir()+"/gw.db")

	base := time.Now()
	freezeNow(t, base)
	old, kept := archiveFixture(t, st, base, DefaultRetentionDays)

	// 剪枝前：全窗口统计。
	before, err := st.GetUsageStats(ctx, 0, 0, "")
	if err != nil {
		t.Fatalf("剪枝前统计: %v", err)
	}
	if before.TotalRequests == 0 {
		t.Fatal("fixture 没造出数据，测试无意义")
	}

	res, err := st.PruneOldUsage(ctx, DefaultRetentionDays)
	if err != nil {
		t.Fatalf("剪枝失败: %v", err)
	}
	if res.Skipped {
		t.Fatalf("剪枝被跳过: %s", res.Reason)
	}
	if res.DeletedRows != int64(old) {
		t.Errorf("删除行数 = %d, want %d（fixture 声称的窗口外行数）", res.DeletedRows, old)
	}
	_ = kept

	after, err := st.GetUsageStats(ctx, 0, 0, "")
	if err != nil {
		t.Fatalf("剪枝后统计: %v", err)
	}

	// 逐字段精确相等 —— 不是「差不多」，任何一个字段不等都是数据丢失。
	assertStatsEqual(t, before, after)
}

// E2：反复剪枝**幂等** —— 第二次及以后不得改变任何数字。
//
// 非幂等的形态有两种，都很隐蔽：
//   - 归档写入用累加（ON CONFLICT DO UPDATE ... + excluded），若同一批行
//     被聚合两次，归档数字凭空变大且不报错（实测过 3 万行 × 1 token：
//     两轮后 rollup 总数多 33%）；
//   - 水位判断写错，导致已剪区间被反复重扫。
//
// 断言：连续剪 3 次，第 2、3 次的统计与前一次**完全相等**，且删除行数为 0。
func TestStress_Prune_RepeatedPruneIsIdempotent(t *testing.T) {
	ctx := context.Background()
	st := testStore(t, t.TempDir()+"/gw.db")

	base := time.Now()
	freezeNow(t, base)
	archiveFixture(t, st, base, DefaultRetentionDays)

	// 第一次：真的剪。
	first, err := st.PruneOldUsage(ctx, DefaultRetentionDays)
	if err != nil {
		t.Fatalf("第 1 次剪枝: %v", err)
	}
	if first.Skipped {
		t.Fatalf("第 1 次剪枝被跳过: %s", first.Reason)
	}
	snap1, err := st.GetUsageStats(ctx, 0, 0, "")
	if err != nil {
		t.Fatalf("统计 1: %v", err)
	}

	// 第 2、3 次：必须无事可做，且数字一字不变。
	for i := 2; i <= 3; i++ {
		res, err := st.PruneOldUsage(ctx, DefaultRetentionDays)
		if err != nil {
			t.Fatalf("第 %d 次剪枝: %v", i, err)
		}
		// 水位已到位 → Skipped；即使没跳过，也不得删掉任何行。
		if res.DeletedRows != 0 {
			t.Errorf("第 %d 次剪枝删除了 %d 行, want 0（幂等性被破坏）", i, res.DeletedRows)
		}
		if res.RollupRows != 0 {
			t.Errorf("第 %d 次剪枝写了 %d 行归档, want 0", i, res.RollupRows)
		}
		snap, err := st.GetUsageStats(ctx, 0, 0, "")
		if err != nil {
			t.Fatalf("统计 %d: %v", i, err)
		}
		assertStatsEqual(t, snap1, snap)
	}

	// 表 B（终身累计）必须同样不变 —— 它是「剪枝后累计不变」的保证。
	lt, err := st.GetUsageLifetime(ctx)
	if err != nil {
		t.Fatalf("读终身累计: %v", err)
	}
	if lt.TotalTokens != snap1.TotalTokens {
		t.Errorf("终身累计 total_tokens = %d, 与统计的 %d 不一致",
			lt.TotalTokens, snap1.TotalTokens)
	}
	if lt.RequestCount != snap1.TotalRequests {
		t.Errorf("终身累计 request_count = %d, 与统计的 %d 不一致",
			lt.RequestCount, snap1.TotalRequests)
	}
}

// E3：剪枝**不得**改变「终身累计」（表 B），即使明细被物理删除。
//
// 与 E1/E2 的区别：这里显式把表 B 当作独立事实来核对 —— 先记下剪枝前的
// 表 B，剪枝后再读，必须逐字段相同。表 B 由触发器维护、与剪枝解耦，
// 这是它存在的唯一理由。
func TestStress_Prune_LifetimeTotalsSurvivePrune(t *testing.T) {
	ctx := context.Background()
	st := testStore(t, t.TempDir()+"/gw.db")

	base := time.Now()
	freezeNow(t, base)
	archiveFixture(t, st, base, DefaultRetentionDays)

	before, err := st.GetUsageLifetime(ctx)
	if err != nil {
		t.Fatalf("剪枝前读终身累计: %v", err)
	}
	if before == nil {
		t.Fatal("终身累计行不存在（usage_totals 的 id=1 占位行丢了）")
	}

	res, err := st.PruneOldUsage(ctx, DefaultRetentionDays)
	if err != nil {
		t.Fatalf("剪枝: %v", err)
	}
	if res.DeletedRows == 0 {
		t.Fatal("剪枝没删任何行，本用例无法证明「删除后累计不变」")
	}

	after, err := st.GetUsageLifetime(ctx)
	if err != nil {
		t.Fatalf("剪枝后读终身累计: %v", err)
	}

	// 逐字段核对表 B。剪枝**只**允许改 pruned_through_day。
	type pair struct {
		name   string
		before int64
		after  int64
	}
	pairs := []pair{
		{"request_count", before.RequestCount, after.RequestCount},
		{"success_count", before.SuccessCount, after.SuccessCount},
		{"error_count", before.ErrorCount, after.ErrorCount},
		{"input_tokens", before.InputTokens, after.InputTokens},
		{"output_tokens", before.OutputTokens, after.OutputTokens},
		{"total_tokens", before.TotalTokens, after.TotalTokens},
		{"cached_tokens", before.CachedTokens, after.CachedTokens},
		{"reasoning_tokens", before.ReasoningTokens, after.ReasoningTokens},
		{"latency_sum_ms", before.LatencySumMs, after.LatencySumMs},
		{"first_record_at", before.FirstRecordAt, after.FirstRecordAt},
	}
	for _, p := range pairs {
		if p.before != p.after {
			t.Errorf("剪枝改动了表 B 的 %s: %d → %d —— 终身累计被剪枝污染",
				p.name, p.before, p.after)
		}
	}
	// 费用是 REAL，单独比（但仍要求精确相等：同一批行求和，没有理由变）。
	if before.CostTotal != after.CostTotal {
		t.Errorf("剪枝改动了表 B 的 cost_total: %v → %v", before.CostTotal, after.CostTotal)
	}
	// 水位**必须**推进 —— 否则上面的「不变」只是因为压根没剪。
	if after.PrunedThrough == "" {
		t.Error("pruned_through_day 仍为空，但删除行数 > 0：水位与删除不一致")
	}
}

// assertStatsEqual 逐字段断言两份统计相等 —— 直接复用 usage_archive_test.go
// 里的那一份（签名 `(t, before, after)`，覆盖字段含 CacheHitRate），
// 不在这里另写一份。
//
// 为什么不复制：复制出来的版本必然在下次加字段时漏跟，表现是
// 「新字段漂移了但测试全绿」。复用的代价是失败信息不带 label，
// 所以调用处先想清楚断言失败时读者需要什么上下文。
