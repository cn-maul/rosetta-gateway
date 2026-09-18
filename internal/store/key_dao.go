package store

import (
	"context"
	"database/sql"
	"time"
)

type AccessKey struct {
	ID           string
	KeyHash      string
	KeyPrefix    string
	Name         string
	Enabled      bool
	ExpiresAt    int64
	QuotaTokens  int64
	UsedTokens   int64
	RPMLimit     int
	TPMLimit     int
	CreatedAt    int64
	LastUsedAt   int64
}

func (s *Store) ListAccessKeys(ctx context.Context) ([]AccessKey, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, key_hash, key_prefix, name, enabled, expires_at, quota_tokens, used_tokens, rpm_limit, tpm_limit, created_at, last_used_at FROM access_keys ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make([]AccessKey, 0)
	for rows.Next() {
		var k AccessKey
		var enabled int
		if err := rows.Scan(&k.ID, &k.KeyHash, &k.KeyPrefix, &k.Name, &enabled, &k.ExpiresAt, &k.QuotaTokens, &k.UsedTokens, &k.RPMLimit, &k.TPMLimit, &k.CreatedAt, &k.LastUsedAt); err != nil {
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
		`SELECT id, key_hash, key_prefix, name, enabled, expires_at, quota_tokens, used_tokens, rpm_limit, tpm_limit, created_at, last_used_at FROM access_keys WHERE id = ?`, id).
		Scan(&k.ID, &k.KeyHash, &k.KeyPrefix, &k.Name, &enabled, &k.ExpiresAt, &k.QuotaTokens, &k.UsedTokens, &k.RPMLimit, &k.TPMLimit, &k.CreatedAt, &k.LastUsedAt)
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
		`INSERT INTO access_keys (id, key_hash, key_prefix, name, enabled, expires_at, quota_tokens, used_tokens, rpm_limit, tpm_limit, created_at, last_used_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		k.ID, k.KeyHash, k.KeyPrefix, k.Name, enabled, k.ExpiresAt, k.QuotaTokens, k.UsedTokens, k.RPMLimit, k.TPMLimit, now, 0)
	return err
}

func (s *Store) UpdateAccessKey(ctx context.Context, id string, k *AccessKey) error {
	enabled := 0
	if k.Enabled {
		enabled = 1
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE access_keys SET name = ?, enabled = ?, expires_at = ?, quota_tokens = ?, rpm_limit = ?, tpm_limit = ? WHERE id = ?`,
		k.Name, enabled, k.ExpiresAt, k.QuotaTokens, k.RPMLimit, k.TPMLimit, id)
	return err
}

func (s *Store) DeleteAccessKey(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM access_keys WHERE id = ?`, id)
	return err
}

func (s *Store) AddUsageTokens(ctx context.Context, keyID string, tokens int64) error {
	now := time.Now().UnixMilli()
	_, err := s.db.ExecContext(ctx,
		`UPDATE access_keys SET used_tokens = used_tokens + ?, last_used_at = ? WHERE id = ?`,
		tokens, now, keyID)
	return err
}
