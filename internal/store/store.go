package store

import (
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

type Store struct {
	db     *sql.DB
	logger *slog.Logger
}

func Open(dbPath string, logger *slog.Logger) (*Store, error) {
	dir := filepath.Dir(dbPath)
	if dir != "" {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return nil, fmt.Errorf("create db dir: %w", err)
		}
	}

	db, err := sql.Open("sqlite", dbPath+"?_journal_mode=WAL&_foreign_keys=ON")
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}

	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	s := &Store{db: db, logger: logger}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}

	return s, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) DB() *sql.DB {
	return s.db
}

func (s *Store) migrate() error {
	migrations := []string{
		`CREATE TABLE IF NOT EXISTS providers (
			id            TEXT PRIMARY KEY,
			slug          TEXT NOT NULL UNIQUE,
			name          TEXT NOT NULL,
			protocol      TEXT NOT NULL DEFAULT 'auto',
			endpoint      TEXT NOT NULL,
			enabled       INTEGER NOT NULL DEFAULT 1,
			timeout_ms    INTEGER NOT NULL DEFAULT 0,
			max_retries   INTEGER NOT NULL DEFAULT 2,
			quirks_json   TEXT,
			created_at    INTEGER NOT NULL,
			updated_at    INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS provider_credentials (
			id            TEXT PRIMARY KEY,
			provider_id   TEXT NOT NULL REFERENCES providers(id) ON DELETE CASCADE,
			label         TEXT,
			api_key_enc   BLOB NOT NULL,
			enabled       INTEGER NOT NULL DEFAULT 1,
			weight        INTEGER NOT NULL DEFAULT 1,
			status        TEXT NOT NULL DEFAULT 'healthy',
			cooldown_until INTEGER NOT NULL DEFAULT 0,
			last_error    TEXT,
			created_at    INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS upstream_models (
			id                TEXT PRIMARY KEY,
			provider_id       TEXT NOT NULL REFERENCES providers(id) ON DELETE CASCADE,
			model_id          TEXT NOT NULL,
			display_name      TEXT,
			enabled           INTEGER NOT NULL DEFAULT 1,
			context_window    INTEGER,
			max_output_tokens INTEGER,
			supports_thinking INTEGER,
			default_extra_json TEXT,
			UNIQUE (provider_id, model_id)
		)`,
		`CREATE TABLE IF NOT EXISTS routes (
			id                TEXT PRIMARY KEY,
			public_name       TEXT NOT NULL UNIQUE,
			provider_id       TEXT NOT NULL REFERENCES providers(id) ON DELETE RESTRICT,
			upstream_model_id TEXT NOT NULL REFERENCES upstream_models(id) ON DELETE RESTRICT,
			enabled           INTEGER NOT NULL DEFAULT 1,
			priority          INTEGER NOT NULL DEFAULT 0,
			fallback_route_id TEXT REFERENCES routes(id) ON DELETE SET NULL,
			extra_json        TEXT,
			created_at        INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS access_keys (
			id            TEXT PRIMARY KEY,
			key_hash      TEXT NOT NULL UNIQUE,
			key_prefix    TEXT NOT NULL,
			name          TEXT NOT NULL,
			enabled       INTEGER NOT NULL DEFAULT 1,
			expires_at    INTEGER NOT NULL DEFAULT 0,
			quota_tokens  INTEGER NOT NULL DEFAULT 0,
			used_tokens   INTEGER NOT NULL DEFAULT 0,
			rpm_limit     INTEGER NOT NULL DEFAULT 0,
			tpm_limit     INTEGER NOT NULL DEFAULT 0,
			created_at    INTEGER NOT NULL,
			last_used_at  INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE TABLE IF NOT EXISTS usage_records (
			id                TEXT PRIMARY KEY,
			ts                INTEGER NOT NULL,
			access_key_id     TEXT NOT NULL,
			public_model      TEXT NOT NULL,
			provider_id       TEXT NOT NULL,
			upstream_model    TEXT NOT NULL,
			ingress_protocol  TEXT NOT NULL,
			stream            INTEGER NOT NULL,
			input_tokens      INTEGER NOT NULL DEFAULT 0,
			output_tokens     INTEGER NOT NULL DEFAULT 0,
			total_tokens      INTEGER NOT NULL DEFAULT 0,
			reasoning_tokens  INTEGER NOT NULL DEFAULT 0,
			cached_tokens     INTEGER NOT NULL DEFAULT 0,
			usage_state       TEXT NOT NULL,
			status            TEXT NOT NULL,
			http_status       INTEGER NOT NULL,
			error_code        TEXT,
			latency_ms        INTEGER NOT NULL,
			ttfb_ms           INTEGER NOT NULL DEFAULT 0,
			request_id        TEXT
		)`,
		`CREATE INDEX IF NOT EXISTS idx_usage_ts ON usage_records(ts)`,
		`CREATE INDEX IF NOT EXISTS idx_usage_key_ts ON usage_records(access_key_id, ts)`,
		`CREATE INDEX IF NOT EXISTS idx_usage_model_ts ON usage_records(public_model, ts)`,
		`CREATE INDEX IF NOT EXISTS idx_usage_prov_ts ON usage_records(provider_id, ts)`,
		`CREATE TABLE IF NOT EXISTS app_settings (
			key         TEXT PRIMARY KEY,
			value       TEXT NOT NULL,
			updated_at  INTEGER NOT NULL
		)`,
		`CREATE TRIGGER IF NOT EXISTS trg_update_used_tokens
		 AFTER INSERT ON usage_records
		 BEGIN
		   UPDATE access_keys SET used_tokens = used_tokens + NEW.total_tokens WHERE id = NEW.access_key_id;
		 END`,
	}

	for i, m := range migrations {
		if _, err := s.db.Exec(m); err != nil {
			return fmt.Errorf("migration %d: %w", i, err)
		}
	}

	s.logger.Info("database migrations completed")
	return nil
}
