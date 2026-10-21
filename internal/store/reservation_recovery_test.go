package store

// 预占的崩溃恢复（2026-10-21 修 P0-9）。
//
// reserved_tokens 是**终身**预占：它没有「窗口结束」这回事，只有请求正常
// 收尾时由 ReleaseQuota / ReleaseUserQuota 原额退回。
//
// 进程崩溃或被强杀时收尾代码不会执行 —— 那些预占就**永久留在库里**，
// 而终身配额的判定是 `used + reserved + est <= quota`，于是残留直接吃掉
// key / 用户的终身额度。实测：一次崩溃 + 一个 est=2000 的预占就能让一把
// quota=10000 的 key 少掉 20% 的额度；反复崩溃则额度完全冻结，只能人工
// 改库才能恢复。
//
// 解法：启动时清零（见 reservation_recovery.go）。预占的语义是「**本进程**
// 正在处理的请求占掉的额度」，而进程重启后没有在途请求。

import (
	"context"
	"path/filepath"
	"testing"
)

// 模拟崩溃：预占之后**不**释放，直接重开数据库（等价于进程被强杀）。
func TestReservationRecovery_重启后清零陈旧预占(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "gw.db")

	st := testStore(t, dbPath)
	mustUser(t, st, "u1", 0, false)
	if err := st.CreateAccessKey(ctx, &AccessKey{
		ID: "k1", Name: "k1", UserID: "u1", KeyHash: "h1", QuotaTokens: 10000,
	}); err != nil {
		t.Fatalf("create key: %v", err)
	}

	// 预占 2000 token（终身额度 10000）。
	if _, ok, err := st.ReserveQuota(ctx, "k1", 2000); err != nil || !ok {
		t.Fatalf("ReserveQuota: ok=%v err=%v", ok, err)
	}
	// 模拟崩溃：直接 Close，不调 ReleaseQuota。
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// 重开（= 进程重启）。
	st2 := testStore(t, dbPath)
	defer st2.Close()

	var reserved int64
	if err := st2.read.QueryRowContext(ctx,
		`SELECT reserved_tokens FROM access_keys WHERE id = ?`, "k1").Scan(&reserved); err != nil {
		t.Fatalf("read reserved: %v", err)
	}
	if reserved != 0 {
		t.Errorf("重启后 reserved_tokens = %d，期望 0 —— 崩溃留下的终身预占冻结了额度", reserved)
	}

	// 额度必须重新可用：再预占 10000（全部额度）应当成功。
	if _, ok, err := st2.ReserveQuota(ctx, "k1", 10000); err != nil || !ok {
		t.Errorf("重启后应能预占全部额度（10000），实际 ok=%v err=%v", ok, err)
	}
}

// 用户级预占同样要恢复。
func TestReservationRecovery_用户级预占也清零(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "gw.db")

	st := testStore(t, dbPath)
	if err := st.CreateUser(ctx, &User{
		ID: "u1", Username: "u1", PasswordHash: "h", Role: RoleUser,
		Status: UserStatusActive, AuthVersion: 1, QuotaTokens: 10000,
	}); err != nil {
		t.Fatalf("create user: %v", err)
	}
	if _, ok, err := st.ReserveUserQuota(ctx, "u1", 3000); err != nil || !ok {
		t.Fatalf("ReserveUserQuota: ok=%v err=%v", ok, err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	st2 := testStore(t, dbPath)
	defer st2.Close()

	var reserved int64
	if err := st2.read.QueryRowContext(ctx,
		`SELECT reserved_tokens FROM users WHERE id = ?`, "u1").Scan(&reserved); err != nil {
		t.Fatalf("read reserved: %v", err)
	}
	if reserved != 0 {
		t.Errorf("重启后 users.reserved_tokens = %d，期望 0", reserved)
	}
}
