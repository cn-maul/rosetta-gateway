package outwire

import (
	"context"
	"log/slog"
	"sync"
	"testing"

	"github.com/cn-maul/rosetta"
)

// captureHandler 收集所有日志记录，用来断言「某条路径到底写没写日志」。
type captureHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *captureHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, r.Clone())
	return nil
}

func (h *captureHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *captureHandler) WithGroup(string) slog.Handler      { return h }

func (h *captureHandler) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.records)
}

// withCapturedSlog 把默认 logger 换成捕获器，返回恢复函数。
func withCapturedSlog(t *testing.T) *captureHandler {
	t.Helper()
	h := &captureHandler{}
	prev := slog.Default()
	slog.SetDefault(slog.New(h))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return h
}

// MapUpstreamError 仍然要写：它只在真正要把错误回给客户端时调用，
// 那时这条日志是排查的唯一线索。
func TestMapUpstreamError_仍写日志(t *testing.T) {
	h := withCapturedSlog(t)
	status, code, _ := MapUpstreamError(&rosetta.APIError{
		StatusCode: 400, Type: "server_error", Message: "Model is unavailable.",
	})
	if status != 400 || code != "invalid_request_error" {
		t.Fatalf("映射结果变了：status=%d code=%q", status, code)
	}
	if h.count() != 1 {
		t.Fatalf("MapUpstreamError 应写 1 条日志，实际 %d", h.count())
	}
}

// logAPIError 的字段补齐了：上游的 code / param / request_id / in_band。
// 没有 RequestID 时不能写出空字段（会让日志采集器里多一堆空键）。
func TestLogAPIError_带上游上下文(t *testing.T) {
	h := withCapturedSlog(t)
	logAPIError(&rosetta.APIError{
		StatusCode: 400,
		Type:       "invalid_request_error",
		Code:       "model_not_found",
		Param:      "model",
		Message:    "Model is unavailable.",
		RequestID:  "req_abc123",
	})
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.records) != 1 {
		t.Fatalf("应写 1 条，实际 %d", len(h.records))
	}
	got := map[string]string{}
	h.records[0].Attrs(func(a slog.Attr) bool {
		got[a.Key] = a.Value.String()
		return true
	})
	for k, want := range map[string]string{
		"status": "400", "type": "invalid_request_error", "code": "model_not_found",
		"param": "model", "message": "Model is unavailable.", "upstream_request_id": "req_abc123",
	} {
		if got[k] != want {
			t.Fatalf("字段 %s = %q, want %q（全部字段：%v）", k, got[k], want, got)
		}
	}
}

func TestLogAPIError_空字段不输出(t *testing.T) {
	h := withCapturedSlog(t)
	logAPIError(&rosetta.APIError{StatusCode: 500, Type: "server_error", Message: "boom"})
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.records) != 1 {
		t.Fatalf("应写 1 条，实际 %d", len(h.records))
	}
	keys := map[string]bool{}
	h.records[0].Attrs(func(a slog.Attr) bool {
		keys[a.Key] = true
		return true
	})
	for _, k := range []string{"code", "param", "upstream_request_id", "in_band"} {
		if keys[k] {
			t.Fatalf("空字段 %s 不该出现在日志里：%v", k, keys)
		}
	}
}
