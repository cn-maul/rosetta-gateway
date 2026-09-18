package outwire

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/cn-maul/rosetta"
)

type ErrorBody struct {
	Error *ErrorDetail `json:"error,omitempty"`
}

type ErrorDetail struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code,omitempty"`
	Param   string `json:"param,omitempty"`
}

func MapUpstreamError(err error) (int, string, string) {
	if err == nil {
		return http.StatusOK, "", ""
	}

	if errors.Is(err, rosetta.ErrContextTooLong) {
		return http.StatusBadRequest, "context_length_exceeded", "request exceeds model context window"
	}
	if errors.Is(err, rosetta.ErrInvalidRequest) {
		return http.StatusBadRequest, "invalid_request_error", err.Error()
	}
	if errors.Is(err, rosetta.ErrStreamTruncated) {
		return http.StatusOK, "", ""
	}

	var apiErr *rosetta.APIError
	if errors.As(err, &apiErr) {
		switch {
		case apiErr.StatusCode == 401 || apiErr.StatusCode == 403:
			return http.StatusBadGateway, "upstream_auth_error", "upstream provider authentication failed"
		case apiErr.StatusCode == 402:
			return http.StatusBadGateway, "upstream_quota_exhausted", "upstream provider quota exhausted"
		case apiErr.StatusCode == 429:
			return http.StatusTooManyRequests, "rate_limit_exceeded", "rate limit exceeded"
		case apiErr.StatusCode >= 500:
			return http.StatusBadGateway, "upstream_error", "upstream provider error"
		case apiErr.StatusCode == 400:
			return http.StatusBadRequest, "invalid_request_error", apiErr.Message
		default:
			return http.StatusBadGateway, "upstream_error", apiErr.Message
		}
	}

	var transportErr *rosetta.TransportError
	if errors.As(err, &transportErr) {
		return http.StatusGatewayTimeout, "upstream_timeout", "upstream connection failed"
	}

	return http.StatusInternalServerError, "internal_error", "internal gateway error"
}

func WriteOpenAIError(w http.ResponseWriter, statusCode int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	json.NewEncoder(w).Encode(ErrorBody{
		Error: &ErrorDetail{
			Message: message,
			Type:    errorTypeFromCode(code),
			Code:    code,
		},
	})
}

func WriteAnthropicError(w http.ResponseWriter, statusCode int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	json.NewEncoder(w).Encode(AnthropicErrorBody{
		Type: "error",
		Error: &ErrorDetail{
			Type:    errorTypeFromCode(code),
			Message: message,
		},
	})
}

func errorTypeFromCode(code string) string {
	switch code {
	case "model_not_found":
		return "invalid_request_error"
	case "invalid_api_key":
		return "authentication_error"
	case "rate_limit_exceeded":
		return "rate_limit_error"
	case "context_length_exceeded":
		return "invalid_request_error"
	case "invalid_request_error":
		return "invalid_request_error"
	default:
		return "api_error"
	}
}
