package ratelimit

import (
	"testing"
	"time"
)

// RPM：窗口内计数递增，超限拒绝并给出窗口剩余时间；翻窗后清零。
func TestAllowRPM_FixedWindow(t *testing.T) {
	l := New()

	for i := 0; i < 3; i++ {
		if ok, _ := l.AllowRPM("k1", 3); !ok {
			t.Fatalf("request %d should pass", i+1)
		}
	}
	ok, retry := l.AllowRPM("k1", 3)
	if ok {
		t.Fatalf("4th request should be rejected")
	}
	if retry <= 0 || retry > time.Minute+2*time.Second {
		t.Fatalf("retryAfter = %v, want (0, 1m2s]", retry)
	}

	// 不限（0）永远放行，且不限额 key 不建桶计数干扰。
	for i := 0; i < 100; i++ {
		if ok, _ := l.AllowRPM("k2", 0); !ok {
			t.Fatalf("unlimited key should always pass")
		}
	}

	// 被拒的请求也计数：窗口翻转前再怎么试都被拒。
	if ok, _ := l.AllowRPM("k1", 3); ok {
		t.Fatalf("rejected request must still count against the window")
	}
}

// TPM：预占超限拒绝且不消耗额度；Commit 把预占校正为真实用量。
func TestReserveTPM_ReserveAndCommit(t *testing.T) {
	l := New()

	if ok, _ := l.ReserveTPM("k1", 1000, 600); !ok {
		t.Fatalf("first reserve should pass")
	}
	ok, retry := l.ReserveTPM("k1", 1000, 600)
	if ok {
		t.Fatalf("second reserve (600+600>1000) should be rejected")
	}
	if retry <= 0 {
		t.Fatalf("retryAfter must be positive")
	}

	// 提交真实用量 200：预占 600 → 校正为 200，窗口余 800。
	l.CommitTPM("k1", 1000, 600, 200)
	if ok, _ := l.ReserveTPM("k1", 1000, 800); !ok {
		t.Fatalf("800 should fit after commit correction")
	}

	// 全额退还（请求失败，actual=0）：第二次预占的 800 全退，
	// 窗口回到已提交的真实用量 200 —— 退还语义是「退预占差值」，不是清零。
	l.CommitTPM("k1", 1000, 800, 0)
	if ok, _ := l.ReserveTPM("k1", 1000, 800); !ok {
		t.Fatalf("refund should restore window to committed usage (200), leaving 800 available")
	}
	if ok, _ := l.ReserveTPM("k1", 1000, 1); ok {
		t.Fatalf("window must still hold the committed 200 tokens")
	}
}

// 不限（0）与零值边界。
func TestTPM_UnlimitedAndZero(t *testing.T) {
	l := New()
	if ok, _ := l.ReserveTPM("k1", 0, 1<<30); !ok {
		t.Fatalf("unlimited key should always pass")
	}
	// est=0 的请求预占 0：即使额度 1 也能过，且 commit(actual) 走正常校正。
	if ok, _ := l.ReserveTPM("k2", 1, 0); !ok {
		t.Fatalf("zero estimate should pass")
	}
	l.CommitTPM("k2", 1, 0, 50)
	// 校正后窗口 tokens=50 > 1 → 拒绝。
	if ok, _ := l.ReserveTPM("k2", 1, 0); ok {
		t.Fatalf("window should reflect committed usage")
	}
}

// 不同的 key 互不影响。
func TestKeysAreIsolated(t *testing.T) {
	l := New()
	if ok, _ := l.AllowRPM("a", 1); !ok {
		t.Fatalf("first request of key a should pass")
	}
	if ok, _ := l.AllowRPM("b", 1); !ok {
		t.Fatalf("key b must not be affected by key a")
	}
	if ok, _ := l.AllowRPM("a", 1); ok {
		t.Fatalf("second request of key a should be rejected")
	}
}
