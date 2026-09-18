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
	Extra              map[string]any      `json:"-"`
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

func DecodeOpenAIChatRequest(r *http.Request) (*OpenAIChatRequest, error) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 32*1024*1024))
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

	if r.Extra != nil {
		req.Extra = r.Extra
	}

	return req
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
		return rosetta.User(extractText(m.Content))
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
		return rosetta.User(extractText(m.Content))
	}
}

func extractText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}

	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}

	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return string(raw)
	}

	result := ""
	for _, p := range parts {
		if p.Type == "text" {
			result += p.Text
		}
	}
	return result
}
