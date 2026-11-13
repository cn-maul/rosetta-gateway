package store

// 「首字之后」的生成耗时（gen time）在两条来源支上的一致性测试。
//
// # 这个指标为什么需要单独的测试
//
// gen time = latency - ttfb，它是一条**派生**指标：分子是两列的差，
// 分母是「两者都测到了」的样本数。于是每个环节都有一处可以写错而不报错：
//
//  1. 条件写宽（只要 status='ok' 就计入）→ 一次 ttfb=0 的请求会把
//     整段 latency 算成「生成耗时」，均值凭空变大；
//  2. 条件写严/漏条件（latency<=ttfb 的脏数据相减）→ 负数进分子，
//     均值偏小；
//  3. 归档支与明细支口径不一致 → 同一天在剪枝前后给出两个数。
//     归档没有 latency/ttfb 的逐条记录，只能用成功延迟累计减首字累计还原，
//     这条还原式子必须与明细支的条件逐字对齐。
//
// 第 3 条是最容易漏的：它只在「剪枝跑过之后」才显形，而剪枝默认关闭。
// 所以下面的用例刻意把两条支分开钉住，再钉一个两段相加≈总耗时的不变量。

import (
	"context"
	"testing"
	"time"
)

// seedGenUsage 插一条只关心 (status, latency, ttfb) 的明细记录。
func seedGenUsage(t *testing.T, st *Store, id string, status string, latencyMs, ttfbMs int64) {
	t.Helper()
	_, err := st.db.ExecContext(context.Background(),
		`INSERT INTO usage_records (id, ts, access_key_id, public_model, provider_id, upstream_model,
		                            ingress_protocol, stream, input_tokens, output_tokens, total_tokens,
		                            reasoning_tokens, cached_tokens, usage_state, status, http_status,
		                            latency_ms, ttfb_ms)
		 VALUES (?, ?, 'k1', 'm', 'p', 'up', 'chat', 0, 10, 20, 30, 0, 0, 'reported', ?, 200, ?, ?)`,
		id, time.Now().UnixMilli(), status, latencyMs, ttfbMs)
	if err != nil {
		t.Fatalf("seed usage %s: %v", id, err)
	}
}

// TestUsageStats_GenTimeDetailBranch 钉住明细支的三条口径：
//   - 成功 + 测到首字 + latency>ttfb → 计入 (latency-ttfb) 与样本数；
//   - ttfb 缺失（0）→ 不计入（否则整段 latency 都会被当成生成时间）；
//   - latency<=ttfb（脏数据）→ 不计入（否则减去负数把均值带偏）；
//   - 非成功请求 → 不计入（生成时间只对「真的吐过字」的请求有意义）。
func TestUsageStats_GenTimeDetailBranch(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()

	seedGenUsage(t, st, "ok-normal", UsageStatusOK, 1200, 200) // 生成 1000，计入
	seedGenUsage(t, st, "ok-no-ttfb", UsageStatusOK, 900, 0)   // 没测到首字，不计入
	seedGenUsage(t, st, "ok-dirty", UsageStatusOK, 500, 600)   // latency<ttfb，不计入
	seedGenUsage(t, st, "err", "error", 800, 100)              // 非成功，不计入

	stats, err := st.GetUsageStats(ctx, 0, time.Now().UnixMilli(), "")
	if err != nil {
		t.Fatalf("GetUsageStats: %v", err)
	}
	if stats.GenSamples != 1 {
		t.Fatalf("生成耗时样本数必须是 1（只有 ok-normal 满足三条条件），实际 %d", stats.GenSamples)
	}
	if stats.GenMs != 1000 {
		t.Fatalf("平均生成耗时必须是 1000ms（1200-200），实际 %v", stats.GenMs)
	}
}

// TestUsageStats_GenTimePlusTtfbIsTotal 钉住「两段相加≈平均总耗时」这个
// 不变量 —— 它是界面上把首字与生成分成两项后仍然说得通的前提。
//
// 两条请求的 ttfb 与生成时间都不同，加权后各自独立成立；如果哪一侧的
// 分母口径错位（比如生成用 n_ttfb 而首字用另一个数），这里会露馅。
func TestUsageStats_GenTimePlusTtfbIsTotal(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()

	seedGenUsage(t, st, "a", UsageStatusOK, 1000, 200) // gen 800
	seedGenUsage(t, st, "b", UsageStatusOK, 3000, 800) // gen 2200

	stats, err := st.GetUsageStats(ctx, 0, time.Now().UnixMilli(), "")
	if err != nil {
		t.Fatalf("GetUsageStats: %v", err)
	}
	if stats.GenSamples != 2 || stats.TtfbSamples != 2 {
		t.Fatalf("两条请求都该同时贡献首字与生成样本，实际 gen=%d ttfb=%d",
			stats.GenSamples, stats.TtfbSamples)
	}
	if stats.GenMs != 1500 {
		t.Fatalf("平均生成耗时必须是 (800+2200)/2 = 1500，实际 %v", stats.GenMs)
	}
	if stats.TtfbMs != 500 {
		t.Fatalf("平均首字必须是 (200+800)/2 = 500，实际 %v", stats.TtfbMs)
	}
}

// TestUsageSource_GenTimeRollupBranch 钉住归档支的还原式子：
// ok_latency_sum_ms - ok_ttfb_sum_ms，样本数沿用 ok_ttfb_count。
//
// 直接查归一化来源而不是走 GetUsageStats：归档行是**聚合**行，
// 这里要断言的是那两列归一口径本身，而不是它们在真库里被 SUM 之后的结果。
func TestUsageSource_GenTimeRollupBranch(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()

	base := time.Date(2026, 1, 5, 0, 0, 0, 0, time.Local)
	day := base.Format("2006-01-02")
	if _, err := st.db.ExecContext(ctx,
		`INSERT INTO usage_daily_rollups (day, user_id, access_key_id, public_model, upstream_model,
		                                 provider_id, ingress_protocol, stream, request_count,
		                                 success_count, error_count, input_tokens, output_tokens,
		                                 total_tokens, cost_total, ttfb_sum_ms,
		                                 ttfb_count, ok_ttfb_sum_ms, ok_ttfb_count, ok_output_tokens,
		                                 ok_latency_sum_ms, ok_sample_count, cost_pending_count)
		 VALUES (?, '', 'k1', 'm', 'up', 'p', 'chat', 0, 3, 3, 0, 30, 60, 90, 0, 1000,
		         3, 1000, 3, 60, 3000, 3, 0)`, day); err != nil {
		t.Fatalf("seed rollup: %v", err)
	}

	src, args := UsageSource(UsageFilter{From: 0, To: 0})
	var genSum, nGen, nGenAll int64
	if err := st.db.QueryRowContext(ctx,
		"SELECT gen_ok_sum, n_gen_ok, (SELECT COALESCE(SUM(ok_ttfb_count),0) FROM usage_daily_rollups) FROM "+src,
		args...).Scan(&genSum, &nGen, &nGenAll); err != nil {
		t.Fatalf("query rollup branch: %v", err)
	}
	if nGenAll != 3 {
		t.Fatalf("归档表里 ok_ttfb_count 应是 3，实际 %d", nGenAll)
	}
	if genSum != 2000 {
		t.Fatalf("归档支生成耗时累计必须是 3000-1000 = 2000，实际 %d", genSum)
	}
	if nGen != 3 {
		t.Fatalf("归档支生成耗时样本数必须沿用 ok_ttfb_count = 3，实际 %d", nGen)
	}
}

// TestUsageSource_GenTimeRollupBranchGuardsDirtyData 钉住归档支的 CASE 保护：
// 归档表里 ok_ttfb_sum_ms > ok_latency_sum_ms 时（脏数据/口径错位），
// 生成耗时必须是 0 而不是负数 —— 负数会直接污染区间均值。
func TestUsageSource_GenTimeRollupBranchGuardsDirtyData(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()

	day := time.Date(2026, 1, 6, 0, 0, 0, 0, time.Local).Format("2006-01-02")
	if _, err := st.db.ExecContext(ctx,
		`INSERT INTO usage_daily_rollups (day, user_id, access_key_id, public_model, upstream_model,
		                                 provider_id, ingress_protocol, stream, request_count,
		                                 success_count, error_count, input_tokens, output_tokens,
		                                 total_tokens, cost_total, ttfb_sum_ms,
		                                 ttfb_count, ok_ttfb_sum_ms, ok_ttfb_count, ok_output_tokens,
		                                 ok_latency_sum_ms, ok_sample_count, cost_pending_count)
		 VALUES (?, '', 'k1', 'm', 'up', 'p', 'chat', 0, 1, 1, 0, 10, 20, 30, 0, 900,
		         1, 900, 1, 20, 500, 1, 0)`, day); err != nil {
		t.Fatalf("seed rollup: %v", err)
	}

	src, args := UsageSource(UsageFilter{From: 0, To: 0})
	var genSum int64
	if err := st.db.QueryRowContext(ctx, "SELECT gen_ok_sum FROM "+src, args...).Scan(&genSum); err != nil {
		t.Fatalf("query rollup branch: %v", err)
	}
	if genSum != 0 {
		t.Fatalf("首字累计大于延迟累计时生成耗时必须是 0（不能是负数），实际 %d", genSum)
	}
}
