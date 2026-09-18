package inwire

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/cn-maul/rosetta"
)

type AnthropicRequest struct {
	Model         string            `json:"model"`
	Messages      []AnthropicMsg    `json:"messages"`
	System        any               `json:"system,omitempty"`
	MaxTokens     int               `json:"max_tokens"`
	Temperature   *float64          `json:"temperature,omitempty"`
	TopP          *float64          `json:"top_p,omitempty"`
	TopK          *int              `json:"top_k,omitempty"`
	StopSequences []string          `json:"stop_sequences,omitempty"`
	Stream        bool              `json:"stream,omitempty"`
	Tools         []AnthropicTool   `json:"tools,omitempty"`
	ToolChoice    any               `json:"tool_choice,omitempty"`
	Metadata      map[string]any    `json:"metadata,omitempty"`
}

type AnthropicMsg struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type AnthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

func DecodeAnthropicRequest(r *http.Request) (*AnthropicRequest, error) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 32*1024*1024))
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}

	var req AnthropicRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, fmt.Errorf("decode body: %w", err)
	}

	if req.Model == "" {
		return nil, fmt.Errorf("model is required")
	}
	if len(req.Messages) == 0 {
		return nil, fmt.Errorf("messages is required")
	}
	if req.MaxTokens == 0 {
		req.MaxTokens = 4096
	}

	return &req, nil
}

func (r *AnthropicRequest) ToRosetta() *rosetta.ChatRequest {
	msgs := make([]rosetta.Message, 0, len(r.Messages))
	for _, m := range r.Messages {
		msgs = append(msgs, convertAnthropicMessage(m))
	}

	req := &rosetta.ChatRequest{
		Model:           r.Model,
		Messages:        msgs,
		MaxOutputTokens: r.MaxTokens,
		Temperature:     r.Temperature,
		TopP:            r.TopP,
		StopSequences:   r.StopSequences,
	}

	if r.System != nil {
		switch v := r.System.(type) {
		case string:
			req.System = v
		case []any:
			text := ""
			for _, block := range v {
				if m, ok := block.(map[string]any); ok {
					if t, ok := m["text"].(string); ok {
						text += t
					}
				}
			}
			req.System = text
		}
	}

	if len(r.Tools) > 0 {
		tools := make([]rosetta.ToolDefinition, 0, len(r.Tools))
		for _, t := range r.Tools {
			tools = append(tools, rosetta.ToolDefinition{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  t.InputSchema,
			})
		}
		req.Tools = tools
	}

	if r.ToolChoice != nil {
		if req.Extra == nil {
			req.Extra = make(map[string]any)
		}
		req.Extra["tool_choice"] = r.ToolChoice
	}

	return req
}

func convertAnthropicMessage(m AnthropicMsg) rosetta.Message {
	switch m.Role {
	case "user":
		text := extractAnthropicContent(m.Content)
		return rosetta.User(text)
	case "assistant":
		blocks := extractAnthropicBlocks(m.Content)
		if len(blocks) == 1 && blocks[0].Type == rosetta.BlockText {
			return rosetta.Assistant(blocks[0].Text)
		}
		return rosetta.AssistantBlocks(blocks...)
	default:
		return rosetta.User(extractAnthropicContent(m.Content))
	}
}

func extractAnthropicContent(raw json.RawMessage) string {
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

func extractAnthropicBlocks(raw json.RawMessage) []rosetta.Block {
	var parts []struct {
		Type      string `json:"type"`
		Text      string `json:"text"`
		Thinking  string `json:"thinking"`
		Signature string `json:"signature"`
		Name      string `json:"name"`
		ID        string `json:"id"`
		Content   string `json:"content"`
		ToolUseID string `json:"tool_use_id"`
		IsError   bool   `json:"is_error"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return []rosetta.Block{{Type: rosetta.BlockText, Text: string(raw)}}
	}

	var blocks []rosetta.Block
	for _, p := range parts {
		switch p.Type {
		case "text":
			blocks = append(blocks, rosetta.Block{Type: rosetta.BlockText, Text: p.Text})
		case "thinking":
			blocks = append(blocks, rosetta.Thinking(p.Thinking, p.Signature))
		case "tool_use":
			blocks = append(blocks, rosetta.ToolCall(p.ID, p.Name, p.Content))
		case "tool_result":
			blocks = append(blocks, rosetta.Block{Type: rosetta.BlockToolResult, ToolCallID: p.ToolUseID, Content: p.Content, IsError: p.IsError})
		}
	}
	return blocks
}
