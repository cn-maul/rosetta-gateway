package ingress

import (
	"strings"
	"unicode/utf8"

	"github.com/cn-maul/rosetta"
)

// EstimateRequestTokens roughly estimates a request's input tokens plus the
// output upper bound for TPM reservation.
//
// Precision is not required here: the reservation is corrected with actual
// usage after the request finishes. The estimate only needs the right order of
// magnitude so TPM is neither bypassed nor needlessly rejected.
func EstimateRequestTokens(req *rosetta.ChatRequest, outputCeiling int64) int64 {
	return EstimateInputTokensOnly(req) + outputCeiling
}

// RequestOutputCeiling returns the client's cap when present, otherwise the
// largest configured output ceiling in the resolved candidate chain.
func RequestOutputCeiling(req *rosetta.ChatRequest, maxByCandidate []int) int64 {
	if req != nil && req.MaxOutputTokens > 0 {
		return int64(req.MaxOutputTokens)
	}
	var ceiling int
	for _, n := range maxByCandidate {
		if n > ceiling {
			ceiling = n
		}
	}
	return int64(ceiling)
}

// EstimateInputTokensOnly estimates only the input side, excluding
// max_tokens.
//
// The client-interrupt fallback needs an input estimate without charging the
// full output ceiling. Keeping the character-based formula in one place also
// prevents TPM reservation and usage fallback from drifting apart.
func EstimateInputTokensOnly(req *rosetta.ChatRequest) int64 {
	ascii, other := 0, 0
	add := func(s string) {
		for _, r := range s {
			if r < utf8.RuneSelf {
				ascii++
			} else {
				other++
			}
		}
	}
	add(req.System)
	for _, m := range req.Messages {
		for _, blk := range m.Blocks {
			switch blk.Type {
			case rosetta.BlockImage:
				ascii += 1500 * 4
			case rosetta.BlockAudio:
				// The normalized model carries the bytes, not duration.
				// Charge both the flat clip estimate and the inline payload.
				ascii += 500 * 4
				add(blk.AudioData)
			case rosetta.BlockFile:
				// Remote file references keep the policy floor. Inline
				// payloads additionally scale with their serialized bytes.
				ascii += 3000 * 4
				if !isRemoteReference(blk.FileData) {
					add(blk.FileData)
				}
			default:
				add(blk.Text)
				add(blk.Thinking)
				add(blk.Signature)
				add(blk.Sealed)
				add(blk.Content)
				add(blk.Arguments)
			}
			add(blk.FileName)
			add(blk.MimeType)
		}
	}
	for _, t := range req.Tools {
		add(t.Name)
		add(t.Description)
		add(string(t.Parameters))
	}
	return int64((ascii+3)/4 + other)
}

func isRemoteReference(value string) bool {
	return strings.HasPrefix(value, "http://") || strings.HasPrefix(value, "https://")
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
