package snapshot

import (
	"context"

	"github.com/cn-maul/rosetta-gateway/internal/routing"
	"github.com/cn-maul/rosetta-gateway/internal/store"
	"github.com/cn-maul/rosetta-gateway/internal/upstream"
)

// RebuildFromDB 只搬运「路由/上游/密钥」这三类解析元数据。
// 凭据解密不在这里发生——上游池在 BuildFromStore 里自行解密建客户端，
// 所以本函数不需要主密钥，也不需要 logger。
func RebuildFromDB(ctx context.Context, st *store.Store, pool *upstream.Pool) (*Snapshot, error) {
	snap := &Snapshot{
		Routes:    routing.NewRouteIndex(),
		Providers: make(map[string]*ProviderSnapshot),
		Keys:      make(map[string]*KeySnapshot),
	}

	providers, err := st.ListProviders(ctx)
	if err != nil {
		return nil, err
	}
	for _, p := range providers {
		snap.Providers[p.Slug] = &ProviderSnapshot{
			ID:       p.ID,
			Slug:     p.Slug,
			Name:     p.Name,
			Endpoint: p.Endpoint,
			Enabled:  p.Enabled,
		}

		snap.Routes.AddProvider(&routing.ProviderRef{
			ID:       p.ID,
			Slug:     p.Slug,
			Endpoint: p.Endpoint,
			Protocol: p.Protocol,
			Enabled:  p.Enabled,
		})
	}

	models, err := st.ListAllUpstreamModels(ctx)
	if err != nil {
		return nil, err
	}
	for _, m := range models {
		snap.Routes.AddUpstreamModel(&routing.UpstreamModel{
			ID:         m.ID,
			ProviderID: m.ProviderID,
			ModelID:    m.ModelID,
			Enabled:    m.Enabled,
		})
	}

	routes, err := st.ListRoutes(ctx)
	if err != nil {
		return nil, err
	}
	for _, r := range routes {
		snap.Routes.AddRoute(&routing.Route{
			ID:              r.ID,
			PublicName:      r.PublicName,
			ProviderID:      r.ProviderID,
			UpstreamModelID: r.UpstreamModelID,
			Enabled:         r.Enabled,
			Priority:        r.Priority,
			FallbackRouteID: r.FallbackRouteID,
			ExtraJSON:       r.ExtraJSON,
		})
	}

	keys, err := st.ListAccessKeys(ctx)
	if err != nil {
		return nil, err
	}
	for _, k := range keys {
		snap.Keys[k.ID] = &KeySnapshot{
			ID:      k.ID,
			KeyHash: k.KeyHash,
			Name:    k.Name,
			Enabled: k.Enabled,
		}
	}

	return snap, nil
}
