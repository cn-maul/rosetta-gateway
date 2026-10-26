package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/cn-maul/rosetta-gateway/internal/lifecycle"
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
	return HealthzReadinessHandler(legacyReadiness{db: db, runtime: reloader}, logger)
}

// HealthzReadinessHandler projects the lifecycle-owned readiness result.
func HealthzReadinessHandler(checker lifecycle.Checker, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		ready := checker.Readiness(ctx)
		body := map[string]any{
			"status": ready.Status,
			"checks": map[string]string{
				"database":  string(ready.Database),
				"runtime":   string(ready.Runtime),
				"bootstrap": string(ready.Bootstrap),
			},
		}
		if ready.ErrorCode != "" {
			body["error_code"] = ready.ErrorCode
			if ready.Database == lifecycle.DimensionFailed {
				logger.Error("healthz: database ping failed", "error_code", ready.ErrorCode)
			}
		}
		code := http.StatusOK
		if ready.Status != "ok" {
			code = http.StatusServiceUnavailable
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(body)
	}
}

type legacyReadiness struct {
	db      HealthzPinger
	runtime HealthzDirty
}

func (l legacyReadiness) Readiness(ctx context.Context) lifecycle.Readiness {
	result := lifecycle.Readiness{
		Status: "ok", Database: lifecycle.DimensionOK,
		Runtime: lifecycle.DimensionOK, Bootstrap: lifecycle.DimensionOK,
	}
	if err := l.db.Ping(ctx); err != nil {
		result.Status = "unhealthy"
		result.ErrorCode = "database_unavailable"
		result.Database = lifecycle.DimensionFailed
	} else if l.runtime.IsDirty() {
		result.Status = "degraded"
		result.ErrorCode = "runtime_lags_behind_database"
		result.Runtime = lifecycle.DimensionPending
	}
	return result
}
