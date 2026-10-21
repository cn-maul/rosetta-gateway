package access

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cn-maul/rosetta-gateway/internal/identity"
	"github.com/cn-maul/rosetta-gateway/internal/store"
)

func gate(t *testing.T, method, path, pattern string, u *store.User) (int, bool) {
	t.Helper()
	reached := false
	mux := http.NewServeMux()
	mux.HandleFunc(method+" "+pattern, func(http.ResponseWriter, *http.Request) {})

	req := httptest.NewRequest(method, path, nil)
	if u != nil {
		req = req.WithContext(identity.WithUser(req.Context(), u))
	}
	rec := httptest.NewRecorder()
	AdminGateGuard(mux, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	})).ServeHTTP(rec, req)
	return rec.Code, reached
}

func TestAdminGateGuard_AllowsWhitelistedNormalUser(t *testing.T) {
	u := &store.User{ID: "u1", Role: store.RoleUser, Status: store.UserStatusActive}
	code, reached := gate(t, http.MethodGet, "/admin/api/keys", "/admin/api/keys", u)
	if code != http.StatusOK || !reached {
		t.Fatalf("白名单端点应放行，code=%d reached=%v", code, reached)
	}
}

func TestAdminGateGuard_RejectsUnlistedNormalUser(t *testing.T) {
	u := &store.User{ID: "u1", Role: store.RoleUser, Status: store.UserStatusActive}
	code, reached := gate(t, http.MethodGet, "/admin/api/users", "/admin/api/users", u)
	if code != http.StatusForbidden || reached {
		t.Fatalf("非白名单端点应 403，code=%d reached=%v", code, reached)
	}
}

func TestAdminGateGuard_AllowsAdmin(t *testing.T) {
	u := &store.User{ID: "admin-1", Role: store.RoleAdmin, Status: store.UserStatusActive}
	code, reached := gate(t, http.MethodGet, "/admin/api/users", "/admin/api/users", u)
	if code != http.StatusOK || !reached {
		t.Fatalf("管理员应放行，code=%d reached=%v", code, reached)
	}
}

func TestAdminGateGuard_FailsClosed(t *testing.T) {
	// 未注册模式：mux.Handler 返回空 pattern，白名单必然不命中。
	u := &store.User{ID: "u1", Role: store.RoleUser, Status: store.UserStatusActive}
	code, reached := gate(t, http.MethodGet, "/admin/api/unknown", "/admin/api/unknown", u)
	if code != http.StatusForbidden || reached {
		t.Fatalf("未知路由应 fail-closed，code=%d reached=%v", code, reached)
	}

	// 空 ID 的 admin 不是有效身份，必须按普通用户处理。
	ghost := &store.User{ID: "", Role: store.RoleAdmin, Status: store.UserStatusActive}
	code, reached = gate(t, http.MethodGet, "/admin/api/users", "/admin/api/users", ghost)
	if code != http.StatusForbidden || reached {
		t.Fatalf("空 ID admin 必须 fail-closed，code=%d reached=%v", code, reached)
	}
}
