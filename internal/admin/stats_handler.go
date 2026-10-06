package admin

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/cn-maul/rosetta-gateway/internal/store"
)

type StatsHandler struct {
	store *store.Store
}

func NewStatsHandler(st *store.Store) *StatsHandler {
	return &StatsHandler{store: st}
}

// costFor 决定是否把费用回给调用方。
//
// scope 非空 = 普通用户的请求（callerScope 的取值）。按
// MULTIUSER.md §8 的既定决策，普通用户不显示金额 —— 网关不做计费结算，
// 展示费用只会引发对账争议，而他们本来也无法核实单价配置。
//
// 保留字段返回 0 而不是省略，是为了让前端不必为两种形态写两套渲染。
func costFor(scope string, cost float64) float64 {
	if scope != "" {
		return 0
	}
	return cost
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
	// 「全部」档的费用不会缩水。普通用户该字段恒为 0（见 costFor）。
	Cost float64 `json:"cost"`

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

	// 作用域收窄：普通用户只统计自己的数据，且**不返回费用**。
	//
	// 为什么费用要归零：MULTIUSER.md §8 的既定决策 —— 网关不做计费结算，
	// 金额展示容易引发对账争议，因此普通用户看不到钱，管理员才看得到。
	// 这里返回 0 而不是省略字段，是因为 Stats 契约里 cost 是非可选数字，
	// 前端无需为两种形态写两套渲染。
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
		Cost:            costFor(scope, stats.Cost),
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
