package admin

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cn-maul/rosetta-gateway/internal/store"
)

func TestR9_AdminDecodeMapsBodyOverflowTo413(t *testing.T) {
	st := newTestStore(t)
	h := NewGroupHandler(st)
	body := []byte(`{"name":"` + strings.Repeat("x", 128) + `"}`)
	req := jsonRequest(http.MethodPost, "/admin/api/groups", bytes.NewReader(body))
	req.Body = http.MaxBytesReader(httptest.NewRecorder(), io.NopCloser(bytes.NewReader(body)), 16)
	rec := httptest.NewRecorder()

	h.Create(rec, asAdmin(req))
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized admin body status=%d body=%s, want 413", rec.Code, rec.Body.String())
	}
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
