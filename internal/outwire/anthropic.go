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
	case "invalid_request_error", "context_length_exceeded", "request_too_large":
		return "invalid_request_error"
	case "model_not_found":
		return "not_found_error"
	case "insufficient_quota", "rate_limit_exceeded", "upstream_quota_exhausted":
		return "rate_limit_error"
	case "upstream_auth_error":
		// 上游 401/403：目标级凭据问题，报成 api_error 会让客户端以为是自己
		// 请求的问题而不重试、不换凭据。
		if status == http.StatusForbidden {
			return "permission_error"
		}
		return "authentication_error"
	case "upstream_timeout":
		return "timeout_error"
	case "internal_error":
		return "api_error"
	default:
		// upstream_* 一族：网关把上游问题翻译成 502/504/404/429 带给下游，
		// 类型按状态码归类，不在消息里暴露内部 code。
		//
		// overloaded_error 值得单独一类：Claude Code 与 Anthropic 官方 SDK
		// 对 529 有专门的指数退避重试策略，收到 api_error 时行为可能不同。
		switch {
		case status == http.StatusNotFound:
			return "not_found_error"
		case status == http.StatusTooManyRequests:
			return "rate_limit_error"
		case status == statusOverloaded:
			return "overloaded_error"
		case status == http.StatusGatewayTimeout:
			return "timeout_error"
		case status == http.StatusPaymentRequired:
			return "billing_error"
		case status == http.StatusConflict:
			return "conflict_error"
		default:
			return "api_error"
		}
	}
}

// statusOverloaded 是 Anthropic 的「上游过载」状态码。它不是标准 HTTP 码，
// 由 Anthropic 自定义；SDK 依据它 + overloaded_error 类型做退避重试。
const statusOverloaded = 529

// ---- usage 编码（Anthropic 计费桶语义）----

// anthropicUsageFields 把 rosetta 统一 Usage 翻译成 Anthropic wire 的 usage 对象。
//
// 关键在 input_tokens：SDK 的统一 Usage 沿 OpenAI 语义（CachedInputTokens ⊆
// InputTokens，见 provider_anthropic.go 的 toUsage —— 它把缓存读写**折进**了
// InputTokens），而 Anthropic wire 语义里 input_tokens / cache_read_input_tokens /
// cache_creation_input_tokens 是**互斥**的计费桶。直接把统一 InputTokens 抄进
// input_tokens 会把缓存 token 计两遍，下游按 Anthropic 口径计费时多收一次钱。
// 所以这里先拆掉缓存部分（下限 0，防非同类上游报出含缓存的负差值），缓存
// 读/写各填各的字段。
//
// emitInput 控制是否带 input_tokens：非流式恒带（官方响应里它是必有字段）；
// 流式 message_delta 只有上游真的报了输入（原始 InputTokens > 0）才带 ——
// message_start 阶段发 0 占位、终值在此补齐的既有约定保持不变。
func anthropicUsageFields(u rosetta.Usage, emitInput bool) map[string]any {
	uncached := u.InputTokens - u.CachedInputTokens - u.CachedCreationTokens
	if uncached < 0 {
		uncached = 0
	}
	usage := map[string]any{"output_tokens": u.OutputTokens}
	if emitInput {
		usage["input_tokens"] = uncached
	}
	if u.CachedInputTokens > 0 {
		usage["cache_read_input_tokens"] = u.CachedInputTokens
	}
	if u.CachedCreationTokens > 0 {
		usage["cache_creation_input_tokens"] = u.CachedCreationTokens
	}
	return usage
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

	// 统一 Usage 的 InputTokens 已含缓存读写（OpenAI 语义），Anthropic wire
	// 要求三桶互斥 —— 拆分逻辑见 anthropicUsageFields。
	usage := anthropicUsageFields(resp.Usage, true)

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
	// openToolIdx 是当前开放 tool_use 块对应的 ToolIndex（同块判据，见 ensureBlock）。
	openToolIdx int
	// pendingToolID/pendingToolName 只在首片（ToolID 非空）有意义，
	// 由 EventToolCall 在切块前填入。
	pendingToolID   string
	pendingToolName string

	wroteContent bool
	sawTerminal  bool
}

// NewAnthropicSSE 构造 sink。id 是网关生成的兜底响应 id：message_start 若等
// 不到上游的 message_start 事件（某些上游直接吐内容），用兜底 id 开场。
func NewAnthropicSSE(w io.Writer, flusher http.Flusher, id, model string) *AnthropicSSE {
	// openToolIdx 初始为 -1（而非零值 0）：否则「还没开过块」与
	//「开了 ToolIndex=0 的块」不可区分，首片会被误判为同块而复用。
	return &AnthropicSSE{w: w, flusher: flusher, id: id, model: model, openToolIdx: -1}
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

// ensureBlock 切到指定类型的开放块：同一块则复用；变化则先 stop 旧块再
// start 新块（index 严格递增）。
//
// **同块的判据是 ToolIndex，不是 ToolID**：rosetta 的流式契约是首片带
// ToolID/ToolName、续片只带 ToolIndex+ArgumentsDelta（SDK 侧
// anthropic 的 input_json_delta、openai-chat 的续 chunk、responses 的
// function_call_arguments.delta 都不填 ToolID）。用 ToolID 判会让每个参数
// 分片都切出一个新块：客户端收到两个 tool_use —— 一个有 id/name 但参数
// 为空、一个有参数但没 id/name，工具调用整体失效。
func (s *AnthropicSSE) ensureBlock(blockType string, toolIdx int) {
	if s.openType == blockType && (blockType != "tool_use" || s.openToolIdx == toolIdx) {
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
		// 首片（ToolID 非空）才带 id/name；续片复用同一个块，不再重开。
		cb = map[string]any{"type": "tool_use", "id": s.pendingToolID, "name": s.pendingToolName, "input": map[string]any{}}
	}
	s.writeEvent("content_block_start", map[string]any{
		"type": "content_block_start", "index": idx, "content_block": cb,
	})
	s.openType = blockType
	s.openToolIdx = toolIdx
}

func (s *AnthropicSSE) closeOpenBlock() {
	if s.openType == "" {
		return
	}
	s.writeEvent("content_block_stop", map[string]any{
		"type": "content_block_stop", "index": s.blockIndex - 1,
	})
	// 注意：这里只清「开放块」状态，**不能**清 pendingToolID/pendingToolName ——
	// ensureBlock 的执行顺序是先 closeOpenBlock() 再拿 pending* 构造新块，
	// 清了就会把第二个工具的 id/name 掏成空串。
	s.openType = ""
	s.openToolIdx = -1
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
		s.ensureBlock("text", 0)
		if ev.Text != "" {
			s.wroteContent = true
		}
		s.writeEvent("content_block_delta", map[string]any{
			"type": "content_block_delta", "index": s.blockIndex - 1,
			"delta": map[string]any{"type": "text_delta", "text": ev.Text},
		})
	case rosetta.EventThinkingDelta:
		s.ensureBlock("thinking", 0)
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
		// 首片会带 ToolID/ToolName；续片两者皆空，此时沿用上一次的值开块。
		if ev.ToolID != "" {
			s.pendingToolID, s.pendingToolName = ev.ToolID, ev.ToolName
		}
		s.ensureBlock("tool_use", ev.ToolIndex)
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

	// message_start 时 input 未知发了 0，这里补权威值。Anthropic 的增量
	// usage 是累计口径，客户端以最终值计算成本。缓存 token 必须从 input_tokens
	// 拆出、各填各的互斥计费桶（语义与拆分理由见 anthropicUsageFields）。
	u := anthropicUsageFields(usage, usage.InputTokens > 0)
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
