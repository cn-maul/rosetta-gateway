package store

import (
	"context"
	"database/sql"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// usageSeedColumns 是往usage_records 塞测试数据时用的显式列清单。
//
// 显式写出全部列名是刻意的：早先版本用
// `INSERT INTO usage_records (id, ts, ...)` 漏列，NOT NULL 约束报错后
// 反复数 VALUES 个数，数错三次。与其数不如列全 —— 少写的列由默认值兜住。
const usageSeedColumns = `id, ts, access_key_id, user_id, public_model, provider_id,
	upstream_model, ingress_protocol, stream, total_tokens, usage_state, status, http_status, latency_ms`

func openAt(t *testing.T, path string) *Store {
	t.Helper()
	st, err := Open(path, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func newStore(t *testing.T) *Store {
	return openAt(t, filepath.Join(t.TempDir(), "gw.db"))
}

// seedUsage 插一条用量记录。参数给不齐的列用零值/默认值。
func seedUsage(t *testing.T, db *Store, id, keyID, userID string, tokens int) {
	t.Helper()
	var uid any
	if userID != "" {
		uid = userID
	}
	_, err := db.db.ExecContext(context.Background(),
		`INSERT INTO usage_records (`+usageSeedColumns+`)
		 VALUES (?, ?, ?, ?, 'm', 'p', 'up', 'openai-chat', 0, ?, 'reported', 'ok', 200, 10)`,
		id, time.Now().UnixMilli(), keyID, uid, tokens)
	if err != nil {
		t.Fatalf("seed usage %s: %v", id, err)
	}
}

// ---- 迁移兼容性（最重要的一组）----

// 老库升级：先造一个「改造前」的 access_keys 行（还没有 user_id 列），
// 再用同一文件重开，验证迁移把它变成 user_id 为 NULL 的无归属 key
// 且**读取不报错**。
func TestMigration_ExistingKeyBecomesOrphan(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gw.db")
	ctx := context.Background()

	// 第一次打开：模拟老库（schema 里还没有 user_id）。
	first := openAt(t, path)
	if _, err := first.db.Exec(`INSERT INTO access_keys
		(id, key_hash, key_prefix, name, enabled, quota_tokens, used_tokens, rpm_limit, tpm_limit, created_at)
		VALUES ('k1','hash1','sk-gw-abc','legacy',1,0,0,0,0,?)`, time.Now().UnixMilli()); err != nil {
		t.Fatalf("seed legacy key: %v", err)
	}
	first.Close()

	// 第二次打开：走迁移路径。
	second := openAt(t, path)

	k, err := second.GetAccessKey(ctx, "k1")
	if err != nil {
		t.Fatalf("read migrated key: %v", err)
	}
	if k == nil {
		t.Fatal("migrated key disappeared")
	}
	// 关键：必须是空串（不是字符串 "NULL"，也不能读不出来）。
	// 空串是「无归属」的唯一表示，DAO 层必须精确还原它 —— 迁移退役
	// （retireOrphanKeys）与鉴权拒绝（ErrKeyUnowned）都以此为条件，
	// 读成 "NULL" 会让退役漏掉这批 key。
	if k.UserID != "" {
		t.Errorf("UserID = %q, want empty for orphan key", k.UserID)
	}
}

// user_id 在库里确实是 NULL，但 DAO 读出来必须是空串。
func TestUserIDColumnIsNullableAndReadsAsEmpty(t *testing.T) {
	db := newStore(t)
	if _, err := db.db.Exec(`INSERT INTO access_keys
		(id, key_hash, key_prefix, name, enabled, quota_tokens, used_tokens, rpm_limit, tpm_limit, created_at)
		VALUES ('k1','h','sk-','n',1,0,0,0,0,?)`, time.Now().UnixMilli()); err != nil {
		t.Fatalf("seed: %v", err)
	}
	var raw sql.NullString
	if err := db.read.QueryRow(`SELECT user_id FROM access_keys WHERE id='k1'`).Scan(&raw); err != nil {
		t.Fatalf("raw read: %v", err)
	}
	if raw.Valid {
		t.Errorf("user_id should be NULL for a key created without owner, got %q", raw.String)
	}
	k, err := db.GetAccessKey(context.Background(), "k1")
	if err != nil {
		t.Fatalf("dao read: %v", err)
	}
	if k.UserID != "" {
		t.Errorf("dao UserID = %q, want empty", k.UserID)
	}
}

// 存量用量记录升级后必须保留，统计口径不变。
func TestMigration_ExistingUsageRowsSurvive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gw.db")
	ctx := context.Background()

	first := openAt(t, path)
	if _, err := first.db.Exec(`INSERT INTO usage_records
		(id, ts, access_key_id, public_model, provider_id, upstream_model, ingress_protocol, stream,
		 input_tokens, output_tokens, total_tokens, usage_state, status, http_status, latency_ms)
		VALUES ('u1',?, 'k1','m','p','up','openai-chat',0, 10,20,30,'reported','ok',200,100)`,
		time.Now().UnixMilli()); err != nil {
		t.Fatalf("seed legacy usage: %v", err)
	}
	first.Close()

	second := openAt(t, path)
	stats, err := second.GetUsageStats(ctx, 0, 0, "")
	if err != nil {
		t.Fatalf("stats after migration: %v", err)
	}
	if stats.TotalRequests != 1 || stats.TotalTokens != 30 {
		t.Errorf("legacy usage lost: requests=%d tokens=%d", stats.TotalRequests, stats.TotalTokens)
	}
}

// ---- key 归属与作用域 ----

func TestAccessKeys_UserScoping(t *testing.T) {
	db := newStore(t)
	ctx := context.Background()
	for _, u := range []struct{ id, name string }{{"u1", "alice"}, {"u2", "bob"}} {
		if err := db.CreateUser(ctx, &User{ID: u.id, Username: u.name}); err != nil {
			t.Fatalf("create %s: %v", u.id, err)
		}
	}
	mk := func(id, userID string) {
		if err := db.CreateAccessKey(ctx, &AccessKey{
			ID: id, KeyHash: "h" + id, KeyPrefix: "sk-", Name: id, Enabled: true, UserID: userID,
		}); err != nil {
			t.Fatalf("create key %s: %v", id, err)
		}
	}
	mk("ka", "u1")
	mk("kb", "u2")
	mk("kc", "") // 孤儿

	alice, err := db.ListAccessKeysByUser(ctx, "u1")
	if err != nil {
		t.Fatalf("list alice: %v", err)
	}
	if len(alice) != 1 || alice[0].ID != "ka" {
		t.Errorf("alice sees %d keys, want just ka", len(alice))
	}

	// userID 为空必须返回**空切片**，不是全部 ——
	// 否则漏传 userID 的调用会把所有 key（含别人的）吐出来。
	none, err := db.ListAccessKeysByUser(ctx, "")
	if err != nil {
		t.Fatalf("list by empty user: %v", err)
	}
	if len(none) != 0 {
		t.Errorf("empty userID returned %d keys, want 0", len(none))
	}

	all, err := db.ListAccessKeys(ctx)
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	if len(all) != 3 {
		t.Errorf("ListAccessKeys = %d, want 3 (admin view sees everything)", len(all))
	}
}

// UpdateAccessKey 不得改 user_id —— 否则一个改名字的 PATCH 就能把别人的 key 划走。
func TestUpdateAccessKey_DoesNotChangeOwner(t *testing.T) {
	db := newStore(t)
	ctx := context.Background()
	if err := db.CreateUser(ctx, &User{ID: "u1", Username: "alice"}); err != nil {
		t.Fatalf("create user: %v", err)
	}
	if err := db.CreateAccessKey(ctx, &AccessKey{
		ID: "k1", KeyHash: "h1", KeyPrefix: "sk-", Name: "orig", Enabled: true, UserID: "u1",
	}); err != nil {
		t.Fatalf("create key: %v", err)
	}

	k, _ := db.GetAccessKey(ctx, "k1")
	k.Name = "renamed"
	k.UserID = "someone-else" // 恶意/错误的尝试
	if err := db.UpdateAccessKey(ctx, "k1", k); err != nil {
		t.Fatalf("update: %v", err)
	}

	after, _ := db.GetAccessKey(ctx, "k1")
	if after.UserID != "u1" {
		t.Errorf("owner changed to %q via a name update; must stay u1", after.UserID)
	}
	if after.Name != "renamed" {
		t.Errorf("name not updated: %q", after.Name)
	}
}

// ---- 用户生命周期 ----

func TestUser_DisableTakesEffect(t *testing.T) {
	db := newStore(t)
	ctx := context.Background()
	if err := db.CreateUser(ctx, &User{ID: "u1", Username: "alice", Status: UserStatusActive, AuthVersion: 1}); err != nil {
		t.Fatalf("create: %v", err)
	}
	u, _ := db.GetUser(ctx, "u1")
	if !u.IsActive() {
		t.Fatal("fresh user should be active")
	}
	// 复刻 handler 里真实的禁用路径：UpdateUser 改状态、BumpAuthVersion 让
	// 既有会话失效 —— UpdateUser 从不写 auth_version，两步职责分开
	//（见 TestUpdateUser_DoesNotRewindAuthVersion）。
	if err := db.UpdateUser(ctx, &User{
		ID: "u1", Username: "alice", Role: RoleUser,
		Status: UserStatusDisabled,
	}); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if err := db.BumpAuthVersion(ctx, "u1"); err != nil {
		t.Fatalf("bump auth version: %v", err)
	}
	got, _ := db.GetUser(ctx, "u1")
	if got.IsActive() {
		t.Error("user still active after disable")
	}
	if got.AuthVersion != 2 {
		t.Errorf("auth_version = %d, want 2 (disable must invalidate existing sessions)", got.AuthVersion)
	}
}

// UpdateUser 绝不把 auth_version 写回旧值。
//
// 旧 SQL 无条件写 auth_version = 快照值：handler 读库（av=5）→ 用户并发改密
// （SetUserPassword 原子递增到 av=6）→ PATCH 落库把 av 覆盖回 5 → 改密前被
// 窃取的旧 JWT 重新通过校验，吊销被静默撤销。修复后资料更新永不触碰
// 会话失效栅栏 —— 栅栏只能前进（SetUserPassword / BumpAuthVersion 的相对递增）。
func TestUpdateUser_DoesNotRewindAuthVersion(t *testing.T) {
	db := newStore(t)
	ctx := context.Background()
	if err := db.CreateUser(ctx, &User{
		ID: "u1", Username: "alice", DisplayName: "Alice", AuthVersion: 5,
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	snap, err := db.GetUser(ctx, "u1") // handler 持有的旧快照
	if err != nil || snap == nil {
		t.Fatalf("get: %v", err)
	}

	// 读库与落库之间发生的并发改密。
	if err := db.SetUserPassword(ctx, "u1", "new-hash"); err != nil {
		t.Fatalf("set password: %v", err)
	}

	// 用旧快照 PATCH 落库，只改 display_name。
	snap.DisplayName = "Alice Renamed"
	if err := db.UpdateUser(ctx, snap); err != nil {
		t.Fatalf("update: %v", err)
	}

	got, _ := db.GetUser(ctx, "u1")
	if got.AuthVersion != 6 {
		t.Errorf("auth_version = %d, want 6 (a profile PATCH must not rewind the bump made by SetUserPassword)", got.AuthVersion)
	}
	if got.DisplayName != "Alice Renamed" {
		t.Errorf("display_name = %q, want \"Alice Renamed\" (profile fields must still update)", got.DisplayName)
	}
}

// 改密码必须同时递增 auth_version —— 否则旧 JWT 在改密后仍然有效。
func TestSetUserPassword_BumpsAuthVersion(t *testing.T) {
	db := newStore(t)
	ctx := context.Background()
	if err := db.CreateUser(ctx, &User{ID: "u1", Username: "alice", PasswordHash: "old", AuthVersion: 3}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := db.SetUserPassword(ctx, "u1", "new-hash"); err != nil {
		t.Fatalf("set password: %v", err)
	}
	u, _ := db.GetUser(ctx, "u1")
	if u.PasswordHash != "new-hash" {
		t.Errorf("password not updated: %q", u.PasswordHash)
	}
	if u.AuthVersion != 4 {
		t.Errorf("auth_version = %d, want 4 (must bump to invalidate old sessions)", u.AuthVersion)
	}
}

// SetInitialAdminPassword 只对「未设密码的管理员」生效**一次**。
//
// 这是 bootstrap「设过即 409」防线的落点：若条件 UPDATE 退化成先查后写，
// 两个并发请求都能通过判定、后写者覆盖先写者。测试钉住三个边界 ——
// 第二次调用必须 no-op、普通用户的空密码账号不可经此初始化、未知 id no-op。
func TestSetInitialAdminPassword_OnlyOnce(t *testing.T) {
	db := newStore(t)
	ctx := context.Background()
	if err := db.CreateUser(ctx, &User{ID: "a1", Username: "admin", PasswordHash: "", Role: RoleAdmin, AuthVersion: 1}); err != nil {
		t.Fatalf("create admin: %v", err)
	}
	if err := db.CreateUser(ctx, &User{ID: "u1", Username: "alice", PasswordHash: "", Role: RoleUser, AuthVersion: 1}); err != nil {
		t.Fatalf("create user: %v", err)
	}

	ok, err := db.SetInitialAdminPassword(ctx, "a1", "hash-1")
	if err != nil || !ok {
		t.Fatalf("first setup: ok=%v err=%v, want true", ok, err)
	}
	// 第二次必须 no-op：既不改哈希，也不再递增 auth_version。
	ok, err = db.SetInitialAdminPassword(ctx, "a1", "hash-attacker")
	if err != nil {
		t.Fatalf("second setup: %v", err)
	}
	if ok {
		t.Fatal("second setup reported success — the one-shot window is not closed")
	}
	u, _ := db.GetUser(ctx, "a1")
	if u.PasswordHash != "hash-1" {
		t.Errorf("password was overwritten by second call: %q", u.PasswordHash)
	}
	if u.AuthVersion != 2 {
		t.Errorf("auth_version = %d, want 2 (bumped exactly once)", u.AuthVersion)
	}

	// 普通用户的空密码账号：不是 bootstrap 的目标，绝不能经此初始化。
	ok, err = db.SetInitialAdminPassword(ctx, "u1", "hash-x")
	if err != nil {
		t.Fatalf("non-admin setup: %v", err)
	}
	if ok {
		t.Fatal("non-admin empty-password account was initialized via SetInitialAdminPassword")
	}

	// 不存在的 id：no-op，不报错。
	if ok, err = db.SetInitialAdminPassword(ctx, "ghost", "hash-y"); err != nil || ok {
		t.Errorf("ghost id: ok=%v err=%v, want false/nil", ok, err)
	}
}

// bootstrap_completed 标记必须与设密**同事务**生效：设密成功 ⇔ 标记置位。
//
// 标记是免鉴权引导窗口的开关（为什么不能只看「当前是否存在空密码 admin」，
// 见 settings_dao.go 的注释）。分两步写的两种残态都不可接受 ——
// 「设密成功但置标失败」留下窗口仍开着的库（而密码已是抢先者设的那把）；
// 「置标成功但设密失败」把真正要引导的人锁在门外。这里钉住两件事：
// 成功路径两个写入同时可观测；任何 no-op 失败路径都不产生标记。
func TestSetInitialAdminPassword_BootstrapMarkerAtomic(t *testing.T) {
	db := newStore(t)
	ctx := context.Background()

	// 从未引导过的库：标记必须不存在。
	if done, err := db.BootstrapCompleted(ctx); err != nil || done {
		t.Fatalf("fresh db BootstrapCompleted = %v (err %v), want false", done, err)
	}

	if err := db.CreateUser(ctx, &User{ID: "a1", Username: "admin", Role: RoleAdmin, AuthVersion: 1}); err != nil {
		t.Fatalf("create admin: %v", err)
	}
	// 失败路径 1：目标是空密码普通用户（不是引导对象）—— 不写标记。
	if err := db.CreateUser(ctx, &User{ID: "u1", Username: "bob", Role: RoleUser, AuthVersion: 1}); err != nil {
		t.Fatalf("create user: %v", err)
	}
	if ok, err := db.SetInitialAdminPassword(ctx, "u1", "hash-x"); err != nil || ok {
		t.Fatalf("non-admin setup: ok=%v err=%v, want false/nil", ok, err)
	}
	if done, err := db.BootstrapCompleted(ctx); err != nil || done {
		t.Fatalf("failed setup flipped the marker: %v (err %v), want false", done, err)
	}

	// 失败路径 2：id 不存在 —— 同样不写标记。
	if ok, err := db.SetInitialAdminPassword(ctx, "ghost", "hash-y"); err != nil || ok {
		t.Fatalf("ghost setup: ok=%v err=%v, want false/nil", ok, err)
	}
	if done, err := db.BootstrapCompleted(ctx); err != nil || done {
		t.Fatalf("ghost setup flipped the marker: %v (err %v), want false", done, err)
	}

	// 成功路径：密码与标记必须同时可观测（同一事务提交）。
	if ok, err := db.SetInitialAdminPassword(ctx, "a1", "hash-1"); err != nil || !ok {
		t.Fatalf("first setup: ok=%v err=%v, want true", ok, err)
	}
	done, err := db.BootstrapCompleted(ctx)
	if err != nil || !done {
		t.Fatalf("BootstrapCompleted after successful setup = %v (err %v), want true", done, err)
	}
	// 重复初始化（0 行受影响）：无任何写入，标记保持不变。
	if ok, err := db.SetInitialAdminPassword(ctx, "a1", "hash-2"); err != nil || ok {
		t.Fatalf("second setup: ok=%v err=%v, want false/nil", ok, err)
	}
}

// 用户名唯一且大小写不敏感：否则 "admin" 与 "Admin" 能建成两个账号，
// 而登录查询用 NOCASE 只会命中一个 —— 表现为「密码错误」，实为账号撞车。
func TestCreateUser_UsernameUniqueCaseInsensitive(t *testing.T) {
	db := newStore(t)
	ctx := context.Background()
	if err := db.CreateUser(ctx, &User{ID: "u1", Username: "admin"}); err != nil {
		t.Fatalf("create admin: %v", err)
	}
	err := db.CreateUser(ctx, &User{ID: "u2", Username: "Admin"})
	if err == nil {
		t.Fatal("duplicate username differing only in case was accepted")
	}
	if !IsUniqueViolation(err) {
		t.Errorf("want unique violation, got %v", err)
	}
}

func TestGetUserByUsername_CaseInsensitive(t *testing.T) {
	db := newStore(t)
	ctx := context.Background()
	if err := db.CreateUser(ctx, &User{ID: "u1", Username: "Alice"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	for _, probe := range []string{"alice", "ALICE", "Alice"} {
		u, err := db.GetUserByUsername(ctx, probe)
		if err != nil {
			t.Fatalf("lookup %q: %v", probe, err)
		}
		if u == nil || u.ID != "u1" {
			t.Errorf("lookup %q returned %+v, want u1", probe, u)
		}
	}
	if none, _ := db.GetUserByUsername(ctx, "nobody"); none != nil {
		t.Errorf("unknown user returned %+v, want nil", none)
	}
}

// 删用户级联删 key，但**保留** usage_records —— 用量是对账依据，不能被抹掉。
func TestDeleteUser_CascadesKeysKeepsUsage(t *testing.T) {
	db := newStore(t)
	ctx := context.Background()
	if err := db.CreateUser(ctx, &User{ID: "u1", Username: "alice"}); err != nil {
		t.Fatalf("create user: %v", err)
	}
	if err := db.CreateAccessKey(ctx, &AccessKey{
		ID: "k1", KeyHash: "h1", KeyPrefix: "sk-", Name: "k", UserID: "u1",
	}); err != nil {
		t.Fatalf("create key: %v", err)
	}
	seedUsage(t, db, "u1", "k1", "u1", 42)

	if err := db.DeleteUser(ctx, "u1"); err != nil {
		t.Fatalf("delete user: %v", err)
	}
	if keys, _ := db.ListAccessKeys(ctx); len(keys) != 0 {
		t.Errorf("keys survived user deletion: %+v", keys)
	}
	var n int
	if err := db.read.QueryRow(`SELECT COUNT(*) FROM usage_records WHERE id='u1'`).Scan(&n); err != nil {
		t.Fatalf("count usage: %v", err)
	}
	if n != 1 {
		t.Error("usage_records must survive user deletion (对账依据不能被抹掉)")
	}
}

// ---- 用户级用量 ----

func TestSumUserUsedTokens(t *testing.T) {
	db := newStore(t)
	ctx := context.Background()
	seedUsage(t, db, "x1", "k1", "u1", 100)
	seedUsage(t, db, "x2", "k1", "u1", 50)
	seedUsage(t, db, "x3", "k2", "u2", 999)
	seedUsage(t, db, "x4", "k3", "", 7) // 无归属 key 的用量，不算到任何人头上

	got, err := db.SumUserUsedTokens(ctx, "u1")
	if err != nil {
		t.Fatalf("sum: %v", err)
	}
	if got != 150 {
		t.Errorf("SumUserUsedTokens(u1) = %d, want 150", got)
	}
	if empty, err := db.SumUserUsedTokens(ctx, "nobody"); err != nil || empty != 0 {
		t.Errorf("unknown user sum = %d (err %v), want 0", empty, err)
	}
}

func TestRecomputeUserUsage(t *testing.T) {
	db := newStore(t)
	ctx := context.Background()
	if err := db.CreateUser(ctx, &User{ID: "u1", Username: "alice"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	seedUsage(t, db, "x1", "k1", "u1", 100)
	seedUsage(t, db, "x2", "k1", "u1", 30)

	used, err := db.RecomputeUserUsage(ctx, "u1")
	if err != nil {
		t.Fatalf("recompute: %v", err)
	}
	if used != 130 {
		t.Errorf("recomputed = %d, want 130", used)
	}
	u, _ := db.GetUser(ctx, "u1")
	if u.UsedTokens != 130 {
		t.Errorf("users.used_tokens = %d after recompute, want 130", u.UsedTokens)
	}
	// 不存在的用户必须报 ErrNotFound，而不是回 0 + nil
	if _, err := db.RecomputeUserUsage(ctx, "ghost"); err != ErrNotFound {
		t.Errorf("recompute unknown user: err = %v, want ErrNotFound", err)
	}
}

func TestCountUsers(t *testing.T) {
	db := newStore(t)
	ctx := context.Background()
	if n, _ := db.CountUsers(ctx); n != 0 {
		t.Errorf("fresh db has %d users, want 0", n)
	}
	if err := db.CreateUser(ctx, &User{ID: "u1", Username: "a"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if n, _ := db.CountUsers(ctx); n != 1 {
		t.Errorf("after 1 create: %d, want 1", n)
	}
}

// ---- 无归属 key 的退役（2026-10-05 决策）----

// 升级后不发新 key 就不给用：存量无归属 key 必须被禁用。
func TestRetireOrphanKeys_DisablesUnowned(t *testing.T) {
	db := newStore(t)
	ctx := context.Background()
	if err := db.CreateUser(ctx, &User{ID: "u1", Username: "alice"}); err != nil {
		t.Fatalf("create user: %v", err)
	}
	// 一把无归属（会退役）、一把有归属（必须保持原样）
	if err := db.CreateAccessKey(ctx, &AccessKey{ID: "orphan", KeyHash: "h1", KeyPrefix: "sk-", Name: "orphan", Enabled: true}); err != nil {
		t.Fatalf("create orphan: %v", err)
	}
	if err := db.CreateAccessKey(ctx, &AccessKey{ID: "owned", KeyHash: "h2", KeyPrefix: "sk-", Name: "owned", Enabled: true, UserID: "u1"}); err != nil {
		t.Fatalf("create owned: %v", err)
	}

	if err := db.retireOrphanKeys(); err != nil {
		t.Fatalf("retire: %v", err)
	}

	orphan, _ := db.GetAccessKey(ctx, "orphan")
	if orphan.Enabled {
		t.Error("unowned key still enabled; it must not be usable after migration")
	}
	owned, _ := db.GetAccessKey(ctx, "owned")
	if !owned.Enabled {
		t.Error("owned key was disabled by retireOrphanKeys")
	}
}

// 退役必须**只动无归属的**：不能误伤已有用户的 key。
// 这条若写错就是生产事故 —— 全公司的 key 一起失效。
func TestRetireOrphanKeys_LeavesOwnedKeysAlone(t *testing.T) {
	db := newStore(t)
	ctx := context.Background()
	for i, u := range []string{"u1", "u2", "u3"} {
		if err := db.CreateUser(ctx, &User{ID: u, Username: u}); err != nil {
			t.Fatalf("create %s: %v", u, err)
		}
		if err := db.CreateAccessKey(ctx, &AccessKey{
			ID: "k" + u, KeyHash: "h" + u, KeyPrefix: "sk-", Name: u, Enabled: true, UserID: u,
		}); err != nil {
			t.Fatalf("create key %d: %v", i, err)
		}
	}
	if err := db.retireOrphanKeys(); err != nil {
		t.Fatalf("retire: %v", err)
	}
	keys, _ := db.ListAccessKeys(ctx)
	for _, k := range keys {
		if !k.Enabled {
			t.Errorf("key %q was disabled but has owner %q", k.ID, k.UserID)
		}
	}
	if len(keys) != 3 {
		t.Errorf("key count = %d, want 3", len(keys))
	}
}

// 幂等：跑两次结果相同，且不会把管理员后来补好归属并启用的 key 再关掉。
func TestRetireOrphanKeys_Idempotent(t *testing.T) {
	db := newStore(t)
	ctx := context.Background()
	if err := db.CreateAccessKey(ctx, &AccessKey{ID: "orphan", KeyHash: "h1", KeyPrefix: "sk-", Name: "o", Enabled: true}); err != nil {
		t.Fatalf("create: %v", err)
	}
	for i := 0; i < 3; i++ {
		if err := db.retireOrphanKeys(); err != nil {
			t.Fatalf("retire %d: %v", i, err)
		}
	}
	k, _ := db.GetAccessKey(ctx, "orphan")
	if k.Enabled {
		t.Error("key re-enabled by repeated retire calls")
	}

	// 管理员接管：补上归属并重新启用，之后不该再被退役
	if err := db.CreateUser(ctx, &User{ID: "u1", Username: "alice"}); err != nil {
		t.Fatalf("create user: %v", err)
	}
	if _, err := db.db.Exec(`UPDATE access_keys SET user_id='u1', enabled=1 WHERE id='orphan'`); err != nil {
		t.Fatalf("take over: %v", err)
	}
	if err := db.retireOrphanKeys(); err != nil {
		t.Fatalf("retire after takeover: %v", err)
	}
	k, _ = db.GetAccessKey(ctx, "orphan")
	if !k.Enabled || k.UserID != "u1" {
		t.Errorf("taken-over key was retired again: enabled=%v user=%q", k.Enabled, k.UserID)
	}
}
