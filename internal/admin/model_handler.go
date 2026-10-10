package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/cn-maul/rosetta-gateway/internal/config"
	"github.com/cn-maul/rosetta-gateway/internal/effort"
	"github.com/cn-maul/rosetta-gateway/internal/store"
	"github.com/cn-maul/rosetta-gateway/internal/upstream"
)

type ModelHandler struct {
	store     *store.Store
	masterKey []byte
	cfg       *config.Config
}

func NewModelHandler(st *store.Store, masterKey []byte, cfg *config.Config) *ModelHandler {
	return &ModelHandler{store: st, masterKey: masterKey, cfg: cfg}
}

// modelRequest 是上游模型的创建 / PATCH 输入。
//
// PATCH 语义（2026-09-21 重构）：字段为指针，nil = 未提供（保持原值）。
// ContextWindow / MaxOutputTokens 传 0 是合法值 —— 会落回 NULL，即「未设置」，
// 旧实现下这两个字段一旦写入就再也无法清空。
type modelRequest struct {
	ModelID         *string `json:"model_id"`
	DisplayName     *string `json:"display_name"`
	Enabled         *bool   `json:"enabled"`
	ContextWindow   *int    `json:"context_window"`
	MaxOutputTokens *int    `json:"max_output_tokens"`

	// 单价（元 / 百万 tokens），0 = 不计费。PATCH 语义同样是 nil = 保持原值、
	// 0 = 显式清空。price_input 是缓存未命中的输入价，price_cache_hit 是缓存
	// 命中的输入价，price_output 是输出价。
	PriceInput    *float64 `json:"price_input"`
	PriceCacheHit *float64 `json:"price_cache_hit"`
	PriceOutput   *float64 `json:"price_output"`

	// EffortLevels 是该模型支持的思考挡位（空串 = 清除，回到「未配置」）。
	//
	// 用逗号分隔的字符串而不是数组：这份配置天然要照着模型文档手工抄，
	// 字符串形式与库里那列、以及导出的 CSV 完全同形，少一次转换就少一处
	// 「界面能配但库里存不下」的环节。
	EffortLevels *string `json:"effort_levels"`

	// SupportsThinking 是「这个模型能不能思考」开关，三态：
	// 字段缺席 = 不改；true = 支持；false = 确定不支持；**null = 清除**
	// （回到「未配置」，网关不再干预）。
	//
	// 之所以要 null 这一档：库里那一列是可空的，而「清除」是界面上一个明确
	// 的动作（把勾去掉并保存）。裸 *bool 只能表达前三种 —— 省略和 null 都
	// 解不出来，于是「清除」永远做不到，只能从 true 翻到 false，而那在语义上
	// 完全相反（一个是「不知道」，一个是「确定不支持」）。
	//
	// 此前这一列已在 schema 里存在却**既无接口字段也无界面入口**，于是
	// 「配了不生效」—— 那正是这个字段要消灭的状态。
	SupportsThinking *nullableBool `json:"supports_thinking"`
}

// nullableBool 解码 JSON 的三态布尔：true / false / null。
//
// 字段缺席由外层的 *nullableBool 表达（指针为 nil），而这里的 null 表示
// 「显式清空」。两者必须分开 —— 缺席是「不改」，null 是「改成未配置」，
// 而 JSON 默认无法区分 null 与缺席（除非解到 *bool 的指针再判）。
type nullableBool struct {
	Value bool
	Valid bool // false = 显式 null，即清除
}

func (b *nullableBool) UnmarshalJSON(data []byte) error {
	if string(data) == "null" {
		b.Valid = false
		return nil
	}
	var v bool
	if err := json.Unmarshal(data, &v); err != nil {
		return fmt.Errorf("must be true, false or null: %w", err)
	}
	b.Value, b.Valid = v, true
	return nil
}

// asOpt 把已解码的三态转成落库用的可空指针：显式 null → nil（未配置）。
func (b *nullableBool) asOpt() *bool {
	if !b.Valid {
		return nil
	}
	v := b.Value
	return &v
}

// applyEffortLevels 把请求里的逗号分隔挡位解析进模型。
//
// 只做**语法**层面的一致化（大小写、别名、排序、重复），不替管理员判断
// 「这个模型到底支持哪些」—— 那是要照着厂商文档填的事实，网关猜不得。
// 解析不出任何已知档位时返回错误而不是静默忽略：忽略会让「填错了」表现为
// 「配了但没生效」，正是这个功能要消灭的那类沉默故障。
func applyEffortLevels(m *store.UpstreamModel, csv string) error {
	known, unknown := effort.ParseLevels(csv)
	if len(known) == 0 && len(unknown) == 0 {
		return nil // 空串 = 清除
	}
	if len(known) == 0 {
		return fmt.Errorf("effort_levels 里没有一个可识别的挡位（收到 %q），合法值：%s",
			csv, levelListHint())
	}
	m.EffortLevels = known
	m.UnknownLevels = unknown
	return nil
}

func levelListHint() string {
	names := make([]string, 0, len(effort.KnownLevels))
	for _, l := range effort.KnownLevels {
		names = append(names, string(l))
	}
	return strings.Join(names, " / ")
}

type modelResponse struct {
	ID              string  `json:"id"`
	ProviderID      string  `json:"provider_id"`
	ModelID         string  `json:"model_id"`
	DisplayName     string  `json:"display_name"`
	Enabled         bool    `json:"enabled"`
	ContextWindow   int     `json:"context_window"`
	MaxOutputTokens int     `json:"max_output_tokens"`
	PriceInput      float64 `json:"price_input"`
	PriceCacheHit   float64 `json:"price_cache_hit"`
	PriceOutput     float64 `json:"price_output"`
	TokensPerSec    float64 `json:"tokens_per_sec,omitempty"`
	TtfbMs          float64 `json:"ttfb_ms,omitempty"`
	SuccessRate     float64 `json:"success_rate"`
	CallCount       int     `json:"call_count,omitempty"`

	// EffortLevels 是该模型真实支持的思考挡位（强度升序）。
	// 空切片 = 未配置，此时数据面不干预思考强度。
	EffortLevels []effort.Level `json:"effort_levels"`
	// UnknownLevels 是配置里出现但网关不认识的原样值：前端必须显示出来，
	// 否则「配了不生效」会变成无从排查的静默失败。
	UnknownLevels []string `json:"unknown_effort_levels,omitempty"`
	// SupportsThinking 三态：nil = 未配置（不下发该字段），
	// true/false 才是显式声明。
	SupportsThinking *bool `json:"supports_thinking,omitempty"`
}

func (h *ModelHandler) List(w http.ResponseWriter, r *http.Request, providerID string) {
	if !requireProvider(w, r, h.store, providerID) {
		return
	}

	models, err := h.store.ListUpstreamModels(r.Context(), providerID)
	if err != nil {
		writeServerError(w, "list upstream models", err)
		return
	}
	// 统计只来自历史真实调用，不做探测；未调用过的模型不在 map 中，速度/成功率留空。
	stats, _ := h.store.ListModelThroughput(r.Context(), providerID)
	result := make([]modelResponse, 0, len(models))
	for _, m := range models {
		resp := toModelResponse(m)
		if st, ok := stats[m.ModelID]; ok {
			resp.TokensPerSec = st.Tps
			resp.TtfbMs = st.TtfbMs
			resp.SuccessRate = st.SuccessRate
			resp.CallCount = st.SuccessSample
		}
		result = append(result, resp)
	}
	writeJSON(w, http.StatusOK, result)
}

// ListAll 扁平返回全部上游模型（跨所有 provider），供前端一次取全、本地按
// provider_id 分组。此前 Routes/Settings 页先取 providers、再对每个 provider
// 各发一次 models 请求（1+N）；模型总量不大，一次取回即可。
//
// 刻意不带 per-provider 的吞吐/成功率：那是窗口函数重查询（见 ListModelThroughput），
// 且这两个页面只用到 id/provider_id/model_id/display_name/价格，用不到统计。
func (h *ModelHandler) ListAll(w http.ResponseWriter, r *http.Request) {
	models, err := h.store.ListAllUpstreamModels(r.Context())
	if err != nil {
		writeServerError(w, "list all upstream models", err)
		return
	}
	result := make([]modelResponse, 0, len(models))
	for _, m := range models {
		result = append(result, toModelResponse(m))
	}
	writeJSON(w, http.StatusOK, result)
}

func (h *ModelHandler) Create(w http.ResponseWriter, r *http.Request, providerID string) {
	if !requireProvider(w, r, h.store, providerID) {
		return
	}

	var req modelRequest
	if err := decodeJSON(w, r, &req); err != nil {
		if errors.Is(err, errUnsupportedMediaType) {
			return
		}
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}

	modelID := strings.TrimSpace(derefStr(req.ModelID))
	if modelID == "" {
		writeError(w, http.StatusBadRequest, "model_id is required")
		return
	}

	ctxWindow := derefInt(req.ContextWindow)
	maxOut := derefInt(req.MaxOutputTokens)
	if ctxWindow < 0 || maxOut < 0 {
		writeError(w, http.StatusBadRequest, "context_window and max_output_tokens cannot be negative")
		return
	}

	priceIn := derefFloat(req.PriceInput)
	priceHit := derefFloat(req.PriceCacheHit)
	priceOut := derefFloat(req.PriceOutput)
	if priceIn < 0 || priceHit < 0 || priceOut < 0 {
		writeError(w, http.StatusBadRequest, "price cannot be negative")
		return
	}

	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}

	m := &store.UpstreamModel{
		ID:              generateID(),
		ProviderID:      providerID,
		ModelID:         modelID,
		DisplayName:     strings.TrimSpace(derefStr(req.DisplayName)),
		Enabled:         enabled,
		ContextWindow:   ctxWindow,
		MaxOutputTokens: maxOut,
		PriceInput:      priceIn,
		PriceCacheHit:   priceHit,
		PriceOutput:     priceOut,
	}
	// 挡位：nil = 不设置（与 PATCH 的 nil 语义一致）。单条创建多用于「导入时
	// 顺手补一条」，管理员此刻往往没有档位信息，所以只在显式传了才校验。
	if req.EffortLevels != nil {
		if err := applyEffortLevels(m, *req.EffortLevels); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	if req.SupportsThinking != nil {
		m.SupportsThinking = req.SupportsThinking.asOpt()
	}

	if err := h.store.CreateUpstreamModel(r.Context(), m); err != nil {
		if store.IsUniqueViolation(err) {
			writeError(w, http.StatusConflict, "该上游下已存在同名模型")
			return
		}
		writeServerError(w, "create upstream model", err)
		return
	}

	writeJSON(w, http.StatusCreated, toModelResponse(*m))
}

func (h *ModelHandler) Update(w http.ResponseWriter, r *http.Request, id string) {
	existing, err := h.store.GetUpstreamModel(r.Context(), id)
	if err != nil {
		writeServerError(w, "get upstream model", err)
		return
	}
	if existing == nil {
		writeError(w, http.StatusNotFound, "model not found")
		return
	}

	var req modelRequest
	if err := decodeJSON(w, r, &req); err != nil {
		if errors.Is(err, errUnsupportedMediaType) {
			return
		}
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}

	if req.ModelID != nil {
		v := strings.TrimSpace(*req.ModelID)
		if v == "" {
			writeError(w, http.StatusBadRequest, "model_id cannot be empty")
			return
		}
		existing.ModelID = v
	}
	// 可清空字段：空串即清空
	if req.DisplayName != nil {
		existing.DisplayName = strings.TrimSpace(*req.DisplayName)
	}
	if req.Enabled != nil {
		existing.Enabled = *req.Enabled
	}
	// 传 0 = 显式清空（落 NULL）；负数是非法输入
	if req.ContextWindow != nil {
		if *req.ContextWindow < 0 {
			writeError(w, http.StatusBadRequest, "context_window cannot be negative")
			return
		}
		existing.ContextWindow = *req.ContextWindow
	}
	if req.MaxOutputTokens != nil {
		if *req.MaxOutputTokens < 0 {
			writeError(w, http.StatusBadRequest, "max_output_tokens cannot be negative")
			return
		}
		existing.MaxOutputTokens = *req.MaxOutputTokens
	}
	// 单价：nil = 不改，0 = 清空（不计费），负数非法。
	for _, p := range []struct {
		name string
		req  *float64
		dst  *float64
	}{
		{"price_input", req.PriceInput, &existing.PriceInput},
		{"price_cache_hit", req.PriceCacheHit, &existing.PriceCacheHit},
		{"price_output", req.PriceOutput, &existing.PriceOutput},
	} {
		if p.req == nil {
			continue
		}
		if *p.req < 0 {
			writeError(w, http.StatusBadRequest, p.name+" cannot be negative")
			return
		}
		*p.dst = *p.req
	}

	// 挡位：nil = 不改；空串 = 清除（回到「未配置」，数据面不再干预强度）。
	if req.EffortLevels != nil {
		if err := applyEffortLevels(existing, *req.EffortLevels); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	// 支持思考：nil = 不改；null = 清除（回到未配置）；true/false = 显式声明。
	if req.SupportsThinking != nil {
		existing.SupportsThinking = req.SupportsThinking.asOpt()
	}

	if err := h.store.UpdateUpstreamModel(r.Context(), id, existing); err != nil {
		if store.IsUniqueViolation(err) {
			writeError(w, http.StatusConflict, "该上游下已存在同名模型")
			return
		}
		writeNotFoundOrError(w, "update upstream model", "上游模型不存在（可能已被并发删除）", err)
		return
	}

	writeJSON(w, http.StatusOK, toModelResponse(*existing))
}

func (h *ModelHandler) Delete(w http.ResponseWriter, r *http.Request, id string) {
	if err := h.store.DeleteUpstreamModel(r.Context(), id); err != nil {
		writeDeleteError(w, "delete upstream model", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// modelTestResponse 是单模型可用性探测的响应体。
//
// # 为什么状态码恒为 200，失败放在 body 里
//
// 与 provider 的 Test 同一口径：探测失败的原因有十几种（凭据错、模型不存在、
// 没权限、上游 5xx、超时、网络不通），每一种都是**探测的结论**而不是
// 「这个管理接口调用失败」。用 4xx/5xx 表达它们，会让前端只能拿到
// ApiFail 里那句统一的错误文案，反而丢掉了真正的诊断信息。
//
// 只有「你调的 id 不存在」才用 404 —— 那是调用方用错了接口，不是模型不可用。
type modelTestResponse struct {
	Status string `json:"status"` // "ok" | "error"
	// ModelID 回显被探测的上游模型名。前端把它显示在结果里，
	// 避免「点了 A 行、结果来自 B 行」这种复核不了的错觉。
	ModelID    string `json:"model_id"`
	ProviderID string `json:"provider_id"`
	Message    string `json:"message"`
	// LatencyMs 是端到端往返耗时（含上游生成 1 个 token 的时间）。
	LatencyMs int64 `json:"latency_ms"`
	// InputTokens / OutputTokens 是上游回报的真实用量。带出来是为了让
	// 「这次探测花了多少」可见 —— 探测会真实计费，不该是个隐形成本。
	InputTokens  int64 `json:"input_tokens,omitempty"`
	OutputTokens int64 `json:"output_tokens,omitempty"`
	// InBand 表示上游用 HTTP 200 + 错误体回绝了请求（部分中转网关如此）。
	// 前端据此把文案说明白，否则「HTTP 成功却报错」看起来像网关自己坏了。
	InBand bool `json:"in_band,omitempty"`
}

// Test 对单个上游模型发一次最小真实推理，验证它现在可用。
//
// 与 ProviderHandler.Test 的分工：那个测的是「provider 这个 endpoint + 凭据
// 通不通」（拉 /models），**证明不了任何具体模型能推理** —— 模型下架、
// 账号无权限、名字写错时 /models 全都照常返回。所以这里的判据只能是
// 真的发一次推理请求，见 upstream.TestUpstreamModel 的说明与成本分析。
//
// 路径用 upstream_models.id 而不是 model_id 字符串：前者是稳定的主键，
// 后者在同一 provider 下唯一但在跨 provider 之间会重名，拿它做路径参数
// 无法唯一定位（而且可能含 / 等需要转义的字符）。
func (h *ModelHandler) Test(w http.ResponseWriter, r *http.Request, id string) {
	m, err := h.store.GetUpstreamModel(r.Context(), id)
	if err != nil {
		writeServerError(w, "get upstream model", err)
		return
	}
	if m == nil {
		writeError(w, http.StatusNotFound, "model not found")
		return
	}

	// 超时取「provider 自身超时 + 余量」：探测走的是同一条上游路径，
	// 给它比 provider 更短的预算会得到一个无法归因的 ctx 超时。
	// 上界 60s：一次 max_tokens=1 的调用不该更久，真卡住就得报出来，
	// 而不是让管理请求也跟着挂住。
	//
	// cfg 的空值兜底：NewModelHandler 的其它调用点（Discover）不碰 cfg，
	// 所以历史上允许传 nil（测试里就是这么用的）。本端点要经 cfg 算超时、
	// 还要把它交给 NewProviderClient，是第一个真正依赖它的路径 ——
	// 请求路径上 deref 一个 nil 指针会把 500 变成 panic，这里补上兜底。
	cfg := h.cfg
	if cfg == nil {
		cfg = config.Default()
	}
	timeout := cfg.UpstreamTimeout()
	if timeout <= 0 || timeout > 60*time.Second {
		timeout = 60 * time.Second
	}
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()

	res := upstream.TestUpstreamModel(ctx, h.store, m, h.masterKey, cfg)

	resp := modelTestResponse{
		Status:       "ok",
		ModelID:      m.ModelID,
		ProviderID:   m.ProviderID,
		LatencyMs:    res.LatencyMs,
		InputTokens:  res.InputTokens,
		OutputTokens: res.OutputTokens,
		InBand:       res.InBand,
	}
	if res.Err != nil {
		resp.Status = "error"
		resp.Message = res.Err.Error()
	} else {
		resp.Message = fmt.Sprintf("模型可用，%dms", res.LatencyMs)
	}
	writeJSON(w, http.StatusOK, resp)
}

type discoveredModel struct {
	ModelID         string `json:"model_id"`
	DisplayName     string `json:"display_name,omitempty"`
	ContextWindow   int    `json:"context_window,omitempty"`
	MaxOutputTokens int    `json:"max_output_tokens,omitempty"`
}

// Discover 直接拉取上游原始 /models，带出各家扩展的上下文/最大输出容量；
// 探测不到的条目用「设置」里的默认容量兜底，一并返回给前端。
func (h *ModelHandler) Discover(w http.ResponseWriter, r *http.Request, providerID string) {
	if !requireProvider(w, r, h.store, providerID) {
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()

	cands, err := upstream.DiscoverUpstreamModels(ctx, h.store, providerID, h.masterKey, h.cfg)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]string{"status": "error", "message": err.Error()})
		return
	}

	def, err := h.store.GetModelDefaults(ctx)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]string{"status": "error", "message": err.Error()})
		return
	}

	result := make([]discoveredModel, 0, len(cands))
	for _, c := range cands {
		name := c.DisplayName
		if name == "" {
			name = c.ID
		}
		ctxWindow := c.ContextWindow
		if ctxWindow <= 0 {
			ctxWindow = def.ContextWindow
		}
		maxOut := c.MaxOutputTokens
		if maxOut <= 0 {
			maxOut = def.MaxOutputTokens
		}
		result = append(result, discoveredModel{
			ModelID:         c.ID,
			DisplayName:     name,
			ContextWindow:   ctxWindow,
			MaxOutputTokens: maxOut,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "models": result})
}

// modelImportItem 是批量导入的单条模型。
type modelImportItem struct {
	ModelID         string `json:"model_id"`
	DisplayName     string `json:"display_name"`
	ContextWindow   int    `json:"context_window"`
	MaxOutputTokens int    `json:"max_output_tokens"`
}

// ImportModels 批量 upsert 模型（按 provider_id+model_id 幂等），一次写入避免逐条 reload。
func (h *ModelHandler) ImportModels(w http.ResponseWriter, r *http.Request, providerID string) {
	if !requireProvider(w, r, h.store, providerID) {
		return
	}

	var body struct {
		Models []modelImportItem `json:"models"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		if errors.Is(err, errUnsupportedMediaType) {
			return
		}
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}

	def, err := h.store.GetModelDefaults(r.Context())
	if err != nil {
		writeServerError(w, "get model defaults", err)
		return
	}

	models := make([]*store.UpstreamModel, 0, len(body.Models))
	for _, it := range body.Models {
		if strings.TrimSpace(it.ModelID) == "" {
			continue
		}
		ctxWindow := it.ContextWindow
		if ctxWindow <= 0 {
			ctxWindow = def.ContextWindow
		}
		maxOut := it.MaxOutputTokens
		if maxOut <= 0 {
			maxOut = def.MaxOutputTokens
		}
		models = append(models, &store.UpstreamModel{
			ID:              generateID(),
			ProviderID:      providerID,
			ModelID:         strings.TrimSpace(it.ModelID),
			DisplayName:     it.DisplayName,
			Enabled:         true,
			ContextWindow:   ctxWindow,
			MaxOutputTokens: maxOut,
		})
	}

	if err := h.store.ImportUpstreamModels(r.Context(), models); err != nil {
		writeServerError(w, "import upstream models", err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "imported": len(models)})
}

func toModelResponse(m store.UpstreamModel) modelResponse {
	// 挡位切片必须是**非 nil** 的：JSON 里 null 与 [] 对前端是两种状态，
	// 而这里对外承诺的就是「空数组 = 未配置」，nil 会序列化成 null。
	levels := m.EffortLevels
	if levels == nil {
		levels = []effort.Level{}
	}
	return modelResponse{
		ID:               m.ID,
		ProviderID:       m.ProviderID,
		ModelID:          m.ModelID,
		DisplayName:      m.DisplayName,
		Enabled:          m.Enabled,
		ContextWindow:    m.ContextWindow,
		MaxOutputTokens:  m.MaxOutputTokens,
		PriceInput:       m.PriceInput,
		PriceCacheHit:    m.PriceCacheHit,
		PriceOutput:      m.PriceOutput,
		EffortLevels:     levels,
		UnknownLevels:    m.UnknownLevels,
		SupportsThinking: m.SupportsThinking,
	}
}
