package store

// P1.5：用量归档（终身累计表 + 触发器 + 一次性回填 + 30 天剪枝）。见 MULTIUSER.md §4.8。
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

// 一次性数据迁移的完成标记，存在 app_settings 里。
//
// 为什么用标记而不是「靠 SQL 自身幂等」：这两步都是全表扫，每次启动都跑
// 会让启动时间随历史数据量线性增长；而 backfill 的谓词 `cost_total = 0`
// 对「未配置价格的模型」恒为真，重跑永远命中同一批行，是死循环的经典形状。
const (
	settingUsageCostBackfillKey = "usage_cost_backfilled_v1"
	settingUsageTotalsSeedKey   = "usage_totals_seeded_v1"
)

// usageDailyRollupsDDL 是表 A 的建表语句。
//
// 抽成常量而不是写死在 migrations 里：老库这张表可能带的是**旧的 7 列主键**
// （缺 upstream_model 维度），而 SQLite 不能 ALTER TABLE 改主键，只能整张重建 ——
// 重建时要原样再执行一遍同一份 DDL。两处各写一份迟早漂移，而漂移的后果是
// 「新建库与重建库的结构不同」，最难查的那类差异。
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
	reasoning_tokens INTEGER NOT NULL DEFAULT 0,
	total_tokens     INTEGER NOT NULL DEFAULT 0,
	-- 固化计价：不固化的话改价后历史费用永久算错。
	cost_total       REAL NOT NULL DEFAULT 0,
	-- 延迟按请求数加权平均所需（算术平均会被快慢请求互相抵消）。
	latency_sum_ms   INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (day, user_id, access_key_id, public_model, upstream_model,
	             provider_id, ingress_protocol, stream)
)`

// ensureRollupDimensions 把「旧 7 列主键」的表 A 重建为当前的 8 列。
//
// 为什么必须重建而不是 ADD COLUMN：归档写入靠
// `ON CONFLICT(day, user_id, ..., upstream_model, ...) DO UPDATE` 做幂等合并，
// 而 SQLite 要求 ON CONFLICT 的目标列**恰好**构成主键或唯一索引。只补列不改
// 主键 → 那条 ON CONFLICT 直接报错（"target columns for on conflict do update
// clause do not match any PRIMARY KEY or UNIQUE constraint"）→ 剪枝永久失败；
// 列在而主键不含它，同样是静默双计。
//
// 为什么判定重建不丢数据：缺列的表是「P1.5 schema 已落盘、归档逻辑还没接上」
// 那一版建的，从来没写入过行 —— 有行就说明真跑过归档，那种库得人工处理。
// 所以仅在该表为空时重建；非空就让启动失败，比悄悄丢历史好。
func (s *Store) ensureRollupDimensions() error {
	has, err := s.columnExists("usage_daily_rollups", "upstream_model")
	if err != nil {
		return fmt.Errorf("check rollup dimension column: %w", err)
	}
	if has {
		return nil
	}
	var rows int64
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM usage_daily_rollups`).Scan(&rows); err != nil {
		return fmt.Errorf("count rollup rows: %w", err)
	}
	if rows > 0 {
		return fmt.Errorf("usage_daily_rollups has %d rows but lacks the upstream_model "+
			"dimension; archived rows cannot be re-derived from pruned details - repair by hand", rows)
	}
	if _, err := s.db.Exec(`DROP TABLE usage_daily_rollups`); err != nil {
		return fmt.Errorf("drop stale rollup table: %w", err)
	}
	if _, err := s.db.Exec(usageDailyRollupsDDL); err != nil {
		return fmt.Errorf("recreate rollup table: %w", err)
	}
	// DROP TABLE 连带删掉它的全部索引（含 ensureUserIndexes 建的
	// idx_rollup_user / idx_rollup_key），必须逐条重造。
	// 这里刻意写成完整清单而不是复用 ensureUserIndexes：那个函数还建
	// access_keys / users 的索引，与本表无关，而它跑在 ensureColumns 之后、
	// 晚于本次调用。索引清单重复两处的风险是「加索引忘了同步这里」，
	// 而症状是重建库上的归档查询全表扫 —— 慢，不报错，最难发现的那类。
	for _, stmt := range []string{
		`CREATE INDEX IF NOT EXISTS idx_rollup_day ON usage_daily_rollups(day)`,
		`CREATE INDEX IF NOT EXISTS idx_rollup_user ON usage_daily_rollups(user_id)`,
		`CREATE INDEX IF NOT EXISTS idx_rollup_key ON usage_daily_rollups(access_key_id)`,
	} {
		if _, err := s.db.Exec(stmt); err != nil {
			return fmt.Errorf("recreate rollup index: %w", err)
		}
	}
	s.logger.Info("recreated usage_daily_rollups with the upstream_model dimension")
	return nil
}

// ensureUsageTotalsTrigger 建终身累计触发器。
//
// **必须排在 ensureColumns 之后调用**：触发器体引用 NEW.cost_total，而老库的
// usage_records 原本没有这一列（列是 ensureColumns 补的）。塞进 migrations 列表
// 去建，老库升级时 SQLite 直接报「no such column: cost_total」→ migrate() 失败
// → **整个网关起不来**（与 ensureUserIndexes 同一类问题）。
//
// 先 DROP 再 CREATE（不用 CREATE ... IF NOT EXISTS）：后者在触发器已存在时
// 会完全跳过，于是改过触发器体之后**老库永远保留旧体**，与新库静默分叉且不报错
// —— totals 漂移只有在对账时才可能发现。DROP + CREATE 同样幂等。
func (s *Store) ensureUsageTotalsTrigger() error {
	// DROP 与 CREATE 放在**同一条写事务**里。分两次 Exec 时若 DROP 成功、
	// CREATE 失败，就会留下「触发器已删但没重建」的窗口 —— 那段时间内
	// usage_totals 完全不再累加，且没有任何信号（DROP 本身不报错）。
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin usage totals trigger tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }() //nolint:errcheck // 已提交时是 no-op

	if _, err := tx.Exec(`DROP TRIGGER IF EXISTS trg_update_usage_totals`); err != nil {
		return fmt.Errorf("drop usage totals trigger: %w", err)
	}
	_, err = tx.Exec(`CREATE TRIGGER trg_update_usage_totals
		AFTER INSERT ON usage_records
		BEGIN
		  UPDATE usage_totals SET
		    request_count    = request_count + 1,
		    success_count    = success_count + CASE WHEN NEW.status = 'ok' THEN 1 ELSE 0 END,
		    error_count      = error_count + CASE WHEN NEW.status NOT IN ('ok', 'canceled') THEN 1 ELSE 0 END,
		    input_tokens     = input_tokens + NEW.input_tokens,
		    output_tokens    = output_tokens + NEW.output_tokens,
		    total_tokens     = total_tokens + NEW.total_tokens,
		    cached_tokens    = cached_tokens + NEW.cached_tokens,
		    reasoning_tokens = reasoning_tokens + NEW.reasoning_tokens,
		    cost_total       = cost_total + NEW.cost_total,
		    latency_sum_ms   = latency_sum_ms + NEW.latency_ms,
		    first_record_at  = CASE WHEN first_record_at = 0 THEN NEW.ts
		                           ELSE MIN(first_record_at, NEW.ts) END
		  WHERE id = 1;
		END`)
	if err != nil {
		return fmt.Errorf("create usage totals trigger: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit usage totals trigger tx: %w", err)
	}
	return nil
}

// backfillUsageCost 给存量明细补固化费用。
//
// cost_total 是 P1.5 新增列，历史行全是 0 —— 直接切到「读 SUM(cost_total)」，
// 昨天还在报表里的费用今早就凭空归零。所以先按**当前**单价补一遍：
// 这是唯一一次用当前价回算历史，此后改价不再影响已落库的金额。
//
// 分批的原因：一条 UPDATE 覆盖百万行会长时间持有写锁；SQLite 的 UPDATE
// 又不支持 ORDER BY / LIMIT，所以用 rowid 游标手动切批。
//
// 游标而不是 `WHERE cost_total = 0 LIMIT n`：后者在未配价的模型上永远成立，
// 每批都选中同一批行 → 永不收敛。rowid 游标严格递增，必然终止。
func (s *Store) backfillUsageCost() error {
	ctx := context.Background()
	if done, _, err := s.getSetting(ctx, settingUsageCostBackfillKey); err != nil {
		return fmt.Errorf("read cost backfill marker: %w", err)
	} else if done != "" {
		return nil
	}

	const batch = 5000
	var cursor int64
	var total int64
	for {
		// 本批的 rowid 上界。取上界到 UPDATE 之间没有写入竞争：migrate() 跑在
		// 网关开始接流量之前，且新写入的 cost_total 由 DAO 直接算好。
		var next sql.NullInt64
		if err := s.db.QueryRowContext(ctx,
			`SELECT MAX(rowid) FROM (SELECT rowid FROM usage_records WHERE rowid > ? ORDER BY rowid LIMIT ?)`,
			cursor, batch).Scan(&next); err != nil {
			return fmt.Errorf("usage cost backfill cursor: %w", err)
		}
		if !next.Valid {
			break
		}

		// 口径与 freezeUsageCost 逐字一致，只是搬进 SQL：NULL 价按 0，未命中输入
		// 用 MAX(input - cached, 0) 兜住上游把 cached 算在 input 之外的情形
		// （负数会把总额抹成负的）。模型已删除时子查询返回 NULL → COALESCE 成 0
		// → 该行 0 元，与「未配价 = 0」同语义，不会因一行查不到价就整体失败。
		res, err := s.db.ExecContext(ctx,
			`UPDATE usage_records SET cost_total = COALESCE((
					SELECT (MAX(usage_records.input_tokens - usage_records.cached_tokens, 0) * COALESCE(m.price_input, 0)
					        + usage_records.cached_tokens * (CASE WHEN COALESCE(m.price_cache_hit, 0) > 0 THEN m.price_cache_hit ELSE COALESCE(m.price_input, 0) END)
					        + usage_records.output_tokens * COALESCE(m.price_output, 0)) / 1000000.0
					FROM upstream_models m
					WHERE m.provider_id = usage_records.provider_id AND m.model_id = usage_records.upstream_model), 0)
				WHERE rowid > ? AND rowid <= ?`,
			cursor, next.Int64)
		if err != nil {
			return fmt.Errorf("usage cost backfill batch: %w", err)
		}
		if n, aerr := res.RowsAffected(); aerr == nil {
			total += n
		}
		cursor = next.Int64
	}

	if err := s.setSetting(ctx, settingUsageCostBackfillKey, fmt.Sprintf("%d", total)); err != nil {
		return fmt.Errorf("write cost backfill marker: %w", err)
	}
	if total > 0 {
		s.logger.Info("backfilled frozen cost for historical usage rows", "rows", total)
	}
	return nil
}

// reconcileUsageTotals 把「触发器存在之前就已落库」的明细一次性灌进累计表。
//
// 需要它的理由：usage_totals 是 P1.5 新建的空表，触发器只能覆盖此后新插入的行。
// 不补这一步，老库升级后「全部」档只剩今天的数据 —— 又一种「界面能配但不生效」。
//
// # 为什么是覆盖（SET = 子查询聚合）而不是累加
//
// 执行点在 migrate() 里、流量开始之前，此时表 B 只有 (id=1) 占位行、全为 0；
// 覆盖与累加等价，而覆盖对「标记丢失后重跑」是安全的（累加会翻倍）。
//
// # 为什么先看 pruned_through_day
//
// 剪枝一旦跑过，明细表里已经没有全部历史，按明细重算会把已归档的部分冲掉。
// 那种情况下表 B 已由触发器与归档共同维护，不能被「重算」倒退覆盖。
func (s *Store) reconcileUsageTotals() error {
	ctx := context.Background()
	if done, _, err := s.getSetting(ctx, settingUsageTotalsSeedKey); err != nil {
		return fmt.Errorf("read totals seed marker: %w", err)
	} else if done != "" {
		return nil
	}

	var pruned string
	if err := s.db.QueryRowContext(ctx,
		`SELECT pruned_through_day FROM usage_totals WHERE id = 1`).Scan(&pruned); err != nil {
		return fmt.Errorf("read usage_totals watermark: %w", err)
	}
	if pruned != "" {
		// 已有归档水位：表 B 正在被维护，不再重算。标记照写，免得每次启动白查。
		s.logger.Warn("usage totals already archived; skipping one-time reconcile",
			"pruned_through_day", pruned)
		return s.setSetting(ctx, settingUsageTotalsSeedKey, "skipped")
	}

	// CASE 表达式必须与触发器、与 UsageSource 的归一化列逐字对齐：三处口径
	// 任何一个不同，「剪枝前后同一窗口数字相等」这条验收就永久做不到。
	// 这里直接用 UsageSource 而不是「只扫明细」：剪枝跑过之后明细里已经没有
	// 更早的记录，只扫明细会把终身累计重算成最近 30 天的数字 ——
	// 那是把「表 B 永久正确」这个存在理由直接推翻。
	//
	// pruned_through_day 为空时表 A 必然为空（归档与水位同事务写入），
	// 所以这个来源在两种状态下都对。
	//
	// ⚠️ src 下面被引用**十次**，而 args 只在末尾整体传一次。
	// 这只在 UsageFilter{} **零占位符**时正确 —— 十个子查询各自含 N 个 ?，
	// args 却只有一份，按顺序绑定时第 k 个子查询的占位符会绑到 args 的第
	// 1..N 位，于是**第二个及之后的子查询全部错绑**（读到别的值，或报
	// "column index out of range"）。
	//
	// 当前恰好成立：UsageFilter{} 的所有字段都是零值 → usageSource 不
	// append 任何参数。将来若给对账加过滤条件（例如只对账某 provider），
	// 下面十处引用必须同步改成各带一份 args，否则静默算错总额。
	// 这也是为什么不把 filter 参数化 —— 一旦参数化，这条约束立刻变成地雷。
	// 对账的口径必须是「全部」，否则终身累计本身就错了。
	src, args := UsageSource(UsageFilter{}) //nolint:staticcheck // 见上方：args 必须为零长度
	if _, err := s.db.ExecContext(ctx,
		`UPDATE usage_totals SET
		     request_count    = (SELECT `+UsageSumExpr("n", "u")+` FROM `+src+`),
		     success_count    = (SELECT COALESCE(SUM(u.n_ok), 0) FROM `+src+`),
		     error_count      = (SELECT COALESCE(SUM(u.n_err), 0) FROM `+src+`),
		     input_tokens     = (SELECT `+UsageSumExpr("input_tokens", "u")+` FROM `+src+`),
		     output_tokens    = (SELECT `+UsageSumExpr("output_tokens", "u")+` FROM `+src+`),
		     total_tokens     = (SELECT `+UsageSumExpr("total_tokens", "u")+` FROM `+src+`),
		     cached_tokens    = (SELECT `+UsageSumExpr("cached_tokens", "u")+` FROM `+src+`),
		     reasoning_tokens = (SELECT `+UsageSumExpr("reasoning_tokens", "u")+` FROM `+src+`),
		     cost_total       = (SELECT `+UsageSumExpr("cost_total", "u")+` FROM `+src+`),
		     latency_sum_ms   = (SELECT `+UsageSumExpr("latency_sum", "u")+` FROM `+src+`),
		     first_record_at  = (SELECT COALESCE(MIN(u.bucket_ts), 0) FROM `+src+`)
		   WHERE id = 1`, args...); err != nil {
		return fmt.Errorf("reconcile usage totals: %w", err)
	}
	return s.setSetting(ctx, settingUsageTotalsSeedKey, fmt.Sprintf("%d", time.Now().UnixMilli()))
}

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
	// 圈批规则与旧实现 DELETE 子查询里的一致：取**最老**的 maxPruneDelete 行
	//（按 id 排序，id 含时间前缀因而与 ts 单调），而不是「任意前 N 行」——
	// 保证处理的是**连续的一段**，不会在明细里打散出许多时间碎片影响后续统计。
	// 圈批到删除之间没有写入竞争：rowid 只会因本事务自己的 DELETE 而消失，
	// 而那发生在圈批的用途全部完成之后。
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO prune_batch (rid)
		     SELECT rowid FROM usage_records WHERE ts < ? ORDER BY id LIMIT ?`,
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
			     input_tokens, output_tokens, cached_tokens, reasoning_tokens, total_tokens,
			     cost_total, latency_sum_ms)
			 SELECT `+dayExpr+` AS day,
			       COALESCE(user_id, ''), access_key_id, public_model, upstream_model, provider_id, ingress_protocol, stream,
			       COUNT(*),
			       COUNT(CASE WHEN status = 'ok' THEN 1 END),
			       COUNT(CASE WHEN status NOT IN ('ok', 'canceled') THEN 1 END),
			       COALESCE(SUM(input_tokens), 0), COALESCE(SUM(output_tokens), 0),
			       COALESCE(SUM(cached_tokens), 0), COALESCE(SUM(reasoning_tokens), 0),
			       COALESCE(SUM(total_tokens), 0), COALESCE(SUM(cost_total), 0),
			       COALESCE(SUM(latency_ms), 0)
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
			       reasoning_tokens = reasoning_tokens + excluded.reasoning_tokens,
			       total_tokens     = total_tokens + excluded.total_tokens,
			       cost_total       = cost_total + excluded.cost_total,
			       latency_sum_ms   = latency_sum_ms + excluded.latency_sum_ms`); err != nil {
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
	RequestCount    int64
	SuccessCount    int64
	ErrorCount      int64
	InputTokens     int64
	OutputTokens    int64
	TotalTokens     int64
	CachedTokens    int64
	ReasoningTokens int64
	CostTotal       float64
	LatencySumMs    int64
	FirstRecordAt   int64
	PrunedThrough   string
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
		        total_tokens, cached_tokens, reasoning_tokens, cost_total, latency_sum_ms,
		        first_record_at, pruned_through_day
		   FROM usage_totals WHERE id = 1`).
		Scan(&t.RequestCount, &t.SuccessCount, &t.ErrorCount, &t.InputTokens, &t.OutputTokens,
			&t.TotalTokens, &t.CachedTokens, &t.ReasoningTokens, &t.CostTotal, &t.LatencySumMs,
			&t.FirstRecordAt, &t.PrunedThrough)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}
