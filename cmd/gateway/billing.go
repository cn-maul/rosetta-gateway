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

import (
	"encoding/json"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/cn-maul/rosetta-gateway/internal/auth"
	"github.com/cn-maul/rosetta-gateway/internal/outwire"

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
	case "localhost", "localhost.localdomain", "ip6-localhost", "ip6-loopback":
		return true
	}
	return false
}
