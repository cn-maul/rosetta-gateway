package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cn-maul/rosetta-gateway/internal/store"
	"github.com/cn-maul/rosetta-gateway/internal/userauth"
)

// 无用户会话存储替身：本文件的用例都跑在「未登录」状态，
// 因此鉴权不可能通过 —— 下游 handler 是否被触达完全由放行规则决定。
type bootstrapSessionStore struct{}

func (bootstrapSessionStore) GetUser(context.Context, string) (*store.User, error) { return nil, nil }
func (bootstrapSessionStore) CountUsers(context.Context) (int, error)              { return 0, nil }
func (bootstrapSessionStore) TouchUserLogin(context.Context, string) error         { return nil }

// TestSessionGate_RejectsAnonymous 确认统一认证后管理面**没有**任何免鉴权窗口。
//
// 改造前这里有三条旁路：运维凭据（admin_token / admin_auth.json）、
// 「users 表为空」的引导窗口、以及 password/check + password/set。
// 三条全部移除后，任何未携带有效会话的请求都必须在中间件这一层被拒，
// 且不得触达下游 handler。
//
// 这是本次改造最需要被钉住的不变量：一旦有人「为了方便」把某个端点
// 挪到 publicAdminMux 上，这就是唯一的哨兵。
func TestSessionGate_RejectsAnonymous(t *testing.T) {
	for _, path := range []string{
		"/admin/api/keys",
		"/admin/api/users",
		"/admin/api/providers",
		"/admin/api/settings",
		"/admin/api/me",
		// 旧的两条旁路：路由已删除，但断言保留 ——
		// 万一有人手滑把路径加回 publicAdminMux，这里立刻红。
		"/admin/api/password/set",
		"/admin/api/password/check",
	} {
		for _, method := range []string{http.MethodGet, http.MethodPost} {
			t.Run(method+" "+path, func(t *testing.T) {
				reached := false
				mgr := newEnabledTestManager(t)
				a := NewUserAuth(mgr, bootstrapSessionStore{})

				req := newRequest(method, path, nil)
				rec := newRecorder()
				a.ServeHTTP(rec, req, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
					reached = true
				}))

				if reached {
					t.Fatalf("%s %s 在未登录时被放行到了 handler", method, path)
				}
				if rec.Code != http.StatusUnauthorized {
					t.Fatalf("%s %s 期望 401，实际 %d", method, path, rec.Code)
				}
			})
		}
	}
}

// TestSessionGate_BearerTokenIsNotAnAdminCredential 钉死「旧运维凭据已失效」。
//
// 曾经 config 的 admin_token 是一把**万能钥匙**：不带会话、不过 users 表、
// 不受 auth_version 约束。统一认证后它必须与任何随机字符串一样被拒 ——
// 否则等于留了一条「谁改的查不到」的管理旁路。
func TestSessionGate_BearerTokenIsNotAnAdminCredential(t *testing.T) {
	reached := false
	mgr := newEnabledTestManager(t)
	a := NewUserAuth(mgr, bootstrapSessionStore{})

	req := newRequest(http.MethodGet, "/admin/api/users", nil)
	req.Header.Set("Authorization", "Bearer tok-1234567890-abcdefghijklmnop")
	rec := newRecorder()
	a.ServeHTTP(rec, req, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		reached = true
	}))

	if reached {
		t.Fatal("admin_token 仍能进管理面 —— 并行通道没有被真正移除")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("期望 401，实际 %d", rec.Code)
	}
}

// newEnabledTestManager 构造一个**已启用**的会话管理器。
// 本文件测的是「有会话机制、但请求没带会话」这条路径，所以必须真启用 ——
// 未启用时中间件会在鉴权之前就短路，断言到的就不是鉴权逻辑而是部署故障分支。
func newEnabledTestManager(t *testing.T) *userauth.Manager {
	t.Helper()
	t.Setenv(userauth.SecretEnvName, "csrf-test-secret-long-enough-32-chars")
	mgr, err := userauth.NewManager("")
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	if !mgr.Enabled() {
		t.Fatal("manager disabled in test; would short-circuit before the auth path")
	}
	return mgr
}

func newRequest(method, target string, body io.Reader) *http.Request {
	req := httptest.NewRequest(method, target, body)
	// Host 必填：httptest 的默认值为空，同源判定会比对失败。
	req.Host = "127.0.0.1:8080"
	return req
}

func newRecorder() *httptest.ResponseRecorder { return httptest.NewRecorder() }
