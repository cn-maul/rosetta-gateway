package store

// 充值流水的回归测试。
//
// 本模块要钉住的不变量（逐条对应下面的用例）：
//   - 充值成功必写一条流水，且**每个字段**都对（尤其是 balance_after 与
//     操作者署名 —— 这两个错了流水就没了用途）。
//   - was_unlimited 两种取值都正确，且**不能**用「新值 == delta」反推：
//     一个本来 0 分的用户被充值到恰好等于 delta 的值时不是语义突变。
//   - 负 delta（扣减）同样记流水。
//   - 用户不存在 → 既不改余额也不写流水。
//   - **并发充值下流水条数与金额求和都正确** —— 这条最容易错：余额自增是
//     单条 UPDATE 原子完成的，但流水是另一条语句，两者之间的窗口正是
//     「余额改了、流水没写」的窗口。
//   - 查询按 user_id 收窄，空 userID 返回空而不是「全部」。

import (
	"context"
	"errors"
	"sync"
	"testing"
)

// ---- 1. 基本写入与字段正确性 ----

// 充值成功后余额与流水必须同时正确，且流水字段逐个可读。
func TestTopup_RecordsLedgerRowWithCorrectFields(t *testing.T) {
	ctx := context.Background()
	st := testStore(t, t.TempDir()+"/gw.db")
	mustUser(t, st, "u1", 1000, false)

	got, err := st.AdjustBalanceWithLedger(ctx, "u1", 5000, "admin-1", "root", "工单 #42")
	if err != nil {
		t.Fatalf("adjust with ledger: %v", err)
	}

	// 余额必须真的加上去了。
	assertBalance(t, st, "u1", 6000, true)

	if got.ID == "" {
		t.Error("流水 id 为空：前端要拿它做 key，缺了就只能按索引渲染")
	}
	if got.UserID != "u1" {
		t.Errorf("流水 user_id = %q, want u1（这是被调整的人，不是操作者）", got.UserID)
	}
	if got.DeltaCents != 5000 {
		t.Errorf("流水 delta_cents = %d, want 5000", got.DeltaCents)
	}
	// balance_after 必须等于**真实**余额，且不是「旧值 + delta」凑出来的。
	if got.BalanceAfter != 6000 {
		t.Errorf("流水 balance_after = %d, want 6000（要等于回读的真实余额）", got.BalanceAfter)
	}
	if got.WasUnlimited {
		t.Error("was_unlimited = true, want false（用户本来就有额度）")
	}
	if got.OperatorID != "admin-1" || got.OperatorUsername != "root" {
		t.Errorf("操作者 = (%q,%q), want (admin-1, root)", got.OperatorID, got.OperatorUsername)
	}
	if got.Remark != "工单 #42" {
		t.Errorf("remark = %q, want 工单 #42", got.Remark)
	}
	if got.Ts <= 0 {
		t.Error("ts 未落库")
	}

	// 库里确实有一行，且读回来的字段与返回值一致。
	list, err := st.ListTopupsByUser(ctx, "u1", 100, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("流水条数 = %d, want 1", len(list))
	}
	if list[0].ID != got.ID {
		t.Errorf("读回 id = %q, want %q", list[0].ID, got.ID)
	}
	if list[0].BalanceAfter != 6000 || list[0].DeltaCents != 5000 {
		t.Errorf("读回 (delta=%d, after=%d), want (5000, 6000)",
			list[0].DeltaCents, list[0].BalanceAfter)
	}
	if list[0].OperatorUsername != "root" {
		t.Errorf("读回 operator_username = %q, want root", list[0].OperatorUsername)
	}
}

// ---- 2. was_unlimited 的两种取值 ----

// 给「不限额」（NULL）用户充值 = 语义突变，必须被记下来。
func TestTopup_WasUnlimitedTrueWhenToppingUpUnlimitedUser(t *testing.T) {
	ctx := context.Background()
	st := testStore(t, t.TempDir()+"/gw.db")
	mustUser(t, st, "vip", 0, true) // unlimited=true → balance_cents IS NULL

	got, err := st.AdjustBalanceWithLedger(ctx, "vip", 10000, "admin-1", "root", "")
	if err != nil {
		t.Fatalf("adjust: %v", err)
	}
	if !got.WasUnlimited {
		t.Error("was_unlimited = false, want true（这次把无限额度切成了 100 元）")
	}
	// 不限额 → 有限额 的切换本身也要正确。
	assertBalance(t, st, "vip", 10000, true)

	list, _ := st.ListTopupsByUser(ctx, "vip", 10, 0)
	if len(list) != 1 || !list[0].WasUnlimited {
		t.Errorf("读回 was_unlimited 未保持：%+v", list)
	}
}

// **这是本模块最容易写错的一处**：不能用「新值 == delta」反推 was_unlimited。
// 一个本来就有额度、余额恰好是 0 的用户，被充值 5000 后余额也是 5000 ——
// 数字上与「从 NULL 变过来」完全一样，但它**不是**语义突变。
// 这个用例把两种情形放在同一个数值上，确保判据是「旧值是不是 NULL」。
func TestTopup_WasUnlimitedNotInferredFromEqualValues(t *testing.T) {
	ctx := context.Background()
	st := testStore(t, t.TempDir()+"/gw.db")
	mustUser(t, st, "zero", 0, false) // 有限额，余额 0（真没钱，不是无限）

	got, err := st.AdjustBalanceWithLedger(ctx, "zero", 5000, "admin-1", "root", "")
	if err != nil {
		t.Fatalf("adjust: %v", err)
	}
	if got.BalanceAfter != 5000 {
		t.Fatalf("balance_after = %d, want 5000", got.BalanceAfter)
	}
	if got.WasUnlimited {
		t.Error("was_unlimited = true, want false（旧值是 0 不是 NULL，只是碰巧相等）")
	}
}

// 连续两次充值：第一次对有限额用户（false），第二次对已充值用户（仍是 false）。
func TestTopup_WasUnlimitedOnlyTrueOnTheTransition(t *testing.T) {
	ctx := context.Background()
	st := testStore(t, t.TempDir()+"/gw.db")
	mustUser(t, st, "vip2", 0, true)

	first, _ := st.AdjustBalanceWithLedger(ctx, "vip2", 1000, "admin-1", "root", "")
	if !first.WasUnlimited {
		t.Fatal("第一次（从 NULL 切过来）was_unlimited 应为 true")
	}
	second, _ := st.AdjustBalanceWithLedger(ctx, "vip2", 1000, "admin-1", "root", "")
	if second.WasUnlimited {
		t.Error("第二次（已经是有限额）was_unlimited 应为 false")
	}
	assertBalance(t, st, "vip2", 2000, true)
}

// ---- 3. 负 delta（扣减）也记流水 ----

// 扣钱也是一次余额变化，同样必须留痕 —— 否则「余额少了」这一侧无从查起。
func TestTopup_NegativeDeltaAlsoRecorded(t *testing.T) {
	ctx := context.Background()
	st := testStore(t, t.TempDir()+"/gw.db")
	mustUser(t, st, "u1", 5000, false)

	got, err := st.AdjustBalanceWithLedger(ctx, "u1", -2000, "admin-1", "root", "退款")
	if err != nil {
		t.Fatalf("adjust: %v", err)
	}
	if got.DeltaCents != -2000 {
		t.Errorf("delta_cents = %d, want -2000（方向必须保留，不能存绝对值）", got.DeltaCents)
	}
	if got.BalanceAfter != 3000 {
		t.Errorf("balance_after = %d, want 3000", got.BalanceAfter)
	}
	assertBalance(t, st, "u1", 3000, true)

	list, _ := st.ListTopupsByUser(ctx, "u1", 10, 0)
	if len(list) != 1 || list[0].DeltaCents != -2000 {
		t.Fatalf("扣减未记流水：%+v", list)
	}
}

// 扣减把余额夹到 0：流水仍要记，且 balance_after 反映夹断后的真实值。
// delta 保留原始意图（-5000），balance_after 记实际结果（0）——
// 两者不同才是对账需要的：正是这 5000 分的差让「扣不动」变得可解释。
func TestTopup_ClampedDeductionStillRecorded(t *testing.T) {
	ctx := context.Background()
	st := testStore(t, t.TempDir()+"/gw.db")
	mustUser(t, st, "u1", 300, false)

	got, err := st.AdjustBalanceWithLedger(ctx, "u1", -5000, "admin-1", "root", "")
	if err != nil {
		t.Fatalf("adjust: %v", err)
	}
	if got.DeltaCents != -5000 {
		t.Errorf("delta_cents = %d, want -5000（保留原始扣减意图）", got.DeltaCents)
	}
	if got.BalanceAfter != 0 {
		t.Errorf("balance_after = %d, want 0（夹断后的真实值）", got.BalanceAfter)
	}
	assertBalance(t, st, "u1", 0, true)
}

// ---- 4. 用户不存在：不改余额、不写流水 ----

func TestTopup_UnknownUserWritesNothing(t *testing.T) {
	ctx := context.Background()
	st := testStore(t, t.TempDir()+"/gw.db")

	_, err := st.AdjustBalanceWithLedger(ctx, "ghost", 5000, "admin-1", "root", "")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	// 关键：失败的调用不能留下流水。
	n, cerr := st.CountTopupsByUser(ctx, "ghost")
	if cerr != nil {
		t.Fatalf("count: %v", cerr)
	}
	if n != 0 {
		t.Errorf("用户不存在却写了 %d 条流水：流水是为已发生的变更留痕的", n)
	}
}

func TestTopup_ZeroDeltaRejected(t *testing.T) {
	ctx := context.Background()
	st := testStore(t, t.TempDir()+"/gw.db")
	mustUser(t, st, "u1", 1000, false)

	if _, err := st.AdjustBalanceWithLedger(ctx, "u1", 0, "admin-1", "root", ""); err == nil {
		t.Fatal("delta=0 应当报错（调平不是一次有意义的操作，更可能是调用方 bug）")
	}
	n, _ := st.CountTopupsByUser(ctx, "u1")
	if n != 0 {
		t.Errorf("delta=0 却写了 %d 条流水", n)
	}
}

// ---- 5. 并发：条数与金额求和都必须正确（重点） ----

// 并发充值下，流水条数必须等于操作次数，delta 求和必须等于总充值额，
// 且最后一条流水的 balance_after 必须等于真实余额。
//
// 这条用例专门盯住「余额自增是原子的、流水写入是另一次语句」这个结构：
// 每个 worker 做「预读 → UPDATE…RETURNING → INSERT 流水」，三者之间没有
// 任何互斥。若流水写漏了或写重了，这里必然红。
func TestTopup_ConcurrentTopUpsLedgerCountAndSumAreExact(t *testing.T) {
	ctx := context.Background()
	st := testStore(t, t.TempDir()+"/gw.db")
	mustUser(t, st, "u1", 0, false)

	const workers = 10
	const delta = 100

	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			if _, err := st.AdjustBalanceWithLedger(ctx, "u1", delta, "admin-1", "root", ""); err != nil {
				t.Errorf("worker %d adjust: %v", i, err)
			}
		}(i)
	}
	close(start)
	wg.Wait()

	// 余额：与 balance_dao_test 的并发用例同一条不变量，不能被流水改动破坏。
	assertBalance(t, st, "u1", delta*workers, true)

	// 条数：必须正好 workers 条。
	list, err := st.ListTopupsByUser(ctx, "u1", 1000, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != workers {
		t.Fatalf("流水条数 = %d, want %d（漏写或重写都会在这里暴露）", len(list), workers)
	}

	// 金额求和：必须等于总充值额。
	var sum int64
	for _, tp := range list {
		sum += tp.DeltaCents
	}
	if sum != delta*workers {
		t.Errorf("流水 delta 求和 = %d, want %d", sum, delta*workers)
	}

	// ID 必须互不相同（否则说明有两次写了同一行）。
	seen := make(map[string]bool, len(list))
	for _, tp := range list {
		if seen[tp.ID] {
			t.Fatalf("流水 id 重复：%s", tp.ID)
		}
		seen[tp.ID] = true
	}

	// 最大的 balance_after 必须等于最终真实余额。
	//
	// **不能**断言 list[0] 就是最新那条：并发下「执行最后一次 UPDATE 的
	// 那个 worker」与「rowid 最大的那条流水」未必是同一个 —— 余额自增与
	// 流水 INSERT 是两条独立语句，中间完全可能又夹进别的 worker 的 UPDATE。
	// 按序取第一条在这里是 flaky 的（-count=25 能稳定复现）。
	//
	// 这正是 DAO 注释里写明「并发下 balance_after 只保证本次自己的结果是
	// 权威值，不保证与相邻那条首尾相接」的原因：逐笔核对要靠 delta 求和，
	// 而 balance_after 的正确用法是取最大值对齐终值。
	var maxAfter int64
	for _, tp := range list {
		if tp.BalanceAfter > maxAfter {
			maxAfter = tp.BalanceAfter
		}
	}
	if maxAfter != delta*workers {
		t.Errorf("流水 balance_after 最大值 = %d, want %d（必须与真实余额一致）",
			maxAfter, delta*workers)
	}
}

// ---- 6. 查询的收窄与分页 ----

func TestTopup_ListIsScopedToUser(t *testing.T) {
	ctx := context.Background()
	st := testStore(t, t.TempDir()+"/gw.db")
	mustUser(t, st, "alice", 0, false)
	mustUser(t, st, "bob", 0, false)

	if _, err := st.AdjustBalanceWithLedger(ctx, "alice", 1000, "a1", "root", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AdjustBalanceWithLedger(ctx, "bob", 2000, "a1", "root", ""); err != nil {
		t.Fatal(err)
	}

	alice, _ := st.ListTopupsByUser(ctx, "alice", 100, 0)
	if len(alice) != 1 || alice[0].DeltaCents != 1000 {
		t.Fatalf("alice 的流水被污染：%+v", alice)
	}
	bob, _ := st.ListTopupsByUser(ctx, "bob", 100, 0)
	if len(bob) != 1 || bob[0].DeltaCents != 2000 {
		t.Fatalf("bob 的流水被污染：%+v", bob)
	}
	na, _ := st.CountTopupsByUser(ctx, "alice")
	nb, _ := st.CountTopupsByUser(ctx, "bob")
	if na != 1 || nb != 1 {
		t.Errorf("count = (%d,%d), want (1,1)", na, nb)
	}
}

// 空 userID 必须返回空而不是全部：这是权限边界最后一道闸。
// 一个漏传 id 的调用若看到别人的充值记录（含管理员署名），就是越权。
func TestTopup_EmptyUserIDReturnsNothing(t *testing.T) {
	ctx := context.Background()
	st := testStore(t, t.TempDir()+"/gw.db")
	mustUser(t, st, "alice", 0, false)
	if _, err := st.AdjustBalanceWithLedger(ctx, "alice", 1000, "a1", "root", ""); err != nil {
		t.Fatal(err)
	}

	list, err := st.ListTopupsByUser(ctx, "", 100, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 0 {
		t.Errorf("空 userID 返回了 %d 条（应是 0）：「查不到 id」不等于「查全部」", len(list))
	}
	n, _ := st.CountTopupsByUser(ctx, "")
	if n != 0 {
		t.Errorf("空 userID 的 count = %d, want 0", n)
	}
}

func TestTopup_ListIsNewestFirstAndPaginates(t *testing.T) {
	ctx := context.Background()
	st := testStore(t, t.TempDir()+"/gw.db")
	mustUser(t, st, "u1", 0, false)

	// 逐笔充值，余额依次递增；金额可用来判断先后。
	for i := 1; i <= 5; i++ {
		if _, err := st.AdjustBalanceWithLedger(ctx, "u1", int64(i*100), "a1", "root", ""); err != nil {
			t.Fatalf("adjust %d: %v", i, err)
		}
	}

	all, _ := st.ListTopupsByUser(ctx, "u1", 100, 0)
	if len(all) != 5 {
		t.Fatalf("条数 = %d, want 5", len(all))
	}
	// 倒序：最新的一笔 delta 应该是 500（最后充的）。
	if all[0].DeltaCents != 500 {
		t.Errorf("第一条 delta = %d, want 500（应按时间倒序）", all[0].DeltaCents)
	}

	page1, _ := st.ListTopupsByUser(ctx, "u1", 2, 0)
	page2, _ := st.ListTopupsByUser(ctx, "u1", 2, 2)
	if len(page1) != 2 || len(page2) != 2 {
		t.Fatalf("分页条数 = (%d,%d), want (2,2)", len(page1), len(page2))
	}
	if page1[0].ID == page2[0].ID {
		t.Error("两页返回了同一条记录：offset 没生效")
	}
	// offset 超出范围返回空，不报错。
	page9, err := st.ListTopupsByUser(ctx, "u1", 2, 999)
	if err != nil {
		t.Fatalf("list past end: %v", err)
	}
	if len(page9) != 0 {
		t.Errorf("越界分页返回了 %d 条", len(page9))
	}
}

// limit 必须被钳制：`LIMIT -1` 在 SQLite 里是「不限制」，一个漏钳的入参
// 就是一次全表物化。负数走默认值。
func TestTopup_LimitIsClamped(t *testing.T) {
	ctx := context.Background()
	st := testStore(t, t.TempDir()+"/gw.db")
	mustUser(t, st, "u1", 0, false)
	for i := 0; i < 3; i++ {
		if _, err := st.AdjustBalanceWithLedger(ctx, "u1", 100, "a1", "root", ""); err != nil {
			t.Fatal(err)
		}
	}

	// limit=-1 会变成默认值 100（不是「不限制」）。
	list, err := st.ListTopupsByUser(ctx, "u1", -1, 0)
	if err != nil {
		t.Fatalf("list limit=-1: %v", err)
	}
	if len(list) != 3 {
		t.Errorf("limit=-1 返回 %d 条, want 3（应回落默认 100）", len(list))
	}
	// 超大 limit 也不能放行成全表。
	huge, err := st.ListTopupsByUser(ctx, "u1", 1<<40, 0)
	if err != nil {
		t.Fatalf("list huge limit: %v", err)
	}
	if len(huge) != 3 {
		t.Errorf("超大 limit 返回 %d 条, want 3", len(huge))
	}
}

// 可空列（remark / operator_username）落 NULL 再读回，必须是空串而不是崩。
func TestTopup_NullableColumnsReadBackAsEmptyString(t *testing.T) {
	ctx := context.Background()
	st := testStore(t, t.TempDir()+"/gw.db")
	mustUser(t, st, "u1", 0, false)

	// 空字符串走 nullIfEmpty → 落库为 NULL。
	if _, err := st.AdjustBalanceWithLedger(ctx, "u1", 100, "a1", "", ""); err != nil {
		t.Fatal(err)
	}
	list, _ := st.ListTopupsByUser(ctx, "u1", 10, 0)
	if len(list) != 1 {
		t.Fatalf("条数 = %d, want 1", len(list))
	}
	if list[0].Remark != "" || list[0].OperatorUsername != "" {
		t.Errorf("NULL 列读回 = (%q,%q), want 空串", list[0].Remark, list[0].OperatorUsername)
	}
}

// ---- 7. 与既有 AdjustBalance 的关系 ----

// 老的 AdjustBalance 必须保持原样（管理面仍可能有别的调用方），
// 且它**不**写流水 —— 流水是本模块的新职责，老方法没被要求改。
// 这个用例钉住「两者的边界」：老方法只改余额。
func TestTopup_LegacyAdjustBalanceWritesNoLedgerRow(t *testing.T) {
	ctx := context.Background()
	st := testStore(t, t.TempDir()+"/gw.db")
	mustUser(t, st, "u1", 0, false)

	if err := st.AdjustBalance(ctx, "u1", 1000); err != nil {
		t.Fatalf("legacy adjust: %v", err)
	}
	assertBalance(t, st, "u1", 1000, true)

	n, _ := st.CountTopupsByUser(ctx, "u1")
	if n != 0 {
		t.Errorf("老 AdjustBalance 写了 %d 条流水（应保持只改余额的语义）", n)
	}
}

// 流水写入走读池，且在写池被占用（长事务）时仍可读 —— 钱包页不能因为
// 后台正在剪枝就转圈。WAL 下读写分池正是为此。
func TestTopup_ListReadsThroughReadPool(t *testing.T) {
	ctx := context.Background()
	st := testStore(t, t.TempDir()+"/gw.db")
	mustUser(t, st, "u1", 0, false)
	if _, err := st.AdjustBalanceWithLedger(ctx, "u1", 1000, "a1", "root", ""); err != nil {
		t.Fatal(err)
	}

	tx, err := st.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer tx.Rollback()
	// 写池被这个事务钉住（单连接池）。
	if _, err := tx.ExecContext(ctx, `UPDATE users SET remark = ? WHERE id = ?`, "held", "u1"); err != nil {
		t.Fatalf("write in tx: %v", err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		list, err := st.ListTopupsByUser(ctx, "u1", 10, 0)
		if err != nil {
			t.Errorf("读池被写事务堵住: %v", err)
			return
		}
		if len(list) != 1 {
			t.Errorf("读池读到 %d 条, want 1", len(list))
		}
	}()
	<-done
}
