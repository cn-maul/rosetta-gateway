package store

import (
	"context"
	"database/sql"
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
