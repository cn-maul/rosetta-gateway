package admin

import (
	"errors"
	"net/http"
	"strings"

	"github.com/cn-maul/rosetta-gateway/internal/crypto"
	"github.com/cn-maul/rosetta-gateway/internal/store"
)

type CredentialHandler struct {
	store     *store.Store
	masterKey []byte
}

func NewCredentialHandler(st *store.Store, masterKey []byte) *CredentialHandler {
	return &CredentialHandler{store: st, masterKey: masterKey}
}

// credentialRequest 是上游凭据的创建 / PATCH 输入。
//
// PATCH 语义（2026-09-21 重构）：字段为指针，nil = 未提供（保持原值）。
// APIKey 是「不提供即不更换」；显式传空串会被拒绝 —— 空密钥的凭据没有意义，
// 要弃用请直接删除凭据或置 enabled=false，不该静默变成一条永远 401 的记录。
//
// Enabled 额外带一条**跨行**约束：同一个上游同时只允许一把凭据启用。
// 把它设为 true 是「切到这一把」（同 provider 的其余凭据在同一事务里
// 一并停用，见 store.EnableExclusiveCredential），设为 false 只是停用它自己。
// 新建时该字段缺席则按「当前一把都没有 → 启用；已有 → 停用」处理，
// 于是「加一把备用 key」不会顺手把正在跑的流量切走。
type credentialRequest struct {
	Label   *string `json:"label"`
	APIKey  *string `json:"api_key"`
	Weight  *int    `json:"weight"`
	Enabled *bool   `json:"enabled"`
}

type credentialResponse struct {
	ID         string `json:"id"`
	ProviderID string `json:"provider_id"`
	Label      string `json:"label"`
	APIKeyMask string `json:"api_key_mask"`
	Enabled    bool   `json:"enabled"`
	Weight     int    `json:"weight"`
	Status     string `json:"status"`
	CreatedAt  int64  `json:"created_at"`
}

func (h *CredentialHandler) List(w http.ResponseWriter, r *http.Request, providerID string) {
	if !requireProvider(w, r, h.store, providerID) {
		return
	}

	creds, err := h.store.ListCredentials(r.Context(), providerID)
	if err != nil {
		writeServerError(w, "list credentials", err)
		return
	}
	result := make([]credentialResponse, 0, len(creds))
	for _, c := range creds {
		result = append(result, h.toCredentialResponse(c))
	}
	writeJSON(w, http.StatusOK, result)
}

func (h *CredentialHandler) Create(w http.ResponseWriter, r *http.Request, providerID string) {
	if !requireProvider(w, r, h.store, providerID) {
		return
	}

	var req credentialRequest
	if err := decodeJSON(w, r, &req); err != nil {
		if errors.Is(err, errUnsupportedMediaType) {
			return
		}
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}

	// 密钥里的首尾空白在 HTTP 头里非法，粘贴时极易带上换行，统一裁掉
	apiKey := strings.TrimSpace(derefStr(req.APIKey))
	if apiKey == "" {
		writeError(w, http.StatusBadRequest, "api_key is required")
		return
	}

	enc, err := encryptSecret(apiKey, h.masterKey)
	if err != nil {
		writeServerError(w, "encrypt api key", err)
		return
	}

	// 同一上游同时只允许一把启用（见 store.EnableExclusiveCredential），
	// 所以新建时的默认值不是恒定的 true，而是「当前一把都没有才默认启用」：
	// 「加一把备用 key」不该顺手把正在跑的流量切走，而「这个上游刚建好、
	// 这是它的第一把 key」仍然要自动可用。
	wantEnabled := false
	if req.Enabled != nil {
		wantEnabled = *req.Enabled
	} else {
		hasEnabled, err := h.store.HasEnabledCredential(r.Context(), providerID)
		if err != nil {
			writeServerError(w, "check enabled credentials", err)
			return
		}
		wantEnabled = !hasEnabled
	}

	weight := derefInt(req.Weight)
	if weight <= 0 {
		weight = 1
	}

	c := &store.Credential{
		ID:         generateID(),
		ProviderID: providerID,
		Label:      strings.TrimSpace(derefStr(req.Label)),
		APIKeyEnc:  enc,
		// 一律先以**停用态**落库，要启用再走 EnableExclusiveCredential：
		// 那样「关掉同上游其它凭据」与「打开这一把」收在同一个事务里。
		// 否则这里是 INSERT enabled=1、下一步才去关旧的，两步之间崩溃
		// 就留下一把「两把都启用」的记录，而界面上完全看不出来。
		Enabled: false,
		Weight:  weight,
		Status:  "healthy",
	}

	if err := h.store.CreateCredential(r.Context(), c); err != nil {
		writeServerError(w, "create credential", err)
		return
	}

	if wantEnabled {
		if err := h.store.EnableExclusiveCredential(r.Context(), providerID, c.ID); err != nil {
			writeNotFoundOrError(w, "enable credential", "凭据不存在（可能已被并发删除）", err)
			return
		}
		c.Enabled = true
	}

	writeJSON(w, http.StatusCreated, h.toCredentialResponse(*c))
}

func (h *CredentialHandler) Update(w http.ResponseWriter, r *http.Request, id string) {
	existing, err := h.store.GetCredential(r.Context(), id)
	if err != nil {
		// DB 故障不是「资源不存在」。混在一起报 404 会让运维去删库重建配置。
		writeServerError(w, "get credential", err)
		return
	}
	if existing == nil {
		writeError(w, http.StatusNotFound, "credential not found")
		return
	}

	var req credentialRequest
	if err := decodeJSON(w, r, &req); err != nil {
		if errors.Is(err, errUnsupportedMediaType) {
			return
		}
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}

	// label 可清空
	if req.Label != nil {
		existing.Label = strings.TrimSpace(*req.Label)
	}
	if req.APIKey != nil {
		apiKey := strings.TrimSpace(*req.APIKey)
		if apiKey == "" {
			writeError(w, http.StatusBadRequest, "api_key cannot be empty; delete the credential instead")
			return
		}
		enc, err := encryptSecret(apiKey, h.masterKey)
		if err != nil {
			writeServerError(w, "encrypt api key", err)
			return
		}
		existing.APIKeyEnc = enc
	}
	if req.Enabled != nil {
		// 启用的收尾交给 EnableExclusiveCredential（它会把同 provider 的
		// 其余凭据一并停用）。所以在 UpdateCredential 之前一律把 enabled
		// 压成 false，让这一步不去动启用态 —— 否则这里是「开新的」、
		// 下一步是「关旧的」，两条独立写之间崩溃就留下两把都启用，
		// 而请求池会照权重在两把 key 之间轮询，没人知道该只有一把在用。
		existing.Enabled = false
	}
	if req.Weight != nil {
		if *req.Weight < 1 {
			writeError(w, http.StatusBadRequest, "weight must be >= 1")
			return
		}
		existing.Weight = *req.Weight
	}

	if err := h.store.UpdateCredential(r.Context(), id, existing); err != nil {
		writeNotFoundOrError(w, "update credential", "凭据不存在（可能已被并发删除）", err)
		return
	}

	if req.Enabled != nil && *req.Enabled {
		if err := h.store.EnableExclusiveCredential(r.Context(), existing.ProviderID, id); err != nil {
			writeNotFoundOrError(w, "enable credential", "凭据不存在（可能已被并发删除）", err)
			return
		}
		existing.Enabled = true
	}

	writeJSON(w, http.StatusOK, h.toCredentialResponse(*existing))
}

func (h *CredentialHandler) Delete(w http.ResponseWriter, r *http.Request, id string) {
	if err := h.store.DeleteCredential(r.Context(), id); err != nil {
		writeDeleteError(w, "delete credential", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (h *CredentialHandler) toCredentialResponse(c store.Credential) credentialResponse {
	mask := ""
	if len(c.APIKeyEnc) > 0 {
		if plain, err := decryptSecret(c.APIKeyEnc, h.masterKey); err == nil {
			mask = crypto.MaskKey(plain)
		} else {
			mask = "****"
		}
	}
	return credentialResponse{
		ID:         c.ID,
		ProviderID: c.ProviderID,
		Label:      c.Label,
		APIKeyMask: mask,
		Enabled:    c.Enabled,
		Weight:     c.Weight,
		Status:     c.Status,
		CreatedAt:  c.CreatedAt,
	}
}
