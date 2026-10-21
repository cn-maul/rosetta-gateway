package admin

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/cn-maul/rosetta-gateway/internal/config"
	"github.com/cn-maul/rosetta-gateway/internal/identity"
	"github.com/cn-maul/rosetta-gateway/internal/probe"
	"github.com/cn-maul/rosetta-gateway/internal/snapshot"
	"github.com/cn-maul/rosetta-gateway/internal/store"
)

// ProbeHandler 暴露探测结果给管理面（总览健康卡 + 连通率时间轴页）。
//
// 探测循环在 cmd/gateway 的后台 goroutine 里跑（internal/probe.Runner），
// 本 handler 只负责读 model_probes 表，不触发任何探测。
type ProbeHandler struct {
	store *store.Store
	cfg   *config.Config
}

// NewProbeHandler 接收运行配置而不是把判定阈值写死。
//
// 探测循环按 probes.down_threshold / probes.healthy_rate 落库并判健康，
// 管理面若继续用 3 / 0.8，配置一改就出现“同一个模型两套健康结论”。
// nil 只用于不关心健康档位的测试构造；生产装配始终传真实 cfg。
func NewProbeHandler(st *store.Store, cfg *config.Config) *ProbeHandler {
	return &ProbeHandler{store: st, cfg: cfg}
}

// probeWindowMs 返回健康判定窗口，口径为 probes.window_size × interval_sec。
//
// 旧实现固定 1 小时，默认 10 分钟间隔下只能装 6 次探测，而配置的
// window_size 是 10 —— 管理面展示与探测器判定的窗口从一开始就不同。
// cfg 为 nil（仅测试构造）时保留旧默认，避免把测试辅助函数全部改签名。
func (h *ProbeHandler) probeWindowMs() int64 {
	if h.cfg == nil {
		return int64(time.Hour / time.Millisecond)
	}
	interval := h.cfg.Probes.IntervalSec
	window := h.cfg.Probes.WindowSize
	if interval <= 0 {
		interval = 600
	}
	if window <= 0 {
		window = 10
	}
	return int64(interval) * int64(window) * int64(time.Second/time.Millisecond)
}

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
// # 按身份收窄（2026-10-14 页面合并）
//
// 现在普通用户也读这个端点（已进 internal/access 白名单），因此必须
// 与 model-names 一样**按组白名单收窄**：他只该看到自己够得着的模型有多通。
//
// 收窄必须**同时作用在摘要与时间轴两个端点**上，否则时间轴会成为绕过
// 摘要的旁路：普通用户只要能对任意 upstream_model_id 查时间轴，就能
// 通过控制响应体大小/有无来枚举「哪些模型 id 存在、哪些探测记录存在」
// —— 模型清单本身就是普通用户可读的信息（model-names），而探测数据
// 额外泄露了**未在清单里的模型**（已停用的路由、别的组的模型）的存在与
// 健康状态。网关/端点拓扑也刻意不给（只有 id 与供应商 slug）。
func (h *ProbeHandler) Models(w http.ResponseWriter, r *http.Request) {
	allowed, ok := h.allowedModelIDs(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "需要登录")
		return
	}
	healths, err := h.store.ProbeHealthSummary(r.Context(), h.probeWindowMs())
	if err != nil {
		writeServerError(w, "probe health summary", err)
		return
	}
	result := make([]modelHealthResponse, 0, len(healths))
	for _, hh := range healths {
		if allowed != nil && !allowed[hh.ModelID] {
			continue
		}
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
		resp.Health = string(h.classifyProbeHealth(hh))
		if !hh.LastOK && hh.LastErrorKind != "" {
			resp.LastError = hh.LastErrorKind
		}
		result = append(result, resp)
	}
	writeJSON(w, http.StatusOK, result)
}

// allowedModelIDs 算出这个调用者允许看到哪些上游模型。
//
// 返回值语义与 map 查找必须区分开，所以用 (nil, true) 表示「不限制」：
//
//   - 管理员 / 无白名单约束的用户 → (nil, true)：全集。
//   - 空白名单交集（一个模型都调不到）→ (空 map, true)：**空集**，不是全集。
//     这正是 model-names 里那条规则「交集为空必须返回空而不是全集」——
//     在这里返回全集等于把权限收紧呈现成权限放开，方向完全相反。
//
// 第二返回值 false 表示未登录，调用方必须自己写 401 并直接返回。
func (h *ProbeHandler) allowedModelIDs(r *http.Request) (map[string]bool, bool) {
	me := identity.UserFromContext(r.Context())
	// 空 ID **不**当 admin（fail-closed），与 model-names 同一口径。
	if me == nil || me.ID == "" {
		return nil, false
	}
	if me.IsAdmin() {
		return nil, true
	}
	snap := snapshot.Get()
	u := snap.UsersByID[me.ID]
	if u == nil || u.AllowedModels.Unrestricted {
		return nil, true
	}
	// 收窄：只放行「公开名在组白名单内」的那条路由的主目标模型。
	//
	// 用 Routes.Resolve 而不是 ListRoutes 全表扫：Resolve 会跳过被禁用的
	// provider/model（见 routing.go 的 synthCandidates），与数据面一致 ——
	// 界面上摆出来一条实际不可用的模型，与不给它摆出来同样误导。
	allowed := make(map[string]bool, len(u.AllowedModels.Models))
	for _, name := range snap.Routes.ListPublicNames() {
		if !u.AllowedModels.Allows(name) {
			continue
		}
		res, err := snap.Routes.Resolve(name)
		if err != nil || len(res.Candidates) == 0 || res.Candidates[0].UpstreamModel == nil {
			continue
		}
		allowed[res.Candidates[0].UpstreamModel.ID] = true
	}
	return allowed, true
}

// classifyProbeHealth 直接复用探测器的判定函数与配置阈值。
//
// admin 与 probe 都是“读配置 + 展示/计算”的上层，admin 依赖 probe
// 不构成循环；相比之下，复制判定逻辑会让两个页面在配置变化后给出
// 不同健康结论，是更难发现的错误。
func (h *ProbeHandler) classifyProbeHealth(ph store.ProbeHealth) probe.Health {
	if h.cfg == nil {
		return probe.Classify(ph, 3, 0.8)
	}
	return probe.Classify(ph, h.cfg.Probes.DownThreshold, h.cfg.Probes.HealthyRate)
}

// Timeline 返回若干模型在时间轴上的连通率分桶。
//
// 参数：model_id（必填，逗号分隔的**多个**）、days（默认 7）、
// granularity（hour|day，默认 hour）。一次请求覆盖用户勾选的若干模型
// —— 前端是一排卡片各画各的曲线，循环发 N 个请求既慢又难做加载态。
func (h *ProbeHandler) Timeline(w http.ResponseWriter, r *http.Request) {
	allowed, ok := h.allowedModelIDs(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "需要登录")
		return
	}
	raw := r.URL.Query().Get("model_id")
	// 去重 + 去空：前端可能重复勾选同一模型，重复 id 会让同一桶被算两次。
	seen := make(map[string]bool)
	modelIDs := make([]string, 0, 4)
	// 请求里被要求但不被允许的 id。用于区分两种「空」：
	// 请求了但一个都不允许 → 403（说清是权限）；本来就没传 → 400。
	denied := 0
	for _, id := range strings.Split(raw, ",") {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			continue
		}
		// 收窄（见 Models 的说明）：普通用户不得借时间轴端点探测
		// 够不着的模型。**直接剔除**而不是整条拒绝 —— 前端一次查一批，
		// 其中混进一个越权 id 就让整张卡片都查不出来，会被误读成
		// 「这些模型都挂了」。剔除后返回的那部分是真实可看的子集。
		if allowed != nil && !allowed[id] {
			denied++
			continue
		}
		seen[id] = true
		modelIDs = append(modelIDs, id)
	}
	if len(modelIDs) == 0 {
		if denied > 0 {
			writeError(w, http.StatusForbidden, "无权查看这些模型的探测数据")
			return
		}
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
	// 二次过滤：正常路径下 modelIDs 已全部在允许集合内，这里是纵深防御
	// —— 若将来 store 层改了过滤语义（比如支持按 provider 展开），
	// 漏出的桶会在这里被拦住，而不是直接进响应体。
	if allowed != nil {
		kept := make([]store.ProbeTimelineBucket, 0, len(buckets))
		for _, b := range buckets {
			if allowed[b.ModelID] {
				kept = append(kept, b)
			}
		}
		buckets = kept
	}
	writeJSON(w, http.StatusOK, buckets)
}
