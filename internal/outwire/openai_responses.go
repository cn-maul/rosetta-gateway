package outwire

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/cn-maul/rosetta"
)

// OpenAI Responses 协议的出站编码（非流式对象 + SSE 事件流）。
//
// 事件序列（OpenAI 规范子集，覆盖 Codex / 客户端 SDK 的消费面）：
//
//	response.created
//	  → response.output_item.added → response.content_part.added
//	    → response.output_text.delta* → response.output_text.done
//	  → response.content_part.done → response.output_item.done
//	  → （function_call 同构：output_item.added → function_call_arguments.delta*
//	     → function_call_arguments.done → output_item.done）
//	→ response.completed（含完整 response 对象与 usage）
//
// 断流（truncated/overflow/error）发 response.failed 后关闭 —— Responses 协议
// 的 §8.2 等价物；canceled 不写任何东西。thinking 增量在本协议下不出事件
// （Responses 的 reasoning 输出是 provider 托管语义，跨协议透传没有意义），
// 与 OpenAI chat 下游把 reasoning_content 透传给 DeepSeek 系的既有约定不冲突。

// ResponsesSSE 把统一事件流编码为 Responses SSE。
type ResponsesSSE struct {
	w       io.Writer
	flusher http.Flusher
	model   string
	mu      sync.Mutex

	id          string
	started     bool // response.created 已发出
	outputIndex int
	// 打开中的 message item（content_index 恒 0：一个 message 一个 text part）。
	msgOpen bool
	text    strings.Builder
	// 打开中的 function_call item。
	toolOpen bool
	toolID   string
	toolName string
	// thinkText 累积当前 reasoning item 的可读文本，供 output_item.done
	// 的 summary 收尾。与 text（message 正文）分开：两者是不同的 item。
	thinkText strings.Builder
	// toolIdx 是当前开放 function_call item 对应的 ToolIndex。
	// 同项判据必须是它而不是 toolID：rosetta 的续片（Responses 侧的
	// function_call_arguments.delta）只带 ToolIndex+ArgumentsDelta，ToolID 为空。
	// 用 ToolID 判会把一个工具调用的参数分片拆成多个 item，客户端按 call_id
	// 分发工具执行时拿到的是空参数 + 空call_id 的幽灵项。
	toolIdx  int
	toolArgs strings.Builder
	fcSeq    int

	wroteContent bool
	sawTerminal  bool
	// output 累积已收尾的 output item（response.completed 内嵌完整对象）。
	output []map[string]any
}

func NewResponsesSSE(w io.Writer, flusher http.Flusher, id, model string) *ResponsesSSE {
	return &ResponsesSSE{w: w, flusher: flusher, id: id, model: model}
}

func (s *ResponsesSSE) writeEvent(event string, payload map[string]any) {
	payload["type"] = event
	data, err := json.Marshal(payload)
	if err != nil {
		return
	}
	fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", event, data)
	if s.flusher != nil {
		s.flusher.Flush()
	}
}

func (s *ResponsesSSE) responseSkeleton(status string) map[string]any {
	return map[string]any{
		"id":                  s.id,
		"object":              "response",
		"created_at":          time.Now().Unix(),
		"status":              status,
		"model":               s.model,
		"output":              []any{},
		"usage":               nil,
		"error":               nil,
		"incomplete_details":  nil,
		"parallel_tool_calls": true,
	}
}

func (s *ResponsesSSE) ensureStarted(id string) {
	if s.started {
		return
	}
	s.started = true
	if id != "" {
		s.id = id
	}
	s.writeEvent("response.created", map[string]any{"response": s.responseSkeleton("in_progress")})
}

// closeMessage 收尾打开中的 message item（part.done + item.done）。
func (s *ResponsesSSE) closeMessage() {
	if !s.msgOpen {
		return
	}
	idx := s.outputIndex - 1
	done := strings.Clone(s.text.String())
	s.writeEvent("response.output_text.done", map[string]any{
		"item_id": s.id + "-msg-" + itoa(idx), "output_index": idx, "content_index": 0, "text": done,
	})
	s.writeEvent("response.content_part.done", map[string]any{
		"item_id": s.id + "-msg-" + itoa(idx), "output_index": idx, "content_index": 0,
		"part": map[string]any{"type": "output_text", "text": done, "annotations": []any{}},
	})
	item := map[string]any{
		"type": "message", "id": s.id + "-msg-" + itoa(idx), "status": "completed", "role": "assistant",
		"content": []map[string]any{{"type": "output_text", "text": done, "annotations": []any{}}},
	}
	s.writeEvent("response.output_item.done", map[string]any{
		"output_index": idx, "item": item,
	})
	s.output = append(s.output, item)
	s.msgOpen = false
}

// closeTool 收尾打开中的 function_call item。
func (s *ResponsesSSE) closeTool() {
	if !s.toolOpen {
		return
	}
	idx := s.outputIndex - 1
	args := s.toolArgs.String()
	if strings.TrimSpace(args) == "" {
		args = "{}"
	}
	s.writeEvent("response.function_call_arguments.done", map[string]any{
		"item_id": s.id + "-fc-" + itoa(idx), "output_index": idx, "arguments": args,
	})
	item := map[string]any{
		"type": "function_call", "id": s.id + "-fc-" + itoa(idx), "call_id": s.toolID,
		"name": s.toolName, "arguments": args, "status": "completed",
	}
	s.writeEvent("response.output_item.done", map[string]any{
		"output_index": idx, "item": item,
	})
	s.output = append(s.output, item)
	s.toolOpen = false
}

// thinkingDelta 把统一模型的 thinking 增量编码为 reasoning item。
//
// 修复前这里没有 EventThinkingDelta 分支，Responses 流式下的思考内容
// 整个不出事件。可读文本丢失只是体验问题，真正的损失是 sealed 载荷：
// 它是 OpenAI 要求原样回传的续接状态，客户端多轮 reasoning 会话要靠它。
//
// 事件按 OpenAI 规范走，与 message / function_call 两条支路同构：
//
//	response.output_item.added(type=reasoning)
//	  → response.reasoning_summary_text.delta*   （有可读文本时）
//	→ response.output_item.done(带 encrypted_content)
//
// 只在 SealedBy == "openai-responses" 时发。别的协议族封的载荷不该
// 出现在 Responses 事件里 —— 那是别家的不透明状态，写出来既无意义，
// 也可能把跨协议的内部形状泄漏给客户端。
func (s *ResponsesSSE) thinkingDelta(ev *rosetta.Event) {
	if ev.SealedBy != "" && ev.SealedBy != "openai-responses" {
		return
	}
	// 没有 sealed 载荷的裸思考增量仍可显示，但没有续接价值。
	// 上游是 OpenAI 时它总会带 sealed，这里兜住别家实现。
	if ev.Sealed == "" && ev.Text == "" {
		return
	}

	s.ensureStarted("")
	idx := s.outputIndex
	s.outputIndex++

	// encrypted_content 只在 output_item.done 出现（OpenAI 不在任何
	// *.delta 事件里发它），所以这里不发 reasoning_summary_part.* 的
	// added/done 配对 —— 规范的流式形状里 reasoning item 不是分块容器。
	item := map[string]any{
		"type":   "reasoning",
		"status": "in_progress",
	}
	if ev.ReasoningID != "" {
		item["id"] = ev.ReasoningID
	}
	s.writeEvent("response.output_item.added", map[string]any{
		"output_index": idx, "item": item,
	})

	if ev.Text != "" {
		s.thinkText.WriteString(ev.Text)
		s.wroteContent = true
		s.writeEvent("response.reasoning_summary_text.delta", map[string]any{
			"item_id": ev.ReasoningID, "output_index": idx,
			"summary_index": 0, "delta": ev.Text,
		})
	}

	done := map[string]any{
		"type":   "reasoning",
		"status": "completed",
	}
	if ev.ReasoningID != "" {
		done["id"] = ev.ReasoningID
	}
	if ev.Text != "" {
		done["summary"] = []map[string]any{{
			"type": "summary_text", "text": s.thinkText.String(),
		}}
	}
	if ev.Sealed != "" {
		done["encrypted_content"] = ev.Sealed
	}
	s.writeEvent("response.output_item.done", map[string]any{
		"output_index": idx, "item": done,
	})
	s.output = append(s.output, done)
}

func (s *ResponsesSSE) Event(ev *rosetta.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch ev.Type {
	case rosetta.EventMessageStart:
		s.ensureStarted(ev.ID)
	case rosetta.EventThinkingDelta:
		s.thinkingDelta(ev)
	case rosetta.EventTextDelta:
		if ev.Text == "" {
			return
		}
		s.ensureStarted("")
		if !s.msgOpen {
			s.closeTool()
			idx := s.outputIndex
			s.outputIndex++
			itemID := s.id + "-msg-" + itoa(idx)
			s.writeEvent("response.output_item.added", map[string]any{
				"output_index": idx,
				"item":         map[string]any{"type": "message", "id": itemID, "status": "in_progress", "role": "assistant", "content": []any{}},
			})
			s.writeEvent("response.content_part.added", map[string]any{
				"item_id": itemID, "output_index": idx, "content_index": 0,
				"part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}},
			})
			s.msgOpen = true
		}
		s.wroteContent = true
		s.text.WriteString(ev.Text)
		s.writeEvent("response.output_text.delta", map[string]any{
			"item_id": s.id + "-msg-" + itoa(s.outputIndex-1), "output_index": s.outputIndex - 1,
			"content_index": 0, "delta": ev.Text,
		})
	case rosetta.EventToolCall:
		s.ensureStarted("")
		if !s.toolOpen || s.toolIdx != ev.ToolIndex {
			s.closeMessage()
			s.closeTool()
			idx := s.outputIndex
			s.outputIndex++
			s.toolOpen = true
			s.toolIdx = ev.ToolIndex
			// 首片带 call_id/name；续片两者皆空，沿用上一次的值。
			if ev.ToolID != "" {
				s.toolID = ev.ToolID
				s.toolName = ev.ToolName
			}
			s.toolArgs.Reset()
			itemID := s.id + "-fc-" + itoa(idx)
			s.writeEvent("response.output_item.added", map[string]any{
				"output_index": idx,
				"item": map[string]any{
					"type": "function_call", "id": itemID, "call_id": ev.ToolID,
					"name": ev.ToolName, "arguments": "", "status": "in_progress",
				},
			})
		}
		s.wroteContent = true
		if ev.ArgumentsDelta != "" {
			s.toolArgs.WriteString(ev.ArgumentsDelta)
			s.writeEvent("response.function_call_arguments.delta", map[string]any{
				"item_id": s.id + "-fc-" + itoa(s.outputIndex-1), "output_index": s.outputIndex - 1,
				"delta": ev.ArgumentsDelta,
			})
		}
	case rosetta.EventMessageEnd:
		s.sawTerminal = true
	}
}

func (s *ResponsesSSE) Finish(status string, stop rosetta.StopReason, usage rosetta.Usage, _ bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if status == "canceled" {
		return
	}
	s.closeMessage()
	s.closeTool()

	if status != "ok" {
		resp := s.responseSkeleton("failed")
		resp["error"] = map[string]any{"code": "upstream_error", "message": "upstream stream truncated"}
		s.writeEvent("response.failed", map[string]any{"response": resp})
		return
	}

	resp := s.responseSkeleton("completed")
	if len(s.output) > 0 {
		resp["output"] = s.output
	}
	// StopLength 在 Responses 语义里是「未完成」而非失败。
	if stop == rosetta.StopLength {
		resp["status"] = "incomplete"
		resp["incomplete_details"] = map[string]any{"reason": "max_output_tokens"}
	}
	resp["usage"] = map[string]any{
		"input_tokens":  usage.InputTokens,
		"output_tokens": usage.OutputTokens,
		"total_tokens":  usage.TotalTokens,
	}
	s.writeEvent("response.completed", map[string]any{"response": resp})
}

func (s *ResponsesSSE) Keepalive() {
	s.mu.Lock()
	defer s.mu.Unlock()
	fmt.Fprintf(s.w, ": %s\n\n", "keepalive")
	if s.flusher != nil {
		s.flusher.Flush()
	}
}

func (s *ResponsesSSE) WroteContent() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.wroteContent
}

func (s *ResponsesSSE) SawTerminal() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sawTerminal
}

// WriteResponsesResponse 编码 Responses 非流式响应对象。
func WriteResponsesResponse(w http.ResponseWriter, resp *rosetta.ChatResponse, model string) {
	id := resp.ID
	if id == "" {
		id = fmt.Sprintf("resp_%d", time.Now().UnixNano())
	}
	output := make([]map[string]any, 0, len(resp.Content))
	// msgIdx 参与 message item 的 id 生成。旧实现恒用 `id + "-msg"`，
	// 于是「文本 → tool_use → 文本」形态（resp.Content 里有多个 text 块）
	// 会产出**多个 id 完全相同的 message item** —— 下游按 id 索引/去重时
	// 互相覆盖，第二个文本段凭空消失。流式路径用的是 s.outputIndex 递增，
	// 这里必须对称。
	msgIdx := 0
	for _, blk := range resp.Content {
		switch blk.Type {
		case rosetta.BlockThinking:
			// reasoning item 是 reasoning 会话的续接载体，非流式侧同样要出。
			// 修复前这里只处理 BlockText / BlockToolCall，sealed 载荷被
			// 整个丢掉，客户端下一轮回放时报 "Item with id 'rs_…' not found"。
			//
			// SealedBy 门闩：只有 openai-responses 自己封的载荷才写回
			// Responses。chat / anthropic 封的写进来是这个协议的异物。
			if blk.SealedBy != "" && blk.SealedBy != "openai-responses" {
				continue
			}
			item := map[string]any{"type": "reasoning", "status": "completed"}
			if blk.ReasoningID != "" {
				item["id"] = blk.ReasoningID
			}
			if blk.Thinking != "" {
				item["summary"] = []map[string]any{{
					"type": "summary_text", "text": blk.Thinking,
				}}
			}
			if blk.Sealed != "" {
				item["encrypted_content"] = blk.Sealed
			}
			output = append(output, item)
		case rosetta.BlockText:
			output = append(output, map[string]any{
				"type": "message", "id": id + "-msg-" + itoa(msgIdx), "status": "completed", "role": "assistant",
				"content": []map[string]any{{"type": "output_text", "text": blk.Text, "annotations": []any{}}},
			})
			msgIdx++
		case rosetta.BlockToolCall:
			args := blk.Arguments
			if strings.TrimSpace(args) == "" {
				args = "{}"
			}
			output = append(output, map[string]any{
				"type": "function_call", "id": id + "-fc-" + blk.ToolCallID, "call_id": blk.ToolCallID,
				"name": blk.ToolName, "arguments": args, "status": "completed",
			})
		}
	}

	status := "completed"
	var incomplete any
	if resp.StopReason == rosetta.StopLength {
		status = "incomplete"
		incomplete = map[string]any{"reason": "max_output_tokens"}
	}

	usage := map[string]any{
		"input_tokens":  resp.Usage.InputTokens,
		"output_tokens": resp.Usage.OutputTokens,
		"total_tokens":  resp.Usage.TotalTokens,
	}
	// 缓存命中的 input token 必须报出，否则客户端按全价计费，成本偏差一个
	// 数量级。与 OpenAI Chat 非流式侧的 prompt_tokens_details 对齐。
	if resp.Usage.CachedInputTokens > 0 {
		usage["input_tokens_details"] = map[string]any{
			"cached_tokens": resp.Usage.CachedInputTokens,
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"id":                  id,
		"object":              "response",
		"created_at":          time.Now().Unix(),
		"status":              status,
		"model":               model,
		"output":              output,
		"parallel_tool_calls": true,
		"usage":               usage,
		"error":               nil,
		"incomplete_details":  incomplete,
	})
}

func itoa(n int) string {
	return fmt.Sprintf("%d", n)
}
