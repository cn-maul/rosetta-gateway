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
//   - POST /admin/api/reload 本身就是重建，跳过以免双跑；providers 的
//     test / models/discover 是只读探测，也不触发。
//     首次设置密码（/admin/api/bootstrap）要审计但同样不重建 —— 见下方。
//
// 重建与请求生命周期解耦（context.Background() + 超时）：客户端写完就断开
// 不该把重建一起取消，否则这次写入会悬空到下一次触发。
// 重建失败时响应已发出、无法改写，只能 ERROR 留痕 —— 运行时与库的分叉
// 由下一次写操作或手动 reload 收敛。
func AutoReload(next http.Handler, reload func(context.Context) error, audit WriteAuditor, logger *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 请求体只在此处缓存一份（上限 maxAuditBodyBytes）：handler 要读，审计要字段名。
		// 超过上限时**必须原样放行**：body 被截断后 handler 会拿残缺 JSON 去解析，
		// 报出一个与真实原因无关的 400（大 provider 的模型批量导入就会这样失败）。
		// 审计字段名可以为空，body 不能被中间件改坏。
		var auditFields string
		switch r.Method {
		case http.MethodPost, http.MethodPatch, http.MethodPut:
			if r.Body != nil {
				buf, _ := io.ReadAll(io.LimitReader(r.Body, maxAuditBodyBytes+1))
				if int64(len(buf)) <= maxAuditBodyBytes {
					r.Body = io.NopCloser(bytes.NewReader(buf))
					auditFields = extractFieldNames(buf)
				} else {
					// 超限：只把已读部分拼回去，还原成完整的原 body。
					// 上限本身由 decodeJSON（max_request_body_bytes）负责拒绝，
					// 中间件不做第二道限制。
					r.Body = struct {
						io.Reader
						io.Closer
					}{
						Reader: io.MultiReader(bytes.NewReader(buf), r.Body),
						Closer: r.Body,
					}
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

		// 首次设置管理员密码（/admin/api/bootstrap）**必须审计但绝不重建**：
		// 它是免鉴权写接口，是整个系统最敏感的一步；而它只改 users 表的密码
		// 哈希，不动快照，重建纯属无谓开销。
		// （2026-10-06 起接替已删除的 /admin/api/password/set —— 后者的豁免
		//  窗口正是那个「任何人可劫持管理员密码」的漏洞入口。）
		isBootstrap := p == "/admin/api/bootstrap"
		if status >= http.StatusBadRequest && !isBootstrap {
			return
		}
		if audit != nil {
			audit(r.Method, p, status, clientIP(r), auditFields)
		}
		if isBootstrap || status >= http.StatusBadRequest {
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

// AuditOnly 给免鉴权的 bootstrap 端点单独挂审计。
//
// 为什么需要它：POST /admin/api/bootstrap 走 publicAdminMux，**不经过**
// AutoReload —— 而 AutoReload 里那个 `isBootstrap` 审计分支恰恰是为它写的，
// 于是成为死代码：整个系统最敏感的一步（设置管理员密码）零审计留痕。
//
// 为什么不用 AutoReload：它会在审计之后触发快照重建，而 bootstrap 只改
// users 表的密码哈希、不动任何快照，重建纯属无谓开销（且首次部署时
// provider 池还没建，重建反而可能报错）。
//
// 只审计不重建，是这个端点需要的全部语义。
func AuditOnly(next http.Handler, audit WriteAuditor) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if audit == nil || r.Method != http.MethodPost {
			next.ServeHTTP(w, r)
			return
		}

		// 请求体在此处缓存一份供handler 读，审计只要字段名（body 里有密码明文，
		// 绝不能落库）。超过上限原样放行：截断的 body 会让 handler 拿残缺 JSON
		// 去解析，报出与真实原因无关的错误。字段名可以为空，body 不能被改坏。
		var fields string
		var body []byte
		if r.Body != nil && r.ContentLength != 0 && r.ContentLength <= maxAuditBodyBytes {
			body, _ = io.ReadAll(r.Body)
			fields = extractFieldNames(body)
			r.Body = io.NopCloser(bytes.NewReader(body))
		}

		sw := &statusResponseWriter{ResponseWriter: w, statusCode: http.StatusOK}
		next.ServeHTTP(sw, r)

		// 只审计成功：失败的那次没有改变任何状态，记下来只会淹没真正的变更。
		// 但 bootstrap 的失败要留痕 —— 它意味着有人反复在猜管理员密码，
		// 属于攻击信号。因此这里放宽到 >= 200 即记录（2xx 与 4xx 都记）。
		if sw.StatusCode() < 400 {
			audit(r.Method, r.URL.Path, sw.StatusCode(), clientIP(r), fields)
		}
	})
}
