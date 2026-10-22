package inwire

import (
	"strings"

	"github.com/cn-maul/rosetta"
)

// Responses 入口的 thinking 回放：chat 上游版。
//
// # 为什么需要自己拼 messages
//
// rosetta 的 OpenAI chat 编码器明确丢弃 thinking 块（provider_openai_chat.go
// 的 encodeMessages），而 DeepSeek 的 chat 协议要求 `reasoning_content`
// 原样回传 —— 多轮 thinking 会话（尤其带工具调用）缺了它会直接 400
// "The reasoning_content in the thinking mode must be passed back"。
//
// Chat 入口的解法是把客户端原始 wire messages 整份透传（见
// OpenAIChatRequest.chatMessagesWithReasoning）。Responses 的 item 与 chat 的
// message 不是同一种形状，没有现成 wire 可透传，因此这里从**已经归一化过的**
// rosetta 消息重新编码一份 chat messages：相比照抄 Responses item→chat 的
// 整条转换链，输入是网关自己的统一模型（多模态、工具配对、媒体搬家都已由
// ToRosetta 处理完），编码规则与 SDK 的 encodeMessages 一一对应，只多写一个
// `reasoning_content` 字段。
//
// # 触发条件与回退
//
// 只有请求里真的存在**带可读文本的 thinking 块**时才接管；没有 reasoning 的
// 请求继续走 SDK 编码器，行为逐字不变。遇到本编码器不认识的形状时返回 false
// 并回退到 SDK 路径（宁可维持修复前的行为，也不要在网关里静默改写内容）。
//
// Anthropic 上游不在此列：它的 thinking 块必须带 Anthropic 自己签发的
// signature，Responses 的 reasoning item（summary + encrypted_content）无法
// 伪造签名，硬塞只会被上游拒绝，因此仍由 applyReplayGate 剥除。

// responsesChatMessagesWithReasoning 在需要回放 thinking 时返回完整的
// chat messages 数组；第二个返回值为 false 表示应继续走 SDK 编码器。
func responsesChatMessagesWithReasoning(req *rosetta.ChatRequest) ([]map[string]any, bool) {
	if req == nil || !hasReplayableResponsesReasoning(req.Messages) {
		return nil, false
	}

	// Responses 把一轮 assistant 拆成多条消息（reasoning / message /
	// function_call 各一条），chat 里它们必须合成**一条** assistant 消息：
	// reasoning_content 与 tool_calls 只有在同一条消息上才构成合法的一轮。
	var (
		out     = make([]map[string]any, 0, len(req.Messages)+1)
		pending *chatAssistantTurn
	)
	flush := func() {
		if pending == nil {
			return
		}
		if mm, ok := pending.message(); ok {
			out = append(out, mm)
		}
		pending = nil
	}

	if req.System != "" {
		out = append(out, map[string]any{"role": "system", "content": req.System})
	}

	for _, m := range req.Messages {
		switch m.Role {
		case rosetta.RoleAssistant:
			if pending == nil {
				pending = &chatAssistantTurn{}
			}
			if !pending.add(m) {
				return nil, false
			}
			continue
		}
		flush()
		switch m.Role {
		case rosetta.RoleSystem:
			if t := rosettaMessageText(m); t != "" {
				out = append(out, map[string]any{"role": "system", "content": t})
			}
		case rosetta.RoleUser:
			mm, ok := chatUserMessage(m)
			if !ok {
				return nil, false
			}
			out = append(out, mm)
		case rosetta.RoleTool:
			for _, b := range m.Blocks {
				if b.Type != rosetta.BlockToolResult {
					continue
				}
				out = append(out, map[string]any{
					"role":         "tool",
					"tool_call_id": b.ToolCallID,
					"content":      b.Content,
				})
			}
		default:
			return nil, false
		}
	}
	flush()
	return out, true
}

// hasReplayableResponsesReasoning 判断是否存在 chat 协议真的能回放的思考。
//
// 只认可读文本（thinking 块的 Thinking 字段）。encrypted_content 是 Responses
// 自家的封装，chat 协议没有承载它的字段；没有文本时回放一份空
// reasoning_content 既不满足上游，也会把「无状态可回放」伪装成已回放。
func hasReplayableResponsesReasoning(messages []rosetta.Message) bool {
	for _, m := range messages {
		if m.Role != rosetta.RoleAssistant {
			continue
		}
		for _, b := range m.Blocks {
			if b.Type == rosetta.BlockThinking && strings.TrimSpace(b.Thinking) != "" {
				return true
			}
		}
	}
	return false
}

type chatAssistantTurn struct {
	texts     []string
	reasoning []string
	toolCalls []map[string]any
}

// add 累积一条 assistant 消息；遇到 chat 无法表达的块返回 false。
func (t *chatAssistantTurn) add(m rosetta.Message) bool {
	for _, b := range m.Blocks {
		switch b.Type {
		case rosetta.BlockText:
			if b.Text != "" {
				t.texts = append(t.texts, b.Text)
			}
		case rosetta.BlockThinking:
			// RedactedThinking 没有可读文本，chat 没有对应字段：丢弃。
			if text := strings.TrimSpace(b.Thinking); text != "" {
				t.reasoning = append(t.reasoning, text)
			}
		case rosetta.BlockToolCall:
			t.toolCalls = append(t.toolCalls, map[string]any{
				"id":   b.ToolCallID,
				"type": "function",
				"function": map[string]any{
					"name":      b.ToolName,
					"arguments": b.Arguments,
				},
			})
		case rosetta.BlockRedactedThinking:
			// 无文本可回放，跳过。
		default:
			return false
		}
	}
	return true
}

func (t *chatAssistantTurn) message() (map[string]any, bool) {
	content := strings.Join(t.texts, "\n")
	// 只有思考、没有正文也没有工具调用的一轮，SDK 会直接报错（G6）；
	// chat 协议里它也没有任何可说的内容（applyReplayGate 同样会丢掉它）。
	if content == "" && len(t.toolCalls) == 0 {
		return nil, false
	}
	mm := map[string]any{"role": "assistant"}
	if content != "" {
		mm["content"] = content
	} else if len(t.reasoning) > 0 {
		// DeepSeek 的 assistant 消息带 reasoning_content 时仍要求有 content
		// 字段（可以是空串）。SDK 只在有正文时写 content，回放路径补一个
		// 空串，避免严格解析器把缺字段当成畸形消息。
		mm["content"] = ""
	}
	if len(t.reasoning) > 0 {
		mm["reasoning_content"] = strings.Join(t.reasoning, "\n")
	}
	if len(t.toolCalls) > 0 {
		mm["tool_calls"] = t.toolCalls
	}
	return mm, true
}

func rosettaMessageText(m rosetta.Message) string {
	var parts []string
	for _, b := range m.Blocks {
		if b.Type == rosetta.BlockText && b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// chatUserMessage 与 SDK 的 encodeOpenAIUser 等价：纯文本时给字符串，
// 含媒体块时给分块数组。
func chatUserMessage(m rosetta.Message) (map[string]any, bool) {
	multimodal := false
	for _, b := range m.Blocks {
		switch b.Type {
		case rosetta.BlockImage, rosetta.BlockAudio, rosetta.BlockFile:
			multimodal = true
		}
		if multimodal {
			break
		}
	}
	if !multimodal {
		return map[string]any{"role": "user", "content": rosettaMessageText(m)}, true
	}

	parts := make([]map[string]any, 0, len(m.Blocks))
	for _, b := range m.Blocks {
		switch b.Type {
		case rosetta.BlockText:
			if b.Text != "" {
				parts = append(parts, map[string]any{"type": "text", "text": b.Text})
			}
		case rosetta.BlockImage:
			parts = append(parts, map[string]any{
				"type":      "image_url",
				"image_url": map[string]any{"url": b.ImageURL},
			})
		case rosetta.BlockAudio:
			if b.AudioData == "" || (b.AudioFormat != "wav" && b.AudioFormat != "mp3") {
				return nil, false
			}
			parts = append(parts, map[string]any{
				"type":        "input_audio",
				"input_audio": map[string]any{"data": b.AudioData, "format": b.AudioFormat},
			})
		case rosetta.BlockFile:
			f := map[string]any{}
			if b.FileID != "" {
				f["file_id"] = b.FileID
			} else {
				data, ok := chatFileDataURL(b)
				if !ok {
					return nil, false
				}
				f["file_data"] = data
			}
			if b.FileName != "" {
				f["filename"] = b.FileName
			}
			parts = append(parts, map[string]any{"type": "file", "file": f})
		default:
			return nil, false
		}
	}
	return map[string]any{"role": "user", "content": parts}, true
}

// chatFileDataURL 与 SDK 的 fileDataURL 同规则：data: URL 原样、裸 base64
// 按 MimeType 包装；http(s) 地址 chat 协议不收，交给 SDK 路径报明确的 400。
func chatFileDataURL(b rosetta.Block) (string, bool) {
	if strings.HasPrefix(b.FileData, "data:") {
		return b.FileData, true
	}
	if strings.HasPrefix(b.FileData, "http://") || strings.HasPrefix(b.FileData, "https://") {
		return "", false
	}
	if b.FileData == "" {
		return "", false
	}
	media := b.MimeType
	if media == "" {
		media = "application/pdf"
	}
	return "data:" + media + ";base64," + b.FileData, true
}
