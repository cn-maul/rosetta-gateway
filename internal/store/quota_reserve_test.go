package store

// 2026-10-07 计费链修复（P0-2：预占与触发器双倍记账）的回归测试。
//
// 修复前的形态：ReserveQuota 把预占直接加进 used_tokens；usage 落库触发器
// 再加真实用量；ReleaseQuota 按 actual-est 补差。一次请求的净记账是
// est + actual + (actual - est) = 2×actual —— 每个请求双倍扣费。
//
// 修复后的不变量（下列用例逐条钉住）：
//   - 预占落在独立列 reserved_tokens，used_tokens 全程只归 usage 触发器管；
//   - reserve → 落 usage → release 之后，used_tokens == 真实用量（不是 2 倍）；
//   - 判定用 used+reserved+est 三者之和，在途预占不许被并发第二个请求绕过；
//   - 释放不得把 reserved_tokens 推成负数（负预占 = 凭空多出额度）；
//   - usage missing（只 release 不落记录）时 used_tokens 不变、预占归零。

import (
	"context"
	"testing"
)

// assertTokens 读回某把 key 的 used_tokens 与 reserved_tokens 并断言终态。
// 两列必须一起看：只断言 used 会放过「预占泄漏进 used」或「预占没退干净」
// 这两类回归 —— 它们正是本次修复的缺陷形态。
func assertTokens(t *testing.T, st *Store, keyID string, wantUsed, wantReserved int64) {
	t.Helper()
	var used, reserved int64
	if err := st.read.QueryRowContext(context.Background(),
		`SELECT used_tokens, reserved_tokens FROM access_keys WHERE id = ?`, keyID).
		Scan(&used, &reserved); err != nil {
		t.Fatalf("read tokens of %s: %v", keyID, err)
	}
	if used != wantUsed || reserved != wantReserved {
		t.Fatalf("tokens wrong: used=%d (want %d), reserved=%d (want %d)",
			used, wantUsed, reserved, wantReserved)
	}
}

func mustCreateKey(t *testing.T, st *Store, id string, quota int64) {
	t.Helper()
	if err := st.CreateAccessKey(context.Background(), &AccessKey{
		ID: id, KeyHash: "h-" + id, KeyPrefix: "sk-gw-" + id, Name: "n",
		Enabled: true, QuotaTokens: quota,
	}); err != nil {
		t.Fatalf("create key %s: %v", id, err)
	}
}

// 完整链路：reserve(200) → 落 usage(actual=120) → release(200)
// ⇒ used_tokens == 120（真实用量，不是 2 倍）且 reserved_tokens == 0。
func TestQuotaReserve_FullFlowChargesActualOnce(t *testing.T) {
	ctx := context.Background()
	st := testStore(t, t.TempDir()+"/gw.db")
	mustCreateKey(t, st, "k1", 1000)

	reserved, ok, err := st.ReserveQuota(ctx, "k1", 200)
	if err != nil || !ok || reserved != 200 {
		t.Fatalf("reserve: ok=%v reserved=%d err=%v", ok, reserved, err)
	}
	// 预占只进 reserved_tokens：used_tokens 在 usage 落库前必须保持 0。
	assertTokens(t, st, "k1", 0, 200)

	const actual = 120
	if err := st.CreateUsageRecord(ctx, &UsageRecord{
		ID: "u1", AccessKeyID: "k1", PublicModel: "flash", IngressProtocol: "chat",
		TotalTokens: actual, UsageState: "reported", Status: "ok",
	}); err != nil {
		t.Fatalf("usage: %v", err)
	}

	if err := st.ReleaseQuota(ctx, "k1", reserved); err != nil {
		t.Fatalf("release: %v", err)
	}
	// 修复前这里是 est+actual+(actual-est)=240（双倍）；修复后必须恰为 actual。
	assertTokens(t, st, "k1", actual, 0)
}

// 判定必须计入在途预占：used(40) + reserved(50) + est(20) > quota(100) 时拒绝，
// 且被拒的请求不得留下任何痕迹；恰好压线（est=10 → 100 > 100 为假）则放行。
func TestQuotaReserve_RejectsWhenUsedPlusReservedPlusEstExceedsQuota(t *testing.T) {
	ctx := context.Background()
	st := testStore(t, t.TempDir()+"/gw.db")
	mustCreateKey(t, st, "k1", 100)

	// used_tokens = 40（触发器累加）。
	if err := st.CreateUsageRecord(ctx, &UsageRecord{
		ID: "u1", AccessKeyID: "k1", PublicModel: "flash", IngressProtocol: "chat",
		TotalTokens: 40, UsageState: "reported", Status: "ok",
	}); err != nil {
		t.Fatalf("seed usage: %v", err)
	}
	if reserved, ok, err := st.ReserveQuota(ctx, "k1", 50); err != nil || !ok || reserved != 50 {
		t.Fatalf("reserve 50: ok=%v reserved=%d err=%v", ok, reserved, err)
	}

	// 40+50+20=110 > 100 → 拒绝，且 reserved_tokens 不得被被拒请求改动。
	if reserved, ok, err := st.ReserveQuota(ctx, "k1", 20); ok || reserved != 0 || err != nil {
		t.Fatalf("over-quota reserve must be rejected, got ok=%v reserved=%d err=%v", ok, reserved, err)
	}
	assertTokens(t, st, "k1", 40, 50)

	// 边界：40+50+10=100 → 恰好用满，放行。
	if _, ok, err := st.ReserveQuota(ctx, "k1", 10); err != nil || !ok {
		t.Fatalf("boundary reserve (exactly quota) should pass: ok=%v err=%v", ok, err)
	}
	assertTokens(t, st, "k1", 40, 60)
}

// 释放不得把 reserved_tokens 推成负数：预占被并发路径先退掉一部分
// （这里用直接改库模拟）后再按原额退回，剩余必须夹在 0。
// 负预占会让后续 used+reserved+est 判定凭空多出额度。
func TestQuotaReserve_ReleaseClampsAtZero(t *testing.T) {
	ctx := context.Background()
	st := testStore(t, t.TempDir()+"/gw.db")
	mustCreateKey(t, st, "k1", 1000)

	reserved, ok, err := st.ReserveQuota(ctx, "k1", 100)
	if err != nil || !ok {
		t.Fatalf("reserve: ok=%v err=%v", ok, err)
	}
	// 模拟竞争：另一条路径已先把预占退到只剩 30。
	if _, err := st.db.ExecContext(ctx,
		`UPDATE access_keys SET reserved_tokens = 30 WHERE id = 'k1'`); err != nil {
		t.Fatalf("simulate partial release: %v", err)
	}
	if err := st.ReleaseQuota(ctx, "k1", reserved); err != nil {
		t.Fatalf("release: %v", err)
	}
	assertTokens(t, st, "k1", 0, 0)
}

// usage missing（上游没报用量）：不落任何 usage 记录、直接收尾
// ⇒ used_tokens 不变、reserved_tokens 归零。
//
// 这是对 P1-3 的 store 侧钉子：修复前网关层在 missing 时整条收尾被跳过，
// 预占（含 max_tokens 全额）永不退回。修复后调用方保证「missing → 也调
// ReleaseQuota」，本用例钉住它退的只是预占、绝不碰 used_tokens。
func TestQuotaReserve_UsageMissingOnlyRefundsReservation(t *testing.T) {
	ctx := context.Background()
	st := testStore(t, t.TempDir()+"/gw.db")
	mustCreateKey(t, st, "k1", 1000)

	reserved, ok, err := st.ReserveQuota(ctx, "k1", 300)
	if err != nil || !ok {
		t.Fatalf("reserve: ok=%v err=%v", ok, err)
	}
	assertTokens(t, st, "k1", 0, 300)

	// 不落 usage 记录，直接收尾（对应网关的 rate.releaseQuota 路径）。
	if err := st.ReleaseQuota(ctx, "k1", reserved); err != nil {
		t.Fatalf("release: %v", err)
	}
	assertTokens(t, st, "k1", 0, 0)
}
