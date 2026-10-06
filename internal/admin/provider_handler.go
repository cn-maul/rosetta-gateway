package admin

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/cn-maul/rosetta-gateway/internal/config"
	"github.com/cn-maul/rosetta-gateway/internal/snapshot"
	"github.com/cn-maul/rosetta-gateway/internal/store"
	"github.com/cn-maul/rosetta-gateway/internal/upstream"
	"net/http"
	"strings"
	"time"
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
	// Ready / ReadyReason 是**运行时**状态（P4 / 设计 §4.7），来自快照，
	// 不是库里的配置字段。
	//
	// 为什么要下发：上游池重建时若某 provider 的凭据解密失败或客户端建不起来，
	// 它在库里看起来一切正常（enabled=1、凭据在），但请求打过去必然失败。
	// 此前这类失败只进日志，界面一字不提，运维只能靠「莫名 500」反推根因。
	//
	// Ready=false 只在「一条可用凭据都没有」时出现；个别凭据失败但仍有可用
	// 上游时 Ready 仍为 true，只在 ReadyReason 里给一句降级说明。
	// 注意 disabled 的 provider 本来就不会被池构建，Ready 对它是 false 且
	// 无意义 —— 前端只在 enabled 时展示这个状态，避免「停用」被读成「故障」。
	Ready       bool   `json:"ready"`
	ReadyReason string `json:"ready_reason,omitempty"`
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
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "上游不存在（可能已被并发删除）")
		return
	}
	if store.IsUniqueViolation(err) {
		writeError(w, http.StatusConflict, "该上游标识（slug）已存在，请换个名称后重试")
		return
	}
	writeServerError(w, action, err)
}

// List 列出全部 provider，并附带**运行时**就绪状态。
//
// 就绪状态来自当前快照而不是库：库只记录配了什么，快照才反映「池实际建出了什么」。
// 两者不一致正是最该看见的信息（配了却没建起来 = 凭据问题）。
func (h *ProviderHandler) List(w http.ResponseWriter, r *http.Request) {
	providers, err := h.store.ListProviders(r.Context())
	if err != nil {
		writeServerError(w, "list providers", err)
		return
	}
	snap := snapshot.Get()
	result := make([]providerResponse, 0, len(providers))
	for _, p := range providers {
		res := toProviderResponse(p)
		if sp, ok := snap.Providers[p.Slug]; ok {
			res.Ready = sp.Ready
			res.ReadyReason = sp.Reason
		}
		result = append(result, res)
	}
	writeJSON(w, http.StatusOK, result)
}

// Create 只需显示名称、协议、Endpoint（以及可选的 api_key）。
// slug 由系统生成，启用固定为 true，超时与重试直接取设置里的默认值；
// 若填写了 api_key，会顺带创建一条名为 default 的凭据。
func (h *ProviderHandler) Create(w http.ResponseWriter, r *http.Request) {
	var req providerCreateRequest
	if err := decodeJSON(w, r, &req); err != nil {
		if errors.Is(err, errUnsupportedMediaType) {
			return
		}
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
	slug, err := h.uniqueSlug(r.Context(), slugify(name))
	if err != nil {
		writeServerError(w, "resolve unique slug", err)
		return
	}
	p := &store.Provider{
		ID:         generateID(),
		Slug:       slug,
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
	if err := decodeJSON(w, r, &req); err != nil {
		if errors.Is(err, errUnsupportedMediaType) {
			return
		}
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
	//
	// 上界不能少：time.Duration 是 int64 纳秒，ms > 9.223e12 时
	// `time.Duration(ms) * time.Millisecond`（internal/upstream 的两处转换）
	// 会回绕成负数，负 Duration 让 context.WithTimeout 立即过期、
	// time.AfterFunc 立即开火。修复前这里只拒负数，实测
	// PATCH {"timeout_ms":9223372036854} 照单全收（200）。
	// 与 config.validate、settings_handler 共用同一个常量。
	if req.TimeoutMs != nil {
		if *req.TimeoutMs < 0 {
			writeError(w, http.StatusBadRequest, "timeout_ms cannot be negative")
			return
		}
		if *req.TimeoutMs > config.MaxDurationMillis {
			writeError(w, http.StatusBadRequest, fmt.Sprintf(
				"timeout_ms 过大：%d（上限 %d）", *req.TimeoutMs, config.MaxDurationMillis))
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

// maxSlugLen 与 config 侧 bootstrap 的 slug 正则 `^[a-z0-9]{2,32}$` 一致。
// 超过就截断 —— slug 会进 /v1/models/{model} 路径解析、日志、以及
// routing.RouteIndex.providers 的键，无界增长没有任何收益。
const maxSlugLen = 32

// uniqueSlug 基于 base 生成一个未被占用的 slug：命中已有 slug 就追加 -2/-3…。
// GetProviderBySlug 对「不存在」返回 (nil, nil)，对真实 DB 故障返回 (nil, err)。
// 旧实现 `if err != nil || p == nil { return slug }` 把 err 当「可用」——查询挂了
// 会返回一个可能其实已被占用的 slug，最终撞上 UNIQUE 约束报成 500，掩盖真实原因。
//
// 追加后缀必须**重新截断**：`-2` 追加到 32 字符的 base 上就变成 34 字符，
// 与 config 侧的正则不一致；而继续往后加 -3/-4… 只会更长。所以先给 base
// 留出后缀空间，再截断。
func (h *ProviderHandler) uniqueSlug(ctx context.Context, base string) (string, error) {
	slug := base
	for i := 2; ; i++ {
		p, err := h.store.GetProviderBySlug(ctx, slug)
		if err != nil {
			return "", err
		}
		if p == nil {
			return slug, nil
		}
		next := fmt.Sprintf("%s-%d", base, i)
		if len(next) > maxSlugLen {
			// 数字后缀吃掉的位置从 base 尾部截，仍然唯一（后缀不同）。
			suffix := fmt.Sprintf("-%d", i)
			next = base[:maxSlugLen-len(suffix)] + suffix
			if len(next) > len(base) {
				// base 已经短到放不下后缀，用哈希兜底避免死循环。
				sum := sha256.Sum256([]byte(fmt.Sprintf("%s#%d", base, i)))
				next = base[:1] + "-" + hex.EncodeToString(sum[:])[:maxSlugLen-2]
			}
		}
		slug = next
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
	// 截断到 config 侧 bootstrap 正则 ^[a-z0-9]{2,32}$ 的上界。
	// 不截的话，一个长显示名会产出任意长度的 slug，与 bootstrap 路径
	// 的校验口径不一致（那条会直接被 config.validate 拒掉）。
	if len(slug) > maxSlugLen {
		slug = strings.Trim(slug[:maxSlugLen], "-")
		if slug == "" {
			slug = "provider"
		}
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
