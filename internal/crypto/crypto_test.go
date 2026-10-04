package crypto

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLoadMasterKey_TruncatedFileIsRejected 坐实一个会静默摧毁全部存量凭据
// 的缺陷：旧实现用 os.WriteFile（O_TRUNC）写 master.key，进程在写入中途被
// kill / 断电会留下 0 字节或半截文件。重启时：
//
//	0 字节  → 旧实现当作「没有密钥」静默生成新密钥
//	半截内容 → 旧实现直接 sha256 当作密钥使用
//
// 两种情况都没有任何告警，而库里全部 provider_credentials.api_key_enc
// 是用旧密钥加密的 —— 换密钥等于全部永久解不开，PrepareFromStore 逐条
// continue，所有 provider 变成零凭据，/v1 全站 404。
func TestLoadMasterKey_TruncatedFileIsRejected(t *testing.T) {
	cases := []struct {
		name    string
		content string
	}{
		{"空文件（写入被完全截断）", ""},
		{"只剩 5 字符", "abcde"},
		{"44 字符但不是合法 base64", strings.Repeat("!", 44)},
		// 44 字符的 base64url 有 43 个有效位，解出 32 字节；这里少一个 '='
		// 让它解出 31 字节 —— 长度对但内容不是密钥。
		{"长度合法但解出 31 字节", strings.Repeat("A", 42) + "=="},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, KeyFileName)
			if err := os.WriteFile(path, []byte(tc.content), 0o600); err != nil {
				t.Fatalf("seed: %v", err)
			}
			// 显式清掉环境变量，确保走文件路径。
			t.Setenv(DefaultEnvName, "")

			key, generated, weak, err := LoadMasterKey("", dir)
			if err == nil {
				t.Fatalf("损坏的密钥文件必须报错，实际却返回了 key=%x generated=%v weak=%v", key, generated, weak)
			}
			if key != nil {
				t.Fatalf("出错时不应返回任何密钥，实际 %x", key)
			}
			if !strings.Contains(err.Error(), KeyFileName) {
				t.Fatalf("错误信息应指明是哪个文件，实际 %q", err)
			}
		})
	}
}

// TestLoadMasterKey_EmptyFileErrorSuggestsRecovery 确认空文件的报错给出了
// 可操作的补救指引 —— 运维看到「master key unavailable」若不知道下一步，
// 很可能直接删库重建，把一次可恢复的截断变成真丢数据。
func TestLoadMasterKey_EmptyFileErrorSuggestsRecovery(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, KeyFileName), nil, 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	t.Setenv(DefaultEnvName, "")

	_, _, _, err := LoadMasterKey("", dir)
	if err == nil {
		t.Fatal("空文件必须报错")
	}
	// 报错里必须提到环境变量这个恢复路径。
	if !strings.Contains(err.Error(), DefaultEnvName) {
		t.Fatalf("报错应提示用环境变量 %s 指定旧密钥，实际 %q", DefaultEnvName, err)
	}
}

// TestLoadMasterKey_ValidFileIsReused 保证修好的判定逻辑没有误伤正常密钥。
func TestLoadMasterKey_ValidFileIsReused(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(DefaultEnvName, "")

	first, generated, weak, err := LoadMasterKey("", dir)
	if err != nil {
		t.Fatalf("首次生成失败: %v", err)
	}
	if !generated || weak {
		t.Fatalf("首次应报告 generated=true weak=false，实际 generated=%v weak=%v", generated, weak)
	}

	second, generated2, _, err := LoadMasterKey("", dir)
	if err != nil {
		t.Fatalf("二次加载失败: %v", err)
	}
	if generated2 {
		t.Fatal("二次加载不应再生成新密钥")
	}
	if string(first) != string(second) {
		t.Fatal("同一密钥文件两次加载得到不同的推导结果")
	}
}

// TestLoadMasterKey_AtomicWriteLeavesNoTempFiles 确认原子写不留残骸。
func TestLoadMasterKey_AtomicWriteLeavesNoTempFiles(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(DefaultEnvName, "")

	if _, _, _, err := LoadMasterKey("", dir); err != nil {
		t.Fatalf("生成失败: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != KeyFileName {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("目录应只剩 %s 一个文件，实际 %v", KeyFileName, names)
	}
}

// TestLoadMasterKey_EnvKeyOverridesFile 确认环境变量优先级未被破坏：
// 运维换密钥的正规路径就是设环境变量，不能因为文件校验变严而失效。
func TestLoadMasterKey_EnvKeyOverridesFile(t *testing.T) {
	dir := t.TempDir()
	if _, _, _, err := LoadMasterKey("", dir); err != nil {
		t.Fatalf("生成失败: %v", err)
	}
	// 文件保持是自动生成的合法格式，模拟正常场景。
	t.Setenv(DefaultEnvName, "some-user-supplied-passphrase")

	key, generated, _, err := LoadMasterKey("", dir)
	if err != nil {
		t.Fatalf("环境变量路径失败: %v", err)
	}
	if generated {
		t.Fatal("环境变量提供密钥时不应报告 generated")
	}
	if len(key) != 32 {
		t.Fatalf("密钥长度应为 32，实际 %d", len(key))
	}
}
