package store

import (
	"context"
	"errors"
	"testing"
)

// 2026-10-10 P0 回归验证（临时，用完删除）。
//
//  1. 扣费失败后重试必须真的扣到钱（修复前：失败路径提交了占位行，
//     重试永远命中幂等快速路径，delta 恒为 0）。
func TestVerifyP0_RetryAfterInsufficientActuallyCharges(t *testing.T) {
	ctx := context.Background()
	st := testStore(t, t.TempDir()+"/gw.db")
	mustUser(t, st, "u1", 50, false) // 0.5 元

	// 余额不够 → 欠费
	if err := st.ChargeBalance(ctx, "u1", centsYuan(300), "req-1"); !errors.Is(err, ErrInsufficientBalance) {
		t.Fatalf("want ErrInsufficientBalance, got %v", err)
	}
	assertBalance(t, st, "u1", 50, true)

	// 充值后重试同一个 request_id：必须真的扣 300（修复前扣 0）。
	st.SetBalance(ctx, "u1", 10000, false)
	if err := st.ChargeBalance(ctx, "u1", centsYuan(300), "req-1"); err != nil {
		t.Fatalf("retry after top-up must succeed: %v", err)
	}
	assertBalance(t, st, "u1", 10000-300, true)
}

// 2. 失败路径不得留下占位行。
func TestVerifyP0_FailedChargeLeavesNoPlaceholder(t *testing.T) {
	ctx := context.Background()
	st := testStore(t, t.TempDir()+"/gw.db")
	mustUser(t, st, "u1", 50, false)

	if err := st.ChargeBalance(ctx, "u1", centsYuan(300), "req-x"); !errors.Is(err, ErrInsufficientBalance) {
		t.Fatalf("want ErrInsufficientBalance, got %v", err)
	}
	var n int
	if err := st.read.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM balance_charges WHERE request_id='req-x'`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatalf("失败扣费留下了 %d 条占位行 —— 重试将永远不扣钱", n)
	}
}

// 3. 用户不存在时同样不得留下占位行。
func TestVerifyP0_NotFoundLeavesNoPlaceholder(t *testing.T) {
	ctx := context.Background()
	st := testStore(t, t.TempDir()+"/gw.db")

	if err := st.ChargeBalance(ctx, "ghost", centsYuan(100), "req-ghost"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	var n int
	if err := st.read.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM balance_charges WHERE request_id='req-ghost'`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatalf("ErrNotFound 路径留下了 %d 条占位行", n)
	}
}

// 4. 跨用户同 request_id：两个用户都必须被扣（修复前第二个被静默跳过）。
func TestVerifyP0_CrossUserSameRequestIDBothCharged(t *testing.T) {
	ctx := context.Background()
	st := testStore(t, t.TempDir()+"/gw.db")
	mustUser(t, st, "ua", 10000, false)
	mustUser(t, st, "ub", 10000, false)

	if err := st.ChargeBalance(ctx, "ua", centsYuan(700), "shared-req"); err != nil {
		t.Fatalf("ua charge: %v", err)
	}
	if err := st.ChargeBalance(ctx, "ub", centsYuan(700), "shared-req"); err != nil {
		t.Fatalf("ub charge must succeed: %v", err)
	}

	assertBalance(t, st, "ua", 10000-700, true)
	assertBalance(t, st, "ub", 10000-700, true)
}

// 5. 同一用户的重试仍然幂等（复合主键没有破坏原有语义）。
func TestVerifyP0_SameUserRetryStillIdempotent(t *testing.T) {
	ctx := context.Background()
	st := testStore(t, t.TempDir()+"/gw.db")
	mustUser(t, st, "u1", 10000, false)

	if err := st.ChargeBalance(ctx, "u1", centsYuan(500), "idem"); err != nil {
		t.Fatalf("first charge: %v", err)
	}
	for i := 0; i < 3; i++ {
		if err := st.ChargeBalance(ctx, "u1", centsYuan(500), "idem"); err != nil {
			t.Fatalf("retry %d: %v", i, err)
		}
	}
	assertBalance(t, st, "u1", 10000-500, true) // 只扣一次
}

// 6. 老库升级：单列主键的存量表必须被重建为复合主键且**数据不丢**。
func TestVerifyP0_MigratesLegacyTableAndKeepsRows(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir() + "/gw.db"
	st := testStore(t, dir)

	// 造一个「旧 schema」：单列 request_id 主键 + 三行历史。
	st.db.Exec(`DROP TABLE balance_charges`)
	if _, err := st.db.Exec(`CREATE TABLE balance_charges (
		request_id   TEXT PRIMARY KEY,
		user_id      TEXT NOT NULL,
		amount_cents INTEGER NOT NULL,
		ts           INTEGER NOT NULL)`); err != nil {
		t.Fatalf("create legacy table: %v", err)
	}
	if _, err := st.db.Exec(`CREATE INDEX idx_balance_charges_user ON balance_charges(user_id, ts)`); err != nil {
		t.Fatalf("create legacy index: %v", err)
	}
	for i, u := range []string{"ua", "ub", "uc"} {
		if _, err := st.db.Exec(
			`INSERT INTO balance_charges VALUES (?,?,?,?)`,
			"legacy-"+u, u, 100+i, 1700000000000); err != nil {
			t.Fatalf("seed legacy row: %v", err)
		}
	}
	st.Close()

	// 重新 Open：迁移应重建表并保留全部三行。
	st2 := testStore(t, dir)
	var n int
	if err := st2.read.QueryRowContext(ctx, `SELECT COUNT(*) FROM balance_charges`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 3 {
		t.Fatalf("迁移后应保留 3 行，实际 %d —— 老账目被丢了", n)
	}
	// 索引必须一并重建。
	if _, err := st2.read.ExecContext(ctx,
		`SELECT amount_cents FROM balance_charges WHERE request_id=? AND user_id=?`,
		"legacy-ub", "ub"); err != nil {
		t.Fatalf("复合主键查询失败（索引可能没重建）: %v", err)
	}
	// 迁移后同 request_id 跨用户不再互相取消。
	//
	// 用 request_id="legacy-ua"（存量行属于 ua）去扣 **ub** 的钱：
	// 修复前单列主键会让这一笔被静默跳过，ub 一分不少。
	mustUser(t, st2, "ua", 5000, false)
	mustUser(t, st2, "ub", 5000, false)
	if err := st2.ChargeBalance(ctx, "ub", centsYuan(100), "legacy-ua"); err != nil {
		t.Fatalf("post-migration cross-user charge must succeed: %v", err)
	}
	assertBalance(t, st2, "ub", 5000-100, true)
	assertBalance(t, st2, "ua", 5000, true) // 别人的 request_id 不该动到它

	// 同一用户重复扣仍幂等（复合主键没破坏原语义）。
	if err := st2.ChargeBalance(ctx, "ub", centsYuan(100), "legacy-ua"); err != nil {
		t.Fatalf("same-user retry: %v", err)
	}
	assertBalance(t, st2, "ub", 5000-100, true)
}
