package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cn-maul/rosetta-gateway/internal/config"
	"github.com/cn-maul/rosetta-gateway/internal/ratelimit"
	"github.com/cn-maul/rosetta-gateway/internal/store"
	"github.com/cn-maul/rosetta-gateway/internal/upstream"
)

// routeTestApp 只用于验证**路由层**行为（pattern 匹配与兜底响应形状），
// 不参与鉴权/数据面语义 —— 那些由 handleIngress 侧的用例覆盖。
func routeTestApp(t *testing.T) *gatewayApp {
	t.Helper()
	db, err := store.Open(t.TempDir()+"/routes.db", discardLogger())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return buildApp(routeDeps{
		db: db, cfg: config.Default(), logger: discardLogger(),
		pool: upstream.NewPool(discardLogger()), rateLimiter: ratelimit.New(),
		usage: newUsageRecorder(db, discardLogger()),
	})
}

// 模型详情必须能寻址含 "/" 的模型 id。
//
// 列表端点（catalog.List）在 ?include=upstream 时下发的 id 就是
// "供应商slug/模型id"，而 public_name 本身也允许含 "/"（routing.go 明确
// 支持「acme/gpt-4」这种按客户分模型名）。单段通配 {model} 匹配不到多段
// 路径，请求会落到兜底 404 —— 症状是「模型能列出来，点进去说模型不存在」，
// 恰好是「先列模型、再拉详情拿 context_length」的那类客户端必踩。
func TestV1Route_ModelDetailMatchesSlashContainingIDs(t *testing.T) {
	app := routeTestApp(t)
	cases := []string{
		"/v1/models/gpt-4",                     // 普通 id
		"/v1/models/acme/gpt-4",                // ?include=upstream 形态 / 分客户命名的 public_name
		"/v1/models/team/squad/model-2024-10",  // 多段
		"/openai/v1/models/acme/gpt-4",         // openai 别名
		"/anthropic/v1/models/acme/claude-3-5", // anthropic 别名
	}
	for _, path := range cases {
		t.Run(path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			_, got := app.Mux.Handler(req)
			if !strings.Contains(got, "/models/{model...}") {
				t.Fatalf("%s 落到 %q，说明通配不是 {model...}，含斜杠的 id 无法寻址", path, got)
			}
		})
	}
}

// /v1 下未匹配的路径必须回 JSON，而不是 net/http 默认的 text/plain。
//
// 客户端 SDK 对 text/plain 的 "404 page not found" 调 res.json() 会抛
// SyntaxError，在客户端日志里看起来像网关崩了，而真相只是路径拼错。
func TestV1Route_UnknownPathReturnsJSONNotFound(t *testing.T) {
	app := routeTestApp(t)
	cases := []struct {
		path        string
		anthropic   bool
		description string
	}{
		{"/v1/definitely-not-registered", false, "未知 /v1 子路径"},
		{"/v1/chat/completions/extra", false, "已知端点后面多一段"},
		{"/openai/v1/nope", false, "openai 别名未知路径"},
		{"/anthropic/v1/nope", true, "anthropic 别名未知路径"},
	}
	for _, tc := range cases {
		t.Run(tc.description, func(t *testing.T) {
			rec := httptest.NewRecorder()
			// 方法也用非常规的 DELETE：兜底不按方法注册，必须接住所有方法。
			app.Mux.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, tc.path, nil))

			if rec.Code != http.StatusNotFound {
				t.Fatalf("status=%d body=%q, want 404", rec.Code, rec.Body.String())
			}
			if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
				t.Fatalf("Content-Type=%q，兜底必须是 JSON（text/plain 会让客户端 SDK 解析抛错）", ct)
			}
			// 响应体必须能整体解析成一个 JSON，且带错误信息 —— 只断言
			// Content-Type 会漏掉「写了头但 body 是空/半截」这种写法。
			var body map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("响应体不是合法 JSON: %v body=%q", err, rec.Body.String())
			}
			if tc.anthropic {
				if body["type"] != "error" {
					t.Fatalf("anthropic 别名必须回 {\"type\":\"error\"} 结构，实际 body=%v", body)
				}
			} else if _, ok := body["error"]; !ok {
				t.Fatalf("OpenAI 形状缺少 error 字段: body=%v", body)
			}
			if !strings.Contains(rec.Body.String(), tc.path) {
				t.Errorf("兜底应回显请求路径（便于客户端自查拼写），实际 body=%q", rec.Body.String())
			}
		})
	}
}
