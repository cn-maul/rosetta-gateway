package config

import "testing"

// usage_retention_days 的三项契约：缺省值、范围校验、与「配额预检成本」的关系。
//
// 第三项本身无法在此断言（那属于 store 层与真实数据量），但**取值必须被校验**
// ——写错一个 0 或负数会让剪枝静默停摆，而那正是本配置想控制的行为。

func TestUsageRetentionDays_缺省为30(t *testing.T) {
	c, err := Parse([]byte(`{"listen":"127.0.0.1:8666"}`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if c.UsageRetentionDays != 30 {
		t.Fatalf("缺省应为 30（与 store.DefaultRetentionDays 同源），实际 %d", c.UsageRetentionDays)
	}
}

func TestUsageRetentionDays_显式值原样保留(t *testing.T) {
	// 保留 1 天对低用量部署是合法诉求，不能被下界挡掉。
	for _, days := range []int{1, 7, 30, 365, 3650} {
		body := []byte(`{"listen":"127.0.0.1:8666","usage_retention_days":` + itoa(days) + `}`)
		c, err := Parse(body)
		if err != nil {
			t.Fatalf("days=%d 应合法: %v", days, err)
		}
		if c.UsageRetentionDays != days {
			t.Fatalf("days=%d 未原样保留，实际 %d", days, c.UsageRetentionDays)
		}
	}
}

func TestUsageRetentionDays_越界被拒(t *testing.T) {
	cases := []struct {
		name string
		json string
	}{
		{"负数", `{"usage_retention_days":-1}`},
		{"超出上限", `{"usage_retention_days":3651}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := Parse([]byte(c.json)); err == nil {
				t.Fatalf("%s 应被 validate 挡下：剪枝会静默停摆或行为反直觉", c.json)
			}
		})
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		return "-" + string(b)
	}
	return string(b)
}
