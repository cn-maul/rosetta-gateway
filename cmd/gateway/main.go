package main

import (
	"context"
	crand "crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
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
	"github.com/cn-maul/rosetta-gateway/internal/auth"
	"github.com/cn-maul/rosetta-gateway/internal/authhttp"
	"github.com/cn-maul/rosetta-gateway/internal/config"
	"github.com/cn-maul/rosetta-gateway/internal/crypto"
	"github.com/cn-maul/rosetta-gateway/internal/dataplane"
	"github.com/cn-maul/rosetta-gateway/internal/ingress"
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

func handleIngress(pool *upstream.Pool, cfg *config.Config, usage *usageRecorder, limiter *ratelimit.Limiter, codec ingress.Codec) http.HandlerFunc {
	db, logger := usage.Store(), usage.Logger()
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()

		authCtx, err := auth.Authenticate(r)
		if err != nil {
			authhttp.WriteAuthError(w, err, codec.WriteError)
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
		// TPMLimit==0（未启用，也是主流配置）时**整段跳过**：ingress.EstimateRequestTokens
		// 会遍历全部消息与工具定义拼出一个与请求体同量级的字符串再估算，
		// 是一次纯浪费的全量分配；而它算出的 est 随后必然被丢弃。
		// 同一个请求在 attempt* 里还会再 BuildRosetta 一次，估算那次是多余的第 N 次。
		// 两种预占共用同一个 est 与同一个 dataplane.RateCommit 收尾 —— 分开放两次
		// 估算/两次收尾只会让两边在某条路径上被漏掉一处（且配额那条漏掉是
		// **终身**偏差，不像 TPM 窗口会自愈）。
		//
		// est 的计算按需触发：TPMLimit==0 且 key 未配quota 时整个函数跳过，
		// 不做那次「遍历全部消息与工具定义拼出请求体同量级字符串」的估算。
		var rate *dataplane.RateCommit
		var est int64
		needEstimate := authCtx.TPMLimit > 0
		if needEstimate {
			est = ingress.EstimateRequestTokens(ing.BuildRosetta())
			if ok, retry := limiter.ReserveTPM(authCtx.KeyID, authCtx.TPMLimit, est); !ok {
				logger.Warn("rate limited (tpm)", "key_id", authCtx.KeyID,
					"estimated_tokens", est, "retry_after", retry.String(),
					"request_id", server.RequestIDFromContext(r.Context()))
				setRetryAfter(w, retry)
				codec.WriteError(w, http.StatusTooManyRequests, "rate_limit_exceeded",
					"token rate limit exceeded for this API key")
				return
			}
			rate = dataplane.NewRateCommit(limiter, authCtx.KeyID, authCtx.TPMLimit, est, db, logger)
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
		// dataplane.RateCommit 原额退回（Commit → ReleaseQuota）。
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
				est = ingress.EstimateRequestTokens(ing.BuildRosetta())
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
				// 拒绝前必须退回 TPM 预占（2026-10-21 修 P0-8）。
				//
				// 这条早退路径此前**漏了** rate.Commit(0)：key 级配额已满
				// 时直接 return，TPM 窗口里的 est 预占就永远留在那儿。
				// 配额已耗尽的 key 可以反复触发这个拒绝，每次留下一笔含
				// max_tokens 上界的 est —— 几十次就能吃光该 key 整分钟的
				// TPM 额度，于是「配额满」会级联成「TPM 也满」，而客户端
				// 只看到一个 429。
				//
				// 注意退的是 **TPM 窗口**，不是终身配额：终身配额根本没
				// 预占成功（rok=false 时 reserved=0），没有东西要退。
				rate.Commit(0)
				codec.WriteError(w, http.StatusTooManyRequests, "insufficient_quota",
					"this API key has exhausted its token quota")
				return
			default:
				if rate == nil {
					rate = dataplane.NewRateCommit(limiter, authCtx.KeyID, 0, 0, db, logger)
				}
				rate.SetKeyQuotaReserved(reserved)
			}
		}

		// 用户级总额度**原子预占**（MULTIUSER.md §4.3：三级配额的最外层总闸）。
		//
		// # 为什么必须是预占而不是「查了再放」（2026-10-14 上线前审计修复）
		//
		// 修复前这里是裸读比较：SumUserUsedTokens → used >= quota 判定 → 放行。
		// used 只含**已落库**的真实用量，不含尚未终结的在途请求，而读-判-放行
		// 三步之间没有任何互斥。实测：配额 1000、50 并发各估 200 token，
		// 50 个请求**全部通过**（占用口径 10000 token，超发 10 倍）。
		// 原注释「并发下容忍至多一个在途请求超发」只在串行时成立。
		//
		// key 级早在 2026-10-07 就换成了 ReserveQuota 原子事务预占，用户级却
		// 没跟上 —— 两级配额一个原子一个不原子，恰好是这种「一半修好了」的
		// 形态最容易被当成「已经有限流」。
		//
		// ReserveUserQuota 把检查与占用合并进同一条写事务，并发请求被 SQLite
		// 写锁串行化，第二个进来时读到的 used+reserved 已含第一个的预占。
		//
		// 位置在 key 级之后：两级额度独立配置，**任意一级满都要拒绝**，
		// 谁先判不影响结果；放在一起共用同一个 est，少算一次估算。
		//
		// 只在用户配了额度时才走这一步：quota_tokens=0（不限额）是常态，
		// 为常态付一次写事务不划算（与 key 级「quota>0 才预占」同口径）。
		// 额度判断本身交给 ReserveUserQuota 事务内做，这里只借快照判「是否
		// 配置了额度」—— 与 key 级用 GetKeyQuota 判 quota>0 完全对称。
		if u := snapshot.Get().UsersByID[authCtx.UserID]; u != nil && u.QuotaTokens > 0 {
			if !needEstimate {
				est = ingress.EstimateRequestTokens(ing.BuildRosetta())
			}
			reserved, rok, rerr := db.ReserveUserQuota(r.Context(), authCtx.UserID, est)
			switch {
			case rerr != nil:
				// 与 key 级同口径：查询抖动 fail-open，不因一次写失败拒绝正常
				// 流量。但记 ERROR —— fail-open 的代价是真超发，无声无息就
				// 查不到了。
				logger.Error("user quota reserve failed (fail-open)",
					"error", rerr, "user_id", authCtx.UserID)
			case !rok:
				logger.Warn("user quota exceeded", "user_id", authCtx.UserID,
					"estimated_tokens", est,
					"request_id", server.RequestIDFromContext(r.Context()))
				// 拒绝前必须退回 key 级预占：它已经在同一请求里立起来了，
				// 直接 return 会让这把 key 的额度被这次被拒的请求永久吃掉一块。
				rate.Commit(0)
				codec.WriteError(w, http.StatusTooManyRequests, "insufficient_quota",
					"this account has exhausted its token quota")
				return
			default:
				if rate == nil {
					rate = dataplane.NewRateCommit(limiter, authCtx.KeyID, 0, 0, db, logger)
				}
				rate.SetUserQuotaReserved(authCtx.UserID, reserved)
			}
		}

		snap := snapshot.Get()
		res, err := snap.Routes.Resolve(ing.Model)
		if err != nil {
			rate.Commit(0)
			codec.WriteError(w, http.StatusNotFound, "model_not_found", modelNotFoundMessage(snap, ing.Model))
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
			rate.Commit(0)
			logger.Warn("model not allowed for caller",
				"model", route.PublicName, "key_id", authCtx.KeyID, "user_id", authCtx.UserID,
				"request_id", server.RequestIDFromContext(r.Context()))
			codec.WriteError(w, http.StatusForbidden, "model_not_allowed",
				fmt.Sprintf("model %q is not available for this API key", ing.Model))
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
		budget := dataplane.TotalRequestBudget(snap, cfg)
		ctx, cancelBudget := context.WithTimeout(r.Context(), budget)
		defer cancelBudget()
		r = r.WithContext(ctx)
		logger.Debug("request total budget armed",
			"budget_ms", budget.Milliseconds(),
			"model", ing.Model,
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
			// 余额预检已写回 402/500。rate.Commit(0) 不能省：走到这里说明
			// TPM/配额预占可能已经立起来了（needEstimate 为真时），不退的话
			// 它们会一直占着窗口/终身额度直到过期。与上面 model_not_allowed
			// 的收尾同口径。
			rate.Commit(0)
			return
		}

		// 尝试预算：链上按 position 升序最多打 maxTargets 个目标。
		// 未开启故障转移 → 只打主目标（等价于改造前的单目标行为，零回归）。
		// 策略值统一来自「设置」页写入的全局默认（快照），config 只作兜底。
		cands := res.Candidates
		limit := len(cands)
		if !route.FailoverEnabled {
			limit = 1
		} else if budget := dataplane.FailoverMaxTargets(snap, cfg); budget < limit {
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
		if ing.RequiresStructuredOutput {
			var serviceable []routing.Candidate
			for _, c := range active {
				if c.Provider != nil && c.Provider.Protocol != "anthropic" {
					serviceable = append(serviceable, c)
				}
			}
			if len(serviceable) == 0 {
				rate.Commit(0)
				logger.Warn("no upstream can serve structured output request",
					"model", ing.Model, "key_id", authCtx.KeyID,
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

		threshold := dataplane.FailoverFailureThreshold(snap, cfg)
		keyID := authCtx.KeyID

		var out dataplane.Outcome
		// lastTried 记录实际尝试到哪个候选（链耗尽时据此归因，不猜链上最后一个）。
		// triedAny 与之配对：Candidate 是值类型，「没试过」与「试过零值」无法
		// 从 lastTried 本身区分，必须有独立布尔。
		var lastTried routing.Candidate
		var triedAny bool
		// routeTrace 记录链上的失败决策（2026-10-17）。只在真的发生过转移时
		// 才有内容 —— 单目标成功、首跳成功都不写，避免给每条记录加噪声。
		// 由 dataplane.AppendRouteTrace 统一格式化，保证「第几个目标」与失败原因成对出现。
		var routeTrace []string
		for i, cand := range active {
			isLast := i == len(active)-1

			// 客户端已断开就收手：半路跑掉的人不该消耗整条链的下游配额，
			// 也不该把一次在途取消误记成目标的失败。此刻尚未写出任何字节，
			// 直接返回、不记 error usage（断流是客户端行为，不是上游故障）。
			// 此处还没领探测名额，不存在归还义务。
			if cerr := r.Context().Err(); cerr != nil {
				logger.Info("client disconnected, aborting failover",
					"model", ing.Model, "attempt", i+1, "error", cerr,
					"request_id", server.RequestIDFromContext(r.Context()))
				rate.Commit(0)
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
			//
			// threshold<=0（运维显式关闭目标级熔断）时**完全跳过**：
			// 熔断器既然不启用，就不该再拿它的半开名额去限流 ——
			// 否则运维「关了熔断」得到的仍是「被限成一个并发 + 静默
			// 改投次目标」（见 upstream.Pool.RecordTargetFailure 的
			// threshold<=0 分支，那条路径已经把 recovering 清掉了）。
			var probeToken upstream.ProbeToken
			probeClaimed, probeRecorded := false, false
			if route.FailoverEnabled && threshold > 0 {
				var claimed bool
				probeToken, claimed = pool.ClaimTargetProbe(cand.TargetID)
				if !claimed {
					// 仍在冷却中，或探测名额已被同链的并发请求领走 —— 沿链继续。
					// 没领到名额，也就没有归还义务。
					routeTrace = dataplane.AppendRouteTrace(routeTrace, i+1, cand, "skip", "probe_busy_or_cooling")
					if !isLast {
						logger.Warn("failover: target not claiming a probe slot, switching target",
							"model", ing.Model, "provider", cand.Provider.Slug, "attempt", i+1,
							"request_id", server.RequestIDFromContext(r.Context()))
						continue
					}
					out = dataplane.Outcome{Eligible: true, StatusCode: http.StatusBadGateway,
						Code: "upstream_error", Message: "no available upstream provider"}
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
				// 该目标当前无健康凭据：这是 provider 的凭据状态，不是目标/上游
				// 故障。把目标熔断与凭据冷却叠加会延长恢复时间，而且下一次
				// 请求仍会先被目标熔断挡住，看不到“补一把 key 就好”。
				if probeClaimed {
					pool.ReleaseTargetProbe(cand.TargetID, probeToken)
				}
				probeRecorded = true // 名额已在上方显式归还
				out = dataplane.Outcome{Eligible: true, StatusCode: http.StatusBadGateway,
					Code: "upstream_error", Message: "no available upstream provider"}
				routeTrace = dataplane.AppendRouteTrace(routeTrace, i+1, cand, "fail", "no_healthy_credential")
				if !isLast {
					logger.Warn("failover: no healthy credential, switching target",
						"model", ing.Model, "provider", cand.Provider.Slug, "request_id", server.RequestIDFromContext(r.Context()))
					continue
				}
				break
			}

			if ing.Stream {
				out = attemptStream(w, r, client, ing, cand, authCtx, codec, rate, cfg, snap, usage, start, routeTrace)
			} else {
				out = attemptNonStream(w, r, client, ing, cand, authCtx, codec, rate, cfg, snap, usage, start, routeTrace)
			}
			// 记住**实际尝试到**的候选，供链耗尽时记账归因。
			// 不能用 active[len(active)-1] —— 那是链上最后一个候选，而实际最后
			// 尝试的可能是链上第一个（failover_max_targets=1、或前面的目标被
			// 跳过时）。归因错目标会让排障指向一个从未真正打过的上游。
			lastTried = cand
			triedAny = true

			if out.Committed {
				// 只有真成功才记成功 —— 断流/溢出/上游错误虽已提交，却是失败，
				// 必须让目标熔断计数与凭据健康照常累计，否则故障永不转移。
				if out.Success {
					pool.RecordCredentialSuccess(credID)
					pool.RecordTargetSuccess(cand.TargetID, probeToken)
					routeTrace = dataplane.AppendRouteTrace(routeTrace, i+1, cand, "ok", "")
				} else if out.ClientGone {
					// 客户端自己走了（用户点停止 / 进程退出 / 网络切换）。
					// 这既不是上游故障也不该罚凭据：照记的话，连续三次
					// 取消就会把健康目标熔断一整个冷却窗口，期间该路由
					// 全量 502。凭据同样不冷却 —— key 本身没问题。
					routeTrace = dataplane.AppendRouteTrace(routeTrace, i+1, cand, "committed_fail", "client_canceled")
					pool.RecordTargetSuccess(cand.TargetID, probeToken)
					pool.RecordCredentialSuccess(credID)
				} else {
					pool.RecordTargetFailure(cand.TargetID, probeToken, threshold)
					routeTrace = dataplane.AppendRouteTrace(routeTrace, i+1, cand, "committed_fail", out.Code)
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
			// 判据只认 Canceled：请求 ctx 上还挂着 dataplane.TotalRequestBudget 的超时，
			// 用 ctx.Err()==nil 会把「预算到期」也一起放过去 —— 那是网关侧的
			// 事件，但请求确实已经打到了这个目标，用它当「上游健康」的证据
			// 同样不成立，所以这里只排除真正的客户端取消。
			if out.Eligible && !dataplane.IsClientGone(r.Context(), nil) {
				pool.RecordTargetFailure(cand.TargetID, probeToken, threshold)
				probeRecorded = true // 名额由记账释放
				if out.CredentialCooldown > 0 {
					pool.MarkCredentialCooldown(credID, out.CredentialCooldown)
				}
			}

			// 未提交的失败：记下「哪个目标、什么原因、是否可转移」。
			// 不可转移同样要记 —— 运维最常问的正是「为什么这次没换上游」。
			if out.Eligible {
				routeTrace = dataplane.AppendRouteTrace(routeTrace, i+1, cand, "fail_switch", out.Code)
			} else {
				routeTrace = dataplane.AppendRouteTrace(routeTrace, i+1, cand, "fail_terminal", out.Code)
			}

			// 没走到记账的两种形态 —— 不可转移错误（out.Eligible==false）与
			// 客户端中断（ctx 已取消）—— 在此归还探测名额，否则 halfOpen 卡死、
			// 该目标被跳到下一次 Install 才复位。已记账的绝不能再还；
			// ReleaseTargetProbe 按令牌比较，迟到的旧请求也清不掉新探针。
			if probeClaimed && !probeRecorded {
				pool.ReleaseTargetProbe(cand.TargetID, probeToken)
			}

			if !out.Eligible || isLast {
				break
			}
			logger.Warn("failover: switching to next target",
				"model", ing.Model, "from_provider", cand.Provider.Slug,
				"error_code", out.Code, "attempt", i+1, "route_trace", strings.Join(routeTrace, "; "),
				"request_id", server.RequestIDFromContext(r.Context()))
		}

		// 走到这里 = 最后一次尝试未提交（要么不可转移错误、要么链已耗尽）。
		// 写出最终错误并记一条 usage —— 若 active 为空（理论不该发生）兜底成 502。
		// 客户端已经断开时它根本收不到这个响应（写了只会撞 broken pipe），
		// 且这次失败与上游无关，按 canceled 记账，与流式路径同一口径。
		// 只把「客户端真的走了」算作 clientGone：ctx 上还挂着
		// dataplane.TotalRequestBudget 的超时（DeadlineExceeded），那是网关侧事件，
		// 记成 canceled 会让网关自己的超时在报表里与用户主动取消混为一谈，
		// 并让「写不写错误响应」这个决定被预算到期误触发。
		clientGone := dataplane.IsClientGone(r.Context(), r.Context().Err())

		statusCode, code, message := out.StatusCode, out.Code, out.Message
		// 兜底：不许把 200 或 0 当作「失败结果」发出去。
		//
		// statusCode==0 是「没有候选目标」的哨兵，一直有 502 兜底。
		// statusCode==200 则是**本该不可能**的状态：这条路径只在链已耗尽、
		// 响应尚未提交时到达，任何失败都必须用 >=400 表达。修复前它真的发生过
		// （见 dataplane.OutcomeFromErr 的注释：流式哨兵被映射成 200），客户端于是拿到
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
		rate.Commit(0)
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
				"model", ing.Model, "request_id", server.RequestIDFromContext(r.Context()))
		}
		errorCode := code
		errorDetail := out.Detail
		if clientGone {
			errorCode = ""
			errorDetail = ""
		}
		usage.Record(&store.UsageRecord{
			ID:              generateID(),
			AccessKeyID:     keyID,
			UserID:          authCtx.UserID,
			RequestID:       server.RequestIDFromContext(r.Context()),
			PublicModel:     ing.Model,
			ProviderID:      provID,
			UpstreamModel:   upstreamModel,
			IngressProtocol: codec.Name(),
			Stream:          ing.Stream,
			UsageState:      "none",
			Status:          status,
			HTTPStatus:      statusCode,
			ErrorCode:       errorCode,
			ErrorDetail:     errorDetail,
			LatencyMs:       time.Since(start).Milliseconds(),
			RouteTrace:      strings.Join(routeTrace, "; "),
		})
	}
}

// modelNotFoundMessage 拼「模型不存在」的文案，并在能猜到时给出候选。
//
// # 为什么值得加这一段（magpie 的 unknownModel 做法）
//
// 只回 `model "xxx" not found` 时，用户面对三个无法区分的可能性：
// 名字打错了 / 这个模型压根没配 / 名字对但我没权限。三个的后续动作完全不同，
// 而原文什么都不说 —— 于是最常见的一类接入故障（打错字）需要用户自己去翻
// 管理界面逐个比对才能排除。
//
// 追加候选把这一步消掉。刻意**只列名字、不列数量**以外的任何配置信息：
// 这条错误会回给持有任一有效 key 的调用方，列出全部可调模型等于把网关的
// 模型清单暴露给任何一个拿到 key 的人（哪怕该 key 的白名单本应只许调一个）。
//
// 鉴权口径：路由索引是全局的，不按身份收窄 —— 但 model_not_found 之前
// 已经过了 auth 与白名单（白名单在下面才判），能走到这里的调用方本来就有
// 一把有效 key。列候选名不新增暴露面，因为同一把 key 调
// GET /v1/models 就能拿到同一份清单。
func modelNotFoundMessage(snap *snapshot.Snapshot, requested string) string {
	base := fmt.Sprintf("model %q not found", requested)
	if snap == nil || snap.Routes == nil {
		return base
	}
	sugg := snap.Routes.SuggestModel(requested, 5)
	if len(sugg) == 0 {
		return base + "（该名字没有匹配到已配置的模型；用 GET /v1/models 查看可用清单）"
	}
	return fmt.Sprintf("%s。可用的相似模型：%s", base, strings.Join(sugg, ", "))
}

// attemptStream 尝试在当前目标上完成一次流式响应。
//
// 关键：SSE 头与状态码**推迟到拿到第一个上游事件之后才写**。这样「建立失败」
// 和「首字迟迟不来」都发生在向下游写出任何字节之前，可安全地让外层循环换目标；
// 一旦写了头并提交首个事件，就再无回退余地（DESIGN §414 的约束）。
//
// 本函数只做与协议无关的事：看门狗、心跳、事件循环、断流状态归类、落库；
// 「统一事件 → 下游协议分片」全部经由 codec.NewSink 的 StreamSink 完成。
// priorTrace 是外层循环在本目标之前已经做出的决策（例如「第 1 个目标 404，
// 可转移」）。它必须带进来：本 attempt 一旦提交（写下首字节），usage 就在
// 这里落库且不会再回到外层 —— 只写当前一跳会丢掉「为什么走到备用上游」，
// 而那恰恰是这条记录最有价值的部分。
func attemptStream(w http.ResponseWriter, r *http.Request, client *rosetta.Client, ing *ingress.Request, cand routing.Candidate, authCtx *auth.Context, codec ingress.Codec, rate *dataplane.RateCommit, cfg *config.Config, snap *snapshot.Snapshot, usage *usageRecorder, start time.Time, priorTrace []string) dataplane.Outcome {
	ctx := r.Context()
	logger := usage.Logger()
	publicModel := ing.Model

	// 每个 attempt 现造一份请求，并把公共别名换成该目标的上游 model_id、
	// 挂上该上游协议的私有透传字段。必须在 attempt 内部做（而不是在调用点
	// 造好传进来）：链上各目标的协议不同，Extra 形状也不同，复用同一实例
	// 会把 A 目标的字段泄漏给 B 目标。
	upstreamReq := ing.BuildRosetta()
	upstreamReq.Model = cand.UpstreamModel.ModelID
	ing.ApplyUpstreamExtras(upstreamReq, cand.Provider.Protocol)
	ing.ApplyThinkingCapability(upstreamReq, cand.UpstreamModel, cand.Provider.Protocol)
	// 剥掉目标协议回放不了的思考块（chat 全剥 / anthropic 剥无签名的），
	// 否则 harness 回传 thinking 后的第二轮必然 400。详见 replay_gate.go。
	applyReplayGate(upstreamReq, cand.Provider.Protocol)

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
	// 并计入 dataplane.TotalRequestBudget —— 它不会无限，只是比首字预算宽。
	ttftTimeout := dataplane.FirstTokenTimeout(snap, cfg)

	stream, err := client.ChatStream(ctx, upstreamReq)
	if err != nil {
		return dataplane.OutcomeFromErr(err)
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
			return dataplane.OutcomeFromErr(cerr)
		}
		if ttftTimedOut.Load() {
			logger.Warn("stream first-token timeout", "model", publicModel,
				"provider", cand.Provider.Slug, "ttft_timeout_ms", ttftTimeout.Milliseconds())
			return dataplane.Outcome{Eligible: true, CredentialCooldown: 60 * time.Second,
				StatusCode: http.StatusGatewayTimeout, Code: "upstream_timeout",
				Message: "upstream first-token timeout"}
		}
		// 干净 EOF 却在首个事件之前（上游返回 200 后立刻空流）—— 按可转移的空响应处理。
		return dataplane.Outcome{Eligible: true, StatusCode: http.StatusBadGateway,
			Code: "upstream_error", Message: "upstream returned empty response"}
	}

	// —— 首个事件已到，自此提交：写 SSE 头，之后任何中断都只能如实 truncated 收尾 ——
	var ttfbMs int64
	ttfbMs = time.Since(start).Milliseconds()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	// 流式编码器由 codec 构造：OpenAI 走 SSEWriter，Anthropic 走块状态机。
	sink := codec.NewSink(w, publicModel, ing)
	// 单次写截止时间（2026-10-17）：下游挂死不读时让 Write 自己返回超时，
	// 而不是让 handler 永久阻塞、连带把上游流与 token 一起耗着。
	// 用法是能力探测，测试替身等无底层连接的实现会静默跳过。
	outwire.BindWriteDeadline(sink, w)

	idleTimeout := dataplane.StreamIdleTimeout(snap, cfg)
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
	// errorDetail 只在 default 分支（上游真的返回了错误）填 —— truncated /
	// overflow / idle_timeout 是「流断了」而不是「上游回了错误正文」，
	// canceled 更是客户端自己的行为，都不该有详情。
	errorDetail := ""
	httpStatus := 200

	// partialOutRunes 累计本次流里**已经收到**的输出侧字符数（正文 + 思考）。
	//
	// 它的唯一用途是「客户端中途断开」这一条路径：见 ingress.PartialOutTokens 的注释。
	partialOutRunes := 0

	// consume 消费一个上游事件（首个 + 后续走同一套逻辑）：看门狗续期与
	// usage/stopReason 留档是循环的职责，协议编码全部交给 sink。
	consume := func(ev *rosetta.Event) {
		// 时刻先于 Reset 落账（顺序不能反，见 lastEventAt 声明处的注释），
		// 然后 idle 看门狗续期、心跳对齐。
		lastEventAt.Store(time.Now().UnixNano())
		idleTimer.Reset(idleTimeout)
		heartbeatTicker.Reset(heartbeatInterval)
		switch ev.Type {
		case rosetta.EventMessageEnd:
			if ev.Usage != nil {
				lastUsage = *ev.Usage
			}
			stopReason = ev.StopReason
		case rosetta.EventTextDelta, rosetta.EventThinkingDelta:
			// 只数**正文与思考**：工具调用的参数是 JSON 片段，
			// 混进 token 估算会让数字失去意义（半个 JSON 对象既不是
			// 中文也不是英文，按字符数估会离谱地偏高）。
			partialOutRunes += len([]rune(ev.Text))
		}
		sink.Event(ev)
	}

	consume(stream.Event())
	for stream.Next() {
		consume(stream.Event())
		// 下游连接已不可用（2026-10-21 修 *X12）：继续把上游流读完只会把
		// 字节写进一个死连接，而上游还在按 token 计费、我们还在为它付钱。
		// 立即关流，让下面的写失败分支收尾。
		//
		// 关流用 Close（而非 Abort）：这是**我们**主动不看了，不是上游
		// 出错 —— Close 保持 Err()==nil，于是终态由写失败分支给出，
		// 不会被误记成上游故障进熔断计数。
		if outwire.FirstWriteError(sink) != nil {
			_ = stream.Close()
			break
		}
	}

	// 终态判定顺序（2026-10-21 修 *X12）——顺序本身就是 bug：
	//
	// **写失败必须先于 stream.Err() 判**，否则这条路径永远走不到：主循环
	// 发现写失败后 Close 并 break，而 SDK 的生产 goroutine 可能已经先一步把
	// 「EOF 却没有 [DONE]」折成 ErrStreamTruncated 写进 s.err。于是
	// stream.Err() 非空、进 truncated 分支：残缺流被判成「上游截断」——
	// 既进熔断计数，又让运维看不出是下游写不进去。测试 F1c 正是这一种。
	//
	// 另一个理由不依赖竞态：写失败意味着**我们主动放弃了这条流**，之后
	// 发生的一切都是放弃的后果。把下游不可用记成上游故障，会让熔断器
	// 因为自己的客户端掉线而惩罚上游。
	//
	// 两者同时出现时 Stream 端的错误仍要留痕：根因可能真的是上游，
	// 只是被下游写失败抢先报了。降一级到 Debug 是为了不让每次客户端
	// 断网都刷一条 WARN。
	if werr := outwire.FirstWriteError(sink); werr != nil {
		// 向下游写失败（2026-10-21 修 *X12）。
		//
		// 三个 sink 的 SSE 写点此前**全部**丢弃 fmt.Fprintf 的返回值，
		// 于是下游挂死/断开后状态机照常推进、残缺的流被当成 status=ok
		// 收尾：既不进熔断计数，用量里也无痕。sink 现在记下第一次写失败
		// （见 outwire.WriteErrorReporter），这里把它变成终态。
		//
		// 归因：写失败 = 下游不可用，与「客户端主动断开」同类，
		// 因此复用 canceled 这个取值（它已经是「不计入上游熔断」的那一类，
		// 见 dataplane.Outcome.ClientGone）。单独一个 errorCode 让运维能
		// 区分「客户端自己走了」与「我们写不进去」。
		status = "canceled"
		errorCode = "downstream_write_failed"
		if serr := stream.Err(); serr != nil {
			logger.Debug("stream error observed after downstream write failure",
				"model", publicModel, "key_id", authCtx.KeyID, "stream_error", serr)
		}
		if outwire.IsDownstreamWriteError(werr) {
			logger.Info("downstream write failed mid-stream",
				"model", publicModel, "key_id", authCtx.KeyID, "error", werr)
		} else {
			// 认不出来的写错误可能是网关自身的 bug，不该悄悄算到客户端头上。
			logger.Error("unexpected response write error",
				"model", publicModel, "key_id", authCtx.KeyID, "error", werr)
		}
	} else if err := stream.Err(); err != nil {
		switch {
		case errors.Is(err, rosetta.ErrStreamTruncated):
			status = "truncated"
			errorCode = "stream_truncated"
			logger.Warn("stream truncated", "model", publicModel, "key_id", authCtx.KeyID, "error", err)
		case errors.Is(err, rosetta.ErrStreamOverflow):
			status = "overflow"
			errorCode = "stream_overflow"
			logger.Error("stream overflow", "model", publicModel, "key_id", authCtx.KeyID, "error", err)
		case dataplane.IsClientGone(ctx, err):
			// 客户端主动断开不是上游故障。此前一律记 error，后果是：后台错误率虚高、
			// 成功率虚低，且真故障被 ERROR 噪音淹没。单独一个取值才能把两者分开。
			status = "canceled"
			logger.Info("client disconnected mid-stream", "model", publicModel, "key_id", authCtx.KeyID, "error", err)
		default:
			status = "error"
			errorCode = "upstream_error"
			errorDetail = outwire.UpstreamErrorDetail(err)
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
	sink.Finish(status, stopReason, lastUsage, ing.WantsStreamUsage)

	latency := time.Since(start).Milliseconds()

	// 客户端中途断开时的用量兜底（2026-10-21 修 A4-P1-1 / 汇总 F1）。
	//
	// # 问题
	//
	// `lastUsage` 只在 EventMessageEnd 上赋值，而客户端断开时上游**不会**
	// 发出该事件 —— 于是 usage 恒为零值：
	//   - usage_state="missing" → cost_total=0 → 网关的上游成本被完全漏记；
	//   - trg_update_used_tokens 累加 0 → key 的终身 token 配额不涨；
	//   - rate.ReleaseQuota() 把 est 预占全额退回 → 预占也不涨。
	//
	// 三者叠加的结果是：**用户点一次「停止」，就能免费消耗已生成的全部
	// 上游 token，并且完全绕过终身 token 配额**。可被主动利用 ——
	// 收第一个 token 就断开，重复 N 次。
	//
	// # 为什么不能只靠「不退预占」解决
	//
	// 只不退预占能堵住配额绕过，但 cost_total 仍是 0：报表上的「上游成本」
	// 依旧被系统性低估（每条 canceled 记录都是 0），而 cost_total 无论
	// 成败都记是既有口径（那是网关的成本，不是用户的应收）。
	//
	// # 这里的处置
	//
	// 用**已经收到的输出字符数**折一个保守下界，作为本次调用的用量落库：
	//   - 输出 token 数 = 已收字符数按 1 token/4 字符（ASCII）折算，
	//     不足 1 个也算 1（有内容就至少 1 个 token）；
	//   - 输入 token 数用**请求前就有的估算值**（est 的输入部分）。
	//
	// 为什么不精确到分毫：客户端断开时上游到底生成了多少 token **无从得知**
	//（SDK 的 usage 只从终止事件派生，abort 路径不产生该事件）。
	// 用「已收到的字符数」是唯一有依据的下界 —— 它不会高估（收到的就是上游
	// 真吐出来的），也不会是 0。
	//
	// # 为什么不收用户钱
	//
	// 落库的 status 仍是 "canceled" → usageRecorder.charge 只对 status=="ok"
	// 扣费（见该函数注释）。所以这条路径**只影响成本口径与配额**，
	// 不会变成「用户为一次没拿到结果的调用付钱」。这与既有口径一致：
	// 扣费只看成功，成本无论成败都记。
	//
	// # 什么时候不兜底
	//
	// 上游报了 usage（!lastUsage.IsZero()）→ 用真实值，不兜底。
	// 一个字符都没收到（partialOutRunes==0）→ 兜底也兜不出什么，
	// 保持 missing —— 这时「什么都没生成」是事实，不该编一个数字。
	if lastUsage.IsZero() && partialOutRunes > 0 {
		estOut := ingress.PartialOutTokens(partialOutRunes)
		estIn := ingress.EstimateInputTokensOnly(ing.BuildRosetta())
		lastUsage = rosetta.Usage{
			InputTokens:  estIn,
			OutputTokens: estOut,
			TotalTokens:  estIn + estOut,
		}
		logger.Warn("stream ended without upstream usage; charging a conservative lower bound",
			"model", publicModel, "key_id", authCtx.KeyID,
			"status", status,
			"received_runes", partialOutRunes,
			"estimated_output_tokens", estOut,
			"estimated_input_tokens", estIn,
			"note", "客户端中断时上游不发终止事件，usage 无从得知；按已收内容折算下界")
	}

	usage.Record(&store.UsageRecord{
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
		ErrorDetail:     errorDetail,
		LatencyMs:       latency,
		TTFBMs:          ttfbMs,
		// 流式已提交后的断流无法再转移，但「打到了哪个目标、为什么断」
		// 仍需可查。成功时留 ok，断流留对应状态。
		RouteTrace: strings.Join(dataplane.AppendRouteTrace(priorTrace, len(priorTrace)+1, cand, dataplane.StreamCommitOutcome(status), errorCode), "; "),
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
	//
	// 客户端中断的路径在上一步已经被兜底成「有 usage 了」（partialOutRunes>0
	// 时），所以走的是上面那条「已知」分支：TPM 按保守下界校正，
	// 终身配额的预占被**消耗**掉一部分（触发器按 total_tokens 累加），
	// 而不再全额退回 —— 那正是「点停止 = 免费刷上游 token」这条通道。
	if !lastUsage.IsZero() {
		rate.Commit(lastUsage.TotalTokens)
	} else {
		rate.ReleaseQuota()
	}
	// committed=true（字节已写出、无法回退换目标）与「这一次算成功」是两件事：
	// truncated / overflow / error / canceled 都已提交，但对上游而言是失败。
	// 只有 status=="ok" 才允许外层记成功，否则「先200 再断流」这类最常见的
	// 上游故障永远不累计失败、目标永不熔断。
	// clientGone 只给「客户端自己走了」的那一种：它不该计入熔断计数
	// （见 dataplane.Outcome.ClientGone）。
	return dataplane.Outcome{Committed: true, Success: status == "ok", ClientGone: status == "canceled"}
}

// priorTrace 的含义与 attemptStream 相同：提交后的落库在这里发生，
// 必须把此前目标的失败决策一并带上。
func attemptNonStream(w http.ResponseWriter, r *http.Request, client *rosetta.Client, ing *ingress.Request, cand routing.Candidate, authCtx *auth.Context, codec ingress.Codec, rate *dataplane.RateCommit, cfg *config.Config, snap *snapshot.Snapshot, usage *usageRecorder, start time.Time, priorTrace []string) dataplane.Outcome {
	publicModel := ing.Model
	// 同attemptStream：别名 → 上游 model_id 的映射与协议私有字段的透传
	// 必须在本次 attempt 内完成，不能由调用方造好传入。
	upstreamReq := ing.BuildRosetta()
	upstreamReq.Model = cand.UpstreamModel.ModelID
	ing.ApplyUpstreamExtras(upstreamReq, cand.Provider.Protocol)
	ing.ApplyThinkingCapability(upstreamReq, cand.UpstreamModel, cand.Provider.Protocol)
	// 与 attemptStream 同一刀，理由见 replay_gate.go。
	applyReplayGate(upstreamReq, cand.Provider.Protocol)

	// 绑定 r.Context() 而非 context.Background()：客户端断开时上游调用应随之取消，
	// 否则断连请求会一直占用上游连接与配额直到超时（默认 120s）。
	ctx, cancel := context.WithTimeout(r.Context(), dataplane.NonStreamTimeout(snap, cfg))
	defer cancel()

	resp, err := client.Chat(ctx, upstreamReq)
	if err != nil {
		return dataplane.OutcomeFromErr(err)
	}

	// 非流式响应一次性返回，拿不到"首字"这一独立时刻，用响应到达时刻近似（≈总耗时）。
	latency := time.Since(start).Milliseconds()
	ttfbMs := latency

	codec.WriteNonStream(w, resp, publicModel, ing)

	usage.Record(&store.UsageRecord{
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
		RouteTrace:      strings.Join(dataplane.AppendRouteTrace(priorTrace, len(priorTrace)+1, cand, "ok", ""), "; "),
	})
	// 收尾口径与 attemptStream 一致（见该处的注释）：usage 已知 → 全量收尾；
	// missing → 只退配额预占，TPM 预占保留（窗口自愈）。
	if !resp.Usage.IsZero() {
		rate.Commit(resp.Usage.TotalTokens)
	} else {
		rate.ReleaseQuota()
	}
	// 非流式能走到这里就意味着上游完整返回了响应 —— 一定是成功。
	// 漏写 Success 会让每次非流式成功都被外层记成「目标失败」，
	// 健康的链首目标 3 个请求后就被误熔断（详见 dataplane.Outcome.Success）。
	return dataplane.Outcome{Committed: true, Success: true}
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
			writeErr(w, http.StatusNotFound, "model_not_found", modelNotFoundMessage(snap, modelID))
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
