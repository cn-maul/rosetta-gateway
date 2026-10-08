package server

// AdminGateGuard 的白名单判定测试。
//
// # 为什么这里必须有测试
//
// userAccessiblePrefixes 是一个**白名单**：不在其中的前缀一律要求管理员。
// 这套机制的安全性完全依赖于「该放行的路径确实在列表里」，而列表是纯手写的
// 字符串 —— 加一个端点时忘记加进白名单，用户就会看到 403（可用性 bug），
// 或者反过来、把 admin 端点误加进去（越权）。两者都是**静默**的：
// 没有编译错误，没有测试失败，只有一个用户看到错误的页面。
//
// 本文件钉住当前白名单的实际可达性，防止后续改动悄悄破坏它。

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cn-maul/rosetta-gateway/internal/store"
)

// gate 对一条路径跑一次 AdminGateGuard，返回状态码与是否放行到 next。
//
// 身份注入用包私有的 userKey（本文件与 user_auth.go 同包）—— 与真实中间件
// UserAuthMiddleware 注入的是同一个 key，所以这里测的就是真实判定路径，
// 不是另建一条旁路。
//
// # 为什么必须传一个真的 mux（2026-10-10 改）
//
// AdminGateGuard 现在靠 `routes.Handler(r)` 拿到**注册模式**再去比对白名单
// （表里的键形如 `PATCH /admin/api/keys/{id}`，**不是**请求的原始路径）。
// 传 nil 会让判定退化成「一律要求管理员」（fail-closed），那些「应放行」
// 的用例会全部失败 —— 测不出真实行为。
//
// 所以这里按**生产 main.go 的 pattern**注册：调用方给的 path 用于构造请求，
// pattern 用于注册。两者对含 `{id}` 的路由**必须不同** —— 这正是本函数
// 要显式区分的原因（第一版把 k1 当 pattern 注册，于是 pattern 是
// `PATCH /admin/api/keys/k1`，与表里的 `{id}` 对不上而被 403；那不是缺陷，
// 是测试写错了 —— 真实 mux 在 main.go 里注册的就是 `{id}`）。
func gate(t *testing.T, u *store.User, method, path, pattern string) (int, bool) {
	t.Helper()
	reached := false
	mux := http.NewServeMux()
	mux.HandleFunc(method+" "+pattern, func(http.ResponseWriter, *http.Request) {})

	req := httptest.NewRequest(method, path, nil)
	if u != nil {
		req = req.WithContext(context.WithValue(req.Context(), userKey, u))
	}
	rec := httptest.NewRecorder()
	AdminGateGuard(mux, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	})).ServeHTTP(rec, req)
	return rec.Code, reached
}

// gateAt 是 gate 的便捷形式：请求路径与注册模式相同（无路径参数时）。
func gateAt(t *testing.T, u *store.User, method, path string) (int, bool) {
	t.Helper()
	return gate(t, u, method, path, path)
}

// 普通用户**应该**能访问的端点（方法 + 路径）。
//
// 新增任何「普通用户也要用」的端点时，都要在 userAccessibleRoutes 里加一条 ——
// 否则用户打开那个页面只会看到 403，而原因藏在服务端。
func TestAdminGate_UserAccessiblePrefixesAreReachableByNormalUser(t *testing.T) {
	normal := &store.User{ID: "u1", Username: "u1", Role: store.RoleUser, Status: store.UserStatusActive}

	for _, c := range []struct{ method, path, pattern string }{
		{http.MethodGet, "/admin/api/keys", "/admin/api/keys"},
		{http.MethodPost, "/admin/api/keys", "/admin/api/keys"},
		// 含路径参数的：请求用具体 id，注册模式用生产里的 {id}。
		{http.MethodPatch, "/admin/api/keys/k1", "/admin/api/keys/{id}"},
		{http.MethodDelete, "/admin/api/keys/k1", "/admin/api/keys/{id}"},
		{http.MethodPost, "/admin/api/keys/k1/recompute-usage", "/admin/api/keys/{id}/recompute-usage"},
		{http.MethodGet, "/admin/api/usage", "/admin/api/usage"},
		{http.MethodGet, "/admin/api/usage/history", "/admin/api/usage/history"},
		{http.MethodGet, "/admin/api/usage/history.csv", "/admin/api/usage/history.csv"},
		{http.MethodGet, "/admin/api/usage/by-key", "/admin/api/usage/by-key"},
		{http.MethodGet, "/admin/api/usage/by-model", "/admin/api/usage/by-model"},
		{http.MethodGet, "/admin/api/usage/by-provider", "/admin/api/usage/by-provider"},
		{http.MethodGet, "/admin/api/usage/by-day", "/admin/api/usage/by-day"},
		{http.MethodGet, "/admin/api/stats", "/admin/api/stats"},
		{http.MethodGet, "/admin/api/me", "/admin/api/me"},
		{http.MethodPost, "/admin/api/me/password", "/admin/api/me/password"},
		// 充值流水：钱包页要能看到「我什么时候被充过钱」。
		{http.MethodGet, "/admin/api/topups", "/admin/api/topups"},
		{http.MethodPost, "/admin/api/logout", "/admin/api/logout"},
		{http.MethodGet, "/admin/api/model-names", "/admin/api/model-names"},
	} {
		t.Run(c.method+" "+c.path, func(t *testing.T) {
			code, reached := gate(t, normal, c.method, c.path, c.pattern)
			if !reached {
				t.Errorf("普通用户访问 %s %s 被拦（code=%d）：该端点对普通用户不可用", c.method, c.path, code)
			}
		})
	}
}

// 普通用户**不应该**能访问的端点 —— 越权防线。
func TestAdminGate_AdminOnlyPrefixesRejectNormalUser(t *testing.T) {
	normal := &store.User{ID: "u1", Username: "u1", Role: store.RoleUser, Status: store.UserStatusActive}

	for _, c := range []struct{ method, path string }{
		{http.MethodGet, "/admin/api/users"},
		{http.MethodPut, "/admin/api/users/someone/balance"},
		{http.MethodPost, "/admin/api/users/someone/password"},
		{http.MethodGet, "/admin/api/groups"},
		{http.MethodGet, "/admin/api/routes"},
		{http.MethodGet, "/admin/api/providers"},
		{http.MethodGet, "/admin/api/settings"},
		{http.MethodGet, "/admin/api/audit"},
		// 本次加固的直接动机：这两个在 /admin/api/usage 前缀下，
		// 旧的前缀白名单会**整体放行**它们。
		{http.MethodGet, "/admin/api/usage/by-user"},
		{http.MethodPost, "/admin/api/usage/prune"},
	} {
		t.Run(c.method+" "+c.path, func(t *testing.T) {
			code, reached := gateAt(t, normal, c.method, c.path)
			if reached {
				t.Errorf("普通用户竟然能进入 %s %s —— 越权", c.method, c.path)
			}
			if code != http.StatusForbidden {
				t.Errorf("code = %d, want 403", code)
			}
		})
	}
}

// 本次加固的核心断言：**未注册/新增的端点默认 fail-closed**。
//
// 这条用一条「存在但不在白名单里」的路径证明机制：它不是靠人工维护
// 「哪些该拦」，而是靠「不在表里就拦」。旧的前缀实现会在这里放行。
func TestAdminGate_UnlistedRouteIsDeniedByDefault(t *testing.T) {
	normal := &store.User{ID: "u1", Username: "u1", Role: store.RoleUser, Status: store.UserStatusActive}

	// 模拟「将来有人在 /admin/api/usage 下加了个新端点，但忘了加白名单」。
	// 前缀实现会放行它（静默越权），精确实现必须拦。
	mux := http.NewServeMux()
	mux.HandleFunc("GET /admin/api/usage/future-admin-only", func(http.ResponseWriter, *http.Request) {})

	req := httptest.NewRequest(http.MethodGet, "/admin/api/usage/future-admin-only", nil)
	req = req.WithContext(context.WithValue(req.Context(), userKey, normal))
	rec := httptest.NewRecorder()
	reached := false
	AdminGateGuard(mux, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
	})).ServeHTTP(rec, req)

	if reached {
		t.Error("未列入白名单的新端点被放行了 —— 这正是本次加固要消灭的静默越权")
	}
	if rec.Code != http.StatusForbidden {
		t.Errorf("code = %d, want 403", rec.Code)
	}
}

// 拿不到身份（中间件没注入）一律 403 —— 不能因为「没登录」就放行。
func TestAdminGate_NoIdentityIsRejected(t *testing.T) {
	code, reached := gateAt(t, nil, http.MethodGet, "/admin/api/users")
	if reached {
		t.Error("无身份竟被放行进管理端点")
	}
	if code != http.StatusForbidden {
		t.Errorf("code = %d, want 403", code)
	}
}

// 引导态合成 user（ID 为空、role=admin）能过 —— 引导窗口需要它建第一个账号。
func TestAdminGate_BootstrapSyntheticAdminPasses(t *testing.T) {
	bootstrap := &store.User{Role: store.RoleAdmin, Status: store.UserStatusActive}
	code, reached := gateAt(t, bootstrap, http.MethodGet, "/admin/api/users")
	if !reached {
		t.Errorf("引导态合成管理员被拦（code=%d）：首次引导会卡住", code)
	}
}

// 管理员对所有路径都能进 —— 白名单不该反过来把 admin 挡住。
func TestAdminGate_AdminPassesEverything(t *testing.T) {
	admin := &store.User{ID: "a1", Username: "a1", Role: store.RoleAdmin, Status: store.UserStatusActive}
	for _, c := range []struct{ method, path string }{
		{http.MethodGet, "/admin/api/topups"},
		{http.MethodGet, "/admin/api/users"},
		{http.MethodGet, "/admin/api/groups"},
		{http.MethodGet, "/admin/api/providers"},
	} {
		if _, reached := gateAt(t, admin, c.method, c.path); !reached {
			t.Errorf("管理员访问 %s %s 被拦", c.method, c.path)
		}
	}
}
