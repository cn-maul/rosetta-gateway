package auth

// 管理员数据面锁定（2026-10 控制面/数据面分离）的鉴权层用例。
//
// 业务要求原话：「管理员账号不能调用模型……管理员需要新建普通用户账户来调用API，
// 这样的逻辑，管理员账号只负责管理网关。」
//
// 落点选在 auth.Authenticate 而不是 handleIngress，是因为数据面所有入口
// （/v1 推理、/v1/models、billing 查询、org 端点）都只做这一次鉴权；
// 收敛到这一处，口径才不会在各端点之间漂移。

import (
	"testing"

	"github.com/cn-maul/rosetta-gateway/internal/snapshot"
)

const (
	adminUserID  = "u-admin"
	adminUserKey = "sk-gw-admin-key"
)

// initAdminLockSnap 装一份含「管理员 key + 普通用户 key」的快照。
//
// 两个方向都必须覆盖：只测管理员被拒，证明不了「没有误伤正常用户」——
// 而后者才是回归的真实风险（鉴权层一旦写错，代价是**所有**用户都不能用）。
func initAdminLockSnap(t *testing.T) {
	t.Helper()
	snapshot.Init(&snapshot.Snapshot{
		KeysByHash: map[string]*snapshot.KeySnapshot{
			hashOf(adminUserKey): {
				ID: "k-admin", Name: "admin-key", Enabled: true,
				UserID: adminUserID, AllowedModels: snapshot.AllowAll(),
				GroupModelAllow: snapshot.AllowAll(),
			},
			hashOf(testKey): {
				ID: testKeyID, Name: "t", Enabled: true,
				UserID: testUserID, AllowedModels: snapshot.AllowAll(),
				GroupModelAllow: snapshot.AllowAll(),
			},
			// 无归属 key（user_id 为空）：存量数据的真实形态，用来钉住
			// 「管理员策略不得抢占数据不一致的错误」。
			hashOf(orphanKey): {ID: "k-orphan", Enabled: true, UserID: "",
				AllowedModels: snapshot.AllowAll(), GroupModelAllow: snapshot.AllowAll()},
		},
		UsersByID: map[string]*snapshot.UserSnapshot{
			adminUserID: {ID: adminUserID, Name: "root", Role: userRoleAdmin,
				Status: snapshot.UserStatusActive, AuthVersion: 1,
				AllowedModels: snapshot.AllowAll()},
			testUserID: {ID: testUserID, Name: "alice", Role: "user",
				Status: snapshot.UserStatusActive, AuthVersion: 1,
				AllowedModels: snapshot.AllowAll()},
		},
	})
	t.Cleanup(func() { snapshot.Init(&snapshot.Snapshot{}) })
}

// 管理员的 key —— 即便它完全有效（启用、未过期、来源 IP 在白名单内、归属用户
// 启用）—— 也必须被拒绝，且报的是**专属**错误。
func TestAuthenticate_RejectsAdminKey(t *testing.T) {
	initAdminLockSnap(t)

	ctx, err := Authenticate(reqWithKey(adminUserKey))
	if err != ErrAdminCannotCallModel {
		t.Fatalf("err = %v, want ErrAdminCannotCallModel", err)
	}
	// 拒绝时不得返回任何可用上下文：那会让调用方误以为鉴权通过。
	if ctx != nil {
		t.Fatalf("被拒时不该返回 Context（否则调用方可能当成放行）：%+v", ctx)
	}
}

// 反向：同一份快照里普通用户的 key 必须**照常通过**。
//
// 这条与上面那条成对存在。鉴权层是全局热路径，管理员拦截一旦写得太宽
// （例如误判成「Role 非空即拒绝」「用户名含 admin 即拒绝」），症状是
// 全体用户 403 —— 而且看起来像「部署坏了」，很难定位。
func TestAuthenticate_NormalUserStillWorks(t *testing.T) {
	initAdminLockSnap(t)

	ctx, err := Authenticate(reqWithKey(testKey))
	if err != nil {
		t.Fatalf("普通用户不应被管理员策略波及：%v", err)
	}
	if ctx.UserID != testUserID {
		t.Fatalf("UserID = %q, want %q", ctx.UserID, testUserID)
	}
	// 白名单判定也必须照常工作（拦截不能顺手破坏鉴权的其它职责）。
	if !ctx.AllowsModel("gpt-4o") {
		t.Error("普通用户的模型白名单判定被管理员拦截破坏了")
	}
}

// IsAdminOwner 判据要与 ErrAdminCannotCallModel **完全一致**。
//
// 这条用例的作用是防止两条判据各写各的：一旦 auth 拦截用「角色 == admin」、
// 而 IsAdminOwner 用「IsAdmin() 或其他条件」，两者就会在某处漂移，
// 表现为「拦截生效了但 org 端点没收窄」这类极难查的不一致。
func TestContext_IsAdminOwnerMatchesLockout(t *testing.T) {
	initAdminLockSnap(t)

	if _, err := Authenticate(reqWithKey(adminUserKey)); err != ErrAdminCannotCallModel {
		t.Fatalf("管理员 key 应被拒（前置条件不成立）：%v", err)
	}

	// 用一个手工构造的 Context 走 IsAdminOwner（被拒时 Authenticate 不给 Context，
	// 只能自己构造 —— 这正是 IsAdminOwner 做成 Context 方法的原因：
	// 它服务于「已经鉴权成功」的调用方，例如 orgCostsScope）。
	admin := &Context{UserID: adminUserID}
	if !admin.IsAdminOwner() {
		t.Error("IsAdminOwner(管理员) = false，与 ErrAdminCannotCallModel 的判据不一致")
	}
	normal := &Context{UserID: testUserID}
	if normal.IsAdminOwner() {
		t.Error("IsAdminOwner(普通用户) = true，判据写反了")
	}
	// 查不到用户（数据不一致）必须**判非管理员**，而不是默认 true。
	// 反过来写的话，一个 user_id 失效的 key 会被当成管理员而在别处被放行。
	if (&Context{UserID: "u-ghost"}).IsAdminOwner() {
		t.Error("查不到归属用户时必须判非管理员（fail-closed 方向应是「不放行」）")
	}
}

// 判定顺序：管理员**同时**被禁用时，报「用户已禁用」而不是「管理员不能调模型」。
//
// 顺序反了会误导运维：账号被禁用时去查「为什么管理员不能调模型」是白费功夫，
// 正确处置是解禁账号。两个都是 403，不靠状态码区分，只能靠错误类型 ——
// 所以这条用例专门钉住**错误类型**。
func TestAuthenticate_DisabledAdminReportsDisabled(t *testing.T) {
	snapshot.Init(&snapshot.Snapshot{
		KeysByHash: map[string]*snapshot.KeySnapshot{
			hashOf(adminUserKey): {ID: "k-admin", Enabled: true, UserID: adminUserID,
				AllowedModels: snapshot.AllowAll(), GroupModelAllow: snapshot.AllowAll()},
		},
		UsersByID: map[string]*snapshot.UserSnapshot{
			adminUserID: {ID: adminUserID, Name: "root", Role: userRoleAdmin,
				Status: "disabled", AuthVersion: 1, AllowedModels: snapshot.AllowAll()},
		},
	})
	t.Cleanup(func() { snapshot.Init(&snapshot.Snapshot{}) })

	if _, err := Authenticate(reqWithKey(adminUserKey)); err != ErrUserDisabled {
		t.Fatalf("err = %v, want ErrUserDisabled —— 「账号被禁用」必须优先于「角色限制」上报", err)
	}
}

// 无归属 key 仍报 ErrKeyUnowned（管理员策略不得抢占前者的错误）。
//
// 存量数据里 user_id 为空的 key 是真实存在的形态（迁移前发的）。若管理员判定
// 排在它前面，这类 key 会得到一个语义完全错误的「管理员不能调模型」。
func TestAuthenticate_UnownedKeyNotMisreportedAsAdmin(t *testing.T) {
	initAdminLockSnap(t)

	if _, err := Authenticate(reqWithKey(orphanKey)); err != ErrKeyUnowned {
		t.Fatalf("err = %v, want ErrKeyUnowned（管理员策略不得抢占数据不一致的错误）", err)
	}
}

// 角色变更立即生效：管理员被降为普通用户后，快照一换就能调用，不需要动 key。
//
// 这是「判归属用户角色、而不是给 key 打标记」的设计收益所在
// （见 auth.go 的注释）。用例证明改角色这一条管理动作就足以恢复可用性。
func TestAuthenticate_DemotedAdminCanCallAgain(t *testing.T) {
	initAdminLockSnap(t)

	// 先确认被拒。
	if _, err := Authenticate(reqWithKey(adminUserKey)); err != ErrAdminCannotCallModel {
		t.Fatalf("前置条件不成立：err = %v", err)
	}

	// 降级为普通用户后重新发布快照（等价于管理面改角色触发 Reload）。
	snap := snapshot.Get()
	users := map[string]*snapshot.UserSnapshot{}
	for k, v := range snap.UsersByID {
		users[k] = v
	}
	demoted := *users[adminUserID] // 拷贝，不污染上一份快照里的对象
	demoted.Role = "user"
	users[adminUserID] = &demoted
	snap.UsersByID = users
	snapshot.Init(snap)

	if _, err := Authenticate(reqWithKey(adminUserKey)); err != nil {
		t.Fatalf("降级为普通用户后应可调用：%v", err)
	}
}

// 用户状态字面量与 snapshot/snapshot.go 的常量必须一致。
//
// 鉴权包里刻意复制了 "active"/"admin" 两个字面量（不 import store，避免把
// 数据库驱动拖进热路径依赖图）。复制就意味着存在「两处不同步」的风险 —
// 而不同步的症状是**静默**的（鉴权恒失败或恒放行）。这条用例把两处钉在一起。
func TestAuthenticate_RoleAndStatusLiteralsMatchSnapshotPackage(t *testing.T) {
	if userRoleAdmin != "admin" {
		t.Errorf("userRoleAdmin = %q，与 store.RoleAdmin 的字面量不一致", userRoleAdmin)
	}
	admin := &snapshot.UserSnapshot{Role: userRoleAdmin, Status: snapshot.UserStatusActive}
	if !admin.IsActive() {
		t.Error("用 userRoleAdmin 构造的用户应处于 active —— IsActive 的比较字面量已漂移")
	}
}

// 兜底：确认鉴权成功时才会拿到非 nil Context（管理员被拒时为 nil），
// 且普通用户的 Context 带齐热路径需要的全部字段。
func TestAuthenticate_ContextShapeForNormalUser(t *testing.T) {
	initAdminLockSnap(t)

	ctx, err := Authenticate(reqWithKey(testKey))
	if err != nil {
		t.Fatalf("普通用户不应被拦截：%v", err)
	}
	if ctx.KeyID != testKeyID || ctx.UserID != testUserID || ctx.Name != "t" {
		t.Fatalf("上下文字段不全：key=%q user=%q name=%q", ctx.KeyID, ctx.UserID, ctx.Name)
	}
	if ctx.UserStatus != snapshot.UserStatusActive {
		t.Errorf("UserStatus = %q, want active", ctx.UserStatus)
	}
	if ctx.IsAdminOwner() {
		t.Error("普通用户的 IsAdminOwner 必须为 false")
	}
	// 确认用的是 HTTP 头取 key 的真实路径（回归：若 extractKey 变了，
	// 这里会因「拿不到 key」而红，错误信息直指问题）。
	if reqWithKey(testKey).Header.Get("Authorization") != "Bearer "+testKey {
		t.Error("测试自身构造的请求缺 Authorization 头")
	}
}
