package admin

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cn-maul/rosetta-gateway/internal/store"
)

// seedPriceModels 建一个 provider 下多个模型，返回它们的 id。
func seedPriceModels(t *testing.T, st *store.Store, n int) []string {
	t.Helper()
	ids := make([]string, 0, n)
	for i := 0; i < n; i++ {
		id := "m" + string(rune('a'+i))
		if err := st.CreateUpstreamModel(t.Context(), &store.UpstreamModel{
			ID: id, ProviderID: "p1", ModelID: "model-" + id, Enabled: true,
		}); err != nil {
			t.Fatalf("seed model %s: %v", id, err)
		}
		ids = append(ids, id)
	}
	return ids
}

// batchPrice 走真实 HTTP 入口批量设置价格，返回响应码与 body。
func batchPrice(t *testing.T, h *ModelHandler, body string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	req := asAdmin(jsonRequest(http.MethodPost, "/admin/api/models/batch-price", bytes.NewBufferString(body)))
	rec := httptest.NewRecorder()
	h.BatchUpdatePrices(rec, req)
	var resp map[string]any
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("unmarshal: %v body=%s", err, rec.Body.String())
		}
	}
	return rec, resp
}

// TestBatchPrice_AppliesToAll 核心：批量设置后，每个目标模型的库里价格都对。
//
// 断言直接查 store 而不是只看响应体 —— 这条链路最容易出的 bug 是
// 「响应说成功、库里没写」（与单条 PriceRoundTrip 同理由）。
func TestBatchPrice_AppliesToAll(t *testing.T) {
	st := newTestStore(t)
	h := NewModelHandler(st, nil, nil)
	ids := seedPriceModels(t, st, 3)

	rec, resp := batchPrice(t, h, `{"model_ids":["ma","mb","mc"],"price_input":1.5,"price_cache_hit":0.15,"price_output":6}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("batch: code=%d body=%s", rec.Code, rec.Body.String())
	}
	if resp["updated"].(float64) != 3 {
		t.Fatalf("updated=%v 期望 3", resp["updated"])
	}

	for _, id := range ids {
		m, err := st.GetUpstreamModel(t.Context(), id)
		if err != nil || m == nil {
			t.Fatalf("get %s: %v", id, err)
		}
		if m.PriceInput != 1.5 || m.PriceCacheHit != 0.15 || m.PriceOutput != 6 {
			t.Fatalf("%s 价格未写入: %+v", id, m)
		}
	}
}

// TestBatchPrice_ZeroClears 批量传 0 = 清除价格（落 NULL），与单条路径同语义。
func TestBatchPrice_ZeroClears(t *testing.T) {
	st := newTestStore(t)
	h := NewModelHandler(st, nil, nil)
	ids := seedPriceModels(t, st, 2)

	// 先设价
	if rec, _ := batchPrice(t, h, `{"model_ids":["ma","mb"],"price_input":1,"price_cache_hit":0.1,"price_output":2}`); rec.Code != http.StatusOK {
		t.Fatalf("set: code=%d", rec.Code)
	}
	// 再清空
	if rec, _ := batchPrice(t, h, `{"model_ids":["ma","mb"],"price_input":0,"price_cache_hit":0,"price_output":0}`); rec.Code != http.StatusOK {
		t.Fatalf("clear: code=%d", rec.Code)
	}

	for _, id := range ids {
		m, _ := st.GetUpstreamModel(t.Context(), id)
		if m.PriceInput != 0 || m.PriceCacheHit != 0 || m.PriceOutput != 0 {
			t.Fatalf("%s 清除后价格残留: %+v", id, m)
		}
	}
}

// TestBatchPrice_NegativeRejected 负数必须 400，且不写任何模型。
func TestBatchPrice_NegativeRejected(t *testing.T) {
	st := newTestStore(t)
	h := NewModelHandler(st, nil, nil)
	seedPriceModels(t, st, 2)

	rec, _ := batchPrice(t, h, `{"model_ids":["ma","mb"],"price_input":-1,"price_cache_hit":0,"price_output":0}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("negative: code=%d 期望 400", rec.Code)
	}
	// 未写入
	m, _ := st.GetUpstreamModel(t.Context(), "ma")
	if m.PriceInput != 0 {
		t.Fatalf("负数请求仍写入了价格: %+v", m)
	}
}

// TestBatchPrice_MissingModelReports 传了不存在的 id 必须报错并列出，不能静默跳过。
//
// 静默跳过会让运维以为全改好了、实则漏了 —— 那正是本功能要消灭的
// 「配了但没生效」类故障。
func TestBatchPrice_MissingModelReports(t *testing.T) {
	st := newTestStore(t)
	h := NewModelHandler(st, nil, nil)
	seedPriceModels(t, st, 1)

	rec, _ := batchPrice(t, h, `{"model_ids":["ma","ghost"],"price_input":1,"price_cache_hit":0,"price_output":2}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing: code=%d 期望 404", rec.Code)
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte("ghost")) {
		t.Fatalf("报错应列出缺失的 id: %s", rec.Body.String())
	}
	// 部分写入也不允许：ma 不该被写（整体替换语义）
	m, _ := st.GetUpstreamModel(t.Context(), "ma")
	if m.PriceInput != 0 {
		t.Fatalf("部分失败时 ma 仍被写入: %+v", m)
	}
}

// TestBatchPrice_EmptyIds 空 ids 必须 400。
func TestBatchPrice_EmptyIds(t *testing.T) {
	st := newTestStore(t)
	h := NewModelHandler(st, nil, nil)

	rec, _ := batchPrice(t, h, `{"model_ids":[],"price_input":1,"price_cache_hit":0,"price_output":2}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("empty ids: code=%d 期望 400", rec.Code)
	}
}

// TestBatchPrice_Dedup 重复 id 只算一次，updated 数正确。
func TestBatchPrice_Dedup(t *testing.T) {
	st := newTestStore(t)
	h := NewModelHandler(st, nil, nil)
	seedPriceModels(t, st, 1)

	rec, resp := batchPrice(t, h, `{"model_ids":["ma","ma","ma"],"price_input":1,"price_cache_hit":0,"price_output":2}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("dedup: code=%d body=%s", rec.Code, rec.Body.String())
	}
	if resp["updated"].(float64) != 1 {
		t.Fatalf("updated=%v 期望 1（去重后）", resp["updated"])
	}
}
