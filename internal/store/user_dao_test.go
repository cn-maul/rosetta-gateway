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
	if err := db.CreateUser(ctx, &User{ID: "u1", Username: "alice", Status: UserStatusActive}); err != nil {
		t.Fatalf("create: %v", err)
	}
	u, _ := db.GetUser(ctx, "u1")
	if !u.IsActive() {
		t.Fatal("fresh user should be active")
	}
	if err := db.UpdateUser(ctx, &User{
		ID: "u1", Username: "alice", Role: RoleUser,
		Status: UserStatusDisabled, AuthVersion: u.AuthVersion,
	}); err != nil {
		t.Fatalf("disable: %v", err)
	}
	got, _ := db.GetUser(ctx, "u1")
	if got.IsActive() {
		t.Error("user still active after disable")
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
