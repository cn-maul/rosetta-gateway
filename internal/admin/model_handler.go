package admin

import (
	"context"
	"errors"
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
	ModelID         *string `json:"model_id"`
	DisplayName     *string `json:"display_name"`
	Enabled         *bool   `json:"enabled"`
	ContextWindow   *int    `json:"context_window"`
	MaxOutputTokens *int    `json:"max_output_tokens"`

	// 单价（元 / 百万 tokens），0 = 不计费。PATCH 语义同样是 nil = 保持原值、
	// 0 = 显式清空。price_input 是缓存未命中的输入价，price_cache_hit 是缓存
	// 命中的输入价，price_output 是输出价。
	PriceInput    *float64 `json:"price_input"`
	PriceCacheHit *float64 `json:"price_cache_hit"`
	PriceOutput   *float64 `json:"price_output"`
}

type modelResponse struct {
	ID              string  `json:"id"`
	ProviderID      string  `json:"provider_id"`
	ModelID         string  `json:"model_id"`
	DisplayName     string  `json:"display_name"`
	Enabled         bool    `json:"enabled"`
	ContextWindow   int     `json:"context_window"`
	MaxOutputTokens int     `json:"max_output_tokens"`
	PriceInput      float64 `json:"price_input"`
	PriceCacheHit   float64 `json:"price_cache_hit"`
	PriceOutput     float64 `json:"price_output"`
	TokensPerSec    float64 `json:"tokens_per_sec,omitempty"`
	TtfbMs          float64 `json:"ttfb_ms,omitempty"`
	SuccessRate     float64 `json:"success_rate"`
	CallCount       int     `json:"call_count,omitempty"`
}

func (h *ModelHandler) List(w http.ResponseWriter, r *http.Request, providerID string) {
	if !requireProvider(w, r, h.store, providerID) {
		return
	}

	models, err := h.store.ListUpstreamModels(r.Context(), providerID)
	if err != nil {
		writeServerError(w, "list upstream models", err)
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

// ListAll 扁平返回全部上游模型（跨所有 provider），供前端一次取全、本地按
// provider_id 分组。此前 Routes/Settings 页先取 providers、再对每个 provider
// 各发一次 models 请求（1+N）；模型总量不大，一次取回即可。
//
// 刻意不带 per-provider 的吞吐/成功率：那是窗口函数重查询（见 ListModelThroughput），
// 且这两个页面只用到 id/provider_id/model_id/display_name/价格，用不到统计。
func (h *ModelHandler) ListAll(w http.ResponseWriter, r *http.Request) {
	models, err := h.store.ListAllUpstreamModels(r.Context())
	if err != nil {
		writeServerError(w, "list all upstream models", err)
		return
	}
	result := make([]modelResponse, 0, len(models))
	for _, m := range models {
		result = append(result, toModelResponse(m))
	}
	writeJSON(w, http.StatusOK, result)
}

func (h *ModelHandler) Create(w http.ResponseWriter, r *http.Request, providerID string) {
	if !requireProvider(w, r, h.store, providerID) {
		return
	}

	var req modelRequest
	if err := decodeJSON(w, r, &req); err != nil {
		if errors.Is(err, errUnsupportedMediaType) {
			return
		}
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

	priceIn := derefFloat(req.PriceInput)
	priceHit := derefFloat(req.PriceCacheHit)
	priceOut := derefFloat(req.PriceOutput)
	if priceIn < 0 || priceHit < 0 || priceOut < 0 {
		writeError(w, http.StatusBadRequest, "price cannot be negative")
		return
	}

	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}

	m := &store.UpstreamModel{
		ID:              generateID(),
		ProviderID:      providerID,
		ModelID:         modelID,
		DisplayName:     strings.TrimSpace(derefStr(req.DisplayName)),
		Enabled:         enabled,
		ContextWindow:   ctxWindow,
		MaxOutputTokens: maxOut,
		PriceInput:      priceIn,
		PriceCacheHit:   priceHit,
		PriceOutput:     priceOut,
	}

	if err := h.store.CreateUpstreamModel(r.Context(), m); err != nil {
		if store.IsUniqueViolation(err) {
			writeError(w, http.StatusConflict, "该上游下已存在同名模型")
			return
		}
		writeServerError(w, "create upstream model", err)
		return
	}

	writeJSON(w, http.StatusCreated, toModelResponse(*m))
}

func (h *ModelHandler) Update(w http.ResponseWriter, r *http.Request, id string) {
	existing, err := h.store.GetUpstreamModel(r.Context(), id)
	if err != nil {
		writeServerError(w, "get upstream model", err)
		return
	}
	if existing == nil {
		writeError(w, http.StatusNotFound, "model not found")
		return
	}

	var req modelRequest
	if err := decodeJSON(w, r, &req); err != nil {
		if errors.Is(err, errUnsupportedMediaType) {
			return
		}
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
	// 单价：nil = 不改，0 = 清空（不计费），负数非法。
	for _, p := range []struct {
		name string
		req  *float64
		dst  *float64
	}{
		{"price_input", req.PriceInput, &existing.PriceInput},
		{"price_cache_hit", req.PriceCacheHit, &existing.PriceCacheHit},
		{"price_output", req.PriceOutput, &existing.PriceOutput},
	} {
		if p.req == nil {
			continue
		}
		if *p.req < 0 {
			writeError(w, http.StatusBadRequest, p.name+" cannot be negative")
			return
		}
		*p.dst = *p.req
	}

	if err := h.store.UpdateUpstreamModel(r.Context(), id, existing); err != nil {
		if store.IsUniqueViolation(err) {
			writeError(w, http.StatusConflict, "该上游下已存在同名模型")
			return
		}
		writeNotFoundOrError(w, "update upstream model", "上游模型不存在（可能已被并发删除）", err)
		return
	}

	writeJSON(w, http.StatusOK, toModelResponse(*existing))
}

func (h *ModelHandler) Delete(w http.ResponseWriter, r *http.Request, id string) {
	if err := h.store.DeleteUpstreamModel(r.Context(), id); err != nil {
		writeDeleteError(w, "delete upstream model", err)
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
	if !requireProvider(w, r, h.store, providerID) {
		return
	}

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
	if !requireProvider(w, r, h.store, providerID) {
		return
	}

	var body struct {
		Models []modelImportItem `json:"models"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		if errors.Is(err, errUnsupportedMediaType) {
			return
		}
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}

	def, err := h.store.GetModelDefaults(r.Context())
	if err != nil {
		writeServerError(w, "get model defaults", err)
		return
	}

	models := make([]*store.UpstreamModel, 0, len(body.Models))
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
		models = append(models, &store.UpstreamModel{
			ID:              generateID(),
			ProviderID:      providerID,
			ModelID:         strings.TrimSpace(it.ModelID),
			DisplayName:     it.DisplayName,
			Enabled:         true,
			ContextWindow:   ctxWindow,
			MaxOutputTokens: maxOut,
		})
	}

	if err := h.store.ImportUpstreamModels(r.Context(), models); err != nil {
		writeServerError(w, "import upstream models", err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "imported": len(models)})
}

func toModelResponse(m store.UpstreamModel) modelResponse {
	return modelResponse{
		ID:              m.ID,
		ProviderID:      m.ProviderID,
		ModelID:         m.ModelID,
		DisplayName:     m.DisplayName,
		Enabled:         m.Enabled,
		ContextWindow:   m.ContextWindow,
		MaxOutputTokens: m.MaxOutputTokens,
		PriceInput:      m.PriceInput,
		PriceCacheHit:   m.PriceCacheHit,
		PriceOutput:     m.PriceOutput,
	}
}
