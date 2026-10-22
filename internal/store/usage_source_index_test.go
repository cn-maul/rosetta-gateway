package store

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestUsageSource_RollupBranchUsesDayRangeIndex 钉住 R10 的修复：
// 归档支的过滤谓词必须是 **day 裸列**的范围比较，不能是关于 day 的表达式。
//
// # 缺陷的形状
//
// 旧谓词是「dayEndMs > ? AND dayStartMs <= ?」，其中
// dayStartMs = CAST(strftime('%s', day, 'utc') AS INTEGER) * 1000。
// 谓词左侧是关于索引列的表达式，SQLite 无法把它化成索引查找区间 ——
// 实测查询计划是
//
//	SCAN usage_daily_rollups USING COVERING INDEX idx_rollup_day
//
// 也就是整个索引全扫。表 A 随时间单调增长，而「全部历史」窗口恰好要
// 扫全部行，于是归档/剪枝相关扫描退化为全表。
//
// 修法是把谓词换成「day >= ? AND day <= ?」：左侧是裸列，SQLite 直接
// 用它做索引区间查找（SEARCH ... USING COVERING INDEX idx_rollup_day
// (day>? AND day<?)）。
//
// # 为什么用 EXPLAIN QUERY PLAN 而不是断言耗时
//
// 耗时受机器与数据量影响，测试会flaky；而「是否走索引区间」是 SQLite
// 的查询计划性质，稳定可断言。耗时数据留在修复时的实测记录里。
func TestUsageSource_RollupBranchUsesDayRangeIndex(t *testing.T) {
	st := newStore(t)

	// 造一天的归档行（足够让查询计划成形）。
	_, err := st.db.ExecContext(context.Background(),
		`INSERT INTO usage_daily_rollups (day, user_id, access_key_id, public_model,
		                            upstream_model, provider_id, ingress_protocol, stream,
		                            request_count, total_tokens)
		 VALUES ('2026-01-01', '', 'k1', 'm', 'up', 'p', 'chat', 0, 1, 100)`)
	if err != nil {
		t.Fatalf("seed rollup: %v", err)
	}

	src, args := UsageSource(UsageFilter{From: 1000, To: 2000})
	// 每个条件在明细支与归档支各出现一次，所以 From/To 各占两个参数。
	if len(args) != 4 {
		t.Fatalf("参数数 = %d，期望 4（From/To 各在两支各一次）", len(args))
	}

	// src 是「SELECT ... FROM (subquery) u」的整体，要包一层才能直接查。
	rows, err := st.db.QueryContext(context.Background(),
		"EXPLAIN QUERY PLAN SELECT * FROM "+src, args...)
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	defer rows.Close()

	var plan []string
	for rows.Next() {
		var id, parent, notused int
		var detail string
		if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
			t.Fatalf("scan plan: %v", err)
		}
		plan = append(plan, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate plan: %v", err)
	}

	joined := strings.Join(plan, " | ")
	// 归档支必须命中 idx_rollup_day 的**区间查找**。
	//
	// 退化形态是 "SCAN usage_daily_rollups USING COVERING INDEX idx_rollup_day"
	// （全扫）；修好之后是
	// "SEARCH usage_daily_rollups USING COVERING INDEX idx_rollup_day (day>? AND day<?)"
	// （区间查找）。
	if !strings.Contains(joined, "SEARCH usage_daily_rollups") {
		t.Fatalf("归档支没有走索引区间查找，查询计划退化：\n%s", joined)
	}
	if !strings.Contains(joined, "idx_rollup_day") {
		t.Fatalf("归档支没有使用 idx_rollup_day，查询计划：\n%s", joined)
	}
	if !strings.Contains(joined, "day>?") {
		t.Fatalf("归档支不是按 day 列区间过滤（谓词左侧必须是裸列），查询计划：\n%s", joined)
	}
}

// TestUsageSource_RollupDayRangeMatchesIntersectionRule 钉住 R10 改法的
// **语义等价性**：归档支按 day 文本范围过滤后，命中的天集合仍等于
// 「与窗口有交集的天」（P0-7 确立的边界规则）。
//
// 旧谓词算的是「那一天的本地午夜落在窗口内」，新谓词算的是
// 「那一天有任何一刻落在窗口内」。后者更宽，而 P0-7 明确选了更宽的
// 那个（宁可多算不可少算）—— 这条测试把那个选择钉住。
func TestUsageSource_RollupDayRangeMatchesIntersectionRule(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()

	// 造 5 天连续归档行。
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.Local)
	for i := 0; i < 5; i++ {
		day := base.AddDate(0, 0, i).Format("2006-01-02")
		_, err := st.db.ExecContext(ctx,
			`INSERT INTO usage_daily_rollups (day, user_id, access_key_id, public_model,
			                            upstream_model, provider_id, ingress_protocol, stream,
			                            request_count, total_tokens)
			 VALUES (?, '', 'k1', 'm', 'up', 'p', 'chat', 0, 1, 100)`, day)
		if err != nil {
			t.Fatalf("seed rollup %s: %v", day, err)
		}
	}

	// 窗口取第 2 天的正中间到第 4 天的正中间：按「有交集即计入」，
	// 第 2、3、4 天都要被计入。
	from := base.AddDate(0, 0, 1).Add(12 * time.Hour).UnixMilli()
	to := base.AddDate(0, 0, 3).Add(12 * time.Hour).UnixMilli()

	src, args := UsageSource(UsageFilter{From: from, To: to})
	rows, err := st.db.QueryContext(ctx, "SELECT * FROM "+src, args...)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()

	got := map[string]bool{}
	for rows.Next() {
		var (
			n, nOK, nErr                             int64
			in, out, cached, cachedCreation          int64
			reasoning, total                         int64
			cost                                     float64
			lat, ttfbSum, nTTFB                      int64
			ttfbOKSum, nTTFBOK                       int64
			outOK, latOK, nOKSpeed                   int64
			nCostPending                             int64
			day                                      string
			bucketTS                                 int64
			userID, keyID, pubModel, upModel, provID string
		)
		if err := rows.Scan(&n, &nOK, &nErr, &in, &out, &cached, &cachedCreation, &reasoning, &total,
			&cost, &lat, &ttfbSum, &nTTFB, &ttfbOKSum, &nTTFBOK, &outOK, &latOK,
			&nOKSpeed, &nCostPending, &day, &bucketTS, &userID, &keyID, &pubModel,
			&upModel, &provID); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got[day] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate: %v", err)
	}

	for _, want := range []string{"2026-01-02", "2026-01-03", "2026-01-04"} {
		if !got[want] {
			t.Errorf("归档支漏掉 %s —— 窗口与该天有交集，必须计入", want)
		}
	}
	for _, notWant := range []string{"2026-01-01", "2026-01-05"} {
		if got[notWant] {
			t.Errorf("归档支多算 %s —— 该天与窗口无交集", notWant)
		}
	}
}
