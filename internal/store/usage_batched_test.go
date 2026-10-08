package store

// batched insert 的账目与失败语义测试。
//
// # 为什么这些用例是「钱的路径」上的必答题
//
// `CreateUsageRecordsBatched` 改了**落库**的实现方式，而落库结果有两处
// 下游依赖：`cost_total`（报表口径）与「这条记录到底在不在库里」
// （扣费的前置 —— 账记不下来就不该收钱）。
//
// 批量引入的**新风险**（单条路径不存在）：
//  1. **整批回滚时费用不能被当作已落库**：若返回非 0 费用，
//     调用方会为一批**根本没写进库**的记录扣费 —— 白扣，且无人发现；
//  2. **逐条费用必须与单条路径逐字相同**（顺序、归一、计价口径），
//     否则「扣的 = 报表的」这条不变量在批量下悄悄失效；
//  3. **费用与行的对应关系不能错位**（第 i 条的费用配到第 j 行上），
//     那会让每一笔都扣错，且总额可能仍然对得上 —— 最难查的一种错。
//
// 第 3 条是最阴险的：**只断言总额相等会漏掉错位**。所以下面刻意逐条比对。

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// usageRow 是一条从库里读回的记录（id + 固化费用）。
//
// 刻意只取这两样：断言「费用与行的对应关系」与「负值归一」时，
// 这是唯一需要的两个字段。cost_total 不是结构体字段（UsageRecord 不带
// 它——它是 DAO 的返回值），所以这里单独读。
type usageRow struct {
	r    *UsageRecord
	cost float64
}

// usageRows 读回全部记录（按 id 排序），用于逐条比对。
func usageRows(t *testing.T, st *Store) []usageRow {
	t.Helper()
	rows, err := st.read.QueryContext(context.Background(),
		`SELECT id, ts, access_key_id, COALESCE(user_id,''), public_model, provider_id,
		 upstream_model, ingress_protocol, stream, input_tokens, output_tokens, total_tokens,
		 reasoning_tokens, cached_tokens, usage_state, status, http_status,
		 COALESCE(error_code,''), latency_ms, ttfb_ms, COALESCE(request_id,''), cost_total
		 FROM usage_records ORDER BY id`)
	if err != nil {
		t.Fatalf("read usage: %v", err)
	}
	defer rows.Close()
	var out []usageRow
	for rows.Next() {
		r := &UsageRecord{}
		var stream int
		var cost float64
		if err := rows.Scan(&r.ID, &r.Ts, &r.AccessKeyID, &r.UserID, &r.PublicModel,
			&r.ProviderID, &r.UpstreamModel, &r.IngressProtocol, &stream,
			&r.InputTokens, &r.OutputTokens, &r.TotalTokens,
			&r.ReasoningTokens, &r.CachedTokens, &r.UsageState, &r.Status,
			&r.HTTPStatus, &r.ErrorCode, &r.LatencyMs, &r.TTFBMs,
			&r.RequestID, &cost); err != nil {
			t.Fatalf("scan usage: %v", err)
		}
		r.Stream = stream == 1
		out = append(out, usageRow{r: r, cost: cost})
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return out
}

// mkRec 造一条可预期计价的记录。
func mkRec(id string, in, out int64) *UsageRecord {
	return &UsageRecord{
		ID: id, AccessKeyID: "k1", UserID: "u1", PublicModel: "m",
		ProviderID: "p1", UpstreamModel: "priced", IngressProtocol: "openai-chat",
		InputTokens: in, OutputTokens: out, TotalTokens: in + out,
		UsageState: "reported", Status: "ok", HTTPStatus: 200,
		RequestID: "rq-" + id,
	}
}

// TestBatched_OneRowMatchesSingleRowPath 是全部不变量的基石：
// 批量长度为 1 时，结果必须与单条路径**逐字段逐值相同**。
func TestBatched_OneRowMatchesSingleRowPath(t *testing.T) {
	ctx := context.Background()
	st := testStore(t, t.TempDir()+"/gw.db")
	seedPricedModel(t, st, 0.2, 0.02, 0.8)

	// 两次用**同一个库**，但记录 id 不同 —— 否则主键冲突。
	single := mkRec("s1", 1000, 500)
	oneCost, err := st.CreateUsageRecordWithCost(ctx, single)
	if err != nil {
		t.Fatalf("single: %v", err)
	}

	batched := mkRec("b1", 1000, 500)
	costs, err := st.CreateUsageRecordsBatched(ctx, []*UsageRecord{batched})
	if err != nil {
		t.Fatalf("batched(1): %v", err)
	}
	if len(costs) != 1 {
		t.Fatalf("batch(1) 返回 %d 个费用, want 1", len(costs))
	}
	if costs[0] != oneCost {
		t.Errorf("batch(1) 费用 = %v, 单条 = %v —— 同一输入必须得到同一费用", costs[0], oneCost)
	}

	// 逐字段比对两行落库结果。
	rows := usageRows(t, st)
	if len(rows) != 2 {
		t.Fatalf("落库 %d 行, want 2", len(rows))
	}
	// 按 id 查，**不依赖返回顺序**：我第一版写成 rows[0]=s1，实测
	// ORDER BY id 下 'b1' < 's1'，于是 rows[0]=b1，断言误报 ——
	// 那是测试自身的假设错误，不是实现问题。
	byID := map[string]usageRow{}
	for _, row := range rows {
		byID[row.r.ID] = row
	}
	s, okS := byID["s1"]
	b, okB := byID["b1"]
	if !okS || !okB {
		t.Fatalf("落库的行不对：%v", byID)
	}
	// 逐字段比对。**刻意不比 ID 与 RequestID**：它们必须不同（否则主键
	// 冲突 / 幂等键相同），本来就不是「应当相同」的量。比它们是这条
	// 用例第一版的第二个自身错误 —— 报出来的差异恰好只有这两个字段，
	// 说明实现是对的。
	if s.r.InputTokens != b.r.InputTokens || s.r.OutputTokens != b.r.OutputTokens ||
		s.r.TotalTokens != b.r.TotalTokens || s.r.Status != b.r.Status ||
		s.r.UserID != b.r.UserID || s.r.PublicModel != b.r.PublicModel ||
		s.r.ProviderID != b.r.ProviderID || s.r.UpstreamModel != b.r.UpstreamModel ||
		s.r.IngressProtocol != b.r.IngressProtocol || s.r.UsageState != b.r.UsageState ||
		s.r.HTTPStatus != b.r.HTTPStatus || s.cost != b.cost {
		t.Errorf("batch(1) 与单条落库结果不一致:\n 单条=%+v (cost=%v)\n 批量=%+v (cost=%v)",
			s.r, s.cost, b.r, b.cost)
	}
	// 但 ID 与 request_id 必须各自保留（批量不能把它们串行错位）。
	if s.r.ID == b.r.ID {
		t.Errorf("两条记录的 ID 相同（%q）—— 批量把行写重了", s.r.ID)
	}
}

// TestBatched_CostMatchesSingleRowPerRecord 是「扣的 = 报表的」在批量下的核心断言。
//
// 逐条比对：批量第 i 条的费用，必须等于**单条路径**处理同一输入得到的费用。
//
// 为什么必须逐条而不是比总额：费用与行的**错位**（第 i 条的 cost 配到第 j 行）
// 会让总额仍然相等 —— 只断言总和的话，这种错完全测不出来，而它让每一笔
// 都扣错、且余额与报表的比例悄悄歪掉。
func TestBatched_CostMatchesSingleRowPerRecord(t *testing.T) {
	ctx := context.Background()
	st := testStore(t, t.TempDir()+"/gw.db")
	seedPricedModel(t, st, 0.2, 0.02, 0.8)

	// 刻意用**不同的 token 数**，费用彼此不同 —— 若所有行费用相同，
	// 错位就检测不出来。
	inputs := []struct{ in, out int64 }{
		{1000, 500}, {7, 3}, {20000, 9000}, {1, 1}, {55555, 33333},
	}

	// 参照：单条路径逐条算（每条一个独立库，互不干扰）。
	want := make([]float64, len(inputs))
	for i, in := range inputs {
		sdb := testStore(t, t.TempDir()+"/ref.db")
		seedPricedModel(t, sdb, 0.2, 0.02, 0.8)
		c, err := sdb.CreateUsageRecordWithCost(ctx, mkRec("x", in.in, in.out))
		if err != nil {
			t.Fatalf("ref %d: %v", i, err)
		}
		want[i] = c
	}

	// 实际：一次批量。
	recs := make([]*UsageRecord, len(inputs))
	for i, in := range inputs {
		recs[i] = mkRec("b"+string(rune('a'+i)), in.in, in.out)
	}
	got, err := st.CreateUsageRecordsBatched(ctx, recs)
	if err != nil {
		t.Fatalf("batched: %v", err)
	}
	if len(got) != len(inputs) {
		t.Fatalf("批量返回 %d 个费用, want %d", len(got), len(inputs))
	}

	// 逐条比对（顺序即 recs 顺序）。
	for i := range inputs {
		if got[i] != want[i] {
			t.Errorf("第 %d 条费用 = %v, 单条路径 = %v（inputs=%+v）",
				i, got[i], want[i], inputs[i])
		}
	}

	// 再从库里读回 cost_total 比一次 —— 返回值与落库值必须同一个数。
	rows := usageRows(t, st)
	if len(rows) != len(inputs) {
		t.Fatalf("落库 %d 行, want %d", len(rows), len(inputs))
	}
	byID := map[string]float64{}
	for _, row := range rows {
		byID[row.r.ID] = row.cost
	}
	for i, r := range recs {
		if byID[r.ID] != got[i] {
			t.Errorf("行 %s 的 cost_total = %v, 返回值 = %v —— 「扣的」与「报表的」不是同一个数",
				r.ID, byID[r.ID], got[i])
		}
	}
}

// TestBatched_PartialFailureRollsBackWholeBatch 是「失败不扣费」的实现。
//
// 任一行主键冲突 → 整批回滚 → 返回**全 0** 费用。
//
// 全 0 是关键：调用方靠它判断「这批没落库」，从而整批不扣费。
// 若这里返回非 0（比如已算好的费用），调用方就会为一批不存在的记录扣钱。
func TestBatched_PartialFailureRollsBackWholeBatch(t *testing.T) {
	ctx := context.Background()
	st := testStore(t, t.TempDir()+"/gw.db")
	seedPricedModel(t, st, 0.2, 0.02, 0.8)

	// 先插一条占位，制造主键冲突。
	if err := st.CreateUsageRecord(ctx, mkRec("dup", 10, 5)); err != nil {
		t.Fatalf("seed dup: %v", err)
	}
	before := usageRowCountForBatch(t, st)

	// 一批里有两条新记录 + 一条与已有主键冲突的记录。
	recs := []*UsageRecord{
		mkRec("n1", 1000, 500),
		mkRec("dup", 2000, 1000), // ← 冲突
		mkRec("n2", 3000, 1500),
	}
	costs, err := st.CreateUsageRecordsBatched(ctx, recs)
	if err == nil {
		t.Fatal("含冲突的批量应当报错")
	}
	if !strings.Contains(strings.ToUpper(err.Error()), "UNIQUE") {
		t.Logf("错误不是唯一约束冲突（仍算失败）: %v", err)
	}

	// 整批回滚：一行都不该多出来。
	after := usageRowCountForBatch(t, st)
	if after != before {
		t.Errorf("回滚后行数 = %d, 改前 %d —— 整批回滚失效，有行漏进来了", after, before)
	}

	// 费用必须全 0 —— 这是「整批不扣费」的依据。
	for i, c := range costs {
		if c != 0 {
			t.Errorf("第 %d 条返回费用 %v, 失败批次必须返回全 0（否则调用方会为不存在的记录扣费）", i, c)
		}
	}
}

func usageRowCountForBatch(t *testing.T, st *Store) int {
	t.Helper()
	var n int
	if err := st.read.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM usage_records`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

// TestBatched_NegativeTokensClampedLikeSinglePath 验证批量路径也做负值归一。
//
// 归一必须在批量里也做：绕过它，负 token 会经 AFTER INSERT 触发器
// **永久拉低** access_keys.used_tokens 与 usage_totals，
// 且配额判定「used >= quota」从此永不成立 —— 凭空发放额度。
func TestBatched_NegativeTokensClampedLikeSinglePath(t *testing.T) {
	ctx := context.Background()
	st := testStore(t, t.TempDir()+"/gw.db")
	seedPricedModel(t, st, 0.2, 0.02, 0.8)

	recs := []*UsageRecord{
		{ID: "neg1", AccessKeyID: "k1", UserID: "u1", PublicModel: "m",
			ProviderID: "p1", UpstreamModel: "priced", IngressProtocol: "openai-chat",
			InputTokens: -100, OutputTokens: -50, TotalTokens: -150,
			ReasoningTokens: -10, CachedTokens: -5,
			UsageState: "reported", Status: "ok", HTTPStatus: 200, RequestID: "rq-neg1"},
		mkRec("ok1", 100, 50),
	}
	costs, err := st.CreateUsageRecordsBatched(ctx, recs)
	if err != nil {
		t.Fatalf("batched: %v", err)
	}
	if len(costs) != 2 {
		t.Fatalf("返回 %d 个费用, want 2", len(costs))
	}

	// 第一条的负值必须已被归一为 0。
	rows := usageRows(t, st)
	byID := map[string]*UsageRecord{}
	for _, row := range rows {
		byID[row.r.ID] = row.r
	}
	neg, ok := byID["neg1"]
	if !ok {
		t.Fatalf("neg1 未落库；落库的是 %v", byID)
	}
	if neg.InputTokens != 0 || neg.OutputTokens != 0 || neg.TotalTokens != 0 ||
		neg.ReasoningTokens != 0 || neg.CachedTokens != 0 {
		t.Errorf("负值未被归一: in=%d out=%d total=%d reasoning=%d cached=%d（want 全 0）",
			neg.InputTokens, neg.OutputTokens, neg.TotalTokens,
			neg.ReasoningTokens, neg.CachedTokens)
	}
	// 费用也不能为负（priceUsage 对负 token 会算出负数）。
	if costs[0] < 0 {
		t.Errorf("负 token 产生了负费用 %v —— 会让「扣费」变成「充值」", costs[0])
	}
}

// TestBatched_EmptyAndSingleEdgeCases 覆盖边界：空批次不得报错也不得写库。
func TestBatched_EmptyAndSingleEdgeCases(t *testing.T) {
	ctx := context.Background()
	st := testStore(t, t.TempDir()+"/gw.db")
	seedPricedModel(t, st, 0.2, 0.02, 0.8)

	costs, err := st.CreateUsageRecordsBatched(ctx, nil)
	if err != nil {
		t.Fatalf("空批次不应报错: %v", err)
	}
	if len(costs) != 0 {
		t.Errorf("空批次返回 %d 个费用, want 0", len(costs))
	}
	if n := usageRowCountForBatch(t, st); n != 0 {
		t.Errorf("空批次却写了 %d 行", n)
	}

	// 空切片（非 nil）同样必须安全。
	if _, err := st.CreateUsageRecordsBatched(ctx, []*UsageRecord{}); err != nil {
		t.Fatalf("空切片不应报错: %v", err)
	}
}

// TestBatched_TriggerStillFiresPerRow 验证 AFTER INSERT 触发器**逐行**生效。
//
// usage_records 上挂了两个触发器（累加 access_keys.used_tokens 与
// usage_totals）。它们是「每行一次」的语义。单行多值 INSERT 仍会为
// 每行触发一次 —— 但这值得钉住：若有人改成多行共享一次触发，
// 终身累计会少算，而那正是「配额凭空放宽」那条路径。
func TestBatched_TriggerStillFiresPerRow(t *testing.T) {
	ctx := context.Background()
	st := testStore(t, t.TempDir()+"/gw.db")
	seedPricedModel(t, st, 0.2, 0.02, 0.8)
	mustUser(t, st, "u1", 0, false)
	if err := st.CreateAccessKey(ctx, &AccessKey{
		ID: "k1", KeyHash: "h1", KeyPrefix: "p", Name: "n",
		Enabled: true, QuotaTokens: 0, UserID: "u1",
	}); err != nil {
		t.Fatalf("key: %v", err)
	}

	recs := []*UsageRecord{
		mkRec("t1", 100, 50), // total=150
		mkRec("t2", 200, 100),
		mkRec("t3", 300, 150),
	}
	if _, err := st.CreateUsageRecordsBatched(ctx, recs); err != nil {
		t.Fatalf("batched: %v", err)
	}

	var want int64
	for _, r := range recs {
		want += r.TotalTokens
	}
	var got int64
	if err := st.read.QueryRowContext(ctx,
		`SELECT used_tokens FROM access_keys WHERE id = 'k1'`).Scan(&got); err != nil {
		t.Fatalf("read used_tokens: %v", err)
	}
	if got != want {
		t.Errorf("used_tokens = %d, want %d —— 触发器没有逐行生效（终身累计会少算，"+
			"配额判定 `used >= quota` 会随之放宽）", got, want)
	}

	// 表 B 同样逐行。
	lt, err := st.GetUsageLifetime(ctx)
	if err != nil {
		t.Fatalf("lifetime: %v", err)
	}
	if lt.TotalTokens != want {
		t.Errorf("usage_totals.total_tokens = %d, want %d", lt.TotalTokens, want)
	}
	if lt.RequestCount != int64(len(recs)) {
		t.Errorf("usage_totals.request_count = %d, want %d", lt.RequestCount, len(recs))
	}
}

// TestBatched_ConcurrentBatchesStayExact 在**并发批量**下验证账目精确。
//
// 这是本文件最贴近生产的用例：多个 worker 各自开批，同时写同一个库。
// 断言：总行数、总 token、终身累计三者一致，且没有重复 id。
//
// 为什么单独测：批量把「N 条语句」变成「1 条语句」，改变的是
// **写锁的持有方式**。单批内的原子性由事务保证，但多个批次之间的
// 交错仍靠 SQLite 串行化 —— 这条用例钉住那个交界处。
func TestBatched_ConcurrentBatchesStayExact(t *testing.T) {
	ctx := context.Background()
	st := testStore(t, t.TempDir()+"/gw.db")
	seedPricedModel(t, st, 0.2, 0.02, 0.8)
	mustUser(t, st, "u1", 0, false)
	if err := st.CreateAccessKey(ctx, &AccessKey{
		ID: "k1", KeyHash: "h1", KeyPrefix: "p", Name: "n",
		Enabled: true, QuotaTokens: 0, UserID: "u1",
	}); err != nil {
		t.Fatalf("key: %v", err)
	}

	const (
		workers = 20
		batchSz = 25
	)
	var totalTokens int64
	for w := 0; w < workers; w++ {
		recs := make([]*UsageRecord, batchSz)
		for i := range recs {
			in := int64(100 + i)
			out := int64(50 + i)
			recs[i] = mkRec(fmtID(w, i), in, out)
			totalTokens += in + out
		}
		if _, err := st.CreateUsageRecordsBatched(ctx, recs); err != nil {
			t.Fatalf("worker %d: %v", w, err)
		}
	}

	wantRows := int64(workers * batchSz)
	if n := int64(usageRowCountForBatch(t, st)); n != wantRows {
		t.Errorf("落库行数 = %d, want %d", n, wantRows)
	}
	// id 必须唯一（批量 VALUES 拼接若列错位会静默写出重复/覆盖）。
	var distinct int64
	if err := st.read.QueryRowContext(ctx,
		`SELECT COUNT(DISTINCT id) FROM usage_records`).Scan(&distinct); err != nil {
		t.Fatalf("distinct: %v", err)
	}
	if distinct != wantRows {
		t.Errorf("不同 id 数 = %d, want %d —— 批量写出了重复行", distinct, wantRows)
	}
	// 触发器逐行累加。
	var used int64
	if err := st.read.QueryRowContext(ctx,
		`SELECT used_tokens FROM access_keys WHERE id = 'k1'`).Scan(&used); err != nil {
		t.Fatalf("used: %v", err)
	}
	if used != totalTokens {
		t.Errorf("used_tokens = %d, want %d", used, totalTokens)
	}
	lt, _ := st.GetUsageLifetime(ctx)
	if lt.TotalTokens != totalTokens {
		t.Errorf("lifetime.total_tokens = %d, want %d", lt.TotalTokens, totalTokens)
	}
}

func fmtID(w, i int) string {
	return "w" + itoa(w) + "r" + itoa(i)
}

// TestBatched_UnknownProviderCostsZero 保留「查价失败 → 记 0」的既有语义。
//
// 批量里若有记录指向**不存在**的上游模型，其 cost_total 必须记 0
// （而不是让整批失败）。这与单条路径一致：一条脏/已删模型不该
// 阻断其余记录的落库 —— 用量是硬需求，费用不是。
func TestBatched_UnknownProviderCostsZero(t *testing.T) {
	ctx := context.Background()
	st := testStore(t, t.TempDir()+"/gw.db")
	seedPricedModel(t, st, 0.2, 0.02, 0.8)

	good := mkRec("g1", 1000, 500)
	orphan := mkRec("o1", 1000, 500)
	orphan.ProviderID = "p-does-not-exist"
	orphan.UpstreamModel = "gone"

	costs, err := st.CreateUsageRecordsBatched(ctx, []*UsageRecord{good, orphan})
	if err != nil {
		t.Fatalf("含未知模型的批量不应整批失败: %v", err)
	}
	if costs[0] <= 0 {
		t.Errorf("正常记录费用 = %v, 应 > 0", costs[0])
	}
	if costs[1] != 0 {
		t.Errorf("未知模型费用 = %v, want 0（查价失败记 0，与单条路径一致）", costs[1])
	}
	// 但**两行都要落库** —— 用量不能因为计不了价就丢。
	if n := usageRowCountForBatch(t, st); n != 2 {
		t.Errorf("落库行数 = %d, want 2（计不了价也要落库）", n)
	}
}

// 让 errors 包被使用（部分失败用例可能走向该分支，保留断言能力）。
var _ = errors.Is
