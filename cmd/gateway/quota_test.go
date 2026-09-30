package main

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/cn-maul/rosetta-gateway/internal/store"
)

func goodHarness(t *testing.T) (http.HandlerFunc, *store.Store) {
	t.Helper()
	good := fakeGood()
	t.Cleanup(good.Close)
	return buildHarness(t, []struct {
		slug, url string
		fail      bool
	}{
		{"good", good.URL, false},
	}, false)
}

// 配额用尽 → 请求前预检直接 429，绝不触碰上游。
func TestQuota_ExceededRejectsBeforeUpstream(t *testing.T) {
	h, db := goodHarness(t)
	ctx := context.Background()

	if err := db.UpdateAccessKey(ctx, "k1", &store.AccessKey{
		ID: "k1", Name: "t", Enabled: true, QuotaTokens: 100,
	}); err != nil {
		t.Fatalf("set quota: %v", err)
	}
	// 触发器把 used_tokens 累加到 100（同步：INSERT 内即触发）
	if err := db.CreateUsageRecord(ctx, &store.UsageRecord{
		ID: "u1", AccessKeyID: "k1", PublicModel: "flash", IngressProtocol: "openai-chat",
		TotalTokens: 100, UsageState: "reported", Status: "ok",
	}); err != nil {
		t.Fatalf("seed usage: %v", err)
	}

	rec := postChat(h, "flash")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("want 429 on exhausted quota, got %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "insufficient_quota") {
		t.Fatalf("want insufficient_quota error type, body=%s", rec.Body.String())
	}
}

// quota=0 表示不限：哪怕已有用量也照常放行到上游。
func TestQuota_ZeroMeansUnlimited(t *testing.T) {
	h, db := goodHarness(t)
	ctx := context.Background()

	// k1 默认 quota 0；灌一大堆用量仍应放行
	if err := db.CreateUsageRecord(ctx, &store.UsageRecord{
		ID: "u1", AccessKeyID: "k1", PublicModel: "flash", IngressProtocol: "openai-chat",
		TotalTokens: 999999, UsageState: "reported", Status: "ok",
	}); err != nil {
		t.Fatalf("seed usage: %v", err)
	}

	rec := postChat(h, "flash")
	if rec.Code != http.StatusOK {
		t.Fatalf("quota 0 must not block, got %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "pong") {
		t.Fatalf("expected served-by-good pong, body=%s", rec.Body.String())
	}
}
