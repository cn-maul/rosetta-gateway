package auth

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"

	"github.com/cn-maul/rosetta-gateway/internal/snapshot"
)

const (
	testKey         = "sk-gw-test-key"
	testUserID      = "u1"
	testKeyID       = "k1"
	disabledUser    = "u3"
	orphanKey       = "sk-gw-orphan"      // user_id 为空（迁移前的存量 key）
	ghostKey        = "sk-gw-ghost"       // user_id 指向不存在的行
	disabledUserKey = "sk-gw-disabled-u3" // 归属一个已被禁用的用户
)

func hashOf(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

// initSnap 装一份测试快照。
//
// 快照**只能整体构造后一次性发布**，不能构造完再往 map 里塞 —— 已发布的
// 快照会被并发请求无锁读取（见 snapshot.Snapshot 的并发契约）。所以所有
// key 都必须在这里就位。
//
// # groupAllow 要写两处，这是有意的
//
// 真实链路里 `KeySnapshot.GroupModelAllow` 是 **rebuild 已折算好的最终结果**
// （key 级组覆盖在此之前就并入，见 rebuild.go 的注释），热路径不再判断
// 「该用 key 的组还是用户的组」。所以测试里也必须给这个**已折算**的形态，
// 用户那一行只是为了完整性而保留。
//
// 漏掉它会得到「零值 = 拒绝全部」—— 那是 ModelAllow 刻意的 fail-closed
// 默认值，症状是所有模型都不可用，提示很明确。
func initSnap(t *testing.T, keyAllow, groupAllow snapshot.ModelAllow) {
	t.Helper()
	snapshot.Init(&snapshot.Snapshot{
		KeysByHash: map[string]*snapshot.KeySnapshot{
			hashOf(testKey): {
				ID: testKeyID, Name: "t", Enabled: true,
				UserID: testUserID, AllowedModels: keyAllow,
				GroupModelAllow: groupAllow,
			},
			hashOf(orphanKey):       {ID: "k-orphan", Enabled: true, UserID: "", AllowedModels: snapshot.AllowAll(), GroupModelAllow: snapshot.AllowAll()},
			hashOf(ghostKey):        {ID: "k-ghost", Enabled: true, UserID: "ghost", AllowedModels: snapshot.AllowAll(), GroupModelAllow: snapshot.AllowAll()},
			hashOf(disabledUserKey): {ID: "k-disabled", Enabled: true, UserID: disabledUser, AllowedModels: snapshot.AllowAll(), GroupModelAllow: snapshot.AllowAll()},
		},
		UsersByID: map[string]*snapshot.UserSnapshot{
			testUserID: {ID: testUserID, Name: "alice", Role: "user", Status: "active",
				AuthVersion: 1, AllowedModels: groupAllow},
			disabledUser: {ID: disabledUser, Name: "carol", Role: "user", Status: "disabled",
				AuthVersion: 1, AllowedModels: snapshot.AllowAll()},
		},
	})
	t.Cleanup(func() { snapshot.Init(&snapshot.Snapshot{}) })
}

func reqWithKey(key string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	r.Header.Set("Authorization", "Bearer "+key)
	return r
}

// 鉴权要带出归属用户与两个维度的白名单 —— 这三样都是热路径判权限的依据。
func TestAuthenticate_CarriesIdentityAndAllowances(t *testing.T) {
	initSnap(t, snapshot.AllowOnly([]string{"a", "b"}), snapshot.AllowOnly([]string{"b", "c"}))

	ctx, err := Authenticate(reqWithKey(testKey))
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if ctx.KeyID != testKeyID || ctx.UserID != testUserID {
		t.Fatalf("identity: key=%q user=%q", ctx.KeyID, ctx.UserID)
	}
	// 求交：key 允许 {a,b}、组允许 {b,c} → 只有 b。
	if !ctx.AllowsModel("b") {
		t.Error("b must be allowed (in both whitelists)")
	}
	if ctx.AllowsModel("a") {
		t.Error("a must be denied — key 级虽允许，但组白名单里没有")
	}
	if ctx.AllowsModel("c") {
		t.Error("c must be denied — 组允许但 key 不允许")
	}
	if ctx.KeyModelAllow.Unrestricted || ctx.GroupModelAllow.Unrestricted {
		t.Errorf("both dimensions should be restricted; got key=%+v group=%+v",
			ctx.KeyModelAllow, ctx.GroupModelAllow)
	}
}

// 两个维度都不限制 = 不限制全部模型（改造前的行为，必须保持）。
func TestAuthenticate_NoWhitelistAllowsEverything(t *testing.T) {
	initSnap(t, snapshot.AllowAll(), snapshot.AllowAll())

	ctx, err := Authenticate(reqWithKey(testKey))
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	for _, m := range []string{"gpt-4o", "claude-sonnet", "任意名字"} {
		if !ctx.AllowsModel(m) {
			t.Errorf("model %q must be allowed when no whitelist is configured", m)
		}
	}
	if !ctx.KeyModelAllow.Unrestricted || !ctx.GroupModelAllow.Unrestricted {
		t.Errorf("both dimensions should be unrestricted; got key=%+v group=%+v",
			ctx.KeyModelAllow, ctx.GroupModelAllow)
	}
}

// 只有 key 级限制、组不限制 → key 级生效（P1 的向后兼容方向：
// 「组没配」不能把 key 自己配的限制冲掉）。
func TestAuthenticate_KeyWhitelistAppliesWithoutGroup(t *testing.T) {
	initSnap(t, snapshot.AllowOnly([]string{"a"}), snapshot.AllowAll())

	ctx, err := Authenticate(reqWithKey(testKey))
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if !ctx.AllowsModel("a") || ctx.AllowsModel("b") {
		t.Error("key-level whitelist must apply on its own")
	}
}

// 只有组限制、key 不限制 → 组级生效。
func TestAuthenticate_GroupWhitelistAppliesWithoutKeyLimit(t *testing.T) {
	initSnap(t, snapshot.AllowAll(), snapshot.AllowOnly([]string{"a"}))

	ctx, err := Authenticate(reqWithKey(testKey))
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if !ctx.AllowsModel("a") || ctx.AllowsModel("b") {
		t.Error("group whitelist must apply on its own")
	}
}

// 无归属 key 必须拒绝：user_id 为空 = 迁移前的存量 key，
// 「不发新 key 就不给用」（MULTIUSER.md §5.1 修订）。
func TestAuthenticate_RejectsUnownedKey(t *testing.T) {
	initSnap(t, snapshot.AllowAll(), snapshot.AllowAll())

	if _, err := Authenticate(reqWithKey(orphanKey)); err != ErrKeyUnowned {
		t.Fatalf("err = %v, want ErrKeyUnowned", err)
	}
}

// user_id 指向不存在的行（数据不一致）→ 同样拒绝。
// 放行等于让一个「谁的 key 说不清」的凭据通过。
func TestAuthenticate_RejectsKeyWhoseUserIsMissing(t *testing.T) {
	initSnap(t, snapshot.AllowAll(), snapshot.AllowAll())

	if _, err := Authenticate(reqWithKey(ghostKey)); err != ErrKeyUnowned {
		t.Fatalf("err = %v, want ErrKeyUnowned", err)
	}
}

// 归属用户被禁用 → 拒绝，并且要与 ErrKeyUnowned 区分开：
// 前者是「这个人的门锁了」，后者是「这把钥匙不算数」。
func TestAuthenticate_RejectsDisabledUser(t *testing.T) {
	initSnap(t, snapshot.AllowAll(), snapshot.AllowAll())

	if _, err := Authenticate(reqWithKey(disabledUserKey)); err != ErrUserDisabled {
		t.Fatalf("err = %v, want ErrUserDisabled", err)
	}
}

// ---- P2：有效期 / 来源 IP / key 级组覆盖 ----

// initSnapKey 发布一份只含指定那把 key 的快照（外加固定的三个对照 key），
// 供需要逐项定制 KeySnapshot 字段的用例使用。
func initSnapKey(t *testing.T, key *snapshot.KeySnapshot) {
	t.Helper()
	key.KeyHash = hashOf(testKey)
	key.Name = "t"
	if key.Enabled == false && key.ID == "" {
		key.Enabled = true
	}
	snapshot.Init(&snapshot.Snapshot{
		KeysByHash: map[string]*snapshot.KeySnapshot{
			hashOf(testKey):         key,
			hashOf(orphanKey):       {ID: "k-orphan", Enabled: true, UserID: "", AllowedModels: snapshot.AllowAll(), GroupModelAllow: snapshot.AllowAll()},
			hashOf(ghostKey):        {ID: "k-ghost", Enabled: true, UserID: "ghost", AllowedModels: snapshot.AllowAll(), GroupModelAllow: snapshot.AllowAll()},
			hashOf(disabledUserKey): {ID: "k-disabled", Enabled: true, UserID: disabledUser, AllowedModels: snapshot.AllowAll(), GroupModelAllow: snapshot.AllowAll()},
		},
		UsersByID: map[string]*snapshot.UserSnapshot{
			testUserID: {ID: testUserID, Name: "alice", Role: "user", Status: "active",
				AuthVersion: 1, AllowedModels: snapshot.AllowAll()},
		},
	})
	t.Cleanup(func() { snapshot.Init(&snapshot.Snapshot{}) })
}

func TestAuthenticate_Expiry(t *testing.T) {
	now := time.Now().UnixMilli()

	cases := []struct {
		name      string
		expiresAt int64
		wantErr   error
	}{
		{"永不过期", 0, nil},
		{"尚未到期", now + 3600_000, nil},
		{"刚好到点即失效", now - 1, ErrKeyExpired},
		{"早已过期", now - 86400_000, ErrKeyExpired},
	}
	for _, tc := range cases {
		initSnapKey(t, &snapshot.KeySnapshot{
			ID: testKeyID, Enabled: true, UserID: testUserID,
			AllowedModels: snapshot.AllowAll(), GroupModelAllow: snapshot.AllowAll(),
			ExpiresAt: tc.expiresAt,
		})
		_, err := Authenticate(reqWithKey(testKey))
		if tc.wantErr == nil && err != nil {
			t.Errorf("%s: err = %v, want nil", tc.name, err)
		}
		if tc.wantErr != nil && err != tc.wantErr {
			t.Errorf("%s: err = %v, want %v", tc.name, err, tc.wantErr)
		}
	}
}

func TestAuthenticate_IPWhitelist(t *testing.T) {
	base := func(nets []netip.Prefix) *snapshot.KeySnapshot {
		return &snapshot.KeySnapshot{
			ID: testKeyID, Enabled: true, UserID: testUserID,
			AllowedModels: snapshot.AllowAll(), GroupModelAllow: snapshot.AllowAll(),
			AllowedNets: nets,
		}
	}
	mustParse := func(s string) netip.Prefix {
		p, err := netip.ParsePrefix(s)
		if err != nil {
			t.Fatalf("parse %q: %v", s, err)
		}
		return p
	}

	cases := []struct {
		name    string
		nets    []netip.Prefix
		remote  string
		wantErr error
	}{
		{"未配置限制 → 任意来源", nil, "203.0.113.9:1234", nil},
		{"命中 CIDR", []netip.Prefix{mustParse("10.0.0.0/8")}, "10.1.2.3:1234", nil},
		{"落在 CIDR 之外", []netip.Prefix{mustParse("10.0.0.0/8")}, "203.0.113.9:1234", ErrIPNotAllowed},
		{"多段里命中其一", []netip.Prefix{mustParse("10.0.0.0/8"), mustParse("192.168.0.0/16")}, "192.168.1.1:1", nil},
		// 库里那一列解析失败时是「非 nil 的空切片」→ 一条都不允许。
		// 这条断言存在的意义：把它误改成 nil 就成了静默放权。
		{"配置损坏（全拒）", []netip.Prefix{}, "10.1.2.3:1234", ErrIPNotAllowed},
		// IPv4-mapped IPv6（::ffff:a.b.c.d）是 Go netip 里的独立类型，
		// 不 Unmap 就会把正常 IPv4 客户端全部拦掉。
		{"IPv4-mapped IPv6 命中 IPv4 白名单", []netip.Prefix{mustParse("10.0.0.0/8")}, "[::ffff:10.1.2.3]:1234", nil},
		{"IPv4-mapped IPv6 不命中", []netip.Prefix{mustParse("10.0.0.0/8")}, "[::ffff:203.0.113.9]:1234", ErrIPNotAllowed},
	}
	for _, tc := range cases {
		initSnapKey(t, base(tc.nets))
		r := reqWithKey(testKey)
		r.RemoteAddr = tc.remote
		_, err := Authenticate(r)
		if tc.wantErr == nil && err != nil {
			t.Errorf("%s: err = %v, want nil", tc.name, err)
		}
		if tc.wantErr != nil && err != tc.wantErr {
			t.Errorf("%s: err = %v, want %v", tc.name, err, tc.wantErr)
		}
	}
}

// X-Forwarded-For 由客户端随意填写，采信它等于让 IP 白名单形同虚设。
func TestAuthenticate_IgnoresForwardedForHeader(t *testing.T) {
	p, _ := netip.ParsePrefix("10.0.0.0/8")
	initSnapKey(t, &snapshot.KeySnapshot{
		ID: testKeyID, Enabled: true, UserID: testUserID,
		AllowedModels: snapshot.AllowAll(), GroupModelAllow: snapshot.AllowAll(),
		AllowedNets: []netip.Prefix{p},
	})

	r := reqWithKey(testKey)
	r.RemoteAddr = "203.0.113.9:1234" // 真实来源不在白名单内
	r.Header.Set("X-Forwarded-For", "10.0.0.1")
	r.Header.Set("X-Real-IP", "10.0.0.1")

	if _, err := Authenticate(r); err != ErrIPNotAllowed {
		t.Fatalf("err = %v, want ErrIPNotAllowed —— 采信 XFF 就等于白名单可伪造", err)
	}
}

// key 级组覆盖：快照里存的是**已折算**的最终结果，热路径直接用它。
func TestAuthenticate_UsesResolvedGroupAllowance(t *testing.T) {
	// 归属用户所在组是「不限制」，但这把 key 被指定到了一个受限组 ——
	// 生效的必须是受限那一份（这正是 key 级组覆盖的目的：
	// 让管理员给某个人的某把 key 单独收紧）。
	initSnapKey(t, &snapshot.KeySnapshot{
		ID: testKeyID, Enabled: true, UserID: testUserID,
		AllowedModels:   snapshot.AllowAll(),
		GroupModelAllow: snapshot.AllowOnly([]string{"flash"}),
	})
	ctx, err := Authenticate(reqWithKey(testKey))
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if !ctx.AllowsModel("flash") || ctx.AllowsModel("pro") {
		t.Errorf("key 级组覆盖未生效: Allows(flash)=%v Allows(pro)=%v",
			ctx.AllowsModel("flash"), ctx.AllowsModel("pro"))
	}
}
