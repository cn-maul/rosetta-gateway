package inwire

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/cn-maul/rosetta"
)

// Anthropic Messages 协议的入站解码（POST /v1/messages）。
//
// 覆盖面与取舍：
//   - content 同时接受字符串与块数组；user 块里的 tool_result 会拆成 rosetta 的
//     RoleTool 消息（Anthropic 把工具结果放在 user 轮里，rosetta 是独立角色）。
//   - thinking / redacted_thinking 原样映射为 rosetta 块（含签名）—— 上游同为
//     Anthropic 时完整回放；上游是 OpenAI 系时由 SDK 适配器剥除
//     （provider_openai_chat.go 的 thinking 分支），即 DESIGN §17 R4 的默认策略。
//   - cache_control（Claude Code 依赖它做提示缓存）逐块透传给 rosetta；
//     OpenAI 系上游自动忽略。
//   - top_k / tool_choice 不是 rosetta 建模字段：anthropic 上游经 Extra 原样
//     透传，openai-chat / openai-responses 上游把 tool_choice 翻译成各自形状
//     （能等价翻译的必须翻译），top_k 丢弃（OpenAI 系没有这个旋钮）。
//     rosetta 的保留键校验已确认这两个键不在各协议的保留集合里，不会撞车。

type AnthropicMessagesRequest struct {
	Model     string             `json:"model"`
	MaxTokens int                `json:"max_tokens"`
	System    json.RawMessage    `json:"system,omitempty"` // string | [{type:"text",text,cache_control}]
	Messages  []AnthropicMessage `json:"messages"`

	Temperature *float64 `json:"temperature,omitempty"`
	TopP        *float64 `json:"top_p,omitempty"`
	TopK        *int     `json:"top_k,omitempty"`

	StopSequences []string             `json:"stop_sequences,omitempty"`
	Stream        bool                 `json:"stream,omitempty"`
	Tools         []AnthropicTool      `json:"tools,omitempty"`
	ToolChoice    *AnthropicToolChoice `json:"tool_choice,omitempty"`
	Thinking      *AnthropicThinking   `json:"thinking,omitempty"`

	// RawEffort 是 thinking.budget_tokens 映射出的**档位**（原值，见
	// OpenAIChatRequest.RawEffort）；空串 = 未请求思考。
	//
	// Anthropic 没有 effort 字段，客户端给的是 token 预算，而预算→档位的映射
	// 不可逆（三个档位分别对应一段预算区间）。但映射方向是确定的，所以数据面
	// 仍能按「目标模型支持的档位子集」把档位夹紧，再交回 rosetta 反算预算 ——
	// 对 OpenAI 系上游而言，夹紧后的档位才是真正发出去的 reasoning_effort。
	RawEffort string `json:"-"`

	// Metadata / service_tier / 等未建模字段由 encoding/json 忽略。
}

type AnthropicMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"` // string | 块数组
}

type AnthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
	// CacheControl 透传为 rosetta 的工具级缓存断点（ usually 打在最后一个工具上，
	// 缓存它之前的整套工具表）。
	CacheControl *AnthropicCacheControl `json:"cache_control,omitempty"`
}

type AnthropicToolChoice struct {
	Type string `json:"type"` // auto | any | tool | none
	Name string `json:"name,omitempty"`
}

type AnthropicThinking struct {
	Type         string `json:"type"` // enabled | disabled
	BudgetTokens int    `json:"budget_tokens,omitempty"`
}

type AnthropicCacheControl struct {
	Type string `json:"type,omitempty"`
	TTL  string `json:"ttl,omitempty"`
}

// anthropicBlock 是入站内容块的宽松形状：各类型只取自己需要的字段。
type anthropicBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`

	Source *struct {
		Type      string `json:"type"` // base64 | url | text
		MediaType string `json:"media_type"`
		Data      string `json:"data"`
		URL       string `json:"url"`
	} `json:"source,omitempty"`

	ID    string          `json:"id,omitempty"`    // tool_use
	Name  string          `json:"name,omitempty"`  // tool_use / tool_result
	Input json.RawMessage `json:"input,omitempty"` // tool_use

	ToolUseID string          `json:"tool_use_id,omitempty"` // tool_result
	Content   json.RawMessage `json:"content,omitempty"`     // tool_result: string | 块
	IsError   *bool           `json:"is_error,omitempty"`

	Thinking  string `json:"thinking,omitempty"`  // thinking
	Signature string `json:"signature,omitempty"` // thinking
	Data      string `json:"data,omitempty"`      // redacted_thinking

	CacheControl *AnthropicCacheControl `json:"cache_control,omitempty"`
}

// DecodeAnthropicMessagesRequest 解析 /v1/messages 请求体。
// 超限以 *http.MaxBytesError 报出（调用方映射 413），与 OpenAI 入口同一套纪律。
func DecodeAnthropicMessagesRequest(r *http.Request, maxBytes int64) (*AnthropicMessagesRequest, error) {
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

	var req AnthropicMessagesRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, fmt.Errorf("decode body: %w", err)
	}

	if req.Model == "" {
		return nil, fmt.Errorf("model is required")
	}
	// Anthropic 协议的 max_tokens 是必填（OpenAI 才是可选）：官方 SDK 与
	// Claude Code 都会带，缺失说明不是合法的 Messages 请求。
	if req.MaxTokens <= 0 {
		return nil, fmt.Errorf("max_tokens: must be a positive integer")
	}
	if len(req.Messages) == 0 {
		return nil, fmt.Errorf("messages: required")
	}
	if req.ToolChoice != nil {
		switch req.ToolChoice.Type {
		case "auto", "any", "none":
		case "tool":
			if req.ToolChoice.Name == "" {
				return nil, fmt.Errorf("tool_choice: type=tool requires name")
			}
		default:
			return nil, fmt.Errorf("tool_choice.type: must be auto|any|tool|none, got %q", req.ToolChoice.Type)
		}
	}
	if req.Thinking != nil {
		switch req.Thinking.Type {
		case "enabled":
			if req.Thinking.BudgetTokens <= 0 {
				return nil, fmt.Errorf("thinking: type=enabled requires budget_tokens > 0")
			}
		case "disabled":
		default:
			return nil, fmt.Errorf("thinking.type: must be enabled|disabled, got %q", req.Thinking.Type)
		}
		// 预算 → 档位，与 rosetta 的 effortFromBudget 同一分界，
		// 保证 ToRosetta 之后数据面拿到的档位与最终发出去的 budget 对得上。
		if req.Thinking.Type == "enabled" {
			switch {
			case req.Thinking.BudgetTokens <= 4096:
				req.RawEffort = "low"
			case req.Thinking.BudgetTokens <= 16384:
				req.RawEffort = "medium"
			default:
				req.RawEffort = "high"
			}
		}
	}

	return &req, nil
}

// ToRosetta 组装统一请求。RoleSystem 的 cache_control 断点只能活在消息块里
// （ChatRequest.System 是纯字符串），所以 system 为块数组时走 RoleSystem 消息。
func (a *AnthropicMessagesRequest) ToRosetta() *rosetta.ChatRequest {
	req := &rosetta.ChatRequest{
		Model:           a.Model,
		MaxOutputTokens: a.MaxTokens,
		Temperature:     a.Temperature,
		TopP:            a.TopP,
		StopSequences:   a.StopSequences,
	}

	switch {
	case len(a.System) == 0:
	case json.Unmarshal(a.System, new(string)) == nil:
		var s string
		_ = json.Unmarshal(a.System, &s)
		req.System = s
	default:
		var blocks []anthropicBlock
		if err := json.Unmarshal(a.System, &blocks); err == nil {
			var sysBlocks []rosetta.Block
			for _, b := range blocks {
				if b.Type != "text" {
					continue
				}
				sysBlocks = append(sysBlocks, rosetta.Block{Type: rosetta.BlockText, Text: b.Text, CacheControl: b.cacheControl()})
			}
			if len(sysBlocks) > 0 {
				req.Messages = append(req.Messages, rosetta.Message{Role: rosetta.RoleSystem, Blocks: sysBlocks})
			}
		}
	}

	if len(a.Tools) > 0 {
		tools := make([]rosetta.ToolDefinition, 0, len(a.Tools))
		for _, t := range a.Tools {
			params := t.InputSchema
			if len(params) == 0 {
				params = json.RawMessage(`{}`)
			}
			td := rosetta.ToolDefinition{Name: t.Name, Description: t.Description, Parameters: params}
			if t.CacheControl != nil {
				td.CacheControl = t.CacheControl.toRosetta()
			}
			tools = append(tools, td)
		}
		req.Tools = tools
	}

	if a.Thinking != nil && a.Thinking.Type == "enabled" {
		req.Thinking = &rosetta.ThinkingConfig{BudgetTokens: a.Thinking.BudgetTokens}
	}

	for _, m := range a.Messages {
		req.Messages = append(req.Messages, convertAnthropicMessage(m)...)
	}
	return req
}

// ApplyUpstreamExtras 把 Anthropic 入口的协议私有字段挂到 req.Extra。
// 门的开口方向与 OpenAI 入口相反：anthropic 上游拿原样字段，openai-chat /
// openai-responses 拿翻译后的 tool_choice，top_k 谁都拿不到（只有 Anthropic
// 有这个旋钮）。
//
// openai-responses 分支（2026-10-07 P1-8）：此前对 responses 整个跳过、把
// tool_choice 静默丢掉 —— 故障转移前后同一请求行为不一致。现在做等价翻译
// （映射见 AnthropicToolChoice.responsesValue）；top_k 在 Responses 无对应物，
// 丢弃不改变正确性。
func (a *AnthropicMessagesRequest) ApplyUpstreamExtras(req *rosetta.ChatRequest, protocol string) {
	switch protocol {
	case "anthropic":
		extra := map[string]any{}
		if a.TopK != nil {
			extra["top_k"] = *a.TopK
		}
		if a.ToolChoice != nil {
			// Anthropic 形状原样重建（字段是已知集合，比保留原始字节更可预测；
			// 两个键都不在 rosetta 的 anthropic 保留键集合里，不会撞车）。
			extra["tool_choice"] = a.ToolChoice.anthropicValue()
		}
		if len(extra) > 0 {
			mergeExtra(req, extra)
		}
	case "", "openai-chat", "auto":
		// "auto" 必须与 ""/"openai-chat" 同组：upstream.buildClient 对 auto
		// 不加 WithProtocol，让 SDK 自动探测，实际落到的几乎总是 OpenAI 方言。
		// 把 auto 漏在这里会让「协议自动探测」这条最常用的配置静默丢掉 tool_choice。
		if a.ToolChoice == nil {
			return
		}
		if v := a.ToolChoice.openAIValue(); v != nil {
			mergeExtra(req, map[string]any{"tool_choice": v})
		}
	case "openai-responses":
		if a.ToolChoice == nil {
			return
		}
		if v := a.ToolChoice.responsesValue(); v != nil {
			mergeExtra(req, map[string]any{"tool_choice": v})
		}
		// top_k 在 Responses API 无对应物：它是采样多样性旋钮（软偏好），
		// 丢弃只改变采样倾向、不改变请求的正确性 —— 与结构化输出这类硬约束
		// 的处置（网关层拒绝）不同。
	}
}

// anthropicValue 把 ToolChoice 表达为 Anthropic wire 形状（{"type","name"?}）。
func (t *AnthropicToolChoice) anthropicValue() map[string]any {
	m := map[string]any{"type": t.Type}
	if t.Name != "" {
		m["name"] = t.Name
	}
	return m
}

func (t *AnthropicCacheControl) toRosetta() *rosetta.CacheControl {
	return &rosetta.CacheControl{Type: t.Type, TTL: t.TTL}
}

func (b *anthropicBlock) cacheControl() *rosetta.CacheControl {
	if b.CacheControl == nil {
		return nil
	}
	return b.CacheControl.toRosetta()
}

// openAIValue 把 Anthropic 的 tool_choice 翻译成 OpenAI chat 的形状。
// 不认识的类型返回 nil（上层跳过）—— decode 已把类型收敛到白名单，这是防御。
func (t *AnthropicToolChoice) openAIValue() any {
	switch t.Type {
	case "auto":
		return "auto"
	case "any":
		return "required"
	case "none":
		return "none"
	case "tool":
		return map[string]any{
			"type":     "function",
			"function": map[string]any{"name": t.Name},
		}
	}
	return nil
}

// responsesValue 把 Anthropic 的 tool_choice 翻译成 Responses 的形状：
// auto→"auto"、any→"required"、tool→{"type":"function","name"}。
// "none" 返回 nil（丢弃）—— Responses 只表达「模型自主决定不调工具」，
// 与 Anthropic 的 none（禁止调用已声明工具）语义并不完全对齐，且 OpenAI
// 官方已将其归入 deprecated；翻译过去只会造成两协议行为漂移的错觉。
// 不认识的类型返回 nil（上层跳过）—— decode 已把类型收敛到白名单，这是防御。
func (t *AnthropicToolChoice) responsesValue() any {
	switch t.Type {
	case "auto":
		return "auto"
	case "any":
		return "required"
	case "tool":
		if t.Name == "" {
			return nil
		}
		return map[string]any{"type": "function", "name": t.Name}
	}
	return nil
}

// mergeExtra 把 extra 并进 req.Extra（不覆盖已有键 —— Extra 由多个来源共用）。
func mergeExtra(req *rosetta.ChatRequest, extra map[string]any) {
	if req.Extra == nil {
		req.Extra = extra
		return
	}
	for k, v := range extra {
		req.Extra[k] = v
	}
}

// convertAnthropicMessage 把一条 Anthropic 消息转成 0..2 条 rosetta 消息：
// user 轮里的 tool_result 拆成 RoleTool 消息（余下块组成 RoleUser），
// 其余角色一对一。
func convertAnthropicMessage(m AnthropicMessage) []rosetta.Message {
	switch m.Role {
	case "assistant":
		if blocks := assistantBlocks(m.Content); len(blocks) > 0 {
			return []rosetta.Message{rosetta.AssistantBlocks(blocks...)}
		}
		// content 是纯字符串（或空）的形态：AssistantBlocks 会因零块被
		// rosetta validate 拒掉，这里按文本走 Assistant。
		return []rosetta.Message{rosetta.Assistant(contentString(m.Content))}
	default: // user；未知角色按 user 处理（rosetta validate 会拦非法角色）。
		return convertUserMessage(m.Content)
	}
}

// contentString 把「string | 块数组」的 content 读成纯文本（块数组取 text 拼接）。
func contentString(raw json.RawMessage) string {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var sb strings.Builder
	for _, b := range decodeBlocks(raw) {
		if b.Type == "text" {
			sb.WriteString(b.Text)
		}
	}
	return sb.String()
}

func assistantBlocks(raw json.RawMessage) []rosetta.Block {
	blocks := decodeBlocks(raw)
	out := make([]rosetta.Block, 0, len(blocks))
	for _, b := range blocks {
		switch b.Type {
		case "text":
			if b.Text != "" {
				out = append(out, rosetta.Block{Type: rosetta.BlockText, Text: b.Text})
			}
		case "tool_use":
			args := string(b.Input)
			if strings.TrimSpace(args) == "" {
				args = "{}"
			}
			out = append(out, rosetta.ToolCall(b.ID, b.Name, args))
		case "thinking":
			// 签名一起带上：回放给 Anthropic 上游时缺签名会被拒（R4）。
			out = append(out, rosetta.Thinking(b.Thinking, b.Signature))
		case "redacted_thinking":
			out = append(out, rosetta.Block{Type: rosetta.BlockRedactedThinking, Thinking: b.Data})
		}
	}
	return out
}

func convertUserMessage(raw json.RawMessage) []rosetta.Message {
	blocks := decodeBlocks(raw)

	// content 是纯字符串（最常见形态）：一个文本块。
	if len(blocks) == 0 {
		var s string
		if err := json.Unmarshal(raw, &s); err == nil {
			return []rosetta.Message{rosetta.User(s)}
		}
		return []rosetta.Message{rosetta.User("")}
	}

	var toolResults []rosetta.Block
	var rest []rosetta.Block
	for _, b := range blocks {
		switch b.Type {
		case "tool_result":
			text := toolResultText(b)
			tb := rosetta.Block{Type: rosetta.BlockToolResult, ToolCallID: b.ToolUseID, Content: text}
			if b.IsError != nil {
				tb.IsError = *b.IsError
			}
			toolResults = append(toolResults, tb)
		case "text":
			if b.Text != "" {
				rest = append(rest, rosetta.Block{Type: rosetta.BlockText, Text: b.Text, CacheControl: b.cacheControl()})
			} else if b.CacheControl != nil {
				// 空文本块 + 断点：rosetta 会拒绝，这里保留结构让它报出一致错误。
				rest = append(rest, rosetta.Block{Type: rosetta.BlockText, Text: "", CacheControl: b.cacheControl()})
			}
		case "image":
			if u := imageURL(b); u != "" {
				rest = append(rest, rosetta.Block{Type: rosetta.BlockImage, ImageURL: u, CacheControl: b.cacheControl()})
			}
		case "document":
			if blk, ok := fileBlock(b); ok {
				rest = append(rest, blk)
			}
		default:
			// 未知块类型（各家私有扩展）：交给 rosetta validate 报 400，
			// 不静默丢弃 —— 静默丢弃正是本网关反复修过的「无声吞内容」。
			rest = append(rest, rosetta.Block{Type: rosetta.BlockType(b.Type)})
		}
	}

	var out []rosetta.Message
	if len(toolResults) > 0 {
		out = append(out, rosetta.Message{Role: rosetta.RoleTool, Blocks: toolResults})
	}
	if len(rest) > 0 {
		out = append(out, rosetta.Message{Role: rosetta.RoleUser, Blocks: rest})
	}
	if len(out) == 0 {
		out = append(out, rosetta.User(""))
	}
	return out
}

// decodeBlocks 把 content 解成块数组；纯字符串形态返回 nil（调用方按文本处理）。
func decodeBlocks(raw json.RawMessage) []anthropicBlock {
	if len(raw) == 0 {
		return nil
	}
	var blocks []anthropicBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil
	}
	return blocks
}

// imageURL 归一 Anthropic image source：base64 → data: URL，url → 原样。
func imageURL(b anthropicBlock) string {
	if b.Source == nil {
		return ""
	}
	switch b.Source.Type {
	case "base64":
		if b.Source.Data == "" {
			return ""
		}
		media := b.Source.MediaType
		if media == "" {
			media = "image/png"
		}
		return "data:" + media + ";base64," + b.Source.Data
	case "url":
		return b.Source.URL
	}
	return ""
}

// fileBlock 归一 document 块（Anthropic 的 PDF 等文档输入）。
func fileBlock(b anthropicBlock) (rosetta.Block, bool) {
	if b.Source == nil {
		return rosetta.Block{}, false
	}
	media := b.Source.MediaType
	if media == "" {
		media = "application/pdf"
	}
	switch b.Source.Type {
	case "base64":
		if b.Source.Data == "" {
			return rosetta.Block{}, false
		}
		return rosetta.Block{Type: rosetta.BlockFile, FileName: b.Name, MimeType: media, FileData: b.Source.Data}, true
	case "url":
		if b.Source.URL == "" {
			return rosetta.Block{}, false
		}
		return rosetta.Block{Type: rosetta.BlockFile, FileName: b.Name, MimeType: media, FileData: b.Source.URL}, true
	}
	return rosetta.Block{}, false
}

// toolResultText 展平 tool_result 的 content（string 或块数组）为文本。
func toolResultText(b anthropicBlock) string {
	if len(b.Content) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(b.Content, &s); err == nil {
		return s
	}
	var sb strings.Builder
	for _, inner := range decodeBlocks(b.Content) {
		if inner.Type == "text" {
			sb.WriteString(inner.Text)
		}
	}
	return sb.String()
}
