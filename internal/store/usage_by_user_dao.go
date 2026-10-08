package store

import "context"

// UserUsageStat 是「按用户汇总消费」的一行（GET /admin/api/usage/by-user 的数据源）。
//
// 与 UsageStats 的区别是**维度**：那个是「一个区间的合计」，这个是「每个用户
// 各自多少」。管理员看钱包页要的正是后者 —— 「谁花得多」。
type UserUsageStat struct {
	// UserID 是 usage_records.user_id 的**冗余固化**值（多用户改造 P0，
	// 见 UsageRecord.UserID 的注释：归属是历史事实，不靠 JOIN key 回溯）。
	//
	// 空串 = **无归属**：user_id 为 NULL/'' 的历史用量（迁移前建的无主 key
	// 产生的）。它不是一个具体用户，所以单独成一行而不是被丢掉 —— 丢掉它，
	// 这张表的合计就对不上总览的全局合计，而「两张表对不上账」比多一行难解释得多。
	UserID string
	// Username 是 LEFT JOIN users 拿到的展示名。
	//
	// **空串有两种可能，调用方必须靠 UserID 区分**：
	//   - UserID 非空而 Username 空 → users 表里已没有这个 id，即**用户已删除**；
	//   - UserID 本身为空           → 无归属用量。
	// 两种都不是「名字叫空字符串」。前端在第一种情况下必须回退显示 UserID ——
	// 留白会看起来像「无归属」，而「用户已删除」与「无归属」在处置上是两回事
	// （前者是一个真实发生过消费的账号，后者是迁移遗留）。
	Username string
	// Role 是 users.role（admin / user）；空串同样表示 JOIN 不到（用户已删除）。
	//
	// 为什么连角色一起查出来：管理员**不该**出现在这张表里（后端
	// ErrAdminCannotCallModel 已拦下管理员调用模型），但升级前可能有他的历史
	// 用量。不给这一列的话，那一行看起来就像一条脏数据；给了就能如实标注
	// 「管理员 · 升级前的历史用量」。刻意**不**在 SQL 里按角色过滤掉它 ——
	// 静默过滤会让这张表的合计小于全局合计，而那是最难查的一类问题。
	Role string
	// Requests 是请求次数，取 SUM(n) 而**不是** COUNT(*)：归档一行代表
	// 「一天 × 一维度下的一组请求」（见 UsageSource 的 n 列），
	// COUNT(*) 会把它当成 1 次请求，直接少算。
	Requests int64
	Tokens   int64
	// Cost 是该用户固化的费用合计（元），即 SUM(cost_total)。
	//
	// 读固化值而不是按当前单价重算：改价不回溯历史（见 freezeUsageCost）。
	// 这一列是这张表的主角 —— 用户要的就是「每人花了多少钱」。
	Cost float64
}

// UsageByUser 按用户汇总 [from,to]（毫秒）内的用量，**按费用降序**返回，
// 并同时返回有消费的用户总数（供调用方判断结果是否被 limit 截断）。
//
// # 数据源是 UsageSource（明细 ∪ 日归档）
//
// 与 GetUsageStats / SumCostBuckets 同一个理由：只查明细的话，30 天剪枝一跑，
// 按用户的汇总就会**逐日缩水** —— 用户上个月花的钱会凭空消失，而这张表正是
// 管理员用来对账的。归档那半边是「剪枝后合计不变」的唯一保证。
//
// # JOIN users 为什么放在归一化来源**之外**
//
// 与 handler.groupBy 里 JOIN access_keys 同一个理由：用户名是展示属性、
// 不参与聚合，塞进 UNION 支里会随「一个用户对应多条来源行」把计数复制多份。
// users.id 是主键（至多匹配一行），放在外层是 1:1 的，聚合口径不受影响。
//
// 用 **LEFT** JOIN 而不是 INNER：用量是历史事实，用户被删除**不该**让那一行
// 消失 —— 那会让这张表的合计小于全局合计，而且完全看不出少在哪。
//
// # 排序
//
// cost DESC：管理员看这张表是为了找「谁花得多」，金额降序才是它的用途。
// 相等时以 user_id 兜底 —— 没有 tiebreaker 时同额行的相对顺序由查询计划决定，
// 两次刷新之间会跳，读起来像数据在自己变。
//
// limit <= 0 时不加 LIMIT。**调用方必须自己钳制上限**（handler 走 clampLimit）：
// 这个查询要扫归一化来源的全部行，不设限等于把整表物化进内存。
func (s *Store) UsageByUser(ctx context.Context, from, to int64, limit int) ([]UserUsageStat, int64, error) {
	src, args := UsageSource(UsageFilter{From: from, To: to})

	// 有消费的用户总数（去重），单独一条查询。
	//
	// 为什么必须有它：列表被 limit 截断时，调用方**只有**拿到总数才能说出
	// 「显示前 N / 共 M 位用户」。没有它，一张被截断的表与一张完整的表长得
	// 一模一样 —— 而这张表是拿来做金额判断的：把「前 100 名」读成「全部用户」
	// 会让人得出「这家公司只有 100 个账号」，或更糟：「消费合计对不上汇总卡」。
	// 与 ExportCSV 用 X-Export-Truncated 表态是同一个原则：**截断必须可见**。
	var total int64
	if err := s.read.QueryRowContext(ctx,
		`SELECT COUNT(DISTINCT u.user_id) FROM `+src, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	// 聚合别名（requests / tokens / cost）在 ORDER BY 里可直接引用，
	// SQLite 允许；GROUP BY 里不行，那里必须写原列名。
	//
	// ⚠️ UsageSumExpr 只返回 `COALESCE(SUM(...), 0)`，**不带 AS 别名** ——
	// 别名的拼接是调用方的责任（对比 usage_handler.groupBy 里的
	// `COALESCE(SUM(u.n), 0) AS count` 与 `UsageSumExpr(...) + " AS tokens"`）。
	// 漏掉别名时 ORDER BY cost 会报 "no such column: cost"，
	// 而这个报错在编译期看不出来，只有真跑一次查询才会暴露。
	query := `SELECT u.user_id,
		       COALESCE(usr.username, '') AS username,
		       COALESCE(usr.role, '')     AS role,
		       COALESCE(SUM(u.n), 0)      AS requests,
		       ` + UsageSumExpr("total_tokens", "u") + ` AS tokens,
		       ` + UsageSumExpr("cost_total", "u") + `   AS cost
		    FROM ` + src + `
		    LEFT JOIN users usr ON usr.id = u.user_id
		   GROUP BY u.user_id
		   ORDER BY cost DESC, u.user_id ASC`
	rowArgs := args
	if limit > 0 {
		query += ` LIMIT ?`
		// 复制一份再 append：args 由 UsageSource 返回，可能与调用方共享底层数组，
		// 原地 append 会污染它（本函数只读一次，但这是给后来者埋雷的形状）。
		rowArgs = append(append([]any{}, args...), limit)
	}

	rows, err := s.read.QueryContext(ctx, query, rowArgs...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	// 初始化为空切片而非 nil：nil 会被编码成 JSON null，前端 .map() 直接
	// TypeError（与 writeGroupEntries 同一条约定）。
	out := make([]UserUsageStat, 0)
	for rows.Next() {
		var st UserUsageStat
		if err := rows.Scan(&st.UserID, &st.Username, &st.Role,
			&st.Requests, &st.Tokens, &st.Cost); err != nil {
			return nil, 0, err
		}
		out = append(out, st)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	return out, total, nil
}
