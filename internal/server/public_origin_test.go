package server

import (
	"net/http"
	"testing"
	"time"
)

func serveWithPublicOrigin(h *csrfHarness, origin string, req *http.Request) (bool, int) {
	reached := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
	})
	guarded := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.a.ServeHTTP(w, r, next)
	})
	rec := newRecorder()
	WithPublicOrigin(guarded, origin).ServeHTTP(rec, req)
	return reached, rec.Code
}

// TLS 反代后浏览器看到 https，而进程内 r.TLS 为 nil。配置 public_origin
// 后，正常同源写请求必须放行；伪造的 X-Forwarded-Proto 不参与判定。
func TestPublicOrigin_ReverseProxySameOriginPasses(t *testing.T) {
	h := newCSRFHarness(t)
	req := newRequest(http.MethodPost, "/admin/api/usage/prune", nil)
	req = withCookie(req, h.tok)
	req.Header.Set("Origin", "https://gw.example.com")
	req.Header.Set("X-Forwarded-Proto", "http")

	reached, code := serveWithPublicOrigin(h, "https://gw.example.com", req)
	if !reached || code != http.StatusOK {
		t.Fatalf("反代同源写请求被拒：reached=%v code=%d", reached, code)
	}
}

func TestPublicOrigin_SchemeMismatchStillRejected(t *testing.T) {
	h := newCSRFHarness(t)
	req := newRequest(http.MethodPost, "/admin/api/usage/prune", nil)
	req = withCookie(req, h.tok)
	req.Header.Set("Origin", "http://gw.example.com")
	req.Header.Set("X-Forwarded-Proto", "https")

	reached, code := serveWithPublicOrigin(h, "https://gw.example.com", req)
	if reached || code != http.StatusForbidden {
		t.Fatalf("协议降级请求未被拒绝：reached=%v code=%d", reached, code)
	}
}

func TestPublicOrigin_HostMismatchStillRejected(t *testing.T) {
	h := newCSRFHarness(t)
	req := newRequest(http.MethodPost, "/admin/api/usage/prune", nil)
	req = withCookie(req, h.tok)
	req.Header.Set("Origin", "https://evil.example.com")

	reached, code := serveWithPublicOrigin(h, "https://gw.example.com", req)
	if reached || code != http.StatusForbidden {
		t.Fatalf("跨主机请求未被拒绝：reached=%v code=%d", reached, code)
	}
}

func TestSessionCookie_SecureFollowsPublicOrigin(t *testing.T) {
	for _, tc := range []struct {
		origin string
		secure bool
	}{
		{"https://gw.example.com", true},
		{"http://gw.example.com", false},
	} {
		t.Run(tc.origin, func(t *testing.T) {
			req := newRequest(http.MethodPost, "/admin/api/login", nil)
			rec := newRecorder()
			handler := WithPublicOrigin(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				SetSessionCookie(w, "tok", time.Now().Add(time.Hour), SessionCookieSecure(r))
			}), tc.origin)
			handler.ServeHTTP(rec, req)

			cookies := rec.Result().Cookies()
			if len(cookies) != 1 {
				t.Fatalf("cookie 数 = %d, want 1", len(cookies))
			}
			if cookies[0].Secure != tc.secure {
				t.Fatalf("Secure = %v, want %v", cookies[0].Secure, tc.secure)
			}
		})
	}
}
