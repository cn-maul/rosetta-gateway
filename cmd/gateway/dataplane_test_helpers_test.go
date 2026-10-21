package main

import (
	"net/http"

	"github.com/cn-maul/rosetta-gateway/internal/config"
	"github.com/cn-maul/rosetta-gateway/internal/dataplane"
	"github.com/cn-maul/rosetta-gateway/internal/ingress"
	"github.com/cn-maul/rosetta-gateway/internal/ratelimit"
	"github.com/cn-maul/rosetta-gateway/internal/upstream"
)

// handleIngress keeps the existing test harness call shape while exercising
// the production data plane handler.
func handleIngress(
	pool *upstream.Pool,
	cfg *config.Config,
	usage *usageRecorder,
	limiter *ratelimit.Limiter,
	codec ingress.Codec,
) http.HandlerFunc {
	return dataplane.NewHandler(dataplane.Deps{
		Pool: pool, Config: cfg, Usage: usage, Limiter: limiter,
	}).Handle(codec)
}
