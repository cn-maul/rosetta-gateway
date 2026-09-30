package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cn-maul/rosetta-gateway/internal/store"
)

func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	st, err := store.Open(t.TempDir()+"/gw.db", logger)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	ctx := context.Background()
	_ = st.CreateProvider(ctx, &store.Provider{ID: "p1", Slug: "p1", Name: "Alpha", Endpoint: "http://a", Enabled: true, Protocol: "openai-chat"})
	_ = st.CreateProvider(ctx, &store.Provider{ID: "p2", Slug: "p2", Name: "Beta", Endpoint: "http://b", Enabled: true, Protocol: "openai-chat"})
	_ = st.CreateUpstreamModel(ctx, &store.UpstreamModel{ID: "m1", ProviderID: "p1", ModelID: "alpha-chat", Enabled: true})
	_ = st.CreateUpstreamModel(ctx, &store.UpstreamModel{ID: "m2", ProviderID: "p2", ModelID: "beta-chat", Enabled: true})
	_ = st.CreateRoute(ctx, &store.Route{ID: "r1", PublicName: "flash", ProviderID: "p1", UpstreamModelID: "m1", Enabled: true})
	return st
}

func TestRouteTargetReplaceAndList(t *testing.T) {
	st := newTestStore(t)
	h := NewRouteTargetHandler(st)
	ctx := context.Background()

	body := `{"targets":[{"provider_id":"p2","upstream_model_id":"m2"},{"provider_id":"p1","upstream_model_id":"m1","enabled":false}]}`
	req := httptest.NewRequest(http.MethodPut, "/admin/api/routes/r1/targets", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	h.Replace(rec, req, "r1")
	if rec.Code != http.StatusOK {
		t.Fatalf("replace code = %d, body=%s", rec.Code, rec.Body.String())
	}

	targets, _ := st.ListRouteTargets(ctx, "r1")
	if len(targets) != 2 || targets[0].ProviderID != "p2" || targets[1].ProviderID != "p1" {
		t.Fatalf("chain order wrong: %+v", targets)
	}
	if !targets[0].Enabled || targets[1].Enabled {
		t.Fatalf("enabled flags wrong: %+v", targets)
	}

	// GET 应补齐 provider/model 展示字段。
	req2 := httptest.NewRequest(http.MethodGet, "/admin/api/routes/r1/targets", nil)
	rec2 := httptest.NewRecorder()
	h.List(rec2, req2, "r1")
	if rec2.Code != http.StatusOK {
		t.Fatalf("list code = %d", rec2.Code)
	}
	var resp []routeTargetResponse
	if err := json.Unmarshal(rec2.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp) != 2 || resp[0].ProviderName != "Beta" || resp[0].ModelID != "beta-chat" {
		t.Fatalf("enrichment wrong: %+v", resp)
	}
}

func TestRouteTargetReplaceValidation(t *testing.T) {
	st := newTestStore(t)
	h := NewRouteTargetHandler(st)

	cases := []struct {
		name string
		body string
	}{
		{"empty chain", `{"targets":[]}`},
		{"cross-provider model", `{"targets":[{"provider_id":"p1","upstream_model_id":"m2"}]}`},
		{"missing provider", `{"targets":[{"provider_id":"nope","upstream_model_id":"m1"}]}`},
		{"missing model", `{"targets":[{"provider_id":"p1","upstream_model_id":"nope"}]}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPut, "/admin/api/routes/r1/targets", bytes.NewBufferString(c.body))
			rec := httptest.NewRecorder()
			h.Replace(rec, req, "r1")
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("want 400, got %d body=%s", rec.Code, rec.Body.String())
			}
		})
	}

	// 未知 route → 404
	req := httptest.NewRequest(http.MethodPut, "/admin/api/routes/zzz/targets", bytes.NewBufferString(`{"targets":[{"provider_id":"p1","upstream_model_id":"m1"}]}`))
	rec := httptest.NewRecorder()
	h.Replace(rec, req, "zzz")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d", rec.Code)
	}
}
