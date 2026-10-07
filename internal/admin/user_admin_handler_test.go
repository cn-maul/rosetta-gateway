package admin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cn-maul/rosetta-gateway/internal/store"
)

// ---- 空密码 admin 的产生源拦截（P0-4 第二层防线）----
//
// 免鉴权 bootstrap 窗口的一次性标记是兜底（见 bootstrap_handler_test.go），
// 这里验证第一层：用户管理面根本不允许空密码 admin 被建出/升级出来。

// 创建 admin 必须带初始密码。
func TestCreateUser_RejectsEmptyPasswordAdmin(t *testing.T) {
	st := newScopeStore(t)
	h := NewUserHandler(st, newTestManager(t))

	rec := httptest.NewRecorder()
	h.CreateUser(rec, asAdmin(jsonRequest(http.MethodPost, "/admin/api/users",
		strings.NewReader(`{"username":"boss","role":"admin"}`))))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s, want 400 (admin must come with an initial password)", rec.Code, rec.Body.String())
	}
	// 拒绝必须发生在任何写入之前：库里没有这个账号。
	if u, _ := st.GetUserByUsername(t.Context(), "boss"); u != nil {
		t.Error("empty-password admin was created despite the rejection")
	}
}

// 普通用户的空密码账号行为不变：仍可建出（由管理员稍后重置密码）。
func TestCreateUser_AllowsEmptyPasswordRegularUser(t *testing.T) {
	st := newScopeStore(t)
	h := NewUserHandler(st, newTestManager(t))

	rec := httptest.NewRecorder()
	h.CreateUser(rec, asAdmin(jsonRequest(http.MethodPost, "/admin/api/users",
		strings.NewReader(`{"username":"newbie"}`))))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d body=%s, want 201", rec.Code, rec.Body.String())
	}
	u, err := st.GetUserByUsername(t.Context(), "newbie")
	if err != nil || u == nil {
		t.Fatalf("get created user: %v", err)
	}
	if u.Role != store.RoleUser || u.PasswordHash != "" {
		t.Errorf("role=%q hash=%q, want a regular user with empty hash (behavior must be unchanged)",
			u.Role, u.PasswordHash)
	}
}

// 把空密码账号升级成 admin 必须被拒：先重置密码，再升级。
func TestUpdateUser_RejectsPromotingPasswordlessUser(t *testing.T) {
	st := newScopeStore(t)
	h := NewUserHandler(st, newTestManager(t))
	if err := st.CreateUser(t.Context(), &store.User{
		ID: "u1", Username: "bob",
		Role: store.RoleUser, Status: store.UserStatusActive, AuthVersion: 1,
	}); err != nil {
		t.Fatalf("create: %v", err)
	}

	rec := httptest.NewRecorder()
	h.UpdateUser(rec, asAdmin(jsonRequest(http.MethodPatch, "/admin/api/users/u1",
		strings.NewReader(`{"role":"admin"}`))), "u1")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s, want 400 (set a password before promoting)", rec.Code, rec.Body.String())
	}
	u, _ := st.GetUser(t.Context(), "u1")
	if u.Role != store.RoleUser {
		t.Errorf("role = %q, want user (promotion must be refused entirely)", u.Role)
	}
}

// 已有密码的账号正常升级：role 生效，且会话失效仍由 BumpAuthVersion 承担
//（UpdateUser 从不写 auth_version，角色变更恰好 bump 一次）。
func TestUpdateUser_PromotesUserWithPassword(t *testing.T) {
	st := newScopeStore(t)
	h := NewUserHandler(st, newTestManager(t))
	if err := st.CreateUser(t.Context(), &store.User{
		ID: "u1", Username: "bob", PasswordHash: "h1",
		Role: store.RoleUser, Status: store.UserStatusActive, AuthVersion: 1,
	}); err != nil {
		t.Fatalf("create: %v", err)
	}

	rec := httptest.NewRecorder()
	h.UpdateUser(rec, asAdmin(jsonRequest(http.MethodPatch, "/admin/api/users/u1",
		strings.NewReader(`{"role":"admin"}`))), "u1")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s, want 200", rec.Code, rec.Body.String())
	}
	u, _ := st.GetUser(t.Context(), "u1")
	if u.Role != store.RoleAdmin {
		t.Errorf("role = %q, want admin", u.Role)
	}
	if u.AuthVersion != 2 {
		t.Errorf("auth_version = %d, want 2 (role change must bump exactly once)", u.AuthVersion)
	}
}

// ---- 重建唯一所有者 = server.AutoReload（P1-6）----
//
// 旧实现 CreateUser/UpdateUser/DeleteUser 各自同步重建，失败即 500：
// 库里的写已提交，而 AutoReload 对 >=400 的响应直接返回，既不审计也不
// MarkDirty，后台兜底完全不触发，数据面继续按旧快照放行。此处钉住
// 「handler 内零重建调用」—— 注入计数桩并断言一次都不被调。
func TestUserHandlers_DoNotRebuildInline(t *testing.T) {
	st := newScopeStore(t)
	reloads := 0
	h := NewUserHandler(st, newTestManager(t)).WithReload(func(context.Context) error {
		reloads++
		return nil
	})
	ctx := t.Context()
	// 分组必须真实存在：带分组的 CreateUser 正是旧代码触发 handler 内重建的场景。
	if err := st.CreateGroup(ctx, &store.Group{ID: "g1", Name: "dev"}); err != nil {
		t.Fatalf("create group: %v", err)
	}

	// CreateUser 带分组（旧代码在此同步重建）。
	rec := httptest.NewRecorder()
	h.CreateUser(rec, asAdmin(jsonRequest(http.MethodPost, "/admin/api/users",
		strings.NewReader(`{"username":"alice","password":"Str0ngPassw0rd!","group_id":"g1"}`))))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create user = %d body=%s, want 201", rec.Code, rec.Body.String())
	}
	// 后续操作要用 handler 生成的真实 id（请求体不接受 id）。
	var created userResponse
	decodeBody(t, rec, &created)

	// UpdateUser 改角色（bumpVersion 路径，旧代码在此同步重建）。
	rec = httptest.NewRecorder()
	h.UpdateUser(rec, asAdmin(jsonRequest(http.MethodPatch, "/admin/api/users/"+created.ID,
		strings.NewReader(`{"role":"admin"}`))), created.ID)
	if rec.Code != http.StatusOK {
		t.Fatalf("update user = %d body=%s, want 200", rec.Code, rec.Body.String())
	}

	// DeleteUser（旧代码直接调 h.reload）。
	rec = httptest.NewRecorder()
	h.DeleteUser(rec, asAdmin(jsonRequest(http.MethodDelete, "/admin/api/users/"+created.ID, nil)), created.ID)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete user = %d body=%s, want 200", rec.Code, rec.Body.String())
	}

	if reloads != 0 {
		t.Errorf("reload was called %d time(s) inside handlers; rebuild ownership belongs to server.AutoReload "+
			"(an inline failure would return 500 and skip its audit/MarkDirty fallback)", reloads)
	}
}
