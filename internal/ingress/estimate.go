package ingress

import (
	"strings"

	"github.com/cn-maul/rosetta"
)

// EstimateRequestTokens roughly estimates a request's input tokens plus the
// output upper bound for TPM reservation.
//
// Precision is not required here: the reservation is corrected with actual
// usage after the request finishes. The estimate only needs the right order of
// magnitude so TPM is neither bypassed nor needlessly rejected.
func EstimateRequestTokens(req *rosetta.ChatRequest) int64 {
	return EstimateInputTokensOnly(req) + int64(req.MaxOutputTokens)
}

// EstimateInputTokensOnly estimates only the input side, excluding
// max_tokens.
//
// The client-interrupt fallback needs an input estimate without charging the
// full output ceiling. Keeping the character-based formula in one place also
// prevents TPM reservation and usage fallback from drifting apart.
func EstimateInputTokensOnly(req *rosetta.ChatRequest) int64 {
	var b strings.Builder
	b.WriteString(req.System)
	for _, m := range req.Messages {
		for _, blk := range m.Blocks {
			b.WriteString(blk.Text)
			b.WriteString(blk.Thinking)
			b.WriteString(blk.Content)
			b.WriteString(blk.Arguments)
		}
	}
	for _, t := range req.Tools {
		b.WriteString(t.Name)
		b.WriteString(t.Description)
		b.WriteString(string(t.Parameters))
	}
	return int64(rosetta.EstimateTokens(b.String()))
}

// PartialOutTokens converts already-received output characters into a
// conservative token lower bound for client-interrupt accounting.
//
// The actual generated token count is unknowable when the upstream stream
// aborts before a terminal usage event. Every received character was billed,
// so using one token per character is a safe lower bound for both ASCII and
// non-ASCII content.
func PartialOutTokens(runes int) int64 {
	if runes <= 0 {
		return 0
	}
	return int64(runes)
}
