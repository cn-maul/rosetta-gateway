package inwire

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/cn-maul/rosetta"
)

type OpenAIChatRequest struct {
	Model              string              `json:"model"`
	Messages           []OpenAIMessage     `json:"messages"`
	MaxTokens          *int                `json:"max_tokens,omitempty"`
	MaxCompletionTokens *int               `json:"max_completion_tokens,omitempty"`
	Temperature        *float64            `json:"temperature,omitempty"`
	TopP               *float64            `json:"top_p,omitempty"`
	Stop               []string            `json:"stop,omitempty"`
	Stream             bool                `json:"stream,omitempty"`
	StreamOptions      *StreamOptions      `json:"stream_options,omitempty"`
	Tools              []OpenAITool        `json:"tools,omitempty"`
	ToolChoice         any                 `json:"tool_choice,omitempty"`
	PresencePenalty     *float64            `json:"presence_penalty,omitempty"`
	FrequencyPenalty    *float64            `json:"frequency_penalty,omitempty"`
	N                  *int                `json:"n,omitempty"`

	// Extra is the raw passthrough slot mirroring rosetta.ChatRequest.Extra.
	// The json:"-" tag stops the decoder from ever writing it and nothing
	// else fills it yet, so it is always nil today. When unrecognized-field
	// collection lands, feed it through ApplyProtocolPrivateExtra so the
	// per-protocol gate still applies.
	Extra map[string]any `json:"-"`
}

type StreamOptions struct {
	IncludeUsage *bool `json:"include_usage,omitempty"`
}

type OpenAIMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content,omitempty"`
	Name       string          `json:"name,omitempty"`
	ToolCalls  []OpenAIToolCall `json:"tool_calls,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
}

type OpenAIToolCall struct {
	ID       string          `json:"id"`
	Type     string          `json:"type"`
	Function OpenAIFunction  `json:"function"`
}

type OpenAIFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type OpenAITool struct {
	Type     string          `json:"type"`
	Function OpenAIFuncDef   `json:"function"`
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
// maxBytes <= 0 时用内置兜底。调用方应当传入 cfg.Defaults.MaxRequestBodyBytes ——
// 这个上限此前在两层各写一份（这里硬编码 32MiB，中间件读配置），配大了内层先截断、
// json 报出「unexpected end of JSON input」这种看不出原因的错误；配小了外层先拦、
// 但错误被包成 400 而非 413。同一份值只有一个来源，才不会自相矛盾。
func DecodeOpenAIChatRequest(r *http.Request, maxBytes int64) (*OpenAIChatRequest, error) {
	if maxBytes <= 0 {
		maxBytes = defaultMaxBodyBytes
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBytes))
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
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

// ApplyProtocolPrivateExtra forwards the OpenAI-only request fields the SDK
// does not model onto req.Extra, so they reach the upstream instead of being
// silently dropped on the floor.
//
// Only an OpenAI-Chat upstream parses this shape. Anthropic spells tool_choice
// as an object rather than a bare string and has no penalty knobs at all, so
// forwarding OpenAI-shaped values there would turn today's silent drop into a
// hard 400; the Responses API has no penalty fields either. Both are skipped
// and the fields stay dropped, as before.
//
// "auto" and an empty protocol are let through on purpose: they resolve to an
// OpenAI-compatible dialect in practice, and whitelisting only "openai-chat"
// would disable the passthrough for the common bootstrap config.
//
// n is deliberately absent. rosetta models a single assistant turn -- the
// unary decoder reads Choices[0] and the streaming decoder skips every
// index != 0 -- so n > 1 would bill the caller for candidates the gateway
// then discards. Dropping it is the safer outcome.
func (r *OpenAIChatRequest) ApplyProtocolPrivateExtra(req *rosetta.ChatRequest, protocol string) {
	if protocol == "anthropic" || protocol == "openai-responses" {
		return
	}

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
		return rosetta.Assistant(extractText(m.Content))
	case "tool":
		return rosetta.ToolResult(m.ToolCallID, "", extractText(m.Content))
	default:
		// 角色未知时按 user 处理（rosetta 的 validate 随后会拒绝非法角色）。
		return userMessage(m.Content)
	}
}

// openAIContentPart 是 OpenAI「内容块数组」里的一块。
//
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

	result := ""
	var images []string
	for _, p := range parts {
		switch p.Type {
		case "text":
			result += p.Text
		case "image_url":
			if p.ImageURL != nil && p.ImageURL.URL != "" {
				images = append(images, p.ImageURL.URL)
			}
		}
	}
	return result, images
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
