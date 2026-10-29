package store

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	wireproto "github.com/cn-maul/rosetta-gateway/internal/protocol"

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
	var setupTable int
	if err := st.DB().QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'user_password_setup_tokens'`,
	).Scan(&setupTable); err != nil {
		t.Fatalf("inspect password setup table: %v", err)
	}
	if setupTable != 1 {
		t.Fatal("fresh database is missing user_password_setup_tokens")
	}
}

func TestSchema_AddsPasswordSetupTableToExistingCurrentDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "existing.db")
	st := openSchemaTestStore(t, path)
	if _, err := st.DB().Exec(`DROP TABLE user_password_setup_tokens`); err != nil {
		t.Fatalf("drop setup table: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	reopened := openSchemaTestStore(t, path)
	var n int
	if err := reopened.DB().QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'user_password_setup_tokens'`,
	).Scan(&n); err != nil {
		t.Fatalf("inspect setup table: %v", err)
	}
	if n != 1 {
		t.Fatal("existing current-version database did not gain user_password_setup_tokens")
	}
}

func TestSchema_RejectsNonCurrentVersionWithoutModifyingDatabase(t *testing.T) {
	for _, version := range []int{0, 1, currentSchemaVersion + 1} {
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

// 旧版 usage_records 没有 provider_name / upstream_protocol / request_ip。
// 当前版本必须给同版本旧库纯新增这些列，同时保留原有配置与历史数据。
func TestSchema_AddsUsageHistoryColumnsToExistingCurrentDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v166.db")
	st := openSchemaTestStore(t, path)
	if err := st.CreateNetwork(context.Background(), &Network{
		ID: "n1", Name: "keep me", ProxyURL: "http://127.0.0.1:7897",
	}); err != nil {
		t.Fatalf("create network: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// 去掉新增列，精确模拟 v1.6.6 已存在的当前版本库。
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open raw database: %v", err)
	}
	for _, column := range []string{"provider_name", "upstream_protocol", "request_ip"} {
		if _, err := raw.Exec(`ALTER TABLE usage_records DROP COLUMN ` + column); err != nil {
			_ = raw.Close()
			t.Fatalf("drop %s: %v", column, err)
		}
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw database: %v", err)
	}

	reopened := openSchemaTestStore(t, path)
	columns := tableColumns(t, reopened, "usage_records")
	for _, column := range []string{"provider_name", "upstream_protocol", "request_ip"} {
		if !columns[column] {
			t.Errorf("v1.6.6 database did not gain usage_records.%s", column)
		}
	}
	networks, err := reopened.ListNetworks(context.Background())
	if err != nil {
		t.Fatalf("list networks after migration: %v", err)
	}
	if len(networks) != 1 || networks[0].Name != "keep me" {
		t.Fatalf("migration changed existing data: %+v", networks)
	}
}

func TestSchema_AddsR3AccountingObjectsToExistingV168Database(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v168-upgrade.db")
	st := openSchemaTestStore(t, path)
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open raw database: %v", err)
	}
	if _, err := raw.Exec(`DROP TABLE quota_reservation_leases`); err != nil {
		_ = raw.Close()
		t.Fatalf("drop lease table: %v", err)
	}
	if _, err := raw.Exec(`DROP INDEX idx_usage_charge_retry`); err != nil {
		_ = raw.Close()
		t.Fatalf("drop retry index: %v", err)
	}
	// v1.6.8 has the current schema version and usage-history attribution,
	// but none of the R3 accountable-charge columns or objects.
	for _, column := range []string{"charge_attempts", "charge_next_at", "charge_last_error"} {
		if _, err := raw.Exec(`ALTER TABLE usage_records DROP COLUMN ` + column); err != nil {
			_ = raw.Close()
			t.Fatalf("drop %s: %v", column, err)
		}
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw database: %v", err)
	}

	reopened := openSchemaTestStore(t, path)
	var n int
	if err := reopened.DB().QueryRow(
		`SELECT COUNT(*) FROM sqlite_master
		  WHERE type = 'table' AND name = 'quota_reservation_leases'`).Scan(&n); err != nil {
		t.Fatalf("inspect lease table: %v", err)
	}
	if n != 1 {
		t.Fatal("existing current-version database did not gain quota_reservation_leases")
	}
	if err := reopened.DB().QueryRow(
		`SELECT COUNT(*) FROM sqlite_master
		  WHERE type = 'index' AND name = 'idx_usage_charge_retry'`).Scan(&n); err != nil {
		t.Fatalf("inspect retry index: %v", err)
	}
	if n != 1 {
		t.Fatal("existing current-version database did not gain idx_usage_charge_retry")
	}
	columns := tableColumns(t, reopened, "usage_records")
	for _, column := range []string{"charge_attempts", "charge_next_at", "charge_last_error"} {
		if !columns[column] {
			t.Errorf("v1.6.8 database did not gain usage_records.%s", column)
		}
	}
}

func TestSchema_NormalizesLegacyProtocolValues(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy-protocols.db")
	st := openSchemaTestStore(t, path)
	if err := st.CreateProvider(context.Background(), &Provider{
		ID: "p1", Slug: "p1", Name: "P1", Protocol: wireproto.Chat,
		Endpoint: "http://example.invalid/v1", Enabled: true,
	}); err != nil {
		t.Fatalf("create provider: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open raw database: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO usage_records (
			id, ts, access_key_id, public_model, provider_id, upstream_model,
			ingress_protocol, stream, usage_state, status, http_status, latency_ms
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"u1", int64(1700000000000), "k1", "public", "p1", "upstream",
		wireproto.LegacyResponse, 0, "reported", "ok", 200, 10,
	); err != nil {
		t.Fatalf("insert legacy usage row: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO usage_daily_rollups (day, ingress_protocol)
		VALUES (?, ?)`, "2026-10-20", wireproto.LegacyChat); err != nil {
		t.Fatalf("insert legacy rollup row: %v", err)
	}
	if _, err := db.Exec(`UPDATE providers SET protocol = ? WHERE id = ?`,
		wireproto.LegacyChat, "p1"); err != nil {
		t.Fatalf("set legacy provider protocol: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close raw database: %v", err)
	}

	st2 := openSchemaTestStore(t, path)
	var providerProtocol string
	if err := st2.DB().QueryRow(`SELECT protocol FROM providers WHERE id = 'p1'`).
		Scan(&providerProtocol); err != nil {
		t.Fatalf("read provider protocol: %v", err)
	}
	if providerProtocol != wireproto.Chat {
		t.Fatalf("provider protocol = %q, want %q", providerProtocol, wireproto.Chat)
	}

	var ingressProtocol string
	if err := st2.DB().QueryRow(`SELECT ingress_protocol FROM usage_records WHERE id = 'u1'`).
		Scan(&ingressProtocol); err != nil {
		t.Fatalf("read usage protocol: %v", err)
	}
	if ingressProtocol != wireproto.Response {
		t.Fatalf("usage protocol = %q, want %q", ingressProtocol, wireproto.Response)
	}

	var rollupProtocol string
	if err := st2.DB().QueryRow(`SELECT ingress_protocol FROM usage_daily_rollups WHERE day = '2026-10-20'`).
		Scan(&rollupProtocol); err != nil {
		t.Fatalf("read rollup protocol: %v", err)
	}
	if rollupProtocol != wireproto.Chat {
		t.Fatalf("rollup protocol = %q, want %q", rollupProtocol, wireproto.Chat)
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
			"expires_at", "allowed_ips", "group_id", "key_enc",
		},
		"usage_records": {
			"user_id", "error_detail", "route_trace", "cost_total",
			"provider_name", "upstream_protocol", "request_ip",
			"charge_attempts", "charge_next_at", "charge_last_error",
		},
		"users": {"balance_cents", "balance_remainder", "group_id", "reserved_tokens"},
		"usage_daily_rollups": {
			"upstream_model", "ok_ttfb_sum_ms", "ok_ttfb_count",
			"ok_sample_count", "cost_pending_count",
		},
		"quota_reservation_leases": {
			"request_id", "scope", "target_id", "reserved", "released",
			"lease_until", "last_error", "created_at",
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
	if tableColumns(t, st, "upstream_models")["enabled"] {
		t.Error("upstream_models.enabled should not exist: models are always available once added")
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
		ID: "p1", Slug: "p1", Name: "P1", Protocol: "chat",
		Endpoint: "http://example.test", Enabled: true,
	}); err != nil {
		t.Fatalf("create provider: %v", err)
	}
	if err := st.CreateUpstreamModel(ctx, &UpstreamModel{
		ID: "m1", ProviderID: "p1", ModelID: "model-1"}); err != nil {
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
