package admin

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cn-maul/rosetta-gateway/internal/store"
	"github.com/cn-maul/rosetta-gateway/internal/userauth"
)

// seedUninitializedAdmin 造出「启动时引导出的 admin」（密码为空）。
func seedUninitializedAdmin(t *testing.T, st *store.Store) *store.User {
	t.Helper()
	u := &store.User{
		ID: "boot-admin", Username: "admin",
		Role: store.RoleAdmin, Status: store.UserStatusActive, AuthVersion: 1,
	}
	if err := st.CreateUser(t.Context(), u); err != nil {
		t.Fatalf("seed admin: %v", err)
	}
	return u
}

// BootstrapStatus 必须在有未初始化 admin 时报告 needs_setup。
func TestBootstrapStatus_ReportsPendingAdmin(t *testing.T) {
	st := newScopeStore(t)
	h := NewUserHandler(st, newTestManager(t))
	seedUninitializedAdmin(t, st)

	rec := httptest.NewRecorder()
	h.BootstrapStatus(rec, jsonRequest(http.MethodGet, "/admin/api/bootstrap", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got bootstrapStatusResponse
	decodeBody(t, rec, &got)
	if !got.NeedsSetup {
		t.Error("needs_setup = false; the login page would show a password box that cannot work")
	}
	if got.Username != "admin" {
		t.Errorf("username = %q, want %q", got.Username, "admin")
	}
}

// 设过密码后必须报告「不需要引导」——否则界面会一直显示设密码表单。
func TestBootstrapStatus_AfterSetup(t *testing.T) {
	st := newScopeStore(t)
	h := NewUserHandler(st, newTestManager(t))
	u := seedUninitializedAdmin(t, st)
	if err := st.SetUserPassword(t.Context(), u.ID, "pbkdf2-sha256$210000$c2FsdA==$aGFzaA=="); err != nil {
		t.Fatalf("set password: %v", err)
	}

	rec := httptest.NewRecorder()
	h.BootstrapStatus(rec, jsonRequest(http.MethodGet, "/admin/api/bootstrap", nil))

	var got bootstrapStatusResponse
	decodeBody(t, rec, &got)
	if got.NeedsSetup {
		t.Error("needs_setup = true after password was set")
	}
}

// 核心流程：设完密码**直接拿到可用会话**，不必再登一次。
//
// 旧流程要三步（用 admin_token 进后台 → 找到 admin → 重置密码），
// 统一认证后那条路已经不存在，引导必须自成闭环。
func TestBootstrapSetup_IssuesUsableSession(t *testing.T) {
	st := newScopeStore(t)
	mgr := newTestManager(t)
	h := NewUserHandler(st, mgr)
	u := seedUninitializedAdmin(t, st)

	rec := httptest.NewRecorder()
	h.BootstrapSetup(rec, jsonRequest(http.MethodPost, "/admin/api/bootstrap",
		strings.NewReader(`{"password":"Str0ngPassw0rd!"}`)))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var got loginResponse
	decodeBody(t, rec, &got)
	if got.Token == "" {
		t.Fatal("no token issued; the user would have to log in again immediately")
	}
	if got.Username != "admin" {
		t.Errorf("username = %q, want admin", got.Username)
	}

	// 签发的令牌必须**立刻可用**：带的是 SetUserPassword 递增后的新
	// auth_version。读旧版本号的话，前端刚拿到令牌就 401。
	sess, err := mgr.Verify(got.Token, mustAuthVersion(t, st, u.ID))
	if err != nil {
		t.Fatalf("issued token is not usable: %v", err)
	}
	if sess.UserID != u.ID {
		t.Errorf("session user = %q, want %q", sess.UserID, u.ID)
	}

	// 设完之后必须能用新密码走正常登录。
	if !userauth.VerifyPassword(mustHash(t, st, u.ID), "Str0ngPassw0rd!") {
		t.Error("password not verifiable after setup")
	}
}

// 引导是一次性的：设完再调必须 409，而不是静默覆盖。
//
// 静默成功会让「重复提交」（用户连点两次、脚本重试）看起来像成功了，
// 而实际第一次设的密码已经被悄悄改掉 —— 没有任何人知道新密码是什么。
func TestBootstrapSetup_RejectsSecondCall(t *testing.T) {
	st := newScopeStore(t)
	h := NewUserHandler(st, newTestManager(t))
	seedUninitializedAdmin(t, st)

	first := httptest.NewRecorder()
	h.BootstrapSetup(first, jsonRequest(http.MethodPost, "/admin/api/bootstrap",
		strings.NewReader(`{"password":"Str0ngPassw0rd!"}`)))
	if first.Code != http.StatusOK {
		t.Fatalf("first setup failed: %d body=%s", first.Code, first.Body.String())
	}

	second := httptest.NewRecorder()
	h.BootstrapSetup(second, jsonRequest(http.MethodPost, "/admin/api/bootstrap",
		strings.NewReader(`{"password":"An0therPassw0rd!"}`)))
	if second.Code != http.StatusConflict {
		t.Fatalf("second setup = %d, want 409 (must not silently overwrite)", second.Code)
	}

	// 关键：第一次设的密码必须还在。
	if !userauth.VerifyPassword(mustHash(t, st, "boot-admin"), "Str0ngPassw0rd!") {
		t.Error("first password was overwritten by a second bootstrap call")
	}
}

// 弱口令必须被拒，且**不能**消耗一次 KDF。
func TestBootstrapSetup_RejectsWeakPassword(t *testing.T) {
	st := newScopeStore(t)
	h := NewUserHandler(st, newTestManager(t))
	seedUninitializedAdmin(t, st)

	rec := httptest.NewRecorder()
	h.BootstrapSetup(rec, jsonRequest(http.MethodPost, "/admin/api/bootstrap",
		strings.NewReader(`{"password":"short"}`)))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	// 密码没设上 → 引导窗口必须仍然开着，否则用户被永久卡在门外。
	pending, err := st.FindUninitializedAdmin(t.Context())
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if pending == nil {
		t.Fatal("weak password consumed the bootstrap window; admin is now locked out")
	}
}

// 跨站请求必须被拒：这是免鉴权写接口的第二道 CSRF 防线。
//
// 第一道是 decodeJSON 的 Content-Type 断言（与请求体形状耦合），
// 这道只看浏览器行为（Sec-Fetch-Site 由浏览器强制发送，JS 无法伪造）。
func TestBootstrapSetup_RejectsCrossOrigin(t *testing.T) {
	st := newScopeStore(t)
	h := NewUserHandler(st, newTestManager(t))
	seedUninitializedAdmin(t, st)

	cases := []struct {
		name  string
		build func(*http.Request)
	}{
		{"Sec-Fetch-Site=cross-site", func(r *http.Request) {
			r.Header.Set("Sec-Fetch-Site", "cross-site")
		}},
		{"same-site（同站跨源）", func(r *http.Request) {
			// 攻击者控制的子域也能打，对 CSRF 而言与跨源等价。
			r.Header.Set("Sec-Fetch-Site", "same-site")
		}},
		{"Origin 指向别处", func(r *http.Request) {
			r.Header.Set("Origin", "http://evil.example.com")
		}},
		{"Sec-Fetch-Site 优先于伪造的 Origin", func(r *http.Request) {
			r.Header.Set("Sec-Fetch-Site", "cross-site")
			r.Header.Set("Origin", "http://example.com")
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := jsonRequest(http.MethodPost, "/admin/api/bootstrap",
				strings.NewReader(`{"password":"Attacker#2026pw"}`))
			tc.build(req)
			rec := httptest.NewRecorder()
			h.BootstrapSetup(rec, req)

			if rec.Code != http.StatusForbidden {
				t.Fatalf("cross-origin setup = %d, want 403", rec.Code)
			}
			// 密码**不能**被设上。VerifyPassword 对空哈希返回 false，
			// 所以这里断言的是「攻击者的密码不成立」。
			if userauth.VerifyPassword(mustHash(t, st, "boot-admin"), "Attacker#2026pw") {
				t.Fatal("attacker password was accepted — admin takeover via CSRF")
			}
		})
	}
}

// 同源与 CLI 必须放行，否则运维没法用命令行完成初始化。
func TestBootstrapSetup_AllowsSameOriginAndCLI(t *testing.T) {
	cases := []struct {
		name  string
		build func(*http.Request)
	}{
		{"Sec-Fetch-Site=same-origin", func(r *http.Request) {
			r.Header.Set("Sec-Fetch-Site", "same-origin")
		}},
		{"Sec-Fetch-Site=none（地址栏直达）", func(r *http.Request) {
			r.Header.Set("Sec-Fetch-Site", "none")
		}},
		{"Origin 与 Host 一致", func(r *http.Request) {
			r.Header.Set("Origin", "http://example.com")
		}},
		{"curl：不带来源头", func(*http.Request) {}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := newScopeStore(t)
			h := NewUserHandler(st, newTestManager(t))
			seedUninitializedAdmin(t, st)

			req := jsonRequest(http.MethodPost, "/admin/api/bootstrap",
				strings.NewReader(`{"password":"Legit#2026pw"}`))
			tc.build(req)
			rec := httptest.NewRecorder()
			h.BootstrapSetup(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("legit setup rejected: %d body=%s", rec.Code, rec.Body.String())
			}
		})
	}
}

// 普通用户的空密码账号**不能**走这条引导通道。
//
// 管理员可以为某人建一个空密码账号；那个人若能自助设密，
// 就等于绕过了管理员的意图。
func TestBootstrapSetup_IgnoresNonAdminAccount(t *testing.T) {
	st := newScopeStore(t)
	h := NewUserHandler(st, newTestManager(t))
	if err := st.CreateUser(t.Context(), &store.User{
		ID: "u1", Username: "bob",
		Role: store.RoleUser, Status: store.UserStatusActive, AuthVersion: 1,
	}); err != nil {
		t.Fatalf("create user: %v", err)
	}

	rec := httptest.NewRecorder()
	h.BootstrapSetup(rec, jsonRequest(http.MethodPost, "/admin/api/bootstrap",
		strings.NewReader(`{"password":"Sneaky#2026pw"}`)))

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (no uninitialized admin exists)", rec.Code)
	}
	if userauth.VerifyPassword(mustHash(t, st, "u1"), "Sneaky#2026pw") {
		t.Fatal("a non-admin account was password-initialized through the bootstrap endpoint")
	}
}

// 标记已置时引导窗口必须**永久**关闭 —— 哪怕库里又出现了空密码 admin。
//
// 旧判定「存在 role=admin 且 password_hash=” 的账号」是窗口曾经开放的原因，
// 不是开关：bug 期间建出/升级出的空密码 admin 会让仅凭该判定的窗口
// 永久重开。修复后窗口绑定一次性标记（bootstrap_completed，与设密同事务
// 写入），标记置位即关死。这里模拟 bug 期间的残留状态做行为断言。
func TestBootstrapStatus_MarkerClosedDespiteEmptyPasswordAdmin(t *testing.T) {
	st := newScopeStore(t)
	h := NewUserHandler(st, newTestManager(t))
	seedUninitializedAdmin(t, st)
	// 真实完成一次引导（标记与密码同事务写入）。
	if ok, err := st.SetInitialAdminPassword(t.Context(), "boot-admin",
		"pbkdf2-sha256$210000$c2FsdA==$aGFzaA=="); err != nil || !ok {
		t.Fatalf("complete bootstrap: ok=%v err=%v, want true", ok, err)
	}
	// 模拟 bug 期间残留的空密码 admin（创建入口已堵住，store 层直插只为测判定）。
	if err := st.CreateUser(t.Context(), &store.User{
		ID: "ghost-admin", Username: "ghost",
		Role: store.RoleAdmin, Status: store.UserStatusActive, AuthVersion: 1,
	}); err != nil {
		t.Fatalf("seed empty-password admin: %v", err)
	}

	rec := httptest.NewRecorder()
	h.BootstrapStatus(rec, jsonRequest(http.MethodGet, "/admin/api/bootstrap", nil))
	var got bootstrapStatusResponse
	decodeBody(t, rec, &got)
	if got.NeedsSetup {
		t.Error("needs_setup = true although bootstrap already completed; the unauthenticated window must not reopen")
	}
	if got.Username != "" {
		t.Errorf("username = %q, want empty (no setup pending)", got.Username)
	}
}

// 标记已置时 POST bootstrap 必须 409，且不给任何账号设上密码 ——
// 包括库里恰好存在的空密码 admin（P0-4 的行为断言）。
func TestBootstrapSetup_MarkerClosedBlocksEmptyPasswordAdmin(t *testing.T) {
	st := newScopeStore(t)
	h := NewUserHandler(st, newTestManager(t))
	seedUninitializedAdmin(t, st)
	if ok, err := st.SetInitialAdminPassword(t.Context(), "boot-admin",
		"pbkdf2-sha256$210000$c2FsdA==$aGFzaA=="); err != nil || !ok {
		t.Fatalf("complete bootstrap: ok=%v err=%v, want true", ok, err)
	}
	if err := st.CreateUser(t.Context(), &store.User{
		ID: "ghost-admin", Username: "ghost",
		Role: store.RoleAdmin, Status: store.UserStatusActive, AuthVersion: 1,
	}); err != nil {
		t.Fatalf("seed empty-password admin: %v", err)
	}

	rec := httptest.NewRecorder()
	h.BootstrapSetup(rec, jsonRequest(http.MethodPost, "/admin/api/bootstrap",
		strings.NewReader(`{"password":"Attacker#2026pw"}`)))
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d body=%s, want 409 (window must be closed by the marker)", rec.Code, rec.Body.String())
	}
	if userauth.VerifyPassword(mustHash(t, st, "ghost-admin"), "Attacker#2026pw") {
		t.Fatal("attacker took over the empty-password admin via bootstrap after the window was closed")
	}
	if userauth.VerifyPassword(mustHash(t, st, "boot-admin"), "Attacker#2026pw") {
		t.Fatal("attacker overwrote the initialized admin password")
	}
}

func mustAuthVersion(t *testing.T, st *store.Store, id string) int64 {
	t.Helper()
	u, err := st.GetUser(t.Context(), id)
	if err != nil || u == nil {
		t.Fatalf("get user %s: %v", id, err)
	}
	return u.AuthVersion
}

func mustHash(t *testing.T, st *store.Store, id string) string {
	t.Helper()
	u, err := st.GetUser(t.Context(), id)
	if err != nil || u == nil {
		t.Fatalf("get user %s: %v", id, err)
	}
	return u.PasswordHash
}
