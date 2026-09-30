package adminauth

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// 认证优先级：admin_auth.json 里的「用户密码」> config.json / ADMIN_TOKEN 的
// admin_token。这是管理后台唯一的鉴权事实来源，写成测试钉住，避免以后被改歪。
func TestVerify_PasswordBeatsConfigToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)

	// 尚无凭据文件：此时只认 config 兜底令牌（首次部署的引导通道）。
	s, err := Open(path, "cfg-token")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if !s.HasCredential() {
		t.Fatal("fallback token 应算作已配置凭据")
	}
	if s.HasUserPassword() {
		t.Fatal("尚未设置过用户密码")
	}
	if !s.Verify("cfg-token") {
		t.Fatal("设置密码前，config 令牌应可用")
	}
	if s.Verify("wrong") {
		t.Fatal("错误令牌必须失败")
	}

	// 设置用户密码后：密码生效，config 令牌**立即失效**。
	if err := s.Set("hunter2"); err != nil {
		t.Fatalf("set: %v", err)
	}
	if !s.HasUserPassword() {
		t.Fatal("应报告已有用户密码")
	}
	if !s.Verify("hunter2") {
		t.Fatal("用户密码应可用")
	}
	if s.Verify("cfg-token") {
		t.Fatal("设置密码后 config 令牌必须失效（用户密码优先，不是并存）")
	}
}

// 两者都没有：既不算「已配置凭据」，也校验不过 —— 前端据此进入首次设置。
func TestVerify_NoCredentialAtAll(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), FileName), "")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if s.HasCredential() {
		t.Fatal("无密码无兜底令牌时不应报告已配置凭据")
	}
	if s.Verify("") {
		t.Fatal("空令牌必须失败")
	}
	if s.Verify("anything") {
		t.Fatal("无凭据时任何输入都必须失败")
	}
}

// 凭据文件损坏 → 锁定态：一律拒绝，但 HasCredential 必须为真。
// 否则 password/set 的引导窗口会向所有人敞开，一个坏文件就等于后台任人接管。
func TestLocked_DeniesEverythingButClaimsCredential(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	if err := os.WriteFile(path, []byte("{ this is not json"), 0o600); err != nil {
		t.Fatalf("write corrupt: %v", err)
	}

	if _, err := Open(path, "cfg-token"); err == nil {
		t.Fatal("损坏的凭据文件必须让 Open 返回错误（调用方据此降级为锁定态）")
	}

	locked := NewLocked(path, errors.New("corrupt"))
	if !locked.HasCredential() {
		t.Fatal("锁定态必须视为已有凭据，否则首次设置窗口会对所有人敞开")
	}
	if locked.HasUserPassword() {
		t.Fatal("锁定态不应声称有可用的用户密码")
	}
	if locked.Verify("cfg-token") || locked.Verify("") {
		t.Fatal("锁定态必须拒绝一切令牌（包括 config 兜底令牌）")
	}
	if locked.LockedError() == nil {
		t.Fatal("LockedError 应返回原因")
	}
}

// 密码落盘并跨「重启」存活；Set 之后新开一个 Store 也能校验通过。
func TestSet_PersistsAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)

	s1, err := Open(path, "")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := s1.Set("s3cret-pw"); err != nil {
		t.Fatalf("set: %v", err)
	}

	// 模拟重启：重新从磁盘加载。
	s2, err := Open(path, "")
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if !s2.HasUserPassword() {
		t.Fatal("重启后应读到用户密码")
	}
	if !s2.Verify("s3cret-pw") {
		t.Fatal("重启后原密码应仍可用")
	}
	if s2.Verify("s3cret-p") {
		t.Fatal("前缀不应通过")
	}
}

// 过短的密码必须被拒（否则弱口令被并发爆破）。
func TestSet_RejectsWeakPassword(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), FileName), "")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := s.Set("12345"); !errors.Is(err, ErrWeakPassword) {
		t.Fatalf("want ErrWeakPassword, got %v", err)
	}
	// 被拒后不应留下任何凭据。
	if s.HasUserPassword() {
		t.Fatal("弱密码被拒后不应落盘")
	}
}
