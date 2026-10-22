package server

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cn-maul/rosetta-gateway/internal/identity"
	"github.com/cn-maul/rosetta-gateway/internal/store"
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
	audit := func(string, string, string, int, string, string) {}

	base := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/admin/api/deny":
			w.WriteHeader(http.StatusUnauthorized)
		default:
			w.WriteHeader(http.StatusOK)
		}
	})
	h := AutoReload(base, reload, audit, logger, nil)

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

// 审计回调在成功写操作上被调用，且只带字段名、不带请求体值。
func TestAutoReload_AuditCapturesFieldNames(t *testing.T) {
	var audited []string
	audit := func(actor, method, path string, status int, remote, fields string) {
		audited = append(audited, actor+"|"+method+"|"+path+"|"+fields)
	}
	reload := func(context.Context) error { return nil }
	base := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	h := AutoReload(base, reload, audit, newTestLogger(), nil)

	rec := httptest.NewRecorder()
	sw := &statusResponseWriter{ResponseWriter: rec, statusCode: http.StatusOK}
	req := httptest.NewRequest(http.MethodPost, "/admin/api/keys", strings.NewReader(`{"name":"a","quota_tokens":100}`))
	req = req.WithContext(identity.WithUser(req.Context(), &store.User{ID: "u1", Username: "alice"}))
	h.ServeHTTP(sw, req)

	if len(audited) != 1 {
		t.Fatalf("audit should fire exactly once, got %v", audited)
	}
	// 字段名排序后拼接；值（"a"/100）绝不出现。
	if audited[0] != "alice|POST|/admin/api/keys|name,quota_tokens" {
		t.Fatalf("audit entry = %q", audited[0])
	}

	// GET 不审计。
	rec = httptest.NewRecorder()
	sw = &statusResponseWriter{ResponseWriter: rec, statusCode: http.StatusOK}
	h.ServeHTTP(sw, httptest.NewRequest(http.MethodGet, "/admin/api/keys", nil))
	if len(audited) != 1 {
		t.Fatalf("GET must not be audited, got %v", audited)
	}
}

// 审计 A7-16：不可逆 prune 的 keep_days 在 query string 里，必须连同值
// 进入审计；其他 query 参数不采集，避免把 token/api_key 之类敏感值落库。
func TestAutoReload_AuditCapturesKeepDaysQueryValue(t *testing.T) {
	var audited []string
	audit := func(actor, method, path string, status int, remote, fields string) {
		audited = append(audited, fields)
	}
	reload := func(context.Context) error { return nil }
	base := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	h := AutoReload(base, reload, audit, newTestLogger(), nil)

	rec := httptest.NewRecorder()
	sw := &statusResponseWriter{ResponseWriter: rec, statusCode: http.StatusOK}
	req := httptest.NewRequest(http.MethodPost,
		"/admin/api/usage/prune?keep_days=45&token=should-not-be-audited", nil)
	h.ServeHTTP(sw, req)

	if len(audited) != 1 {
		t.Fatalf("audit should fire once, got %v", audited)
	}
	if audited[0] != "keep_days=45" {
		t.Fatalf("audit fields = %q, want keep_days=45 and no unrelated query values", audited[0])
	}
}

// 审计 A7-15：bootstrap 的失败必须留痕（错误令牌/重复初始化是攻击信号）。
func TestAuditOnly_RecordsBootstrapFailure(t *testing.T) {
	var audited []struct {
		status int
		fields string
	}
	audit := func(actor, method, path string, status int, remote, fields string) {
		audited = append(audited, struct {
			status int
			fields string
		}{status, fields})
	}
	base := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	})
	h := AuditOnly(base, audit)

	req := httptest.NewRequest(http.MethodPost, "/admin/api/bootstrap",
		strings.NewReader(`{"password":"pw","token":"bad"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("handler status = %d, want 403", rec.Code)
	}
	if len(audited) != 1 {
		t.Fatalf("bootstrap failure must be audited, got %d entries", len(audited))
	}
	if audited[0].status != http.StatusForbidden {
		t.Errorf("audit status = %d, want 403", audited[0].status)
	}
	if audited[0].fields != "password,token" {
		t.Errorf("audit fields = %q, want field names only", audited[0].fields)
	}
}

// reload 失败只留日志、不向上传播 —— 此时响应已发出，无法改写；
// 调用方（runtimeReloader）保证失败不改变运行状态。
func TestAutoReload_ReloadFailureIsLoggedNotPanicked(t *testing.T) {
	reload := func(context.Context) error { return errors.New("db busy") }
	base := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	h := AutoReload(base, reload, nil, newTestLogger(), nil)

	rec := httptest.NewRecorder()
	sw := &statusResponseWriter{ResponseWriter: rec, statusCode: http.StatusOK}
	h.ServeHTTP(sw, httptest.NewRequest(http.MethodPost, "/admin/api/keys", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("reload failure must not alter the response, code=%d", rec.Code)
	}
}
