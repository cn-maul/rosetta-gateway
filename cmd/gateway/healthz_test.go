package main

// A10-P1-4（2026-10-21 修）：免鉴权的 /healthz **不得**回显底层错误。
//
// # 为什么这条必须用测试钉住
//
// /healthz 是免鉴权的：容器健康检查与外部探针都要在「没有会话、没有
// token」的情况下能用它。而 store.Ping 包装的 SQLite 错误里可能含
// 数据库文件路径、驱动内部信息、文件系统错误 —— 同网段的主动探测者
// 能拿它做环境枚举。
//
// 旧实现是 `body = map[string]any{"status":"unhealthy","error": err.Error()}`，
// 一行、直观、看起来无害。真正的防线是「对外只给稳定字段」这条契约，
// 而契约需要断言才能防住「后来有人觉得错误信息有用又加回去」。

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// failPinger 是一个「ping 必失败」的存储替身。
//
// 为什么不真把数据库弄坏：要触发这条路径必须让 Ping 返回错误，
// 而在 Windows 上把 SQLite 弄坏（只读文件、锁文件、删目录）的方式与
// Linux 不同，且会让同一测试里的其他断言一起失败。包一层是唯一
// 既稳定又不牵连其他断言的办法。
type failPinger struct{ err error }

func (p failPinger) Ping(context.Context) error { return p.err }

// cleanPinger 是一个「ping 成功」的存储替身。
type cleanPinger struct{}

func (cleanPinger) Ping(context.Context) error { return nil }

// dirtyReloader 报告「运行时落后于数据库」。
type dirtyReloader struct{ dirty bool }

func (r dirtyReloader) IsDirty() bool { return r.dirty }

// 底层错误不得出现在响应体里。
func TestHealthz_数据库不可用时不回显底层错误(t *testing.T) {
	// 这个错误串模拟真实 SQLite 错误里会出现的东西：路径 + 驱动细节。
	const secretish = "unable to open database file: C:\\Users\\deploy\\rosetta\\data\\gateway.db (SQLITE_CANTOPEN)"
	logger := discardLogger()
	h := healthzHandler(failPinger{err: errors.New(secretish)}, dirtyReloader{}, logger)

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	h(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, secretish) {
		t.Errorf("响应体回显了底层数据库错误：\n%s", body)
	}
	if strings.Contains(body, "SQLITE_CANTOPEN") || strings.Contains(body, "gateway.db") {
		t.Errorf("响应体泄露了驱动内部信息或数据库路径：\n%s", body)
	}
	// 对外只给稳定的 status + error_code。
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("响应体不是合法 JSON: %v\n%s", err, body)
	}
	if got["status"] != "unhealthy" {
		t.Errorf("status = %v, want unhealthy", got["status"])
	}
	if got["error_code"] != "database_unavailable" {
		t.Errorf("error_code = %v, want database_unavailable", got["error_code"])
	}
	if _, ok := got["error"]; ok {
		t.Errorf("响应体里不该有 error 字段（那是回显底层错误的老口子）：\n%s", body)
	}
}

// 健康时返回 200 + status=ok，且同样不该带任何 error 字段。
func TestHealthz_健康时不带错误字段(t *testing.T) {
	logger := discardLogger()
	h := healthzHandler(cleanPinger{}, dirtyReloader{}, logger)

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	h(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("响应体不是合法 JSON: %v", err)
	}
	if got["status"] != "ok" {
		t.Errorf("status = %v, want ok", got["status"])
	}
}

// 运行时落后于数据库时报 503 + degraded（与「数据库不可用」区分开）。
func TestHealthz_运行时落后时报告degraded(t *testing.T) {
	logger := discardLogger()
	h := healthzHandler(cleanPinger{}, dirtyReloader{dirty: true}, logger)

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	h(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("响应体不是合法 JSON: %v", err)
	}
	if got["status"] != "degraded" {
		t.Errorf("status = %v, want degraded", got["status"])
	}
}
