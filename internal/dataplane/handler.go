package dataplane

import (
	"context"
	crand "crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cn-maul/rosetta"
	"github.com/cn-maul/rosetta-gateway/internal/auth"
	"github.com/cn-maul/rosetta-gateway/internal/authhttp"
	"github.com/cn-maul/rosetta-gateway/internal/config"
	"github.com/cn-maul/rosetta-gateway/internal/httpx"
	"github.com/cn-maul/rosetta-gateway/internal/ingress"
	"github.com/cn-maul/rosetta-gateway/internal/outwire"
	wireproto "github.com/cn-maul/rosetta-gateway/internal/protocol"
	"github.com/cn-maul/rosetta-gateway/internal/ratelimit"
	"github.com/cn-maul/rosetta-gateway/internal/routing"
	"github.com/cn-maul/rosetta-gateway/internal/snapshot"
	"github.com/cn-maul/rosetta-gateway/internal/store"
	"github.com/cn-maul/rosetta-gateway/internal/upstream"
	"github.com/cn-maul/rosetta-gateway/internal/usage"
)

// SnapshotProvider returns the current immutable runtime snapshot.
type SnapshotProvider func() *snapshot.Snapshot

// Deps contains the data plane's explicit runtime dependencies. Snapshot is an
// injected provider so the handler does not reach into the package global; a
// nil provider defaults to snapshot.Get.
type Deps struct {
	Pool                 *upstream.Pool
	Config               *config.Config
	Usage                *usage.Recorder
	Limiter              *ratelimit.Limiter
	Snapshot             SnapshotProvider
	MaxConcurrentStreams int
	// StreamAdmissionTimeout 是「等待流式并发名额」的最长时间。
	//
	// 0 = 不等待（拿到名额就拿，拿不到立刻 503）。
	// 负值 = 用默认值。
	StreamAdmissionTimeout time.Duration
}

// Handler serves protocol-independent model requests through the configured
// upstream failover chain.
type Handler struct {
	pool     *upstream.Pool
	cfg      *config.Config
	usage    *usage.Recorder
	limiter  *ratelimit.Limiter
	snapshot SnapshotProvider
	db       *store.Store
	logger   *slog.Logger
	streams  chan struct{}
	// admissionTimeout 是等待流式并发名额的上限（见 defaultStreamAdmissionTimeout）。
	admissionTimeout time.Duration
}

const defaultMaxConcurrentStreams = 256

// defaultStreamAdmissionTimeout 是等待流式并发名额的默认上限。
//
// # 为什么需要它（2026-10-22 修 §7.1-3）
//
// 原来的并发上限是**纯阻塞**：拿不到名额就一直等，直到客户端断开。
// 过载时请求会堆在网关里 —— 每个都占着一个 goroutine、一个连接、
// 以及一份已鉴权但尚未结算的配额预占。堆到一定程度，网关对**所有**
// 请求都变慢（不只是流式），而客户端看到的只是「越来越慢」，
// 没有一个明确的可重试信号。
//
// 加了等待上限之后，过载会变成**快速失败**：等待超过上限就回 503 +
// Retry-After，客户端可以立刻重试或降级，而网关自己不会被拖垮。
//
// 5 秒是刻意选的：足够覆盖一次正常的名额周转（长流结束会立刻释放），
// 又短到「客户端以为自己卡死了」之前。
const defaultStreamAdmissionTimeout = 5 * time.Second

// NewHandler builds a data plane handler from explicit dependencies.
func NewHandler(d Deps) *Handler {
	if d.Snapshot == nil {
		d.Snapshot = snapshot.Get
	}
	db := d.Usage.Store()
	logger := d.Usage.Logger()
	streamLimit := d.MaxConcurrentStreams
	if streamLimit <= 0 {
		streamLimit = defaultMaxConcurrentStreams
		if d.Config != nil && d.Config.Defaults.MaxConcurrentStreams > 0 {
			streamLimit = d.Config.Defaults.MaxConcurrentStreams
		}
	}
	admissionTimeout := d.StreamAdmissionTimeout
	if admissionTimeout <= 0 {
		admissionTimeout = defaultStreamAdmissionTimeout
		if d.Config != nil && d.Config.Defaults.StreamAdmissionTimeoutMs > 0 {
			admissionTimeout = time.Duration(d.Config.Defaults.StreamAdmissionTimeoutMs) * time.Millisecond
		}
	}
	return &Handler{
		pool: d.Pool, cfg: d.Config, usage: d.Usage, limiter: d.Limiter,
		snapshot: d.Snapshot, db: db, logger: logger,
		streams:          make(chan struct{}, streamLimit),
		admissionTimeout: admissionTimeout,
	}
}

func newID() string {
	b := make([]byte, 16)
	crand.Read(b)
	return hex.EncodeToString(b)
}

// setRetryAfter 写 Retry-After 响应头（向上取整秒，至少 1）。
//
// 必须向上取整而不是截断：windowRemaining 已经带了 +1s 余量，若这里再用
// int(d.Seconds()) 截断，余量会被吃掉大半（剩 0.9s 时算成 1s，客户端 1s 后
// 重试仍在原窗口内，立刻又被拒 —— 正是这个函数要避免的抖动）。
func setRetryAfter(w http.ResponseWriter, d time.Duration) {
	seconds := int(math.Ceil(d.Seconds()))
	if seconds < 1 {
		seconds = 1
	}
	w.Header().Set("Retry-After", strconv.Itoa(seconds))
}

// sessionHeaders are the downstream headers that identify one conversation.
//
// The order is intentional. An explicit OpenCode header wins because it is the
// client's direct instruction to the upstream. DeepSeek Harness has its own
// header on the native Messages path; its OpenAI-compatible adapters instead
// send OpenAI-style affinity headers. The generic names cover other clients
// that use the same conventions. Do not include an installation-wide user ID
// here: sharing one cache affinity across every conversation is not equivalent
// to preserving one conversation's affinity.
var sessionHeaders = []string{
	upstream.OpenCodeSessionHeader,
	"x-deepseek-harness-session-id",
	"x-session-affinity",
	"session_id",
	"x-session-id",
	"session-id",
	// x-client-request-id is commonly unique per HTTP attempt, not per
	// conversation. It remains a last-resort compatibility alias, but must
	// never outrank a stable session identifier.
	"x-client-request-id",
}

func conversationSessionID(r *http.Request) string {
	for _, name := range sessionHeaders {
		if value := strings.TrimSpace(r.Header.Get(name)); value != "" {
			return value
		}
	}
	return ""
}

// withOpenCodeSession carries the downstream conversation ID through the
// protocol adapters. When the client sends no conversation identifier, the
// per-request ID is the only safe fallback: it prevents OpenCode from
// rejecting the request, although only a client-supplied identifier can
// preserve routing/cache affinity across turns.
func withOpenCodeSession(r *http.Request) *http.Request {
	sessionID := conversationSessionID(r)
	if sessionID == "" {
		sessionID = httpx.RequestIDFromContext(r.Context())
	}
	if sessionID == "" {
		return r
	}
	return r.WithContext(upstream.WithOpenCodeSession(r.Context(), sessionID))
}

func (h *Handler) Handle(codec ingress.Codec) http.HandlerFunc {
	db, logger := h.db, h.logger
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		diag := newRequestDiagnostics(r)
		var decoded *ingress.Request
		diag.setRemote(httpx.ClientIPOf(r))
		diag.setRequestID(httpx.RequestIDFromContext(r.Context()))
		diag.setIngressName(codec.Name())
		obs := observeResponse(w)
		w = obs
		defer func() {
			diag.observeResponse(obs)
			if decoded != nil && !diag.hasAttempts() {
				// The successful path gets request shape from the attempt itself.
				// Build a second Rosetta view only for failures that never reach
				// an upstream attempt, so diagnostics do not tax every request.
				diag.setIngressRequest(decoded.BuildRosetta(), decoded.Stream)
			}
			if status, _, _ := diag.result(); status == "" {
				diag.setResult(outcomeUsageStatus(obs.statusCode()), "", "")
			}
			logger.Info("gateway request diagnostic", diag.attrs()...)
		}()

		authStart := diag.begin("auth")
		authCtx, err := auth.Authenticate(r)
		diag.end("auth", authStart)
		if err != nil {
			diag.setResult("error", "invalid_api_key", err.Error())
			authhttp.WriteAuthError(w, err, codec.WriteError)
			return
		}
		diag.setAuth(authCtx.UserID, authCtx.KeyID)
		r = withOpenCodeSession(r)

		// RPM 限速（DESIGN §11.4）：额度随鉴权从快照带出（0 = 不限）。
		// 被拒的请求**不**计数 —— 实现见 ratelimit.AllowRPM 的注释：
		// 若计数，客户端在窗口内疯狂重试会让计数只增不减，把整个窗口
		// 永久锁死（直到窗口翻转才恢复），一个配错 RPM 的客户端就能把
		// 自己彻底堵死。计数只记真正被放行的请求。
		//
		// （此前这里写的是「被拒的请求同样计数」，与实现相反。实现是对的，
		//  错的是注释——留着会诱导后人「修反」成count-on-reject。）
		if ok, retry := h.limiter.AllowRPM(authCtx.KeyID, authCtx.RPMLimit); !ok {
			diag.setResult("error", "rate_limit_exceeded", "")
			logger.Warn("rate limited (rpm)", "key_id", authCtx.KeyID,
				"retry_after", retry.String(),
				"request_id", httpx.RequestIDFromContext(r.Context()))
			setRetryAfter(w, retry)
			codec.WriteError(w, http.StatusTooManyRequests, "rate_limit_exceeded",
				"request rate limit exceeded for this API key")
			return
		}

		decodeStart := diag.begin("decode")
		ing, err := codec.Decode(r, int64(h.cfg.Defaults.MaxRequestBodyBytes))
		diag.end("decode", decodeStart)
		if err != nil {
			diag.setResult("error", "invalid_request_error", err.Error())
			// 超限时中间件的 MaxBytesReader 会返回 *http.MaxBytesError。
			// 旧实现把它包成 "read body: ..." 一并当 400 回，客户端看不出
			// 「是body太大」还是「body格式错」——两者要采取的行动完全不同。
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				codec.WriteError(w, http.StatusRequestEntityTooLarge, "request_too_large",
					fmt.Sprintf("request body exceeds %d bytes", tooLarge.Limit))
				return
			}
			codec.WriteError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
			return
		}
		decoded = ing
		diag.setPublicModel(ing.Model)
		if ing.Stream {
			admissionStart := diag.begin("admission")
			if !h.acquireStreamSlot(r.Context()) {
				diag.end("admission", admissionStart)
				// 客户端已断开：不必再写任何响应。
				if r.Context().Err() != nil {
					return
				}
				// 过载：并发流已满，且在等待上限内没有等到名额。
				//
				// 旧实现是无界阻塞（等客户端断开才放弃），过载时请求会在
				// 网关里堆积，每个都占着 goroutine、连接与一份已预占的配额 ——
				// 而且客户端只看到「越来越慢」，没有可重试的信号。
				//
				// 现在快速失败：503 + Retry-After。过载是**可重试**的，
				// 客户端拿到明确信号后可以立刻重试或降级；网关自己不会被
				// 一堆积的请求拖垮。
				//
				// 记 ERROR 而非 WARN：这是过载信号，运维需要看到它。
				logger.Error("stream admission timeout: too many concurrent streams",
					"limit", cap(h.streams),
					"admission_timeout_ms", h.admissionTimeout.Milliseconds(),
					"model", ing.Model,
					"request_id", httpx.RequestIDFromContext(r.Context()))
				setRetryAfter(w, h.admissionTimeout)
				codec.WriteError(w, http.StatusServiceUnavailable, "server_overloaded",
					"too many concurrent streaming requests; retry shortly")
				diag.setResult("error", "server_overloaded", "")
				return
			}
			diag.end("admission", admissionStart)
			defer func() { <-h.streams }()
		}

		// TPM 限速（DESIGN §11.4）：流式下真实 token 只有流结束才知道，
		// 所以按「请求前估算预占 + 请求后按真实 usage 校正」两段执行。
		// 被拒的请求不预占（没放行就不该消耗窗口额度），也不预扣任何东西。
		//
		// TPMLimit==0（未启用，也是主流配置）时**整段跳过**：ingress.EstimateRequestTokens
		// 会遍历全部消息与工具定义拼出一个与请求体同量级的字符串再估算，
		// 是一次纯浪费的全量分配；而它算出的 est 随后必然被丢弃。
		// 同一个请求在 attempt* 里还会再 BuildRosetta 一次，估算那次是多余的第 N 次。
		// 两种预占共用同一个 est 与同一个 RateCommit 收尾 —— 分开放两次
		// 估算/两次收尾只会让两边在某条路径上被漏掉一处（且配额那条漏掉是
		// **终身**偏差，不像 TPM 窗口会自愈）。
		//
		// est 的计算按需触发：TPMLimit==0 且 key 未配quota 时整个函数跳过，
		// 不做那次「遍历全部消息与工具定义拼出请求体同量级字符串」的估算。
		var rate *RateCommit
		var est int64
		needEstimate := authCtx.TPMLimit > 0
		// Route resolution must precede estimation: an omitted client cap
		// reserves against the largest output ceiling in the candidate chain.
		snap := h.snapshot()
		routingStart := diag.begin("routing")
		res, err := snap.Routes.Resolve(ing.Model)
		diag.end("routing", routingStart)
		if err != nil {
			codec.WriteError(w, http.StatusNotFound, "model_not_found", ModelNotFoundMessage(snap, ing.Model))
			diag.setResult("error", "model_not_found", "")
			return
		}
		route := res.Route
		outCeilings := make([]int, 0, len(res.Candidates))
		for _, cand := range res.Candidates {
			if cand.UpstreamModel != nil {
				outCeilings = append(outCeilings, cand.UpstreamModel.MaxOutputTokens)
			}
		}
		outputCeiling := ingress.RequestOutputCeiling(ing.BuildRosetta(), outCeilings)
		quotaStart := diag.begin("quota")
		if needEstimate {
			est = ingress.EstimateRequestTokens(ing.BuildRosetta(), outputCeiling)
			rate = NewRateCommit(h.limiter, authCtx.KeyID, authCtx.TPMLimit, 0, db, logger)
			rate.SetRequestID(httpx.RequestIDFromContext(r.Context()))
		}

		// 终身 token 配额**原子预占**（DESIGN §11.2）。
		//
		// 原实现是纯查询（GetKeyQuota）：读 used → 比较 → 放行。并发下多个
		// 请求读到同一个 used 并**全部通过** —— 注释说「容忍至多一个在途超发」，
		// 那只在串行时成立：剩余额度 1000 token 时，50 个并发请求各预估 200
		// token 会全部放行，超发数十倍。
		//
		// ReserveQuota 把「检查」与「占用」合并进同一条写事务，并发请求被
		// SQLite 写锁串行化，第二个进来时读到的 used+reserved 已含第一个的预占。
		// 预占落在独立的 reserved_tokens 列（不动 used_tokens —— 真实用量由
		// usage 落库触发器累加，混写会让一次请求被记两次），收尾时由
		// RateCommit 原额退回（Commit → ReleaseQuota）。
		//
		// 这里只借 GetKeyQuota 判「是否配置了额度」（quota>0，决定要不要做
		// 估算与预占），真正的额度判定（used+reserved+est<=quota）在
		// ReserveQuota 事务内做。2026-10-07 之前这里把第一个返回值（quota）
		// 丢弃、名叫 quota 的变量绑到的是第二个返回值（used），判定变成
		// 「已用量>0」—— 全新 key 的终身配额因此完全不生效（P0-1）。
		//
		// 为什么不套用 ratelimit 的窗口限速器：TPM 限**速率**（分钟窗口一翻
		// 自然释放），配额限**终身累计**（没有「窗口结束」）。两者语义不同，
		// 拿窗口限速器去限终身额度会在窗口翻转时凭空释放额度。
		if quota, _, _, qerr := db.GetKeyQuota(r.Context(), authCtx.KeyID); qerr != nil {
			// 查询抖动 fail-open：读池故障不该变成流量全拒。但记 ERROR——
			// fail-open 的代价是真超发，无声无息就查不到了。
			logger.Error("quota lookup failed (fail-open)", "error", qerr, "key_id", authCtx.KeyID)
		} else if quota > 0 {
			if !needEstimate {
				est = ingress.EstimateRequestTokens(ing.BuildRosetta(), outputCeiling)
			}
			reserved, rok, rerr := db.ReserveQuota(r.Context(), authCtx.KeyID, est)
			switch {
			case rerr != nil:
				logger.Error("quota reserve failed (fail-open)",
					"error", rerr, "key_id", authCtx.KeyID)
			case !rok:
				logger.Warn("quota exceeded", "key_id", authCtx.KeyID,
					"estimated_tokens", est,
					"request_id", httpx.RequestIDFromContext(r.Context()))
				// 拒绝前必须退回 TPM 预占（2026-10-21 修 P0-8）。
				//
				// 这条早退路径此前**漏了** rate.Commit(0)：key 级配额已满
				// 时直接 return，TPM 窗口里的 est 预占就永远留在那儿。
				// 配额已耗尽的 key 可以反复触发这个拒绝，每次留下一笔含
				// max_tokens 上界的 est —— 几十次就能吃光该 key 整分钟的
				// TPM 额度，于是「配额满」会级联成「TPM 也满」，而客户端
				// 只看到一个 429。
				//
				// 注意退的是 **TPM 窗口**，不是终身配额：终身配额根本没
				// 预占成功（rok=false 时 reserved=0），没有东西要退。
				rate.Commit(0)
				codec.WriteError(w, http.StatusTooManyRequests, "insufficient_quota",
					"this API key has exhausted its token quota")
				diag.end("quota", quotaStart)
				diag.setResult("error", "insufficient_quota", "")
				return
			default:
				if rate == nil {
					rate = NewRateCommit(h.limiter, authCtx.KeyID, 0, 0, db, logger)
					rate.SetRequestID(httpx.RequestIDFromContext(r.Context()))
				}
				rate.SetKeyQuotaReserved(reserved)
			}
		}

		// 用户级总额度**原子预占**（MULTIUSER.md §4.3：三级配额的最外层总闸）。
		//
		// # 为什么必须是预占而不是「查了再放」（2026-10-14 上线前审计修复）
		//
		// 修复前这里是裸读比较：SumUserUsedTokens → used >= quota 判定 → 放行。
		// used 只含**已落库**的真实用量，不含尚未终结的在途请求，而读-判-放行
		// 三步之间没有任何互斥。实测：配额 1000、50 并发各估 200 token，
		// 50 个请求**全部通过**（占用口径 10000 token，超发 10 倍）。
		// 原注释「并发下容忍至多一个在途请求超发」只在串行时成立。
		//
		// key 级早在 2026-10-07 就换成了 ReserveQuota 原子事务预占，用户级却
		// 没跟上 —— 两级配额一个原子一个不原子，恰好是这种「一半修好了」的
		// 形态最容易被当成「已经有限流」。
		//
		// ReserveUserQuota 把检查与占用合并进同一条写事务，并发请求被 SQLite
		// 写锁串行化，第二个进来时读到的 used+reserved 已含第一个的预占。
		//
		// 位置在 key 级之后：两级额度独立配置，**任意一级满都要拒绝**，
		// 谁先判不影响结果；放在一起共用同一个 est，少算一次估算。
		//
		// 只在用户配了额度时才走这一步：quota_tokens=0（不限额）是常态，
		// 为常态付一次写事务不划算（与 key 级「quota>0 才预占」同口径）。
		// 额度判断本身交给 ReserveUserQuota 事务内做，这里只借快照判「是否
		// 配置了额度」—— 与 key 级用 GetKeyQuota 判 quota>0 完全对称。
		if u := h.snapshot().UsersByID[authCtx.UserID]; u != nil && u.QuotaTokens > 0 {
			if !needEstimate {
				est = ingress.EstimateRequestTokens(ing.BuildRosetta(), outputCeiling)
			}
			reserved, rok, rerr := db.ReserveUserQuota(r.Context(), authCtx.UserID, est)
			switch {
			case rerr != nil:
				// 与 key 级同口径：查询抖动 fail-open，不因一次写失败拒绝正常
				// 流量。但记 ERROR —— fail-open 的代价是真超发，无声无息就
				// 查不到了。
				logger.Error("user quota reserve failed (fail-open)",
					"error", rerr, "user_id", authCtx.UserID)
			case !rok:
				logger.Warn("user quota exceeded", "user_id", authCtx.UserID,
					"estimated_tokens", est,
					"request_id", httpx.RequestIDFromContext(r.Context()))
				// 拒绝前必须退回 key 级预占：它已经在同一请求里立起来了，
				// 直接 return 会让这把 key 的额度被这次被拒的请求永久吃掉一块。
				rate.Commit(0)
				codec.WriteError(w, http.StatusTooManyRequests, "insufficient_quota",
					"this account has exhausted its token quota")
				diag.end("quota", quotaStart)
				diag.setResult("error", "insufficient_quota", "")
				return
			default:
				if rate == nil {
					rate = NewRateCommit(h.limiter, authCtx.KeyID, 0, 0, db, logger)
					rate.SetRequestID(httpx.RequestIDFromContext(r.Context()))
				}
				rate.SetUserQuotaReserved(authCtx.UserID, reserved)
			}
		}
		diag.end("quota", quotaStart)
		// TPM is reserved last among admission checks with the same estimate.
		// Quota exhaustion must remain visible as insufficient_quota rather
		// than being masked by a TPM rejection caused by that same request.
		if needEstimate {
			if ok, retry := h.limiter.ReserveTPM(authCtx.KeyID, authCtx.TPMLimit, est); !ok {
				rate.ReleaseQuota()
				logger.Warn("rate limited (tpm)", "key_id", authCtx.KeyID,
					"estimated_tokens", est, "retry_after", retry.String(),
					"request_id", httpx.RequestIDFromContext(r.Context()))
				setRetryAfter(w, retry)
				codec.WriteError(w, http.StatusTooManyRequests, "rate_limit_exceeded",
					"token rate limit exceeded for this API key")
				diag.setResult("error", "rate_limit_exceeded", "")
				return
			}
			rate.setTPMReserved(est)
		}

		// 模型白名单（多用户改造 P1）：组白名单 ∩ key 白名单，任一非空即生效。
		//
		// 判定用的 `route.PublicName` 有两个来源，必须都覆盖到：
		//   - 具名路由命中 → 就是该 route 的公共名；
		//   - `provider/model` 直连形式 → routing.Resolve 会**合成**一条 route，
		//     其 PublicName 就是请求原文。
		//
		// 也就是说这条判定等价于「按客户端实际写的那个标识符查白名单」。
		// 这正是我们要的：如果只按具名路由判定而放行直连形式，受限用户只要
		// 改写成 `provider/model` 就能拿到白名单之外的上游模型 —— 白名单形同虚设。
		// 管理员若确实要放开某个直连标识符，把它本身写进白名单即可
		// （/v1/models?include=upstream 会列出这些标识符供挑选）。
		//
		// 位置排在限速与配额预检之后、触碰上游之前：白名单判定只是内存里的
		// 线性扫描，比两次数据库预检便宜；放前面会让「超额度的请求」拿到 403
		// 而不是 429，掩盖更该先解决的配额问题。两者都不打上游，
		// 顺序只影响错误码优先级。
		if !authCtx.AllowsModel(route.PublicName) {
			rate.Commit(0)
			diag.setResult("error", "model_not_allowed", "")
			logger.Warn("model not allowed for caller",
				"model", route.PublicName, "key_id", authCtx.KeyID, "user_id", authCtx.UserID,
				"request_id", httpx.RequestIDFromContext(r.Context()))
			codec.WriteError(w, http.StatusForbidden, "model_not_allowed",
				fmt.Sprintf("model %q is not available for this API key", ing.Model))
			return
		}

		// 整请求总预算（2026-10-10 修复的 P2）。
		//
		// 放在这里是因为：此刻配额已经预占、路由与白名单都已判定，再往后就是
		// 真正开始打上游了 —— 而「打上游」是唯一会长时间挂住的阶段。放在
		// 更早会让纯本地的判定（解码、限速）也背上这个 deadline，白白消耗预算。
		//
		// 绑到 r 上而不是局部 ctx：故障转移循环、配额收尾（rate.commit）、
		// 用量落库都从 r 派生 context，让整条请求共享同一个预算。
		//
		// 为什么不能靠 WriteTimeout 兜底：它是**每个写操作**的间隔上限，
		// 不是请求总时长；而流式响应天生长时间不写，套上去会掐死正常长流
		// （main 里 WriteTimeout 为 0 的注释解释的正是这件事）。
		//
		// 到期后表现：由上游 ctx 传播为 error，各 attempt 的错误映射会把它
		// 归类为传输层失败（可转移），链耗尽后回 504。已提交的流此时靠
		// idle 看门狗收尾，不会出现「写了头却没有终止信号」。
		budget := TotalRequestBudget(snap, h.cfg)
		ctx, cancelBudget := context.WithTimeout(r.Context(), budget)
		defer cancelBudget()
		r = r.WithContext(ctx)
		logger.Debug("request total budget armed",
			"budget_ms", budget.Milliseconds(),
			"model", ing.Model,
			"request_id", httpx.RequestIDFromContext(r.Context()))

		// 尝试预算：链上按 position 升序最多打 maxTargets 个目标。
		// 未开启故障转移 → 只打主目标（等价于改造前的单目标行为，零回归）。
		// 策略值统一来自「设置」页写入的全局默认（快照），config 只作兜底。
		cands := res.Candidates
		limit := len(cands)
		if !route.FailoverEnabled {
			limit = 1
		} else if budget := FailoverMaxTargets(snap, h.cfg); budget < limit {
			limit = budget
		}
		preflight := preflightCandidates(route.FailoverEnabled, limit, cands)

		// 余额预检（Lead 与用户确认的口径：预检拒绝，不做预占/退款）。
		//
		// 位置：白名单之后、触碰上游之前。理由与上面的白名单同级 —— 白名单
		// 判定只是一次内存线性扫描（零 IO），而余额预检要**查库**；把便宜的
		// 判定放前面，403 的语义就不会被 402 掩盖（一个无权用的模型，不该先
		// 让调用方看到"你没钱"）。
		//
		// 位置与 token 配额预检（ReserveQuota，在 Resolve 之前）**不同**是有
		// 意的：配额预检只要一个 est（估 token），不需要知道价格；而余额预检
		// 必须知道单价，单价挂在**上游模型**上 → 必须先 Resolve 才知道这条链
		// 上有哪些模型、各自多贵。两者都不打上游，顺序只影响错误码优先级。
		//
		// 与配额预检的另一处不同：**不做预占**。余额是浮点量，估算与真实值
		// 必然不等，预占了就得退款（等于把 ReserveQuota/ReleaseQuota 那套再
		// 实现一遍，而那正是 2026-10-07 的 P0-2 教训）。所以这里是纯查询。
		balanceStart := diag.begin("balance_precheck")
		bal := h.precheckBalance(w, r, authCtx, ing, preflight, codec)
		diag.end("balance_precheck", balanceStart)
		if !bal {
			// 余额预检已写回 402/500。rate.Commit(0) 不能省：走到这里说明
			// TPM/配额预占可能已经立起来了（needEstimate 为真时），不退的话
			// 它们会一直占着窗口/终身额度直到过期。与上面 model_not_allowed
			// 的收尾同口径。
			rate.Commit(0)
			diag.setResult("error", "balance_precheck_failed", "")
			return
		}

		// The same attempt budget is used by balance preflight and the actual
		// lane. It is computed above, before any admission query that depends
		// on which upstream models can actually serve this request.

		// 开故障转移时，跳过正被熔断的目标（若全被熔断则退回整条链，宁可打也不 404）。
		//
		// 这一步只做**查询**（TargetAvailable 已是纯函数），不占探测名额：
		// 紧接着的预算截断会把排到 failover_max_targets 之外的目标丢掉，
		// 它们永远进不了下面的请求循环，也就没人替它们释放名额。
		// 名额统一在循环内、真的要发请求前由 ClaimTargetProbe 领取。
		active := cands
		if route.FailoverEnabled {
			var avail []routing.Candidate
			for _, c := range cands {
				if h.pool.TargetAvailable(c.TargetID) {
					avail = append(avail, c)
				}
			}
			if len(avail) > 0 {
				active = avail
			}
		}

		// 硬约束过滤（2026-10-07 P1-8）：请求要求结构化输出（json_object /
		// json_schema）而候选上游实际使用 anthropic 协议时，跳过该候选。
		// "auto" 必须先取池中探测后的协议，否则自动识别出的 Anthropic
		// 会被误当成 Chat 放行，导致约束静默丢失。
		//
		// 为什么不能放行：Anthropic 没有 response_format / text.format 的对应物，
		// inwire 层已无处可翻；照发等于接受「200 但约束静默丢失」——尤其恶劣的
		// 是它让故障转移改变语义：同一请求打 chat 目标时约束生效，转移
		// 到 anthropic 目标后仍回 200 但约束消失。取舍：跳过候选比谎报成功更
		// 安全 —— 客户端拿到明确错误，至少知道该改请求或改路由，而不是拿着
		// 「看起来成功」的无约束输出去下游消费。
		//
		// 探测结果为 chat / response 的候选不受影响：两种协议都能表达这两类
		// 约束（inwire 层已做等价翻译）。放在熔断筛选之后、预算
		// 截断之前：先剔除服务不了的目标，让 failover_max_targets 的预算花在
		// 真正可服务的候选上。
		if ing.RequiresStructuredOutput {
			var serviceable []routing.Candidate
			for _, c := range active {
				if c.Provider != nil && h.effectiveProviderProtocol(c) != wireproto.Anthropic {
					serviceable = append(serviceable, c)
				}
			}
			if len(serviceable) == 0 {
				rate.Commit(0)
				logger.Warn("no upstream can serve structured output request",
					"model", ing.Model, "key_id", authCtx.KeyID,
					"candidates", len(active),
					"request_id", httpx.RequestIDFromContext(r.Context()))
				codec.WriteError(w, http.StatusBadRequest, "invalid_request_error",
					"this request requires structured output (json_object/json_schema), "+
						"but no candidate upstream can serve it: the anthropic protocol has no equivalent of response_format")
				diag.setResult("error", "invalid_request_error", "")
				return
			}
			active = serviceable
		}
		if len(active) > limit {
			active = active[:limit]
		}

		threshold := FailoverFailureThreshold(snap, h.cfg)
		keyID := authCtx.KeyID

		var out Outcome
		// lastTarget 记录本次请求最后评估到的候选。归因不能只看“是否已调用
		// 上游”：没有健康凭据时请求同样已经选中了这个目标，只是没走到网络
		// 调用；若仍落成空 provider_id，用量页就会出现“未知上游”。
		// Candidate 是值类型，额外用 evaluatedAny 区分“没评估”与“评估到零值”。
		var lastTarget routing.Candidate
		var evaluatedAny bool
		var lastUpstreamProtocol string
		// routeTrace 记录链上的失败决策（2026-10-17）。只在真的发生过转移时
		// 才有内容 —— 单目标成功、首跳成功都不写，避免给每条记录加噪声。
		// 由 AppendRouteTrace 统一格式化，保证「第几个目标」与失败原因成对出现。
		var routeTrace []string
		for i, cand := range active {
			isLast := i == len(active)-1
			lastTarget = cand
			evaluatedAny = true
			diag.setSelected(cand, "", h.effectiveProviderProtocol(cand))

			// 客户端已断开就收手：半路跑掉的人不该消耗整条链的下游配额，
			// 也不该把一次在途取消误记成目标的失败。此刻尚未写出任何字节，
			// 直接返回、不记 error usage（断流是客户端行为，不是上游故障）。
			// 此处还没领探测名额，不存在归还义务。
			if cerr := r.Context().Err(); cerr != nil {
				logger.Info("client disconnected, aborting failover",
					"model", ing.Model, "attempt", i+1, "error", cerr,
					"request_id", httpx.RequestIDFromContext(r.Context()))
				rate.Commit(0)
				return
			}

			// 真正要打这个目标时才领 half-open 探测名额。放在这里（而不是
			// 上面的筛选里）是因为名额要与「随后的成功/失败记账」一一配对 ——
			// 提前领取会让被预算裁掉的目标永久占住名额。
			//
			// 名额有两条出路：记账释放（Record*），或没走到记账时由循环尾部
			// 的 ReleaseTargetProbe 显式归还（probeClaimed/probeRecorded 就是
			// 这对配对标志）。缺了后一条的话，不可转移错误与客户端中断这两种
			// 合法形态会把名额永久占死 —— 一次 400 就废掉一个目标。
			//
			// 非主目标且没开故障转移时不用熔断器管，行为与改造前一致。
			//
			// threshold<=0（运维显式关闭目标级熔断）时**完全跳过**：
			// 熔断器既然不启用，就不该再拿它的半开名额去限流 ——
			// 否则运维「关了熔断」得到的仍是「被限成一个并发 + 静默
			// 改投次目标」（见 upstream.Pool.RecordTargetFailure 的
			// threshold<=0 分支，那条路径已经把 recovering 清掉了）。
			var probeToken upstream.ProbeToken
			probeClaimed, probeRecorded := false, false
			if route.FailoverEnabled && threshold > 0 {
				var claimed bool
				probeToken, claimed = h.pool.ClaimTargetProbe(cand.TargetID)
				if !claimed {
					// 仍在冷却中，或探测名额已被同链的并发请求领走 —— 沿链继续。
					// 没领到名额，也就没有归还义务。
					routeTrace = AppendRouteTrace(routeTrace, i+1, cand, "skip", "probe_busy_or_cooling")
					diag.addAttempt(attemptDiagnostics{
						Index: i + 1, TargetID: cand.TargetID,
						ProviderID: providerID(cand), ProviderName: providerName(cand),
						ProviderSlug: providerSlug(cand), UpstreamModel: cand.UpstreamModel.ModelID,
						Protocol: h.effectiveProviderProtocol(cand), EndpointHost: providerEndpointHost(cand),
						NetworkID: providerNetworkID(cand), ProxyConfigured: providerProxyConfigured(cand),
						Status: "skipped", ErrorCode: "probe_busy_or_cooling", Eligible: false,
						FailureStage: "target_circuit_breaker", FailureAttribution: "target_configuration",
						AttributionConfidence: "high",
					})
					if !isLast {
						logger.Warn("failover: target not claiming a probe slot, switching target",
							"model", ing.Model, "provider", cand.Provider.Slug, "attempt", i+1,
							"request_id", httpx.RequestIDFromContext(r.Context()))
						continue
					}
					out = Outcome{Eligible: true, StatusCode: http.StatusBadGateway,
						Code: "upstream_error", Message: "no available upstream provider"}
					break
				}
				probeClaimed = true
			}

			// 一次 attempt 只取该 provider 的一把凭据：某把 key 失败时本请求不就地换
			// 同 provider 的下一把，而是让位给链上下一个目标。跨请求的 key 轮换交给
			// 冷却 —— 坏 key 被踢出 healthy 后，下个请求自会选到好 key。这是有意取舍，
			// 免得「一个请求把某 provider 所有 key 各打一遍」放大延迟与配额消耗。
			credentialAffinity := strings.TrimSpace(ing.CacheKey)
			if credentialAffinity == "" {
				credentialAffinity = conversationSessionID(r)
			}
			credentialStart := diag.begin("credential_select")
			client, credID, cerr := h.pool.GetAnyClientByAffinity(cand.Provider.Slug, credentialAffinity)
			diag.end("credential_select", credentialStart)
			if cerr != nil {
				// 该目标当前无健康凭据：这是 provider 的凭据状态，不是目标/上游
				// 故障。把目标熔断与凭据冷却叠加会延长恢复时间，而且下一次
				// 请求仍会先被目标熔断挡住，看不到“补一把 key 就好”。
				if probeClaimed {
					h.pool.ReleaseTargetProbe(cand.TargetID, probeToken)
				}
				probeRecorded = true // 名额已在上方显式归还
				out = Outcome{Eligible: true, StatusCode: http.StatusBadGateway,
					Code: "upstream_error", Message: "no available upstream provider"}
				routeTrace = AppendRouteTrace(routeTrace, i+1, cand, "fail", "no_healthy_credential")
				diag.addAttempt(attemptDiagnostics{
					Index: i + 1, TargetID: cand.TargetID,
					ProviderID: providerID(cand), ProviderName: providerName(cand),
					ProviderSlug: providerSlug(cand), UpstreamModel: cand.UpstreamModel.ModelID,
					Protocol: h.effectiveProviderProtocol(cand), EndpointHost: providerEndpointHost(cand),
					NetworkID: providerNetworkID(cand), ProxyConfigured: providerProxyConfigured(cand),
					Status: "failed", ErrorCode: "no_healthy_credential",
					ErrorMessage: cerr.Error(), CredentialMs: 0, Eligible: true,
					FailureStage: "target_credential_pool", FailureAttribution: "target_configuration",
					AttributionConfidence: "high",
				})
				if !isLast {
					logger.Warn("failover: no healthy credential, switching target",
						"model", ing.Model, "provider", cand.Provider.Slug, "request_id", httpx.RequestIDFromContext(r.Context()))
					continue
				}
				break
			}
			lastUpstreamProtocol = upstreamProtocolName(client.Protocol())
			diag.setSelected(cand, credID, lastUpstreamProtocol)

			attemptTrace := upstream.NewAttemptTrace()
			attemptCtx := upstream.WithAttemptTrace(r.Context(), attemptTrace)
			attemptReq := r.WithContext(attemptCtx)
			attemptStart := time.Now()
			if ing.Stream {
				out = h.attemptStream(w, attemptReq, client, ing, cand, authCtx, codec, rate, snap, start, routeTrace, diag, attemptTrace, i+1, attemptStart)
			} else {
				out = h.attemptNonStream(w, attemptReq, client, ing, cand, authCtx, codec, rate, snap, start, routeTrace, diag, attemptTrace, i+1, attemptStart)
			}
			decision := out.LaneDecision()
			if decision.Committed {
				recordCommittedOutcome(h.pool, decision, cand.TargetID, credID, probeToken, threshold)
				routeTrace = AppendRouteTrace(routeTrace, i+1, cand, decision.TraceOutcome, decision.TraceReason)
				probeRecorded = true // 名额由记账释放
				return
			}

			// 未写出任何下游字节才可能转移；此处按分类回写凭据冷却与目标熔断计数。
			//
			// credCooldown==0 表示这次失败不该罚凭据（当前只有 404/410：目标服务不了
			// 这个模型，key 本身是好的）—— 只累计目标熔断。旧实现在这个分支调
			// MarkCredentialError，既不改库也不参与健康过滤，是个纯日志空操作，已删。
			//
			// 客户端在调用进行中断开时必须跳过全部记账：SDK 会把 context 取消包成
			// TransportError，落进可转移集合，若照记就会把一把健康凭据冷却 60s、并
			// 累计目标熔断 —— 单 key provider 会被几次用户中断搞成整体不可用
			// （凭据冷却期内不再被选中，也就没有任何请求能成功以触发复苏）。
			// r.Context() 只在客户端断开/服务关停时取消（上游超时用的是派生 ctx），
			// 因此这个判据能精确区分「客户端跑了」与「上游真的坏了」。
			// 判据只认 Canceled：请求 ctx 上还挂着 TotalRequestBudget 的超时，
			// 用 ctx.Err()==nil 会把「预算到期」也一起放过去 —— 那是网关侧的
			// 事件，但请求确实已经打到了这个目标，用它当「上游健康」的证据
			// 同样不成立，所以这里只排除真正的客户端取消。
			if decision.PenalizeUpstream {
				h.pool.RecordTargetFailure(cand.TargetID, probeToken, threshold)
				probeRecorded = true // 名额由记账释放
				if out.CredentialCooldown > 0 {
					h.pool.MarkCredentialCooldown(credID, out.CredentialCooldown)
				}
			}

			// 未提交的失败：记下「哪个目标、什么原因、是否可转移」。
			// 不可转移同样要记 —— 运维最常问的正是「为什么这次没换上游」。
			routeTrace = AppendRouteTrace(routeTrace, i+1, cand, decision.TraceOutcome, decision.TraceReason)

			// 没走到记账的两种形态 —— 不可转移错误（out.Eligible==false）与
			// 客户端中断（ctx 已取消）—— 在此归还探测名额，否则 halfOpen 卡死、
			// 该目标被跳到下一次 Install 才复位。已记账的绝不能再还；
			// ReleaseTargetProbe 按令牌比较，迟到的旧请求也清不掉新探针。
			if probeClaimed && !probeRecorded {
				h.pool.ReleaseTargetProbe(cand.TargetID, probeToken)
			}

			if !decision.Eligible || isLast {
				break
			}
			logger.Warn("failover: switching to next target",
				"model", ing.Model, "from_provider", cand.Provider.Slug,
				"error_code", out.Code, "attempt", i+1, "route_trace", strings.Join(routeTrace, "; "),
				"request_id", httpx.RequestIDFromContext(r.Context()))
		}

		// 走到这里 = 最后一次尝试未提交（要么不可转移错误、要么链已耗尽）。
		// 写出最终错误并记一条 usage —— 若 active 为空（理论不该发生）兜底成 502。
		// 客户端已经断开时它根本收不到这个响应（写了只会撞 broken pipe），
		// 且这次失败与上游无关，按 canceled 记账，与流式路径同一口径。
		// 只把「客户端真的走了」算作 clientGone：ctx 上还挂着
		// TotalRequestBudget 的超时（DeadlineExceeded），那是网关侧事件，
		// 记成 canceled 会让网关自己的超时在报表里与用户主动取消混为一谈，
		// 并让「写不写错误响应」这个决定被预算到期误触发。
		out = out.WithRequestContext(r.Context())
		decision := out.LaneDecision()
		clientGone := decision.DownstreamClient
		gatewayTimeout := decision.GatewayTimeout

		statusCode, code, message := out.StatusCode, out.Code, out.Message
		// 兜底：不许把 200 或 0 当作「失败结果」发出去。
		//
		// statusCode==0 是「没有候选目标」的哨兵，一直有 502 兜底。
		// statusCode==200 则是**本该不可能**的状态：这条路径只在链已耗尽、
		// 响应尚未提交时到达，任何失败都必须用 >=400 表达。修复前它真的发生过
		// （见 OutcomeFromErr 的注释：流式哨兵被映射成 200），客户端于是拿到
		// 一个「成功但内容为空」的响应。把这一并归入 502，是为了让这条不变量
		// 在**写出响应**这个最后一关口上也有牙齿 —— 将来谁再引入一个返回 200
		// 的失败分支，症状会是「这里莫名其妙变成 502」，而不是「客户端拿到
		// 假成功且无人察觉」。
		if statusCode == 0 || statusCode == http.StatusOK {
			statusCode, code, message = http.StatusBadGateway, "upstream_error", "no available upstream provider"
		}
		if !clientGone {
			codec.WriteError(w, statusCode, code, message)
		}
		diag.setResult(decision.DiagnosticStatus, decision.DiagnosticError, out.Detail)
		diag.setTerminal(clientGone, gatewayTimeout)
		rate.Commit(0)
		provID, providerName, upstreamModel, upstreamProtocol := "", "", "", ""
		// 归因到最后评估到的目标：即使失败发生在凭据选择阶段，这个目标仍是
		// 本次请求真实命中的路由成员。Provider 判空防御 active 为空的理论
		// 分支，避免解引用零值 Candidate 时 panic。
		if evaluatedAny && lastTarget.Provider != nil {
			provID = lastTarget.Provider.ID
			providerName = lastTarget.Provider.Name
			upstreamModel = lastTarget.UpstreamModel.ModelID
			upstreamProtocol = h.effectiveProviderProtocol(lastTarget)
			if lastUpstreamProtocol != "" {
				upstreamProtocol = lastUpstreamProtocol
			}
		}
		status := decision.UsageStatus
		errorCode := decision.DiagnosticError
		errorDetail := out.Detail
		switch decision.Kind {
		case OutcomeClientAborted:
			logger.Info("client disconnected before any response was written",
				"model", ing.Model, "request_id", httpx.RequestIDFromContext(r.Context()))
			errorCode = ""
			errorDetail = ""
		case OutcomeGatewayTimeout:
			errorCode = "gateway_timeout"
			errorDetail = "gateway request budget exceeded"
			logger.Warn("request cut by gateway total request budget",
				"model", ing.Model, "request_id", httpx.RequestIDFromContext(r.Context()))
		}
		h.usage.Record(&store.UsageRecord{
			ID:               newID(),
			AccessKeyID:      keyID,
			UserID:           authCtx.UserID,
			RequestID:        httpx.RequestIDFromContext(r.Context()),
			PublicModel:      ing.Model,
			ProviderID:       provID,
			ProviderName:     providerName,
			UpstreamModel:    upstreamModel,
			IngressProtocol:  codec.Name(),
			UpstreamProtocol: upstreamProtocol,
			Stream:           ing.Stream,
			UsageState:       store.UsageStateNone,
			Status:           status,
			HTTPStatus:       statusCode,
			ErrorCode:        errorCode,
			ErrorDetail:      errorDetail,
			LatencyMs:        time.Since(start).Milliseconds(),
			RouteTrace:       strings.Join(routeTrace, "; "),
		})
		diag.setRouteTrace(strings.Join(routeTrace, "; "))
	}
}

// acquireStreamSlot 申请一个流式并发名额。
//
// 返回 true = 拿到名额（调用方负责在请求结束时释放）。
// 返回 false = 没拿到：要么 ctx 已取消（客户端走了），要么等待超时
// （过载）。调用方据此决定是静默返回还是回 503。
//
// # 为什么等待必须有上限（2026-10-22 修 §7.1-3）
//
// 旧实现是纯阻塞：拿不到名额就一直等，直到客户端断开。过载时请求会
// 堆在网关里 —— 每个都占着一个 goroutine、一个连接、以及一份已鉴权
// 但尚未结算的配额预占。堆到一定程度，网关对**所有**请求都变慢
// （不只是流式），而客户端看到的只是「越来越慢」，没有一个明确的可
// 重试信号。
//
// 有了上限，过载就变成**快速失败**：等不到名额就回 503 + Retry-After。
func (h *Handler) acquireStreamSlot(ctx context.Context) bool {
	timer := time.NewTimer(h.admissionTimeout)
	defer timer.Stop()
	select {
	case h.streams <- struct{}{}:
		return true
	case <-ctx.Done():
		return false
	case <-timer.C:
		return false
	}
}

// modelNotFoundMessage 拼「模型不存在」的文案，并在能猜到时给出候选。
//
// # 为什么值得加这一段（magpie 的 unknownModel 做法）
//
// 只回 `model "xxx" not found` 时，用户面对三个无法区分的可能性：
// 名字打错了 / 这个模型压根没配 / 名字对但我没权限。三个的后续动作完全不同，
// 而原文什么都不说 —— 于是最常见的一类接入故障（打错字）需要用户自己去翻
// 管理界面逐个比对才能排除。
//
// 追加候选把这一步消掉。刻意**只列名字、不列数量**以外的任何配置信息：
// 这条错误会回给持有任一有效 key 的调用方，列出全部可调模型等于把网关的
// 模型清单暴露给任何一个拿到 key 的人（哪怕该 key 的白名单本应只许调一个）。
//
// 鉴权口径：路由索引是全局的，不按身份收窄 —— 但 model_not_found 之前
// 已经过了 auth 与白名单（白名单在下面才判），能走到这里的调用方本来就有
// 一把有效 key。列候选名不新增暴露面，因为同一把 key 调
// GET /v1/models 就能拿到同一份清单。
func ModelNotFoundMessage(snap *snapshot.Snapshot, requested string) string {
	base := fmt.Sprintf("model %q not found", requested)
	if snap == nil || snap.Routes == nil {
		return base
	}
	sugg := snap.Routes.SuggestModel(requested, 5)
	if len(sugg) == 0 {
		return base + "（该名字没有匹配到已配置的模型；用 GET /v1/models 查看可用清单）"
	}
	return fmt.Sprintf("%s。可用的相似模型：%s", base, strings.Join(sugg, ", "))
}

// effectiveProviderProtocol returns the protocol the pool actually resolved,
// falling back to the routing snapshot while the pool has no entry yet. The
// configured value may be "auto"; protocol-specific behavior must never treat
// that as Chat.
func (h *Handler) effectiveProviderProtocol(c routing.Candidate) string {
	if c.Provider == nil {
		return ""
	}
	if h.pool != nil {
		if protocol := h.pool.ResolvedProtocol(c.Provider.Slug); protocol != "" {
			return protocol
		}
	}
	return wireproto.Normalize(c.Provider.Protocol)
}

// attemptStream 尝试在当前目标上完成一次流式响应。
//
// 关键：SSE 头与状态码**推迟到拿到第一个上游事件之后才写**。这样「建立失败」
// 和「首字迟迟不来」都发生在向下游写出任何字节之前，可安全地让外层循环换目标；
// 一旦写了头并提交首个事件，就再无回退余地（DESIGN §414 的约束）。
//
// 本函数只做与协议无关的事：看门狗、心跳、事件循环、断流状态归类、落库；
// 「统一事件 → 下游协议分片」全部经由 codec.NewSink 的 StreamSink 完成。
// priorTrace 是外层循环在本目标之前已经做出的决策（例如「第 1 个目标 404，
// 可转移」）。它必须带进来：本 attempt 一旦提交（写下首字节），usage 就在
// 这里落库且不会再回到外层 —— 只写当前一跳会丢掉「为什么走到备用上游」，
// 而那恰恰是这条记录最有价值的部分。
func (h *Handler) attemptStream(w http.ResponseWriter, r *http.Request, client *rosetta.Client, ing *ingress.Request, cand routing.Candidate, authCtx *auth.Context, codec ingress.Codec, rate *RateCommit, snap *snapshot.Snapshot, start time.Time, priorTrace []string, diag *requestDiagnostics, attemptTrace *upstream.AttemptTrace, attemptIndex int, attemptStart time.Time) Outcome {
	ctx := r.Context()
	logger := h.usage.Logger()
	publicModel := ing.Model
	attempt := attemptFromCandidate(attemptIndex, cand, attemptTrace)
	upstreamProtocol := upstreamProtocolName(client.Protocol())
	ctx = withPromptCacheAffinity(ctx, ing, upstreamProtocol, conversationSessionID(r))
	attempt.Protocol = upstreamProtocol
	attempt.StartedAtMs = attemptStart.Sub(start).Milliseconds()
	buildStart := time.Now()

	// 每个 attempt 现造一份请求，并把公共别名换成该目标的上游 model_id、
	// 挂上该上游协议的私有透传字段。必须在 attempt 内部做（而不是在调用点
	// 造好传进来）：链上各目标的协议不同，Extra 形状也不同，复用同一实例
	// 会把 A 目标的字段泄漏给 B 目标。
	upstreamReq := ing.BuildRosetta()
	upstreamReq.Model = cand.UpstreamModel.ModelID
	ing.ApplyUpstreamExtras(upstreamReq, upstreamProtocol)
	ing.ApplyThinkingCapability(upstreamReq, cand.UpstreamModel, upstreamProtocol)
	// 剥掉目标协议回放不了的思考块（chat 全剥 / anthropic 剥无签名的），
	// 否则 harness 回传 thinking 后的第二轮必然 400。详见 replay_gate.go。
	h.applyReplayGate(upstreamReq, upstreamProtocol)
	attempt.RequestBuildMs = time.Since(buildStart).Milliseconds()
	diag.setIngressRequest(upstreamReq, true)

	// 首字预算只覆盖「等首个事件」这一段；建立连接那段交给 ResponseHeaderTimeout
	// 与下面的总请求预算（2026-10-10 修复的 P2）。
	//
	// # 为什么**不**给 ChatStream 套一个带超时的子 context
	//
	// 直觉上「连接也该算进首字预算」，做法是
	//
	//	preCtx, cancel := context.WithTimeout(ctx, ttftTimeout)
	//	stream, err := client.ChatStream(preCtx, req)
	//	defer cancel()   // ← 危险，见下
	//
	// 但这个做法**会掐断正在正常输出的长流**，已实测确认：
	// rosetta 的 ChatStream 内部是 `WithCancel(ctx)` 后把 ctx 直接交给
	// provider.StreamChat，最终 `http.Do(ctx, call)`（SDK 内没有
	// WithoutCancel / detach），所以 ctx 一被取消，net/http 立刻关闭响应体。
	// 首字到达后再 cancel，等于在流输出到一半时把它掐了 ——
	// 客户端拿到半截回答，且 status 无法表达（头已经写出去了）。
	// 实测：发 4 个事件、首个之后取消 → 只读到 2 个，err=context canceled；
	// 不取消的对照组 4 个全读完。`defer cancel()` 更糟，它要等整个
	// attemptStream 返回才执行，等于把流的后半程一起掐掉。
	//
	// 所以首字预算**只**由下面的 ttftTimer 承担，它的作用范围天然是
	// 「拿到流对象之后、首个事件之前」—— 这一段是安全的：此时尚未写出任何
	// 字节，取消/关流都属于「未提交」，外层仍可转移。
	//
	// 「拨号 + 等响应头」这段仍由 internal/upstream 的
	// ResponseHeaderTimeout（60s，镜像为 responseHeaderWait）兜底，
	// 并计入 TotalRequestBudget —— 它不会无限，只是比首字预算宽。
	ttftTimeout := FirstTokenTimeout(snap, h.cfg)

	callStart := time.Now()
	attempt.UpstreamCallStarted = true
	stream, err := client.ChatStream(ctx, upstreamReq)
	if err != nil {
		out := OutcomeFromErrCtx(r.Context(), err)
		attempt.UpstreamCallMs = time.Since(callStart).Milliseconds()
		finishAttempt(&attempt, attemptTrace, attemptStart, out, err, diag)
		return out
	}
	defer stream.Close()
	attempt.UpstreamCallMs = time.Since(callStart).Milliseconds()

	// 首字（TTFT）看门狗：只掐「一个事件都没等到」的慢上游，触发即关流，
	// 尚未写头 → 未提交 → 外层可转移。与下面的 idle 看门狗是两回事。
	var ttftTimedOut atomic.Bool
	// ttftDone 让「定时器已触发」这件事变成可等待的：Stop() 返回 false 只说明
	// 回调已经**开始**跑（或跑完），不保证 stream.Close() 已落地。这个 channel
	// 由回调关闭，用于消除那个窗口。
	ttftDone := make(chan struct{})
	ttftTimer := time.AfterFunc(ttftTimeout, func() {
		ttftTimedOut.Store(true)
		_ = stream.Close()
		close(ttftDone)
	})
	gotFirst := stream.Next()

	// **必须看 Stop() 的返回值**：返回 false = 定时器已触发，而回调里的
	// stream.Close() 可能刚刚或即将执行。此时即便 gotFirst 为真，流也已经/
	// 即将被关掉，若继续往下走就会写 200 与 SSE 头，产出「空但 ok」的假正常流
	// —— 客户端拿到 200 与正确的响应头，正文却是空的，且状态码无法表达失败。
	// 等 ttftDone 闭合，确保 Close 一定已发生，再按「超时」处理。
	ttftFired := !ttftTimer.Stop()
	if ttftFired {
		<-ttftDone
	}

	if !gotFirst || ttftTimedOut.Load() {
		if cerr := stream.Err(); cerr != nil {
			out := OutcomeFromErrCtx(r.Context(), cerr)
			finishAttempt(&attempt, attemptTrace, attemptStart, out, cerr, diag)
			return out
		}
		if ttftTimedOut.Load() {
			logger.Warn("stream first-token timeout", "model", publicModel,
				"provider", cand.Provider.Slug, "ttft_timeout_ms", ttftTimeout.Milliseconds())
			// 首字超时是目标侧的性能/可用性问题，不是这把 key 失效：
			// 只累计目标熔断，不冷却凭据。否则单 key provider 会在一次
			// 慢响应后整家 60s 不可用。
			out := Outcome{Kind: OutcomeUpstreamFailed,
				Eligible:   true,
				StatusCode: http.StatusGatewayTimeout, Code: "upstream_timeout",
				Message: "upstream first-token timeout",
				Detail:  "upstream did not produce its first stream event before the first-token timeout"}
			finishAttempt(&attempt, attemptTrace, attemptStart, out, nil, diag)
			return out
		}
		// 干净 EOF 却在首个事件之前（上游返回 200 后立刻空流）—— 按可转移的空响应处理。
		out := Outcome{Kind: OutcomeUpstreamFailed,
			Eligible: true, StatusCode: http.StatusBadGateway,
			Code: "upstream_error", Message: "upstream returned empty response",
			Detail: "upstream closed the stream cleanly before producing its first event"}
		finishAttempt(&attempt, attemptTrace, attemptStart, out, nil, diag)
		return out
	}

	// —— 首个事件已到，自此提交：写 SSE 头，之后任何中断都只能如实 truncated 收尾 ——
	ttfbMs := time.Since(start).Milliseconds()
	firstEventAt := time.Now()
	// usage 的 ttfbMs 保持「请求进入网关到首字」的端到端口径；
	// attempt.TTFBMs 则只量上游调用开始到首字，避免把鉴权、排队和请求
	// 构造算进上游性能。
	attempt.TTFBMs = firstEventAt.Sub(callStart).Milliseconds()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	// 流式编码器由 codec 构造：OpenAI 走 SSEWriter，Anthropic 走块状态机。
	sink := codec.NewSink(w, publicModel, ing)
	// 单次写截止时间（2026-10-17）：下游挂死不读时让 Write 自己返回超时，
	// 而不是让 handler 永久阻塞、连带把上游流与 token 一起耗着。
	// 用法是能力探测，测试替身等无底层连接的实现会静默跳过。
	outwire.BindWriteDeadline(sink, w)

	idleTimeout := StreamIdleTimeout(snap, h.cfg)
	var idleTimedOut atomic.Bool
	// lastEventAt 记最近一次上游事件到达的时刻，idle 回调据此**复核**后再关流。
	//
	// 只靠 Timer.Reset 不够：Reset 追不回已经派发的回调，超时边界上「事件按时
	// 到达、回调却已起跑」会把健康流误杀成 stream_idle_timeout —— 与 TTFT
	// 看门狗已用 ttftDone 修掉的是同一类竞态，但 idle 路径此前仍在。
	// 复核判据取「回调执行时刻距最近事件是否真的已满 idleTimeout」：真空闲时
	// 必然成立；边界竞速中到达的事件让复核不通过，此时直接退出 —— consume
	// 里的 Reset 已把看门狗续上，流继续。
	// lastEventAt 必须在 Reset **之前**更新：反过来会让回调读到旧时刻，
	// 把「事件刚到」误判成「已空闲整段超时」。
	var lastEventAt atomic.Int64
	lastEventAt.Store(time.Now().UnixNano())
	idleTimer := time.AfterFunc(idleTimeout, func() {
		if time.Since(time.Unix(0, lastEventAt.Load())) < idleTimeout {
			return // 边界竞速：事件在超时边缘合法到达，流还活着
		}
		idleTimedOut.Store(true)
		logger.Warn("stream idle timeout",
			"model", publicModel, "key_id", authCtx.KeyID,
			"idle_timeout_ms", idleTimeout.Milliseconds())
		_ = stream.Close()
	})
	defer idleTimer.Stop()

	// 心跳间隔必须严格为正：idleTimeout 为 1ms 时 idleTimeout/2 == 0，
	// time.NewTicker 会当场 panic —— 而此刻 SSE 响应头已经写出，收不回来。
	// config.validate 已挡掉负的超时配置，这里是最后一道防线。
	heartbeatInterval := idleTimeout / 2
	if heartbeatInterval <= 0 {
		heartbeatInterval = time.Second
	}
	heartbeatTicker := time.NewTicker(heartbeatInterval)
	defer heartbeatTicker.Stop()

	// 心跳只在**真的空闲**时才发。上游持续吐字时由 handleEvent 把 ticker 推后，
	// 否则一条两秒的流会连发十几条 `: keepalive` —— 对标准 SSE 客户端无害，
	// 对按行解析的下游是纯噪声。
	// 心跳 goroutine 与主循环会并发写同一个 ResponseWriter，所以统一经由 sink
	// （各实现内部有锁），不再自己 fmt.Fprintf(w, ...)。
	// 心跳 goroutine 必须**保证在 handler 返回前退出**：它与主循环并发写同一个
	// ResponseWriter，若handler 先返回、goroutine 还在 ticker 周期里，就会写一个
	// 已结束的响应（连接复用时甚至串到下一个请求）。原来的 ctx.Done() 只在
	// 客户端断开时才触发，正常完成路径上 goroutine 会一直活着。
	// quit + WaitGroup 是必须的：ticker 最多要等一个周期才退出，不能靠 defer。
	quit := make(chan struct{})
	var hbWG sync.WaitGroup
	hbWG.Add(1)
	go func() {
		defer hbWG.Done()
		for {
			select {
			case <-heartbeatTicker.C:
				sink.Keepalive()
			case <-quit:
				return
			case <-ctx.Done():
				return
			}
		}
	}()
	defer func() {
		close(quit)
		hbWG.Wait()
	}()

	var lastUsage rosetta.Usage
	var stopReason rosetta.StopReason
	status := store.UsageStatusOK
	errorCode := ""
	// errorDetail 只在 default 分支（上游真的返回了错误）填 —— truncated /
	// overflow / idle_timeout 是「流断了」而不是「上游回了错误正文」，
	// canceled 更是客户端自己的行为，都不该有详情。
	errorDetail := ""
	httpStatus := 200
	var terminalErr error

	// partialOutRunes 累计本次流里**已经收到**的输出侧字符数（正文 + 思考）。
	//
	// 它的唯一用途是「客户端中途断开」这一条路径：见 ingress.PartialOutTokens 的注释。
	partialOutRunes := 0

	// consume 消费一个上游事件（首个 + 后续走同一套逻辑）：看门狗续期与
	// usage/stopReason 留档是循环的职责，协议编码全部交给 sink。
	consume := func(ev *rosetta.Event) {
		// 时刻先于 Reset 落账（顺序不能反，见 lastEventAt 声明处的注释），
		// 然后 idle 看门狗续期、心跳对齐。
		lastEventAt.Store(time.Now().UnixNano())
		idleTimer.Reset(idleTimeout)
		heartbeatTicker.Reset(heartbeatInterval)
		switch ev.Type {
		case rosetta.EventMessageEnd:
			attempt.StreamEvents++
			if ev.Usage != nil {
				lastUsage = *ev.Usage
			}
			stopReason = ev.StopReason
		case rosetta.EventTextDelta, rosetta.EventThinkingDelta:
			// 只数**正文与思考**：工具调用的参数是 JSON 片段，
			// 混进 token 估算会让数字失去意义（半个 JSON 对象既不是
			// 中文也不是英文，按字符数估会离谱地偏高）。
			partialOutRunes += len([]rune(ev.Text))
			attempt.StreamEvents++
		case rosetta.EventToolCall:
			attempt.StreamEvents++
			if attempt.FirstToolCallMs == 0 {
				attempt.FirstToolCallMs = time.Since(attemptStart).Milliseconds()
			}
			if ev.ToolID != "" || ev.ToolName != "" {
				attempt.ToolCalls++
			}
		case rosetta.EventMessageStart:
			attempt.StreamEvents++
			diag.setUpstreamResponse(ev.ID)
		}
		sink.Event(ev)
	}

	lastEvent := time.Now()
	consume(stream.Event())
	// The first event can fail after WriteHeader has committed a 200 response.
	// In that case the stream loop would otherwise exit through the generic
	// client-abort path and the outer request lane would append a second usage
	// row as an uncommitted 502. Treat the first write failure as terminal here.
	firstWriteFailed := outwire.FirstWriteError(sink) != nil
	if firstWriteFailed {
		_ = stream.Close()
	}
	for !firstWriteFailed && stream.Next() {
		now := time.Now()
		if gap := now.Sub(lastEvent).Milliseconds(); gap > attempt.MaxEventGapMs {
			attempt.MaxEventGapMs = gap
		}
		lastEvent = now
		consume(stream.Event())
		// 下游连接已不可用（2026-10-21 修 *X12）：继续把上游流读完只会把
		// 字节写进一个死连接，而上游还在按 token 计费、我们还在为它付钱。
		// 立即关流，让下面的写失败分支收尾。
		//
		// 关流用 Close（而非 Abort）：这是**我们**主动不看了，不是上游
		// 出错 —— Close 保持 Err()==nil，于是终态由写失败分支给出，
		// 不会被误记成上游故障进熔断计数。
		if outwire.FirstWriteError(sink) != nil {
			_ = stream.Close()
			break
		}
	}
	attempt.PostTTFBMs = time.Since(firstEventAt).Milliseconds()

	// 终态判定顺序（2026-10-21 修 *X12）——顺序本身就是 bug：
	//
	// **写失败必须先于 stream.Err() 判**，否则这条路径永远走不到：主循环
	// 发现写失败后 Close 并 break，而 SDK 的生产 goroutine 可能已经先一步把
	// 「EOF 却没有 [DONE]」折成 ErrStreamTruncated 写进 s.err。于是
	// stream.Err() 非空、进 truncated 分支：残缺流被判成「上游截断」——
	// 既进熔断计数，又让运维看不出是下游写不进去。测试 F1c 正是这一种。
	//
	// 另一个理由不依赖竞态：写失败意味着**我们主动放弃了这条流**，之后
	// 发生的一切都是放弃的后果。把下游不可用记成上游故障，会让熔断器
	// 因为自己的客户端掉线而惩罚上游。
	//
	// 两者同时出现时 Stream 端的错误仍要留痕：根因可能真的是上游，
	// 只是被下游写失败抢先报了。降一级到 Debug 是为了不让每次客户端
	// 断网都刷一条 WARN。
	if werr := outwire.FirstWriteError(sink); werr != nil {
		// 向下游写失败（2026-10-21 修 *X12）。
		//
		// 三个 sink 的 SSE 写点此前**全部**丢弃 fmt.Fprintf 的返回值，
		// 于是下游挂死/断开后状态机照常推进、残缺的流被当成 status=ok
		// 收尾：既不进熔断计数，用量里也无痕。sink 现在记下第一次写失败
		// （见 outwire.WriteErrorReporter），这里把它变成终态。
		//
		// 归因：写失败 = 下游不可用，与「客户端主动断开」同类，
		// 因此复用 canceled 这个取值（它已经是「不计入上游熔断」的那一类，
		// 见 Outcome.ClientGone）。单独一个 errorCode 让运维能
		// 区分「客户端自己走了」与「我们写不进去」。
		status = store.UsageStatusCanceled
		errorCode = "downstream_write_failed"
		errorDetail = outwire.SafeErrorDetail(werr)
		terminalErr = werr
		if serr := stream.Err(); serr != nil {
			logger.Debug("stream error observed after downstream write failure",
				"model", publicModel, "key_id", authCtx.KeyID, "stream_error", serr)
		}
		if outwire.IsDownstreamWriteError(werr) {
			logger.Info("downstream write failed mid-stream",
				"model", publicModel, "key_id", authCtx.KeyID, "error", werr)
		} else {
			// 认不出来的写错误可能是网关自身的 bug，不该悄悄算到客户端头上。
			logger.Error("unexpected response write error",
				"model", publicModel, "key_id", authCtx.KeyID, "error", werr)
		}
	} else if err := stream.Err(); err != nil {
		terminalErr = err
		// 网关总预算到期不是上游故障（2026-10-22 修 *X11）。
		// 必须排在 truncated / client-gone 之前：预算到期时上游流通常
		// 也会报 TransportError，若先按上游错误归类，就会冷却凭据并累计
		// 目标熔断。到期之后也不再重试 —— 共享 deadline 已经没了。
		if errors.Is(r.Context().Err(), context.DeadlineExceeded) {
			status = store.UsageStatusTimeout
			errorCode = "gateway_timeout"
			errorDetail = "gateway request budget exceeded"
			if detail := outwire.SafeErrorDetail(err); detail != "" && detail != errorDetail {
				errorDetail += "\n" + detail
			}
			logger.Warn("stream cut by gateway total request budget",
				"model", publicModel, "key_id", authCtx.KeyID,
				"elapsed_ms", time.Since(start).Milliseconds(),
				"error", err)
		}
		if !errors.Is(r.Context().Err(), context.DeadlineExceeded) {
			switch {
			case errors.Is(err, rosetta.ErrStreamTruncated):
				status = store.UsageStatusTruncated
				errorCode = "stream_truncated"
				errorDetail = outwire.UpstreamErrorDetail(err)
				logger.Warn("stream truncated", "model", publicModel, "key_id", authCtx.KeyID, "error", err)
			case errors.Is(err, rosetta.ErrStreamOverflow):
				status = store.UsageStatusOverflow
				errorCode = "stream_overflow"
				errorDetail = outwire.UpstreamErrorDetail(err)
				logger.Error("stream overflow", "model", publicModel, "key_id", authCtx.KeyID, "error", err)
			case IsClientGone(ctx, err):
				// 客户端主动断开不是上游故障。此前一律记 error，后果是：后台错误率虚高、
				// 成功率虚低，且真故障被 ERROR 噪音淹没。单独一个取值才能把两者分开。
				status = store.UsageStatusCanceled
				logger.Info("client disconnected mid-stream", "model", publicModel, "key_id", authCtx.KeyID, "error", err)
			default:
				status = store.UsageStatusError
				errorCode = "upstream_error"
				errorDetail = outwire.UpstreamErrorDetail(err)
				logger.Error("stream error", "error", err, "model", publicModel, "key_id", authCtx.KeyID)
			}
		}
	} else if idleTimedOut.Load() && !sink.SawTerminal() {
		status = store.UsageStatusTruncated
		errorCode = "stream_idle_timeout"
		errorDetail = "upstream stream produced no event within the idle timeout"
		logger.Warn("stream cut by idle watchdog without terminal event",
			"model", publicModel, "key_id", authCtx.KeyID,
			"idle_timeout_ms", idleTimeout.Milliseconds(),
			"content_written", sink.WroteContent())
	} else if !sink.WroteContent() {
		logger.Warn("stream finished with no content",
			"model", publicModel, "key_id", authCtx.KeyID,
			"stop_reason", string(stopReason),
			"input_tokens", lastUsage.InputTokens,
			"output_tokens", lastUsage.OutputTokens,
			"reasoning_tokens", lastUsage.ReasoningTokens)
	}

	// 终止序列由 sink 按协议收尾：ok 时发终止事件（finish_reason+[DONE] 或
	// message_delta+message_stop），断流按 DESIGN §8.2 不发终止（OpenAI）/
	// 发 error 事件（Anthropic），canceled 不写任何东西。
	sink.Finish(status, stopReason, lastUsage, ing.WantsStreamUsage)

	latency := time.Since(start).Milliseconds()

	// 客户端中途断开时的用量兜底（2026-10-21 修 A4-P1-1 / 汇总 F1）。
	//
	// # 问题
	//
	// `lastUsage` 只在 EventMessageEnd 上赋值，而客户端断开时上游**不会**
	// 发出该事件 —— 于是 usage 恒为零值：
	//   - usage_state="missing" → cost_total=0 → 网关的上游成本被完全漏记；
	//   - trg_update_used_tokens 累加 0 → key 的终身 token 配额不涨；
	//   - rate.ReleaseQuota() 把 est 预占全额退回 → 预占也不涨。
	//
	// 三者叠加的结果是：**用户点一次「停止」，就能免费消耗已生成的全部
	// 上游 token，并且完全绕过终身 token 配额**。可被主动利用 ——
	// 收第一个 token 就断开，重复 N 次。
	//
	// # 为什么不能只靠「不退预占」解决
	//
	// 只不退预占能堵住配额绕过，但 cost_total 仍是 0：报表上的「上游成本」
	// 依旧被系统性低估（每条 canceled 记录都是 0），而 cost_total 无论
	// 成败都记是既有口径（那是网关的成本，不是用户的应收）。
	//
	// # 这里的处置
	//
	// 用**已经收到的输出字符数**折一个保守下界，作为本次调用的用量落库：
	//   - 输出 token 数 = 已收字符数按 1 token/4 字符（ASCII）折算，
	//     不足 1 个也算 1（有内容就至少 1 个 token）；
	//   - 输入 token 数用**请求前就有的估算值**（est 的输入部分）。
	//
	// 为什么不精确到分毫：客户端断开时上游到底生成了多少 token **无从得知**
	//（SDK 的 usage 只从终止事件派生，abort 路径不产生该事件）。
	// 用「已收到的字符数」是唯一有依据的下界 —— 它不会高估（收到的就是上游
	// 真吐出来的），也不会是 0。
	//
	// # 为什么不收用户钱
	//
	// 落库的 status 仍是 "canceled" → usageRecorder.charge 只对 status=="ok"
	// 扣费（见该函数注释）。所以这条路径**只影响成本口径与配额**，
	// 不会变成「用户为一次没拿到结果的调用付钱」。这与既有口径一致：
	// 扣费只看成功，成本无论成败都记。
	//
	// # 什么时候不兜底
	//
	// 上游报了 usage（!lastUsage.IsZero()）→ 用真实值，不兜底。
	// 一个字符都没收到（partialOutRunes==0）→ 兜底也兜不出什么，
	// 保持 missing —— 这时「什么都没生成」是事实，不该编一个数字。
	if lastUsage.IsZero() && partialOutRunes > 0 {
		estOut := ingress.PartialOutTokens(partialOutRunes)
		estIn := ingress.EstimateInputTokensOnly(ing.BuildRosetta())
		lastUsage = rosetta.Usage{
			InputTokens:  estIn,
			OutputTokens: estOut,
			TotalTokens:  estIn + estOut,
		}
		logger.Warn("stream ended without upstream usage; charging a conservative lower bound",
			"model", publicModel, "key_id", authCtx.KeyID,
			"status", status,
			"received_runes", partialOutRunes,
			"estimated_output_tokens", estOut,
			"estimated_input_tokens", estIn,
			"note", "客户端中断时上游不发终止事件，usage 无从得知；按已收内容折算下界")
	}

	// 配额预占的释放挂到落库之后（2026-10-22 修 A8-P1-1 / 汇总 M0）。
	// 先 Record 再 Commit 会让「预占已退、usage 还没落库」成为一个真实窗口：
	// 并发的下一个请求读不到本次用量，能多占一份额度。
	h.usage.RecordWithCallback(&store.UsageRecord{
		ID:                   newID(),
		AccessKeyID:          authCtx.KeyID,
		UserID:               authCtx.UserID,
		RequestID:            httpx.RequestIDFromContext(r.Context()),
		PublicModel:          publicModel,
		ProviderID:           cand.Provider.ID,
		ProviderName:         cand.Provider.Name,
		UpstreamModel:        cand.UpstreamModel.ModelID,
		IngressProtocol:      codec.Name(),
		UpstreamProtocol:     upstreamProtocol,
		Stream:               true,
		InputTokens:          lastUsage.InputTokens,
		OutputTokens:         lastUsage.OutputTokens,
		TotalTokens:          lastUsage.TotalTokens,
		ReasoningTokens:      lastUsage.ReasoningTokens,
		CachedTokens:         lastUsage.CachedInputTokens,
		CachedCreationTokens: lastUsage.CachedCreationTokens,
		UsageState:           usageStateFor(lastUsage),
		Status:               status,
		HTTPStatus:           httpStatus,
		ErrorCode:            errorCode,
		ErrorDetail:          errorDetail,
		LatencyMs:            latency,
		TTFBMs:               ttfbMs,
		// 流式已提交后的断流无法再转移，但「打到了哪个目标、为什么断」
		// 仍需可查。成功时留 ok，断流留对应状态。
		RouteTrace: strings.Join(AppendRouteTrace(priorTrace, len(priorTrace)+1, cand, StreamCommitOutcome(status), errorCode), "; "),
	}, func() {
		// 收尾按「usage 是否已知」分两条（2026-10-07 修复的 P1-3）：
		//   - 已知：全量收尾 —— TPM 以真实 usage（输入+输出）替换估算值，
		//     配额预占退回。
		//   - missing（上游没报任何 token 数）：**只退配额预占，TPM 预占故意保留**。
		//     commit(0) 会全额退还 TPM 预占，等于不报用量的上游完全绕过 TPM；
		//     保留估算值让限速继续约束这类流量，估算值随窗口翻转自然清零。
		//     但配额预占不能跟着一起跳过 —— 它是终身累计，预占按 max_tokens 全额
		//     计，不退就等于每次 missing 请求永久吃掉数万 token 额度（修复前整个
		//     commit 被跳过的正是这条路径）。
		//
		// 客户端中断的路径在上一步已经被兜底成「有 usage 了」（partialOutRunes>0
		// 时），所以走的是上面那条「已知」分支：TPM 按保守下界校正，
		// 终身配额的预占被**消耗**掉一部分（触发器按 total_tokens 累加），
		// 而不再全额退回 —— 那正是「点停止 = 免费刷上游 token」这条通道。
		if !lastUsage.IsZero() {
			rate.Commit(lastUsage.TotalTokens)
		} else {
			rate.ReleaseQuota()
		}
	})
	attempt.Status = status
	attempt.ErrorCode = errorCode
	attempt.ErrorDetail = errorDetail
	attempt.Committed = true
	attempt.Success = status == store.UsageStatusOK
	attempt.DurationMs = time.Since(attemptStart).Milliseconds()
	attempt.ToolCalls = maxInt(attempt.ToolCalls, 0)
	attempt.OutputRunes = partialOutRunes
	attempt.PostTTFBMs = time.Since(firstEventAt).Milliseconds()
	applyTraceTimings(&attempt, attemptTrace)
	out := Outcome{Kind: committedKindForStatus(status), Code: errorCode}.normalize()
	setAttemptDiagnosis(&attempt, out, terminalErr)
	diag.addAttempt(attempt)
	diag.setStreamOutput(partialOutRunes, attempt.ToolCalls, attempt.FirstToolCallMs)
	diag.setStreamMetrics(attempt.StreamEvents, attempt.MaxEventGapMs)
	diag.setUsage(lastUsage.OutputTokens)
	diag.setResult(status, errorCode, errorDetail)
	diag.setRouteTrace(strings.Join(AppendRouteTrace(priorTrace, len(priorTrace)+1, cand, StreamCommitOutcome(status), errorCode), "; "))
	diag.setTerminal(status == store.UsageStatusCanceled, status == store.UsageStatusTimeout)
	// committed=true（字节已写出、无法回退换目标）与「这一次算成功」是两件事：
	// truncated / overflow / error / canceled 都已提交，但对上游而言是失败。
	// 只有 status=="ok" 才允许外层记成功，否则「先200 再断流」这类最常见的
	// 上游故障永远不累计失败、目标永不熔断。
	// clientGone 只给「客户端自己走了」的那一种：它不该计入熔断计数
	// （见 Outcome.ClientGone）。
	return out
}

// priorTrace 的含义与 attemptStream 相同：提交后的落库在这里发生，
// 必须把此前目标的失败决策一并带上。
func (h *Handler) attemptNonStream(w http.ResponseWriter, r *http.Request, client *rosetta.Client, ing *ingress.Request, cand routing.Candidate, authCtx *auth.Context, codec ingress.Codec, rate *RateCommit, snap *snapshot.Snapshot, start time.Time, priorTrace []string, diag *requestDiagnostics, attemptTrace *upstream.AttemptTrace, attemptIndex int, attemptStart time.Time) Outcome {
	publicModel := ing.Model
	attempt := attemptFromCandidate(attemptIndex, cand, attemptTrace)
	upstreamProtocol := upstreamProtocolName(client.Protocol())
	attempt.Protocol = upstreamProtocol
	attempt.StartedAtMs = attemptStart.Sub(start).Milliseconds()
	buildStart := time.Now()
	// 同attemptStream：别名 → 上游 model_id 的映射与协议私有字段的透传
	// 必须在本次 attempt 内完成，不能由调用方造好传入。
	upstreamReq := ing.BuildRosetta()
	upstreamReq.Model = cand.UpstreamModel.ModelID
	ing.ApplyUpstreamExtras(upstreamReq, upstreamProtocol)
	ing.ApplyThinkingCapability(upstreamReq, cand.UpstreamModel, upstreamProtocol)
	// 与 attemptStream 同一刀，理由见 replay_gate.go。
	h.applyReplayGate(upstreamReq, upstreamProtocol)
	attempt.RequestBuildMs = time.Since(buildStart).Milliseconds()
	diag.setIngressRequest(upstreamReq, false)

	// 绑定 r.Context() 而非 context.Background()：客户端断开时上游调用应随之取消，
	// 否则断连请求会一直占用上游连接与配额直到超时（默认 120s）。
	ctx, cancel := context.WithTimeout(r.Context(), NonStreamTimeout(snap, h.cfg))
	ctx = withPromptCacheAffinity(ctx, ing, upstreamProtocol, conversationSessionID(r))
	defer cancel()

	callStart := time.Now()
	attempt.UpstreamCallStarted = true
	resp, err := client.Chat(ctx, upstreamReq)
	if err != nil {
		out := OutcomeFromErrCtx(r.Context(), err)
		attempt.UpstreamCallMs = time.Since(callStart).Milliseconds()
		finishAttempt(&attempt, attemptTrace, attemptStart, out, err, diag)
		return out
	}
	attempt.UpstreamCallMs = time.Since(callStart).Milliseconds()
	diag.setUpstreamResponse(resp.ID)

	// 非流式响应一次性返回，拿不到"首字"这一独立时刻，用响应到达时刻近似（≈总耗时）。
	latency := time.Since(start).Milliseconds()
	ttfbMs := latency

	writeResult := codec.WriteNonStream(w, resp, publicModel, ing)
	if writeResult.Err != nil {
		if writeResult.EncodeError() {
			return h.finishNonStreamEncodeFailure(
				w, r, cand, authCtx, codec, rate, publicModel,
				start, priorTrace, diag, &attempt, attemptTrace, attemptStart,
				writeResult,
			)
		}
		return h.finishNonStreamWriteFailure(
			r, resp, cand, authCtx, codec, rate, publicModel, upstreamProtocol,
			start, priorTrace, diag, &attempt, attemptTrace, attemptStart,
			latency, ttfbMs, writeResult,
		)
	}

	h.usage.RecordWithCallback(&store.UsageRecord{
		ID:                   newID(),
		AccessKeyID:          authCtx.KeyID,
		UserID:               authCtx.UserID,
		RequestID:            httpx.RequestIDFromContext(r.Context()),
		PublicModel:          publicModel,
		ProviderID:           cand.Provider.ID,
		ProviderName:         cand.Provider.Name,
		UpstreamModel:        cand.UpstreamModel.ModelID,
		IngressProtocol:      codec.Name(),
		UpstreamProtocol:     upstreamProtocol,
		Stream:               false,
		InputTokens:          resp.Usage.InputTokens,
		OutputTokens:         resp.Usage.OutputTokens,
		TotalTokens:          resp.Usage.TotalTokens,
		ReasoningTokens:      resp.Usage.ReasoningTokens,
		CachedTokens:         resp.Usage.CachedInputTokens,
		CachedCreationTokens: resp.Usage.CachedCreationTokens,
		UsageState:           usageStateFor(resp.Usage),
		Status:               store.UsageStatusOK,
		HTTPStatus:           200,
		LatencyMs:            latency,
		TTFBMs:               ttfbMs,
		RouteTrace:           strings.Join(AppendRouteTrace(priorTrace, len(priorTrace)+1, cand, "ok", ""), "; "),
	}, func() {
		// 收尾口径与 attemptStream 一致（见该处的注释）：usage 已知 → 全量收尾；
		// missing → 只退配额预占，TPM 预占保留（窗口自愈）。
		if !resp.Usage.IsZero() {
			rate.Commit(resp.Usage.TotalTokens)
		} else {
			rate.ReleaseQuota()
		}
	})
	attempt.Status = store.UsageStatusOK
	attempt.DurationMs = time.Since(attemptStart).Milliseconds()
	attempt.TTFBMs = attempt.UpstreamCallMs
	attempt.Committed = true
	attempt.Success = true
	success := Outcome{Kind: OutcomeCommittedSuccess}.normalize()
	finishAttempt(&attempt, attemptTrace, attemptStart, success, nil, diag)
	diag.setUsage(resp.Usage.OutputTokens)
	diag.setResult(store.UsageStatusOK, "", "")
	diag.setRouteTrace(strings.Join(AppendRouteTrace(priorTrace, len(priorTrace)+1, cand, "ok", ""), "; "))
	// 非流式能走到这里就意味着上游完整返回了响应 —— 一定是成功。
	// 漏写 Success 会让每次非流式成功都被外层记成「目标失败」，
	// 健康的链首目标 3 个请求后就被误熔断（详见 Outcome.Success）。
	return success
}

func (h *Handler) finishNonStreamEncodeFailure(
	w http.ResponseWriter,
	r *http.Request,
	cand routing.Candidate,
	authCtx *auth.Context,
	codec ingress.Codec,
	rate *RateCommit,
	publicModel string,
	start time.Time,
	priorTrace []string,
	diag *requestDiagnostics,
	attempt *attemptDiagnostics,
	attemptTrace *upstream.AttemptTrace,
	attemptStart time.Time,
	writeResult outwire.NonStreamWriteResult,
) Outcome {
	code := "response_encode_failed"
	message := "gateway failed to encode the upstream response"
	detail := outwire.SafeErrorDetail(writeResult.Err)
	h.logger.Error("non-stream response encoding failed",
		"model", publicModel, "key_id", authCtx.KeyID, "error", writeResult.Err)
	codec.WriteError(w, http.StatusInternalServerError, code, message)

	h.usage.Record(&store.UsageRecord{
		ID:               newID(),
		AccessKeyID:      authCtx.KeyID,
		UserID:           authCtx.UserID,
		RequestID:        httpx.RequestIDFromContext(r.Context()),
		PublicModel:      publicModel,
		ProviderID:       cand.Provider.ID,
		ProviderName:     cand.Provider.Name,
		UpstreamModel:    cand.UpstreamModel.ModelID,
		IngressProtocol:  codec.Name(),
		UpstreamProtocol: h.effectiveProviderProtocol(cand),
		Stream:           false,
		UsageState:       store.UsageStateNone,
		Status:           store.UsageStatusError,
		HTTPStatus:       http.StatusInternalServerError,
		ErrorCode:        code,
		ErrorDetail:      detail,
		LatencyMs:        time.Since(start).Milliseconds(),
		RouteTrace: strings.Join(AppendRouteTrace(
			priorTrace, len(priorTrace)+1, cand, "fail_terminal", code), "; "),
	})
	rate.Commit(0)

	out := Outcome{Kind: OutcomeEncodeFailed, Code: code}.normalize()
	finishAttempt(attempt, attemptTrace, attemptStart, out, writeResult.Err, diag)
	diag.setResult(store.UsageStatusError, code, detail)
	diag.setRouteTrace(strings.Join(AppendRouteTrace(
		priorTrace, len(priorTrace)+1, cand, "fail_terminal", code), "; "))
	diag.setTerminal(false, false)
	return out
}

func (h *Handler) finishNonStreamWriteFailure(
	r *http.Request,
	resp *rosetta.ChatResponse,
	cand routing.Candidate,
	authCtx *auth.Context,
	codec ingress.Codec,
	rate *RateCommit,
	publicModel, upstreamProtocol string,
	start time.Time,
	priorTrace []string,
	diag *requestDiagnostics,
	attempt *attemptDiagnostics,
	attemptTrace *upstream.AttemptTrace,
	attemptStart time.Time,
	latency, ttfbMs int64,
	writeResult outwire.NonStreamWriteResult,
) Outcome {
	writeErr := writeResult.Err
	status := store.UsageStatusCanceled
	errorCode := "downstream_write_failed"
	errorDetail := outwire.SafeErrorDetail(writeErr)
	httpStatus := http.StatusOK
	logger := h.logger

	if outwire.IsDownstreamWriteError(writeErr) {
		logger.Info("downstream write failed after non-stream response",
			"model", publicModel, "key_id", authCtx.KeyID, "error", writeErr)
	} else {
		logger.Error("unexpected non-stream response write error",
			"model", publicModel, "key_id", authCtx.KeyID, "error", writeErr)
	}

	h.usage.RecordWithCallback(&store.UsageRecord{
		ID:                   newID(),
		AccessKeyID:          authCtx.KeyID,
		UserID:               authCtx.UserID,
		RequestID:            httpx.RequestIDFromContext(r.Context()),
		PublicModel:          publicModel,
		ProviderID:           cand.Provider.ID,
		ProviderName:         cand.Provider.Name,
		UpstreamModel:        cand.UpstreamModel.ModelID,
		IngressProtocol:      codec.Name(),
		UpstreamProtocol:     upstreamProtocol,
		Stream:               false,
		InputTokens:          resp.Usage.InputTokens,
		OutputTokens:         resp.Usage.OutputTokens,
		TotalTokens:          resp.Usage.TotalTokens,
		ReasoningTokens:      resp.Usage.ReasoningTokens,
		CachedTokens:         resp.Usage.CachedInputTokens,
		CachedCreationTokens: resp.Usage.CachedCreationTokens,
		UsageState:           usageStateFor(resp.Usage),
		Status:               status,
		HTTPStatus:           httpStatus,
		ErrorCode:            errorCode,
		ErrorDetail:          errorDetail,
		LatencyMs:            latency,
		TTFBMs:               ttfbMs,
		RouteTrace: strings.Join(AppendRouteTrace(
			priorTrace, len(priorTrace)+1, cand, "committed_fail", errorCode), "; "),
	}, func() {
		if !resp.Usage.IsZero() {
			rate.Commit(resp.Usage.TotalTokens)
		} else {
			rate.ReleaseQuota()
		}
	})

	out := Outcome{Kind: OutcomeDownstreamWriteFailed, Code: errorCode}.normalize()
	attempt.Status = status
	attempt.ErrorCode = errorCode
	attempt.ErrorDetail = errorDetail
	attempt.DurationMs = time.Since(attemptStart).Milliseconds()
	attempt.TTFBMs = attempt.UpstreamCallMs
	attempt.Committed = true
	attempt.Success = false
	finishAttempt(attempt, attemptTrace, attemptStart, out, writeErr, diag)
	diag.setUsage(resp.Usage.OutputTokens)
	diag.setResult(status, errorCode, errorDetail)
	diag.setRouteTrace(strings.Join(AppendRouteTrace(
		priorTrace, len(priorTrace)+1, cand, "committed_fail", errorCode), "; "))
	diag.setTerminal(true, false)
	return out
}

// usageStateFor 区分「上游报了用量」与「上游一个 token 数都没给」。
//
// 必须区分：上游不报 usage（第三方兼容服务常见）时若按 "reported" 落 0 token，
// 配额、费用报表、TPM 校正会把系统性漏账当成正常数据，且事后无法从库里分辨。
// SDK 的 Usage.IsZero 把 cached/reasoning 也计入 —— 只报缓存命中或思考 token
// 也算「报了」，否则会丢掉真实数字（见 SDK usage.go 的注释）。
func usageStateFor(u rosetta.Usage) string {
	if u.IsZero() {
		return store.UsageStateMissing
	}
	return store.UsageStateReported
}
