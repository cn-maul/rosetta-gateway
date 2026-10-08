package store

// UsageByUser 的回归测试（钱包页「消费汇总」一表一行一个用户的数据源）。
//
// 本模块要钉住的不变量（逐条对应下面的用例）：
//   - **金额正确**：每行 cost 是 SUM(cost_total)，即落库时固化的费用之和，
//     不是按当前单价重算的。
//   - **排序稳定**：费用降序；同额时按 user_id 兜底 —— 没有 tiebreaker 时
//     相对顺序由查询计划决定，两次刷新之间会跳。
//   - **用户已删除时退化**：username 为空但 UserID 仍在，且那一行的金额
//     不能被丢掉（用量是历史事实，删用户不是抹账）。
//   - **剪枝后不缩水**：明细被归档进表 A 之后，按用户的合计必须与剪枝前
//     一致。这是 UsageSource（明细 ∪ 日归档）存在的全部理由，而漏掉归档支
//     在「还没剪过枝」的库上完全测不出来（那一支恒为空）—— 所以必须真剪枝。
//   - **无归属用量单独成行**：user_id 为空的迁移遗留记录不能被静默丢掉，
//     否则这张表的合计小于全局合计，而对不上账是最难查的一类问题。
//
// 计价用一个**有单价**的模型（复用 balance_dao_test 的 seedPricedModel）：
// 不给单价时所有 cost_total 都是 0，测出来的「金额正确」是假通过 —— 0 == 0
// 恒成立，改坏了也看不出来。

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// pricedModel 是 seedPriced 建出的上游模型名。
//
// freezeUsageCost 按 (provider_id, upstream_model) 查价，所以用量记录的
// UpstreamModel **必须**等于模型表里的 ModelID，否则查价失败 → cost 记 0 →
// 全部金额断言退化成 0 == 0。
const pricedModel = "priced"

// seedByUserUsage 落一条用量记录（字段取测试关心的那几个）。
//
// 名字刻意与 user_dao_test.go 的 seedUsage 区分：同一个 package 里两个同名
// 辅助函数会直接编译失败。
func seedByUserUsage(t *testing.T, st *Store, id, userID, keyID string, ts, in, out int64) {
	t.Helper()
	if err := st.CreateUsageRecord(context.Background(), &UsageRecord{
		ID: id, Ts: ts, UserID: userID, AccessKeyID: keyID,
		PublicModel: pricedModel, ProviderID: "p1", UpstreamModel: pricedModel,
		IngressProtocol: "openai-chat",
		InputTokens:     in, OutputTokens: out, TotalTokens: in + out,
		Status: "ok", HTTPStatus: 200, UsageState: "reported",
	}); err != nil {
		t.Fatalf("seed usage %s: %v", id, err)
	}
}

// seedPriced 建 p1 + 单价 **1 元 / 百万** input token 的模型（输出免费）。
//
// ⚠️ 价格单位是「元 / 百万 tokens」，所以这里传 1.0 而不是 1_000_000 ——
// priceUsage 的公式是 `token 数 × 单价 / 1e6`，传 1e6 会算出 50 万元一条，
// 于是金额断言全部离谱地偏大（而那种失败看起来像「聚合算错了」，
// 实际只是价的量纲搞错了）。
//
// 于是 100_000 input token 恰好 0.1 元 —— 用例里的金额都能手算核对。
func seedPriced(t *testing.T, st *Store) {
	t.Helper()
	seedPricedModel(t, st, 1.0, 0, 0)
}

// seedByNameUser 建一个普通用户（用户名 = id）。
//
// 为什么必须建：UsageByUser 靠 LEFT JOIN users 取展示名，不建用户就测不出
// 「JOIN 生效」—— 而「用户名恒为空」看起来与「用户已删除」一模一样，
// 那正是本文件要区分的两件事。
func seedByNameUser(t *testing.T, st *Store, ids ...string) {
	t.Helper()
	for _, id := range ids {
		if err := st.CreateUser(context.Background(), &User{
			ID: id, Username: id, PasswordHash: "x",
			Role: RoleUser, Status: UserStatusActive,
		}); err != nil {
			t.Fatalf("create user %s: %v", id, err)
		}
	}
}

// TestUsageByUser_CostAndOrdering 守住金额、次数与降序排序。
func TestUsageByUser_CostAndOrdering(t *testing.T) {
	st := testStore(t, t.TempDir()+"/gw.db")
	ctx := t.Context()
	seedPriced(t, st)
	seedByNameUser(t, st, "user-a", "user-b", "user-c")

	now := time.Now().UnixMilli()
	// 三个用户，输入 token 数不同 → 金额必然不同（1e6 token = 1 元）。
	// 刻意让**创建顺序与金额顺序相反**：若实现漏了 ORDER BY，行序会是插入序，
	// 这条断言就能抓到；而「按插入顺序恰好也对」的用例是测不出排序的。
	seedByUserUsage(t, st, "u-small", "user-a", "kA", now, 100_000, 0) // 0.10 元
	seedByUserUsage(t, st, "u-big", "user-c", "kC", now, 500_000, 0)   // 0.50 元
	seedByUserUsage(t, st, "u-mid", "user-b", "kB", now, 300_000, 0)   // 0.30 元

	rows, total, err := st.UsageByUser(ctx, 0, now+1, 100)
	if err != nil {
		t.Fatalf("UsageByUser: %v", err)
	}
	if total != 3 {
		t.Fatalf("total = %d, want 3", total)
	}
	if len(rows) != 3 {
		t.Fatalf("got %d rows, want 3: %+v", len(rows), rows)
	}

	// 降序：c(0.50) > b(0.30) > a(0.10)
	wantOrder := []struct {
		user string
		cost float64
	}{{"user-c", 0.5}, {"user-b", 0.3}, {"user-a", 0.1}}
	for i, want := range wantOrder {
		got := rows[i]
		if got.UserID != want.user {
			t.Errorf("row %d: user = %q, want %q (order must be cost DESC)", i, got.UserID, want.user)
		}
		// 浮点比较给 epsilon：cost_total 是 REAL，除 1e6 后有二进制尾数。
		if diff := got.Cost - want.cost; diff > 1e-9 || diff < -1e-9 {
			t.Errorf("row %d (%s): cost = %v, want %v", i, got.UserID, got.Cost, want.cost)
		}
		if got.Requests != 1 {
			t.Errorf("row %d (%s): requests = %d, want 1", i, got.UserID, got.Requests)
		}
		if got.Username != want.user {
			t.Errorf("row %d: username = %q, want %q (JOIN users failed?)", i, got.Username, want.user)
		}
	}
}

// TestUsageByUser_TieBreakIsStable 同额时必须有稳定兜底键。
//
// 没有 tiebreaker 时 SQLite 对同额行的相对顺序不做保证（换个查询计划就变），
// 界面上表现为「刷新一下行就换位置」。所以断言 user_id 升序。
func TestUsageByUser_TieBreakIsStable(t *testing.T) {
	st := testStore(t, t.TempDir()+"/gw.db")
	ctx := t.Context()
	seedPriced(t, st)
	seedByNameUser(t, st, "aaa", "mmm", "zzz")

	now := time.Now().UnixMilli()
	// 三个用户金额**完全相同**，且插入顺序刻意与 user_id 字典序相反。
	for _, u := range []string{"zzz", "mmm", "aaa"} {
		seedByUserUsage(t, st, "rec-"+u, u, "k-"+u, now, 200_000, 0)
	}

	rows, _, err := st.UsageByUser(ctx, 0, now+1, 100)
	if err != nil {
		t.Fatalf("UsageByUser: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("got %d rows, want 3", len(rows))
	}
	for i, want := range []string{"aaa", "mmm", "zzz"} {
		if rows[i].UserID != want {
			t.Fatalf("row %d = %q, want %q (tie must fall back to user_id ASC for stable ordering); got %+v",
				i, rows[i].UserID, want, rows)
		}
	}
}

// TestUsageByUser_DeletedUserKeepsCost 用户已删除时**照实显示**那一行。
//
// 用量是历史事实（见 DeleteUser 的注释：删用户不抹用量）。若这里用 INNER JOIN
// 或按 users 过滤，已删用户的那笔消费会从表里消失 —— 于是这张表的合计小于
// 总览的全局合计，而管理员完全看不出少在哪。
//
// 断言：UserID 仍在、Username 为空（前端据此回退显示 id）、金额仍在。
func TestUsageByUser_DeletedUserKeepsCost(t *testing.T) {
	st := testStore(t, t.TempDir()+"/gw.db")
	ctx := t.Context()
	seedPriced(t, st)

	// 先建用户再落用量：这样才能先确认「JOIN 拿得到名字」，删掉之后的空
	// 才是删除造成的 —— 否则测试可能因为「一开始就没建用户」而以错误的
	// 理由通过。
	if err := st.CreateUser(ctx, &User{ID: "ghost", Username: "ghost", PasswordHash: "x"}); err != nil {
		t.Fatalf("create user: %v", err)
	}

	now := time.Now().UnixMilli()
	seedByUserUsage(t, st, "rec-gone", "ghost", "kG", now, 400_000, 0)

	before, _, err := st.UsageByUser(ctx, 0, now+1, 100)
	if err != nil {
		t.Fatalf("UsageByUser before delete: %v", err)
	}
	if len(before) != 1 || before[0].Username != "ghost" {
		t.Fatalf("precondition failed: want username %q, got %+v", "ghost", before)
	}
	if before[0].Role != RoleUser {
		t.Fatalf("precondition failed: role = %q, want %q", before[0].Role, RoleUser)
	}

	if err := st.DeleteUser(ctx, "ghost"); err != nil {
		t.Fatalf("delete user: %v", err)
	}

	rows, total, err := st.UsageByUser(ctx, 0, now+1, 100)
	if err != nil {
		t.Fatalf("UsageByUser: %v", err)
	}
	if total != 1 || len(rows) != 1 {
		t.Fatalf("deleted-user row must not be dropped: total=%d rows=%+v", total, rows)
	}
	got := rows[0]
	if got.UserID != "ghost" {
		t.Errorf("user_id = %q, want %q", got.UserID, "ghost")
	}
	if got.Username != "" {
		t.Errorf("username = %q, want empty (row must be identifiable as a deleted user)", got.Username)
	}
	if got.Role != "" {
		t.Errorf("role = %q, want empty for a deleted user", got.Role)
	}
	if diff := got.Cost - 0.4; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("cost = %v, want 0.4 — a deleted user's spend must survive", got.Cost)
	}
}

// TestUsageByUser_UnattributedRowIsKept 无归属用量（user_id 为空）单独成行。
//
// 迁移前的无主 key 产生的记录 user_id 为 NULL。把它过滤掉会让这张表的合计
// 小于总览的全局合计 —— 而管理员看到两个数字对不上时，没有任何线索能指向
// 「少了一条无归属的用量」。
func TestUsageByUser_UnattributedRowIsKept(t *testing.T) {
	st := testStore(t, t.TempDir()+"/gw.db")
	ctx := t.Context()
	seedPriced(t, st)
	seedByNameUser(t, st, "someone")

	now := time.Now().UnixMilli()
	seedByUserUsage(t, st, "rec-orphan", "", "kOrphan", now, 100_000, 0)
	seedByUserUsage(t, st, "rec-owned", "someone", "kOwned", now, 200_000, 0)

	rows, total, err := st.UsageByUser(ctx, 0, now+1, 100)
	if err != nil {
		t.Fatalf("UsageByUser: %v", err)
	}
	if total != 2 || len(rows) != 2 {
		t.Fatalf("unattributed usage must be its own row: total=%d rows=%+v", total, rows)
	}
	if rows[0].UserID != "someone" {
		t.Fatalf("row 0 = %q, want %q (0.2 > 0.1)", rows[0].UserID, "someone")
	}
	if rows[1].UserID != "" {
		t.Errorf("row 1 user_id = %q, want empty for unattributed usage", rows[1].UserID)
	}
	if diff := rows[1].Cost - 0.1; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("unattributed cost = %v, want 0.1", rows[1].Cost)
	}
}

// TestUsageByUser_SurvivesPrune 是本文件最重要的用例：剪枝后合计不缩水。
//
// 若只查明细（不走 UsageSource），30 天前的用量被归档后，这个用户在这张表上
// 的消费会**逐日变小** —— 管理员据此对账会得出「他上个月没花钱」。
// 而漏掉归档支在「还没剪过枝」的库上完全看不出来（归档支恒为空），
// 所以必须先真剪枝再断言。
func TestUsageByUser_SurvivesPrune(t *testing.T) {
	st := testStore(t, t.TempDir()+"/gw.db")
	ctx := t.Context()
	seedPriced(t, st)
	seedByNameUser(t, st, "alice")

	// 一条在保留窗口外（会被剪进表 A），一条在窗口内（留在明细）。
	old := time.Now().AddDate(0, 0, -(DefaultRetentionDays + 5)).UnixMilli()
	recent := time.Now().Add(-time.Hour).UnixMilli()
	seedByUserUsage(t, st, "rec-old", "alice", "kA", old, 300_000, 0)
	seedByUserUsage(t, st, "rec-new", "alice", "kA", recent, 100_000, 0)

	before, _, err := st.UsageByUser(ctx, 0, time.Now().UnixMilli(), 100)
	if err != nil {
		t.Fatalf("UsageByUser before prune: %v", err)
	}
	if len(before) != 1 {
		t.Fatalf("want 1 user before prune, got %+v", before)
	}
	wantCost, wantReqs, wantTokens := before[0].Cost, before[0].Requests, before[0].Tokens
	if wantReqs != 2 {
		t.Fatalf("precondition: want 2 requests before prune, got %d", wantReqs)
	}

	if _, err := st.PruneOldUsage(ctx, DefaultRetentionDays); err != nil {
		t.Fatalf("prune: %v", err)
	}

	after, _, err := st.UsageByUser(ctx, 0, time.Now().UnixMilli(), 100)
	if err != nil {
		t.Fatalf("UsageByUser after prune: %v", err)
	}
	if len(after) != 1 {
		t.Fatalf("want 1 user after prune, got %+v", after)
	}
	if after[0].Requests != wantReqs {
		t.Errorf("requests changed across prune: %d -> %d (archive branch not wired)",
			wantReqs, after[0].Requests)
	}
	if diff := after[0].Cost - wantCost; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("cost changed across prune: %v -> %v (archive branch not wired)",
			wantCost, after[0].Cost)
	}
	if after[0].Tokens != wantTokens {
		t.Errorf("tokens changed across prune: %d -> %d", wantTokens, after[0].Tokens)
	}
}

// TestUsageByUser_LimitAndTotal 守住「截断可见」：total 是**全部**用户数，
// 不受 limit 影响，且返回行数被 limit 钳制。
//
// 这一条是给前端「显示前 N / 共 M 位用户」用的。若 total 也跟着 limit 变，
// 一张被截断的表与一张完整的表在界面上完全同形 —— 而这张表是按金额读账的，
// 把「前 N 名」读成全量会直接得出错误结论。
func TestUsageByUser_LimitAndTotal(t *testing.T) {
	st := testStore(t, t.TempDir()+"/gw.db")
	ctx := t.Context()
	seedPriced(t, st)
	seedByNameUser(t, st, "u0", "u1", "u2", "u3", "u4")

	now := time.Now().UnixMilli()
	// 5 个用户，金额递减（token 数决定），便于断言截断后留下的是**最贵的**几个。
	for i := 0; i < 5; i++ {
		user := fmt.Sprintf("u%d", i)
		seedByUserUsage(t, st, "rec-"+user, user, "k-"+user, now, int64(100_000*(5-i)), 0)
	}

	rows, total, err := st.UsageByUser(ctx, 0, now+1, 3)
	if err != nil {
		t.Fatalf("UsageByUser: %v", err)
	}
	if total != 5 {
		t.Errorf("total = %d, want 5 (must not be affected by limit)", total)
	}
	if len(rows) != 3 {
		t.Fatalf("got %d rows, want 3 (limit not applied): %+v", len(rows), rows)
	}
	// 截断必须留下**最贵的**那几个：u0 最贵，u1 次之，u2 第三。
	for i, want := range []string{"u0", "u1", "u2"} {
		if rows[i].UserID != want {
			t.Errorf("row %d = %q, want %q (truncation must keep the top spenders)",
				i, rows[i].UserID, want)
		}
	}

	// limit <= 0 表示不设限（调用方必须自己钳制，见 UsageByUser 注释）。
	all, totalAll, err := st.UsageByUser(ctx, 0, now+1, 0)
	if err != nil {
		t.Fatalf("UsageByUser unlimited: %v", err)
	}
	if len(all) != 5 || totalAll != 5 {
		t.Errorf("limit=0 must mean no limit: got %d rows, total=%d", len(all), totalAll)
	}
}

// TestUsageByUser_SumEqualsGlobalStats 钉住界面上那句「本表合计与上方
// 『用户消费金额』应当一致」。
//
// # 为什么这条必须有用例
//
// 钱包页把这两个数字并排显示，并明确告诉管理员「不一致说明有一侧读取失败」。
// 那句文案只有在两者**恒等**时才成立 —— 而它们来自两个不同的聚合查询
// （stats 走 SUM(u.cost_total) 单行合计，本表走 GROUP BY user_id 后逐行求和）。
// 两条查询任何一处口径分叉（不同的 FROM、不同的时间边界、漏了某一支 UNION、
// 归档那半边只在一边接上），界面上就会出现一个**看起来像数据损坏**的差额，
// 而管理员会去排查一个实际不存在的账目问题。
//
// 三种场景各测一遍，因为它们的 SQL 路径不同：
//   - 明细（未剪枝）；
//   - 归档（剪枝后，数据只来自表 A）；
//   - 混合（一条在窗口内、一条在外）。
func TestUsageByUser_SumEqualsGlobalStats(t *testing.T) {
	st := testStore(t, t.TempDir()+"/gw.db")
	ctx := t.Context()
	seedPriced(t, st)
	seedByNameUser(t, st, "alice", "bob", "carol")

	old := time.Now().AddDate(0, 0, -(DefaultRetentionDays + 5)).UnixMilli()
	recent := time.Now().Add(-time.Hour).UnixMilli()
	seedByUserUsage(t, st, "r1", "alice", "kA", old, 300_000, 0)
	seedByUserUsage(t, st, "r2", "alice", "kA", recent, 100_000, 0)
	seedByUserUsage(t, st, "r3", "bob", "kB", recent, 250_000, 0)
	seedByUserUsage(t, st, "r4", "carol", "kC", old, 50_000, 0)
	// 无归属那一行也必须计入合计（它就是 stats 眼里的全局用量之一）。
	seedByUserUsage(t, st, "r5", "", "kOrphan", recent, 70_000, 0)

	// assertEqual 比对「按用户求和」与全局 stats。
	//
	// 用 stats 的同一个 from/to 口径（0 = 不限起点），与 handler 里的
	// groupFilter/usageFilter 保持一致。
	assertEqual := func(t *testing.T, label string) {
		t.Helper()
		to := time.Now().UnixMilli()
		rows, _, err := st.UsageByUser(ctx, 0, to, 0)
		if err != nil {
			t.Fatalf("%s: UsageByUser: %v", label, err)
		}
		var sumCost float64
		var sumReqs, sumTokens int64
		for _, r := range rows {
			sumCost += r.Cost
			sumReqs += r.Requests
			sumTokens += r.Tokens
		}
		global, err := st.GetUsageStats(ctx, 0, to, "")
		if err != nil {
			t.Fatalf("%s: GetUsageStats: %v", label, err)
		}
		if diff := sumCost - global.Cost; diff > 1e-9 || diff < -1e-9 {
			t.Errorf("%s: per-user cost sum = %v, global stats cost = %v (差 %v) — "+
				"界面上写着两者应当一致，分叉会让管理员去查一个不存在的账目问题",
				label, sumCost, global.Cost, diff)
		}
		if sumReqs != global.TotalRequests {
			t.Errorf("%s: per-user requests = %d, global = %d", label, sumReqs, global.TotalRequests)
		}
		if sumTokens != global.TotalTokens {
			t.Errorf("%s: per-user tokens = %d, global = %d", label, sumTokens, global.TotalTokens)
		}
		// 用户数应当是 4（alice / bob / carol / 无归属），本用例顺带钉住
		// 「无归属也单独成行且计入合计」。
		if len(rows) != 4 {
			t.Errorf("%s: got %d rows, want 4 (3 users + 1 unattributed)", label, len(rows))
		}
	}

	t.Run("detail-only", func(t *testing.T) { assertEqual(t, "detail-only") })

	// 剪枝后：**窗口外**的老记录进了表 A，而窗口内的仍在明细里 ——
	// 此时同一份合计同时来自两支（明细 ∪ 归档），是最容易分叉的场景。
	if _, err := st.PruneOldUsage(ctx, DefaultRetentionDays); err != nil {
		t.Fatalf("prune: %v", err)
	}
	t.Run("mixed-detail-and-archive", func(t *testing.T) {
		assertEqual(t, "mixed-detail-and-archive")
	})
}

// TestUsageByUser_TimeWindow 时间窗口必须生效。
//
// 时间窗口是 UsageByUser 与「全部历史」的唯一区别 —— 若它被忽略，界面上
// 无论选哪个区间都会看到同一个数，而用户无从发现。刻意让窗口外的那条金额更大，
// 这样「窗口没生效」会表现为合计偏大，而不是恰好相等。
func TestUsageByUser_TimeWindow(t *testing.T) {
	st := testStore(t, t.TempDir()+"/gw.db")
	ctx := t.Context()
	seedPriced(t, st)
	seedByNameUser(t, st, "bob")

	now := time.Now().UnixMilli()
	inWindow := now - 3600_000
	outWindow := now - 48*3600_000
	seedByUserUsage(t, st, "rec-in", "bob", "kB", inWindow, 100_000, 0)
	seedByUserUsage(t, st, "rec-out", "bob", "kB", outWindow, 900_000, 0)

	rows, _, err := st.UsageByUser(ctx, now-24*3600_000, now, 100)
	if err != nil {
		t.Fatalf("UsageByUser: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	if rows[0].Requests != 1 {
		t.Errorf("requests = %d, want 1 (out-of-window record leaked in)", rows[0].Requests)
	}
	if diff := rows[0].Cost - 0.1; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("cost = %v, want 0.1 (only the in-window record)", rows[0].Cost)
	}

	// 全部历史：两条都要在。
	rowsAll, _, err := st.UsageByUser(ctx, 0, now, 100)
	if err != nil {
		t.Fatalf("UsageByUser all: %v", err)
	}
	if len(rowsAll) != 1 {
		t.Fatalf("got %d rows, want 1", len(rowsAll))
	}
	if rowsAll[0].Requests != 2 {
		t.Errorf("requests = %d, want 2 for the full window", rowsAll[0].Requests)
	}
}
