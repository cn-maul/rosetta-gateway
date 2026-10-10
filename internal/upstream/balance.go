package upstream

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/cn-maul/rosetta-gateway/internal/store"
)

// 上游余额查询。
//
// # 这不是「网关自己的账」
//
// 网关自己的余额（用户余额、费用统计）在 store 与 cmd/gateway/billing.go 里，
// 与本文件无关。本文件问的是**另一个问题**：管理员在本网关上配的每一把上游
// API Key，在上游那边还剩多少钱。这是采购/续费决策的依据，网关内部的账
// 答不了 —— 用户花掉的钱已经付给上游了，而上游那边扣了多少只有上游知道。
//
// # 为什么按**凭据**查而不是按 provider 查
//
// 余额挂在 API Key 上，不在 endpoint 上：同一个上游的两把 key 余额互相独立，
// 而网关恰恰是多 key 轮换的（见 Pool.selectWeighted）。按 provider 汇总成
// 一个数会掩盖「A 号已经欠费、B 号还满着」这种真正要紧的事实，而且故障
// 转移恰恰会悄悄从欠费的那把切到好的那把 —— 运维不知道哪把该充值了。
//
// # 为什么不加轮询/缓存
//
// 余额是**钱**。一个自动刷新、可能显示过期值的余额数字，比「查一下」更危险：
// 界面上的「还剩 300 元」若来自 10 分钟前的缓存，而管理员正要据此决定充值
// 金额，看到的是假数据。因此这里只做显式点按查询（POST），绝不后台轮询，
// 响应里也带上取数时刻让界面能说清这个数字有多旧。

// balanceTimeout 是单次余额查询的上限。
//
// 取 15s 而不复用 provider 的 upstream_timeout_ms（默认 120s）：余额端点
// 是廉价的账户查询，不是生成请求，没有任何理由等两分钟；而管理页在等两分钟
// 之后看到的一定是超时 —— 那比早点失败更难排查。15s 足以覆盖慢 CDN 与
// 跨洲链路，又能让「点了没反应」在有生之年发生。
const balanceTimeout = 15 * time.Second

// CredentialBalance 是**一把凭据**在上游侧的余额结果。
type CredentialBalance struct {
	CredentialID string `json:"credential_id"`
	Label        string `json:"label"`
	Status       string `json:"status"` // "ok" | "error" | "unsupported"
	Message      string `json:"message,omitempty"`
	// Amount 是上游报告的可用额度，单位与 Currency 一致（未换算成元 ——
	// 上游可能报美元，汇率随时在动，网关编一个换算值等于撒谎）。
	Amount   float64 `json:"amount,omitempty"`
	Currency string  `json:"currency,omitempty"`
	// Detail 是上游附带的补充说明（总额度、已用、到期日…），原样带出：
	// 各家的余额响应里常有「本周重置」这类运维必须看到的信息，
	// 网关不认识就丢掉会让管理员错过关键事实。
	Detail string `json:"detail,omitempty"`
	// FetchedAt 是本次取数的毫秒时间戳。界面必须拿它说明数字的时效 ——
	// 余额是**点按查询**的结果，不是后台轮询的快照。
	FetchedAt int64 `json:"fetched_at,omitempty"`
}

// ProviderBalance 是一个上游下全部凭据的查询结果。
type ProviderBalance struct {
	ProviderID string              `json:"provider_id"`
	Provider   string              `json:"provider"`
	Status     string              `json:"status"` // "ok" | "partial" | "error" | "unsupported"
	Message    string              `json:"message,omitempty"`
	Results    []CredentialBalance `json:"results"`
}

// QueryProviderBalance 逐条凭据查询上游余额。
//
// 逐条而不是并发或批量，有三个理由：
//   - 凭据本身可能正被数据面使用，并发打同一个上游是在给生产流量添乱；
//   - 余额查询失败不该影响其它凭据的结果 —— 逐条执行天然做到「一把查不到、
//     另一把照常显示」，而这恰恰是最常见的情形（过期的那把）；
//   - 返回顺序与凭据列表一致，界面上不会看到随机的行序。
func QueryProviderBalance(ctx context.Context, st *store.Store, providerID string, masterKey []byte) (*ProviderBalance, error) {
	p, err := st.GetProvider(ctx, providerID)
	if err != nil {
		return nil, err
	}
	if p == nil {
		return nil, fmt.Errorf("provider not found")
	}
	creds, err := st.ListCredentials(ctx, providerID)
	if err != nil {
		return nil, err
	}

	out := &ProviderBalance{
		ProviderID: p.ID,
		Provider:   p.Name,
		Results:    make([]CredentialBalance, 0, len(creds)),
	}

	ok, failed, unsupported := 0, 0, 0
	for _, c := range creds {
		res := CredentialBalance{
			CredentialID: c.ID,
			Label:        c.Label,
			Status:       "error",
		}
		// 停用的凭据不查：它的余额与本次排障无关，而查它要占用上游连接。
		// 但状态必须回传，否则界面上那一行会「凭空消失」，看起来像被删了。
		if !c.Enabled {
			res.Status = "unsupported"
			res.Message = "凭据已停用，未查询"
			unsupported++
			out.Results = append(out.Results, res)
			continue
		}
		key, derr := decryptCredentialKey(c.APIKeyEnc, masterKey)
		if derr != nil {
			res.Message = "凭据解密失败：" + derr.Error()
			failed++
			out.Results = append(out.Results, res)
			continue
		}
		amount, currency, detail, perr := fetchCredentialBalance(ctx, p, key)
		if perr != nil {
			res.Message = perr.Error()
			if isUnsupportedBalanceErr(perr) {
				res.Status = "unsupported"
				unsupported++
			} else {
				failed++
			}
			out.Results = append(out.Results, res)
			continue
		}
		res.Status = "ok"
		res.Amount = amount
		res.Currency = currency
		res.Detail = detail
		res.FetchedAt = time.Now().UnixMilli()
		ok++
		out.Results = append(out.Results, res)
	}

	switch {
	case ok == 0 && unsupported == len(out.Results) && unsupported > 0:
		out.Status = "unsupported"
	case ok == 0 && failed > 0:
		out.Status = "error"
	case failed > 0 || unsupported > 0:
		out.Status = "partial"
	default:
		out.Status = "ok"
	}
	return out, nil
}

// unsupportedBalanceErr 标记「这个上游没有可用的余额查询方式」。
// 它与「查了但失败」（401/超时）语义完全不同：前者是配置问题（换 provider
// 或改端点），后者是凭据或网络问题 —— 界面必须分开说，否则管理员会去换 key
// 而真正该改的是 endpoint。
type unsupportedBalanceErr struct{ msg string }

func (e *unsupportedBalanceErr) Error() string { return e.msg }

func isUnsupportedBalanceErr(err error) bool {
	var e *unsupportedBalanceErr
	return errorsAs(err, &e)
}

// fetchCredentialBalance 按 provider 的协议与端点形状选择余额查询方式。
func fetchCredentialBalance(ctx context.Context, p *store.Provider, apiKey string) (amount float64, currency, detail string, err error) {
	return balanceDialect()(ctx, p, apiKey)
}

// balanceDialect 是「这个上游该怎么查余额」的判定。
//
// 它是**变量**而不是函数，为的是测试能指向 httptest 的本地地址：真实判定
// 依赖端点主机名（api.openai.com / api.deepseek.com），而测试服务器只能用
// 127.0.0.1，否则就只能去打真实上游（要钱、且不稳定）。生产路径下它永远是
// detectBalanceDialect —— 唯一的写入点是测试的 init/cleanup。
//
// 之所以按端点特征自动判定、而不是给 provider 加一个 dialect 配置列：
// 余额是**只读的辅助信息**，为它加一列就多一个需要有人维护的配置项，而
// 绝大多数 provider 压根不需要它（不支持的那些如实报 unsupported 即可）。
// 自动判定让「新建上游」这个动作不需要附带任何新知识。
var balanceDialect = detectBalanceDialect

// detectBalanceDialect 按「先看端点特征、再回落协议」的顺序选择查询方式。
//
// 端点特征优先于协议：同一个 openai-chat 协议下，api.openai.com 有 billing
// 端点而绝大多数中转站没有。若按协议先行，每个中转站都会被发一次注定 404
// 的请求，而「这个上游不支持」与「这个 key 欠费了」在上游侧返回的东西
// 无法区分。
func detectBalanceDialect() func(context.Context, *store.Provider, string) (float64, string, string, error) {
	return func(ctx context.Context, p *store.Provider, apiKey string) (float64, string, string, error) {
		base := strings.TrimRight(p.Endpoint, "/")
		switch {
		case strings.Contains(base, "api.openai.com"):
			return fetchOpenAIBalance(ctx, base, apiKey)
		case strings.Contains(base, "api.deepseek.com"):
			return fetchDeepSeekBalance(ctx, base, apiKey)
		case p.Protocol == "anthropic":
			// Anthropic 至今没有公开的余额查询 API（组织用量要走管理后台/
			// 合同）。如实说「不支持」而不是去猜一个端点打过去：猜出来的 404
			// 与真正的 404 无法区分，会把管理员引向错误的排查方向。
			return 0, "", "", &unsupportedBalanceErr{
				msg: "Anthropic 未提供公开的余额查询接口，额度请在其管理后台查看",
			}
		default:
			return 0, "", "", &unsupportedBalanceErr{
				msg: "该上游没有已知的余额查询方式（内置支持 OpenAI 官方与 DeepSeek）",
			}
		}
	}
}

// balanceHTTPClient 与 DiscoverUpstreamModels 同一纪律：不跟随重定向。
//
// 理由相同且更重要 —— 这里带着 API key。一次 302 就能把 key 与账户余额信息
// 送到管理员填的 endpoint 之外的主机上。ErrUseLastResponse 让 3xx 原样返回，
// 由调用方按状态码报错。
var balanceHTTPClient = &http.Client{
	Timeout: balanceTimeout,
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

// doBalanceGET 发一次带凭据的 GET 并返回响应体。
//
// 非 200 的响应体**只进日志、不进 error**（与 fetchModels 同一纪律）：
// 这些端点的错误页里常有内部主机名、路径、框架版本，而本错误经管理 API
// 原样回显给管理员 —— 而管理员本来就有权配任意 endpoint，风险不在这里，
// 但把上游的内部细节摊给终端用户没有任何好处。
func doBalanceGET(ctx context.Context, url, apiKey string, anthropic bool) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if anthropic {
		req.Header.Set("x-api-key", apiKey)
		req.Header.Set("anthropic-version", "2023-06-01")
	} else {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	resp, err := balanceHTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		preview, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		slog.Warn("upstream balance query returned non-200",
			"url", url, "status", resp.StatusCode,
			"body_preview", strings.TrimSpace(string(preview)))
		if resp.StatusCode == http.StatusNotFound {
			return nil, &unsupportedBalanceErr{msg: fmt.Sprintf("该上游没有余额查询端点（%s 返回 404）", url)}
		}
		return nil, fmt.Errorf("上游返回 HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 64*1024))
}

// fetchOpenAIBalance 取 OpenAI 官方账户的额度上限与已用额，给出**可用**额度。
//
// 取「总额度 − 已用」而不是只报总额度：管理员的问题是「还能花多少」，
// 报一个更大的数字会让他以为比实际宽裕。优先用 hard_limit_usd，退回
// hard_limit（早期字段没有货币单位）；两者都没有就报错而不是编一个值。
func fetchOpenAIBalance(ctx context.Context, base, apiKey string) (float64, string, string, error) {
	body, err := doBalanceGET(ctx, base+"/dashboard/billing/subscription", apiKey, false)
	if err != nil {
		return 0, "", "", err
	}
	var sub struct {
		HardLimitUSD *float64 `json:"hard_limit_usd"`
		HardLimit    *float64 `json:"hard_limit"`
		SoftLimitUSD *float64 `json:"soft_limit_usd"`
	}
	if err := json.Unmarshal(body, &sub); err != nil {
		return 0, "", "", fmt.Errorf("解析 OpenAI 订阅信息失败：%w", err)
	}

	limit := sub.HardLimitUSD
	if limit == nil {
		limit = sub.HardLimit
	}
	if limit == nil {
		return 0, "", "", &unsupportedBalanceErr{
			msg: "OpenAI 订阅响应里没有额度上限字段（该 key 可能不是标准 API 计费账号）",
		}
	}

	// 已用额是**当月**的，7 月订阅的余额不会在 8 月被扣 —— 所以拿它算
	// 「可用」在跨月时会偏大。detail 里把两个原始值都带出，让管理员自己
	// 判断，界面上不该出现一个看起来精确、实际有口径问题的可用余额。
	used := 0.0
	if b, err := doBalanceGET(ctx, base+"/dashboard/billing/usage", apiKey, false); err == nil {
		var usage struct {
			TotalUsage *float64 `json:"total_usage"`
		}
		if json.Unmarshal(b, &usage) == nil && usage.TotalUsage != nil {
			used = *usage.TotalUsage
		}
	}

	detail := fmt.Sprintf("总额度 %.2f USD，本月已用 %.2f USD", *limit, used)
	return *limit - used, "USD", detail, nil
}

// fetchDeepSeekBalance 走 DeepSeek 公开的 /user/balance。
//
// 该端点直接给 is_available 标志，而余额数值可能按老接口形态放在别的键上，
// 两种都试：取不到数值时按 is_available=false 报「已无可用余额」，
// 而不是返回「查不到」—— 前者是真实答案，后者是网关的失败，混为一谈会让
// 管理员以为要换 key 而其实只是该充值了。
func fetchDeepSeekBalance(ctx context.Context, base, apiKey string) (float64, string, string, error) {
	body, err := doBalanceGET(ctx, base+"/user/balance", apiKey, false)
	if err != nil {
		return 0, "", "", err
	}
	var payload struct {
		IsAvailable *bool `json:"is_available"`
		Balance     any   `json:"balance_infos"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return 0, "", "", fmt.Errorf("解析 DeepSeek 余额失败：%w", err)
	}
	if payload.Balance == nil {
		return 0, "", "", &unsupportedBalanceErr{msg: "DeepSeek 余额响应里没有 balance_infos 字段"}
	}

	amount, ok := firstNumericIn(payload.Balance)
	if !ok {
		return 0, "", "", fmt.Errorf("无法从 balance_infos 中读出余额数值")
	}

	detail := ""
	if payload.IsAvailable != nil && !*payload.IsAvailable {
		// is_available=false 是**上游明确说的话**，不能被数值盖过去。
		detail = "上游标记该账号余额不足/不可用"
		amount = 0
	}
	return amount, "CNY", detail, nil
}

// firstNumericIn 从 balance_infos 里取第一个可解析的数值。
//
// DeepSeek 的 balance_infos 是「币种名 → 金额」的映射，而币种名随账号而变
// （CNY / USD 都见过），所以不能按固定键读，只能取「第一个数值」。
// 多币种时这个取法是有偏的（不保证取到用户真正在花的那种），因此 balance
// 信息只在单一数值时被采信 —— 拿不准时宁可说「读不出」也不给一个可能指错
// 币种的数字，那比没有数字更糟。
func firstNumericIn(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case map[string]any:
		if len(t) != 1 {
			return 0, false
		}
		for _, inner := range t {
			return firstNumericIn(inner)
		}
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(t), 64)
		if err == nil {
			return f, true
		}
	}
	return 0, false
}

// errorsAs 是 errors.As 的薄封装（避免在文件顶部多一个与本文件无关的 import
// 出现在 helper 区域）。单独一层也让 unsupportedBalanceErr 的判定只有一处。
func errorsAs(err error, target **unsupportedBalanceErr) bool {
	for err != nil {
		if e, ok := err.(*unsupportedBalanceErr); ok {
			*target = e
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}
