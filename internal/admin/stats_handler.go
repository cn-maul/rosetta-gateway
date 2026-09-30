package admin

import (
	"net/http"
	"strconv"

	"github.com/cn-maul/rosetta-gateway/internal/config"
	"github.com/cn-maul/rosetta-gateway/internal/snapshot"
	"github.com/cn-maul/rosetta-gateway/internal/store"
	"github.com/cn-maul/rosetta-gateway/internal/upstream"
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
	tps, _ := h.store.GetRecentThroughput(r.Context(), 5)
	ttfb, _ := h.store.GetRecentTtfbMs(r.Context(), 5)
	hitRate, _ := h.store.CacheHitRate(r.Context(), from, to)
	writeJSON(w, http.StatusOK, statsResponse{
		TotalRequests:   stats.TotalRequests,
		TotalTokens:     stats.TotalTokens,
		InputTokens:     stats.InputTokens,
		OutputTokens:    stats.OutputTokens,
		CachedTokens:    stats.CachedTokens,
		CacheHitRate:    hitRate,
		ErrorCount:      stats.ErrorCount,
		AvgTokensPerSec: tps,
		AvgTtfbMs:       ttfb,
		Cost:            stats.Cost,
	})
}

type ReloadHandler struct {
	store     *store.Store
	masterKey []byte
	pool      *upstream.Pool
	cfg       *config.Config
}

func NewReloadHandler(st *store.Store, masterKey []byte, pool *upstream.Pool, cfg *config.Config) *ReloadHandler {
	return &ReloadHandler{store: st, masterKey: masterKey, pool: pool, cfg: cfg}
}

func (h *ReloadHandler) Reload(w http.ResponseWriter, r *http.Request) {
	if h.pool != nil {
		if err := h.pool.BuildFromStore(r.Context(), h.store, h.masterKey, h.cfg); err != nil {
			writeServerError(w, "rebuild upstream pool", err)
			return
		}
	}
	snap, err := snapshot.RebuildFromDB(r.Context(), h.store)
	if err != nil {
		writeServerError(w, "rebuild snapshot", err)
		return
	}
	snapshot.Swap(snap)
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}
