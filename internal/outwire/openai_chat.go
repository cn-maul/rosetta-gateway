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

type OpenAIChatResponse struct {
	ID      string             `json:"id"`
	Object  string             `json:"object"`
	Created int64              `json:"created"`
	Model   string             `json:"model"`
	Choices []OpenAIChatChoice `json:"choices"`
	Usage   *OpenAIUsage       `json:"usage,omitempty"`
}

type OpenAIChatChoice struct {
	Index        int            `json:"index"`
	Message      *OpenAIMessage `json:"message,omitempty"`
	Delta        *OpenAIMessage `json:"delta,omitempty"`
	FinishReason *string        `json:"finish_reason"`
}

type OpenAIMessage struct {
	Role      string          `json:"role"`
	Content   json.RawMessage `json:"content,omitempty"`
	ToolCalls json.RawMessage `json:"tool_calls,omitempty"`
	// ReasoningContent 承载思维链，与流式的 delta.reasoning_content 同名同义
	// （DeepSeek / Qwen / vLLM 的约定）。为空时整体省略，标准 OpenAI 客户端
	// 对未知字段是忽略语义，不受影响。
	ReasoningContent string `json:"reasoning_content,omitempty"`
}

type OpenAIToolCall struct {
	ID       string         `json:"id"`
	Type     string         `json:"type"`
	Function OpenAIFunction `json:"function"`
}

type OpenAIFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type OpenAIUsage struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	TotalTokens      int64 `json:"total_tokens"`
	// PromptTokensDetails 承载缓存命中的 token 数。
	//
	// 不能省：流式路径一直有输出（openai_chat.go 的 SSE usage chunk 里
	// 带 prompt_tokens_details.cached_tokens），非流式路径却没有 ——
	// 同一个网关、同一次缓存命中，**非流式客户端算出的成本会系统性偏高
	// 一个数量级**（把全部 prompt token 按全价计费）。这种不一致尤其刺眼：
	// 用户切到非流式模式后账单突然变高，而响应里没有任何线索说明为什么。
	PromptTokensDetails *PromptTokensDetails `json:"prompt_tokens_details,omitempty"`
}

type PromptTokensDetails struct {
	CachedTokens int64 `json:"cached_tokens"`
}

type SSEWriter struct {
	w       io.Writer
	flusher http.Flusher
	id      string
	model   string
	created int64

	// mu 串行化对底层 writer 的写入。网关的心跳 goroutine 与主事件循环会并发
	// 写同一个 http.ResponseWriter，而 Go 明确不支持并发使用 ResponseWriter
	// （server.statusResponseWriter 的 written/statusCode 也是无锁写）。
	// 实测高频窗口下未观测到字节损坏，但那是运气，不是契约 —— 一旦交错出一个
	// 半个 `data:` 行，下游只会报「流式响应中没有内容」。
	mu       sync.Mutex
	roleSent bool
}

func NewSSEWriter(w io.Writer, flusher http.Flusher, id, model string, created int64) *SSEWriter {
	return &SSEWriter{w: w, flusher: flusher, id: id, model: model, created: created}
}

// SetResponseID 在上游 message_start 带回真实响应 id 后覆盖预先生成的兜底 id；
// 必须在首个分片写出前调用，否则分片已带旧 id 无法追溯。
func (sw *SSEWriter) SetResponseID(id string) {
	sw.mu.Lock()
	defer sw.mu.Unlock()
	if sw.roleSent || id == "" {
		return
	}
	sw.id = id
}

func (sw *SSEWriter) WriteEvent(event, data string) error {
	sw.mu.Lock()
	defer sw.mu.Unlock()
	if event != "" {
		fmt.Fprintf(sw.w, "event: %s\n", event)
	}
	fmt.Fprintf(sw.w, "data: %s\n\n", data)
	if sw.flusher != nil {
		sw.flusher.Flush()
	}
	return nil
}

// WriteComment 写一行 SSE 注释（`: keepalive`）。注释被所有标准 SSE 客户端忽略，
// 唯一作用是让连接与中间代理不因长时间零字节而误判空闲并断开。
func (sw *SSEWriter) WriteComment(text string) error {
	sw.mu.Lock()
	defer sw.mu.Unlock()
	fmt.Fprintf(sw.w, ": %s\n\n", text)
	if sw.flusher != nil {
		sw.flusher.Flush()
	}
	return nil
}

// writeChunk 组装一个标准的 chat.completion.chunk 信封（含 id/object/created/model），
// 严格客户端依赖这些字段与末尾的 finish_reason 分片。
func (sw *SSEWriter) writeChunk(delta map[string]any, finish *string) error {
	chunk := map[string]any{
		"id":      sw.id,
		"object":  "chat.completion.chunk",
		"created": sw.created,
		"model":   sw.model,
		"choices": []map[string]any{{
			"index":         0,
			"delta":         delta,
			"finish_reason": finish,
		}},
	}
	data, _ := json.Marshal(chunk)
	return sw.WriteEvent("", string(data))
}

// ensureRole 在首个内容/工具分片前补发 delta.role="assistant"，符合 OpenAI 流式约定。
func (sw *SSEWriter) ensureRole() error {
	if sw.roleSent {
		return nil
	}
	sw.roleSent = true
	return sw.writeChunk(map[string]any{"role": "assistant"}, nil)
}

func (sw *SSEWriter) WriteTextDelta(delta string) error {
	if err := sw.ensureRole(); err != nil {
		return err
	}
	return sw.writeChunk(map[string]any{"content": delta}, nil)
}

// WriteThinkingDelta 把上游的推理（思维链）增量透传为 delta.reasoning_content。
//
// OpenAI 官方协议没有这个字段，但 DeepSeek / Qwen / vLLM / OpenRouter 一致用它承载
// reasoning_content，rosetta 的 openai-chat 适配器也按这个键回读
// （provider_openai_chat.go 的 ReasoningContent），所以键名对整个链路是自洽的。
//
// 不透传的代价不只是「少了一个字段」：只吐思考的流（思考型模型在 max_tokens
// 耗尽于思考期时正是这种形态）到下游会变成**零内容**的流，且照样以
// finish_reason:"stop" + [DONE] 收尾，下游只能报出「流式响应中没有内容」这种
// 指向不了任何一层的错误。
func (sw *SSEWriter) WriteThinkingDelta(delta string) error {
	if err := sw.ensureRole(); err != nil {
		return err
	}
	return sw.writeChunk(map[string]any{"reasoning_content": delta}, nil)
}

// WriteToolCallDelta 发一个工具调用增量分片。
//
// # function 必须始终存在（2026-10 修复的 opencode 兼容性缺陷）
//
// OpenAI 的流式协议里 delta.tool_calls[] 的每一项都**必须**带 function
// （首片给 name、续片只给 arguments 增量；两者都空时给空串）。
//
// 原实现是「name 与 arguments 都为空就整个省略 function」：
//
//	if len(fn) > 0 { td["function"] = fn }
//
// 于是上游吐一个「只带 index 的空拍」时，网关发出的是
// `{"index":0}` —— 严格按 schema 校验的客户端（opencode 的 AI SDK）
// 直接报：
//
//	invalid_type at choices[0].delta.tool_calls[0].function
//	Invalid input: expected object, received undefined
//
// 实测复现（Hy4 / OpenRouter 会吐这种空拍）：网关产出的分片与报错里的
// 那一条逐字节一致。
//
// 省略字段的初衷是「不发明数据」，但空对象 {} 并不发明任何东西 ——
// 它只是把「这一拍没有新增的 name/arguments」表达成协议要求的形状。
// 客户端把 arguments 增量拼起来时，空串增量本来就是无操作。
func (sw *SSEWriter) WriteToolCallDelta(index int, id, name, argsDelta string) error {
	if err := sw.ensureRole(); err != nil {
		return err
	}
	td := map[string]any{"index": index}
	if id != "" {
		td["id"] = id
		td["type"] = "function"
	}
	// function 恒存在：缺失即违反协议，严格客户端会整条响应判为非法。
	// name/arguments 为空时给空串而不是省略键 —— 空串增量在客户端侧
	// 是「本次没有新增内容」，语义正确且符合 schema。
	td["function"] = map[string]any{
		"name":      name,
		"arguments": argsDelta,
	}
	return sw.writeChunk(map[string]any{"tool_calls": []map[string]any{td}}, nil)
}

// WriteFinish 发出带 finish_reason 的收尾分片（delta 为空），必须在 [DONE] 之前。
func (sw *SSEWriter) WriteFinish(reason string) error {
	if err := sw.ensureRole(); err != nil {
		return err
	}
	r := reason
	return sw.writeChunk(map[string]any{}, &r)
}

func (sw *SSEWriter) WriteUsage(u rosetta.Usage) error {
	usage := map[string]any{
		"prompt_tokens":     u.InputTokens,
		"completion_tokens": u.OutputTokens,
		"total_tokens":      u.TotalTokens,
	}
	// 缓存命中的 token 按 OpenAI 官方口径放在 prompt_tokens_details.cached_tokens。
	// 网关自己明明把 CachedInputTokens 记进了 usage_records，却不下发给下游 ——
	// 下游据此算出的成本会系统性偏高（缓存命中部分通常便宜一个数量级）。
	if u.CachedInputTokens > 0 {
		usage["prompt_tokens_details"] = map[string]any{"cached_tokens": u.CachedInputTokens}
	}
	chunk := map[string]any{
		"id":      sw.id,
		"object":  "chat.completion.chunk",
		"created": sw.created,
		"model":   sw.model,
		"choices": []any{},
		"usage":   usage,
	}
	data, _ := json.Marshal(chunk)
	return sw.WriteEvent("", string(data))
}

func (sw *SSEWriter) WriteDone() error {
	return sw.WriteEvent("", "[DONE]")
}

// OpenAIFinishReason 把 rosetta 的统一停止原因映射为 OpenAI 的 finish_reason 字符串。
func OpenAIFinishReason(s rosetta.StopReason) string {
	switch s {
	case rosetta.StopLength:
		return "length"
	case rosetta.StopToolUse:
		return "tool_calls"
	case rosetta.StopContentFilter:
		return "content_filter"
	default:
		return "stop"
	}
}

func WriteNonStreamResponse(w http.ResponseWriter, resp *rosetta.ChatResponse, model string) {
	choices := make([]OpenAIChatChoice, 0)
	finishReason := OpenAIFinishReason(resp.StopReason)

	msg := &OpenAIMessage{
		Role:             "assistant",
		Content:          jsonString(resp.Text()),
		ReasoningContent: resp.ThinkingText(),
	}
	if len(resp.ToolCalls()) > 0 {
		tcs := make([]OpenAIToolCall, 0, len(resp.ToolCalls()))
		for _, tc := range resp.ToolCalls() {
			tcs = append(tcs, OpenAIToolCall{
				ID:   tc.ToolCallID,
				Type: "function",
				Function: OpenAIFunction{
					Name:      tc.ToolName,
					Arguments: tc.Arguments,
				},
			})
		}
		raw, _ := json.Marshal(tcs)
		msg.ToolCalls = raw
		// OpenAI 的语义是「带 tool_calls 时 content 为 null」，而不是整个字段消失。
		// 置 nil 会被 omitempty 连字段一起抹掉，客户端拿不到 content 键 ——
		// 部分 SDK 会把它判成响应结构不合法。
		msg.Content = json.RawMessage("null")
	}

	choices = append(choices, OpenAIChatChoice{
		Index:        0,
		Message:      msg,
		FinishReason: &finishReason,
	})

	usage := &OpenAIUsage{
		PromptTokens:     resp.Usage.InputTokens,
		CompletionTokens: resp.Usage.OutputTokens,
		TotalTokens:      resp.Usage.TotalTokens,
	}
	// 与流式路径对齐：缓存命中的 prompt token 必须报出来，否则非流式客户端
	// 按全价计算成本，账单会明显高于同一请求的流式版本。
	if resp.Usage.CachedInputTokens > 0 {
		usage.PromptTokensDetails = &PromptTokensDetails{
			CachedTokens: resp.Usage.CachedInputTokens,
		}
	}

	out := OpenAIChatResponse{
		ID:      resp.ID,
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   model,
		Choices: choices,
		Usage:   usage,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

// jsonString 把字符串编码成一个完整的 JSON 字面量（含引号与转义）。
// 此前是「手工拼引号 + 剥掉首尾引号的 json.Marshal 产物」，结果等价但脆弱：
// 任一处引号写漏就产出非法 JSON，而这是下游解析的第一个字段。
func jsonString(s string) json.RawMessage {
	b, err := json.Marshal(s)
	if err != nil {
		return json.RawMessage(`""`)
	}
	return json.RawMessage(b)
}
