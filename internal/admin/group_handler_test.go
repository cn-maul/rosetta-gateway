package admin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cn-maul/rosetta-gateway/internal/routing"
	"github.com/cn-maul/rosetta-gateway/internal/snapshot"
	"github.com/cn-maul/rosetta-gateway/internal/store"
)

// initRouteSnapshot 装一份「有若干公开模型」的快照，供白名单校验与
// 模型名清单用。
//
// users 参数可选：ModelNames 这类端点要按用户身份收窄，测试就需要快照里
// 有对应的用户条目。**一次性构造后整体发布**，不在发布之后改 map ——
// 已发布的快照按契约是被并发无锁读取的（见 snapshot.Snapshot 的说明）。
func initRouteSnapshot(t *testing.T, names ...string) {
	t.Helper()
	initSnapshotWithUsers(t, names, nil)
}

func initSnapshotWithUsers(t *testing.T, names []string, users map[string]*snapshot.UserSnapshot) {
	t.Helper()
	ri := routing.NewRouteIndex()
	ri.AddProvider(&routing.ProviderRef{ID: "p1", Slug: "good", Protocol: "openai-chat", Enabled: true})
	ri.AddUpstreamModel(&routing.UpstreamModel{ID: "good/m", ProviderID: "p1", ModelID: "good-model", Enabled: true})
	for _, n := range names {
		ri.AddRoute(&routing.Route{
			ID: "r-" + n, PublicName: n,
			ProviderID: "p1", UpstreamModelID: "good/m", Enabled: true,
		})
	}
	if users == nil {
		users = map[string]*snapshot.UserSnapshot{}
	}
	snapshot.Init(&snapshot.Snapshot{
		Routes: ri, Providers: map[string]*snapshot.ProviderSnapshot{},
		KeysByHash: map[string]*snapshot.KeySnapshot{},
		UsersByID:  users,
	})
	t.Cleanup(func() { snapshot.Init(&snapshot.Snapshot{}) })
}

// ---- 分组 CRUD ----

func TestGroupHandler_CreateListUpdateDelete(t *testing.T) {
	st := newScopeStore(t)
	h := NewGroupHandler(st)

	// 建
	rec := httptest.NewRecorder()
	h.Create(rec, asAdmin(jsonRequest(http.MethodPost, "/admin/api/groups",
		strings.NewReader(`{"name":"研发","description":"内部"}`))))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: code = %d body = %s", rec.Code, rec.Body.String())
	}
	var created groupResponse
	decodeBody(t, rec, &created)
	if created.ID == "" || created.Name != "研发" {
		t.Fatalf("created = %+v", created)
	}
	// 新组的白名单必须是空数组（= 不限制），前端要能直接渲染。
	if created.Models == nil || len(created.Models) != 0 {
		t.Errorf("new group models = %v, want empty non-nil slice", created.Models)
	}

	// 列
	rec2 := httptest.NewRecorder()
	h.List(rec2, asAdmin(httptest.NewRequest(http.MethodGet, "/admin/api/groups", nil)))
	if rec2.Code != http.StatusOK {
		t.Fatalf("list: code = %d", rec2.Code)
	}
	var list []groupResponse
	decodeBody(t, rec2, &list)
	if len(list) != 1 || list[0].ID != created.ID {
		t.Fatalf("list = %+v", list)
	}

	// 改
	rec3 := httptest.NewRecorder()
	h.Update(rec3, asAdmin(jsonRequest(http.MethodPatch, "/admin/api/groups/"+created.ID,
		strings.NewReader(`{"name":"研发二组"}`))), created.ID)
	if rec3.Code != http.StatusOK {
		t.Fatalf("update: code = %d body = %s", rec3.Code, rec3.Body.String())
	}
	got, _ := st.GetGroup(context.Background(), created.ID)
	if got.Name != "研发二组" {
		t.Errorf("name = %q", got.Name)
	}

	// 删
	rec4 := httptest.NewRecorder()
	h.Delete(rec4, asAdmin(httptest.NewRequest(http.MethodDelete, "/admin/api/groups/"+created.ID, nil)), created.ID)
	if rec4.Code != http.StatusOK {
		t.Fatalf("delete: code = %d body = %s", rec4.Code, rec4.Body.String())
	}
	if g, _ := st.GetGroup(context.Background(), created.ID); g != nil {
		t.Error("group still exists after delete")
	}
}

// 组名重复 → 409（而不是 500）。前端要能区分「重名」与「服务出错了」。
func TestGroupHandler_CreateDuplicateNameConflicts(t *testing.T) {
	st := newScopeStore(t)
	h := NewGroupHandler(st)
	body := `{"name":"dev"}`

	rec := httptest.NewRecorder()
	h.Create(rec, asAdmin(jsonRequest(http.MethodPost, "/admin/api/groups", strings.NewReader(body))))
	if rec.Code != http.StatusCreated {
		t.Fatalf("first create: code = %d", rec.Code)
	}
	rec2 := httptest.NewRecorder()
	h.Create(rec2, asAdmin(jsonRequest(http.MethodPost, "/admin/api/groups", strings.NewReader(body))))
	if rec2.Code != http.StatusConflict {
		t.Fatalf("duplicate name: code = %d body = %s, want 409", rec2.Code, rec2.Body.String())
	}
}

func TestGroupHandler_CreateRejectsEmptyName(t *testing.T) {
	h := NewGroupHandler(newScopeStore(t))
	for _, body := range []string{`{"name":""}`, `{"name":"   "}`} {
		rec := httptest.NewRecorder()
		h.Create(rec, asAdmin(jsonRequest(http.MethodPost, "/admin/api/groups", strings.NewReader(body))))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("body %s: code = %d, want 400", body, rec.Code)
		}
	}
}

// 组里还有成员时删不掉，且错误信息要说清**为什么**与**怎么办**。
// 只说「删除失败」会让管理员去改库，而真正要做的是把人迁走。
func TestGroupHandler_DeleteRefusedWhenMembersExist(t *testing.T) {
	st := newScopeStore(t)
	ctx := context.Background()
	h := NewGroupHandler(st)

	if err := st.CreateGroup(ctx, &store.Group{ID: "g1", Name: "dev"}); err != nil {
		t.Fatalf("seed group: %v", err)
	}
	if err := st.CreateUser(ctx, &store.User{ID: "u1", Username: "alice", GroupID: "g1"}); err != nil {
		t.Fatalf("seed user: %v", err)
	}

	rec := httptest.NewRecorder()
	h.Delete(rec, asAdmin(httptest.NewRequest(http.MethodDelete, "/admin/api/groups/g1", nil)), "g1")
	if rec.Code != http.StatusConflict {
		t.Fatalf("code = %d body = %s, want 409", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "移到别的组") {
		t.Errorf("错误信息应给出处置办法（把人移到别的组）：%s", rec.Body.String())
	}
	// 人数也要报出来 —— 迁移工作量取决于它。
	if !strings.Contains(rec.Body.String(), "1") {
		t.Errorf("错误信息应包含成员人数：%s", rec.Body.String())
	}
}

// ---- 白名单写入 ----

func TestGroupHandler_SetModelsReplacesAndValidates(t *testing.T) {
	initRouteSnapshot(t, "flash", "pro")
	st := newScopeStore(t)
	ctx := context.Background()
	h := NewGroupHandler(st)
	if err := st.CreateGroup(ctx, &store.Group{ID: "g1", Name: "dev"}); err != nil {
		t.Fatalf("seed group: %v", err)
	}

	// 合法名单
	rec := httptest.NewRecorder()
	h.SetModels(rec, asAdmin(jsonRequest(http.MethodPut, "/admin/api/groups/g1/models",
		strings.NewReader(`{"models":["flash","pro"]}`))), "g1")
	if rec.Code != http.StatusOK {
		t.Fatalf("set models: code = %d body = %s", rec.Code, rec.Body.String())
	}

	// 含不存在的模型名 → 400，且不落库（整体拒绝，不做部分写入）。
	rec2 := httptest.NewRecorder()
	h.SetModels(rec2, asAdmin(jsonRequest(http.MethodPut, "/admin/api/groups/g1/models",
		strings.NewReader(`{"models":["flash","typo-model"]}`))), "g1")
	if rec2.Code != http.StatusBadRequest {
		t.Fatalf("unknown model: code = %d body = %s, want 400", rec2.Code, rec2.Body.String())
	}
	if !strings.Contains(rec2.Body.String(), "typo-model") {
		t.Errorf("错误信息要点名是哪个模型不存在：%s", rec2.Body.String())
	}
	models, _ := st.ListGroupModelsByGroup(ctx, "g1")
	if len(models) != 2 {
		t.Errorf("models = %v, want the previous 2 entries untouched", models)
	}

	// 清空 = 不限制（不是「拒绝全部」）。
	rec3 := httptest.NewRecorder()
	h.SetModels(rec3, asAdmin(jsonRequest(http.MethodPut, "/admin/api/groups/g1/models",
		strings.NewReader(`{"models":[]}`))), "g1")
	if rec3.Code != http.StatusOK {
		t.Fatalf("clear: code = %d", rec3.Code)
	}
	models, _ = st.ListGroupModelsByGroup(ctx, "g1")
	if len(models) != 0 {
		t.Errorf("models = %v, want empty after clear", models)
	}

	// 不存在的组 → 404。
	rec4 := httptest.NewRecorder()
	h.SetModels(rec4, asAdmin(jsonRequest(http.MethodPut, "/admin/api/groups/nope/models",
		strings.NewReader(`{"models":[]}`))), "nope")
	if rec4.Code != http.StatusNotFound {
		t.Errorf("unknown group: code = %d, want 404", rec4.Code)
	}
}

// ---- 权限：分组端点是 admin-only ----

// 普通用户碰分组必须 403 —— 否则他能给自己换一个不限制的组、
// 或者把别人的组改成受限。
func TestGroupHandler_RequiresAdmin(t *testing.T) {
	st := newScopeStore(t)
	ctx := context.Background()
	if err := st.CreateGroup(ctx, &store.Group{ID: "g1", Name: "dev"}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	h := NewGroupHandler(st)

	cases := []struct {
		name string
		call func(rec *httptest.ResponseRecorder)
	}{
		{"list", func(rec *httptest.ResponseRecorder) {
			h.List(rec, asUser(httptest.NewRequest(http.MethodGet, "/admin/api/groups", nil), "u1"))
		}},
		{"create", func(rec *httptest.ResponseRecorder) {
			h.Create(rec, asUser(jsonRequest(http.MethodPost, "/admin/api/groups", strings.NewReader(`{"name":"x"}`)), "u1"))
		}},
		{"update", func(rec *httptest.ResponseRecorder) {
			h.Update(rec, asUser(jsonRequest(http.MethodPatch, "/admin/api/groups/g1", strings.NewReader(`{"name":"x"}`)), "u1"), "g1")
		}},
		{"delete", func(rec *httptest.ResponseRecorder) {
			h.Delete(rec, asUser(httptest.NewRequest(http.MethodDelete, "/admin/api/groups/g1", nil), "u1"), "g1")
		}},
		{"setModels", func(rec *httptest.ResponseRecorder) {
			h.SetModels(rec, asUser(jsonRequest(http.MethodPut, "/admin/api/groups/g1/models", strings.NewReader(`{"models":[]}`)), "u1"), "g1")
		}},
	}
	for _, tc := range cases {
		rec := httptest.NewRecorder()
		tc.call(rec)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s: code = %d, want 403 (body=%s)", tc.name, rec.Code, rec.Body.String())
		}
	}
	// 确认没有任何副作用。
	if g, _ := st.GetGroup(ctx, "g1"); g == nil || g.Name != "dev" {
		t.Errorf("group was modified by a non-admin: %+v", g)
	}
}

// ---- 用户归属分组 ----

func TestUserHandler_CreateWithGroupAndValidation(t *testing.T) {
	st := newScopeStore(t)
	ctx := context.Background()
	if err := st.CreateGroup(ctx, &store.Group{ID: "g1", Name: "dev"}); err != nil {
		t.Fatalf("seed group: %v", err)
	}
	h := NewUserHandler(st, nil)

	rec := httptest.NewRecorder()
	h.CreateUser(rec, asAdmin(jsonRequest(http.MethodPost, "/admin/api/users",
		strings.NewReader(`{"username":"alice","group_id":"g1"}`))))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: code = %d body = %s", rec.Code, rec.Body.String())
	}
	var created userResponse
	decodeBody(t, rec, &created)
	if created.GroupID != "g1" {
		t.Errorf("GroupID = %q, want g1", created.GroupID)
	}

	// 指向不存在的组 → 400。写进一个悬空 id 会让快照回落成「不限制」，
	// 也就是「以为收紧了，实际放得更开」。
	rec2 := httptest.NewRecorder()
	h.CreateUser(rec2, asAdmin(jsonRequest(http.MethodPost, "/admin/api/users",
		strings.NewReader(`{"username":"bob","group_id":"ghost"}`))))
	if rec2.Code != http.StatusBadRequest {
		t.Fatalf("unknown group: code = %d body = %s, want 400", rec2.Code, rec2.Body.String())
	}
	if u, _ := st.GetUserByUsername(ctx, "bob"); u != nil {
		t.Error("user must not be created when group_id is invalid")
	}
}

func TestUserHandler_UpdateGroupID(t *testing.T) {
	st := newScopeStore(t)
	ctx := context.Background()
	for _, g := range []string{"g1", "g2"} {
		if err := st.CreateGroup(ctx, &store.Group{ID: g, Name: g}); err != nil {
			t.Fatalf("seed group %s: %v", g, err)
		}
	}
	if err := st.CreateUser(ctx, &store.User{ID: "u1", Username: "alice", GroupID: "g1"}); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	h := NewUserHandler(st, nil)

	// 换组
	rec := httptest.NewRecorder()
	h.UpdateUser(rec, asAdmin(jsonRequest(http.MethodPatch, "/admin/api/users/u1",
		strings.NewReader(`{"group_id":"g2"}`))), "u1")
	if rec.Code != http.StatusOK {
		t.Fatalf("move: code = %d body = %s", rec.Code, rec.Body.String())
	}
	if u, _ := st.GetUser(ctx, "u1"); u.GroupID != "g2" {
		t.Errorf("GroupID = %q, want g2", u.GroupID)
	}

	// 传空串 = 移出分组（与「不传」必须区分开）。
	rec2 := httptest.NewRecorder()
	h.UpdateUser(rec2, asAdmin(jsonRequest(http.MethodPatch, "/admin/api/users/u1",
		strings.NewReader(`{"group_id":""}`))), "u1")
	if rec2.Code != http.StatusOK {
		t.Fatalf("ungroup: code = %d", rec2.Code)
	}
	if u, _ := st.GetUser(ctx, "u1"); u.GroupID != "" {
		t.Errorf("GroupID = %q, want empty after ungroup", u.GroupID)
	}

	// 不传 group_id = 保持原值。
	if err := st.CreateUser(ctx, &store.User{ID: "u2", Username: "bob", GroupID: "g1"}); err != nil {
		t.Fatalf("seed bob: %v", err)
	}
	rec3 := httptest.NewRecorder()
	h.UpdateUser(rec3, asAdmin(jsonRequest(http.MethodPatch, "/admin/api/users/u2",
		strings.NewReader(`{"display_name":"Bob"}`))), "u2")
	if rec3.Code != http.StatusOK {
		t.Fatalf("patch other field: code = %d", rec3.Code)
	}
	if u, _ := st.GetUser(ctx, "u2"); u.GroupID != "g1" {
		t.Errorf("GroupID = %q, want g1 unchanged (字段缺省 ≠ 清空)", u.GroupID)
	}
}

// ---- key 白名单 ----

func TestKeyHandler_AllowedModels(t *testing.T) {
	initRouteSnapshot(t, "flash", "pro")
	st := newScopeStore(t)
	ctx := context.Background()

	// 建一把普通用户（asUser）的 key 需要归属用户存在。
	if err := st.CreateUser(ctx, &store.User{ID: "u1", Username: "alice"}); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	h := NewKeyHandler(st)

	rec := httptest.NewRecorder()
	h.Create(rec, asAdmin(jsonRequest(http.MethodPost, "/admin/api/keys",
		strings.NewReader(`{"name":"k","allowed_models":["flash"],"user_id":"u1"}`))))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: code = %d body = %s", rec.Code, rec.Body.String())
	}
	var created keyResponse
	decodeBody(t, rec, &created)
	if len(created.AllowedModels) != 1 || created.AllowedModels[0] != "flash" {
		t.Fatalf("AllowedModels = %v, want [flash]", created.AllowedModels)
	}

	// 不存在的模型名 → 400，不落库。
	rec2 := httptest.NewRecorder()
	h.Create(rec2, asAdmin(jsonRequest(http.MethodPost, "/admin/api/keys",
		strings.NewReader(`{"name":"k2","allowed_models":["nope"],"user_id":"u1"}`))))
	if rec2.Code != http.StatusBadRequest {
		t.Fatalf("unknown model: code = %d body = %s, want 400", rec2.Code, rec2.Body.String())
	}

	// PATCH 传空数组 = 清除限制。
	rec3 := httptest.NewRecorder()
	h.Update(rec3, asUser(jsonRequest(http.MethodPatch, "/admin/api/keys/"+created.ID,
		strings.NewReader(`{"allowed_models":[]}`)), "u1"), created.ID)
	if rec3.Code != http.StatusOK {
		t.Fatalf("clear: code = %d body = %s", rec3.Code, rec3.Body.String())
	}
	var cleared keyResponse
	decodeBody(t, rec3, &cleared)
	if len(cleared.AllowedModels) != 0 {
		t.Errorf("AllowedModels = %v, want empty (= 不限制)", cleared.AllowedModels)
	}
	k, _ := st.GetAccessKey(ctx, created.ID)
	if k.AllowedModels != nil {
		t.Errorf("db AllowedModels = %v, want nil after clear", k.AllowedModels)
	}

	// PATCH 不传 = 保持原值。
	rec4 := httptest.NewRecorder()
	h.Update(rec4, asUser(jsonRequest(http.MethodPatch, "/admin/api/keys/"+created.ID,
		strings.NewReader(`{"name":"renamed"}`)), "u1"), created.ID)
	if rec4.Code != http.StatusOK {
		t.Fatalf("patch name: code = %d", rec4.Code)
	}
	var same keyResponse
	decodeBody(t, rec4, &same)
	if len(same.AllowedModels) != 0 {
		t.Errorf("AllowedModels = %v, want still empty", same.AllowedModels)
	}
}

// ---- 模型名清单（普通用户可用，按身份收窄）----

type modelNamesResponse struct {
	Models     []string `json:"models"`
	Restricted bool     `json:"restricted"`
}

func fetchModelNames(t *testing.T, h *GroupHandler, r *http.Request) modelNamesResponse {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ModelNames(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("model-names: code = %d body = %s", rec.Code, rec.Body.String())
	}
	var out modelNamesResponse
	decodeBody(t, rec, &out)
	return out
}

// 快照里的用户条目（ModelNames 按它收窄）。
func snapUser(id, group string, allow snapshot.ModelAllow) *snapshot.UserSnapshot {
	return &snapshot.UserSnapshot{
		ID: id, Name: id, Role: store.RoleUser, Status: store.UserStatusActive,
		AuthVersion: 1, GroupID: group, AllowedModels: allow,
	}
}

func TestModelNames_AdminSeesEverything(t *testing.T) {
	initRouteSnapshot(t, "flash", "pro")
	h := NewGroupHandler(newScopeStore(t))

	got := fetchModelNames(t, h, asAdmin(httptest.NewRequest(http.MethodGet, "/admin/api/model-names", nil)))
	if got.Restricted {
		t.Error("admin should get the unrestricted list")
	}
	if len(got.Models) != 2 || got.Models[0] != "flash" || got.Models[1] != "pro" {
		t.Fatalf("models = %v, want [flash pro] (sorted)", got.Models)
	}
}

// 普通用户只拿到「他最多能用到什么」= 与组白名单求交。
func TestModelNames_NormalUserScopedByGroup(t *testing.T) {
	users := map[string]*snapshot.UserSnapshot{
		// 在受限组里
		"u1": snapUser("u1", "g1", snapshot.AllowOnly([]string{"flash", "pro"})),
		// 不在任何组
		"u2": snapUser("u2", "", snapshot.AllowAll()),
		// 组白名单与路由无交集
		"u3": snapUser("u3", "g1", snapshot.AllowOnly([]string{"nonexistent"})),
	}
	initSnapshotWithUsers(t, []string{"flash", "pro", "opus"}, users)

	h := NewGroupHandler(newScopeStore(t))

	restricted := fetchModelNames(t, h, asUser(httptest.NewRequest(http.MethodGet, "/admin/api/model-names", nil), "u1"))
	if !restricted.Restricted {
		t.Error("u1 is in a restricted group → restricted should be true")
	}
	if len(restricted.Models) != 2 || restricted.Models[0] != "flash" || restricted.Models[1] != "pro" {
		t.Fatalf("u1 models = %v, want [flash pro]（opus 已被组白名单排除）", restricted.Models)
	}

	// 不在任何组 → 全集，且 restricted=false（界面据此知道这不是被收窄过的）。
	open := fetchModelNames(t, h, asUser(httptest.NewRequest(http.MethodGet, "/admin/api/model-names", nil), "u2"))
	if open.Restricted || len(open.Models) != 3 {
		t.Fatalf("u2 models = %v restricted=%v, want 全集 3 个 + restricted=false", open.Models, open.Restricted)
	}

	// 组白名单与路由无交集 → 空列表，**不是全集**。
	// 返回全集等于「静默放宽」：用户会以为自己被限制在某几个模型上，
	// 实际却什么都能调。
	none := fetchModelNames(t, h, asUser(httptest.NewRequest(http.MethodGet, "/admin/api/model-names", nil), "u3"))
	if !none.Restricted {
		t.Error("u3 is restricted → restricted should be true")
	}
	if len(none.Models) != 0 {
		t.Fatalf("u3 models = %v, want empty（交集为空不得回退成全集）", none.Models)
	}
}

// 未登录 → 401。没有身份时若返回全集，等于把路由拓扑公开出去。
func TestModelNames_RequiresLogin(t *testing.T) {
	initRouteSnapshot(t, "flash")
	h := NewGroupHandler(newScopeStore(t))

	rec := httptest.NewRecorder()
	h.ModelNames(rec, httptest.NewRequest(http.MethodGet, "/admin/api/model-names", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("code = %d, want 401", rec.Code)
	}
}

// ---- P2：密钥的有效期 / 来源 IP / 分组覆盖（管理写入口）----

// newKeyStore 造一个带用户与分组的库，供密钥 P2 用例使用。
func newKeyStore(t *testing.T) *store.Store {
	t.Helper()
	st := newScopeStore(t)
	ctx := context.Background()
	if err := st.CreateUser(ctx, &store.User{ID: "u1", Username: "alice"}); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if err := st.CreateGroup(ctx, &store.Group{ID: "g1", Name: "restricted"}); err != nil {
		t.Fatalf("seed group: %v", err)
	}
	return st
}

func TestKeyHandler_P2Validation(t *testing.T) {
	st := newKeyStore(t)
	ctx := context.Background()
	h := NewKeyHandler(st)
	admin := func(body string) *http.Request {
		return asAdminID(jsonRequest(http.MethodPost, "/admin/api/keys", strings.NewReader(body)), "admin1")
	}
	user := func(body string) *http.Request {
		return asUser(jsonRequest(http.MethodPost, "/admin/api/keys", strings.NewReader(body)), "u1")
	}

	// body 全部写全（不做字符串拼装）—— 拼装出来的 JSON 出错时报错信息
	// 指向解析器，很难看出是哪一步拼错的。
	cases := []struct {
		name     string
		as       func(string) *http.Request
		body     string
		wantCode int
	}{
		{"ok-cidr", admin, `{"name":"ok-cidr","user_id":"u1","allowed_ips":"10.0.0.0/8, 203.0.113.7"}`, http.StatusCreated},
		{"bad-one-in-list", admin, `{"name":"bad-one-in-list","user_id":"u1","allowed_ips":"10.0.0.0/8,bogus"}`, http.StatusBadRequest},
		{"bad-garbage", admin, `{"name":"bad-garbage","user_id":"u1","allowed_ips":"not-an-ip"}`, http.StatusBadRequest},
		{"neg-expires", admin, `{"name":"neg-expires","user_id":"u1","expires_at":-1}`, http.StatusBadRequest},
		{"zero-expires", admin, `{"name":"zero-expires","user_id":"u1","expires_at":0}`, http.StatusCreated},
		{"admin-set-group", admin, `{"name":"admin-set-group","user_id":"u1","group_id":"g1"}`, http.StatusCreated},
		{"group-missing", admin, `{"name":"group-missing","user_id":"u1","group_id":"ghost"}`, http.StatusBadRequest},
		// 发 key 已收敛为管理员专属（P1-5）：自助发 key 让管理员落在
		// 具体 key 上的禁用/限额/期限/IP 全部可被「重新建一把」绕过。
		// 分组覆盖同理 —— 那是权限提升，不是配置。
		{"user-set-group", user, `{"name":"user-set-group","group_id":"g1"}`, http.StatusForbidden},
		{"user-set-ips", user, `{"name":"user-set-ips","allowed_ips":"10.0.0.0/8"}`, http.StatusForbidden},
	}
	for _, tc := range cases {
		rec := httptest.NewRecorder()
		h.Create(rec, tc.as(tc.body))
		if rec.Code != tc.wantCode {
			t.Errorf("%s: code = %d, want %d (body=%s)", tc.name, rec.Code, tc.wantCode, rec.Body.String())
		}
		// 失败的创建绝不能留下半条记录。按本用例独有的名字判定 ——
		// 同表里成功过的用例也会建 key，用公共名字查会互相干扰。
		keys, _ := st.ListAccessKeysByUser(ctx, "u1")
		created := false
		for _, key := range keys {
			if key.Name == tc.name {
				created = true
			}
		}
		if wantCreated := tc.wantCode < http.StatusBadRequest; created != wantCreated {
			t.Errorf("%s: created = %v, want %v（校验失败却写入了记录，或该成功却没写）",
				tc.name, created, wantCreated)
		}
	}
}
