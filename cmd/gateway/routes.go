package main

// 生产 HTTP 路由装配。main() 只负责生命周期与依赖初始化，实际暴露的
// 数据面/管理面路由由这里的 buildHandler 注册。测试也必须调用同一个
// buildHandler —— 否则端点漏注册仍可能只在线上发现。

import (
	"context"
	"io/fs"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/cn-maul/rosetta-gateway/internal/admin"
	"github.com/cn-maul/rosetta-gateway/internal/billinghttp"
	"github.com/cn-maul/rosetta-gateway/internal/config"
	"github.com/cn-maul/rosetta-gateway/internal/dataplane"
	"github.com/cn-maul/rosetta-gateway/internal/gatewayapp"
	"github.com/cn-maul/rosetta-gateway/internal/ingress"
	"github.com/cn-maul/rosetta-gateway/internal/ratelimit"
	"github.com/cn-maul/rosetta-gateway/internal/server"
	"github.com/cn-maul/rosetta-gateway/internal/snapshot"
	"github.com/cn-maul/rosetta-gateway/internal/store"
	"github.com/cn-maul/rosetta-gateway/internal/upstream"
	"github.com/cn-maul/rosetta-gateway/internal/userauth"
	"github.com/cn-maul/rosetta-gateway/internal/webui"
)

// routeDeps 是构建生产 HTTP 根入口所需的运行时依赖。
type routeDeps struct {
	db          *store.Store
	cfg         *config.Config
	logger      *slog.Logger
	masterKey   []byte
	pool        *upstream.Pool
	reloader    *runtimeReloader
	sessionMgr  *userauth.Manager
	rateLimiter *ratelimit.Limiter
	usage       *usageRecorder
}

// adminRouteSpec is the single registration source for the admin API. Public
// routes still pass through the same authz matrix but use the unauthenticated
// mux.
type adminRouteSpec struct {
	Pattern string
	Handler http.HandlerFunc
	Public  bool
}

// registeredRoute 是生产 mux 的一条注册记录。
type registeredRoute struct {
	Pattern string
	Public  bool
}

// gatewayApp 是生产路由装配的完整结果。
//
// 暴露 mux 不是为了线上直接使用，而是让测试能对**真实注册结果**做契约
// 断言：路由是否存在、实际注册模式是什么、是否挂在免鉴权 mux 上。
type gatewayApp struct {
	Handler        http.Handler
	Mux            *http.ServeMux
	AdminMux       *http.ServeMux
	PublicAdminMux *http.ServeMux
	Routes         []registeredRoute
}

// routeMux 在注册路由时顺带记录真实 pattern。
//
// 这样路由清单不需要在测试里另抄一份，也不可能与 buildApp 的注册调用
// 漂移。注意：记录的是 net/http 的 pattern（含方法），与 mux.Handler()
// 返回的注册模式一致。
type routeMux struct {
	*http.ServeMux
	public  bool
	records map[string]registeredRoute
}

func newRouteMux(public bool, records map[string]registeredRoute) *routeMux {
	return &routeMux{ServeMux: http.NewServeMux(), public: public, records: records}
}

func (m *routeMux) record(pattern string) {
	// 同一 pattern 若同时注册在受保护与免鉴权两个 mux 上，免鉴权优先：
	// 那是最宽松、也最需要被安全矩阵盯住的注册形态。
	prev, ok := m.records[pattern]
	public := m.public
	if ok {
		public = public || prev.Public
	}
	m.records[pattern] = registeredRoute{Pattern: pattern, Public: public}
}

func (m *routeMux) Handle(pattern string, h http.Handler) {
	m.record(pattern)
	m.ServeMux.Handle(pattern, h)
}

func (m *routeMux) HandleFunc(pattern string, h func(http.ResponseWriter, *http.Request)) {
	m.record(pattern)
	m.ServeMux.HandleFunc(pattern, h)
}

func registerAdminRoutes(adminMux, publicAdminMux *routeMux, specs []adminRouteSpec) {
	for _, spec := range specs {
		mux := adminMux
		if spec.Public {
			mux = publicAdminMux
		}
		mux.HandleFunc(spec.Pattern, spec.Handler)
	}
}

// buildHandler 注册生产路由并套上生产中间件。
func buildHandler(d routeDeps) http.Handler {
	return buildApp(d).Handler
}

// buildHandlerFromApp adapts the lifecycle-owned dependencies to the command
// package's routeDeps without making gatewayapp depend on cmd/gateway.
func buildHandlerFromApp(d gatewayapp.HandlerDeps) http.Handler {
	return buildHandler(routeDeps{
		db:          d.DB,
		cfg:         d.Config,
		logger:      d.Logger,
		masterKey:   d.MasterKey,
		pool:        d.Pool,
		reloader:    d.Reloader,
		sessionMgr:  d.SessionManager,
		rateLimiter: d.RateLimiter,
		usage:       d.Usage,
	})
}

// buildApp 与 buildHandler 是同一套生产装配；额外返回真实 mux 与路由清单
// 供测试校验。
func buildApp(d routeDeps) *gatewayApp {
	db, cfg, logger := d.db, d.cfg, d.logger
	masterKey, sessionMgr := d.masterKey, d.sessionMgr
	pool, reloader := d.pool, d.reloader
	rateLimiter, usage := d.rateLimiter, d.usage
	dataPlane := dataplane.NewHandler(dataplane.Deps{
		Pool: pool, Config: cfg, Usage: usage, Limiter: rateLimiter,
		Snapshot: snapshot.Get,
	})

	providerHandler := admin.NewProviderHandler(db, masterKey, cfg)
	// 出站网络（代理地址的共享清单）。provider 的下拉从它取值，
	// 管理入口与 provider 同级 —— 见 admin/network_handler.go 的注释。
	networkHandler := admin.NewNetworkHandler(db)
	credentialHandler := admin.NewCredentialHandler(db, masterKey)
	modelHandler := admin.NewModelHandler(db, masterKey, cfg)
	modelGroupHandler := admin.NewModelGroupHandler(db)
	probeHandler := admin.NewProbeHandler(db, cfg)
	routeHandler := admin.NewRouteHandler(db)
	routeTargetHandler := admin.NewRouteTargetHandler(db)
	keyHandler := admin.NewKeyHandler(db)
	statsHandler := admin.NewStatsHandler(db)
	settingsHandler := admin.NewSettingsHandler(db, cfg)
	reloadHandler := admin.NewReloadHandler(reloader.Reload)
	auditHandler := admin.NewAuditHandler(db)
	usageHandler := admin.NewUsageHandler(db, cfg)
	// 供应商 + 模型的导入导出。写在 settings 附近是因为它在界面上的入口
	// 也在「设置」页里，与设置项同一层级。
	configTransferHandler := admin.NewConfigTransferHandler(db, masterKey, cfg)
	// WithReload：禁用账号/改角色后必须立刻重建快照，否则数据面仍放行。
	userHandler := admin.NewUserHandler(db, sessionMgr).WithReload(reloader.Reload)
	records := make(map[string]registeredRoute)
	mux := newRouteMux(false, records)
	mux.HandleFunc("POST /v1/chat/completions", dataPlane.Handle(ingress.NewOpenAIChat()))
	mux.HandleFunc("POST /v1/messages", dataPlane.Handle(ingress.NewAnthropicMessages()))
	mux.HandleFunc("POST /v1/responses", dataPlane.Handle(ingress.NewResponses()))
	// D9：/v1/models 在两种协议下路径相同、响应形状不同 —— 按认证头分流，
	// 显式别名路径永远优先。
	mux.HandleFunc("GET /v1/models", handleListModels(""))
	mux.HandleFunc("GET /openai/v1/models", handleListModels("openai"))
	mux.HandleFunc("GET /anthropic/v1/models", handleListModels("anthropic"))
	// 单模型详情：外部工具（Cherry Studio / LobeChat / 各类网关面板）按
	// OpenRouter/LiteLLM 约定读 context_length / max_output_tokens。
	// 额度/费用查询（官方 usage/costs + 生态兼容 dashboard billing，见 billing.go）
	mux.HandleFunc("GET /dashboard/billing/subscription", billinghttp.Subscription(db))
	mux.HandleFunc("GET /v1/dashboard/billing/subscription", billinghttp.Subscription(db))
	mux.HandleFunc("GET /dashboard/billing/usage", billinghttp.Usage(db))
	mux.HandleFunc("GET /v1/dashboard/billing/usage", billinghttp.Usage(db))
	mux.HandleFunc("GET /v1/organization/costs", billinghttp.OrgCosts(db))
	mux.HandleFunc("GET /v1/organization/usage/completions", billinghttp.OrgUsageCompletions(db))

	mux.HandleFunc("GET /v1/models/{model}", handleGetModel(""))
	mux.HandleFunc("GET /openai/v1/models/{model}", handleGetModel("openai"))
	mux.HandleFunc("GET /anthropic/v1/models/{model}", handleGetModel("anthropic"))

	adminMux := newRouteMux(false, records)
	// 免鉴权 mux：登录与首次引导本身就是取得凭据的入口，
	// 要求先有凭据是死循环。它们的防护是限速 + 失败原因不细分 + 同源判定。
	publicAdminMux := newRouteMux(true, records)
	groupHandler := admin.NewGroupHandler(db)
	adminRouteSpecs := []adminRouteSpec{
		{Pattern: "POST /admin/api/login", Handler: userHandler.Login, Public: true},
		{Pattern: "GET /admin/api/session", Handler: userHandler.SessionStatus, Public: true},
		// 首次登录引导：没有这两个端点，新部署的 admin（空密码）永远登不进去。
		{Pattern: "GET /admin/api/bootstrap", Handler: userHandler.BootstrapStatus, Public: true},
		{Pattern: "POST /admin/api/bootstrap", Handler: userHandler.BootstrapSetup, Public: true},
		{Pattern: "GET /admin/api/providers", Handler: providerHandler.List, Public: false},
		{Pattern: "POST /admin/api/providers", Handler: providerHandler.Create, Public: false},
		{Pattern: "GET /admin/api/providers/{id}", Handler: func(w http.ResponseWriter, r *http.Request) { providerHandler.Get(w, r, r.PathValue("id")) }, Public: false},
		{Pattern: "PATCH /admin/api/providers/{id}", Handler: func(w http.ResponseWriter, r *http.Request) { providerHandler.Update(w, r, r.PathValue("id")) }, Public: false},
		{Pattern: "DELETE /admin/api/providers/{id}", Handler: func(w http.ResponseWriter, r *http.Request) { providerHandler.Delete(w, r, r.PathValue("id")) }, Public: false},
		{Pattern: "POST /admin/api/providers/{id}/test", Handler: func(w http.ResponseWriter, r *http.Request) { providerHandler.Test(w, r, r.PathValue("id")) }, Public: false},
		// 出站网络（2026-10-20）：代理地址的共享清单。直连不是一条记录，
		// 而是保留 id store.NetworkDirectID，不在此处出现。
		{Pattern: "GET /admin/api/networks", Handler: networkHandler.List, Public: false},
		{Pattern: "POST /admin/api/networks", Handler: networkHandler.Create, Public: false},
		{Pattern: "PATCH /admin/api/networks/{id}", Handler: func(w http.ResponseWriter, r *http.Request) { networkHandler.Update(w, r, r.PathValue("id")) }, Public: false},
		{Pattern: "DELETE /admin/api/networks/{id}", Handler: func(w http.ResponseWriter, r *http.Request) { networkHandler.Delete(w, r, r.PathValue("id")) }, Public: false},
		// 上游余额查询：逐条凭据打上游的余额端点（见 internal/upstream/balance.go）。
		// 纯读操作，不改任何配置，因此不触发 AutoReload（挂 reload 的写操作判据
		// 看的是方法/路径白名单，这里是 POST 却明确不参与，理由见 handler 注释）。
		{Pattern: "POST /admin/api/providers/{id}/balance", Handler: func(w http.ResponseWriter, r *http.Request) { providerHandler.Balance(w, r, r.PathValue("id")) }, Public: false},

		{Pattern: "GET /admin/api/providers/{id}/credentials", Handler: func(w http.ResponseWriter, r *http.Request) { credentialHandler.List(w, r, r.PathValue("id")) }, Public: false},
		{Pattern: "POST /admin/api/providers/{id}/credentials", Handler: func(w http.ResponseWriter, r *http.Request) { credentialHandler.Create(w, r, r.PathValue("id")) }, Public: false},
		{Pattern: "PATCH /admin/api/credentials/{id}", Handler: func(w http.ResponseWriter, r *http.Request) { credentialHandler.Update(w, r, r.PathValue("id")) }, Public: false},
		{Pattern: "DELETE /admin/api/credentials/{id}", Handler: func(w http.ResponseWriter, r *http.Request) { credentialHandler.Delete(w, r, r.PathValue("id")) }, Public: false},

		{Pattern: "GET /admin/api/providers/{id}/models", Handler: func(w http.ResponseWriter, r *http.Request) { modelHandler.List(w, r, r.PathValue("id")) }, Public: false},
		{Pattern: "POST /admin/api/providers/{id}/models", Handler: func(w http.ResponseWriter, r *http.Request) { modelHandler.Create(w, r, r.PathValue("id")) }, Public: false},
		{Pattern: "POST /admin/api/providers/{id}/models/discover", Handler: func(w http.ResponseWriter, r *http.Request) { modelHandler.Discover(w, r, r.PathValue("id")) }, Public: false},
		{Pattern: "POST /admin/api/providers/{id}/models/import", Handler: func(w http.ResponseWriter, r *http.Request) { modelHandler.ImportModels(w, r, r.PathValue("id")) }, Public: false},
		{Pattern: "PATCH /admin/api/models/{id}", Handler: func(w http.ResponseWriter, r *http.Request) { modelHandler.Update(w, r, r.PathValue("id")) }, Public: false},
		{Pattern: "DELETE /admin/api/models/{id}", Handler: func(w http.ResponseWriter, r *http.Request) { modelHandler.Delete(w, r, r.PathValue("id")) }, Public: false},
		// 批量设置价格：把同一组价格写到多个模型。POST 语义是「整体替换」，
		// 与单条 PATCH 的 nil=不改 不同 —— 见 model_handler.go 的 batchPriceRequest。
		// AutoReload 对 POST 自动生效（审计 + 快照重建），无需额外白名单。
		{Pattern: "POST /admin/api/models/batch-price", Handler: modelHandler.BatchUpdatePrices, Public: false},

		// 模型归组：把不同上游的「同一个模型」标成一组，用量统计可按组显示。
		// 纯统计/展示层概念，不参与数据面路由判定。
		{Pattern: "GET /admin/api/model-groups", Handler: modelGroupHandler.List, Public: false},
		{Pattern: "POST /admin/api/model-groups", Handler: modelGroupHandler.Create, Public: false},
		{Pattern: "PATCH /admin/api/model-groups/{id}", Handler: func(w http.ResponseWriter, r *http.Request) { modelGroupHandler.Update(w, r, r.PathValue("id")) }, Public: false},
		{Pattern: "DELETE /admin/api/model-groups/{id}", Handler: func(w http.ResponseWriter, r *http.Request) { modelGroupHandler.Delete(w, r, r.PathValue("id")) }, Public: false},
		{Pattern: "GET /admin/api/model-groups/{id}/members", Handler: func(w http.ResponseWriter, r *http.Request) { modelGroupHandler.GroupMembers(w, r, r.PathValue("id")) }, Public: false},
		// 指派/取消指派：路径里的 {id} 是**组** id（对某个组的操作）；
		// 移出组不属于任何组，所以单独一个 unassign 端点而不是带空 group_id 的指派。
		{Pattern: "POST /admin/api/model-groups/{id}/assign", Handler: func(w http.ResponseWriter, r *http.Request) { modelGroupHandler.AssignModels(w, r, r.PathValue("id")) }, Public: false},
		{Pattern: "POST /admin/api/model-groups/unassign", Handler: modelGroupHandler.UnassignModels, Public: false},

		// 上游模型健康探测（读 model_probes 表，不触发探测 —— 探测循环在后台跑）。
		{Pattern: "GET /admin/api/probes/models", Handler: probeHandler.Models, Public: false},
		{Pattern: "GET /admin/api/probes/timeline", Handler: probeHandler.Timeline, Public: false},
		// 单模型可用性探测。与 provider 的 /providers/{id}/test 是两件事：
		// 那个拉 /models 证明「endpoint + 凭据通」，这个发一次最小真实推理
		// （max_tokens=1）证明「这个 model_id 现在真的能推理」—— 模型下架、
		// 账号无权限、名字写错时前者照样绿。见 upstream.TestUpstreamModel。
		{Pattern: "POST /admin/api/models/{id}/test", Handler: func(w http.ResponseWriter, r *http.Request) { modelHandler.Test(w, r, r.PathValue("id")) }, Public: false},
		{Pattern: "GET /admin/api/upstream-models", Handler: modelHandler.ListAll, Public: false},

		{Pattern: "GET /admin/api/routes", Handler: routeHandler.List, Public: false},
		{Pattern: "GET /admin/api/route-catalog", Handler: routeHandler.Catalog, Public: false},
		{Pattern: "POST /admin/api/routes", Handler: routeHandler.Create, Public: false},
		{Pattern: "PATCH /admin/api/routes/{id}", Handler: func(w http.ResponseWriter, r *http.Request) { routeHandler.Update(w, r, r.PathValue("id")) }, Public: false},
		{Pattern: "DELETE /admin/api/routes/{id}", Handler: func(w http.ResponseWriter, r *http.Request) { routeHandler.Delete(w, r, r.PathValue("id")) }, Public: false},
		{Pattern: "GET /admin/api/routes/{id}/targets", Handler: func(w http.ResponseWriter, r *http.Request) { routeTargetHandler.List(w, r, r.PathValue("id")) }, Public: false},
		{Pattern: "PUT /admin/api/routes/{id}/targets", Handler: func(w http.ResponseWriter, r *http.Request) { routeTargetHandler.Replace(w, r, r.PathValue("id")) }, Public: false},

		{Pattern: "GET /admin/api/keys", Handler: keyHandler.List, Public: false},
		// 管理员不能建 key（2026-10 控制面/数据面分离）：普通用户自助建，
		// 管理员要去建普通用户、由那个用户自己发 key。策略在路由层拦，
		// internal/admin/key_handler.go 的 Create 保持「普通用户自助」原样。
		// **若将来另有代码直接用 keyHandler.Create 组路由，必须同样套本包装。**
		{Pattern: "POST /admin/api/keys", Handler: denyAdminKeyCreate(keyHandler.Create), Public: false},
		{Pattern: "PATCH /admin/api/keys/{id}", Handler: func(w http.ResponseWriter, r *http.Request) { keyHandler.Update(w, r, r.PathValue("id")) }, Public: false},
		{Pattern: "DELETE /admin/api/keys/{id}", Handler: func(w http.ResponseWriter, r *http.Request) { keyHandler.Delete(w, r, r.PathValue("id")) }, Public: false},
		// 重算 used_tokens：触发器维护的派生值没有自愈路径，偏高后 key 会变成死 key。
		{Pattern: "POST /admin/api/keys/{id}/recompute-usage", Handler: func(w http.ResponseWriter, r *http.Request) { keyHandler.RecomputeUsage(w, r, r.PathValue("id")) }, Public: false},

		{Pattern: "GET /admin/api/stats", Handler: statsHandler.Get, Public: false},
		{Pattern: "POST /admin/api/reload", Handler: reloadHandler.Reload, Public: false},
		{Pattern: "GET /admin/api/audit", Handler: auditHandler.List, Public: false},
		{Pattern: "GET /admin/api/usage", Handler: usageHandler.Query, Public: false},
		{Pattern: "GET /admin/api/usage/by-key", Handler: usageHandler.GroupByKey, Public: false},
		{Pattern: "GET /admin/api/usage/by-model", Handler: usageHandler.GroupByModel, Public: false},
		{Pattern: "GET /admin/api/usage/by-provider", Handler: usageHandler.GroupByProvider, Public: false},
		{Pattern: "GET /admin/api/usage/by-day", Handler: usageHandler.GroupByDay, Public: false},
		// 按用户汇总消费（钱包页「消费汇总」一表一行一个用户）。
		// **admin-only 由 handler 内的 requireAdmin 把关** —— 路径在普通用户可
		// 访问的 /admin/api/usage 前缀下，AdminGateGuard 的前缀白名单会整体放行，
		// 管不到名单内部的单个端点。而这是全站口径（每个用户的消费金额 + 用户名），
		// 普通用户拿到就等于看到同事的账单。与上面的 prune 是同一个坑。
		{Pattern: "GET /admin/api/usage/by-user", Handler: usageHandler.GroupByUser, Public: false},
		// 手动触发用量归档。admin-only 由 handler 内的 requireAdmin 把关 ——
		// 路径在普通用户可访问的 /admin/api/usage 前缀下，白名单管不到这里。
		{Pattern: "POST /admin/api/usage/prune", Handler: usageHandler.Prune, Public: false},
		{Pattern: "GET /admin/api/settings", Handler: settingsHandler.Get, Public: false},
		{Pattern: "PUT /admin/api/settings", Handler: settingsHandler.Update, Public: false},
		// 供应商 + 模型的导入导出。admin-only 由 handler 内的 requireAdmin 把关
		// —— 路径在 /admin/api/config- 前缀下，白名单管不到这里，而导出体里
		// 可能含全部上游凭据，绝不能落到普通用户手里。
		{Pattern: "POST /admin/api/config-export/export", Handler: configTransferHandler.Export, Public: false},
		{Pattern: "POST /admin/api/config-export/import", Handler: configTransferHandler.Import, Public: false},
		{Pattern: "GET /admin/api/usage/history", Handler: usageHandler.History, Public: false},
		{Pattern: "GET /admin/api/me", Handler: userHandler.Me, Public: false},
		{Pattern: "POST /admin/api/logout", Handler: userHandler.Logout, Public: false},
		{Pattern: "GET /admin/api/users", Handler: userHandler.ListUsers, Public: false},
		{Pattern: "POST /admin/api/users", Handler: userHandler.CreateUser, Public: false},
		{Pattern: "PATCH /admin/api/users/{id}", Handler: func(w http.ResponseWriter, r *http.Request) { userHandler.UpdateUser(w, r, r.PathValue("id")) }, Public: false},
		{Pattern: "DELETE /admin/api/users/{id}", Handler: func(w http.ResponseWriter, r *http.Request) { userHandler.DeleteUser(w, r, r.PathValue("id")) }, Public: false},
		{Pattern: "POST /admin/api/users/{id}/password", Handler: func(w http.ResponseWriter, r *http.Request) { userHandler.ResetPassword(w, r, r.PathValue("id")) }, Public: false},
		// 余额充值：走 adminMux 即自动要求管理员 + 记审计（AutoReload 对
		// PUT 走审计分支，字段名由 extractFieldNames 自动取，见 autoreload.go）。
		// 审计记的是**字段名**（delta_cents）而非数值 —— 这是既有审计的粒度，
		// 不在本端点内改变。余额变更不需要重建快照：预检与扣费直接读库。
		{Pattern: "PUT /admin/api/users/{id}/balance", Handler: func(w http.ResponseWriter, r *http.Request) { userHandler.AdjustBalance(w, r, r.PathValue("id")) }, Public: false},
		// 充值流水查询。**唯一一个**端点，作用域由会话身份决定（不接受参数指定
		// 查谁）：普通用户只看到自己的，管理员也只看自己的那一份 —— 钱包页是
		// 个人账本页，不存在「看全站充值」的用例。
		//
		// 路径 /admin/api/topups **不在** userAccessiblePrefixes 白名单里，所以
		// AdminGateGuard 会要求管理员 —— 而普通用户需要看自己的充值记录。所以
		// 必须把这个前缀加进白名单，作用域收窄由 handler 自己做
		// （与 key_handler / usage_handler 的 callerScope 同一责任分配：
		// 白名单只管「谁能进这个端点」，进来之后看谁由 handler 判）。
		{Pattern: "GET /admin/api/topups", Handler: userHandler.ListTopups, Public: false},
		{Pattern: "POST /admin/api/me/password", Handler: userHandler.ChangePassword, Public: false},
		// 分组与模型白名单（多用户改造 P1）。全部 admin-only ——
		// server.AdminGateGuard 是**前缀白名单**，/groups 不在其中即自动要求管理员。
		//
		// 快照重建靠外层 AutoReload，这里不再显式调 reload（见 GroupHandler 注释）。
		{Pattern: "GET /admin/api/groups", Handler: groupHandler.List, Public: false},
		{Pattern: "POST /admin/api/groups", Handler: groupHandler.Create, Public: false},
		{Pattern: "PATCH /admin/api/groups/{id}", Handler: func(w http.ResponseWriter, r *http.Request) { groupHandler.Update(w, r, r.PathValue("id")) }, Public: false},
		{Pattern: "DELETE /admin/api/groups/{id}", Handler: func(w http.ResponseWriter, r *http.Request) { groupHandler.Delete(w, r, r.PathValue("id")) }, Public: false},
		// PUT 而不是 PATCH：语义是**整体替换**白名单（勾选后保存），
		// 不是增量合并。用 PATCH 会让「取消勾选」无法表达。
		{Pattern: "PUT /admin/api/groups/{id}/models", Handler: func(w http.ResponseWriter, r *http.Request) { groupHandler.SetModels(w, r, r.PathValue("id")) }, Public: false},
		// 模型名清单：**普通用户也要能读** —— 否则 key 级白名单对他们等于不存在
		// （/admin/api/routes 是 admin-only，而自助收紧正是这个功能的目的）。
		// 读到的清单已按身份收窄，见 group_handler.ModelNames。
		{Pattern: "GET /admin/api/model-names", Handler: groupHandler.ModelNames, Public: false},
		{Pattern: "GET /admin/api/usage/history.csv", Handler: usageHandler.ExportCSV, Public: false},
	}
	registerAdminRoutes(adminMux, publicAdminMux, adminRouteSpecs)

	// 鉴权链：**只有用户会话一条通道**（users 表 + JWT）。
	//
	// 此前并行的「运维凭据」通道（admin_token / admin_auth.json）已删除。
	// 它本来的用途是「建第一个账号」与「忘记密码时应急」，而统一认证后
	// 这两件事都有更干净的位置：首次由 /admin/api/bootstrap 引导完成，
	// 忘记密码由管理员在「用户管理」里重置。留着一个绕过 users 表的
	// 管理入口，等于留一条「不产生会话、不受 auth_version 约束」的旁路。
	adminGuarded := server.NewUserAuth(sessionMgr, db).
		// adminMux 传了两次，这是刻意的：AdminGateGuard 用**同一个 mux**
		// 去查「这个请求会被哪个路由模式处理」，再拿那个模式比对普通用户
		// 白名单（见 server.userAccessibleRoutes）。复用真实路由表而不是
		// 手抄一份路径清单，是为了让白名单与真实路由**不可能漂移**。
		Guard(server.AdminGateGuard(adminMux.ServeMux, adminMux.ServeMux))

	// 管理写操作审计（DESIGN §13.3）：谁、何时、动了哪个资源、动了哪些字段
	// （只记字段名不记值 —— body 里有 api_key 与密码明文）。
	// 审计在重建之前同步落库，失败只 WARN 不阻塞。
	// actor 写死 "admin" 是已知的不精确：回调没透传会话身份，
	// 普通用户对自己资源的写操作也会落 "admin" 记录（见 store.AuditEntry 注释）。
	audit := func(method, path string, status int, remote, fields string) {
		entry := &store.AuditEntry{
			Ts:     time.Now().UnixMilli(),
			Actor:  "admin",
			Remote: remote,
			Method: method,
			Path:   path,
			Status: status,
			Fields: fields,
		}
		if err := db.CreateAuditEntry(context.Background(), entry); err != nil {
			logger.Warn("audit entry write failed", "error", err, "path", path)
		}
	}

	// 管理写操作成功后自动重建运行时（池 + 快照）：配置生效不再依赖前端自觉调
	// POST /admin/api/reload，任何带凭据的调用方（curl/脚本）写完立即生效 ——
	// 包括禁用下游 Key 这类安全敏感操作（auth 读快照，不重建就照常放行）。
	// AutoReload 在鉴权链外侧：401/429 的失败响应不会触发重建。
	// 前端 mutate() 里的 reload 调用保留为兜底（服务端重建失败时再给一次机会）。
	adminAuto := server.AutoReload(adminGuarded, reloader.Reload, audit, logger, reloader.MarkDirty)

	// 免鉴权端点必须显式注册到根 mux：它们不进 adminAuto（那会走鉴权链），
	// 但也不会因为「没注册」而落到 /admin/api/ 前缀上被鉴权拦掉 ——
	// 那样会得到 401 而不是功能缺失，症状是「登录页一直转圈」。
	// Go 1.22 的 ServeMux 按最具体模式匹配，精确路径优先于 /admin/api/ 前缀，
	// 与注册顺序无关；仍写明以免后人误改。
	mux.Handle("POST /admin/api/login", publicAdminMux)
	mux.Handle("GET /admin/api/session", publicAdminMux)
	mux.Handle("GET /admin/api/bootstrap", publicAdminMux)
	// bootstrap 是免鉴权写接口，挂在 publicAdminMux 上因而**绕过**了 adminAuto
	// 里的审计 —— 而它恰恰是整个系统最敏感的一步（设置管理员密码）。
	// 单独套 AuditOnly：只补审计，不触发快照重建（它只改密码哈希，不动快照）。
	mux.Handle("POST /admin/api/bootstrap", server.AuditOnly(publicAdminMux, audit))

	mux.Handle("GET /admin/api/", adminAuto)
	mux.Handle("POST /admin/api/", adminAuto)
	mux.Handle("PATCH /admin/api/", adminAuto)
	mux.Handle("DELETE /admin/api/", adminAuto)
	mux.Handle("PUT /admin/api/", adminAuto)

	// SPA 产物在 embed FS 的 dist/ 子目录下；剥掉 /admin 前缀后交给 FileServer。
	// hash 路由下路径只有 /admin/（入口）与 /admin/assets/*（静态资源）两类。
	webuiFS, _ := fs.Sub(webui.StaticFS, "dist")
	fileServer := http.FileServer(http.FS(webuiFS))
	webHandler := func(w http.ResponseWriter, r *http.Request) {
		// 只服务已知的几类路径，其余一律 404。虽然 http.FileServer + embed FS
		// 本身已防路径穿越，显式白名单能避免把未知路径静默回退成 index.html
		// 或暴露 dist 下意外多出的文件。
		p := strings.TrimPrefix(r.URL.Path, "/admin")
		switch {
		case p == "" || p == "/":
			// 入口页：直接读 index.html 内容并写出，**不能**把路径改写成
			// "/index.html" 再交给 http.FileServer —— 后者对任何以
			// "/index.html" 结尾的路径都会 301 重定向到 "./"（浏览器解析成
			// /admin/），于是 /admin/ → /index.html → ./ → /admin/ 无限循环，
			// 表现为「127.0.0.1 将您重定向的次数过多」。
			data, err := fs.ReadFile(webuiFS, "index.html")
			if err != nil {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			// index.html 不缓存：它引用的是带 hash 的资源文件名，网关升级后
			// 浏览器若启发式缓存了旧 index.html，会去请求已不存在的旧 hash 资源，
			// 表现为「打开管理后台白屏」。no-cache 允许缓存但强制回源校验。
			w.Header().Set("Cache-Control", "no-cache")
			w.Write(data)
		case p == "/theme-init.js":
			// 主题防闪脚本。不走 FileServer 是为了拿到明确的缓存策略 ——
			// 文件名不带 hash，内容变了必须让浏览器立刻看到。
			data, err := fs.ReadFile(webuiFS, "theme-init.js")
			if err != nil {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
			w.Header().Set("Cache-Control", "no-cache")
			w.Write(data)
		case strings.HasPrefix(p, "/assets/") && p != "/assets/" && !strings.HasSuffix(p, "/"):
			// 静态资源。**排除 "/assets/" 本身**：http.FileServer 对以 / 结尾
			// 且解析为目录的路径会生成 HTML 目录列表，把构建产物文件名全部
			// 列出来。embed FS 下无敏感文件，但白名单既然存在就把这条堵上。
			// 文件名带内容 hash，可以放心 immutable 长缓存。
			r.URL.Path = p
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			fileServer.ServeHTTP(w, r)
		default:
			http.NotFound(w, r)
		}
	}

	// 注意：Go 1.22+ 的 ServeMux 会拒绝 "GET /admin/"（路径更泛、方法更窄）
	// 与 "/admin/api/"（路径更窄、方法不限）并存——两者互不更具特异性，注册期直接 panic。
	// 因此两侧都显式声明方法：API 按方法逐个注册，静态资源只挂 GET 子树。
	// 于是 "GET /admin/api/" 在路径上严格更具体、方法相同，冲突消除。
	mux.HandleFunc("GET /admin/", webHandler)

	// 存活 / 就绪探针。**不放在 /admin/ 下**：那个路径只读 embed FS，
	// 数据库写池卡死、用量写入持续失败、快照重建长期落后时它照样返回 200，
	// 容器与编排系统因此既不重启也不告警。这里 ping 两个连接池，
	// 并把「运行时落后于数据库」这件事报成不就绪。
	//
	// 免鉴权是刻意的：探针要在没有会话、没有 token 的情况下可用。
	// 因此只返回布尔状态，不泄露任何配置或凭据。
	//
	// 错误体**不回显**（2026-10-21 修 A10-P1-4）：探针端点免鉴权，而
	// SQLite 的错误字符串可以含数据库路径、驱动内部信息、文件系统错误。
	// 同网段的主动探测者能拿它做环境枚举。现在只返回稳定的
	// error_code，完整错误只进服务端日志。
	mux.HandleFunc("GET /healthz", healthzHandler(db, reloader, logger))

	handler := server.Recovery(mux, logger)
	handler = server.WithPublicOrigin(handler, cfg.PublicOrigin)
	handler = server.Middleware(handler, logger)
	handler = server.SecurityHeaders(handler)
	handler = server.CORS(handler)
	handler = server.RequestSizeLimit(int64(cfg.Defaults.MaxRequestBodyBytes))(handler)
	// 必须包在最外层：Middleware 记请求日志、鉴权中间件与审计都通过
	// ClientIPOf 读同一个来源，转发链的信任边界只能注入一次。
	handler = server.WithTrustedProxies(handler, cfg.TrustedProxyPrefixes())

	routes := make([]registeredRoute, 0, len(records))
	for _, r := range records {
		routes = append(routes, r)
	}
	return &gatewayApp{
		Handler:        handler,
		Mux:            mux.ServeMux,
		AdminMux:       adminMux.ServeMux,
		PublicAdminMux: publicAdminMux.ServeMux,
		Routes:         routes,
	}
}
