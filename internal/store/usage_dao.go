package store

import (
	"context"
	"strings"
	"time"
)

type UsageRecord struct {
	ID              string
	Ts              int64
	AccessKeyID     string
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
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO usage_records (id, ts, access_key_id, public_model, provider_id, upstream_model, ingress_protocol, stream, input_tokens, output_tokens, total_tokens, reasoning_tokens, cached_tokens, usage_state, status, http_status, error_code, latency_ms, ttfb_ms, request_id) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ID, ts, r.AccessKeyID, r.PublicModel, r.ProviderID, r.UpstreamModel, r.IngressProtocol, stream, r.InputTokens, r.OutputTokens, r.TotalTokens, r.ReasoningTokens, r.CachedTokens, r.UsageState, r.Status, r.HTTPStatus, nullIfEmpty(r.ErrorCode), r.LatencyMs, r.TTFBMs, nullIfEmpty(r.RequestID))
	return err
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
	// Cost 是费用（元），按**当前** upstream_models 里的单价实时计算
	// （改价后历史区间统计随之变化，usage_records 不固化金额）。
	Cost float64
}

// GetUsageStats 返回累计统计。from/to 为毫秒时间戳，0 表示不设该边界
// （from=0 即统计全部历史）。ErrorCount 排除 canceled：那是「客户端主动断开」，
// 既不是上游故障也不是本网关的失败，算进错误率只会让成功率虚低。
//
// 费用口径（单价来自 upstream_models，均为「元 / 百万 tokens」）：
//   - 缓存未命中的输入 = MAX(input - cached, 0) × price_input；
//   - 缓存命中的输入   = cached × price_cache_hit，未单独配置时回退 price_input
//     （只填了输入价的模型，缓存命中部分不会被错算成免费）；
//   - 输出             = output × price_output；
//   - 未配置价格的模型（三价皆空/0）贡献 0，不会污染总额。
//
// LEFT JOIN 按 (provider_id, upstream_model) 匹配：usage_records 里那对字段
// 正是上游模型的自然键，记录的模型已从库里删掉时退化为 0 而不是让统计整体失败。
// JOIN 至多 1:1（provider_id+model_id 在 upstream_models 上 UNIQUE），
// 因此缓存命中率可在同一条 SELECT 里一并聚合，不会因行复制而失真。
func (s *Store) GetUsageStats(ctx context.Context, from, to int64) (*UsageStats, error) {
	var stats UsageStats
	where, args := timeRangeClause(from, to, "u.")
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*), COALESCE(SUM(u.total_tokens), 0), COALESCE(SUM(u.input_tokens), 0), COALESCE(SUM(u.output_tokens), 0), COALESCE(SUM(u.cached_tokens), 0),
		        COUNT(CASE WHEN u.status NOT IN ('ok', 'canceled') THEN 1 END),
		        COALESCE(SUM(u.cached_tokens) * 1.0 / NULLIF(SUM(u.input_tokens), 0), 0),
		        COALESCE(SUM(
		           MAX(u.input_tokens - u.cached_tokens, 0) * COALESCE(m.price_input, 0)
		           + u.cached_tokens * (CASE WHEN COALESCE(m.price_cache_hit, 0) > 0 THEN m.price_cache_hit ELSE COALESCE(m.price_input, 0) END)
		           + u.output_tokens * COALESCE(m.price_output, 0)
		        ) / 1000000.0, 0)
		   FROM usage_records u
		   LEFT JOIN upstream_models m ON m.provider_id = u.provider_id AND m.model_id = u.upstream_model`+where,
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
func (s *Store) GetRecentThroughput(ctx context.Context, limit int) (float64, error) {
	if limit <= 0 {
		limit = 50
	}
	var v float64
	err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(output_tokens) * 1000.0 / NULLIF(SUM(latency_ms), 0), 0)
		   FROM (SELECT output_tokens, latency_ms FROM usage_records
		          WHERE status = 'ok' AND output_tokens > 0 AND latency_ms > 0
		          ORDER BY ts DESC LIMIT ?)`, limit).
		Scan(&v)
	if err != nil {
		return 0, err
	}
	return v, nil
}

// GetRecentTtfbMs 返回最近若干次成功调用的平均首字延迟（毫秒）。
// 只统计 status='ok' 且 ttfb_ms>0（真正流式、拿到过首字）的记录，取算术平均；
// 无样本时返回 0。与 GetRecentThroughput 口径一致（同一「近 N 次」窗口）。
func (s *Store) GetRecentTtfbMs(ctx context.Context, limit int) (float64, error) {
	if limit <= 0 {
		limit = 5
	}
	var v float64
	err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(AVG(ttfb_ms), 0)
		   FROM (SELECT ttfb_ms FROM usage_records
		          WHERE status = 'ok' AND ttfb_ms > 0
		          ORDER BY ts DESC LIMIT ?)`, limit).
		Scan(&v)
	if err != nil {
		return 0, err
	}
	return v, nil
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
	rows, err := s.db.QueryContext(ctx,
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
