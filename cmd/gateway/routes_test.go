package main

import (
	"net/http"
	"sort"
	"strings"
	"testing"

	"github.com/cn-maul/rosetta-gateway/internal/config"
	"github.com/cn-maul/rosetta-gateway/internal/ratelimit"
	"github.com/cn-maul/rosetta-gateway/internal/store"
	"github.com/cn-maul/rosetta-gateway/internal/upstream"
)

func TestBuildApp_RegistersEveryCataloguedRoute(t *testing.T) {
	db, err := store.Open(t.TempDir()+"/routes.db", discardLogger())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	cfg := config.Default()
	app := buildApp(routeDeps{
		db: db, cfg: cfg, logger: discardLogger(),
		pool: upstream.NewPool(discardLogger()), rateLimiter: ratelimit.New(),
		usage: newUsageRecorder(db, discardLogger()),
	})

	// 99 是当前真实生产契约（含 5 条 /admin/api/ 方法兜底与 3 条 /v1
	// 子树兜底）。端点增删必须显式更新这个数，不能靠测试里手抄第三份清单。
	if len(app.Routes) != 99 {
		t.Fatalf("生产路由契约应为 99 条，实际 %d 条", len(app.Routes))
	}
	seen := make(map[string]registeredRoute)
	for _, r := range app.Routes {
		if r.Pattern == "" {
			t.Fatal("采集到空 pattern")
		}
		if _, ok := seen[r.Pattern]; ok {
			t.Fatalf("生产路由重复注册: %s", r.Pattern)
		}
		seen[r.Pattern] = r
	}

	required := []string{
		"POST /v1/chat/completions", "POST /v1/messages", "POST /v1/responses",
		"GET /v1/models", "GET /healthz", "GET /admin/",
		// 模型详情必须是 {model...}（通配剩余整段）：列表端点会下发
		// "供应商slug/模型id" 这种含 "/" 的 id，单段通配匹配不到。
		"GET /v1/models/{model...}",
		"GET /openai/v1/models/{model...}", "GET /anthropic/v1/models/{model...}",
		// /v1 子树兜底：未匹配路径回 JSON 404，而不是 text/plain。
		"/v1/", "/openai/v1/", "/anthropic/v1/",
		"POST /admin/api/login", "GET /admin/api/session",
		"GET /admin/api/providers", "POST /admin/api/providers",
		"GET /admin/api/usage/by-user", "POST /admin/api/usage/prune",
		"GET /admin/api/usage/history.csv",
		"PUT /admin/api/routes/{id}/targets",
		"POST /admin/api/config-export/export",
		// 站点品牌：GET 免鉴权（登录页用），PUT 管理员。
		"GET /admin/api/site", "PUT /admin/api/site",
		// 协议探测：创建上游之前先问「这个中转支持哪几种协议」。
		"POST /admin/api/providers/probe-protocols",
	}
	for _, pattern := range required {
		if _, ok := seen[pattern]; !ok {
			t.Errorf("生产路由缺失: %s", pattern)
		}
	}

	// 几条容易写错、且安全敏感的路径必须落在正确 mux 上。
	public := map[string]bool{
		"POST /admin/api/login":                true,
		"GET /admin/api/session":               true,
		"GET /admin/api/bootstrap":             true,
		"POST /admin/api/bootstrap":            true,
		"POST /admin/api/password-setup/check": true,
		"POST /admin/api/password-setup":       true,
		// 站点品牌 GET 必须免鉴权：登录页在拿到会话之前就要渲染站名/LOGO，
		// 要求带凭据才能取品牌是死循环。它只回名称与 LOGO。
		"GET /admin/api/site": true,
	}
	for pattern, wantPublic := range public {
		if got := seen[pattern].Public; got != wantPublic {
			t.Errorf("%s 的 Public = %v，期望 %v（注册到了错误的 mux）", pattern, got, wantPublic)
		}
	}
	for pattern := range seen {
		if strings.HasPrefix(pattern, "/admin/api/") && !strings.HasSuffix(pattern, "/") {
			if _, explicitlyPublic := public[pattern]; explicitlyPublic {
				continue
			}
			if seen[pattern].Public {
				t.Errorf("管理 API 泄露到免鉴权 mux: %s", pattern)
			}
		}
	}
}

func TestBuildApp_HandlersMatchServeMuxPatterns(t *testing.T) {
	db, err := store.Open(t.TempDir()+"/routes-pattern.db", discardLogger())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	cfg := config.Default()
	app := buildApp(routeDeps{
		db: db, cfg: cfg, logger: discardLogger(),
		pool: upstream.NewPool(discardLogger()), rateLimiter: ratelimit.New(),
		usage: newUsageRecorder(db, discardLogger()),
	})

	sorted := append([]registeredRoute(nil), app.Routes...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Pattern < sorted[j].Pattern })
	for _, r := range sorted {
		method, path, ok := strings.Cut(r.Pattern, " ")
		if !ok {
			// 无方法的子树兜底（/v1/、/openai/v1/、/anthropic/v1/）：
			// 它必须接住**所有**方法，所以不按方法逐个注册。
			// 用一个肯定没人注册过的子路径验证它确实落到这条兜底上。
			if !strings.HasSuffix(r.Pattern, "/") {
				t.Fatalf("pattern 缺少方法: %q", r.Pattern)
			}
			req, err := http.NewRequest(http.MethodDelete,
				r.Pattern+"definitely-not-a-registered-endpoint", nil)
			if err != nil {
				t.Fatalf("构造请求 %s: %v", r.Pattern, err)
			}
			if _, got := app.Mux.Handler(req); got != r.Pattern {
				t.Errorf("%s 没能接住未注册子路径，实际落到 %q", r.Pattern, got)
			}
			continue
		}
		// 前缀兜底不是具体端点，mux.Handler 对其只返回自身 pattern；
		// 它的作用由安全矩阵中的真实请求验证。
		if strings.HasSuffix(path, "/") && path != "/admin/" {
			continue
		}
		reqPath := concretePath(path)
		req, err := http.NewRequest(method, reqPath, nil)
		if err != nil {
			t.Fatalf("构造请求 %s: %v", r.Pattern, err)
		}
		mux := app.AdminMux
		if r.Public {
			mux = app.PublicAdminMux
		} else if !strings.HasPrefix(path, "/admin/api/") {
			mux = app.Mux
		}
		_, got := mux.Handler(req)
		if got != r.Pattern {
			t.Errorf("%s 实际注册为 %q", r.Pattern, got)
		}
	}
}

func concretePath(pattern string) string {
	var b strings.Builder
	for {
		i := strings.IndexByte(pattern, '{')
		if i < 0 {
			b.WriteString(pattern)
			return b.String()
		}
		b.WriteString(pattern[:i])
		j := strings.IndexByte(pattern[i:], '}')
		if j < 0 {
			b.WriteString(pattern[i:])
			return b.String()
		}
		b.WriteString("x")
		pattern = pattern[i+j+1:]
	}
}
