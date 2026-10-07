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

// 新建用户的余额必须默认**不限额**（NULL），不是 0 分。
//
// 这是一条会被静默吞掉的退化：CreateUser 里若不显式置 Unlimited，
// Go 零值 false 会让 balanceValue 把 0 写进库 —— 新账号一建出来就是
// 「余额 0 分」，首次调用立刻 402，而存量用户（迁移后为 NULL）仍正常。
// 症状是「升级后新建的账号全都用不了」，且没有任何报错指向这里。
func TestCreateUser_DefaultsToUnlimitedBalance(t *testing.T) {
	st := newScopeStore(t)
	h := NewUserHandler(st, newTestManager(t))

	rec := httptest.NewRecorder()
	h.CreateUser(rec, asAdmin(jsonRequest(http.MethodPost, "/admin/api/users",
		strings.NewReader(`{"username":"fresh"}`))))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d body=%s, want 201", rec.Code, rec.Body.String())
	}
	var created userResponse
	decodeBody(t, rec, &created)
	if !created.Unlimited {
		t.Errorf("新建用户应默认不限额（balance_unlimited=true），实际 balance_cents=%d —— "+
			"新账号会一建出来就是欠费状态", created.BalanceCents)
	}

	// 落库也必须是 NULL：DTO 说 unlimited 但库里存了 0 的话，下一个
	// 读回该用户的请求会看到 0 分，扣费直接 402。
	u, err := st.GetUser(t.Context(), created.ID)
	if err != nil {
		t.Fatalf("get user: %v", err)
	}
	if u == nil || !u.Unlimited {
		t.Errorf("库里该用户的余额应为 NULL(不限额)，实际 %+v", u)
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
// （UpdateUser 从不写 auth_version，角色变更恰好 bump 一次）。
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

// ---- 余额充值（delta_cents 相对调整）----

// mkBalanceUser 建一个带初始余额的用户。
func mkBalanceUser(t *testing.T, st *store.Store, id string, cents int64, unlimited bool) {
	t.Helper()
	if err := st.CreateUser(t.Context(), &store.User{
		ID: id, Username: id, Role: store.RoleUser,
		Status: store.UserStatusActive, AuthVersion: 1,
		BalanceCents: cents, Unlimited: unlimited,
	}); err != nil {
		t.Fatalf("create %s: %v", id, err)
	}
}

// 充值是**相对**调整：原余额加上 delta，且响应里带回调整后的余额。
func TestAdjustBalance_TopUpIsRelative(t *testing.T) {
	st := newScopeStore(t)
	h := NewUserHandler(st, newTestManager(t))
	mkBalanceUser(t, st, "u1", 5_000, false) // 50.00 元

	rec := httptest.NewRecorder()
	h.AdjustBalance(rec, asAdmin(jsonRequest(http.MethodPut, "/admin/api/users/u1/balance",
		strings.NewReader(`{"delta_cents":2500}`))), "u1")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s, want 200", rec.Code, rec.Body.String())
	}
	var got userResponse
	decodeBody(t, rec, &got)
	// 5000 + 2500 = 7500 分 = 75.00 元。
	if got.BalanceCents != 7_500 {
		t.Errorf("balance_cents = %d, want 7500 (relative: 5000 + 2500)", got.BalanceCents)
	}
	if got.Unlimited {
		t.Error("unlimited = true, want false (a finite account stays finite)")
	}
	// 库里的值必须真的落下去，不能只改响应。
	cents, limited, err := st.BalanceOf(t.Context(), "u1")
	if err != nil || cents != 7_500 || !limited {
		t.Errorf("stored balance = (%d, limited=%v, err=%v), want (7500, true, nil)", cents, limited, err)
	}
}

// **NULL 不限额**必须经得起「充值」：AdjustBalance 把 NULL 当 0 起算，于是
// 「给不限额用户充 100 元」会把它切成有限额。这条行为是刻意的（给不限额
// 用户充值的意图通常就是「给他设额度」，静默无操作会让人以为失败），
// 但它一旦变成静默发生就会是事故 —— 所以钉住它，且响应必须如实反映。
func TestAdjustBalance_UnlimitedBecomesFiniteOnTopUp(t *testing.T) {
	st := newScopeStore(t)
	h := NewUserHandler(st, newTestManager(t))
	mkBalanceUser(t, st, "vip", 0, true) // NULL = 不限额

	rec := httptest.NewRecorder()
	h.AdjustBalance(rec, asAdmin(jsonRequest(http.MethodPut, "/admin/api/users/vip/balance",
		strings.NewReader(`{"delta_cents":10000}`))), "vip")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s, want 200", rec.Code, rec.Body.String())
	}
	var got userResponse
	decodeBody(t, rec, &got)
	if got.BalanceCents != 10_000 {
		t.Errorf("balance_cents = %d, want 10000", got.BalanceCents)
	}
	if got.Unlimited {
		t.Error("unlimited = true, want false — 充值已把 NULL 切成有限额，响应不能说谎")
	}
}

// 0 元与「不限额」是两个意思相反的状态，绝不能让前端分不出来：
// DTO 必须同时给出数值与 unlimited 标志，且 unlimited 用户不能被读成 0 元。
func TestAdjustBalance_ListShowsUnlimitedNotZero(t *testing.T) {
	st := newScopeStore(t)
	h := NewUserHandler(st, newTestManager(t))
	mkBalanceUser(t, st, "vip", 0, true)
	mkBalanceUser(t, st, "broke", 0, false) // 真的一分钱都没有

	rec := httptest.NewRecorder()
	h.ListUsers(rec, asAdminID(jsonRequest(http.MethodGet, "/admin/api/users", nil), "boss"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s, want 200", rec.Code, rec.Body.String())
	}
	var got []userResponse
	decodeBody(t, rec, &got)

	byID := map[string]userResponse{}
	for _, u := range got {
		byID[u.ID] = u
	}
	vip, ok := byID["vip"]
	if !ok {
		t.Fatal("vip missing from list response")
	}
	if !vip.Unlimited {
		t.Error("vip.unlimited = false, want true（NULL 必须如实带出，否则界面显示成「0.00 元」）")
	}
	broke, ok := byID["broke"]
	if !ok {
		t.Fatal("broke missing from list response")
	}
	if broke.Unlimited {
		t.Error("broke.unlimited = true, want false（0 分是「真没钱」，不是「不限额」）")
	}
	if broke.BalanceCents != 0 {
		t.Errorf("broke.balance_cents = %d, want 0", broke.BalanceCents)
	}
}

// 扣减超过余额必须被拒：SQL 侧的 MAX(0,…) 会夹到 0，那是一次**部分成功**，
// 管理员以为扣了 5000 分、实际只扣到 0，而界面上毫无异常。
func TestAdjustBalance_RejectsOverdraft(t *testing.T) {
	st := newScopeStore(t)
	h := NewUserHandler(st, newTestManager(t))
	mkBalanceUser(t, st, "u1", 1_000, false) // 10.00 元

	rec := httptest.NewRecorder()
	h.AdjustBalance(rec, asAdmin(jsonRequest(http.MethodPut, "/admin/api/users/u1/balance",
		strings.NewReader(`{"delta_cents":-5000}`))), "u1")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s, want 400（超额扣减是一次静默的部分成功，必须挡在门外）",
			rec.Code, rec.Body.String())
	}
	cents, _, _ := st.BalanceOf(t.Context(), "u1")
	if cents != 1_000 {
		t.Errorf("balance = %d, want 1000 (a rejected request must not write)", cents)
	}
}

// 请求畸形要 400：缺 delta_cents 与 delta_cents=0 是两回事 ——
// 前者多半是前端 bug，后者是「把空输入框当成了 0」。都不该静默成功。
func TestAdjustBalance_RejectsMissingAndZeroDelta(t *testing.T) {
	st := newScopeStore(t)
	h := NewUserHandler(st, newTestManager(t))
	mkBalanceUser(t, st, "u1", 1_000, false)

	for name, body := range map[string]string{
		"missing": `{}`,
		"zero":    `{"delta_cents":0}`,
	} {
		rec := httptest.NewRecorder()
		h.AdjustBalance(rec, asAdmin(jsonRequest(http.MethodPut, "/admin/api/users/u1/balance",
			strings.NewReader(body))), "u1")
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d body=%s, want 400", name, rec.Code, rec.Body.String())
		}
	}
	cents, _, _ := st.BalanceOf(t.Context(), "u1")
	if cents != 1_000 {
		t.Errorf("balance = %d, want 1000 (rejected requests must not write)", cents)
	}
}

// 普通用户不能给他人充值（端点只挂 requireAdmin，但仍要有测试钉住）。
func TestAdjustBalance_RequiresAdmin(t *testing.T) {
	st := newScopeStore(t)
	h := NewUserHandler(st, newTestManager(t))
	mkBalanceUser(t, st, "u1", 1_000, false)

	rec := httptest.NewRecorder()
	h.AdjustBalance(rec, asUser(jsonRequest(http.MethodPut, "/admin/api/users/u1/balance",
		strings.NewReader(`{"delta_cents":999999}`)), "u2"), "u1")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d body=%s, want 403", rec.Code, rec.Body.String())
	}
	cents, _, _ := st.BalanceOf(t.Context(), "u1")
	if cents != 1_000 {
		t.Errorf("balance = %d, want 1000 (a non-admin must not be able to mint balance)", cents)
	}
}

// 余额变更**不**重建快照：预检与扣费都直接读库（store.BalanceOf）。
// 在 handler 内重建只会白白多一次全量 RebuildFromDB，与「重建的唯一所有者
// 是 AutoReload」同源的原则一致。
func TestAdjustBalance_DoesNotRebuildInline(t *testing.T) {
	st := newScopeStore(t)
	reloads := 0
	h := NewUserHandler(st, newTestManager(t)).WithReload(func(context.Context) error {
		reloads++
		return nil
	})
	mkBalanceUser(t, st, "u1", 0, false)

	rec := httptest.NewRecorder()
	h.AdjustBalance(rec, asAdmin(jsonRequest(http.MethodPut, "/admin/api/users/u1/balance",
		strings.NewReader(`{"delta_cents":100}`))), "u1")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s, want 200", rec.Code, rec.Body.String())
	}
	if reloads != 0 {
		t.Errorf("reload called %d time(s); balance is read from the DB on the hot path, "+
			"so a snapshot rebuild here is pure overhead", reloads)
	}
}
