package store

import (
	"context"
	"testing"
)

// 查价失败不得变成「永久免单」（2026-10-12 修 P1-2）。
//
// 原实现的 freezeUsageCost 在读池出错时记 0 元并打一条 WARN，然后**什么都没有**：
//   - cost_total 是固化字段，报表一律读它、不再按当前价重算；
//   - main.go 的 charge 用同一个 cost 扣费 → 该请求**完全免单**；
//   - RecomputeCost 在生产代码中**没有任何调用点**（纯手工接口）。
//
// 于是「有没有人在偷偷免单」这件事**没有任何可观测性**：报表上它就是一条
// 正常的 0 元记录，WARN 日志会被轮转冲掉。修法是把它变成一个**可查询的状态**
// （usage_state='cost_pending'）并加一个自动重算的消费者。

// usageStateOf 读一条记录的 usage_state 与 cost_total。
func usageStateOf(t *testing.T, db *Store, id string) (state string, cost float64) {
	t.Helper()
	if err := db.read.QueryRowContext(context.Background(),
		`SELECT usage_state, cost_total FROM usage_records WHERE id = ?`, id).
		Scan(&state, &cost); err != nil {
		t.Fatalf("read usage state (%s): %v", id, err)
	}
	return state, cost
}

// seedCostPending 直接写 SQL 造出一条「查价失败后的产物」：
// usage_state='cost_pending' 且 cost_total=0。
//
// 刻意**不走 CreateUsageRecord**：那个函数内部会调 freezeUsageCost，
// 它会正常查到价格并算出真实费用 —— 于是「模拟失败」根本模拟不出来
// （实测：传进去的 UsageState 确实被写成了 cost_pending，但 cost 却是
// 正确的 0.0006）。真实故障时这两者是**同一次调用**里一起产生的，
// 而复现它只能绕过计价函数直接写库。
func seedCostPending(t *testing.T, db *Store, id string, input, output int) {
	t.Helper()
	var uid any = "u1"
	_, err := db.db.ExecContext(context.Background(),
		`INSERT INTO usage_records (id, ts, access_key_id, user_id, public_model,
		   provider_id, upstream_model, ingress_protocol, stream,
		   input_tokens, output_tokens, total_tokens, usage_state, status, http_status,
		   latency_ms, cost_total)
		 VALUES (?, ?, 'k1', ?, 'priced', 'p1', 'priced', 'chat', 0, ?, ?, ?, ?, 'ok', 200, 10, 0)`,
		id, 1700000000000, uid, input, output, input+output, usageStateCostPending)
	if err != nil {
		t.Fatalf("seed cost_pending %s: %v", id, err)
	}
}

// cost_pending 标记必须能被 RecomputePendingCosts 自动纠正回真实费用。
//
// 这是整个修复的落点：标记本身没有价值，**自动重算**才有。
//
// 复用 balance_dao_test.go 的 seedPricedModel：它建的是 provider="p1"、
// model="priced"，价格由参数给定。
func TestCostPending_能被自动重算纠正(t *testing.T) {
	db := newStore(t)
	ctx := context.Background()
	seedPricedModel(t, db, 0.2, 0.02, 0.8)
	seedCostPending(t, db, "u-pending", 1000, 500)

	// 先确认它确实是「待重算 + 0 元」这个坏状态。
	state, cost := usageStateOf(t, db, "u-pending")
	if state != usageStateCostPending || cost != 0 {
		t.Fatalf("前置状态不对：state=%q cost=%v", state, cost)
	}

	fixed, remaining, err := db.RecomputePendingCosts(ctx, 100)
	if err != nil {
		t.Fatalf("RecomputePendingCosts: %v", err)
	}
	if fixed != 1 {
		t.Errorf("重算了 %d 条，期望 1 —— 待重算队列没有被消费", fixed)
	}
	if remaining {
		t.Error("remaining 应为 false：只有一条待重算记录")
	}

	// 费用必须变成真实值：(1000*0.2 + 500*0.8) / 1e6 = 0.0006 元。
	state, cost = usageStateOf(t, db, "u-pending")
	if want := 0.0006; cost < want*0.99 || cost > want*1.01 {
		t.Errorf("重算后 cost = %v，期望约 %v —— 该请求原本是免单的", cost, want)
	}
	// ★ 状态必须复位，否则下一轮还会捞到它、白跑一次，
	// 且「待重算队列非空」这个运维信号永不消失。
	if state == usageStateCostPending {
		t.Errorf("重算后 usage_state 仍是 %q：会被无限重复捞取", usageStateCostPending)
	}
}

// 正常记录**不能**被误标为 cost_pending，否则重算任务会无谓地反复扫全表。
func TestCostPending_未配价模型不标记(t *testing.T) {
	db := newStore(t)
	ctx := context.Background()
	// 只建 provider，不建 upstream_models → 查价必然 sql.ErrNoRows。
	if err := db.CreateProvider(ctx, &Provider{
		ID: "p2", Slug: "p2", Name: "p2",
		Endpoint: "http://x", Protocol: "chat", Enabled: true}); err != nil {
		t.Fatalf("create provider: %v", err)
	}

	if err := db.CreateUsageRecord(ctx, &UsageRecord{
		ID: "u-noprice", Ts: 1700000000000, UserID: "u1", AccessKeyID: "k1",
		PublicModel: "m", ProviderID: "p2", UpstreamModel: "unknown",
		IngressProtocol: "chat",
		InputTokens:     100, TotalTokens: 100,
		UsageState: "reported", Status: "ok", HTTPStatus: 200, LatencyMs: 10,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	state, cost := usageStateOf(t, db, "u-noprice")
	if state == usageStateCostPending {
		t.Errorf("未配价（sql.ErrNoRows）被标成 cost_pending（实际 %q）：\n"+
			"  那不是「暂时算不出来」，重算多少次都是 0 —— 标了只会在待重算队列里"+
			"永远积压，重算任务每 5 分钟白扫一次", state)
	}
	if cost != 0 {
		t.Errorf("未配价的费用应是 0，得到 %v", cost)
	}

	// 待重算队列必须为空。
	fixed, remaining, err := db.RecomputePendingCosts(ctx, 100)
	if err != nil {
		t.Fatalf("RecomputePendingCosts: %v", err)
	}
	if fixed != 0 || remaining {
		t.Errorf("待重算队列应为空，得到 fixed=%d remaining=%v", fixed, remaining)
	}
}

// 待重算队列为空时是 no-op，且不报错 —— 后台循环每 5 分钟都会调它。
func TestCostPending_队列为空是NoOp(t *testing.T) {
	db := newStore(t)
	fixed, remaining, err := db.RecomputePendingCosts(context.Background(), 100)
	if err != nil {
		t.Fatalf("空队列不应报错：%v", err)
	}
	if fixed != 0 {
		t.Errorf("fixed = %d，期望 0", fixed)
	}
	if remaining {
		t.Error("remaining 应为 false")
	}
}

// limit 上界必须生效：长期故障下待重算队列会增长，无界扫描会把写池占死。
func TestCostPending_尊重Limit上界(t *testing.T) {
	db := newStore(t)
	ctx := context.Background()
	seedPricedModel(t, db, 0.2, 0.02, 0.8)

	const total = 5
	for i := 0; i < total; i++ {
		seedCostPending(t, db, "u-lim-"+itoa(i), 100, 0)
	}

	// limit=2：只处理 2 条，且 remaining 应为 true（后面还有）。
	fixed, remaining, err := db.RecomputePendingCosts(ctx, 2)
	if err != nil {
		t.Fatalf("RecomputePendingCosts: %v", err)
	}
	if fixed != 2 {
		t.Errorf("fixed = %d，期望 2 —— limit 未生效", fixed)
	}
	if !remaining {
		t.Error("remaining 应为 true：捞满了 limit，后面还有 3 条")
	}
}
