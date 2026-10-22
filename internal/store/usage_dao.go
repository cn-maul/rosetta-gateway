package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

type UsageRecord struct {
	ID          string
	Ts          int64
	AccessKeyID string
	// UserID 是**冗余固化**的归属（多用户改造 P0），不靠 JOIN access_keys 回溯。
	//
	// 为什么冗余：归属是历史事实。用户改名、key 被删、key 转归别人，
	// 都不该改变「这条消耗是谁的」。若靠 JOIN 回溯，删key 后用量记录
	// 就失去归属，审计链断裂。
	// 空串 = 损坏/人工写入的无归属行；当前写入路径必须固化具体 user id。
	UserID          string
	PublicModel     string
	ProviderID      string
	UpstreamModel   string
	IngressProtocol string
	Stream          bool
	InputTokens     int64
	OutputTokens    int64
	TotalTokens     int64
	ReasoningTokens int64
	CachedTokens    int64
	// CachedCreationTokens 是 Anthropic cache_creation_input_tokens ——
	// 本次请求写入缓存的输入 token（已含在 InputTokens 里）。OpenAI 系
	// 协议自动缓存、不单独上报写入量，该值恒为 0。
	CachedCreationTokens int64
	UsageState           string
	Status               string
	HTTPStatus           int
	ErrorCode            string
	// ErrorDetail 是上游返回的失败详情（脱敏 + 截断后），供调用历史排查。
	// 空串 = 无详情（成功、预检失败、或 TransportError 这类没有上游正文的失败）。
	ErrorDetail string
	LatencyMs   int64
	TTFBMs      int64
	RequestID   string
	// PreviousCost 是**落库前**已有的 cost_total，只在重算路径上被读
	// （见 RecomputeCost）。
	//
	// 补扣余额必须用「重算值 - 原值」的**差额**：原值那一笔在落库时
	// 已经扣过一次（或本该扣但没扣成），用重算值去扣会重复收钱。
	//
	// 写路径不填它 —— 写入时 cost_total 由 freezeUsageCost 现算，没有
	// 「前一个值」可言。
	PreviousCost float64
	// RouteTrace 是失败决策轨迹（2026-10-17）：按发生顺序记录链上每个目标的
	// 决策与原因，形如 "1:deepseek/model -> 404 model_not_found (switch);
	// 2:openai/gpt-4o -> ok"。
	//
	// 为什么用文本而不是结构化列：这张表已有 30 天明细保留期与日归档机制，
	// 轨迹只服务于「这一次为什么走了备用上游」的排障，不需要按字段聚合；
	// 一个 TEXT 列就能在调用明细里原样展示，避免为一次排障新增 JSON schema
	// 与迁移面。空串 = 单目标或成功首跳，没有可讲的决策。
	RouteTrace string
}

// CreateUsageRecord 落一条用量记录。
//
// Ts 为 0 时取当前时刻；非 0 则用它。此前该字段被**整个忽略**、恒写 time.Now()，
// 于是任何显式带 Ts 的插入都静默落在「现在」—— 回填历史数据时整条时间线错位，
// 而且因为字段名和列名都叫 ts，看不出哪里错了。
//
// 它是 CreateUsageRecordWithCost 的薄包装（只返回 error，丢掉固化的费用），
// 保留是为了让既有 26 处调用点（测试、剪枝回填、E2E）一行都不用改。
// 需要**扣费**的调用方请用 CreateUsageRecordWithCost 拿回 cost_total ——
// 见那里的说明。
func (s *Store) CreateUsageRecord(ctx context.Context, r *UsageRecord) error {
	_, err := s.CreateUsageRecordWithCost(ctx, r)
	return err
}

// CreateUsageRecordWithCost 落一条用量记录，**并返回该条固化的费用（元）**。
//
// # 为什么需要这个「新方法」而不是改 CreateUsageRecord 的签名
//
// 余额扣费要求「扣掉的金额与 cost_total 完全相等」，而 cost_total 是在这里
// 按**落库当时**的单价算出来的。所以最自然的两条路是：
//   - 改签名让本方法返回 cost：调用点全要改（实测 28 处引用，其中 26 处在
//     别的任务拥有的测试文件里），且「返回值被丢弃」在 Go 里是合法的 ——
//     改了签名之后仍会有人写 `_ = st.CreateUsageRecord(...)` 把它照旧丢掉。
//   - 另加 CostOfRecord 按 id 读回：多一次主键查询，且读回的是**已落库**的
//     值，与刚算出的那个值之间隔了一次 IO 往返。
//
// 本方法是第三条：老签名保留为包装（零回归），新签名给出费用。既不漏
// （唯一算费用的地方仍只有 freezeUsageCost 一处），也不多余（无额外查询）。
//
// 返回的 cost 与写进 cost_total 列的**是同一个变量**，不是重算一次 ——
// 所以「报表显示花了 X、余额扣了 Y」在结构上就不可能发生。
//
// 落库失败时返回 (0, err)：此时没有任何记录，费用无意义。**不要**把
// 「算出来了但没落库」的金额拿去扣费 —— 那会扣一笔永远没有对账记录的
// 钱。
func (s *Store) CreateUsageRecordWithCost(ctx context.Context, r *UsageRecord) (float64, error) {
	ts := r.Ts

	// token 数一律不许为负。
	//
	// 上游（或某个不守规矩的中转）回报负数时，负值会经两条路径放大：
	//   - AFTER INSERT 触发器把 total_tokens 累加进 access_keys.used_tokens
	//     与 usage_totals —— **终身累计**直接被拉低，且不会自行恢复；
	//   - 配额判定「used >= quota」因此永远不成立，等于凭空发放配额。
	// 两个后果都不是「显示难看」，是账目与配额同时失真。
	//
	// 为什么在入口 clamp 而不是加 DDL CHECK：SQLite 无法给**已有表**追加
	// CHECK 约束（要重建表 = 搬全部历史明细 + 重建索引 + 重建触发器），
	// 迁移风险远大于收益。而这里是唯一写入口（CreateUsageRecord），
	// 在此归一已覆盖所有调用方 —— 导入、剪枝回填、E2E 都走它。
	//
	// 归一为 0 而不是报错：一条用量记录的可信度本就由 usage_state 表达，
	// 为一条脏数据拒绝整个请求会让上游的不当行为变成网关的可用性问题。
	// 已在 DESIGN §11.1 的「宁可少算」原则内。
	if r.InputTokens < 0 {
		r.InputTokens = 0
	}
	if r.OutputTokens < 0 {
		r.OutputTokens = 0
	}
	if r.TotalTokens < 0 {
		r.TotalTokens = 0
	}
	if r.ReasoningTokens < 0 {
		r.ReasoningTokens = 0
	}
	if r.CachedTokens < 0 {
		r.CachedTokens = 0
	}
	if r.CachedCreationTokens < 0 {
		r.CachedCreationTokens = 0
	}
	if ts == 0 {
		ts = time.Now().UnixMilli()
	}
	stream := 0
	if r.Stream {
		stream = 1
	}
	// P1.5 固化费用：按落库**当时**的上游单价算好写进 cost_total，之后所有
	// 费用查询一律 SUM(cost_total)，不再 JOIN upstream_models 按当前价重算 ——
	// 否则「管理员改个价，昨天报表跟着变」。
	//
	// 刻意放在 DAO 内而不是调用方：落库点有 worker、sync fallback、启动自检
	// 三处，任一处的闭包都不同 —— 在调用方算等于把「别忘了算」变成三个
	// 可以各自忘记的地方，而忘了的表征是**费用静默偏低**，没人会发现。
	cost, costPending := s.freezeUsageCost(ctx, r)
	// 查价失败时把待重算标记**随这条 INSERT 一起写进去**。
	// 旧实现是 INSERT 之后再 UPDATE 那一行，而此刻行还不存在 ——
	// UPDATE 影响 0 行且不报错，于是这笔用量带着**原 usage_state**
	// （通常是 reported）与 cost_total=0 落库，后台的
	// RecomputePendingCosts 按 usage_state='cost_pending' 扫描，
	// 永远捞不到它 —— 「一次读池抖动 = 一笔永久免单」，且事后无从发现。
	usageState := r.UsageState
	if costPending {
		usageState = usageStateCostPending
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO usage_records (id, ts, access_key_id, user_id, public_model, provider_id, upstream_model, ingress_protocol, stream, input_tokens, output_tokens, total_tokens, reasoning_tokens, cached_tokens, cached_creation_tokens, usage_state, status, http_status, error_code, error_detail, latency_ms, ttfb_ms, request_id, route_trace, cost_total) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ID, ts, r.AccessKeyID, nullIfEmpty(r.UserID), r.PublicModel, r.ProviderID, r.UpstreamModel, r.IngressProtocol, stream, r.InputTokens, r.OutputTokens, r.TotalTokens, r.ReasoningTokens, r.CachedTokens, r.CachedCreationTokens, usageState, r.Status, r.HTTPStatus, nullIfEmpty(r.ErrorCode), nullIfEmpty(r.ErrorDetail), r.LatencyMs, r.TTFBMs, nullIfEmpty(r.RequestID), nullIfEmpty(r.RouteTrace), cost)
	if err != nil {
		// 落库失败：费用随记录一起作废。返回 0 而不是 cost —— 调用方若误用
		// 这个值去扣费，扣的金额必须与「库里真实存在的记录」对应，
		// 而失败的记录根本不存在。
		return 0, err
	}
	return cost, nil
}

// CreateUsageRecordsBatched 批量落库：**一个事务内一条多值 INSERT**，返回每条固化的费用。
//
// # 为什么需要它（性能）
//
// 单条落库的实测成本是 **296µs/行**（batch 探针实测）。
// 拆开看，这 296µs 里绝大部分是**每次独立事务的固定开销**：
// 把 N 行并进一个事务后，bs=50 时降到 **39µs/行（7.6×）**。
// 单写连接（SetMaxOpenConns(1)）下每个成功请求都要付一次这个固定开销，
// 于是网关吞吐被钉在落库速率上（实测 3693 req/s，仅 INSERT）。
//
// # 关键约束：**每条的费用仍然由 freezeUsageCost 单独算**
//
// 不是「先插入再统一计价」，也不是「算一次总价」—— 定价依赖每条记录的
// provider_id / upstream_model / token 数，必须逐条算。批量只省掉
// 事务与语句的**提交**开销，不改变任何计价语义。
//
// 这保证「扣的 = 报表的」这条不变量**在批量下逐字不变**：返回的第 i 个
// 费用就是第 i 条写进 cost_total 列的那个值，与单条路径同源。
//
// # 返回值与失败语义（调用方必须照此处理）
//
// 返回 costs[i] 是第 i 条固化的费用（失败则为 0）。
// 事务是**全有或全无**：任一行 INSERT 失败（主键冲突等）整个事务回滚，
// 此时**所有** costs 都返回 0，调用方必须把整批当作「全部未落库」处理
// —— 包括**不要**为任何一条扣费。这与单条路径的
// 「落库失败 → 返回 (0, err) → 不扣费」完全一致。
func (s *Store) CreateUsageRecordsBatched(ctx context.Context, recs []*UsageRecord) ([]float64, error) {
	if len(recs) == 0 {
		return nil, nil
	}
	// 单条直接走原路径：批量构造（拼 VALUES、分配 args）反而比一次
	// Exec 慢，且引入一条需要单独维护的代码路径毫无收益。
	if len(recs) == 1 {
		cost, err := s.CreateUsageRecordWithCost(ctx, recs[0])
		if err != nil {
			return []float64{0}, err
		}
		return []float64{cost}, nil
	}

	// 计价在事务外做：freezeUsageCost 只读数据库（查价），
	// 放到事务内会延长持写锁时间，而查价走的是读池（WAL 下不阻塞）。
	costs, costPending := s.batchComputeCosts(recs)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return make([]float64, len(recs)), err
	}
	defer func() { _ = tx.Rollback() }() //nolint:errcheck // 已提交时是 no-op

	var (
		sb   strings.Builder
		args = make([]any, 0, len(recs)*usageInsertColumns)
	)
	sb.WriteString(`INSERT INTO usage_records
		(id, ts, access_key_id, user_id, public_model, provider_id, upstream_model,
		 ingress_protocol, stream, input_tokens, output_tokens, total_tokens,
		 reasoning_tokens, cached_tokens, cached_creation_tokens, usage_state, status, http_status,
		 error_code, error_detail, latency_ms, ttfb_ms, request_id, route_trace,
		 cost_total) VALUES `)

	for i, r := range recs {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString("(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)")
		args = append(args, usageInsertArgs(r, costs[i], costPending[i])...)
	}

	if _, err := tx.ExecContext(ctx, sb.String(), args...); err != nil {
		// 全批回滚：返回全 0，调用方不得为任何一条扣费。
		return make([]float64, len(recs)), err
	}
	if err := tx.Commit(); err != nil {
		return make([]float64, len(recs)), err
	}
	return costs, nil
}

// usageInsertColumns 是 usage_records 写入路径的列数（仅用于容量预估）。
const usageInsertColumns = 25

// batchComputeCosts 逐条算固化费用，并顺带完成 token 负值归一与 ts 兜底。
//
// 归一必须在这里做（而不能只在单条路径做）：批量路径也是写入口，
// 绕过归一会让负 token 经触发器永久拉低终身累计（见
// CreateUsageRecordWithCost 的注释）。
//
// 第二个返回值逐条标记「查价失败、需随 INSERT 一起写成 cost_pending」。
func (s *Store) batchComputeCosts(recs []*UsageRecord) ([]float64, []bool) {
	now := time.Now().UnixMilli()
	costs := make([]float64, len(recs))
	pending := make([]bool, len(recs))
	for i, r := range recs {
		if r.InputTokens < 0 {
			r.InputTokens = 0
		}
		if r.OutputTokens < 0 {
			r.OutputTokens = 0
		}
		if r.TotalTokens < 0 {
			r.TotalTokens = 0
		}
		if r.ReasoningTokens < 0 {
			r.ReasoningTokens = 0
		}
		if r.CachedTokens < 0 {
			r.CachedTokens = 0
		}
		if r.CachedCreationTokens < 0 {
			r.CachedCreationTokens = 0
		}
		if r.Ts == 0 {
			r.Ts = now
		}
		cost, p := s.freezeUsageCost(context.Background(), r)
		costs[i] = cost
		pending[i] = p
	}
	return costs, pending
}

// usageInsertArgs 把一条记录摊成 INSERT 的绑定参数（顺序与列清单一致）。
//
// costPending 为真时把 usage_state 改写为 cost_pending：待重算标记必须与记录
// 同时落库，单独事后 UPDATE 会命中 0 行（那时行还不存在），于是该笔用量
// 永久免单。
func usageInsertArgs(r *UsageRecord, cost float64, costPending bool) []any {
	stream := 0
	if r.Stream {
		stream = 1
	}
	usageState := r.UsageState
	if costPending {
		usageState = usageStateCostPending
	}
	return []any{
		r.ID, r.Ts, r.AccessKeyID, nullIfEmpty(r.UserID), r.PublicModel,
		r.ProviderID, r.UpstreamModel, r.IngressProtocol, stream,
		r.InputTokens, r.OutputTokens, r.TotalTokens,
		r.ReasoningTokens, r.CachedTokens, r.CachedCreationTokens, usageState, r.Status,
		r.HTTPStatus, nullIfEmpty(r.ErrorCode), nullIfEmpty(r.ErrorDetail),
		r.LatencyMs, r.TTFBMs, nullIfEmpty(r.RequestID), nullIfEmpty(r.RouteTrace), cost,
	}
}

// priceUsage 是包内别名，保留历史调用点与测试（见 pricing.go 的 PriceUsage）。
//
// 真正的定义在 pricing.go —— 全仓唯一，且现在**同时**被余额预检
// （cmd/gateway/billing.go）与落库实扣共用，消除了审计 A4-P1-3 指出的
// 两份公式漂移（只配 price_cache_hit 的模型预检 fail-open 漏放行）。
func priceUsage(priceInput, priceCacheHit, priceCacheCreation, priceOutput, inputTokens, cachedTokens, cachedCreationTokens, outputTokens float64) float64 {
	return PriceUsage(priceInput, priceCacheHit, priceCacheCreation, priceOutput, inputTokens, cachedTokens, cachedCreationTokens, outputTokens)
}

// usageStateCostPending 是 cost_total 因查价失败而**待重算**的标记
// （2026-10-12 修 P1-2）。
//
// # 为什么需要一个可查询的状态，而不只是一条 WARN
//
// 原来 freezeUsageCost 查价失败就记 0 并打 WARN，然后**什么都没有**：
//   - cost_total 是固化字段，之后所有报表一律读它、不再按当前价重算；
//   - main.go 的 charge 用同一个 cost 扣费 → 费用 0 → **该请求完全免单**；
//   - RecomputeCost 在生产代码中**没有任何调用点**（纯手工接口）。
//
// 于是一次读池抖动就永久留下一笔免单，且没有任何落盘线索能事后发现
// （报表上它就是一条正常的 0 元记录）。WARN 日志会被日志轮转冲掉。
//
// 加上这个状态后，「哪些记录待重算」变成一条**可查询**的事实：
// 后台任务能周期性地把它们捞出来重算（见 RecomputePendingCosts），
// 而不必依赖人工记得去调某个接口。
//
// 与 reported / missing 的区别：那两者描述的是**上游有没有报 token 数**，
// 本值描述的是「我们有没有算出费用」—— 两个正交维度，不冲突。
const usageStateCostPending = UsageStateCostPending

// freezeUsageCost 算出一条用量的固化费用（元）。
//
// **只负责查价 + 记录待重算标记**，算钱一律委托 priceUsage
// （见那里的「只有一份公式」）。
//
// **查价失败不返回错误、不阻断落库**：用量是硬需求（配额、限速、审计都靠它），
// 费用不是。模型不存在、没配价、读池抖动，后果都只是这一条记录计 0 元 ——
// 与「未配价 = 0」的既有口径同语义。让费用查询有能力打死用量落库，
// 是把 nice-to-have 接到了 critical path 上。
//
// 但**绝不能只是打个 WARN 就完事**（2026-10-12 修 P1-2）：那会让一次读池
// 抖动永久变成一笔免单，且事后无从发现。所以失败时把 usage_state 标成
// usageStateCostPending，让后台的 RecomputePendingCosts 能把它捞回来重算。
//
// 第二个返回值 pending 为真时，调用方**必须**把 usage_state 一并写成
// cost_pending（随 INSERT 落库）。这个标记与「记录已存在」必须同时成立，
// 否则待重算队列与待重算记录会永久错开。
func (s *Store) freezeUsageCost(ctx context.Context, r *UsageRecord) (float64, bool) {
	var pin, phit, pcreate, pout float64
	err := s.read.QueryRowContext(ctx,
		`SELECT COALESCE(price_input, 0), COALESCE(price_cache_hit, 0),
		        COALESCE(price_cache_creation, 0), COALESCE(price_output, 0)
		   FROM upstream_models WHERE provider_id = ? AND model_id = ?`,
		r.ProviderID, r.UpstreamModel).Scan(&pin, &phit, &pcreate, &pout)
	if err != nil {
		// sql.ErrNoRows 是**正常业务状态**（模型已删除 / 从没配过价），
		// 与「未配价 = 0」同语义，不需要重算 —— 重算多少次都是 0。
		// 其余错误（读池抖动、磁盘故障）才是真正的「暂时算不出来」，
		// 必须留待重算，否则就是永久免单。
		if !errors.Is(err, sql.ErrNoRows) {
			// WARN 保留：它给出具体的 request_id，是排障的第一线索。
			s.logger.Warn("freeze usage cost: price lookup failed, marked cost_pending",
				"provider_id", r.ProviderID, "upstream_model", r.UpstreamModel,
				"request_id", r.RequestID, "error", err)
			return 0, true
		}
		return 0, false
	}
	return priceUsage(pin, phit, pcreate, pout,
		float64(r.InputTokens), float64(r.CachedTokens), float64(r.CachedCreationTokens), float64(r.OutputTokens)), false
}

// RecomputePendingCosts 重算所有 usage_state='cost_pending' 的记录（2026-10-12 修 P1-2）。
//
// 这是 freezeUsageCost 标记出来的「待重算」队列的消费者。修好之后，
// 一次读池抖动造成的 0 元记录会在下一轮被自动纠正，**不再需要人工干预**。
//
// 逐条重算（复用 RecomputeCost 的逐条语义），不用一条大 UPDATE：
//   - 每条要独立查一次价，逐条才知道该不该改 —— 一次大 UPDATE 会把
//     「本来就算对了的」也重写一遍，破坏 cost_total 的「固化于落库时」语义；
//   - 精确知道改了几条，能进日志与告警（这是「有没有人在偷偷免单」的唯一可观测点）。
//
// 上界 limit 防止一次积压（长期故障后可能有大量待重算）把写池占死。
// 返回 (重算条数, 是否还有剩余)。
func (s *Store) RecomputePendingCosts(ctx context.Context, limit int) (int64, bool, error) {
	if limit <= 0 {
		limit = 500
	}
	rows, err := s.read.QueryContext(ctx,
		`SELECT id FROM usage_records WHERE usage_state = ? LIMIT ?`,
		usageStateCostPending, limit)
	if err != nil {
		return 0, false, fmt.Errorf("scan cost_pending: %w", err)
	}
	ids := make([]string, 0, limit)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, false, fmt.Errorf("scan id: %w", err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, false, fmt.Errorf("iterate cost_pending: %w", err)
	}
	if len(ids) == 0 {
		return 0, false, nil
	}

	var fixed int64
	for _, id := range ids {
		ok, rerr := s.RecomputeCost(ctx, id)
		if rerr != nil {
			// 单条失败不该中断整批：其余记录仍应被重算。
			// 留 usage_state 不变，下一轮还会捞到它。
			s.logger.Warn("recompute pending cost failed", "record_id", id, "error", rerr)
			continue
		}
		if ok {
			fixed++
		}
	}
	// 捞满 limit 就说明可能还有剩余，交给调用方下一轮继续。
	remaining := len(ids) >= limit
	return fixed, remaining, nil
}

// UnchargedUsage 是一条「已落库但**没扣到钱**」的用量记录。
//
// 这是 F4（落库与扣费之间的无对账窗口）的载体：usage 落库成功后、扣费
// 执行前进程被杀，或扣费遇到瞬时故障且重试也全部失败，这笔费用就永远
// 留在「报表上有、余额没扣」的状态 —— 且**没有任何落盘痕迹**说明它漏了。
type UnchargedUsage struct {
	RecordID  string
	UserID    string
	RequestID string
	CostYuan  float64
}

// FindUnchargedUsage 找出「已落库、有费用、但 balance_charges 里没有对应
// 扣费流水」的用量记录（2026-10-21 修 A4-P1-4 / 汇总 F4）。
//
// # 判定条件
//
//   - status = 'ok'：只有成功的请求才该扣用户钱（见 usageRecorder.charge
//     的「只对成功的请求扣费」）。失败/取消的记录 cost_total 记的是**网关的
//     上游成本**，不是用户的应收 —— 把它们算成「漏扣」会让对账补扣用户
//     根本不该付的钱。
//   - cost_total > 0：没费用就不用补扣（未配价模型恒为 0）。
//   - request_id 非空：空 request_id 没有幂等键，补扣会**真的重复扣**。
//     那类记录被排除在外的理由见 usageRecorder.charge 里同名的分支。
//   - balance_charges 里没有 (request_id, user_id)：这正是「没扣过」的证据。
//     ChargeBalance 的占位行是幂等键，扣费成功必有占位行。
//
// # 为什么用 LEFT JOIN 而不是先查再逐条比对
//
// 一次 LEFT JOIN 就能把「有费用但无流水」的点集算出来，且能走
// (request_id, user_id) 主键索引。逐条比对是 N+1 次查询 —— 在
// 高并发网关上这不是「慢一点」，是每秒几千次额外 IO。
//
// # 边界：归档表不在内
//
// 只查 usage_records（明细表）。已归档到 usage_daily_rollups 的记录
// 不在这张表里，因此不会被重复扣 —— 这是对的：归档是**报表**口径的
// 搬迁，不是「这笔钱还没收」的意思。
func (s *Store) FindUnchargedUsage(ctx context.Context, limit int) ([]UnchargedUsage, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.read.QueryContext(ctx,
		`SELECT u.id, COALESCE(u.user_id, ''), COALESCE(u.request_id, ''), u.cost_total
		   FROM usage_records u
		   LEFT JOIN balance_charges c
		     ON c.request_id = u.request_id AND c.user_id = COALESCE(u.user_id, '')
		  WHERE u.status = 'ok'
		    AND u.cost_total > 0
		    AND COALESCE(u.request_id, '') <> ''
		    AND c.request_id IS NULL
		  ORDER BY u.ts
		  LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UnchargedUsage
	for rows.Next() {
		var u UnchargedUsage
		if err := rows.Scan(&u.RecordID, &u.UserID, &u.RequestID, &u.CostYuan); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// ChargeUnchargedUsage 补扣一条「已落库但没扣到钱」的用量记录。
//
// 幂等由 balance_charges 的占位行保证：同一个 (request_id, user_id)
// 第二次调用会命中幂等快速路径，不会重复扣。所以这个方法可以安全地被
// 对账任务反复调用。
//
// 欠费（ErrInsufficientBalance）**不算失败**：用户真的没钱了。这笔
// 金额会进 balance_pending 待结算账本，用户充值后被结转（见
// ChargeBalance 的欠费分支）。这里返回 nil，让对账任务把它当成
// 「处理过了」—— 否则它会每轮都把同一笔捞出来重试，而重试永远不会
// 成功（欠费不是瞬时故障）。
func (s *Store) ChargeUnchargedUsage(ctx context.Context, u UnchargedUsage) error {
	if u.UserID == "" || u.RequestID == "" || u.CostYuan <= 0 {
		return nil
	}
	err := s.ChargeBalance(ctx, u.UserID, u.CostYuan, u.RequestID)
	if errors.Is(err, ErrInsufficientBalance) {
		return nil
	}
	return err
}

// recomputeChargeKey 生成「重算补扣」的幂等键。
//
// # 为什么不能直接用 request_id（2026-10-21 修 P0-5）
//
// 落库时的扣费已经用 request_id 占位行（balance_charges 的主键）。
// 重算补扣若用同一个 request_id，INSERT OR IGNORE 会撞上那个占位行、
// RowsAffected()==0 → 直接返回「已扣过」→ **补扣变成空操作**。
//
// 实测（修复前的探针）：落库时按 cost=0.0006 扣过（占位行已写），
// 重算把费用改成 0.1004（差额 0.0998 元），余额分文未动。
//
// # 键的形式
//
//	recompute:<record_id>:<cost_micros>
//
// 编码「目标费用」而不是「差额」，是因为它让幂等的语义可以说清楚：
//   - 对同一条记录、同一个目标费用 → 重复重算 → 同键 → 只补扣一次；
//   - 对同一条记录、不同的目标费用 → 不同键 → 补扣新的差额。
//
// 第二条超出了「重算重复执行」的最常见场景，但它是**必要**的：
// 管理员改价后，后台任务会把同一条记录再重算一次（上一轮可能因为
// 欠费而没成功、或者两轮之间改了价）。若键不纳入目标费用，
// 第二轮会命中第一轮的幂等行而不补扣 —— 那就是“改价后仍旧不收钱”。
//
// 粒度用微元（整数）而不是元：价格被保存到 1e-9 元（priceUsage 的舍入），
// 而微元粒度下的 1 微元 = 1e-6 元 才是真正意味着的最小可区分差异。
//
// record_id 里的分隔符适合性：record_id 是 32 位 hex（无冒号与细正符号），
// 故不会与前缀或分隔符冲突；而且它与普通扣费的 request_id（客户端可控）
// 不同空间，不会合并成同一行。
func recomputeChargeKey(recordID string, costYuan float64) string {
	if recordID == "" {
		// 没有记录标识 → 无法构造幂等键 → 返回空串，
		// 让 chargeBalanceTx 走「不做幂等」的分支（每次都扣）。
		// 这条路径实际不可达（RecomputeCost 按 id 查记录，查到就有 id），
		// 但它的语义比「猜一个键」安全。
		return ""
	}
	return fmt.Sprintf("recompute:%s:%d", recordID, YuanToMicros(costYuan))
}

// RecomputeCost 按**当前**单价重算某条用量的固化费用。
//
// 用途：freezeUsageCost 因读池抖动失败而记 0 时，事后补救。它是唯一一处
// 允许用「当前价」覆盖已固化费用的地方 —— 因为原值本就是错的（0），
// 不覆盖会把错误永久保留。
//
// 按ID 精确重算，不做批量：逐条处理才知道该不该改，不必引入「重算区间」
// 这种会误伤正常固化值的批量语义。
//
// 成功后把 usage_state 复位成 'reported'（2026-10-12 修 P1-2）：
// freezeUsageCost 失败时把它标成 cost_pending，而 RecomputePendingCosts
// 是按这个状态捞记录的 —— 不复位的话同一条会被无限次捞出、每轮白跑一次，
// 且「待重算队列非空」这个信号永远不消失（运维会误以为还在积压）。
//
// 注意：若重算后费用**仍是 0**（模型确实没配价 / 已删除），也照样复位 ——
// 那时 0 是正确值而非失败，留着 pending 只会造成无意义的重复扫描。
func (s *Store) RecomputeCost(ctx context.Context, recordID string) (bool, error) {
	// 重算需要知道这条记录属于谁、request_id 是什么 —— 补扣余额要用。
	var r UsageRecord
	err := s.read.QueryRowContext(ctx,
		`SELECT COALESCE(provider_id, ''), COALESCE(upstream_model, ''),
		        COALESCE(input_tokens, 0), COALESCE(cached_tokens, 0),
		        COALESCE(cached_creation_tokens, 0), COALESCE(output_tokens, 0),
		        COALESCE(user_id, ''), COALESCE(request_id, ''), COALESCE(cost_total, 0)
		   FROM usage_records WHERE id = ?`, recordID).
		Scan(&r.ProviderID, &r.UpstreamModel, &r.InputTokens, &r.CachedTokens, &r.CachedCreationTokens, &r.OutputTokens,
			&r.UserID, &r.RequestID, &r.PreviousCost)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	cost, costPending := s.freezeUsageCost(ctx, &r)
	// 重算成功即复位为 reported；查价仍然失败则**保持** cost_pending，
	// 让下一轮继续捞它（旧实现无条件写 reported，于是重算失败的那次
	// 把这条记录永久移出待重算队列，变成一笔没被纠正的 0 元）。
	state := UsageStateReported
	if costPending {
		state = usageStateCostPending
	}

	// # 补扣余额（2026-10-21 修 P0-5）
	//
	// 此前这里只 UPDATE 了 cost_total，**从不碰余额** —— 而落库时扣费走的是
	// CreateUsageRecordWithCost 返回的那个 cost（落库时是 0），于是
	// 「一次读池抖动」= 该请求**永久免单**：报表 0 元、余额不动、无 ERROR。
	//
	// 补扣的金额是**差额**（重算值 - 原值），不是重算值：原值已经扣过一次
	// （或本该扣但没扣成）。用差额才能与「流水里已记的金额」对得上。
	//
	// 幂等由 chargeBalanceTx 的占位行保证：它以 (request_id, user_id)
	// 为主键，第二次补扣会命中「已扣过」而直接返回，不会重复扣。
	//
	// 为什么不能调 ChargeBalance（它开自己的事务）：那会让「明细改写」
	// 与「余额扣减」落在两个事务里 —— 又一处「明细说扣了、余额没扣」的可能。
	// 必须在**同一个事务**里完成。
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }() //nolint:errcheck // 已提交时是 no-op

	res, err := tx.ExecContext(ctx,
		`UPDATE usage_records SET cost_total = ?, usage_state = ? WHERE id = ?`,
		cost, state, recordID)
	if err != nil {
		return false, err
	}
	n, aerr := res.RowsAffected()
	if aerr != nil {
		return false, nil //nolint:nilerr // 无法确认影响行数时不谎报成功
	}
	if n == 0 {
		// 记录已被剪枝（明细不在了）。表 B 的累计已由归档维护，无需补。
		return false, tx.Commit()
	}

	// 补扣差额。
	//
	// 欠费不是错误：用户真的没钱了。欠费时**整体回滚**（明细的 cost_total
	// 保持原值），让这条记录留在 cost_pending 队列里，下次重算再试 ——
	// 而它会一直重试到用户充值。这正是「欠费不硬扣」口径的落地：
	// 收不到就记着，不假装收到了。
	//
	// 回滚由本函数做（defer 兜底）：chargeBalanceTx 不回滚 —— 那会把上面
	// 那次 UPDATE 一起撤掉，明细就又回到「0 元待重算」的死循环。
	//
	// user_id 为空 = 无归属的历史记录：没有可扣的账户，补扣无从谈起，
	// 但明细与累计表仍要更新（它们是**报表**口径，与余额无关）。
	insufficient := false
	if delta := cost - r.PreviousCost; delta > 0 && r.UserID != "" {
		// 幂等键用**重算专属的键**，不用 request_id（见 recomputeChargeKey）。
		if _, cerr := s.chargeBalanceTx(ctx, tx, r.UserID, delta,
			recomputeChargeKey(recordID, cost)); cerr != nil {
			switch {
			case errors.Is(cerr, ErrInsufficientBalance):
				insufficient = true
			case errors.Is(cerr, ErrNotFound):
				// 用户不存在（无归属的历史记录，或用户已被删）。
				//
				// 这**不该**阻塞重算：明细的 cost_total 与累计表是
				// **报表**口径，与余额无关。没有可扣的账户时，正确做法是
				// 把报表补对、记一条 WARN，而不是让这条记录永远停在
				// cost_pending —— 那会让待重算队列非空这个信号永不消失。
				s.logger.Warn("recompute cost: user not found, cannot charge balance",
					"record_id", recordID, "user_id", r.UserID, "delta", delta)
			default:
				return false, cerr
			}
		}
	}
	if insufficient {
		// 回滚，让这条记录留在待重算队列里。
		// （defer 里的 tx.Rollback() 兜底，这里不重复调用。）
		s.logger.Warn("recompute cost: insufficient balance, keeping record pending",
			"record_id", recordID, "user_id", r.UserID,
			"delta", cost-r.PreviousCost, "request_id", r.RequestID)
		return false, nil
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

type UsageStats struct {
	TotalRequests int64
	SuccessCount  int64
	CanceledCount int64
	TotalTokens   int64
	InputTokens   int64
	OutputTokens  int64
	CachedTokens  int64
	// CachedCreationTokens 是写入缓存的输入 token 累计（Anthropic
	// cache_creation_input_tokens）。它已含在 InputTokens 内，单独成列是为了
	// 让「命中率」与「写入量」可以分开看：命中率的分母仍是全部输入
	// （含写入），而写入量本身是成本分析的另一维。
	CachedCreationTokens int64
	ErrorCount           int64
	// CacheHitRate 是缓存命中率（0~1）：cached_tokens / input_tokens。
	// 分母用 input_tokens 而非 total_tokens —— 缓存命中衡量的是输入侧，
	// 输出 token 与缓存无关，计入分母只会稀释指标。分子分母的语义前提：
	// 两个上游协议的 cached 都 ⊆ input（chat 的
	// prompt_tokens_details.cached_tokens、anthropic 被 SDK 折进 input 的
	// cache_read/cache_creation），故比值恒 ≤ 1。并入本查询是为了省掉对
	// 同一区间的第二次全扫（此前的 stats 页要为它单独再扫一遍 usage_records）。
	CacheHitRate float64
	// Cost 是费用（元），读 usage_records.cost_total —— 每条记录落库**当时**
	// 按单价算好并固化的金额（见 freezeUsageCost）。
	//
	// P1.5 之前这里 JOIN upstream_models 按当前价重算，于是「管理员改个价，
	// 昨天的报表跟着变」：对账时两个数字互相对不上，且没人能说出是哪次改价
	// 影响的。固化后历史金额不可变，改价只影响此后的记录。
	// 未配价的模型贡献 0（与固化前口径一致）。
	Cost float64
	// CostPendingCount 是「费用还是 0、等着被重算」的记录数
	// （2026-10-21 修 A6-P1-4 / 汇总 M5）。
	//
	// 为什么需要它成为**可查的事实**：这类记录的费用是 0，报表上它就是
	// 一条正常的 0 元记录 —— 与「本来免费」完全同形，事后无从分辨。
	// 而剪枝会把明细搬进归档表，待重算的标记跟着明细一起消失（M5），
	// 于是连「曾经有多少条待重算」都问不出来。归档层现在也累加这个计数
	// （usage_daily_rollups.cost_pending_count），所以剪枝前后这个数
	// 都能查到 —— 它的存在本身就是「系统里有免单风险」的信号。
	CostPendingCount int64
	// LastSuccessAt 是所选区间内最近一次成功请求的时间（毫秒）；0 = 无成功请求。
	// 与 ErrorCount 同源，供总览的“需要处理”直接判断服务是否真的跑通过。
	LastSuccessAt int64
	// FirstRecordAt 是所选区间内最早已有记录的时间（毫秒）；0 = 区间无记录。
	FirstRecordAt int64
	// TTFBMs / TtfbSamples 是**所选区间**内成功流式请求的首字延迟：
	// 分子是成功请求 ttfb 的累加值，分母是真正测到首字的成功请求数。
	// 它与 RecentPerformance 的“最近 N 条”口径不同，不能把两者混用。
	TtfbMs      float64
	TtfbSamples int64
	// TokensPerSec / ThroughputSamples 是**所选区间**内成功请求的输出速度：
	// 用输出 token 总和 / 成功请求耗时总和加权，避免单次快慢请求带偏。
	TokensPerSec      float64
	ThroughputSamples int64
}

// GetUsageStats 返回累计统计。from/to 为毫秒时间戳，0 表示不设该边界
// （from=0 即统计全部历史）。ErrorCount 排除 canceled：那是「客户端主动断开」，
// 既不是上游故障也不是本网关的失败，算进错误率只会让成功率虚低。
// 仪表盘需要精确成功率的，请用 SuccessCount + ErrorCount 作分母；
// CanceledCount 单独返回，不能把它算进成功或失败。
//
// scopeUserID 非空时只统计该用户的用量（多用户改造的作用域收窄）。
// 空串 = 全局（admin 视角）。**这个参数不能省略** —— 它由
// admin.callerScope 提供，是普通用户看不到全局数据的唯一保障。
//
// 费用口径：SUM(cost_total)，即落库时固化的金额（元）。单价规则见
// freezeUsageCost —— 不在这里重算，改价不回溯历史。
//
// P1.5 起**不再 JOIN upstream_models**：除了上面说的回溯问题，JOIN 还让每次
// 统计都多扫一遍模型表。缓存命中率原本依赖「JOIN 至多 1:1 才不会因行复制
// 失真」这个前提（UNIQUE(provider_id, model_id) 保证），去掉 JOIN 后前提
// 自然消失，口径更稳。记录的模型已被删掉时费用仍是当初算好的那个数，
// 不会因为关联不上而归零 —— 这正是固化想要的行为。
//
// 数据源是 UsageSource（明细 ∪ 日归档）：只查明细的话，30 天后每次剪枝
// 都会让这个数字**自动变小**，总览「全部」档就成了数据丢失而非优化。
func (s *Store) GetUsageStats(ctx context.Context, from, to int64, scopeUserID string) (*UsageStats, error) {
	var stats UsageStats
	src, args := UsageSource(UsageFilter{From: from, To: to, UserID: scopeUserID})
	err := s.read.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(u.n), 0), `+UsageSumExpr("total_tokens", "u")+`,
		        `+UsageSumExpr("input_tokens", "u")+`, `+UsageSumExpr("output_tokens", "u")+`,
		        `+UsageSumExpr("cached_tokens", "u")+`,
		        `+UsageSumExpr("cached_creation_tokens", "u")+`,
		        COALESCE(SUM(u.n_err), 0),
		        COALESCE(SUM(u.cached_tokens) * 1.0 / NULLIF(SUM(u.input_tokens), 0), 0),
		        `+UsageSumExpr("cost_total", "u")+`,
		        `+UsageSumExpr("n_cost_pending", "u")+`,
		        COALESCE(SUM(u.n_ok), 0),
		        COALESCE(SUM(u.n), 0) - COALESCE(SUM(u.n_ok), 0) - COALESCE(SUM(u.n_err), 0),
		        COALESCE(SUM(u.ttfb_ok_sum) * 1.0 / NULLIF(SUM(u.n_ttfb_ok), 0), 0),
		        COALESCE(SUM(u.n_ttfb_ok), 0),
		        COALESCE(SUM(CASE WHEN u.out_ok > 0 AND u.lat_ok > 0 THEN u.out_ok ELSE 0 END) * 1000.0
		                 / NULLIF(SUM(CASE WHEN u.out_ok > 0 AND u.lat_ok > 0 THEN u.lat_ok ELSE 0 END), 0), 0),
		        COALESCE(SUM(u.n_ok_speed), 0),
		        CASE WHEN COALESCE(SUM(u.n_ok), 0) > 0
		             THEN COALESCE(MAX(CASE WHEN u.n_ok > 0 THEN u.bucket_ts END), 0)
		             ELSE 0 END,
		        COALESCE(MIN(CASE WHEN u.n > 0 THEN u.bucket_ts END), 0)
		   FROM `+src,
		args...).
		Scan(&stats.TotalRequests, &stats.TotalTokens, &stats.InputTokens, &stats.OutputTokens, &stats.CachedTokens, &stats.CachedCreationTokens, &stats.ErrorCount, &stats.CacheHitRate, &stats.Cost, &stats.CostPendingCount,
			&stats.SuccessCount, &stats.CanceledCount, &stats.TtfbMs, &stats.TtfbSamples, &stats.TokensPerSec, &stats.ThroughputSamples,
			&stats.LastSuccessAt, &stats.FirstRecordAt)
	if err != nil {
		return nil, err
	}
	return &stats, nil
}

// timeRangeClause 构造 ts 时间过滤的 WHERE 子句。from/to 为毫秒时间戳，
// 0 表示不设该边界。prefix 是 ts 列的限定前缀（如 "u."，JOIN 时用来消歧），
// 返回的 args 与 where 配套使用。
func timeRangeClause(from, to int64, prefix string) (string, []any) {
	var conds []string
	var args []any
	if from > 0 {
		conds = append(conds, prefix+"ts >= ?")
		args = append(args, from)
	}
	if to > 0 {
		conds = append(conds, prefix+"ts <= ?")
		args = append(args, to)
	}
	if len(conds) == 0 {
		return "", nil
	}
	return " WHERE " + strings.Join(conds, " AND "), args
}

// GetRecentThroughput 返回最近若干次成功调用的平均输出速度（token/s）。
// 用聚合口径（输出 token 之和 / 延迟之和），比逐条速率平均更稳，不会被单次快慢请求带偏。
// 只统计 status='ok' 且确有输出、确有耗时的记录；不足样本时返回 0。
//
// scopeUserID 非空时只看该用户的最近记录（作用域收窄，用途同 GetUsageStats）。
func (s *Store) GetRecentThroughput(ctx context.Context, limit int, scopeUserID string) (float64, error) {
	if limit <= 0 {
		limit = 50
	}
	userCond, args := scopeClause("", scopeUserID)
	var v float64
	err := s.read.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(output_tokens) * 1000.0 / NULLIF(SUM(latency_ms), 0), 0)
		   FROM (SELECT output_tokens, latency_ms FROM usage_records
		          WHERE status = 'ok' AND output_tokens > 0 AND latency_ms > 0`+userCond+`
		          ORDER BY ts DESC LIMIT ?)`, append(args, limit)...).
		Scan(&v)
	if err != nil {
		return 0, err
	}
	return v, nil
}

// GetRecentTtfbMs 返回最近若干次成功调用的平均首字延迟（毫秒）。
// 只统计 status='ok' 且 ttfb_ms>0（真正流式、拿到过首字）的记录，取算术平均；
// 无样本时返回 0。与 GetRecentThroughput 口径一致（同一「近 N 次」窗口）。
func (s *Store) GetRecentTtfbMs(ctx context.Context, limit int, scopeUserID string) (float64, error) {
	if limit <= 0 {
		limit = 5
	}
	userCond, args := scopeClause("", scopeUserID)
	var v float64
	err := s.read.QueryRowContext(ctx,
		`SELECT COALESCE(AVG(ttfb_ms), 0)
		   FROM (SELECT ttfb_ms FROM usage_records
		          WHERE status = 'ok' AND ttfb_ms > 0`+userCond+`
		          ORDER BY ts DESC LIMIT ?)`, append(args, limit)...).
		Scan(&v)
	if err != nil {
		return 0, err
	}
	return v, nil
}

// scopeClause 构造「按用户收窄」的 AND 条件片段（含前导 AND）。
//
// prefix 是列限定前缀（"u." 或空）。scopeUserID 为空时返回空串 ——
// 那是 admin 语义。**调用方必须确保这个空值来自 callerScope 而非漏传**。
func scopeClause(prefix, scopeUserID string) (string, []any) {
	if scopeUserID == "" {
		return "", nil
	}
	return " AND " + prefix + "user_id = ?", []any{scopeUserID}
}

// ModelStat 是单个上游模型的运行时统计（只来自历史 usage_records，不做探测）。
type ModelStat struct {
	Tps           float64 // 近 5 次可测调用的平均输出速度（token/s）
	TtfbMs        float64 // 近 5 次可测调用的平均首字延迟（毫秒）
	SuccessRate   float64 // 近 100 次调用的成功率（0~1）
	SuccessSample int     // 成功率样本量（≤100）；>0 说明该模型被调用过
}

// throughputWindow 界定「近期表现」类统计（模型速度/成功率/TTFB）的回看窗口。
const throughputWindow = 30 * 24 * time.Hour

// ListModelThroughput 返回某上游下每个 model_id 的近期速度（近 5 次）与成功率（近 100 次）。
// 用窗口函数按模型分区取最近 N 条：速度只统计成功且有输出/耗时的记录，
// 成功率 = 最近 100 次里 status='ok' 的占比。未调用过的模型不出现在结果里。
//
// 统计范围限定近 throughputWindow：窗口函数要对该 provider 的全部历史排序，
// 调用量大的 provider 积累几十万行后，挂在本查询上的
// GET /providers/{id}/models 每次打开模型页都会全表扫一遍 —— 而它持着唯一的
// DB 连接（SetMaxOpenConns(1)），会直接阻塞配额预检与用量写入。
// 「近期表现」本来就只关心最近的数据，超窗的老记录没有统计价值。
func (s *Store) ListModelThroughput(ctx context.Context, providerID string) (map[string]ModelStat, error) {
	cutoff := time.Now().Add(-throughputWindow).UnixMilli()
	rows, err := s.read.QueryContext(ctx,
		`WITH base AS (
		   SELECT upstream_model, ts, output_tokens, latency_ms, ttfb_ms,
		          CASE WHEN status = 'ok' THEN 1 ELSE 0 END AS is_ok,
		          CASE WHEN status = 'ok' AND output_tokens > 0 AND latency_ms > 0 THEN 1 ELSE 0 END AS is_meas,
		          CASE WHEN status = 'ok' AND ttfb_ms > 0 THEN 1 ELSE 0 END AS is_ttfb
		     FROM usage_records
		    WHERE provider_id = ? AND status <> 'canceled' AND ts >= ?
		 ),
		 ranked AS (
		   SELECT upstream_model, output_tokens, latency_ms, ttfb_ms, is_ok, is_meas, is_ttfb,
		          ROW_NUMBER() OVER (PARTITION BY upstream_model ORDER BY ts DESC) AS rn_all,
		          ROW_NUMBER() OVER (PARTITION BY upstream_model, is_meas ORDER BY ts DESC) AS rn_meas,
		          ROW_NUMBER() OVER (PARTITION BY upstream_model, is_ttfb ORDER BY ts DESC) AS rn_ttfb
		     FROM base
		 )
		 SELECT upstream_model,
		        COALESCE(SUM(CASE WHEN is_meas = 1 AND rn_meas <= 5 THEN output_tokens END) * 1000.0
		                 / NULLIF(SUM(CASE WHEN is_meas = 1 AND rn_meas <= 5 THEN latency_ms END), 0), 0) AS tps,
		        COALESCE(SUM(CASE WHEN rn_all <= 100 AND is_ok = 1 THEN 1 ELSE 0 END) * 1.0
		                 / NULLIF(SUM(CASE WHEN rn_all <= 100 THEN 1 ELSE 0 END), 0), 1) AS success_rate,
		        SUM(CASE WHEN rn_all <= 100 THEN 1 ELSE 0 END) AS sample_n,
		        COALESCE(AVG(CASE WHEN is_ttfb = 1 AND rn_ttfb <= 5 THEN ttfb_ms END), 0) AS ttfb_ms
		   FROM ranked
		  GROUP BY upstream_model`, providerID, cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[string]ModelStat)
	for rows.Next() {
		var model string
		var st ModelStat
		if err := rows.Scan(&model, &st.Tps, &st.SuccessRate, &st.SuccessSample, &st.TtfbMs); err != nil {
			return nil, err
		}
		out[model] = st
	}
	return out, rows.Err()
}

// ---- 额度/费用查询接口（OpenAI usage/costs 与 dashboard billing 的数据源）----

// BucketUsage 是一个时间桶内的聚合用量（官方 /v1/organization/usage 形状的数据源）。
type BucketUsage struct {
	BucketStart  int64 // Unix 秒，桶起点
	InputTokens  int64
	CachedTokens int64
	OutputTokens int64
	Requests     int64
}

// SumUsageBuckets 把 [from,to]（毫秒）内的用量按 bucketSec 秒宽分桶聚合（全组织口径）。
//
// 数据源是 UsageSource（明细 ∪ 日归档）：官方 /v1/organization/usage 是客户端
// 的对账数据源，剪枝后只查明细会让历史区间静默少报。
//
// ⚠️ 归档的分辨率是**一天**：bucketSec=1h 时，归档的一天会被整个记到该天
// 起始的那一个小时。小时级 + 归档区间是近似（区间合计仍相等）。
// 设计 §4.8 把「改日界」明确划到范围外，故保留原桶宽。
func (s *Store) SumUsageBuckets(ctx context.Context, from, to, bucketSec int64) ([]BucketUsage, error) {
	return s.SumUsageBucketsScoped(ctx, "", from, to, bucketSec)
}

// SumUsageBucketsScoped 同 SumUsageBuckets，但可按 userID 收窄。
//
// userID 空串 = 不限制（调用方必须自行确保该空值来自服务端判定的管理员
// 身份，绝不能来自请求参数）。非空时只统计该用户的用量 —— 这是多用户部署
// 下「org-wide 端点不能泄露别的租户」的实现点。
func (s *Store) SumUsageBucketsScoped(ctx context.Context, userID string, from, to, bucketSec int64) ([]BucketUsage, error) {
	if bucketSec <= 0 {
		bucketSec = 3600 // SQLite 的 x/0 返回 NULL 而不报错，扫 int64 时才炸；这里直接兜底
	}
	src, args := UsageSource(UsageFilter{From: from, To: to, UserID: userID})
	args = append([]any{bucketSec * 1000, bucketSec * 1000}, args...)
	rows, err := s.read.QueryContext(ctx,
		`SELECT (u.bucket_ts / ?) * ? / 1000 AS bucket, `+UsageSumExpr("input_tokens", "u")+`,
		        `+UsageSumExpr("cached_tokens", "u")+`, `+UsageSumExpr("output_tokens", "u")+`,
		        COALESCE(SUM(u.n), 0)
		   FROM `+src+`
		  GROUP BY bucket ORDER BY bucket`,
		args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]BucketUsage, 0)
	for rows.Next() {
		var b BucketUsage
		if err := rows.Scan(&b.BucketStart, &b.InputTokens, &b.CachedTokens, &b.OutputTokens, &b.Requests); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// BucketCost 是一个时间桶内单个模型的估算费用（官方 /v1/organization/costs 的数据源）。
type BucketCost struct {
	BucketStart int64
	Model       string
	Cost        float64 // 元（落库时固化的 cost_total，口径同 GetUsageStats）
}

// SumCostBuckets 把 [from,to]（毫秒）内的费用按 bucketSec 秒宽、按模型分桶。
// 口径与 GetUsageStats 完全一致：读固化的 cost_total，不 JOIN 单价表
// （改价不回溯历史，见 UsageStats.Cost 的注释），并同样合并日归档
// —— 这个端点是客户端账单对账的入口，剪枝后少报就是账单对不上。
func (s *Store) SumCostBuckets(ctx context.Context, from, to, bucketSec int64) ([]BucketCost, error) {
	return s.SumCostBucketsScoped(ctx, "", from, to, bucketSec)
}

// SumCostBucketsScoped 同 SumCostBuckets，但可按 userID 收窄。
// 空 userID = 不限制（仅限服务端判定的管理员身份）。
func (s *Store) SumCostBucketsScoped(ctx context.Context, userID string, from, to, bucketSec int64) ([]BucketCost, error) {
	if bucketSec <= 0 {
		bucketSec = 3600 // 同 SumUsageBuckets：除零在 SQLite 里静默返回 NULL
	}
	src, args := UsageSource(UsageFilter{From: from, To: to, UserID: userID})
	args = append([]any{bucketSec * 1000, bucketSec * 1000}, args...)
	rows, err := s.read.QueryContext(ctx,
		`SELECT (u.bucket_ts / ?) * ? / 1000 AS bucket, u.upstream_model,
		        `+UsageSumExpr("cost_total", "u")+`
		   FROM `+src+`
		  GROUP BY bucket, u.upstream_model ORDER BY bucket`,
		args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]BucketCost, 0)
	for rows.Next() {
		var b BucketCost
		if err := rows.Scan(&b.BucketStart, &b.Model, &b.Cost); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// DayTokens 是某把 key 在一天内的 token 总量（legacy dashboard/billing/usage 的 daily_costs）。
type DayTokens struct {
	Day   string // YYYY-MM-DD（UTC）
	Total int64
}

// SumTokensForKey 统计某把 key 在 [from,to]（毫秒）内的 token 总量。
// 合并日归档：这是 legacy billing/usage 的 total_usage 来源，
// 剪枝后只查明细会让长期客户的账单凭空变少。
func (s *Store) SumTokensForKey(ctx context.Context, keyID string, from, to int64) (int64, error) {
	var v int64
	src, args := UsageSource(UsageFilter{From: from, To: to, KeyID: keyID})
	err := s.read.QueryRowContext(ctx,
		`SELECT `+UsageSumExpr("total_tokens", "u")+` FROM `+src, args...).Scan(&v)
	return v, err
}

// SumTokensByDayForKey 按天统计某把 key 的 token 总量。
//
// # 日界口径：这一条用 **UTC**，且归档支必须换算成同一套
//
// 明细侧用 date(ts/1000,'unixepoch')，是 UTC 日界。而表 A 的 day 是**本地**日界。
// 直接把两者并到一个 GROUP BY day 下，同一批用量会按两套日界分别落桶：
// UTC 与本地相差若干小时，那几小时里的请求在剪枝前后落到不同日期 ——
// 表现为**客户端账单突然少报**（这里曾经真出过：总量 69300 → 44550）。
//
// 所以归档支先把 day 还原成本地午夜的时间戳，再取它的 UTC 日期：
//
//	date(CAST(strftime('%s', day, 'utc') AS INTEGER), 'unixepoch')
//
// 与明细支完全同一套日界，剪枝前后按天结果一致。
//
// 影响面：这两个端点是 /v1/organization/* 与 legacy billing 的数据源，
// 与管理界面看到的本地日界不是同一套（设计 §4.8 把「统一日界」划到范围外）。
func (s *Store) SumTokensByDayForKey(ctx context.Context, keyID string, from, to int64) ([]DayTokens, error) {
	// 两支各自的列都要**不带前缀**地拼进 UNION：派生表本身没有名字，
	// 在里面写 `u.day` 会报 "no such column" —— 别名只在派生表外面生效。
	// 所以下面整体包一层 `... ) x` 再聚合。
	const detailPart = `SELECT date(ts / 1000, 'unixepoch') AS day, total_tokens
	                     FROM usage_records
	                    WHERE access_key_id = ? AND ts >= ? AND ts <= ?`
	// 归档支：本地 day → 本地午夜秒 → UTC 日期，与明细支对齐。
	//
	// 窗口过滤必须在这里自己下发：不能借用 UsageSource（它的 day 是**本地**日界，
	// 与这里的 UTC 日界不是同一套，借过来会把边缘那天算漏或算重）。
	//
	// 边界规则与 UsageSource 一致：**只取完整落在窗口内的天**。
	// 归档的分辨率就是一天，窗口边缘若切在某天中间，那一天的部分数据已不可得。
	// 原实现用 `day <= date(to,...)` 把边界日**整天计入**（多算），与
	// UsageSource 的「宁可少算、不越界多算」相反 —— 账单多报最多一整天。
	// 费用口径下多算会被当成错账，少算可解释，故统一取整日包含。
	//
	// ⚠️ 86400000 假定一天 24 小时。有 DST 的时区里那天可能是 23 或 25 小时，
	// dayEnd 会偏差 1 小时，只影响恰好落在这一小时内的窗口边缘。
	const rollupPart = `SELECT date(CAST(strftime('%s', day, 'utc') AS INTEGER), 'unixepoch') AS day,
	                            total_tokens
	                      FROM usage_daily_rollups
	                     WHERE access_key_id = ?
	                       AND (CAST(strftime('%s', day, 'utc') AS INTEGER) * 1000) >= ?
	                       AND (CAST(strftime('%s', day, 'utc') AS INTEGER) * 1000 + 86400000) <= ?`
	rows, err := s.read.QueryContext(ctx,
		`SELECT x.day, COALESCE(SUM(x.total_tokens), 0)
		   FROM (`+detailPart+`
		         UNION ALL `+rollupPart+`) x
		  GROUP BY x.day ORDER BY x.day`,
		keyID, from, to, keyID, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]DayTokens, 0)
	for rows.Next() {
		var d DayTokens
		if err := rows.Scan(&d.Day, &d.Total); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}
