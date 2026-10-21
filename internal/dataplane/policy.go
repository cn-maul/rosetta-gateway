package dataplane

import (
	"log/slog"
	"time"

	"github.com/cn-maul/rosetta-gateway/internal/config"
	"github.com/cn-maul/rosetta-gateway/internal/snapshot"
)

// FailoverMaxTargets 解析「一次请求最多尝试链上几个目标」：
// 设置页写入的全局默认优先，未配置（0）回落 config。
func FailoverMaxTargets(snap *snapshot.Snapshot, cfg *config.Config) int {
	if snap != nil && snap.Runtime.FailoverMaxTargets > 0 {
		return snap.Runtime.FailoverMaxTargets
	}
	return cfg.FailoverMaxTargets()
}

// FailoverFailureThreshold 解析「某目标连续失败几次即熔断」：设置页优先，回落 config。
func FailoverFailureThreshold(snap *snapshot.Snapshot, cfg *config.Config) int {
	if snap != nil && snap.Runtime.FailoverFailureThreshold > 0 {
		return snap.Runtime.FailoverFailureThreshold
	}
	return cfg.FailoverFailureThreshold()
}

// SafeMillis 把毫秒数转成 time.Duration，并钳制在不会整数回绕的范围内。
//
// 为什么要钳制而不只是校验：校验（config.validate + settings_handler）挡住的是
// **新写入**的脏值，挡不住数据库里已经存在的、或在别的部署路径下写进去的。
// 而回绕的后果是灾难性的 —— time.Duration 是 int64 纳秒，
// `time.Duration(ms) * time.Millisecond` 在 ms > 9.223e12 时得到**负数**：
//
//	context.WithTimeout(ctx, 负值)  → deadline 立即过期 → 非流式请求全挂
//	time.AfterFunc(负值, ...)      → 立即开火 → 流刚发头就被自己关掉
//
// 而 UI 上显示的是一个「巨大但合法」的超时值，没有任何异常信号。
// 24 小时足够任何网关用，且离溢出点有 380 倍余量。
func SafeMillis(ms int) time.Duration {
	const maxMillis = 86_400_000 // 24h
	if ms <= 0 {
		return 0
	}
	if ms > maxMillis {
		slog.Warn("runtime millisecond setting exceeds safe range, clamping",
			"ms", ms, "clamped_to_ms", maxMillis)
		return time.Duration(maxMillis) * time.Millisecond
	}
	return time.Duration(ms) * time.Millisecond
}

// NonStreamTimeout：设置页的全局默认优先，其次 config 的上游超时。
func NonStreamTimeout(snap *snapshot.Snapshot, cfg *config.Config) time.Duration {
	if snap != nil && snap.Runtime.UpstreamTimeoutMs > 0 {
		return SafeMillis(snap.Runtime.UpstreamTimeoutMs)
	}
	return cfg.UpstreamTimeout()
}

// FirstTokenTimeout：设置页的全局默认优先，其次 config 的首字超时。
func FirstTokenTimeout(snap *snapshot.Snapshot, cfg *config.Config) time.Duration {
	if snap != nil && snap.Runtime.StreamFirstTokenTimeoutMs > 0 {
		return SafeMillis(snap.Runtime.StreamFirstTokenTimeoutMs)
	}
	return cfg.StreamFirstTokenTimeout()
}

// StreamIdleTimeout：设置页的全局默认优先，其次 config 的流式空闲超时。
func StreamIdleTimeout(snap *snapshot.Snapshot, cfg *config.Config) time.Duration {
	if snap != nil && snap.Runtime.StreamIdleTimeoutMs > 0 {
		return SafeMillis(snap.Runtime.StreamIdleTimeoutMs)
	}
	return cfg.StreamIdleTimeout()
}

// TotalRequestBudget 是**整个 /v1 请求**（跨所有故障转移尝试）的总上限。
//
// # 为什么需要它（2026-10-10 修复的 P2）
//
// 此前没有任何东西限制一次请求的总时长，而每个环节的超时都是**逐跳**的：
//
//	拨号/TLS 10s + 等响应头 60s + 首字 30s  ≈ 100s / 次尝试
//	× (failover_max_targets 默认 3)        ≈ 300s
//
// 于是「一串半死的上游」可以把客户端挂住约 5 分钟。期间它一直占着：一个
// 下游连接、一个上游连接、一条配额预占行、一个故障转移槽位。攒够几个，
// 网关对新请求而言已经不可用，而监控上看它「还活着」——
// 病在上游，症状却在网关自己身上。
//
// # 为什么由现有配置推导，而不是新增一个「总超时」开关
//
// 总预算必须**大于**任何单跳预算（故障转移会逐次复用单跳预算），
// 又必须**有限**。直接推导的好处是：运维调过的 upstream_timeout_ms、
// stream_first_token_timeout_ms、failover_max_targets 立刻反映到总预算上，
// 不需要「调完设置还要记得同步另一个总开关」这种跨字段的隐性依赖；
// 也不存在「总超时配得比单跳还短」这种自相矛盾的配置。
//
// # 公式与余量的来历
//
// 每次尝试的最坏上界取「非流式超时」与「首字 + 响应头等待」中较大的一个 ——
// 二者是不同路径，取 max 才不会把流式或非流式其中一条判短了。
// 再乘 (目标数+1)：+1 是给「最后一个目标失败后写错误响应」留的额度
// （failover_max_targets 限制的是尝试的**成功**目标数）。
// 每跳再加 perAttemptSlack：给「取流、解析、写头、落库」这些不计入任何
// 看门狗的开销留余量 —— 余量不足的表现是总预算先把健康请求掐掉，
// 那是比「慢」严重得多的事故。
//
// 非流式请求同样套用：它们的单跳预算就是 upstream_timeout_ms，
// 逐跳累加的后果完全一样。
func TotalRequestBudget(snap *snapshot.Snapshot, cfg *config.Config) time.Duration {
	// 逐跳上界：两条路径取较大者。responseHeaderWait 是等上游响应头的上限，
	// 它在**首字看门狗启动之前**发生，所以首字预算管不到它，必须单独算。
	perAttempt := NonStreamTimeout(snap, cfg)
	if first := FirstTokenTimeout(snap, cfg) + responseHeaderWait; first > perAttempt {
		perAttempt = first
	}
	perAttempt += perAttemptSlack

	attempts := FailoverMaxTargets(snap, cfg) + 1
	budget := time.Duration(attempts) * perAttempt

	// 下限保护：即便所有配置都被设成极小值，也要留出一个不至于让正常请求
	// 必然失败的地板。上限保护同理：配置被设得极大时，总预算不应该大到
	// 「等于没有」——那正是本函数要消灭的状态。
	const (
		minTotalBudget = 30 * time.Second
		maxTotalBudget = 30 * time.Minute
	)
	if budget < minTotalBudget {
		return minTotalBudget
	}
	if budget > maxTotalBudget {
		return maxTotalBudget
	}
	return budget
}

const (
	// responseHeaderWait 镜像 internal/upstream 里 ResponseHeaderTimeout 的
	// 60s。它出现在这里是因为总预算必须算上「拿到响应头之前」的那段 ——
	// 而首字看门狗是在 ChatStream 返回**之后**才启动的，覆盖不到这里。
	//
	// 两处必须同步改：改 upstream 的同时改这里，否则总预算会算少一段。
	responseHeaderWait = 60 * time.Second
	// perAttemptSlack 是每跳的固定余量，覆盖不计入任何看门狗的开销：
	// 取流、心跳、协议转换、落库。给小了会让总预算提前掐掉健康请求。
	perAttemptSlack = 5 * time.Second
)
