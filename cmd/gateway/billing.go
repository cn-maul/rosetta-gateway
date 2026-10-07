package main

// 额度/费用查询接口（2026-10-04）。
//
// 两套形状，服务两类调用方：
//
//  1. OpenAI 官方 Usage/Costs API（2024-12 起）：
//     GET /v1/organization/costs、GET /v1/organization/usage/completions。
//     响应是 `object:"page"` + `bucket`（start_time/end_time/results）结构，
//     costs 的 result 带 `line_item: "model:<id>"` 与 `amount:{value,currency}`。
//     网关映射：**按调用者身份收窄**（管理员/运维凭据看全量，普通用户只看
//     自己，见 orgCostsScope），费用按 upstream_models
//     的单价实时估算（口径同 GetUsageStats），currency 诚实标 "cny"（单价配置
//     的量纲是 元/百万 tokens，不做汇率换算）。
//
//  2. 生态事实标准（one-api/new-api 时代起客户端通用）：
//     GET /dashboard/billing/subscription 与 /dashboard/billing/usage
//     （/v1/... 前缀同样受理）。ChatGPT-Next-Web / LobeChat 等工具用它显示
//     「余额/已用」。网关映射：把 **百万 tokens 记作 1 美元等价单位** ——
//     hard_limit_usd = quota_tokens/1e6、total_usage（美分）= used/1e4。
//     这样做的原因：网关的配额量纲是 token（DESIGN §11.2），客户端 UI 只消费
//     两个数的**比值**（进度条/余额），同量纲保证比例正确；绝对值是 token 量纲
//     而非美元，文档与响应注释均已说明。key 未设配额（不限）时 hard_limit_usd
//     返回 1e9，避免 UI 把 0 显示成「零余额」。
//     usage 端点是 **per-key** 口径（谁查谁自己），与 subscription 的配额主体一致。
//
// —— 上面两套是**查询**接口。以下是数据面的**余额**逻辑（预检 + 扣费），
// 它决定「这次调用收不收费、收多少」，口径见各函数的注释。

import (
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/cn-maul/rosetta"
	"github.com/cn-maul/rosetta-gateway/internal/auth"
	"github.com/cn-maul/rosetta-gateway/internal/outwire"
	"github.com/cn-maul/rosetta-gateway/internal/server"

	"github.com/cn-maul/rosetta-gateway/internal/routing"
	"github.com/cn-maul/rosetta-gateway/internal/snapshot"
	"github.com/cn-maul/rosetta-gateway/internal/store"
)

// ptmPerUSD 是「1 美元等价单位 = 多少 token」。见文件头注释的单位映射说明。
const ptmPerUSD = 1_000_000

// unlimitedHardLimitUSD 是未设配额（不限）key 对外显示的额度上限。
const unlimitedHardLimitUSD = 1_000_000_000

// accessUntilFarFuture 是 subscription 的 access_until（2100-01-01，Unix 秒）。
const accessUntilFarFuture = 4102444800

func billingSubscription(db *store.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		authCtx, err := auth.Authenticate(r)
		if err != nil {
			writeAuthError(w, err, outwire.WriteOpenAIError)
			return
		}
		quota, used, ok, qerr := db.GetKeyQuota(r.Context(), authCtx.KeyID)
		if qerr != nil || !ok {
			// key 在快照里存在但库里已查不到（刚删）：按未配置额度处理。
			quota, used = 0, 0
		}

		hard := float64(quota) / ptmPerUSD
		if quota <= 0 {
			hard = unlimitedHardLimitUSD
		}
		usedUSD := float64(used) / ptmPerUSD

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"object":                "billing_subscription",
			"has_payment_method":    true,
			"soft_limit_usd":        hard,
			"hard_limit_usd":        hard,
			"system_hard_limit_usd": hard,
			"access_until":          accessUntilFarFuture,
			"plan": map[string]any{
				"title": "rosetta-gateway (1 unit = 1M tokens)",
				"id":    "gateway-token-quota",
			},
			"account_name": authCtx.Name,
			// one-api 系客户端常读 total_available/total_used 两个扩展字段：
			// 给出与 hard_limit 同量纲的余额，省得各工具自己减。
			"total_available": hard - usedUSD,
			"total_used":      usedUSD,
		})
	}
}

func billingUsage(db *store.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		authCtx, err := auth.Authenticate(r)
		if err != nil {
			writeAuthError(w, err, outwire.WriteOpenAIError)
			return
		}

		// OpenAI legacy 形参是 YYYY-MM-DD（UTC 日期）；end_date 按含端日处理
		//（+1 天），与 one-api 系代理的行为一致，避免「查到月底少一天」。
		startDate := r.URL.Query().Get("start_date")
		endDate := r.URL.Query().Get("end_date")
		from, to, bad := billingRangeMs(startDate, endDate)
		if bad {
			outwire.WriteOpenAIError(w, http.StatusBadRequest, "invalid_request_error",
				"start_date/end_date must be YYYY-MM-DD and start_date must be before end_date")
			return
		}

		var totalTokens int64
		var days []store.DayTokens
		if to > from {
			// 错误必须上抛。丢弃它等于把 DB 故障报成「你什么都没用过」：
			// ChatGPT-Next-Web / LobeChat 用这个数画余额进度条，一次
			// 瞬时故障就会让付费用户看到「额度没用过」。同文件里的
			// orgCosts / orgUsageCompletions 都是判错返回 500。
			var qerr error
			totalTokens, qerr = db.SumTokensForKey(r.Context(), authCtx.KeyID, from, to)
			if qerr != nil {
				outwire.WriteOpenAIError(w, http.StatusInternalServerError, "internal_error", "usage query failed")
				return
			}
			days, qerr = db.SumTokensByDayForKey(r.Context(), authCtx.KeyID, from, to)
			if qerr != nil {
				outwire.WriteOpenAIError(w, http.StatusInternalServerError, "internal_error", "usage query failed")
				return
			}
		}

		lineItems := make([]map[string]any, 0, len(days))
		daily := make([]map[string]any, 0, len(days))
		for _, d := range days {
			cents := float64(d.Total) / ptmPerUSD * 100
			lineItems = append(lineItems, map[string]any{"name": "gateway", "cost": cents})
			daily = append(daily, map[string]any{"date": d.Day, "line_items": []map[string]any{{"name": "gateway", "cost": cents}}})
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"object":      "list",
			"total_usage": float64(totalTokens) / ptmPerUSD * 100, // 美分（PTM 等价口径）
			"daily_costs": daily,
			// 扩展字段：token 量纲的原值，工具想按 token 展示时用。
			"total_tokens": totalTokens,
			"line_items":   lineItems,
		})
	}
}

// orgCostsScope 决定 org-wide 查询能看到谁的数据。
//
// 这两个端点要兼容 OpenAI 官方「组织视角」的形状，但组织 = 本网关的
// **整个部署**。多用户部署下，运维凭据持钥人（ID 为空的合成身份）看全量，
// 普通用户只看自己 —— 与 admin 面 callerScope 的口径一致。
//
// 修复前这里只做 auth.Authenticate，于是任何一把 sk-gw key 都能读到
// **别的租户**的模型名、用量与费用。实测：普通用户自己的
// /admin/api/stats 显示 total_tokens:12、cost:0（费用对非管理员归零），
// 而同一把 key 打 /v1/organization/usage/completions 拿到的是管理员租户的
// input_tokens:20、num_model_requests:2；/costs 直接吐出 value:0.000072 cny
// —— 恰好是 stats 刻意不给的那笔钱。
//
// 返回空串 = 不限制（仅限管理员）。
func orgCostsScope(authCtx *auth.Context) string {
	u := snapshot.Get().UsersByID[authCtx.UserID]
	if u != nil && u.Role == store.RoleAdmin {
		return ""
	}
	return authCtx.UserID
}

func orgCosts(db *store.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		authCtx, err := auth.Authenticate(r)
		if err != nil {
			writeAuthError(w, err, outwire.WriteOpenAIError)
			return
		}

		bucketSec, from, to, errResp := orgQueryWindow(r)
		if errResp {
			outwire.WriteOpenAIError(w, http.StatusBadRequest, "invalid_request_error",
				"start_time/end_time must be unix seconds; bucket_width must be 1h or 1d")
			return
		}

		rows, qerr := db.SumCostBucketsScoped(r.Context(), orgCostsScope(authCtx), from, to, bucketSec)
		if qerr != nil {
			outwire.WriteOpenAIError(w, http.StatusInternalServerError, "internal_error", "costs query failed")
			return
		}
		buckets := make([]map[string]any, 0, len(rows))
		for _, b := range rows {
			buckets = append(buckets, map[string]any{
				"object":     "bucket",
				"start_time": b.BucketStart,
				"end_time":   b.BucketStart + bucketSec,
				"results": []map[string]any{{
					"object":    "organization.costs.result",
					"line_item": "model:" + b.Model,
					"amount":    map[string]any{"value": b.Cost, "currency": "cny"},
					"cost_type": "inference",
				}},
			})
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"object":    "page",
			"data":      buckets,
			"has_more":  false,
			"next_page": nil,
		})
	}
}

func orgUsageCompletions(db *store.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		authCtx, err := auth.Authenticate(r)
		if err != nil {
			writeAuthError(w, err, outwire.WriteOpenAIError)
			return
		}

		bucketSec, from, to, errResp := orgQueryWindow(r)
		if errResp {
			outwire.WriteOpenAIError(w, http.StatusBadRequest, "invalid_request_error",
				"start_time/end_time must be unix seconds; bucket_width must be 1h or 1d")
			return
		}

		rows, qerr := db.SumUsageBucketsScoped(r.Context(), orgCostsScope(authCtx), from, to, bucketSec)
		if qerr != nil {
			outwire.WriteOpenAIError(w, http.StatusInternalServerError, "internal_error", "usage query failed")
			return
		}
		buckets := make([]map[string]any, 0, len(rows))
		for _, b := range rows {
			buckets = append(buckets, map[string]any{
				"object":     "bucket",
				"start_time": b.BucketStart,
				"end_time":   b.BucketStart + bucketSec,
				"results": []map[string]any{{
					"object":             "organization.usage.completions.result",
					"input_tokens":       b.InputTokens,
					"input_cached":       b.CachedTokens,
					"output_tokens":      b.OutputTokens,
					"total_tokens":       b.InputTokens + b.OutputTokens,
					"num_model_requests": b.Requests,
				}},
			})
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"object":    "page",
			"data":      buckets,
			"has_more":  false,
			"next_page": nil,
		})
	}
}

// orgQueryWindow 解析 usage/costs 的公共时间参数（start_time/end_time 为
// Unix 秒），缺省窗口为最近 30 天。内部统一换算成毫秒；**默认上界用
// UnixMilli** —— 若按秒截断，同一秒内插入的记录会被上界切掉（实测踩过：
// costs 恒为空页）。显式传入的 end_time 保持秒语义（*1000）。
func orgQueryWindow(r *http.Request) (bucketSec, from, to int64, bad bool) {
	bucketSec = 86400
	switch r.URL.Query().Get("bucket_width") {
	case "1h":
		bucketSec = 3600
	case "1d", "":
	default:
		return 0, 0, 0, true
	}
	to = time.Now().UnixMilli()
	from = to - 30*86400_000
	if v := r.URL.Query().Get("start_time"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			from = n * 1000
		}
	}
	if v := r.URL.Query().Get("end_time"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			to = n * 1000
		}
	}
	if from >= to {
		return 0, 0, 0, true
	}
	return bucketSec, from, to, false
}

// billingRangeMs 把 YYYY-MM-DD 日期对换成毫秒区间；end_date 含端日（+1 天）。
//
// 参数缺失时回退最近 30 天，但**给了却解析不了**必须报 400 ——
// 静默回退意味着调用方拿到的是另一个时间窗的数据却毫不知情
// （实测 `?start_date=garbage` 与反向区间都返回了与不带参数完全相同的
// 数值，看起来「生效了」）。反向区间同理：那是明显的调用方 bug。
func billingRangeMs(start, end string) (from, to int64, bad bool) {
	now := time.Now()
	to = now.UnixMilli()
	from = to - 30*86400_000
	if start != "" {
		t, err := time.ParseInLocation("2006-01-02", start, time.UTC)
		if err != nil {
			return 0, 0, true
		}
		from = t.UnixMilli()
	}
	if end != "" {
		t, err := time.ParseInLocation("2006-01-02", end, time.UTC)
		if err != nil {
			return 0, 0, true
		}
		to = t.AddDate(0, 0, 1).UnixMilli() // 含端日
	}
	if from >= to {
		return 0, 0, true
	}
	return from, to, false
}

// isLoopbackListen 报告 listen 地址是否只绑定回环。
//
// 用途：判断「首次初始化窗口 + 绑非回环」这个组合是否构成
// **任何能连到端口的人都能抢先成为第一个管理员**的暴露面。
// 解析失败时**保守返回 false** —— 宁可多报一次警，不可漏掉真实暴露。
func isLoopbackListen(listen string) bool {
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		// 可能是 ":8666"（空 host = 所有接口）或压根不是 host:port。
		if strings.HasPrefix(listen, ":") {
			return false
		}
		return false
	}
	host = strings.TrimSpace(host)
	// 空 host 意味着绑定全部接口（Go 的 net.Listen 语义）。
	if host == "" {
		return false
	}
	// 已经是 IP 字面量。
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	// 主机名：localhost 及常见别名视为回环。
	switch strings.ToLower(host) {
	case "localhost", "localhost.localname", "ip6-localhost", "ip6-loopback":
		return true
	}
	return false
}

// =====================================================================
// 数据面余额：预检（准入）与扣费（记账）
// =====================================================================
//
// # 两套口径必须分清：cost_total 是「上游成本」，余额扣费是「用户应付」
//
// 这一节最容易被后来者搞混的地方，故先说清：
//
//	usage_records.cost_total —— **无论请求成败都会记**，只要触发了上游调用。
//	  它的含义是「这次调用让网关付给上游多少钱」，是网关的成本/审计口径，
//	  失败、超时、断流同样产生成本，所以必须记。
//
//	balance_cents 的扣减 —— **只对成功请求**（Lead 与用户确认的口径）。
//	  它的含义是「用户要为这次成功拿到的东西付多少钱」，是应收口径。
//	  失败请求没给用户产出任何东西，不该收钱。
//
// 两者数字相同（同一次调用的 cost_total 既是成本也是应收），但**触发条件
// 不同**，所以不是同一笔账。若把 cost_total 的「无论成败都记」直接搬过来
// 当扣费触发条件，用户会为上游 502 付费 —— 这与用户确认的口径相反。
//
// # 预检与扣费为什么是两个不同的数
//
//	预检（请求前）：按**模型单价 + 估算 token 数**估算本次量级，只回答
//	  「余额够不够做这一次」。它是准入闸门，**不负责确定扣多少**。
//	扣费（成功后）：金额取该条 usage 落库时固化的 cost_total。
//
// 为什么不干脆在预检时就按估算值扣掉：估不准。输出长度事前不可知（流式更是
// 边生成边计费），预扣多了要退款（余额逻辑没有「预占/退款」这一对操作，
// 与 token 配额的 ReserveQuota/ReleaseQuota 机制不同），预扣少了则拦不住
// 超支。**先按上界准入、后按实收**把「估不准」这件事消掉，且不需要找零。
//
// # 管理员完全跳过（Lead 与用户确认）
//
// 管理员的余额**不参与任何判定**。刻意不做「给管理员算一个无限余额」：
// 那样会把管理员塞进「余额够不够」这条判定路径，等于让他依赖一次数据库读 ——
// 而他的调用恰恰最不该被余额查询的抖动影响。直接 early-return，热路径上
// 管理员的余额开销是**零**（不读库、不估算）。
// 与 orgCostsScope 同源：那里也是判 admin 后直接返回空作用域。

// balanceExempt 报告该用户是否**不受余额约束**（管理员）。
//
// 判据用快照里的 Role（与 orgCostsScope 同一口径、同一处字面量），
// 零查库 —— 这条判定在每个普通用户请求上都要跑，管理员更不能因此多一次读。
func balanceExempt(userID string) bool {
	u := snapshot.Get().UsersByID[userID]
	return u != nil && u.Role == store.RoleAdmin
}

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
// 计价公式与 store.priceUsage **同源**（同样的三项、同样的 1e6 换算）。
// 这里不复用 store 的函数是因为 priceUsage 是包私有的，而热路径在
// cmd/gateway 层。两处若漂移，后果比不预检更糟：用户会在「明明够钱」时被拒。
// 故改任一侧口径都必须同步另一侧。
func estimateBalanceCost(req *rosetta.ChatRequest, m *routing.UpstreamModel) int64 {
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
	inPrice := m.PriceInput
	outPrice := m.PriceOutput
	if inPrice <= 0 && outPrice <= 0 {
		// 未配价模型：cost_total 恒 0，扣费也是 0。预检放行（Lead 口径：
		// 未配价不拒绝，admin 与普通用户一样），返回 0 让判定自然放行。
		return 0
	}
	// 守卫：单价可能只配了输入价或只配了输出价。缺哪一项就用另一项兜底，
	// 避免「只配了输出价」的模型因 inPrice=0 让输入侧完全免费、严重低估
	// 本次开销。兜底方向是「取已配价里另一个」，偏保守。
	if inPrice <= 0 {
		inPrice = outPrice
	}
	if outPrice <= 0 {
		outPrice = inPrice
	}
	// 输出侧按 max_tokens 全额计上界；客户端没给（<=0）时回落模型配置的最大
	// 输出 —— 都拿不到就只按输入算（下界，宁可不高估）。
	maxOut := int64(req.MaxOutputTokens)
	if maxOut <= 0 {
		maxOut = int64(m.MaxOutputTokens)
	}
	cost := (float64(inTokens)*inPrice + float64(maxOut)*outPrice) / 1_000_000.0
	return yuanToCents(cost)
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
func estimateBalanceCostChain(req *rosetta.ChatRequest, cands []routing.Candidate) int64 {
	var max int64
	for _, c := range cands {
		if c.UpstreamModel == nil {
			continue
		}
		if e := estimateBalanceCost(req, c.UpstreamModel); e > max {
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
func yuanToCents(y float64) int64 { return store.YuanToCents(y) }

// centsToYuan 是 yuanToCents 的反向（展示/日志用）。
func centsToYuan(c int64) float64 { return float64(c) / 100 }

// balanceVerdict 是预检的结论，供 handleIngress 决定放行还是拒绝。
type balanceVerdict int

const (
	balanceAllow balanceVerdict = iota
	// balanceReject 表示余额不足，应回 402。
	balanceReject
)

// balancePreflight 在触碰上游**之前**判断「这次调用该不该被余额拦住」。
//
// 纯函数：不碰 DB、不碰全局。判定逻辑单独抽出来是为了能直接单测边界
// （差一分钱、刚好等于、无限额、管理员），而不必为每个边界都搭一套
// 上游 harness —— 边界算错钱，而这类错误在端到端测试里极难发现。
func balancePreflight(estCents, balanceCents int64, unlimited, exempt bool, priced bool) balanceVerdict {
	if exempt || unlimited {
		return balanceAllow
	}
	if estCents <= 0 {
		// 估出 0。两种含义，调用方用 priced 区分：
		//
		//   - priced=false：整条链都没配价，本次真的不计费 → 放行。
		//   - priced=true ：模型配了价，只是这次请求太小，四舍五入成 0 分。
		//     放行等于「欠费用户靠发足够小的请求就能一直用」——预检形同虚设。
		//     这种一律按「余额必须 > 0」判定：不够 1 分钱就等于没有余额。
		if !priced {
			return balanceAllow
		}
		if balanceCents > 0 {
			return balanceAllow
		}
		return balanceReject
	}
	if balanceCents >= estCents {
		return balanceAllow
	}
	return balanceReject
}

// chainHasPricedModel 报告这条链上是否存在**配置过价格**的上游模型。
//
// 与「估算值是否大于 0」严格区分：后者会因为「小请求四舍五入成 0」而
// 为 false，前者反映的是「这个模型到底有没有价格」。调用方靠它区分
// 「本次真的不计费」与「计费但恰好算成 0 分」——后者仍须校验余额。
func chainHasPricedModel(cands []routing.Candidate) bool {
	for _, c := range cands {
		if c.UpstreamModel == nil {
			continue
		}
		if c.UpstreamModel.PriceInput > 0 || c.UpstreamModel.PriceOutput > 0 ||
			c.UpstreamModel.PriceCacheHit > 0 {
			return true
		}
	}
	return false
}

// precheckBalance 执行余额预检，返回 false 表示**已经把错误写回客户端**、
// 调用方应当立即返回（绝不触碰上游）。
//
// # fail-closed 还是 fail-open：本函数选 fail-closed（读不到余额就拒绝）
//
// 这与本文件既有的两处查询抖动决策（token 配额预检、用户 token 配额预检都
// fail-open）**刻意相反**，理由如下，请勿"统一"成 fail-open：
//
//   - 配额是 **token**，超发的代价是"某个 key 多用了 N 个 token"。
//     有硬上限（quota），且下一次预检就会挡住继续超发。代价有界、可恢复。
//   - 余额是**钱**，漏放一次 = 一次上游调用白送，而用户**永远不会知道**
//     （没有对账单、没有报错、余额悄悄归零他也不确定是花掉的还是别的原因）。
//     且**没有兜底**：配额漏了，下次 used>quota 会被挡住；余额漏了，钱就没了。
//
// 更关键的是**不对称性**：fail-open 的风险是"偶发的可用性损失"（读库抖动时
// 短暂拒绝用户，可重试即恢复）；fail-closed 的风险是"偶发的资金损失"
// （读库抖动时放行，白送一次调用）。前者随抖动自愈，后者在网关重启前都不会
// 被任何人发现。因此宁可误拒（用户看到明确错误 + 日志，重试即可）不可漏扣。
//
// 代价与缓解：读库失败会误拒正常流量，所以这一路必须**记 ERROR**（fail-open
// 的教训是"记日志才看得见"；这里同理，只是反过来——记录是为了发现抖动）。
//
// # 管理员的短路在最前面
//
// balanceExempt 为真时立刻返回 true，**一次库都不查**。这是口径要求
// （管理员完全不受余额约束），也顺带让管理员的调用不受余额查询抖动影响。
func precheckBalance(w http.ResponseWriter, r *http.Request, db *store.Store, logger *slog.Logger,
	authCtx *auth.Context, ing *ingressRequest, cands []routing.Candidate,
	codec ingressCodec) bool {

	if balanceExempt(authCtx.UserID) {
		return true // 管理员：不读库、不估算、不判定
	}

	// 先估算，再决定要不要读库。顺序有讲究：未配价模型估算恒 0，而未配价
	// 是**放行**的（Lead 口径），那么这类用户根本不该因为「余额读不出来」
	// 而被拒 —— 不读库就不可能读失败。这顺带消掉了「没配价的部署被余额
	// 功能整体拖垮」的可能。
	//
	// 但「估算为 0」有**两种**含义，不能混为一谈：
	//
	//  1. 整条链都没配价 → 本次调用**真的不计费**（cost_total 恒 0），
	//     余额不变。这种才允许直接放行、不读库。
	//  2. 模型配了价，但这次请求小到四舍五入算成 0 分（输入两三个字、
	//     客户端没传 max_tokens、模型又没配 MaxOutputTokens）—— 扣费几乎
	//     为零，但**不能据此放行**：那等于「余额已欠费的用户只要发足够小的
	//     请求就能一直用下去」，预检形同虚设。所以这种情况**照样读余额**，
	//     只是判定时按「余额是否为 0」而不是「够不够付本次」来做。
	//
	// 区分方式：链上是否存在**已配价**的模型（而不是估算值是否大于 0）。
	est := estimateBalanceCostChain(ing.buildRosetta(), cands)
	chainPriced := chainHasPricedModel(cands)
	if est <= 0 && !chainPriced {
		return true // 整条链都没配价：本次真的不计费，不读库
	}

	cents, limited, err := db.BalanceOf(r.Context(), authCtx.UserID)
	if err != nil {
		// fail-closed（理由见本函数注释）。
		logger.Error("balance lookup failed, rejecting (fail-closed)",
			"error", err, "user_id", authCtx.UserID, "key_id", authCtx.KeyID,
			"estimated_cents", est, "request_id", server.RequestIDFromContext(r.Context()))
		codec.WriteError(w, http.StatusInternalServerError, "internal_error",
			"balance check unavailable")
		return false
	}

	switch balancePreflight(est, cents, !limited, false, chainPriced) {
	case balanceReject:
		logger.Warn("insufficient balance",
			"user_id", authCtx.UserID, "key_id", authCtx.KeyID,
			"estimated_cents", est, "balance_cents", cents,
			"request_id", server.RequestIDFromContext(r.Context()))
		codec.WriteError(w, http.StatusPaymentRequired, "insufficient_balance",
			"insufficient balance: this request would cost more than your remaining balance, "+
				"please top up your account")
		return false
	default:
		return true
	}
}
