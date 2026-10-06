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

func TestModelAllow_Intersect(t *testing.T) {
	all := AllowAll()
	onlyA := AllowOnly([]string{"a", "b"})
	onlyB := AllowOnly([]string{"b", "c"})

	cases := []struct {
		name string
		a, b ModelAllow
		// want 是逐模型期望的可见性
		want map[string]bool
	}{
		{"all∩all", all, all, map[string]bool{"a": true, "b": true, "z": true}},
		{"all∩A", all, onlyA, map[string]bool{"a": true, "b": true, "c": false}},
		{"A∩all", onlyA, all, map[string]bool{"a": true, "b": true, "c": false}},
		{"A∩B", onlyA, onlyB, map[string]bool{"a": false, "b": true, "c": false}},
	}
	for _, tc := range cases {
		got := tc.a.Intersect(tc.b)
		for model, want := range tc.want {
			if got.Allows(model) != want {
				t.Errorf("%s: Allows(%q)=%v, want %v (result=%+v)",
					tc.name, model, got.Allows(model), want, got)
			}
		}
	}
}

// 交集为空 = 拒绝全部。语义正确：组只允许 A、key 只允许 B（A≠B）时，
// 这把 key 确实什么都调不了 —— 不能因为「交集为空」就放宽成不限制。
func TestModelAllow_IntersectDisjointIsDenyAll(t *testing.T) {
	got := AllowOnly([]string{"a"}).Intersect(AllowOnly([]string{"b"}))
	if got.Unrestricted {
		t.Fatal("disjoint intersection became Unrestricted")
	}
	if got.Allows("a") || got.Allows("b") {
		t.Error("disjoint intersection allowed something")
	}
}
