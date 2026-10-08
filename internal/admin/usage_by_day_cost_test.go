package admin

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/cn-maul/rosetta-gateway/internal/store"
)

// by-day 的费用列（2026-10-10 新增）。
//
// # 为什么这条要单独钉
//
// 钱包页的「消耗记录」改成按天一行（用户要的是「10-08 花了多少」而不是
// 「10-08 第 37 次花了多少」），而 by-day 此前**只给 count/tokens** ——
// 钱拿不到，那张表只能显示 token，显示不出「这一天花了多少」。
//
// 所以这里测的不是「JSON 里多了一个字段」，而是这个字段的三个不变量。
// 缺任何一个，钱包页都会以一种「看起来正常」的方式骗人。

// newDayCostStore 造一个「输入 1 元/百万 token」的库。
//
// 价必须配上：不配价时 cost_total 恒 0，于是「金额正确」退化成 0 == 0，
// 测试假绿。这与 usage_by_user_test.go 的 newByUserStore 同一个道理，
// 那里已经踩过一次，不要在这里重蹈。
func newDayCostStore(t *testing.T) *store.Store {
	t.Helper()
	st := newTestStore(t)
	if err := st.CreateUpstreamModel(t.Context(), &store.UpstreamModel{
		ID: "priced", ProviderID: "p1", ModelID: "priced", Enabled: true,
		PriceInput: 1.0,
	}); err != nil {
		t.Fatalf("seed priced model: %v", err)
	}
	return st
}

// nearly 比较金额。cost_total 是 REAL，1e-6 元 = 元/1e6，除法之后必然带
// 二进制尾数，直接 == 会在绝大多数合法数据上假失败。
//
// epsilon 取 1e-9：这个量级远小于任何有业务意义的金额（最小计费单位是
// 微元 = 1e-6 元），所以它只吃掉浮点噪声，不会放过真的算错了。
func nearly(got, want float64) bool {
	return math.Abs(got-want) < 1e-9
}

// localDayStart 返回「daysBack 个自然日之前」的**本地 0 点**毫秒时间戳。
//
// 必须自己算本地日界，不能用 now - N*86400_000：后者是滚动 24 小时窗口，
// 在跨日界时会落到「昨天」，于是造出来的数据被后端分到与预期不同的一天
// —— 测试会因「找不到今天那一行」而失败，且报错信息完全指不到真正的原因。
// 口径必须与 store.dayExpr（strftime(...,'localtime')）一致，见 usage_source.go。
func localDayStart(t *testing.T, daysBack int) int64 {
	t.Helper()
	now := time.Now()
	d := time.Date(now.Year(), now.Month(), now.Day()-daysBack, 0, 0, 0, 0, now.Location())
	return d.UnixMilli()
}

// dayCostEntry 是本文件只关心三个字段的最小投影。
type dayCostEntry struct {
	Key    string  `json:"key"`
	Count  int64   `json:"count"`
	Tokens int64   `json:"tokens"`
	Cost   float64 `json:"cost"`
}

// callByDay 调一次 by-day 并解出响应。
//
// to 显式给「明天的 0 点」而不是省略：by-day 的上界过滤对 day 用
// dayEndMs <= to，省略时后端会拿默认窗口（近 7 天）兜底，而本文件的「昨天」
// 恰好落在窗口内所以照样过 —— 但那属于**碰巧**。显式给上界，这个测试
// 才真正在测「我请求的那一段区间」，而不是「后端默认值恰好包含我的数据」。
func callByDay(t *testing.T, h *UsageHandler, as func(*http.Request) *http.Request) []dayCostEntry {
	t.Helper()
	rec := httptest.NewRecorder()
	to := strconv.FormatInt(localDayStart(t, -1)+86_400_000, 10)
	req := httptest.NewRequest(http.MethodGet, "/admin/api/usage/by-day?from=0&to="+to, nil)
	h.GroupByDay(rec, as(req))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var out []dayCostEntry
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, rec.Body.String())
	}
	return out
}

// TestGroupByDay_CostIsSumOfThatDay 钉住「按天的费用 = 那天各条费用之和」。
//
// 这是钱包页那张表的**全部意义**：用户拿它对账，对不上就会怀疑网关没扣钱。
// 三个不变量里它最关键，因为 count/tokens 本来就是对的，只有费用是新加的 ——
// 一个「字段存在但恒为 0」的实现在其它地方都不会露出来。
func TestGroupByDay_CostIsSumOfThatDay(t *testing.T) {
	st := newDayCostStore(t)
	h := NewUsageHandler(st)
	ctx := t.Context()

	// 两天：今天 3 条 10 万 token（各 0.1 元，合计 0.3），昨天 1 条 25 万（0.25 元）。
	// 刻意让两天的**条数与 token 数都不同**：如果实现退化成「按 count 或 tokens
	// 换算费用」，只有两天恰好同比例时才会对，那样测出来是假绿。
	day0 := localDayStart(t, 0)
	for i, ts := range []int64{day0 + 3_600_000, day0 + 7_200_000, day0 + 9_000_000} {
		if err := st.CreateUsageRecord(ctx, &store.UsageRecord{
			ID: "today-" + string(rune('a'+i)), Ts: ts, UserID: "u1",
			AccessKeyID: "k1", PublicModel: "m", ProviderID: "p1", UpstreamModel: "priced",
			IngressProtocol: "openai-chat",
			InputTokens:     100_000, TotalTokens: 100_000,
			Status: "ok", HTTPStatus: 200, UsageState: "reported",
		}); err != nil {
			t.Fatalf("create today usage: %v", err)
		}
	}
	if err := st.CreateUsageRecord(ctx, &store.UsageRecord{
		ID: "yest-0", Ts: localDayStart(t, 1) + 3_600_000, UserID: "u1",
		AccessKeyID: "k1", PublicModel: "m", ProviderID: "p1", UpstreamModel: "priced",
		IngressProtocol: "openai-chat",
		InputTokens:     250_000, TotalTokens: 250_000,
		Status: "ok", HTTPStatus: 200, UsageState: "reported",
	}); err != nil {
		t.Fatalf("create yesterday usage: %v", err)
	}

	got := callByDay(t, h, asAdmin)
	if len(got) != 2 {
		t.Fatalf("应恰好 2 天，实际 %d：%+v", len(got), got)
	}
	// by-day 对 day 用 ORDER BY key ASC，即日期**升序**：昨天在前、今天在后。
	// 断言顺序而不只是断言集合 —— 前端直接按返回顺序渲染（不再自己排序），
	// 顺序错了就是表里日期倒着排。
	if got[0].Cost > got[1].Cost {
		t.Fatalf("日期应升序，实际 %+v", got)
	}
	yest, today := got[0], got[1]
	if yest.Count != 1 || yest.Tokens != 250_000 {
		t.Errorf("昨天应为 1 次 / 250000 token，实际 %d / %d", yest.Count, yest.Tokens)
	}
	if today.Count != 3 || today.Tokens != 300_000 {
		t.Errorf("今天应为 3 次 / 300000 token，实际 %d / %d", today.Count, today.Tokens)
	}
	if !nearly(yest.Cost, 0.25) {
		t.Errorf("昨天费用应 0.25 元，实际 %v", yest.Cost)
	}
	if !nearly(today.Cost, 0.30) {
		t.Errorf("今天费用应 0.30 元（3×0.1），实际 %v", today.Cost)
	}
}

// TestGroupByDay_CostMatchesStats 钉住「按天求和 == stats 的区间合计」。
//
// 钱包页的顶部大卡（总消费）与下面这张按天表**同时**渲染，界面上也写着
// 两者应当一致。一旦有人给某个端点换了费用口径（比如一个读当前单价、
// 一个读固化值），两个数字就会分叉，而用户没有任何办法知道该信哪个。
// 这条断言把「一致」变成构造上必须成立的事实。
func TestGroupByDay_CostMatchesStats(t *testing.T) {
	st := newDayCostStore(t)
	h := NewUsageHandler(st)
	ctx := t.Context()

	// 跨 3 天，且 token 数各不相同（防止「按条数摊」的假实现蒙混过关）。
	for i, back := range []int{0, 1, 3} {
		if err := st.CreateUsageRecord(ctx, &store.UsageRecord{
			ID: "r-" + string(rune('a'+i)), Ts: localDayStart(t, back) + int64(3_600_000*(i+1)),
			UserID: "u1", AccessKeyID: "k1", PublicModel: "m", ProviderID: "p1", UpstreamModel: "priced",
			IngressProtocol: "openai-chat",
			InputTokens:     int64(50_000 * (i + 1)), TotalTokens: int64(50_000 * (i + 1)),
			Status: "ok", HTTPStatus: 200, UsageState: "reported",
		}); err != nil {
			t.Fatalf("create usage: %v", err)
		}
	}

	// 两个口径的合计必须相等。
	//
	// 为什么要真的调 stats 而不是把期望值写死：写死只能证明「按天求和等于
	// 我以为的那个数」，而钱包页顶部大卡读的是 stats。真正要防的是**两个
	// 端点的费用口径分叉**（一个读落库固化的 cost_total、另一个按当前单价
	// 重算），那只在两边并排比较时才看得见。
	sumDayCost := 0.0
	for _, d := range callByDay(t, h, asAdmin) {
		sumDayCost += d.Cost
	}

	// 0.05 + 0.10 + 0.15（第三天 back=3、i=2 → 150000 token）。
	// 这条硬断言挡住「按天表整体少算/多算」；下面那条挡住「按天表对了但 stats 错了」。
	if !nearly(sumDayCost, 0.30) {
		t.Fatalf("按天合计应 0.30 元，实际 %v（逐天：%+v）", sumDayCost, callByDay(t, h, asAdmin))
	}

	statsRec := httptest.NewRecorder()
	statsH := NewStatsHandler(st)
	// from/to 与 callByDay 完全相同，否则比的是两个不同区间。
	to := strconv.FormatInt(localDayStart(t, -1)+86_400_000, 10)
	statsH.Get(statsRec, asAdmin(httptest.NewRequest(http.MethodGet,
		"/admin/api/stats?from=0&to="+to, nil)))
	if statsRec.Code != http.StatusOK {
		t.Fatalf("stats code=%d body=%s", statsRec.Code, statsRec.Body.String())
	}
	var big struct {
		Cost float64 `json:"cost"`
	}
	if err := json.Unmarshal(statsRec.Body.Bytes(), &big); err != nil {
		t.Fatalf("unmarshal stats: %v body=%s", err, statsRec.Body.String())
	}
	if !nearly(big.Cost, sumDayCost) {
		t.Fatalf("两个口径分叉：按天求和 %v vs stats.cost %v（钱包页顶部大卡与下面这张表必须一致）",
			sumDayCost, big.Cost)
	}
}

// TestGroupByDay_ScopedToCaller 钉住普通用户只看到自己那几天的费用。
//
// 与 usage_scope_history_test.go 是同一类断言的另一个端点：by-day 在
// userAccessibleRoutes 白名单里（钱包页普通用户要用），而钱包页这一格
// 显示的是**金额** —— 比 history 的模型名/token 数更敏感。
// 少了这层收窄，一个普通用户就能看到全站每天花了多少钱。
func TestGroupByDay_ScopedToCaller(t *testing.T) {
	st := newDayCostStore(t)
	h := NewUsageHandler(st)
	ctx := t.Context()
	for _, u := range []string{"u1", "u2"} {
		if err := st.CreateUser(ctx, &store.User{
			ID: u, Username: u, PasswordHash: "x",
			Role: store.RoleUser, Status: store.UserStatusActive,
		}); err != nil {
			t.Fatalf("create user %s: %v", u, err)
		}
	}
	// 同一天：u1 花 10 万（0.1 元），u2 花 90 万（0.9 元）。
	ts := localDayStart(t, 0) + 3_600_000
	for _, r := range []*store.UsageRecord{
		{ID: "d-u1", Ts: ts, UserID: "u1", AccessKeyID: "k1", PublicModel: "m", ProviderID: "p1", UpstreamModel: "priced", IngressProtocol: "openai-chat", InputTokens: 100_000, TotalTokens: 100_000, Status: "ok", HTTPStatus: 200, UsageState: "reported"},
		{ID: "d-u2", Ts: ts, UserID: "u2", AccessKeyID: "k2", PublicModel: "m", ProviderID: "p1", UpstreamModel: "priced", IngressProtocol: "openai-chat", InputTokens: 900_000, TotalTokens: 900_000, Status: "ok", HTTPStatus: 200, UsageState: "reported"},
	} {
		if err := st.CreateUsageRecord(ctx, r); err != nil {
			t.Fatalf("create usage %s: %v", r.ID, err)
		}
	}

	admin := callByDay(t, h, asAdmin)
	if len(admin) != 1 || !nearly(admin[0].Cost, 1.0) {
		t.Fatalf("管理员应看到 1 天合计 1.0 元，实际 %+v", admin)
	}

	user := callByDay(t, h, func(r *http.Request) *http.Request { return asUser(r, "u1") })
	if len(user) != 1 {
		t.Fatalf("u1 应只看到 1 天，实际 %d：%+v", len(user), user)
	}
	if !nearly(user[0].Cost, 0.1) {
		t.Fatalf("u1 只应看到自己那 0.1 元，实际 %v —— 金额跨用户泄露", user[0].Cost)
	}
	if user[0].Count != 1 {
		t.Fatalf("u1 的当日次数应为 1，实际 %d", user[0].Count)
	}
}
