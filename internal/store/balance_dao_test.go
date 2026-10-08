package store

// 用户余额与扣费的回归测试。
//
// 本模块要钉住的不变量（逐条对应下面的用例）：
//   - 「不限额」是 NULL，**不是 0**；0 永远是「真没钱」。看到 0 当成不限
//     会得到一个永远能用的欠费账户。
//   - 扣费金额与 usage_records.cost_total **逐分相等**（同源同值：扣的是
//     CreateUsageRecordWithCost 返回的那个 cost 经 YuanToCents 换算的结果，
//     不重算）。
//   - 幂等：同一 requestID 重复扣只扣一次，且幂等命中**不是错误**。
//   - 绝不扣成负数（含并发）。
//   - 0 元（未配价 / usage missing）完全 no-op：不占位、不动余额。
//   - 老库升级后存量用户是「不限额」而不是「余额 0」。
//
// 并发用例刻意走**真实并发**（goroutine + 同一写池）而不是模拟时序：写池是
// 单连接（SetMaxOpenConns(1)），SQLite 的写锁天然把并发 UPDATE 串行化，这
// 正是「一条语句合并判余额与扣减」能成立的前提。模拟时序测不出这个前提。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// mustUser 造一个带初始余额的用户（unlimited=false 时 cents 生效）。
func mustUser(t *testing.T, st *Store, id string, cents int64, unlimited bool) {
	t.Helper()
	if err := st.CreateUser(context.Background(), &User{
		ID: id, Username: id, BalanceCents: cents, Unlimited: unlimited,
	}); err != nil {
		t.Fatalf("create user %s: %v", id, err)
	}
}

// assertBalance 读回余额三元组并断言。
//
// 第二个返回值是 **limited**（= 该行受余额约束），不是 unlimited —— 名字反着
// 正是 BalanceOf 注释里专门警告的坑，测试里也跟着用正向语义。
func assertBalance(t *testing.T, st *Store, userID string, wantCents int64, wantLimited bool) {
	t.Helper()
	cents, limited, err := st.BalanceOf(context.Background(), userID)
	if err != nil {
		t.Fatalf("BalanceOf(%s): %v", userID, err)
	}
	if cents != wantCents || limited != wantLimited {
		t.Fatalf("balance(%s) = (%d, limited=%v), want (%d, limited=%v)",
			userID, cents, limited, wantCents, wantLimited)
	}
}

// seedPricedModel 造一个 provider + 带价格的模型，供计价用例使用。
func seedPricedModel(t *testing.T, st *Store, pin, phit, pout float64) {
	t.Helper()
	ctx := context.Background()
	if err := st.CreateProvider(ctx, &Provider{
		ID: "p1", Slug: "p1", Name: "P1", Endpoint: "http://x",
		Protocol: "openai-chat", Enabled: true,
	}); err != nil {
		t.Fatalf("provider: %v", err)
	}
	if err := st.CreateUpstreamModel(ctx, &UpstreamModel{
		ID: "m1", ProviderID: "p1", ModelID: "priced", Enabled: true,
		PriceInput: pin, PriceCacheHit: phit, PriceOutput: pout,
	}); err != nil {
		t.Fatalf("model: %v", err)
	}
}

// ---- 1. 「不限额」是 NULL 而非 0 ----

// 两种状态必须可区分，且各自的判定方向相反：
//   - NULL（不限额）→ 预检永远放行；
//   - 0（真没钱）  → 预检拒绝。
//
// 把 0 当成「不限」会造出一个永远能用的欠费账户，这是本模块最贵的错。
func TestBalance_NullMeansUnlimitedZeroMeansBroke(t *testing.T) {
	ctx := context.Background()
	st := testStore(t, t.TempDir()+"/gw.db")
	mustUser(t, st, "rich", 0, true) // NULL
	mustUser(t, st, "broke", 0, false)

	// 不限额：cents 无意义、恒 0，但 limited=false（=不限额）才是判定依据。
	assertBalance(t, st, "rich", 0, false)
	ok, err := st.HasSufficientBalance(ctx, "rich", 999_999_99)
	if err != nil || !ok {
		t.Fatalf("unlimited user must always pass precheck: ok=%v err=%v", ok, err)
	}

	// 余额 0：任何金额都过不了预检。**不可**因为「数值也是 0」就放行。
	assertBalance(t, st, "broke", 0, true)
	ok, err = st.HasSufficientBalance(ctx, "broke", 1)
	if err != nil || ok {
		t.Fatalf("zero balance must fail precheck: ok=%v err=%v", ok, err)
	}

	// 恰好压线：余额 == 金额 → 放行（边界是 >=，不是 >）。
	if err := st.SetBalance(ctx, "broke", 100, false); err != nil {
		t.Fatalf("set balance: %v", err)
	}
	if ok, err := st.HasSufficientBalance(ctx, "broke", 100); err != nil || !ok {
		t.Fatalf("exact-balance precheck must pass: ok=%v err=%v", ok, err)
	}
	if ok, _ := st.HasSufficientBalance(ctx, "broke", 101); ok {
		t.Fatal("over-balance precheck must fail")
	}
}

// 用户不存在必须报 ErrNotFound，**绝不能**回 (0, false, nil) ——
// 那会被热路径读成「不限额放行」，一个 id 拼错就变成无限放行。
func TestBalanceOf_UnknownUserIsErrorNotUnlimited(t *testing.T) {
	st := testStore(t, t.TempDir()+"/gw.db")
	cents, limited, err := st.BalanceOf(context.Background(), "ghost")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	if !limited {
		t.Fatal("unknown user reported as unlimited (limited=false) — a typo'd id would pass every precheck")
	}
	if cents != 0 {
		t.Fatalf("cents = %d, want 0 (conservative fail-closed value)", cents)
	}
	if ok, _ := st.HasSufficientBalance(context.Background(), "ghost", 1); ok {
		t.Fatal("unknown user passed the precheck")
	}
}

// ---- 2. 扣费与 cost_total 口径完全一致 ----

// centsYuan 把「分」写成 ChargeBalance 的入参（元）。
//
// 2026-10-10 起 ChargeBalance 收**元**而不是分：单次费用可能远小于一分，
// 按分取整会直接变 0 而一次都扣不到（见 ChargeBalance 的「余数」一节）。
// 测试里绝大多数场景都是整数分，用这个助手保持可读。
func centsYuan(c int64) float64 { return float64(c) / 100 }

// readRemainder 读某用户当前攒下的不足一分余数（微元）。
func readRemainder(t *testing.T, st *Store, userID string) int64 {
	t.Helper()
	var v int64
	if err := st.read.QueryRowContext(context.Background(),
		`SELECT balance_remainder FROM users WHERE id = ?`, userID).Scan(&v); err != nil {
		t.Fatalf("read remainder: %v", err)
	}
	return v
}

// 本用例是整个模块的核心：**扣掉的分必须等于 cost_total 换算的分**。
//
// 计价口径（priceUsage）：未命中输入 = MAX(input-cached,0)×price_input，
// 命中输入 = cached×price_cache_hit（为 0 回退 price_input），输出 =
// output×price_output，全部 /1e6，再舍入到 1e-9 元。
// 这里 600k×0.2 + 400k×0.02 + 100k×0.8 = 0.208 元 → 21 分（0.208×100=20.8，
// 四舍五入得 21）。
func TestCharge_CostMatchesUsageRecordCostTotal(t *testing.T) {
	ctx := context.Background()
	st := testStore(t, t.TempDir()+"/gw.db")
	seedPricedModel(t, st, 0.2, 0.02, 0.8)
	mustUser(t, st, "u1", 100_00, false) // 100 元 = 10000 分

	cost, err := st.CreateUsageRecordWithCost(ctx, &UsageRecord{
		ID: "r1", UserID: "u1", AccessKeyID: "k1", PublicModel: "priced",
		ProviderID: "p1", UpstreamModel: "priced", IngressProtocol: "openai-chat",
		InputTokens: 1_000_000, CachedTokens: 400_000, OutputTokens: 100_000,
		TotalTokens: 1_100_000, UsageState: "reported", Status: "ok",
		RequestID: "req-1",
	})
	if err != nil {
		t.Fatalf("usage: %v", err)
	}
	if diff := cost - 0.208; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("cost_total = %v, want 0.208", cost)
	}
	// 0.208 元 = 20.8 分。余数机制下**取整方向变成向下取整**（20 分），
	// 零头 0.8 分留在 balance_remainder 里继续攒。
	//
	// 为什么不再四舍五入成 21：余数已经保证「余额减少总量 == 报表费用总量」
	// ——每一分钱最终都会被扣掉，只是可能分几次。若这里再 round 一次向上取整，
	// 就等于凭空多扣用户 0.2 分，而那 0.2 分没有任何消费支撑。
	// （对比：修复前没有余数，取整误差是**永久丢失**的那一种。）
	if err := st.ChargeBalance(ctx, "u1", cost, "req-1"); err != nil {
		t.Fatalf("charge: %v", err)
	}
	assertBalance(t, st, "u1", 100_00-20, true)
	if got := readRemainder(t, st, "u1"); got != 8_000 {
		t.Errorf("余数 = %d 微元, want 8000（0.208 元 = 20.8 分，零头 0.8 分留着）", got)
	}
}

// 幂等：同一 requestID 扣两次只扣一次，且第二次**不是错误**。
// 重试方要的正是「别再扣一次」，回错误只会让调用方把它当失败而重试更多次。
func TestCharge_IdempotentOnSameRequestID(t *testing.T) {
	ctx := context.Background()
	st := testStore(t, t.TempDir()+"/gw.db")
	mustUser(t, st, "u1", 1000, false)

	for i := 0; i < 5; i++ {
		if err := st.ChargeBalance(ctx, "u1", centsYuan(30), "same-req"); err != nil {
			t.Fatalf("charge #%d must be a no-op nil, got %v", i, err)
		}
	}
	assertBalance(t, st, "u1", 1000-30, true)

	// 流水只有一行，且金额是真实扣掉的那笔。
	var n int
	var amount int64
	if err := st.read.QueryRowContext(ctx,
		`SELECT COUNT(*), COALESCE(SUM(amount_cents), 0) FROM balance_charges WHERE request_id = ?`,
		"same-req").Scan(&n, &amount); err != nil {
		t.Fatalf("read charges: %v", err)
	}
	if n != 1 || amount != 30 {
		t.Fatalf("balance_charges rows=%d amount=%d, want 1/30", n, amount)
	}

	// 换一个 requestID 就是**新的一次消费**，必须真的再扣一次。
	if err := st.ChargeBalance(ctx, "u1", centsYuan(30), "other-req"); err != nil {
		t.Fatalf("charge other: %v", err)
	}
	assertBalance(t, st, "u1", 1000-60, true)
}

// 0 元（未配价模型 / usage missing）必须**完全 no-op**：
// 不占位（否则库里堆 0 元流水噪音 + 白占幂等键）、不动余额、不开事务。
func TestCharge_ZeroAmountIsCompleteNoOp(t *testing.T) {
	ctx := context.Background()
	st := testStore(t, t.TempDir()+"/gw.db")
	mustUser(t, st, "u1", 500, false)

	// 负数也当 0（费用没有负的概念，负费用流进来会变成「扣费即充值」）。
	for _, amt := range []int64{0, -1, -100} {
		if err := st.ChargeBalance(ctx, "u1", float64(amt), fmt.Sprintf("zero-%d", amt)); err != nil {
			t.Fatalf("charge(%d) must be no-op nil, got %v", amt, err)
		}
	}
	assertBalance(t, st, "u1", 500, true)

	var n int
	if err := st.read.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM balance_charges`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatalf("balance_charges has %d rows, want 0 (0 元扣费必须完全不占位)", n)
	}

	// 未配价模型 → cost_total 恒 0 → 扣费金额 0 → no-op。这条把「计价」与
	// 「no-op」两条路径接起来：口径 A（usage missing 不扣钱）的落点。
	if err := st.CreateProvider(ctx, &Provider{
		ID: "pf", Slug: "pf", Name: "PF", Endpoint: "http://f",
		Protocol: "openai-chat", Enabled: true,
	}); err != nil {
		t.Fatalf("provider: %v", err)
	}
	if err := st.CreateUpstreamModel(ctx, &UpstreamModel{
		ID: "mf", ProviderID: "pf", ModelID: "free", Enabled: true,
	}); err != nil {
		t.Fatalf("model: %v", err)
	}
	cost, err := st.CreateUsageRecordWithCost(ctx, &UsageRecord{
		ID: "r-free", UserID: "u1", AccessKeyID: "k1", PublicModel: "free",
		ProviderID: "pf", UpstreamModel: "free", IngressProtocol: "openai-chat",
		InputTokens: 1_000_000, OutputTokens: 1_000_000, TotalTokens: 2_000_000,
		UsageState: "reported", Status: "ok", RequestID: "req-free",
	})
	if err != nil {
		t.Fatalf("usage: %v", err)
	}
	if cost != 0 {
		t.Fatalf("unpriced model cost = %v, want 0", cost)
	}
	if err := st.ChargeBalance(ctx, "u1", cost, "req-free"); err != nil {
		t.Fatalf("charge unpriced: %v", err)
	}
	assertBalance(t, st, "u1", 500, true)
}

// ---- 3. 余额不足：拒绝 + 绝不扣成负数 ----

// 余额不足时：**不扣**（保持原值，绝不为负）+ 报 ErrInsufficientBalance。
//
// 刻意不是「尽力扣到 0」：余额是整数分，扣一部分会让「扣掉的金额」与
// cost_total 不等 —— 报表说花了 3 元、余额只少了 0.8 元，正是本模块要杜绝的
// 漂移。余额守在原处 + 明确报欠费，账目才是自洽的。
func TestCharge_InsufficientLeavesBalanceUntouched(t *testing.T) {
	ctx := context.Background()
	st := testStore(t, t.TempDir()+"/gw.db")
	mustUser(t, st, "u1", 50, false) // 0.5 元

	err := st.ChargeBalance(ctx, "u1", centsYuan(300), "req-big")
	if !errors.Is(err, ErrInsufficientBalance) {
		t.Fatalf("err = %v, want ErrInsufficientBalance", err)
	}
	assertBalance(t, st, "u1", 50, true) // 一分没扣，且绝不为负

	// 恰好够时正常扣，扣完归零 —— 归零是**正常终态**，不是欠费。
	if err := st.ChargeBalance(ctx, "u1", centsYuan(50), "req-exact"); err != nil {
		t.Fatalf("exact charge must succeed: %v", err)
	}
	assertBalance(t, st, "u1", 0, true)

	// 归零后再扣必须报欠费，且余额仍为 0（不会变负）。
	if err := st.ChargeBalance(ctx, "u1", centsYuan(1), "req-after"); !errors.Is(err, ErrInsufficientBalance) {
		t.Fatalf("err = %v, want ErrInsufficientBalance", err)
	}
	assertBalance(t, st, "u1", 0, true)
}

// 不限额用户扣费：余额不动、且不报错（余额无限、减一个数没有意义）。
// 关键是**不能**把它 COALESCE 成 0 去「扣 0 元」——那会凭空造出流水。
func TestCharge_UnlimitedUserIsNeverDebited(t *testing.T) {
	ctx := context.Background()
	st := testStore(t, t.TempDir()+"/gw.db")
	mustUser(t, st, "vip", 0, true)

	for i := 0; i < 3; i++ {
		if err := st.ChargeBalance(ctx, "vip", centsYuan(10_000), fmt.Sprintf("vip-%d", i)); err != nil {
			t.Fatalf("unlimited charge must be nil no-op, got %v", err)
		}
	}
	assertBalance(t, st, "vip", 0, false) // 仍是 NULL（limited=false）

	var n int
	if err := st.read.QueryRowContext(ctx, `SELECT COUNT(*) FROM balance_charges`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 3 {
		t.Fatalf("balance_charges rows=%d, want 3 (不限额也留痕，便于排查「为什么没扣」)", n)
	}
}

// 并发扣费**不得超扣**：余额必须 ≥ 0，且总扣款 = 实际成功的次数 × 单价。
//
// 这是「一条 UPDATE 合并判余额与扣减」的立身之本。若实现退化成
// 「先 SELECT 余额、再 UPDATE 扣」，两个 goroutine 会同时读到同一份余额、
// 都判定「够」，然后各自扣一次 —— 余额被扣成负数。
func TestCharge_ConcurrentNeverGoesNegative(t *testing.T) {
	ctx := context.Background()
	st := testStore(t, t.TempDir()+"/gw.db")
	// 余额 100 分，每人扣 30 分 → 最多 3 人成功，4 人及以上必有人欠费。
	mustUser(t, st, "u1", 100, false)

	const workers = 16
	var wg sync.WaitGroup
	var mu sync.Mutex
	// 用 int64：余额单位是整数分（int64），计数与之同类型才能直接比。
	var okCount, insufficient int64

	start := make(chan struct{})
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start // 最大化并发重叠，让竞态真的有机会发生
			err := st.ChargeBalance(ctx, "u1", centsYuan(30), fmt.Sprintf("conc-%d", i))
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				okCount++
			case errors.Is(err, ErrInsufficientBalance):
				insufficient++
			default:
				t.Errorf("unexpected error: %v", err)
			}
		}(i)
	}
	close(start)
	wg.Wait()

	cents, limited, err := st.BalanceOf(ctx, "u1")
	if err != nil || !limited {
		t.Fatalf("read balance: %v limited=%v", err, limited)
	}
	if cents < 0 {
		t.Fatalf("balance went NEGATIVE: %d — 并发扣费超扣了", cents)
	}
	// 成功的每一次都真扣了 30 分：余额 + 已扣 = 初始值，无遗漏无重复。
	if cents != 100-30*okCount {
		t.Fatalf("balance = %d, want %d (ok=%d, insufficient=%d)",
			cents, 100-30*okCount, okCount, insufficient)
	}
	// 100/30 ⇒ 最多 3 次成功、13 次欠费。多一次成功就是超扣。
	if okCount != 3 {
		t.Fatalf("charged %d times, want exactly 3 (balance allows only 3 × 30 ≤ 100)", okCount)
	}
	if insufficient != int64(workers-3) {
		t.Fatalf("insufficient = %d, want %d", insufficient, workers-3)
	}
}

// 并发重试同一 requestID：只扣一次（这是幂等与并发的交叉点 —— 最危险的
// 组合：两个 goroutine 同时用同一个幂等键）。
func TestCharge_ConcurrentSameRequestIDChargesOnce(t *testing.T) {
	ctx := context.Background()
	st := testStore(t, t.TempDir()+"/gw.db")
	mustUser(t, st, "u1", 1000, false)

	const workers = 8
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if err := st.ChargeBalance(ctx, "u1", centsYuan(100), "retry-key"); err != nil {
				t.Errorf("idempotent charge must be nil, got %v", err)
			}
		}()
	}
	close(start)
	wg.Wait()

	assertBalance(t, st, "u1", 1000-100, true)
	var n int
	if err := st.read.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM balance_charges WHERE request_id='retry-key'`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Fatalf("balance_charges rows=%d, want 1（8 个并发重试只占位一次）", n)
	}
}

// ---- 4. 充值 ----

// AdjustBalance 用 SQL 相对自增：两个并发充值都生效，不会后写覆盖先写。
// 读-改-写的写法在这里会丢掉一次充值，而充值丢钱是用户直接可见的故障。
func TestAdjustBalance_ConcurrentTopUpsDoNotLoseUpdates(t *testing.T) {
	ctx := context.Background()
	st := testStore(t, t.TempDir()+"/gw.db")
	mustUser(t, st, "u1", 0, false)

	const workers = 10
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if err := st.AdjustBalance(ctx, "u1", 100); err != nil {
				t.Errorf("adjust: %v", err)
			}
		}()
	}
	close(start)
	wg.Wait()

	assertBalance(t, st, "u1", 100*workers, true)
}

// 扣减到负数被夹在 0：负余额会让「余额不足」判定恒真，用户被永久锁死。
func TestAdjustBalance_ClampsAtZero(t *testing.T) {
	ctx := context.Background()
	st := testStore(t, t.TempDir()+"/gw.db")
	mustUser(t, st, "u1", 30, false)

	if err := st.AdjustBalance(ctx, "u1", -100); err != nil {
		t.Fatalf("adjust: %v", err)
	}
	assertBalance(t, st, "u1", 0, true)
}

// SetBalance 可在「不限额」与「有限额」之间来回切换，且必须真的落 NULL。
func TestSetBalance_TogglesUnlimited(t *testing.T) {
	ctx := context.Background()
	st := testStore(t, t.TempDir()+"/gw.db")
	mustUser(t, st, "u1", 0, true)

	if err := st.SetBalance(ctx, "u1", 500, false); err != nil {
		t.Fatalf("set limited: %v", err)
	}
	assertBalance(t, st, "u1", 500, true)

	if err := st.SetBalance(ctx, "u1", 0, true); err != nil {
		t.Fatalf("set unlimited: %v", err)
	}
	// 关键：库里必须是**真 NULL**，不是 0。读出来 limited=false 才算对。
	var raw sql.NullInt64
	if err := st.read.QueryRowContext(ctx,
		`SELECT balance_cents FROM users WHERE id='u1'`).Scan(&raw); err != nil {
		t.Fatalf("raw read: %v", err)
	}
	if raw.Valid {
		t.Fatalf("balance_cents = %v, want SQL NULL for 不限额（0 会被读成「真没钱」）", raw.Int64)
	}
	assertBalance(t, st, "u1", 0, false)

	if err := st.SetBalance(ctx, "ghost", 100, false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown user: err = %v, want ErrNotFound", err)
	}
}

// ---- 5. 迁移 ----

// 老库（没有 balance_cents 列）升级后必须拿到该列，且**存量用户是
// 「不限额」（NULL）而不是「余额 0」**。
//
// 这是加列时最容易被写错的一步：`ADD COLUMN balance_cents INTEGER NOT NULL
// DEFAULT 0` 会让所有存量用户一夜之间变成「一分钱没有」，全部被 402 挡在
// 门外 —— 一次升级造成全站不可用。故迁移刻意**不带 DEFAULT**。
func TestMigration_BalanceColumnDefaultsExistingUsersToUnlimited(t *testing.T) {
	path := t.TempDir() + "/legacy.db"
	ctx := context.Background()

	first := testStore(t, path)
	mustUser(t, first, "old", 0, true)
	mustUser(t, first, "fresh", 100, false)
	if err := first.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// 摘掉列，模拟「该列落地之前的库」。
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	if _, err := raw.Exec(`CREATE TABLE tmp_users AS SELECT id, username, display_name,
		password_hash, role, status, quota_tokens, used_tokens, auth_version, remark,
		created_at, updated_at, last_login_at, group_id FROM users`); err != nil {
		t.Fatalf("stage copy: %v", err)
	}
	if _, err := raw.Exec(`DROP TABLE users`); err != nil {
		t.Fatalf("drop old table: %v", err)
	}
	if _, err := raw.Exec(`ALTER TABLE tmp_users RENAME TO users`); err != nil {
		t.Fatalf("swap table: %v", err)
	}
	_ = raw.Close()

	second := testStore(t, path)
	if has, err := second.columnExists("users", "balance_cents"); err != nil || !has {
		t.Fatalf("balance_cents must be restored on upgrade: has=%v err=%v", has, err)
	}

	// 存量行必须是 NULL（不限额），不是 0。
	var v sql.NullInt64
	if err := second.read.QueryRowContext(ctx,
		`SELECT balance_cents FROM users WHERE id='old'`).Scan(&v); err != nil {
		t.Fatalf("read migrated balance: %v", err)
	}
	if v.Valid {
		t.Fatalf("migrated user balance = %d, want NULL（存量用户升级后应是不限额）", v.Int64)
	}
	if ok, err := second.HasSufficientBalance(ctx, "old", 999_999_99); err != nil || !ok {
		t.Fatalf("migrated unlimited user must pass precheck: ok=%v err=%v", ok, err)
	}
}

// userColumns 与 scanUser 必须逐列同步（列清单漂移的典型后果是**静默赋错值**：
// 少一列时后面的列会扫进前面的字段，编译与测试全绿，账目在暗处对不上）。
// 本用例对每个字段填入可区分的值，任何错位都会当场暴露。
func TestUserColumns_NoSilentColumnShift(t *testing.T) {
	ctx := context.Background()
	st := testStore(t, t.TempDir()+"/gw.db")

	if err := st.CreateUser(ctx, &User{
		ID: "u1", Username: "alice", DisplayName: "Alice", PasswordHash: "h",
		Role: RoleAdmin, Status: UserStatusDisabled, QuotaTokens: 111, UsedTokens: 222,
		AuthVersion: 333, Remark: "note", BalanceCents: 444, Unlimited: false,
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	u, err := st.GetUser(ctx, "u1")
	if err != nil || u == nil {
		t.Fatalf("get: %v %v", u, err)
	}
	for _, c := range []struct {
		name string
		got  any
		want any
	}{
		{"username", u.Username, "alice"},
		{"display_name", u.DisplayName, "Alice"},
		{"role", u.Role, RoleAdmin},
		{"status", u.Status, UserStatusDisabled},
		{"quota_tokens", u.QuotaTokens, int64(111)},
		{"used_tokens", u.UsedTokens, int64(222)},
		{"auth_version", u.AuthVersion, int64(333)},
		{"remark", u.Remark, "note"},
		{"balance_cents", u.BalanceCents, int64(444)},
		{"unlimited", u.Unlimited, false},
	} {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v（列清单与 scanUser 错位）", c.name, c.got, c.want)
		}
	}
}

// UpdateUser 绝不写 balance_cents：余额只能经 Set/Adjust/Charge 变，
// 否则一个改昵称的 PATCH 就能把用户余额改回 0（= 欠费）。
func TestUpdateUser_DoesNotTouchBalance(t *testing.T) {
	ctx := context.Background()
	st := testStore(t, t.TempDir()+"/gw.db")
	mustUser(t, st, "u1", 500, false)

	snap, err := st.GetUser(ctx, "u1")
	if err != nil || snap == nil {
		t.Fatalf("get: %v", err)
	}
	snap.DisplayName = "Alice Renamed"
	snap.BalanceCents = 0 // 恶意/错误的尝试
	if err := st.UpdateUser(ctx, snap); err != nil {
		t.Fatalf("update: %v", err)
	}
	assertBalance(t, st, "u1", 500, true)
}

// ---- 6. YuanToCents ----

func TestYuanToCents(t *testing.T) {
	cases := []struct {
		yuan float64
		want int64
		why  string
	}{
		{0, 0, "0 元"},
		{0.001, 0, "不足半分 → 0"},
		{0.005, 1, "恰好半分 → 进位（四舍五入，不是 banker's rounding）"},
		{0.014, 1, "1.4 分 → 1"},
		{0.015, 2, "1.5 分 → 2"},
		{0.208, 21, "20.8 分 → 21（截断会少收）"},
		{1, 100, "1 元 = 100 分"},
		{1234.567, 123457, "大额仍精确"},
		{-1, 0, "负数夹到 0（负费用 = 扣费即充值，必须挡住）"},
	}
	for _, c := range cases {
		if got := YuanToCents(c.yuan); got != c.want {
			t.Errorf("YuanToCents(%v) = %d, want %d — %s", c.yuan, got, c.want, c.why)
		}
	}
}

// priceUsage 是全仓唯一的单价公式；这里钉住它的几条口径，防止有人为了
// 「优化」改坏回退规则（命中价回退输入价、负数夹 0、1e-9 舍入）。
func TestPriceUsage_SharedPricingRules(t *testing.T) {
	cases := []struct {
		name            string
		pin, phit, pout float64
		in, cached, out float64
		want            float64
	}{
		{"未命中输入按输入价", 0.2, 0.02, 0.8, 1_000_000, 0, 0, 0.2},
		// 0.6M×0.2 + 0.4M×0.02 = 0.12 + 0.008 = 0.128
		{"命中价生效", 0.2, 0.02, 0.8, 1_000_000, 400_000, 0, 0.128},
		{"命中价未配置 → 回退输入价（不白送）", 0.5, 0, 0, 1_000_000, 1_000_000, 0, 0.5},
		{"输出按输出价", 0.2, 0.02, 0.8, 0, 0, 100_000, 0.08},
		// input=100 < cached=500 ⇒ 未命中夹 0，只按 cached×0.02 计：
		// 500×0.02 = 10，/1e6 = 1e-5 元
		{"cached > input → 未命中夹 0（不产生负费用）", 0.2, 0.02, 0.8, 100, 500, 0, 0.00001},
		{"未配价全 0", 0, 0, 0, 1_000_000, 0, 1_000_000, 0},
		{"舍入到 1e-9（无长尾）", 0.003, 0, 0, 1_000_000, 0, 0, 0.003},
	}
	for _, c := range cases {
		got := priceUsage(c.pin, c.phit, c.pout, c.in, c.cached, c.out)
		if diff := got - c.want; diff > 1e-9 || diff < -1e-9 {
			t.Errorf("%s: priceUsage = %v, want %v", c.name, got, c.want)
		}
	}
}

// CreateUsageRecord 必须仍是 CreateUsageRecordWithCost 的等价包装：
// 26 处既有调用点靠这个不回归，且落库结果与费用口径完全一致。
func TestCreateUsageRecord_KeepsOldSignatureAndSameCost(t *testing.T) {
	ctx := context.Background()
	st := testStore(t, t.TempDir()+"/gw.db")
	seedPricedModel(t, st, 0.2, 0.02, 0.8)

	rec := func(id string) *UsageRecord {
		return &UsageRecord{
			ID: id, UserID: "u1", AccessKeyID: "k1", PublicModel: "priced",
			ProviderID: "p1", UpstreamModel: "priced", IngressProtocol: "openai-chat",
			InputTokens: 1_000_000, CachedTokens: 400_000, OutputTokens: 100_000,
			TotalTokens: 1_100_000, UsageState: "reported", Status: "ok", RequestID: id,
		}
	}
	if err := st.CreateUsageRecord(ctx, rec("a")); err != nil {
		t.Fatalf("old signature: %v", err)
	}
	cost, err := st.CreateUsageRecordWithCost(ctx, rec("b"))
	if err != nil {
		t.Fatalf("new signature: %v", err)
	}

	// 两条记录的 cost_total 必须与返回值一致（不能是「返回一个新算的值」）。
	var a, b float64
	if err := st.read.QueryRowContext(ctx,
		`SELECT cost_total FROM usage_records WHERE id='a'`).Scan(&a); err != nil {
		t.Fatalf("read a: %v", err)
	}
	if err := st.read.QueryRowContext(ctx,
		`SELECT cost_total FROM usage_records WHERE id='b'`).Scan(&b); err != nil {
		t.Fatalf("read b: %v", err)
	}
	if a != b || b != cost {
		t.Fatalf("cost drift: a=%v b=%v returned=%v（返回的必须是写进列里的那个值）", a, b, cost)
	}
}

// 计价查价必须走**读池**。
//
// 写池是单连接（SetMaxOpenConns(1)）且被用量 INSERT、余额扣费、管理写全部
// 排队。若 freezeUsageCost 的查价走写池，每条用量落库就多抢一次那唯一连接，
// 高负载下会与配额/余额预检直接抢锁。本用例占住写池后 freezeUsageCost 仍须
// 返回正确结果 —— 这只能发生在它走读池时。
//
// 刻意**不**在这里落库：CreateUsageRecord 本身就要写池，占住写池时它必然
// 阻塞，那测的是「INSERT 会不会等锁」而不是「查价走哪个池」。
func TestCostLookup_UsesReadPool(t *testing.T) {
	ctx := context.Background()
	st := testStore(t, t.TempDir()+"/gw.db")
	seedPricedModel(t, st, 0.2, 0.02, 0.8)

	// 占住写池的那唯一一条连接。
	tx, err := st.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE users SET remark = remark WHERE 0`); err != nil {
		t.Fatalf("hold write pool: %v", err)
	}

	done := make(chan float64, 1)
	go func() {
		done <- st.freezeUsageCost(ctx, &UsageRecord{
			ProviderID: "p1", UpstreamModel: "priced",
			InputTokens: 1_000_000, TotalTokens: 1_000_000,
		})
	}()

	select {
	case cost := <-done:
		if diff := cost - 0.2; diff > 1e-9 || diff < -1e-9 {
			t.Fatalf("cost = %v, want 0.2（1M 未命中输入 × 0.2 元/百万）", cost)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("查价在写池被占时卡住 —— freezeUsageCost 必须走读池（s.read）")
	}
}
