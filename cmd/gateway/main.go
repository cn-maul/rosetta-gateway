package main

import (
	"context"
	crand "crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/cn-maul/rosetta-gateway/internal/auth"
	"github.com/cn-maul/rosetta-gateway/internal/authhttp"
	"github.com/cn-maul/rosetta-gateway/internal/config"
	"github.com/cn-maul/rosetta-gateway/internal/crypto"
	"github.com/cn-maul/rosetta-gateway/internal/dataplane"
	"github.com/cn-maul/rosetta-gateway/internal/outwire"
	"github.com/cn-maul/rosetta-gateway/internal/probe"
	"github.com/cn-maul/rosetta-gateway/internal/ratelimit"
	"github.com/cn-maul/rosetta-gateway/internal/routing"
	"github.com/cn-maul/rosetta-gateway/internal/server"
	"github.com/cn-maul/rosetta-gateway/internal/snapshot"
	"github.com/cn-maul/rosetta-gateway/internal/store"
	"github.com/cn-maul/rosetta-gateway/internal/upstream"
	"github.com/cn-maul/rosetta-gateway/internal/userauth"
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

// healthzPinger 是健康检查需要的最小存储能力。
//
// 抽成接口只有一个理由：**可测**。要验证「数据库不可用时响应体不得
// 回显底层错误」，就得能造一个 ping 失败的存储 —— 真把数据库弄坏
// 不可靠（Windows 上 SQLite 的文件锁行为与 Linux 不同），而这里要测的
// 恰恰是错误路径。
type healthzPinger interface {
	Ping(ctx context.Context) error
}

// healthzDirty 是「运行时是否落后于数据库」的判据。
//
// 同样抽出来是为了可测：reloader 的 dirty 标志是 unexported 字段，
// 而构造一个真的「重建失败过」的 runtimeReloader 需要整套 DB + 池。
type healthzDirty interface {
	IsDirty() bool
}

// healthzHandler 返回 GET /healthz 的处理器。
//
// # 为什么错误体不回显底层错误（2026-10-21 修 A10-P1-4）
//
// 这个端点是**免鉴权**的：探针要在没有会话、没有 token 的情况下可用。
// 而 SQLite 的错误字符串可以含数据库路径、驱动内部信息、文件系统错误
// （store.Ping 就会把写池/读池的原始错误包进来）。同网段的主动探测者
// 能拿它做环境枚举与故障定位。
//
// 现在对外只有两个稳定字段：
//   - status：ok / degraded / unhealthy；
//   - error_code：一个不随实现漂移的机器可读代码。
//
// 完整错误**只进服务端日志** —— 需要诊断细节的运维看日志，
// 而匿名探测者只能看到一个「数据库不可用」。
//
// # 三种状态的语义
//
//   - ok：两个连接池都通，且运行时没落后。200。
//   - degraded：数据库可用，但运行时落后于数据库（上次重建失败）。
//     报 503 让编排系统有机会介入，同时区别于「进程死了」。
//   - unhealthy：数据库不可用。503。
func healthzHandler(db healthzPinger, reloader healthzDirty, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		body := map[string]any{"status": "ok"}
		code := http.StatusOK
		if err := db.Ping(ctx); err != nil {
			// 完整错误只进日志，对外只给一个稳定代码。
			logger.Error("healthz: database ping failed", "error", err)
			body = map[string]any{"status": "unhealthy", "error_code": "database_unavailable"}
			code = http.StatusServiceUnavailable
		} else if reloader.IsDirty() {
			// 数据库可用、运行时落后：进程还活着但配置未生效。
			body = map[string]any{"status": "degraded", "error_code": "runtime_lags_behind_database"}
			code = http.StatusServiceUnavailable
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(body)
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
		// 当前格式只接受 AES-256-GCM 密文。这里没有「无主密钥继续跑」
		// 的降级路径：那会把上游 API key 明文写库，且无法再与当前格式区分。
		logger.Error("master key unavailable, refusing to start", "error", err,
			"hint", "确认状态目录可写，或设置 "+cfg.MasterKeyEnv)
		os.Exit(1)
	case generatedKey:
		logger.Info("generated master key", "path", filepath.Join(homeDir, crypto.KeyFileName))
	case weakKey:
		if cfg.RequireStrongMasterKey {
			// require_strong_master_key=true 是部署点显式声明「不接受低熵主密钥」。
			// 与 bootstrap 令牌那条路径（fail-closed）保持同一取向：主密钥一旦
			// 可爆破，全部上游 API key 一起失守，不给「先跑起来再说」的余地。
			// 改法：删掉该文件让网关自动生成随机 master.key，然后逐条重存凭据
			// （或改用环境变量注入一条 44 字符的 base64url 密钥）。
			logger.Error("master key material is weak (<32 chars) and require_strong_master_key is set; refusing to start",
				"hint", "删除 "+filepath.Join(homeDir, crypto.KeyFileName)+" 后重启以自动生成随机主密钥，并重新保存全部上游凭据")
			os.Exit(1)
		}
		logger.Warn("master key material is short (<32 chars) and looks like a passphrase; " +
			"upstream API keys encrypted with it are brute-forceable offline. " +
			"Recommended: delete the weak key, restart to auto-generate a random master.key, then re-save every credential. " +
			"Or set require_strong_master_key=true to refuse to start on a weak key")
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

	// 探测循环的上下文：随 shutdown 取消。探测是低频后台任务，用独立的
	// context 而不是复用 reloadCtx（那是重建重试的，语义不同）。
	probeCtx, stopProbe := context.WithCancel(context.Background())
	defer stopProbe()

	reloadCtx, stopReloadRetry := context.WithCancel(context.Background())
	defer stopReloadRetry()
	go reloader.watchReloadRetry(reloadCtx)

	usage := newUsageRecorder(db, logger)
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
	// 拒绝启动的判据见 ensureBootstrapToken 的注释：只有「引导窗口开着
	// 且没能用一次性令牌保护上」才是致命的。与 master.key 的处理同一个
	// 原则 —— 一次瞬态失败不该换来一条明文凭据或一个免鉴权的管理员入口。
	// 容器编排会按重启策略重试；单机部署则得到一条明确的日志而不是一个
	// 悄悄裸奔的服务。
	if err := ensureBootstrapToken(context.Background(), db, logger); err != nil {
		logger.Error("bootstrap protection unavailable, refusing to start",
			"error", err,
			"impact", "免鉴权的 POST /admin/api/bootstrap 在本次启动中没有任何凭据保护；"+
				"继续监听会把「首个管理员」的决定权交给任何能连到本端口的人",
			"hint", "修复后重启即可；也可显式设置一个至少 32 字节的 "+
				bootstrapTokenEnv+" 后重启")
		os.Exit(1)
	}

	// 首次初始化窗口对所有能连到端口的人开放（POST /admin/api/bootstrap
	// 免鉴权，直到管理员设完密码为止），完成它需要那枚一次性引导令牌。
	// 默认 listen 绑回环，只有本机能碰；运维把它改成 0.0.0.0
	// （Docker 的 `-p 8666:8666` 就会这么要求）是很常见的做法，
	// 那时整个局域网都能**尝试**抢先成为第一个管理员 —— 但只有知道
	// 启动日志里那个一次性令牌的人才能真的完成它。这个告警仍不能省：
	// 它提醒的是「尽快把这件事做掉」。
	//
	// 走到这里说明 ensureBootstrapToken 已成功返回：窗口要么已被令牌
	// 保护，要么压根没开（引导已完成 / 没有待初始化管理员）。
	pending, err := db.FindUninitializedAdmin(context.Background())
	if err != nil {
		logger.Error("cannot read bootstrap state; admin UI may be unreachable", "error", err)
	} else if pending != nil && !isLoopbackListen(cfg.Listen) {
		logger.Error("SECURITY: admin password not yet set AND listening on a non-loopback address — "+
			"anyone who can reach this port can attempt to become the first administrator; "+
			"only holders of the bootstrap token can complete it",
			"listen", cfg.Listen,
			"immediate_action", "立即在浏览器打开 /admin/ 完成首次设置密码（需填本次部署的引导令牌）")
	} else if pending != nil {
		logger.Warn("admin password not set yet; open the admin UI with the bootstrap token to complete first-run setup",
			"note", "监听地址是回环，仅本机可访问；一旦改为 0.0.0.0 暴露到网络，请立即完成设置")
	}

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
		logger.Error("session secret unusable; refusing to start", "error", err)
		os.Exit(1)
	}
	if sessionMgr.Enabled() {
		logger.Info("user sessions enabled", "ttl", sessionMgr.TTL(),
			"source", secretSource(secretPath))
	} else {
		logger.Error("user sessions unavailable; refusing to start",
			"reason", "session secret could not be loaded or generated")
		os.Exit(1)
	}

	handler := buildHandler(routeDeps{
		db: db, cfg: cfg, logger: logger, masterKey: masterKey,
		pool: pool, reloader: reloader, sessionMgr: sessionMgr,
		rateLimiter: ratelimit.New(), usage: usage,
	})

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
		// 审计日志按**行数**封顶（不是天数）：它的唯一读路径是
		// 「最近 N 条」，更早的行没有任何下游消费者，却随写操作次数无界增长。
		// 跟着用量剪枝同一个循环跑即可，keepRows <= 0 表示部署点显式关闭。
		pruneAudit := func(reason string) {
			if cfg.AuditLogKeepRows <= 0 {
				return
			}
			n, err := db.PruneAuditLog(context.Background(), cfg.AuditLogKeepRows)
			if err != nil {
				logger.Error("audit log prune failed", "error", err, "trigger", reason)
				return
			}
			if n > 0 {
				logger.Info("audit log pruned", "trigger", reason,
					"deleted_rows", n, "keep_rows", cfg.AuditLogKeepRows)
			}
		}
		runPrune := func(reason string) {
			// 保留期来自配置（config.UsageRetentionDays），默认 30。
			// 此前硬编码 store.DefaultRetentionDays，用量小的部署无法调短 ——
			// 性能审计实测：它直接决定用户级配额预检的
			// 单次耗时（30 天 ≈ 63ms，7 天 ≈ 15ms）。
			res, err := db.PruneOldUsage(context.Background(), cfg.UsageRetentionDays)
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
			pruneAudit(reason)
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

	// 费用重算循环（2026-10-12 修 P1-2）。
	//
	// freezeUsageCost 在读池抖动时会把 usage_state 标成 cost_pending 并记 0 元，
	// 那个 0 会同时让报表与扣费变成「完全免单」。本循环把它捞回来自动重算，
	// 于是「一次抖动 = 一笔永久免单」降级为「一次抖动 = 最多 5 分钟的延迟」。
	//
	// 周期取 5 分钟而不是并进剪枝的 24 小时：抖动是偶发的，而免单是实时的
	// —— 每笔待重算记录都在这段时间里被当成 0 元，少收的钱随时间累积。
	//
	// 单次上限 500 条 + 「还有剩余就再排一轮」：长期故障（读池持续不可用）
	// 下待重算队列会增长，无界扫描会把写池占死，反而拖垮正常流量。
	recomputeDone := make(chan struct{})
	go func() {
		defer close(recomputeDone)
		runRecompute := func(reason string) {
			// 单轮最多循环 10 批（5000 条），避免长期积压时一轮干太久
			// 占住写连接。剩下的留给下一轮。
			for round := 0; round < 10; round++ {
				fixed, remaining, err := db.RecomputePendingCosts(context.Background(), 500)
				if err != nil {
					// 单次失败不终止循环：下个周期重试即可。
					logger.Error("pending cost recompute failed", "error", err, "trigger", reason)
					return
				}
				if fixed > 0 {
					logger.Info("recomputed pending usage costs",
						"trigger", reason, "fixed", fixed, "remaining", remaining)
				}
				if !remaining {
					return
				}
			}
			logger.Warn("pending cost recompute backlog still large after 10 rounds",
				"trigger", reason, "batch_size", 500)
		}
		runRecompute("startup")
		tk := time.NewTicker(5 * time.Minute)
		defer tk.Stop()
		for {
			select {
			case <-tk.C:
				runRecompute("periodic")
			case <-shutdown:
				return
			}
		}
	}()

	// 扣费对账循环（2026-10-21 修 A4-P1-4 / 汇总 F4）。
	//
	// # 它补的是什么洞
	//
	// usage 落库与余额扣费是**两个事务**（usageRecorder 先落库、再 charge）。
	// 两者之间如果进程被杀，或扣费遇到瞬时故障且重试也全部失败，这笔费用就
	// 永远停在「报表上有、余额没扣」的状态 —— 而且**没有任何落盘痕迹**
	// 说明它漏了：usage_state 是 'reported'、cost_total 非 0，看起来完全正常。
	//
	// 修复前的扣费侧已有重试（见 retryCharge），但重试只在**进程活着**时
	// 有用。崩溃窗口与「重试全部失败」这两条路径依然无人兜底，而全仓
	// 此前**没有任何对账任务**比对「该扣的」与「扣了的」。
	//
	// # 为什么单独一个循环而不是并进 runRecompute
	//
	// 两者的判据不同：重算找的是「费用是 0 但应该有」（usage_state='cost_pending'
	// 且查价失败），对账找的是「费用有但没扣」。前者要改明细与累计表，
	// 后者只动余额。混在一起会让两个不同的失败模式共享一套重试策略。
	//
	// # 周期
	//
	// 10 分钟。这类漏扣是低频事件（要恰好在两个事务之间崩溃），但金额是
	// 钱 —— 10 分钟的延迟远小于「永远不补」。而对账要扫 usage_records 并
	// LEFT JOIN balance_charges，比重算贵，所以周期取得比重算长。
	reconcileDone := make(chan struct{})
	go func() {
		defer close(reconcileDone)
		runReconcile := func(reason string) {
			// 单轮最多 500 条：漏扣是罕见事件，一轮捞不完说明有系统性问题，
			// 剩下的交给下一轮 —— 无界扫描会把写池占死。
			found, err := db.FindUnchargedUsage(context.Background(), 500)
			if err != nil {
				logger.Error("charge reconciliation scan failed", "error", err, "trigger", reason)
				return
			}
			if len(found) == 0 {
				return
			}
			var charged, failed int
			for _, u := range found {
				if cerr := db.ChargeUnchargedUsage(context.Background(), u); cerr != nil {
					failed++
					logger.Error("charge reconciliation failed to charge",
						"error", cerr, "record_id", u.RecordID,
						"user_id", u.UserID, "request_id", u.RequestID,
						"cost_yuan", u.CostYuan)
					continue
				}
				charged++
			}
			// 补扣成功要留痕：这是「报表与余额曾经分叉、现已收敛」的证据。
			// 审计是唯一能看到「这个系统曾经漏收过钱」的地方 —— 漏收本身
			// 不会在数据里留下痕迹（那正是它难查的原因）。
			logger.Warn("reconciled uncharged usage records",
				"trigger", reason, "found", len(found), "charged", charged, "failed", failed)
		}
		runReconcile("startup")
		tk := time.NewTicker(10 * time.Minute)
		defer tk.Stop()
		for {
			select {
			case <-tk.C:
				runReconcile("periodic")
			case <-shutdown:
				return
			}
		}
	}()

	// 上游模型定时探测（2026-10-14）。照抄 runRecompute 的骨架：周期短、
	// 需要并发上限、有 done 等待先例。探测请求复用 upstream.TestUpstreamModel
	// （max_tokens=1、不经 usage_recorder），**不扣任何网关用户的钱**，
	// 结果只进 model_probes 表 —— 绝不进 UsageSource / 配额 / 余额
	// （见 store.go model_probes 建表注释的隔离红线）。
	//
	// 探测循环在 cfg.Probes.Enabled 时启动。探测失败**不写**凭据冷却，
	// 不碰 pool 的熔断状态机 —— 那是业务流量的信号，探测要回答的是
	// 「这个 model_id 现在能不能推理」。
	probeDone := make(chan struct{})
	if cfg.Probes.Enabled {
		go func() {
			defer close(probeDone)
			probeRunner := probe.NewRunner(db, cfg, masterKey, logger)
			probeRunner.Run(probeCtx)
		}()
	} else {
		close(probeDone)
	}

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
	stopProbe() // 让探测循环的 ctx 取消，它会在下一个 select 返回
	select {
	case <-pruneDone:
	case <-time.After(5 * time.Second):
		logger.Warn("usage prune still running at shutdown; exiting anyway")
	}
	// 费用重算循环同理：它会往写池写 UPDATE，与 db.Close() 并发同样会拿到
	// "database is closed"。它的单批上限很小（500 条），正常情况下立刻返回，
	// 所以给一个更短的上限（2s）—— 不该把关停时间主要花在这里。
	select {
	case <-recomputeDone:
	case <-time.After(2 * time.Second):
		logger.Warn("cost recompute still running at shutdown; exiting anyway")
	}
	// 扣费对账循环同理：它也会往写池写（补扣余额），与 db.Close() 并发会拿到
	// "database is closed"。它的单轮上限是 500 条，正常情况下立刻返回。
	select {
	case <-reconcileDone:
	case <-time.After(2 * time.Second):
		logger.Warn("charge reconciliation still running at shutdown; exiting anyway")
	}
	// 探测循环同理：它往写池写 model_probes，与 db.Close() 并发会拿到
	// "database is closed"。探测是低频写，正常情况下立刻返回。
	select {
	case <-probeDone:
	case <-time.After(2 * time.Second):
		logger.Warn("probe runner still running at shutdown; exiting anyway")
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
	// 返回 false 表示仍有记录没落库成功（flush 反复重试仍失败，
	// 2026-10-12 修 P1-1）。此时必须**大声报出来**：那是真丢了计费记录，
	// 而原实现是静默 return，日志上一片祥和、实际最多 50 条用量凭空消失，
	// 用户因此免单且配额被放宽。
	if !usage.Wait(drainCtx) {
		// drain 超时：让仍在退避重试的 worker 立刻退出，避免它与随后的
		// db.Close() 并发写（"database is closed"）并拖过进程退出。
		usage.Abort()
		logger.Error("usage drain incomplete: records were lost during shutdown",
			"hint", "check earlier 'usage record lost' / 'usage recorder aborted' errors for request_id")
	}
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
// 决定哪些熔断状态该跨重建保留。
//
// # 读失败不能当「集合为空」（2026-10-21 修 A5-P1-2）
//
// 旧实现在读失败时返回空集，而 Install 的语义是「集合外一律丢弃」
// —— 于是一次读取失败就把**全站熔断状态**清空了，让正在熔断的坏上游
// 立刻复活。而 Reload 仍报「成功」，管理面上的状态也不会提示任何异常。
//
// 正确的区分：目标确实没了（集合为空且读到了）→ 旧熔断状态应当丢掉；
// 集合**未知**（读失败）→ 应当保留全部既有熔断状态。
func liveTargetIDs(ctx context.Context, db *store.Store) upstream.LiveTargets {
	targets, err := db.ListAllRouteTargets(ctx)
	if err != nil {
		// 读失败：集合未知。不记日志（调用方的 reload 循环会记）。
		return upstream.LiveTargets{Known: false}
	}
	ids := make(map[string]bool, len(targets))
	for _, t := range targets {
		ids[t.ID] = true
	}
	return upstream.LiveTargets{IDs: ids, Known: true}
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
	// bootstrap 的代理地址 → networks.id。一条配置里的多个上游可能共用
	// 同一个代理，所以按 URL 归并成**一条**网络，而不是每个上游建一条
	// 同名（会被 UNIQUE(name) 拒）或各写一份（回到本次改造前的老问题）。
	networkByURL := make(map[string]string)

	for _, bp := range cfg.Bootstrap.Providers {
		// 先解析网络再建 provider：providers.network_id 是 FK，指向的行
		// 必须在它之前存在。
		networkID := ""
		if proxyURL := strings.TrimSpace(bp.ProxyURL); proxyURL != "" {
			id, ok := networkByURL[proxyURL]
			if !ok {
				id = "net-" + generateID()
				if err := db.CreateNetwork(ctx, &store.Network{ID: id, Name: proxyURL, ProxyURL: proxyURL}); err != nil {
					logger.Error("bootstrap: create network failed", "proxy_url", proxyURL, "error", err)
					continue
				}
				networkByURL[proxyURL] = id
			}
			networkID = id
		}
		p := &store.Provider{
			ID:         bp.Slug,
			Slug:       bp.Slug,
			Name:       bp.Name,
			Protocol:   bp.Protocol,
			Endpoint:   bp.Endpoint,
			Enabled:    true,
			MaxRetries: cfg.Defaults.MaxRetries,
			NetworkID:  networkID,
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

			enc, err := crypto.Encrypt([]byte(apiKey), masterKey)
			if err != nil {
				logger.Error("bootstrap: encrypt credential failed",
					"slug", bp.Slug, "label", bc.Label, "error", err)
				continue
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
type errorWriter = authhttp.ErrorWriter

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
			authhttp.WriteAuthError(w, err, writeErr)
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
	// # 披露口径（2026-10-14 改：nil 从「不发」改为「发 true」）
	//
	// 三种落点，每一种客户端都有明确答案：
	//
	//   - 配了 effort_levels → 下发 supported_reasoning，工具据此生成完整
	//     的档位下拉框。
	//   - supports_thinking 为 nil（未配置）→ **下发 true**。
	//   - supports_thinking 显式 false → 下发 false，工具隐藏思考开关。
	//
	// 为什么 nil 要发 true 而不是不发（原来的做法）：
	//
	// 缺席对客户端不是中立的。OpenRouter 约定里这个字段缺席时，多数工具按
	// 「不支持」处理，于是「未配置」在下游就等于「没有思考能力」—— 而网关
	// 自己的语义明明是「不干预，交给上游」。两边对同一个 nil 的解释相反，
	// 且用户看到的现象是「我明明能思考，工具却不给我开」。
	//
	// 发 true 也**不等于**替上游承诺档位：supported_reasoning 缺席仍然表示
	// 「档位由上游定」，那正是未配置状态的准确描述。两字段组合起来正好
	// 覆盖三种配置，无需第四种取值。
	//
	// 关键：这里**只影响披露**，数据面不动。数据面仍然只在
	// supports_thinking 显式为 false 时才剥掉思考（见
	// ingress.Request.ApplyThinkingCapability）—— 把「不知道」当成「不支持」会让全部未配置
	// 模型（包括 anthropic 协议的）静默失去思考能力，而那正是本函数
	// 下面那段旧注释警告过的事。
	if m := modelThinkingCapability(snap, modelID); m != nil {
		// nil（未配置）→ true：向客户端表明「可以思考」，但不下发档位集合。
		supports := true
		if m.SupportsThinking != nil {
			supports = *m.SupportsThinking
		}
		entry["supports_thinking"] = supports
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
// ingress.Request.ApplyThinkingCapability），所以这里披露的是「默认会落到
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
			authhttp.WriteAuthError(w, err, writeErr)
			return
		}

		modelID := r.PathValue("model")
		snap := snapshot.Get()
		res, rerr := snap.Routes.Resolve(modelID)
		if rerr != nil {
			writeErr(w, http.StatusNotFound, "model_not_found", dataplane.ModelNotFoundMessage(snap, modelID))
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
