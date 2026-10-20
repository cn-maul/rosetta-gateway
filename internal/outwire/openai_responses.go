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
	// rw 用于单次写截止时间（可为 nil），见 setWriteDeadline 的注释。
	rw    http.ResponseWriter
	model string
	mu    sync.Mutex

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
	// reasonOpen 标记当前是否有**未收尾**的 reasoning item，reasonIdx 是它的
	// output_index。
	//
	// 这两个字段是必需的，不是优化：思考增量是**多次到达**的（上游每 reasoning
	// chunk 一次），而一个 reasoning item 按规范只能 added→delta*→done 一次。
	// 没有这两个字段时，每个增量都独立开一个 item 又立刻收尾，于是 N 片思考
	// 变成 N 个 reasoning item；更要命的是 thinkText 是跨 item 累积的，于是
	// 第 2 个 item 的 summary 变成「第 1+2 片」、第 3 个变成「第 1+2+3 片」——
	// 客户端（Codex / harness）按 item 渲染思考过程，看到的就是一串逐段变长、
	// 前缀重复的思考块（2026-10-18 用户实测截图）。真正丢失的不是显示效果：
	// 携带 sealed 载荷的那一片会单独成一个 item，其余片的思考内容对模型没有任何
	// 意义，而对话历史里会多出 N-1 条垃圾 item。
	reasonOpen bool
	reasonIdx  int
	// reasonItemID 是这个 reasoning item 的 id。上游给了 rs_ id 就用它
	// （客户端靠它回放寻址），否则网关生成 —— 但必须**记住**并在后续的
	// delta/done 事件里复用同一个值：added 用 A、done 用 B 会让客户端
	// 认不出这是同一个 item。
	reasonItemID string
	// toolIdx 是当前开放 function_call item 对应的 ToolIndex。
	// 同项判据必须是它而不是 toolID：rosetta 的续片（Responses 侧的
	// function_call_arguments.delta）只带 ToolIndex+ArgumentsDelta，ToolID 为空。
	// 用 ToolID 判会把一个工具调用的参数分片拆成多个 item，客户端按 call_id
	// 分发工具执行时拿到的是空参数 + 空call_id 的幽灵项。
	toolIdx  int
	toolArgs strings.Builder
	fcSeq    int
	// seq 是事件序号。Responses 协议里每个事件都带 sequence_number，
	// 客户端（Codex CLI）用它检测丢帧与乱序；本网关此前不发，Codex 侧
	// 只能退化成「按到达顺序处理」，中间丢一个事件也察觉不到。
	// 从 0 开始（response.created 是第一个事件，序号 0），与 OpenAI 一致。
	seq int

	// downgraded 是入站降级过的工具：工具名 → 原生类型（"custom" /
	// "local_shell"）。模型对它们的调用要还原成 custom_tool_call /
	// local_shell_call，并把降级时套上的 JSON 外壳剥掉 ——
	// Codex 拿到一个参数是 {"input":"…"} 的普通 function_call，
	// 找不到自己的执行器。
	downgraded map[string]string

	wroteContent bool
	sawTerminal  bool
	// output 累积已收尾的 output item（response.completed 内嵌完整对象）。
	output []map[string]any
}

func NewResponsesSSE(w io.Writer, flusher http.Flusher, id, model string, downgraded map[string]string) *ResponsesSSE {
	return &ResponsesSSE{w: w, flusher: flusher, id: id, model: model, downgraded: downgraded}
}

// WithResponseWriter 绑定底层 ResponseWriter，用于设置单次写截止时间。
//
// 签名必须**不返回值**：BindWriteDeadline 的能力探测用的是
// `interface{ WithResponseWriter(http.ResponseWriter) }`，返回自身的那版
// 不满足该接口（Go 的方法集按签名精确匹配），于是探测失败、rw 恒为 nil，
// 截止时间整条路径静默失效。
func (s *ResponsesSSE) WithResponseWriter(rw http.ResponseWriter) {
	s.rw = rw
}

// WithDowngradedTools 接收入站降级过的工具表（见 BindDowngradedTools）。
func (s *ResponsesSSE) WithDowngradedTools(tools map[string]string) {
	s.downgraded = tools
}

func (s *ResponsesSSE) writeEvent(event string, payload map[string]any) {
	setWriteDeadline(s.rw)
	defer clearWriteDeadline(s.rw)
	payload["type"] = event
	// 序号在 payload 建好之后、序列化之前分配，且只由 writeEvent 一处
	// 发放 —— 任何绕过它直接写的路径都拿不到序号，也就不会出现重号。
	payload["sequence_number"] = s.seq
	s.seq++
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
	// done 事件与 delta 用同一个判据：custom 走 custom_tool_call_input.done，
	// 且载荷是裸文本而不是 JSON 字符串 —— 与 added / delta 同形，客户端
	// 才能把三段拼成完整输入。
	if s.downgraded[s.toolName] == "custom" {
		s.writeEvent("response.custom_tool_call_input.done", map[string]any{
			"item_id": s.id + "-fc-" + itoa(idx), "output_index": idx,
			"input": unwrapCustomInput(args),
		})
	} else {
		s.writeEvent("response.function_call_arguments.done", map[string]any{
			"item_id": s.id + "-fc-" + itoa(idx), "output_index": idx, "arguments": args,
		})
	}
	item := s.toolCallItem(idx, args)
	s.writeEvent("response.output_item.done", map[string]any{
		"output_index": idx, "item": item,
	})
	s.output = append(s.output, item)
	s.toolOpen = false
}

// toolCallItem 造出当前工具调用的 output item。
//
// 入站被降级过的工具（custom / local_shell）在这里还原成客户端认识的
// 原生形态：Codex 收到 custom_tool_call 时去执行自己的 apply_patch，
// 收到一个名叫 apply_patch 的普通 function_call 则不会执行 ——
// 它按 item 的 type 分派，参数外壳对它也没有意义。
func (s *ResponsesSSE) toolCallItem(idx int, args string) map[string]any {
	itemID := s.id + "-fc-" + itoa(idx)
	switch s.downgraded[s.toolName] {
	case "custom":
		// custom_tool_call 的入参是裸文本，没有 JSON 外壳。
		return map[string]any{
			"type": "custom_tool_call", "id": itemID, "call_id": s.toolID,
			"name": s.toolName, "input": unwrapCustomInput(args), "status": "completed",
		}
	case "local_shell":
		// local_shell_call 的入参是 action 对象：{"type":"exec","command":[…]}。
		return map[string]any{
			"type": "local_shell_call", "id": itemID, "call_id": s.toolID,
			"name": s.toolName, "action": localShellAction(args), "status": "completed",
		}
	default:
		return map[string]any{
			"type": "function_call", "id": itemID, "call_id": s.toolID,
			"name": s.toolName, "arguments": args, "status": "completed",
		}
	}
}

// unwrapCustomInput 从降级后的 {"input":"…"} 参数里取回裸文本。
//
// 形状不是预期时返回原串：模型偶尔会不按 schema 输出（把文本直接当参数、
// 或者参数本身是对象）。原样透传至少让 Codex 的执行器看到模型真正写了
// 什么，比换成空串或 "{}" 更有用。
func unwrapCustomInput(args string) string {
	var obj struct {
		Input string `json:"input"`
	}
	if json.Unmarshal([]byte(args), &obj) != nil || obj.Input == "" {
		return args
	}
	return obj.Input
}

// localShellAction 把降级后的 {"command":[…],"timeout_ms":N} 参数还原成
// local_shell_call 的 action 对象。
//
// 补 "type":"exec"：降级时它被剥掉了（那是 Responses 的判别字段，上游
// 不认），但 Codex 的执行器按它决定怎么跑命令。
func localShellAction(args string) any {
	var obj map[string]any
	if json.Unmarshal([]byte(args), &obj) != nil {
		return map[string]any{"type": "exec"}
	}
	if _, ok := obj["type"]; !ok {
		obj["type"] = "exec"
	}
	return obj
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

	// encrypted_content 只在 output_item.done 出现（OpenAI 不在任何
	// *.delta 事件里发它），所以这里不发 reasoning_summary_part.* 的
	// added/done 配对 —— 规范的流式形状里 reasoning item 不是分块容器。
	//
	// item 的生命周期因此横跨多片增量：**首片**才 added，**末片**才 done。
	// 提前 done 会让每个增量各成一个 item（见 reasonOpen 的注释）。
	if !s.reasonOpen {
		s.ensureStarted("")
		// 思考 item 必须在正文/工具 item 之前收尾：Responses 的 output_index
		// 是严格递增的分配序，不能在开着的 item 中间插一个新 item。
		s.closeMessage()
		s.closeTool()
		s.reasonOpen = true
		s.reasonIdx = s.outputIndex
		s.outputIndex++
		s.thinkText.Reset()
		if ev.ReasoningID != "" {
			s.reasonItemID = ev.ReasoningID
		} else {
			s.reasonItemID = s.id + "-reason-" + itoa(s.reasonIdx)
		}
		s.writeEvent("response.output_item.added", map[string]any{
			"output_index": s.reasonIdx,
			"item":         map[string]any{"type": "reasoning", "status": "in_progress", "id": s.reasonItemID},
		})
	}

	if ev.Text != "" {
		s.thinkText.WriteString(ev.Text)
		s.wroteContent = true
		s.writeEvent("response.reasoning_summary_text.delta", map[string]any{
			"item_id": s.reasonItemID, "output_index": s.reasonIdx,
			"summary_index": 0, "delta": ev.Text,
		})
	}

	// 末片判定：sealed 载荷只能落在一个 item 上（它是一整块不透明状态，
	// 拆到多个 item 里客户端只能拼第一个、其余丢弃），裸文本增量则等到
	// 「不再有思考」时由 closeReason 收尾 —— 不能每片都收，否则又回到
	// 一片一个 item。
	if ev.Sealed == "" {
		return
	}
	s.closeReason(ev)
}

// closeReason 收尾打开中的 reasoning item（output_item.done）。
// done 事件只发一次（reasonOpen 是幂等闩）。
func (s *ResponsesSSE) closeReason(ev *rosetta.Event) {
	if !s.reasonOpen {
		return
	}
	s.reasonOpen = false
	done := map[string]any{
		"type":   "reasoning",
		"status": "completed",
		"id":     s.reasonItemID,
		"summary": func() []map[string]any {
			if s.thinkText.Len() == 0 {
				return []map[string]any{}
			}
			return []map[string]any{{
				"type": "summary_text", "text": s.thinkText.String(),
			}}
		}(),
	}
	if ev != nil && ev.Sealed != "" {
		done["encrypted_content"] = ev.Sealed
	}
	s.writeEvent("response.output_item.done", map[string]any{
		"output_index": s.reasonIdx, "item": done,
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
		// output_index 是严格递增的分配序：思考 item 还没 done 就开新的
		// message item，客户端会看到交错的半开 item。
		s.closeReason(nil)
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
		s.closeReason(nil)
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
			s.writeEvent("response.output_item.added", map[string]any{
				"output_index": idx,
				"item":         s.toolCallItem(idx, ""),
			})
		}
		s.wroteContent = true
		if ev.ArgumentsDelta != "" {
			s.toolArgs.WriteString(ev.ArgumentsDelta)
			// custom 工具的入参是自由文本，Codex 听的是
			// custom_tool_call_input.delta；发成 function_call_arguments.delta
			// 等于把带 JSON 外壳的增量喂给一个按 type 分派的执行器。
			// local_shell 与普通 function 共用 arguments.delta：它的 action
			// 对象本身就是 JSON，拆成 custom 那样的裸文本事件反而对不上
			// Codex 侧的 action 累积逻辑。
			event := "response.function_call_arguments.delta"
			if s.downgraded[s.toolName] == "custom" {
				event = "response.custom_tool_call_input.delta"
			}
			s.writeEvent(event, map[string]any{
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
		// canceled 不写任何东西，收尾 item 也一样（与 message/tool 同纪律）。
		return
	}
	// 思考全程没有 sealed 载荷时，reasoning item 要在这里才收尾 ——
	// 否则一次纯文本思考响应会漏掉 output_item.done，客户端的 item 序列
	// 悬在半开状态。
	s.closeReason(nil)
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
	setWriteDeadline(s.rw)
	defer clearWriteDeadline(s.rw)
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
	WriteResponsesResponseWithTools(w, resp, model, nil)
}

// WriteResponsesResponseWithTools 是 WriteResponsesResponse 的带降级表版本：
// downgraded 把降级过的工具名映射回原生类型，输出的 item 相应地写成
// custom_tool_call / local_shell_call 而非普通 function_call。
func WriteResponsesResponseWithTools(w http.ResponseWriter, resp *rosetta.ChatResponse, model string, downgraded map[string]string) {
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
			itemID := id + "-fc-" + blk.ToolCallID
			switch downgraded[blk.ToolName] {
			case "custom":
				output = append(output, map[string]any{
					"type": "custom_tool_call", "id": itemID, "call_id": blk.ToolCallID,
					"name": blk.ToolName, "input": unwrapCustomInput(args), "status": "completed",
				})
			case "local_shell":
				output = append(output, map[string]any{
					"type": "local_shell_call", "id": itemID, "call_id": blk.ToolCallID,
					"name": blk.ToolName, "action": localShellAction(args), "status": "completed",
				})
			default:
				output = append(output, map[string]any{
					"type": "function_call", "id": itemID, "call_id": blk.ToolCallID,
					"name": blk.ToolName, "arguments": args, "status": "completed",
				})
			}
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
