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
