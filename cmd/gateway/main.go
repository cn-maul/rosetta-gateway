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
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/cn-maul/rosetta"
	"github.com/cn-maul/rosetta-gateway/internal/admin"
	"github.com/cn-maul/rosetta-gateway/internal/adminauth"
	"github.com/cn-maul/rosetta-gateway/internal/auth"
	"github.com/cn-maul/rosetta-gateway/internal/config"
	"github.com/cn-maul/rosetta-gateway/internal/crypto"
	"github.com/cn-maul/rosetta-gateway/internal/inwire"
	"github.com/cn-maul/rosetta-gateway/internal/outwire"
	"github.com/cn-maul/rosetta-gateway/internal/routing"
	"github.com/cn-maul/rosetta-gateway/internal/server"
	"github.com/cn-maul/rosetta-gateway/internal/snapshot"
	"github.com/cn-maul/rosetta-gateway/internal/store"
	"github.com/cn-maul/rosetta-gateway/internal/upstream"
	"github.com/cn-maul/rosetta-gateway/internal/webui"
)

// homeEnvVar 是状态根目录的环境变量名。
//
// 容器镜像靠它把 config.json、master.key、admin_auth.json 与数据库整体
// 指到挂载卷上，让镜像层保持无状态（见 DOCKER.md）。
const homeEnvVar = "ROSETTA_GW_HOME"

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

	masterKey, generatedKey, err := crypto.LoadMasterKey(cfg.MasterKeyEnv, homeDir)
	switch {
	case err != nil:
		logger.Warn("master key unavailable, credential encryption disabled", "error", err)
		masterKey = nil
	case generatedKey:
		logger.Info("generated master key", "path", filepath.Join(homeDir, crypto.KeyFileName))
	}

	db, err := store.Open(cfg.DBPath, logger)
	if err != nil {
		logger.Error("failed to open database", "error", err)
		os.Exit(1)
	}
	defer db.Close()

	bootstrapDB(db, cfg, logger, masterKey)

	pool := upstream.NewPool(logger)
	reloader := &runtimeReloader{db: db, masterKey: masterKey, pool: pool, cfg: cfg}
	if err := reloader.Reload(context.Background()); err != nil {
		logger.Warn("failed to build runtime from DB, falling back to config", "error", err)
		if err := pool.BuildFromConfig(cfg); err != nil {
			logger.Error("failed to build upstream pool from config", "error", err)
		}
		snapshot.Init(buildSnapshotFromConfig(cfg))
	}

	usage := newUsageRecorder(db, logger)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", handleChatCompletions(pool, cfg, usage))
	mux.HandleFunc("GET /v1/models", handleListModels())

	// 管理端凭据。两个来源，优先级：用户在后台设置的密码（admin_auth.json）
	// > config.json 的 admin_token（或 ADMIN_TOKEN 环境变量）作为兜底。
	//
	// 凭据放在可执行文件同级而不是数据库里，理由见 internal/adminauth 的包注释：
	// gateway.db 是「可丢弃的运行时数据」（删库重建是常规操作），
	// 而管理员密码是身份凭据 —— 放库里等于「删库 = 把自己锁在门外」。
	adminToken := cfg.AdminToken
	if adminToken == "" {
		adminToken = os.Getenv("ADMIN_TOKEN")
	}

	authPath := adminauth.ResolvePath(homeDir)
	authStore, err := adminauth.Open(authPath, adminToken)
	switch {
	case err != nil:
		// 凭据文件坏了**不能**让进程起不来：/v1 数据面根本不读管理凭据，
		// 为一份坏掉的管理凭据把全部转发拖死完全不成比例（一次磁盘写坏、
		// 一次手工编辑失误 = 全部转发服务中断）。降级成锁定态：
		// 后台进不去、也绝不放行设置新密码，但转发照常。
		logger.Error("admin credential file unusable, admin API locked",
			"path", authPath, "error", err,
			"recovery", "删除该文件后重启：改用 config.json 的 admin_token，或重新设置密码")
		authStore = adminauth.NewLocked(authPath, err)
	case authStore.HasUserPassword():
		logger.Info("admin password loaded", "path", authStore.Path())
	case adminToken != "":
		logger.Info("using admin_token from config; set a password in the admin UI to override it")
	default:
		logger.Warn("no admin credential configured; the admin UI will ask you to set a password on first visit")
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
	usageHandler := admin.NewUsageHandler(db)
	passwordHandler := admin.NewPasswordHandler(authStore)

	adminMux := http.NewServeMux()
	adminMux.HandleFunc("GET /admin/api/providers", providerHandler.List)
	adminMux.HandleFunc("POST /admin/api/providers", providerHandler.Create)
	adminMux.HandleFunc("GET /admin/api/providers/{id}", func(w http.ResponseWriter, r *http.Request) { providerHandler.Get(w, r, r.PathValue("id")) })
	adminMux.HandleFunc("PATCH /admin/api/providers/{id}", func(w http.ResponseWriter, r *http.Request) { providerHandler.Update(w, r, r.PathValue("id")) })
	adminMux.HandleFunc("DELETE /admin/api/providers/{id}", func(w http.ResponseWriter, r *http.Request) { providerHandler.Delete(w, r, r.PathValue("id")) })
	adminMux.HandleFunc("POST /admin/api/providers/{id}/test", func(w http.ResponseWriter, r *http.Request) { providerHandler.Test(w, r, r.PathValue("id")) })

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
	adminMux.HandleFunc("GET /admin/api/upstream-models", modelHandler.ListAll)

	adminMux.HandleFunc("GET /admin/api/routes", routeHandler.List)
	adminMux.HandleFunc("POST /admin/api/routes", routeHandler.Create)
	adminMux.HandleFunc("PATCH /admin/api/routes/{id}", func(w http.ResponseWriter, r *http.Request) { routeHandler.Update(w, r, r.PathValue("id")) })
	adminMux.HandleFunc("DELETE /admin/api/routes/{id}", func(w http.ResponseWriter, r *http.Request) { routeHandler.Delete(w, r, r.PathValue("id")) })
	adminMux.HandleFunc("GET /admin/api/routes/{id}/targets", func(w http.ResponseWriter, r *http.Request) { routeTargetHandler.List(w, r, r.PathValue("id")) })
	adminMux.HandleFunc("PUT /admin/api/routes/{id}/targets", func(w http.ResponseWriter, r *http.Request) { routeTargetHandler.Replace(w, r, r.PathValue("id")) })

	adminMux.HandleFunc("GET /admin/api/keys", keyHandler.List)
	adminMux.HandleFunc("POST /admin/api/keys", keyHandler.Create)
	adminMux.HandleFunc("PATCH /admin/api/keys/{id}", func(w http.ResponseWriter, r *http.Request) { keyHandler.Update(w, r, r.PathValue("id")) })
	adminMux.HandleFunc("DELETE /admin/api/keys/{id}", func(w http.ResponseWriter, r *http.Request) { keyHandler.Delete(w, r, r.PathValue("id")) })

	adminMux.HandleFunc("GET /admin/api/stats", statsHandler.Get)
	adminMux.HandleFunc("POST /admin/api/reload", reloadHandler.Reload)
	adminMux.HandleFunc("GET /admin/api/usage", usageHandler.Query)
	adminMux.HandleFunc("GET /admin/api/usage/by-key", usageHandler.GroupByKey)
	adminMux.HandleFunc("GET /admin/api/usage/by-model", usageHandler.GroupByModel)
	adminMux.HandleFunc("GET /admin/api/usage/by-provider", usageHandler.GroupByProvider)
	adminMux.HandleFunc("GET /admin/api/usage/by-day", usageHandler.GroupByDay)
	adminMux.HandleFunc("GET /admin/api/settings", settingsHandler.Get)
	adminMux.HandleFunc("PUT /admin/api/settings", settingsHandler.Update)
	adminMux.HandleFunc("GET /admin/api/password/check", passwordHandler.Check)
	adminMux.HandleFunc("POST /admin/api/password/set", passwordHandler.Set)
	adminMux.HandleFunc("GET /admin/api/auth/verify", passwordHandler.Verify)
	adminMux.HandleFunc("GET /admin/api/usage/history", usageHandler.History)

	adminWrapped := server.AdminAuth(adminMux, authStore)

	// 管理写操作成功后自动重建运行时（池 + 快照）：配置生效不再依赖前端自觉调
	// POST /admin/api/reload，任何带凭据的调用方（curl/脚本）写完立即生效 ——
	// 包括禁用下游 Key 这类安全敏感操作（auth 读快照，不重建就照常放行）。
	// AutoReload 在 AdminAuth 外侧：401/429 的失败响应不会触发重建。
	// 前端 mutate() 里的 reload 调用保留为兜底（服务端重建失败时再给一次机会）。
	adminAuto := server.AutoReload(adminWrapped, reloader.Reload, logger)

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
		// 只服务已知的两类路径，其余一律 404。虽然 http.FileServer + embed FS
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
			w.Write(data)
		case strings.HasPrefix(p, "/assets/"):
			// 静态资源，原样交给 FileServer。
			r.URL.Path = p
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
		Addr:         cfg.Listen,
		Handler:      handler,
		ReadTimeout:  30 * time.Second,
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

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	logger.Info("shutting down...")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		logger.Error("shutdown error", "error", err)
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
}

// Reload 串行执行一次「池 + 快照」重建；任何一步失败都不改变运行状态。
func (rr *runtimeReloader) Reload(ctx context.Context) error {
	rr.mu.Lock()
	defer rr.mu.Unlock()

	providers, err := rr.pool.PrepareFromStore(ctx, rr.db, rr.masterKey, rr.cfg)
	if err != nil {
		return fmt.Errorf("prepare upstream pool: %w", err)
	}
	snap, err := snapshot.RebuildFromDB(ctx, rr.db)
	if err != nil {
		return fmt.Errorf("rebuild snapshot: %w", err)
	}
	rr.pool.Install(providers)
	snapshot.Swap(snap)
	return nil
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

// writeAuthError 把鉴权失败映射成 OpenAI 兼容的错误响应。
// /v1 下的每个端点都走这一处，免得口径漂移（例如某个端点把「密钥被禁用」
// 也当成 401 而非 403）。
func writeAuthError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, auth.ErrNoKey):
		outwire.WriteOpenAIError(w, http.StatusUnauthorized, "invalid_api_key", "missing API key")
	case errors.Is(err, auth.ErrKeyDisabled):
		outwire.WriteOpenAIError(w, http.StatusForbidden, "invalid_api_key", "API key disabled")
	case errors.Is(err, auth.ErrInvalidKey):
		outwire.WriteOpenAIError(w, http.StatusUnauthorized, "invalid_api_key", "invalid API key")
	default:
		outwire.WriteOpenAIError(w, http.StatusUnauthorized, "invalid_api_key", "authentication failed")
	}
}

func handleChatCompletions(pool *upstream.Pool, cfg *config.Config, usage *usageRecorder) http.HandlerFunc {
	db, logger := usage.db, usage.logger
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()

		authCtx, err := auth.Authenticate(r)
		if err != nil {
			writeAuthError(w, err)
			return
		}

		// 终身 token 配额预检（DESIGN §11.2）：quota>0 且 used>=quota 直接 429。
		// 读的是库里的权威 used_tokens（触发器实时累加）；查询抖动时 fail-open，
		// 不因一次读失败拒绝正常流量。并发下容忍至多一个在途请求超发（post-deduct 语义）。
		if quota, used, ok, qerr := db.GetKeyQuota(r.Context(), authCtx.KeyID); qerr != nil {
			logger.Error("quota lookup failed", "error", qerr, "key_id", authCtx.KeyID)
		} else if ok && quota > 0 && used >= quota {
			logger.Warn("quota exceeded", "key_id", authCtx.KeyID, "used", used, "quota", quota,
				"request_id", server.RequestIDFromContext(r.Context()))
			outwire.WriteOpenAIError(w, http.StatusTooManyRequests, "insufficient_quota",
				"this API key has exhausted its token quota")
			return
		}

		req, err := inwire.DecodeOpenAIChatRequest(r, int64(cfg.Defaults.MaxRequestBodyBytes))
		if err != nil {
			// 超限时中间件的 MaxBytesReader 会返回 *http.MaxBytesError。
			// 旧实现把它包成 "read body: ..." 一并当 400 回，客户端看不出
			// 「是body太大」还是「body格式错」——两者要采取的行动完全不同。
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				outwire.WriteOpenAIError(w, http.StatusRequestEntityTooLarge, "invalid_request_error",
					fmt.Sprintf("request body exceeds %d bytes", tooLarge.Limit))
				return
			}
			outwire.WriteOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
			return
		}

		snap := snapshot.Get()
		res, err := snap.Routes.Resolve(req.Model)
		if err != nil {
			outwire.WriteOpenAIError(w, http.StatusNotFound, "model_not_found", fmt.Sprintf("model %q not found", req.Model))
			return
		}
		route := res.Route

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
		if len(active) > limit {
			active = active[:limit]
		}

		threshold := failoverFailureThreshold(snap, cfg)
		sendUsage := wantsStreamUsage(req)
		keyID := authCtx.KeyID

		var out attemptOutcome
		for i, cand := range active {
			isLast := i == len(active)-1

			// 客户端已断开就收手：半路跑掉的人不该消耗整条链的下游配额，
			// 也不该把一次在途取消误记成目标的失败。此刻尚未写出任何字节，
			// 直接返回、不记 error usage（断流是客户端行为，不是上游故障）。
			if cerr := r.Context().Err(); cerr != nil {
				logger.Info("client disconnected, aborting failover",
					"model", req.Model, "attempt", i+1, "error", cerr,
					"request_id", server.RequestIDFromContext(r.Context()))
				return
			}

			rosettaReq := req.ToRosetta()
			rosettaReq.Model = cand.UpstreamModel.ModelID
			req.ApplyProtocolPrivateExtra(rosettaReq, cand.Provider.Protocol)

			// 一次 attempt 只取该 provider 的一把凭据：某把 key 失败时本请求不就地换
			// 同 provider 的下一把，而是让位给链上下一个目标。跨请求的 key 轮换交给
			// 冷却 —— 坏 key 被踢出 healthy 后，下个请求自会选到好 key。这是有意取舍，
			// 免得「一个请求把某 provider 所有 key 各打一遍」放大延迟与配额消耗。
			client, credID, cerr := pool.GetAnyClient(cand.Provider.Slug)
			if cerr != nil {
				// 该目标当前无健康凭据：累计目标失败，换链上下一个。
				pool.RecordTargetFailure(cand.TargetID, threshold)
				out = attemptOutcome{eligible: true, statusCode: http.StatusBadGateway,
					code: "upstream_error", message: "no available upstream provider"}
				if !isLast {
					logger.Warn("failover: no healthy credential, switching target",
						"model", req.Model, "provider", cand.Provider.Slug, "request_id", server.RequestIDFromContext(r.Context()))
					continue
				}
				break
			}

			if req.Stream {
				out = attemptStream(w, r, client, rosettaReq, req.Model, cand, keyID, sendUsage, cfg, snap, usage, start)
			} else {
				out = attemptNonStream(w, r, client, rosettaReq, req.Model, cand, keyID, cfg, snap, usage, start)
			}

			if out.committed {
				pool.RecordCredentialSuccess(credID)
				pool.RecordTargetSuccess(cand.TargetID)
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
				if out.credCooldown > 0 {
					pool.MarkCredentialCooldown(credID, out.credCooldown)
				}
			}

			if !out.eligible || isLast {
				break
			}
			logger.Warn("failover: switching to next target",
				"model", req.Model, "from_provider", cand.Provider.Slug,
				"error_code", out.code, "attempt", i+1,
				"request_id", server.RequestIDFromContext(r.Context()))
		}

		// 走到这里 = 最后一次尝试未提交（要么不可转移错误、要么链已耗尽）。
		// 写出最终错误并记一条 usage —— 若 active 为空（理论不该发生）兜底成 502。
		// 客户端已经断开时它根本收不到这个响应（写了只会撞 broken pipe），
		// 且这次失败与上游无关，按 canceled 记账，与流式路径同一口径。
		clientGone := r.Context().Err() != nil

		statusCode, code, message := out.statusCode, out.code, out.message
		if statusCode == 0 {
			statusCode, code, message = http.StatusBadGateway, "upstream_error", "no available upstream provider"
		}
		if !clientGone {
			outwire.WriteOpenAIError(w, statusCode, code, message)
		}
		provID, upstreamModel := "", ""
		if len(active) > 0 {
			last := active[len(active)-1]
			provID, upstreamModel = last.Provider.ID, last.UpstreamModel.ModelID
		}
		status := "error"
		if clientGone {
			status = "canceled"
			logger.Info("client disconnected before any response was written",
				"model", req.Model, "request_id", server.RequestIDFromContext(r.Context()))
		}
		errorCode := code
		if clientGone {
			errorCode = ""
		}
		usage.record(&store.UsageRecord{
			ID:              generateID(),
			AccessKeyID:     keyID,
			PublicModel:     req.Model,
			ProviderID:      provID,
			UpstreamModel:   upstreamModel,
			IngressProtocol: "openai-chat",
			Stream:          req.Stream,
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
	committed    bool
	eligible     bool
	credCooldown time.Duration
	statusCode   int
	code         string
	message      string
}

// outcomeFromErr 把一次上游 err 归类成「未提交」的结果。
func outcomeFromErr(err error) attemptOutcome {
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

// nonStreamTimeout：设置页的全局默认优先，其次 config 的上游超时。
func nonStreamTimeout(snap *snapshot.Snapshot, cfg *config.Config) time.Duration {
	if snap != nil && snap.Runtime.UpstreamTimeoutMs > 0 {
		return time.Duration(snap.Runtime.UpstreamTimeoutMs) * time.Millisecond
	}
	return cfg.UpstreamTimeout()
}

// firstTokenTimeout：设置页的全局默认优先，其次 config 的首字超时。
func firstTokenTimeout(snap *snapshot.Snapshot, cfg *config.Config) time.Duration {
	if snap != nil && snap.Runtime.StreamFirstTokenTimeoutMs > 0 {
		return time.Duration(snap.Runtime.StreamFirstTokenTimeoutMs) * time.Millisecond
	}
	return cfg.StreamFirstTokenTimeout()
}

// streamIdleTimeout：设置页的全局默认优先，其次 config 的流式空闲超时。
func streamIdleTimeout(snap *snapshot.Snapshot, cfg *config.Config) time.Duration {
	if snap != nil && snap.Runtime.StreamIdleTimeoutMs > 0 {
		return time.Duration(snap.Runtime.StreamIdleTimeoutMs) * time.Millisecond
	}
	return cfg.StreamIdleTimeout()
}

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
func attemptStream(w http.ResponseWriter, r *http.Request, client *rosetta.Client, req *rosetta.ChatRequest, publicModel string, cand routing.Candidate, keyID string, sendUsage bool, cfg *config.Config, snap *snapshot.Snapshot, usage *usageRecorder, start time.Time) attemptOutcome {
	ctx := r.Context()
	logger := usage.logger

	stream, err := client.ChatStream(ctx, req)
	if err != nil {
		return outcomeFromErr(err)
	}
	defer stream.Close()

	// 首字（TTFT）看门狗：只掐「一个事件都没等到」的慢上游，触发即关流，
	// 尚未写头 → 未提交 → 外层可转移。与下面的 idle 看门狗是两回事。
	ttftTimeout := firstTokenTimeout(snap, cfg)
	var ttftTimedOut atomic.Bool
	ttftTimer := time.AfterFunc(ttftTimeout, func() {
		ttftTimedOut.Store(true)
		_ = stream.Close()
	})
	gotFirst := stream.Next()
	ttftTimer.Stop()

	if !gotFirst {
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

	flusher, _ := w.(http.Flusher)
	sse := outwire.NewSSEWriter(w, flusher, "chatcmpl-"+generateID(), publicModel, time.Now().Unix())

	idleTimeout := streamIdleTimeout(snap, cfg)
	var idleTimedOut atomic.Bool
	idleTimer := time.AfterFunc(idleTimeout, func() {
		idleTimedOut.Store(true)
		logger.Warn("stream idle timeout",
			"model", publicModel, "key_id", keyID,
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
	// 心跳 goroutine 与主循环会并发写同一个 ResponseWriter，所以统一经由 sse
	// （outwire.SSEWriter 内部有锁），不再自己 fmt.Fprintf(w, ...)。
	go func() {
		for {
			select {
			case <-heartbeatTicker.C:
				sse.WriteComment("keepalive")
			case <-ctx.Done():
				return
			}
		}
	}()

	var lastUsage rosetta.Usage
	var stopReason rosetta.StopReason
	status := "ok"
	errorCode := ""
	httpStatus := 200
	sawTerminal := false
	wroteContent := false

	// handleEvent 消费一个上游事件（首个 + 后续走同一套逻辑）。
	handleEvent := func(ev *rosetta.Event) {
		idleTimer.Reset(idleTimeout)
		heartbeatTicker.Reset(heartbeatInterval)
		switch ev.Type {
		case rosetta.EventMessageStart:
			sse.SetResponseID(ev.ID)
		case rosetta.EventTextDelta:
			if ev.Text != "" {
				wroteContent = true
			}
			sse.WriteTextDelta(ev.Text)
		case rosetta.EventThinkingDelta:
			// 思考增量必须透传：只吐 reasoning_content 的流若被丢掉，下游会收到
			// 一条「零内容 + finish_reason:stop + [DONE]」的假正常流。空 Text 是
			// Anthropic thinking signature 载体，OpenAI 下游无对应字段，跳过不算丢内容。
			if ev.Text != "" {
				wroteContent = true
				sse.WriteThinkingDelta(ev.Text)
			}
		case rosetta.EventToolCall:
			wroteContent = true
			sse.WriteToolCallDelta(ev.ToolIndex, ev.ToolID, ev.ToolName, ev.ArgumentsDelta)
		case rosetta.EventMessageEnd:
			sawTerminal = true
			if ev.Usage != nil {
				lastUsage = *ev.Usage
			}
			stopReason = ev.StopReason
		}
	}

	handleEvent(stream.Event())
	for stream.Next() {
		handleEvent(stream.Event())
	}

	if err := stream.Err(); err != nil {
		switch {
		case errors.Is(err, rosetta.ErrStreamTruncated):
			status = "truncated"
			errorCode = "stream_truncated"
			logger.Warn("stream truncated", "model", publicModel, "key_id", keyID, "error", err)
		case errors.Is(err, rosetta.ErrStreamOverflow):
			status = "overflow"
			errorCode = "stream_overflow"
			logger.Error("stream overflow", "model", publicModel, "key_id", keyID, "error", err)
		case isClientGone(ctx, err):
			// 客户端主动断开不是上游故障。此前一律记 error，后果是：后台错误率虚高、
			// 成功率虚低，且真故障被 ERROR 噪音淹没。单独一个取值才能把两者分开。
			status = "canceled"
			logger.Info("client disconnected mid-stream", "model", publicModel, "key_id", keyID, "error", err)
		default:
			status = "error"
			errorCode = "upstream_error"
			logger.Error("stream error", "error", err, "model", publicModel, "key_id", keyID)
		}
	} else if idleTimedOut.Load() && !sawTerminal {
		status = "truncated"
		errorCode = "stream_idle_timeout"
		logger.Warn("stream cut by idle watchdog without terminal event",
			"model", publicModel, "key_id", keyID,
			"idle_timeout_ms", idleTimeout.Milliseconds(),
			"content_written", wroteContent)
	} else if !wroteContent {
		logger.Warn("stream finished with no content",
			"model", publicModel, "key_id", keyID,
			"stop_reason", string(stopReason),
			"input_tokens", lastUsage.InputTokens,
			"output_tokens", lastUsage.OutputTokens,
			"reasoning_tokens", lastUsage.ReasoningTokens)
	}

	if status == "ok" {
		sse.WriteFinish(outwire.OpenAIFinishReason(stopReason))
	}
	if sendUsage && (lastUsage.InputTokens > 0 || lastUsage.OutputTokens > 0) {
		sse.WriteUsage(lastUsage)
	}
	if status == "ok" {
		sse.WriteDone()
	}

	latency := time.Since(start).Milliseconds()

	usage.record(&store.UsageRecord{
		ID:              generateID(),
		AccessKeyID:     keyID,
		PublicModel:     publicModel,
		ProviderID:      cand.Provider.ID,
		UpstreamModel:   cand.UpstreamModel.ModelID,
		IngressProtocol: "openai-chat",
		Stream:          true,
		InputTokens:     lastUsage.InputTokens,
		OutputTokens:    lastUsage.OutputTokens,
		TotalTokens:     lastUsage.TotalTokens,
		ReasoningTokens: lastUsage.ReasoningTokens,
		CachedTokens:    lastUsage.CachedInputTokens,
		UsageState:      "reported",
		Status:          status,
		HTTPStatus:      httpStatus,
		ErrorCode:       errorCode,
		LatencyMs:       latency,
		TTFBMs:          ttfbMs,
	})
	return attemptOutcome{committed: true}
}

func attemptNonStream(w http.ResponseWriter, r *http.Request, client *rosetta.Client, req *rosetta.ChatRequest, publicModel string, cand routing.Candidate, keyID string, cfg *config.Config, snap *snapshot.Snapshot, usage *usageRecorder, start time.Time) attemptOutcome {
	// 绑定 r.Context() 而非 context.Background()：客户端断开时上游调用应随之取消，
	// 否则断连请求会一直占用上游连接与配额直到超时（默认 120s）。
	ctx, cancel := context.WithTimeout(r.Context(), nonStreamTimeout(snap, cfg))
	defer cancel()

	resp, err := client.Chat(ctx, req)
	if err != nil {
		return outcomeFromErr(err)
	}

	// 非流式响应一次性返回，拿不到"首字"这一独立时刻，用响应到达时刻近似（≈总耗时）。
	latency := time.Since(start).Milliseconds()
	ttfbMs := latency

	outwire.WriteNonStreamResponse(w, resp, publicModel)

	usage.record(&store.UsageRecord{
		ID:              generateID(),
		AccessKeyID:     keyID,
		PublicModel:     publicModel,
		ProviderID:      cand.Provider.ID,
		UpstreamModel:   cand.UpstreamModel.ModelID,
		IngressProtocol: "openai-chat",
		Stream:          false,
		InputTokens:     resp.Usage.InputTokens,
		OutputTokens:    resp.Usage.OutputTokens,
		TotalTokens:     resp.Usage.TotalTokens,
		ReasoningTokens: resp.Usage.ReasoningTokens,
		CachedTokens:    resp.Usage.CachedInputTokens,
		UsageState:      "reported",
		Status:          "ok",
		HTTPStatus:      200,
		LatencyMs:       latency,
		TTFBMs:          ttfbMs,
	})
	return attemptOutcome{committed: true}
}


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
}

const (
	// usageQueueSize 是有界队列容量。超过该值即触发同步写背压。
	usageQueueSize = 1024
	// usageWorkers 是并发落库的 worker 数。SQLite 单写锁下并发写没有收益，
	// 1 个 worker 足够，队列本身负责吸收突发。
	usageWorkers = 1
)

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
func (u *usageRecorder) worker() {
	defer u.wg.Done()
	for rec := range u.queue {
		if err := u.db.CreateUsageRecord(context.Background(), rec); err != nil {
			u.logger.Error("failed to record usage", "error", err, "key_id", rec.AccessKeyID)
		}
	}
}

// record 排队一条异步落库，立即返回。
//
// 队列满时退化为同步写：宁可让当前请求多等一次 SQLite 写，也不丢用量记录。
// 同步写失败同样记日志，与异步路径口径一致。
func (u *usageRecorder) record(rec *store.UsageRecord) {
	select {
	case u.queue <- rec:
	default:
		// 队列已满 —— 背压：同步写。
		if err := u.db.CreateUsageRecord(context.Background(), rec); err != nil {
			u.logger.Error("failed to record usage (sync fallback)", "error", err, "key_id", rec.AccessKeyID)
		}
	}
}

// wait 关闭队列、等 worker 把在途写入全部完成，或 ctx 到期（到期即放弃，
// 不让一个卡住的 SQLite 写把进程关停拖成无限期）。
func (u *usageRecorder) wait(ctx context.Context) {
	close(u.queue)
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

func handleListModels() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// 与 /v1/chat/completions 同一鉴权口径（官方 OpenAI 的 /v1/models 同样要求
		// Authorization）。这里不查配额 —— 列个目录不消耗 token —— 但必须校验密钥：
		// 否则任何人都能枚举出全部公开模型名，等于白送一份路由与供应商结构图。
		if _, err := auth.Authenticate(r); err != nil {
			writeAuthError(w, err)
			return
		}

		type openaiModelEntry struct {
			ID      string `json:"id"`
			Object  string `json:"object"`
			Created int64  `json:"created"`
			OwnedBy string `json:"owned_by"`
		}

		routes := snapshot.Get().Routes.ListRoutes()
		data := make([]openaiModelEntry, 0, len(routes))
		for _, route := range routes {
			if route.Enabled {
				data = append(data, openaiModelEntry{
					ID:      route.PublicName,
					Object:  "model",
					Created: 0,
					OwnedBy: "gateway",
				})
			}
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"object": "list",
			"data":   data,
		})
	}
}

func generateID() string {
	b := make([]byte, 16)
	crand.Read(b)
	return hex.EncodeToString(b)
}
