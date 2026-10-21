package dataplane

import (
	"github.com/cn-maul/rosetta"
)

// applyReplayGate strips thinking blocks the selected upstream protocol cannot
// replay, and drops assistant turns that become empty. It is idempotent and
// must run per attempt because failover can change the upstream protocol.
//
// OpenAI chat discards every thinking / redacted-thinking block. Anthropic
// keeps only signed thinking blocks; an assistant turn left with no blocks is
// removed because the SDK rejects such a turn with a 400. OpenAI Responses
// handles foreign seals through the SDK's SealedBy gate and is left untouched.
func (h *Handler) applyReplayGate(req *rosetta.ChatRequest, protocol string) {
	keepThinking, keepRedacted := false, false
	switch protocol {
	case "", "openai-chat", "auto":
		// OpenAI chat has no replayable thinking block.
	case "anthropic":
		keepThinking, keepRedacted = true, true
	default:
		// openai-responses handles foreign seals inside the SDK.
		return
	}

	kept := make([]rosetta.Message, 0, len(req.Messages))
	droppedBlocks, droppedTurns := 0, 0
	for _, m := range req.Messages {
		if m.Role != rosetta.RoleAssistant {
			kept = append(kept, m)
			continue
		}
		blocks := make([]rosetta.Block, 0, len(m.Blocks))
		removed := 0
		for _, b := range m.Blocks {
			switch b.Type {
			case rosetta.BlockThinking:
				if !keepThinking || b.Signature == "" {
					removed++
					continue
				}
			case rosetta.BlockRedactedThinking:
				if !keepRedacted {
					removed++
					continue
				}
			}
			blocks = append(blocks, b)
		}
		if removed == 0 {
			kept = append(kept, m)
			continue
		}
		droppedBlocks += removed
		if len(blocks) == 0 {
			droppedTurns++
			continue
		}
		m.Blocks = blocks
		kept = append(kept, m)
	}
	if droppedBlocks == 0 {
		return
	}
	req.Messages = kept
	h.logger.Debug("stripped unplayable thinking blocks for upstream protocol",
		"protocol", protocol, "blocks", droppedBlocks, "turns", droppedTurns)
}
