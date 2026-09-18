package outwire

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/cn-maul/rosetta"
)

type OpenAIResponseOut struct {
	ID         string                  `json:"id"`
	Object     string                  `json:"object"`
	Status     string                  `json:"status"`
	Output     []OpenAIResponseOutItem `json:"output"`
	Usage      *OpenAIUsage            `json:"usage"`
	CreatedAt  int64                   `json:"created_at"`
	Model      string                  `json:"model"`
}

type OpenAIResponseOutItem struct {
	Type      string                   `json:"type"`
	ID        string                   `json:"id,omitempty"`
	Name      string                   `json:"name,omitempty"`
	Arguments string                   `json:"arguments,omitempty"`
	Content   []OpenAIResponseOutBlock `json:"content,omitempty"`
}

type OpenAIResponseOutBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type OpenAIResponsesSSEWriter struct {
	w       io.Writer
	flusher http.Flusher
}

func NewOpenAIResponsesSSEWriter(w io.Writer, flusher http.Flusher) *OpenAIResponsesSSEWriter {
	return &OpenAIResponsesSSEWriter{w: w, flusher: flusher}
}

func (sw *OpenAIResponsesSSEWriter) WriteEvent(event, data string) error {
	if event != "" {
		fmt.Fprintf(sw.w, "event: %s\n", event)
	}
	fmt.Fprintf(sw.w, "data: %s\n\n", data)
	if sw.flusher != nil {
		sw.flusher.Flush()
	}
	return nil
}

func (sw *OpenAIResponsesSSEWriter) WriteCreated(id, model string) error {
	data := map[string]any{
		"type":     "response.created",
		"response": map[string]any{
			"id":         id,
			"object":     "response",
			"status":     "in_progress",
			"output":     []any{},
			"created_at": time.Now().Unix(),
			"model":      model,
		},
	}
	b, _ := json.Marshal(data)
	return sw.WriteEvent("response.created", string(b))
}

func (sw *OpenAIResponsesSSEWriter) WriteInProgress(id string) error {
	data := map[string]any{
		"type":     "response.in_progress",
		"response": map[string]string{"id": id},
	}
	b, _ := json.Marshal(data)
	return sw.WriteEvent("response.in_progress", string(b))
}

func (sw *OpenAIResponsesSSEWriter) WriteOutputItemAdded(index int, item map[string]any) error {
	data := map[string]any{
		"type":           "response.output_item.added",
		"output_index":   index,
		"output_item":    item,
	}
	b, _ := json.Marshal(data)
	return sw.WriteEvent("response.output_item.added", string(b))
}

func (sw *OpenAIResponsesSSEWriter) WriteContentPartAdded(outputIndex, contentIndex int, part map[string]any) error {
	data := map[string]any{
		"type":             "response.content_part.added",
		"output_index":     outputIndex,
		"content_index":    contentIndex,
		"part":             part,
	}
	b, _ := json.Marshal(data)
	return sw.WriteEvent("response.content_part.added", string(b))
}

func (sw *OpenAIResponsesSSEWriter) WriteTextDelta(outputIndex, contentIndex int, delta string) error {
	data := map[string]any{
		"type":          "response.output_text.delta",
		"output_index":  outputIndex,
		"content_index": contentIndex,
		"delta":         delta,
	}
	b, _ := json.Marshal(data)
	return sw.WriteEvent("response.output_text.delta", string(b))
}

func (sw *OpenAIResponsesSSEWriter) WriteTextDone(outputIndex, contentIndex int, text string) error {
	data := map[string]any{
		"type":          "response.output_text.done",
		"output_index":  outputIndex,
		"content_index": contentIndex,
		"text":          text,
	}
	b, _ := json.Marshal(data)
	return sw.WriteEvent("response.output_text.done", string(b))
}

func (sw *OpenAIResponsesSSEWriter) WriteContentPartDone(outputIndex, contentIndex int) error {
	data := map[string]any{
		"type":          "response.content_part.done",
		"output_index":  outputIndex,
		"content_index": contentIndex,
	}
	b, _ := json.Marshal(data)
	return sw.WriteEvent("response.content_part.done", string(b))
}

func (sw *OpenAIResponsesSSEWriter) WriteOutputItemDone(index int) error {
	data := map[string]any{
		"type":         "response.output_item.done",
		"output_index": index,
	}
	b, _ := json.Marshal(data)
	return sw.WriteEvent("response.output_item.done", string(b))
}

func (sw *OpenAIResponsesSSEWriter) WriteCompleted(id string, resp *rosetta.ChatResponse, model string) error {
	outputItems := []OpenAIResponseOutItem{}
	if text := resp.Text(); text != "" {
		outputItems = append(outputItems, OpenAIResponseOutItem{
			Type: "message",
			ID:   id,
			Content: []OpenAIResponseOutBlock{
				{Type: "output_text", Text: text},
			},
		})
	}

	completed := map[string]any{
		"type":     "response.completed",
		"response": OpenAIResponseOut{
			ID:        id,
			Object:    "response",
			Status:    "completed",
			Output:    outputItems,
			Model:     model,
			CreatedAt: time.Now().Unix(),
			Usage: &OpenAIUsage{
				PromptTokens:     resp.Usage.InputTokens,
				CompletionTokens: resp.Usage.OutputTokens,
				TotalTokens:      resp.Usage.TotalTokens,
			},
		},
	}
	b, _ := json.Marshal(completed)
	return sw.WriteEvent("response.completed", string(b))
}

func (sw *OpenAIResponsesSSEWriter) WriteDone() error {
	return sw.WriteEvent("", "[DONE]")
}

func WriteNonStreamOpenAIResponse(w http.ResponseWriter, resp *rosetta.ChatResponse, model string) {
	outputItems := []OpenAIResponseOutItem{}
	if text := resp.Text(); text != "" {
		outputItems = append(outputItems, OpenAIResponseOutItem{
			Type: "message",
			ID:   resp.ID,
			Content: []OpenAIResponseOutBlock{
				{Type: "output_text", Text: text},
			},
		})
	}

	out := OpenAIResponseOut{
		ID:        resp.ID,
		Object:    "response",
		Status:    "completed",
		Output:    outputItems,
		Model:     model,
		CreatedAt: time.Now().Unix(),
		Usage: &OpenAIUsage{
			PromptTokens:     resp.Usage.InputTokens,
			CompletionTokens: resp.Usage.OutputTokens,
			TotalTokens:      resp.Usage.TotalTokens,
		},
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}
