package effort

import (
	"reflect"
	"testing"
)

func TestParse_别名与大小写(t *testing.T) {
	cases := []struct {
		in   string
		want Level
	}{
		{"low", Low},
		{"LOW", Low},
		{"  high  ", High},
		{"xhigh", XHigh},
		{"x-high", XHigh},
		{"minimal", Minimal},
		{"min", Minimal},
		{"default", Medium},
		{"max", XHigh},
		{"none", None},
		// 拼错 / 完全不认识的写法必须落在 Unset —— 「退回默认」与
		// 「夹到最接近的一档」是两种处置，混淆会把用户的显式选择丢掉。
		{"turbo", Unset},
		{"", Unset},
	}
	for _, c := range cases {
		if got := Parse(c.in); got != c.want {
			t.Errorf("Parse(%q) = %q，期望 %q", c.in, got, c.want)
		}
	}
}

func TestParseLevels_排序去重与未知保留(t *testing.T) {
	// 手抄的 CSV 顺序混乱很常见；等价集合必须在库里落成同一个值。
	known, unknown := ParseLevels("high, low ,medium,low")
	want := []Level{Low, Medium, High}
	if !reflect.DeepEqual(known, want) {
		t.Fatalf("已知档位 = %v，期望 %v", known, want)
	}
	if len(unknown) != 0 {
		t.Fatalf("不该有未知档位，实际 %v", unknown)
	}

	// 厂商私有值必须原样带回，不能被悄悄吞掉（吞掉 = 配了不生效且无从查起）。
	known, unknown = ParseLevels("low,ultra_deep, xhigh ")
	wantKnown := []Level{Low, XHigh}
	if !reflect.DeepEqual(known, wantKnown) {
		t.Fatalf("已知档位 = %v，期望 %v", known, wantKnown)
	}
	if !reflect.DeepEqual(unknown, []string{"ultra_deep"}) {
		t.Fatalf("未知档位 = %v，期望 [ultra_deep]", unknown)
	}

	// 空串 = 未配置，与「配了个无效值」必须可区分（后者有 unknown 非空）。
	if k, u := ParseLevels(""); len(k) != 0 || len(u) != 0 {
		t.Fatalf("空串应得到空结果，实际 %v / %v", k, u)
	}
	if k, u := ParseLevels("turbo"); len(k) != 0 || !reflect.DeepEqual(u, []string{"turbo"}) {
		t.Fatalf("无效值应只进 unknown，实际 %v / %v", k, u)
	}
}

func TestFormatLevels_往返(t *testing.T) {
	csv := "low,high,ultra_deep"
	known, unknown := ParseLevels(csv)
	if got := FormatLevels(known, unknown); got != "low,high,ultra_deep" {
		t.Fatalf("往返得到 %q，期望 %q", got, csv)
	}
	if got := FormatLevels(nil, nil); got != "" {
		t.Fatalf("空集合应拼成空串，实际 %q", got)
	}
}

func TestClamp_就近夹取(t *testing.T) {
	cases := []struct {
		name      string
		wanted    Level
		supported []Level
		want      Level
	}{
		{"支持就原样返回", High, []Level{Low, Medium, High}, High},
		{"超出上界夹到最高", XHigh, []Level{Low, Medium, High}, High},
		{"低于下界夹到最低", Minimal, []Level{Low, Medium, High}, Low},
		{"中间值夹到最近", XHigh, []Level{Low, High}, High},
		// 只支持 xhigh 的模型：客户端要 high 时必须给 high 的语义（top），
		// 而不是「就近」把它降到 low —— 那是反的。
		{"只支持最高档时给最高", High, []Level{XHigh}, XHigh},
		// 未配置 = 网关不干预：原样返回，交回既有的三档归一（零回归）。
		{"未配置时不干预", XHigh, nil, XHigh},
		// 不认识的挡位不猜：宁可让上游自己决定，也不要把拼错的意图
		// 映射成一个「看起来生效了」的值。
		{"未知档位不猜", Level("turbo"), []Level{Low, High}, Level("turbo")},
		{"未指定不改", Unset, []Level{Low, High}, Unset},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Clamp(c.wanted, c.supported); got != c.want {
				t.Fatalf("Clamp(%q, %v) = %q，期望 %q", c.wanted, c.supported, got, c.want)
			}
		})
	}
}

func TestClamp_同距取较弱(t *testing.T) {
	// low(1) 与 high(3) 之间，medium(2) 到两边距离相同 → 取较弱的一侧。
	// 「宁可少想一点」比「超出一个未列出的档位」安全。
	if got := Clamp(Medium, []Level{Low, High}); got != Low {
		t.Fatalf("同距时应取较弱档，实际 %q", got)
	}
}

func TestToRosettaLevel_折叠到三档(t *testing.T) {
	// rosetta 只认三档（其 validate 会拒其它值），这是唯一的接缝。
	cases := map[Level]string{
		Minimal: "low",
		Low:     "low",
		Medium:  "medium",
		High:    "high",
		XHigh:   "high",
		// 不认识的档位折叠到 medium 而不是 high：它在 rosetta 侧本来就
		// 会被 parseEffort 归到默认，而默认是 medium。
		Level("turbo"): "medium",
	}
	for in, want := range cases {
		if got := ToRosettaLevel(in); got != want {
			t.Errorf("ToRosettaLevel(%q) = %q，期望 %q", in, got, want)
		}
	}
}

func TestSupports(t *testing.T) {
	supported := []Level{Low, Medium, High}
	if !Supports(Medium, supported) {
		t.Fatal("Medium 应被视为支持")
	}
	if Supports(XHigh, supported) {
		t.Fatal("XHigh 不在支持集里")
	}
	// 未指定不算「不支持」：那是「按上游默认」，不是「这个值无效」。
	if !Supports(Unset, supported) {
		t.Fatal("Unset 应视为支持（不改）")
	}
	// 未配置支持集时一切都放行 —— 与 Clamp 的零回归口径一致。
	if !Supports(XHigh, nil) {
		t.Fatal("支持集为空时应全部放行")
	}
}

// TestKnownLevels_有序 是给 UI 与文档看的契约：前端勾选器按这个顺序排
// 按钮，落库排序也依赖它。反转顺序会让「强度递增」的语义整个翻掉，且
// 没有任何报错。
func TestKnownLevels_有序(t *testing.T) {
	want := []Level{Minimal, Low, Medium, High, XHigh}
	if !reflect.DeepEqual(KnownLevels, want) {
		t.Fatalf("KnownLevels = %v，期望 %v", KnownLevels, want)
	}
}
