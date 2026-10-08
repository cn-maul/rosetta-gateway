package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/cn-maul/rosetta-gateway/internal/store"
)

// 2026-10-10 回归：`/usage/history` 必须能区分「显式 from=0（=全部历史）」与
// 「未传 from（=最近 7 天）」。
//
// # 原来的 bug
//
// queryRange 里是 `if from == 0 { from = now - window }`，把**显式传的 0**
// 与「没传」混为一谈。而前端 range.ts 的「全部」档正是发 `from=0`
// （见 rangeStart：days<=0 → 0），语义注释还写着「后端把 from=0 解释为
// 不限起点」—— 这个假设只对 /stats 成立（它用 queryRangeExplicit），
// 对 history 是错的。
//
// 后果：**静默返回错误的数据**。用户选「全部」，实际拿到的是最近 7 天；
// 更糟的是当 7 天内没有记录时返回空表，界面显示「该时间范围内没有调用」，
// 而用户以为自己看的是全部历史。反过来，选「全部」但数据恰好都在 7 天内，
// 一切看起来正常 —— 这是最难发现的那类。
//
// 一度靠前端绕开（钱包页改用显式 30 天）。那是把 bug 藏起来，不是修它：
// History.vue 选「全部」时同样中招，且没有任何地方提示过。
func TestUsage_History_DistinguishesExplicitZeroFromMissing(t *testing.T) {
	st := newScopeStore(t)
	seedTwoUsers(t, st)

	// 造两条：一条 30 天前，一条 1 小时前。
	now := time.Now()
	old := now.Add(-30 * 24 * time.Hour).UnixMilli()
	recent := now.Add(-time.Hour).UnixMilli()
	for _, r := range []struct {
		id string
		ts int64
	}{{"old", old}, {"recent", recent}} {
		if err := st.CreateUsageRecord(context.Background(), &store.UsageRecord{
			ID: r.id, Ts: r.ts, UserID: "u1", AccessKeyID: "k1", PublicModel: "m",
			ProviderID: "p1", UpstreamModel: "up", TotalTokens: 10, Status: "ok",
			HTTPStatus: 200, IngressProtocol: "openai-chat", UsageState: "reported",
		}); err != nil {
			t.Fatalf("seed %s: %v", r.id, err)
		}
	}
	h := NewUsageHandler(st)

	get := func(q string) int {
		rec := httptest.NewRecorder()
		h.History(rec, asAdmin(httptest.NewRequest(http.MethodGet, "/admin/api/usage/history"+q, nil)))
		if rec.Code != http.StatusOK {
			t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
		}
		var resp usageHistoryResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		return len(resp.Records)
	}

	// from=0 显式传：前端「全部」档就是这么发的 → 必须拿到全部 2 条。
	if got := get(fmt.Sprintf("?from=0&to=%d&limit=100", now.UnixMilli())); got != 2 {
		t.Errorf("from=0（全部历史）返回 %d 条, want 2 —— "+
			"显式 0 被当成了「没传」而回落到默认窗口", got)
	}

	// 未传 from：默认窗口（7 天）→ 只有 1 条。
	if got := get(fmt.Sprintf("?to=%d&limit=100", now.UnixMilli())); got != 1 {
		t.Errorf("未传 from（默认 7 天）返回 %d 条, want 1", got)
	}

	// 显式窄区间：from 设成 29 天前 → 只有 1 条。
	narrow := now.Add(-29 * 24 * time.Hour).UnixMilli()
	if got := get(fmt.Sprintf("?from=%d&to=%d&limit=100", narrow, now.UnixMilli())); got != 1 {
		t.Errorf("显式 29 天窗口返回 %d 条, want 1", got)
	}
}

// CSV 导出与列表共用同一个 where，必须同样区分显式 0 —— 否则「导出全部」
// 导出的文件与页面上看到的不是同一批记录，而这种不一致对账时才被发现。
func TestUsage_ExportCSV_DistinguishesExplicitZeroFromMissing(t *testing.T) {
	st := newScopeStore(t)
	seedTwoUsers(t, st)
	now := time.Now()
	old := now.Add(-30 * 24 * time.Hour).UnixMilli()
	if err := st.CreateUsageRecord(context.Background(), &store.UsageRecord{
		ID: "old", Ts: old, UserID: "u1", AccessKeyID: "k1", PublicModel: "m",
		ProviderID: "p1", UpstreamModel: "up", TotalTokens: 10, Status: "ok",
		HTTPStatus: 200, IngressProtocol: "openai-chat", UsageState: "reported",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	h := NewUsageHandler(st)

	rec := httptest.NewRecorder()
	h.ExportCSV(rec, asAdmin(httptest.NewRequest(http.MethodPost,
		fmt.Sprintf("/admin/api/usage/history.csv?from=0&to=%d", now.UnixMilli()), nil)))

	// 表头 + 1 行数据；空导出只有表头。
	lines := 0
	for _, b := range rec.Body.Bytes() {
		if b == '\n' {
			lines++
		}
	}
	if lines < 2 {
		t.Errorf("from=0 导出只有 %d 行（应含表头 + 30 天前那条）：%s", lines, rec.Body.String())
	}
}
