package probe

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/cn-maul/rosetta"
)

// captureHandler 收集日志记录，用来断言「探针到底写了什么级别的日志」。
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

func (h *captureHandler) snapshot() []slog.Record {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]slog.Record, len(h.records))
	copy(out, h.records)
	return out
}

// 归类是纯计算，绝不能写日志。
//
// 这是「启动日志里那条 400 WARN」的回归钉：旧 classifyError 走
// MapUpstreamError，其 400/404/529 分支会 slog.Warn 一条不带模型名的
// "upstream returned an error"。那条日志既定位不到模型，又每轮每个坏模型
// 刷一条。
func TestClassifyError_不产生日志(t *testing.T) {
	cases := []error{
		&rosetta.APIError{StatusCode: 400, Type: "server_error", Message: "Model is unavailable."},
		&rosetta.APIError{StatusCode: 404},
		&rosetta.APIError{StatusCode: 410},
		&rosetta.APIError{StatusCode: 529},
		&rosetta.APIError{StatusCode: 429},
		&rosetta.APIError{StatusCode: 401},
		&rosetta.TransportError{Err: errors.New("dial tcp: timeout")},
		errors.New("boom"),
	}
	for _, err := range cases {
		h := &captureHandler{}
		prev := slog.Default()
		slog.SetDefault(slog.New(h))
		kind := classifyError(err)
		slog.SetDefault(prev)
		if n := len(h.snapshot()); n != 0 {
			t.Fatalf("classifyError(%v) 写了 %d 条日志（应为 0），归类结果 %q", err, n, kind)
		}
	}
}

// 归类结果本身不能变 —— 只换调用入口，不改语义。
func TestClassifyError_归类不变(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"nil", nil, ""},
		{"400", &rosetta.APIError{StatusCode: 400}, "error"},
		{"404", &rosetta.APIError{StatusCode: 404}, "not_found"},
		{"410", &rosetta.APIError{StatusCode: 410}, "not_found"},
		{"529", &rosetta.APIError{StatusCode: 529}, "upstream"},
		{"429", &rosetta.APIError{StatusCode: 429}, "rate_limited"},
		{"401", &rosetta.APIError{StatusCode: 401}, "auth"},
		{"500", &rosetta.APIError{StatusCode: 500}, "upstream"},
		{"transport", &rosetta.TransportError{Err: errors.New("x")}, "timeout"},
		{"unknown", errors.New("boom"), "error"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := classifyError(c.err); got != c.want {
				t.Fatalf("classifyError(%v) = %q, want %q", c.err, got, c.want)
			}
		})
	}
}

// 一轮探测的汇总：失败模型必须点名，不能只说「有失败」。
func TestLogRoundOutcome_全成功一条INFO(t *testing.T) {
	h := &captureHandler{}
	prev := slog.Default()
	slog.SetDefault(slog.New(h))
	defer slog.SetDefault(prev)

	r := &Runner{logger: slog.Default()}
	r.logRoundOutcome(context.Background(), []probeOutcome{
		{ok: true, model: "a", latency: 120},
		{ok: true, model: "b", latency: 300},
	})
	recs := h.snapshot()
	if len(recs) != 1 {
		t.Fatalf("应写 1 条汇总日志，实际 %d", len(recs))
	}
	if recs[0].Level != slog.LevelInfo {
		t.Fatalf("全成功应是 INFO，实际 %v", recs[0].Level)
	}
	attrs := attrsOf(recs[0])
	if attrs["failed"] != "0" || attrs["models"] != "2" || attrs["slowest_ms"] != "300" {
		t.Fatalf("汇总字段不对：%v", attrs)
	}
}

func TestLogRoundOutcome_有失败点名并WARN(t *testing.T) {
	h := &captureHandler{}
	prev := slog.Default()
	slog.SetDefault(slog.New(h))
	defer slog.SetDefault(prev)

	r := &Runner{logger: slog.Default()}
	r.logRoundOutcome(context.Background(), []probeOutcome{
		{ok: true, model: "spark", latency: 100},
		{ok: false, model: "minimax-m3.1-flash-free", errKind: "not_found"},
	})
	recs := h.snapshot()
	if len(recs) != 1 {
		t.Fatalf("应写 1 条汇总日志，实际 %d", len(recs))
	}
	if recs[0].Level != slog.LevelWarn {
		t.Fatalf("有失败应是 WARN，实际 %v", recs[0].Level)
	}
	attrs := attrsOf(recs[0])
	if attrs["failed"] != "1" {
		t.Fatalf("failed = %q, want 1", attrs["failed"])
	}
	if !strings.Contains(attrs["failing_models"], "minimax-m3.1-flash-free") {
		t.Fatalf("日志必须点名失败模型，实际 failing_models=%q", attrs["failing_models"])
	}
	if strings.Contains(attrs["failing_models"], "spark") {
		t.Fatalf("成功的模型不该出现在 failing_models 里：%q", attrs["failing_models"])
	}
}

// 取消/落库失败的那一条不算结论，不能污染「N 个模型不健康」。
func TestLogRoundOutcome_跳过的不计数(t *testing.T) {
	h := &captureHandler{}
	prev := slog.Default()
	slog.SetDefault(slog.New(h))
	defer slog.SetDefault(prev)

	r := &Runner{logger: slog.Default()}
	r.logRoundOutcome(context.Background(), []probeOutcome{
		{ok: true, model: "a", latency: 100},
	})
	attrs := attrsOf(h.snapshot()[0])
	if attrs["models"] != "1" || attrs["failed"] != "0" {
		t.Fatalf("跳过项不该被计入：%v", attrs)
	}
}

// 关闭过程中不该再写汇总（此时上下文已取消，日志只会干扰收尾排查）。
func TestLogRoundOutcome_取消后静默(t *testing.T) {
	h := &captureHandler{}
	prev := slog.Default()
	slog.SetDefault(slog.New(h))
	defer slog.SetDefault(prev)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := &Runner{logger: slog.Default()}
	r.logRoundOutcome(ctx, []probeOutcome{{ok: true, model: "a", latency: 1}})
	if n := len(h.snapshot()); n != 0 {
		t.Fatalf("取消后应静默，实际写了 %d 条", n)
	}
}

// 没有任何结论（全部在 stagger 阶段就被取消）时也不该写。
func TestLogRoundOutcome_空结果静默(t *testing.T) {
	h := &captureHandler{}
	prev := slog.Default()
	slog.SetDefault(slog.New(h))
	defer slog.SetDefault(prev)

	r := &Runner{logger: slog.Default()}
	r.logRoundOutcome(context.Background(), nil)
	if n := len(h.snapshot()); n != 0 {
		t.Fatalf("空结果应静默，实际写了 %d 条", n)
	}
}

func attrsOf(r slog.Record) map[string]string {
	out := map[string]string{}
	r.Attrs(func(a slog.Attr) bool {
		out[a.Key] = a.Value.String()
		return true
	})
	return out
}
