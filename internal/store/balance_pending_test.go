package store

import (
	"context"
	"errors"
	"testing"
)

// 欠费用户的费用零头不得永久漏收（2026-10-12 修 P1-3）。
//
// 原实现在 ChargeBalance 的欠费分支直接 return ErrInsufficientBalance，
// defer 的 Rollback 把同一事务里刚累加的 users.balance_remainder **一并撤销**。
// 而原注释称「余数照留，由调用方重试时重新累加实现」—— 实测真实调用方
// （cmd/gateway 的 usageRecorder.charge）拿到 ErrInsufficientBalance 只记一条
// ERROR 就返回，**没有任何重试**，那条路径并不存在。
//
// 后果：欠费时已结算的部分收不回来（按设计，欠费不硬扣），但**零头**本该
// 留存并随下次充值结清；回滚让它彻底消失 —— 无流水、对账查不出。
//
// 修法是独立账本 balance_pending：欠费时把金额记进去（幂等，主键 request_id），
// 充值时结转。
//
// # 前提：什么才会走到欠费分支
//
// 余额不足**不是**充分条件。ChargeBalance 的分流是：
//   - 累计不足一分 → `remainder < MicrosPerCent` 提前 commit，**余额根本不动**；
//     这是「攒零头」的正常路径，不欠费。
//   - 够扣的钱、但余额不够 → 才走欠费分支。
// 所以测试素材必须用「≥1 分」的金额，否则根本到不了被测代码。

// pendingAmount 读某用户的待结算总额（微元）。
func pendingAmount(t *testing.T, st *Store, userID string) int64 {
	t.Helper()
	var v int64
	if err := st.read.QueryRowContext(context.Background(),
		`SELECT COALESCE(SUM(amount_micros), 0) FROM balance_pending WHERE user_id = ?`,
		userID).Scan(&v); err != nil {
		t.Fatalf("read pending: %v", err)
	}
	return v
}

// 欠费分支必须把零头记进待结算账本，而不是随回滚丢掉。
//
// 素材：余额 1 分，费用 3 元（300 分）。整除后 cents=300、零头 0，
// 但**这次欠费本身**就是一笔应收 —— 按口径不硬扣，所以它必须以
// 「待结算」的形式留在账上，否则就是永久免单。
func TestBalancePending_欠费金额记入账本(t *testing.T) {
	ctx := context.Background()
	st := testStore(t, t.TempDir()+"/gw.db")
	mustUser(t, st, "broke", 1, false)

	if err := st.ChargeBalance(ctx, "broke", 3.0, "req-1"); !errors.Is(err, ErrInsufficientBalance) {
		t.Fatalf("应返回 ErrInsufficientBalance，得到 %v", err)
	}

	if got := pendingAmount(t, st, "broke"); got != 3_000_000 {
		t.Errorf("待结算账本 = %d 微元，期望 3000000 —— 欠费金额随回滚丢失了\n"+
			"  这笔费用永远收不回来，且无流水、对账查不出", got)
	}
}

// 账本必须幂等：同一 request_id 重复扣费不重复记账。
//
// 将来若给调用方加重试（这是最自然的后续演进），没有幂等就会凭空
// 多收用户钱 —— 那是比漏收更糟的方向。
func TestBalancePending_同RequestId幂等(t *testing.T) {
	ctx := context.Background()
	st := testStore(t, t.TempDir()+"/gw.db")
	mustUser(t, st, "broke2", 1, false)

	for i := 0; i < 3; i++ {
		if err := st.ChargeBalance(ctx, "broke2", 3.0, "same-req"); !errors.Is(err, ErrInsufficientBalance) {
			t.Fatalf("第 %d 次应欠费，得到 %v", i+1, err)
		}
	}

	if got := pendingAmount(t, st, "broke2"); got != 3_000_000 {
		t.Errorf("待结算 = %d 微元，期望 3000000（幂等）—— 重复记账会凭空多收用户钱", got)
	}
}

// 不同 request_id 必须各自记账 —— 真实的多次调用不能被误去重。
func TestBalancePending_不同RequestId分别记账(t *testing.T) {
	ctx := context.Background()
	st := testStore(t, t.TempDir()+"/gw.db")
	mustUser(t, st, "broke3", 1, false)

	const n = 3
	for i := 0; i < n; i++ {
		req := "req-" + itoa(i)
		if err := st.ChargeBalance(ctx, "broke3", 3.0, req); !errors.Is(err, ErrInsufficientBalance) {
			t.Fatalf("%s 应欠费，得到 %v", req, err)
		}
	}
	if got := pendingAmount(t, st, "broke3"); got != int64(n)*3_000_000 {
		t.Errorf("待结算 = %d，期望 %d —— 不同的调用被误去重了", got, int64(n)*3_000_000)
	}
}

// 审计 *X17：空 request_id 的欠费不得蒸发。公共 API 允许空 request_id，
// 旧的「requestID == "" 直接跳过 pending」会让已发生的应收完全消失。
// 这里断言：余额不变（欠费不硬扣）、仍有 pending 记录、且欠费错误照常返回。
// 欠费尝试本身必须局部回滚，所以 balance_charges 不应留下占位行。
func TestBalancePending_EmptyRequestIDStillRecordsDebt(t *testing.T) {
	ctx := context.Background()
	st := testStore(t, t.TempDir()+"/gw.db")
	mustUser(t, st, "empty-req", 1, false)

	if err := st.ChargeBalance(ctx, "empty-req", 3.0, ""); !errors.Is(err, ErrInsufficientBalance) {
		t.Fatalf("应返回 ErrInsufficientBalance，得到 %v", err)
	}
	if got := pendingAmount(t, st, "empty-req"); got != 3_000_000 {
		t.Fatalf("待结算 = %d 微元，期望 3000000（空 request_id 也必须记账）", got)
	}
	var charges int
	if err := st.read.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM balance_charges WHERE user_id = 'empty-req'`).Scan(&charges); err != nil {
		t.Fatalf("read charges: %v", err)
	}
	if charges != 0 {
		t.Fatalf("balance_charges 占位行 = %d，期望 0（欠费尝试应局部回滚）", charges)
	}
}

// 审计 *X17：欠费事务必须一次提交。这里在 balance_pending 的写入路径上
// 注入失败（把主键改成 CHECK 不可行，直接用只读触发器模拟），断言
// balance_charges 占位行与 users.balance_remainder 都不留下半个状态。
// 控制器：通过临时新增一个会拒绝插入的触发器，验证事务整体回滚。
func TestBalancePending_InsufficientPathIsAtomic(t *testing.T) {
	ctx := context.Background()
	st := testStore(t, t.TempDir()+"/gw.db")
	mustUser(t, st, "atomic", 1, false)

	if _, err := st.db.ExecContext(ctx,
		`CREATE TRIGGER reject_pending BEFORE INSERT ON balance_pending
		 BEGIN SELECT RAISE(ABORT, 'pending write rejected'); END`); err != nil {
		t.Fatalf("create trigger: %v", err)
	}

	err := st.ChargeBalance(ctx, "atomic", 3.0, "atomic-req")
	if err == nil || errors.Is(err, ErrInsufficientBalance) {
		t.Fatalf("pending 写失败时应返回底层错误而不是吞掉，得到 %v", err)
	}

	var charges int
	if err := st.read.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM balance_charges WHERE request_id = 'atomic-req'`).Scan(&charges); err != nil {
		t.Fatalf("read charges: %v", err)
	}
	if charges != 0 {
		t.Fatalf("balance_charges 留下 %d 条占位行 —— 占位与欠款必须同事务", charges)
	}
	if got := readRemainder(t, st, "atomic"); got != 0 {
		t.Fatalf("balance_remainder = %d，期望 0 —— 失败的欠费事务留下了余数", got)
	}
	assertBalance(t, st, "atomic", 1, true)
}

// 充值必须把待结算金额结转掉，且余额里体现出来。
//
// 欠下的钱本来就是他要付的，充值是它该被收掉的时刻 —— 否则它永远挂着、
// 永远收不回。
func TestBalancePending_充值时结转(t *testing.T) {
	ctx := context.Background()
	st := testStore(t, t.TempDir()+"/gw.db")
	mustUser(t, st, "broke4", 1, false)

	// 两笔欠费各 3 元 = 60000 微元 = 6 分。
	for i := 0; i < 2; i++ {
		if err := st.ChargeBalance(ctx, "broke4", 3.0, "r"+itoa(i)); !errors.Is(err, ErrInsufficientBalance) {
			t.Fatalf("应欠费，得到 %v", err)
		}
	}
	if got := pendingAmount(t, st, "broke4"); got != 6_000_000 {
		t.Fatalf("待结算 = %d，期望 6000000", got)
	}

	// 充值 10000 分 → 10000 + 1（余额原值） - 600（结转）= 9401 分。
	if _, err := st.AdjustBalanceWithLedger(ctx, "broke4", 10000, "op", "admin", "充值"); err != nil {
		t.Fatalf("AdjustBalanceWithLedger: %v", err)
	}

	// 这些账号都是**限额**账号（BalanceOf 的 limited=true），
	// 与 Unlimited 的 NULL 语义不同 —— assertBalance 的第二个参数
	// 正是「期望是否限额」，别写成 false。
	assertBalance(t, st, "broke4", 9401, true)

	if got := pendingAmount(t, st, "broke4"); got != 0 {
		t.Errorf("充值后待结算 = %d，期望 0 —— 账本没清干净，下次充值会重复扣", got)
	}
}

// 结转扣到超过余额时必须夹在 0，绝不为负
// （负余额会让「余额不足」判定恒真，用户被永久锁死）。
func TestBalancePending_结转后余额不负(t *testing.T) {
	ctx := context.Background()
	st := testStore(t, t.TempDir()+"/gw.db")
	mustUser(t, st, "poor", 0, false)

	if err := st.ChargeBalance(ctx, "poor", 3.0, "p0"); !errors.Is(err, ErrInsufficientBalance) {
		t.Fatalf("应欠费，得到 %v", err)
	}

	// 只充值 1 分，远不够结转 3 元（300 分）。
	if _, err := st.AdjustBalanceWithLedger(ctx, "poor", 100, "op", "admin", "小额充值"); err != nil {
		t.Fatalf("AdjustBalanceWithLedger: %v", err)
	}
	assertBalance(t, st, "poor", 0, true)
}

// 没有待结算记录时，充值不得受影响（结转是 no-op）。
func TestBalancePending_无待结算时充值不受影响(t *testing.T) {
	ctx := context.Background()
	st := testStore(t, t.TempDir()+"/gw.db")
	mustUser(t, st, "clean", 5000, false)

	if _, err := st.AdjustBalanceWithLedger(ctx, "clean", 10000, "op", "admin", "充值"); err != nil {
		t.Fatalf("AdjustBalanceWithLedger: %v", err)
	}
	assertBalance(t, st, "clean", 15000, true)
}
