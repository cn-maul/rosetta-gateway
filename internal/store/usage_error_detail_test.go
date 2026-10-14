package store

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"strings"
	"testing"
)

// seedKeyForUsage 建一把可用的 key（usage 落库路径需要真实归属）。
func seedKeyForUsage(t *testing.T, st *Store) *AccessKey {
	t.Helper()
	ctx := context.Background()
	u := &User{ID: "u1", Username: "u1", Role: RoleUser, Status: UserStatusActive, AuthVersion: 1}
	if err := st.CreateUser(ctx, u); err != nil {
		t.Fatalf("create user: %v", err)
	}
	k := &AccessKey{ID: "k1", KeyHash: "h1", KeyPrefix: "sk-t", Name: "k1", Enabled: true, UserID: "u1"}
	if err := st.CreateAccessKey(ctx, k); err != nil {
		t.Fatalf("create key: %v", err)
	}
	return k
}

// TestUsageErrorDetail_RoundTripSingle 单条落库：error_detail 写入后可原样读回。
func TestUsageErrorDetail_RoundTripSingle(t *testing.T) {
	st := testStore(t, t.TempDir()+"/x.db")
	seedKeyForUsage(t, st)
	ctx := context.Background()

	detail := "You exceeded your current quota, please check your plan and billing details."
	if err := st.CreateUsageRecord(ctx, &UsageRecord{
		ID: "r1", AccessKeyID: "k1", UserID: "u1", PublicModel: "flash",
		ProviderID: "p1", UpstreamModel: "deepseek-chat", IngressProtocol: "openai-chat",
		Status: "error", HTTPStatus: 402, ErrorCode: "insufficient_quota",
		ErrorDetail: detail, UsageState: "none",
	}); err != nil {
		t.Fatalf("record: %v", err)
	}

	var got string
	err := st.Reader().QueryRowContext(ctx,
		`SELECT COALESCE(error_detail,'') FROM usage_records WHERE id='r1'`).Scan(&got)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got != detail {
		t.Fatalf("往返不一致: got %q want %q", got, detail)
	}
}

// TestUsageErrorDetail_RoundTripBatched 批量路径同样带 detail。
//
// 两条写路径（单条 INSERT 与多值批量）各有一份列清单与参数拼装，
// 批量那条最容易漏列 —— 症状是「detail 静默丢成 NULL」，
// 单测里任何一条路径没覆盖到都可能放过去。
func TestUsageErrorDetail_RoundTripBatched(t *testing.T) {
	st := testStore(t, t.TempDir()+"/x.db")
	seedKeyForUsage(t, st)
	ctx := context.Background()

	recs := []*UsageRecord{
		{ID: "b1", AccessKeyID: "k1", UserID: "u1", PublicModel: "flash", ProviderID: "p1",
			UpstreamModel: "m", IngressProtocol: "openai-chat", Status: "error", HTTPStatus: 429,
			ErrorCode: "rate_limited", ErrorDetail: "Too many requests per minute", UsageState: "none"},
		{ID: "b2", AccessKeyID: "k1", UserID: "u1", PublicModel: "flash", ProviderID: "p1",
			UpstreamModel: "m", IngressProtocol: "openai-chat", Status: "ok", HTTPStatus: 200,
			UsageState: "none"},
	}
	if _, err := st.CreateUsageRecordsBatched(ctx, recs); err != nil {
		t.Fatalf("batched: %v", err)
	}

	var d1, d2 sql.NullString
	if err := st.Reader().QueryRowContext(ctx, `SELECT error_detail FROM usage_records WHERE id='b1'`).Scan(&d1); err != nil {
		t.Fatalf("read b1: %v", err)
	}
	if d1.String != "Too many requests per minute" {
		t.Fatalf("b1 detail = %v", d1)
	}
	if err := st.Reader().QueryRowContext(ctx, `SELECT error_detail FROM usage_records WHERE id='b2'`).Scan(&d2); err != nil {
		t.Fatalf("read b2: %v", err)
	}
	if d2.Valid {
		t.Fatalf("成功记录的 detail 必须是 NULL，got %q", d2.String)
	}
}

// TestUsageErrorDetail_Migration 老库（无此列）升级后列存在且可读写。
//
// 模拟真实老库：建一个不含 error_detail 列的 usage_records，打开 store
// 触发迁移，再走一遍完整写读。ensureColumns 的 additions 只是「声明」，
// 这条测的是「声明真的被执行了」。
func TestUsageErrorDetail_Migration(t *testing.T) {
	path := t.TempDir() + "/old.db"
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	// 旧 schema：逐列手抄当前 DDL，但**不带 error_detail**。
	if _, err := raw.Exec(`CREATE TABLE usage_records (
		id TEXT PRIMARY KEY, ts INTEGER NOT NULL, access_key_id TEXT NOT NULL,
		user_id TEXT, public_model TEXT NOT NULL, provider_id TEXT NOT NULL,
		upstream_model TEXT NOT NULL, ingress_protocol TEXT NOT NULL,
		stream INTEGER NOT NULL DEFAULT 0, input_tokens INTEGER NOT NULL DEFAULT 0,
		output_tokens INTEGER NOT NULL DEFAULT 0, total_tokens INTEGER NOT NULL DEFAULT 0,
		reasoning_tokens INTEGER NOT NULL DEFAULT 0, cached_tokens INTEGER NOT NULL DEFAULT 0,
		usage_state TEXT NOT NULL, status TEXT NOT NULL, http_status INTEGER NOT NULL,
		error_code TEXT, latency_ms INTEGER NOT NULL, ttfb_ms INTEGER NOT NULL DEFAULT 0,
		request_id TEXT, cost_total REAL NOT NULL DEFAULT 0)`); err != nil {
		t.Fatalf("create old table: %v", err)
	}
	raw.Close()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	st, err := Open(path, logger)
	if err != nil {
		t.Fatalf("open migrated store: %v", err)
	}
	defer st.Close()

	if has, err := st.columnExists("usage_records", "error_detail"); err != nil || !has {
		t.Fatalf("迁移后应存在 error_detail 列: has=%v err=%v", has, err)
	}

	// 迁移后的库必须立即可写（列默认 NULL，不带 DEFAULT 的语义）。
	seedKeyForUsage(t, st)
	if err := st.CreateUsageRecord(context.Background(), &UsageRecord{
		ID: "r9", AccessKeyID: "k1", UserID: "u1", PublicModel: "flash", ProviderID: "p1",
		UpstreamModel: "m", IngressProtocol: "openai-chat", Status: "error", HTTPStatus: 500,
		ErrorCode: "upstream_error", ErrorDetail: "upstream 500", UsageState: "none",
	}); err != nil {
		t.Fatalf("write to migrated db: %v", err)
	}
}

// TestUsageErrorDetail_LongTextFits 1KB 边界的文本能完整落库（TEXT 列无截断）。
func TestUsageErrorDetail_LongTextFits(t *testing.T) {
	st := testStore(t, t.TempDir()+"/x.db")
	seedKeyForUsage(t, st)
	ctx := context.Background()

	detail := strings.Repeat("a", 1024) // outwire 截断后最长 1024 字节
	if err := st.CreateUsageRecord(ctx, &UsageRecord{
		ID: "r2", AccessKeyID: "k1", UserID: "u1", PublicModel: "flash", ProviderID: "p1",
		UpstreamModel: "m", IngressProtocol: "openai-chat", Status: "error", HTTPStatus: 400,
		ErrorCode: "invalid_request", ErrorDetail: detail, UsageState: "none",
	}); err != nil {
		t.Fatalf("record: %v", err)
	}
	var got string
	if err := st.Reader().QueryRowContext(ctx, `SELECT error_detail FROM usage_records WHERE id='r2'`).Scan(&got); err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(got) != 1024 {
		t.Fatalf("应完整存下 1024 字节，got %d", len(got))
	}
}
