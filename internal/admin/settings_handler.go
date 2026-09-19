package admin

import (
	"net/http"

	"github.com/cn-maul/rosetta-gateway/internal/store"
)

type SettingsHandler struct {
	store *store.Store
}

func NewSettingsHandler(st *store.Store) *SettingsHandler {
	return &SettingsHandler{store: st}
}

type modelDefaultsResponse struct {
	DefaultContextWindow   int `json:"default_context_window"`
	DefaultMaxOutputTokens int `json:"default_max_output_tokens"`
}

func (h *SettingsHandler) Get(w http.ResponseWriter, r *http.Request) {
	d, err := h.store.GetModelDefaults(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, modelDefaultsResponse{
		DefaultContextWindow:   d.ContextWindow,
		DefaultMaxOutputTokens: d.MaxOutputTokens,
	})
}

func (h *SettingsHandler) Update(w http.ResponseWriter, r *http.Request) {
	var req modelDefaultsResponse
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if req.DefaultContextWindow <= 0 || req.DefaultMaxOutputTokens <= 0 {
		writeError(w, http.StatusBadRequest, "默认上下文与最大输出必须为正整数")
		return
	}
	d := store.ModelDefaults{
		ContextWindow:   req.DefaultContextWindow,
		MaxOutputTokens: req.DefaultMaxOutputTokens,
	}
	if err := h.store.SetModelDefaults(r.Context(), d); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, req)
}
