package main

// perf_split_test.go —— 拆分测量：**INSERT 与扣费各占多少**。
//
// # 为什么需要这个文件（先量再优化）
//
// `AUDIT/test-perf.md` 的因果对照（931 → 36144 req/s，38.8×）用的是
// `model_not_found` 作为对照组。那条路径**两件事都不做**：不写 usage、
// 也不扣费。所以那个 38.8× 是「去掉 INSERT **和** 扣费」的合并效果，
// **无法回答「两边各占多少」**。
//
// 而这个问题决定了优化方向：
//   - 若 INSERT 占大头 → 批量落库是正确的一击；
//   - 若扣费占大头 → 批量 INSERT 基本白干（扣费那条事务还在）。
//
// # 一个必须先说清楚的前提（本文件最重要的发现）
//
// `buildHarness`（failover_test.go）**从来不往 `upstream_models` 写价格**：
// 它只建 provider / user / access_key / 快照路由，一条模型价都没有。
// 于是 `freezeUsageCost` 查价查不到 → `cost_total` 恒 0 →
// `ChargeBalance` 在第一行就 `if amountYuan <= 0 { return nil }` 短路，
// **连事务都不开**（见 balance_dao.go 的「amountYuan <= 0 时完全 no-op」）。
//
// 也就是说：**现有的 38.8× 对照测的是「入队 + INSERT」这一条，扣费是零成本。**
// 这解释了为什么那条对照看起来那么夸张，也说明它**没有**覆盖生产上真正
// 的每请求两次写事务（INSERT 一次 + 扣费一次）。
//
// 本文件用三个手臂把两笔成本分开量：
//
//	手臂 N（neither）：model_not_found —— 0 次写事务
//	手臂 I（insert only）：正常请求但**无价格** —— 1 次写事务（INSERT）
//	手臂 F（full）：正常请求 + **有价格 + 有余额** —— 2 次写事务（INSERT + 扣费）
//
// 于是：INSERT 的成本 ≈ N vs I；扣费的成本 ≈ I vs F。
//
// 另配一组**store 层微基准**（同一写池、同并发）直接量两种事务各自的
// 天花板，作为上面三手臂的交叉验证 —— 两套方法给出一致的排序才可信。

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cn-maul/rosetta-gateway/internal/store"
)

// discardLoggerForSplit 是本地丢弃日志器（与 failover_test.go 的 buildHarness
// 里那个同款）。微基准要跑几千次事务，日志必须丢掉，否则输出淹没数字。
func discardLoggerForSplit() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// splitArm 记录一个手臂的原始数字。
type splitArm struct {
	name       string
	qps        float64
	p50, p99   time.Duration
	ok, failed int
	usageRows  int
}

// runSplitArm 跑一个手臂并打印（复用既有 runNonStreamWorkload，不另造脚手架）。
func runSplitArm(t *testing.T, env *perfEnv, db *store.Store, body string, conc, total int, label string) splitArm {
	t.Helper()
	base := usageRowCount(t, db)
	r := runNonStreamWorkload(env.client, env.gw.URL, body, conc, total)
	rows := usageRowCount(t, db) - base
	t.Logf("split %-16s 吞吐=%8.1f req/s  P50=%-12v P99=%-14v ok=%-6d failed=%-4d 新增 usage 行=%d",
		label, r.qps, r.p50.Round(time.Microsecond), r.p99.Round(time.Microsecond), r.ok, r.failed, rows)
	return splitArm{
		name: label, qps: r.qps, p50: r.p50, p99: r.p99,
		ok: r.ok, failed: r.failed, usageRows: rows,
	}
}

// drainUsage 等异步落库追平到 want 行，避免上一手臂的尾巴污染下一手臂。
//
// 这是拆分测量的**关键纪律**：usage 是异步落库的（单 worker），
// 请求返回 ≠ 已落库。不等干净就换手臂，量到的是「上一轮的积压 + 本轮」，
// 数字无法归因。
func drainUsage(t *testing.T, db *store.Store, want int) {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		if usageRowCount(t, db) >= want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("落库未追平：想要 %d 行，实际 %d 行（等待超时）", want, usageRowCount(t, db))
}

// TestSplitInsertVsCharge 是本次优化的**方向判据**。
//
// 跑法（约 3~5 分钟，故 -short 下跳过）：
//
//	go test ./cmd/gateway/ -run TestSplitInsertVsCharge -v -count=1
//
// # ⚠️ 本用例第一版暴露了一个比「拆分数字」更重要的前提问题
//
// 第一版跑出来手臂 I 与手臂 F **吞吐几乎相同**（3827 vs 3459），且
// 手臂 F 的扣费流水是 **0 行**。原因不是价格没生效，而是：
//
//	buildHarness 直接返回 handleIngress，**没有包 server.Middleware**，
//	所以 context 里没有 request_id；而 usageRecorder.charge 在
//	`rec.RequestID == ""` 时**直接 return，完全不扣费**（见其注释：
//	「空 request_id → 没有幂等键，扣费不做」）。
//
// 这意味着 **`AUDIT/test-perf.md` 里所有性能数字都只含 INSERT、不含扣费**
// —— 包括那条 38.8× 的因果对照。生产上每个成功请求实际是
// **两次写事务**（INSERT + 扣费），而现有压测只覆盖了前一次。
//
// 所以本用例**必须**自己包一层 Middleware，否则「扣费成本」永远量不到。
// 这也是为什么本文件的第一版给出了一个**无效但看不出错**的数字
// （I≈F）——「两边一样快」会被误读成「扣费免费」，而真相是「扣费没跑」。
func TestSplitInsertVsCharge(t *testing.T) {
	if testing.Short() {
		t.Skip("拆分测量是重负载的，-short 下跳过")
	}
	good := fakeGood()
	defer good.Close()

	h, db := buildHarness(t, []struct {
		slug, url string
		fail      bool
	}{{"good", good.URL, false}}, false)
	// 包一层生产同款的 Middleware，让 request_id 存在（扣费幂等键）。
	// 不包的话 charge() 会因空 request_id 静默跳过，扣费成本恒为 0。
	// 复用 e2e_fake_upstream_test.go 的 middlewareForTest，不另造一份 ——
	// 两份实现会在 server 改 requestIDKey 时只改一处。
	// 它返回 http.Handler，而 newPerfEnv 要 http.HandlerFunc，故取方法值。
	env := newPerfEnv(t, middlewareForTest(h).ServeHTTP)

	ctx := context.Background()
	// 给测试用户充一笔足够大的余额：手臂 F 必须走**扣费成功**的路径，
	// 而不是被余额守卫拦下（拦下虽然也开事务，但错误路径与成功路径的
	// 语句数不同，量出来会偏低）。
	if err := db.AdjustBalance(ctx, testUserID, 100_000_000); err != nil {
		t.Fatalf("充值测试用户: %v", err)
	}

	const conc = 200
	const total = 3000

	// 预热（三条路径都走一遍）。预热必须在量之前：首次请求要付连接建立、
	// SDK 客户端构建、SQLite 预编译语句、快照懒初始化。
	for i := 0; i < 200; i++ {
		if code, err := doNonStream(env.client, env.gw.URL, nonStreamBody); err != nil || code != 200 {
			t.Fatalf("预热失败: code=%d err=%v", code, err)
		}
		if code, _ := doNonStream(env.client, env.gw.URL, notFoundBody); code != 404 {
			t.Fatalf("预热(404)意外状态: %d", code)
		}
	}
	drainUsage(t, db, usageRowCount(t, db))
	settle()

	// —— 手臂 N：两件事都不做（基准天花板）——
	//
	// 放在最前面：它不产生任何持久化副作用，不会污染后面两个手臂。
	armN := runSplitArm(t, env, db, notFoundBody, conc, total, "N 都不做(404)")

	// —— 手臂 I：只 INSERT（无价格 → 扣费 no-op）——
	armI := runSplitArm(t, env, db, nonStreamBody, conc, total, "I 只INSERT")
	totalI := usageRowCount(t, db)
	drainUsage(t, db, totalI)
	settle()

	// —— 手臂 F：INSERT + 扣费 ——
	//
	// 到这里才写价格：priceRoute 会**同时**改快照与数据库，且不可逆
	// （不能把价格改回「未配置」来重跑手臂 I），所以 F 必须排在 I 之后。
	//
	// 单价取 2000 元/百万 tokens，让每次请求都**跨过一分**：
	// fakeGood 报 input=5 + output=3 = 8 token，成本 = 8×2000/1e6 = 0.016 元
	// = 1.6 分。这样每次扣费都走**完整路径**（含真正的余额扣减与
	// balance_charges 金额回写），而不是「余数不够一分」的短路分支 ——
	// 后者语句更少，量出来会低估扣费成本。
	priceRoute(t, db, "good", "good-model", 2000.0, 2000.0)
	// 改价后重新预热，让新快照在热路径生效。
	for i := 0; i < 100; i++ {
		if code, err := doNonStream(env.client, env.gw.URL, nonStreamBody); err != nil || code != 200 {
			t.Fatalf("改价后预热失败: code=%d err=%v", code, err)
		}
	}
	drainUsage(t, db, usageRowCount(t, db))
	settle()

	armF := runSplitArm(t, env, db, nonStreamBody, conc, total, "F INSERT+扣费")

	// 校验手臂 F 真的在扣费（否则「F 与 I 一样快」会被误读成「扣费免费」）。
	var charged int64
	if err := db.Reader().QueryRow(
		`SELECT COALESCE(SUM(amount_cents),0) FROM balance_charges WHERE user_id = ?`,
		testUserID).Scan(&charged); err != nil {
		t.Fatalf("读扣费合计: %v", err)
	}
	var chargeRows int64
	if err := db.Reader().QueryRow(
		`SELECT COUNT(*) FROM balance_charges WHERE user_id = ?`, testUserID).Scan(&chargeRows); err != nil {
		t.Fatalf("读扣费行数: %v", err)
	}
	t.Logf("split 手臂F 扣费流水: %d 行, 合计 %d 分（必须 > 0，否则「扣费成本」没被测到）",
		chargeRows, charged)
	if chargeRows == 0 {
		t.Fatal("手臂 F 一条扣费流水都没有 —— 价格/余额没生效，拆分结论无效")
	}

	// —— 汇总：把两笔成本分开 ——
	t.Logf("")
	t.Logf("=== 拆分汇总（conc=%d, n=%d，同机同方法）===", conc, total)
	t.Logf("split 手臂N 都不做   : %8.1f req/s  ← 天花板（测量回路 + 网关非DB部分）", armN.qps)
	t.Logf("split 手臂I 只INSERT : %8.1f req/s", armI.qps)
	t.Logf("split 手臂F INSERT+扣费: %8.1f req/s", armF.qps)

	// 用「每请求耗时」相加的模型来分解：总耗时 = 固定部分 + INSERT 部分 + 扣费部分。
	// 比直接比 QPS 更直观，因为 QPS 是倒数量纲，差值不可线性分解。
	perReq := func(q float64) time.Duration {
		if q <= 0 {
			return 0
		}
		return time.Duration(float64(time.Second) / q)
	}
	dN, dI, dF := perReq(armN.qps), perReq(armI.qps), perReq(armF.qps)
	t.Logf("split 每请求耗时: N=%v  I=%v  F=%v", dN.Round(time.Microsecond), dI.Round(time.Microsecond), dF.Round(time.Microsecond))

	insertCost := dI - dN
	chargeCost := dF - dI
	t.Logf("split 归属: INSERT≈%v/请求   扣费≈%v/请求",
		insertCost.Round(time.Microsecond), chargeCost.Round(time.Microsecond))

	// 两笔成本谁占大头 —— 这就是优化方向的判据。
	if insertCost > 0 && chargeCost > 0 {
		ratio := float64(insertCost) / float64(chargeCost)
		switch {
		case ratio > 2:
			t.Logf("split 结论 **INSERT 是大头**（INSERT/扣费 = %.2f×）→ 优先批量落库", ratio)
		case ratio < 0.5:
			t.Logf("split 结论 **扣费是大头**（INSERT/扣费 = %.2f×）→ 只批量 INSERT 基本白干，"+
				"必须同时处理扣费那条事务", ratio)
		default:
			t.Logf("split 结论 **两笔相当**（INSERT/扣费 = %.2f×）→ 只优化一侧最多省一半", ratio)
		}
	} else {
		t.Logf("split 结论 有一侧的成本测不出来（<=0）—— 说明该侧在并发热路径上不是限制，" +
			"见上面的 QPS 与扣费流水行数")
	}
}

// TestSplitStoreTxCeiling 是交叉验证：在 **store 层**直接量两种事务各自的天花板。
//
// 为什么要第二套方法：上面三手臂的差值是间接推断（还有快照改价、余额预检等
// 差异混在里面）。这里把两种事务单独拎出来、同一写池、同一并发跑，
// 得到的是**直接测量**。两套方法给出一致的排序，结论才可信。
//
// 注意：两者都受 `SetMaxOpenConns(1)` 约束（同一个 Store 实例），
// 所以这里量的是「在单写连接下，N 并发能跑多少条该种事务」——
// 正是我们要比较的量。
//
//	go test ./cmd/gateway/ -run TestSplitStoreTxCeiling -v -count=1
func TestSplitStoreTxCeiling(t *testing.T) {
	if testing.Short() {
		t.Skip("事务天花板微基准是重负载的，-short 下跳过")
	}
	logger := discardLoggerForSplit()
	db, err := store.Open(t.TempDir()+"/split.db", logger)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	ctx := context.Background()
	if err := db.CreateUser(ctx, &store.User{
		ID: "u1", Username: "u1", Role: store.RoleUser, Status: store.UserStatusActive,
		BalanceCents: 1_000_000_000,
	}); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if err := db.CreateProvider(ctx, &store.Provider{
		ID: "p1", Slug: "p1", Name: "p1", Protocol: "openai-chat", Enabled: true,
	}); err != nil {
		t.Fatalf("seed provider: %v", err)
	}
	if err := db.CreateUpstreamModel(ctx, &store.UpstreamModel{
		ID: "m1", ProviderID: "p1", ModelID: "m", Enabled: true,
		PriceInput: 30, PriceOutput: 30,
	}); err != nil {
		t.Fatalf("seed model: %v", err)
	}

	const conc = 200
	const ops = 3000

	// 串行参考值：单线程下的天花板（排除并发调度噪声）。
	serialInsert := measureSerial(t, ops, func(i int) error {
		_, err := db.CreateUsageRecordWithCost(ctx, &store.UsageRecord{
			ID: fmt.Sprintf("si-%d", i), UserID: "u1", AccessKeyID: "k1",
			PublicModel: "m", ProviderID: "p1", UpstreamModel: "m",
			IngressProtocol: "openai-chat", Status: "ok",
			InputTokens: 5, OutputTokens: 3, TotalTokens: 8,
		})
		return err
	})
	serialCharge := measureSerial(t, ops, func(i int) error {
		return db.ChargeBalance(ctx, "u1", 0.01, fmt.Sprintf("sc-%d", i))
	})
	t.Logf("split-tx 串行: INSERT=%.0f ops/s   扣费=%.0f ops/s", serialInsert, serialCharge)

	// 并发：这才是并发热路径上的真实形态。
	var next1, next2 atomic.Int64
	concInsert, errInsert := measureConcurrent(conc, ops, &next1, func(i int) error {
		_, err := db.CreateUsageRecordWithCost(ctx, &store.UsageRecord{
			ID: fmt.Sprintf("ci-%d", i), UserID: "u1", AccessKeyID: "k1",
			PublicModel: "m", ProviderID: "p1", UpstreamModel: "m",
			IngressProtocol: "openai-chat", Status: "ok",
			InputTokens: 5, OutputTokens: 3, TotalTokens: 8,
		})
		return err
	})
	concCharge, errCharge := measureConcurrent(conc, ops, &next2, func(i int) error {
		return db.ChargeBalance(ctx, "u1", 0.01, fmt.Sprintf("cc-%d", i))
	})

	t.Logf("split-tx 并发(%d): INSERT=%.0f ops/s(错误 %d)   扣费=%.0f ops/s(错误 %d)",
		conc, concInsert, errInsert, concCharge, errCharge)
	t.Logf("split-tx 并发/串行 扩展比: INSERT=%.2f×  扣费=%.2f×",
		concInsert/serialInsert, concCharge/serialCharge)

	// 换算成「每事务耗时」再比较 —— ops/s 是倒数量纲，直接比会读反。
	// 这一点我在第一版写错了：ops/s 高 = 便宜，而我把「ops/s 高」读成了
	// 「更贵」。下面一律用 perOp 比较。
	perOpInsert := time.Duration(float64(time.Second) / concInsert)
	perOpCharge := time.Duration(float64(time.Second) / concCharge)
	t.Logf("split-tx 每事务耗时: INSERT=%v   扣费=%v",
		perOpInsert.Round(time.Microsecond), perOpCharge.Round(time.Microsecond))

	insertOverCharge := float64(perOpInsert) / float64(perOpCharge)
	t.Logf("split-tx 归属: INSERT 耗时 / 扣费耗时 = %.2f×（>1 表示 INSERT 更贵）",
		insertOverCharge)

	// 两种事务都是「读/写若干行 + 一次 commit」，理论上应同量级。
	// 差得多说明其中一条重（语句数、索引、触发器更多）——
	// usage_records 上有两个 AFTER INSERT 触发器 + 4 个索引，
	// 所以 INSERT 更贵是符合预期的。
	switch {
	case insertOverCharge > 1.5:
		t.Logf("split-tx 结论 **INSERT 事务更贵**（是扣费的 %.2f× 耗时）→ 批量 INSERT 收益可观",
			insertOverCharge)
	case insertOverCharge < 0.67:
		t.Logf("split-tx 结论 **扣费事务更贵**（是 INSERT 的 %.2f× 耗时）→ 只批量 INSERT 收益有限，"+
			"需同时处理扣费那侧", 1/insertOverCharge)
	default:
		t.Logf("split-tx 结论 两种事务成本同量级（比值 %.2f×）→ 只批量一侧最多省约一半",
			insertOverCharge)
	}
}

// measureSerial 串行跑 ops 次，返回 ops/s。
func measureSerial(t *testing.T, ops int, fn func(i int) error) float64 {
	t.Helper()
	start := time.Now()
	for i := 0; i < ops; i++ {
		if err := fn(i); err != nil {
			t.Fatalf("serial op %d: %v", i, err)
		}
	}
	return float64(ops) / time.Since(start).Seconds()
}

// measureConcurrent 以 conc 并发跑 ops 次（atomic 发号），返回 ops/s 与错误数。
//
// 返回错误数而**不**在内部 Fatal：本微基准里「扣费返回 ErrInsufficientBalance」
// 是可能且有意义的数据（说明余额被扣光了），调用方需要把它打印出来
// 而不是让整个用例炸掉 —— 那会掩盖真正的性能数字。
func measureConcurrent(conc, ops int, next *atomic.Int64, fn func(i int) error) (float64, int64) {
	var wg sync.WaitGroup
	var errCount atomic.Int64
	start := time.Now()
	for w := 0; w < conc; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				i := int(next.Add(1))
				if i > ops {
					return
				}
				if err := fn(i); err != nil {
					errCount.Add(1)
				}
			}
		}()
	}
	wg.Wait()
	return float64(ops) / time.Since(start).Seconds(), errCount.Load()
}
