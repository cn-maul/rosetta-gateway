package admin

import (
	"database/sql"
	"encoding/csv"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/cn-maul/rosetta-gateway/internal/server"
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

// queryRangeExplicit 解析 from/to，并区分「显式传 from=0（=全部历史）」与
// 「未传 from（=取默认 window）」。总览页的「全部」档需要前者。
// 返回 (from, to, explicit)；explicit 为 true 表示请求里带了 from 参数。
//
// 解析失败必须显式报错，不能像旧实现那样把错误丢进 `_` 后置 0 ——
// `?from=abc` 会让 from=0，而「from=0 且 explicit」本会被解读为「全部历史」——
// 不生成 ts >= ? 下界，直接退化成全表扫描。usage_records 没有保留策略、
// 行数无上界，全表聚合是本项目最重的一条查询。
//
// # 唯一的区间解析入口（2026-10-10）
//
// 此前还有一个 queryRange，它把显式 from=0 回落成默认窗口，于是
// 「全部历史」静默变成「最近 7 天」。/stats 用的是本函数、history 与 CSV
// 用的是那个错的 —— 两边对同一个「全部」给出不同结果。现已全部收敛到本函数，
// 那个错的一并删除：留着它等于留一个「看起来能用、实际静默给错数据」的坑。
func queryRangeExplicit(q url.Values, window time.Duration) (from, to int64, explicit bool, err error) {
	rawFrom := q.Get("from")
	from, err = parseOptionalUnixMilli(rawFrom, "from")
	if err != nil {
		return 0, 0, false, err
	}
	to, err = parseOptionalUnixMilli(q.Get("to"), "to")
	if err != nil {
		return 0, 0, false, err
	}
	explicit = rawFrom != ""
	if !explicit {
		from = time.Now().Add(-window).UnixMilli()
	}
	if to == 0 {
		to = time.Now().UnixMilli()
	}
	return from, to, explicit, nil
}

// groupFilter 是 by-* 端点用的条件构造：与 usageFilter 同源、同一套语义，
// 但不带明细独有的那三个维度过滤（by-* 的过滤维度就是分组维度本身，
// 另加过滤会与 GROUP BY 打架）。
//
// from=0 且 explicit 时表示「全部历史」（不设下界）；否则按 from/to 过滤。
//
// scopeUserID 与 usageFilter 同义：非空时强制按 user_id 收窄。
// **每个 by-* 端点都必须过这里** —— 漏掉一个，对应的端点就会把全局数据
// 暴露给普通用户（白名单前缀只保证「能进来」，不保证「只看到自己的」）。
func groupFilter(from, to int64, explicit bool, scopeUserID string) (where string, args []any, f store.UsageFilter) {
	q := url.Values{} // 无额外维度过滤
	return usageFilter(from, to, explicit, q, scopeUserID)
}

// usageFilter 构造明细查询的 WHERE，并把同一组条件翻译成 store.UsageFilter
// 供聚合查询（summarize / by-*）复用。
//
// 之所以一次返回两种形态：明细列表与聚合合计**必须落在同一组过滤条件上**。
// 之前靠的是「两处各拆一遍参数」，那正是 usageFilter 这类单一构造点要消灭的
// 重复；现在多返回一个结构体仍然只有一处拆解，且改条件只需改这一处。
//
// 只构造一次、两处复用。分开拼两份迟早会漂移 —— 旧实现的 group_by 分支就直接
// `args = []any{from, to}` 把 key_id / model / provider_id 三个过滤条件整体丢了，
// 于是「按密钥筛选 + 按天分组」会静默返回全量数据。
//
// explicit && from==0 → 「全部历史」（不设下界），与 by-* 端点同口径。此前 /usage
// 明细把 from=0 当「最近 window」，而 by-* 把 from=0 当「全部」——同名参数语义相反，
// curl/API 调用方极易踩坑，这里统一。
//
// # scopeUserID：多用户改造的权限收窄（最要命的一类漏洞）
//
// 传入非空值时**强制**追加 `user_id = ?`。这是所有用量端点
// （Query / by-key / by-model / by-provider / by-day / History / CSV）
// 共用的唯一 WHERE 构造点 —— 收在这里而不是各 handler 里，
// 是因为**漏一处就是数据泄露**，而漏写是这类改动最常见的失误。
//
// 空值 = admin（不加过滤）。调用方用 callerScope 决定传什么。
func usageFilter(from, to int64, explicit bool, q url.Values, scopeUserID string) (where string, args []any, f store.UsageFilter) {
	f = store.UsageFilter{UserID: scopeUserID}
	if explicit && from == 0 {
		// 「全部历史」：from 不设下界。明细侧与聚合侧都不收下界。
		from = 0
	} else {
		f.From = from
	}
	f.To = to

	conds := []string{"ts <= ?"}
	args = []any{to}
	if from > 0 {
		conds = append([]string{"ts >= ?"}, conds...)
		args = append([]any{from}, args...)
	}
	// 权限过滤放在**用户显式过滤条件之前**：这样即使请求带了
	// ?key_id=别人的key，也只会得到空集而不是别人的记录 ——
	// AND 是可交换的，但顺序影响的是索引选择，先user_id 走索引更快。
	if scopeUserID != "" {
		conds = append(conds, `user_id = ?`)
		args = append(args, scopeUserID)
	}
	for _, flt := range []struct{ param, col string }{
		{"key_id", "access_key_id"},
		{"model", "public_model"},
		{"provider_id", "provider_id"},
	} {
		if v := q.Get(flt.param); v != "" {
			conds = append(conds, flt.col+` = ?`)
			args = append(args, v)
		}
	}
	// 明细侧要按原始 user_id 过滤（列可空），而聚合侧走 UsageSource
	// 内部统一的 COALESCE 归一，两边语义等价：usage_records 里的
	// NULL user_id 归一后是 ''，不会匹配任何真实用户 id。
	f.KeyID = q.Get("key_id")
	f.PublicModel = q.Get("model")
	f.ProviderID = q.Get("provider_id")
	return " WHERE " + strings.Join(conds, " AND "), args, f
}

// callerScope 决定本次请求能看到谁的数据。
//
// admin（含引导态合成用户）返回空串 = 不限制；普通用户返回自己的 id。
//
// 为什么不从 query参数里读 ?user_id=：那等于任何普通用户都能
// 传别人的 id 来看数据。作用域只能由**服务端**根据会话身份判定。
func callerScope(r *http.Request) string {
	me := server.UserFromContext(r.Context())
	if me == nil {
		// 没有身份：给一个不可能匹配任何行的哨兵值。
		// 刻意**不返回空串**（那是 admin 语义），否则未鉴权请求会看到全量。
		return "\x00none"
	}
	// 空 ID **不**当admin（fail-closed）：返回哨兵值而非空串，
	// 空串是 admin 语义（不限作用域）。统一认证后该状态不可达，
	// 但留着等于给「拿不到身份」发一张全量通行证。
	if me.ID == "" {
		return "\x00none"
	}
	if me.IsAdmin() {
		return "" // admin
	}
	return me.ID
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
		// 日界用**本地时区**，与 /usage/by-day、归档表 day 列、前端 GroupByDay
		// 同一套口径。原实现写的是 `(ts/86400000)*86400000` —— 那是
		// 「epoch 以来的第几天」（UTC），与中国区相差 8 小时，于是同一个界面里
		// 两个按天趋势的数字对不上。
		//
		// 仍是**毫秒时间戳**（不是日期字符串）：ts 列被 Scan 进 int64，
		// 换类型会把整个请求打成 500（usageRecordEntry.Ts 是 int64）。
		// 所以把「本地日期的午夜」换算回毫秒 —— strftime('%s') 按 **UTC**
		// 解释 epoch，得先减一个时区偏移才能得到真正的本地午夜。
		//
		// 口径必须逐字等于 store.dayExpr 的分桶，否则剪枝前后同一窗口差一天。
		const localMidnightMs = `CAST(strftime('%s', ts / 1000, 'unixepoch', 'localtime') AS INTEGER) * 1000`
		return localMidnightMs + ` as ts, ` + blank + `, ` + agg,
			` GROUP BY ` + localMidnightMs, true
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

// latency 扫描目标必须是 float64，不能是 int64。
//
// 两条聚合表达式都产出 REAL：`UsageRatioExpr` 靠 `* 1.0` 强制浮点除法，
// `groupByClause` 用的 `AVG(latency_ms)` 在 SQLite 里同样返回 REAL。
// modernc.org/sqlite 把 REAL 交回 float64，而 database/sql 对 `*int64`
// 目标走的是「先 FormatFloat 再 ParseInt」—— 平均延迟一旦是小数
// （13.3333…）就 ParseInt 失败，整个请求 500，且 `summarize` 在明细查询
// 之前跑，所以连一条记录都不返回。实测：延迟恰好整除的 60 秒窗 200，
// 含不同时延的 1 小时窗与默认 24 小时窗都 500。
type usageRecordEntry struct {
	Ts              int64   `json:"ts"`
	PublicModel     string  `json:"public_model"`
	ProviderID      string  `json:"provider_id"`
	UpstreamModel   string  `json:"upstream_model"`
	IngressProtocol string  `json:"ingress_protocol"`
	Stream          bool    `json:"stream"`
	InputTokens     int64   `json:"input_tokens"`
	OutputTokens    int64   `json:"output_tokens"`
	TotalTokens     int64   `json:"total_tokens"`
	Status          string  `json:"status"`
	LatencyMs       float64 `json:"latency_ms"`
}

type usageSummary struct {
	TotalRequests int64   `json:"total_requests"`
	TotalTokens   int64   `json:"total_tokens"`
	InputTokens   int64   `json:"input_tokens"`
	OutputTokens  int64   `json:"output_tokens"`
	ErrorCount    int64   `json:"error_count"`
	AvgLatencyMs  float64 `json:"avg_latency_ms"`
}

func (h *UsageHandler) Query(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	from, to, explicit, err := queryRangeExplicit(q, 24*time.Hour)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	limit := clampLimit(q.Get("limit"), defaultUsageLimit, maxUsageLimit)

	// 记录列表走明细（单次调用无法从日聚合还原），summary 走合并来源。
	// 两者共用 usageFilter 拆出的同一组条件。
	where, args, filter := usageFilter(from, to, explicit, q, callerScope(r))

	summary, err := h.summarize(filter)
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

	rows, err := h.store.Reader().Query(query, rowArgs...)
	if err != nil {
		writeServerError(w, "usage query", err)
		return
	}
	defer rows.Close()

	records := make([]usageRecordEntry, 0)
	for rows.Next() {
		var rec usageRecordEntry
		// 延迟先落到 sql.NullFloat64 再取整：AVG() 返回 REAL，直接扫进
		// 整型会因 ParseInt 失败把整个请求打成 500（见类型注释）。
		var latency sql.NullFloat64
		if err := rows.Scan(&rec.Ts, &rec.PublicModel, &rec.ProviderID, &rec.UpstreamModel, &rec.IngressProtocol,
			&rec.Stream, &rec.InputTokens, &rec.OutputTokens, &rec.TotalTokens, &rec.Status, &latency); err != nil {
			// 旧实现是 continue：某一行扫不动就静默丢掉，页面上少一条没人知道。
			writeServerError(w, "usage scan", err)
			return
		}
		rec.LatencyMs = roundMillis(latency)
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
//
// 数据源是 store.UsageSource（明细 ∪ 日归档），与 store.GetUsageStats 同构：
// 只查明细的话，剪枝一跑起来这里的合计就会逐日变小，而它和下面的记录列表
// 并排显示在同一张卡上 —— 合计比列表还小是没法解释的。
//
// avg_latency_ms 用「延迟总和 / 请求数」而不是 AVG(延迟列)：归档行的
// latency_sum_ms 是一组请求的累计值，直接平均等于把一天的耗时当成一次请求
// 折进去。两支归一后 SUM(延迟)/SUM(请求数) 对明细也正好等于原口径的 AVG。
func (h *UsageHandler) summarize(f store.UsageFilter) (usageSummary, error) {
	var s usageSummary
	src, args := store.UsageSource(f)
	// `* 1.0` 让这条比值走 REAL，扫整型会 ParseInt 失败（见 usageSummary 注释）。
	var avgLatency sql.NullFloat64
	err := h.store.Reader().QueryRow(
		`SELECT COALESCE(SUM(u.n), 0),
		       `+store.UsageSumExpr("total_tokens", "u")+`,
		       `+store.UsageSumExpr("input_tokens", "u")+`,
		       `+store.UsageSumExpr("output_tokens", "u")+`,
		       COALESCE(SUM(u.n_err), 0),
		       `+store.UsageRatioExpr("latency_sum", "n", "u")+`
		   FROM `+src, args...).
		Scan(&s.TotalRequests, &s.TotalTokens, &s.InputTokens, &s.OutputTokens, &s.ErrorCount, &avgLatency)
	if err != nil {
		return usageSummary{}, err
	}
	s.AvgLatencyMs = roundMillis(avgLatency)
	return s, nil
}

// roundMillis 把 SQL 的 REAL 比值收敛回整毫秒。
//
// 落库是按 0.5ms 向上取整的单次值，聚合后出现小数是正常的（平均值本来
// 就该允许小数），但对外契约一直是整数毫秒，这里四舍五入保持不变。
// NULL → 0，与各查询原有的 COALESCE 口径一致。
func roundMillis(v sql.NullFloat64) float64 {
	if !v.Valid {
		return 0
	}
	return math.Round(v.Float64)
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
	// OutputTokens 是本次**生成**出来的 token 数（2026-10-11 新增）。
	//
	// 为什么必须单独下发、不能拿 TotalTokens 顶替：TotalTokens = 输入 + 输出，
	// 而输入 token 是请求侧送进去的、不是模型"吐"出来的。用总量算速度会把
	// 速度虚高好几倍（一个 1 万输入 / 2 百输出的请求，用总量算出来的"速度"
	// 是真实生成速度的 50 倍），而表格里那一列只有数字、看不出错。
	OutputTokens int64 `json:"output_tokens"`
	// Tps 是本次请求的平均输出速度（token/s），**由后端算好**（2026-10-11 新增）。
	//
	// # 为什么在后端算而不是前端拿 output_tokens/latency_ms 现算
	//
	// 口径必须与总览页「平均速度」逐字一致，否则同一批请求会在两个页面给出
	// 两个数，而这种偏差没有任何迹象。总览页读的是 store.GetRecentThroughput，
	// 其公式是 `SUM(output_tokens) * 1000.0 / NULLIF(SUM(latency_ms), 0)`
	// —— 单条记录下 SUM 退化，等价于 `output_tokens * 1000 / latency_ms`。
	// 把这条公式写在后端、紧挨着 tpsFor 的注释，全项目只有一处实现；
	// 放前端就等于让 TS 与 SQL 各持一份同样的公式，改一处忘一处时
	// 两个页面会静默分叉。
	//
	// 除零/无样本的边界同理：`NULLIF(...,0)` 那半边保护只存在于后端，
	// 前端复刻一份就要在 JS 里再写一次 `latency_ms > 0` 判断。
	//
	// # 0 值语义：不是「速度为零」，而是「没有可算的样本」
	//
	// 上游没报 usage（output_tokens=0）、非流式短请求、或错误请求
	// （latency_ms=0）时这里恒为 0，前端据此显示 '—' 而不是 '0.0'。
	//
	// 为什么不能显示 0.0：本列的可算样本里 tps 恒 > 0（分子分母都 > 0 的
	// 商不可能为 0），所以 0.0 只可能来自"没有样本"。把它印成 0.0 会被读成
	// 「这次生成极慢」，而真相是「这次压根没有能算速度的数据」—— 与
	// GetRecentThroughput 用 NULLIF 挡住除零是同一个意图：
	// 宁可说"不知道"，也不给一个会被误读的数。
	Tps       float64 `json:"tps"`
	TTFBMs    int64   `json:"ttfb_ms"`
	LatencyMs int64   `json:"latency_ms"`
	Status    string  `json:"status"`
	// Stream 标记这次调用是否为流式（2026-10-10 新增）。
	//
	// 为什么必要：流式与非流式的**首字时间含义完全不同** —— 非流式的 ttfb
	// 大致等于总耗时（响应一次性回来），而流式的 ttfb 是「多久出第一个字」。
	// 两者混在同一列里、不加标注，运维看到「首字 15 秒」无法判断是模型慢
	// 还是压根没用流式。
	Stream bool `json:"stream"`
	// Cost 是该条用量的固化费用（元），2026-10-10 新增。
	//
	// 取的是 cost_total —— 落库**当时**按当时单价算好并固化的那个值，
	// 与账单、报表同源。改价不回溯历史，这里也就不会被后续改价影响。
	//
	// 为什么必须显示：单价可能低到单次不足一分钱，而余额按分扣减，
	// 于是「报表在涨、余额不动」。把每条的费用摆出来，用户才能对上账；
	// 否则只能看到余额缓慢变化，无从判断钱花在哪。
	Cost float64 `json:"cost"`
}

// tpsFor 计算单条请求的平均输出速度（token/s）；无样本时返回 0。
//
// # 公式来源（不许在这里"顺手改进"）
//
// 逐字取自 store.GetRecentThroughput：
//
//	SUM(output_tokens) * 1000.0 / NULLIF(SUM(latency_ms), 0)
//
// 单条记录下两个 SUM 各自退化成该行自己的列，于是得到
// `output_tokens * 1000 / latency_ms`。这是刻意的：表格每行的速度与
// 总览页的「平均速度」必须是同一套算法，否则同一批请求会出现两个数
// （表格逐行平均 ≠ 总量加权平均），而对不上时没人知道该信哪个。
//
// # 口径的一个已知性质（不是缺陷）
//
// 分母用 latency_ms —— **总耗时**，含首字延迟与输入处理时间，不是纯粹的
// 解码时长。所以这个数系统性略低于"吐字速率"。保持它是对的：改成分母
// 只取 (latency - ttfb) 会让本列与总览页立刻分叉，而分叉的代价大于
// 「略保守」这点偏差。
//
// # 为什么 latency_ms <= 0 或 output_tokens <= 0 直接返回 0
//
// latency_ms=0 是真会出现的（错误请求在触碰上游前就返回，没走计时），
// 直接相除会得到 +Inf 并序列化成 JSON 里非法的 `Inf` 字面量 —— 前端
// JSON.parse 会当场抛错，整张表打不开。这与 GetRecentThroughput 的
// NULLIF(SUM(latency_ms), 0) 是同一处保护。
func tpsFor(outputTokens, latencyMs int64) float64 {
	if outputTokens <= 0 || latencyMs <= 0 {
		return 0
	}
	return float64(outputTokens) * 1000 / float64(latencyMs)
}

// usageHistoryResponse 是「调用历史」的分页结果：当页明细 + 过滤后的总条数。
// 前端要靠 total 才能显示「第 x / y 页」并正确禁用翻页按钮。
type usageHistoryResponse struct {
	Records []usageHistoryEntry `json:"records"`
	Total   int64               `json:"total"`
}

// usageHistoryFilters 组装 History/CSV 共用的过滤子句：时间范围之外支持
// status（精确）、public_model（精确）、access_key_id（精确）。
// 列名是字面量、值全参数化，无注入面。返回 WHERE 子句与配套参数。
// usageHistoryFilters 构造历史/CSV 查询的 WHERE。
//
// scopeUserID 的语义同 usageFilter —— **这里也必须收**，否则
// 普通用户能通过 /admin/api/usage/history 或 history.csv 看到
// 全公司的调用明细（这比看到自己的用量严重得多：明细里有模型名与时间戳，
// 足以推断出同事在做什么项目）。
func usageHistoryFilters(from, to int64, status, model, keyID, scopeUserID string) (string, []any) {
	conds := []string{"u.ts >= ?", "u.ts <= ?"}
	args := []any{from, to}
	if scopeUserID != "" {
		conds = append(conds, "u.user_id = ?")
		args = append(args, scopeUserID)
	}
	if status != "" {
		conds = append(conds, "u.status = ?")
		args = append(args, status)
	}
	if model != "" {
		conds = append(conds, "u.public_model = ?")
		args = append(args, model)
	}
	if keyID != "" {
		conds = append(conds, "u.access_key_id = ?")
		args = append(args, keyID)
	}
	return " WHERE " + strings.Join(conds, " AND "), args
}

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

// ExportCSV 以 CSV 流式导出调用明细（与 History 同一套过滤参数，
// 一次最多导 maxUsageLimit 行）。排障取证用：把一段时间的明细拉进表格工具。
// Content-Disposition 带 filename，浏览器直接触发下载。
//
// # 静默截断是排障场景里最贵的一种 bug
//
// 这个端点原本把 limit 拉满到 maxUsageLimit，于是「共 12 万条、导出后只有
// 1000 行」看起来像是「上游只失败过 1000 次」—— 一个被截断的证据被读成了
// 完整的证据。现在上限命中时写 X-Export-Truncated: 1 与 X-Export-Total，
// 让前端能在下载后明确提示「已截断」而不是让人对着一个假象做判断。
func (h *UsageHandler) ExportCSV(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	// 与 History 同一个理由改用 queryRangeExplicit：显式 from=0 是「全部」，
	// 不能被回落成默认 7 天，否则导出的文件与页面上「全部」看到的不一致，
	// 而这种不一致要等对账时才发现（2026-10-10 修复）。
	from, to, _, err := queryRangeExplicit(q, 7*24*time.Hour)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	limit := clampLimit(q.Get("limit"), maxUsageLimit, maxUsageLimit)
	where, args := usageHistoryFilters(from, to, q.Get("status"), q.Get("model"), q.Get("key_id"), callerScope(r))

	// 总条数与 History 同源：多一次 COUNT 换来「导出的到底是不是全部」。
	var total int64
	if err := h.store.Reader().QueryRow(
		`SELECT COUNT(*) FROM usage_records u`+where, args...).Scan(&total); err != nil {
		writeServerError(w, "usage export count", err)
		return
	}

	rows, err := h.store.Reader().Query(
		`SELECT u.ts, u.public_model, u.upstream_model, COALESCE(a.name, ''), u.access_key_id, u.total_tokens, u.output_tokens, u.ttfb_ms, u.latency_ms, u.status, u.stream, COALESCE(u.cost_total, 0)
		   FROM usage_records u LEFT JOIN access_keys a ON a.id = u.access_key_id`+where+
			` ORDER BY u.ts DESC, u.id DESC LIMIT ?`, append(args, limit)...)
	if err != nil {
		writeServerError(w, "usage export", err)
		return
	}
	defer rows.Close()

	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="usage-history.csv"`)
	w.Header().Set("X-Export-Total", strconv.FormatInt(total, 10))
	if total > limit {
		w.Header().Set("X-Export-Truncated", "1")
	}
	cw := csv.NewWriter(w)
	// 列名与页面表格一一对应：导出件要能独立读懂，缺了 stream/cost 就得
	// 回网关上看，而 CSV 的常见用途恰恰是拿出去单独分析。
	//
	// output_tokens / tps 追加在**末尾**而不是插在 total_tokens 之后：
	// 页面新增了「速度」列，导出件若不给这两个数，用户就没法在表格工具里
	// 复现这一列（连自己算都算不了，因为缺 output_tokens）——
	// 那正是本注释开头说的「拿出去读不懂」。但列位置一动，任何按下标取值
	// 的既有消费方（脚本、Excel 模板）会静默错位，所以新增列一律追加，
	// 让原有 11 列的下标保持不变。
	_ = cw.Write([]string{"ts", "public_model", "upstream_model", "key_name", "key_id",
		"total_tokens", "ttfb_ms", "latency_ms", "status", "stream", "cost",
		"output_tokens", "tps"})
	for rows.Next() {
		var e usageHistoryEntry
		if err := rows.Scan(&e.Ts, &e.PublicModel, &e.UpstreamModel, &e.KeyName, &e.KeyID,
			&e.TotalTokens, &e.OutputTokens, &e.TTFBMs, &e.LatencyMs, &e.Status, &e.Stream, &e.Cost); err != nil {
			return
		}
		e.Tps = tpsFor(e.OutputTokens, e.LatencyMs)
		stream := "false"
		if e.Stream {
			stream = "true"
		}
		// tps 无样本时写空串而不是 0：CSV 是拿去做数值分析的，空单元格会被
		// 读成"缺失"（AVERAGE 等函数自动跳过），而 0 会被算进平均值里、
		// 把整体速度往下拽。页面显示 '—' 与这里留空是同一个意图。
		tps := ""
		if e.Tps > 0 {
			tps = strconv.FormatFloat(e.Tps, 'f', -1, 64)
		}
		_ = cw.Write([]string{
			time.UnixMilli(e.Ts).Format(time.RFC3339), e.PublicModel, e.UpstreamModel,
			e.KeyName, e.KeyID,
			strconv.FormatInt(e.TotalTokens, 10), strconv.FormatInt(e.TTFBMs, 10),
			strconv.FormatInt(e.LatencyMs, 10), e.Status, stream,
			strconv.FormatFloat(e.Cost, 'f', -1, 64),
			strconv.FormatInt(e.OutputTokens, 10), tps,
		})
	}
	cw.Flush()
}

// History 返回调用明细（时间倒序，分页），供「调用历史」页展示。
// 参数：from/to（毫秒时间戳，默认近 7 天）、limit（默认 200，上限 1000）、offset（默认 0）、
// status / model / key_id（可选精确过滤，与 CSV 导出共用同一套过滤，口径一致）。
//
// 排序必须带唯一列兜底（`u.ts DESC, u.id DESC`）：毫秒时间戳在并发下会重复，
// 只按 ts 排序时同值行的相对顺序不确定 —— 配上 OFFSET 分页就会出现
// 「某条记录在两页里各出现一次、另一条被整个跳过」。id 是主键，唯一且稳定，
// 用它当 tiebreaker 才能保证翻页不漏不重。
func (h *UsageHandler) History(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	// 用 queryRangeExplicit 而不是 queryRange：前端的「全部」档发的是
	// from=0，而 queryRange 会把显式 0 回落成默认 7 天 —— 用户选「全部」
	// 实际只看到最近一周，且没有任何提示（2026-10-10 修复）。
	// 与 /stats 现在是同一套口径，「总览选全部」与「历史选全部」一致。
	from, to, _, err := queryRangeExplicit(q, 7*24*time.Hour)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	limit := clampLimit(q.Get("limit"), 200, maxUsageLimit)
	offset := clampOffset(q.Get("offset"))
	where, args := usageHistoryFilters(from, to, q.Get("status"), q.Get("model"), q.Get("key_id"), callerScope(r))

	// 总条数单独统计（计数不需要 JOIN access_keys，WHERE 只引用 u 列）。
	var total int64
	if err := h.store.Reader().QueryRow(
		`SELECT COUNT(*) FROM usage_records u`+where, args...).Scan(&total); err != nil {
		writeServerError(w, "usage history count", err)
		return
	}

	rows, err := h.store.Reader().Query(
		`SELECT u.ts, u.public_model, u.upstream_model, COALESCE(a.name, ''), u.access_key_id, u.total_tokens, u.output_tokens, u.ttfb_ms, u.latency_ms, u.status, u.stream, COALESCE(u.cost_total, 0)
		   FROM usage_records u LEFT JOIN access_keys a ON a.id = u.access_key_id`+where+
			` ORDER BY u.ts DESC, u.id DESC LIMIT ? OFFSET ?`, append(args, limit, offset)...)
	if err != nil {
		writeServerError(w, "usage history", err)
		return
	}
	defer rows.Close()

	entries := make([]usageHistoryEntry, 0)
	for rows.Next() {
		var e usageHistoryEntry
		if err := rows.Scan(&e.Ts, &e.PublicModel, &e.UpstreamModel, &e.KeyName, &e.KeyID,
			&e.TotalTokens, &e.OutputTokens, &e.TTFBMs, &e.LatencyMs, &e.Status, &e.Stream, &e.Cost); err != nil {
			writeServerError(w, "usage history scan", err)
			return
		}
		// tps 在 Scan 之后算，不放进 SELECT：公式只有一处（tpsFor），
		// 且 SQL 里再写一遍除法意味着除零保护也要在 SQL 与 Go 两边各写一份。
		e.Tps = tpsFor(e.OutputTokens, e.LatencyMs)
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
	// Cost 是固化费用合计（元），即 SUM(cost_total)。
	//
	// 2026-10-10 新增：钱包页的「消耗记录」改成按天列总额（逐条明细在
	// 「调用历史」页），而 by-day 此前只给 count/tokens，前端拿不到钱。
	//
	// 四个维度共用这一份实现，所以 by-model / by-key / by-provider 也一并
	// 带上了 cost。刻意**不加 if column == "day" 分支**：那会让 SELECT 列
	// 与 Scan 的元数随维度变化，而 Scan 的顺序错位在编译期与测试期都很难
	// 察觉（多扫/少扫一列的报错信息指向 Scan，而不是真正出错的那一行）。
	// 多带一个字段对现有调用方是无害的（老前端不读就是忽略），
	// 而「按天有钱、按模型没钱」这种不对称才是真正会咬人的形状。
	//
	// 口径与 /stats 的 cost 逐字一致：都读落库当时固化的 cost_total，
	// 改价不回溯历史。所以「按天求和 == 总览的区间合计」在构造上就成立。
	Cost float64 `json:"cost"`
	// Name：按密钥分组时下发的密钥名称（JOIN access_keys）；其余维度为空。
	Name string `json:"name,omitempty"`
}

// groupBy 的分组列取自归一化来源。传的是**列名**而非表达式，四个调用点
// （day / upstream_model / provider_id / access_key_id）都是表 A 与明细表
// 双方都有的维度列 —— 剪枝后要能靠它把归档重新分桶。
func (h *UsageHandler) GroupByKey(w http.ResponseWriter, r *http.Request) {
	// 分组键仍是 access_key_id，但把展示口径换成密钥名（access_keys.name）。
	// 密钥被删除后 LEFT JOIN 取不到名字，前端回退显示原始 key。
	//
	// 时间过滤在 groupBy 里统一拼，调用方只给列名与展示名表达式。
	h.groupBy(w, r, "access_key_id", `COALESCE(a.name, '')`)
}

func (h *UsageHandler) GroupByModel(w http.ResponseWriter, r *http.Request) {
	// 只统计底层上游模型用量（按 upstream_model 分组）。表 A 也有这一维
	// （P1.5 补的），所以剪枝后这条分项统计依然完整。
	h.groupBy(w, r, "upstream_model", "")
}

func (h *UsageHandler) GroupByProvider(w http.ResponseWriter, r *http.Request) {
	h.groupBy(w, r, "provider_id", "")
}

func (h *UsageHandler) GroupByDay(w http.ResponseWriter, r *http.Request) {
	// 直接取归一化来源的 day 列：明细侧由 dayExpr 按**本地时区**算出，
	// 归档侧是表 A 已有的本地 day，两支逐字同一个表达式（同一口径）。
	//
	// 不要写 ts / 86400000（那是「epoch 以来的第几天」，客户端拿到
	// 20709 这种整数无法还原成日期），也不要在这里重新 strftime 一次 ——
	// 那样表 A 的 day 就会被当成 UTC 二次偏移。
	//
	// 这一条顺带修掉了「全部」档趋势起点：合并来源后最早的一天来自表 A，
	// 剪枝不再让图上凭空少掉 30 天以前。
	h.groupBy(w, r, "day", "")
}

// groupBy 是四个 by-* 端点的唯一实现。
//
// 数据源是 store.UsageSource（明细 ∪ 日归档）。若只查明细，剪枝一跑起来
// 「全部」档的分项统计就会逐日缩水，而这几个数字正是运维判断流量分布的依据。
//
// count 用 SUM(u.n) 而**不是 COUNT(*)**：归档一行代表一天×一维度下的一组
// 请求（n=request_count），COUNT(*) 会把它当成 1 次请求，直接少算。
//
// column 与 nameExpr 只由本文件的字面量调用点传入，不含用户输入。
func (h *UsageHandler) groupBy(w http.ResponseWriter, r *http.Request, column, nameExpr string) {
	q := r.URL.Query()
	from, to, explicit, err := queryRangeExplicit(q, 7*24*time.Hour)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	limit := clampLimit(q.Get("limit"), defaultUsageLimit, maxUsageLimit)

	_, _, filter := groupFilter(from, to, explicit, callerScope(r))
	src, args := store.UsageSource(filter)

	// 按天分组时返回时间序列，必须按日期升序；
	// 其余维度按用量降序更有意义（找最耗量的 key / 模型）。
	order := "ORDER BY tokens DESC"
	if column == "day" {
		order = "ORDER BY key ASC"
	}
	args = append(args, limit)

	withName := nameExpr != ""
	query := `SELECT u.` + column + ` AS key, COALESCE(SUM(u.n), 0) AS count, ` +
		store.UsageSumExpr("total_tokens", "u") + ` AS tokens, ` +
		store.UsageSumExpr("cost_total", "u") + ` AS cost`
	if withName {
		// JOIN 放在归一化来源之外：密钥名是展示属性，不参与聚合，
		// 放里面会因一条密钥对应多行来源而把计数复制多份。
		query += `, ` + nameExpr + ` AS name`
	}
	query += ` FROM ` + src
	if withName {
		query += ` LEFT JOIN access_keys a ON a.id = u.` + column
	}
	// 聚合别名在 ORDER BY 里可直接引用（SQLite 允许），GROUP BY 不行 ——
	// 那里必须写原列名。
	query += ` GROUP BY u.` + column + ` ` + order + ` LIMIT ?`

	rows, err := h.store.Reader().Query(query, args...)
	if err != nil {
		writeServerError(w, "usage group by "+column, err)
		return
	}
	defer rows.Close()

	writeGroupEntries(w, rows, withName)
}

// Prune 手动触发用量归档（POST /admin/api/usage/prune?keep_days=N）。
//
// **必须 admin-only，且不能只靠路径白名单**：本路径落在普通用户可访问的
// /admin/api/usage 前缀下，白名单只保证「能进来」，不保证「有权限」。
// 剪枝是**不可逆**的数据删除（明细被聚合进表 A 后，单次调用的记录就没了），
// 让普通用户能触发就是把一个只读面变成了破坏性操作。
//
// 存在的理由：自动剪枝按 24h 周期跑，运维无法回答「上个月的用量还在吗」。
// 这个端点让它当场可查、可控 —— 干完还会回 PruneResult（删了多少行、
// 剪到哪一天），不需要再去翻库。
//
// keep_days 缺省用 store.DefaultRetentionDays（定时任务用的是同一个值）。
// 参数非法时 store 侧返回 skipped=true 而不是报错，所以这里**不**预先拒绝 ——
// 两处各有一套校验，迟早在某个边界上给出不同答案。越界的语义（1 或 99999）
// 写进 reason 字段回给调用方。
func (h *UsageHandler) Prune(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireAdmin(w, r); !ok {
		return
	}
	keepDays := store.DefaultRetentionDays
	if raw := strings.TrimSpace(r.URL.Query().Get("keep_days")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, "keep_days 必须是整数")
			return
		}
		keepDays = n
	}
	res, err := h.store.PruneOldUsage(r.Context(), keepDays)
	if err != nil {
		writeServerError(w, "prune usage", err)
		return
	}
	// 走 POST：AutoReload 中间件据此触发审计（字段名会进 audit_log，
	// 而 keep_days 就是这里最需要留痕的那个参数）。
	writeJSON(w, http.StatusOK, res)
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
		// 扫描顺序必须与 groupBy 里 SELECT 的列顺序**逐字对应**：
		// key, count, tokens, cost[, name]。加 cost 时漏改这里的表现是
		// Scan 报「期望 N 列、实际 M 列」—— 报错点在 Scan，读代码的人会先
		// 去查 Scan 本身，而不是想到「上面那条 SELECT 加了一列」。
		if withName {
			err = rows.Scan(&key, &e.Count, &e.Tokens, &e.Cost, &name)
		} else {
			err = rows.Scan(&key, &e.Count, &e.Tokens, &e.Cost)
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
