package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// TestProbes_显式关闭不被改回开启 钉住「enabled 键是否出现」这条判据。
//
// 此前 setDefaults 用的是 `if !c.Probes.Enabled { c.Probes.Enabled = true }`，
// 于是运维写 `{"probes":{"enabled":false}}`（最自然的关法）会被静默改回开启
// —— 之后每 10 分钟对每个上游模型发一次真实推理并向上游付费，
// 而配置文件里明明写着关闭。没有报错、没有日志，只有账单。
func TestProbes_显式关闭不被改回开启(t *testing.T) {
	c, err := Parse([]byte(`{"listen":"127.0.0.1:8666","probes":{"enabled":false}}`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if c.Probes.Enabled {
		t.Fatal(`probes.enabled=false 被静默改回 true —— 会持续对上游发起付费探测`)
	}
	// 其余字段仍应被填上默认值（只尊重 enabled，不放过整块）。
	if c.Probes.IntervalSec != 600 || c.Probes.TimeoutMs != 30000 || c.Probes.Concurrency != 4 {
		t.Fatalf("其余字段应照常取默认，实际 %+v", c.Probes)
	}
}

// TestProbes_整块缺失默认开启：功能默认是开的，不能因为修了上一条就连环关掉。
func TestProbes_整块缺失默认开启(t *testing.T) {
	c, err := Parse([]byte(`{"listen":"127.0.0.1:8666"}`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !c.Probes.Enabled {
		t.Fatal("probes 整块缺失时应默认开启")
	}
}

func TestProbes_部分对象仍默认开启(t *testing.T) {
	c, err := Parse([]byte(`{"listen":"127.0.0.1:8666","probes":{"interval_sec":300}}`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !c.Probes.Enabled {
		t.Fatal("probes 对象缺省 enabled 时必须默认开启")
	}
	if c.Probes.IntervalSec != 300 {
		t.Fatalf("显式 interval_sec 未保留，实际 %d", c.Probes.IntervalSec)
	}
}

// TestAuditLogKeepRows 覆盖新配置项的缺省与关闭语义。
func TestAuditLogKeepRows(t *testing.T) {
	t.Run("缺省 20000", func(t *testing.T) {
		c, err := Parse([]byte(`{"listen":"127.0.0.1:8666"}`))
		if err != nil {
			t.Fatal(err)
		}
		if c.AuditLogKeepRows != 20000 {
			t.Fatalf("缺省应为 20000，实际 %d", c.AuditLogKeepRows)
		}
	})
	t.Run("-1 表示永久留存", func(t *testing.T) {
		c, err := Parse([]byte(`{"listen":"127.0.0.1:8666","audit_log_keep_rows":-1}`))
		if err != nil {
			t.Fatalf("-1 应合法（关闭剪枝）: %v", err)
		}
		if c.AuditLogKeepRows != -1 {
			t.Fatalf("-1 不应被默认值覆盖，实际 %d", c.AuditLogKeepRows)
		}
	})
	t.Run("0 表示永久留存", func(t *testing.T) {
		c, err := Parse([]byte(`{"listen":"127.0.0.1:8666","audit_log_keep_rows":0}`))
		if err != nil {
			t.Fatalf("0 应合法（关闭剪枝）: %v", err)
		}
		if c.AuditLogKeepRows != 0 {
			t.Fatalf("显式 0 不应被默认值覆盖，实际 %d", c.AuditLogKeepRows)
		}
	})
	t.Run("正数原样保留", func(t *testing.T) {
		c, err := Parse([]byte(`{"listen":"127.0.0.1:8666","audit_log_keep_rows":500}`))
		if err != nil {
			t.Fatal(err)
		}
		if c.AuditLogKeepRows != 500 {
			t.Fatalf("实际 %d", c.AuditLogKeepRows)
		}
	})
	t.Run("越界被拒", func(t *testing.T) {
		if _, err := Parse([]byte(`{"audit_log_keep_rows":-2}`)); err == nil {
			t.Fatal("-2 应被 validate 挡下（既不是 -1 也不是正数）")
		}
		if _, err := Parse([]byte(`{"audit_log_keep_rows":10000001}`)); err == nil {
			t.Fatal("超出上界应被挡下")
		}
	})
}

// TestRequireStrongMasterKey_缺省为false：直接改成 fail-closed 会让所有有意
// 使用口令形态主密钥的部署在升级后起不来，所以缺省必须保持只告警。
func TestRequireStrongMasterKey_缺省为false(t *testing.T) {
	c, err := Parse([]byte(`{"listen":"127.0.0.1:8666"}`))
	if err != nil {
		t.Fatal(err)
	}
	if c.RequireStrongMasterKey {
		t.Fatal("缺省应为 false（保持只告警）")
	}
	c2, err := Parse([]byte(`{"listen":"127.0.0.1:8666","require_strong_master_key":true}`))
	if err != nil {
		t.Fatal(err)
	}
	if !c2.RequireStrongMasterKey {
		t.Fatal("显式 true 应被保留")
	}
}

// 示例配置必须能被真实解析器接受 —— 它们是用户抄的第一份配置。
func TestExampleConfigs_可被解析(t *testing.T) {
	for _, p := range []string{"config.example.json", filepath.Join("docker", "config.default.json")} {
		t.Run(p, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join("..", "..", p))
			if err != nil {
				t.Fatalf("读取 %s: %v", p, err)
			}
			if !json.Valid(data) {
				t.Fatalf("%s 不是合法 JSON", p)
			}
			c, err := Parse(data)
			if err != nil {
				t.Fatalf("%s 无法通过 Parse/validate: %v", p, err)
			}
			if c.AuditLogKeepRows <= 0 {
				t.Fatalf("%s 应显式带上正数的 audit_log_keep_rows", p)
			}
		})
	}
}

// 示例配置的整个 defaults 块（config.example.json / docker 默认 / 代码
// setDefaults）必须同源。
// 此前 docker 默认值还是旧的三档，与代码里的 300000/120000/60000 全部不一致
// —— 容器部署的流式请求会远比裸机更容易被超时掐断，而没有任何报错提示。
func TestRuntimeDefaults_三处一致(t *testing.T) {
	want := Default().Defaults
	for _, p := range []string{"config.example.json", filepath.Join("docker", "config.default.json")} {
		t.Run(p, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join("..", "..", p))
			if err != nil {
				t.Fatalf("读取 %s: %v", p, err)
			}
			c, err := Parse(data)
			if err != nil {
				t.Fatalf("解析 %s: %v", p, err)
			}
			if got := c.Defaults; !reflect.DeepEqual(got, want) {
				t.Errorf("defaults 与 setDefaults 不一致：\n  文件: %+v\n  代码: %+v", got, want)
			}
		})
	}
}
