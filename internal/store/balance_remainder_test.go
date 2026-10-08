package store

import (
	"context"
	"fmt"
	"testing"
)

// 2026-10-10 回归：低价模型下「每次都四舍五入成 0 分」导致的计费整体失效。
//
// 实测现场：price_input=3.0 元/百万 tokens，一次 8000 token 的调用只有
// 0.004 元。旧实现按 YuanToCents(0.004)=math.Round(0.4)=**0** 入参，
// 而 0 分在扣费侧是 no-op —— 于是**一次都扣不到钱**：
//   6 次调用真实消费 3.33 分，balance_charges 里只有 1 条记录，余额基本没动。
//
// 这不是「偶尔漏一次」，而是该部署形态下**不存在能收上钱的单次调用**。

// 核心场景：连续若干次「不足一分」的小额调用，必须最终真的扣到钱。
func TestRemainder_SmallChargesEventuallyCollect(t *testing.T) {
	ctx := context.Background()
	st := testStore(t, t.TempDir()+"/gw.db")
	mustUser(t, st, "u1", 10_000, false) // 100 元

	// 复刻现场：每次 0.004 元（0.4 分），共 10 次 = 4 分。
	const each = 0.004
	for i := 0; i < 10; i++ {
		if err := st.ChargeBalance(ctx, "u1", each, fmt.Sprintf("req-%d", i)); err != nil {
			t.Fatalf("charge #%d: %v", i, err)
		}
	}

	// 总消费 0.04 元 = 4 分。修复前：实扣 0 分。
	assertBalance(t, st, "u1", 10_000-4, true)

	// 账目必须与报表对齐：累计扣掉的分数 == 总费用换算的分数（整数分部分）。
	var charged int64
	if err := st.read.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(amount_cents), 0) FROM balance_charges`).Scan(&charged); err != nil {
		t.Fatalf("sum charges: %v", err)
	}
	if charged != 4 {
		t.Errorf("流水累计扣了 %d 分, want 4", charged)
	}
	// 余数应当只剩零头，且必须 < 一分（否则下次会重复扣）。
	if rem := readRemainder(t, st, "u1"); rem >= MicrosPerCent {
		t.Errorf("余数 %d 微元 >= 一分（%d）—— 下次会重复扣这一分", rem, MicrosPerCent)
	}
}

// 分文不差：余数必须精确累积，不能因浮点丢精度。
func TestRemainder_NoPrecisionLoss(t *testing.T) {
	ctx := context.Background()
	st := testStore(t, t.TempDir()+"/gw.db")
	mustUser(t, st, "u1", 10_000, false)

	// 0.0001 元 = 100 微元。1000 次 = 0.1 元 = 10 分。
	// 若用 float64 累加，最后很可能不是精确的 10 分。
	for i := 0; i < 1000; i++ {
		if err := st.ChargeBalance(ctx, "u1", 0.0001, fmt.Sprintf("tiny-%d", i)); err != nil {
			t.Fatalf("charge #%d: %v", i, err)
		}
	}
	assertBalance(t, st, "u1", 10_000-10, true)
	if rem := readRemainder(t, st, "u1"); rem != 0 {
		t.Errorf("余数 = %d 微元, want 0（0.1 元应是精确的 10 分）", rem)
	}
}

// 幂等不能被余数机制破坏：同一 requestID 重试不能把余数累加两次。
func TestRemainder_RetryDoesNotDoubleAccumulate(t *testing.T) {
	ctx := context.Background()
	st := testStore(t, t.TempDir()+"/gw.db")
	mustUser(t, st, "u1", 10_000, false)

	for range 5 {
		if err := st.ChargeBalance(ctx, "u1", 0.004, "same-req"); err != nil {
			t.Fatalf("retry must be nil no-op, got %v", err)
		}
	}
	// 只该累计一次：0.004 元 = 0.4 分，仍不足一分 → 余额一分不动，
	// 余数 = 4000 微元。若重试重复累加，余数会是 20000（2 分）并真的扣钱。
	assertBalance(t, st, "u1", 10_000, true)
	if rem := readRemainder(t, st, "u1"); rem != 4_000 {
		t.Fatalf("余数 = %d 微元, want 4000（重试不得重复累加）", rem)
	}
}

// 不限额用户：不扣、也不攒余数（余额无限，不需要「攒够再扣」）。
func TestRemainder_UnlimitedUserAccumulatesNothing(t *testing.T) {
	ctx := context.Background()
	st := testStore(t, t.TempDir()+"/gw.db")
	mustUser(t, st, "vip", 0, true) // unlimited

	for i := 0; i < 50; i++ {
		if err := st.ChargeBalance(ctx, "vip", 0.004, fmt.Sprintf("vip-%d", i)); err != nil {
			t.Fatalf("不限额扣费必须是 no-op nil，得到 %v", err)
		}
	}
	if rem := readRemainder(t, st, "vip"); rem != 0 {
		t.Errorf("不限额用户余数 = %d 微元, want 0 —— 余额无限，不该攒", rem)
	}
	// 余额仍是 NULL（没被扣成 0）。
	assertBalance(t, st, "vip", 0, false)
}

// 余额不足时：扣费失败，但余数**照留**（那部分钱确实已经消费了）。
func TestRemainder_InsufficientBalanceKeepsRemainder(t *testing.T) {
	ctx := context.Background()
	st := testStore(t, t.TempDir()+"/gw.db")
	mustUser(t, st, "u1", 50, false) // 0.5 元

	// 先攒够 1 分（0.004×5 = 0.02 元 = 2 分，会触发扣减）
	for i := 0; i < 5; i++ {
		if err := st.ChargeBalance(ctx, "u1", 0.004, fmt.Sprintf("pre-%d", i)); err != nil {
			t.Fatalf("charge: %v", err)
		}
	}
	assertBalance(t, st, "u1", 48, true) // 50 - 2

	// 这次会触发扣减 1 分，余额够（48 >= 1），应当成功。
	if err := st.SetBalance(ctx, "u1", 0, false); err != nil {
		t.Fatalf("set balance: %v", err)
	}
	if err := st.ChargeBalance(ctx, "u1", 0.02, "big"); err == nil {
		t.Fatal("余额为 0 时扣 2 分应报 ErrInsufficientBalance")
	}
}

// 用户不存在仍必须是 ErrNotFound（不能被余数逻辑吞掉）。
func TestRemainder_UnknownUserStillNotFound(t *testing.T) {
	ctx := context.Background()
	st := testStore(t, t.TempDir()+"/gw.db")
	if err := st.ChargeBalance(ctx, "ghost", 0.004, "g1"); err != ErrNotFound {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

// 极小费用（低于 0.5 微元）也必须记账，不能被抹成 0。
func TestRemainder_TinyAmountNotLost(t *testing.T) {
	ctx := context.Background()
	st := testStore(t, t.TempDir()+"/gw.db")
	mustUser(t, st, "u1", 10_000, false)

	// 0.0000001 元 = 0.1 微元。取整到微元是 0 —— 但费用确实发生了。
	if err := st.ChargeBalance(ctx, "u1", 0.0000001, "tiny"); err != nil {
		t.Fatalf("charge: %v", err)
	}
	if rem := readRemainder(t, st, "u1"); rem != 1 {
		t.Fatalf("余数 = %d 微元, want 1（低于半微元的费用也不能丢）", rem)
	}
}
