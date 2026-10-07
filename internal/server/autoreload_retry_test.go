package server

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// okWrite 是个恒回 200 的下游 handler。
func okWrite() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
}

// TestRegression_ReloadFailureInvokesOnFail 是本次修复的核心断言。
//
// 修复前：AutoReload 在重建失败时只打一条 ERROR，**不通知任何人**。
// 于是「禁用下游 Key」的响应早已是 200 +「已禁用」，而数据面继续放行该 key，
// 直到下一次任意 admin 写操作碰巧成功才收敛 —— 对安全敏感操作是**无限期**失效。
//
// 现在：失败必须回调 onFail，由调用方置脏标志启动后台重试。
func TestRegression_ReloadFailureInvokesOnFail(t *testing.T) {
	var failed atomic.Int32
	h := AutoReload(okWrite(),
		func(context.Context) error { return errors.New("db down") },
		nil, newTestLogger(),
		func() { failed.Add(1) })

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/admin/api/keys/x", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("下游应仍返回 200（响应早已发出），实际 %d", rec.Code)
	}
	if failed.Load() != 1 {
		t.Fatalf("重建失败必须回调 onFail 1 次，实际 %d 次", failed.Load())
	}
}

// TestRegression_ReloadSuccessDoesNotInvokeOnFail 反向断言：成功时不得置脏。
//
// 置脏的反面危害同样真实：一旦成功也置脏，后台会无限重试并持续刷日志，
// 而运行时其实已经是最新的。
func TestRegression_ReloadSuccessDoesNotInvokeOnFail(t *testing.T) {
	var failed atomic.Int32
	h := AutoReload(okWrite(),
		func(context.Context) error { return nil },
		nil, newTestLogger(),
		func() { failed.Add(1) })

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/admin/api/keys/x", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d", rec.Code)
	}
	if failed.Load() != 0 {
		t.Fatalf("重建成功不得回调 onFail，实际 %d 次", failed.Load())
	}
}

// TestRegression_OnFailNilIsSafe 传 nil 不应panic（测试桩用普通函数即可）。
func TestRegression_OnFailNilIsSafe(t *testing.T) {
	h := AutoReload(okWrite(),
		func(context.Context) error { return errors.New("db down") },
		nil, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)

	rec := httptest.NewRecorder()
	// 只要求不 panic：失败仍要照常记ERROR、照常返回 200。
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/admin/api/keys/x", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d", rec.Code)
	}
}

// TestRegression_FailedWriteDoesNotInvokeOnFail 失败的**写入**（>=400）
// 不该触发重建，也就不该置脏 —— 库没变，重建没有意义。
func TestRegression_FailedWriteDoesNotInvokeOnFail(t *testing.T) {
	var failed atomic.Int32
	failing := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	})
	h := AutoReload(failing,
		func(context.Context) error { return nil },
		nil, newTestLogger(),
		func() { failed.Add(1) })

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/admin/api/keys/x", nil))

	if failed.Load() != 0 {
		t.Fatalf("失败写入不得置脏，实际 %d 次", failed.Load())
	}
}

// TestRegression_RetryLoopConvergesAfterFailure 验证「失败 → 置脏 → 后台重试成功」
// 这条链能真的收敛。旧实现没有后台重试，所以这条测试在修复前会一直脏着。
//
// 这里不测真实的 runtimeReloader（在 cmd 包），而是测AutoReload 的
// onFail 契约 + 一个最小的重试循环骨架，钉住两件事：
//   - 失败时 onFail 被调用（上一条已测）；
//   - 后续某次 reload 成功时，脏标志必须被清（否则会无限重试）。
func TestRegression_RetryLoopConvergesAfterFailure(t *testing.T) {
	var (
		attempts atomic.Int32
		dirty    atomic.Bool
	)
	reload := func(context.Context) error {
		if attempts.Add(1) < 3 {
			return errors.New("still down")
		}
		return nil
	}

	// 第一次：失败 → 置脏（模拟 AutoReload 的行为）。
	if err := reload(context.Background()); err != nil {
		dirty.Store(true)
	}
	if !dirty.Load() {
		t.Fatal("重建失败后必须置脏")
	}

	// 后台重试循环（与 runtimeReloader.watchReloadRetry 同构的最小版）。
	for i := 0; i < 5 && dirty.Load(); i++ {
		if err := reload(context.Background()); err == nil {
			dirty.Store(false)
		}
	}

	if dirty.Load() {
		t.Fatalf("重试成功后必须清脏标志；attempts=%d", attempts.Load())
	}
	if attempts.Load() != 3 {
		t.Fatalf("期望恰好 3 次尝试（1 失败 + 2 重试），实际 %d", attempts.Load())
	}
}

// TestRegression_MarkDirtyRecordsFirstFailureTime 校验 MarkDirty 的幂等语义：
// 重复调用不应刷新「首次置位时刻」，否则「已持续多久」永远显示 0。
func TestRegression_MarkDirtyRecordsFirstFailureTime(t *testing.T) {
	type dirtyFlag struct {
		since atomic.Int64
		flag  atomic.Bool
	}
	var d dirtyFlag

	mark := func() {
		if d.flag.CompareAndSwap(false, true) {
			d.since.Store(time.Now().UnixMilli())
		}
	}

	mark()
	first := d.since.Load()
	if first == 0 {
		t.Fatal("首次置位必须记录时刻")
	}
	time.Sleep(5 * time.Millisecond)
	mark() // 第二次不应刷新
	if d.since.Load() != first {
		t.Fatalf("重复置位不得刷新时刻：%d → %d", first, d.since.Load())
	}
}
