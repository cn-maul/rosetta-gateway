package admin

import (
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
	creds, err := h.store.ListCredentials(r.Context(), providerID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	result := make([]credentialResponse, 0, len(creds))
	for _, c := range creds {
		result = append(result, h.toCredentialResponse(c))
	}
	writeJSON(w, http.StatusOK, result)
}

func (h *CredentialHandler) Create(w http.ResponseWriter, r *http.Request, providerID string) {
	var req credentialRequest
	if err := decodeJSON(r, &req); err != nil {
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
		writeError(w, http.StatusInternalServerError, "failed to encrypt key: "+err.Error())
		return
	}

	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
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
		Enabled:    enabled,
		Weight:     weight,
		Status:     "healthy",
	}

	if err := h.store.CreateCredential(r.Context(), c); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusCreated, h.toCredentialResponse(*c))
}

func (h *CredentialHandler) Update(w http.ResponseWriter, r *http.Request, id string) {
	existing, err := h.store.GetCredential(r.Context(), id)
	if err != nil || existing == nil {
		writeError(w, http.StatusNotFound, "credential not found")
		return
	}

	var req credentialRequest
	if err := decodeJSON(r, &req); err != nil {
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
			writeError(w, http.StatusInternalServerError, "failed to encrypt key")
			return
		}
		existing.APIKeyEnc = enc
	}
	if req.Enabled != nil {
		existing.Enabled = *req.Enabled
	}
	if req.Weight != nil {
		if *req.Weight < 1 {
			writeError(w, http.StatusBadRequest, "weight must be >= 1")
			return
		}
		existing.Weight = *req.Weight
	}

	if err := h.store.UpdateCredential(r.Context(), id, existing); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, h.toCredentialResponse(*existing))
}

func (h *CredentialHandler) Delete(w http.ResponseWriter, r *http.Request, id string) {
	if err := h.store.DeleteCredential(r.Context(), id); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
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
