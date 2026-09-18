package admin

import (
	"net/http"

	"github.com/cn-maul/rosetta-gateway/internal/store"
)

type ModelHandler struct {
	store *store.Store
}

func NewModelHandler(st *store.Store) *ModelHandler {
	return &ModelHandler{store: st}
}

type modelRequest struct {
	ModelID          string `json:"model_id"`
	DisplayName      string `json:"display_name"`
	Enabled          *bool  `json:"enabled"`
	ContextWindow    int    `json:"context_window"`
	MaxOutputTokens  int    `json:"max_output_tokens"`
	DefaultExtraJSON string `json:"default_extra_json"`
}

type modelResponse struct {
	ID               string `json:"id"`
	ProviderID       string `json:"provider_id"`
	ModelID          string `json:"model_id"`
	DisplayName      string `json:"display_name"`
	Enabled          bool   `json:"enabled"`
	ContextWindow    int    `json:"context_window"`
	MaxOutputTokens  int    `json:"max_output_tokens"`
	DefaultExtraJSON string `json:"default_extra_json"`
}

func (h *ModelHandler) List(w http.ResponseWriter, r *http.Request, providerID string) {
	models, err := h.store.ListUpstreamModels(r.Context(), providerID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	result := make([]modelResponse, 0, len(models))
	for _, m := range models {
		result = append(result, toModelResponse(m))
	}
	writeJSON(w, http.StatusOK, result)
}

func (h *ModelHandler) Create(w http.ResponseWriter, r *http.Request, providerID string) {
	var req modelRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}

	if req.ModelID == "" {
		writeError(w, http.StatusBadRequest, "model_id is required")
		return
	}

	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}

	m := &store.UpstreamModel{
		ID:               generateID(),
		ProviderID:       providerID,
		ModelID:          req.ModelID,
		DisplayName:      req.DisplayName,
		Enabled:          enabled,
		ContextWindow:    req.ContextWindow,
		MaxOutputTokens:  req.MaxOutputTokens,
		DefaultExtraJSON: req.DefaultExtraJSON,
	}

	if err := h.store.CreateUpstreamModel(r.Context(), m); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusCreated, toModelResponse(*m))
}

func (h *ModelHandler) Update(w http.ResponseWriter, r *http.Request, id string) {
	existing, err := h.store.GetUpstreamModel(r.Context(), id)
	if err != nil || existing == nil {
		writeError(w, http.StatusNotFound, "model not found")
		return
	}

	var req modelRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}

	if req.ModelID != "" {
		existing.ModelID = req.ModelID
	}
	if req.DisplayName != "" {
		existing.DisplayName = req.DisplayName
	}
	if req.Enabled != nil {
		existing.Enabled = *req.Enabled
	}
	if req.ContextWindow != 0 {
		existing.ContextWindow = req.ContextWindow
	}
	if req.MaxOutputTokens != 0 {
		existing.MaxOutputTokens = req.MaxOutputTokens
	}
	if req.DefaultExtraJSON != "" {
		existing.DefaultExtraJSON = req.DefaultExtraJSON
	}

	if err := h.store.UpdateUpstreamModel(r.Context(), id, existing); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, toModelResponse(*existing))
}

func (h *ModelHandler) Delete(w http.ResponseWriter, r *http.Request, id string) {
	if err := h.store.DeleteUpstreamModel(r.Context(), id); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (h *ModelHandler) Discover(w http.ResponseWriter, r *http.Request, providerID string) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "message": "model discovery requires upstream pool connection"})
}

func toModelResponse(m store.UpstreamModel) modelResponse {
	return modelResponse{
		ID:               m.ID,
		ProviderID:       m.ProviderID,
		ModelID:          m.ModelID,
		DisplayName:      m.DisplayName,
		Enabled:          m.Enabled,
		ContextWindow:    m.ContextWindow,
		MaxOutputTokens:  m.MaxOutputTokens,
		DefaultExtraJSON: m.DefaultExtraJSON,
	}
}
