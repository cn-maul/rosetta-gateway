package admin

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"

	"github.com/cn-maul/rosetta-gateway/internal/crypto"
	"github.com/cn-maul/rosetta-gateway/internal/store"
)

type KeyHandler struct {
	store *store.Store
}

func NewKeyHandler(st *store.Store) *KeyHandler {
	return &KeyHandler{store: st}
}

type keyRequest struct {
	Name         string `json:"name"`
	Enabled      *bool  `json:"enabled"`
	ExpiresAt    int64  `json:"expires_at"`
	QuotaTokens  int64  `json:"quota_tokens"`
	RPMLimit     int    `json:"rpm_limit"`
	TPMLimit     int    `json:"tpm_limit"`
}

type keyResponse struct {
	ID          string `json:"id"`
	KeyPrefix   string `json:"key_prefix"`
	Name        string `json:"name"`
	Enabled     bool   `json:"enabled"`
	ExpiresAt   int64  `json:"expires_at"`
	QuotaTokens int64  `json:"quota_tokens"`
	UsedTokens  int64  `json:"used_tokens"`
	RPMLimit    int    `json:"rpm_limit"`
	TPMLimit    int    `json:"tpm_limit"`
	CreatedAt   int64  `json:"created_at"`
}

type keyCreateResponse struct {
	keyResponse
	PlaintextKey string `json:"plaintext_key"`
}

func (h *KeyHandler) List(w http.ResponseWriter, r *http.Request) {
	keys, err := h.store.ListAccessKeys(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	result := make([]keyResponse, 0, len(keys))
	for _, k := range keys {
		result = append(result, toKeyResponse(k))
	}
	writeJSON(w, http.StatusOK, result)
}

func (h *KeyHandler) Create(w http.ResponseWriter, r *http.Request) {
	var req keyRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}

	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}

	plainKey := "sk-gw-" + crypto.GenerateKey()
	hash := sha256.Sum256([]byte(plainKey))
	keyHash := hex.EncodeToString(hash[:])
	keyPrefix := plainKey[:11] + "..."

	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}

	k := &store.AccessKey{
		ID:          generateID(),
		KeyHash:     keyHash,
		KeyPrefix:   keyPrefix,
		Name:        req.Name,
		Enabled:     enabled,
		ExpiresAt:   req.ExpiresAt,
		QuotaTokens: req.QuotaTokens,
		RPMLimit:    req.RPMLimit,
		TPMLimit:    req.TPMLimit,
	}

	if err := h.store.CreateAccessKey(r.Context(), k); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusCreated, keyCreateResponse{
		keyResponse:  toKeyResponse(*k),
		PlaintextKey: plainKey,
	})
}

func (h *KeyHandler) Update(w http.ResponseWriter, r *http.Request, id string) {
	existing, err := h.store.GetAccessKey(r.Context(), id)
	if err != nil || existing == nil {
		writeError(w, http.StatusNotFound, "key not found")
		return
	}

	var req keyRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}

	if req.Name != "" {
		existing.Name = req.Name
	}
	if req.Enabled != nil {
		existing.Enabled = *req.Enabled
	}
	if req.ExpiresAt != 0 {
		existing.ExpiresAt = req.ExpiresAt
	}
	if req.QuotaTokens != 0 {
		existing.QuotaTokens = req.QuotaTokens
	}
	if req.RPMLimit != 0 {
		existing.RPMLimit = req.RPMLimit
	}
	if req.TPMLimit != 0 {
		existing.TPMLimit = req.TPMLimit
	}

	if err := h.store.UpdateAccessKey(r.Context(), id, existing); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, toKeyResponse(*existing))
}

func (h *KeyHandler) Delete(w http.ResponseWriter, r *http.Request, id string) {
	if err := h.store.DeleteAccessKey(r.Context(), id); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func toKeyResponse(k store.AccessKey) keyResponse {
	return keyResponse{
		ID:          k.ID,
		KeyPrefix:   k.KeyPrefix,
		Name:        k.Name,
		Enabled:     k.Enabled,
		ExpiresAt:   k.ExpiresAt,
		QuotaTokens: k.QuotaTokens,
		UsedTokens:  k.UsedTokens,
		RPMLimit:    k.RPMLimit,
		TPMLimit:    k.TPMLimit,
		CreatedAt:   k.CreatedAt,
	}
}
