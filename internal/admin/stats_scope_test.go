package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/cn-maul/rosetta-gateway/internal/store"
)

// 2026-10-10：总览页（stats）新增余额，并把**费用对普通用户开放**。
//
// 费用此前按 MULTIUSER.md §8「网关不做计费结算」被强制归零。余额计费落地后
// 该前提不成立：余额是真扣的，且用户已能在调用历史看到每一条 cost_total，
// 于是出现「明细可见、合计为 0」。这里钉住两件事：
//  1. 普通用户能拿到**自己**的费用；
//  2. 但拿不到**别人**的（scope 收窄没有被顺手破坏）。

func seedCostlyUsage(t *testing.T, st *store.Store, id, userID, keyID string, cost float64) {
	t.Helper()
	if err := st.CreateUsageRecord(t.Context(), &store.UsageRecord{
		ID: id, Ts: time.Now().UnixMilli(), UserID: userID, AccessKeyID: keyID,
		PublicModel: "m", ProviderID: "p1", UpstreamModel: "up",
		TotalTokens: 100, Status: "ok", HTTPStatus: 200,
		IngressProtocol: "openai-chat", UsageState: "reported",
	}); err != nil {
		t.Fatalf("seed usage %s: %v", id, err)
	}
	if _, err := st.DB().Exec(`UPDATE usage_records SET cost_total = ? WHERE id = ?`, cost, id); err != nil {
		t.Fatalf("set cost %s: %v", id, err)
	}
}

func getStats(t *testing.T, st *store.Store, req *http.Request) statsResponse {
	t.Helper()
	rec := httptest.NewRecorder()
	NewStatsHandler(st).Get(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp statsResponse
	decodeBody(t, rec, &resp)
	return resp
}

// 普通用户现在能看到自己的费用（此前恒为 0）。
func TestStats_CostVisibleToNormalUser(t *testing.T) {
	st := newScopeStore(t)
	seedTwoUsers(t, st)
	// 两个用户都有 key（seedTwoUsers 建了 k1→u1、k2→u2）
	seedCostlyUsage(t, st, "s1", "u1", "k1", 1.5)
	seedCostlyUsage(t, st, "s2", "u2", "k2", 9.5)

	from := time.Now().Add(-time.Hour).UnixMilli()
	to := time.Now().Add(time.Hour).UnixMilli()
	q := "/admin/api/stats?from=" + itoa(from) + "&to=" + itoa(to)

	alice := getStats(t, st, asUser(httptest.NewRequest(http.MethodGet, q, nil), "u1"))
	// alice 只该看到自己的 1.5，绝不能是 bob 的 9.5，也不是两者之和。
	if alice.Cost < 1.4 || alice.Cost > 1.6 {
		t.Errorf("alice 的费用 = %v，期望 1.5（自己的消费）", alice.Cost)
	}
	if alice.Cost > 2 {
		t.Errorf("alice 的费用 = %v —— 疑似泄露了 bob 的用量", alice.Cost)
	}

	admin := getStats(t, st, asAdmin(httptest.NewRequest(http.MethodGet, q, nil)))
	if admin.Cost < 10.9 || admin.Cost > 11.1 {
		t.Errorf("管理员看到的费用 = %v，期望 11（全局合计）", admin.Cost)
	}
}

// lifetime（终身累计）是**全局**单行，仍必须只对管理员开放 ——
// 那是这次改动**没有**顺带放开的东西，防止将来被误当成「费用也一起放开了」。
func TestStats_LifetimeStillAdminOnly(t *testing.T) {
	st := newScopeStore(t)
	seedTwoUsers(t, st)
	seedCostlyUsage(t, st, "l1", "u1", "k1", 1.0)
	seedCostlyUsage(t, st, "l2", "u2", "k2", 1.0)

	q := "/admin/api/stats?include_lifetime=true"
	if got := getStats(t, st, asUser(httptest.NewRequest(http.MethodGet, q, nil), "u1")).Lifetime; got != nil {
		t.Errorf("普通用户拿到了终身累计（全局数据）：%+v", got)
	}
	if got := getStats(t, st, asAdmin(httptest.NewRequest(http.MethodGet, q, nil))).Lifetime; got == nil {
		t.Error("管理员请求 include_lifetime 却没拿到终身累计")
	}
}

// 余额随 stats 一并下发，且是**当前登录者**的，不是别人的。
func TestStats_BalanceIsCallersOwn(t *testing.T) {
	st := newScopeStore(t)
	ctx := context.Background()
	for _, u := range []struct {
		id    string
		cents int64
		rem   int64
	}{
		{"u1", 5000, 1234},
		{"u2", 7777, 0},
	} {
		if err := st.CreateUser(ctx, &store.User{
			ID: u.id, Username: u.id, PasswordHash: "x", Role: store.RoleUser,
			Status: store.UserStatusActive, AuthVersion: 1,
			BalanceCents: u.cents, BalanceRemainder: u.rem,
		}); err != nil {
			t.Fatalf("create %s: %v", u.id, err)
		}
	}

	got := getStats(t, st, asUser(httptest.NewRequest(http.MethodGet, "/admin/api/stats", nil), "u1"))
	if got.BalanceCents != 5000 || got.BalanceRemainder != 1234 {
		t.Errorf("alice 看到余额 %d 分 / 余数 %d，期望 5000 / 1234",
			got.BalanceCents, got.BalanceRemainder)
	}
	if got.BalanceUnlimited {
		t.Error("有限额用户却报 balance_unlimited=true")
	}

	bob := getStats(t, st, asUser(httptest.NewRequest(http.MethodGet, "/admin/api/stats", nil), "u2"))
	if bob.BalanceCents != 7777 {
		t.Errorf("bob 看到余额 %d，期望 7777（他自己的）", bob.BalanceCents)
	}
}

// 不限额用户：unlimited=true 且余数不外泄（他本来就不攒余数）。
func TestStats_BalanceUnlimitedFlag(t *testing.T) {
	st := newScopeStore(t)
	ctx := context.Background()
	if err := st.CreateUser(ctx, &store.User{
		ID: "vip", Username: "vip", PasswordHash: "x", Role: store.RoleUser,
		Status: store.UserStatusActive, AuthVersion: 1,
		Unlimited: true, // balance_cents = NULL
	}); err != nil {
		t.Fatalf("create vip: %v", err)
	}
	got := getStats(t, st, asUser(httptest.NewRequest(http.MethodGet, "/admin/api/stats", nil), "vip"))
	if !got.BalanceUnlimited {
		t.Error("不限额用户应报 balance_unlimited=true")
	}
	if got.BalanceRemainder != 0 {
		t.Errorf("不限额用户的余数应为 0，实际 %d", got.BalanceRemainder)
	}
}

// itoa 避免为了一处拼接引入 strconv 后又不小心换错包。
func itoa(v int64) string {
	b, _ := json.Marshal(v)
	return string(b)
}
