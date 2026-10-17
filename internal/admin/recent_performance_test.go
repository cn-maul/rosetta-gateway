package admin

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"testing"

	"github.com/cn-maul/rosetta-gateway/internal/store"
)

func TestStatsRecentPerformanceSamplesAndScope(t *testing.T) {
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
		count, firstCount int
	}{
		{"u1", 50, 250, 5, 2}, {"u2", 999, 900, 1, 1}, {"no-samples", 0, 0, 0, 0},
	} {
		t.Run(tc.user, func(t *testing.T) {
			rr := httptest.NewRecorder()
			// Date-range statistics can be empty while recent samples still exist.
			h.Get(rr, asUser(httptest.NewRequest("GET", "/admin/api/stats?from=9999999999999", nil), tc.user))
			if rr.Code != 200 {
				t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
			}
			var got statsResponse
			if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if got.AvgTokensPerSec != tc.tps || got.AvgTtfbMs != tc.ttfb || got.ThroughputSamples != tc.count || got.TtfbSamples != tc.firstCount || got.RecentPerformanceStatus != "ready" || got.RecentSampleLimit != 5 || got.TotalRequests != 0 {
				t.Fatalf("stats=%+v", got)
			}
		})
	}
	got, err := st.GetRecentPerformance(t.Context(), 5, "")
	if err != nil || got.ThroughputSamples != 5 || got.TtfbSamples != 3 {
		t.Fatalf("admin samples=%+v err=%v", got, err)
	}
}
