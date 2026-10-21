package store

// 2026-10-14 上线前审计修复（用户级配额并发超发）的回归测试。
//
// 修复前的形态：handleIngress 里是裸读比较 ——
// SumUserUsedTokens → used >= quota → 放行。used 只含**已落库**的真实用量，
// 不含尚未终结的在途请求，而读-判-放行三步之间没有任何互斥。
// 实测：配额 1000、50 并发各估 200 token，50 个请求**全部通过**
// （占用口径 10000，超发 10 倍）。key 级早在 2026-10-07 换成了 ReserveQuota
// 原子预占，用户级没跟上 —— 一半修好的形态最容易被人当成「已经有限流」。
//
// 本文件钉住的不变量与 key 级（quota_reserve_test.go）逐条对称：
//   - 并发预占总量不得超过用户额度；
//   - 判定计入 used（真实用量）+ reserved（在途）+ est；
//   - 预占只写 reserved_tokens，绝不碰派生列 user_usage_totals；
//   - 释放原额退回且夹在 0，不产生负预占；
//   - 无额度用户（quota<=0）不受限，也不留预占；
//   - 老库升级后 users.reserved_tokens 必须在位，否则预检 fail-open。

import (
	"context"
	"database/sql"
	"sync"
	"testing"
)

// mustUserWithQuota 造一个带终身额度上限的用户。
func mustUserWithQuota(t *testing.T, st *Store, id string, quota int64) {
	t.Helper()
	if err := st.CreateUser(context.Background(), &User{
		ID: id, Username: id, PasswordHash: "h", Role: RoleUser,
		Status: UserStatusActive, AuthVersion: 1, QuotaTokens: quota,
	}); err != nil {
		t.Fatalf("create user %s: %v", id, err)
	}
}

// assertUserTokens 读回用户的已用量（派生列）与在途预占并断言终态。
//
// 两列必须一起看：只断言 used 会放过「预占泄漏进 used」这类双倍记账回归。
func assertUserTokens(t *testing.T, st *Store, userID string, wantUsed, wantReserved int64) {
	t.Helper()
	ctx := context.Background()
	var reserved int64
	if err := st.read.QueryRowContext(ctx,
		`SELECT reserved_tokens FROM users WHERE id = ?`, userID).Scan(&reserved); err != nil {
		t.Fatalf("read reserved of %s: %v", userID, err)
	}
	var used int64
	err := st.read.QueryRowContext(ctx,
		`SELECT total_tokens FROM user_usage_totals WHERE user_id = ?`, userID).Scan(&used)
	if err == sql.ErrNoRows {
		used = 0 // 还没有任何用量的用户没有累加行，与 ReserveUserQuota 的口径一致
	} else if err != nil {
		t.Fatalf("read usage totals of %s: %v", userID, err)
	}
	if used != wantUsed || reserved != wantReserved {
		t.Fatalf("user %s tokens wrong: used=%d (want %d), reserved=%d (want %d)",
			userID, used, wantUsed, reserved, wantReserved)
	}
}

// 核心不变量：并发 ReserveUserQuota 的放行总量不得超过用户额度。
//
// 修复前这个断言在裸读实现下会得到 50/50 全部放行（超发 10 倍）；
// 现在必须是恰好 5 次（5 × 200 = 1000）。
func TestReserveUserQuota_ConcurrentNeverExceedsTotal(t *testing.T) {
	ctx := context.Background()
	st := testStore(t, t.TempDir()+"/gw.db")

	const (
		quota   = 1000
		est     = 200
		workers = 50
	)
	mustUserWithQuota(t, st, "u1", quota)

	var (
		mu            sync.Mutex
		granted       int64
		grantedTokens int64
		rejected      int64
		otherErr      int64
		errs          []error
	)
	runConcurrent(workers, func(i int) {
		reserved, ok, err := st.ReserveUserQuota(ctx, "u1", est)
		mu.Lock()
		defer mu.Unlock()
		switch {
		case err != nil:
			otherErr++
			errs = append(errs, err)
		case ok:
			granted++
			grantedTokens += reserved
		default:
			rejected++
		}
	})

	if otherErr != 0 {
		t.Fatalf("出现 %d 次非预期错误: %v", otherErr, errs)
	}
	if granted != quota/est {
		t.Errorf("放行 %d 次, want **恰好** %d 次（quota %d / est %d）", granted, quota/est, quota, est)
	}
	if granted+rejected != workers {
		t.Errorf("放行+拒绝 = %d, want %d", granted+rejected, workers)
	}
	if grantedTokens > quota {
		t.Errorf("放行预占总量 = %d, want <= %d —— **超发**（用户级预占失效）", grantedTokens, quota)
	}
	assertUserTokens(t, st, "u1", 0, quota)
}

// 释放一次必须精确恢复一个请求的额度（不多不少）。
//
// 「不多」很重要：releaseQuota 在网关层是幂等的（先取后清零），
// 重复调用不会二次扣减；「不少」则保证上一条请求的预占确实退干净了，
// 否则额度会被慢慢吃光而没人知道是哪条请求吃的。
func TestReserveUserQuota_ReleaseRestoresExactlyOneSlot(t *testing.T) {
	ctx := context.Background()
	st := testStore(t, t.TempDir()+"/gw.db")
	mustUserWithQuota(t, st, "u1", 1000)

	const est = 500
	for i := 0; i < 2; i++ {
		if _, ok, err := st.ReserveUserQuota(ctx, "u1", est); err != nil || !ok {
			t.Fatalf("reserve %d: ok=%v err=%v", i, ok, err)
		}
	}
	// 额度已用满（1000），第三次必须被拒。
	if _, ok, err := st.ReserveUserQuota(ctx, "u1", est); err != nil || ok {
		t.Fatalf("third reserve must be rejected: ok=%v err=%v", ok, err)
	}
	assertUserTokens(t, st, "u1", 0, 1000)

	// 退掉一个 → 正好腾出 500，再要一个应当通过。
	if err := st.ReleaseUserQuota(ctx, "u1", est); err != nil {
		t.Fatalf("release: %v", err)
	}
	assertUserTokens(t, st, "u1", 0, 500)
	if _, ok, err := st.ReserveUserQuota(ctx, "u1", est); err != nil || !ok {
		t.Fatalf("reserve after release: ok=%v err=%v", ok, err)
	}
	assertUserTokens(t, st, "u1", 0, 1000)
}

// 判定必须同时计入真实用量与在途预占：
// used(300) + reserved(400) + est(300) = 1000 = quota → 恰好放行；
// 再要 1 个 → 1001 > 1000 → 拒绝。
//
// 这里同时覆盖「真实用量」这一维：修复前读的是裸 used，用量与在途预占
// 混在一起判定才能暴露「两处口径不一致」—— 那正是超发的另一半来源。
func TestReserveUserQuota_CountsBothUsedAndReserved(t *testing.T) {
	ctx := context.Background()
	st := testStore(t, t.TempDir()+"/gw.db")
	mustUserWithQuota(t, st, "u1", 1000)

	// 借一把 key 落 300 token 的真实用量（触发器会把它累进 user_usage_totals）。
	mustCreateKey(t, st, "k1", 0)
	if err := st.CreateUsageRecord(ctx, &UsageRecord{
		ID: "u1", AccessKeyID: "k1", UserID: "u1",
		PublicModel: "flash", IngressProtocol: "openai-chat",
		TotalTokens: 300, UsageState: "reported", Status: "ok",
	}); err != nil {
		t.Fatalf("seed usage: %v", err)
	}
	assertUserTokens(t, st, "u1", 300, 0)

	if _, ok, err := st.ReserveUserQuota(ctx, "u1", 400); err != nil || !ok {
		t.Fatalf("reserve 400: ok=%v err=%v", ok, err)
	}
	assertUserTokens(t, st, "u1", 300, 400)

	// 300+400+300 = 1000 恰好用满 → 放行。
	if _, ok, err := st.ReserveUserQuota(ctx, "u1", 300); err != nil || !ok {
		t.Fatalf("boundary reserve must pass: ok=%v err=%v", ok, err)
	}
	// 再要 1 个就超了。
	if _, ok, err := st.ReserveUserQuota(ctx, "u1", 1); err != nil || ok {
		t.Fatalf("over-quota reserve must be rejected: ok=%v err=%v", ok, err)
	}
	// 被拒的请求不得留下任何痕迹。
	assertUserTokens(t, st, "u1", 300, 700)
}

// quota<=0（不限额）是常态：不预占、不受限、不报错。
//
// 判错这一档的后果比超发更严重 —— 若把不限额用户判成「额度 0」，
// 等于让所有没配额度的用户全部 429。
func TestReserveUserQuota_UnlimitedUserPassesWithoutReservation(t *testing.T) {
	ctx := context.Background()
	st := testStore(t, t.TempDir()+"/gw.db")
	mustUserWithQuota(t, st, "u1", 0)

	reserved, ok, err := st.ReserveUserQuota(ctx, "u1", 100000)
	if err != nil || !ok || reserved != 0 {
		t.Fatalf("unlimited user: ok=%v reserved=%d err=%v, want true/0/nil", ok, reserved, err)
	}
	assertUserTokens(t, st, "u1", 0, 0)

	// 负额度同理（配置里写错符号的兜底），按不限额处理。
	if _, err := st.db.ExecContext(ctx,
		`UPDATE users SET quota_tokens = -5 WHERE id = 'u1'`); err != nil {
		t.Fatalf("set negative quota: %v", err)
	}
	if _, ok, err := st.ReserveUserQuota(ctx, "u1", 100000); err != nil || !ok {
		t.Fatalf("negative quota treated as unlimited: ok=%v err=%v", ok, err)
	}
	assertUserTokens(t, st, "u1", 0, 0)
}

// 未知用户：判定为拒绝而不是报错。
//
// 「报错」会让网关层走进 fail-open 分支之外的一条路（这里直接 500），
// 而「拒绝」才是安全默认：宁可这一个请求被拒，也不能因为一个孤儿 user_id
// 把配额预检整体废掉。返回值刻意是 (0, false, nil) 而不是 error。
func TestReserveUserQuota_UnknownUserRejected(t *testing.T) {
	st := testStore(t, t.TempDir()+"/gw.db")
	reserved, ok, err := st.ReserveUserQuota(context.Background(), "ghost", 100)
	if err != nil {
		t.Fatalf("unknown user must not error: %v", err)
	}
	if ok || reserved != 0 {
		t.Fatalf("unknown user: ok=%v reserved=%d, want false/0", ok, reserved)
	}
}

// est<=0（未知输出长度）按 1 个 token 预占：既不能「花 0 就放行」
// （那等于没有预检），也不能因为算不出长度就拒绝。
func TestReserveUserQuota_ZeroEstimateReservesOneToken(t *testing.T) {
	ctx := context.Background()
	st := testStore(t, t.TempDir()+"/gw.db")
	mustUserWithQuota(t, st, "u1", 3)

	for i := 1; i <= 3; i++ {
		reserved, ok, err := st.ReserveUserQuota(ctx, "u1", 0)
		if err != nil || !ok || reserved != 1 {
			t.Fatalf("reserve %d with est=0: ok=%v reserved=%d err=%v, want true/1/nil", i, ok, reserved, err)
		}
	}
	if _, ok, err := st.ReserveUserQuota(ctx, "u1", 0); err != nil || ok {
		t.Fatalf("4th zero-estimate reserve must be rejected: ok=%v err=%v", ok, err)
	}
	assertUserTokens(t, st, "u1", 0, 3)
}

// 释放不得把 reserved_tokens 推成负数。
//
// 负预占 = 凭空多出额度，且这个偏差是**终身**的（不像 TPM 窗口会自愈），
// 之后每次判定都多出 |负数| 个 token 的假余量。
func TestReserveUserQuota_ReleaseClampsAtZero(t *testing.T) {
	ctx := context.Background()
	st := testStore(t, t.TempDir()+"/gw.db")
	mustUserWithQuota(t, st, "u1", 1000)

	reserved, ok, err := st.ReserveUserQuota(ctx, "u1", 100)
	if err != nil || !ok {
		t.Fatalf("reserve: ok=%v err=%v", ok, err)
	}
	// 模拟竞争：另一条路径已先把预占退到 30。
	if _, err := st.db.ExecContext(ctx,
		`UPDATE users SET reserved_tokens = 30 WHERE id = 'u1'`); err != nil {
		t.Fatalf("simulate partial release: %v", err)
	}
	if err := st.ReleaseUserQuota(ctx, "u1", reserved); err != nil {
		t.Fatalf("release: %v", err)
	}
	assertUserTokens(t, st, "u1", 0, 0)

	// 幂等：再释放一次（网关层重复收尾）也不能变负。
	if err := st.ReleaseUserQuota(ctx, "u1", reserved); err != nil {
		t.Fatalf("second release: %v", err)
	}
	assertUserTokens(t, st, "u1", 0, 0)
}

// 完整链路：reserve(300) → 落 usage(actual=180) → release(300)
// ⇒ 真实用量恰为 180，预占归零。
//
// 这是用户级版本的「P0-2 双倍记账」钉子：若预占被误写进
// user_usage_totals，终态会是 180+300=480。
func TestReserveUserQuota_FullFlowCountsActualOnce(t *testing.T) {
	ctx := context.Background()
	st := testStore(t, t.TempDir()+"/gw.db")
	mustUserWithQuota(t, st, "u1", 100000)
	mustCreateKey(t, st, "k1", 0)

	reserved, ok, err := st.ReserveUserQuota(ctx, "u1", 300)
	if err != nil || !ok || reserved != 300 {
		t.Fatalf("reserve: ok=%v reserved=%d err=%v", ok, reserved, err)
	}
	// 预占期间真实用量必须仍是 0。
	assertUserTokens(t, st, "u1", 0, 300)

	if err := st.CreateUsageRecord(ctx, &UsageRecord{
		ID: "u1", AccessKeyID: "k1", UserID: "u1",
		PublicModel: "flash", IngressProtocol: "openai-chat",
		TotalTokens: 180, UsageState: "reported", Status: "ok",
	}); err != nil {
		t.Fatalf("usage: %v", err)
	}
	if err := st.ReleaseUserQuota(ctx, "u1", reserved); err != nil {
		t.Fatalf("release: %v", err)
	}
	assertUserTokens(t, st, "u1", 180, 0)
}
