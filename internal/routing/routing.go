package routing

import (
	"errors"
	"strings"
)

var (
	ErrModelNotFound = errors.New("model_not_found")
)

type Route struct {
	ID               string
	PublicName       string
	ProviderID       string
	UpstreamModelID  string
	Enabled          bool
	Priority         int
	FallbackRouteID  string
	ExtraJSON        string
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
}

type Resolution struct {
	Route          *Route
	Provider       *ProviderRef
	UpstreamModel  *UpstreamModel
}

func NewRouteIndex() *RouteIndex {
	return &RouteIndex{
		byPublicName:   make(map[string]*Route),
		byID:           make(map[string]*Route),
		providers:      make(map[string]*ProviderRef),
		providerModels: make(map[string]map[string]*UpstreamModel),
	}
}

func (ri *RouteIndex) AddRoute(r *Route) {
	ri.byPublicName[r.PublicName] = r
	ri.byID[r.ID] = r
}

func (ri *RouteIndex) RemoveRoute(id string) {
	if r, ok := ri.byID[id]; ok {
		delete(ri.byPublicName, r.PublicName)
		delete(ri.byID, id)
	}
}

func (ri *RouteIndex) AddProvider(p *ProviderRef) {
	ri.providers[p.ID] = p
	ri.providers[p.Slug] = p
	if _, ok := ri.providerModels[p.ID]; !ok {
		ri.providerModels[p.ID] = make(map[string]*UpstreamModel)
	}
}

func (ri *RouteIndex) RemoveProvider(id string) {
	if p, ok := ri.providers[id]; ok {
		delete(ri.providers, p.Slug)
		delete(ri.providers, id)
		delete(ri.providerModels, id)
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

func (ri *RouteIndex) RemoveUpstreamModel(providerID, upstreamModelID string) {
	if models, ok := ri.providerModels[providerID]; ok {
		delete(models, upstreamModelID)
	}
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
		p := ri.findProviderByID(r.ProviderID)
		if p == nil || !p.Enabled {
			return nil, ErrModelNotFound
		}
		m := ri.findUpstreamModel(r.ProviderID, r.UpstreamModelID)
		if m == nil || !m.Enabled {
			return nil, ErrModelNotFound
		}
		return &Resolution{Route: r, Provider: p, UpstreamModel: m}, nil
	}

	if idx := strings.Index(model, "/"); idx > 0 {
		slug := model[:idx]
		rest := model[idx+1:]
		p := ri.findProviderBySlug(slug)
		if p != nil && p.Enabled {
			if m := ri.FindUpstreamModelByModelID(p.ID, rest); m != nil && m.Enabled {
				return &Resolution{
					Route: &Route{
						PublicName:      model,
						ProviderID:      p.ID,
						UpstreamModelID: m.ID,
						Enabled:         true,
					},
					Provider:      p,
					UpstreamModel: m,
				}, nil
			}
		}
	}

	return nil, ErrModelNotFound
}

func (ri *RouteIndex) ListRoutes() []*Route {
	routes := make([]*Route, 0, len(ri.byID))
	for _, r := range ri.byID {
		routes = append(routes, r)
	}
	return routes
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
