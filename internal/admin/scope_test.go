package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cn-maul/rosetta-gateway/internal/store"
	"github.com/cn-maul/rosetta-gateway/internal/userauth"
)

func newScopeStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "gw.db"),
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// seedUsage 造一条用量记录。
func seedUsageRow(t *testing.T, st *store.Store, id, keyID, userID string, tokens int) {
	t.Helper()
	var uid any
	if userID != "" {
		uid = userID
	}
	_, err := st.DB().Exec(`INSERT INTO usage_records
		(id, ts, access_key_id, user_id, public_model, provider_id, upstream_model, ingress_protocol,
		 stream, input_tokens, output_tokens, total_tokens, usage_state, status, http_status, latency_ms)
		VALUES (?,?,?,?, 'm','p','up','openai-chat', 0, ?,0, ?, 'reported','ok',200,10)`,
		id, time.Now().UnixMilli(), keyID, uid, tokens, tokens)
	if err != nil {
		t.Fatalf("seed usage %s: %v", id, err)
	}
}

func seedTwoUsers(t *testing.T, st *store.Store) {
	t.Helper()
	ctx := context.Background()
	for _, u := range []struct{ id, name string }{{"u1", "alice"}, {"u2", "bob"}} {
		if err := st.CreateUser(ctx, &store.User{
			ID: u.id, Username: u.name, PasswordHash: "x",
			Role: store.RoleUser, Status: store.UserStatusActive, AuthVersion: 1,
		}); err != nil {
			t.Fatalf("create %s: %v", u.id, err)
		}
	}
	for _, k := range []struct{ id, user string }{{"k1", "u1"}, {"k2", "u2"}} {
		if err := st.CreateAccessKey(ctx, &store.AccessKey{
			ID: k.id, KeyHash: "h" + k.id, KeyPrefix: "sk-", Name: k.id, Enabled: true, UserID: k.user,
		}); err != nil {
			t.Fatalf("create %s: %v", k.id, err)
		}
	}
}

// ---- 用量作用域收窄（数据泄露防线）----

// 普通用户查用量只能看到自己的。这是多用户改造最要命的一类漏洞：
// 改造前 /admin/api/usage 返回全局数据。
func TestUsage_ScopedToOwnUser(t *testing.T) {
	st := newScopeStore(t)
	seedTwoUsers(t, st)
	seedUsageRow(t, st, "e1", "k1", "u1", 100)
	seedUsageRow(t, st, "e2", "k1", "u1", 50)
	seedUsageRow(t, st, "e3", "k2", "u2", 9999)

	h := NewUsageHandler(st)

	// alice 查：只应看到自己的 150
	rec := httptest.NewRecorder()
	req := asUser(httptest.NewRequest(http.MethodGet, "/admin/api/usage?limit=100", nil), "u1")
	h.Query(rec, req)

	var resp struct {
		Summary struct {
			TotalRequests int64 `json:"total_requests"`
			TotalTokens   int64 `json:"total_tokens"`
		} `json:"summary"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v (body=%s)", err, rec.Body.String())
	}
	if resp.Summary.TotalTokens != 150 {
		t.Errorf("alice sees %d tokens, want 150 (bob's 9999 leaked!)", resp.Summary.TotalTokens)
	}
	if resp.Summary.TotalRequests != 2 {
		t.Errorf("alice sees %d requests, want 2", resp.Summary.TotalRequests)
	}
}

// 普通用户即使显式传别人的 key_id，也只能拿到空集。
// 若权限过滤写在用户过滤之后，AND 可交换，所以这条其实自动成立 ——
// 但它是最容易被「优化」掉的顺序，值得钉住。
func TestUsage_CannotPeekOthersViaKeyIDParam(t *testing.T) {
	st := newScopeStore(t)
	seedTwoUsers(t, st)
	seedUsageRow(t, st, "e3", "k2", "u2", 9999)

	h := NewUsageHandler(st)
	rec := httptest.NewRecorder()
	req := asUser(httptest.NewRequest(http.MethodGet, "/admin/api/usage?key_id=k2&limit=100", nil), "u1")
	h.Query(rec, req)

	body := rec.Body.String()
	if strings.Contains(body, "9999") {
		t.Errorf("alice saw bob's usage by passing key_id=k2: %s", body)
	}
}

// TestUsage_FractionalLatencyDoesNot500 钉住一条 P1 的修复。
//
// 平均延迟只要不是整数，`/admin/api/usage` 过去就整个 500：
// UsageRatioExpr 的 `* 1.0` 与 groupByClause 的 AVG() 都产出 REAL，
// 而扫描目标是 int64，database/sql 走 FormatFloat→ParseInt 遇小数即失败。
// 而且 summarize 在明细查询之前跑，所以连一条记录都读不出来。
//
// 套件此前抓不到它，因为 seedUsageRow 把每行 latency_ms 固定成 10，
// 任何条数的平均值都恰好整除。
func TestUsage_FractionalLatencyDoesNot500(t *testing.T) {
	st := newScopeStore(t)
	seedTwoUsers(t, st)
	// 10 + 10 + 15 = 35 / 3 = 11.666… 必然是小数
	for i, latency := range []int{10, 10, 15} {
		_, err := st.DB().Exec(`INSERT INTO usage_records
			(id, ts, access_key_id, user_id, public_model, provider_id, upstream_model, ingress_protocol,
			 stream, input_tokens, output_tokens, total_tokens, usage_state, status, http_status, latency_ms)
			VALUES (?,?,?,?, 'm','p','up','openai-chat', 0, 10,2,12, 'reported','ok',200,?)`,
			fmt.Sprintf("f%d", i), time.Now().UnixMilli(), "k1", "u1", latency)
		if err != nil {
			t.Fatalf("seed latency=%d: %v", latency, err)
		}
	}

	h := NewUsageHandler(st)
	for _, q := range []string{
		"/admin/api/usage?limit=100",
		"/admin/api/usage?group_by=key",
		"/admin/api/usage?group_by=model",
		"/admin/api/usage?group_by=day",
	} {
		t.Run(q, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.Query(rec, asUser(httptest.NewRequest(http.MethodGet, q, nil), "u1"))
			if rec.Code != http.StatusOK {
				t.Fatalf("%s -> %d, 期望 200（body=%s）", q, rec.Code, rec.Body.String())
			}
			var resp struct {
				Summary struct {
					TotalRequests int64   `json:"total_requests"`
					AvgLatencyMs  float64 `json:"avg_latency_ms"`
				} `json:"summary"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatalf("unmarshal: %v (body=%s)", err, rec.Body.String())
			}
			if resp.Summary.TotalRequests != 3 {
				t.Fatalf("total_requests=%d, 期望 3（记录一条都不能少）", resp.Summary.TotalRequests)
			}
			// 11.666… 四舍五入成整数毫秒，对外契约仍是整数
			if resp.Summary.AvgLatencyMs != 12 {
				t.Fatalf("avg_latency_ms=%v, 期望 12（35/3 取整）", resp.Summary.AvgLatencyMs)
			}
		})
	}
}

// 分组端点（by-key）同样必须收窄 —— 它是另一条WHERE 构造路径。
func TestUsage_GroupByKey_Scoped(t *testing.T) {
	st := newScopeStore(t)
	seedTwoUsers(t, st)
	seedUsageRow(t, st, "e1", "k1", "u1", 100)
	seedUsageRow(t, st, "e3", "k2", "u2", 9999)

	h := NewUsageHandler(st)
	rec := httptest.NewRecorder()
	h.GroupByKey(rec, asUser(httptest.NewRequest(http.MethodGet, "/admin/api/usage/by-key", nil), "u1"))

	body := rec.Body.String()
	if strings.Contains(body, "k2") {
		t.Errorf("by-key leaked bob's key: %s", body)
	}
	if !strings.Contains(body, "k1") {
		t.Errorf("by-key lost alice's own key: %s", body)
	}
}

// 历史明细端点必须收窄 —— 明细里有模型名与时间戳，泄露更严重。
func TestUsage_History_Scoped(t *testing.T) {
	st := newScopeStore(t)
	seedTwoUsers(t, st)
	seedUsageRow(t, st, "e1", "k1", "u1", 100)
	seedUsageRow(t, st, "e3", "k2", "u2", 9999)

	h := NewUsageHandler(st)
	rec := httptest.NewRecorder()
	h.History(rec, asUser(httptest.NewRequest(http.MethodGet, "/admin/api/usage/history?limit=100", nil), "u1"))

	body := rec.Body.String()
	if strings.Contains(body, "9999") {
		t.Errorf("history leaked bob's records: %s", body)
	}
}

// admin 看到全量 —— 收窄不能把管理员也挡住，否则改造就没意义了。
func TestUsage_AdminSeesEverything(t *testing.T) {
	st := newScopeStore(t)
	seedTwoUsers(t, st)
	seedUsageRow(t, st, "e1", "k1", "u1", 100)
	seedUsageRow(t, st, "e3", "k2", "u2", 9999)

	h := NewUsageHandler(st)
	rec := httptest.NewRecorder()
	h.Query(rec, asAdmin(httptest.NewRequest(http.MethodGet, "/admin/api/usage?limit=100", nil)))

	body := rec.Body.String()
	if !strings.Contains(body, "9999") {
		t.Errorf("admin cannot see all usage (scope too narrow): %s", body)
	}
}

// 未鉴权请求（无身份）必须什么都看不到。
// callerScope 返回哨兵值而非空串 —— 空串是 admin 语义，会全量泄露。
func TestUsage_NoIdentitySeesNothing(t *testing.T) {
	st := newScopeStore(t)
	seedTwoUsers(t, st)
	seedUsageRow(t, st, "e3", "k2", "u2", 9999)

	h := NewUsageHandler(st)
	rec := httptest.NewRecorder()
	h.Query(rec, httptest.NewRequest(http.MethodGet, "/admin/api/usage?limit=100", nil))

	if strings.Contains(rec.Body.String(), "9999") {
		t.Errorf("unauthenticated request saw data: %s", rec.Body.String())
	}
}

// ---- key 作用域收窄 ----

// 普通用户只看到自己的 key。
func TestKeyList_ScopedToOwnUser(t *testing.T) {
	st := newScopeStore(t)
	seedTwoUsers(t, st)
	h := NewKeyHandler(st)

	rec := httptest.NewRecorder()
	h.List(rec, asUser(httptest.NewRequest(http.MethodGet, "/admin/api/keys", nil), "u1"))

	var keys []keyResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &keys); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(keys) != 1 || keys[0].UserID != "u1" {
		t.Errorf("alice sees %+v, want only her own key", keys)
	}
}

// 普通用户改别人的 key 必须 404（而不是 200/403）。
func TestKeyUpdate_RejectsOthersKey(t *testing.T) {
	st := newScopeStore(t)
	seedTwoUsers(t, st)
	h := NewKeyHandler(st)

	rec := httptest.NewRecorder()
	req := asUser(jsonRequest(http.MethodPatch, "/admin/api/keys/k2",
		strings.NewReader(`{"quota_tokens":1}`)), "u1")
	h.Update(rec, req, "k2")

	if rec.Code != http.StatusNotFound {
		t.Errorf("alice updating bob's key: code=%d, want 404 (body=%s)", rec.Code, rec.Body.String())
	}
	// 关键：确认真的没改
	k, _ := st.GetAccessKey(context.Background(), "k2")
	if k.QuotaTokens != 0 {
		t.Errorf("bob's key quota was changed to %d by alice", k.QuotaTokens)
	}
}

// 普通用户删别人的 key 必须失败。
func TestKeyDelete_RejectsOthersKey(t *testing.T) {
	st := newScopeStore(t)
	seedTwoUsers(t, st)
	h := NewKeyHandler(st)

	rec := httptest.NewRecorder()
	h.Delete(rec, asUser(httptest.NewRequest(http.MethodDelete, "/admin/api/keys/k2", nil), "u1"), "k2")

	if rec.Code != http.StatusNotFound {
		t.Errorf("alice deleting bob's key: code=%d, want 404", rec.Code)
	}
	if k, _ := st.GetAccessKey(context.Background(), "k2"); k == nil {
		t.Error("bob's key was actually deleted by alice")
	}
}

// 普通用户改别人的 key 拿到的必须是 404 而不是 403 ——
// 403 会确认「这把 key 存在」，本身也是信息（可枚举他人 key 的 id）。
func TestKeyUpdate_OthersKeyIs404Not403(t *testing.T) {
	st := newScopeStore(t)
	seedTwoUsers(t, st)
	h := NewKeyHandler(st)

	rec := httptest.NewRecorder()
	req := asUser(jsonRequest(http.MethodPatch, "/admin/api/keys/k2",
		strings.NewReader(`{"quota_tokens":1}`)), "u1")
	h.Update(rec, req, "k2")

	if strings.Contains(rec.Body.String(), "不是你的") {
		t.Errorf("response reveals existence of another user's key: %s", rec.Body.String())
	}
}

// ---- 用户管理权限 ----

// 普通用户不能列用户。
func TestListUsers_NonAdminForbidden(t *testing.T) {
	st := newScopeStore(t)
	seedTwoUsers(t, st)
	h := NewUserHandler(st, nil)

	rec := httptest.NewRecorder()
	h.ListUsers(rec, asUser(httptest.NewRequest(http.MethodGet, "/admin/api/users", nil), "u1"))

	if rec.Code != http.StatusForbidden {
		t.Errorf("non-admin listing users: code=%d, want 403", rec.Code)
	}
}

// 管理员不能把自己降级或禁用 —— 那会把自己锁在门外。
func TestUpdateUser_CannotSelfDemoteOrDisable(t *testing.T) {
	st := newScopeStore(t)
	ctx := context.Background()
	seedTwoUsers(t, st)
	if err := st.CreateUser(ctx, &store.User{
		ID: "admin1", Username: "root", PasswordHash: "x",
		Role: store.RoleAdmin, Status: store.UserStatusActive, AuthVersion: 1,
	}); err != nil {
		t.Fatalf("create admin: %v", err)
	}
	h := NewUserHandler(st, nil)

	for _, body := range []string{`{"role":"user"}`, `{"status":"disabled"}`} {
		rec := httptest.NewRecorder()
		// 身份 id 必须是 admin1 —— 与操作目标一致，否则测的是
		//「改别人」而不是「改自己」，防护不会触发，测试假通过。
		req := asAdminID(jsonRequest(http.MethodPatch, "/admin/api/users/admin1",
			strings.NewReader(body)), "admin1")
		h.UpdateUser(rec, req, "admin1")
		if rec.Code != http.StatusBadRequest {
			t.Errorf("self-%s: code=%d, want 400 (body=%s)", body, rec.Code, rec.Body.String())
		}
	}
	// 确认没被改
	u, _ := st.GetUser(ctx, "admin1")
	if !u.IsAdmin() || !u.IsActive() {
		t.Errorf("admin locked themselves out: role=%s status=%s", u.Role, u.Status)
	}
}

// 管理员不能删自己。
func TestDeleteUser_CannotSelfDelete(t *testing.T) {
	st := newScopeStore(t)
	ctx := context.Background()
	seedTwoUsers(t, st)
	if err := st.CreateUser(ctx, &store.User{
		ID: "admin1", Username: "root", PasswordHash: "x",
		Role: store.RoleAdmin, Status: store.UserStatusActive, AuthVersion: 1,
	}); err != nil {
		t.Fatalf("create admin: %v", err)
	}
	h := NewUserHandler(st, nil)

	rec := httptest.NewRecorder()
	h.DeleteUser(rec,
		asAdminID(httptest.NewRequest(http.MethodDelete, "/admin/api/users/admin1", nil), "admin1"),
		"admin1")

	if rec.Code != http.StatusBadRequest {
		t.Errorf("self-delete: code=%d, want 400", rec.Code)
	}
	if u, _ := st.GetUser(ctx, "admin1"); u == nil {
		t.Error("admin actually deleted themselves")
	}
}

// 禁用账号必须递增 auth_version —— 否则旧会话仍然有效。
func TestUpdateUser_DisableBumpsAuthVersion(t *testing.T) {
	st := newScopeStore(t)
	ctx := context.Background()
	seedTwoUsers(t, st)
	h := NewUserHandler(st, nil)

	rec := httptest.NewRecorder()
	req := asAdmin(jsonRequest(http.MethodPatch, "/admin/api/users/u1",
		strings.NewReader(`{"status":"disabled"}`)))
	h.UpdateUser(rec, req, "u1")

	if rec.Code != http.StatusOK {
		t.Fatalf("disable failed: %d %s", rec.Code, rec.Body.String())
	}
	u, _ := st.GetUser(ctx, "u1")
	if u.AuthVersion <= 1 {
		t.Errorf("auth_version = %d, want >1 so old sessions are invalidated", u.AuthVersion)
	}
}

// 用户名重名必须报 409 而不是 500。
func TestCreateUser_DuplicateIsConflict(t *testing.T) {
	st := newScopeStore(t)
	seedTwoUsers(t, st)
	h := NewUserHandler(st, nil)

	rec := httptest.NewRecorder()
	req := asAdmin(jsonRequest(http.MethodPost, "/admin/api/users",
		strings.NewReader(`{"username":"alice","password":"Str0ngPassw0rd!"}`)))
	h.CreateUser(rec, req)

	if rec.Code != http.StatusConflict {
		t.Errorf("duplicate username: code=%d, want 409 (body=%s)", rec.Code, rec.Body.String())
	}
}

// 弱密码必须被拒。
func TestCreateUser_RejectsWeakPassword(t *testing.T) {
	st := newScopeStore(t)
	h := NewUserHandler(st, nil)

	rec := httptest.NewRecorder()
	req := asAdmin(jsonRequest(http.MethodPost, "/admin/api/users",
		strings.NewReader(`{"username":"carol","password":"123"}`)))
	h.CreateUser(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("weak password: code=%d, want 400", rec.Code)
	}
}

// 非法用户名（含中文/空格）必须被拒 —— 见 validUsername 的理由。
func TestCreateUser_RejectsBadUsername(t *testing.T) {
	st := newScopeStore(t)
	h := NewUserHandler(st, nil)

	for _, name := range []string{"ab", "中文名", "has space", "a/b"} {
		rec := httptest.NewRecorder()
		req := asAdmin(jsonRequest(http.MethodPost, "/admin/api/users",
			strings.NewReader(`{"username":"`+name+`"}`)))
		h.CreateUser(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("username %q accepted: code=%d, want 400", name, rec.Code)
		}
	}
}

// 密码可以留空（建号后由本人设置）—— 这是引导流程的一部分。
func TestCreateUser_AllowsEmptyPassword(t *testing.T) {
	st := newScopeStore(t)
	h := NewUserHandler(st, nil)

	rec := httptest.NewRecorder()
	req := asAdmin(jsonRequest(http.MethodPost, "/admin/api/users",
		strings.NewReader(`{"username":"dave"}`)))
	h.CreateUser(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("create with empty password: code=%d body=%s", rec.Code, rec.Body.String())
	}
	var u userResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &u)
	if u.HasPassword {
		t.Error("HasPassword should be false for a user created without one")
	}
}

// ---- 登录 ----

// 会话未启用（没配密钥）时登录必须 503，不能放行。
func TestLogin_SessionsDisabled(t *testing.T) {
	st := newScopeStore(t)
	seedTwoUsers(t, st)
	h := NewUserHandler(st, nil) // mgr 为 nil = 未启用

	rec := httptest.NewRecorder()
	req := jsonRequest(http.MethodPost, "/admin/api/login",
		strings.NewReader(`{"username":"alice","password":"whatever"}`))
	h.Login(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("login with sessions disabled: code=%d, want 503", rec.Code)
	}
}

// 引导账号（无密码）不能登录，且**必须明确告诉用户去设置密码**。
//
// 改动前这条只断言 `!= 200`：空哈希在 VerifyPassword 里直接返回 false，
// 于是请求落到通用分支、回「用户名或密码错误」，测试照样绿 ——
// 但那是个假阴性：用户看到的是「密码错了」，于是反复重试。
// 现在断言状态码 + 文案关键词，锁住的是「提示是否可执行」而不是「没登上」。
func TestLogin_BootstrapAccountCannotLogin(t *testing.T) {
	st := newScopeStore(t)
	ctx := context.Background()
	if err := st.CreateUser(ctx, &store.User{
		ID: "u1", Username: "admin", PasswordHash: "",
		Role: store.RoleAdmin, Status: store.UserStatusActive, AuthVersion: 1,
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	mgr := newTestManager(t)
	h := NewUserHandler(st, mgr)

	// 两种输入都要覆盖：空密码被 400「必填」提前拦掉，
	// 任意密码才会走到那个「尚未设置密码」的分支。
	for _, body := range []string{
		`{"username":"admin","password":""}`,
		`{"username":"admin","password":"whatever-guess"}`,
	} {
		rec := httptest.NewRecorder()
		req := jsonRequest(http.MethodPost, "/admin/api/login", strings.NewReader(body))
		h.Login(rec, req)

		if rec.Code == http.StatusOK {
			t.Fatalf("bootstrap account logged in: %s", body)
		}
		var r map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &r)
		e, _ := r["error"].(map[string]any)
		msg, _ := e["message"].(string)
		if body == `{"username":"admin","password":""}` {
			// 空密码属于参数错误，提示「必填」就够了。
			if !strings.Contains(msg, "必填") {
				t.Errorf("空密码应提示必填，得到 %q", msg)
			}
			continue
		}
		if rec.Code != http.StatusBadRequest {
			t.Errorf("status=%d（应为 400）", rec.Code)
		}
		if !strings.Contains(msg, "尚未设置密码") {
			t.Errorf("引导账号必须收到「尚未设置密码」的指引，得到 %q —— "+
				"「用户名或密码错误」会让用户以为密码输错了并反复重试", msg)
		}
	}
}

// 用户不存在与密码错误必须回**同样的状态码与文案** ——
// 区分二者等于给攻击者一个枚举用户名的 oracle。
func TestLogin_NoUserEnumeration(t *testing.T) {
	st := newScopeStore(t)
	seedTwoUsers(t, st)
	mgr := newTestManager(t)
	h := NewUserHandler(st, mgr)

	get := func(body string) (int, string) {
		rec := httptest.NewRecorder()
		h.Login(rec, jsonRequest(http.MethodPost, "/admin/api/login", strings.NewReader(body)))
		var r map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &r)
		e, _ := r["error"].(map[string]any)
		msg, _ := e["message"].(string)
		return rec.Code, msg
	}

	codeGhost, msgGhost := get(`{"username":"ghost","password":"Str0ngPassw0rd!"}`)
	codeWrong, msgWrong := get(`{"username":"alice","password":"wrong-password"}`)

	if codeGhost != codeWrong {
		t.Errorf("status differs: nonexistent=%d wrong-password=%d (enumeration oracle!)",
			codeGhost, codeWrong)
	}
	if msgGhost != msgWrong {
		t.Errorf("message differs: %q vs %q (enumeration oracle!)", msgGhost, msgWrong)
	}
}

func newTestManager(t *testing.T) *userauth.Manager {
	t.Helper()
	t.Setenv(userauth.SecretEnvName, "test-secret-that-is-at-least-32-characters-long")
	m, err := userauth.NewManager("")
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	if m == nil {
		t.Fatal("manager is nil; test env not applied")
	}
	return m
}
