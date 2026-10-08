package server

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// 2026-10-10 回归：并发上限曾误用于会话鉴权路径，导致总览页（并发 5 个请求）
// 的第 5 个必然 429，报「密码尝试过于频繁」—— 而用户密码正确、根本没在登录。
//
// 修法是会话鉴权不施加并发上限（该上限只为限制 PBKDF2 的 CPU 放大），
// 失败计数与冷却照旧。下面两条把它钉住。
func TestSessionAuth_ParallelReadsAreNotRateLimited(t *testing.T) {
	a := newSessionGate(t)

	// 总览页的并发数（api.stats + usageByDay + usageByModel + usageByKey + usageByProvider）。
	const parallel = 5

	var wg sync.WaitGroup
	codes := make([]int, parallel)
	start := make(chan struct{})
	for i := range parallel {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodGet, "/admin/api/stats", nil)
			req.RemoteAddr = "198.51.100.20:34567"
			rec := httptest.NewRecorder()
			<-start // 尽量让 5 个请求真正重叠
			a.ServeHTTP(rec, req, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
			}))
			codes[i] = rec.Code
		}(i)
	}
	close(start)
	wg.Wait()

	// 全部应是 401（没带令牌），绝不能是 429。
	for i, c := range codes {
		if c == http.StatusTooManyRequests {
			t.Fatalf("第 %d 个并发请求被限速成 429 —— 会话鉴权路径不应施加并发上限", i)
		}
		if c != http.StatusUnauthorized {
			t.Fatalf("第 %d 个并发请求期望 401，得到 %d", i, c)
		}
	}
}

// 反向钉住：失败计数与冷却**必须**照旧生效。去掉并发上限不能连带把
// 「拿无效令牌撞门」的防护一起去掉。
func TestSessionAuth_StillThrottlesAfterLimit(t *testing.T) {
	a := newSessionGate(t)
	do := func(token string) int {
		req := httptest.NewRequest(http.MethodGet, "/admin/api/stats", nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		req.RemoteAddr = "198.51.100.21:34567"
		rec := httptest.NewRecorder()
		a.ServeHTTP(rec, req, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		return rec.Code
	}

	// 无效令牌全部计入失败计数（单发，不涉及并发）。
	for i := range LoginFailLimit {
		if got := do("bad-" + string(rune('a'+i))); got != http.StatusUnauthorized {
			t.Fatalf("第 %d 次无效令牌应 401，得到 %d", i+1, got)
		}
	}
	if got := do("bad-again"); got != http.StatusTooManyRequests {
		t.Fatalf("超过阈值应 429，得到 %d —— 去掉并发上限时把失败计数也弄丢了", got)
	}
}

// 会话鉴权的限速器构造时就不带并发上限（NewUserAuth 的约定）。
func TestSessionAuth_ThrottleHasNoConcurrencyCap(t *testing.T) {
	a := newSessionGate(t)
	if a.throttle.capConcurrent {
		t.Fatal("会话鉴权的限速器不应施加并发上限（它不跑 KDF，只统计失败次数）")
	}
	// 登录端点的限速器仍然必须带上限。
	th := NewFailureThrottle(1, time.Second)
	if !th.capConcurrent {
		t.Fatal("默认构造的限速器必须施加并发上限（登录/改密路径要跑 KDF）")
	}
}
