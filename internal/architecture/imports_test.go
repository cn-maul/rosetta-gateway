// Package architecture 用机器检查守住包依赖方向。
//
// 这些规则不是风格偏好：admin 反向依赖 server 会让管理面无法独立测试，
// store 反向依赖上层会让迁移与事务代码被 HTTP/运行时细节污染。把它们写成
// 测试而不是文档，新增依赖边时 CI 会直接变红。
package architecture

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const modulePath = "github.com/cn-maul/rosetta-gateway"

type packageImports struct {
	pkg     string
	imports []string
}

func TestDependencyDirection(t *testing.T) {
	root := findModuleRoot(t)
	cmd := exec.Command("go", "list", "-f", `{{.ImportPath}}|{{join .Imports ","}}`, "./...")
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go list: %v\n%s", err, out)
	}

	packages := parsePackages(t, string(out))
	violations := checkRules(packages)
	if len(violations) > 0 {
		t.Fatalf("包依赖方向违规：\n  %s", strings.Join(violations, "\n  "))
	}
}

func TestCheckRulesDetectsForbiddenEdge(t *testing.T) {
	packages := map[string]packageImports{
		modulePath + "/internal/admin": {
			pkg:     modulePath + "/internal/admin",
			imports: []string{modulePath + "/internal/server", modulePath + "/internal/store"},
		},
	}
	violations := checkRules(packages)
	if len(violations) != 1 {
		t.Fatalf("应恰好捕获 1 条违规，得到 %d 条：%v", len(violations), violations)
	}
	if !strings.Contains(violations[0], "internal/admin ->") {
		t.Fatalf("违规信息缺少依赖边：%q", violations[0])
	}
}

func findModuleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("找不到 go.mod：测试必须在仓库内运行")
		}
		dir = parent
	}
}

func parsePackages(t *testing.T, raw string) map[string]packageImports {
	t.Helper()
	out := make(map[string]packageImports)
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		pkg, imports, ok := strings.Cut(line, "|")
		if !ok {
			t.Fatalf("无法解析 go list 输出：%q", line)
		}
		var list []string
		if imports != "" {
			list = strings.Split(imports, ",")
		}
		out[pkg] = packageImports{pkg: pkg, imports: list}
	}
	return out
}

func checkRules(packages map[string]packageImports) []string {
	rules := []struct {
		from    string
		forbid  []string
		message string
	}{
		{
			from:    modulePath + "/internal/admin",
			forbid:  []string{modulePath + "/internal/server"},
			message: "管理面不得反向依赖 HTTP 装配层；共享能力请放 httpx/identity/guard/access",
		},
		{
			from: modulePath + "/internal/store",
			forbid: []string{
				modulePath + "/internal/admin",
				modulePath + "/internal/server",
				modulePath + "/internal/dataplane",
				modulePath + "/internal/gatewayapp",
				modulePath + "/cmd/gateway",
			},
			message: "store 是底层，不得依赖任何上层",
		},
		{
			from: modulePath + "/internal/routing",
			forbid: []string{
				modulePath + "/internal/admin",
				modulePath + "/internal/server",
				modulePath + "/cmd/gateway",
			},
			message: "routing 只做解析，不得依赖管理面/HTTP/进程入口",
		},
		{
			from: modulePath + "/internal/snapshot",
			forbid: []string{
				modulePath + "/internal/admin",
				modulePath + "/internal/server",
				modulePath + "/cmd/gateway",
			},
			message: "snapshot 只做运行时视图，不得依赖管理面/HTTP/进程入口",
		},
		{
			from: modulePath + "/internal/usage",
			forbid: []string{
				modulePath + "/internal/admin",
				modulePath + "/internal/server",
				modulePath + "/internal/snapshot",
				modulePath + "/cmd/gateway",
			},
			message: "usage 只负责落库/扣费，不得依赖管理面、HTTP 装配、全局快照或进程入口",
		},
		{
			from: modulePath + "/internal/billing",
			forbid: []string{
				modulePath + "/internal/admin",
				modulePath + "/internal/server",
				modulePath + "/internal/snapshot",
				modulePath + "/internal/usage",
				modulePath + "/cmd/gateway",
			},
			message: "billing 是纯计价/预检策略，不得依赖 HTTP、全局快照、usage 状态机或进程入口",
		},
		{
			from: modulePath + "/internal/ingress",
			forbid: []string{
				modulePath + "/internal/admin",
				modulePath + "/internal/server",
				modulePath + "/internal/snapshot",
				modulePath + "/internal/usage",
				modulePath + "/internal/billing",
				modulePath + "/cmd/gateway",
			},
			message: "ingress 只做下游协议编解码，不得依赖管理面、运行时状态、计费状态机或进程入口",
		},
		{
			from: modulePath + "/internal/dataplane",
			forbid: []string{
				modulePath + "/internal/admin",
				modulePath + "/internal/server",
				modulePath + "/cmd/gateway",
			},
			message: "dataplane 只做请求转发策略，不得依赖管理面、HTTP 服务器装配或进程入口",
		},
	}

	var violations []string
	for _, rule := range rules {
		pkg, ok := packages[rule.from]
		if !ok {
			continue
		}
		for _, imp := range pkg.imports {
			for _, forbidden := range rule.forbid {
				if imp == forbidden {
					violations = append(violations,
						rule.from+" -> "+imp+"："+rule.message)
				}
			}
		}
	}
	return violations
}
