package server

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// statusGetter 由 Middleware 的 statusResponseWriter 实现，AutoReload 用它读
// handler 实际写出的状态码。断言失败（如测试里传入裸 ResponseWriter）时按
// 成功处理 —— 宁可多重建一次，也不漏掉一次生效。
type statusGetter interface{ StatusCode() int }

// autoReloadTimeout 是单次重建的时间上限。重建只做 DB 读与 client 构建，
// 正常毫秒级；上限只为防一个卡死的 SQLite 读把 admin 写请求的 goroutine 拖住。
const autoReloadTimeout = 30 * time.Second

// AutoReload 在管理写操作成功落库后自动重建运行时（上游池 + 快照），
// 使配置变更不再依赖前端自觉调用 POST /admin/api/reload：
// 任何带凭据的调用方（curl / 脚本 / 第三方集成）写完立即生效 ——
// 包括安全敏感的「禁用下游 Key」：auth 校验读的是快照，不重建就照常放行。
//
// 触发规则：
//   - 只对写方法（POST/PATCH/PUT/DELETE）触发，GET 一律跳过；
//   - 只在响应状态码 < 400（业务成功）时触发，失败的写入不重建；
//   - POST /admin/api/reload 本身就是重建，跳过以免双跑；password/set
//     只动凭据文件、不影响运行时；providers 的 test / models/discover
//     是只读探测，也不触发。
//
// 重建与请求生命周期解耦（context.Background() + 超时）：客户端写完就断开
// 不该把重建一起取消，否则这次写入会悬空到下一次触发。
// 重建失败时响应已发出、无法改写，只能 ERROR 留痕 —— 运行时与库的分叉
// 由下一次写操作或手动 reload 收敛，这与前端 mutate() 失败仅弹 toast 的旧行为一致，
// 区别在于现在服务端是第一执行者，前端调用降级为兜底。
func AutoReload(next http.Handler, reload func(context.Context) error, logger *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r)

		switch r.Method {
		case http.MethodPost, http.MethodPatch, http.MethodPut, http.MethodDelete:
		default:
			return
		}
		p := r.URL.Path
		if p == "/admin/api/reload" || p == "/admin/api/password/set" ||
			strings.HasSuffix(p, "/test") || strings.HasSuffix(p, "/discover") {
			return
		}
		if sg, ok := w.(statusGetter); ok && sg.StatusCode() >= http.StatusBadRequest {
			return
		}

		ctx, cancel := context.WithTimeout(context.Background(), autoReloadTimeout)
		defer cancel()
		if err := reload(ctx); err != nil {
			logger.Error("auto reload after admin write failed; runtime lags behind database until next reload",
				"error", err, "path", p,
				"request_id", RequestIDFromContext(r.Context()))
		}
	})
}
