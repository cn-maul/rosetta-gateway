package store

import (
	"fmt"
)

// fixBalanceChargesPK 把 balance_charges 从「request_id 单列主键」重建为
// 「(request_id, user_id) 复合主键」。
//
// # 为什么必须重建而不是 ADD COLUMN
//
// SQLite 不能 ALTER 主键。ChargeBalance 靠
// `INSERT OR IGNORE INTO balance_charges (...)` + RowsAffected()==0 判幂等，
// 而「只扣一次」这个不变量完全由主键保证 —— 主键缺 user_id 时，两个不同用户
// 撞同一个 request_id，第二个人的 INSERT 会被静默跳过，扣费凭空消失
// （2026-10-10 修复的 P0）。request_id 来自客户端可控的请求头，
// 「不同用户 + 同一 request_id」并不是罕见场景。
//
// # 为什么不能像 ensureRollupDimensions 那样「表非空就拒绝启动」
//
// 那个函数敢那么做，是因为缺列的表是「schema 已落盘、逻辑还没接上」的过渡态，
// 从来没有行。而 balance_charges 里是**真金白银的扣费流水**：老库里每一行
// 都对应一次已经发生的扣费。把它们丢掉来换取一个干净的表结构，等于用历史账目
// 换 schema —— 方向完全错了。所以这里必须逐行搬运。
//
// # 搬运会不会丢行
//
// 旧主键是 request_id 单列，所以旧库里 (request_id) 天然唯一，搬运后
// (request_id, user_id) 只会被放宽、不会撞车 —— 不存在丢行。
// 全部包在一个事务里：中途失败则整体回滚，磁盘上的表保持原样。
func (s *Store) fixBalanceChargesPK() error {
	// 已迁移过的库跳过。判定方式不看表结构（那要多一次 pragma 查询且
	// 对复合主键没有单一列可查），而是查是否还存在「单列主键」。
	// sqlite 的 PRAGMA table_info 对复合主键会把两列都标成 pk=1，
	// 而单列主键表只有 request_id 一列 pk=1 —— 据此可精确区分。
	rows, err := s.db.Query(`PRAGMA table_info(balance_charges)`)
	if err != nil {
		return fmt.Errorf("inspect balance_charges schema: %w", err)
	}
	pkCols := map[string]bool{}
	for rows.Next() {
		var (
			cid       int
			name, typ string
			notNull   int
			dfltValue any
			pk        int
		)
		if err := rows.Scan(&cid, &name, &typ, &notNull, &dfltValue, &pk); err != nil {
			rows.Close()
			return fmt.Errorf("scan balance_charges column: %w", err)
		}
		if pk > 0 {
			pkCols[name] = true
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("read balance_charges columns: %w", err)
	}
	rows.Close()

	// 新库由 migrations 直接建成复合主键，两列都在 pk 里 → 无需重建。
	if len(pkCols) >= 2 {
		return nil
	}
	// 表不存在（理论上不可能：migrations 先建表）时不要动，避免建出一张空壳。
	if len(pkCols) == 0 {
		return nil
	}

	s.logger.Warn("rebuilding balance_charges: primary key must be (request_id, user_id); " +
		"single-column request_id silently cancels charges of colliding users (2026-10-10 P0)")

	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin balance_charges pk fix: %w", err)
	}
	defer func() { _ = tx.Rollback() }() //nolint:errcheck // 已提交时是 no-op

	for _, stmt := range []string{
		`CREATE TABLE balance_charges_new (
			request_id   TEXT NOT NULL,
			user_id      TEXT NOT NULL,
			amount_cents INTEGER NOT NULL,
			ts           INTEGER NOT NULL,
			PRIMARY KEY (request_id, user_id)
		)`,
		`INSERT INTO balance_charges_new (request_id, user_id, amount_cents, ts)
			SELECT request_id, user_id, amount_cents, ts FROM balance_charges`,
		`DROP TABLE balance_charges`,
		`ALTER TABLE balance_charges_new RENAME TO balance_charges`,
		// DROP TABLE 会连带删掉索引，必须逐条重造（与 ensureRollupDimensions 同理）。
		`CREATE INDEX IF NOT EXISTS idx_balance_charges_user ON balance_charges(user_id, ts)`,
	} {
		if _, err := tx.Exec(stmt); err != nil {
			return fmt.Errorf("balance_charges pk fix (%s): %w", firstLine(stmt), err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit balance_charges pk fix: %w", err)
	}
	s.logger.Info("balance_charges primary key is now (request_id, user_id)")
	return nil
}

// firstLine 取 SQL 的第一行，用于错误信息里指明是哪一步失败。
func firstLine(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			return s[:i]
		}
	}
	return s
}
