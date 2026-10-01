package admin

import (
	"context"
	"net/http"
	"strconv"

	"github.com/cn-maul/rosetta-gateway/internal/store"
)

type StatsHandler struct {
	store *store.Store
}

func NewStatsHandler(st *store.Store) *StatsHandler {
	return &StatsHandler{store: st}
}

type statsResponse struct {
	TotalRequests   int64   `json:"total_requests"`
	TotalTokens     int64   `json:"total_tokens"`
	InputTokens     int64   `json:"input_tokens"`
	OutputTokens    int64   `json:"output_tokens"`
	CachedTokens    int64   `json:"cached_tokens"`
	CacheHitRate    float64 `json:"cache_hit_rate"`
	ErrorCount      int64   `json:"error_count"`
	AvgTokensPerSec float64 `json:"avg_tokens_per_sec"`
	AvgTtfbMs       float64 `json:"avg_ttfb_ms"`
	// Cost 为费用（元）：按各模型当前单价对区间内用量实时估算，
	// 未配置价格的模型按 0 计（见 store.GetUsageStats 的费用口径）。
	Cost float64 `json:"cost"`
}

func (h *StatsHandler) Get(w http.ResponseWriter, r *http.Request) {
	// from/to 为毫秒时间戳；from=0 表示统计全部历史（总览「全部」档）。
	from, _ := strconv.ParseInt(r.URL.Query().Get("from"), 10, 64)
	to, _ := strconv.ParseInt(r.URL.Query().Get("to"), 10, 64)

	stats, err := h.store.GetUsageStats(r.Context(), from, to)
	if err != nil {
		writeServerError(w, "usage stats", err)
		return
	}
	// 缓存命中率已并入 GetUsageStats 同一条 SELECT（见 usage_dao.go）：
	// 此前它是对同一区间 usage_records 的第二次独立全扫，纯属重复。
	tps, _ := h.store.GetRecentThroughput(r.Context(), 5)
	ttfb, _ := h.store.GetRecentTtfbMs(r.Context(), 5)
	writeJSON(w, http.StatusOK, statsResponse{
		TotalRequests:   stats.TotalRequests,
		TotalTokens:     stats.TotalTokens,
		InputTokens:     stats.InputTokens,
		OutputTokens:    stats.OutputTokens,
		CachedTokens:    stats.CachedTokens,
		CacheHitRate:    stats.CacheHitRate,
		ErrorCount:      stats.ErrorCount,
		AvgTokensPerSec: tps,
		AvgTtfbMs:       ttfb,
		Cost:            stats.Cost,
	})
}

// ReloadHandler 处理手动触发的 POST /admin/api/reload。
// 实际重建逻辑由注入的 reload 函数提供（cmd/gateway 的 runtimeReloader），
// 与管理写操作后的自动 reload（server.AutoReload）共用同一把串行锁。
type ReloadHandler struct {
	reload func(ctx context.Context) error
}

func NewReloadHandler(reload func(ctx context.Context) error) *ReloadHandler {
	return &ReloadHandler{reload: reload}
}

func (h *ReloadHandler) Reload(w http.ResponseWriter, r *http.Request) {
	if err := h.reload(r.Context()); err != nil {
		writeServerError(w, "reload runtime", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}
