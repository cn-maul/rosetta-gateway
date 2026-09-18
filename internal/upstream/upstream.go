package upstream

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand"
	"os"
	"sync"
	"time"

	"github.com/cn-maul/rosetta"
	"github.com/cn-maul/rosetta-gateway/internal/config"
	"github.com/cn-maul/rosetta-gateway/internal/crypto"
	"github.com/cn-maul/rosetta-gateway/internal/store"
)

type CredentialEntry struct {
	ID            string
	Label         string
	APIKey        string
	Weight        int
	Enabled       bool
	Status        string
	CooldownUntil time.Time
	Client        *rosetta.Client
}

type Pool struct {
	mu        sync.RWMutex
	providers map[string]*ProviderEntry
	logger    *slog.Logger
}

type ProviderEntry struct {
	ID          string
	Slug        string
	Name        string
	Protocol    string
	Endpoint    string
	Enabled     bool
	Timeout     time.Duration
	MaxRetries  int
	Credentials []*CredentialEntry
}

func NewPool(logger *slog.Logger) *Pool {
	return &Pool{
		providers: make(map[string]*ProviderEntry),
		logger:    logger,
	}
}

func (p *Pool) BuildFromConfig(cfg *config.Config) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	for _, bp := range cfg.Bootstrap.Providers {
		prov := &ProviderEntry{
			ID:         bp.Slug,
			Slug:       bp.Slug,
			Name:       bp.Name,
			Protocol:   bp.Protocol,
			Endpoint:   bp.Endpoint,
			Enabled:    true,
			Timeout:    cfg.UpstreamTimeout(),
			MaxRetries: cfg.Defaults.MaxRetries,
		}

		for _, bc := range bp.Credentials {
			apiKey := bc.APIKey
			if bc.APIKeyEnv != "" {
				apiKey = resolveEnv(bc.APIKeyEnv)
			}
			if apiKey == "" {
				p.logger.Warn("skipping credential with empty key", "provider", bp.Slug, "label", bc.Label)
				continue
			}

			client, err := buildClient(prov, apiKey, cfg)
			if err != nil {
				return fmt.Errorf("build client for %s/%s: %w", bp.Slug, bc.Label, err)
			}

			cred := &CredentialEntry{
				ID:     bp.Slug + "-" + bc.Label,
				Label:  bc.Label,
				APIKey: apiKey,
				Weight: 1,
				Enabled: true,
				Status: "healthy",
				Client: client,
			}
			prov.Credentials = append(prov.Credentials, cred)
		}

		p.providers[bp.Slug] = prov
	}
	return nil
}

func (p *Pool) BuildFromStore(ctx context.Context, st *store.Store, masterKey []byte, cfg *config.Config) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	providers, err := st.ListProviders(ctx)
	if err != nil {
		return err
	}

	for _, sp := range providers {
		prov := &ProviderEntry{
			ID:         sp.ID,
			Slug:       sp.Slug,
			Name:       sp.Name,
			Protocol:   sp.Protocol,
			Endpoint:   sp.Endpoint,
			Enabled:    sp.Enabled,
			Timeout:    cfg.UpstreamTimeout(),
			MaxRetries: sp.MaxRetries,
		}

		creds, err := st.ListCredentials(ctx, sp.ID)
		if err != nil {
			p.logger.Error("failed to list credentials", "provider", sp.Slug, "error", err)
			continue
		}

		for _, sc := range creds {
			if !sc.Enabled {
				continue
			}

			apiKey, err := decryptCredentialKey(sc.APIKeyEnc, masterKey)
			if err != nil {
				p.logger.Error("failed to decrypt credential", "provider", sp.Slug, "credential", sc.Label, "error", err)
				continue
			}

			client, err := buildClient(prov, apiKey, cfg)
			if err != nil {
				p.logger.Error("failed to build client", "provider", sp.Slug, "credential", sc.Label, "error", err)
				continue
			}

			cooldownUntil := time.Time{}
			if sc.CooldownUntil > 0 {
				cooldownUntil = time.UnixMilli(sc.CooldownUntil)
			}

			cred := &CredentialEntry{
				ID:            sc.ID,
				Label:         sc.Label,
				APIKey:        apiKey,
				Weight:        sc.Weight,
				Enabled:       sc.Enabled,
				Status:        sc.Status,
				CooldownUntil: cooldownUntil,
				Client:        client,
			}
			prov.Credentials = append(prov.Credentials, cred)
		}

		p.providers[sp.Slug] = prov
	}
	return nil
}

func (p *Pool) GetAnyClient(providerSlug string) (*rosetta.Client, string, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	prov, ok := p.providers[providerSlug]
	if !ok || !prov.Enabled {
		return nil, "", fmt.Errorf("provider %q not found or disabled", providerSlug)
	}

	healthy := p.getHealthyCredentials(prov)
	if len(healthy) == 0 {
		return nil, "", fmt.Errorf("no healthy credentials for provider %q", providerSlug)
	}

	cred := p.selectWeighted(healthy)
	return cred.Client, cred.ID, nil
}

func (p *Pool) GetClientForCredential(providerSlug, credID string) (*rosetta.Client, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	prov, ok := p.providers[providerSlug]
	if !ok || !prov.Enabled {
		return nil, fmt.Errorf("provider %q not found or disabled", providerSlug)
	}

	for _, cred := range prov.Credentials {
		if cred.ID == credID && cred.Enabled {
			return cred.Client, nil
		}
	}
	return nil, fmt.Errorf("credential %q not found", credID)
}

func (p *Pool) GetProvider(slug string) (*ProviderEntry, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	prov, ok := p.providers[slug]
	return prov, ok
}

func (p *Pool) ListProviders() []*ProviderEntry {
	p.mu.RLock()
	defer p.mu.RUnlock()
	result := make([]*ProviderEntry, 0, len(p.providers))
	for _, prov := range p.providers {
		result = append(result, prov)
	}
	return result
}

func (p *Pool) MarkCredentialCooldown(credID string, duration time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()

	for _, prov := range p.providers {
		for _, cred := range prov.Credentials {
			if cred.ID == credID {
				cred.CooldownUntil = time.Now().Add(duration)
				cred.Status = "cooling"
				p.logger.Info("credential cooling down",
					"credential", credID,
					"until", cred.CooldownUntil,
					"duration", duration)
				return
			}
		}
	}
}

func (p *Pool) MarkCredentialError(credID string, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	for _, prov := range p.providers {
		for _, cred := range prov.Credentials {
			if cred.ID == credID {
				cred.Status = "error"
				p.logger.Warn("credential error",
					"credential", credID,
					"error", err)
				return
			}
		}
	}
}

func (p *Pool) TestConnection(ctx context.Context, providerSlug string) error {
	client, _, err := p.GetAnyClient(providerSlug)
	if err != nil {
		return err
	}
	_, err = client.ListModels(ctx)
	return err
}

func (p *Pool) getHealthyCredentials(prov *ProviderEntry) []*CredentialEntry {
	now := time.Now()
	var result []*CredentialEntry
	for _, cred := range prov.Credentials {
		if !cred.Enabled {
			continue
		}
		if cred.Status == "cooling" && now.Before(cred.CooldownUntil) {
			continue
		}
		if cred.Status == "disabled" {
			continue
		}
		result = append(result, cred)
	}
	return result
}

func (p *Pool) selectWeighted(creds []*CredentialEntry) *CredentialEntry {
	if len(creds) == 0 {
		return nil
	}
	if len(creds) == 1 {
		return creds[0]
	}

	totalWeight := 0
	for _, c := range creds {
		totalWeight += c.Weight
	}

	r := rand.Intn(totalWeight)
	for _, c := range creds {
		r -= c.Weight
		if r < 0 {
			return c
		}
	}
	return creds[0]
}

func buildClient(prov *ProviderEntry, apiKey string, cfg *config.Config) (*rosetta.Client, error) {
	opts := []rosetta.Option{
		rosetta.WithEndpoint(prov.Endpoint),
		rosetta.WithAPIKey(apiKey),
		rosetta.WithMaxRetries(prov.MaxRetries),
	}
	if prov.Protocol != "" && prov.Protocol != "auto" {
		switch prov.Protocol {
		case "openai-chat":
			opts = append(opts, rosetta.WithProtocol(rosetta.ProtoOpenAIChat))
		case "openai-responses":
			opts = append(opts, rosetta.WithProtocol(rosetta.ProtoOpenAIResponses))
		case "anthropic":
			opts = append(opts, rosetta.WithProtocol(rosetta.ProtoAnthropic))
		}
	}
	if prov.Timeout > 0 {
		opts = append(opts, rosetta.WithTimeout(prov.Timeout))
	}

	return rosetta.NewClient(opts...)
}

func resolveEnv(name string) string {
	return os.Getenv(name)
}

func decryptCredentialKey(enc []byte, masterKey []byte) (string, error) {
	if masterKey == nil {
		return string(enc), nil
	}
	plain, err := crypto.Decrypt(enc, masterKey)
	if err != nil {
		return "", err
	}
	return string(plain), nil
}
