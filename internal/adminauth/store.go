// Package adminauth 持有管理后台的凭据：加载、校验、更新。
//
// # 为什么存在运行时状态
//
// 旧实现把 config.json 里的 admin_token 在启动时拷贝成一个不可变字符串，
// 传给 net/http 中间件的闭包。后台「设置密码」只写配置文件、进程内看不到，
// 于是新密码在重启前永远无效 —— 前端表现为「输入密码 → 401 → 再输入 → 再 401」
// 的无限循环。本包把凭据变成带读写锁的运行时状态，Set 成功后立即生效。
//
// # 为什么落盘在可执行文件同级，而不是数据库
//
// gateway.db 在本项目里是「可丢弃的运行时数据」：删库重建是常规操作
// （凭据加密密钥丢失时就是这么处理的）。管理员密码是身份凭据，
// 放进可被随手删掉的库里等于「删库 = 把自己锁在门外」。
// 与 master.key 同理，身份状态必须独立于业务数据。
//
// # 为什么不复用 config.json 的 admin_token 字段
//
// config.json 是运维手写的引导配置，程序回写它会丢掉注释与字段顺序；
// 且 admin_token 语义是「运维引导用的静态令牌」，与「用户设置的管理密码」
// 混用会让两边的判定逻辑互相污染 —— 旧实现不得不用
// `len(token) == 64` 来猜「这串到底是明文还是哈希」，一个恰好 64 字符的
// 明文令牌就会被误判成哈希而永久锁死。两个概念，两个存储。
//
// admin_token 仍然被支持，作为「用户尚未设置密码」时的兜底凭据（见 fallback）。
package adminauth

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"unicode"
)

const (
	// FileName 是凭据文件名，位于可执行文件同级目录。
	FileName = "admin_auth.json"

	// kdfIterations 是 PBKDF2-HMAC-SHA256 的迭代次数。
	// 单机网关的登录频率极低，取 21 万次（OWASP 对 PBKDF2-SHA256 的下限建议量级），
	// 单次校验约几十毫秒，对交互无感，对离线爆破则是数万倍的成本放大。
	kdfIterations = 210_000

	kdfKeyLen = 32
	saltLen   = 16
)

// ErrWeakPassword 表示新密码未通过最低强度校验。
//
// 强度门槛（2026-10-04 从 6 位上调）：至少 8 字符 + 至少两类字符
// （小写/大写/数字/符号）。理由：管理密码是在线爆破的目标（登录限速只是
// 减速带），PBKDF2 只防离线；内网部署不等于低价值目标 —— 网关背后是
// 所有上游的付费凭据。已在用的弱密码不受影响（校验只在 Set 时做），
// 换密码时自然收紧。
var ErrWeakPassword = errors.New("密码长度至少为 8 位，且需包含字母/数字/符号中的至少两类")

// validatePassword 是新密码的最低强度门槛。只约束新设置的密码，
// 不影响既有凭据的校验路径。
func validatePassword(p string) error {
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

// Credential 是用户设置的管理密码经 KDF 派生后的持久化形态。
// 明文密码不落盘、不进内存缓存，只在校验的瞬间存在。
type Credential struct {
	Version int    `json:"version"`
	Algo    string `json:"algo"` // 目前恒为 "pbkdf2-sha256"
	Iter    int    `json:"iter"`
	Salt    string `json:"salt"` // base64(std)
	Hash    string `json:"hash"` // base64(std)
}

// Store 是管理后台凭据的运行时视图，可并发读写。
type Store struct {
	mu   sync.RWMutex
	path string
	cred *Credential
	// fallback 来自 config.json 的 admin_token（明文）或 ADMIN_TOKEN 环境变量。
	// 仅在用户从未设置过密码时生效，用于平滑迁移与应急恢复。
	fallback string
	// locked 非 nil 表示凭据文件存在但不可用（读不了 / 格式错）。见 NewLocked。
	locked error
}

// Open 加载（或惰性创建）凭据存储。
//
// path 为凭据文件路径；fallback 为 config.json 的 admin_token，可为空。
// 文件不存在不是错误：此时视为「尚未设置密码」，由 fallback 决定是否放行。
//
// 返回的错误表示「文件在，但不可用」（读不了或格式错）。调用方**不应该**因此拒绝启动：
// /v1 数据面根本不读管理凭据，为一份坏掉的管理凭据把全部转发服务拖死不成比例。
// 正确的降级是 NewLocked —— 后台进不去，但转发照跑。
func Open(path, fallback string) (*Store, error) {
	s := &Store{path: path, fallback: fallback}

	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("读取凭据文件: %w", err)
	}

	var c Credential
	if err := json.Unmarshal(data, &c); err != nil {
		// 文件损坏不能当作「无密码」静默放行，否则一个字节的损坏就等于后台失守。
		return nil, fmt.Errorf("凭据文件 %s 格式错误（可删除该文件后重启，改用 config 的 admin_token 登录）: %w", path, err)
	}
	if c.Algo != "pbkdf2-sha256" || c.Hash == "" || c.Salt == "" {
		return nil, fmt.Errorf("凭据文件 %s 内容无效：algo=%q", path, c.Algo)
	}
	s.cred = &c
	return s, nil
}

// NewLocked 返回一个「拒绝一切」的 Store，用于凭据文件损坏时的降级。
//
// 三种状态的边界必须分清楚：
//   - 文件**不存在** → 正常状态（还没设过密码），由 fallback 决定放行 → Open。
//   - 文件**存在且可用** → 正常校验 → Open。
//   - 文件**存在但坏了** → 既不能当「没密码」放行（一个字节的损坏 = 后台失守），
//     也不该拒绝启动（/v1 数据面不读它，却会跟着一起死）。所以锁定：
//     所有管理请求一律拒绝，理由通过 LockedError 透给前端，并指明恢复方法。
func NewLocked(path string, cause error) *Store {
	return &Store{path: path, locked: cause}
}

// LockedError 报告 Store 是否处于锁定态，以及原因（nil 表示正常）。
func (s *Store) LockedError() error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.locked
}

// Path 返回凭据文件的落盘路径。
func (s *Store) Path() string { return s.path }

// HasCredential 报告是否已经配置了任何可用凭据（用户密码或 config 兜底令牌）。
// 前端的 has_password 与「首次设置」判定都基于它。
func (s *Store) HasCredential() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	// 锁定态一律视为「已有凭据」。否则 password/set 的引导窗口会向所有人敞开 ——
	// 一个损坏的凭据文件就变成了「后台任人接管」，比拒绝服务严重得多。
	if s.locked != nil {
		return true
	}
	return s.cred != nil || s.fallback != ""
}

// HasUserPassword 报告是否已经由用户在后台设置过密码。
// 与 HasCredential 的区别：只看凭据文件，不含 config 兜底。
func (s *Store) HasUserPassword() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cred != nil
}

// Verify 校验一个明文令牌/密码。
//
// 用户设置的密码优先；未设置时才回退到 config 的 admin_token。
// 两边都用恒定时间比较，避免通过响应时间侧信道逐字节爆破。
func (s *Store) Verify(token string) bool {
	if token == "" {
		return false
	}

	s.mu.RLock()
	cred := s.cred
	fallback := s.fallback
	locked := s.locked
	s.mu.RUnlock()

	if locked != nil {
		return false
	}
	if cred != nil {
		return cred.matches(token)
	}
	return verifyFallback(token, fallback)
}

// Set 设置（或更新）管理密码：派生 → 落盘 → 立即生效。
//
// 顺序很关键：先写盘再更新内存。反过来会在写盘失败时留下
// 「内存已换、磁盘未换」的状态 —— 进程重启后密码悄悄回退，且没有任何提示。
func (s *Store) Set(password string) error {
	if err := validatePassword(password); err != nil {
		return err
	}

	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return fmt.Errorf("生成随机盐失败: %w", err)
	}

	key, err := pbkdf2.Key(sha256.New, password, salt, kdfIterations, kdfKeyLen)
	if err != nil {
		return fmt.Errorf("派生密码哈希失败: %w", err)
	}

	cred := &Credential{
		Version: 1,
		Algo:    "pbkdf2-sha256",
		Iter:    kdfIterations,
		Salt:    base64.StdEncoding.EncodeToString(salt),
		Hash:    base64.StdEncoding.EncodeToString(key),
	}

	if err := s.persist(cred); err != nil {
		return err
	}

	s.mu.Lock()
	s.cred = cred
	s.mu.Unlock()
	return nil
}

// persist 原子写入凭据文件：先写临时文件再重命名，
// 避免写入中途断电/崩溃留下半截 JSON（下次启动直接拒绝服务）。
func (s *Store) persist(c *Credential) error {
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化凭据失败: %w", err)
	}
	data = append(data, '\n')

	dir := filepath.Dir(s.path)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("创建凭据目录: %w", err)
		}
	}

	// 临时名必须唯一：固定的 "<path>.tmp" 在并发首次设置密码时会互相覆盖 ——
	// 两个请求各写一份 tmp、再各rename，其中一份被丢掉，且丢的那份可能是
	// 后来者（后写者胜出，先写者的密码静默失效）。
	f, err := os.CreateTemp(dir, filepath.Base(s.path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("创建凭据临时文件: %w", err)
	}
	tmp := f.Name()
	defer func() {
		// rename 成功后 os.Remove 会失败（文件已不在），忽略即可。
		_ = os.Remove(tmp)
	}()
	// 0o600：凭据文件只有属主可读写。Windows 上该权限位不生效，
	// 但保留它可以让同一份代码在 Linux 部署时自动获得正确权限。
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return fmt.Errorf("设置凭据文件权限: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return fmt.Errorf("写入凭据文件: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("刷新凭据文件: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("关闭凭据文件: %w", err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return fmt.Errorf("提交凭据文件: %w", err)
	}
	return nil
}

func (c *Credential) matches(password string) bool {
	salt, err := base64.StdEncoding.DecodeString(c.Salt)
	if err != nil {
		return false
	}
	want, err := base64.StdEncoding.DecodeString(c.Hash)
	if err != nil {
		return false
	}
	iter := c.Iter
	if iter <= 0 {
		iter = kdfIterations
	}
	got, err := pbkdf2.Key(sha256.New, password, salt, iter, len(want))
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(got, want) == 1
}

// verifyFallback 校验 config.json 的 admin_token，一律按**明文**比较。
//
// 历史实现要猜「这串到底是明文还是 sha256 摘要」（`len==64 && isHex` 启发式），
// 代价是运维把一个恰好 64 位 hex 的明文令牌写进 config 时，拿它当 Bearer 会恒 401
// —— 正是本包注释第 21-23 行声称已消除的那个失败模式。
// 现在唯一的「摘要形态」是 admin_auth.json 里的 PBKDF2，且有 algo 字段显式标明，
// 不需要猜。admin_token 就是明文，说清楚比猜对更重要。
func verifyFallback(token, expected string) bool {
	if expected == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(token), []byte(expected)) == 1
}

// ResolvePath 返回可执行文件同级目录下的凭据文件路径。
func ResolvePath(exeDir string) string {
	return filepath.Join(exeDir, FileName)
}
