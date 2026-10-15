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
	"errors"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/cn-maul/rosetta-gateway/internal/store"
)

// 无环境变量时必须生成一个真实强度的随机令牌。
//
// 「非空」是最低限度的断言 —— 但真正救命的是**长度与字符集**：空串或
// 短串都能通过「非空」检查，却等于把引导窗口敞开。
func TestEnsureBootstrapToken_GeneratesStrongTokenWhenEnvUnset(t *testing.T) {
	t.Setenv(bootstrapTokenEnv, "")
	db := newTestStore(t)
	ctx := context.Background()

	if err := ensureBootstrapToken(ctx, db, slog.Default()); err != nil {
		t.Fatalf("ensureBootstrapToken: %v", err)
	}

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

	if err := ensureBootstrapToken(ctx, db, slog.Default()); err != nil {
		t.Fatalf("ensureBootstrapToken: %v", err)
	}

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

	if err := ensureBootstrapToken(ctx, db, slog.Default()); err != nil {
		t.Fatalf("ensureBootstrapToken: %v", err)
	}
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

			// 建出待初始化管理员 = **窗口确实开着**。这一条不能省：
			// 没有它就没有窗口可保护，弱令牌也就只是「无害的失败」
			// （见 unprotectedWindowError），测试会钉错东西。
			ensureBootstrapAdmin(ctx, db, slog.Default())

			if err := ensureBootstrapToken(ctx, db, slog.Default()); err == nil {
				t.Error("弱环境变量令牌在窗口开着时竟然没有导致失败 —— 它会被静默接受")
			}

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

	if err := ensureBootstrapToken(ctx, db, slog.Default()); err != nil {
		t.Fatalf("ensureBootstrapToken: %v", err)
	}

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

// ---------------------------------------------------------------------------
// 2026-10-14 §6.1：令牌布置失败时**不得**开始监听。
//
// 下面这组测试针对的是上一轮留下的 fail-open：ensureBootstrapToken 在
// 随机源失败 / 弱环境变量 / 持久化失败时只记一条 ERROR 日志然后 return，
// 进程照常监听端口。日志里写了 ERROR 并不等于「窗口是关着的」——
// 而引导窗口挂在**免鉴权**的 POST /admin/api/bootstrap 上，绑 0.0.0.0 时
// 「谁先连到端口谁就是管理员」。
//
// 判据钉在返回值上（ensureBootstrapToken 返回 error），而 main 对它的
// 反应由 TestMainRefusesToServeWhenBootstrapTokenCannotBeEstablished
// （真的把进程跑起来、真的探端口）单独验证。
// ---------------------------------------------------------------------------

// failPersistStore 是一个「一切都正常，只有写令牌会失败」的 store。
//
// 为什么需要它：三种失效模式里，「持久化失败」无法用坏环境或坏随机源
// 触发 —— 环境变量正常、随机源正常时只有数据库会失败。把真实库弄坏
// 来测又不可靠（SQLite 的只读盘、目录权限在 Windows 上表现各异，
// 且会让同一个测试里的其他断言一起失败）。包一层只让 SetBootstrapToken
// 返回错误，是唯一既能稳定触发、又不牵连其他断言的办法。
type failPersistStore struct {
	*store.Store
	persistErr error
}

func (s *failPersistStore) SetBootstrapToken(ctx context.Context, token string) error {
	if s.persistErr != nil {
		return s.persistErr
	}
	return s.Store.SetBootstrapToken(ctx, token)
}

// failReadTokenStore 只让「读已有令牌」失败。
//
// 单独一个类型而不是给 failPersistStore 再加一个字段：这条路径与「写库
// 失败」在语义上完全不同 —— 一个是「不知道有没有令牌」，另一个是
// 「有令牌但存不下去」。它们的可观测后果也不同（前者可能是重启时令牌
// 已被消费，后者是本次启动刚生成的令牌丢了），值得分别钉住。
type failReadTokenStore struct {
	*store.Store
	readErr error
}

func (s *failReadTokenStore) BootstrapToken(ctx context.Context) (string, error) {
	if s.readErr != nil {
		return "", s.readErr
	}
	return s.Store.BootstrapToken(ctx)
}

// 随机源失败 + 窗口开着 → 必须报错，且**不得**写库。
//
// 「不得写库」比「必须报错」更容易被做坏：写一个空串进去看起来也能让
// BootstrapTokenRequired 回 false，但库里从此多出一条「配了令牌但值是
// 空」的记录，而 store 侧恰好把这一态判为 ErrBootstrapTokenMismatch ——
// 运维会看到「令牌永远不对」，真实原因是从没生成过。
func TestEnsureBootstrapToken_RandFailureRefusesWhenWindowOpen(t *testing.T) {
	t.Setenv(bootstrapTokenEnv, "")
	db := newTestStore(t)
	ctx := context.Background()
	ensureBootstrapAdmin(ctx, db, slog.Default())

	restore := swapBootstrapRand(func([]byte) (int, error) {
		return 0, errors.New("injected CSPRNG failure")
	})
	t.Cleanup(restore)

	if err := ensureBootstrapToken(ctx, db, slog.Default()); err == nil {
		t.Fatal("crypto/rand 失败但 ensureBootstrapToken 没有报错 —— 窗口将在无凭据状态下开放")
	}
	if tok, err := db.BootstrapToken(ctx); err != nil {
		t.Fatalf("read token: %v", err)
	} else if tok != "" {
		t.Errorf("token = %q, want 空（随机源失败时绝不能写库）", tok)
	}
}

// 弱环境变量 + 窗口开着 → 必须报错（不再静默接受一个 "1234"）。
func TestEnsureBootstrapToken_WeakEnvRefusesWhenWindowOpen(t *testing.T) {
	t.Setenv(bootstrapTokenEnv, "short")
	db := newTestStore(t)
	ctx := context.Background()
	ensureBootstrapAdmin(ctx, db, slog.Default())

	err := ensureBootstrapToken(ctx, db, slog.Default())
	if err == nil {
		t.Fatal("弱环境变量令牌被接受 —— 窗口防护强度等于 10^4 次猜测")
	}
	if tok, err := db.BootstrapToken(ctx); err != nil {
		t.Fatalf("read token: %v", err)
	} else if tok != "" {
		t.Errorf("token = %q, want 空（弱令牌被拒绝后不得写库）", tok)
	}
}

// 持久化失败 + 窗口开着 → 必须报错。
//
// 这一条是三种模式里唯一「看起来最无害」的一种：随机源正常、令牌算得出
// 来，只是没能存下来。也正因如此它最容易在重构中被当成「无关紧要」。
func TestEnsureBootstrapToken_PersistFailureRefusesWhenWindowOpen(t *testing.T) {
	t.Setenv(bootstrapTokenEnv, "")
	db := newTestStore(t)
	ctx := context.Background()
	ensureBootstrapAdmin(ctx, db, slog.Default())

	broken := &failPersistStore{Store: db, persistErr: errors.New("disk full")}
	if err := ensureBootstrapToken(ctx, broken, slog.Default()); err == nil {
		t.Fatal("写库失败但 ensureBootstrapToken 没有报错 —— 令牌只存在于内存里，重启即失效")
	}
}

// 读不到已有令牌 + 窗口开着 → 必须报错。
//
// 这一条曾经比其它路径更宽松（直接 return error、不问窗口开没开）。
// 单看代码没什么不对，但「某些失败致命、某些不致命」的分歧正是本轮
// 修复要消灭的东西：让四种失败共用 unprotectedWindowError 这一个判据。
func TestEnsureBootstrapToken_UnreadableTokenRefusesWhenWindowOpen(t *testing.T) {
	t.Setenv(bootstrapTokenEnv, "")
	db := newTestStore(t)
	ctx := context.Background()
	ensureBootstrapAdmin(ctx, db, slog.Default())

	broken := &failReadTokenStore{Store: db, readErr: errors.New("database is locked")}
	if err := ensureBootstrapToken(ctx, broken, slog.Default()); err == nil {
		t.Fatal("读不到已有令牌却没有报错 —— 无法确认窗口是否已受保护")
	}
}

// 状态读不出来 → 必须报错（fail-secure）。
//
// 这一态与上面三条都不同：失败不在「令牌」上，而在「我们不知道窗口开没开」。
// 此时假设「大概是关着的」正是要避免的 —— 状态不明不能当安全。
func TestEnsureBootstrapToken_UnreadableCompletedMarkerRefuses(t *testing.T) {
	t.Setenv(bootstrapTokenEnv, "")
	db := newTestStore(t)
	ctx := context.Background()
	ensureBootstrapAdmin(ctx, db, slog.Default())

	broken := &failPersistStore{Store: db, persistErr: errors.New("unused")}
	// 封掉「读 completed 标记」这一步：关掉底层库让它所有查询都失败，
	// 而不是改返回值 —— 这样第二次读（FindUninitializedAdmin）也会失败，
	// 正好验证「判不出来一律按致命处理」。
	if err := db.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}
	if err := ensureBootstrapToken(ctx, broken, slog.Default()); err == nil {
		t.Fatal("读不到窗口状态却没有报错 —— 把「状态不明」当成了「窗口关着」")
	}
}

// 窗口关着时，以上所有失败都**不得**阻止启动。
//
// 这是本次修复最容易做过头的地方，也是本组测试里最重要的一条。
// 已经完成过引导的实例（bootstrap_completed 已置）根本没有窗口可保护，
// 让它因为一次 crypto/rand 异常、或一个它此后再也不会被读到的
// ROSETTA_GW_BOOTSTRAP_TOKEN 而拒绝启动，是把一个安全改进变成
// 可用性事故：症状（正常的线上实例突然起不来）完全指不到真正的原因。
func TestEnsureBootstrapToken_FailuresHarmlessWhenWindowClosed(t *testing.T) {
	t.Setenv(bootstrapTokenEnv, "")
	db := newTestStore(t)
	ctx := context.Background()

	// 走完整引导路径，把窗口真正关掉。
	ensureBootstrapAdmin(ctx, db, slog.Default())
	if err := ensureBootstrapToken(ctx, db, slog.Default()); err != nil {
		t.Fatalf("precondition: ensureBootstrapToken: %v", err)
	}
	pending, err := db.FindUninitializedAdmin(ctx)
	if err != nil || pending == nil {
		t.Fatalf("precondition: pending admin not found (err %v)", err)
	}
	tok, err := db.BootstrapToken(ctx)
	if err != nil || tok == "" {
		t.Fatalf("precondition: token not issued (err %v)", err)
	}
	if ok, err := db.SetInitialAdminPasswordWithToken(ctx, pending.ID,
		"pbkdf2-sha256$210000$c2FsdA==$aGFzaA==", tok); !ok || err != nil {
		t.Fatalf("precondition: complete setup: ok=%v err=%v", ok, err)
	}
	if done, err := db.BootstrapCompleted(ctx); err != nil || !done {
		t.Fatalf("precondition: bootstrap not marked completed (err %v)", err)
	}

	// 三种失败各来一次，全部必须放行。
	restore := swapBootstrapRand(func([]byte) (int, error) {
		return 0, errors.New("injected CSPRNG failure")
	})
	if err := ensureBootstrapToken(ctx, db, slog.Default()); err != nil {
		t.Errorf("已完成引导的实例因随机源失败而拒绝启动（窗口已关，不该致命）: %v", err)
	}
	restore()

	t.Setenv(bootstrapTokenEnv, "short")
	if err := ensureBootstrapToken(ctx, db, slog.Default()); err != nil {
		t.Errorf("已完成引导的实例因一个不再被读取的环境变量而拒绝启动: %v", err)
	}

	t.Setenv(bootstrapTokenEnv, "")
	broken := &failPersistStore{Store: db, persistErr: errors.New("disk full")}
	if err := ensureBootstrapToken(ctx, broken, slog.Default()); err != nil {
		t.Errorf("已完成引导的实例因写库失败而拒绝启动（窗口已关，不该致命）: %v", err)
	}

	brokenRead := &failReadTokenStore{Store: db, readErr: errors.New("database is locked")}
	if err := ensureBootstrapToken(ctx, brokenRead, slog.Default()); err != nil {
		t.Errorf("已完成引导的实例因读令牌失败而拒绝启动（窗口已关，不该致命）: %v", err)
	}
}

// 没有任何待初始化管理员时，失败同样不该致命。
//
// 与上一条是不同的一态：上一条是「引导做过一次」，这条是「压根没有窗口」
// （例如 users 表非空，因此 ensureBootstrapAdmin 什么也不建）。
// 判定依据是 FindUninitializedAdmin 为 nil，与 BootstrapSetup 的权威判定
// 同源：不存在 role=admin 且 password_hash 为空的账号，那个免鉴权端点
// 在写库之前就会回 409。
func TestEnsureBootstrapToken_FailuresHarmlessWhenNoPendingAdmin(t *testing.T) {
	t.Setenv(bootstrapTokenEnv, "")
	db := newTestStore(t)
	ctx := context.Background()

	// 建一个**有密码**的普通用户：users 表非空 → ensureBootstrapAdmin 不引导。
	if err := db.CreateUser(ctx, &store.User{
		ID: "u-1", PasswordHash: "pbkdf2-sha256$210000$c2FsdA==$aGFzaA==",
		Username: "alice", Role: store.RoleUser, Status: store.UserStatusActive, AuthVersion: 1,
	}); err != nil {
		t.Fatalf("seed user: %v", err)
	}

	restore := swapBootstrapRand(func([]byte) (int, error) {
		return 0, errors.New("injected CSPRNG failure")
	})
	t.Cleanup(restore)
	if err := ensureBootstrapToken(ctx, db, slog.Default()); err != nil {
		t.Errorf("无待初始化管理员时随机源失败却拒绝启动（窗口不存在，不该致命）: %v", err)
	}
}

// swapBootstrapRand 临时替换随机源，返回还原函数。
//
// 包一层的理由与 production 里 bootstrapRandRead 一致：让「随机源失败」
// 这条分支真的能被执行到，而不是只活在注释里。还原走 t.Cleanup 而非
// defer，是为了让 panic / t.Fatal 提前退出时也不至于污染同包其它测试
// （bootstrapRandRead 是包级变量，残留一个失败的实现会让后面所有
// 依赖真实随机源的测试诡异地全挂）。
func swapBootstrapRand(f func([]byte) (int, error)) func() {
	prev := bootstrapRandRead
	bootstrapRandRead = f
	return func() { bootstrapRandRead = prev }
}
