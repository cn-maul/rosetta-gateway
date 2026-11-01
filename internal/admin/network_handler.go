package admin

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/cn-maul/rosetta-gateway/internal/config"
	"github.com/cn-maul/rosetta-gateway/internal/store"
	"github.com/cn-maul/rosetta-gateway/internal/upstream"
)

// NetworkHandler 管理「出站网络选项」—— 一份可复用的代理地址清单。
//
// 存在的理由：代理地址天然是**多个上游共用**的（同一个 clash / 公司代理），
// 而此前它以纯文本存在每个 provider 行上。改一次要改 N 处，且手填 URL
// 极易打错（少个斜杠、端口写错）—— 而打错不报错，只是连不上。
// 建成可枚举的清单后，新建上游从下拉里选，要新增点「新建网络」。
//
// 直连不是一条网络记录，而是 store.NetworkDirectID 这个保留 id（见该常量
// 的注释），由前端作为下拉的第一项展示。
type NetworkHandler struct {
	store *store.Store
	// cfg 只为算探测窗口（probes.window_size × interval_sec）：列表上的
	// 延迟数字必须与供应商列表、健康摘要是同一个窗口，否则同一时刻两个
	// 页面能给出互相矛盾的平均值。cfg 为 nil 时 probeWindowMs 兜底 1 小时。
	cfg *config.Config
}

func NewNetworkHandler(st *store.Store, cfg *config.Config) *NetworkHandler {
	return &NetworkHandler{store: st, cfg: cfg}
}

// @ts-type Network
type networkResponse struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// ProxyURL 是这条网络的代理地址（http/https/socks5/socks5h）。
	ProxyURL string `json:"proxy_url"`
	// ProviderCount 是引用它的上游数（派生值）。列表页用它提示
	// 「删掉这条会影响几个上游」—— 前端逐个查是 N+1。
	ProviderCount int   `json:"provider_count"`
	CreatedAt     int64 `json:"created_at"`

	// 以下是**探针测得**的连通性摘要（2026-11-01 起代理由后台定时测速，
	// 不再靠「测试」按钮）。窗口与供应商延迟同一口径。
	//
	// LatencySamples = 0 表示窗口内没有成功样本；此时 LastOK/LastError
	// 仍有意义 —— 它们回答「这段时间一次都没通，是没测过还是一直失败」，
	// 而只显示「—」会把两种情况糊在一起。
	LatencyMs      int64  `json:"latency_ms"`
	LatencyP95Ms   int64  `json:"latency_p95_ms"`
	LatencySamples int64  `json:"latency_samples"`
	LastProbeOK    bool   `json:"last_probe_ok"`
	LastProbeError string `json:"last_probe_error,omitempty"`
	// LastProbeAt 是最近一次探测时间（毫秒），0 = 从未探测。
	LastProbeAt int64 `json:"last_probe_at"`
}

func toNetworkResponse(n store.Network) networkResponse {
	return networkResponse{
		ID: n.ID, Name: n.Name, ProxyURL: n.ProxyURL,
		ProviderCount: n.ProviderCount, CreatedAt: n.CreatedAt,
	}
}

// List 列出全部网络（不含直连那个保留 id —— 它不是一条记录）。
func (h *NetworkHandler) List(w http.ResponseWriter, r *http.Request) {
	networks, err := h.store.ListNetworks(r.Context())
	if err != nil {
		writeServerError(w, "list networks", err)
		return
	}
	latency := h.networkProbeLatency(r.Context())
	result := make([]networkResponse, 0, len(networks))
	for _, n := range networks {
		res := toNetworkResponse(n)
		applyNetworkProbeLatency(&res, latency)
		result = append(result, res)
	}
	writeJSON(w, http.StatusOK, result)
}

// networkProbeLatency 取窗口内各代理的测速摘要，供列表填充延迟列。
//
// 与 ProviderHandler 的 probeLatency 同一取舍：探测表查询失败只记日志、返回
// nil（前端显示「—」），不让「代理清单」这个管理入口跟着 500 ——
// 延迟是排障线索，不是配置本身。
func (h *NetworkHandler) networkProbeLatency(ctx context.Context) map[string]store.NetworkLatency {
	lat, err := h.store.NetworkLatencyByID(ctx, probeWindowMs(h.cfg))
	if err != nil {
		slog.Warn("network list: probe latency unavailable", "error", err)
		return nil
	}
	return lat
}

// applyNetworkProbeLatency 把测速结果填进响应。
//
// 三条信息分开填，因为它们各自可能缺失：
//   - 平均/P95/样本数只在窗口内有成功样本时才有意义（Samples=0 → 保持零值，
//     前端显示「—」，不能写 0ms 把「没测通」印成「快」）；
//   - 最近一次成败与时间**任何时候都填**：从没测过（LastProbeAt=0）与最近
//     一次失败（LastProbeOK=false + 错误正文）是两种完全不同的状态，
//     界面上必须能区分。
func applyNetworkProbeLatency(res *networkResponse, latency map[string]store.NetworkLatency) {
	l, ok := latency[res.ID]
	if !ok {
		return
	}
	res.LastProbeAt = l.LastTs
	res.LastProbeOK = l.LastOK
	res.LastProbeError = l.LastError
	if l.Samples <= 0 {
		return
	}
	res.LatencyMs = l.AvgMs
	res.LatencyP95Ms = l.P95Ms
	res.LatencySamples = l.Samples
}

type networkRequest struct {
	Name     *string `json:"name"`
	ProxyURL *string `json:"proxy_url"`
}

func (h *NetworkHandler) Create(w http.ResponseWriter, r *http.Request) {
	var req networkRequest
	if err := decodeJSON(w, r, &req); err != nil {
		if decodeFailed(err) {
			return
		}
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	name := strings.TrimSpace(derefStr(req.Name))
	proxyURL := strings.TrimSpace(derefStr(req.ProxyURL))
	if name == "" {
		writeError(w, http.StatusBadRequest, "name 不能为空")
		return
	}
	if err := validateProxyURL(proxyURL); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	n := &store.Network{ID: "net-" + generateID(), Name: name, ProxyURL: proxyURL}
	if err := h.store.CreateNetwork(r.Context(), n); err != nil {
		if store.IsUniqueViolation(err) {
			writeError(w, http.StatusConflict, "已存在同名的网络")
			return
		}
		writeServerError(w, "create network", err)
		return
	}
	writeJSON(w, http.StatusCreated, toNetworkResponse(*n))
}

func (h *NetworkHandler) Update(w http.ResponseWriter, r *http.Request, id string) {
	var req networkRequest
	if err := decodeJSON(w, r, &req); err != nil {
		if decodeFailed(err) {
			return
		}
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	// PATCH 语义（2026-10-21 修 A7-10）：两个字段都是指针，缺失即「不改」。
	//
	// 此前这里用 derefStr 取零值后立刻把空串判为非法，于是只 PATCH name 或
	// 只 PATCH proxy_url 都会 400 —— 名义上是 PATCH，实际必须整条重发。
	// 前端当前总同时发两个字段（Networks.vue 的表单整体提交），所以还没
	// 在 UI 上暴露，但契约已经是错的，且下一个接入方一定会踩到。
	//
	// 空串不是「未提供」：proxy_url 显式传空串 = 清空代理直连上游。
	name := strings.TrimSpace(derefStr(req.Name))
	if req.Name != nil {
		if name == "" {
			writeError(w, http.StatusBadRequest, "name 不能为空")
			return
		}
	}
	proxyURL := strings.TrimSpace(derefStr(req.ProxyURL))
	if req.ProxyURL != nil {
		if err := validateProxyURL(proxyURL); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	// 两个字段都没提供：这不是错（幂等的空 PATCH），但也没什么可改的。
	// 返回 400 而不是 200，是为了让调用方发现自己拼错了请求体，
	// 而不是拿到一个「成功」的假象。
	if req.Name == nil && req.ProxyURL == nil {
		writeError(w, http.StatusBadRequest, "没有要更新的字段（name / proxy_url 至少提供一个）")
		return
	}
	// 部分 PATCH 要把未提供的字段从库里读回来再写：UpdateNetwork 是无条件
	// 覆盖两列，直接把零值写进去会把没打算改的那一列清空。这不是多一次
	// 查询的成本问题，而是「PATCH 变成清空另一个字段」的正确性问题。
	if req.Name == nil || req.ProxyURL == nil {
		cur, err := h.store.GetNetwork(r.Context(), id)
		if err != nil {
			writeServerError(w, "get network", err)
			return
		}
		if cur == nil {
			writeError(w, http.StatusNotFound, "网络不存在（可能已被并发删除）")
			return
		}
		if req.Name == nil {
			name = cur.Name
		}
		if req.ProxyURL == nil {
			proxyURL = cur.ProxyURL
		}
	}
	if err := h.store.UpdateNetwork(r.Context(), id, name, proxyURL); err != nil {
		if store.IsUniqueViolation(err) {
			writeError(w, http.StatusConflict, "已存在同名的网络")
			return
		}
		writeNotFoundOrError(w, "update network", "网络不存在（可能已被并发删除）", err)
		return
	}
	writeJSON(w, http.StatusOK, networkResponse{ID: id, Name: name, ProxyURL: proxyURL})
}

// Delete 删除一条网络。引用它的上游由 FK 的 ON DELETE SET NULL 回到直连，
// 不删上游 —— 详见 store.DeleteNetwork 的注释。
func (h *NetworkHandler) Delete(w http.ResponseWriter, r *http.Request, id string) {
	if err := h.store.DeleteNetwork(r.Context(), id); err != nil {
		writeDeleteError(w, "delete network", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// @ts-type NetworkTestResult
type networkTestResponse struct {
	Status    string `json:"status"`
	Message   string `json:"message"`
	LatencyMs int64  `json:"latency_ms"`
}

// Test 通过该代理访问一个固定的轻量探测地址，验证代理确实能建立出站连接。
//
// 状态码恒为 200：代理拒绝、超时或目标不可达是**测试结论**，不是管理接口
// 调用失败；只有网络记录不存在才返回 404。前端据此展示行内联通状态。
func (h *NetworkHandler) Test(w http.ResponseWriter, r *http.Request, id string) {
	n, err := h.store.GetNetwork(r.Context(), id)
	if err != nil {
		writeServerError(w, "get network", err)
		return
	}
	if n == nil {
		writeError(w, http.StatusNotFound, "代理不存在（可能已被并发删除）")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
	defer cancel()

	latencyMs, err := upstream.TestProxyConnectivity(ctx, n.ProxyURL)
	if err != nil {
		writeJSON(w, http.StatusOK, networkTestResponse{
			Status: "error", Message: err.Error(), LatencyMs: latencyMs,
		})
		return
	}
	writeJSON(w, http.StatusOK, networkTestResponse{
		Status:    "ok",
		Message:   fmt.Sprintf("代理可用，%dms", latencyMs),
		LatencyMs: latencyMs,
	})
}

// validateProxyURL 在写入前校验代理地址。
//
// **为什么必须在这里校验**：此前 proxy_url 是逐 provider 手填的，填错要到
// 请求真的打过去才暴露（表现是 504 / 连接超时，且日志里只有一条 WARN）。
// 现在它是一份共享清单 —— 填错会让**所有**选它的上游一起连不上，
// 故障面从「一个上游」放大到「一批」。共享配置的校验必须在写入点，
// 而不是等到使用时。
func validateProxyURL(raw string) error {
	if raw == "" {
		return errors.New("proxy_url 不能为空（不代理请用「直连」，它不是一个网络条目）")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return errors.New("proxy_url 不是合法的 URL（应形如 http://127.0.0.1:7897）")
	}
	switch u.Scheme {
	case "http", "https", "socks5", "socks5h":
		return nil
	default:
		return errors.New("proxy_url 的协议只支持 http / https / socks5 / socks5h，收到：" + u.Scheme)
	}
}
