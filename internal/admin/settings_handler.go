package admin

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/cn-maul/rosetta-gateway/internal/config"
	"github.com/cn-maul/rosetta-gateway/internal/store"
)

type SettingsHandler struct {
	store *store.Store
	cfg   *config.Config
}

func NewSettingsHandler(st *store.Store, cfg *config.Config) *SettingsHandler {
	return &SettingsHandler{store: st, cfg: cfg}
}

// settingsResponse 是「设置」页的全部可热改全局默认。
//
// 两类来源：
//   - 模型容量默认（model_defaults）：探测不到上游容量时的兜底；
//   - 运行时默认（runtime_defaults）：各类超时与故障转移策略。
//
// Get 一律回显**生效值**：DB 里没配（0）就填 config.json 的值，这样界面显示的
// 是「真正在用的数」，而不是一个空 0 让人以为是「不限」。
type settingsResponse struct {
	DefaultContextWindow   int `json:"default_context_window"`
	DefaultMaxOutputTokens int `json:"default_max_output_tokens"`

	UpstreamTimeoutMs         int `json:"upstream_timeout_ms"`
	StreamIdleTimeoutMs       int `json:"stream_idle_timeout_ms"`
	StreamFirstTokenTimeoutMs int `json:"stream_first_token_timeout_ms"`
	FailoverMaxTargets        int `json:"failover_max_targets"`
	FailoverFailureThreshold  int `json:"failover_failure_threshold"`
}

func (h *SettingsHandler) Get(w http.ResponseWriter, r *http.Request) {
	d, err := h.store.GetModelDefaults(r.Context())
	if err != nil {
		writeServerError(w, "get model defaults", err)
		return
	}

	rt, err := h.store.GetRuntimeDefaults(r.Context())
	if err != nil {
		writeServerError(w, "get runtime defaults", err)
		return
	}
	// 未配置（0）→ 回显 config 的生效值，界面不出现「0 = 不限」的误读。
	if rt.UpstreamTimeoutMs <= 0 {
		rt.UpstreamTimeoutMs = h.cfg.Defaults.UpstreamTimeoutMs
	}
	if rt.StreamIdleTimeoutMs <= 0 {
		rt.StreamIdleTimeoutMs = h.cfg.Defaults.StreamIdleTimeoutMs
	}
	if rt.StreamFirstTokenTimeoutMs <= 0 {
		rt.StreamFirstTokenTimeoutMs = h.cfg.Defaults.StreamFirstTokenTimeoutMs
	}
	if rt.FailoverMaxTargets <= 0 {
		rt.FailoverMaxTargets = h.cfg.FailoverMaxTargets()
	}
	if rt.FailoverFailureThreshold <= 0 {
		rt.FailoverFailureThreshold = h.cfg.FailoverFailureThreshold()
	}

	writeJSON(w, http.StatusOK, settingsResponse{
		DefaultContextWindow:      d.ContextWindow,
		DefaultMaxOutputTokens:    d.MaxOutputTokens,
		UpstreamTimeoutMs:         rt.UpstreamTimeoutMs,
		StreamIdleTimeoutMs:       rt.StreamIdleTimeoutMs,
		StreamFirstTokenTimeoutMs: rt.StreamFirstTokenTimeoutMs,
		FailoverMaxTargets:        rt.FailoverMaxTargets,
		FailoverFailureThreshold:  rt.FailoverFailureThreshold,
	})
}

// Update 保存设置。校验必须硬性：
//   - 超时会直接喂给 time.AfterFunc / context.WithTimeout —— 0 或负数会变成
//     「立即超时」，把全部转发打挂（config.validate 里有同一份约束）；
//   - 尝试目标数与熔断阈值至少为 1，否则故障转移要么不转移、要么第一次失败就熔断。
//
// 保存后由前端触发 POST /admin/api/reload 重建快照，运行时立即生效（无需重启）。
func (h *SettingsHandler) Update(w http.ResponseWriter, r *http.Request) {
	var req settingsResponse
	if err := decodeJSON(w, r, &req); err != nil {
		if errors.Is(err, errUnsupportedMediaType) {
			return
		}
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if req.DefaultContextWindow <= 0 || req.DefaultMaxOutputTokens <= 0 {
		writeError(w, http.StatusBadRequest, "默认上下文与最大输出必须为正整数")
		return
	}
	// maxDurMillis 与 config.validate 里的同名常量一致：time.Duration 是
	// int64 纳秒，ms > 9.223e12 时 `time.Duration(ms) * time.Millisecond`
	// 会回绕成负数 —— 负 Duration 让 context.WithTimeout 立即过期、
	// time.AfterFunc 立即开火，等于全站转发被打挂且 UI 上看不出异常。
	// 详见 config.validate 的注释。
	const maxDurMillis = 86_400_000
	for _, f := range []struct {
		name string
		val  int
		max  int
	}{
		{"upstream_timeout_ms", req.UpstreamTimeoutMs, maxDurMillis},
		{"stream_idle_timeout_ms", req.StreamIdleTimeoutMs, maxDurMillis},
		{"stream_first_token_timeout_ms", req.StreamFirstTokenTimeoutMs, maxDurMillis},
		// 熔断阈值过大等于永不熔断。
		{"failover_max_targets", req.FailoverMaxTargets, 100},
		{"failover_failure_threshold", req.FailoverFailureThreshold, 1000},
	} {
		if f.val < 1 {
			writeError(w, http.StatusBadRequest, f.name+" 必须为 >= 1 的整数")
			return
		}
		if f.val > f.max {
			writeError(w, http.StatusBadRequest,
				fmt.Sprintf("%s 过大：%d（上限 %d）", f.name, f.val, f.max))
			return
		}
	}

	d := store.ModelDefaults{
		ContextWindow:   req.DefaultContextWindow,
		MaxOutputTokens: req.DefaultMaxOutputTokens,
	}
	rt := store.RuntimeDefaults{
		UpstreamTimeoutMs:         req.UpstreamTimeoutMs,
		StreamIdleTimeoutMs:       req.StreamIdleTimeoutMs,
		StreamFirstTokenTimeoutMs: req.StreamFirstTokenTimeoutMs,
		FailoverMaxTargets:        req.FailoverMaxTargets,
		FailoverFailureThreshold:  req.FailoverFailureThreshold,
	}
	if err := h.store.SaveSettings(r.Context(), d, rt); err != nil {
		writeServerError(w, "save settings", err)
		return
	}

	writeJSON(w, http.StatusOK, req)
}
