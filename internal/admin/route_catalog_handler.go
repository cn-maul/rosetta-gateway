package admin

import (
	"net/http"
	"time"

	"github.com/cn-maul/rosetta-gateway/internal/snapshot"
)

// The catalog is admin-only. It contains configuration, never live health claims.
// Four bulk reads replace per-route/per-target queries and include only the
// provider fields needed by route selection, leaving endpoints and secrets out.
// @ts-type RouteCatalogProvider
type routeCatalogProvider struct {
	ID          string `json:"id"`
	Slug        string `json:"slug"`
	Name        string `json:"name"`
	Enabled     bool   `json:"enabled"`
	Ready       *bool  `json:"ready"`
	ReadyReason string `json:"ready_reason,omitempty"`
}

type routeChainSummary struct {
	Total   int `json:"total"`
	Enabled int `json:"enabled"`
	Standby int `json:"standby"`
}

// @ts-type RouteCatalogEntry
type routeCatalogEntry struct {
	routeResponse
	Targets []routeTargetResponse `json:"targets"`
	Chain   routeChainSummary     `json:"chain"`
}

// @ts-type RouteCatalog
type routeCatalogResponse struct {
	Routes      []routeCatalogEntry    `json:"routes"`
	Providers   []routeCatalogProvider `json:"providers"`
	Models      []modelResponse        `json:"models"`
	GeneratedAt int64                  `json:"generated_at"`
}

func (h *RouteHandler) Catalog(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireAdmin(w, r); !ok {
		return
	}
	ctx := r.Context()
	routes, err := h.store.ListRoutes(ctx)
	if err != nil {
		writeServerError(w, "route catalog routes", err)
		return
	}
	providers, err := h.store.ListProviders(ctx)
	if err != nil {
		writeServerError(w, "route catalog providers", err)
		return
	}
	models, err := h.store.ListAllUpstreamModels(ctx)
	if err != nil {
		writeServerError(w, "route catalog models", err)
		return
	}
	targets, err := h.store.ListAllRouteTargets(ctx)
	if err != nil {
		writeServerError(w, "route catalog targets", err)
		return
	}
	resp := routeCatalogResponse{
		Routes:      make([]routeCatalogEntry, 0, len(routes)),
		Providers:   make([]routeCatalogProvider, 0, len(providers)),
		Models:      make([]modelResponse, 0, len(models)),
		GeneratedAt: time.Now().UnixMilli(),
	}
	providerNames := make(map[string]string, len(providers))
	modelNames := make(map[string]string, len(models))
	snap := snapshot.Get()
	for _, p := range providers {
		entry := routeCatalogProvider{ID: p.ID, Slug: p.Slug, Name: p.Name, Enabled: p.Enabled}
		if snap != nil {
			if sp, ok := snap.Providers[p.Slug]; ok && sp != nil {
				ready := sp.Ready
				entry.Ready, entry.ReadyReason = &ready, sp.Reason
			}
		}
		resp.Providers = append(resp.Providers, entry)
		providerNames[p.ID] = p.Name
	}
	for _, m := range models {
		resp.Models = append(resp.Models, toModelResponse(m))
		modelNames[m.ID] = m.ModelID
	}
	byRoute := make(map[string][]routeTargetResponse)
	for _, t := range targets {
		byRoute[t.RouteID] = append(byRoute[t.RouteID], routeTargetResponse{
			ID: t.ID, RouteID: t.RouteID, ProviderID: t.ProviderID,
			ProviderName: providerNames[t.ProviderID], UpstreamModelID: t.UpstreamModelID,
			ModelID: modelNames[t.UpstreamModelID], Position: t.Position, Enabled: t.Enabled,
		})
	}
	for _, route := range routes {
		chain := byRoute[route.ID]
		if chain == nil {
			chain = []routeTargetResponse{}
		}
		entry := routeCatalogEntry{routeResponse: toRouteResponse(route), Targets: chain}
		entry.Chain.Total = len(chain)
		for i, t := range chain {
			if t.Enabled {
				entry.Chain.Enabled++
			}
			if i > 0 {
				entry.Chain.Standby++
			}
		}
		resp.Routes = append(resp.Routes, entry)
	}
	writeJSON(w, http.StatusOK, resp)
}
