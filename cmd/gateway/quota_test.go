package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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

// P0-1 回归（2026-10-07）：**全新** key（used_tokens=0）的终身配额必须生效。
//
// 修复前 handleIngress 把 GetKeyQuota 的第一个返回值（quota）丢弃，名叫 quota
// 的变量绑到的是第二个返回值（used），`else if quota > 0` 实际判的是
// 「已用量>0」—— used=0 的 key 永远走不到 ReserveQuota，quota=1 也照打上游。
// 请求显式带 max_tokens=64：est ≥ 64 > quota=1，确保被 ReserveQuota 的判定
// 拒绝，而不是恰好压线（est==1）放行造成假绿。
func TestQuota_FreshKeyWithTinyQuotaRejects(t *testing.T) {
	h, db := goodHarness(t)
	ctx := context.Background()

	if err := db.UpdateAccessKey(ctx, "k1", &store.AccessKey{
		ID: "k1", Name: "t", Enabled: true, QuotaTokens: 1,
	}); err != nil {
		t.Fatalf("set quota: %v", err)
	}

	body := `{"model":"flash","messages":[{"role":"user","content":"hi"}],"max_tokens":64,"stream":false}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testAccessKey)
	rec := httptest.NewRecorder()
	h(rec, req)

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("fresh key (used=0) with quota=1 must be 429, got %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "insufficient_quota") {
		t.Fatalf("want insufficient_quota error type, body=%s", rec.Body.String())
	}
}

// P0-2 回归（2026-10-07）：完整请求后 used_tokens 必须等于**真实用量**，不是 2 倍。
//
// 修复前：ReserveQuota 把预占加进 used_tokens → 触发器再加真实用量 →
// ReleaseQuota 只补差值，净记账 2×actual，每个请求双倍扣费。
// 修复后：预占走独立的 reserved_tokens 列，used_tokens 只由触发器累加一次。
//
// fakeGood 报告 usage total_tokens=8。usage 落库是异步的（usageRecorder 的
// worker），轮询等待触发器累加完成；若 used 停在 0 或跳到 16（双倍）都在
// 截止时间后判负。
func TestQuota_UsedTokensEqualsActualAfterRequest(t *testing.T) {
	h, db := goodHarness(t)
	ctx := context.Background()

	if err := db.UpdateAccessKey(ctx, "k1", &store.AccessKey{
		ID: "k1", Name: "t", Enabled: true, QuotaTokens: 100000,
	}); err != nil {
		t.Fatalf("set quota: %v", err)
	}

	rec := postChat(h, "flash")
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d body=%s", rec.Code, rec.Body.String())
	}

	const actual = 8 // fakeGood 的 usage：prompt 5 + completion 3
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, used, _, err := db.GetKeyQuota(ctx, "k1")
		if err != nil {
			t.Fatalf("get quota: %v", err)
		}
		if used == actual {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("used_tokens = %d after deadline, want exactly %d (0=用量没落库, 2×%d=双倍记账回归)",
				used, actual, actual)
		}
		time.Sleep(10 * time.Millisecond)
	}

	// 收尾必须把预占退干净：预占列残留会让后续请求被凭空压死
	// （判定是 used+reserved+est<=quota，残留的 reserved 纯属虚占）。
	var reserved int64
	if err := db.DB().QueryRowContext(ctx,
		`SELECT reserved_tokens FROM access_keys WHERE id = 'k1'`).Scan(&reserved); err != nil {
		t.Fatalf("read reserved_tokens: %v", err)
	}
	if reserved != 0 {
		t.Fatalf("reserved_tokens = %d after request, want 0 (预占没退干净)", reserved)
	}
}
