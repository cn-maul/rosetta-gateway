package store

import (
	"context"
	"database/sql"
	"time"
)

type AccessKey struct {
	ID          string
	KeyHash     string
	KeyPrefix   string
	Name        string
	Enabled     bool
	QuotaTokens int64
	UsedTokens  int64
	// RPMLimit / TPMLimit 是 Key 维度的每分钟限速（DESIGN §11.4），0 = 不限。
	RPMLimit  int
	TPMLimit  int
	CreatedAt int64
}

func (s *Store) ListAccessKeys(ctx context.Context) ([]AccessKey, error) {
	rows, err := s.read.QueryContext(ctx,
		`SELECT id, key_hash, key_prefix, name, enabled, quota_tokens, used_tokens, rpm_limit, tpm_limit, created_at FROM access_keys ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make([]AccessKey, 0)
	for rows.Next() {
		var k AccessKey
		var enabled int
		if err := rows.Scan(&k.ID, &k.KeyHash, &k.KeyPrefix, &k.Name, &enabled, &k.QuotaTokens, &k.UsedTokens, &k.RPMLimit, &k.TPMLimit, &k.CreatedAt); err != nil {
			return nil, err
		}
		k.Enabled = enabled == 1
		result = append(result, k)
	}
	return result, rows.Err()
}

func (s *Store) GetAccessKey(ctx context.Context, id string) (*AccessKey, error) {
	var k AccessKey
	var enabled int
	err := s.read.QueryRowContext(ctx,
		`SELECT id, key_hash, key_prefix, name, enabled, quota_tokens, used_tokens, rpm_limit, tpm_limit, created_at FROM access_keys WHERE id = ?`, id).
		Scan(&k.ID, &k.KeyHash, &k.KeyPrefix, &k.Name, &enabled, &k.QuotaTokens, &k.UsedTokens, &k.RPMLimit, &k.TPMLimit, &k.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	k.Enabled = enabled == 1
	return &k, nil
}

func (s *Store) CreateAccessKey(ctx context.Context, k *AccessKey) error {
	now := time.Now().UnixMilli()
	enabled := 0
	if k.Enabled {
		enabled = 1
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO access_keys (id, key_hash, key_prefix, name, enabled, quota_tokens, rpm_limit, tpm_limit, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		k.ID, k.KeyHash, k.KeyPrefix, k.Name, enabled, k.QuotaTokens, k.RPMLimit, k.TPMLimit, now)
	// 回写时间戳：落库用的是局部变量 now，结构体仍是零值 → 创建响应里的
	// created_at 会是 0，与随后 GET 到的同一条记录不一致（前端会显示「建于 1970」）。
	if err == nil {
		k.CreatedAt = now
	}
	return err
}

func (s *Store) UpdateAccessKey(ctx context.Context, id string, k *AccessKey) error {
	enabled := 0
	if k.Enabled {
		enabled = 1
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE access_keys SET name = ?, enabled = ?, quota_tokens = ?, rpm_limit = ?, tpm_limit = ? WHERE id = ?`,
		k.Name, enabled, k.QuotaTokens, k.RPMLimit, k.TPMLimit, id)
	return checkAffected(res, err)
}

// GetKeyQuota 读单个密钥的配额与已用量，供热路径预检。
// ok=false 表示密钥不存在（已删除）。used_tokens 由 usage_records 触发器实时累加，
// 故这里是权威值；配额预检直接查库而非读快照——快照只在管理写操作后重建，会严重滞后。
func (s *Store) GetKeyQuota(ctx context.Context, id string) (quota, used int64, ok bool, err error) {
	err = s.read.QueryRowContext(ctx,
		`SELECT quota_tokens, used_tokens FROM access_keys WHERE id = ?`, id).
		Scan(&quota, &used)
	if err == sql.ErrNoRows {
		return 0, 0, false, nil
	}
	if err != nil {
		return 0, 0, false, err
	}
	return quota, used, true, nil
}

// DeleteAccessKey 删除一把访问密钥；id 不存在时返回 ErrNotFound。
func (s *Store) DeleteAccessKey(ctx context.Context, id string) error {
	return deleteByID(ctx, s.db, "access_keys", id)
}

// RecomputeUsedTokens 从 usage_records 重新计算某把密钥的已用量并写回
// access_keys.used_tokens，返回修正后的值。
//
// 为什么需要它：used_tokens 由 INSERT 触发器单调累加（store.go 的
// trg_update_used_tokens），**没有任何回退或重算路径**。一次误写、一次手工
// 插记录、或一次 bug，都会让它永久偏高 —— 而配额预检（GetKeyQuota 的
// used >= quota）会从此对这把 key 恒返回 429，且管理 API 里没有修正入口，
// 唯一的办法是直接改库。这是「触发器维护的派生值」的固有代价，
// 补一个显式重算把恢复能力还给运维。
func (s *Store) RecomputeUsedTokens(ctx context.Context, keyID string) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	var used int64
	// 只有计入配额的记录才应计入 used_tokens。当前 total_tokens 即计费口径，
	// 故直接求和；status 不筛 —— canceled 的请求同样消耗了上游配额。
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(total_tokens), 0) FROM usage_records WHERE access_key_id = ?`,
		keyID).Scan(&used); err != nil {
		return 0, err
	}
	res, err := tx.ExecContext(ctx,
		`UPDATE access_keys SET used_tokens = ? WHERE id = ?`, used, keyID)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		// 老版本 SQLite 驱动可能不支持；不因此把成功变成失败。
		s.logger.Debug("recompute used_tokens: RowsAffected unavailable", "error", err)
	} else if n == 0 {
		return 0, ErrNotFound
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return used, nil
}
