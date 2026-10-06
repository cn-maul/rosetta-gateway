package snapshot

import (
	"net/netip"
	"strings"
	"sync/atomic"

	"github.com/cn-maul/rosetta-gateway/internal/routing"
)

// Snapshot 是 /v1 数据面解析路由/密钥所用的只读快照。
//
// # 并发契约（重要）
//
// Get() 返回的 *Snapshot 会被并发请求 handler 无锁读取（Routes 的 RouteIndex、
// Keys map 等）。因此 Snapshot 及其内部结构在构建完成后**必须保持不可变**：
//   - 只允许通过 Init / Swap 整体替换（atomic.Pointer），绝不允许原地修改
//     已发布的 Snapshot（例如调用 RouteIndex.AddRoute / 往 Keys map 写值）。
//   - 新增字段/方法时，若会修改已发布快照的内容，必须先复制一份再改，再 Swap。
//
// 违反该契约会引入数据竞争。当前所有写路径（RebuildFromDB、buildSnapshotFromConfig）
// 都是「新建 → 填充 → Swap」，符合上述要求。

type Snapshot struct {
	Routes    *routing.RouteIndex
	Providers map[string]*ProviderSnapshot
	// KeysByHash 以 SHA-256(明文key) 的 hex 为键建索引，供热路径 O(1) 鉴权
	// （internal/auth）。此前的 ID 键 map 没有任何读者，鉴权靠全量遍历 ——
	// key 数量上百 + 高 QPS 时每次请求都是 O(N) 字符串比较。
	KeysByHash map[string]*KeySnapshot
	// UsersByID 以用户 id 建索引，供热路径查用户的启用状态与配额。
	// 为什么热路径需要它：禁用用户必须**立刻**生效，而读库会引入
	// 每请求查询。放进快照后随 Swap 一次全量更新（D5 的既有机制）。
	UsersByID map[string]*UserSnapshot
	// Runtime 是超时与故障转移策略的全局默认（来自 app_settings，0 = 回落 config）。
	Runtime RuntimeDefaults
}

// UserSnapshot 是用户在数据面热路径上需要的全部字段。
//
// 刻意只放热路径真正会读的字段：放进来就意味着它必须随每次快照重建
// 被正确填充，多一个字段多一处可能忘记赋值的地方。
type UserSnapshot struct {
	ID     string
	Name   string
	Role   string
	Status string
	// QuotaTokens 是用户级总额度，0 = 不限。
	QuotaTokens int64
	// AuthVersion 供会话校验比对（改密码/禁用后旧 JWT 立即失效）。
	AuthVersion int64
	// GroupID 是所属分组（P1，显示用）。
	GroupID string
	// AllowedModels 是**该用户所属组**的模型白名单（P1）。
	//
	// 不放在单独的 group 表结构里让热路径去查：多一次间接寻址、多一处
	// 可能忘记填充。重建时已经解析好，热路径拿到就是最终答案。
	// 用户不属于任何组、或组未配白名单 → AllowAll()。
	AllowedModels ModelAllow
}

// UserStatusActive 与 store.UserStatusActive 保持字面量一致。
//
// 这里复制常量而不 import store，是为了让鉴权热路径不依赖 store 包 ——
// 那个包会把数据库驱动、事务、全部 DAO 一起拖进 auth 的依赖图。
// 两处字面量必须同步修改（值都是 "active"）。
const UserStatusActive = "active"

// IsActive 报告该用户是否可用。
func (u *UserSnapshot) IsActive() bool {
	return u != nil && u.Status == UserStatusActive
}

// ModelAllow 是「哪些公开模型名可见」的判定器（多用户改造 P1）。
//
// # 为什么零值是「拒绝全部」而不是「不限制」
//
// 这是本类型唯一需要小心的设计点。快照重建时若漏填某个字段，零值会被
// 直接用在热路径上：
//
//   - 零值 = 拒绝全部 → 那把 key 立刻用不了，日志与用户投诉会指出问题，
//     是有界的、可发现的故障。
//   - 零值 = 不限制 → 本该受限的 key 静默拿到全部模型，没有任何人会发现。
//
// 后者正是多用户改造要消除的那类失败（权限边界悄悄失效），所以这里
// 明确让零值偏向收紧。需要「不限制」时必须显式写 AllowAll()。
type ModelAllow struct {
	// Unrestricted 为真 = 该维度没有白名单，不参与收窄。
	Unrestricted bool
	// Models 是白名单内容，仅在 Unrestricted 为假时有意义。
	Models []string
}

// AllowAll 表示不限制（无白名单）。
func AllowAll() ModelAllow { return ModelAllow{Unrestricted: true} }

// AllowOnly 表示只允许列出的公开模型名。
//
// 注意：列表为空（或全是空白）时得到的是**拒绝全部**，不是「不限制」。
// 「空」在这一层的含义只能是「什么都不允许」；要表达「不限制」必须
// 用 AllowAll()，让调用方的意图显式写出来。
//
// 重复项会被丢弃：快照是常驻内存的，而重复项只会让每个请求多扫几次。
// 正常路径下来源已经是集合（组的白名单来自主键表），但 key 级白名单
// 存在 JSON 列里，手改库或将来换写入路径都可能带重复。
func AllowOnly(models []string) ModelAllow {
	out := make([]string, 0, len(models))
	seen := make(map[string]struct{}, len(models))
	for _, m := range models {
		if m = strings.TrimSpace(m); m != "" {
			if _, dup := seen[m]; dup {
				continue
			}
			seen[m] = struct{}{}
			out = append(out, m)
		}
	}
	return ModelAllow{Models: out}
}

// Allows 报告某个公开模型名是否可见。
func (a ModelAllow) Allows(model string) bool {
	if a.Unrestricted {
		return true
	}
	for _, m := range a.Models {
		if m == model {
			return true
		}
	}
	return false
}

// Intersect 求两个白名单的交集：任一维度不限制即取另一个，都受限则取交。
//
// 交为空时结果是一个「受限的空白名单」= 拒绝全部，语义正确：
// 组只允许 A、key 只允许 B（且 A≠B）时，这把 key 确实什么都调不了。
func (a ModelAllow) Intersect(b ModelAllow) ModelAllow {
	if a.Unrestricted {
		return b
	}
	if b.Unrestricted {
		return a
	}
	out := make([]string, 0, len(a.Models))
	for _, m := range a.Models {
		if b.Allows(m) {
			out = append(out, m)
		}
	}
	return ModelAllow{Models: out}
}

// RuntimeDefaults 镜像 store.RuntimeDefaults，避免 snapshot 的使用方（如 cmd/gateway
// 的转发路径）为了读两个整数去依赖 store。0 一律表示「未配置，回落 config」。
type RuntimeDefaults struct {
	UpstreamTimeoutMs         int
	StreamIdleTimeoutMs       int
	StreamFirstTokenTimeoutMs int
	FailoverMaxTargets        int
	FailoverFailureThreshold  int
	// DefaultContextWindow / DefaultMaxOutputTokens 是模型容量默认值
	//（设置页「模型默认」）。/v1/models 的 context_length / max_output_tokens
	// 在上游模型未单独覆盖时回落到这里，供外部工具读取正确容量。
	DefaultContextWindow   int
	DefaultMaxOutputTokens int
}

// ProviderSnapshot 是管理面与 /v1/models 看到的 provider 视图。
//
// Ready 表示「本次重建后该 provider 是否有可用上游」。
// 它在此前根本不存在：凭据解密失败只进日志，界面上 provider 看起来一切正常，
// 而请求打过去必然失败 —— 运维只能靠「莫名 500」反推（P4 / 设计 §4.7）。
type ProviderSnapshot struct {
	ID       string
	Slug     string
	Name     string
	Endpoint string
	Enabled  bool
	// Ready=false 表示没有任何可用凭据能建成上游客户端。
	// Reason 是面向运维的一句话原因；空串表示正常。
	Ready  bool
	Reason string
}

type KeySnapshot struct {
	ID      string
	KeyHash string
	Name    string
	Enabled bool
	// UserID 是归属用户（多用户改造 P0）。热路径上**必然非空**：
	// 无归属 key 已被启动迁移 retireOrphanKeys 禁用，且鉴权查不到归属用户
	// 即返回 ErrKeyUnowned（401），不会走到这里之后的分支。
	UserID string
	// RPMLimit / TPMLimit 是 Key 维度每分钟限速（DESIGN §11.4），0 = 不限。
	// 随快照下发：热路径取额度不查库，管理改动经 reload 生效。
	RPMLimit int
	TPMLimit int
	// AllowedModels 是 key 级模型白名单（P1）。未配置 → AllowAll()。
	// 与用户的 AllowedModels 求交，且 key 级只能更紧。
	AllowedModels ModelAllow
	// GroupModelAllow 是**这把 key 实际生效**的组白名单（P2 的 key 级组覆盖）。
	//
	// 为什么不放一个 GroupsByID 让热路径自己查：多一次间接寻址、多一处
	// 可能忘记填充的地方。重建时已经解析好，热路径拿到就是最终答案。
	GroupModelAllow ModelAllow
	// ExpiresAt 是有效期截止（毫秒，0 = 永不过期，P2）。
	// 热路径直接比较，过期即刻 403，不靠任何后台扫库任务。
	ExpiresAt int64
	// AllowedNets 是来源 IP 白名单（P2，nil = 不限制）。
	// 已在 store 层解析好；非 nil 且为空 = 配置了但全都解析不出来
	// → 一条都不允许（见 store.parseAllowedNets 的注释）。
	AllowedNets []netip.Prefix
}

// AllowsIP 报告来源地址 addr 是否被这把 key 允许。
//
// 「nil = 不限制、非 nil 空 = 全拒」这条约定收在这里，不让热路径各处的调用方
// 自己去解读切片状态 —— 那正是 P2 最容易写反的地方。
func (k *KeySnapshot) AllowsIP(addr netip.Addr) bool {
	if k.AllowedNets == nil {
		return true
	}
	// IPv4-mapped IPv6（::ffff:10.0.0.1）在 netip 里是独立类型；
	// 不 Unmap 的话「只写了 IPv4 白名单」会把 IPv4 客户端全部拦掉。
	if addr.Is4In6() {
		addr = addr.Unmap()
	}
	// 逐条比对而不是建 map：白名单通常一两条，且这是每请求一次的热路径。
	for _, p := range k.AllowedNets {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

var current atomic.Pointer[Snapshot]

func Init(s *Snapshot) {
	current.Store(s)
}

// Get 返回当前快照。返回的指针是共享只读数据，调用方不得修改其内容
// （见 Snapshot 的并发契约注释）。
func Get() *Snapshot {
	s := current.Load()
	if s == nil {
		return &Snapshot{
			Routes:     routing.NewRouteIndex(),
			Providers:  make(map[string]*ProviderSnapshot),
			KeysByHash: make(map[string]*KeySnapshot),
			UsersByID:  make(map[string]*UserSnapshot),
		}
	}
	return s
}

func Swap(s *Snapshot) {
	current.Store(s)
}
