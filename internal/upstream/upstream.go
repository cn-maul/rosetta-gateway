package upstream

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
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
	targets   map[string]*targetHealth
	logger    *slog.Logger
}

// targetHealth 是某个链目标（provider+model，按 route_targets.id 归键）的熔断状态。
// consecutiveFails 达到阈值即把 until 推到未来某点，其间该目标被跳过并让位给链上下一个。
type targetHealth struct {
	consecutiveFails int
	until            time.Time
}

// targetBreakerCooldown 是目标达阈值后的熔断时长。与 5xx 凭据冷却同量级：
// 短到能较快自愈，长到不会让一个坏目标在每个请求上都被重新试一遍。
const targetBreakerCooldown = 60 * time.Second

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
		targets:   make(map[string]*targetHealth),
		logger:    logger,
	}
}

func (p *Pool) BuildFromConfig(cfg *config.Config) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	// 重建前先清空：否则从配置删掉的上游会一直留在池里，
	// /v1 仍然能路由到它（快照已删、池里还在）。
	p.providers = make(map[string]*ProviderEntry)
	// 健康态随池一起清零：凭据冷却在上面的 providers 重建里已隐含重置，
	// 目标熔断这张表若不清就会随「每次保存链都重新生成 targetID」单调堆积成泄漏。
	// 两层一起在重建时归零 —— 语义一致，配置变更本就是重新探测的正当理由。
	p.targets = make(map[string]*targetHealth)

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

	// 重建前先清空：admin 的每个写操作都会触发 reload → 本函数，
	// 不清空的话「删除上游」在池里永不生效，请求仍会被转发到已删除的 provider。
	p.providers = make(map[string]*ProviderEntry)
	// 目标级健康/熔断态与凭据冷却一起清零 —— 重建即重新探测。既避免旧 targetID
	// （每次保存链都重新生成主键）在本表里累积成泄漏，也让两层健康态语义一致。
	p.targets = make(map[string]*targetHealth)

	providers, err := st.ListProviders(ctx)
	if err != nil {
		return err
	}

	for _, sp := range providers {
		timeout := cfg.UpstreamTimeout()
		if sp.TimeoutMs > 0 {
			timeout = time.Duration(sp.TimeoutMs) * time.Millisecond
		}
		// max_retries 是权威值，直接用。此前写成 `if sp.MaxRetries > 0`，
		// 把用户显式设置的 0（= 不重试）静默换成了全局默认 —— 与项目里
		// 「0 是合法显式值」的 PATCH 契约（见 admin/helpers.go）直接冲突。
		// timeout_ms 的 0 有文档明示的「回落全局默认」语义，max_retries 没有。
		maxRetries := sp.MaxRetries
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

// MarkCredentialCooldown 把一把凭据踢出健康轮换一段时间（冷却）。
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

// RecordCredentialSuccess 在一把凭据成功响应后清除其冷却，恢复 healthy。
// 不这样做的话：一次偶发 5xx 把 key 打进 60s cooling，即便它马上又好了，
// 这一分钟内仍被 getHealthyCredentials 跳过 —— 对单 key provider 等于凭空造 outage。
func (p *Pool) RecordCredentialSuccess(credID string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	for _, prov := range p.providers {
		for _, cred := range prov.Credentials {
			if cred.ID == credID {
				if cred.Status != "healthy" || !cred.CooldownUntil.IsZero() {
					cred.Status = "healthy"
					cred.CooldownUntil = time.Time{}
					p.logger.Info("credential recovered", "credential", credID)
				}
				return
			}
		}
	}
}

// TargetAvailable 报告链上某目标当前是否可打（未处于熔断冷却期）。
func (p *Pool) TargetAvailable(targetID string) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	h, ok := p.targets[targetID]
	if !ok {
		return true
	}
	return !time.Now().Before(h.until)
}

// RecordTargetFailure 给目标累计一次失败；达到 threshold 则熔断 targetBreakerCooldown。
// threshold<=0 表示不启用目标级熔断（仅凭据级冷却生效）。
func (p *Pool) RecordTargetFailure(targetID string, threshold int) {
	if threshold <= 0 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	h := p.targets[targetID]
	if h == nil {
		h = &targetHealth{}
		p.targets[targetID] = h
	}
	h.consecutiveFails++
	if h.consecutiveFails >= threshold {
		h.until = time.Now().Add(targetBreakerCooldown)
		h.consecutiveFails = 0
		p.logger.Warn("target circuit opened",
			"target", targetID, "threshold", threshold, "until", h.until)
	}
}

// RecordTargetSuccess 清零目标的连续失败计数（成功即认为健康）。
func (p *Pool) RecordTargetSuccess(targetID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if h, ok := p.targets[targetID]; ok {
		h.consecutiveFails = 0
		h.until = time.Time{}
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

	// 权重全为 0 时不能走加权逻辑：rand.IntN(0) 会 panic，直接把网关打挂。
	// 这是可达状态 —— 后台把每条凭据的权重都改成 0 即可。退化为均匀随机。
	if totalWeight <= 0 {
		return creds[rand.IntN(len(creds))]
	}

	r := rand.IntN(totalWeight)
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
	// 同理：0 表示不重试，是显式配置，不回落全局默认。
	entry := &ProviderEntry{
		Protocol:   p.Protocol,
		Endpoint:   p.Endpoint,
		Timeout:    timeout,
		MaxRetries: p.MaxRetries,
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

// upstreamHTTPClient 是所有 rosetta 客户端共享的 HTTP 客户端（共用一个连接池）。
// 不用 SDK 自建 transport 的两个原因：它的 Proxy 为 nil，不读 HTTP_PROXY/
// HTTPS_PROXY；ResponseHeaderTimeout 固定 30s，而非流式上游要等完整生成才发
// 响应头，慢生成会在 30s 被掐断（网关非流式超时默认 120s）。这里恢复环境
// 代理、头超时放宽到 60s，连接池参数与 SDK 默认保持一致。CheckRedirect 留空，
// 由 SDK 补跨主机重定向防护；client.Timeout 也不设，超时一律走 ctx。
var upstreamHTTPClient = &http.Client{
	Transport: &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		MaxIdleConns:          128,
		MaxIdleConnsPerHost:   64,
		IdleConnTimeout:       90 * time.Second,
		ForceAttemptHTTP2:     true,
		ResponseHeaderTimeout: 60 * time.Second,
	},
}

func buildClient(prov *ProviderEntry, apiKey string, cfg *config.Config) (*rosetta.Client, error) {
	opts := []rosetta.Option{
		rosetta.WithEndpoint(prov.Endpoint),
		rosetta.WithAPIKey(apiKey),
		rosetta.WithMaxRetries(prov.MaxRetries),
		rosetta.WithHTTPClient(upstreamHTTPClient),
	}
	if prov.Protocol != "" && prov.Protocol != "auto" {
		switch prov.Protocol {
		case "openai-chat":
			opts = append(opts, rosetta.WithProtocol(rosetta.ProtoOpenAIChat))
		case "openai-responses":
			opts = append(opts, rosetta.WithProtocol(rosetta.ProtoOpenAIResponses))
		case "anthropic":
			opts = append(opts, rosetta.WithProtocol(rosetta.ProtoAnthropic))
		default:
			// 拼错的协议名此前被静默忽略 —— 不加 WithProtocol，退化成 SDK 的
			// 自动探测，请求打到错误的端点形态上，报出来的错误指向下游而非配置。
			// 配置错了就说配置错了。管理 API 在写入时已用同一份白名单校验过，
			// 这里是运行时（含 bootstrap 配置）的最后一道。
			return nil, fmt.Errorf("provider %s: 未知的 protocol %q（合法值：auto / openai-chat / openai-responses / anthropic）", prov.Slug, prov.Protocol)
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
	return crypto.DecryptWithFallback(enc, masterKey)
}
