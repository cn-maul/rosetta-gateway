package admin

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cn-maul/rosetta-gateway/internal/dialect"
)

// 加模型时不传方言 = 自动，后端必须按模型名推断出「生效值」回给前端，
// 而不是让前端自己再推一遍。
func TestModelHandler_DialectAutoDetected(t *testing.T) {
	st := newTestStore(t)
	h := NewModelHandler(st, nil, nil)

	req := jsonRequest(http.MethodPost, "/admin/api/providers/p1/models",
		bytes.NewBufferString(`{"model_id":"deepseek-chat"}`))
	rec := httptest.NewRecorder()
	h.Create(rec, req, "p1")
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: code=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp modelResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.ThinkingDialect != dialect.Auto {
		t.Fatalf("库里应存「自动」，实际 %q", resp.ThinkingDialect)
	}
	if resp.EffectiveDialect != dialect.DeepSeek {
		t.Fatalf("生效方言应推断为 deepseek，实际 %q", resp.EffectiveDialect)
	}
	if resp.DialectSource != dialect.SourceDetected {
		t.Fatalf("来源应为 detected，实际 %q", resp.DialectSource)
	}
}

// 认不出模型名时必须退回 passthrough，并且要说清是兜底而不是推断。
func TestModelHandler_DialectFallback(t *testing.T) {
	st := newTestStore(t)
	h := NewModelHandler(st, nil, nil)

	req := jsonRequest(http.MethodPost, "/admin/api/providers/p1/models",
		bytes.NewBufferString(`{"model_id":"my-relay-model"}`))
	rec := httptest.NewRecorder()
	h.Create(rec, req, "p1")
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: code=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp modelResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.EffectiveDialect != dialect.Passthrough || resp.DialectSource != dialect.SourceFallback {
		t.Fatalf("应兜底成 passthrough/fallback，实际 %q/%q",
			resp.EffectiveDialect, resp.DialectSource)
	}
}

// 手工指定必须压过推断，并且要真的落库（不是只改响应）。
func TestModelHandler_DialectManualOverridesDetection(t *testing.T) {
	st := newTestStore(t)
	h := NewModelHandler(st, nil, nil)

	// 模型名看着像 DeepSeek，但管理员知道这个中转站把它转成了 Qwen 的方言。
	req := jsonRequest(http.MethodPost, "/admin/api/providers/p1/models",
		bytes.NewBufferString(`{"model_id":"deepseek-compat","thinking_dialect":"qwen"}`))
	rec := httptest.NewRecorder()
	h.Create(rec, req, "p1")
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: code=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp modelResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.EffectiveDialect != dialect.Qwen || resp.DialectSource != dialect.SourceManual {
		t.Fatalf("手工指定应胜出，实际 %q/%q", resp.EffectiveDialect, resp.DialectSource)
	}

	m, err := st.GetUpstreamModel(t.Context(), resp.ID)
	if err != nil || m == nil {
		t.Fatalf("get: %v %v", m, err)
	}
	if m.ThinkingDialect != dialect.Qwen {
		t.Fatalf("方言没落库：%+v", m)
	}

	// 改回自动：空串 = 清除，重新按模型名推断。
	patch := jsonRequest(http.MethodPatch, "/admin/api/models/"+resp.ID,
		bytes.NewBufferString(`{"thinking_dialect":""}`))
	prec := httptest.NewRecorder()
	h.Update(prec, patch, resp.ID)
	if prec.Code != http.StatusOK {
		t.Fatalf("patch: code=%d body=%s", prec.Code, prec.Body.String())
	}
	var presp modelResponse
	_ = json.Unmarshal(prec.Body.Bytes(), &presp)
	if presp.ThinkingDialect != dialect.Auto || presp.EffectiveDialect != dialect.DeepSeek {
		t.Fatalf("改回自动后应重新推断成 deepseek，实际 %q/%q",
			presp.ThinkingDialect, presp.EffectiveDialect)
	}
}

// 不改方言的 PATCH 必须原样保留它 —— 与价格那条纪律一致：
// 「只改了显示名」不该把方言顺手清掉。
func TestModelHandler_DialectSurvivesUnrelatedPatch(t *testing.T) {
	st := newTestStore(t)
	h := NewModelHandler(st, nil, nil)

	req := jsonRequest(http.MethodPost, "/admin/api/providers/p1/models",
		bytes.NewBufferString(`{"model_id":"glm-5.3","thinking_dialect":"glm"}`))
	rec := httptest.NewRecorder()
	h.Create(rec, req, "p1")
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: code=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp modelResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)

	patch := jsonRequest(http.MethodPatch, "/admin/api/models/"+resp.ID,
		bytes.NewBufferString(`{"display_name":"GLM 5.3"}`))
	prec := httptest.NewRecorder()
	h.Update(prec, patch, resp.ID)
	if prec.Code != http.StatusOK {
		t.Fatalf("patch: code=%d body=%s", prec.Code, prec.Body.String())
	}
	m, _ := st.GetUpstreamModel(t.Context(), resp.ID)
	if m.ThinkingDialect != dialect.GLM {
		t.Fatalf("无关 PATCH 不该清掉方言：%+v", m)
	}
}

// 非法方言值必须 400，而不是静默退回自动 —— 否则「填错了」看起来像
// 「配了但没生效」。
func TestModelHandler_DialectRejectsUnknown(t *testing.T) {
	st := newTestStore(t)
	h := NewModelHandler(st, nil, nil)

	req := jsonRequest(http.MethodPost, "/admin/api/providers/p1/models",
		bytes.NewBufferString(`{"model_id":"m","thinking_dialect":"gemini"}`))
	rec := httptest.NewRecorder()
	h.Create(rec, req, "p1")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("非法方言应 400，实际 %d body=%s", rec.Code, rec.Body.String())
	}
}
