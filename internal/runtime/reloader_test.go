package runtime_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cn-maul/rosetta-gateway/internal/api"
	"github.com/cn-maul/rosetta-gateway/internal/config"
	"github.com/cn-maul/rosetta-gateway/internal/runtime"
	"github.com/cn-maul/rosetta-gateway/internal/snapshot"
	"github.com/cn-maul/rosetta-gateway/internal/store"
	"github.com/cn-maul/rosetta-gateway/internal/upstream"
)

// R0-AC-10 / R0-AC-11: every failed rebuild, including the first one, enters
// the existing dirty/retry state; a later successful rebuild clears it.
func TestR0AC10_RebuildFailureMarksDirtyUntilSuccessfulRetry(t *testing.T) {
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	st, err := store.Open(filepath.Join(t.TempDir(), "gw.db"), logger)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		snapshot.Init(&snapshot.Snapshot{})
		st.Close()
	})

	pool := upstream.NewPool(logger)
	reloader := runtime.New(st, []byte("test-master-key"), pool, config.Default(), logger)

	// Fail the snapshot stage after the pool stage has succeeded. Renaming the
	// settings table makes GetRuntimeDefaults return a real SQL error, then the
	// rename back restores a healthy database for the retry.
	if _, err := st.DB().ExecContext(ctx,
		`ALTER TABLE app_settings RENAME TO app_settings_r0d_backup`); err != nil {
		t.Fatalf("inject rebuild failure: %v", err)
	}
	if err := reloader.Reload(ctx); err == nil {
		t.Fatal("Reload succeeded while the settings table was unreadable")
	}
	if !reloader.IsDirty() {
		t.Fatal("failed initial-style reload did not mark runtime dirty")
	}

	health := api.HealthzHandler(st, reloader, logger)
	rec := httptest.NewRecorder()
	health(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("healthz status after failed reload = %d, want 503", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, "runtime_lags_behind_database") {
		t.Fatalf("healthz body = %s, want runtime_lags_behind_database", body)
	}

	if _, err := st.DB().ExecContext(ctx,
		`ALTER TABLE app_settings_r0d_backup RENAME TO app_settings`); err != nil {
		t.Fatalf("restore settings table: %v", err)
	}
	if err := reloader.Reload(ctx); err != nil {
		t.Fatalf("retry reload: %v", err)
	}
	if reloader.IsDirty() {
		t.Fatal("successful retry did not clear runtime dirty state")
	}

	rec = httptest.NewRecorder()
	health(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("healthz status after successful retry = %d, want 200", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, `"status":"ok"`) {
		t.Fatalf("healthz body = %s, want healthy status", body)
	}
}
