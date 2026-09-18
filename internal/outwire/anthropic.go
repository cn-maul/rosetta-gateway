package outwire

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/cn-maul/rosetta"
)

type AnthropicResponse struct {
	ID           string              `json:"id"`
	Type         string              `json:"type"`
	Role         string              `json:"role"`
	Content      []AnthropicBlock    `json:"content"`
	Model        string              `json:"model"`
	StopReason   string              `json:"stop_reason"`
	StopSequence *string             `json:"stop_sequence,omitempty"`
	Usage        *AnthropicUsage     `json:"usage"`
}

type AnthropicBlock struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

type AnthropicUsage struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
}

type AnthropicSSEWriter struct {
	w       io.Writer
	flusher http.Flusher
}

func NewAnthropicSSEWriter(w io.Writer, flusher http.Flusher) *AnthropicSSEWriter {
	return &AnthropicSSEWriter{w: w, flusher: flusher}
}

func (sw *AnthropicSSEWriter) WriteEvent(event, data string) error {
	if event != "" {
		fmt.Fprintf(sw.w, "event: %s\n", event)
	}
	fmt.Fprintf(sw.w, "data: %s\n\n", data)
	if sw.flusher != nil {
		sw.flusher.Flush()
	}
	return nil
}

func (sw *AnthropicSSEWriter) WriteMessageStart(id, model string) error {
	data := map[string]any{
		"type":  "message_start",
		"message": map[string]any{
			"id":         id,
			"type":       "message",
			"role":       "assistant",
			"content":    []any{},
			"model":      model,
			"stop_reason": nil,
			"usage":      map[string]int64{"input_tokens": 0, "output_tokens": 0},
		},
	}
	b, _ := json.Marshal(data)
	return sw.WriteEvent("message_start", string(b))
}

func (sw *AnthropicSSEWriter) WriteContentBlockStart(index int, blockType string) error {
	data := map[string]any{
		"type":  "content_block_start",
		"index": index,
		"content_block": map[string]string{
			"type": blockType,
		},
	}
	b, _ := json.Marshal(data)
	return sw.WriteEvent("content_block_start", string(b))
}

func (sw *AnthropicSSEWriter) WriteContentBlockDelta(index int, text string) error {
	data := map[string]any{
		"type":  "content_block_delta",
		"index": index,
		"delta": map[string]string{
			"type": "text_delta",
			"text": text,
		},
	}
	b, _ := json.Marshal(data)
	return sw.WriteEvent("content_block_delta", string(b))
}

func (sw *AnthropicSSEWriter) WriteContentBlockStop(index int) error {
	data := map[string]any{
		"type":  "content_block_stop",
		"index": index,
	}
	b, _ := json.Marshal(data)
	return sw.WriteEvent("content_block_stop", string(b))
}

func (sw *AnthropicSSEWriter) WriteMessageDelta(stopReason string, usage AnthropicUsage) error {
	data := map[string]any{
		"type": "message_delta",
		"delta": map[string]string{
			"stop_reason": stopReason,
		},
		"usage": map[string]int64{
			"output_tokens": usage.OutputTokens,
		},
	}
	b, _ := json.Marshal(data)
	return sw.WriteEvent("message_delta", string(b))
}

func (sw *AnthropicSSEWriter) WriteMessageStop() error {
	data := map[string]string{"type": "message_stop"}
	b, _ := json.Marshal(data)
	return sw.WriteEvent("message_stop", string(b))
}

func (sw *AnthropicSSEWriter) WritePing() error {
	return sw.WriteEvent("ping", "{}")
}

func (sw *AnthropicSSEWriter) WriteError(errType, message string) error {
	data := map[string]any{
		"type": "error",
		"error": map[string]string{
			"type":    errType,
			"message": message,
		},
	}
	b, _ := json.Marshal(data)
	return sw.WriteEvent("error", string(b))
}

func WriteNonStreamAnthropicResponse(w http.ResponseWriter, resp *rosetta.ChatResponse, model string) {
	content := []AnthropicBlock{}
	if text := resp.Text(); text != "" {
		content = append(content, AnthropicBlock{Type: "text", Text: text})
	}

	stopReason := "end_turn"
	switch resp.StopReason {
	case rosetta.StopLength:
		stopReason = "max_tokens"
	case rosetta.StopToolUse:
		stopReason = "tool_use"
	}

	out := AnthropicResponse{
		ID:         resp.ID,
		Type:       "message",
		Role:       "assistant",
		Content:    content,
		Model:      model,
		StopReason: stopReason,
		Usage: &AnthropicUsage{
			InputTokens:  resp.Usage.InputTokens,
			OutputTokens: resp.Usage.OutputTokens,
		},
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

type AnthropicErrorBody struct {
	Type  string          `json:"type"`
	Error *ErrorDetail    `json:"error"`
}

func WriteAnthropicErrorResponse(w http.ResponseWriter, statusCode int, errType, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	json.NewEncoder(w).Encode(AnthropicErrorBody{
		Type: "error",
		Error: &ErrorDetail{
			Type:    errType,
			Message: message,
		},
	})
}

func MapAnthropicError(err error) (int, string, string) {
	if err == nil {
		return 200, "", ""
	}

	statusCode, code, message := MapUpstreamError(err)

	switch code {
	case "invalid_api_key":
		return 401, "authentication_error", message
	case "rate_limit_exceeded":
		return 429, "rate_limit_error", message
	case "model_not_found":
		return 404, "not_found_error", message
	case "context_length_exceeded":
		return 400, "invalid_request_error", message
	case "invalid_request_error":
		return statusCode, "invalid_request_error", message
	default:
		return 500, "api_error", message
	}
}

var _ = time.Now
