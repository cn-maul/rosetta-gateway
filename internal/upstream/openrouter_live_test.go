package upstream

// 对**真实** OpenRouter /models 响应的解析验证。
//
// # 为什么需要联网跑
//
// openrouter_reasoning_test.go 用的是节选夹具，而夹具是我从当时的响应里
// 手抄的。抄写过程本身可能漏掉字段、也可能在我抄完之后上游又改了形状 ——
// 而这个 bug 的本质正是「我们以为的形状 ≠ 上游实际的形状」。
// 拿真实响应跑一遍，是唯一能证伪「夹具恰好对上了」的手段。
//
// 默认**跳过**：CI 不该依赖第三方公网 API（对方挂了/限流/改形状都会让
// 我们的 CI 变红，而这不是我们代码的问题）。需要时用环境变量显式开启：
//
//	OPENROUTER_LIVE=1 go test ./internal/upstream/ -run Live -v
//
// 它的定位是「上游改形状时的第一道警报」，不是常规回归 —— 常规回归由
// 上面那个夹具测试负责，那个不联网、每次都跑。

import (
	"net/http"
	"os"
	"testing"
)

func TestFetchModels_OpenRouterLive(t *testing.T) {
	if os.Getenv("OPENROUTER_LIVE") == "" {
		t.Skip("设 OPENROUTER_LIVE=1 才会跑（依赖第三方公网 API）")
	}

	cands, err := fetchModels(t.Context(), http.DefaultClient,
		"https://openrouter.ai/api/v1/models", "openai-chat", "sk-not-needed-for-models")
	if err != nil {
		t.Fatalf("fetchModels 失败: %v", err)
	}
	t.Logf("解析成功：%d 个模型", len(cands))
	if len(cands) == 0 {
		t.Fatal("一个模型都没解析出来")
	}

	// 统计几项，确认「上游报了就能用」这个承诺真的成立了。
	var withThinking, withLevels int
	for _, c := range cands {
		if c.SupportsThinking != nil && *c.SupportsThinking {
			withThinking++
		}
		if len(c.EffortLevels) > 0 {
			withLevels++
		}
	}
	t.Logf("认成支持思考的: %d；带 effort 档位的: %d", withThinking, withLevels)

	// 这两个数不该是 0：OpenRouter 有大量模型带 reasoning 对象，
	// 若解析路径没把对象拆开，它们会全部退化成「不知道」。
	if withThinking == 0 {
		t.Error("没有任何模型被认成支持思考 —— reasoning 对象多半没被拆开")
	}
	if withLevels == 0 {
		t.Error("没有任何模型带 effort 档位 —— supported_efforts 多半没被取出")
	}
}
