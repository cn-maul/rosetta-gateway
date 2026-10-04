package admin

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
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
	// RPMLimit / TPMLimit：Key 维度每分钟限速（DESIGN §11.4），0 = 不限。
	RPMLimit *int `json:"rpm_limit"`
	TPMLimit *int `json:"tpm_limit"`
}

type keyResponse struct {
	ID          string `json:"id"`
	KeyPrefix   string `json:"key_prefix"`
	Name        string `json:"name"`
	Enabled     bool   `json:"enabled"`
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
	if err := decodeJSON(w, r, &req); err != nil {
		if errors.Is(err, errUnsupportedMediaType) {
			return
		}
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

	rpm, tpm, bad, err := limitPair(req)
	if bad != "" {
		writeError(w, http.StatusBadRequest, bad)
		return
	}
	if err != nil {
		writeServerError(w, "parse limits", err)
		return
	}

	k := &store.AccessKey{
		ID:          generateID(),
		KeyHash:     keyHash,
		KeyPrefix:   keyPrefix,
		Name:        name,
		Enabled:     enabled,
		QuotaTokens: quota,
		RPMLimit:    rpm,
		TPMLimit:    tpm,
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
	if err := decodeJSON(w, r, &req); err != nil {
		if errors.Is(err, errUnsupportedMediaType) {
			return
		}
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
	// 限速字段沿用 PATCH 语义：nil = 保持原值，显式数字（含 0 = 取消限制）才覆盖。
	if req.RPMLimit != nil {
		if *req.RPMLimit < 0 {
			writeError(w, http.StatusBadRequest, "rpm_limit cannot be negative")
			return
		}
		existing.RPMLimit = *req.RPMLimit
	}
	if req.TPMLimit != nil {
		if *req.TPMLimit < 0 {
			writeError(w, http.StatusBadRequest, "tpm_limit cannot be negative")
			return
		}
		existing.TPMLimit = *req.TPMLimit
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

// RecomputeUsage 从 usage_records 重算该密钥的已用量并覆盖 used_tokens。
//
// 存在的理由：used_tokens 由数据库触发器单调累加，没有任何回退路径。
// 一旦因误写或 bug 偏高，配额预检会从此恒返回 429，而 Update 刻意不写
// used_tokens（防止请求体随意改配额计数）—— 于是这把 key 在管理界面上
// 变成「怎么改配置都救不回来」的死 key，只能直接改库。这个端点把
// 恢复能力还给运维。
func (h *KeyHandler) RecomputeUsage(w http.ResponseWriter, r *http.Request, id string) {
	used, err := h.store.RecomputeUsedTokens(r.Context(), id)
	if err != nil {
		// key 不存在时 DAO 返回 ErrNotFound → 404，而不是把「重算了一把
		// 不存在的 key」报成 200 + used_tokens=0（前端会以为重算已完成）。
		writeNotFoundOrError(w, "recompute key usage", "密钥不存在", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":      "ok",
		"used_tokens": used,
	})
}

func toKeyResponse(k store.AccessKey) keyResponse {
	return keyResponse{
		ID:          k.ID,
		KeyPrefix:   k.KeyPrefix,
		Name:        k.Name,
		Enabled:     k.Enabled,
		QuotaTokens: k.QuotaTokens,
		UsedTokens:  k.UsedTokens,
		RPMLimit:    k.RPMLimit,
		TPMLimit:    k.TPMLimit,
		CreatedAt:   k.CreatedAt,
	}
}

// limitPair 解析 rpm_limit / tpm_limit（PATCH 语义 + 非负校验）。
// 返回值约定：bad 非空 = 校验失败（直接 400）；否则 err 上抛为 500。
func limitPair(req keyRequest) (rpm, tpm int, bad string, err error) {
	if req.RPMLimit != nil {
		if *req.RPMLimit < 0 {
			return 0, 0, "rpm_limit cannot be negative", nil
		}
		rpm = *req.RPMLimit
	}
	if req.TPMLimit != nil {
		if *req.TPMLimit < 0 {
			return 0, 0, "tpm_limit cannot be negative", nil
		}
		tpm = *req.TPMLimit
	}
	return rpm, tpm, "", nil
}
