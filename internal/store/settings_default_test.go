package store

import (
	"context"
	"testing"
)

// 模型容量的兜底默认值（探测不到上游 context_length / max_completion_tokens 时生效）
// 被刻意定在当代主流模型的真实规格：128K 上下文 / 64K 最大输出。
//
// 这组数字是**产品决定**而不是随手取的保守值，所以用测试钉死：
// 早期版本用的是 8192/4096，会让一个刚接入、未暴露容量的新模型被凭空削成
// 8K ——用户侧只看到「这模型怎么只能写 8K 字」，自查不出来是网关默认值兜的。
func TestModelDefaults_FallbackIs128K64K(t *testing.T) {
	if defaultContextWindowFallback != 131_072 {
		t.Errorf("defaultContextWindowFallback = %d, want 131072 (128K)",
			defaultContextWindowFallback)
	}
	if defaultMaxOutputTokensFallback != 65_536 {
		t.Errorf("defaultMaxOutputTokensFallback = %d, want 65536 (64K)",
			defaultMaxOutputTokensFallback)
	}

	ctx := context.Background()
	st := testStore(t, t.TempDir()+"/gw.db")

	// 1) 未配置 → 读回兜底默认值。
	got, err := st.GetModelDefaults(ctx)
	if err != nil {
		t.Fatalf("get unset: %v", err)
	}
	if got.ContextWindow != 131_072 || got.MaxOutputTokens != 65_536 {
		t.Fatalf("unset should fall back to 128K/64K, got %+v", got)
	}

	// 2) 显式配 0 / 负数（模型未设置 → NULL）也必须落回兜底，而不是 0。
	if err := st.SetModelDefaults(ctx, ModelDefaults{}); err != nil {
		t.Fatalf("set zero: %v", err)
	}
	z, err := st.GetModelDefaults(ctx)
	if err != nil {
		t.Fatalf("get zero: %v", err)
	}
	if z.ContextWindow != 131_072 || z.MaxOutputTokens != 65_536 {
		t.Fatalf("zero values must fall back, got %+v", z)
	}

	// 3) 显式配置覆盖兜底，且不被回落逻辑改写。
	want := ModelDefaults{ContextWindow: 32000, MaxOutputTokens: 8000}
	if err := st.SetModelDefaults(ctx, want); err != nil {
		t.Fatalf("set explicit: %v", err)
	}
	back, err := st.GetModelDefaults(ctx)
	if err != nil {
		t.Fatalf("get explicit: %v", err)
	}
	if back != want {
		t.Fatalf("explicit config must win over fallback:\n got %+v\nwant %+v", back, want)
	}
}
