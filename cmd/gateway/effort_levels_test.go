package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cn-maul/rosetta"
	"github.com/cn-maul/rosetta-gateway/internal/effort"
	"github.com/cn-maul/rosetta-gateway/internal/inwire"
	"github.com/cn-maul/rosetta-gateway/internal/routing"
	"github.com/cn-maul/rosetta-gateway/internal/snapshot"
)

// 思考挡位：模型级配置 → /v1/models 披露 → 数据面按目标模型夹取。
//
// 三件事是一条链：客户端靠 /v1/models 知道有哪些档可选，网关靠模型配置
// 判断哪些档真实存在，而中间的「夹取」是两者之间不撒谎的那一环。任何一环
// 掉链子的表现都是同一个现象（实际强度与请求的不一致），所以必须三处同测。

// newEffortSnapshot 装一份含指定挡位的单模型快照。
func newEffortSnapshot(t *testing.T, publicName, modelID string, levels []effort.Level) {
	t.Helper()
	ri := routing.NewRouteIndex()
	ri.AddProvider(&routing.ProviderRef{ID: "p1", Slug: "prov", Protocol: "openai-chat", Enabled: true})
	ri.AddUpstreamModel(&routing.UpstreamModel{
		ID: "m1", ProviderID: "p1", ModelID: modelID, Enabled: true,
		EffortLevels: levels,
	})
	ri.AddRoute(&routing.Route{ID: "r1", PublicName: publicName, ProviderID: "p1", UpstreamModelID: "m1", Enabled: true})
	snapshot.Init(&snapshot.Snapshot{
		Routes: ri, Providers: map[string]*snapshot.ProviderSnapshot{},
		KeysByHash: snapshotKeysForTest(), UsersByID: snapshotUsersForTest(),
	})
	t.Cleanup(func() {
		snapshot.Init(&snapshot.Snapshot{
			Routes: routing.NewRouteIndex(), Providers: map[string]*snapshot.ProviderSnapshot{},
			KeysByHash: map[string]*snapshot.KeySnapshot{}, UsersByID: map[string]*snapshot.UserSnapshot{},
		})
	})
}

// TestListModels_DisclosesEffortLevels 钉住「agent 工具能读到挡位」。
//
// 这是本功能的**目的**，不是附带功能：没有它，客户端只能硬编码一张全局表，
// 而各模型档位不同（o 系列有 minimal、GPT-5 有 xhigh、Claude 只有
// budget_tokens），猜错的后果是请求被静默降级。
func TestListModels_DisclosesEffortLevels(t *testing.T) {
	newEffortSnapshot(t, "flash", "gpt-5", []effort.Level{effort.Low, effort.Medium, effort.High, effort.XHigh})

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+testAccessKey)
	rec := httptest.NewRecorder()
	handleListModels("")(rec, req)

	var out struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v（body=%s）", err, rec.Body.String())
	}
	if len(out.Data) != 1 {
		t.Fatalf("期望 1 个模型，实际 %d", len(out.Data))
	}
	raw, ok := out.Data[0]["supported_reasoning"]
	if !ok {
		t.Fatalf("supported_reasoning 缺失，agent 工具无从知道可选档位：%v", out.Data[0])
	}
	levels, ok := raw.([]any)
	if !ok || len(levels) != 4 {
		t.Fatalf("supported_reasoning = %v，期望 4 项", raw)
	}
	if levels[3] != string(effort.XHigh) {
		t.Fatalf("xhigh 未原样下发：%v", levels)
	}
}

// TestListModels_未配置挡位不下发 守住「缺席」与「空数组」的区分：
// 下发空数组会让客户端以为「这个模型不支持思考」并据此禁用选择器，
// 而实际含义是「网关没配过，按上游默认」。
func TestListModels_未配置挡位不下发(t *testing.T) {
	newEffortSnapshot(t, "flash", "gpt-4o", nil)

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+testAccessKey)
	rec := httptest.NewRecorder()
	handleListModels("")(rec, req)

	if strings.Contains(rec.Body.String(), "supported_reasoning") {
		t.Fatalf("未配置挡位时不该下发 supported_reasoning：%s", rec.Body.String())
	}
}

// TestApplyThinkingCapability_档位处理 覆盖「原样送达」这一新契约的全部分支。
//
// 关键判据是 EffortRaw 而非 Effort：收敛到 rosetta 之后，网关的职责是
// **把客户端要的档位原样交出去**（或夹到模型支持的范围内），而不是替 SDK
// 归一到三档。xhigh 被压成 high 正是本功能要消灭的行为。
func TestApplyThinkingCapability_档位处理(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		m    *routing.UpstreamModel
		// proto 决定走原样（OpenAI 系）还是折回三档（Anthropic）
		proto string
		// wantRaw 是期望的 EffortRaw（OpenAI 系）
		wantRaw string
		// rosetta 归一后的初始 Effort（ToRosetta 的产物）
		initial rosetta.Effort
		want    rosetta.Effort
	}{
		{
			name: "支持集内的档位原样送达（xhigh 不被压成 high）",
			raw:  "xhigh",
			m:    &routing.UpstreamModel{ModelID: "gpt-5", EffortLevels: []effort.Level{effort.Low, effort.High, effort.XHigh}},
			// 归一化会把 xhigh 压成 high；断言这里发出去的是 xhigh 本身。
			wantRaw: "xhigh",
			want:    rosetta.EffortUnset,
		},
		{
			name: "未配置支持集时也原样送达",
			raw:  "xhigh",
			m:    &routing.UpstreamModel{ModelID: "gpt-5"},
			// 「未配置」= 网关不干预，而不是「退回三档」。
			wantRaw: "xhigh",
			want:    rosetta.EffortUnset,
		},
		{
			name: "minimal 也原样送达",
			raw:  "minimal",
			m:    &routing.UpstreamModel{ModelID: "o3", EffortLevels: []effort.Level{effort.Minimal, effort.Low}},
			// 归一化会把 minimal 压成 low，同样是被消灭的行为。
			wantRaw: "minimal",
			want:    rosetta.EffortUnset,
		},
		{
			name: "夹到支持集内仍然原样送达",
			raw:  "xhigh",
			m:    &routing.UpstreamModel{ModelID: "gpt-5", EffortLevels: []effort.Level{effort.Low, effort.Medium}},
			// 夹到 medium 之后发出去的必须是 medium，而不是 high。
			wantRaw: "medium",
			want:    rosetta.EffortUnset,
		},
		{
			name: "客户端未指定挡位时不写 EffortRaw",
			raw:  "",
			m:    &routing.UpstreamModel{ModelID: "gpt-5", EffortLevels: []effort.Level{effort.Low}},
			// 不该由网关替客户端选一档：入站归一留下的 Effort 原样不动。
			wantRaw: "",
			initial: rosetta.EffortMedium,
			want:    rosetta.EffortMedium,
		},
		{
			name: "拼错的档位不写 EffortRaw",
			raw:  "turbo",
			m:    &routing.UpstreamModel{ModelID: "gpt-5", EffortLevels: []effort.Level{effort.Low}},
			// 猜一个值比不写更糟：不写则退回上游默认，猜则会静默改变强度。
			wantRaw: "",
			want:    rosetta.EffortUnset,
		},
		{
			// Anthropic 没有 effort 字段：发原样值会被上游拒，而它的旋钮是
			// token 预算。那条路径由入站解码填好 BudgetTokens，这里只把档位
			// 还给 SDK 的三档映射，让它折回预算。
			name:  "Anthropic 折回三档而不发原样值",
			raw:   "xhigh",
			m:     &routing.UpstreamModel{ModelID: "claude", EffortLevels: []effort.Level{effort.Low}},
			proto: "anthropic",

			wantRaw: "",
			want:    rosetta.EffortLow,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ing := &ingressRequest{rawEffort: c.raw}
			req := &rosetta.ChatRequest{
				Model:    "public",
				Thinking: &rosetta.ThinkingConfig{Effort: c.initial},
			}
			proto := c.proto
			if proto == "" {
				proto = "openai-chat"
			}
			ing.applyThinkingCapability(req, c.m, proto)

			if req.Thinking.EffortRaw != c.wantRaw {
				t.Fatalf("EffortRaw = %q，期望 %q", req.Thinking.EffortRaw, c.wantRaw)
			}
			if req.Thinking.Effort != c.want {
				t.Fatalf("Effort = %q，期望 %q", req.Thinking.Effort, c.want)
			}
		})
	}
}

// TestApplyThinkingCapability_不支持思考时剥掉 钉住 supports_thinking 的
// 执行点：显式声明不支持的模型不该收到思考配置。
//
// 剥掉而不是报错：思考强度是请求偏好，为一个偏好让整个请求失败，比退化成
// 不思考更糟 —— 客户端拿到的仍是完整答案。
func TestApplyThinkingCapability_不支持思考时剥掉(t *testing.T) {
	no := false
	req := &rosetta.ChatRequest{
		Model:    "public",
		Thinking: &rosetta.ThinkingConfig{EffortRaw: "high"},
	}
	ing := &ingressRequest{rawEffort: "high"}
	ing.applyThinkingCapability(req, &routing.UpstreamModel{
		ModelID: "gpt-4o", SupportsThinking: &no,
	}, "openai-chat")

	if req.Thinking != nil {
		t.Fatalf("不支持思考的模型不该收到思考配置：%+v", req.Thinking)
	}
}

// TestApplyThinkingCapability_未配置不等于不支持 是最容易写反的一格：
// 把 nil 当成 false，会让所有从未配置过的模型凭空失去思考能力 —— 一次
// 静默的、影响全站的降级。
func TestApplyThinkingCapability_未配置不等于不支持(t *testing.T) {
	yes := true
	for _, sup := range []*bool{nil, &yes} {
		req := &rosetta.ChatRequest{
			Model:    "public",
			Thinking: &rosetta.ThinkingConfig{EffortRaw: "high"},
		}
		ing := &ingressRequest{rawEffort: "high"}
		ing.applyThinkingCapability(req, &routing.UpstreamModel{
			ModelID: "gpt-5", SupportsThinking: sup,
		}, "openai-chat")

		if req.Thinking == nil {
			t.Fatalf("SupportsThinking=%v 时不该剥掉思考配置", sup)
		}
		if req.Thinking.EffortRaw != "high" {
			t.Fatalf("SupportsThinking=%v 时档位应原样保留，实际 %q", sup, req.Thinking.EffortRaw)
		}
	}
}

// TestApplyThinkingCapability_未请求思考不新建Thinking 是零回归的关键一格：
// 客户端没提 effort 时请求里可能压根没有 Thinking，不能凭空造一个 ——
// 那等于替所有请求打开了思考。
func TestApplyThinkingCapability_未请求思考不新建Thinking(t *testing.T) {
	req := &rosetta.ChatRequest{Model: "public"}
	ing := &ingressRequest{rawEffort: "high"}
	ing.applyThinkingCapability(req, &routing.UpstreamModel{
		ModelID: "gpt-5", EffortLevels: []effort.Level{effort.Low},
	}, "openai-chat")

	if req.Thinking != nil {
		t.Fatalf("不该凭空创建 Thinking：%+v", req.Thinking)
	}
}

// TestListModels_披露思考开关 是「工具侧那个是否支持思考模式的开关」的
// 数据来源。三态各有各的下发方式，混了会让客户端误判：
//
//	true  → supports_thinking:true
//	false → supports_thinking:false（客户端据此禁用思考选择器）
//	未配置 → **字段缺席**（不是 false：那是「不知道」，报成 false 会让
//	         所有没配过的模型凭空失去思考能力）
func TestListModels_披露思考开关(t *testing.T) {
	sup := func(b bool) *bool { return &b }
	cases := []struct {
		name   string
		sup    *bool
		want   string // 期望出现在响应里的片段
		absent bool
	}{
		{"支持", sup(true), `"supports_thinking":true`, false},
		{"不支持", sup(false), `"supports_thinking":false`, false},
		{"未配置则不下发", nil, "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ri := routing.NewRouteIndex()
			ri.AddProvider(&routing.ProviderRef{ID: "p1", Slug: "prov", Protocol: "openai-chat", Enabled: true})
			ri.AddUpstreamModel(&routing.UpstreamModel{
				ID: "m1", ProviderID: "p1", ModelID: "gpt-4o", Enabled: true,
				SupportsThinking: c.sup,
			})
			ri.AddRoute(&routing.Route{ID: "r1", PublicName: "flash", ProviderID: "p1", UpstreamModelID: "m1", Enabled: true})
			snapshot.Init(&snapshot.Snapshot{
				Routes: ri, Providers: map[string]*snapshot.ProviderSnapshot{},
				KeysByHash: snapshotKeysForTest(), UsersByID: snapshotUsersForTest(),
			})
			t.Cleanup(func() {
				snapshot.Init(&snapshot.Snapshot{
					Routes: routing.NewRouteIndex(), Providers: map[string]*snapshot.ProviderSnapshot{},
					KeysByHash: map[string]*snapshot.KeySnapshot{}, UsersByID: map[string]*snapshot.UserSnapshot{},
				})
			})

			req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
			req.Header.Set("Authorization", "Bearer "+testAccessKey)
			rec := httptest.NewRecorder()
			handleListModels("")(rec, req)

			body := rec.Body.String()
			if c.absent {
				if strings.Contains(body, "supports_thinking") {
					t.Fatalf("未配置时不该下发 supports_thinking：%s", body)
				}
				return
			}
			if !strings.Contains(body, c.want) {
				t.Fatalf("缺少 %s：%s", c.want, body)
			}
		})
	}
}

// TestRawEffort_三个入站协议都带上原始档位
//
// RawEffort 是夹取的数据来源；任何一个协议漏填，该协议的请求就永远
// 不会被夹取（且没有任何症状）。
func TestRawEffort_三个入站协议都带上原始档位(t *testing.T) {
	t.Run("openai-chat", func(t *testing.T) {
		r, err := inwire.DecodeOpenAIChatRequest(
			effortBody(`{"model":"m","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"XHigh"}`), 0)
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		if r.RawEffort != "XHigh" {
			t.Fatalf("RawEffort = %q，期望原样保留大小写", r.RawEffort)
		}
	})

	t.Run("openai-responses", func(t *testing.T) {
		r, err := inwire.DecodeResponsesRequest(
			effortBody(`{"model":"m","input":"hi","reasoning":{"effort":"xhigh"}}`), 0)
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		if r.RawEffort != "xhigh" {
			t.Fatalf("RawEffort = %q", r.RawEffort)
		}
	})

	t.Run("anthropic", func(t *testing.T) {
		// Anthropic 没有 effort 字段：预算 → 档位的映射分界必须与 rosetta
		// 的 effortFromBudget 一致，否则夹紧后的档位与最终发出去的 budget
		// 会对不上（对 OpenAI 系上游而言落地的就是那个 budget）。
		r, err := inwire.DecodeAnthropicMessagesRequest(
			effortBody(`{"model":"m","max_tokens":4096,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"enabled","budget_tokens":32768}}`), 0)
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		if r.RawEffort != "high" {
			t.Fatalf("RawEffort = %q，期望 high（32768 属最高档）", r.RawEffort)
		}

		small, err := inwire.DecodeAnthropicMessagesRequest(
			effortBody(`{"model":"m","max_tokens":4096,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"enabled","budget_tokens":1024}}`), 0)
		if err != nil {
			t.Fatalf("decode small: %v", err)
		}
		if small.RawEffort != "low" {
			t.Fatalf("小预算 RawEffort = %q，期望 low", small.RawEffort)
		}

		// disabled 不请求思考 → 不带档位，夹取不该碰它。
		off, err := inwire.DecodeAnthropicMessagesRequest(
			effortBody(`{"model":"m","max_tokens":4096,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"disabled"}}`), 0)
		if err != nil {
			t.Fatalf("decode off: %v", err)
		}
		if off.RawEffort != "" {
			t.Fatalf("thinking.disabled 不该带档位，实际 %q", off.RawEffort)
		}
	})
}

// effortBody 把一段 JSON 包装成 inwire 的解码器要的 *http.Request。
func effortBody(body string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	return r
}
