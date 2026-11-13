package inwire

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/cn-maul/rosetta"

	"github.com/cn-maul/rosetta-gateway/internal/effort"
)

type OpenAIChatRequest struct {
	Model               string          `json:"model"`
	Messages            []OpenAIMessage `json:"messages"`
	MaxTokens           *int            `json:"max_tokens,omitempty"`
	MaxCompletionTokens *int            `json:"max_completion_tokens,omitempty"`
	Temperature         *float64        `json:"temperature,omitempty"`
	TopP                *float64        `json:"top_p,omitempty"`
	// Stop 是 FlexibleStringList 而非 []string：OpenAI 官方允许
	// `"stop": "END"` 或 `"stop": ["END","X"]` 两种形态。声明成 []string 时
	// 传 string 会让 json.Unmarshal 报
	// "cannot unmarshal string into Go struct field Req.stop of type []string"，
	// 而 decode 是全字段反序列化、一处报错整体返回 —— 于是一个完全合法的
	// OpenAI 请求被网关硬 400，且错误消息里是 Go 的结构体字段名，对客户端毫无意义。
	Stop          FlexibleStringList `json:"stop,omitempty"`
	Stream        bool               `json:"stream,omitempty"`
	StreamOptions *StreamOptions     `json:"stream_options,omitempty"`
	Tools         []OpenAITool       `json:"tools,omitempty"`
	ToolChoice    any                `json:"tool_choice,omitempty"`
	// ReasoningEffort 是 o1/GPT-5 系列的思考强度。旧实现根本没有这个字段，
	// encoding/json 静默忽略 → 客户端设了 reasoning_effort:"high" 完全失效，
	// 无日志、无报错，而 rosetta 与上游 OpenAI 侧都完整支持它。
	// 这是最常用的入站协议，丢它的代价最大（用户以为在用推理模式，实际没有）。
	ReasoningEffort *string `json:"reasoning_effort,omitempty"`
	// Thinking 是 DeepSeek / Kimi / Qwen 等 OpenAI 兼容上游的显式思考开关，
	// 形状 {"type":"enabled"|"disabled"}（部分上游还带 budget_tokens）。
	//
	// 它不是 OpenAI 官方字段，但在这类 Chat 方言里被广泛使用。此前结构体
	// 没有声明它，encoding/json 静默丢弃 —— 客户端显式关思考，网关回 200，
	// 上游照常思考，链路上没有任何信号。
	//
	// 保留原始字节而不是再定义一套结构：这个对象各家的扩展字段并不一致，
	// 而目标上游同为 Chat 方言时最正确的做法是原样转发。
	Thinking json.RawMessage `json:"thinking,omitempty"`
	// ThinkingOff 记录「客户端显式要求不思考」。它单独存在是因为 rosetta
	// 的 Thinking 只能表达「请求思考」：nil 是「不发」，上游会按默认开。
	// 显式关闭必须由数据面按目标协议写到 wire 上。
	ThinkingOff bool `json:"-"`

	// Suffix 是 model 尾巴上的一次性覆盖（@thinking:off 等）。
	Suffix ModelSuffix `json:"-"`
	// PromptCacheKey is an explicit cache-affinity hint. It is separate from
	// conversation headers: clients may keep the same conversation while
	// intentionally changing the reusable prefix (and vice versa).
	PromptCacheKey string `json:"prompt_cache_key,omitempty"`

	// RawEffort 是 reasoning_effort 的**原样**取值（规范化前的空白/大小写/别名
	// 都保留），解析失败时为空。
	//
	// 它与 reasoning_effort 分开存而不是每次重算，是因为 ToRosetta 里已经要解析
	// 一次，而数据面在 ToRosetta **之后**还要用原始档位按「目标模型支持的档位
	// 子集」做夹取（见 effort.Clamp）。在那一步若只剩三档的 Effort，minimal
	// 与 low 已被合并，xhigh 与 high 已被合并 —— 夹取就退化成了「在 low/medium/
	// high 里选」，模型只支持 xhigh 的那种配置会被夹到 high 去，也就是本功能
	// 要消灭的那类静默降级。
	RawEffort        string   `json:"-"`
	PresencePenalty  *float64 `json:"presence_penalty,omitempty"`
	FrequencyPenalty *float64 `json:"frequency_penalty,omitempty"`
	// N 是「每个输入生成几个候选」。**已声明但从未转发**（2026-10-14 起
	// 显式拒绝 n>1，见 DecodeOpenAIChatRequest）。
	//
	// 为什么保留这个字段而不是删掉：删了它，客户端传 `{"n":3}` 会在
	// 严格解码下变成「未知字段」错误，或者在宽松解码下被静默忽略 ——
	// 后者正是要消灭的失败模式。留着字段、在 decode 侧明确拒绝，
	// 客户端拿到的是一个可据以改代码的 400（带原因），而不是一个
	// 看起来成功、实际少给了候选的 200。
	N              *int            `json:"n,omitempty"`
	ResponseFormat json.RawMessage `json:"response_format,omitempty"`
	Seed           *int            `json:"seed,omitempty"`
	User           string          `json:"user,omitempty"`
	// UserID 是 DeepSeek 的官方写法（`user_id`）。OpenAI 标准是 `user`，
	// 两边含义相同（业务侧用户标识，用于滥用监测与缓存隔离），但名字不同。
	//
	// DeepSeek 超集同时接受 `user`，所以这里不必二选一：哪个来了用哪个，
	// 两个都来以 `user_id` 为准（它是该厂商文档里的正式字段）。
	UserID            string `json:"user_id,omitempty"`
	ParallelToolCalls *bool  `json:"parallel_tool_calls,omitempty"`
}

// chatThinking 是 Chat 方言 thinking 对象里网关需要理解的部分。
type chatThinking struct {
	Type         string `json:"type"`
	BudgetTokens int    `json:"budget_tokens,omitempty"`
}

// parseChatThinking 解析 thinking 对象；形状不对时 ok=false。
func (r *OpenAIChatRequest) parseChatThinking() (chatThinking, bool) {
	if len(r.Thinking) == 0 {
		return chatThinking{}, false
	}
	var t chatThinking
	if err := json.Unmarshal(r.Thinking, &t); err != nil {
		return chatThinking{}, false
	}
	return t, true
}

// FlexibleStringList 接受 JSON 的 string、array、null 三种形态，统一成
// []string。OpenAI 的 stop / stop_sequences 两个字段都是这个规格。
type FlexibleStringList []string

// UnmarshalJSON 实现 json.Unmarshaler。
func (f *FlexibleStringList) UnmarshalJSON(data []byte) error {
	if len(data) == 0 || string(data) == "null" {
		*f = nil
		return nil
	}
	var single string
	if err := json.Unmarshal(data, &single); err == nil {
		if single == "" {
			*f = nil
		} else {
			*f = []string{single}
		}
		return nil
	}
	var many []string
	if err := json.Unmarshal(data, &many); err != nil {
		return fmt.Errorf("must be a string or an array of strings: %w", err)
	}
	*f = many
	return nil
}

// MarshalJSON 保持数组形态（不做无损往返，够用即可）。
func (f FlexibleStringList) MarshalJSON() ([]byte, error) {
	if f == nil {
		return []byte("null"), nil
	}
	return json.Marshal([]string(f))
}

type StreamOptions struct {
	IncludeUsage *bool `json:"include_usage,omitempty"`
}

type OpenAIMessage struct {
	Role       string           `json:"role"`
	Content    json.RawMessage  `json:"content,omitempty"`
	Name       string           `json:"name,omitempty"`
	ToolCalls  []OpenAIToolCall `json:"tool_calls,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
	// ReasoningContent is DeepSeek's replayable thinking value on the
	// OpenAI-compatible Chat protocol. It is not ordinary display text:
	// thinking-mode upstreams require it back verbatim on assistant turns.
	ReasoningContent string `json:"reasoning_content,omitempty"`
	// Refusal 是模型拒答时的正文（OpenAI 的
	// {"role":"assistant","content":null,"refusal":"..."} 形态）。
	//
	// 不解析它会直接卡死会话：content 为 null → extractText 得空串 →
	// rosetta.Assistant("") → 校验报 "message has no content blocks" → 400。
	// 于是「模型正当拒绝后，客户端换个说法再问一次」永远拿不到回答。
	// rosetta 解码侧自己把 refusal 折成文本块（provider_openai_chat.go），
	// 网关入站侧必须对称处理。
	Refusal *string `json:"refusal,omitempty"`
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

type OpenAITool struct {
	Type     string        `json:"type"`
	Function OpenAIFuncDef `json:"function"`
}

type OpenAIFuncDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// defaultMaxBodyBytes 是未显式指定上限时的兜底（32 MiB，与 config.Defaults 的默认值一致）。
const defaultMaxBodyBytes = 32 * 1024 * 1024

// DecodeOpenAIChatRequest 读取并解析请求体。
//
// maxBytes <= 0 时用内置兜底。调用方应当传入 cfg.Defaults.MaxRequestBodyBytes，
// 与外层 RequestSizeLimit 中间件保持同一来源。
//
// 超限必须以 *http.MaxBytesError 报出（调用方据此映射 413），因此这里读
// maxBytes+1 再显式判长度：LimitReader 到点即停、不报错，只读 maxBytes 会让
// 超限 body 被静默截断、json 报出「unexpected end of JSON input」（400），
// 调用方的 413 分支永远走不到。外层 MaxBytesReader 在读越限时也会先行报出
// 同类错误，两条路汇到同一个 413。
func DecodeOpenAIChatRequest(r *http.Request, maxBytes int64) (*OpenAIChatRequest, error) {
	if maxBytes <= 0 {
		maxBytes = defaultMaxBodyBytes
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	if int64(len(body)) > maxBytes {
		return nil, &http.MaxBytesError{Limit: maxBytes}
	}

	var req OpenAIChatRequest
	if err := decodeTopLevelObject(body, &req); err != nil {
		return nil, fmt.Errorf("decode body: %w", err)
	}

	if req.Model == "" {
		return nil, fmt.Errorf("model is required")
	}
	if len(req.Messages) == 0 {
		return nil, fmt.Errorf("messages is required")
	}
	// n > 1 一律拒绝（2026-10-14，交接文档 §7.2 第 1 条）。
	//
	// # 为什么必须 400 而不是静默降级
	//
	// rosetta 与三个上游协议都不支持多候选，ToRosetta 也从不读这个字段。
	// 于是客户端传 `{"n":3}` 时会拿到一个 200 响应、里面只有 1 个候选 ——
	// 请求被静默打了对折，且**没有任何信号**指出发生过这件事。
	//
	// 这类「成功但结果不对」比报错难查得多：调用方看到的是模型行为，
	// 不会想到去核对 choices 数组的长度。显式 400 让它在开发阶段
	// 就暴露，而不是在生产里变成一个长期的错误结论。
	//
	// # 边界
	//
	//   - n == 1：合法且是 OpenAI 的默认，必须放行。
	//   - n == 0：OpenAI 视同未指定（不是「要 0 个候选」），放行。
	//     拒绝它会破坏按「0 = 不设」生成请求体的客户端。
	//   - n < 0：不合法，一并拒绝 —— 与 n>1 同一个原因（无意义的取值
	//     不该被静默接受），且响应里要说清楚是 n 的问题。
	if req.N != nil {
		if *req.N > 1 {
			return nil, fmt.Errorf("n=%d is not supported: this gateway returns exactly 1 choice per request; set n=1 or omit it", *req.N)
		}
		if *req.N < 0 {
			return nil, fmt.Errorf("n=%d is invalid: must be >= 0", *req.N)
		}
	}

	// thinking 是已知的高风险字段：不校验就转发，非法值会被上游或更晚的
	// 链路吞掉，客户端只能看到 200。这里与 n 同一口径 —— 显式 400 让问题
	// 在开发阶段暴露。
	if t, ok := req.parseChatThinking(); ok {
		switch t.Type {
		case "enabled":
			if t.BudgetTokens < 0 {
				return nil, fmt.Errorf("thinking.budget_tokens: must be >= 0")
			}
		case "disabled":
			req.ThinkingOff = true
		default:
			return nil, fmt.Errorf("thinking.type: must be enabled|disabled, got %q", t.Type)
		}
	} else if len(req.Thinking) > 0 {
		return nil, fmt.Errorf("thinking: must be a JSON object with a type field")
	}

	if req.ReasoningEffort != nil {
		req.RawEffort = strings.TrimSpace(*req.ReasoningEffort)
	}
	// reasoning_effort:none 与 thinking.disabled 是同一意图：必须真正关掉，
	// 不能因为 rosetta 没有「关闭」值就静默退回上游默认。
	if req.ReasoningEffort != nil && effort.Parse(req.RawEffort) == effort.None {
		req.ThinkingOff = true
	}

	// model 后缀最后应用：它优先于请求体字段，且必须在校验之后，
	// 否则后缀写出的 thinking 对象还没走过上面的类型校验。
	baseModel, sfx, err := ParseModelSuffix(req.Model)
	if err != nil {
		return nil, err
	}
	req.Model = baseModel
	req.Suffix = sfx
	if sfx.Set {
		switch {
		case sfx.ThinkingOff:
			req.Thinking = nil
			req.ThinkingOff = true
			req.RawEffort = "none"
			req.ReasoningEffort = &req.RawEffort
		case sfx.ThinkingOn:
			req.ThinkingOff = false
			req.Thinking = nil
			// Chat 协议没有统一的预算字段：预算按区间折成档位，
			// 与 rosetta 的 effortFromBudget 同一口径。
			if sfx.Level != effort.Unset && sfx.Level != effort.None {
				req.RawEffort = string(sfx.Level)
				req.ReasoningEffort = &req.RawEffort
			} else if req.RawEffort == "" || effort.Parse(req.RawEffort) == effort.None {
				// 只写 @thinking:on / @thinking:adaptive：Chat 协议没有
				// 「开」这个布尔位，唯一能打开思考的就是给一个档位。
				// medium 与 rosetta 在「请求思考但未指定档位」时的兜底一致。
				req.RawEffort = "medium"
				req.ReasoningEffort = &req.RawEffort
			}
			if sfx.Budget > 0 {
				req.Thinking = json.RawMessage(fmt.Sprintf(
					`{"type":"enabled","budget_tokens":%d}`, sfx.Budget))
			}
		}
	}
	return &req, nil
}

func (r *OpenAIChatRequest) ToRosetta() *rosetta.ChatRequest {
	msgs := make([]rosetta.Message, 0, len(r.Messages))
	for _, m := range r.Messages {
		msg := convertMessage(m)
		msgs = append(msgs, msg)
	}

	user := r.User
	if r.UserID != "" {
		user = r.UserID
	}
	req := &rosetta.ChatRequest{
		Model:             r.Model,
		Messages:          msgs,
		MaxOutputTokens:   maxTokens(r),
		Temperature:       r.Temperature,
		TopP:              r.TopP,
		StopSequences:     r.Stop,
		Seed:              r.Seed,
		User:              user,
		PresencePenalty:   r.PresencePenalty,
		FrequencyPenalty:  r.FrequencyPenalty,
		ParallelToolCalls: r.ParallelToolCalls,
		PromptCacheKey:    r.PromptCacheKey,
	}

	// reasoning_effort → Thinking.Effort。rosetta 只认 low/medium/high
	// （request.go 的 validate 会拒其它值），OpenAI 官方也是这三档。
	// 未知值静默忽略而不是报错：客户端可能是按更新的模型能力发的，
	// 直接 400 会让整个请求不可用，而忽略只是退回默认强度。
	//
	// 归一口径由 internal/effort 单一提供（2026-10-21 修 P0-12）：
	// 此前这里是本包自己的一张 switch，它漏掉了 max / very_high / min /
	// mid / default 等写法 —— 漏掉的那些落进 default 分支返回 Unset，
	// 于是「客户端要最高档思考」变成「完全不思考」，且日志无痕。
	// 强度与开关分两步：客户端常把两者一起发（DeepSeek 文档的示例就是
	// thinking:{type:"enabled"} + reasoning_effort:"high"），先按 effort
	// 定强度，再由开关覆盖成「关」。
	if r.ReasoningEffort != nil {
		if lvl, ok := effort.ToThinkingLevel(effort.Parse(*r.ReasoningEffort)); ok {
			req.Thinking = &rosetta.ThinkingConfig{Effort: rosetta.Effort(lvl)}
		}
	}
	if t, ok := r.parseChatThinking(); ok {
		switch t.Type {
		case "disabled":
			// rosetta 的 Thinking 只有「请求思考」一种语义，nil 等于「不发」
			// （上游按默认，DeepSeek 系默认就是开思考），恰好与请求相反。
			// 显式关闭由数据面按目标协议写到 wire 上（见
			// ingress.Request.ApplyProtocolThinking）。
			req.Thinking = nil
		case "enabled":
			// 显式开启：没有 reasoning_effort 时按预算（或 SDK 默认强度）。
			if req.Thinking == nil && t.BudgetTokens > 0 {
				req.Thinking = &rosetta.ThinkingConfig{BudgetTokens: t.BudgetTokens}
			}
		}
	}

	if len(r.Tools) > 0 {
		tools := make([]rosetta.ToolDefinition, 0, len(r.Tools))
		for _, t := range r.Tools {
			tools = append(tools, rosetta.ToolDefinition{
				Name:        t.Function.Name,
				Description: t.Function.Description,
				Parameters:  t.Function.Parameters,
			})
		}
		req.Tools = tools
	}
	if tc := chatToolChoiceToRosetta(r.ToolChoice); tc != nil {
		req.ToolChoice = tc
	}
	if rf := chatResponseFormatToRosetta(r.ResponseFormat); rf != nil {
		req.ResponseFormat = rf
	}

	return req
}

// chatToolChoiceToRosetta 把 chat 的 tool_choice（any：string 或命名函数
// map 两种形态）翻译成 rosetta 的统一 ToolChoice。不认识的形状返回 nil
// （丢弃，与旧实现的处置一致——绝不把 OpenAI 形状原样塞给别的协议）。
func chatToolChoiceToRosetta(v any) *rosetta.ToolChoice {
	switch t := v.(type) {
	case string:
		switch t {
		case "auto":
			return &rosetta.ToolChoice{Mode: rosetta.ToolChoiceAuto}
		case "none":
			return &rosetta.ToolChoice{Mode: rosetta.ToolChoiceNone}
		case "required":
			return &rosetta.ToolChoice{Mode: rosetta.ToolChoiceRequired}
		}
		return nil
	case map[string]any:
		if t["type"] != "function" {
			return nil
		}
		fn, ok := t["function"].(map[string]any)
		if !ok {
			return nil
		}
		name, _ := fn["name"].(string)
		if name == "" {
			return nil
		}
		return &rosetta.ToolChoice{Mode: rosetta.ToolChoiceTool, Name: name}
	}
	return nil
}

// chatResponseFormatToRosetta 把 chat 的 response_format（json.RawMessage）
// 翻译成 rosetta 的统一 ResponseFormat。SDK 按目标协议映射（chat 的
// response_format、Responses 的 text.format、Anthropic 的 output_config）。
// 未建模的字段（如 json_schema 的 description）在翻译中丢弃 —— 这是从
// RawMessage 直传迁到类型化字段的代价，换取的是不再依赖 WithExtraOverrides。
func chatResponseFormatToRosetta(raw json.RawMessage) *rosetta.ResponseFormat {
	if len(raw) == 0 {
		return nil
	}
	var f struct {
		Type       string          `json:"type"`
		JSONSchema json.RawMessage `json:"json_schema"`
	}
	if json.Unmarshal(raw, &f) != nil {
		return nil
	}
	switch f.Type {
	case "text":
		return &rosetta.ResponseFormat{Type: "text"}
	case "json_object":
		return &rosetta.ResponseFormat{Type: "json_object"}
	case "json_schema":
		rf := &rosetta.ResponseFormat{Type: "json_schema"}
		if len(f.JSONSchema) > 0 {
			var js struct {
				Name   string          `json:"name"`
				Schema json.RawMessage `json:"schema"`
				Strict *bool           `json:"strict"`
			}
			if json.Unmarshal(f.JSONSchema, &js) == nil {
				rf.Name = js.Name
				rf.Schema = js.Schema
				rf.Strict = js.Strict
			}
		}
		return rf
	}
	return nil
}

// 本入口不再需要 ApplyUpstreamExtras。
//
// tool_choice / response_format / seed / user / presence_penalty /
// frequency_penalty / parallel_tool_calls / prompt_cache_key 全部在 ToRosetta
// 落到 rosetta.ChatRequest 的类型化字段，由 SDK 按目标协议翻译（v1.3.0 起
// 这些同时是 SDK 保留键，塞 Extra 会撞 openaiChatReservedPayloadKeys）。
//
// top_k 刻意不支持：它不是 OpenAI 官方 Chat 参数（只有 vLLM 一类自建
// 服务认），而入站结构体也没有这个字段。见 DESIGN §5.2.1 的「已知丢弃」。
//
// 曾经独占的 reasoning_content 回放也交还给 SDK 了：v1.3.0 的 chat 编码器
// 把 BlockThinking 编码成同一条 assistant 消息的 reasoning_content（P1-1a），
// 因此这里不再整份接管 messages，也不再依赖 WithExtraOverrides。
//
// n 依然刻意不支持：rosetta 只建模单条 assistant 轮次，n > 1 会让调用方
// 为网关随后丢弃的候选付费。

func maxTokens(r *OpenAIChatRequest) int {
	if r.MaxCompletionTokens != nil {
		return *r.MaxCompletionTokens
	}
	if r.MaxTokens != nil {
		return *r.MaxTokens
	}
	return 0
}

// RequiresStructuredOutput 报告该请求是否带**硬性**结构化输出约束
// （response_format.type 为 json_object / json_schema）。
//
// 为什么需要单独判定：这两类约束在 Anthropic 协议没有对应物，透传等于
// 「200 但约束静默丢失」。网关据此把 anthropic 候选从链上滤掉（全被滤空
// 则 400），而不是放行后让约束悄悄失效 —— 跳过候选比谎报成功更安全
// （见 handleIngress 的过滤逻辑）。response_format 缺失、type 为 "text"
// 或畸形 JSON 时返回 false：前两者本就无约束，畸形体交给上游拒绝。
func (r *OpenAIChatRequest) RequiresStructuredOutput() bool {
	if len(r.ResponseFormat) == 0 {
		return false
	}
	var f struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(r.ResponseFormat, &f) != nil {
		return false
	}
	return f.Type == "json_object" || f.Type == "json_schema"
}

func convertMessage(m OpenAIMessage) rosetta.Message {
	switch m.Role {
	case "system":
		return rosetta.System(extractText(m.Content))
	case "user":
		// 唯一保留多模态的角色：文本 + image_url 一起透传给上游。
		return userMessage(m.Content)
	case "assistant":
		if len(m.ToolCalls) > 0 || m.ReasoningContent != "" {
			blocks := make([]rosetta.Block, 0, len(m.ToolCalls)+2)
			if m.ReasoningContent != "" {
				blocks = append(blocks, rosetta.Thinking(m.ReasoningContent, ""))
			}
			if text := extractText(m.Content); text != "" {
				blocks = append(blocks, rosetta.Block{Type: rosetta.BlockText, Text: text})
			}
			for _, tc := range m.ToolCalls {
				blocks = append(blocks, rosetta.ToolCall(tc.ID, tc.Function.Name, tc.Function.Arguments))
			}
			return rosetta.AssistantBlocks(blocks...)
		}
		return rosetta.Assistant(assistantText(m))
	case "tool":
		return rosetta.ToolResult(m.ToolCallID, "", extractText(m.Content))
	case "developer":
		// o1/GPT-5 系列用 developer 取代 system。降级成 user 会让上游把它当
		// 对话轮次的一部分，指令优先级下降、行为与直连上游不一致。
		return rosetta.System(extractText(m.Content))
	case "function":
		// 老式 function 角色（角色名直接承载函数名）本质是工具结果。
		return rosetta.ToolResult(m.Name, "", extractText(m.Content))
	default:
		// 角色未知时按 user 处理（rosetta 的 validate 随后会拒绝非法角色）。
		return userMessage(m.Content)
	}
}

// assistantText 取 assistant 消息的正文，refusal 作为 content 为空时的兜底。
//
// OpenAI 的拒答形态是 {"role":"assistant","content":null,"refusal":"I can't…"}：
// content 是 null，拒答正文在 refusal 字段。只读 content 会得到空串，而
// rosetta 的校验要求 assistant 至少有一个非空块 —— 于是
// "message has no content blocks" → 400。后果是模型正当拒绝之后，
// 客户端**任何**后续续聊（哪怕只是换个说法）都必然 400，整条会话卡死。
//
// 优先级：content 有正文时用它（refusal 字段此时按 OpenAI 语义也为空），
// 否则回落 refusal。
func assistantText(m OpenAIMessage) string {
	if t := extractText(m.Content); t != "" {
		return t
	}
	if m.Refusal != nil && *m.Refusal != "" {
		return *m.Refusal
	}
	return ""
}

// openAIContentPart 是 OpenAI「内容块数组」里的一块。
//
// image_url.detail 落在 rosetta.Block.ImageDetail 上：它决定上游把图片
// 缩到 512×512（low）还是保留原图（high / original / auto），直接影响
// 识别精度与 token 成本，丢了等于替客户端选了一个它没选的档。
//
// 输入媒体本身（input_audio / file）必须保留，不能再静默丢掉。
type openAIContentPart struct {
	Type     string `json:"type"`
	Text     string `json:"text"`
	ImageURL *struct {
		URL    string `json:"url"`
		Detail string `json:"detail"`
	} `json:"image_url"`
	InputAudio *struct {
		Data   string `json:"data"`
		Format string `json:"format"`
	} `json:"input_audio"`
	File *struct {
		FileData string `json:"file_data"`
		FileID   string `json:"file_id"`
		Filename string `json:"filename"`
	} `json:"file"`
}

// userContent 是一条 Chat 消息里可代理的内容。文本拼成一段，媒体保持
// 输入顺序，供 user 消息和 tool 结果共用。
type userContent struct {
	Text   string
	Blocks []rosetta.Block
	// Dropped 记录识别为媒体、但缺少可代理载荷的分块。它单独存在是为了
	// 让入口能返回 400，而不是把这种块当成“没有内容”继续转发。
	Dropped []string
}

// extractUserContent 拆开一条消息的 content：文本拼成一段，媒体按顺序收集。
//
// 旧实现只抽文本，把 image_url **静默丢掉**：客户端发多模态请求，网关照单全收
// 并回 200，而上游只看到文字 —— 用户以为模型「看不懂图」，实际是网关半路把图删了，
// 链路上不留任何痕迹。rosetta 原生有 BlockImage，跨协议翻译（OpenAI image_url
// ↔ Anthropic image source ↔ Responses input_image）正是这个网关存在的意义。
//
// 非法 URL（file:// 之类）不在这里白名单：rosetta 的 validate 会拦下并返回
// ErrInvalidRequest，网关已有的错误映射把它变成 400。
//
// 都认不出来时不产生内容。旧实现 `return string(raw)` 会把畸形 JSON **原样当正文**
// 发给上游：客户端传个数字或嵌套对象，上游收到一段 JSON 字面量文本，
// 而链路上看不出任何异常（它确实是一段"合法的"文本），排查时只能靠肉眼比对话。
func extractUserContent(raw json.RawMessage) userContent {
	if len(raw) == 0 {
		return userContent{}
	}

	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return userContent{Text: s}
	}

	var parts []openAIContentPart
	if err := json.Unmarshal(raw, &parts); err != nil {
		// 单块的「对象形态」：非标准，但手写请求里很常见，尽力提取。
		var single openAIContentPart
		if err := json.Unmarshal(raw, &single); err != nil {
			return userContent{}
		}
		if single.Type == "image_url" && single.ImageURL != nil && single.ImageURL.URL != "" {
			return userContent{Blocks: []rosetta.Block{{
				Type: rosetta.BlockImage, ImageURL: single.ImageURL.URL,
				ImageDetail: normalizeImageDetail(single.ImageURL.Detail),
			}}}
		}
		if single.Text != "" {
			return userContent{Text: single.Text}
		}
		return chatContentBlock(single)
	}

	var out userContent
	var text strings.Builder
	for _, p := range parts {
		switch p.Type {
		case "text":
			text.WriteString(p.Text)
		case "image_url":
			if p.ImageURL != nil && p.ImageURL.URL != "" {
				out.Blocks = append(out.Blocks, rosetta.Block{
					Type: rosetta.BlockImage, ImageURL: p.ImageURL.URL,
					ImageDetail: normalizeImageDetail(p.ImageURL.Detail),
				})
			} else {
				out.Dropped = append(out.Dropped, p.Type)
			}
		case "input_audio":
			if p.InputAudio != nil && p.InputAudio.Data != "" {
				out.Blocks = append(out.Blocks, rosetta.Block{
					Type:        rosetta.BlockAudio,
					AudioData:   stripDataURLPrefix(p.InputAudio.Data),
					AudioFormat: p.InputAudio.Format,
				})
			} else {
				out.Dropped = append(out.Dropped, p.Type)
			}
		case "file":
			if b, ok := chatFileBlock(p); ok {
				out.Blocks = append(out.Blocks, b)
			} else {
				out.Dropped = append(out.Dropped, p.Type)
			}
		}
	}
	out.Text = text.String()
	return out
}

func chatContentBlock(p openAIContentPart) userContent {
	switch p.Type {
	case "image_url":
		if p.ImageURL != nil && p.ImageURL.URL != "" {
			return userContent{Blocks: []rosetta.Block{{
				Type: rosetta.BlockImage, ImageURL: p.ImageURL.URL,
				ImageDetail: normalizeImageDetail(p.ImageURL.Detail),
			}}}
		}
	case "input_audio":
		if p.InputAudio != nil && p.InputAudio.Data != "" {
			return userContent{Blocks: []rosetta.Block{{
				Type:        rosetta.BlockAudio,
				AudioData:   stripDataURLPrefix(p.InputAudio.Data),
				AudioFormat: p.InputAudio.Format,
			}}}
		}
	case "file":
		if b, ok := chatFileBlock(p); ok {
			return userContent{Blocks: []rosetta.Block{b}}
		}
	default:
		return userContent{}
	}
	return userContent{Dropped: []string{p.Type}}
}

func chatFileBlock(p openAIContentPart) (rosetta.Block, bool) {
	if p.File == nil {
		return rosetta.Block{}, false
	}
	// Chat has no mime_type field in the common file part. The filename lets
	// rosetta derive it where possible.
	b := rosetta.Block{
		Type:     rosetta.BlockFile,
		FileName: p.File.Filename,
	}
	switch {
	case p.File.FileID != "":
		b.FileID = p.File.FileID
	case p.File.FileData != "":
		b.FileData = stripDataURLPrefix(p.File.FileData)
	default:
		return rosetta.Block{}, false
	}
	return b, true
}

func stripDataURLPrefix(data string) string {
	if idx := strings.Index(data, ","); strings.HasPrefix(data, "data:") && idx > 0 {
		return data[idx+1:]
	}
	return data
}

// normalizeImageDetail 把厂商扩展的 detail 收敛到 rosetta 认识的三个值。
//
// DeepSeek 多了一个 `original`（保留原图，不缩放到 512）。rosetta 的校验
// 只认 auto/low/high，原样透传会把一个合法请求打成 400。`original` 与
// `high` 的语义一致（doc：high、original 与 auto 都保留原图），所以映射
// 到 high 而不丢弃 —— 丢弃会让上游按默认档处理，改变客户端的成本选择。
//
// 未知取值同样收敛成空串（交给上游默认），而不是让它 400：
// detail 是锦上添花的提示位，不该成为整条请求的失败点。
func normalizeImageDetail(detail string) string {
	switch strings.ToLower(strings.TrimSpace(detail)) {
	case "auto", "low", "high":
		return strings.ToLower(strings.TrimSpace(detail))
	case "original":
		return "high"
	default:
		return ""
	}
}

// extractText 只要文本，丢弃图像。用于 system / assistant / tool —— 这三个角色的
// 内容在 OpenAI 协议里本就是纯文本，图像只对 user 有意义。
func extractText(raw json.RawMessage) string {
	return extractUserContent(raw).Text
}

// userMessage 组装 user 消息。
//
// 无图像时直接走 rosetta.User，与旧实现逐字节一致（不改变既有行为）；
// 带图像时才构造多块消息。只有在 rosetta 的 validate 允许的角色-块组合内
// （RoleUser 支持 text / image / audio / file）才可能通过。
func userMessage(raw json.RawMessage) rosetta.Message {
	content := extractUserContent(raw)
	if len(content.Blocks) == 0 {
		return rosetta.User(content.Text)
	}
	blocks := make([]rosetta.Block, 0, len(content.Blocks)+1)
	if content.Text != "" {
		blocks = append(blocks, rosetta.Block{Type: rosetta.BlockText, Text: content.Text})
	}
	blocks = append(blocks, content.Blocks...)
	return rosetta.Message{Role: rosetta.RoleUser, Blocks: blocks}
}
