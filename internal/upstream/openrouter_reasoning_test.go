package upstream

// OpenRouter 的 `reasoning` 是**对象**而非布尔（2026-10-15 实测确认）。
//
// # 这个 bug 的形状
//
// 报给用户的错误是：
//
//	解析模型列表失败：json: cannot unmarshal object into Go struct field
//	.data.2.reasoning of type bool
//
// 注意 `.data.2.` —— 失败发生在**解析第 3 个模型**时。也就是说这不是
// 「某一个模型数据异常」，而是**整个列表解析中断**：几百个模型一个都拿不到，
// 「获取模型」功能对 OpenRouter 完全不可用。任何一个模型带这个字段就全废。
//
// # 为什么用真实响应做夹具
//
// 这个 bug 的本质是「我们以为的形状」与「上游实际的形状」不一致。
// 凭记忆构造的用例恰好能通过（写测试的人会照着自己代码里的 *bool 写
// JSON），所以这里直接抓真实响应存成夹具。

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestFetchModels_OpenRouter对象形态reasoning 是本 bug 的直接回归。
//
// 钉三件事：
//  1. 解析不报错（修复前在这里就返回 error）；
//  2. mandatory=true 的模型被认成「支持思考」；
//  3. supported_efforts 被带出来 —— 不带的话 effort_levels 全空，
//     而空 = 网关不干预 = 客户端发的思考档位裸奔。
func TestFetchModels_OpenRouter对象形态reasoning(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, openRouterModelsFixture)
	}))
	defer srv.Close()

	cands, err := fetchModels(t.Context(), http.DefaultClient, srv.URL, "openai-chat", "sk-test")
	if err != nil {
		t.Fatalf("fetchModels 失败（修复前是 cannot unmarshal object）: %v", err)
	}

	byID := make(map[string]ModelCandidate, len(cands))
	for _, c := range cands {
		byID[c.ID] = c
	}

	t.Run("mandatory=true 认成支持思考", func(t *testing.T) {
		c, ok := byID["google/gemini-4-argon"]
		if !ok {
			t.Fatalf("没解析出该模型，拿到 %d 个候选", len(cands))
		}
		if c.SupportsThinking == nil {
			t.Fatal("SupportsThinking = nil —— 上游明明报了 mandatory，不该是「不知道」")
		}
		if !*c.SupportsThinking {
			t.Error("SupportsThinking = false，但 mandatory=true 蕴含支持思考")
		}
	})

	t.Run("supported_efforts 从对象内部被取出", func(t *testing.T) {
		c := byID["google/gemini-4-argon"]
		want := []string{"high", "medium", "low"}
		if len(c.EffortLevels) != len(want) {
			t.Fatalf("EffortLevels = %v, want %v（OpenRouter 把它放在 reasoning 对象**内部**，不收就全空）",
				c.EffortLevels, want)
		}
		for i := range want {
			if c.EffortLevels[i] != want[i] {
				t.Errorf("EffortLevels[%d] = %q, want %q", i, c.EffortLevels[i], want[i])
			}
		}
	})

	t.Run("缺席 reasoning 仍是「不知道」而非 false", func(t *testing.T) {
		c := byID["openai/gpt-4o"]
		if c.SupportsThinking == nil {
			return // 正确：没报 = 不干预
		}
		t.Errorf("SupportsThinking = %v，但该模型没有 reasoning 字段，应为 nil（不知道）", *c.SupportsThinking)
	})
}

// TestFlexBool_形态矩阵 把 flexBool 的判定语义逐条钉住。
//
// 重点是**判定顺序**：有 supported_efforts → 支持；否则取 mandatory。
// 顺序反了会把「思考可选」（mandatory=false 但仍支持）的模型误判成
// 「不支持」，进而被网关剥掉思考 —— 那是比「不干预」糟糕得多的结果。
func TestFlexBool_形态矩阵(t *testing.T) {
	cases := []struct {
		name       string
		json       string
		wantSet    bool
		wantBool   *bool
		wantLevels []string
	}{
		{"缺席", `{}`, false, nil, nil},
		{"显式 null", `{"reasoning":null}`, false, nil, nil},
		{"布尔 true", `{"reasoning":true}`, true, boolp(true), nil},
		{"布尔 false", `{"reasoning":false}`, true, boolp(false), nil},
		// OpenRouter 真实形态之一：强制思考。
		{"对象 mandatory=true", `{"reasoning":{"mandatory":true}}`, true, boolp(true), nil},
		// 关键用例：mandatory=false **不是**「不支持」，它只说「思考可选」。
		{"对象 mandatory=false 但有档位 → 支持", `{"reasoning":{"mandatory":false,"supported_efforts":["low","high"]}}`,
			true, boolp(true), []string{"low", "high"}},
		{"对象 default_enabled", `{"reasoning":{"mandatory":false,"default_enabled":true}}`, true, boolp(false), nil},
		{"对象 全空 → 取 mandatory", `{"reasoning":{}}`, true, boolp(false), nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var e rawModelEntry
			if err := json.Unmarshal([]byte(tc.json), &e); err != nil {
				t.Fatalf("反序列化失败: %v", err)
			}
			got := e.supportsThinkingFor()
			switch {
			case tc.wantSet && got == nil:
				t.Fatalf("supportsThinkingFor = nil, want %v", *tc.wantBool)
			case !tc.wantSet && got != nil:
				t.Fatalf("supportsThinkingFor = %v, want nil", *got)
			case tc.wantSet && *got != *tc.wantBool:
				t.Fatalf("supportsThinkingFor = %v, want %v", *got, *tc.wantBool)
			}
			if len(tc.wantLevels) != len(e.effortLevelsFor()) {
				t.Fatalf("effortLevelsFor = %v, want %v", e.effortLevelsFor(), tc.wantLevels)
			}
		})
	}
}

// TestFlexBool_不认识的形状报错 不静默忽略。
//
// 静默忽略的后果：上游报了、我们没读到 → effort_levels 空 → 网关不干预
// → 客户端发的思考档位裸奔。管理员看到的是「导入成功，思考能力没填」，
// 读不出任何异常。宁可整次发现失败并报出形态。
func TestFlexBool_不认识的形状报错(t *testing.T) {
	for _, bad := range []string{
		`{"reasoning":"yes"}`,
		`{"reasoning":1}`,
		`{"reasoning":["low","high"]}`,
	} {
		var e rawModelEntry
		if err := json.Unmarshal([]byte(bad), &e); err == nil {
			t.Errorf("%s 被静默接受 —— 上游报了但我们没读到 = 静默降级", bad)
		} else if !strings.Contains(err.Error(), "reasoning") {
			t.Errorf("%s 的错误信息没指出是 reasoning 字段: %v", bad, err)
		}
	}
}

// openRouterModelsFixture 是从 https://openrouter.ai/api/v1/models 抓的
// 真实响应节选（2026-10-15），覆盖三种实际出现的 reasoning 形态：
//
//	{"mandatory":true,"supported_efforts":[...],"default_effort":"..."}
//	{"mandatory":false}
//	{"mandatory":false,"default_enabled":true,"supported_efforts":[...]}
//
// 以及一个完全没有 reasoning 字段的模型。刻意不做「规范化」——
// 真实响应长什么样，夹具就长什么样，否则这个夹具就失去了意义。
const openRouterModelsFixture = `{
  "data": [
    {
      "id": "openai/gpt-4o",
      "name": "GPT-4o",
      "context_length": 128000,
      "top_provider": {"context_length": 128000, "max_completion_tokens": 16384}
    },
    {
      "id": "liquid/lfm-2.5-24b-a2b",
      "name": "LFM2.5 24B A2B",
      "context_length": 32768,
      "reasoning": {"mandatory": true}
    },
    {
      "id": "google/gemini-4-argon",
      "name": "Gemini 4 Argon",
      "context_length": 1048576,
      "reasoning": {
        "mandatory": true,
        "supported_efforts": ["high", "medium", "low"],
        "default_effort": "medium"
      },
      "top_provider": {"context_length": 1048576, "max_completion_tokens": 65536}
    },
    {
      "id": "openai/gpt-5.1-codex",
      "name": "GPT-5.1 Codex",
      "context_length": 400000,
      "reasoning": {
        "mandatory": false,
        "default_enabled": true,
        "supported_efforts": ["max", "xhigh", "high", "medium", "low", "minimal", "none"]
      }
    },
    {
      "id": "some/plain-model",
      "name": "Plain Model",
      "context_length": 8192,
      "reasoning": {"mandatory": false}
    }
  ]
}`
