// Package probe 实现上游模型的定时探测与健康判定。
//
// 探测请求复用 upstream.TestUpstreamModel（max_tokens=1、Thinking=nil、
// 非流式单次、不经 usage_recorder —— 不扣任何网关用户的钱，见
// upstream.go 的注释）。本包只负责「调度 + 落库 + 健康判定」，
// 不碰 pool 的熔断状态机（探测失败绝不写 cooldown，见 store.go
// model_probes 建表注释的隔离红线）。
package probe

import (
	"context"
	"log/slog"
	"math/rand"
	"strings"
	"sync"
	"time"

	"github.com/cn-maul/rosetta-gateway/internal/config"
	"github.com/cn-maul/rosetta-gateway/internal/outwire"
	"github.com/cn-maul/rosetta-gateway/internal/store"
	"github.com/cn-maul/rosetta-gateway/internal/upstream"
)

// Health 是健康判定的结果。
type Health string

const (
	HealthUnknown  Health = "unknown"  // 窗口内无探测记录
	HealthHealthy  Health = "healthy"  // 最近成功且窗口成功率达标
	HealthDegraded Health = "degraded" // 最近失败但未达 down，或成功率偏低
	HealthDown     Health = "down"     // 连续失败达阈值
)

// Classify 把窗口健康摘要归到一档。这是本特性的核心纯函数。
//
// # 为什么不能「一次不通就判死、一次通就判活」
//
// 单次结果噪声太大（上游偶发 429、瞬时抖动），据此切健康态会让总览页
// 的红绿在几分钟内来回跳，失去「一眼看出谁坏了」的意义。所以用两个
// 独立信号叠加：
//
//   - **连续失败**（down 的唯一依据）：连续 N 次失败才判 down。单次失败
//     只是 degraded，给上游一次自愈的机会。
//   - **窗口成功率**（healthy 的依据）：最近成功 + 窗口成功率 ≥ 阈值才
//     判 healthy。单次成功不足以把一个长期抖动的模型标成健康 —— 那会
//     掩盖「10 次里挂 6 次」的实情。
//
// 阈值全部来自配置（downThreshold / healthyRate），不硬编码。
func Classify(h store.ProbeHealth, downThreshold int, healthyRate float64) Health {
	if h.WindowCount == 0 {
		return HealthUnknown
	}
	if h.ConsecutiveFail >= int64(downThreshold) {
		return HealthDown
	}
	if h.LastOK {
		rate := float64(h.WindowOK) / float64(h.WindowCount)
		if rate >= healthyRate {
			return HealthHealthy
		}
		return HealthDegraded
	}
	// 最近失败但没到 down 阈值：degraded。
	return HealthDegraded
}

// Runner 是定时探测循环。
type Runner struct {
	store     *store.Store
	cfg       *config.Config
	masterKey []byte
	logger    *slog.Logger
	stagger   time.Duration
	// proxyProbe 是代理测速的注入点，默认 upstream.TestProxyConnectivity。
	//
	// 为什么要留这个口子：那条探测打的是固定的外部 HTTPS 目标
	// （https://www.gstatic.com/generate_204），单元测试既不能依赖外网，
	// 也没法用一个本地假代理顶上 —— 客户端对 https 目标会先发 CONNECT，
	// 假代理要么不会隧道要么通不过 TLS 校验。测试里换成假函数，验的是
	// 「调度、落库、取消纪律」，不是 generate_204 本身（后者在
	// upstream 包有自己的用例）。
	proxyProbe func(ctx context.Context, proxyURL string) (int64, error)
}

func NewRunner(st *store.Store, cfg *config.Config, masterKey []byte, logger *slog.Logger) *Runner {
	return &Runner{
		store: st, cfg: cfg, masterKey: masterKey, logger: logger,
		stagger: intervalStagger, proxyProbe: upstream.TestProxyConnectivity,
	}
}

// Run 阻塞执行探测循环，直到 ctx 取消。
//
// 照抄 runRecompute 的骨架（cmd/gateway/main.go）：周期短、需要并发上限、
// 有 done 等待先例。启动后先立刻跑一轮（不等第一个 ticker 周期），
// 这样「刚部署就能看到健康数据」而不是干等一个间隔。
func (r *Runner) Run(ctx context.Context) {
	interval := time.Duration(r.cfg.Probes.IntervalSec) * time.Second
	if interval <= 0 {
		interval = 10 * time.Minute
	}
	// 先跑一轮再进 ticker。
	r.runOnce(ctx)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.runOnce(ctx)
		}
	}
}

// runOnce 探测一轮全部上游模型。
//
// 并发上限由配置控制（默认 4）：一轮可能打几十个上游，全并发会同时打满
// 连接。每个模型在周期内随机偏移（stagger），避免所有模型同一秒打上游
// 造成限速尖峰。
func (r *Runner) runOnce(ctx context.Context) {
	models, err := r.store.ListAllUpstreamModels(ctx)
	if err != nil {
		r.logger.Warn("probe: list models failed", "error", err)
		return
	}
	if len(models) == 0 {
		return
	}

	concurrency := r.cfg.Probes.Concurrency
	if concurrency <= 0 {
		concurrency = 4
	}
	timeout := time.Duration(r.cfg.Probes.TimeoutMs) * time.Millisecond
	if timeout <= 0 {
		timeout = 30 * time.Second
	}

	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	outcomes := make([]probeOutcome, 0, len(models))
	var mu sync.Mutex
	for _, m := range models {
		m := m
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-sem }()

			// 每个模型随机偏移，避免同一秒全打上游。
			stagger := r.stagger
			if stagger <= 0 {
				stagger = intervalStagger
			}
			timer := time.NewTimer(time.Duration(rand.Int63n(int64(stagger))))
			select {
			case <-timer.C:
			case <-ctx.Done():
				timer.Stop()
				return
			}
			out := r.probeOne(ctx, m, timeout)
			if out.skipped {
				return
			}
			mu.Lock()
			outcomes = append(outcomes, out)
			mu.Unlock()
		}()
	}
	wg.Wait()

	// 一轮一条汇总，而不是每个失败各一条 WARN。
	//
	// 旧实现在这里没有汇总：失败只由 outwire 的日志副作用带出来，而那条
	// 日志既没有模型名也没有供应商（映射函数看不到它们），运维看到的是
	// 「每 10 分钟刷一条不知道是谁坏了」的 WARN。汇总把「这一轮几个坏了、
	// 分别是哪几个」一次说清；单个模型的成败明细降 DEBUG（要细节的人
	// 自己开 log_level=debug），失败原因仍逐条落在 model_probes.error_detail
	// 里供界面展示。
	r.logRoundOutcome(ctx, outcomes)

	// 代理测速跟着同一轮跑：它不打上游、不计费，一次 generate_204 的
	// 代价远小于一次推理，所以不需要独立周期 —— 独立周期只会让两套
	// 定时器在界面上显示的时间戳互相错开几分钟，看起来像「有的一直
	// 没测」。放在模型探测之后，顺序也符合排障时的因果链（先看模型
	// 通不通，再看代理通不通）。
	r.probeNetworks(ctx, timeout)

	// 代理测速跟着同一轮跑：它不打上游、不计费，一次 generate_204 的
	// 代价远小于一次推理，所以不需要独立周期 —— 独立周期只会让两套
	// 定时器在界面上显示的时间戳互相错开几分钟，看起来像「有的一直
	// 没测」。放在模型探测之后，顺序也符合排障时的因果链（先看模型
	// 通不通，再看代理通不通）。
	r.probeNetworks(ctx, timeout)

	// 顺带剪枝超期探测记录（保留期独立配置）。
	if ctx.Err() == nil && r.cfg.Probes.RetentionDays > 0 {
		if n, err := r.store.PruneOldProbes(ctx, r.cfg.Probes.RetentionDays); err != nil {
			r.logger.Warn("probe: prune failed", "error", err)
		} else if n > 0 {
			r.logger.Debug("probe: pruned old records", "count", n)
		}
		if n, err := r.store.PruneOldNetworkProbes(ctx, r.cfg.Probes.RetentionDays); err != nil {
			r.logger.Warn("probe: prune network probes failed", "error", err)
		} else if n > 0 {
			r.logger.Debug("probe: pruned old network records", "count", n)
		}
	}
}

// probeNetworks 对所有出站网络各做一次连通性测速。
//
// 为什么不再靠「测试」按钮：手动测试测的是「我刚才点那一下」，而代理挂掉
// 通常发生在没人点按钮的时候 —— 那时列表上还挂着上次成功留下的绿色。
// 改成定时测速后，界面上的数字与供应商延迟同一口径（窗口内的成功样本），
// 「代理坏了」这件事不需要人主动去问。
//
// 失败绝不写 poool 的熔断状态，也不改 provider 配置：与 model_probes 同一
// 条隔离红线，探测只是观测。
func (r *Runner) probeNetworks(ctx context.Context, timeout time.Duration) {
	networks, err := r.store.ListNetworks(ctx)
	if err != nil {
		r.logger.Warn("probe: list networks failed", "error", err)
		return
	}
	if len(networks) == 0 {
		return
	}

	concurrency := r.cfg.Probes.Concurrency
	if concurrency <= 0 {
		concurrency = 4
	}

	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	for _, n := range networks {
		n := n
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-sem }()
			r.probeNetwork(ctx, n, timeout)
		}()
	}
	wg.Wait()
}

// probeNetwork 测一条代理并落库。
func (r *Runner) probeNetwork(ctx context.Context, n store.Network, timeout time.Duration) {
	// 与模型探测同一套超时上限：代理探测自己内部还压了 10 秒硬上限
	// （见 upstream.TestProxyConnectivity），这里的 timeout 只是外层兜底，
	// 防止某条代理把整轮拖到下一个周期。
	pctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	probe := r.proxyProbe
	if probe == nil {
		probe = upstream.TestProxyConnectivity
	}
	latencyMs, err := probe(pctx, n.ProxyURL)

	// Cancellation is a lifecycle stop, not a probe result — 与 probeOne 一致。
	if ctx.Err() != nil {
		return
	}
	rec := store.NetworkProbe{
		NetworkID: n.ID,
		Ts:        time.Now().UnixMilli(),
		OK:        err == nil,
		LatencyMs: latencyMs,
	}
	if err != nil {
		// 错误正文已由 upstream 侧脱敏（代理 URL 里可能带账号密码，
		// TestProxyConnectivity 的错误文案不含完整 URL）。
		rec.Error = err.Error()
	}
	if err := r.store.RecordNetworkProbe(ctx, rec); err != nil {
		r.logger.Warn("probe: record network probe failed", "network", n.Name, "error", err)
	}
}

// intervalStagger 是探测抖动的**上限**（毫秒），不是「间隔的 1/4」。
//
// 目的只有一个：别让整轮探测在同一个瞬间打出去（同一秒对内网代理或
// 上游打十几个并发，会撞限速也会让延迟数字互相污染）。取固定 2.5 秒
// 而不是按间隔比例算：间隔调小到 60 秒时，1/4 只有 15 秒，抖动窗口
// 比探测本身还长，反而把一轮拖到下一轮；固定值在 60 秒与 1 小时间隔
// 都表现为「一小撮随机错开」。
var intervalStagger = 2500 * time.Millisecond

// probeOutcome 是一轮内单个模型的探测结论（只用于这一轮的汇总日志）。
//
// skipped 表示「这一条没得出结论」：上下文取消（进程在关）或落库失败。
// 这两种都不该算进「N 个模型不健康」，否则关一次进程或者一次写库抖动
// 就会在日志里伪装成一批上游故障。
type probeOutcome struct {
	ok      bool
	model   string
	errKind string
	latency int64
	skipped bool
}

// probeOne 探测单个模型并落库，返回本轮汇总需要的结论。
func (r *Runner) probeOne(ctx context.Context, m store.UpstreamModel, timeout time.Duration) probeOutcome {
	// 每个探测独立超时：一个挂死的上游不能卡住整轮。
	pctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	res := upstream.TestUpstreamModel(pctx, r.store, &m, r.masterKey, r.cfg)

	rec := store.ProbeRecord{
		ModelID:    m.ID,
		ProviderID: m.ProviderID,
		Ts:         time.Now().UnixMilli(),
		OK:         res.OK,
		InBand:     res.InBand,
		LatencyMs:  res.LatencyMs,
	}
	if !res.OK {
		rec.ErrorKind = classifyError(res.Err)
		rec.ErrorDetail = outwire.UpstreamErrorDetail(res.Err)
	}
	// Cancellation is a lifecycle stop, not a probe result. Do not mutate
	// probe history after the owner has begun shutdown.
	if ctx.Err() != nil {
		return probeOutcome{skipped: true}
	}
	if err := r.store.RecordProbe(ctx, rec); err != nil {
		r.logger.Warn("probe: record failed", "model", m.ModelID, "error", err)
		return probeOutcome{skipped: true}
	}
	out := probeOutcome{
		ok:      res.OK,
		model:   m.ModelID,
		errKind: rec.ErrorKind,
		latency: res.LatencyMs,
	}
	// 逐条结果只走 DEBUG：这是「细节」，要它的人会开 log_level=debug。
	// INFO 级别留在一轮的汇总上（见 logRoundOutcome）。
	r.logger.Debug("probe: model probed",
		"model", m.ModelID, "provider_id", m.ProviderID,
		"ok", res.OK, "latency_ms", res.LatencyMs, "error_kind", rec.ErrorKind)
	return out
}

// logRoundOutcome 把一轮探测的结果合成一条日志。
//
// 级别口径：
//   - 全部成功 → INFO（一行带样本数与最慢一条，用来确认探针还活着）；
//   - 有失败 → WARN，并列出失败模型的 provider/model/error_kind。
//
// 旧实现没有这条汇总，失败靠 outwire 的日志副作用带出来，且不带模型名 ——
// 一条无法定位的 WARN，每轮每个坏模型各一条。
func (r *Runner) logRoundOutcome(ctx context.Context, outcomes []probeOutcome) {
	if ctx.Err() != nil || len(outcomes) == 0 {
		return
	}
	failed := make([]probeOutcome, 0)
	var slowest int64
	for _, o := range outcomes {
		if !o.ok {
			failed = append(failed, o)
			continue
		}
		if o.latency > slowest {
			slowest = o.latency
		}
	}
	if len(failed) == 0 {
		r.logger.Info("probe: round finished",
			"models", len(outcomes), "failed", 0, "slowest_ms", slowest)
		return
	}
	names := make([]string, 0, len(failed))
	for _, o := range failed {
		names = append(names, o.model)
	}
	r.logger.Warn("probe: round finished with failing models",
		"models", len(outcomes), "failed", len(failed),
		"failing_models", strings.Join(names, ", "),
		"hint", "失败原因见 model_probes.error_detail（界面「模型探测」页可看原文）；若不期望该模型可用，可从上游模型列表删掉它")
}

// classifyError 把探测错误归到归一化的失败类别（时间轴/总览展示用）。
func classifyError(err error) string {
	if err == nil {
		return ""
	}
	// 复用 outwire 的归类：它已经按状态码/哨兵分好了。
	//
	// 用 UpstreamErrorCode 而不是 MapUpstreamError：后者对 400/404/529
	// 会顺带 slog.Warn 一条不带模型名的 "upstream returned an error"，
	// 而这里只是想知道「该归到哪一类」——归类是纯计算，不该有日志副作用。
	_, code, _ := outwire.UpstreamErrorCode(err)
	switch code {
	case "upstream_timeout":
		return "timeout"
	case "rate_limit_exceeded":
		return "rate_limited"
	case "upstream_auth_error":
		return "auth"
	case "model_not_found":
		return "not_found"
	case "upstream_error":
		return "upstream"
	default:
		return "error"
	}
}
