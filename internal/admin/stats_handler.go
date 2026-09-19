package admin

import (
	"net/http"

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
	ErrorCount      int64   `json:"error_count"`
	AvgTokensPerSec float64 `json:"avg_tokens_per_sec"`
	AvgTtfbMs       float64 `json:"avg_ttfb_ms"`
}

func (h *StatsHandler) Get(w http.ResponseWriter, r *http.Request) {
	stats, err := h.store.GetUsageStats(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	tps, _ := h.store.GetRecentThroughput(r.Context(), 5)
	ttfb, _ := h.store.GetRecentTtfbMs(r.Context(), 5)
	writeJSON(w, http.StatusOK, statsResponse{
		TotalRequests:   stats.TotalRequests,
		TotalTokens:     stats.TotalTokens,
		InputTokens:     stats.InputTokens,
		OutputTokens:    stats.OutputTokens,
		ErrorCount:      stats.ErrorCount,
		AvgTokensPerSec: tps,
		AvgTtfbMs:       ttfb,
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
			writeError(w, http.StatusInternalServerError, "failed to rebuild upstream pool: "+err.Error())
			return
		}
	}
	snap, err := snapshot.RebuildFromDB(r.Context(), h.store, h.pool, h.masterKey, nil)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to rebuild snapshot: "+err.Error())
		return
	}
	snapshot.Swap(snap)
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}
