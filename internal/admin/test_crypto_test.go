package admin

import (
	"testing"

	"github.com/cn-maul/rosetta-gateway/internal/crypto"
)

// testCredentialKey 是 admin 包测试统一使用的当前格式 AES-256 主密钥。
var testCredentialKey = []byte("0123456789abcdef0123456789abcdef")

func encryptTestCredential(t *testing.T, plain string) []byte {
	t.Helper()
	enc, err := crypto.Encrypt([]byte(plain), testCredentialKey)
	if err != nil {
		t.Fatalf("encrypt test credential: %v", err)
	}
	return enc
}
