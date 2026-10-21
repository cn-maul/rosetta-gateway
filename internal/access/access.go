package access

import (
	"net/http"

	"github.com/cn-maul/rosetta-gateway/internal/httpx"
	"github.com/cn-maul/rosetta-gateway/internal/identity"
)

// userAccessibleRoutes 是**普通用户**可访问的管理端点，按
// 「方法 + 路由模式」**精确**列出（不再是前缀）。
//
// # 为什么从「前缀白名单」改成「精确路由」（2026-10-10）
//
// 原来是一串前缀，用 strings.HasPrefix 判定。安全性靠「该放行的路径在列表里」，
// 但前缀匹配有个**结构性缺陷**：它会自动放行该子树下**未来新增的一切端点**。
//
// 实测踩到过：新增 /admin/api/usage/by-user（全站账单）时，它因为以
// /admin/api/usage 开头而被网关层**静默放行**，唯一防线只剩 handler 里的
// requireAdmin。当时是安全的（那个 handler 写了 requireAdmin），但：
//   - 网关层不拦、**也不报错**，表现为端点照常 200；
//   - 将来有人在这前缀下加端点忘了写 requireAdmin → 静默越权；
//   - 现有测试只能覆盖**已存在**的端点，覆盖不了「将来会加的」。
//
// 改成精确模式后，新增端点**默认 fail-closed**：它的模式不在本表里，
// 普通用户直接被网关层 403。要放行必须显式加一行 —— 漏洞从「静默」变成
// 「显式且立刻可见」。
//
// # 为什么用路由模式而不是裸路径
//
// 表里的键是 `mux.Handler(r)` 返回的**注册模式**（如
// `PATCH /admin/api/keys/{id}`），不是请求的原始路径。这样做的好处：
//   - 与真实路由用**同一个匹配引擎**，零漂移（手抄路径迟早对不上）；
//   - 路径参数天然支持（`{id}` 由 mux 负责匹配，不必自己解析）；
//   - 方法也是键的一部分，所以 `DELETE /admin/api/keys/{id}` 与
//     `GET /admin/api/keys` 是两个独立授权项 —— 不能靠「路径在表里」
//     就放行所有方法。
//
// # 每条都必须自己做作用域收窄
//
// 放行只意味着「能进这个端点」，**进来之后能看谁的数据**由 handler 判
// （见 internal/admin/key_handler.go 与 usage_handler.go 的 callerScope）。
// 否则本表本身就成了泄露入口。
//
// # 刻意**不在**表里的
//
//   - `/admin/api/usage/by-user`：全站账单，纯 admin-only。
//   - `/admin/api/usage/prune`：不可逆删除，纯 admin-only。
//
// 两者仍保留 handler 内的 requireAdmin 作为第二道防线（纵深防御）。
// 网关层现在也拦了 —— 这正是本次加固的目的。
var userAccessibleRoutes = map[string]bool{
	// 访问密钥：普通用户自助管理自己的 key（handler 按 user_id 收窄）。
	"GET /admin/api/keys":                       true,
	"POST /admin/api/keys":                      true,
	"PATCH /admin/api/keys/{id}":                true,
	"DELETE /admin/api/keys/{id}":               true,
	"POST /admin/api/keys/{id}/recompute-usage": true,

	// 用量与统计：均按 user_id 收窄。
	"GET /admin/api/usage":             true,
	"GET /admin/api/usage/by-key":      true,
	"GET /admin/api/usage/by-model":    true,
	"GET /admin/api/usage/by-provider": true,
	"GET /admin/api/usage/by-day":      true,
	"GET /admin/api/usage/history":     true,
	"GET /admin/api/usage/history.csv": true,
	"GET /admin/api/stats":             true,

	// 身份与自助改密。
	"GET /admin/api/me":           true,
	"POST /admin/api/me/password": true,

	// 充值流水：普通用户**要能看自己的**充值记录 —— 那正是钱包页的意义
	// （有权知道自己什么时候被充过钱、充了多少）。作用域完全由会话身份决定，
	// 该端点不接受任何参数指定查谁（见 admin.UserHandler.ListTopups）。
	"GET /admin/api/topups": true,

	"POST /admin/api/logout": true,

	// 模型名清单：key 级白名单要能「用户自助收紧」，就得让普通用户读到
	// 可选模型。返回内容已按身份收窄（只拿到自己组内的），且只是一串公开
	// 模型名 —— 不含上游、凭据、路由拓扑。
	"GET /admin/api/model-names": true,

	// 上游模型连通率（2026-10-14 页面合并：与「可用模型」合到同一张卡片
	// 里之后，普通用户也要读得到 —— 他能看到的模型列表和这些模型此刻通不通
	// 是同一个问题，拆开问等于让他在两个页面之间来回跳）。
	//
	// **只放行读**：写探测记录的循环在后台跑，这两个端点只读 model_probes。
	// 两个 handler 都已按组白名单收窄到「组内公开名的主目标模型」，
	// 交集为空时返回空集而不是全集（与 model-names 同一口径）。
	//
	// 不放行 /providers /upstream-models /routes：那些会暴露端点、
	// 凭据协议与整条故障转移链的拓扑。
	"GET /admin/api/probes/models":   true,
	"GET /admin/api/probes/timeline": true,
}

// AdminGateGuard 在会话鉴权之后再做一次「非白名单即要求管理员」的判定。
//
// 必须套在 UserAuth 之后 —— 它依赖 context 里的用户身份。
//
// # routes 参数的作用
//
// 它只用来说明「这个请求会被哪个路由模式处理」，**不负责转发**（转发交给
// next）。生产里两者是同一个 adminMux（见 cmd/gateway/main.go）——
// 传两次看着冗余，但这正是「查路由表」与「执行路由」解耦的代价：
// 本函数可以拿真实的注册模式去比对白名单，而不必手抄一份路径清单。
//
// 若 routes 为 nil（测试里可能），判定退化为「一律要求管理员」——
// fail-closed，不会因为漏传参数而放行任何人。
func AdminGateGuard(routes *http.ServeMux, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u := identity.UserFromContext(r.Context())
		// 管理员直接放行，连路由表都不必查。
		//
		// 必须同时校验 ID 非空：`IsAdmin()` 只看 Role，一个空 ID 的
		// admin 会被它判为真。而空 ID 意味着「没有真实身份」——那正是
		// 2026-10-12 统一改成 fail-closed 的状态（原 requireAdmin 在
		// internal/admin/user_admin_handler.go 曾放行它，已修复）。
		//
		// 生产中该状态不可达（身份唯一注入点是 server.UserAuthMiddleware，
		// 来自 store.GetUser 的 SQL 查询；空 id 查不到行会被 `u == nil`
		// 拦成 401），但闸门不该依赖「不可达」来维持安全。
		if u != nil && u.IsAdmin() && u.ID != "" {
			next.ServeHTTP(w, r)
			return
		}

		// 普通用户：拿真实路由模式去比对白名单。
		//
		// 未注册的路径 pattern 为空 → 不在表里 → 403（fail-closed）。
		// 这一点由 TestProbeServeMuxHandlerSemantics 的实测固定：
		// mux.Handler() 对未注册子路径返回空 pattern，且不匹配尾斜杠。
		if routes != nil {
			if _, pattern := routes.Handler(r); Allowed(pattern) {
				next.ServeHTTP(w, r)
				return
			}
		}

		httpx.WriteAuthError(w, http.StatusForbidden, "需要管理员权限")
	})
}

// Allowed 报告某个已注册路由模式是否对普通用户开放。
// 未注册/未知模式一律 false（fail-closed）。
func Allowed(pattern string) bool {
	return userAccessibleRoutes[pattern]
}
