package upstream

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/cn-maul/rosetta-gateway/internal/config"
	"github.com/cn-maul/rosetta-gateway/internal/crypto"
	"github.com/cn-maul/rosetta-gateway/internal/store"
)

func newTestPool() *Pool {
	return NewPool(slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func newTestConfig() *config.Config {
	cfg := config.Default()
	cfg.Bootstrap.Providers = []config.BootstrapProvider{{
		Slug:     "testprov",
		Name:     "Test Provider",
		Protocol: "openai-chat",
		Endpoint: "https://example.invalid/v1",
		Credentials: []config.BootstrapCredential{
			{Label: "k1", APIKey: "sk-test-1"},
		},
		Models: []string{"test-model"},
	}}
	return cfg
}

// 已从配置中删除的 target，其熔断态必须在重建时被丢弃。
//
// 这条测试原先断言的是相反的行为（重建清空全部熔断态），理由是
// 「重建即重新探测」。但熔断状态与配置变更是正交的：任何 admin 写操作都会
// 触发一次重建（server.AutoReload），若重建清空熔断，运维改一个模型的
// context_width 就会让正在熔断中的坏上游立刻复活并被打满 —— 这不是
// 「重新探测」，是让熔断形同虚设。正确语义：仍在配置中的目标保留状态，
// 已删除的丢弃（targetID 每次保存链都重新生成，留着会单调堆积）。
func TestPoolBuildDropsHealthOfDeletedTargets(t *testing.T) {
	p := newTestPool()

	// threshold=1：一次失败即熔断，TargetAvailable 转 false。
	p.RecordTargetFailure("ghost#t0", 1)
	if p.TargetAvailable("ghost#t0") {
		t.Fatalf("expected target circuit-open after threshold reached")
	}

	// bootstrap 配置里没有 route_targets，liveTargetIDs 为空集 → 该条目应被丢弃。
	if err := p.BuildFromConfig(newTestConfig()); err != nil {
		t.Fatalf("build from config: %v", err)
	}
	if !p.TargetAvailable("ghost#t0") {
		t.Errorf("已删除 target 的熔断态应随重建丢弃，仍不可用")
	}
	if len(p.targets) != 0 {
		t.Errorf("targets map 应只剩存活target，len=%d", len(p.targets))
	}
}

// 仍在配置中的目标，其熔断态必须跨重建保留 —— 重建不等于恢复。
func TestInstall_PreservesHealthOfLiveTargets(t *testing.T) {
	p := newTestPool()
	if err := p.BuildFromConfig(newTestConfig()); err != nil {
		t.Fatalf("build: %v", err)
	}
	p.RecordTargetFailure("live#t0", 1)
	if p.TargetAvailable("live#t0") {
		t.Fatalf("前置条件不成立：目标未熔断")
	}

	// 模拟 admin 写操作后的重建，且该 target 仍在库中。
	p.Install(p.ProvidersSnapshot(), map[string]bool{"live#t0": true})

	if p.TargetAvailable("live#t0") {
		t.Errorf("重建把仍在配置中的目标的熔断态清掉了：改一个无关设置就能让坏上游复活")
	}
}

// TestTargetAvailable_IsPureQuery 钉住一条 P1 的修复。
//
// TargetAvailable 过去顺带把目标标成 halfOpen（占用探测名额）。调用方
// （handleIngress）会先对整条链问一遍，再按 failover_max_targets 预算截断，
// 被截掉的目标永远进不了请求循环，也就等不到 RecordTarget* 来释放名额。
// 结果：一个完全健康的备份目标因排在预算之外而永久被判「探测在途」，
// 主目标熔断期间整条链返回 502，必须人工调高预算才能恢复。
//
// 查询与领名额必须是两个动作。
func TestTargetAvailable_IsPureQuery(t *testing.T) {
	p := newTestPool()
	p.RecordTargetFailure("t#0", 1) // 熔断

	// 未到冷却期：不可用，且不该留下任何「探测在途」痕迹
	if p.TargetAvailable("t#0") {
		t.Fatalf("熔断期内应不可用")
	}
	if p.ClaimTargetProbe("t#0") {
		t.Fatalf("熔断期内不应领到探测名额")
	}
}

// ClaimTargetProbe 的 half-open 语义本身必须保留：冷却到期后只放一个。
func TestClaimTargetProbe_AllowsOnlyOneAfterCooldown(t *testing.T) {
	p := newTestPool()
	p.RecordTargetFailure("t#0", 1)
	// 熔断期内：查询与领名额都应拒绝
	if p.TargetAvailable("t#0") {
		t.Fatalf("熔断期内应不可用")
	}
	// 模拟「冷却刚到期」
	p.mu.Lock()
	p.targets["t#0"].until = time.Now().Add(-time.Second)
	p.mu.Unlock()

	if !p.ClaimTargetProbe("t#0") {
		t.Fatalf("冷却刚到期时应放行第一个探测请求")
	}
	if p.ClaimTargetProbe("t#0") {
		t.Errorf("探测名额已被占用，不应放行第二个（否则熔断-惊群重现）")
	}

	// 结果记账后名额释放
	p.RecordTargetSuccess("t#0")
	if !p.ClaimTargetProbe("t#0") {
		t.Errorf("探测成功后名额应释放，下一个请求仍能打")
	}
}

// 重建不得继承 halfOpen：它只对「当前这个在途请求」有意义，重建时那个
// 请求要么已记账、要么随旧池作废。继承下来 = 一个永远没人释放的名额。
func TestInstall_ClearsStaleHalfOpen(t *testing.T) {
	p := newTestPool()
	if err := p.BuildFromConfig(newTestConfig()); err != nil {
		t.Fatalf("build: %v", err)
	}
	p.RecordTargetFailure("live#t0", 1)
	p.mu.Lock()
	p.targets["live#t0"].until = time.Now().Add(-time.Second)
	p.mu.Unlock()
	if !p.ClaimTargetProbe("live#t0") {
		t.Fatalf("前置条件不成立：应能领到探测名额")
	}

	// 一次 admin 写操作触发的重建，target 仍在配置中。
	p.Install(p.ProvidersSnapshot(), map[string]bool{"live#t0": true})

	if !p.ClaimTargetProbe("live#t0") {
		t.Errorf("重建继承了 halfOpen：探测名额被永久占住，目标再也无法被尝试")
	}
}

// 重建失败绝不能留下空池：旧实现先清空 p.providers 再查库，ListProviders 一失败
// 池就空了，之后所有 /v1 请求都选不到上游，直到下一次成功的 reload。
func TestBuildFromStore_ErrorKeepsOldPool(t *testing.T) {
	p := newTestPool()
	cfg := newTestConfig()
	if err := p.BuildFromConfig(cfg); err != nil {
		t.Fatalf("build from config: %v", err)
	}
	if _, _, err := p.GetAnyClient("testprov"); err != nil {
		t.Fatalf("pool should serve before failed rebuild: %v", err)
	}

	// 用一个已关闭的 store 制造 ListProviders 失败。
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	st.Close()

	if err := p.BuildFromStore(context.Background(), st, nil, cfg); err == nil {
		t.Fatalf("expected error building from closed store")
	}

	// 失败的重建不得影响旧池：同一个 provider 仍然拿得到 client。
	if _, _, err := p.GetAnyClient("testprov"); err != nil {
		t.Fatalf("pool must keep serving after failed rebuild: %v", err)
	}
	// 目标熔断表也不得被失败重建清掉（Install 未执行）。
	p.RecordTargetFailure("t#x", 1)
	if p.TargetAvailable("t#x") {
		t.Fatalf("failed rebuild must not reset target health")
	}
}

// TestPrepareFromStore_ReportsFailures 是 P4「失败 provider 可见化」的池侧断言。
//
// 改造前，单个 provider 的凭据问题只在日志里 `continue` 掉，
// 调用方（reload）拿不到任何信息，界面上该 provider 看起来一切正常。
// 现在必须返回失败清单，否则下游的可见化就是空中楼阁。
//
// 用**错误的 masterKey** 制造解密失败：这是真实运维里最常见的成因
// （换了主密钥文件、迁移时漏带 .masterkey），不是刻意为难。
func TestPrepareFromStore_ReportsFailures(t *testing.T) {
	p := newTestPool()
	cfg := newTestConfig()
	ctx := context.Background()

	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	goodKey := make([]byte, 32)
	for i := range goodKey {
		goodKey[i] = byte(i)
	}
	wrongKey := make([]byte, 32)
	for i := range wrongKey {
		wrongKey[i] = byte(i + 1)
	}

	if err := st.CreateProvider(ctx, &store.Provider{
		ID: "p1", Slug: "prov", Name: "P",
		Endpoint: "https://example.invalid/v1", Protocol: "openai-chat", Enabled: true,
	}); err != nil {
		t.Fatalf("create provider: %v", err)
	}
	enc, err := crypto.Encrypt([]byte("sk-real-key"), goodKey)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if err := st.CreateCredential(ctx, &store.Credential{
		ProviderID: "p1", Label: "default", APIKeyEnc: enc, Enabled: true,
	}); err != nil {
		t.Fatalf("create credential: %v", err)
	}

	// 正确密钥：建得起来，没有失败。
	providers, failures, err := p.PrepareFromStore(ctx, st, goodKey, cfg)
	if err != nil {
		t.Fatalf("prepare with good key: %v", err)
	}
	if len(failures) != 0 {
		t.Fatalf("expected no failures with correct key, got %+v", failures)
	}
	if len(providers["prov"].Credentials) != 1 {
		t.Fatalf("credential should be usable with correct key")
	}

	// 错误密钥：provider 一条可用凭据都没有 → 必须报「未就绪」且带原因与阶段。
	providers2, failures2, err := p.PrepareFromStore(ctx, st, wrongKey, cfg)
	if err != nil {
		// 整体失败会挡住其它 provider 的更新，那不是我们要的语义。
		t.Fatalf("broken credentials must not fail the whole prepare: %v", err)
	}
	if len(failures2) == 0 {
		t.Fatal("provider with undecryptable credentials must be reported")
	}
	var found bool
	for _, f := range failures2 {
		if f.Slug != "prov" {
			continue
		}
		found = true
		if f.Reason == "" {
			t.Errorf("failure must carry a reason, got %+v", f)
		}
		if f.Stage == "" {
			t.Errorf("failure must carry a stage, got %+v", f)
		}
	}
	if !found {
		t.Fatalf("no failure reported for slug prov: %+v", failures2)
	}
	// 未就绪的 provider 不该带着「能用的凭据」出现在池里。
	if len(providers2["prov"].Credentials) != 0 {
		t.Errorf("provider with no usable credentials must not expose any: %+v", providers2["prov"].Credentials)
	}
}

// TestPrepareFromStore_PartialCredentialFailure 守「部分凭据失败不算未就绪」。
//
// 两条凭据里一条坏、一条好，provider 仍能服务 —— 若把它整体标成不可用，
// 界面会谎报故障（明明能用却说坏了），运维会去查一个不存在的问题。
// 但那条坏凭据仍要可见，所以 failures 里必须有它。
func TestPrepareFromStore_PartialCredentialFailure(t *testing.T) {
	p := newTestPool()
	cfg := newTestConfig()
	ctx := context.Background()

	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	if err := st.CreateProvider(ctx, &store.Provider{
		ID: "p1", Slug: "prov", Name: "P",
		Endpoint: "https://example.invalid/v1", Protocol: "openai-chat", Enabled: true,
	}); err != nil {
		t.Fatalf("create provider: %v", err)
	}
	enc, err := crypto.Encrypt([]byte("sk-good"), key)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if err := st.CreateCredential(ctx, &store.Credential{
		ID: "c-good", ProviderID: "p1", Label: "good", APIKeyEnc: enc, Enabled: true,
	}); err != nil {
		t.Fatalf("create credential: %v", err)
	}
	other := make([]byte, 32)
	for i := range other {
		other[i] = byte(i + 7)
	}
	badEnc, err := crypto.Encrypt([]byte("sk-bad"), other)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if err := st.CreateCredential(ctx, &store.Credential{
		ID: "c-bad", ProviderID: "p1", Label: "bad", APIKeyEnc: badEnc, Enabled: true,
	}); err != nil {
		t.Fatalf("create credential: %v", err)
	}

	providers, failures, err := p.PrepareFromStore(ctx, st, key, cfg)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if len(providers["prov"].Credentials) != 1 {
		t.Fatalf("provider must still serve via the good credential, got %d",
			len(providers["prov"].Credentials))
	}
	// 坏凭据要被报出来，但不能带「零可用凭据」那种致命标记。
	var sawBad, sawFatal bool
	for _, f := range failures {
		if f.Slug != "prov" {
			continue
		}
		if f.Stage == "decrypt" {
			sawBad = true
		}
		if f.Stage == "no_credentials" || f.Stage == "list_credentials" {
			sawFatal = true
		}
	}
	if !sawBad {
		t.Errorf("the broken credential must be reported, got %+v", failures)
	}
	if sawFatal {
		t.Errorf("provider with a working credential must not be marked fatal: %+v", failures)
	}
}
