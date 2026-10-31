package admin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/cn-maul/rosetta-gateway/internal/store"
)

// seedProbeAt 在指定时间戳落一条探测记录（模型行与 provider 行按需补）。
//
// 与 probe_handler_test.go 的 seedProbe 的差别只有 ts：那个固定用 time.Now()，
// 用来测「摘要里能不能看到」；这个要测**桶边界**，必须能指定时间。
func seedProbeAt(t *testing.T, st *store.Store, modelID string, ts int64, ok bool) {
	t.Helper()
	ctx := context.Background()
	if existing, err := st.GetProvider(ctx, "p1"); err == nil && existing == nil {
		if err := st.CreateProvider(ctx, &store.Provider{
			ID: "p1", Slug: "good", Name: "good", Protocol: "chat",
			Endpoint: "https://example.invalid/v1", Enabled: true,
		}); err != nil {
			t.Fatalf("seed provider: %v", err)
		}
	}
	if existing, err := st.GetUpstreamModel(ctx, modelID); err == nil && existing == nil {
		if err := st.CreateUpstreamModel(ctx, &store.UpstreamModel{
			ID: modelID, ProviderID: "p1", ModelID: modelID}); err != nil {
			t.Fatalf("seed upstream model %s: %v", modelID, err)
		}
	}
	if err := st.RecordProbe(ctx, store.ProbeRecord{
		ModelID: modelID, ProviderID: "p1", Ts: ts, OK: ok, InBand: ok, LatencyMs: 12,
	}); err != nil {
		t.Fatalf("seed probe %s: %v", modelID, err)
	}
}

// TestProbeTimeline_DayGranularityUsesLocalDayWindow 天粒度的时间轴按
// **本地日**切桶与取窗口（2026-10-31 修，后端接口审计 #4）。
//
// 两件事一起钉：
//  1. 桶起点是本地午夜，不是 UTC 午夜（东八区下差 8 小时，落在 08:00）；
//  2. 窗口从「本地日界」起算 `days` 天，而不是滚动 now-days*24h —— 后者
//     会把首尾两天切成半个桶，前端按「最近 N 天」画 N 根柱子时就对不上。
//
// 构造：昨天本地 00:30 与 23:30 各一条探测，请求 days=2&granularity=day。
// 期望恰好 1 个桶（昨天），起点＝昨天本地午夜，count=2。若桶仍按 UTC 切，
// 这两条会分属两个桶（本地 00:30 属 UTC 前一天 16:30 桶）—— 直接红。
func TestProbeTimeline_DayGranularityUsesLocalDayWindow(t *testing.T) {
	st := newScopeStore(t)
	initProbeScopedSnapshot(t, nil)

	y := time.Now().AddDate(0, 0, -1)
	midnight := time.Date(y.Year(), y.Month(), y.Day(), 0, 0, 0, 0, time.Local)
	seedProbeAt(t, st, "up/flash", midnight.Add(30*time.Minute).UnixMilli(), true)
	seedProbeAt(t, st, "up/flash", midnight.Add(23*time.Hour+30*time.Minute).UnixMilli(), false)

	h := NewProbeHandler(st, nil)
	code, buckets := fetchTimeline(t, h, asAdmin(httptest.NewRequest(http.MethodGet,
		"/admin/api/probes/timeline?model_id=up%2Fflash&granularity=day&days=2", nil)))
	if code != http.StatusOK {
		t.Fatalf("timeline code = %d, want 200", code)
	}
	if len(buckets) != 1 {
		t.Fatalf("昨天两条探测应合成 1 个天桶，实际 %d 个: %+v", len(buckets), buckets)
	}
	if buckets[0].Ts != midnight.UnixMilli() {
		t.Fatalf("天桶起点应为本地午夜 %s，实际 %s",
			midnight.Format(time.RFC3339),
			time.UnixMilli(buckets[0].Ts).In(time.Local).Format(time.RFC3339))
	}
	if buckets[0].Count != 2 || buckets[0].OK != 1 {
		t.Fatalf("桶计数应为 2 次 / 1 成功，实际 %d / %d", buckets[0].Count, buckets[0].OK)
	}
}

// TestProbeTimeline_DayGranularityWindowExcludesOlderDays days 卡住窗口
// 下界：days=1 时窗口从**今天本地午夜**起算，昨天的探测不该出现。
//
// 构造刻意选「昨天 23:30」而不是中午：滚动窗口 now-24h 永远包含昨天 23:30
// （此刻离它不足 24 小时），所以这条用例能把「按本地日界取窗口」与
// 「滚动 now-days*24h」区分开 —— 后者会把昨天那根柱子也画出来。
func TestProbeTimeline_DayGranularityWindowExcludesOlderDays(t *testing.T) {
	st := newScopeStore(t)
	initProbeScopedSnapshot(t, nil)

	y := time.Now().AddDate(0, 0, -1)
	yesterdayLate := time.Date(y.Year(), y.Month(), y.Day(), 23, 30, 0, 0, time.Local)
	seedProbeAt(t, st, "up/flash", yesterdayLate.UnixMilli(), true)
	todayNoon := time.Date(time.Now().Year(), time.Now().Month(), time.Now().Day(), 12, 0, 0, 0, time.Local)
	seedProbeAt(t, st, "up/flash", todayNoon.UnixMilli(), true)

	h := NewProbeHandler(st, nil)
	code, buckets := fetchTimeline(t, h, asAdmin(httptest.NewRequest(http.MethodGet,
		"/admin/api/probes/timeline?model_id=up%2Fflash&granularity=day&days=1", nil)))
	if code != http.StatusOK {
		t.Fatalf("timeline code = %d, want 200", code)
	}
	if len(buckets) != 1 {
		t.Fatalf("days=1 应只剩今天 1 个桶，实际 %d 个: %+v", len(buckets), buckets)
	}
	today := time.Date(time.Now().Year(), time.Now().Month(), time.Now().Day(), 0, 0, 0, 0, time.Local)
	if buckets[0].Ts != today.UnixMilli() {
		t.Fatalf("唯一桶应是今天本地午夜 %s，实际 %s",
			today.Format(time.RFC3339),
			time.UnixMilli(buckets[0].Ts).In(time.Local).Format(time.RFC3339))
	}
}
