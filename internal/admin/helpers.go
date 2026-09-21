package admin

import (
	"encoding/json"
	"net/http"

	"github.com/cn-maul/rosetta-gateway/internal/crypto"
)

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]string{
			"message": message,
			"type":    "invalid_request_error",
		},
	})
}

func decodeJSON(r *http.Request, v any) error {
	defer r.Body.Close()
	return json.NewDecoder(r.Body).Decode(v)
}

// ---------- PATCH 部分更新语义（2026-09-21 重构）----------
//
// PATCH 请求体里的标量字段一律用指针表示「是否提供」：
//   - nil  → 客户端未提供该字段，保持数据库原值
//   - 非nil → 显式赋新值，此时空串 / 0 都是**合法值**（会真正落库 / 落 NULL）
//
// 旧实现用 `if req.X != ""` / `if req.X != 0` 判断，导致
// 「priority 改回 0」「清空兜底路由」「超时重置为全局默认」等操作在界面上无效。
//
// derefStr / derefInt 只用于 Create 这类「缺省即零值」的场合；
// Update 必须写 `if req.X != nil`，否则显式置 0 会被误判为未提供。

func derefStr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func derefInt(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

// encryptSecret 在有主密钥时加密，否则退化为明文存储，
// 保证未配置主密钥时凭据仍可用。
func encryptSecret(plain string, masterKey []byte) ([]byte, error) {
	if masterKey == nil {
		return []byte(plain), nil
	}
	return crypto.Encrypt([]byte(plain), masterKey)
}

// decryptSecret 与 upstream.decryptCredentialKey 共用同一套兜底：
// 解不开时按明文处理，兼容早期无主密钥时代的落库数据。
func decryptSecret(enc []byte, masterKey []byte) (string, error) {
	return crypto.DecryptWithFallback(enc, masterKey)
}
