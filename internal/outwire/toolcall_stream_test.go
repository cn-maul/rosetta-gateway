package outwire

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/cn-maul/rosetta"
)

// 工具调用的参数通常跨多个 SSE 分片。rosetta 的契约是：
// **首片**带 ToolID/ToolName，**续片只带 ToolIndex + ArgumentsDelta**
// （已在 SDK v0.6.0 的三个 provider 里逐一核实：anthropic 的
// input_json_delta、openai-chat 的续 chunk、responses 的
// function_call_arguments.delta 都不填 ToolID/ToolName）。
//
// sink 若用 ToolID 判「是否同一个工具块」，续片的 ToolID 为空就会切出新块，
// 于是客户端收到两个 tool_use：一个有 id/name 但参数为空，一个有参数但
// 没有 id/name —— 工具调用整体失效。
func TestAnthropicSink_MultiPartToolArgsStayInOneBlock(t *testing.T) {
	var sb strings.Builder
	s := NewAnthropicSSE(nopWriter{&sb}, nopWriter{&sb}, "msg_1", "m1")

	// 首片：有 id/name，无参数。
	s.Event(&rosetta.Event{Type: rosetta.EventToolCall, ToolIndex: 0, ToolID: "toolu_1", ToolName: "get_weather"})
	// 续片1：只有 ToolIndex + 参数片段（SDK 实际行为）。
	s.Event(&rosetta.Event{Type: rosetta.EventToolCall, ToolIndex: 0, ArgumentsDelta: `{"city":`})
	// 续片2：同一工具继续。
	s.Event(&rosetta.Event{Type: rosetta.EventToolCall, ToolIndex: 0, ArgumentsDelta: `"SF"}`})
	s.Finish("ok", rosetta.StopToolUse, rosetta.Usage{}, false)

	body := sb.String()
	var starts, stops int
	var toolStart string
	var jsonParts []string
	var blockIdx []string

	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var ev map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev); err != nil {
			t.Fatalf("事件不是合法 JSON: %q", line)
		}
		switch ev["type"] {
		case "content_block_start":
			starts++
			blockIdx = append(blockIdx, fmt.Sprint(ev["index"]))
			if cb, ok := ev["content_block"].(map[string]any); ok && cb["type"] == "tool_use" {
				toolStart = string(mustJSON(t, cb))
			}
		case "content_block_stop":
			stops++
		case "content_block_delta":
			if d, ok := ev["delta"].(map[string]any); ok && d["type"] == "input_json_delta" {
				jsonParts = append(jsonParts, d["partial_json"].(string))
			}
		}
	}

	if starts != 1 {
		t.Errorf("content_block_start 出现 %d 次（期望 1）：参数分片把工具调用切成了多个块", starts)
	}
	if stops != 1 {
		t.Errorf("content_block_stop 出现 %d 次（期望 1）", stops)
	}
	if !strings.Contains(toolStart, "toolu_1") || !strings.Contains(toolStart, "get_weather") {
		t.Errorf("tool_use 块缺少 id/name：%s", toolStart)
	}
	// 三段参数必须落进同一个块并能拼回合法 JSON。
	joined := strings.Join(jsonParts, "")
	if joined != `{"city":"SF"}` {
		t.Errorf("参数分片拼接结果 = %q，期望 {\"city\":\"SF\"}", joined)
	}
}

func TestResponsesSink_MultiPartToolArgsStayInOneItem(t *testing.T) {
	var sb strings.Builder
	s := NewResponsesSSE(nopWriter{&sb}, nopWriter{&sb}, "resp_1", "m1")

	s.Event(&rosetta.Event{Type: rosetta.EventToolCall, ToolIndex: 0, ToolID: "call_1", ToolName: "f"})
	s.Event(&rosetta.Event{Type: rosetta.EventToolCall, ToolIndex: 0, ArgumentsDelta: `{"a":`})
	s.Event(&rosetta.Event{Type: rosetta.EventToolCall, ToolIndex: 0, ArgumentsDelta: `1}`})
	s.Finish("ok", rosetta.StopToolUse, rosetta.Usage{}, false)

	body := sb.String()
	added, completed := 0, 0
	var deltas []string
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var ev map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev); err != nil {
			continue
		}
		switch ev["type"] {
		case "response.output_item.added":
			added++
		case "response.output_item.done":
			completed++
		case "response.function_call_arguments.delta":
			deltas = append(deltas, ev["delta"].(string))
		}
	}
	if added != 1 {
		t.Errorf("response.output_item.added %d 次（期望 1）：参数分片把一个工具调用拆成了多个 item", added)
	}
	if completed != 1 {
		t.Errorf("response.output_item.done %d 次（期望 1）", completed)
	}
	if got := strings.Join(deltas, ""); got != `{"a":1}` {
		t.Errorf("参数分片拼接 = %q，期望 {\"a\":1}", got)
	}
}

// 反向：两个**不同**的工具必须切成两个块。防止修复时把判据改成
// 「ToolIndex 为空就复用」而让并行工具调用粘在一起。
func TestAnthropicSink_DistinctToolsStillSplit(t *testing.T) {
	var sb strings.Builder
	s := NewAnthropicSSE(nopWriter{&sb}, nopWriter{&sb}, "msg_1", "m1")

	s.Event(&rosetta.Event{Type: rosetta.EventToolCall, ToolIndex: 0, ToolID: "toolu_1", ToolName: "a"})
	s.Event(&rosetta.Event{Type: rosetta.EventToolCall, ToolIndex: 0, ArgumentsDelta: `{}`})
	s.Event(&rosetta.Event{Type: rosetta.EventToolCall, ToolIndex: 1, ToolID: "toolu_2", ToolName: "b"})
	s.Event(&rosetta.Event{Type: rosetta.EventToolCall, ToolIndex: 1, ArgumentsDelta: `{}`})
	s.Finish("ok", rosetta.StopToolUse, rosetta.Usage{}, false)

	starts := strings.Count(sb.String(), `"content_block_start"`)
	if starts != 2 {
		t.Errorf("两个不同工具应切成 2 个块，实际 content_block_start %d 次", starts)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

// nopWriter 收集事件文本。
type nopWriter struct{ sb *strings.Builder }

func (w nopWriter) Write(p []byte) (int, error) { return w.sb.Write(p) }
func (w nopWriter) Flush()                      {}

var _ http.Flusher = nopWriter{}
