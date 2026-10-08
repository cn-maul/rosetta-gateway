package store

// 并发压测与账目完整性验证 —— 余额扣费（场景 A / B / C / F）。
//
// # 这个文件与 balance_dao_test.go 的分工
//
// balance_dao_test.go 是**功能回归**：逐个字段、逐条分支地钉住语义。
// 本文件是**压力与不变量验证**：用真实并发把「钱不会算错」这条全局性质
// 压到极限，并在压力下做**精确等值**断言（不是「差不多」）。
//
// # 为什么必须是精确等值
//
// 钱和账目是这个项目的核心正确性。一次静默的漏扣或重复扣都比一个功能 bug
// 严重：漏扣是收入凭空少了一笔且无人知道；重复扣是用户被多收钱且会投诉。
// 所以本文件的断言一律用整数分/微元比较，**绝不用浮点近似**
// （`math.Abs(a-b) < 0.01` 这类断言会把「差一分」这种 P0 级线索吞掉）。
//
// # 并发测试为什么不能只跑一次
//
// 并发 bug 的触发依赖交错，单次运行很可能恰好走的是安全路径。所以：
//   - 本文件里的并发用例都用 `-count=20` 重复跑过（见 AUDIT/test-billing.md）；
//   - 并发用例内部也刻意用 `sync.WaitGroup` + 显式 start 门闩放大交错，
//     而不是串行调用后假装并发。
//
// # 环境说明：-race
//
// 本机 `go test -race` 默认因 MinGW 路径带空格而失败（既有环境问题）。
// 可用无空格路径的 CC 绕过：
//
//	CGO_ENABLED=1 CC=C:/mingw64/bin/gcc.exe go test -race ./internal/store/
//
// 已用阳性对照验证 race 运行时确实生效（故意制造的数据竞争被报告）。
// 详见 AUDIT/test-billing.md 的「-race 环境」一节。

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// discardLogger 给需要绕过 testStore（例如**故意不 Close**）的用例用。
//
// testStore 会注册 t.Cleanup(st.Close)，而 F1 要模拟「进程被杀、没有优雅
// 关闭」，不能走那条路径。日志同样丢弃：并发压测会打大量 WARN，
// 输出淹没测试结果反而看不见失败证据。
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// ---------------------------------------------------------------------------
// 共用助手
// ---------------------------------------------------------------------------

// chargeOutcome 是一次并发扣费的结果分类。
//
// 刻意区分这三类而不是只记「成功/失败」：`ErrInsufficientBalance` 是
// **预期的业务拒绝**，而其它 error 是**异常**。把两者混成一个 error
// 计数器会让「并发下出现数据库错误」被当成「余额不足」而漏掉。
type chargeOutcome struct {
	ok           int64 // 成功扣费
	insufficient int64 // 余额不足（预期内的拒绝）
	otherErr     int64 // 其它错误（异常，必须为 0）
	errs         []error
	mu           sync.Mutex
}

func (o *chargeOutcome) record(err error) {
	switch {
	case err == nil:
		atomic.AddInt64(&o.ok, 1)
	case errors.Is(err, ErrInsufficientBalance):
		atomic.AddInt64(&o.insufficient, 1)
	default:
		atomic.AddInt64(&o.otherErr, 1)
		o.mu.Lock()
		if len(o.errs) < 5 { // 只留前几条，避免刷屏
			o.errs = append(o.errs, err)
		}
		o.mu.Unlock()
	}
}

// runConcurrent 启动 n 个 goroutine 执行 fn，用门闩让它们尽量同时开跑。
//
// 为什么要门闩（start channel）：直接 `go fn()` 时第一个 goroutine 可能已经
// 跑完了，后面才被调度 —— 那就退化成了串行测试，什么都压不出来。
func runConcurrent(n int, fn func(i int)) {
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start // 门闩：等所有 goroutine 就位
			fn(i)
		}(i)
	}
	close(start)
	wg.Wait()
}

// sumChargeCents 汇总 balance_charges 里某个用户累计扣掉的分数。
//
// 这是「账目」侧的权威数字：余额的变化必须与它严格一致。
func sumChargeCents(t *testing.T, st *Store, userID string) int64 {
	t.Helper()
	var v int64
	if err := st.read.QueryRowContext(context.Background(),
		`SELECT COALESCE(SUM(amount_cents), 0) FROM balance_charges WHERE user_id = ?`,
		userID).Scan(&v); err != nil {
		t.Fatalf("sum balance_charges(user=%s): %v", userID, err)
	}
	return v
}

// countChargeRows 统计某个用户在 balance_charges 里的流水行数。
func countChargeRows(t *testing.T, st *Store, userID string) int64 {
	t.Helper()
	var v int64
	if err := st.read.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM balance_charges WHERE user_id = ?`, userID).Scan(&v); err != nil {
		t.Fatalf("count balance_charges(user=%s): %v", userID, err)
	}
	return v
}

// balanceCentsOf 读余额分（断言 limited=true 的用法见 assertBalance）。
func balanceCentsOf(t *testing.T, st *Store, userID string) int64 {
	t.Helper()
	cents, limited, err := st.BalanceOf(context.Background(), userID)
	if err != nil {
		t.Fatalf("BalanceOf(%s): %v", userID, err)
	}
	if !limited {
		t.Fatalf("BalanceOf(%s) 报不限额，但本用例期望一个有限额账户", userID)
	}
	return cents
}

// ---------------------------------------------------------------------------
// A. 并发扣费
// ---------------------------------------------------------------------------

// A1：N 个 goroutine 同扣一个用户，**总额必须精确**。
//
// 构造：余额给足（远超 N 次扣费总额），N 个 goroutine 各扣 1 分且 request_id
// 互不相同。此时不应出现任何 ErrInsufficientBalance —— 每一个都该成功。
//
// 断言（三条，全部精确等值）：
//  1. 成功次数 == N；
//  2. 余额 == 初始 - N 分；
//  3. 流水累计 == N 分。
//
// 这三条必须**同时**成立。只查余额会漏掉「流水写错但余额对」；
// 只查流水会漏掉「流水对但余额扣错」。
func TestStress_ConcurrentCharge_TotalIsExact(t *testing.T) {
	ctx := context.Background()
	st := testStore(t, t.TempDir()+"/gw.db")

	const (
		n         = 200
		eachCents = 1
		initial   = 100_000 // 1000 元，远超 200 分
	)
	mustUser(t, st, "u1", initial, false)

	var out chargeOutcome
	runConcurrent(n, func(i int) {
		out.record(st.ChargeBalance(ctx, "u1", centsYuan(eachCents), fmt.Sprintf("req-%d", i)))
	})

	if out.otherErr != 0 {
		t.Fatalf("出现 %d 次非预期错误（并发下不该有 DB 错误）: %v", out.otherErr, out.errs)
	}
	if out.ok != n {
		t.Fatalf("成功 %d 次, want %d（余额充足，不该有拒绝）", out.ok, n)
	}
	if out.insufficient != 0 {
		t.Fatalf("余额充足却出现 %d 次 ErrInsufficientBalance —— 并发下丢了余额可见性", out.insufficient)
	}

	wantBalance := int64(initial - n*eachCents)
	gotBalance := balanceCentsOf(t, st, "u1")
	if gotBalance != wantBalance {
		t.Errorf("余额 = %d 分, want %d（差 %d 分）", gotBalance, wantBalance, gotBalance-wantBalance)
	}

	wantCharged := int64(n * eachCents)
	gotCharged := sumChargeCents(t, st, "u1")
	if gotCharged != wantCharged {
		t.Errorf("流水累计 = %d 分, want %d（账目与余额必须同源）", gotCharged, wantCharged)
	}

	// 账目恒等式：初始 - 累计扣费 == 当前余额。这一条把「余额」与「流水」
	// 两个独立事实绑在一起，任一侧算错都会让它不成立。
	if initial-gotCharged != gotBalance {
		t.Errorf("账目不闭合: %d - %d = %d, 但余额是 %d",
			initial, gotCharged, initial-gotCharged, gotBalance)
	}
	if rows := countChargeRows(t, st, "u1"); rows != n {
		t.Errorf("流水行数 = %d, want %d（每次扣费恰一行）", rows, n)
	}
}

// A2：余额**刚好够 M 次** → 恰好 M 次成功，其余 ErrInsufficientBalance。
//
// 这是「不得超扣」最锋利的形态：M 与 N 有明确差值，任何一次超扣都会
// 让成功次数 > M，任何一次漏算都会让 < M。断言必须是**恰好相等**，
// 不能写成 `<= M`。
//
// 构造：余额 = M 分，N=M*3 个 goroutine 各扣 1 分。
func TestStress_ConcurrentCharge_ExactlyMBecauseBalanceIsM(t *testing.T) {
	ctx := context.Background()
	st := testStore(t, t.TempDir()+"/gw.db")

	const (
		m       = 50 // 余额恰好 50 分
		n       = m * 3
		initial = m
	)
	mustUser(t, st, "u1", initial, false)

	var out chargeOutcome
	runConcurrent(n, func(i int) {
		out.record(st.ChargeBalance(ctx, "u1", centsYuan(1), fmt.Sprintf("req-%d", i)))
	})

	if out.otherErr != 0 {
		t.Fatalf("出现 %d 次非预期错误: %v", out.otherErr, out.errs)
	}

	// **恰好** M 次成功 —— 不许是 49（漏收）也不许是 51（超扣）。
	if out.ok != m {
		t.Errorf("成功 %d 次, want **恰好** %d 次", out.ok, m)
	}
	if out.ok+out.insufficient != n {
		t.Errorf("成功+拒绝 = %d, want %d（有请求既没成功也没被明确拒绝）",
			out.ok+out.insufficient, n)
	}

	// 余额必须**正好归零**（不能为负，也不能剩下）。
	got := balanceCentsOf(t, st, "u1")
	if got != 0 {
		t.Errorf("余额 = %d 分, want 0（M 次各扣 1 分，恰好扣完）", got)
	}
	if got < 0 {
		t.Errorf("余额为负（%d）—— 严重的资金正确性缺陷", got)
	}

	// 流水累计必须等于真正扣掉的钱（= M 分，不是 N 分）。
	if charged := sumChargeCents(t, st, "u1"); charged != m {
		t.Errorf("流水累计 = %d 分, want %d", charged, m)
	}
	// 被拒绝的请求**不该留下扣了钱的流水行**。它们可能因「先占位」留下
	// 一行 amount_cents=0（设计如此：占位与扣费同事务，失败会回滚 —— 见
	// ChargeBalance 的 P0 修复注释），但**绝不能**留下非 0 金额。
	var bogus int64
	if err := st.read.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM balance_charges WHERE user_id = ? AND amount_cents > 0`,
		"u1").Scan(&bogus); err != nil {
		t.Fatalf("count charged rows: %v", err)
	}
	if bogus != m {
		t.Errorf("amount_cents>0 的流水行数 = %d, want %d（恰好 M 行真的扣了钱）", bogus, m)
	}
}

// A3：并发扣费**绝不为负**，且大额与小额的混合下总额精确。
//
// 与 A2 的区别：这里扣费金额**不整齐**（1/2/3 分混排），用于暴露
// 「按次数估算」类的错误假设 —— 正确的实现只关心金额，不关心次数。
//
// 构造：余额 = 100 分；200 个 goroutine 轮流扣 1/2/3 分。
// 期望：成功者金额之和 <= 100，且余额 == 100 - 成功金额之和，且 >= 0。
func TestStress_ConcurrentCharge_MixedAmountsStayExactAndNonNegative(t *testing.T) {
	ctx := context.Background()
	st := testStore(t, t.TempDir()+"/gw.db")

	const (
		n       = 200
		initial = 100
	)
	mustUser(t, st, "u1", initial, false)

	amounts := []int64{1, 2, 3}

	var (
		out     chargeOutcome
		mu      sync.Mutex
		okCents int64
	)
	runConcurrent(n, func(i int) {
		c := amounts[i%len(amounts)]
		err := st.ChargeBalance(ctx, "u1", centsYuan(c), fmt.Sprintf("mix-%d", i))
		out.record(err)
		if err == nil {
			mu.Lock()
			okCents += c
			mu.Unlock()
		}
	})

	if out.otherErr != 0 {
		t.Fatalf("出现 %d 次非预期错误: %v", out.otherErr, out.errs)
	}

	got := balanceCentsOf(t, st, "u1")
	if got < 0 {
		t.Fatalf("余额为负（%d）—— 严重缺陷", got)
	}
	// 三方差闭环：初始 - 成功金额之和 == 余额 == 初始 - 流水累计。
	if initial-okCents != got {
		t.Errorf("初始(%d) - 成功金额(%d) = %d, 但余额 = %d",
			initial, okCents, initial-okCents, got)
	}
	if charged := sumChargeCents(t, st, "u1"); charged != initial-got {
		t.Errorf("流水累计 = %d, want %d（= 初始 - 余额）", charged, initial-got)
	}
	// 剩余余额必须**小于任何一次扣费的最小金额**，否则说明还有请求本该成功
	// 却被拒了（漏收）。注意：这条依赖「金额 >= 1 分」，故最小金额是 1。
	if got >= 1 && out.insufficient > 0 {
		// 只报告不失败：混合金额下无法保证「恰好扣到 < 1 分」，
		// 因为一次 3 分的请求需要 3 分余量，可能 1 分余量时被拒而留下 1 分。
		t.Logf("提示: 余额残留 %d 分且有 %d 次拒绝（混合金额下属正常，非缺陷）",
			got, out.insufficient)
	}
}

// ---------------------------------------------------------------------------
// B. 余数机制精确性
// ---------------------------------------------------------------------------

// B1：大量「不足一分」的小额扣费累加，余额扣减必须**精确**等于总费用。
//
// 用 0.0001 元（= 100 微元 = 0.01 分）这一档：单次远不足一分，
// 必须靠余数攒。10000 次 = 1 元 = 100 分，是个**整数**结果，
// 所以任何精度损失都会让它不精确 —— 这正是要抓的。
//
// 顺带覆盖「跨过一分时正确清零」：余数在过程中必然会多次跨越 10000。
func TestStress_RemainderAccumulationIsExact(t *testing.T) {
	ctx := context.Background()
	st := testStore(t, t.TempDir()+"/gw.db")

	const (
		n         = 10_000
		eachYuan  = 0.0001 // 100 微元
		initial   = 10_000 // 100 元
		wantCents = int64(n * 100 / MicrosPerCent)
	)
	mustUser(t, st, "u1", initial, false)

	for i := 0; i < n; i++ {
		if err := st.ChargeBalance(ctx, "u1", eachYuan, fmt.Sprintf("tiny-%d", i)); err != nil {
			t.Fatalf("第 %d 次小额扣费失败: %v", i, err)
		}
	}

	got := balanceCentsOf(t, st, "u1")
	want := int64(initial) - wantCents
	if got != want {
		t.Errorf("余额 = %d 分, want %d（差 %d 分）—— 余数机制丢/多了钱",
			got, want, got-want)
	}
	if rem := readRemainder(t, st, "u1"); rem != 0 {
		t.Errorf("余数 = %d 微元, want 0（总数是整除的，不该有残留）", rem)
	}
	if charged := sumChargeCents(t, st, "u1"); charged != wantCents {
		t.Errorf("流水累计 = %d 分, want %d", charged, wantCents)
	}
}

// B2：极小金额（低于半微元）不能被抹成 0。
//
// 0.0000001 元 = 0.1 微元。若实现里写成 `int64(yuan*1e6)` 就得到 0，
// 这笔费用凭空消失 —— 而它是真的消费。YuanToMicros 有专门的兜底
// （`micros < 1` 时返回 1），这条用例钉住它。
//
// 断言：N 次 0.0000001 元 = N 微元。取 N = 10000 → 恰好 1 分。
func TestStress_TinyAmountsAreNotLost(t *testing.T) {
	ctx := context.Background()
	st := testStore(t, t.TempDir()+"/gw.db")

	const (
		n         = 10_000
		eachYuan  = 0.0000001 // 0.1 微元
		initial   = 1_000
		wantCents = int64(1) // 10000 次 × 1 微元 = 10000 微元 = 1 分
	)
	mustUser(t, st, "u1", initial, false)

	for i := 0; i < n; i++ {
		if err := st.ChargeBalance(ctx, "u1", eachYuan, fmt.Sprintf("sub-%d", i)); err != nil {
			t.Fatalf("第 %d 次亚微元扣费失败: %v", i, err)
		}
	}

	if got := balanceCentsOf(t, st, "u1"); got != int64(initial)-wantCents {
		t.Errorf("余额 = %d 分, want %d —— 亚微元费用被丢弃了",
			got, int64(initial)-wantCents)
	}
	if rem := readRemainder(t, st, "u1"); rem != 0 {
		t.Errorf("余数 = %d 微元, want 0", rem)
	}
}

// B3：**并发下余数不丢** —— N 个 goroutine 各扣极小金额，最终余数与扣减精确。
//
// 这是余数机制最危险的地方：`balance_remainder = balance_remainder + ?`
// 若被实现成「SELECT 再 UPDATE」，并发就会互相覆盖（丢失更新），
// 表现为**少收钱**。实现用 UPDATE...RETURNING 单语句回避了它，本用例验证。
//
// 构造：N=200 个并发，每个扣 0.0003 元（300 微元）。总计 60000 微元 = 6 分。
// 断言：余额恰好少 6 分，余数恰好 0。
func TestStress_ConcurrentRemainder_NoLostUpdates(t *testing.T) {
	ctx := context.Background()
	st := testStore(t, t.TempDir()+"/gw.db")

	const (
		n        = 200
		eachYuan = 0.0003 // 300 微元
		initial  = 10_000
	)
	mustUser(t, st, "u1", initial, false)

	wantMicros := int64(n) * 300
	wantCents := wantMicros / MicrosPerCent
	wantRem := wantMicros % MicrosPerCent

	var out chargeOutcome
	runConcurrent(n, func(i int) {
		out.record(st.ChargeBalance(ctx, "u1", eachYuan, fmt.Sprintf("conc-rem-%d", i)))
	})

	if out.otherErr != 0 {
		t.Fatalf("出现 %d 次非预期错误: %v", out.otherErr, out.errs)
	}
	if out.ok != n {
		t.Fatalf("成功 %d 次, want %d（余额充足）", out.ok, n)
	}

	got := balanceCentsOf(t, st, "u1")
	if want := int64(initial) - wantCents; got != want {
		t.Errorf("余额 = %d 分, want %d（差 %d 分）—— 并发下余数发生丢失更新（少收钱）",
			got, want, got-want)
	}
	// 余数必须精确等于「总额 mod 一分」——多一微元就是多收，少一微元就是少收。
	if rem := readRemainder(t, st, "u1"); rem != wantRem {
		t.Errorf("余数 = %d 微元, want %d（差 %d 微元）—— 并发下余数不精确",
			rem, wantRem, rem-wantRem)
	}
}

// ---------------------------------------------------------------------------
// B4/B5：余数机制的两个**方向性**缺陷（刻意不修的探针）
//
// 下面两个用例与上面 B1–B3 的性质不同：B1–B3 验证「机制按设计工作」，
// 而这两个**量化缺陷的方向与幅度**。它们断言的是「实际观测到的行为」，
// 不是「应该有的行为」—— 因此它们会一直绿，但每个用例都把
// 损失/偏差的绝对大小打进日志。这样做的理由：把缺陷固化成可执行的证据，
// 比在报告里写一段散文更不容易被后人忽略；同时也保证 CI 不会因为
// 「发现了 bug」而变红（按任务要求，发现 bug 只报告、不修）。
// ---------------------------------------------------------------------------

// B4：余额不足时，**本次调用已累加的余数被回滚丢弃**，而真实调用方不会重试。
//
// # 现象
//
// ChargeBalance 在余额不足时返回 ErrInsufficientBalance 并**回滚整个事务**
// —— 这是 2026-10-10 修 P0（失败路径误提交占位行）时的正确改动。但回滚
// 连带撤销了同一事务里前面那次 `balance_remainder = balance_remainder + ?`
// （见 balance_dao.go 里 `UPDATE users SET balance_remainder = ... RETURNING`），
// 于是**本次费用中留在余数里的那部分钱消失了**。
//
// 代码注释（balance_dao.go「回滚会一并撤销上面那条余数累加」一段）对此知情，
// 它给出的理由是：「余数照留由**调用方重试时重新累加**实现」。
//
// # 但这个前提在代码里不成立
//
// 真实调用方是 usageRecorder.charge（cmd/gateway/main.go）：它在收到
// ErrInsufficientBalance 时**只记一条 ERROR 日志然后放弃**，不重试 ——
// 注释写的是「预检在前，正常路径走不到这里……钱收不回，报欠费比悄悄放过更诚实」。
//
// 也就是说「调用方重试时重新累加」这条路径**不存在**。余额不足时那一笔
// 费用（含其不足一分的余数部分）永久漏收，且不会有任何人对账发现。
//
// # 幅度（本用例实测）
//
// 漏收上限 = 单次调用中「跨过整分之前的那部分」，即 < 1 分/次。
// 对 >= 1 分的调用，不扣属于**设计意图**（「不扣 + 报欠费」优于
// 「扣一部分 + 报欠费」，见 balance_dao.go 的说明）；本用例量化的是
// **设计注释与实现不一致**的那一部分 —— 余数机制本应保住的小额。
func TestStress_RemainderDiscardedOnInsufficientBalance(t *testing.T) {
	ctx := context.Background()
	st := testStore(t, t.TempDir()+"/gw.db")

	const (
		zero  = 0    // 一分钱都没有
		fee   = 0.02 // 2 分：跨过一分阈值，触发扣减与余额守卫
		topUp = 100
	)
	mustUser(t, st, "u1", zero, false)

	// 这次扣费会：累加 20000 微元到余数 → 跨过一分 → 尝试扣 2 分 →
	// 余额 0 不够 → ErrInsufficientBalance → **整体回滚**。
	err := st.ChargeBalance(ctx, "u1", fee, "insufficient-req")
	if !errors.Is(err, ErrInsufficientBalance) {
		t.Fatalf("err = %v, want ErrInsufficientBalance", err)
	}

	rem := readRemainder(t, st, "u1")
	if rem == 0 {
		t.Logf("缺陷确认: 本次费用 %v 元（%d 微元）随 ErrInsufficientBalance 的回滚"+
			"被**永久丢弃**；真实调用方不重试，故这笔钱收不回来（上限 < 1 分/次）",
			fee, YuanToMicros(fee))
	} else {
		// 若某天实现改成「失败时单独保留余数」，会走到这里 —— 那是改进。
		t.Logf("余数 = %d 微元：本次费用未丢失（实现已改为失败时保留余数）", rem)
	}

	// 充值后什么都不会自动发生 —— 真实链路里没有「重试」这一步。
	if err := st.AdjustBalance(ctx, "u1", topUp); err != nil {
		t.Fatalf("充值: %v", err)
	}
	after := balanceCentsOf(t, st, "u1")
	if after != topUp {
		t.Errorf("充值后余额 = %d, want %d（那笔 2 分费用不该被追溯扣除）", after, topUp)
	}
	// 漏收的证据：余额一分没少，流水里也没有这笔费用。
	if charged := sumChargeCents(t, st, "u1"); charged != 0 {
		t.Errorf("流水累计 = %d 分, want 0（失败的调用不该留下扣费记录）", charged)
	}
}

// B5：YuanToMicros 对亚微元费用的「至少记 1 微元」兜底使误差**单向偏大**。
//
// # 与 YuanToCents 的关键差别
//
// YuanToCents 的四舍五入误差是**零均值**的（其注释正是以此论证「四舍五入
// 优于截断」）。但 YuanToMicros 不是：它对 `micros < 1` 的正费用一律
// **向上**记 1 微元：
//
//	// 极端小的正费用（低于 0.5 微元）也至少记 1 微元 ——
//	// 记 0 等于把这次消费丢掉，而余数机制的意义正是「不丢」。
//
// 这个方向作为**单次**决策无可指摘（宁可多记不可丢），但它是**系统性向上**
// 的：偏差随调用次数线性累积，不会相互抵消。
//
// # 幅度（本用例实测）
//
// 每次 0.0000001 元 = 0.1 微元，被记为 1 微元 → **每次放大 10 倍**。
// 调用越多，多收越多。这不是「差一分」的噪声，而是与调用量成正比的
// 倍数级偏差 —— 在极低价模型或极短请求上会真实发生。
//
// 本用例断言的是**实际行为**（会绿），并把放大倍数打进日志。
func TestStress_SubMicroFeesRoundUpSystematically(t *testing.T) {
	if testing.Short() {
		t.Skip("该用例要跑上万次独立事务，-short 下跳过")
	}
	ctx := context.Background()
	st := testStore(t, t.TempDir()+"/gw.db")

	const (
		n        = 10_000
		eachYuan = 0.0000001 // 0.1 微元
		initial  = 1_000_000
	)
	mustUser(t, st, "u1", initial, false)

	for i := 0; i < n; i++ {
		if err := st.ChargeBalance(ctx, "u1", eachYuan, fmt.Sprintf("submicro-%d", i)); err != nil {
			t.Fatalf("第 %d 次扣费: %v", i, err)
		}
	}

	charged := int64(initial) - balanceCentsOf(t, st, "u1")

	// 真实费用：n × 0.1 微元 = 1000 微元 = 0.1 分（不足一分，本应只进余数）。
	trueMicros := int64(n) / 10
	// 实际记账：每次被兜底成 1 微元 → n 微元 = 1 分。
	wantCharged := int64(n) / MicrosPerCent
	if charged != wantCharged {
		t.Errorf("实扣 = %d 分, want %d（兜底行为变了？）", charged, wantCharged)
	}

	// 倍数用**微元**比而不是分：真实值不足一分，用分会截断成 0 而无法比较。
	ratio := float64(n) / float64(trueMicros)
	t.Logf("偏差确认: %d 次 × %v 元 —— 真实费用 %d 微元（%.1f 分），"+
		"实扣 %d 分（= %d 微元），**放大 %.0f 倍**（误差单向偏大，随调用次数线性累积）",
		n, eachYuan, trueMicros, float64(trueMicros)/float64(MicrosPerCent),
		charged, int64(n), ratio)

	// 兜底本身必须仍在（否则亚微元费用会被抹成 0，那是更糟的方向）。
	if rem := readRemainder(t, st, "u1"); rem != 0 {
		t.Errorf("余数 = %d 微元, want 0（n 是 10000 的整数倍）", rem)
	}
}

// ---------------------------------------------------------------------------
// C. 幂等
// ---------------------------------------------------------------------------

// C1：同一 request_id 并发重试 N 次 → 流水 1 行、只扣 1 次。
//
// 为什么必须**并发**重试：串行重试只验证了「第二次读到了已存在的占位」；
// 并发重试才暴露「两个事务同时 SELECT 到没有占位、都执行 INSERT」的竞态。
// 主键是最后一道闸，本用例验证它真的挡住了。
func TestStress_Idempotency_SameRequestIDConcurrent(t *testing.T) {
	ctx := context.Background()
	st := testStore(t, t.TempDir()+"/gw.db")

	const (
		n        = 50
		eachCent = 10
		initial  = 1_000
	)
	mustUser(t, st, "u1", initial, false)

	var out chargeOutcome
	runConcurrent(n, func(i int) {
		out.record(st.ChargeBalance(ctx, "u1", centsYuan(eachCent), "SAME-REQ-ID"))
	})

	if out.otherErr != 0 {
		t.Fatalf("出现 %d 次非预期错误: %v", out.otherErr, out.errs)
	}
	// 幂等命中**不是错误**（设计如此）：调用方要的正是「别再扣一次」。
	if out.ok != n {
		t.Errorf("返回 nil 的次数 = %d, want %d（幂等命中也是 nil）", out.ok, n)
	}

	if rows := countChargeRows(t, st, "u1"); rows != 1 {
		t.Errorf("流水行数 = %d, want **恰好 1**（同一 request_id 只该占一行）", rows)
	}
	got := balanceCentsOf(t, st, "u1")
	if want := int64(initial - eachCent); got != want {
		t.Errorf("余额 = %d 分, want %d —— 并发重试重复扣费了（差 %d 分）",
			got, want, got-want)
	}
	if charged := sumChargeCents(t, st, "u1"); charged != eachCent {
		t.Errorf("流水累计 = %d 分, want %d —— 重复扣费", charged, eachCent)
	}
}

// C2：**不同**用户撞同一 request_id → 两人都要扣（修复过的 P0）。
//
// 修复前主键只有 request_id，第二个用户的占位撞唯一键 → INSERT OR IGNORE
// 影响 0 行 → 走「已扣过」快速路径直接返回，**第二个人一分钱没扣**。
// 这是跨租户的静默漏收。
//
// 断言：两个用户余额都减少、各自有 1 行流水、累计各等于自己的金额。
func TestStress_Idempotency_DifferentUsersSameRequestIDBothCharged(t *testing.T) {
	ctx := context.Background()
	st := testStore(t, t.TempDir()+"/gw.db")

	const (
		initial    = 1_000
		aliceCents = 7
		bobCents   = 13
	)
	mustUser(t, st, "alice", initial, false)
	mustUser(t, st, "bob", initial, false)

	// 并发发起，用同一个 request_id —— 最贴近「两个请求恰好撞号」的布局。
	var outA, outB chargeOutcome
	runConcurrent(2, func(i int) {
		if i == 0 {
			outA.record(st.ChargeBalance(ctx, "alice", centsYuan(aliceCents), "COLLIDE"))
		} else {
			outB.record(st.ChargeBalance(ctx, "bob", centsYuan(bobCents), "COLLIDE"))
		}
	})

	if outA.otherErr != 0 || outB.otherErr != 0 {
		t.Fatalf("非预期错误: alice=%v bob=%v", outA.errs, outB.errs)
	}

	// 两个人**都**必须被扣到。这是本用例的全部意义。
	gotA := balanceCentsOf(t, st, "alice")
	gotB := balanceCentsOf(t, st, "bob")
	if gotA != initial-aliceCents {
		t.Errorf("alice 余额 = %d, want %d —— 她的扣费被 bob 的 request_id 吞了",
			gotA, initial-aliceCents)
	}
	if gotB != initial-bobCents {
		t.Errorf("bob 余额 = %d, want %d —— 他的扣费被 alice 的 request_id 吞了",
			gotB, initial-bobCents)
	}
	if charged := sumChargeCents(t, st, "alice"); charged != aliceCents {
		t.Errorf("alice 流水累计 = %d, want %d", charged, aliceCents)
	}
	if charged := sumChargeCents(t, st, "bob"); charged != bobCents {
		t.Errorf("bob 流水累计 = %d, want %d", charged, bobCents)
	}
	// 每个用户各 1 行：复合主键 (request_id, user_id) 允许同 request_id 共存。
	if rows := countChargeRows(t, st, "alice"); rows != 1 {
		t.Errorf("alice 流水行数 = %d, want 1", rows)
	}
	if rows := countChargeRows(t, st, "bob"); rows != 1 {
		t.Errorf("bob 流水行数 = %d, want 1", rows)
	}
}

// C3：**扣费失败后重试 → 钱要能扣到**（修复过的 P0）。
//
// 修复前失败路径会 **Commit** 占位行，于是「这次没扣成」被永久记成
// 「已经扣过」：用户充值后重试，命中 RowsAffected()==0 快速路径，
// 静静返回 nil，一分钱都收不回来。
//
// 本用例把它压到并发形态：余额不足 → 并发重试全部失败 → 充值 →
// 再重试必须**真的扣到钱**。
//
// 断言（关键）：充值后重试成功，余额精确减少该金额；失败的那些重试
// **不得**留下任何 amount_cents>0 的流水。
func TestStress_Idempotency_RetryAfterInsufficientEventuallyCharges(t *testing.T) {
	ctx := context.Background()
	st := testStore(t, t.TempDir()+"/gw.db")

	const (
		initial   = 50  // 0.5 元
		chargeAmt = 500 // 5 元 —— 远超余额，必然失败
		topUp     = 10_000
	)
	mustUser(t, st, "u1", initial, false)

	// 阶段 1：余额不足，并发重试 N 次（同一 request_id）。
	const retries = 20
	var fail phase
	runConcurrent(retries, func(i int) {
		err := st.ChargeBalance(ctx, "u1", centsYuan(chargeAmt), "RETRY-AFTER-INSUFF")
		fail.record(err)
	})
	if fail.insufficient != retries {
		t.Errorf("阶段1: ErrInsufficientBalance = %d 次, want %d（余额不足应全部拒绝）",
			fail.insufficient, retries)
	}
	// 失败不得留下「已扣钱」的痕迹。
	if charged := sumChargeCents(t, st, "u1"); charged != 0 {
		t.Errorf("阶段1: 失败的扣费却记了 %d 分流水 —— 占位被误提交（P0 回归）", charged)
	}
	if got := balanceCentsOf(t, st, "u1"); got != initial {
		t.Errorf("阶段1: 余额 = %d, want %d（失败不该动余额）", got, initial)
	}

	// 阶段 2：充值。
	if err := st.AdjustBalance(ctx, "u1", topUp); err != nil {
		t.Fatalf("充值失败: %v", err)
	}
	afterTopUp := balanceCentsOf(t, st, "u1")
	if afterTopUp != initial+topUp {
		t.Fatalf("充值后余额 = %d, want %d", afterTopUp, initial+topUp)
	}

	// 阶段 3：用**同一个 request_id** 重试 —— 钱必须能扣到。
	//
	// 修复前：这里返回 nil（命中占位幂等快速路径），余额纹丝不动。
	// 修复后：占位随失败一起回滚，重试是一次全新的扣费，真的扣钱。
	err := st.ChargeBalance(ctx, "u1", centsYuan(chargeAmt), "RETRY-AFTER-INSUFF")
	if err != nil {
		t.Fatalf("阶段3: 充值后重试仍失败: %v（钱收不回来）", err)
	}
	got := balanceCentsOf(t, st, "u1")
	if want := int64(afterTopUp - chargeAmt); got != want {
		t.Errorf("阶段3: 余额 = %d, want %d —— 差 %d 分；失败路径误提交占位导致重试被静默吞掉（P0 回归）",
			got, want, got-want)
	}
	if charged := sumChargeCents(t, st, "u1"); charged != chargeAmt {
		t.Errorf("阶段3: 流水累计 = %d 分, want %d", charged, chargeAmt)
	}
}

// phase 是带行数的结果分类（与 chargeOutcome 同构，供无并发场景用）。
type phase struct {
	ok           int64
	insufficient int64
	otherErr     int64
	errs         []error
	mu           sync.Mutex
}

func (p *phase) record(err error) {
	switch {
	case err == nil:
		atomic.AddInt64(&p.ok, 1)
	case errors.Is(err, ErrInsufficientBalance):
		atomic.AddInt64(&p.insufficient, 1)
	default:
		atomic.AddInt64(&p.otherErr, 1)
		p.mu.Lock()
		p.errs = append(p.errs, err)
		p.mu.Unlock()
	}
}

// A4：**完整计费生命周期**端到端并发 —— 落 usage → 触发器累加 → 读回
// 固化费用 → 按该费用扣余额，四个环节串起来后账目必须闭合。
//
// # 为什么这条最重要
//
// A1–A3 只压了「扣」这一个环节：调用方直接给一个金额。但真实链路上，
// 扣费的金额**不是调用方随便给的**，而是 `CreateUsageRecordWithCost`
// 返回的那个 `cost`（它与写进 `usage_records.cost_total` 的是**同一个变量**）。
//
// # 本用例的第一版断言是错的，失败结果本身就是一条重要发现
//
// 初版按「逐条 YuanToCents(cost) 求和 == 实际扣的分」断言，实测直接失败：
//
//	余额减少 = 7 分, want 0（差 7）
//	流水累计 = 7 分, want 0
//
// 原因：本 fixture 下每次费用只有约 600 **微元**（0.006 分），逐条
// `YuanToCents` 全部舍成 0，而实际收上了 7 分。这说明真实链路走的是
// **微元累加 + 满一分才扣**（ChargeBalance 的余数机制），而不是
// 「每条各自舍入到分再相加」—— 后者在低价模型下会把每一笔都舍成 0，
// 那正是余数机制要解决的那个缺陷的原形。
//
// 所以正确的恒等式在**微元**层面成立，本用例据此断言（比按分断言更锋利：
// 差 1 微元 = 1e-6 元就会失败）。
//
// 构造：N 个并发请求，每个：
//  1. 造一条 usage（带明确 token 数），拿回 cost；
//  2. 扣余额，并记录 YuanToMicros(cost)；
//  3. 全部结束后核对：余额减少 == floor(Σ微元 / 每分微元)、
//     余数 == Σ微元 mod 每分微元、且从库里读回固化费用重算一遍也一致。
func TestStress_FullLifecycle_ChargeMatchesFrozenCostExactly(t *testing.T) {
	ctx := context.Background()
	st := testStore(t, t.TempDir()+"/gw.db")
	seedPricedModel(t, st, 0.2, 0.02, 0.8)
	// used_tokens 由 usage 触发器按 access_key_id 累加，故必须真有一把 key。
	mustKeyWithQuota(t, st, "k1", 0) // quota=0 = 不限，不干扰本用例

	const (
		workers = 100
		initial = 10_000_000 // 10 万元，绝对够
	)
	mustUser(t, st, "u1", initial, false)

	type result struct {
		costMicros int64
		err        error
	}
	results := make([]result, workers)

	runConcurrent(workers, func(i int) {
		// 每个请求的 token 数刻意不同，让 cost 不是一个常数 ——
		// 若有人把金额算成「次数 × 某个固定值」，这里会立刻暴露。
		in := int64(1000 + i*7)
		out := int64(500 + i*3)
		cached := int64(i % 100)

		rec := &UsageRecord{
			ID: fmt.Sprintf("lc-%d", i), UserID: "u1", AccessKeyID: "k1",
			PublicModel: "priced", ProviderID: "p1", UpstreamModel: "priced",
			IngressProtocol: "openai-chat",
			InputTokens:     in, OutputTokens: out, CachedTokens: cached,
			TotalTokens: in + out,
			UsageState:  "reported", Status: "ok", HTTPStatus: 200,
			LatencyMs: 100,
		}
		cost, err := st.CreateUsageRecordWithCost(ctx, rec)
		if err != nil {
			results[i] = result{err: fmt.Errorf("worker %d usage: %w", i, err)}
			return
		}
		micros := YuanToMicros(cost)
		if err := st.ChargeBalance(ctx, "u1", cost, fmt.Sprintf("lc-req-%d", i)); err != nil {
			results[i] = result{err: fmt.Errorf("worker %d charge: %w", i, err)}
			return
		}
		results[i] = result{costMicros: micros}
	})

	// 任何一环失败都必须暴露 —— 不能只看「最后余额」。
	var wantMicros int64
	for i, r := range results {
		if r.err != nil {
			t.Fatalf("worker %d 失败: %v", i, r.err)
		}
		wantMicros += r.costMicros
	}
	// 微元层面累加 → 整除得分，余数留下。这是余数机制的精确语义。
	wantCents := wantMicros / MicrosPerCent
	wantRem := wantMicros % MicrosPerCent

	// 闭包 1：余额减少 == floor(总微元 / 每分微元)。
	gotBalance := balanceCentsOf(t, st, "u1")
	if got := int64(initial) - gotBalance; got != wantCents {
		t.Errorf("余额减少 = %d 分, want %d（总微元 %d / %d）",
			got, wantCents, wantMicros, MicrosPerCent)
	}

	// 闭包 2：流水累计 == 同一数字。
	if got := sumChargeCents(t, st, "u1"); got != wantCents {
		t.Errorf("流水累计 = %d 分, want %d", got, wantCents)
	}

	// 闭包 3：**余数精确** —— 余数机制在真实链路上的核心断言。
	// 多发/少发哪怕 1 微元都会让这里失败，而 1 微元 = 1e-6 元。
	if rem := readRemainder(t, st, "u1"); rem != wantRem {
		t.Errorf("余数 = %d 微元, want %d（总微元 %d mod %d）—— 余数不精确",
			rem, wantRem, wantMicros, MicrosPerCent)
	}

	// 闭包 4：从**库里读回**固化费用再算一遍，必须与扣费侧用的总数一致。
	// 这条防的是「返回值与落库值不一致」（两者本应是同一个变量，
	// 但正是「本应是」才需要验证）。
	var sumFrozenMicros int64
	rows, err := st.read.QueryContext(ctx,
		`SELECT cost_total FROM usage_records WHERE user_id = ? ORDER BY id`, "u1")
	if err != nil {
		t.Fatalf("读 usage: %v", err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var c float64
		if err := rows.Scan(&c); err != nil {
			t.Fatalf("scan cost: %v", err)
		}
		sumFrozenMicros += YuanToMicros(c)
		n++
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if n != workers {
		t.Fatalf("usage 记录数 = %d, want %d", n, workers)
	}
	if sumFrozenMicros != wantMicros {
		t.Errorf("库里固化费用换算的微元总和 = %d, 扣费侧用的 = %d（差 %d 微元）",
			sumFrozenMicros, wantMicros, sumFrozenMicros-wantMicros)
	}

	// 闭包 5：触发器把真实用量累进了 used_tokens（与扣费是不同的机制，
	// 两者都对才说明「报表」与「余额」不会各说各话）。
	var wantTokens int64
	for i := 0; i < workers; i++ {
		in := int64(1000 + i*7)
		out := int64(500 + i*3)
		wantTokens += in + out
	}
	if used := readUsed(t, st, "k1"); used != wantTokens {
		t.Errorf("used_tokens = %d, want %d", used, wantTokens)
	}
}

// ---------------------------------------------------------------------------
// F. 持久化健壮性
// ---------------------------------------------------------------------------

// F1：不 Close 直接丢引用 → 重开库数据仍完整可读。
//
// 场景：进程被 kill（没有优雅关闭），WAL 里可能有未 checkpoint 的已提交事务。
// 重开必须能读到它们 —— 否则「已提交的扣费丢了」就意味着账目凭空少一笔。
//
// # 关于生命周期（这里有个测试自身的坑，记下来免得后人踩）
//
// 本用例的关键是「**旧句柄仍然打开着**的同时重开同一个库」—— 这才等价于
// 「进程被杀，文件锁还挂着」。所以**不能**用 testStore（它注册了
// t.Cleanup(st.Close)，但那只在测试结束时生效，不影响场景）。
//
// 但旧句柄不能一直开着不关：Windows 上未释放的文件句柄会让
// `t.TempDir()` 的清理失败（实测报 "The process cannot access the file
// because it is being used by another process"）。所以用 t.Cleanup
// **在验证之后**关闭它 —— Cleanup 是 LIFO，而 t.TempDir() 的删除是自己
// 注册的、更早注册，故本文件的 Cleanup 会先跑，句柄先释放，删除才成功。
//
// 这不削弱用例：重开与全部读校验都发生在旧句柄仍然打开的时候。
func TestStress_Persistence_ReopenWithoutCloseKeepsCommittedData(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "gw.db")

	st1, err := Open(path, discardLogger())
	if err != nil {
		t.Fatalf("首次打开: %v", err)
	}
	// 在验证完成后释放句柄（理由见上）。注意这是 t.Cleanup，不是立即 Close。
	t.Cleanup(func() { _ = st1.Close() })

	mustUser(t, st1, "u1", 1_000, false)
	for i := 0; i < 50; i++ {
		if err := st1.ChargeBalance(ctx, "u1", centsYuan(3), fmt.Sprintf("persist-%d", i)); err != nil {
			t.Fatalf("第 %d 次扣费: %v", i, err)
		}
	}
	wantBalance := int64(1_000 - 50*3)
	wantCharged := int64(50 * 3)

	if got := balanceCentsOf(t, st1, "u1"); got != wantBalance {
		t.Fatalf("关闭前余额 = %d, want %d", got, wantBalance)
	}

	// **st1 仍然打开**，此时重开同一个库 —— 模拟崩溃后重启的第二个实例。
	st2, err := Open(path, discardLogger())
	if err != nil {
		t.Fatalf("未 Close 后重开失败: %v", err)
	}
	defer st2.Close()

	got := balanceCentsOf(t, st2, "u1")
	if got != wantBalance {
		t.Errorf("重开后余额 = %d, want %d —— 已提交的扣费丢失了 %d 分",
			got, wantBalance, got-wantBalance)
	}
	if charged := sumChargeCents(t, st2, "u1"); charged != wantCharged {
		t.Errorf("重开后流水累计 = %d, want %d", charged, wantCharged)
	}
	// 重开后必须仍然可以正常扣费（不是「只读能用」）。
	if err := st2.ChargeBalance(ctx, "u1", centsYuan(1), "after-reopen"); err != nil {
		t.Errorf("重开后继续扣费失败: %v", err)
	}
	if got := balanceCentsOf(t, st2, "u1"); got != wantBalance-1 {
		t.Errorf("重开后扣 1 分, 余额 = %d, want %d", got, wantBalance-1)
	}
}

// F2：并发读写下是否报 `database is locked`。
//
// 这是真实的可用性问题：写池是单连接（SetMaxOpenConns(1)），读池 4 连接，
// 且 DSN 带 `_busy_timeout=5000`。本用例让**读写同时**高强度发生，
// 统计任何包含 "locked"/"busy" 的错误。
//
// 期望：0 次。非 0 就要如实报告为可用性缺陷（不是资金缺陷，但会让请求 500）。
func TestStress_Persistence_NoDatabaseLockedUnderConcurrentReadWrite(t *testing.T) {
	ctx := context.Background()
	st := testStore(t, t.TempDir()+"/gw.db")
	mustUser(t, st, "u1", 1_000_000, false)
	mustUser(t, st, "u2", 1_000_000, false)

	const (
		writers    = 20
		writesEach = 25
		readers    = 20
		readsEach  = 50
	)
	var (
		lockErrs  int64
		otherErrs int64
		mu        sync.Mutex
		firstErrs []error
	)
	note := func(err error) {
		if err == nil {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		if len(firstErrs) < 8 {
			firstErrs = append(firstErrs, err)
		}
	}

	// 写者：扣费（走写池）。读者：读余额（走读池）。
	runConcurrent(writers+readers, func(i int) {
		if i < writers {
			user := "u1"
			if i%2 == 0 {
				user = "u2"
			}
			for j := 0; j < writesEach; j++ {
				err := st.ChargeBalance(ctx, user, centsYuan(1), fmt.Sprintf("rw-w-%d-%d", i, j))
				if err != nil {
					if isBusyOrLocked(err) {
						atomic.AddInt64(&lockErrs, 1)
					} else {
						atomic.AddInt64(&otherErrs, 1)
					}
					note(err)
				}
			}
			return
		}
		for j := 0; j < readsEach; j++ {
			if _, _, err := st.BalanceOf(ctx, "u1"); err != nil {
				if isBusyOrLocked(err) {
					atomic.AddInt64(&lockErrs, 1)
				} else {
					atomic.AddInt64(&otherErrs, 1)
				}
				note(err)
			}
			// 也读一次明细（读池上的聚合查询），更接近真实看板负载。
			var n int64
			if err := st.read.QueryRowContext(ctx,
				`SELECT COUNT(*) FROM balance_charges WHERE user_id = ?`, "u1").Scan(&n); err != nil {
				if isBusyOrLocked(err) {
					atomic.AddInt64(&lockErrs, 1)
				} else {
					atomic.AddInt64(&otherErrs, 1)
				}
				note(err)
			}
		}
	})

	if otherErrs > 0 {
		t.Errorf("并发读写出现 %d 次非 busy/locked 错误: %v", otherErrs, firstErrs)
	}
	if lockErrs > 0 {
		t.Errorf("并发读写出现 %d 次 database is locked / busy —— 真实可用性问题: %v",
			lockErrs, firstErrs)
	}

	// 账目仍必须精确：写者总数 × 1 分。
	total := int64(writers * writesEach)
	sum := sumChargeCents(t, st, "u1") + sumChargeCents(t, st, "u2")
	if sum != total {
		t.Errorf("并发读写后流水累计 = %d 分, want %d（写丢或写重）", sum, total)
	}
	b1 := balanceCentsOf(t, st, "u1")
	b2 := balanceCentsOf(t, st, "u2")
	if b1 < 0 || b2 < 0 {
		t.Errorf("出现负余额: u1=%d u2=%d", b1, b2)
	}
	if b1+b2 != 2_000_000-total {
		t.Errorf("两户余额之和 = %d, want %d", b1+b2, int64(2_000_000)-total)
	}
}

// isBusyOrLocked 判定错误是否为 SQLite 的忙/锁冲突。
//
// 用文本匹配而不是错误类型：modernc 驱动把这类错误包成 *sqlite.Error，
// 但它没有导出可供 errors.As 的具名 sentinel（与 tx.go 的
// IsUniqueViolation 同一处理方式 —— 那份注释解释了为什么文本匹配是
// 这里稳定且够用的判据）。
func isBusyOrLocked(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, sub := range []string{"database is locked", "database table is locked", "sqlite_busy", "busy"} {
		if strings.Contains(msg, sub) {
			return true
		}
	}
	return false
}

// F3：**长写事务**与并发读写共存时是否报 locked。
//
// 与 F2 的差别：F2 里每个写都是短事务（单条 UPDATE）。真实系统里有一类
// **长写事务** —— `PruneOldUsage` 的剪枝（圈批 → 聚合 → 删除全程持锁，
// 首批可达数万行）。写池是单连接，所以剪枝期间**所有**写入排队。
//
// 要验证的是：排队是否表现为「等到 busy_timeout（5s）后报 locked」。
// 若剪枝耗时 < 5s，写入应当只是变慢，不应失败。
//
// 构造：少量跨 30 天的明细（让剪枝真的删东西但不会太久）+
// 同时跑的扣费与读余额。统计任何 locked/busy 错误。
func TestStress_Persistence_PruneWhileChargingNoLocked(t *testing.T) {
	ctx := context.Background()
	st := testStore(t, t.TempDir()+"/gw.db")

	base := time.Now()
	freezeNow(t, base)
	// 造足够被剪的数据，但不至于让剪枝跑几十秒。
	mustProviderAndModel(t, st)
	seedOldUsage(t, st, base)

	mustUser(t, st, "u1", 10_000_000, false)

	var (
		lockErrs  int64
		otherErrs int64
		mu        sync.Mutex
		firstErrs []error
		charges   int64
	)
	note := func(err error) {
		mu.Lock()
		defer mu.Unlock()
		if len(firstErrs) < 8 {
			firstErrs = append(firstErrs, err)
		}
	}

	const writers = 10
	var prunedRows int64
	runConcurrent(writers+1, func(i int) {
		// 一个 goroutine 负责剪枝（长写事务）。
		if i == writers {
			res, err := st.PruneOldUsage(ctx, DefaultRetentionDays)
			if err != nil {
				if isBusyOrLocked(err) {
					atomic.AddInt64(&lockErrs, 1)
				} else {
					atomic.AddInt64(&otherErrs, 1)
				}
				note(err)
				return
			}
			atomic.StoreInt64(&prunedRows, res.DeletedRows)
			return
		}
		// 其余并发扣费（短写事务，会与剪枝争写池）。
		for j := 0; j < 20; j++ {
			err := st.ChargeBalance(ctx, "u1", centsYuan(1), fmt.Sprintf("prune-w-%d-%d", i, j))
			if err != nil {
				if isBusyOrLocked(err) {
					atomic.AddInt64(&lockErrs, 1)
				} else {
					atomic.AddInt64(&otherErrs, 1)
				}
				note(err)
				continue
			}
			atomic.AddInt64(&charges, 1)
		}
		// 同时读余额（走读池，应当完全不受写锁影响）。
		for j := 0; j < 20; j++ {
			if _, _, err := st.BalanceOf(ctx, "u1"); err != nil {
				if isBusyOrLocked(err) {
					atomic.AddInt64(&lockErrs, 1)
				} else {
					atomic.AddInt64(&otherErrs, 1)
				}
				note(err)
			}
		}
	})

	if otherErrs > 0 {
		t.Errorf("剪枝期间出现 %d 次非 busy/locked 错误: %v", otherErrs, firstErrs)
	}
	if lockErrs > 0 {
		t.Errorf("剪枝期间出现 %d 次 database is locked / busy —— 可用性问题: %v",
			lockErrs, firstErrs)
	}
	// 断言剪枝**真的删了东西**：否则本用例是空转（没有长写事务，
	// 「不报 locked」这个结论就没有被验证到）。这是防止用例退化成
	// 「永远绿但什么都没测」的关键一步。
	if prunedRows == 0 {
		t.Fatal("剪枝未删除任何行 —— 本用例没有真正制造长写事务，结论无效")
	}
	t.Logf("剪枝删除 %d 行（长写事务确实发生），期间 %d 次扣费、0 次 locked",
		prunedRows, charges)

	// 账目仍须精确：成功扣费次数 × 1 分。
	if got, want := sumChargeCents(t, st, "u1"), charges; got != want {
		t.Errorf("流水累计 = %d 分, 成功扣费 %d 次 —— 每次 1 分，账目对不上", got, want)
	}
	if b := balanceCentsOf(t, st, "u1"); b != 10_000_000-charges {
		t.Errorf("余额 = %d, want %d", b, int64(10_000_000)-charges)
	}
}

// F4：**跨连接**（两个 Store 打开同一个文件）并发扣费。
//
// 这是 F2/F3 覆盖不到的一类：单进程内 SetMaxOpenConns(1) 完全避免了自争抢，
// 但备份脚本、CLI 工具、或**误起的第二个实例**会以独立连接打开同一个文件。
// DSN 里的 `_busy_timeout=5000` 正是为这种情形准备的（store.go 的注释说明了
// 「没有它，一次写撞锁就直接失败，而配额预检是 fail-open 的，会静默放行
// 超额请求」）。
//
// 断言：两个 Store 各自并发扣同一个用户，**总额精确**且不报 locked。
// 这是对「多实例下账目仍然正确」的直接验证。
func TestStress_Persistence_TwoStoresSameFileNoLostMoney(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "shared.db")

	st1, err := Open(path, discardLogger())
	if err != nil {
		t.Fatalf("open st1: %v", err)
	}
	defer st1.Close()
	st2, err := Open(path, discardLogger())
	if err != nil {
		t.Fatalf("open st2: %v", err)
	}
	defer st2.Close()

	mustUser(t, st1, "u1", 1_000_000, false)

	const perStore = 50
	var (
		lockErrs  int64
		otherErrs int64
		ok        int64
		mu        sync.Mutex
		firstErrs []error
	)
	note := func(err error) {
		mu.Lock()
		defer mu.Unlock()
		if len(firstErrs) < 8 {
			firstErrs = append(firstErrs, err)
		}
	}

	// 两个 Store 各起一个 goroutine，各自连续扣费（request_id 带前缀避免撞号）。
	runConcurrent(2, func(i int) {
		st := st1
		prefix := "s1"
		if i == 1 {
			st = st2
			prefix = "s2"
		}
		for j := 0; j < perStore; j++ {
			err := st.ChargeBalance(ctx, "u1", centsYuan(1), fmt.Sprintf("%s-%d", prefix, j))
			if err != nil {
				if isBusyOrLocked(err) {
					atomic.AddInt64(&lockErrs, 1)
				} else {
					atomic.AddInt64(&otherErrs, 1)
				}
				note(err)
				continue
			}
			atomic.AddInt64(&ok, 1)
		}
	})

	if otherErrs > 0 {
		t.Errorf("双 Store 出现 %d 次非 busy/locked 错误: %v", otherErrs, firstErrs)
	}
	if lockErrs > 0 {
		t.Errorf("双 Store 出现 %d 次 database is locked —— busy_timeout 未生效？: %v",
			lockErrs, firstErrs)
	}

	want := int64(perStore * 2)
	if ok != want {
		t.Errorf("成功扣费 %d 次, want %d", ok, want)
	}
	// 两个 Store 都读一遍：账目必须一致且精确。
	for name, st := range map[string]*Store{"st1": st1, "st2": st2} {
		if got := sumChargeCents(t, st, "u1"); got != want {
			t.Errorf("%s 读到流水累计 = %d 分, want %d", name, got, want)
		}
		if got := balanceCentsOf(t, st, "u1"); got != 1_000_000-want {
			t.Errorf("%s 读到余额 = %d, want %d", name, got, int64(1_000_000)-want)
		}
	}
}

// mustProviderAndModel 造一个 provider + 模型（剪枝 fixture 需要它们来定价）。
func mustProviderAndModel(t *testing.T, st *Store) {
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
		PriceInput: 0.2, PriceOutput: 0.8,
	}); err != nil {
		t.Fatalf("model: %v", err)
	}
}

// seedOldUsage 造一批「保留窗口之外」的明细，供剪枝真的删除。
//
// 行数刻意取小（2000）：本用例验证的是「剪枝与扣费共存时是否 locked」，
// 不是剪枝性能。行数太大只会让用例变慢，而锁定行为与行数的关系是单调的
// —— 小批量没复现的话，大批量只会更久地持锁，不会改变「是否报 locked」
// 这个二值结论（这一点在报告里作为局限写明）。
func seedOldUsage(t *testing.T, st *Store, base time.Time) {
	t.Helper()
	ctx := context.Background()
	old := base.AddDate(0, 0, -DefaultRetentionDays-5).UnixMilli()
	for i := 0; i < 2000; i++ {
		if err := st.CreateUsageRecord(ctx, &UsageRecord{
			ID: fmt.Sprintf("old-%d", i), Ts: old + int64(i),
			AccessKeyID: "k1", PublicModel: "priced", ProviderID: "p1",
			UpstreamModel: "priced", IngressProtocol: "openai-chat",
			InputTokens: 100, OutputTokens: 50, TotalTokens: 150,
			UsageState: "reported", Status: "ok", HTTPStatus: 200,
			LatencyMs: 100,
		}); err != nil {
			t.Fatalf("seed old usage %d: %v", i, err)
		}
	}
}
