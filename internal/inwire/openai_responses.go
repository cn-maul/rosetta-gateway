package inwire

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/cn-maul/rosetta"
)

type OpenAIResponsesRequest struct {
	Model          string              `json:"model"`
	Messages       []OpenAIMessage     `json:"-"`
	Instructions   string              `json:"instructions,omitempty"`
	MaxOutputTokens *int               `json:"max_output_tokens,omitempty"`
	Temperature    *float64            `json:"temperature,omitempty"`
	TopP           *float64            `json:"top_p,omitempty"`
	Stop           []string            `json:"stop,omitempty"`
	Stream         bool                `json:"stream,omitempty"`
	Tools          []OpenAITool        `json:"tools,omitempty"`
	ToolChoice     any                 `json:"tool_choice,omitempty"`
	PreviousResponseID string          `json:"previous_response_id,omitempty"`
	Store          *bool               `json:"store,omitempty"`
	Metadata       map[string]any      `json:"metadata,omitempty"`
}

type OpenAIResponseOutput struct {
	ID      string                     `json:"id"`
	Object  string                     `json:"object"`
	Status  string                     `json:"status"`
	Output  []OpenAIResponseOutputItem `json:"output,omitempty"`
	Usage   *Usage                     `json:"usage,omitempty"`
}

type Usage struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	TotalTokens      int64 `json:"total_tokens"`
}

type OpenAIResponseOutputItem struct {
	Type  string          `json:"type"`
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Arguments string      `json:"arguments,omitempty"`
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content,omitempty"`
}

func DecodeOpenAIResponsesRequest(r *http.Request) (*OpenAIResponsesRequest, error) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 32*1024*1024))
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}

	var req OpenAIResponsesRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, fmt.Errorf("decode body: %w", err)
	}

	if req.Model == "" {
		return nil, fmt.Errorf("model is required")
	}

	return &req, nil
}

func (r *OpenAIResponsesRequest) ToRosetta() *rosetta.ChatRequest {
	req := &rosetta.ChatRequest{
		Model: r.Model,
	}

	if r.Instructions != "" {
		req.System = r.Instructions
	}

	if r.MaxOutputTokens != nil {
		req.MaxOutputTokens = *r.MaxOutputTokens
	}
	req.Temperature = r.Temperature
	req.TopP = r.TopP
	req.StopSequences = r.Stop

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

	if r.PreviousResponseID != "" {
		if req.Extra == nil {
			req.Extra = make(map[string]any)
		}
		req.Extra["previous_response_id"] = r.PreviousResponseID
	}
	if r.Store != nil {
		if req.Extra == nil {
			req.Extra = make(map[string]any)
		}
		req.Extra["store"] = *r.Store
	}
	if r.Metadata != nil {
		if req.Extra == nil {
			req.Extra = make(map[string]any)
		}
		req.Extra["metadata"] = r.Metadata
	}

	return req
}
