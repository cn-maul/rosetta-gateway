package store

import (
	"context"
	"database/sql"
	"errors"
	"net/netip"
	"path/filepath"
	"sort"
	"testing"
	"time"
)

// ---- 老库升级：P1 的两个增量列 ----

// 老库（没有 users.group_id、没有 access_keys.allowed_models_json）
// 升级后必须能正常打开，且存量数据照常可读。
//
// # 这条为什么值得单独写一个测试
//
// P1 新增了一个**带外键的列**（users.group_id REFERENCES groups(id)）。
// SQLite 对 `ALTER TABLE ADD COLUMN ... REFERENCES` 有额外要求：外键目标表
// 必须已存在。groups 由 migrations 列表创建、ensureColumns 在其后执行，
// 所以顺序是对的 —— 但顺序一旦被后人调换，症状是**整个网关起不来**
// （migrate 失败 = Open 失败），而不是某个功能不可用。这条测试守住那个顺序。
func TestMigration_P1ColumnsAddedToLegacyDB(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	ctx := context.Background()

	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	for _, ddl := range []string{
		`CREATE TABLE access_keys (
			id TEXT PRIMARY KEY, key_hash TEXT NOT NULL UNIQUE, key_prefix TEXT NOT NULL,
			name TEXT NOT NULL, enabled INTEGER NOT NULL DEFAULT 1,
			quota_tokens INTEGER NOT NULL DEFAULT 0, used_tokens INTEGER NOT NULL DEFAULT 0,
			created_at INTEGER NOT NULL)`,
		`CREATE TABLE users (
			id TEXT PRIMARY KEY, username TEXT NOT NULL UNIQUE, display_name TEXT,
			password_hash TEXT NOT NULL, role TEXT NOT NULL DEFAULT 'user',
			status TEXT NOT NULL DEFAULT 'active', quota_tokens INTEGER NOT NULL DEFAULT 0,
			used_tokens INTEGER NOT NULL DEFAULT 0, auth_version INTEGER NOT NULL DEFAULT 1,
			remark TEXT, created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL,
			last_login_at INTEGER NOT NULL DEFAULT 0)`,
		`CREATE TABLE usage_records (
			id TEXT PRIMARY KEY, ts INTEGER NOT NULL, access_key_id TEXT NOT NULL,
			public_model TEXT NOT NULL, provider_id TEXT NOT NULL, upstream_model TEXT NOT NULL,
			ingress_protocol TEXT NOT NULL, stream INTEGER NOT NULL,
			input_tokens INTEGER NOT NULL DEFAULT 0, output_tokens INTEGER NOT NULL DEFAULT 0,
			total_tokens INTEGER NOT NULL DEFAULT 0, reasoning_tokens INTEGER NOT NULL DEFAULT 0,
			cached_tokens INTEGER NOT NULL DEFAULT 0, usage_state TEXT NOT NULL,
			status TEXT NOT NULL, http_status INTEGER NOT NULL, error_code TEXT,
			latency_ms INTEGER NOT NULL, ttfb_ms INTEGER NOT NULL DEFAULT 0, request_id TEXT)`,
	} {
		if _, err := raw.Exec(ddl); err != nil {
			t.Fatalf("legacy ddl: %v", err)
		}
	}
	now := time.Now().UnixMilli()
	if _, err := raw.Exec(`INSERT INTO users
		(id, username, password_hash, role, status, created_at, updated_at)
		VALUES ('u-legacy','legacy','hash','user','active',?,?)`, now, now); err != nil {
		t.Fatalf("seed legacy user: %v", err)
	}
	if _, err := raw.Exec(`INSERT INTO access_keys
		(id, key_hash, key_prefix, name, enabled, quota_tokens, used_tokens, created_at)
		VALUES ('k-legacy','h-legacy','sk-gw-legacy','存量key',1,0,0,?)`, now); err != nil {
		t.Fatalf("seed legacy key: %v", err)
	}
	raw.Close()

	db := openAt(t, path)

	// 新增列必须真的加上（否则后面的读写会静默走 NULL）。
	for _, c := range []struct{ table, column string }{
		{"users", "group_id"},
		{"access_keys", "allowed_models_json"},
	} {
		has, cerr := db.columnExists(c.table, c.column)
		if cerr != nil {
			t.Fatalf("columnExists(%s.%s): %v", c.table, c.column, cerr)
		}
		if !has {
			t.Errorf("%s.%s was not added by migration", c.table, c.column)
		}
	}

	// 存量数据照常可读，且新列取「未配置」语义（空串 / nil = 不限制）。
	u, err := db.GetUser(ctx, "u-legacy")
	if err != nil || u == nil {
		t.Fatalf("read legacy user: %v (u=%v)", err, u)
	}
	if u.GroupID != "" {
		t.Errorf("GroupID = %q, want empty for legacy user", u.GroupID)
	}
	k, err := db.GetAccessKey(ctx, "k-legacy")
	if err != nil || k == nil {
		t.Fatalf("read legacy key: %v (k=%v)", err, k)
	}
	if k.AllowedModels != nil {
		t.Errorf("AllowedModels = %v, want nil (未配置限制)", k.AllowedModels)
	}

	// groups 表存在且为空 —— 「无分组」就是 P1 的向后兼容状态。
	groups, err := db.ListGroups(ctx)
	if err != nil {
		t.Fatalf("list groups: %v", err)
	}
	if len(groups) != 0 {
		t.Errorf("legacy db should have no groups, got %d", len(groups))
	}
}

// ---- 分组 CRUD ----

func TestGroup_CreateGetListUpdate(t *testing.T) {
	db := newStore(t)
	ctx := context.Background()

	g := &Group{ID: "g1", Name: "研发", Description: "内部研发组"}
	if err := db.CreateGroup(ctx, g); err != nil {
		t.Fatalf("create: %v", err)
	}
	if g.CreatedAt == 0 {
		t.Error("CreatedAt must be backfilled on create")
	}

	got, err := db.GetGroup(ctx, "g1")
	if err != nil || got == nil {
		t.Fatalf("get: %v (g=%v)", err, got)
	}
	if got.Name != "研发" || got.Description != "内部研发组" {
		t.Fatalf("get mismatch: %+v", got)
	}

	got.Name = "研发二组"
	if err := db.UpdateGroup(ctx, got); err != nil {
		t.Fatalf("update: %v", err)
	}
	again, _ := db.GetGroup(ctx, "g1")
	if again.Name != "研发二组" {
		t.Errorf("name not updated: %q", again.Name)
	}

	list, err := db.ListGroups(ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("list: %v len=%d", err, len(list))
	}

	// 不存在的 id → (nil, nil)，与 GetUser 约定一致。
	missing, err := db.GetGroup(ctx, "nope")
	if err != nil || missing != nil {
		t.Fatalf("missing group = %v, err = %v; want nil,nil", missing, err)
	}
}

// 组名唯一性必须大小写不敏感。与 users.username 同一个理由：
// 大小写敏感的唯一约束会让「研发」与「Dev」看起来是两个组，
// 而管理员以为是同一个。
func TestGroup_UniqueNameIsCaseInsensitive(t *testing.T) {
	db := newStore(t)
	ctx := context.Background()

	if err := db.CreateGroup(ctx, &Group{ID: "g1", Name: "Dev"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	err := db.CreateGroup(ctx, &Group{ID: "g2", Name: "dev"})
	if err == nil {
		t.Fatal("duplicate name differing only in case must be rejected")
	}
	if !IsUniqueViolation(err) {
		t.Errorf("err = %v, want a unique-violation (so the handler can map it to 409)", err)
	}
}

// 组里还有成员时拒绝删除。
//
// 这一条是安全性要求，不是洁癖：白名单非空的组被删掉后，靠外键
// ON DELETE SET NULL 会让那些成员**从「受限」变成「不受限」**，
// 等于一次删除操作悄悄给一组人扩权。
func TestGroup_DeleteRefusesWhenMembersExist(t *testing.T) {
	db := newStore(t)
	ctx := context.Background()

	if err := db.CreateGroup(ctx, &Group{ID: "g1", Name: "dev"}); err != nil {
		t.Fatalf("create group: %v", err)
	}
	if err := db.CreateUser(ctx, &User{ID: "u1", Username: "alice", GroupID: "g1"}); err != nil {
		t.Fatalf("create user: %v", err)
	}

	err := db.DeleteGroup(ctx, "g1")
	if err == nil {
		t.Fatal("deleting a group with members must fail")
	}
	if !errors.Is(err, ErrGroupNotEmpty) {
		t.Errorf("err = %v, want ErrGroupNotEmpty", err)
	}
	// 组与成员归属都不能变。
	if g, _ := db.GetGroup(ctx, "g1"); g == nil {
		t.Error("group disappeared despite the refusal")
	}
	if u, _ := db.GetUser(ctx, "u1"); u == nil || u.GroupID != "g1" {
		t.Errorf("member's group changed: %+v", u)
	}

	// 把成员移走后就能删。
	u, _ := db.GetUser(ctx, "u1")
	u.GroupID = ""
	if err := db.UpdateUser(ctx, u); err != nil {
		t.Fatalf("ungroup: %v", err)
	}
	if err := db.DeleteGroup(ctx, "g1"); err != nil {
		t.Fatalf("delete after ungroup: %v", err)
	}
	if g, _ := db.GetGroup(ctx, "g1"); g != nil {
		t.Error("group still present after delete")
	}
}

// 删除组要连带清掉它的白名单（外键 CASCADE），否则残留行会在
// 同名新组被创建时「复活」成旧权限。
func TestGroup_DeleteCascadesModelWhitelist(t *testing.T) {
	db := newStore(t)
	ctx := context.Background()

	if err := db.CreateGroup(ctx, &Group{ID: "g1", Name: "dev"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := db.ReplaceGroupModels(ctx, "g1", []string{"m1", "m2"}); err != nil {
		t.Fatalf("set models: %v", err)
	}
	if err := db.DeleteGroup(ctx, "g1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	byGroup, err := db.ListGroupModels(ctx)
	if err != nil {
		t.Fatalf("list models: %v", err)
	}
	if _, exists := byGroup["g1"]; exists {
		t.Errorf("whitelist rows survived group deletion: %v", byGroup)
	}
}

func TestGroup_DeleteMissingReturnsNotFound(t *testing.T) {
	db := newStore(t)
	if err := db.DeleteGroup(context.Background(), "nope"); err != ErrNotFound {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

// ---- 组白名单 ----

func TestGroup_ReplaceModelsIsWholesaleAndNormalized(t *testing.T) {
	db := newStore(t)
	ctx := context.Background()
	if err := db.CreateGroup(ctx, &Group{ID: "g1", Name: "dev"}); err != nil {
		t.Fatalf("create: %v", err)
	}

	// 去空白、丢空串、去重、排序。
	if err := db.ReplaceGroupModels(ctx, "g1", []string{" m2 ", "m1", "", "m2", "m1"}); err != nil {
		t.Fatalf("replace: %v", err)
	}
	got, err := db.ListGroupModelsByGroup(ctx, "g1")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 2 || got[0] != "m1" || got[1] != "m2" {
		t.Fatalf("models = %v, want [m1 m2] (sorted, deduped, trimmed)", got)
	}

	// 整体替换：旧值必须消失（这是「取消勾选」的表达方式）。
	if err := db.ReplaceGroupModels(ctx, "g1", []string{"m3"}); err != nil {
		t.Fatalf("replace again: %v", err)
	}
	got, _ = db.ListGroupModelsByGroup(ctx, "g1")
	if len(got) != 1 || got[0] != "m3" {
		t.Fatalf("models = %v, want [m3] — replace must be wholesale", got)
	}

	// 传空 = 清空（该组不限制），不是「拒绝全部模型」。
	if err := db.ReplaceGroupModels(ctx, "g1", nil); err != nil {
		t.Fatalf("clear: %v", err)
	}
	got, _ = db.ListGroupModelsByGroup(ctx, "g1")
	if len(got) != 0 {
		t.Fatalf("models = %v, want empty after clear", got)
	}
}

// 给不存在的组设白名单必须报错。空白名单时一句 INSERT 都不执行，
// 光靠外键兜不住 —— 那会静默成功，管理员以为配好了。
func TestGroup_ReplaceModelsUnknownGroup(t *testing.T) {
	db := newStore(t)
	ctx := context.Background()

	if err := db.ReplaceGroupModels(ctx, "nope", nil); err != ErrNotFound {
		t.Fatalf("nil list: err = %v, want ErrNotFound", err)
	}
	if err := db.ReplaceGroupModels(ctx, "nope", []string{"m1"}); err != ErrNotFound {
		t.Fatalf("non-empty list: err = %v, want ErrNotFound", err)
	}
}

func TestGroup_ListModelsGroupsByGroupID(t *testing.T) {
	db := newStore(t)
	ctx := context.Background()
	for _, id := range []string{"g1", "g2"} {
		if err := db.CreateGroup(ctx, &Group{ID: id, Name: id}); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
	}
	if err := db.ReplaceGroupModels(ctx, "g1", []string{"a", "b"}); err != nil {
		t.Fatalf("g1 models: %v", err)
	}
	if err := db.ReplaceGroupModels(ctx, "g2", []string{"c"}); err != nil {
		t.Fatalf("g2 models: %v", err)
	}

	got, err := db.ListGroupModels(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("groups in map = %d, want 2 (%v)", len(got), got)
	}
	sort.Strings(got["g1"])
	if len(got["g1"]) != 2 || got["g1"][0] != "a" || got["g1"][1] != "b" {
		t.Errorf("g1 = %v, want [a b]", got["g1"])
	}
	if len(got["g2"]) != 1 || got["g2"][0] != "c" {
		t.Errorf("g2 = %v, want [c]", got["g2"])
	}
}

// ---- key 级白名单 ----

func TestAccessKey_AllowedModelsRoundTrip(t *testing.T) {
	db := newStore(t)
	ctx := context.Background()

	k := &AccessKey{
		ID: "k1", KeyHash: "h1", KeyPrefix: "sk-", Name: "n", Enabled: true,
		AllowedModels: []string{"gpt-4o", "claude-sonnet"},
	}
	if err := db.CreateAccessKey(ctx, k); err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := db.GetAccessKey(ctx, "k1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(got.AllowedModels) != 2 {
		t.Fatalf("AllowedModels = %v, want 2 entries", got.AllowedModels)
	}

	// 未配置 → nil（不是空切片）。这个区分是「不限制」与「拒绝全部」的分界，
	// 必须精确。
	plain := &AccessKey{ID: "k2", KeyHash: "h2", KeyPrefix: "sk-", Name: "n", Enabled: true}
	if err := db.CreateAccessKey(ctx, plain); err != nil {
		t.Fatalf("create plain: %v", err)
	}
	got2, _ := db.GetAccessKey(ctx, "k2")
	if got2.AllowedModels != nil {
		t.Errorf("unconfigured key AllowedModels = %v, want nil", got2.AllowedModels)
	}

	// 清空（PATCH 传 []）→ 回到 nil。
	got.AllowedModels = nil
	if err := db.UpdateAccessKey(ctx, "k1", got); err != nil {
		t.Fatalf("update: %v", err)
	}
	got3, _ := db.GetAccessKey(ctx, "k1")
	if got3.AllowedModels != nil {
		t.Errorf("cleared AllowedModels = %v, want nil", got3.AllowedModels)
	}
}

// 库里的 JSON 坏掉时**拒绝全部模型**，而不是「不限制」。
//
// 这是 fail-closed 的关键一条：当成「不限制」等于一次数据故障变成静默放权
// （把本该受限的 key 放开给全部模型，且无人发现）。当成「拒绝全部」的代价
// 只是那把 key 暂时用不了，管理员重新保存一次白名单即可修好。
func TestAccessKey_CorruptAllowedModelsJSONDeniesAll(t *testing.T) {
	db := newStore(t)
	ctx := context.Background()

	if err := db.CreateAccessKey(ctx, &AccessKey{
		ID: "k1", KeyHash: "h1", KeyPrefix: "sk-", Name: "n", Enabled: true,
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := db.db.Exec(
		`UPDATE access_keys SET allowed_models_json = ? WHERE id = 'k1'`, "gpt-4o,broken-json"); err != nil {
		t.Fatalf("corrupt: %v", err)
	}

	got, err := db.GetAccessKey(ctx, "k1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.AllowedModels == nil {
		t.Fatal("corrupt JSON must not degrade to nil (= 不限制); it must fail closed")
	}
	if len(got.AllowedModels) != 0 {
		t.Errorf("corrupt JSON → %v, want an empty (deny-all) list", got.AllowedModels)
	}
}

// ---- 用户分组 ----

func TestUser_GroupIDRoundTrip(t *testing.T) {
	db := newStore(t)
	ctx := context.Background()
	if err := db.CreateGroup(ctx, &Group{ID: "g1", Name: "dev"}); err != nil {
		t.Fatalf("create group: %v", err)
	}

	if err := db.CreateUser(ctx, &User{ID: "u1", Username: "alice", GroupID: "g1"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	u, err := db.GetUser(ctx, "u1")
	if err != nil || u == nil {
		t.Fatalf("get: %v", err)
	}
	if u.GroupID != "g1" {
		t.Fatalf("GroupID = %q, want g1", u.GroupID)
	}

	// 移到别的组 / 移出组（空串 → NULL）。
	u.GroupID = ""
	if err := db.UpdateUser(ctx, u); err != nil {
		t.Fatalf("update: %v", err)
	}
	again, _ := db.GetUser(ctx, "u1")
	if again.GroupID != "" {
		t.Errorf("GroupID = %q, want empty after removal", again.GroupID)
	}
	// 库里必须是真 NULL（而不是空串）—— 两种状态在语义上是同一个，
	// 但 NULL 才能让外键 ON DELETE SET NULL 生效。
	var raw sql.NullString
	if err := db.read.QueryRow(`SELECT group_id FROM users WHERE id='u1'`).Scan(&raw); err != nil {
		t.Fatalf("raw read: %v", err)
	}
	if raw.Valid {
		t.Errorf("group_id in db = %q, want NULL", raw.String)
	}
}

// 组被删（成员已迁走）后，用户的 group_id 由外键置 NULL，
// 不会留下指向不存在组的悬空 id。
func TestUser_GroupIDClearedByGroupDeletion(t *testing.T) {
	db := newStore(t)
	ctx := context.Background()
	if err := db.CreateGroup(ctx, &Group{ID: "g1", Name: "dev"}); err != nil {
		t.Fatalf("create group: %v", err)
	}
	// 直接改库把用户放进组（绕开「有成员不让删」的守卫），
	// 验证外键这一层安全网确实在工作。
	if err := db.CreateUser(ctx, &User{ID: "u1", Username: "alice"}); err != nil {
		t.Fatalf("create user: %v", err)
	}
	if _, err := db.db.Exec(`UPDATE users SET group_id='g1' WHERE id='u1'`); err != nil {
		t.Fatalf("force group: %v", err)
	}
	if _, err := db.db.Exec(`DELETE FROM groups WHERE id='g1'`); err != nil {
		t.Fatalf("force delete: %v", err)
	}
	u, _ := db.GetUser(ctx, "u1")
	if u.GroupID != "" {
		t.Errorf("dangling group_id = %q, want empty (FK should SET NULL)", u.GroupID)
	}
}

// ---- P2：有效期 / 来源 IP / key 级分组覆盖 ----

func TestAccessKey_P2FieldsRoundTrip(t *testing.T) {
	db := newStore(t)
	ctx := context.Background()
	if err := db.CreateGroup(ctx, &Group{ID: "g1", Name: "dev"}); err != nil {
		t.Fatalf("create group: %v", err)
	}
	// user_id 与 group_id 都是外键：必须先有对应的行（_foreign_keys=ON 是开启的，
	// 这条约束值得让测试替我们守住 —— 写错归属应当在建 key 时就失败）。
	if err := db.CreateUser(ctx, &User{ID: "u1", Username: "alice"}); err != nil {
		t.Fatalf("create user: %v", err)
	}

	k := &AccessKey{
		ID: "k1", KeyHash: "h1", KeyPrefix: "sk-", Name: "n", Enabled: true,
		UserID: "u1", ExpiresAt: 1893456000000,
		AllowedIPs: "10.0.0.0/8, 203.0.113.7",
		GroupID:    "g1",
	}
	if err := db.CreateAccessKey(ctx, k); err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := db.GetAccessKey(ctx, "k1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ExpiresAt != 1893456000000 {
		t.Errorf("ExpiresAt = %d", got.ExpiresAt)
	}
	if got.GroupID != "g1" {
		t.Errorf("GroupID = %q", got.GroupID)
	}
	if got.AllowedIPs != "10.0.0.0/8, 203.0.113.7" {
		t.Errorf("AllowedIPs = %q", got.AllowedIPs)
	}
	// 解析结果也要在（热路径直接用它，不查库）。
	if len(got.AllowedNets) != 2 {
		t.Fatalf("AllowedNets = %v, want 2", got.AllowedNets)
	}
	// 单 IP 被当成 /32 处理。
	if !got.AllowedNets[1].Contains(netip.MustParseAddr("203.0.113.7")) {
		t.Errorf("单 IP 未按 /32 处理: %v", got.AllowedNets[1])
	}

	// 清空：三个字段都能改回「未配置」。
	got.ExpiresAt, got.AllowedIPs, got.GroupID = 0, "", ""
	if err := db.UpdateAccessKey(ctx, "k1", got); err != nil {
		t.Fatalf("clear: %v", err)
	}
	cleared, _ := db.GetAccessKey(ctx, "k1")
	if cleared.ExpiresAt != 0 || cleared.AllowedIPs != "" || cleared.GroupID != "" {
		t.Errorf("清空未生效: %+v", cleared)
	}
	if cleared.AllowedNets != nil {
		t.Errorf("清空后 AllowedNets = %v, want nil（nil = 不限制）", cleared.AllowedNets)
	}
}

func TestParseAllowedNets(t *testing.T) {
	cases := []struct {
		raw     string
		wantN   int
		wantErr bool
	}{
		{"", 0, false},                              // 空 = 不限制（nil）
		{"   ", 0, false},                           // 纯空白同上
		{"10.0.0.0/8", 1, false},                    // 单个 CIDR
		{"10.0.0.0/8,192.168.0.0/16", 2, false},     // 多个
		{" 10.0.0.0/8 , 192.168.0.0/16 ", 2, false}, // 空白容忍
		{"10.0.0.0/8,,192.168.0.0/16", 2, false},    // 空项跳过
		{"203.0.113.7", 1, false},                   // 单 IP
		{"2001:db8::/32", 1, false},                 // IPv6
		{"::1", 1, false},                           // IPv6 单地址
		{"10.0.0.0/8,not-an-ip", 0, true},           // 有一条坏 → 整体报错
		{"999.1.1.1", 0, true},                      // 越界 IPv4
		{"10.0.0.0/33", 0, true},                    // 越界前缀长度
	}
	for _, tc := range cases {
		nets, err := ParseAllowedNets(tc.raw)
		if tc.wantErr {
			if err == nil {
				t.Errorf("%q: err = nil, want error", tc.raw)
			} else if nets != nil {
				t.Errorf("%q: 报错时 nets 应为 nil，实际 %v", tc.raw, nets)
			}
			continue
		}
		if err != nil {
			t.Errorf("%q: err = %v, want nil", tc.raw, err)
			continue
		}
		if len(nets) != tc.wantN {
			t.Errorf("%q: nets = %v, want %d 条", tc.raw, nets, tc.wantN)
		}
		// 空输入必须是 nil 而不是空切片：nil 与空切片语义不同（见函数注释）。
		if tc.raw == "" || tc.raw == "   " {
			if nets != nil {
				t.Errorf("%q: 未配置应返回 nil，实际 %v（空切片会被当成「全拒」）", tc.raw, nets)
			}
		}
	}
}

// CIDR 写坏时读路径必须**全拒**，不能放行也不能「跳过坏的那条」。
func TestAccessKey_CorruptAllowedIPsDeniesAll(t *testing.T) {
	db := newStore(t)
	ctx := context.Background()
	if err := db.CreateAccessKey(ctx, &AccessKey{
		ID: "k1", KeyHash: "h1", KeyPrefix: "sk-", Name: "n", Enabled: true,
		AllowedIPs: "10.0.0.0/8",
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	// 手工改库制造损坏（管理写入口已在写入点挡住，这里模拟手改）。
	if _, err := db.db.Exec(`UPDATE access_keys SET allowed_ips = ? WHERE id='k1'`, "10.0.0.0/8,bogus"); err != nil {
		t.Fatalf("corrupt: %v", err)
	}
	got, err := db.GetAccessKey(ctx, "k1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.AllowedNets == nil {
		t.Fatal("损坏的 IP 白名单不得退化成 nil（= 不限制），那是一次静默放宽")
	}
	if len(got.AllowedNets) != 0 {
		t.Errorf("AllowedNets = %v, want 空（全拒）", got.AllowedNets)
	}
}
