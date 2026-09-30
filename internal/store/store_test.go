package store

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"testing"
)

func testStore(t *testing.T, path string) *Store {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	st, err := Open(path, logger)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// 迁移回归：老库（routes 带 fallback_route_id 与 4 个「按路由策略」列）升级后
// 必须能正常打开、数据不丢、废弃列被摘除。
//
// 这条路径有真实风险：ALTER TABLE DROP COLUMN 对某些列（如带外键的
// fallback_route_id）可能不被 SQLite 接受，一旦失败就会把网关挡在启动之外。
// 因此 dropDeadColumns 对失败只记 WARN 不中断，本测试同时守住「能开」与「列已删」。
func TestMigration_DropsObsoleteRouteColumns(t *testing.T) {
	path := t.TempDir() + "/old.db"

	// 用原生 SQL 造一个「老 schema」的库（含本次要摘除的列）。
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	ddl := []string{
		`CREATE TABLE providers (
			id TEXT PRIMARY KEY, slug TEXT NOT NULL UNIQUE, name TEXT NOT NULL,
			protocol TEXT NOT NULL DEFAULT 'auto', endpoint TEXT NOT NULL,
			enabled INTEGER NOT NULL DEFAULT 1, timeout_ms INTEGER NOT NULL DEFAULT 0,
			max_retries INTEGER NOT NULL DEFAULT 2, quirks_json TEXT,
			created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL)`,
		`CREATE TABLE upstream_models (
			id TEXT PRIMARY KEY, provider_id TEXT NOT NULL, model_id TEXT NOT NULL,
			display_name TEXT, enabled INTEGER NOT NULL DEFAULT 1,
			context_window INTEGER, max_output_tokens INTEGER, supports_thinking INTEGER,
			UNIQUE (provider_id, model_id))`,
		`CREATE TABLE routes (
			id TEXT PRIMARY KEY, public_name TEXT NOT NULL UNIQUE,
			provider_id TEXT NOT NULL, upstream_model_id TEXT NOT NULL,
			enabled INTEGER NOT NULL DEFAULT 1,
			fallback_route_id TEXT REFERENCES routes(id) ON DELETE SET NULL,
			failover_enabled INTEGER NOT NULL DEFAULT 0,
			max_targets INTEGER NOT NULL DEFAULT 0,
			failure_threshold INTEGER NOT NULL DEFAULT 0,
			stream_first_token_timeout_ms INTEGER NOT NULL DEFAULT 0,
			nonstream_timeout_ms INTEGER NOT NULL DEFAULT 0,
			created_at INTEGER NOT NULL)`,
	}
	for i, s := range ddl {
		if _, err := raw.Exec(s); err != nil {
			t.Fatalf("seed ddl[%d]: %v", i, err)
		}
	}
	seed := []string{
		`INSERT INTO providers (id, slug, name, endpoint, created_at, updated_at) VALUES ('p1','p1','P1','http://x',1,1)`,
		`INSERT INTO upstream_models (id, provider_id, model_id, enabled) VALUES ('m1','p1','chat',1)`,
		`INSERT INTO routes (id, public_name, provider_id, upstream_model_id, enabled, failover_enabled, max_targets, created_at)
		   VALUES ('r1','flash','p1','m1',1,1,4,1)`,
	}
	for i, s := range seed {
		if _, err := raw.Exec(s); err != nil {
			t.Fatalf("seed data[%d]: %v", i, err)
		}
	}
	_ = raw.Close()

	// 正常路径打开 → 触发迁移。
	st := testStore(t, path)

	got, err := st.GetRoute(context.Background(), "r1")
	if err != nil {
		t.Fatalf("get route after migration: %v", err)
	}
	if got == nil || got.PublicName != "flash" {
		t.Fatalf("route lost during migration: %+v", got)
	}
	if !got.FailoverEnabled {
		t.Fatalf("failover_enabled not preserved: %+v", got)
	}

	for _, col := range []string{
		"fallback_route_id", "max_targets", "failure_threshold",
		"stream_first_token_timeout_ms", "nonstream_timeout_ms",
	} {
		has, err := st.columnExists("routes", col)
		if err != nil {
			t.Fatalf("columnExists(%s): %v", col, err)
		}
		if has {
			t.Fatalf("obsolete column %s still present after migration", col)
		}
	}
}

// 模拟升级：先建库并写入一条 route（此时还没有 route_targets），
// 关闭后重新 Open 触发迁移 —— 老 route 应被回填一条 position 0 的目标，
// 且再开一次不应重复插入（幂等）。
func TestMigration_BackfillRouteTargets(t *testing.T) {
	ctx := context.Background()
	path := t.TempDir() + "/gw.db"

	st := testStore(t, path)
	if err := st.CreateProvider(ctx, &Provider{ID: "p1", Slug: "p1", Name: "P1", Endpoint: "http://x", Enabled: true, Protocol: "openai-chat"}); err != nil {
		t.Fatalf("create provider: %v", err)
	}
	m := &UpstreamModel{ID: "m1", ProviderID: "p1", ModelID: "chat", Enabled: true}
	if err := st.CreateUpstreamModel(ctx, m); err != nil {
		t.Fatalf("create model: %v", err)
	}
	if err := st.CreateRoute(ctx, &Route{ID: "r1", PublicName: "flash", ProviderID: "p1", UpstreamModelID: "m1", Enabled: true}); err != nil {
		t.Fatalf("create route: %v", err)
	}
	st.Close()

	// 重开：ensureColumns + backfill
	st2 := testStore(t, path)
	targets, err := st2.ListRouteTargets(ctx, "r1")
	if err != nil {
		t.Fatalf("list targets: %v", err)
	}
	if len(targets) != 1 || targets[0].Position != 0 || targets[0].ProviderID != "p1" || targets[0].UpstreamModelID != "m1" {
		t.Fatalf("backfill wrong: %+v", targets)
	}

	// 再重开：不应重复回填
	st2.Close()
	st3 := testStore(t, path)
	targets3, _ := st3.ListRouteTargets(ctx, "r1")
	if len(targets3) != 1 {
		t.Fatalf("backfill not idempotent, got %d targets", len(targets3))
	}
}

// route 的 failover_enabled 读写往返（策略参数已收拢为全局 runtime_defaults，不再按路由存）。
func TestRouteFailoverFlag_RoundTrip(t *testing.T) {
	ctx := context.Background()
	st := testStore(t, t.TempDir()+"/gw.db")

	_ = st.CreateProvider(ctx, &Provider{ID: "p1", Slug: "p1", Name: "P1", Endpoint: "http://x", Enabled: true, Protocol: "openai-chat"})
	_ = st.CreateUpstreamModel(ctx, &UpstreamModel{ID: "m1", ProviderID: "p1", ModelID: "chat", Enabled: true})

	in := &Route{
		ID: "r1", PublicName: "flash", ProviderID: "p1", UpstreamModelID: "m1", Enabled: true,
		FailoverEnabled: true,
	}
	if err := st.CreateRoute(ctx, in); err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := st.GetRoute(ctx, "r1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !got.FailoverEnabled {
		t.Fatalf("failover flag not persisted: %+v", got)
	}
}

// 全局运行时默认（超时 + 故障转移策略）读写往返；未配置时读回全 0（由调用方回落 config）。
func TestRuntimeDefaults_RoundTrip(t *testing.T) {
	ctx := context.Background()
	st := testStore(t, t.TempDir()+"/gw.db")

	// 未配置：应读回零值（不是错误）。
	zero, err := st.GetRuntimeDefaults(ctx)
	if err != nil {
		t.Fatalf("get unset: %v", err)
	}
	if zero != (RuntimeDefaults{}) {
		t.Fatalf("unset should be zero, got %+v", zero)
	}

	want := RuntimeDefaults{
		UpstreamTimeoutMs:         90000,
		StreamIdleTimeoutMs:       45000,
		StreamFirstTokenTimeoutMs: 20000,
		FailoverMaxTargets:        4,
		FailoverFailureThreshold:  5,
	}
	if err := st.SetRuntimeDefaults(ctx, want); err != nil {
		t.Fatalf("set: %v", err)
	}
	got, err := st.GetRuntimeDefaults(ctx)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got != want {
		t.Fatalf("round-trip wrong:\n got %+v\nwant %+v", got, want)
	}
}

// ReplaceRouteTargets 整体重建链：position 重排、主目标列同步。
func TestReplaceRouteTargets(t *testing.T) {
	ctx := context.Background()
	st := testStore(t, t.TempDir()+"/gw.db")

	_ = st.CreateProvider(ctx, &Provider{ID: "p1", Slug: "p1", Name: "P1", Endpoint: "http://x", Enabled: true, Protocol: "openai-chat"})
	_ = st.CreateProvider(ctx, &Provider{ID: "p2", Slug: "p2", Name: "P2", Endpoint: "http://y", Enabled: true, Protocol: "openai-chat"})
	_ = st.CreateUpstreamModel(ctx, &UpstreamModel{ID: "m1", ProviderID: "p1", ModelID: "chat", Enabled: true})
	_ = st.CreateUpstreamModel(ctx, &UpstreamModel{ID: "m2", ProviderID: "p2", ModelID: "chat", Enabled: true})
	_ = st.CreateRoute(ctx, &Route{ID: "r1", PublicName: "flash", ProviderID: "p1", UpstreamModelID: "m1", Enabled: true})

	err := st.ReplaceRouteTargets(ctx, "r1", []RouteTarget{
		{ProviderID: "p2", UpstreamModelID: "m2", Enabled: true},
		{ProviderID: "p1", UpstreamModelID: "m1", Enabled: true},
	})
	if err != nil {
		t.Fatalf("replace: %v", err)
	}

	targets, _ := st.ListRouteTargets(ctx, "r1")
	if len(targets) != 2 {
		t.Fatalf("want 2 targets, got %d", len(targets))
	}
	if targets[0].Position != 0 || targets[0].ProviderID != "p2" {
		t.Fatalf("order wrong: %+v", targets)
	}
	if targets[1].Position != 1 || targets[1].ProviderID != "p1" {
		t.Fatalf("order wrong: %+v", targets)
	}
	// 主目标列应同步为链首 p2/m2
	r, _ := st.GetRoute(ctx, "r1")
	if r.ProviderID != "p2" || r.UpstreamModelID != "m2" {
		t.Fatalf("primary columns not synced: %+v", r)
	}
}

// 配额写路径（create/update 落 quota_tokens）+ GetKeyQuota 读到触发器累加的 used_tokens。
func TestAccessKeyQuota_WriteAndRead(t *testing.T) {
	ctx := context.Background()
	st := testStore(t, t.TempDir()+"/gw.db")

	k := &AccessKey{ID: "k1", KeyHash: "h1", KeyPrefix: "sk-gw-k1", Name: "n", Enabled: true, QuotaTokens: 1000}
	if err := st.CreateAccessKey(ctx, k); err != nil {
		t.Fatalf("create key: %v", err)
	}

	quota, used, ok, err := st.GetKeyQuota(ctx, "k1")
	if err != nil || !ok {
		t.Fatalf("get quota: ok=%v err=%v", ok, err)
	}
	if quota != 1000 || used != 0 {
		t.Fatalf("want quota=1000 used=0, got quota=%d used=%d", quota, used)
	}

	// update 改配额（name/enabled 一并回写，验证 quota 未被 UPDATE 漏掉）
	k.Name = "renamed"
	k.QuotaTokens = 2000
	if err := st.UpdateAccessKey(ctx, "k1", k); err != nil {
		t.Fatalf("update key: %v", err)
	}
	if quota, _, _, _ = st.GetKeyQuota(ctx, "k1"); quota != 2000 {
		t.Fatalf("quota not updated, got %d", quota)
	}

	// 落一条 usage，触发器应把 used_tokens 累加 total_tokens
	if err := st.CreateUsageRecord(ctx, &UsageRecord{
		ID: "u1", AccessKeyID: "k1", PublicModel: "flash", IngressProtocol: "openai-chat",
		TotalTokens: 500, UsageState: "reported", Status: "ok",
	}); err != nil {
		t.Fatalf("usage: %v", err)
	}
	_, used, _, _ = st.GetKeyQuota(ctx, "k1")
	if used != 500 {
		t.Fatalf("trigger did not bump used_tokens, got %d", used)
	}

	// 不存在的密钥 → ok=false，不报错
	if _, _, ok, err := st.GetKeyQuota(ctx, "missing"); ok || err != nil {
		t.Fatalf("missing key should be ok=false err=nil, got ok=%v err=%v", ok, err)
	}
}

// SyncHeadTarget：绕过目标链接口直接改 route 主目标列时，把改动落到 position 0 目标，
// 避免 routes 列与链分叉导致运行时静默失效。
func TestSyncHeadTarget(t *testing.T) {
	ctx := context.Background()
	st := testStore(t, t.TempDir()+"/gw.db")

	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("setup: %v", err)
		}
	}
	must(st.CreateProvider(ctx, &Provider{ID: "p1", Slug: "p1", Name: "P1", Endpoint: "http://x", Protocol: "openai-chat", Enabled: true}))
	must(st.CreateProvider(ctx, &Provider{ID: "p2", Slug: "p2", Name: "P2", Endpoint: "http://y", Protocol: "openai-chat", Enabled: true}))
	must(st.CreateUpstreamModel(ctx, &UpstreamModel{ID: "m1", ProviderID: "p1", ModelID: "c1", Enabled: true}))
	must(st.CreateUpstreamModel(ctx, &UpstreamModel{ID: "m2", ProviderID: "p2", ModelID: "c2", Enabled: true}))
	must(st.CreateRoute(ctx, &Route{ID: "r1", PublicName: "flash", ProviderID: "p1", UpstreamModelID: "m1", Enabled: true}))

	// 空链：补一条 position 0 目标，取新主目标。
	must(st.SyncHeadTarget(ctx, "r1", "p2", "m2"))
	ts, _ := st.ListRouteTargets(ctx, "r1")
	if len(ts) != 1 || ts[0].Position != 0 || ts[0].ProviderID != "p2" || ts[0].UpstreamModelID != "m2" {
		t.Fatalf("empty-chain sync wrong: %+v", ts)
	}

	// 有链：只改链首，第二条（position 1）保持不动。
	must(st.CreateRouteTarget(ctx, &RouteTarget{ID: "t2", RouteID: "r1", ProviderID: "p1", UpstreamModelID: "m1", Position: 1, Enabled: true}))
	must(st.SyncHeadTarget(ctx, "r1", "p1", "m1"))
	ts, _ = st.ListRouteTargets(ctx, "r1")
	if len(ts) != 2 {
		t.Fatalf("want 2 targets, got %d", len(ts))
	}
	if ts[0].ProviderID != "p1" || ts[0].UpstreamModelID != "m1" {
		t.Fatalf("head not synced: %+v", ts[0])
	}
	if ts[1].ID != "t2" || ts[1].Position != 1 {
		t.Fatalf("non-head target mutated: %+v", ts[1])
	}

	// 幂等：已一致时不新增、不改。
	must(st.SyncHeadTarget(ctx, "r1", "p1", "m1"))
	ts, _ = st.ListRouteTargets(ctx, "r1")
	if len(ts) != 2 || ts[0].ProviderID != "p1" {
		t.Fatalf("not idempotent: %+v", ts)
	}
}

// 费用口径：单价存于 upstream_models（元/百万 tokens），GetUsageStats 在读取时
// 联表实时算钱。三条不变量：
//   - 未命中输入 / 命中输入 / 输出各按各价计；
//   - 只配了输入价时，命中部分回退用输入价（不白送）；
//   - 没配价的模型（含已被删除的模型）贡献 0，不让整体统计失败。
func TestUsageStats_Cost(t *testing.T) {
	ctx := context.Background()
	st := testStore(t, t.TempDir()+"/gw.db")

	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("setup: %v", err)
		}
	}
	must(st.CreateProvider(ctx, &Provider{ID: "p1", Slug: "p1", Name: "P1", Endpoint: "http://x", Protocol: "openai-chat", Enabled: true}))
	must(st.CreateUpstreamModel(ctx, &UpstreamModel{
		ID: "m1", ProviderID: "p1", ModelID: "priced", Enabled: true,
		PriceInput: 0.2, PriceCacheHit: 0.02, PriceOutput: 0.8,
	}))
	must(st.CreateUpstreamModel(ctx, &UpstreamModel{
		ID: "m2", ProviderID: "p1", ModelID: "input-only", Enabled: true,
		PriceInput: 0.5,
	}))
	must(st.CreateUpstreamModel(ctx, &UpstreamModel{ID: "m3", ProviderID: "p1", ModelID: "free", Enabled: true}))

	usage := func(id, model string, in, cached, out int64) {
		t.Helper()
		must(st.CreateUsageRecord(ctx, &UsageRecord{
			ID: id, Ts: 1700000000000, AccessKeyID: "k1", PublicModel: model,
			ProviderID: "p1", UpstreamModel: model, IngressProtocol: "openai-chat",
			InputTokens: in, OutputTokens: out, TotalTokens: in + out, CachedTokens: cached,
			UsageState: "reported", Status: "ok",
		}))
	}
	usage("u1", "priced", 1_000_000, 400_000, 100_000) // 600k*0.2 + 400k*0.02 + 100k*0.8
	usage("u2", "input-only", 1_000_000, 1_000_000, 0) // 1M*0.5（命中回退输入价）
	usage("u3", "free", 5_000_000, 0, 5_000_000)       // 未配置价格 → 0
	usage("u4", "deleted-model", 2_000_000, 0, 2_000_000)

	stats, err := st.GetUsageStats(ctx, 0, 0)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	// 0.208 + 0.5 = 0.708
	if diff := stats.Cost - 0.708; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("want cost=0.708, got %v", stats.Cost)
	}
	if stats.TotalRequests != 4 || stats.InputTokens != 9_000_000 {
		t.Fatalf("token/request aggregate wrong: %+v", stats)
	}

	// 时间边界：区间外的记录不计费。
	if _, err = st.GetUsageStats(ctx, 1700000000001, 0); err != nil {
		t.Fatalf("stats after range: %v", err)
	}
	future, err := st.GetUsageStats(ctx, 1700000000001, 1700000000002)
	if err != nil {
		t.Fatalf("stats future: %v", err)
	}
	if future.Cost != 0 || future.TotalRequests != 0 {
		t.Fatalf("out-of-range window should be empty, got %+v", future)
	}
}

// 重新导入模型列表走 Upsert：价格是人工配置，不能被上游模型列表的
// upsert 顺手清零。
func TestUpsertUpstreamModel_KeepsPrices(t *testing.T) {
	ctx := context.Background()
	st := testStore(t, t.TempDir()+"/gw.db")

	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("setup: %v", err)
		}
	}
	must(st.CreateProvider(ctx, &Provider{ID: "p1", Slug: "p1", Name: "P1", Endpoint: "http://x", Protocol: "openai-chat", Enabled: true}))
	must(st.CreateUpstreamModel(ctx, &UpstreamModel{
		ID: "m1", ProviderID: "p1", ModelID: "chat", Enabled: true,
		DisplayName: "Chat", PriceInput: 1, PriceCacheHit: 0.1, PriceOutput: 2,
	}))

	must(st.UpsertUpstreamModel(ctx, &UpstreamModel{
		ID: "other", ProviderID: "p1", ModelID: "chat", Enabled: true, DisplayName: "Chat",
	}))

	got, err := st.GetUpstreamModel(ctx, "m1")
	if err != nil || got == nil {
		t.Fatalf("get: %v %v", got, err)
	}
	if got.PriceInput != 1 || got.PriceCacheHit != 0.1 || got.PriceOutput != 2 {
		t.Fatalf("prices wiped by upsert: %+v", got)
	}
}
