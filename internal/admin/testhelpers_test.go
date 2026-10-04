package admin

import (
	"io"
	"net/http"
	"net/http/httptest"
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
