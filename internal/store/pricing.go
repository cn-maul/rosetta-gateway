package store

// 全仓唯一的计价口径（2026-10-21 修 A4-P1-3 / 汇总 F3）。
//
// # 为什么把它单独抽成一个文件
//
// 审计前的现状是**两份计价公式**：
//
//   - cmd/gateway/billing.go 的 estimateBalanceCost —— 余额**预检**用；
//   - internal/store/usage_dao.go 的 priceUsage —— 落库**实扣**用。
//
// 两者必须永远相等：预检按 A 估、实扣按 B 收，预检就失去意义。
// 而「相等」这件事没有任何机制保证 —— 注释写了「改一侧必须同步另一侧」，
// 这靠人记。审计里就找到了一处真实漂移：**只配了 price_cache_hit** 的模型，
// 预检因 inPrice<=0 && outPrice<=0 提前返回 0（fail-open，漏放行），
// 实扣却按缓存价照收 —— 余额 1 元的用户可以持续刷到余额耗尽。
//
// 现在两侧都调用本文件的口径，漂移在结构上不再可能。
//
// # 三个层次的划分
//
//   - PriceUsage：唯一的价格公式（元）。落库计价与余额预检共用。
//   - EffectivePrices：把「未配置的单价」解析成**实际生效价**。
//     落库侧与预检侧都要用它，保证「哪些算钱、哪些免费」的口径一致。
//   - EstimateUpperBoundCost：预检专用的**上界**估算（输出按 max_tokens 全额）。
//
import "math"

// PriceUnitDivisor 是「元/百万 tokens」换算成「元/token」的除数。
const PriceUnitDivisor = 1_000_000.0

// Prices 是一组**原始**单价（元/百万 tokens）。
//
// 四个字段与 upstream_models 的 price_input / price_cache_hit /
// price_cache_creation / price_output 一一对应；0 表示未配置
// （不是「免费」，见 EffectivePrices）。
type Prices struct {
	Input         float64
	CacheHit      float64
	CacheCreation float64
	Output        float64
}

// EffectivePrices 是**实际生效**的单价。
type EffectivePrices struct {
	Input         float64 // 缓存未命中输入的生效单价
	CacheHit      float64 // 缓存命中（读取）输入的生效单价
	CacheCreation float64 // 缓存写入（creation）输入的生效单价
	Output        float64 // 输出的生效单价
}

// Priced 报告这组单价是否**真的配了价**。
//
// 语义是「这个模型会不会产生费用」，而不是「这次调用花了多少」。
// 预检用它区分两件完全不同的事：
//   - priced=false：整条链都没配价 → 本次真的不计费 → 直接放行、不读余额；
//   - priced=true ：配了价，只是这次请求小到四舍五入成 0 分 → 仍须校验余额。
func (p Prices) Priced() bool {
	return p.Input > 0 || p.CacheHit > 0 || p.CacheCreation > 0 || p.Output > 0
}

// EffectivePrices 把原始单价解析成生效单价，并给出「是否配价」。
//
// # 兜底规则（与审计修复前**逐字一致**，只是搬到了这里）
//
//   - 缓存命中价未配（<=0）→ 回退**输入价**。只填输入价的模型不会把
//     命中部分算成免费（否则缓存越命中越亏钱，方向完全错）。
//   - 输入价未配（<=0）→ 回退**输出价**；输出价也未配 → 回退**缓存命中价**。
//     兜底方向是「取已配价里最贵的那一个」，偏保守（宁可高估预检、
//     不可低估实扣）。审计前的预检只回退到「另一个」且**不含缓存价**，
//     于是只配 price_cache_hit 的模型在预检侧被当成「完全未配价」。
//   - 输出价未配（<=0）→ 回退生效输入价，理由同上。
//
// 最后一条 fallback 链里「全部未配」时返回全 0 且 Priced()=false ——
// 这就是「未配价 = 0 元」的既有口径。
//
// # 为什么输入价的兜底必须包含缓存命中价
//
// 这是本次修复的核心。只配 price_cache_hit=3 的模型：
//   - 修复前预检：inPrice=0 && outPrice=0 → 提前 return 0 → fail-open；
//   - 修复前实扣：未命中部分免费、命中部分按 3 元/百万收费。
//
// 修复后预检与实扣都按同一份 EffectivePrices 走，预检估出的值 ≥ 实扣值。
func (p Prices) EffectivePrices() (EffectivePrices, bool) {
	if !p.Priced() {
		return EffectivePrices{}, false
	}
	eff := EffectivePrices{Input: p.Input, CacheHit: p.CacheHit, CacheCreation: p.CacheCreation, Output: p.Output}

	// 输入价缺失 → 回退「已配价里最贵的一个」（输出价 / 缓存命中价）。
	if eff.Input <= 0 {
		eff.Input = eff.Output
		if eff.Input <= 0 {
			eff.Input = eff.CacheHit
		}
		if eff.Input <= 0 {
			eff.Input = eff.CacheCreation
		}
	}
	// 缓存命中价 / 缓存写入价缺失 → 回退生效输入价（不免费）。
	if eff.CacheHit <= 0 {
		eff.CacheHit = eff.Input
	}
	if eff.CacheCreation <= 0 {
		eff.CacheCreation = eff.Input
	}
	// 输出价缺失 → 回退生效输入价。
	if eff.Output <= 0 {
		eff.Output = eff.Input
	}
	// 输入价仍为 0 只可能发生在「只配了 <=0 的价」这种配置上（正常配置下
	// Priced() 已经把它拦掉），此时保持 0 —— 与「未配价 = 0」同语义。
	return eff, true
}

// PriceUsage 是**全仓唯一的单价公式**：把一次调用的 token 数换算成费用（元）。
//
// # 为什么必须只有这一份
//
// 这套公式有两个消费者，语义要求它们**永远相等**：
//   - usage_records.cost_total（freezeUsageCost 落库时固化，报表全部读它）；
//   - 余额扣费（balance_dao 收到 usage 落库时返回的同一个 cost 值，
//     不重新计算 —— 见 CreateUsageRecordWithCost）；
//   - 余额预检（cmd/gateway/billing.go 的 estimateBalanceCost，经
//     EstimateUpperBoundCost 走同一份单价）。
//
// 两处各写一份公式时，漂移几乎必然发生，且**没有任何人会发现**：报表显示
// 「花了 3.00 元」、余额实际扣了 2.97 元，两个数字各看都合理，只有逐条对账
// 才看得出来 —— 而没人会对账。所以本函数是纯函数（无 IO、无状态），调用方
// 只能取它的返回值、不得复制表达式。
//
// # 口径（单价「元/百万 tokens」）：
//
//	缓存未命中输入 = MAX(input - cached - cached_creation, 0) × price_input
//	缓存命中输入   = cached × price_cache_hit（<=0 时回退 price_input，
//	                 只填输入价的模型不会把命中部分算成免费）
//	缓存写入输入   = cached_creation × price_cache_creation（<=0 时回退
//	                 price_input，即未配置独立写入价时按输入价收费一次）
//	输出           = output × price_output
//	合计           = 上述四项之和 / 1e6
//
// 四个价格来自上游单价表，NULL/未配置一律按 0（调用方用 COALESCE 兜底），
// 未配置价格的模型因此贡献 0 —— 与「未配价 = 0」的既有口径同语义。
//
// 负 token 不在这里兜：CreateUsageRecord 已在入口把负值归一为 0
// （见其注释），而 PriceUsage 的调用方只有 freezeUsageCost 一处。
func PriceUsage(priceInput, priceCacheHit, priceCacheCreation, priceOutput, inputTokens, cachedTokens, cachedCreationTokens, outputTokens float64) float64 {
	uncached := inputTokens - cachedTokens - cachedCreationTokens
	if uncached < 0 {
		uncached = 0
	}
	hit := priceCacheHit
	if hit <= 0 {
		hit = priceInput
	}
	creation := priceCacheCreation
	if creation <= 0 {
		creation = priceInput
	}
	total := (uncached*priceInput + cachedTokens*hit + cachedCreationTokens*creation + outputTokens*priceOutput) / PriceUnitDivisor
	// 舍入到 1e-9 元。float64 的二进制表示无法精确表达十进制小数，
	// 于是「×3 个单价再除 1e6」会带出长尾（如 0.0030000000000000005），
	// 这些值原样进 JSON 响应，前端展示与对账都会看到脏尾巴。
	//
	// 精度取 1e-9 而非更高：1 纳元的 1% 仍远小于任何真实计费的最小粒度，
	// 而更长的尾数只会把噪声带得更远。
	if total != 0 {
		total = math.Round(total*1e9) / 1e9
	}
	return total
}

// PriceUsageWithPrices is the Prices version used by the actual-cost path.
// It resolves fallback prices through EffectivePrices so precheck and stored
// usage cannot disagree about which token buckets are billable.
func PriceUsageWithPrices(p Prices, inputTokens, cachedTokens, cachedCreationTokens, outputTokens float64) float64 {
	eff, priced := p.EffectivePrices()
	if !priced {
		// An explicitly unpriced model stays free. The difference between
		// "no price configured" and "configured to zero for this bucket" is
		// decided once, in EffectivePrices.
		return 0
	}
	return PriceUsage(eff.Input, eff.CacheHit, eff.CacheCreation, eff.Output,
		inputTokens, cachedTokens, cachedCreationTokens, outputTokens)
}

// EstimateUpperBoundCost 估算「一次调用最多可能花多少钱」（元）。
//
// # 这是**预检**专用的上界估算，不是实扣
//
//   - 输入侧按调用方给的 token 估算（事前可知）；
//   - 输出侧按 max_tokens **全额计上界** —— 输出长度事前不可知，流式更是
//     边生成边计费，取上界是唯一诚实的做法：宁可高估把请求挡在门外
//     （用户充值后重试），也不要低估放行后才发现钱不够。
//   - 缓存命中在预检里**无法预知**（要看上游命中情况），因此按「全部未命中」
//     的最高价计。这同样是有意的保守：缓存命中单价通常低于输入价，按未命中
//     算是上界。真实扣费用的是落库时那个含缓存命中的 cost_total，两者差额
//     就是「预检保守、扣费精确」的体现，不是偏差。
//
// # 与实扣的关系
//
// 单价来自同一份 Prices（经 EffectivePrices 解析），所以：
//   - 预检估出的值 **≥** 实扣值（保守方向正确）；
//   - 「哪些 token 收费」的口径**逐字一致**（同一份兜底规则）。
//
// 审计修复前这两条都不成立：预检的兜底规则是另一套，且漏掉了「只配
// price_cache_hit」这一整类配置，导致预检 fail-open。
func EstimateUpperBoundCost(p Prices, inputTokens, maxOutputTokens int64) float64 {
	eff, priced := p.EffectivePrices()
	if !priced {
		// 未配价模型：cost_total 恒 0，扣费也是 0。预检放行（Lead 口径：
		// 未配价不拒绝，admin 与普通用户一样），返回 0 让判定自然放行。
		return 0
	}
	if inputTokens < 0 {
		inputTokens = 0
	}
	if maxOutputTokens < 0 {
		maxOutputTokens = 0
	}
	// 预检按「全部未命中」计 —— 但**输入侧的价要取输入价与缓存命中价的较大者**。
	//
	// 原因：预检事前不知道有多少 token 会命中缓存，而实扣对命中部分按
	// price_cache_hit 收、对未命中部分按 price_input 收。所以「全部命中」
	// 是实扣侧的上界情形：每 token 的最高可能单价 = max(price_input,
	// price_cache_hit, price_cache_creation)。只按 price_input 估会在
	// 「缓存命中价 / 缓存写入价 > 输入价」这种（少见但管理面允许）配置下
	// **低估**，方向是漏放行 —— 正是本次要消除的那一类错误。
	//
	// 常规配置（缓存命中价 < 输入价）下 max 就是 price_input，与审计前的
	// 「按未命中计」完全一致，不改变行为。
	inputPrice := eff.Input
	if eff.CacheHit > inputPrice {
		inputPrice = eff.CacheHit
	}
	if eff.CacheCreation > inputPrice {
		inputPrice = eff.CacheCreation
	}
	total := (float64(inputTokens)*inputPrice + float64(maxOutputTokens)*eff.Output) / PriceUnitDivisor
	return total
}
