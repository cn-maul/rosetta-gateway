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
// quota_tokens 是终身 token 配额（input+output 累计）：0 = 不限，>0 时请求前预检，
// 达到即 429 拒绝（used_tokens 由 usage 触发器实时累加，见 DESIGN §11）。
type keyRequest struct {
	Name        *string `json:"name"`
	Enabled     *bool   `json:"enabled"`
	QuotaTokens *int64  `json:"quota_tokens"`
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
		writeServerError(w, "list keys", err)
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

	var quota int64
	if req.QuotaTokens != nil {
		if *req.QuotaTokens < 0 {
			writeError(w, http.StatusBadRequest, "quota_tokens cannot be negative")
			return
		}
		quota = *req.QuotaTokens
	}

	k := &store.AccessKey{
		ID:          generateID(),
		KeyHash:     keyHash,
		KeyPrefix:   keyPrefix,
		Name:        name,
		Enabled:     enabled,
		QuotaTokens: quota,
	}

	if err := h.store.CreateAccessKey(r.Context(), k); err != nil {
		writeServerError(w, "create key", err)
		return
	}

	writeJSON(w, http.StatusCreated, keyCreateResponse{
		keyResponse:  toKeyResponse(*k),
		PlaintextKey: plainKey,
	})
}

func (h *KeyHandler) Update(w http.ResponseWriter, r *http.Request, id string) {
	existing, err := h.store.GetAccessKey(r.Context(), id)
	if err != nil {
		writeServerError(w, "get key", err)
		return
	}
	if existing == nil {
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
	if req.QuotaTokens != nil {
		if *req.QuotaTokens < 0 {
			writeError(w, http.StatusBadRequest, "quota_tokens cannot be negative")
			return
		}
		existing.QuotaTokens = *req.QuotaTokens
	}

	if err := h.store.UpdateAccessKey(r.Context(), id, existing); err != nil {
		writeNotFoundOrError(w, "update key", "密钥不存在（可能已被并发删除）", err)
		return
	}

	writeJSON(w, http.StatusOK, toKeyResponse(*existing))
}

func (h *KeyHandler) Delete(w http.ResponseWriter, r *http.Request, id string) {
	if err := h.store.DeleteAccessKey(r.Context(), id); err != nil {
		writeDeleteError(w, "delete key", err)
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
