package store

import (
	"context"
	"database/sql"
	"time"
)

// ProbeRecord 是一次上游模型探测的结果。
//
// 与 usage_records 完全隔离（见 store.go model_probes 建表注释）：
// 探测结果只进本表，绝不进 UsageSource / 配额 / 余额。
type ProbeRecord struct {
	ModelID     string
	ProviderID  string
	Ts          int64
	OK          bool
	InBand      bool
	LatencyMs   int64
	ErrorKind   string
	ErrorDetail string
}

// RecordProbe 落一条探测结果。
func (s *Store) RecordProbe(ctx context.Context, r ProbeRecord) error {
	ok := 0
	if r.OK {
		ok = 1
	}
	inBand := 0
	if r.InBand {
		inBand = 1
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO model_probes (model_id, provider_id, ts, ok, in_band, latency_ms, error_kind, error_detail)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ModelID, r.ProviderID, r.Ts, ok, inBand, r.LatencyMs,
		nullIfEmpty(r.ErrorKind), nullIfEmpty(r.ErrorDetail))
	return err
}

// ProbeHealth 是某个模型在窗口内的健康摘要。
type ProbeHealth struct {
	ModelID    string
	ProviderID string
	// WindowCount / WindowOK 是窗口内探测次数与成功次数（连通率 = OK/Count）。
	WindowCount int64
	WindowOK    int64
	// ConsecutiveFail 是当前连续失败次数（从最近一次成功之后数起）。
	ConsecutiveFail int64
	// LastOK 最近一次是否成功。
	LastOK bool
	// LastTs 最近一次探测时间（毫秒）。0 = 从未探测。
	LastTs int64
	// LastLatencyMs 最近一次往返耗时。
	LastLatencyMs int64
	// LastErrorKind / LastErrorDetail 最近一次失败的原因（最近成功则空）。
	LastErrorKind   string
	LastErrorDetail string
}

// ProbeHealthSummary 返回全部模型在窗口内的健康摘要。
//
// 一次查询拿全（LEFT JOIN 到模型表，未探测的模型也列出 —— 总览页要能
// 显示「还没探测过」而不是凭空消失）。窗口由调用方传毫秒时间戳。
func (s *Store) ProbeHealthSummary(ctx context.Context, windowMs int64) ([]ProbeHealth, error) {
	cutoff := time.Now().UnixMilli() - windowMs
	rows, err := s.read.QueryContext(ctx,
		`SELECT um.id, um.provider_id,
		        COALESCE(w.cnt, 0), COALESCE(w.okcnt, 0),
		        COALESCE(w.consec_fail, 0),
		        COALESCE(w.last_ok, 0),
		        COALESCE(w.last_ts, 0),
		        COALESCE(l.latency_ms, 0), COALESCE(l.error_kind, ''), COALESCE(l.error_detail, '')
		   FROM upstream_models um
		   LEFT JOIN (
		     SELECT model_id,
		            COUNT(*) AS cnt,
		            SUM(ok) AS okcnt,
		            -- 连续失败：从最近一次成功之后数。窗口内没有成功 → 全部算失败。
		            (SELECT COUNT(*) FROM model_probes p2
		              WHERE p2.model_id = p.model_id AND p2.ts >= ? AND p2.ts > COALESCE(
		                (SELECT MAX(ts) FROM model_probes p3
		                  WHERE p3.model_id = p.model_id AND p3.ok = 1 AND p3.ts >= ?), 0)) AS consec_fail,
		            -- last_ok 必须取**最近一次探测**的 ok，不能用 MAX(ok)：
		            -- MAX(ok) 只要窗口内有任意一次成功就为真，而「最近失败、
		            -- 但窗口里之前成功过」恰恰是最该正确报告的形态
		            -- （现在挂了 ≠ 一直好）。
		            (SELECT ok FROM model_probes p8
		              WHERE p8.model_id = p.model_id AND p8.ts = (
		                SELECT MAX(ts) FROM model_probes p9
		                 WHERE p9.model_id = p.model_id AND p9.ts >= ?)) AS last_ok,
		            MAX(ts) AS last_ts
		     FROM model_probes p
		     WHERE p.ts >= ?
		     GROUP BY p.model_id
		   ) w ON w.model_id = um.id
		   LEFT JOIN model_probes l ON l.id = (
		     SELECT id FROM model_probes p7
		      WHERE p7.model_id = um.id
		      ORDER BY p7.ts DESC LIMIT 1
		   )`,
		cutoff, cutoff, cutoff, cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]ProbeHealth, 0)
	for rows.Next() {
		var h ProbeHealth
		var lastOK int
		if err := rows.Scan(&h.ModelID, &h.ProviderID,
			&h.WindowCount, &h.WindowOK, &h.ConsecutiveFail,
			&lastOK, &h.LastTs, &h.LastLatencyMs,
			&h.LastErrorKind, &h.LastErrorDetail); err != nil {
			return nil, err
		}
		h.LastOK = lastOK == 1
		out = append(out, h)
	}
	return out, rows.Err()
}

// ProbeTimelineBucket 是时间轴的一个分桶（某模型某时间窗的连通率）。
type ProbeTimelineBucket struct {
	Ts      int64 // 桶起点（毫秒）
	Count   int64
	OK      int64
	Latency int64 // 平均延迟（毫秒）
}

// ProbeTimeline 返回某模型在 [from,to] 内按 bucketMs 分桶的连通率。
//
// 时间轴页按小时/天画热力图用。桶内没有探测记录的行**不返回**（前端画空白
// 格），而不是填 0 —— 没探测 ≠ 全失败，两者必须可区分。
func (s *Store) ProbeTimeline(ctx context.Context, modelID string, from, to, bucketMs int64) ([]ProbeTimelineBucket, error) {
	rows, err := s.read.QueryContext(ctx,
		`SELECT (ts / ?) * ? AS bucket, COUNT(*), SUM(ok), AVG(latency_ms)
		   FROM model_probes
		  WHERE model_id = ? AND ts >= ? AND ts <= ?
		  GROUP BY bucket ORDER BY bucket`,
		bucketMs, bucketMs, modelID, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]ProbeTimelineBucket, 0)
	for rows.Next() {
		var b ProbeTimelineBucket
		var avg sql.NullFloat64
		if err := rows.Scan(&b.Ts, &b.Count, &b.OK, &avg); err != nil {
			return nil, err
		}
		if avg.Valid {
			b.Latency = int64(avg.Float64)
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// PruneOldProbes 删除超期探测记录，返回删除行数。
//
// 与 usage 的 PruneOldUsage 分开：探测表的保留期独立配置（默认 30 天），
// 不该被 usage 的保留期配置牵着走。
func (s *Store) PruneOldProbes(ctx context.Context, keepDays int) (int64, error) {
	if keepDays <= 0 {
		return 0, nil
	}
	cutoff := time.Now().AddDate(0, 0, -keepDays).UnixMilli()
	res, err := s.db.ExecContext(ctx, `DELETE FROM model_probes WHERE ts < ?`, cutoff)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	return n, nil
}
