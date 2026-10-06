package admin

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cn-maul/rosetta-gateway/internal/server"
	"github.com/cn-maul/rosetta-gateway/internal/store"
)

// jsonRequest 构造一个带 JSON 请求体的测试请求。
//
// 为什么需要它：decodeJSON 现在**强制断言 Content-Type 是 application/json**
// （这是防管理面 bootstrap CSRF 的第一道防线 —— text/plain 属于 CORS 的
// safelisted 类型，浏览器发它不触发预检，旧实现不校验 Content-Type 等于让
// 任意第三方页面在「尚未设置管理密码」的窗口里跨站抢占管理员）。
//
// 而旧的测试全部用裸 httptest.NewRequest，一个 Content-Type 都不设 ——
// 换句话说，**整个 admin 测试套件都在绕过这道防线**。这类盲区比缺陷本身
// 更危险：它让「加了防护但测试全绿」变成假信号。所以测试必须和真实前端
// （web/src/api.ts:47 显式设 application/json）一样把头设上。
func jsonRequest(method, target string, body io.Reader) *http.Request {
	r := httptest.NewRequest(method, target, body)
	if body != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	return r
}

// asAdmin 给请求注入一个 admin 身份。
//
// # 为什么需要它
//
// 多用户改造后，管理端 handler 全部通过 server.UserFromContext 判定权限：
// 拿不到身份时 usage 过滤会加上「不可能匹配任何行」的哨兵条件
// （callerScope 的设计），key 的增删改则直接 401。
//
// 而这些测试是改造前写的 —— 它们直接调 handler，不经过中间件，
// 于是身份为空、不停用断言。**这是测试适配新契约，不是代码缺陷**。
func asAdmin(r *http.Request) *http.Request {
	return asAdminID(r, "admin-test")
}

// asAdminID 注入一个**指定 id** 的 admin 身份。
//
// 为什么要带 id 参数：handler 里的「不能对自己下手」防护
// （UpdateUser 的降级/禁用拦截、DeleteUser 的自删拦截）靠
// `me.ID == 目标ID` 判定。测这些防护时，身份 id 必须与操作目标一致，
// 否则测的是「管理员改别人」而不是「管理员改自己」，防护不会触发 ——
// 那样测试会假通过，而防护实际是坏的。
func asAdminID(r *http.Request, id string) *http.Request {
	ctx := context.WithValue(r.Context(), server.UserCtxKey(), &store.User{
		ID: id, Username: id,
		Role: store.RoleAdmin, Status: store.UserStatusActive, AuthVersion: 1,
	})
	return r.WithContext(ctx)
}

// asUser 给请求注入一个普通用户身份（用于验证作用域收窄）。
func asUser(r *http.Request, id string) *http.Request {
	ctx := context.WithValue(r.Context(), server.UserCtxKey(), &store.User{
		ID: id, Username: id,
		Role: store.RoleUser, Status: store.UserStatusActive, AuthVersion: 1,
	})
	return r.WithContext(ctx)
}

// decodeBody 把响应体解成 v。
//
// 失败时把原始 body 一并打出来：JSON 解析失败的最常见原因是「响应其实是个
// 错误对象」或「写了两次响应」，这时候光看 unmarshal 的报错定位不到问题。
func decodeBody(t *testing.T, rec *httptest.ResponseRecorder, v any) {
	t.Helper()
	if err := json.Unmarshal(rec.Body.Bytes(), v); err != nil {
		t.Fatalf("decode response: %v body=%s", err, rec.Body.String())
	}
}
