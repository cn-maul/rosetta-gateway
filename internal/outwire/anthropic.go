package outwire

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/cn-maul/rosetta"
)

// ---- 错误响应（DESIGN §9 Anthropic 形状）----
//
// {"type":"error","error":{"type":"<err_type>","message":"..."}}
//
// Anthropic 的错误体没有 code 字段，网关内部的语义 code（如 upstream_auth_error）
// 在这里翻译成 Anthropic 的错误类型；状态码沿用 MapUpstreamError 的映射结果，
// 不在 Anthropic 通道里重新分类。

func WriteAnthropicError(w http.ResponseWriter, statusCode int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	json.NewEncoder(w).Encode(map[string]any{
		"type": "error",
		"error": map[string]any{
			"type":    anthropicErrorType(code, statusCode),
			"message": message,
		},
	})
}

func anthropicErrorType(code string, status int) string {
	switch code {
	case "invalid_api_key":
		// 401 = 没给 key / key 不对；403 = key 被停用 —— Anthropic 用两种类型区分。
		if status == http.StatusForbidden {
			return "permission_error"
		}
		return "authentication_error"
	case "invalid_request_error", "context_length_exceeded":
		return "invalid_request_error"
	case "model_not_found":
		return "not_found_error"
	case "insufficient_quota", "rate_limit_exceeded":
		return "rate_limit_error"
	case "request_too_large":
		return "request_too_large"
	case "internal_error":
		return "api_error"
	default:
		// upstream_* 一族：网关把上游问题翻译成 502/504/404/429 带给下游，
		// 类型按状态码归类，不在消息里暴露内部 code。
		switch {
		case status == http.StatusNotFound:
			return "not_found_error"
		case status == http.StatusTooManyRequests:
			return "rate_limit_error"
		default:
			return "api_error"
		}
	}
}

// ---- 非流式响应 ----

// WriteAnthropicResponse 编码 Anthropic Messages 形状的非流式响应。
// content 块按 rosetta 统一内容的原始顺序输出（thinking → text → tool_use），
// 上游是 OpenAI 系时 thinking 以 reasoning_content 聚合而来，天然只有一块。
func WriteAnthropicResponse(w http.ResponseWriter, resp *rosetta.ChatResponse, model string) {
	content := make([]map[string]any, 0, len(resp.Content))
	for _, blk := range resp.Content {
		switch blk.Type {
		case rosetta.BlockText:
			content = append(content, map[string]any{"type": "text", "text": blk.Text})
		case rosetta.BlockThinking:
			b := map[string]any{"type": "thinking", "thinking": blk.Thinking}
			if blk.Signature != "" {
				b["signature"] = blk.Signature
			}
			content = append(content, b)
		case rosetta.BlockRedactedThinking:
			content = append(content, map[string]any{"type": "redacted_thinking", "data": blk.Thinking})
		case rosetta.BlockToolCall:
			// input 必须是 JSON 对象；rosetta 校验已保证非空时是合法 JSON 对象，
			// 空 input 补 {} —— Anthropic 的 input 字段不可缺省。
			input := json.RawMessage(blk.Arguments)
			if len(input) == 0 {
				input = json.RawMessage(`{}`)
			}
			content = append(content, map[string]any{
				"type": "tool_use", "id": blk.ToolCallID, "name": blk.ToolName, "input": input,
			})
		}
	}

	usage := map[string]any{
		"input_tokens":  resp.Usage.InputTokens,
		"output_tokens": resp.Usage.OutputTokens,
	}
	if resp.Usage.CachedInputTokens > 0 {
		usage["cache_read_input_tokens"] = resp.Usage.CachedInputTokens
	}
	if resp.Usage.CachedCreationTokens > 0 {
		usage["cache_creation_input_tokens"] = resp.Usage.CachedCreationTokens
	}

	id := resp.ID
	if id == "" {
		id = fmt.Sprintf("msg_%d", time.Now().UnixNano())
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"id":            id,
		"type":          "message",
		"role":          "assistant",
		"model":         model,
		"content":       content,
		"stop_reason":   AnthropicStopReason(resp.StopReason),
		"stop_sequence": nil,
		"usage":         usage,
	})
}

// AnthropicStopReason 把 rosetta 统一停止原因映射为 Anthropic 的 stop_reason。
// Anthropic 没有内容过滤语义：content_filter / refusal 都映射为 refusal
// （closest semantic：生成因内容策略中止）。
func AnthropicStopReason(s rosetta.StopReason) string {
	switch s {
	case rosetta.StopLength:
		return "max_tokens"
	case rosetta.StopToolUse:
		return "tool_use"
	case rosetta.StopContentFilter, rosetta.StopRefusal:
		return "refusal"
	default: // end / other / 未知
		return "end_turn"
	}
}

// ---- 流式编码 ----

// AnthropicSSE 把统一事件流编码为 Anthropic Messages 的 SSE 事件序列：
//
//	message_start → (content_block_start → content_block_delta* → content_block_stop)*
//	  → message_delta(stop_reason, usage) → message_stop
//
// 块状态机：Anthropic 要求块严格按 index 顺序 start/stop，文本、思考、工具
// 调用各自是一块。上游（rosetta）保证同类增量连续到达，这里按「类型或工具
// id 变化即切块」推进 index。断流（truncated/overflow/error）按 DESIGN §8.2
// 发 error 事件后直接关闭 —— 不发 message_stop，客户端据此判定异常。
type AnthropicSSE struct {
	w       io.Writer
	flusher http.Flusher
	model   string
	// mu 串行化写底层 writer：心跳 goroutine（Keepalive）与主事件循环
	// （Event/Finish）会并发到达，与 SSEWriter 同理。
	mu sync.Mutex

	id         string
	started    bool // message_start 已发出
	blockIndex int
	openType   string // "" | "text" | "thinking" | "tool_use"
	openToolID string

	wroteContent bool
	sawTerminal  bool
}

// NewAnthropicSSE 构造 sink。id 是网关生成的兜底响应 id：message_start 若等
// 不到上游的 message_start 事件（某些上游直接吐内容），用兜底 id 开场。
func NewAnthropicSSE(w io.Writer, flusher http.Flusher, id, model string) *AnthropicSSE {
	return &AnthropicSSE{w: w, flusher: flusher, id: id, model: model}
}

func (s *AnthropicSSE) writeRaw(event, data string) {
	fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", event, data)
	if s.flusher != nil {
		s.flusher.Flush()
	}
}

func (s *AnthropicSSE) writeEvent(event string, payload any) {
	data, err := json.Marshal(payload)
	if err != nil {
		// 载荷全部是可序列化的基本类型/map，这里只为守住契约；真失败时宁可
		// 跳过这条分片也不能写出半截 JSON。
		return
	}
	s.writeRaw(event, string(data))
}

// Keepalive 写一条空闲心跳（SSE 注释形式，对所有标准客户端与中间代理无害）。
func (s *AnthropicSSE) Keepalive() {
	s.mu.Lock()
	defer s.mu.Unlock()
	fmt.Fprintf(s.w, ": %s\n\n", "keepalive")
	if s.flusher != nil {
		s.flusher.Flush()
	}
}

// ensureStarted 补发 message_start。正常情况下上游首个事件就是 message_start
// 并带回响应 id；某些上游可能直接吐内容，这里用生成 id 兜底。
//
// usage 里的 input_tokens 此刻必然未知（上游在结束时才报），发 0 ——
// 权威数字在 message_delta 的 usage 里补齐（Anthropic 的增量 usage 是
// 累计口径，客户端以最终值为准；发 0 比谎报一个好）。
func (s *AnthropicSSE) ensureStarted(id string) {
	if s.started {
		return
	}
	s.started = true
	if id != "" {
		s.id = id
	}
	s.writeEvent("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id": s.id, "type": "message", "role": "assistant", "model": s.model,
			"content": []any{}, "stop_reason": nil, "stop_sequence": nil,
			"usage": map[string]any{"input_tokens": 0, "output_tokens": 0},
		},
	})
}

// ensureBlock 切到指定类型的开放块：类型不变则复用；变化则先 stop 旧块再
// start 新块（index 严格递增）。
func (s *AnthropicSSE) ensureBlock(blockType, toolID, toolName string) {
	if s.openType == blockType && (blockType != "tool_use" || s.openToolID == toolID) {
		return
	}
	s.closeOpenBlock()
	s.ensureStarted("")
	idx := s.blockIndex
	s.blockIndex++
	var cb map[string]any
	switch blockType {
	case "text":
		cb = map[string]any{"type": "text", "text": ""}
	case "thinking":
		cb = map[string]any{"type": "thinking", "thinking": ""}
	case "tool_use":
		cb = map[string]any{"type": "tool_use", "id": toolID, "name": toolName, "input": map[string]any{}}
	}
	s.writeEvent("content_block_start", map[string]any{
		"type": "content_block_start", "index": idx, "content_block": cb,
	})
	s.openType = blockType
	s.openToolID = toolID
}

func (s *AnthropicSSE) closeOpenBlock() {
	if s.openType == "" {
		return
	}
	s.writeEvent("content_block_stop", map[string]any{
		"type": "content_block_stop", "index": s.blockIndex - 1,
	})
	s.openType = ""
	s.openToolID = ""
}

// Event 消费一个上游事件。EventMessageEnd 只留痕不编码 —— 终止序列
// （含 stop_reason 与权威 usage）统一由 Finish 发出，保证终态判定
// （canceled/truncated）发生在任何终止分片之前。
func (s *AnthropicSSE) Event(ev *rosetta.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch ev.Type {
	case rosetta.EventMessageStart:
		s.ensureStarted(ev.ID)
	case rosetta.EventTextDelta:
		s.ensureBlock("text", "", "")
		if ev.Text != "" {
			s.wroteContent = true
		}
		s.writeEvent("content_block_delta", map[string]any{
			"type": "content_block_delta", "index": s.blockIndex - 1,
			"delta": map[string]any{"type": "text_delta", "text": ev.Text},
		})
	case rosetta.EventThinkingDelta:
		s.ensureBlock("thinking", "", "")
		if ev.Text != "" {
			s.wroteContent = true
			s.writeEvent("content_block_delta", map[string]any{
				"type": "content_block_delta", "index": s.blockIndex - 1,
				"delta": map[string]any{"type": "thinking_delta", "thinking": ev.Text},
			})
		}
		if ev.Signature != "" {
			// 空文本 + 签名 = Anthropic thinking 块的收尾签名；原样透传，
			// 否则下游把这段 thinking 回传给 Anthropic 上游时会因签名缺失被拒。
			s.writeEvent("content_block_delta", map[string]any{
				"type": "content_block_delta", "index": s.blockIndex - 1,
				"delta": map[string]any{"type": "signature_delta", "signature": ev.Signature},
			})
		}
	case rosetta.EventToolCall:
		s.ensureBlock("tool_use", ev.ToolID, ev.ToolName)
		s.wroteContent = true
		s.writeEvent("content_block_delta", map[string]any{
			"type": "content_block_delta", "index": s.blockIndex - 1,
			"delta": map[string]any{"type": "input_json_delta", "partial_json": ev.ArgumentsDelta},
		})
	case rosetta.EventMessageEnd:
		s.sawTerminal = true
	}
}

func (s *AnthropicSSE) Finish(status string, stop rosetta.StopReason, usage rosetta.Usage, _ bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if status == "canceled" {
		// 客户端已经走了，写什么都是徒劳。
		return
	}
	s.closeOpenBlock()

	if status != "ok" {
		// DESIGN §8.2：Anthropic 下游的断流语义 —— 发 error 事件后关闭，
		// 不发 message_stop。客户端据此判定异常而不是把半截回答当成功。
		s.writeEvent("error", map[string]any{
			"type":  "error",
			"error": map[string]any{"type": "api_error", "message": "upstream stream truncated"},
		})
		return
	}

	u := map[string]any{"output_tokens": usage.OutputTokens}
	// message_start 时 input 未知发了 0，这里补权威值。Anthropic 的增量
	// usage 是累计口径，客户端以最终值计算成本。
	if usage.InputTokens > 0 {
		u["input_tokens"] = usage.InputTokens
	}
	if usage.CachedInputTokens > 0 {
		u["cache_read_input_tokens"] = usage.CachedInputTokens
	}
	if usage.CachedCreationTokens > 0 {
		u["cache_creation_input_tokens"] = usage.CachedCreationTokens
	}
	s.writeEvent("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": AnthropicStopReason(stop), "stop_sequence": nil},
		"usage": u,
	})
	s.writeEvent("message_stop", map[string]any{"type": "message_stop"})
}

func (s *AnthropicSSE) WroteContent() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.wroteContent
}

func (s *AnthropicSSE) SawTerminal() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sawTerminal
}
