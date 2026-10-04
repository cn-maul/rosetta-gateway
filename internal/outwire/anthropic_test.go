package outwire

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cn-maul/rosetta"
)

// collect 逐行抽出 SSE 事件，返回 event 名与 data 载荷的配对序列。
func collect(t *testing.T, raw string) [][2]string {
	t.Helper()
	var out [][2]string
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimRight(line, "\r")
		switch {
		case strings.HasPrefix(line, "event: "):
			out = append(out, [2]string{strings.TrimPrefix(line, "event: "), ""})
		case strings.HasPrefix(line, "data: "):
			if len(out) == 0 {
				t.Fatalf("data before event: %q", line)
			}
			out[len(out)-1][1] = strings.TrimPrefix(line, "data: ")
		}
	}
	return out
}

func eventNames(events [][2]string) []string {
	names := make([]string, len(events))
	for i, e := range events {
		names[i] = e[0]
	}
	return names
}

// 正常文本流：message_start → 一块 text（start/delta/stop）→ message_delta → message_stop。
// 块序列是 Anthropic 客户端解析的依据，顺序与配对不能错。
func TestAnthropicSSE_TextFlow(t *testing.T) {
	rec := httptest.NewRecorder()
	s := NewAnthropicSSE(rec, nil, "msg_1", "gw-model")

	s.Event(&rosetta.Event{Type: rosetta.EventMessageStart, ID: "up-1"})
	s.Event(&rosetta.Event{Type: rosetta.EventTextDelta, Text: "你好"})
	s.Event(&rosetta.Event{Type: rosetta.EventTextDelta, Text: "，世界"})
	s.Event(&rosetta.Event{Type: rosetta.EventMessageEnd, Usage: &rosetta.Usage{InputTokens: 7, OutputTokens: 3}, StopReason: rosetta.StopEnd})
	s.Finish("ok", rosetta.StopEnd, rosetta.Usage{InputTokens: 7, OutputTokens: 3}, false)

	events := collect(t, rec.Body.String())
	want := []string{"message_start", "content_block_start", "content_block_delta", "content_block_delta", "content_block_stop", "message_delta", "message_stop"}
	if got := eventNames(events); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("event sequence = %v, want %v", got, want)
	}

	// message_start 带上游真实 id。
	if !strings.Contains(events[0][1], `"id":"up-1"`) {
		t.Fatalf("message_start should carry upstream id: %s", events[0][1])
	}
	// 文本块只开一次（两个 delta 同块）。
	if strings.Count(rec.Body.String(), `"content_block_start"`) != 1 {
		t.Fatalf("expected exactly one content_block_start")
	}
	// message_delta 带权威 usage 与 stop_reason。
	if !strings.Contains(events[len(events)-2][1], `"stop_reason":"end_turn"`) ||
		!strings.Contains(events[len(events)-2][1], `"output_tokens":3`) {
		t.Fatalf("message_delta missing stop_reason/usage: %s", events[len(events)-2][1])
	}
}

// contentBlockStarts 解析全部 content_block_start 事件的 (index, content_block)。
func contentBlockStarts(t *testing.T, raw string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, e := range collect(t, raw) {
		if e[0] != "content_block_start" {
			continue
		}
		var p struct {
			Index        int            `json:"index"`
			ContentBlock map[string]any `json:"content_block"`
		}
		if err := json.Unmarshal([]byte(e[1]), &p); err != nil {
			t.Fatalf("parse content_block_start: %v (%s)", err, e[1])
		}
		out = append(out, p.ContentBlock)
	}
	return out
}

// 思考流：thinking 块 + signature_delta；签名是 Anthropic 回放 thinking 的必要条件。
func TestAnthropicSSE_ThinkingAndSignature(t *testing.T) {
	rec := httptest.NewRecorder()
	s := NewAnthropicSSE(rec, nil, "msg_1", "gw-model")

	s.Event(&rosetta.Event{Type: rosetta.EventMessageStart, ID: "up-1"})
	s.Event(&rosetta.Event{Type: rosetta.EventThinkingDelta, Text: "推理中"})
	// 空 Text + 非空 Signature = thinking 块的收尾签名。
	s.Event(&rosetta.Event{Type: rosetta.EventThinkingDelta, Signature: "sig==", Text: ""})
	s.Event(&rosetta.Event{Type: rosetta.EventTextDelta, Text: "答案"})
	s.Finish("ok", rosetta.StopEnd, rosetta.Usage{}, false)

	raw := rec.Body.String()
	blocks := contentBlockStarts(t, raw)
	if len(blocks) != 2 || blocks[0]["type"] != "thinking" || blocks[1]["type"] != "text" {
		t.Fatalf("expected [thinking, text] blocks, got %+v", blocks)
	}
	for _, want := range []string{
		`"type":"thinking_delta"`,
		// 签名必须透传，否则下游把 thinking 回传给 Anthropic 上游时会被拒。
		`"type":"signature_delta"`,
		`"signature":"sig=="`,
	} {
		if !strings.Contains(raw, want) {
			t.Fatalf("missing %q in output:\n%s", want, raw)
		}
	}
	if !s.WroteContent() {
		t.Fatalf("thinking/text should count as content")
	}
}

// 工具调用：每个 tool_use 一块；工具 id 变化即切块。
func TestAnthropicSSE_ToolCallBlocks(t *testing.T) {
	rec := httptest.NewRecorder()
	s := NewAnthropicSSE(rec, nil, "msg_1", "gw-model")

	s.Event(&rosetta.Event{Type: rosetta.EventMessageStart, ID: "up-1"})
	s.Event(&rosetta.Event{Type: rosetta.EventToolCall, ToolIndex: 0, ToolID: "t1", ToolName: "get_weather", ArgumentsDelta: `{"city":`})
	s.Event(&rosetta.Event{Type: rosetta.EventToolCall, ToolIndex: 0, ToolID: "t1", ToolName: "get_weather", ArgumentsDelta: `"北京"}`})
	s.Event(&rosetta.Event{Type: rosetta.EventToolCall, ToolIndex: 1, ToolID: "t2", ToolName: "get_time", ArgumentsDelta: `{}`})
	s.Event(&rosetta.Event{Type: rosetta.EventMessageEnd, Usage: &rosetta.Usage{InputTokens: 5, OutputTokens: 9}, StopReason: rosetta.StopToolUse})
	s.Finish("ok", rosetta.StopToolUse, rosetta.Usage{InputTokens: 5, OutputTokens: 9}, false)

	raw := rec.Body.String()
	blocks := contentBlockStarts(t, raw)
	if len(blocks) != 2 {
		t.Fatalf("expected 2 tool_use blocks, got %d:\n%s", len(blocks), raw)
	}
	if blocks[0]["id"] != "t1" || blocks[0]["name"] != "get_weather" ||
		blocks[1]["id"] != "t2" || blocks[1]["name"] != "get_time" {
		t.Fatalf("tool blocks mismatch: %+v", blocks)
	}
	if !strings.Contains(raw, `"stop_reason":"tool_use"`) {
		t.Fatalf("stop_reason should be tool_use:\n%s", raw)
	}
}

// 断流语义（DESIGN §8.2）：发 error 事件、不发 message_stop —— 客户端据此判定异常。
func TestAnthropicSSE_TruncatedSendsErrorNotStop(t *testing.T) {
	rec := httptest.NewRecorder()
	s := NewAnthropicSSE(rec, nil, "msg_1", "gw-model")

	s.Event(&rosetta.Event{Type: rosetta.EventMessageStart, ID: "up-1"})
	s.Event(&rosetta.Event{Type: rosetta.EventTextDelta, Text: "半截"})
	s.Finish("truncated", "", rosetta.Usage{}, false)

	events := collect(t, rec.Body.String())
	names := eventNames(events)
	if names[len(names)-1] != "error" {
		t.Fatalf("last event should be error, got %v", names)
	}
	if !strings.Contains(events[len(events)-1][1], `"type":"api_error"`) {
		t.Fatalf("error payload should be api_error: %s", events[len(events)-1][1])
	}
	for _, e := range events {
		if e[0] == "message_stop" || e[0] == "message_delta" {
			t.Fatalf("truncated stream must not send %s", e[0])
		}
	}
	// 块也要闭合：text 块的 content_block_stop 必须在 error 之前。
	stopped := false
	for _, e := range events {
		if e[0] == "content_block_stop" {
			stopped = true
		}
		if e[0] == "error" && !stopped {
			t.Fatalf("open content block was never stopped before error event")
		}
	}
}

// canceled：客户端已走，一个字节都不该再写。
func TestAnthropicSSE_CanceledWritesNothing(t *testing.T) {
	rec := httptest.NewRecorder()
	s := NewAnthropicSSE(rec, nil, "msg_1", "gw-model")
	s.Finish("canceled", "", rosetta.Usage{}, false)
	if rec.Body.Len() != 0 {
		t.Fatalf("canceled finish should write nothing, got %q", rec.Body.String())
	}
}

// 非流式：content 按块顺序（thinking → text → tool_use），input 必为对象。
func TestWriteAnthropicResponse(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteAnthropicResponse(rec, &rosetta.ChatResponse{
		ID: "up-9", Model: "claude-3",
		Content: []rosetta.Block{
			{Type: rosetta.BlockThinking, Thinking: "想", Signature: "sig"},
			{Type: rosetta.BlockText, Text: "答"},
			{Type: rosetta.BlockToolCall, ToolCallID: "t1", ToolName: "f", Arguments: `{"a":1}`},
			{Type: rosetta.BlockToolCall, ToolCallID: "t2", ToolName: "g"},
		},
		StopReason: rosetta.StopToolUse,
		Usage:      rosetta.Usage{InputTokens: 10, OutputTokens: 4, CachedInputTokens: 6},
	}, "gw-model")

	var out struct {
		ID         string `json:"id"`
		Type       string `json:"type"`
		Model      string `json:"model"`
		StopReason string `json:"stop_reason"`
		Content    []struct {
			Type      string          `json:"type"`
			Text      string          `json:"text"`
			Thinking  string          `json:"thinking"`
			Signature string          `json:"signature"`
			ID        string          `json:"id"`
			Input     json.RawMessage `json:"input"`
		} `json:"content"`
		Usage struct {
			InputTokens  int64 `json:"input_tokens"`
			OutputTokens int64 `json:"output_tokens"`
			CacheRead    int64 `json:"cache_read_input_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, rec.Body.String())
	}
	if out.ID != "up-9" || out.Type != "message" || out.Model != "gw-model" {
		t.Fatalf("envelope mismatch: %+v", out)
	}
	if out.StopReason != "tool_use" {
		t.Fatalf("stop_reason = %q, want tool_use", out.StopReason)
	}
	if len(out.Content) != 4 ||
		out.Content[0].Type != "thinking" || out.Content[0].Signature != "sig" ||
		out.Content[1].Type != "text" || out.Content[1].Text != "答" ||
		out.Content[2].Type != "tool_use" || string(out.Content[2].Input) != `{"a":1}` ||
		out.Content[3].Type != "tool_use" || string(out.Content[3].Input) != `{}` {
		t.Fatalf("content blocks mismatch: %+v", out.Content)
	}
	if out.Usage.CacheRead != 6 {
		t.Fatalf("cache_read_input_tokens = %d, want 6", out.Usage.CacheRead)
	}
}

// 错误形状与类型映射（含 401/403 的 authentication/permission 区分）。
func TestWriteAnthropicError(t *testing.T) {
	cases := []struct {
		status int
		code   string
		want   string
	}{
		{http.StatusUnauthorized, "invalid_api_key", "authentication_error"},
		{http.StatusForbidden, "invalid_api_key", "permission_error"},
		{http.StatusBadRequest, "invalid_request_error", "invalid_request_error"},
		{http.StatusBadRequest, "context_length_exceeded", "invalid_request_error"},
		{http.StatusNotFound, "model_not_found", "not_found_error"},
		{http.StatusTooManyRequests, "insufficient_quota", "rate_limit_error"},
		{http.StatusRequestEntityTooLarge, "request_too_large", "request_too_large"},
		{http.StatusBadGateway, "upstream_auth_error", "api_error"},
		{http.StatusGatewayTimeout, "upstream_timeout", "api_error"},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		WriteAnthropicError(rec, c.status, c.code, "msg")
		var out struct {
			Type  string `json:"type"`
			Error struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("code=%s: unmarshal: %v", c.code, err)
		}
		if rec.Code != c.status || out.Type != "error" || out.Error.Type != c.want {
			t.Fatalf("code=%s: got type=%q (http %d), want %q (%d)",
				c.code, out.Error.Type, rec.Code, c.want, c.status)
		}
	}
}
