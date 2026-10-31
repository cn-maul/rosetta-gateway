package admin

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cn-maul/rosetta-gateway/internal/store"
)

// 2026-10-31：界面不再暴露「缓存写入」价格，两条链路必须各自守住自己的语义：
//
//   - 批量改价（整体替换三档）**不许**顺手把别人配过的缓存写入价清成 0；
//   - 手动添加模型能直接带价入库（含缓存写入价这一列，别在 INSERT 里漏掉）。
//
// 两条都属于「静默丢配置」类缺陷：接口回 200、界面看不出异常，等到月底对账
// 才发现某项单价是 0。

// seedCacheCreationModel 建一个带缓存写入价的模型，返回它的 id。
func seedCacheCreationModel(t *testing.T, st *store.Store) string {
	t.Helper()
	if err := st.CreateUpstreamModel(t.Context(), &store.UpstreamModel{
		ID: "cw1", ProviderID: "p1", ModelID: "cache-write-model",
		PriceInput: 2, PriceCacheHit: 0.2, PriceCacheCreation: 2.5, PriceOutput: 8,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	return "cw1"
}

// TestBatchPrice_LeavesCacheCreationAlone 批量改价不带 price_cache_creation 时，
// 已有值必须原样保留。
func TestBatchPrice_LeavesCacheCreationAlone(t *testing.T) {
	st := newTestStore(t)
	h := NewModelHandler(st, nil, nil)
	id := seedCacheCreationModel(t, st)

	rec, _ := batchPrice(t, h, `{"model_ids":["`+id+`"],"price_input":1,"price_cache_hit":0.1,"price_output":3}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("batch: code=%d body=%s", rec.Code, rec.Body.String())
	}

	m, err := st.GetUpstreamModel(t.Context(), id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if m.PriceInput != 1 || m.PriceCacheHit != 0.1 || m.PriceOutput != 3 {
		t.Fatalf("三档价格未按请求写入: %+v", m)
	}
	if m.PriceCacheCreation != 2.5 {
		t.Fatalf("缓存写入价被批量改价清掉了：实际 %v，期望 2.5", m.PriceCacheCreation)
	}
}

// TestBatchPrice_ExplicitCacheCreationOverwrites 显式带上该字段时仍然生效
// （接口没关死这条能力，只是界面上不暴露）。
func TestBatchPrice_ExplicitCacheCreationOverwrites(t *testing.T) {
	st := newTestStore(t)
	h := NewModelHandler(st, nil, nil)
	id := seedCacheCreationModel(t, st)

	rec, _ := batchPrice(t, h, `{"model_ids":["`+id+`"],"price_input":1,"price_cache_hit":0.1,"price_cache_creation":4,"price_output":3}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("batch: code=%d body=%s", rec.Code, rec.Body.String())
	}
	m, _ := st.GetUpstreamModel(t.Context(), id)
	if m.PriceCacheCreation != 4 {
		t.Fatalf("显式传入的缓存写入价没写入：实际 %v，期望 4", m.PriceCacheCreation)
	}
}

// TestBatchPrice_NegativeCacheCreationRejected 指针化之后，非负校验不能漏掉
// 这一档（漏掉就会把负价写进库）。
func TestBatchPrice_NegativeCacheCreationRejected(t *testing.T) {
	st := newTestStore(t)
	h := NewModelHandler(st, nil, nil)
	id := seedCacheCreationModel(t, st)

	rec, _ := batchPrice(t, h, `{"model_ids":["`+id+`"],"price_input":1,"price_cache_hit":0.1,"price_cache_creation":-1,"price_output":3}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("负数缓存写入价: code=%d 期望 400 body=%s", rec.Code, rec.Body.String())
	}
	m, _ := st.GetUpstreamModel(t.Context(), id)
	if m.PriceCacheCreation != 2.5 {
		t.Fatalf("负数被拒后原值被动过：实际 %v，期望 2.5", m.PriceCacheCreation)
	}
}

// TestImportModels_AcceptsPrices 手动添加模型可以直接带价（含缓存写入价）。
func TestImportModels_AcceptsPrices(t *testing.T) {
	st := newTestStore(t)
	h := NewModelHandler(st, nil, nil)

	body := `{"models":[{"model_id":"manual-model","display_name":"手工模型","context_window":128000,"max_output_tokens":8192,"price_input":3,"price_cache_hit":0.3,"price_output":9}]}`
	req := asAdmin(jsonRequest(http.MethodPost, "/admin/api/providers/p1/models/import", bytes.NewBufferString(body)))
	rec := httptest.NewRecorder()
	h.ImportModels(rec, req, "p1")
	if rec.Code != http.StatusOK {
		t.Fatalf("import: code=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, rec.Body.String())
	}
	if resp["imported"].(float64) != 1 {
		t.Fatalf("imported=%v 期望 1", resp["imported"])
	}

	models, err := st.ListUpstreamModels(t.Context(), "p1")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	m, ok := findByModelID(models, "manual-model")
	if !ok {
		t.Fatalf("导入的模型不存在，现有 %d 个", len(models))
	}
	if m.PriceInput != 3 || m.PriceCacheHit != 0.3 || m.PriceOutput != 9 {
		t.Fatalf("导入未写入价格: %+v", m)
	}
	// 缓存写入价没传 → 保持未配置（0），不是被填成输入价。
	if m.PriceCacheCreation != 0 {
		t.Fatalf("未传的缓存写入价不该被填值：实际 %v", m.PriceCacheCreation)
	}
}

// TestBatchPrice_ExplicitZeroCacheCreationClears 显式传 0 仍然表示「清除这一档」
// —— 指针化不能把这条意图也一起关掉。「不传 = 保持」与「传 0 = 清除」是两件事，
// 只留前者会让运维再也删不掉一个配错的缓存写入价。
func TestBatchPrice_ExplicitZeroCacheCreationClears(t *testing.T) {
	st := newTestStore(t)
	h := NewModelHandler(st, nil, nil)
	id := seedCacheCreationModel(t, st)

	rec, _ := batchPrice(t, h, `{"model_ids":["`+id+`"],"price_input":1,"price_cache_hit":0.1,"price_cache_creation":0,"price_output":3}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("batch: code=%d body=%s", rec.Code, rec.Body.String())
	}
	m, _ := st.GetUpstreamModel(t.Context(), id)
	if m.PriceCacheCreation != 0 {
		t.Fatalf("显式传 0 应清除缓存写入价：实际 %v", m.PriceCacheCreation)
	}
}

// findByModelID 在模型列表里按 model_id 找 —— newTestStore 已经给 p1 塞了
// 一个 alpha-chat，不能用「列表长度」断言导入结果。
func findByModelID(models []store.UpstreamModel, modelID string) (store.UpstreamModel, bool) {
	for _, m := range models {
		if m.ModelID == modelID {
			return m, true
		}
	}
	return store.UpstreamModel{}, false
}

// TestImportModels_ZeroPriceStaysUnconfigured 填 0 等于「不配价」，
// 落库必须是未配置（0/NULL），不能变成「确认免费」的 0 元条目。
func TestImportModels_ZeroPriceStaysUnconfigured(t *testing.T) {
	st := newTestStore(t)
	h := NewModelHandler(st, nil, nil)

	body := `{"models":[{"model_id":"free-form","price_input":0,"price_output":0}]}`
	req := asAdmin(jsonRequest(http.MethodPost, "/admin/api/providers/p1/models/import", bytes.NewBufferString(body)))
	rec := httptest.NewRecorder()
	h.ImportModels(rec, req, "p1")
	if rec.Code != http.StatusOK {
		t.Fatalf("import: code=%d body=%s", rec.Code, rec.Body.String())
	}
	models, _ := st.ListUpstreamModels(t.Context(), "p1")
	m, ok := findByModelID(models, "free-form")
	if !ok {
		t.Fatalf("导入的模型不存在，现有 %d 个", len(models))
	}
	if m.PriceInput != 0 || m.PriceOutput != 0 {
		t.Fatalf("填 0 应保持未配置: %+v", m)
	}
}
