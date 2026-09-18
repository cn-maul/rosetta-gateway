package admin

import (
	"context"
	"net/http"

	"github.com/cn-maul/rosetta-gateway/internal/snapshot"
	"github.com/cn-maul/rosetta-gateway/internal/store"
)

type StatsHandler struct {
	store *store.Store
}

func NewStatsHandler(st *store.Store) *StatsHandler {
	return &StatsHandler{store: st}
}

type statsResponse struct {
	TotalRequests int64 `json:"total_requests"`
	TotalTokens   int64 `json:"total_tokens"`
	InputTokens   int64 `json:"input_tokens"`
	OutputTokens  int64 `json:"output_tokens"`
	ErrorCount    int64 `json:"error_count"`
}

func (h *StatsHandler) Get(w http.ResponseWriter, r *http.Request) {
	stats, err := h.store.GetUsageStats(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, statsResponse{
		TotalRequests: stats.TotalRequests,
		TotalTokens:   stats.TotalTokens,
		InputTokens:   stats.InputTokens,
		OutputTokens:  stats.OutputTokens,
		ErrorCount:    stats.ErrorCount,
	})
}

type ReloadHandler struct {
	store   *store.Store
	masterKey []byte
}

func NewReloadHandler(st *store.Store, masterKey []byte) *ReloadHandler {
	return &ReloadHandler{store: st, masterKey: masterKey}
}

func (h *ReloadHandler) Reload(w http.ResponseWriter, r *http.Request) {
	snap, err := snapshot.RebuildFromDB(r.Context(), h.store, nil, h.masterKey, nil)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to rebuild snapshot: "+err.Error())
		return
	}
	snapshot.Swap(snap)
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func init() {
	_ = context.Background
}
