package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cn-maul/rosetta-gateway/internal/store"
)

// TestStats_LifetimeIsServedToAdmin 钉住 F15 的修复。
//
// usage_totals（表 B）连同它的触发器、backfill 与启动 reconcile 一直在被
// 正确维护，但 GetUsageLifetime 全仓**零非测试调用点** —— 属于「没有执行点
// 的列」。现在它有了执行点：管理员显式要终身视角（include_lifetime=true）时
// 才查，且因为表 B 是不分用户的全局单行，**必须**限管理员。
func TestStats_LifetimeIsServedToAdmin(t *testing.T) {
	st := newScopeStore(t)
	seedTwoUsers(t, st)
	seedUsageRow(t, st, "e1", "k1", "u1", 100)
	seedUsageRow(t, st, "e2", "k1", "u1", 50)

	h := NewStatsHandler(st)

	// 管理员显式要终身数据 -> 带 lifetime 字段
	rec := httptest.NewRecorder()
	h.Get(rec, asAdmin(httptest.NewRequest(http.MethodGet, "/admin/api/stats?include_lifetime=true", nil)))
	if rec.Code != http.StatusOK {
		t.Fatalf("admin stats code=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		TotalRequests int64 `json:"total_requests"`
		Lifetime      *struct {
			RequestCount int64   `json:"request_count"`
			TotalTokens  int64   `json:"total_tokens"`
			CostTotal    float64 `json:"cost_total"`
		} `json:"lifetime"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, rec.Body.String())
	}
	if resp.Lifetime == nil {
		t.Fatalf("管理员显式请求终身数据却没有返回 lifetime 字段：%s", rec.Body.String())
	}
	if resp.Lifetime.RequestCount != 2 {
		t.Fatalf("lifetime.request_count=%d，期望 2（触发器已按写入累加）",
			resp.Lifetime.RequestCount)
	}
	if resp.Lifetime.TotalTokens != 150 {
		t.Fatalf("lifetime.total_tokens=%d，期望 150", resp.Lifetime.TotalTokens)
	}

	// 不带 include_lifetime -> 字段缺席（前端要能区分两种形态）
	rec2 := httptest.NewRecorder()
	h.Get(rec2, asAdmin(httptest.NewRequest(http.MethodGet, "/admin/api/stats", nil)))
	if rec2.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec2.Code, rec2.Body.String())
	}
	if containsJSONKey(rec2.Body.Bytes(), "lifetime") {
		t.Fatalf("未显式请求时不该返回 lifetime 字段：%s", rec2.Body.String())
	}

	// 普通用户即使传 include_lifetime=true 也不给（表 B 是全局单行，
	// 给了就是别人的数字）
	rec3 := httptest.NewRecorder()
	h.Get(rec3, asUser(httptest.NewRequest(http.MethodGet, "/admin/api/stats?include_lifetime=true", nil), "u1"))
	if rec3.Code != http.StatusOK {
		t.Fatalf("user stats code=%d body=%s", rec3.Code, rec3.Body.String())
	}
	if containsJSONKey(rec3.Body.Bytes(), "lifetime") {
		t.Fatalf("普通用户拿到了全局终身数据（跨租户泄露）：%s", rec3.Body.String())
	}
}

func containsJSONKey(body []byte, key string) bool {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return false
	}
	_, ok := m[key]
	return ok
}

var _ = store.DefaultRetentionDays
