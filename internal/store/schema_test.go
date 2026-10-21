package store

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

func openSchemaTestStore(t *testing.T, path string) *Store {
	t.Helper()
	st, err := Open(path, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("Open(%s): %v", path, err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func TestSchema_FreshDatabaseUsesCurrentVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fresh.db")
	st := openSchemaTestStore(t, path)

	var version int
	if err := st.DB().QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatalf("read user_version: %v", err)
	}
	if version != currentSchemaVersion {
		t.Fatalf("user_version = %d, want %d", version, currentSchemaVersion)
	}
}

func TestSchema_RejectsNonCurrentVersionWithoutModifyingDatabase(t *testing.T) {
	for _, version := range []int{0, currentSchemaVersion + 1} {
		t.Run("version_"+schemaTestItoa(version), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "old.db")
			seedVersionedDatabase(t, path, version)

			_, err := Open(path, slog.New(slog.NewTextHandler(io.Discard, nil)))
			if !errors.Is(err, ErrUnsupportedSchemaVersion) {
				t.Fatalf("Open error = %v, want ErrUnsupportedSchemaVersion", err)
			}

			db, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatalf("reopen raw: %v", err)
			}
			defer db.Close()

			var gotVersion int
			if err := db.QueryRow(`PRAGMA user_version`).Scan(&gotVersion); err != nil {
				t.Fatalf("read user_version: %v", err)
			}
			if gotVersion != version {
				t.Fatalf("rejected database version changed: got %d, want %d", gotVersion, version)
			}
			var value string
			if err := db.QueryRow(`SELECT value FROM marker WHERE id = 1`).Scan(&value); err != nil {
				t.Fatalf("read marker: %v", err)
			}
			if value != "preserved" {
				t.Fatalf("rejected database content changed: %q", value)
			}
			var unexpected int
			if err := db.QueryRow(`
				SELECT COUNT(*) FROM sqlite_master
				 WHERE name NOT IN ('marker', 'sqlite_autoindex_marker_1')`).Scan(&unexpected); err != nil {
				t.Fatalf("count schema objects: %v", err)
			}
			if unexpected != 0 {
				t.Fatalf("rejected database gained %d schema objects", unexpected)
			}
		})
	}
}

func TestSchema_ReopensCurrentVersionAndKeepsData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "current.db")
	st := openSchemaTestStore(t, path)
	if err := st.CreateNetwork(context.Background(), &Network{
		ID: "n1", Name: "proxy", ProxyURL: "http://127.0.0.1:7897",
	}); err != nil {
		t.Fatalf("create network: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	st2 := openSchemaTestStore(t, path)
	networks, err := st2.ListNetworks(context.Background())
	if err != nil {
		t.Fatalf("list networks: %v", err)
	}
	if len(networks) != 1 || networks[0].Name != "proxy" {
		t.Fatalf("current database data not preserved: %+v", networks)
	}
}

func TestSchema_CanonicalObjectsAndColumns(t *testing.T) {
	st := openSchemaTestStore(t, filepath.Join(t.TempDir(), "canonical.db"))

	requiredTriggers := []string{
		"trg_update_used_tokens",
		"trg_update_user_usage_totals",
		"trg_update_usage_totals",
		"trg_update_usage_totals_cost",
	}
	for _, name := range requiredTriggers {
		var typ string
		if err := st.DB().QueryRow(
			`SELECT type FROM sqlite_master WHERE name = ?`, name).Scan(&typ); err != nil {
			t.Fatalf("trigger %s missing: %v", name, err)
		}
		if typ != "trigger" {
			t.Fatalf("%s has type %q, want trigger", name, typ)
		}
	}

	requiredIndexes := []string{
		"idx_usage_ts",
		"idx_usage_user_ts",
		"idx_access_keys_user",
		"idx_users_group",
		"idx_route_targets_route",
		"idx_route_targets_unique",
		"idx_rollup_day",
		"idx_rollup_user",
		"idx_rollup_key",
	}
	for _, name := range requiredIndexes {
		var typ string
		if err := st.DB().QueryRow(
			`SELECT type FROM sqlite_master WHERE name = ?`, name).Scan(&typ); err != nil {
			t.Fatalf("index %s missing: %v", name, err)
		}
		if typ != "index" {
			t.Fatalf("%s has type %q, want index", name, typ)
		}
	}

	requiredColumns := map[string][]string{
		"access_keys": {
			"user_id", "rpm_limit", "tpm_limit", "allowed_models_json",
			"expires_at", "allowed_ips", "group_id",
		},
		"usage_records": {"user_id", "error_detail", "route_trace", "cost_total"},
		"users":         {"balance_cents", "balance_remainder", "group_id", "reserved_tokens"},
		"usage_daily_rollups": {
			"upstream_model", "ok_ttfb_sum_ms", "ok_ttfb_count",
			"ok_sample_count", "cost_pending_count",
		},
	}
	for table, columns := range requiredColumns {
		got := tableColumns(t, st, table)
		for _, column := range columns {
			if !got[column] {
				t.Errorf("%s.%s missing from canonical schema", table, column)
			}
		}
	}

	var rowCount int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM usage_totals WHERE id = 1`).Scan(&rowCount); err != nil {
		t.Fatalf("read usage_totals placeholder: %v", err)
	}
	if rowCount != 1 {
		t.Fatalf("usage_totals(id=1) rows = %d, want 1", rowCount)
	}
}

func TestCreateRoute_SeedsHeadTargetAtomically(t *testing.T) {
	ctx := context.Background()
	st := openSchemaTestStore(t, filepath.Join(t.TempDir(), "route.db"))
	if err := st.CreateProvider(ctx, &Provider{
		ID: "p1", Slug: "p1", Name: "P1", Protocol: "openai-chat",
		Endpoint: "http://example.test", Enabled: true,
	}); err != nil {
		t.Fatalf("create provider: %v", err)
	}
	if err := st.CreateUpstreamModel(ctx, &UpstreamModel{
		ID: "m1", ProviderID: "p1", ModelID: "model-1", Enabled: true,
	}); err != nil {
		t.Fatalf("create model: %v", err)
	}
	if err := st.CreateRoute(ctx, &Route{
		ID: "r1", PublicName: "public", ProviderID: "p1",
		UpstreamModelID: "m1", Enabled: true,
	}); err != nil {
		t.Fatalf("create route: %v", err)
	}

	targets, err := st.ListRouteTargets(ctx, "r1")
	if err != nil {
		t.Fatalf("list targets: %v", err)
	}
	if len(targets) != 1 {
		t.Fatalf("targets = %d, want 1", len(targets))
	}
	if targets[0].Position != 0 || targets[0].ProviderID != "p1" ||
		targets[0].UpstreamModelID != "m1" ||
		targets[0].Enabled != true {
		t.Fatalf("head target mismatch: %+v", targets[0])
	}
}

func seedVersionedDatabase(t *testing.T, path string, version int) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE marker (id INTEGER PRIMARY KEY, value TEXT NOT NULL)`); err != nil {
		t.Fatalf("create marker: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO marker (id, value) VALUES (1, 'preserved')`); err != nil {
		t.Fatalf("insert marker: %v", err)
	}
	if _, err := db.Exec(`PRAGMA user_version = ` + schemaTestItoa(version)); err != nil {
		t.Fatalf("set user_version: %v", err)
	}
}

func tableColumns(t *testing.T, st *Store, table string) map[string]bool {
	t.Helper()
	rows, err := st.DB().Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		t.Fatalf("table_info(%s): %v", table, err)
	}
	defer rows.Close()
	out := make(map[string]bool)
	for rows.Next() {
		var (
			cid, notNull, pk int
			name, typ        string
			defaultValue     any
		)
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &pk); err != nil {
			t.Fatalf("scan table_info(%s): %v", table, err)
		}
		out[name] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("table_info(%s): %v", table, err)
	}
	return out
}

func schemaTestItoa(v int) string {
	if v == 0 {
		return "0"
	}
	var digits [20]byte
	i := len(digits)
	for v > 0 {
		i--
		digits[i] = byte('0' + v%10)
		v /= 10
	}
	return string(digits[i:])
}
