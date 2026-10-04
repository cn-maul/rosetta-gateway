package ratelimit

import (
	"sync"
	"time"
)

// Limiter 是下游 Key 维度的 RPM/TPM 限速器（DESIGN §11.4）。
//
// 实现：内存固定窗口 —— 每个 key 一个每分钟翻转的桶，记请求数与 token 数。
// 不用 golang.org/x/time/rate：零依赖是本项目纪律；且令牌桶「匀速放行」的
// 语义与「每分钟 N 个请求」的运营直觉不符。
//
// 取舍（§11.4 已明示，运营可见）：
//   - 计数只在内存，重启归零 —— 接受；
//   - 桶按 keyID 建立且不清扫：key 由管理员创建、数量有界，不会泄漏；
//   - TPM 先按请求前估算预占，请求后按真实 usage 校正（CommitTPM）；
//     校正落在「当前」窗口 —— 若请求跨了窗口边界，差值会记入新窗口，
//     误差量级为单个请求，可接受；
//   - 单锁守护：热路径只有一次 map 读写（微秒级），不值得分片。
type Limiter struct {
	mu      sync.Mutex
	buckets map[string]*windowBucket
}

type windowBucket struct {
	windowStart int64 // 当前窗口起点（Unix 分钟数）
	requests    int64
	tokens      int64
}

const windowLen = time.Minute

func New() *Limiter {
	return &Limiter{buckets: make(map[string]*windowBucket)}
}

// bucketLocked 取 key 的桶，窗口翻转时清零计数。
func (l *Limiter) bucketLocked(keyID string, now time.Time) *windowBucket {
	b, ok := l.buckets[keyID]
	if !ok {
		b = &windowBucket{}
		l.buckets[keyID] = b
	}
	if start := now.Unix() / 60; b.windowStart != start {
		b.windowStart = start
		b.requests = 0
		b.tokens = 0
	}
	return b
}

// AllowRPM 记一次请求并报告是否超出 rpmLimit（0 = 不限）。
// 被拒绝的请求同样计数：固定窗口语义下请求就是发生了，放行会放大突发。
func (l *Limiter) AllowRPM(keyID string, rpmLimit int) (ok bool, retryAfter time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if rpmLimit <= 0 {
		return true, 0
	}
	now := time.Now()
	b := l.bucketLocked(keyID, now)
	if b.requests >= int64(rpmLimit) {
		return false, windowRemaining(now)
	}
	b.requests++
	return true, 0
}

// ReserveTPM 为一次请求预占 est 个 token，报告是否超出 tpmLimit（0 = 不限）。
// 预占随后由 CommitTPM 按真实 usage 校正；被拒绝的请求不预占 ——
// 没放行的请求不该消耗窗口额度。
func (l *Limiter) ReserveTPM(keyID string, tpmLimit int, est int64) (ok bool, retryAfter time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if tpmLimit <= 0 {
		return true, 0
	}
	now := time.Now()
	b := l.bucketLocked(keyID, now)
	if b.tokens+est > int64(tpmLimit) {
		return false, windowRemaining(now)
	}
	b.tokens += est
	return true, 0
}

// CommitTPM 把预占的 reserved 校正为真实用量 actual（差值回补/追加到当前窗口）。
// reserved == actual 时是空操作；actual 通常是 usage.TotalTokens（输入+输出）。
func (l *Limiter) CommitTPM(keyID string, reserved, actual int64) {
	if reserved == actual {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.bucketLocked(keyID, time.Now())
	b.tokens += actual - reserved
}

func windowRemaining(now time.Time) time.Duration {
	next := now.Truncate(windowLen).Add(windowLen)
	// +1s 余量：边界整秒时 Retry-After 恰好为 0 会让客户端立即重试又立即被拒。
	return next.Sub(now) + time.Second
}
