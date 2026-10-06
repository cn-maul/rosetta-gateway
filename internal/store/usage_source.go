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
		CASE WHEN status = 'ok' THEN 1 ELSE 0 END AS n_ok,
		CASE WHEN status NOT IN ('ok', 'canceled') THEN 1 ELSE 0 END AS n_err,
		input_tokens, output_tokens, cached_tokens, reasoning_tokens, total_tokens,
		cost_total, latency_ms AS latency_sum,
		` + dayExpr + ` AS day, ts AS bucket_ts,
		COALESCE(user_id, '') AS user_id, access_key_id, public_model, upstream_model, provider_id`

	// 归档分支：1 行 = 一天 × 一维度下的一组请求，计数已聚合在 request_count 里。
	// bucket_ts 取**当天本地午夜**：分桶查询（/v1/organization/* 的 1h/1d 桶）
	// 需要一个毫秒时间戳，而表 A 只有 day。取午夜是与 dayExpr 反向自洽的选法 ——
	// 取别的点会让同一天在两个表达式下落到不同桶。
	usageSourceRollupCols = `request_count AS n,
		success_count AS n_ok,
		error_count AS n_err,
		input_tokens, output_tokens, cached_tokens, reasoning_tokens, total_tokens,
		cost_total, latency_sum_ms AS latency_sum,
		day, ` + dayStartMs + ` AS bucket_ts,
		user_id, access_key_id, public_model, upstream_model, provider_id`

	// dayStartMs/dayEndMs 把表 A 的 day（'YYYY-MM-DD'，本地时区）还原成
	// 本地零点的毫秒时间戳。'utc' 修饰符的含义是「把这个值当本地时间理解、
	// 转成 UTC」，正是 dayExpr 反向需要的那个方向。
	dayStartMs = `CAST(strftime('%s', day, 'utc') AS INTEGER) * 1000`
	dayEndMs   = `(` + dayStartMs + ` + 86400000)`
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
// # 边界规则：归档只取**完整落在窗口内**的天
//
// 归档的分辨率就是一天（表 A 的 day 列没有更细的时间）。窗口边缘若切在某天
// 中间，那一天的部分数据已经不可得。因此规则取「整日包含」：
// 宁可少算窗口边缘那一天，也不越界多算 —— 费用口径下少算可解释，多算会被
// 当成错账。UI 的时间范围 to 都是「现在」，而当天永远不会被剪，所以上界
// 实际不触发；下界只在「近 1 年」「全部」这类跨到归档区的范围上生效。
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

	if f.From > 0 {
		add("ts >= ?", dayStartMs+" >= ?", f.From)
	}
	if f.To > 0 {
		add("ts <= ?", dayEndMs+" <= ?", f.To)
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
