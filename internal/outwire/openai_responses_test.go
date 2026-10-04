package outwire

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cn-maul/rosetta"
)

// responses 事件序列：created → (item.added → part.added → text.delta →
// part.done → item.done) → completed（带 usage 与完整文本）。
func TestResponsesSSE_TextFlow(t *testing.T) {
	rec := httptest.NewRecorder()
	s := NewResponsesSSE(rec, nil, "resp_1", "gw-model")

	s.Event(&rosetta.Event{Type: rosetta.EventMessageStart, ID: "up-1"})
	s.Event(&rosetta.Event{Type: rosetta.EventTextDelta, Text: "你好"})
	s.Event(&rosetta.Event{Type: rosetta.EventTextDelta, Text: "，世界"})
	s.Event(&rosetta.Event{Type: rosetta.EventMessageEnd, Usage: &rosetta.Usage{InputTokens: 7, OutputTokens: 3}})
	s.Finish("ok", rosetta.StopEnd, rosetta.Usage{InputTokens: 7, OutputTokens: 3, TotalTokens: 10}, false)

	raw := rec.Body.String()
	names := eventNames(collect(t, raw))
	want := []string{
		"response.created", "response.output_item.added", "response.content_part.added",
		"response.output_text.delta", "response.output_text.delta",
		"response.output_text.done", "response.content_part.done", "response.output_item.done",
		"response.completed",
	}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("event sequence = %v\nraw:\n%s", names, raw)
	}
	// completed 事件内嵌完整 response（文本 + usage）。
	completed := names[len(names)-1]
	_ = completed
	var done struct {
		Response struct {
			Status string `json:"status"`
			Model  string `json:"model"`
			Output []struct {
				Type    string `json:"type"`
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"output"`
			Usage struct {
				TotalTokens int64 `json:"total_tokens"`
			} `json:"usage"`
		} `json:"response"`
	}
	last := collect(t, raw)[len(names)-1]
	if err := json.Unmarshal([]byte(last[1]), &done); err != nil {
		t.Fatalf("parse completed: %v", err)
	}
	if done.Response.Status != "completed" || done.Response.Model != "gw-model" ||
		len(done.Response.Output) != 1 || done.Response.Output[0].Content[0].Text != "你好，世界" ||
		done.Response.Usage.TotalTokens != 10 {
		t.Fatalf("completed payload: %+v", done.Response)
	}
	// 增量事件带 OpenAI 规范的 type 字段（与 event: 行同名）。
	if !strings.Contains(raw, `"type":"response.output_text.delta"`) {
		t.Fatalf("payload type field missing:\n%s", raw)
	}
}

// 工具调用：function_call item + arguments delta/done；断流发 response.failed。
func TestResponsesSSE_ToolCallAndFailure(t *testing.T) {
	rec := httptest.NewRecorder()
	s := NewResponsesSSE(rec, nil, "resp_1", "gw-model")
	s.Event(&rosetta.Event{Type: rosetta.EventMessageStart, ID: "up-1"})
	s.Event(&rosetta.Event{Type: rosetta.EventToolCall, ToolID: "c1", ToolName: "f", ArgumentsDelta: `{"a":`})
	s.Event(&rosetta.Event{Type: rosetta.EventToolCall, ToolID: "c1", ToolName: "f", ArgumentsDelta: `1}`})
	s.Finish("truncated", "", rosetta.Usage{}, false)

	raw := rec.Body.String()
	names := eventNames(collect(t, raw))
	if names[len(names)-1] != "response.failed" {
		t.Fatalf("last event should be response.failed, got %v", names)
	}
	if !strings.Contains(raw, `"call_id":"c1"`) {
		t.Fatalf("tool call payload missing:\n%s", raw)
	}
	// arguments 在线格式上是 JSON 字符串（OpenAI 规范）：解析 output_item.done
	// 后断言其值等于 {"a":1}，避免子串断言对转义形态敏感。
	var doneItem struct {
		Item struct {
			Arguments string `json:"arguments"`
		} `json:"item"`
	}
	for _, e := range collect(t, raw) {
		if e[0] == "response.output_item.done" {
			if err := json.Unmarshal([]byte(e[1]), &doneItem); err == nil && doneItem.Item.Arguments == `{"a":1}` {
				break
			}
		}
	}
	if doneItem.Item.Arguments != `{"a":1}` {
		t.Fatalf("arguments done payload wrong: %+v", doneItem.Item)
	}
	for _, n := range names {
		if n == "response.completed" {
			t.Fatalf("truncated stream must not complete")
		}
	}
}

// 非流式对象：output 块序、status/usage、tool input 对象化。
func TestWriteResponsesResponse(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteResponsesResponse(rec, &rosetta.ChatResponse{
		ID: "up-9", Model: "x",
		Content: []rosetta.Block{
			{Type: rosetta.BlockText, Text: "答"},
			{Type: rosetta.BlockToolCall, ToolCallID: "c1", ToolName: "f", Arguments: `{"a":1}`},
			{Type: rosetta.BlockToolCall, ToolCallID: "c2", ToolName: "g"},
		},
		StopReason: rosetta.StopLength,
		Usage:      rosetta.Usage{InputTokens: 5, OutputTokens: 6, TotalTokens: 11},
	}, "gw-model")

	var out struct {
		ID       string `json:"id"`
		Object   string `json:"object"`
		Status   string `json:"status"`
		Model    string `json:"model"`
		Incomplete *struct {
			Reason string `json:"reason"`
		} `json:"incomplete_details"`
		Output []struct {
			Type      string `json:"type"`
			CallID    string `json:"call_id"`
			Arguments string `json:"arguments"`
			Content   []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
		Usage struct {
			InputTokens  int64 `json:"input_tokens"`
			OutputTokens int64 `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, rec.Body.String())
	}
	if out.Object != "response" || out.Status != "incomplete" || out.Incomplete.Reason != "max_output_tokens" {
		t.Fatalf("envelope: %+v", out)
	}
	if len(out.Output) != 3 || out.Output[0].Content[0].Text != "答" ||
		out.Output[1].CallID != "c1" || out.Output[1].Arguments != `{"a":1}` ||
		out.Output[2].Arguments != `{}` {
		t.Fatalf("output items: %+v", out.Output)
	}
	if out.Usage.InputTokens != 5 || out.Usage.OutputTokens != 6 {
		t.Fatalf("usage: %+v", out.Usage)
	}
}
