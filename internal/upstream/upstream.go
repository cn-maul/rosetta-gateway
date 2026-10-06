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

// Pool 是上游 provider/凭据/熔断状态的运行时容器。
//
// credIndex（credID → *CredentialEntry）不是可选优化：MarkCredentialCooldown 与
// RecordCredentialSuccess 在每次流式成功与每次失败时都会被调用，二者都持**写锁**。
// 没有索引时它们要在锁内遍历所有 provider 的所有凭据（O(N×M)），而 GetAnyClient
// 持读锁 —— 于是整个数据面被这两把写锁串行化。加索引把两者降到 O(1)。
type Pool struct {
	mu          sync.RWMutex
	providers   map[string]*ProviderEntry
	credIndex   map[string]*CredentialEntry
	targets     map[string]*targetHealth
	cooldownSto CooldownStore
	logger      *slog.Logger
}

// CooldownStore 是凭据冷却状态的持久化出口。由 store.Store 实现；
// 为 nil 时 Pool 只改内存（测试与 bootstrap 路径）。
type CooldownStore interface {
	SetCredentialCooldown(ctx context.Context, id, status string, until time.Time) error
}

// targetHealth 是某个链目标（provider+model，按 route_targets.id 归键）的熔断状态。
// consecutiveFails 达到阈值即把 until 推到未来某点，其间该目标被跳过并让位给链上下一个。
type targetHealth struct {
	consecutiveFails int
	until            time.Time
	// halfOpen 标记「冷却刚到期、已放一个探测请求进去」。
	// 没有它，冷却到期瞬间所有在途请求会同时打向刚恢复（或仍坏）的目标，
	// 形成惊群 —— 要么瞬时并发尖峰，要么全体真实失败各自计一次。
	halfOpen bool
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

// ProviderFailure 描述「一个 provider 为什么没能在本次重建里就绪」。
//
// # 为什么必须有这个结构（P4 / 设计 §4.7）
//
// 重建池时单个 provider 可能因凭据解密失败、client 构建失败而跳过。
// 在此之前这些失败**只进日志**：运维看到的现象是「路由配好了、请求莫名 500」，
// 而根因（某个 key 解不开）躺在日志里没人看 —— 配置界面对此一字不提，
// 这正是本项目最忌讳的「界面在说谎」。
//
// 现在失败项随快照下发到管理面，Providers 页能直接标出
// 「这个 provider 未就绪：原因」。
//
// # 为什么是「一条凭据失败整个 provider 就算失败」
//
// 池里一个 provider 若一条可用凭据都没有，它就没有任何 client ——
// 路由过去必然失败。部分凭据可用时 provider **不算失败**（还能服务），
// 所以只有「零可用凭据」或「连凭据清单都读不出来」才记入这里。
type ProviderFailure struct {
	ID     string // provider id（与 store.Provider.ID 同域）
	Slug   string
	Name   string
	Reason string // 面向运维的一句话原因，直接来自下面的失败分支
	Stage  string // "list_credentials" | "decrypt" | "build_client" | "no_credentials"
	Detail string // 原始错误文本（可能含凭据标签；不含密钥本身）
}

func NewPool(logger *slog.Logger) *Pool {
	return &Pool{
		providers: make(map[string]*ProviderEntry),
		credIndex: make(map[string]*CredentialEntry),
		targets:   make(map[string]*targetHealth),
		logger:    logger,
	}
}

// SetCooldownStore 注入冷却持久化出口。必须在任何数据面请求之前调用
// （main 在建池后立即设置）；之后调用不影响已在途的请求。
func (p *Pool) SetCooldownStore(s CooldownStore) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cooldownSto = s
}

// reindexLocked 重建 credID 索引。调用方必须已持写锁。
func (p *Pool) reindexLocked() {
	idx := make(map[string]*CredentialEntry)
	for _, prov := range p.providers {
		for _, cred := range prov.Credentials {
			idx[cred.ID] = cred
		}
	}
	p.credIndex = idx
}

func (p *Pool) BuildFromConfig(cfg *config.Config) error {
	// 先在局部表里完整构建，全部成功才 Install 替换 —— 与 BuildFromStore 同一纪律：
	// 构建中途失败（buildClient 出错）绝不能把旧池清掉，否则 /v1 全站断粮。
	newProviders := make(map[string]*ProviderEntry)
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

		newProviders[bp.Slug] = prov
	}
	// config bootstrap 路径不存在 route_targets（路由只能来自 DB），
	// 所以存活 target 集合是**空集**（不是 nil）：先前留下的任何熔断条目
	// 都对应一个不存在的目标，应当丢弃。
	p.Install(newProviders, map[string]bool{})
	return nil
}

// BuildFromStore 重建池并 Install。
//
// 失败清单**刻意不在这里消费**：本函数只在启动路径上用一次，
// 「哪些 provider 未就绪」的可见化由 reload 路径经快照下发到管理面。
// 这里仍保留原语义（整体失败才返回 error），未就绪的 provider 只记日志。
func (p *Pool) BuildFromStore(ctx context.Context, st *store.Store, masterKey []byte, cfg *config.Config) error {
	providers, failures, err := p.PrepareFromStore(ctx, st, masterKey, cfg)
	if err != nil {
		return err
	}
	for _, f := range failures {
		p.logger.Warn("provider not ready after pool build",
			"provider", f.Slug, "stage", f.Stage, "reason", f.Reason, "detail", f.Detail)
	}
	p.Install(providers, liveTargetIDsFromStore(ctx, st))
	return nil
}

// liveTargetIDsFromStore 读出全部 route target 的 ID 集合。
// 失败时返回**空集**而非 nil：Install 的语义是「集合外的条目一律丢弃」，
// 而调用方能走到这里说明 PrepareFromStore 已经成功查过库了，此处再失败
// 只可能是目标表本身为空。此时把熔断全清的后果（坏上游短暂复活），
// 远小于把已删目标的状态永远留在内存里的后果（单调堆积）。
func liveTargetIDsFromStore(ctx context.Context, st *store.Store) map[string]bool {
	targets, err := st.ListAllRouteTargets(ctx)
	if err != nil {
		return map[string]bool{}
	}
	ids := make(map[string]bool, len(targets))
	for _, t := range targets {
		ids[t.ID] = true
	}
	return ids
}

// PrepareFromStore 从数据库构建一份完整的 provider 表（含解密、建 client），
// **不触碰池的现有状态**。旧实现「先清空 p.providers 再查库」在 ListProviders
// 失败时会留下一个空池，之后所有 /v1 请求都选不到上游，直到下一次成功 reload。
//
// 三个返回值的分工（P4 / 设计 §4.7 的 partial-failure 通道）：
//   - providers：建好的表；
//   - failures：**没能就绪的 provider 清单**（单 provider 的凭据问题）；
//   - err：整体失败（如 ListProviders 读库失败），此时 providers 为 nil，
//     调用方不得 Install。
//
// failures 与 err 分开是刻意的：单个 provider 的凭据问题不该让整个 reload
// 失败（其它 provider 照样要更新），但**必须可观测** —— 此前这些信息只在日志里，
// 管理面看不到，运维只能靠「路由配了却莫名 500」反推。调用方要把它带进快照，
// 让界面能标出来。
//
// 判定「未就绪」：一条可用凭据都没有。部分凭据可用时 provider 仍能服务，
// 不计入 failures。
func (p *Pool) PrepareFromStore(ctx context.Context, st *store.Store, masterKey []byte, cfg *config.Config) (map[string]*ProviderEntry, []ProviderFailure, error) {
	providers, err := st.ListProviders(ctx)
	if err != nil {
		return nil, nil, err
	}

	var failures []ProviderFailure
	newProviders := make(map[string]*ProviderEntry, len(providers))
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

		credErrs := 0

		creds, err := st.ListCredentials(ctx, sp.ID)
		if err != nil {
			p.logger.Error("failed to list credentials", "provider", sp.Slug, "error", err)
			failures = append(failures, ProviderFailure{
				ID: sp.ID, Slug: sp.Slug, Name: sp.Name,
				Stage:  "list_credentials",
				Reason: "读取凭据失败，该 provider 本次未建立任何上游连接",
				Detail: err.Error(),
			})
			continue
		}

		for _, sc := range creds {
			if !sc.Enabled {
				continue
			}

			apiKey, err := decryptCredentialKey(sc.APIKeyEnc, masterKey)
			if err != nil {
				p.logger.Error("failed to decrypt credential", "provider", sp.Slug, "credential", sc.Label, "error", err)
				credErrs++
				// 同一 provider 的多条凭据可能各自失败，界面上要看到的是
				// 「哪一条、为什么」，所以每条都留一条记录，而不是只留第一条。
				failures = append(failures, ProviderFailure{
					ID: sp.ID, Slug: sp.Slug, Name: sp.Name,
					Stage:  "decrypt",
					Reason: fmt.Sprintf("凭据「%s」解密失败", sc.Label),
					Detail: err.Error(),
				})
				continue
			}

			client, err := buildClient(prov, apiKey, cfg)
			if err != nil {
				p.logger.Error("failed to build client", "provider", sp.Slug, "credential", sc.Label, "error", err)
				credErrs++
				failures = append(failures, ProviderFailure{
					ID: sp.ID, Slug: sp.Slug, Name: sp.Name,
					Stage:  "build_client",
					Reason: fmt.Sprintf("凭据「%s」建客户端失败（协议/端点不合法？）", sc.Label),
					Detail: err.Error(),
				})
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

		// 「零可用凭据」才是真正的未就绪：一条可用凭据都没有，路由过来必然失败。
		// 部分凭据可用时 provider 仍能服务，那几条失败已在上面各自留痕。
		if len(prov.Credentials) == 0 && credErrs == 0 {
			failures = append(failures, ProviderFailure{
				ID: sp.ID, Slug: sp.Slug, Name: sp.Name,
				Stage:  "no_credentials",
				Reason: "没有已启用的凭据，该 provider 不会有可用上游",
			})
		}

		newProviders[sp.Slug] = prov
	}
	return newProviders, failures, nil
}

// Install 以构建好的新表原子替换运行状态。
//
// **熔断状态跨重建保留**：任何 admin 写操作都会触发一次重建，而熔断状态与
// 配置变更是正交的 —— 运维改一个模型的 context_width，不该让正在熔断中的
// 坏上游立刻复活并被打满。旧实现在这里 `make(map[string]*targetHealth)`
// 把全部计数与 until 清零，等于每次保存设置都重置全站熔断。
//
// liveTargetIDs 是重建后仍然存在的 target ID 全集（来自 route_targets）。
// 不在其中的旧条目被丢弃：targetID 每次保存链都重新生成，留着会单调堆积。
func (p *Pool) Install(providers map[string]*ProviderEntry, liveTargetIDs map[string]bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.providers = providers
	p.reindexLocked()

	kept := make(map[string]*targetHealth, len(liveTargetIDs))
	for tid := range liveTargetIDs {
		if h, ok := p.targets[tid]; ok {
			// 沿用旧的健康状态（含未到期的 until），但**清掉 halfOpen**。
			//
			// halfOpen 的含义是「有一个探测请求正在途」，只对该请求所在的
			// 进程实例有效。重建时上一个在途请求要么已完成（结果已记账、
			// 名额已释放），要么随旧池一起作废 —— 两种情况下把它继承下来
			// 都是错的，而错的方向是「永久不可用」：没有任何东西会再来
			// 释放它。until 没到期时本来就该熔断，保留它即可。
			h.halfOpen = false
			kept[tid] = h
		} else {
			kept[tid] = &targetHealth{}
		}
	}
	p.targets = kept
}

// ProvidersSnapshot 返回当前 provider 表的浅拷贝，仅供测试与诊断使用。
func (p *Pool) ProvidersSnapshot() map[string]*ProviderEntry {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make(map[string]*ProviderEntry, len(p.providers))
	for k, v := range p.providers {
		out[k] = v
	}
	return out
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
	cred, ok := p.credIndex[credID]
	if ok {
		cred.CooldownUntil = time.Now().Add(duration)
		cred.Status = "cooling"
	}
	store := p.cooldownSto
	p.mu.Unlock()

	if !ok {
		return
	}
	p.logger.Info("credential cooling down",
		"credential", credID, "until", cred.CooldownUntil, "duration", duration)
	// 必须落库：冷却是运行时状态，而池会被任何 admin 写操作重建。
	// 只改内存的话，重建时 PrepareFromStore 从库读到 cooldown_until=0，
	// 刚被判 401 无效的 key 立刻回到轮换里。
	if store != nil {
		if err := store.SetCredentialCooldown(context.Background(), credID, "cooling", cred.CooldownUntil); err != nil {
			p.logger.Warn("persist credential cooldown failed", "credential", credID, "error", err)
		}
	}
}

// RecordCredentialSuccess 在一把凭据成功响应后清除其冷却，恢复 healthy。
// 不这样做的话：一次偶发 5xx 把 key 打进 60s cooling，即便它马上又好了，
// 这一分钟内仍被 getHealthyCredentials 跳过 —— 对单 key provider 等于凭空造 outage。
func (p *Pool) RecordCredentialSuccess(credID string) {
	p.mu.Lock()
	cred, ok := p.credIndex[credID]
	changed := false
	if ok && (cred.Status != "healthy" || !cred.CooldownUntil.IsZero()) {
		cred.Status = "healthy"
		cred.CooldownUntil = time.Time{}
		changed = true
	}
	store := p.cooldownSto
	p.mu.Unlock()

	if !changed {
		return
	}
	p.logger.Info("credential recovered", "credential", credID)
	if store != nil {
		if err := store.SetCredentialCooldown(context.Background(), credID, "healthy", time.Time{}); err != nil {
			p.logger.Warn("persist credential recovery failed", "credential", credID, "error", err)
		}
	}
}

// TargetAvailable 报告链上某目标当前是否**可能**可打（未处于熔断冷却期）。
//
// 它是**纯查询**，不占用任何名额 —— 「冷却到期后只放一个探测请求」这个
// half-open 语义由 ClaimTargetProbe 承担，两者必须分开：调用方会先把整条链
// 问一遍，再按 failover_max_targets 预算截断，被截掉的目标根本没进请求
// 循环，也就永远等不到 RecordTargetSuccess/RecordTargetFailure 来释放名额。
// 副作用留在查询里会让「探测名额」被预算裁掉的健康目标永久占住：
// 主目标熔断期间明明有可用上游，却稳定返回 502，且必须人工调高预算或重启
// 才能恢复（Install 沿用旧健康状态，reload 也救不回来）。
func (p *Pool) TargetAvailable(targetID string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	h, ok := p.targets[targetID]
	if !ok {
		return true
	}
	return !time.Now().Before(h.until)
}

// ClaimTargetProbe 在**真的要向该目标发请求**时调用：原子地判断可用并占用
// half-open 探测名额。
//
// 冷却刚到期时只放行一个探测请求，其余请求仍看到不可用 —— 不这样做，
// 冷却到期瞬间所有请求会同时打向这个目标：若已自愈则是并发尖峰，若仍坏则
// 全体真实失败并各自计一次，熔断-惊群会按「冷却时长 + 攒满阈值的时长」
// 周期性循环。成败由 RecordTargetSuccess / RecordTargetFailure 收敛：
// 成功清零计数并释放名额，失败立刻重新熔断。
//
// 返回 false 表示「此刻不打」：仍在冷却中，或探测名额已被别的请求领走。
// 调用方应当把该目标当作本次不可用并继续沿链转移。
func (p *Pool) ClaimTargetProbe(targetID string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	h, ok := p.targets[targetID]
	if !ok {
		return true // 从未见过的目标没有熔断状态，直接可打
	}
	if time.Now().Before(h.until) {
		return false
	}
	if h.halfOpen {
		return false // 探测请求已在途，不放第二个
	}
	h.halfOpen = true
	return true
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
	h.halfOpen = false // 探测名额已消耗，无论成败都由本次结果裁决
	if h.consecutiveFails >= threshold {
		h.until = time.Now().Add(targetBreakerCooldown)
		h.consecutiveFails = 0
		p.logger.Warn("target circuit opened",
			"target", targetID, "threshold", threshold, "until", h.until)
	}
}

// RecordTargetSuccess 清零目标的连续失败计数（成功即认为健康）。
//
// **不碰until**：熔断期内的成功可能来自熔断之前就已发出的在途请求
// （非流式默认超时 120s，远长于 60s 熔断），拿它解除熔断等于让陈旧响应
// 推翻新判定的故障。只有冷却期真正结束后的成功才允许清零计数 ——
// 而那本来就由 TargetAvailable 的时间比较负责，无需显式动作。
func (p *Pool) RecordTargetSuccess(targetID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if h, ok := p.targets[targetID]; ok {
		h.consecutiveFails = 0
		h.halfOpen = false // 探测成功，目标确认健康，释放名额
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

	// 不跟随重定向。默认的 10 跳 + 跨主机跟随意味着：一个 302 就能把
	// 探测面从「管理员填的那个 endpoint」扩大到「任意主机」—— 包括
	// 169.254.169.254 这类元数据地址。API key 也会被带去新主机
	//（Go 在跨主机跳转时会剥离敏感头，但 URL 里的东西不会）。
	// ErrUseLastResponse 让 3xx 原样作为响应返回，fetchModels 走非-200
	// 分支把状态码报给管理员 —— 信息足够排障，探测能力被切断。
	client := &http.Client{
		Timeout: 15 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
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
		// 响应体片段**只进日志，不进 error**。
		//
		// 旧实现把上游响应体前 512 字节塞进 error，而这个 error 经
		// model_handler / provider_handler 原样回显给调用方。管理员把某
		// provider 的 endpoint 指到内网服务（127.0.0.1、169.254.169.254、
		// 任何未暴露端口）后调 discover，就拿到了内网的**可读**响应 ——
		// 这把盲打变成了读取原语。真实上游的错误页里也常带内部主机名、
		// 路径、框架版本。
		//
		// 单租户下「管理员能配任意 endpoint」本身不算越权（内网自建
		// LLM 服务是常见用法，禁掉内网地址会打断它），所以这里只切断
		// 「响应体回显」这一条，它才是把盲打变成读取的那一步。
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		slog.Warn("upstream models discovery returned non-200",
			"url", url, "status", resp.StatusCode, "body_preview", strings.TrimSpace(string(body)))
		return nil, fmt.Errorf("上游返回 HTTP %d（响应体已记入网关日志）", resp.StatusCode)
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
		// NoIdempotencyKey 关闭 SDK 自带的对话 POST 重试（传输层错误与 429/503）。
		// SDK v1.0.0 起把 chat POST 标为 RetryIdempotent（"behaves like RetryAlways"），
		// 而 MaxRetries 默认 2 —— 与 DESIGN「对话是 POST，SDK 不重试，重试由
		// 故障转移链承担」的假设相反。不改的话，非流式慢生成（响应头晚于
		// ResponseHeaderTimeout=60s）会被原样重发，上游重复生成重复计费，
		// 且 Idempotency-Key 对 DeepSeek/vLLM/各类中转普遍无效。
		// 网关自己的重试策略：故障转移链（route_targets），不是同一个上游反复打。
		rosetta.WithQuirks(rosetta.Quirks{NoIdempotencyKey: true}),
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
