package store

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

// TestBackfillProxyNetworks 验证老库迁移：老 schema 里 providers.proxy_url
// 的非空值会被搬进 networks 行并回填 network_id，且同址合并成一条。
func TestBackfillProxyNetworks(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "old.db")
	ctx := context.Background()

	// 用裸 sql 造一个**老库**：providers 有 proxy_url、没有 network_id，
	// 其它表一概没有。不能用 Open 造 —— Open 自己就跑 migrate，会把新表
	// 一次建齐，那就没有「老库」可迁了。
	seedOld(t, dbPath)

	// Open 会跑完整条迁移链：ensureColumns 补 network_id → backfillProxyNetworks
	// 搬数据 → dropDeadColumns 摘掉 proxy_url。
	st := openForTest(t, dbPath)
	providers, err := st.ListProviders(ctx)
	if err != nil {
		t.Fatalf("list providers: %v", err)
	}
	networks, err := st.ListNetworks(ctx)
	if err != nil {
		t.Fatalf("list networks: %v", err)
	}
	if len(networks) != 1 {
		t.Fatalf("同址代理应合并成一条网络，得到 %d 条: %+v", len(networks), networks)
	}
	if networks[0].ProxyURL != "http://127.0.0.1:7897" {
		t.Errorf("网络地址不对: %q", networks[0].ProxyURL)
	}
	if networks[0].ProviderCount != 2 {
		t.Errorf("引用数应为 2，得到 %d", networks[0].ProviderCount)
	}

	bySlug := map[string]Provider{}
	for _, p := range providers {
		bySlug[p.Slug] = p
	}
	for _, slug := range []string{"a", "b"} {
		if bySlug[slug].NetworkID != networks[0].ID {
			t.Errorf("provider %s 应引用刚建的网络，得到 %q", slug, bySlug[slug].NetworkID)
		}
		if bySlug[slug].ProxyURL != "http://127.0.0.1:7897" {
			t.Errorf("provider %s 的派生 ProxyURL 不对: %q", slug, bySlug[slug].ProxyURL)
		}
	}
	for _, slug := range []string{"c", "d"} {
		if bySlug[slug].NetworkID != "" {
			t.Errorf("provider %s 应为直连，得到 %q", slug, bySlug[slug].NetworkID)
		}
		if bySlug[slug].ProxyURL != "" {
			t.Errorf("provider %s 的直连 ProxyURL 应为空，得到 %q", slug, bySlug[slug].ProxyURL)
		}
	}
	st.Close()

	// 迁移幂等：再开一次库不能再建出第二条同址网络。
	st2 := openForTest(t, dbPath)
	defer st2.Close()
	again, err := st2.ListNetworks(ctx)
	if err != nil {
		t.Fatalf("list networks again: %v", err)
	}
	if len(again) != 1 {
		t.Fatalf("重开后应仍只有一条网络，得到 %d 条", len(again))
	}
}

// seedOld 用裸连接造出迁移前的库形态。
func seedOld(t *testing.T, dbPath string) {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath+"?_foreign_keys=ON")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE networks (
		id TEXT PRIMARY KEY, name TEXT NOT NULL UNIQUE,
		proxy_url TEXT NOT NULL, created_at INTEGER NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE providers (
		id TEXT PRIMARY KEY, slug TEXT NOT NULL UNIQUE, name TEXT NOT NULL,
		protocol TEXT NOT NULL DEFAULT 'auto', endpoint TEXT NOT NULL,
		enabled INTEGER NOT NULL DEFAULT 1, timeout_ms INTEGER NOT NULL DEFAULT 0,
		max_retries INTEGER NOT NULL DEFAULT 2, quirks_json TEXT,
		proxy_url TEXT, balance_spec_json TEXT,
		created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	// 四行：两个共用同一个代理，另两个直连（NULL / 空串两种写法）。
	if _, err := db.Exec(`INSERT INTO providers
		(id, slug, name, protocol, endpoint, enabled, timeout_ms, max_retries, proxy_url, created_at, updated_at)
		VALUES
		('p1','a','A','auto','https://a',1,0,2,'http://127.0.0.1:7897',1,1),
		('p2','b','B','auto','https://b',1,0,2,'http://127.0.0.1:7897',2,2),
		('p3','c','C','auto','https://c',1,0,2,NULL,3,3),
		('p4','d','D','auto','https://d',1,0,2,'',4,4)`); err != nil {
		t.Fatal(err)
	}
}

func openForTest(t *testing.T, dbPath string) *Store {
	t.Helper()
	st, err := Open(dbPath, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("open %s: %v", dbPath, err)
	}
	return st
}
