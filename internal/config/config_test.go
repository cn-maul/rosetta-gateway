package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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

func TestBootstrapCredential_ResolveAPIKeyPrecedence(t *testing.T) {
	env := func(name string) string {
		if name == "SET_KEY" {
			return "from-env"
		}
		return ""
	}
	for _, tc := range []struct {
		name string
		cred BootstrapCredential
		want string
	}{
		{"explicit key wins", BootstrapCredential{APIKey: "from-config", APIKeyEnv: "SET_KEY"}, "from-config"},
		{"env used when key empty", BootstrapCredential{APIKeyEnv: "SET_KEY"}, "from-env"},
		{"unset env does not erase key", BootstrapCredential{APIKey: "from-config", APIKeyEnv: "UNSET_KEY"}, "from-config"},
		{"no source", BootstrapCredential{APIKeyEnv: "UNSET_KEY"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cred.ResolveAPIKey(env); got != tc.want {
				t.Fatalf("ResolveAPIKey() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestR2_DeploymentDocsMatchEffectiveContracts(t *testing.T) {
	read := func(name string) string {
		t.Helper()
		data, err := os.ReadFile(filepath.Join("..", "..", name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		return string(data)
	}

	docker := read("DOCKER.md")
	if !strings.Contains(docker, "非空的 `config.json.timezone` 最高") {
		t.Fatal("DOCKER.md must document config timezone as the final override")
	}
	if !strings.Contains(docker, "`usage_retention_days` 到期后") {
		t.Fatal("DOCKER.md must describe retention as configurable, not fixed at 30 days")
	}

	readme := read("README.md")
	if !strings.Contains(readme, "默认保留 30 天") ||
		!strings.Contains(readme, "`usage_retention_days` 可配置") {
		t.Fatal("README.md must describe 30 days as a configurable default")
	}

	design := read("DESIGN.md")
	if !strings.Contains(design, "写 `0` 或 `-1` 都显式关闭剪枝") {
		t.Fatal("DESIGN.md must document 0 and -1 as disabling audit pruning")
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

// max_request_body_bytes 必须有上界（2026-10-12 修 P2-18）。
//
// 它此前是整张校验表里**唯一** max=0（无上界）的字段。而 inwire 的三条入口
// （chat / response / anthropic）都是 io.ReadAll 全量读进内存，
// MaxBytesReader 是唯一闸门 —— 误配成天文数字（手滑多敲几个 0、把 GB 当 MB）
// 后，局域网内任意客户端发一个超大 body 就能让单进程网关 OOM。
//
// 本测试钉住那道上界：边界内通过、越界被拒。
func TestMaxRequestBodyBytes_有上界(t *testing.T) {
	const maxBody = 512 * 1024 * 1024
	for _, tc := range []struct {
		name    string
		bytes   int
		wantErr bool
	}{
		{"默认值(32MiB)", 32 * 1024 * 1024, false},
		{"恰好等于上界", maxBody, false},
		{"零(走默认)", 0, false},
		{"负数", -1, true},
		{"超出上界一个字节", maxBody + 1, true},
		{"误配成 1GiB", 1024 * 1024 * 1024, true},
		{"手滑多敲几个 0(1TiB)", 1 << 40, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(`{"listen":"127.0.0.1:8666","defaults":{"max_request_body_bytes":` + itoa(tc.bytes) + `}}`)
			_, err := Parse(body)
			if tc.wantErr && err == nil {
				t.Fatalf("bytes=%d 应被挡下：无上界时误配会让网关被超大 body 打成 OOM", tc.bytes)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("bytes=%d 应合法：%v", tc.bytes, err)
			}
		})
	}
}
