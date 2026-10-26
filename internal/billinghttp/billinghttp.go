// Package billinghttp exposes read-only billing and usage query endpoints.
// Pricing policy lives in internal/billing; this package only maps store data
// into the external HTTP response shapes.
package billinghttp

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/cn-maul/rosetta-gateway/internal/auth"
	"github.com/cn-maul/rosetta-gateway/internal/authhttp"
	"github.com/cn-maul/rosetta-gateway/internal/outwire"
	"github.com/cn-maul/rosetta-gateway/internal/store"
)

// ptmPerUSD 是「1 美元等价单位 = 多少 token」。见文件头注释的单位映射说明。
const ptmPerUSD = 1_000_000

// unlimitedHardLimitUSD 是未设配额（不限）key 对外显示的额度上限。
const unlimitedHardLimitUSD = 1_000_000_000

// accessUntilFarFuture 是 subscription 的 access_until（2100-01-01，Unix 秒）。
const accessUntilFarFuture = 4102444800

func Subscription(db *store.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		authCtx, err := auth.Authenticate(r)
		if err != nil {
			authhttp.WriteAuthError(w, err, outwire.WriteOpenAIError)
			return
		}
		// 查库失败必须上抛 500，不能与「key 已删除」混成一个分支
		//（2026-10-12 修 P2-5）。
		//
		// 这两种情况语义完全相反，旧实现却都落到 `quota, used = 0, 0`：
		//   - ok=false（key 确实不在库里，刚被删）→ 按未配置额度处理是**对的**；
		//   - qerr != nil（读库抖动 / SQLITE_BUSY / 磁盘故障）→ 那是服务端故障，
		//     此时 quota=0 会让下面的 `quota <= 0` 分支判定成「不限额」，
		//     于是 hard_limit 被报成 unlimitedHardLimitUSD（10 亿）——
		//     一次瞬时故障就让客户端看到一个根本不存在的巨额额度。
		//
		// 与同文件 billingUsage 的口径一致（那里注释已写明：丢弃错误等于把
		// DB 故障报成「你什么都没用过」，而 ChatGPT-Next-Web / LobeChat
		// 正是拿这个数画余额进度条）。判错返回 500，客户端会重试，
		// 比显示一个错的额度诚实得多。
		quota, used, ok, qerr := db.GetKeyQuota(r.Context(), authCtx.KeyID)
		if qerr != nil {
			outwire.WriteOpenAIError(w, http.StatusInternalServerError,
				"internal_error", "quota query failed")
			return
		}
		if !ok {
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

func Usage(db *store.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		authCtx, err := auth.Authenticate(r)
		if err != nil {
			authhttp.WriteAuthError(w, err, outwire.WriteOpenAIError)
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
	// 复用 auth.Context.IsAdminOwner：管理员判定在数据面有两处（鉴权拦截
	// 与这里的 org-wide 可见范围），同一判据必须只有一份实现，否则将来
	// 「谁能调模型」与「谁看得到全量费用」很容易各改各的、悄悄漂移。
	if authCtx.IsAdminOwner() {
		return ""
	}
	return authCtx.UserID
}

func OrgCosts(db *store.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		authCtx, err := auth.Authenticate(r)
		if err != nil {
			authhttp.WriteAuthError(w, err, outwire.WriteOpenAIError)
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

func OrgUsageCompletions(db *store.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		authCtx, err := auth.Authenticate(r)
		if err != nil {
			authhttp.WriteAuthError(w, err, outwire.WriteOpenAIError)
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
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n <= 0 {
			return 0, 0, 0, true
		}
		from = n * 1000
	}
	if v := r.URL.Query().Get("end_time"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n <= 0 {
			return 0, 0, 0, true
		}
		to = n * 1000
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
