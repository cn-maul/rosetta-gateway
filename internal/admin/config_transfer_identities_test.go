package admin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"testing"

	"github.com/cn-maul/rosetta-gateway/internal/config"
	"github.com/cn-maul/rosetta-gateway/internal/crypto"
	"github.com/cn-maul/rosetta-gateway/internal/store"
)

// newTransferHandler 造一个独立的空库 handler（用于「导出 → 导入到另一个库」）。
func newTransferHandler(t *testing.T) *ConfigTransferHandler {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	st, err := store.Open(t.TempDir()+"/gw.db", logger)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return NewConfigTransferHandler(st, []byte("0123456789abcdef0123456789abcdef"), &config.Config{})
}

// seedIdentities 往源库塞一套「分组 / 用户 / 密钥 / 路由」，覆盖 v4 的四个新段。
//
// 刻意造出三种容易搞错的形态：
//   - 用户 admin 的余额是 **0 元**（余额耗尽，不是不限额）；
//   - 用户 dogge 的余额是 **NULL**（不限额）—— 这两者在库里是不同状态，
//     导入时混成一个会让某个账号突然 402 或突然无限刷；
//   - 路由 Deep 有两条链目标，第二条指向 beta 的模型（跨 provider 的链）。
func seedIdentities(t *testing.T, h *ConfigTransferHandler) {
	t.Helper()
	ctx := context.Background()
	st := h.store

	seedSource(t, h, st) // p1/alpha/p2/beta + m1..m3 + c1/c2

	g := &store.Group{ID: "g1", Name: "vip", Description: "VIP 组"}
	if err := st.CreateGroup(ctx, g); err != nil {
		t.Fatalf("seed group: %v", err)
	}
	if err := st.ReplaceGroupModels(ctx, "g1", []string{"alpha-chat"}); err != nil {
		t.Fatalf("seed group models: %v", err)
	}

	zero := int64(0)
	users := []struct {
		id, name, role string
		balance        *int64
	}{
		{"u1", "admin", store.RoleAdmin, &zero},
		{"u2", "dogge", store.RoleUser, nil}, // nil = 不限额
	}
	for _, u := range users {
		usr := &store.User{
			ID: u.id, Username: u.name, Role: u.role, Status: store.UserStatusActive,
			Unlimited: u.balance == nil, GroupID: "g1",
		}
		if u.balance != nil {
			usr.BalanceCents = *u.balance
		}
		if err := st.CreateUser(ctx, usr); err != nil {
			t.Fatalf("seed user %s: %v", u.name, err)
		}
	}

	// 密钥：明文 "sk-gw-testkey-aaaa..."，密文按 master.key 加密，导出侧靠它回读明文。
	for _, k := range []struct{ id, name, owner, plain string }{
		{"k1", "admin-key", "u1", "sk-gw-adminkey-0001"},
		{"k2", "dogge-key", "u2", "sk-gw-dogge-key-0002"},
	} {
		enc, err := crypto.Encrypt([]byte(k.plain), h.masterKey)
		if err != nil {
			t.Fatalf("encrypt key: %v", err)
		}
		sum := sha256.Sum256([]byte(k.plain))
		ak := &store.AccessKey{
			ID: k.id, KeyHash: hex.EncodeToString(sum[:]), KeyPrefix: k.plain[:11] + "...",
			KeyEnc: enc, Name: k.name, Enabled: true, UserID: k.owner,
			AllowedModels: []string{"alpha-chat"},
		}
		if err := st.CreateAccessKey(ctx, ak); err != nil {
			t.Fatalf("seed key %s: %v", k.name, err)
		}
	}

	// 路由：Deep 指向 alpha-chat，链上还挂了 beta-pro（跨 provider）。
	r := &store.Route{ID: "r1", PublicName: "Deep", ProviderID: "p1",
		UpstreamModelID: "m1", Enabled: true, FailoverEnabled: true}
	if err := st.CreateRoute(ctx, r); err != nil {
		t.Fatalf("seed route: %v", err)
	}
	// CreateRoute 自带「种链首目标」（position 0 = p1/m1），所以这里只补
	// 第二个目标；再建一条 position 0 会撞 route_targets 的唯一约束。
	rt := &store.RouteTarget{ID: "rt2", RouteID: "r1",
		ProviderID: "p2", UpstreamModelID: "m3", Position: 1, Enabled: true}
	if err := st.RunTx(ctx, func(ctx context.Context, tx store.Tx) error {
		return st.CreateRouteTargetTx(ctx, tx, rt)
	}); err != nil {
		t.Fatalf("seed route target: %v", err)
	}
}

// exportSealed 导出并返回加密包装体（导入侧要的就是这个形态）。
func exportSealed(t *testing.T, h *ConfigTransferHandler) configExport {
	t.Helper()
	out, _ := exportBody(t, h, "")
	sealed, err := sealExport(out, "v4-import-pass")
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	return sealed
}

// TestExport_IncludesIdentitiesAndRoutes 钉住导出体真的带上了四个新段。
func TestExport_IncludesIdentitiesAndRoutes(t *testing.T) {
	h, _ := transferFixture(t)
	seedIdentities(t, h)

	out, _ := exportBody(t, h, "")
	if got := len(out.UserGroups); got != 1 {
		t.Fatalf("用户分组导出 %d 条，期望 1", got)
	}
	if send := out.UserGroups[0]; send.Name != "vip" || len(send.Models) != 1 || send.Models[0] != "alpha-chat" {
		t.Fatalf("分组导出内容不对：%+v", send)
	}
	if got := len(out.Users); got != 2 {
		t.Fatalf("用户导出 %d 条，期望 2", got)
	}
	if got := len(out.AccessKeys); got != 2 {
		t.Fatalf("访问密钥导出 %d 条，期望 2", got)
	}
	if got := len(out.Routes); got != 1 {
		t.Fatalf("路由导出 %d 条，期望 1", got)
	}
	if r := out.Routes[0]; r.PublicName != "Deep" || len(r.Targets) != 2 {
		t.Fatalf("路由导出内容不对：%+v", r)
	}
	if r := out.Routes[0]; r.Targets[1].ProviderSlug != "beta" || r.Targets[1].ModelID != "beta-pro" {
		t.Fatalf("路由第二个目标应解析成 beta/beta-pro，实际 %+v", r.Targets[1])
	}

	// 余额语义：admin 是 0（有钱但花光了），dogge 是 nil（不限额）。
	byName := map[string]userExport{}
	for _, u := range out.Users {
		byName[u.Username] = u
	}
	if b := byName["admin"].BalanceCents; b == nil || *b != 0 {
		t.Fatalf("admin 的余额应导出 0（不是 nil），实际 %v", b)
	}
	if b := byName["dogge"].BalanceCents; b != nil {
		t.Fatalf("dogge 的余额应导出 nil（不限额），实际 %v", *b)
	}
	// 口令哈希与明文密钥：缺了这两样，导入后账号登不进、密钥永久失效。
	if byName["dogge"].PasswordHash == "" {
		t.Log("（提示）dogge 没有口令哈希，说明种子没设密码；本条不断言")
	}
	for _, k := range out.AccessKeys {
		if k.Key == "" {
			t.Fatalf("访问密钥 %q 的明文为空的 —— 哈希无法重建密钥", k.Name)
		}
	}
}

// TestImport_IdentitiesRoundTrip 是 v4 的主用例：导出源库、导入空库、
// 逐项核对用户/余额/密钥/路由真的搬过去了。
func TestImport_IdentitiesRoundTrip(t *testing.T) {
	src, _ := transferFixture(t)
	seedIdentities(t, src)
	sealed := exportSealed(t, src)

	dst := newTransferHandler(t)
	resp := importInto(t, dst, importRequest{Data: sealed, Passphrase: "v4-import-pass"})

	if resp.UserGroupsCreated != 1 || resp.UsersCreated != 2 ||
		resp.AccessKeysCreated != 2 || resp.RoutesCreated != 1 {
		t.Fatalf("计数不对：分组 %d 用户 %d 密钥 %d 路由 %d（warnings=%v）",
			resp.UserGroupsCreated, resp.UsersCreated,
			resp.AccessKeysCreated, resp.RoutesCreated, resp.Warnings)
	}

	ctx := context.Background()
	users, err := dst.store.ListUsers(ctx)
	if err != nil {
		t.Fatalf("list users: %v", err)
	}
	byName := map[string]store.User{}
	for _, u := range users {
		byName[u.Username] = u
	}
	if u, ok := byName["dogge"]; !ok {
		t.Fatalf("用户 dogge 没导进来，现有 %v", keysOfUsers(users))
	} else if !u.Unlimited {
		t.Fatalf("dogge 应是不限额，导入后变成了有限额")
	}
	if u, ok := byName["admin"]; !ok {
		t.Fatalf("用户 admin 没导进来")
	} else if u.Unlimited || u.BalanceCents != 0 {
		t.Fatalf("admin 应是 0 元余额（有限额），导入后 Unlimited=%v balance=%d",
			u.Unlimited, u.BalanceCents)
	}

	// 密钥：明文要真的能反解出原来那串，哈希要与明文自洽。
	keys, err := dst.store.ListAccessKeys(ctx)
	if err != nil {
		t.Fatalf("list keys: %v", err)
	}
	if len(keys) != 2 {
		t.Fatalf("密钥导入 %d 条，期望 2", len(keys))
	}
	wantPlain := map[string]string{"admin-key": "sk-gw-adminkey-0001", "dogge-key": "sk-gw-dogge-key-0002"}
	for _, k := range keys {
		blob, found, err := dst.store.AccessKeySecret(ctx, k.ID)
		if err != nil || !found {
			t.Fatalf("读密钥 %s 的密文失败：found=%v err=%v", k.Name, found, err)
		}
		plain, derr := decryptSecret(blob, dst.masterKey)
		if derr != nil {
			t.Fatalf("解密钥 %s 失败：%v", k.Name, derr)
		}
		if string(plain) != wantPlain[k.Name] {
			t.Fatalf("密钥 %s 明文 = %q，期望 %q", k.Name, string(plain), wantPlain[k.Name])
		}
		sum := sha256.Sum256([]byte(plain))
		if k.KeyHash != hex.EncodeToString(sum[:]) {
			t.Fatalf("密钥 %s 的哈希与明文不自洽 —— 导入后这把 key 会永久失效", k.Name)
		}
	}

	// 路由链：两条目标都要在，且顺序与 position 对齐。
	routes, err := dst.store.ListRoutes(ctx)
	if err != nil {
		t.Fatalf("list routes: %v", err)
	}
	if len(routes) != 1 || routes[0].PublicName != "Deep" {
		t.Fatalf("路由导入不对：%+v", routes)
	}
	targets, err := dst.store.ListRouteTargets(ctx, routes[0].ID)
	if err != nil {
		t.Fatalf("list targets: %v", err)
	}
	if len(targets) != 2 {
		t.Fatalf("链目标导入 %d 条，期望 2", len(targets))
	}
	if targets[0].Position != 0 || targets[1].Position != 1 {
		t.Fatalf("链目标的位置没按顺序落：%+v", targets)
	}
	// 链首要与 route 的目标列一致（SyncHeadTarget 的语义）。
	if routes[0].UpstreamModelID != targets[0].UpstreamModelID {
		t.Fatalf("route 的目标列 %q 与链首 %q 不一致",
			routes[0].UpstreamModelID, targets[0].UpstreamModelID)
	}
}

// TestImport_SkipsRoutesOnNameConflict 钉住「路由撞名不改名」：
// 公开名是调用方代码里的地址，加 -2 后缀等于让那条客户端指向不存在的模型。
func TestImport_SkipsRoutesOnNameConflict(t *testing.T) {
	src, _ := transferFixture(t)
	seedIdentities(t, src)
	sealed := exportSealed(t, src)

	dst := newTransferHandler(t)
	// 先导一遍把 Deep 建出来（同时把上游/模型建出来，路由的链目标才有外键可指），
	// 第二遍就是「同一个文件导第二次」——这正是运维重试时会发生的事。
	first := importInto(t, dst, importRequest{Data: sealed, Passphrase: "v4-import-pass"})
	if first.RoutesCreated != 1 {
		t.Fatalf("首遍应建 1 条路由，实际 %d（warnings=%v）", first.RoutesCreated, first.Warnings)
	}
	resp := importInto(t, dst, importRequest{Data: sealed, Passphrase: "v4-import-pass"})

	if resp.RoutesSkipped != 1 || resp.RoutesCreated != 0 {
		t.Fatalf("撞名应跳过：created=%d skipped=%d", resp.RoutesCreated, resp.RoutesSkipped)
	}
	routes, _ := dst.store.ListRoutes(context.Background())
	// 库里应仍只有首遍那条，没有 Deep-2 之类的新名字。
	if len(routes) != 1 || routes[0].PublicName != "Deep" {
		t.Fatalf("撞名后不应新建路由：%+v", routes)
	}
}

// TestImport_SkipsRouteWithBrokenTarget 钉住「目标解析不到就整条跳过」：
// 半条链比没有链更难排查（请求失败时说不清是目标挂了还是链本来就不全）。
func TestImport_SkipsRouteWithBrokenTarget(t *testing.T) {
	dst := newTransferHandler(t)
	body := configExport{
		Routes: []routeExport{{
			PublicName: "Broken",
			Enabled:    true,
			Targets: []routeTargetExport{
				{ProviderSlug: "ghost", ModelID: "nope", Enabled: true},
			},
		}},
	}
	resp := importInto(t, dst, importRequest{Data: body})
	if resp.RoutesCreated != 0 || resp.RoutesSkipped != 1 {
		t.Fatalf("目标解析不到应跳过整条：created=%d skipped=%d", resp.RoutesCreated, resp.RoutesSkipped)
	}
	if routes, _ := dst.store.ListRoutes(context.Background()); len(routes) != 0 {
		t.Fatalf("断链路由不应落库：%+v", routes)
	}
}

// TestImport_SkipsKeyWithoutOwner 钉住「没有归属用户的密钥直接跳过」：
// 无归属 key 在鉴权时一律 401，建一把永远用不了的 key 只会让人以为导入成功了。
func TestImport_SkipsKeyWithoutOwner(t *testing.T) {
	dst := newTransferHandler(t)
	yes := true
	body := configExport{
		AccessKeys: []accessKeyExport{{
			Key: "sk-gw-orphan-key", Name: "orphan", User: "nobody", Enabled: &yes,
		}},
	}
	resp := importInto(t, dst, importRequest{Data: body})
	if resp.AccessKeysCreated != 0 || resp.AccessKeysSkipped != 1 {
		t.Fatalf("无归属密钥应跳过：created=%d skipped=%d", resp.AccessKeysCreated, resp.AccessKeysSkipped)
	}
	if len(resp.Warnings) == 0 {
		t.Fatal("跳过无归属密钥必须给出告警，否则用户以为导入成功了")
	}
}

// TestImport_AccessKeysIdempotent 钉住「同一把 key 导两次不建两条」。
func TestImport_AccessKeysIdempotent(t *testing.T) {
	dst := newTransferHandler(t)
	pwHash := store.RoleUser
	_ = pwHash
	body := configExport{
		Users:      []userExport{{Username: "dogge", Role: store.RoleUser, Status: store.UserStatusActive}},
		AccessKeys: []accessKeyExport{{Key: "sk-gw-dup-key", Name: "dup", User: "dogge", Enabled: boolPtr(true)}},
	}
	first := importInto(t, dst, importRequest{Data: body})
	if first.AccessKeysCreated != 1 {
		t.Fatalf("首次导入应建 1 把密钥，实际 %d（warnings=%v）", first.AccessKeysCreated, first.Warnings)
	}
	second := importInto(t, dst, importRequest{Data: body})
	if second.AccessKeysCreated != 0 || second.AccessKeysSkipped != 1 {
		t.Fatalf("第二次应跳过同一把密钥：created=%d skipped=%d",
			second.AccessKeysCreated, second.AccessKeysSkipped)
	}
	if keys, _ := dst.store.ListAccessKeys(context.Background()); len(keys) != 1 {
		t.Fatalf("库里密钥应只有 1 条，实际 %d", len(keys))
	}
}

// TestImport_DryRunCountsIdentitiesButWritesNothing 钉住干跑：
// 计数要能反映真实结果（否则预览骗人），但一个字节都不许写。
func TestImport_DryRunCountsIdentitiesButWritesNothing(t *testing.T) {
	src, _ := transferFixture(t)
	seedIdentities(t, src)
	sealed := exportSealed(t, src)

	dst := newTransferHandler(t)
	resp := importInto(t, dst, importRequest{Data: sealed, Passphrase: "v4-import-pass", DryRun: true})
	if resp.UserGroupsCreated != 1 || resp.UsersCreated != 2 ||
		resp.AccessKeysCreated != 2 || resp.RoutesCreated != 1 {
		t.Fatalf("干跑计数不对：分组 %d 用户 %d 密钥 %d 路由 %d",
			resp.UserGroupsCreated, resp.UsersCreated,
			resp.AccessKeysCreated, resp.RoutesCreated)
	}
	ctx := context.Background()
	if users, _ := dst.store.ListUsers(ctx); len(users) != 0 {
		t.Fatalf("干跑写了用户：%v", keysOfUsers(users))
	}
	if keys, _ := dst.store.ListAccessKeys(ctx); len(keys) != 0 {
		t.Fatalf("干跑写了密钥：%d 条", len(keys))
	}
	if groups, _ := dst.store.ListGroups(ctx); len(groups) != 0 {
		t.Fatalf("干跑写了分组：%d 条", len(groups))
	}
	if routes, _ := dst.store.ListRoutes(ctx); len(routes) != 0 {
		t.Fatalf("干跑写了路由：%d 条", len(routes))
	}
}

// TestImport_UserConflictKeepsExisting 钉住「用户撞名跳过、不动既有账号」：
// 覆盖会改掉别人的密码与余额，那是迁移变入侵的分界线。
func TestImport_UserConflictKeepsExisting(t *testing.T) {
	dst := newTransferHandler(t)
	ctx := context.Background()
	existing := &store.User{ID: "u1", Username: "dogge", Role: store.RoleAdmin,
		Status: store.UserStatusActive, BalanceCents: 12345}
	if err := dst.store.CreateUser(ctx, existing); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	body := configExport{Users: []userExport{{
		Username: "DOGGE", Role: store.RoleUser, Status: store.UserStatusActive,
		BalanceCents: nil,
	}}}
	resp := importInto(t, dst, importRequest{Data: body})
	if resp.UsersCreated != 0 || resp.UsersSkipped != 1 {
		t.Fatalf("撞名应跳过：created=%d skipped=%d", resp.UsersCreated, resp.UsersSkipped)
	}
	users, _ := dst.store.ListUsers(ctx)
	if len(users) != 1 {
		t.Fatalf("撞名后不应新建用户：%d 条", len(users))
	}
	// 大小写不同的同名用户在 COLLATE NOCASE 下是同一个人，既有角色与余额必须原样。
	if users[0].Role != store.RoleAdmin || users[0].BalanceCents != 12345 {
		t.Fatalf("既有账号被改动了：role=%s balance=%d", users[0].Role, users[0].BalanceCents)
	}
}

func keysOfUsers(users []store.User) []string {
	out := make([]string, 0, len(users))
	for _, u := range users {
		out = append(out, u.Username)
	}
	return out
}

func boolPtr(v bool) *bool { return &v }
