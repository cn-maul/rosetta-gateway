package auth

import (
	"crypto/sha256"
	"encoding/hex"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"github.com/cn-maul/rosetta-gateway/internal/snapshot"
)

type Context struct {
	KeyID string
	Name  string
	// UserID 是这把 key 的归属用户（多用户改造 P0）。热路径上**保证非空**。
	UserID string
	// UserStatus 随快照下发，让「用户被禁用」在热路径零成本生效，
	// 不需要每请求查库。
	UserStatus string
	// RPMLimit / TPMLimit 是该 key 的每分钟限速额度（0 = 不限），
	// 从快照随鉴权一并带出，供转发热路径执行限速（DESIGN §11.4）。
	RPMLimit int
	TPMLimit int
	// KeyModelAllow / GroupModelAllow 是模型可见性的两个维度（P1）：
	// key 级白名单（access_keys.allowed_models_json）与用户所属组的白名单
	// （user_group_models）。两者求**交**，且 key 级只能更紧。
	//
	// 都从快照带出，热路径判定零查库、零分配（白名单是个位数长度，
	// 线性扫描比建 map 更快也更省内存）。
	KeyModelAllow   snapshot.ModelAllow
	GroupModelAllow snapshot.ModelAllow
}

// userRoleAdmin 与 store.RoleAdmin 保持字面量一致。
//
// 与 UserStatusActive 同理：复制常量而不 import store，是为了让鉴权热路径
// 不依赖 store 包（那个包会把数据库驱动、事务、全部 DAO 拖进 auth 的依赖图）。
// 两处字面量必须同步修改（值都是 "admin"）。
const userRoleAdmin = "admin"

// AllowsModel 报告本请求是否有权使用某个公开模型名。
//
// 两个维度独立判定而不是先求交再查：求交要为每个请求分配一个新切片，
// 而这里白名单只有个位数，两次线性扫描的代价更低、且零分配。
func (c *Context) AllowsModel(model string) bool {
	return c.KeyModelAllow.Allows(model) && c.GroupModelAllow.Allows(model)
}

// IsAdminOwner 报告这把 key 的归属用户是不是管理员。
//
// 数据面据此把管理员彻底挡在门外（ErrAdminCannotCallModel）：管理员只做
// 控制面管理，调用模型必须用**普通用户**的 key。
func (c *Context) IsAdminOwner() bool {
	u := snapshot.Get().UsersByID[c.UserID]
	return u != nil && u.Role == userRoleAdmin
}

func Authenticate(r *http.Request) (*Context, error) {
	key := extractKey(r)
	if key == "" {
		return nil, ErrNoKey
	}

	hash := sha256.Sum256([]byte(key))
	hashHex := hex.EncodeToString(hash[:])

	// 按哈希索引 O(1) 查表，而不是逐 key 常量时间比较（旧实现全量遍历，
	// 与 DESIGN「SHA-256 索引」的口径也不符）。map 查找的耗时确实会泄露
	// 「与存储哈希前缀的匹配程度」，但存储的是高熵 key 的 SHA-256 ——
	// 前缀匹配信息无法反推哈希原像，更无法还原 key 本身，不构成可用的
	// 侧信道。慢哈希（bcrypt 类）同样不必要：key 是网关生成的高熵随机串
	// （DESIGN §6.2），不是用户口令。
	matched := snapshot.Get().KeysByHash[hashHex]
	if matched == nil {
		return nil, ErrInvalidKey
	}
	if !matched.Enabled {
		return nil, ErrKeyDisabled
	}

	// 有效期（P2）。放在这里而不是最后：过期是「这把钥匙本身到期了」，
	// 与归属用户无关，先判可以少走一次 map 查找，也不会泄露用户状态。
	//
	// 用 `now >= ExpiresAt` 而不是 `>`：expires_at 语义是「到这一刻起失效」，
	// 写成严格大于会让恰好在这一毫秒的请求通过，边界行为不可预期。
	if matched.ExpiresAt > 0 && time.Now().UnixMilli() >= matched.ExpiresAt {
		return nil, ErrKeyExpired
	}

	// 来源 IP 白名单（P2）。AllowsIP 在未配置限制时直接返回 true，
	// 所以这里不需要分支；为省一次 map 查找而重复「是否配置了限制」的判断，
	// 反而会引入两处语义漂移的风险。
	//
	// 取值只用 RemoteAddr（直连对端），**刻意不读 X-Forwarded-For**：
	// 那个头由客户端随意填写，采信它等于让 allowed_ips 形同虚设
	// （任何人都能伪造一个白名单内的来源 IP）。代价是网关前面有反向代理时，
	// 判定的是代理的地址 —— 这一点必须在文档与界面提示里讲明。
	addr, _ := netip.ParseAddr(peerHost(r))
	if !matched.AllowsIP(addr) {
		return nil, ErrIPNotAllowed
	}

	// 归属用户的启用状态随快照一并带出，热路径据此零成本拒绝已禁用账号
	// （不查库，禁用随 Swap 立即生效）。
	snap := snapshot.Get()
	u := snap.UsersByID[matched.UserID]
	if u == nil {
		// 查不到归属用户 → 拒绝。两种成因都不该放行：
		//   - user_id 为空：迁移前的无归属 key。多用户改造决定「不发新 key
		//     就不给用」（MULTIUSER.md §5.1 的修订），它必须失效到有人接手为止；
		//   - user_id 指向一个不存在的行：数据不一致，放行等于绕过归属约束。
		//
		// 明确 401 好过静默放行：后者会让「谁的 key 在用」彻底无从追查。
		return nil, ErrKeyUnowned
	}
	if !u.IsActive() {
		return nil, ErrUserDisabled
	}

	// 管理员彻底退出数据面（2026-10 需求确认：「管理员账号只负责管理网关」）。
	//
	// # 为什么放在**鉴权层**而不是 handleIngress 里加一道检查
	//
	// 数据面有**多条**入口都只做 auth.Authenticate：/v1 三个推理端点、
	// /v1/models、/dashboard/billing/*、/v1/organization/*。在 handleIngress
	// 里加检查只能覆盖推理端点，其余入口会各自漂移 —— 而漂移的方向恰恰是
	// 最危险的：管理员旧 key 在 /v1/models 或 billing 查询上照常通过
	// （billing 本来也用 admin 看全量，那是控制面语义），只有推理端点被挡，
	// 口径就不再一致。收敛到 Authenticate 这一处，**所有**数据面入口同时生效。
	//
	// # 为什么排在「用户存在」「用户启用」之后
	//
	// 一个「无归属 / 已禁用」的 key 应当先报它自己的错，管理员判定排在其后：
	// 前两者是数据不一致/运维操作，与角色无关；而且这样管理员账号被禁用时
	// 报的是「用户已禁用」而不是「管理员不能调模型」—— 处置动作完全不同。
	//
	// # 为什么不判定「key 是否属于管理员」而是「**归属用户**是不是管理员」
	//
	// 判的是归属用户的角色（u.Role），不是 key 上某个可写的标记。角色是
	// users 表的权威事实，经快照下发、热路径零查库。管理员改自己的角色为
	// user 后，下一次快照重建就自动恢复可调用，无需手工清理任何 key 标记。
	//
	// # 存量管理员 key 的行为：立即失效（fail-fast）
	//
	// 见 ErrAdminCannotCallModel 的注释：不保留、给清晰错误，让管理员知道
	// 「要去建普通用户」而不是以为 key 坏了而反复重试。
	if u.Role == userRoleAdmin {
		return nil, ErrAdminCannotCallModel
	}

	return &Context{
		KeyID:         matched.ID,
		Name:          matched.Name,
		UserID:        matched.UserID,
		UserStatus:    u.Status,
		RPMLimit:      matched.RPMLimit,
		TPMLimit:      matched.TPMLimit,
		KeyModelAllow: matched.AllowedModels,
		// 组白名单直接取**已解析好的**那一份：P2 的 key 级组覆盖
		// （access_keys.group_id）在快照重建时就已折算完毕，
		// 热路径不需要判断「该用 key 的组还是用户的组」。
		GroupModelAllow: matched.GroupModelAllow,
	}, nil
}

// peerHost 取请求的对端地址（host 部分）。
//
// 刻意不读 X-Forwarded-For / X-Real-IP：那两个头由客户端随意填写，
// 采信它们会让任何基于来源地址的判定（IP 白名单、失败限速）都能被伪造。
// 代价是网关前面有反向代理时看到的是代理地址 —— 对「限源」这类需求来说，
// 宁可判定得粗一点，也不要给一个可伪造的口子。
func peerHost(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// extractKey 只从请求头取密钥。
//
// 刻意不支持 `?key=`：查询串会被写进访问日志、Referer、以及沿途中间代理的日志，
// 等于把一把长期有效的密钥散落到多个不设防的地方。Authorization / X-Api-Key
// 是唯一入口。
func extractKey(r *http.Request) string {
	auth := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(auth) > len(prefix) && strings.EqualFold(auth[:len(prefix)], prefix) {
		return auth[len(prefix):]
	}

	if key := r.Header.Get("X-Api-Key"); key != "" {
		return key
	}

	return ""
}
