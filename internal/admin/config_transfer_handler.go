package admin

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/cn-maul/rosetta-gateway/internal/config"
	"github.com/cn-maul/rosetta-gateway/internal/effort"
	wireproto "github.com/cn-maul/rosetta-gateway/internal/protocol"
	"github.com/cn-maul/rosetta-gateway/internal/store"
)

// ConfigTransferHandler 负责供应商 + 模型的导出与导入。
//
// 为什么只做这两样：它们是「换个部署还要再配一遍」的东西，而路由与故障
// 转移链是**本部署的组织结构**（公开名是给调用方看的契约，跨环境照抄会
// 撞名），访问密钥与分组则根本不该离开这个库。
//
// 导入的语义是**只增不改**：已存在的 slug 原样保留，新来的改名加后缀。
// 这比"覆盖"安全 —— 覆盖要先删，删错了就没了；而加后缀最坏结果是库里
// 多一个重复的供应商，用户自己能看出来并删掉。
type ConfigTransferHandler struct {
	store     *store.Store
	masterKey []byte
	cfg       *config.Config
}

func NewConfigTransferHandler(st *store.Store, masterKey []byte, cfg *config.Config) *ConfigTransferHandler {
	return &ConfigTransferHandler{store: st, masterKey: masterKey, cfg: cfg}
}

// exportRequest 是导出请求。
//
// Passphrase **必填**（2026-10-07 起）：导出体总是包含上游凭据，「只导结构」的
// 明文模式已删除。理由：明文与加密两种产物让导入侧要兼容两套形态，而明文带
// 凭据的文件（微信/邮件/网盘是它最常见的去向）等于把 API Key 白送；
// 收敛成单一形态 —— **必带凭据 + 必加密**，产物统一是 .json.enc。
type exportRequest struct {
	Passphrase string `json:"passphrase"`
}

// Export 打包当前库里的供应商与模型。
func (h *ConfigTransferHandler) Export(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireAdmin(w, r); !ok {
		return
	}
	var req exportRequest
	// 请求体是可选的：没有 body 时按「缺口令」处理，走下面统一的报错，
	// 让调用方拿到的永远是同一句「必须设置加密口令」而不是 JSON 解析错误。
	if r.ContentLength != 0 {
		if err := decodeJSON(w, r, &req); err != nil {
			if errors.Is(err, errUnsupportedMediaType) {
				return
			}
			writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}
	}

	pass := strings.TrimSpace(req.Passphrase)
	if pass == "" {
		writeError(w, http.StatusBadRequest,
			"必须设置加密口令（至少 8 位）：导出文件包含全部上游 API Key，不允许明文导出")
		return
	}

	body, err := h.buildExport(r.Context())
	if err != nil {
		writeServerError(w, "build export", err)
		return
	}

	payload, err := sealExport(body, pass)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// 走文件下载而非 JSON 响应：响应体会被浏览器内联显示，而这份内容
	// 含全部凭据，落到磁盘才是用户预期的去向。
	stamp := time.Now().Format("20060102-150405")
	name := "rosetta-config-" + stamp + ".json.enc"
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Disposition", "attachment; filename="+strconv.Quote(name))
	w.Header().Set("Cache-Control", "no-store")
	if err := writeIndentedJSON(w, payload); err != nil {
		slog.Error("config export: write failed", "error", err)
	}
}

// buildExport 从库里读出供应商与模型（含凭据 —— 导出总是带全量数据）。
func (h *ConfigTransferHandler) buildExport(ctx context.Context) (configExport, error) {
	out := configExport{
		Version:    exportFormatVersion,
		ExportedAt: time.Now().UnixMilli(),
	}

	providers, err := h.store.ListProviders(ctx)
	if err != nil {
		return out, err
	}
	models, err := h.store.ListAllUpstreamModels(ctx)
	if err != nil {
		return out, err
	}
	byProvider := make(map[string][]store.UpstreamModel, len(providers))
	for _, m := range models {
		byProvider[m.ProviderID] = append(byProvider[m.ProviderID], m)
	}

	// 归组先读出来建 id→name 映射：模型导出里用名字关联（跨库 id 无意义），
	// 一次查询而不是每个模型现取，避免 N+1。
	groups, err := h.store.ListModelGroups(ctx)
	if err != nil {
		return out, err
	}
	groupName := make(map[string]string, len(groups))
	for _, g := range groups {
		groupName[g.ID] = g.DisplayName
		out.ModelGroups = append(out.ModelGroups, modelGroupExport{DisplayName: g.DisplayName})
	}
	// 出站网络同样先读出来建 id→name 映射：导出体里用名字关联（跨库 id
	// 无意义），一次查询而不是每个 provider 现取，避免 N+1。
	networks, err := h.store.ListNetworks(ctx)
	if err != nil {
		return out, err
	}
	networkName := make(map[string]string, len(networks))
	for _, n := range networks {
		networkName[n.ID] = n.Name
		out.Networks = append(out.Networks, networkExport{Name: n.Name, ProxyURL: n.ProxyURL})
	}

	for _, p := range providers {
		out.Providers = append(out.Providers, providerExport{
			Slug: p.Slug, Name: p.Name, Protocol: p.Protocol, Endpoint: p.Endpoint,
			Enabled: p.Enabled, TimeoutMs: p.TimeoutMs, MaxRetries: p.MaxRetries,
			QuirksJSON: p.QuirksJSON,
			// p.NetworkID 为空 → 查表得空串 → omitempty 不写出，语义是直连。
			NetworkName: networkName[p.NetworkID],
		})
		for _, m := range byProvider[p.ID] {
			out.Models = append(out.Models, modelExport{
				ProviderSlug:     p.Slug,
				ModelID:          m.ModelID,
				DisplayName:      m.DisplayName,
				ContextWindow:    m.ContextWindow,
				MaxOutputTokens:  m.MaxOutputTokens,
				SupportsThinking: m.SupportsThinking,
				// 挡位也要导出：它是手工按模型文档配出来的运维知识，
				// 一次「换台机器重新部署」就配一遍是纯粹的重复劳动。
				// 形态与库里那列一致（CSV），未知档位附在后面不被丢。
				EffortLevels:       effort.FormatLevels(m.EffortLevels, m.UnknownLevels),
				PriceInput:         m.PriceInput,
				PriceCacheHit:      m.PriceCacheHit,
				PriceCacheCreation: m.PriceCacheCreation,
				PriceOutput:        m.PriceOutput,
				// 归组用显示名（跨库靠名字对齐，同 provider 靠 slug）。
				// m.GroupID 为空 → 查表得空串 → omitempty 不写出。
				GroupName: groupName[m.GroupID],
			})
		}
		creds, err := h.store.ListCredentials(ctx, p.ID)
		if err != nil {
			return out, err
		}
		for _, c := range creds {
			plain, err := decryptSecret(c.APIKeyEnc, h.masterKey)
			if err != nil {
				// 单条解不开不能中断整个导出：一份缺一条凭据的导出仍然有用，
				// 而「因为第 7 条解不开所以什么都不给」是更糟的结果。
				// 但必须留下痕迹 —— 静默跳过等于让人以为文件是完整的。
				slog.Warn("config export: credential undecryptable, skipped",
					"provider", p.Slug, "label", c.Label, "error", err)
				continue
			}
			out.Credentials = append(out.Credentials, credentialExport{
				ProviderSlug: p.Slug, Label: c.Label, APIKey: plain,
				Enabled: &c.Enabled, Weight: c.Weight,
			})
		}
	}
	return out, nil
}

// importRequest 是导入请求。
type importRequest struct {
	Passphrase string       `json:"passphrase"`
	Data       configExport `json:"data"`
	DryRun     bool         `json:"dry_run"`
}

// importResponse 逐项报告结果。
//
// **逐项**而不是只给总数：导入是「改别人的库」的动作，用户需要知道
// 哪几条真的进去了、哪几条被改名、哪几条被跳过。笼统的"成功导入 12 个"
// 无法回答「我那个 meme 怎么变成 meme-2 了」。
// @ts-type ConfigImportResult
type importResponse struct {
	DryRun             bool              `json:"dry_run"`
	ProvidersCreated   int               `json:"providers_created"`
	ProvidersRenamed   map[string]string `json:"providers_renamed,omitempty"`
	ModelsCreated      int               `json:"models_created"`
	ModelsSkipped      int               `json:"models_skipped"`
	CredentialsAdded   int               `json:"credentials_added"`
	CredentialsSkipped int               `json:"credentials_skipped"`
	Warnings           []string          `json:"warnings,omitempty"`
	Notes              []string          `json:"notes,omitempty"`
}

// Import 导入供应商与模型。
func (h *ConfigTransferHandler) Import(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireAdmin(w, r); !ok {
		return
	}
	var req importRequest
	if err := decodeJSON(w, r, &req); err != nil {
		if errors.Is(err, errUnsupportedMediaType) {
			return
		}
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}

	body, err := openExport(req.Data, strings.TrimSpace(req.Passphrase))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	resp, err := h.apply(r.Context(), body, req.DryRun)
	if err != nil {
		writeServerError(w, "apply import", err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (h *ConfigTransferHandler) apply(ctx context.Context, in *configExport, dryRun bool) (*importResponse, error) {
	resp := &importResponse{DryRun: dryRun, ProvidersRenamed: map[string]string{}}

	// 现有 slug → id，供模型与凭据按 slug 关联。
	existing, err := h.store.ListProviders(ctx)
	if err != nil {
		return nil, err
	}
	bySlug := make(map[string]*store.Provider, len(existing))
	for i := range existing {
		bySlug[existing[i].Slug] = &existing[i]
	}

	// slug 重命名时要占位，否则文件里两个同名 provider（导出后手工改过）
	// 会拿到同一个 -2 后缀。
	taken := make(map[string]bool, len(existing))
	for slug := range bySlug {
		taken[slug] = true
	}

	// 出站网络：按名字对齐（跨库 id 无意义，同 provider 靠 slug 对齐）。
	// 已存在的同名网络直接复用 —— 导出的地址多半与本库一致，而新建一条
	// 同名网络会被 UNIQUE(name) 拒掉，反而让整个导入失败。
	//
	// 干跑时**不写库**：这里只把「文件里引用了哪些名字」记下来，写库发生在
	// 下面真正 apply 的分支里。
	networkByName := make(map[string]string)
	existingNetworks, err := h.store.ListNetworks(ctx)
	if err != nil {
		return nil, err
	}
	for _, n := range existingNetworks {
		networkByName[n.Name] = n.ID
	}
	// pendingNetworks 暂存真实写入路径中要新建的网络：它们必须和 provider /
	// 模型 / 凭据一起进下面那笔事务，否则「导入回 500 但网络已落库」正是
	// 这次审计要消灭的半套配置。干跑只记录计划，不追加到这里。
	pendingNetworks := make([]store.Network, 0)
	for _, ne := range in.Networks {
		name := strings.TrimSpace(ne.Name)
		if name == "" {
			resp.Warnings = append(resp.Warnings, "跳过一个既无名称也无代理地址的网络")
			continue
		}
		if _, ok := networkByName[name]; ok {
			continue
		}
		if err := validateProxyURL(strings.TrimSpace(ne.ProxyURL)); err != nil {
			resp.Warnings = append(resp.Warnings,
				fmt.Sprintf("网络 %s 的 proxy_url 非法（%v），未导入", name, err))
			continue
		}
		id := store.NewNetworkID()
		networkByName[name] = id
		if dryRun {
			resp.Notes = append(resp.Notes, fmt.Sprintf("将新建网络 %s", name))
			continue
		}
		pendingNetworks = append(pendingNetworks,
			store.Network{ID: id, Name: name, ProxyURL: strings.TrimSpace(ne.ProxyURL)})
	}

	newProviders := make([]pendingProvider, 0, len(in.Providers))
	// 每个待建provider 连同它在**导出文件里的原 slug** 一起记下来。
	// 模型与凭据都按原 slug 引用，重命名后必须靠它找到归属。

	for _, pe := range in.Providers {
		slug := strings.TrimSpace(pe.Slug)
		if slug == "" {
			slug = slugify(pe.Name)
		}
		if slug == "" {
			resp.Warnings = append(resp.Warnings, "跳过一个既无 slug 也无名称的供应商")
			continue
		}

		target := slug
		if taken[target] {
			target = uniqueSlug(taken, slug)
			resp.ProvidersRenamed[slug] = target
			resp.Notes = append(resp.Notes,
				fmt.Sprintf("供应商 %s 已存在，导入的这条改名为 %s", slug, target))
		}
		taken[target] = true
		// network_name 指向的文件里没有那条网络时（手工编辑过、或那部分被
		// 删了）必须说出来：静默按直连导入会让人以为「代理配好了」，
		// 实际那条上游从此不走代理，且症状是 504 而非任何导入期错误。
		if name := strings.TrimSpace(pe.NetworkName); name != "" && networkByName[name] == "" {
			resp.Warnings = append(resp.Warnings, fmt.Sprintf(
				"供应商 %s 引用的网络 %q 不在文件里，该条按直连导入", target, name))
		}

		p := store.Provider{
			// ID 必须现生成：导出体里刻意不带 id（跨库无意义），而
			// createProvider 是**原样写入** p.ID 的 —— 不给就是空串，
			// 第二个 provider 立刻撞 UNIQUE(id)。
			ID:   generateID(),
			Slug: target, Name: strings.TrimSpace(pe.Name), Protocol: strings.TrimSpace(pe.Protocol),
			Endpoint: strings.TrimSpace(pe.Endpoint), Enabled: pe.Enabled,
			TimeoutMs: pe.TimeoutMs, MaxRetries: pe.MaxRetries, QuirksJSON: pe.QuirksJSON,
			// 出站网络按名字引用；空串 = 直连。
			//
			// 代理地址跟着一起搬。注意它指向的是**本机**的代理地址
			// （如 127.0.0.1:7897），换一台机器导入时该端口未必相同 ——
			// 但那是运维该判断的事，导入侧原样搬运、不做「猜一个新地址」
			// 这种自作聪明。
			NetworkID: networkByName[strings.TrimSpace(pe.NetworkName)],
			CreatedAt: time.Now().UnixMilli(), UpdatedAt: time.Now().UnixMilli(),
		}
		// 与手工创建保持同一套兜底：缺协议/端点的记录导进来就 unusable，
		// 不如当场报错让人知道文件有问题。
		if p.Protocol == "" {
			p.Protocol = wireproto.Chat
		}
		p.Protocol, err = normalizeProtocol(p.Protocol)
		if err != nil {
			return nil, fmt.Errorf("供应商 %s 的 protocol 非法：%w", slug, err)
		}
		if p.Endpoint == "" {
			resp.Warnings = append(resp.Warnings,
				fmt.Sprintf("供应商 %s 缺少 endpoint，已跳过", slug))
			continue
		}
		// 数值字段与 provider handler 同一套边界（CONFIG-01）：导入路径原先
		// 照单全收，绕过了 handler 的非负与 MaxDurationMillis 校验 —— 一份
		// timeout_ms 超大的文件会让 time.Duration 回绕成负数，负 Duration
		// 使 context.WithTimeout 立即过期，该 provider 的所有请求当场全挂。
		// 违例按导入的既有语义跳过并告警，不让单条脏数据中断整份导入。
		if pe.TimeoutMs < 0 || pe.TimeoutMs > config.MaxDurationMillis {
			resp.Warnings = append(resp.Warnings, fmt.Sprintf(
				"供应商 %s 的 timeout_ms 非法（%d，须在 0~%d 之间），已跳过",
				slug, pe.TimeoutMs, config.MaxDurationMillis))
			continue
		}
		if pe.MaxRetries < 0 {
			resp.Warnings = append(resp.Warnings,
				fmt.Sprintf("供应商 %s 的 max_retries 为负数（%d），已跳过", slug, pe.MaxRetries))
			continue
		}
		newProviders = append(newProviders, pendingProvider{origSlug: slug, provider: p})
	}

	// 干跑也要算清模型与凭据会怎样，但一个字节都不写。
	// 新建的 provider 还没有 id，所以用一个占位串占住「原slug → 有这个
	// provider」这个事实，让attach 能把它们算进来 —— 预览与实际不一致
	// 比预览不准更糟。
	if dryRun {
		resp.ProvidersCreated = len(newProviders)
		placeholder := make(map[string]string, len(newProviders))
		for _, n := range newProviders {
			placeholder[n.origSlug] = "dry-run-placeholder"
		}
		out := h.attach(ctx, in, bySlug, placeholder, true, resp, nil)
		resp.ModelsCreated, resp.CredentialsAdded = out.models, out.creds
		return resp, nil
	}

	// 真实写入：先落 provider，拿到 id 才能挂模型。
	//
	// idOf 按**文件里的原 slug** 建索引 —— 这一步是整个导入里最容易错的
	// 地方：文件里的模型与凭据引用的都是原 slug，而重命名之后库里的 slug
	// 已经变了（alpha → alpha-2）。按最终 slug 查会「找不到供应商」，模型
	// 与凭据被静默丢弃；按原 slug 查才对。
	idOf := make(map[string]string, len(bySlug))
	for slug, p := range bySlug {
		idOf[slug] = p.ID
	}
	// 真实写入整批放进一笔事务：provider / 模型组 / 模型 / 凭据任一条硬失败
	// 都让整批不生效。
	//
	// 旧实现逐条自动提交。中途失败时接口回 500，但先前那几条**已经落库**；
	// 运维重试就会因重名再追加 `-2`/`-3` 后缀 —— 「导入失败一次」在库里
	// 留下半套配置加一份重名副本，两者都不会自动消失。
	//
	// 校验所需的数据（现有 slug、模型清单）在事务外读齐（见上方
	// ListProviders / ListAllUpstreamModels），事务内只做写入。
	if err := h.store.RunTx(ctx, func(ctx context.Context, tx store.Tx) error {
		for i := range pendingNetworks {
			n := pendingNetworks[i]
			if err := h.store.CreateNetworkTx(ctx, tx, &n); err != nil {
				return fmt.Errorf("创建网络 %s：%w", n.Name, err)
			}
		}
		for _, n := range newProviders {
			p := n.provider
			if err := h.store.CreateProviderTx(ctx, tx, &p); err != nil {
				return fmt.Errorf("创建供应商 %s：%w", p.Slug, err)
			}
			idOf[n.origSlug] = p.ID
			resp.ProvidersCreated++
		}
		out := h.attach(ctx, in, bySlug, idOf, false, resp, tx)
		resp.ModelsCreated, resp.CredentialsAdded = out.models, out.creds
		return nil
	}); err != nil {
		return nil, err
	}
	return resp, nil
}

// pendingProvider 记住「文件里的原 slug → 实际要落库的 Provider」。
//
// 单独带 origSlug 而不是靠 finalSlug 反查，是因为反查在两个 provider 改名
// 后指向同一个目标时会挑错 —— 而这种文件（同一个源导出两次、改了名再导入）
// 并不罕见。
type pendingProvider struct {
	origSlug string
	provider store.Provider
}

// attachOutcome 汇总挂靠结果。
type attachOutcome struct {
	models int
	creds  int
}

// attach 处理模型与凭据的挂靠。
//
// idOf 为 nil 时是干跑：只数数，不写库；此时新建的 provider 还没有 id，
// 但仍要按原 slug 认领它，否则干跑会报「找不到供应商」而少算模型 ——
// 预览与实际不一致比不准更糟。
func (h *ConfigTransferHandler) attach(
	ctx context.Context,
	in *configExport,
	bySlug map[string]*store.Provider,
	idOf map[string]string,
	dry bool,
	resp *importResponse,
	// tx 非 nil 时所有写入走这笔事务（真实导入路径）；干跑时为 nil，只数不写。
	tx store.Tx,
) attachOutcome {
	var out attachOutcome

	// 归组导入必须排在模型循环**之前**：模型靠 group_name 关联组，
	// 组不存在就写不进 group_id。同名组复用现有组（导入是合并而不是
	// 重置配置）；当前格式没有这个切片时，所有模型未归组。
	groupByName := make(map[string]bool) // 本库已有（含本次新建）的组名
	for _, g := range in.ModelGroups {
		name := strings.TrimSpace(g.DisplayName)
		if name == "" || groupByName[name] {
			continue
		}
		groupByName[name] = true
		existing, err := h.store.GetModelGroupByName(ctx, name)
		if err != nil {
			resp.Warnings = append(resp.Warnings, fmt.Sprintf("查询模型组 %q 失败：%v", name, err))
			continue
		}
		if existing != nil {
			resp.Notes = append(resp.Notes, fmt.Sprintf("模型组 %q 已存在，复用", name))
			continue
		}
		if dry {
			continue
		}
		ng := &store.ModelGroup{
			ID: generateID(), DisplayName: name,
		}
		var cerr error
		if tx != nil {
			cerr = h.store.CreateModelGroupTx(ctx, tx, ng)
		} else {
			cerr = h.store.CreateModelGroup(ctx, ng)
		}
		if cerr != nil {
			resp.Warnings = append(resp.Warnings, fmt.Sprintf("模型组 %q 创建失败：%v", name, cerr))
		}
	}
	// 名字 → 库内组 id 的解析表。dry 路径不建组，这里按名字查一遍现状
	// （干跑报出的模型归组数与实际一致才有意义）。
	resolveGroup := make(map[string]string)
	lookupGroup := h.store.GetModelGroupByName
	if tx != nil {
		lookupGroup = func(ctx context.Context, name string) (*store.ModelGroup, error) {
			return h.store.GetModelGroupByNameTx(ctx, tx, name)
		}
	}
	for name := range groupByName {
		g, err := lookupGroup(ctx, name)
		if err == nil && g != nil {
			resolveGroup[name] = g.ID
		}
	}

	// 模型查重必须按 **provider_id**，不能按 slug。
	//
	// 反例很具体：目标库已有 alpha（含模型 alpha-1），导入文件里也有
	// alpha（含 alpha-1）。供应商改名成 alpha-2 后，这两个 alpha-1 分属
	// **不同的 provider** —— 是两条独立的记录。而按原 slug 查重会把它们
	// 认成同一个，于是导入的那个模型被跳过，模型挂到了错误的供应商上，
	// 而且界面上只显示一句"已存在"，看不出挂错了地方。
	existingModels := make(map[string]bool)
	if models, err := h.store.ListAllUpstreamModels(ctx); err == nil {
		for _, m := range models {
			existingModels[m.ProviderID+"\x00"+m.ModelID] = true
		}
	}

	for _, me := range in.Models {
		slug := strings.TrimSpace(me.ProviderSlug)
		pid, ok := idOf[slug]
		if !ok {
			resp.ModelsSkipped++
			resp.Warnings = append(resp.Warnings,
				fmt.Sprintf("模型 %s：找不到对应的供应商 %s，已跳过", me.ModelID, slug))
			continue
		}
		modelID := strings.TrimSpace(me.ModelID)
		if modelID == "" {
			resp.ModelsSkipped++
			resp.Warnings = append(resp.Warnings, "跳过一个没有 model_id 的模型")
			continue
		}
		if existingModels[pid+"\x00"+modelID] {
			// 已有同名模型：保留现有的。改它会影响在跑的流量，而导入方
			// 未必知道本库当前的定价与上下文设置。
			resp.ModelsSkipped++
			resp.Notes = append(resp.Notes,
				fmt.Sprintf("模型 %s / %s 已存在，保留库中原有配置", slug, modelID))
			continue
		}
		existingModels[pid+"\x00"+modelID] = true

		// 数值字段与 model handler 同一套边界（CONFIG-01）：负数原先原样落库
		// —— 负单价尤其危险，它让该模型的每次调用**倒贴钱**，账目静默失真。
		// 违例跳过并告警。
		if me.ContextWindow < 0 || me.MaxOutputTokens < 0 ||
			me.PriceInput < 0 || me.PriceCacheHit < 0 || me.PriceCacheCreation < 0 || me.PriceOutput < 0 {
			resp.ModelsSkipped++
			resp.Warnings = append(resp.Warnings,
				fmt.Sprintf("模型 %s / %s 含负数的窗口/输出上限/单价字段，已跳过", slug, modelID))
			continue
		}

		m := store.UpstreamModel{
			// 同 provider：ID 必须现生成，DAO 是原样写入的。
			ID:         generateID(),
			ProviderID: pid, ModelID: modelID, DisplayName: me.DisplayName,
			ContextWindow:   me.ContextWindow,
			MaxOutputTokens: me.MaxOutputTokens, SupportsThinking: me.SupportsThinking,
			PriceInput: me.PriceInput, PriceCacheHit: me.PriceCacheHit,
			PriceCacheCreation: me.PriceCacheCreation, PriceOutput: me.PriceOutput,
		}
		// 归组：文件里的组名解析成库内组 id（组在循环前已确保存在）。
		// 解析不到（如 dry 路径库里还没有这个组）→ 未归组。
		if gname := strings.TrimSpace(me.GroupName); gname != "" {
			if gid, ok := resolveGroup[gname]; ok {
				m.GroupID = gid
			}
		}
		// 挡位：一个都识别不出来但原文非空 = 文件里写的是无效值。
		// 照实跳过并告警，而不是静默导成「未配置」—— 后者会让运维以为
		// 挡位已经配好，而实际数据面上完全没有生效。
		if me.EffortLevels != "" {
			known, unknown := effort.ParseLevels(me.EffortLevels)
			if len(known) == 0 {
				resp.ModelsSkipped++
				resp.Warnings = append(resp.Warnings,
					fmt.Sprintf("模型 %s / %s 的 effort_levels 无任何可识别档位（%q），已跳过",
						slug, modelID, me.EffortLevels))
				continue
			}
			m.EffortLevels, m.UnknownLevels = known, unknown
		}
		if dry {
			out.models++
			continue
		}
		merr := error(nil)
		if tx != nil {
			merr = h.store.CreateUpstreamModelTx(ctx, tx, &m)
		} else {
			merr = h.store.CreateUpstreamModel(ctx, &m)
		}
		if merr != nil {
			resp.ModelsSkipped++
			resp.Warnings = append(resp.Warnings,
				fmt.Sprintf("模型 %s / %s 写入失败：%v", slug, modelID, merr))
			continue
		}
		out.models++
	}

	for _, ce := range in.Credentials {
		slug := strings.TrimSpace(ce.ProviderSlug)
		pid, ok := idOf[slug]
		if !ok {
			resp.CredentialsSkipped++
			continue
		}
		if ce.Enabled == nil {
			resp.CredentialsSkipped++
			resp.Warnings = append(resp.Warnings,
				fmt.Sprintf("凭据 %s / %s 缺少 enabled 字段，已跳过", slug, ce.Label))
			continue
		}
		apiKey := strings.TrimSpace(ce.APIKey)
		if apiKey == "" {
			// 导出时没带凭据是正常路径，不该报成错。
			continue
		}
		enc, err := encryptSecret(apiKey, h.masterKey)
		if err != nil {
			resp.CredentialsSkipped++
			resp.Warnings = append(resp.Warnings,
				fmt.Sprintf("凭据 %s / %s 加密失败：%v", slug, ce.Label, err))
			continue
		}
		weight := ce.Weight
		if weight <= 0 {
			weight = 1
		}
		c := store.Credential{
			ID:         generateID(),
			ProviderID: pid, Label: strings.TrimSpace(ce.Label), APIKeyEnc: enc,
			Enabled: *ce.Enabled, Weight: weight, Status: "healthy",
			CreatedAt: time.Now().UnixMilli(),
		}
		if dry {
			out.creds++
			continue
		}
		cerr := error(nil)
		if tx != nil {
			cerr = h.store.CreateCredentialTx(ctx, tx, &c)
		} else {
			cerr = h.store.CreateCredential(ctx, &c)
		}
		if cerr != nil {
			resp.CredentialsSkipped++
			resp.Warnings = append(resp.Warnings,
				fmt.Sprintf("凭据 %s / %s 写入失败：%v", slug, ce.Label, cerr))
			continue
		}
		out.creds++
	}

	return out
}

// uniqueSlug 在 taken 里给 base 找一个没被占用的名字：base、base-2、base-3…
//
// 追加后缀而不是覆盖，因为 slug 是路由、日志与管理界面的对外引用；
// 覆盖会让既有引用指向另一个供应商，且无声无息。
func uniqueSlug(taken map[string]bool, base string) string {
	if !taken[base] {
		return base
	}
	for i := 2; ; i++ {
		cand := fmt.Sprintf("%s-%d", base, i)
		if !taken[cand] {
			return cand
		}
	}
}
