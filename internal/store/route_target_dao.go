package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"time"
)

// RouteTarget 是故障转移链上的一个成员：一条 route 下的某个 (provider, model) 目标。
// Position 升序即尝试顺序，0 通常就是 routes 表里那份「主目标」（迁移时自动回填）。
type RouteTarget struct {
	ID              string
	RouteID         string
	ProviderID      string
	UpstreamModelID string
	Position        int
	Enabled         bool
	CreatedAt       int64
}

const routeTargetColumns = `id, route_id, provider_id, upstream_model_id, position, enabled, created_at`

func scanRouteTarget(sc scanner) (RouteTarget, error) {
	var t RouteTarget
	var enabled int
	err := sc.Scan(&t.ID, &t.RouteID, &t.ProviderID, &t.UpstreamModelID, &t.Position, &enabled, &t.CreatedAt)
	t.Enabled = enabled == 1
	return t, err
}

// ListAllRouteTargets 一次性拉出全部目标，供快照重建按 route_id 分组建链，
// 避免对每条 route 各发一次查询。
func (s *Store) ListAllRouteTargets(ctx context.Context) ([]RouteTarget, error) {
	rows, err := s.read.QueryContext(ctx,
		`SELECT `+routeTargetColumns+` FROM route_targets ORDER BY route_id, position`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make([]RouteTarget, 0)
	for rows.Next() {
		t, err := scanRouteTarget(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, t)
	}
	return result, rows.Err()
}

func (s *Store) ListRouteTargets(ctx context.Context, routeID string) ([]RouteTarget, error) {
	rows, err := s.read.QueryContext(ctx,
		`SELECT `+routeTargetColumns+` FROM route_targets WHERE route_id = ? ORDER BY position`, routeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make([]RouteTarget, 0)
	for rows.Next() {
		t, err := scanRouteTarget(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, t)
	}
	return result, rows.Err()
}

func (s *Store) GetRouteTarget(ctx context.Context, id string) (*RouteTarget, error) {
	t, err := scanRouteTarget(s.read.QueryRowContext(ctx,
		`SELECT `+routeTargetColumns+` FROM route_targets WHERE id = ?`, id))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}

func (s *Store) CreateRouteTarget(ctx context.Context, t *RouteTarget) error {
	return createRouteTarget(ctx, s.db, t)
}

// createRouteTarget 接受 execer，供事务内复用（见 CreateRouteWithHeadTarget）。
func createRouteTarget(ctx context.Context, ex execer, t *RouteTarget) error {
	if t.CreatedAt == 0 {
		t.CreatedAt = time.Now().UnixMilli()
	}
	if t.ID == "" {
		t.ID = generateTargetID()
	}
	enabled := 0
	if t.Enabled {
		enabled = 1
	}
	_, err := ex.ExecContext(ctx,
		`INSERT INTO route_targets (id, route_id, provider_id, upstream_model_id, position, enabled, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		t.ID, t.RouteID, t.ProviderID, t.UpstreamModelID, t.Position, enabled, t.CreatedAt)
	return err
}

func (s *Store) UpdateRouteTarget(ctx context.Context, id string, t *RouteTarget) error {
	enabled := 0
	if t.Enabled {
		enabled = 1
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE route_targets SET route_id = ?, provider_id = ?, upstream_model_id = ?, position = ?, enabled = ? WHERE id = ?`,
		t.RouteID, t.ProviderID, t.UpstreamModelID, t.Position, enabled, id)
	return err
}

func (s *Store) DeleteRouteTarget(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM route_targets WHERE id = ?`, id)
	return err
}

// generateTargetID 生成 32 位十六进制主键，与网关各处 ID 同构。
func generateTargetID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// SyncHeadTarget 把链首（position 0 目标）对齐到 route 的 provider/model 列。
//
// 运行时以链为准（buildCandidates 一旦见到目标行就不再看 routes 主目标列），
// 所以当有人绕过目标链接口、直接 PATCH route.provider_id/upstream_model_id 时，
// 必须把这次改动落到链首 —— 否则 DB 列变了、响应回显新值，实际流量仍打旧目标，
// 静默失效。链为空（从未配过链的老 route）则补一条 position 0。幂等：已一致直接返回。
func (s *Store) SyncHeadTarget(ctx context.Context, routeID, providerID, modelID string) error {
	return syncHeadTarget(ctx, s.db, routeID, providerID, modelID)
}

// syncHeadTarget 接受 execer：这里的「读链首 → 改链首」是两条语句，
// 必须与调用方处在同一事务里。否则两个并发 PATCH 各自读到同一个链首、
// 后提交的覆盖先提交的，丢一次更新 —— SetMaxOpenConns(1) 只保证单条语句串行，
// 覆盖不了「两条语句为一组」的组合。
func syncHeadTarget(ctx context.Context, ex execer, routeID, providerID, modelID string) error {
	var headID, headProvider, headModel string
	err := ex.QueryRowContext(ctx,
		`SELECT id, provider_id, upstream_model_id FROM route_targets WHERE route_id = ? ORDER BY position LIMIT 1`,
		routeID).Scan(&headID, &headProvider, &headModel)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return createRouteTarget(ctx, ex, &RouteTarget{
			RouteID:         routeID,
			ProviderID:      providerID,
			UpstreamModelID: modelID,
			Position:        0,
			Enabled:         true,
		})
	case err != nil:
		return err
	}
	if headProvider == providerID && headModel == modelID {
		return nil
	}
	_, err = ex.ExecContext(ctx,
		`UPDATE route_targets SET provider_id = ?, upstream_model_id = ? WHERE id = ?`,
		providerID, modelID, headID)
	return err
}

// ReplaceRouteTargets 用给定顺序整体重建某条 route 的目标链（position 按切片下标重排）。
// 供后台「保存有序目标列表」使用：删旧插新，一次事务内完成，避免中途状态让快照读到断链。
// in[0] 会同时写回 routes.provider_id / upstream_model_id 作为主目标（向后兼容列）。
func (s *Store) ReplaceRouteTargets(ctx context.Context, routeID string, targets []RouteTarget) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `DELETE FROM route_targets WHERE route_id = ?`, routeID); err != nil {
		return err
	}

	now := time.Now().UnixMilli()
	for i := range targets {
		t := targets[i]
		t.RouteID = routeID
		t.Position = i
		if t.ID == "" {
			t.ID = generateTargetID()
		}
		enabled := 0
		if t.Enabled {
			enabled = 1
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO route_targets (id, route_id, provider_id, upstream_model_id, position, enabled, created_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?)`,
			t.ID, t.RouteID, t.ProviderID, t.UpstreamModelID, t.Position, enabled, now); err != nil {
			return err
		}
	}

	// 主目标列同步：链首即 routes 的 (provider, model)，保证旧的按名解析与非故障转移路径一致。
	if len(targets) > 0 {
		head := targets[0]
		if _, err := tx.ExecContext(ctx,
			`UPDATE routes SET provider_id = ?, upstream_model_id = ? WHERE id = ?`,
			head.ProviderID, head.UpstreamModelID, routeID); err != nil {
			return err
		}
	}

	return tx.Commit()
}
