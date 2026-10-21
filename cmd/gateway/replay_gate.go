package main

// 回放闸门：把「目标协议无法回放」的思考块在**出站前**剥掉。
//
// # 为什么需要它
//
// Anthropic Messages 入站允许客户端回传 thinking / redacted_thinking 块
// （inwire/anthropic.go 的 assistantBlocks 原样搬进统一模型，带签名）。
// 这是**正确**的：签名的 thinking 只有原样回放才有效。
//
// 但下游目标协议不一定有这个概念，于是同一批块在出站时会撞上 SDK 各家的
// 硬约束：
//
//   - openai-chat（provider_openai_chat.go）：thinking / redacted-thinking
//     一律跳过。若该 assistant turn 恰好只有这些块，跳完就变成一条
//     「什么都没有」的 assistant，SDK 明确报
//     "assistant turn has only thinking/redacted-thinking blocks,
//     which the OpenAI chat protocol cannot replay"（400）。
//   - anthropic（provider_anthropic.go）：无签名的 thinking 被跳过；该
//     turn 若因此空了，同样报 "assistant turn has only unsigned thinking
//     blocks, which anthropic cannot replay"。
//   - openai-responses：foreign seal 由 SealedBy 门闩挡掉，空 turn 不报错，
//     不需要这一刀。
//
// 这两条报错是 SDK **故意**写清楚的（宁可报错也不发一条畸形空消息出去），
// 但它没有退路可选 —— 客户端无法为别人的协议重造签名，剥掉是唯一的走法。
// 网关必须在 attempt 内做这一刀，因为故障转移链上换一个目标就换一个协议。
//
// # 剥掉是否损失信息
//
// 不损失**对话语义**：思考文本从来不是对话内容，上游在这些协议上也从来
// 收不到它。被剥掉的 turn 必然不含 text / tool_call（否则就不空了），
// 也就是说它对模型而言本来就是一条空消息。删掉它不改变模型看到的
// 上下文形状，也不打断工具调用链 —— tool_call 块在同一 turn 里，会被
// 保留。
//
// 唯一真正丢失的是 DeepSeek 系在部分场景下对 reasoning_content 回放的
// 偏好。那需要改 SDK 的 chat 编码器（网关改不了，Extra 也进不了 messages），
// 等 SDK 支持后再说。

import (
	"log/slog"

	"github.com/cn-maul/rosetta"
)

// applyReplayGate 按目标上游协议剥掉无法回放的思考块，并丢弃剥空后的
// assistant turn。幂等、可重复调用。
//
// 与 ingress.Request.ApplyUpstreamExtras / ApplyThinkingCapability 一样，必须在每个
// attempt 内部调（协议是「发给谁」这一层的属性）。
func applyReplayGate(req *rosetta.ChatRequest, protocol string) {
	// keepThinking 表达「这个协议能原样回放哪一类思考块」。
	// chat 一个都不能 —— 它把 thinking / redacted-thinking 一律跳过。
	keepThinking, keepRedacted := false, false
	switch protocol {
	case "", "openai-chat", "auto":
		// chat 协议完全没有思考块的概念：带不带签名都剥。
	case "anthropic":
		// Anthropic 能原样回放**有签名**的 thinking 与 redacted_thinking，
		// 只剥无签名的（客户端手写、签名已在往返中丢失的那种）。
		keepThinking, keepRedacted = true, true
	default:
		// openai-responses 由 SDK 的 SealedBy 门闩自行处理，不干预。
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
				// anthropic 有 redacted_thinking（data 原样回放），chat 没有。
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
		// 剥空了就整条删掉：留着一条零块 assistant，SDK 会以「只有思考块
		// 无法回放」报 400，而它其实已经什么都不剩了。
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
	slog.Debug("stripped unplayable thinking blocks for upstream protocol",
		"protocol", protocol, "blocks", droppedBlocks, "turns", droppedTurns)
}
