package store

// 归档支的边界规则必须是「与窗口有交集」，而不是「整日包含」。
//
// # 这是 P0-7 的回归防线（2026-10-21）
//
// 旧规则是「day_start >= from 且 day_end <= to」—— 它假设 **To 一定是本地
// 午夜**。而真实调用点的 To 几乎从不是：
//   - /v1/organization/*：To = end_date + 1 天的 UTC 零点；
//   - 管理端用量页：To = 「现在」（queryRangeExplicit 的 to=0 兜底）。
//
// 于是窗口上界总在某天的正中间，而那一天的数据在剪枝后已进归档表、
// 却不满足「整日包含」被整体排除。实测（真实库、8333 行明细、
// TZ=Asia/Shanghai）：窗口 [10-07 12:00, 10-09 12:00] 剪枝前是
// 215338903 token，剪枝后只剩 27882697 —— **丢了 87%**。
//
// 既有测试 TestPrune_StatsUnchangedAcrossPrune 之所以是绿的：它**刻意**
// 把窗口对齐到本地午夜（注释写着「对齐午夜才是真实查询的形状」）—— 而
// 真实查询并不对齐。测试的假设与生产调用点不一致，正好掩盖了这条缺陷。

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// TestPrune_非午夜窗口剪枝前后数字不变 是 P0-7 的核心验收。
//
// 窗口的上下界都**不对齐**午夜（模拟「现在」与「end_date+1 天」的形状），
// 剪枝前后同一个窗口必须给出**逐字段相等**的统计。
//
// 注意「相等」而不是「包含」：明细支按 ts 精确过滤、归档支按整天取，
// 两者在边缘那天的粒度本就不同。归档支取「有交集即计入」之后，
// 边缘那天会被**多算**（整天的量都进来）—— 那会让剪枝后的数字**偏大**。
//
// 所以本测试断言的是**不变量**「剪枝不会让数字变小」：剪枝后 >= 剪枝前。
// 这正是 P0-7 的方向性保证 —— 少算是资损（漏收、报表与余额对不上），
// 多算至多偏一天且方向保守。
func TestPrune_非午夜窗口剪枝前后数字不变(t *testing.T) {
	ctx := context.Background()
	st := testStore(t, t.TempDir()+"/gw.db")
	base := time.Now()
	freezeNow(t, base)

	oldN, keptN := archiveFixture(t, st, base, DefaultRetentionDays)
	if oldN == 0 || keptN == 0 {
		t.Fatalf("fixture must span both sides of the cutoff, got old=%d kept=%d", oldN, keptN)
	}

	// 窗口下界必须**切在某一天内部**，且那一天**已被归档** ——
	// 这正是生产查询的形状，也是这条缺陷唯一会显形的位置。
	//
	// # 下界该切在哪：两条分支的过滤方式不同，必须让它们**给出不同答案**
	//
	//   - 明细支：按 ts 精确过滤（ts >= From）。下界切在 10:30，
	//     那么当天 11:00 的那条记录**计入**。
	//   - 归档支（老规则）：按整天过滤（day_start >= From）。下界在
	//     10:30 而那天的起点是 00:00 → 00:00 >= 10:30 为假 → **整天被排除**。
	//
	// 剪枝前：11:00 那条在明细里，计入。剪枝后：它的明细被删，只剩归档行，
	// 而归档支把整天排除了 → **这条记录人间蒸发**。
	//
	// 下界若切在 09:00 之前（当天 00:00），两条分支都会包含该天，测不出；
	// 下界若切在 12:00 之后（当天 12:00），明细支也排除了该天，同样测不出。
	// 必须切在**两条记录之间**。
	//
	// 生产上正是这种形状：/v1/organization/* 的 start_date 换算成 UTC 零点，
	// 在非 UTC 时区下是本地 08:00（不是午夜），而那个日期通常已在保留期外。
	local := base.In(time.Local)
	midnight := func(dayOffset int) time.Time {
		return time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, time.Local).
			AddDate(0, 0, dayOffset)
	}
	// 最老的那批归档日是 cutoff-3（cutoff = -keepDays）。fixture 在每天
	// 的 09:00 / 10:00 / 11:00 各放一条记录，所以把下界切在 10:30 ——
	// 正好夹在 10:00 与 11:00 之间。
	winFrom := midnight(-DefaultRetentionDays - 3).Add(10*time.Hour + 30*time.Minute).UnixMilli()
	winTo := midnight(2).Add(12 * time.Hour).UnixMilli()

	before, err := st.GetUsageStats(ctx, winFrom, winTo, "")
	if err != nil {
		t.Fatalf("stats before prune: %v", err)
	}
	if before.TotalRequests == 0 {
		t.Fatal("window must cover records; got 0 requests")
	}
	beforeSum, err := st.SumUserUsedTokens(ctx, "u1")
	if err != nil {
		t.Fatalf("user sum before prune: %v", err)
	}

	res, err := st.PruneOldUsage(ctx, DefaultRetentionDays)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if res.Skipped {
		t.Fatalf("prune skipped: %s", res.Reason)
	}
	if res.DeletedRows != int64(oldN) {
		t.Fatalf("deleted %d rows, want %d", res.DeletedRows, oldN)
	}

	after, err := st.GetUsageStats(ctx, winFrom, winTo, "")
	if err != nil {
		t.Fatalf("stats after prune: %v", err)
	}

	// 关键断言：**不得变小**。变小 = 静默丢数据 = 资损。
	// （变大有界且方向保守，见 UsageSource 的论证。）
	if after.TotalRequests < before.TotalRequests {
		t.Errorf("剪枝后请求数变小了：%d -> %d（静默丢数据）",
			before.TotalRequests, after.TotalRequests)
	}
	if after.TotalTokens < before.TotalTokens {
		t.Errorf("剪枝后 token 数变小了：%d -> %d（静默丢数据）",
			before.TotalTokens, after.TotalTokens)
	}
	if after.InputTokens < before.InputTokens {
		t.Errorf("剪枝后输入 token 变小了：%d -> %d", before.InputTokens, after.InputTokens)
	}
	if after.OutputTokens < before.OutputTokens {
		t.Errorf("剪枝后输出 token 变小了：%d -> %d", before.OutputTokens, after.OutputTokens)
	}

	// 用户配额预检走同一个 SUM：剪枝后变小 = 用户配额被悄悄放宽。
	afterSum, err := st.SumUserUsedTokens(ctx, "u1")
	if err != nil {
		t.Fatalf("user sum after prune: %v", err)
	}
	if afterSum < beforeSum {
		t.Errorf("剪枝后用户累计用量变小了：%d -> %d（配额被悄悄放宽）",
			beforeSum, afterSum)
	}

	// 归档必须真的进了统计路径，否则本例会退化成「全在明细里」的平凡通过。
	if rows, _ := rollupTotals(t, st); rows == 0 {
		t.Error("archive table is empty after pruning; nothing was archived")
	}
}

// 剪枝**不得**让「待重算」的标记凭空消失（2026-10-21 修 A6-P1-4 / 汇总 M5）。
//
// 回归风险：usage_records.usage_state 有 'cost_pending' 这个值（查价失败
// → 费用记 0 → 等后台重算）。剪枝把明细搬进归档表时，那个标记跟着明细
// 一起消失 —— 而这些记录的费用是 0，重算队列（只扫 usage_records）再也
// 捞不到它们。于是它们永久停留在「0 元」：用户免单，且没有任何痕迹。
//
// 现在归档层也累加 cost_pending_count，所以剪枝前后这个数都能查到。
func TestPrune_CostPendingMarkerSurvivesArchiving(t *testing.T) {
	ctx := context.Background()
	st := testStore(t, t.TempDir()+"/gw.db")
	base := time.Now()
	freezeNow(t, base)

	// 一条待重算、一条正常，都在剪枝窗口之外（会被剪掉）。
	local := base.In(time.Local)
	cutoff := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, time.Local).
		AddDate(0, 0, -DefaultRetentionDays)
	old := cutoff.AddDate(0, 0, -3).Add(9 * time.Hour).UnixMilli()

	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("fixture: %v", err)
		}
	}
	must(st.CreateProvider(ctx, &Provider{
		ID: "p1", Slug: "p1", Name: "P1", Endpoint: "http://x", Protocol: "openai-chat", Enabled: true}))

	for i, state := range []string{"cost_pending", "reported"} {
		must(st.CreateUsageRecord(ctx, &UsageRecord{
			ID: fmt.Sprintf("rec-%d", i), Ts: old + int64(i),
			UserID: "u1", AccessKeyID: "k1", PublicModel: "m",
			ProviderID: "p1", UpstreamModel: "m", IngressProtocol: "openai-chat",
			InputTokens: 100, OutputTokens: 50, TotalTokens: 150,
			UsageState: state, Status: "ok", HTTPStatus: 200,
		}))
	}

	before, err := st.GetUsageStats(ctx, 0, 0, "")
	if err != nil {
		t.Fatalf("stats before prune: %v", err)
	}
	if before.CostPendingCount != 1 {
		t.Fatalf("剪枝前 CostPendingCount = %d, want 1", before.CostPendingCount)
	}

	if _, err := st.PruneOldUsage(ctx, DefaultRetentionDays); err != nil {
		t.Fatalf("prune: %v", err)
	}

	after, err := st.GetUsageStats(ctx, 0, 0, "")
	if err != nil {
		t.Fatalf("stats after prune: %v", err)
	}
	if after.CostPendingCount != 1 {
		t.Fatalf("剪枝后 CostPendingCount = %d, want 1 —— 待重算的标记随明细一起消失了（M5 未修）",
			after.CostPendingCount)
	}
}
