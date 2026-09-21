package admin

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/cn-maul/rosetta-gateway/internal/config"
	"github.com/cn-maul/rosetta-gateway/internal/store"
	"github.com/cn-maul/rosetta-gateway/internal/upstream"
)

type ProviderHandler struct {
	store     *store.Store
	masterKey []byte
	cfg       *config.Config
}

func NewProviderHandler(st *store.Store, masterKey []byte, cfg *config.Config) *ProviderHandler {
	return &ProviderHandler{store: st, masterKey: masterKey, cfg: cfg}
}

// providerCreateRequest 是新建上游的输入。
// slug 由系统生成、enabled 固定 true、超时与重试取「设置」里的全局默认，
// 三者都不接受客户端指定 —— 所以不与 PATCH 共用结构体。
type providerCreateRequest struct {
	Name       string `json:"name"`
	Protocol   string `json:"protocol"`
	Endpoint   string `json:"endpoint"`
	QuirksJSON string `json:"quirks_json"`
	APIKey     string `json:"api_key"`
}

// providerUpdateRequest 是上游的 PATCH 输入。
//
// PATCH 语义（2026-09-21 重构）：字段为指针，nil = 未提供（保持原值）；
// 非 nil 即显式赋新值，TimeoutMs=0 / MaxRetries=0 是合法值
// —— 表示「回到全局默认」，旧实现下这两个字段一旦改过就再也回不去。
//
// 这里**没有 slug 字段**：slug 是日志、路由与管理界面对外引用的稳定标识，
// 创建后不可修改。客户端即使传了也会被忽略。
type providerUpdateRequest struct {
	Name       *string `json:"name"`
	Protocol   *string `json:"protocol"`
	Endpoint   *string `json:"endpoint"`
	Enabled    *bool   `json:"enabled"`
	TimeoutMs  *int    `json:"timeout_ms"`
	MaxRetries *int    `json:"max_retries"`
	QuirksJSON *string `json:"quirks_json"`
}

type providerResponse struct {
	ID         string `json:"id"`
	Slug       string `json:"slug"`
	Name       string `json:"name"`
	Protocol   string `json:"protocol"`
	Endpoint   string `json:"endpoint"`
	Enabled    bool   `json:"enabled"`
	TimeoutMs  int    `json:"timeout_ms"`
	MaxRetries int    `json:"max_retries"`
	QuirksJSON string `json:"quirks_json"`
	CreatedAt  int64  `json:"created_at"`
	UpdatedAt  int64  `json:"updated_at"`
}

func (h *ProviderHandler) List(w http.ResponseWriter, r *http.Request) {
	providers, err := h.store.ListProviders(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	result := make([]providerResponse, 0, len(providers))
	for _, p := range providers {
		result = append(result, toProviderResponse(p))
	}
	writeJSON(w, http.StatusOK, result)
}

// Create 只需显示名称、协议、Endpoint（以及可选的 api_key）。
// slug 由系统生成，启用固定为 true，超时与重试直接取设置里的默认值；
// 若填写了 api_key，会顺带创建一条名为 default 的凭据。
func (h *ProviderHandler) Create(w http.ResponseWriter, r *http.Request) {
	var req providerCreateRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}

	endpoint := strings.TrimSpace(req.Endpoint)
	if endpoint == "" {
		writeError(w, http.StatusBadRequest, "endpoint is required")
		return
	}

	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = endpoint
	}
	protocol := strings.TrimSpace(req.Protocol)
	if protocol == "" {
		protocol = "openai-chat"
	}

	now := time.Now().UnixMilli()
	p := &store.Provider{
		ID:         generateID(),
		Slug:       h.uniqueSlug(r.Context(), slugify(name)),
		Name:       name,
		Protocol:   protocol,
		Endpoint:   endpoint,
		Enabled:    true,
		TimeoutMs:  h.cfg.Defaults.UpstreamTimeoutMs,
		MaxRetries: h.cfg.Defaults.MaxRetries,
		QuirksJSON: req.QuirksJSON,
		CreatedAt:  now,
		UpdatedAt:  now,
	}

	if err := h.store.CreateProvider(r.Context(), p); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	if strings.TrimSpace(req.APIKey) != "" {
		enc, err := encryptSecret(strings.TrimSpace(req.APIKey), h.masterKey)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to encrypt key: "+err.Error())
			return
		}
		c := &store.Credential{
			ID:         generateID(),
			ProviderID: p.ID,
			Label:      "default",
			APIKeyEnc:  enc,
			Enabled:    true,
			Weight:     1,
			Status:     "healthy",
		}
		if err := h.store.CreateCredential(r.Context(), c); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}

	writeJSON(w, http.StatusCreated, toProviderResponse(*p))
}

func (h *ProviderHandler) Get(w http.ResponseWriter, r *http.Request, id string) {
	p, err := h.store.GetProvider(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if p == nil {
		writeError(w, http.StatusNotFound, "provider not found")
		return
	}
	writeJSON(w, http.StatusOK, toProviderResponse(*p))
}

func (h *ProviderHandler) Update(w http.ResponseWriter, r *http.Request, id string) {
	existing, err := h.store.GetProvider(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if existing == nil {
		writeError(w, http.StatusNotFound, "provider not found")
		return
	}

	var req providerUpdateRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}

	// 必填字段：显式提供时不允许置空
	if req.Name != nil {
		name := strings.TrimSpace(*req.Name)
		if name == "" {
			writeError(w, http.StatusBadRequest, "name cannot be empty")
			return
		}
		existing.Name = name
	}
	if req.Protocol != nil {
		protocol := strings.TrimSpace(*req.Protocol)
		if protocol == "" {
			writeError(w, http.StatusBadRequest, "protocol cannot be empty")
			return
		}
		existing.Protocol = protocol
	}
	if req.Endpoint != nil {
		endpoint := strings.TrimSpace(*req.Endpoint)
		if endpoint == "" {
			writeError(w, http.StatusBadRequest, "endpoint cannot be empty")
			return
		}
		existing.Endpoint = endpoint
	}
	if req.Enabled != nil {
		existing.Enabled = *req.Enabled
	}
	// 0 是合法值，语义为「回到全局默认」
	if req.TimeoutMs != nil {
		if *req.TimeoutMs < 0 {
			writeError(w, http.StatusBadRequest, "timeout_ms cannot be negative")
			return
		}
		existing.TimeoutMs = *req.TimeoutMs
	}
	if req.MaxRetries != nil {
		if *req.MaxRetries < 0 {
			writeError(w, http.StatusBadRequest, "max_retries cannot be negative")
			return
		}
		existing.MaxRetries = *req.MaxRetries
	}
	// 可清空字段
	if req.QuirksJSON != nil {
		existing.QuirksJSON = *req.QuirksJSON
	}

	if err := h.store.UpdateProvider(r.Context(), id, existing); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, toProviderResponse(*existing))
}

func (h *ProviderHandler) Delete(w http.ResponseWriter, r *http.Request, id string) {
	if err := h.store.DeleteProvider(r.Context(), id); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// Test 用即时构建的客户端拉取模型列表来验证连通性，不依赖内存池，
// 因此刚添加、尚未 reload 的上游也能立即测通。
func (h *ProviderHandler) Test(w http.ResponseWriter, r *http.Request, id string) {
	p, err := h.store.GetProvider(r.Context(), id)
	if err != nil || p == nil {
		writeError(w, http.StatusNotFound, "provider not found")
		return
	}

	client, err := upstream.NewProviderClient(r.Context(), h.store, id, h.masterKey, h.cfg)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]string{"status": "error", "message": err.Error()})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	start := time.Now()
	models, err := client.ListModels(ctx)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]string{"status": "error", "message": err.Error()})
		return
	}
	latencyMs := time.Since(start).Milliseconds()

	writeJSON(w, http.StatusOK, map[string]any{
		"status":      "ok",
		"message":     fmt.Sprintf("连接正常，%dms", latencyMs),
		"latency_ms":  latencyMs,
		"model_count": len(models),
	})
}

func (h *ProviderHandler) uniqueSlug(ctx context.Context, base string) string {
	slug := base
	for i := 2; ; i++ {
		p, err := h.store.GetProviderBySlug(ctx, slug)
		if err != nil || p == nil {
			return slug
		}
		slug = fmt.Sprintf("%s-%d", base, i)
	}
}

func slugify(s string) string {
	var b strings.Builder
	prevDash := true
	for _, r := range strings.ToLower(s) {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			prevDash = false
		case !prevDash:
			b.WriteByte('-')
			prevDash = true
		}
	}
	slug := strings.Trim(b.String(), "-")
	if slug == "" {
		slug = "provider"
	}
	return slug
}

func toProviderResponse(p store.Provider) providerResponse {
	return providerResponse{
		ID:         p.ID,
		Slug:       p.Slug,
		Name:       p.Name,
		Protocol:   p.Protocol,
		Endpoint:   p.Endpoint,
		Enabled:    p.Enabled,
		TimeoutMs:  p.TimeoutMs,
		MaxRetries: p.MaxRetries,
		QuirksJSON: p.QuirksJSON,
		CreatedAt:  p.CreatedAt,
		UpdatedAt:  p.UpdatedAt,
	}
}

func generateID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}
