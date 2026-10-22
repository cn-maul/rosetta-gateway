package outwire

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cn-maul/rosetta"
)

// collectEvents 把 SSE 文本解析成 (事件名, payload) 列表。
func collectEvents(t *testing.T, raw string) [][2]string {
	t.Helper()
	var out [][2]string
	var name string
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "event: "):
			name = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			out = append(out, [2]string{name, strings.TrimPrefix(line, "data: ")})
			name = ""
		}
	}
	return out
}

// TestOpenAISink_RenumbersAnthropicBlockIndex 钉住 A3-P1-1 / 汇总 O1。
//
// Anthropic 上游的 ToolIndex 是**整条消息的 content block 序号**，
// 而不是「第几个工具调用」。修复前它被原样下发给 OpenAI 客户端，
// 于是第一个工具调用出现在 index=1（因为文本块占了 0），客户端的
// tool_call 数组合并逻辑错位。
func TestOpenAISink_RenumbersAnthropicBlockIndex(t *testing.T) {
	var sb strings.Builder
	s := NewOpenAISink(NewSSEWriter(nopWriter{&sb}, nil, "chatcmpl-1", "m", 0))

	// 模拟 Anthropic 上游：index 1 = 第一个 tool_use，index 3 = 第二个。
	s.Event(&rosetta.Event{Type: rosetta.EventToolCall, ToolIndex: 1, ToolID: "toolu_1", ToolName: "get_weather"})
	s.Event(&rosetta.Event{Type: rosetta.EventToolCall, ToolIndex: 1, ArgumentsDelta: `{"city":`})
	s.Event(&rosetta.Event{Type: rosetta.EventToolCall, ToolIndex: 1, ArgumentsDelta: `"SF"}`})
	s.Event(&rosetta.Event{Type: rosetta.EventToolCall, ToolIndex: 3, ToolID: "toolu_2", ToolName: "get_time"})
	s.Event(&rosetta.Event{Type: rosetta.EventToolCall, ToolIndex: 3, ArgumentsDelta: `{}`})
	s.Finish("ok", rosetta.StopToolUse, rosetta.Usage{}, false)

	var indices []int
	for _, e := range collectEvents(t, sb.String()) {
		var chunk struct {
			Choices []struct {
				Delta struct {
					ToolCalls []struct {
						Index int `json:"index"`
					} `json:"tool_calls"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if json.Unmarshal([]byte(e[1]), &chunk) != nil {
			continue
		}
		for _, c := range chunk.Choices {
			for _, tc := range c.Delta.ToolCalls {
				indices = append(indices, tc.Index)
			}
		}
	}
	want := []int{0, 0, 0, 1, 1}
	if len(indices) != len(want) {
		t.Fatalf("tool_call 分片数 = %d，期望 %d（indices=%v）", len(indices), len(want), indices)
	}
	for i, w := range want {
		if indices[i] != w {
			t.Fatalf("tool_call index 序列 = %v，期望 %v（首个工具必须是 0）", indices, want)
		}
	}
}

// TestResponsesSink_ReasoningItemBoundaryByReasoningID 钉住 A3-P1-2 / 汇总 O2。
//
// 修复前 reasoning item 的边界只靠 sealed 载荷判定，于是没有 sealed 的
// 第一个 item 永不收尾，第二个 item 的思考文本串进第一个（summary 变成
// "A1B1"）、第二个 item 从未被创建、它的 sealed 载荷被挂到错误的 id 上。
func TestResponsesSink_ReasoningItemBoundaryByReasoningID(t *testing.T) {
	var sb strings.Builder
	s := NewResponsesSSE(nopWriter{&sb}, nil, "resp_1", "m", nil)

	s.Event(&rosetta.Event{Type: rosetta.EventMessageStart, ID: "up-1"})
	s.Event(&rosetta.Event{Type: rosetta.EventThinkingDelta, Text: "A1", ReasoningID: "rs_1"})
	s.Event(&rosetta.Event{Type: rosetta.EventThinkingDelta, Text: "B1", ReasoningID: "rs_2"})
	s.Event(&rosetta.Event{Type: rosetta.EventThinkingDelta, Sealed: "SEAL_B", SealedBy: string(rosetta.ProtoOpenAIResponses), ReasoningID: "rs_2"})
	s.Finish("ok", rosetta.StopEnd, rosetta.Usage{InputTokens: 1, OutputTokens: 1}, false)

	raw := sb.String()
	// 两个 reasoning item 必须**各自**有一个 added 与一个 done。
	addedIDs := map[string]bool{}
	doneIDs := map[string]bool{}
	for _, e := range collectEvents(t, raw) {
		switch e[0] {
		case "response.output_item.added":
			var ev struct {
				Item struct {
					Type string `json:"type"`
					ID   string `json:"id"`
				} `json:"item"`
			}
			if json.Unmarshal([]byte(e[1]), &ev) == nil && ev.Item.Type == "reasoning" {
				addedIDs[ev.Item.ID] = true
			}
		case "response.output_item.done":
			var ev struct {
				Item struct {
					Type string `json:"type"`
					ID   string `json:"id"`
				} `json:"item"`
			}
			if json.Unmarshal([]byte(e[1]), &ev) == nil && ev.Item.Type == "reasoning" {
				doneIDs[ev.Item.ID] = true
			}
		}
	}
	if !addedIDs["rs_1"] || !addedIDs["rs_2"] {
		t.Fatalf("两个 reasoning item 都必须有 added（修复前 rs_2 从未被创建）: added=%v\n%s", addedIDs, raw)
	}
	if !doneIDs["rs_1"] || !doneIDs["rs_2"] {
		t.Fatalf("两个 reasoning item 都必须有 done: done=%v\n%s", doneIDs, raw)
	}
	if !strings.Contains(raw, `"id":"rs_1"`) || !strings.Contains(raw, `"id":"rs_2"`) {
		t.Fatalf("两个 reasoning id 都必须出现:\n%s", raw)
	}
	// 第一个 item 的 summary 只能是 A1，不能是累计的 A1B1。
	for _, e := range collectEvents(t, raw) {
		if e[0] != "response.output_item.done" {
			continue
		}
		var ev struct {
			Item struct {
				Type    string `json:"type"`
				ID      string `json:"id"`
				Summary []struct {
					Text string `json:"text"`
				} `json:"summary"`
			} `json:"item"`
		}
		if json.Unmarshal([]byte(e[1]), &ev) != nil || ev.Item.Type != "reasoning" {
			continue
		}
		if ev.Item.ID == "rs_1" {
			if len(ev.Item.Summary) != 1 || ev.Item.Summary[0].Text != "A1" {
				t.Fatalf("rs_1 的 summary 被污染为 %+v，期望 A1", ev.Item.Summary)
			}
		}
	}
	// sealed 载荷必须挂在 rs_2 上，不能挂到 rs_1。
	if !strings.Contains(raw, `"encrypted_content":"SEAL_B","id":"rs_2"`) &&
		!strings.Contains(raw, `"id":"rs_2"`) {
		t.Fatalf("sealed 载荷未挂到 rs_2:\n%s", raw)
	}
	if strings.Contains(raw, `"encrypted_content":"SEAL_B","id":"rs_1"`) {
		t.Fatalf("sealed 载荷错挂到 rs_1（跨轮续接状态错配）:\n%s", raw)
	}
}

// TestResponsesSink_TextDoesNotBleedAcrossMessageItems 钉住 A3-P1-3 / 汇总 O3。
//
// 修复前 s.text 跨 item 共享且不清空，第二个 message item 的
// output_text.done 变成 "AAABBB"。
func TestResponsesSink_TextDoesNotBleedAcrossMessageItems(t *testing.T) {
	var sb strings.Builder
	s := NewResponsesSSE(nopWriter{&sb}, nil, "resp_1", "m", nil)

	i0 := 0
	i2 := 2
	s.Event(&rosetta.Event{Type: rosetta.EventTextDelta, Text: "AAA", OutputIndex: &i0})
	s.Event(&rosetta.Event{Type: rosetta.EventToolCall, ToolIndex: 0, ToolID: "c1", ToolName: "f", ArgumentsDelta: `{}`})
	s.Event(&rosetta.Event{Type: rosetta.EventTextDelta, Text: "BBB", OutputIndex: &i2})
	s.Finish("ok", rosetta.StopEnd, rosetta.Usage{}, false)

	var texts []string
	for _, e := range collectEvents(t, sb.String()) {
		if e[0] != "response.output_text.done" {
			continue
		}
		var ev struct {
			Text string `json:"text"`
		}
		if json.Unmarshal([]byte(e[1]), &ev) == nil {
			texts = append(texts, ev.Text)
		}
	}
	if len(texts) != 2 {
		t.Fatalf("output_text.done 出现 %d 次，期望 2（两个 message item）: %v", len(texts), texts)
	}
	if texts[0] != "AAA" || texts[1] != "BBB" {
		t.Fatalf("文本串味：%v，期望 [AAA BBB]", texts)
	}
}

// TestResponsesSink_HonorsUpstreamOutputIndex 钉住 A3-P1-4 / 汇总 O4。
//
// 下游 output_index 必须跟随上游的 item 位置，而不是按到达顺序自分配。
func TestResponsesSink_HonorsUpstreamOutputIndex(t *testing.T) {
	var sb strings.Builder
	s := NewResponsesSSE(nopWriter{&sb}, nil, "resp_1", "m", nil)

	// 上游先给 index 0 的 message，再给 index 2 的 function_call（跳过 1）。
	i0 := 0
	i2 := 2
	s.Event(&rosetta.Event{Type: rosetta.EventTextDelta, Text: "hi", OutputIndex: &i0})
	s.Event(&rosetta.Event{Type: rosetta.EventToolCall, ToolIndex: 0, ToolID: "c1", ToolName: "f", ArgumentsDelta: `{}`, OutputIndex: &i2})
	s.Finish("ok", rosetta.StopEnd, rosetta.Usage{}, false)

	var added []int
	for _, e := range collectEvents(t, sb.String()) {
		if e[0] != "response.output_item.added" {
			continue
		}
		var ev struct {
			OutputIndex int `json:"output_index"`
		}
		if json.Unmarshal([]byte(e[1]), &ev) == nil {
			added = append(added, ev.OutputIndex)
		}
	}
	if len(added) != 2 || added[0] != 0 || added[1] != 2 {
		t.Fatalf("output_index 分配 = %v，期望 [0 2]（必须跟随上游）", added)
	}
}

// TestAnthropicWriteResponse_InvalidToolArguments 钉住 A3-P1-5 / 汇总 O5。
//
// 上游给的 arguments 不是合法 JSON 时，修复前整条响应编码失败，
// 客户端收到 HTTP 200 + 零字节。
func TestAnthropicWriteResponse_InvalidToolArguments(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteAnthropicResponse(rec, &rosetta.ChatResponse{
		ID: "msg_1",
		Content: []rosetta.Block{
			{Type: rosetta.BlockToolCall, ToolCallID: "c1", ToolName: "f", Arguments: "not-json"},
		},
		StopReason: rosetta.StopToolUse,
		Usage:      rosetta.Usage{InputTokens: 1, OutputTokens: 1},
	}, "m")

	if rec.Body.Len() == 0 {
		t.Fatal("响应体为空 —— 非法 arguments 让整条响应报废（200 + 零字节）")
	}
	var out struct {
		Content []struct {
			Type  string         `json:"type"`
			Input map[string]any `json:"input"`
		} `json:"content"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("响应体不是合法 JSON: %v\n%s", err, rec.Body.String())
	}
	if len(out.Content) != 1 || out.Content[0].Type != "tool_use" {
		t.Fatalf("content = %+v", out.Content)
	}
	if out.Content[0].Input == nil {
		t.Fatalf("非法 arguments 必须退化成空对象 {}，实际 %+v", out.Content[0].Input)
	}
}

// TestResponsesUsage_ReasoningTokensReported 钉住 A3-P1-6 / 汇总 O6。
func TestResponsesUsage_ReasoningTokensReported(t *testing.T) {
	// 流式
	var sb strings.Builder
	s := NewResponsesSSE(nopWriter{&sb}, nil, "resp_1", "m", nil)
	s.Event(&rosetta.Event{Type: rosetta.EventTextDelta, Text: "x"})
	s.Finish("ok", rosetta.StopEnd, rosetta.Usage{InputTokens: 5, OutputTokens: 9, TotalTokens: 14, ReasoningTokens: 4}, false)
	if !strings.Contains(sb.String(), `"reasoning_tokens":4`) {
		t.Fatalf("流式 usage 缺 reasoning_tokens:\n%s", sb.String())
	}

	// 非流式
	rec := httptest.NewRecorder()
	WriteResponsesResponse(rec, &rosetta.ChatResponse{
		ID:         "resp_2",
		Content:    []rosetta.Block{{Type: rosetta.BlockText, Text: "x"}},
		StopReason: rosetta.StopEnd,
		Usage:      rosetta.Usage{InputTokens: 5, OutputTokens: 9, TotalTokens: 14, ReasoningTokens: 4},
	}, "m")
	if !strings.Contains(rec.Body.String(), `"reasoning_tokens":4`) {
		t.Fatalf("非流式 usage 缺 reasoning_tokens:\n%s", rec.Body.String())
	}

	// OpenAI Chat 流式
	var sb2 strings.Builder
	sw := NewSSEWriter(nopWriter{&sb2}, nil, "chatcmpl-1", "m", 0)
	if err := sw.WriteUsage(rosetta.Usage{InputTokens: 5, OutputTokens: 9, TotalTokens: 14, ReasoningTokens: 4}); err != nil {
		t.Fatalf("WriteUsage: %v", err)
	}
	if !strings.Contains(sb2.String(), `"reasoning_tokens":4`) {
		t.Fatalf("OpenAI Chat 流式 usage 缺 reasoning_tokens:\n%s", sb2.String())
	}
}

var _ http.ResponseWriter = (*httptest.ResponseRecorder)(nil)
