package upstream

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/cn-maul/rosetta-gateway/internal/config"
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
