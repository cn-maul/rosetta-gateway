package upstream

import (
	"sync"
	"testing"
	"time"
)

// 熔断器的半开名额只对「冷却到期后的恢复期」生效，绝不能把健康目标
// 也限成一个并发。
//
// # 这是 P0-11 的回归防线（2026-10-21）
//
// 旧实现里 halfOpen 是一个全局互斥位：ClaimTargetProbe 先查冷却、再查
// halfOpen，两者都不看目标是否「正在恢复」。于是任何时刻一个目标最多
// 只允许 1 个在途请求 —— 开启 failover 后健康路由的并发能力塌成 1，
// 其余请求静默改投链上下一个目标（若那是另一家供应商，就是**在健康
// 状态下把流量送往另一家并按另一家计费**，而界面显示熔断正常）。
func TestClaimTargetProbe_健康目标不限并发(t *testing.T) {
	p := newTestPool()
	const tid = "t#0"
	// 目标存活、从未失败 —— 与生产稳态完全一致。
	p.Install(map[string]*ProviderEntry{}, LiveTargets{IDs: map[string]bool{tid: true}, Known: true})

	const n = 64
	var ok int
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, claimed := p.ClaimTargetProbe(tid); claimed {
				mu.Lock()
				ok++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if ok != n {
		t.Fatalf("健康目标只放行了 %d/%d 个并发 claim；熔断器不该在健康期限流", ok, n)
	}
}

// 冷却到期后的**恢复期**仍只放一个探针 —— 惊群防护不能被一起拆掉。
//
// 这与上一条是同一枚硬币的两面：健康期不限流，恢复期限流。
func TestClaimTargetProbe_恢复期只放一个探针(t *testing.T) {
	p := newTestPool()
	const tid = "t#0"
	p.Install(map[string]*ProviderEntry{}, LiveTargets{IDs: map[string]bool{tid: true}, Known: true})
	p.RecordTargetFailure(tid, 0, 1) // 立即熔断

	// 冷却期内：既不可用，也领不到名额。
	if p.TargetAvailable(tid) {
		t.Fatalf("熔断期内应不可用")
	}
	if _, ok := p.ClaimTargetProbe(tid); ok {
		t.Fatalf("熔断期内不应领到探测名额")
	}

	// 模拟「冷却刚到期」。
	p.mu.Lock()
	p.targets[tid].until = time.Now().Add(-time.Second)
	p.mu.Unlock()

	if _, ok := p.ClaimTargetProbe(tid); !ok {
		t.Fatalf("冷却刚到期时应放行第一个探测请求")
	}
	if _, ok := p.ClaimTargetProbe(tid); ok {
		t.Errorf("恢复期内不应放行第二个探针（否则熔断-惊群重现）")
	}
}

// 探针成功后目标回到「健康」，恢复期结束 —— 不再受半开名额限制。
//
// 没有这条，一次「坏过又好了」的目标会永久停在恢复期：每次冷却到期
// 都只放一个探针，其余请求静默改投次目标。这正是 P0-11 的残留形态。
func TestClaimTargetProbe_探针成功后恢复健康(t *testing.T) {
	p := newTestPool()
	const tid = "t#0"
	p.Install(map[string]*ProviderEntry{}, LiveTargets{IDs: map[string]bool{tid: true}, Known: true})
	p.RecordTargetFailure(tid, 0, 1)
	p.mu.Lock()
	p.targets[tid].until = time.Now().Add(-time.Second)
	p.mu.Unlock()

	token, ok := p.ClaimTargetProbe(tid)
	if !ok {
		t.Fatalf("冷却到期应放行探针")
	}
	p.RecordTargetSuccess(tid, token) // 探针成功 → 目标确认健康

	// 目标已健康：并发不再受限。
	if _, ok := p.ClaimTargetProbe(tid); !ok {
		t.Fatalf("探针成功后目标应回到健康态，不再被半开名额限制")
	}
	// 第一个 claim 只验证放行，不占用名额（目标是健康的）。
	if _, ok := p.ClaimTargetProbe(tid); !ok {
		t.Fatalf("健康目标的第二个并发 claim 也应放行")
	}
}

// 熔断被显式关闭（threshold=0）时，目标不进恢复期 —— 运维「关了熔断」
// 得到的必须是真的不限流，而不是「被限成一个并发 + 静默改投次目标」。
func TestRecordTargetFailure_阈值关闭时不进恢复期(t *testing.T) {
	p := newTestPool()
	const tid = "t#0"
	p.Install(map[string]*ProviderEntry{}, LiveTargets{IDs: map[string]bool{tid: true}, Known: true})

	p.RecordTargetFailure(tid, 0, 0) // 熔断关闭

	if _, ok := p.ClaimTargetProbe(tid); !ok {
		t.Fatalf("熔断已关闭，目标不应被半开名额限制")
	}
	if _, ok := p.ClaimTargetProbe(tid); !ok {
		t.Fatalf("熔断已关闭，并发 claim 应全部放行")
	}
}

// 熔断前发出的旧请求可能在恢复期的新探针之后才返回。它不能成功、失败或
// 归还名额时清掉**新探针**的占用，否则第二个请求会立刻绕过半开保护。
func TestProbeToken_迟到旧请求不影响新探针(t *testing.T) {
	const tid = "t#0"

	setup := func(t *testing.T) (p *Pool, oldToken, newToken ProbeToken) {
		t.Helper()
		p = newTestPool()
		p.Install(map[string]*ProviderEntry{}, LiveTargets{IDs: map[string]bool{tid: true}, Known: true})

		// 第一次熔断，冷却到期后旧请求拿到恢复期探针 A。
		p.RecordTargetFailure(tid, 0, 1)
		p.expireCooldownForTest(tid)
		oldToken, ok := p.ClaimTargetProbe(tid)
		if !ok || oldToken == 0 {
			t.Fatalf("前置条件不成立：未领到第一代探针")
		}

		// A 尚未返回时，另一条更早发出的在途请求再次触发熔断。
		// token=0 表示它在 claim 之前就已发出，属于上一周期；它打开
		// 新周期后，A 的迟到结果不能再支配新周期的探针名额。
		p.RecordTargetFailure(tid, 0, 1)
		p.expireCooldownForTest(tid)
		newToken, ok = p.ClaimTargetProbe(tid)
		if !ok || newToken == 0 {
			t.Fatalf("前置条件不成立：未领到第二代探针")
		}
		if newToken == oldToken {
			t.Fatalf("探针令牌没有换代：old=%d new=%d", oldToken, newToken)
		}
		return p, oldToken, newToken
	}

	assertNewProbeStillHeld := func(t *testing.T, p *Pool) {
		t.Helper()
		// 先把冷却拨到到期：否则「领不到」可能只是冷却未结束，
		// 掩盖掉「新探针名额被旧请求误清」这个真正的缺陷。
		p.expireCooldownForTest(tid)
		if _, ok := p.ClaimTargetProbe(tid); ok {
			t.Fatalf("迟到旧请求清掉了新探针名额，恢复期放行了第二个请求")
		}
	}

	t.Run("旧请求成功", func(t *testing.T) {
		p, oldToken, newToken := setup(t)
		p.RecordTargetSuccess(tid, oldToken)
		assertNewProbeStillHeld(t, p)
		assertCurrentProbeRecoversTarget(t, p, newToken)
	})

	t.Run("旧请求失败", func(t *testing.T) {
		p, oldToken, newToken := setup(t)
		// threshold 调高，隔离「迟到失败只累计、不再开新周期」这条
		// 路径；否则重新熔断会合法地让 B 所属周期失效。
		p.RecordTargetFailure(tid, oldToken, 100)
		assertNewProbeStillHeld(t, p)
		assertCurrentProbeRecoversTarget(t, p, newToken)
	})

	t.Run("旧请求归还", func(t *testing.T) {
		p, oldToken, newToken := setup(t)
		p.ReleaseTargetProbe(tid, oldToken)
		assertNewProbeStillHeld(t, p)
		assertCurrentProbeRecoversTarget(t, p, newToken)
	})
}

func assertCurrentProbeRecoversTarget(t *testing.T, p *Pool, token ProbeToken) {
	t.Helper()
	const targetID = "t#0"
	p.expireCooldownForTest(targetID)
	p.RecordTargetSuccess(targetID, token)
	if _, ok := p.ClaimTargetProbe(targetID); !ok {
		t.Fatalf("当前新探针成功后应结束恢复期，后续请求不应再受限")
	}
}

// 新探针自己的成功结果仍必须释放名额并结束恢复期。
func TestProbeToken_当前探针成功恢复健康(t *testing.T) {
	p := newTestPool()
	const tid = "t#0"
	p.Install(map[string]*ProviderEntry{}, LiveTargets{IDs: map[string]bool{tid: true}, Known: true})
	p.RecordTargetFailure(tid, 0, 1)
	p.expireCooldownForTest(tid)

	token, ok := p.ClaimTargetProbe(tid)
	if !ok {
		t.Fatalf("冷却到期应放行探针")
	}
	p.RecordTargetSuccess(tid, token)

	if _, ok := p.ClaimTargetProbe(tid); !ok {
		t.Fatalf("当前探针成功后应结束恢复期，后续请求不应再受限")
	}
}

func (p *Pool) expireCooldownForTest(targetID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if h, ok := p.targets[targetID]; ok {
		h.until = time.Now().Add(-time.Second)
	}
}
