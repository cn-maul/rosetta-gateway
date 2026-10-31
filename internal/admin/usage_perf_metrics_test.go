package admin

// 总览七指标里新增的三项在 HTTP 契约上的形状：
//
//   - /admin/api/stats 的 avg_gen_ms / gen_samples（「首字之后」的生成耗时）；
//   - /admin/api/usage/by-day 每行的 cached_tokens / cache_hit_rate /
//     avg_gen_ms / n_gen。
//
// # 为什么这两处要分开钉
//
// stats 与 by-day 走的是两条**不同的 SQL**（GetUsageStats 单条聚合 vs
// groupBy 的分组查询），但界面上它们是同一个数字的「区间值」与「那天的值」。
// 任何一侧漏加列，表现都是「总览说命中率 70%，趋势表里那天写 0%」——
// 而这在 JSON 里完全不报错（Go 的零值会安静地序列化成 0）。
//
// 所以这里断言的是**非零且正确**，不是「字段存在」。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/cn-maul/rosetta-gateway/internal/store"
)

// perfDay 是 by-day 响应里本文件关心的那几列。
type perfDay struct {
	Key          string  `json:"key"`
	Count        int64   `json:"count"`
	Tokens       int64   `json:"tokens"`
	Ok           int64   `json:"ok"`
	Err          int64   `json:"err"`
	CachedTokens int64   `json:"cached_tokens"`
	CacheHitRate float64 `json:"cache_hit_rate"`
	AvgGenMs     float64 `json:"avg_gen_ms"`
	NGen         int64   `json:"n_gen"`
}

// seedPerfUsage 造两条同一天的记录，刻意让三件事同时成立：
//   - 命中率 ≠ 0 也 ≠ 1（分母是输入 token，不是总量）；
//   - 生成耗时 > 0 且两条不等（钉住「加权」而不是「取最后一条」）；
//   - 有一条失败记录（不能污染命中率与生成耗时的样本）。
func seedPerfUsage(t *testing.T, st *store.Store, dayStart int64) {
	t.Helper()
	ctx := t.Context()
	rows := []*store.UsageRecord{
		{
			ID: "perf-a", Ts: dayStart + 3_600_000, UserID: "u1", AccessKeyID: "k1",
			PublicModel: "m", ProviderID: "p1", UpstreamModel: "up", IngressProtocol: "chat",
			InputTokens: 1000, OutputTokens: 100, TotalTokens: 1100, CachedTokens: 800,
			Status: "ok", HTTPStatus: 200, UsageState: "reported",
			LatencyMs: 1200, TTFBMs: 200, // 生成 1000
		},
		{
			ID: "perf-b", Ts: dayStart + 7_200_000, UserID: "u1", AccessKeyID: "k1",
			PublicModel: "m", ProviderID: "p1", UpstreamModel: "up", IngressProtocol: "chat",
			InputTokens: 1000, OutputTokens: 100, TotalTokens: 1100, CachedTokens: 200,
			Status: "ok", HTTPStatus: 200, UsageState: "reported",
			LatencyMs: 3200, TTFBMs: 200, // 生成 3000
		},
		{
			ID: "perf-err", Ts: dayStart + 9_000_000, UserID: "u1", AccessKeyID: "k1",
			PublicModel: "m", ProviderID: "p1", UpstreamModel: "up", IngressProtocol: "chat",
			InputTokens: 5000, OutputTokens: 0, TotalTokens: 5000, CachedTokens: 5000,
			Status: "error", HTTPStatus: 500, UsageState: "reported",
			LatencyMs: 8000, TTFBMs: 100,
		},
	}
	for _, r := range rows {
		if err := st.CreateUsageRecord(ctx, r); err != nil {
			t.Fatalf("seed %s: %v", r.ID, err)
		}
	}
}

// TestStats_GenTimeExposed 钉住 stats 端点的两段耗时口径与作用域。
//
// 期望值按「成功 + 测到首字」的三条记录里的两条算：
// 生成耗时 (1000+3000)/2 = 2000，样本 2；首字恒 200，样本 2。
func TestStats_GenTimeExposed(t *testing.T) {
	st := newTestStore(t)
	seedPerfUsage(t, st, localDayStart(t, 0))
	h := NewStatsHandler(st)

	call := func(t *testing.T, as func(*http.Request) *http.Request) statsResponse {
		t.Helper()
		rr := httptest.NewRecorder()
		h.Get(rr, as(httptest.NewRequest(http.MethodGet, "/admin/api/stats?from=0&to=9999999999999", nil)))
		if rr.Code != http.StatusOK {
			t.Fatalf("code=%d body=%s", rr.Code, rr.Body.String())
		}
		var got statsResponse
		if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
			t.Fatalf("unmarshal: %v body=%s", err, rr.Body.String())
		}
		return got
	}

	admin := call(t, asAdmin)
	if admin.AvgGenMs != 2000 || admin.GenSamples != 2 {
		t.Fatalf("admin 视角生成耗时应为 avg=2000 samples=2，实际 avg=%v samples=%d（整个响应：%+v）",
			admin.AvgGenMs, admin.GenSamples, admin)
	}
	if admin.AvgTtfbMs != 200 || admin.TtfbSamples != 2 {
		t.Fatalf("首字应与生成同源同样本：avg=%v samples=%d", admin.AvgTtfbMs, admin.TtfbSamples)
	}

	// 作用域：别的用户看到的是 0/0，而不是全局数字。
	other := call(t, func(r *http.Request) *http.Request { return asUser(r, "u2") })
	if other.AvgGenMs != 0 || other.GenSamples != 0 {
		t.Fatalf("u2 没有用量，生成耗时必须是 0/0，实际 avg=%v samples=%d", other.AvgGenMs, other.GenSamples)
	}
}

// TestGroupByDay_PerfMetrics 钉住趋势表每天那几列。
//
// 命中率分母是 input_tokens：成功两条 800+200=1000，输入 1000+1000=2000
// → 50%。刻意让失败那条带着 5000 缓存 token 与 5000 输入 —— 如果实现
// 把非成功请求也算进分母，这里会掉到 (1000+5000)/(2000+5000) ≈ 85.7%，
// 而那正是「命中率看起来很漂亮、实际是被失败请求抬起来的」那种假绿。
func TestGroupByDay_PerfMetrics(t *testing.T) {
	st := newTestStore(t)
	dayStart := localDayStart(t, 0)
	seedPerfUsage(t, st, dayStart)
	h := NewUsageHandler(st)

	rec := httptest.NewRecorder()
	to := strconv.FormatInt(localDayStart(t, -1)+86_400_000, 10)
	h.GroupByDay(rec, asAdmin(httptest.NewRequest(http.MethodGet, "/admin/api/usage/by-day?from=0&to="+to, nil)))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var page struct {
		Records []perfDay `json:"records"`
		Total   int64     `json:"total"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, rec.Body.String())
	}
	if len(page.Records) != 1 {
		t.Fatalf("应只有今天一行，实际 %d：%+v", len(page.Records), page.Records)
	}
	got := page.Records[0]
	if got.Count != 3 || got.Ok != 2 || got.Err != 1 {
		t.Fatalf("计数口径应先对上（3 条 2 成功 1 失败），实际 %+v", got)
	}
	// 缓存两列**刻意不过滤 status**，与 /stats 的 cache_hit_rate 逐字同口径：
	// 前缀缓存衡量的是「这次请求的输入有多少是命中的」，与这次请求最后
	// 成功与否无关 —— 失败请求同样消耗了（可能被缓存的）输入。
	// 三条记录合计：cached 800+200+5000 = 6000，input 1000+1000+5000 = 7000。
	if got.CachedTokens != 6000 {
		t.Fatalf("缓存 token 必须与 /stats 同口径累加（含失败请求的输入侧），期望 6000，实际 %d", got.CachedTokens)
	}
	if want := 6000.0 / 7000.0; got.CacheHitRate < want-1e-9 || got.CacheHitRate > want+1e-9 {
		t.Fatalf("命中率必须是 cached/input = 6000/7000 ≈ %.6f，实际 %v", want, got.CacheHitRate)
	}
	// 生成耗时反过来**只算成功且测到首字的**那两条：(1000+3000)/2 = 2000。
	// 这一处分母与上面两列不同是刻意的：失败请求没有「首字之后」的时长。
	if got.AvgGenMs != 2000 || got.NGen != 2 {
		t.Fatalf("生成耗时必须是 avg=2000 n=2，实际 avg=%v n=%d", got.AvgGenMs, got.NGen)
	}
	if wantKey := time.UnixMilli(dayStart).Format("2006-01-02"); got.Key != wantKey {
		t.Fatalf("分组键必须是本地日期 %s，实际 %s", wantKey, got.Key)
	}
}
