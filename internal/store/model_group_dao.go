package store

import (
	"context"
	"database/sql"
	"log/slog"
	"time"
)

// ModelGroup 是「跨上游的同一个模型」的一个归组。
//
// 语义：不同上游的同一个模型（叫法往往不同）可以标成一组，
// 用量统计按组聚合、显示组名。一个模型最多属于一组（group_id 单列），
// NULL = 未归组。
type ModelGroup struct {
	ID          string
	DisplayName string
	CreatedAt   int64
}

const modelGroupColumns = `id, display_name, created_at`

func (s *Store) ListModelGroups(ctx context.Context) ([]ModelGroup, error) {
	rows, err := s.read.QueryContext(ctx,
		`SELECT `+modelGroupColumns+` FROM model_groups ORDER BY display_name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make([]ModelGroup, 0)
	for rows.Next() {
		var g ModelGroup
		if err := rows.Scan(&g.ID, &g.DisplayName, &g.CreatedAt); err != nil {
			return nil, err
		}
		result = append(result, g)
	}
	return result, rows.Err()
}

func (s *Store) GetModelGroup(ctx context.Context, id string) (*ModelGroup, error) {
	var g ModelGroup
	err := s.read.QueryRowContext(ctx,
		`SELECT `+modelGroupColumns+` FROM model_groups WHERE id = ?`, id).
		Scan(&g.ID, &g.DisplayName, &g.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &g, nil
}

// GetModelGroupByName 按显示名查组（导入侧按名字匹配用）。
func (s *Store) GetModelGroupByName(ctx context.Context, name string) (*ModelGroup, error) {
	return getModelGroupByName(ctx, s.read, name)
}

// GetModelGroupByNameTx 是事务内版本。
//
// 导入在事务里新建的组必须能被**同一笔事务**读到：WAL 下读池看不到未提交行，
// 走读池会拿到 nil，于是模型拿不到 group_id —— 表现为「归组导入成功但模型
// 全都没归组」，且界面不会报错。
func (s *Store) GetModelGroupByNameTx(ctx context.Context, tx Tx, name string) (*ModelGroup, error) {
	return getModelGroupByName(ctx, tx, name)
}

func getModelGroupByName(ctx context.Context, q execer, name string) (*ModelGroup, error) {
	var g ModelGroup
	err := q.QueryRowContext(ctx,
		`SELECT `+modelGroupColumns+` FROM model_groups WHERE display_name = ?`, name).
		Scan(&g.ID, &g.DisplayName, &g.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &g, nil
}

func (s *Store) CreateModelGroup(ctx context.Context, g *ModelGroup) error {
	g.CreatedAt = time.Now().UnixMilli()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO model_groups (id, display_name, created_at) VALUES (?, ?, ?)`,
		g.ID, g.DisplayName, g.CreatedAt)
	return err
}

// CreateModelGroupTx 在一笔事务里创建归组，供批量导入整体回滚使用。
func (s *Store) CreateModelGroupTx(ctx context.Context, tx Tx, g *ModelGroup) error {
	g.CreatedAt = time.Now().UnixMilli()
	_, err := tx.ExecContext(ctx,
		`INSERT INTO model_groups (id, display_name, created_at) VALUES (?, ?, ?)`,
		g.ID, g.DisplayName, g.CreatedAt)
	return err
}

func (s *Store) UpdateModelGroup(ctx context.Context, id, displayName string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE model_groups SET display_name = ? WHERE id = ?`, displayName, id)
	return checkAffected(res, err)
}

// DeleteModelGroup 删除归组。归属由 model_group_members 的
// ON DELETE CASCADE 清掉 —— 删组不删模型，也不删任何用量。
func (s *Store) DeleteModelGroup(ctx context.Context, id string) error {
	return deleteByID(ctx, s.db, "model_groups", id)
}

// SetModelGroup 给单个上游模型指派归组。groupID 传空串 = 移出组。
//
// 校验组存在：把模型挂到一个不存在的组会在 FK 约束下失败（报错是 SQLite
// 的英文原文），与其让约束报错不如在入口给出人话。
//
// 归属写在独立的 model_group_members（按 provider_id + model_id），
// 而不是 upstream_models.group_id：后者随 provider 级联删除，删一个上游
// 就会让该上游的历史用量失去组名、在「按模型」统计里重新裂成单独一行。
func (s *Store) SetModelGroup(ctx context.Context, modelID, groupID string) error {
	if groupID != "" {
		g, err := s.GetModelGroup(ctx, groupID)
		if err != nil {
			return err
		}
		if g == nil {
			return ErrNotFound
		}
	}
	m, err := s.GetUpstreamModel(ctx, modelID)
	if err != nil {
		return err
	}
	if m == nil {
		return ErrNotFound
	}
	return s.setGroupMemberships(ctx, s.db, []GroupMemberRef{
		{ProviderID: m.ProviderID, ModelID: m.ModelID},
	}, groupID)
}

// GroupMemberRef 是归属的**不可变定位符**：provider_id + model_id。
//
// 不用 upstream_models.id：那一行被删后 id 就没有意义了，而历史用量
// 一直按 (provider_id, upstream_model) 记着。两者是同一个坐标系。
//
// 导出是因为管理面要用它按 (provider_id, model_id) 重新挂回**已删除上游**
// 的历史成员 —— 那条历史行没有 upstream_models.id 可传。
type GroupMemberRef struct {
	ProviderID string
	ModelID    string
}

// ModelGroupHistoryMember 是一个**只有历史用量、没有当前模型行**的组成员。
//
// 删掉上游 / 上游模型后，upstream_models 里的那一行会被级联删除，但
// usage_records 仍按 (provider_id, upstream_model) 记着历史事实。要让这些
// 历史行继续按组名合并，归属必须留在 model_group_members 里；本类型就是
// 管理页展示那部分「还活着但已没有配置行」的成员所需的投影。
type ModelGroupHistoryMember struct {
	ProviderID string
	ModelID    string
	// ProviderName 优先取当前上游名，取不到再回退到用量里固化的旧名字
	// （上游已删时只剩这个来源）。
	ProviderName string
	Count        int64
	Tokens       int64
}

// setGroupMemberships 在同一笔写入里更新归属表与 upstream_models.group_id
// 兼容镜像。groupID 为空 = 移出组。
//
// 镜像列只为「回填新表」保留：所有读路径都读归属表，写路径两处同写，
// 这样旧版本程序或手工 SQL 留下的 group_id 仍能在下次启动时被搬进来。
func (s *Store) setGroupMemberships(ctx context.Context, ex execer, members []GroupMemberRef, groupID string) error {
	for _, m := range members {
		if groupID == "" {
			if _, err := ex.ExecContext(ctx,
				`DELETE FROM model_group_members WHERE provider_id = ? AND model_id = ?`,
				m.ProviderID, m.ModelID); err != nil {
				return err
			}
		} else {
			if _, err := ex.ExecContext(ctx,
				`INSERT INTO model_group_members (group_id, provider_id, model_id) VALUES (?, ?, ?)
				 ON CONFLICT(provider_id, model_id) DO UPDATE SET group_id = excluded.group_id`,
				groupID, m.ProviderID, m.ModelID); err != nil {
				return err
			}
		}
		if _, err := ex.ExecContext(ctx,
			`UPDATE upstream_models SET group_id = ? WHERE provider_id = ? AND model_id = ?`,
			nullIfEmpty(groupID), m.ProviderID, m.ModelID); err != nil {
			return err
		}
	}
	return nil
}

// deleteGroupMembershipsWithoutUsage 清掉「模型行已不存在、且从来没有用量」
// 的归属行。
//
// 保留有历史用量的归属是为了让统计继续按组合并（这是这张表存在的理由）；
// 但一个从未被调用过、配置也删掉的模型挂在这里没有任何意义，只会让组列表
// 的成员数比「当前成员 + 历史成员」多出一个不可见的空壳。
//
// modelID 为空 = 清该 provider 下的全部此类归属（删除整个上游时用）。
func deleteGroupMembershipsWithoutUsage(ctx context.Context, ex execer, providerID, modelID string) error {
	query := `DELETE FROM model_group_members
	 WHERE provider_id = ?
	   AND NOT EXISTS (
	     SELECT 1 FROM usage_records u
	      WHERE u.provider_id = model_group_members.provider_id
	        AND u.upstream_model = model_group_members.model_id
	   )
	   AND NOT EXISTS (
	     SELECT 1 FROM usage_daily_rollups r
	      WHERE r.provider_id = model_group_members.provider_id
	        AND r.upstream_model = model_group_members.model_id
	   )`
	args := []any{providerID}
	if modelID != "" {
		query += ` AND model_id = ?`
		args = append(args, modelID)
	}
	_, err := ex.ExecContext(ctx, query, args...)
	return err
}

// settingModelGroupOrphanRepairKey 记录「删上游导致归属丢失」的一次性修补
// 已经跑过。
//
// 必须是持久标记：修补依据是历史用量的 public_model 与现存组成员的别名
// 重合，属于推断。用户如果事后手动把某个历史成员移出组，下次启动不能
// 又把它悄悄加回去 —— 那会让「移出」这个动作看起来没生效。
const settingModelGroupOrphanRepairKey = "model_group_orphan_repair_v1"

// repairOrphanGroupMembershipsOnce 一次性修补 2026-11-03 之前因删上游而
// 丢失的组归属。
//
// # 为什么需要推断
//
// 旧实现把归属存在 upstream_models.group_id 上，删 provider 时模型行级联
// 删除，组员关系随之消失。历史用量只留下 (provider_id, upstream_model)，
// 没有任何字段直接记录「它当时属于哪个组」。可用的线索只有 public_model：
// 同一组的不同上游通常服务同一个公开模型名。
//
// # 保守判据
//
// 只凭 public_model 重合是不够的：同一个公开模型名后面可能挂着完全不同的
// 上游模型（真实数据里 DeepSeek-V4.1-Flash 的用量里有 `cn:space-bunny`）。
// 那样修补会把另一个模型并进来，比不合并更难发现。
//
// 因此要求两条同时成立：
//   - public_model 与该组的既有历史重合（同一业务出口）；
//   - 去掉厂商前缀（`cn:` 等最后一段冒号之前的部分）后，model_id 与组内
//     某个成员的 model_id 逐字相同（同一个真模型的不同叫法）。
//
// 仍不唯一就跳过 —— 归属是推断，宁可让个别历史成员暂时独立。
func (s *Store) repairOrphanGroupMembershipsOnce(ctx context.Context) error {
	v, ok, err := s.getSetting(ctx, settingModelGroupOrphanRepairKey)
	if err != nil || (ok && v == "1") {
		return err
	}

	type pair struct{ providerID, modelID string }
	orphans := make(map[pair]bool)
	rows, err := s.db.QueryContext(ctx, `
		SELECT provider_id, upstream_model FROM usage_records
		 WHERE NOT EXISTS (
		   SELECT 1 FROM upstream_models um
		    WHERE um.provider_id = usage_records.provider_id
		      AND um.model_id = usage_records.upstream_model)
		   AND NOT EXISTS (
		   SELECT 1 FROM model_group_members mgm
		    WHERE mgm.provider_id = usage_records.provider_id
		      AND mgm.model_id = usage_records.upstream_model)
		UNION
		SELECT provider_id, upstream_model FROM usage_daily_rollups
		 WHERE NOT EXISTS (
		   SELECT 1 FROM upstream_models um
		    WHERE um.provider_id = usage_daily_rollups.provider_id
		      AND um.model_id = usage_daily_rollups.upstream_model)
		   AND NOT EXISTS (
		   SELECT 1 FROM model_group_members mgm
		    WHERE mgm.provider_id = usage_daily_rollups.provider_id
		      AND mgm.model_id = usage_daily_rollups.upstream_model)`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var p pair
		if err := rows.Scan(&p.providerID, &p.modelID); err != nil {
			rows.Close()
			return err
		}
		orphans[p] = true
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if len(orphans) == 0 {
		// 空库不能记「已修补」：首次启动时还没有任何历史数据，若此刻落标记，
		// 之后导入/升级进来的孤儿永远不会被检查。
		var hasHistory int
		if err := s.db.QueryRowContext(ctx,
			`SELECT EXISTS (SELECT 1 FROM usage_records
			                UNION ALL SELECT 1 FROM usage_daily_rollups)`).Scan(&hasHistory); err != nil {
			return err
		}
		if hasHistory == 0 {
			return nil
		}
		return s.setSetting(ctx, settingModelGroupOrphanRepairKey, "1")
	}

	// 每个组的历史成员：public_model 别名集合 + model_id 叫法集合。
	type groupHistory struct {
		aliases map[string]bool
		names   map[string]bool
	}
	groups := make(map[string]*groupHistory)
	aliasRows, err := s.db.QueryContext(ctx, `
		SELECT u.public_model, mgm.group_id, mgm.model_id
		  FROM usage_records u
		  JOIN model_group_members mgm
		    ON mgm.provider_id = u.provider_id AND mgm.model_id = u.upstream_model
		 GROUP BY u.public_model, mgm.group_id, mgm.model_id
		UNION
		SELECT r.public_model, mgm.group_id, mgm.model_id
		  FROM usage_daily_rollups r
		  JOIN model_group_members mgm
		    ON mgm.provider_id = r.provider_id AND mgm.model_id = r.upstream_model
		 GROUP BY r.public_model, mgm.group_id, mgm.model_id
		UNION
		-- 组里刚加入、还没产生用量的成员：用路由的公开名补上别名，
		-- 否则「先归组、后调用」的历史成员会因为没有别名而永远修不回来。
		SELECT rt.public_name, mgm.group_id, mgm.model_id
		  FROM routes rt
		  JOIN upstream_models um ON um.id = rt.upstream_model_id
		  JOIN model_group_members mgm
		    ON mgm.provider_id = um.provider_id AND mgm.model_id = um.model_id
		 GROUP BY rt.public_name, mgm.group_id, mgm.model_id`)
	if err != nil {
		return err
	}
	for aliasRows.Next() {
		var alias, groupID, modelID string
		if err := aliasRows.Scan(&alias, &groupID, &modelID); err != nil {
			aliasRows.Close()
			return err
		}
		g := groups[groupID]
		if g == nil {
			g = &groupHistory{aliases: map[string]bool{}, names: map[string]bool{}}
			groups[groupID] = g
		}
		g.aliases[alias] = true
		g.names[modelID] = true
	}
	if err := aliasRows.Close(); err != nil {
		return err
	}

	repaired := 0
	for p := range orphans {
		aliases, err := s.usagePublicModels(ctx, p.providerID, p.modelID)
		if err != nil {
			return err
		}
		modelAliases := modelIDVariants(p.modelID)
		candidates := make(map[string]bool)
		for groupID, g := range groups {
			matchedAlias := false
			for alias := range aliases {
				if g.aliases[alias] {
					matchedAlias = true
					break
				}
			}
			if !matchedAlias {
				continue
			}
			for name := range modelAliases {
				if g.names[name] {
					candidates[groupID] = true
					break
				}
			}
		}
		if len(candidates) != 1 {
			continue
		}
		var groupID string
		for id := range candidates {
			groupID = id
		}
		if _, err := s.db.ExecContext(ctx,
			`INSERT INTO model_group_members (group_id, provider_id, model_id) VALUES (?, ?, ?)
			 ON CONFLICT(provider_id, model_id) DO NOTHING`,
			groupID, p.providerID, p.modelID); err != nil {
			return err
		}
		repaired++
	}
	if repaired > 0 {
		slog.Info("repaired orphan model group memberships", "rows", repaired)
	}
	return s.setSetting(ctx, settingModelGroupOrphanRepairKey, "1")
}

// modelIDVariants 返回一个模型 id 的等价叫法：原文，以及去掉厂商前缀后的
// 最后一段（`cn:deepseek-v4.1-flash` → `deepseek-v4.1-flash`）。
//
// 只剥一段而不是递归：本仓库见过的差异就是「中转加前缀」。剥多了会把
// `vendor/foo:bar` 这种带路径的名字压成 `bar`，反而容易撞到别的模型。
func modelIDVariants(modelID string) map[string]bool {
	out := map[string]bool{modelID: true}
	for i := len(modelID) - 1; i >= 0; i-- {
		if modelID[i] == ':' {
			out[modelID[i+1:]] = true
			break
		}
	}
	return out
}

// usagePublicModels 返回某个历史模型用过的全部 public_model。
func (s *Store) usagePublicModels(ctx context.Context, providerID, modelID string) (map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT DISTINCT public_model FROM usage_records
		 WHERE provider_id = ? AND upstream_model = ?
		UNION
		SELECT DISTINCT public_model FROM usage_daily_rollups
		 WHERE provider_id = ? AND upstream_model = ?`,
		providerID, modelID, providerID, modelID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]bool)
	for rows.Next() {
		var alias string
		if err := rows.Scan(&alias); err != nil {
			return nil, err
		}
		out[alias] = true
	}
	return out, rows.Err()
}

// AssignModelsToGroup 在**单个事务**里把一批模型指派到同一个组（空 groupID =
// 移出组），返回实际更新的行数。
//
// # 为什么必须是事务
//
// 旧实现是 handler 里逐条 SetModelGroup，每条 UPDATE 独立自动提交。而
// handler 的文档与注释都宣称「全部校验后才逐条写，任一 id 不存在则整笔拒绝」
// —— 那只是**校验**阶段成立，写入阶段一旦中途失败（SQLITE_BUSY、另一个
// 请求刚好删掉了某个模型、组被删掉触发 FK），前 N-1 条已经永久落库，接口
// 却回 500。运维看到报错重试，此时模型已被指派了一半，重试会「成功」并把
// 剩下的一半补上 —— 表面上一切正常，实际上是两次不同的写入拼出来的结果。
//
// 上限 500 个模型，单事务完全撑得住；同仓 ImportUpstreamModels 与配置导入
// 都是这个形态。
func (s *Store) AssignModelsToGroup(ctx context.Context, modelIDs []string, groupID string) (int, error) {
	if len(modelIDs) == 0 {
		return 0, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	members := make([]GroupMemberRef, 0, len(modelIDs))
	for _, mid := range modelIDs {
		m, err := getUpstreamModel(ctx, tx, mid)
		if err != nil {
			return 0, err
		}
		// 模型不存在说明「校验通过之后模型被删了」——整笔回滚，不留半套。
		if m == nil {
			return 0, ErrNotFound
		}
		members = append(members, GroupMemberRef{ProviderID: m.ProviderID, ModelID: m.ModelID})
	}
	if err := s.setGroupMemberships(ctx, tx, members, groupID); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return len(members), nil
}

// UnassignModelsFromGroup 在单个事务里把一批模型移出归组，返回实际更新的
// 行数。**跳过**不存在的模型（移出组对「模型已被删」无害，不该整笔拒绝）。
//
// 仍然用事务：虽然移出是可重放的单调操作，但半途失败会留下「一部分移出了、
// 一部分还在组里」的中间态，而接口回 500 —— 调用方要么看到错误、要么看到
// 一个数字，两种都不该对应半个结果。
func (s *Store) UnassignModelsFromGroup(ctx context.Context, modelIDs []string) (int, error) {
	if len(modelIDs) == 0 {
		return 0, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	members := make([]GroupMemberRef, 0, len(modelIDs))
	for _, mid := range modelIDs {
		m, err := getUpstreamModel(ctx, tx, mid)
		if err != nil {
			return 0, err
		}
		if m == nil {
			continue
		}
		members = append(members, GroupMemberRef{ProviderID: m.ProviderID, ModelID: m.ModelID})
	}
	if err := s.setGroupMemberships(ctx, tx, members, ""); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return len(members), nil
}

// ListModelsInGroup 列出组内**当前仍存在**的上游模型（管理页展示组成员用）。
func (s *Store) ListModelsInGroup(ctx context.Context, groupID string) ([]UpstreamModel, error) {
	rows, err := s.read.QueryContext(ctx,
		`SELECT `+modelColumns+` FROM upstream_models um`+modelGroupJoin+
			` WHERE mgm.group_id = ? ORDER BY um.provider_id, um.model_id`, groupID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make([]UpstreamModel, 0)
	for rows.Next() {
		m, err := scanModel(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, m)
	}
	return result, rows.Err()
}

// ListGroupHistoryMembers 列出组内**已经没有当前模型行**的成员。
//
// 与 ListModelsInGroup 是互补的两半：前者展示「现在能配的」，后者展示
// 「历史统计里还在合并的」。删除上游后归组不该失效，但管理页也必须能看见
// 它 —— 否则运维只会看到一个成员数对不上、又找不到多出来的是谁的组。
func (s *Store) ListGroupHistoryMembers(ctx context.Context, groupID string) ([]ModelGroupHistoryMember, error) {
	src, args := UsageSource(UsageFilter{})
	query := `SELECT mgm.provider_id, mgm.model_id,
		COALESCE(MAX(NULLIF(p.name, '')), COALESCE((
		  SELECT ur.provider_name FROM usage_records ur
		   WHERE ur.provider_id = mgm.provider_id
		     AND ur.upstream_model = mgm.model_id
		     AND COALESCE(ur.provider_name, '') <> ''
		   ORDER BY ur.ts DESC LIMIT 1
		), ''), ''),
		COALESCE(SUM(u.n), 0), COALESCE(SUM(u.total_tokens), 0)
	  FROM model_group_members mgm
	  LEFT JOIN providers p ON p.id = mgm.provider_id
	  LEFT JOIN ` + src + `
	    ON u.provider_id = mgm.provider_id AND u.upstream_model = mgm.model_id
	 WHERE mgm.group_id = ?
	   AND NOT EXISTS (
	     SELECT 1 FROM upstream_models um
	      WHERE um.provider_id = mgm.provider_id AND um.model_id = mgm.model_id
	   )
	   AND EXISTS (
	     SELECT 1 FROM usage_records ur
	      WHERE ur.provider_id = mgm.provider_id AND ur.upstream_model = mgm.model_id
	     UNION ALL
	     SELECT 1 FROM usage_daily_rollups r
	      WHERE r.provider_id = mgm.provider_id AND r.upstream_model = mgm.model_id
	   )
	 GROUP BY mgm.provider_id, mgm.model_id
	 ORDER BY 5 DESC, mgm.provider_id, mgm.model_id`
	args = append(args, groupID)

	rows, err := s.read.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make([]ModelGroupHistoryMember, 0)
	for rows.Next() {
		var m ModelGroupHistoryMember
		if err := rows.Scan(&m.ProviderID, &m.ModelID, &m.ProviderName, &m.Count, &m.Tokens); err != nil {
			return nil, err
		}
		result = append(result, m)
	}
	return result, rows.Err()
}

// ListUngroupedHistoryMembers 列出「有历史用量、但当前没有模型行、也没归组」
// 的 (provider_id, model_id) 对，供管理页把它们补进组。
//
// 这些对是删上游后留下的孤儿：旧实现把 group_id 存在 upstream_models 上，
// 行一删归属就没了，而历史用量不会跟着消失。没有这个入口，用户看到的就是
// 「统计里多一行、组却加不进去」—— 只能删库重建，那是不可接受的处置。
func (s *Store) ListUngroupedHistoryMembers(ctx context.Context) ([]ModelGroupHistoryMember, error) {
	src, args := UsageSource(UsageFilter{})
	query := `SELECT u.provider_id, u.upstream_model,
		COALESCE(MAX(NULLIF(p.name, '')), COALESCE((
		  SELECT ur.provider_name FROM usage_records ur
		   WHERE ur.provider_id = u.provider_id
		     AND ur.upstream_model = u.upstream_model
		     AND COALESCE(ur.provider_name, '') <> ''
		   ORDER BY ur.ts DESC LIMIT 1
		), ''), ''),
		COALESCE(SUM(u.n), 0), COALESCE(SUM(u.total_tokens), 0)
	  FROM ` + src + `
	  LEFT JOIN providers p ON p.id = u.provider_id
	 WHERE NOT EXISTS (
	   SELECT 1 FROM upstream_models um
	    WHERE um.provider_id = u.provider_id AND um.model_id = u.upstream_model
	 )
	   AND NOT EXISTS (
	   SELECT 1 FROM model_group_members mgm
	    WHERE mgm.provider_id = u.provider_id AND mgm.model_id = u.upstream_model
	 )
	 GROUP BY u.provider_id, u.upstream_model
	 ORDER BY 5 DESC, u.provider_id, u.upstream_model`

	rows, err := s.read.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make([]ModelGroupHistoryMember, 0)
	for rows.Next() {
		var m ModelGroupHistoryMember
		if err := rows.Scan(&m.ProviderID, &m.ModelID, &m.ProviderName, &m.Count, &m.Tokens); err != nil {
			return nil, err
		}
		result = append(result, m)
	}
	return result, rows.Err()
}

// AssignHistoryMembersToGroup 把历史定位符直接写进归属表（不需要当前模型行）。
//
// 与 AssignModelsToGroup 的差别只在入参：那个接 upstream_models.id、供
// 普通模型选择用；这个接 (provider_id, model_id)，正是删掉模型行之后仍然
// 可定位历史用量的唯一坐标系。
func (s *Store) AssignHistoryMembersToGroup(ctx context.Context, groupID string, members []GroupMemberRef) (int, error) {
	if len(members) == 0 {
		return 0, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if err := s.setGroupMemberships(ctx, tx, members, groupID); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return len(members), nil
}

// UnassignHistoryMembersFromGroup 按历史定位符移出组，不要求模型行存在。
func (s *Store) UnassignHistoryMembersFromGroup(ctx context.Context, members []GroupMemberRef) (int, error) {
	if len(members) == 0 {
		return 0, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if err := s.setGroupMemberships(ctx, tx, members, ""); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return len(members), nil
}

// GroupNamesByModel 返回上游模型行 id → 组名 的映射（列表批量富化用）。
//
// 一次查询而不是逐模型 JOIN：/usage/by-model 之类接口每页可能有几百行，
// 逐行查组的 N+1 在 SQLite 上完全没必要 —— 组本来就是小表（几十个），
// 一次拉全 + 本地映射比什么都快，还不需要碰 usage 查询的 SQL。
func (s *Store) GroupNamesByModel(ctx context.Context) (map[string]string, error) {
	rows, err := s.read.QueryContext(ctx,
		`SELECT um.id, g.display_name
		   FROM upstream_models um
		   JOIN model_group_members mgm
		     ON mgm.provider_id = um.provider_id AND mgm.model_id = um.model_id
		   JOIN model_groups g ON g.id = mgm.group_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[string]string)
	for rows.Next() {
		var id, name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, err
		}
		out[id] = name
	}
	return out, rows.Err()
}
