package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
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
	probeToken  ProbeToken
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
	// halfOpenToken 标记「冷却刚到期、已放哪一个探测请求进去」。
	// 没有它，冷却到期瞬间所有在途请求会同时打向刚恢复（或仍坏）的目标，
	// 形成惊群 —— 要么瞬时并发尖峰，要么全体真实失败各自计一次。
	//
	// # 它只对**冷却到期后的恢复期**有意义（2026-10-21 修 P0-11）
	//
	// 此前 halfOpen 是一个**全局互斥位**：ClaimTargetProbe 无论目标是否
	// 处于冷却，都先查 halfOpen。于是任何时刻一个目标最多只允许 1 个
	// 在途请求 —— 熔断器的「故障期间限流」被误植成了「常态限流」。
	//
	// 后果：开启 failover 后健康路由的并发能力被压成 1，其余请求静默
	// 改投链上下一个目标（route_trace 记 skip/probe_busy_or_cooling）。
	// 链上第二个目标是另一家供应商时，这就是**在健康状态下把流量送往
	// 另一家供应商并按另一家计费**，而界面上熔断状态显示「正常」。
	//
	// 现在它的含义收紧为「这个目标正在半开恢复期，且已放一个探针进去」，
	// 由 recovering 标志划定边界（见 claimTargetProbeLocked）。
	//
	// 这里存令牌而不是 bool：熔断前已经发出的旧请求会在恢复期才返回，
	// 它的 success/failure/release 不能清掉恢复期里**另一个**新探针的名额。
	halfOpenToken ProbeToken
	// halfOpenCycle 记录占用名额的探针属于哪一次熔断周期。
	// 新一轮熔断会把 breakerCycle 加一，于是上一周期遗留的探针即使
	// 还没返回，也不会永久堵住新周期的探针名额。
	halfOpenCycle uint64
	// breakerCycle 每进入一次冷却期加一，用于识别跨周期的迟到结果。
	breakerCycle uint64
	// recovering 标记「这个目标正在半开恢复期」。
	//
	// 它把「健康的目标」与「冷却刚到期、正在半开恢复的目标」区分开 ——
	// 这两种状态在 until 上无法区分（健康目标的 until 是零值，而零值
	// 也可能是「已到期」）。没有这个标志，半开名额要么对健康目标生效
	// （吞吐塌成 1），要么对恢复期失效（惊群重现）。
	//
	// 它的生命周期严格对应「冷却到期 → 探针裁决」这一段：
	//   - RecordTargetFailure 触发熔断时置位（进入冷却）；
	//   - 冷却到期、第一次 ClaimTargetProbe 时仍置位（半开，限一个探针）；
	//   - RecordTargetSuccess（探针成功）清位 —— 目标已确认健康，
	//     回到「健康目标无限并发」；
	//   - RecordTargetFailure（探针失败）重新置位并推入新一轮冷却。
	//
	// 于是「曾经坏过但现在健康」与「从未坏过」在**放行行为上完全一致** ——
	// 区别只在熔断计数（consecutiveFails）上，而它本来就该记着历史失败。
	recovering bool
}

// ProbeToken 标识一次半开探针 claim。0 表示本次请求没有占用探针名额
// （健康目标、未开启故障转移，或目标级熔断被关闭）。
type ProbeToken uint64

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
	// ProxyURL 是该 provider 实际要用的出站代理（http/https/socks5）。
	// 空 = 不代理，走进程环境变量。
	//
	// 它是**派生**的：provider 行上只存 network_id，实际地址由 networks 表
	// 在读路径 JOIN 出来（见 store.Provider 的 ProxyURL 注释）。
	ProxyURL string
	// ProxyConfigError 记录该 provider 的代理配置为什么没生效（URL 非法）。
	// 非空时请求仍会发 —— 走直连 —— 但管理面据此把「代理没生效」说出来，
	// 而不是让运维对着一个连不上的上游瞎猜。
	//
	// 实际暴露方式是建池循环里的一条 warn 日志（见 PrepareFromStore），
	// 不是 ProviderFailure —— 那个清单的语义是「未就绪」，而配错代理的
	// provider 是就绪的，混进去会让管理面把一个能用的上游标成坏的。
	//
	// 为什么是字段而不是返回值：buildClient 的调用点有三处，而这三处
	// 各自对「构建失败」的处理策略不同（建池记 failure、探测路径直接返回）。
	// 加返回值会逼着三处都改签名，而它们真正需要区分的只是「能不能用」，
	// 不是「代理有没有配错」。
	ProxyConfigError error
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
			ProxyURL:   bp.ProxyURL,
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

			// 代理配错：回落直连、请求照常能发，所以不阻断建池（那会让 /v1
			// 全站断粮），但必须说出来。bootstrap 路径同样要 —— 它是零配置
			// 部署的入口，配错代理时的症状与库路径一模一样。
			if prov.ProxyConfigError != nil {
				p.logger.Warn("provider network proxy_url invalid; falling back to direct connection",
					"provider", bp.Slug, "error", prov.ProxyConfigError)
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
	p.Install(newProviders, LiveTargets{IDs: map[string]bool{}, Known: true})
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
//
// # 读失败不能当成「集合为空」（2026-10-21 修 A5-P1-2）
//
// 旧实现在读失败时返回空集，而 Install 的语义是「集合外的条目一律丢弃」
// —— 于是一次读取失败就把**全站熔断状态**清空了。
//
// 后果：一次短暂的读池抖动（SQLITE_BUSY / 磁盘瞬时故障）就让所有正在熔断的
// 坏上游立刻复活。它们会被再次打满、再次触发熔断、再次熔断——
// 而这一轮重复对上游的真实压力，在故障期内恰好放大。
//
// 更还有一层：Reload 的路径里「重建成功」与「熔断状态被主动清空」是两件事。
// 旧实现让它们同时发生，而管理面上的状态仍显示「已重建」——没人知道熔断被静默清了。
//
// 正确的区分是三态：
//   - Known=true 且集合非空 → 按集合过滤（正常路径）；
//   - Known=true 且集合为空 → 目标表里真的没了目标，清空熔断状态正确；
//   - Known=false       → 集合未知，**保留**全部既有熔断状态。
func liveTargetIDsFromStore(ctx context.Context, st *store.Store) LiveTargets {
	targets, err := st.ListAllRouteTargets(ctx)
	if err != nil {
		return LiveTargets{Known: false}
	}
	ids := make(map[string]bool, len(targets))
	for _, t := range targets {
		ids[t.ID] = true
	}
	return LiveTargets{IDs: ids, Known: true}
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
			ProxyURL:   sp.ProxyURL,
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

		// 该 provider 下各模型的思考能力，注入给 SDK 的模型档案（见
		// buildClient 的说明）。读失败不阻断建池：模型档案只影响「SDK 要不要
		// 拦一次思考请求」，而客户端、能力档案这些都已经配好了 —— 因为档案
		// 读不出来就把整个上游踢出池子，代价远大于收益，且症状会指向「凭据
		// 有问题」这个完全错误的方向。
		modelInfos := modelInfosForProvider(ctx, st, sp.ID, p.logger)

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

			client, err := buildClient(prov, apiKey, cfg, modelInfos...)
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

		// 代理配错：client 建成了（回落直连），请求照常能发，所以**不**
		// 记进 failures —— 那个清单的语义是「未就绪」，而这里 provider 是
		// 就绪的，混进去会让管理面把一个能用的上游标成坏的。
		//
		// 但也不能不出声：运维以为流量过了代理，实际在直连，排查「上游连不上」
		// 时会朝着完全错误的方向猜（代理挂了？IP 被封了？）。日志记一次，
		// 且带上 provider 与那个非法值 —— 前者定位是哪家，后者让人看出错在哪。
		if prov.ProxyConfigError != nil {
			p.logger.Warn("provider network proxy_url invalid; falling back to direct connection",
				"provider", sp.Slug, "error", prov.ProxyConfigError)
		}

		newProviders[sp.Slug] = prov
	}
	return newProviders, failures, nil
}

// LiveTargets 是「当前库里还存在的 route target」这个信息的完整表达。
//
// # 为什么不是一个纯集合（2026-10-21 修 A5-P1-2）
//
// 旧接口只传一个 map[string]bool，而下面的 Install 以「集合外一律丢弃」为语义。
// 这个接口无法区分「目标确实没了」与「我读失败了」——而两者的处置
// 完全不同：
//   - 目标确实没了 → 旧熔断状态应当丢掉（它们已无对应的目标）；
//   - 我读失败了   → 旧熔断状态应当**保留**（目标可能还在，只是我不知道）。
//
// 旧实现在读失败时返回空集 → 一次短暂的读池抖动就把全站熔断状态清空，
// 让正在熔断的坏上游立刻复活。
//
// Known=false 表示「集合未知」，Install 就保留全部既有熔断状态。
type LiveTargets struct {
	// IDs 是当前存在的 target ID 集合。Known=false 时无意义。
	IDs map[string]bool
	// Known 表示 IDs 是否可信（即「我成功读到了」。
	Known bool
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
func (p *Pool) Install(providers map[string]*ProviderEntry, live LiveTargets) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.providers = providers
	p.reindexLocked()

	// 集合未知（读库失败）→ 保留**全部**既有熔断状态。
	//
	// 原因见 LiveTargets 的注释：一次读池抖动就把全站熔断状态清空，
	// 会让正在熔断的坏上游立刻复活。而旧实现的“返回空集”恰好导致这一点。
	if !live.Known {
		return
	}

	kept := make(map[string]*targetHealth, len(live.IDs))
	for tid := range live.IDs {
		if h, ok := p.targets[tid]; ok {
			// 沿用旧的健康状态（含未到期的 until），但**清掉 halfOpenToken**。
			//
			// halfOpen 的含义是「有一个探测请求正在途」，只对该请求所在的
			// 进程实例有效。重建时上一个在途请求要么已完成（结果已记账、
			// 名额已释放），要么随旧池一起作废 —— 两种情况下把它继承下来
			// 都是错的，而错的方向是「永久不可用」：没有任何东西会再来
			// 释放它。until 没到期时本来就该熔断，保留它即可。
			h.halfOpenToken = 0
			h.halfOpenCycle = 0
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
	// until 必须在锁内取值：cred 是**共享指针**，解锁后别的 goroutine 可能正在
	// 改写 CooldownUntil/Status。锁外读会把撕裂值传进 SetCredentialCooldown，
	// 而冷却是**要落库持久化**的——写进去的就是错的值，且没有任何迹象。
	// （RecordCredentialSuccess 同理，见下。）
	var until time.Time
	var ok bool

	p.mu.Lock()
	cred, found := p.credIndex[credID]
	if found {
		cred.CooldownUntil = time.Now().Add(duration)
		cred.Status = "cooling"
		until = cred.CooldownUntil // 快照：锁外只用它，不再碰 cred
		ok = true
	}
	store := p.cooldownSto
	p.mu.Unlock()

	if !ok {
		return
	}
	p.logger.Info("credential cooling down",
		"credential", credID, "until", until, "duration", duration)
	// 必须落库：冷却是运行时状态，而池会被任何 admin 写操作重建。
	// 只改内存的话，重建时 PrepareFromStore 从库读到 cooldown_until=0，
	// 刚被判 401 无效的 key 立刻回到轮换里。
	if store != nil {
		if err := store.SetCredentialCooldown(context.Background(), credID, "cooling", until); err != nil {
			p.logger.Warn("persist credential cooldown failed", "credential", credID, "error", err)
		}
	}
}

// RecordCredentialSuccess 在一把凭据成功响应后清除其冷却，恢复 healthy。
// 不这样做的话：一次偶发 5xx 把 key 打进 60s cooling，即便它马上又好了，
// 这一分钟内仍被 getHealthyCredentials 跳过 —— 对单 key provider 等于凭空造 outage。
func (p *Pool) RecordCredentialSuccess(credID string) {
	// 与 MarkCredentialCooldown 同一个纪律：**锁内判断、锁外只用自己的局部值**。
	// 这里的落库参数是常量，天然不受竞态影响；但 changed 这个判定若放到锁外
	// 去读 cred.Status，就会与并发的 MarkCredentialCooldown 抢读同一行 ——
	// 后者会把 CooldownUntil 改成未来某一刻，而前者据此判定「本来就是 healthy」，
	// 于是**刚被冷却的凭据被误判为无需恢复**，落库与内存就此分叉。
	var changed bool
	var store CooldownStore

	p.mu.Lock()
	cred, ok := p.credIndex[credID]
	if ok && (cred.Status != "healthy" || !cred.CooldownUntil.IsZero()) {
		cred.Status = "healthy"
		cred.CooldownUntil = time.Time{}
		changed = true
	}
	store = p.cooldownSto
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
	// RLock 而非 Lock（2026-10-12 修 P2-17）：本函数只读 targets 与 h.until，
	// 不改任何状态（上方注释也写明「纯查询，不占任何名额」）。取写锁会让它
	// 与所有写路径（冷却 / 恢复记账）互相排队 —— 而它正跑在故障转移的候选
	// 筛选里，一排队就把 credIndex 的 O(1) 收益抵消掉了。
	p.mu.RLock()
	defer p.mu.RUnlock()
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
// 返回 (0, true) 表示健康目标可以直接打，但没有占用半开名额；
// 返回 (token, true) 表示领到了一次半开探针名额；
// 返回 (0, false) 表示「此刻不打」：仍在冷却中，或探测名额已被别的请求领走。
// 调用方应当把后者当作本次不可用并继续沿链转移。
func (p *Pool) ClaimTargetProbe(targetID string) (ProbeToken, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.claimTargetProbeLocked(targetID)
}

// claimTargetProbeLocked 是 ClaimTargetProbe 的加锁实现。
//
// # 三种状态，三种处置（2026-10-21 修 P0-11）
//
//  1. **健康目标**（!recovering）：
//     无限并发地放行。熔断器不该在健康期限流 —— 那不是熔断器的职责，
//     而是把「故障期间限流」误植成「常态限流」。
//
//  2. **仍在冷却中**（now < until）：
//     拒绝。冷却未到期，本来就不该打。
//
//  3. **半开恢复期**（recovering && 冷却已到期）：
//     只放**一个**探测请求。惊群只发生在这个瞬间：所有在途请求同时
//     打向刚恢复（或仍坏）的目标。要么瞬时并发尖峰，要么全体真实失败
//     各自计一次，把「冷却到期」变成周期性惊群。
//
// 第 3 种的「半开期」边界由 recovering 标志给出，而不是靠 until 判断 ——
// 因为 until 过期后会一直留着（过期），靠它判断会导致「过期即永久限流」。
// recovering 在探针成功时清掉（目标确认健康，回到第 1 种），在探针失败
// 时重新推入冷却（回到第 2 种）。
func (p *Pool) claimTargetProbeLocked(targetID string) (ProbeToken, bool) {
	h, ok := p.targets[targetID]
	if !ok {
		return 0, true // 从未见过的目标没有熔断状态，直接可打
	}
	if time.Now().Before(h.until) {
		return 0, false
	}
	if !h.recovering {
		// 从未熔断：健康目标不参与半开限流。
		return 0, true
	}
	// 冷却已过期，且仍在恢复期 —— 这才是「半开」。
	//
	// halfOpenToken 记的是「已放哪个探针进去、结果还没回来」。
	// 探针回来之后（成功或失败）它会被清掉；这里不设过期时间，
	// 因为「探针在途」这件事本身有明确的结束点（记账路径），
	// 与「冷却何时到期」是两件事。
	if h.halfOpenToken != 0 && h.halfOpenCycle == h.breakerCycle {
		return 0, false // 探测请求已在途，不放第二个
	}
	p.probeToken++
	if p.probeToken == 0 {
		p.probeToken++
	}
	h.halfOpenToken = p.probeToken
	h.halfOpenCycle = h.breakerCycle
	return h.halfOpenToken, true
}

// RecordTargetFailure 给目标累计一次失败；达到 threshold 则熔断 targetBreakerCooldown。
// threshold<=0 表示不启用目标级熔断（仅凭据级冷却生效）。
func (p *Pool) RecordTargetFailure(targetID string, token ProbeToken, threshold int) {
	if threshold <= 0 {
		p.ReleaseTargetProbe(targetID, token)
		// 熔断被显式关闭 → 这个目标**不进入恢复期**。
		// 不清它的话，targetHealth.recovering 会一直挂着，而调用方
		// （cmd/gateway）在 threshold=0 时仍会调 ClaimTargetProbe ——
		// 于是「运维以为已关闭熔断」的目标仍被限成一个探针，且余下的
		// 请求静默改投链上下一个目标（见 A5 P1-3）。
		p.mu.Lock()
		if h, ok := p.targets[targetID]; ok {
			h.recovering = false
		}
		p.mu.Unlock()
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
	// 进入「恢复期」：这个目标坏过，冷却到期后要走一次半开探测。
	// 健康目标（recovering=false）不受半开名额限制 —— 见
	// claimTargetProbeLocked 的三态说明。
	h.recovering = true
	if token != 0 && h.halfOpenToken == token {
		h.halfOpenToken = 0 // 当前探针已被本次结果消耗
		h.halfOpenCycle = 0
	}
	if h.consecutiveFails >= threshold {
		h.until = time.Now().Add(targetBreakerCooldown)
		h.consecutiveFails = 0
		// 新冷却周期：上一代探针即便仍在途，也属于旧周期。这里不清
		// halfOpenToken —— 它的迟到结果必须能按令牌被忽略；新周期
		// 的 claim 会通过 halfOpenCycle 判定它已失效并替换。
		h.breakerCycle++
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
func (p *Pool) RecordTargetSuccess(targetID string, token ProbeToken) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if h, ok := p.targets[targetID]; ok {
		h.consecutiveFails = 0
		// 只有当前探针自己的结果才能释放名额。熔断前发出的旧请求 token=0，
		// 或恢复期里上一代探针的过期 token，都不能误清新探针。
		if token == 0 || h.halfOpenToken != token {
			return
		}
		h.halfOpenToken = 0
		// 目标确认健康 → 退出恢复期。**必须**清掉它：否则这个目标
		// 在下一次冷却到期时仍会被当作「半开中」而限成一个探针 ——
		// 而它早就被证明是好的，那正是 P0-11 那个 bug 的残留形态。
		//
		// 注意这只在「冷却已过期」时才真正回到健康态：熔断期内的成功
		// 可能来自熔断之前就已发出的在途请求，拿它解除恢复期等于让
		// 陈旧响应推翻新判定的故障（见本函数上方关于 until 的说明）。
		// 因此这里只在 until 已过期时才清 recovering。
		if !time.Now().Before(h.until) {
			h.recovering = false
		}
	}
}

// ReleaseTargetProbe 归还一次**未走到记账**的探测名额。
//
// 名额的正常出路是 RecordTargetSuccess / RecordTargetFailure，但热路径上有
// 两种合法形态既领了名额、又永远不会走到记账：
//  1. 不可转移错误（如上游 400）：不参与故障转移记账，直接沿链返回；
//  2. 客户端中断：刻意不记失败（把用户点「停止」算成目标故障会把健康目标
//     误熔断），同样直接返回。
//
// 两种形态若不显式归还，halfOpenToken 永久非零，该目标被 ClaimTargetProbe
// 恒拒绝 —— 一次 400 就把链上的目标废到下一次 Install 为止，且这正发生在
// 上游故障期间（本来就在频繁转移）。
//
// 不得在已调用 Record* 之后再调。调用方用「已记账则不归还」的本地标志
// 保证这一点；令牌比较则保证迟到的旧请求不会误清新探针。
func (p *Pool) ReleaseTargetProbe(targetID string, token ProbeToken) {
	if token == 0 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if h, ok := p.targets[targetID]; ok {
		if h.halfOpenToken == token {
			h.halfOpenToken = 0
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
		ProxyURL:   p.ProxyURL,
	}
	// 探测路径同样注入模型档案（理由同 buildClient）。整份档案都带上是
	// 安全的：SDK 的思考闸门按 req.Model 逐条查，其余模型的条目对本次请求
	// 没有任何影响；而连通性测试走 ListModels，根本不经过闸门。
	//
	// 单模型探测之所以不受影响，还有 TestUpstreamModel 显式置
	// Thinking=nil 那一条（闸门对 nil 直接放行）—— 见那里的成本分析。
	return buildClient(entry, apiKey, cfg, modelInfosForProvider(ctx, st, p.ID, slog.Default())...)
}

// probeMaxTokens 是单模型可用性探测的 max_tokens。
//
// 取 1：探测的目的是回答「这个名字现在能不能真的推理」，而判断这一点只需
// 上游接受请求并产出一个 token —— 生成 1 个 token 与生成 1000 个对
// 「模型是否存在 / 账号有没有权限 / 名字有没有写错」给出的答案完全相同，
// 成本却相差三个数量级。这也是本函数相对「跑一次真实对话」的全部成本优势。
//
// 取 1 而不是 0：0 在 OpenAI 协议里含义是「由上游决定」，某些上游会因此
// 生成到 stop 或 max_output，把一次探测变成一次完整（且计费可观）的推理。
const probeMaxTokens = 1

// probeInput 是探测请求的提示词。
//
// 单字符 "hi" 而不是空串：空 messages 会被 rosetta 的 validate 直接拒绝
// （Messages must not be empty），而空 content 又被部分上游当成非法输入。
// "hi" 是最短的、所有协议都接受的输入，输入 token 数约 1~2 个。
const probeInput = "hi"

// ModelProbeResult 是一次单模型可用性探测的结果。
//
// 分三层信息，因为三者的可操作性完全不同：
//   - OK：这个模型现在能推理（唯一可信的「可用」证据）；
//   - LatencyMs：观测到的往返耗时，用于区分「可用但很慢」与「可用」；
//   - Err / InBand：失败原因。InBand 表示上游用 HTTP 200 + 错误体回绝
//     （部分中转站如此），此时 OK=false 但 HTTP 是成功的 —— 只看状态码
//     会把这种「200 但没推理」误判成可用。
type ModelProbeResult struct {
	OK        bool
	LatencyMs int64
	// OutputTokens / InputTokens 是上游回报的真实用量。探测的计费依据在这里 ——
	// 数值小（通常 output=1）是这个设计成立的前提，所以把它带出来让调用方
	// 有机会显示「这次探测花了多少」，而不是让成本隐形成事实。
	InputTokens  int64
	OutputTokens int64
	// Err 为 nil 表示请求在协议层成功；与 OK 的区别见 InBand。
	Err error
	// InBand 为真表示上游以 2xx 返回了错误体（InBand=true 的 APIError）。
	InBand bool
}

// TestUpstreamModel 对**单个上游模型**做一次最小真实推理，验证它现在可用。
//
// # 为什么不能用 ListModels 代替（本函数存在的全部理由）
//
// provider 级的连通性测试调的是 client.ListModels，它证明的是
// 「这个 endpoint + 这把凭据能拉到模型列表」。这与「某个具体 model_id
// 能推理」是两件不同的事，而且差别恰好落在管理员最需要答案的几种情形上：
//   - 模型已下架 / 改名：仍在 /models 里缓存着，或干脆不在列表里而配置还留着；
//   - 账号对该模型无权限：列表里看得见，调用返回 403/404；
//   - 名字写错 / 大小写不符：列表能通，调用报 model_not_found；
//   - 模型需要特定参数（如 reasoning 模型拒绝某些字段）：列表完全测不出来。
//
// 所以「测试模型是否可用」必须真的发一次推理请求。代价是真的花钱 —— 见
// probeMaxTokens 的取值理由，以及本函数只做**非流式**单次调用的原因：
// 流式会在同一件事上多引入 SSE 解析、首字看门狗与断流判定三条失败路径，
// 而探测要回答的问题不需要其中任何一条。buildClient 已用
// WithQuirks{NoIdempotencyKey: true} 关掉了 SDK 的对话 POST 重试，
// 因此一次探测最多对上游产生一次计费调用（不会因重试翻倍）。
//
// 注意：这里的开销走的是 **provider 自带凭据**（resolveCredential 取的那把），
// 与请求它的管理员账号余额无关，因此不经 usage_recorder、不扣任何网关用户的钱。
func TestUpstreamModel(ctx context.Context, st *store.Store, m *store.UpstreamModel, masterKey []byte, cfg *config.Config) ModelProbeResult {
	start := time.Now()

	client, err := NewProviderClient(ctx, st, m.ProviderID, masterKey, cfg)
	if err != nil {
		// 建不出客户端 = 没有可用凭据 / provider 配置非法。这不是「模型不可用」，
		// 但探测结果里它同样是「现在打不通」，所以照实返回原因。
		return ModelProbeResult{Err: err}
	}

	req := &rosetta.ChatRequest{
		Model:           m.ModelID,
		Messages:        []rosetta.Message{rosetta.User(probeInput)},
		MaxOutputTokens: probeMaxTokens,
		// 显式不请求思考：推理模型的思考预算会绕过 max_tokens 下限
		// （Anthropic 侧 budget >= max_tokens 时会把 max_tokens 顶到
		// budget+4096），一次「最小」探测会因此变成几千 token。探测要的是
		// 「能不能推理」，不是「思考得对不对」。
		Thinking: nil,
	}

	resp, err := client.Chat(ctx, req)
	latency := time.Since(start).Milliseconds()
	if err != nil {
		// 把上游错误原样带出：那是管理员唯一能据以行动的信息（401 换 key、
		// 404 改模型名、403 开权限）。rosetta 的 APIError.Error() 已对凭据
		// 做掩码（httpx.MaskSecrets），不会把 key 回显出来。
		var apiErr *rosetta.APIError
		return ModelProbeResult{
			LatencyMs: latency,
			Err:       err,
			InBand:    errors.As(err, &apiErr) && apiErr.InBand,
		}
	}

	return ModelProbeResult{
		OK:           true,
		LatencyMs:    latency,
		InputTokens:  resp.Usage.InputTokens,
		OutputTokens: resp.Usage.OutputTokens,
	}
}

// ModelCandidate 是模型发现返回的原始条目；容量字段为 0 表示上游未暴露。
type ModelCandidate struct {
	ID              string
	DisplayName     string
	ContextWindow   int
	MaxOutputTokens int
	// EffortLevels 是上游自报的思考挡位集合，**空表示上游没报**（而不是
	// 「不支持思考」—— 这两件事必须可区分，见 rawModelEntry 的注释）。
	//
	// 2026-10-14 新增：effort_levels 原本是纯手工填的列，但部分上游的
	// /models 明明带着答案（实测某上游返回
	// reasoning_supported_efforts=[low,medium,high,xhigh,max] 且
	// reasoning_default_effort=max）。手工填意味着管理员要么不知道、
	// 要么照着文档抄 —— 于是网关不披露、也不夹取，客户端发的档位裸奔。
	// 让发现路径把它带上来，导入时就填好，配置与上游同源。
	EffortLevels []string
	// SupportsThinking 三态：nil = 上游没报（不知道），指针 = 上游明说。
	// 用指针而不是 bool 是为了保住「不知道」—— 把「没报」当成 false 会让
	// 全部模型的思考被剥掉。
	SupportsThinking *bool
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

	// 思考能力。**多个字段名都收**，因为各家写法不统一且没有约定：
	//   - reasoning_supported_efforts：实测某国产上游用这个（值形如
	//     ["low","medium","high","xhigh","max"]）
	//   - supported_reasoning：OpenRouter 约定
	//   - supported_efforts / effort_levels：另一些网关的写法
	// 都收下是为了「上游报了就能用」，而不是猜哪个才是标准 ——
	// 猜错的后果是配置永远为空，而空 = 网关不干预 = 静默。
	ReasoningSupportedEfforts []string `json:"reasoning_supported_efforts"`
	SupportedReasoning        []string `json:"supported_reasoning"`
	SupportedEfforts          []string `json:"supported_efforts"`
	EffortLevelsRaw           []string `json:"effort_levels"`

	// supports_reasoning / supports_thinking：同样是多写法，且**必须用
	// *bool** —— 缺席（nil）与显式 false 是两件事：前者是「不知道，交给
	// 上游」，后者是「明确不支持，剥掉思考」。
	SupportsReasoning *bool `json:"supports_reasoning"`
	SupportsThinking  *bool `json:"supports_thinking"`
	// Reasoning 是最麻烦的一个字段名：它在不同上游是**两种不兼容的形状**。
	//
	// 实测 OpenRouter（2026-10-15，462 个模型）给的是**对象**：
	//
	//	"reasoning": {"mandatory":true,"supported_efforts":["high","medium","low"],
	//	               "default_effort":"medium"}
	//	"reasoning": {"mandatory":false,"default_enabled":true}
	//
	// 而某些上游给的是裸布尔。原先这里声明成 *bool，OpenRouter 的对象形态
	// 直接触发 `json: cannot unmarshal object into Go struct field
	// .data.2.reasoning of type bool` —— **整个模型列表解析失败**，
	// 「获取模型」整个功能不可用（不是某一个模型出错，是全部）。
	//
	// 两种形状的解读见 flexBool 的注释。
	Reasoning flexBool `json:"reasoning"`
}

// flexBool 收「可能是布尔、也可能是对象」的思考能力字段。
//
// # 为什么需要它
//
// `reasoning` 这个字段名在不同上游是**两种不兼容的形状**。实测 OpenRouter
// （2026-10-15，462 个模型全量统计）给的是**对象**，0 个模型给裸布尔：
//
//	"reasoning": {"mandatory":true,"supported_efforts":["high","medium","low"],
//	               "default_effort":"medium"}
//	"reasoning": {"mandatory":false,"default_enabled":true}
//
// 声明成 *bool 时，OpenRouter 的对象形态会触发
// `json: cannot unmarshal object into Go struct field .data.N.reasoning of
// type bool` —— 而这是**整个列表**解析失败，不是一个模型出错：
// 「获取模型」功能对 OpenRouter 完全不可用。
//
// # 两种形状各自怎么解读
//
// 对象形态（OpenRouter）的 `mandatory` 语义**不是**「支持思考」而是
// 「思考是强制的」—— 强制蕴含支持，于是 Mandatory=true → 支持；
// 但 Mandatory=false 不等于不支持，它只说明「思考可选」，而 OpenRouter
// 在这条分支上通常还给了 default_enabled 或 supported_efforts。
// 所以判定顺序是：有 supported_efforts → 支持（上游明确列了档位）；
// 否则取 mandatory。这个顺序不能反 —— 反了会把「思考可选」的模型
// 误判成「不支持」，进而被网关剥掉思考。
type flexBool struct {
	set   bool
	value bool
	// isObj 区分「布尔形态」与「对象形态」。
	//
	// **必须有这个标志**：boolPtr 的判定要读 obj.Mandatory，而布尔形态下
	// obj 是零值（Mandatory=false）。若不区分，布尔 `true` 会被读成
	// obj.Mandatory=false —— 于是「上游明说支持」被翻转成「不支持」，
	// 网关会开始剥掉这个模型的思考。那是比原 bug 更糟的结果：原 bug
	// 是整个功能不可用，这个是**静默地做错事**。
	isObj bool
	// obj 收对象形态里的其余字段。effort 集合必须收：OpenRouter 把它
	// 放在 reasoning 对象**内部**而非顶层平铺，不收的话接 OpenRouter 时
	// effort_levels 全空 —— 而空 = 网关不干预 = 静默，客户端发的思考档位
	// 裸奔，正是这个字段存在的意义要防的事。
	obj struct {
		Mandatory        bool     `json:"mandatory"`
		DefaultEnabled   bool     `json:"default_enabled"`
		SupportedEfforts []string `json:"supported_efforts"`
		DefaultEffort    string   `json:"default_effort"`
	}
}

// UnmarshalJSON 接受布尔、对象、以及 null 三种形态。
//
// 遇到**不认识的形状**（字符串、数字、数组）返回 error 而不是静默忽略：
// 静默忽略的后果是「上游报了但我们没读到」，而空 = 网关不干预 = 静默 ——
// 那正是最该避免的结果。宁可让整次发现失败并报出形态，
// 也不要给管理员一个「配置成功但其实没填上」的假象。
func (f *flexBool) UnmarshalJSON(b []byte) error {
	trimmed := strings.TrimSpace(string(b))
	if trimmed == "null" {
		return nil
	}
	// 第一字节定形态，避免先 unmarshal 到 any 再断言类型。
	switch trimmed[0] {
	case 't', 'f':
		var v bool
		if err := json.Unmarshal(b, &v); err != nil {
			return err
		}
		f.set, f.value, f.isObj = true, v, false
		return nil
	case '{':
		if err := json.Unmarshal(b, &f.obj); err != nil {
			return err
		}
		f.set, f.isObj = true, true
		return nil
	default:
		return fmt.Errorf("reasoning: 不认识的形状 %q（期望布尔或对象）", trimmed)
	}
}

// boolPtr 返回三态结果：nil = 上游没报（不知道，交给上游）。
func (f *flexBool) boolPtr() *bool {
	if !f.set {
		return nil
	}
	if !f.isObj {
		v := f.value
		return &v
	}
	if len(f.obj.SupportedEfforts) > 0 {
		// 明确列了档位 = 明确支持。mandatory 在这里不参与判定。
		t := true
		return &t
	}
	v := f.obj.Mandatory
	return &v
}

// effortLevels 取对象形态里的 supported_efforts（布尔形态恒为 nil）。
func (f *flexBool) effortLevels() []string {
	return f.obj.SupportedEfforts
}

// effortLevelsFor 从各家字段名里取出第一个非空的挡位集合。
//
// 只取第一个**非空**的：某些上游会同时给多个（有的空、有的有值），
// 取第一个非空比取第一个字段更稳 —— 后者可能正好落在那个空数组上。
func (e *rawModelEntry) effortLevelsFor() []string {
	for _, cand := range [][]string{
		e.ReasoningSupportedEfforts,
		e.SupportedReasoning,
		e.SupportedEfforts,
		e.EffortLevelsRaw,
		// OpenRouter 把 supported_efforts 放在 reasoning 对象**内部**，
		// 不是顶层平铺。放最后：顶层那些字段一旦有值就更权威（同一家
		// 不会同时给两套矛盾值），这里只在它们都缺席时兜底。
		e.Reasoning.effortLevels(),
	} {
		if len(cand) > 0 {
			return cand
		}
	}
	return nil
}

// supportsThinkingFor 同理取第一个**非 nil** 的布尔。
//
// 这里刻意不能用 `if e.SupportsThinking != nil` 优先，因为一个上游可能
// 同时给 supports_reasoning=true 与 supports_thinking=false（自相矛盾的
// 情况下不能猜），所以顺序固定为 reasoning → thinking → reasoning 别名。
func (e *rawModelEntry) supportsThinkingFor() *bool {
	if e.SupportsReasoning != nil {
		return e.SupportsReasoning
	}
	if e.SupportsThinking != nil {
		return e.SupportsThinking
	}
	return e.Reasoning.boolPtr()
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
	urls := modelListURLs(base)

	// 与数据面使用同一个 provider 出站网络，同时禁止重定向：一个 302
	// 就能把带 API key 的探测扩大到任意主机，包括云元数据地址。
	client, err := probeHTTPClientFor(p.ProxyURL, 15*time.Second)
	if err != nil {
		return nil, err
	}
	var lastErr error
	// triedErrs 按候选顺序留痕：全部候选都失败时，管理员需要知道网关
	// **试过哪些路径**，否则「endpoint 填错了」与「上游不提供模型列表」
	// 在界面上是同一句话，只能靠猜。
	triedErrs := make([]string, 0, len(urls))
	for _, u := range urls {
		cands, err := fetchModels(ctx, client, u, p.Protocol, apiKey)
		if err != nil {
			lastErr = err
			triedErrs = append(triedErrs, fmt.Sprintf("%s → %v", u, err))
			continue
		}
		return cands, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("无法获取模型列表")
	}
	// errors.Join 需要 []error：每个候选的「路径 → 原因」各占一条，
	// lastErr 再单列（它已经是最后一条路径的 error，重复会让文案变啰嗦）。
	parts := make([]error, 0, len(triedErrs)+1)
	for _, e := range triedErrs {
		parts = append(parts, errors.New(e))
	}
	parts = append(parts, lastErr)
	return nil, fmt.Errorf("模型列表探测失败（已尝试 %d 个路径）：%w", len(urls), errors.Join(parts...))
}

// modelListURLs 给出模型列表端点的候选路径，按「最可能命中」排序。
//
// # 为什么要多个候选
//
// 管理员填的 endpoint 形态并不统一：有人填到域根（https://api.x.com），
// 有人填到已经带版本的那一层（https://api.x.com/v1）。前者要的模型端点是
// /v1/models，后者要的是 /v1/models 之下**不能**再加一次 /v1。
//
// 修复前非 anthropic 协议只试 /models 一个路径，于是「endpoint 填域根 +
// OpenAI 兼容」这个最常见的组合（DeepSeek、Moonshot、OpenRouter、vLLM、
// 各类中转都如此）必然 404 —— 而真实错误会被报成「上游返回 HTTP 404」，
// 指向管理员去检查 API Key，而 key 其实完全正常。
//
// 候选表只含三种形态，都来自实际部署：
//   - base 已以 /vN 结尾 → base/models（不重复加版本）
//   - base 以 /v1 结尾 → base/models
//   - 域根 → 先试 base/v1/models（OpenAI 系的事实标准），再退回 base/models
//
// 三家上游（OpenAI 系、Anthropic、vLLM）的模型列表都在 /v1/models，
// 所以**不按协议分候选**：候选只取决于 base 的形状。保留 protocol 参数是为了
// 将来出现协议专属路径时有地方落，当前形态下它不参与判定。
func modelListURLs(base string) []string {
	if hasVersionSuffix(base) {
		return []string{base + "/models"}
	}
	return []string{base + "/v1/models", base + "/models"}
}

// hasVersionSuffix 判断 base 是否已经以 /vN（N 为数字）结尾。
//
// 只认结尾一段：路径中间出现的 /v1（如 /proxy/v1）不算，那仍是可以再拼
// 一层版本的中转路径。用 path 而非字符串后缀匹配是为了避免
// "https://host/v1beta" 这类被判成已带版本。
func hasVersionSuffix(base string) bool {
	u, err := url.Parse(base)
	if err != nil {
		return false
	}
	last := u.Path
	if i := strings.LastIndex(last, "/"); i >= 0 {
		last = last[i+1:]
	}
	if len(last) < 2 || last[0] != 'v' {
		return false
	}
	for _, c := range last[1:] {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
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
			// 思考能力：原样带给 admin 层，由那边做「上游报了才填」的判定。
			// 这里**不做**任何档位清洗 —— 网关的 effort 包是 admin 层的事，
			// 在这一层归一会丢掉 UnknownLevels 机制（厂商私有值需要原样保留）。
			EffortLevels:     e.effortLevelsFor(),
			SupportsThinking: e.supportsThinkingFor(),
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
		// 手搓 transport **不会**继承 net/http 的这两个默认值
		// （它们只在 http.DefaultTransport 上预置）。缺了会怎样：
		//
		//   - 无 DialContext：内核 TCP 握手默认要等 2 分钟+
		//     （Windows 更久），黑洞上游（SYN 无响应、丢包）的连接建立
		//     会一直挂着，直到 ctx 超时或系统级 TCP 超时。
		//   - 无 TLSHandshakeTimeout：TLS 握手阶段同样无上限，
		//     对端接受 TCP 后不回应 TLS 记录即永久挂起。
		//
		// 后果不是「慢」而是「失败检测被拖到看门狗」：本该在几秒内判定
		// 「这个上游死了」并走故障转移，实际要拖到 30s/120s 的流空闲 /
		// 非流式看门狗才触发，期间这一个目标占着整条链的请求预算。
		//
		// 值取 10s：本地与同机房上游的握手在百毫秒级，10s 足够宽松；
		// 而它把「上游已死」的判定从分钟级拉回十秒级。
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSHandshakeTimeout: 10 * time.Second,
	},
}

// upstreamProxyClients 按 proxy URL 缓存「走该代理的 http.Client」。
//
// # 为什么不能只改那一个共享 client 的 Proxy
//
// Proxy 是 **Transport 级**的属性，而 Transport 由 client 持有。一个全局
// 共享 client 意味着「要么全走代理、要么全不走」—— 而现实是上游服务商的
// 网络位置差异很大：api.openai.com 要翻墙、deepseek 直连即可、公司内网
// 的 vLLM 只能走内网代理。做成全局开关会让能直连的那些也平白多绕一跳，
// 延迟和故障面都上去。
//
// # 为什么按 URL 缓存而不是每把凭据建一个
//
// 每个 Transport 自带一套连接池。同一个 provider 下 N 把凭据若各建一个，
// 就是 N 套到同一个代理的连接池 —— MaxIdleConnsPerHost=64 会被 N 等分，
// 且代理侧同时维持 N 倍的空闲连接。而「同一 provider 的凭据共享代理配置」
// 是既定事实（出站网络挂在 provider 上，不在凭据行上），所以按 URL 缓存
// 恰好命中这个粒度。多个 provider 选同一条网络时也共享同一个 client ——
// 那正是「共享清单」想达成的效果。
//
// # 为什么值里放 *http.Client 而不是 *http.Transport
//
// buildClient 需要的是 client（WithHTTPClient 收 *http.Client）。放在值里
// 顺带把 Transport 的构造细节收拢在这一处。
var (
	upstreamProxyMu       sync.Mutex
	upstreamProxyClients  = make(map[string]*http.Client)
	upstreamProxyBuildErr = make(map[string]error)
)

// proxyHTTPClientFor 返回走指定代理的 client；proxyURL 为空表示不代理
// （回落到进程环境变量），此时返回全局共享的那个。
//
// 代理 URL 非法时返回错误，同时回落全局 client（调用方 buildClient 把错误
// 记进 prov.ProxyConfigError）。理由：代理配错是**配置错误**，不是运行时
// 故障 —— 但让整个 provider 建池失败会把症状变成「路由配好了、请求莫名 500」，
// 而根因躺在建池日志里没人看（这正是 ProviderFailure 机制要消灭的东西）。
// 更合适的做法是让它继续用直连 client 服务，同时把「代理没生效」说出来。
func proxyHTTPClientFor(proxyURL string) (*http.Client, error) {
	if strings.TrimSpace(proxyURL) == "" {
		return upstreamHTTPClient, nil
	}
	u, err := url.Parse(proxyURL)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https" &&
		u.Scheme != "socks5" && u.Scheme != "socks5h") {
		return upstreamHTTPClient, fmt.Errorf("网络 %q 的代理地址不合法（支持 http/https/socks5，如 http://127.0.0.1:7897）", proxyURL)
	}

	upstreamProxyMu.Lock()
	defer upstreamProxyMu.Unlock()
	if c, ok := upstreamProxyClients[proxyURL]; ok {
		return c, nil
	}
	// 代理地址的 scheme 已校验，这里不再判错；http.ProxyURL 只吃 *url.URL。
	tr := &http.Transport{
		Proxy: http.ProxyURL(u),
		// 其余参数与全局 client 逐字相同 —— 抄一份而不是共用 Transport：
		// Proxy 不同就必须是不同的 Transport，而池子参数不一致会让「走代理的
		// 上游」和「直连的上游」在连接复用、超时行为上表现不同，
		// 排查时会被这个差异带偏。
		MaxIdleConns:          128,
		MaxIdleConnsPerHost:   64,
		IdleConnTimeout:       90 * time.Second,
		ForceAttemptHTTP2:     true,
		ResponseHeaderTimeout: 60 * time.Second,
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSHandshakeTimeout: 10 * time.Second,
	}
	c := &http.Client{Transport: tr}
	upstreamProxyClients[proxyURL] = c
	return c, nil
}

// probeHTTPClientFor 为模型发现、余额等辅助查询构造 client。
//
// 它与数据面共用 provider 的出站网络和连接池，但额外设置短超时并禁止
// 重定向 —— 这些请求携带 API key，且不该因为辅助功能拖住管理页面。
func probeHTTPClientFor(proxyURL string, timeout time.Duration) (*http.Client, error) {
	base, err := proxyHTTPClientFor(proxyURL)
	if err != nil {
		return nil, err
	}
	c := *base
	c.Timeout = timeout
	c.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &c, nil
}

func buildClient(prov *ProviderEntry, apiKey string, cfg *config.Config, modelInfos ...rosetta.ModelInfo) (*rosetta.Client, error) {
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
	}
	// 代理：为空走全局共享 client（读进程环境变量），否则用该 provider
	// 独享的 client。代理配错时**不失败**，而是回落到全局 client 并把错误
	// 记进 ProviderFailure —— 理由见 proxyHTTPClientFor 的注释。
	hc, proxyErr := proxyHTTPClientFor(prov.ProxyURL)
	opts = append(opts, rosetta.WithHTTPClient(hc))
	prov.ProxyConfigError = proxyErr
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

	// 把网关这一侧配好的模型能力注入 SDK 的模型档案。
	//
	// 为什么在这里而不是各自调用点：模型能力是**这个上游的**属性，而一个
	// client 恰好就是「一个上游 + 一把凭据」—— 数据面、探测、连通性测试三条
	// 路径都经过这里，注入一次就全覆盖，漏不掉。
	//
	// 为什么值得注入：这让 SDK 自己的 gateThinking 生效 —— 它会在「模型
	// 明确声明不支持思考」时返回 ErrThinkingUnsupported，而不是把一个思考
	// 请求发给一个根本不思考的模型、拿回一个 200 与一段不含推理的正文。
	// 注入前这个机制对本网关**形同虚设**：SDK 只信「Known」的条目，而除了
	// models 文件，网关从没告诉过它任何模型叫什么。
	//
	// 可变参数而非新参数：绝大多数调用点（bootstrap 配置、探测、连通性测试）
	// 没有模型档案可注入，而为一个「通常没有」的参数改全部调用点的签名
	// 只会制造噪音。
	if len(modelInfos) > 0 {
		opts = append(opts, rosetta.WithModelInfo(modelInfos...))
	}

	return rosetta.NewClient(opts...)
}

// modelInfosForProvider 把库里配好的模型思考能力转成 SDK 的 ModelInfo。
//
// 只转「思考能力」这一组字段：上下文窗口、价格这些网关自己在快照里用，
// 不必再抄一份进 SDK —— 抄了就会出现两份可能分叉的真相，而这正是这次要
// 消灭的东西。
//
// 三态的处理是这里最要紧的一处：`SupportsThinking` 是**可空**的（nil =
// 未配置），而 SDK 的 ModelInfo.Known 语义是「这一条来自手工配置、可信」。
// 因此：
//   - 配了 true  → SupportsThinking=true，SDK 不拦（正常放行）；
//   - 配了 false → SupportsThinking=false，SDK 对思考请求返回
//     ErrThinkingUnsupported（这正是我们要的执行点）；
//   - 没配（nil）→ **不产出条目**。产出一条 Known=true 且
//     SupportsThinking=false 的条目，等于宣称「这个模型不思考」——
//     而事实是「不知道」。那会把一个未配置的普通模型变成思考请求的
//     拒绝源，比不做这个功能更糟。
//
// 不产出条目时 SDK 不认得这个模型，gateThinking 对它直接放行（`!ok`
// 分支），即完全保持旧行为。
func modelInfosForProvider(ctx context.Context, st *store.Store, providerID string, logger *slog.Logger) []rosetta.ModelInfo {
	models, err := st.ListUpstreamModels(ctx, providerID)
	if err != nil {
		logger.Warn("failed to read model capabilities for provider (thinking gate disabled)",
			"provider", providerID, "error", err)
		return nil
	}
	infos := make([]rosetta.ModelInfo, 0, len(models))
	for _, m := range models {
		if m.SupportsThinking == nil && len(m.EffortLevels) == 0 {
			continue
		}
		info := rosetta.ModelInfo{ID: m.ModelID}
		if m.SupportsThinking != nil {
			info.SupportsThinking = *m.SupportsThinking
		}
		if m.SupportsThinking != nil && !*m.SupportsThinking {
			// 显式否认思考时必须同时设 DisableThinking：合并语义是 OR，
			// 单靠 SupportsThinking=false 压不过任何低优先级层（见 SDK 的
			// mergeInfo 注释）。设了它，「配了挡位」也不会把思考复活。
			info.DisableThinking = true
		}
		if len(m.EffortLevels) > 0 {
			levels := make([]string, 0, len(m.EffortLevels))
			for _, l := range m.EffortLevels {
				levels = append(levels, string(l))
			}
			info.EffortLevels = levels
		}
		infos = append(infos, info)
	}
	return infos
}

func resolveEnv(name string) string {
	return os.Getenv(name)
}

func decryptCredentialKey(enc []byte, masterKey []byte) (string, error) {
	if len(masterKey) == 0 {
		return "", crypto.ErrNoMasterKey
	}
	plain, err := crypto.Decrypt(enc, masterKey)
	if err != nil {
		return "", err
	}
	return string(plain), nil
}
