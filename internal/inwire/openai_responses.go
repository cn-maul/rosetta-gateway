package inwire

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/cn-maul/rosetta"
)

// OpenAI Responses 协议的入站解码（POST /v1/responses）。
//
// 覆盖面与取舍：
//   - input 收字符串与 item 数组两种形态；message 的 content 收字符串与
//     分块数组（input_text / output_text / input_image）。
//   - function_call / function_call_output item 映射为 rosetta 的工具调用与
//     工具结果；reasoning item 剥除（历史思考不属于对话内容）。
//   - instructions 映射为 System；developer/system 角色 item 映射为 RoleSystem。
//   - reasoning.effort 映射为 rosetta 的 ThinkingConfig（SDK 负责跨协议翻译）。
//   - 不支持的 item 类型（item_reference / web_search 等内建工具）**显式 400**，
//     不静默丢弃 —— 静默丢弃正是本网关反复修过的「无声吞内容」。
//   - previous_response_id / store：网关不托管会话状态（DESIGN §1 明确不做），
//     两者被忽略；Codex 等客户端在 store:false 下随请求带全量历史，天然兼容。
//   - text.format 映射到 openai-chat 上游的 response_format（json_object 直映、
//     json_schema 翻译外壳）；responses 上游经 Extra 原样回传；anthropic 上游
//     没有对应物 —— tool_choice 仍做等价翻译（any/tool），结构化输出这类
//     硬约束则由网关层拒绝（见 RequiresStructuredOutput 与 handleIngress）。

type ResponsesRequest struct {
	Model           string          `json:"model"`
	Input           json.RawMessage `json:"input"`
	Instructions    string          `json:"instructions,omitempty"`
	MaxOutputTokens *int            `json:"max_output_tokens,omitempty"`
	Temperature     *float64        `json:"temperature,omitempty"`
	TopP            *float64        `json:"top_p,omitempty"`
	Stream          bool            `json:"stream,omitempty"`

	Tools             []ResponsesTool `json:"tools,omitempty"`
	ToolChoice        json.RawMessage `json:"tool_choice,omitempty"`
	ParallelToolCalls *bool           `json:"parallel_tool_calls,omitempty"`
	Reasoning         *struct {
		Effort string `json:"effort,omitempty"`
	} `json:"reasoning,omitempty"`

	Store              *bool           `json:"store,omitempty"`
	PreviousResponseID string          `json:"previous_response_id,omitempty"`
	Text               json.RawMessage `json:"text,omitempty"` // {"format": {...}}

	// metadata / service_tier / safety_identifier 等未建模字段由 encoding/json 忽略。
}

type ResponsesTool struct {
	Type        string          `json:"type"` // 仅支持 function（内建工具无法代理到任意上游）
	Name        string          `json:"name,omitempty"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

type responsesItem struct {
	Type    string          `json:"type"`
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`

	CallID    string          `json:"call_id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Arguments string          `json:"arguments,omitempty"`
	Output    json.RawMessage `json:"output,omitempty"` // function_call_output

	ID string `json:"id,omitempty"` // item_reference
}

type responsesContentPart struct {
	Type     string `json:"type"`
	Text     string `json:"text"`
	ImageURL string `json:"image_url"`
}

// DecodeResponsesRequest 解析 /v1/responses 请求体。超限以 *http.MaxBytesError
// 报出（调用方映射 413），与其他入口同一套纪律。
func DecodeResponsesRequest(r *http.Request, maxBytes int64) (*ResponsesRequest, error) {
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

	var req ResponsesRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, fmt.Errorf("decode body: %w", err)
	}
	if req.Model == "" {
		return nil, fmt.Errorf("model is required")
	}
	if len(req.Input) == 0 {
		return nil, fmt.Errorf("input is required")
	}
	if req.MaxOutputTokens != nil && *req.MaxOutputTokens <= 0 {
		return nil, fmt.Errorf("max_output_tokens: must be a positive integer")
	}
	for _, t := range req.Tools {
		if t.Type != "function" {
			return nil, fmt.Errorf("tools: unsupported tool type %q (only \"function\" can be proxied to upstreams)", t.Type)
		}
		if t.Name == "" {
			return nil, fmt.Errorf("tools: function tool requires name")
		}
	}
	if len(req.ToolChoice) > 0 {
		var s string
		if json.Unmarshal(req.ToolChoice, &s) != nil {
			var obj struct {
				Type string `json:"type"`
				Name string `json:"name"`
			}
			if json.Unmarshal(req.ToolChoice, &obj) != nil {
				return nil, fmt.Errorf("tool_choice: must be a string or {type,name} object")
			}
			if obj.Type == "function" && obj.Name == "" {
				return nil, fmt.Errorf("tool_choice: type=function requires name")
			}
		}
	}

	return &req, nil
}

// ToRosetta 组装统一请求。input 是字符串时整体作为一条 user 消息。
func (a *ResponsesRequest) ToRosetta() *rosetta.ChatRequest {
	req := &rosetta.ChatRequest{
		Model:       a.Model,
		Temperature: a.Temperature,
		TopP:        a.TopP,
		System:      a.Instructions,
	}
	if a.MaxOutputTokens != nil {
		req.MaxOutputTokens = *a.MaxOutputTokens
	}
	if len(a.Tools) > 0 {
		tools := make([]rosetta.ToolDefinition, 0, len(a.Tools))
		for _, t := range a.Tools {
			params := t.Parameters
			if len(params) == 0 {
				params = json.RawMessage(`{}`)
			}
			tools = append(tools, rosetta.ToolDefinition{Name: t.Name, Description: t.Description, Parameters: params})
		}
		req.Tools = tools
	}
	// minimal / xhigh 是 OpenAI 为 gpt-5 系列新增的两档（比 low 更省、
	// 比 high 更强），直连上游时合法。旧实现只认 low/medium/high，这两档
	// 落空 → req.Thinking 保持 nil → 客户端以为自己开了推理模式，实际静默
	// 不思考。归一逻辑见 inwire.parseEffort。
	if a.Reasoning != nil && a.Reasoning.Effort != "" {
		if eff := parseEffort(a.Reasoning.Effort); eff != rosetta.EffortUnset {
			req.Thinking = &rosetta.ThinkingConfig{Effort: eff}
		}
	}

	// 字符串形态：整段就是一条 user 消息。
	var s string
	if err := json.Unmarshal(a.Input, &s); err == nil {
		req.Messages = append(req.Messages, rosetta.User(s))
		return req
	}

	var items []responsesItem
	if err := json.Unmarshal(a.Input, &items); err != nil {
		// 非法 input 在 decode 侧就该报（这里补一道：ToRosetta 可被逐 attempt 调用）。
		return req
	}
	for _, item := range items {
		switch item.Type {
		case "", "message":
			req.Messages = append(req.Messages, convertResponsesMessage(item)...)
		case "function_call":
			args := item.Arguments
			if strings.TrimSpace(args) == "" {
				args = "{}"
			}
			req.Messages = append(req.Messages, rosetta.Message{
				Role:   rosetta.RoleAssistant,
				Blocks: []rosetta.Block{rosetta.ToolCall(item.CallID, item.Name, args)},
			})
		case "function_call_output":
			req.Messages = append(req.Messages, rosetta.ToolResult(item.CallID, "", responsesItemOutputText(item)))
		case "reasoning":
			// 历史思考不是对话内容：剥除（与 OpenAI 适配器对 thinking 块的处置一致）。
		default:
			// item_reference / 内建工具调用等：留空块让 rosetta validate 以
			// 明确错误拒绝，不静默吞。
			req.Messages = append(req.Messages, rosetta.Message{
				Role:   rosetta.RoleUser,
				Blocks: []rosetta.Block{{Type: rosetta.BlockType("responses_item:" + item.Type)}},
			})
		}
	}
	return req
}

func convertResponsesMessage(item responsesItem) []rosetta.Message {
	role := item.Role
	switch role {
	case "system", "developer":
		return []rosetta.Message{rosetta.System(responsesContentText(item.Content, false))}
	case "assistant":
		// assistant 的正文在 output_text 分块里。
		return []rosetta.Message{rosetta.Assistant(responsesContentText(item.Content, true))}
	case "user":
		// 显式 user：只认 input_text。output_text 是 assistant 侧的标记，
		// 混进来等于把模型自己的输出当成用户说的话。
		return userMessageFromContent(item.Content)
	default: // role 缺失 / 未知角色
		// 拿不准时**只**采信 input_text。
		//
		// 修复前这一支把 input_text 与 output_text 并集，于是「role 缺失
		// 的 message 项」里的模型输出被当成人话发给上游。实测：
		//   输入 role:user{input_text:"QUESTION"} + role 缺失{output_text:"SECRET-ASSISTANT"}
		//   -> 上游收到 messages[1] = {role:"user", content:"SECRET-ASSISTANT"}
		// 而同一请求里显式写 role:"assistant" 时，输出落在 assistant 轮。
		// 丢弃 output_text 是更安全的错法：少一段历史，而不是把模型的
		// 上一句回答伪装成用户的新指令（那会真的改变它的行为）。
		return userMessageFromContent(item.Content)
	}
}

// userMessageFromContent 把一个 content 变成单条 user 消息。
//
// 文本分块收成一段，图片分块保留 —— 与修复前的并集逻辑相比，
// 唯一的行为变化是 user 侧不再吃 output_text。
func userMessageFromContent(content json.RawMessage) []rosetta.Message {
	// content 可能是纯字符串（最常见的 `{"role":"user","content":"你好"}`）。
	// decodeResponsesParts 只认分块数组，字符串会得到空结果 —— 修复前
	// 这里因此把那条消息整条丢掉，user 的第一句话凭空消失。
	if s, ok := plainContentText(content); ok {
		if s == "" {
			return nil
		}
		return []rosetta.Message{rosetta.User(s)}
	}
	blocks := make([]rosetta.Block, 0, 2)
	for _, p := range decodeResponsesParts(content) {
		switch p.Type {
		case "input_text":
			blocks = append(blocks, rosetta.Block{Type: rosetta.BlockText, Text: p.Text})
		case "input_image":
			if p.ImageURL != "" {
				blocks = append(blocks, rosetta.Block{Type: rosetta.BlockImage, ImageURL: p.ImageURL})
			}
		}
	}
	// 没有任何可采信的分块时**整条丢弃**。
	//
	// 不能塞一个空文本块：rosetta 的校验会判「message has no content blocks」
	// （空串不算内容）→ 整个请求 400，那比「把 assistant 输出当用户输入」还糟
	// —— 至少后者请求还能通。实测：role 缺失且只带 output_text 的项
	// 直接把请求打成 400。
	//
	// 丢掉之后剩下的项照常转发，客户端拿到的是「少了一段它自己没标角色的
	// 历史」，而不是整个请求失败。
	if len(blocks) == 0 {
		return nil
	}
	return []rosetta.Message{{Role: rosetta.RoleUser, Blocks: blocks}}
}

// plainContentText 报告 content 是否是纯字符串形态，并返回其值。
// 分块数组返回 ok=false（调用方走 decodeResponsesParts）。
func plainContentText(raw json.RawMessage) (string, bool) {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, true
	}
	return "", false
}

// responsesContentText 把 message 的 content（字符串或分块数组）读成纯文本。
// includeOutputText 区分两侧：assistant 的 output_text 才是正文，user 侧只认
// input_text —— 读错方向会把对端协议的标记当正文传给上游。
func responsesContentText(raw json.RawMessage, includeOutputText bool) string {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var sb strings.Builder
	for _, p := range decodeResponsesParts(raw) {
		if (includeOutputText && p.Type == "output_text") || (!includeOutputText && p.Type == "input_text") {
			sb.WriteString(p.Text)
		}
	}
	return sb.String()
}

func decodeResponsesParts(raw json.RawMessage) []responsesContentPart {
	if len(raw) == 0 {
		return nil
	}
	var parts []responsesContentPart
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil
	}
	return parts
}

// responsesItemOutputText 展平 function_call_output 的 output（字符串或分块）。
func responsesItemOutputText(item responsesItem) string {
	var s string
	if err := json.Unmarshal(item.Output, &s); err == nil {
		return s
	}
	var sb strings.Builder
	for _, p := range decodeResponsesParts(item.Output) {
		if p.Type == "output_text" {
			sb.WriteString(p.Text)
		}
	}
	return sb.String()
}

// ApplyUpstreamExtras 按上游协议挂 Responses 私有字段。
//
// anthropic 分支（2026-10-07 P1-8）：此前对 anthropic 整个跳过、把 tool_choice
// 静默丢掉 —— 故障转移前后同一请求行为不一致。现在能等价翻译的 tool_choice
// 必须翻译；text.format 在 Anthropic 无对应物，属硬约束，由网关层
// requiresStructuredOutput 拒绝，不会带着丢失的约束放行。
func (a *ResponsesRequest) ApplyUpstreamExtras(req *rosetta.ChatRequest, protocol string) {
	switch protocol {
	case "anthropic":
		// tool_choice 等价翻译：auto→{"type":"auto"}、required→{"type":"any"}、
		// 命名函数→{"type":"tool"}；"none" 在 Anthropic 无对应物（无法表达
		// 「工具照常声明但本轮禁止调用」），丢弃 —— 见 responsesToolChoiceAnthropic。
		if tc := responsesToolChoiceAnthropic(a.ToolChoice); tc != nil {
			mergeExtra(req, map[string]any{"tool_choice": tc})
		}
		// text.format 在 Anthropic 无对应物。json_object / json_schema 是硬约束，
		// 不允许「200 但约束丢失」—— 这类请求在 handleIngress 就被
		// requiresStructuredOutput 挡在 anthropic 候选之外，到不了这里。
		// parallel_tool_calls 亦无开关对应物，丢弃不改变正确性。
		return

	case "", "openai-chat", "auto":
		// "auto" 与 ""/"openai-chat" 同组：auto 下SDK 自动探测，落点几乎总是
		// OpenAI 方言，漏掉它等于让最常用的协议配置静默丢字段。
		extra := map[string]any{}
		if v := responsesToolChoiceOpenAI(a.ToolChoice); v != nil {
			extra["tool_choice"] = v
		}
		if f := a.textFormatMap(); f != nil {
			if rf := textFormatToResponseFormat(f); rf != nil {
				extra["response_format"] = rf
			}
		}
		if a.ParallelToolCalls != nil {
			extra["parallel_tool_calls"] = *a.ParallelToolCalls
		}
		if len(extra) > 0 {
			mergeExtra(req, extra)
		}
	case "openai-responses":
		extra := map[string]any{}
		if len(a.ToolChoice) > 0 {
			extra["tool_choice"] = json.RawMessage(a.ToolChoice)
		}
		// text 是 {"format": {...}} 的外壳，所以这里要**拆开**再放回 format。
		//
		// 修复前是把整个 a.Text 当成 format 的值，出站变成
		//   "text":{"format":{"format":{"type":"json_schema",…}}}
		// 上游看到的 format 对象里没有 type、schema 埋在重复的 format 键下，
		// 于是结构化输出**静默不生效**，而客户端以为它生效了
		// （实测抓到的出站 body 就是双层）。同函数上面的 openai-chat 分支
		// 用 textFormatMap() 正确拆包 —— 两个分支的处理方式本就该一致。
		if f := a.textFormatMap(); f != nil {
			extra["text"] = map[string]any{"format": f}
		}
		// text 里除 format 之外的键（verbosity 等）不能跟着丢，原样带上。
		if rest := a.textPassthroughKeys(); len(rest) > 0 {
			textObj, _ := extra["text"].(map[string]any)
			if textObj == nil {
				textObj = map[string]any{}
				extra["text"] = textObj
			}
			for k, v := range rest {
				if _, taken := textObj[k]; !taken {
					textObj[k] = v
				}
			}
		}
		if a.ParallelToolCalls != nil {
			extra["parallel_tool_calls"] = *a.ParallelToolCalls
		}
		// previous_response_id / store 交给 openai-responses 上游承接。
		//
		// 修复前这两个字段解析完就被丢弃：客户端拿它续会话，收到的却是
		// 「只含本次 input」的 200，模型完全不知道前文。SDK 没把
		// previous_response_id 列入 openai-responses 的保留键，走 Extra 合法。
		// 非 openai-responses 上游没有对应概念（chat/anthropic 都是无状态），
		// 继续按原样丢弃 —— 那条路径客户端本就该自带全量历史。
		if a.PreviousResponseID != "" {
			extra["previous_response_id"] = a.PreviousResponseID
		}
		if a.Store != nil {
			extra["store"] = *a.Store
		}
		if len(extra) > 0 {
			mergeExtra(req, extra)
		}
	}
}

func (a *ResponsesRequest) textFormatMap() map[string]any {
	if len(a.Text) == 0 {
		return nil
	}
	var wrapper struct {
		Format map[string]any `json:"format"`
	}
	if json.Unmarshal(a.Text, &wrapper) != nil || wrapper.Format == nil {
		return nil
	}
	return wrapper.Format
}

// textPassthroughKeys 取出 text 对象里除 format 之外的键（verbosity 等）。
//
// format 由 textFormatMap 单独处理；其余键我们没有建模，但原样带给上游
// 比静默丢弃更接近「透传」的本意，也避免修 format 时把别的字段一起弄丢。
func (a *ResponsesRequest) textPassthroughKeys() map[string]any {
	if len(a.Text) == 0 {
		return nil
	}
	var obj map[string]any
	if json.Unmarshal(a.Text, &obj) != nil {
		return nil
	}
	delete(obj, "format")
	if len(obj) == 0 {
		return nil
	}
	return obj
}

// responsesToolChoiceOpenAI 把 Responses 的 tool_choice 翻译成 openai-chat 形状。
func responsesToolChoiceOpenAI(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		switch s {
		case "auto", "none", "required":
			return s
		}
		return nil
	}
	var obj struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if json.Unmarshal(raw, &obj) != nil {
		return nil
	}
	switch obj.Type {
	case "auto", "none", "required":
		return obj.Type
	case "function":
		return map[string]any{"type": "function", "function": map[string]any{"name": obj.Name}}
	}
	return nil
}

// responsesToolChoiceAnthropic 把 Responses 的 tool_choice 翻译成 Anthropic 形状。
//
//	"auto" / {"type":"auto"}       → {"type":"auto"}
//	"required" / {"type":"required"} → {"type":"any"}（Anthropic 的「必须调一个」）
//	{"type":"function","name":…}   → {"type":"tool","name":…}
//	"none"                          → nil（丢弃）
//
// "none" 丢弃的原因与 chatToolChoiceAnthropic 相同：Anthropic 无法表达
// 「工具照常声明、但本轮禁止调用」。decode 已把 tool_choice 收敛为字符串或
// {type,name} 两种形状，未知取值返回 nil 丢弃（防御）。
func responsesToolChoiceAnthropic(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		switch s {
		case "auto":
			return map[string]any{"type": "auto"}
		case "required":
			return map[string]any{"type": "any"}
		default: // "none" 等：无对应物，丢弃。
			return nil
		}
	}
	var obj struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if json.Unmarshal(raw, &obj) != nil {
		return nil
	}
	switch obj.Type {
	case "auto":
		return map[string]any{"type": "auto"}
	case "required":
		return map[string]any{"type": "any"}
	case "function":
		if obj.Name == "" {
			return nil
		}
		return map[string]any{"type": "tool", "name": obj.Name}
	}
	return nil
}

// textFormatToResponseFormat 把 Responses 的 text.format 翻译成 openai-chat 的
// response_format。text（默认）返回 nil（无需表达）；json_object 直映；
// json_schema 翻译外壳（Responses 平铺 name/schema/strict，chat 嵌套在
// json_schema 子对象里）。未知类型返回占位错误形态的 map 由 SDK 拒绝 ——
// 不在这里静默吞。
func textFormatToResponseFormat(f map[string]any) map[string]any {
	switch t, _ := f["type"].(string); t {
	case "", "text":
		return nil
	case "json_object":
		return map[string]any{"type": "json_object"}
	case "json_schema":
		inner := map[string]any{"type": "json_schema"}
		for _, k := range []string{"name", "schema", "strict", "description"} {
			if v, ok := f[k]; ok {
				inner[k] = v
			}
		}
		return map[string]any{"type": "json_schema", "json_schema": inner}
	default:
		return map[string]any{"type": t}
	}
}

// responseFormatToTextFormat 把 openai-chat 的 response_format 翻译回 Responses
// 的 text.format —— textFormatToResponseFormat 的反向（chat 入口打到
// responses 上游时用）。json_object 直映；json_schema 把 chat 嵌套在
// json_schema 子对象里的 name/schema/strict/description 摊平回 Responses 的
// 平铺形状；"text"/缺省是两协议共同的默认值，返回 nil 不显式表达；未知类型
// 摊平原样透传，交给上游 SDK 拒绝 —— 不在这里静默吞。
func responseFormatToTextFormat(raw json.RawMessage) map[string]any {
	var f map[string]any
	if json.Unmarshal(raw, &f) != nil {
		return nil
	}
	switch t, _ := f["type"].(string); t {
	case "", "text":
		return nil
	case "json_object":
		return map[string]any{"type": "json_object"}
	case "json_schema":
		inner, _ := f["json_schema"].(map[string]any)
		out := map[string]any{"type": "json_schema"}
		for _, k := range []string{"name", "schema", "strict", "description"} {
			if v, ok := inner[k]; ok {
				out[k] = v
			}
		}
		return out
	default:
		return map[string]any{"type": t}
	}
}

// RequiresStructuredOutput 报告该请求是否带**硬性**结构化输出约束
// （text.format.type 为 json_object / json_schema）。与 chat 入口的同名方法
// 同一用途：Anthropic 上游没有对应物，网关据此把 anthropic 候选从链上滤掉
// 而不是放行后让约束静默失效（见 handleIngress）。format 缺失或为默认的
// "text" 时返回 false。
func (a *ResponsesRequest) RequiresStructuredOutput() bool {
	f := a.textFormatMap()
	if f == nil {
		return false
	}
	t, _ := f["type"].(string)
	return t == "json_object" || t == "json_schema"
}
