package main

import (
	"context"
	crand "crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"math"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/cn-maul/rosetta"
	"github.com/cn-maul/rosetta-gateway/internal/admin"
	"github.com/cn-maul/rosetta-gateway/internal/auth"
	"github.com/cn-maul/rosetta-gateway/internal/config"
	"github.com/cn-maul/rosetta-gateway/internal/crypto"
	"github.com/cn-maul/rosetta-gateway/internal/effort"
	"github.com/cn-maul/rosetta-gateway/internal/inwire"
	"github.com/cn-maul/rosetta-gateway/internal/outwire"
	"github.com/cn-maul/rosetta-gateway/internal/ratelimit"
	"github.com/cn-maul/rosetta-gateway/internal/routing"
	"github.com/cn-maul/rosetta-gateway/internal/server"
	"github.com/cn-maul/rosetta-gateway/internal/snapshot"
	"github.com/cn-maul/rosetta-gateway/internal/store"
	"github.com/cn-maul/rosetta-gateway/internal/upstream"
	"github.com/cn-maul/rosetta-gateway/internal/userauth"
	"github.com/cn-maul/rosetta-gateway/internal/webui"
)

// homeEnvVar 是状态根目录的环境变量名。
//
// 容器镜像靠它把 config.json、master.key、session_secret 与数据库整体
// 指到挂载卷上，让镜像层保持无状态（见 DOCKER.md）。
const homeEnvVar = "ROSETTA_GW_HOME"

// secretSource 报告当前用的是哪一把会话密钥，只用于启动日志。
//
// 值得单独说一句：密钥**从哪里来**必须可见。曾经的口令是静默回退的 ——
// 环境变量没配就退回 admin_token，而那条路径在多用户模式下根本不通，
// 于是「配好了」与「没配好」在日志里长得一模一样。
func secretSource(path string) string {
	if v := strings.TrimSpace(os.Getenv(userauth.SecretEnvName)); v != "" {
		return userauth.SecretEnvName + "（环境变量）"
	}
	return path + "（自动生成并持久化）"
}

// buildVersion 由构建期注入：-ldflags "-X main.buildVersion=x.y.z"。
// 未注入时为 "dev"。取值与前端页脚的 __APP_VERSION__ 同源
// —— 都来自 web/package.json 的 version，由 CI 打镜像时统一传入。
var buildVersion = "dev"

// resolveHome 返回状态根目录。
//
// 默认取可执行文件所在目录：这样无论当前工作目录是什么，都读同一份 config.json、
// 命中同一个数据库 —— 状态跟着二进制走，而不是跟着 CWD 漂。
// homeEnvVar 非空时改用它，用于把状态整体重定向到挂载卷。
func resolveHome(exeDir string) string {
	override := strings.TrimSpace(os.Getenv(homeEnvVar))
	if override == "" {
		return exeDir
	}
	if abs, err := filepath.Abs(override); err == nil {
		return abs
	}
	return override
}

// slogLevel 把 config.log_level 映射成 slog 级别。
// 非法值在 config.validate 里已被挡掉，这里的 default 只是兜底。
func slogLevel(name string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

func main() {
	// 日志级别要等 config 读出来才知道，但「读 config 失败」这件事本身也得有日志 ——
	// 用 LevelVar 先占位、拿到配置后再调，比造两个 logger 干净。
	logLevel := new(slog.LevelVar)
	logLevel.Set(slog.LevelInfo)
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: logLevel,
	}))
	// 让 internal/admin 这类不方便注入 logger 的包直接 slog.Error 也能落在
	// 同一个 handler 上（writeServerError 就是这么写的）。
	slog.SetDefault(logger)

	exePath, err := os.Executable()
	if err != nil {
		logger.Error("failed to resolve executable path", "error", err)
		os.Exit(1)
	}
	homeDir := resolveHome(filepath.Dir(exePath))

	logger.Info("rosetta-gateway starting", "version", buildVersion, "home_dir", homeDir)

	configPath := filepath.Join(homeDir, "config.json")
	cfg, generated, err := config.LoadOrGenerate(configPath)
	if err != nil {
		logger.Error("failed to load config", "error", err)
		os.Exit(1)
	}
	if generated {
		logger.Info("no config found, generated default", "path", configPath)
	}

	if !filepath.IsAbs(cfg.DBPath) {
		cfg.DBPath = filepath.Join(homeDir, cfg.DBPath)
	}

	logLevel.Set(slogLevel(cfg.LogLevel))
	logger.Info("config loaded", "listen", cfg.Listen, "db_path", cfg.DBPath, "log_level", cfg.LogLevel)

	// 时区必须在**打开数据库之前**应用（2026-10-09 修复的线上缺陷）。
	//
	// 按天分桶的日界是 SQLite 的 'localtime'（store.dayExpr），它读的是
	// Go 的 time.Local；容器里没设 TZ 时那是 UTC，北京时间 0~8 点的调用
	// 全被算进「昨天」。改写 time.Local 必须早于 store.Open —— 归档水位
	// 与每日剪枝都拿「今天是哪天」当切分点，晚了它们已经用旧口径算过一轮。
	//
	// 顺带打印生效时区：这个值决定所有按天数字的日界，必须让运维一眼
	// 可查（看到 UTC 而用户都在东八区，就知道该配 timezone / TZ 了）。
	appliedTZ, err := config.ApplyTimezone(cfg.Timezone)
	if err != nil {
		logger.Error("invalid timezone in config", "error", err)
		os.Exit(1)
	}
	logger.Info("daily-bucket timezone applied", "timezone", appliedTZ,
		"note", "按天统计的日界由此时区决定；容器部署若与用户所在时区不符，请在 config.json 配 timezone 或设 TZ 环境变量")

	// 端口自检：落在浏览器保留端口（6666 / 6000 / 10080 …）上时，
	// 浏览器根本不会发出请求，服务端**没有任何日志**，前端只显示 ERR_UNSAFE_PORT。
	// 这种「完全静默」的失败模式排查成本极高，所以在启动这一步就喊出来。
	if port, service, blocked := config.CheckListenPort(cfg.Listen); blocked {
		logger.Warn("监听端口被浏览器保留，管理界面将无法在浏览器中打开",
			"listen", cfg.Listen,
			"port", port,
			"reserved_for", service,
			"browser_error", "ERR_UNSAFE_PORT",
			"hint", "改用黑名单外的端口（如 8666）；容器里也可把宿主端口映射成 8666",
		)
	}

	masterKey, generatedKey, weakKey, err := crypto.LoadMasterKey(cfg.MasterKeyEnv, homeDir)
	switch {
	case err != nil:
		// 这里绝不能降级成「无主密钥继续跑」：encryptSecret 在没有密钥时会把
		// 上游 API key **明文**写进数据库，而 DecryptWithFallback 之后又一直按
		// 明文接受 —— 一次瞬态失败（目录不存在/不可写/权限异常）就换来一批
		// 不可逆的明文凭据，且没有任何补救路径。宁可拒绝启动。
		logger.Error("master key unavailable, refusing to start", "error", err,
			"hint", "确认状态目录可写，或设置 "+cfg.MasterKeyEnv)
		os.Exit(1)
	case generatedKey:
		logger.Info("generated master key", "path", filepath.Join(homeDir, crypto.KeyFileName))
	case weakKey:
		logger.Warn("master key material is short (<32 chars) and looks like a passphrase; " +
			"upstream API keys encrypted with it are brute-forceable offline. " +
			"Recommended: delete the weak key, restart to auto-generate a random master.key, then re-save every credential")
	}

	db, err := store.Open(cfg.DBPath, logger)
	if err != nil {
		logger.Error("failed to open database", "error", err)
		os.Exit(1)
	}
	defer db.Close()

	bootstrapDB(db, cfg, logger, masterKey)

	pool := upstream.NewPool(logger)
	// 冷却状态必须落库，否则任何 admin 写操作触发的池重建都会把刚被判坏的
	// 凭据立刻放回轮换。
	pool.SetCooldownStore(db)
	// logger 必须传：Reload 在「有 provider 未就绪」时要留 WARN。
	// 漏掉它不会在启动时报错，只会在**第一次出现凭据故障时** panic ——
	// 恰巧是最需要那条日志的时刻。
	reloader := &runtimeReloader{db: db, masterKey: masterKey, pool: pool, cfg: cfg, logger: logger}
	if err := reloader.Reload(context.Background()); err != nil {
		logger.Warn("failed to build runtime from DB, falling back to config", "error", err)
		if err := pool.BuildFromConfig(cfg); err != nil {
			logger.Error("failed to build upstream pool from config", "error", err)
		}
		snapshot.Init(buildSnapshotFromConfig(cfg))
	}

	// 后台重建重试：任一 admin 写操作的重建失败后置脏，这里负责兜底收敛。
	//
	// shutdown 通道在这里声明（原先在下方 500 行处）：重试循环必须受进程
	// 生命周期约束，否则主流程 return 后该goroutine 仍在跑，且它持有
	// rr.mu —— 那会让进程退出时卡在一次正在进行的重建上。
	// 下方原声明处改为复用（不重复 make）。
	shutdown := make(chan struct{})

	reloadCtx, stopReloadRetry := context.WithCancel(context.Background())
	defer stopReloadRetry()
	go reloader.watchReloadRetry(reloadCtx)

	usage := newUsageRecorder(db, logger)
	mux := http.NewServeMux()
	rateLimiter := ratelimit.New()
	mux.HandleFunc("POST /v1/chat/completions", handleIngress(pool, cfg, usage, rateLimiter, openaiChatCodec{}))
	mux.HandleFunc("POST /v1/messages", handleIngress(pool, cfg, usage, rateLimiter, anthropicMessagesCodec{}))
	mux.HandleFunc("POST /v1/responses", handleIngress(pool, cfg, usage, rateLimiter, openaiResponsesCodec{}))
	// D9：/v1/models 在两种协议下路径相同、响应形状不同 —— 按认证头分流，
	// 显式别名路径永远优先。
	mux.HandleFunc("GET /v1/models", handleListModels(""))
	mux.HandleFunc("GET /openai/v1/models", handleListModels("openai"))
	mux.HandleFunc("GET /anthropic/v1/models", handleListModels("anthropic"))
	// 单模型详情：外部工具（Cherry Studio / LobeChat / 各类网关面板）按
	// OpenRouter/LiteLLM 约定读 context_length / max_output_tokens。
	// 额度/费用查询（官方 usage/costs + 生态兼容 dashboard billing，见 billing.go）
	mux.HandleFunc("GET /dashboard/billing/subscription", billingSubscription(db))
	mux.HandleFunc("GET /v1/dashboard/billing/subscription", billingSubscription(db))
	mux.HandleFunc("GET /dashboard/billing/usage", billingUsage(db))
	mux.HandleFunc("GET /v1/dashboard/billing/usage", billingUsage(db))
	mux.HandleFunc("GET /v1/organization/costs", orgCosts(db))
	mux.HandleFunc("GET /v1/organization/usage/completions", orgUsageCompletions(db))

	mux.HandleFunc("GET /v1/models/{model}", handleGetModel(""))
	mux.HandleFunc("GET /openai/v1/models/{model}", handleGetModel("openai"))
	mux.HandleFunc("GET /anthropic/v1/models/{model}", handleGetModel("anthropic"))

	// 管理端身份：**只有一种来源 —— users 表**。
	//
	// 2026-10-06 起移除了两条并行的运维凭据通道：
	//   - config.json 的 admin_token / ADMIN_TOKEN 环境变量
	//   - 可执行文件同级的 admin_auth.json（internal/adminauth）
	//
	// 移除理由不是「少一个功能」，而是它们制造了一个**死锁**：启动时
	// ensureBootstrapAdmin 建的 admin 账号密码为空（登不进去），
	// 而 admin_token 曾被实现成「users 表为空才放行」的一次性窗口 ——
	// users 永远非空，于是两条路都堵死，没有任何途径设置第一个密码。
	// MULTIUSER.md §3.5 记了这个坑；中间方案的「并行通道始终有效」
	// 仍是绕过症状，真正的收口是让 users 表成为唯一身份来源，
	// 并给它一个自洽的首次登录引导（见 ensureBootstrapAdmin 的注释）。
	//
	// 代价要说清楚：**删库 = 失明**。这是明确的取舍 ——
	// 「可随手删掉的数据文件里存身份」正是 adminauth 当初存在的原因，
	// 而多用户已经把身份、配额、路由、key 全部绑在同一个库上，
	// 为身份单独造一套抗删除存储带来的复杂度远超收益。

	// 启动期一次性操作，与任何请求无关，故用 Background 而非请求 ctx。
	ensureBootstrapAdmin(context.Background(), db, logger)

	// 首次初始化窗口对所有能连到端口的人开放（POST /admin/api/bootstrap
	// 免鉴权，直到管理员设完密码为止）。默认 listen 绑回环，只有本机能碰；
	// 运维把它改成 0.0.0.0 是很常见的做法，那时整个局域网都能抢先
	// 成为第一个管理员。这个告警不能省 —— 它是唯一提醒。
	pending, err := db.FindUninitializedAdmin(context.Background())
	if err != nil {
		logger.Error("cannot read bootstrap state; admin UI may be unreachable", "error", err)
	} else if pending != nil && !isLoopbackListen(cfg.Listen) {
		logger.Error("SECURITY: admin password not yet set AND listening on a non-loopback address — "+
			"anyone who can reach this port can become the first administrator",
			"listen", cfg.Listen,
			"immediate_action", "立即在浏览器打开 /admin/ 完成首次设置密码；"+
				"在此之前把 listen 改回 127.0.0.1")
	} else if pending != nil {
		logger.Warn("admin password not set yet; open the admin UI to complete first-run setup",
			"note", "监听地址是回环，仅本机可访问；一旦改为 0.0.0.0 暴露到网络，请立即完成设置")
	}

	providerHandler := admin.NewProviderHandler(db, masterKey, cfg)
	credentialHandler := admin.NewCredentialHandler(db, masterKey)
	modelHandler := admin.NewModelHandler(db, masterKey, cfg)
	routeHandler := admin.NewRouteHandler(db)
	routeTargetHandler := admin.NewRouteTargetHandler(db)
	keyHandler := admin.NewKeyHandler(db)
	statsHandler := admin.NewStatsHandler(db)
	settingsHandler := admin.NewSettingsHandler(db, cfg)
	reloadHandler := admin.NewReloadHandler(reloader.Reload)
	auditHandler := admin.NewAuditHandler(db)
	usageHandler := admin.NewUsageHandler(db)
	// 供应商 + 模型的导入导出。写在 settings 附近是因为它在界面上的入口
	// 也在「设置」页里，与设置项同一层级。
	configTransferHandler := admin.NewConfigTransferHandler(db, masterKey, cfg)
	// 会话签名密钥。**不再要求运维配环境变量**：没配就从
	// <homeDir>/session_secret 读，文件也没有就自动生成并原子落盘。
	//
	// 为什么必须持久化而不是每次启动现生成：那样每次重启都会换一把密钥，
	// 所有人的会话在下一次重启后集体失效，症状是「莫名其妙被登出」，
	// 且没有任何错误信息。放在 homeDir 而不是数据库，理由同 master.key：
	// 它是身份的一部分，删库不该把所有人踢下线。
	secretPath := filepath.Join(homeDir, userauth.SecretFileName)
	sessionMgr, err := userauth.NewManager(secretPath)
	if err != nil {
		// 密钥配得太短、文件不可读、生成失败都属于部署错误：
		// 直接拒绝启动，否则会让人以为配好了，实际所有登录都被挡在门外
		// 却查不到原因。
		logger.Error("session secret unusable; refusing to start", "error", err)
		os.Exit(1)
	}
	if sessionMgr.Enabled() {
		logger.Info("user sessions enabled", "ttl", sessionMgr.TTL(),
			"source", secretSource(secretPath))
	} else {
		// 理论不可达：secretPath 非空时 NewManager 要么启用、要么报错退出。
		// 留这条分支只为不把 nil 当成正常状态往下传。
		logger.Error("user sessions unavailable; refusing to start",
			"reason", "session secret could not be loaded or generated")
		os.Exit(1)
	}
	// WithReload：禁用账号/改角色后必须立刻重建快照，否则数据面仍放行。
	userHandler := admin.NewUserHandler(db, sessionMgr).WithReload(reloader.Reload)

	adminMux := http.NewServeMux()
	// 免鉴权 mux：登录与首次引导本身就是取得凭据的入口，
	// 要求先有凭据是死循环。它们的防护是限速 + 失败原因不细分 + 同源判定。
	publicAdminMux := http.NewServeMux()
	publicAdminMux.HandleFunc("POST /admin/api/login", userHandler.Login)
	publicAdminMux.HandleFunc("GET /admin/api/session", userHandler.SessionStatus)
	// 首次登录引导：没有这两个端点，新部署的 admin（空密码）永远登不进去。
	publicAdminMux.HandleFunc("GET /admin/api/bootstrap", userHandler.BootstrapStatus)
	publicAdminMux.HandleFunc("POST /admin/api/bootstrap", userHandler.BootstrapSetup)
	adminMux.HandleFunc("GET /admin/api/providers", providerHandler.List)
	adminMux.HandleFunc("POST /admin/api/providers", providerHandler.Create)
	adminMux.HandleFunc("GET /admin/api/providers/{id}", func(w http.ResponseWriter, r *http.Request) { providerHandler.Get(w, r, r.PathValue("id")) })
	adminMux.HandleFunc("PATCH /admin/api/providers/{id}", func(w http.ResponseWriter, r *http.Request) { providerHandler.Update(w, r, r.PathValue("id")) })
	adminMux.HandleFunc("DELETE /admin/api/providers/{id}", func(w http.ResponseWriter, r *http.Request) { providerHandler.Delete(w, r, r.PathValue("id")) })
	adminMux.HandleFunc("POST /admin/api/providers/{id}/test", func(w http.ResponseWriter, r *http.Request) { providerHandler.Test(w, r, r.PathValue("id")) })
	// 上游余额查询：逐条凭据打上游的余额端点（见 internal/upstream/balance.go）。
	// 纯读操作，不改任何配置，因此不触发 AutoReload（挂 reload 的写操作判据
	// 看的是方法/路径白名单，这里是 POST 却明确不参与，理由见 handler 注释）。
	adminMux.HandleFunc("POST /admin/api/providers/{id}/balance", func(w http.ResponseWriter, r *http.Request) { providerHandler.Balance(w, r, r.PathValue("id")) })

	adminMux.HandleFunc("GET /admin/api/providers/{id}/credentials", func(w http.ResponseWriter, r *http.Request) { credentialHandler.List(w, r, r.PathValue("id")) })
	adminMux.HandleFunc("POST /admin/api/providers/{id}/credentials", func(w http.ResponseWriter, r *http.Request) { credentialHandler.Create(w, r, r.PathValue("id")) })
	adminMux.HandleFunc("PATCH /admin/api/credentials/{id}", func(w http.ResponseWriter, r *http.Request) { credentialHandler.Update(w, r, r.PathValue("id")) })
	adminMux.HandleFunc("DELETE /admin/api/credentials/{id}", func(w http.ResponseWriter, r *http.Request) { credentialHandler.Delete(w, r, r.PathValue("id")) })

	adminMux.HandleFunc("GET /admin/api/providers/{id}/models", func(w http.ResponseWriter, r *http.Request) { modelHandler.List(w, r, r.PathValue("id")) })
	adminMux.HandleFunc("POST /admin/api/providers/{id}/models", func(w http.ResponseWriter, r *http.Request) { modelHandler.Create(w, r, r.PathValue("id")) })
	adminMux.HandleFunc("POST /admin/api/providers/{id}/models/discover", func(w http.ResponseWriter, r *http.Request) { modelHandler.Discover(w, r, r.PathValue("id")) })
	adminMux.HandleFunc("POST /admin/api/providers/{id}/models/import", func(w http.ResponseWriter, r *http.Request) { modelHandler.ImportModels(w, r, r.PathValue("id")) })
	adminMux.HandleFunc("PATCH /admin/api/models/{id}", func(w http.ResponseWriter, r *http.Request) { modelHandler.Update(w, r, r.PathValue("id")) })
	adminMux.HandleFunc("DELETE /admin/api/models/{id}", func(w http.ResponseWriter, r *http.Request) { modelHandler.Delete(w, r, r.PathValue("id")) })
	// 单模型可用性探测。与 provider 的 /providers/{id}/test 是两件事：
	// 那个拉 /models 证明「endpoint + 凭据通」，这个发一次最小真实推理
	// （max_tokens=1）证明「这个 model_id 现在真的能推理」—— 模型下架、
	// 账号无权限、名字写错时前者照样绿。见 upstream.TestUpstreamModel。
	adminMux.HandleFunc("POST /admin/api/models/{id}/test", func(w http.ResponseWriter, r *http.Request) { modelHandler.Test(w, r, r.PathValue("id")) })
	adminMux.HandleFunc("GET /admin/api/upstream-models", modelHandler.ListAll)

	adminMux.HandleFunc("GET /admin/api/routes", routeHandler.List)
	adminMux.HandleFunc("POST /admin/api/routes", routeHandler.Create)
	adminMux.HandleFunc("PATCH /admin/api/routes/{id}", func(w http.ResponseWriter, r *http.Request) { routeHandler.Update(w, r, r.PathValue("id")) })
	adminMux.HandleFunc("DELETE /admin/api/routes/{id}", func(w http.ResponseWriter, r *http.Request) { routeHandler.Delete(w, r, r.PathValue("id")) })
	adminMux.HandleFunc("GET /admin/api/routes/{id}/targets", func(w http.ResponseWriter, r *http.Request) { routeTargetHandler.List(w, r, r.PathValue("id")) })
	adminMux.HandleFunc("PUT /admin/api/routes/{id}/targets", func(w http.ResponseWriter, r *http.Request) { routeTargetHandler.Replace(w, r, r.PathValue("id")) })

	adminMux.HandleFunc("GET /admin/api/keys", keyHandler.List)
	// 管理员不能建 key（2026-10 控制面/数据面分离）：普通用户自助建，
	// 管理员要去建普通用户、由那个用户自己发 key。策略在路由层拦，
	// internal/admin/key_handler.go 的 Create 保持「普通用户自助」原样。
	// **若将来另有代码直接用 keyHandler.Create 组路由，必须同样套本包装。**
	adminMux.HandleFunc("POST /admin/api/keys", denyAdminKeyCreate(keyHandler.Create))
	adminMux.HandleFunc("PATCH /admin/api/keys/{id}", func(w http.ResponseWriter, r *http.Request) { keyHandler.Update(w, r, r.PathValue("id")) })
	adminMux.HandleFunc("DELETE /admin/api/keys/{id}", func(w http.ResponseWriter, r *http.Request) { keyHandler.Delete(w, r, r.PathValue("id")) })
	// 重算 used_tokens：触发器维护的派生值没有自愈路径，偏高后 key 会变成死 key。
	adminMux.HandleFunc("POST /admin/api/keys/{id}/recompute-usage", func(w http.ResponseWriter, r *http.Request) { keyHandler.RecomputeUsage(w, r, r.PathValue("id")) })

	adminMux.HandleFunc("GET /admin/api/stats", statsHandler.Get)
	adminMux.HandleFunc("POST /admin/api/reload", reloadHandler.Reload)
	adminMux.HandleFunc("GET /admin/api/audit", auditHandler.List)
	adminMux.HandleFunc("GET /admin/api/usage", usageHandler.Query)
	adminMux.HandleFunc("GET /admin/api/usage/by-key", usageHandler.GroupByKey)
	adminMux.HandleFunc("GET /admin/api/usage/by-model", usageHandler.GroupByModel)
	adminMux.HandleFunc("GET /admin/api/usage/by-provider", usageHandler.GroupByProvider)
	adminMux.HandleFunc("GET /admin/api/usage/by-day", usageHandler.GroupByDay)
	// 按用户汇总消费（钱包页「消费汇总」一表一行一个用户）。
	// **admin-only 由 handler 内的 requireAdmin 把关** —— 路径在普通用户可
	// 访问的 /admin/api/usage 前缀下，AdminGateGuard 的前缀白名单会整体放行，
	// 管不到名单内部的单个端点。而这是全站口径（每个用户的消费金额 + 用户名），
	// 普通用户拿到就等于看到同事的账单。与上面的 prune 是同一个坑。
	adminMux.HandleFunc("GET /admin/api/usage/by-user", usageHandler.GroupByUser)
	// 手动触发用量归档。admin-only 由 handler 内的 requireAdmin 把关 ——
	// 路径在普通用户可访问的 /admin/api/usage 前缀下，白名单管不到这里。
	adminMux.HandleFunc("POST /admin/api/usage/prune", usageHandler.Prune)
	adminMux.HandleFunc("GET /admin/api/settings", settingsHandler.Get)
	adminMux.HandleFunc("PUT /admin/api/settings", settingsHandler.Update)
	// 供应商 + 模型的导入导出。admin-only 由 handler 内的 requireAdmin 把关
	// —— 路径在 /admin/api/config- 前缀下，白名单管不到这里，而导出体里
	// 可能含全部上游凭据，绝不能落到普通用户手里。
	adminMux.HandleFunc("POST /admin/api/config-export/export", configTransferHandler.Export)
	adminMux.HandleFunc("POST /admin/api/config-export/import", configTransferHandler.Import)
	adminMux.HandleFunc("GET /admin/api/usage/history", usageHandler.History)
	adminMux.HandleFunc("GET /admin/api/me", userHandler.Me)
	adminMux.HandleFunc("POST /admin/api/logout", userHandler.Logout)
	adminMux.HandleFunc("GET /admin/api/users", userHandler.ListUsers)
	adminMux.HandleFunc("POST /admin/api/users", userHandler.CreateUser)
	adminMux.HandleFunc("PATCH /admin/api/users/{id}", func(w http.ResponseWriter, r *http.Request) { userHandler.UpdateUser(w, r, r.PathValue("id")) })
	adminMux.HandleFunc("DELETE /admin/api/users/{id}", func(w http.ResponseWriter, r *http.Request) { userHandler.DeleteUser(w, r, r.PathValue("id")) })
	adminMux.HandleFunc("POST /admin/api/users/{id}/password", func(w http.ResponseWriter, r *http.Request) { userHandler.ResetPassword(w, r, r.PathValue("id")) })
	// 余额充值：走 adminMux 即自动要求管理员 + 记审计（AutoReload 对
	// PUT 走审计分支，字段名由 extractFieldNames 自动取，见 autoreload.go）。
	// 审计记的是**字段名**（delta_cents）而非数值 —— 这是既有审计的粒度，
	// 不在本端点内改变。余额变更不需要重建快照：预检与扣费直接读库。
	adminMux.HandleFunc("PUT /admin/api/users/{id}/balance", func(w http.ResponseWriter, r *http.Request) { userHandler.AdjustBalance(w, r, r.PathValue("id")) })
	// 充值流水查询。**唯一一个**端点，作用域由会话身份决定（不接受参数指定
	// 查谁）：普通用户只看到自己的，管理员也只看自己的那一份 —— 钱包页是
	// 个人账本页，不存在「看全站充值」的用例。
	//
	// 路径 /admin/api/topups **不在** userAccessiblePrefixes 白名单里，所以
	// AdminGateGuard 会要求管理员 —— 而普通用户需要看自己的充值记录。所以
	// 必须把这个前缀加进白名单，作用域收窄由 handler 自己做
	// （与 key_handler / usage_handler 的 callerScope 同一责任分配：
	// 白名单只管「谁能进这个端点」，进来之后看谁由 handler 判）。
	adminMux.HandleFunc("GET /admin/api/topups", userHandler.ListTopups)
	adminMux.HandleFunc("POST /admin/api/me/password", userHandler.ChangePassword)
	// 分组与模型白名单（多用户改造 P1）。全部 admin-only ——
	// server.AdminGateGuard 是**前缀白名单**，/groups 不在其中即自动要求管理员。
	//
	// 快照重建靠外层 AutoReload，这里不再显式调 reload（见 GroupHandler 注释）。
	groupHandler := admin.NewGroupHandler(db)
	adminMux.HandleFunc("GET /admin/api/groups", groupHandler.List)
	adminMux.HandleFunc("POST /admin/api/groups", groupHandler.Create)
	adminMux.HandleFunc("PATCH /admin/api/groups/{id}", func(w http.ResponseWriter, r *http.Request) { groupHandler.Update(w, r, r.PathValue("id")) })
	adminMux.HandleFunc("DELETE /admin/api/groups/{id}", func(w http.ResponseWriter, r *http.Request) { groupHandler.Delete(w, r, r.PathValue("id")) })
	// PUT 而不是 PATCH：语义是**整体替换**白名单（勾选后保存），
	// 不是增量合并。用 PATCH 会让「取消勾选」无法表达。
	adminMux.HandleFunc("PUT /admin/api/groups/{id}/models", func(w http.ResponseWriter, r *http.Request) { groupHandler.SetModels(w, r, r.PathValue("id")) })
	// 模型名清单：**普通用户也要能读** —— 否则 key 级白名单对他们等于不存在
	// （/admin/api/routes 是 admin-only，而自助收紧正是这个功能的目的）。
	// 读到的清单已按身份收窄，见 group_handler.ModelNames。
	adminMux.HandleFunc("GET /admin/api/model-names", groupHandler.ModelNames)
	adminMux.HandleFunc("GET /admin/api/usage/history.csv", usageHandler.ExportCSV)

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
		Guard(server.AdminGateGuard(adminMux, adminMux))

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

	handler := server.Recovery(mux, logger)
	handler = server.Middleware(handler, logger)
	handler = server.SecurityHeaders(handler)
	handler = server.CORS(handler)
	handler = server.RequestSizeLimit(int64(cfg.Defaults.MaxRequestBodyBytes))(handler)

	srv := &http.Server{
		Addr:        cfg.Listen,
		Handler:     handler,
		ReadTimeout: 30 * time.Second,
		// WriteTimeout 必须为 0：net/http 的写超时从「开始写响应」起算，覆盖整个
		// 响应时长 —— 5 分钟一到会把仍在正常吐字的流式响应硬切（大输出的慢推理
		// 模型恰好会撞上），下游看到的是来历不明的 truncated。流的生命周期已由
		// TTFT/空闲看门狗约束（attemptStream），非流式由 upstream_timeout 限定
		// handler 时长，全局写超时在这里只会误伤长流，不会多保护任何东西。
		WriteTimeout: 0,
		// 慢连接防护：30s 读不完请求头、空闲 2 分钟的 keep-alive 连接即回收。
		// IdleTimeout 不设的话回落 ReadTimeout(30s)，聊天客户端「想一会儿再发
		// 下一条」的间隔内连接被反复重建，复用率反而更低。
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}

	go func() {
		logger.Info("server starting", "addr", cfg.Listen)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("server error", "error", err)
			os.Exit(1)
		}
	}()

	// 用量归档的每日定时剪枝（设计 §4.8：明细 30 天，累计永久）。
	// （shutdown 通道已在 reloader 构造处声明，供此处与后台重建重试共用。）
	//
	// 启动后**立刻先跑一次**再进 24h 循环：只等第一个 tick 的话，长期停机后
	// 重启的实例要等满 24h 才开始剪，而这段时间里 usage_records 还在接收新写入
	// ——「表在涨、剪枝没跑」正是这张表当初要解决的问题。
	//
	// 跑在独立 goroutine 里而不是启动流程内：剪枝是全表 DELETE（首批可达
	// 百万行），放启动路径上会让网关「起来了但还打不开页面」。
	pruneDone := make(chan struct{})
	go func() {
		defer close(pruneDone)
		runPrune := func(reason string) {
			res, err := db.PruneOldUsage(context.Background(), store.DefaultRetentionDays)
			switch {
			case err != nil:
				// 单次失败不终止循环：下个周期重试即可，而退出循环等于
				// 永久放弃归档，明细表会一直涨到把写池堵死。
				logger.Error("usage prune failed", "error", err, "trigger", reason)
			case res.Skipped:
				logger.Debug("usage prune skipped", "reason", res.Reason, "trigger", reason)
			default:
				logger.Info("usage pruned into daily rollups", "trigger", reason,
					"deleted_rows", res.DeletedRows, "rollup_rows", res.RollupRows,
					"pruned_through_day", res.PrunedThrough)
			}
		}
		runPrune("startup")
		t := time.NewTicker(24 * time.Hour)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				runPrune("daily")
			case <-shutdown:
				return
			}
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	// 先停剪枝循环再关库：它可能正在跑一条全表 DELETE，与 main 返回时的
	// `defer db.Close()` 并发会拿到 "database is closed"。
	//
	// 等待有上限：剪枝是百万行量级的 DELETE，无界等待能把 SIGTERM 拖到几十秒，
	// 而编排器的终止宽限期通常更短 —— 那就变成强杀，剪枝事务虽会整体回滚
	// （聚合与删除同事务），但退出被拖长这件事本身已经不对。超时后照常退出。
	close(shutdown)
	select {
	case <-pruneDone:
	case <-time.After(5 * time.Second):
		logger.Warn("usage prune still running at shutdown; exiting anyway")
	}

	logger.Info("shutting down...")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		// Shutdown 到点只是**返回错误**，并不会终止仍在跑的 handler
		// （流请求 WriteTimeout 为 0、生命周期只看 TTFT/空闲看门狗，
		//  完全可能超过这 10s）。必须显式 Close 把在途连接掐掉：
		// 它返回时所有 handler 都已被中断，之后不会再有人调 usage.record。
		//
		// 顺序不能反 —— 先 close 队列再掐 handler 的话，那条 handler 收尾时
		// 恰好撞上「队列已关」。现在 record 侧也有 mu 兜底（双保险），
		// 但把顺序修正过来才是它本来的意图。
		logger.Error("graceful shutdown incomplete; forcing close of in-flight connections",
			"error", err, "note", "streams longer than the shutdown budget are terminated")
		if cerr := srv.Close(); cerr != nil {
			logger.Error("forced close failed", "error", cerr)
		}
	}

	// handler 都返回了，但在途的用量记录还在异步落库。不等它们，
	// 进程一退这批记录就没了 —— 表现为「最后几次调用的用量查不到」。
	drainCtx, drainCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer drainCancel()
	usage.wait(drainCtx)
	logger.Info("server stopped")
}

// runtimeReloader 把「上游池重建 + 快照重建」收口成一个可串行、可复用的原子操作。
//
// 为什么要收口（而不是各 handler 自行重建、或只依赖前端调 reload）：
//   - 池与快照是运行时的唯一事实视图，管理写操作落库后必须重建才生效；
//     生效路径若只靠前端，任何绕过前端的调用方（curl/脚本）都会造成静默分叉，
//     最敏感的是禁用下游 Key —— auth 校验读快照，不重建就照常放行。
//   - 池与快照是两个独立的原子域，「先换一个、再建另一个」时第二步失败会两边
//     分叉。这里先各自完整构建（PrepareFromStore / RebuildFromDB 都不触碰
//     运行状态），两边都成功才依次替换；任一步失败则运行时保持旧状态继续服务。
//
// mu 串行化并发触发（管理写操作的自动 reload + 手动 POST /admin/api/reload），
// 避免两轮重建交错执行。
type runtimeReloader struct {
	mu        sync.Mutex
	db        *store.Store
	masterKey []byte
	pool      *upstream.Pool
	cfg       *config.Config
	logger    *slog.Logger

	// dirty 置位表示「上一次重建失败，运行时落后于数据库」。
	//
	// 没有它的后果：某次 admin 写操作的重建失败后，只打一条 ERROR 就结束了 ——
	// 而**响应早已是 200 +「已禁用」**。于是调用方看到「已禁用」，
	// 数据面却继续放行该 key，直到下一次任意 admin 写操作碰巧成功才收敛。
	// 「禁用下游 Key」这类安全敏感操作会**无限期失效**，而日志里只有一条
	// 早已被淹没的 ERROR，没有任何迹象指向当前仍在放行。
	//
	// 用 atomic 而不是复用 mu：置位/清除是高频轻操作，而 mu 被 Reload 的
	// 整个执行过程持有（重建是百毫秒级），后台重试绝不能被它挡住。
	dirty atomic.Bool

	// dirtySince 记录首次置位时刻，用于「持续失败」时把重试间隔逐步拉长
	// 并在日志里报出已持续多久 —— 一个失败三天的问题不该表现得像刚发生。
	dirtySince atomic.Int64
}

// reloadRetryInterval 是后台重试的基准间隔。
//
// 取30 秒：短到「禁用一个 key 后最多半分钟就真的生效」，长到不会在
// 数据库持续故障时把日志刷爆（每次重试一条 WARN，30 秒一条 = 每小时 120 条）。
const reloadRetryInterval = 30 * time.Second

// reloadTimeout 是单次重建的时间上限。与 server.autoReloadTimeout 同值但
// 独立定义（跨包不导出私有常量）。重建只做 DB 读与 client 构建，正常毫秒级；
// 上限只为防一个卡死的 SQLite 读把后台重试 goroutine 永久挂住。
const reloadTimeout = 30 * time.Second

// reloadRetryMaxInterval 是退避上限。持续失败时按 2 的幂次拉长，
// 封顶 10 分钟：再长就失去了「自动收敛」的意义 —— 那时正确的做法是
// 让人知道（见下面的告警日志），而不是继续静默重试。
const reloadRetryMaxInterval = 10 * time.Minute

// MarkDirty 记下一次失败的重建，等待后台重试兜底。
func (rr *runtimeReloader) MarkDirty() {
	if rr.dirty.CompareAndSwap(false, true) {
		rr.dirtySince.Store(time.Now().UnixMilli())
	}
}

// markClean 清脏标志。必须由 Reload 自己在**成功后**调用 ——
// 不能放在调用方，否则「Reload 成功但调用方没记」与「Reload 失败」在
// 标志上无法区分，那正是本缺陷的形态。
func (rr *runtimeReloader) markClean() {
	rr.dirty.Store(false)
	rr.dirtySince.Store(0)
}

// IsDirty 报告运行时是否落后于数据库。供健康检查与测试读取。
func (rr *runtimeReloader) IsDirty() bool { return rr.dirty.Load() }

// watchReloadRetry 在 dirty 置位时反复重试，直到成功或 ctx 结束。
//
// 为什么必须有它：AutoReload 的一次性尝试失败后，若没有后续触发，
// 「已禁用」与「仍在放行」的偏离会**无限期**存在。审计与数据面各有一条
// 独立通道（adminAuto、bootstrap）都走 Reload，所以单一失败点会让两处
// 同时失准。
//
// 退避：第 n 次失败后等 min(2^n × 30s, 10min)。数据库短暂抖动会在几秒内
// 被自愈，而真故障不会因为高频重试变好 —— 退避让两者都得到合理对待。
func (rr *runtimeReloader) watchReloadRetry(ctx context.Context) {
	backoff := reloadRetryInterval
	for {
		// 脏标志是唯一的触发条件：干净时不轮询、不占连接、不产生日志。
		if !rr.dirty.Load() {
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second): // 轮询间隔：1s 足够跟手
			}
			continue
		}

		since := rr.dirtySince.Load()
		if since > 0 {
			elapsed := time.Since(time.UnixMilli(since)).Round(time.Second)
			rr.logger.Warn("runtime still lags behind database; retrying rebuild",
				"dirty_for", elapsed.String(),
				"next_retry_in", backoff.String())
		}

		rctx, cancel := context.WithTimeout(ctx, reloadTimeout)
		if err := rr.Reload(rctx); err == nil {
			rr.logger.Info("runtime caught up with database after failed reload",
				"dirty_for", time.Since(time.UnixMilli(since)).Round(time.Second).String())
			backoff = reloadRetryInterval
		} else {
			rr.logger.Warn("runtime reload retry failed; will retry again",
				"error", err, "next_retry_in", backoff.String())
			backoff *= 2
			if backoff > reloadRetryMaxInterval {
				backoff = reloadRetryMaxInterval
			}
		}
		cancel()

		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
	}
}

// Reload 串行执行一次「池 + 快照」重建；任何一步失败都不改变运行状态。
//
// failures 会随快照下发（P4 / 设计 §4.7）：池里没建成的 provider 在管理面
// 标成「未就绪」并给出原因。此前这些只进日志，运维看到的是
// 「路由配了但请求莫名 500」，查不到根因。
func (rr *runtimeReloader) Reload(ctx context.Context) error {
	rr.mu.Lock()
	defer rr.mu.Unlock()

	providers, poolFailures, err := rr.pool.PrepareFromStore(ctx, rr.db, rr.masterKey, rr.cfg)
	if err != nil {
		return fmt.Errorf("prepare upstream pool: %w", err)
	}
	// 把池侧的失败投影成 snapshot 的最小视图。两类情形都要让界面看见，
	// 但语义不同：
	//   - 零可用凭据 / 读不到凭据清单 → provider **完全不可用**（Ready=false）；
	//   - 个别凭据失败但还有可用的 → provider 仍在服务，界面标为降级。
	//
	// 同一 provider 的多条凭据失败在界面上重复标没意义，按 slug 只留一条，
	// 「完全不可用」优先于「部分降级」。
	type proj struct {
		reason string
		fatal  bool
	}
	bySlug := make(map[string]proj, len(poolFailures))
	for _, f := range poolFailures {
		fatal := f.Stage == "no_credentials" || f.Stage == "list_credentials"
		if prev, ok := bySlug[f.Slug]; ok && prev.fatal && !fatal {
			continue
		}
		bySlug[f.Slug] = proj{reason: f.Reason, fatal: fatal}
	}
	failures := make([]snapshot.ProviderFailure, 0, len(bySlug))
	for slug, p := range bySlug {
		failures = append(failures, snapshot.ProviderFailure{Slug: slug, Reason: p.reason})
	}
	// 排序只为让日志与任何按序展示的地方稳定（map 遍历顺序是随机的）。
	sort.Slice(failures, func(i, j int) bool { return failures[i].Slug < failures[j].Slug })

	// logger 为空时不留痕也不崩：这是「有 provider 未就绪」时唯一会走到的分支，
	// 让它 panic 等于在最需要日志的时刻把进程打挂。
	if len(poolFailures) > 0 && rr.logger != nil {
		rr.logger.Warn("some providers are not ready after reload",
			"count", len(poolFailures), "providers", len(bySlug))
	}

	snap, err := snapshot.RebuildFromDB(ctx, rr.db, failures)
	if err != nil {
		return fmt.Errorf("rebuild snapshot: %w", err)
	}
	rr.pool.Install(providers, liveTargetIDs(ctx, rr.db))
	snapshot.Swap(snap)
	// 只有走到这里（池与快照都已换成功）才清脏标志。
	// 上面每条 return err 的路径都保持 dirty —— 运行时确实落后于库，
	// 后台重试循环因此知道还要再试。
	rr.markClean()
	return nil
}

// liveTargetIDs 返回当前库里全部 route target 的 ID 集合，交给 Pool.Install
// 决定哪些熔断状态该跨重建保留。读不到时返回空集（与 upstream 侧的兜底一致：
// 清掉全部熔断只是让坏上游短暂复活，保留已删目标的状态则会单调堆积）。
func liveTargetIDs(ctx context.Context, db *store.Store) map[string]bool {
	targets, err := db.ListAllRouteTargets(ctx)
	if err != nil {
		return map[string]bool{}
	}
	ids := make(map[string]bool, len(targets))
	for _, t := range targets {
		ids[t.ID] = true
	}
	return ids
}

func bootstrapDB(db *store.Store, cfg *config.Config, logger *slog.Logger, masterKey []byte) {
	ctx := context.Background()
	existing, _ := db.ListProviders(ctx)
	if len(existing) > 0 {
		if len(cfg.Bootstrap.Providers) > 0 {
			logger.Warn("database already has providers, ignoring bootstrap config")
		}
		return
	}

	if len(cfg.Bootstrap.Providers) == 0 {
		return
	}

	logger.Info("bootstrapping database from config")

	// 记录 provider slug + 上游模型名 -> 实际写入 upstream_models.id 的映射。
	// routes.upstream_model_id 是外键，必须引用真实主键，不能自行拼接。
	modelIDByKey := make(map[string]string)

	for _, bp := range cfg.Bootstrap.Providers {
		p := &store.Provider{
			ID:         bp.Slug,
			Slug:       bp.Slug,
			Name:       bp.Name,
			Protocol:   bp.Protocol,
			Endpoint:   bp.Endpoint,
			Enabled:    true,
			MaxRetries: cfg.Defaults.MaxRetries,
		}
		if err := db.CreateProvider(ctx, p); err != nil {
			logger.Error("bootstrap: create provider failed", "slug", bp.Slug, "error", err)
			continue
		}

		for _, bc := range bp.Credentials {
			apiKey := bc.APIKey
			if bc.APIKeyEnv != "" {
				apiKey = os.Getenv(bc.APIKeyEnv)
			}
			if apiKey == "" {
				continue
			}

			var enc []byte
			if masterKey != nil {
				enc, _ = crypto.Encrypt([]byte(apiKey), masterKey)
			} else {
				enc = []byte(apiKey)
			}

			c := &store.Credential{
				ID:         generateID(),
				ProviderID: bp.Slug,
				Label:      bc.Label,
				APIKeyEnc:  enc,
				Enabled:    true,
				Weight:     1,
				Status:     "healthy",
			}
			if err := db.CreateCredential(ctx, c); err != nil {
				logger.Error("bootstrap: create credential failed", "provider", bp.Slug, "error", err)
			}
		}

		for _, modelID := range bp.Models {
			m := &store.UpstreamModel{
				ID:         generateID(),
				ProviderID: bp.Slug,
				ModelID:    modelID,
				Enabled:    true,
			}
			if err := db.CreateUpstreamModel(ctx, m); err != nil {
				logger.Error("bootstrap: create model failed", "provider", bp.Slug, "model", modelID, "error", err)
				continue
			}
			modelIDByKey[bp.Slug+"/"+modelID] = m.ID
		}
	}

	for _, br := range cfg.Bootstrap.Routes {
		providerID := br.Provider
		upstreamModelID, ok := modelIDByKey[providerID+"/"+br.Model]
		if !ok {
			logger.Error("bootstrap: skip route, upstream model not found",
				"public_name", br.PublicName, "provider", providerID, "model", br.Model)
			continue
		}

		r := &store.Route{
			ID:              generateID(),
			PublicName:      br.PublicName,
			ProviderID:      providerID,
			UpstreamModelID: upstreamModelID,
			Enabled:         true,
		}
		if err := db.CreateRoute(ctx, r); err != nil {
			logger.Error("bootstrap: create route failed", "public_name", br.PublicName, "error", err)
		}
	}

	logger.Info("bootstrap completed")
}

func buildSnapshotFromConfig(cfg *config.Config) *snapshot.Snapshot {
	snap := &snapshot.Snapshot{
		Routes:     routing.NewRouteIndex(),
		Providers:  make(map[string]*snapshot.ProviderSnapshot),
		KeysByHash: make(map[string]*snapshot.KeySnapshot),
	}
	modelIDs := make(map[string]string)

	for _, bp := range cfg.Bootstrap.Providers {
		snap.Providers[bp.Slug] = &snapshot.ProviderSnapshot{
			ID:       bp.Slug,
			Slug:     bp.Slug,
			Name:     bp.Name,
			Endpoint: bp.Endpoint,
			Enabled:  true,
		}
		snap.Routes.AddProvider(&routing.ProviderRef{
			ID:       bp.Slug,
			Slug:     bp.Slug,
			Endpoint: bp.Endpoint,
			Protocol: bp.Protocol,
			Enabled:  true,
		})
		for _, modelID := range bp.Models {
			id := cfgModelID(bp.Slug, modelID)
			modelIDs[bp.Slug+"/"+modelID] = id
			snap.Routes.AddUpstreamModel(&routing.UpstreamModel{
				ID:         id,
				ProviderID: bp.Slug,
				ModelID:    modelID,
				Enabled:    true,
			})
		}
	}

	for _, br := range cfg.Bootstrap.Routes {
		upstreamModelID, ok := modelIDs[br.Provider+"/"+br.Model]
		if !ok {
			continue
		}
		snap.Routes.AddRoute(&routing.Route{
			ID:              br.PublicName,
			PublicName:      br.PublicName,
			ProviderID:      br.Provider,
			UpstreamModelID: upstreamModelID,
			Enabled:         true,
		})
	}

	return snap
}

// cfgModelID 为配置引导的上游模型生成稳定的合成主键。
// 内存快照与 DB 引导两条路径都必须让 route 引用模型主键，
// 而不是 provider/model 拼接串，否则按公开名解析会找不到模型。
func cfgModelID(providerSlug, modelID string) string { return providerSlug + "/" + modelID }

// errorWriter 是「按下游协议写出错误响应」的统一签名。
type errorWriter func(w http.ResponseWriter, status int, code, message string)

// denyAdminKeyCreate 拦截**管理员**调用 POST /admin/api/keys（建 key）。
//
// # 需求（2026-10 控制面/数据面分离）
//
// 「普通用户自助建 key，管理员不能建」。管理员只做控制面管理；要调模型，
// 需先建普通用户、由普通用户自己发 key（与管理员不能调 /v1 配套）。
//
// # 为什么在**路由/中间件层**拦，而不改 internal/admin/key_handler.go 的 Create
//
//  1. 改动面最小、边界最清晰：这是一条**路由级策略**，只针对「POST /admin/api/keys」
//     这一个端点。写在 handler 里会与 Create 内部那一大段「自助建 key 的额度封顶」
//     逻辑缠在一起，而那段的语义（谁能建、建出来多少额度）与本策略（谁**不允许**
//     建）其实是两件事，混在一起反而更难读。
//  2. 依赖的鉴权上下文本来就在这一层可用：AdminGateGuard / UserAuth 已把登录用户
//     注入 context（server.UserFromContext），路由层读它即可，无需再查库。
//  3. Create 仍保持「普通用户自助」的原逻辑不动，回归风险最低。
//
// 换来的约束：这条策略绑定在「main.go 注册的这条路由」上。若将来另有代码用
// KeyHandler.Create 组新路由，必须记得同样套上本包装（或改到 handler 层）。
// 已在下方注册处留下注释标注，避免后人漏掉。
//
// # 为什么管理员仍能**管理别人的** key（PATCH/DELETE/列表）
//
// 本策略只拦「新建」这一个动作。管理员照旧可以：列出全部 key、把已有的 key
// 认领给某个用户（Update 的 user_id 路径）、禁用/启用/改配额/改分组覆盖等。
// 也就是说管理员握有对存量 key 的**完整治理权**，只是不再亲手铸造新的数据面凭据。
// 这正是需求要的「管理员只负责管理网关」。
//
// 身份缺失一律 401（fail-closed）：拿不到登录身份绝不等于「当作普通用户放行」，
// 那会让匿名请求绕过本策略。与 key_handler.Create 的同一处口径一致。
func denyAdminKeyCreate(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u := server.UserFromContext(r.Context())
		if u == nil || u.ID == "" {
			// 空 ID 一律拒绝：绝不能让「拿不到身份」被当成「非管理员」而放行。
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"message":"需要登录","type":"auth_error"}}`))
			return
		}
		if u.IsAdmin() {
			// 403（不是 401）：管理员身份是**已确认**的，不允许只是策略限制，
			// 与数据面拦截管理员调 /v1 的口径一致。消息明确指向正确出路
			// （建普通用户），避免管理员以为 key 创建坏了而去排查无关配置。
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":{"message":"管理员账号不能创建 API key：请新建一个普通用户账户，由该用户自行创建 key 后用于调用模型","type":"auth_error"}}`))
			return
		}
		next(w, r)
	}
}

// writeAuthError 把鉴权失败映射成下游协议对应的错误响应。
// /v1 下的每个端点都走这一处，免得口径漂移（例如某个端点把「密钥被禁用」
// 也当成 401 而非 403）。状态码与 code 是协议无关的语义，形状由各协议渲染。
func writeAuthError(w http.ResponseWriter, err error, writeErr errorWriter) {
	switch {
	case errors.Is(err, auth.ErrNoKey):
		writeErr(w, http.StatusUnauthorized, "invalid_api_key", "missing API key")
	case errors.Is(err, auth.ErrKeyDisabled):
		writeErr(w, http.StatusForbidden, "invalid_api_key", "API key disabled")
	case errors.Is(err, auth.ErrInvalidKey):
		writeErr(w, http.StatusUnauthorized, "invalid_api_key", "invalid API key")
	case errors.Is(err, auth.ErrKeyExpired):
		// 403 而非 401：key 本身是有效的，只是不该再用。
		// 客户端看到 401 的第一反应是「重新配 key」，那是误导 ——
		// 真相是「换一把或让管理员续期」。
		writeErr(w, http.StatusForbidden, "key_expired", "this API key has expired")
	case errors.Is(err, auth.ErrIPNotAllowed):
		// 同样 403 + 独立 code：客户端能据此区分「key 坏了」与
		//「你的网络位置不允许用这把 key」，后者往往指向防火墙/代理配置问题。
		writeErr(w, http.StatusForbidden, "ip_not_allowed",
			"source IP is not allowed for this API key")
	case errors.Is(err, auth.ErrAdminCannotCallModel):
		// 管理员账号不得调用模型（控制面/数据面分离，2026-10 需求确认）。
		//
		// # 为什么是 403 而不是 401
		//
		// 401 的语义是「凭据无效，请换一把 key」。而这里**key 和账号都是好的**，
		// 只是角色不允许 —— 换多少把管理员的 key 都没用。回 401 会让 SDK 与
		// 运维走错方向：SDK 按认证失败提示「重新配置 key」（甚至自动重试
		// 新 key），运维则会以为 key 损坏/过期而白查一圈。
		//
		// 403 =「明确知道你是谁，但不允许」，与本文件既有的 key_disabled /
		// key_expired / ip_not_allowed / model_not_allowed 口径一致。
		//
		// # 为什么**不**用 404 藏起来
		//
		// 隐藏存在性能减少信息泄露，但代价是管理员看到「key 不存在」，
		// 完全不知道发生了角色策略变更 —— 与「给清晰消息、让他知道要去建
		// 普通用户」的需求正相反。而且这里没有可枚举的攻击面：调用方本来
		// 就持有这把 key 的明文，404 藏不住任何东西。
		//
		// # 为什么 code 要独立于 invalid_api_key
		//
		// SDK 按 error.type 分流重试：invalid_api_key 归 authentication_error，
		// 客户端可能反复换 key 重试。本条的 type 映射见 outwire 的
		// errorTypeFromCode（→ permission_error），是**终态**错误，
		// 重试无意义，必须由人去建普通用户。
		writeErr(w, http.StatusForbidden, "admin_cannot_call_model",
			"administrator accounts cannot call models: create a regular user account "+
				"and use the API key issued to that user")
	default:
		writeErr(w, http.StatusUnauthorized, "invalid_api_key", "authentication failed")
	}
}

// ---- 下游协议编解码（DESIGN §6.1 / §7）----
//
// ingressCodec 抽象「下游协议」的差异：解码请求、写错误、编码非流式响应、
// 构造流式 sink。转发的骨架（鉴权、配额、路由、故障转移、看门狗、落库）
// 是协议无关的，只有 handleIngress 一个实现 —— 新增下游协议不再复制循环。
type ingressCodec interface {
	// Name 是 usage_records.ingress_protocol 的取值。
	Name() string
	// Decode 读取并解析请求体；错误由调用方映射成 400/413。
	Decode(r *http.Request, maxBytes int64) (*ingressRequest, error)
	// WriteError 把网关内部 code 渲染成协议的错误形状。
	WriteError(w http.ResponseWriter, status int, code, message string)
	// WriteNonStream 编码非流式成功响应。
	WriteNonStream(w http.ResponseWriter, resp *rosetta.ChatResponse, model string)
	// NewSink 构造流式编码器；调用前 SSE 响应头已写出。
	NewSink(w http.ResponseWriter, model string) outwire.StreamSink
}

// ingressRequest 是解码后的协议无关请求视图。
//
// buildRosetta 是**每次 attempt 调一次**的工厂而不是建好的请求：链上各目标的
// 上游协议可能不同，applyUpstreamExtras 会往 req.Extra 挂不同形状的协议私有
// 字段，跨 attempt 复用同一个实例会把 A 目标的 Extra 泄漏给 B 目标。
type ingressRequest struct {
	buildRosetta     func() *rosetta.ChatRequest
	stream           bool
	model            string // 对外的公开模型名（解析前原样）
	wantsStreamUsage bool
	// requiresStructuredOutput 表示请求带**硬性**结构化输出约束（chat 的
	// response_format 或 responses 的 text.format 为 json_object / json_schema）。
	// 这类约束打到 anthropic 上游等于「200 但约束静默丢失」，不能放行 ——
	// handleIngress 据此把 anthropic 候选从链上滤掉，全被滤空则 400。
	requiresStructuredOutput bool
	applyUpstreamExtras      func(req *rosetta.ChatRequest, upstreamProtocol string)

	// rawEffort 是客户端**原样**请求的思考挡位（归一到三档之前），空串 = 未指定。
	//
	// 为什么要单独带一路而不复用 buildRosetta 之后的 Thinking.Effort：三档归一
	// 会把 minimal 合并进 low、xhigh 合并进 high，而模型档位配置可能恰好只支持
	// xhigh 或 minimal —— 归一之后再夹取就会选错档，等于这个功能没做。
	rawEffort string
}

// applyThinkingCapability 按**目标模型**配好的思考能力调整请求的思考设置。
//
// 每个 attempt 都要调一次（而不是解析后只调一次）：故障转移链上各目标的模型
// 可能不同、能力也可能不同 —— 调整必须在「实际要发给谁」这个粒度上做。
// 这与 applyUpstreamExtras 必须在 attempt 内部做的理由完全一致。
//
// 三件事，顺序不可换：
//
//  1. **不支持思考**（supports_thinking 显式为 false）：剥掉整个 Thinking。
//     放在最前面，因为它是唯一会让「思考」这个动作**彻底消失**的分支，
//     而下面两步都只在「还要思考」的前提下才有意义。
//
//  2. **夹紧挡位**（effort_levels 已配）：客户端要的那一档不在模型支持集里
//     时，就近取一档，而不是丢弃整个意图。
//
//  3. **写回原值**（支持集内或未配置）：把客户端的原始档位原样交给 SDK。
//     走 ThinkingConfig.EffortRaw 而不是 Effort —— 后者只认三档，会把
//     xhigh 静默压成 high，那正是本功能要消灭的行为。
//
// 客户端没指定挡位（Unset）时不写 EffortRaw：那既可能来自未配置（网关不
// 该干预），也可能来自「思考但不要档位」；两种都不该被网关替它选一档。
func (ing *ingressRequest) applyThinkingCapability(req *rosetta.ChatRequest, m *routing.UpstreamModel, protocol string) {
	if m == nil || req.Thinking == nil {
		return
	}

	// 1. 模型明确不支持思考：剥掉，而不是让请求带着思考配置打过去。
	//
	// 为什么不留给 SDK 的闸门（它已经能报 ErrThinkingUnsupported）：故障转移
	// 链上换一个目标可能就换成了支持思考的模型，而闸门的作用域只到单个
	// client。在网关这一层处理，才能「剥掉思考」与「换个目标」同时成立。
	// 剥而不是 400：思考强度是**请求偏好**，为一个偏好让整个请求失败，
	// 比退化成不思考更糟 —— 客户端拿到的仍是完整答案。
	//
	// nil（未配置）不进来：那不是「不支持」，是「不知道」，而「不知道」
	// 绝不能被当成「确定不支持」。
	if m.SupportsThinking != nil && !*m.SupportsThinking {
		req.Thinking = nil
		return
	}

	if ing.rawEffort == "" {
		return
	}

	// 2. 夹紧到模型支持的子集。
	wanted := effort.Parse(ing.rawEffort)
	if wanted == effort.Unset {
		return
	}
	applied := wanted
	if len(m.EffortLevels) > 0 && !effort.Supports(wanted, m.EffortLevels) {
		applied = effort.Clamp(wanted, m.EffortLevels)
		// 降级必须留痕：没有它，「实际强度和我要的不一样」这个现象与本
		// 功能落地前完全同形（都是强度不对），没有人能分辨是客户端没配好、
		// 还是网关夹的。
		slog.Warn("clamped reasoning effort to the model's supported set",
			"model", m.ModelID, "provider", m.ProviderID,
			"requested", string(wanted), "applied", string(applied))
	}

	// 3. 原样写回。Anthropic 例外：那个协议没有 effort 字段，发上去会被
	// 拒，而它的思考旋钮是 token 预算 —— 那条路径由 ingwire 在解码时就把
	// budget_tokens 填好了（见 AnthropicMessagesRequest.RawEffort），这里
	// 只需把档位还给 SDK 的三档映射，让它折回预算。
	if protocol == "anthropic" {
		req.Thinking.EffortRaw = ""
		req.Thinking.Effort = rosetta.Effort(effort.ToRosettaLevel(applied))
		return
	}
	req.Thinking.EffortRaw = string(applied)
	req.Thinking.Effort = rosetta.EffortUnset
}

// rateCommit 把一次请求的两种预占（TPM 窗口 + 终身配额）在请求终结时收尾。
// nil 接收者安全：TPM 未启用（额度 0）时调用方可以放一个 nil ——
// 现在主路径在 TPMLimit==0 时干脆不建这个结构，commit 就是纯 no-op。
type rateCommit struct {
	limiter  *ratelimit.Limiter
	keyID    string
	limit    int // 预占时的 tpmLimit；0 = 未启用，CommitTPM 据此直接返回
	reserved int64

	// store 用于**终身配额**的预占退回；为 nil 时跳过（不限额或测试）。
	store  *store.Store
	logger *slog.Logger
	// quotaReserved 是本次请求预占的 token 数（0 = 未预占，或已释放）。
	quotaReserved int64
}

// commit 用量已知时的**全量**收尾：TPM 按真实用量校正 + 退配额预占。
//
// 正常路径一律走这里，别为单一预占另起入口 —— 两种预占共用同一个 est 与
// 同一个收尾，分头调用时容易只改一处而漏掉另一处，且配额那条漏掉是
// **终身**偏差，不像 TPM 窗口会自愈。唯一允许绕开 commit 的路径是
// usage missing（见 releaseQuota 的说明）。
func (rc *rateCommit) commit(actual int64) {
	if rc == nil {
		return
	}
	rc.limiter.CommitTPM(rc.keyID, rc.limit, rc.reserved, actual)
	rc.releaseQuota()
}

// releaseQuota 只退终身配额的预占，**不碰 TPM 窗口**。
//
// 独立成方法是因为有一条真实路径必须拆开收尾：上游没报 usage
// （usage_state="missing"）时，TPM 预占要**故意保留**——commit(0) 会全额退还
// 预占，等于不报用量的上游完全绕过 TPM；保留估算值让限速继续约束这类流量，
// 估算值随窗口翻转自然清零。但配额预占必须退掉：它是终身累计，预占按
// max_tokens 全额计，不退就等于每次 missing 请求永久吃掉数万 token 额度
// （2026-10-07 修复的 P1-3 —— 修复前 missing 时整个 commit 被跳过）。
//
// quotaReserved 先取后清零，让释放天然幂等：任何路径重复收尾（或未来重构
// 引入第二次调用）时第二次是空操作，不会把别的请求刚立起来的预占退掉。
func (rc *rateCommit) releaseQuota() {
	if rc == nil || rc.store == nil || rc.quotaReserved <= 0 {
		return
	}
	reserved := rc.quotaReserved
	rc.quotaReserved = 0
	if err := rc.store.ReleaseQuota(context.Background(), rc.keyID, reserved); err != nil {
		if rc.logger != nil {
			rc.logger.Warn("release quota reservation failed",
				"key_id", rc.keyID, "reserved", reserved, "error", err)
		}
	}
}

// setRetryAfter 写 Retry-After 响应头（向上取整秒，至少 1）。
//
// 必须向上取整而不是截断：windowRemaining 已经带了 +1s 余量，若这里再用
// int(d.Seconds()) 截断，余量会被吃掉大半（剩 0.9s 时算成 1s，客户端 1s 后
// 重试仍在原窗口内，立刻又被拒 —— 正是这个函数要避免的抖动）。
func setRetryAfter(w http.ResponseWriter, d time.Duration) {
	seconds := int(math.Ceil(d.Seconds()))
	if seconds < 1 {
		seconds = 1
	}
	w.Header().Set("Retry-After", strconv.Itoa(seconds))
}

// estimateRequestTokens 粗估一次请求的输入 token —— TPM 限速的预占依据。
// 不追求精确（精确值在请求终结时经 CommitTPM 以真实 usage 校正），
// 只需落在同一数量级，避免预占远小于真实值令 TPM 形同虚设、或反之误拒。
// 口径：文本/思考/工具参数/工具定义的字符估算 + max_tokens（输出侧按上限全额
// 预占，宁可先多占后退还，也不先少占再超发）。
func estimateRequestTokens(req *rosetta.ChatRequest) int64 {
	var b strings.Builder
	b.WriteString(req.System)
	for _, m := range req.Messages {
		for _, blk := range m.Blocks {
			b.WriteString(blk.Text)
			b.WriteString(blk.Thinking)
			b.WriteString(blk.Content)
			b.WriteString(blk.Arguments)
		}
	}
	for _, t := range req.Tools {
		b.WriteString(t.Name)
		b.WriteString(t.Description)
		b.WriteString(string(t.Parameters))
	}
	return int64(rosetta.EstimateTokens(b.String())) + int64(req.MaxOutputTokens)
}

// openaiChatCodec 服务 POST /v1/chat/completions。
type openaiChatCodec struct{}

func (openaiChatCodec) Name() string { return "openai-chat" }

func (openaiChatCodec) Decode(r *http.Request, maxBytes int64) (*ingressRequest, error) {
	req, err := inwire.DecodeOpenAIChatRequest(r, maxBytes)
	if err != nil {
		return nil, err
	}
	return &ingressRequest{
		buildRosetta:             req.ToRosetta,
		stream:                   req.Stream,
		model:                    req.Model,
		wantsStreamUsage:         wantsStreamUsage(req),
		requiresStructuredOutput: req.RequiresStructuredOutput(),
		applyUpstreamExtras:      req.ApplyProtocolPrivateExtra,
		rawEffort:                req.RawEffort,
	}, nil
}

func (openaiChatCodec) WriteError(w http.ResponseWriter, status int, code, message string) {
	outwire.WriteOpenAIError(w, status, code, message)
}

func (openaiChatCodec) WriteNonStream(w http.ResponseWriter, resp *rosetta.ChatResponse, model string) {
	outwire.WriteNonStreamResponse(w, resp, model)
}

func (openaiChatCodec) NewSink(w http.ResponseWriter, model string) outwire.StreamSink {
	flusher, _ := w.(http.Flusher)
	return outwire.NewOpenAISink(outwire.NewSSEWriter(w, flusher, "chatcmpl-"+generateID(), model, time.Now().Unix()))
}

// anthropicMessagesCodec 服务 POST /v1/messages（Anthropic Messages 协议，
// Claude Code 等客户端的接入点）。
type anthropicMessagesCodec struct{}

func (anthropicMessagesCodec) Name() string { return "anthropic" }

func (anthropicMessagesCodec) Decode(r *http.Request, maxBytes int64) (*ingressRequest, error) {
	req, err := inwire.DecodeAnthropicMessagesRequest(r, maxBytes)
	if err != nil {
		return nil, err
	}
	return &ingressRequest{
		buildRosetta: req.ToRosetta,
		stream:       req.Stream,
		model:        req.Model,
		// Anthropic 的 message_delta 恒带 usage，没有 include_usage 开关。
		wantsStreamUsage: false,
		// Anthropic 入站协议没有结构化输出概念（无 response_format 对应物），
		// 恒为 false —— 不参与硬约束过滤。
		requiresStructuredOutput: false,
		applyUpstreamExtras:      req.ApplyUpstreamExtras,
		rawEffort:                req.RawEffort,
	}, nil
}

func (anthropicMessagesCodec) WriteError(w http.ResponseWriter, status int, code, message string) {
	outwire.WriteAnthropicError(w, status, code, message)
}

func (anthropicMessagesCodec) WriteNonStream(w http.ResponseWriter, resp *rosetta.ChatResponse, model string) {
	outwire.WriteAnthropicResponse(w, resp, model)
}

func (anthropicMessagesCodec) NewSink(w http.ResponseWriter, model string) outwire.StreamSink {
	flusher, _ := w.(http.Flusher)
	return outwire.NewAnthropicSSE(w, flusher, "msg_"+generateID(), model)
}

// openaiResponsesCodec 服务 POST /v1/responses（OpenAI Responses 协议，
// Codex CLI 等客户端的接入点）。错误形状与 openai-chat 同一套信封。
type openaiResponsesCodec struct{}

func (openaiResponsesCodec) Name() string { return "openai-responses" }

func (openaiResponsesCodec) Decode(r *http.Request, maxBytes int64) (*ingressRequest, error) {
	req, err := inwire.DecodeResponsesRequest(r, maxBytes)
	if err != nil {
		return nil, err
	}
	return &ingressRequest{
		buildRosetta: req.ToRosetta,
		stream:       req.Stream,
		model:        req.Model,
		// Responses 的 response.completed 恒带 usage，没有 include_usage 开关。
		wantsStreamUsage:         false,
		requiresStructuredOutput: req.RequiresStructuredOutput(),
		applyUpstreamExtras:      req.ApplyUpstreamExtras,
		rawEffort:                req.RawEffort,
	}, nil
}

func (openaiResponsesCodec) WriteError(w http.ResponseWriter, status int, code, message string) {
	outwire.WriteOpenAIError(w, status, code, message)
}

func (openaiResponsesCodec) WriteNonStream(w http.ResponseWriter, resp *rosetta.ChatResponse, model string) {
	outwire.WriteResponsesResponse(w, resp, model)
}

func (openaiResponsesCodec) NewSink(w http.ResponseWriter, model string) outwire.StreamSink {
	flusher, _ := w.(http.Flusher)
	return outwire.NewResponsesSSE(w, flusher, "resp_"+generateID(), model)
}

func handleIngress(pool *upstream.Pool, cfg *config.Config, usage *usageRecorder, limiter *ratelimit.Limiter, codec ingressCodec) http.HandlerFunc {
	db, logger := usage.db, usage.logger
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()

		authCtx, err := auth.Authenticate(r)
		if err != nil {
			writeAuthError(w, err, codec.WriteError)
			return
		}

		// RPM 限速（DESIGN §11.4）：额度随鉴权从快照带出（0 = 不限）。
		// 被拒的请求**不**计数 —— 实现见 ratelimit.AllowRPM 的注释：
		// 若计数，客户端在窗口内疯狂重试会让计数只增不减，把整个窗口
		// 永久锁死（直到窗口翻转才恢复），一个配错 RPM 的客户端就能把
		// 自己彻底堵死。计数只记真正被放行的请求。
		//
		// （此前这里写的是「被拒的请求同样计数」，与实现相反。实现是对的，
		//  错的是注释——留着会诱导后人「修反」成count-on-reject。）
		if ok, retry := limiter.AllowRPM(authCtx.KeyID, authCtx.RPMLimit); !ok {
			logger.Warn("rate limited (rpm)", "key_id", authCtx.KeyID,
				"retry_after", retry.String(),
				"request_id", server.RequestIDFromContext(r.Context()))
			setRetryAfter(w, retry)
			codec.WriteError(w, http.StatusTooManyRequests, "rate_limit_exceeded",
				"request rate limit exceeded for this API key")
			return
		}

		// 用户级配额预检（MULTIUSER.md §4.3：三级配额的最外层总闸）。
		//
		// **只在用户配了额度时才查库**：quota_tokens=0（不限额）是常态，
		// 为常态付一次 SUM 查询成本不合理。这一点与 key 级预检不同 ——
		// key 的额度就在即将读取的那一行里，查是顺带。
		//
		// 已用量走实时 SUM（§4.3 决策 B）而不是触发器累加：触发器按
		// access_key_id 累加，**感知不到 key 被删除**，会永久留下偏高的
		// 计数让用户再也撞不开上限。SUM 走 idx_usage_user_ts 索引。
		if u := snapshot.Get().UsersByID[authCtx.UserID]; u != nil && u.QuotaTokens > 0 {
			used, uerr := db.SumUserUsedTokens(r.Context(), authCtx.UserID)
			if uerr != nil {
				// 与 key 级同口径：查询抖动时 fail-open，不因一次读失败
				// 拒绝正常流量。并发下容忍至多一个在途请求超发。
				logger.Error("user quota lookup failed", "error", uerr, "user_id", authCtx.UserID)
			} else if used >= u.QuotaTokens {
				logger.Warn("user quota exceeded", "user_id", authCtx.UserID,
					"used", used, "quota", u.QuotaTokens,
					"request_id", server.RequestIDFromContext(r.Context()))
				codec.WriteError(w, http.StatusTooManyRequests, "insufficient_quota",
					"this account has exhausted its token quota")
				return
			}
		}

		ing, err := codec.Decode(r, int64(cfg.Defaults.MaxRequestBodyBytes))
		if err != nil {
			// 超限时中间件的 MaxBytesReader 会返回 *http.MaxBytesError。
			// 旧实现把它包成 "read body: ..." 一并当 400 回，客户端看不出
			// 「是body太大」还是「body格式错」——两者要采取的行动完全不同。
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				codec.WriteError(w, http.StatusRequestEntityTooLarge, "request_too_large",
					fmt.Sprintf("request body exceeds %d bytes", tooLarge.Limit))
				return
			}
			codec.WriteError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
			return
		}

		// TPM 限速（DESIGN §11.4）：流式下真实 token 只有流结束才知道，
		// 所以按「请求前估算预占 + 请求后按真实 usage 校正」两段执行。
		// 被拒的请求不预占（没放行就不该消耗窗口额度），也不预扣任何东西。
		//
		// TPMLimit==0（未启用，也是主流配置）时**整段跳过**：estimateRequestTokens
		// 会遍历全部消息与工具定义拼出一个与请求体同量级的字符串再估算，
		// 是一次纯浪费的全量分配；而它算出的 est 随后必然被丢弃。
		// 同一个请求在 attempt* 里还会再 buildRosetta 一次，估算那次是多余的第 N 次。
		// 两种预占共用同一个 est 与同一个 rateCommit 收尾 —— 分开放两次
		// 估算/两次收尾只会让两边在某条路径上被漏掉一处（且配额那条漏掉是
		// **终身**偏差，不像 TPM 窗口会自愈）。
		//
		// est 的计算按需触发：TPMLimit==0 且 key 未配quota 时整个函数跳过，
		// 不做那次「遍历全部消息与工具定义拼出请求体同量级字符串」的估算。
		var rate *rateCommit
		var est int64
		needEstimate := authCtx.TPMLimit > 0
		if needEstimate {
			est = estimateRequestTokens(ing.buildRosetta())
			if ok, retry := limiter.ReserveTPM(authCtx.KeyID, authCtx.TPMLimit, est); !ok {
				logger.Warn("rate limited (tpm)", "key_id", authCtx.KeyID,
					"estimated_tokens", est, "retry_after", retry.String(),
					"request_id", server.RequestIDFromContext(r.Context()))
				setRetryAfter(w, retry)
				codec.WriteError(w, http.StatusTooManyRequests, "rate_limit_exceeded",
					"token rate limit exceeded for this API key")
				return
			}
			rate = &rateCommit{
				limiter: limiter, keyID: authCtx.KeyID, limit: authCtx.TPMLimit,
				reserved: est, store: db, logger: logger,
			}
		}

		// 终身 token 配额**原子预占**（DESIGN §11.2）。
		//
		// 原实现是纯查询（GetKeyQuota）：读 used → 比较 → 放行。并发下多个
		// 请求读到同一个 used 并**全部通过** —— 注释说「容忍至多一个在途超发」，
		// 那只在串行时成立：剩余额度 1000 token 时，50 个并发请求各预估 200
		// token 会全部放行，超发数十倍。
		//
		// ReserveQuota 把「检查」与「占用」合并进同一条写事务，并发请求被
		// SQLite 写锁串行化，第二个进来时读到的 used+reserved 已含第一个的预占。
		// 预占落在独立的 reserved_tokens 列（不动 used_tokens —— 真实用量由
		// usage 落库触发器累加，混写会让一次请求被记两次），收尾时由
		// rateCommit 原额退回（commit → releaseQuota）。
		//
		// 这里只借 GetKeyQuota 判「是否配置了额度」（quota>0，决定要不要做
		// 估算与预占），真正的额度判定（used+reserved+est<=quota）在
		// ReserveQuota 事务内做。2026-10-07 之前这里把第一个返回值（quota）
		// 丢弃、名叫 quota 的变量绑到的是第二个返回值（used），判定变成
		// 「已用量>0」—— 全新 key 的终身配额因此完全不生效（P0-1）。
		//
		// 为什么不套用 ratelimit 的窗口限速器：TPM 限**速率**（分钟窗口一翻
		// 自然释放），配额限**终身累计**（没有「窗口结束」）。两者语义不同，
		// 拿窗口限速器去限终身额度会在窗口翻转时凭空释放额度。
		if quota, _, _, qerr := db.GetKeyQuota(r.Context(), authCtx.KeyID); qerr != nil {
			// 查询抖动 fail-open：读池故障不该变成流量全拒。但记 ERROR——
			// fail-open 的代价是真超发，无声无息就查不到了。
			logger.Error("quota lookup failed (fail-open)", "error", qerr, "key_id", authCtx.KeyID)
		} else if quota > 0 {
			if !needEstimate {
				est = estimateRequestTokens(ing.buildRosetta())
			}
			reserved, rok, rerr := db.ReserveQuota(r.Context(), authCtx.KeyID, est)
			switch {
			case rerr != nil:
				logger.Error("quota reserve failed (fail-open)",
					"error", rerr, "key_id", authCtx.KeyID)
			case !rok:
				logger.Warn("quota exceeded", "key_id", authCtx.KeyID,
					"estimated_tokens", est,
					"request_id", server.RequestIDFromContext(r.Context()))
				codec.WriteError(w, http.StatusTooManyRequests, "insufficient_quota",
					"this API key has exhausted its token quota")
				return
			default:
				if rate == nil {
					rate = &rateCommit{limiter: limiter, keyID: authCtx.KeyID, store: db, logger: logger}
				}
				rate.quotaReserved = reserved
			}
		}

		snap := snapshot.Get()
		res, err := snap.Routes.Resolve(ing.model)
		if err != nil {
			rate.commit(0)
			codec.WriteError(w, http.StatusNotFound, "model_not_found", fmt.Sprintf("model %q not found", ing.model))
			return
		}
		route := res.Route

		// 模型白名单（多用户改造 P1）：组白名单 ∩ key 白名单，任一非空即生效。
		//
		// 判定用的 `route.PublicName` 有两个来源，必须都覆盖到：
		//   - 具名路由命中 → 就是该 route 的公共名；
		//   - `provider/model` 直连形式 → routing.Resolve 会**合成**一条 route，
		//     其 PublicName 就是请求原文。
		//
		// 也就是说这条判定等价于「按客户端实际写的那个标识符查白名单」。
		// 这正是我们要的：如果只按具名路由判定而放行直连形式，受限用户只要
		// 改写成 `provider/model` 就能拿到白名单之外的上游模型 —— 白名单形同虚设。
		// 管理员若确实要放开某个直连标识符，把它本身写进白名单即可
		// （/v1/models?include=upstream 会列出这些标识符供挑选）。
		//
		// 位置排在限速与配额预检之后、触碰上游之前：白名单判定只是内存里的
		// 线性扫描，比两次数据库预检便宜；放前面会让「超额度的请求」拿到 403
		// 而不是 429，掩盖更该先解决的配额问题。两者都不打上游，
		// 顺序只影响错误码优先级。
		if !authCtx.AllowsModel(route.PublicName) {
			rate.commit(0)
			logger.Warn("model not allowed for caller",
				"model", route.PublicName, "key_id", authCtx.KeyID, "user_id", authCtx.UserID,
				"request_id", server.RequestIDFromContext(r.Context()))
			codec.WriteError(w, http.StatusForbidden, "model_not_allowed",
				fmt.Sprintf("model %q is not available for this API key", ing.model))
			return
		}

		// 整请求总预算（2026-10-10 修复的 P2）。
		//
		// 放在这里是因为：此刻配额已经预占、路由与白名单都已判定，再往后就是
		// 真正开始打上游了 —— 而「打上游」是唯一会长时间挂住的阶段。放在
		// 更早会让纯本地的判定（解码、限速）也背上这个 deadline，白白消耗预算。
		//
		// 绑到 r 上而不是局部 ctx：故障转移循环、配额收尾（rate.commit）、
		// 用量落库都从 r 派生 context，让整条请求共享同一个预算。
		//
		// 为什么不能靠 WriteTimeout 兜底：它是**每个写操作**的间隔上限，
		// 不是请求总时长；而流式响应天生长时间不写，套上去会掐死正常长流
		// （main 里 WriteTimeout 为 0 的注释解释的正是这件事）。
		//
		// 到期后表现：由上游 ctx 传播为 error，各 attempt 的错误映射会把它
		// 归类为传输层失败（可转移），链耗尽后回 504。已提交的流此时靠
		// idle 看门狗收尾，不会出现「写了头却没有终止信号」。
		budget := totalRequestBudget(snap, cfg)
		ctx, cancelBudget := context.WithTimeout(r.Context(), budget)
		defer cancelBudget()
		r = r.WithContext(ctx)
		logger.Debug("request total budget armed",
			"budget_ms", budget.Milliseconds(),
			"model", ing.model,
			"request_id", server.RequestIDFromContext(r.Context()))

		// 余额预检（Lead 与用户确认的口径：预检拒绝，不做预占/退款）。
		//
		// 位置：白名单之后、触碰上游之前。理由与上面的白名单同级 —— 白名单
		// 判定只是一次内存线性扫描（零 IO），而余额预检要**查库**；把便宜的
		// 判定放前面，403 的语义就不会被 402 掩盖（一个无权用的模型，不该先
		// 让调用方看到"你没钱"）。
		//
		// 位置与 token 配额预检（ReserveQuota，在 Resolve 之前）**不同**是有
		// 意的：配额预检只要一个 est（估 token），不需要知道价格；而余额预检
		// 必须知道单价，单价挂在**上游模型**上 → 必须先 Resolve 才知道这条链
		// 上有哪些模型、各自多贵。两者都不打上游，顺序只影响错误码优先级。
		//
		// 与配额预检的另一处不同：**不做预占**。余额是浮点量，估算与真实值
		// 必然不等，预占了就得退款（等于把 ReserveQuota/ReleaseQuota 那套再
		// 实现一遍，而那正是 2026-10-07 的 P0-2 教训）。所以这里是纯查询。
		if bal := precheckBalance(w, r, db, logger, authCtx, ing, res.Candidates, codec); !bal {
			// 余额预检已写回 402/500。rate.commit(0) 不能省：走到这里说明
			// TPM/配额预占可能已经立起来了（needEstimate 为真时），不退的话
			// 它们会一直占着窗口/终身额度直到过期。与上面 model_not_allowed
			// 的收尾同口径。
			rate.commit(0)
			return
		}

		// 尝试预算：链上按 position 升序最多打 maxTargets 个目标。
		// 未开启故障转移 → 只打主目标（等价于改造前的单目标行为，零回归）。
		// 策略值统一来自「设置」页写入的全局默认（快照），config 只作兜底。
		cands := res.Candidates
		limit := len(cands)
		if !route.FailoverEnabled {
			limit = 1
		} else if budget := failoverMaxTargets(snap, cfg); budget < limit {
			limit = budget
		}

		// 开故障转移时，跳过正被熔断的目标（若全被熔断则退回整条链，宁可打也不 404）。
		//
		// 这一步只做**查询**（TargetAvailable 已是纯函数），不占探测名额：
		// 紧接着的预算截断会把排到 failover_max_targets 之外的目标丢掉，
		// 它们永远进不了下面的请求循环，也就没人替它们释放名额。
		// 名额统一在循环内、真的要发请求前由 ClaimTargetProbe 领取。
		active := cands
		if route.FailoverEnabled {
			var avail []routing.Candidate
			for _, c := range cands {
				if pool.TargetAvailable(c.TargetID) {
					avail = append(avail, c)
				}
			}
			if len(avail) > 0 {
				active = avail
			}
		}

		// 硬约束过滤（2026-10-07 P1-8）：请求要求结构化输出（json_object /
		// json_schema）而候选上游是 anthropic 协议时，跳过该候选。
		//
		// 为什么不能放行：Anthropic 没有 response_format / text.format 的对应物，
		// inwire 层已无处可翻；照发等于接受「200 但约束静默丢失」——尤其恶劣的
		// 是它让故障转移改变语义：同一请求打 openai-chat 目标时约束生效，转移
		// 到 anthropic 目标后仍回 200 但约束消失。取舍：跳过候选比谎报成功更
		// 安全 —— 客户端拿到明确错误，至少知道该改请求或改路由，而不是拿着
		// 「看起来成功」的无约束输出去下游消费。
		//
		// "auto" / "" / openai 系协议不受影响：openai-chat 与 openai-responses
		// 都能表达这两类约束（inwire 层已做等价翻译）。放在熔断筛选之后、预算
		// 截断之前：先剔除服务不了的目标，让 failover_max_targets 的预算花在
		// 真正可服务的候选上。
		if ing.requiresStructuredOutput {
			var serviceable []routing.Candidate
			for _, c := range active {
				if c.Provider != nil && c.Provider.Protocol != "anthropic" {
					serviceable = append(serviceable, c)
				}
			}
			if len(serviceable) == 0 {
				rate.commit(0)
				logger.Warn("no upstream can serve structured output request",
					"model", ing.model, "key_id", authCtx.KeyID,
					"candidates", len(active),
					"request_id", server.RequestIDFromContext(r.Context()))
				codec.WriteError(w, http.StatusBadRequest, "invalid_request_error",
					"this request requires structured output (json_object/json_schema), "+
						"but no candidate upstream can serve it: the anthropic protocol has no equivalent of response_format")
				return
			}
			active = serviceable
		}
		if len(active) > limit {
			active = active[:limit]
		}

		threshold := failoverFailureThreshold(snap, cfg)
		keyID := authCtx.KeyID

		var out attemptOutcome
		// lastTried 记录实际尝试到哪个候选（链耗尽时据此归因，不猜链上最后一个）。
		// triedAny 与之配对：Candidate 是值类型，「没试过」与「试过零值」无法
		// 从 lastTried 本身区分，必须有独立布尔。
		var lastTried routing.Candidate
		var triedAny bool
		for i, cand := range active {
			isLast := i == len(active)-1

			// 客户端已断开就收手：半路跑掉的人不该消耗整条链的下游配额，
			// 也不该把一次在途取消误记成目标的失败。此刻尚未写出任何字节，
			// 直接返回、不记 error usage（断流是客户端行为，不是上游故障）。
			// 此处还没领探测名额，不存在归还义务。
			if cerr := r.Context().Err(); cerr != nil {
				logger.Info("client disconnected, aborting failover",
					"model", ing.model, "attempt", i+1, "error", cerr,
					"request_id", server.RequestIDFromContext(r.Context()))
				rate.commit(0)
				return
			}

			// 真正要打这个目标时才领 half-open 探测名额。放在这里（而不是
			// 上面的筛选里）是因为名额要与「随后的成功/失败记账」一一配对 ——
			// 提前领取会让被预算裁掉的目标永久占住名额。
			//
			// 名额有两条出路：记账释放（Record*），或没走到记账时由循环尾部
			// 的 ReleaseTargetProbe 显式归还（probeClaimed/probeRecorded 就是
			// 这对配对标志）。缺了后一条的话，不可转移错误与客户端中断这两种
			// 合法形态会把名额永久占死 —— 一次 400 就废掉一个目标。
			//
			// 非主目标且没开故障转移时不用熔断器管，行为与改造前一致。
			probeClaimed, probeRecorded := false, false
			if route.FailoverEnabled {
				if !pool.ClaimTargetProbe(cand.TargetID) {
					// 仍在冷却中，或探测名额已被同链的并发请求领走 —— 沿链继续。
					// 没领到名额，也就没有归还义务。
					if !isLast {
						logger.Warn("failover: target not claiming a probe slot, switching target",
							"model", ing.model, "provider", cand.Provider.Slug, "attempt", i+1,
							"request_id", server.RequestIDFromContext(r.Context()))
						continue
					}
					out = attemptOutcome{eligible: true, statusCode: http.StatusBadGateway,
						code: "upstream_error", message: "no available upstream provider"}
					break
				}
				probeClaimed = true
			}

			// 一次 attempt 只取该 provider 的一把凭据：某把 key 失败时本请求不就地换
			// 同 provider 的下一把，而是让位给链上下一个目标。跨请求的 key 轮换交给
			// 冷却 —— 坏 key 被踢出 healthy 后，下个请求自会选到好 key。这是有意取舍，
			// 免得「一个请求把某 provider 所有 key 各打一遍」放大延迟与配额消耗。
			client, credID, cerr := pool.GetAnyClient(cand.Provider.Slug)
			if cerr != nil {
				// 该目标当前无健康凭据：累计目标失败，换链上下一个。
				pool.RecordTargetFailure(cand.TargetID, threshold)
				probeRecorded = true // 名额由记账释放
				out = attemptOutcome{eligible: true, statusCode: http.StatusBadGateway,
					code: "upstream_error", message: "no available upstream provider"}
				if !isLast {
					logger.Warn("failover: no healthy credential, switching target",
						"model", ing.model, "provider", cand.Provider.Slug, "request_id", server.RequestIDFromContext(r.Context()))
					continue
				}
				break
			}

			if ing.stream {
				out = attemptStream(w, r, client, ing, cand, authCtx, codec, rate, cfg, snap, usage, start)
			} else {
				out = attemptNonStream(w, r, client, ing, cand, authCtx, codec, rate, cfg, snap, usage, start)
			}
			// 记住**实际尝试到**的候选，供链耗尽时记账归因。
			// 不能用 active[len(active)-1] —— 那是链上最后一个候选，而实际最后
			// 尝试的可能是链上第一个（failover_max_targets=1、或前面的目标被
			// 跳过时）。归因错目标会让排障指向一个从未真正打过的上游。
			lastTried = cand
			triedAny = true

			if out.committed {
				// 只有真成功才记成功 —— 断流/溢出/上游错误虽已提交，却是失败，
				// 必须让目标熔断计数与凭据健康照常累计，否则故障永不转移。
				if out.success {
					pool.RecordCredentialSuccess(credID)
					pool.RecordTargetSuccess(cand.TargetID)
				} else {
					pool.RecordTargetFailure(cand.TargetID, threshold)
				}
				probeRecorded = true // 名额由记账释放
				return
			}

			// 未写出任何下游字节才可能转移；此处按分类回写凭据冷却与目标熔断计数。
			//
			// credCooldown==0 表示这次失败不该罚凭据（当前只有 404/410：目标服务不了
			// 这个模型，key 本身是好的）—— 只累计目标熔断。旧实现在这个分支调
			// MarkCredentialError，既不改库也不参与健康过滤，是个纯日志空操作，已删。
			//
			// 客户端在调用进行中断开时必须跳过全部记账：SDK 会把 context 取消包成
			// TransportError，落进可转移集合，若照记就会把一把健康凭据冷却 60s、并
			// 累计目标熔断 —— 单 key provider 会被几次用户中断搞成整体不可用
			// （凭据冷却期内不再被选中，也就没有任何请求能成功以触发复苏）。
			// r.Context() 只在客户端断开/服务关停时取消（上游超时用的是派生 ctx），
			// 因此这个判据能精确区分「客户端跑了」与「上游真的坏了」。
			if out.eligible && r.Context().Err() == nil {
				pool.RecordTargetFailure(cand.TargetID, threshold)
				probeRecorded = true // 名额由记账释放
				if out.credCooldown > 0 {
					pool.MarkCredentialCooldown(credID, out.credCooldown)
				}
			}

			// 没走到记账的两种形态 —— 不可转移错误（out.eligible==false）与
			// 客户端中断（ctx 已取消）—— 在此归还探测名额，否则 halfOpen 卡死、
			// 该目标被跳到下一次 Install 才复位。已记账的绝不能再还：
			// halfOpen 是共享 bool 而非计数，重复归还会错清并发请求刚领到的
			// 名额（见 ReleaseTargetProbe 的注释）。
			if probeClaimed && !probeRecorded {
				pool.ReleaseTargetProbe(cand.TargetID)
			}

			if !out.eligible || isLast {
				break
			}
			logger.Warn("failover: switching to next target",
				"model", ing.model, "from_provider", cand.Provider.Slug,
				"error_code", out.code, "attempt", i+1,
				"request_id", server.RequestIDFromContext(r.Context()))
		}

		// 走到这里 = 最后一次尝试未提交（要么不可转移错误、要么链已耗尽）。
		// 写出最终错误并记一条 usage —— 若 active 为空（理论不该发生）兜底成 502。
		// 客户端已经断开时它根本收不到这个响应（写了只会撞 broken pipe），
		// 且这次失败与上游无关，按 canceled 记账，与流式路径同一口径。
		clientGone := r.Context().Err() != nil

		statusCode, code, message := out.statusCode, out.code, out.message
		// 兜底：不许把 200 或 0 当作「失败结果」发出去。
		//
		// statusCode==0 是「没有候选目标」的哨兵，一直有 502 兜底。
		// statusCode==200 则是**本该不可能**的状态：这条路径只在链已耗尽、
		// 响应尚未提交时到达，任何失败都必须用 >=400 表达。修复前它真的发生过
		// （见 outcomeFromErr 的注释：流式哨兵被映射成 200），客户端于是拿到
		// 一个「成功但内容为空」的响应。把这一并归入 502，是为了让这条不变量
		// 在**写出响应**这个最后一关口上也有牙齿 —— 将来谁再引入一个返回 200
		// 的失败分支，症状会是「这里莫名其妙变成 502」，而不是「客户端拿到
		// 假成功且无人察觉」。
		if statusCode == 0 || statusCode == http.StatusOK {
			statusCode, code, message = http.StatusBadGateway, "upstream_error", "no available upstream provider"
		}
		if !clientGone {
			codec.WriteError(w, statusCode, code, message)
		}
		rate.commit(0)
		provID, upstreamModel := "", ""
		// 归因到**实际尝试过**的目标，而不是 active 的最后一个：
		// failover_max_targets=1、或前面的候选被跳过时，两者不是同一个。
		// Provider 判空：active 为空（理论不该发生）时 lastTried 是零值
		// Candidate，解引用 nil Provider 会 panic —— 一行防御，零业务成本。
		if triedAny && lastTried.Provider != nil {
			provID, upstreamModel = lastTried.Provider.ID, lastTried.UpstreamModel.ModelID
		}
		status := "error"
		if clientGone {
			status = "canceled"
			logger.Info("client disconnected before any response was written",
				"model", ing.model, "request_id", server.RequestIDFromContext(r.Context()))
		}
		errorCode := code
		if clientGone {
			errorCode = ""
		}
		usage.record(&store.UsageRecord{
			ID:              generateID(),
			AccessKeyID:     keyID,
			UserID:          authCtx.UserID,
			RequestID:       server.RequestIDFromContext(r.Context()),
			PublicModel:     ing.model,
			ProviderID:      provID,
			UpstreamModel:   upstreamModel,
			IngressProtocol: codec.Name(),
			Stream:          ing.stream,
			UsageState:      "none",
			Status:          status,
			HTTPStatus:      statusCode,
			ErrorCode:       errorCode,
			LatencyMs:       time.Since(start).Milliseconds(),
		})
	}
}

// attemptOutcome 是一次「对某个链目标发起上游调用」的结果。
//
// committed=true 表示已向下游写出内容（非流式的完整响应、或流式已发首字并写完整个流），
// 此时 usage 已由该次 attempt 落库，请求终结，绝不能回退去换目标（半条流收不回）。
// committed=false 时 attempt 未碰过 ResponseWriter，由外层循环决定「换下一个目标」还是
// 「把这次错误写回客户端」。eligible 告诉外层这次失败值不值得转移。
type attemptOutcome struct {
	committed bool
	// success 表示本次 attempt 真的成功抵达上游（流式仅 status=="ok" 为真）。
	// committed 只说明「字节已写出、不能回退」，断流/溢出同样是 committed
	// 却是失败 —— 熔断与凭据健康必须看success，不能看 committed。
	success      bool
	eligible     bool
	credCooldown time.Duration
	statusCode   int
	code         string
	message      string
}

// outcomeFromErr 把一次上游 err 归类成「未提交」的结果。
//
// # 流式哨兵必须在**这里**单独处理（2026-10-10 修复的 P1）
//
// outwire.MapUpstreamError 把 ErrStreamTruncated / ErrStreamOverflow 映射成
// (200, "", "")，理由是「内容已经写到线上去了，没什么可补的」。这个前提
// **只在已提交时成立**，而本函数的每一个调用点都是**未提交**路径
// （见 attemptStream 的 !gotFirst 分支：SSE 头要等到第一个事件到达才写）。
//
// 于是修复前：上游在首个事件前硬失败 → 拿到 (200,"","") → statusCode 非 0
// 所以 main 的 502 兜底不生效 → 客户端收到
// `{"error":{"message":"","type":"api_error"}}` 且 **HTTP 200**。
// 两个后果叠加：
//   - OpenAI/Anthropic SDK 只在 >=400 时抛错，于是调用方拿到一个空字符串和
//     「成功」；按状态码记账的监控把它记成一条成功调用。
//   - FailoverEligible 对这两个哨兵返回 false，故障转移链直接 break ——
//     一个本该被链吸收的失败，反而被伪造成成功返回。
//
// 所以在这里改判：既给出真实的状态码，也让链继续尝试下一个目标。
// MapUpstreamError 本身**保持原样** —— 它服务的已提交路径（「如实 truncated
// 收尾，不追加错误体」）那个语义是对的，不该为迁就未提交路径而改掉。
func outcomeFromErr(err error) attemptOutcome {
	if errors.Is(err, rosetta.ErrStreamTruncated) || errors.Is(err, rosetta.ErrStreamOverflow) {
		return attemptOutcome{
			// 首批事件之前就断流 = 这次尝试什么也没产出，值得换目标再试。
			eligible:     true,
			credCooldown: 60 * time.Second,
			statusCode:   http.StatusBadGateway,
			code:         "upstream_error",
			message:      "upstream stream failed before the first event",
		}
	}
	eligible := outwire.FailoverEligible(err)
	statusCode, code, message := outwire.MapUpstreamError(err)
	return attemptOutcome{
		eligible:     eligible,
		credCooldown: outwire.CredentialCooldown(err),
		statusCode:   statusCode,
		code:         code,
		message:      message,
	}
}

// failoverMaxTargets 解析「一次请求最多尝试链上几个目标」：
// 设置页写入的全局默认优先，未配置（0）回落 config。
func failoverMaxTargets(snap *snapshot.Snapshot, cfg *config.Config) int {
	if snap != nil && snap.Runtime.FailoverMaxTargets > 0 {
		return snap.Runtime.FailoverMaxTargets
	}
	return cfg.FailoverMaxTargets()
}

// failoverFailureThreshold 解析「某目标连续失败几次即熔断」：设置页优先，回落 config。
func failoverFailureThreshold(snap *snapshot.Snapshot, cfg *config.Config) int {
	if snap != nil && snap.Runtime.FailoverFailureThreshold > 0 {
		return snap.Runtime.FailoverFailureThreshold
	}
	return cfg.FailoverFailureThreshold()
}

// safeMillis 把毫秒数转成 time.Duration，并钳制在不会整数回绕的范围内。
//
// 为什么要钳制而不只是校验：校验（config.validate + settings_handler）挡住的是
// **新写入**的脏值，挡不住数据库里已经存在的、或在别的部署路径下写进去的。
// 而回绕的后果是灾难性的 —— time.Duration 是 int64 纳秒，
// `time.Duration(ms) * time.Millisecond` 在 ms > 9.223e12 时得到**负数**：
//
//	context.WithTimeout(ctx, 负值)  → deadline 立即过期 → 非流式请求全挂
//	time.AfterFunc(负值, ...)      → 立即开火 → 流刚发头就被自己关掉
//
// 而 UI 上显示的是一个「巨大但合法」的超时值，没有任何异常信号。
// 24 小时足够任何网关用，且离溢出点有 380 倍余量。
func safeMillis(ms int) time.Duration {
	const maxMillis = 86_400_000 // 24h
	if ms <= 0 {
		return 0
	}
	if ms > maxMillis {
		slog.Warn("runtime millisecond setting exceeds safe range, clamping",
			"ms", ms, "clamped_to_ms", maxMillis)
		return time.Duration(maxMillis) * time.Millisecond
	}
	return time.Duration(ms) * time.Millisecond
}

// nonStreamTimeout：设置页的全局默认优先，其次 config 的上游超时。
func nonStreamTimeout(snap *snapshot.Snapshot, cfg *config.Config) time.Duration {
	if snap != nil && snap.Runtime.UpstreamTimeoutMs > 0 {
		return safeMillis(snap.Runtime.UpstreamTimeoutMs)
	}
	return cfg.UpstreamTimeout()
}

// firstTokenTimeout：设置页的全局默认优先，其次 config 的首字超时。
func firstTokenTimeout(snap *snapshot.Snapshot, cfg *config.Config) time.Duration {
	if snap != nil && snap.Runtime.StreamFirstTokenTimeoutMs > 0 {
		return safeMillis(snap.Runtime.StreamFirstTokenTimeoutMs)
	}
	return cfg.StreamFirstTokenTimeout()
}

// streamIdleTimeout：设置页的全局默认优先，其次 config 的流式空闲超时。
func streamIdleTimeout(snap *snapshot.Snapshot, cfg *config.Config) time.Duration {
	if snap != nil && snap.Runtime.StreamIdleTimeoutMs > 0 {
		return safeMillis(snap.Runtime.StreamIdleTimeoutMs)
	}
	return cfg.StreamIdleTimeout()
}

// totalRequestBudget 是**整个 /v1 请求**（跨所有故障转移尝试）的总上限。
//
// # 为什么需要它（2026-10-10 修复的 P2）
//
// 此前没有任何东西限制一次请求的总时长，而每个环节的超时都是**逐跳**的：
//
//	拨号/TLS 10s + 等响应头 60s + 首字 30s  ≈ 100s / 次尝试
//	× (failover_max_targets 默认 3)        ≈ 300s
//
// 于是「一串半死的上游」可以把客户端挂住约 5 分钟。期间它一直占着：一个
// 下游连接、一个上游连接、一条配额预占行、一个故障转移槽位。攒够几个，
// 网关对新请求而言已经不可用，而监控上看它「还活着」——
// 病在上游，症状却在网关自己身上。
//
// # 为什么由现有配置推导，而不是新增一个「总超时」开关
//
// 总预算必须**大于**任何单跳预算（故障转移会逐次复用单跳预算），
// 又必须**有限**。直接推导的好处是：运维调过的 upstream_timeout_ms、
// stream_first_token_timeout_ms、failover_max_targets 立刻反映到总预算上，
// 不需要「调完设置还要记得同步另一个总开关」这种跨字段的隐性依赖；
// 也不存在「总超时配得比单跳还短」这种自相矛盾的配置。
//
// # 公式与余量的来历
//
// 每次尝试的最坏上界取「非流式超时」与「首字 + 响应头等待」中较大的一个 ——
// 二者是不同路径，取 max 才不会把流式或非流式其中一条判短了。
// 再乘 (目标数+1)：+1 是给「最后一个目标失败后写错误响应」留的额度
// （failover_max_targets 限制的是尝试的**成功**目标数）。
// 每跳再加 perAttemptSlack：给「取流、解析、写头、落库」这些不计入任何
// 看门狗的开销留余量 —— 余量不足的表现是总预算先把健康请求掐掉，
// 那是比「慢」严重得多的事故。
//
// 非流式请求同样套用：它们的单跳预算就是 upstream_timeout_ms，
// 逐跳累加的后果完全一样。
func totalRequestBudget(snap *snapshot.Snapshot, cfg *config.Config) time.Duration {
	// 逐跳上界：两条路径取较大者。responseHeaderWait 是等上游响应头的上限，
	// 它在**首字看门狗启动之前**发生，所以首字预算管不到它，必须单独算。
	perAttempt := nonStreamTimeout(snap, cfg)
	if first := firstTokenTimeout(snap, cfg) + responseHeaderWait; first > perAttempt {
		perAttempt = first
	}
	perAttempt += perAttemptSlack

	attempts := failoverMaxTargets(snap, cfg) + 1
	budget := time.Duration(attempts) * perAttempt

	// 下限保护：即便所有配置都被设成极小值，也要留出一个不至于让正常请求
	// 必然失败的地板。上限保护同理：配置被设得极大时，总预算不应该大到
	// 「等于没有」——那正是本函数要消灭的状态。
	const (
		minTotalBudget = 30 * time.Second
		maxTotalBudget = 30 * time.Minute
	)
	if budget < minTotalBudget {
		return minTotalBudget
	}
	if budget > maxTotalBudget {
		return maxTotalBudget
	}
	return budget
}

const (
	// responseHeaderWait 镜像 internal/upstream 里 ResponseHeaderTimeout 的
	// 60s。它出现在这里是因为总预算必须算上「拿到响应头之前」的那段 ——
	// 而首字看门狗是在 ChatStream 返回**之后**才启动的，覆盖不到这里。
	//
	// 两处必须同步改：改 upstream 的同时改这里，否则总预算会算少一段。
	responseHeaderWait = 60 * time.Second
	// perAttemptSlack 是每跳的固定余量，覆盖不计入任何看门狗的开销：
	// 取流、心跳、协议转换、落库。给小了会让总预算提前掐掉健康请求。
	perAttemptSlack = 5 * time.Second
)

// wantsStreamUsage reports whether the caller asked for a trailing usage
// chunk, i.e. OpenAI's {"stream_options":{"include_usage":true}}. The flag
// only controls what the gateway forwards downstream: the SDK already asks
// the upstream for usage on every stream.
func wantsStreamUsage(req *inwire.OpenAIChatRequest) bool {
	return req.StreamOptions != nil &&
		req.StreamOptions.IncludeUsage != nil &&
		*req.StreamOptions.IncludeUsage
}

// attemptStream 尝试在当前目标上完成一次流式响应。
//
// 关键：SSE 头与状态码**推迟到拿到第一个上游事件之后才写**。这样「建立失败」
// 和「首字迟迟不来」都发生在向下游写出任何字节之前，可安全地让外层循环换目标；
// 一旦写了头并提交首个事件，就再无回退余地（DESIGN §414 的约束）。
//
// 本函数只做与协议无关的事：看门狗、心跳、事件循环、断流状态归类、落库；
// 「统一事件 → 下游协议分片」全部经由 codec.NewSink 的 StreamSink 完成。
func attemptStream(w http.ResponseWriter, r *http.Request, client *rosetta.Client, ing *ingressRequest, cand routing.Candidate, authCtx *auth.Context, codec ingressCodec, rate *rateCommit, cfg *config.Config, snap *snapshot.Snapshot, usage *usageRecorder, start time.Time) attemptOutcome {
	ctx := r.Context()
	logger := usage.logger
	publicModel := ing.model

	// 每个 attempt 现造一份请求，并把公共别名换成该目标的上游 model_id、
	// 挂上该上游协议的私有透传字段。必须在 attempt 内部做（而不是在调用点
	// 造好传进来）：链上各目标的协议不同，Extra 形状也不同，复用同一实例
	// 会把 A 目标的字段泄漏给 B 目标。
	upstreamReq := ing.buildRosetta()
	upstreamReq.Model = cand.UpstreamModel.ModelID
	ing.applyUpstreamExtras(upstreamReq, cand.Provider.Protocol)
	ing.applyThinkingCapability(upstreamReq, cand.UpstreamModel, cand.Provider.Protocol)

	// 首字预算只覆盖「等首个事件」这一段；建立连接那段交给 ResponseHeaderTimeout
	// 与下面的总请求预算（2026-10-10 修复的 P2）。
	//
	// # 为什么**不**给 ChatStream 套一个带超时的子 context
	//
	// 直觉上「连接也该算进首字预算」，做法是
	//
	//	preCtx, cancel := context.WithTimeout(ctx, ttftTimeout)
	//	stream, err := client.ChatStream(preCtx, req)
	//	defer cancel()   // ← 危险，见下
	//
	// 但这个做法**会掐断正在正常输出的长流**，已实测确认：
	// rosetta 的 ChatStream 内部是 `WithCancel(ctx)` 后把 ctx 直接交给
	// provider.StreamChat，最终 `http.Do(ctx, call)`（SDK 内没有
	// WithoutCancel / detach），所以 ctx 一被取消，net/http 立刻关闭响应体。
	// 首字到达后再 cancel，等于在流输出到一半时把它掐了 ——
	// 客户端拿到半截回答，且 status 无法表达（头已经写出去了）。
	// 实测：发 4 个事件、首个之后取消 → 只读到 2 个，err=context canceled；
	// 不取消的对照组 4 个全读完。`defer cancel()` 更糟，它要等整个
	// attemptStream 返回才执行，等于把流的后半程一起掐掉。
	//
	// 所以首字预算**只**由下面的 ttftTimer 承担，它的作用范围天然是
	// 「拿到流对象之后、首个事件之前」—— 这一段是安全的：此时尚未写出任何
	// 字节，取消/关流都属于「未提交」，外层仍可转移。
	//
	// 「拨号 + 等响应头」这段仍由 internal/upstream 的
	// ResponseHeaderTimeout（60s，镜像为 responseHeaderWait）兜底，
	// 并计入 totalRequestBudget —— 它不会无限，只是比首字预算宽。
	ttftTimeout := firstTokenTimeout(snap, cfg)

	stream, err := client.ChatStream(ctx, upstreamReq)
	if err != nil {
		return outcomeFromErr(err)
	}
	defer stream.Close()

	// 首字（TTFT）看门狗：只掐「一个事件都没等到」的慢上游，触发即关流，
	// 尚未写头 → 未提交 → 外层可转移。与下面的 idle 看门狗是两回事。
	var ttftTimedOut atomic.Bool
	// ttftDone 让「定时器已触发」这件事变成可等待的：Stop() 返回 false 只说明
	// 回调已经**开始**跑（或跑完），不保证 stream.Close() 已落地。这个 channel
	// 由回调关闭，用于消除那个窗口。
	ttftDone := make(chan struct{})
	ttftTimer := time.AfterFunc(ttftTimeout, func() {
		ttftTimedOut.Store(true)
		_ = stream.Close()
		close(ttftDone)
	})
	gotFirst := stream.Next()

	// **必须看 Stop() 的返回值**：返回 false = 定时器已触发，而回调里的
	// stream.Close() 可能刚刚或即将执行。此时即便 gotFirst 为真，流也已经/
	// 即将被关掉，若继续往下走就会写 200 与 SSE 头，产出「空但 ok」的假正常流
	// —— 客户端拿到 200 与正确的响应头，正文却是空的，且状态码无法表达失败。
	// 等 ttftDone 闭合，确保 Close 一定已发生，再按「超时」处理。
	ttftFired := !ttftTimer.Stop()
	if ttftFired {
		<-ttftDone
	}

	if !gotFirst || ttftTimedOut.Load() {
		if cerr := stream.Err(); cerr != nil {
			return outcomeFromErr(cerr)
		}
		if ttftTimedOut.Load() {
			logger.Warn("stream first-token timeout", "model", publicModel,
				"provider", cand.Provider.Slug, "ttft_timeout_ms", ttftTimeout.Milliseconds())
			return attemptOutcome{eligible: true, credCooldown: 60 * time.Second,
				statusCode: http.StatusGatewayTimeout, code: "upstream_timeout",
				message: "upstream first-token timeout"}
		}
		// 干净 EOF 却在首个事件之前（上游返回 200 后立刻空流）—— 按可转移的空响应处理。
		return attemptOutcome{eligible: true, statusCode: http.StatusBadGateway,
			code: "upstream_error", message: "upstream returned empty response"}
	}

	// —— 首个事件已到，自此提交：写 SSE 头，之后任何中断都只能如实 truncated 收尾 ——
	var ttfbMs int64
	ttfbMs = time.Since(start).Milliseconds()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	// 流式编码器由 codec 构造：OpenAI 走 SSEWriter，Anthropic 走块状态机。
	sink := codec.NewSink(w, publicModel)

	idleTimeout := streamIdleTimeout(snap, cfg)
	var idleTimedOut atomic.Bool
	// lastEventAt 记最近一次上游事件到达的时刻，idle 回调据此**复核**后再关流。
	//
	// 只靠 Timer.Reset 不够：Reset 追不回已经派发的回调，超时边界上「事件按时
	// 到达、回调却已起跑」会把健康流误杀成 stream_idle_timeout —— 与 TTFT
	// 看门狗已用 ttftDone 修掉的是同一类竞态，但 idle 路径此前仍在。
	// 复核判据取「回调执行时刻距最近事件是否真的已满 idleTimeout」：真空闲时
	// 必然成立；边界竞速中到达的事件让复核不通过，此时直接退出 —— consume
	// 里的 Reset 已把看门狗续上，流继续。
	// lastEventAt 必须在 Reset **之前**更新：反过来会让回调读到旧时刻，
	// 把「事件刚到」误判成「已空闲整段超时」。
	var lastEventAt atomic.Int64
	lastEventAt.Store(time.Now().UnixNano())
	idleTimer := time.AfterFunc(idleTimeout, func() {
		if time.Since(time.Unix(0, lastEventAt.Load())) < idleTimeout {
			return // 边界竞速：事件在超时边缘合法到达，流还活着
		}
		idleTimedOut.Store(true)
		logger.Warn("stream idle timeout",
			"model", publicModel, "key_id", authCtx.KeyID,
			"idle_timeout_ms", idleTimeout.Milliseconds())
		_ = stream.Close()
	})
	defer idleTimer.Stop()

	// 心跳间隔必须严格为正：idleTimeout 为 1ms 时 idleTimeout/2 == 0，
	// time.NewTicker 会当场 panic —— 而此刻 SSE 响应头已经写出，收不回来。
	// config.validate 已挡掉负的超时配置，这里是最后一道防线。
	heartbeatInterval := idleTimeout / 2
	if heartbeatInterval <= 0 {
		heartbeatInterval = time.Second
	}
	heartbeatTicker := time.NewTicker(heartbeatInterval)
	defer heartbeatTicker.Stop()

	// 心跳只在**真的空闲**时才发。上游持续吐字时由 handleEvent 把 ticker 推后，
	// 否则一条两秒的流会连发十几条 `: keepalive` —— 对标准 SSE 客户端无害，
	// 对按行解析的下游是纯噪声。
	// 心跳 goroutine 与主循环会并发写同一个 ResponseWriter，所以统一经由 sink
	// （各实现内部有锁），不再自己 fmt.Fprintf(w, ...)。
	// 心跳 goroutine 必须**保证在 handler 返回前退出**：它与主循环并发写同一个
	// ResponseWriter，若handler 先返回、goroutine 还在 ticker 周期里，就会写一个
	// 已结束的响应（连接复用时甚至串到下一个请求）。原来的 ctx.Done() 只在
	// 客户端断开时才触发，正常完成路径上 goroutine 会一直活着。
	// quit + WaitGroup 是必须的：ticker 最多要等一个周期才退出，不能靠 defer。
	quit := make(chan struct{})
	var hbWG sync.WaitGroup
	hbWG.Add(1)
	go func() {
		defer hbWG.Done()
		for {
			select {
			case <-heartbeatTicker.C:
				sink.Keepalive()
			case <-quit:
				return
			case <-ctx.Done():
				return
			}
		}
	}()
	defer func() {
		close(quit)
		hbWG.Wait()
	}()

	var lastUsage rosetta.Usage
	var stopReason rosetta.StopReason
	status := "ok"
	errorCode := ""
	httpStatus := 200

	// consume 消费一个上游事件（首个 + 后续走同一套逻辑）：看门狗续期与
	// usage/stopReason 留档是循环的职责，协议编码全部交给 sink。
	consume := func(ev *rosetta.Event) {
		// 时刻先于 Reset 落账（顺序不能反，见 lastEventAt 声明处的注释），
		// 然后 idle 看门狗续期、心跳对齐。
		lastEventAt.Store(time.Now().UnixNano())
		idleTimer.Reset(idleTimeout)
		heartbeatTicker.Reset(heartbeatInterval)
		if ev.Type == rosetta.EventMessageEnd {
			if ev.Usage != nil {
				lastUsage = *ev.Usage
			}
			stopReason = ev.StopReason
		}
		sink.Event(ev)
	}

	consume(stream.Event())
	for stream.Next() {
		consume(stream.Event())
	}

	if err := stream.Err(); err != nil {
		switch {
		case errors.Is(err, rosetta.ErrStreamTruncated):
			status = "truncated"
			errorCode = "stream_truncated"
			logger.Warn("stream truncated", "model", publicModel, "key_id", authCtx.KeyID, "error", err)
		case errors.Is(err, rosetta.ErrStreamOverflow):
			status = "overflow"
			errorCode = "stream_overflow"
			logger.Error("stream overflow", "model", publicModel, "key_id", authCtx.KeyID, "error", err)
		case isClientGone(ctx, err):
			// 客户端主动断开不是上游故障。此前一律记 error，后果是：后台错误率虚高、
			// 成功率虚低，且真故障被 ERROR 噪音淹没。单独一个取值才能把两者分开。
			status = "canceled"
			logger.Info("client disconnected mid-stream", "model", publicModel, "key_id", authCtx.KeyID, "error", err)
		default:
			status = "error"
			errorCode = "upstream_error"
			logger.Error("stream error", "error", err, "model", publicModel, "key_id", authCtx.KeyID)
		}
	} else if idleTimedOut.Load() && !sink.SawTerminal() {
		status = "truncated"
		errorCode = "stream_idle_timeout"
		logger.Warn("stream cut by idle watchdog without terminal event",
			"model", publicModel, "key_id", authCtx.KeyID,
			"idle_timeout_ms", idleTimeout.Milliseconds(),
			"content_written", sink.WroteContent())
	} else if !sink.WroteContent() {
		logger.Warn("stream finished with no content",
			"model", publicModel, "key_id", authCtx.KeyID,
			"stop_reason", string(stopReason),
			"input_tokens", lastUsage.InputTokens,
			"output_tokens", lastUsage.OutputTokens,
			"reasoning_tokens", lastUsage.ReasoningTokens)
	}

	// 终止序列由 sink 按协议收尾：ok 时发终止事件（finish_reason+[DONE] 或
	// message_delta+message_stop），断流按 DESIGN §8.2 不发终止（OpenAI）/
	// 发 error 事件（Anthropic），canceled 不写任何东西。
	sink.Finish(status, stopReason, lastUsage, ing.wantsStreamUsage)

	latency := time.Since(start).Milliseconds()

	usage.record(&store.UsageRecord{
		ID:              generateID(),
		AccessKeyID:     authCtx.KeyID,
		UserID:          authCtx.UserID,
		RequestID:       server.RequestIDFromContext(r.Context()),
		PublicModel:     publicModel,
		ProviderID:      cand.Provider.ID,
		UpstreamModel:   cand.UpstreamModel.ModelID,
		IngressProtocol: codec.Name(),
		Stream:          true,
		InputTokens:     lastUsage.InputTokens,
		OutputTokens:    lastUsage.OutputTokens,
		TotalTokens:     lastUsage.TotalTokens,
		ReasoningTokens: lastUsage.ReasoningTokens,
		CachedTokens:    lastUsage.CachedInputTokens,
		UsageState:      usageStateFor(lastUsage),
		Status:          status,
		HTTPStatus:      httpStatus,
		ErrorCode:       errorCode,
		LatencyMs:       latency,
		TTFBMs:          ttfbMs,
	})
	// 收尾按「usage 是否已知」分两条（2026-10-07 修复的 P1-3）：
	//   - 已知：全量收尾 —— TPM 以真实 usage（输入+输出）替换估算值，
	//     配额预占退回。
	//   - missing（上游没报任何 token 数）：**只退配额预占，TPM 预占故意保留**。
	//     commit(0) 会全额退还 TPM 预占，等于不报用量的上游完全绕过 TPM；
	//     保留估算值让限速继续约束这类流量，估算值随窗口翻转自然清零。
	//     但配额预占不能跟着一起跳过 —— 它是终身累计，预占按 max_tokens 全额
	//     计，不退就等于每次 missing 请求永久吃掉数万 token 额度（修复前整个
	//     commit 被跳过的正是这条路径）。
	if !lastUsage.IsZero() {
		rate.commit(lastUsage.TotalTokens)
	} else {
		rate.releaseQuota()
	}
	// committed=true（字节已写出、无法回退换目标）与「这一次算成功」是两件事：
	// truncated / overflow / error / canceled 都已提交，但对上游而言是失败。
	// 只有 status=="ok" 才允许外层记成功，否则「先200 再断流」这类最常见的
	// 上游故障永远不累计失败、目标永不熔断。
	return attemptOutcome{committed: true, success: status == "ok"}
}

func attemptNonStream(w http.ResponseWriter, r *http.Request, client *rosetta.Client, ing *ingressRequest, cand routing.Candidate, authCtx *auth.Context, codec ingressCodec, rate *rateCommit, cfg *config.Config, snap *snapshot.Snapshot, usage *usageRecorder, start time.Time) attemptOutcome {
	publicModel := ing.model
	// 同attemptStream：别名 → 上游 model_id 的映射与协议私有字段的透传
	// 必须在本次 attempt 内完成，不能由调用方造好传入。
	upstreamReq := ing.buildRosetta()
	upstreamReq.Model = cand.UpstreamModel.ModelID
	ing.applyUpstreamExtras(upstreamReq, cand.Provider.Protocol)
	ing.applyThinkingCapability(upstreamReq, cand.UpstreamModel, cand.Provider.Protocol)

	// 绑定 r.Context() 而非 context.Background()：客户端断开时上游调用应随之取消，
	// 否则断连请求会一直占用上游连接与配额直到超时（默认 120s）。
	ctx, cancel := context.WithTimeout(r.Context(), nonStreamTimeout(snap, cfg))
	defer cancel()

	resp, err := client.Chat(ctx, upstreamReq)
	if err != nil {
		return outcomeFromErr(err)
	}

	// 非流式响应一次性返回，拿不到"首字"这一独立时刻，用响应到达时刻近似（≈总耗时）。
	latency := time.Since(start).Milliseconds()
	ttfbMs := latency

	codec.WriteNonStream(w, resp, publicModel)

	usage.record(&store.UsageRecord{
		ID:              generateID(),
		AccessKeyID:     authCtx.KeyID,
		UserID:          authCtx.UserID,
		RequestID:       server.RequestIDFromContext(r.Context()),
		PublicModel:     publicModel,
		ProviderID:      cand.Provider.ID,
		UpstreamModel:   cand.UpstreamModel.ModelID,
		IngressProtocol: codec.Name(),
		Stream:          false,
		InputTokens:     resp.Usage.InputTokens,
		OutputTokens:    resp.Usage.OutputTokens,
		TotalTokens:     resp.Usage.TotalTokens,
		ReasoningTokens: resp.Usage.ReasoningTokens,
		CachedTokens:    resp.Usage.CachedInputTokens,
		UsageState:      usageStateFor(resp.Usage),
		Status:          "ok",
		HTTPStatus:      200,
		LatencyMs:       latency,
		TTFBMs:          ttfbMs,
	})
	// 收尾口径与 attemptStream 一致（见该处的注释）：usage 已知 → 全量收尾；
	// missing → 只退配额预占，TPM 预占保留（窗口自愈）。
	if !resp.Usage.IsZero() {
		rate.commit(resp.Usage.TotalTokens)
	} else {
		rate.releaseQuota()
	}
	// 非流式能走到这里就意味着上游完整返回了响应 —— 一定是成功。
	// 漏写success 会让每次非流式成功都被外层记成「目标失败」，
	// 健康的链首目标 3 个请求后就被误熔断（详见 attemptOutcome.success）。
	return attemptOutcome{committed: true, success: true}
}

// usageStateFor 区分「上游报了用量」与「上游一个 token 数都没给」。
//
// 必须区分：上游不报 usage（第三方兼容服务常见）时若按 "reported" 落 0 token，
// 配额、费用报表、TPM 校正会把系统性漏账当成正常数据，且事后无法从库里分辨。
// SDK 的 Usage.IsZero 把 cached/reasoning 也计入 —— 只报缓存命中或思考 token
// 也算「报了」，否则会丢掉真实数字（见 SDK usage.go 的注释）。
func usageStateFor(u rosetta.Usage) string {
	if u.IsZero() {
		return "missing"
	}
	return "reported"
}

// usage_records.status / usage_state 的取值。
//
// 这两个字符串是 usage 记录的**判读字段**（报表、成功率、以及余额扣费的
// 判据都在读它们），散成裸字面量时改一处忘一处的后果很隐蔽：扣费判据一旦
// 与落库侧写的不一致，用户就会「明明成功却被免单」或「明明失败却被扣钱」，
// 而这两种都**没有任何报错**。故在此固定成常量 —— 只给**新增**的判读方
// （usageRecorder.charge）用，既有落库处的裸字面量保持原样，避免本次改动
// 波及热路径上每一条 usage 记录。
const (
	// usageStatusOK 是「这次调用真的成功」。与 attemptOutcome.success 的
	// 判据同源：流式只有 status=="ok" 才算成功，断流/溢出/中断都不是。
	usageStatusOK = "ok"
)

// isClientGone 判断一次流式中断是否源于**客户端**断开，而不是上游故障。
//
// 请求 context 被取消（用户点「停止生成」、客户端进程退出、网络切换）时，
// SDK 会把 context 错误从流里透出来。这类中断既不该算进上游的成功率，
// 也不该打成 ERROR —— 它跟路由、凭据、上游的健康状况无关。
func isClientGone(ctx context.Context, err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	return ctx.Err() != nil
}

// usageRecorder 负责把用量记录异步落库，并在关停时等它们写完。
//
// 异步是必须的 —— 不能让客户端为了等一次 SQLite 写多耗一个来回；但纯粹的
// fire-and-forget 有真实代价：srv.Shutdown 只等 handler 返回、不等这些 goroutine，
// 进程退出时尾部若干条 usage 会凭空消失（表现是配额与统计对不上账）。
//
// 用固定数量的 worker + 有界队列取代「每条记录一个 goroutine」：
//   - 高并发下不再无限堆积 goroutine（配合 SetMaxOpenConns(1)，无界 goroutine
//     只会全部阻塞在 DB 锁上排队，白白吃内存）；
//   - 队列满时 record 退化为同步写，提供背压而不是静默丢记录。
type usageRecorder struct {
	db     *store.Store
	logger *slog.Logger
	queue  chan *store.UsageRecord
	wg     sync.WaitGroup

	// mu 保护 closed，且**把「发送」纳入临界区**（2026-10-10 修复的 P1）。
	//
	// 为什么需要它：`record` 里的 `select { case ch <- rec: default: }`
	// 对**已关闭**的 channel 并不安全 —— 向关闭的 channel 发送是运行时
	// panic，而不是「default 分支」那种「暂时不可写」。只写 `default` 挡不住。
	//
	// 触发顺序：Shutdown 的 10s 预算到期后它只是**返回错误**，并不会杀掉
	// 仍在跑的 handler（http.Server.Shutdown 到点即返回）；紧接着 wait 关闭
	// 队列；此时那条还活着的流收尾调用 record → send on closed channel →
	// panic。流请求的存活时间本来就可能超过 10s（WriteTimeout 为 0，
	// 生命周期只看 TTFT/空闲看门狗），所以这是正常时序，不是边角。
	//
	// 用锁把「判 closed + 发送」变成原子操作：要么在关闭前成功入队，
	// 要么看见 closed 后走同步写，**永远不会 panic**。
	mu     sync.Mutex
	closed bool
}

const (
	// usageQueueSize 是有界队列容量。超过该值即触发同步写背压。
	usageQueueSize = 1024
	// usageWorkers 是并发落库的 worker 数。SQLite 单写锁下并发写没有收益，
	// 1 个 worker 足够，队列本身负责吸收突发。
	usageWorkers = 1

	// usageBatchSize 是一批最多攒多少条 usage 记录（2026-11 P1 性能修复）。
	//
	// 取 50 是**实测**出来的（AUDIT/fix-p1-perf.md 的 batch 探针，
	// 同机同方法）：每行耗时随批大小的曲线是
	//   bs=1 → 305µs   bs=10 → 77µs   bs=50 → 39µs   bs=200 → 45µs
	// 收益在 50 附近饱和，再往上反而略升（大 VALUES 文本变大，
	// 解析与 B-tree 插入都更贵）。
	//
	// 上限的意义不只是吞吐，还有**延迟上界**：一批攒到 50 才发，
	// 最坏情况下每条记录要等 49 个同伴都到齐才落库。高负载下 50 条
	// 形成只需要几十微秒（实测 39µs/行 × 50），可以忽略；
	// 而**低负载下队列根本攒不满**，靠 `default` 分支立刻发出，
	// 所以延迟不受批大小影响 —— 这是选「非阻塞攒批」而非「定时批量」的原因。
	usageBatchSize = 50
)

// ctxBackground 是 context.Background 的短别名。
//
// 落库是异步的、与请求无关，用 Background 而不是请求的 ctx：
// 客户端断开**不该**取消用量落库（那次调用真的发生了、真的要记账）。
// 抽成常量只是为了不重复 import context 后到处敲长名字。
func ctxBackground() context.Context { return context.Background() }

func newUsageRecorder(db *store.Store, logger *slog.Logger) *usageRecorder {
	u := &usageRecorder{
		db:     db,
		logger: logger,
		queue:  make(chan *store.UsageRecord, usageQueueSize),
	}
	for i := 0; i < usageWorkers; i++ {
		u.wg.Add(1)
		go u.worker()
	}
	return u
}

// worker 消费队列并落库，直到队列关闭。
//
// 落库**之后**按「请求是否成功」决定要不要扣余额（Lead 与用户确认的口径）。
// 为什么扣费挂在落库之后、而不是请求结束时按内存里的估算扣：金额必须等于
// 那条 usage 落库时固化的 cost_total。两处各算一份必然漂移，而漂移的后果是
// 「报表显示花了 X、余额少了 Y」且无人能发现。所以这里用
// CreateUsageRecordWithCost —— 它返回的**就是写进 cost_total 列的那个变量**
// （不是重算一次），换句话说「扣的 = 报表的」在结构上就不可能漂移。
//
// # 批量落库（2026-10-11，P1 性能修复）
//
// 原来是 `for rec := range u.queue` 一次一条，每条一个**独立事务**。
// 实测单条落库 **296µs/行**；把 N 行并进一个事务后，批大小 50 时只需
// **39µs/行（7.6×）**。差的是每次独立事务的固定开销（commit）。
//
// 写池是单连接（SetMaxOpenConns(1)），于是每个成功请求都要付一次那个
// 固定开销 —— 这就是 AUDIT/test-perf.md 那条 P1 的直接成因：
// 吞吐被钉在落库速率上（实测仅 INSERT 时 3693 req/s）。
//
// 现在改成：**攒够一批（或队列已排空）就一个事务写入**。
// 计价仍**逐条**由 store 的 freezeUsageCost 算（批量只省提交开销，
// **不改计价语义**），所以「扣的 = 报表的」这条不变量逐字不变。
//
// # 改后的事务边界
//
// 一批 N 条 = **一个事务**（一条 N 行多值 INSERT）。
// 失败时**整批回滚**，store 返回**全 0 费用**，调用方据此**整批不扣费**。
// 这与单条路径的「落库失败 → 不扣费」口径完全一致：账记不下来时不收钱。
//
// 扣费**不跟着批量**，仍是逐条一个事务 —— 理由见 AUDIT/fix-p1-perf.md 第 4 节：
// 幂等键 (request_id, user_id) 是逐条的，聚合扣费会让「哪几笔算已扣过」
// 变成需要额外状态才能回答的问题，错一次就是漏收或重复收费。
func (u *usageRecorder) worker() {
	defer u.wg.Done()

	// batch 是复用缓冲区：高负载下每秒数千批，每批都 make 一次会直接
	// 变成 GC 压力（而 GC 压力正是我们想减掉的东西）。
	batch := make([]*store.UsageRecord, 0, usageBatchSize)

	for {
		// 取第一条：**阻塞**读。关闭时 channel 被 close，这里返回 !ok，
		// 冲掉手上这批再退出（关停 drain 的全部意义就是别弄丢已发生的调用）。
		rec, ok := <-u.queue
		if !ok {
			u.flush(ctxBackground(), batch)
			return
		}
		batch = append(batch, rec)

		// 尽力多攒：把**当前已就绪**的记录挪进这批。
		//
		// 用 `default` 而不是阻塞读：阻塞会把「攒批」变成「每个请求等一批」，
		// 尾延迟会变成 N×批间隔，低负载时尤其难看。这里要的是
		// 「能攒就攒，攒不到就立刻发」—— 低负载时批自然很小（退化成单条），
		// 而低负载本来就不需要吞吐优化，**低延迟更重要**。
	drain:
		for len(batch) < usageBatchSize {
			select {
			case r2, ok2 := <-u.queue:
				if !ok2 {
					break drain
				}
				batch = append(batch, r2)
			default:
				break drain
			}
		}

		u.flush(ctxBackground(), batch)
		batch = batch[:0]
	}
}

// flush 把一批记录落库，然后**逐条**扣费。
//
// 落库是批量的（省提交开销），扣费保持逐条（幂等键逐条不同，聚合风险高
// —— 见 worker 的注释）。
//
// 失败语义：整批落库失败 → store 返回全 0 费用 → **一条都不扣**。
// 这与单条路径一致，且比「部分扣费」更好：部分扣费会让「报表与余额」
// 在这一批里出现无法解释的偏差，而用量是**计费依据**，宁缺勿滥。
func (u *usageRecorder) flush(ctx context.Context, batch []*store.UsageRecord) {
	if len(batch) == 0 {
		return
	}
	costs, err := u.db.CreateUsageRecordsBatched(ctx, batch)
	if err != nil {
		u.logger.Error("failed to record usage (batched)", "error", err,
			"count", len(batch), "key_id", batch[0].AccessKeyID)
		// 落库失败**不扣费**：账都记不下来时扣钱，等于凭空收了一笔
		// 无据可查的费用。宁可漏扣（cost_total 侧有 RecomputeCost 事后
		// 补救）也不做无据收费。
		return
	}
	for i, rec := range batch {
		var cost float64
		if i < len(costs) {
			cost = costs[i]
		}
		u.charge(rec, cost)
	}
}

// charge 对一条**已落库**的用量记录扣余额。cost 是本次落库时固化的费用（元），
// 由 CreateUsageRecordWithCost 给出 —— 它与 cost_total 列是同一个值。
//
// # 只对成功的请求扣费（Lead 与用户确认的第四条口径）
//
// 判据是 usage_records.status == "ok"。它是「这一次调用真的成功」的既有
// 事实来源，attemptStream/attemptNonStream 落库时就定好了，且**与熔断计数的
// 判据是同一个**（见 attemptOutcome.success 的注释：committed 只说明字节已
// 写出，断流/溢出也是 committed 却是失败）。用同一个判据，余额与健康度不会
// 各说各话。
//
// 明确**不扣**的形态（它们都不该收用户钱）：上游错误与不可转移错误、
// 流式截断/溢出/中断、客户端主动断开（canceled）、以及根本没碰上游就被拒的
// 请求（限流、配额、余额预检自身、model_not_found）。后者压根不会走到这里
// —— 没有 usage 记录可扣。
//
// 与 cost_total 的差别要记牢：cost_total **无论成败都记**（那是网关的上游
// 成本），扣费**只看成功**（那是用户的应收）。同一次调用两个数相同、触发条件
// 不同。见 billing.go 的口径说明。
//
// # 与 rateCommit 收尾的关系（两者互不干扰）
//
// rateCommit 管的是**预占**（TPM 窗口 + 终身 token 配额），它必须在请求
// 结束那一刻收尾（流式是边写边收）。扣费管的是**实收**，它挂在 usage 落库
// 之后、异步发生。两者没有共享状态：预占退回的是"还没花的额度"，扣费收的是
// "已经花的钱"，方向相反不会互相抵消。
//
// usage missing（上游没报 token 数）时 cost_total 记 0 → 扣费也是 0
// （store.ChargeBalance 对 0 元 no-op）。这与该路径"只退配额预占、保留 TPM
// 预占"的既有决策不冲突：后者是终身累计不退就永久泄漏，前者随窗口翻转自愈，
// 而我们收 0 元本来就是正确结果（不知道实际花了多少，就不该收钱）。
func (u *usageRecorder) charge(rec *store.UsageRecord, costYuan float64) {
	// 管理员短路（纵深防御：数据面上不可达，见 billing.go 的 balanceExempt）。
	//
	// 2026-10 控制面/数据面分离后，管理员在 auth.Authenticate 就被拒
	// （ErrAdminCannotCallModel），压根不会有管理员的 usage 落到这里。保留
	// 这道短路是纵深防御：余额是「钱」，万一某条路径绕过 auth 送来一条管理员
	// 的 usage 记录，这里立刻 no-op，不去碰他的余额。判据不依赖任何 DB 状态、
	// 只读快照，代价为零。
	if balanceExempt(rec.UserID) {
		return
	}
	// 非成功不扣：见本函数「只对成功的请求扣费」。
	if rec.Status != usageStatusOK {
		return
	}
	// 空 request_id → 没有幂等键，扣费**不做**（宁可漏扣不可重复扣）。
	//
	// 生产里 request_id 由 server.Middleware 每个请求生成，必非空；为空
	// 意味着有代码路径绕过了中间件，那属于异常而非常态。这里记 ERROR
	// 而不是静默跳过 —— 漏扣是一次没人发现的钱（对账才看得出），而
	// 无幂等的扣费会在重复收尾时**真的扣第二次**，用户立刻就会投诉。
	// 两害相权取轻，但绝不无声无息。
	if rec.RequestID == "" {
		u.logger.Error("usage record has empty request_id, skipping charge (no idempotency key)",
			"user_id", rec.UserID, "access_key_id", rec.AccessKeyID,
			"public_model", rec.PublicModel, "cost_cents", store.YuanToCents(costYuan))
		return
	}
	// 元 → 分走 store 的 YuanToCents（唯一实现）：预检侧估的与这里扣的
	// 必须用同一个换算，否则「预检以为够、实际差一分钱」会成为常态。
	//
	// 传给 ChargeBalance 的是**元**而不是分（2026-10-10）：单价可能远低于
	// 一分（实测 price_input=3.0 元/百万时，一次 8000 token 的调用只有
	// 0.004 元），按分取整就是 0，而 0 分在扣费侧是 no-op —— 一次都扣不到钱。
	// 余数的累计与折算都由 store 内部完成，这里只负责把「这一次实际花了
	// 多少元」这个已经固化在 cost_total 里的数字送过去。
	err := u.db.ChargeBalance(context.Background(), rec.UserID, costYuan, rec.RequestID)
	switch {
	case err == nil:
	case errors.Is(err, store.ErrInsufficientBalance):
		// 预检在前，正常路径走不到这里。走到 = 并发透支或预检漏了。
		// 此时上游已被调用、费用已固化进 cost_total，钱收不回，
		// 「报欠费」比「悄悄放过」诚实。响应早已写出，改不了，只能记 ERROR。
		u.logger.Error("charge failed: insufficient balance (precheck should have prevented this)",
			"user_id", rec.UserID, "request_id", rec.RequestID,
			"access_key_id", rec.AccessKeyID, "public_model", rec.PublicModel)
	default:
		u.logger.Error("failed to charge balance",
			"error", err, "user_id", rec.UserID, "request_id", rec.RequestID,
			"access_key_id", rec.AccessKeyID, "public_model", rec.PublicModel)
	}
}

// record 排队一条异步落库，立即返回。
//
// 队列满时退化为同步写：宁可让当前请求多等一次 SQLite 写，也不丢用量记录。
// 同步写失败同样记日志，与异步路径口径一致。
//
// 扣费在两条路径上**都必须发生**，所以同步回退里那行 CreateUsageRecord 后面
// 紧跟着 u.charge(rec)，与 worker 里完全一致。漏掉的后果是「队列一满，
// 从此之后所有请求都不扣费」—— 一个只在高压时才出现、且不会有任何报错的
// 漏收，正是 CreateUsageRecord 注释里说的「把'别忘了算'变成可以各自忘记的
// 地方」。这里刻意不抽公共函数：两条路径的收尾已经足够短，抽出去反而让
// 「它们是同一件事」这件事看不出来。
func (u *usageRecorder) record(rec *store.UsageRecord) {
	// 「判关闭 + 入队」必须在同一把锁里完成，否则会与 wait 的 close 竞争，
	// 产生 send on closed channel（见 mu 的注释）。锁外的同步写只碰 DB。
	u.mu.Lock()
	if !u.closed {
		select {
		case u.queue <- rec:
			u.mu.Unlock()
			return
		default:
			// 队列已满 —— 落到下面同步写（背压）。
		}
	}
	u.mu.Unlock()

	// 到这里有两种可能：队列满（背压），或队列已关闭（关停收尾）。
	// 后者仍走同步写而不是丢弃：这条记录对应的是一次**已经发生的调用**，
	// 而关停 drain 的全部意义就是别把它弄丢。
	cost, err := u.db.CreateUsageRecordWithCost(context.Background(), rec)
	if err != nil {
		u.logger.Error("failed to record usage (sync fallback)", "error", err, "key_id", rec.AccessKeyID)
		return
	}
	u.charge(rec, cost)
}

// wait 关闭队列、等 worker 把在途写入全部完成，或 ctx 到期（到期即放弃，
// 不让一个卡住的 SQLite 写把进程关停拖成无限期）。
//
// closed 标志与 close **同处一把锁**：这是 record 判据与 close 动作的配对点。
// 两者分开就又出现「record 已判完未关闭、close 恰好插进来」的窗口。
func (u *usageRecorder) wait(ctx context.Context) {
	u.mu.Lock()
	u.closed = true
	close(u.queue)
	u.mu.Unlock()

	done := make(chan struct{})
	go func() {
		u.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
	}
}

func handleListModels(forceShape string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// D9 分流（DESIGN §6.1）：显式别名路径永远优先；/v1/models 按认证头
		// 分流 —— 带 x-api-key 且不带 Authorization: Bearer 的请求按 Anthropic
		// 形状返回，其余按 OpenAI 形状。
		shape := forceShape
		if shape == "" {
			if r.Header.Get("X-Api-Key") != "" && !hasBearer(r) {
				shape = "anthropic"
			} else {
				shape = "openai"
			}
		}
		writeErr := errorWriter(outwire.WriteOpenAIError)
		if shape == "anthropic" {
			writeErr = outwire.WriteAnthropicError
		}

		// 与转发入口同一鉴权口径（官方 OpenAI / Anthropic 的 /v1/models 同样要求
		// 认证）。这里不查配额 —— 列个目录不消耗 token —— 但必须校验密钥：
		// 否则任何人都能枚举出全部公开模型名，等于白送一份路由与供应商结构图。
		authCtx, err := auth.Authenticate(r)
		if err != nil {
			writeAuthError(w, err, writeErr)
			return
		}

		snap := snapshot.Get()
		// 轨道一：虚拟名。?include=upstream 时追加轨道二的 slug/model 形式
		// （可能非常长且随 provider 增长，默认不列 —— DESIGN §5.2）。
		ids := make([]string, 0, 16)
		seen := make(map[string]bool)
		for _, route := range snap.Routes.ListRoutes() {
			// 模型白名单（多用户改造 P1）：列表就是「你能用什么」的权威答案，
			// 必须与转发路径用**同一套判定**，否则会出现「列表里有、调起来 403」
			// 或者反过来「能调但列表不显示」。
			if route.Enabled && authCtx.AllowsModel(route.PublicName) && !seen[route.PublicName] {
				seen[route.PublicName] = true
				ids = append(ids, route.PublicName)
			}
		}
		if r.URL.Query().Get("include") == "upstream" {
			for _, m := range snap.Routes.ListUpstreamModels() {
				id := m.ProviderSlug + "/" + m.ModelID
				// 直连标识符与公开模型名是**同一个命名空间里的字符串**：
				// 白名单里出现哪个就放行哪个。这里直接用 id 判定即可 ——
				// routing.Resolve 对这类形式会合成一条 PublicName 恰为 id 的
				// route，所以「解析后判定」与「按 id 判定」完全等价，
				// 多走一次 Resolve 只是白费。
				if m.Enabled && !seen[id] && authCtx.AllowsModel(id) {
					seen[id] = true
					ids = append(ids, id)
				}
			}
		}

		w.Header().Set("Content-Type", "application/json")
		if shape == "anthropic" {
			entries := make([]map[string]any, 0, len(ids))
			for _, id := range ids {
				// created_at：网关不为模型持久化创建时间，用固定纪元占位
				//（客户端不消费该字段，保持确定性比编一个值诚实）。
				entry := map[string]any{
					"type": "model", "id": id, "display_name": id,
					"created_at": "1970-01-01T00:00:00Z",
				}
				appendModelMetadata(entry, snap, id)
				entries = append(entries, entry)
			}
			var first, last any
			if len(entries) > 0 {
				first, last = entries[0]["id"], entries[len(entries)-1]["id"]
			}
			json.NewEncoder(w).Encode(map[string]any{
				"data": entries, "has_more": false, "first_id": first, "last_id": last,
			})
			return
		}

		entries := make([]map[string]any, 0, len(ids))
		for _, id := range ids {
			entry := map[string]any{
				"id": id, "object": "model", "created": 0, "owned_by": "gateway",
			}
			appendModelMetadata(entry, snap, id)
			entries = append(entries, entry)
		}
		json.NewEncoder(w).Encode(map[string]any{
			"object": "list",
			"data":   entries,
		})
	}
}

// appendModelMetadata 把「解析后的链首上游模型」的容量写进模型条目：
// context_length（OpenRouter 约定）、max_input_tokens / max_output_tokens
// （LiteLLM 约定）。上游模型未单独覆盖时回落设置页的模型容量默认；
// 两级都为 0 则不加字段（不编造数字）。
func appendModelMetadata(entry map[string]any, snap *snapshot.Snapshot, modelID string) {
	cw, mo := modelCapacity(snap, modelID)
	if cw > 0 {
		entry["context_length"] = cw
		entry["max_input_tokens"] = cw
	}
	if mo > 0 {
		entry["max_output_tokens"] = mo
	}
	// 思考能力：这是工具侧「是否支持思考模式」那个开关的数据来源。
	//
	// 三态与数据面完全一致，且**只在确定为「不支持」时才下发 false**：
	// 未配置（不知道）时缺席，客户端维持既有行为 —— 把「不知道」报成 false
	// 会让所有未配置的模型都失去思考选择器，那是一次静默的能力回退。
	if m := modelThinkingCapability(snap, modelID); m != nil {
		if m.SupportsThinking != nil {
			entry["supports_thinking"] = *m.SupportsThinking
		}
		if len(m.EffortLevels) > 0 {
			// 键名取 OpenRouter 的 supported_reasoning —— 那是这条链路上已有的
			// 既成约定，字段形状（字符串数组）也相同。
			out := make([]string, 0, len(m.EffortLevels))
			for _, l := range m.EffortLevels {
				out = append(out, string(l))
			}
			entry["supported_reasoning"] = out
		}
	}
}

// modelThinkingCapability 取对外模型名解析后**链首**上游模型的思考能力。
//
// 只看链首：故障转移到别的目标时数据面会按那个目标的能力重新处理（见
// ingressRequest.applyThinkingCapability），所以这里披露的是「默认会落到
// 哪一档」，而不是「这条链所有可能的档位集合」—— 后者是个会误导客户端的并集。
func modelThinkingCapability(snap *snapshot.Snapshot, modelID string) *routing.UpstreamModel {
	res, err := snap.Routes.Resolve(modelID)
	if err != nil || len(res.Candidates) == 0 {
		return nil
	}
	return res.Candidates[0].UpstreamModel
}

// modelCapacity 解析一个对外模型名到链首上游模型的容量。
func modelCapacity(snap *snapshot.Snapshot, modelID string) (contextWindow, maxOutput int) {
	res, err := snap.Routes.Resolve(modelID)
	if err != nil || len(res.Candidates) == 0 {
		return 0, 0
	}
	m := res.Candidates[0].UpstreamModel
	if m == nil {
		return 0, 0
	}
	cw, mo := m.ContextWindow, m.MaxOutputTokens
	if cw <= 0 {
		cw = snap.Runtime.DefaultContextWindow
	}
	if mo <= 0 {
		mo = snap.Runtime.DefaultMaxOutputTokens
	}
	return cw, mo
}

// handleGetModel 返回单个对外模型的详情（OpenAI /v1/models/{id} 形状 + 容量
// 元数据）。轨道一的虚拟名与轨道二的 slug/model 都可查。
func handleGetModel(forceShape string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		shape := forceShape
		if shape == "" {
			if r.Header.Get("X-Api-Key") != "" && !hasBearer(r) {
				shape = "anthropic"
			} else {
				shape = "openai"
			}
		}
		writeErr := errorWriter(outwire.WriteOpenAIError)
		if shape == "anthropic" {
			writeErr = outwire.WriteAnthropicError
		}
		authCtx, err := auth.Authenticate(r)
		if err != nil {
			writeAuthError(w, err, writeErr)
			return
		}

		modelID := r.PathValue("model")
		snap := snapshot.Get()
		res, rerr := snap.Routes.Resolve(modelID)
		if rerr != nil {
			writeErr(w, http.StatusNotFound, "model_not_found", fmt.Sprintf("model %q not found", modelID))
			return
		}
		// 模型白名单（P1）：校验**解析后**的公开模型名，而不是 URL 原文。
		// 虚拟名与 provider/model 两种写法都解析到同一条路由，只查原文
		// 会让 slug 形式绕过白名单。
		if !authCtx.AllowsModel(res.Route.PublicName) {
			writeErr(w, http.StatusForbidden, "model_not_allowed",
				fmt.Sprintf("model %q is not available for this API key", modelID))
			return
		}

		w.Header().Set("Content-Type", "application/json")
		if shape == "anthropic" {
			entry := map[string]any{
				"type": "model", "id": modelID, "display_name": modelID,
				"created_at": "1970-01-01T00:00:00Z",
			}
			appendModelMetadata(entry, snap, modelID)
			json.NewEncoder(w).Encode(entry)
			return
		}
		entry := map[string]any{
			"id": modelID, "object": "model", "created": 0, "owned_by": "gateway",
		}
		appendModelMetadata(entry, snap, modelID)
		json.NewEncoder(w).Encode(entry)
	}
}

// hasBearer 报告请求是否带 Authorization: Bearer 头（D9 分流用，大小写不敏感）。
func hasBearer(r *http.Request) bool {
	const prefix = "Bearer "
	auth := r.Header.Get("Authorization")
	return len(auth) > len(prefix) && strings.EqualFold(auth[:len(prefix)], prefix)
}

func generateID() string {
	b := make([]byte, 16)
	crand.Read(b)
	return hex.EncodeToString(b)
}
