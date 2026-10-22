package gatewayapp

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"

	"github.com/cn-maul/rosetta-gateway/internal/api"
	"github.com/cn-maul/rosetta-gateway/internal/billing"
	"github.com/cn-maul/rosetta-gateway/internal/config"
	"github.com/cn-maul/rosetta-gateway/internal/crypto"
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
type BootstrapFunc func(context.Context, *store.Store, *slog.Logger) error

// Options are the process-level hooks supplied by cmd/gateway.
type Options struct {
	Version   string
	Bootstrap BootstrapFunc
}

// Run initializes the gateway and serves traffic until shutdown. Configuration,
// storage, runtime state, and session initialization are owned here; the
// command package only supplies version and handler/bootstrapping hooks.
func Run(opts Options) error {
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
	defer db.Close()

	bootstrapDB(db, cfg, logger, masterKey)

	pool := upstream.NewPool(logger)
	pool.SetCooldownStore(db)
	reloader := runtime.New(db, masterKey, pool, cfg, logger)
	if err := reloader.Reload(context.Background()); err != nil {
		logger.Warn("failed to build runtime from DB, falling back to config", "error", err)
		if err := pool.BuildFromConfig(cfg); err != nil {
			logger.Error("failed to build upstream pool from config", "error", err)
		}
		snapshot.Init(buildSnapshotFromConfig(cfg))
	}

	reloadCtx, stopReloadRetry := context.WithCancel(context.Background())
	defer stopReloadRetry()
	go reloader.WatchRetry(reloadCtx)

	recorder := usage.New(usage.Config{
		DB:              db,
		Logger:          logger,
		IsBalanceExempt: balanceExempt,
	})
	defer recorder.Abort()

	if opts.Bootstrap != nil {
		if err := opts.Bootstrap(context.Background(), db, logger); err != nil {
			logger.Error("bootstrap initialization failed, refusing to start",
				"error", err,
				"impact", "无法安全地建立首次管理员账号；继续监听会让初始化状态不确定",
				"hint", "检查数据库可用性与状态目录权限后重启")
			return fmt.Errorf("bootstrap initialization failed, refusing to start: %w", err)
		}
	}

	pending, err := db.FindUninitializedAdmin(context.Background())
	if err != nil {
		logger.Error("cannot read bootstrap state; admin UI may be unreachable", "error", err)
	} else if pending != nil && !isLoopbackListen(cfg.Listen) {
		logger.Error("SECURITY: admin password not yet set AND listening on a non-loopback address — "+
			"anyone who can reach this port can attempt to become the first administrator; "+
			"complete first-run setup immediately",
			"listen", cfg.Listen,
			"immediate_action", "立即在浏览器打开 /admin/ 完成首次设置密码")
	} else if pending != nil {
		logger.Warn("admin password not set yet; open the admin UI to complete first-run setup",
			"note", "监听地址是回环，仅本机可访问；一旦改为 0.0.0.0 暴露到网络，请立即完成设置")
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
	})

	return Serve(ServeDeps{
		DB:        db,
		Config:    cfg,
		Logger:    logger,
		MasterKey: masterKey,
		Reloader:  reloader,
		Usage:     recorder,
		Handler:   handler,
	})
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
