package server

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/cn-maul/rosetta-gateway/internal/store"
	"github.com/cn-maul/rosetta-gateway/internal/userauth"
)

type ctxKey string

const (
	sessionKey ctxKey = "admin_session"
	// userKey 存*store.User，供handler 少查一次库。
	userKey ctxKey = "admin_user"
)

// SessionCookieName 是会话 cookie 名。
//
// 刻意**不用** __Host- 前缀：那要求 Secure 属性，而本项目跑在局域网 HTTP 上，
// 浏览器不会接受没有 Secure 的 __Host- cookie。改用普通名字 + SameSite=Strict。
const SessionCookieName = "rosetta_session"

// SessionTTL 与 userauth.Manager 的令牌有效期一致。
// cookie 的 MaxAge 必须与 JWT exp 对齐，否则会出现「cookie 还在但令牌已过期」
// 的窗口，前端表现为「明明有登录态却一直 401」。
const SessionTTL = 8 * time.Hour

// SessionStore 是会话中间件依赖的最小数据面（便于测试替身）。
type SessionStore interface {
	GetUser(ctx context.Context, id string) (*store.User, error)
	CountUsers(ctx context.Context) (int, error)
	TouchUserLogin(ctx context.Context, id string) error
}

// SessionFromContext 返回当前请求的会话；未登录时返回 nil。
func SessionFromContext(ctx context.Context) *userauth.Session {
	v, _ := ctx.Value(sessionKey).(*userauth.Session)
	return v
}

// UserCtxKey 是注入用户身份的 context key。
//
// 导出给测试包用：admin 包的测试需要构造「已登录」请求，
// 而 userKey 是私有的。
func UserCtxKey() any { return userKey }

// UserFromContext 返回当前请求的用户；未登录时返回 nil。
func UserFromContext(ctx context.Context) *store.User {
	v, _ := ctx.Value(userKey).(*store.User)
	return v
}

// IsAdmin 报告当前请求是否具备管理面权限。
func IsAdmin(ctx context.Context) bool {
	u := UserFromContext(ctx)
	return u != nil && u.IsAdmin()
}

// UserAuthMiddleware 保护管理后台 API：**只有用户会话一条通道**
// （users 表里的身份 + JWT 会话）。
//
// # 2026-10-06：移除了并行的「运维凭据」通道
//
// 此前有第二条通道（config 的 admin_token 或 admin_auth.json 的密码），
// 它的存在理由是「建第一个账号」与「忘记密码时应急」。统一认证后这两件事
// 都有了更合适的位置：
//
//   - 首次：GET/POST /admin/api/bootstrap（免鉴权，仅对「已建出但未设密码」
//     的那个 admin 有效，设完即失效）
//   - 忘记密码：管理员在「用户管理」里重置该用户密码（递增 auth_version，
//     旧会话一次性作废）
//
// 留着旁路通道的代价是实打实的：它绕过 users 表，因此不产生会话、
// 不受 auth_version 约束、也不留下任何可归属到具体用户的记录。
// 那是一条「谁改的」永远查不到的管理入口。
type UserAuthMiddleware struct {
	mgr      *userauth.Manager
	store    SessionStore
	throttle *FailureThrottle
}

func NewUserAuth(mgr *userauth.Manager, st SessionStore) *UserAuthMiddleware {
	return &UserAuthMiddleware{
		mgr: mgr, store: st,
		// 刻意去掉并发上限（WithoutConcurrencyCap）：会话鉴权一次 KDF 都不跑，
		// 并发上限在这里没有任何防护收益，只有误伤 —— 总览页并发 5 个请求、
		// 上限是 4，于是每个登录用户打开总览都吃一个 429
		// （2026-10-10 修复）。失败次数与冷却照常生效，撞无效令牌的防护没削弱。
		throttle: NewFailureThrottle(loginFailLimit, loginCooldown).WithoutConcurrencyCap(),
	}
}

// WithThrottle 注入自定义限速器（测试用；线上走 NewUserAuth 的默认值）。
func (a *UserAuthMiddleware) WithThrottle(th *FailureThrottle) *UserAuthMiddleware {
	a.throttle = th
	return a
}

// UserAuth 构造会话中间件并套在 next 前面。
//
// 签名是构造函数而非「中间件函数」是为了能作为一个整体挂到 mux 上。
func UserAuth(next http.Handler, mgr *userauth.Manager, st SessionStore) http.Handler {
	return NewUserAuth(mgr, st).Guard(next)
}

// Guard 把鉴权套在 next 前面。
func (a *UserAuthMiddleware) Guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.ServeHTTP(w, r, next)
	})
}

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
		u := UserFromContext(r.Context())
		// 管理员直接放行，连路由表都不必查。
		//
		// 必须同时校验 ID 非空：`IsAdmin()` 只看 Role，一个空 ID 的
		// admin 会被它判为真。而空 ID 意味着「没有真实身份」——那正是
		// 2026-10-12 统一改成 fail-closed 的状态（原 requireAdmin 在
		// internal/admin/user_admin_handler.go 曾放行它，已修复）。
		//
		// 生产中该状态不可达（userKey 唯一注入点是本文件下方 ServeHTTP 的
		// `:302`，身份来自 store.GetUser 的 SQL 查询，空 id 查不到行会被
		// `u == nil` 拦成 401），但闸门不该依赖「不可达」来维持安全。
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
			if _, pattern := routes.Handler(r); userAccessibleRoutes[pattern] {
				next.ServeHTTP(w, r)
				return
			}
		}

		writeAdminErr(w, http.StatusForbidden, "需要管理员权限")
	})
}

// ServeHTTP 实现标准中间件签名 (next 可继续传递请求)。
//
// 只有一条路径：解析会话令牌 → 查库比对 auth_version → 注入身份。
// 没有任何旁路。免鉴权的端点（登录、首次引导）挂在 publicAdminMux 上，
// 根本不经过这个中间件。
func (a *UserAuthMiddleware) ServeHTTP(w http.ResponseWriter, r *http.Request, next http.Handler) {
	// 会话密钥不可用属于部署故障。main.go 在启动时就会因取不到密钥而退出，
	// 走到这里只可能是测试构造。回 503 而不是 401：前者说「服务端没准备好」，
	// 后者说「你的凭据不对」—— 让用户去检查自己的 cookie 是白费功夫。
	if !a.mgr.Enabled() {
		writeAdminErr(w, http.StatusServiceUnavailable, "管理后台未就绪：会话密钥不可用")
		return
	}

	ip := clientIP(r)
	if !a.throttle.Allow(ip) {
		writeTooManyAttempts(w, a.throttle.RetryAfter(ip))
		return
	}
	defer a.throttle.Release(ip)

	token := sessionToken(r)
	if token == "" {
		// 无凭据请求**不计入**失败限速：它不消耗任何验证资源（连 KDF 都不跑），
		// 把它当「失败」计数只会制造一个零成本的 DoS 杠杆 —— 任何 IP 连发 10 个
		// 匿名请求就能让同一出口（NAT/公司网关）后面的**所有人**被 429 锁出一分钟，
		// 包括带着有效会话的正常用户；登录页每次探测 /me 也会计数，等于自己锁自己。
		// 限速的对象是「试图伪造凭据」：token 非空但解析/校验失败才计入。
		writeAdminErr(w, http.StatusUnauthorized, "需要登录")
		return
	}

	// 先解析令牌拿到 user id，再查库比 auth_version。
	// 顺序不能反：auth_version 在库里，不解析令牌无从得知该查谁。
	probe, err := a.mgr.Peek(token)
	if err != nil || probe == nil {
		a.throttle.Fail(ip)
		writeAdminErr(w, http.StatusUnauthorized, sessionErrMessage(err))
		return
	}

	u, err := a.store.GetUser(r.Context(), probe.UserID)
	if err != nil {
		// 数据库故障不该回 401（那是「你的凭据不对」），回 500。
		writeAdminErr(w, http.StatusInternalServerError, "服务内部错误")
		return
	}
	if u == nil {
		a.throttle.Fail(ip)
		writeAdminErr(w, http.StatusUnauthorized, "账号不存在")
		return
	}
	if !u.IsActive() {
		a.throttle.Fail(ip)
		writeAdminErr(w, http.StatusForbidden, "账号已被禁用")
		return
	}

	sess, err := a.mgr.Verify(token, u.AuthVersion)
	if err != nil {
		a.throttle.Fail(ip)
		writeAdminErr(w, http.StatusUnauthorized, sessionErrMessage(err))
		return
	}

	a.throttle.Success(ip)

	// 同源校验（2026-10-14 上线前审计加固）。
	//
	// # 为什么中间件这一层必须有，而不是继续依赖每个 handler
	//
	// 此前管理面的 CSRF 防线是**分散的**，靠两种手段凑齐：
	//   - 会话 cookie 的 SameSite=Strict（挡住绝大多数跨站请求）；
	//   - decodeJSON 强制 Content-Type: application/json（挡住 text/plain
	//     这类 CORS safelisted 简单请求）。
	//
	// 两者都成立，但它们都是**逐 handler** 才生效的：凡是不走 decodeJSON 的
	// 写端点就没有第二道门。实测 `POST /admin/api/usage/prune`、
	// `POST /admin/api/logout`、`POST /admin/api/keys/{id}/recompute-usage`
	// 这类无请求体端点带有效会话 + 跨站头即可触达 handler（返回 200）。
	//
	// # 为什么 SameSite=Strict 挡不住这一类
	//
	// Strict 只在**跨站**（registrable domain 不同）时不发 cookie。
	// 「同站不同源」——例如 gw.corp.example.com 与攻击者拿到的
	// evil.corp.example.com —— 属于 same-site，浏览器**照发 cookie**，
	// 而 origin 又确实不同。企业内网里子域可被注册/接管是常见的，
	// 所以这不是理论场景。中间件这一层补同源判定，把它彻底关掉。
	//
	// # 为什么只对 cookie 凭据生效
	//
	// Authorization: Bearer 不存在这个威胁：浏览器跨源发带该头的请求会触发
	// CORS 预检，而 /admin 从不设 Access-Control-Allow-Origin
	// （见 CORS 中间件），预检必然失败。非浏览器客户端（curl / SDK）同理
	// 不受同源策略约束。所以只拦「凭 cookie 进来的跨站写请求」，
	// 不影响脚本化运维这条通道。
	// 读方法一律放过：GET/HEAD/OPTIONS 按语义必须无副作用，且管理面的
	// 跨源读取本来就由「不设 CORS 头」在浏览器侧拦住。只有写方法需要这层。
	if !safeMethods[r.Method] && cookieAuthed(r) && !sameOrigin(r) {
		writeForbidden(w)
		return
	}

	ctx := context.WithValue(r.Context(), sessionKey, sess)
	ctx = context.WithValue(ctx, userKey, u)
	next.ServeHTTP(w, r.WithContext(ctx))
}

// safeMethods 是 CSRF 不构成威胁的 HTTP 方法：它们按语义必须是「只读」。
var safeMethods = map[string]bool{
	http.MethodGet:     true,
	http.MethodHead:    true,
	http.MethodOptions: true,
}

// cookieAuthed 报告本次会话凭据是否来自 Cookie（而非 Authorization 头）。
func cookieAuthed(r *http.Request) bool {
	c, err := r.Cookie(SessionCookieName)
	return err == nil && c.Value != ""
}

func sessionErrMessage(err error) string {
	switch {
	case errors.Is(err, userauth.ErrTokenExpired):
		return "登录已过期，请重新登录"
	case errors.Is(err, userauth.ErrStaleVersion):
		return "凭据已失效（密码或账号状态有变更），请重新登录"
	default:
		return "需要登录"
	}
}

// sessionToken 从请求里取令牌：Cookie 优先，其次 Authorization 头。
//
// 支持 Authorization 是为了让 curl / SDK 能直接用 Bearer 调管理 API
// （脚本化运维、冒烟测试都需要）。浏览器走 Cookie。
func sessionToken(r *http.Request) string {
	if c, err := r.Cookie(SessionCookieName); err == nil && c.Value != "" {
		return c.Value
	}
	return bearerToken(r)
}

// SetSessionCookie 写出会话 cookie。
func SetSessionCookie(w http.ResponseWriter, token string, exp time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    token,
		Path:     "/admin",
		Expires:  exp,
		MaxAge:   int(time.Until(exp).Seconds()),
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		// 刻意不设 Secure：局域网是 HTTP，设了浏览器直接丢弃 cookie。
		// 这是「不上 HTTPS」的已知代价，§6.1 已记录。
	})
}

// ClearSessionCookie 清除会话 cookie。
//
// 路径与写入时一致，否则浏览器会当成另一个 cookie 留着不清。
func ClearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    "",
		Path:     "/admin",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	})
}

// TouchLogin 更新最后登录时间。失败只记日志 —— 它是辅助信息，
// 不该让登录失败。
func TouchLogin(st SessionStore, ctx context.Context, userID string) {
	if userID == "" {
		return
	}
	if err := st.TouchUserLogin(ctx, userID); err != nil {
		slog.Warn("admin api: touch login failed", "user_id", userID, "error", err)
	}
}

func writeAdminErr(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]string{"message": message, "type": "auth_error"},
	})
}
