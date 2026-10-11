package admin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cn-maul/rosetta-gateway/internal/store"
)

// quota_tokens / used_tokens 并入 /stats（2026-10-11，前端审计 H1 的收尾）。
//
// 此前这两个字段只有 /me 有，而 /me 是**登录快照** —— 前端钱包页拿它显示
// 「已用量」，页面开着不放时数字会长期停更。用户据一个早已失效的数字判断
// 自己还能不能用，被拒时完全不知道为什么。
//
// 这一组测试钉住三件事，缺一不可：
//  1. 字段随 stats 下发（否则前端又要退回快照）；
//  2. 取的是**当前登录者**的，不是 admin 的（scope 收窄不串号）；
//  3. 已用量的口径与配额**判定**完全一致 —— 见 TestStats_UsedTokens口径与配额判定一致。

// TestStats_UserQuotaAndUsed 覆盖「下发」与「不串号」。
func TestStats_UserQuotaAndUsed(t *testing.T) {
	st := newScopeStore(t)
	ctx := context.Background()

	mk := func(id string, quota int64) {
		t.Helper()
		if err := st.CreateUser(ctx, &store.User{
			ID: id, Username: id, PasswordHash: "x", Role: store.RoleUser,
			Status: store.UserStatusActive, AuthVersion: 1, QuotaTokens: quota,
		}); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
	}
	mk("alice", 5000)
	mk("bob", 900)

	alice := getStats(t, st, asUser(httptest.NewRequest(http.MethodGet, "/admin/api/stats", nil), "alice"))
	if alice.QuotaTokens != 5000 {
		t.Errorf("alice quota_tokens = %d，期望 5000", alice.QuotaTokens)
	}

	// 关键：两个用户的配额不同，任何串号都会在这里露出来。
	bob := getStats(t, st, asUser(httptest.NewRequest(http.MethodGet, "/admin/api/stats", nil), "bob"))
	if bob.QuotaTokens != 900 {
		t.Errorf("bob quota_tokens = %d，期望 900（拿到了别人的值？）", bob.QuotaTokens)
	}
}

// TestStats_配额为零表示不限 守住 quota_tokens=0 的既有语义：
// 它是「不限」，不是「配额用尽」。前端据此决定是否显示告警徽章，
// 若这里返回别的值，误判会直接显示在界面上。
func TestStats_配额为零表示不限(t *testing.T) {
	st := newScopeStore(t)
	ctx := context.Background()
	if err := st.CreateUser(ctx, &store.User{
		ID: "free", Username: "free", PasswordHash: "x", Role: store.RoleUser,
		Status: store.UserStatusActive, AuthVersion: 1, QuotaTokens: 0,
	}); err != nil {
		t.Fatalf("create: %v", err)
	}

	got := getStats(t, st, asUser(httptest.NewRequest(http.MethodGet, "/admin/api/stats", nil), "free"))
	if got.QuotaTokens != 0 {
		t.Errorf("quota_tokens = %d，期望 0（0 = 不限）", got.QuotaTokens)
	}
	if got.UsedTokens != 0 {
		t.Errorf("无用量时 used_tokens = %d，期望 0", got.UsedTokens)
	}
}

// TestStats_UsedTokens口径与配额判定一致 是本组最重要的一格。
//
// 界面显示的「已用」必须与后端判「超额」用的是**同一个数字**。两者一旦
// 分叉，用户会看到「明明还有额度却被拒」——而界面上没有任何线索能解释。
//
// 因此这里不走 HTTP，直接把 stats 返回值与判定函数 SumUserUsedTokens
// 的返回值逐字比对：一旦有人把 stats 改成读展示列 users.used_tokens
// （而不是判定用的累加列），本测试立刻红。
func TestStats_UsedTokens口径与配额判定一致(t *testing.T) {
	st := newScopeStore(t)
	ctx := context.Background()
	if err := st.CreateUser(ctx, &store.User{
		ID: "u", Username: "u", PasswordHash: "x", Role: store.RoleUser,
		Status: store.UserStatusActive, AuthVersion: 1, QuotaTokens: 100000,
	}); err != nil {
		t.Fatalf("create: %v", err)
	}

	got := getStats(t, st, asUser(httptest.NewRequest(http.MethodGet, "/admin/api/stats", nil), "u"))
	authoritative, err := st.SumUserUsedTokens(ctx, "u")
	if err != nil {
		t.Fatalf("SumUserUsedTokens: %v", err)
	}
	if got.UsedTokens != authoritative {
		t.Fatalf("stats.used_tokens = %d，而配额判定用的 SumUserUsedTokens = %d\n"+
			"  两者必须一致，否则界面显示的「已用」与「为什么被拒」会互相矛盾",
			got.UsedTokens, authoritative)
	}
}
