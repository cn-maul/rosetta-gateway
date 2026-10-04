package main

// 额度/费用查询接口（2026-10-04）。
//
// 两套形状，服务两类调用方：
//
//  1. OpenAI 官方 Usage/Costs API（2024-12 起）：
//     GET /v1/organization/costs、GET /v1/organization/usage/completions。
//     响应是 `object:"page"` + `bucket`（start_time/end_time/results）结构，
//     costs 的 result 带 `line_item: "model:<id>"` 与 `amount:{value,currency}`。
//     网关映射：org-wide 聚合（任一有效 sk-gw key 可查），费用按 upstream_models
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
		from, to := billingRangeMs(startDate, endDate)

		var totalTokens int64
		var days []store.DayTokens
		if to > from {
			totalTokens, _ = db.SumTokensForKey(r.Context(), authCtx.KeyID, from, to)
			days, _ = db.SumTokensByDayForKey(r.Context(), authCtx.KeyID, from, to)
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

func orgCosts(db *store.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, err := auth.Authenticate(r); err != nil {
			writeAuthError(w, err, outwire.WriteOpenAIError)
			return
		}

		bucketSec, from, to, errResp := orgQueryWindow(r)
		if errResp {
			outwire.WriteOpenAIError(w, http.StatusBadRequest, "invalid_request_error",
				"start_time/end_time must be unix seconds; bucket_width must be 1h or 1d")
			return
		}

		rows, qerr := db.SumCostBuckets(r.Context(), from, to, bucketSec)
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
		if _, err := auth.Authenticate(r); err != nil {
			writeAuthError(w, err, outwire.WriteOpenAIError)
			return
		}

		bucketSec, from, to, errResp := orgQueryWindow(r)
		if errResp {
			outwire.WriteOpenAIError(w, http.StatusBadRequest, "invalid_request_error",
				"start_time/end_time must be unix seconds; bucket_width must be 1h or 1d")
			return
		}

		rows, qerr := db.SumUsageBuckets(r.Context(), from, to, bucketSec)
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
// 参数缺失/非法时回退最近 30 天。
func billingRangeMs(start, end string) (from, to int64) {
	now := time.Now()
	to = now.UnixMilli()
	from = to - 30*86400_000
	if t, err := time.ParseInLocation("2006-01-02", start, time.UTC); err == nil {
		from = t.UnixMilli()
	}
	if t, err := time.ParseInLocation("2006-01-02", end, time.UTC); err == nil {
		to = t.AddDate(0, 0, 1).UnixMilli() // 含端日
	}
	if from >= to {
		from, to = now.UnixMilli()-30*86400_000, now.UnixMilli()
	}
	return from, to
}

// isLoopbackListen 报告 listen 地址是否只绑定回环。
//
// 用途只有一个：判断「无管理凭据 + 绑非回环」这个组合是否构成可被
// 任意人抢占管理员的窗口（password/set 在无凭据时被豁免鉴权）。
// 解析失败时**保守返回 false** —— 宁可多报一次警，不可漏掉真实暴露。
func isLoopbackListen(listen string) bool {
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		// 可能是 ":8080"（空 host = 所有接口）或压根不是 host:port。
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
