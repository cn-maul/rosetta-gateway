package crypto

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// master.key 的格式契约：凡是要写这个文件的入口（Go 侧自动生成、任何脚本），
// 写出的内容都必须能被 validKeyFormat 接受，否则网关在下次启动时判定
// 「文件已损坏」并拒绝运行（LoadMasterKey 的文件分支）。
//
// 这条契约曾经被 gateway.ps1 违反过（2026-10-10 修复的 P1）：脚本用
// Get-Random 生成 **64 个 hex 字符** 落盘，而校验要求 44 字符 base64url。
// 症状极隐蔽 —— 脚本自己启动时把密钥塞进环境变量，而环境变量分支
// **不做格式校验**，所以它自己跑得好好的；直到换一种启动方式
// （双击 bin\gateway.exe、docker run 挂载同一个 home、另一个人的 shell）
// 读文件才炸。因此这里既钉住 Go 侧的输出，也钉住那个具体反例。
func TestGenerateKeyIsAcceptedByItsOwnValidator(t *testing.T) {
	key := GenerateKey()
	if !validKeyFormat(key) {
		t.Fatalf("GenerateKey() 产出的密钥过不了自家校验器：len=%d，%q", len(key), key)
	}
	// 长度断言写死，避免将来有人改了 GenerateKey 的编码方式而
	// 意外与 validKeyFormat 的 44/32 假设脱节（脱节 = 老库全部无法启动）。
	if len(key) != 44 {
		t.Fatalf("密钥长度 = %d，validKeyFormat 要求 44", len(key))
	}
	dec, err := base64.URLEncoding.DecodeString(key)
	if err != nil || len(dec) != 32 {
		t.Fatalf("密钥不是 32 字节 base64url：%v", err)
	}
}

// 回归钉：gateway.ps1 曾经写出的 64-hex 格式**必须**被拒绝。
// 任何人再把那段生成逻辑加回脚本时，这条测试立刻红。
func TestValidKeyFormatRejectsLegacyHexKey(t *testing.T) {
	legacyHex := strings.Repeat("a1b2c3d4", 8) // 64 个 hex 字符，旧脚本的产物
	if len(legacyHex) != 64 {
		t.Fatalf("测试素材长度不对：%d", len(legacyHex))
	}
	if validKeyFormat(legacyHex) {
		t.Fatal("64-hex 格式被当成合法密钥接受 —— 网关会在读文件时拒绝启动，" +
			"说明 validKeyFormat 与实际期望格式已经脱节")
	}
}

// 端到端：Go 自动生成 → 落盘 → 重新读取，必须成功且拿到**同一把**密钥。
// 这正是旧脚本留下坏文件后、换启动方式会遇到的那条路径。
func TestGeneratedKeyRoundTripsThroughFile(t *testing.T) {
	dir := t.TempDir()

	first, generated, _, err := LoadMasterKey("ROSETTA_TEST_KEY_UNSET", dir)
	if err != nil {
		t.Fatalf("首次加载: %v", err)
	}
	if !generated {
		t.Fatal("首次加载应当生成新密钥")
	}

	path := filepath.Join(dir, KeyFileName)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读回密钥文件: %v", err)
	}
	if !validKeyFormat(strings.TrimSpace(string(raw))) {
		t.Fatalf("落盘的内容不符合自家格式：%q", string(raw))
	}

	// 第二次加载：必须复用同一把（换了 key = 库里全部凭据永久解不开）。
	second, generated2, _, err := LoadMasterKey("ROSETTA_TEST_KEY_UNSET", dir)
	if err != nil {
		t.Fatalf("二次加载: %v", err)
	}
	if generated2 {
		t.Fatal("已有密钥文件时不应重新生成")
	}
	if string(first) != string(second) {
		t.Fatal("二次加载拿到的不是同一把密钥 —— 存量凭据将无法解密")
	}
}

// 环境变量分支**不**校验格式（那是刻意的：用户手填口令可以是任意长度）。
// 这条测试固定住这个不对称，避免有人日后「顺手统一」，
// 把手填口令的正常路径一起堵死。
func TestEnvVarKeySkipsFormatValidation(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ROSETTA_TEST_KEY_ENV", "short-manual-passphrase")

	key, generated, _, err := LoadMasterKey("ROSETTA_TEST_KEY_ENV", dir)
	if err != nil {
		t.Fatalf("环境变量密钥不应被格式校验拒绝: %v", err)
	}
	if generated {
		t.Fatal("提供了环境变量密钥时不应生成新密钥")
	}
	if len(key) != 32 {
		t.Fatalf("密钥长度 = %d，期望 32（SHA-256 派生）", len(key))
	}
}
