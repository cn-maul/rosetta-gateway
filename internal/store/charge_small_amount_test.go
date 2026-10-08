package store

import (
	"context"
	"strconv"
	"testing"
)

// 2026-10-10：扣费路径里那条「未够一分时把 amount_cents 写成 0」的 UPDATE
// 是**纯冗余的写**（性能修复）。
//
// # 为什么它冗余
//
// 占位 INSERT 时 amount_cents 就已经写的是 0：
//
//	INSERT OR IGNORE INTO balance_charges (request_id, user_id, amount_cents, ts)
//	VALUES (?, ?, 0, ?)     ← 第三个参数是字面量 0
//
// 而「余数不足一分」那条分支做的事是：
//
//	UPDATE balance_charges SET amount_cents = 0 WHERE request_id=? AND user_id=?
//
// 也就是**把已经是 0 的列再写成 0**。这条语句在低价模型场景下几乎每笔都
// 执行（单次费用普遍不足一分），是纯粹的写放大。
//
// # 删它的前提：占位行的初值必须是 0，且中途没有别的路径写过它
//
// 两者都由本文件与 ChargeBalance 保证：
//   - 占位 INSERT 写死 0（上面）；
//   - 唯一会写非 0 的是下面「够扣的钱」分支里的 UPDATE，而那是**另一个分支**
//     —— 一笔扣费只走其中一条路径。
//
// 所以删掉是安全的：不足一分时流水行的 amount_cents 本来就是 0。

// TestCharge_SmallAmount_LeavesLedgerRowAtZero 是这条修复的行为钉子：
// 费用不足一分时，流水行必须存在、且 amount_cents 为 0。
//
// 修复前这条 UPDATE 会把它显式写成 0；修复后靠占位 INSERT 的初值。
// 两种路径结果必须**相同** —— 这正是可以删那条 UPDATE 的依据。
func TestCharge_SmallAmount_LeavesLedgerRowAtZero(t *testing.T) {
	ctx := context.Background()
	st := testStore(t, t.TempDir()+"/gw.db")
	mustUser(t, st, "u1", 1000, false) // 10 元

	// 0.004 元 = 4000 微元 < MicrosPerCent(10000) → 走「不足一分」分支
	const amountYuan = 0.004
	if err := st.ChargeBalance(ctx, "u1", amountYuan, "req-small"); err != nil {
		t.Fatalf("charge: %v", err)
	}

	var amount int64
	if err := st.read.QueryRowContext(ctx,
		`SELECT amount_cents FROM balance_charges WHERE request_id = ? AND user_id = ?`,
		"req-small", "u1").Scan(&amount); err != nil {
		t.Fatalf("读流水行失败（占位行必须存在）: %v", err)
	}
	if amount != 0 {
		t.Errorf("amount_cents = %d，want 0（不足一分时不该从余额扣钱）", amount)
	}

	// 余额也不能动 —— 余数机制的意义：零头留下，下次凑够再收。
	assertBalance(t, st, "u1", 1000, true)
	if rem := readRemainder(t, st, "u1"); rem != 4000 {
		t.Errorf("余数 = %d 微元，want 4000（0.004 元 = 4000 微元应完整留下）", rem)
	}
}

// TestCharge_SmallAmount_RepeatedSmallChargesStillCollect 钉住「删掉那条
// UPDATE 不会影响余数累积」：这是余数机制的核心不变量。
func TestCharge_SmallAmount_RepeatedSmallChargesStillCollect(t *testing.T) {
	ctx := context.Background()
	st := testStore(t, t.TempDir()+"/gw.db")
	mustUser(t, st, "u1", 1000, false)

	// 3 次 × 0.004 元 = 0.012 元 = 1.2 分 → 应跨过一分线，扣 1 分
	for i := range 3 {
		if err := st.ChargeBalance(ctx, "u1", 0.004, "r"+strconv.Itoa(i)); err != nil {
			t.Fatalf("charge %d: %v", i, err)
		}
	}
	// 扣 1 分；余数 2000 微元（0.012 - 0.01）
	assertBalance(t, st, "u1", 999, true)
	if rem := readRemainder(t, st, "u1"); rem != 2000 {
		t.Errorf("余数 = %d，want 2000", rem)
	}

	// 流水行：3 条，其中最后一条记录了实扣的 1 分
	var rows int
	var total int64
	if err := st.read.QueryRowContext(ctx,
		`SELECT COUNT(*), COALESCE(SUM(amount_cents),0) FROM balance_charges WHERE user_id = ?`,
		"u1").Scan(&rows, &total); err != nil {
		t.Fatalf("读流水: %v", err)
	}
	if rows != 3 || total != 1 {
		t.Errorf("流水 %d 条 / 合计 %d 分，want 3 / 1", rows, total)
	}
}

// TestCharge_SmallAmount_IdempotentRetryKeepsSingleRow 确认幂等在
// 「不足一分」这条路径上同样成立 —— 删掉那条 UPDATE 后，重试不得写出两行。
func TestCharge_SmallAmount_IdempotentRetryKeepsSingleRow(t *testing.T) {
	ctx := context.Background()
	st := testStore(t, t.TempDir()+"/gw.db")
	mustUser(t, st, "u1", 1000, false)

	for i := range 4 {
		if err := st.ChargeBalance(ctx, "u1", 0.004, "same-req"); err != nil {
			t.Fatalf("重试 %d: %v", i, err)
		}
	}
	var rows int
	if err := st.read.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM balance_charges WHERE request_id = ?`, "same-req").Scan(&rows); err != nil {
		t.Fatalf("读流水: %v", err)
	}
	if rows != 1 {
		t.Errorf("流水 %d 行, want 1（重试不得重复占位）", rows)
	}
	// 只该累计一次余数
	if rem := readRemainder(t, st, "u1"); rem != 4000 {
		t.Errorf("余数 = %d，want 4000（重试不得重复累加）", rem)
	}
}
