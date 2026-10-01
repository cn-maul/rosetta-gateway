package server

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

func newTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// 管理写操作成功后必须自动触发重建（不再依赖前端调 reload）；
// GET、失败的写入、reload 端点自身、只读探测端点都不触发。
func TestAutoReload_TriggersOnSuccessfulWrite(t *testing.T) {
	var reloads int
	reload := func(context.Context) error { reloads++; return nil }
	logger := newTestLogger()

	base := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/admin/api/deny":
			w.WriteHeader(http.StatusUnauthorized)
		default:
			w.WriteHeader(http.StatusOK)
		}
	})
	h := AutoReload(base, reload, logger)

	call := func(method, path string) {
		t.Helper()
		rec := httptest.NewRecorder()
		// 经 statusResponseWriter 包装才有状态码可见 —— 与生产链路一致
		//（Middleware 在 AutoReload 外侧）。
		sw := &statusResponseWriter{ResponseWriter: rec, statusCode: http.StatusOK}
		h.ServeHTTP(sw, httptest.NewRequest(method, path, nil))
	}

	// GET 不触发。
	call(http.MethodGet, "/admin/api/stats")
	if reloads != 0 {
		t.Fatalf("GET must not trigger reload, got %d", reloads)
	}

	// 写方法 + 2xx 触发。
	call(http.MethodPost, "/admin/api/keys")
	call(http.MethodPatch, "/admin/api/keys/x")
	call(http.MethodPut, "/admin/api/settings")
	call(http.MethodDelete, "/admin/api/routes/x")
	if reloads != 4 {
		t.Fatalf("each successful write must trigger exactly one reload, got %d", reloads)
	}

	// 写方法 + 4xx（鉴权失败/业务失败）不触发。
	call(http.MethodPatch, "/admin/api/deny")
	if reloads != 4 {
		t.Fatalf("failed write must not trigger reload, got %d", reloads)
	}

	// reload 端点自身会重建，跳过以免双跑。
	call(http.MethodPost, "/admin/api/reload")
	if reloads != 4 {
		t.Fatalf("reload endpoint must not auto-reload itself, got %d", reloads)
	}

	// 只读探测端点不触发。
	call(http.MethodPost, "/admin/api/providers/x/test")
	call(http.MethodPost, "/admin/api/providers/x/models/discover")
	if reloads != 4 {
		t.Fatalf("read-only probe endpoints must not trigger reload, got %d", reloads)
	}
}

// reload 失败只留日志、不向上传播 —— 此时响应已发出，无法改写；
// 调用方（runtimeReloader）保证失败不改变运行状态。
func TestAutoReload_ReloadFailureIsLoggedNotPanicked(t *testing.T) {
	reload := func(context.Context) error { return errors.New("db busy") }
	base := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	h := AutoReload(base, reload, newTestLogger())

	rec := httptest.NewRecorder()
	sw := &statusResponseWriter{ResponseWriter: rec, statusCode: http.StatusOK}
	h.ServeHTTP(sw, httptest.NewRequest(http.MethodPost, "/admin/api/keys", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("reload failure must not alter the response, code=%d", rec.Code)
	}
}
