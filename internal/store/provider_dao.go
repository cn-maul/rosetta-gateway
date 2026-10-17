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

	// ModelCount / CredCount 是**派生的统计值**，不在 providers 表里 ——
	// 它们的真源分别是 upstream_models 与 provider_credentials 的行数。
	// 只有 ListProviders 会填（见该函数的注释），单条读路径（GetProvider 等）
	// 一律留 0。
	ModelCount int
	CredCount  int
}

// providerColumns 是 providers 全部读路径共用的列清单，末尾两个是**派生
// 计数**（见 Provider.ModelCount 的注释）。
//
// 为什么用 LEFT JOIN 子查询而不是「先查 providers 再逐个数」：那是 N+1 ——
// 十几个上游就是十几次往返，而列表页每次刷新都要跑一遍。两个子查询各自
// 只扫一张表按 provider_id 过滤（UNIQUE(provider_id, model_id) 约束让
// SQLite 走索引前缀），成本远低于多一次网络往返。
//
// 为什么不做成 providers 上的物化列：它会被「导入模型 / 删除模型 /
// 加凭据 / 删凭据」四类写操作中的任何一处漏更新，而漏更新的后果是
// 静默显示错误数字 —— 那比慢更难查。
const providerColumns = `id, slug, name, protocol, endpoint, enabled, timeout_ms, max_retries, quirks_json, proxy_url, balance_spec_json, created_at, updated_at,
	(SELECT COUNT(*) FROM upstream_models um WHERE um.provider_id = providers.id) AS model_count,
	(SELECT COUNT(*) FROM provider_credentials c WHERE c.provider_id = providers.id) AS cred_count`

// scanProvider 用 sql.NullString 承接可空列 quirks_json，
// 否则该列为 NULL 时扫描会直接报错。
func scanProvider(sc scanner) (Provider, error) {
	var p Provider
	var enabled int
	var quirks sql.NullString
	var proxyURL sql.NullString
	var balanceSpec sql.NullString

	// 派生计数来自 COALESCE 出来的非空值，扫进 int 即可。
	err := sc.Scan(&p.ID, &p.Slug, &p.Name, &p.Protocol, &p.Endpoint, &enabled,
		&p.TimeoutMs, &p.MaxRetries, &quirks, &proxyURL, &balanceSpec, &p.CreatedAt, &p.UpdatedAt,
		&p.ModelCount, &p.CredCount)
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
