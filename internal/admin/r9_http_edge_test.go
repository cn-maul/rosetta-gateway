package admin

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cn-maul/rosetta-gateway/internal/store"
)

// assertSingleErrorEnvelope 断言响应体是**恰好一个** error 信封。
//
// 只断言 rec.Code 会漏掉「413 之后再追加一个 400」这个 bug：Code 取的是
// 第一次写的状态码，看起来完全正确，但 body 里是两个 JSON 串，客户端
// res.json() 抛 SyntaxError。
func assertSingleErrorEnvelope(t *testing.T, rec *httptest.ResponseRecorder, wantStatus int) {
	t.Helper()
	if rec.Code != wantStatus {
		t.Fatalf("status=%d body=%s, want %d", rec.Code, rec.Body.String(), wantStatus)
	}
	dec := json.NewDecoder(bytes.NewReader(rec.Body.Bytes()))
	var envelope apiErrorResponse
	if err := dec.Decode(&envelope); err != nil {
		t.Fatalf("响应体不是合法 JSON 错误信封: %v body=%q", err, rec.Body.String())
	}
	if envelope.Error.Message == "" {
		t.Fatalf("错误信封没有 message: body=%q", rec.Body.String())
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); err != io.EOF {
		t.Fatalf("响应体在第一个错误信封之后还有内容（err=%v），说明又写了一次响应: body=%q",
			err, rec.Body.String())
	}
}

func TestR9_AdminDecodeMapsBodyOverflowTo413(t *testing.T) {
	st := newTestStore(t)
	h := NewGroupHandler(st)
	body := []byte(`{"name":"` + strings.Repeat("x", 128) + `"}`)
	req := jsonRequest(http.MethodPost, "/admin/api/groups", bytes.NewReader(body))
	req.Body = http.MaxBytesReader(httptest.NewRecorder(), io.NopCloser(bytes.NewReader(body)), 16)
	rec := httptest.NewRecorder()

	h.Create(rec, asAdmin(req))
	assertSingleErrorEnvelope(t, rec, http.StatusRequestEntityTooLarge)
}

// 415 走的是同一条「helper 已写过响应」路径，所以也必须在调用方 return，
// 否则 415 后面会跟一个 400（decodeJSON 的哨兵被当成普通解析错误）。
func TestR9_AdminDecodeMapsWrongContentTypeTo415Only(t *testing.T) {
	st := newTestStore(t)
	h := NewGroupHandler(st)
	req := httptest.NewRequest(http.MethodPost, "/admin/api/groups",
		strings.NewReader(`{"name":"x"}`))
	req.Header.Set("Content-Type", "text/plain")
	rec := httptest.NewRecorder()

	h.Create(rec, asAdmin(req))
	assertSingleErrorEnvelope(t, rec, http.StatusUnsupportedMediaType)
}

func TestR9_AuditLimitRejectsMalformedValue(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "audit.db"),
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	rec := httptest.NewRecorder()
	NewAuditHandler(st).List(rec,
		httptest.NewRequest(http.MethodGet, "/admin/api/audit?limit=abc", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("audit limit=abc status=%d body=%s, want 400", rec.Code, rec.Body.String())
	}
}

func TestR9_GroupPatchRejectsBlankName(t *testing.T) {
	st := newScopeStore(t)
	h := NewGroupHandler(st)
	if err := st.CreateGroup(t.Context(), &store.Group{ID: "g1", Name: "dev"}); err != nil {
		t.Fatalf("seed group: %v", err)
	}
	for _, body := range []string{`{"name":""}`, `{"name":"   "}`} {
		rec := httptest.NewRecorder()
		h.Update(rec, asAdmin(jsonRequest(http.MethodPatch,
			"/admin/api/groups/g1", strings.NewReader(body))), "g1")
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s status=%d body=%s, want 400", body, rec.Code, rec.Body.String())
		}
	}
	g, err := st.GetGroup(t.Context(), "g1")
	if err != nil || g == nil || g.Name != "dev" {
		t.Fatalf("blank PATCH changed stored name: group=%+v err=%v", g, err)
	}
}
