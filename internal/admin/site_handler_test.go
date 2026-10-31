package admin

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cn-maul/rosetta-gateway/internal/identity"
)

// site_handler_test.go 覆盖「站点品牌」两条端点。
//
// 重点不是 happy path，而是三条**失效模式**：
//   - 名称按字节而不是 rune 计数（中文名会被莫名其妙截短）；
//   - LOGO 收下远程 URL（把每个访客的浏览器送去第三方地址）；
//   - LOGO 不卡解码后体积（每次页面加载都拖着几 MB 的 data URL）。
// 另外单独断言「未配置」与「GET 免鉴权」—— 这两条决定了登录页能不能渲染。

func newSiteHandler(t *testing.T) *SiteHandler {
	t.Helper()
	return NewSiteHandler(newTestStore(t))
}

func putSite(t *testing.T, h *SiteHandler, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.Update(rec, jsonRequest(http.MethodPut, "/admin/api/site", bytes.NewBufferString(body)))
	return rec
}

func getSite(t *testing.T, h *SiteHandler) siteBrandingResponse {
	t.Helper()
	// 不经过 asAdmin：这条端点在生产上就是免鉴权的，测试里也不该伪造身份。
	// 伪造会让「有人给它加上 requireAdmin」这种回归测不出来。
	rec := httptest.NewRecorder()
	h.Get(rec, httptest.NewRequest(http.MethodGet, "/admin/api/site", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET site code = %d, body=%s", rec.Code, rec.Body.String())
	}
	var resp siteBrandingResponse
	decodeBody(t, rec, &resp)
	return resp
}

// 未设置时两个字段都是空串：后端不代填默认名，默认名归前端。
func TestSiteGetUnsetReturnsEmptyStrings(t *testing.T) {
	h := newSiteHandler(t)
	resp := getSite(t, h)
	if resp.SiteName != "" || resp.SiteLogo != "" {
		t.Fatalf("未配置时不应回显默认值，实际 %+v", resp)
	}
}

// PUT → GET 回读，且名称被 trim。
func TestSiteUpdateThenReadBack(t *testing.T) {
	h := newSiteHandler(t)
	logo := "data:image/png;base64," + base64.StdEncoding.EncodeToString([]byte("fake-png-bytes"))
	rec := putSite(t, h, `{"site_name":"  My Gateway  ","site_logo":"`+logo+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT code = %d, body=%s", rec.Code, rec.Body.String())
	}
	var status map[string]string
	decodeBody(t, rec, &status)
	if status["status"] != "ok" {
		t.Fatalf("PUT 响应形状错误: %s", rec.Body.String())
	}

	resp := getSite(t, h)
	if resp.SiteName != "My Gateway" {
		t.Fatalf("site_name 未 trim 或未落库: %q", resp.SiteName)
	}
	if resp.SiteLogo != logo {
		t.Fatalf("site_logo 未原样保存: %q", resp.SiteLogo)
	}

	// 空串 = 清除。这条容易漏：前端「移除 LOGO」发的是空串，
	// 若把空串当作「未提供、保持原值」，用户就永远删不掉 LOGO。
	rec = putSite(t, h, `{"site_name":"My Gateway","site_logo":""}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("清空 LOGO 应成功，code = %d, body=%s", rec.Code, rec.Body.String())
	}
	if got := getSite(t, h); got.SiteLogo != "" {
		t.Fatalf("清空后 site_logo 应为空，实际 %q", got.SiteLogo)
	}
}

// 名称上限按 rune 计：65 个汉字必须 400，而不是「第 22 个汉字就 400」。
func TestSiteUpdateRejectsTooLongName(t *testing.T) {
	h := newSiteHandler(t)

	// 64 个汉字（192 字节）是合法的 —— 这条同时证明计数用的是 rune。
	okBody, _ := json.Marshal(map[string]string{"site_name": strings.Repeat("中", maxSiteNameRunes)})
	if rec := putSite(t, h, string(okBody)); rec.Code != http.StatusOK {
		t.Fatalf("%d 个汉字应被接受，code = %d, body=%s", maxSiteNameRunes, rec.Code, rec.Body.String())
	}

	tooLong, _ := json.Marshal(map[string]string{"site_name": strings.Repeat("中", maxSiteNameRunes+1)})
	rec := putSite(t, h, string(tooLong))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("超长名称应为 400，实际 %d, body=%s", rec.Code, rec.Body.String())
	}
	assertSingleErrorEnvelope(t, rec, http.StatusBadRequest)
	if !strings.Contains(rec.Body.String(), "64") {
		t.Fatalf("错误文案应说明上限 64，实际 %s", rec.Body.String())
	}
}

// LOGO 只接受内联 data URL：远程 URL 必须 400。
func TestSiteUpdateRejectsRemoteLogoURL(t *testing.T) {
	h := newSiteHandler(t)
	for _, logo := range []string{
		"https://example.com/logo.png",
		"data:text/plain;base64,aGVsbG8=",
		"data:image/png;base64",       // 缺 ";base64," 之外的逗号
		"data:image/;base64,aGVsbG8=", // 缺图片类型
	} {
		body, _ := json.Marshal(map[string]string{"site_logo": logo})
		rec := putSite(t, h, string(body))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("logo=%q 应为 400，实际 %d, body=%s", logo, rec.Code, rec.Body.String())
		}
		assertSingleErrorEnvelope(t, rec, http.StatusBadRequest)
	}
	// 被拒绝后库里必须仍是空的：校验失败不能顺手写进去。
	if got := getSite(t, h); got.SiteLogo != "" {
		t.Fatalf("校验失败不应落库，实际 %q", got.SiteLogo)
	}
}

// LOGO 超过解码后 256KB 必须 400，且文案说明上限。
func TestSiteUpdateRejectsOversizeLogo(t *testing.T) {
	h := newSiteHandler(t)

	// 恰好 256KB 合法；多 1 字节非法。边界两侧都测，避免「>= 写成 >」这类错。
	atLimit := "data:image/png;base64," + base64.StdEncoding.EncodeToString(make([]byte, maxSiteLogoBytes))
	body, _ := json.Marshal(map[string]string{"site_logo": atLimit})
	if rec := putSite(t, h, string(body)); rec.Code != http.StatusOK {
		t.Fatalf("恰好 %d 字节应被接受，code = %d, body=%s", maxSiteLogoBytes, rec.Code, rec.Body.String())
	}

	overLimit := "data:image/png;base64," + base64.StdEncoding.EncodeToString(make([]byte, maxSiteLogoBytes+1))
	body, _ = json.Marshal(map[string]string{"site_logo": overLimit})
	rec := putSite(t, h, string(body))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("超过 %d 字节应为 400，实际 %d, body=%s", maxSiteLogoBytes, rec.Code, rec.Body.String())
	}
	assertSingleErrorEnvelope(t, rec, http.StatusBadRequest)
	if !strings.Contains(rec.Body.String(), "256KB") {
		t.Fatalf("错误文案应说明 256KB 上限，实际 %s", rec.Body.String())
	}
}

// GET 在没有会话时必须 200 —— 登录页在取得会话之前就要渲染品牌。
//
// 这条是「免鉴权」的 handler 级证明；mux 级的证明在 cmd/gateway/routes_test.go
// 的 public 清单与 internal/server/authz_matrix_test.go 的 I3 断言里。
func TestSiteGetWithoutSession(t *testing.T) {
	h := newSiteHandler(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/api/site", nil)
	// 显式确认没有身份进入 handler。
	if user := identity.UserFromContext(req.Context()); user != nil {
		t.Fatalf("测试前置错误：请求意外带了身份 %+v", user)
	}
	h.Get(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("无会话 GET /admin/api/site 必须 200，实际 %d, body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("Content-Type 应为 application/json，实际 %q", ct)
	}
}
