package store

import (
	"context"
	"database/sql"
	"time"
)

type Provider struct {
	ID          string
	Slug        string
	Name        string
	Protocol    string
	Endpoint    string
	Enabled     bool
	TimeoutMs   int
	MaxRetries  int
	QuirksJSON  string
	CreatedAt   int64
	UpdatedAt   int64
}

// providerColumns 是 providers 三处读路径共用的列清单。
const providerColumns = `id, slug, name, protocol, endpoint, enabled, timeout_ms, max_retries, quirks_json, created_at, updated_at`

// scanProvider 用 sql.NullString 承接可空列 quirks_json，
// 否则该列为 NULL 时扫描会直接报错。
func scanProvider(sc scanner) (Provider, error) {
	var p Provider
	var enabled int
	var quirks sql.NullString

	err := sc.Scan(&p.ID, &p.Slug, &p.Name, &p.Protocol, &p.Endpoint, &enabled,
		&p.TimeoutMs, &p.MaxRetries, &quirks, &p.CreatedAt, &p.UpdatedAt)
	if err != nil {
		return p, err
	}
	p.Enabled = enabled == 1
	p.QuirksJSON = quirks.String
	return p, nil
}

func (s *Store) ListProviders(ctx context.Context) ([]Provider, error) {
	rows, err := s.read.QueryContext(ctx, `SELECT `+providerColumns+` FROM providers ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make([]Provider, 0)
	for rows.Next() {
		p, err := scanProvider(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, p)
	}
	return result, rows.Err()
}

func (s *Store) GetProvider(ctx context.Context, id string) (*Provider, error) {
	p, err := scanProvider(s.read.QueryRowContext(ctx, `SELECT `+providerColumns+` FROM providers WHERE id = ?`, id))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

func (s *Store) GetProviderBySlug(ctx context.Context, slug string) (*Provider, error) {
	p, err := scanProvider(s.read.QueryRowContext(ctx, `SELECT `+providerColumns+` FROM providers WHERE slug = ?`, slug))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

func (s *Store) CreateProvider(ctx context.Context, p *Provider) error {
	return createProvider(ctx, s.db, p)
}

// createProvider 接受 execer，供事务内复用（见 CreateProviderWithCredential）。
func createProvider(ctx context.Context, ex execer, p *Provider) error {
	now := time.Now().UnixMilli()
	enabled := 0
	if p.Enabled {
		enabled = 1
	}
	_, err := ex.ExecContext(ctx,
		`INSERT INTO providers (id, slug, name, protocol, endpoint, enabled, timeout_ms, max_retries, quirks_json, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		p.ID, p.Slug, p.Name, p.Protocol, p.Endpoint, enabled, p.TimeoutMs, p.MaxRetries, nullIfEmpty(p.QuirksJSON), now, now)
	// 回写时间戳，否则创建响应里的 created_at/updated_at 是 0，与库里不一致。
	if err == nil {
		p.CreatedAt, p.UpdatedAt = now, now
	}
	return err
}

func (s *Store) UpdateProvider(ctx context.Context, id string, p *Provider) error {
	enabled := 0
	if p.Enabled {
		enabled = 1
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE providers SET slug = ?, name = ?, protocol = ?, endpoint = ?, enabled = ?, timeout_ms = ?, max_retries = ?, quirks_json = ?, updated_at = ? WHERE id = ?`,
		p.Slug, p.Name, p.Protocol, p.Endpoint, enabled, p.TimeoutMs, p.MaxRetries, nullIfEmpty(p.QuirksJSON), time.Now().UnixMilli(), id)
	return checkAffected(res, err)
}

// DeleteProvider 删除一个上游；id 不存在时返回 ErrNotFound。
func (s *Store) DeleteProvider(ctx context.Context, id string) error {
	return deleteByID(ctx, s.db, "providers", id)
}
