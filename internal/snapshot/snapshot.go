package snapshot

import (
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
	// Runtime 是超时与故障转移策略的全局默认（来自 app_settings，0 = 回落 config）。
	Runtime RuntimeDefaults
}

// RuntimeDefaults 镜像 store.RuntimeDefaults，避免 snapshot 的使用方（如 cmd/gateway
// 的转发路径）为了读两个整数去依赖 store。0 一律表示「未配置，回落 config」。
type RuntimeDefaults struct {
	UpstreamTimeoutMs         int
	StreamIdleTimeoutMs       int
	StreamFirstTokenTimeoutMs int
	FailoverMaxTargets        int
	FailoverFailureThreshold  int
}

type ProviderSnapshot struct {
	ID       string
	Slug     string
	Name     string
	Endpoint string
	Enabled  bool
}

type KeySnapshot struct {
	ID      string
	KeyHash string
	Name    string
	Enabled bool
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
		}
	}
	return s
}

func Swap(s *Snapshot) {
	current.Store(s)
}
