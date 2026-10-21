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
//
// 审计 *X4：数据源必须是 UsageSource（明细 ∪ 日归档），不能直接查
// usage_records。剪枝一跑，近期样本会从明细消失，而归档里的对应列
// （ok_output_tokens / ok_latency_sum_ms / ok_ttfb_sum_ms / ok_ttfb_count）正是
// 为这条查询保存的。没有归档支，统计会在剪枝后突然归零。
//
// 样本选择口径：
//   - 吞吐只取 status='ok' 且有正输出、正延迟的请求；分子分母都用同一批
//     样本（out_ok / lat_ok），与归档侧 ok_* 列同口径；
//   - 首字只取成功且 ttfb_ok_sum>0 的样本，分母用 n_ttfb_ok。
//
// LIMIT 仍在最内层先选最近 N 条；归档行是「一天×一维度」的聚合，按
// bucket_ts 排序，天然是日粒度。对刚剪枝的数据，这个窗口代表最近 N 个
// 可用聚合行，而不是伪造单请求样本。
func (s *Store) GetRecentPerformance(ctx context.Context, limit int, scopeUserID string) (RecentPerformance, error) {
	if limit <= 0 {
		limit = 5
	}
	src, args := UsageSource(UsageFilter{UserID: scopeUserID})

	params := append(append([]any{}, args...), limit)
	params = append(params, args...)
	params = append(params, limit)

	var result RecentPerformance
	err := s.read.QueryRowContext(ctx, `
		WITH throughput AS (
		 SELECT out_ok, lat_ok, n_ok_speed FROM `+src+`
		 WHERE out_ok > 0 AND lat_ok > 0
		 ORDER BY bucket_ts DESC, user_id DESC
		 LIMIT ?
		), first_token AS (
		 SELECT ttfb_ok_sum, n_ttfb_ok FROM `+src+`
		 WHERE ttfb_ok_sum > 0
		 ORDER BY bucket_ts DESC, user_id DESC
		 LIMIT ?
		)
		SELECT (SELECT COALESCE(SUM(out_ok)*1000.0/NULLIF(SUM(lat_ok),0),0) FROM throughput),
		       (SELECT COALESCE(SUM(n_ok_speed),0) FROM throughput),
		       (SELECT COALESCE(SUM(ttfb_ok_sum)*1.0/NULLIF(SUM(n_ttfb_ok),0),0) FROM first_token),
		       (SELECT COALESCE(SUM(n_ttfb_ok),0) FROM first_token)`, params...).Scan(
		&result.TokensPerSec, &result.ThroughputSamples, &result.TtfbMs, &result.TtfbSamples)
	return result, err
}
