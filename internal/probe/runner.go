// Package probe 对出站代理做定时连通性测速。
//
// 为什么不靠「测试」按钮：手动测试测的是「我刚才点那一下」，而代理挂掉
// 通常发生在没人点按钮的时候 —— 那时列表上还挂着上次成功留下的绿色。
// 定时测速让「代理坏了」不需要人主动去问，界面延迟列也始终反映近期状态。
//
// 失败绝不写 pool 的熔断状态，也不改 provider 配置：探测只是观测。
package probe

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/cn-maul/rosetta-gateway/internal/config"
	"github.com/cn-maul/rosetta-gateway/internal/store"
	"github.com/cn-maul/rosetta-gateway/internal/upstream"
)

// Runner 是定时代理测速循环。
type Runner struct {
	store  *store.Store
	cfg    *config.Config
	logger *slog.Logger
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

func NewRunner(st *store.Store, cfg *config.Config, logger *slog.Logger) *Runner {
	return &Runner{
		store: st, cfg: cfg, logger: logger,
		proxyProbe: upstream.TestProxyConnectivity,
	}
}

// Run 阻塞执行测速循环，直到 ctx 取消。
//
// 启动后先立刻跑一轮（不等第一个 ticker 周期），这样「刚部署就能看到延迟」
// 而不是干等一个间隔。
func (r *Runner) Run(ctx context.Context) {
	interval := time.Duration(r.cfg.Probes.IntervalSec) * time.Second
	if interval <= 0 {
		interval = 10 * time.Minute
	}
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

// runOnce 对所有出站网络各做一次连通性测速，并顺带剪枝超期记录。
func (r *Runner) runOnce(ctx context.Context) {
	timeout := time.Duration(r.cfg.Probes.TimeoutMs) * time.Millisecond
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	r.probeNetworks(ctx, timeout)

	if ctx.Err() == nil && r.cfg.Probes.RetentionDays > 0 {
		if n, err := r.store.PruneOldNetworkProbes(ctx, r.cfg.Probes.RetentionDays); err != nil {
			r.logger.Warn("probe: prune network probes failed", "error", err)
		} else if n > 0 {
			r.logger.Debug("probe: pruned old network records", "count", n)
		}
	}
}

// probeNetworks 对所有出站网络各做一次连通性测速。
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
	// 代理探测自己内部还压了 10 秒硬上限（见 upstream.TestProxyConnectivity），
	// 这里的 timeout 只是外层兜底，防止某条代理把整轮拖到下一个周期。
	pctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	probe := r.proxyProbe
	if probe == nil {
		probe = upstream.TestProxyConnectivity
	}
	latencyMs, err := probe(pctx, n.ProxyURL)

	// Cancellation is a lifecycle stop, not a probe result.
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
