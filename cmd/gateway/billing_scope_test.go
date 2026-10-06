package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cn-maul/rosetta-gateway/internal/store"
)

// TestOrgEndpoints_ScopedToCallerUser 钉住一条 P1 的修复：org-wide 端点
// 必须按调用者身份收窄。
//
// 修复前这两个端点只做 auth.Authenticate，于是**任何一把 sk-gw key**都能读到
// 别的租户的模型名、用量与费用。实测：普通用户自己的 /admin/api/stats 显示
// total_tokens:12、cost:0（费用对非管理员刻意归零），而同一把 key 打
// /v1/organization/usage/completions 拿到的是**管理员租户**的
// input_tokens:20、num_model_requests:2；/costs 直接吐出 value:0.000072 cny
// —— 恰好是 stats 不给的那笔钱。
func TestOrgEndpoints_ScopedToCallerUser(t *testing.T) {
	up := fakeGood()
	defer up.Close()
	_, db, _ := buildHarnessFull(t, []struct {
		slug, url string
		fail      bool
	}{{slug: "good", url: up.URL}}, false, openaiChatCodec{})
	ctx := context.Background()

	if err := db.CreateProvider(ctx, &store.Provider{
		ID: "good", Slug: "good", Name: "good", Protocol: "openai-chat",
		Endpoint: up.URL, Enabled: true,
	}); err != nil {
		t.Fatalf("seed provider: %v", err)
	}
	if err := db.UpsertUpstreamModel(ctx, &store.UpstreamModel{
		ID: "pm1", ProviderID: "good", ModelID: "good-model", Enabled: true, PriceOutput: 2,
	}); err != nil {
		t.Fatalf("seed price: %v", err)
	}
	// 另一个用户名下的一笔大额用量，测试 key（tester）不该看到。
	if err := db.CreateUser(ctx, &store.User{
		ID: "vip", Username: "vip", PasswordHash: "x",
		Role: store.RoleUser, Status: store.UserStatusActive, AuthVersion: 1,
	}); err != nil {
		t.Fatalf("seed vip user: %v", err)
	}
	if err := db.CreateUsageRecord(ctx, &store.UsageRecord{
		ID: "other", AccessKeyID: "k1", UserID: "vip", PublicModel: "flash", ProviderID: "good",
		UpstreamModel: "good-model", IngressProtocol: "openai-chat",
		OutputTokens: 1_000_000, TotalTokens: 1_000_000,
		UsageState: "reported", Status: "ok", HTTPStatus: 200,
	}); err != nil {
		t.Fatalf("seed other-tenant usage: %v", err)
	}

	rec := httptest.NewRecorder()
	orgUsageCompletions(db)(rec, authedGet("/v1/organization/usage/completions?bucket_width=1d"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "1000000") {
		t.Fatalf("普通用户读到了别的租户的用量：%s", rec.Body.String())
	}

	rec2 := httptest.NewRecorder()
	orgCosts(db)(rec2, authedGet("/v1/organization/costs?bucket_width=1d"))
	if rec2.Code != http.StatusOK {
		t.Fatalf("costs status=%d body=%s", rec2.Code, rec2.Body.String())
	}
	if strings.Contains(rec2.Body.String(), "good-model") {
		t.Fatalf("普通用户读到了别的租户的费用行（模型名/金额都泄露）：%s", rec2.Body.String())
	}
}

// TestBillingUsage_BadDateRangeIsRejected 钉住 F14 的另一半：收到**无法解析**
// 的日期必须报 400，而不是静默回退到「最近 30 天」—— 静默回退等于给回调用方
// 另一个时间窗的数据却让它以为生效了（实测 `?start_date=garbage` 与反向区间
// 返回的数值与不带参数时完全相同）。
func TestBillingUsage_BadDateRangeIsRejected(t *testing.T) {
	up := fakeGood()
	defer up.Close()
	_, db, _ := buildHarnessFull(t, []struct {
		slug, url string
		fail      bool
	}{{slug: "good", url: up.URL}}, false, openaiChatCodec{})

	for _, q := range []string{
		"/v1/dashboard/billing/usage?start_date=garbage",
		"/v1/dashboard/billing/usage?end_date=not-a-date",
		"/v1/dashboard/billing/usage?start_date=2026-10-10&end_date=2026-10-01", // 反向
	} {
		rec := httptest.NewRecorder()
		billingUsage(db)(rec, authedGet(q))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s -> %d，期望 400（静默回退到近 30 天等于骗调用方）", q, rec.Code)
		}
	}

	// 不带参数仍按「最近 30 天」正常工作 —— 别把正常路径一起拒了。
	rec := httptest.NewRecorder()
	billingUsage(db)(rec, authedGet("/v1/dashboard/billing/usage"))
	if rec.Code != http.StatusOK {
		t.Fatalf("无参数应 200，实际 %d body=%s", rec.Code, rec.Body.String())
	}
}
