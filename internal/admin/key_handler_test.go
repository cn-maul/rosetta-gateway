package admin

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// 创建带配额 → 201 + quota_tokens 落库；PATCH 显式改配额；负数 → 400。
func TestKeyHandler_QuotaWritePath(t *testing.T) {
	st := newTestStore(t)
	h := NewKeyHandler(st)
	ctx := t.Context()

	// create with quota
	rec := httptest.NewRecorder()
	req := jsonRequest(http.MethodPost, "/admin/api/keys",
		bytes.NewBufferString(`{"name":"cli","quota_tokens":1000}`))
	h.Create(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create code=%d body=%s", rec.Code, rec.Body.String())
	}
	var created keyCreateResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("unmarshal create: %v", err)
	}
	if created.QuotaTokens != 1000 || created.PlaintextKey == "" {
		t.Fatalf("create wrong: quota=%d plaintext=%q", created.QuotaTokens, created.PlaintextKey)
	}
	// 库里确实写了配额
	dbKey, _ := st.GetAccessKey(ctx, created.ID)
	if dbKey == nil || dbKey.QuotaTokens != 1000 {
		t.Fatalf("db quota not persisted: %+v", dbKey)
	}

	// PATCH 显式改为 0（不限）：验证 0 是合法写入而非被「非空才覆盖」吞掉
	rec2 := httptest.NewRecorder()
	req2 := jsonRequest(http.MethodPatch, "/admin/api/keys/"+created.ID,
		bytes.NewBufferString(`{"quota_tokens":0}`))
	h.Update(rec2, asAdmin(req2), created.ID)
	if rec2.Code != http.StatusOK {
		t.Fatalf("update code=%d body=%s", rec2.Code, rec2.Body.String())
	}
	var upd keyResponse
	_ = json.Unmarshal(rec2.Body.Bytes(), &upd)
	if upd.QuotaTokens != 0 {
		t.Fatalf("expected quota cleared to 0, got %d", upd.QuotaTokens)
	}

	// PATCH 负数 → 400
	rec3 := httptest.NewRecorder()
	req3 := jsonRequest(http.MethodPatch, "/admin/api/keys/"+created.ID,
		bytes.NewBufferString(`{"quota_tokens":-5}`))
	h.Update(rec3, asAdmin(req3), created.ID)
	if rec3.Code != http.StatusBadRequest {
		t.Fatalf("negative quota should 400, got %d body=%s", rec3.Code, rec3.Body.String())
	}
}
