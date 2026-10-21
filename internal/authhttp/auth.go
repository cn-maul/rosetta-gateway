// Package authhttp maps API-key authentication failures to protocol-neutral
// HTTP status/code/message triples.
package authhttp

import (
	"errors"
	"net/http"

	"github.com/cn-maul/rosetta-gateway/internal/auth"
)

// ErrorWriter renders one protocol-specific error response.
type ErrorWriter func(w http.ResponseWriter, status int, code, message string)

// WriteAuthError keeps every data-plane endpoint on the same auth error policy.
func WriteAuthError(w http.ResponseWriter, err error, writeErr ErrorWriter) {
	switch {
	case errors.Is(err, auth.ErrNoKey):
		writeErr(w, http.StatusUnauthorized, "invalid_api_key", "missing API key")
	case errors.Is(err, auth.ErrKeyDisabled):
		writeErr(w, http.StatusForbidden, "invalid_api_key", "API key disabled")
	case errors.Is(err, auth.ErrInvalidKey):
		writeErr(w, http.StatusUnauthorized, "invalid_api_key", "invalid API key")
	case errors.Is(err, auth.ErrKeyExpired):
		writeErr(w, http.StatusForbidden, "key_expired", "this API key has expired")
	case errors.Is(err, auth.ErrIPNotAllowed):
		writeErr(w, http.StatusForbidden, "ip_not_allowed",
			"source IP is not allowed for this API key")
	case errors.Is(err, auth.ErrAdminCannotCallModel):
		writeErr(w, http.StatusForbidden, "admin_cannot_call_model",
			"administrator accounts cannot call models: create a regular user account "+
				"and use the API key issued to that user")
	default:
		writeErr(w, http.StatusUnauthorized, "invalid_api_key", "authentication failed")
	}
}
