// Package billing contains pure pricing and balance-preflight policy.
// It has no HTTP, snapshot-global, or process-lifecycle dependencies.
package billing

import (
	"strings"

	"github.com/cn-maul/rosetta"
	"github.com/cn-maul/rosetta-gateway/internal/routing"
	"github.com/cn-maul/rosetta-gateway/internal/store"
)

// IsBalanceExempt 报告该角色是否**不受余额约束**（管理员）。
//
// 调用方从当前运行时用户视图取角色；billing 包本身不读全局快照。
//
// # 2026-10 起这是**纵深防御**，数据面上不可达
//
// 控制面/数据面分离后，管理员在 auth.Authenticate 就被拒（ErrAdminCannot-
// CallModel），压根到不了预检与扣费。所以这里判 admin 的两处短路在数据面
// 上都是死代码。但**刻意保留**：
//
//   - 余额是「钱」。多保留一道零成本的短路，等于「即使上游拦截被绕过（重构、
//     新数据面端点、内部复用），管理员也永远不会被读库判定挡掉、或被扣费」。
//     删掉它换来的只是一点点「整洁」，却让资金正确性依赖一个跨文件的假设
//     「auth 那一层一定会先拦住」。纵深防御的代价只有几行，收益是失败模式
//     从「静默超支/扣错钱」变成「被短路挡住」。
//   - 它同样是纯内存判定，不给数据面每个普通请求加任何开销（只在命中 admin
//     时才 early-return；普通用户走原路径）。
//
// 与 orgCostsScope 的关系：那里也判 admin（看全量费用），但那是**控制面**
// 语义（管理员在管理端看全局账），不走 Authenticate，所以那条路径仍是可达
// 的 —— 别因为数据面管理员被拦就把那条也一起关掉。
func IsBalanceExempt(role string) bool { return role == store.RoleAdmin }

// estimateBalanceCost 估算「本次请求大概要花多少钱」（单位：**分**）。
//
// 输入侧与 estimateRequestTokens 同思路（文本/思考/工具参数/工具定义的字符
// 估算），输出侧按 max_tokens **全额计上界** —— 输出长度事前不可知，流式更是
// 边生成边计费，取上界是唯一诚实的做法：宁可高估把请求挡在门外（用户充值后
// 重试），也不要低估放行后才发现钱不够。
//
// 缓存命中在预检里**无法预知**（要看上游命中情况），因此按「全部未命中」的
// 最高价计。这同样是有意的保守：缓存命中单价通常低于输入价，按未命中算是
// 上界。真实扣费用的是落库时那个含缓存命中的 cost_total，两者差额就是
// 「预检保守、扣费精确」的体现，不是偏差。
//
// 返回值单位是**整数分**，与 store 的 balance_cents 对齐：预检不引入第二套
// 货币单位，避免「元/分」在两处各算一遍后出现舍入差（预检以为够、实际差
// 一分钱被拒）。
//
// # 计价公式现在**与实扣同源**（2026-10-21 修 A4-P1-3 / 汇总 F3）
//
// 审计前这里是**手抄的第二份公式**，兜底规则与实扣的 store.priceUsage
// 不同（少了缓存价这一项），后果是：只配了 price_cache_hit 的模型在预检
// 侧被判成「完全未配价」→ 提前 return 0 → **fail-open 漏放行**（余额 1 元
// 的用户可以持续刷到余额耗尽，而实扣照收缓存价）。
//
// 现在两侧都走 store.Prices / store.EstimateUpperBoundCost：
//   - 单价来自同一份 Prices，兜底规则**逐字一致**；
//   - 实扣用 store.PriceUsage，预检用 store.EstimateUpperBoundCost，
//     两者共用同一份 EffectivePrices 解析结果。
//
// 换句话说：预检估出的值 **≥** 实扣值（保守方向仍然正确），但
// 「哪些 token 收费」的口径不再可能分叉。
func EstimateBalanceCost(req *rosetta.ChatRequest, m *routing.UpstreamModel) int64 {
	var b strings.Builder
	b.WriteString(req.System)
	for _, msg := range req.Messages {
		for _, blk := range msg.Blocks {
			b.WriteString(blk.Text)
			b.WriteString(blk.Thinking)
			b.WriteString(blk.Content)
			b.WriteString(blk.Arguments)
		}
	}
	for _, t := range req.Tools {
		b.WriteString(t.Name)
		b.WriteString(t.Description)
		b.WriteString(string(t.Parameters))
	}
	inTokens := int64(rosetta.EstimateTokens(b.String()))
	// 输出侧按 max_tokens 全额计上界；客户端没给（<=0）时回落模型配置的最大
	// 输出 —— 都拿不到就只按输入算（下界，宁可不高估）。
	maxOut := int64(req.MaxOutputTokens)
	if maxOut <= 0 {
		maxOut = int64(m.MaxOutputTokens)
	}
	cost := store.EstimateUpperBoundCost(store.Prices{
		Input:    m.PriceInput,
		CacheHit: m.PriceCacheHit,
		Output:   m.PriceOutput,
	}, inTokens, maxOut)
	return YuanToCents(cost)
}

// estimateBalanceCostChain 估算一条**链**最贵的那次调用要花多少钱（分）。
//
// 为什么取整条链的最大值而不是链首：故障转移下真正服务的可能是链上任意
// 一个目标，各目标的模型单价可以差很多（同一公开名背后可能是廉价模型与
// 昂贵模型各一）。按链首估会在「转移到了贵目标」时低估，用链首的预算
// 放行了一笔实际付不起的钱 —— 正是预检要防的事。
//
// 取上界的代价是可能误拒（用户其实付得起链首那笔）。这个方向是安全的：
// 误拒可充值后重试，漏放则是欠费。
//
// 整条链都没配价时返回 0 → 调用方判定为「本次不会扣费」而跳过余额读取。
func EstimateBalanceCostChain(req *rosetta.ChatRequest, cands []routing.Candidate) int64 {
	var max int64
	for _, c := range cands {
		if c.UpstreamModel == nil {
			continue
		}
		if e := EstimateBalanceCost(req, c.UpstreamModel); e > max {
			max = e
		}
	}
	return max
}

// yuanToCents 把「元」（浮点）换算成「整数分」。
//
// 直接委托 store.YuanToCents 而不自己写：预检估的与扣费扣的**必须**是同
// 一次换算，否则会出现「预检算 0.005 元、实际扣 1 分」的错位，让「刚好
// 够」的用户被莫名其妙拒掉。store 那边是唯一实现，这里只做转接。
//
// 为什么要在入口就取整成整数分：预检判定与实际扣费用的都是整数分
// （balance_cents 就是整数分列）。若预检拿浮点元去比一个整数分的余额，
// 边界上会出现「预检算的 0.004 元、余额 0 分」的浮点比较歧义。统一在入口
// 取整，让两侧做同一种整数比较。
func YuanToCents(y float64) int64 { return store.YuanToCents(y) }

// centsToYuan 是 yuanToCents 的反向（展示/日志用）。
func CentsToYuan(c int64) float64 { return float64(c) / 100 }

// balanceVerdict 是预检的结论，供 handleIngress 决定放行还是拒绝。
type Verdict int

const (
	Allow Verdict = iota
	// Reject 表示余额不足，应回 402。
	Reject
)

// MinSubcentSpendableCents 是「计费模型上，本次估算不足 1 分」时，
// 用户余额至少要有的分数（2026-10-10 修复的 P2）。
//
// # 为什么不是原来的 > 0
//
// 原判定是「余额 > 0 即放行」，而扣费侧 ChargeBalance 对 amountCents<=0
// 直接 no-op。于是 1 分钱的余额可以发出**无限多个**四舍五入成 0 分的请求：
// 每次都白嫖，却每次都真的消耗上游 token、也每次都写一条 usage/cost_total。
// 单价越便宜的模型越容易触发（单价 0.1 元/百万 token 时，几百个 token 的
// 普通请求都不到半分）—— 而「低价模型 + 多租户」正是余额计费的目标场景。
// 也就是说这道预检在**最该生效的部署形态**里被绕开了。
//
// # 为什么取 100 分（1 元）而不是 1 分
//
// 修复前想要的是「零余额不得白嫖」；现在这一条已经由 balanceCents >= 1
// 覆盖（估 0 分时余额为 0 仍拒）。100 分这条**只针对仍有余额**的用户，
// 目的是把「持续白嫖」与「偶发一次小请求」分开：
//
//   - 偶发小请求（余额充足、只是这次请求太小）→ 放行，不打扰；
//   - 余额已经低到 1 元以下 → 不允许再靠 0 分请求继续消耗上游，
//     直到充值。
//
// 也就是说它是一条**地板线**，不是新的计费口径：正常计费的请求（estCents>0）
// 完全不经过它，仍按「余额 >= 本次估算」判定。
const MinSubcentSpendableCents = 100 // 1 元

// balancePreflight 在触碰上游**之前**判断「这次调用该不该被余额拦住」。
//
// 纯函数：不碰 DB、不碰全局。判定逻辑单独抽出来是为了能直接单测边界
// （差一分钱、刚好等于、无限额、管理员），而不必为每个边界都搭一套
// 上游 harness —— 边界算错钱，而这类错误在端到端测试里极难发现。
func Preflight(estCents, balanceCents int64, unlimited, exempt bool, priced bool) Verdict {
	if exempt || unlimited {
		return Allow
	}
	if estCents <= 0 {
		// 估出 0。两种含义，调用方用 priced 区分：
		//
		//   - priced=false：整条链都没配价，本次真的不计费 → 放行。
		//   - priced=true ：模型配了价，只是这次请求太小，四舍五入成 0 分。
		//     放行等于「欠费用户靠发足够小的请求就能一直用」——预检形同虚设。
		//     这种一律按「余额必须 >= MinSubcentSpendableCents」判定：
		//     欠费用户连白嫖都不行。
		if !priced {
			return Allow
		}
		if balanceCents >= MinSubcentSpendableCents {
			return Allow
		}
		return Reject
	}
	if balanceCents >= estCents {
		return Allow
	}
	return Reject
}

// chainHasPricedModel 报告这条链上是否存在**配置过价格**的上游模型。
//
// 与「估算值是否大于 0」严格区分：后者会因为「小请求四舍五入成 0」而
// 为 false，前者反映的是「这个模型到底有没有价格」。调用方靠它区分
// 「本次真的不计费」与「计费但恰好算成 0 分」——后者仍须校验余额。
func ChainHasPricedModel(cands []routing.Candidate) bool {
	for _, c := range cands {
		if c.UpstreamModel == nil {
			continue
		}
		if (store.Prices{
			Input:    c.UpstreamModel.PriceInput,
			CacheHit: c.UpstreamModel.PriceCacheHit,
			Output:   c.UpstreamModel.PriceOutput,
		}).Priced() {
			return true
		}
	}
	return false
}
