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
// # 断言的形状（照 OpenAI 自己的线格式，不是「填满空串」）
//
//   - 空拍（无 name 无 arguments）→ function 必须有，且只含 arguments:""
//   - 首片（有 id/name）          → function 带 name，且不能因为修了这个
//     bug 而把 id/type 弄丢
//   - 续片（只有 arguments）      → function 带 arguments，且**不含 name 键**
//
// 第三条是关键：name 必须**省略**而不是填 ""。下游合并增量有两种写法
// （真值判断 `if (fn.name)` / 存在判断 `if (fn.name !== undefined)`），
// 后者在真实 OpenAI 流上安全（续片没有 name 键），但 name:"" 会被它
// 覆盖到已收到的真实工具名上 —— 工具名丢失。所以这里钉的是
// 「与 OpenAI 线格式逐字一致」，而不是「字段都存在」。
func TestWriteToolCallDelta_AlwaysEmitsFunction(t *testing.T) {
	cases := []struct {
		name        string
		index       int
		id          string
		toolName    string
		argsDelta   string
		wantFn      map[string]any
		wantNameKey bool // function 里是否应出现 name 键
		wantID      bool
	}{
		{
			// 用户报的那一条：上游只给 index，name 与 arguments 都还没到。
			name:        "空拍：只有 index",
			index:       0,
			wantFn:      map[string]any{"arguments": ""},
			wantNameKey: false,
			wantID:      false,
		},
		{
			name:        "首片：带 id 与 name",
			index:       0,
			id:          "call_1",
			toolName:    "bash",
			wantFn:      map[string]any{"name": "bash", "arguments": ""},
			wantNameKey: true,
			wantID:      true,
		},
		{
			// SDK 契约：续片不带 ToolID/ToolName，只有 arguments 增量。
			// name 键必须**缺席**（不是空串）—— 见文件头的说明。
			name:        "续片：只有 arguments",
			index:       0,
			argsDelta:   `{"city":`,
			wantFn:      map[string]any{"arguments": `{"city":`},
			wantNameKey: false,
			wantID:      false,
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
			// name 键的有无必须**精确**匹配 OpenAI 线格式：该省略时必须是
			// 缺席，不能是空串（空串会被「存在判断」式的合并覆盖掉真实工具名）。
			_, hasName := fn["name"]
			if hasName != tc.wantNameKey {
				extra := "；续片若带 name:\"\" 会覆盖首片的真实工具名"
				if tc.wantNameKey {
					extra = "；首片必须带 name"
				}
				t.Errorf("function.name 存在=%v，期望 %v%s", hasName, tc.wantNameKey, extra)
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

// 空拍不得冲掉已收到的工具名。
//
// 这是「function 恒存在」这个修复**引入**的隐患的回归测试：
// 如果把 name 恒填成 ""，下游用「存在判断」合并增量
// （if (fn.name !== undefined) { snapshot.name = fn.name }）时，
// 空拍的 "" 会覆盖掉首片收到的真实工具名 —— 工具调用静默失效，
// 且比原来「缺 function」更难查（响应完全合法，只是名字没了）。
func TestToolCallDelta_EmptyBeatDoesNotClobberName(t *testing.T) {
	var sb strings.Builder
	sw := NewSSEWriter(nopWriter{&sb}, nopWriter{&sb}, "gen-1", "m1", 1)

	// 首片带名字，随后夹一个空拍（Hy4 的实际行为），再续参数。
	_ = sw.WriteToolCallDelta(0, "call_1", "bash", "")
	_ = sw.WriteToolCallDelta(0, "", "", "")
	_ = sw.WriteToolCallDelta(0, "", "", `{"cmd":"ls"}`)

	// 模拟下游「存在判断」式合并：键存在就覆盖。
	snapshotName := ""
	snapshotArgs := ""
	for _, line := range strings.Split(sb.String(), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			continue
		}
		var ev map[string]any
		if err := json.Unmarshal([]byte(payload), &ev); err != nil {
			t.Fatalf("分片不是合法 JSON: %q", line)
		}
		choices, _ := ev["choices"].([]any)
		if len(choices) == 0 {
			continue
		}
		c0 := choices[0].(map[string]any)
		delta, _ := c0["delta"].(map[string]any)
		tcsRaw, ok := delta["tool_calls"]
		if !ok {
			continue
		}
		tc0 := tcsRaw.([]any)[0].(map[string]any)
		fn := tc0["function"].(map[string]any)
		// 存在判断：键在就覆盖（这正是 name:"" 会闯祸的写法）
		if v, exists := fn["name"]; exists {
			snapshotName = v.(string)
		}
		if v, exists := fn["arguments"]; exists {
			snapshotArgs += v.(string)
		}
	}

	if snapshotName != "bash" {
		t.Errorf("空拍冲掉了工具名：合并结果 name=%q，期望 bash —— 说明 name 被填成了空串而非省略", snapshotName)
	}
	if snapshotArgs != `{"cmd":"ls"}` {
		t.Errorf("参数拼接 = %q，期望 {\"cmd\":\"ls\"}", snapshotArgs)
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
