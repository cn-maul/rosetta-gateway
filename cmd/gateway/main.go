package main

import (
	"context"
	crand "crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/cn-maul/rosetta"
	"github.com/cn-maul/rosetta-gateway/internal/admin"
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

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))

	// 配置与数据都按可执行文件所在目录解析，与当前工作目录无关：
	// 无论从哪个目录双击或命令行启动，都读同一个 config.json、命中同一个数据库。
	exePath, err := os.Executable()
	if err != nil {
		logger.Error("failed to resolve executable path", "error", err)
		os.Exit(1)
	}
	exeDir := filepath.Dir(exePath)

	configPath := filepath.Join(exeDir, "config.json")
	cfg, generated, err := config.LoadOrGenerate(configPath)
	if err != nil {
		logger.Error("failed to load config", "error", err)
		os.Exit(1)
	}
	if generated {
		logger.Info("no config found, generated default", "path", configPath)
	}

	if !filepath.IsAbs(cfg.DBPath) {
		cfg.DBPath = filepath.Join(exeDir, cfg.DBPath)
	}

	logger.Info("config loaded", "listen", cfg.Listen, "db_path", cfg.DBPath, "log_level", cfg.LogLevel)

	masterKey, err := crypto.GetMasterKey(cfg.MasterKeyEnv)
	if err != nil {
		logger.Warn("master key not set, credential encryption disabled", "error", err)
		masterKey = nil
	}

	db, err := store.Open(cfg.DBPath, logger)
	if err != nil {
		logger.Error("failed to open database", "error", err)
		os.Exit(1)
	}
	defer db.Close()

	bootstrapDB(db, cfg, logger)

	pool := upstream.NewPool(logger)
	if err := pool.BuildFromStore(context.Background(), db, masterKey, cfg); err != nil {
		logger.Warn("failed to build pool from DB, falling back to config", "error", err)
		if err := pool.BuildFromConfig(cfg); err != nil {
			logger.Error("failed to build upstream pool from config", "error", err)
		}
	}

	snap, err := snapshot.RebuildFromDB(context.Background(), db, pool, masterKey, logger)
	if err != nil {
		logger.Error("failed to rebuild snapshot from DB", "error", err)
		snap = buildSnapshotFromConfig(cfg)
	}
	snapshot.Init(snap)

	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", handleChatCompletions(pool, cfg, db, logger))
	mux.HandleFunc("GET /v1/models", handleListModels())

	adminToken := cfg.AdminToken
	if adminToken == "" {
		adminToken = os.Getenv("ADMIN_TOKEN")
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
	adminMux.HandleFunc("GET /admin/api/settings", settingsHandler.Get)
	adminMux.HandleFunc("PUT /admin/api/settings", settingsHandler.Update)
	adminMux.HandleFunc("POST /admin/api/reload", reloadHandler.Reload)
	adminMux.HandleFunc("GET /admin/api/usage", usageHandler.Query)
	adminMux.HandleFunc("GET /admin/api/usage/by-key", usageHandler.GroupByKey)
	adminMux.HandleFunc("GET /admin/api/usage/by-model", usageHandler.GroupByModel)
	adminMux.HandleFunc("GET /admin/api/usage/by-provider", usageHandler.GroupByProvider)
	adminMux.HandleFunc("GET /admin/api/usage/by-day", usageHandler.GroupByDay)
	adminMux.HandleFunc("GET /admin/api/usage/history", usageHandler.History)

	adminWrapped := server.AdminAuth(adminMux, adminToken)

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

func bootstrapDB(db *store.Store, cfg *config.Config, logger *slog.Logger) {
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

	masterKey, _ := crypto.GetMasterKey(cfg.MasterKeyEnv)

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

		if req.Stream {
			handleStreamRequest(w, r, client, rosettaReq, req.Model, res.Provider.ID, res.UpstreamModel.ModelID, authCtx.KeyID, cfg, db, logger, start)
		} else {
			handleNonStreamRequest(w, client, rosettaReq, req.Model, res.Provider.ID, res.UpstreamModel.ModelID, authCtx.KeyID, cfg, db, logger, start)
		}
	}
}

func handleStreamRequest(w http.ResponseWriter, r *http.Request, client *rosetta.Client, req *rosetta.ChatRequest, publicModel, providerID, upstreamModelID, keyID string, cfg *config.Config, db *store.Store, logger *slog.Logger, start time.Time) {
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

	idleTimeout := cfg.StreamIdleTimeout()
	idleTimer := time.AfterFunc(idleTimeout, func() {
		logger.Warn("stream idle timeout", "model", publicModel, "key_id", keyID)
		stream.Close()
	})
	defer idleTimer.Stop()

	heartbeatInterval := idleTimeout / 2
	heartbeatTicker := time.NewTicker(heartbeatInterval)
	defer heartbeatTicker.Stop()

	var sendUsage bool
	if req.Extra != nil {
		if so, ok := req.Extra["stream_options"]; ok {
			if m, ok := so.(map[string]any); ok {
				if v, ok := m["include_usage"].(bool); ok {
					sendUsage = v
				}
			}
		}
	}

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
			sse.WriteTextDelta(ev.Text)
		case rosetta.EventThinkingDelta:
		case rosetta.EventToolCall:
			sse.WriteToolCallDelta(ev.ToolIndex, ev.ToolID, ev.ToolName, ev.ArgumentsDelta)
		case rosetta.EventMessageEnd:
			if ev.Usage != nil {
				lastUsage = *ev.Usage
			}
			stopReason = ev.StopReason
		}
	}

	if err := stream.Err(); err != nil {
		if err == rosetta.ErrStreamTruncated {
			status = "truncated"
			logger.Warn("stream truncated", "model", publicModel, "key_id", keyID)
		} else {
			status = "error"
			logger.Error("stream error", "error", err, "model", publicModel, "key_id", keyID)
		}
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
