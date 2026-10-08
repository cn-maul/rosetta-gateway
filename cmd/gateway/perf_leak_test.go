package main

// perf_leak_test.go —— 泄漏诊断（回答「残留的 goroutine 到底是什么」）。
//
// # 为什么需要单独一个文件
//
// D3（慢上游 100 并发）在 settle 之后仍看到 +492 条 goroutine 残留。
// 这个数字有两种可能，而它们的处置完全相反：
//
//  1. **真泄漏**：每次请求留下常驻 goroutine（漏听 ctx / 漏 close）。
//     这是 P0/P1 级问题，必须写进报告并给出定位。
//  2. **测量假象**：残留的是**别人**的 goroutine —— 前一个用例留下的
//     httptest 服务器 accept 循环、上一次 hang 用例里被取消但尚未退出的
//     请求、或本用例自身仍在跑收尾（usage 落库 / 配额释放）的 handler。
//
// 光看数字无法区分。判据只有一个：**把栈打出来看调用点**。
// 这个文件就是那个判据，不靠推理。
//
// 注意：诊断用例只在需要时跑（名字带 Diag 前缀），不参与常规门禁。

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cn-maul/rosetta-gateway/internal/store"
)

// goroutineProfile 返回「栈顶函数 → 条数」的聚合快照，按条数降序。
//
// 聚合而不是逐条打印：几百条原始栈无法阅读，而结论只需要
// 「哪几个函数占了绝大多数」。
//
// # 解析规则（必须按 goroutine 分块 + 抹掉实参）
//
// runtime.Stack(all=true) 的格式是：
//
//	goroutine 123 [chan receive]:
//	github.com/x/y.funcName(0x2ddbe4907680, 0xc000123456)   ← 栈顶
//		/path/file.go:42 +0x1d
//	created by ... in goroutine 5
//		/path/file.go:10 +0x2a
//
// 两个坑，都踩过：
//   - 逐行扫会把**深层帧**也算成一个 goroutine（计数完全不失真）；
//   - 不抹掉实参则每条栈顶都因指针不同而唯一，聚合结果退化成
//     「每条都是 1」，看不出「哪一类占了 486 条」。
func goroutineProfile() []string {
	buf := make([]byte, 1<<22)
	n := runtime.Stack(buf, true)

	counts := map[string]int{}
	var block []string
	flush := func() {
		if len(block) == 0 {
			return
		}
		for _, l := range block {
			if l == "" || l[0] == '\t' || l[0] == ' ' {
				continue
			}
			if strings.HasPrefix(l, "created by ") || strings.HasPrefix(l, "goroutine ") {
				continue
			}
			counts[stripArgs(l)]++
			break
		}
		block = block[:0]
	}

	for _, raw := range strings.Split(string(buf[:n]), "\n") {
		if strings.HasPrefix(raw, "goroutine ") {
			flush()
			block = append(block, raw)
			continue
		}
		block = append(block, raw)
	}
	flush()

	out := make([]string, 0, len(counts))
	for k, v := range counts {
		out = append(out, fmt.Sprintf("%5d  %s", v, k))
	}
	// 按条数降序；条数相同时按名字，保证输出稳定（可 diff）。
	sort.Slice(out, func(i, j int) bool {
		var ci, cj int
		var si, sj string
		fmt.Sscanf(out[i], "%d  %s", &ci, &si)
		fmt.Sscanf(out[j], "%d  %s", &cj, &sj)
		if ci != cj {
			return ci > cj
		}
		return si < sj
	})
	return out
}

// stripArgs 去掉栈帧里的实参，只留函数名。
//
// 例：`net/http.(*persistConn).writeLoop(0x2ddbe4907680)` → `net/http.(*persistConn).writeLoop`
// 括号是**配对**删除的：函数名里可能含 `)`（如 `func1.1()`、
// 泛型实例化 `foo[...]`），所以从最后一个 `(` 处截断而不是第一个。
func stripArgs(frame string) string {
	if i := strings.LastIndexByte(frame, '('); i > 0 {
		return frame[:i]
	}
	return frame
}

// TestPerfDiagBottleneck 定位并发扩展的拐点归属。
//
// # 要回答的问题
//
// perf_test.go 的 B 场景显示 QPS 在 50 并发见顶（~7900），到 200 并发
// 反而掉到 ~1900。这是「网关有瓶颈」还是「测量回路自己饱和」？
//
// 这个区分是本报告最重要的判断，因为**压测客户端、网关、假上游三者跑在
// 同一台 12 核机器上**。高并发时三者抢同一批 CPU，QPS 下降完全可以
// 由「测量回路自身饱和」解释 —— 那样它就不是网关的缺陷。
//
// # 判据：同一并发下对比「直连假上游」与「经网关」的 QPS 曲线
//
//   - 若**直连**在同样并发下也见顶/下降 → 拐点属于测量回路（CPU/客户端），
//     不是网关；
//   - 若直连继续线性增长而经网关见顶 → 拐点确实在网关内部，
//     再进一步用下面的 DB 微基准定位到具体资源。
type diagPoint struct {
	conc int
	n    int
	qps  float64
	p50  time.Duration
	p99  time.Duration
}

func (p diagPoint) String() string {
	return fmt.Sprintf("conc=%-4d n=%-5d QPS=%-9.1f P50=%-9v P99=%-9v",
		p.conc, p.n, p.qps, p.p50.Round(time.Microsecond), p.p99.Round(time.Microsecond))
}

// measureScaling 以固定样本量与固定并发打 target，返回该点的统计。
//
// **固定 n（而不是 n=并发×8）**：小并发下 n=80 只需 25ms，样本太少、
// 受单次调度抖动影响极大，算出的 QPS 不可信 —— B 场景第一版就是这样，
// conc=10 得到 3243 QPS 而 conc=50 得到 7913 QPS 的「超线性」怪象。
// 固定 n=1500 让每个点都有足够样本，代价只是小并发跑得久一点。
func measureScaling(client *http.Client, target string, conc, n int) diagPoint {
	r := runNonStreamWorkload(client, target, nonStreamBody, conc, n)
	return diagPoint{conc: conc, n: n, qps: r.qps, p50: r.p50, p99: r.p99}
}

func TestDiagBottleneck(t *testing.T) {
	if testing.Short() {
		t.Skip("瓶颈定位在 -short 下跳过")
	}
	good := fakeGood()
	defer good.Close()

	h, db := buildHarness(t, []struct {
		slug, url string
		fail      bool
	}{{"good", good.URL, false}}, false)
	env := newPerfEnv(t, h)

	const n = 1500
	levels := []int{10, 50, 100, 200}

	// 预热两条路径
	dh := perfClient()
	defer dh.CloseIdleConnections()
	for i := 0; i < 50; i++ {
		if code, err := doNonStream(env.client, env.gw.URL, nonStreamBody); err != nil || code != 200 {
			t.Fatalf("预热(网关)失败: code=%d err=%v", code, err)
		}
		if code, err := doNonStream(dh, good.URL, nonStreamBody); err != nil || code != 200 {
			t.Fatalf("预热(直连)失败: code=%d err=%v", code, err)
		}
	}

	t.Logf("--- 对照 1：直连假上游（无网关，测测量回路自身的天花板）---")
	direct := make([]diagPoint, 0, len(levels))
	for _, c := range levels {
		p := measureScaling(dh, good.URL, c, n)
		t.Logf("Diag-direct %s", p)
		direct = append(direct, p)
	}

	t.Logf("--- 对照 2：经网关 ---")
	via := make([]diagPoint, 0, len(levels))
	for _, c := range levels {
		p := measureScaling(env.client, env.gw.URL, c, n)
		t.Logf("Diag-via    %s", p)
		via = append(via, p)
	}

	t.Logf("--- 对比：同一并发下的 QPS（直连 / 经网关 / 比值）---")
	for i, c := range levels {
		ratio := via[i].qps / direct[i].qps
		t.Logf("Diag-cmp conc=%-4d 直连=%-9.1f 经网关=%-9.1f 经网关/直连=%.3f  网关净QPS=%.1f",
			c, direct[i].qps, via[i].qps, ratio,
			1/(1/via[i].qps-1/direct[i].qps))
	}

	// 拐点归属：直连是否也见顶？
	directPeak, directPeakConc := 0.0, 0
	viaPeak, viaPeakConc := 0.0, 0
	for i := range levels {
		if direct[i].qps > directPeak {
			directPeak, directPeakConc = direct[i].qps, levels[i]
		}
		if via[i].qps > viaPeak {
			viaPeak, viaPeakConc = via[i].qps, levels[i]
		}
	}
	t.Logf("Diag-peak 直连峰值=%.1f @conc=%d；经网关峰值=%.1f @conc=%d",
		directPeak, directPeakConc, viaPeak, viaPeakConc)
	if directPeakConc == viaPeakConc && directPeakConc < levels[len(levels)-1] {
		t.Logf("Diag-结论 两者在同一并发见顶 → 拐点属于**测量回路/本机 CPU**，不是网关内部瓶颈")
	} else if viaPeakConc < directPeakConc {
		t.Logf("Diag-结论 经网关先于直连见顶 → 拐点在**网关内部**，看下面的 DB 微基准")
	}

	// —— DB 微基准：把每请求两次数据库操作的天花板量出来 ——
	//
	// 每个请求会做：1 次 GetKeyQuota（读池，4 连接）+ 1 次 usage INSERT
	// （写池，1 连接，异步 worker）。若这两者的天花板低于观测 QPS，
	// 那它们就是瓶颈；若远高于，则瓶颈不在 DB。
	t.Logf("--- DB 微基准（每请求的两次 DB 操作的独立吞吐）---")

	// 读池：4 连接并发跑 GetKeyQuota
	{
		const c = 4
		const ops = 4000
		var wg sync.WaitGroup
		var next int
		var mu sync.Mutex
		start := time.Now()
		for i := 0; i < c; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for {
					mu.Lock()
					if next >= ops {
						mu.Unlock()
						return
					}
					next++
					mu.Unlock()
					_, _, _, _ = db.GetKeyQuota(contextTODO(), "k1")
				}
			}()
		}
		wg.Wait()
		el := time.Since(start)
		t.Logf("Diag-db  GetKeyQuota(读池 %d 连接): %d 次 %v → %.0f ops/s", c, ops, el.Round(time.Millisecond), float64(ops)/el.Seconds())
	}

	// 写池：单连接跑 usage INSERT（与 worker 的路径一致）
	{
		const ops = 2000
		start := time.Now()
		for i := 0; i < ops; i++ {
			rec := &store.UsageRecord{
				ID: generateID(), AccessKeyID: "k1", UserID: testUserID,
				RequestID: generateID(), PublicModel: "flash",
				ProviderID: "good", UpstreamModel: "good-model",
				IngressProtocol: "openai-chat", Status: usageStatusOK,
				InputTokens: 5, OutputTokens: 3, TotalTokens: 8,
			}
			if _, err := db.CreateUsageRecordWithCost(contextTODO(), rec); err != nil {
				t.Fatalf("insert usage: %v", err)
			}
		}
		el := time.Since(start)
		t.Logf("Diag-db  CreateUsageRecordWithCost(写池单连接): %d 次 %v → %.0f ops/s", ops, el.Round(time.Millisecond), float64(ops)/el.Seconds())
	}

	// —— 队列积压的直接证据：把请求量与落库追平时间一起看 ——
	//
	// 若 usage worker（单 goroutine）跟不上到达速率，队列（1024）会满，
	// 之后请求退化为**同步写**（在写池单连接上排队）—— 那正是
	// 「并发越高 QPS 越低」的经典成因。追平时间与请求墙钟的差就是积压量级。
	t.Logf("--- 队列积压检查（单 worker 是否跟得上）---")
	for _, c := range []int{50, 200} {
		before := runtime.NumGoroutine()
		_ = before
		base := usageRowCount(t, db)
		res := runNonStreamWorkload(env.client, env.gw.URL, nonStreamBody, c, 1500)
		drainStart := time.Now()
		var landed int
		for time.Since(drainStart) < 30*time.Second {
			landed = usageRowCount(t, db) - base
			if landed >= 1500 {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Logf("Diag-queue conc=%-4d 请求墙钟=%-9v QPS=%-9.1f → 落库追平=%-9v（落库 %d/1500）",
			c, res.wall.Round(time.Millisecond), res.qps, time.Since(drainStart).Round(time.Millisecond), landed)
	}
}

// usageRowCount 查 usage_records 行数（诊断用，忽略错误）。
func usageRowCount(t *testing.T, db *store.Store) int {
	t.Helper()
	var n int
	if err := db.Reader().QueryRow(`SELECT COUNT(*) FROM usage_records`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

// contextTODO 是 context.Background 的本地别名，避免诊断文件顶部再引一个包
// （本文件其余部分不需要 context）。
func contextTODO() context.Context { return context.Background() }

// TestPerfDiagUsageWriteIsTheCause 用「不产生用量记录的请求」做因果对照。
//
// # 为什么需要这个对照
//
// 上面几条测试建立了相关性：请求吞吐（~5000/s）高于落库上限（~3100/s），
// 高并发持续负载下吞吐单调崩塌。但**相关性不等于因果** ——
// 崩塌也可能来自 CPU 饱和、连接池、或别的共享资源。
//
// # 判据：让请求「成功但不写 usage_records」
//
// 走 model_not_found（模型名不存在）的请求会在 Resolve 阶段就被拒，
// **完全不产生 usage 记录**（usage.record 只在 attempt* 末尾调用）。
// 它同样要过：鉴权、RPM 限速、GetKeyQuota（读池）、白名单、路由解析 ——
// 唯一少掉的就是「入队 + 异步落库」。
//
// 于是对照就干净了：
//   - 若「无用量记录」的请求在同样并发下**不塌**（吞吐保持高位），
//     而「有用量记录」的请求塌 → 因果指向落库路径；
//   - 若两者都塌 → 瓶颈在更前面的共享资源（CPU/读池/客户端），
//     落库只是同时被压到而已。
//
// 注意两边都必须是非流式、同一个假上游、同一并发与样本量。
func TestDiagUsageWriteIsTheCause(t *testing.T) {
	if testing.Short() {
		t.Skip("因果对照在 -short 下跳过")
	}
	good := fakeGood()
	defer good.Close()

	h, db := buildHarness(t, []struct {
		slug, url string
		fail      bool
	}{{"good", good.URL, false}}, false)
	env := newPerfEnv(t, h)

	// 热路径预热（两条路径都要）
	for i := 0; i < 200; i++ {
		if code, err := doNonStream(env.client, env.gw.URL, nonStreamBody); err != nil || code != 200 {
			t.Fatalf("预热失败: code=%d err=%v", code, err)
		}
		if code, _ := doNonStream(env.client, env.gw.URL, notFoundBody); code != http.StatusNotFound {
			t.Fatalf("预热(404 路径)意外状态: %d", code)
		}
	}
	settle()

	const conc = 200
	const total = 6000

	// 手臂 A：正常情况下（每条都写 usage）
	baseA := usageRowCount(t, db)
	rA := runNonStreamWorkload(env.client, env.gw.URL, nonStreamBody, conc, total)
	rowsA := usageRowCount(t, db) - baseA
	t.Logf("Diag-cause A 有用量记录: 吞吐=%.0f req/s P50=%v P99=%v ok=%d 新增 usage 行=%d",
		rA.qps, rA.p50.Round(time.Microsecond), rA.p99.Round(time.Microsecond), rA.ok, rowsA)

	// 等 A 的落库彻底清空，避免它的尾巴污染 B。
	{
		d := time.Now()
		for time.Since(d) < 30*time.Second {
			if usageRowCount(t, db)-baseA >= total {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	settle()
	baseB := usageRowCount(t, db)

	// 手臂 B：model_not_found（成功走到路由解析，但不产生 usage 记录）
	var (
		mu    sync.Mutex
		lat   []time.Duration
		codes = map[int]int{}
		next  atomic.Int64
		wg    sync.WaitGroup
	)
	startB := time.Now()
	for w := 0; w < conc; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				if int(next.Add(1)) > total {
					return
				}
				t0 := time.Now()
				code, _ := doNonStream(env.client, env.gw.URL, notFoundBody)
				d := time.Since(t0)
				mu.Lock()
				lat = append(lat, d)
				codes[code]++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	wallB := time.Since(startB)
	sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
	rowsB := usageRowCount(t, db) - baseB
	qpsB := float64(total) / wallB.Seconds()
	t.Logf("Diag-cause B 无用量记录(404): 吞吐=%.0f req/s P50=%v P99=%v codes=%v 新增 usage 行=%d",
		qpsB, percentile(lat, 50).Round(time.Microsecond), percentile(lat, 99).Round(time.Microsecond), codes, rowsB)

	t.Logf("Diag-cause 对比: A(写用量)=%.0f req/s vs B(不写用量)=%.0f req/s → 比值 %.2f×",
		rA.qps, qpsB, qpsB/rA.qps)
	if rowsB == 0 && qpsB > rA.qps*2 {
		t.Logf("Diag-cause 结论 去掉「入队+落库」后吞吐提升 %.1f× → 因果确认："+
			"高并发吞吐崩塌由 **usage 落库路径**（单 worker + 单连接写池）引起", qpsB/rA.qps)
	} else if rowsB == 0 {
		t.Logf("Diag-cause 结论 去掉落库后吞吐仅 %.2f× → 不足以归因于落库，瓶颈在更前面的共享资源", qpsB/rA.qps)
	} else {
		t.Logf("Diag-cause 注意 B 也产生了 %d 条 usage 行（预期 0）—— 对照不成立，结论不可用", rowsB)
	}
}

// notFoundBody 是一个模型名不存在的合法请求：会被 Resolve 拒掉（404），
// 不触碰上游、也不产生 usage 记录。用于上面的因果对照。
const notFoundBody = `{"model":"no-such-model","messages":[{"role":"user","content":"hi"}],"stream":false}`

// TestPerfDiagUsageWriteCeiling 定位「用量落库」这条链的吞吐上限。
//
// # 为什么怀疑它
//
// 请求吞吐在高并发下量到 4000~6000/s（见 Diag-variance），而
// CreateUsageRecordWithCost 的独立微基准只量到 ~1733 ops/s。**每个成功请求
// 恰好产生一条用量记录**，所以若落库上限真的低于请求吞吐，队列（1024）
// 迟早填满，随后 record() 走同步回退分支 —— 请求就在写池单连接上排队，
// 这正好能解释「并发越高 QPS 越低」。
//
// 但「怀疑」不够。这一步把这条链拆成三段分别量，看瓶颈落在哪：
//  1. 纯 INSERT（含触发器）—— SQLite 写池单连接的真实上限；
//  2. freezeUsageCost 的读池查价 —— 是否反而比写更贵（读池 4 连接）；
//  3. 两者相加（= 真实 worker 路径）。
//
// # 还要验证「行数增长是否让写变慢」
//
// 触发器 trg_update_used_tokens 在每次 INSERT 后更新 access_keys 与
// usage_totals。若那两条 UPDATE 依赖的索引缺失，随明细表增长写会越来越慢 ——
// 那是一个会随运行时间恶化的缺陷，而不是常数瓶颈。所以这里在空表与
// 满表（已插入 2 万行）两种状态下各量一次。
func TestDiagUsageWriteCeiling(t *testing.T) {
	if testing.Short() {
		t.Skip("写吞吐定位在 -short 下跳过")
	}
	good := fakeGood()
	defer good.Close()
	h, db := buildHarness(t, []struct {
		slug, url string
		fail      bool
	}{{"good", good.URL, false}}, false)
	_ = h

	mkRec := func() *store.UsageRecord {
		return &store.UsageRecord{
			ID: generateID(), AccessKeyID: "k1", UserID: testUserID,
			RequestID: generateID(), PublicModel: "flash",
			ProviderID: "good", UpstreamModel: "good-model",
			IngressProtocol: "openai-chat", Status: usageStatusOK,
			InputTokens: 5, OutputTokens: 3, TotalTokens: 8,
		}
	}

	// 1. 读池查价单独计时。
	//
	// 直接跑 freezeUsageCost 用的那条 SQL（unexported，跨包调不到），
	// 口径一致：同样的 SELECT、同样的参数、同样走读池。
	//
	// **注意 harness 里没有 upstream_models 行**（路由是内存 RouteIndex，
	// 没有落库），所以这里会得到 sql.ErrNoRows —— 而生产里 bootstrap/管理面
	// 建过模型后那一行是存在的。两种形态都量：命中的成本才是生产口径。
	{
		const ops = 3000
		noRow := 0
		start := time.Now()
		for i := 0; i < ops; i++ {
			var pin, phit, pout float64
			err := db.Reader().QueryRow(
				`SELECT COALESCE(price_input, 0), COALESCE(price_cache_hit, 0), COALESCE(price_output, 0)
				   FROM upstream_models WHERE provider_id = ? AND model_id = ?`,
				"good", "good-model").Scan(&pin, &phit, &pout)
			switch {
			case err == sql.ErrNoRows:
				noRow++
			case err != nil:
				t.Fatalf("price lookup: %v", err)
			}
		}
		el := time.Since(start)
		t.Logf("Diag-usage (a) 读池查价(串行)  %d 次 %v → %.0f ops/s（未命中 %d/%d）",
			ops, el.Round(time.Millisecond), float64(ops)/el.Seconds(), noRow, ops)
	}

	// 1b. 插入一条价格行，让完整路径走**生产形态**（查价命中）。
	//
	// 不插的话每次落库都会多一次失败的读 + 一条 WARN 日志，
	// 量出来的不是生产口径。
	if err := db.CreateUpstreamModel(contextTODO(), &store.UpstreamModel{
		ID: "um-diag", ProviderID: "good", ModelID: "good-model",
		Enabled: true, PriceInput: 1, PriceOutput: 2,
	}); err != nil {
		t.Logf("Diag-usage 插入价格行失败（%v）：完整路径将走「查不到价」分支", err)
	}

	// 2. 完整 worker 路径（查价 + INSERT），空表附近
	measureFull := func(label string, ops int) float64 {
		start := time.Now()
		for i := 0; i < ops; i++ {
			if _, err := db.CreateUsageRecordWithCost(contextTODO(), mkRec()); err != nil {
				t.Fatalf("insert: %v", err)
			}
		}
		el := time.Since(start)
		rate := float64(ops) / el.Seconds()
		t.Logf("Diag-usage %s: %d 次 %v → %.0f ops/s", label, ops, el.Round(time.Millisecond), rate)
		return rate
	}

	emptyRate := measureFull("(b) 完整路径(空表)", 2000)

	// 3. 灌到 2 万行，再量一次 —— 验证是否随表增长恶化
	for i := 0; i < 18000; i++ {
		if _, err := db.CreateUsageRecordWithCost(contextTODO(), mkRec()); err != nil {
			t.Fatalf("bulk insert: %v", err)
		}
	}
	rows := usageRowCount(t, db)
	grownRate := measureFull(fmt.Sprintf("(c) 完整路径(%d 行)", rows), 2000)

	t.Logf("Diag-usage 汇总: 空表 %.0f ops/s → %d 行后 %.0f ops/s（变化 %+.1f%%）",
		emptyRate, rows, grownRate, (grownRate-emptyRate)/emptyRate*100)

	// 4. **并发**写吞吐：这才是「队列满后同步回退」的真实速度。
	//
	// 上面 (b)/(c) 都是**串行**循环，量到的是单线程上限（~3100/s）。
	// 但同步回退发生在**请求 goroutine** 里 —— 高并发时几百个 goroutine
	// 同时抢写池那**唯一一条**连接。竞争下的实际吞吐可能远低于串行值，
	// 而那个数字才是「背压时请求被压到多少」的直接答案。
	//
	// 为什么值得单独量：Diag-cause 显示有用量记录的请求在 conc=200 下只有
	// 187 req/s，而串行写能到 3100/s —— 差 16 倍。这个差必须由测量解释，
	// 不能靠猜。
	{
		const writers = 200
		const opsEach = 25 // 200×25 = 5000 次
		var wg sync.WaitGroup
		start := time.Now()
		for w := 0; w < writers; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 0; i < opsEach; i++ {
					if _, err := db.CreateUsageRecordWithCost(contextTODO(), mkRec()); err != nil {
						return
					}
				}
			}()
		}
		wg.Wait()
		el := time.Since(start)
		concRate := float64(writers*opsEach) / el.Seconds()
		t.Logf("Diag-usage (d) 完整路径(**%d 并发**, %d 次): %v → %.0f ops/s"+
			"（对比串行 %.0f ops/s：并发只有 %.2f× 串行 → 写池单连接是串行化点）",
			writers, writers*opsEach, el.Round(time.Millisecond), concRate, grownRate, concRate/grownRate)
	}

	t.Logf("Diag-usage 对比: 请求吞吐（并发50，见 Diag-variance）约 4100~5900/s。" +
		"落库若低于它，队列会填满并触发同步回退 —— 那会把请求吞吐压到落库速率上。")
}

// TestPerfDiagSustainedBackpressure 验证「用量落库速率是不是整个网关的稳态上限」。
//
// # 推理链（每一步都有上面的实测数字支撑）
//
//  1. 每个成功请求恰好产生 1 条 usage_records（attemptNonStream / attemptStream
//     末尾的 usage.record）。
//  2. 落库由**单个** worker goroutine 串行做（usageWorkers=1），
//     实测上限约 3100 ops/s（20k 行后降到 2346，见 Diag-usage）。
//  3. 队列容量 1024，满了之后 record() 退化为**在请求路径上同步写**。
//  4. 于是：持续请求速率 > 落库速率时，队列必然填满，此后每个请求都要等
//     一次同步 INSERT —— 请求吞吐被钉在落库速率上，延迟随队列长度增长。
//
// # 判据：看「前半段」与「后半段」的吞吐差
//
// 队列吸收的是**突发**（前 1024 条），持续超载则表现为后段变慢。
// 所以把一次长压测切成若干时间窗，比较各窗的完成数与 P50：
//   - 若各窗吞吐基本持平 → 队列够用，落库不是瓶颈；
//   - 若后段吞吐收敛到 ~落库速率且延迟抬升 → 背压成立，瓶颈确认在落库。
//
// 样本量刻意取大（超过队列容量 1024 的 ~6 倍），否则测的还是「突发吸收」。
func TestDiagSustainedBackpressure(t *testing.T) {
	if testing.Short() {
		t.Skip("持续背压在 -short 下跳过")
	}
	good := fakeGood()
	defer good.Close()

	h, db := buildHarness(t, []struct {
		slug, url string
		fail      bool
	}{{"good", good.URL, false}}, false)
	env := newPerfEnv(t, h)

	for i := 0; i < 200; i++ {
		if code, err := doNonStream(env.client, env.gw.URL, nonStreamBody); err != nil || code != 200 {
			t.Fatalf("预热失败: code=%d err=%v", code, err)
		}
	}
	settle()
	// 基数必须在压测**之前**取：落库是异步的，压测期间写的是 [base, base+total)。
	// （第一版在压测之后取，于是永远等不到那 total 条 —— 白等 60s 超时，
	//  算出「17 ops/s」这种毫无意义的数字。）
	base := usageRowCount(t, db)

	const conc = 200
	const total = 6000

	type bucket struct {
		count int
		lat   []time.Duration
	}
	const nbuckets = 6
	buckets := make([]bucket, nbuckets)
	var mu sync.Mutex
	var next atomic.Int64
	var wg sync.WaitGroup

	start := time.Now()
	for w := 0; w < conc; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				if int(next.Add(1)) > total {
					return
				}
				t0 := time.Now()
				code, err := doNonStream(env.client, env.gw.URL, nonStreamBody)
				d := time.Since(t0)
				elapsed := time.Since(start)
				b := int(float64(elapsed) / float64(time.Second))
				if b >= nbuckets {
					b = nbuckets - 1
				}
				mu.Lock()
				if err == nil && code == 200 {
					buckets[b].count++
					buckets[b].lat = append(buckets[b].lat, d)
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	wall := time.Since(start)

	t.Logf("Diag-bp 持续压测: conc=%d total=%d wall=%v 总吞吐=%.0f req/s（落库上限实测约 3100/s）",
		conc, total, wall.Round(time.Millisecond), float64(total)/wall.Seconds())
	for i, b := range buckets {
		if len(b.lat) == 0 {
			t.Logf("Diag-bp 第 %ds 窗口: 无样本", i)
			continue
		}
		sort.Slice(b.lat, func(x, y int) bool { return b.lat[x] < b.lat[y] })
		t.Logf("Diag-bp 第 %ds 窗口: 完成=%-5d 该窗吞吐=%-8.0f req/s P50=%-9v P99=%-9v",
			i, b.count, float64(b.count), percentile(b.lat, 50).Round(time.Microsecond),
			percentile(b.lat, 99).Round(time.Microsecond))
	}

	// 落库追平：请求全返回后还要多久才把 total 条写完。
	//
	// **不先 settle**：settle 会 sleep 数百毫秒 + 两轮 GC，那段时间落库照跑，
	// 会把「追平耗时」量少。要量的是「请求墙钟结束时还欠多少、以什么速率还」。
	drainStart := time.Now()
	var landed int
	for time.Since(drainStart) < 60*time.Second {
		landed = usageRowCount(t, db) - base
		if landed >= total {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	drainEl := time.Since(drainStart)
	t.Logf("Diag-bp 落库追平: 请求墙钟=%v 结束后又用了 %v 才写完（落库 %d/%d）→ 追平期速率 ≈ %.0f ops/s",
		wall.Round(time.Millisecond), drainEl.Round(time.Millisecond), landed, total,
		float64(landed)/drainEl.Seconds())
	t.Logf("Diag-bp 端到端: %d 条从压测开始到全部落库共 %v → 端到端 %.0f ops/s",
		total, (wall + drainEl).Round(time.Millisecond), float64(total)/(wall+drainEl).Seconds())
	t.Logf("Diag-bp 结论判据: 落库速率明显低于请求吞吐、且需在请求结束后继续追平 → " +
		"队列为突发吸收缓冲，持续负载下背压成立")
}

// TestPerfDiagVariance 量测量的**可重复性**。
//
// # 为什么这个必须先做
//
// 瓶颈定位时出现了一个自相矛盾的结果：同一台机器、同一个网关、同为
// conc=50，一次量到 2355 QPS，另一次（几分钟后、不同测试里）量到 5901 QPS。
// 差 2.5 倍。在这种抖动下谈「拐点在哪个并发」是没有意义的 ——
// 数字的噪声比要检测的效应还大。
//
// 所以先回答一个问题：**同一场景重复 N 次，QPS 的离散度有多大？**
//   - 若离散度 < 10%：前面的矛盾来自「前置负载不同」，可以归因；
//   - 若离散度 > 50%：本机的性能数字只能作**量级**参考，
//     报告里必须明写这一点，不能拿它去做精细的拐点判断。
//
// 这个结论直接决定报告怎么写，所以它是本文件里最重要的一条测试。
func TestDiagVariance(t *testing.T) {
	if testing.Short() {
		t.Skip("方差测量在 -short 下跳过")
	}
	good := fakeGood()
	defer good.Close()

	h, _ := buildHarness(t, []struct {
		slug, url string
		fail      bool
	}{{"good", good.URL, false}}, false)
	env := newPerfEnv(t, h)

	for i := 0; i < 100; i++ {
		if code, err := doNonStream(env.client, env.gw.URL, nonStreamBody); err != nil || code != 200 {
			t.Fatalf("预热失败: code=%d err=%v", code, err)
		}
	}

	const conc = 50
	const n = 1500
	const rounds = 5

	var qps []float64
	var p50s []time.Duration
	for i := 0; i < rounds; i++ {
		// 每轮之间 settle：让上一轮的 usage 落库追平、GC 收尾，
		// 尽量让每轮起点一致（否则量的是「上一轮的尾巴」）。
		settle()
		time.Sleep(200 * time.Millisecond)
		r := runNonStreamWorkload(env.client, env.gw.URL, nonStreamBody, conc, n)
		qps = append(qps, r.qps)
		p50s = append(p50s, r.p50)
		t.Logf("Diag-var round=%d QPS=%-9.1f P50=%-9v P99=%-9v codes=%v",
			i+1, r.qps, r.p50.Round(time.Microsecond), r.p99.Round(time.Microsecond), r.codes)
	}

	sorted := make([]float64, len(qps))
	copy(sorted, qps)
	sort.Float64s(sorted)
	lo, hi := sorted[0], sorted[len(sorted)-1]
	mid := sorted[len(sorted)/2]
	spread := (hi - lo) / mid * 100
	t.Logf("Diag-var 汇总 conc=%d n=%d rounds=%d: min=%.1f median=%.1f max=%.1f 极差/中位=%.1f%%",
		conc, n, rounds, lo, mid, hi, spread)
	if spread > 50 {
		t.Logf("Diag-var 结论 离散度 %.1f%% > 50%% —— 本机数字只能作量级参考，"+
			"不足以支撑精细的拐点判断（报告须明写）", spread)
	} else {
		t.Logf("Diag-var 结论 离散度 %.1f%% 可接受，数字可用于比较不同并发级别", spread)
	}
	// 不断言：这是测量实验，不是门禁。它的产出是「数字能不能信」这个结论。
}

// TestPerfDiagGoroutineStacksSlowUpstream 复现 D3 场景并打印残留 goroutine 的栈。
//
// 手法与 D3 完全相同（同一个假上游、同一并发），差别只在最后多打一份栈。
// 单独写而不是改 D3：D3 是门禁用例，诊断代码不该混进门禁
// （它依赖 runtime.Stack，输出不可控且慢）。
func TestDiagGoroutineStacksSlowUpstream(t *testing.T) {
	if testing.Short() {
		t.Skip("诊断用例在 -short 下跳过")
	}

	slow := fakeSlowStream(80*time.Millisecond, 10)
	defer slow.Close()

	h, _ := buildHarness(t, []struct {
		slug, url string
		fail      bool
	}{{"slow", slow.URL, false}}, false)
	env := newPerfEnv(t, h)

	// 预热
	for i := 0; i < 10; i++ {
		if _, _, code, err := doStream(env.client, env.gw.URL, streamBody); err != nil || code != 200 {
			t.Fatalf("预热失败: code=%d err=%v", code, err)
		}
	}
	settle()
	before := runtime.NumGoroutine()
	t.Logf("Diag 基线 goroutine=%d", before)

	const conc = 100
	const total = 300
	var (
		wg   sync.WaitGroup
		next int
		mu   sync.Mutex
	)
	work := make(chan struct{}, conc)
	for i := 0; i < conc; i++ {
		work <- struct{}{}
	}
	for w := 0; w < conc; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				mu.Lock()
				if next >= total {
					mu.Unlock()
					return
				}
				next++
				mu.Unlock()
				_, _, _, _ = doStream(env.client, env.gw.URL, streamBody)
			}
		}()
	}
	wg.Wait()
	t.Logf("Diag 请求全部返回，立刻读=%d", runtime.NumGoroutine())

	// 逐步观察回落过程：区分「异步收尾慢」与「永不退出」。
	for _, d := range []time.Duration{50 * time.Millisecond, 200 * time.Millisecond, 500 * time.Millisecond, time.Second, 2 * time.Second} {
		time.Sleep(d)
		runtime.GC()
		t.Logf("Diag settle +%-6v → goroutine=%d", d, runtime.NumGoroutine())
	}

	after := runtime.NumGoroutine()
	t.Logf("Diag 最终 goroutine=%d (delta %+d)", after, after-before)
	prof := goroutineProfile()
	limit := 25
	if len(prof) < limit {
		limit = len(prof)
	}
	for _, line := range prof[:limit] {
		t.Logf("Diag 栈聚合 | %s", line)
	}

	// —— 决定性实验 1：关客户端空闲连接 ——
	//
	// 上面残留的 goroutine 若是**空闲 keep-alive 连接**，关掉连接池就应该
	// 让它们消失；若它们是「漏听 ctx 的请求/心跳 goroutine」，关池毫无影响。
	// 比读栈猜「persistConn 是谁的」可靠得多（网关与压测客户端在同一个
	// 进程里，栈上分不出归属）。
	env.client.CloseIdleConnections()
	for _, d := range []time.Duration{100 * time.Millisecond, 500 * time.Millisecond} {
		time.Sleep(d)
		runtime.GC()
		t.Logf("Diag 关客户端池后 +%-6v → goroutine=%d", d, runtime.NumGoroutine())
	}
	residual := runtime.NumGoroutine()
	t.Logf("Diag 关客户端池后残留=%d（基线 %d，差 %+d）—— 这部分只可能是"+
		"网关侧到假上游的空闲连接（idle pool）或真实泄漏", residual, before, residual-before)

	// —— 决定性实验 2：泄漏随「请求数」增长，空闲池随「并发数」封顶 ——
	//
	// 这是区分两者的**唯一可靠判据**，且不需要碰生产代码：
	//   - 若每次请求漏一条 → 请求数 ×10，残留也 ×10；
	//   - 若是空闲连接池 → 残留由并发数（同时打开过的连接数）决定，
	//     与总请求数无关，请求数 ×10 残留基本不变。
	t.Logf("Diag --- 判据实验：同并发下把请求数 ×10 ---")
	for _, totalReq := range []int{100, 1000} {
		// 每个规模用**全新的客户端**，避免上一轮的连接池干扰。
		c := perfClient()
		var wg2 sync.WaitGroup
		var mu2 sync.Mutex
		n2 := 0
		for w := 0; w < 50; w++ {
			wg2.Add(1)
			go func() {
				defer wg2.Done()
				for {
					mu2.Lock()
					if n2 >= totalReq {
						mu2.Unlock()
						return
					}
					n2++
					mu2.Unlock()
					_, _, _, _ = doStream(c, env.gw.URL, streamBody)
				}
			}()
		}
		wg2.Wait()
		time.Sleep(300 * time.Millisecond)
		runtime.GC()
		withPool := runtime.NumGoroutine()
		c.CloseIdleConnections()
		time.Sleep(300 * time.Millisecond)
		runtime.GC()
		afterClose := runtime.NumGoroutine()
		t.Logf("Diag 请求=%-5d 并发=50 → 关池前=%d 关池后=%d（基线 %d）",
			totalReq, withPool, afterClose, before)
	}
	t.Logf("Diag 结论判据: 若「请求 100→1000」后关池残留**基本不变** → 空闲池(非泄漏)；"+
		"若随之 ×10 → 真实泄漏。基线=%d", before)
}
