package admin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestAdminQueries_HonorRequestCancellation 钉住「管理面的查询吃请求上下文」。
//
// # 缺陷的形状
//
// 这些查询原先是 `store.Reader().Query(...)` / `.QueryRow(...)` —— 不带
// context 的版本。于是客户端断开、代理超时、浏览器关页面之后，SELECT 仍然
// 在读池里跑完：读池只有 4 条连接，管理面最重的几条聚合（全表扫描 +
// GROUP BY）只要残留几次，就能让「打开用量页」变成几十秒。
//
// # 怎么测
//
// 把请求上下文取消掉再调 handler。吃上下文的查询会立刻返回
// "context canceled" 并走错误分支；不吃上下文的查询会**成功**返回 200 ——
// 所以「返回码不是 200」就是这个断言的全部内容。这里刻意不断言具体错误
// 文案（不锁死实现），只看「查询有没有被取消这件事传下去」。
func TestAdminQueries_HonorRequestCancellation(t *testing.T) {
	st := newScopeStore(t)
	uh := NewUsageHandler(st)

	cases := []struct {
		name string
		call func(w http.ResponseWriter, r *http.Request)
	}{
		{"调用历史", uh.History},
		{"导出 CSV", uh.ExportCSV},
		{"按密钥", uh.GroupByKey},
		{"按模型", uh.GroupByModel},
		{"按上游", uh.GroupByProvider},
		{"按天", uh.GroupByDay},
		{"按用户", uh.GroupByUser},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := asAdmin(jsonRequest(http.MethodGet, "/admin/api/usage/history?from=0&to=0", nil))
			ctx, cancel := context.WithCancel(req.Context())
			cancel() // 模拟「客户端已经走了」
			req = req.WithContext(ctx)

			rec := httptest.NewRecorder()
			tc.call(rec, req)
			if rec.Code == http.StatusOK {
				t.Fatalf("上下文已取消却返回 200 —— 这条查询没有吃请求上下文，断开后仍会在读池里跑\nbody=%s",
					rec.Body.String())
			}
		})
	}
}
