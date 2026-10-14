package main

// 2026-10-14 上线前审计加固（引导抢注窗口）在**启动流程**这一层的回归测试。
//
// internal/admin/bootstrap_token_test.go 钉住的是「令牌进来之后能不能用对」。
// 本文件钉住的是更靠上游、也更致命的一段：令牌**有没有被正确地产生**。
//
// 下面三种「写错」在 HTTP 层全部表现为同一个 403，运维无从分辨：
//   - 压根没生成令牌（写了个空串）；
//   - 重启时把运维手里那份覆盖成新值；
//   - 已完成引导的安装又凭空多出一段带凭据的日志。
// 它们都不会让任何测试变红 —— 除非在这里断言**库里那一条记录本身**。

import (
	"context"
	"encoding/hex"
	"log/slog"
	"os"
	"strings"
	"testing"
)

// 无环境变量时必须生成一个真实强度的随机令牌。
//
// 「非空」是最低限度的断言 —— 但真正救命的是**长度与字符集**：空串或
// 短串都能通过「非空」检查，却等于把引导窗口敞开。
func TestEnsureBootstrapToken_GeneratesStrongTokenWhenEnvUnset(t *testing.T) {
	t.Setenv(bootstrapTokenEnv, "")
	db := newTestStore(t)
	ctx := context.Background()

	ensureBootstrapToken(ctx, db, slog.Default())

	tok, err := db.BootstrapToken(ctx)
	if err != nil {
		t.Fatalf("read token: %v", err)
	}
	if tok == "" {
		t.Fatal("no token persisted —— 引导窗口此刻没有任何凭据保护")
	}
	if len(tok) != 64 {
		t.Errorf("token length = %d chars, want 64 (32 bytes hex)", len(tok))
	}
	if _, err := hex.DecodeString(tok); err != nil {
		t.Errorf("token is not hex: %v", err)
	}
	req, err := db.BootstrapTokenRequired(ctx)
	if err != nil || !req {
		t.Errorf("BootstrapTokenRequired = %v (err %v), want true", req, err)
	}
}

// 重启时不得覆盖已有令牌。
//
// 覆盖会让运维手里那份立刻失效：他们从**第一次**启动的日志里抄下令牌，
// 重启后（滚动更新、崩溃自愈都是常规操作）再填进去就被拒，而正确的令牌
// 只存在于**这一次**的日志里、没被他们看见 —— 于是引导被永久卡死。
func TestEnsureBootstrapToken_DoesNotOverwriteExistingToken(t *testing.T) {
	t.Setenv(bootstrapTokenEnv, "")
	db := newTestStore(t)
	ctx := context.Background()

	const mine = "aaaaaaaabbbbbbbbccccccccddddddddeeeeeeeeffffffff0000000011111111"
	if err := db.SetBootstrapToken(ctx, mine); err != nil {
		t.Fatalf("seed token: %v", err)
	}

	ensureBootstrapToken(ctx, db, slog.Default())

	got, err := db.BootstrapToken(ctx)
	if err != nil {
		t.Fatalf("read token: %v", err)
	}
	if got != mine {
		t.Fatalf("token = %q, want %q（重启覆盖令牌 = 运维手里的那把钥匙当场失效）", got, mine)
	}
}

// 已完成引导的安装不得再产生令牌。
//
// 这不是洁癖：那枚令牌永远不会有人去消费，却会以 WARN 级别留在日志与
// app_settings 里 —— 一枚永远有效的凭据静静躺在生产库中，等的就是哪天
// 有人误删 bootstrap_completed 让窗口重开的那一刻。
func TestEnsureBootstrapToken_NoTokenAfterBootstrapCompleted(t *testing.T) {
	t.Setenv(bootstrapTokenEnv, "")
	db := newTestStore(t)
	ctx := context.Background()

	ensureBootstrapToken(ctx, db, slog.Default())
	if tok, _ := db.BootstrapToken(ctx); tok == "" {
		t.Fatal("precondition: first call must produce a token")
	}
	// 走真实设密路径，把 bootstrap_completed 置上（且顺带消费令牌）。
	ensureBootstrapAdmin(ctx, db, slog.Default())
	pending, err := db.FindUninitializedAdmin(ctx)
	if err != nil || pending == nil {
		t.Fatalf("precondition: pending admin not found (err %v)", err)
	}
	if ok, err := db.SetInitialAdminPasswordWithToken(ctx, pending.ID,
		"pbkdf2-sha256$210000$c2FsdA==$aGFzaA==", "wrong-token-on-purpose"); ok || err == nil {
		t.Fatalf("precondition: setup should fail with token mismatch, got ok=%v err=%v", ok, err)
	}
	tok, err := db.BootstrapToken(ctx)
	if err != nil {
		t.Fatalf("read token: %v", err)
	}
	if ok, err := db.SetInitialAdminPasswordWithToken(ctx, pending.ID,
		"pbkdf2-sha256$210000$c2FsdA==$aGFzaA==", tok); !ok || err != nil {
		t.Fatalf("setup with correct token: ok=%v err=%v", ok, err)
	}

	// 现在这是一个「已完成引导」的安装。
	ensureBootstrapToken(ctx, db, slog.Default())

	got, err := db.BootstrapToken(ctx)
	if err != nil {
		t.Fatalf("read token: %v", err)
	}
	if got != "" {
		t.Fatalf("token = %q, want 空（已完成引导的安装不得凭空多出一枚凭据）", got)
	}
}

// 环境变量里的令牌必须**原样**落库。
//
// 这里钉的是一件很朴素但很容易被做坏的事：运维在部署清单里写的令牌，
// 必须就是他之后填进表单的那个值。任何「顺手规范化」（trim、去空白、
// 大小写折叠）都会让两边对不上，而症状是「配置看起来没问题却一直 403」。
func TestEnsureBootstrapToken_UsesEnvTokenVerbatim(t *testing.T) {
	const envTok = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	t.Setenv(bootstrapTokenEnv, envTok)
	db := newTestStore(t)
	ctx := context.Background()

	ensureBootstrapToken(ctx, db, slog.Default())

	got, err := db.BootstrapToken(ctx)
	if err != nil {
		t.Fatalf("read token: %v", err)
	}
	if got != envTok {
		t.Fatalf("token = %q, want 环境变量原值 %q", got, envTok)
	}
}

// 太短的环境变量令牌必须被拒绝，且**不得落库**。
//
// 静默接受 "1234" 等于让「配置了令牌」这件事变成一个安慰剂：窗口看起来
// 被保护了，实际防护强度等于 10^4 次猜测。宁可让它明明白白报错。
func TestEnsureBootstrapToken_RejectsWeakEnvToken(t *testing.T) {
	cases := []struct{ name, tok string }{
		{"过短", "short"},
		{"刚好少一个字节", strings.Repeat("a", bootstrapTokenMinLen-1)},
		{"常见弱口令", "password-password-password-pw"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(bootstrapTokenEnv, tc.tok)
			db := newTestStore(t)
			ctx := context.Background()

			ensureBootstrapToken(ctx, db, slog.Default())

			got, err := db.BootstrapToken(ctx)
			if err != nil {
				t.Fatalf("read token: %v", err)
			}
			if got != "" {
				t.Fatalf("token = %q, want 空（弱令牌被拒绝后不得写库）", got)
			}
			req, err := db.BootstrapTokenRequired(ctx)
			if err != nil {
				t.Fatalf("required: %v", err)
			}
			if req {
				t.Error("BootstrapTokenRequired = true —— 一枚弱令牌被当成了有效防护")
			}
		})
	}
}

// 刚好达到下限的令牌必须被接受 —— 拒绝阈值以下的所有值，
// 门槛不是「越严越好」，越严会把人推向可预测的绕行方式。
func TestEnsureBootstrapToken_AcceptsEnvTokenAtMinimumLength(t *testing.T) {
	tok := strings.Repeat("x", bootstrapTokenMinLen)
	t.Setenv(bootstrapTokenEnv, tok)
	db := newTestStore(t)
	ctx := context.Background()

	ensureBootstrapToken(ctx, db, slog.Default())

	got, err := db.BootstrapToken(ctx)
	if err != nil || got != tok {
		t.Fatalf("token = %q (err %v), want %q", got, err, tok)
	}
}

// newBootstrapToken 的返回语义。
//
// 失败时返回的是 (空串, error)，调用方据此**不写库**。这条不能退化：
// 一旦有人在某个分支里忽略 error、把空串写进库，库里就会多出一条
// 「配了令牌、值为空」的记录，而 store 侧恰好把这一态判为
// ErrBootstrapTokenMismatch —— 运维会看到「令牌永远不对」，
// 真实原因是这枚令牌从一开始就没被生成过。
func TestNewBootstrapToken_ReturnsErrorNotEmptyString(t *testing.T) {
	tok, err := newBootstrapToken()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tok == "" {
		t.Fatal("empty token with nil error —— 调用方无从分辨失败")
	}
	if len(tok) != 64 {
		t.Errorf("length = %d, want 64", len(tok))
	}
	// 两次调用必须不同（否则就不是随机源）。
	other, err := newBootstrapToken()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if other == tok {
		t.Error("newBootstrapToken returned the same value twice")
	}
}

// validateBootstrapToken 的边界。
func TestValidateBootstrapToken(t *testing.T) {
	cases := []struct {
		name    string
		tok     string
		wantErr bool
	}{
		{"空", "", true},
		{"差一字节", strings.Repeat("a", bootstrapTokenMinLen-1), true},
		{"正好下限", strings.Repeat("a", bootstrapTokenMinLen), false},
		{"高于下限", strings.Repeat("a", bootstrapTokenMinLen*2), false},
		{"64 位 hex", strings.Repeat("f", 64), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateBootstrapToken(tc.tok)
			if (err != nil) != tc.wantErr {
				t.Fatalf("validateBootstrapToken(%d bytes) err = %v, wantErr %v",
					len(tc.tok), err, tc.wantErr)
			}
		})
	}
}

// 确认测试自身没有污染进程环境：t.Setenv 会在测试结束后恢复，
// 这里显式再确认一次常量名与预期一致，避免后人改了变量名而测试
// 悄悄变成「设置了一个没人读的环境变量」的空断言。
func TestBootstrapTokenEnvConstant(t *testing.T) {
	if bootstrapTokenEnv != "ROSETTA_GW_BOOTSTRAP_TOKEN" {
		t.Errorf("bootstrapTokenEnv = %q, want ROSETTA_GW_BOOTSTRAP_TOKEN", bootstrapTokenEnv)
	}
	if _, ok := os.LookupEnv("ROSETTA_GW_BOOTSTRAP_TOKEN_OLD"); ok {
		t.Fatal("unexpected env var present")
	}
}
