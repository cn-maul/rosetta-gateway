package admin

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cn-maul/rosetta-gateway/internal/snapshot"
	"github.com/cn-maul/rosetta-gateway/internal/store"
)

// 「只能收紧」守卫的测试：管理员对 key 施加的强制措施（禁用/配额/限速/
// 有效期/IP 白名单）不能被归属者用 PATCH 撤销。每条规则同时验证
// 「放宽被拒」与「收紧放行」两个方向，防止守卫退化成一刀切。
//
// 另含自助发 key 的用例：发 key 已对所有登录用户开放（只归本人），
// 于是 Create 路径的「额度封顶」与 Update 路径的 guardNoLoosening
// 共同构成同一套「只能收紧」语义，两边都要有测试守着。

func newLooseningStore(t *testing.T) (*store.Store, *KeyHandler) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "gw.db"),
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()
	if err := st.CreateUser(ctx, &store.User{
		ID: "u1", Username: "alice", PasswordHash: "x",
		Role: store.RoleUser, Status: store.UserStatusActive, AuthVersion: 1,
	}); err != nil {
		t.Fatalf("create user: %v", err)
	}
	h := NewKeyHandler(st)
	return st, h
}

// seedRestrictedKey 造一把被管理员全面管束的 key：禁用、配额/限速收紧、
// 设了有效期与 IP 白名单。
func seedRestrictedKey(t *testing.T, st *store.Store) *store.AccessKey {
	t.Helper()
	k := &store.AccessKey{
		ID: "kr", KeyHash: "h-kr", KeyPrefix: "sk-", Name: "restricted",
		Enabled: false, QuotaTokens: 1000, UserID: "u1",
		RPMLimit: 10, TPMLimit: 1000,
		ExpiresAt:  time.Now().Add(48 * time.Hour).UnixMilli(),
		AllowedIPs: "10.0.0.0/8",
	}
	if err := st.CreateAccessKey(context.Background(), k); err != nil {
		t.Fatalf("seed key: %v", err)
	}
	return k
}

func patchKey(h *KeyHandler, as func(*http.Request) *http.Request, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := as(jsonRequest(http.MethodPatch, "/admin/api/keys/kr", strings.NewReader(body)))
	h.Update(rec, req, "kr")
	return rec
}

func TestKeyUpdate_OwnerCannotReenableDisabledKey(t *testing.T) {
	st, h := newLooseningStore(t)
	seedRestrictedKey(t, st)

	rec := patchKey(h, func(r *http.Request) *http.Request { return asUser(r, "u1") }, `{"enabled":true}`)
	if rec.Code != http.StatusForbidden {
		t.Errorf("re-enable disabled key: code=%d body=%s, want 403", rec.Code, rec.Body.String())
	}
	if k, _ := st.GetAccessKey(context.Background(), "kr"); k.Enabled {
		t.Errorf("disabled key was re-enabled: %+v", k)
	}
}

func TestKeyUpdate_OwnerCanSelfDisable(t *testing.T) {
	st, h := newLooseningStore(t)
	seedRestrictedKey(t, st)
	if err := st.UpdateAccessKey(context.Background(), "kr", &store.AccessKey{
		ID: "kr", KeyHash: "h-kr", KeyPrefix: "sk-", Name: "restricted",
		Enabled: true, UserID: "u1",
	}); err != nil {
		t.Fatalf("enable key: %v", err)
	}

	rec := patchKey(h, func(r *http.Request) *http.Request { return asUser(r, "u1") }, `{"enabled":false}`)
	if rec.Code != http.StatusOK {
		t.Errorf("self-disable: code=%d body=%s, want 200", rec.Code, rec.Body.String())
	}
}

func TestKeyUpdate_OwnerCannotLoosenQuotaAndLimits(t *testing.T) {
	st, h := newLooseningStore(t)
	seedRestrictedKey(t, st)
	asU := func(r *http.Request) *http.Request { return asUser(r, "u1") }

	for name, body := range map[string]string{
		"raise quota": `{"quota_tokens":2000}`,
		"clear quota": `{"quota_tokens":0}`,
		"raise rpm":   `{"rpm_limit":100}`,
		"clear rpm":   `{"rpm_limit":0}`,
		"raise tpm":   `{"tpm_limit":5000}`,
		"clear tpm":   `{"tpm_limit":0}`,
	} {
		rec := patchKey(h, asU, body)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s: code=%d body=%s, want 403", name, rec.Code, rec.Body.String())
		}
	}
	// 收紧方向必须放行。
	rec := patchKey(h, asU, `{"quota_tokens":500,"rpm_limit":5,"tpm_limit":500}`)
	if rec.Code != http.StatusOK {
		t.Errorf("tighten limits: code=%d body=%s, want 200", rec.Code, rec.Body.String())
	}
	k, _ := st.GetAccessKey(context.Background(), "kr")
	if k.QuotaTokens != 500 || k.RPMLimit != 5 || k.TPMLimit != 500 {
		t.Errorf("tightened values not applied: %+v", k)
	}
}

func TestKeyUpdate_OwnerCanSetLimitsOnUnrestrictedKey(t *testing.T) {
	st, h := newLooseningStore(t)
	if err := st.CreateAccessKey(context.Background(), &store.AccessKey{
		ID: "kfree", KeyHash: "h-free", KeyPrefix: "sk-", Name: "free", Enabled: true, UserID: "u1",
	}); err != nil {
		t.Fatalf("seed key: %v", err)
	}

	// 现值 0（不限）：给自己加限制是收紧，放行。
	rec := httptest.NewRecorder()
	req := asUser(jsonRequest(http.MethodPatch, "/admin/api/keys/kfree",
		strings.NewReader(`{"quota_tokens":100,"rpm_limit":10,"tpm_limit":1000}`)), "u1")
	h.Update(rec, req, "kfree")
	if rec.Code != http.StatusOK {
		t.Errorf("self-tighten unlimited key: code=%d body=%s, want 200", rec.Code, rec.Body.String())
	}
}

func TestKeyUpdate_OwnerCannotExtendOrClearExpiry(t *testing.T) {
	st, h := newLooseningStore(t)
	seedRestrictedKey(t, st)
	asU := func(r *http.Request) *http.Request { return asUser(r, "u1") }

	extend := time.Now().Add(240 * time.Hour).UnixMilli()
	rec := patchKey(h, asU, `{"expires_at":`+strconv.FormatInt(extend, 10)+`}`)
	if rec.Code != http.StatusForbidden {
		t.Errorf("extend expiry: code=%d body=%s, want 403", rec.Code, rec.Body.String())
	}
	rec = patchKey(h, asU, `{"expires_at":0}`)
	if rec.Code != http.StatusForbidden {
		t.Errorf("clear expiry (revive forever): code=%d body=%s, want 403", rec.Code, rec.Body.String())
	}

	shorten := time.Now().Add(1 * time.Hour).UnixMilli()
	rec = patchKey(h, asU, `{"expires_at":`+strconv.FormatInt(shorten, 10)+`}`)
	if rec.Code != http.StatusOK {
		t.Errorf("shorten expiry: code=%d body=%s, want 200", rec.Code, rec.Body.String())
	}
}

func TestKeyUpdate_OwnerCannotWidenIPAllowlist(t *testing.T) {
	st, h := newLooseningStore(t)
	seedRestrictedKey(t, st)
	asU := func(r *http.Request) *http.Request { return asUser(r, "u1") }

	for name, body := range map[string]string{
		"clear allowlist": `{"allowed_ips":""}`,
		"widen to v4 all": `{"allowed_ips":"0.0.0.0/0"}`,
		"outside net":     `{"allowed_ips":"192.168.0.0/16"}`,
	} {
		rec := patchKey(h, asU, body)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s: code=%d body=%s, want 403", name, rec.Code, rec.Body.String())
		}
	}
	// 收紧：落在现有 /8 内的子网放行。
	rec := patchKey(h, asU, `{"allowed_ips":"10.1.2.0/24,10.3.0.0/16"}`)
	if rec.Code != http.StatusOK {
		t.Errorf("narrow allowlist: code=%d body=%s, want 200", rec.Code, rec.Body.String())
	}
}

func TestKeyUpdate_AdminBypassesLooseningGuard(t *testing.T) {
	st, h := newLooseningStore(t)
	seedRestrictedKey(t, st)

	// 管理员可以做全部「放宽」动作：重新启用、清配额、清限速、清有效期、清 IP。
	rec := patchKey(h, asAdmin, `{"enabled":true,"quota_tokens":0,"rpm_limit":0,"tpm_limit":0,"expires_at":0,"allowed_ips":""}`)
	if rec.Code != http.StatusOK {
		t.Errorf("admin full relax: code=%d body=%s, want 200", rec.Code, rec.Body.String())
	}
	k, _ := st.GetAccessKey(context.Background(), "kr")
	if !k.Enabled || k.QuotaTokens != 0 || k.RPMLimit != 0 || k.TPMLimit != 0 || k.ExpiresAt != 0 || k.AllowedIPs != "" {
		t.Errorf("admin relax not applied: %+v", k)
	}
}

// ---- 自助发 key（对所有登录用户开放，但只归本人）----

// createKey 以 as() 注入的身份发一把 key，返回响应记录。
func createKey(h *KeyHandler, as func(*http.Request) *http.Request, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.Create(rec, as(jsonRequest(http.MethodPost, "/admin/api/keys", strings.NewReader(body))))
	return rec
}

// 开放自助发 key 后，最要紧的一条：**额度封顶必须真的生效**。
//
// 这段封顶逻辑在「发 key 收敛为管理员专属」期间是被 403 挡住的死代码 ——
// 删掉门却不补测试，等于把一段从没跑过的代码直接放进生产路径。
// 所以这里同时钉住三个方向：限速强制清零、quota 不得超过用户级、
// 0（不限）时继承用户级额度。
func TestKeyCreate_UserQuotaCappedAndLimitsCleared(t *testing.T) {
	st, h := newLooseningStore(t)
	// 给 alice 一个用户级总额上限 —— 这是普通用户能拿到的天花板。
	setUserQuota(t, st, "u1", 5000)
	asU := func(r *http.Request) *http.Request { return asUser(r, "u1") }

	// 任意填的限速一律清零：users 表没有 rpm/tpm 列，没有用户级上限可比。
	rec := createKey(h, asU, `{"name":"a","quota_tokens":1000,"rpm_limit":600,"tpm_limit":99999}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: code=%d body=%s, want 201", rec.Code, rec.Body.String())
	}
	var created keyCreateResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if created.RPMLimit != 0 || created.TPMLimit != 0 {
		t.Errorf("rpm/tpm not cleared: rpm=%d tpm=%d, want 0/0", created.RPMLimit, created.TPMLimit)
	}
	// 未超用户级额度时按用户请求的值走。
	if created.QuotaTokens != 1000 {
		t.Errorf("quota=%d, want 1000", created.QuotaTokens)
	}
	// 落库也必须是清零后的值（回显正确但库里没清 = 下一把 key 又能用限速）。
	if k, _ := st.GetAccessKey(context.Background(), created.ID); k == nil ||
		k.RPMLimit != 0 || k.TPMLimit != 0 {
		t.Errorf("rpm/tpm not cleared in db: %+v", k)
	}

	// 超过用户级额度 → 403，且不得留下半条记录。
	rec = createKey(h, asU, `{"name":"b","quota_tokens":999999}`)
	if rec.Code != http.StatusForbidden {
		t.Errorf("quota over user ceiling: code=%d body=%s, want 403", rec.Code, rec.Body.String())
	}
	keys, _ := st.ListAccessKeysByUser(context.Background(), "u1")
	for _, k := range keys {
		if k.Name == "b" {
			t.Fatalf("rejected create still wrote a row: %+v", k)
		}
	}

	// 请求 0（不限）时继承用户级额度：用户不该靠「不填」拿到比授权更多的额度。
	rec = createKey(h, asU, `{"name":"c","quota_tokens":0}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create unlimited: code=%d body=%s, want 201", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if created.QuotaTokens != 5000 {
		t.Errorf("quota=0 should inherit user ceiling 5000, got %d", created.QuotaTokens)
	}
}

// 用户级额度本身不限（0）时，key 级 0 保持 0；管理员则完全不受封顶约束。
func TestKeyCreate_NoUserCeilingAndAdminUncapped(t *testing.T) {
	st, h := newLooseningStore(t)
	asU := func(r *http.Request) *http.Request { return asUser(r, "u1") }

	rec := createKey(h, asU, `{"name":"free","quota_tokens":0,"rpm_limit":30}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: code=%d body=%s, want 201", rec.Code, rec.Body.String())
	}
	var created keyCreateResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if created.QuotaTokens != 0 {
		t.Errorf("quota=%d, want 0 (user has no ceiling, key-level 0 stays 0)", created.QuotaTokens)
	}
	if created.RPMLimit != 0 {
		t.Errorf("rpm=%d, want 0 even with no user ceiling", created.RPMLimit)
	}

	// 管理员：给 alice 配了用户级额度也照样能建超额的 key（自己那份）。
	// 管理员身份必须是**库里真实存在的一行**：access_keys.user_id 有外键
	// 约束（store.go），而归属恒为 me.ID，没有「匿名管理员」这条路。
	seedAdmin(t, st, "admin-real")
	setUserQuota(t, st, "u1", 5000)
	rec = createKey(h, func(r *http.Request) *http.Request {
		return asAdminID(r, "admin-real")
	}, `{"name":"by-admin","quota_tokens":777777,"rpm_limit":600,"tpm_limit":600}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("admin create: code=%d body=%s, want 201", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if created.QuotaTokens != 777777 || created.RPMLimit != 600 || created.TPMLimit != 600 {
		t.Errorf("admin was capped: %+v", created)
	}
}

// 归属规则：任何人建 key 都只归自己，请求里的 user_id 被静默忽略 ——
// **管理员也不例外**。管理员要替别人建只能建完再走 Update 的认领。
//
// 两个方向都要钉住：普通用户不能借 user_id 越权，管理员也不能用
// 「创建时指定」把审计里的「谁建的」和「谁改的」混成一条。
func TestKeyCreate_OwnerIsAlwaysSelf(t *testing.T) {
	st, h := newLooseningStore(t)
	// 新增一个 u2：若 user_id 真被采纳，建出来的 key 会归到 u2 名下。
	if err := st.CreateUser(context.Background(), &store.User{
		ID: "u2", Username: "bob", PasswordHash: "x",
		Role: store.RoleUser, Status: store.UserStatusActive, AuthVersion: 1,
	}); err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedAdmin(t, st, "admin-real")

	t.Run("regular user", func(t *testing.T) {
		rec := createKey(h, func(r *http.Request) *http.Request { return asUser(r, "u1") },
			`{"name":"mine","user_id":"u2"}`)
		if rec.Code != http.StatusCreated {
			t.Fatalf("create: code=%d body=%s, want 201", rec.Code, rec.Body.String())
		}
		var created keyCreateResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if created.UserID != "u1" {
			t.Errorf("owner=%q, want u1 (user_id must be ignored)", created.UserID)
		}
		if bob, _ := st.ListAccessKeysByUser(context.Background(), "u2"); len(bob) != 0 {
			t.Errorf("key landed on another user: %+v", bob)
		}
	})

	t.Run("admin", func(t *testing.T) {
		rec := createKey(h, func(r *http.Request) *http.Request {
			return asAdminID(r, "admin-real")
		}, `{"name":"admins-own","user_id":"u1"}`)
		if rec.Code != http.StatusCreated {
			t.Fatalf("create: code=%d body=%s, want 201", rec.Code, rec.Body.String())
		}
		var created keyCreateResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		// 归属自己，而不是请求里指定的 u1。
		if created.UserID != "admin-real" {
			t.Errorf("owner=%q, want admin-real (admin cannot create for others)", created.UserID)
		}
		// 显式指向一个不存在的用户也必须被忽略 —— 不能借它拿到 400 之外的信息，
		// 也不能让「归属校验」这条路径成为可达的分支。
		rec = createKey(h, func(r *http.Request) *http.Request {
			return asAdminID(r, "admin-real")
		}, `{"name":"ghost","user_id":"does-not-exist"}`)
		if rec.Code != http.StatusCreated {
			t.Errorf("unknown user_id: code=%d body=%s, want 201 (ignored)", rec.Code, rec.Body.String())
		}
	})
}

// 身份缺失（中间件没注入）必须 401。
//
// 不靠「拿不到身份就当普通用户发一把无归属 key」来兜底：那会造出一把
// 永远 401 的死物，且等于给匿名开了一条写库路径。
func TestKeyCreate_RequiresLogin(t *testing.T) {
	_, h := newLooseningStore(t)
	rec := createKey(h, func(r *http.Request) *http.Request { return r }, `{"name":"anon"}`)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous create: code=%d body=%s, want 401", rec.Code, rec.Body.String())
	}
	// 空 ID 同样拒绝（fail-closed，与 ownedByCaller 同口径）。
	rec = createKey(h, func(r *http.Request) *http.Request { return asAdminID(r, "") }, `{"name":"noid"}`)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("empty-id create: code=%d body=%s, want 401", rec.Code, rec.Body.String())
	}
}

// 自助发 key 之后，用户仍**不能**自助设分组覆盖。
//
// 这条边界与自助建 key 并不矛盾，判据是「这个字段的方向」：
//   - group_id 只能指向一个组，选宽的那个就是**放宽**（权限提升）；
//   - expires_at / allowed_ips 没有「更宽」这个方向，自设就是给自己加限制。
//
// 所以前者留给管理员（他的收紧手段），后者允许用户自设（也是收紧）。
func TestKeyCreate_GroupOverrideStillAdminOnly(t *testing.T) {
	st, h := newLooseningStore(t)
	if err := st.CreateGroup(context.Background(), &store.Group{ID: "g1", Name: "restricted"}); err != nil {
		t.Fatalf("seed group: %v", err)
	}
	asU := func(r *http.Request) *http.Request { return asUser(r, "u1") }

	rec := createKey(h, asU, `{"name":"try-group","group_id":"g1"}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("user set group: code=%d body=%s, want 403", rec.Code, rec.Body.String())
	}
	if k, _ := st.GetAccessKey(context.Background(), "try-group"); k != nil {
		t.Fatalf("rejected create still wrote a row: %+v", k)
	}

	// 但给**自己**加限制的两个字段是放行的。
	rec = createKey(h, asU, `{"name":"self-restrict","allowed_ips":"10.0.0.0/8","expires_at":`+
		strconv.FormatInt(time.Now().Add(24*time.Hour).UnixMilli(), 10)+`}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("user self-restrict: code=%d body=%s, want 201", rec.Code, rec.Body.String())
	}
	var created keyCreateResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if created.AllowedIPs != "10.0.0.0/8" || created.ExpiresAt == 0 {
		t.Errorf("self-restriction not applied: ips=%q expires=%d", created.AllowedIPs, created.ExpiresAt)
	}
}

// seedAdmin 在库里造一个真实存在的管理员行。
//
// 为什么必须落库：Create 的归属恒等于 me.ID，而 access_keys.user_id 上有
// 外键约束（store.go 的建表语句）—— 身份只在 context 里而库里没这一行，
// 落库时会撞 FOREIGN KEY。用 asAdmin 那个不落库的 admin 身份测 Create
// 会得到 500，读起来像 handler 的 bug，其实只是测试没给身份建行。
func seedAdmin(t *testing.T, st *store.Store, id string) {
	t.Helper()
	if err := st.CreateUser(context.Background(), &store.User{
		ID: id, Username: id, PasswordHash: "x",
		Role: store.RoleAdmin, Status: store.UserStatusActive, AuthVersion: 1,
	}); err != nil {
		t.Fatalf("seed admin: %v", err)
	}
}

// setUserQuota 给一个用户设用户级总额度，并重建快照 ——
// Create 的封顶读的是 snapshot.Get().UsersByID，**不查库**。
//
// 必须重建：只写库不重建的话，handler 读到的仍是旧额度，
// 而测试会「因为期望值恰好等于旧值」而假通过。
//
// 快照是**进程全局单例**（数据面热路径按指针读），所以这里注册 cleanup
// 把它换回空快照：不还原就会顺着 go test 的执行顺序漏给后续用例，
// 让「用户级不限」和「用户级 5000」互相污染 —— 这类污染的报错点
// 永远落在另一个测试上，极难定位。
func setUserQuota(t *testing.T, st *store.Store, userID string, quota int64) {
	t.Helper()
	t.Cleanup(func() { snapshot.Swap(&snapshot.Snapshot{}) })
	ctx := context.Background()
	u, err := st.GetUser(ctx, userID)
	if err != nil || u == nil {
		t.Fatalf("get user %s: %v", userID, err)
	}
	u.QuotaTokens = quota
	if err := st.UpdateUser(ctx, u); err != nil {
		t.Fatalf("update user %s: %v", userID, err)
	}
	snap, err := snapshot.RebuildFromDB(ctx, st, nil)
	if err != nil {
		t.Fatalf("rebuild snapshot: %v", err)
	}
	snapshot.Swap(snap)
}

// P1-5 回归：分组覆盖的**清除**与设置一样是管理员动作。
//
// 原实现的清空分支排在管理员判定之前，管理员把 key 压到更严的组后，
// 用户一条 {"group_id":""} 就能退回归属用户的（更宽松）分组。
func TestKeyUpdate_OwnerCannotClearGroupOverride(t *testing.T) {
	st, h := newLooseningStore(t)
	if err := st.CreateGroup(context.Background(), &store.Group{
		ID: "g-strict", Name: "strict",
	}); err != nil {
		t.Fatalf("seed group: %v", err)
	}
	k := seedRestrictedKey(t, st)
	k.GroupID = "g-strict"
	if err := st.UpdateAccessKey(context.Background(), "kr", k); err != nil {
		t.Fatalf("seed group override: %v", err)
	}

	rec := patchKey(h, func(r *http.Request) *http.Request { return asUser(r, "u1") }, `{"group_id":""}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("clear group override: code=%d body=%s, want 403", rec.Code, rec.Body.String())
	}
	got, _ := st.GetAccessKey(context.Background(), "kr")
	if got.GroupID != "g-strict" {
		t.Fatalf("group override was cleared: %q", got.GroupID)
	}

	rec = patchKey(h, asAdmin, `{"group_id":""}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin clear group override: code=%d body=%s, want 200", rec.Code, rec.Body.String())
	}
	got, _ = st.GetAccessKey(context.Background(), "kr")
	if got.GroupID != "" {
		t.Fatalf("admin clear did not take effect: %q", got.GroupID)
	}
}
