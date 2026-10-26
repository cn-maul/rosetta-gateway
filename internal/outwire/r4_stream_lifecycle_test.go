package outwire

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/cn-maul/rosetta"
)

func TestR4_AnthropicTerminalOnlyStartsMessage(t *testing.T) {
	rec := httptest.NewRecorder()
	s := NewAnthropicSSE(rec, nil, "msg_1", "gw-model")

	s.Event(&rosetta.Event{Type: rosetta.EventMessageEnd, Usage: &rosetta.Usage{}})
	s.Finish("ok", rosetta.StopEnd, rosetta.Usage{}, false)

	names := eventNames(collect(t, rec.Body.String()))
	if len(names) < 3 || names[0] != "message_start" || names[len(names)-2] != "message_delta" || names[len(names)-1] != "message_stop" {
		t.Fatalf("terminal-only event sequence = %v", names)
	}
}

func TestR4_ResponsesTerminalOnlyStartsResponse(t *testing.T) {
	rec := httptest.NewRecorder()
	s := NewResponsesSSE(rec, nil, "resp_1", "gw-model", nil)

	s.Event(&rosetta.Event{Type: rosetta.EventMessageEnd, Usage: &rosetta.Usage{}})
	s.Finish("ok", rosetta.StopEnd, rosetta.Usage{}, false)

	names := eventNames(collect(t, rec.Body.String()))
	if len(names) < 2 || names[0] != "response.created" || names[len(names)-1] != "response.completed" {
		t.Fatalf("terminal-only event sequence = %v", names)
	}
}

func TestR4_ChatNonStreamKeepsTextWithToolCalls(t *testing.T) {
	rec := httptest.NewRecorder()
	result := WriteNonStreamResponse(rec, &rosetta.ChatResponse{
		ID: "resp_1",
		Content: []rosetta.Block{
			{Type: rosetta.BlockText, Text: "I checked the weather."},
			{Type: rosetta.BlockToolCall, ToolCallID: "call_1", ToolName: "weather", Arguments: `{"city":"Beijing"}`},
		},
		StopReason: rosetta.StopToolUse,
		Usage:      rosetta.Usage{InputTokens: 3, OutputTokens: 4, TotalTokens: 7},
	}, "gw-model")
	if result.Err != nil {
		t.Fatalf("write response: %v", result.Err)
	}

	var body struct {
		Choices []struct {
			Message struct {
				Content   json.RawMessage `json:"content"`
				ToolCalls []any           `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	msg := body.Choices[0].Message
	if string(msg.Content) != `"I checked the weather."` || len(msg.ToolCalls) != 1 {
		t.Fatalf("text or tool calls lost: content=%s tools=%d", msg.Content, len(msg.ToolCalls))
	}
}
