package snapshot

import (
	"context"

	"github.com/cn-maul/rosetta-gateway/internal/routing"
	"github.com/cn-maul/rosetta-gateway/internal/store"
)

// RebuildFromDB 只搬运「路由/上游/密钥」这三类解析元数据。
// 凭据解密不在这里发生——上游池在 BuildFromStore 里自行解密建客户端，
// 所以本函数不需要主密钥，也不需要 logger，同样不需要池
// （池的重建由 ReloadHandler 显式先做，分工是「先池后快照」）。
func RebuildFromDB(ctx context.Context, st *store.Store) (*Snapshot, error) {
	snap := &Snapshot{
		Routes:     routing.NewRouteIndex(),
		Providers:  make(map[string]*ProviderSnapshot),
		KeysByHash: make(map[string]*KeySnapshot),
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
			FailoverEnabled: r.FailoverEnabled,
		})
	}

	// route_targets 已按 route_id, position 升序返回，逐条 append 即为有序链。
	targets, err := st.ListAllRouteTargets(ctx)
	if err != nil {
		return nil, err
	}
	for _, t := range targets {
		snap.Routes.AddRouteTarget(&routing.Target{
			ID:              t.ID,
			RouteID:         t.RouteID,
			ProviderID:      t.ProviderID,
			UpstreamModelID: t.UpstreamModelID,
			Position:        t.Position,
			Enabled:         t.Enabled,
		})
	}

	keys, err := st.ListAccessKeys(ctx)
	if err != nil {
		return nil, err
	}
	keysByHash := make(map[string]*KeySnapshot, len(keys))
	for _, k := range keys {
		keysByHash[k.KeyHash] = &KeySnapshot{
			ID:      k.ID,
			KeyHash: k.KeyHash,
			Name:    k.Name,
			Enabled: k.Enabled,
		}
	}
	snap.KeysByHash = keysByHash

	// 运行时全局默认（超时与故障转移策略）。读失败不致命：留 0 即全部回落 config。
	if rd, err := st.GetRuntimeDefaults(ctx); err == nil {
		snap.Runtime = RuntimeDefaults{
			UpstreamTimeoutMs:         rd.UpstreamTimeoutMs,
			StreamIdleTimeoutMs:       rd.StreamIdleTimeoutMs,
			StreamFirstTokenTimeoutMs: rd.StreamFirstTokenTimeoutMs,
			FailoverMaxTargets:        rd.FailoverMaxTargets,
			FailoverFailureThreshold:  rd.FailoverFailureThreshold,
		}
	} else {
		return nil, err
	}

	return snap, nil
}
