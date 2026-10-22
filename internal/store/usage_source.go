package store

// UsageFilter 是 UsageSource 接受的条件集合。
//
// 刻意**不含** status / http_status：归档表按天聚合后无法还原单次请求的状态，
// 带上 status 的查询（调用历史、CSV 导出）只能打在明细上，剪枝后查不到
// 30 天前的那次调用 —— 那是设计 §4.9 明确接受的取舍（追查靠日志，不靠库）。
type UsageFilter struct {
	// From/To 为毫秒时间戳，0 表示不设该边界。
	From int64
	To   int64
	// UserID 非空时按用户收窄。空串 = 全局（admin 视角），
	// 语义同 scopeClause：调用方必须自己确保这个空值来自 callerScope。
	UserID string
	// 以下三个非空时按维度收窄。
	KeyID       string
	PublicModel string
	ProviderID  string
}

// 归一化后的列集合。两个分支的列名与含义必须逐项一致 —— 消费方按
// 「SUM(u.列名)」聚合，列名对不上就是静默的错数。
//
// n 是**请求数**而不是行数：明细一行 = 一次请求，归档一行 = 一天一维度下的一组
// 请求。用 COUNT(*) 统计 UNION 结果会把归档行当成 1 次请求，直接少算。
const (
	// 明细分支：1 行 = 1 次请求。bucket_ts = 真实 ts。
	usageSourceDetailCols = `1 AS n,
		CASE WHEN status = '` + UsageStatusOK + `' THEN 1 ELSE 0 END AS n_ok,
		CASE WHEN status NOT IN ('` + UsageStatusOK + `', '` + UsageStatusCanceled + `') THEN 1 ELSE 0 END AS n_err,
		input_tokens, output_tokens, cached_tokens, cached_creation_tokens, reasoning_tokens, total_tokens,
		cost_total, latency_ms AS latency_sum,
		CASE WHEN ttfb_ms > 0 THEN ttfb_ms ELSE 0 END AS ttfb_sum,
		CASE WHEN ttfb_ms > 0 THEN 1 ELSE 0 END AS n_ttfb,
		-- 近期性能的独立口径：只取成功请求。ttfb_sum/n_ttfb 还要服务
		-- 分组统计，那里包含失败请求的已测首字，不能在这里顺手改口径。
		CASE WHEN status = '` + UsageStatusOK + `' AND ttfb_ms > 0 THEN ttfb_ms ELSE 0 END AS ttfb_ok_sum,
		CASE WHEN status = '` + UsageStatusOK + `' AND ttfb_ms > 0 THEN 1 ELSE 0 END AS n_ttfb_ok,
		-- 速度的分子分母，**只取成功请求**。canceled 的 latency 是
		-- 「断开前跑了多久」而非完整耗时，用它当分母会把平均速度压低；
		-- truncated / error 同理（它们往往还没开始吐 token）。
		CASE WHEN status = '` + UsageStatusOK + `' THEN output_tokens ELSE 0 END AS out_ok,
		CASE WHEN status = '` + UsageStatusOK + `' THEN latency_ms ELSE 0 END AS lat_ok,
		-- 速度样本的实际条数。明细一行一个请求；归档支对应 ok_sample_count。
		-- 与 out_ok/lat_ok 必须同口径，否则归档后「样本数」会退化成行数。
		CASE WHEN status = '` + UsageStatusOK + `' AND output_tokens > 0 AND latency_ms > 0 THEN 1 ELSE 0 END AS n_ok_speed,
		-- 待重算的条数（2026-10-21 修 M5）。「费用还是 0、等着被纠正」
		-- 的记录数，归档后仍然可数 —— 剪枝不该让这个信号凭空消失。
		CASE WHEN usage_state = '` + UsageStateCostPending + `' THEN 1 ELSE 0 END AS n_cost_pending,
		` + dayExpr + ` AS day, ts AS bucket_ts,
		COALESCE(user_id, '') AS user_id, access_key_id, public_model, upstream_model, provider_id`

	// 归档分支：1 行 = 一天 × 一维度下的一组请求，计数已聚合在 request_count 里。
	// bucket_ts 取**当天本地午夜**：分桶查询（/v1/organization/* 的 1h/1d 桶）
	// 需要一个毫秒时间戳，而表 A 只有 day。取午夜是与 dayExpr 反向自洽的选法 ——
	// 取别的点会让同一天在两个表达式下落到不同桶。
	usageSourceRollupCols = `request_count AS n,
		success_count AS n_ok,
		error_count AS n_err,
		input_tokens, output_tokens, cached_tokens, cached_creation_tokens, reasoning_tokens, total_tokens,
		cost_total, latency_sum_ms AS latency_sum,
		ttfb_sum_ms AS ttfb_sum,
		ttfb_count AS n_ttfb,
		-- 成功请求的首字累计值，对应明细的 ttfb_ok_sum / n_ttfb_ok。
		ok_ttfb_sum_ms AS ttfb_ok_sum,
		ok_ttfb_count AS n_ttfb_ok,
		-- 归档侧直接带成功请求的 token / 延迟累计值，不靠 success_count 的
		-- 比例去折算总体列：折算出来的是估算值，而这张表的立身之本是
		-- 「剪枝前后同一个窗口给出同一个数」（见 usageDailyRollupsDDL）。
		ok_output_tokens AS out_ok,
		ok_latency_sum_ms AS lat_ok,
		-- 成功请求数里「同时有正输出与正延迟」的条数，对应明细的 n_ok_speed。
		ok_sample_count AS n_ok_speed,
		-- 归档层的待重算条数（2026-10-21 修 M5）。
		cost_pending_count AS n_cost_pending,
		day, ` + dayStartMs + ` AS bucket_ts,
		user_id, access_key_id, public_model, upstream_model, provider_id`

	// dayStartMs/dayEndMs 把表 A 的 day（'YYYY-MM-DD'，本地时区）还原成
	// 本地零点的毫秒时间戳。'utc' 修饰符的含义是「把这个值当本地时间理解、
	// 转成 UTC」，正是 dayExpr 反向需要的那个方向。
	dayStartMs = `CAST(strftime('%s', day, 'utc') AS INTEGER) * 1000`

	// dayLowerBound/dayUpperBound 把窗口的毫秒时间戳换算成 **day 文本**
	// 范围，用作归档支的过滤谓词。
	//
	// # 为什么不用 dayStartMs/dayEndMs 过滤（2026-10-22 修 R10）
	//
	// 旧谓词是 `dayEndMs > ? AND dayStartMs <= ?`（dayEndMs = dayStartMs +
	// 86400000）：谓词的**左侧**是一个
	// 关于索引列 day 的表达式，SQLite 无法把它化成索引的查找区间 ——
	// 实测（5000 行归档表）查询计划是
	//
	//	SCAN usage_daily_rollups USING COVERING INDEX idx_rollup_day
	//
	// 也就是**整个索引全扫**。表 A 随时间单调增长（每天每维度一行），
	// 剪枝跑得越久它越大，而「全部历史」窗口恰好要扫全部行 ——
	// 于是剪枝/归档相关的扫描退化为全表，运维看到的是「越用越慢」。
	//
	// # 为什么改成文本范围就快了
	//
	// `day >= ? AND day <= ?` 的左侧是**裸列**，SQLite 能直接用它做索引
	// 区间查找：
	//
	//	SEARCH usage_daily_rollups USING COVERING INDEX idx_rollup_day (day>? AND day<?)
	//
	// 实测同一张表：1.0ms → ~0ms（命中索引区间，不再全扫）。
	//
	// # 为什么语义等价
	//
	// day 列是 'YYYY-MM-DD' 定宽零填充，字典序即时间序；把窗口上下界
	// 按**同一套**本地日界（dayExpr 的 'localtime' 口径）换算成日期串，
	// 得到的正是「与窗口有交集的那几天」。
	//
	// 旧谓词算的是「那一天的本地午夜落在窗口内」，新谓词算的是
	// 「那一天**有任何一刻**落在窗口内」—— 后者更宽，而这正是 P0-7
	// 确立的规则（见 usageSource 的「边界规则」注释：有交集即计入，
	// 宁可多算不可少算）。
	//
	// 等价性实测：300 个随机窗口（含非午夜边界）下两种谓词的行数
	// **完全一致**（不一致 0 个）。
	dayLowerBound = `strftime('%Y-%m-%d', ? / 1000, 'unixepoch', 'localtime')`
	dayUpperBound = `strftime('%Y-%m-%d', ? / 1000, 'unixepoch', 'localtime')`
)

// UsageSource 返回可直接嵌进 FROM 的子查询，把「未剪枝的明细」与
// 「已归档的日聚合」归一成同一组列，供所有**聚合**类入口共用。
//
// 返回的 SQL 形如 `(SELECT ... FROM usage_records WHERE ... UNION ALL
// SELECT ... FROM usage_daily_rollups WHERE ...) u`，参数按出现顺序返回。
//
// # 为什么必须合并而不是二选一
//
// 明细保留 30 天（表 A），终身累计在表 B。但**聚合入口打的是明细**：
// 用户级配额预检（SUM over usage_records，热路径）、总览 stats、
// 用量页 summary、四个 by-* 分组端点。剪枝一旦真的跑起来，这些入口的
// 求和就会随时间**自动变小** —— 用户配额每天清零、总览「全部」档从 3 亿
// 掉到 5 百万。后者被设计 §4.8 直接判定为数据丢失，比「看板变慢」严重得多。
//
// # 边界规则：归档只取**与窗口有交集**的天
//
// 归档的分辨率就是一天（表 A 的 day 列没有更细的时间）。窗口边缘若切在某天
// 中间，那一天的**部分**数据已经不可得（明细被剪掉了）。
//
// 因此规则取「有交集即计入」，而不是「整日包含」：少算会造成真实资损
// （收入漏记、余额与报表对不上），多算至多偏一天且方向保守。
// 详见下方 add() 调用处的完整论证（2026-10-21 修 P0-7）。
//
// ⚠️ 86400000 假定一天 24 小时。在有 DST 的时区里，那天可能是 23 或 25 小时，
// dayEnd 会偏差 1 小时，只影响恰好落在这一小时内的窗口边缘。
//
// 归档分支的空表代价：水位未推进时该分支扫 0 行，rollup 表本身也远小于明细。
// 所以这里**不读** usage_totals.pruned_through_day 做短路 —— 省不了一次
// 往返查询，却让「有没有归档过」这个状态多出一处需要保持一致的判断。
func UsageSource(f UsageFilter) (string, []any) {
	return usageSource("u", f)
}

func usageSource(alias string, f UsageFilter) (string, []any) {
	// 两个 UNION 支的占位符各自独立，参数必须按「先明细支、后归档支」的顺序
	// 分两段返回。合成一段（每条件只 append 一次）是这里最容易写出的错：
	// SQL 仍能拼出来，只是绑定数对不上，报 "missing argument with index N" ——
	// 每个调用方都会当场炸，所以测试必然抓到。
	type cond struct {
		text  string
		value any
	}
	var detailConds, rollupConds []cond
	add := func(dText, rText string, v any) {
		detailConds = append(detailConds, cond{dText, v})
		rollupConds = append(rollupConds, cond{rText, v})
	}

	// 边界规则：归档支取**与窗口有交集**的天，而不是「整日包含」的天。
	//
	// # 为什么（2026-10-21 修 P0-7）
	//
	// 旧规则是「dayStart >= From 且 dayEnd <= To」（整日包含）。它错在
	// 假设 **To 一定是本地午夜** —— 而真实调用点的 To 几乎从不是：
	//   - /v1/organization/*：To = end_date + 1 天的 UTC 零点；
	//   - 管理端用量页：To = 「现在」（queryRangeExplicit）。
	//
	// 于是窗口上界总在某天的正中间，而那一天的数据在剪枝后已进归档表，
	// 却不满足「整日包含」被整体排除。实测（真实库、8333 行明细、
	// TZ=Asia/Shanghai）：窗口 [10-07 12:00, 10-09 12:00] 在剪枝前
	// 是 215338903 token，剪枝后只剩 27882697 —— **丢了 87%**。
	//
	// 「整日包含」还有一个更隐蔽的错：它让**剪枝前后的同一个窗口给出
	// 不同的数**。剪枝前那条天在明细里（按 ts 精确过滤，边界正确），
	// 剪枝后它在归档里（按整天过滤，边界错误）—— 这条验收
	// （TestPrune_StatsUnchangedAcrossPrune）之所以绿，是因为它
	// 刻意把窗口对齐到本地午夜，与生产调用点不一致。
	//
	// # 交集规则的实际含义
	//
	// 归档的一行代表「某一天的全部用量」。它与窗口有交集时必须计入，
	// 哪怕只有部分落在窗口内 —— 因为**明细已被删**，那部分的精确值
	// 已经不可得。
	//
	// 代价：窗口边缘那一天会被**多算**（整天的量都算进来，而窗口只
	// 覆盖其中一部分）。这个方向是刻意选的：
	//   - 少算 = 收入漏记、用户配额被悄悄放宽、报表与余额对不上；
	//   - 多算 = 用户被多扣一点、报表偏保守。
	// 少算会造成**真实资损**且无声无息，多算至少有界（一天）且可解释。
	//
	// 换言之：宁可多算，不可少算。
	if f.From > 0 {
		// 归档支用 day 文本范围（走 idx_rollup_day 的索引区间），
		// 而不是 dayEndMs > ?（表达式谓词 → 索引退化为全扫，见 R10）。
		add("ts >= ?", "day >= "+dayLowerBound, f.From)
	}
	if f.To > 0 {
		add("ts <= ?", "day <= "+dayUpperBound, f.To)
	}
	// user_id 过滤明细支**必须**写成裸列比较（user_id = ?），不能 COALESCE：
	// 表达式谓词吃不到 idx_usage_user_ts(user_id, ts)，用户级配额预检是每个
	// /v1 请求都跑的热路径，退化成全表扫就是全站延迟抬升。语义上两者等价 ——
	// user_id 为 NULL 的行（无归属的历史记录）既不等于任何非空 scope 值，
	// 也不该被计入某个具体用户的用量。归档支本来就是裸列。
	if f.UserID != "" {
		add("user_id = ?", "user_id = ?", f.UserID)
	}
	for _, c := range []struct{ val, col string }{
		{f.KeyID, "access_key_id"},
		{f.PublicModel, "public_model"},
		{f.ProviderID, "provider_id"},
	} {
		if c.val != "" {
			add(c.col+" = ?", c.col+" = ?", c.val)
		}
	}

	branch := func(selects, from string, conds []cond) string {
		s := `SELECT ` + selects + ` FROM ` + from
		for i, c := range conds {
			if i == 0 {
				s += ` WHERE ` + c.text
			} else {
				s += ` AND ` + c.text
			}
		}
		return s
	}
	sql := "(" + branch(usageSourceDetailCols, "usage_records", detailConds) +
		" UNION ALL " +
		branch(usageSourceRollupCols, "usage_daily_rollups", rollupConds) + ") " + alias

	var args []any
	for _, c := range detailConds {
		args = append(args, c.value)
	}
	for _, c := range rollupConds {
		args = append(args, c.value)
	}
	return sql, args
}

// UsageRatioExpr 构造「加权平均」表达式：SUM(分子) / SUM(分母)。
//
// 归档行的 latency_sum_ms / ttfb 是**一组请求的累计值**，不是单次值。
// 直接 AVG(延迟列) 会把一天的总量当成一次请求的耗时平均进去，越剪越离谱。
// 分母用请求数（n）而不是行数，理由同 UsageFilter 的 n 列。
//
// 分母为 0 时返回 0（与各查询原有的 COALESCE 口径一致），不做 NULL 传播。
func UsageRatioExpr(numerator, denominator, alias string) string {
	return "COALESCE(SUM(" + alias + "." + numerator + ") * 1.0 / " +
		"NULLIF(SUM(" + alias + "." + denominator + "), 0), 0)"
}

// UsageSumExpr 构造求和表达式，归档与明细共用同一个列名。
func UsageSumExpr(column, alias string) string {
	return "COALESCE(SUM(" + alias + "." + column + "), 0)"
}
