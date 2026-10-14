package server

// 2026-10-14 上线前审计加固（管理面 CSRF 中间件兜底）的回归测试。
//
// # 修复前的形态
//
// 管理面的 CSRF 防线是**分散**的，靠两件事凑齐：
//   - 会话 cookie 的 SameSite=Strict（挡绝大多数跨站请求）；
//   - decodeJSON 强制 Content-Type: application/json（挡住 text/plain 这类
//     CORS safelisted 简单请求）。
//
// 两者都是**逐 handler**才生效的。凡是不走 decodeJSON 的写端点就没有
// 第二道门 —— 实测 `POST /admin/api/usage/prune`、`POST /admin/api/logout`、
// `POST /admin/api/keys/{id}/recompute-usage` 这类无请求体端点，
// 带有效会话 + 跨站头即可触达 handler 并返回 200。
//
// # 为什么 SameSite=Strict 挡不住这一类
//
// Strict 只在跨**站**（registrable domain 不同）时不发 cookie。「同站不同源」
// —— gw.corp.example.com 与攻击者拿到的 evil.corp.example.com —— 属于
// same-site，浏览器照发 cookie，而 origin 确实不同。企业内网里子域可被
// 注册/接管是常见的，所以这不是理论场景。
//
// # 本文件钉住的判定边界
//
//   - cookie 凭据 + 写方法 + 非同源 → 403，且**不得触达**下游 handler；
//   - cookie 凭据 + 同源 → 放行；
//   - Bearer 凭据 + 跨源 → 放行（浏览器跨源发 Authorization 会触发 CORS
//     预检，而 /admin 从不设 Access-Control-Allow-Origin，预检必然失败；
//     非浏览器客户端本就不受同源策略约束）；
//   - 读方法（GET/HEAD/OPTIONS）→ 放行（按语义无副作用，且管理面的跨源
//     读取本来就由「不设 CORS 头」在浏览器侧拦住）。

import (
	"context"
	"net/http"
	"testing"

	"github.com/cn-maul/rosetta-gateway/internal/store"
	"github.com/cn-maul/rosetta-gateway/internal/userauth"
)

// activeSessionStore 返回一个恒定 admin 的会话存储替身。
//
// 与 bootstrapSessionStore（恒返回 nil，钉「未登录」）相对：这里要钉的是
// 「**有**有效会话时同源校验是否生效」，所以必须能通过鉴权走到那一行。
type activeSessionStore struct{ user *store.User }

func (s activeSessionStore) GetUser(context.Context, string) (*store.User, error) {
	return s.user, nil
}
func (activeSessionStore) CountUsers(context.Context) (int, error)      { return 1, nil }
func (activeSessionStore) TouchUserLogin(context.Context, string) error { return nil }

// issueTestSession 签发一个与该用户 authVersion 匹配的会话令牌。
func issueTestSession(t *testing.T, mgr *userauth.Manager, userID, role string,
	authVersion int64) string {
	t.Helper()
	tok, _, err := mgr.Issue(&userauth.UserClaims{
		UserID: userID, AuthVersion: authVersion, Role: role, Username: userID,
	})
	if err != nil {
		t.Fatalf("issue session: %v", err)
	}
	return tok
}

// csrfHarness 装好一条「会话鉴权 + 下游 handler」的最短真实链路。
type csrfHarness struct {
	mgr *userauth.Manager
	a   *UserAuthMiddleware
	tok string
}

func newCSRFHarness(t *testing.T) *csrfHarness {
	t.Helper()
	mgr := newEnabledTestManager(t)
	u := &store.User{
		ID: "u-admin", Username: "admin", PasswordHash: "h",
		Role: store.RoleAdmin, Status: store.UserStatusActive, AuthVersion: 1,
	}
	return &csrfHarness{
		mgr: mgr,
		a:   NewUserAuth(mgr, activeSessionStore{user: u}),
		tok: issueTestSession(t, mgr, u.ID, u.Role, u.AuthVersion),
	}
}

// withCookie / withBearer 是两种凭据的携带方式。
func withCookie(req *http.Request, tok string) *http.Request {
	req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: tok})
	return req
}

func withBearer(req *http.Request, tok string) *http.Request {
	req.Header.Set("Authorization", "Bearer "+tok)
	return req
}

// sameSiteCrossOrigin 复现「同站不同源」：Sec-Fetch-Site 报 same-site，
// 但 Origin 指向另一个主机。Strict cookie 在这一格**照发**。
func sameSiteCrossOrigin(req *http.Request) *http.Request {
	req.Header.Set("Sec-Fetch-Site", "same-site")
	req.Header.Set("Origin", "http://evil.corp.example.com")
	return req
}

// crossSite 复现严格跨站。
func crossSite(req *http.Request) *http.Request {
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	return req
}

// run 跑一次请求，返回下游 handler 是否被触达与响应码。
func (h *csrfHarness) run(t *testing.T, req *http.Request) (reached bool, code int) {
	t.Helper()
	rec := newRecorder()
	h.a.ServeHTTP(rec, req, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		reached = true
	}))
	return reached, rec.Code
}

// 核心用例：cookie 凭据 + 写方法 + 非同源 → 403，且 handler 不得被触达。
//
// 「不得被触达」与 403 本身同等重要：中间件写出响应后若仍调 next，
// handler 一侧的副作用（删 key、剪枝归档、改配置）就已经发生了，
// 403 只是一张事后通知。
func TestCSRF_CookieCrossOriginWriteIsBlocked(t *testing.T) {
	h := newCSRFHarness(t)

	cases := []struct {
		name   string
		method string
		mutate func(*http.Request) *http.Request
	}{
		{"POST 同站不同源", http.MethodPost, sameSiteCrossOrigin},
		{"POST 跨站", http.MethodPost, crossSite},
		// 无请求体、不调 decodeJSON 的写端点形态 —— 修复前这一格完全没有
		// 第二道防线，带会话 + 跨站头即可触达 handler（返回 200）。
		{"POST 只有 same-site 头", http.MethodPost, func(r *http.Request) *http.Request {
			r.Header.Set("Sec-Fetch-Site", "same-site")
			return r
		}},
		{"PUT 跨站", http.MethodPut, crossSite},
		{"PATCH 跨站", http.MethodPatch, crossSite},
		{"DELETE 跨站", http.MethodDelete, crossSite},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := newRequest(tc.method, "/admin/api/usage/prune", nil)
			req = withCookie(req, h.tok)
			req = tc.mutate(req)

			reached, code := h.run(t, req)
			if reached {
				t.Error("跨源写请求触达了 handler —— 副作用已经发生，403 只是事后通知")
			}
			if code != http.StatusForbidden {
				t.Errorf("状态码 = %d, want %d", code, http.StatusForbidden)
			}
		})
	}
}

// 同源 cookie 写请求必须放行 —— 否则这道闸门自己就把管理面锁死了。
func TestCSRF_SameOriginCookieWritePasses(t *testing.T) {
	h := newCSRFHarness(t)

	cases := []struct {
		name   string
		mutate func(*http.Request) *http.Request
	}{
		{"Sec-Fetch-Site=same-origin", func(r *http.Request) *http.Request {
			r.Header.Set("Sec-Fetch-Site", "same-origin")
			return r
		}},
		{"Origin 与 Host 一致", func(r *http.Request) *http.Request {
			r.Header.Set("Origin", "http://"+r.Host)
			return r
		}},
		// curl / 脚本化运维：不带任何浏览器上下文头，必须照常可用。
		{"无浏览器头（curl）", func(r *http.Request) *http.Request { return r }},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := newRequest(http.MethodPost, "/admin/api/usage/prune", nil)
			req = withCookie(req, h.tok)
			req = tc.mutate(req)

			reached, code := h.run(t, req)
			if !reached {
				t.Errorf("同源写请求被中间件拦下（状态码 %d）—— 闸门误伤了正常操作", code)
			}
			if code != http.StatusOK {
				t.Errorf("状态码 = %d, want %d", code, http.StatusOK)
			}
		})
	}
}

// Bearer 凭据不拦：脚本化运维（curl / SDK）必须继续可用。
//
// 浏览器侧的安全由「/admin 从不设 Access-Control-Allow-Origin」保证：
// 跨源发带 Authorization 头的请求会触发 CORS 预检，预检必然失败，
// 浏览器根本不会把真正的请求发出去。非浏览器客户端不受同源策略约束，
// 对它而言同源校验没有意义 —— 它手里的 Bearer 令牌才是凭据。
func TestCSRF_BearerCrossOriginWritePasses(t *testing.T) {
	h := newCSRFHarness(t)

	for _, site := range []string{"same-site", "cross-site"} {
		t.Run(site, func(t *testing.T) {
			req := newRequest(http.MethodPost, "/admin/api/usage/prune", nil)
			req = withBearer(req, h.tok)
			req.Header.Set("Sec-Fetch-Site", site)
			req.Header.Set("Origin", "http://evil.corp.example.com")

			reached, code := h.run(t, req)
			if !reached {
				t.Errorf("Bearer 凭据的跨源写请求被拦下（状态码 %d）—— 脚本化运维会坏掉", code)
			}
			if code != http.StatusOK {
				t.Errorf("状态码 = %d, want %d", code, http.StatusOK)
			}
		})
	}
}

// 读方法跨源必须放行。
//
// 按语义 GET/HEAD/OPTIONS 必须无副作用；管理面的跨源**读取**由「不设 CORS
// 头」在浏览器侧拦住（响应拿不到，CSRF 拿不到数据也发不出去）。
// 在这里拦读方法只会造成一种结果：同站不同源的部署里，浏览器发起的正常
// 轮询/刷新被 403，界面看起来像「随机请求失败」。
func TestCSRF_CrossOriginReadsPass(t *testing.T) {
	h := newCSRFHarness(t)

	for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodOptions} {
		t.Run(method, func(t *testing.T) {
			req := newRequest(method, "/admin/api/keys", nil)
			req = withCookie(req, h.tok)
			req = sameSiteCrossOrigin(req)

			reached, code := h.run(t, req)
			if !reached {
				t.Errorf("%s 被同源校验拦下（状态码 %d）—— 读方法不该被这道闸门管", method, code)
			}
			if code != http.StatusOK {
				t.Errorf("状态码 = %d, want %d", code, http.StatusOK)
			}
		})
	}
}

// 空 cookie 值不算「凭 cookie 进来的」，不该被同源校验拦。
//
// 边界细节：浏览器会把一个已清空的同名 cookie 一并带上（值为空串）。
// 鉴权此时由 Bearer 兜住，而同源校验不该再叠一刀。
func TestCSRF_EmptyCookieFallsBackToBearer(t *testing.T) {
	h := newCSRFHarness(t)

	req := newRequest(http.MethodPost, "/admin/api/usage/prune", nil)
	req = withCookie(req, "") // 空串：等价于没有 cookie
	req = withBearer(req, h.tok)
	req = sameSiteCrossOrigin(req)

	reached, code := h.run(t, req)
	if !reached {
		t.Errorf("空 cookie + Bearer 的请求被同源校验拦下（状态码 %d）", code)
	}
	if code != http.StatusOK {
		t.Errorf("状态码 = %d, want %d", code, http.StatusOK)
	}
}
