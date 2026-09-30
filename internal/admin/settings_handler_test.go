package admin

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cn-maul/rosetta-gateway/internal/config"
)

// 设置页新增的运行时全局默认（超时 + 故障转移策略）必须能保存并读回；
// 未配置时 Get 要回显 config 的生效值，而不是留一个 0 让人误读成「不限」。
func TestSettingsHandler_RuntimeDefaults(t *testing.T) {
	st := newTestStore(t)
	cfg := config.Default()
	h := NewSettingsHandler(st, cfg)

	// 1) 未配置：回显 config 的值（不等于 0）。
	rec := httptest.NewRecorder()
	h.Get(rec, httptest.NewRequest(http.MethodGet, "/admin/api/settings", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("get code=%d body=%s", rec.Code, rec.Body.String())
	}
	var got settingsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.UpstreamTimeoutMs != cfg.Defaults.UpstreamTimeoutMs ||
		got.FailoverMaxTargets != cfg.FailoverMaxTargets() ||
		got.FailoverFailureThreshold != cfg.FailoverFailureThreshold() {
		t.Fatalf("unset should echo config defaults, got %+v", got)
	}

	// 2) 保存一组新值 → 读回应一致。
	want := settingsResponse{
		DefaultContextWindow:      128000,
		DefaultMaxOutputTokens:    8192,
		UpstreamTimeoutMs:         90000,
		StreamIdleTimeoutMs:       45000,
		StreamFirstTokenTimeoutMs: 20000,
		FailoverMaxTargets:        4,
		FailoverFailureThreshold:  5,
	}
	b, _ := json.Marshal(want)
	rec2 := httptest.NewRecorder()
	h.Update(rec2, httptest.NewRequest(http.MethodPut, "/admin/api/settings", bytes.NewReader(b)))
	if rec2.Code != http.StatusOK {
		t.Fatalf("update code=%d body=%s", rec2.Code, rec2.Body.String())
	}

	rec3 := httptest.NewRecorder()
	h.Get(rec3, httptest.NewRequest(http.MethodGet, "/admin/api/settings", nil))
	var back settingsResponse
	if err := json.Unmarshal(rec3.Body.Bytes(), &back); err != nil {
		t.Fatalf("unmarshal back: %v", err)
	}
	if back != want {
		t.Fatalf("round-trip mismatch:\n got %+v\nwant %+v", back, want)
	}
}

// 超时/阈值给 0 或负数必须 400：这些值直接喂给看门狗与 context.WithTimeout，
// 0 会变成「立即超时」，把全部转发打挂。
func TestSettingsHandler_RejectsNonPositive(t *testing.T) {
	st := newTestStore(t)
	h := NewSettingsHandler(st, config.Default())

	base := settingsResponse{
		DefaultContextWindow:      8192,
		DefaultMaxOutputTokens:    4096,
		UpstreamTimeoutMs:         120000,
		StreamIdleTimeoutMs:       60000,
		StreamFirstTokenTimeoutMs: 30000,
		FailoverMaxTargets:        3,
		FailoverFailureThreshold:  3,
	}
	cases := []struct {
		name  string
		mutate func(*settingsResponse)
	}{
		{"upstream_timeout_ms=0", func(s *settingsResponse) { s.UpstreamTimeoutMs = 0 }},
		{"stream_idle_timeout_ms=0", func(s *settingsResponse) { s.StreamIdleTimeoutMs = 0 }},
		{"stream_first_token_timeout_ms=-1", func(s *settingsResponse) { s.StreamFirstTokenTimeoutMs = -1 }},
		{"failover_max_targets=0", func(s *settingsResponse) { s.FailoverMaxTargets = 0 }},
		{"failover_failure_threshold=0", func(s *settingsResponse) { s.FailoverFailureThreshold = 0 }},
		{"default_context_window=0", func(s *settingsResponse) { s.DefaultContextWindow = 0 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := base
			tc.mutate(&in)
			b, _ := json.Marshal(in)
			rec := httptest.NewRecorder()
			h.Update(rec, httptest.NewRequest(http.MethodPut, "/admin/api/settings", bytes.NewReader(b)))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("want 400, got %d body=%s", rec.Code, rec.Body.String())
			}
		})
	}
}
