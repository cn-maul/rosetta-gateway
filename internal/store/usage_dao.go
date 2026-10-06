package store

import (
	"context"
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
	// 空串 = 无归属 key（user_id 为 NULL）产生的用量，即迁移前遗留的历史记录。
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
	UsageState      string
	Status          string
	HTTPStatus      int
	ErrorCode       string
	LatencyMs       int64
	TTFBMs          int64
	RequestID       string
}

// CreateUsageRecord 落一条用量记录。
//
// Ts 为 0 时取当前时刻；非 0 则用它。此前该字段被**整个忽略**、恒写 time.Now()，
// 于是任何显式带 Ts 的插入都静默落在「现在」—— 回填历史数据时整条时间线错位，
// 而且因为字段名和列名都叫 ts，看不出哪里错了。
func (s *Store) CreateUsageRecord(ctx context.Context, r *UsageRecord) error {
	ts := r.Ts
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
	cost := s.freezeUsageCost(ctx, r)
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO usage_records (id, ts, access_key_id, user_id, public_model, provider_id, upstream_model, ingress_protocol, stream, input_tokens, output_tokens, total_tokens, reasoning_tokens, cached_tokens, usage_state, status, http_status, error_code, latency_ms, ttfb_ms, request_id, cost_total) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ID, ts, r.AccessKeyID, nullIfEmpty(r.UserID), r.PublicModel, r.ProviderID, r.UpstreamModel, r.IngressProtocol, stream, r.InputTokens, r.OutputTokens, r.TotalTokens, r.ReasoningTokens, r.CachedTokens, r.UsageState, r.Status, r.HTTPStatus, nullIfEmpty(r.ErrorCode), r.LatencyMs, r.TTFBMs, nullIfEmpty(r.RequestID), cost)
	return err
}

// freezeUsageCost 算出一条用量的固化费用（元）。
//
// 口径与原 GetUsageStats 的 JOIN 重算逐字一致（单价「元 / 百万 tokens」，
// NULL 按 0）：
//   - 缓存未命中输入 = MAX(input - cached, 0) × price_input；
//   - 缓存命中输入   = cached × price_cache_hit，未单独配置时回退 price_input
//     （只填输入价的模型，命中部分不会被算成免费）；
//   - 输出           = output × price_output；
//   - 未配置价格的模型贡献 0。
//
// **查价失败一律返回 0，不返回错误、不阻断落库**：用量是硬需求（配额、限速、
// 审计都靠它），费用不是。模型不存在、没配价、读池抖动，后果都只是这一条
// 记录计 0 元 —— 与「未配价 = 0」的既有口径同语义。让费用查询有能力打死
// 用量落库，是把 nice-to-have 接到了 critical path 上。
func (s *Store) freezeUsageCost(ctx context.Context, r *UsageRecord) float64 {
	var pin, phit, pout float64
	err := s.read.QueryRowContext(ctx,
		`SELECT COALESCE(price_input, 0), COALESCE(price_cache_hit, 0), COALESCE(price_output, 0)
		   FROM upstream_models WHERE provider_id = ? AND model_id = ?`,
		r.ProviderID, r.UpstreamModel).Scan(&pin, &phit, &pout)
	if err != nil {
		return 0
	}
	uncached := r.InputTokens - r.CachedTokens
	if uncached < 0 {
		uncached = 0
	}
	hit := phit
	if hit <= 0 {
		hit = pin
	}
	return (float64(uncached)*pin + float64(r.CachedTokens)*hit + float64(r.OutputTokens)*pout) / 1_000_000.0
}

type UsageStats struct {
	TotalRequests int64
	TotalTokens   int64
	InputTokens   int64
	OutputTokens  int64
	CachedTokens  int64
	ErrorCount    int64
	// CacheHitRate 是缓存命中率（0~1）：cached_tokens / input_tokens。
	// 分母用 input_tokens 而非 total_tokens —— 缓存命中衡量的是输入侧，
	// 输出 token 与缓存无关，计入分母只会稀释指标。分子分母的语义前提：
	// 两个上游协议的 cached 都 ⊆ input（openai-chat 的
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
}

// GetUsageStats 返回累计统计。from/to 为毫秒时间戳，0 表示不设该边界
// （from=0 即统计全部历史）。ErrorCount 排除 canceled：那是「客户端主动断开」，
// 既不是上游故障也不是本网关的失败，算进错误率只会让成功率虚低。
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
		        COALESCE(SUM(u.n_err), 0),
		        COALESCE(SUM(u.cached_tokens) * 1.0 / NULLIF(SUM(u.input_tokens), 0), 0),
		        `+UsageSumExpr("cost_total", "u")+`
		   FROM `+src,
		args...).
		Scan(&stats.TotalRequests, &stats.TotalTokens, &stats.InputTokens, &stats.OutputTokens, &stats.CachedTokens, &stats.ErrorCount, &stats.CacheHitRate, &stats.Cost)
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
	// 窗口过滤必须在这里自己下发给 day：不能借用 UsageSource（它的 day 是
	// **本地**日界，与这里的 UTC 日界不是同一套，借过来会把边缘那天算漏或算重）。
	// 上界用「该 UTC 日的结束」、下界用「该 UTC 日的开始」，与明细支的
	// ts >= ? AND ts <= ? 同一个语义。
	const rollupPart = `SELECT date(CAST(strftime('%s', day, 'utc') AS INTEGER), 'unixepoch') AS day,
	                            total_tokens
	                      FROM usage_daily_rollups
	                     WHERE access_key_id = ?
	                       AND day >= date(?, 'unixepoch', 'localtime')
	                       AND day <= date(?, 'unixepoch', 'localtime')`
	rows, err := s.read.QueryContext(ctx,
		`SELECT x.day, COALESCE(SUM(x.total_tokens), 0)
		   FROM (`+detailPart+`
		         UNION ALL `+rollupPart+`) x
		  GROUP BY x.day ORDER BY x.day`,
		keyID, from, to, keyID, from/1000, to/1000)
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
