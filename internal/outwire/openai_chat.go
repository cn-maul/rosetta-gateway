package outwire

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/cn-maul/rosetta"
)

type OpenAIChatResponse struct {
	ID      string                `json:"id"`
	Object  string                `json:"object"`
	Created int64                 `json:"created"`
	Model   string                `json:"model"`
	Choices []OpenAIChatChoice    `json:"choices"`
	Usage   *OpenAIUsage          `json:"usage,omitempty"`
}

type OpenAIChatChoice struct {
	Index        int              `json:"index"`
	Message      *OpenAIMessage   `json:"message,omitempty"`
	Delta        *OpenAIMessage   `json:"delta,omitempty"`
	FinishReason *string          `json:"finish_reason"`
}

type OpenAIMessage struct {
	Role     string          `json:"role"`
	Content  json.RawMessage `json:"content,omitempty"`
	ToolCalls json.RawMessage `json:"tool_calls,omitempty"`
}

type OpenAIToolCall struct {
	ID       string        `json:"id"`
	Type     string        `json:"type"`
	Function OpenAIFunction `json:"function"`
}

type OpenAIFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type OpenAIUsage struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	TotalTokens      int64 `json:"total_tokens"`
}

type OpenAIUsageChunk struct {
	Choices []struct{}      `json:"choices"`
	Usage   *OpenAIUsage    `json:"usage"`
}

type SSEWriter struct {
	w       io.Writer
	flusher http.Flusher
	flushed bool
}

func NewSSEWriter(w io.Writer, flusher http.Flusher) *SSEWriter {
	return &SSEWriter{w: w, flusher: flusher}
}

func (sw *SSEWriter) WriteEvent(event, data string) error {
	if event != "" {
		fmt.Fprintf(sw.w, "event: %s\n", event)
	}
	fmt.Fprintf(sw.w, "data: %s\n\n", data)
	if sw.flusher != nil {
		sw.flusher.Flush()
	}
	sw.flushed = true
	return nil
}

func (sw *SSEWriter) WriteTextDelta(delta string) error {
	chunk := map[string]any{
		"id":     "",
		"object": "chat.completion.chunk",
		"choices": []map[string]any{{
			"index": 0,
			"delta": map[string]string{"content": delta},
		}},
	}
	data, _ := json.Marshal(chunk)
	return sw.WriteEvent("", string(data))
}

func (sw *SSEWriter) WriteToolCallDelta(index int, id, name, argsDelta string) error {
	delta := map[string]any{
		"index": index,
	}
	if id != "" {
		delta["id"] = id
		delta["type"] = "function"
	}
	if name != "" {
		delta["function"] = map[string]string{"name": name, "arguments": argsDelta}
	} else if argsDelta != "" {
		delta["function"] = map[string]string{"arguments": argsDelta}
	}
	chunk := map[string]any{
		"object": "chat.completion.chunk",
		"choices": []map[string]any{{
			"index": 0,
			"delta": map[string]any{"tool_calls": []map[string]any{delta}},
		}},
	}
	data, _ := json.Marshal(chunk)
	return sw.WriteEvent("", string(data))
}

func (sw *SSEWriter) WriteUsage(u rosetta.Usage, model string) error {
	resp := OpenAIUsageChunk{
		Usage: &OpenAIUsage{
			PromptTokens:     u.InputTokens,
			CompletionTokens: u.OutputTokens,
			TotalTokens:      u.TotalTokens,
		},
	}
	data, _ := json.Marshal(resp)
	return sw.WriteEvent("", string(data))
}

func (sw *SSEWriter) WriteDone() error {
	return sw.WriteEvent("", "[DONE]")
}

func WriteNonStreamResponse(w http.ResponseWriter, resp *rosetta.ChatResponse, model string) {
	choices := make([]OpenAIChatChoice, 0)
	finishReason := string(resp.StopReason)
	if resp.StopReason == rosetta.StopEnd {
		finishReason = "stop"
	}

	msg := &OpenAIMessage{
		Role:    "assistant",
		Content: json.RawMessage(`"` + escapeJSON(resp.Text()) + `"`),
	}
	if len(resp.ToolCalls()) > 0 {
		tcs := make([]OpenAIToolCall, 0, len(resp.ToolCalls()))
		for _, tc := range resp.ToolCalls() {
			tcs = append(tcs, OpenAIToolCall{
				ID:   tc.ToolCallID,
				Type: "function",
				Function: OpenAIFunction{
					Name:      tc.ToolName,
					Arguments: tc.Arguments,
				},
			})
		}
		raw, _ := json.Marshal(tcs)
		msg.ToolCalls = raw
		msg.Content = nil
	}

	choices = append(choices, OpenAIChatChoice{
		Index:        0,
		Message:      msg,
		FinishReason: &finishReason,
	})

	usage := &OpenAIUsage{
		PromptTokens:     resp.Usage.InputTokens,
		CompletionTokens: resp.Usage.OutputTokens,
		TotalTokens:      resp.Usage.TotalTokens,
	}

	out := OpenAIChatResponse{
		ID:      resp.ID,
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   model,
		Choices: choices,
		Usage:   usage,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

func escapeJSON(s string) string {
	b, _ := json.Marshal(s)
	return string(b[1 : len(b)-1])
}
