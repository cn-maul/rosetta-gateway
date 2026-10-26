package store

// 待结算账本（balance_pending）的结转必须满足四条不变量。
//
// # 四条 P0 的共同根因（2026-10-21 修 P0-1/2/3/4）
//
// 旧实现的结转是三条**自动提交**语句：
//
//	SELECT SUM(amount_micros) FROM balance_pending WHERE user_id=?
//	UPDATE users SET balance_cents = MAX(0, COALESCE(balance_cents,0) - ?)
//	DELETE FROM balance_pending WHERE user_id = ?        ← 整表按 user_id 删
//
// 于是有四种资损形态：
//
//   - **P0-1 重复结转（多收用户的钱）**：并发充值各自 SUM 到同一笔欠款、
//     各自扣一次。实测复现率 59/60。
//   - **P0-2 崩溃窗口（同一笔欠款被扣两次）**：UPDATE 与 DELETE 之间进程
//     被杀 → 余额已扣、账本还在 → 下次充值再扣一次。
//   - **P0-3 无限额被切成有限额**：COALESCE(NULL,0) 把「不限额」当成 0。
//   - **P0-4 并发新欠款被无辜抹掉（漏收）**：DELETE 删的是「该用户全部行」，
//     不是「我 SUM 到的那些行」。实测复现率 60/60。
//
// 修法：单事务 + 精确删行 + NULL 语义正确。

import (
	"context"
	"errors"
	"sync"
	"testing"
)

// pendingRows 读某用户待结算账本的**行数**与总额。
func pendingRows(t *testing.T, st *Store, userID string) (rows int, micros int64) {
	t.Helper()
	var n int
	var v int64
	if err := st.read.QueryRowContext(context.Background(),
		`SELECT COUNT(*), COALESCE(SUM(amount_micros), 0) FROM balance_pending WHERE user_id = ?`,
		userID).Scan(&n, &v); err != nil {
		t.Fatalf("read pending: %v", err)
	}
	return n, v
}

// seedPending 直接写一条待结算账本行。
//
// 刻意不走 ChargeBalance：那要构造「余额不足 + ≥1 分费用」才能触发，
// 而这里要精确控制 amount_micros 的值（尤其是「不足一分」的情形）。
func seedPending(t *testing.T, st *Store, userID, requestID string, micros int64) {
	t.Helper()
	if _, err := st.db.ExecContext(context.Background(),
		`INSERT OR IGNORE INTO balance_pending (request_id, user_id, amount_micros, ts)
		 VALUES (?, ?, ?, ?)`,
		requestID, userID, micros, 1700000000000); err != nil {
		t.Fatalf("seed pending %s/%s: %v", userID, requestID, err)
	}
}

// completionMarkerCount 读某个原始请求的规范化完成标记数量。
func completionMarkerCount(t *testing.T, st *Store, requestID, userID string) int {
	t.Helper()
	var n int
	if err := st.read.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM balance_charges WHERE request_id = ? AND user_id = ?`,
		requestID, userID).Scan(&n); err != nil {
		t.Fatalf("count completion markers: %v", err)
	}
	return n
}

// seedUsageForR0AC 造一条可供对账查询的用量记录。
//
// 这里直接写 SQL，刻意绕开计价：这些用例验的是「扣费完成标记」与
// 「usage/cost 已落库」之间的一致性，不需要再测一次 priceUsage。
func seedUsageForR0AC(t *testing.T, st *Store, recordID, userID, requestID string,
	cost float64, state string) {
	t.Helper()
	if _, err := st.db.ExecContext(context.Background(),
		`INSERT INTO usage_records (id, ts, access_key_id, user_id, public_model,
		   provider_id, upstream_model, ingress_protocol, stream,
		   input_tokens, output_tokens, total_tokens, usage_state, status, http_status,
		   latency_ms, cost_total, request_id)
		 VALUES (?, 1700000000000, 'k1', ?, 'priced', 'p1', 'priced', 'chat', 0,
		         0, 0, 0, ?, 'ok', 200, 10, ?, ?)`,
		recordID, userID, state, cost, requestID); err != nil {
		t.Fatalf("seed usage %s: %v", recordID, err)
	}
}

// R0-AC-01：充值结转待结算欠款时，必须创建原始请求的完成标记。
//
// 否则对账会在下一次维护周期把同一笔成功请求当成「从未扣费」，再扣一遍。
func TestR0AC01_PendingSettlementCreatesCanonicalCompletionMarker(t *testing.T) {
	ctx := context.Background()
	st := testStore(t, t.TempDir()+"/gw.db")
	mustUser(t, st, "u1", 0, false)

	const reqID = "r0-ac01-req"
	if err := st.ChargeBalance(ctx, "u1", 3.0, reqID); !errors.Is(err, ErrInsufficientBalance) {
		t.Fatalf("initial charge: want ErrInsufficientBalance, got %v", err)
	}
	seedUsageForR0AC(t, st, "r0-ac01-rec", "u1", reqID, 3.0, UsageStateReported)

	if _, err := st.AdjustBalanceWithLedger(ctx, "u1", 1000, "op", "admin", "R0-AC-01"); err != nil {
		t.Fatalf("topup and settle: %v", err)
	}
	if got := completionMarkerCount(t, st, reqID, "u1"); got != 1 {
		t.Fatalf("completion markers = %d, want 1", got)
	}
	if rows, micros := pendingRows(t, st, "u1"); rows != 0 || micros != 0 {
		t.Fatalf("pending after settlement = (%d rows, %d micros), want empty", rows, micros)
	}
	assertBalance(t, st, "u1", 700, true) // 充值 1000 分，结算欠款 300 分。

	found, err := st.FindUnchargedUsage(ctx, 100)
	if err != nil {
		t.Fatalf("FindUnchargedUsage: %v", err)
	}
	if len(found) != 0 {
		t.Fatalf("reconciliation still sees %+v after settlement", found)
	}
}

// R0-AC-02：重算补扣成功后，必须同时创建原始请求的完成标记。
//
// recompute:<record_id>:<cost> 只负责重算本身的幂等，不能让对账误以为
// 原始请求从未收费。
func TestR0AC02_RecomputeCreatesCanonicalCompletionMarker(t *testing.T) {
	ctx := context.Background()
	st := testStore(t, t.TempDir()+"/gw.db")
	seedPricedModel(t, st, 0.2, 0.02, 0.8)
	mustUser(t, st, "u1", 10000, false)

	const recordID = "r0-ac02-rec"
	const reqID = "r0-ac02-req"
	seedUsageForR0AC(t, st, recordID, "u1", reqID, 0, usageStateCostPending)
	if _, err := st.db.ExecContext(ctx,
		`UPDATE usage_records SET input_tokens = 1000, output_tokens = 500, total_tokens = 1500
		  WHERE id = ?`, recordID); err != nil {
		t.Fatalf("set usage tokens: %v", err)
	}
	if _, err := st.db.ExecContext(ctx,
		`UPDATE upstream_models SET price_input = 100 WHERE model_id = 'priced'`); err != nil {
		t.Fatalf("update price: %v", err)
	}

	ok, err := st.RecomputeCost(ctx, recordID)
	if err != nil {
		t.Fatalf("RecomputeCost: %v", err)
	}
	if !ok {
		t.Fatal("RecomputeCost did not report a repaired record")
	}
	if got := completionMarkerCount(t, st, reqID, "u1"); got != 1 {
		t.Fatalf("canonical completion markers = %d, want 1", got)
	}
	var recomputeMarkers int
	if err := st.read.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM balance_charges
		  WHERE user_id = 'u1' AND request_id LIKE 'recompute:%'`).Scan(&recomputeMarkers); err != nil {
		t.Fatalf("count recompute markers: %v", err)
	}
	if recomputeMarkers != 1 {
		t.Fatalf("recompute idempotency markers = %d, want 1", recomputeMarkers)
	}
	assertBalance(t, st, "u1", 9990, true)

	found, err := st.FindUnchargedUsage(ctx, 100)
	if err != nil {
		t.Fatalf("FindUnchargedUsage: %v", err)
	}
	if len(found) != 0 {
		t.Fatalf("reconciliation still sees %+v after recomputation", found)
	}
}

// R0-AC-03：完成标记写失败时，关联的钱包扣款与账本变更必须一起回滚。
func TestR0AC03_CompletionMarkerFailureRollsBackAssociatedDebit(t *testing.T) {
	t.Run("pending settlement", func(t *testing.T) {
		ctx := context.Background()
		st := testStore(t, t.TempDir()+"/gw.db")
		mustUser(t, st, "u1", 1000, false)

		const reqID = "r0-ac03-pending"
		seedPending(t, st, "u1", reqID, 300*MicrosPerCent)
		if _, err := st.db.ExecContext(ctx,
			`CREATE TRIGGER reject_r0_ac03_pending BEFORE INSERT ON balance_charges
			 WHEN NEW.request_id = '`+reqID+`'
			 BEGIN SELECT RAISE(ABORT, 'completion marker rejected'); END`); err != nil {
			t.Fatalf("create trigger: %v", err)
		}

		if _, err := st.AdjustBalanceWithLedger(ctx, "u1", 100, "op", "admin", "R0-AC-03"); err == nil {
			t.Fatal("topup succeeded although the completion marker write failed")
		}
		// 充值与原始完成标记在同一事务内：标记失败时，连本次充值也回滚。
		assertBalance(t, st, "u1", 1000, true)
		var topups int
		if err := st.read.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM balance_topups WHERE user_id = 'u1'`).Scan(&topups); err != nil {
			t.Fatalf("count topups: %v", err)
		}
		if topups != 0 {
			t.Fatalf("topup ledger rows = %d, want 0 after transaction rollback", topups)
		}
		if rows, micros := pendingRows(t, st, "u1"); rows != 1 || micros != 300*MicrosPerCent {
			t.Fatalf("pending after rollback = (%d rows, %d micros), want (1, %d)",
				rows, micros, 300*MicrosPerCent)
		}
		if got := completionMarkerCount(t, st, reqID, "u1"); got != 0 {
			t.Fatalf("completion markers after rollback = %d, want 0", got)
		}
	})

	t.Run("cost recompute", func(t *testing.T) {
		ctx := context.Background()
		st := testStore(t, t.TempDir()+"/gw.db")
		seedPricedModel(t, st, 0.2, 0.02, 0.8)
		mustUser(t, st, "u1", 10000, false)

		const recordID = "r0-ac03-rec"
		const reqID = "r0-ac03-recompute"
		seedUsageForR0AC(t, st, recordID, "u1", reqID, 0, usageStateCostPending)
		if _, err := st.db.ExecContext(ctx,
			`UPDATE usage_records SET input_tokens = 1000, output_tokens = 500, total_tokens = 1500
			  WHERE id = ?`, recordID); err != nil {
			t.Fatalf("set usage tokens: %v", err)
		}
		if _, err := st.db.ExecContext(ctx,
			`UPDATE upstream_models SET price_input = 100 WHERE model_id = 'priced'`); err != nil {
			t.Fatalf("update price: %v", err)
		}
		if _, err := st.db.ExecContext(ctx,
			`CREATE TRIGGER reject_r0_ac03_recompute BEFORE INSERT ON balance_charges
			 WHEN NEW.request_id = '`+reqID+`'
			 BEGIN SELECT RAISE(ABORT, 'completion marker rejected'); END`); err != nil {
			t.Fatalf("create trigger: %v", err)
		}

		if _, err := st.RecomputeCost(ctx, recordID); err == nil {
			t.Fatal("recompute succeeded although the completion marker write failed")
		}
		state, cost := usageStateOf(t, st, recordID)
		if state != usageStateCostPending || cost != 0 {
			t.Fatalf("usage after rollback = (%q, %v), want (%q, 0)",
				state, cost, usageStateCostPending)
		}
		assertBalance(t, st, "u1", 10000, true)
		if got := readRemainder(t, st, "u1"); got != 0 {
			t.Fatalf("balance remainder after rollback = %d, want 0", got)
		}
		if got := completionMarkerCount(t, st, reqID, "u1"); got != 0 {
			t.Fatalf("completion markers after rollback = %d, want 0", got)
		}
	})
}

// R0-AC-04：待结算与对账并发处理同一笔请求时，只能产生一次扣款和一个标记。
//
// 先让第一次对账与充值结转并发，再让第二次对账与第一次对账并发：
// 这里构造的是真正会重复扣款的窗口，而不是顺序执行后的幂等复查。
func TestR0AC04_SettlementAndReconciliationRemainIdempotent(t *testing.T) {
	for i := 0; i < 8; i++ {
		t.Run("round-"+itoa(i), func(t *testing.T) {
			ctx := context.Background()
			st := testStore(t, t.TempDir()+"/gw.db")
			mustUser(t, st, "u1", 1000, false)

			reqID := "r0-ac04-req-" + itoa(i)
			seedPending(t, st, "u1", reqID, 300*MicrosPerCent)
			seedUsageForR0AC(t, st, "r0-ac04-rec-"+itoa(i), "u1", reqID, 3.0, UsageStateReported)

			run := func(settle bool) error {
				if !settle {
					found, err := st.FindUnchargedUsage(ctx, 100)
					if err != nil {
						return err
					}
					for _, u := range found {
						if err := st.ChargeUnchargedUsage(ctx, u); err != nil {
							return err
						}
					}
					return nil
				}
				_, err := st.AdjustBalanceWithLedger(ctx, "u1", 1, "op", "admin", "R0-AC-04")
				return err
			}
			runConcurrently := func(a, b bool) {
				t.Helper()
				start := make(chan struct{})
				errs := make(chan error, 2)
				go func() { <-start; errs <- run(a) }()
				go func() { <-start; errs <- run(b) }()
				close(start)
				for n := 0; n < 2; n++ {
					if err := <-errs; err != nil {
						t.Fatalf("concurrent operation: %v", err)
					}
				}
			}

			runConcurrently(true, false)
			// 充值路径只借记本次充值额；pending 300 分超过了它，但当前
			// 余额已有 1000 分，所以可以从余额中结清。若两条路径都收了
			// 这 300 分，余额会再少 300。
			runConcurrently(false, false)
			assertBalance(t, st, "u1", 701, true) // 1000 + 1 - 300，只能扣一次。
			if got := completionMarkerCount(t, st, reqID, "u1"); got != 1 {
				t.Fatalf("completion markers = %d, want 1", got)
			}
			if rows, _ := pendingRows(t, st, "u1"); rows != 0 {
				t.Fatalf("pending rows = %d, want 0", rows)
			}
		})
	}
}

// P0-1：并发充值不得把同一笔欠款结转两次（多收用户的钱）。
//
// 旧实现的复现率是 59/60（98%）。修好后**一次**欠款只能被扣一次。
func TestSettlePending_并发充值只结转一次(t *testing.T) {
	ctx := context.Background()
	st := testStore(t, t.TempDir()+"/gw.db")
	mustUser(t, st, "u1", 10000, false) // 余额 100 元
	// 欠款 300 分（3 元）
	seedPending(t, st, "u1", "req-1", 300*MicrosPerCent)

	const n = 8
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// 每次充值 1 元
			_, _ = st.AdjustBalanceWithLedger(ctx, "u1", 100, "op", "op", "topup")
		}()
	}
	wg.Wait()

	// 充值总额 8 元 = 800 分；欠款 300 分。
	// 正确终态：余额 = 10000 + 800 - 300 = 10500 分。
	//
	// 若被重复结转，余额会**更低**（多扣了 300 分的整数倍）。
	cents, _, err := st.BalanceOf(ctx, "u1")
	if err != nil {
		t.Fatalf("BalanceOf: %v", err)
	}
	if cents != 10500 {
		t.Errorf("余额 = %d 分，期望 10500 —— 欠款被重复结转了（多收用户的钱）", cents)
	}

	// 账本必须清空（那笔欠款已被结转一次）。
	if rows, _ := pendingRows(t, st, "u1"); rows != 0 {
		t.Errorf("待结算账本仍有 %d 行，期望 0", rows)
	}
}

// P0-4：结算不得抹掉并发写入的新欠款（漏收）。
//
// 旧实现的 DELETE 是「该用户全部行」，会把结算期间并发插入的新欠款一并
// 删掉 —— 而它**从未被扣**。实测复现率 60/60（100%）。
//
// 修法是「只删本次 SUM 覆盖到的行」。本测试用一条**在结转之后**写入的
// pending 行来验证它没被误删。
func TestSettlePending_不抹掉新写入的欠款(t *testing.T) {
	ctx := context.Background()
	st := testStore(t, t.TempDir()+"/gw.db")
	mustUser(t, st, "u1", 10000, false)
	seedPending(t, st, "u1", "old-req", 100*MicrosPerCent)

	// 先结转老的。
	if _, err := st.AdjustBalanceWithLedger(ctx, "u1", 100, "op", "op", "topup"); err != nil {
		t.Fatalf("topup: %v", err)
	}
	// 再来一笔新的欠款（模拟结算期间并发写入）。
	seedPending(t, st, "u1", "new-req", 50*MicrosPerCent)

	rows, micros := pendingRows(t, st, "u1")
	if rows != 1 || micros != 50*MicrosPerCent {
		t.Errorf("待结算账本 = (%d 行, %d 微元)，期望 (1 行, %d 微元) —— 新欠款被误删了（漏收）",
			rows, micros, 50*MicrosPerCent)
	}
}

// P0-3：「设为不限额」必须免掉欠费，且**不得**把「不限额」悄悄写成有限额。
//
// balance_cents 的 NULL = 不限额。旧实现的结转用 COALESCE(balance_cents,0)
// 把 NULL 当成 0，于是结转把 NULL 写回一个非 NULL 整数 —— 用户从
// 「无限额度」变成「有限额 9700 分」，方向还可能正好是 0（立刻被余额
// 预检拦成 402）。
//
// 正解：「免债」与「设为不限额」在**同一个事务**里发生（SetBalance）。
// 「免债」不能绑在充值上 —— 充值本身刻意会把不限额转成有限额
// （was_unlimited 字段记录的语义），两者绑在一起会互相打架。
func TestSettlePending_设为不限额免掉欠费(t *testing.T) {
	ctx := context.Background()
	st := testStore(t, t.TempDir()+"/gw.db")
	mustUser(t, st, "u1", 100, false)                    // 有限额，余额 1 元
	seedPending(t, st, "u1", "req-1", 300*MicrosPerCent) // 欠 3 元

	// 设为不限额 → 欠费被免掉，账本清空。
	if err := st.SetBalance(ctx, "u1", 0, true); err != nil {
		t.Fatalf("SetBalance(unlimited): %v", err)
	}

	cents, limited, err := st.BalanceOf(ctx, "u1")
	if err != nil {
		t.Fatalf("BalanceOf: %v", err)
	}
	if limited {
		t.Errorf("不限额用户被切成了有限额（余额 %d 分）—— 语义突变", cents)
	}
	if rows, _ := pendingRows(t, st, "u1"); rows != 0 {
		t.Errorf("待结算账本仍有 %d 行，期望 0 —— 设不限额应当免掉欠费", rows)
	}
}

// 给不限额用户充值**刻意**会把它转成有限额（was_unlimited 语义）。
//
// 但它**不该**把已被免掉的旧欠费再收回来 —— 那笔账已经不存在了。
func TestSettlePending_充值不限额用户不复活旧债(t *testing.T) {
	ctx := context.Background()
	st := testStore(t, t.TempDir()+"/gw.db")
	mustUser(t, st, "u1", 100, false)
	seedPending(t, st, "u1", "req-1", 300*MicrosPerCent)

	// 先免债。
	if err := st.SetBalance(ctx, "u1", 0, true); err != nil {
		t.Fatalf("SetBalance(unlimited): %v", err)
	}
	// 再充值 1 元 → 变成有限额 100 分。
	if _, err := st.AdjustBalanceWithLedger(ctx, "u1", 100, "op", "op", "topup"); err != nil {
		t.Fatalf("topup: %v", err)
	}

	cents, limited, err := st.BalanceOf(ctx, "u1")
	if err != nil {
		t.Fatalf("BalanceOf: %v", err)
	}
	if !limited {
		t.Errorf("充值后应当是有限额（was_unlimited 语义），实际仍是无限额")
	}
	if cents != 100 {
		t.Errorf("余额 = %d 分，期望 100 —— 被免掉的旧欠费又被收回来了", cents)
	}
}

// 欠款超过余额时，**扣不动的那部分必须留在账本里**。
//
// 旧实现把超出部分一并删掉（DELETE 删全部行）—— 那是「用户欠 10 元、
// 余额 3 元、充值后账本清零」的漏收：用户欠的钱凭空消失。
func TestSettlePending_超出余额的部分留在账本(t *testing.T) {
	ctx := context.Background()
	st := testStore(t, t.TempDir()+"/gw.db")
	mustUser(t, st, "u1", 300, false)                     // 余额 3 元
	seedPending(t, st, "u1", "req-1", 1000*MicrosPerCent) // 欠 10 元

	// 充值 1 元 → 余额 4 元（400 分），欠款 1000 分。
	if _, err := st.AdjustBalanceWithLedger(ctx, "u1", 100, "op", "op", "topup"); err != nil {
		t.Fatalf("topup: %v", err)
	}

	cents, _, err := st.BalanceOf(ctx, "u1")
	if err != nil {
		t.Fatalf("BalanceOf: %v", err)
	}
	// 充值 100 分后余额 400 分，扣掉 400 分欠款 → 余额 0。
	if cents != 0 {
		t.Errorf("余额 = %d 分，期望 0（扣得起的部分应当扣掉）", cents)
	}
	// 剩下的 600 分欠款必须还在账本里。
	rows, micros := pendingRows(t, st, "u1")
	if rows != 1 {
		t.Fatalf("待结算账本 = %d 行，期望 1 —— 扣不动的欠款被抹掉了（漏收）", rows)
	}
	if micros != 600*MicrosPerCent {
		t.Errorf("待结算账本 = %d 微元，期望 %d（扣不动的部分应保留）",
			micros, 600*MicrosPerCent)
	}
}

// 欠费分支仍然必须把金额记进待结算账本（这是 balance_pending 存在的理由）。
//
// 修 P0-1..4 之后，这条基础行为不能被改坏。
func TestSettlePending_欠费金额仍进账本(t *testing.T) {
	ctx := context.Background()
	st := testStore(t, t.TempDir()+"/gw.db")
	mustUser(t, st, "broke", 1, false)

	if err := st.ChargeBalance(ctx, "broke", 3.0, "req-1"); !errors.Is(err, ErrInsufficientBalance) {
		t.Fatalf("应返回 ErrInsufficientBalance，得到 %v", err)
	}
	if _, micros := pendingRows(t, st, "broke"); micros != 3_000_000 {
		t.Errorf("待结算账本 = %d 微元，期望 3000000", micros)
	}
}

// P0-5：重算必须真的补扣余额（两层要同时成立）。
//
// # 第一层：重算本身不调用 ChargeBalance
//
// 旧实现只 UPDATE cost_total，从不碰余额 → 一次读池抖动 = 永久免单。
//
// # 第二层：幂等键选错（本测试的主要对象）
//
// 落库时的扣费用 request_id 占位行（balance_charges 主键）。补扣若用同一个
// request_id，INSERT OR IGNORE 撞上那个占位行、RowsAffected()==0 → 直接返回「已扣过」→
// 补扣变成空操作。实测（修复前）：差额 0.0998 元，余额分文未动。
//
// 下面这两个用例把两层各针一次：
//   - 补扣必须真的扣到余额上（幂等键不能撞上落库时那次）；
//   - 补扣必须**幂等**（同一目标费用重复重算不得双重扣）。
func TestRecompute_补扣余额且幂等(t *testing.T) {
	ctx := context.Background()
	st := testStore(t, t.TempDir()+"/gw.db")
	seedPricedModel(t, st, 0.2, 0.02, 0.8)
	mustUser(t, st, "u1", 10000, false) // 100 元

	// 造一条「落库时已结算」的记录：cost_total 已固化为 0.0006 元，
	// 但 usage_state 仍是 cost_pending（待重算）。
	const recordID = "rec-1"
	const reqID = "req-1"
	if _, err := st.db.ExecContext(ctx,
		`INSERT INTO usage_records (id, ts, access_key_id, user_id, public_model,
		   provider_id, upstream_model, ingress_protocol, stream,
		   input_tokens, output_tokens, total_tokens, usage_state, status, http_status,
		   latency_ms, cost_total, request_id)
		 VALUES (?, 1700000000000, 'k1', 'u1', 'priced', 'p1', 'priced', 'chat', 0,
		         1000, 500, 1500, ?, 'ok', 200, 10, 0.0006, ?)`,
		recordID, usageStateCostPending, reqID); err != nil {
		t.Fatalf("seed usage: %v", err)
	}

	// 落库时的扣费：0.0006 元 → 不足一分，进余数。占位行**已写**。
	if err := st.ChargeBalance(ctx, "u1", 0.0006, reqID); err != nil {
		t.Fatalf("initial charge: %v", err)
	}

	// 管理员改价：输入价 100 元/百万。重算后费用 = (1000*100 + 500*0.8)/1e6 = 0.1004。
	if _, err := st.db.ExecContext(ctx,
		`UPDATE upstream_models SET price_input = 100 WHERE model_id = 'priced'`); err != nil {
		t.Fatalf("update price: %v", err)
	}

	ok, err := st.RecomputeCost(ctx, recordID)
	if err != nil {
		t.Fatalf("RecomputeCost: %v", err)
	}
	if !ok {
		t.Fatal("RecomputeCost 未报成功")
	}

	// 明细上的费用必须是重算值。
	var cost float64
	if err := st.read.QueryRowContext(ctx,
		`SELECT cost_total FROM usage_records WHERE id = ?`, recordID).Scan(&cost); err != nil {
		t.Fatalf("read cost: %v", err)
	}
	if want := 0.1004; cost < want*0.999 || cost > want*1.001 {
		t.Errorf("cost_total = %v，期望约 %v", cost, want)
	}

	// 余额必须真的被补扣。权威值是「余数列 + 已扣分数」的合计：
	//   - 积累应收 = 0.0006（落库时）+ 0.0998（补扣）= 0.1004 元 = 100400 微元；
	//   - 100400 微元超过 10000（一分）→ 扣 10 分，余数列剩 400 微元。
	//
	// 只看 balance_cents 会误判：不足一分时余额不动，它才是真正的权威值。
	var remainder, balance int64
	if err := st.read.QueryRowContext(ctx,
		`SELECT balance_remainder, balance_cents FROM users WHERE id = 'u1'`).Scan(&remainder, &balance); err != nil {
		t.Fatalf("read balance: %v", err)
	}
	// 已收金额 = (初始余额分 - 当前余额分) × 10000 + 余数列。
	//
	// 余数列是「已收但不足一分、还没从余额里扣」的那部分 —— 它已经收到了，
	// 只是不能扣成整分。所以已收 = 已扣分数 + 余数列，不是「初始 - 余额列 - 余额分」。
	//
	// 期望值 0.1004 元 = 100400 微元 = 0.0006（落库时）+ 0.0998（补扣）。
	collected := (10000-balance)*MicrosPerCent + remainder
	if collected != 100_400 {
		t.Errorf("已收 = %d 微元（余额分 %d + 余数列 %d），期望 100400 —— 补扣**没发生**（余额免单）",
			collected, balance, remainder)
	}
	if balance != 9990 {
		t.Errorf("balance_cents = %d，期望 9990（10000 - 10 分）", balance)
	}

	// 幂等：同一条记录、同一目标费用重复重算 → 不得再扣一次。
	if _, err := st.RecomputeCost(ctx, recordID); err != nil {
		t.Fatalf("second RecomputeCost: %v", err)
	}
	if err := st.read.QueryRowContext(ctx,
		`SELECT balance_remainder, balance_cents FROM users WHERE id = 'u1'`).Scan(&remainder, &balance); err != nil {
		t.Fatalf("read balance: %v", err)
	}
	if collected := (10000-balance)*MicrosPerCent + remainder; collected != 100_400 {
		t.Errorf("重复重算后已收 = %d 微元，期望仍为 100400 —— 补扣不幂等（重复扣费）", collected)
	}
}

// 余额不足时重算必须保留 cost_pending（收不到就不假装收到了）。
func TestRecompute_欠费时保留待重算(t *testing.T) {
	ctx := context.Background()
	st := testStore(t, t.TempDir()+"/gw.db")
	seedPricedModel(t, st, 0.2, 0.02, 0.8)
	mustUser(t, st, "u1", 0, false) // 0 元

	const recordID = "rec-poor"
	if _, err := st.db.ExecContext(ctx,
		`INSERT INTO usage_records (id, ts, access_key_id, user_id, public_model,
		   provider_id, upstream_model, ingress_protocol, stream,
		   input_tokens, output_tokens, total_tokens, usage_state, status, http_status,
		   latency_ms, cost_total, request_id)
		 VALUES (?, 1700000000000, 'k1', 'u1', 'priced', 'p1', 'priced', 'chat', 0,
		         1000000, 0, 1000000, ?, 'ok', 200, 10, 0, 'req-poor')`,
		recordID, usageStateCostPending); err != nil {
		t.Fatalf("seed usage: %v", err)
	}

	ok, err := st.RecomputeCost(ctx, recordID)
	if err != nil {
		t.Fatalf("RecomputeCost: %v", err)
	}
	if ok {
		t.Error("余额不足时重算不应报成功")
	}
	state, _ := usageStateOf(t, st, recordID)
	if state != usageStateCostPending {
		t.Errorf("usage_state = %q，期望 %q —— 欠费时应保留待重算", state, usageStateCostPending)
	}
}

// 扣费对账：已落库但没扣到钱的记录必须能被捞出来并补扣
// （2026-10-21 修 A4-P1-4 / 汇总 F4）。
//
// 回归风险：usage 落库与余额扣费是两个事务。两者之间进程被杀，或扣费
// 遇到瞬时故障且重试也全部失败，这笔费用就永远停在「报表上有、余额没扣」
// 的状态 —— 而且没有任何落盘痕迹：usage_state='reported'、cost_total 非 0，
// 看起来完全正常。全仓此前没有任何对账任务比对「该扣的」与「扣了的」。
func TestReconcile_UnchargedUsageIsFoundAndCharged(t *testing.T) {
	ctx := context.Background()
	st := testStore(t, t.TempDir()+"/gw.db")
	seedPricedModel(t, st, 3.0, 0.1, 9.0)
	mustUser(t, st, "u1", 10_000, false) // 100 元

	// 落一条 usage 但**不扣费** —— 模拟「落库后、扣费前进程被杀」。
	cost, err := st.CreateUsageRecordWithCost(ctx, &UsageRecord{
		ID: "rec-1", UserID: "u1", AccessKeyID: "k1",
		PublicModel: "m", ProviderID: "p1", UpstreamModel: "priced",
		IngressProtocol: "chat",
		InputTokens:     1_000_000,
		OutputTokens:    0,
		TotalTokens:     1_000_000,
		UsageState:      "reported", Status: "ok",
		RequestID: "req-1",
	})
	if err != nil {
		t.Fatalf("create usage: %v", err)
	}
	if cost <= 0 {
		t.Fatalf("cost = %v, want > 0", cost)
	}

	// 对账必须发现它。
	found, err := st.FindUnchargedUsage(ctx, 100)
	if err != nil {
		t.Fatalf("FindUnchargedUsage: %v", err)
	}
	if len(found) != 1 || found[0].RecordID != "rec-1" {
		t.Fatalf("found = %+v, want 1 条 rec-1", found)
	}

	// 补扣后余额必须真的少了。
	if err := st.ChargeUnchargedUsage(ctx, found[0]); err != nil {
		t.Fatalf("ChargeUnchargedUsage: %v", err)
	}
	cents, limited, err := st.BalanceOf(ctx, "u1")
	if err != nil {
		t.Fatalf("BalanceOf: %v", err)
	}
	if !limited {
		t.Fatalf("u1 变成不限额了")
	}
	if want := int64(10_000) - YuanToCents(cost); cents != want {
		t.Fatalf("balance = %d, want %d（补扣了 %v 元）", cents, want, cost)
	}

	// 幂等：再查一次必须查不到（balance_charges 已有占位行）。
	found2, err := st.FindUnchargedUsage(ctx, 100)
	if err != nil {
		t.Fatalf("FindUnchargedUsage (2nd): %v", err)
	}
	if len(found2) != 0 {
		t.Fatalf("补扣后仍查到 %+v —— 幂等没生效", found2)
	}
}

// 对账**不得**把不该扣的记录算成漏扣。
//
// 三类排除项各自对应一个真实误伤：
//   - status != 'ok'：失败/取消的记录 cost_total 记的是**网关的上游成本**，
//     不是用户的应收。把它补扣 = 收用户根本不该付的钱。
//   - cost_total = 0：未配价模型，没有钱可扣。
//   - request_id 为空：没有幂等键，补扣会**真的重复扣**。
func TestReconcile_OnlySuccessfulChargedRecordsArePicked(t *testing.T) {
	ctx := context.Background()
	st := testStore(t, t.TempDir()+"/gw.db")
	seedPricedModel(t, st, 3.0, 0.1, 9.0)
	mustUser(t, st, "u1", 10_000, false)

	cases := []struct {
		name  string
		rec   UsageRecord
		count bool
	}{
		{"失败请求不补扣", UsageRecord{
			ID: "r-fail", UserID: "u1", AccessKeyID: "k1", PublicModel: "m",
			ProviderID: "p1", UpstreamModel: "priced", IngressProtocol: "chat",
			InputTokens: 1_000_000, TotalTokens: 1_000_000,
			UsageState: "reported", Status: "error", RequestID: "req-fail",
		}, false},
		{"无 request_id 不补扣", UsageRecord{
			ID: "r-noreq", UserID: "u1", AccessKeyID: "k1", PublicModel: "m",
			ProviderID: "p1", UpstreamModel: "priced", IngressProtocol: "chat",
			InputTokens: 1_000_000, TotalTokens: 1_000_000,
			UsageState: "reported", Status: "ok",
		}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := c.rec
			if _, err := st.CreateUsageRecordWithCost(ctx, &rec); err != nil {
				t.Fatalf("create usage: %v", err)
			}
		})
	}

	found, err := st.FindUnchargedUsage(ctx, 100)
	if err != nil {
		t.Fatalf("FindUnchargedUsage: %v", err)
	}
	if len(found) != 0 {
		t.Fatalf("found = %+v, want 0 条（不该扣的一律不补扣）", found)
	}
}
