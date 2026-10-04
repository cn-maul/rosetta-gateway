package ratelimit

import (
	"sync"
	"testing"
	"time"
)

// 单请求估算超过整个窗口额度时，该key 不能被永久饿死。
//
// 旧实现直接 `b.tokens + est > limit` 判否，且 b.tokens 只增不减 ——
// 于是「b.tokens + est > limit」在每个窗口都成立，该 key 永远 429，
// 而窗口翻转只清零 b.tokens、不改变 est 的大小关系。Claude Code 这类
// 长会话 + 小 tpm_limit 的组合会直接把这把 key 打死。
func TestTPM_OversizedEstimateDoesNotStarveKeyForever(t *testing.T) {
	l := New()
	const limit = 5000

	// est 远超额度：本次必须被拒（否则限速形同虚设）。
	if ok, _ := l.ReserveTPM("k1", limit, 100000); ok {
		t.Fatalf("单请求估算 100000 > 额度 5000，必须拒绝")
	}

	// 但额度被记为「已用满」而非累加到 est：下一个窗口必须能恢复。
	l.mu.Lock()
	b := l.buckets["k1"]
	used := b.tokens
	l.mu.Unlock()
	if used > int64(limit) {
		t.Fatalf("桶计数 %d 超过额度 %d：窗口翻转后仍会被永久拒绝", used, limit)
	}
}

// 未启用 TPM（limit=0）时不得留下任何桶 —— 否则删除 key 后条目永不回收，
// map 随历史上出现过的 key 数无界增长。这是主流配置（默认不开 TPM）。
func TestTPM_UnlimitedLeavesNoBuckets(t *testing.T) {
	l := New()

	// Reserve + Commit 走一遍完整生命周期，两次都不该建桶。
	for i := 0; i < 100; i++ {
		key := "k" + string(rune('a'+i%26))
		if ok, _ := l.ReserveTPM(key, 0, 999); !ok {
			t.Fatalf("limit=0 应恒放行")
		}
		l.CommitTPM(key, 0, 12345, 678)
	}

	l.mu.Lock()
	n := len(l.buckets)
	l.mu.Unlock()
	if n != 0 {
		t.Errorf("未启用 TPM 时创建了 %d 个桶，map 会随 key 增删无界增长", n)
	}
}

// 被删key 的桶必须能被回收。sweep 以 touched 为判据（不是 windowStart ——
// 活跃 key 每分钟都因窗口翻转而更新，只有真正不再用的才会过期）。
func TestLimiter_SweepsIdleBuckets(t *testing.T) {
	l := New()
	l.lastSweep = time.Time{} // 强制下一次 bucketLocked 触发清扫

	l.ReserveTPM("k1", 1000, 10)
	l.ReserveTPM("k2", 1000, 10)

	l.mu.Lock()
	n := len(l.buckets)
	l.mu.Unlock()
	if n != 2 {
		t.Fatalf("前置条件不成立：桶数 = %d", n)
	}

	// 把 lastSweep 往回拨，等价于「距上次清扫已超过 sweepInterval」。
	l.mu.Lock()
	l.lastSweep = time.Now().Add(-time.Hour)
	l.mu.Unlock()

	// 但两个桶都是刚触碰的（touched = now），不该被回收。
	l.ReserveTPM("k3", 1000, 10)
	l.mu.Lock()
	n = len(l.buckets)
	l.mu.Unlock()
	if n != 3 {
		t.Errorf("活跃桶被误回收：桶数 = %d，期望 3", n)
	}

	// 把两个老桶的 touched 推到很久以前，再触发清扫。
	l.mu.Lock()
	l.buckets["k1"].touched = time.Now().Add(-time.Hour)
	l.buckets["k2"].touched = time.Now().Add(-time.Hour)
	l.lastSweep = time.Now().Add(-time.Hour)
	l.mu.Unlock()
	l.ReserveTPM("k4", 1000, 10)

	l.mu.Lock()
	_, k1 := l.buckets["k1"]
	_, k3 := l.buckets["k3"]
	n = len(l.buckets)
	l.mu.Unlock()
	if k1 {
		t.Errorf("闲置超期的桶未被回收：桶数 = %d", n)
	}
	if !k3 {
		t.Errorf("活跃桶被误回收：桶数 = %d", n)
	}
}

// CommitTPM 跨窗口时不得把桶压成负数 —— 负数会让后续预留从负值起算，
// TPM 被静默放宽。
func TestTPM_NeverGoesNegative(t *testing.T) {
	l := New()

	// 在窗口 A 预占一大笔。
	if ok, _ := l.ReserveTPM("k1", 1000, 900); !ok {
		t.Fatalf("预占应通过")
	}
	// 手工把桶推到新窗口（tokens 已清零），模拟跨窗校正。
	l.mu.Lock()
	l.buckets["k1"].windowStart = 0 // 必定不等于当前分钟 → 下次访问时清零
	l.mu.Unlock()

	l.CommitTPM("k1", 1000, 900, 0) // 退还 900，落到已清零的新窗口

	l.mu.Lock()
	tokens := l.buckets["k1"].tokens
	l.mu.Unlock()
	if tokens < 0 {
		t.Errorf("桶被压成负数 %d：后续预留从负值起算，TPM 限速被静默放宽", tokens)
	}
}

// 并发下桶计数不得丢失或越界。
func TestLimiter_ConcurrentReserveCommit(t *testing.T) {
	l := New()
	const workers, per = 8, 200

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < per; i++ {
				l.ReserveTPM("k1", 1_000_000, 10)
				l.CommitTPM("k1", 1_000_000, 10, 10)
				l.AllowRPM("k1", 1_000_000)
			}
		}()
	}
	wg.Wait()

	l.mu.Lock()
	tokens := l.buckets["k1"].tokens
	requests := l.buckets["k1"].requests
	l.mu.Unlock()
	// reserved == actual 时校正是空操作，桶应停在最后一次预占的值。
	if tokens < 0 || requests < 0 {
		t.Errorf("并发下出现负值：tokens=%d requests=%d", tokens, requests)
	}
}
