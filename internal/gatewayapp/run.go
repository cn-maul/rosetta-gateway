package gatewayapp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/cn-maul/rosetta-gateway/internal/api"
	"github.com/cn-maul/rosetta-gateway/internal/billing"
	"github.com/cn-maul/rosetta-gateway/internal/config"
	"github.com/cn-maul/rosetta-gateway/internal/crypto"
	"github.com/cn-maul/rosetta-gateway/internal/lifecycle"
	"github.com/cn-maul/rosetta-gateway/internal/ratelimit"
	"github.com/cn-maul/rosetta-gateway/internal/runtime"
	"github.com/cn-maul/rosetta-gateway/internal/snapshot"
	"github.com/cn-maul/rosetta-gateway/internal/store"
	"github.com/cn-maul/rosetta-gateway/internal/upstream"
	"github.com/cn-maul/rosetta-gateway/internal/usage"
	"github.com/cn-maul/rosetta-gateway/internal/userauth"
)

// HomeEnvVar redirects all process state to a mounted volume.
const HomeEnvVar = "ROSETTA_GW_HOME"

// BootstrapFunc initializes first-run administrator credentials. It remains an
// injected concern so gatewayapp does not depend on the command package.
type BootstrapFunc func(context.Context, *store.Store, *slog.Logger) (lifecycle.BootstrapState, error)

// Options are the process-level hooks supplied by cmd/gateway.
type Options struct {
	Version   string
	Bootstrap BootstrapFunc
}

// Run initializes the gateway and serves traffic until shutdown. Configuration,
// storage, runtime state, and session initialization are owned here; the
// command package only supplies version and handler/bootstrapping hooks.
func Run(opts Options) (retErr error) {
	logLevel := new(slog.LevelVar)
	logLevel.Set(slog.LevelInfo)
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: logLevel,
	}))
	slog.SetDefault(logger)

	exePath, err := os.Executable()
	if err != nil {
		logger.Error("failed to resolve executable path", "error", err)
		return err
	}
	homeDir := resolveHome(filepath.Dir(exePath))

	logger.Info("rosetta-gateway starting", "version", opts.Version, "home_dir", homeDir)

	configPath := filepath.Join(homeDir, "config.json")
	cfg, generated, err := config.LoadOrGenerate(configPath)
	if err != nil {
		logger.Error("failed to load config", "error", err)
		return err
	}
	if generated {
		logger.Info("no config found, generated default", "path", configPath)
	}
	if !filepath.IsAbs(cfg.DBPath) {
		cfg.DBPath = filepath.Join(homeDir, cfg.DBPath)
	}

	logLevel.Set(slogLevel(cfg.LogLevel))
	logger.Info("config loaded", "listen", cfg.Listen, "db_path", cfg.DBPath, "log_level", cfg.LogLevel)

	appliedTZ, err := config.ApplyTimezone(cfg.Timezone)
	if err != nil {
		logger.Error("invalid timezone in config", "error", err)
		return err
	}
	logger.Info("daily-bucket timezone applied", "timezone", appliedTZ,
		"note", "按天统计的日界由此时区决定；容器部署若与用户所在时区不符，请在 config.json 配 timezone 或设 TZ 环境变量")

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
		logger.Error("master key unavailable, refusing to start", "error", err,
			"hint", "确认状态目录可写，或设置 "+cfg.MasterKeyEnv)
		return err
	case generatedKey:
		logger.Info("generated master key", "path", filepath.Join(homeDir, crypto.KeyFileName))
	case weakKey:
		if cfg.RequireStrongMasterKey {
			err := fmt.Errorf("master key material is weak and require_strong_master_key is set")
			logger.Error("master key material is weak (<32 chars) and require_strong_master_key is set; refusing to start",
				"error", err,
				"hint", "删除 "+filepath.Join(homeDir, crypto.KeyFileName)+" 后重启以自动生成随机主密钥，并重新保存全部上游凭据")
			return err
		}
		logger.Warn("master key material is short (<32 chars) and looks like a passphrase; " +
			"upstream API keys encrypted with it are brute-forceable offline. " +
			"Recommended: delete the weak key, restart to auto-generate a random master.key, then re-save every credential. " +
			"Or set require_strong_master_key=true to refuse to start on a weak key")
	}

	db, err := store.Open(cfg.DBPath, logger)
	if err != nil {
		logger.Error("failed to open database", "error", err)
		return err
	}
	// Never close storage unless every worker and writer has proven that it
	// exited. On an unproven join the process returns with an error and lets
	// process exit close the OS resources instead of racing a live SQL call.
	closeStore := false
	defer func() {
		if !closeStore {
			return
		}
		if err := db.Close(); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("close store: %w", err))
		}
	}()

	if err := bootstrapDB(context.Background(), db, cfg, logger, masterKey); err != nil {
		logger.Error("database bootstrap failed, refusing to start",
			"error", err,
			"impact", "配置中的上游、凭据、模型或路由未能作为完整图提交")
		return err
	}

	pool := upstream.NewPool(logger)
	pool.SetCooldownStore(db)
	reloader := runtime.New(db, masterKey, pool, cfg, logger)
	lifecycleState := lifecycle.NewManager(db, reloader)
	if err := reloader.Reload(context.Background()); err != nil {
		logger.Warn("failed to build runtime from DB, falling back to config", "error", err)
		if err := pool.BuildFromConfig(cfg); err != nil {
			logger.Error("failed to build upstream pool from config", "error", err)
		}
		snapshot.Init(buildSnapshotFromConfig(cfg))
	}

	reloadCtx, stopReloadRetry := context.WithCancel(context.Background())
	defer stopReloadRetry()
	reloadRetryDone := make(chan struct{})
	lifecycleState.AddWorkers(1)
	go func() {
		defer close(reloadRetryDone)
		defer lifecycleState.WorkerExited()
		reloader.WatchRetry(reloadCtx)
	}()

	recorder := usage.New(usage.Config{
		DB:              db,
		Logger:          logger,
		IsBalanceExempt: balanceExempt,
	})
	lifecycleState.AddWorkers(1)
	go func() {
		<-recorder.Done()
		lifecycleState.WorkerExited()
	}()

	bootstrapState := lifecycle.BootstrapUnknown
	if opts.Bootstrap != nil {
		outcome, err := opts.Bootstrap(context.Background(), db, logger)
		if err != nil {
			logger.Error("bootstrap initialization failed, refusing to start",
				"error", err,
				"outcome", outcome,
				"impact", "无法安全地建立首次管理员账号；继续监听会让初始化状态不确定",
				"hint", "检查数据库可用性与状态目录权限后重启")
			return fmt.Errorf("bootstrap initialization failed, refusing to start: %w", err)
		}
		bootstrapState = outcome
	}

	pending, err := db.FindUninitializedAdmin(context.Background())
	if err != nil {
		logger.Error("cannot read bootstrap state, refusing to start",
			"error", err,
			"impact", "无法确认首次管理员账号是否可用，继续监听会产生错误的就绪状态")
		return fmt.Errorf("read bootstrap state: %w", err)
	} else if pending != nil && !isLoopbackListen(cfg.Listen) {
		bootstrapState = lifecycle.BootstrapSetupRequired
		logger.Error("SECURITY: admin password not yet set AND listening on a non-loopback address — "+
			"anyone who can reach this port can attempt to become the first administrator; "+
			"complete first-run setup immediately",
			"listen", cfg.Listen,
			"immediate_action", "立即在浏览器打开 /admin/ 完成首次设置密码")
	} else if pending != nil {
		bootstrapState = lifecycle.BootstrapSetupRequired
		logger.Warn("admin password not set yet; open the admin UI to complete first-run setup",
			"note", "监听地址是回环，仅本机可访问；一旦改为 0.0.0.0 暴露到网络，请立即完成设置")
	}
	lifecycleState.SetBootstrap(bootstrapState)

	// TLS 契约（2026-10-22 修 A10-P0-1 残留）。
	//
	// 公开监听（0.0.0.0 等）且没有声明 https 的公开源 = 明文暴露。
	// 此前这个事实只写在 README 里，进程自己一声不吭；改了 listen 地址
	// 就悄悄变成不安全部署。
	if err := checkTLSContract(cfg, logger); err != nil {
		return err
	}

	secretPath := filepath.Join(homeDir, userauth.SecretFileName)
	sessionMgr, err := userauth.NewManager(secretPath)
	if err != nil {
		logger.Error("session secret unusable; refusing to start", "error", err)
		return err
	}
	if !sessionMgr.Enabled() {
		err := fmt.Errorf("session secret could not be loaded or generated")
		logger.Error("user sessions unavailable; refusing to start", "reason", err)
		return err
	}
	logger.Info("user sessions enabled", "ttl", sessionMgr.TTL(),
		"source", secretSource(secretPath))

	handler := api.BuildHandler(api.Deps{
		DB:          db,
		Config:      cfg,
		Logger:      logger,
		MasterKey:   masterKey,
		Pool:        pool,
		Reloader:    reloader,
		SessionMgr:  sessionMgr,
		RateLimiter: ratelimit.New(),
		Usage:       recorder,
		Lifecycle:   lifecycleState,
	})

	retErr = Serve(ServeDeps{
		DB:              db,
		Config:          cfg,
		Logger:          logger,
		MasterKey:       masterKey,
		Reloader:        reloader,
		Usage:           recorder,
		Handler:         handler,
		Lifecycle:       lifecycleState,
		ReloadRetryDone: reloadRetryDone,
	})

	lifecycleState.SetPhase(lifecycle.PhaseStopping)
	stopReloadRetry()
	allJoined := retErr == nil || !errors.Is(retErr, ErrWorkersNotJoined)
	select {
	case <-reloadRetryDone:
	default:
		select {
		case <-reloadRetryDone:
		case <-time.After(lifecycle.DefaultShutdownBudget().RuntimeRetryJoin):
			allJoined = false
		}
	}

	drainCtx, drainCancel := context.WithTimeout(
		context.Background(), lifecycle.DefaultShutdownBudget().UsageDrain)
	usageDrained := recorder.Wait(drainCtx)
	drainCancel()
	if !usageDrained {
		recorder.Abort()
		joinCtx, joinCancel := context.WithTimeout(
			context.Background(), lifecycle.DefaultShutdownBudget().UsageAbortJoin)
		if !recorder.Join(joinCtx) {
			allJoined = false
		}
		joinCancel()
		logger.Error("usage drain incomplete: records were lost during shutdown",
			"hint", "check earlier 'usage record lost' / 'usage recorder aborted' errors for request_id")
	} else {
		recorder.CloseQueue()
		if !recorder.Join(context.Background()) {
			allJoined = false
		}
	}

	if allJoined && lifecycleState.WorkersJoined() == false &&
		lifecycleState.PendingWorkers() == 0 {
		lifecycleState.MarkWorkersJoined()
	}
	if !lifecycleState.WorkersJoined() {
		allJoined = false
	}
	lifecycleState.SetPhase(lifecycle.PhaseStopped)
	if !allJoined {
		retErr = errors.Join(retErr, ErrWorkersNotJoined)
		return retErr
	}
	closeStore = true
	return retErr
}

func resolveHome(exeDir string) string {
	override := strings.TrimSpace(os.Getenv(HomeEnvVar))
	if override == "" {
		return exeDir
	}
	if abs, err := filepath.Abs(override); err == nil {
		return abs
	}
	return override
}

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

func secretSource(path string) string {
	if v := strings.TrimSpace(os.Getenv(userauth.SecretEnvName)); v != "" {
		return userauth.SecretEnvName + "（环境变量）"
	}
	return path + "（自动生成并持久化）"
}

func balanceExempt(userID string) bool {
	u := snapshot.Get().UsersByID[userID]
	return u != nil && billing.IsBalanceExempt(u.Role)
}

func isLoopbackListen(listen string) bool {
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		return false
	}
	host = strings.TrimSpace(host)
	if host == "" {
		return false
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	switch strings.ToLower(host) {
	case "localhost", "localhost.localname", "ip6-localhost", "ip6-loopback":
		return true
	}
	return false
}

// checkTLSContract 实施「公开监听且未声明 TLS」的进程级契约
// （2026-10-22 修 A10-P0-1 残留）。
//
// # 判据
//
//   - 监听回环地址 → 暴露面只有本机，明文 HTTP 可接受，不告警。
//   - 监听非回环地址（0.0.0.0 / 某个内网 IP / 域名）→ **公开监听**。
//     此时若 public_origin 的 scheme 是 https，说明有 TLS 反代在前面
//     终止 TLS —— 合规，不告警。
//     若 public_origin 为空或 scheme 是 http → 网关在**明文**上对外服务。
//
// # 为什么默认只告警
//
// 「反代 + 0.0.0.0 监听」是**正确且常见**的部署：TLS 在同机的 nginx
// 上终止，网关自己看到的仍是明文。运维忘了配 public_origin 的情况
// 也有，但那不该让进程起不来 —— 起来一条 ERROR 已经足够醒目。
// 需要硬失败的人把 require_tls_for_public_listen 置 true。
//
// # 为什么这条告警值得存在
//
// 之前这个安全事实只存在于 README 的一段说明里。改了 listen 地址、
// 或者换了个部署方式，进程不会说一个字 —— 而被动观察者能完整截获
// 管理员会话 cookie、JWT 与上游凭据。告警把它变成**运行期可见**的事实。
func checkTLSContract(cfg *config.Config, logger *slog.Logger) error {
	if cfg == nil || isLoopbackListen(cfg.Listen) {
		return nil
	}
	origin := strings.TrimSpace(cfg.PublicOrigin)
	if strings.HasPrefix(strings.ToLower(origin), "https://") {
		// 已声明 https 公开源：TLS 在反代处终止，合规。
		return nil
	}

	const detail = "网关自身不做 TLS；监听在非回环地址且未声明 https 的公开源，" +
		"管理员会话 cookie、JWT、上游凭据与下游 Authorization 头可在同网段被完整截获。"
	if cfg.RequireTLSForPublicListen {
		logger.Error("SECURITY: refusing to start — public listen without declared TLS",
			"listen", cfg.Listen,
			"public_origin", origin,
			"detail", detail,
			"fix", "在 public_origin 里声明 https:// 的公开源（前置 TLS 反代），"+
				"或把 listen 改回回环地址，或去掉 require_tls_for_public_listen")
		return errors.New("public listen without declared TLS: refusing to start")
	}

	// # 为什么区分「通配」与「指定网卡」两种非回环监听
	//
	// 0.0.0.0 / :: 表示「绑定本命名空间的全部接口」—— 它到底能不能被
	// 外部访问，取决于**进程之外**的东西：容器端口映射（把宿主端口映射到
	// 127.0.0.1 就把暴露面收回本机）、宿主机防火墙、k8s NetworkPolicy。
	// 进程看不到这些，所以只能告警。
	//
	// 而绑定到某个具体的内网/公网 IP（如 192.168.1.10:8666）是**明确的
	// 暴露意图** —— 那张网卡上的人一定能连上。那种情况用 ERROR。
	//
	// 分级的意义：容器化部署的默认模板就是 0.0.0.0（容器内必须如此），
	// 若一律 ERROR，每次启动都会刷一条红色安全日志，运维很快就会学会
	// 无视 SECURITY 前缀 —— 那比不告警更糟。
	if isWildcardListen(cfg.Listen) {
		logger.Warn("SECURITY: listening on all interfaces without declared TLS",
			"listen", cfg.Listen,
			"public_origin", origin,
			"detail", detail,
			"note", "容器部署通常需要 0.0.0.0 才能在命名空间内被映射；"+
				"若宿主端口映射限定在回环且前面有 TLS 反代，这条是可接受的",
			"recommended_fix", "前置 TLS 反代并在 public_origin 里声明 https:// 的公开源")
		return nil
	}
	logger.Error("SECURITY: bound to a specific non-loopback interface without declared TLS",
		"listen", cfg.Listen,
		"public_origin", origin,
		"detail", detail,
		"recommended_fix", "前置 TLS 反代并在 public_origin 里声明 https:// 的公开源；"+
			"或在 config.json 里设置 require_tls_for_public_listen=true 让它拒绝启动")
	return nil
}

// isWildcardListen 报告监听地址是否是「全部接口」的通配形式
// （0.0.0.0 / :: / [::]）。
//
// 通配监听的**实际暴露面由进程之外决定**（端口映射、防火墙、网络策略），
// 所以安全契约对它的处置是告警而非硬失败（见 checkTLSContract）。
func isWildcardListen(listen string) bool {
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		return false
	}
	host = strings.TrimSpace(host)
	if host == "" {
		// 空 host 在 net.Listen 里等价于「全部接口」。
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsUnspecified()
}
