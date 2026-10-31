package admin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"strings"
	"time"

	"github.com/cn-maul/rosetta-gateway/internal/store"
)

// 导入导出的「人」与「路由」两段。
//
// 与供应商那段分开成文件，是因为这两段的**冲突语义不同**：
//
//   - 供应商撞名 → 改名加后缀（slug 是本库内部标识，改了不影响调用方）。
//   - 路由撞名 → **跳过不改名**：公开名是调用方写在代码里的地址，
//     悄悄换个名字等于把那条客户端指向一个不存在的模型；而覆盖会
//     直接改掉线上行为。跳过并告警是唯一安全的处理。
//   - 用户撞名 → 跳过，不动既有账号（覆盖会改掉别人的密码与余额）。
//   - 密钥撞哈希 → 跳过（同一把 key 导两次不该建两条记录）。

// applyIdentities 导入用户分组、用户、访问密钥。
//
// 顺序是硬的：分组 → 用户 → 密钥。用户与密钥都靠**组名**关联分组，
// 而外键要求分组先存在；密钥靠**用户名**关联归属用户，用户的 key 一律
// 401，所以必须用户先建。
//
// 全部走调用方那笔事务：任何一条硬失败都让整批不生效（与供应商段同理）。
func (h *ConfigTransferHandler) applyIdentities(
	ctx context.Context,
	in *configExport,
	dry bool,
	resp *importResponse,
	tx store.Tx,
) error {
	// 先把现状读齐（事务外读，WAL 下事务内的读池看不到未提交行）。
	existingGroups, err := h.store.ListGroups(ctx)
	if err != nil {
		return err
	}
	groupID := make(map[string]string, len(existingGroups))
	for _, g := range existingGroups {
		groupID[g.Name] = g.ID
	}
	existingUsers, err := h.store.ListUsers(ctx)
	if err != nil {
		return err
	}
	userID := make(map[string]string, len(existingUsers))
	for _, u := range existingUsers {
		userID[store.CanonicalUsername(u.Username)] = u.ID
	}
	existingKeys, err := h.store.ListAccessKeys(ctx)
	if err != nil {
		return err
	}
	keyHashes := make(map[string]bool, len(existingKeys))
	for _, k := range existingKeys {
		keyHashes[k.KeyHash] = true
	}

	// ---- 1) 用户分组 ----
	for _, ge := range in.UserGroups {
		name := strings.TrimSpace(ge.Name)
		if name == "" {
			resp.Warnings = append(resp.Warnings, "跳过一个没有名称的用户分组")
			continue
		}
		if _, ok := groupID[name]; ok {
			// 只增不改：既有分组的白名单原样保留。覆盖它等于替别人改权限，
			// 而且是那种「导入一次、一群人突然看见更多模型」的静默变化。
			resp.Notes = append(resp.Notes,
				fmt.Sprintf("用户分组 %q 已存在，保留库中原有白名单", name))
			continue
		}
		gid := generateID()
		groupID[name] = gid
		if dry {
			resp.UserGroupsCreated++
			continue
		}
		g := store.Group{ID: gid, Name: name, Description: strings.TrimSpace(ge.Description)}
		if cerr := h.createGroup(ctx, tx, &g); cerr != nil {
			return fmt.Errorf("创建用户分组 %s：%w", name, cerr)
		}
		// 白名单紧跟建组写：分组存在而白名单没写会留下「空 = 不限制」的组，
		// 那是把权限放大，不是缩小。空名单本身是合法语义（不限制），
		// 所以只在文件里确实带了成员时才写。
		if models := store.NormalizeModelNames(ge.Models); len(models) > 0 {
			if cerr := h.replaceGroupModels(ctx, tx, gid, models); cerr != nil {
				return fmt.Errorf("写用户分组 %s 的白名单：%w", name, cerr)
			}
		}
		resp.UserGroupsCreated++
	}

	// ---- 2) 用户 ----
	for _, ue := range in.Users {
		username := store.CanonicalUsername(ue.Username)
		if username == "" {
			resp.UsersSkipped++
			resp.Warnings = append(resp.Warnings, "跳过一个没有用户名的用户")
			continue
		}
		if _, ok := userID[username]; ok {
			// 覆盖既有账号会改掉别人的密码、角色与余额 —— 那是「迁移」变成
			// 「入侵」的分界线。跳过并说出来。
			resp.UsersSkipped++
			resp.Notes = append(resp.Notes,
				fmt.Sprintf("用户 %q 已存在，保留库中原有账号（密码/余额/角色未改）", username))
			continue
		}
		role := strings.TrimSpace(ue.Role)
		if role != store.RoleAdmin && role != store.RoleUser {
			if role != "" {
				resp.Warnings = append(resp.Warnings,
					fmt.Sprintf("用户 %q 的角色 %q 不认识，按普通用户导入", username, role))
			}
			role = store.RoleUser
		}
		status := strings.TrimSpace(ue.Status)
		if status != store.UserStatusActive && status != store.UserStatusDisabled {
			if status != "" {
				resp.Warnings = append(resp.Warnings,
					fmt.Sprintf("用户 %q 的状态 %q 不认识，按 active 导入", username, status))
			}
			status = store.UserStatusActive
		}
		if ue.QuotaTokens < 0 {
			resp.UsersSkipped++
			resp.Warnings = append(resp.Warnings,
				fmt.Sprintf("用户 %q 的 quota_tokens 为负数（%d），已跳过", username, ue.QuotaTokens))
			continue
		}
		uid := generateID()
		userID[username] = uid
		su := store.User{
			ID: uid, Username: username,
			DisplayName:  strings.TrimSpace(ue.DisplayName),
			PasswordHash: ue.PasswordHash,
			Role:         role, Status: status,
			QuotaTokens: ue.QuotaTokens,
			UsedTokens:  ue.UsedTokens,
			// nil = 不限额（库里落 NULL），数字 = 余额。这个区分是余额语义的
			// 全部要害：把「不限额」导成 0 会让那个账号立刻 402。
			Unlimited: ue.BalanceCents == nil,
			Remark:    strings.TrimSpace(ue.Remark),
			GroupID:   groupID[strings.TrimSpace(ue.GroupName)],
		}
		if ue.BalanceCents != nil {
			su.BalanceCents = *ue.BalanceCents
		}
		if gname := strings.TrimSpace(ue.GroupName); gname != "" && su.GroupID == "" {
			resp.Warnings = append(resp.Warnings,
				fmt.Sprintf("用户 %q 引用的分组 %q 不在文件里，该账号未归组", username, gname))
		}
		if dry {
			resp.UsersCreated++
			continue
		}
		if cerr := h.createUser(ctx, tx, &su); cerr != nil {
			return fmt.Errorf("创建用户 %s：%w", username, cerr)
		}
		resp.UsersCreated++
	}

	// ---- 3) 访问密钥 ----
	for _, ke := range in.AccessKeys {
		plain := strings.TrimSpace(ke.Key)
		if plain == "" {
			resp.AccessKeysSkipped++
			resp.Warnings = append(resp.Warnings, "跳过一个没有明文密钥的访问密钥")
			continue
		}
		if ke.Enabled == nil {
			resp.AccessKeysSkipped++
			resp.Warnings = append(resp.Warnings,
				fmt.Sprintf("访问密钥 %q 缺少 enabled 字段，已跳过", ke.Name))
			continue
		}
		sum := sha256.Sum256([]byte(plain))
		keyHash := hex.EncodeToString(sum[:])
		if keyHashes[keyHash] {
			// 同一把 key 已在本库：跳过而不是报错。导入是幂等的合并不是重置。
			resp.AccessKeysSkipped++
			resp.Notes = append(resp.Notes,
				fmt.Sprintf("访问密钥 %q 与本库已有密钥相同，已跳过", ke.Name))
			continue
		}
		owner := store.CanonicalUsername(ke.User)
		ownerID, ok := userID[owner]
		if owner == "" || !ok {
			// 无归属 key 在鉴权时一律 401（见 store.AccessKey.UserID 的注释）。
			// 建一把永远用不了的 key 只会让人以为导入成功了。
			resp.AccessKeysSkipped++
			resp.Warnings = append(resp.Warnings, fmt.Sprintf(
				"访问密钥 %q 的归属用户 %q 不在本库也不在文件里，已跳过", ke.Name, ke.User))
			continue
		}
		if ke.QuotaTokens < 0 || ke.RPMLimit < 0 || ke.TPMLimit < 0 || ke.ExpiresAt < 0 {
			resp.AccessKeysSkipped++
			resp.Warnings = append(resp.Warnings,
				fmt.Sprintf("访问密钥 %q 含负数的限额/有效期字段，已跳过", ke.Name))
			continue
		}
		// 白名单与归属分组：组名解析不到就退回「沿用归属用户的分组」，
		// 而不是按空组导入（那会把 key 的可见范围放大到用户全组）。
		gid := groupID[strings.TrimSpace(ke.GroupName)]
		if gname := strings.TrimSpace(ke.GroupName); gname != "" && gid == "" {
			resp.Warnings = append(resp.Warnings, fmt.Sprintf(
				"访问密钥 %q 引用的分组 %q 不在文件里，该 key 沿用归属用户的分组", ke.Name, gname))
		}
		var keyEnc []byte
		if len(h.masterKey) > 0 {
			enc, cerr := encryptSecret(plain, h.masterKey)
			if cerr != nil {
				return fmt.Errorf("加密访问密钥 %s：%w", ke.Name, cerr)
			}
			keyEnc = enc
		}
		prefix := plain
		if len(prefix) > 11 {
			prefix = prefix[:11]
		}
		sk := store.AccessKey{
			ID: generateID(), KeyHash: keyHash, KeyPrefix: prefix + "...",
			KeyEnc: keyEnc, Name: strings.TrimSpace(ke.Name),
			Enabled: *ke.Enabled, QuotaTokens: ke.QuotaTokens,
			UserID: ownerID,
			// AllowedModels：nil 与空数组在库里语义不同（不限制 / 拒绝全部）。
			// 导入侧把「文件里没写」收敛成不限制 —— 带着空数组导进来会让
			// 一把本来能用的 key 全线 403，而「拒绝全部」这个态本来就
			// 只是解析失败的兜底，不是给人配的。
			AllowedModels: store.NormalizeModelNames(ke.AllowedModels),
			ExpiresAt:     ke.ExpiresAt,
			AllowedIPs:    strings.TrimSpace(ke.AllowedIPs),
			GroupID:       gid,
			RPMLimit:      ke.RPMLimit,
			TPMLimit:      ke.TPMLimit,
		}
		keyHashes[keyHash] = true
		if dry {
			resp.AccessKeysCreated++
			continue
		}
		if cerr := h.createAccessKey(ctx, tx, &sk); cerr != nil {
			return fmt.Errorf("创建访问密钥 %s：%w", sk.Name, cerr)
		}
		resp.AccessKeysCreated++
	}
	return nil
}

// applyRoutes 导入路由与故障转移链。
//
// 必须在模型之后跑：链目标要解析成 (provider_id, upstream_model_id)，
// 而模型可能是这次导入新建的、id 刚刚生成。
//
// modelIDs 的键是 `providerID + "\x00" + modelID`（与 attach 的查重键同形）。
func (h *ConfigTransferHandler) applyRoutes(
	ctx context.Context,
	in *configExport,
	bySlug map[string]*store.Provider,
	idOf map[string]string,
	modelIDs map[string]string,
	dry bool,
	resp *importResponse,
	tx store.Tx,
) error {
	if len(in.Routes) == 0 {
		return nil
	}
	existing, err := h.store.ListRoutes(ctx)
	if err != nil {
		return err
	}
	takenNames := make(map[string]bool, len(existing))
	for _, r := range existing {
		takenNames[r.PublicName] = true
	}

	// resolve 把「文件里的 provider slug + model_id」解析成本库 id。
	// idOf 优先：文件里的 provider 可能已被改名（alpha → alpha-2），
	// 而路由引用的是**文件里的** slug。
	resolve := func(slug, modelID string) (pid, mid string, ok bool) {
		pid = idOf[slug]
		if pid == "" {
			if p := bySlug[slug]; p != nil {
				pid = p.ID
			}
		}
		if pid == "" {
			return "", "", false
		}
		mid = modelIDs[pid+"\x00"+modelID]
		if mid == "" {
			return "", "", false
		}
		return pid, mid, true
	}

	for _, re := range in.Routes {
		name := strings.TrimSpace(re.PublicName)
		if name == "" {
			resp.RoutesSkipped++
			resp.Warnings = append(resp.Warnings, "跳过一个没有公开名的路由")
			continue
		}
		if takenNames[name] {
			// **不改名**：公开名是调用方写在代码里的模型名，加 -2 后缀
			// 等于悄悄让那条客户端指向不存在的模型；覆盖则直接改掉线上行为。
			resp.RoutesSkipped++
			resp.Notes = append(resp.Notes,
				fmt.Sprintf("路由 %q 已存在，保留库中原有配置", name))
			continue
		}
		if len(re.Targets) == 0 {
			// 没有链的路由在运行时拿不到任何候选，等于一条永远 404 的
			// 模型名。宁可跳过并告警。
			resp.RoutesSkipped++
			resp.Warnings = append(resp.Warnings,
				fmt.Sprintf("路由 %q 没有任何上游目标，已跳过", name))
			continue
		}
		type resolvedTarget struct {
			pid, mid string
			enabled  bool
		}
		targets := make([]resolvedTarget, 0, len(re.Targets))
		broken := ""
		for _, te := range re.Targets {
			slug := strings.TrimSpace(te.ProviderSlug)
			mid := strings.TrimSpace(te.ModelID)
			pid, modelID, ok := resolve(slug, mid)
			if !ok {
				broken = fmt.Sprintf("%s/%s", slug, mid)
				break
			}
			targets = append(targets, resolvedTarget{pid: pid, mid: modelID, enabled: te.Enabled})
		}
		if broken != "" {
			// 半条链比没有链更难排查：请求失败时说不清是目标挂了还是
			// 链本来就不全。
			resp.RoutesSkipped++
			resp.Warnings = append(resp.Warnings,
				fmt.Sprintf("路由 %q 的目标 %s 解析不到，整条已跳过", name, broken))
			continue
		}
		takenNames[name] = true
		if dry {
			resp.RoutesCreated++
			continue
		}
		rid := generateID()
		head := targets[0]
		r := store.Route{
			ID: rid, PublicName: name, Enabled: re.Enabled,
			FailoverEnabled: re.FailoverEnabled,
			ProviderID:      head.pid, UpstreamModelID: head.mid,
		}
		if cerr := h.createRoute(ctx, tx, &r); cerr != nil {
			return fmt.Errorf("创建路由 %s：%w", name, cerr)
		}
		for i, t := range targets {
			rt := store.RouteTarget{
				ID: generateID(), RouteID: rid,
				ProviderID: t.pid, UpstreamModelID: t.mid,
				Position: i, Enabled: t.enabled,
				CreatedAt: time.Now().UnixMilli(),
			}
			if cerr := h.createRouteTarget(ctx, tx, &rt); cerr != nil {
				return fmt.Errorf("创建路由 %s 的第 %d 个目标：%w", name, i+1, cerr)
			}
		}
		resp.RoutesCreated++
	}
	return nil
}

// 下面四个小包装只在事务内被调用（tx 恒非 nil）：这里是导入路径，
// 逐条自动提交会让「中途失败」留下半套数据 —— 那正是 2026-10-31 的
// 审计要求消灭的形态。保留 tx 形参是为了让调用点显式声明这一点。
func (h *ConfigTransferHandler) createGroup(ctx context.Context, tx store.Tx, g *store.Group) error {
	return h.store.CreateGroupTx(ctx, tx, g)
}

func (h *ConfigTransferHandler) replaceGroupModels(ctx context.Context, tx store.Tx, groupID string, models []string) error {
	return h.store.ReplaceGroupModelsTx(ctx, tx, groupID, models)
}

func (h *ConfigTransferHandler) createUser(ctx context.Context, tx store.Tx, u *store.User) error {
	return h.store.CreateUserTx(ctx, tx, u)
}

func (h *ConfigTransferHandler) createAccessKey(ctx context.Context, tx store.Tx, k *store.AccessKey) error {
	return h.store.CreateAccessKeyTx(ctx, tx, k)
}

func (h *ConfigTransferHandler) createRoute(ctx context.Context, tx store.Tx, r *store.Route) error {
	return h.store.CreateRouteTx(ctx, tx, r)
}

func (h *ConfigTransferHandler) createRouteTarget(ctx context.Context, tx store.Tx, t *store.RouteTarget) error {
	return h.store.CreateRouteTargetTx(ctx, tx, t)
}
