package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// bootstrapCreds 是一个凭据状态的测试替身。
type bootstrapCreds struct {
	has bool
	ok  bool
}

func (s bootstrapCreds) Verify(string) bool  { return s.ok }
func (s bootstrapCreds) HasCredential() bool { return s.has }

// TestBootstrapPasswordSet_RejectsCrossOrigin 锁死 CSRF 的第二道防线。
//
// 第一道（admin.decodeJSON 的 Content-Type 断言）与请求体形状耦合；
// 这道只看浏览器行为，与 body 无关，是纵深防御。
//
// 攻击场景：全新部署（无 admin_auth.json、config 无 admin_token）时
// password/set 被豁免鉴权。管理员在该机浏览器访问任意第三方页面，
// 页面发 POST /admin/api/password/set。Sec-Fetch-Site 由浏览器强制发送、
// 页面 JS 无法伪造，因此这里能可靠拦住。
func TestBootstrapPasswordSet_RejectsCrossOrigin(t *testing.T) {
	cases := []struct {
		name  string
		build func(*http.Request)
	}{
		{"Sec-Fetch-Site=cross-site", func(r *http.Request) {
			r.Header.Set("Sec-Fetch-Site", "cross-site")
		}},
		{"Sec-Fetch-Site=same-site", func(r *http.Request) {
			// same-site 是「同站跨源」（a.example.com → b.example.com），
			// 对 CSRF 而言与跨源等价 —— 攻击者控制的子域也能打。
			r.Header.Set("Sec-Fetch-Site", "same-site")
		}},
		{"Origin 指向别处", func(r *http.Request) {
			r.Header.Set("Origin", "http://evil.example.com")
		}},
		{"Sec-Fetch-Site 优先于伪造的 Origin", func(r *http.Request) {
			r.Header.Set("Sec-Fetch-Site", "cross-site")
			r.Header.Set("Origin", "http://127.0.0.1:8080") // 伪造同源
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reached := false
			h := AdminAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				reached = true
			}), bootstrapCreds{has: false})

			req := httptest.NewRequest(http.MethodPost, "/admin/api/password/set", strings.NewReader(`{"password":"attacker123"}`))
			req.Host = "127.0.0.1:8080"
			tc.build(req)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if reached {
				t.Fatal("跨站请求被放行到了 handler —— 管理员密码可被第三方页面抢占")
			}
			if rec.Code != http.StatusForbidden {
				t.Fatalf("期望 403，实际 %d", rec.Code)
			}
		})
	}
}

// TestBootstrapPasswordSet_AllowsSameOriginAndCLI 保证防线没误伤：
// 浏览器同源访问要放行，curl/SDK 这类不带 Sec-Fetch-Site 的客户端也要放行
// （否则运维没法用命令行初始化管理密码）。
func TestBootstrapPasswordSet_AllowsSameOriginAndCLI(t *testing.T) {
	cases := []struct {
		name  string
		build func(*http.Request)
	}{
		{"Sec-Fetch-Site=same-origin", func(r *http.Request) {
			r.Header.Set("Sec-Fetch-Site", "same-origin")
		}},
		{"Sec-Fetch-Site=none（用户直接敲地址栏）", func(r *http.Request) {
			r.Header.Set("Sec-Fetch-Site", "none")
		}},
		{"Origin 与 Host 一致", func(r *http.Request) {
			r.Header.Set("Origin", "http://127.0.0.1:8080")
		}},
		{"curl：不带任何来源头", func(*http.Request) {}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reached := false
			h := AdminAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				reached = true
			}), bootstrapCreds{has: false})

			req := httptest.NewRequest(http.MethodPost, "/admin/api/password/set", strings.NewReader(`{"password":"legit-password-1"}`))
			req.Host = "127.0.0.1:8080"
			tc.build(req)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if !reached {
				t.Fatalf("同源/CLI 请求被误拒，状态 %d body=%s", rec.Code, rec.Body.String())
			}
		})
	}
}

// TestBootstrapPasswordSet_OnceCredentialExistsStillNeedsAuth 确认同源检查
// 没有把豁免范围扩大：一旦存在凭据，password/set 就回到普通鉴权路径，
// 同源也不通。
func TestBootstrapPasswordSet_OnceCredentialExistsStillNeedsAuth(t *testing.T) {
	reached := false
	h := AdminAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
	}), bootstrapCreds{has: true, ok: false})

	req := httptest.NewRequest(http.MethodPost, "/admin/api/password/set",
		strings.NewReader(`{"password":"attacker123"}`))
	req.Host = "127.0.0.1:8080"
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if reached {
		t.Fatal("已有凭据时 password/set 不该被放行")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("期望 401，实际 %d", rec.Code)
	}
}
