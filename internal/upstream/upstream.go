package upstream

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"os"
	"strings"
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
				ID:      bp.Slug + "-" + bc.Label,
				Label:   bc.Label,
				APIKey:  apiKey,
				Weight:  1,
				Enabled: true,
				Status:  "healthy",
				Client:  client,
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
		timeout := cfg.UpstreamTimeout()
		if sp.TimeoutMs > 0 {
			timeout = time.Duration(sp.TimeoutMs) * time.Millisecond
		}
		maxRetries := cfg.Defaults.MaxRetries
		if sp.MaxRetries > 0 {
			maxRetries = sp.MaxRetries
		}
		prov := &ProviderEntry{
			ID:         sp.ID,
			Slug:       sp.Slug,
			Name:       sp.Name,
			Protocol:   sp.Protocol,
			Endpoint:   sp.Endpoint,
			Enabled:    sp.Enabled,
			Timeout:    timeout,
			MaxRetries: maxRetries,
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

// resolveCredential 取出 provider 定义及其第一个可用（已启用、可解密、非空）凭据的明文 API Key。
func resolveCredential(ctx context.Context, st *store.Store, providerID string, masterKey []byte) (*store.Provider, string, error) {
	p, err := st.GetProvider(ctx, providerID)
	if err != nil {
		return nil, "", err
	}
	if p == nil {
		return nil, "", fmt.Errorf("provider not found")
	}

	creds, err := st.ListCredentials(ctx, providerID)
	if err != nil {
		return nil, "", err
	}

	for _, c := range creds {
		if !c.Enabled {
			continue
		}
		key, err := decryptCredentialKey(c.APIKeyEnc, masterKey)
		if err != nil {
			continue
		}
		if key != "" {
			return p, key, nil
		}
	}
	return nil, "", fmt.Errorf("该上游没有可用凭据，请先添加 API Key")
}

// NewProviderClient 依据数据库中的 provider 定义与其中一个可用凭据即时建 rosetta 客户端，
// 供后台连通性测试使用；不依赖内存池，刚添加尚未 reload 的上游也能立即测通。
func NewProviderClient(ctx context.Context, st *store.Store, providerID string, masterKey []byte, cfg *config.Config) (*rosetta.Client, error) {
	p, apiKey, err := resolveCredential(ctx, st, providerID, masterKey)
	if err != nil {
		return nil, err
	}

	timeout := cfg.UpstreamTimeout()
	if p.TimeoutMs > 0 {
		timeout = time.Duration(p.TimeoutMs) * time.Millisecond
	}
	maxRetries := cfg.Defaults.MaxRetries
	if p.MaxRetries > 0 {
		maxRetries = p.MaxRetries
	}

	entry := &ProviderEntry{
		Protocol:   p.Protocol,
		Endpoint:   p.Endpoint,
		Timeout:    timeout,
		MaxRetries: maxRetries,
	}
	return buildClient(entry, apiKey, cfg)
}

// ModelCandidate 是模型发现返回的原始条目；容量字段为 0 表示上游未暴露。
type ModelCandidate struct {
	ID              string
	DisplayName     string
	ContextWindow   int
	MaxOutputTokens int
}

// rawModelEntry 宽松匹配各家 /models 的字段名；缺字段解析为 0，多余字段忽略。
type rawModelEntry struct {
	ID                  string `json:"id"`
	Name                string `json:"name"`
	DisplayName         string `json:"display_name"`
	ContextLength       int    `json:"context_length"`
	ContextWindow       int    `json:"context_window"`
	MaxInputTokens      int    `json:"max_input_tokens"`
	MaxCompletionTokens int    `json:"max_completion_tokens"`
	MaxOutputTokens     int    `json:"max_output_tokens"`
	TopProvider         struct {
		ContextLength       int `json:"context_length"`
		MaxCompletionTokens int `json:"max_completion_tokens"`
	} `json:"top_provider"`
}

// DiscoverUpstreamModels 直接向 provider 拉一次原始 /models，
// 解析出各家扩展的上下文/最大输出容量（rosetta 的 ListModels 不填这些）。
// OpenAI 兼容端点用 Authorization: Bearer；anthropic 用 x-api-key + anthropic-version。
func DiscoverUpstreamModels(ctx context.Context, st *store.Store, providerID string, masterKey []byte, cfg *config.Config) ([]ModelCandidate, error) {
	p, apiKey, err := resolveCredential(ctx, st, providerID, masterKey)
	if err != nil {
		return nil, err
	}

	base := strings.TrimRight(p.Endpoint, "/")
	urls := []string{base + "/models"}
	if p.Protocol == "anthropic" {
		urls = []string{base + "/v1/models", base + "/models"}
	}

	client := &http.Client{Timeout: 15 * time.Second}
	var lastErr error
	for _, u := range urls {
		cands, err := fetchModels(ctx, client, u, p.Protocol, apiKey)
		if err != nil {
			lastErr = err
			continue
		}
		return cands, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("无法获取模型列表")
	}
	return nil, lastErr
}

func fetchModels(ctx context.Context, client *http.Client, url, protocol, apiKey string) ([]ModelCandidate, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if protocol == "anthropic" {
		req.Header.Set("x-api-key", apiKey)
		req.Header.Set("anthropic-version", "2023-06-01")
	} else {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("上游返回 HTTP %d：%s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var payload struct {
		Data   []rawModelEntry `json:"data"`
		Models []rawModelEntry `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("解析模型列表失败：%w", err)
	}

	entries := payload.Data
	if len(entries) == 0 {
		entries = payload.Models
	}

	result := make([]ModelCandidate, 0, len(entries))
	for _, e := range entries {
		if e.ID == "" {
			continue
		}
		name := e.DisplayName
		if name == "" {
			name = e.Name
		}
		result = append(result, ModelCandidate{
			ID:              e.ID,
			DisplayName:     name,
			ContextWindow:   firstPositive(e.ContextLength, e.ContextWindow, e.MaxInputTokens, e.TopProvider.ContextLength),
			MaxOutputTokens: firstPositive(e.MaxCompletionTokens, e.MaxOutputTokens, e.TopProvider.MaxCompletionTokens),
		})
	}
	if len(result) == 0 {
		return nil, fmt.Errorf("上游未返回任何模型")
	}
	return result, nil
}

func firstPositive(vals ...int) int {
	for _, v := range vals {
		if v > 0 {
			return v
		}
	}
	return 0
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
