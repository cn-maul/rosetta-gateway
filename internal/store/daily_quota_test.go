package store

import (
	"context"
	"testing"
	"time"
)

// 每日额度的三条核心语义：准入才计数、token 按真实用量校正、跨日自动归零。
//
// 这三条都落在 SQL 里（条件 UPDATE / MAX 夹 0 / day 分桶），编译通过不能
// 证明它们成立 —— 而写错的代价是「额度形同虚设」或「重启即重置」。
func TestDailyQuota_ReserveSettleAndRollover(t *testing.T) {
	ctx := context.Background()
	st := testStore(t, t.TempDir()+"/gw.db")
	if err := st.CreateUser(ctx, &User{ID: "u1", Username: "u1"}); err != nil {
		t.Fatalf("user: %v", err)
	}
	if err := st.CreateAccessKey(ctx, &AccessKey{
		ID: "k1", UserID: "u1", Name: "k1", KeyHash: "hash", Enabled: true,
	}); err != nil {
		t.Fatalf("key: %v", err)
	}

	limit := DailyQuotaLimit{Requests: 2, Tokens: 100}

	// 第一次：估算 40 token，准入并记 1 次请求。
	res, ok, err := st.ReserveDailyQuota(ctx, "first", "k1", limit, 40)
	if err != nil || !ok {
		t.Fatalf("reserve #1 = (%+v, %v, %v)", res, ok, err)
	}
	if res.Tokens != 40 {
		t.Fatalf("reserved tokens = %d, want 40", res.Tokens)
	}
	// 真实用量 10：token 退回 30，请求数保持 1（失败/重试也算一次调用）。
	if err := st.SettleDailyQuota(ctx, "k1", res, 10); err != nil {
		t.Fatalf("settle: %v", err)
	}
	if err := st.SettleDailyQuota(ctx, "k1", res, 1); err != nil {
		t.Fatalf("repeat settlement: %v", err)
	}
	reqs, toks, err := st.GetDailyUsage(ctx, "k1")
	if err != nil {
		t.Fatalf("get usage: %v", err)
	}
	if reqs != 1 || toks != 10 {
		t.Fatalf("after settle = (%d req, %d tok), want (1, 10)", reqs, toks)
	}

	// 第二次：估算 50 会把 token 推到 60（<= 100），放行。
	if _, ok, err := st.ReserveDailyQuota(ctx, "second", "k1", limit, 50); err != nil || !ok {
		t.Fatalf("reserve #2 = (ok=%v, err=%v)", ok, err)
	}
	// 第三次：请求数已到 2，必须拒绝，且被拒的这次不得计数。
	if _, ok, err := st.ReserveDailyQuota(ctx, "third", "k1", limit, 1); err != nil || ok {
		t.Fatalf("reserve #3 = (ok=%v, err=%v), want rejected", ok, err)
	}
	reqs, toks, err = st.GetDailyUsage(ctx, "k1")
	if err != nil {
		t.Fatalf("get usage: %v", err)
	}
	if reqs != 2 || toks != 60 {
		t.Fatalf("after reject = (%d req, %d tok), want (2, 60)", reqs, toks)
	}

	// 跨日：新的一天是全新一行，计数从零开始（不需要任何清零任务）。
	if _, err := st.db.ExecContext(ctx,
		`UPDATE access_key_daily_usage SET day = '2000-01-01' WHERE access_key_id = 'k1'`); err != nil {
		t.Fatalf("rewind day: %v", err)
	}
	reqs, toks, err = st.GetDailyUsage(ctx, "k1")
	if err != nil {
		t.Fatalf("get usage after rollover: %v", err)
	}
	if reqs != 0 || toks != 0 {
		t.Fatalf("new day = (%d req, %d tok), want (0, 0)", reqs, toks)
	}
	newDay, ok, err := st.ReserveDailyQuota(ctx, "new-day", "k1", limit, 1)
	if err != nil || !ok {
		t.Fatalf("reserve on new day = (ok=%v, err=%v)", ok, err)
	}

	// 准入后没打到上游（客户端在首个目标前断开）→ 整笔退回，包含请求数。
	// 不退回的话，「发起即断开」就能合法地把每日请求额度刷完。
	reqs, toks, err = st.GetDailyUsage(ctx, "k1")
	if err != nil {
		t.Fatalf("get usage before release: %v", err)
	}
	if reqs != 1 || toks != 1 {
		t.Fatalf("before release = (%d req, %d tok), want (1, 1)", reqs, toks)
	}
	if err := st.ReleaseDailyQuota(ctx, "k1", newDay); err != nil {
		t.Fatalf("release: %v", err)
	}
	reqs, toks, err = st.GetDailyUsage(ctx, "k1")
	if err != nil {
		t.Fatalf("get usage after release: %v", err)
	}
	if reqs != 0 || toks != 0 {
		t.Fatalf("after release = (%d req, %d tok), want (0, 0)", reqs, toks)
	}
}

// 不配 token 上限时只按请求数计数：库里的 tokens 必须保持 0，不能把估算值
// 写进一个没人看的列（否则运维读到的「今日 token」是凭空的预估值）。
func TestDailyQuota_RequestsOnlyDoesNotRecordTokens(t *testing.T) {
	ctx := context.Background()
	st := testStore(t, t.TempDir()+"/gw.db")
	if err := st.CreateUser(ctx, &User{ID: "u1", Username: "u1"}); err != nil {
		t.Fatalf("user: %v", err)
	}
	if err := st.CreateAccessKey(ctx, &AccessKey{
		ID: "k1", UserID: "u1", Name: "k1", KeyHash: "hash", Enabled: true,
	}); err != nil {
		t.Fatalf("key: %v", err)
	}
	res, ok, err := st.ReserveDailyQuota(ctx, "requests-only", "k1", DailyQuotaLimit{Requests: 5}, 999)
	if err != nil || !ok {
		t.Fatalf("reserve = (ok=%v, err=%v)", ok, err)
	}
	if res.Tokens != 0 {
		t.Fatalf("reserved = %+v, want no token reservation", res)
	}
	reqs, toks, err := st.GetDailyUsage(ctx, "k1")
	if err != nil {
		t.Fatalf("get usage: %v", err)
	}
	if reqs != 1 || toks != 0 {
		t.Fatalf("usage = (%d req, %d tok), want (1, 0)", reqs, toks)
	}
}

func TestDailyQuota_RecoveryPreservesHistoricalCountsAndUnknownUsage(t *testing.T) {
	ctx := context.Background()
	st := testStore(t, t.TempDir()+"/gw.db")
	now := time.Now()
	yesterday := now.AddDate(0, 0, -1)
	day := dailyUsageDay(yesterday)
	if _, err := st.db.ExecContext(ctx, `INSERT INTO access_key_daily_usage VALUES ('k1', ?, 5, 100)`, day); err != nil {
		t.Fatal(err)
	}
	for _, requestID := range []string{"never-started", "unknown", "reported"} {
		res, ok, err := st.reserveDailyQuota(ctx, requestID, "k1", DailyQuotaLimit{Tokens: 1000}, 40, yesterday)
		if err != nil || !ok {
			t.Fatalf("reserve %s: %v / %v", requestID, ok, err)
		}
		if requestID != "never-started" {
			if err := st.StartDailyQuota(ctx, "k1", res); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := st.CreateUsageRecord(ctx, &UsageRecord{ID: "usage-1", RequestID: "reported", AccessKeyID: "k1",
		UsageState: UsageStateReported, Status: UsageStatusOK, InputTokens: 4, OutputTokens: 3, TotalTokens: 7}); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := st.recoverStaleReservations(ctx); err != nil {
			t.Fatal(err)
		}
		var requests, tokens int64
		if err := st.read.QueryRowContext(ctx, `SELECT requests, tokens FROM access_key_daily_usage WHERE access_key_id = 'k1' AND day = ?`, day).Scan(&requests, &tokens); err != nil || requests != 7 || tokens != 147 {
			t.Fatalf("recovered usage = (%d, %d, %v), want (7, 147, nil)", requests, tokens, err)
		}
	}
}

func TestDailyQuota_AdditiveUpgradeRetainsDailyUsage(t *testing.T) {
	ctx := context.Background()
	st := testStore(t, t.TempDir()+"/gw.db")
	for _, statement := range []string{
		`DROP TRIGGER trg_daily_quota_usage`,
		`DROP TABLE daily_quota_reservations`,
		`INSERT INTO access_key_daily_usage VALUES ('legacy', date('now','localtime'), 2, 75)`,
	} {
		if _, err := st.db.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.ensureSchema(ctx); err != nil {
		t.Fatal(err)
	}
	res, ok, err := st.ReserveDailyQuota(ctx, "new-request", "legacy", DailyQuotaLimit{Tokens: 100}, 25)
	if err != nil || !ok {
		t.Fatalf("reserve after upgrade: %v / %v", ok, err)
	}
	if err := st.ReleaseDailyQuota(ctx, "legacy", res); err != nil {
		t.Fatal(err)
	}
	requests, tokens, err := st.GetDailyUsage(ctx, "legacy")
	if err != nil || requests != 2 || tokens != 75 {
		t.Fatalf("legacy counts = %d / %d: %v", requests, tokens, err)
	}
}
