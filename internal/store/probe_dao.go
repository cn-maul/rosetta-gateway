package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
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
// @ts-type ProbeTimelineBucket
type ProbeTimelineBucket struct {
	ModelID string `json:"model_id"` // 模型 id（多模型查询时必需）
	Ts      int64  `json:"ts"`       // 桶起点（毫秒）
	Count   int64  `json:"count"`
	OK      int64  `json:"ok"`
	Latency int64  `json:"latency"` // 平均延迟（毫秒）
}

// probeBucket 描述一种探测分桶粒度：把 ts 归到**本地时区**的桶起点。
//
// 2026-10-31 修（后端接口审计 #4）：这里原先用 `(ts / ?) * ?`，即按 UTC
// epoch 的整倍数分桶。小时桶在东八区恰好等于本地整点（偏移是整小时），
// 天桶却是 UTC 午夜＝本地 08:00 —— 与全仓其余日界口径分叉（dayExpr、
// usage_daily_rollups.day、前端 localDayKey 都是本地日界），于是「按天
// 用量」与「按天探测」两张图上同一天会落在不同的桶里。
//
// 改法：用 strftime 的 'localtime' 取本地桶键（与 dayExpr 同一套时区语义，
// 本地时区偏移/夏令时由 SQLite 自己算），再在 Go 侧把桶键解析回**本地**
// 桶起点毫秒 —— 与 usageSourceRollupCols 里 bucket_ts 取「当天本地午夜」
// 是同一个选法，两边可互相加总。
type probeBucket struct {
	expr   string // SQL 表达式：ts → 本地桶键（字符串）
	layout string // 桶键的 Go 时间布局，用来解析回毫秒
}

// probeBucketFor 按请求的桶宽选出分桶粒度。只支持按小时与按天（管理面
// /admin/api/probes/timeline 的 granularity 就这两种），其它宽度直接报错，
// 而不是悄悄退回 UTC 整倍数分桶 —— 那正是这次要消除的偏差。
func probeBucketFor(bucketMs int64) (probeBucket, error) {
	switch {
	case bucketMs >= 24*3600*1000:
		return probeBucket{`strftime('%Y-%m-%d', ts / 1000, 'unixepoch', 'localtime')`, "2006-01-02"}, nil
	case bucketMs >= 3600*1000:
		return probeBucket{`strftime('%Y-%m-%d %H', ts / 1000, 'unixepoch', 'localtime')`, "2006-01-02 15"}, nil
	}
	return probeBucket{}, fmt.Errorf("不支持的分桶宽度 %dms：探测时间轴只支持按小时或按天（本地时区对齐）", bucketMs)
}

// ProbeTimeline 返回给定模型在 [from,to] 内按 bucketMs 分桶的连通率。
//
// 时间轴页按小时/天画曲线用。桶内没有探测记录的行**不返回**（前端断开
// 折线），而不是填 0 —— 没探测 ≠ 全失败，两者必须可区分。
//
// 桶边界按**本地时区**对齐（见 probeBucket），返回的 Ts 是桶起点的毫秒
// 时间戳：小时桶＝本地整点，天桶＝本地午夜。
//
// modelIDs 支持**多个**：一次查询覆盖用户勾选的若干模型，返回值带
// ModelID 让前端分流。多模型时用 IN (...) 而不是循环逐个查 —— 前端
// 一次勾 6 个模型不该发 6 个请求，而 model_probes 上有
// (model_id, ts) 索引，IN 查询正是它擅长的形状。单个模型走等值分支，
// 让 SQLite 用上索引的最左前缀（IN (x) 与 = x 的计划可能不同）。
func (s *Store) ProbeTimeline(ctx context.Context, modelIDs []string, from, to, bucketMs int64) ([]ProbeTimelineBucket, error) {
	if len(modelIDs) == 0 {
		return nil, nil
	}
	bucket, err := probeBucketFor(bucketMs)
	if err != nil {
		return nil, err
	}
	// 桶键是本地日期/小时串，按字符串排序即按时间排序，所以 ORDER BY bucket
	// 与按毫秒排序等价（'2006-01-02' 与 '2006-01-02 15' 都是定宽零填充）。
	const queryTail = `
		   FROM model_probes
		  WHERE `
	var (
		rows *sql.Rows
		qerr error
	)
	// 单个模型：等值比较（索引最左前缀）。
	if len(modelIDs) == 1 {
		rows, qerr = s.read.QueryContext(ctx,
			`SELECT model_id, `+bucket.expr+` AS bucket, COUNT(*), SUM(ok), AVG(latency_ms)`+
				queryTail+`model_id = ? AND ts >= ? AND ts <= ?
			  GROUP BY model_id, bucket ORDER BY model_id, bucket`,
			modelIDs[0], from, to)
	} else {
		// 多模型：IN 查询。占位符按 len(modelIDs) 拼，不能写成固定个数。
		//
		// 参数绑定顺序必须**逐字匹配占位符在 SQL 中出现的顺序**：WHERE 里
		// model_id IN 的各 id 先，最后才是 ts >= ? / ts <= ?。早先把 from,to
		// 放在 id 前面，导致时间值被绑给了 model_id 占位符，查询恒返回 0 行
		// （且不报错，因为「时间戳恰好等于某个模型 id」永远不成立，是静默的
		// 空结果）。
		ph := strings.TrimSuffix(strings.Repeat("?,", len(modelIDs)), ",")
		args := make([]any, 0, len(modelIDs)+2)
		for _, id := range modelIDs {
			args = append(args, id)
		}
		args = append(args, from, to)
		rows, qerr = s.read.QueryContext(ctx,
			`SELECT model_id, `+bucket.expr+` AS bucket, COUNT(*), SUM(ok), AVG(latency_ms)`+
				queryTail+`model_id IN (`+ph+`) AND ts >= ? AND ts <= ?
			  GROUP BY model_id, bucket ORDER BY model_id, bucket`,
			args...)
	}
	if qerr != nil {
		return nil, qerr
	}
	defer rows.Close()

	out := make([]ProbeTimelineBucket, 0)
	for rows.Next() {
		var (
			b   ProbeTimelineBucket
			key string
			avg sql.NullFloat64
		)
		if err := rows.Scan(&b.ModelID, &key, &b.Count, &b.OK, &avg); err != nil {
			return nil, err
		}
		start, err := time.ParseInLocation(bucket.layout, key, time.Local)
		if err != nil {
			return nil, fmt.Errorf("解析探测分桶键 %q: %w", key, err)
		}
		b.Ts = start.UnixMilli()
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
