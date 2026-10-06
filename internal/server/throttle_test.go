package server

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

// TestFailureThrottleAccumulates 钉住一个真实踩过的坑：计数必须能累加到阈值。
//
// 修复前的写法是 `if !ok || !time.Now().Before(e.until) { e = &failEntry{} }`——
// 新条目的 until 是零值，而「现在」永远不在零值之前，于是这个条件对新条目恒为真：
// 每次失败都新建条目、count 清回 1，`count >= limit` 永远不成立。
// 编译通过、看代码「像是对的」，只有实测才会发现限速完全没生效
// （2026-09-24：连打 26 次错误密码，全是 401，一条 429 都没有）。
func TestFailureThrottleAccumulates(t *testing.T) {
	const limit = 10
	th := NewFailureThrottle(limit, time.Minute)
	const ip = "203.0.113.7"

	for i := 1; i < limit; i++ {
		if !th.Allow(ip) {
			t.Fatalf("第 %d 次失败就进了冷却：阈值 %d，不该这么早", i, limit)
		}
		th.Fail(ip)
	}

	if !th.Allow(ip) {
		t.Fatalf("第 %d 次失败前不该被拦截", limit)
	}
	th.Fail(ip)

	if th.Allow(ip) {
		t.Fatalf("累计 %d 次失败后必须进入冷却，但 Allow 仍放行", limit)
	}
	if d := th.RetryAfter(ip); d <= 0 || d > time.Minute {
		t.Fatalf("冷却剩余时长应在 (0, 1m] 内，得到 %v", d)
	}
}

// TestFailureThrottleSuccessResets 一次成功即清零，正常使用不会误伤。
func TestFailureThrottleSuccessResets(t *testing.T) {
	th := NewFailureThrottle(3, time.Minute)
	const ip = "203.0.113.8"

	th.Fail(ip)
	th.Fail(ip)
	th.Success(ip)
	th.Fail(ip)
	th.Fail(ip)

	if !th.Allow(ip) {
		t.Fatal("成功一次应清空计数，之后两次失败不该触发冷却")
	}
}

// TestFailureThrottleCooldownExpiry 冷却结束后计数必须从零重新开始，
// 且能重新累加到阈值（即 until 已被正确清回零值）。
func TestFailureThrottleCooldownExpiry(t *testing.T) {
	th := NewFailureThrottle(2, time.Millisecond)
	const ip = "203.0.113.9"

	th.Fail(ip)
	th.Fail(ip)
	if th.Allow(ip) {
		t.Fatal("达到阈值应立即进入冷却")
	}

	time.Sleep(5 * time.Millisecond)
	if !th.Allow(ip) {
		t.Fatal("冷却结束后应放行")
	}

	// 冷却后的两次失败要能再次触发。若 until 没被清零，
	// count 会在每次 Fail 时被重置，这里就会永远进不了冷却。
	th.Fail(ip)
	if !th.Allow(ip) {
		t.Fatal("冷却后第一次失败不该立刻再进冷却")
	}
	th.Fail(ip)
	if th.Allow(ip) {
		t.Fatal("冷却后累计到阈值应再次进入冷却（说明 until 未清零、计数被反复重置）")
	}
}

// newSessionGate 构造「会话已启用、无任何用户」的管理门禁。
//
// 限速现在只挂在会话解析这一条路径上（运维凭据通道已删除），
// 所以这才是限速真正生效的地方。
func newSessionGate(t *testing.T) *UserAuthMiddleware {
	t.Helper()
	return NewUserAuth(newEnabledTestManager(t), bootstrapSessionStore{})
}

// TestSessionAuthThrottledBlocksWith429 端到端地确认限速真的接在请求路径上：
// 前 limit 次是 401，之后必须是 429 且带 Retry-After。
func TestSessionAuthThrottledBlocksWith429(t *testing.T) {
	a := newSessionGate(t)

	do := func(token string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/admin/api/stats", nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		req.RemoteAddr = "198.51.100.4:34567"
		rec := httptest.NewRecorder()
		a.ServeHTTP(rec, req, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		return rec
	}

	limit := LoginFailLimit
	for i := range limit {
		if got := do("invalid-" + strconv.Itoa(i)).Code; got != http.StatusUnauthorized {
			t.Fatalf("第 %d 次无效令牌应 401，得到 %d", i+1, got)
		}
	}

	rec := do("invalid-again")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("超过阈值应 429，得到 %d", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Fatal("429 响应必须带 Retry-After 头")
	}

	// 冷却期内即便带上格式正确的令牌也拒绝 —— 否则爆破者可以靠「猜对」跳过惩罚。
	if got := do("well-formed-but-invalid-token").Code; got != http.StatusTooManyRequests {
		t.Fatalf("冷却期内请求也应被限速拦截，得到 %d", got)
	}
}

// TestSessionAuthBlocksAnonymous 基础拒绝路径：没有任何令牌必须 401，
// 且不得触达下游 handler。
func TestSessionAuthBlocksAnonymous(t *testing.T) {
	a := newSessionGate(t)
	reached := false

	req := httptest.NewRequest(http.MethodGet, "/admin/api/stats", nil)
	req.RemoteAddr = "198.51.100.5:1234"
	rec := httptest.NewRecorder()
	a.ServeHTTP(rec, req, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	}))

	if reached {
		t.Fatal("未登录请求不该触达下游 handler")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("期望 401，得到 %d", rec.Code)
	}
}

// TestClientIPIgnoresForwardedFor 来源 IP 不采信可伪造头，
// 否则攻击者换个 X-Forwarded-For 就能重置自己的计数。
func TestClientIPIgnoresForwardedFor(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/admin/api/stats", nil)
	req.RemoteAddr = "198.51.100.6:9999"
	req.Header.Set("X-Forwarded-For", "1.2.3.4")
	if got := clientIP(req); got != "198.51.100.6" {
		t.Fatalf("clientIP = %q，不应采信 X-Forwarded-For", got)
	}
}
