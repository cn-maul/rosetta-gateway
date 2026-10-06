// Package userauth 处理多用户的口令校验与哈希编码。
//
// 统一认证后（2026-10-06），users 表是系统里**唯一**的身份来源：
// 管理员只是 role='admin' 的普通用户，所有口令——不分角色——
// 都由本包哈希与校验。曾经与它并存、并号称「密码规则要与它保持一致」的
// internal/adminauth（admin_auth.json + admin_token 旁路）已整体删除。
//
// # 为什么密码进了数据库（曾经的包注释反对这样做）
//
// 反对理由是「gateway.db 可随手删，密码放进去等于删库 = 锁死门外」。
// 多用户把身份、配额、key 归属全部绑在同一张库上之后，这个理由不再成立：
// 删库丢掉的本来就不只是密码，而是整套账号体系。
// 统一认证明确接受了「删库 = 失明」——重启后重新走首次设置密码
// （/admin/api/bootstrap），代价换来了单一通道与完整的改密审计。
// 需要永久保全的只剩两把密钥（master.key / session_secret），它们独立于库存放。
package userauth

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode"
)

// ErrWeakPassword 表示新密码未通过最低强度校验。
//
// 门槛：至少 8 字符 + 至少两类字符（字母/数字/符号）。
// 用户口令的威胁面比管理员口令小（拿不到上游凭据），
// 但仍是在线爆破的目标，登录限速只是减速带。
var ErrWeakPassword = errors.New("密码长度至少为 8 位，且需包含字母/数字/符号中的至少两类")

// KDF 参数。整套口令体系只有这一处定义，不存在第二套要「保持一致」的参数。
const (
	kdfIterations = 210_000
	kdfKeyLen     = 32
	saltLen       = 16
	// algoName 显式标出算法，不需要靠「哈希是否恰好 64 位 hex」去猜 ——
	// 那种启发式会让一个恰好 64 字符的明文口令被误判成哈希。
	algoName = "pbkdf2-sha256"
)

// ValidatePassword 校验新口令的最低强度。只作用于「设置新口令」，
// 不影响既有口令的校验路径（弱口令一旦在用，不强制用户改）。
func ValidatePassword(p string) error {
	runes := []rune(p)
	if len(runes) < 8 {
		return ErrWeakPassword
	}
	var hasLower, hasUpper, hasDigit, hasOther bool
	for _, r := range runes {
		switch {
		case unicode.IsLower(r):
			hasLower = true
		case unicode.IsUpper(r):
			hasUpper = true
		case unicode.IsDigit(r):
			hasDigit = true
		default:
			hasOther = true
		}
	}
	classes := 0
	for _, h := range []bool{hasLower, hasUpper, hasDigit, hasOther} {
		if h {
			classes++
		}
	}
	if classes < 2 {
		return ErrWeakPassword
	}
	return nil
}

// HashPassword 生成自描述的哈希串，格式：
//
//	pbkdf2-sha256$<iter>$<salt-b64>$<hash-b64>
//
// 自描述是刻意的：iter 随存量编码一同保存，将来调参只需解旧串用新参数重算，
// 不需要「猜这个串是哪个版本产生的」（adminauth 早期用 `len==64 && isHex`
// 猜明文/哈希，代价是一个恰好 64 字符的明文令牌永久锁死）。
func HashPassword(password string) (string, error) {
	return hashWithIterations(password, kdfIterations)
}

// hashWithIterations 按指定迭代数生成编码。参数化是为了将来调参时
// 仍能解旧编码（解码端从串里读iter，而不是用常量），也供测试构造低迭代串。
func hashWithIterations(password string, iter int) (string, error) {
	if iter <= 0 {
		return "", errors.New("迭代次数必须为正数")
	}
	if err := ValidatePassword(password); err != nil {
		return "", err
	}
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("生成随机盐失败: %w", err)
	}
	key, err := pbkdf2.Key(sha256.New, password, salt, iter, kdfKeyLen)
	if err != nil {
		return "", fmt.Errorf("派生密码哈希失败: %w", err)
	}
	return fmt.Sprintf("%s$%d$%s$%s", algoName, iter,
		base64.StdEncoding.EncodeToString(salt),
		base64.StdEncoding.EncodeToString(key)), nil
}

// VerifyPassword 校验口令是否与编码相符。
//
// 恒定时间比较，避免通过响应时间逐字节爆破。格式非法一律返回 false ——
// 不区分「格式错」与「密码错」，免得给攻击者一个格式探测的oracle。
func VerifyPassword(encoded, password string) bool {
	if encoded == "" || password == "" {
		return false
	}
	parts := strings.Split(encoded, "$")
	if len(parts) != 4 || parts[0] != algoName {
		return false
	}
	iter, err := strconv.Atoi(parts[1])
	if err != nil || iter <= 0 {
		return false
	}
	salt, err := base64.StdEncoding.DecodeString(parts[2])
	if err != nil {
		return false
	}
	want, err := base64.StdEncoding.DecodeString(parts[3])
	if err != nil || len(want) == 0 {
		return false
	}
	got, err := pbkdf2.Key(sha256.New, password, salt, iter, len(want))
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(got, want) == 1
}
