package outwire

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/cn-maul/rosetta"
)

type ErrorBody struct {
	Error *ErrorDetail `json:"error,omitempty"`
}

type ErrorDetail struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code,omitempty"`
	Param   string `json:"param,omitempty"`
}

func MapUpstreamError(err error) (int, string, string) {
	if err == nil {
		return http.StatusOK, "", ""
	}

	if errors.Is(err, rosetta.ErrContextTooLong) {
		return http.StatusBadRequest, "context_length_exceeded", "request exceeds model context window"
	}
	if errors.Is(err, rosetta.ErrInvalidRequest) {
		return http.StatusBadRequest, "invalid_request_error", err.Error()
	}
	if errors.Is(err, rosetta.ErrStreamTruncated) {
		// Partial content is already on the wire; there is no error body to
		// add on top of it.
		return http.StatusOK, "", ""
	}
	if errors.Is(err, rosetta.ErrStreamOverflow) {
		// The SDK's accumulation guard tripped: same shape as truncation —
		// whatever was produced is already delivered, nothing to append.
		return http.StatusOK, "", ""
	}
	// 上游返回了 HTTP 成功但响应体解不开（rosetta v1.0.1 新增的哨兵）。
	//
	// 2026-10-10 修复：此前 SDK 用裸 fmt.Errorf 包装解码失败，没有哨兵也没有
	// 类型，于是这一档全部落到下面的兜底 → 500 internal_error，且
	// FailoverEligible 同样不命中 → **故障转移链不会换到健康上游**。
	// 实测（真实进程 + 假上游）：链首返回 `200 + 半截 JSON` 时，健康上游被
	// 调用 0 次，客户端收到 500 —— 一个「接受请求但返回垃圾」的上游
	// （中转网关挂了、返回 HTML 错误页）永远不会被换掉，且 500 把排障方向
	// 指向网关自己，而坏的是上游。
	//
	// 归 502 upstream_error：故障转移与断路器把目标级故障算在**上游**账上，
	// 客户端 SDK 也能据此重试到别处。日志里记原始错误便于定位，但不回显给客户端
	// （响应文案是固定串，不含上游原文）。
	if errors.Is(err, rosetta.ErrUpstreamMalformed) {
		logUpstreamMalformed(err)
		return http.StatusBadGateway, "upstream_error", "upstream provider returned an unreadable response"
	}

	var apiErr *rosetta.APIError
	if errors.As(err, &apiErr) {
		switch {
		case apiErr.StatusCode == 401 || apiErr.StatusCode == 403:
			return http.StatusBadGateway, "upstream_auth_error", "upstream provider authentication failed"
		case apiErr.StatusCode == 402:
			return http.StatusBadGateway, "upstream_quota_exhausted", "upstream provider quota exhausted"
		case apiErr.StatusCode == 429:
			return http.StatusTooManyRequests, "rate_limit_exceeded", "rate limit exceeded"
		case apiErr.StatusCode == statusOverloaded:
			// 上游过载：Anthropic 的 529 有专门语义（overloaded_error），
			// 官方 SDK 对它做指数退避重试。压成 502 会让客户端退化成
			// 「未知错误」而不重试 —— 而过载恰恰是最该重试的场景。
			// 503 同理（Service Unavailable，也是可重试的过载信号）。
			logAPIError(apiErr)
			return apiErr.StatusCode, "upstream_error", "upstream provider overloaded"
		case apiErr.StatusCode >= 500:
			return http.StatusBadGateway, "upstream_error", "upstream provider error"
		case apiErr.StatusCode == 400:
			// 不回显上游原文：上游错误 message 里常带内部 URL、账号标识，
			// 有时还有对方的 prompt片段。且这些文本对下游定位自己的问题无用
			// —— 该做的是对照网关的路由配置。把原文放进日志而不是响应体。
			logAPIError(apiErr)
			return http.StatusBadRequest, "invalid_request_error", "upstream rejected the request"
		case apiErr.StatusCode == http.StatusNotFound, apiErr.StatusCode == http.StatusGone:
			// 上游说「没有这个模型 / 没有这个端点」。这是目标级的配置问题，
			// 不是网关内部故障 —— 旧实现落到 default 分支，把上游那句
			// "not found" 原样塞进 502 upstream_error 里，客户端看不出该去查
			// 自己的 model 名还是该去查网关的 provider 配置。
			logAPIError(apiErr)
			return http.StatusNotFound, "model_not_found", "upstream provider does not serve this model"
		default:
			logAPIError(apiErr)
			return http.StatusBadGateway, "upstream_error", "upstream provider error"
		}
	}

	var transportErr *rosetta.TransportError
	if errors.As(err, &transportErr) {
		// 同理：transport error 的原文含上游 endpoint 地址，不外泄。
		return http.StatusGatewayTimeout, "upstream_timeout", "upstream connection failed"
	}

	return http.StatusInternalServerError, "internal_error", "internal gateway error"
}

// logAPIError 把上游错误原文留在服务端日志里（响应体只给分类后的固定文案）。
func logAPIError(e *rosetta.APIError) {
	slog.Warn("upstream returned an error",
		"status", e.StatusCode, "type", e.Type, "message", e.Message)
}

// logUpstreamMalformed 记录「上游响应解不开」的原始错误。
//
// 刻意只记日志、**不回显**：响应文案是固定串，不含上游原文。
// 原始错误里带解码偏移（json.SyntaxError 的 Offset），排障时很有用。
func logUpstreamMalformed(err error) {
	slog.Warn("upstream response could not be decoded",
		"error", err, "hint", "upstream accepted the request but returned an unreadable body")
}

// UpstreamErrorDetail 从一次上游错误里提取「落库用」的失败详情。
//
// 与 MapUpstreamError 的分工：后者决定「响应什么给客户端」（固定文案，
// 不回显上游原文），本函数决定「把什么写进 usage_records.error_detail」
// 供运维在调用历史里排查。两条职责分开 —— 客户端看到的和运维查到的
// 不必是同一份文本，前者要克制，后者要够用。
//
// # 为什么 Message 与 Raw 都要截留
//
// Message 是上游那句人话（「额度用完」「429 limit」），排障时先看它；
// Raw 是完整响应体，里面有 type / code / param / request_id —— 这些字段
// 恰恰是「工具调用记录对不上」这类结构化 400 的关键。旧实现只在 Message
// 为空时才退回 Raw，于是最常见的「有 message 但还带一堆结构化字段」
// 错误只剩一句话，调用历史里看不到上游到底为何拒绝。
//
// 两段都保留，且用固定的分隔标题标明来源；前端据此分区展示，而不是把
// 一段原始 JSON 直接塞给运维猜。
//
// # 为什么必须自己脱敏
//
// SDK 只在 APIError.Error() 拼字符串时调 httpx.MaskSecrets（rosetta 内部
// 包，网关 import 不到），Message 字段本身是**原文未脱敏** —— 上游可能
// 把调用方的 key 回显在错误里。这里用与 SDK 同一份正则（secretRe 的
// 复制）做掩码，保证落库的文本不含明文凭据。
//
// # 截断到 8KB
//
// 上游错误体理论上可到 4KB（SDK 的 safeTruncateBody 上限）。usage_records
// 是明细表，失败率通常 <1%，但最坏情况（100% 失败）下 4KB × 日请求量
// 会让库体积失控。8KB 足以完整保留 SDK 的 Raw（4960 字节）与一段
// message，同时仍把单条记录的膨胀压在上限内。旧实现的 1KB 会从中间
// 截断结构化 400 的 JSON，正好丢掉 request_id / param 等排障字段。
func UpstreamErrorDetail(err error) string {
	if err == nil {
		return ""
	}
	var apiErr *rosetta.APIError
	if errors.As(err, &apiErr) {
		var parts []string
		if msg := strings.TrimSpace(apiErr.Message); msg != "" {
			parts = append(parts, "message: "+msg)
		}
		if raw := strings.TrimSpace(string(apiErr.Raw)); raw != "" && raw != strings.TrimSpace(apiErr.Message) {
			// Raw 是原始响应体（json.RawMessage）。SDK 的 body redaction 已对
			// 凭据形状做过掩码，这里再过一次 maskSecrets 是纵深防御 ——
			// 落库文本绝不能含明文凭据。
			parts = append(parts, "raw: "+raw)
		}
		return truncateErrorDetail(maskSecrets(strings.Join(parts, "\n")))
	}
	// TransportError 没有上游正文，但它的底层 err 正是 DNS / TCP / TLS /
	// proxy / timeout 的现场。旧实现把这一类统一返回空串，调用历史只剩
	// "upstream_timeout"，无法判断是域名解析失败、拒绝连接、证书错误，还是
	// 代理不可达。这里只截留底层原因，不拼 TransportError.Error()（后者虽
	// 已由 SDK 对 URL 做过处理，但没有必要把端点写进明细表）。
	var transportErr *rosetta.TransportError
	if errors.As(err, &transportErr) {
		if transportErr.Err != nil {
			return truncateErrorDetail(maskSecrets("transport: " + transportErr.Err.Error()))
		}
		return truncateErrorDetail("transport: connection failed")
	}
	// Malformed / truncated / overflow and local stream errors carry useful
	// reason text but never the response body. Keep those reasons; otherwise
	// the UI would show only a short code for exactly the failures operators
	// most often need to diagnose.
	if errors.Is(err, rosetta.ErrUpstreamMalformed) ||
		errors.Is(err, rosetta.ErrStreamTruncated) ||
		errors.Is(err, rosetta.ErrStreamOverflow) {
		return truncateErrorDetail(maskSecrets(strings.TrimSpace(err.Error())))
	}
	return ""
}

// SafeErrorDetail returns a masked, truncated description for an arbitrary
// local or downstream error. It is used for failures that have no upstream
// response body (for example a failed write to the caller) but whose concrete
// OS/network reason is still the most useful diagnostic. Never use it for a
// client-facing response: the text is for logs and the usage detail drawer.
func SafeErrorDetail(err error) string {
	if err == nil {
		return ""
	}
	return truncateErrorDetail(maskSecrets(strings.TrimSpace(err.Error())))
}

func truncateErrorDetail(detail string) string {
	const maxDetailBytes = 8 << 10
	if len(detail) > maxDetailBytes {
		// 按 rune 边界截：上游（DeepSeek / 月之暗面等）的报错常是中文，
		// 按字节截会把最后一个汉字劈成残字节 —— 存进库再经 JSON 编码时
		// 会变成 replacement char，界面上是「乱码尾巴」。宁可少截一个字。
		end := maxDetailBytes
		for end > 0 && !utf8.RuneStart(detail[end]) {
			end--
		}
		detail = detail[:end]
	}
	return detail
}

// maskSecrets 是 rosetta 内部 httpx.MaskSecrets 的复制（同一份正则）。
//
// 为什么复制而不是 import：httpx 是 rosetta 的 internal 包，网关 import
// 不到。正则必须与 SDK 保持一致 —— 若两边漂移，网关落库的文本可能
// 漏掉 SDK 已经掩掉的某种凭据形状。这份正则覆盖 API key / JWT / AWS /
// GitHub / Slack token 等常见形状。
var secretRe = regexp.MustCompile(`sk-[A-Za-z0-9_-]{8,}|AIza[0-9A-Za-z_-]{20,}|AKIA[0-9A-Z]{16}|gh[pousr]_[A-Za-z0-9]{16,}|xox[baprs]-[A-Za-z0-9-]{10,}|eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{4,}\.[A-Za-z0-9_-]{4,}`)

func maskSecrets(s string) string { return secretRe.ReplaceAllString(s, "***") }

func WriteOpenAIError(w http.ResponseWriter, statusCode int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	json.NewEncoder(w).Encode(ErrorBody{
		Error: &ErrorDetail{
			Message: message,
			Type:    errorTypeFromCode(code),
			Code:    code,
		},
	})
}

// errorTypeFromCode 把网关内部语义 code 翻译成 OpenAI 的 error.type。
//
// 关键诉求是**让客户端能按类型决策重试/退避**。旧实现只有 5 个分支，
// 其余（含 upstream_* 一族）全塌成 api_error —— 而塌掉的恰恰是客户端
// 最需要区分的四类：鉴权失败（换 key）、额度耗尽（充值/换账号）、
// 超时（可重试）、上游过载（应退避重试）。收到 api_error 的 SDK 通常
// 直接放弃重试。
//
// 注意 HTTP 状态码与 code 字段始终保留，按这两者判断的客户端不受影响；
// 受影响的只是按 type 判断的那部分。
func errorTypeFromCode(code string) string {
	switch code {
	case "model_not_found":
		return "invalid_request_error"
	case "invalid_api_key":
		return "authentication_error"
	case "rate_limit_exceeded":
		return "rate_limit_error"
	case "context_length_exceeded":
		return "invalid_request_error"
	case "invalid_request_error":
		return "invalid_request_error"
	case "request_too_large":
		// 413 属于请求本身过大，重试无用但也不该报成 api_error ——
		// 客户端据此可以主动裁剪输入再试。
		return "invalid_request_error"
	case "insufficient_quota":
		// 与 rate_limit 区分：额度耗尽要充值/换账号，退避没有意义。
		return "insufficient_quota"
	case "insufficient_balance":
		// 余额耗尽（402）。与 insufficient_quota 刻意分开：那是**token 配额**
		// （终身累计额度），这一条是**人民币余额**（可充值）。两者的处置动作
		// 不同（换 key vs 充值），塌成一类会让客户端给出错误建议。
		// 沿用 insufficient_quota 这个 type 名而不是新造：OpenAI SDK 只认得
		// 有限几种 type，而「额度不足，去充值」这个语义两者一致。
		return "insufficient_quota"
	case "upstream_auth_error":
		// 上游 401/403 —— 目标级凭据问题，故障转移链有机会换一把 key。
		// 报成 api_error 会让客户端以为是自己请求的问题。
		return "authentication_error"
	case "upstream_quota_exhausted":
		return "insufficient_quota"
	case "upstream_timeout":
		// 上游超时是可重试的瞬时故障，客户端应该退避后重发。
		return "timeout_error"
	case "upstream_error":
		// 上游 5xx。多为过载/暂时不可用，可重试。
		return "api_error"
	case "admin_cannot_call_model":
		// 管理员账号调用模型（2026-10 控制面/数据面分离）。
		//
		// # 为什么必须是 permission_error 而不是 authentication_error
		//
		// 这正是本文件整张表的**核心诉求**（见函数头注释：让客户端能按类型
		// 决策重试/退避）。authentication_error 的标准处置是「换一把 key /
		// 重新认证」，而这里 key 和账号**都是好的**、只是角色不允许 ——
		// 客户端换一万把管理员的 key 也一样不通，只会无限重试并刷满日志。
		//
		// permission_error 表达的是「终态的权限边界，不要重试，请改权限」。
		// 正确动作是人去新建一个普通用户、用那个账号的 key 调模型 —— 恰好
		// 就是本条错误消息告诉他的那句话。
		return "permission_error"
	default:
		return "api_error"
	}
}

// FailoverEligible 报告一次上游失败是否「值得换一个链上目标再试」。
//
// 判定集合（2026-09 与需求确认，2026-09-24 纳入 404/410）：
// 5xx、401/403（鉴权）、402（额度）、429（限流）、404/410（目标没有这个模型）、
// 408/传输层超时。400 一类（非法请求、上下文超限）不在列 —— 换上游既救不回来，
// 还会把同一份坏请求往链上每个目标各打一次，白白烧配额并放大延迟。
// 流式截断/溢出的错误体（ErrStreamTruncated/Overflow）也不转移：内容已经写给下游，
// 回退不了，只能如实按 truncated 收尾。
//
// 为什么 404/410 要转移（2026-09-24 e2e 实测暴露的缺口）：上游 404 的语义是
// 「我这个提供商没有这个模型」，属于**目标级**配置问题，而链正是为吸收目标级
// 故障存在的。最现实的场景是上游退役模型 —— 链首那个模型被下架后若不算可转移，
// 整条链会永久硬失败，故障转移在最需要它的场景里恰好是失效的。
// 注意「这次失败不该罚凭据」：404 说明 key 是好的，只是目标配错了，所以
// CredentialCooldown(404) 必须是 0，让目标级熔断（RecordTargetFailure）去记账。
func FailoverEligible(err error) bool {
	if err == nil {
		return false
	}
	// 上游响应体解不开（rosetta v1.0.1 新增）→ **可转移**。
	//
	// 与上面注释的同一个理由：这是目标级故障，上游「接受请求却返回垃圾」
	// （中转网关挂了、返回 HTML 错误页、body 被截断），链正是为吸收这类
	// 故障而存在的。修复前它落到文件末尾的 `return false`，于是链直接 break，
	// 健康目标一个都不试。
	//
	// 注意这个哨兵**只在一元响应上出现**——SDK 侧刻意没给流式事件解析打这个
	// 哨兵（见 rosetta 的 upstream_malformed_test.go 注释）：流式解不开时先前
	// 事件可能已交付，此时重试会重放已交付的输出并可能重复计费。
	// 所以「流式解不开」走的是流哨兵（ErrStreamTruncated/Overflow）那条路，
	// 那条路按设计也不可转移。两者不冲突。
	if errors.Is(err, rosetta.ErrUpstreamMalformed) {
		return true
	}
	var transportErr *rosetta.TransportError
	if errors.As(err, &transportErr) {
		return true
	}
	var apiErr *rosetta.APIError
	if errors.As(err, &apiErr) {
		switch {
		case apiErr.StatusCode == http.StatusUnauthorized,
			apiErr.StatusCode == http.StatusForbidden,
			apiErr.StatusCode == http.StatusNotFound,
			apiErr.StatusCode == http.StatusGone,
			apiErr.StatusCode == http.StatusPaymentRequired,
			apiErr.StatusCode == http.StatusRequestTimeout,
			apiErr.StatusCode == http.StatusTooManyRequests:
			return true
		case apiErr.StatusCode >= 500:
			return true
		}
	}
	return false
}

// CredentialCooldown 返回产出该错误的凭据应被踢出轮换多久；0 表示「不冷却」。
//
// 只有能归因到**这把凭据本身**的状态才冷却：401/403（key 无效/无权限）、
// 402（账号额度耗尽）、429（账号级限流）。
//
// 408 / 5xx / 传输错误 / 超时一律返回 0：它们是目标或链路故障，由
// RecordTargetFailure 的目标熔断负责。旧实现把这几种也冷却 60s，对单 key
// provider 的后果是：链首抖动一次就把唯一一把 key 踢出轮换，故障转移到达
// 下一跳时若那家也在冷却，整条链会直接返回 502，而且冷却期内没有任何请求
// 能成功以触发 RecordCredentialSuccess 复苏。
//
// 429 本应按 Retry-After，但 rosetta v0.5.1 未把该 header 暴露到 APIError 上
// （重试退避在 SDK 内部完成），故取 DESIGN 的 60s 上限作为保守值。
func CredentialCooldown(err error) time.Duration {
	var apiErr *rosetta.APIError
	if errors.As(err, &apiErr) {
		switch {
		case apiErr.StatusCode == http.StatusUnauthorized, apiErr.StatusCode == http.StatusForbidden:
			return 30 * time.Minute
		case apiErr.StatusCode == http.StatusPaymentRequired:
			return time.Hour
		case apiErr.StatusCode == http.StatusTooManyRequests:
			return 60 * time.Second
		}
	}
	return 0
}
