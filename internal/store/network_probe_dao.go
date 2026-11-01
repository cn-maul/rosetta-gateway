package store

import (
	"context"
	"database/sql"
	"math"
	"time"
)

// NetworkProbe 是一次代理连通性探测的结果。
//
// 与 model_probes 分开一张表（理由见 schema.go 建表注释）：这里探的是
// 「这条代理能不能出去」，不打上游、不计费，粒度是代理而不是模型。
type NetworkProbe struct {
	NetworkID string
	Ts        int64
	OK        bool
	LatencyMs int64
	Error     string
}

// RecordNetworkProbe 落一条代理探测结果。
func (s *Store) RecordNetworkProbe(ctx context.Context, p NetworkProbe) error {
	ok := 0
	if p.OK {
		ok = 1
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO network_probes (network_id, ts, ok, latency_ms, error)
		 VALUES (?, ?, ?, ?, ?)`,
		p.NetworkID, p.Ts, ok, p.LatencyMs, nullIfEmpty(p.Error))
	return err
}

// NetworkLatency 是某条代理在窗口内的测速摘要。
type NetworkLatency struct {
	NetworkID string
	// Samples 是窗口内**成功**样本数。0 表示这段时间没测通过（或还没测过）。
	Samples int64
	AvgMs   int64
	P95Ms   int64
	MaxMs   int64
	// LastOK 是最近一次探测是否成功（窗口内一条成功样本都没有时更有用：
	// 界面要能说清「上次就失败了」而不是只显示「—」）。
	LastOK bool
	// LastError 是最近一次失败的原因（最近一次成功则为空）。
	LastError string
	// LastTs 是最近一次探测时间（毫秒），0 = 从未探测。
	LastTs int64
}

// NetworkLatencyByID 返回窗口内各代理的测速摘要。
//
// 只统计**成功且耗时非 0** 的样本做平均：失败那条的 latency 是「等到超时
// 才放弃」的时长，混进平均会让一条连不通的代理显示成最快的那条 —— 与
// ProbeLatencyByProvider 同一取舍。
//
// 一次查询拿全（GROUP BY network_id），P95 再逐条补：窗口内单条代理的
// 样本量在几十条量级，一条 ORDER BY 的代价可以忽略。
func (s *Store) NetworkLatencyByID(ctx context.Context, windowMs int64) (map[string]NetworkLatency, error) {
	cutoff := time.Now().UnixMilli() - windowMs
	rows, err := s.read.QueryContext(ctx,
		`SELECT network_id,
		        COUNT(*),
		        COALESCE(AVG(latency_ms), 0),
		        COALESCE(MAX(latency_ms), 0),
		        MAX(ts)
		   FROM network_probes
		  WHERE ts >= ? AND ok = 1 AND latency_ms > 0
		  GROUP BY network_id`,
		cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[string]NetworkLatency)
	for rows.Next() {
		var (
			l    NetworkLatency
			avg  sql.NullFloat64
			last sql.NullInt64
		)
		if err := rows.Scan(&l.NetworkID, &l.Samples, &avg, &l.MaxMs, &last); err != nil {
			return nil, err
		}
		if avg.Valid {
			l.AvgMs = int64(math.Round(avg.Float64))
		}
		if last.Valid {
			l.LastTs = last.Int64
		}
		out[l.NetworkID] = l
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// 最近一次探测的成败：即使在失败中也要能显示原因，否则界面上只有
	// 一个「—」，看不出是「没测过」还是「一直在失败」。
	lastRows, err := s.read.QueryContext(ctx,
		`SELECT network_id, ok, COALESCE(error, ''), ts
		   FROM network_probes p
		  WHERE ts = (SELECT MAX(ts) FROM network_probes x WHERE x.network_id = p.network_id)`)
	if err != nil {
		return nil, err
	}
	defer lastRows.Close()
	for lastRows.Next() {
		var (
			id  string
			ok  int
			msg string
			ts  int64
		)
		if err := lastRows.Scan(&id, &ok, &msg, &ts); err != nil {
			return nil, err
		}
		entry := out[id]
		entry.NetworkID = id
		entry.LastOK = ok == 1
		entry.LastError = msg
		entry.LastTs = ts
		out[id] = entry
	}
	if err := lastRows.Err(); err != nil {
		return nil, err
	}

	for id, l := range out {
		p95, perr := s.networkP95Latency(ctx, cutoff, id)
		if perr != nil {
			return nil, perr
		}
		l.P95Ms = p95
		out[id] = l
	}
	return out, nil
}

// networkP95Latency 取某代理窗口内成功探测延迟的第 95 百分位。
//
// 与 probeP95Latency 同一手法：用 OFFSET 直接取那条样本，不把全部延迟
// 读进内存排序。
func (s *Store) networkP95Latency(ctx context.Context, cutoff int64, networkID string) (int64, error) {
	var n int64
	if err := s.read.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM network_probes
		  WHERE ts >= ? AND ok = 1 AND latency_ms > 0 AND network_id = ?`,
		cutoff, networkID).Scan(&n); err != nil {
		return 0, err
	}
	if n == 0 {
		return 0, nil
	}
	rank := (95*n + 99) / 100
	if rank < 1 {
		rank = 1
	}
	if rank > n {
		rank = n
	}
	var v int64
	if err := s.read.QueryRowContext(ctx,
		`SELECT latency_ms FROM network_probes
		  WHERE ts >= ? AND ok = 1 AND latency_ms > 0 AND network_id = ?
		  ORDER BY latency_ms ASC LIMIT 1 OFFSET ?`,
		cutoff, networkID, rank-1).Scan(&v); err != nil {
		return 0, err
	}
	return v, nil
}

// PruneOldNetworkProbes 删除超期代理探测记录，返回删除行数。
//
// 保留期与 model_probes 共用 probes.retention_days：两者都是「排障线索」，
// 分成两个配置项只会让人忘掉其中一个。
func (s *Store) PruneOldNetworkProbes(ctx context.Context, keepDays int) (int64, error) {
	if keepDays <= 0 {
		return 0, nil
	}
	cutoff := time.Now().AddDate(0, 0, -keepDays).UnixMilli()
	res, err := s.db.ExecContext(ctx, `DELETE FROM network_probes WHERE ts < ?`, cutoff)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
