package admin

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cn-maul/rosetta-gateway/internal/store"
)

// 2026-10-10 新增字段：调用历史要能区分流式/非流式，并显示每条的费用。
//
// 两者都是为了让用户能解释「钱花在哪」：
//   - stream：首字时间在流式与非流式下含义完全不同，混在一列无法解读；
//   - cost：单价低时单次费用不足一分，余额按分扣减会长时间不动，
//     看不到每条费用就会以为扣费坏了。

func seedHistoryRows(t *testing.T, st *store.Store) int64 {
	t.Helper()
	ctx := t.Context()
	if err := st.CreateAccessKey(ctx, &store.AccessKey{
		ID: "kA", KeyHash: "hashA", KeyPrefix: "sk-a", Name: "A", Enabled: true,
	}); err != nil {
		t.Fatalf("seed key: %v", err)
	}
	ts := time.Now().UnixMilli()
	// 一条流式、一条非流式，且费用都刻意取「不足一分」的真实量级。
	rows := []struct {
		id     string
		stream bool
		cost   float64
	}{
		{"s1", true, 0.004986},
		{"n1", false, 0.123456},
	}
	for _, r := range rows {
		if err := st.CreateUsageRecord(ctx, &store.UsageRecord{
			ID: r.id, Ts: ts, AccessKeyID: "kA", PublicModel: "code",
			ProviderID: "p1", UpstreamModel: "cn:glm", Stream: r.stream,
			TotalTokens: 100, Status: "ok", HTTPStatus: http.StatusOK,
			IngressProtocol: "openai-chat", UsageState: "reported",
		}); err != nil {
			t.Fatalf("seed %s: %v", r.id, err)
		}
		// cost_total 走冻结计价；这里直接写固化值更贴近真实链路。
		if _, err := st.DB().Exec(`UPDATE usage_records SET cost_total = ? WHERE id = ?`,
			r.cost, r.id); err != nil {
			t.Fatalf("set cost %s: %v", r.id, err)
		}
	}
	return ts
}

func TestUsage_History_ExposesStreamAndCost(t *testing.T) {
	st := newScopeStore(t)
	ts := seedHistoryRows(t, st)
	h := NewUsageHandler(st)

	rec := httptest.NewRecorder()
	req := asAdmin(httptest.NewRequest(http.MethodGet,
		fmt.Sprintf("/admin/api/usage/history?from=0&to=%d&limit=100", ts+1), nil))
	h.History(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}

	var resp usageHistoryResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, rec.Body.String())
	}
	if len(resp.Records) != 2 {
		t.Fatalf("records=%d, want 2", len(resp.Records))
	}

	byStream := map[bool]float64{}
	for _, r := range resp.Records {
		byStream[r.Stream] = r.Cost
	}
	// 流式那条必须是 true 且带上费用，非流式那条必须是 false。
	if c, ok := byStream[true]; !ok || c < 0.004 {
		t.Errorf("流式记录缺失或费用不对：stream=true 的 cost=%v", c)
	}
	if c, ok := byStream[false]; !ok || c < 0.123 {
		t.Errorf("非流式记录缺失或费用不对：stream=false 的 cost=%v", c)
	}

	// 费用必须原样透传（不被取整成 0）—— 那正是本次要修的症状。
	for _, r := range resp.Records {
		if r.Cost <= 0 {
			t.Errorf("记录 %s 的 cost=%v —— 单次不足一分时费用不应显示为 0", r.PublicModel, r.Cost)
		}
	}
}

// CSV 导出必须与页面表格同字段，否则拿出去的文件无法独立解读。
func TestUsage_ExportCSV_IncludesStreamAndCost(t *testing.T) {
	st := newScopeStore(t)
	ts := seedHistoryRows(t, st)
	h := NewUsageHandler(st)

	rec := httptest.NewRecorder()
	req := asAdmin(httptest.NewRequest(http.MethodPost,
		fmt.Sprintf("/admin/api/usage/history.csv?from=0&to=%d", ts+1), nil))
	h.ExportCSV(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	header := strings.SplitN(body, "\n", 2)[0]
	for _, col := range []string{"stream", "cost"} {
		if !strings.Contains(header, col) {
			t.Errorf("CSV 表头缺 %q 列：%s", col, header)
		}
	}
	// 数据行也要有这两列的值。
	if !strings.Contains(body, "true") || !strings.Contains(body, "false") {
		t.Errorf("CSV 数据行缺流式标记：%s", body)
	}
}
