package outwire

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
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
	// CompletionTokensDetails 承载思考 token 数（OpenAI 官方口径
	// completion_tokens_details.reasoning_tokens）。
	//
	// 缺了它的后果与缺 prompt_tokens_details 同样严重，只是方向相反：
	// 客户端把全部 completion_tokens 当成「回答 token」，于是思考型
	// 模型的成本统计与上下文占用评估都系统性失真
	// （2026-10-21 修 A3-P1-6 / 汇总 O6）。
	CompletionTokensDetails *CompletionTokensDetails `json:"completion_tokens_details,omitempty"`
}

type PromptTokensDetails struct {
	CachedTokens int64 `json:"cached_tokens"`
}

type CompletionTokensDetails struct {
	ReasoningTokens int64 `json:"reasoning_tokens"`
}

type SSEWriter struct {
	w       io.Writer
	flusher http.Flusher
	// rw 是底层 ResponseWriter 的引用（可为 nil，单测直接传 bytes.Buffer 时）。
	// 每次写前用它设单次写截止时间，见 setWriteDeadline 的注释。
	rw      http.ResponseWriter
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
	// writeErr 是**第一次**写失败的原因（2026-10-21 修 *X12）。
	// 见 write_error.go 的 WriteErrorReporter。
	writeErr error

	// envelope 是「除 choices/usage 之外的那段固定前缀」，预编码一次复用
	// （2026-10-22 修 R9）。
	//
	// # 为什么值得这么做
	//
	// 旧实现的每个事件都要构造一个 map（5 个键，含一个 []map[string]any
	// 的 choices）、把 delta 也做成 map 塞进去，再整体 json.Marshal。
	// 一条长回答动辄几百上千个分片，**每个分片**都重新编码一段完全相同的
	// id/object/created/model —— 那部分逐字节一模一样，却付了完整的
	// 反射编码代价，还带着 map 与 interface 的分配。
	//
	// 现在固定前缀只编码一次，每个事件用 strconv.AppendQuote 序列化变化的
	// 字符串、用 json.Marshal 序列化变化的少量结构，直接拼进一个复用缓冲。
	//
	// # 为什么不是手写整条 JSON
	//
	// delta 里的 content / arguments 是**模型输出**，必须走正确的转义
	// （引号、反斜杠、控制字符、非法 UTF-8）。手拼字符串会在这些字符上
	// 出错，而那种错误只在特定输出下才出现 —— 用 AppendQuote 与 Marshal
	// 保证与旧实现逐字节同语义。
	envelope []byte
	// buf 是复用的拼接缓冲：高负载下每个事件都 make 一次会把 GC 压力
	// 直接叠到热路径上。
	buf []byte
	// frame 是复用的「data: …\n\n」帧缓冲，见 writeEventLocked。
	frame []byte
}

func NewSSEWriter(w io.Writer, flusher http.Flusher, id, model string, created int64) *SSEWriter {
	sw := &SSEWriter{w: w, flusher: flusher, id: id, model: model, created: created}
	sw.rebuildEnvelope()
	return sw
}

// rebuildEnvelope 重新编码「choices 之前」的那段固定前缀。
//
// 调用时机有约束：id 在流中途可被 SetResponseID 覆盖（上游 message_start
// 带回真实响应 id 时）。那种覆盖发生在首片之前，所以重建一次即可 ——
// 一旦 roleSent，说明已经有分片带着旧 id 写出去了，此时不该再改。
func (sw *SSEWriter) rebuildEnvelope() {
	sw.envelope = sw.envelope[:0]
	sw.envelope = append(sw.envelope, `{"id":`...)
	sw.envelope = appendJSONString(sw.envelope, sw.id)
	sw.envelope = append(sw.envelope, `,"object":"chat.completion.chunk","created":`...)
	sw.envelope = strconv.AppendInt(sw.envelope, sw.created, 10)
	sw.envelope = append(sw.envelope, `,"model":`...)
	sw.envelope = appendJSONString(sw.envelope, sw.model)
	sw.envelope = append(sw.envelope, `,"choices":[`...)
}

// WithResponseWriter 绑定底层 ResponseWriter，用于设置单次写截止时间。
// 不改变原有构造签名（多处调用点与测试在用），由 codec.NewSink 额外调用。
//
// 签名必须**不返回值**：BindWriteDeadline 的能力探测用的是
// `interface{ WithResponseWriter(http.ResponseWriter) }`，返回自身的那版
// 不满足该接口（Go 的方法集按签名精确匹配），于是探测失败、rw 恒为 nil，
// 截止时间整条路径静默失效。
func (sw *SSEWriter) WithResponseWriter(rw http.ResponseWriter) {
	sw.rw = rw
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
	// 信封里嵌着 id，改完必须重建，否则后续分片仍带旧 id。
	sw.rebuildEnvelope()
}

func (sw *SSEWriter) WriteEvent(event, data string) error {
	sw.mu.Lock()
	defer sw.mu.Unlock()
	return sw.writeEventLocked(event, []byte(data))
}

// writeEventLocked 在**已持锁**的前提下写一个 SSE 事件。
//
// 供 writeChoiceChunk / WriteUsage 这类已经在锁内拼好 payload 的路径使用：
// 它们不能调 WriteEvent（会二次加锁 → 死锁），而重复一遍写逻辑又会把
// 「写失败要记 writeErr」这条纪律再抄一份。
func (sw *SSEWriter) writeEventLocked(event string, data []byte) error {
	setWriteDeadline(sw.rw)
	defer clearWriteDeadline(sw.rw)
	if event != "" {
		if _, err := fmt.Fprintf(sw.w, "event: %s\n", event); err != nil {
			sw.noteWriteErr(err)
			return err
		}
	}
	// 一次性写出「data: <payload>\n\n」：三条 Fprintf 会各自走一次
	// 格式化与反射，而分片写还可能被中间代理观察成半行。
	sw.frame = append(sw.frame[:0], "data: "...)
	sw.frame = append(sw.frame, data...)
	sw.frame = append(sw.frame, '\n', '\n')
	if _, err := sw.w.Write(sw.frame); err != nil {
		sw.noteWriteErr(err)
		return err
	}
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
	setWriteDeadline(sw.rw)
	defer clearWriteDeadline(sw.rw)
	if _, err := fmt.Fprintf(sw.w, ": %s\n\n", text); err != nil {
		sw.noteWriteErr(err)
		return err
	}
	if sw.flusher != nil {
		sw.flusher.Flush()
	}
	return nil
}

// noteWriteErr 记下第一次写失败（后续的失败不覆盖第一次的原因：
// 第一次才是根因，后面的多半是它的连锁反应）。调用方持有 sw.mu。
func (sw *SSEWriter) noteWriteErr(err error) {
	if sw.writeErr == nil && err != nil {
		sw.writeErr = err
	}
}

// WriteError 见 WriteErrorReporter（2026-10-21 修 *X12）。
func (sw *SSEWriter) WriteError() error {
	sw.mu.Lock()
	defer sw.mu.Unlock()
	return sw.writeErr
}

// writeChunk 组装一个标准的 chat.completion.chunk 信封（含 id/object/created/model），
// 严格客户端依赖这些字段与末尾的 finish_reason 分片。
func (sw *SSEWriter) writeChunk(delta map[string]any, finish *string) error {
	deltaJSON, err := json.Marshal(delta)
	if err != nil {
		// delta 只含字符串/数字/切片，编码失败属于程序错误；退回旧路径
		// 让它以同样的方式失败，避免在这里静默吞掉一个事件。
		return err
	}
	return sw.writeChoiceChunk(deltaJSON, finish)
}

// writeChoiceChunk 用**已序列化的 delta** 拼出一个 chat.completion.chunk。
//
// 信封（id/object/created/model）与 choices 的骨架都是常量，只有 delta
// 与 finish_reason 变化 —— 这正是把 map 逐事件重建省掉的地方。
func (sw *SSEWriter) writeChoiceChunk(deltaJSON []byte, finish *string) error {
	sw.mu.Lock()
	defer sw.mu.Unlock()

	sw.buf = append(sw.buf[:0], sw.envelope...)
	sw.buf = append(sw.buf, `{"index":0,"delta":`...)
	sw.buf = append(sw.buf, deltaJSON...)
	sw.buf = append(sw.buf, `,"finish_reason":`...)
	if finish == nil {
		sw.buf = append(sw.buf, "null"...)
	} else {
		sw.buf = appendJSONString(sw.buf, *finish)
	}
	// 收口：close choice 对象 → close choices 数组 → close chunk 对象。
	// envelope 已经打开了 chunk 与 choices，这里不能再多一个 '}'。
	sw.buf = append(sw.buf, '}', ']', '}')
	return sw.writeEventLocked("", sw.buf)
}

// appendJSONString 把 s 作为 JSON 字符串字面量追加到 dst。
//
// 用 strconv.AppendQuote 而不是 fmt：它直接处理转义（引号、反斜杠、
// 控制字符），且不像 fmt 那样走反射 —— 热路径上每个事件都要用。
// encoding/json 对字符串的转义规则与 strconv 的实现一致（都会转义
// <、>、& 吗？见下方说明）。
//
// ⚠️ 两者对 U+2028/U+2029 与 HTML 敏感字符的处理**不同**：
// strconv 不转义 `<> &` 与 U+2028/U+2029，而 encoding/json 会。
// 对 SSE 的 data 段而言，这两者的产物都是合法 JSON，客户端解析后得到
// 同一个字符串 —— 差别只在字节层面，不影响语义。为保持与旧实现
// **逐字节一致**，凡是用 AppendQuote 拼接的位置都只用于
// id/model/finish_reason 这类受控枚举值（不含用户输入）。模型输出
// （content/arguments）一律走 json.Marshal。
func appendJSONString(dst []byte, s string) []byte {
	return strconv.AppendQuote(dst, s)
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
// reasoning_content，rosetta 的 chat 适配器也按这个键回读
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
// # 关于 function 字段（2026-10 修复的 opencode 兼容性缺陷）
//
// 原实现是「name 与 arguments 都为空就整个省略 function」：
//
//	if len(fn) > 0 { td["function"] = fn }
//
// 于是上游吐一个「只带 index 的空拍」时，网关发出 `{"index":0}`，
// opencode（其 AI SDK 的 zod schema 比 OpenAI 官方规范更严格）整条判非法：
//
//	invalid_type at choices[0].delta.tool_calls[0].function
//	Invalid input: expected object, received undefined
//
// 实测复现（Hy4 / OpenRouter 会吐这种空拍）：网关产出的分片与报错里那一条
// 逐字节一致。
//
// ⚠️ 一处必须纠正的认知：OpenAI 官方规范里 function 其实是**可选**的。
// 官方 SDK 类型（由 openai-openapi 生成）写的是 `function?: ToolCall.Function`，
// 所以原实现并不「违反 OpenAI 规范」—— 它只是过不了 AI SDK 那套更严格的
// schema。修正后恒发 function 对**两种**客户端都成立（可选字段给了值，
// 合法；严格 schema 也满足），所以这个方向没有副作用。
//
// # 为什么「恒发 function」对其它客户端也无副作用（已实测）
//
// 用 AI SDK 的**原样 zod schema**（vercel/ai 的 openaiChatChunkSchema，
// 逐字取自 packages/openai/src/chat/chat-api.ts）实测四种产出：
//
//	OpenAI 原生流 / 本修复的产出   → 全部通过
//	修复前（无 function）          → 拒绝，报 expected object, received undefined
//
// 对「存在判断」式合并的影响也一并量化过（三种策略 × 四种产出）：
// 本修复的产出与 OpenAI 原生基准**结果完全一致**（name 与 arguments
// 逐字同构），即对第三方客户端是零行为差异。
//
// 内部字段的填法见下方实现处的注释，那里有个真实的覆盖隐患。
func (sw *SSEWriter) WriteToolCallDelta(index int, id, name, argsDelta string) error {
	if err := sw.ensureRole(); err != nil {
		return err
	}
	td := map[string]any{"index": index}
	if id != "" {
		td["id"] = id
		td["type"] = "function"
	}
	// function 恒存在：缺失即触发严格客户端的 schema 校验失败（见函数注释）。
	//
	// 但**内部字段**要严格照 OpenAI 自己的线格式来，不能图省事填空串：
	//   - name      ：只在非空时出现。真实 OpenAI 的续片根本不带这个键。
	//   - arguments ：恒出现（空串表示「这一拍没有新增参数」）。
	//
	// 为什么不能把 name 也恒填成 ""：下游合并增量的常见写法有两种
	//
	//	if (fn.name) { ... }               // 真值判断 → 空串安全
	//	if (fn.name !== undefined) { ... } // 存在判断 → 空串会被当成新值覆盖进去
	//
	// 第二种写法在真实 OpenAI 流上是安全的（续片压根没有 name 键），
	// 但 name:"" 是「有定义的空串」，会被它覆盖到已收到的真实工具名上 ——
	// 于是工具名丢失、工具调用失效。这个隐患是填空串**引入**的，
	// 照 OpenAI 的格式省略该键则两种写法都安全。
	fn := map[string]any{"arguments": argsDelta}
	if name != "" {
		fn["name"] = name
	}
	td["function"] = fn
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
	// 思考 token 走 completion_tokens_details.reasoning_tokens
	// （2026-10-21 修 A3-P1-6 / 汇总 O6）。
	//
	// DeepSeek / OpenRouter 一系的客户端按这个字段把「思考 token」与
	// 「回答 token」分开计价与展示。缺了它，客户端只能把全部
	// completion_tokens 当成回答 token —— 思考型模型的成本统计与
	// 上下文占用评估都系统性失真。
	if u.ReasoningTokens > 0 {
		usage["completion_tokens_details"] = map[string]any{
			"reasoning_tokens": u.ReasoningTokens,
		}
	}
	usageJSON, err := json.Marshal(usage)
	if err != nil {
		return err
	}

	sw.mu.Lock()
	defer sw.mu.Unlock()
	// 与 writeChoiceChunk 同一套拼法：信封前缀复用，只是这里是
	// "choices":[],"usage":{...} —— usage 分片在 finish 之后单独发。
	sw.buf = append(sw.buf[:0], sw.envelope...)
	sw.buf = append(sw.buf, `],"usage":`...)
	sw.buf = append(sw.buf, usageJSON...)
	sw.buf = append(sw.buf, '}')
	return sw.writeEventLocked("", sw.buf)
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

func WriteNonStreamResponse(w http.ResponseWriter, resp *rosetta.ChatResponse, model string) NonStreamWriteResult {
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
		// OpenAI permits text alongside tool_calls. Keep non-empty text;
		// use an explicit null only when there is no text, rather than
		// letting omitempty remove the key entirely.
		if len(resp.Text()) == 0 {
			msg.Content = json.RawMessage("null")
		}
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
	// 思考 token 同样要与流式路径对齐（2026-10-21 修 A3-P1-6 / 汇总 O6）。
	if resp.Usage.ReasoningTokens > 0 {
		usage.CompletionTokensDetails = &CompletionTokensDetails{
			ReasoningTokens: resp.Usage.ReasoningTokens,
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

	return encodeNonStreamPayload(w, out)
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
