package store

import (
	"context"
	"database/sql"
	"time"
)

type Provider struct {
	ID         string
	Slug       string
	Name       string
	Protocol   string
	Endpoint   string
	Enabled    bool
	TimeoutMs  int
	MaxRetries int
	QuirksJSON string
	// ProxyURL 是该 provider 独享的出站代理。空串 = 不代理（直连或读
	// 进程环境变量）。见 store.go 里 proxy_url 列的注释。
	ProxyURL string
	// BalanceSpecJSON 是余额查询的**显式配置**，JSON 串，空串 = 不配
	// （回落到按端点特征的自动判定）。见 balance.go 的 parseBalanceSpec。
	//
	// 为什么是一整个 JSON 列而不是三个标量列（path / field / header）：
	// 余额响应各家的形状差异很大（字段名、嵌套层级、币种键名），三个标量
	// 迟早不够用，而每加一列就是一次 schema 迁移 + 三处读路径同步改。
	// 一列 JSON 让「新增一种表达」不碰 schema，且留空即「未配置」的语义
	// 与 proxy_url / quirks_json 一致。
	BalanceSpecJSON string
	CreatedAt       int64
	UpdatedAt       int64
}

// providerColumns 是 providers 三处读路径共用的列清单。
const providerColumns = `id, slug, name, protocol, endpoint, enabled, timeout_ms, max_retries, quirks_json, proxy_url, balance_spec_json, created_at, updated_at`

// scanProvider 用 sql.NullString 承接可空列 quirks_json，
// 否则该列为 NULL 时扫描会直接报错。
func scanProvider(sc scanner) (Provider, error) {
	var p Provider
	var enabled int
	var quirks sql.NullString
	var proxyURL sql.NullString
	var balanceSpec sql.NullString

	err := sc.Scan(&p.ID, &p.Slug, &p.Name, &p.Protocol, &p.Endpoint, &enabled,
		&p.TimeoutMs, &p.MaxRetries, &quirks, &proxyURL, &balanceSpec, &p.CreatedAt, &p.UpdatedAt)
	if err != nil {
		return p, err
	}
	p.Enabled = enabled == 1
	p.QuirksJSON = quirks.String
	p.ProxyURL = proxyURL.String
	p.BalanceSpecJSON = balanceSpec.String
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
		`INSERT INTO providers (id, slug, name, protocol, endpoint, enabled, timeout_ms, max_retries, quirks_json, proxy_url, balance_spec_json, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		p.ID, p.Slug, p.Name, p.Protocol, p.Endpoint, enabled, p.TimeoutMs, p.MaxRetries,
		nullIfEmpty(p.QuirksJSON), nullIfEmpty(p.ProxyURL), nullIfEmpty(p.BalanceSpecJSON), now, now)
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
		`UPDATE providers SET slug = ?, name = ?, protocol = ?, endpoint = ?, enabled = ?, timeout_ms = ?, max_retries = ?, quirks_json = ?, proxy_url = ?, balance_spec_json = ?, updated_at = ? WHERE id = ?`,
		p.Slug, p.Name, p.Protocol, p.Endpoint, enabled, p.TimeoutMs, p.MaxRetries,
		nullIfEmpty(p.QuirksJSON), nullIfEmpty(p.ProxyURL), nullIfEmpty(p.BalanceSpecJSON),
		time.Now().UnixMilli(), id)
	return checkAffected(res, err)
}

// DeleteProvider 删除一个上游；id 不存在时返回 ErrNotFound。
func (s *Store) DeleteProvider(ctx context.Context, id string) error {
	return deleteByID(ctx, s.db, "providers", id)
}
