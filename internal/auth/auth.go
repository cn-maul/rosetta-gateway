package auth

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"

	"github.com/cn-maul/rosetta-gateway/internal/snapshot"
)

type Context struct {
	KeyID string
	Name  string
}

func Authenticate(r *http.Request) (*Context, error) {
	key := extractKey(r)
	if key == "" {
		return nil, ErrNoKey
	}

	hash := sha256.Sum256([]byte(key))
	hashHex := hex.EncodeToString(hash[:])

	snap := snapshot.Get()
	for _, ks := range snap.Keys {
		if ks.KeyHash == hashHex {
			if !ks.Enabled {
				return nil, ErrKeyDisabled
			}
			return &Context{KeyID: ks.ID, Name: ks.Name}, nil
		}
	}

	return nil, ErrInvalidKey
}

func extractKey(r *http.Request) string {
	auth := r.Header.Get("Authorization")
	if strings.HasPrefix(auth, "Bearer ") {
		return strings.TrimPrefix(auth, "Bearer ")
	}

	if key := r.Header.Get("X-Api-Key"); key != "" {
		return key
	}

	if key := r.URL.Query().Get("key"); key != "" {
		return key
	}

	return ""
}
