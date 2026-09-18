package snapshot

import (
	"context"
	"log/slog"

	"github.com/cn-maul/rosetta-gateway/internal/crypto"
	"github.com/cn-maul/rosetta-gateway/internal/routing"
	"github.com/cn-maul/rosetta-gateway/internal/store"
	"github.com/cn-maul/rosetta-gateway/internal/upstream"
)

func RebuildFromDB(ctx context.Context, st *store.Store, pool *upstream.Pool, masterKey []byte, logger *slog.Logger) (*Snapshot, error) {
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
			ID:          k.ID,
			KeyHash:     k.KeyHash,
			Name:        k.Name,
			Enabled:     k.Enabled,
			QuotaTokens: k.QuotaTokens,
			UsedTokens:  k.UsedTokens,
			RPMLimit:    k.RPMLimit,
			TPMLimit:    k.TPMLimit,
		}
	}

	_ = masterKey
	_ = logger

	return snap, nil
}

func decryptKey(enc []byte, masterKey []byte) (string, error) {
	if masterKey == nil {
		return "", crypto.ErrNoMasterKey
	}
	plain, err := crypto.Decrypt(enc, masterKey)
	if err != nil {
		return "", err
	}
	return string(plain), nil
}
