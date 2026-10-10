package inwire

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/cn-maul/rosetta"
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

	// RawEffort 是 reasoning_effort 的**原样**取值（规范化前的空白/大小写/别名
	// 都保留），解析失败时为空。
	//
	// 它与 reasoning_effort 分开存而不是每次重算，是因为 ToRosetta 里已经要解析
	// 一次，而数据面在 ToRosetta **之后**还要用原始档位按「目标模型支持的档位
	// 子集」做夹取（见 effort.Clamp）。在那一步若只剩三档的 Effort，minimal
	// 与 low 已被合并，xhigh 与 high 已被合并 —— 夹取就退化成了「在 low/medium/
	// high 里选」，模型只支持 xhigh 的那种配置会被夹到 high 去，也就是本功能
	// 要消灭的那类静默降级。
	RawEffort         string          `json:"-"`
	PresencePenalty   *float64        `json:"presence_penalty,omitempty"`
	FrequencyPenalty  *float64        `json:"frequency_penalty,omitempty"`
	N                 *int            `json:"n,omitempty"`
	ResponseFormat    json.RawMessage `json:"response_format,omitempty"`
	Seed              *int            `json:"seed,omitempty"`
	User              string          `json:"user,omitempty"`
	ParallelToolCalls *bool           `json:"parallel_tool_calls,omitempty"`
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
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, fmt.Errorf("decode body: %w", err)
	}

	if req.Model == "" {
		return nil, fmt.Errorf("model is required")
	}
	if len(req.Messages) == 0 {
		return nil, fmt.Errorf("messages is required")
	}

	if req.ReasoningEffort != nil {
		req.RawEffort = strings.TrimSpace(*req.ReasoningEffort)
	}
	return &req, nil
}

func (r *OpenAIChatRequest) ToRosetta() *rosetta.ChatRequest {
	msgs := make([]rosetta.Message, 0, len(r.Messages))
	for _, m := range r.Messages {
		msg := convertMessage(m)
		msgs = append(msgs, msg)
	}

	req := &rosetta.ChatRequest{
		Model:           r.Model,
		Messages:        msgs,
		MaxOutputTokens: maxTokens(r),
		Temperature:     r.Temperature,
		TopP:            r.TopP,
		StopSequences:   r.Stop,
	}

	// reasoning_effort → Thinking.Effort。rosetta 只认 low/medium/high
	// （request.go 的 validate 会拒其它值），OpenAI 官方也是这三档。
	// 未知值静默忽略而不是报错：客户端可能是按更新的模型能力发的，
	// 直接 400 会让整个请求不可用，而忽略只是退回默认强度。
	if r.ReasoningEffort != nil {
		if eff := parseEffort(*r.ReasoningEffort); eff != rosetta.EffortUnset {
			req.Thinking = &rosetta.ThinkingConfig{Effort: eff}
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

	return req
}

// ApplyProtocolPrivateExtra 把 OpenAI-Chat 私有请求字段按**目标上游协议**处理：
// 能等价翻译的必须翻译，不能翻译的按「软偏好丢弃 / 硬约束由网关层拒绝」分别处置。
// 此前的实现是「目标为 anthropic / openai-responses 就整个 return」，把
// response_format、tool_choice 等全部静默丢掉 —— 故障转移到不同协议的目标后，
// 同一请求的行为悄然改变，路由不再语义透明（2026-10-07 P1-8）。
//
// 各目标的处置矩阵：
//   - openai-chat / "" / auto：OpenAI 方言原样直传（含未知协议值 —— 池构建期
//     已拒绝拼写错误的协议名，这里保持旧的宽容行为不做二次拦截）。
//   - openai-responses：response_format → text.format（等价约束，反向复用
//     textFormatToResponseFormat 的翻译）、tool_choice 摊平成 Responses 形状、
//     parallel_tool_calls 直传。penalty / seed / user 无对应物：三者是**软偏好**
//     （采样倾向、缓存归并、租户标注），丢弃不改变请求的正确性，注释留痕即可。
//   - anthropic：tool_choice 翻译（auto→{"type":"auto"}、required→{"type":"any"}、
//     命名函数→{"type":"tool"}；"none" 无对应物，见 chatToolChoiceAnthropic）。
//     response_format 是**硬约束**，Anthropic 无对应物 —— 它不允许静默降级，
//     由 ingressRequest.requiresStructuredOutput 在网关层把「只余 anthropic
//     候选」的这类请求整体 400（见 handleIngress），到不了本函数。penalty /
//     seed / user / parallel_tool_calls 同为软偏好或无效开关，丢弃不破坏正确性。
//
// n is deliberately absent. rosetta models a single assistant turn -- the
// unary decoder reads Choices[0] and the streaming decoder skips every
// index != 0 -- so n > 1 would bill the caller for candidates the gateway
// then discards. Dropping it is the safer outcome.
func (r *OpenAIChatRequest) ApplyProtocolPrivateExtra(req *rosetta.ChatRequest, protocol string) {
	switch protocol {
	case "openai-responses":
		// —— 能等价翻译的必须翻译 ——
		extra := map[string]any{}
		// response_format → Responses 的 text.format。json_object 直映、
		// json_schema 摊平外壳，与 textFormatToResponseFormat 互为镜像；
		// "text"/缺省是两协议共同的默认值，无需显式表达。
		if len(r.ResponseFormat) > 0 {
			if tf := responseFormatToTextFormat(r.ResponseFormat); tf != nil {
				extra["text"] = map[string]any{"format": tf}
			}
		}
		// 字符串三档（auto/none/required）两协议同名；命名函数从 chat 的
		// 嵌套 {"type":"function","function":{"name"}} 摊平成 Responses 的
		// {"type":"function","name"}。
		if r.ToolChoice != nil {
			if tc := chatToolChoiceResponses(r.ToolChoice); tc != nil {
				extra["tool_choice"] = tc
			}
		}
		if r.ParallelToolCalls != nil {
			extra["parallel_tool_calls"] = *r.ParallelToolCalls
		}
		// presence_penalty / frequency_penalty / seed / user：Responses API 无对应物。
		// 它们是软偏好或标注类字段，丢弃不改变请求的正确性 —— 与结构化输出这类
		// 硬约束不同（后者在网关层被拒绝，绝不静默降级）。
		if len(extra) > 0 {
			mergeExtra(req, extra)
		}
		return

	case "anthropic":
		// tool_choice 有等价翻译（映射表见 chatToolChoiceAnthropic）。
		if r.ToolChoice != nil {
			if tc := chatToolChoiceAnthropic(r.ToolChoice); tc != nil {
				mergeExtra(req, map[string]any{"tool_choice": tc})
			}
		}
		// response_format 在 Anthropic 无对应物。json_object / json_schema 是
		// **硬约束**：静默丢弃会让客户端拿到「200 但约束失效」的响应，且故障
		// 转移前后行为不一致 —— 这类请求不会走到这里（requiresStructuredOutput
		// 已在 handleIngress 把 anthropic 候选过滤掉，全被滤空则 400）。
		// penalty / seed / user 是软偏好，parallel_tool_calls 是 Anthropic
		// 没有开关表达的控制位 —— 丢弃均不改变请求的正确性。
		return
	}

	// "" / "openai-chat" / "auto"（及未知值）：OpenAI 方言直传。
	extra := map[string]any{}
	if r.ToolChoice != nil {
		extra["tool_choice"] = r.ToolChoice
	}
	if r.PresencePenalty != nil {
		extra["presence_penalty"] = *r.PresencePenalty
	}
	if r.FrequencyPenalty != nil {
		extra["frequency_penalty"] = *r.FrequencyPenalty
	}
	// response_format 保留客户端原始 JSON（{"type":"json_object"} 或含 json_schema
	// 的对象）：这里用 json.RawMessage 直传、不做结构往返，schema 里我们没建模的
	// 字段（strict、schema 名等）才不会被解码环节吃掉。显式 null 也照原样透传。
	if len(r.ResponseFormat) > 0 {
		extra["response_format"] = r.ResponseFormat
	}
	if r.Seed != nil {
		extra["seed"] = *r.Seed
	}
	if r.User != "" {
		extra["user"] = r.User
	}
	if r.ParallelToolCalls != nil {
		extra["parallel_tool_calls"] = *r.ParallelToolCalls
	}
	// reasoning_effort 故意**不**进 Extra，尽管它是 OpenAI 的字段。
	//
	// rosetta 把 reasoning_effort 列在 openaiChatReservedPayloadKeys 里，
	// Extra 一旦带上同名键，mergeExtra 就报 ErrInvalidRequest，而网关
	// buildClient 没有开 WithExtraOverrides —— 于是客户端只要设了这个字段，
	// 每个请求都必然 400，且死在网关自己的 SDK 校验层，一个字节都到不了上游。
	// 此前为了"保住 minimal/xhigh 原始值"而直传，正是这条 400 的来源。
	//
	// 不直传的代价：Thinking.Effort 只归一到 low/medium/high，minimal→low、
	// xhigh→high。语义方向正确（都是"少想一点"/"多想想"），只是粒度变粗。
	// 这个取舍与本函数对未知 effort 值的既有处理一致（静默退回默认而非 400）：
	// 客户端可能按更新的模型能力在发请求，让整个请求不可用才是更糟的失败。
	if len(extra) == 0 {
		return
	}

	if req.Extra == nil {
		req.Extra = extra
		return
	}
	for k, v := range extra {
		req.Extra[k] = v
	}
}

func maxTokens(r *OpenAIChatRequest) int {
	if r.MaxCompletionTokens != nil {
		return *r.MaxCompletionTokens
	}
	if r.MaxTokens != nil {
		return *r.MaxTokens
	}
	return 0
}

// chatToolChoiceAnthropic 把 chat 的 tool_choice 翻译成 Anthropic wire 形状。
//
//	"auto"                → {"type":"auto"}
//	"required"            → {"type":"any"}（Anthropic 的「必须调一个工具」）
//	命名函数（嵌套 function 形状） → {"type":"tool","name":…}
//	"none"                → nil（丢弃）
//
// "none" 丢弃的原因：Anthropic 无法表达「工具照常声明、但本轮禁止调用」——
// 官方语义里最接近的做法是把 tools 列表整个去掉，那是对请求内容的改动而不是
// 翻译，只能放弃该控制位。tool_choice 是 any 类型（decode 未收敛其形状），
// 不认识的形状一律返回 nil 丢弃，绝不把 OpenAI 形状原样发给 Anthropic 上游。
func chatToolChoiceAnthropic(v any) any {
	switch t := v.(type) {
	case string:
		switch t {
		case "auto":
			return map[string]any{"type": "auto"}
		case "required":
			return map[string]any{"type": "any"}
		default: // "none" 等：无对应物，丢弃。
			return nil
		}
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
		return map[string]any{"type": "tool", "name": name}
	}
	return nil
}

// chatToolChoiceResponses 把 chat 的 tool_choice 翻译成 Responses 形状。
// 字符串三档（auto/none/required）两协议同名直取；命名函数只差外壳 ——
// chat 嵌套 {"type":"function","function":{"name"}}，Responses 摊平为
// {"type":"function","name"}。不认识的形状返回 nil 丢弃。
func chatToolChoiceResponses(v any) any {
	switch t := v.(type) {
	case string:
		switch t {
		case "auto", "none", "required":
			return t
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
		return map[string]any{"type": "function", "name": name}
	}
	return nil
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
		if len(m.ToolCalls) > 0 {
			blocks := make([]rosetta.Block, 0, len(m.ToolCalls)+1)
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
// 只声明我们真的会透传的字段：多出来的（image_url.detail / input_audio / file…）
// 由 encoding/json 忽略，不会被误当成正文。
type openAIContentPart struct {
	Type     string `json:"type"`
	Text     string `json:"text"`
	ImageURL *struct {
		URL string `json:"url"`
	} `json:"image_url"`
}

// extractUserContent 拆开一条消息的 content：文本拼成一段，图像 URL 原样收集。
//
// 旧实现只抽文本，把 image_url **静默丢掉**：客户端发多模态请求，网关照单全收
// 并回 200，而上游只看到文字 —— 用户以为模型「看不懂图」，实际是网关半路把图删了，
// 链路上不留任何痕迹。rosetta 原生有 BlockImage，跨协议翻译（OpenAI image_url
// ↔ Anthropic image source ↔ Responses input_image）正是这个网关存在的意义。
//
// 非法 URL（file:// 之类）不在这里白名单：rosetta 的 validate 会拦下并返回
// ErrInvalidRequest，网关已有的错误映射把它变成 400。
//
// 都认不出来时返回空串。旧实现 `return string(raw)` 会把畸形 JSON **原样当正文**
// 发给上游：客户端传个数字或嵌套对象，上游收到一段 JSON 字面量文本，
// 而链路上看不出任何异常（它确实是一段"合法的"文本），排查时只能靠肉眼比对话。
func extractUserContent(raw json.RawMessage) (string, []string) {
	if len(raw) == 0 {
		return "", nil
	}

	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, nil
	}

	var parts []openAIContentPart
	if err := json.Unmarshal(raw, &parts); err != nil {
		// 单块的「对象形态」：非标准，但手写请求里很常见，尽力提取。
		var single openAIContentPart
		if err := json.Unmarshal(raw, &single); err != nil {
			return "", nil
		}
		if single.Type == "image_url" && single.ImageURL != nil && single.ImageURL.URL != "" {
			return "", []string{single.ImageURL.URL}
		}
		if single.Text != "" {
			return single.Text, nil
		}
		return "", nil
	}

	var text strings.Builder
	var images []string
	for _, p := range parts {
		switch p.Type {
		case "text":
			text.WriteString(p.Text)
		case "image_url":
			if p.ImageURL != nil && p.ImageURL.URL != "" {
				images = append(images, p.ImageURL.URL)
			}
		}
	}
	return text.String(), images
}

// extractText 只要文本，丢弃图像。用于 system / assistant / tool —— 这三个角色的
// 内容在 OpenAI 协议里本就是纯文本，图像只对 user 有意义。
func extractText(raw json.RawMessage) string {
	text, _ := extractUserContent(raw)
	return text
}

// userMessage 组装 user 消息。
//
// 无图像时直接走 rosetta.User，与旧实现逐字节一致（不改变既有行为）；
// 带图像时才构造多块消息。只有在 rosetta 的 validate 允许的角色-块组合内
// （RoleUser 支持 text / image / audio / file）才可能通过。
func userMessage(raw json.RawMessage) rosetta.Message {
	text, images := extractUserContent(raw)
	if len(images) == 0 {
		return rosetta.User(text)
	}
	blocks := make([]rosetta.Block, 0, len(images)+1)
	if text != "" {
		blocks = append(blocks, rosetta.Block{Type: rosetta.BlockText, Text: text})
	}
	for _, u := range images {
		blocks = append(blocks, rosetta.Block{Type: rosetta.BlockImage, ImageURL: u})
	}
	return rosetta.Message{Role: rosetta.RoleUser, Blocks: blocks}
}

// parseEffort 把各协议的各种思考强度写法归一到 rosetta 的三档。
//
// 入口不止 OpenAI Chat 的 reasoning_effort：Responses 的 reasoning.effort
// 还会有 minimal（gpt-5 系列新增，比 low 更省）与 xhigh（比 high 更强），
// 直连上游时这两档是合法值。rosetta 侧只认 low/medium/high，所以做映射：
//
//	minimal → low（都属"少想一点"这一侧）
//	xhigh   → high
//
// 未知值返回 EffortUnset，调用方据此不设 Thinking —— 退回上游默认强度，
// 而不是把整个请求 400 掉（客户端可能按更新的模型能力在发请求）。
func parseEffort(s string) rosetta.Effort {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "minimal":
		return rosetta.EffortLow
	case "low":
		return rosetta.EffortLow
	case "medium":
		return rosetta.EffortMedium
	case "high", "xhigh":
		return rosetta.EffortHigh
	default:
		return rosetta.EffortUnset
	}
}
