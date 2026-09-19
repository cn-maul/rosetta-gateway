package store

import (
	"context"
	"database/sql"
	"time"
)

type Route struct {
	ID               string
	PublicName       string
	ProviderID       string
	UpstreamModelID  string
	Enabled          bool
	Priority         int
	FallbackRouteID  string
	ExtraJSON        string
	CreatedAt        int64
}

// nullIfEmpty 把空串转成 NULL。
// fallback_route_id 与 extra_json 都是可空列，其中 fallback_route_id 还带
// REFERENCES routes(id) 外键：写入空串会被判定为"引用 id 为空串的行"，
// 在启用外键的连接上直接报 FOREIGN KEY constraint failed。必须落 NULL。
func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func (s *Store) ListRoutes(ctx context.Context) ([]Route, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, public_name, provider_id, upstream_model_id, enabled, priority, fallback_route_id, extra_json, created_at FROM routes ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make([]Route, 0)
	for rows.Next() {
		var r Route
		var enabled int
		var fallback, extra sql.NullString
		if err := rows.Scan(&r.ID, &r.PublicName, &r.ProviderID, &r.UpstreamModelID, &enabled, &r.Priority, &fallback, &extra, &r.CreatedAt); err != nil {
			return nil, err
		}
		r.Enabled = enabled == 1
		r.FallbackRouteID = fallback.String
		r.ExtraJSON = extra.String
		result = append(result, r)
	}
	return result, rows.Err()
}

func (s *Store) GetRoute(ctx context.Context, id string) (*Route, error) {
	var r Route
	var enabled int
	var fallback, extra sql.NullString
	err := s.db.QueryRowContext(ctx,
		`SELECT id, public_name, provider_id, upstream_model_id, enabled, priority, fallback_route_id, extra_json, created_at FROM routes WHERE id = ?`, id).
		Scan(&r.ID, &r.PublicName, &r.ProviderID, &r.UpstreamModelID, &enabled, &r.Priority, &fallback, &extra, &r.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	r.Enabled = enabled == 1
	r.FallbackRouteID = fallback.String
	r.ExtraJSON = extra.String
	return &r, nil
}

func (s *Store) CreateRoute(ctx context.Context, r *Route) error {
	now := time.Now().UnixMilli()
	enabled := 0
	if r.Enabled {
		enabled = 1
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO routes (id, public_name, provider_id, upstream_model_id, enabled, priority, fallback_route_id, extra_json, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ID, r.PublicName, r.ProviderID, r.UpstreamModelID, enabled, r.Priority, nullIfEmpty(r.FallbackRouteID), nullIfEmpty(r.ExtraJSON), now)
	return err
}

func (s *Store) UpdateRoute(ctx context.Context, id string, r *Route) error {
	enabled := 0
	if r.Enabled {
		enabled = 1
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE routes SET public_name = ?, provider_id = ?, upstream_model_id = ?, enabled = ?, priority = ?, fallback_route_id = ?, extra_json = ? WHERE id = ?`,
		r.PublicName, r.ProviderID, r.UpstreamModelID, enabled, r.Priority, nullIfEmpty(r.FallbackRouteID), nullIfEmpty(r.ExtraJSON), id)
	return err
}

func (s *Store) DeleteRoute(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM routes WHERE id = ?`, id)
	return err
}
