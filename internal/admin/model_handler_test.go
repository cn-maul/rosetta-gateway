package admin

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// 单价（元/百万 tokens）的 PATCH 语义与 context_window 一致：
//   - 未出现的字段保持原值；
//   - 显式 0 = 清空（不计费）；
//   - 负数 = 400。
//
// 这条链路最容易出的 bug 是「改价只在响应里对、库里没写」——
// 所以断言直接查 store，而不是只看 handler 返回体。
func TestModelHandler_PriceRoundTrip(t *testing.T) {
	st := newTestStore(t)
	h := NewModelHandler(st, nil, nil)

	patch := func(body string) (*httptest.ResponseRecorder, modelResponse) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPatch, "/admin/api/models/m1", bytes.NewBufferString(body))
		rec := httptest.NewRecorder()
		h.Update(rec, req, "m1")
		var resp modelResponse
		if rec.Code == http.StatusOK {
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
		}
		return rec, resp
	}

	// 设价
	rec, resp := patch(`{"price_input":0.2,"price_cache_hit":0.02,"price_output":0.8}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("set prices: code=%d body=%s", rec.Code, rec.Body.String())
	}
	if resp.PriceInput != 0.2 || resp.PriceCacheHit != 0.02 || resp.PriceOutput != 0.8 {
		t.Fatalf("response prices wrong: %+v", resp)
	}
	m, err := st.GetUpstreamModel(t.Context(), "m1")
	if err != nil || m == nil {
		t.Fatalf("get: %v %v", m, err)
	}
	if m.PriceInput != 0.2 || m.PriceCacheHit != 0.02 || m.PriceOutput != 0.8 {
		t.Fatalf("prices not persisted: %+v", m)
	}

	// 不带价格字段的 PATCH 必须原样保留
	rec, _ = patch(`{"enabled":false}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("patch without prices: code=%d body=%s", rec.Code, rec.Body.String())
	}
	m, _ = st.GetUpstreamModel(t.Context(), "m1")
	if m.PriceInput != 0.2 || m.PriceCacheHit != 0.02 || m.PriceOutput != 0.8 {
		t.Fatalf("prices clobbered by unrelated patch: %+v", m)
	}
	if m.Enabled {
		t.Fatalf("unrelated patch should still apply: %+v", m)
	}

	// 显式 0 = 清空
	rec, _ = patch(`{"price_cache_hit":0}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("clear price: code=%d body=%s", rec.Code, rec.Body.String())
	}
	m, _ = st.GetUpstreamModel(t.Context(), "m1")
	if m.PriceCacheHit != 0 || m.PriceInput != 0.2 {
		t.Fatalf("clear semantics wrong: %+v", m)
	}

	// 负价非法
	for _, body := range []string{
		`{"price_input":-1}`,
		`{"price_cache_hit":-0.5}`,
		`{"price_output":-1}`,
	} {
		rec, _ := patch(body)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("negative price %s: want 400, got %d", body, rec.Code)
		}
	}
}

// List 必须把价格下发，否则界面上编辑表单永远回显 0（看着像没保存过）。
func TestModelHandler_ListIncludesPrices(t *testing.T) {
	st := newTestStore(t)
	h := NewModelHandler(st, nil, nil)

	req := httptest.NewRequest(http.MethodPost, "/admin/api/providers/p1/models",
		bytes.NewBufferString(`{"model_id":"priced","price_input":1,"price_output":3}`))
	rec := httptest.NewRecorder()
	h.Create(rec, req, "p1")
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: code=%d body=%s", rec.Code, rec.Body.String())
	}

	req2 := httptest.NewRequest(http.MethodGet, "/admin/api/providers/p1/models", nil)
	rec2 := httptest.NewRecorder()
	h.List(rec2, req2, "p1")
	var list []modelResponse
	if err := json.Unmarshal(rec2.Body.Bytes(), &list); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	var found bool
	for _, m := range list {
		if m.ModelID == "priced" {
			found = true
			if m.PriceInput != 1 || m.PriceCacheHit != 0 || m.PriceOutput != 3 {
				t.Fatalf("listed prices wrong: %+v", m)
			}
		}
	}
	if !found {
		t.Fatalf("created model missing from list: %+v", list)
	}
}
