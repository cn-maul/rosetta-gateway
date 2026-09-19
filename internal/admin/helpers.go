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

// encryptSecret 在有主密钥时加密，否则退化为明文存储，
// 保证未配置 ROSETTA_GW_MASTER_KEY 时凭据仍可用。
func encryptSecret(plain string, masterKey []byte) ([]byte, error) {
	if masterKey == nil {
		return []byte(plain), nil
	}
	return crypto.Encrypt([]byte(plain), masterKey)
}

func decryptSecret(enc []byte, masterKey []byte) (string, error) {
	if masterKey == nil {
		return string(enc), nil
	}
	plain, err := crypto.Decrypt(enc, masterKey)
	if err != nil {
		return "", err
	}
	return string(plain), nil
}
