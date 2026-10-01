package store

import (
	"context"
	"database/sql"
	"time"
)

type Route struct {
	ID              string
	PublicName      string
	ProviderID      string
	UpstreamModelID string
	Enabled         bool

	// 是否启用自动故障转移。链成员与顺序见 route_targets 表。
	//
	// 故障转移的**策略参数**（最多尝试几个目标、熔断阈值、各类超时）不按 route 存：
	// 它们是全局的，由「设置」页写入 app_settings，运行时经快照读取（见 DESIGN §10）。
	// 这样避免了「界面上看不到、却仍在生效」的隐形按路由覆盖。
	FailoverEnabled bool

	CreatedAt int64
}

// nullIfEmpty 把空串转成 NULL。
// 供 display_name / label / last_error 等可空列复用（写入空串会与「未设置」混淆）。
func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// routeColumns 是 routes 三处读路径共用的列清单。
const routeColumns = `id, public_name, provider_id, upstream_model_id, enabled, failover_enabled, created_at`

// scanRoute 承接一行 route。
func scanRoute(sc scanner) (Route, error) {
	var r Route
	var enabled, failover int
	err := sc.Scan(&r.ID, &r.PublicName, &r.ProviderID, &r.UpstreamModelID, &enabled,
		&failover, &r.CreatedAt)
	if err != nil {
		return r, err
	}
	r.Enabled = enabled == 1
	r.FailoverEnabled = failover == 1
	return r, nil
}

func (s *Store) ListRoutes(ctx context.Context) ([]Route, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+routeColumns+` FROM routes ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make([]Route, 0)
	for rows.Next() {
		r, err := scanRoute(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, r)
	}
	return result, rows.Err()
}

func (s *Store) GetRoute(ctx context.Context, id string) (*Route, error) {
	r, err := scanRoute(s.db.QueryRowContext(ctx,
		`SELECT `+routeColumns+` FROM routes WHERE id = ?`, id))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

func (s *Store) CreateRoute(ctx context.Context, r *Route) error {
	return createRoute(ctx, s.db, r)
}

// createRoute 接受 execer，以便在事务里与「种链首目标」原子地一起完成。
func createRoute(ctx context.Context, ex execer, r *Route) error {
	now := time.Now().UnixMilli()
	enabled := 0
	if r.Enabled {
		enabled = 1
	}
	failover := 0
	if r.FailoverEnabled {
		failover = 1
	}
	_, err := ex.ExecContext(ctx,
		`INSERT INTO routes (id, public_name, provider_id, upstream_model_id, enabled, failover_enabled, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		r.ID, r.PublicName, r.ProviderID, r.UpstreamModelID, enabled, failover, now)
	// 回写时间戳，否则创建响应里的 created_at 是 0，与库里不一致。
	if err == nil {
		r.CreatedAt = now
	}
	return err
}

func (s *Store) UpdateRoute(ctx context.Context, id string, r *Route) error {
	return updateRoute(ctx, s.db, id, r)
}

// updateRoute 接受 execer，以便在事务里与「对齐链首」原子地一起完成。
func updateRoute(ctx context.Context, ex execer, id string, r *Route) error {
	enabled := 0
	if r.Enabled {
		enabled = 1
	}
	failover := 0
	if r.FailoverEnabled {
		failover = 1
	}
	res, err := ex.ExecContext(ctx,
		`UPDATE routes SET public_name = ?, provider_id = ?, upstream_model_id = ?, enabled = ?, failover_enabled = ? WHERE id = ?`,
		r.PublicName, r.ProviderID, r.UpstreamModelID, enabled, failover, id)
	return checkAffected(res, err)
}

// DeleteRoute 删除一条 route；id 不存在时返回 ErrNotFound。
func (s *Store) DeleteRoute(ctx context.Context, id string) error {
	return deleteByID(ctx, s.db, "routes", id)
}
