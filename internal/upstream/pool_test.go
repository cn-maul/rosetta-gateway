package upstream

import (
	"io"
	"log/slog"
	"testing"

	"github.com/cn-maul/rosetta-gateway/internal/config"
)

func newTestPool() *Pool {
	return NewPool(slog.New(slog.NewTextHandler(io.Discard, nil)))
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
	if err := p.BuildFromConfig(config.Default()); err != nil {
		t.Fatalf("build from config: %v", err)
	}
	if !p.TargetAvailable("ghost#t0") {
		t.Fatalf("target health not reset on rebuild: still unavailable")
	}
	if len(p.targets) != 0 {
		t.Fatalf("targets map not cleared on rebuild, len=%d", len(p.targets))
	}
}
