package admin

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/cn-maul/rosetta-gateway/internal/store"
)

// 回归测试：GroupByKey 与其它 by-* 端点共用 groupBy，过滤条件统一由
// store.UsageSource 下发（时间 + 作用域），JOIN access_keys 只取展示名。
// 曾因调用方 SQL 自带 WHERE 又被拼接一个 WHERE 而报
// `SQL logic error: near "WHERE"`（usage group by named 500）。
// 本测试验证带 / 不带 from/to 参数都能正常返回 200 + 分组结果。
func TestUsageGroupByKey_NoDoubleWhere(t *testing.T) {
	st := newTestStore(t)
	h := NewUsageHandler(st)
	ctx := t.Context()

	// 造一条访问密钥与两条用量记录（不同 key，便于验证分组与时间过滤）。
	keyA := &store.AccessKey{ID: "kA", KeyHash: "hashA", KeyPrefix: "sk-gw-a...", Name: "Alpha", Enabled: true}
	keyB := &store.AccessKey{ID: "kB", KeyHash: "hashB", KeyPrefix: "sk-gw-b...", Name: "Beta", Enabled: true}
	if err := st.CreateAccessKey(ctx, keyA); err != nil {
		t.Fatalf("create keyA: %v", err)
	}
	if err := st.CreateAccessKey(ctx, keyB); err != nil {
		t.Fatalf("create keyB: %v", err)
	}

	now := time.Now().UnixMilli()
	recs := []*store.UsageRecord{
		{ID: "u1", Ts: now - 3600_000, AccessKeyID: "kA", PublicModel: "m", ProviderID: "p1", UpstreamModel: "x", TotalTokens: 100, Status: "ok", HTTPStatus: 200},
		{ID: "u2", Ts: now - 3600_000, AccessKeyID: "kB", PublicModel: "m", ProviderID: "p1", UpstreamModel: "x", TotalTokens: 200, Status: "ok", HTTPStatus: 200},
	}
	for _, r := range recs {
		if err := st.CreateUsageRecord(ctx, r); err != nil {
			t.Fatalf("create usage: %v", err)
		}
	}

	cases := []struct {
		name string
		url  string
		want int // 期望分组数
	}{
		{"default window", "/admin/api/usage/by-key", 2},
		{"explicit range all", "/admin/api/usage/by-key?from=0", 2},
		{"explicit narrow range", "/admin/api/usage/by-key?from=" + strconv.FormatInt(now-600_000, 10), 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, tc.url, nil)
			h.GroupByKey(rec, asAdmin(req))
			if rec.Code != http.StatusOK {
				t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
			}
			var entries []usageGroupEntry
			if err := json.Unmarshal(rec.Body.Bytes(), &entries); err != nil {
				t.Fatalf("unmarshal: %v body=%s", err, rec.Body.String())
			}
			if len(entries) != tc.want {
				t.Fatalf("want %d groups, got %d: %+v", tc.want, len(entries), entries)
			}
		})
	}
}

// TestUsageGroupBy_AfterPrune 守住「剪枝后 by-* 端点仍然出数」。
//
// 总览页的四个 by-*（by-day 趋势 / by-model / by-key / by-provider）是运维判断
// 流量分布的依据。它们改走 store.UsageSource 后若拼错，症状是**端点 500 或
// 静默变空** —— 而这类缺陷在「还没剪过枝」的库上完全看不出来，
// 因为归档那一支恒为空。所以这条用例必须先剪枝再断言。
//
// 顺带守住 by-day 的趋势起点：合并来源后最早的一天来自表 A，
// 剪枝不该让图上凭空少掉保留窗口以前的历史（设计 §4.8「趋势起点」一节）。
func TestUsageGroupBy_AfterPrune(t *testing.T) {
	st := newTestStore(t)
	h := NewUsageHandler(st)
	ctx := t.Context()

	// 两条记录：一条在保留窗口外（会被剪进表 A），一条在窗口内（留在明细）。
	old := time.Now().AddDate(0, 0, -(store.DefaultRetentionDays + 5)).UnixMilli()
	recent := time.Now().Add(-time.Hour).UnixMilli()
	for i, ts := range []int64{old, recent} {
		if err := st.CreateUsageRecord(ctx, &store.UsageRecord{
			ID:              fmt.Sprintf("u%d", i),
			Ts:              ts,
			AccessKeyID:     "kA",
			PublicModel:     "m",
			ProviderID:      "p1",
			UpstreamModel:   "x",
			IngressProtocol: "openai-chat",
			InputTokens:     10,
			OutputTokens:    5,
			TotalTokens:     15,
			Status:          "ok",
			HTTPStatus:      200,
		}); err != nil {
			t.Fatalf("create usage: %v", err)
		}
	}

	// 「全部历史」档：from=0 且显式传参。
	groups := map[string]func(http.ResponseWriter, *http.Request){
		"by-day":      h.GroupByDay,
		"by-model":    h.GroupByModel,
		"by-key":      h.GroupByKey,
		"by-provider": h.GroupByProvider,
	}
	before := make(map[string]int)
	for dim, fn := range groups {
		n := callGroup(t, fn, "/admin/api/usage/"+dim+"?from=0")
		before[dim] = n
		if n == 0 {
			t.Fatalf("%s returned 0 groups before pruning", dim)
		}
	}

	if _, err := st.PruneOldUsage(ctx, store.DefaultRetentionDays); err != nil {
		t.Fatalf("prune: %v", err)
	}

	for dim, fn := range groups {
		after := callGroup(t, fn, "/admin/api/usage/"+dim+"?from=0")
		if after == 0 {
			t.Fatalf("%s returned no groups after pruning (archive branch not wired)", dim)
		}
		if after != before[dim] {
			t.Errorf("%s group count changed across prune: %d -> %d", dim, before[dim], after)
		}
	}
}

// callGroup 请求一个 by-* 端点并返回分组数；非 200 直接失败并带上响应体。
func callGroup(t *testing.T, fn func(http.ResponseWriter, *http.Request), url string) int {
	t.Helper()
	rec := httptest.NewRecorder()
	fn(rec, asAdmin(httptest.NewRequest(http.MethodGet, url, nil)))
	if rec.Code != http.StatusOK {
		t.Fatalf("%s code=%d body=%s", url, rec.Code, rec.Body.String())
	}
	var entries []usageGroupEntry
	if err := json.Unmarshal(rec.Body.Bytes(), &entries); err != nil {
		t.Fatalf("%s unmarshal: %v body=%s", url, err, rec.Body.String())
	}
	return len(entries)
}

// TestUsageGroupBy_ScopeNarrowsArchive 守住「归档那一支也要按用户收窄」。
//
// 这是本次改造最严重的潜在缺陷面：UsageSource 有明细与归档**两支**，
// 作用域收窄任何一支漏掉，普通用户就能读到别人的数据。
// 而漏掉归档支在「还没剪过枝」的库上完全测不出来（那一支恒为空），
// 所以这条用例必须先剪枝 —— 剪完后 by-* 的数据只可能来自表 A。
//
// 白名单前缀只保证「能进来」，不保证「只看到自己的」，所以这条断言不能省。
func TestUsageGroupBy_ScopeNarrowsArchive(t *testing.T) {
	st := newTestStore(t)
	h := NewUsageHandler(st)
	ctx := t.Context()

	// 两个用户各一把 key，各一条记录，都在保留窗口外（剪枝后进表 A）。
	old := time.Now().AddDate(0, 0, -(store.DefaultRetentionDays + 5)).UnixMilli()
	for i, u := range []string{"u1", "u2"} {
		if err := st.CreateUsageRecord(ctx, &store.UsageRecord{
			ID:              "rec-" + u,
			Ts:              old,
			UserID:          u,
			AccessKeyID:     "k-" + u,
			PublicModel:     "m",
			ProviderID:      "p1",
			UpstreamModel:   "x",
			IngressProtocol: "openai-chat",
			InputTokens:     100 * int64(i+1),
			OutputTokens:    10,
			TotalTokens:     100 * int64(i+1),
			Status:          "ok",
			HTTPStatus:      200,
		}); err != nil {
			t.Fatalf("create usage: %v", err)
		}
	}
	if _, err := st.PruneOldUsage(ctx, store.DefaultRetentionDays); err != nil {
		t.Fatalf("prune: %v", err)
	}

	// 此时 by-* 的数据只可能来自归档（明细里的老记录已被删）。
	rec := httptest.NewRecorder()
	h.GroupByKey(rec, asUser(httptest.NewRequest(http.MethodGet, "/admin/api/usage/by-key?from=0", nil), "u1"))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var entries []usageGroupEntry
	if err := json.Unmarshal(rec.Body.Bytes(), &entries); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, rec.Body.String())
	}
	if len(entries) != 1 {
		t.Fatalf("u1 should see exactly 1 group, got %d: %+v", len(entries), entries)
	}
	if entries[0].Key != "k-u1" {
		t.Errorf("u1 saw another user's key: %q", entries[0].Key)
	}
	if entries[0].Tokens != 100 {
		t.Errorf("tokens = %d, want 100 (u1's own usage)", entries[0].Tokens)
	}
}

// 调用历史分页：跨页不得重复、不得漏行，且 total 是**过滤后的总条数**
// （不是当页条数）。
//
// 刻意让所有记录的 ts 完全相同：只按 `ts DESC` 排序时同值行的相对顺序不确定，
// 配上 OFFSET 分页就会出现「某条在两页里各出现一次、另一条被整个跳过」。
// 查询里用 `u.id DESC` 做 tiebreaker 才能保证稳定。为能逐页核对，这里给每条
// 记录一个唯一的 total_tokens 当标签，最后断言恰好覆盖 {1..N} 各一次。
func TestUsageHistory_PaginationNoDupNoSkip(t *testing.T) {
	st := newTestStore(t)
	h := NewUsageHandler(st)
	ctx := t.Context()

	if err := st.CreateAccessKey(ctx, &store.AccessKey{
		ID: "kA", KeyHash: "hashA", KeyPrefix: "sk-gw-a...", Name: "A", Enabled: true,
	}); err != nil {
		t.Fatalf("seed key: %v", err)
	}

	const n = 7
	ts := time.Now().UnixMilli()
	for i := 1; i <= n; i++ {
		if err := st.CreateUsageRecord(ctx, &store.UsageRecord{
			ID:              fmt.Sprintf("u%02d", i),
			Ts:              ts, // 全部相同：专治「只按 ts 排序」的不确定性
			AccessKeyID:     "kA",
			PublicModel:     "m",
			ProviderID:      "p1",
			UpstreamModel:   "x",
			TotalTokens:     int64(i), // 唯一标签
			Status:          "ok",
			HTTPStatus:      http.StatusOK,
			IngressProtocol: "openai-chat",
			UsageState:      "reported",
		}); err != nil {
			t.Fatalf("seed usage %d: %v", i, err)
		}
	}

	const limit = 3
	seen := map[int64]int{}
	pages := 0
	for offset := 0; ; offset += limit {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet,
			fmt.Sprintf("/admin/api/usage/history?from=0&to=%d&limit=%d&offset=%d", ts+1, limit, offset), nil)
		h.History(rec, asAdmin(req))
		if rec.Code != http.StatusOK {
			t.Fatalf("offset=%d code=%d body=%s", offset, rec.Code, rec.Body.String())
		}
		var resp usageHistoryResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("unmarshal: %v body=%s", err, rec.Body.String())
		}
		// total 不受分页影响：每一页都应报同一份总数。
		if resp.Total != n {
			t.Fatalf("offset=%d total=%d, want %d", offset, resp.Total, n)
		}
		if len(resp.Records) == 0 {
			break
		}
		for _, r := range resp.Records {
			seen[r.TotalTokens]++
		}
		pages++
		if pages > n {
			t.Fatal("pagination did not terminate")
		}
	}

	if pages != 3 { // 7 条 / 每页 3 → 3 页
		t.Fatalf("want 3 pages, got %d", pages)
	}
	if len(seen) != n {
		t.Fatalf("saw %d distinct rows, want %d (%v)", len(seen), n, seen)
	}
	for tok, c := range seen {
		if c != 1 {
			t.Fatalf("row with tokens=%d appeared %d times across pages (期望恰好一次)", tok, c)
		}
	}
}

// 非法/越界分页参数不应 500：负数 offset 按首页处理，limit 超上限被钳制。
func TestUsageHistory_ClampsPagingParams(t *testing.T) {
	st := newTestStore(t)
	h := NewUsageHandler(st)

	for _, q := range []string{
		"limit=-5&offset=-3",
		"limit=999999&offset=100000",
		"limit=abc&offset=xyz",
	} {
		rec := httptest.NewRecorder()
		h.History(rec, asAdmin(httptest.NewRequest(http.MethodGet, "/admin/api/usage/history?"+q, nil)))
		if rec.Code != http.StatusOK {
			t.Fatalf("q=%q code=%d body=%s", q, rec.Code, rec.Body.String())
		}
		var resp usageHistoryResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("q=%q unmarshal: %v", q, err)
		}
		if resp.Records == nil {
			t.Fatalf("q=%q records 必须是 [] 而不是 null", q)
		}
	}
}

// TestExportCSV_ReportsTruncation 守住「被截断的证据必须看起来被截断」。
//
// CSV 导出有一次行数上限（maxUsageLimit）。静默截断在排障场景里是最贵的一种
// bug：界面显示「共 12 万条」、表格能翻到第 120 页，而导出的文件里只有 1000 行 ——
// 于是「上游只失败过 1000 次」这个结论看起来有了数据支撑，实际上那份数据是被
// 截断的取证。命中上限时必须发 X-Export-Truncated 与真实总行数。
func TestExportCSV_ReportsTruncation(t *testing.T) {
	st := newTestStore(t)
	h := NewUsageHandler(st)
	ctx := t.Context()

	// 造 maxUsageLimit+1 条：刚好越过上限，截断与「差一点就满了」必须可区分。
	for i := 0; i < maxUsageLimit+1; i++ {
		rec := &store.UsageRecord{
			ID: fmt.Sprintf("u%04d", i), Ts: time.Now().UnixMilli() - int64(i),
			PublicModel: "m", ProviderID: "p1", UpstreamModel: "x",
			TotalTokens: int64(i), Status: "ok", HTTPStatus: 200,
		}
		if err := st.CreateUsageRecord(ctx, rec); err != nil {
			t.Fatalf("create usage %d: %v", i, err)
		}
	}

	rec := httptest.NewRecorder()
	h.ExportCSV(rec, asAdmin(httptest.NewRequest(http.MethodGet, "/admin/api/usage/history.csv", nil)))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Export-Truncated"); got != "1" {
		t.Errorf("命中行数上限时 X-Export-Truncated=%q, want 1", got)
	}
	if got := rec.Header().Get("X-Export-Total"); got != strconv.Itoa(maxUsageLimit+1) {
		t.Errorf("X-Export-Total=%q, want %d（必须报真实总行数，不能报导出行数）", got, maxUsageLimit+1)
	}

	// 未截断时不得带这个头：否则界面上会出现「已截断」的字样而数据其实是全的。
	st2 := newTestStore(t)
	h2 := NewUsageHandler(st2)
	if err := st2.CreateUsageRecord(ctx, &store.UsageRecord{
		ID: "u1", Ts: time.Now().UnixMilli(), PublicModel: "m",
		ProviderID: "p1", UpstreamModel: "x", TotalTokens: 1, Status: "ok", HTTPStatus: 200,
	}); err != nil {
		t.Fatalf("create usage: %v", err)
	}
	rec2 := httptest.NewRecorder()
	h2.ExportCSV(rec2, asAdmin(httptest.NewRequest(http.MethodGet, "/admin/api/usage/history.csv", nil)))
	if rec2.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec2.Code, rec2.Body.String())
	}
	if got := rec2.Header().Get("X-Export-Truncated"); got != "" {
		t.Errorf("未截断时不得写 X-Export-Truncated，实际 %q", got)
	}
	if got := rec2.Header().Get("X-Export-Total"); got != "1" {
		t.Errorf("X-Export-Total=%q, want 1", got)
	}
}
