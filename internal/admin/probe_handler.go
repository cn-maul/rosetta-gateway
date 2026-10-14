package admin

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/cn-maul/rosetta-gateway/internal/store"
)

// ProbeHandler 暴露探测结果给管理面（总览健康卡 + 连通率时间轴页）。
//
// 探测循环在 cmd/gateway 的后台 goroutine 里跑（internal/probe.Runner），
// 本 handler 只负责读 model_probes 表，不触发任何探测。
type ProbeHandler struct {
	store *store.Store
}

func NewProbeHandler(st *store.Store) *ProbeHandler {
	return &ProbeHandler{store: st}
}

// probeWindowMs 是健康判定的默认窗口（与配置的 window_size × interval 相当，
// 这里用固定 1 小时：窗口内 10 次探测（10 分钟间隔）足够算成功率）。
const probeWindowMs = int64(3600 * 1000)

// modelHealthResponse 是总览健康卡的一行。
type modelHealthResponse struct {
	ModelID    string `json:"model_id"`
	ProviderID string `json:"provider_id"`
	// Health 是健康态：unknown / healthy / degraded / down。
	Health string `json:"health"`
	// SuccessRate 是窗口内成功率（0~1）；从未探测为 0。
	SuccessRate float64 `json:"success_rate"`
	// WindowCount / WindowOK 是窗口内探测次数与成功次数。
	WindowCount int64 `json:"window_count"`
	WindowOK    int64 `json:"window_ok"`
	// ConsecutiveFail 连续失败次数。
	ConsecutiveFail int64 `json:"consecutive_fail"`
	// LastTs 最近一次探测时间（毫秒）；0 = 从未探测。
	LastTs int64 `json:"last_ts"`
	// LastError 最近一次失败原因（最近成功则为空）。
	LastError string `json:"last_error,omitempty"`
}

// Models 返回全部模型的健康摘要（总览页健康卡的数据源）。
//
// 判定参数从哪来：探测循环用的 down_threshold / healthy_rate 在 config 里，
// 而本 handler 只持 store。为保持单一口径，这里用与配置默认一致的常量
// （down=3、healthy=0.8）—— 若将来配置可调，这里要改成从快照读。
// 目前配置是启动时固定的，用常量不会与循环分叉。
func (h *ProbeHandler) Models(w http.ResponseWriter, r *http.Request) {
	healths, err := h.store.ProbeHealthSummary(r.Context(), probeWindowMs)
	if err != nil {
		writeServerError(w, "probe health summary", err)
		return
	}
	result := make([]modelHealthResponse, 0, len(healths))
	for _, hh := range healths {
		resp := modelHealthResponse{
			ModelID:         hh.ModelID,
			ProviderID:      hh.ProviderID,
			WindowCount:     hh.WindowCount,
			WindowOK:        hh.WindowOK,
			ConsecutiveFail: hh.ConsecutiveFail,
			LastTs:          hh.LastTs,
		}
		if hh.WindowCount > 0 {
			resp.SuccessRate = float64(hh.WindowOK) / float64(hh.WindowCount)
		}
		resp.Health = string(classifyProbeHealth(hh))
		if !hh.LastOK && hh.LastErrorKind != "" {
			resp.LastError = hh.LastErrorKind
		}
		result = append(result, resp)
	}
	writeJSON(w, http.StatusOK, result)
}

// classifyProbeHealth 是 admin 层对健康态的判定（与 probe.Classify 同口径）。
//
// 为什么这里再写一份而不是 import probe 包：probe 包依赖 config（Runner 构造），
// 而 admin 的 ProbeHandler 只持 store，import 会引入不必要的依赖链。
// 判定逻辑本身是纯函数，复制一份并加注释指向 probe.Classify，避免漂移。
func classifyProbeHealth(h store.ProbeHealth) string {
	if h.WindowCount == 0 {
		return "unknown"
	}
	if h.ConsecutiveFail >= 3 {
		return "down"
	}
	if h.LastOK {
		if float64(h.WindowOK)/float64(h.WindowCount) >= 0.8 {
			return "healthy"
		}
		return "degraded"
	}
	return "degraded"
}

// Timeline 返回若干模型在时间轴上的连通率分桶。
//
// 参数：model_id（必填，逗号分隔的**多个**）、days（默认 7）、
// granularity（hour|day，默认 hour）。一次请求覆盖用户勾选的若干模型
// —— 前端是一排卡片各画各的曲线，循环发 N 个请求既慢又难做加载态。
func (h *ProbeHandler) Timeline(w http.ResponseWriter, r *http.Request) {
	raw := r.URL.Query().Get("model_id")
	// 去重 + 去空：前端可能重复勾选同一模型，重复 id 会让同一桶被算两次。
	seen := make(map[string]bool)
	modelIDs := make([]string, 0, 4)
	for _, id := range strings.Split(raw, ",") {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		modelIDs = append(modelIDs, id)
	}
	if len(modelIDs) == 0 {
		writeError(w, http.StatusBadRequest, "model_id is required")
		return
	}
	// 上限：一次画几百条曲线既没人看得懂，也让查询变重。
	if len(modelIDs) > 24 {
		writeError(w, http.StatusBadRequest, "一次最多查看 24 个模型")
		return
	}
	days := 7
	if v := r.URL.Query().Get("days"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 90 {
			writeError(w, http.StatusBadRequest, "days must be 1..90")
			return
		}
		days = n
	}
	bucketMs := int64(3600 * 1000) // 默认按小时
	if g := r.URL.Query().Get("granularity"); g == "day" {
		bucketMs = int64(24 * 3600 * 1000)
	}

	now := time.Now().UnixMilli()
	from := now - int64(days)*24*3600*1000
	buckets, err := h.store.ProbeTimeline(r.Context(), modelIDs, from, now, bucketMs)
	if err != nil {
		writeServerError(w, "probe timeline", err)
		return
	}
	writeJSON(w, http.StatusOK, buckets)
}
