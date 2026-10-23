package routing

import (
	"errors"
	"sort"
	"strings"
	"sync"

	"github.com/cn-maul/rosetta-gateway/internal/effort"
)

// RouteIndex 的锁策略（2026-10-21 修 *X15）。
//
// 只有**公开方法**加锁；私有的 find* / buildCandidates 一律假定调用方
// 已持锁。这样 Resolve 内部连锁查找时不会出现「同一 goroutine 嵌套
// RLock」—— 那个写法在 Go 里本身可重入，但只要有写锁排在中间就会
// 死锁（RLock 会让路给等待中的 Lock）。热路径上少两次加锁解锁也是
// 净收益。
//
// 新增公开方法时必须自己决定加不加锁，别靠调用私有 find* 蒙混过关。

var (
	ErrModelNotFound = errors.New("model_not_found")
)

type Route struct {
	ID              string
	PublicName      string
	ProviderID      string
	UpstreamModelID string
	Enabled         bool

	// 是否启用自动故障转移。链成员见 RouteIndex.targetsByRoute；
	// ProviderID/UpstreamModelID 是「主目标」的镜像视图（等价 position 0），
	// 由写入路径与 route_targets 原子同步。
	//
	// 注意：故障转移的**策略参数**（最多尝试几个目标、熔断阈值、各类超时）是全局的，
	// 由「设置」页写入 app_settings 并在运行时经快照读取 —— 不在这里按 route 覆盖，
	// 避免出现「界面上看不到、却仍在生效」的隐形配置。
	FailoverEnabled bool
}

// Target 是故障转移链上的一个成员（一个 provider+model）。
type Target struct {
	ID              string
	RouteID         string
	ProviderID      string
	UpstreamModelID string
	Position        int
	Enabled         bool
}

type ProviderRef struct {
	ID   string
	Slug string
	// Name 是运维填写的人类可读名称，供调用历史的 route_trace 与固化归因
	// 使用。Slug 是路由标识，Name 才是「实际是哪家上游」的可读答案。
	Name     string
	Endpoint string
	// Protocol is the upstream wire protocol ("chat",
	// "response", "anthropic", or "auto"/"" to let the SDK probe).
	// Request construction needs it: protocol-private fields are only valid
	// on the dialect that defines them.
	Protocol string
	// NetworkID and ProxyConfigured are runtime facts for diagnostics. They
	// never contain a proxy URL or credentials: an operator only needs to know
	// whether the outbound path used a configured network.
	NetworkID       string
	ProxyConfigured bool
	Enabled         bool
}

type UpstreamModel struct {
	ID              string
	ProviderID      string
	ModelID         string
	DisplayName     string
	ContextWindow   int
	MaxOutputTokens int

	// SupportsThinking 显式声明该模型能否思考；nil = 未配置（网关不干预）。
	//
	// 三态不可塌成 bool：「未配置」与「确定不支持」必须分开 —— 前者是「不知道」
	// （放行），后者是「知道不支持」（剥掉思考配置）。用 bool 会让所有未
	// 配置的模型一夜之间变成不能思考。
	SupportsThinking *bool

	// EffortLevels 是本模型真实支持的思考挡位（强度升序），nil = 未配置。
	//
	// 数据面用它把客户端请求的挡位夹到模型真正支持的范围内；/v1/models 用它
	// 向 agent 工具披露「这个模型有哪些档可选」—— 没有后者，客户端只能靠
	// 硬编码猜，而猜错的后果是静默降级（inwire.parseEffort 会把 xhigh 压成
	// high），谁也看不见。
	//
	// 与价格字段同理由放在这里：落库时静态、请求时只读，放进快照可以让热
	// 路径零查库地拿到它（见上面价格字段的注释）。
	EffortLevels []effort.Level

	// 单价，单位「元 / 百万 tokens」，0 = 未配置（与 store.freezeUsageCost
	// 的「未配价按 0 计」同义）。语义与上游_models 三列逐字对应：
	// PriceInput 是缓存未命中的输入，PriceCacheHit 是缓存命中的输入，
	// PriceCacheCreation 是写入缓存的输入（Anthropic cache creation），
	// PriceOutput 是输出。
	//
	// 为什么把计费字段放进这个「路由/拓扑」结构（Lead 已确认的口径）：
	// 余额预检要在**请求前**按单价估算本次费用，而预估算是热路径上每个
	// 非管理员请求都会做的事。若这里没有价格，就得为每个请求多打一次库
	// （upstream_models 按 provider_id+model_id 查），把一次内存快照读
	// 换成一次 SQLite 读 —— 而快照存在的全部意义正是「热路径无锁读」。
	// ContextWindow / MaxOutputTokens 早就是同一类「落库时静态、请求时
	// 只读」的元数据，价格与它们同源，不是异类。
	//
	// 代价与对策：这里放的是**本快照生成时刻**的价格，价格改动要等下一次
	// reload 才在预检里生效。这与「固化费用」的口径是同向的（cost_total
	// 也按落库当时的单价固化），且预检只是「够不够」的粗判，真正入账仍
	// 由 store.freezeUsageCost 按扣费当时的价格算 —— 预检用旧价的窗口
	// 最多是一笔预检尺度的偏差，不会让账目漂移。
	PriceInput         float64
	PriceCacheHit      float64
	PriceCacheCreation float64
	PriceOutput        float64
}

type RouteIndex struct {
	// mu 保护下面六张 map。
	//
	// 为什么需要它（2026-10-21 修 *X15）：snapshot.Snapshot 的并发契约
	// 写着「已发布的快照不可变」，但那是**纯注释**——Add* 是导出方法，
	// 任何包都能在 Swap 之后原地改这张索引。一旦有人为了「让新配置立即
	// 生效」这么做，热路径的 map 读写立刻变成并发 map 读写，而那是
	// **Go 运行时 fatal error，不可恢复**（不是 panic，recover 拦不住）。
	// 锁把「约定」变成「机制」：写路径 serialized，读路径无锁化前的过渡期
	// 至少不会崩进程。
	//
	// 用 RWMutex 而不是 Mutex：Resolve 是**每个请求**都要走的热路径，
	// 而写路径只在快照重建时发生（秒级一次）。读多写少正是 RWMutex 的
	// 适用场景，读读之间不互斥。
	mu           sync.RWMutex
	byPublicName map[string]*Route
	byID         map[string]*Route
	providers    map[string]*ProviderRef
	// providerModels: providerID -> upstream_models.id -> model，解析 route 外键用。
	providerModels map[string]map[string]*UpstreamModel
	// modelsByModelID: providerID -> upstream_models.model_id -> model，
	// 解析 provider/model 直连形式用。两者分开是因为键不同：外键指向行主键
	// （32 位 hex），而 client 传的是上游模型名。
	modelsByModelID map[string]map[string]*UpstreamModel
	targetsByRoute  map[string][]*Target
}

// Candidate 是一个可执行的解析结果：链上的一个目标，已解析到 provider 与上游模型。
type Candidate struct {
	TargetID      string // 熔断计数的键（合成目标为 routeID+"#synth"）
	Position      int
	Provider      *ProviderRef
	UpstreamModel *UpstreamModel
}

type Resolution struct {
	Route *Route
	// Candidates 是按 position 升序、且已过滤掉 disabled provider/model 的可用目标链。
	// 至少含一个元素才会返回；Route.Candidates[0] 即主目标。
	Candidates []Candidate
}

func NewRouteIndex() *RouteIndex {
	return &RouteIndex{
		byPublicName:    make(map[string]*Route),
		byID:            make(map[string]*Route),
		providers:       make(map[string]*ProviderRef),
		providerModels:  make(map[string]map[string]*UpstreamModel),
		modelsByModelID: make(map[string]map[string]*UpstreamModel),
		targetsByRoute:  make(map[string][]*Target),
	}
}

func (ri *RouteIndex) AddRoute(r *Route) {
	ri.mu.Lock()
	defer ri.mu.Unlock()
	ri.byPublicName[r.PublicName] = r
	ri.byID[r.ID] = r
}

// AddRouteTarget 追加一个链成员，并按 position 稳定排序。
//
// 快照重建目前确实按 route_id, position 顺序喂进来，但「调用方保证有序」
// 只是一条口头约定：任何新的装载路径（测试、增量刷新、未来的导入工具）
// 一旦乱序，故障转移就会按错误优先级打上游。排序放在这里，约定变成机制；
// 同 position 时 SliceStable 保留插入顺序，行为可预测。
func (ri *RouteIndex) AddRouteTarget(t *Target) {
	ri.mu.Lock()
	defer ri.mu.Unlock()
	targets := append(ri.targetsByRoute[t.RouteID], t)
	sort.SliceStable(targets, func(i, j int) bool {
		return targets[i].Position < targets[j].Position
	})
	ri.targetsByRoute[t.RouteID] = targets
}

// AddProvider 登记一个上游。
//
// ID 与 Slug 放进**同一张 map**：Resolve 的 provider/model 直连分支要按
// slug 查，而 findProviderByID 要按 ID 查。代价是两个命名空间没有隔离 ——
// 理论上 slug 恰好等于另一 provider 的 32 位 hex ID 时会取到错误对象。
// 现实中不可达（slug 由 slugify 从显示名生成，见 admin/provider_handler.go），
// 故不拆成两张表：多一次 map 查找换取热路径（Resolve 每次请求都调
// findProviderBySlug）少一次分支。
func (ri *RouteIndex) AddProvider(p *ProviderRef) {
	ri.mu.Lock()
	defer ri.mu.Unlock()
	ri.providers[p.ID] = p
	ri.providers[p.Slug] = p
	if _, ok := ri.providerModels[p.ID]; !ok {
		ri.providerModels[p.ID] = make(map[string]*UpstreamModel)
	}
	if _, ok := ri.modelsByModelID[p.ID]; !ok {
		ri.modelsByModelID[p.ID] = make(map[string]*UpstreamModel)
	}
}

// AddUpstreamModel 以上游模型的主键 ID 建索引。
// Route.UpstreamModelID 外键指向 upstream_models.id，Resolve 便是拿它来查，
// 因此这里必须按 ID（而非 ModelID）分类，否则按公开模型名解析必然失败。
//
// 同时维护 modelsByModelID 反向索引：provider/model 直连形式
// （client 传 "myslug/gpt-4"）走 FindUpstreamModelByModelID，而那是**每个请求
// 都会走**的热路径。旧实现是遍历 providerModels[providerID] 逐个字符串比较，
// 上游模型一多（单 provider 挂几十个模型很常见）就成 O(N)。反查表把它变成
// 两次 O(1) 查找 —— 团队已把 Keys 从 O(N) 改成 map，只是漏了这里。
func (ri *RouteIndex) AddUpstreamModel(m *UpstreamModel) {
	ri.mu.Lock()
	defer ri.mu.Unlock()
	if _, ok := ri.providerModels[m.ProviderID]; !ok {
		ri.providerModels[m.ProviderID] = make(map[string]*UpstreamModel)
	}
	ri.providerModels[m.ProviderID][m.ID] = m
	if _, ok := ri.modelsByModelID[m.ProviderID]; !ok {
		ri.modelsByModelID[m.ProviderID] = make(map[string]*UpstreamModel)
	}
	ri.modelsByModelID[m.ProviderID][m.ModelID] = m
}

// FindUpstreamModelByModelID 按 (providerID, modelID) 反查，供需要上游模型名的场景使用。
//
// 走 modelsByModelID 反查表，O(1)。同名 ModelID 在同一 provider 下重复时
// 返回最后 Add 的那个 —— 与旧线性扫描「返回第一个匹配」的行为差异在
// 配置层就被排除了（同一 provider 下 ModelID 唯一）。
func (ri *RouteIndex) FindUpstreamModelByModelID(providerID, modelID string) *UpstreamModel {
	ri.mu.RLock()
	defer ri.mu.RUnlock()
	return ri.modelsByModelID[providerID][modelID]
}

func (ri *RouteIndex) Resolve(model string) (*Resolution, error) {
	ri.mu.RLock()
	defer ri.mu.RUnlock()
	if r, ok := ri.byPublicName[model]; ok {
		if !r.Enabled {
			// 命中一条被显式禁用的具名路由 → 立即否决，**不得**继续往下走
			// provider/model 直连兜底。
			//
			// 旧实现写成 `if r, ok := ...; ok && r.Enabled`，!r.Enabled 时条件
			// 短路、控制流直接落到下一个分支，而下一支只看 p.Enabled，
			// 完全不再看「这个名字是否已被显式禁用」。触发路径完全现实：
			// public_name 只校验非空、不限制含 "/"，所以「acme/gpt-4」这种
			// 按客户分模型名的路由是合法的 —— 运维把它禁掉后，客户端请求该名字
			// 会命中直连兜底、请求成功打到上游，禁用形同虚设。
			//
			// 这与 buildCandidates 里「绝不复活死目标」是同一条原则：显式的
			// 禁用必须胜过任何兜底。
			return nil, ErrModelNotFound
		}
		cands := ri.buildCandidates(r)
		if len(cands) == 0 {
			return nil, ErrModelNotFound
		}
		return &Resolution{Route: r, Candidates: cands}, nil
	}

	// provider/model 直连形式：没有对应的具名 route，现造一个单目标 Route 兜底。
	if idx := strings.Index(model, "/"); idx > 0 {
		slug := model[:idx]
		rest := model[idx+1:]
		p := ri.findProviderBySlug(slug)
		if p != nil && p.Enabled {
			if m := ri.FindUpstreamModelByModelID(p.ID, rest); m != nil {
				synth := &Route{
					PublicName:      model,
					ProviderID:      p.ID,
					UpstreamModelID: m.ID,
					Enabled:         true,
				}
				return &Resolution{
					Route:      synth,
					Candidates: []Candidate{{TargetID: synth.PublicName + "#synth", Provider: p, UpstreamModel: m}},
				}, nil
			}
		}
	}

	return nil, ErrModelNotFound
}

// buildCandidates 产出某条 route 的有序可用目标链。
//
// 优先读 route_targets（AddRouteTarget 喂进来的）；为空时回落为「主目标」单元素
// —— 保证从未配置过链的老 route 行为与故障转移前完全一致（零回归）。
// 逐个过滤掉 provider 缺失/禁用、model 缺失/禁用的成员，只留下真正可打的。
//
// 「跨路由兜底」不在这里实现：旧的 fallback_route_id 单跳兜底已被 route_targets
// 有序链取代（见 DESIGN §10），把目标直接挂到同一条链上即可获得同样甚至更强的
// 能力（多跳、可熔断、可排序），无需第二套机制。
func (ri *RouteIndex) buildCandidates(r *Route) []Candidate {
	targets := ri.targetsByRoute[r.ID]
	var out []Candidate
	for _, t := range targets {
		if !t.Enabled {
			continue
		}
		p := ri.findProviderByID(t.ProviderID)
		if p == nil || !p.Enabled {
			continue
		}
		m := ri.findUpstreamModel(t.ProviderID, t.UpstreamModelID)
		if m == nil {
			continue
		}
		out = append(out, Candidate{TargetID: t.ID, Position: t.Position, Provider: p, UpstreamModel: m})
	}
	// 只要这条 route 有目标行（哪怕当前全被禁用/不可用），就以它为准 ——
	// 不能回落到 routes 的主目标列，否则等于无视运维对链的显式禁用、把死目标复活。
	// 仅当「一条目标行都没有」（从未配置过链的老 route）才合成主目标兜底。
	if len(targets) > 0 {
		return out
	}

	p := ri.findProviderByID(r.ProviderID)
	if p == nil || !p.Enabled {
		return nil
	}
	m := ri.findUpstreamModel(r.ProviderID, r.UpstreamModelID)
	if m == nil {
		return nil
	}
	return []Candidate{{TargetID: r.ID + "#synth", Provider: p, UpstreamModel: m}}
}

func (ri *RouteIndex) ListRoutes() []*Route {
	ri.mu.RLock()
	defer ri.mu.RUnlock()
	routes := make([]*Route, 0, len(ri.byID))
	for _, r := range ri.byID {
		routes = append(routes, r)
	}
	// 返回的是**指针副本**：切片本身是新建的，但元素仍指向索引内部的
	// *Route。调用方改 *Route.Enabled 依然是原地改已发布快照 —— 那是
	// 锁管不到的事（锁只保证 map 结构不被并发写坏）。
	// 审计 *X15 记录了这个残留风险；把它彻底关掉需要 Route 变成不可变值
	// 类型，那是另一次重构，不在本次范围。
	return routes
}

// ListPublicNames 返回全部**具名路由**的公开名（不含 provider/model 直连
// 形式产生的合成路由）。
//
// 为什么需要它（2026-10-14 探测端点按身份收窄）：白名单是按**公开名**精确
// 匹配的，而调用方要拿它去反查上游模型 id。没有这个方法的话每个调用方
// 只能自己 for 一遍 ListRoutes 取 PublicName —— 而 ListRoutes 的返回值
// 是 map 遍历出来的无序指针切片，写错一处顺序依赖就出静默 bug。
//
// 返回的是副本：调用方排序或改它都不会影响索引。刻意**不排序** ——
// 这是索引快照的枚举，不是给人看的清单；要稳定顺序的调用方自己排。
func (ri *RouteIndex) ListPublicNames() []string {
	ri.mu.RLock()
	defer ri.mu.RUnlock()
	names := make([]string, 0, len(ri.byPublicName))
	for name := range ri.byPublicName {
		names = append(names, name)
	}
	return names
}

// SuggestModel 对一个未命中的模型名给出可用名建议，返回 nil 表示没有可建议的。
//
// # 为什么要给建议（而不是让用户自己对着 /v1/models 找）
//
// 「模型名打错」是最高频的接入故障，而它现在的报错只有一句
// `model "xxx" not found`：用户不知道是名字错了、还是这个模型根本没配、
// 还是自己没权限，于是只能去翻管理界面逐个比对。给出「你是不是想用 X」
// 把这一步消掉，是 magpie 的 unknownModel + provider.Closest 做的事
// （错误信息即产品）。
//
// 判定分两档，都刻意不用编辑距离那种需要浮点阈值的算法 —— 阈值调不准会出现
// 「明显不像的名字被推荐出来」，那比不给建议更糟：
//  1. 前缀/包含匹配：命中最强的信号。用户多打了一段前缀、漏了版本后缀、
//     或把 gpt-4o 写成 gpt4o 时都能命中。
//  2. 子序列匹配：字符按序出现即可，用来接住漏字符/多字符的笔误。
//
// 上限 5 条：再多就成了清单噪音，与错误文案的定位相悖。
func (ri *RouteIndex) SuggestModel(requested string, max int) []string {
	if max <= 0 {
		max = 5
	}
	names := ri.ListPublicNames()
	lower := strings.ToLower(requested)
	type hit struct {
		name     string
		contains bool
	}
	hits := make([]hit, 0, len(names))
	for _, n := range names {
		ln := strings.ToLower(n)
		if strings.Contains(ln, lower) {
			hits = append(hits, hit{name: n, contains: true})
			continue
		}
		if isSubsequence(lower, ln) {
			hits = append(hits, hit{name: n})
		}
	}
	if len(hits) == 0 {
		return nil
	}
	// 含 requested 的排前面（最可能是漏了前缀/后缀），其次按长度升序
	// 让更具体的名字优先，最后按字典序保证输出稳定（map 遍历无序）。
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].contains != hits[j].contains {
			return hits[i].contains
		}
		if len(hits[i].name) != len(hits[j].name) {
			return len(hits[i].name) < len(hits[j].name)
		}
		return hits[i].name < hits[j].name
	})
	if len(hits) > max {
		hits = hits[:max]
	}
	out := make([]string, len(hits))
	for i, h := range hits {
		out[i] = h.name
	}
	return out
}

// isSubsequence 判断 want 的字符是否按序出现在 have 中（调用方已转小写）。
// 长度必须不大于 have，否则直接否 —— 否则 requested 的超长部分恒为子序列，
// 会把「明显不是它」的模型全推荐出来。
func isSubsequence(want, have string) bool {
	if len(want) == 0 || len(want) > len(have) {
		return false
	}
	i := 0
	for j := 0; j < len(have) && i < len(want); j++ {
		if want[i] == have[j] {
			i++
		}
	}
	return i == len(want)
}

// UpstreamModelRef 是轨道二（slug/model 直连）的枚举项。
type UpstreamModelRef struct {
	ProviderID   string
	ProviderSlug string
	ModelID      string
	Enabled      bool
}

// ListUpstreamModels 枚举全部 (provider, upstream model) 对，供
// /v1/models?include=upstream 展开。providers 索引按 ID 与 slug 各存一份，
// 这里按 ID 去重；provider 禁用时 Enabled=false，由调用方决定是否列出。
func (ri *RouteIndex) ListUpstreamModels() []UpstreamModelRef {
	ri.mu.RLock()
	defer ri.mu.RUnlock()
	seen := make(map[string]bool, len(ri.providers))
	out := make([]UpstreamModelRef, 0)
	for _, p := range ri.providers {
		if seen[p.ID] {
			continue
		}
		seen[p.ID] = true
		for _, m := range ri.providerModels[p.ID] {
			out = append(out, UpstreamModelRef{
				ProviderID:   p.ID,
				ProviderSlug: p.Slug,
				ModelID:      m.ModelID,
				Enabled:      p.Enabled,
			})
		}
	}
	return out
}

func (ri *RouteIndex) findProviderByID(id string) *ProviderRef {
	return ri.providers[id]
}

func (ri *RouteIndex) findProviderBySlug(slug string) *ProviderRef {
	return ri.providers[slug]
}

func (ri *RouteIndex) findUpstreamModel(providerID, modelID string) *UpstreamModel {
	if models, ok := ri.providerModels[providerID]; ok {
		return models[modelID]
	}
	return nil
}
