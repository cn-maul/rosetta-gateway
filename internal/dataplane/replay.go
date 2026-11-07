package dataplane

import (
	"strings"

	"github.com/cn-maul/rosetta"
)

// applyReplayGate strips thinking blocks the selected upstream protocol cannot
// replay, and drops assistant turns that become empty. It is idempotent and
// must run per attempt because failover can change the upstream protocol.
//
// 各协议的可回放范围（rosetta v1.3.0 起）：
//
//   - OpenAI chat：`BlockThinking` 会被编码成同一条 assistant 消息的
//     `reasoning_content`（DeepSeek / Qwen / vLLM 系要求原样回传）。所以
//     **带可读文本、没有 Anthropic 签名**的 thinking 现在要保留，不能再一刀剥掉。
//     剥的只剩两种：签过名的（别的协议族签发，chat 写不出对应语义）和无文本的
//     （没有任何可回放内容）。
//   - Anthropic：只保留带签名的 thinking —— 签名是上游校验的唯一凭据。
//   - OpenAI Responses：外来封由 SDK 的 SealedBy 门闩自理，整层不动。
//
// `BlockRedactedThinking` 在任何协议下都没有可读文本：chat 没有字段能承载它，
// Anthropic 只有带签名的才留。它被剥掉后留下的空轮必须整条删除，否则 SDK 会以
// 「assistant turn has only thinking/redacted-thinking blocks」拒绝整个请求。
func (h *Handler) applyReplayGate(req *rosetta.ChatRequest, protocol string) {
	isChat := false
	switch protocol {
	case "", "chat", "auto":
		isChat = true
	case "anthropic":
	default:
		// response handles foreign seals inside the SDK.
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
		hasText, hasToolCall := false, false
		for _, b := range m.Blocks {
			switch b.Type {
			case rosetta.BlockThinking:
				if isChat {
					if b.Signature != "" || strings.TrimSpace(b.Thinking) == "" {
						removed++
						continue
					}
				} else if b.Signature == "" {
					removed++
					continue
				}
			case rosetta.BlockRedactedThinking:
				if isChat {
					removed++
					continue
				}
			case rosetta.BlockText:
				if strings.TrimSpace(b.Text) != "" {
					hasText = true
				}
			case rosetta.BlockToolCall:
				hasToolCall = true
			}
			blocks = append(blocks, b)
		}
		// chat 里「只有思考、没有正文也没有工具调用」的轮编不出合法消息：
		// SDK 的 chat 编码器会以「assistant turn has only thinking blocks」
		// 拒绝整个请求。整轮丢掉 —— 它本来就不带任何对话信息。
		if isChat && len(blocks) > 0 && !hasText && !hasToolCall {
			droppedBlocks += len(blocks)
			droppedTurns++
			continue
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
	if droppedBlocks == 0 && droppedTurns == 0 {
		return
	}
	req.Messages = kept
	h.logger.Debug("stripped unplayable thinking blocks for upstream protocol",
		"protocol", protocol, "blocks", droppedBlocks, "turns", droppedTurns)
}
