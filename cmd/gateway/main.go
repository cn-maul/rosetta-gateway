package main

import (
	"context"
	crand "crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/cn-maul/rosetta-gateway/internal/auth"
	"github.com/cn-maul/rosetta-gateway/internal/authhttp"
	"github.com/cn-maul/rosetta-gateway/internal/dataplane"
	"github.com/cn-maul/rosetta-gateway/internal/gatewayapp"
	"github.com/cn-maul/rosetta-gateway/internal/outwire"
	"github.com/cn-maul/rosetta-gateway/internal/routing"
	"github.com/cn-maul/rosetta-gateway/internal/runtime"
	"github.com/cn-maul/rosetta-gateway/internal/server"
	"github.com/cn-maul/rosetta-gateway/internal/snapshot"
)

// homeEnvVar 是状态根目录的环境变量名。
//
// 容器镜像靠它把 config.json、master.key、session_secret 与数据库整体
// 指到挂载卷上，让镜像层保持无状态（见 DOCKER.md）。
const homeEnvVar = gatewayapp.HomeEnvVar

// buildVersion 由构建期注入：-ldflags "-X main.buildVersion=x.y.z"。
// 未注入时为 "dev"。取值与前端页脚的 __APP_VERSION__ 同源
// —— 都来自 web/package.json 的 version，由 CI 打镜像时统一传入。
var buildVersion = "dev"

// healthzPinger 是健康检查需要的最小存储能力。
//
// 抽成接口只有一个理由：**可测**。要验证「数据库不可用时响应体不得
// 回显底层错误」，就得能造一个 ping 失败的存储 —— 真把数据库弄坏
// 不可靠（Windows 上 SQLite 的文件锁行为与 Linux 不同），而这里要测的
// 恰恰是错误路径。
type healthzPinger interface {
	Ping(ctx context.Context) error
}

// healthzDirty 是「运行时是否落后于数据库」的判据。
//
// 同样抽出来是为了可测：reloader 的 dirty 标志是 unexported 字段，
// 而构造一个真的「重建失败过」的 runtimeReloader 需要整套 DB + 池。
type healthzDirty interface {
	IsDirty() bool
}

// healthzHandler 返回 GET /healthz 的处理器。
//
// # 为什么错误体不回显底层错误（2026-10-21 修 A10-P1-4）
//
// 这个端点是**免鉴权**的：探针要在没有会话、没有 token 的情况下可用。
// 而 SQLite 的错误字符串可以含数据库路径、驱动内部信息、文件系统错误
// （store.Ping 就会把写池/读池的原始错误包进来）。同网段的主动探测者
// 能拿它做环境枚举与故障定位。
//
// 现在对外只有两个稳定字段：
//   - status：ok / degraded / unhealthy；
//   - error_code：一个不随实现漂移的机器可读代码。
//
// 完整错误**只进服务端日志** —— 需要诊断细节的运维看日志，
// 而匿名探测者只能看到一个「数据库不可用」。
//
// # 三种状态的语义
//
//   - ok：两个连接池都通，且运行时没落后。200。
//   - degraded：数据库可用，但运行时落后于数据库（上次重建失败）。
//     报 503 让编排系统有机会介入，同时区别于「进程死了」。
//   - unhealthy：数据库不可用。503。
func healthzHandler(db healthzPinger, reloader healthzDirty, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		body := map[string]any{"status": "ok"}
		code := http.StatusOK
		if err := db.Ping(ctx); err != nil {
			// 完整错误只进日志，对外只给一个稳定代码。
			logger.Error("healthz: database ping failed", "error", err)
			body = map[string]any{"status": "unhealthy", "error_code": "database_unavailable"}
			code = http.StatusServiceUnavailable
		} else if reloader.IsDirty() {
			// 数据库可用、运行时落后：进程还活着但配置未生效。
			body = map[string]any{"status": "degraded", "error_code": "runtime_lags_behind_database"}
			code = http.StatusServiceUnavailable
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(body)
	}
}

func main() {
	if err := gatewayapp.Run(gatewayapp.Options{
		Version:      buildVersion,
		BuildHandler: buildHandlerFromApp,
		Bootstrap:    initializeAdminBootstrap,
	}); err != nil {
		slog.Error("gateway exited", "error", err)
		os.Exit(1)
	}
}

// runtimeReloader 保留 cmd 内既有调用名；实现归 internal/runtime。
//
// Reloader itself lives in internal/runtime so routes and tests can share it
// without depending on package main.
type runtimeReloader = runtime.Reloader

// errorWriter 是「按下游协议写出错误响应」的统一签名。
type errorWriter = authhttp.ErrorWriter

// denyAdminKeyCreate 拦截**管理员**调用 POST /admin/api/keys（建 key）。
//
// # 需求（2026-10 控制面/数据面分离）
//
// 「普通用户自助建 key，管理员不能建」。管理员只做控制面管理；要调模型，
// 需先建普通用户、由普通用户自己发 key（与管理员不能调 /v1 配套）。
//
// # 为什么在**路由/中间件层**拦，而不改 internal/admin/key_handler.go 的 Create
//
//  1. 改动面最小、边界最清晰：这是一条**路由级策略**，只针对「POST /admin/api/keys」
//     这一个端点。写在 handler 里会与 Create 内部那一大段「自助建 key 的额度封顶」
//     逻辑缠在一起，而那段的语义（谁能建、建出来多少额度）与本策略（谁**不允许**
//     建）其实是两件事，混在一起反而更难读。
//  2. 依赖的鉴权上下文本来就在这一层可用：AdminGateGuard / UserAuth 已把登录用户
//     注入 context（server.UserFromContext），路由层读它即可，无需再查库。
//  3. Create 仍保持「普通用户自助」的原逻辑不动，回归风险最低。
//
// 换来的约束：这条策略绑定在「main.go 注册的这条路由」上。若将来另有代码用
// KeyHandler.Create 组新路由，必须记得同样套上本包装（或改到 handler 层）。
// 已在下方注册处留下注释标注，避免后人漏掉。
//
// # 为什么管理员仍能**管理别人的** key（PATCH/DELETE/列表）
//
// 本策略只拦「新建」这一个动作。管理员照旧可以：列出全部 key、把已有的 key
// 认领给某个用户（Update 的 user_id 路径）、禁用/启用/改配额/改分组覆盖等。
// 也就是说管理员握有对存量 key 的**完整治理权**，只是不再亲手铸造新的数据面凭据。
// 这正是需求要的「管理员只负责管理网关」。
//
// 身份缺失一律 401（fail-closed）：拿不到登录身份绝不等于「当作普通用户放行」，
// 那会让匿名请求绕过本策略。与 key_handler.Create 的同一处口径一致。
func denyAdminKeyCreate(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u := server.UserFromContext(r.Context())
		if u == nil || u.ID == "" {
			// 空 ID 一律拒绝：绝不能让「拿不到身份」被当成「非管理员」而放行。
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"message":"需要登录","type":"auth_error"}}`))
			return
		}
		if u.IsAdmin() {
			// 403（不是 401）：管理员身份是**已确认**的，不允许只是策略限制，
			// 与数据面拦截管理员调 /v1 的口径一致。消息明确指向正确出路
			// （建普通用户），避免管理员以为 key 创建坏了而去排查无关配置。
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":{"message":"管理员账号不能创建 API key：请新建一个普通用户账户，由该用户自行创建 key 后用于调用模型","type":"auth_error"}}`))
			return
		}
		next(w, r)
	}
}

func handleListModels(forceShape string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// D9 分流（DESIGN §6.1）：显式别名路径永远优先；/v1/models 按认证头
		// 分流 —— 带 x-api-key 且不带 Authorization: Bearer 的请求按 Anthropic
		// 形状返回，其余按 OpenAI 形状。
		shape := forceShape
		if shape == "" {
			if r.Header.Get("X-Api-Key") != "" && !hasBearer(r) {
				shape = "anthropic"
			} else {
				shape = "openai"
			}
		}
		writeErr := errorWriter(outwire.WriteOpenAIError)
		if shape == "anthropic" {
			writeErr = outwire.WriteAnthropicError
		}

		// 与转发入口同一鉴权口径（官方 OpenAI / Anthropic 的 /v1/models 同样要求
		// 认证）。这里不查配额 —— 列个目录不消耗 token —— 但必须校验密钥：
		// 否则任何人都能枚举出全部公开模型名，等于白送一份路由与供应商结构图。
		authCtx, err := auth.Authenticate(r)
		if err != nil {
			authhttp.WriteAuthError(w, err, writeErr)
			return
		}

		snap := snapshot.Get()
		// 轨道一：虚拟名。?include=upstream 时追加轨道二的 slug/model 形式
		// （可能非常长且随 provider 增长，默认不列 —— DESIGN §5.2）。
		ids := make([]string, 0, 16)
		seen := make(map[string]bool)
		for _, route := range snap.Routes.ListRoutes() {
			// 模型白名单（多用户改造 P1）：列表就是「你能用什么」的权威答案，
			// 必须与转发路径用**同一套判定**，否则会出现「列表里有、调起来 403」
			// 或者反过来「能调但列表不显示」。
			if route.Enabled && authCtx.AllowsModel(route.PublicName) && !seen[route.PublicName] {
				seen[route.PublicName] = true
				ids = append(ids, route.PublicName)
			}
		}
		if r.URL.Query().Get("include") == "upstream" {
			for _, m := range snap.Routes.ListUpstreamModels() {
				id := m.ProviderSlug + "/" + m.ModelID
				// 直连标识符与公开模型名是**同一个命名空间里的字符串**：
				// 白名单里出现哪个就放行哪个。这里直接用 id 判定即可 ——
				// routing.Resolve 对这类形式会合成一条 PublicName 恰为 id 的
				// route，所以「解析后判定」与「按 id 判定」完全等价，
				// 多走一次 Resolve 只是白费。
				if m.Enabled && !seen[id] && authCtx.AllowsModel(id) {
					seen[id] = true
					ids = append(ids, id)
				}
			}
		}

		w.Header().Set("Content-Type", "application/json")
		if shape == "anthropic" {
			entries := make([]map[string]any, 0, len(ids))
			for _, id := range ids {
				// created_at：网关不为模型持久化创建时间，用固定纪元占位
				//（客户端不消费该字段，保持确定性比编一个值诚实）。
				entry := map[string]any{
					"type": "model", "id": id, "display_name": id,
					"created_at": "1970-01-01T00:00:00Z",
				}
				appendModelMetadata(entry, snap, id)
				entries = append(entries, entry)
			}
			var first, last any
			if len(entries) > 0 {
				first, last = entries[0]["id"], entries[len(entries)-1]["id"]
			}
			json.NewEncoder(w).Encode(map[string]any{
				"data": entries, "has_more": false, "first_id": first, "last_id": last,
			})
			return
		}

		entries := make([]map[string]any, 0, len(ids))
		for _, id := range ids {
			entry := map[string]any{
				"id": id, "object": "model", "created": 0, "owned_by": "gateway",
			}
			appendModelMetadata(entry, snap, id)
			entries = append(entries, entry)
		}
		json.NewEncoder(w).Encode(map[string]any{
			"object": "list",
			"data":   entries,
		})
	}
}

// appendModelMetadata 把「解析后的链首上游模型」的容量写进模型条目：
// context_length（OpenRouter 约定）、max_input_tokens / max_output_tokens
// （LiteLLM 约定）。上游模型未单独覆盖时回落设置页的模型容量默认；
// 两级都为 0 则不加字段（不编造数字）。
func appendModelMetadata(entry map[string]any, snap *snapshot.Snapshot, modelID string) {
	cw, mo := modelCapacity(snap, modelID)
	if cw > 0 {
		entry["context_length"] = cw
		entry["max_input_tokens"] = cw
	}
	if mo > 0 {
		entry["max_output_tokens"] = mo
	}
	// 思考能力：这是工具侧「是否支持思考模式」那个开关的数据来源。
	//
	// # 披露口径（2026-10-14 改：nil 从「不发」改为「发 true」）
	//
	// 三种落点，每一种客户端都有明确答案：
	//
	//   - 配了 effort_levels → 下发 supported_reasoning，工具据此生成完整
	//     的档位下拉框。
	//   - supports_thinking 为 nil（未配置）→ **下发 true**。
	//   - supports_thinking 显式 false → 下发 false，工具隐藏思考开关。
	//
	// 为什么 nil 要发 true 而不是不发（原来的做法）：
	//
	// 缺席对客户端不是中立的。OpenRouter 约定里这个字段缺席时，多数工具按
	// 「不支持」处理，于是「未配置」在下游就等于「没有思考能力」—— 而网关
	// 自己的语义明明是「不干预，交给上游」。两边对同一个 nil 的解释相反，
	// 且用户看到的现象是「我明明能思考，工具却不给我开」。
	//
	// 发 true 也**不等于**替上游承诺档位：supported_reasoning 缺席仍然表示
	// 「档位由上游定」，那正是未配置状态的准确描述。两字段组合起来正好
	// 覆盖三种配置，无需第四种取值。
	//
	// 关键：这里**只影响披露**，数据面不动。数据面仍然只在
	// supports_thinking 显式为 false 时才剥掉思考（见
	// ingress.Request.ApplyThinkingCapability）—— 把「不知道」当成「不支持」会让全部未配置
	// 模型（包括 anthropic 协议的）静默失去思考能力，而那正是本函数
	// 下面那段旧注释警告过的事。
	if m := modelThinkingCapability(snap, modelID); m != nil {
		// nil（未配置）→ true：向客户端表明「可以思考」，但不下发档位集合。
		supports := true
		if m.SupportsThinking != nil {
			supports = *m.SupportsThinking
		}
		entry["supports_thinking"] = supports
		if len(m.EffortLevels) > 0 {
			// 键名取 OpenRouter 的 supported_reasoning —— 那是这条链路上已有的
			// 既成约定，字段形状（字符串数组）也相同。
			out := make([]string, 0, len(m.EffortLevels))
			for _, l := range m.EffortLevels {
				out = append(out, string(l))
			}
			entry["supported_reasoning"] = out
		}
	}
}

// modelThinkingCapability 取对外模型名解析后**链首**上游模型的思考能力。
//
// 只看链首：故障转移到别的目标时数据面会按那个目标的能力重新处理（见
// ingress.Request.ApplyThinkingCapability），所以这里披露的是「默认会落到
// 哪一档」，而不是「这条链所有可能的档位集合」—— 后者是个会误导客户端的并集。
func modelThinkingCapability(snap *snapshot.Snapshot, modelID string) *routing.UpstreamModel {
	res, err := snap.Routes.Resolve(modelID)
	if err != nil || len(res.Candidates) == 0 {
		return nil
	}
	return res.Candidates[0].UpstreamModel
}

// modelCapacity 解析一个对外模型名到链首上游模型的容量。
func modelCapacity(snap *snapshot.Snapshot, modelID string) (contextWindow, maxOutput int) {
	res, err := snap.Routes.Resolve(modelID)
	if err != nil || len(res.Candidates) == 0 {
		return 0, 0
	}
	m := res.Candidates[0].UpstreamModel
	if m == nil {
		return 0, 0
	}
	cw, mo := m.ContextWindow, m.MaxOutputTokens
	if cw <= 0 {
		cw = snap.Runtime.DefaultContextWindow
	}
	if mo <= 0 {
		mo = snap.Runtime.DefaultMaxOutputTokens
	}
	return cw, mo
}

// handleGetModel 返回单个对外模型的详情（OpenAI /v1/models/{id} 形状 + 容量
// 元数据）。轨道一的虚拟名与轨道二的 slug/model 都可查。
func handleGetModel(forceShape string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		shape := forceShape
		if shape == "" {
			if r.Header.Get("X-Api-Key") != "" && !hasBearer(r) {
				shape = "anthropic"
			} else {
				shape = "openai"
			}
		}
		writeErr := errorWriter(outwire.WriteOpenAIError)
		if shape == "anthropic" {
			writeErr = outwire.WriteAnthropicError
		}
		authCtx, err := auth.Authenticate(r)
		if err != nil {
			authhttp.WriteAuthError(w, err, writeErr)
			return
		}

		modelID := r.PathValue("model")
		snap := snapshot.Get()
		res, rerr := snap.Routes.Resolve(modelID)
		if rerr != nil {
			writeErr(w, http.StatusNotFound, "model_not_found", dataplane.ModelNotFoundMessage(snap, modelID))
			return
		}
		// 模型白名单（P1）：校验**解析后**的公开模型名，而不是 URL 原文。
		// 虚拟名与 provider/model 两种写法都解析到同一条路由，只查原文
		// 会让 slug 形式绕过白名单。
		if !authCtx.AllowsModel(res.Route.PublicName) {
			writeErr(w, http.StatusForbidden, "model_not_allowed",
				fmt.Sprintf("model %q is not available for this API key", modelID))
			return
		}

		w.Header().Set("Content-Type", "application/json")
		if shape == "anthropic" {
			entry := map[string]any{
				"type": "model", "id": modelID, "display_name": modelID,
				"created_at": "1970-01-01T00:00:00Z",
			}
			appendModelMetadata(entry, snap, modelID)
			json.NewEncoder(w).Encode(entry)
			return
		}
		entry := map[string]any{
			"id": modelID, "object": "model", "created": 0, "owned_by": "gateway",
		}
		appendModelMetadata(entry, snap, modelID)
		json.NewEncoder(w).Encode(entry)
	}
}

// hasBearer 报告请求是否带 Authorization: Bearer 头（D9 分流用，大小写不敏感）。
func hasBearer(r *http.Request) bool {
	const prefix = "Bearer "
	auth := r.Header.Get("Authorization")
	return len(auth) > len(prefix) && strings.EqualFold(auth[:len(prefix)], prefix)
}

func generateID() string {
	b := make([]byte, 16)
	crand.Read(b)
	return hex.EncodeToString(b)
}
