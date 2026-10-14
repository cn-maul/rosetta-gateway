package store

import (
	"context"
	"database/sql"
	"strings"

	"github.com/cn-maul/rosetta-gateway/internal/effort"
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

	// EffortLevels 是该模型**真实支持**的思考挡位，按强度升序。
	// nil = 未配置（网关不干预思考强度，完全沿用本功能落地前的行为）。
	//
	// UnknownLevels 是配置里出现但本网关不认识的原样值（厂商私有写法）。
	// 它必须与 EffortLevels 分开：网关夹不了它们，但也不能丢 —— 丢掉的配置在
	// 界面上会表现为「配了不生效」且无从排查。
	EffortLevels  []effort.Level
	UnknownLevels []string

	// 价格：单位为「元 / 百万 tokens」，0 = 未配置（费用统计按 0 计）。
	// PriceInput 对应缓存未命中的输入，PriceCacheHit 对应缓存命中的输入，
	// PriceOutput 对应输出。
	PriceInput    float64
	PriceCacheHit float64
	PriceOutput   float64

	// GroupID 是模型归组（跨上游的同一个模型标成一组，统计合并显示）。
	// 空串 = 未归组。组的权威显示名在 model_groups.display_name，
	// 本列只是归属指针 —— 改组名不需要动这里（见 model_group_dao.go）。
	GroupID string
}

// modelColumns 是 upstream_models 三处读路径共用的列清单。
const modelColumns = `id, provider_id, model_id, display_name, enabled, context_window, max_output_tokens, supports_thinking, effort_levels, price_input, price_cache_hit, price_output, group_id`

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

// effortColumnValue 把结构体里的挡位切片拼回 CSV 列值。
//
// 空切片落 **nil**（= NULL = 未配置）而不是空串：这两个状态在数据面语义上
// 不同（前者不干预思考强度），必须可区分地存。unknown 原样附在后面，
// 所以「配了一个网关不认识的厂商私有档」不会在保存时被抹掉。
func effortColumnValue(m *UpstreamModel) any {
	csv := effort.FormatLevels(m.EffortLevels, m.UnknownLevels)
	if csv == "" {
		return nil
	}
	return csv
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
	var effortCSV sql.NullString
	var priceIn, priceHit, priceOut sql.NullFloat64
	var groupID sql.NullString

	err := sc.Scan(&m.ID, &m.ProviderID, &m.ModelID, &displayName, &enabled,
		&ctxWindow, &maxOut, &thinking, &effortCSV, &priceIn, &priceHit, &priceOut, &groupID)
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
	m.GroupID = groupID.String
	if thinking.Valid {
		v := thinking.Bool
		m.SupportsThinking = &v
	}
	// NULL 与空串都表示「未配置」——两种都得到空 EffortLevels，数据面据此
	// 不干预思考强度。解析在读取时做一次，快照侧拿到的是已就绪的切片。
	if effortCSV.Valid && effortCSV.String != "" {
		m.EffortLevels, m.UnknownLevels = effort.ParseLevels(effortCSV.String)
	}
	return m, nil
}

func (s *Store) ListUpstreamModels(ctx context.Context, providerID string) ([]UpstreamModel, error) {
	rows, err := s.read.QueryContext(ctx,
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
	rows, err := s.read.QueryContext(ctx,
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
	m, err := scanModel(s.read.QueryRowContext(ctx,
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
		`INSERT INTO upstream_models (id, provider_id, model_id, display_name, enabled, context_window, max_output_tokens, supports_thinking, effort_levels, price_input, price_cache_hit, price_output, group_id) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		m.ID, m.ProviderID, m.ModelID, nullIfEmpty(m.DisplayName), enabled,
		nullIfZeroInt(m.ContextWindow), nullIfZeroInt(m.MaxOutputTokens), m.SupportsThinking,
		effortColumnValue(m),
		nullIfZeroFloat(m.PriceInput), nullIfZeroFloat(m.PriceCacheHit), nullIfZeroFloat(m.PriceOutput),
		nullIfEmpty(m.GroupID))
	return err
}

func (s *Store) UpdateUpstreamModel(ctx context.Context, id string, m *UpstreamModel) error {
	enabled := 0
	if m.Enabled {
		enabled = 1
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE upstream_models SET model_id = ?, display_name = ?, enabled = ?, context_window = ?, max_output_tokens = ?, supports_thinking = ?, effort_levels = ?, price_input = ?, price_cache_hit = ?, price_output = ?, group_id = ? WHERE id = ?`,
		m.ModelID, nullIfEmpty(m.DisplayName), enabled,
		nullIfZeroInt(m.ContextWindow), nullIfZeroInt(m.MaxOutputTokens), m.SupportsThinking,
		effortColumnValue(m),
		nullIfZeroFloat(m.PriceInput), nullIfZeroFloat(m.PriceCacheHit), nullIfZeroFloat(m.PriceOutput),
		nullIfEmpty(m.GroupID), id)
	return checkAffected(res, err)
}

// DeleteUpstreamModel 删除一个上游模型；id 不存在时返回 ErrNotFound。
func (s *Store) DeleteUpstreamModel(ctx context.Context, id string) error {
	return deleteByID(ctx, s.db, "upstream_models", id)
}

// BatchUpdateModelPrices 把同一组价格批量写到多个模型。
//
// 为什么单独一个方法而不是循环调 UpdateUpstreamModel：批量改价是「要么全进、
// 要么全不进」的语义（与 ImportUpstreamModels 同理由），循环逐条自动提交会在
// 第 N 条失败时把前 N-1 条永久落下，运维以为全改好了、实则改了一半。
//
// 价格必须复用 nullIfZeroFloat：0 与 NULL 同义（未配置价格），批量场景下
// 传 0 等于「批量清除价格」，与单条 PATCH 的既有约定保持一致 —— 否则
// 批量写 0 会落成字面 0 元，与单条路径语义分裂。
//
// 返回实际更新的行数。调用方（admin handler）用它核对「ids 里有多少真的
// 存在」—— 传了 10 个 id 只更新 8 个，说明有 2 个 id 写错了，必须报错
// 而不是静默跳过（沿用凭据「坏值必须让操作失败而不是部分生效」的立场）。
func (s *Store) BatchUpdateModelPrices(ctx context.Context, ids []string, priceInput, priceCacheHit, priceOutput float64) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	placeholders := strings.Repeat("?,", len(ids))
	placeholders = placeholders[:len(placeholders)-1]
	args := make([]any, 0, len(ids)+3)
	args = append(args, nullIfZeroFloat(priceInput), nullIfZeroFloat(priceCacheHit), nullIfZeroFloat(priceOutput))
	for _, id := range ids {
		args = append(args, id)
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE upstream_models SET price_input = ?, price_cache_hit = ?, price_output = ? WHERE id IN (`+placeholders+`)`,
		args...)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	return n, nil
}

// UpsertUpstreamModel 按 (provider_id, model_id) 幂等写入。
//
// 冲突时**不覆盖价格列**：价格是运维手工填的配置，而本方法的调用方是
// 「探测 / 批量导入模型列表」—— 上游返回的模型列表永远不带价格，
// 若把价格也写进 DO UPDATE SET，一次重新导入就会把配好的单价悄悄清零。
//
// effort_levels 同理不覆盖：它也是手工按模型文档填的，而导入侧拿不到这个
// 信息（各家 /models 不返回挡位集合）。覆盖它等于「重新导入 = 抹掉挡位配置」。
func (s *Store) UpsertUpstreamModel(ctx context.Context, m *UpstreamModel) error {
	return upsertUpstreamModel(ctx, s.db, m)
}

func upsertUpstreamModel(ctx context.Context, ex execer, m *UpstreamModel) error {
	enabled := 0
	if m.Enabled {
		enabled = 1
	}
	_, err := ex.ExecContext(ctx,
		`INSERT INTO upstream_models (id, provider_id, model_id, display_name, enabled, context_window, max_output_tokens, supports_thinking, effort_levels, price_input, price_cache_hit, price_output)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(provider_id, model_id) DO UPDATE SET display_name = excluded.display_name, enabled = excluded.enabled, context_window = excluded.context_window, max_output_tokens = excluded.max_output_tokens, supports_thinking = excluded.supports_thinking`,
		m.ID, m.ProviderID, m.ModelID, nullIfEmpty(m.DisplayName), enabled,
		nullIfZeroInt(m.ContextWindow), nullIfZeroInt(m.MaxOutputTokens), m.SupportsThinking,
		effortColumnValue(m),
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
