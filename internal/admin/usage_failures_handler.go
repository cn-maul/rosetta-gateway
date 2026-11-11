package admin

import (
	"net/http"
	"time"

	"github.com/cn-maul/rosetta-gateway/internal/store"
)

// 失败口径与 store.UsageSource 的 n_err 逐字一致：**排除 ok 与 canceled**。
//
// 为什么排除 canceled：客户端中途断开不是上游的问题，把它算进「失败原因」
// 会让一张排障视图里混进大量「用户自己关掉了页面」。而 downstream_write_failed
// 同属 client 侧（已提交后回写失败），一并排除——它们要查的是网关或客户端，
// 不是上游。
const usageFailureCond = `status NOT IN ('` + store.UsageStatusOK + `', '` + store.UsageStatusCanceled + `')`

// @ts-type UsageFailureCodeEntry
//
// 按错误码聚合的一行。「什么错最多」是这个端点的第一问，所以排在最前。
type usageFailureCodeEntry struct {
	// Code 是网关归一化短码（upstream_error / rate_limit_exceeded / ...）。
	// error_code 为空的老记录回退成 status（error / timeout / truncated），
	// 这样它们仍然成组出现，而不是挤成一行空 key。
	Code string `json:"code"`
	// HTTPStatus 是当时回给客户端的状态码。同一个 code 可能对应多个状态码
	// （upstream_error 既可能是 502 也可能是 500），分开计数才不会把
	// 「上游报错」和「上游不可达」混成一堆。
	HTTPStatus int   `json:"http_status"`
	Count      int64 `json:"count"`
	// LastTs 是这一组里最近一次失败的时间（毫秒）。用来回答
	// 「这个错还在发生吗」——只有历史堆里才有、现在不再出现的错，
	// 和正在持续报的错，处置优先级完全不同。
	LastTs int64 `json:"last_ts"`
}

// @ts-type UsageFailureUpstreamEntry
//
// 按上游聚合的一行。回答第二问：**是不是某个上游在坏**。
//
// 与按错误码分开而不是合并成一个多维 GROUP BY：合并后每个「上游 × 错误码」
// 占一行，上游多的时候真正的问题会被切成碎片，反而看不出「这家整体不行」。
type usageFailureUpstreamEntry struct {
	ProviderID string `json:"provider_id"`
	// ProviderName 是落库时固化的展示名，上游删除后仍在。
	// 同一 id 在历史里改过名时取字典序较大者——只影响显示，不影响计数。
	ProviderName string `json:"provider_name"`
	Total        int64  `json:"total"`
	Failed       int64  `json:"failed"`
	// Rate 是失败率（0–1），由后端算好下发。
	//
	// 不让前端现算的理由与 tps 相同：同一个公式存在两处实现，改一处忘一处时
	// 两个页面会静默分叉，而对不上时没人知道该信哪个。除零边界也只留一处。
	Rate float64 `json:"rate"`
	// LastFailTs 是该上游最近一次失败的时间（毫秒）；无失败时为 0。
	LastFailTs int64 `json:"last_fail_ts"`
}

// @ts-type UsageFailures
//
// 失败原因聚合的响应（GET /admin/api/usage/failures）。
type usageFailuresResponse struct {
	From   int64 `json:"from"`
	To     int64 `json:"to"`
	Total  int64 `json:"total"`
	Failed int64 `json:"failed"`
	// Rate 是区间整体失败率（0–1），后端算好。
	Rate      float64                     `json:"rate"`
	Codes     []usageFailureCodeEntry     `json:"codes"`
	Upstreams []usageFailureUpstreamEntry `json:"upstreams"`
}

// Failures 汇总区间内的失败构成（GET /admin/api/usage/failures）。
//
// # 为什么直查明细表，不像其它 by-* 那样走 UsageSource
//
// UsageSource 是「明细 ∪ 日归档」的归一化来源，但归档行只有聚合计数
// （n / n_ok / n_err），**没有 error_code、provider_id 这些维度**。
// 失败原因恰恰只能按维度回答，所以这里用不了归一化来源。
//
// 代价必须说清楚：结果只覆盖明细保留期（默认 30 天，见
// config.usage_retention_days）。更早的失败在归档里只剩「失败了多少次」，
// 说不出「因为什么」。这个限制是数据本身的性质，不是实现偷懒，
// 所以前端文案要能自洽，不能让人以为「30 天前没有失败」。
//
// # 收窄
//
// 普通用户只能看自己的（callerScope 返回 user_id），管理员看全站。
// 与其它 usage 端点同一个口径，不另立一套。
func (h *UsageHandler) Failures(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	// 默认 24 小时：这是一个**排障**视图，「今天哪里在坏」比「这个月趋势」
	// 更常用。用户仍可用 from/to 显式指定区间。
	from, to, _, err := queryRangeExplicit(q, 24*time.Hour)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	limit := clampLimit(q.Get("limit"), 10, 50)

	where := "ts >= ? AND ts <= ?"
	args := []any{from, to}
	if scope := callerScope(r); scope != "" {
		where += " AND user_id = ?"
		args = append(args, scope)
	}

	resp := usageFailuresResponse{
		From:      from,
		To:        to,
		Codes:     []usageFailureCodeEntry{},
		Upstreams: []usageFailureUpstreamEntry{},
	}

	// 1) 区间整体：总请求与失败数。一次扫出，避免再发一条只为算比率的查询。
	if err := h.store.DB().QueryRowContext(r.Context(),
		`SELECT COUNT(*), COALESCE(SUM(CASE WHEN `+usageFailureCond+` THEN 1 ELSE 0 END), 0)
		   FROM usage_records WHERE `+where, args...,
	).Scan(&resp.Total, &resp.Failed); err != nil {
		writeError(w, http.StatusInternalServerError, "读取失败汇总: "+err.Error())
		return
	}
	if resp.Total > 0 {
		resp.Rate = float64(resp.Failed) / float64(resp.Total)
	}

	// 2) 按错误码。
	codeArgs := append(append([]any{}, args...), limit)
	codeRows, err := h.store.DB().QueryContext(r.Context(),
		`SELECT COALESCE(NULLIF(error_code, ''), status) AS code,
		        http_status,
		        COUNT(*) AS n,
		        MAX(ts) AS last_ts
		   FROM usage_records
		  WHERE `+where+` AND `+usageFailureCond+`
		  GROUP BY code, http_status
		  ORDER BY n DESC, code ASC
		  LIMIT ?`, codeArgs...)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "读取失败错误码: "+err.Error())
		return
	}
	defer codeRows.Close()
	for codeRows.Next() {
		var e usageFailureCodeEntry
		if err := codeRows.Scan(&e.Code, &e.HTTPStatus, &e.Count, &e.LastTs); err != nil {
			writeError(w, http.StatusInternalServerError, "解析失败错误码: "+err.Error())
			return
		}
		resp.Codes = append(resp.Codes, e)
	}
	if err := codeRows.Err(); err != nil {
		writeError(w, http.StatusInternalServerError, "遍历失败错误码: "+err.Error())
		return
	}

	// 3) 按上游。HAVING 用完整表达式而不是别名 `failed`：别名可用是 SQLite
	// 的扩展行为，写全表达式在任何方言下都成立。
	//
	// 顺带回答「这家是不是整体不行」：rate 的分母是该上游**全部**请求，
	// 不是只有失败的请求 —— 用失败数当分母会得到恒等于 1 的废数。
	upArgs := append(append([]any{}, args...), limit)
	upRows, err := h.store.DB().QueryContext(r.Context(),
		`SELECT provider_id,
		        COALESCE(MAX(provider_name), '') AS name,
		        COUNT(*) AS total,
		        SUM(CASE WHEN `+usageFailureCond+` THEN 1 ELSE 0 END) AS failed,
		        COALESCE(MAX(CASE WHEN `+usageFailureCond+` THEN ts END), 0) AS last_fail_ts
		   FROM usage_records
		  WHERE `+where+`
		  GROUP BY provider_id
		 HAVING SUM(CASE WHEN `+usageFailureCond+` THEN 1 ELSE 0 END) > 0
		  ORDER BY failed DESC, provider_id ASC
		  LIMIT ?`, upArgs...)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "读取失败上游: "+err.Error())
		return
	}
	defer upRows.Close()
	for upRows.Next() {
		var e usageFailureUpstreamEntry
		if err := upRows.Scan(&e.ProviderID, &e.ProviderName, &e.Total, &e.Failed, &e.LastFailTs); err != nil {
			writeError(w, http.StatusInternalServerError, "解析失败上游: "+err.Error())
			return
		}
		if e.Total > 0 {
			e.Rate = float64(e.Failed) / float64(e.Total)
		}
		resp.Upstreams = append(resp.Upstreams, e)
	}
	if err := upRows.Err(); err != nil {
		writeError(w, http.StatusInternalServerError, "遍历失败上游: "+err.Error())
		return
	}

	writeJSON(w, http.StatusOK, resp)
}
