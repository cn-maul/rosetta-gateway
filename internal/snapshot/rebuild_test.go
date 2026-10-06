package snapshot

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/cn-maul/rosetta-gateway/internal/store"
)

// 快照重建必须把「组白名单」与「key 白名单」都装配到热路径用的位置上。
//
// # 为什么这条必须要测
//
// ModelAllow 的零值是「拒绝全部」（刻意的 fail-closed 设计）。这意味着
// 重建函数里**漏填**某个字段不会表现为「放得太宽」，而是表现为
// 「那把 key 什么都调不了」—— 故障是有界的、可发现的。但反过来，
// 如果有人把 AllowAll() 兜在「查不到就放行」的位置上，就会静默放权。
// 两种都只有测到具体字段才能确认，光看代码看不出来。
func TestRebuildFromDB_PopulatesModelAllowances(t *testing.T) {
	db := openTestStore(t)
	ctx := context.Background()

	// 组 g1 只允许 m1；组 g2 没配白名单（= 不限制）。
	for _, g := range []struct{ id, name string }{{"g1", "restricted"}, {"g2", "open"}} {
		if err := db.CreateGroup(ctx, &store.Group{ID: g.id, Name: g.name}); err != nil {
			t.Fatalf("create group %s: %v", g.id, err)
		}
	}
	if err := db.ReplaceGroupModels(ctx, "g1", []string{"m1"}); err != nil {
		t.Fatalf("set g1 models: %v", err)
	}

	users := []struct{ id, group string }{
		{"u-restricted", "g1"}, // 组有白名单
		{"u-open", "g2"},       // 组存在但没配
		{"u-nogroup", ""},      // 不属于任何组
	}
	for _, u := range users {
		if err := db.CreateUser(ctx, &store.User{ID: u.id, Username: u.id, GroupID: u.group}); err != nil {
			t.Fatalf("create user %s: %v", u.id, err)
		}
	}

	// key：一把带白名单、一把不带。
	keys := []struct {
		id, hash, owner string
		allowed         []string
	}{
		{"k-restricted", "h-restricted", "u-restricted", []string{"m1"}},
		{"k-plain", "h-plain", "u-open", nil},
	}
	for _, k := range keys {
		if err := db.CreateAccessKey(ctx, &store.AccessKey{
			ID: k.id, KeyHash: k.hash, KeyPrefix: "sk-", Name: k.id,
			Enabled: true, UserID: k.owner, AllowedModels: k.allowed,
		}); err != nil {
			t.Fatalf("create key %s: %v", k.id, err)
		}
	}

	snap, err := RebuildFromDB(ctx, db, nil)
	if err != nil {
		t.Fatalf("rebuild: %v", err)
	}

	// 用户维度：g1 的成员必须拿到「只允许 m1」。
	ur := snap.UsersByID["u-restricted"]
	if ur == nil {
		t.Fatal("u-restricted missing from snapshot")
	}
	if ur.AllowedModels.Unrestricted {
		t.Error("u-restricted.AllowedModels is Unrestricted; group g1 has a whitelist")
	}
	if !ur.AllowedModels.Allows("m1") || ur.AllowedModels.Allows("m2") {
		t.Errorf("u-restricted allow-set = %+v, want only m1", ur.AllowedModels)
	}
	if ur.GroupID != "g1" {
		t.Errorf("GroupID = %q, want g1", ur.GroupID)
	}

	// 成员在 g2（组存在但没配白名单）→ **不限制**，与「没分组」等价。
	for _, id := range []string{"u-open", "u-nogroup"} {
		u := snap.UsersByID[id]
		if u == nil {
			t.Fatalf("%s missing from snapshot", id)
		}
		if !u.AllowedModels.Unrestricted {
			t.Errorf("%s.AllowedModels = %+v, want Unrestricted（组未配白名单 = 不限制）", id, u.AllowedModels)
		}
	}

	// key 维度。
	kr := snap.KeysByHash["h-restricted"]
	if kr == nil {
		t.Fatal("k-restricted missing from snapshot")
	}
	if kr.AllowedModels.Unrestricted || !kr.AllowedModels.Allows("m1") || kr.AllowedModels.Allows("m2") {
		t.Errorf("k-restricted allow-set = %+v, want only m1", kr.AllowedModels)
	}
	kp := snap.KeysByHash["h-plain"]
	if kp == nil {
		t.Fatal("k-plain missing from snapshot")
	}
	if !kp.AllowedModels.Unrestricted {
		t.Errorf("k-plain (no whitelist) = %+v, want Unrestricted", kp.AllowedModels)
	}
}

// 分组功能未启用（groups 表为空）时，全部 key 可见全部模型 ——
// 这是 P1 的向后兼容保证：升级后不配分组，行为与改造前完全一致。
func TestRebuildFromDB_NoGroupsMeansUnrestricted(t *testing.T) {
	db := openTestStore(t)
	ctx := context.Background()

	if err := db.CreateUser(ctx, &store.User{ID: "u1", Username: "alice"}); err != nil {
		t.Fatalf("create user: %v", err)
	}
	sum := sha256.Sum256([]byte("sk-test"))
	hexKey := hex.EncodeToString(sum[:])
	if err := db.CreateAccessKey(ctx, &store.AccessKey{
		ID: "k1", KeyHash: hexKey, KeyPrefix: "sk-", Name: "n", Enabled: true, UserID: "u1",
	}); err != nil {
		t.Fatalf("create key: %v", err)
	}

	snap, err := RebuildFromDB(ctx, db, nil)
	if err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	u := snap.UsersByID["u1"]
	k := snap.KeysByHash[hexKey]
	if u == nil || k == nil {
		t.Fatal("snapshot missing user or key")
	}
	if !u.AllowedModels.Unrestricted || !k.AllowedModels.Unrestricted {
		t.Errorf("no groups + no key whitelist must be unrestricted; got user=%+v key=%+v",
			u.AllowedModels, k.AllowedModels)
	}
	// 组合判定：两个维度都不限制 → 任意模型可见。
	for _, m := range []string{"anything", "任意"} {
		if !u.AllowedModels.Intersect(k.AllowedModels).Allows(m) {
			t.Errorf("model %q should be visible with no whitelists", m)
		}
	}
}

// TestRebuildFromDB_MarksNotReadyProviders 是 P4「失败 provider 可见化」的核心断言。
//
// 改造前，池重建时某个 provider 的凭据解不开只写一行日志，快照里它看起来
// 与正常 provider 无异 —— 界面上 enabled=1、凭据在，但请求打过去必然失败。
// 运维看到的是「路由配好了却莫名 500」，而根因躺在日志里没人看。
// 这正是设计 §4.7 要消灭的状态：失败必须进管理面，而不是只进日志。
//
// 断言 Ready=false + Reason 非空：只测其中一个都不够 ——
// Reason 为空的「未就绪」等于没给原因，界面照样说不清。
func TestRebuildFromDB_MarksNotReadyProviders(t *testing.T) {
	db := openTestStore(t)
	ctx := context.Background()

	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("setup: %v", err)
		}
	}
	must(db.CreateProvider(ctx, &store.Provider{
		ID: "p-ok", Slug: "ok-prov", Name: "正常上游",
		Endpoint: "https://example.invalid/v1", Protocol: "openai-chat", Enabled: true,
	}))
	must(db.CreateProvider(ctx, &store.Provider{
		ID: "p-bad", Slug: "bad-prov", Name: "凭据坏了的上游",
		Endpoint: "https://example.invalid/v1", Protocol: "openai-chat", Enabled: true,
	}))

	// 正常 provider 在快照里必须是 Ready=true 且不带原因。
	snap, err := RebuildFromDB(ctx, db, nil)
	if err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	if p := snap.Providers["ok-prov"]; p == nil || !p.Ready || p.Reason != "" {
		t.Fatalf("healthy provider must be Ready with no reason, got %+v", p)
	}

	// 报告 bad-prov 未就绪后，它必须被标出来，且原因非空。
	const reason = "凭据「default」解密失败"
	snap2, err := RebuildFromDB(ctx, db, []ProviderFailure{
		{ID: "p-bad", Slug: "bad-prov", Reason: reason},
	})
	if err != nil {
		t.Fatalf("rebuild with failures: %v", err)
	}
	bad := snap2.Providers["bad-prov"]
	if bad == nil {
		t.Fatal("bad-prov missing from snapshot")
	}
	if bad.Ready {
		t.Error("provider reported as not-ready must have Ready=false")
	}
	if bad.Reason != reason {
		t.Errorf("Reason = %q, want %q", bad.Reason, reason)
	}
	// 同一批里未报告的 provider 不能被株连。
	if ok := snap2.Providers["ok-prov"]; ok == nil || !ok.Ready {
		t.Errorf("unreported provider must stay Ready, got %+v", ok)
	}
}

// TestRebuildFromDB_UnknownSlugFailureIsIgnored 守「失败清单里的未知项不炸」。
//
// 池与快照是两次独立读取，provider 可能在两次之间被删 —— 那时失败清单里有
// 快照查不到的 slug。忽略即可（该 provider 已经不存在），不能让重建整体失败：
// 一个已删 provider 的残留失败记录不该让全站快照停在旧值上。
func TestRebuildFromDB_UnknownSlugFailureIsIgnored(t *testing.T) {
	db := openTestStore(t)
	ctx := context.Background()
	if err := db.CreateProvider(ctx, &store.Provider{
		ID: "p1", Slug: "prov", Name: "P",
		Endpoint: "https://example.invalid/v1", Protocol: "openai-chat", Enabled: true,
	}); err != nil {
		t.Fatalf("create provider: %v", err)
	}
	snap, err := RebuildFromDB(ctx, db, []ProviderFailure{
		{Slug: "已删除的上游", Reason: "gone"},
	})
	if err != nil {
		t.Fatalf("rebuild must tolerate unknown failure slugs: %v", err)
	}
	if p := snap.Providers["prov"]; p == nil || !p.Ready {
		t.Errorf("existing provider unaffected, got %+v", p)
	}
}

func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "gw.db"),
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}
