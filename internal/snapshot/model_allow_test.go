package snapshot

import "testing"

// 零值必须是「拒绝全部」。
//
// 这条不是风格偏好，是权限边界的最后一道防线：快照重建时若漏填
// AllowedModels，零值会直接用在热路径上。零值若是「不限制」，
// 漏填就变成静默放权 —— 受限用户拿到全部模型且无人发现。
func TestModelAllow_ZeroValueDeniesEverything(t *testing.T) {
	var zero ModelAllow
	for _, m := range []string{"gpt-4o", "claude-sonnet", ""} {
		if zero.Allows(m) {
			t.Errorf("zero ModelAllow allowed %q; it must deny (fail closed)", m)
		}
	}
}

func TestModelAllow_Unrestricted(t *testing.T) {
	a := AllowAll()
	if !a.Unrestricted {
		t.Fatal("AllowAll().Unrestricted = false")
	}
	for _, m := range []string{"gpt-4o", "任意中文名", ""} {
		if !a.Allows(m) {
			t.Errorf("AllowAll denied %q", m)
		}
	}
}

func TestModelAllow_Only(t *testing.T) {
	a := AllowOnly([]string{"gpt-4o", "  claude-sonnet  ", "", "gpt-4o"})
	if a.Unrestricted {
		t.Fatal("AllowOnly must not be Unrestricted")
	}
	if !a.Allows("gpt-4o") || !a.Allows("claude-sonnet") {
		t.Error("listed models must be allowed (and trimmed)")
	}
	if a.Allows("gpt-4o-mini") || a.Allows("") {
		t.Error("unlisted/empty model must be denied")
	}
	// 去空白后的空串被丢弃，不会变成「允许空模型名」。
	if len(a.Models) != 2 {
		t.Errorf("models = %v, want 2 after trim/dedupe", a.Models)
	}
}

// AllowOnly 传空列表得到的是「拒绝全部」，不是「不限制」。
// 「不限制」必须由调用方显式说 AllowAll()。
func TestModelAllow_EmptyOnlyIsDenyAll(t *testing.T) {
	a := AllowOnly(nil)
	if a.Unrestricted {
		t.Fatal("AllowOnly(nil) must not be Unrestricted — 空只能表示「什么都不允许」")
	}
	if a.Allows("gpt-4o") {
		t.Error("AllowOnly(nil) allowed a model")
	}
}
