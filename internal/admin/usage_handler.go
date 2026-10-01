package admin

import (
	"database/sql"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/cn-maul/rosetta-gateway/internal/store"
)

type UsageHandler struct {
	store *store.Store
}

func NewUsageHandler(st *store.Store) *UsageHandler {
	return &UsageHandler{store: st}
}

// 用量查询的分页口径：默认 100，上限 1000（与 History 一致）。
const (
	defaultUsageLimit = 100
	maxUsageLimit     = 1000
)

// clampLimit 解析并钳制 limit。
//
// 必须钳制：SQLite 的 `LIMIT -1` 语义是「不限制」，而 strconv.ParseInt("-1")
// 恰好得到 -1 —— `/admin/api/usage?limit=-1` 会静默拉全表，
// `limit=999999999` 同理把整表物化进内存。同一个不变量在 History 里已经做对了，
// 这里是漏掉的那一处。
func clampLimit(raw string, def, max int64) int64 {
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n <= 0 {
		return def
	}
	if n > max {
		return max
	}
	return n
}

// queryRange 解析 from/to（毫秒时间戳），缺省取「最近 window」。
func queryRange(q url.Values, window time.Duration) (from, to int64) {
	from, _ = strconv.ParseInt(q.Get("from"), 10, 64)
	to, _ = strconv.ParseInt(q.Get("to"), 10, 64)
	if from == 0 {
		from = time.Now().Add(-window).UnixMilli()
	}
	if to == 0 {
		to = time.Now().UnixMilli()
	}
	return from, to
}

// queryRangeExplicit 解析 from/to，并区分「显式传 from=0（=全部历史）」与
// 「未传 from（=取默认 window）」。总览页的「全部」档需要前者。
// 返回 (from, to, explicit)；explicit 为 true 表示请求里带了 from 参数。
func queryRangeExplicit(q url.Values, window time.Duration) (from, to int64, explicit bool) {
	rawFrom := q.Get("from")
	from, _ = strconv.ParseInt(rawFrom, 10, 64)
	to, _ = strconv.ParseInt(q.Get("to"), 10, 64)
	explicit = rawFrom != ""
	if !explicit && from == 0 {
		from = time.Now().Add(-window).UnixMilli()
	}
	if to == 0 {
		to = time.Now().UnixMilli()
	}
	return from, to, explicit
}

// groupRangeClause 构造分组查询的 ts 过滤子句。from=0 且 explicit 时表示
// 「全部历史」（不设下界）；否则按 from/to 过滤。
// prefix 是 ts 列的限定前缀（如 "u."）；无 JOIN 的查询传空串。
func groupRangeClause(from, to int64, explicit bool, prefix string) (string, []any) {
	var conds []string
	var args []any
	if !(explicit && from == 0) {
		conds = append(conds, prefix+"ts >= ?")
		args = append(args, from)
	}
	conds = append(conds, prefix+"ts <= ?")
	args = append(args, to)
	return " WHERE " + strings.Join(conds, " AND "), args
}

// usageFilter 构造明细查询与汇总查询**共用**的 WHERE 子句。
//
// 只构造一次、两处复用。分开拼两份迟早会漂移 —— 旧实现的 group_by 分支就直接
// `args = []any{from, to}` 把 key_id / model / provider_id 三个过滤条件整体丢了，
// 于是「按密钥筛选 + 按天分组」会静默返回全量数据。
func usageFilter(from, to int64, q url.Values) (where string, args []any) {
	where = ` WHERE ts >= ? AND ts <= ?`
	args = []any{from, to}
	for _, f := range []struct{ param, col string }{
		{"key_id", "access_key_id"},
		{"model", "public_model"},
		{"provider_id", "provider_id"},
	} {
		if v := q.Get(f.param); v != "" {
			where += ` AND ` + f.col + ` = ?`
			args = append(args, v)
		}
	}
	return where, args
}

// usageRecordColumns 是 usageRecordEntry 对应的列顺序，明细查询与分组查询共用。
const usageRecordColumns = `ts, public_model, provider_id, upstream_model, ingress_protocol, stream, ` +
	`input_tokens, output_tokens, total_tokens, status, latency_ms`

// groupByClause 返回按 group_by 维度聚合时的 SELECT 列与 GROUP BY 子句。
//
// 关键约束：SELECT 里**不能**出现任何非聚合裸列。旧实现图省事把
// `ts, public_model, provider_id, ...` 原样留着、只追加一个 GROUP BY ——
// SQLite 对此不报错，而是每个分组取「某一行」的值，结果不确定（换个查询计划就变）。
// 这里所有非分组列一律给确定的中性值。
func groupByClause(groupBy string) (selectCols, groupClause string, ok bool) {
	const blank = `'' as public_model, '' as provider_id, '' as upstream_model, ` +
		`'' as ingress_protocol, 0 as stream`
	const agg = `SUM(input_tokens) as input_tokens, SUM(output_tokens) as output_tokens, ` +
		`SUM(total_tokens) as total_tokens, '' as status, AVG(latency_ms) as latency_ms`

	switch groupBy {
	case "day":
		// (ts / 86400000) * 86400000 = 当天 00:00 的毫秒时间戳（UTC）。
		return `(ts / 86400000) * 86400000 as ts, ` + blank + `, ` + agg, ` GROUP BY (ts / 86400000)`, true
	case "key":
		return `0 as ts, ` + blank + `, ` + agg, ` GROUP BY access_key_id`, true
	case "model":
		return `0 as ts, ` + blank + `, ` + agg, ` GROUP BY public_model`, true
	case "provider":
		return `0 as ts, ` + blank + `, ` + agg, ` GROUP BY provider_id`, true
	}
	return "", "", false
}

type usageQueryResponse struct {
	Records []usageRecordEntry `json:"records"`
	Summary usageSummary       `json:"summary"`
}

type usageRecordEntry struct {
	Ts              int64  `json:"ts"`
	PublicModel     string `json:"public_model"`
	ProviderID      string `json:"provider_id"`
	UpstreamModel   string `json:"upstream_model"`
	IngressProtocol string `json:"ingress_protocol"`
	Stream          bool   `json:"stream"`
	InputTokens     int64  `json:"input_tokens"`
	OutputTokens    int64  `json:"output_tokens"`
	TotalTokens     int64  `json:"total_tokens"`
	Status          string `json:"status"`
	LatencyMs       int64  `json:"latency_ms"`
}

type usageSummary struct {
	TotalRequests int64 `json:"total_requests"`
	TotalTokens   int64 `json:"total_tokens"`
	InputTokens   int64 `json:"input_tokens"`
	OutputTokens  int64 `json:"output_tokens"`
	ErrorCount    int64 `json:"error_count"`
	AvgLatencyMs  int64 `json:"avg_latency_ms"`
}

func (h *UsageHandler) Query(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	from, to := queryRange(q, 24*time.Hour)
	limit := clampLimit(q.Get("limit"), defaultUsageLimit, maxUsageLimit)

	where, args := usageFilter(from, to, q)

	summary, err := h.summarize(where, args)
	if err != nil {
		writeServerError(w, "usage summary", err)
		return
	}

	selectCols := usageRecordColumns
	groupClause := ""
	if cols, group, ok := groupByClause(q.Get("group_by")); ok {
		selectCols, groupClause = cols, group
	}

	// 明细行必须带唯一列兜底：毫秒时间戳在并发写入下会重复，纯 `ts DESC` 让同
	// 毫秒记录随机重排，翻页时同一条可能跳变或漏现 —— 与 History 端点
	// (u.ts DESC, u.id DESC) 同一口径。分组行没有单一 id（ts 也常是 0/日桶），不套用。
	orderBy := ` ORDER BY ts DESC`
	if groupClause == "" {
		orderBy += `, id DESC`
	}
	query := `SELECT ` + selectCols + ` FROM usage_records` + where + groupClause +
		orderBy + ` LIMIT ?`
	rowArgs := append(append([]any{}, args...), limit)

	rows, err := h.store.DB().Query(query, rowArgs...)
	if err != nil {
		writeServerError(w, "usage query", err)
		return
	}
	defer rows.Close()

	records := make([]usageRecordEntry, 0)
	for rows.Next() {
		var rec usageRecordEntry
		if err := rows.Scan(&rec.Ts, &rec.PublicModel, &rec.ProviderID, &rec.UpstreamModel, &rec.IngressProtocol,
			&rec.Stream, &rec.InputTokens, &rec.OutputTokens, &rec.TotalTokens, &rec.Status, &rec.LatencyMs); err != nil {
			// 旧实现是 continue：某一行扫不动就静默丢掉，页面上少一条没人知道。
			writeServerError(w, "usage scan", err)
			return
		}
		records = append(records, rec)
	}
	if err := rows.Err(); err != nil {
		writeServerError(w, "usage rows", err)
		return
	}

	writeJSON(w, http.StatusOK, usageQueryResponse{
		Records: records,
		Summary: summary,
	})
}

// summarize 对**全量**过滤结果做聚合，而不是把被 LIMIT 截断的当页记录累加起来。
//
// 页内累加会让 total_requests / avg_latency_ms 变成「本页」的口径，而字段名与
// 前端文案说的都是总量 —— 运维据此判断流量规模会系统性偏低。
func (h *UsageHandler) summarize(where string, args []any) (usageSummary, error) {
	var s usageSummary
	var avg sql.NullFloat64
	err := h.store.DB().QueryRow(
		`SELECT COUNT(*),
		        COALESCE(SUM(total_tokens), 0),
		        COALESCE(SUM(input_tokens), 0),
		        COALESCE(SUM(output_tokens), 0),
		        COALESCE(SUM(CASE WHEN status NOT IN ('ok', 'canceled') THEN 1 ELSE 0 END), 0),
		        AVG(latency_ms)
		   FROM usage_records`+where, args...).
		Scan(&s.TotalRequests, &s.TotalTokens, &s.InputTokens, &s.OutputTokens, &s.ErrorCount, &avg)
	if err != nil {
		return usageSummary{}, err
	}
	if avg.Valid {
		s.AvgLatencyMs = int64(avg.Float64)
	}
	return s, nil
}

// usageHistoryEntry 是「调用历史」表格的一行：一次真实请求的精简视图。
// KeyName 来自 access_keys（密钥被删后为空，前端回退显示 KeyID）。
type usageHistoryEntry struct {
	Ts            int64  `json:"ts"`
	PublicModel   string `json:"public_model"`
	UpstreamModel string `json:"upstream_model"`
	KeyName       string `json:"key_name"`
	KeyID         string `json:"key_id"`
	TotalTokens   int64  `json:"total_tokens"`
	TTFBMs        int64  `json:"ttfb_ms"`
	LatencyMs     int64  `json:"latency_ms"`
	Status        string `json:"status"`
}

// usageHistoryResponse 是「调用历史」的分页结果：当页明细 + 过滤后的总条数。
// 前端要靠 total 才能显示「第 x / y 页」并正确禁用翻页按钮。
type usageHistoryResponse struct {
	Records []usageHistoryEntry `json:"records"`
	Total   int64               `json:"total"`
}

// usageHistoryWhere 是明细查询与计数查询**共用**的过滤子句（口径必须一致，
// 否则总页数与实际能翻到的页数会对不上）。
const usageHistoryWhere = ` WHERE u.ts >= ? AND u.ts <= ?`

// clampOffset 解析分页偏移，钳制为非负；非法输入按 0（首页）。
// 与 clampLimit 同理：负 offset 在 SQLite 里虽是合法语法，但语义诡异，
// 不如在入口处挡掉。
func clampOffset(raw string) int64 {
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// History 返回调用明细（时间倒序，分页），供「调用历史」页展示。
// 参数：from/to（毫秒时间戳，默认近 7 天）、limit（默认 200，上限 1000）、offset（默认 0）。
//
// 排序必须带唯一列兜底（`u.ts DESC, u.id DESC`）：毫秒时间戳在并发下会重复，
// 只按 ts 排序时同值行的相对顺序不确定 —— 配上 OFFSET 分页就会出现
// 「某条记录在两页里各出现一次、另一条被整个跳过」。id 是主键，唯一且稳定，
// 用它当 tiebreaker 才能保证翻页不漏不重。
func (h *UsageHandler) History(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	from, to := queryRange(q, 7*24*time.Hour)
	limit := clampLimit(q.Get("limit"), 200, maxUsageLimit)
	offset := clampOffset(q.Get("offset"))

	// 总条数单独统计（计数不需要 JOIN access_keys，WHERE 只引用 u.ts）。
	var total int64
	if err := h.store.DB().QueryRow(
		`SELECT COUNT(*) FROM usage_records u`+usageHistoryWhere, from, to).Scan(&total); err != nil {
		writeServerError(w, "usage history count", err)
		return
	}

	rows, err := h.store.DB().Query(
		`SELECT u.ts, u.public_model, u.upstream_model, COALESCE(a.name, ''), u.access_key_id, u.total_tokens, u.ttfb_ms, u.latency_ms, u.status
		   FROM usage_records u LEFT JOIN access_keys a ON a.id = u.access_key_id`+usageHistoryWhere+
			` ORDER BY u.ts DESC, u.id DESC LIMIT ? OFFSET ?`, from, to, limit, offset)
	if err != nil {
		writeServerError(w, "usage history", err)
		return
	}
	defer rows.Close()

	entries := make([]usageHistoryEntry, 0)
	for rows.Next() {
		var e usageHistoryEntry
		if err := rows.Scan(&e.Ts, &e.PublicModel, &e.UpstreamModel, &e.KeyName, &e.KeyID, &e.TotalTokens, &e.TTFBMs, &e.LatencyMs, &e.Status); err != nil {
			writeServerError(w, "usage history scan", err)
			return
		}
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		writeServerError(w, "usage history rows", err)
		return
	}
	writeJSON(w, http.StatusOK, usageHistoryResponse{Records: entries, Total: total})
}

type usageGroupEntry struct {
	Key    string `json:"key"`
	Count  int64  `json:"count"`
	Tokens int64  `json:"tokens"`
	// Name：按密钥分组时下发的密钥名称（JOIN access_keys）；其余维度为空。
	Name string `json:"name,omitempty"`
}

func (h *UsageHandler) GroupByKey(w http.ResponseWriter, r *http.Request) {
	// 分组键仍是 access_key_id，但把展示口径换成密钥名（access_keys.name）。
	// 密钥被删除后 LEFT JOIN 取不到名字，前端回退显示原始 key。
	//
	// 时间过滤统一由 groupByNamed 拼到「FROM/JOIN 与 GROUP BY 之间」，
	// 因此这里拆成前置（select + from + join）与后置（group + order）两段，
	// 均不带 WHERE —— 否则 WHERE 会与拼接的 WHERE 冲突或落到 GROUP BY 之后
	// 造成 `near "WHERE": syntax error`。
	h.groupByNamed(w, r,
		`SELECT u.access_key_id as key, COUNT(*), SUM(u.total_tokens), COALESCE(a.name, '')
		   FROM usage_records u LEFT JOIN access_keys a ON a.id = u.access_key_id`,
		` GROUP BY u.access_key_id ORDER BY SUM(u.total_tokens) DESC`)
}

func (h *UsageHandler) GroupByModel(w http.ResponseWriter, r *http.Request) {
	// 只统计底层上游模型用量（按 usage_records.upstream_model 分组）。
	h.groupBy(w, r, "upstream_model")
}

func (h *UsageHandler) GroupByProvider(w http.ResponseWriter, r *http.Request) {
	h.groupBy(w, r, "provider_id")
}

func (h *UsageHandler) GroupByDay(w http.ResponseWriter, r *http.Request) {
	// ts 是毫秒时间戳，需转成可读的 YYYY-MM-DD。
	// 注意不要用 ts / 86400000（那是"epoch 以来的第几天"，
	// 客户端拿到 20709 这种整数无法还原成日期）。
	h.groupBy(w, r, "strftime('%Y-%m-%d', ts / 1000, 'unixepoch', 'localtime')")
}

func (h *UsageHandler) groupBy(w http.ResponseWriter, r *http.Request, column string) {
	q := r.URL.Query()
	from, to, explicit := queryRangeExplicit(q, 7*24*time.Hour)
	limit := clampLimit(q.Get("limit"), defaultUsageLimit, maxUsageLimit)

	// 按天分组时返回时间序列，必须按日期升序；
	// 其余维度按用量降序更有意义（找最耗量的 key / 模型）。
	order := "ORDER BY SUM(total_tokens) DESC"
	if strings.Contains(column, "strftime") {
		order = "ORDER BY key ASC"
	}

	// column 只由本文件的字面量调用点传入（见上方四个 GroupBy*），不含用户输入。
	// 本查询没有 JOIN，ts 列无需前缀。
	where, args := groupRangeClause(from, to, explicit, "")
	query := `SELECT ` + column + ` as key, COUNT(*), SUM(total_tokens) FROM usage_records` + where + ` GROUP BY ` + column + ` ` + order + ` LIMIT ?`
	args = append(args, limit)

	rows, err := h.store.DB().Query(query, args...)
	if err != nil {
		writeServerError(w, "usage group by "+column, err)
		return
	}
	defer rows.Close()

	writeGroupEntries(w, rows, false)
}

// groupByNamed 运行一条「第 4 列为展示名（如密钥名）」的分组查询。
// 调用方把 SQL 拆成两段传入：
//   - head：SELECT 列 + FROM + JOIN（**不含** WHERE）；
//   - tail：GROUP BY + ORDER BY 等（在 WHERE 之后的部分）。
// 时间过滤在这里用 groupRangeClause 生成 WHERE 插到 head 与 tail 之间，
// 保证 WHERE 位于 FROM/JOIN 之后、GROUP BY 之前（SQLite 语法要求）。
func (h *UsageHandler) groupByNamed(w http.ResponseWriter, r *http.Request, head, tail string) {
	q := r.URL.Query()
	from, to, explicit := queryRangeExplicit(q, 7*24*time.Hour)
	limit := clampLimit(q.Get("limit"), defaultUsageLimit, maxUsageLimit)

	// 本分支的查询都 JOIN 了 usage_records u，ts 用 u. 前缀限定。
	where, args := groupRangeClause(from, to, explicit, "u.")
	args = append(args, limit)
	rows, err := h.store.DB().Query(head+where+tail+" LIMIT ?", args...)
	if err != nil {
		writeServerError(w, "usage group by named", err)
		return
	}
	defer rows.Close()

	writeGroupEntries(w, rows, true)
}

// writeGroupEntries 把分组查询结果编码成 JSON。
//
// 初始化为空切片而非 nil：nil 切片会被编码成 JSON null，客户端拿到 null
// 再 .map() 会直接 TypeError。空结果必须是 []。
func writeGroupEntries(w http.ResponseWriter, rows *sql.Rows, withName bool) {
	entries := make([]usageGroupEntry, 0)
	for rows.Next() {
		var e usageGroupEntry
		var key, name string
		var err error
		if withName {
			err = rows.Scan(&key, &e.Count, &e.Tokens, &name)
		} else {
			err = rows.Scan(&key, &e.Count, &e.Tokens)
		}
		if err != nil {
			writeServerError(w, "usage group scan", err)
			return
		}
		e.Key = key
		e.Name = name
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		writeServerError(w, "usage group rows", err)
		return
	}
	writeJSON(w, http.StatusOK, entries)
}
