package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"sort"
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

// maxAuditBodyBytes 是为提取字段名而缓存请求体的上限。管理写请求都是小 JSON；
// 超过上限的字段名记为空（审计不因大 body 失效）。
const maxAuditBodyBytes = 64 << 10

// WriteAuditor 是审计回调：一次管理写操作成功后的留痕入口。
// fields 是请求体顶层字段名（不含值 —— body 里会有 api_key/密码明文）。
type WriteAuditor func(method, path string, status int, remote, fields string)

// AutoReload 在管理写操作成功落库后自动重建运行时（上游池 + 快照），
// 使配置变更不再依赖前端自觉调用 POST /admin/api/reload：
// 任何带凭据的调用方（curl / 脚本 / 第三方集成）写完立即生效 ——
// 包括安全敏感的「禁用下游 Key」：auth 校验读的是快照，不重建就照常放行。
//
// 同时按 WriteAuditor 留痕写操作审计（DESIGN §13.3）。审计在重建之前同步执行：
// 失败只记 WARN，不阻塞管理操作，更不能让它吞掉重建。
//
// 触发规则：
//   - 只对写方法（POST/PATCH/PUT/DELETE）触发，GET 一律跳过；
//   - 只在响应状态码 < 400（业务成功）时触发，失败的写入不重建；
//   - POST /admin/api/reload 本身就是重建，跳过以免双跑；password/set
//     只动凭据文件、不影响运行时；providers 的 test / models/discover
//     是只读探测，也不触发（但 password/set 照常审计）。
//
// 重建与请求生命周期解耦（context.Background() + 超时）：客户端写完就断开
// 不该把重建一起取消，否则这次写入会悬空到下一次触发。
// 重建失败时响应已发出、无法改写，只能 ERROR 留痕 —— 运行时与库的分叉
// 由下一次写操作或手动 reload 收敛。
func AutoReload(next http.Handler, reload func(context.Context) error, audit WriteAuditor, logger *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 请求体只在此处缓存一份（上限 64KB）：handler 要读，审计要字段名。
		// 管理写请求都是小 JSON，内存代价可忽略。
		var auditFields string
		switch r.Method {
		case http.MethodPost, http.MethodPatch, http.MethodPut:
			if r.Body != nil {
				buf, _ := io.ReadAll(io.LimitReader(r.Body, maxAuditBodyBytes+1))
				r.Body = io.NopCloser(bytes.NewReader(buf))
				if int64(len(buf)) <= maxAuditBodyBytes {
					auditFields = extractFieldNames(buf)
				}
			}
		}

		next.ServeHTTP(w, r)

		switch r.Method {
		case http.MethodPost, http.MethodPatch, http.MethodPut, http.MethodDelete:
		default:
			return
		}
		p := r.URL.Path
		if p == "/admin/api/reload" ||
			strings.HasSuffix(p, "/test") || strings.HasSuffix(p, "/discover") {
			return
		}
		status := http.StatusOK
		if sg, ok := w.(statusGetter); ok {
			status = sg.StatusCode()
		}

		// password/set 不触发重建（凭据文件不在快照里），但必须审计 ——
		// 它恰恰是管理面最敏感的写操作。其余非成功写入不审计不重建。
		isPasswordSet := p == "/admin/api/password/set"
		if status >= http.StatusBadRequest && !isPasswordSet {
			return
		}
		if audit != nil {
			audit(r.Method, p, status, clientIP(r), auditFields)
		}
		if isPasswordSet || status >= http.StatusBadRequest {
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

// extractFieldNames 提取 JSON body 的顶层字段名（排序后逗号分隔）。
// 非法 JSON / 空 body 返回空串 —— 字段名是尽力而为的补充，不是审计成立的条件。
func extractFieldNames(body []byte) string {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return ""
	}
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	sort.Strings(names)
	return strings.Join(names, ",")
}
