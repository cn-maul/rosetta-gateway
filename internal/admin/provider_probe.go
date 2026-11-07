package admin

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/cn-maul/rosetta"
	wireproto "github.com/cn-maul/rosetta-gateway/internal/protocol"
	"github.com/cn-maul/rosetta-gateway/internal/upstream"
)

// provider_probe.go 实现「上游协议探测」：对同一个 endpoint 并发打三条协议
// 路径（chat / response / anthropic），回答「这个中转到底支持哪几种协议」。
//
// # 为什么要单独做这个端点
//
// 管理员拿到一个中转地址时，唯一能确认协议的办法是「建好 provider、配好路由、
// 发一条真实请求看报什么错」。这条链路上任何一环出错（凭据、路由、模型名）
// 都会掩盖「上游根本不支持 response 协议」这个真正的原因。探测把这件事拆成
// 一个不依赖任何已配置数据的一次性请求：只判**路径存不存在**，不判能不能
// 成功推理。
//
// # 判定口径：ok = 状态码不是 404/405
//
// 这是「路径存在性」而不是「可用性」的判据。真实的协议实现几乎不会对合法
// 路径回 200 —— 空 body 缺 model/messages 必然是 400，缺 key 是 401/403，
// 限流是 429，上游过载是 5xx。这些**全部**意味着「这条路径有人接」，只是
// 这次请求没被接受。反过来，路径不存在时上游才会回 404，方法不对回 405。
// 用「状态码 == 200」做判据会把所有协议都判成不支持，用「4xx 即不支持」
// 会把 401 这种「路径对了只差 key」判死。

const (
	// protocolProbeTimeout 是**单条**探测的时间上限。
	//
	// 10s 与仓库里其它管理面即时探测（provider Test 的 15s、model discover）
	// 同一量级：够慢上游握手 + 走完代理，又不至于让管理页面以为卡死。
	protocolProbeTimeout = 10 * time.Second

	// protocolProbeBudget 是三条探测的**总**时间上限。
	//
	// 15s 而不是 30s：三条是并发的，正常最坏也就一条的时间；总预算略微
	// 放大只为覆盖「三条同时排队等同一个代理连接」的情况。前端这条请求的
	// 超时是 25s（见 web/src/api.ts 的 probeProtocols），后端 15s 必须
	// 先于它到期，否则用户看到的是前端超时、拿不到任何一条结论。
	protocolProbeBudget = 15 * time.Second

	// probeRequestBody 是探测请求体。
	//
	// 恒为 `{}`（不带 model、不带 messages）是用心选过的：
	//   - 带真实模型名就可能真的产生一次推理计费，而管理员只是想看路径在不在；
	//   - 带 model 还要先知道这个上游认哪个模型名，探测就退化成「配置题」；
	//   - `{}` 对三条路径都是「格式合法但缺必填字段」→ 真实实现回 400，
	//     不存在回 404，正是我们要区分的那一对。
	probeRequestBody = "{}"
)

// protocolProbeRequest 是探测端点的输入。
//
// 不加 @ts-type：前端的调用参数是内联字面量类型（web/src/api.ts 的
// probeProtocols），types.ts 里没有对应接口，标了会让 dtoContract 测试找不到映射。
type protocolProbeRequest struct {
	Endpoint string `json:"endpoint"`
	// APIKey 可空：有些内网网关不校验凭据，填了反而会被 401。
	APIKey string `json:"api_key"`
	// NetworkID 空串 / "direct" = 直连；否则必须是 networks 表里的一条。
	NetworkID string `json:"network_id"`
}

// protocolProbeEntry 是一条协议路径的探测结论。
//
// 三个协议固定按 chat / response / anthropic 的顺序返回，前端按下标对齐
// 展示（见 web/src/pages/Providers.tsx）。顺序在服务端定死而不是让前端排序：
// 前端排序需要它自己知道「chat 在 response 前面」，多一份会漂移的知识。
// @ts-type ProtocolProbeEntry
type protocolProbeEntry struct {
	Protocol string `json:"protocol"`
	OK       bool   `json:"ok"`
	Status   int    `json:"status"`
	Message  string `json:"message"`
}

// @ts-type ProtocolProbeResult
type protocolProbeResult struct {
	Results []protocolProbeEntry `json:"results"`
}

// protocolProbePaths 是要试的三条路径，按固定顺序。
//
// 路径只写「版本之后」的那一段：版本层由 joinProbeURL 拼，与数据面
// （rosetta 的 joinEndpoint）完全同语义 —— endpoint 填到域根还是填到 /v1，
// 探测都必须得到同一个地址，否则会出现「探测 404 但转发正常」这种最难查
// 的不一致。
var protocolProbePaths = []struct {
	protocol string
	path     string
}{
	{wireproto.Chat, "/chat/completions"},
	{wireproto.Response, "/responses"},
	{wireproto.Anthropic, "/messages"},
}

// ProbeProtocols 并发探测三条协议路径。
//
// # 为什么恒回 200
//
// 与 provider Test / balance 同一口径（见 provider_handler.go 的 Balance 注释）：
// 「这条路径不存在」是**探测的结论**，不是管理接口调用失败。用 4xx/5xx 表达它
// 会让前端只拿到一句统一文案，把「不支持该协议」「key 不对」「网络不通」三种
// 完全不同的后续动作压成同一个错误。
//
// 只有**参数非法**才 400：endpoint 缺失/不合法、network_id 指向不存在或代理
// 地址损坏的网络 —— 这些是调用方（或它选的网络配置）给错了东西，重试无意义。
func (h *ProviderHandler) ProbeProtocols(w http.ResponseWriter, r *http.Request) {
	var req protocolProbeRequest
	if err := decodeJSON(w, r, &req); err != nil {
		if decodeFailed(err) {
			return
		}
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}

	endpoint := strings.TrimSpace(req.Endpoint)
	if endpoint == "" {
		writeError(w, http.StatusBadRequest, "endpoint is required")
		return
	}
	if err := validateProbeEndpoint(endpoint); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	proxyURL, err := h.resolveProbeProxy(r.Context(), req.NetworkID)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	// 复用数据面的出站 transport（见 upstream.ProbeHTTPClientFor 的注释）：
	// 探测结论要能迁移到真实转发上，连接参数就不能是另一套。
	// 3xx 不跟随：探测请求带 api_key，跟随跳转等于把凭据重发到响应头
	// 指定的任意地址。
	client, err := upstream.ProbeHTTPClientFor(proxyURL, protocolProbeTimeout)
	if err != nil {
		// 网络条目里的 proxy_url 坏了。回 400 而不是「三条 ok=false」：
		// 三条全 false 会把「网络配错了」伪装成「三条路径都不存在」，
		// 把管理员指向完全错误的排查方向（去查上游，而问题在代理）。
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), protocolProbeBudget)
	defer cancel()

	apiKey := strings.TrimSpace(req.APIKey)
	results := make([]protocolProbeEntry, len(protocolProbePaths))
	var wg sync.WaitGroup
	for i := range protocolProbePaths {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// 每个 goroutine 只写自己那一格：不开锁也不会竞态，
			// 而顺序天然保持 protocolProbePaths 的声明顺序。
			results[i] = probeOneProtocol(ctx, client, endpoint, apiKey, protocolProbePaths[i].protocol, protocolProbePaths[i].path)
		}(i)
	}
	wg.Wait()

	writeJSON(w, http.StatusOK, protocolProbeResult{Results: results})
}

// probeOneProtocol 探测单条路径，永不返回 error —— 失败也是结论。
func probeOneProtocol(ctx context.Context, client *http.Client, endpoint, apiKey, protocol, path string) protocolProbeEntry {
	probeURL := joinProbeURL(endpoint, path)
	entry := protocolProbeEntry{Protocol: protocol}
	// 消息里回显地址是为了让管理员能直接复制去 curl；走脱敏函数是纵深
	// 防御 —— endpoint 在校验阶段已禁掉 userinfo/查询串，但这个函数不只
	// 服务于「已校验」的输入。
	shown := displayProbeEndpoint(probeURL)

	// 单条超时压在 10s：总预算（15s）管不到「三条同时慢」之外的情况，
	// 而 client.Timeout 是唯一的兜底 —— context 到期与 client 超时谁先
	// 到都算这条探测失败，但不能只依赖其中一层。
	reqCtx, cancel := context.WithTimeout(ctx, protocolProbeTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, probeURL, strings.NewReader(probeRequestBody))
	if err != nil {
		entry.Message = fmt.Sprintf("%s 请求构造失败：%v", shown, err)
		return entry
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if apiKey != "" {
		if protocol == wireproto.Anthropic {
			// Anthropic 认 x-api-key 而不认 Authorization；版本头必须同时给，
			// 缺它时上游回 400 —— 那也会被算成「路径存在」，但错误原因会
			// 被掩盖成「格式不对」而不是「缺 header」。
			req.Header.Set("x-api-key", apiKey)
			req.Header.Set("anthropic-version", rosetta.AnthropicVersion)
		} else {
			req.Header.Set("Authorization", "Bearer "+apiKey)
		}
	} else if protocol == wireproto.Anthropic {
		// 没给 key 也带上版本头：这样 401「缺 key」与 400「缺版本头」
		// 不会混在一起，管理员看到的原因才是真的原因。
		req.Header.Set("anthropic-version", rosetta.AnthropicVersion)
	}

	resp, err := client.Do(req)
	if err != nil {
		// 传输层失败（DNS、拒绝连接、TLS、超时）。err 里只会有 URL ——
		// api_key 在 header 里，http 包不会把它拼进 error，这里也绝不再
		// 手动拼 header 进去。
		entry.Message = fmt.Sprintf("%s 请求失败：%v", shown, err)
		return entry
	}
	defer resp.Body.Close()
	// 排空少量 body 让连接可复用；上限截断是为了防止上游回一个巨大错误页
	// 把这次探测拖成「读 body 超时」。内容本身**不读进任何可回显的位置**：
	// 上游响应体可能是内网服务的可读内容（见 upstream.fetchModels 的同款注释）。
	io.Copy(io.Discard, io.LimitReader(resp.Body, 32<<10))

	status := resp.StatusCode
	entry.Status = status
	entry.OK = status != http.StatusNotFound && status != http.StatusMethodNotAllowed
	switch {
	case !entry.OK:
		entry.Message = fmt.Sprintf("%s 路径不存在（HTTP %d）", shown, status)
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		// 这是最有价值的一格结论：路径在、但凭据不对。前端据此显示黄色告警
		// 而不是「不支持」（见 Providers.tsx 的 probeTone）。
		if apiKey == "" {
			entry.Message = fmt.Sprintf("%s 路径存在，但需要鉴权（HTTP %d）：请填写 api_key 后重试", shown, status)
		} else {
			entry.Message = fmt.Sprintf("%s 路径存在，但鉴权未通过（HTTP %d）：请检查 api_key 是否属于这个上游", shown, status)
		}
	default:
		entry.Message = fmt.Sprintf("%s 路径存在（HTTP %d）", shown, status)
	}
	return entry
}

// resolveProbeProxy 把 network_id 解析成代理地址（空串 = 直连）。
//
// 复用 resolveNetworkID 而不是在这里重写一份校验：错误文案必须与
// provider 创建时逐字一致，否则同一个错误的两种说法会让人以为是两件事。
// 代价是多一次 GetNetwork 读 —— 探测是低频管理操作，可读性更值钱。
func (h *ProviderHandler) resolveProbeProxy(ctx context.Context, networkID string) (string, error) {
	id, err := resolveNetworkID(ctx, h.store, networkID)
	if err != nil || id == "" {
		return "", err
	}
	n, err := h.store.GetNetwork(ctx, id)
	if err != nil {
		return "", err
	}
	if n == nil {
		// 校验通过后被并发删除。回错误而不是静默直连：静默直连会让探测
		// 结论来自一条与真实转发不同的网络路径，而这正是探测要排除的变量。
		return "", fmt.Errorf("network_id %q 指向的网络不存在（可能已被删除）", id)
	}
	return n.ProxyURL, nil
}

// validateProbeEndpoint 校验探测用的 endpoint。
//
// 规则与 rosetta（数据面 SDK）的 validateEndpoint 对齐 —— 探测宽松、转发严格
// 会让「探测通过但建 provider 后转发失败」成为可能，而这个端点存在的意义
// 恰恰是提前发现这类问题。重复实现的原因：rosetta 的这些函数未导出。
func validateProbeEndpoint(raw string) error {
	if strings.TrimSpace(raw) != raw {
		return fmt.Errorf("endpoint 首尾不能有空白")
	}
	if strings.IndexFunc(raw, func(r rune) bool { return r < 0x20 || r == 0x7f }) >= 0 {
		return fmt.Errorf("endpoint 不能包含控制字符")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Opaque != "" || u.User != nil || u.Host == "" {
		return fmt.Errorf("endpoint 不是合法的 URL（应形如 https://api.example.com）")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("endpoint 只支持 http/https，当前是 %q", u.Scheme)
	}
	if u.RawQuery != "" || u.Fragment != "" {
		// 查询串里常被塞凭据，而它会随请求行落进上游访问日志；数据面同样
		// 拒绝这类 endpoint，所以这里提前说清楚而不是打出去再报错。
		return fmt.Errorf("endpoint 不能带查询串或片段（%s），版本层请写在路径里", displayProbeEndpoint(raw))
	}
	return nil
}

// displayProbeEndpoint 去掉 userinfo / 查询串 / 片段后再回显，用于错误与
// 结论文案。与 rosetta 的 displayEndpoint 同语义：即使 endpoint 已被校验
// 拒绝，也不把其中可能藏着的凭据原样回给前端或写进日志。
func displayProbeEndpoint(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	if u.User == nil && u.RawQuery == "" && u.Fragment == "" {
		return raw
	}
	u.User = nil
	u.RawQuery = ""
	u.Fragment = ""
	return u.String()
}

// joinProbeURL 把 endpoint 与版本之后的路径拼起来。
//
// 与 rosetta 的 joinEndpoint 逐字同语义（该函数未导出，只能复制）：
//   - 去掉末尾斜杠；
//   - endpoint 解析成功且路径为空或 "/" 时补一层 /v1（域根形态）；
//   - 已有路径（如 https://api.x.com/v1）时不再补版本，直接接路径；
//   - 解析失败时退化成字符串拼接（合法的 http(s) URL 不会走到这里）。
//
// 判据用「路径是否为空」而不是「是否以 /v1 结尾」：中转地址可以是
// https://x.com/proxy/v1，也可以是 https://x.com/proxy —— 后者要拼成
// /proxy/v1/chat/completions，前者拼成 /proxy/v1/chat/completions，
// 两者都必须只补一层版本。
func joinProbeURL(base, path string) string {
	b := strings.TrimRight(base, "/")
	if u, err := url.Parse(b); err == nil && u.Opaque == "" && u.Scheme != "" && u.Host != "" {
		if u.Path == "" || u.Path == "/" {
			u.Path = "/v1"
		}
		u.Path = strings.TrimSuffix(u.Path, "/") + path
		return u.String()
	}
	if u, err := url.Parse(b); err == nil && (u.Path == "" || u.Path == "/") {
		b += "/v1"
	}
	return b + path
}
