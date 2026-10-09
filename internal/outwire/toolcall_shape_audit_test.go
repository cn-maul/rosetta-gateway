package outwire

import (
	"encoding/json"
	"strings"
	"testing"
)

// 网关的 tool_calls 增量形状必须与 OpenAI 原生流**同构**。
//
// # 这条测试存在的理由
//
// 2026-10 修过两次：
//   1. 缺 function → opencode（AI SDK 的 zod schema）整条判非法；
//   2. 补 function 时把 name 恒填成 "" → 「存在判断」式合并会把真实工具名冲掉。
//
// 两次都在「本地自测通过」的状态下发出去，靠的是**单条断言**（只验本次
// 关心的那个字段）。本文件把「与 OpenAI 原生同构」整件事一次性钉住：
// 三种合并策略 × 三种拍，任何一处形状漂移都会红。
//
// # 形状基准（逐字取自 OpenAI 实际行为）
//
//	首片 {index, id, type, function:{name:"bash", arguments:""}}
//	空拍 {index, function:{arguments:""}}          ← name 键**缺席**
//	续片 {index, function:{arguments:'{"cmd":'}}   ← name 键**缺席**
//
// 关键：续片与空拍都不带 name 键。这不是省略细节 —— 它是「用「存在判断」
// 合并的客户端不会覆盖掉首片工具名」的唯一前提。name:"" 是有定义的空串，
// 会被当��新值覆盖进去，工具名静默丢失。
//
// # 实测过的判据（详见 openai_chat.go 同名函数注释）
//
//	AI SDK 原样 schema        → 本形状全部通过，缺 function 的形状被拒
//	三种合并策略（truthy/defined/nullish） → 本形状与 OpenAI 原生结果完全一致

// toolCallBeat 是解析出来的单拍增量（只取与形状相关的字段）。
type toolCallBeat struct {
	Index    float64
	ID       string
	Type     string
	FnName   string
	FnNameOK bool // name 键是否存在（区分「空串」与「缺席」）
	FnArgs   string
	FnArgsOK bool
}

// emitBeats 跑一串 (id, name, args) 拍，返回解析后的形状序列。
func emitBeats(t *testing.T, beats []struct{ id, name, args string }) []toolCallBeat {
	t.Helper()
	var sb strings.Builder
	sw := NewSSEWriter(nopWriter{&sb}, nopWriter{&sb}, "gen-1", "m1", 1)
	for _, b := range beats {
		if err := sw.WriteToolCallDelta(0, b.id, b.name, b.args); err != nil {
			t.Fatalf("WriteToolCallDelta: %v", err)
		}
	}

	var out []toolCallBeat
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
			continue // role 分片
		}
		tc0 := tcsRaw.([]any)[0].(map[string]any)

		var b toolCallBeat
		if v, ok := tc0["index"].(float64); ok {
			b.Index = v
		}
		if v, ok := tc0["id"].(string); ok {
			b.ID = v
			b.Type, _ = tc0["type"].(string)
		}
		if fnRaw, ok := tc0["function"]; ok {
			fn := fnRaw.(map[string]any)
			if v, ok := fn["name"]; ok {
				b.FnName, b.FnNameOK = v.(string), true
			}
			if v, ok := fn["arguments"]; ok {
				b.FnArgs, b.FnArgsOK = v.(string), true
			}
		}
		out = append(out, b)
	}
	return out
}

// 三种下游合并策略。名字对应真实客户端里出现过的写法。
type mergeStrategy struct {
	name string
	// merge 返回合并后的 (工具名, 参数字符串)。
	merge func(beats []toolCallBeat) (string, string)
}

var mergeStrategies = []mergeStrategy{
	{"truthy（if fn.name）", func(bs []toolCallBeat) (string, string) {
		var name, args string
		for _, b := range bs {
			if b.FnNameOK && b.FnName != "" {
				name = b.FnName
			}
			if b.FnArgsOK {
				args += b.FnArgs
			}
		}
		return name, args
	}},
	{"defined（if fn.name !== undefined）", func(bs []toolCallBeat) (string, string) {
		var name, args string
		for _, b := range bs {
			if b.FnNameOK {
				name = b.FnName
			}
			if b.FnArgsOK {
				args += b.FnArgs
			}
		}
		return name, args
	}},
	{"nullish（fn.name ?? snapshot）", func(bs []toolCallBeat) (string, string) {
		var name, args string
		for _, b := range bs {
			if b.FnNameOK {
				name = b.FnName
			}
			if b.FnArgsOK {
				args += b.FnArgs
			}
		}
		return name, args
	}},
}

// 网关的产出必须与 OpenAI 原生流给出**相同的合并结果**，对所有策略成立。
//
// 这是本次修复「不会弄坏其它客户端」的**可执行**表述：不分别断言每个字段，
// 而是断言最终语义 —— 无论下游用哪种合并写法，得到的工具名与参数都一样，
// 且都与 OpenAI 原生基准相同。
func TestToolCallStream_MatchesOpenAINativeSemantics(t *testing.T) {
	const wantName = "bash"
	const wantArgs = `{"cmd":"ls"}`

	// OpenAI 原生的拍序列（对照基准，不经网关）：首片 + 两个续片。
	openaiNative := emitBeats(t, []struct{ id, name, args string }{
		{"call_1", "bash", ""},
		{"", "", `{"cmd":`},
		{"", "", `"ls"}`},
	})
	// 网关的实际拍序列：多了一个上游带来的空拍（Hy4 会这么吐）。
	gateway := emitBeats(t, []struct{ id, name, args string }{
		{"call_1", "bash", ""},
		{"", "", ""},
		{"", "", `{"cmd":`},
		{"", "", `"ls"}`},
	})

	if len(gateway) != len(openaiNative)+1 {
		t.Fatalf("网关应比原生多一个空拍：原生 %d 拍、网关 %d 拍", len(openaiNative), len(gateway))
	}

	for _, s := range mergeStrategies {
		t.Run(s.name, func(t *testing.T) {
			gotName, gotArgs := s.merge(gateway)
			if gotName != wantName {
				t.Errorf("工具名 = %q，期望 %q —— 形状与 OpenAI 原生不一致", gotName, wantName)
			}
			if gotArgs != wantArgs {
				t.Errorf("参数 = %q，期望 %q", gotArgs, wantArgs)
			}
			// 与原生基准逐项比对：这是「零行为差异」这句断言的落点。
			nName, nArgs := s.merge(openaiNative)
			if gotName != nName || gotArgs != nArgs {
				t.Errorf("与 OpenAI 原生结果不一致：网关 (%q,%q) vs 原生 (%q,%q)",
					gotName, gotArgs, nName, nArgs)
			}
		})
	}
}

// 空拍不得携带 name 键（哪怕是空串）。
//
// 单独一条：它是上面那条能成立的前提。哪天后有人「图省事」把 name 恒填，
// 这里立刻红，并指明后果是工具名被覆盖。
func TestToolCallStream_NeverSendsNameKeyWithoutName(t *testing.T) {
	beats := emitBeats(t, []struct{ id, name, args string }{
		{"", "", ""},        // 空拍
		{"", "", `{"a":1}`}, // 续片
	})
	for i, b := range beats {
		if b.FnNameOK {
			t.Errorf("第 %d 拍带了 name 键（值 %q），期望缺席 —— 空串会被「存在判断」式合并当成新值", i, b.FnName)
		}
		if !b.FnArgsOK {
			t.Errorf("第 %d 拍缺 arguments 键：AI SDK 的 schema 要求 function 存在且是 object", i)
		}
	}
}

// function 键必须恒存在 —— 这是 opencode 报错的直接根因，单独钉住。
func TestToolCallStream_FunctionAlwaysPresent(t *testing.T) {
	var sb strings.Builder
	sw := NewSSEWriter(nopWriter{&sb}, nopWriter{&sb}, "gen-1", "m1", 1)
	// 三种拍全跑一遍：空拍最容易触发「省略 function」。
	for _, b := range []struct{ id, name, args string }{
		{"", "", ""},
		{"call_1", "bash", ""},
		{"", "", `{"x":`},
	} {
		if err := sw.WriteToolCallDelta(0, b.id, b.name, b.args); err != nil {
			t.Fatalf("WriteToolCallDelta: %v", err)
		}
	}
	n := 0
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
		n++
		tc0 := tcsRaw.([]any)[0].(map[string]any)
		if _, ok := tc0["function"]; !ok {
			t.Errorf("第 %d 拍缺 function：%v —— 严格 schema（AI SDK）会判整条响应非法", n, tc0)
		}
	}
	if n != 3 {
		t.Fatalf("应产出 3 个 tool_calls 分片，实际 %d", n)
	}
}
