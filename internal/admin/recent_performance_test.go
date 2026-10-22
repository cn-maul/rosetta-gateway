package admin

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"testing"

	"github.com/cn-maul/rosetta-gateway/internal/store"
)

func TestStatsPerformanceUsesSelectedRangeAndScope(t *testing.T) {
	st := newTestStore(t)
	for i := 1; i <= 7; i++ {
		ttfb := int64(0)
		if i == 2 || i == 3 {
			ttfb = int64(i * 100)
		}
		if err := st.CreateUsageRecord(t.Context(), &store.UsageRecord{
			ID: fmt.Sprintf("sample-%d", i), Ts: int64(1000 + i), UserID: "u1", AccessKeyID: "k1",
			Status: "ok", OutputTokens: int64(i * 10), LatencyMs: 1000, TTFBMs: ttfb,
		}); err != nil {
			t.Fatal(err)
		}
	}
	for _, row := range []*store.UsageRecord{
		{ID: "other", Ts: 2000, UserID: "u2", AccessKeyID: "k2", Status: "ok", OutputTokens: 999, LatencyMs: 1000, TTFBMs: 900},
		{ID: "failure", Ts: 3000, UserID: "u1", AccessKeyID: "k1", Status: "error", OutputTokens: 999, LatencyMs: 1000, TTFBMs: 999},
		{ID: "unmeasured", Ts: 4000, UserID: "u1", AccessKeyID: "k1", Status: "ok", OutputTokens: 999},
	} {
		if err := st.CreateUsageRecord(t.Context(), row); err != nil {
			t.Fatal(err)
		}
	}
	h := NewStatsHandler(st)
	for _, tc := range []struct {
		user              string
		tps, ttfb         float64
		count, firstCount int64
	}{
		// 区间从 0 开始，统计的是所选范围内的全部成功样本，而不是“最近 5 条”。
		// u1 的输出 token 合计 10+20+...=280、耗时 7s → 40 tok/s；
		// 首字只有 i=2/3 有样本，均值 250ms。
		{"u1", 40, 250, 7, 2}, {"u2", 999, 900, 1, 1}, {"no-samples", 0, 0, 0, 0},
	} {
		t.Run(tc.user, func(t *testing.T) {
			rr := httptest.NewRecorder()
			h.Get(rr, asUser(httptest.NewRequest("GET", "/admin/api/stats?from=0&to=9999999999999", nil), tc.user))
			if rr.Code != 200 {
				t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
			}
			var got statsResponse
			if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if got.AvgTokensPerSec != tc.tps || got.AvgTtfbMs != tc.ttfb || got.ThroughputSamples != tc.count || got.TtfbSamples != tc.firstCount {
				t.Fatalf("stats=%+v", got)
			}
		})
	}
	got, err := st.GetRecentPerformance(t.Context(), 5, "")
	if err != nil || got.ThroughputSamples != 5 || got.TtfbSamples != 3 {
		t.Fatalf("admin samples=%+v err=%v", got, err)
	}
}
