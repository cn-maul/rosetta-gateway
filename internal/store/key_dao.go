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
	CreatedAt   int64
}

func (s *Store) ListAccessKeys(ctx context.Context) ([]AccessKey, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, key_hash, key_prefix, name, enabled, quota_tokens, used_tokens, created_at FROM access_keys ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make([]AccessKey, 0)
	for rows.Next() {
		var k AccessKey
		var enabled int
		if err := rows.Scan(&k.ID, &k.KeyHash, &k.KeyPrefix, &k.Name, &enabled, &k.QuotaTokens, &k.UsedTokens, &k.CreatedAt); err != nil {
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
	err := s.db.QueryRowContext(ctx,
		`SELECT id, key_hash, key_prefix, name, enabled, quota_tokens, used_tokens, created_at FROM access_keys WHERE id = ?`, id).
		Scan(&k.ID, &k.KeyHash, &k.KeyPrefix, &k.Name, &enabled, &k.QuotaTokens, &k.UsedTokens, &k.CreatedAt)
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
		`INSERT INTO access_keys (id, key_hash, key_prefix, name, enabled, quota_tokens, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		k.ID, k.KeyHash, k.KeyPrefix, k.Name, enabled, k.QuotaTokens, now)
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
	_, err := s.db.ExecContext(ctx,
		`UPDATE access_keys SET name = ?, enabled = ?, quota_tokens = ? WHERE id = ?`,
		k.Name, enabled, k.QuotaTokens, id)
	return err
}

// GetKeyQuota 读单个密钥的配额与已用量，供热路径预检。
// ok=false 表示密钥不存在（已删除）。used_tokens 由 usage_records 触发器实时累加，
// 故这里是权威值；配额预检直接查库而非读快照——快照只在管理写操作后重建，会严重滞后。
func (s *Store) GetKeyQuota(ctx context.Context, id string) (quota, used int64, ok bool, err error) {
	err = s.db.QueryRowContext(ctx,
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
