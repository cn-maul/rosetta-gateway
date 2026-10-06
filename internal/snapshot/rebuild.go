package snapshot

import (
	"context"

	"github.com/cn-maul/rosetta-gateway/internal/routing"
	"github.com/cn-maul/rosetta-gateway/internal/store"
)

// ProviderFailure 是「一个 provider 为什么没能在本次重建里就绪」的最小视图。
//
// 刻意定义成接口要满足的最小字段集，而不是直接引 upstream.ProviderFailure：
// snapshot 是热路径的读视图层，不该反向依赖池的实现包（那会让两者的构建
// 顺序与测试替身都变复杂）。调用方（cmd/gateway 的 runtimeReloader）
// 负责把 upstream 的结构投影到这里。
type ProviderFailure struct {
	ID     string
	Slug   string
	Reason string
}

// RebuildFromDB 只搬运「路由/上游/密钥」这三类解析元数据。
// 凭据解密不在这里发生——上游池在 BuildFromStore 里自行解密建客户端，
// 所以本函数不需要主密钥，也不需要 logger，同样不需要池
// （池的重建由 ReloadHandler 显式先做，分工是「先池后快照」）。
//
// failures 是池重建时报告的「未就绪 provider」清单（P4 / 设计 §4.7）。
// 它随快照下发，让管理面能标出「这个 provider 配了但不可用」——
// 此前这些信息只躺在日志里，界面上 provider 看起来一切正常。
// 传 nil 表示「本次没有失败信息」（例如启动时的首建，或调用方不掌握池状态）。
//
// 注意：这里的失败判定以 **slug** 匹配，与池侧 PrepareFromStore 的
// newProviders 键同域（两者都用 slug 作键）。
func RebuildFromDB(ctx context.Context, st *store.Store, failures []ProviderFailure) (*Snapshot, error) {
	notReady := make(map[string]string, len(failures))
	for _, f := range failures {
		notReady[f.Slug] = f.Reason
	}

	snap := &Snapshot{
		Routes:     routing.NewRouteIndex(),
		Providers:  make(map[string]*ProviderSnapshot),
		KeysByHash: make(map[string]*KeySnapshot),
		UsersByID:  make(map[string]*UserSnapshot),
	}

	providers, err := st.ListProviders(ctx)
	if err != nil {
		return nil, err
	}
	for _, p := range providers {
		reason, bad := notReady[p.Slug]
		snap.Providers[p.Slug] = &ProviderSnapshot{
			ID:       p.ID,
			Slug:     p.Slug,
			Name:     p.Name,
			Endpoint: p.Endpoint,
			Enabled:  p.Enabled,
			Ready:    !bad,
			Reason:   reason,
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
			ID:              m.ID,
			ProviderID:      m.ProviderID,
			ModelID:         m.ModelID,
			Enabled:         m.Enabled,
			ContextWindow:   m.ContextWindow,
			MaxOutputTokens: m.MaxOutputTokens,
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

	// 组 → 模型白名单（P1 / P2）。一次全量读，避免按用户或按 key 的 N+1 查。
	//
	// 读失败**致命**（与 users 同理）：拿不到白名单就等于拿不到权限边界，
	// 若降级为空表，所有组内用户会静默变成「不限制」。宁可让 reload 失败、
	// 保留上一份正确快照。
	groupModels, gerr := st.ListGroupModels(ctx)
	if gerr != nil {
		return nil, gerr
	}

	// 用户列表读失败**一律致命**（返回 error → 本次 Swap 不执行，旧快照继续生效）。
	//
	// 为什么不给「读不到就降级为空用户表」留后门：那会让所有用户悄悄失去
	// 禁用状态与用户级配额约束 —— 一个被管理员禁用的账号会重新可用，
	// 而网关看起来一切正常。与其静默失效，不如让 reload 整体失败、
	// 保留上一份正确快照（与 MULTIUSER.md §4.7「池失败则拒绝 Swap」同源思路）。
	// users 表体量极小，正常路径下这里几乎不会失败。
	users, uerr := st.ListUsers(ctx)
	if uerr != nil {
		return nil, uerr
	}
	usersByID := make(map[string]*UserSnapshot, len(users))
	for _, u := range users {
		usersByID[u.ID] = &UserSnapshot{
			ID:          u.ID,
			Name:        u.Username,
			Role:        u.Role,
			Status:      u.Status,
			QuotaTokens: u.QuotaTokens,
			AuthVersion: u.AuthVersion,
			GroupID:     u.GroupID,
			// 组的白名单：查得到组条目才算「配了白名单」，否则不限制。
			// 注意「组存在但一条模型都没配」在 ListGroupModels 里没有条目，
			// 因此也落入 AllowAll —— 与 §3.3「空白名单 = 不限制」一致。
			AllowedModels: groupModelAllow(groupModels, u.GroupID),
		}
	}
	snap.UsersByID = usersByID

	// key 放在 users 之后：P2 的 key 级分组覆盖需要「用户属于哪个组」才能
	// 算出这把 key 实际生效的组白名单。放在前面就得多存一个 GroupID 字段
	// 让热路径自己二选一 —— 那是把决策推给每个调用方，早点定下来更稳。
	keys, err := st.ListAccessKeys(ctx)
	if err != nil {
		return nil, err
	}
	keysByHash := make(map[string]*KeySnapshot, len(keys))
	for _, k := range keys {
		// 组白名单：key 自己指定了组就以它为准，否则用归属用户的组。
		//
		// 「归属用户查不到」时这里回落成不限制，是**不可达**的 ——
		// 鉴权阶段会先返回 ErrKeyUnowned（401），根本走不到热路径。
		// 写成不限制只是为了让重建不因一行脏数据整体失败。
		groupAllow := AllowAll()
		if u := usersByID[k.UserID]; u != nil {
			groupAllow = u.AllowedModels
		}
		if k.GroupID != "" {
			groupAllow = groupModelAllow(groupModels, k.GroupID)
		}
		keysByHash[k.KeyHash] = &KeySnapshot{
			ID:       k.ID,
			KeyHash:  k.KeyHash,
			Name:     k.Name,
			Enabled:  k.Enabled,
			UserID:   k.UserID,
			RPMLimit: k.RPMLimit,
			TPMLimit: k.TPMLimit,
			// nil 切片 = 未配置限制 → AllowAll；非 nil（哪怕为空，
			// 那是 JSON 损坏的兜底）= AllowOnly。两种取值的约定见
			// store.AccessKey.AllowedModels。
			AllowedModels:   keyModelAllow(k.AllowedModels),
			GroupModelAllow: groupAllow,
			// P2：有效期与来源 IP 白名单。
			ExpiresAt:   k.ExpiresAt,
			AllowedNets: k.AllowedNets,
		}
	}
	snap.KeysByHash = keysByHash

	// 运行时全局默认（超时与故障转移策略 + 模型容量默认）。读失败不致命：留 0 即全部回落 config。
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
	if md, err := st.GetModelDefaults(ctx); err == nil {
		snap.Runtime.DefaultContextWindow = md.ContextWindow
		snap.Runtime.DefaultMaxOutputTokens = md.MaxOutputTokens
	}

	return snap, nil
}

// keyModelAllow 把 key 的白名单列转成热路径判定器。
//
// nil（列 NULL / 合法空数组）= 未配置限制 → AllowAll；
// 非 nil（含空切片，那是 JSON 损坏时的兜底）= AllowOnly。
// 两种取值的约定见 store.AccessKey.AllowedModels 的注释。
func keyModelAllow(models []string) ModelAllow {
	if models == nil {
		return AllowAll()
	}
	return AllowOnly(models)
}

// groupModelAllow 取某用户所属组的白名单。
//
// 三种情况都返回「不限制」，它们在产品语义上等价：
//   - 用户不属于任何组（groupID 为空）；
//   - 组存在但一条模型都没配（ListGroupModels 里没有该 key）；
//   - 传入的 groupID 指向一个已被删除的组（外键 SET NULL 本应避免，
//     但这里是防御性的：宁可「不限制」也不要因为悬空 id 把所有人锁死）。
//
// 注意「组配了白名单」与「组没配」必须区分：前者走 AllowOnly（可能拒绝
// 全部模型），后者走 AllowAll。这正是需要一个 `ok` 判断而不是
// `len(models)>0` 的原因 —— 后者无法区分「没这个 key」与「空白名单」。
func groupModelAllow(byGroup map[string][]string, groupID string) ModelAllow {
	if groupID == "" {
		return AllowAll()
	}
	models, ok := byGroup[groupID]
	if !ok {
		return AllowAll()
	}
	return AllowOnly(models)
}
