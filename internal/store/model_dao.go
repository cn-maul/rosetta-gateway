package store

import (
	"context"
	"database/sql"
)

type UpstreamModel struct {
	ID               string
	ProviderID       string
	ModelID          string
	DisplayName      string
	Enabled          bool
	ContextWindow    int
	MaxOutputTokens  int
	SupportsThinking *bool
	DefaultExtraJSON string
}

// modelColumns 是 upstream_models 三处读路径共用的列清单。
const modelColumns = `id, provider_id, model_id, display_name, enabled, context_window, max_output_tokens, supports_thinking, default_extra_json`

// nullIfZeroInt 把 0 写回 NULL。
// context_window / max_output_tokens 是可空列，0 与"未知"语义不同，
// 且可空整数列在 SQLite 中若被写入 NULL 后仍需能被读回，故统一走 NULL。
func nullIfZeroInt(v int) any {
	if v == 0 {
		return nil
	}
	return v
}

// scanner 覆盖 *sql.Row 与 *sql.Rows，便于复用同一段 scan 逻辑。
type scanner interface {
	Scan(dest ...any) error
}

// scanModel 用 sql.NullXxx 承接可空列，再落回结构体。
// 直接把可空列扫进 string/int 会在列为 NULL 时报
// "converting NULL to string is unsupported"，这是此前 list/get 失败的原因。
func scanModel(sc scanner) (UpstreamModel, error) {
	var m UpstreamModel
	var enabled int
	var displayName, extraJSON sql.NullString
	var ctxWindow, maxOut sql.NullInt64
	var thinking sql.NullBool

	err := sc.Scan(&m.ID, &m.ProviderID, &m.ModelID, &displayName, &enabled,
		&ctxWindow, &maxOut, &thinking, &extraJSON)
	if err != nil {
		return m, err
	}

	m.Enabled = enabled == 1
	m.DisplayName = displayName.String
	m.DefaultExtraJSON = extraJSON.String
	m.ContextWindow = int(ctxWindow.Int64)
	m.MaxOutputTokens = int(maxOut.Int64)
	if thinking.Valid {
		v := thinking.Bool
		m.SupportsThinking = &v
	}
	return m, nil
}

func (s *Store) ListUpstreamModels(ctx context.Context, providerID string) ([]UpstreamModel, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+modelColumns+` FROM upstream_models WHERE provider_id = ? ORDER BY model_id`, providerID)
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

func (s *Store) ListAllUpstreamModels(ctx context.Context) ([]UpstreamModel, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+modelColumns+` FROM upstream_models ORDER BY provider_id, model_id`)
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

func (s *Store) GetUpstreamModel(ctx context.Context, id string) (*UpstreamModel, error) {
	m, err := scanModel(s.db.QueryRowContext(ctx,
		`SELECT `+modelColumns+` FROM upstream_models WHERE id = ?`, id))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &m, nil
}

func (s *Store) CreateUpstreamModel(ctx context.Context, m *UpstreamModel) error {
	enabled := 0
	if m.Enabled {
		enabled = 1
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO upstream_models (id, provider_id, model_id, display_name, enabled, context_window, max_output_tokens, supports_thinking, default_extra_json) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		m.ID, m.ProviderID, m.ModelID, nullIfEmpty(m.DisplayName), enabled,
		nullIfZeroInt(m.ContextWindow), nullIfZeroInt(m.MaxOutputTokens), m.SupportsThinking, nullIfEmpty(m.DefaultExtraJSON))
	return err
}

func (s *Store) UpdateUpstreamModel(ctx context.Context, id string, m *UpstreamModel) error {
	enabled := 0
	if m.Enabled {
		enabled = 1
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE upstream_models SET model_id = ?, display_name = ?, enabled = ?, context_window = ?, max_output_tokens = ?, supports_thinking = ?, default_extra_json = ? WHERE id = ?`,
		m.ModelID, nullIfEmpty(m.DisplayName), enabled,
		nullIfZeroInt(m.ContextWindow), nullIfZeroInt(m.MaxOutputTokens), m.SupportsThinking, nullIfEmpty(m.DefaultExtraJSON), id)
	return err
}

func (s *Store) DeleteUpstreamModel(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM upstream_models WHERE id = ?`, id)
	return err
}

func (s *Store) UpsertUpstreamModel(ctx context.Context, m *UpstreamModel) error {
	enabled := 0
	if m.Enabled {
		enabled = 1
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO upstream_models (id, provider_id, model_id, display_name, enabled, context_window, max_output_tokens, supports_thinking, default_extra_json)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(provider_id, model_id) DO UPDATE SET display_name = excluded.display_name, enabled = excluded.enabled, context_window = excluded.context_window, max_output_tokens = excluded.max_output_tokens, supports_thinking = excluded.supports_thinking, default_extra_json = excluded.default_extra_json`,
		m.ID, m.ProviderID, m.ModelID, nullIfEmpty(m.DisplayName), enabled,
		nullIfZeroInt(m.ContextWindow), nullIfZeroInt(m.MaxOutputTokens), m.SupportsThinking, nullIfEmpty(m.DefaultExtraJSON))
	return err
}
