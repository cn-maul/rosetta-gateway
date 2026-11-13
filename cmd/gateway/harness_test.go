package main

// Package-local test scaffolding. Nothing here is compiled into the gateway
// binary: production startup goes straight through gatewayapp.Run.

import (
	"log/slog"
	"testing"

	"github.com/cn-maul/rosetta-gateway/internal/api"
	"github.com/cn-maul/rosetta-gateway/internal/config"
	"github.com/cn-maul/rosetta-gateway/internal/gatewayapp"
	"github.com/cn-maul/rosetta-gateway/internal/ratelimit"
	"github.com/cn-maul/rosetta-gateway/internal/runtime"
	"github.com/cn-maul/rosetta-gateway/internal/store"
	"github.com/cn-maul/rosetta-gateway/internal/upstream"
	"github.com/cn-maul/rosetta-gateway/internal/usage"
	"github.com/cn-maul/rosetta-gateway/internal/userauth"
)

type runtimeReloader = runtime.Reloader

type usageRecorder = usage.Recorder

const (
	usageStatusOK  = usage.StatusOK
	usageQueueSize = usage.QueueSize
)

func newUsageRecorder(db *store.Store, logger *slog.Logger) *usageRecorder {
	return usage.New(usage.Config{
		DB:              db,
		Logger:          logger,
		IsBalanceExempt: gatewayapp.BalanceExempt,
	})
}

// openTestStore 开一个临时库并登记关闭。日志丢弃：这些用例断言的是数据，
// 不是日志；需要看见 DB 报错的用例（如 bootstrap）自己保留 stderr logger。
func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	db, err := store.Open(t.TempDir()+"/gw.db", discardLogger())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// newDiscardApp 组一个只关心路由/管理面形状的 app：临时库、丢弃日志，
// 空的 pool 与限流器。数据面语义由 handleIngress 侧用例覆盖。
func newDiscardApp(t *testing.T) (*gatewayApp, *store.Store) {
	t.Helper()
	db := openTestStore(t)
	return buildApp(routeDeps{
		db: db, cfg: config.Default(), logger: discardLogger(),
		pool: upstream.NewPool(discardLogger()), rateLimiter: ratelimit.New(),
		usage: newUsageRecorder(db, discardLogger()),
	}), db
}

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
