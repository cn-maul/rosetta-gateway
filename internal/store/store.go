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

	// _busy_timeout：写锁被占用时等最多 5 秒再放弃，而不是立刻抛 SQLITE_BUSY。
	// 单进程内 SetMaxOpenConns(1) 已经避免了自争抢，但备份脚本、CLI 工具、
	// 误起的第二个实例都会以独立连接打开同一个文件 —— 那种情况下没有它，
	// 一次写撞锁就直接失败，而配额预检是 fail-open 的，会静默放行超额请求。
	// _synchronous=NORMAL 是 WAL 模式的官方推荐搭配：WAL 下 FULL 只多保护
	// 「掉电丢最近几个已提交事务」这一种情形（不损坏），代价是每次 commit 都
	// fsync —— 本表的用量 INSERT 每请求一条，NORMAL 省掉这笔开销且无完整性风险。
	db, err := sql.Open("sqlite", dbPath+"?_journal_mode=WAL&_synchronous=NORMAL&_foreign_keys=ON&_busy_timeout=5000")
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
			price_input       REAL,
			price_cache_hit   REAL,
			price_output      REAL,
			UNIQUE (provider_id, model_id)
		)`,
		`CREATE TABLE IF NOT EXISTS routes (
			id                TEXT PRIMARY KEY,
			public_name       TEXT NOT NULL UNIQUE,
			provider_id       TEXT NOT NULL REFERENCES providers(id) ON DELETE RESTRICT,
			upstream_model_id TEXT NOT NULL REFERENCES upstream_models(id) ON DELETE RESTRICT,
			enabled           INTEGER NOT NULL DEFAULT 1,
			failover_enabled  INTEGER NOT NULL DEFAULT 0,
			created_at        INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS access_keys (
			id            TEXT PRIMARY KEY,
			key_hash      TEXT NOT NULL UNIQUE,
			key_prefix    TEXT NOT NULL,
			name          TEXT NOT NULL,
			enabled       INTEGER NOT NULL DEFAULT 1,
			quota_tokens  INTEGER NOT NULL DEFAULT 0,
			used_tokens   INTEGER NOT NULL DEFAULT 0,
			created_at    INTEGER NOT NULL
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
		// route_targets：一条 route 后面的有序上游链。
		// routes.provider_id / upstream_model_id 是「主目标」（position 0）的向后兼容列，
		// 真正的故障转移按本表 position 升序逐个尝试（见 DESIGN §10 的 P2 语义）。
		`CREATE TABLE IF NOT EXISTS route_targets (
			id                TEXT PRIMARY KEY,
			route_id          TEXT NOT NULL REFERENCES routes(id) ON DELETE CASCADE,
			provider_id       TEXT NOT NULL REFERENCES providers(id) ON DELETE RESTRICT,
			upstream_model_id TEXT NOT NULL REFERENCES upstream_models(id) ON DELETE RESTRICT,
			position          INTEGER NOT NULL DEFAULT 0,
			enabled           INTEGER NOT NULL DEFAULT 1,
			created_at        INTEGER NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_route_targets_route ON route_targets(route_id, position)`,
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

	if err := s.ensureColumns(); err != nil {
		return err
	}
	if err := s.dropDeadColumns(); err != nil {
		return err
	}
	if err := s.backfillRouteTargets(); err != nil {
		return err
	}

	s.logger.Info("database migrations completed")
	return nil
}

// dropDeadColumns 删除「全仓从来没有读写路径」的列。
//
//   - access_keys 的 expires_at / rpm_limit / tpm_limit / last_used_at 是建表时就
//     写进去的占位能力：没有任何一处读或写它们。
//   - routes.priority 更糟 —— 它在界面上可编辑、文案写着「按 priority 升序择优」，
//     但 runtime 完全不读它，且 public_name 带 UNIQUE 约束使「同名路由择优」
//     在数据层就不可能存在（同名根本插不进去）。真正需要「一个公开模型名后面
//     挂多个上游」的场景由 route_targets 链承担，那是另一套（有序、可故障转移）机制。
//   - routes.extra_json / upstream_models.default_extra_json 存了但没有任何地方拿它
//     构造请求（`ApplyProtocolPrivateExtra` 只管客户端带的协议私有字段）。要接线得先定清
//     语义：链上各目标的协议可能不同，「保留键」校验没法在写入时一次做完；还要定
//     路由级与模型级同时存在时谁覆盖谁。那是功能设计，不是修 bug —— 没有设计就接线，
//     只会把「静默不生效」换成「静默生效但语义不明」。
//   - routes.fallback_route_id 是**已被 route_targets 有序链取代**的单跳兜底
//     （DESIGN §10 明确写了取代关系）。留着它会与链形成两套并行的兜底机制，
//     且界面上没有入口（API 却能写），是典型的「隐形配置」。
//   - routes.max_targets / failure_threshold / stream_first_token_timeout_ms /
//     nonstream_timeout_ms 是原先的**按路由**故障转移策略覆盖。策略参数已统一收进
//     「设置」页（app_settings 的 runtime_defaults），不再按 route 存 ——
//     否则会出现「全局设置了、某条路由却被旧覆盖值悄悄盖住」的排查地狱。
//
// 留着它们的代价不是磁盘，而是 **schema 与界面在说谎**：运维照着「0 = 不过期」
// 「按优先级择优」去配，配完发现毫无效果，只能翻源码才发现没接线。
// 真要实现时用 ensureColumns 加回来即可（ADD COLUMN 比 DROP 简单得多）。
//
// DROP COLUMN 不幂等（列不存在会报错），所以先查 table_info。
func (s *Store) dropDeadColumns() error {
	for _, d := range []struct{ table, column string }{
		{"access_keys", "expires_at"},
		{"access_keys", "rpm_limit"},
		{"access_keys", "tpm_limit"},
		{"access_keys", "last_used_at"},
		{"routes", "priority"},
		{"routes", "extra_json"},
		{"routes", "fallback_route_id"},
		{"routes", "max_targets"},
		{"routes", "failure_threshold"},
		{"routes", "stream_first_token_timeout_ms"},
		{"routes", "nonstream_timeout_ms"},
		{"upstream_models", "default_extra_json"},
	} {
		has, err := s.columnExists(d.table, d.column)
		if err != nil {
			return fmt.Errorf("check column %s.%s: %w", d.table, d.column, err)
		}
		if !has {
			continue
		}
		if _, err := s.db.Exec(fmt.Sprintf(`ALTER TABLE %s DROP COLUMN %s`, d.table, d.column)); err != nil {
			// 删列失败不能挡住启动：这些列已经没有任何读写路径，留着只是冗余；
			// 而启动失败会让整个网关不可用（代价完全不成比例）。记 WARN 继续。
			s.logger.Warn("failed to drop obsolete column (ignored)",
				"table", d.table, "column", d.column, "error", err)
		}
	}
	return nil
}

// ensureColumns 为已存在的表补齐新增列。
//
// 为什么单独走一遍而不是塞进 migrations 列表：SQLite 的 ALTER TABLE ADD COLUMN
// 不幂等（列已存在会报错），而本函数在每次 Open 时都会执行。先查 table_info，
// 缺哪列补哪列，老库升级与新库首建都覆盖到。
func (s *Store) ensureColumns() error {
	type col struct {
		table string
		name  string
		def   string // ADD COLUMN 后面的类型与默认值片段
	}
	additions := []col{
		{"routes", "failover_enabled", "INTEGER NOT NULL DEFAULT 0"},
		// 模型单价（元 / 百万 tokens），可空：NULL = 未配置价格，统计费用按 0 计。
		// price_input 是「缓存未命中输入」单价；命中的输入另按 price_cache_hit 计
		// （为 0 时回退到 price_input，见 usage_dao 的费用口径）。
		{"upstream_models", "price_input", "REAL"},
		{"upstream_models", "price_cache_hit", "REAL"},
		{"upstream_models", "price_output", "REAL"},
	}

	for _, a := range additions {
		has, err := s.columnExists(a.table, a.name)
		if err != nil {
			return fmt.Errorf("check column %s.%s: %w", a.table, a.name, err)
		}
		if has {
			continue
		}
		if _, err := s.db.Exec(fmt.Sprintf(`ALTER TABLE %s ADD COLUMN %s %s`, a.table, a.name, a.def)); err != nil {
			return fmt.Errorf("add column %s.%s: %w", a.table, a.name, err)
		}
	}
	return nil
}

// columnExists 用 PRAGMA table_info 判断列是否存在。
// 表名/列名来自上面的常量白名单，不做参数化也不接外部输入，无注入面。
func (s *Store) columnExists(table, column string) (bool, error) {
	rows, err := s.db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			cid   int
			name  string
			ctype string
			nn    int
			dflt  sql.NullString
			pk    int
		)
		if err := rows.Scan(&cid, &name, &ctype, &nn, &dflt, &pk); err != nil {
			return false, err
		}
		if name == column {
			return true, nil
		}
	}
	return false, rows.Err()
}

// backfillRouteTargets 为「还没有任何目标行」的老 route 补一条 position 0 的目标，
// 取该 route 自身的 (provider_id, upstream_model_id)。NOT EXISTS 守卫使其幂等：
// 已经通过后台建过目标链的 route 不会被重复插入。
func (s *Store) backfillRouteTargets() error {
	_, err := s.db.Exec(
		`INSERT INTO route_targets (id, route_id, provider_id, upstream_model_id, position, enabled, created_at)
		 SELECT r.id || '#t0', r.id, r.provider_id, r.upstream_model_id, 0, r.enabled, r.created_at
		 FROM routes r
		 WHERE NOT EXISTS (SELECT 1 FROM route_targets t WHERE t.route_id = r.id)`)
	if err != nil {
		return fmt.Errorf("backfill route_targets: %w", err)
	}
	return nil
}
