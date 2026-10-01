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

// 目标熔断态必须在重建池时清零：既堵住「每次保存链重生成 targetID → 本表单调堆积」
// 的泄漏，也让目标级与凭据级健康态语义一致（重建即重新探测）。
func TestPoolBuildResetsTargetHealth(t *testing.T) {
	p := newTestPool()

	// threshold=1：一次失败即熔断，TargetAvailable 转 false。
	p.RecordTargetFailure("ghost#t0", 1)
	if p.TargetAvailable("ghost#t0") {
		t.Fatalf("expected target circuit-open after threshold reached")
	}

	// 重建后该目标态应被整表丢弃 —— 旧 targetID 不再驻留，也不会误伤同名新目标。
	if err := p.BuildFromConfig(newTestConfig()); err != nil {
		t.Fatalf("build from config: %v", err)
	}
	if !p.TargetAvailable("ghost#t0") {
		t.Fatalf("target health not reset on rebuild: still unavailable")
	}
	if len(p.targets) != 0 {
		t.Fatalf("targets map not cleared on rebuild, len=%d", len(p.targets))
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
