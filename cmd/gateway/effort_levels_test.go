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

// TestListModels_下发max原样 钉住「上游报的第五档不会被压掉」。
//
// 用例形状取自实测的一个真实上游：它的
// reasoning_supported_efforts 是 [low,medium,high,xhigh,max]，
// 且 reasoning_default_effort = max。这条测试守的是披露这一环：
// 客户端如果在下拉框里看不到 max，就永远选不到上游默认的那一档。
func TestListModels_下发max原样(t *testing.T) {
	newEffortSnapshot(t, "bunny", "cn:space-bunny",
		[]effort.Level{effort.Low, effort.Medium, effort.High, effort.XHigh, effort.Max})

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+testAccessKey)
	rec := httptest.NewRecorder()
	handleListModels("")(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d body = %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Data) != 1 {
		t.Fatalf("期望 1 个模型，实际 %d", len(out.Data))
	}
	levels, _ := out.Data[0]["supported_reasoning"].([]any)
	if len(levels) != 5 {
		t.Fatalf("supported_reasoning = %v，期望 5 项（含 max）", out.Data[0]["supported_reasoning"])
	}
	if levels[4] != "max" {
		t.Fatalf("最高档下发成了 %v，期望 \"max\"（压成 xhigh 会让客户端选不到上游默认档）", levels[4])
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

// TestApplyThinkingCapability_Anthropic预算与夹取一致 是这次修复的回归守卫。
//
// **旧测试为什么漏掉它**：TestApplyThinkingCapability_档位处理 只断言
// EffortRaw 与 Effort 两个字段，而 Anthropic 分支把这两个都写对了 ——
// 错的是第三个字段 BudgetTokens，而那恰恰是 rosetta plan() 唯一认的那个。
// 于是「日志说已夹到 high、上游实收 medium」这个现象，
// 在只断言 Effort 的测试里完全不可见。
//
// 本测试改为断言**最终发往上游的 budget_tokens**，即整条链路真正的落点：
// 入站预算 → 折算档位（rawEffort）→ 网关夹取 → SDK 三档折算 → 实发预算。
//
// # 2026-10-21 修 A2-P1-3 后的口径变化
//
// 本测试覆盖的是**跨协议**路径（Anthropic 入站 → Anthropic 上游但由
// 「折算档位」驱动的那一支），而 Anthropic→Anthropic 现在走**原样透传**
// （预算不折算，见 TestApplyThinkingCapability_Anthropic预算原样透传）。
//
// 两条路的区分点是 ingressRequest.anthropicBudget：它 > 0 表示
// 「客户端给的是 Anthropic token 预算」，此时网关不折算；本测试构造的
// ingressRequest 只有 rawEffort（anthropicBudget = 0），走的正是
// 「只有档位可用」的那一支 —— 也就是跨协议时会发生的情形。
func TestApplyThinkingCapability_Anthropic预算与夹取一致(t *testing.T) {
	cases := []struct {
		name     string
		budget   int64  // Anthropic 入站的原始预算（inwire 填进 BudgetTokens）
		raw      string // inwire 由 budget 折算出的档位
		levels   []effort.Level
		wantEff  rosetta.Effort
		wantZero bool // BudgetTokens 必须被清零，交回 SDK 折算
	}{
		{
			name:   "超集被夹到 high 后，预算不再沿用客户端原值",
			budget: 5000, // → inwire 折算为 medium
			raw:    "medium",
			// 模型只支持 high：medium 不在集合内，必须夹到 high。
			levels:   []effort.Level{effort.High},
			wantEff:  rosetta.EffortHigh,
			wantZero: true,
		},
		{
			name:   "档位本就在支持集内也要清零，保证落点唯一",
			budget: 40000, // → high，模型支持 high，**不发生夹取**
			raw:    "high",
			levels: []effort.Level{effort.High},
			// 即使没夹取也要清零：否则实发的是 40000 而不是 SDK 折算的 32768，
			// 「档位→预算」的映射就永远存在两条并行的真相。
			wantEff:  rosetta.EffortHigh,
			wantZero: true,
		},
		{
			name:    "夹到 low 时同样不能漏",
			budget:  40000, // → high
			raw:     "high",
			levels:  []effort.Level{effort.Low},
			wantEff: rosetta.EffortLow,
			// 弱化方向同样要生效 —— 只测「变强」会漏掉反向的那一半。
			wantZero: true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := &rosetta.ChatRequest{
				Model: "public",
				Thinking: &rosetta.ThinkingConfig{
					BudgetTokens: int(c.budget),
				},
			}
			ing := &ingressRequest{rawEffort: c.raw}
			ing.applyThinkingCapability(req, &routing.UpstreamModel{
				ModelID: "claude", EffortLevels: c.levels,
			}, "anthropic")

			if req.Thinking.BudgetTokens != 0 {
				t.Fatalf(
					"BudgetTokens = %d，仍是客户端原值 —— rosetta plan() 只认它，"+
						"上面夹取出来的 %q 永远不会生效（上游实收客户端预算）",
					req.Thinking.BudgetTokens, c.wantEff)
			}
			if req.Thinking.Effort != c.wantEff {
				t.Fatalf("Effort = %q，期望 %q", req.Thinking.Effort, c.wantEff)
			}
			if req.Thinking.EffortRaw != "" {
				t.Fatalf("EffortRaw = %q，Anthropic 无此字段，发上去会被拒", req.Thinking.EffortRaw)
			}
		})
	}
}

// TestApplyThinkingCapability_Anthropic预算原样透传 钉住 A2-P1-3 的修复。
//
// 回归风险很具体：预算与档位**不可逆互转**，所以「预算 → 档位 → 预算」
// 这条折返链必然失真，而且两个方向的失真还不一致：
//
//	budget=5000   → medium → 折回 8192   （↑ 客户端要省，网关多花 64%）
//	budget=100000 → high   → 折回 32768  （↓ 客户端要深思考，网关只给 1/3）
//
// Anthropic→Anthropic 不跨协议、客户端给的就是 token 预算、上游收的也该是
// 同一个预算 —— 任何折算都是纯亏损。
//
// 更严重的是它会连带**放大 max_tokens**：SDK 的 plan() 在
// 「budget >= maxTokens」时把 max_tokens 涨到 budget+4096
// （客户端 max_tokens=100 + budget=5000 → 上游收 max_tokens=12288，
// 放大 122 倍）。max_tokens 直接决定上游输出上限与**计费**。
func TestApplyThinkingCapability_Anthropic预算原样透传(t *testing.T) {
	cases := []struct {
		name   string
		budget int
		levels []effort.Level
	}{
		{"预算 2048 落在 low 档", 2048, []effort.Level{effort.Low}},
		// 5000 落在 medium 档，但 SDK 折回 8192 —— 放大 64%。
		{"预算 5000 落在 medium 档", 5000, []effort.Level{effort.Medium}},
		// 100000 落在 high 档，但 SDK 折回 32768 —— 缩到 1/3。
		{"预算 100000 落在 high 档", 100000, []effort.Level{effort.High}},
		// 档位被夹到 low 也不该动预算：夹取描述的是「档位」，
		// 而 Anthropic 上游根本不收档位。
		{"档位被夹到 low 也不动预算", 40000, []effort.Level{effort.Low}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := &rosetta.ChatRequest{
				Model: "public",
				Thinking: &rosetta.ThinkingConfig{
					BudgetTokens: c.budget,
				},
			}
			ing := &ingressRequest{rawEffort: "high", anthropicBudget: c.budget}
			ing.applyThinkingCapability(req, &routing.UpstreamModel{
				ModelID: "claude", EffortLevels: c.levels,
			}, "anthropic")

			if req.Thinking.BudgetTokens != c.budget {
				t.Fatalf("BudgetTokens = %d，期望原样透传 %d —— 折算链会让预算"+
					"被放大或缩小，且两个方向不一致", req.Thinking.BudgetTokens, c.budget)
			}
			if req.Thinking.Effort != rosetta.EffortUnset {
				t.Fatalf("Effort = %q，Anthropic 路径不该留档位（它只认预算）",
					req.Thinking.Effort)
			}
			if req.Thinking.EffortRaw != "" {
				t.Fatalf("EffortRaw = %q，Anthropic 无此字段，发上去会被拒",
					req.Thinking.EffortRaw)
			}
		})
	}
}

// TestApplyThinkingCapability_OpenAI系不动预算 钉住修改的边界：
// BudgetTokens 只有 Anthropic 协议在意，OpenAI 两条路径碰它就等于引入回归。
func TestApplyThinkingCapability_OpenAI系不动预算(t *testing.T) {
	for _, proto := range []string{"openai-chat", "openai-responses"} {
		t.Run(proto, func(t *testing.T) {
			req := &rosetta.ChatRequest{
				Model: "public",
				Thinking: &rosetta.ThinkingConfig{
					BudgetTokens: 5000,
				},
			}
			ing := &ingressRequest{rawEffort: "medium"}
			ing.applyThinkingCapability(req, &routing.UpstreamModel{
				ModelID: "gpt-5", EffortLevels: []effort.Level{effort.High},
			}, proto)

			if req.Thinking.BudgetTokens != 5000 {
				t.Fatalf("%s 路径不该动 BudgetTokens，实际 %d", proto, req.Thinking.BudgetTokens)
			}
			if req.Thinking.EffortRaw != "high" {
				t.Fatalf("%s 路径应把夹取结果原样发在 EffortRaw 上，实际 %q",
					proto, req.Thinking.EffortRaw)
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
// 数据来源。三态各有各的下发方式（2026-10-14 起未配置也下发）：
//
//	true      → supports_thinking:true
//	false     → supports_thinking:false（客户端据此禁用思考选择器）
//	未配置(nil) → supports_thinking:true，但**不下发 supported_reasoning**
//
// # 为什么未配置改成发 true（原来是字段缺席）
//
// 缺席对客户端不是中立的：OpenRouter 约定下多数工具把缺席当「不支持」，
// 于是「未配置」在下游等于「没有思考能力」，而网关的语义是「不干预」。
// 两边对同一个 nil 的解释相反，用户看到的现象是「我明明能思考，工具却不给我开」。
//
// 发 true 不等于替上游承诺档位：supported_reasoning 缺席仍然表示「档位由
// 上游定」，两个字段组合起来正好覆盖三种配置。
//
// 注意这条只管**披露**：数据面仍然只在显式 false 时才剥掉思考
// （见 applyThinkingCapability 与 TestApplyThinkingCapability_显式不支持才剥）。
// 若这里改成发 false 而数据面也照做，全部未配置模型会静默失去思考能力。
func TestListModels_披露思考开关(t *testing.T) {
	sup := func(b bool) *bool { return &b }
	cases := []struct {
		name       string
		sup        *bool
		want       string // 期望出现在响应里的片段
		wantNoLvls bool   // 不该出现 supported_reasoning
	}{
		{"支持", sup(true), `"supports_thinking":true`, true},
		{"不支持", sup(false), `"supports_thinking":false`, true},
		{"未配置按支持下发", nil, `"supports_thinking":true`, true},
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
			if !strings.Contains(body, c.want) {
				t.Fatalf("缺少 %s：%s", c.want, body)
			}
			if c.wantNoLvls && strings.Contains(body, "supported_reasoning") {
				t.Fatalf("没配挡位时不该下发 supported_reasoning：%s", body)
			}
		})
	}
}

// TestApplyThinkingCapability_未配置仍然不干预 钉住披露与数据面的**边界**。
//
// 2026-10-14 把 /v1/models 的「未配置」从字段缺席改成下发 true 之后，
// 极易顺手把数据面也改成「未配置 = 剥掉思考」—— 那会让全部未配置模型
// （含 anthropic 协议的，它们的思考旋钮就是 budget_tokens，剥掉等于彻底关闭）
// 静默失去思考能力，且请求照样成功返回，没有任何错误。
//
// 这里明确只测数据面那一半：未配置 = 网关不干预，原样透传。
func TestApplyThinkingCapability_未配置仍然不干预(t *testing.T) {
	for _, proto := range []string{"openai-chat", "openai-responses", "anthropic"} {
		t.Run(proto, func(t *testing.T) {
			ing := &ingressRequest{}
			req := &rosetta.ChatRequest{}
			req.Thinking = &rosetta.ThinkingConfig{}

			// SupportsThinking 为 nil（未配置），且没有任何 effort_levels。
			ing.applyThinkingCapability(req, &routing.UpstreamModel{
				ModelID: "cn:space-bunny", // 未配置态
			}, proto)

			if req.Thinking == nil {
				t.Fatalf("%s：未配置不应剥掉思考（那是「不知道」，不是「不支持」）", proto)
			}
			if req.Thinking.EffortRaw != "" {
				t.Errorf("%s：未配置时不该替客户端选档位，got %q", proto, req.Thinking.EffortRaw)
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
