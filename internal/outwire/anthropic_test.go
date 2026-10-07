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
	// 首片带 id/name；**续片只带 ToolIndex + ArgumentsDelta** —— 这是 rosetta
	// 的实际契约（SDK 侧 anthropic 的 input_json_delta 不填 ToolID/ToolName）。
	// 旧测试给每个分片都重复传 ToolID/ToolName，恰好掩盖了「用 ToolID 判同块」
	// 导致的「每个参数分片切一个新块」缺陷。
	s.Event(&rosetta.Event{Type: rosetta.EventToolCall, ToolIndex: 0, ToolID: "t1", ToolName: "get_weather"})
	s.Event(&rosetta.Event{Type: rosetta.EventToolCall, ToolIndex: 0, ArgumentsDelta: `{"city":`})
	s.Event(&rosetta.Event{Type: rosetta.EventToolCall, ToolIndex: 0, ArgumentsDelta: `"北京"}`})
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

// 非流式 usage：统一 Usage 的缓存 token 必须从 input_tokens 拆出，填进各自的
// 互斥计费桶。SDK 折叠语义（Cached ⊆ Input）下直接抄 InputTokens 会把缓存
// 算两遍（P1-7）：Input=100/Cached=30/Creation=20 → input=50、cache_read=30、
// cache_creation=20。
func TestWriteAnthropicResponse_UsageCacheBucketsSplit(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteAnthropicResponse(rec, &rosetta.ChatResponse{
		ID:    "up-cache",
		Model: "claude",
		Usage: rosetta.Usage{InputTokens: 100, OutputTokens: 8, CachedInputTokens: 30, CachedCreationTokens: 20},
	}, "gw-model")

	var out struct {
		Usage struct {
			Input         int64 `json:"input_tokens"`
			Output        int64 `json:"output_tokens"`
			CacheRead     int64 `json:"cache_read_input_tokens"`
			CacheCreation int64 `json:"cache_creation_input_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, rec.Body.String())
	}
	if out.Usage.Input != 50 || out.Usage.CacheRead != 30 || out.Usage.CacheCreation != 20 {
		t.Fatalf("usage buckets = input:%d read:%d creation:%d, want 50/30/20 (cached tokens must not double-count into input_tokens)",
			out.Usage.Input, out.Usage.CacheRead, out.Usage.CacheCreation)
	}
	if out.Usage.Output != 8 {
		t.Fatalf("output_tokens = %d, want 8", out.Usage.Output)
	}
}

// 无缓存时三字段形状不变：只有 input_tokens / output_tokens，不出现
// cache_read / cache_creation 键 —— 且 input_tokens 与统一 InputTokens 相等。
func TestWriteAnthropicResponse_UsageNoCacheShapeUnchanged(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteAnthropicResponse(rec, &rosetta.ChatResponse{
		ID:    "up-nocache",
		Model: "claude",
		Usage: rosetta.Usage{InputTokens: 10, OutputTokens: 4},
	}, "gw-model")

	var out struct {
		Usage struct {
			Input  int64 `json:"input_tokens"`
			Output int64 `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, rec.Body.String())
	}
	if out.Usage.Input != 10 || out.Usage.Output != 4 {
		t.Fatalf("usage = %d/%d, want 10/4", out.Usage.Input, out.Usage.Output)
	}
	raw := rec.Body.String()
	for _, k := range []string{"cache_read_input_tokens", "cache_creation_input_tokens"} {
		if strings.Contains(raw, k) {
			t.Fatalf("无缓存时不应出现 %s 键：%s", k, raw)
		}
	}
}

// 流式路径（message_delta 的 usage）与非流式同口径：缓存桶拆分必须两条路径都生效。
func TestAnthropicSSE_UsageCacheBucketsSplit(t *testing.T) {
	rec := httptest.NewRecorder()
	s := NewAnthropicSSE(rec, nil, "msg_1", "gw-model")
	s.Event(&rosetta.Event{Type: rosetta.EventMessageStart, ID: "up-1"})
	s.Event(&rosetta.Event{Type: rosetta.EventTextDelta, Text: "答"})
	s.Event(&rosetta.Event{Type: rosetta.EventMessageEnd, StopReason: rosetta.StopEnd})
	s.Finish("ok", rosetta.StopEnd,
		rosetta.Usage{InputTokens: 100, OutputTokens: 8, CachedInputTokens: 30, CachedCreationTokens: 20}, false)

	var delta struct {
		Usage struct {
			Input         int64 `json:"input_tokens"`
			CacheRead     int64 `json:"cache_read_input_tokens"`
			CacheCreation int64 `json:"cache_creation_input_tokens"`
		} `json:"usage"`
	}
	for _, e := range collect(t, rec.Body.String()) {
		if e[0] != "message_delta" {
			continue
		}
		if err := json.Unmarshal([]byte(e[1]), &delta); err != nil {
			t.Fatalf("parse message_delta: %v (%s)", err, e[1])
		}
	}
	if delta.Usage.Input != 50 || delta.Usage.CacheRead != 30 || delta.Usage.CacheCreation != 20 {
		t.Fatalf("stream usage buckets = input:%d read:%d creation:%d, want 50/30/20",
			delta.Usage.Input, delta.Usage.CacheRead, delta.Usage.CacheCreation)
	}
}

// 流式无缓存：input_tokens 照常补权威值，不出现缓存键（形状与修复前一致）。
func TestAnthropicSSE_UsageNoCacheShapeUnchanged(t *testing.T) {
	rec := httptest.NewRecorder()
	s := NewAnthropicSSE(rec, nil, "msg_1", "gw-model")
	s.Event(&rosetta.Event{Type: rosetta.EventMessageStart, ID: "up-1"})
	s.Event(&rosetta.Event{Type: rosetta.EventTextDelta, Text: "答"})
	s.Event(&rosetta.Event{Type: rosetta.EventMessageEnd, StopReason: rosetta.StopEnd})
	s.Finish("ok", rosetta.StopEnd, rosetta.Usage{InputTokens: 7, OutputTokens: 3}, false)

	raw := rec.Body.String()
	if !strings.Contains(raw, `"input_tokens":7`) {
		t.Fatalf("message_delta 应补 input_tokens=7：\n%s", raw)
	}
	for _, k := range []string{"cache_read_input_tokens", "cache_creation_input_tokens"} {
		if strings.Contains(raw, k) {
			t.Fatalf("无缓存时不应出现 %s 键：\n%s", k, raw)
		}
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
		// request_too_large 是网关自造的 code，Anthropic 官方错误类型表里
		// 没有它。旧实现原样透传，于是响应里的 type 是一个官方 SDK 从未
		// 见过、无法分类的字符串。必须归到 invalid_request_error。
		{http.StatusRequestEntityTooLarge, "request_too_large", "invalid_request_error"},
		// 上游_* 一族：按 code 分类，让客户端能区分「换凭据」与「退避重试」。
		{http.StatusBadGateway, "upstream_auth_error", "authentication_error"},
		{http.StatusForbidden, "upstream_auth_error", "permission_error"},
		{http.StatusGatewayTimeout, "upstream_timeout", "timeout_error"},
		// 过载：Anthropic 自定义 529 + overloaded_error，官方 SDK 对它做
		// 指数退避重试。旧实现落 default → api_error，客户端就不重试了 ——
		// 而过载恰恰是最该重试的场景。
		{529, "upstream_error", "overloaded_error"},
		{statusOverloaded, "upstream_error", "overloaded_error"},
		{http.StatusPaymentRequired, "upstream_quota_exhausted", "rate_limit_error"},
		{http.StatusConflict, "upstream_error", "conflict_error"},
		{http.StatusBadGateway, "upstream_error", "api_error"},
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
