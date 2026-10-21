package main

import (
	"log/slog"
	"net/http"

	"github.com/cn-maul/rosetta-gateway/internal/api"
	"github.com/cn-maul/rosetta-gateway/internal/config"
	"github.com/cn-maul/rosetta-gateway/internal/ratelimit"
	"github.com/cn-maul/rosetta-gateway/internal/runtime"
	"github.com/cn-maul/rosetta-gateway/internal/store"
	"github.com/cn-maul/rosetta-gateway/internal/upstream"
	"github.com/cn-maul/rosetta-gateway/internal/userauth"
)

// routeDeps is the package-local compatibility shape used by the existing
// end-to-end tests. The production assembler is owned by internal/api.
type routeDeps struct {
	db          *store.Store
	cfg         *config.Config
	logger      *slog.Logger
	masterKey   []byte
	pool        *upstream.Pool
	reloader    *runtime.Reloader
	sessionMgr  *userauth.Manager
	rateLimiter *ratelimit.Limiter
	usage       *usageRecorder
}

type gatewayApp = api.App
type registeredRoute = api.Route

func buildHandler(d routeDeps) http.Handler {
	return api.BuildHandler(toAPIDeps(d))
}

func buildApp(d routeDeps) *gatewayApp {
	return api.BuildApp(toAPIDeps(d))
}

func toAPIDeps(d routeDeps) api.Deps {
	return api.Deps{
		DB:          d.db,
		Config:      d.cfg,
		Logger:      d.logger,
		MasterKey:   d.masterKey,
		Pool:        d.pool,
		Reloader:    d.reloader,
		SessionMgr:  d.sessionMgr,
		RateLimiter: d.rateLimiter,
		Usage:       d.usage,
	}
}
