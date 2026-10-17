package store

import "context"

type RecentPerformance struct {
	TokensPerSec      float64
	TtfbMs            float64
	ThroughputSamples int
	TtfbSamples       int
}

// GetRecentPerformance reads both metrics and their actual sample sizes in one
// query. Each metric has its own latest N eligible successful requests, scoped
// to the caller. These windows are independent of the dashboard date range.
func (s *Store) GetRecentPerformance(ctx context.Context, limit int, scopeUserID string) (RecentPerformance, error) {
	if limit <= 0 {
		limit = 5
	}
	cond, args := scopeClause("", scopeUserID)
	params := append(append([]any{}, args...), limit)
	params = append(params, args...)
	params = append(params, limit)
	var result RecentPerformance
	err := s.read.QueryRowContext(ctx, `
		WITH throughput AS (
		 SELECT output_tokens, latency_ms FROM usage_records
		 WHERE status = 'ok' AND output_tokens > 0 AND latency_ms > 0`+cond+`
		 ORDER BY ts DESC, id DESC LIMIT ?
		), first_token AS (
		 SELECT ttfb_ms FROM usage_records
		 WHERE status = 'ok' AND ttfb_ms > 0`+cond+`
		 ORDER BY ts DESC, id DESC LIMIT ?
		)
		SELECT (SELECT COALESCE(SUM(output_tokens)*1000.0/NULLIF(SUM(latency_ms),0),0) FROM throughput),
		       (SELECT COUNT(*) FROM throughput),
		       (SELECT COALESCE(AVG(ttfb_ms),0) FROM first_token),
		       (SELECT COUNT(*) FROM first_token)`, params...).Scan(
		&result.TokensPerSec, &result.ThroughputSamples, &result.TtfbMs, &result.TtfbSamples)
	return result, err
}
