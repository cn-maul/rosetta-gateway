package admin

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// 一把密钥同时只能启用一种限额。这条规则既要在 Create 上生效，也要在
// PATCH 上生效 —— 后者才是真正的难点：只传一个字段时，库里另一个限额
// 可能还开着，必须按「合并后的完整状态」判定。
func TestKeyLimitMode_OnlyOneAtATime(t *testing.T) {
	st := newTestStore(t)
	h := NewKeyHandler(st)

	// 同时给 rpm 与 daily_token → 400，且错误信息点名两个字段。
	rec := httptest.NewRecorder()
	h.Create(rec, asAdmin(jsonRequest(http.MethodPost, "/admin/api/keys",
		bytes.NewBufferString(`{"name":"mixed","rpm_limit":60,"daily_token_limit":1000}`))))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("mixed limits should 400, got %d body=%s", rec.Code, rec.Body.String())
	}
	var env struct {
		Error struct{ Message string } `json:"error"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &env)
	if env.Error.Message == "" {
		t.Fatalf("expected a human-readable message, body=%s", rec.Body.String())
	}

	// 单一限额可以建，并且落库。
	rec = httptest.NewRecorder()
	h.Create(rec, asAdmin(jsonRequest(http.MethodPost, "/admin/api/keys",
		bytes.NewBufferString(`{"name":"daily","daily_request_limit":100}`))))
	if rec.Code != http.StatusCreated {
		t.Fatalf("single daily limit should 201, got %d body=%s", rec.Code, rec.Body.String())
	}
	var created keyCreateResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	if created.DailyRequestLimit != 100 || created.DailyTokenLimit != 0 {
		t.Fatalf("daily limit not persisted: %+v", created)
	}

	// 切到 RPM 时必须把 daily 显式置 0，否则库里的旧值仍算「同时开了两种」。
	rec = httptest.NewRecorder()
	h.Update(rec, asAdmin(jsonRequest(http.MethodPatch, "/admin/api/keys/"+created.ID,
		bytes.NewBufferString(`{"rpm_limit":30}`))), created.ID)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("switching without clearing the old limit should 400, got %d body=%s",
			rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	h.Update(rec, asAdmin(jsonRequest(http.MethodPatch, "/admin/api/keys/"+created.ID,
		bytes.NewBufferString(`{"rpm_limit":30,"daily_request_limit":0}`))), created.ID)
	if rec.Code != http.StatusOK {
		t.Fatalf("clearing the old limit then setting rpm should 200, got %d body=%s",
			rec.Code, rec.Body.String())
	}
	var updated keyResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &updated)
	if updated.RPMLimit != 30 || updated.DailyRequestLimit != 0 {
		t.Fatalf("limit switch not applied: %+v", updated)
	}
}

func TestKeyLimitMode_RejectsNegativeDaily(t *testing.T) {
	st := newTestStore(t)
	h := NewKeyHandler(st)
	rec := httptest.NewRecorder()
	h.Create(rec, asAdmin(jsonRequest(http.MethodPost, "/admin/api/keys",
		bytes.NewBufferString(`{"name":"bad","daily_token_limit":-1}`))))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("negative daily limit should 400, got %d body=%s", rec.Code, rec.Body.String())
	}
}
