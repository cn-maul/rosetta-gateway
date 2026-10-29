package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestKeySecret_CreateThenReadReturnsExactPlaintext(t *testing.T) {
	st := newScopeStore(t)
	seedTwoUsers(t, st)
	masterKey := []byte("0123456789abcdef0123456789abcdef")
	h := NewKeyHandler(st, masterKey)

	createRec := httptest.NewRecorder()
	h.Create(createRec, asUser(jsonRequest(http.MethodPost, "/admin/api/keys",
		strings.NewReader(`{"name":"copyable"}`)), "u1"))
	if createRec.Code != http.StatusCreated {
		t.Fatalf("create code=%d body=%s", createRec.Code, createRec.Body.String())
	}
	var created keyCreateResponse
	decodeBody(t, createRec, &created)
	if !created.CanCopy {
		t.Fatal("new key with master key must be copyable")
	}

	rec := httptest.NewRecorder()
	h.Secret(rec, asUser(httptest.NewRequest(http.MethodGet,
		"/admin/api/keys/"+created.ID+"/secret", nil), "u1"), created.ID)
	if rec.Code != http.StatusOK {
		t.Fatalf("secret code=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control=%q, want no-store", got)
	}
	var secret keySecretResponse
	decodeBody(t, rec, &secret)
	if secret.PlaintextKey != created.PlaintextKey {
		t.Fatalf("secret plaintext mismatch: got %q want %q",
			secret.PlaintextKey, created.PlaintextKey)
	}
}

func TestKeySecret_CrossUserIs404(t *testing.T) {
	st := newScopeStore(t)
	seedTwoUsers(t, st)
	h := NewKeyHandler(st, []byte("0123456789abcdef0123456789abcdef"))

	rec := httptest.NewRecorder()
	h.Secret(rec, asUser(httptest.NewRequest(http.MethodGet,
		"/admin/api/keys/k2/secret", nil), "u1"), "k2")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("cross-user secret code=%d, want 404; body=%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "not yours") ||
		strings.Contains(rec.Body.String(), "不是你的") {
		t.Fatalf("response reveals another key's existence: %s", rec.Body.String())
	}
}

func TestKeySecret_LegacyKeyWithoutCiphertextIs409(t *testing.T) {
	st := newScopeStore(t)
	seedTwoUsers(t, st)
	h := NewKeyHandler(st, []byte("0123456789abcdef0123456789abcdef"))

	rec := httptest.NewRecorder()
	h.Secret(rec, asUser(httptest.NewRequest(http.MethodGet,
		"/admin/api/keys/k1/secret", nil), "u1"), "k1")
	if rec.Code != http.StatusConflict {
		t.Fatalf("legacy secret code=%d, want 409; body=%s", rec.Code, rec.Body.String())
	}
}

func TestKeyList_NeverExposesCiphertextOrPlaintext(t *testing.T) {
	st := newScopeStore(t)
	seedTwoUsers(t, st)
	masterKey := []byte("0123456789abcdef0123456789abcdef")
	h := NewKeyHandler(st, masterKey)

	createRec := httptest.NewRecorder()
	h.Create(createRec, asUser(jsonRequest(http.MethodPost, "/admin/api/keys",
		strings.NewReader(`{"name":"listed"}`)), "u1"))
	if createRec.Code != http.StatusCreated {
		t.Fatalf("create code=%d body=%s", createRec.Code, createRec.Body.String())
	}
	var created keyCreateResponse
	decodeBody(t, createRec, &created)

	enc, found, err := st.AccessKeySecret(context.Background(), created.ID)
	if err != nil || !found || len(enc) == 0 {
		t.Fatalf("stored ciphertext: found=%v len=%d err=%v", found, len(enc), err)
	}

	listRec := httptest.NewRecorder()
	h.List(listRec, asUser(httptest.NewRequest(http.MethodGet,
		"/admin/api/keys", nil), "u1"))
	if listRec.Code != http.StatusOK {
		t.Fatalf("list code=%d body=%s", listRec.Code, listRec.Body.String())
	}
	var listed []keyResponse
	decodeBody(t, listRec, &listed)
	var createdInList *keyResponse
	for i := range listed {
		if listed[i].ID == created.ID {
			createdInList = &listed[i]
			break
		}
	}
	if createdInList == nil || !createdInList.CanCopy {
		t.Fatalf("list should expose can_copy=true, got %+v", listed)
	}
	if strings.Contains(listRec.Body.String(), created.PlaintextKey) {
		t.Fatal("list response leaked plaintext key")
	}
	if strings.Contains(listRec.Body.String(), string(enc)) {
		t.Fatal("list response leaked encrypted key")
	}

	// Guard against marshaling a new field that carries the BLOB later.
	raw, err := json.Marshal(createdInList)
	if err != nil {
		t.Fatalf("marshal listed key: %v", err)
	}
	if bytes.Contains(raw, []byte("key_enc")) {
		t.Fatalf("list DTO unexpectedly contains key_enc: %s", raw)
	}
}
