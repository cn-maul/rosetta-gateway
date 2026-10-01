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

	// 价格：单位为「元 / 百万 tokens」，0 = 未配置（费用统计按 0 计）。
	// PriceInput 对应缓存未命中的输入，PriceCacheHit 对应缓存命中的输入，
	// PriceOutput 对应输出。
	PriceInput    float64
	PriceCacheHit float64
	PriceOutput   float64
}

// modelColumns 是 upstream_models 三处读路径共用的列清单。
const modelColumns = `id, provider_id, model_id, display_name, enabled, context_window, max_output_tokens, supports_thinking, price_input, price_cache_hit, price_output`

// nullIfZeroInt 把 0 写回 NULL。
// context_window / max_output_tokens 是可空列，0 与"未知"语义不同，
// 且可空整数列在 SQLite 中若被写入 NULL 后仍需能被读回，故统一走 NULL。
func nullIfZeroInt(v int) any {
	if v == 0 {
		return nil
	}
	return v
}

// nullIfZeroFloat 同上，用于价格列：0 与 NULL 同义（未配置价格），
// 落 NULL 让「没设过价」和「设了 0 元」在库里可区分地表达为同一件事。
func nullIfZeroFloat(v float64) any {
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
	var displayName sql.NullString
	var ctxWindow, maxOut sql.NullInt64
	var thinking sql.NullBool
	var priceIn, priceHit, priceOut sql.NullFloat64

	err := sc.Scan(&m.ID, &m.ProviderID, &m.ModelID, &displayName, &enabled,
		&ctxWindow, &maxOut, &thinking, &priceIn, &priceHit, &priceOut)
	if err != nil {
		return m, err
	}

	m.Enabled = enabled == 1
	m.DisplayName = displayName.String
	m.ContextWindow = int(ctxWindow.Int64)
	m.MaxOutputTokens = int(maxOut.Int64)
	m.PriceInput = priceIn.Float64
	m.PriceCacheHit = priceHit.Float64
	m.PriceOutput = priceOut.Float64
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
		`INSERT INTO upstream_models (id, provider_id, model_id, display_name, enabled, context_window, max_output_tokens, supports_thinking, price_input, price_cache_hit, price_output) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		m.ID, m.ProviderID, m.ModelID, nullIfEmpty(m.DisplayName), enabled,
		nullIfZeroInt(m.ContextWindow), nullIfZeroInt(m.MaxOutputTokens), m.SupportsThinking,
		nullIfZeroFloat(m.PriceInput), nullIfZeroFloat(m.PriceCacheHit), nullIfZeroFloat(m.PriceOutput))
	return err
}

func (s *Store) UpdateUpstreamModel(ctx context.Context, id string, m *UpstreamModel) error {
	enabled := 0
	if m.Enabled {
		enabled = 1
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE upstream_models SET model_id = ?, display_name = ?, enabled = ?, context_window = ?, max_output_tokens = ?, supports_thinking = ?, price_input = ?, price_cache_hit = ?, price_output = ? WHERE id = ?`,
		m.ModelID, nullIfEmpty(m.DisplayName), enabled,
		nullIfZeroInt(m.ContextWindow), nullIfZeroInt(m.MaxOutputTokens), m.SupportsThinking,
		nullIfZeroFloat(m.PriceInput), nullIfZeroFloat(m.PriceCacheHit), nullIfZeroFloat(m.PriceOutput), id)
	return err
}

// DeleteUpstreamModel 删除一个上游模型；id 不存在时返回 ErrNotFound。
func (s *Store) DeleteUpstreamModel(ctx context.Context, id string) error {
	return deleteByID(ctx, s.db, "upstream_models", id)
}

// UpsertUpstreamModel 按 (provider_id, model_id) 幂等写入。
//
// 冲突时**不覆盖价格列**：价格是运维手工填的配置，而本方法的调用方是
// 「探测 / 批量导入模型列表」—— 上游返回的模型列表永远不带价格，
// 若把价格也写进 DO UPDATE SET，一次重新导入就会把配好的单价悄悄清零。
func (s *Store) UpsertUpstreamModel(ctx context.Context, m *UpstreamModel) error {
	return upsertUpstreamModel(ctx, s.db, m)
}

func upsertUpstreamModel(ctx context.Context, ex execer, m *UpstreamModel) error {
	enabled := 0
	if m.Enabled {
		enabled = 1
	}
	_, err := ex.ExecContext(ctx,
		`INSERT INTO upstream_models (id, provider_id, model_id, display_name, enabled, context_window, max_output_tokens, supports_thinking, price_input, price_cache_hit, price_output)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(provider_id, model_id) DO UPDATE SET display_name = excluded.display_name, enabled = excluded.enabled, context_window = excluded.context_window, max_output_tokens = excluded.max_output_tokens, supports_thinking = excluded.supports_thinking`,
		m.ID, m.ProviderID, m.ModelID, nullIfEmpty(m.DisplayName), enabled,
		nullIfZeroInt(m.ContextWindow), nullIfZeroInt(m.MaxOutputTokens), m.SupportsThinking,
		nullIfZeroFloat(m.PriceInput), nullIfZeroFloat(m.PriceCacheHit), nullIfZeroFloat(m.PriceOutput))
	return err
}

// ImportUpstreamModels 在单个事务里批量 upsert 一组模型。
//
// 逐条自动提交（旧实现）会在第 N 条失败时把前 N-1 条永久落下：批量导入是「要么
// 全进、要么全不进」的语义，部分导入让运维以为清单已同步、实则残缺，且重试前
// 库里已经多了半套模型。空切片直接返回 nil，不空开一个事务。
func (s *Store) ImportUpstreamModels(ctx context.Context, models []*UpstreamModel) error {
	if len(models) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	for _, m := range models {
		if err := upsertUpstreamModel(ctx, tx, m); err != nil {
			return err
		}
	}
	return tx.Commit()
}
