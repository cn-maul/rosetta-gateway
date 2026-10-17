package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cn-maul/rosetta-gateway/internal/snapshot"
	"github.com/cn-maul/rosetta-gateway/internal/store"
)

func TestRouteCatalogContractAndAccess(t *testing.T) {
	st := newTestStore(t)
	ctx := t.Context()
	if err := st.ReplaceRouteTargets(ctx, "r1", []store.RouteTarget{
		{ProviderID: "p2", UpstreamModelID: "m2", Position: 0, Enabled: true},
		{ProviderID: "p1", UpstreamModelID: "m1", Position: 1, Enabled: false},
	}); err != nil {
		t.Fatal(err)
	}
	snapshot.Init(&snapshot.Snapshot{Providers: map[string]*snapshot.ProviderSnapshot{
		"p2": {Ready: true},
	}})
	t.Cleanup(func() { snapshot.Init(&snapshot.Snapshot{}) })
	h := NewRouteHandler(st)
	for _, tc := range []struct {
		name    string
		request *http.Request
		code    int
	}{
		{"anonymous", httptest.NewRequest("GET", "/admin/api/route-catalog", nil), 401},
		{"user", asUser(httptest.NewRequest("GET", "/admin/api/route-catalog", nil), "u1"), 403},
		{"admin", asAdmin(httptest.NewRequest("GET", "/admin/api/route-catalog", nil)), 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rr := httptest.NewRecorder()
			h.Catalog(rr, tc.request)
			if rr.Code != tc.code {
				t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
			}
			if tc.code != 200 {
				return
			}
			var got routeCatalogResponse
			if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if len(got.Routes) != 1 || len(got.Models) != 2 || len(got.Providers) != 2 || got.GeneratedAt <= 0 {
				t.Fatalf("catalog=%+v", got)
			}
			route := got.Routes[0]
			if route.FailoverEnabled || route.Chain != (routeChainSummary{Total: 2, Enabled: 1, Standby: 1}) {
				t.Fatalf("summary=%+v", route)
			}
			if len(route.Targets) != 2 || route.Targets[0].ProviderName != "Beta" || route.Targets[0].ModelID != "beta-chat" || route.Targets[1].Enabled {
				t.Fatalf("ordered chain=%+v", route.Targets)
			}
			for _, p := range got.Providers {
				if p.ID == "p1" && p.Ready != nil {
					t.Fatal("missing runtime state must remain unknown")
				}
				if p.ID == "p2" && (p.Ready == nil || !*p.Ready) {
					t.Fatal("runtime readiness missing")
				}
			}
			for _, field := range []string{`"endpoint"`, `"proxy_url"`, `"api_key"`, `"password_hash"`, `"balance_spec_json"`} {
				if strings.Contains(rr.Body.String(), field) {
					t.Fatalf("unexpected private field %s", field)
				}
			}
		})
	}
}

func TestRouteCatalogEmptyArrays(t *testing.T) {
	st := newTestStore(t)
	if err := st.DeleteRoute(t.Context(), "r1"); err != nil {
		t.Fatal(err)
	}
	rr := httptest.NewRecorder()
	NewRouteHandler(st).Catalog(rr, asAdmin(httptest.NewRequest("GET", "/admin/api/route-catalog", nil)))
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), `"routes":[]`) {
		t.Fatalf("empty catalog=%s", rr.Body.String())
	}
}
