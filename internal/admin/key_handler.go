package admin

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"

	"github.com/cn-maul/rosetta-gateway/internal/crypto"
	"github.com/cn-maul/rosetta-gateway/internal/store"
)

type KeyHandler struct {
	store *store.Store
}

func NewKeyHandler(st *store.Store) *KeyHandler {
	return &KeyHandler{store: st}
}

// keyRequest 是访问密钥的创建 / PATCH 输入。
// PATCH 语义：字段为指针，nil = 未提供（保持原值），非 nil = 显式赋新值。
// 注：quota_tokens 目前只有读路径（列表/详情下发），没有任何写路径与配额校验，
// 属于未接线的存量字段，此处不提供写入入口，避免造成「配额可用」的错觉。
type keyRequest struct {
	Name    *string `json:"name"`
	Enabled *bool   `json:"enabled"`
}

type keyResponse struct {
	ID         string `json:"id"`
	KeyPrefix  string `json:"key_prefix"`
	Name       string `json:"name"`
	Enabled    bool   `json:"enabled"`
	QuotaTokens int64 `json:"quota_tokens"`
	UsedTokens  int64 `json:"used_tokens"`
	CreatedAt  int64  `json:"created_at"`
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

	name := strings.TrimSpace(derefStr(req.Name))
	if name == "" {
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
		ID:        generateID(),
		KeyHash:   keyHash,
		KeyPrefix: keyPrefix,
		Name:      name,
		Enabled:   enabled,
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

	if req.Name != nil {
		name := strings.TrimSpace(*req.Name)
		if name == "" {
			writeError(w, http.StatusBadRequest, "name cannot be empty")
			return
		}
		existing.Name = name
	}
	if req.Enabled != nil {
		existing.Enabled = *req.Enabled
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
		QuotaTokens: k.QuotaTokens,
		UsedTokens:  k.UsedTokens,
		CreatedAt:   k.CreatedAt,
	}
}
