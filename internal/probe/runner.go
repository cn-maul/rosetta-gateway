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
}

func NewRunner(st *store.Store, cfg *config.Config, masterKey []byte, logger *slog.Logger) *Runner {
	return &Runner{store: st, cfg: cfg, masterKey: masterKey, logger: logger}
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

// runOnce 探测一轮全部启用的上游模型。
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
	// 只探测启用的模型。
	enabled := make([]store.UpstreamModel, 0, len(models))
	for _, m := range models {
		if m.Enabled {
			enabled = append(enabled, m)
		}
	}
	if len(enabled) == 0 {
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
	for _, m := range enabled {
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
			time.Sleep(time.Duration(rand.Int63n(int64(intervalStagger))))
			r.probeOne(ctx, m, timeout)
		}()
	}
	wg.Wait()

	// 顺带剪枝超期探测记录（保留期独立配置）。
	if r.cfg.Probes.RetentionDays > 0 {
		if n, err := r.store.PruneOldProbes(ctx, r.cfg.Probes.RetentionDays); err != nil {
			r.logger.Warn("probe: prune failed", "error", err)
		} else if n > 0 {
			r.logger.Debug("probe: pruned old records", "count", n)
		}
	}
}

// intervalStagger 是探测抖动上限（毫秒）。取间隔的 1/4，避免整轮探测
// 被同一个偏移量推成「所有模型同时打」。
var intervalStagger = 2500 * time.Millisecond

// probeOne 探测单个模型并落库。
func (r *Runner) probeOne(ctx context.Context, m store.UpstreamModel, timeout time.Duration) {
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
	if err := r.store.RecordProbe(ctx, rec); err != nil {
		r.logger.Warn("probe: record failed", "model", m.ModelID, "error", err)
	}
}

// classifyError 把探测错误归到归一化的失败类别（时间轴/总览展示用）。
func classifyError(err error) string {
	if err == nil {
		return ""
	}
	// 复用 outwire 的归类：它已经按状态码/哨兵分好了。
	_, code, _ := outwire.MapUpstreamError(err)
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
