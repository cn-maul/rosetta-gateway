package protocol

import "strings"

const (
	Auto      = "auto"
	Chat      = "chat"
	Response  = "response"
	Anthropic = "anthropic"
)

const (
	LegacyChat     = "openai-chat"
	LegacyResponse = "openai-responses"
)

const AllowedValues = "auto / chat / response / anthropic"

func Normalize(value string) string {
	switch strings.TrimSpace(value) {
	case LegacyChat:
		return Chat
	case LegacyResponse:
		return Response
	default:
		return strings.TrimSpace(value)
	}
}

func Valid(value string) bool {
	switch value {
	case Auto, Chat, Response, Anthropic:
		return true
	default:
		return false
	}
}
