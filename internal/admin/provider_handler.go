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

// validProtocols 是管理 API 接受的 protocol 取值，与 upstream.buildClient
// 里的 switch 一一对应（那里现在对未知值直接返回错误）。
//
// 在配置入口就挡住，是为了避免「保存成功、调用时才报错」：protocol 拼错
// （比如写成 "openai"）此前会被静默忽略、退化成 SDK 自动探测，请求打到错误的
// 端点形态上，报出来的错误指向下游，排查方向从一开始就是错的。
var validProtocols = []string{"auto", "openai-chat", "openai-responses", "anthropic"}

func validateProtocol(protocol string) error {
	for _, p := range validProtocols {
		if protocol == p {
			return nil
		}
	}
	return fmt.Errorf("protocol 必须是 %s 之一，收到 %q",
		strings.Join(validProtocols, " / "), protocol)
}

// writeProviderWriteError 把 providers 写操作的失败映射成合适的状态码。
func writeProviderWriteError(w http.ResponseWriter, action string, err error) {
	if store.IsUniqueViolation(err) {
		writeError(w, http.StatusConflict, "该上游标识（slug）已存在，请换个名称后重试")
		return
	}
	writeServerError(w, action, err)
}

func (h *ProviderHandler) List(w http.ResponseWriter, r *http.Request) {
	providers, err := h.store.ListProviders(r.Context())
	if err != nil {
		writeServerError(w, "list providers", err)
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
	if err := validateProtocol(protocol); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
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

	// 表单填了 api_key 就一并创建首条凭据。两半必须原子落库：
	// 先建 provider 再建凭据、第二步失败，会留下一个没有任何凭据的上游 ——
	// 界面报错、用户以为没建成功，它却已经在列表里，且永远不可用。
	var cred *store.Credential
	if apiKey := strings.TrimSpace(req.APIKey); apiKey != "" {
		enc, err := encryptSecret(apiKey, h.masterKey)
		if err != nil {
			writeServerError(w, "encrypt api key", err)
			return
		}
		cred = &store.Credential{
			ID:         generateID(),
			ProviderID: p.ID,
			Label:      "default",
			APIKeyEnc:  enc,
			Enabled:    true,
			Weight:     1,
			Status:     "healthy",
		}
	}

	if err := h.store.CreateProviderWithCredential(r.Context(), p, cred); err != nil {
		writeProviderWriteError(w, "create provider", err)
		return
	}

	writeJSON(w, http.StatusCreated, toProviderResponse(*p))
}

func (h *ProviderHandler) Get(w http.ResponseWriter, r *http.Request, id string) {
	p, err := h.store.GetProvider(r.Context(), id)
	if err != nil {
		writeServerError(w, "get provider", err)
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
		writeServerError(w, "get provider", err)
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
		if err := validateProtocol(protocol); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
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
		writeProviderWriteError(w, "update provider", err)
		return
	}

	writeJSON(w, http.StatusOK, toProviderResponse(*existing))
}

func (h *ProviderHandler) Delete(w http.ResponseWriter, r *http.Request, id string) {
	if err := h.store.DeleteProvider(r.Context(), id); err != nil {
		writeDeleteError(w, "delete provider", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// Test 用即时构建的客户端拉取模型列表来验证连通性，不依赖内存池，
// 因此刚添加、尚未 reload 的上游也能立即测通。
func (h *ProviderHandler) Test(w http.ResponseWriter, r *http.Request, id string) {
	p, err := h.store.GetProvider(r.Context(), id)
	if err != nil {
		writeServerError(w, "get provider", err)
		return
	}
	if p == nil {
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
