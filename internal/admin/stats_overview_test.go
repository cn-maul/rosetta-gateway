package admin

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cn-maul/rosetta-gateway/internal/snapshot"
	"github.com/cn-maul/rosetta-gateway/internal/store"
)

func TestStatsOverviewCountsConfigurationAndRequiresAdmin(t *testing.T) {
	st := newTestStore(t)
	ctx := t.Context()

	if err := st.CreateUpstreamModel(ctx, &store.UpstreamModel{
		ID: "m3", ProviderID: "p1", ModelID: "alpha-extra",
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateUser(ctx, &store.User{
		ID: "u1", Username: "u1", PasswordHash: "x", Role: store.RoleUser,
		Status: store.UserStatusActive, AuthVersion: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateUser(ctx, &store.User{
		ID: "u2", Username: "u2", PasswordHash: "x", Role: store.RoleUser,
		Status: store.UserStatusDisabled, AuthVersion: 1,
	}); err != nil {
		t.Fatal(err)
	}
	for _, k := range []*store.AccessKey{
		{ID: "k1", KeyHash: "h1", KeyPrefix: "sk-gw-1", Name: "k1", Enabled: true, UserID: "u1"},
		{ID: "k2", KeyHash: "h2", KeyPrefix: "sk-gw-2", Name: "k2", Enabled: false, UserID: "u2"},
	} {
		if err := st.CreateAccessKey(ctx, k); err != nil {
			t.Fatal(err)
		}
	}

	// newTestStore 建了两个上游、两个模型、一条公开路由。先给 p1/p2
	// 写入运行时快照，验证“启用 / 就绪 / 未就绪 / 未知”四种状态没有被合成
	// 一个模糊的 ready=false。
	snapshot.Swap(&snapshot.Snapshot{Providers: map[string]*snapshot.ProviderSnapshot{
		"p1": {Ready: true},
		"p2": {Ready: false, Reason: "凭据为空"},
	}})

	rec := httptest.NewRecorder()
	NewStatsHandler(st).GetOverview(rec, asAdmin(httptest.NewRequest(http.MethodGet, "/admin/api/stats/overview", nil)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var got statsOverviewResponse
	decodeBody(t, rec, &got)
	if got.ProvidersTotal != 2 || got.ProvidersEnabled != 2 || got.ProvidersReady != 1 || got.ProvidersUnready != 1 || got.ProvidersUnknown != 0 {
		t.Errorf("providers=%+v", got)
	}
	if got.RoutesTotal != 1 || got.RoutesEnabled != 1 {
		t.Errorf("routes=%+v", got)
	}
	if got.UpstreamModelsTotal != 3 {
		t.Errorf("models=%+v", got)
	}
	if got.UsersTotal != 3 || got.UsersEnabled != 2 { // admin-test + u1/u2
		t.Errorf("users=%+v", got)
	}
	if got.KeysTotal != 2 || got.KeysEnabled != 1 {
		t.Errorf("keys=%+v", got)
	}

	nonAdmin := httptest.NewRecorder()
	NewStatsHandler(st).GetOverview(nonAdmin, asUser(httptest.NewRequest(http.MethodGet, "/admin/api/stats/overview", nil), "u1"))
	if nonAdmin.Code != http.StatusForbidden {
		t.Errorf("normal user status=%d, want 403", nonAdmin.Code)
	}
}
