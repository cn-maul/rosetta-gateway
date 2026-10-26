package gatewayapp

import (
	"context"
	crand "crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/cn-maul/rosetta-gateway/internal/config"
	"github.com/cn-maul/rosetta-gateway/internal/crypto"
	"github.com/cn-maul/rosetta-gateway/internal/routing"
	"github.com/cn-maul/rosetta-gateway/internal/snapshot"
	"github.com/cn-maul/rosetta-gateway/internal/store"
)

func bootstrapDB(ctx context.Context, db *store.Store, cfg *config.Config,
	logger *slog.Logger, masterKey []byte) error {
	existing, err := db.ListProviders(ctx)
	if err != nil {
		return fmt.Errorf("read providers before bootstrap: %w", err)
	}
	if len(cfg.Bootstrap.Providers) == 0 {
		return nil
	}
	completed, err := db.ConfigBootstrapCompleted(ctx)
	if err != nil {
		return fmt.Errorf("read config bootstrap completion marker: %w", err)
	}
	if completed {
		if len(existing) == 0 {
			return fmt.Errorf("config bootstrap completion marker exists but bootstrap graph is empty")
		}
		if len(cfg.Bootstrap.Providers) > 0 {
			logger.Warn("database already has a completed bootstrap graph, ignoring bootstrap config")
		}
		return nil
	}
	if len(existing) > 0 {
		verified, verifyErr := verifyLegacyBootstrapGraph(ctx, db, cfg)
		if verifyErr != nil {
			return fmt.Errorf("verify legacy bootstrap graph: %w", verifyErr)
		}
		if !verified {
			return fmt.Errorf(
				"database has providers but no config bootstrap completion marker and does not match the configured graph; refusing to publish an incomplete or unverifiable graph")
		}
		if err := db.RunTx(ctx, func(ctx context.Context, tx store.Tx) error {
			return db.MarkConfigBootstrapCompletedTx(ctx, tx)
		}); err != nil {
			return fmt.Errorf("backfill config bootstrap completion marker: %w", err)
		}
		logger.Info("verified and marked existing bootstrap graph complete")
		return nil
	}

	logger.Info("bootstrapping database from config")
	err = db.RunTx(ctx, func(ctx context.Context, tx store.Tx) error {
		modelIDByKey := make(map[string]string)
		networkByURL := make(map[string]string)

		for _, bp := range cfg.Bootstrap.Providers {
			networkID := ""
			if proxyURL := strings.TrimSpace(bp.ProxyURL); proxyURL != "" {
				id, ok := networkByURL[proxyURL]
				if !ok {
					id = "net-" + newIdentifier()
					if err := db.CreateNetworkTx(ctx, tx,
						&store.Network{ID: id, Name: proxyURL, ProxyURL: proxyURL}); err != nil {
						return fmt.Errorf("create network for provider %s: %w", bp.Slug, err)
					}
					networkByURL[proxyURL] = id
				}
				networkID = id
			}
			p := &store.Provider{
				ID:         bp.Slug,
				Slug:       bp.Slug,
				Name:       bp.Name,
				Protocol:   bp.Protocol,
				Endpoint:   bp.Endpoint,
				Enabled:    true,
				MaxRetries: cfg.Defaults.MaxRetries,
				NetworkID:  networkID,
			}
			if err := db.CreateProviderTx(ctx, tx, p); err != nil {
				return fmt.Errorf("create provider %s: %w", bp.Slug, err)
			}

			for _, bc := range bp.Credentials {
				apiKey := bc.ResolveAPIKey(os.Getenv)
				if apiKey == "" {
					return fmt.Errorf("credential %s/%s has no usable API key", bp.Slug, bc.Label)
				}
				enc, err := crypto.Encrypt([]byte(apiKey), masterKey)
				if err != nil {
					return fmt.Errorf("encrypt credential %s/%s: %w", bp.Slug, bc.Label, err)
				}
				c := &store.Credential{
					ID:         newIdentifier(),
					ProviderID: bp.Slug,
					Label:      bc.Label,
					APIKeyEnc:  enc,
					Enabled:    true,
					Weight:     1,
					Status:     "healthy",
				}
				if err := db.CreateCredentialTx(ctx, tx, c); err != nil {
					return fmt.Errorf("create credential %s/%s: %w", bp.Slug, bc.Label, err)
				}
			}

			for _, modelID := range bp.Models {
				m := &store.UpstreamModel{
					ID:         newIdentifier(),
					ProviderID: bp.Slug,
					ModelID:    modelID,
				}
				if err := db.CreateUpstreamModelTx(ctx, tx, m); err != nil {
					return fmt.Errorf("create model %s/%s: %w", bp.Slug, modelID, err)
				}
				modelIDByKey[bp.Slug+"/"+modelID] = m.ID
			}
		}

		for _, br := range cfg.Bootstrap.Routes {
			upstreamModelID, ok := modelIDByKey[br.Provider+"/"+br.Model]
			if !ok {
				return fmt.Errorf("route %s references unknown model %s/%s",
					br.PublicName, br.Provider, br.Model)
			}
			r := &store.Route{
				ID:              newIdentifier(),
				PublicName:      br.PublicName,
				ProviderID:      br.Provider,
				UpstreamModelID: upstreamModelID,
				Enabled:         true,
			}
			if err := db.CreateRouteTx(ctx, tx, r); err != nil {
				return fmt.Errorf("create route %s: %w", br.PublicName, err)
			}
			if err := db.CreateRouteTargetTx(ctx, tx, &store.RouteTarget{
				RouteID:         r.ID,
				ProviderID:      r.ProviderID,
				UpstreamModelID: r.UpstreamModelID,
				Position:        0,
				Enabled:         true,
			}); err != nil {
				return fmt.Errorf("create head target for route %s: %w", br.PublicName, err)
			}
		}
		return db.MarkConfigBootstrapCompletedTx(ctx, tx)
	})
	if err != nil {
		return fmt.Errorf("bootstrap graph transaction: %w", err)
	}
	logger.Info("bootstrap completed")
	return nil
}

// verifyLegacyBootstrapGraph distinguishes a complete graph written by an
// older binary from a partial graph left by its non-transactional bootstrap.
// A configured provider is complete only when its provider row, every
// configured model, at least the configured credential count, every configured
// route, and each route's head target all exist and agree.
func verifyLegacyBootstrapGraph(ctx context.Context, db *store.Store, cfg *config.Config) (bool, error) {
	providers, err := db.ListProviders(ctx)
	if err != nil {
		return false, err
	}
	providerBySlug := make(map[string]store.Provider, len(providers))
	for _, p := range providers {
		providerBySlug[p.Slug] = p
	}
	for _, bp := range cfg.Bootstrap.Providers {
		p, ok := providerBySlug[bp.Slug]
		if !ok {
			return false, nil
		}
		creds, err := db.ListCredentials(ctx, p.ID)
		if err != nil {
			return false, err
		}
		if len(creds) < len(bp.Credentials) {
			return false, nil
		}
		models, err := db.ListUpstreamModels(ctx, p.ID)
		if err != nil {
			return false, err
		}
		modelByID := make(map[string]store.UpstreamModel, len(models))
		for _, m := range models {
			modelByID[m.ModelID] = m
		}
		for _, modelID := range bp.Models {
			if _, ok := modelByID[modelID]; !ok {
				return false, nil
			}
		}
	}

	routes, err := db.ListRoutes(ctx)
	if err != nil {
		return false, err
	}
	routeByName := make(map[string]store.Route, len(routes))
	for _, r := range routes {
		routeByName[r.PublicName] = r
	}
	models, err := db.ListAllUpstreamModels(ctx)
	if err != nil {
		return false, err
	}
	modelByID := make(map[string]store.UpstreamModel, len(models))
	for _, m := range models {
		modelByID[m.ID] = m
	}
	for _, br := range cfg.Bootstrap.Routes {
		r, ok := routeByName[br.PublicName]
		if !ok || r.ProviderID != br.Provider {
			return false, nil
		}
		model, ok := modelByID[r.UpstreamModelID]
		if !ok || model.ProviderID != br.Provider || model.ModelID != br.Model {
			return false, nil
		}
		targets, err := db.ListRouteTargets(ctx, r.ID)
		if err != nil {
			return false, err
		}
		if len(targets) == 0 || targets[0].Position != 0 ||
			targets[0].ProviderID != br.Provider ||
			targets[0].UpstreamModelID != r.UpstreamModelID {
			return false, nil
		}
	}
	return true, nil
}

func buildSnapshotFromConfig(cfg *config.Config) *snapshot.Snapshot {
	snap := &snapshot.Snapshot{
		Routes:     routing.NewRouteIndex(),
		Providers:  make(map[string]*snapshot.ProviderSnapshot),
		KeysByHash: make(map[string]*snapshot.KeySnapshot),
	}
	modelIDs := make(map[string]string)

	for _, bp := range cfg.Bootstrap.Providers {
		snap.Providers[bp.Slug] = &snapshot.ProviderSnapshot{
			ID:       bp.Slug,
			Slug:     bp.Slug,
			Name:     bp.Name,
			Endpoint: bp.Endpoint,
			Enabled:  true,
		}
		snap.Routes.AddProvider(&routing.ProviderRef{
			ID:              bp.Slug,
			Slug:            bp.Slug,
			Name:            bp.Name,
			Endpoint:        bp.Endpoint,
			Protocol:        bp.Protocol,
			ProxyConfigured: bp.ProxyURL != "",
			Enabled:         true,
		})
		for _, modelID := range bp.Models {
			id := cfgModelID(bp.Slug, modelID)
			modelIDs[bp.Slug+"/"+modelID] = id
			snap.Routes.AddUpstreamModel(&routing.UpstreamModel{
				ID:         id,
				ProviderID: bp.Slug,
				ModelID:    modelID,
			})
		}
	}

	for _, br := range cfg.Bootstrap.Routes {
		upstreamModelID, ok := modelIDs[br.Provider+"/"+br.Model]
		if !ok {
			continue
		}
		snap.Routes.AddRoute(&routing.Route{
			ID:              br.PublicName,
			PublicName:      br.PublicName,
			ProviderID:      br.Provider,
			UpstreamModelID: upstreamModelID,
			Enabled:         true,
		})
	}
	return snap
}

func cfgModelID(providerSlug, modelID string) string {
	return providerSlug + "/" + modelID
}

func newIdentifier() string {
	b := make([]byte, 16)
	_, _ = crand.Read(b)
	return hex.EncodeToString(b)
}
