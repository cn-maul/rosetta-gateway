package store

import (
	"context"
	"database/sql"
	"errors"
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
	return createUpstreamModel(ctx, s.db, m)
}

// CreateUpstreamModelTx 在一笔事务里创建模型，供批量导入整体回滚使用。
func (s *Store) CreateUpstreamModelTx(ctx context.Context, tx Tx, m *UpstreamModel) error {
	return createUpstreamModel(ctx, tx, m)
}

// createUpstreamModel 接受 execer，供事务内复用（见 ImportUpstreamModels）。
func createUpstreamModel(ctx context.Context, ex execer, m *UpstreamModel) error {
	enabled := 0
	if m.Enabled {
		enabled = 1
	}
	_, err := ex.ExecContext(ctx,
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
//
// 保留旧方法给不需要「自动摘链」的调用方；管理端点走
// DeleteUpstreamModelDetaching（见下）。
func (s *Store) DeleteUpstreamModel(ctx context.Context, id string) error {
	return deleteByID(ctx, s.db, "upstream_models", id)
}

// ErrModelIsRouteHead 表示该模型是某条路由的**链首**，因此不能删。
//
// 单独一个错误类型而不是复用外键违规：处置动作完全不同 —���
// 外键违规要先删引用，本错误要先把该路由换成别的链首。
var ErrModelIsRouteHead = errors.New("upstream model is a route head")

// DeleteUpstreamModelDetaching 删除一个上游模型，并自动把它从所有
// 故障转移链里摘掉；若是某条路由的链首则**拒绝删除**（2026-10-14）。
//
// # 为什么链首拒绝而不是顺延
//
// 顺延（删掉 A 后 A→B→C 变 B→C）看起来更友好，但它是**替管理员做了一次
// 决策**：链的顺序表达的是「先试谁、后试谁」的显式配置，删掉首选之后让
// 次选顶上，等于悄悄改了这条路由的流量分配与优先级，而管理员以为自己
// 只是删了一个模型。拒绝删除把这个决策还给人：界面上告诉他是链首、
// 让他自己去「路由」页改顺序或换主目标。
//
// # 为什么摘链 + 重排要在一个事务里
//
// 中途状态会让快照读到断链：先删目标行、还没重排时，position 会出现空洞，
// 而 Resolve 按 position 升序遍历 —— 空洞本身不致命，但配合「主目标列还没
// 更新」就会让按名解析拿到已删除的模型。所以整个操作原子完成。
//
// # 探测记录级联清理
//
// model_probes 是纯派生数据（见 store.go 的建表注释），对已删模型没有意义，
// 留着只会在 /probes/models 里变成一堆取不到模型行的孤儿。历史曲线断掉
// 是可接受的：那个模型已经不存在了，「它以前通不通」不再有处置价值。
func (s *Store) DeleteUpstreamModelDetaching(ctx context.Context, id string) ([]string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	// 链首判定：position = 0 的那一行。
	//
	// 只看 route_targets 是**不够**的：routes.provider_id/upstream_model_id
	// 是「主目标」列，异常数据下可能还指着某个已不在链上的模型。
	// 两个来源都查，否则会出现「routes 说它是主目标、链上却没有」时
	// 删掉它、留下一个指向空模型的路由。
	//
	// 用 dbtx（而非 execer）是因为它带 QueryContext —— 本函数需要遍历
	// 多行，而 execer 只有 QueryRowContext。dbtx 是包里已有的
	// *sql.DB / *sql.Tx 公共面，不另立一套同义接口。
	heads, err := headRoutesOfModel(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	if len(heads) > 0 {
		return nil, ErrModelIsRouteHead
	}

	// 摘链：删掉引用该模型的全部目标行。
	affected, err := affectedRoutesOfModel(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM route_targets WHERE upstream_model_id = ?`, id); err != nil {
		return nil, err
	}

	// 重排：摘掉之后 position 会出现空洞，而 position 升序即尝试顺序。
	// 不重排的话链路仍能工作（空洞不影响 ORDER BY），但界面上会显示
	// 「0, 2, 3」这种跳号，看起来像数据坏了。
	for _, routeID := range affected {
		if err := compactTargetPositions(ctx, tx, routeID); err != nil {
			return nil, err
		}
	}

	// 探测记录级联清理（见函数注释）。
	if _, err := tx.ExecContext(ctx, `DELETE FROM model_probes WHERE model_id = ?`, id); err != nil {
		return nil, err
	}

	if err := deleteByID(ctx, tx, "upstream_models", id); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return affected, nil
}

// headRoutesOfModel 返回把该模型当作**链首**的路由 id 列表。
//
// 查两个来源：route_targets 的 position 0，以及 routes 的链首镜像列。
// 正常写入路径同步两者；异常数据下两处都查，删除判定才与界面一致。
func headRoutesOfModel(ctx context.Context, ex execer, modelID string) ([]string, error) {
	rows, err := ex.QueryContext(ctx,
		`SELECT route_id FROM route_targets WHERE upstream_model_id = ? AND position = 0`, modelID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var heads []string
	for rows.Next() {
		var routeID string
		if err := rows.Scan(&routeID); err != nil {
			return nil, err
		}
		heads = append(heads, routeID)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// 链首镜像列：即使 route_targets 里没有对应行，routes 仍可能指着它。
	//
	// 单行查询用 QueryRowContext 扫进一个 string —— 一开始写成 Scan(&[]string)，
	// 编译能过（any 接一切）、运行才炸：driver.Value 是 string，塞不进
	// *[]string。跨 SQL 边界的 Scan 目标必须显式给出正确类型。
	seen := make(map[string]bool, len(heads))
	for _, h := range heads {
		seen[h] = true
	}
	var compat string
	err = ex.QueryRowContext(ctx,
		`SELECT id FROM routes WHERE upstream_model_id = ? LIMIT 1`, modelID).Scan(&compat)
	switch {
	case err == nil:
		if !seen[compat] {
			heads = append(heads, compat)
		}
	case errors.Is(err, sql.ErrNoRows):
		// 没有镜像列引用它，正常。
	default:
		return nil, err
	}
	return heads, nil
}

// affectedRoutesOfModel 返回引用该模型的**所有**路由 id（不论 position）。
func affectedRoutesOfModel(ctx context.Context, ex execer, modelID string) ([]string, error) {
	rows, err := ex.QueryContext(ctx,
		`SELECT DISTINCT route_id FROM route_targets WHERE upstream_model_id = ?`, modelID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var routeID string
		if err := rows.Scan(&routeID); err != nil {
			return nil, err
		}
		ids = append(ids, routeID)
	}
	return ids, rows.Err()
}

// compactTargetPositions 把某条链的 position 重排为 0..n-1。
//
// 保持**相对顺序**：删掉中间成员后，剩下成员的先后关系不变（这正是
// 「从故障转移链里拿掉一个备选」的语义）。所以按现有 position 升序重新
// 编号，而不是按 id 或时间。
func compactTargetPositions(ctx context.Context, ex execer, routeID string) error {
	rows, err := ex.QueryContext(ctx,
		`SELECT id FROM route_targets WHERE route_id = ? ORDER BY position`, routeID)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for i, tid := range ids {
		if _, err := ex.ExecContext(ctx,
			`UPDATE route_targets SET position = ? WHERE id = ?`, i, tid); err != nil {
			return err
		}
	}
	return nil
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
// effort_levels：**只在未配置时回填**（2026-10-14 改）。
//
// 原来是「一律不覆盖」，理由是导入侧拿不到这个信息 —— 那个前提已经不成立：
// 部分上游的 /models 明明带着 reasoning_supported_efforts（一并带出见
// upstream.rawModelEntry），导入路径现在能拿到真实档位了。于是「不覆盖」
// 从一条保护措施变成一个缺陷：**已有模型永远填不上**，管理员只能逐个
// 打开编辑弹窗手勾。
//
// 改成只在现有为空时写入（COALESCE(excluded, 现有)），是为了同时保住两件事：
//   - 能自动填（上游报了就填，不必手工）；
//   - 不覆盖手工配置（管理员手动挑过的档位组合、或按模型文档调整过的，
//     一次重新发现不该被上游的原始值顶回去）。
//
// 代价要说清：上游**换**了档位集合时，已手工配过的模型不会跟着变 ——
// 这是刻意的（自动跟随会让「我明明按文档配好了」在某次同步后被改掉，
// 且无迹可寻）。想要更新的模型清空一次即可回填。
//
// # supports_thinking 必须用 COALESCE（2026-10-14）
//
// 导入侧现在会带上游 /models 报的这个字段，而它的三态里 **nil = 上游没报**
// （不是「不支持」）。原来这一列是直接 `= excluded.supports_thinking`，
// 于是一个**没报**的上游会把已有配置抹成 NULL —— 把「已明确支持」变成
// 「不知道」。后果不只是配置丢失：SupportsThinking 一旦从 true 变 nil，
// 数据面就不再据此做任何判断，但管理界面上「支持思考」与「未配置」长得
// 一样，看起来像只是没填。
//
// COALESCE(excluded, 现有) 的语义正是想要的：
//   - 上游明确表态（true/false）→ 覆盖，让上游的变更能被看见；
//   - 上游没报（NULL）→ 保留原值，导入侧永远不破坏已有配置。
//
// effort_levels 用同样的 COALESCE，但**参数顺序是反的**：
// `COALESCE(现有, excluded)` —— 现有配置优先，只有它为空时才用上游的值
// 回填。supports_thinking 那边是 `COALESCE(excluded, 现有)`（上游优先）。
//
// 两个方向不同不是笔误，而是两个字段的性质不同：
//   - supports_thinking 是**上游的能力声明**，上游是权威 → 上游优先；
//   - effort_levels 是**经过人工判断的配置**（按模型文档挑的子集，可能
//     故意只勾其中几档），管理员的意图优先 → 现有优先。
//
// 写成同向的话，另一侧必然错：挡位若上游优先，一次重新发现就会把管理员
// 挑过的组合顶回上游原始集合；supports_thinking 若现有优先，则上游新增支持
// 时网关看不见。
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
		 ON CONFLICT(provider_id, model_id) DO UPDATE SET
		   display_name = excluded.display_name,
		   enabled = excluded.enabled,
		   context_window = excluded.context_window,
		   max_output_tokens = excluded.max_output_tokens,
		   supports_thinking = COALESCE(excluded.supports_thinking, upstream_models.supports_thinking),
		   effort_levels = COALESCE(upstream_models.effort_levels, excluded.effort_levels)`,
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
