package store

import (
	"context"
	"database/sql"
	"time"
)

type Credential struct {
	ID            string
	ProviderID    string
	Label         string
	APIKeyEnc     []byte
	Enabled       bool
	Weight        int
	Status        string
	CooldownUntil int64
	LastError     string
	CreatedAt     int64
}

// credentialColumns 是 provider_credentials 两处读路径共用的列清单。
const credentialColumns = `id, provider_id, label, api_key_enc, enabled, weight, status, cooldown_until, last_error, created_at`

// scanCredential 用 sql.NullString 承接可空列 label / last_error，
// 否则列为 NULL 时扫描会直接报错。
func scanCredential(sc scanner) (Credential, error) {
	var c Credential
	var enabled int
	var label, lastError sql.NullString

	err := sc.Scan(&c.ID, &c.ProviderID, &label, &c.APIKeyEnc, &enabled,
		&c.Weight, &c.Status, &c.CooldownUntil, &lastError, &c.CreatedAt)
	if err != nil {
		return c, err
	}
	c.Enabled = enabled == 1
	c.Label = label.String
	c.LastError = lastError.String
	return c, nil
}

func (s *Store) ListCredentials(ctx context.Context, providerID string) ([]Credential, error) {
	rows, err := s.read.QueryContext(ctx,
		`SELECT `+credentialColumns+` FROM provider_credentials WHERE provider_id = ? ORDER BY created_at`, providerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make([]Credential, 0)
	for rows.Next() {
		c, err := scanCredential(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, c)
	}
	return result, rows.Err()
}

func (s *Store) GetCredential(ctx context.Context, id string) (*Credential, error) {
	c, err := scanCredential(s.read.QueryRowContext(ctx,
		`SELECT `+credentialColumns+` FROM provider_credentials WHERE id = ?`, id))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

func (s *Store) CreateCredential(ctx context.Context, c *Credential) error {
	return createCredential(ctx, s.db, c)
}

// createCredential 接受 execer，供事务内复用（见 CreateProviderWithCredential）。
func createCredential(ctx context.Context, ex execer, c *Credential) error {
	now := time.Now().UnixMilli()
	enabled := 0
	if c.Enabled {
		enabled = 1
	}
	_, err := ex.ExecContext(ctx,
		`INSERT INTO provider_credentials (id, provider_id, label, api_key_enc, enabled, weight, status, cooldown_until, last_error, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.ID, c.ProviderID, nullIfEmpty(c.Label), c.APIKeyEnc, enabled, c.Weight, c.Status, c.CooldownUntil, nullIfEmpty(c.LastError), now)
	// 回写时间戳，否则创建响应里的 created_at 是 0，与库里不一致。
	if err == nil {
		c.CreatedAt = now
	}
	return err
}

func (s *Store) UpdateCredential(ctx context.Context, id string, c *Credential) error {
	enabled := 0
	if c.Enabled {
		enabled = 1
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE provider_credentials SET label = ?, api_key_enc = ?, enabled = ?, weight = ?, status = ?, cooldown_until = ?, last_error = ? WHERE id = ?`,
		nullIfEmpty(c.Label), c.APIKeyEnc, enabled, c.Weight, c.Status, c.CooldownUntil, nullIfEmpty(c.LastError), id)
	return checkAffected(res, err)
}

// SetCredentialCooldown 只更新凭据的冷却状态与冷却截止时间。
//
// 与 UpdateCredential 分开的原因：冷却是**运行时状态**，由数据面在每次
// 上游 401/5xx 时写入；它必须落库，否则任何一次 admin 写操作触发的池重建
// （Install 重新 PrepareFromStore）都会把内存里的 CooldownUntil 读回成库里的
// 0，刚被判坏的 key 立刻复活。凭据 id 不存在时返回 ErrNotFound。
func (s *Store) SetCredentialCooldown(ctx context.Context, id, status string, until time.Time) error {
	var untilMs int64
	if !until.IsZero() {
		untilMs = until.UnixMilli()
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE provider_credentials SET status = ?, cooldown_until = ? WHERE id = ?`,
		status, untilMs, id)
	return checkAffected(res, err)
}

// DeleteCredential 删除一把凭据；id 不存在时返回 ErrNotFound。
func (s *Store) DeleteCredential(ctx context.Context, id string) error {
	return deleteByID(ctx, s.db, "provider_credentials", id)
}
