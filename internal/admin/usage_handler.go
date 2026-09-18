package admin

import (
	"database/sql"
	"net/http"
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

type usageQueryResponse struct {
	Records []usageRecordEntry `json:"records"`
	Summary usageSummary        `json:"summary"`
}

type usageRecordEntry struct {
	Ts             int64  `json:"ts"`
	PublicModel    string `json:"public_model"`
	ProviderID     string `json:"provider_id"`
	UpstreamModel  string `json:"upstream_model"`
	IngressProtocol string `json:"ingress_protocol"`
	Stream         bool   `json:"stream"`
	InputTokens    int64  `json:"input_tokens"`
	OutputTokens   int64  `json:"output_tokens"`
	TotalTokens    int64  `json:"total_tokens"`
	Status         string `json:"status"`
	LatencyMs      int64  `json:"latency_ms"`
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
	fromStr := r.URL.Query().Get("from")
	toStr := r.URL.Query().Get("to")
	keyID := r.URL.Query().Get("key_id")
	model := r.URL.Query().Get("model")
	providerID := r.URL.Query().Get("provider_id")
	groupBy := r.URL.Query().Get("group_by")
	limitStr := r.URL.Query().Get("limit")

	from, _ := strconv.ParseInt(fromStr, 10, 64)
	to, _ := strconv.ParseInt(toStr, 10, 64)
	limit, _ := strconv.ParseInt(limitStr, 10, 64)

	if from == 0 {
		from = time.Now().Add(-24 * time.Hour).UnixMilli()
	}
	if to == 0 {
		to = time.Now().UnixMilli()
	}
	if limit == 0 {
		limit = 100
	}

	query := `SELECT ts, public_model, provider_id, upstream_model, ingress_protocol, stream, input_tokens, output_tokens, total_tokens, status, latency_ms FROM usage_records WHERE ts >= ? AND ts <= ?`
	args := []any{from, to}

	if keyID != "" {
		query += ` AND access_key_id = ?`
		args = append(args, keyID)
	}
	if model != "" {
		query += ` AND public_model = ?`
		args = append(args, model)
	}
	if providerID != "" {
		query += ` AND provider_id = ?`
		args = append(args, providerID)
	}

	if groupBy != "" {
		switch groupBy {
		case "day":
			query = `SELECT (ts / 86400000) * 86400000 as ts, public_model, provider_id, upstream_model, ingress_protocol, stream, SUM(input_tokens), SUM(output_tokens), SUM(total_tokens), status, AVG(latency_ms) FROM usage_records WHERE ts >= ? AND ts <= ? GROUP BY (ts / 86400000), status`
			args = []any{from, to}
		case "key":
			query = `SELECT ts, public_model, provider_id, upstream_model, ingress_protocol, stream, SUM(input_tokens), SUM(output_tokens), SUM(total_tokens), status, AVG(latency_ms) FROM usage_records WHERE ts >= ? AND ts <= ? GROUP BY access_key_id, status`
			args = []any{from, to}
		case "model":
			query = `SELECT ts, public_model, provider_id, upstream_model, ingress_protocol, stream, SUM(input_tokens), SUM(output_tokens), SUM(total_tokens), status, AVG(latency_ms) FROM usage_records WHERE ts >= ? AND ts <= ? GROUP BY public_model, status`
			args = []any{from, to}
		case "provider":
			query = `SELECT ts, public_model, provider_id, upstream_model, ingress_protocol, stream, SUM(input_tokens), SUM(output_tokens), SUM(total_tokens), status, AVG(latency_ms) FROM usage_records WHERE ts >= ? AND ts <= ? GROUP BY provider_id, status`
			args = []any{from, to}
		}
	}

	query += ` ORDER BY ts DESC LIMIT ?`
	args = append(args, limit)

	rows, err := h.store.DB().Query(query, args...)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer rows.Close()

	records := make([]usageRecordEntry, 0)
	var summary usageSummary
	var latencySum int64

	for rows.Next() {
		var rec usageRecordEntry
		if err := rows.Scan(&rec.Ts, &rec.PublicModel, &rec.ProviderID, &rec.UpstreamModel, &rec.IngressProtocol, &rec.Stream, &rec.InputTokens, &rec.OutputTokens, &rec.TotalTokens, &rec.Status, &rec.LatencyMs); err != nil {
			continue
		}
		records = append(records, rec)
		summary.TotalRequests++
		summary.TotalTokens += rec.TotalTokens
		summary.InputTokens += rec.InputTokens
		summary.OutputTokens += rec.OutputTokens
		latencySum += rec.LatencyMs
		if rec.Status != "ok" {
			summary.ErrorCount++
		}
	}

	// 平均延迟 = 延迟总和 / 请求数。原先此处误用 TotalTokens / TotalRequests，
	// 得到的是"每请求平均 token 数"而非延迟。
	if summary.TotalRequests > 0 {
		summary.AvgLatencyMs = latencySum / summary.TotalRequests
	}

	writeJSON(w, http.StatusOK, usageQueryResponse{
		Records: records,
		Summary: summary,
	})
}

type usageGroupEntry struct {
	Key   string `json:"key"`
	Count int64  `json:"count"`
	Tokens int64 `json:"tokens"`
}

func (h *UsageHandler) GroupByKey(w http.ResponseWriter, r *http.Request) {
	h.groupBy(w, r, "access_key_id")
}

func (h *UsageHandler) GroupByModel(w http.ResponseWriter, r *http.Request) {
	h.groupBy(w, r, "public_model")
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
	fromStr := r.URL.Query().Get("from")
	toStr := r.URL.Query().Get("to")

	from, _ := strconv.ParseInt(fromStr, 10, 64)
	to, _ := strconv.ParseInt(toStr, 10, 64)

	if from == 0 {
		from = time.Now().Add(-7 * 24 * time.Hour).UnixMilli()
	}
	if to == 0 {
		to = time.Now().UnixMilli()
	}

	// 按天分组时返回时间序列，必须按日期升序；
	// 其余维度按用量降序更有意义（找最耗量的 key / 模型）。
	order := "ORDER BY SUM(total_tokens) DESC"
	if strings.Contains(column, "strftime") {
		order = "ORDER BY key ASC"
	}

	query := `SELECT ` + column + ` as key, COUNT(*), SUM(total_tokens) FROM usage_records WHERE ts >= ? AND ts <= ? GROUP BY ` + column + ` ` + order

	rows, err := h.store.DB().Query(query, from, to)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer rows.Close()

	// 初始化为空切片而非 nil：nil 切片会被编码成 JSON null，
	// 客户端拿到 null 再 .map() 会直接 TypeError。空结果必须是 []。
	entries := make([]usageGroupEntry, 0)
	for rows.Next() {
		var e usageGroupEntry
		var key string
		if err := rows.Scan(&key, &e.Count, &e.Tokens); err != nil {
			continue
		}
		e.Key = key
		entries = append(entries, e)
	}

	writeJSON(w, http.StatusOK, entries)
}

var _ = sql.ErrNoRows
