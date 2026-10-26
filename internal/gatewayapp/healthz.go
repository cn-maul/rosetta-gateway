package gatewayapp

import (
	"log/slog"
	"net/http"

	"github.com/cn-maul/rosetta-gateway/internal/api"
	"github.com/cn-maul/rosetta-gateway/internal/lifecycle"
)

// HealthzPinger is kept as a compatibility alias for command-local tests.
type HealthzPinger = api.HealthzPinger

// HealthzDirty is kept as a compatibility alias for command-local tests.
type HealthzDirty = api.HealthzDirty

// HealthzHandler delegates to the HTTP assembly package.
func HealthzHandler(db HealthzPinger, reloader HealthzDirty, logger *slog.Logger) http.HandlerFunc {
	return api.HealthzHandler(db, reloader, logger)
}

// HealthzReadinessHandler projects lifecycle-owned readiness.
func HealthzReadinessHandler(checker lifecycle.Checker, logger *slog.Logger) http.HandlerFunc {
	return api.HealthzReadinessHandler(checker, logger)
}
