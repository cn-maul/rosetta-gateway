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

// 回归测试：GroupByKey 走 groupByNamed，查询 JOIN 了 usage_records u。
// 曾因调用方 SQL 自带 WHERE 又被 groupByNamed 拼接一个 WHERE 而报
// `SQL logic error: near "WHERE"`（usage group by named 500）。
// 现在时间过滤统一由 groupByNamed 用 groupRangeClause 拼接（u. 前缀），
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
			h.GroupByKey(rec, req)
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
		h.History(rec, req)
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
		h.History(rec, httptest.NewRequest(http.MethodGet, "/admin/api/usage/history?"+q, nil))
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