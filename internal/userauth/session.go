package userauth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// 会话令牌的默认有效期。
//
// 8 小时覆盖一个工作日：早上登录、下午下班前忘记登出也不至于中途失效，
// 而第二天开工必须重新登录（局域网网关的暴露面比公网服务小得多，
// 不需要 15 分钟那种紧绷的窗口）。
const defaultTTL = 8 * time.Hour

// 签名密钥的环境变量名。留空则**不启用**用户会话。
const SecretEnvName = "ROSETTA_GW_SESSION_SECRET"

// Errors 是会话校验的失败原因，供HTTP 层映射状态码。
var (
	ErrNoToken      = errors.New("no session token")
	ErrTokenInvalid = errors.New("session token is invalid")
	ErrTokenExpired = errors.New("session token has expired")
	// ErrStaleVersion 表示 token 里的 auth_version 低于数据库当前值 ——
	// 密码已改、账号被禁用或角色被调整，旧 token 随之失效。
	ErrStaleVersion = errors.New("session has been invalidated")
)

// Session 是解出的会话载荷。
type Session struct {
	UserID string
	// AuthVersion 是签发时的版本。校验时必须与数据库当前值一致。
	AuthVersion int64
	Role        string
	Username    string
	IssuedAt    time.Time
	ExpiresAt   time.Time
}

// Manager 签发与校验会话令牌。
//
// # 为什么用 HMAC 签名而不是服务端会话表
//
// 局域网单进程网关：签发/校验都是纯 CPU 的 HMAC 运算，零外部依赖。
// 服务端会话表方案需要额外存储，且多一个「表被清空 = 所有人被登出」的故障点。
//
// 代价是**JWT 无法被服务端主动吊销** —— 但那正是 auth_version 存在的原因：
// 改密码/禁用账号时把 users.auth_version 加一，所有旧 token 立即作废。
// 这比查一张黑名单表更可靠（不依赖进程内状态，重启后依然有效）。
type Manager struct {
	secret []byte
	ttl    time.Duration
}

// NewManager 从环境变量读签名密钥并构造 Manager。
//
// 密钥解析顺序：环境变量 → 持久化文件 → **自动生成并写入**。
// 缺密钥时返回 nil —— 调用方应当据此**不启用**用户会话，
// 而不是退化到「用空密钥签名」（那等于任何人都能伪造 token）。
//
// secretPath 为空时不做任何持久化（测试与「一次性容器」场景）。
func NewManager(secretPath string) (*Manager, error) {
	raw := strings.TrimSpace(os.Getenv(SecretEnvName))
	if raw == "" && secretPath != "" {
		loaded, generated, err := LoadOrCreateSecret(secretPath)
		if err != nil {
			return nil, err
		}
		raw = loaded
		if generated {
			// 必须告知路径：悄悄生成一把新密钥 = 所有人手里的旧会话
			// 在下一次重启后集体失效，而界面上看不出任何原因。
			fmt.Fprintf(os.Stderr, "[session] generated a new session secret at %s; "+
				"back it up with master.key — losing it invalidates all sessions\n", secretPath)
		}
	}
	if raw == "" {
		return nil, nil
	}
	// 密钥强度下限 32 字符（约 256bit）。短密钥让 HMAC-SHA256 的安全性
	// 退化到暴力猜测，而它保护的是「谁能进管理后台」。
	if len(raw) < 32 {
		return nil, fmt.Errorf("%s 至少需要 32 个字符（当前 %d）", SecretEnvName, len(raw))
	}
	// 长度检查过了但熵可能不足（例如全 a）。这里不做字典检测 ——
	// 那属于「猜用户会不会偷懒」，而 key 是部署者自己配的。
	return &Manager{secret: []byte(raw), ttl: defaultTTL}, nil
}

// SecretFileName 是会话签名密钥的默认文件名，与 master.key 同级。
const SecretFileName = "session_secret"

// secretBytes 是生成密钥的字节数。32 字节 = 256bit，base64 后 44 字符，
// 稳稳超过 32 字符的强度下限。
const secretBytes = 32

// LoadOrCreateSecret 读取会话密钥；文件不存在时生成并原子写入。
//
// 第二个返回值表示本次是否新建了文件。
//
// # 为什么不放进数据库
//
// 与 master.key 同理：它是**身份**的一部分，删库重建不该把所有人
// 踢下线。但也不在 users 表里 —— 它不是某个人的凭据，是整个部署的。
//
// # 为什么必须原子写
//
// 写到一半崩溃会留下一个短密钥文件，重启时被 32 字符下限拒绝，
// 症状是「网关起不来」而不是「密钥不对」。先写临时文件再 rename。
func LoadOrCreateSecret(path string) (secret string, generated bool, err error) {
	data, readErr := os.ReadFile(path)
	if readErr == nil {
		s := strings.TrimSpace(string(data))
		if s == "" {
			return "", false, fmt.Errorf("会话密钥文件 %s 为空：删除该文件后重启可自动重建", path)
		}
		return s, false, nil
	}
	if !os.IsNotExist(readErr) {
		// 权限不足、目录不存在等。**不猜**：报出来让部署者自己修，
		// 静默退回「无会话」= 整个后台登不进去，且看不出原因。
		return "", false, fmt.Errorf("读取会话密钥 %s 失败: %w", path, readErr)
	}

	buf := make([]byte, secretBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", false, fmt.Errorf("生成会话密钥失败: %w", err)
	}
	s := base64.RawURLEncoding.EncodeToString(buf)
	if err := writeSecretFile(path, []byte(s)); err != nil {
		return "", false, err
	}
	return s, true, nil
}

// writeSecretFile 原子写入密钥文件：先写临时文件再 rename。
func writeSecretFile(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("创建密钥目录 %s 失败: %w", dir, err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("写入临时密钥文件失败: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("落盘会话密钥失败: %w", err)
	}
	return nil
}

// Enabled 报告用户会话是否已启用。
func (m *Manager) Enabled() bool { return m != nil && len(m.secret) > 0 }

// TTL 返回令牌有效期。
//
// ttl == 0 时回落到 defaultTTL。**负值不回落** —— 负 TTL 是「签发即过期」，
// 那是测试构造过期令牌的正当手段；把它静默改成 8 小时会让过期逻辑
// 在测试里测不到。
func (m *Manager) TTL() time.Duration {
	if m == nil || m.ttl == 0 {
		return defaultTTL
	}
	return m.ttl
}

type claims struct {
	UserID      string `json:"uid"`
	AuthVersion int64  `json:"av"`
	Role        string `json:"role"`
	Username    string `json:"usr"`
	jwt.RegisteredClaims
}

// Issue 签发一个会话令牌。
//
// 只允许 HS256 —— 显式写死alg 是 JWT 最容易被利用的地方：
// 若验证端接受 "none" 或把 RS256 的公钥当 HMAC 密钥，攻击者即可伪造。
func (m *Manager) Issue(u *UserClaims) (string, time.Time, error) {
	if !m.Enabled() {
		return "", time.Time{}, errors.New("session manager not enabled")
	}
	if u == nil || u.UserID == "" {
		return "", time.Time{}, errors.New("user id is required")
	}
	now := time.Now()
	exp := now.Add(m.TTL())
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims{
		UserID:      u.UserID,
		AuthVersion: u.AuthVersion,
		Role:        u.Role,
		Username:    u.Username,
		RegisteredClaims: jwt.RegisteredClaims{
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(exp),
			// 没有设NotBefore：签发即生效。
			Issuer:  "rosetta-gateway",
			Subject: u.UserID,
			ID:      randomID(),
		},
	})
	signed, err := tok.SignedString(m.secret)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("sign token: %w", err)
	}
	return signed, exp, nil
}

// Verify 校验令牌签名与有效期，并比对 auth_version。
//
// currentVersion 由调用方从数据库取（-1 表示查不到该用户）。
// 这里刻意不自己查库：让数据访问留在调用方，避免认证逻辑与存储耦合。
func (m *Manager) Verify(token string, currentVersion int64) (*Session, error) {
	if !m.Enabled() {
		return nil, ErrTokenInvalid
	}
	if strings.TrimSpace(token) == "" {
		return nil, ErrNoToken
	}

	parsed, err := jwt.ParseWithClaims(token, &claims{},
		func(t *jwt.Token) (any, error) {
			// 二次确认签名算法：ParseWithClaims 会先用keyfunc 解析header，
			// 若这里不校验 alg，攻击者可以把 alg 改成 none 或 HS256/RS256 混淆。
			if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
				return nil, fmt.Errorf("unexpected signing method %v", t.Header["alg"])
			}
			return m.secret, nil
		},
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
	)
	if err != nil {
		if errors.Is(err, jwt.ErrTokenExpired) {
			return nil, ErrTokenExpired
		}
		return nil, ErrTokenInvalid
	}
	c, ok := parsed.Claims.(*claims)
	if !ok || !parsed.Valid {
		return nil, ErrTokenInvalid
	}
	if c.Subject != c.UserID {
		// 冗余校验：subject 与 uid 必须一致。不一致说明是手工拼装的 token。
		return nil, ErrTokenInvalid
	}

	// auth_version 栅栏：改密码/禁用/改角色后，旧 token 立即作废。
	// currentVersion < 0 表示用户已不存在（被删除）。
	if currentVersion < 0 || c.AuthVersion != currentVersion {
		return nil, ErrStaleVersion
	}

	exp := time.Time{}
	if c.ExpiresAt != nil {
		exp = c.ExpiresAt.Time
	}
	iat := time.Time{}
	if c.IssuedAt != nil {
		iat = c.IssuedAt.Time
	}
	return &Session{
		UserID:      c.UserID,
		AuthVersion: c.AuthVersion,
		Role:        c.Role,
		Username:    c.Username,
		IssuedAt:    iat,
		ExpiresAt:   exp,
	}, nil
}

// Peek 只解析令牌、**不比对 auth_version**，用于「先认出是谁，再去查库」的场景。
//
// 为什么需要它：Verify 需要 currentVersion，而这个值只在数据库里。
// 中间件若直接调 Verify 就得先知道 user_id —— 只能靠 Peek 拿到。
// 顺序固定为 Peek → 查库 → Verify，不接受「先 Verify 再校验」的写法
// （那意味着拿令牌里的旧版本号去比，越权风险）。
func (m *Manager) Peek(token string) (*UserClaims, error) {
	if !m.Enabled() {
		return nil, ErrTokenInvalid
	}
	if strings.TrimSpace(token) == "" {
		return nil, ErrNoToken
	}

	parsed, err := jwt.ParseWithClaims(token, &claims{}, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method %v", t.Header["alg"])
		}
		return m.secret, nil
	}, jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}))
	if err != nil {
		if errors.Is(err, jwt.ErrTokenExpired) {
			return nil, ErrTokenExpired
		}
		return nil, ErrTokenInvalid
	}
	c, ok := parsed.Claims.(*claims)
	if !ok || !parsed.Valid || c.Subject != c.UserID {
		return nil, ErrTokenInvalid
	}
	return &UserClaims{
		UserID:      c.UserID,
		AuthVersion: c.AuthVersion,
		Role:        c.Role,
		Username:    c.Username,
	}, nil
}

// UserClaims 是签发令牌所需的最小信息集。
type UserClaims struct {
	UserID      string
	AuthVersion int64
	Role        string
	Username    string
}

func randomID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// Fingerprint 返回令牌的身份指纹，用于日志而不泄漏令牌本身。
func (s *Session) Fingerprint() string {
	if s == nil || s.UserID == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(s.UserID))
	return base64.RawURLEncoding.EncodeToString(sum[:6])
}
