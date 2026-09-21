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

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))

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
	if err := pool.BuildFromStore(context.Background(), db, masterKey, cfg); err != nil {
		logger.Warn("failed to build pool from DB, falling back to config", "error", err)
		if err := pool.BuildFromConfig(cfg); err != nil {
			logger.Error("failed to build upstream pool from config", "error", err)
		}
	}

	snap, err := snapshot.RebuildFromDB(context.Background(), db, pool)
	if err != nil {
		logger.Error("failed to rebuild snapshot from DB", "error", err)
		snap = buildSnapshotFromConfig(cfg)
	}
	snapshot.Init(snap)

	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", handleChatCompletions(pool, cfg, db, logger))
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

	authStore, err := adminauth.Open(adminauth.ResolvePath(homeDir), adminToken)
	if err != nil {
		logger.Error("failed to load admin credentials", "error", err)
		os.Exit(1)
	}
	if authStore.HasUserPassword() {
		logger.Info("admin password loaded", "path", authStore.Path())
	} else if adminToken != "" {
		logger.Info("using admin_token from config; set a password in the admin UI to override it")
	} else {
		logger.Warn("no admin credential configured; the admin UI will ask you to set a password on first visit")
	}

	providerHandler := admin.NewProviderHandler(db, masterKey, cfg)
	credentialHandler := admin.NewCredentialHandler(db, masterKey)
	modelHandler := admin.NewModelHandler(db, masterKey, cfg)
	routeHandler := admin.NewRouteHandler(db)
	keyHandler := admin.NewKeyHandler(db)
	statsHandler := admin.NewStatsHandler(db)
	settingsHandler := admin.NewSettingsHandler(db)
	reloadHandler := admin.NewReloadHandler(db, masterKey, pool, cfg)
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

	adminMux.HandleFunc("GET /admin/api/routes", routeHandler.List)
	adminMux.HandleFunc("POST /admin/api/routes", routeHandler.Create)
	adminMux.HandleFunc("PATCH /admin/api/routes/{id}", func(w http.ResponseWriter, r *http.Request) { routeHandler.Update(w, r, r.PathValue("id")) })
	adminMux.HandleFunc("DELETE /admin/api/routes/{id}", func(w http.ResponseWriter, r *http.Request) { routeHandler.Delete(w, r, r.PathValue("id")) })

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

	// SPA 产物在 embed FS 的 dist/ 子目录下；剥掉 /admin 前缀后交给 FileServer。
	// hash 路由下路径只有 /admin/（入口）与 /admin/assets/*（静态资源）两类。
	webuiFS, _ := fs.Sub(webui.StaticFS, "dist")
	fileServer := http.FileServer(http.FS(webuiFS))
	webHandler := func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/admin")
		if p == "" {
			p = "/index.html"
		}
		r.URL.Path = p
		fileServer.ServeHTTP(w, r)
	}

	// 注意：Go 1.22+ 的 ServeMux 会拒绝 "GET /admin/"（路径更泛、方法更窄）
	// 与 "/admin/api/"（路径更窄、方法不限）并存——两者互不更具特异性，注册期直接 panic。
	// 因此两侧都显式声明方法：API 按方法逐个注册，静态资源只挂 GET 子树。
	// 于是 "GET /admin/api/" 在路径上严格更具体、方法相同，冲突消除。
	mux.Handle("GET /admin/api/", adminWrapped)
	mux.Handle("POST /admin/api/", adminWrapped)
	mux.Handle("PATCH /admin/api/", adminWrapped)
	mux.Handle("DELETE /admin/api/", adminWrapped)
	mux.Handle("PUT /admin/api/", adminWrapped)
	mux.HandleFunc("GET /admin/", webHandler)

	handler := server.Recovery(mux, logger)
	handler = server.Middleware(handler, logger)
	handler = server.CORS(handler)
	handler = server.RequestSizeLimit(int64(cfg.Defaults.MaxRequestBodyBytes))(handler)

	srv := &http.Server{
		Addr:         cfg.Listen,
		Handler:      handler,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 5 * time.Minute,
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
	logger.Info("server stopped")
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
		Routes:    routing.NewRouteIndex(),
		Providers: make(map[string]*snapshot.ProviderSnapshot),
		Keys:      make(map[string]*snapshot.KeySnapshot),
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

func handleChatCompletions(pool *upstream.Pool, cfg *config.Config, db *store.Store, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()

		authCtx, err := auth.Authenticate(r)
		if err != nil {
			switch err {
			case auth.ErrNoKey:
				outwire.WriteOpenAIError(w, http.StatusUnauthorized, "invalid_api_key", "missing API key")
			case auth.ErrInvalidKey:
				outwire.WriteOpenAIError(w, http.StatusUnauthorized, "invalid_api_key", "invalid API key")
			case auth.ErrKeyDisabled:
				outwire.WriteOpenAIError(w, http.StatusForbidden, "invalid_api_key", "API key disabled")
			default:
				outwire.WriteOpenAIError(w, http.StatusUnauthorized, "invalid_api_key", "authentication failed")
			}
			return
		}

		req, err := inwire.DecodeOpenAIChatRequest(r)
		if err != nil {
			outwire.WriteOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
			return
		}

		res, err := snapshot.Get().Routes.Resolve(req.Model)
		if err != nil {
			outwire.WriteOpenAIError(w, http.StatusNotFound, "model_not_found", fmt.Sprintf("model %q not found", req.Model))
			return
		}

		client, _, err := pool.GetAnyClient(res.Provider.Slug)
		if err != nil {
			outwire.WriteOpenAIError(w, http.StatusBadGateway, "upstream_error", "no available upstream provider")
			return
		}

		rosettaReq := req.ToRosetta()
		rosettaReq.Model = res.UpstreamModel.ModelID
		req.ApplyProtocolPrivateExtra(rosettaReq, res.Provider.Protocol)

		if req.Stream {
			handleStreamRequest(w, r, client, rosettaReq, req.Model, res.Provider.ID, res.UpstreamModel.ModelID, authCtx.KeyID, wantsStreamUsage(req), cfg, db, logger, start)
		} else {
			handleNonStreamRequest(w, client, rosettaReq, req.Model, res.Provider.ID, res.UpstreamModel.ModelID, authCtx.KeyID, cfg, db, logger, start)
		}
	}
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

func handleStreamRequest(w http.ResponseWriter, r *http.Request, client *rosetta.Client, req *rosetta.ChatRequest, publicModel, providerID, upstreamModelID, keyID string, sendUsage bool, cfg *config.Config, db *store.Store, logger *slog.Logger, start time.Time) {
	ctx := r.Context()
	var ttfbMs int64
	firstByte := true

	stream, err := client.ChatStream(ctx, req)
	if err != nil {
		statusCode, code, message := outwire.MapUpstreamError(err)
		outwire.WriteOpenAIError(w, statusCode, code, message)
		return
	}
	defer stream.Close()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	flusher, _ := w.(http.Flusher)
	sse := outwire.NewSSEWriter(w, flusher, "chatcmpl-"+generateID(), publicModel, time.Now().Unix())

	// 看门狗：idleTimeout 内一个上游事件都没有就关流止损。
	//
	// 为什么必须自己记一笔：Stream.Close() 只置 done 并释放连接，
	// **不会**写 stream.Err()（见 rosetta stream.go 的 streamCore.Close）。
	// 于是超时在流上不留任何痕迹 —— Err() 返回 nil，status 保持 "ok"，
	// 下游照常收到 finish_reason:"stop" + [DONE]，把一个「上游卡死」伪装成
	// 正常收尾，本网关自己的 usage_records 也会记成 ok，事后无从追查。
	// DESIGN.md §8.1 明确要求这种情况记 status=truncated。
	var idleTimedOut atomic.Bool
	idleTimeout := cfg.StreamIdleTimeout()
	idleTimer := time.AfterFunc(idleTimeout, func() {
		idleTimedOut.Store(true)
		logger.Warn("stream idle timeout",
			"model", publicModel, "key_id", keyID,
			"idle_timeout_ms", idleTimeout.Milliseconds())
		_ = stream.Close()
	})
	defer idleTimer.Stop()

	heartbeatInterval := idleTimeout / 2
	heartbeatTicker := time.NewTicker(heartbeatInterval)
	defer heartbeatTicker.Stop()

	// sendUsage was resolved from the typed request by the caller; the
	// gateway never populates rosetta.ChatRequest.Extra, so reading the flag
	// back out of it would always yield false.

	go func() {
		for {
			select {
			case <-heartbeatTicker.C:
				fmt.Fprintf(w, ": keepalive\n\n")
				if flusher != nil {
					flusher.Flush()
				}
			case <-ctx.Done():
				return
			}
		}
	}()

	var lastUsage rosetta.Usage
	var stopReason rosetta.StopReason
	status := "ok"
	httpStatus := 200
	// sawTerminal：上游给过 EventMessageEnd。看门狗开火只对「从未拿到终止事件」
	// 的流降级 —— 已收到 message_end 的流是完整的，把它报成截断属于反向误判。
	// 实测 rosetta v0.5.1 在 [DONE]/EOF 之后就短路了 next()，适配器不会在吐出
	// message_end 后继续阻塞，所以这条判定当前**打不到**；留着守的是「适配器
	// 将来在终止事件之后仍等待更多数据」这种情形（对照 zzfake 的 no_done_hold：
	// 那条路径没有 message_end，仍然如实记 truncated）。
	sawTerminal := false
	// wroteContent：往下游写过至少一个内容增量（文本/思考/工具调用）。
	// 全都没有时是可疑的空流，留痕但不改协议行为（见循环后的注释）。
	wroteContent := false

	for stream.Next() {
		if firstByte {
			ttfbMs = time.Since(start).Milliseconds()
			firstByte = false
		}
		idleTimer.Reset(idleTimeout)

		ev := stream.Event()
		switch ev.Type {
		case rosetta.EventMessageStart:
			sse.SetResponseID(ev.ID)
		case rosetta.EventTextDelta:
			if ev.Text != "" {
				wroteContent = true
			}
			sse.WriteTextDelta(ev.Text)
		case rosetta.EventThinkingDelta:
			// 思考增量必须透传：只吐 reasoning_content 的流（思考型模型在
			// max_tokens 耗尽于思考期时就是这种形态）若被丢掉，下游收到的是
			// 一条「零内容 + finish_reason:stop + [DONE]」的正常流，
			// 只能报出「流式响应中没有内容」这种无从排查的错误。
			// Text 为空的事件是 Anthropic thinking signature 的载体，
			// OpenAI 下游没有对应字段，跳过不算丢内容。
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

	if err := stream.Err(); err != nil {
		switch {
		case errors.Is(err, rosetta.ErrStreamTruncated):
			// The upstream ended the stream before its terminal event. Partial
			// content has already been forwarded. The SDK *wraps* this
			// sentinel (fmt.Errorf with %w), so an == comparison never matches
			// and every truncation would be misfiled as a generic error.
			status = "truncated"
			logger.Warn("stream truncated", "model", publicModel, "key_id", keyID, "error", err)
		case errors.Is(err, rosetta.ErrStreamOverflow):
			// The SDK's accumulation guard tripped (64 MiB / 10k blocks) —
			// the upstream is runaway or hostile. Client-visible outcome is
			// the same as truncation: a partial answer and no finish event.
			status = "overflow"
			logger.Error("stream overflow", "model", publicModel, "key_id", keyID, "error", err)
		default:
			status = "error"
			logger.Error("stream error", "error", err, "model", publicModel, "key_id", keyID)
		}
	} else if idleTimedOut.Load() && !sawTerminal {
		// 看门狗掐断且上游从未给出终止事件 —— 与 ErrStreamTruncated 同类：
		// 已转发的内容不回滚，但绝不能让下游收到「正常收尾」（不写
		// finish_reason、不写 [DONE]，客户端据此判定断流）。
		status = "truncated"
		logger.Warn("stream cut by idle watchdog without terminal event",
			"model", publicModel, "key_id", keyID,
			"idle_timeout_ms", idleTimeout.Milliseconds(),
			"content_written", wroteContent)
	} else if !wroteContent {
		// 上游给了终止事件却一个内容增量都没吐。协议上保持原样下发
		// （上游可能因内容过滤合法地返回空回复，网关不该替它改语义），
		// 但必须留痕：下游通常只会报「没有内容」，日志是唯一能区分
		// 「上游确实回了空」与「网关把内容吃掉了」的地方。
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

	recordUsage(db, logger, &store.UsageRecord{
		ID:              generateID(),
		AccessKeyID:     keyID,
		PublicModel:     publicModel,
		ProviderID:      providerID,
		UpstreamModel:   upstreamModelID,
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
		LatencyMs:       latency,
		TTFBMs:          ttfbMs,
	})
}

func handleNonStreamRequest(w http.ResponseWriter, client *rosetta.Client, req *rosetta.ChatRequest, publicModel, providerID, upstreamModelID, keyID string, cfg *config.Config, db *store.Store, logger *slog.Logger, start time.Time) {
	ctx, cancel := context.WithTimeout(context.Background(), cfg.UpstreamTimeout())
	defer cancel()

	resp, err := client.Chat(ctx, req)
	if err != nil {
		statusCode, code, message := outwire.MapUpstreamError(err)
		outwire.WriteOpenAIError(w, statusCode, code, message)
		return
	}

	// 非流式响应一次性返回，拿不到"首字"这一独立时刻，用响应到达时刻近似（≈总耗时）。
	latency := time.Since(start).Milliseconds()
	ttfbMs := latency

	outwire.WriteNonStreamResponse(w, resp, publicModel)

	recordUsage(db, logger, &store.UsageRecord{
		ID:              generateID(),
		AccessKeyID:     keyID,
		PublicModel:     publicModel,
		ProviderID:      providerID,
		UpstreamModel:   upstreamModelID,
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
}

// recordUsage 异步落库一条使用记录，避免阻塞响应返回。
func recordUsage(db *store.Store, logger *slog.Logger, rec *store.UsageRecord) {
	go func() {
		if err := db.CreateUsageRecord(context.Background(), rec); err != nil {
			logger.Error("failed to record usage", "error", err, "key_id", rec.AccessKeyID)
		}
	}()
}

func handleListModels() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
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
