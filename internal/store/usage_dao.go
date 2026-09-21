package store

import (
	"context"
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

func (s *Store) CreateUsageRecord(ctx context.Context, r *UsageRecord) error {
	now := time.Now().UnixMilli()
	stream := 0
	if r.Stream {
		stream = 1
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO usage_records (id, ts, access_key_id, public_model, provider_id, upstream_model, ingress_protocol, stream, input_tokens, output_tokens, total_tokens, reasoning_tokens, cached_tokens, usage_state, status, http_status, error_code, latency_ms, ttfb_ms, request_id) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ID, now, r.AccessKeyID, r.PublicModel, r.ProviderID, r.UpstreamModel, r.IngressProtocol, stream, r.InputTokens, r.OutputTokens, r.TotalTokens, r.ReasoningTokens, r.CachedTokens, r.UsageState, r.Status, r.HTTPStatus, r.ErrorCode, r.LatencyMs, r.TTFBMs, r.RequestID)
	return err
}

type UsageStats struct {
	TotalRequests int64
	TotalTokens   int64
	InputTokens   int64
	OutputTokens  int64
	CachedTokens  int64
	ErrorCount    int64
}

func (s *Store) GetUsageStats(ctx context.Context) (*UsageStats, error) {
	var stats UsageStats
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*), COALESCE(SUM(total_tokens), 0), COALESCE(SUM(input_tokens), 0), COALESCE(SUM(output_tokens), 0), COALESCE(SUM(cached_tokens), 0), COUNT(CASE WHEN status != 'ok' THEN 1 END) FROM usage_records`).
		Scan(&stats.TotalRequests, &stats.TotalTokens, &stats.InputTokens, &stats.OutputTokens, &stats.CachedTokens, &stats.ErrorCount)
	if err != nil {
		return nil, err
	}
	return &stats, nil
}

// CacheHitRate 返回缓存命中率（0~1），口径为「缓存读取输入 token / 总输入 token」。
//
// 分母用 input_tokens 而非 total_tokens：缓存命中衡量的是「输入侧有多少走了缓存」，
// 输出 token 与缓存无关，计入分母只会稀释指标。
//
// 两个上游协议的语义已由 rosetta SDK 统一（这正是该比值恒 ≤ 1 的前提）：
//   - openai-chat：prompt_tokens_details.cached_tokens ⊆ prompt_tokens
//   - anthropic：SDK 把 cache_read 与 cache_creation 一并折进 input_tokens
//     （见 provider_anthropic.go 的 anthroUsage.toUsage），因此同样 ⊆
//
// 全部记录参与统计（与同组其它指标口径一致）：失败记录通常没有 usage、
// input_tokens 为 0，对分子分母都没有贡献；截断记录（truncated）的输入是真实
// 发生过的，应当计入。无输入样本（input_tokens == 0）时返回 0，由调用方决定
// 展示为「—」还是 0%。
func (s *Store) CacheHitRate(ctx context.Context) (float64, error) {
	var v float64
	err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(cached_tokens) * 1.0 / NULLIF(SUM(input_tokens), 0), 0)
		   FROM usage_records`).
		Scan(&v)
	if err != nil {
		return 0, err
	}
	return v, nil
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

// ListModelThroughput 返回某上游下每个 model_id 的近期速度（近 5 次）与成功率（近 100 次）。
// 用窗口函数按模型分区取最近 N 条：速度只统计成功且有输出/耗时的记录，
// 成功率 = 最近 100 次里 status='ok' 的占比。未调用过的模型不出现在结果里。
func (s *Store) ListModelThroughput(ctx context.Context, providerID string) (map[string]ModelStat, error) {
	rows, err := s.db.QueryContext(ctx,
		`WITH base AS (
		   SELECT upstream_model, ts, output_tokens, latency_ms, ttfb_ms,
		          CASE WHEN status = 'ok' THEN 1 ELSE 0 END AS is_ok,
		          CASE WHEN status = 'ok' AND output_tokens > 0 AND latency_ms > 0 THEN 1 ELSE 0 END AS is_meas,
		          CASE WHEN status = 'ok' AND ttfb_ms > 0 THEN 1 ELSE 0 END AS is_ttfb
		     FROM usage_records
		    WHERE provider_id = ?
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
		  GROUP BY upstream_model`, providerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[string]ModelStat)
	for rows.Next() {
		var model string
		var st ModelStat
		if err := rows.Scan(&model, &st.Tps, &st.SuccessRate, &st.SuccessSample, &st.TtfbMs); err != nil {
			continue
		}
		out[model] = st
	}
	return out, rows.Err()
}
