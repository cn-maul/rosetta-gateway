package admin

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/cn-maul/rosetta-gateway/internal/config"
	"github.com/cn-maul/rosetta-gateway/internal/store"
	"github.com/cn-maul/rosetta-gateway/internal/upstream"
)

type ModelHandler struct {
	store     *store.Store
	masterKey []byte
	cfg       *config.Config
}

func NewModelHandler(st *store.Store, masterKey []byte, cfg *config.Config) *ModelHandler {
	return &ModelHandler{store: st, masterKey: masterKey, cfg: cfg}
}

// modelRequest 是上游模型的创建 / PATCH 输入。
//
// PATCH 语义（2026-09-21 重构）：字段为指针，nil = 未提供（保持原值）。
// ContextWindow / MaxOutputTokens 传 0 是合法值 —— 会落回 NULL，即「未设置」，
// 旧实现下这两个字段一旦写入就再也无法清空。
type modelRequest struct {
	ModelID          *string `json:"model_id"`
	DisplayName      *string `json:"display_name"`
	Enabled          *bool   `json:"enabled"`
	ContextWindow    *int    `json:"context_window"`
	MaxOutputTokens  *int    `json:"max_output_tokens"`
	DefaultExtraJSON *string `json:"default_extra_json"`
}

type modelResponse struct {
	ID               string  `json:"id"`
	ProviderID       string  `json:"provider_id"`
	ModelID          string  `json:"model_id"`
	DisplayName      string  `json:"display_name"`
	Enabled          bool    `json:"enabled"`
	ContextWindow    int     `json:"context_window"`
	MaxOutputTokens  int     `json:"max_output_tokens"`
	DefaultExtraJSON string  `json:"default_extra_json"`
	TokensPerSec     float64 `json:"tokens_per_sec,omitempty"`
	TtfbMs           float64 `json:"ttfb_ms,omitempty"`
	SuccessRate      float64 `json:"success_rate"`
	CallCount        int     `json:"call_count,omitempty"`
}

func (h *ModelHandler) List(w http.ResponseWriter, r *http.Request, providerID string) {
	models, err := h.store.ListUpstreamModels(r.Context(), providerID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// 统计只来自历史真实调用，不做探测；未调用过的模型不在 map 中，速度/成功率留空。
	stats, _ := h.store.ListModelThroughput(r.Context(), providerID)
	result := make([]modelResponse, 0, len(models))
	for _, m := range models {
		resp := toModelResponse(m)
		if st, ok := stats[m.ModelID]; ok {
			resp.TokensPerSec = st.Tps
			resp.TtfbMs = st.TtfbMs
			resp.SuccessRate = st.SuccessRate
			resp.CallCount = st.SuccessSample
		}
		result = append(result, resp)
	}
	writeJSON(w, http.StatusOK, result)
}

func (h *ModelHandler) Create(w http.ResponseWriter, r *http.Request, providerID string) {
	var req modelRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}

	modelID := strings.TrimSpace(derefStr(req.ModelID))
	if modelID == "" {
		writeError(w, http.StatusBadRequest, "model_id is required")
		return
	}

	ctxWindow := derefInt(req.ContextWindow)
	maxOut := derefInt(req.MaxOutputTokens)
	if ctxWindow < 0 || maxOut < 0 {
		writeError(w, http.StatusBadRequest, "context_window and max_output_tokens cannot be negative")
		return
	}

	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}

	m := &store.UpstreamModel{
		ID:               generateID(),
		ProviderID:       providerID,
		ModelID:          modelID,
		DisplayName:      strings.TrimSpace(derefStr(req.DisplayName)),
		Enabled:          enabled,
		ContextWindow:    ctxWindow,
		MaxOutputTokens:  maxOut,
		DefaultExtraJSON: derefStr(req.DefaultExtraJSON),
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

	if req.ModelID != nil {
		v := strings.TrimSpace(*req.ModelID)
		if v == "" {
			writeError(w, http.StatusBadRequest, "model_id cannot be empty")
			return
		}
		existing.ModelID = v
	}
	// 可清空字段：空串即清空
	if req.DisplayName != nil {
		existing.DisplayName = strings.TrimSpace(*req.DisplayName)
	}
	if req.Enabled != nil {
		existing.Enabled = *req.Enabled
	}
	// 传 0 = 显式清空（落 NULL）；负数是非法输入
	if req.ContextWindow != nil {
		if *req.ContextWindow < 0 {
			writeError(w, http.StatusBadRequest, "context_window cannot be negative")
			return
		}
		existing.ContextWindow = *req.ContextWindow
	}
	if req.MaxOutputTokens != nil {
		if *req.MaxOutputTokens < 0 {
			writeError(w, http.StatusBadRequest, "max_output_tokens cannot be negative")
			return
		}
		existing.MaxOutputTokens = *req.MaxOutputTokens
	}
	if req.DefaultExtraJSON != nil {
		existing.DefaultExtraJSON = *req.DefaultExtraJSON
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

type discoveredModel struct {
	ModelID         string `json:"model_id"`
	DisplayName     string `json:"display_name,omitempty"`
	ContextWindow   int    `json:"context_window,omitempty"`
	MaxOutputTokens int    `json:"max_output_tokens,omitempty"`
}

// Discover 直接拉取上游原始 /models，带出各家扩展的上下文/最大输出容量；
// 探测不到的条目用「设置」里的默认容量兜底，一并返回给前端。
func (h *ModelHandler) Discover(w http.ResponseWriter, r *http.Request, providerID string) {
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()

	cands, err := upstream.DiscoverUpstreamModels(ctx, h.store, providerID, h.masterKey, h.cfg)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]string{"status": "error", "message": err.Error()})
		return
	}

	def, err := h.store.GetModelDefaults(ctx)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]string{"status": "error", "message": err.Error()})
		return
	}

	result := make([]discoveredModel, 0, len(cands))
	for _, c := range cands {
		name := c.DisplayName
		if name == "" {
			name = c.ID
		}
		ctxWindow := c.ContextWindow
		if ctxWindow <= 0 {
			ctxWindow = def.ContextWindow
		}
		maxOut := c.MaxOutputTokens
		if maxOut <= 0 {
			maxOut = def.MaxOutputTokens
		}
		result = append(result, discoveredModel{
			ModelID:         c.ID,
			DisplayName:     name,
			ContextWindow:   ctxWindow,
			MaxOutputTokens: maxOut,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "models": result})
}

// modelImportItem 是批量导入的单条模型。
type modelImportItem struct {
	ModelID         string `json:"model_id"`
	DisplayName     string `json:"display_name"`
	ContextWindow   int    `json:"context_window"`
	MaxOutputTokens int    `json:"max_output_tokens"`
}

// ImportModels 批量 upsert 模型（按 provider_id+model_id 幂等），一次写入避免逐条 reload。
func (h *ModelHandler) ImportModels(w http.ResponseWriter, r *http.Request, providerID string) {
	var body struct {
		Models []modelImportItem `json:"models"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}

	def, err := h.store.GetModelDefaults(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	imported := 0
	for _, it := range body.Models {
		if strings.TrimSpace(it.ModelID) == "" {
			continue
		}
		ctxWindow := it.ContextWindow
		if ctxWindow <= 0 {
			ctxWindow = def.ContextWindow
		}
		maxOut := it.MaxOutputTokens
		if maxOut <= 0 {
			maxOut = def.MaxOutputTokens
		}
		m := &store.UpstreamModel{
			ID:              generateID(),
			ProviderID:      providerID,
			ModelID:         strings.TrimSpace(it.ModelID),
			DisplayName:     it.DisplayName,
			Enabled:         true,
			ContextWindow:   ctxWindow,
			MaxOutputTokens: maxOut,
		}
		if err := h.store.UpsertUpstreamModel(r.Context(), m); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		imported++
	}

	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "imported": imported})
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
