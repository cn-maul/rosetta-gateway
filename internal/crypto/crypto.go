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
	"runtime"
	"strings"
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
// 推导仍是 SHA-256（改推导会锁死存量凭据，见 WeakMaterial 的注释），
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
		key, err := LoadKeyFile(path)
		if err != nil {
			// 文件存在但为空/格式非法 —— 只可能是「上次写被截断」或损坏。
			// 绝不能静默当作「没有密钥」而重新生成一把：库里全部凭据都是用
			// 旧密钥加密的，换密钥等于全部永久解不开。
			return nil, false, false, fmt.Errorf(
				"%s：%w（用旧密钥（环境变量 %s）启动；若确已丢失，需手动删除该文件并重新配置全部上游凭据）",
				path, err, envName)
		}
		// 既有文件的权限检查（2026-10-21 修 A10-P1-3）。
		//
		// writeKeyAtomic 写出来的是 0600，但**既有部署**的文件可能是
		// 更早的版本写的（0644），或者被运维/备份工具复制时放宽过。
		// 「密钥文件是安全的」这句话只对**新建**的文件成立 —— 而
		// master.key 保护着全部上游 API key，它被同机其他账号读到 =
		// 全部凭据可解密。
		//
		// 处置：**收紧**而不是告警。理由与目录不同 —— 这个文件是我们
		// 自己的产物、只被本进程读、且收紧它不会打断任何外部读者
		// （外部读者读这个文件本来就是要紧的）。改权限失败才告警。
		warnWeakPermissions(path)
		return key, false, WeakMaterial(strings.TrimSpace(string(content))), nil
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

// LoadKeyFile 读取并校验一个 master.key 文件，返回运行时使用的 32 字节密钥。
//
// 迁移工具必须与网关走同一条校验路径：旧工具只查 `len(raw)==44`，会把
// 「44 个字母但 base64 解不出 32 字节」的损坏文件当成合法密钥使用，
// 最终表现为凭据一条都解不开而错误信息指向别处。这里把格式校验与
// sha256 派生收敛成唯一实现（审计 A6-P1-4）。
func LoadKeyFile(path string) ([]byte, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	raw := strings.TrimSpace(string(content))
	if raw == "" {
		return nil, errors.New("内容为空：上次写入可能被截断")
	}
	if !validKeyFormat(raw) {
		return nil, fmt.Errorf(
			"内容格式非法（长度 %d，自动生成的密钥应为 44 字符 base64url）",
			len(raw))
	}
	sum := sha256.Sum256([]byte(raw))
	return sum[:], nil
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
// 阈值 32 字符：自动生成的 master.key 是 44 字符 base64url（熵充足）；
// 手填的短口令（"123456"、公司名拼音）落库加密的是上游付费 API Key，
// 值得被离线字典爆破。
// 只作告警依据，不改变推导 —— 改推导（KDF）会让
// 存量密文全部解不开，锁死代价高于收益。
func WeakMaterial(raw string) bool {
	return len(strings.TrimSpace(raw)) < 32
}

// warnWeakPermissions 把一个密钥文件的权限收紧到 0600。
//
// # 为什么是「收紧」而不是「告警」
//
// 对 master.key / session_secret 这类文件：它们是**本进程独占读取**的产物，
// 收紧权限不会打断任何正当的外部读者（外部读者读它本来就是要紧的事）。
// 而放着 0644 不管 = 同机任何账号都能拷走它 —— master.key 泄漏 = 全部
// 上游凭据可解密。
//
// 与目录的差别（见 store.Open 的注释）：目录可能有意让备份服务读，
// 密钥文件不会。
//
// # Windows
//
// NTFS 的权限模型是 ACL，Chmod 只能改只读位 —— 强改只读位会让后续
// 写入失败。所以 Windows 上跳过整个检查。
func warnWeakPermissions(path string) {
	if runtime.GOOS == "windows" {
		return
	}
	info, err := os.Stat(path)
	if err != nil {
		return
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		if err := os.Chmod(path, 0o600); err != nil {
			slog.Warn("cannot tighten secret file permissions; "+
				"other users on this host may be able to read it",
				"path", path, "mode", fmt.Sprintf("%04o", perm), "error", err)
			return
		}
		slog.Warn("tightened secret file permissions (was readable by other users)",
			"path", path, "was", fmt.Sprintf("%04o", perm))
	}
}
