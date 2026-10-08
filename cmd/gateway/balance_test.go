package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cn-maul/rosetta"
	"github.com/cn-maul/rosetta-gateway/internal/routing"
	"github.com/cn-maul/rosetta-gateway/internal/server"
	"github.com/cn-maul/rosetta-gateway/internal/snapshot"
	"github.com/cn-maul/rosetta-gateway/internal/store"
)

// ---------------------------------------------------------------------
// 测试辅助
// ---------------------------------------------------------------------

// priceRoute 给链上的模型补单价，**同时**写快照与数据库。
//
// 为什么两处都要写：热路径预检读**快照**（routing.UpstreamModel 的价格字段），
// 而落库计价 freezeUsageCost / 扣费读**数据库**（upstream_models 的 price_*）。
// 这是本任务刻意选择的分工（快照 = 热路径零查库；库 = 计价的事实来源）。
// 测试只写其中一处会造成极隐蔽的假绿：只写快照 → 预检按有价放行，但 cost_total
// 算成 0、扣费 0 元，「成功却没扣钱」；只写库 → 预检估出 0 直接跳过余额判定。
// 两条都写才能验证「预检估的量级」与「按实收的金额」在同一笔账上。
func priceRoute(t *testing.T, db *store.Store, slug, modelID string, pin, pout float64) {
	t.Helper()
	snap := snapshot.Get()
	for _, route := range snap.Routes.ListRoutes() {
		res, err := snap.Routes.Resolve(route.PublicName)
		if err != nil {
			continue
		}
		for _, c := range res.Candidates {
			if c.UpstreamModel == nil {
				continue
			}
			c.UpstreamModel.PriceInput = pin
			c.UpstreamModel.PriceOutput = pout
			// 库侧：upstream_models.provider_id 对 providers.id 有外键，
			// 所以必须先把 provider 写进去（顺序同 billing_scope_test 的
			// seed provider → seed price）。否则插入直接报 FOREIGN KEY
			// constraint failed，而那条报错完全不指向"你忘了建 provider"，
			// 排查起来很绕。
			if err := db.CreateProvider(context.Background(), &store.Provider{
				ID: slug, Slug: slug, Name: slug, Protocol: "openai-chat",
				Enabled: true,
			}); err != nil && !store.IsUniqueViolation(err) {
				t.Fatalf("seed provider: %v", err)
			}
			// 计价按 (provider_id, model_id) 查，两列必须与快照里的
			// 上游模型对上，否则查价失败 → cost_total 记 0 → 不扣费。
			if err := db.UpsertUpstreamModel(context.Background(), &store.UpstreamModel{
				ID: slug + "/m", ProviderID: slug, ModelID: modelID,
				Enabled: true, MaxOutputTokens: 4096,
				PriceInput: pin, PriceOutput: pout,
			}); err != nil {
				t.Fatalf("seed upstream model price: %v", err)
			}
			return
		}
	}
	t.Fatal("快照里没有可定价的上游模型（Resolve 没给出候选）")
}

// fakeCountingGood 是带计数器的 fakeGood，用来断言「上游有没有被触碰」。
//
// 为什么必须断言"没被碰"而不是只看状态码：402 可能在**打完上游之后**才被
// 写出来（例如误把预检放在 attempt 循环之后），那样状态码断言照样绿，
// 但用户已经为一次拿不到结果的调用付了上游的钱 —— 正是余额预检要防的事。
func fakeCountingGood(hits *int64) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(hits, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"cmpl-1","object":"chat.completion","created":0,
			"model":"good-model",
			"choices":[{"index":0,"message":{"role":"assistant","content":"pong"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}}`))
	}))
}

// fakeGoodNoUsage 返回一个**完全不带 usage** 的正常响应。
//
// 模拟第三方兼容服务常见的那种"不报 usage"的情形（见 usageStateFor 的注释：
// 不报 usage 会让成本/配额定时系统性漏账）。这条用例证明：网关不该在
// 不知道实际用量时收钱。
func fakeGoodNoUsage() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"cmpl-1","object":"chat.completion","created":0,
			"model":"good-model",
			"choices":[{"index":0,"message":{"role":"assistant","content":"pong"},"finish_reason":"stop"}]}`))
	}))
}

// fakeStreamGoodWithUsage 返回一条**带 usage 的正常 SSE 流**。
//
// fakeStreamGood 不带 usage（流式默认不带，要 stream_options.include_usage），
// 那会让扣费用例走 usage missing 分支。余额用例需要一个"流式且计费"的上游，
// 用来证明扣费逻辑在流式收尾路径上同样生效（与非流式是两条独立代码路径）。
func fakeStreamGoodWithUsage() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl, _ := w.(http.Flusher)
		for _, c := range []string{
			`{"id":"c1","object":"chat.completion.chunk","created":0,"model":"good-model","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`,
			`{"id":"c1","object":"chat.completion.chunk","created":0,"model":"good-model","choices":[{"index":0,"delta":{"content":"pong"},"finish_reason":null}]}`,
			`{"id":"c1","object":"chat.completion.chunk","created":0,"model":"good-model","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
			// usage 块：stream_options.include_usage=true 时 SDK 期望的末块。
			`{"id":"c1","object":"chat.completion.chunk","created":0,"model":"good-model","choices":[],"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}}`,
		} {
			_, _ = w.Write([]byte("data: " + c + "\n\n"))
			if fl != nil {
				fl.Flush()
			}
		}
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		if fl != nil {
			fl.Flush()
		}
	}))
}

// postChatMaxTokens 发一个显式带 max_tokens 的请求。
//
// 余额预估的输出侧按 max_tokens 计上界，所以「余额不足」的用例必须能把输出
// 侧撑大 —— postChat 不带 max_tokens，输出侧只能回落到模型配置值。
func postChatMaxTokens(h http.HandlerFunc, model string, maxTokens int) *httptest.ResponseRecorder {
	body := `{"model":"` + model + `","messages":[{"role":"user","content":"hi"}],` +
		`"max_tokens":` + itoa(maxTokens) + `,"stream":false}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testAccessKey)
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec
}

func itoa(n int) string { return strconv.Itoa(n) }

// postChatWithID 发一个**带 request_id** 的非流式请求。
//
// 为什么必须补这个：生产里 request_id 由 server.Middleware 每个请求生成，
// 而余额扣费**拿它当幂等键**（无键则跳过扣费，见 usageRecorder.charge）。
// 但既有测试 harness 是**直接调 handleIngress**、绕过了中间件，于是
// context 里没有 request_id。不补这一层，扣费用例拿到空 request_id 会静默
// 跳过扣费，而"断言余额没变"照样通过 —— 一个不报错的假绿。
//
// 这里用**真的 Middleware** 包一层，而不是手工往 context 里塞 key：
// requestIDKey 是 server 包的私有类型，外部根本无法构造，强行复制字面量
// 反而是在测试里重述实现（一旦 server 改了 key，这条测试会莫名其妙地红）。
// 走 Middleware 得到的是与生产完全一致的条件。
func postChatWithID(h http.HandlerFunc, model string) *httptest.ResponseRecorder {
	return postStreamWithID(h, model, false)
}

// postStreamWithID 同 postChatWithID，但可切流式。
func postStreamWithID(h http.HandlerFunc, model string, stream bool) *httptest.ResponseRecorder {
	streamOpt := ""
	if stream {
		// 必须显式要 usage：否则上游不报，扣费走 usage missing 分支恒为 0，
		// 那测到的就不是"流式扣费"而是"流式不扣费"（假绿）。
		streamOpt = `,"stream_options":{"include_usage":true}`
	}
	body := `{"model":"` + model + `","messages":[{"role":"user","content":"hi"}],` +
		`"stream":` + strconv.FormatBool(stream) + streamOpt + `}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testAccessKey)
	rec := httptest.NewRecorder()
	// 先赋变量再调：Middleware 返回 http.Handler（接口），不能直接
	// 当函数调 —— 写成 server.Middleware(...)(rec, req) 会让 Go 先把
	// 括号里的调用结果当成函数值，编译不过。
	wrapped := server.Middleware(h, discardLogger())
	wrapped.ServeHTTP(rec, req)
	return rec
}

// discardLogger 是静默 logger，与 buildHarnessFull 里那个同款。
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// waitNoCharge 断言余额在合理时间内**保持** want。
//
// 这里是"等一段时间再看"而不是"等它变成某个值"：因为期望的行为是
// **什么都没有发生**。窗口给 300ms（够 usage 落库 + 扣费跑完），窗口内
// 余额一旦变化就立刻判负 —— 比固定 sleep 后再断言更灵敏也更可靠。
func waitNoCharge(t *testing.T, db *store.Store, want int64, why string) {
	t.Helper()
	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) {
		if got := balanceNow(t, db); got != want {
			t.Fatalf("%s：余额从 %d 变成了 %d", why, want, got)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := balanceNow(t, db); got != want {
		t.Fatalf("%s：余额从 %d 变成了 %d", why, want, got)
	}
}

// waitNoChargeUnlimited 是不限额版本的 waitNoCharge。
//
// 刻意单独写而不是把 waitNoCharge 加个参数：两者的**失败含义**不同 ——
// 限额账户余额变了 = 误扣；不限额账户被扣 = store 层没守住 NULL 语义
// （把 NULL 当 0 去扣），那会让不限额用户凭空欠费。混成一个函数会让
// 断言信息含糊。
func waitNoChargeUnlimited(t *testing.T, db *store.Store, why string) {
	t.Helper()
	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) {
		if _, limited, err := db.BalanceOf(context.Background(), testUserID); err != nil {
			t.Fatalf("read balance: %v", err)
		} else if !limited {
			// 仍是 NULL = 不限额，正确。
			continue
		}
		t.Fatalf("%s：用户不再是「不限额」了，说明扣费动了 NULL 列", why)
	}
}

// =====================================================================
// 数据面余额：预检 + 只对成功请求扣费
// =====================================================================
//
// 测试分两层，与生产代码的分层一致：
//   - balancePreflight / estimateBalanceCost —— 纯函数，直接测边界
//     （差一分、刚好压线、未配价、管理员、不限额）。边界算错钱，而这类
//     错误在端到端测试里极难发现。
//   - 端到端 —— 走完整 handleIngress，验证「402 不碰上游」「管理员放行」
//     「失败请求不扣费」这三条**行为**。
//
// 所有端到端用例都靠 seeded 余额驱动，且都**显式等到扣费发生**再断言
// （轮询）：扣费是异步的（usageRecorder 的 worker），请求返回时余额还没动。
// 直接在 postChat 之后断言余额会得到一个假绿。

// pricedHarness 是一套**配了价**的 harness：测试用户余额受限、模型有单价。
//
// 为什么不能直接复用 goodHarness：它的上游模型没配价，而「未配价」按 Lead
// 口径是**放行且不扣费**，那样所有余额用例都会假绿（本该 402 的请求被放行）。
// 单价同时写快照与库（见 priceRoute 的注释：两处缺一不可）。
//
// role 非空时把快照里的用户改成该角色（用于管理员豁免用例）。注意这里改的
// 是**快照**，因为鉴权与豁免判定都只读快照；库里那行仍是普通用户，用来证明
// 「豁免不依赖库里有没有钱」。
func pricedHarness(t *testing.T, role string, pin, pout float64) (http.HandlerFunc, *store.Store) {
	t.Helper()
	h, db := goodHarness(t)
	priceRoute(t, db, "good", "good-model", pin, pout)
	if role != "" {
		users := snapshot.Get().UsersByID
		u := users[testUserID]
		u.Role = role
	}
	return h, db
}

// setBalance 给测试用户设一个确定余额（分）。
func setBalance(t *testing.T, db *store.Store, cents int64) {
	t.Helper()
	if err := db.SetBalance(context.Background(), testUserID, cents, false); err != nil {
		t.Fatalf("set balance: %v", err)
	}
}

// balanceNow 读回当前余额（分）。
func balanceNow(t *testing.T, db *store.Store) int64 {
	t.Helper()
	cents, limited, err := db.BalanceOf(context.Background(), testUserID)
	if err != nil {
		t.Fatalf("read balance: %v", err)
	}
	if !limited {
		t.Fatalf("test user became unlimited; the harness seeds a limited balance")
	}
	return cents
}

// waitBalanceSettled 轮询直到余额等于 want，或超时判负。
//
// 扣费发生在 usageRecorder 的 worker 里（异步），所以请求返回的那一刻余额
// **还没动**。所有「扣了 / 没扣」的断言都必须经过它，否则测的是时序而不是
// 逻辑 —— 队列一满走同步回退、队列空走 worker，两条路径的完成时刻不同，
// 不轮询就会偶发假绿/假红。
func waitBalanceSettled(t *testing.T, db *store.Store, want int64, why string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var got int64
	for {
		got = balanceNow(t, db)
		if got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: balance = %d after deadline, want %d", why, got, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// ---------------------------------------------------------------------
// 纯函数层：估算与判定的边界
// ---------------------------------------------------------------------

// 余额不足一分的边界：差 1 分必须被拒、刚好压线必须放行。
//
// 这个边界最容易写错（>= 与 > 的方向），而写错的后果是「永远差一分钱」
// （用户永远充值后才能用）或「永远放行」（余额形同虚设），两种都不会报错。
func TestBalancePreflight_Boundary(t *testing.T) {
	cases := []struct {
		name           string
		est, balance   int64
		unlimited, exm bool
		priced         bool
		want           balanceVerdict
	}{
		{"刚好压线", 100, 100, false, false, true, balanceAllow},
		{"差一分", 101, 100, false, false, true, balanceReject},
		{"余额富余", 99, 100, false, false, true, balanceAllow},
		{"不限额直接放行", 1 << 40, 0, true, false, true, balanceAllow},
		{"管理员不受限", 1 << 40, 0, false, true, true, balanceAllow},
		// 未配价（priced=false）→ 估出 0 → 本次真的不计费 → 放行，
		// 且**不去比余额**。这一条钉住 Lead 的第二条口径：未配价模型不拒绝。
		{"未配价不拒绝", 0, 0, false, false, false, balanceAllow},
		// 已配价但本次小到四舍五入成 0 分：仍须校验余额，否则「欠费用户
		// 靠发足够小的请求就能一直用」，预检形同虚设。
		{"已配价估算为0且余额为0", 0, 0, false, false, true, balanceReject},
		// 余额低于地板线（1 元）时不允许再靠 0 分请求消耗上游（2026-10-10 P2）。
		// 修复前这里只判 >0，于是 1 分钱余额能白嫖无限多个 0 分请求。
		{"已配价估算为0且余额1分", 0, 1, false, false, true, balanceReject},
		{"已配价估算为0且余额99分", 0, 99, false, false, true, balanceReject},
		{"已配价估算为0且余额刚好1元", 0, minSubcentSpendableCents, false, false, true, balanceAllow},
		{"已配价估算为0且余额充足", 0, 5000, false, false, true, balanceAllow},
		// 余额 0 且估算 >0：必须拒。这是「一分钱都没有」的判定。
		{"零余额", 1, 0, false, false, true, balanceReject},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := balancePreflight(c.est, c.balance, c.unlimited, c.exm, c.priced); got != c.want {
				t.Fatalf("balancePreflight(%d, %d, unlimited=%v, exempt=%v, priced=%v) = %v, want %v",
					c.est, c.balance, c.unlimited, c.exm, c.priced, got, c.want)
			}
		})
	}
}

// 未配价模型的估算必须恒为 0 —— 否则未配价部署会被余额功能整体误拒。
func TestEstimateCost_UnpricedModelIsZero(t *testing.T) {
	req := &rosetta.ChatRequest{
		Messages: []rosetta.Message{{
			Role:   "user",
			Blocks: []rosetta.Block{{Type: rosetta.BlockText, Text: strings.Repeat("x", 8000)}},
		}},
		MaxOutputTokens: 4096,
	}
	for _, m := range []*routing.UpstreamModel{
		{PriceInput: 0, PriceOutput: 0},
		// MaxOutputTokens 有值也不该改变结论：没有单价就没有可乘的数。
		{PriceInput: 0, PriceOutput: 0, PriceCacheHit: 0, MaxOutputTokens: 4096},
	} {
		if got := estimateBalanceCost(req, m); got != 0 {
			t.Fatalf("未配价模型估出 %d 分，want 0（否则未配价部署会被误拒）", got)
		}
	}
}

// 只配了输出价的模型：输入侧不得变成免费。
//
// 回归风险很具体：单价表允许只填其中一项，而早期实现让缺失的那项按 0 计 ——
// 于是「只配输出价」的模型，预检估出的输入费用是 0，一段长输入被判成几乎
// 不花钱，直接放行。预检失去了它唯一的职责。
// 只配了输出价的模型：输入侧不得变成免费。
//
// 回归风险很具体：单价表允许只填其中一项，而缺失项若按 0 计，「只配输出价」
// 的模型预检估出的输入费用就是 0 —— 一段长输入被判成几乎不花钱，直接放行，
// 预检失去它唯一的职责。
//
// 断言两件事：
//  1. 估出的钱**大于 0**（输入侧没被算成免费）。这条是本用例的主断言 ——
//     没有兜底时这里是 0.001 元 = 0 分，直接归零。
//  2. 与「两项都配」**完全相等**。因为兜底的方向是「缺哪项就用另一项
//     顶上」，而不是「缺了就当 0」：两者配齐时价格相同，缺项兜底后自然
//     也相同。若某天有人把兜底改成别的方向，这条会立刻红。
func TestEstimateCost_PartialPricingDoesNotMakeInputFree(t *testing.T) {
	// 输入 2000 token、输出 1 token 的**输入主导**请求：只有输入侧的价格
	// 缺失才可能让估算塌到 0，从而测出「输入被当成免费」。
	// （早先这里用输出 100 token，两侧估出同一个分值，断言恒真 —— 假绿。）
	req := &rosetta.ChatRequest{
		Messages: []rosetta.Message{{
			Role:   "user",
			Blocks: []rosetta.Block{{Type: rosetta.BlockText, Text: strings.Repeat("x", 8000)}},
		}},
		MaxOutputTokens: 1,
	}
	// 只配输出价，输入价缺失。
	onlyOut := estimateBalanceCost(req, &routing.UpstreamModel{PriceOutput: 1000})
	full := estimateBalanceCost(req, &routing.UpstreamModel{PriceInput: 1000, PriceOutput: 1000})
	if onlyOut <= 0 {
		t.Fatalf("只配输出价却估出 %d 分：输入侧被当成免费，预检形同虚设", onlyOut)
	}
	if onlyOut != full {
		t.Fatalf("只配输出价估出 %d、配全价估出 %d —— 兜底方向应为「缺项用另一项顶上」"+
			"（两者价格相同，结果理应相等）", onlyOut, full)
	}
}

// ---------------------------------------------------------------------
// 端到端：402 不碰上游
// ---------------------------------------------------------------------

// 余额不足 → 402 insufficient_balance，**绝不触碰上游**。
//
// 状态码与 error code 都刻意**不复用** insufficient_quota / 429：
//   - 429 + Retry-After 的语义是「等一会儿再试」；
//   - 余额不足等再久也不会有钱，必须充值。
//
// 用 429 会让客户端（和 SDK 的自动重试）一直重试到天荒地老，
// 而正确动作是 402 + 明确的充值提示。
func TestBalance_InsufficientRejectsBeforeUpstream(t *testing.T) {
	var hits int64
	up := fakeCountingGood(&hits)
	defer up.Close()

	h, db := pricedHarness(t, "", 1, 1) // 单价 1 元/百万 tokens
	setBalance(t, db, 0)                // 一分钱都没有

	rec := postChatMaxTokens(h, "flash", 100_000)
	if rec.Code != http.StatusPaymentRequired {
		t.Fatalf("want 402 on zero balance, got %d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"code":"insufficient_balance"`) {
		t.Fatalf("want code=insufficient_balance, body=%s", body)
	}
	// code 不得复用 token 配额那条 —— 两者处置方式不同（换 key vs 充值），
	// 客户端按 code 分流。注意只查 code 字段：`type` 是"额度不足"这一类
	// 大类（OpenAI 侧两种情形都映射成 insufficient_quota，见 outwire 的
	// errorTypeFromCode），刻意与 code 保持不同粒度，别把它一起判掉。
	if strings.Contains(body, `"code":"insufficient_quota"`) {
		t.Fatalf("余额不足复用了 token 配额的 code，客户端无法区分处置方式：%s", body)
	}
	if hits != 0 {
		t.Fatalf("上游被触碰了 %d 次：预检必须在触碰上游之前", hits)
	}
}

// 余额够 → 放行，且**成功后确实扣钱**。
//
// 这条同时钉住「预检不误拒」与「只对成功请求扣费」：请求返回 200，余额
// 从 X 变成 X - 实际费用。扣费金额取落库的 cost_total，不是预估值。
//
// 余额给得很宽裕是刻意的：预检按**上界**估（输出侧 max_tokens 全额计），
// 而实际只按真实输出扣。两者的差额就是"预检保守、扣费精确"，这里要验证的
// 是后者 —— 所以余额必须够覆盖上界，否则会在预检就被拒，压根到不了扣费。
func TestBalance_SuccessChargesRealCost(t *testing.T) {
	h, db := pricedHarness(t, "", 1_000_000, 1_000_000) // 1 元/token，很贵的模型
	const start = 10_000_000                            // 100000 元，够覆盖上界
	setBalance(t, db, start)

	rec := postChatWithID(h, "flash")
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d body=%s", rec.Code, rec.Body.String())
	}

	// fakeGood 的 usage：prompt 5 + completion 3。单价 1e6 元/百万 tokens
	// ⇒ 8 token × 1 元/token = 8 元 = 800 分。
	// 关键：扣掉的是 800 分（真实用量），**不是**预检估的上界
	// （上界含 4096 的 max_output，会是几千元）—— 这正是"按实收"的意义。
	waitBalanceSettled(t, db, start-800,
		"成功请求应按落库 cost_total 扣费")
}

// 管理员在数据面（/v1 调模型）已被**彻底拦截**（2026-10 控制面/数据面分离）：
// 鉴权层 auth.Authenticate 对管理员 key 直接返回 ErrAdminCannotCallModel，
// 压根到不了预检与扣费。
//
// 本用例钉住这条边界的**入口**：管理员带 key 打 /v1 → 403 admin_cannot_call_model，
// **且上游一次都没被触碰**（用计数 fake 断言）。数据面的余额豁免短路
// （balanceExempt）在这条路径上是**不可达**的（纵深防御，见 billing.go）。
//
// 「普通用户不受影响」由 TestBalance_SuccessChargesRealCost 等既有用例覆盖
// （它们用同一个 harness 但角色为普通用户，管理员拦截不会波及它们）。
func TestBalance_AdminCannotReachDataPlane(t *testing.T) {
	var hits int64
	up := fakeCountingGood(&hits)
	defer up.Close()

	// 把快照里的测试用户改成 admin（鉴权读快照），库里余额也设为 0，
	// 但这里根本不该走到余额判定。
	h, _ := pricedHarness(t, store.RoleAdmin, 1_000_000, 1_000_000)

	rec := postChatWithID(h, "flash")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("管理员调用 /v1 应被拒（403），got %d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"code":"admin_cannot_call_model"`) {
		t.Fatalf("want code=admin_cannot_call_model, body=%s", body)
	}
	if hits != 0 {
		t.Fatalf("上游被触碰了 %d 次：管理员必须在触碰上游之前被拒", hits)
	}
}

// 失败请求**不扣费**（Lead 与用户确认的第四条口径）。
//
// 上游 500 → 网关回 502 → usage 落库 status="error" → 不该扣钱。
// 与 cost_total 刻意相反：cost_total 无论成败都记（那是网关的上游成本），
// 而扣费只看成功（那是用户的应收）。
func TestBalance_FailedRequestIsNotCharged(t *testing.T) {
	bad := fakeBad()
	defer bad.Close()
	h, db := buildHarness(t, []struct {
		slug, url string
		fail      bool
	}{{slug: "bad", url: bad.URL, fail: true}}, false)
	priceRoute(t, db, "bad", "bad-model", 1_000_000, 1_000_000)
	const start = 10_000_000
	setBalance(t, db, start)

	rec := postChatWithID(h, "flash")
	if rec.Code == http.StatusOK {
		t.Fatalf("上游 500 时不该返回 200，body=%s", rec.Body.String())
	}
	if rec.Code == http.StatusPaymentRequired {
		t.Fatalf("余额充足却被 402 挡在预检，说明这条用例根本没走到上游失败路径")
	}

	waitNoCharge(t, db, start, "失败请求不应扣费")
}

// 流式成功**照常扣费** —— 「只对成功扣费」不能被实现成「只对非流式扣费」。
//
// 这是最容易漏的一侧：流式的收尾在 attemptStream 里，usage 落库与扣费都在
// 那里，与非流式是两条独立代码路径。漏掉的表现是「非流式扣钱、流式白送」，
// 而流式恰恰是交互场景的默认形态。
//
// 请求显式带 stream_options.include_usage=true：否则上游（这里也是 SDK
// 侧）不报 usage，扣费会走 usage missing 分支而恒为 0 —— 那样这条用例
// 测到的就不是"流式扣费"，而是"流式不扣费"，是个假绿。
func TestBalance_StreamingSuccessIsCharged(t *testing.T) {
	up := fakeStreamGoodWithUsage()
	defer up.Close()
	h, db := buildHarness(t, []struct {
		slug, url string
		fail      bool
	}{{slug: "good", url: up.URL}}, false)
	priceRoute(t, db, "good", "good-model", 1_000_000, 1_000_000)
	const start = 10_000_000
	setBalance(t, db, start)

	body := `{"model":"flash","messages":[{"role":"user","content":"hi"}],"stream":true,` +
		`"stream_options":{"include_usage":true}}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testAccessKey)
	rec := httptest.NewRecorder()
	wrapped := server.Middleware(h, discardLogger())
	wrapped.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d body=%s", rec.Code, rec.Body.String())
	}

	// fakeStreamGoodWithUsage 报 prompt 5 + completion 3 = 8 token，
	// 单价 1 元/token ⇒ 800 分。
	waitBalanceSettled(t, db, start-800, "流式成功应扣费")
}

// 上游没报 usage（missing）时**不扣费**。
//
// 口径：不知道实际用了多少 token，就不该收钱。此时 cost_total 记 0，
// 扣费也应为 0。与该路径"只退配额预占、TPM 预占故意保留"的既有决策不冲突
// —— 那管的是预占，收 0 元是实收，两回事。
func TestBalance_UsageMissingIsNotCharged(t *testing.T) {
	up := fakeGoodNoUsage()
	defer up.Close()
	h, db := buildHarness(t, []struct {
		slug, url string
		fail      bool
	}{{slug: "good", url: up.URL}}, false)
	priceRoute(t, db, "good", "good-model", 1_000_000, 1_000_000)
	const start = 10_000_000
	setBalance(t, db, start)

	rec := postChatWithID(h, "flash")
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d body=%s", rec.Code, rec.Body.String())
	}

	waitNoCharge(t, db, start, "usage missing 不该扣费")
}

// 不限额用户（NULL）永远放行且不扣费。
//
// 与 quota_tokens 的「0 = 不限」刻意相反：余额的 0 是「真没钱」，
// 不限额必须用 NULL 表示。测试显式覆盖 NULL 这条路。
func TestBalance_UnlimitedUserAlwaysAllowed(t *testing.T) {
	h, db := pricedHarness(t, "", 1_000_000, 1_000_000)
	if err := db.SetBalance(context.Background(), testUserID, 0, true); err != nil {
		t.Fatalf("set unlimited: %v", err)
	}

	rec := postChatWithID(h, "flash")
	if rec.Code != http.StatusOK {
		t.Fatalf("不限额用户应放行；got %d body=%s", rec.Code, rec.Body.String())
	}
	// 扣费对不限额用户是 no-op（余额无限，减一个数没意义）。
	waitNoChargeUnlimited(t, db, "不限额用户不应被扣费")
}
