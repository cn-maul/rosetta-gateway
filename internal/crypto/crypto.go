package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	// KeyFileName 是主密钥在可执行文件同级的文件名。
	// 与 admin_auth.json 同理：加密身份独立于业务数据，删库不影响解密能力。
	KeyFileName = "master.key"
	// DefaultEnvName 是主密钥环境变量名。
	DefaultEnvName = "ROSETTA_GW_MASTER_KEY"
)

var (
	ErrNoMasterKey = errors.New("master key not configured")
)

// LoadMasterKey 解析凭据加密主密钥，优先级：
//  1. 环境变量 envName（留空时用 ROSETTA_GW_MASTER_KEY）
//  2. dir/master.key 文件
//  3. 两者都没有则生成一个写入 dir/master.key，后续启动自动复用
//
// 第 3 条是必须的：早期只认环境变量，导致「双击 exe 启动」与「gateway.ps1 启动」
// 走两条路——前者无密钥、凭据明文落库，后者有密钥，同一份库在两种启动方式下
// 互相解不开。密钥落盘后两种方式共用同一把。
// generated 表示本次是否新生成密钥（调用方可据此打日志）。
func LoadMasterKey(envName, dir string) (key []byte, generated bool, err error) {
	if envName == "" {
		envName = DefaultEnvName
	}
	if raw := strings.TrimSpace(os.Getenv(envName)); raw != "" {
		sum := sha256.Sum256([]byte(raw))
		return sum[:], false, nil
	}
	if strings.TrimSpace(dir) == "" {
		return nil, false, ErrNoMasterKey
	}

	path := filepath.Join(dir, KeyFileName)
	if content, readErr := os.ReadFile(path); readErr == nil {
		if raw := strings.TrimSpace(string(content)); raw != "" {
			sum := sha256.Sum256([]byte(raw))
			return sum[:], false, nil
		}
	}

	raw := GenerateKey()
	if err := os.WriteFile(path, []byte(raw+"\n"), 0o600); err != nil {
		return nil, false, fmt.Errorf("write %s: %w", path, err)
	}
	sum := sha256.Sum256([]byte(raw))
	return sum[:], true, nil
}

// DecryptWithFallback 用 masterKey 解密。masterKey 为空时直接按明文返回；
// 解密失败时，只有当数据看起来是可打印文本才退化为明文
// —— 兼容早期「无主密钥 → 明文落库」的历史数据，同时避免把密文当 API Key 发出去。
func DecryptWithFallback(data, masterKey []byte) (string, error) {
	if len(masterKey) == 0 {
		return string(data), nil
	}
	plain, err := Decrypt(data, masterKey)
	if err == nil {
		return string(plain), nil
	}
	if looksLikeText(data) {
		return string(data), nil
	}
	return "", err
}

// looksLikeText 判断字节序列是否像可打印文本（用于区分明文与密文）。
func looksLikeText(b []byte) bool {
	if len(b) == 0 || !utf8.Valid(b) {
		return false
	}
	for _, r := range string(b) {
		if r == utf8.RuneError || (!unicode.IsPrint(r) && r != '\n' && r != '\t') {
			return false
		}
	}
	return true
}

func Encrypt(plaintext []byte, masterKey []byte) ([]byte, error) {
	block, err := aes.NewCipher(masterKey)
	if err != nil {
		return nil, err
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}

	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}

	ciphertext := gcm.Seal(nonce, nonce, plaintext, nil)
	return ciphertext, nil
}

func Decrypt(ciphertext []byte, masterKey []byte) ([]byte, error) {
	block, err := aes.NewCipher(masterKey)
	if err != nil {
		return nil, err
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}

	nonceSize := gcm.NonceSize()
	if len(ciphertext) < nonceSize {
		return nil, errors.New("ciphertext too short")
	}

	nonce, ciphertext := ciphertext[:nonceSize], ciphertext[nonceSize:]
	return gcm.Open(nil, nonce, ciphertext, nil)
}

func MaskKey(plainKey string) string {
	if len(plainKey) <= 8 {
		return "****"
	}
	return plainKey[:4] + "..." + plainKey[len(plainKey)-4:]
}

func GenerateKey() string {
	b := make([]byte, 32)
	rand.Read(b)
	return base64.URLEncoding.EncodeToString(b)
}
