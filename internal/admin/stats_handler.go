package admin

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/cn-maul/rosetta-gateway/internal/server"
	"github.com/cn-maul/rosetta-gateway/internal/store"
)

type StatsHandler struct {
	store *store.Store
}

func NewStatsHandler(st *store.Store) *StatsHandler {
	return &StatsHandler{store: st}
}

type statsResponse struct {
	TotalRequests   int64   `json:"total_requests"`
	TotalTokens     int64   `json:"total_tokens"`
	InputTokens     int64   `json:"input_tokens"`
	OutputTokens    int64   `json:"output_tokens"`
	CachedTokens    int64   `json:"cached_tokens"`
	CacheHitRate    float64 `json:"cache_hit_rate"`
	ErrorCount      int64   `json:"error_count"`
	AvgTokensPerSec float64 `json:"avg_tokens_per_sec"`
	AvgTtfbMs       float64 `json:"avg_ttfb_ms"`
	// Cost 为费用（元）：每条用量记录落库**当时**按模型单价算好并固化的金额
	// 求和（usage_records.cost_total），未配置价格的模型贡献 0。
	//
	// P1.5 之前这里是按「各模型当前单价」对区间内用量实时估算，于是管理员改个价，
	// 昨天报表里的历史费用跟着变 —— 对账时两个数字对不上，且没人说得清是哪次改价
	// 影响的。固化后历史金额不可变，改价只影响此后的记录（见
	// store.UsageStats.Cost 的注释）。
	//
	// 数据源是 store.UsageSource（明细 ∪ 日归档），所以 30 天明细清理后
	// 「全部」档的费用不会缩水。普通用户拿到的是**自己**的合计（见调用点的
	// 作用域说明）；管理员拿到全局。
	Cost float64 `json:"cost"`

	// Balance 是当前登录者的账户余额（分）与待结算余数（微元），
	// 2026-10-10 新增，供总览页展示。
	//
	// 为什么放在 stats 而不是复用 /me：总览页本来就在拉 stats，多带这两个
	// 字段省掉一次请求；而余额与费用本来就是同一件事的两面（花了多少 /
	// 还剩多少），放在一张卡片里才读得通。
	//
	// 语义与 meResponse 逐字相同，尤其 unlimited 才是「不限额」标志、
	// 0 分是「真没钱」—— 见 store.User.BalanceCents 的对比。
	BalanceCents int64 `json:"balance_cents"`
	// BalanceRemainder 是**不足一分**的待结算余数（微元）：余额按分扣减，
	// 而单价可能低到单次不足一分，不显示它就会出现「费用在涨、余额不动」。
	BalanceRemainder int64 `json:"balance_remainder"`
	BalanceUnlimited bool  `json:"balance_unlimited"`

	// Lifetime 是终身累计（表 B usage_totals），**仅管理员**、且仅当请求
	// 显式带上 include_lifetime=true 时返回。
	//
	// 它的存在意义：剪掉明细之后，「全部」档的起点与合计仍然可算。
	// `first_record_at` 就是为此存的（否则总览「全部」档会凭空丢掉
	// 30 天以前的历史）。此前这张表连同它的触发器、回填与启动 reconcile
	// 全部在维护，却**零非测试读取点** —— 属于「没有执行点的列」。
	// 现在它有了执行点：管理员显式要终身视角时才查，正常区间查询不受影响。
	//
	// 指针 + omitempty：字段不出现 = 没要终身数据，两种形态前端都能区分。
	Lifetime *lifetimeResponse `json:"lifetime,omitempty"`
}

// lifetimeResponse 是终身累计视图（值全为 0 表示无数据）。
type lifetimeResponse struct {
	RequestCount  int64   `json:"request_count"`
	SuccessCount  int64   `json:"success_count"`
	ErrorCount    int64   `json:"error_count"`
	TotalTokens   int64   `json:"total_tokens"`
	CostTotal     float64 `json:"cost_total"`
	FirstRecordAt int64   `json:"first_record_at"` // 毫秒；0 = 从未有过记录
	PrunedThrough string  `json:"pruned_through_day"`
}

func (h *StatsHandler) Get(w http.ResponseWriter, r *http.Request) { // from/to 为毫秒时间戳；两者都不传（from=0）表示统计全部历史（总览「全部」档）。
	//
	// 解析失败必须显式拒绝，不能像旧实现那样把错误丢进 _ 后置 0 ——
	// 那等于让 `?from=abc`、`?from=xyz` 静默退化成「查全表」。而全表聚合
	// 带 LEFT JOIN、无 WHERE 无 LIMIT，是本项目最重的一条查询；
	// usage_records 又没有保留策略，行数无上界。一个手滑的参数就能打出
	// 一记全表扫描。
	from, err := parseOptionalUnixMilli(r.URL.Query().Get("from"), "from")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	to, err := parseOptionalUnixMilli(r.URL.Query().Get("to"), "to")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if from > 0 && to > 0 && from > to {
		writeError(w, http.StatusBadRequest, "from must not be greater than to")
		return
	}

	// 作用域收窄：普通用户只统计自己的数据。
	//
	// 费用**照常返回**（2026-10-10 改）。此前这里对普通用户强制归零，
	// 依据是 MULTIUSER.md §8「网关不做计费结算，展示费用易引发对账争议」——
	// 而那个前提（网关不扣用户的钱）在余额计费落地后已经不成立：
	// 余额是真扣的，且用户能在调用历史里看到每一条 cost_total。
	// 「明细可见、合计为 0」比两边都不可见更费解。
	//
	// 不涉及泄露：scope 非空时 GetUsageStats 只统计该用户自己的行，
	// 拿到的本来就是他自己的消费，与余额扣的是同一批记录。
	//
	// 终身累计（lifetime）仍只对管理员 —— 那是**全局**单行，性质不同。
	scope := callerScope(r)
	stats, err := h.store.GetUsageStats(r.Context(), from, to, scope)
	if err != nil {
		writeServerError(w, "usage stats", err)
		return
	}
	// 缓存命中率已并入 GetUsageStats 同一条 SELECT（见 usage_dao.go）：
	// 此前它是对同一区间 usage_records 的第二次独立全扫，纯属重复。
	// tps/ttfb 是补充指标，查询失败不应让整份 stats 报 500（主统计已成功）；
	// 但旧实现把错误丢进 _ 后指标静默显示 0，运维无从分辨「真的是 0」还是「查挂了」。
	// 补 WARN：值仍回 0，但日志说明原因。
	tps, tpsErr := h.store.GetRecentThroughput(r.Context(), 5, scope)
	if tpsErr != nil {
		slog.Warn("admin stats: recent throughput query failed", "error", tpsErr)
	}
	ttfb, ttfbErr := h.store.GetRecentTtfbMs(r.Context(), 5, scope)
	if ttfbErr != nil {
		slog.Warn("admin stats: recent ttfb query failed", "error", ttfbErr)
	}
	resp := statsResponse{
		TotalRequests:   stats.TotalRequests,
		TotalTokens:     stats.TotalTokens,
		InputTokens:     stats.InputTokens,
		OutputTokens:    stats.OutputTokens,
		CachedTokens:    stats.CachedTokens,
		CacheHitRate:    stats.CacheHitRate,
		ErrorCount:      stats.ErrorCount,
		AvgTokensPerSec: tps,
		AvgTtfbMs:       ttfb,
		Cost:            stats.Cost,
	}

	// 余额随同一份响应下发（省一次请求）。读失败只记 WARN 并保留「不限额」
	// 的默认值 —— 余额是展示性字段，不该让整页统计挂掉；而「不限额」是
	// fail-open 的展示值，用户会看到「不限」而不是一个错误的 0 元。
	//
	// 取值一律来自**当前登录者**，与统计的 scope 是同一个人：普通用户看到
	// 自己的余额与自己的费用，不会看到管理员的数据。
	//
	// 余数必须**回库读**，不能用 context 里那份 user 的字段：那是鉴权时的
	// 身份快照，不带会随扣费变化的字段（user_handler.Me 对余额的处理是
	// 同一个理由，见那里的注释）。用快照会让余数恒为 0 ——
	// 而那正是这个字段存在的全部意义。
	if me := server.UserFromContext(r.Context()); me != nil && me.ID != "" {
		cents, limited, berr := h.store.BalanceOf(r.Context(), me.ID)
		switch {
		case berr != nil:
			slog.Warn("admin stats: balance lookup failed; showing unlimited", "error", berr,
				"user_id", me.ID)
			resp.BalanceUnlimited = true
		default:
			resp.BalanceCents = cents
			resp.BalanceUnlimited = !limited
			if limited {
				// 余数读失败只记 WARN 并按 0 展示：余额已经拿到了，
				// 少一个「待结算」的补充说明不该让整张卡片失效。
				if rem, rerr := h.store.BalanceRemainderOf(r.Context(), me.ID); rerr != nil {
					slog.Warn("admin stats: remainder lookup failed; showing 0",
						"error", rerr, "user_id", me.ID)
				} else {
					resp.BalanceRemainder = rem
				}
			}
		}
	}
	// 终身累计（表 B）**只对管理员**开放，且必须显式要。
	//
	// 表 B 是不分用户的全局单行：普通用户拿到它就等于看到别人的数字
	// （store.GetUsageLifetime 刻意没有 scopeUserID 入参，见其注释）。
	// 查询失败按 tps/ttfb 的同一口径处理：WARN + 不带该字段，不让主统计挂掉。
	if scope == "" && r.URL.Query().Get("include_lifetime") == "true" {
		lt, lerr := h.store.GetUsageLifetime(r.Context())
		if lerr != nil {
			slog.Warn("admin stats: lifetime query failed", "error", lerr)
		} else if lt != nil {
			resp.Lifetime = &lifetimeResponse{
				RequestCount:  lt.RequestCount,
				SuccessCount:  lt.SuccessCount,
				ErrorCount:    lt.ErrorCount,
				TotalTokens:   lt.TotalTokens,
				CostTotal:     lt.CostTotal,
				FirstRecordAt: lt.FirstRecordAt,
				PrunedThrough: lt.PrunedThrough,
			}
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// ReloadHandler 处理手动触发的 POST /admin/api/reload。
// 实际重建逻辑由注入的 reload 函数提供（cmd/gateway 的 runtimeReloader），
// 与管理写操作后的自动 reload（server.AutoReload）共用同一把串行锁。
type ReloadHandler struct {
	reload func(ctx context.Context) error
}

func NewReloadHandler(reload func(ctx context.Context) error) *ReloadHandler {
	return &ReloadHandler{reload: reload}
}

func (h *ReloadHandler) Reload(w http.ResponseWriter, r *http.Request) {
	if err := h.reload(r.Context()); err != nil {
		writeServerError(w, "reload runtime", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// parseOptionalUnixMilli 解析一个可选的毫秒时间戳查询参数。
// 空串 → 0（表示「不限定」）；非数字 → 报错而非静默置 0。
func parseOptionalUnixMilli(v, name string) (int64, error) {
	if v == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s must be a unix millisecond timestamp", name)
	}
	if n < 0 {
		return 0, fmt.Errorf("%s must not be negative", name)
	}
	return n, nil
}
