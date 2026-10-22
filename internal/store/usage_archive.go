package store

// P1.5：用量归档（终身累计表 + 触发器 + 30 天剪枝）。见 MULTIUSER.md §4.8。
//
// 两层结构：
//   - 表 B `usage_totals`：恒定单行终身累计，由触发器在明细落库时实时累加。
//     **它才是「剪枝后累计不变」的保证** —— 与剪枝完全解耦。
//   - 表 A `usage_daily_rollups`：按维度按天归档，支撑剪枝后的趋势图与分项统计。
//
// 剪枝只动表 A 与明细表，绝不动表 B。

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// usageDailyRollupsDDL 是表 A 的建表语句。
//
// 它是 canonical schema（schema.go）的一部分。归档写入、查询归一化与建表
// 共用同一份定义，避免「建表口径」与「读写口径」各写一份后静默漂移。
const usageDailyRollupsDDL = `CREATE TABLE IF NOT EXISTS usage_daily_rollups (
	day              TEXT NOT NULL,      -- 'YYYY-MM-DD'（本地时区）
	user_id          TEXT NOT NULL DEFAULT '',
	access_key_id    TEXT NOT NULL DEFAULT '',
	public_model     TEXT NOT NULL DEFAULT '',
	-- 上游模型：/admin/api/usage/by-model 的分组键就是它（不是 public_model ——
	-- 界面看的是「哪个真模型吃掉了量」）。少了这一维，剪枝后分项统计无法复现。
	upstream_model   TEXT NOT NULL DEFAULT '',
	provider_id      TEXT NOT NULL DEFAULT '',
	ingress_protocol TEXT NOT NULL DEFAULT '',
	stream           INTEGER NOT NULL DEFAULT 0,
	request_count    INTEGER NOT NULL DEFAULT 0,
	success_count    INTEGER NOT NULL DEFAULT 0,
	-- 归档层必须能原样复现明细层的每个统计口径，否则「同一时间窗口剪枝前后
	-- 数字一致」这条验收做不到。所以除设计初稿的 token 分量外还要有
	-- error_count 与 total_tokens：前者是 stats 面板的 ErrorCount
	-- （'ok' / 'canceled' 之外都算错），后者的上报值在 Anthropic
	-- （缓存读计入 input）下不等于 input+output。
	error_count      INTEGER NOT NULL DEFAULT 0,
	input_tokens     INTEGER NOT NULL DEFAULT 0,
	output_tokens    INTEGER NOT NULL DEFAULT 0,
	cached_tokens    INTEGER NOT NULL DEFAULT 0,
	cached_creation_tokens INTEGER NOT NULL DEFAULT 0,
	reasoning_tokens INTEGER NOT NULL DEFAULT 0,
	total_tokens     INTEGER NOT NULL DEFAULT 0,
	-- 固化计价：不固化的话改价后历史费用永久算错。
	cost_total       REAL NOT NULL DEFAULT 0,
	-- 延迟按请求数加权平均所需（算术平均会被快慢请求互相抵消）。
	latency_sum_ms   INTEGER NOT NULL DEFAULT 0,
	-- 首字延迟同样按请求数加权平均所需。与 latency 分成两列而不是复用：
	-- ttfb 只在**真正流式**的请求上有值（非流式的 ttfb_ms 是 0），
	-- 混进 latency_sum_ms 会把「没有首字概念」的请求也算进分母，
	-- 平均值被一路稀释到没有参考价值。ttfb_count 记的是「测到过首字的
	-- 请求数」，正是那个分母。
	ttfb_sum_ms      INTEGER NOT NULL DEFAULT 0,
	ttfb_count       INTEGER NOT NULL DEFAULT 0,
	-- 成功请求的首字延迟累计。近期性能卡片只应该看成功样本，
	-- 而 ttfb_* 还要服务包含失败请求的分组统计，故单独保存。
	ok_ttfb_sum_ms   INTEGER NOT NULL DEFAULT 0,
	ok_ttfb_count    INTEGER NOT NULL DEFAULT 0,
	-- 成功请求的 token / 延迟累计值，单独成列而非用 success_count 去折算
	-- 总体列。速度（output_tokens / latency）只对成功请求有意义：canceled
	-- 的 latency 是「断开前跑了多久」，拿它当分母会把平均速度压低，而那个
	-- 请求根本没跑完。折算出的估算值也让这张表失去「剪枝前后同窗口同数」
	-- 的立身之本，所以宁可多存两列。
	ok_output_tokens INTEGER NOT NULL DEFAULT 0,
	ok_latency_sum_ms INTEGER NOT NULL DEFAULT 0,
	-- 同时满足「成功、正输出、正延迟」的请求数。速度样本数必须来自这里；
	-- 用 success_count 代替会把非流式/零输出的请求也算进分母。
	ok_sample_count INTEGER NOT NULL DEFAULT 0,
	-- 待重算记录数（2026-10-21 修 A6-P1-4 / 汇总 M5）。
	--
	-- 为什么归档表需要这一列：usage_records.usage_state 有 'cost_pending'
	-- 这个值（查价失败 → 费用记 0 → 等后台重算）。剪枝把明细搬进表 A 时，
	-- **待重算的标记跟着明细一起消失了** —— 而那些记录的费用是 0，
	-- 重算队列再也捞不到它们（它只扫 usage_records）。于是它们永久停留在
	-- 「0 元」：用户免单，且没有任何痕迹说明它该被纠正。
	--
	-- 这一列是那个标记在归档层的**替身**：剪枝时把待重算的条数累加进来，
	-- 报表据此能显示「有 N 条待重算」，运维也知道去查。费用本身仍归零
	-- （重算需要明细里的 token 数，聚合后的表 A 已经没有逐条信息）。
	--
	-- 为什么不把「待重算的金额」也算进来：cost_total 对这些行恒为 0，
	-- 没有可累加的金额 —— 要恢复它就得保留逐条 token，那是明细表的工作。
	cost_pending_count INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (day, user_id, access_key_id, public_model, upstream_model,
	             provider_id, ingress_protocol, stream)
)`

// dayExpr 是「本地时区的 YYYY-MM-DD」表达式，表 A 的 day 列与按天趋势共用。
// 抽成常量的理由：归档写入与查询读取必须逐字同一个表达式，否则边界那天的记录
// 会落进两个桶（或两个都不落）。
const dayExpr = `strftime('%Y-%m-%d', ts / 1000, 'unixepoch', 'localtime')`

// nowLocal 单独抽出来只为给测试留一个替换点（剪枝的切分日依赖「今天是哪天」）。
var nowLocal = time.Now

// DefaultRetentionDays 是明细保留天数（设计 §4.8 的硬需求：明细 30 天，
// 累计永久）。
//
// 这里是**唯一**的默认值来源：每日自动剪枝与手动端点都读它。
// 抄一份到别处就会出现「定时任务剪 30 天、界面显示保留 7 天」这种对不上的
// 状态 —— 而这类不一致没有任何报错。
const DefaultRetentionDays = 30

// PruneResult 是一次剪枝的影响面，回给管理端点。
// @ts-type PruneResult
type PruneResult struct {
	RollupRows    int64  `json:"rollup_rows"`        // 写进表 A 的聚合分组数
	DeletedRows   int64  `json:"deleted_rows"`       // 从明细表删掉的行数
	CutoffDay     string `json:"cutoff_day"`         // 保留窗口第一天（本地 'YYYY-MM-DD'）
	PrunedThrough string `json:"pruned_through_day"` // 归档水位：已剪到含哪一天
	Skipped       bool   `json:"skipped"`            // 水位已到位 / 参数不合理
	Reason        string `json:"reason,omitempty"`
}

// PruneOldUsage 把 keepDays 天之前的明细归档进表 A 并删除。
//
// # 为什么「聚合 + 删除」必须在同一事务
//
// 分两条自动提交语句就有窗口：聚合成功、删除失败 → 下次再聚合一遍 → 表 A 双计。
// 双计出来的数字看着完全正常，没人会发现。同事务内失败即整体回滚，
// 表 A 与明细恒等。
//
// # 幂等
//
// 谓词只有一个：`ts < cutoffTs`（本地午夜）。删完再跑，明细里已无匹配行，
// 聚合出 0 组、DELETE 影响 0 行。另外先用 pruned_through_day 挡一道：
// 同一水位内多次调用不必再扫明细。
//
// 积压超过单轮上限（maxPruneDelete）时一次剪不完：水位不推进，下一轮重新
// 圈一批继续。聚合与删除覆盖严格同一批行（见实现内「为什么必须先圈批」），
// 未删尽的行留到下一轮**首次**聚合，不存在重复计数。
//
// # 切分点用本地时区
//
// 与表 A 的 day 列、前端 GroupByDay 同一口径（那是用户在界面上看到的那一套）。
// 注意 SumTokensByDayForKey 用 UTC（/v1/organization/* 的数据源），
// 两套并存是已知且刻意的。
//
// 不动表 B：终身累计与剪枝解耦，那是它存在的唯一理由。
func (s *Store) PruneOldUsage(ctx context.Context, keepDays int) (*PruneResult, error) {
	res := &PruneResult{}

	// 参数边界：<=0 会让 cutoff 落到今天之前、当天数据也被删；
	// 超大值（手滑）等价于清空明细。宁可拒绝也不动数据。
	if keepDays <= 0 {
		res.Skipped, res.Reason = true, "keep_days 必须大于 0"
		return res, nil
	}
	if keepDays > 3650 {
		res.Skipped, res.Reason = true, "keep_days 超出合理范围（≤3650）"
		return res, nil
	}

	local := nowLocal().In(time.Local)
	cut := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, time.Local).
		AddDate(0, 0, -keepDays)
	res.CutoffDay = cut.Format("2006-01-02")
	// 水位记「已剪到的那一天（含）」= 保留窗口的前一天。
	res.PrunedThrough = cut.AddDate(0, 0, -1).Format("2006-01-02")
	cutoffTs := cut.UnixMilli()

	var cur string
	if err := s.db.QueryRowContext(ctx,
		`SELECT pruned_through_day FROM usage_totals WHERE id = 1`).Scan(&cur); err != nil {
		return nil, fmt.Errorf("read prune watermark: %w", err)
	}
	// 'YYYY-MM-DD' 定宽零填充，字典序即时间序。
	if cur != "" && cur >= res.PrunedThrough {
		res.Skipped, res.PrunedThrough, res.Reason = true, cur, "已剪到该水位，无事可做"
		return res, nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin prune: %w", err)
	}
	// 已 Commit 时 Rollback 是空操作，兜住中途 return 的每一条路径。
	defer func() { _ = tx.Rollback() }()

	// 本次最多剪多少行明细。
	//
	// 写池只有单连接，这个事务从圈批、聚合到删除全程持锁。首次剪枝（部署已久、
	// 从没剪过）可能有百万行级明细，一口气全处理会把写连接占住几十秒，期间
	// **所有**写入排队 —— 表现为网关整体卡顿，而不是「后台在忙」。
	//
	// 超出的部分留给下一次剪枝：下一次重新圈一批接着处理。水位只在本次事务内
	// 推进到 cut-1，且只在明细确实清空到 cutoff 后推进，所以中途失败不会留下
	// 「已删但未记账」的空洞 —— 圈批、聚合与删除始终同事务。
	const maxPruneDelete = 20000

	// 先把本轮要处理的 rowid 圈进临时表 prune_batch，再让聚合与删除**都以它为
	// 谓词** —— 这是「剪枝前后同一窗口数字完全相等」的全部依据：进了表 A 的行
	// 必然被删掉，没进表 A 的一行不动。
	//
	// 为什么必须先圈批，而不能像旧实现那样让聚合与删除各带一个 `ts < ?` 谓词：
	// 旧实现聚合无界（扫全部到期行）、删除有界（只删最老 maxPruneDelete 行）。
	// 积压超过上限时，本轮聚合过却没删掉的行，下一轮会被**再聚合一次**；而下面
	// 的 ON CONFLICT 是累加语义，归档数字凭空变大（实测 3 万行 × 1 token：两轮
	// 剪枝后 rollup 总数 4 万，多 33%）—— 不报错、不告警，只有对账能发现。
	// 先圈出「严格同一批行」、两边共用，这条不变式才真正成立。
	//
	// 为什么用临时表而不是把 rowid 逐个绑成 IN (?,?,…)：绑定变量数受
	// SQLITE_MAX_VARIABLE_NUMBER 硬限制（老编译默认 999，SQLite 3.32.0 起默认
	// 32766，具体值随驱动与编译选项而变），2 万个占位符要么直接超限、要么贴着
	// 上限走，每轮还要拼接巨型 SQL 文本与 args 切片。TEMP 表不受参数数限制，
	// SQL 文本恒定，`rowid IN (SELECT rid FROM …)` 由 SQLite 走主键查找高效求值。
	//
	// TEMP 表是**每连接**对象，而 sql.Tx 钉死单连接，本事务内的所有语句必然看到
	// 同一张表。开头的 DROP IF EXISTS 只防一种理论情形 —— 同一连接上曾有异常
	// 残留（TEMP 的 DDL 本身参与事务，回滚会连表一起撤销）；宁可多一句防御，
	// 也不赌「绝不可能残留」。
	if _, err := tx.ExecContext(ctx, `DROP TABLE IF EXISTS temp.prune_batch`); err != nil {
		return nil, fmt.Errorf("clear stale prune batch: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`CREATE TEMP TABLE prune_batch (rid INTEGER PRIMARY KEY)`); err != nil {
		return nil, fmt.Errorf("create prune batch: %w", err)
	}
	// 圈批：取**最老**的 maxPruneDelete 行，而不是「任意前 N 行」——
	// 保证处理的是时间上连续的一段，不会在明细里打散出许多时间碎片。
	//
	// # 按 ts 而不是 id 排序（2026-10-12 修 P2-1）
	//
	// 本处原写 `ORDER BY id`，注释称「id 含时间前缀因而与 ts 单调」——
	// 该前提**不成立**：usage_records.id 来自 cmd/gateway 的 generateID()，
	// 是 16 字节 crypto/rand 的纯 hex（32 字符），不含任何时间成分，
	// 与 ts 完全无关。也就是说旧写法圈出的是**随机散布**的 2 万行。
	//
	// 正确性不受影响：聚合与删除共用同一批 rowid、同事务、同谓词，
	// 「进了表 A 的必然被删掉」这条不变式由圈批本身给出，与排序无关。
	// 受影响的是**可观测性**：水位推进依赖「cutoff 之前已无残留」，
	// 而随机圈批会把老行零散地留在明细里，导致 CutoffDay/PrunedThrough
	// 与实际处理范围对不上，管理端展示的「已剪到哪天」会误导排障。
	//
	// 改按 ts 排序后，圈出的确实是时间最老的一段，水位语义与展示一致。
	// ts 有 idx_usage_ts 索引，排序代价可接受。
	//
	// 圈批到删除之间没有写入竞争：rowid 只会因本事务自己的 DELETE 而消失，
	// 而那发生在圈批的用途全部完成之后。
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO prune_batch (rid)
		     SELECT rowid FROM usage_records WHERE ts < ? ORDER BY ts LIMIT ?`,
		cutoffTs, maxPruneDelete); err != nil {
		return nil, fmt.Errorf("select prune batch: %w", err)
	}

	// 聚合进表 A，谓词严格限定为本轮圈定的那批行（不再直接用 ts，也不再有
	// 参数）。ON CONFLICT 用**累加**而不是覆盖：同一天的明细若因故分两次归档
	// （人工调小 keepDays 再调回、水位被手工回退），覆盖会把先归档的那部分
	// 静默抹掉。累加不重复计数的前提由圈批给出 —— 同一批行只会聚合一次
	//（聚合完即被删除，且与聚合同事务），累加只发生在「不同批次」之间，
	// 每一批的量都只进账一次。
	//
	// COALESCE(user_id, '')：usage_records.user_id 可空（无归属的历史记录），
	// 而表 A 的维度列 NOT NULL —— NULL 进主键会让 ON CONFLICT 永不匹配
	// （SQL 里 NULL ≠ NULL），于是每次归档都多插一行、数字翻倍。
	if rollupRes, err := tx.ExecContext(ctx,
		`INSERT INTO usage_daily_rollups (
			     day, user_id, access_key_id, public_model, upstream_model, provider_id,
			     ingress_protocol, stream,
			     request_count, success_count, error_count,
			     input_tokens, output_tokens, cached_tokens, cached_creation_tokens, reasoning_tokens, total_tokens,
			     cost_total, latency_sum_ms, ttfb_sum_ms, ttfb_count,
			     ok_ttfb_sum_ms, ok_ttfb_count,
			     ok_output_tokens, ok_latency_sum_ms, ok_sample_count, cost_pending_count)
			 SELECT `+dayExpr+` AS day,
			       COALESCE(user_id, ''), access_key_id, public_model, upstream_model, provider_id, ingress_protocol, stream,
			       COUNT(*),
			       COUNT(CASE WHEN status = 'ok' THEN 1 END),
			       COUNT(CASE WHEN status NOT IN ('ok', 'canceled') THEN 1 END),
			       COALESCE(SUM(input_tokens), 0), COALESCE(SUM(output_tokens), 0),
			       COALESCE(SUM(cached_tokens), 0), COALESCE(SUM(cached_creation_tokens), 0),
			       COALESCE(SUM(reasoning_tokens), 0),
			       COALESCE(SUM(total_tokens), 0), COALESCE(SUM(cost_total), 0),
			       COALESCE(SUM(latency_ms), 0),
			       -- 只累计**真正测到过首字**的请求（ttfb_ms > 0）。非流式请求的
			       -- ttfb_ms 是 0，算进 sum 无害但算进 count 会把分母撑大，
			       -- 平均首字被一路稀释。两列必须同口径。
			       COALESCE(SUM(CASE WHEN ttfb_ms > 0 THEN ttfb_ms ELSE 0 END), 0),
			       COUNT(CASE WHEN ttfb_ms > 0 THEN 1 END)
			       ,
			       COALESCE(SUM(CASE WHEN status = 'ok' AND ttfb_ms > 0 THEN ttfb_ms ELSE 0 END), 0),
			       COUNT(CASE WHEN status = 'ok' AND ttfb_ms > 0 THEN 1 END),
			       COALESCE(SUM(CASE WHEN status = 'ok' THEN output_tokens ELSE 0 END), 0),
			       COALESCE(SUM(CASE WHEN status = 'ok' THEN latency_ms ELSE 0 END), 0),
			       COUNT(CASE WHEN status = 'ok' AND output_tokens > 0 AND latency_ms > 0 THEN 1 END),
			       -- 待重算的条数（usage_state='cost_pending'）。它是
			   -- 「这笔费用还是 0、等着被纠正」的标记，归档时不能丢 ——
			   -- 丢了就再也没人知道它该收钱（2026-10-21 修 M5）。
			   COUNT(CASE WHEN usage_state = 'cost_pending' THEN 1 END)
			  FROM usage_records
			 WHERE rowid IN (SELECT rid FROM prune_batch)
			 GROUP BY day, COALESCE(user_id, ''), access_key_id, public_model, upstream_model, provider_id, ingress_protocol, stream
			 ON CONFLICT(day, user_id, access_key_id, public_model, upstream_model, provider_id, ingress_protocol, stream) DO UPDATE SET
			       request_count    = request_count + excluded.request_count,
			       success_count    = success_count + excluded.success_count,
			       error_count      = error_count + excluded.error_count,
			       input_tokens     = input_tokens + excluded.input_tokens,
			       output_tokens    = output_tokens + excluded.output_tokens,
			       cached_tokens    = cached_tokens + excluded.cached_tokens,
			       cached_creation_tokens = cached_creation_tokens + excluded.cached_creation_tokens,
			       reasoning_tokens = reasoning_tokens + excluded.reasoning_tokens,
			       total_tokens     = total_tokens + excluded.total_tokens,
			       cost_total       = cost_total + excluded.cost_total,
			       latency_sum_ms   = latency_sum_ms + excluded.latency_sum_ms,
			       ttfb_sum_ms      = ttfb_sum_ms + excluded.ttfb_sum_ms,
			       ttfb_count       = ttfb_count + excluded.ttfb_count,
			       ok_ttfb_sum_ms   = ok_ttfb_sum_ms + excluded.ok_ttfb_sum_ms,
			       ok_ttfb_count    = ok_ttfb_count + excluded.ok_ttfb_count,
			       ok_output_tokens = ok_output_tokens + excluded.ok_output_tokens,
			       ok_latency_sum_ms = ok_latency_sum_ms + excluded.ok_latency_sum_ms,
			       ok_sample_count = ok_sample_count + excluded.ok_sample_count,
			       cost_pending_count = cost_pending_count + excluded.cost_pending_count`); err != nil {
		return nil, fmt.Errorf("aggregate rollups: %w", err)
	} else if n, aerr := rollupRes.RowsAffected(); aerr == nil {
		res.RollupRows = n
	}

	// 删除与聚合共用同一张 prune_batch：先聚合后删除、同事务、严格同一批行。
	// 因此 RollupRows 与 DeletedRows 描述的是同一批行的两个侧面，恒等可对账。
	if delRes, err := tx.ExecContext(ctx,
		`DELETE FROM usage_records WHERE rowid IN (SELECT rid FROM prune_batch)`); err != nil {
		return nil, fmt.Errorf("delete pruned details: %w", err)
	} else if n, aerr := delRes.RowsAffected(); aerr == nil {
		res.DeletedRows = n
	}

	// **不回收累加列**（user_usage_totals.total_tokens /
	// access_keys.used_tokens）—— 这是刻意的，别再加回来。
	//
	// 这两列是**终身累计**，不是「保留期内用量」：
	//   - DESIGN.md §4 数据模型：`quota_tokens ... 0 = 不限；终身累计 input+output 上限`；
	//   - key_dao.go ReserveQuota 的注释：「配额限的是终身累计，没有『窗口结束』这回事」；
	//   - RecomputeUsedTokens / RecomputeUserUsage 的数据源是 UsageSource
	//     （明细 + 归档），也就是它们把归档量算进权威值。
	//
	// 所以「明细被剪掉」不等于「用量消失」——那部分量已经进了表 A
	// （usage_daily_rollups），仍然属于用户的终身累计。若在这里按被剪量
	// 扣回，会与 Recompute* 的口径分叉，制造出真正的缺陷：**配额被剪枝
	// 悄悄放宽**（用户 30 天前的用量一旦过期，凭空多出一份额度）。
	// TestPrune_StatsUnchangedAcrossPrune 守的就是这条不变量，
	// 它的断言「剪枝后 SumUserUsedTokens 不变」正是本注释的依据。
	//
	// 历史：2026-10-11 曾加过两条「配对回收」UPDATE（意图是抵消
	// trg_update_used_tokens / trg_update_user_usage_totals 的单向累加）。
	// 那两条语句放在 DELETE 之后，靠 `prune_batch JOIN usage_records` 求被剪量
	// —— 而 SQLite 在同事务内 DELETE 之后读不到已删行，JOIN 必然落空、
	// COALESCE(...,0) 把差额兜底成 0，于是回收量恒为 0，是两条空跑语句
	// （2026-10-12 用审计触发器实证：剪枝期间 user_usage_totals 零次 UPDATE）。
	// 也就是说它从未生效过，而这恰好是无害的 —— 生效反而有害，见上。
	// 2026-10-12 删除该死代码，并留下本说明防止后人「顺手修好」它。

	// 用完即弃。DROP 放在 Commit **之前**，让它随本事务一起提交/回滚：若放到
	// Commit 之后用 s.db.Exec，语句可能落到池中另一条连接上（TEMP 表是
	// 每连接的），清理就落空了。
	if _, err := tx.ExecContext(ctx, `DROP TABLE IF EXISTS temp.prune_batch`); err != nil {
		return nil, fmt.Errorf("drop prune batch: %w", err)
	}

	// 水位只在**明细确实清空到cutoff** 时推进，否则不动。
	//
	// 上一版无条件推进：若本次因 maxPruneDelete 截断而没删干净，水位却已
	// 标成「已剪到 cut-1」，下一轮剪枝就会跳过这一段 —— 那段明细既留在
	// 明细表里（占空间、拖慢查询）又不被计入归档，账目与实际永久脱节。
	// 所以必须先确认 cutoffTs 之前已无残留行。
	var remaining int64
	if err := tx.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM usage_records WHERE ts < ? LIMIT 1)`,
		cutoffTs).Scan(&remaining); err != nil {
		return nil, fmt.Errorf("check prune remainder: %w", err)
	}
	if remaining == 0 {
		if _, err := tx.ExecContext(ctx,
			`UPDATE usage_totals SET pruned_through_day = ? WHERE id = 1`, res.PrunedThrough); err != nil {
			return nil, fmt.Errorf("advance prune watermark: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit prune: %w", err)
	}

	// 刻意**不在这里打日志**：调用方（main.go 的每日 ticker / admin 端点）
	// 负责留痕，且它要给日志带上 trigger 与 skipped 的原因。
	// 两边都打 = 一次剪枝两条记录，排查时对不上是第几次跑。
	return res, nil
}

// usageLifetime 是表 B 的一行（总览「全部」档的大数字）。
type usageLifetime struct {
	RequestCount         int64
	SuccessCount         int64
	ErrorCount           int64
	InputTokens          int64
	OutputTokens         int64
	TotalTokens          int64
	CachedTokens         int64
	CachedCreationTokens int64
	ReasoningTokens      int64
	CostTotal            float64
	LatencySumMs         int64
	FirstRecordAt        int64
	PrunedThrough        string
}

// GetUsageLifetime 读终身累计（表 B）。
//
// **只有全局视角能用它**：表 B 是不分用户的全局单行。普通用户的「全部」档必须
// 走「表 A + 明细」的按用户聚合，拿全局数字冒充个人数字就是越权。
// 所以这个方法刻意没有 scopeUserID 参数 —— 少一个可以传错的入参。
func (s *Store) GetUsageLifetime(ctx context.Context) (*usageLifetime, error) {
	var t usageLifetime
	err := s.read.QueryRowContext(ctx,
		`SELECT request_count, success_count, error_count, input_tokens, output_tokens,
		        total_tokens, cached_tokens, cached_creation_tokens, reasoning_tokens, cost_total, latency_sum_ms,
		        first_record_at, pruned_through_day
		   FROM usage_totals WHERE id = 1`).
		Scan(&t.RequestCount, &t.SuccessCount, &t.ErrorCount, &t.InputTokens, &t.OutputTokens,
			&t.TotalTokens, &t.CachedTokens, &t.CachedCreationTokens, &t.ReasoningTokens, &t.CostTotal, &t.LatencySumMs,
			&t.FirstRecordAt, &t.PrunedThrough)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}
