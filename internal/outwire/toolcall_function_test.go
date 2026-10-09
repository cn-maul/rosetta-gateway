package outwire

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/cn-maul/rosetta"
)

// 工具调用增量的 `function` 必须**恒存在**（2026-10 修复的 opencode 兼容缺陷）。
//
// # 缺陷
//
// 原实现「name 与 arguments 都为空就省略 function」，于是上游吐一个
// 只带 index 的空拍时，网关发出 `{"index":0}`。严格按 OpenAI schema 校验
// 的客户端（opencode 的 AI SDK / zod）整条响应判为非法：
//
//	invalid_type at choices[0].delta.tool_calls[0].function
//	Invalid input: expected object, received undefined
//
// 实测：Hy4（经 OpenRouter）会吐这种空拍，网关产出的分片与报错逐字节一致。
//
// # 断言的形状
//
// 三条用例各守一种情形：
//   - 空拍（无 name 无 arguments）→ 必须有 function，且是 object
//   - 首片（有 id/name）          → function 带 name，且不能因为修了这个
//     bug 而把 id/type 弄丢
//   - 续片（只有 arguments）      → function 带 arguments，name 为空串
//
// 第三条同时是回归防线：SDK 的契约是「续片不带 ToolID/ToolName」，
// 改造 function 的构造时最容易顺手把续片的 arguments 也丢掉。
func TestWriteToolCallDelta_AlwaysEmitsFunction(t *testing.T) {
	cases := []struct {
		name      string
		index     int
		id        string
		toolName  string
		argsDelta string
		wantFn    map[string]any
		wantID    bool
	}{
		{
			// 用户报的那一条：上游只给 index，name 与 arguments 都还没到。
			name:   "空拍：只有 index",
			index:  0,
			wantFn: map[string]any{"name": "", "arguments": ""},
			wantID: false,
		},
		{
			name:     "首片：带 id 与 name",
			index:    0,
			id:       "call_1",
			toolName: "bash",
			wantFn:   map[string]any{"name": "bash", "arguments": ""},
			wantID:   true,
		},
		{
			// SDK 契约：续片不带 ToolID/ToolName，只有 arguments 增量。
			name:      "续片：只有 arguments",
			index:     0,
			argsDelta: `{"city":`,
			wantFn:    map[string]any{"name": "", "arguments": `{"city":`},
			wantID:    false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var sb strings.Builder
			sw := NewSSEWriter(nopWriter{&sb}, nopWriter{&sb}, "gen-1", "m1", 1)
			if err := sw.WriteToolCallDelta(tc.index, tc.id, tc.toolName, tc.argsDelta); err != nil {
				t.Fatalf("WriteToolCallDelta: %v", err)
			}

			// 取最后一个带 tool_calls 的分片（首个是 ensureRole 的 role 分片）。
			var last map[string]any
			for _, line := range strings.Split(sb.String(), "\n") {
				line = strings.TrimSpace(line)
				if !strings.HasPrefix(line, "data: ") {
					continue
				}
				var ev map[string]any
				if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev); err != nil {
					t.Fatalf("分片不是合法 JSON: %q", line)
				}
				choices, _ := ev["choices"].([]any)
				if len(choices) == 0 {
					continue
				}
				c0, _ := choices[0].(map[string]any)
				delta, _ := c0["delta"].(map[string]any)
				if _, ok := delta["tool_calls"]; ok {
					last = ev
				}
			}
			if last == nil {
				t.Fatal("没有产出带 tool_calls 的分片")
			}

			choices := last["choices"].([]any)
			c0 := choices[0].(map[string]any)
			delta := c0["delta"].(map[string]any)
			tcs := delta["tool_calls"].([]any)
			tc0 := tcs[0].(map[string]any)

			// 核心断言：function 必须是 object，绝不能是 undefined。
			fnRaw, ok := tc0["function"]
			if !ok {
				t.Fatalf("tool_calls[0] 缺 function 字段：%v —— 严格客户端会判整条响应非法", tc0)
			}
			fn, ok := fnRaw.(map[string]any)
			if !ok {
				t.Fatalf("function 不是 object：%T %v", fnRaw, fnRaw)
			}
			for k, want := range tc.wantFn {
				if fn[k] != want {
					t.Errorf("function.%s = %v，期望 %v", k, fn[k], want)
				}
			}
			if tc0["index"] != float64(tc.index) {
				t.Errorf("index = %v，期望 %d", tc0["index"], tc.index)
			}
			// id/type 只在首片出现（SDK 续片不带），不能因为修 bug 而弄反。
			_, hasID := tc0["id"]
			if hasID != tc.wantID {
				t.Errorf("id 存在=%v，期望 %v（续片不该带 id，首片必须带）", hasID, tc.wantID)
			}
			if tc.wantID && tc0["type"] != "function" {
				t.Errorf("type = %v，期望 function", tc0["type"])
			}
		})
	}
}

// 端到端：一次完整的工具调用（首片 + 续片 + 空拍）在下游能拼回完整参数，
// 且每一拍都带 function —— 即「修了这个 bug 之后工具调用仍然能用」。
func TestOpenAISink_ToolCallStreamChunksAllValid(t *testing.T) {
	var sb strings.Builder
	s := NewOpenAISink(NewSSEWriter(nopWriter{&sb}, nopWriter{&sb}, "gen-1", "m1", 1791526622))

	// 首片带 id/name；续片只带 arguments；中间夹一个空拍（Hy4 会这么吐）。
	s.Event(&rosetta.Event{Type: rosetta.EventToolCall, ToolIndex: 0, ToolID: "call_1", ToolName: "bash"})
	s.Event(&rosetta.Event{Type: rosetta.EventToolCall, ToolIndex: 0})
	s.Event(&rosetta.Event{Type: rosetta.EventToolCall, ToolIndex: 0, ArgumentsDelta: `{"cmd":`})
	s.Event(&rosetta.Event{Type: rosetta.EventToolCall, ToolIndex: 0, ArgumentsDelta: `"ls"}`})
	s.Finish("ok", rosetta.StopToolUse, rosetta.Usage{InputTokens: 1, OutputTokens: 2}, false)

	var args strings.Builder
	var sawToolChunk, allHaveFunction bool
	nTool := 0
	for _, line := range strings.Split(sb.String(), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var ev map[string]any
		// 末尾的 `data: [DONE]` 不是 JSON，是 SSE 的收尾哨兵 —— 必须跳过，
		// 否则解析报错会把「正常收尾」误当成「响应结构非法」。
		if payload := strings.TrimPrefix(line, "data: "); payload == "[DONE]" {
			continue
		} else if err := json.Unmarshal([]byte(payload), &ev); err != nil {
			t.Fatalf("分片不是合法 JSON: %q", line)
		}
		choices, _ := ev["choices"].([]any)
		if len(choices) == 0 {
			continue
		}
		c0, _ := choices[0].(map[string]any)
		delta, _ := c0["delta"].(map[string]any)
		tcsRaw, ok := delta["tool_calls"]
		if !ok {
			continue
		}
		sawToolChunk = true
		tcs := tcsRaw.([]any)
		for _, raw := range tcs {
			nTool++
			tc0 := raw.(map[string]any)
			if fn, ok := tc0["function"].(map[string]any); !ok {
				allHaveFunction = false
			} else {
				allHaveFunction = true
				if a, ok := fn["arguments"].(string); ok {
					args.WriteString(a)
				}
			}
		}
	}

	if !sawToolChunk {
		t.Fatal("整条流里没有任何 tool_calls 分片")
	}
	if !allHaveFunction {
		t.Error("存在不带 function 的 tool_calls 分片 —— 严格客户端会判非法")
	}
	if got := args.String(); got != `{"cmd":"ls"}` {
		t.Errorf("参数拼接 = %q，期望 {\"cmd\":\"ls\"} —— 修 bug 时把 arguments 弄丢了", got)
	}
	if nTool != 4 {
		t.Errorf("tool_calls 分片数 = %d，期望 4", nTool)
	}
}
