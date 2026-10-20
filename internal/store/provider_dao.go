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
	// NetworkID 是该 provider 走的出站网络（指向 networks.id）。
	// 空串 = 直连（等价于 store.NetworkDirectID）。
	//
	// 为什么不再直接存 URL：同一个代理会被多个上游共用，URL 各存一份
	// 意味着改一次要改 N 处，且手写容易打错。见 store.go 里 network_id
	// 列的注释。
	NetworkID string
	// ProxyURL 是**派生**值：NetworkID 指向的网络的 proxy_url，直连时为空串。
	// 它由读路径 JOIN 出来，供展示与池构建使用；不单独落库。
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
// proxy_expr 是把 network_id 解析成实际代理 URL 的表达式：LEFT JOIN 出
// networks.proxy_url，没有网络（直连）或网络已被删 → NULL → COALESCE 成空串
// （空串 = 不代理，读进程环境变量）。
//
// 为什么在读路径 JOIN 而不是把 URL 也冗余存一份：冗余列会被「改网络的
// 地址」与「换 provider 选的网络」两类写操作中的任何一处漏更新，而漏更新
// 的后果是静默走错代理 —— 比慢更难查。派生值没有这个问题。
const proxyExpr = `COALESCE(n.proxy_url, '')`

// 每列都写全表名：providerColumns 会被带 LEFT JOIN 的查询复用，而
// providers.id 与 networks.id 同名，不限定就是 "ambiguous column name: id"。
// 这是 JOIN 引入的唯一额外成本，比逐处加别名更不容易漏。
//
// 列顺序必须与 scanProvider 的 Scan 顺序**逐字对应**：派生出来的代理地址
// 在 providerColumns 之后、两个派生计数之前被扫 —— 那正是 providerSelect
// 里 proxy_expr 写在那里的原因，不是随手排的。
const providerColumns = `providers.id, providers.slug, providers.name, providers.protocol, providers.endpoint, providers.enabled, providers.timeout_ms, providers.max_retries, providers.quirks_json, providers.network_id, providers.balance_spec_json, providers.created_at, providers.updated_at`

// providerCounts 是两个派生计数（见 Provider.ModelCount 的注释）。
const providerCounts = `
	(SELECT COUNT(*) FROM upstream_models um WHERE um.provider_id = providers.id) AS model_count,
	(SELECT COUNT(*) FROM provider_credentials c WHERE c.provider_id = providers.id) AS cred_count`

// scanProvider 用 sql.NullString 承接可空列 quirks_json，
// 否则该列为 NULL 时扫描会直接报错。
func scanProvider(sc scanner) (Provider, error) {
	var p Provider
	var enabled int
	var quirks sql.NullString
	var networkID sql.NullString
	var proxyURL string
	var balanceSpec sql.NullString

	// 派生计数来自 COALESCE 出来的非空值，扫进 int 即可。
	err := sc.Scan(&p.ID, &p.Slug, &p.Name, &p.Protocol, &p.Endpoint, &enabled,
		&p.TimeoutMs, &p.MaxRetries, &quirks, &networkID, &balanceSpec, &p.CreatedAt, &p.UpdatedAt,
		&proxyURL,
		&p.ModelCount, &p.CredCount)
	if err != nil {
		return p, err
	}
	p.Enabled = enabled == 1
	p.QuirksJSON = quirks.String
	p.NetworkID = networkID.String
	p.ProxyURL = proxyURL
	p.BalanceSpecJSON = balanceSpec.String
	return p, nil
}

// providerSelect 是全部 provider 单条/列表读路径共用的 SELECT。
//
// LEFT JOIN networks 而不是子查询：一条 JOIN 就能同时拿到 network_id 与
// 派生的 proxy_url，且不改变结果集的行数（networks.id 是主键，不会放大）。
const providerSelect = `SELECT ` + providerColumns + `, ` + proxyExpr + `,` + providerCounts + `
	FROM providers
	LEFT JOIN networks n ON n.id = providers.network_id`

func (s *Store) ListProviders(ctx context.Context) ([]Provider, error) {
	rows, err := s.read.QueryContext(ctx, providerSelect+` ORDER BY providers.created_at`)
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
	p, err := scanProvider(s.read.QueryRowContext(ctx, providerSelect+` WHERE providers.id = ?`, id))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

func (s *Store) GetProviderBySlug(ctx context.Context, slug string) (*Provider, error) {
	p, err := scanProvider(s.read.QueryRowContext(ctx, providerSelect+` WHERE providers.slug = ?`, slug))
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

// CreateProviderTx 在一笔事务里创建上游，供批量导入整体回滚使用。
func (s *Store) CreateProviderTx(ctx context.Context, tx Tx, p *Provider) error {
	return createProvider(ctx, tx, p)
}

// createProvider 接受 execer，供事务内复用（见 CreateProviderWithCredential）。
func createProvider(ctx context.Context, ex execer, p *Provider) error {
	now := time.Now().UnixMilli()
	enabled := 0
	if p.Enabled {
		enabled = 1
	}
	_, err := ex.ExecContext(ctx,
		`INSERT INTO providers (id, slug, name, protocol, endpoint, enabled, timeout_ms, max_retries, quirks_json, network_id, balance_spec_json, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		p.ID, p.Slug, p.Name, p.Protocol, p.Endpoint, enabled, p.TimeoutMs, p.MaxRetries,
		nullIfEmpty(p.QuirksJSON), nullIfEmpty(normalizeNetworkID(p.NetworkID)), nullIfEmpty(p.BalanceSpecJSON), now, now)
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
		`UPDATE providers SET slug = ?, name = ?, protocol = ?, endpoint = ?, enabled = ?, timeout_ms = ?, max_retries = ?, quirks_json = ?, network_id = ?, balance_spec_json = ?, updated_at = ? WHERE id = ?`,
		p.Slug, p.Name, p.Protocol, p.Endpoint, enabled, p.TimeoutMs, p.MaxRetries,
		nullIfEmpty(p.QuirksJSON), nullIfEmpty(normalizeNetworkID(p.NetworkID)), nullIfEmpty(p.BalanceSpecJSON),
		time.Now().UnixMilli(), id)
	return checkAffected(res, err)
}

// DeleteProvider 删除一个上游；id 不存在时返回 ErrNotFound。
func (s *Store) DeleteProvider(ctx context.Context, id string) error {
	return deleteByID(ctx, s.db, "providers", id)
}
