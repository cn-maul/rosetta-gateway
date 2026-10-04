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
//     json_schema 翻译外壳）；responses 上游经 Extra 原样回传。

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
	default: // user / 未知 → user
		blocks := make([]rosetta.Block, 0, 2)
		parts := decodeResponsesParts(item.Content)
		var text strings.Builder
		for _, p := range parts {
			switch p.Type {
			case "input_text", "output_text":
				text.WriteString(p.Text)
			case "input_image":
				if p.ImageURL != "" {
					blocks = append(blocks, rosetta.Block{Type: rosetta.BlockImage, ImageURL: p.ImageURL})
				}
			}
		}
		if t := text.String(); t != "" || len(blocks) == 0 {
			blocks = append([]rosetta.Block{{Type: rosetta.BlockText, Text: t}}, blocks...)
		}
		return []rosetta.Message{rosetta.Message{Role: rosetta.RoleUser, Blocks: blocks}}
	}
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
func (a *ResponsesRequest) ApplyUpstreamExtras(req *rosetta.ChatRequest, protocol string) {
	switch protocol {
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
		if len(a.Text) > 0 {
			// responses 线格式里 text 是 {"format": ...} 的外壳。
			extra["text"] = map[string]any{"format": json.RawMessage(a.Text)}
		}
		if a.ParallelToolCalls != nil {
			extra["parallel_tool_calls"] = *a.ParallelToolCalls
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
