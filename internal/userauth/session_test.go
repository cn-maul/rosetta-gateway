package userauth

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testSecret() string { return "test-secret-value-that-is-long-enough-32" }

func newTestManager(t *testing.T) *Manager {
	t.Helper()
	t.Setenv(SecretEnvName, testSecret())
	m, err := NewManager("")
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	if m == nil {
		t.Fatal("manager nil; env not applied")
	}
	return m
}

func TestManager_IssueVerifyRoundTrip(t *testing.T) {
	m := newTestManager(t)
	tok, exp, err := m.Issue(&UserClaims{
		UserID: "u1", AuthVersion: 3, Role: "admin", Username: "alice",
	})
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if time.Until(exp) <= 0 {
		t.Error("token already expired at issue time")
	}
	s, err := m.Verify(tok, 3)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if s.UserID != "u1" || s.Role != "admin" || s.Username != "alice" {
		t.Errorf("claims roundtrip wrong: %+v", s)
	}
}

// auth_version 栅栏：改密码/禁用账号后旧 token 立即作废。
// 这是无状态 JWT 唯一的吊销手段，坏了就等于「改密码无效」。
func TestVerify_StaleAuthVersionRejected(t *testing.T) {
	m := newTestManager(t)
	tok, _, _ := m.Issue(&UserClaims{UserID: "u1", AuthVersion: 1})

	if _, err := m.Verify(tok, 1); err != nil {
		t.Fatalf("same version should verify: %v", err)
	}
	if _, err := m.Verify(tok, 2); err != ErrStaleVersion {
		t.Errorf("newer version: err = %v, want ErrStaleVersion", err)
	}
	// currentVersion < 0 = 用户已不存在
	if _, err := m.Verify(tok, -1); err != ErrStaleVersion {
		t.Errorf("deleted user: err = %v, want ErrStaleVersion", err)
	}
}

func TestVerify_ExpiredToken(t *testing.T) {
	m := newTestManager(t)
	// 手工签一个已过期的
	tok, _, err := m.Issue(&UserClaims{UserID: "u1", AuthVersion: 1})
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	// 缩短 TTL 后重签
	m2 := &Manager{secret: m.secret, ttl: -time.Hour}
	tok2, _, err := m2.Issue(&UserClaims{UserID: "u1", AuthVersion: 1})
	if err != nil {
		t.Fatalf("issue expired: %v", err)
	}
	if _, err := m.Verify(tok2, 1); err != ErrTokenExpired {
		t.Errorf("expired token: err = %v, want ErrTokenExpired", err)
	}
	_ = tok
}

// alg=none 攻击：把 alg 改成 none 且签名留空，应被拒。
func TestVerify_RejectsAlgNone(t *testing.T) {
	m := newTestManager(t)
	// 手工构造：header={"alg":"none","typ":"JWT"}, payload 无签名
	unsigned := "eyJhbGciOiJub25lIiwidHlwIjoiSldUIn0." +
		"eyJ1aWQiOiJ1MSIsImF2IjoxLCJzdWIiOiJ1MSJ9."
	if _, err := m.Verify(unsigned, 1); err == nil {
		t.Error("alg=none token was accepted")
	}
}

// 用**另一个密钥**签的 token 必须被拒 —— 密钥混淆防护。
func TestVerify_RejectsForeignSignature(t *testing.T) {
	m := newTestManager(t)
	other := &Manager{secret: []byte("a-completely-different-secret-32chars"), ttl: defaultTTL}
	tok, _, err := other.Issue(&UserClaims{UserID: "u1", AuthVersion: 1})
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if _, err := m.Verify(tok, 1); err != ErrTokenInvalid {
		t.Errorf("foreign-signed token: err = %v, want ErrTokenInvalid", err)
	}
}

func TestVerify_RejectsGarbage(t *testing.T) {
	m := newTestManager(t)
	for _, tok := range []string{"", "   ", "not.a.jwt", "a.b", "...."} {
		if _, err := m.Verify(tok, 1); err == nil {
			t.Errorf("garbage token %q accepted", tok)
		}
	}
}

// 篡改 payload（改 user_id）必须被拒 —— 签名覆盖 payload。
func TestVerify_RejectsTamperedPayload(t *testing.T) {
	m := newTestManager(t)
	tok, _, _ := m.Issue(&UserClaims{UserID: "u1", AuthVersion: 1, Username: "alice"})
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("unexpected token shape: %d segments", len(parts))
	}
	// 把payload 换成 admin 身份
	if _, err := m.Verify(parts[0]+"."+parts[1]+"x."+parts[2], 1); err == nil {
		t.Error("tampered payload accepted")
	}
}

// Peek 不校验 auth_version —— 它只负责「先认出是谁」。
func TestPeek_DoesNotCheckVersion(t *testing.T) {
	m := newTestManager(t)
	tok, _, _ := m.Issue(&UserClaims{UserID: "u1", AuthVersion: 1, Username: "alice"})

	c, err := m.Peek(tok)
	if err != nil {
		t.Fatalf("peek: %v", err)
	}
	if c.UserID != "u1" || c.Username != "alice" {
		t.Errorf("peek claims wrong: %+v", c)
	}
	// 篡改签名必须被拒
	if _, err := m.Peek(tok + "x"); err == nil {
		t.Error("peek accepted tampered token")
	}
}

// 未启用时一切操作都必须失败 —— 少一个登录方式好过一个没验证的入口。
func TestManager_Disabled(t *testing.T) {
	os.Unsetenv(SecretEnvName)
	m, err := NewManager("")
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	if m != nil {
		t.Fatal("manager should be nil when secret unset")
	}
	if m.Enabled() {
		t.Error("nil manager reports enabled")
	}
	// nil 接收者的方法必须安全返回错误而不是 panic
	if _, _, err := m.Issue(&UserClaims{UserID: "u1"}); err == nil {
		t.Error("nil manager issued a token")
	}
	if _, err := m.Verify("anything", 1); err != ErrTokenInvalid {
		t.Errorf("nil manager verify: err = %v, want ErrTokenInvalid", err)
	}
	if _, err := m.Peek("anything"); err != ErrTokenInvalid {
		t.Errorf("nil manager peek: err = %v, want ErrTokenInvalid", err)
	}
}

// 短密钥必须被拒 —— 它保护的是「谁能进管理后台」。
func TestNewManager_RejectsShortSecret(t *testing.T) {
	t.Setenv(SecretEnvName, "too-short")
	if _, err := NewManager(""); err == nil {
		t.Error("short secret accepted; want error")
	}
}

func TestFingerprint_StableAndDistinct(t *testing.T) {
	a := &Session{UserID: "u1"}
	b := &Session{UserID: "u1"}
	c := &Session{UserID: "u2"}
	if a.Fingerprint() != b.Fingerprint() {
		t.Error("fingerprint not stable for same user")
	}
	if a.Fingerprint() == c.Fingerprint() {
		t.Error("fingerprint collides across users")
	}
	var nilSess *Session
	if nilSess.Fingerprint() != "" {
		t.Error("nil session should have empty fingerprint")
	}
}

// 缺密钥时必须自动生成并落盘，且**重启后读到同一把**。
//
// 这条是「不配 SESSION_SECRET 也能用」的整个前提：每次启动都现生成一把
// 新密钥的话，所有人手里的会话在下一次重启后集体失效，
// 症状是「莫名其妙被登出」，且没有任何错误信息。
func TestNewManager_GeneratesAndPersistsSecret(t *testing.T) {
	os.Unsetenv(SecretEnvName)
	path := filepath.Join(t.TempDir(), SecretFileName)

	m1, err := NewManager(path)
	if err != nil {
		t.Fatalf("first NewManager: %v", err)
	}
	if !m1.Enabled() {
		t.Fatal("manager disabled after auto-generation; login would be impossible")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("secret not persisted: %v", err)
	}

	// 模拟重启：同一路径再构造一次，必须拿到**同一把**密钥。
	m2, err := NewManager(path)
	if err != nil {
		t.Fatalf("second NewManager: %v", err)
	}
	tok, _, err := m2.Issue(&UserClaims{UserID: "u1", AuthVersion: 1})
	if err != nil {
		t.Fatalf("issue with reloaded secret: %v", err)
	}
	// 关键断言：重启前签发的令牌，重启后仍然有效。
	if _, err := m2.Verify(tok, 1); err != nil {
		t.Errorf("token issued before restart rejected after restart: %v", err)
	}
}

// 环境变量优先于文件：运维显式配的密钥不能被自动生成的文件顶掉。
func TestNewManager_EnvWinsOverFile(t *testing.T) {
	t.Setenv(SecretEnvName, testSecret())
	path := filepath.Join(t.TempDir(), SecretFileName)
	m, err := NewManager(path)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if !m.Enabled() {
		t.Fatal("manager disabled")
	}
	if _, err := os.Stat(path); err == nil {
		t.Error("file written even though env provided the secret")
	}
}

// 空文件不是「还没配」而是故障：静默重新生成 = 悄悄换掉所有会话的签名密钥。
func TestLoadOrCreateSecret_EmptyFileIsAnError(t *testing.T) {
	path := filepath.Join(t.TempDir(), SecretFileName)
	if err := os.WriteFile(path, []byte("   \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadOrCreateSecret(path); err == nil {
		t.Error("empty secret file silently regenerated; would invalidate all sessions")
	}
}
