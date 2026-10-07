package outwire

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

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
// 规则见 DESIGN §10：401/403→30min、402→1h、429→60s、408/5xx→60s、传输错误→60s。
// 429 本应按 Retry-After，但 rosetta v0.5.1 未把该 header 暴露到 APIError 上
// （重试退避在 SDK 内部完成），故取 DESIGN 的 60s 上限作为保守值。
func CredentialCooldown(err error) time.Duration {
	var transportErr *rosetta.TransportError
	if errors.As(err, &transportErr) {
		return 60 * time.Second
	}
	var apiErr *rosetta.APIError
	if errors.As(err, &apiErr) {
		switch {
		case apiErr.StatusCode == http.StatusUnauthorized, apiErr.StatusCode == http.StatusForbidden:
			return 30 * time.Minute
		case apiErr.StatusCode == http.StatusPaymentRequired:
			return time.Hour
		case apiErr.StatusCode == http.StatusTooManyRequests,
			apiErr.StatusCode == http.StatusRequestTimeout,
			apiErr.StatusCode >= 500:
			return 60 * time.Second
		}
	}
	return 0
}
