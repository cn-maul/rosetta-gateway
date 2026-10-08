package main

// perf_test.go —— 数据面性能与资源泄漏**实测**。
//
// # 为什么要有这个文件
//
// 「应该很快」「goroutine 会回收」都不是结论。这里的每条结论都必须来自
// 一次真实执行 + 一组原始数字，因为性能缺陷与泄漏的共同特征是
// **代码读起来完全正确**：没有死循环、没有忘记 cancel、锁也都在正确的位置，
// 问题只出现在「量」上（队列积压、单写锁饱和、连接不复用）。
//
// # 测量口径（读结论前必须先读这一段）
//
//   - **网关自身开销**：假上游零延迟（handler 里没有任何 sleep，
//     响应体在内存里拼好直接写）。所以下面「非流式基线」的数字
//     ≈ 网关开销 + 本机 loopback RTT。文件里另有一组
//     「直连假上游」的对照测量，两者之差才是纯网关成本。
//   - **上游耗时**：默认的 fakeGood 不含任何人为延迟，任何想测「慢上游」
//     的场景都显式用带 sleep 的假上游，并在报告里标注。
//   - **客户端连接池**：默认 http.Transport 的 MaxIdleConnsPerHost=2，
//     200 并发下会退化成「每请求新建 TCP 连接」，量到的是握手成本而不是
//     网关成本。所以这里显式把空闲连接池调大 —— 这是让测量指向被测对象
//     的必要前提，不是「调优」。
//   - **预热**：首次请求要付连接建立、SDK 客户端构建、SQLite 预编译语句、
//     快照/池的懒初始化。所有分位数统计都在预热之后开始，否则量到的是
//     冷启动而不是稳态。
//   - **分位数而非均值**：均值会把长尾掩盖成「还行」。这里一律排序后取
//     P50/P95/P99，并附 min/max。
//
// # 与其它测试的关系
//
// 复用 failover_test.go 的 buildHarness / buildHarnessFull / fakeGood /
// fakeStreamGood / fakeStreamHang，不重造脚手架。**不修改任何生产代码**：
// 发现的瓶颈只写进 AUDIT/test-perf.md，不在本文件里"顺手优化"。
//
// # 不在本文件里跑的东西
//
// `go test -race` 在本机跑不起来（MinGW 路径含空格，链接期失败），
// 所以这里的并发正确性只由「计数守恒 + 无 panic + 无泄漏」间接保证，
// **数据竞争需要 CI（Linux）上的 -race 才能确认**。这一条写在报告里。

import (
	"bufio"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cn-maul/rosetta-gateway/internal/store"
)

// ---------------------------------------------------------------------------
// 分位数与结果统计
// ---------------------------------------------------------------------------

// perfResult 是一次压测的完整结果。字段刻意齐全（含状态码分布 + 墙钟）：
// 只看 QPS 无法区分「全部成功」与「一半 429、一半飞快」——
// 后者 QPS 可能更高，但那是限速器在替我拒绝，不是网关能力。
type perfResult struct {
	concurrency int
	total       int
	ok          int
	failed      int
	p50         time.Duration
	p95         time.Duration
	p99         time.Duration
	min         time.Duration
	max         time.Duration
	mean        time.Duration
	qps         float64
	wall        time.Duration
	codes       map[int]int
}

// percentile 取**最近秩**分位数（ceil），不做插值。
//
// 为什么不用插值：这里的样本量不大（数百），插值会给出一个「没有任何一次
// 请求真的达到过」的数字，而结论要用它去判断看门狗/超时是否够用 ——
// 那种场景下必须用真实观测值。ceil 保证取到的值一定来自某一次真实请求。
func percentile(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(math.Ceil(p/100*float64(len(sorted)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

func summarize(concurrency int, total int, lat []time.Duration, ok, failed int, wall time.Duration, codes map[int]int) perfResult {
	sorted := make([]time.Duration, len(lat))
	copy(sorted, lat)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })

	var sum time.Duration
	for _, d := range sorted {
		sum += d
	}
	r := perfResult{
		concurrency: concurrency, total: total, ok: ok, failed: failed,
		p50: percentile(sorted, 50), p95: percentile(sorted, 95), p99: percentile(sorted, 99),
		wall: wall, codes: codes,
	}
	if len(sorted) > 0 {
		r.min, r.max = sorted[0], sorted[len(sorted)-1]
		r.mean = sum / time.Duration(len(sorted))
	}
	if wall > 0 {
		r.qps = float64(total) / wall.Seconds()
	}
	return r
}

// logf 打印一行结果。所有关键数字必须**出现在测试输出里**，
// 否则报告里的数字就无法与原始输出对上。
func (r perfResult) logf(t *testing.T, label string) {
	t.Helper()
	t.Logf("%-34s conc=%-4d n=%-5d ok=%-5d fail=%-4d P50=%-9v P95=%-9v P99=%-9v min=%-9v max=%-9v QPS=%-9.1f wall=%-9v codes=%v",
		label, r.concurrency, r.total, r.ok, r.failed,
		r.p50.Round(time.Microsecond), r.p95.Round(time.Microsecond), r.p99.Round(time.Microsecond),
		r.min.Round(time.Microsecond), r.max.Round(time.Microsecond), r.qps,
		r.wall.Round(time.Millisecond), r.codes)
}

// ---------------------------------------------------------------------------
// 假上游
// ---------------------------------------------------------------------------

// fakeStreamMany 发出一条含 n 个 content 事件的正常 SSE 流，可选每事件延迟。
//
// 存在的理由：fakeStreamGood 只发 3 个事件，无法用来验证「长流是否随长度
// 线性吃内存」。而流式路径是全项目唯一会**在响应生命周期内持续分配**的地方
// （每个事件要过一遍协议转换、写 SSE 帧），也是最可能 O(n) 累积的地方。
func fakeStreamMany(n int, perEvent time.Duration) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl, _ := w.(http.Flusher)
		emit := func(delta string, finish string) {
			fmt.Fprintf(w, "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"created\":0,"+
				"\"model\":\"good-model\",\"choices\":[{\"index\":0,\"delta\":{\"content\":%q},"+
				"\"finish_reason\":%s}]}\n\n", delta, finish)
			if fl != nil {
				fl.Flush()
			}
		}
		emit("", "null")
		for i := 0; i < n; i++ {
			if perEvent > 0 {
				select {
				case <-time.After(perEvent):
				case <-r.Context().Done():
					return
				}
			}
			emit(strings.Repeat("x", 8), "null")
		}
		fmt.Fprint(w, "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"created\":0,"+
			"\"model\":\"good-model\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
		if fl != nil {
			fl.Flush()
		}
	}))
}

// fakeUpstreamWithConnStats 与 fakeGood 同行为，但带**连接计数**。
//
// 连接泄漏只能在假上游侧观测：网关侧的 http.Transport 连接池是黑盒
// （能拿到 IdleConn 数，但拿不到「对端还认为连着几条」）。
// 用 ConnState 钩子统计「同时处于已建立状态的连接数」——
// 请求全部结束后若它不回到 ~0，就是网关没有释放上游连接。
type connStats struct {
	open    atomic.Int64
	peak    atomic.Int64
	total   atomic.Int64 // 累计建立过的连接数（用于判断是否复用）
	dropped atomic.Int64
}

func (c *connStats) bumpPeak(n int64) {
	for {
		p := c.peak.Load()
		if n <= p || c.peak.CompareAndSwap(p, n) {
			return
		}
	}
}

func newFakeUpstreamWithConnStats() (*httptest.Server, *connStats) {
	cs := &connStats{}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"cmpl-1","object":"chat.completion","created":0,
			"model":"good-model",
			"choices":[{"index":0,"message":{"role":"assistant","content":"pong"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}}`))
	}))
	srv.Config.ConnState = func(c net.Conn, s http.ConnState) {
		switch s {
		case http.StateNew:
			n := cs.open.Add(1)
			cs.total.Add(1)
			cs.bumpPeak(n)
		case http.StateClosed, http.StateHijacked:
			cs.open.Add(-1)
			cs.dropped.Add(1)
		}
	}
	srv.Start()
	return srv, cs
}

// fakeSlowStream 先延迟 ttft 再开始吐字，用于「慢上游」场景。
// 与 fakeStreamHang 的区别：hang 是永不吐字（测看门狗），这个是慢但会成功
// （测高并发下网关是否被慢流拖住）。
func fakeSlowStream(delay time.Duration, events int) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl, _ := w.(http.Flusher)
		select {
		case <-time.After(delay):
		case <-r.Context().Done():
			return
		}
		for i := 0; i < events; i++ {
			fmt.Fprintf(w, "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"created\":0,"+
				"\"model\":\"good-model\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"x\"},"+
				"\"finish_reason\":null}]}\n\n")
			if fl != nil {
				fl.Flush()
			}
		}
		fmt.Fprint(w, "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"created\":0,"+
			"\"model\":\"good-model\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
		if fl != nil {
			fl.Flush()
		}
	}))
}

// ---------------------------------------------------------------------------
// 网关与客户端包装
// ---------------------------------------------------------------------------

// perfClient 是压测用的 HTTP 客户端。
//
// MaxIdleConnsPerHost 必须显式调大：默认值 2 意味着 200 并发下几乎每个请求
// 都要新建 TCP 连接，量出来的「延迟」主要是三次握手 + 慢启动，
// 完全掩盖网关自身开销。DisableCompression 关掉 gzip 协商——
// SSE 响应不需要它，开着只会让每次响应多一层 reader。
func perfClient() *http.Client {
	return &http.Client{
		Timeout: 120 * time.Second,
		Transport: &http.Transport{
			MaxIdleConns:          512,
			MaxIdleConnsPerHost:   512,
			IdleConnTimeout:       90 * time.Second,
			DisableCompression:    true,
			ForceAttemptHTTP2:     false,
			ResponseHeaderTimeout: 60 * time.Second,
		},
	}
}

// startGateway 把数据面 handler 挂到一个真实 HTTP 服务器上。
//
// **为什么必须走真实 HTTP 而不是直接调 handler**：
//   - 连接复用、连接泄漏、keep-alive 行为只在真实 TCP 栈上存在；
//   - handler 直调时 w 是 httptest.ResponseRecorder，它**不实现
//     http.Flusher**，流式路径的 Flush 会退化成空操作，TTFT 就测不到
//     （会得到「TTFT == 总耗时」这种假数字）。
func startGateway(t *testing.T, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

// doNonStream 发一次非流式请求并读干响应体。
//
// 必须读干（io.Copy → io.Discard）：不读干 body 就不会归还连接到连接池，
// 于是「连接泄漏」的测量会变成「客户端自己没归还」的假阳性。
func doNonStream(client *http.Client, base, body string) (int, error) {
	req, err := http.NewRequest(http.MethodPost, base+"/v1/chat/completions", strings.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+testAccessKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode, nil
}

// doStream 发一次流式请求，返回 TTFT（首字节）与总耗时。
//
// 用 bufio.Reader 读**首字节**而不是首个事件：TTFT 的运营含义是
// 「用户多久看到第一个字」，对应的是字节到达时刻。读首个完整 SSE 帧会把
// 帧内剩余字节的等待也算进去，在高并发下这个差会被放大。
func doStream(client *http.Client, base, body string) (ttft, total time.Duration, status int, err error) {
	req, err := http.NewRequest(http.MethodPost, base+"/v1/chat/completions", strings.NewReader(body))
	if err != nil {
		return 0, 0, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+testAccessKey)
	req.Header.Set("Content-Type", "application/json")
	t0 := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return 0, time.Since(t0), 0, err
	}
	defer resp.Body.Close()
	br := bufio.NewReaderSize(resp.Body, 4096)
	if _, rerr := br.ReadByte(); rerr != nil {
		return 0, time.Since(t0), resp.StatusCode, rerr
	}
	ttft = time.Since(t0)
	_, _ = io.Copy(io.Discard, br)
	return ttft, time.Since(t0), resp.StatusCode, nil
}

// runNonStreamWorkload 以固定并发打 total 次非流式请求。
//
// 用 atomic 发号而不是「每 worker 固定 N 次」：后者在 worker 间耗时不均时
// 会让末尾几个 worker 拖长墙钟，QPS 被低估。
func runNonStreamWorkload(client *http.Client, base, body string, concurrency, total int) perfResult {
	var (
		mu    sync.Mutex
		lat   []time.Duration
		codes = map[int]int{}
		ok    int
		fail  int
		next  atomic.Int64
		wg    sync.WaitGroup
	)
	start := time.Now()
	for w := 0; w < concurrency; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				if int(next.Add(1)) > total {
					return
				}
				t0 := time.Now()
				code, err := doNonStream(client, base, body)
				d := time.Since(t0)
				mu.Lock()
				lat = append(lat, d)
				if err != nil {
					codes[0]++
					fail++
				} else {
					codes[code]++
					if code == http.StatusOK {
						ok++
					} else {
						fail++
					}
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	return summarize(concurrency, total, lat, ok, fail, time.Since(start), codes)
}

// nonStreamBody 是压测用的最小合法请求体。
const nonStreamBody = `{"model":"flash","messages":[{"role":"user","content":"hi"}],"stream":false}`
const streamBody = `{"model":"flash","messages":[{"role":"user","content":"hi"}],"stream":true}`

// settle 等运行时的异步清理落地后再读资源计数。
//
// 只 sleep 一次是不够的：net/http 关闭连接、http.Transport 回收空闲连接、
// 被取消的请求 goroutine 退出都是**异步**的。GC 两轮是为了让第一轮
// 把本次压测产生的垃圾标记掉、第二轮拿到稳定的 HeapInuse。
func settle() {
	for i := 0; i < 3; i++ {
		runtime.GC()
		time.Sleep(120 * time.Millisecond)
	}
	runtime.GC()
	time.Sleep(120 * time.Millisecond)
}

// usageCount 查 usage_records 行数（通过读池，不干扰写池）。
func usageCount(t *testing.T, env *perfEnv) int {
	t.Helper()
	var n int
	if err := env.db.Reader().QueryRow(`SELECT COUNT(*) FROM usage_records`).Scan(&n); err != nil {
		t.Fatalf("count usage_records: %v", err)
	}
	return n
}

// waitUsageCount 轮询直到 usage_records 达到 want 行（或超时）。
//
// 用量是**异步**落库的（usageWorkers=1），请求返回 ≠ 已落库。
// 任何「记录有没有丢」的断言都必须先等落库追平，否则会把
// 「还没写完」误判成「写丢了」。
func waitUsageCount(t *testing.T, env *perfEnv, want int, timeout time.Duration) (int, time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	start := time.Now()
	for {
		n := usageCount(t, env)
		if n >= want {
			return n, time.Since(start)
		}
		if time.Now().After(deadline) {
			return n, time.Since(start)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// perfEnv 汇总一次场景需要的全部句柄。
type perfEnv struct {
	gw     *httptest.Server
	db     *store.Store
	client *http.Client
}

// ---------------------------------------------------------------------------
// A. 吞吐与延迟基线
// ---------------------------------------------------------------------------

// TestPerfBaselineNonStream 建立非流式基线。
//
// 三组对照，缺一不可：
//  1. **直连假上游**：量出「测量回路自身」的成本（loopback + 假上游 handler +
//     客户端开销）。没有它，网关的开销就无法与测量噪声分离。
//  2. **网关单并发**：串行 N 次，量稳态延迟，不受排队影响。
//  3. **网关 10 并发**：同一条路径加上轻度并发，用于观察「有哪些开销是
//     共享资源引起的」（单并发下看不到）。
func TestPerfBaselineNonStream(t *testing.T) {
	if testing.Short() {
		t.Skip("性能基线在 -short 下跳过")
	}
	good := fakeGood()
	defer good.Close()

	h, _ := buildHarness(t, []struct {
		slug, url string
		fail      bool
	}{{"good", good.URL, false}}, false)
	gw := startGateway(t, h)
	client := perfClient()
	defer client.CloseIdleConnections()

	const n = 300

	// 预热：把连接池、SQLite 预编译、SDK 客户端、快照读全部走一遍。
	for i := 0; i < 50; i++ {
		if code, err := doNonStream(client, gw.URL, nonStreamBody); err != nil || code != 200 {
			t.Fatalf("预热失败: code=%d err=%v", code, err)
		}
	}

	// 组 1：直连假上游（对照）。
	var directLat []time.Duration
	for i := 0; i < n; i++ {
		t0 := time.Now()
		if code, err := doNonStream(client, good.URL, nonStreamBody); err != nil || code != 200 {
			t.Fatalf("直连假上游失败: code=%d err=%v", code, err)
		}
		directLat = append(directLat, time.Since(t0))
	}
	direct := summarize(1, n, directLat, n, 0, 0, map[int]int{200: n})
	direct.logf(t, "A1 直连假上游(对照)")

	// 组 2：网关单并发。
	single := runNonStreamWorkload(client, gw.URL, nonStreamBody, 1, n)
	single.logf(t, "A2 网关单并发")

	// 组 3：网关 10 并发。
	c10 := runNonStreamWorkload(client, gw.URL, nonStreamBody, 10, n)
	c10.logf(t, "A3 网关10并发")

	// 纯网关开销 ≈ 网关 P50 − 直连 P50。两者都是本机 loopback，
	// 差值里剩下的就是网关自己做的事情（鉴权、限流、快照解析、配额查库、
	// 协议转换、用量入队）。
	overhead := single.p50 - direct.p50
	t.Logf("A4 纯网关开销(单并发) ≈ 网关P50 − 直连P50 = %v − %v = %v",
		single.p50.Round(time.Microsecond), direct.p50.Round(time.Microsecond), overhead.Round(time.Microsecond))
	t.Logf("环境: GOMAXPROCS=%d NumCPU=%d", runtime.GOMAXPROCS(0), runtime.NumCPU())

	if single.ok != n {
		t.Fatalf("单并发基线出现失败: ok=%d/%d codes=%v", single.ok, n, single.codes)
	}
	if c10.ok != n {
		t.Fatalf("10 并发基线出现失败: ok=%d/%d codes=%v", c10.ok, n, c10.codes)
	}
	// 基线健康性：单并发 P99 不应超过 200ms。超过说明本机环境有严重干扰，
	// 后续所有数字都不可信 —— 这时应该报错而不是静默给出可疑数据。
	if single.p99 > 200*time.Millisecond {
		t.Fatalf("单并发 P99=%v 过高，测量环境不可信（后台有重负载？）", single.p99)
	}
}

// TestPerfBaselineStream 建立流式基线：TTFT 与总耗时分开统计。
//
// # 关于断言（第一版写错了，记在这里）
//
// 第一版用**零延迟**的 fakeStreamGood，然后断言「TTFT < 总耗时」。
// 全量跑时它偶发失败：TTFT(P50)=0s 不小于总耗时(P50)=0s。
//
// 那不是缺陷，是**断言与场景不匹配**：上游零延迟时 4 个事件在几微秒内
// 全部写进同一个 TCP 段，客户端第一次 Read 就把整条流拿回来了 ——
// TTFT 与总耗时本来就必然相等。「TTFT 明显小于总耗时」只有在
// **上游在事件之间有停顿**时才是可观测的。
//
// 所以这条测试分两段：
//
//	A. 零延迟上游：量**网关自身**把首字节送出的成本（TTFT 基线）。
//	   这里不对 TTFT 与总耗时的差做任何断言。
//	B. 事件间有停顿的上游（每事件 10ms × 20 个）：量「分帧是否真的在流」——
//	   TTFT 必须 ≈ 首个事件的延迟，而总耗时 ≈ 20×10ms。
//	   这里断言 TTFT 远小于总耗时，它才是那个有意义的判据：
//	   若 sink 忘了 Flush，全部数据会压到流结束才送出，TTFT 会≈总耗时。
func TestPerfBaselineStream(t *testing.T) {
	if testing.Short() {
		t.Skip("流式基线在 -short 下跳过")
	}

	// ---- A. 零延迟上游：网关自身开销 ----
	good := fakeStreamGood()
	defer good.Close()

	h, _ := buildHarness(t, []struct {
		slug, url string
		fail      bool
	}{{"good", good.URL, false}}, false)
	gw := startGateway(t, h)
	client := perfClient()
	defer client.CloseIdleConnections()

	const n = 200
	for i := 0; i < 30; i++ {
		if _, _, code, err := doStream(client, gw.URL, streamBody); err != nil || code != 200 {
			t.Fatalf("预热失败: code=%d err=%v", code, err)
		}
	}

	var ttfts, totals []time.Duration
	codes := map[int]int{}
	ok := 0
	wallStart := time.Now()
	for i := 0; i < n; i++ {
		ttft, total, code, err := doStream(client, gw.URL, streamBody)
		if err != nil {
			t.Fatalf("流式请求失败: %v", err)
		}
		codes[code]++
		if code == http.StatusOK {
			ok++
		}
		ttfts = append(ttfts, ttft)
		totals = append(totals, total)
	}
	wall := time.Since(wallStart)

	sort.Slice(ttfts, func(i, j int) bool { return ttfts[i] < ttfts[j] })
	sort.Slice(totals, func(i, j int) bool { return totals[i] < totals[j] })
	t.Logf("A5 流式 TTFT   n=%d P50=%-9v P95=%-9v P99=%-9v min=%-9v max=%-9v",
		n, percentile(ttfts, 50).Round(time.Microsecond), percentile(ttfts, 95).Round(time.Microsecond),
		percentile(ttfts, 99).Round(time.Microsecond), ttfts[0].Round(time.Microsecond), ttfts[len(ttfts)-1].Round(time.Microsecond))
	t.Logf("A6 流式 总耗时 n=%d P50=%-9v P95=%-9v P99=%-9v min=%-9v max=%-9v",
		n, percentile(totals, 50).Round(time.Microsecond), percentile(totals, 95).Round(time.Microsecond),
		percentile(totals, 99).Round(time.Microsecond), totals[0].Round(time.Microsecond), totals[len(totals)-1].Round(time.Microsecond))
	t.Logf("A7 流式 串行 QPS=%.1f codes=%v（零延迟上游：TTFT≈总耗时是正常的，"+
		"整条流落在一个 TCP 段里）", float64(n)/wall.Seconds(), codes)

	if ok != n {
		t.Fatalf("流式基线出现非 200: ok=%d/%d codes=%v", ok, n, codes)
	}

	// ---- B. 事件间有停顿的上游：分帧是否真的在流 ----
	const gap = 10 * time.Millisecond
	const events = 20
	slow := fakeStreamMany(events, gap)
	defer slow.Close()

	h2, _ := buildHarness(t, []struct {
		slug, url string
		fail      bool
	}{{"good", slow.URL, false}}, false)
	gw2 := startGateway(t, h2)
	client2 := perfClient()
	defer client2.CloseIdleConnections()

	// 预热
	if _, _, code, err := doStream(client2, gw2.URL, streamBody); err != nil || code != 200 {
		t.Fatalf("预热(停顿流)失败: code=%d err=%v", code, err)
	}

	var ttfts2, totals2 []time.Duration
	for i := 0; i < 20; i++ {
		ttft, total, code, err := doStream(client2, gw2.URL, streamBody)
		if err != nil || code != 200 {
			t.Fatalf("停顿流请求失败: code=%d err=%v", code, err)
		}
		ttfts2 = append(ttfts2, ttft)
		totals2 = append(totals2, total)
	}
	sort.Slice(ttfts2, func(i, j int) bool { return ttfts2[i] < ttfts2[j] })
	sort.Slice(totals2, func(i, j int) bool { return totals2[i] < totals2[j] })
	p50ttft := percentile(ttfts2, 50)
	p50total := percentile(totals2, 50)
	t.Logf("A8 分帧验证(每事件 %v × %d): TTFT P50=%v 总耗时 P50=%v 比值=%.2f",
		gap, events, p50ttft.Round(time.Millisecond), p50total.Round(time.Millisecond),
		float64(p50total)/float64(p50ttft))

	// 判据：上游有 20×10ms=200ms 的停顿，若分帧正常，TTFT 应远小于总耗时。
	// 若 sink 不 Flush（全部压到结束），TTFT 会≈总耗时（比值≈1）。
	if p50total < 5*p50ttft {
		t.Fatalf("TTFT=%v 与总耗时=%v 同量级 —— 分帧未生效（sink 没有逐事件 Flush？）",
			p50ttft, p50total)
	}
	// 反向：TTFT 不该被上游总时长拖住（那说明在等整条流）。
	if p50ttft > 100*time.Millisecond {
		t.Fatalf("TTFT=%v 远超首个事件延迟 %v —— 首字节被后续事件阻塞", p50ttft, gap)
	}
}

// ---------------------------------------------------------------------------
// B. 并发扩展性
// ---------------------------------------------------------------------------

// TestPerfConcurrencyScaling 逐级加压（10/50/100/200），观察 QPS 与延迟。
//
// # 这条测试要回答的问题
//
// 「QPS 是否随并发线性增长」。若某个并发级别开始 QPS 停滞而延迟陡增，
// 那就是拐点；拐点处必然存在一个**串行化的共享资源**。
//
// # 关键：样本量必须固定且足够大
//
// 第一版用 n = 并发×8（10 并发只有 80 个样本、25ms 就跑完），结果为
// conc=10 → 3243 QPS、conc=50 → 7913 QPS 的「超线性」怪象 —— 小样本下
// 单次调度抖动就能把 QPS 拉高或压低一倍。固定 n=1500 后曲线才可解读
// （见 perf_leak_test.go 的 TestDiagBottleneck 对照测量）。
//
// # 断言只做「没有崩」
//
// 性能数字随机器负载波动（实测同场景重复 5 次，极差/中位 = 32.5%，
// 见 TestDiagVariance），把 QPS 阈值写进断言会让测试在负载不同的 CI 上
// 随机红。拐点位置是**报告结论**，不是测试断言。
func TestPerfConcurrencyScaling(t *testing.T) {
	if testing.Short() {
		t.Skip("并发扩展性在 -short 下跳过")
	}
	good := fakeGood()
	defer good.Close()

	h, _ := buildHarness(t, []struct {
		slug, url string
		fail      bool
	}{{"good", good.URL, false}}, false)
	gw := startGateway(t, h)
	client := perfClient()
	defer client.CloseIdleConnections()

	for i := 0; i < 100; i++ {
		if code, err := doNonStream(client, gw.URL, nonStreamBody); err != nil || code != 200 {
			t.Fatalf("预热失败: code=%d err=%v", code, err)
		}
	}

	levels := []int{10, 50, 100, 200}
	const n = 1500 // 固定样本量：见本函数开头「样本量必须固定且足够大」
	results := make([]perfResult, 0, len(levels))
	for _, c := range levels {
		r := runNonStreamWorkload(client, gw.URL, nonStreamBody, c, n)
		r.logf(t, fmt.Sprintf("B 并发扩展 conc=%d", c))
		results = append(results, r)
		if r.failed != 0 {
			t.Errorf("并发 %d 出现失败 %d 次 codes=%v", c, r.failed, r.codes)
		}
	}

	first, last := results[0], results[len(results)-1]
	concRatio := float64(last.concurrency) / float64(first.concurrency)
	qpsRatio := last.qps / first.qps
	t.Logf("B-summary 并发 ×%.1f → QPS ×%.2f（线性则为 ×%.1f）；P99 %v → %v",
		concRatio, qpsRatio, concRatio, first.p99.Round(time.Microsecond), last.p99.Round(time.Microsecond))
	t.Logf("B-summary 扩展效率 = %.2f（1.00 = 完美线性；<0.5 说明已明显饱和）", qpsRatio/concRatio)
	t.Logf("B-summary 拐点与瓶颈归因见 AUDIT/test-perf.md；" +
		"perf_leak_test.go 的 TestDiagBottleneck 给出了直连对照与 DB 微基准")

	if last.ok == 0 {
		t.Fatalf("最高并发下全部失败")
	}
}

// TestPerfDataPlaneNotLimitedByAuthConcurrencyCap 确认数据面不受
// maxConcurrentAttempts=4 影响。
//
// # 背景
//
// internal/server.FailureThrottle 里有 maxConcurrentAttempts=4
// （单 IP 同时进行的**鉴权尝试**上限）。它只被会话鉴权
// （UserAuthMiddleware，即 /admin 路径）使用；数据面 auth.Authenticate
// 是纯快照查表，不经过它。这条测试就是把这个「应该不受影响」变成实测：
// 用远超 4 的并发打数据面，若不出现任何 429，则确认无牵连。
//
// 为什么值得单独立一条：这两个限速器的命名相似（都叫 throttle/limit），
// 且都在鉴权附近，未来把数据面也接上它是个很自然的"加固"动作 ——
// 而那会让 200 并发直接退化成 4 并发。这条断言就是那种改动的红灯。
func TestPerfDataPlaneNotLimitedByAuthConcurrencyCap(t *testing.T) {
	good := fakeGood()
	defer good.Close()

	h, _ := buildHarness(t, []struct {
		slug, url string
		fail      bool
	}{{"good", good.URL, false}}, false)
	gw := startGateway(t, h)
	client := perfClient()
	defer client.CloseIdleConnections()

	// 并发 64（= 上限 4 的 16 倍），全部必须成功。
	r := runNonStreamWorkload(client, gw.URL, nonStreamBody, 64, 64)
	r.logf(t, "B2 数据面 64 并发(校验无 429)")

	if _, bad := r.codes[http.StatusTooManyRequests]; bad {
		t.Fatalf("数据面出现 429 —— 被 maxConcurrentAttempts/限速器误伤: codes=%v", r.codes)
	}
	if r.ok != 64 {
		t.Fatalf("64 并发应全部成功: ok=%d codes=%v", r.ok, r.codes)
	}
}

// ---------------------------------------------------------------------------
// C. 资源泄漏
// ---------------------------------------------------------------------------

// countGoroutines 在清理窗口后读 goroutine 数。
func countGoroutines() int {
	runtime.GC()
	time.Sleep(150 * time.Millisecond)
	return runtime.NumGoroutine()
}

// TestPerfGoroutineLeak 跑 1000 次请求，前后对比 goroutine 数。
//
// # 基线怎么取才公平
//
// 基线必须在**组好 harness、并预热之后**取。原因：usageRecorder 的
// worker goroutine（`for rec := range u.queue`）是长活的，每建一套 harness
// 就多一条；httptest 服务器本身也有 accept goroutine。这些是**常量开销**，
// 把它们放进基线里，剩下的增量才指向「每次请求有没有多留一条」。
//
// # 为什么用 1000
//
// 单条泄漏 × 1000 是 +1000 条 goroutine，任何量的泄漏都藏不住；
// 而少于几百次时，「泄漏 1 条」与「运行时懒创建的 1 条」无法区分。
func TestPerfGoroutineLeak(t *testing.T) {
	good := fakeGood()
	defer good.Close()

	h, _ := buildHarness(t, []struct {
		slug, url string
		fail      bool
	}{{"good", good.URL, false}}, false)
	env := newPerfEnv(t, h)
	client := env.client

	for i := 0; i < 50; i++ {
		if code, err := doNonStream(client, env.gw.URL, nonStreamBody); err != nil || code != 200 {
			t.Fatalf("预热失败: code=%d err=%v", code, err)
		}
	}

	before := countGoroutines()
	const rounds = 1000
	for i := 0; i < rounds; i++ {
		if code, err := doNonStream(client, env.gw.URL, nonStreamBody); err != nil || code != 200 {
			t.Fatalf("第 %d 次请求失败: code=%d err=%v", i, code, err)
		}
	}
	after := countGoroutines()

	t.Logf("C1 goroutine 泄漏: before=%d after=%d delta=%+d（%d 次非流式请求）",
		before, after, after-before, rounds)

	// 容差 5：本机运行时偶有短暂的 netpoll/GC 辅助 goroutine。
	// 1000 次请求若每次泄漏 1 条，delta 会是 +1000，绝无可能落在容差内。
	if delta := after - before; delta > 5 {
		t.Fatalf("疑似 goroutine 泄漏: %d 次请求后多出 %d 条（before=%d after=%d）", rounds, delta, before, after)
	}
}

// TestPerfGoroutineLeakStream 同 C1，但走流式路径。
//
// 流式路径的 goroutine 更多（每请求一个心跳 goroutine + SDK 的流读取
// goroutine），且心跳 goroutine 的退出靠 quit channel + WaitGroup ——
// 这条测试就是那个收尾逻辑的泄漏探针。非流式测试覆盖不到它。
func TestPerfGoroutineLeakStream(t *testing.T) {
	good := fakeStreamGood()
	defer good.Close()

	h, _ := buildHarness(t, []struct {
		slug, url string
		fail      bool
	}{{"good", good.URL, false}}, false)
	env := newPerfEnv(t, h)
	client := env.client

	for i := 0; i < 30; i++ {
		if _, _, code, err := doStream(client, env.gw.URL, streamBody); err != nil || code != 200 {
			t.Fatalf("预热失败: code=%d err=%v", code, err)
		}
	}

	before := countGoroutines()
	const rounds = 500
	for i := 0; i < rounds; i++ {
		_, _, code, err := doStream(client, env.gw.URL, streamBody)
		if err != nil || code != 200 {
			t.Fatalf("第 %d 次流式请求失败: code=%d err=%v", i, code, err)
		}
	}
	after := countGoroutines()

	t.Logf("C2 goroutine 泄漏(流式): before=%d after=%d delta=%+d（%d 次流式请求）",
		before, after, after-before, rounds)
	if delta := after - before; delta > 5 {
		t.Fatalf("流式路径疑似 goroutine 泄漏: %d 次后多出 %d 条", rounds, delta)
	}
}

// TestPerfUpstreamConnectionRelease 验证上游连接被**复用**且不随请求数增长。
//
// # 断言的是「有界 + 复用」，不是「归零」（这一点我一开始写错了）
//
// 第一版断言「请求结束后 open ≈ 0」，实测 open=18 就红了 —— 但那是**误报**：
// 18 条正是 http.Transport 保活的**空闲连接池**，它按设计常驻，用于下一个
// 请求复用。把「空闲池」当泄漏判据，会把一个健康的实现判成有缺陷。
//
// 区分「空闲池」与「连接泄漏」的可靠判据是**增长方式**：
//   - 空闲池：累计建立连接数 ≈ 并发数，与**请求总数无关**（复用率高）；
//   - 泄漏：累计建立连接数 ≈ 请求总数（每个请求一条，从不复用），
//     且 open 随请求数单调上升。
//
// 所以这里断言两件事，都不涉及「是否归零」：
//  1. **复用生效**：累计建立 total 远小于请求数;
//  2. **峰值有界**：peak 不超过并发数的一个小倍数（连接在并发内铺开）。
func TestPerfUpstreamConnectionRelease(t *testing.T) {
	up, cs := newFakeUpstreamWithConnStats()
	defer up.Close()

	h, _ := buildHarness(t, []struct {
		slug, url string
		fail      bool
	}{{"good", up.URL, false}}, false)
	env := newPerfEnv(t, h)
	client := env.client

	for i := 0; i < 20; i++ {
		if code, err := doNonStream(client, env.gw.URL, nonStreamBody); err != nil || code != 200 {
			t.Fatalf("预热失败: code=%d err=%v", code, err)
		}
	}

	const conc = 20
	const total = 300
	r := runNonStreamWorkload(client, env.gw.URL, nonStreamBody, conc, total)
	r.logf(t, "C3 连接复用/释放(20并发×300)")

	settle()
	established := cs.total.Load()
	openAfter := cs.open.Load()
	peak := cs.peak.Load()
	reuse := float64(total) / float64(max64(established, 1))
	t.Logf("C4 上游连接: 累计建立=%d 峰值=%d 结束后 open=%d；请求 %d 次 → 复用倍数 ≈ %.1f×",
		established, peak, openAfter, total, reuse)

	if r.ok != total {
		t.Fatalf("连接测量期间出现失败: ok=%d/%d codes=%v", r.ok, total, r.codes)
	}
	// 判据 1：复用必须生效。若每个请求都新建连接，established 会接近 total。
	if established > int64(conc)*4 {
		t.Fatalf("累计建立 %d 条连接服务 %d 次请求（并发 %d）—— 复用失效，疑似每请求新建/泄漏",
			established, total, conc)
	}
	// 判据 2：峰值不得超出并发数太多（超出说明连接没被回收再利用）。
	if peak > int64(conc)*3 {
		t.Fatalf("上游连接峰值 %d 远超并发数 %d", peak, conc)
	}
	// 判据 3：结束后持有的连接数不得超过「曾经建立过的」——
	// 这只是个自洽性检查（open 不可能大于 total），真正的泄漏判据是上面两条。
	if openAfter > established {
		t.Fatalf("open=%d > 累计建立=%d，计数不自洽", openAfter, established)
	}
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

// TestPerfClientDisconnectReleasesResources 验证客户端中途断开后资源被释放。
//
// # 为什么这条最容易漏
//
// 客户端断开在网关侧表现为 **context 取消**。取消不会让任何一行代码
// 「顺理成章地」返回：上游读取要等 ctx 传播、心跳 goroutine 要等 quit、
// defer 里的 stream.Close() 才会触发。任何一处漏了「听 ctx」，
// 这条路径就留下一条常驻 goroutine —— 而它**只在用户主动关页面时**
// 出现，正常压测（全部读完）永远测不到。
//
// 手法：上游发慢流（每事件 5ms），客户端在读到第一个事件后立刻
// cancel/关闭，然后看 goroutine 是否回落。
func TestPerfClientDisconnectReleasesResources(t *testing.T) {
	slow := fakeStreamMany(200, 5*time.Millisecond)
	defer slow.Close()

	h, _ := buildHarness(t, []struct {
		slug, url string
		fail      bool
	}{{"good", slow.URL, false}}, false)
	env := newPerfEnv(t, h)

	// 预热一条完整流（走正常结束路径，把懒初始化做完）。
	warmClient := perfClient()
	if _, _, code, err := doStream(warmClient, env.gw.URL, streamBody); err != nil || code != 200 {
		t.Fatalf("预热失败: code=%d err=%v", code, err)
	}
	warmClient.CloseIdleConnections()

	before := countGoroutines()

	const rounds = 100
	disconnected := 0
	for i := 0; i < rounds; i++ {
		client := perfClient()
		req, err := http.NewRequest(http.MethodPost, env.gw.URL+"/v1/chat/completions", strings.NewReader(streamBody))
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		req.Header.Set("Authorization", "Bearer "+testAccessKey)
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("第 %d 次请求: %v", i, err)
		}
		// 读到首个字节就断开 —— 模拟用户看到第一个字后关掉页面。
		br := bufio.NewReaderSize(resp.Body, 4096)
		if _, rerr := br.ReadByte(); rerr == nil {
			disconnected++
		}
		// 直接关 body（不读完）：这会关闭底层连接，网关侧看到 ctx 取消。
		_ = resp.Body.Close()
		client.CloseIdleConnections()
	}
	after := countGoroutines()

	t.Logf("C5 客户端中途断开: rounds=%d 成功读到首字节=%d before=%d after=%d delta=%+d",
		rounds, disconnected, before, after, after-before)

	if disconnected == 0 {
		t.Fatal("没有一次成功读到首字节，断开场景未成立（测试本身无效）")
	}
	// 每次断开泄漏 1 条 + 心跳 goroutine 的话，delta 会是 +100 以上。
	// 容差 10：被取消的请求 goroutine 退出是异步的，settle 后可能有少量残留。
	if delta := after - before; delta > 10 {
		t.Fatalf("客户端断开后疑似泄漏: %d 次断开后多出 %d 条 goroutine", rounds, delta)
	}
}

// TestPerfMemoryGrowth 跑大量请求，对比 GC 后的 HeapInuse / HeapObjects。
//
// 判据是 **GC 后仍驻留的部分**，不是 TotalAlloc：后者是累计分配量，
// 单调递增只说明「分配过」（正常，GC 会回收）。
//
// # 为什么用「两段对比」而不是「一个绝对值」
//
// 全量套件里跑时，同进程的其它测试（尤其是 TestDiagUsageWriteCeiling
// 那种灌 2 万行的）会把堆抬到一个与本测试无关的水平。所以这里量的是
// **本测试自己两段之间的增量**：先跑 1000 次取一个点，再跑 3000 次取第二个点，
// 比较第二段的增量。
//
//   - 无泄漏：第二段的驻留增量与第一段同量级（甚至更小，因为堆已预热）；
//   - 有泄漏：第二段请求数是第一段的 3 倍，增量也约 3 倍，且单调。
//
// 段间用同一个已预热的进程，排除了「套件里别的测试抬高了堆」这个干扰。
func TestPerfMemoryGrowth(t *testing.T) {
	good := fakeGood()
	defer good.Close()

	h, _ := buildHarness(t, []struct {
		slug, url string
		fail      bool
	}{{"good", good.URL, false}}, false)
	env := newPerfEnv(t, h)
	client := env.client

	for i := 0; i < 100; i++ {
		if code, err := doNonStream(client, env.gw.URL, nonStreamBody); err != nil || code != 200 {
			t.Fatalf("预热失败: code=%d err=%v", code, err)
		}
	}
	settle()

	// measure 跑 n 次请求，返回该段结束时的 GC 后堆统计。
	measure := func(n int) runtime.MemStats {
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		r := runNonStreamWorkload(client, env.gw.URL, nonStreamBody, 20, n)
		if r.ok != n {
			t.Fatalf("内存测量期间出现失败: ok=%d/%d codes=%v", r.ok, n, r.codes)
		}
		settle()
		runtime.ReadMemStats(&after)
		return after
	}

	// 全量套件里其它测试可能已经灌了大量数据，先取一个「当前水位」。
	var baseline runtime.MemStats
	settle()
	runtime.ReadMemStats(&baseline)
	t.Logf("C6 起始水位: HeapInuse=%.2f MB HeapObjects=%d",
		float64(baseline.HeapInuse)/(1<<20), baseline.HeapObjects)

	const seg1 = 1000
	const seg2 = 3000
	s1 := measure(seg1)
	s2 := measure(seg2)

	heap1 := int64(s1.HeapInuse) - int64(baseline.HeapInuse)
	heap2 := int64(s2.HeapInuse) - int64(baseline.HeapInuse)
	obj1 := int64(s1.HeapObjects) - int64(baseline.HeapObjects)
	obj2 := int64(s2.HeapObjects) - int64(baseline.HeapObjects)

	t.Logf("C7 第一段 %d 次后: HeapInuse %+.2f MB, HeapObjects %+d", seg1,
		float64(heap1)/(1<<20), obj1)
	t.Logf("C8 第二段累计 %d 次后: HeapInuse %+.2f MB, HeapObjects %+d", seg1+seg2,
		float64(heap2)/(1<<20), obj2)
	t.Logf("C9 增量对比: 请求数 ×%.1f（%d→%d）而 HeapInuse 增量 ×%.2f、HeapObjects 增量 ×%.2f —— "+
		"若接近 1 说明与请求数无关（非泄漏）；若接近 3 则线性泄漏",
		float64(seg1+seg2)/float64(seg1), seg1, seg1+seg2,
		safeRatio(heap2, heap1), safeRatio(obj2, obj1))

	// 绝对值兜底（宽松）：4 万次请求后 GC 残留不应超过 64MB。
	// 实测（含套件干扰）在 3MB 以内，64MB 只用来拦住量级失控。
	if heap2 > 64<<20 {
		t.Fatalf("4000 次请求后 GC 残留堆 %+.2f MB，量级异常", float64(heap2)/(1<<20))
	}
	// 增长关系判据：请求数涨 3 倍，驻留增量不应涨到接近 3 倍。
	// 2.5 是宽容的界：真泄漏（每请求一份常驻对象）必然 ≥3。
	if heap1 > 0 && float64(heap2)/float64(heap1) > 2.5 {
		t.Fatalf("请求数 ×3 而驻留堆增量 ×%.2f（%+.2f MB → %+.2f MB）—— 疑似线性泄漏",
			float64(heap2)/float64(heap1), float64(heap1)/(1<<20), float64(heap2)/(1<<20))
	}
}

// safeRatio 计算 a/b，b<=0 时返回 -1（表示「基期为负/零，比值无意义」）。
//
// 需要它是因为 GC 后的堆增量**可以是负数**（起始水位高于结束水位，
// 常见于套件里前面的测试刚释放了大块内存）。直接相除会得到误导性的
// 巨大比值或 NaN。
func safeRatio(a, b int64) float64 {
	if b <= 0 {
		return -1
	}
	return float64(a) / float64(b)
}

// ---------------------------------------------------------------------------
// D. 背压与慢上游
// ---------------------------------------------------------------------------

// TestPerfTTFTWatchdogOnHangingUpstream 验证「上游挂住」时看门狗按时掐流。
//
// # 断言的是「有界」而不是「快」
//
// hang 上游一个事件都不发，网关必须靠 TTFT 看门狗在 ttftMs 后掐掉它。
// 唯一可接受的失败模式是「按时失败」；不可接受的是「一直挂着」
// （那会占住下游连接 + 上游连接 + 配额预占）。
// 所以断言 elapsed 落在 [ttft, ttft+slack] 内，且**并发下每条都如此**。
func TestPerfTTFTWatchdogOnHangingUpstream(t *testing.T) {
	hang := fakeStreamHang()
	defer hang.Close()

	const ttftMs = 150
	h, _ := buildHarness(t, []struct {
		slug, url string
		fail      bool
	}{{"hang", hang.URL, false}}, false, ttftMs)
	env := newPerfEnv(t, h)

	// 单条：确认看门狗真的在 ttftMs 附近触发。
	t0 := time.Now()
	_, _, code, err := doStream(env.client, env.gw.URL, streamBody)
	elapsed := time.Since(t0)
	t.Logf("D1 hang 上游单条: elapsed=%v code=%d err=%v（TTFT 看门狗 %dms）",
		elapsed.Round(time.Millisecond), code, err, ttftMs)

	if elapsed < 100*time.Millisecond {
		t.Fatalf("elapsed=%v 早于 TTFT 预算，说明不是看门狗返回的", elapsed)
	}
	if elapsed > 2500*time.Millisecond {
		t.Fatalf("hang 上游耗时 %v 远超 TTFT 预算 %dms —— 看门狗未按时触发", elapsed, ttftMs)
	}
	if code != http.StatusGatewayTimeout && code != http.StatusBadGateway {
		t.Fatalf("hang 上游应回 504/502，实际 %d", code)
	}

	// 并发：确认没有堆积。每条都应独立、按时地失败。
	const conc = 50
	var (
		mu       sync.Mutex
		lat      []time.Duration
		codes    = map[int]int{}
		okCnt    int
		badCount int
		wg       sync.WaitGroup
	)
	start := time.Now()
	for i := 0; i < conc; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			t0 := time.Now()
			_, _, c, _ := doStream(env.client, env.gw.URL, streamBody)
			d := time.Since(t0)
			mu.Lock()
			lat = append(lat, d)
			codes[c]++
			if c == http.StatusOK {
				okCnt++
			} else {
				badCount++
			}
			mu.Unlock()
		}()
	}
	wg.Wait()
	wall := time.Since(start)
	sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
	t.Logf("D2 hang 上游 %d 并发: 全部完成 wall=%v P50=%v P99=%v codes=%v ok=%d",
		conc, wall.Round(time.Millisecond), percentile(lat, 50).Round(time.Millisecond),
		percentile(lat, 99).Round(time.Millisecond), codes, okCnt)
	if badCount != conc {
		t.Fatalf("hang 上游下 %d 并发应全部失败，实际成功 %d 条", conc, okCnt)
	}
	// 并发下若出现堆积（比如共享一个看门狗或串行化），P99 会远超单条的耗时。
	if p99 := percentile(lat, 99); p99 > 3*time.Second {
		t.Fatalf("hang 上游 %d 并发下 P99=%v，存在明显堆积", conc, p99)
	}
}

// TestPerfSlowUpstreamHighConcurrency 慢上游 × 高并发：网关是否稳定。
//
// 这里上游是**慢但成功**（首字 80ms、10 个事件），与 hang 不同 ——
// 它验证的是「并发慢流会不会把网关自己的资源耗尽」，
// 而不是「故障能否被检测」。
//
// # 关于 goroutine 断言的口径（第一版写错了，记在这里）
//
// 第一版断言「压测后 goroutine 回落」，实测 +492 就红了。但**那不是泄漏**：
// perf_leak_test.go 的 TestPerfDiagGoroutineStacksSlowUpstream 做了判据实验 ——
//   - 关掉客户端空闲连接池：499 → 199（少了 300，那是压测客户端自己的
//     keep-alive 连接，每条连接带 2 条 persistConn goroutine）；
//   - 同并发下把请求数 100 → 1000（×10）：残留 190 → 157，**基本不变**。
//
// 「请求数 ×10 而残留不变」证明它由**并发数**（同时打开过的连接数）决定，
// 是两端 http.Transport 的空闲连接池在保活，不是每请求泄漏。
// 真泄漏必然随请求数线性增长 —— 那才是这里要断言的性质。
//
// 所以断言改成「与请求数无关」的形态：把慢上游请求数 ×5，比较残留增量。
// 空闲池不会因此增长；每次请求漏一条则增量会是 5 倍。
func TestPerfSlowUpstreamHighConcurrency(t *testing.T) {
	if testing.Short() {
		t.Skip("慢上游高并发在 -short 下跳过")
	}
	slow := fakeSlowStream(80*time.Millisecond, 10)
	defer slow.Close()

	h, _ := buildHarness(t, []struct {
		slug, url string
		fail      bool
	}{{"slow", slow.URL, false}}, false)
	env := newPerfEnv(t, h)

	// runSlow 以固定并发跑 total 次慢流，返回结果与「关池后的残留 goroutine」。
	runSlow := func(conc, total int) (perfResult, int) {
		var (
			mu    sync.Mutex
			lat   []time.Duration
			codes = map[int]int{}
			ok    int
			next  atomic.Int64
			wg    sync.WaitGroup
		)
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
					_, _, code, _ := doStream(env.client, env.gw.URL, streamBody)
					d := time.Since(t0)
					mu.Lock()
					lat = append(lat, d)
					codes[code]++
					if code == http.StatusOK {
						ok++
					}
					mu.Unlock()
				}
			}()
		}
		wg.Wait()
		wall := time.Since(start)

		// 关掉两端的空闲连接再读：这样读到的是「非空闲连接」——
		// 也就是真正在跑/被漏掉的 goroutine。不关池的话读到的主要是
		// keep-alive 池，与泄漏无关（见上面的判据实验）。
		env.client.CloseIdleConnections()
		settle()
		return summarize(conc, total, lat, ok, total-ok, wall, codes), runtime.NumGoroutine()
	}

	// 预热
	for i := 0; i < 20; i++ {
		if _, _, code, err := doStream(env.client, env.gw.URL, streamBody); err != nil || code != 200 {
			t.Fatalf("预热失败: code=%d err=%v", code, err)
		}
	}
	env.client.CloseIdleConnections()
	settle()
	baseline := runtime.NumGoroutine()

	r1, g1 := runSlow(100, 300)
	r1.logf(t, "D3 慢上游(首字80ms) 100并发×300")
	t.Logf("D4a 请求=300 后（关空闲池）goroutine=%d（基线 %d，增 %+d）", g1, baseline, g1-baseline)

	r2, g2 := runSlow(100, 1500)
	r2.logf(t, "D3b 慢上游(首字80ms) 100并发×1500")
	t.Logf("D4b 请求=1500 后（关空闲池）goroutine=%d（基线 %d，增 %+d）", g2, baseline, g2-baseline)
	t.Logf("D4c 请求数 ×5 → 残留增量 %+d → %+d。空闲池不随请求数增长；"+
		"每次请求漏一条则增量应为 5 倍（+%d → +%d）",
		g1-baseline, g2-baseline, 5*(g1-baseline), 5*(g2-baseline))

	if r1.ok != 300 || r2.ok != 1500 {
		t.Fatalf("慢上游高并发出现失败: ok=%d/300, %d/1500 codes=%v %v", r1.ok, r2.ok, r1.codes, r2.codes)
	}
	// 泄漏判据：残留**不得随请求数放大**。允许一个宽松的绝对值上界
	// （100 并发下两个连接池的 goroutine 加起来的量级），
	// 但 5 倍请求量不得带来接近 5 倍的残留。
	growth := (g2 - baseline) - (g1 - baseline)
	t.Logf("D4d 增量之差 = %+d（若为 0 量级 → 与请求数无关 → 非泄漏）", growth)
	if g2-baseline > 400 {
		t.Fatalf("1500 次慢流后残留 %d 条 goroutine，远超并发数所能解释的量级", g2-baseline)
	}
}

// TestPerfUsageQueueSaturation 检验 usage 队列（1024）打满时的行为。
//
// # 要验证的三件事
//
//  1. **记录不丢**：请求全部成功 → usage_records 行数必须等于成功请求数。
//     队列满时的设计是「退化为同步写」，那条路径若写失败会记 ERROR 并
//     丢弃记录 —— 所以必须数行数，不能只看「请求都 200」。
//  2. **不崩**：队列满不得导致 panic 或 5xx。
//  3. **量化背压**：请求完成耗时 vs 记录落库追平的耗时。若后者显著更长，
//     说明队列在积压，而积压意味着请求侧承受了同步写的惩罚。
//
// 规模取 1024（队列容量）+ 500 溢出量，确保一定能触发队列满。
func TestPerfUsageQueueSaturation(t *testing.T) {
	good := fakeGood()
	defer good.Close()

	h, db := buildHarness(t, []struct {
		slug, url string
		fail      bool
	}{{"good", good.URL, false}}, false)
	env := newPerfEnv(t, h)
	env.db = db
	client := env.client

	for i := 0; i < 50; i++ {
		if code, err := doNonStream(client, env.gw.URL, nonStreamBody); err != nil || code != 200 {
			t.Fatalf("预热失败: code=%d err=%v", code, err)
		}
	}
	// 预热阶段的记录也会落库，先记下基数。
	settle()
	baseCount := usageCount(t, env)

	const total = usageQueueSize + 500 // 1524，必然溢出队列
	r := runNonStreamWorkload(client, env.gw.URL, nonStreamBody, 200, total)
	r.logf(t, fmt.Sprintf("D5 usage 队列压力(容量 %d, 请求 %d)", usageQueueSize, total))

	// 等异步落库追平，并测量追平耗时。
	landed, drainTime := waitUsageCount(t, env, baseCount+total, 30*time.Second)
	got := landed - baseCount
	t.Logf("D6 usage 落库: 请求成功=%d 落库=%d（基数 %d → %d）追平耗时=%v（请求墙钟 %v）",
		r.ok, got, baseCount, landed, drainTime.Round(time.Millisecond), r.wall.Round(time.Millisecond))

	if r.failed != 0 {
		t.Fatalf("队列满时出现请求失败 %d 次 codes=%v", r.failed, r.codes)
	}
	// 这是本测试的**核心断言**：一条都不能丢。
	if got != total {
		t.Fatalf("用量记录丢失: 成功请求 %d 条，落库仅 %d 条（差 %d）", total, got, total-got)
	}
	// 落库追平不应比请求墙钟慢一个数量级 —— 那说明单 worker 严重跟不上。
	if drainTime > r.wall*10+5*time.Second {
		t.Fatalf("落库追平 %v 远慢于请求墙钟 %v，单 worker 吞吐不足", drainTime, r.wall)
	}
}

// ---------------------------------------------------------------------------
// E. 长流资源占用
// ---------------------------------------------------------------------------

// TestPerfLongStreamMemoryBounded 验证内存不随流长度线性增长。
//
// # 手法：两条长度相差 10 倍的流，比较峰值堆增量
//
// 若流式路径是 O(1)/事件（边收边写、不累积），两条流的峰值堆增量应当
// 在同一量级；若是 O(n)（把所有事件攒在内存里再写），10 倍长度会带来
// 约 10 倍的峰值增量。这条对比比「跑一条长流看绝对值」稳健得多，
// 因为绝对值受机器与 GC 时机影响。
//
// 采样在**流进行中**做：只看结束后（那时内存已释放）什么也发现不了。
func TestPerfLongStreamMemoryBounded(t *testing.T) {
	if testing.Short() {
		t.Skip("长流内存在 -short 下跳过")
	}

	measure := func(events int) (peakDelta int64, total time.Duration, bytes int64, code int) {
		up := fakeStreamMany(events, 0)
		defer up.Close()

		h, _ := buildHarness(t, []struct {
			slug, url string
			fail      bool
		}{{"good", up.URL, false}}, false)
		env := newPerfEnv(t, h)

		// 预热（短流），把懒初始化做完再取基线。
		if _, _, c, err := doStream(env.client, env.gw.URL, streamBody); err != nil || c != 200 {
			t.Fatalf("预热失败: code=%d err=%v", c, err)
		}
		settle()
		var base runtime.MemStats
		runtime.ReadMemStats(&base)

		// 发起长流，在读取过程中采样堆。
		req, err := http.NewRequest(http.MethodPost, env.gw.URL+"/v1/chat/completions", strings.NewReader(streamBody))
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		req.Header.Set("Authorization", "Bearer "+testAccessKey)
		req.Header.Set("Content-Type", "application/json")

		t0 := time.Now()
		resp, err := env.client.Do(req)
		if err != nil {
			t.Fatalf("长流请求失败: %v", err)
		}
		defer resp.Body.Close()
		code = resp.StatusCode

		var peak int64
		var n int64
		buf := make([]byte, 8192)
		for {
			read, rerr := resp.Body.Read(buf)
			n += int64(read)
			if n%(1<<20) < int64(len(buf)) { // 大约每 1MB 采样一次，避免采样本身成为热点
				var ms runtime.MemStats
				runtime.ReadMemStats(&ms)
				if d := int64(ms.HeapInuse) - int64(base.HeapInuse); d > peak {
					peak = d
				}
			}
			if rerr != nil {
				break
			}
		}
		total = time.Since(t0)
		return peak, total, n, code
	}

	shortPeak, shortDur, shortBytes, shortCode := measure(200)
	longPeak, longDur, longBytes, longCode := measure(2000)

	t.Logf("E1 短流(200 事件):  峰值堆增量=%+.2f MB 耗时=%v 响应字节=%d code=%d",
		float64(shortPeak)/(1<<20), shortDur.Round(time.Millisecond), shortBytes, shortCode)
	t.Logf("E2 长流(2000 事件): 峰值堆增量=%+.2f MB 耗时=%v 响应字节=%d code=%d",
		float64(longPeak)/(1<<20), longDur.Round(time.Millisecond), longBytes, longCode)

	if shortCode != 200 || longCode != 200 {
		t.Fatalf("长流测量中出现非 200: short=%d long=%d", shortCode, longCode)
	}
	// 10 倍事件量必须带来约 10 倍的响应字节（否则测的不是同一件事）。
	if longBytes < shortBytes*5 {
		t.Fatalf("长流字节数没有随事件数增长（short=%d long=%d），测量无效", shortBytes, longBytes)
	}
	// 绝对上界：一条流无论多长，驻留堆都不该超过 24MB。
	// 2000 个事件 × 每个约 200 字节的 SSE 帧 ≈ 400KB 的响应体 ——
	// 若实现是「全部攒在内存」，这里会长到与响应体同量级甚至更大；
	// 24MB 的上界对「边收边写」的实现有巨大余量，对「累积」的实现则必然踩爆。
	if longPeak > 24<<20 {
		t.Fatalf("长流峰值堆增量 %.2f MB 过高，疑似随流长度累积", float64(longPeak)/(1<<20))
	}
	t.Logf("E3 长/短峰值比=%.2f（事件数比=10.0；接近 1 说明内存与流长度无关，接近 10 说明线性累积）",
		float64(longPeak)/math.Max(float64(shortPeak), 1))
}

// ---------------------------------------------------------------------------
// 辅助
// ---------------------------------------------------------------------------

// newPerfEnv 组一套「网关 HTTP 服务器 + 客户端」。
//
// 与直接调 handler 的区别见 startGateway 的注释（Flusher / 真实连接）。
// 需要查库断言的用例（用量是否真的落库）自行把 buildHarness 交出的 db
// 填进 env.db —— 大多数性能用例不需要它，所以不强制传。
func newPerfEnv(t *testing.T, h http.HandlerFunc) *perfEnv {
	t.Helper()
	client := perfClient()
	t.Cleanup(client.CloseIdleConnections)
	return &perfEnv{
		gw:     startGateway(t, h),
		client: client,
	}
}
