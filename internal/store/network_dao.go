package store

import (
	"context"
	crand "crypto/rand"
	"database/sql"
	"encoding/hex"
	"strings"
	"time"
)

// NetworkDirectID 是「直连」这个网络选项的保留 id。
//
// # 为什么直连不是 networks 里的一行，而是一个保留 id
//
// 直连没有 proxy_url（它的语义是「不设代理」，由 network_dao 里
// proxyHTTPClientFor 读到空串后回落到共享 client / 进程环境变量）。
// 把它写成一行 proxy_url=” 的话，那一行永远不能删 —— 删了所有「没选
// 网络」的上游会因为 FK 失败被卡住，而它的存在又与「NULL / 空串 = 直连」
// 这条纪律重复，两种表示同一个意思必然出现「库里两个都指直连」的状态。
//
// 保留 id 的代价是它永远不会出现在 ListNetworks 的结果里（那不是一行），
// 而由前端作为下拉的第一项硬编码展示。这样「直连」只有一个表示，
// 且删网络永远不会波及任何 provider。
const NetworkDirectID = "direct"

// Network 是一条可复用的出站网络（代理配置）。
type Network struct {
	ID   string
	Name string
	// ProxyURL 是该网络的代理地址（http/https/socks5/socks5h）。
	// 新建/编辑网络时**不允许为空** —— 一个没有地址的「网络」在语义上
	// 就是直连，而直连由 NetworkDirectID 表示，不需要单独建一行。
	ProxyURL  string
	CreatedAt int64

	// ProviderCount 是引用该网络的上游数（派生值，不在 networks 表里）。
	// 列表页要展示「删掉它会影响几个上游」，前端逐个查是 N+1。
	ProviderCount int
}

// listNetworks 返回全部网络（不含直连那个保留 id）。
func (s *Store) ListNetworks(ctx context.Context) ([]Network, error) {
	rows, err := s.read.QueryContext(ctx, `
		SELECT n.id, n.name, n.proxy_url, n.created_at,
			(SELECT COUNT(*) FROM providers p WHERE p.network_id = n.id) AS provider_count
		FROM networks n
		ORDER BY n.created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make([]Network, 0)
	for rows.Next() {
		var n Network
		if err := rows.Scan(&n.ID, &n.Name, &n.ProxyURL, &n.CreatedAt, &n.ProviderCount); err != nil {
			return nil, err
		}
		result = append(result, n)
	}
	return result, rows.Err()
}

// GetNetwork 按 id 取一条网络；不存在时返回 nil（与 GetProvider 的约定一致）。
func (s *Store) GetNetwork(ctx context.Context, id string) (*Network, error) {
	var n Network
	err := s.read.QueryRowContext(ctx, `SELECT id, name, proxy_url, created_at FROM networks WHERE id = ?`, id).
		Scan(&n.ID, &n.Name, &n.ProxyURL, &n.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &n, nil
}

func (s *Store) CreateNetwork(ctx context.Context, n *Network) error {
	n.ProxyURL = strings.TrimSpace(n.ProxyURL)
	now := time.Now().UnixMilli()
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO networks (id, name, proxy_url, created_at) VALUES (?, ?, ?, ?)`,
		n.ID, n.Name, n.ProxyURL, now); err != nil {
		return err
	}
	n.CreatedAt = now
	return nil
}

func (s *Store) UpdateNetwork(ctx context.Context, id, name, proxyURL string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE networks SET name = ?, proxy_url = ? WHERE id = ?`,
		name, strings.TrimSpace(proxyURL), id)
	return checkAffected(res, err)
}

// DeleteNetwork 删除一个网络。引用它的上游由 FK 的 ON DELETE SET NULL
// 自动回到直连 —— 不级联删上游。
func (s *Store) DeleteNetwork(ctx context.Context, id string) error {
	return deleteByID(ctx, s.db, "networks", id)
}

// NetworkProxyURLs 返回「网络 id → 代理 URL」的映射，供池构建时把
// provider.network_id 解析成实际要用的 URL。
//
// 一次查询而不是每个 provider 现取：池重建是一次性动作，但每个 provider
// 一次往返在十几条上游时就是十几次往返，而这张表通常只有 1~3 行。
func (s *Store) NetworkProxyURLs(ctx context.Context) (map[string]string, error) {
	rows, err := s.read.QueryContext(ctx, `SELECT id, proxy_url FROM networks`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]string)
	for rows.Next() {
		var id, proxyURL string
		if err := rows.Scan(&id, &proxyURL); err != nil {
			return nil, err
		}
		out[id] = proxyURL
	}
	return out, rows.Err()
}

// ProviderProxyURL 是池构建需要的「一个 provider 实际走哪个 URL」。
type ProviderProxyURL struct {
	ProviderID string
	ProxyURL   string
}

// normalizeNetworkID 把「直连」的两种写法（空串与保留 id）统一成空串，
// 即库里永远只有一种表示 —— NULL。
//
// 为什么在写边界归一而不是让每个调用方自觉：NetworkDirectID 是给前端与
// 导入文件用的**对外**取值，它必须在库里落成 NULL，否则「直连」会有两种
// 行内表示，而读路径只认 NULL —— 于是「选了直连但读出来 network_id 非空」
// 这类不一致会变成静默 bug 的温床。
func normalizeNetworkID(id string) string {
	if strings.TrimSpace(id) == NetworkDirectID {
		return ""
	}
	return id
}

// ProviderProxyURLs 一次查出全部 provider 的出站 URL，把 network_id 解析
// 成 networks.proxy_url（没有网络或网络已被删 → 空串 = 直连）。
func (s *Store) ProviderProxyURLs(ctx context.Context) ([]ProviderProxyURL, error) {
	rows, err := s.read.QueryContext(ctx, `
		SELECT p.id, COALESCE(n.proxy_url, '')
		FROM providers p
		LEFT JOIN networks n ON n.id = p.network_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]ProviderProxyURL, 0)
	for rows.Next() {
		var e ProviderProxyURL
		if err := rows.Scan(&e.ProviderID, &e.ProxyURL); err != nil {
			return nil, err
		}
		result = append(result, e)
	}
	return result, rows.Err()
}

// backfillProxyNetworks 把老库里 providers.proxy_url 的非空值迁移到 networks。
//
// 一次性、幂等：迁移完成后 providers.proxy_url 列被 drop（见 dropDeadColumns），
// 所以第二次运行时该列已不存在，此时直接返回。
//
// 迁移策略是「按 URL 归并成一行」：多个 provider 手填了同一个代理地址时，
// 它们本来就该共用一个网络 —— 这正是本次改造想达成的形态，自动归并等于
// 把历史配置顺手整理好。
func (s *Store) backfillProxyNetworks(ctx context.Context) error {
	has, err := s.columnExists("providers", "proxy_url")
	if err != nil {
		return err
	}
	if !has {
		return nil
	}
	rows, err := s.read.QueryContext(ctx,
		`SELECT DISTINCT TRIM(proxy_url) FROM providers
		 WHERE proxy_url IS NOT NULL AND TRIM(proxy_url) <> ''`)
	if err != nil {
		return err
	}
	var urls []string
	for rows.Next() {
		var u string
		if err := rows.Scan(&u); err != nil {
			rows.Close()
			return err
		}
		urls = append(urls, u)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	for _, u := range urls {
		if n, err := s.findNetworkByProxyURL(ctx, u); err != nil {
			return err
		} else if n != nil {
			continue
		}
		// 网络名用 URL 本身兜底：老库里没有名字可继承，而「手填的代理地址」
		// 是一个诚实且唯一的默认展示名。之后管理员可以在界面上改名。
		if err := s.CreateNetwork(ctx, &Network{ID: newNetworkID(), Name: u, ProxyURL: u}); err != nil {
			// UNIQUE(name) 冲突说明并发启动时另一个实例已经建好了同一行。
			// 那种情况下直接用已存在的那行，不是错误。
			if !IsUniqueViolation(err) {
				return err
			}
		}
	}

	// 回填 network_id。每个 provider 单独按 URL 匹配，避免「同 URL 多行」。
	for _, u := range urls {
		n, err := s.findNetworkByProxyURL(ctx, u)
		if err != nil {
			return err
		}
		if n == nil {
			continue
		}
		if _, err := s.db.ExecContext(ctx,
			`UPDATE providers SET network_id = ? WHERE proxy_url = ?`, n.ID, u); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) findNetworkByProxyURL(ctx context.Context, proxyURL string) (*Network, error) {
	var n Network
	err := s.read.QueryRowContext(ctx, `SELECT id, name, proxy_url, created_at FROM networks WHERE proxy_url = ?`, proxyURL).
		Scan(&n.ID, &n.Name, &n.ProxyURL, &n.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &n, nil
}

func newNetworkID() string {
	b := make([]byte, 16)
	crand.Read(b)
	return "net-" + hex.EncodeToString(b)
}
