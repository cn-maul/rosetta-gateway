package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	wireproto "github.com/cn-maul/rosetta-gateway/internal/protocol"
)

// currentSchemaVersion 是当前代码写入的数据库结构版本。
//
// 破坏性结构变更不提供原地迁移：提升版本并更新 canonical DDL，旧数据库
// 会被拒绝启动。开发与测试库直接删除重建，正式环境通过配置导出/导入搬运。
// 纯新增对象和值归一化仍可通过 ensureCompatibleSchema /
// normalizeLegacyProtocolValues 原地补齐。
const currentSchemaVersion = 2

// ErrUnsupportedSchemaVersion marks a database that this build must not touch.
var ErrUnsupportedSchemaVersion = errors.New("unsupported database schema version")

func (s *Store) ensureSchema(ctx context.Context) error {
	var version int
	if err := s.db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}

	var hasObjects int
	if err := s.db.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM sqlite_master
			WHERE name NOT LIKE 'sqlite_%'
		)`).Scan(&hasObjects); err != nil {
		return fmt.Errorf("inspect database schema: %w", err)
	}

	if hasObjects == 0 {
		if version != 0 {
			return unsupportedSchemaVersion(version)
		}
		if err := s.initializeSchema(ctx); err != nil {
			return err
		}
	} else if version != currentSchemaVersion {
		return unsupportedSchemaVersion(version)
	}

	// Additive, backward-compatible objects may be introduced without bumping
	// currentSchemaVersion. The setup-token table is one of those: old databases
	// keep their data and gain the table on the next start.
	if err := s.ensureCompatibleSchema(ctx); err != nil {
		return err
	}

	// The protocol rename changes values, not columns. Normalize existing data
	// before any provider snapshot is rebuilt from the database.
	if err := s.normalizeLegacyProtocolValues(ctx); err != nil {
		return err
	}

	// Reservations are process-local. A previous process cannot still own one
	// after this process has opened the database.
	if _, err := s.recoverStaleReservations(ctx); err != nil {
		return err
	}
	s.logger.Info("database schema ready", "version", currentSchemaVersion)
	return nil
}

// ensureCompatibleSchema adds tables that can be created idempotently on
// databases already stamped with currentSchemaVersion. Keep this limited to
// additive objects: destructive changes still require a schema-version bump.
func (s *Store) ensureCompatibleSchema(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, userPasswordSetupTokensDDL); err != nil {
		return fmt.Errorf("ensure user password setup token schema: %w", err)
	}
	// R3 adds durable quota-lease and charge-retry state. Both objects are
	// additive, so a database already stamped with currentSchemaVersion can
	// acquire them in place. Keep this in compatibility setup rather than the
	// canonical-only path: otherwise an upgraded database succeeds at startup
	// and then fails on the first request that touches the new state.
	if _, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS quota_reservation_leases (
		request_id    TEXT NOT NULL,
		scope         TEXT NOT NULL,
		target_id     TEXT NOT NULL,
		reserved      INTEGER NOT NULL,
		released      INTEGER NOT NULL DEFAULT 0,
		lease_until   INTEGER NOT NULL,
		last_error    TEXT,
		created_at    INTEGER NOT NULL,
		PRIMARY KEY (request_id, scope, target_id)
	)`); err != nil {
		return fmt.Errorf("ensure quota reservation lease table: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS idx_usage_charge_retry
		ON usage_records(usage_state, charge_next_at, ts)`); err != nil {
		return fmt.Errorf("ensure usage charge retry index: %w", err)
	}
	// 2026-10-22 修 F2：cache creation 的独立单价与归档列。
	//
	// 这些是**带默认值的纯新增列**，旧库可以原地升级，不需要提升
	// currentSchemaVersion（破坏性变更才需要）。默认值 0 与「未配置」同义：
	// 未配置 price_cache_creation 时按 price_input 计费，与修复前的行为
	// 逐字一致，因此升级不会改变任何历史或当次金额。
	for _, col := range []struct {
		table  string
		column string
		decl   string
	}{
		{"upstream_models", "price_cache_creation", "REAL"},
		{"usage_records", "cached_creation_tokens", "INTEGER NOT NULL DEFAULT 0"},
		{"usage_daily_rollups", "cached_creation_tokens", "INTEGER NOT NULL DEFAULT 0"},
		{"usage_totals", "cached_creation_tokens", "INTEGER NOT NULL DEFAULT 0"},
		// 2026-10-23：调用历史要回答「实际是谁接的、上下游各是什么协议」。
		// 这两列都是**固化值**，不是 JOIN 当前配置：provider 改名/删除、
		// 路由改协议后，旧记录仍应显示当时的事实。
		{"usage_records", "provider_name", "TEXT"},
		{"usage_records", "upstream_protocol", "TEXT"},
		// 2026-10-25 R3: recoverable accounting retry state. The defaults
		// preserve existing behavior: every historical row is immediately
		// eligible, has made no retry attempts, and has no recorded error.
		{"usage_records", "charge_attempts", "INTEGER NOT NULL DEFAULT 0"},
		{"usage_records", "charge_next_at", "INTEGER NOT NULL DEFAULT 0"},
		{"usage_records", "charge_last_error", "TEXT"},
	} {
		if err := s.ensureAdditiveColumn(ctx, col.table, col.column, col.decl); err != nil {
			return err
		}
	}
	// 旧库的 trg_update_usage_totals 不包含新列，DROP + CREATE 让新增列
	// 从下一行明细开始进入终身累计。DROP/CREATE 都是幂等的，且触发器只
	// 影响后续 INSERT。
	if _, err := s.db.ExecContext(ctx, `DROP TRIGGER IF EXISTS trg_update_usage_totals`); err != nil {
		return fmt.Errorf("refresh usage totals trigger: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, usageTotalsInsertTriggerDDL); err != nil {
		return fmt.Errorf("recreate usage totals trigger: %w", err)
	}
	return nil
}

// ensureAdditiveColumn 在列不存在时给已有表补一列。
//
// 只接受**可空或带默认值**的新增列：SQLite 的 ALTER TABLE ADD COLUMN
// 无法给旧行补一个没有默认值的 NOT NULL 列。调用方传的 decl 必须满足
// 这一点，否则 ALTER 会在旧库上失败（新库走 canonical DDL，不受影响）。
func (s *Store) ensureAdditiveColumn(ctx context.Context, table, column, decl string) error {
	rows, err := s.db.QueryContext(ctx, `PRAGMA table_info(`+table+`)`)
	if err != nil {
		return fmt.Errorf("inspect %s columns: %w", table, err)
	}

	found := false
	for rows.Next() {
		var (
			cid     int
			name    string
			ctype   string
			notNull int
			dflt    sql.NullString
			pk      int
		)
		if err := rows.Scan(&cid, &name, &ctype, &notNull, &dflt, &pk); err != nil {
			return fmt.Errorf("scan %s columns: %w", table, err)
		}
		if name == column {
			found = true
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("read %s columns: %w", table, err)
	}
	// 必须先关闭行集再执行 ALTER：写池是单连接（SetMaxOpenConns(1)），
	// 行集未释放时 Exec 会等待同一个连接，形成永久自锁。
	rows.Close()
	if found {
		return nil
	}
	if _, err := s.db.ExecContext(ctx, `ALTER TABLE `+table+` ADD COLUMN `+column+` `+decl); err != nil {
		return fmt.Errorf("add column %s.%s: %w", table, column, err)
	}
	return nil
}

// normalizeLegacyProtocolValues upgrades the protocol values introduced before
// the switch to chat / response. It is a data-only migration (the column types
// and indexes are unchanged), so databases keep the same schema version.
func (s *Store) normalizeLegacyProtocolValues(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin protocol value migration: %w", err)
	}
	defer func() { _ = tx.Rollback() }() //nolint:errcheck // no-op after commit

	updates := []struct {
		name string
		sql  string
	}{
		{
			name: "providers.protocol",
			sql: `UPDATE providers
				SET protocol = CASE protocol
					WHEN ? THEN ?
					WHEN ? THEN ?
					ELSE protocol
				END
				WHERE protocol IN (?, ?)`,
		},
		{
			name: "usage_records.ingress_protocol",
			sql: `UPDATE usage_records
				SET ingress_protocol = CASE ingress_protocol
					WHEN ? THEN ?
					WHEN ? THEN ?
					ELSE ingress_protocol
				END
				WHERE ingress_protocol IN (?, ?)`,
		},
		{
			name: "usage_daily_rollups.ingress_protocol",
			sql: `UPDATE usage_daily_rollups
				SET ingress_protocol = CASE ingress_protocol
					WHEN ? THEN ?
					WHEN ? THEN ?
					ELSE ingress_protocol
				END
				WHERE ingress_protocol IN (?, ?)`,
		},
	}

	for _, update := range updates {
		result, err := tx.ExecContext(ctx, update.sql,
			wireproto.LegacyChat, wireproto.Chat,
			wireproto.LegacyResponse, wireproto.Response,
			wireproto.LegacyChat, wireproto.LegacyResponse,
		)
		if err != nil {
			return fmt.Errorf("migrate %s: %w", update.name, err)
		}
		if changed, err := result.RowsAffected(); err == nil && changed > 0 {
			s.logger.Info("migrated legacy protocol values", "field", update.name, "rows", changed)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit protocol value migration: %w", err)
	}
	return nil
}

func unsupportedSchemaVersion(found int) error {
	return fmt.Errorf(
		"%w: found %d, require %d; export the configuration, recreate the database, then import it",
		ErrUnsupportedSchemaVersion, found, currentSchemaVersion)
}

func (s *Store) initializeSchema(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin schema initialization: %w", err)
	}
	defer func() { _ = tx.Rollback() }() //nolint:errcheck // no-op after commit

	for i, stmt := range canonicalSchemaStatements {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("initialize schema statement %d: %w", i+1, err)
		}
	}
	if _, err := tx.ExecContext(ctx,
		fmt.Sprintf(`PRAGMA user_version = %d`, currentSchemaVersion)); err != nil {
		return fmt.Errorf("write schema version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit schema initialization: %w", err)
	}
	return nil
}

// canonicalSchemaStatements is the complete, authoritative schema. Do not add
// best-effort ALTER/backfill statements here: unsupported databases are
// rejected before any statement runs.
//
// usageTotalsInsertTriggerDDL 单独抽成常量，是因为它既要出现在 canonical
// schema 里，又要在 ensureCompatibleSchema 中随新增列重建 —— 两处必须逐字
// 同一份定义，否则「新库的累计含新列、旧库的不含」会静默分叉。
const usageTotalsInsertTriggerDDL = `CREATE TRIGGER trg_update_usage_totals
 AFTER INSERT ON usage_records
 BEGIN
   UPDATE usage_totals SET
     request_count    = request_count + 1,
     success_count    = success_count + CASE WHEN NEW.status = 'ok' THEN 1 ELSE 0 END,
     error_count      = error_count + CASE WHEN NEW.status NOT IN ('ok', 'canceled') THEN 1 ELSE 0 END,
     input_tokens     = input_tokens + NEW.input_tokens,
     output_tokens    = output_tokens + NEW.output_tokens,
     total_tokens     = total_tokens + NEW.total_tokens,
     cached_tokens    = cached_tokens + NEW.cached_tokens,
     cached_creation_tokens = cached_creation_tokens + NEW.cached_creation_tokens,
     reasoning_tokens = reasoning_tokens + NEW.reasoning_tokens,
     cost_total       = cost_total + NEW.cost_total,
     latency_sum_ms   = latency_sum_ms + NEW.latency_ms,
     first_record_at  = CASE WHEN first_record_at = 0 THEN NEW.ts
                             ELSE MIN(first_record_at, NEW.ts) END
   WHERE id = 1;
 END`

var canonicalSchemaStatements = []string{
	`CREATE TABLE networks (
		id         TEXT PRIMARY KEY,
		name       TEXT NOT NULL UNIQUE,
		proxy_url  TEXT NOT NULL,
		created_at INTEGER NOT NULL
	)`,
	`CREATE TABLE providers (
		id                 TEXT PRIMARY KEY,
		slug               TEXT NOT NULL UNIQUE,
		name               TEXT NOT NULL,
		protocol           TEXT NOT NULL DEFAULT 'auto',
		endpoint           TEXT NOT NULL,
		enabled            INTEGER NOT NULL DEFAULT 1,
		timeout_ms         INTEGER NOT NULL DEFAULT 0,
		max_retries        INTEGER NOT NULL DEFAULT 2,
		quirks_json        TEXT,
		network_id         TEXT REFERENCES networks(id) ON DELETE SET NULL,
		balance_spec_json  TEXT,
		created_at         INTEGER NOT NULL,
		updated_at         INTEGER NOT NULL
	)`,
	`CREATE TABLE provider_credentials (
		id             TEXT PRIMARY KEY,
		provider_id    TEXT NOT NULL REFERENCES providers(id) ON DELETE CASCADE,
		label          TEXT,
		api_key_enc    BLOB NOT NULL,
		enabled        INTEGER NOT NULL DEFAULT 1,
		weight         INTEGER NOT NULL DEFAULT 1,
		status         TEXT NOT NULL DEFAULT 'healthy',
		cooldown_until INTEGER NOT NULL DEFAULT 0,
		last_error     TEXT,
		created_at     INTEGER NOT NULL
	)`,
	`CREATE TABLE model_groups (
		id           TEXT PRIMARY KEY,
		display_name TEXT NOT NULL UNIQUE,
		created_at   INTEGER NOT NULL
	)`,
	`CREATE TABLE upstream_models (
		id                TEXT PRIMARY KEY,
		provider_id       TEXT NOT NULL REFERENCES providers(id) ON DELETE CASCADE,
		model_id          TEXT NOT NULL,
		display_name      TEXT,
		context_window    INTEGER,
		max_output_tokens INTEGER,
		supports_thinking INTEGER,
		effort_levels     TEXT,
		price_input       REAL,
		price_cache_hit   REAL,
		price_cache_creation REAL,
		price_output      REAL,
		group_id          TEXT REFERENCES model_groups(id) ON DELETE SET NULL,
		UNIQUE (provider_id, model_id)
	)`,
	`CREATE TABLE groups (
		id          TEXT PRIMARY KEY,
		name        TEXT NOT NULL COLLATE NOCASE UNIQUE,
		description TEXT,
		created_at  INTEGER NOT NULL,
		updated_at  INTEGER NOT NULL
	)`,
	`CREATE TABLE users (
		id                 TEXT PRIMARY KEY,
		username           TEXT NOT NULL COLLATE NOCASE UNIQUE,
		display_name       TEXT,
		password_hash      TEXT NOT NULL,
		role               TEXT NOT NULL DEFAULT 'user',
		status             TEXT NOT NULL DEFAULT 'active',
		quota_tokens       INTEGER NOT NULL DEFAULT 0,
		used_tokens        INTEGER NOT NULL DEFAULT 0,
		reserved_tokens    INTEGER NOT NULL DEFAULT 0,
		balance_cents      INTEGER,
		balance_remainder  INTEGER NOT NULL DEFAULT 0,
		auth_version       INTEGER NOT NULL DEFAULT 1,
		group_id           TEXT REFERENCES groups(id) ON DELETE SET NULL,
		remark             TEXT,
		created_at         INTEGER NOT NULL,
		updated_at         INTEGER NOT NULL,
		last_login_at      INTEGER NOT NULL DEFAULT 0
	)`,
	userPasswordSetupTokensDDL,
	`CREATE TABLE access_keys (
		id                  TEXT PRIMARY KEY,
		key_hash            TEXT NOT NULL UNIQUE,
		key_prefix          TEXT NOT NULL,
		name                TEXT NOT NULL,
		enabled             INTEGER NOT NULL DEFAULT 1,
		quota_tokens        INTEGER NOT NULL DEFAULT 0,
		used_tokens         INTEGER NOT NULL DEFAULT 0,
		reserved_tokens     INTEGER NOT NULL DEFAULT 0,
		user_id             TEXT REFERENCES users(id) ON DELETE CASCADE,
		rpm_limit           INTEGER NOT NULL DEFAULT 0,
		tpm_limit           INTEGER NOT NULL DEFAULT 0,
		allowed_models_json TEXT,
		expires_at          INTEGER NOT NULL DEFAULT 0,
		allowed_ips         TEXT,
		group_id            TEXT REFERENCES groups(id) ON DELETE SET NULL,
		created_at          INTEGER NOT NULL
	)`,
	`CREATE TABLE user_group_models (
		group_id     TEXT NOT NULL REFERENCES groups(id) ON DELETE CASCADE,
		public_model TEXT NOT NULL,
		PRIMARY KEY (group_id, public_model)
	)`,
	`CREATE TABLE routes (
		id                TEXT PRIMARY KEY,
		public_name       TEXT NOT NULL UNIQUE,
		provider_id       TEXT NOT NULL REFERENCES providers(id) ON DELETE RESTRICT,
		upstream_model_id TEXT NOT NULL REFERENCES upstream_models(id) ON DELETE RESTRICT,
		enabled           INTEGER NOT NULL DEFAULT 1,
		failover_enabled  INTEGER NOT NULL DEFAULT 0,
		created_at        INTEGER NOT NULL
	)`,
	`CREATE TABLE route_targets (
		id                TEXT PRIMARY KEY,
		route_id          TEXT NOT NULL REFERENCES routes(id) ON DELETE CASCADE,
		provider_id       TEXT NOT NULL REFERENCES providers(id) ON DELETE RESTRICT,
		upstream_model_id TEXT NOT NULL REFERENCES upstream_models(id) ON DELETE RESTRICT,
		position          INTEGER NOT NULL DEFAULT 0,
		enabled           INTEGER NOT NULL DEFAULT 1,
		created_at        INTEGER NOT NULL
	)`,
	`CREATE TABLE usage_records (
		id                TEXT PRIMARY KEY,
		ts                INTEGER NOT NULL,
		access_key_id     TEXT NOT NULL,
		user_id           TEXT,
		public_model      TEXT NOT NULL,
		provider_id       TEXT NOT NULL,
		provider_name     TEXT,
		upstream_model    TEXT NOT NULL,
		ingress_protocol  TEXT NOT NULL,
		upstream_protocol TEXT,
		stream            INTEGER NOT NULL,
		input_tokens      INTEGER NOT NULL DEFAULT 0,
		output_tokens     INTEGER NOT NULL DEFAULT 0,
		total_tokens      INTEGER NOT NULL DEFAULT 0,
		reasoning_tokens  INTEGER NOT NULL DEFAULT 0,
		cached_tokens     INTEGER NOT NULL DEFAULT 0,
		cached_creation_tokens INTEGER NOT NULL DEFAULT 0,
		usage_state       TEXT NOT NULL,
		status            TEXT NOT NULL,
		http_status       INTEGER NOT NULL,
		error_code        TEXT,
		error_detail      TEXT,
		latency_ms        INTEGER NOT NULL,
		ttfb_ms           INTEGER NOT NULL DEFAULT 0,
		request_id        TEXT,
		route_trace       TEXT,
		cost_total        REAL NOT NULL DEFAULT 0,
		-- Accounting retry state. These columns are additive and default to
		-- the existing "eligible now" behavior for every historical row.
		charge_attempts   INTEGER NOT NULL DEFAULT 0,
		charge_next_at    INTEGER NOT NULL DEFAULT 0,
		charge_last_error TEXT
	)`,
	`CREATE TABLE user_usage_totals (
		user_id      TEXT PRIMARY KEY,
		total_tokens INTEGER NOT NULL DEFAULT 0
	)`,
	`CREATE TABLE usage_totals (
		id                 INTEGER PRIMARY KEY CHECK (id = 1),
		request_count      INTEGER NOT NULL DEFAULT 0,
		success_count      INTEGER NOT NULL DEFAULT 0,
		error_count        INTEGER NOT NULL DEFAULT 0,
		input_tokens       INTEGER NOT NULL DEFAULT 0,
		output_tokens      INTEGER NOT NULL DEFAULT 0,
		cached_tokens      INTEGER NOT NULL DEFAULT 0,
		cached_creation_tokens INTEGER NOT NULL DEFAULT 0,
		reasoning_tokens   INTEGER NOT NULL DEFAULT 0,
		total_tokens       INTEGER NOT NULL DEFAULT 0,
		cost_total         REAL NOT NULL DEFAULT 0,
		latency_sum_ms     INTEGER NOT NULL DEFAULT 0,
		first_record_at    INTEGER NOT NULL DEFAULT 0,
		pruned_through_day TEXT NOT NULL DEFAULT ''
	)`,
	usageDailyRollupsDDL,
	`CREATE TABLE app_settings (
		key        TEXT PRIMARY KEY,
		value      TEXT NOT NULL,
		updated_at INTEGER NOT NULL
	)`,
	`CREATE TABLE audit_log (
		id     INTEGER PRIMARY KEY AUTOINCREMENT,
		ts     INTEGER NOT NULL,
		actor  TEXT NOT NULL,
		remote TEXT NOT NULL,
		method TEXT NOT NULL,
		path   TEXT NOT NULL,
		status INTEGER NOT NULL,
		fields TEXT
	)`,
	`CREATE TABLE balance_charges (
		request_id   TEXT NOT NULL,
		user_id      TEXT NOT NULL,
		amount_cents INTEGER NOT NULL,
		ts           INTEGER NOT NULL,
		PRIMARY KEY (request_id, user_id)
	)`,
	`CREATE TABLE balance_pending (
		request_id    TEXT NOT NULL,
		user_id       TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		amount_micros INTEGER NOT NULL,
		ts            INTEGER NOT NULL,
		PRIMARY KEY (request_id, user_id)
	)`,
	`CREATE TABLE quota_reservation_leases (
		request_id    TEXT NOT NULL,
		scope         TEXT NOT NULL,
		target_id     TEXT NOT NULL,
		reserved      INTEGER NOT NULL,
		released      INTEGER NOT NULL DEFAULT 0,
		lease_until   INTEGER NOT NULL,
		last_error    TEXT,
		created_at    INTEGER NOT NULL,
		PRIMARY KEY (request_id, scope, target_id)
	)`,
	`CREATE TABLE balance_topups (
		id                TEXT PRIMARY KEY,
		user_id           TEXT NOT NULL,
		delta_cents       INTEGER NOT NULL,
		balance_after     INTEGER NOT NULL,
		was_unlimited     INTEGER NOT NULL DEFAULT 0,
		operator_id       TEXT NOT NULL,
		operator_username TEXT,
		ts                INTEGER NOT NULL,
		remark            TEXT
	)`,
	`CREATE TABLE model_probes (
		id           INTEGER PRIMARY KEY AUTOINCREMENT,
		model_id     TEXT NOT NULL,
		provider_id  TEXT NOT NULL,
		ts           INTEGER NOT NULL,
		ok           INTEGER NOT NULL,
		in_band      INTEGER NOT NULL DEFAULT 0,
		latency_ms   INTEGER NOT NULL DEFAULT 0,
		error_kind   TEXT,
		error_detail TEXT
	)`,

	`CREATE INDEX idx_usage_ts ON usage_records(ts)`,
	`CREATE INDEX idx_usage_key_ts ON usage_records(access_key_id, ts)`,
	`CREATE INDEX idx_usage_model_ts ON usage_records(public_model, ts)`,
	`CREATE INDEX idx_usage_prov_ts ON usage_records(provider_id, ts)`,
	`CREATE INDEX idx_usage_user_ts ON usage_records(user_id, ts)`,
	`CREATE INDEX idx_usage_charge_retry
		ON usage_records(usage_state, charge_next_at, ts)`,
	`CREATE INDEX idx_access_keys_user ON access_keys(user_id)`,
	`CREATE INDEX idx_users_group ON users(group_id)`,
	`CREATE INDEX idx_route_targets_route ON route_targets(route_id, position)`,
	`CREATE UNIQUE INDEX idx_route_targets_unique
		ON route_targets(route_id, provider_id, upstream_model_id)`,
	`CREATE INDEX idx_rollup_day ON usage_daily_rollups(day)`,
	`CREATE INDEX idx_rollup_user ON usage_daily_rollups(user_id)`,
	`CREATE INDEX idx_rollup_key ON usage_daily_rollups(access_key_id)`,
	`CREATE INDEX idx_audit_ts ON audit_log(ts)`,
	`CREATE INDEX idx_balance_charges_user ON balance_charges(user_id, ts)`,
	`CREATE INDEX idx_balance_pending_user ON balance_pending(user_id)`,
	`CREATE INDEX idx_balance_topups_user ON balance_topups(user_id, ts)`,
	`CREATE INDEX idx_probes_model_ts ON model_probes(model_id, ts)`,
	`CREATE INDEX idx_probes_provider_ts ON model_probes(provider_id, ts)`,

	`INSERT INTO usage_totals (id) VALUES (1)`,

	`CREATE TRIGGER trg_update_used_tokens
	 AFTER INSERT ON usage_records
	 BEGIN
	   UPDATE access_keys SET used_tokens = used_tokens + NEW.total_tokens
	   WHERE id = NEW.access_key_id;
	 END`,
	`CREATE TRIGGER trg_update_user_usage_totals
	 AFTER INSERT ON usage_records
	 BEGIN
	   INSERT INTO user_usage_totals (user_id, total_tokens)
	     SELECT NEW.user_id, NEW.total_tokens WHERE NEW.user_id <> ''
	   ON CONFLICT(user_id) DO UPDATE SET
	     total_tokens = total_tokens + excluded.total_tokens;
	 END`,
	usageTotalsInsertTriggerDDL,
	`CREATE TRIGGER trg_update_usage_totals_cost
	 AFTER UPDATE OF cost_total ON usage_records
	 WHEN OLD.cost_total <> NEW.cost_total
	 BEGIN
	   UPDATE usage_totals SET
	     cost_total = cost_total + (NEW.cost_total - OLD.cost_total)
	   WHERE id = 1;
	 END`,
}
