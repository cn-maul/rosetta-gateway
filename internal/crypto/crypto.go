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
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"unicode"
	"unicode/utf8"
)

const (
	// KeyFileName 是主密钥在状态根目录（home）下的文件名。
	// 密钥独立于数据库存放：删库不影响解密能力，丢密钥不连累账号体系。
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
// weak 表示密钥材料熵偏低（< 32 字符）——大概率是用户手填的口令而非随机密钥。
// 推导仍是 SHA-256（改推导会锁死存量凭据，见 AUDIT 2026-10-04 的评估），
// 调用方应当 WARN 提醒改用自动生成的 master.key。
func LoadMasterKey(envName, dir string) (key []byte, generated bool, weak bool, err error) {
	if envName == "" {
		envName = DefaultEnvName
	}
	if raw := strings.TrimSpace(os.Getenv(envName)); raw != "" {
		sum := sha256.Sum256([]byte(raw))
		return sum[:], false, WeakMaterial(raw), nil
	}
	if strings.TrimSpace(dir) == "" {
		return nil, false, false, ErrNoMasterKey
	}

	path := filepath.Join(dir, KeyFileName)
	content, readErr := os.ReadFile(path)
	switch {
	case readErr == nil:
		raw := strings.TrimSpace(string(content))
		if raw == "" {
			// 文件存在但为空 —— 只可能是「上次写被截断」（旧实现用 O_TRUNC
			// 非原子写，进程在写入中途被 kill 就会留下 0 字节）。绝不能静默
			// 当作「没有密钥」而重新生成一把：库里全部凭据都是用旧密钥加密的，
			// 换密钥等于全部永久解不开，PrepareFromStore 会逐条 continue，
			// 结果是所有 provider 变成零凭据、/v1 全站 404，且无任何告警。
			return nil, false, false, fmt.Errorf(
				"%s 存在但内容为空：上次写入可能被截断。用旧密钥（环境变量 %s）启动；"+
					"若确已丢失，需手动删除该文件并重新配置全部上游凭据", path, envName)
		}
		if !validKeyFormat(raw) {
			// 自动生成的密钥恒为 base64url(32B) = 44 字符；环境变量可以是任意
			// 长度。文件路径只走这条分支，所以格式不符 = 文件损坏或被手工截断。
			return nil, false, false, fmt.Errorf(
				"%s 内容格式非法（长度 %d，自动生成的密钥应为 44 字符 base64url）："+
					"文件很可能已损坏。库里凭据依赖此密钥，请勿删除；"+
					"若确需更换，先用环境变量 %s 指定旧密钥启动", path, len(raw), envName)
		}
		sum := sha256.Sum256([]byte(raw))
		return sum[:], false, WeakMaterial(raw), nil
	case !os.IsNotExist(readErr):
		return nil, false, false, fmt.Errorf("read %s: %w", path, readErr)
	}

	// 自动生成的密钥是 32 字节随机数的 base64url 编码（44 字符），永远不会 weak。
	raw := GenerateKey()
	if err := writeKeyAtomic(path, []byte(raw+"\n")); err != nil {
		return nil, false, false, fmt.Errorf("write %s: %w", path, err)
	}
	sum := sha256.Sum256([]byte(raw))
	return sum[:], true, false, nil
}

// validKeyFormat 报告一段密钥材料是否符合自动生成的格式。
//
// 用于区分「用户手填的环境变量口令」与「本程序生成的 master.key」：
// 后者恒为 base64.URLEncoding(32 字节) = 44 字符。文件路径上的内容若不满足，
// 说明文件被截断或损坏 —— 此时静默采纳等于拿半截密钥去解密，
// 而重新生成则让全部存量凭据永久不可解。两种后果都不可接受，直接报错。
func validKeyFormat(raw string) bool {
	if len(raw) != 44 {
		return false
	}
	dec, err := base64.URLEncoding.DecodeString(raw)
	return err == nil && len(dec) == 32
}

// writeKeyAtomic 原子写密钥文件：临时文件 → fsync → rename → fsync 目录。
//
// 旧实现用 os.WriteFile（O_TRUNC），进程在写入中途被 kill / 断电会留下
// 0 字节或半截文件，而下一次启动会静默采纳 —— 参见调用处的注释。
// userauth 的 session_secret 落盘也是 tmp → rename，理由相同。
func writeKeyAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() {
		if tmp != "" {
			os.Remove(tmp)
		}
	}()
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	// 不 Sync 就 Close 的话，rename 之后的崩溃仍可能让文件系统回放出
	// 空内容 —— 崩溃一致性要求数据先落盘。
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	tmp = "" // 已生效，defer 不再删
	// 目录项本身也要落盘，否则 rename 可能在崩溃后丢失。
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

// plaintextFallbackWarned 节流「命中明文回退」的告警：解密发生在每次池重建
// 与管理面读凭据时，逐条打会刷爆日志，而「库里存在明文凭据」这一事实
// 说一遍就够。进程级一次性，代价是重启后才会再提醒 —— 可接受。
var plaintextFallbackWarned atomic.Bool

// DecryptWithFallback 用 masterKey 解密。masterKey 为空时直接按明文返回；
// 解密失败时，只有当数据看起来是可打印文本才退化为明文
// —— 兼容早期「无主密钥 → 明文落库」的历史数据，同时避免把密文当 API Key 发出去。
//
// 兼容不等于无声：命中文本回退说明库里有一条**未加密**的凭据（主密钥存在
// 却解不开 = 数据不是本密钥加密的，而是明文落库的历史行/手工插入行）。
// 此时数据库备份等于该凭据泄露，且这条数据永远不会自己升级成密文 ——
// 必须 WARN 让运维知道去重新保存一次凭据完成加密迁移。日志只记长度，
// 不记内容：那是明文密钥本身。
func DecryptWithFallback(data, masterKey []byte) (string, error) {
	if len(masterKey) == 0 {
		return string(data), nil
	}
	plain, err := Decrypt(data, masterKey)
	if err == nil {
		return string(plain), nil
	}
	if looksLikeText(data) {
		if plaintextFallbackWarned.CompareAndSwap(false, true) {
			slog.Warn("credential accepted as PLAINTEXT by fallback decryption; "+
				"database backups expose it and it will never self-encrypt — "+
				"re-save the credential to migrate it into encrypted form",
				"bytes", len(data))
		}
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

// WeakMaterial 报告一段密钥材料的熵是否低到值得告警。
// 阈值 32 字符：自动生成的 master.key 是 64 hex；手填的短口令（"123456"、
// 公司名拼音）落库加密的是上游付费 API Key，值得被离线字典爆破。
// 只作告警依据，不改变推导 —— 见 AUDIT 2026-10-04：改推导（KDF）会让
// 存量密文全部解不开，锁死代价高于收益。
func WeakMaterial(raw string) bool {
	return len(strings.TrimSpace(raw)) < 32
}
