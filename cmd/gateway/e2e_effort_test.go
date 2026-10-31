package main

// 思考档位（reasoning effort）落库的端到端守护。
//
// # 为什么需要这一条
//
// 这条链路跨越四层：客户端请求字段 → inwire 解析 → ApplyThinkingCapability
// 夹取/映射 → diagnostics 快照 → usage_records.effort → 管理面 JSON → 前端
// 胶囊。此前每一层都有单测，但**没有一条用例把它们接起来**。
//
// 单测绿而链路断是完全可能的：diagnostics.setIngressRequest 只要被挪到
// ApplyThinkingCapability 之前，拿到的就是客户端原始值（未经夹取），所有
// 单测照样过 —— 因为单测直接调 EffortLabel 喂的是已经处理好的配置。这条
// 用例从 HTTP 入口打进去、从数据库读出来，中间任何一步错位都会红。
//
// # 为什么断言三种输入
//
//   - high        普通档位，走 EffortRaw（非 anthropic 协议）
//   - minimal     超出 rosetta SDK 三档枚举（low/medium/high）的值。它必须
//     经 EffortRaw 原样保留，证明网关没有把它压成 medium
//   - 不带该字段  必须落空串。网关**不得**替客户端编造默认 medium ——
//     编造会让「没要求思考」和「要求了中等思考」在排障时无法区分

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/cn-maul/rosetta-gateway/internal/effort"
	"github.com/cn-maul/rosetta-gateway/internal/snapshot"
	"github.com/cn-maul/rosetta-gateway/internal/store"
)

// effortCases 是三种典型输入的期望落库值。
var effortCases = []struct {
	name string
	// body 是接在 stream 字段之后的 JSON 片段。
	body       string
	wantEffort string
	why        string
}{
	{
		name: "high", body: `,"reasoning_effort":"high"`, wantEffort: "high",
		why: "常规档位必须原样落到补给侧的排障视图里",
	},
	{
		name: "minimal", body: `,"reasoning_effort":"minimal"`, wantEffort: "minimal",
		why: "SDK 只有三档枚举，minimal 超纲 —— 必须经 EffortRaw 原样保留而不是压成 medium",
	},
	{
		name: "absent", body: "", wantEffort: "",
		why: "没要求思考就落空串，网关不替客户端编造默认档位",
	},
}

// TestE2E_EffortRecordedFromClientRequest 走完整数据面：客户端给
// reasoning_effort，落库 effort 必须是实际上游的那一档。
func TestE2E_EffortRecordedFromClientRequest(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, tc := range effortCases {
			name := fmt.Sprintf("%s/stream=%t", tc.name, stream)
			t.Run(name, func(t *testing.T) {
				// 流式与非流式是两条独立的收尾路径（attemptStream 与
				// attemptNonStream 各写一次 Record），必须分别跑。
				up := newFakeUpstream(t, pickReply(stream, "pong"))
				h, db := buildHarness(t, []struct {
					slug, url string
					fail      bool
				}{{slug: "good", url: up.URL()}}, false)

				priceRoute(t, db, "good", "good-model", 1_000_000, 1_000_000)
				e2eSetBalance(t, db, 10_000)

				body := `{"model":"flash","messages":[{"role":"user","content":"hi"}],` +
					fmt.Sprintf(`"stream":%t`, stream) + tc.body + `}`
				rec := e2ePost(h, "/v1/chat/completions", testAccessKey, body)
				if rec.Code != http.StatusOK {
					t.Fatalf("%s 期望 200，实际 %d，body=%s", name, rec.Code, rec.Body.String())
				}

				rows := e2eWaitUsageCount(t, db, "k1", 1)
				if len(rows) != 1 {
					t.Fatalf("%s 期望 1 行 usage_records，实际 %d 行", name, len(rows))
				}
				if got := effortOf(t, db, rows[0].ID); got != tc.wantEffort {
					t.Fatalf("%s 落库 effort = %q，期望 %q —— %s",
						name, got, tc.wantEffort, tc.why)
				}

				// 上游确实收到了一次请求，否则「落库正确」可能来自一条
				// 根本没转发的路径。
				if up.Count() != 1 {
					t.Fatalf("%s 上游被调用 %d 次，期望 1 次", name, up.Count())
				}
			})
		}
	}
}

// TestE2E_EffortClampedByModelCapability 证明落库的是**夹取后**的值。
//
// 这是上面那个「单测绿而链路断」风险的正面反例：模型声明的支持集是
// [low, medium]，客户端要 xhigh，实际发出去以及落库的必须是 medium。
// 若 diagnostics 拿的是夹取前的请求，这里会读到 xhigh。
func TestE2E_EffortClampedByModelCapability(t *testing.T) {
	up := newFakeUpstream(t, replyOK("pong"))
	h, db := buildHarness(t, []struct {
		slug, url string
		fail      bool
	}{{slug: "good", url: up.URL()}}, false)

	// buildHarness 造的模型没有 effort_levels 配置（= 不夹取），这里把快照里
	// 那个模型的支持集改成 [low, medium]。改快照而不是库：数据面热路径只读
	// 快照（见 internal/routing/routing.go 的 UpstreamModel.EffortLevels 注释）。
	bindEffortLevels(t, "good", "good-model", []string{"low", "medium"})

	priceRoute(t, db, "good", "good-model", 1_000_000, 1_000_000)
	e2eSetBalance(t, db, 10_000)

	rec := e2ePost(h, "/v1/chat/completions", testAccessKey,
		`{"model":"flash","messages":[{"role":"user","content":"hi"}],`+
			`"stream":false,"reasoning_effort":"xhigh"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d，body=%s", rec.Code, rec.Body.String())
	}

	rows := e2eWaitUsageCount(t, db, "k1", 1)
	if got := effortOf(t, db, rows[0].ID); got != "medium" {
		t.Fatalf("落库 effort = %q，期望 \"medium\"（xhigh 应被夹到模型支持集里最接近的一档）；"+
			"若读到 xhigh，说明 diagnostics 记的是夹取前的请求", got)
	}
}

// effortOf 直接读一行的 effort 列。
//
// 刻意不扩 e2eUsageRow：那个结构被十来个既有用例共用，加字段要同步改它们
// 的 SELECT 列清单，收益不抵风险。
func effortOf(t *testing.T, db *store.Store, id string) string {
	t.Helper()
	var v string
	if err := db.DB().QueryRowContext(context.Background(),
		`SELECT COALESCE(effort, '') FROM usage_records WHERE id = ?`, id).Scan(&v); err != nil {
		t.Fatalf("读 effort 失败: %v", err)
	}
	return v
}

// bindEffortLevels 改写当前快照里某个上游模型声明的思考挡位支持集。
//
// 直接改快照对象而不是库：数据面热路径只读快照，重建流程本身已有单测覆盖
// （internal/snapshot）。这里要验证的是「夹取后落库」这一段，不是重建。
func bindEffortLevels(t *testing.T, providerID, modelID string, levels []string) {
	t.Helper()
	m := snapshot.Get().Routes.FindUpstreamModelByModelID(providerID, modelID)
	if m == nil {
		t.Fatalf("快照里找不到上游模型 %s/%s —— harness 没装好", providerID, modelID)
	}
	m.EffortLevels = nil
	for _, l := range levels {
		m.EffortLevels = append(m.EffortLevels, effort.Level(l))
	}
}

// pickReply 按流式与否选假上游的响应构造器。
func pickReply(stream bool, content string) func(http.ResponseWriter, *http.Request) {
	if stream {
		return replySSE(content)
	}
	return replyOK(content)
}
