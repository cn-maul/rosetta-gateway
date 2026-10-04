package routing

import (
	"errors"
	"strings"
)

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
	// ProviderID/UpstreamModelID 保留为「主目标」的向后兼容视图（等价 position 0）。
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
	ID       string
	Slug     string
	Endpoint string
	// Protocol is the upstream wire protocol ("openai-chat",
	// "openai-responses", "anthropic", or "auto"/"" to let the SDK probe).
	// Request construction needs it: protocol-private fields are only valid
	// on the dialect that defines them.
	Protocol string
	Enabled  bool
}

type UpstreamModel struct {
	ID              string
	ProviderID      string
	ModelID         string
	DisplayName     string
	Enabled         bool
	ContextWindow   int
	MaxOutputTokens int
}

type RouteIndex struct {
	byPublicName   map[string]*Route
	byID           map[string]*Route
	providers      map[string]*ProviderRef
	providerModels map[string]map[string]*UpstreamModel // providerID -> upstreamModelID -> model
	targetsByRoute map[string][]*Target                  // routeID -> 有序目标链
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
		byPublicName:   make(map[string]*Route),
		byID:           make(map[string]*Route),
		providers:      make(map[string]*ProviderRef),
		providerModels: make(map[string]map[string]*UpstreamModel),
		targetsByRoute: make(map[string][]*Target),
	}
}

func (ri *RouteIndex) AddRoute(r *Route) {
	ri.byPublicName[r.PublicName] = r
	ri.byID[r.ID] = r
}

// AddRouteTarget 追加一个链成员。快照重建按 route_id, position 顺序喂进来，
// 故这里 append 即为有序；调用方保证 position 递增。
func (ri *RouteIndex) AddRouteTarget(t *Target) {
	ri.targetsByRoute[t.RouteID] = append(ri.targetsByRoute[t.RouteID], t)
}

func (ri *RouteIndex) AddProvider(p *ProviderRef) {
	ri.providers[p.ID] = p
	ri.providers[p.Slug] = p
	if _, ok := ri.providerModels[p.ID]; !ok {
		ri.providerModels[p.ID] = make(map[string]*UpstreamModel)
	}
}

// AddUpstreamModel 以上游模型的主键 ID 建索引。
// Route.UpstreamModelID 外键指向 upstream_models.id，Resolve 便是拿它来查，
// 因此这里必须按 ID（而非 ModelID）分类，否则按公开模型名解析必然失败。
func (ri *RouteIndex) AddUpstreamModel(m *UpstreamModel) {
	if _, ok := ri.providerModels[m.ProviderID]; !ok {
		ri.providerModels[m.ProviderID] = make(map[string]*UpstreamModel)
	}
	ri.providerModels[m.ProviderID][m.ID] = m
}

// FindUpstreamModelByModelID 按 (providerID, modelID) 反查，供需要上游模型名的场景使用。
func (ri *RouteIndex) FindUpstreamModelByModelID(providerID, modelID string) *UpstreamModel {
	for _, m := range ri.providerModels[providerID] {
		if m.ModelID == modelID {
			return m
		}
	}
	return nil
}

func (ri *RouteIndex) Resolve(model string) (*Resolution, error) {
	if r, ok := ri.byPublicName[model]; ok && r.Enabled {
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
			if m := ri.FindUpstreamModelByModelID(p.ID, rest); m != nil && m.Enabled {
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
		if m == nil || !m.Enabled {
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
	if m == nil || !m.Enabled {
		return nil
	}
	return []Candidate{{TargetID: r.ID + "#synth", Provider: p, UpstreamModel: m}}
}

func (ri *RouteIndex) ListRoutes() []*Route {
	routes := make([]*Route, 0, len(ri.byID))
	for _, r := range ri.byID {
		routes = append(routes, r)
	}
	return routes
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
// 这里按 ID 去重；model 本身禁用或 provider 禁用时 Enabled=false，
// 由调用方决定是否列出。
func (ri *RouteIndex) ListUpstreamModels() []UpstreamModelRef {
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
				Enabled:      m.Enabled && p.Enabled,
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
