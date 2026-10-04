package admin

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestDecodeJSON_RejectsNonJSONContentType 锁死管理面 CSRF 的第一道防线。
//
// 攻击链：POST /admin/api/password/set 在「尚未配置凭据」时被中间件豁免
// （全新部署的默认状态），而 text/plain 是 CORS 的 safelisted Content-Type
// —— 浏览器发它不触发预检。若 decodeJSON 不校验 Content-Type，任意第三方
// 页面都能在管理员首次访问网关的那次会话里跨站把管理密码设成自己的，
// 随后正常登录读走全部上游 API Key。
func TestDecodeJSON_RejectsNonJSONContentType(t *testing.T) {
	st := newTestStore(t)
	h := NewKeyHandler(st)

	// text/plain 是 CORS 简单请求的 safelisted 类型，**不触发预检**，
	// 所以这是攻击者唯一需要的 Content-Type。
	req := httptest.NewRequest(http.MethodPost, "/admin/api/keys",
		bytes.NewBufferString(`{"name":"pwned"}`))
	req.Header.Set("Content-Type", "text/plain")

	rec := httptest.NewRecorder()
	h.Create(rec, req)

	if rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("text/plain 应被 415 拒绝，实际 %d body=%s", rec.Code, rec.Body.String())
	}
	// 关键：不能只回 415，还必须真的没建出记录。
	list, err := st.ListAccessKeys(t.Context())
	if err != nil {
		t.Fatalf("list keys: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("请求被拒但记录已落库：%+v", list)
	}
}

// TestDecodeJSON_AcceptsJSONContentType 保证 Content-Type 断言没有误伤：
// 真实前端（web/src/api.ts:47）发的就是 application/json。
func TestDecodeJSON_AcceptsJSONContentType(t *testing.T) {
	st := newTestStore(t)
	h := NewKeyHandler(st)

	for _, ct := range []string{
		"application/json",
		"application/json; charset=utf-8",
		"APPLICATION/JSON",
		"application/vnd.api+json", // RFC 6839 结构化后缀
	} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/admin/api/keys",
			bytes.NewBufferString(`{"name":"ok"}`))
		req.Header.Set("Content-Type", ct)
		h.Create(rec, req)
		if rec.Code != http.StatusCreated {
			t.Fatalf("Content-Type %q 应被接受，实际 %d body=%s", ct, rec.Code, rec.Body.String())
		}
	}
}

// TestDecodeJSON_MissingContentTypeRejected 确认缺失 Content-Type 也被拒。
//
// 浏览器对简单请求可以省略 Content-Type，我们要的正是让这些请求进不来。
func TestDecodeJSON_MissingContentTypeRejected(t *testing.T) {
	st := newTestStore(t)
	h := NewKeyHandler(st)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/admin/api/keys",
		bytes.NewBufferString(`{"name":"pwned"}`))
	// 刻意不设 Content-Type
	h.Create(rec, req)

	if rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("缺失 Content-Type 应被 415 拒绝，实际 %d", rec.Code)
	}
}

// TestDecodeJSON_MalformedBodyStill400 保证 415 与 400 两条路径不串：
// Content-Type 正确但 body 是坏 JSON 时，必须是 400 而不是 415。
func TestDecodeJSON_MalformedBodyStill400(t *testing.T) {
	st := newTestStore(t)
	h := NewKeyHandler(st)

	rec := httptest.NewRecorder()
	req := jsonRequest(http.MethodPost, "/admin/api/keys",
		bytes.NewBufferString(`{not json`))
	h.Create(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("坏 JSON 应为 400，实际 %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "invalid JSON") {
		t.Fatalf("400 响应的文案应说明是 JSON 解析失败，实际 %s", rec.Body.String())
	}
}
