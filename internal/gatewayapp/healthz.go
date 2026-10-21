package gatewayapp

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"
)

// HealthzPinger is the storage capability required by the health endpoint.
type HealthzPinger interface {
	Ping(context.Context) error
}

// HealthzDirty reports whether runtime state lags the database.
type HealthzDirty interface {
	IsDirty() bool
}

// HealthzHandler reports database health and runtime convergence without
// exposing underlying error details on the unauthenticated endpoint.
func HealthzHandler(db HealthzPinger, reloader HealthzDirty, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		body := map[string]any{"status": "ok"}
		code := http.StatusOK
		if err := db.Ping(ctx); err != nil {
			logger.Error("healthz: database ping failed", "error", err)
			body = map[string]any{"status": "unhealthy", "error_code": "database_unavailable"}
			code = http.StatusServiceUnavailable
		} else if reloader.IsDirty() {
			body = map[string]any{"status": "degraded", "error_code": "runtime_lags_behind_database"}
			code = http.StatusServiceUnavailable
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(body)
	}
}
