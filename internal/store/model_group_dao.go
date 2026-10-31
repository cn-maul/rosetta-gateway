package store

import (
	"context"
	"database/sql"
	"errors"
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

// DeleteModelGroup 删除归组。组内模型的 group_id 由 FK 的
// ON DELETE SET NULL 自动清掉 —— 删组不删模型。
func (s *Store) DeleteModelGroup(ctx context.Context, id string) error {
	return deleteByID(ctx, s.db, "model_groups", id)
}

// SetModelGroup 给单个上游模型指派归组。groupID 传空串 = 移出组（落 NULL）。
//
// 校验组存在：把模型挂到一个不存在的组会在 FK 约束下失败（报错是 SQLite
// 的英文原文），与其让约束报错不如在入口给出人话。
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
	nilGroup := any(nil)
	if groupID != "" {
		nilGroup = groupID
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE upstream_models SET group_id = ? WHERE id = ?`, nilGroup, modelID)
	return checkAffected(res, err)
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
	nilGroup := any(nil)
	if groupID != "" {
		nilGroup = groupID
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	assigned := 0
	for _, mid := range modelIDs {
		// 行数为 0 说明「校验通过之后模型被删了」——整笔回滚，不留半套。
		if err := checkAffected(tx.ExecContext(ctx,
			`UPDATE upstream_models SET group_id = ? WHERE id = ?`, nilGroup, mid)); err != nil {
			return 0, err
		}
		assigned++
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return assigned, nil
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

	unassigned := 0
	for _, mid := range modelIDs {
		err := checkAffected(tx.ExecContext(ctx,
			`UPDATE upstream_models SET group_id = NULL WHERE id = ?`, mid))
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return 0, err
		}
		unassigned++
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return unassigned, nil
}

// ListModelsInGroup 列出组内全部模型（管理页展示组成员用）。
func (s *Store) ListModelsInGroup(ctx context.Context, groupID string) ([]UpstreamModel, error) {
	rows, err := s.read.QueryContext(ctx,
		`SELECT `+modelColumns+` FROM upstream_models WHERE group_id = ? ORDER BY provider_id, model_id`, groupID)
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

// GroupNamesByModel 返回 model_id → 组名 的映射（统计与列表的批量富化用）。
//
// 一次查询而不是逐模型 JOIN：/usage/by-model 之类接口每页可能有几百行，
// 逐行查组的 N+1 在 SQLite 上完全没必要 —— 组本来就是小表（几十个），
// 一次拉全 + 本地映射比什么都快，还不需要碰 usage 查询的 SQL。
func (s *Store) GroupNamesByModel(ctx context.Context) (map[string]string, error) {
	rows, err := s.read.QueryContext(ctx,
		`SELECT um.id, g.display_name
		   FROM upstream_models um JOIN model_groups g ON g.id = um.group_id`)
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
