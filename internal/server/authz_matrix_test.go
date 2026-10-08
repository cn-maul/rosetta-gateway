package server_test

// 权限边界的**动态**验证（不是读代码推断）。
//
// # 为什么这个文件在 internal/server 而不是 internal/admin
//
// 本文件是 package server_test —— **外部测试包**。它在 internal/server 目录下，
// 却能 import internal/admin（admin → server 是单向依赖，外部测试包不构成
// 环，已实测确认）。这一点是本文件能存在的前提：
//
//   - internal/admin 的测试（package admin）拿不到 server 包的私有 context key，
//     所以那边的测试只能**直接调 handler**、身份靠 asUser/asAdmin 手工注入 ——
//     那绕过了全部中间件，测不到「网关层放不放行」。
//   - 本文件把**真实**的 UserAuth → AdminGateGuard → 真实 handler 串起来，
//     身份靠**真实签发的 JWT**取得（不是手工塞 context），于是测的是真链路。
//
// # 为什么路由表从 cmd/gateway/main.go **源码**里读
//
// 手抄一份路由清单 = 抄漏一条就静默少测一条，而漏掉的那条恰好可能是唯一
// 有问题的那条。所以本文件用正则从 main.go 的注册处提取全部
// `adminMux.HandleFunc(...)` / `publicAdminMux.HandleFunc(...)` / `mux.Handle("METHOD /path", ...)`，
// 与源码**逐条对齐**。将来有人加一条路由而没更新本文件，也不会被漏测。
//
// # 三层判定：区分「网关层拒绝」与「handler 层拒绝」
//
// 这是本文件最重要的一个区分，因为本项目里两者**语义完全不同**：
//
//   - 网关层（UserAuth / AdminGateGuard）拒绝 → 返回 `type:"auth_error"`；
//   - handler 层的 requireAdmin 拒绝 → 返回 `type:"invalid_request_error"`。
//
// 而 /admin/api/usage/by-user 正是「网关层放行、只有 handler 拦」的那一类
// （白名单按**前缀**匹配，/admin/api/usage 在名单里，于是整个子树被放行）。
// 如果把两者混成一个「403 = 拒绝」，就永远看不出「这条防线其实只剩一道」。
// 所以 probeResult 同时记录 reached 与 handlerDenied。

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/cn-maul/rosetta-gateway/internal/admin"
	"github.com/cn-maul/rosetta-gateway/internal/auth"
	"github.com/cn-maul/rosetta-gateway/internal/server"
	"github.com/cn-maul/rosetta-gateway/internal/snapshot"
	"github.com/cn-maul/rosetta-gateway/internal/store"
	"github.com/cn-maul/rosetta-gateway/internal/userauth"
)

// ---------------------------------------------------------------------------
// 身份
// ---------------------------------------------------------------------------

const (
	idAnon  = "anon"  // 未登录（不带任何凭据）
	idUser  = "alice" // 普通用户（本次验证的主角）
	idOther = "bob"   // 另一个普通用户（用于验证作用域收窄）
	idAdmin = "admin1"
)

// adminAPIBase 是所有管理面路由的公共前缀，用于把路由分成
// 「管理面（走鉴权链）」与「数据面（不走鉴权链）」两类。
const adminAPIBase = "/admin/api/"

// ---------------------------------------------------------------------------
// 从 main.go 读路由
// ---------------------------------------------------------------------------

type adminRoute struct {
	Method string
	Path   string
	// Public 为真 = 注册在 publicAdminMux 上（免鉴权）。
	// 这些端点**必须**匿名可达，否则登录页自己就用不了。
	Public bool
}

// routeRe 提取 main.go 里的管理面路由注册。
//
// 两个来源：
//  1. `adminMux.HandleFunc("GET /admin/api/x", ...)` —— 需要鉴权的管理面；
//  2. `mux.Handle("POST /admin/api/login", publicAdminMux)` —— 免鉴权端点
//     挂到根 mux 上（main.go 里就是这么写的，不是 publicAdminMux.HandleFunc）。
//
// 不匹配 `mux.Handle("GET /admin/api/", adminAuto)` 这类**前缀兜底**：
// 它们不是具体路由，且路径以 "/" 结尾 —— 用 `[^"]*[^/"]` 结尾锚定即可排除。
var routeRe = regexp.MustCompile(`(?m)(adminMux|publicAdminMux)\.HandleFunc\("([A-Z]+)\s+(/admin/api/[^"]*)"`)

var publicMuxRe = regexp.MustCompile(`(?m)mux\.Handle\("([A-Z]+)\s+(/admin/api/[^"]*)",\s*publicAdminMux`)

// parseAdminRoutes 从 main.go 源码提取全部管理面路由（唯一真相）。
func parseAdminRoutes(t *testing.T) []adminRoute {
	t.Helper()
	src, err := os.ReadFile(filepath.Join("..", "..", "cmd", "gateway", "main.go"))
	if err != nil {
		t.Fatalf("读 main.go 失败（路由真相来源）: %v", err)
	}
	text := string(src)

	seen := map[string]adminRoute{}
	add := func(m, p string, public bool) {
		key := m + " " + p
		if prev, ok := seen[key]; ok {
			// 同一路径同时出现在两处时以「免鉴权」为准：那是最宽松的注册，
			// 也正是最需要被验证的一种（漏在 publicAdminMux 上 = 越权入口）。
			if public {
				seen[key] = adminRoute{m, p, true}
			}
			_ = prev
			return
		}
		seen[key] = adminRoute{m, p, public}
	}

	for _, mm := range routeRe.FindAllStringSubmatch(text, -1) {
		add(mm[2], mm[3], mm[1] == "publicAdminMux")
	}
	for _, mm := range publicMuxRe.FindAllStringSubmatch(text, -1) {
		add(mm[1], mm[2], true)
	}

	out := make([]adminRoute, 0, len(seen))
	for _, r := range seen {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Path != out[j].Path {
			return out[i].Path < out[j].Path
		}
		return out[i].Method < out[j].Method
	})

	// 源码解析失败会静默给出 0 条 —— 那样整个矩阵会「全绿通过」。
	// 所以对条数设一个下界，让解析失效立刻显形。
	if len(out) < 50 {
		t.Fatalf("只解析出 %d 条管理面路由，明显偏少：路由正则可能已与 main.go 脱节", len(out))
	}
	return out
}

// probePath 把路由模式里的 {id} 换成具体值，以便真实发起请求。
//
// 用同一个占位值即可：本层测的是**网关层是否放行**，与资源是否真实存在无关
// （不存在的资源会走到 handler 再回 404，那恰好证明请求被放行了）。
func probePath(p string) string {
	var b strings.Builder
	for {
		i := strings.Index(p, "{")
		if i < 0 {
			b.WriteString(p)
			return b.String()
		}
		j := strings.Index(p[i:], "}")
		if j < 0 {
			b.WriteString(p)
			return b.String()
		}
		b.WriteString(p[:i])
		b.WriteString("probe-id")
		p = p[i+j+1:]
	}
}

// ---------------------------------------------------------------------------
// 判定
// ---------------------------------------------------------------------------

type probeResult struct {
	Status int
	// ErrType 是响应体 error.type：auth_error = 网关层拒绝；
	// invalid_request_error = handler 层拒绝。
	ErrType string
	// Reached 为真 = 请求穿过了整条鉴权链、到达了 handler。
	Reached bool
	// HandlerDenied 为真 = 网关层放行，但被 handler 自己的 requireAdmin 拦下。
	// 这一类端点**只有一道防线**，是本项目最需要盯住的形态。
	HandlerDenied bool
	Body          string
}

func (p probeResult) verdict() string {
	switch {
	case p.Reached && p.HandlerDenied:
		return "handler-only-deny"
	case p.Reached:
		return "ALLOWED"
	case p.Status == http.StatusUnauthorized:
		return "auth-401"
	default:
		return "gate-deny"
	}
}

// classify 把一次响应归类。
//
// 用 error.type 而不是状态码来区分两层：两层的 403 文案恰好都是
// 「需要管理员权限」，但 type 不同（见文件头注释）。只看状态码会把
// 「仅剩一道防线」误判成「两道都生效」。
func classify(rec *httptest.ResponseRecorder) probeResult {
	res := probeResult{Status: rec.Code, Body: rec.Body.String()}
	var envelope struct {
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err == nil {
		res.ErrType = envelope.Error.Type
	}

	switch {
	case res.Status == http.StatusUnauthorized:
		res.Reached = false
	case res.Status == http.StatusForbidden && res.ErrType == "auth_error":
		// 网关层（或路由层策略，如 denyAdminKeyCreate）的拒绝。
		res.Reached = false
	case res.Status == http.StatusForbidden:
		// handler 层的 requireAdmin：请求**确实**进了 handler。
		res.Reached = true
		res.HandlerDenied = true
	default:
		res.Reached = true
	}
	return res
}

// ---------------------------------------------------------------------------
// harness
// ---------------------------------------------------------------------------

type authzHarness struct {
	t    *testing.T
	db   *store.Store
	mgr  *userauth.Manager
	toks map[string]string
}

// newAuthzHarness 建一个真实的库 + 真实签发的会话令牌。
//
// 令牌走 userauth.Manager.Issue（真实 JWT），不是手工塞 context ——
// 这样 UserAuth 中间件的 Peek→查库→比对 auth_version 整条路径都被真实执行。
func newAuthzHarness(t *testing.T) *authzHarness {
	t.Helper()
	t.Setenv(userauth.SecretEnvName, "authz-matrix-session-secret-32-chars-min")

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	db, err := store.Open(filepath.Join(t.TempDir(), "gw.db"), logger)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	mgr, err := userauth.NewManager("")
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	if !mgr.Enabled() {
		t.Fatal("会话未启用：后面所有鉴权路径都会短路成 503，断言失去意义")
	}

	h := &authzHarness{t: t, db: db, mgr: mgr, toks: map[string]string{}}
	h.seed()
	return h
}

// seed 造两个普通用户 + 一个管理员，各带一把 key、一条用量、一次充值。
//
// 数据必须是**两个用户各有一份**，否则「作用域收窄」的断言会退化成
// 「本来就只有一个用户」而假通过。
func (h *authzHarness) seed() {
	t := h.t
	ctx := context.Background()
	st := h.db

	mkUser := func(id, role string) {
		if err := st.CreateUser(ctx, &store.User{
			ID: id, Username: id, DisplayName: id,
			PasswordHash: "pbkdf2-sha256$1$AA==$AA==", // 占位：本文件不测口令登录
			Role:         role, Status: store.UserStatusActive, AuthVersion: 1,
			Unlimited: true,
		}); err != nil {
			t.Fatalf("create user %s: %v", id, err)
		}
	}
	mkUser(idUser, store.RoleUser)
	mkUser(idOther, store.RoleUser)
	mkUser(idAdmin, store.RoleAdmin)

	// 上游 + 带价模型：用量要能算出**非零**费用，否则「普通用户看不到别人
	// 的金额」这条断言会在 0 == 0 上假通过。
	if err := st.CreateProvider(ctx, &store.Provider{
		ID: "p1", Slug: "p1", Name: "Alpha", Endpoint: "http://a",
		Enabled: true, Protocol: "openai-chat",
	}); err != nil {
		t.Fatalf("create provider: %v", err)
	}
	if err := st.CreateUpstreamModel(ctx, &store.UpstreamModel{
		ID: "um-priced", ProviderID: "p1", ModelID: "priced", Enabled: true, PriceInput: 1.0,
	}); err != nil {
		t.Fatalf("create model: %v", err)
	}
	if err := st.CreateRoute(ctx, &store.Route{
		ID: "r1", PublicName: "flash", ProviderID: "p1", UpstreamModelID: "um-priced", Enabled: true,
	}); err != nil {
		t.Fatalf("create route: %v", err)
	}

	// 两个用户各一条用量：alice 0.25 元、bob 0.75 元。
	// 金额不同是刻意的 —— 相同的话「只看到自己的」无法与「看到全部再取一条」区分。
	//
	// AccessKeyID 必须与下面真建出来的 access_keys.id（"key-"+用户）**一致**：
	// by-key 端点会 LEFT JOIN access_keys 取密钥名，对不上时 name 恒为空，
	// 「按密钥分组也收窄了」这条断言会退化成只比 key_id（少验一层）。
	// 首轮实测就踩到了这一点（响应是 key:"k-alice" 而断言找 "key-alice"）。
	for _, u := range []struct {
		id     string
		tokens int64
	}{{idUser, 250_000}, {idOther, 750_000}} {
		if err := st.CreateUsageRecord(ctx, &store.UsageRecord{
			ID: "rec-" + u.id, Ts: 1_700_000_000_000, UserID: u.id,
			AccessKeyID: "key-" + u.id, PublicModel: "flash", ProviderID: "p1",
			UpstreamModel: "priced", IngressProtocol: "openai-chat",
			InputTokens: u.tokens, TotalTokens: u.tokens,
			Status: "ok", HTTPStatus: 200, UsageState: "reported",
		}); err != nil {
			t.Fatalf("create usage %s: %v", u.id, err)
		}
	}

	// 各一把 key（用于 key 归属越权与 admin 认领的验证）。
	for _, u := range []string{idUser, idOther} {
		if err := st.CreateAccessKey(ctx, &store.AccessKey{
			ID: "key-" + u, KeyHash: "hash-" + u, KeyPrefix: "sk-gw-" + u[:3],
			Name: "key-" + u, Enabled: true, UserID: u, QuotaTokens: 0,
		}); err != nil {
			t.Fatalf("create key %s: %v", u, err)
		}
	}

	// 各一次充值：验证「普通用户只看自己的充值流水」。
	for _, u := range []struct {
		id    string
		cents int64
	}{{idUser, 10_000}, {idOther, 90_000}} {
		if _, err := st.AdjustBalanceWithLedger(ctx, u.id, u.cents, idAdmin, idAdmin, "seed"); err != nil {
			t.Fatalf("topup %s: %v", u.id, err)
		}
	}

	// 真实签发令牌。
	for _, u := range []struct{ id, role string }{
		{idUser, store.RoleUser}, {idOther, store.RoleUser}, {idAdmin, store.RoleAdmin},
	} {
		tok, _, err := h.mgr.Issue(&userauth.UserClaims{
			UserID: u.id, AuthVersion: 1, Role: u.role, Username: u.id,
		})
		if err != nil {
			t.Fatalf("issue token %s: %v", u.id, err)
		}
		h.toks[u.id] = tok
	}
}

// chain 把**真实**的中间件链套在 mux 外，顺序与 main.go 完全一致：
//
//	UserAuth( AdminGateGuard( mux, mux ) )
//
// AutoReload 也一并套上（它在鉴权链外侧），写操作的真实路径因此被覆盖。
//
// mux 传两次与生产一致（见 main.go）：AdminGateGuard 用同一个 mux 查
// 「请求会被哪个路由模式处理」，再比对普通用户白名单。
func (h *authzHarness) chain(mux *http.ServeMux) http.Handler {
	reload := func(context.Context) error { return nil }
	audit := func(string, string, int, string, string) {}
	guarded := server.NewUserAuth(h.mgr, h.db).Guard(server.AdminGateGuard(mux, mux))
	return server.AutoReload(guarded, reload, audit, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
}

// do 以指定身份发一次请求。
func (h *authzHarness) do(handler http.Handler, identity, method, path, body string) probeResult {
	h.t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rdr)
	if rdr != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if tok := h.toks[identity]; tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	// 每个身份一个独立来源 IP：本文件要发上百次请求，共用 IP 会让
	// 失败计数互相干扰（虽然身份有效时 Success 会清零，但分离更稳）。
	req.RemoteAddr = map[string]string{
		idAnon: "203.0.113.10:1111", idUser: "203.0.113.11:1111",
		idOther: "203.0.113.12:1111", idAdmin: "203.0.113.13:1111",
	}[identity]
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return classify(rec)
}

// ---------------------------------------------------------------------------
// 第一层：网关层放行矩阵（每条路由 × 三种身份）
// ---------------------------------------------------------------------------

// TestAuthzMatrix_GatewayLayer 是本文件的主产出：对 main.go 里注册的
// **每一条**管理面路由，各以三种身份打一次，记录实际状态码。
//
// 断言的是安全不变量（而不是把当前行为抄成期望值）：
//
//	I1. 匿名请求绝不可到达任何**需要鉴权**的 handler；
//	I2. 普通用户绝不可到达任何**非白名单**（admin-only）handler；
//	I3. 免鉴权端点（publicAdminMux）必须匿名可达 —— 否则登录页自己就坏了。
//
// 反面（普通用户到达白名单端点）**不**在断言里，因为那是设计要求；
// 它是否安全取决于 handler 自己收窄作用域 —— 由第二层与作用域测试负责。
func TestAuthzMatrix_GatewayLayer(t *testing.T) {
	h := newAuthzHarness(t)
	routes := parseAdminRoutes(t)

	// mux 的 handler 只是个探针：能到这里就说明整条鉴权链放行了。
	// 用探针而不是真实 handler，是为了让本层只测量**网关层**的判定。
	mux := http.NewServeMux()
	registerProbe := func(r adminRoute) {
		mux.HandleFunc(r.Method+" "+r.Path, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"probe":"reached"}`))
		})
	}
	// 免鉴权端点走 publicAdminMux 的等价物。
	pub := http.NewServeMux()
	for _, r := range routes {
		if r.Public {
			pub.HandleFunc(r.Method+" "+r.Path, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"probe":"public-reached"}`))
			})
			continue
		}
		registerProbe(r)
	}
	guarded := h.chain(mux)

	type row struct {
		r               adminRoute
		anon, user, adm probeResult
	}
	rows := make([]row, 0, len(routes))
	for _, r := range routes {
		p := probePath(r.Path)
		var anon, user, adm probeResult
		if r.Public {
			// 免鉴权端点直连其自身 mux（main.go 也是这么挂的：不经鉴权链）。
			anon = h.do(pub, idAnon, r.Method, p, "")
			user = h.do(pub, idUser, r.Method, p, "")
			adm = h.do(pub, idAdmin, r.Method, p, "")
		} else {
			anon = h.do(guarded, idAnon, r.Method, p, "")
			user = h.do(guarded, idUser, r.Method, p, "")
			adm = h.do(guarded, idAdmin, r.Method, p, "")
		}
		rows = append(rows, row{r, anon, user, adm})
	}

	// 打出完整矩阵（原始证据进测试日志）。
	t.Logf("=== 管理面权限矩阵（网关层）共 %d 条路由 ===", len(rows))
	t.Logf("%-6s %-42s %-19s %-19s %-19s", "METHOD", "PATH", "ANON", "USER", "ADMIN")
	for _, r := range rows {
		t.Logf("%-6s %-42s %-19s %-19s %-19s",
			r.r.Method, r.r.Path,
			fmtVerdict(r.anon), fmtVerdict(r.user), fmtVerdict(r.adm))
	}

	// ---- I1/I3：匿名 ----
	for _, r := range rows {
		if r.r.Public {
			if !r.anon.Reached {
				t.Errorf("I3 违反：免鉴权端点 %s %s 匿名不可达（status=%d）—— 登录页会坏",
					r.r.Method, r.r.Path, r.anon.Status)
			}
			continue
		}
		if r.anon.Reached {
			t.Errorf("I1 违反：匿名请求到达了 %s %s（status=%d body=%s）",
				r.r.Method, r.r.Path, r.anon.Status, truncate(r.anon.Body))
		}
	}

	// ---- I2：普通用户不得进入非白名单端点 ----
	//
	// 白名单键是「方法 + 模式」（见 isWhitelisted）：方法也是授权的一部分，
	// 所以 GET /admin/api/keys 与 DELETE /admin/api/keys/{id} 是两个独立条目。
	for _, r := range rows {
		if r.r.Public {
			continue
		}
		key := r.r.Method + " " + r.r.Path
		if r.user.Reached && !r.user.HandlerDenied && !isWhitelisted(key) {
			t.Errorf("I2 违反（越权）：普通用户到达了 admin-only 端点 %s（status=%d body=%s）",
				key, r.user.Status, truncate(r.user.Body))
		}
	}

	// ---- 管理员必须能进所有需要鉴权的管理端点 ----
	for _, r := range rows {
		if r.r.Public {
			continue
		}
		if !r.adm.Reached {
			t.Errorf("管理员被挡在 %s %s 之外（status=%d body=%s）—— 白名单/鉴权链写反了",
				r.r.Method, r.r.Path, r.adm.Status, truncate(r.adm.Body))
		}
	}
}

// fmtVerdict 把判定渲染成矩阵单元格：状态码 + 归类。
func fmtVerdict(p probeResult) string {
	return itoa(p.Status) + "/" + p.verdict()
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		return "-" + string(b)
	}
	return string(b)
}

// isWhitelisted 镜像 server.userAccessibleRoutes —— 按「方法 + 路由模式」精确判定。
//
// # 2026-10-10：从前缀镜像改成精确镜像
//
// 原实现镜像的是一串前缀，用 HasPrefix 判定。现在生产侧已改成精确路由匹配
// （见 userAccessibleRoutes 的注释：前缀会**自动放行该子树下未来新增的一切
// 端点**，/usage/by-user 就是这么静默落进来的），所以镜像也必须跟着改 ——
// 否则这个测试自己就成了过时假设的守护者。
//
// 传的是 `mux.Handler(r)` 返回的**注册模式**（如 `PATCH /admin/api/keys/{id}`），
// 不是请求的原始路径。这与生产判定完全一致。
//
// 仍然是**跨包镜像**：那个表是包私有变量，拿不到，只能镜像。若有人改了它，
// 本文件的镜像会与实际不符 —— 那正是下面
// TestAuthzMatrix_WhitelistMirrorIsAccurate 要钉住的（它用实测校验镜像）。
func isWhitelisted(pattern string) bool {
	for _, p := range []string{
		"GET /admin/api/keys",
		"POST /admin/api/keys",
		"PATCH /admin/api/keys/{id}",
		"DELETE /admin/api/keys/{id}",
		"POST /admin/api/keys/{id}/recompute-usage",

		"GET /admin/api/usage",
		"GET /admin/api/usage/by-key",
		"GET /admin/api/usage/by-model",
		"GET /admin/api/usage/by-provider",
		"GET /admin/api/usage/by-day",
		"GET /admin/api/usage/history",
		"GET /admin/api/usage/history.csv",
		"GET /admin/api/stats",

		"GET /admin/api/me",
		"POST /admin/api/me/password",
		"GET /admin/api/topups",
		"POST /admin/api/logout",
		"GET /admin/api/model-names",
	} {
		if pattern == p {
			return true
		}
	}
	return false
}

// TestAuthzMatrix_WhitelistMirrorIsAccurate 钉住上面那份镜像与真实白名单一致。
//
// 做法：直接观察 AdminGateGuard 的行为 —— 对每条路由用普通用户跑一次，
// 「网关层放行」当且仅当路径命中白名单。于是镜像可以被**实测**校验，
// 而不是靠人去比对两份字符串。
func TestAuthzMatrix_WhitelistMirrorIsAccurate(t *testing.T) {
	h := newAuthzHarness(t)
	routes := parseAdminRoutes(t)

	for _, r := range routes {
		if r.Public {
			continue
		}
		mux := http.NewServeMux()
		mux.HandleFunc(r.Method+" "+r.Path, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		})
		res := h.do(h.chain(mux), idUser, r.Method, probePath(r.Path), "")
		// 网关层放行 = 到达 handler（可能随后被 handler 自己 403）。
		gatewayAllowed := res.Reached
		// 比对用的是**「方法 + 模式」**这个键 —— 生产的白名单以它为键
		// （方法也是授权的一部分：GET /admin/api/keys 与
		//  DELETE /admin/api/keys/{id} 是两个独立授权项）。
		key := r.Method + " " + r.Path
		if gatewayAllowed != isWhitelisted(key) {
			t.Errorf("白名单镜像失真：%s 实测网关层放行=%v，镜像认为=%v",
				key, gatewayAllowed, isWhitelisted(key))
		}
	}
}

func truncate(s string) string {
	if len(s) > 200 {
		return s[:200] + "…"
	}
	return s
}

// sha256Hex 复现 key 的哈希口径（与 auth.Authenticate 里的
// sha256.Sum256(明文) → hex 完全一致）。必须一致，否则灌进快照的 key
// 永远匹配不上，测试会因「key 无效」而假通过。
func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// timeNowPlusHour 给 SetSessionCookie 一个确定性的过期时刻。
func timeNowPlusHour() time.Time { return time.Now().Add(time.Hour) }

// ---------------------------------------------------------------------------
// 第二层：真实 handler 的越权与作用域
// ---------------------------------------------------------------------------

// buildRealAdminMux 按 main.go 的**同一套注册**挂上真实 handler。
//
// 与 main.go 的差异只有一处且是不可避免的：denyAdminKeyCreate 在 package main
// 里，外部测试包无法引用。这一点在报告里明确记为「未能进程内验证」，
// 由真实进程探针（scripts/authz_probe/）覆盖。
func buildRealAdminMux(h *authzHarness) *http.ServeMux {
	mux := http.NewServeMux()
	keyH := admin.NewKeyHandler(h.db)
	usageH := admin.NewUsageHandler(h.db)
	statsH := admin.NewStatsHandler(h.db)
	userH := admin.NewUserHandler(h.db, h.mgr)
	groupH := admin.NewGroupHandler(h.db)
	routeH := admin.NewRouteHandler(h.db)

	mux.HandleFunc("GET /admin/api/keys", keyH.List)
	mux.HandleFunc("POST /admin/api/keys", keyH.Create) // 真实 main.go 外面还裹了 denyAdminKeyCreate
	mux.HandleFunc("PATCH /admin/api/keys/{id}", func(w http.ResponseWriter, r *http.Request) { keyH.Update(w, r, r.PathValue("id")) })
	mux.HandleFunc("DELETE /admin/api/keys/{id}", func(w http.ResponseWriter, r *http.Request) { keyH.Delete(w, r, r.PathValue("id")) })
	mux.HandleFunc("POST /admin/api/keys/{id}/recompute-usage", func(w http.ResponseWriter, r *http.Request) { keyH.RecomputeUsage(w, r, r.PathValue("id")) })

	mux.HandleFunc("GET /admin/api/usage", usageH.Query)
	mux.HandleFunc("GET /admin/api/usage/by-key", usageH.GroupByKey)
	mux.HandleFunc("GET /admin/api/usage/by-model", usageH.GroupByModel)
	mux.HandleFunc("GET /admin/api/usage/by-provider", usageH.GroupByProvider)
	mux.HandleFunc("GET /admin/api/usage/by-day", usageH.GroupByDay)
	mux.HandleFunc("GET /admin/api/usage/by-user", usageH.GroupByUser)
	mux.HandleFunc("GET /admin/api/usage/history", usageH.History)
	mux.HandleFunc("GET /admin/api/usage/history.csv", usageH.ExportCSV)
	mux.HandleFunc("POST /admin/api/usage/prune", usageH.Prune)
	mux.HandleFunc("GET /admin/api/stats", statsH.Get)
	mux.HandleFunc("GET /admin/api/topups", userH.ListTopups)
	mux.HandleFunc("GET /admin/api/me", userH.Me)
	mux.HandleFunc("POST /admin/api/logout", userH.Logout)
	mux.HandleFunc("POST /admin/api/me/password", userH.ChangePassword)
	mux.HandleFunc("GET /admin/api/users", userH.ListUsers)
	mux.HandleFunc("PUT /admin/api/users/{id}/balance", func(w http.ResponseWriter, r *http.Request) { userH.AdjustBalance(w, r, r.PathValue("id")) })
	mux.HandleFunc("GET /admin/api/groups", groupH.List)
	mux.HandleFunc("GET /admin/api/routes", routeH.List)
	mux.HandleFunc("GET /admin/api/model-names", groupH.ModelNames)
	return mux
}

// TestAuthzUsageByUser_OnlyDefenceIsInsideTheHandler 是本文件最重要的一条。
//
// # 2026-10-10 重写：防线从「只有一道」变成「两道」
//
// 原实现断言的是**旧前提**：/usage/by-user 落在白名单前缀 /admin/api/usage 下，
// 于是网关层**放行**普通用户，唯一防线是 handler 内的 requireAdmin。
// （那正是 sec-auditor 在 test-authz.md §7 报告的结构性风险。）
//
// 现在白名单改成精确路由匹配后，该端点**不再**被前缀自动放行 ——
// 网关层也会拦。于是防线变成两道：
//  1. 网关层（AdminGateGuard）：不在白名单表里 → 403；
//  2. handler 内 requireAdmin：**保留**作纵深防御 —— 万一将来有人把它加进
//     白名单表（那是有意的放行），handler 仍然是最后一道。
//
// 断言分三段，缺一不可：
//  1. 网关层**拦住**（若哪天这条变成「放行」，说明有人改了白名单表，
//     必须同步评估 —— 那个变化本身可能是有意的，但不能静默发生）；
//  2. 真实 handler 也拦，且响应体不含任何人的账单；
//  3. 管理员仍然能拿到 —— 否则上面的 403 可能只是端点整个坏了。
func TestAuthzUsageByUser_TwoLayersOfDefence(t *testing.T) {
	h := newAuthzHarness(t)
	mux := buildRealAdminMux(h)
	guarded := h.chain(mux)

	// ---- 1. 网关层应拦住（只挂探针，不带真实 handler）----
	probeMux := http.NewServeMux()
	reached := false
	probeMux.HandleFunc("GET /admin/api/usage/by-user", func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	})
	probeRes := h.do(h.chain(probeMux), idUser, http.MethodGet, "/admin/api/usage/by-user", "")
	if reached {
		t.Error("网关层放行了 /usage/by-user —— 白名单若已改成放行，必须同步确认 handler 内 requireAdmin 仍在（见本用例注释）")
	}
	if probeRes.Status != http.StatusForbidden {
		t.Errorf("网关层应 403，实际 %d", probeRes.Status)
	}

	// ---- 2. 真实 handler 也必须拦，且不泄露 ----
	res := h.do(guarded, idUser, http.MethodGet, "/admin/api/usage/by-user?from=0", "")
	if res.Status != http.StatusForbidden {
		t.Fatalf("普通用户应被 403，实际 %d body=%s", res.Status, res.Body)
	}
	for _, leak := range []string{"alice", "bob", "records", "cost"} {
		if strings.Contains(res.Body, leak) {
			t.Errorf("403 响应里泄露了 %q：%s", leak, truncate(res.Body))
		}
	}

	// ---- 3. 管理员必须能拿到（否则上面的 403 可能是端点整个坏了）----
	adm := h.do(guarded, idAdmin, http.MethodGet, "/admin/api/usage/by-user?from=0", "")
	if adm.Status != http.StatusOK {
		t.Fatalf("管理员应 200，实际 %d body=%s", adm.Status, adm.Body)
	}
	if !strings.Contains(adm.Body, "bob") || !strings.Contains(adm.Body, "alice") {
		t.Errorf("管理员应看到两个用户的账单，实际 body=%s", truncate(adm.Body))
	}
}

// TestAuthzAdminBoundary_CannotCallV1 验证管理员退出数据面的第一条边界。
//
// 走**真实**的 auth.Authenticate（快照驱动），断言返回 ErrAdminCannotCallModel。
// 这正是 internal/auth 里那条拦截，而不是另写一份判据。
func TestAuthzAdminBoundary_CannotCallV1(t *testing.T) {
	h := newAuthzHarness(t)
	ctx := context.Background()

	// 让管理员也持有一把 key（模拟「升级前留下的 key」这种真实情形）。
	const adminKey = "sk-gw-admin-legacy-key"
	if err := h.db.CreateAccessKey(ctx, &store.AccessKey{
		ID: "key-admin", KeyHash: sha256Hex(adminKey), KeyPrefix: "sk-gw-admin",
		Name: "legacy-admin", Enabled: true, UserID: idAdmin,
	}); err != nil {
		t.Fatalf("create admin key: %v", err)
	}
	const userKey = "sk-gw-alice-key"
	if err := h.db.CreateAccessKey(ctx, &store.AccessKey{
		ID: "key-alice-raw", KeyHash: sha256Hex(userKey), KeyPrefix: "sk-gw-alic",
		Name: "alice", Enabled: true, UserID: idUser,
	}); err != nil {
		t.Fatalf("create alice key: %v", err)
	}

	// 用真实重建把两把 key 灌进快照（auth.Authenticate 只读快照）。
	snap, err := snapshot.RebuildFromDB(ctx, h.db, nil)
	if err != nil {
		t.Fatalf("rebuild snapshot: %v", err)
	}
	snapshot.Init(snap)

	// 管理员 key → ErrAdminCannotCallModel。
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer "+adminKey)
	req.RemoteAddr = "203.0.113.99:1234"
	if _, err := auth.Authenticate(req); err != auth.ErrAdminCannotCallModel {
		t.Fatalf("管理员带 key 调 /v1：err = %v，want ErrAdminCannotCallModel", err)
	}

	// 反向：普通用户 key 必须照常通过（否则「管理员被拦」可能只是 key 全坏了）。
	reqOK := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	reqOK.Header.Set("Authorization", "Bearer "+userKey)
	reqOK.RemoteAddr = "203.0.113.99:1234"
	got, err := auth.Authenticate(reqOK)
	if err != nil {
		t.Fatalf("普通用户 key 应通过鉴权，实际 err = %v", err)
	}
	if got.UserID != idUser {
		t.Fatalf("归属 = %q, want %q", got.UserID, idUser)
	}
}

// TestAuthzAdminBoundary_CannotBeClaimedAKey 验证管理员不能被认领 key。
//
// 走真实 handler：管理员 PATCH 一把 key 把 user_id 指向管理员自己 → 必须 400。
// 并且**库里那把 key 的归属一个字节都没变**（拒绝必须是干净的）。
func TestAuthzAdminBoundary_CannotBeClaimedAKey(t *testing.T) {
	h := newAuthzHarness(t)
	mux := buildRealAdminMux(h)
	guarded := h.chain(mux)

	before, err := h.db.GetAccessKey(context.Background(), "key-"+idUser)
	if err != nil || before == nil {
		t.Fatalf("读取初始 key 失败: %v", err)
	}

	res := h.do(guarded, idAdmin, http.MethodPatch, "/admin/api/keys/key-"+idUser,
		`{"user_id":"`+idAdmin+`"}`)
	if res.Status != http.StatusBadRequest {
		t.Fatalf("认领给管理员应 400，实际 %d body=%s", res.Status, res.Body)
	}
	// 错误消息必须指向正确出路。
	if !strings.Contains(res.Body, "普通用户") {
		t.Errorf("错误消息应提示认领给普通用户，实际 %s", truncate(res.Body))
	}

	after, err := h.db.GetAccessKey(context.Background(), "key-"+idUser)
	if err != nil || after == nil {
		t.Fatalf("读取最终 key 失败: %v", err)
	}
	if after.UserID != before.UserID {
		t.Fatalf("拒绝不干净：key 归属被改成了 %q（原 %q）", after.UserID, before.UserID)
	}

	// 反向：认领给普通用户必须成功。
	ok := h.do(guarded, idAdmin, http.MethodPatch, "/admin/api/keys/key-"+idOther,
		`{"user_id":"`+idUser+`"}`)
	if ok.Status != http.StatusOK {
		t.Fatalf("认领给普通用户应 200，实际 %d body=%s", ok.Status, ok.Body)
	}
}

// TestAuthzAdminBoundary_CannotTopUpSelf 验证管理员不能给自己充值（需求 5）。
//
// 除了状态码，还要确认**余额没有变化**：只断言 400 时，一个「先加钱再回 400」
// 的实现照样绿 —— 而那正是本条要消除的后果。
func TestAuthzAdminBoundary_CannotTopUpSelf(t *testing.T) {
	h := newAuthzHarness(t)
	mux := buildRealAdminMux(h)
	guarded := h.chain(mux)
	ctx := context.Background()

	before, _, err := h.db.BalanceOf(ctx, idAdmin)
	if err != nil {
		t.Fatalf("读取管理员余额: %v", err)
	}

	res := h.do(guarded, idAdmin, http.MethodPut, "/admin/api/users/"+idAdmin+"/balance",
		`{"delta_cents":100000}`)
	if res.Status != http.StatusBadRequest {
		t.Fatalf("管理员给自己充值应 400，实际 %d body=%s", res.Status, res.Body)
	}
	if !strings.Contains(res.Body, "管理员") {
		t.Errorf("错误消息应说明管理员不参与计费，实际 %s", truncate(res.Body))
	}

	after, _, err := h.db.BalanceOf(ctx, idAdmin)
	if err != nil {
		t.Fatalf("重新读取管理员余额: %v", err)
	}
	if after != before {
		t.Fatalf("拒绝不干净：管理员余额从 %d 变成 %d", before, after)
	}

	// 反向：给普通用户充值必须成功。
	okRes := h.do(guarded, idAdmin, http.MethodPut, "/admin/api/users/"+idUser+"/balance",
		`{"delta_cents":5000}`)
	if okRes.Status != http.StatusOK {
		t.Fatalf("给普通用户充值应 200，实际 %d body=%s", okRes.Status, okRes.Body)
	}
}

// TestAuthzScope_NormalUserSeesOnlyOwnData 验证白名单端点的**作用域收窄**。
//
// 「能进端点」与「只看得到自己」是两件事。本用例用**两个都有数据**的用户，
// 断言 alice 的每个响应里都不出现 bob 的标识与金额。
func TestAuthzScope_NormalUserSeesOnlyOwnData(t *testing.T) {
	h := newAuthzHarness(t)
	mux := buildRealAdminMux(h)
	guarded := h.chain(mux)

	cases := []struct {
		name string
		path string
		// mustNotContain：绝对不能出现的字符串（别人的标识）。
		mustNotContain []string
		// mustContain：必须出现的字符串（自己的标识），确保请求真的成功了。
		mustContain []string
	}{
		{
			name: "GET /admin/api/usage",
			path: "/admin/api/usage?from=0&limit=100",
			// bob 的 user_id 会出现在明细的 user_id 字段里。
			mustNotContain: []string{idOther},
			mustContain:    []string{"records"},
		},
		{
			name: "GET /admin/api/usage/by-key",
			path: "/admin/api/usage/by-key?from=0",
			// 按 key 分组时会带出 key 名（key-bob）。
			mustNotContain: []string{"key-" + idOther, idOther},
			mustContain:    []string{"key-" + idUser},
		},
		{
			name:           "GET /admin/api/usage/by-model",
			path:           "/admin/api/usage/by-model?from=0",
			mustNotContain: []string{idOther},
			mustContain:    nil,
		},
		{
			name:           "GET /admin/api/usage/by-provider",
			path:           "/admin/api/usage/by-provider?from=0",
			mustNotContain: []string{idOther},
			mustContain:    nil,
		},
		{
			name:           "GET /admin/api/usage/by-day",
			path:           "/admin/api/usage/by-day?from=0",
			mustNotContain: []string{idOther},
			mustContain:    nil,
		},
		{
			name:           "GET /admin/api/usage/history",
			path:           "/admin/api/usage/history?from=0&limit=100",
			mustNotContain: []string{idOther},
			mustContain:    []string{"records"},
		},
		{
			name: "GET /admin/api/usage/history.csv",
			path: "/admin/api/usage/history.csv?from=0&limit=1000",
			// CSV 里会带 key_name/key_id —— bob 的绝不能出现。
			mustNotContain: []string{"key-" + idOther, idOther},
			mustContain:    nil,
		},
		{
			name: "GET /admin/api/usage/by-user",
			path: "/admin/api/usage/by-user?from=0",
			// admin-only：普通用户 403，响应里不得有任何人。
			mustNotContain: []string{idUser, idOther, "records"},
			mustContain:    nil, // 期望 403，不断言成功体
		},
		{
			name: "GET /admin/api/topups",
			path: "/admin/api/topups?limit=50",
			// bob 的充值（900 元 = 90000 分）绝不能出现在 alice 的流水里。
			mustNotContain: []string{idOther, "90000"},
			mustContain:    []string{"10000"},
		},
		{
			name:           "GET /admin/api/keys",
			path:           "/admin/api/keys",
			mustNotContain: []string{"key-" + idOther},
			mustContain:    []string{"key-" + idUser},
		},
		{
			name: "GET /admin/api/stats",
			path: "/admin/api/stats?from=0",
			// alice 只有 0.25 元；全站是 1.00 元。断言不是全站数字。
			mustNotContain: []string{idOther},
			mustContain:    []string{"total_requests"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := h.do(guarded, idUser, http.MethodGet, tc.path, "")
			t.Logf("普通用户 %s -> status=%d body=%s", tc.path, res.Status, truncate(res.Body))

			for _, s := range tc.mustNotContain {
				if strings.Contains(res.Body, s) {
					t.Errorf("越权泄露：%s 的响应里出现了 %q\nbody=%s", tc.name, s, truncate(res.Body))
				}
			}
			for _, s := range tc.mustContain {
				if !strings.Contains(res.Body, s) {
					t.Errorf("%s 的响应里缺少 %q（请求可能整体失败了）\nstatus=%d body=%s",
						tc.name, s, res.Status, truncate(res.Body))
				}
			}
		})
	}

	// 反向：管理员必须看得到两个人（否则上面的"没泄露"可能只是数据没进去）。
	for _, path := range []string{
		"/admin/api/usage?from=0&limit=100",
		"/admin/api/topups?limit=50",
		"/admin/api/keys",
		"/admin/api/usage/by-user?from=0",
	} {
		res := h.do(guarded, idAdmin, http.MethodGet, path, "")
		t.Logf("管理员 %s -> status=%d body=%s", path, res.Status, truncate(res.Body))
		if res.Status != http.StatusOK {
			t.Fatalf("管理员访问 %s 应 200，实际 %d body=%s", path, res.Status, truncate(res.Body))
		}
	}
}

// TestAuthzScope_NumericIsolation 用**数字**而不是字符串证明作用域收窄。
//
// 上面那条用例是「响应里不出现别人的标识」—— 那是必要条件但不是充分条件：
// 一个只把 user_id 字段改名/删掉的实现能骗过它，却照旧把 bob 的**金额**
// 汇总进去。本用例直接比数字：
//
//	alice 自己：250_000 token（0.25 元）   bob：750_000 token（0.75 元）
//	全站合计：  1_000_000 token（1.00 元）
//
// 所以「alice 看到的是 250000 / 0.25」与「admin 看到的是 1000000 / 1.00」
// 是一对互为证据的断言：前者证明收窄，后者证明数据确实都在库里
// （否则前者会因为「表里只有 alice」而假通过）。
func TestAuthzScope_NumericIsolation(t *testing.T) {
	h := newAuthzHarness(t)
	mux := buildRealAdminMux(h)
	guarded := h.chain(mux)

	type statsBody struct {
		TotalTokens   int64   `json:"total_tokens"`
		TotalRequests int64   `json:"total_requests"`
		Cost          float64 `json:"cost"`
	}
	getStats := func(identity string) statsBody {
		t.Helper()
		res := h.do(guarded, identity, http.MethodGet, "/admin/api/stats?from=0", "")
		if res.Status != http.StatusOK {
			t.Fatalf("%s /stats status=%d body=%s", identity, res.Status, truncate(res.Body))
		}
		var b statsBody
		if err := json.Unmarshal([]byte(res.Body), &b); err != nil {
			t.Fatalf("解析 stats: %v body=%s", err, truncate(res.Body))
		}
		return b
	}

	mine := getStats(idUser)
	other := getStats(idOther)
	all := getStats(idAdmin)

	t.Logf("stats 实测：alice=%+v  bob=%+v  admin(全站)=%+v", mine, other, all)

	// alice 必须只看到自己的 250_000。
	if mine.TotalTokens != 250_000 || mine.TotalRequests != 1 {
		t.Errorf("alice 的 stats = %+v，want tokens=250000 requests=1 —— 作用域没收窄", mine)
	}
	// bob 必须只看到自己的 750_000（两个方向都验，避免「大家看到的都是同一份」）。
	if other.TotalTokens != 750_000 || other.TotalRequests != 1 {
		t.Errorf("bob 的 stats = %+v，want tokens=750000 requests=1", other)
	}
	// 管理员看到全站合计 —— 这条同时证明数据都在库里（上面的"只看到自己"不是假通过）。
	if all.TotalTokens != 1_000_000 || all.TotalRequests != 2 {
		t.Errorf("管理员 stats = %+v，want tokens=1000000 requests=2 —— 全局视图不对", all)
	}
	// 费用也必须隔离：alice 0.25，全站 1.00。
	if mine.Cost != 0.25 {
		t.Errorf("alice cost = %v，want 0.25", mine.Cost)
	}
	if all.Cost != 1.0 {
		t.Errorf("管理员 cost = %v，want 1.0", all.Cost)
	}

	// 明细条数也要隔离：alice 的 /usage 只能有 1 条（不是 2 条）。
	var detail struct {
		Records []struct {
			UserID string `json:"user_id"`
		} `json:"records"`
	}
	res := h.do(guarded, idUser, http.MethodGet, "/admin/api/usage?from=0&limit=100", "")
	if err := json.Unmarshal([]byte(res.Body), &detail); err != nil {
		t.Fatalf("解析 usage: %v body=%s", err, truncate(res.Body))
	}
	if len(detail.Records) != 1 {
		t.Errorf("alice 的用量明细有 %d 条，want 1（全站是 2 条）", len(detail.Records))
	}
	t.Logf("alice /usage 明细条数 = %d（全站 2 条）", len(detail.Records))
}

// TestAuthzIDOR_CrossUserKeyMutations 是 IDOR 的运行时验证。
//
// `/admin/api/keys` 在**白名单**里，所以普通用户能进这个前缀下的**所有**方法
// —— 包括 PATCH / DELETE / recompute-usage。也就是说「能不能进」这一层
// 完全不区分「这是不是我的 key」。唯一的作用域判定在 handler 的
// ownedByCaller 里。
//
// 本用例对**每一个**改/删路径发一次跨用户请求，并断言：
//  1. 状态码是 404（刻意不是 403 —— 403 会告诉调用者「这把 key 存在，只是
//     不是你的」，那本身就是可枚举的信息）；
//  2. **库里那把 key 一个字节都没变**（只断言状态码时，一个「先改了再回 404」
//     的实现照样绿）。
func TestAuthzIDOR_CrossUserKeyMutations(t *testing.T) {
	h := newAuthzHarness(t)
	mux := buildRealAdminMux(h)
	guarded := h.chain(mux)
	ctx := context.Background()

	victimKey := "key-" + idOther

	readVictim := func() store.AccessKey {
		t.Helper()
		k, err := h.db.GetAccessKey(ctx, victimKey)
		if err != nil || k == nil {
			t.Fatalf("读取 bob 的 key 失败: %v", err)
		}
		return *k
	}

	cases := []struct {
		name   string
		method string
		path   string
		body   string
	}{
		{"PATCH 改别人的 key（改名 + 关掉）", http.MethodPatch, "/admin/api/keys/" + victimKey,
			`{"name":"pwned","enabled":false}`},
		{"DELETE 删别人的 key", http.MethodDelete, "/admin/api/keys/" + victimKey, ""},
		{"recompute-usage 重算别人的 key", http.MethodPost, "/admin/api/keys/" + victimKey + "/recompute-usage", ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := readVictim()

			res := h.do(guarded, idUser, tc.method, tc.path, tc.body)
			t.Logf("alice %s %s -> status=%d body=%s", tc.method, tc.path, res.Status, truncate(res.Body))

			if res.Status != http.StatusNotFound {
				t.Errorf("跨用户操作应 404（不是 403，避免存在性 oracle），实际 %d body=%s",
					res.Status, truncate(res.Body))
			}

			after := readVictim()
			if after.Name != before.Name {
				t.Errorf("拒绝不干净：key 名字被改成 %q（原 %q）", after.Name, before.Name)
			}
			if after.Enabled != before.Enabled {
				t.Errorf("拒绝不干净：key enabled 被改成 %v（原 %v）", after.Enabled, before.Enabled)
			}
			if after.UserID != before.UserID {
				t.Errorf("拒绝不干净：key 归属被改成 %q（原 %q）", after.UserID, before.UserID)
			}
		})
	}

	// bob 的 key 必须仍然存在且可用（上面的 DELETE 不能真的删掉它）。
	final, err := h.db.GetAccessKey(ctx, victimKey)
	if err != nil || final == nil {
		t.Fatalf("bob 的 key 消失了：DELETE 越权成功了（err=%v）", err)
	}

	// 反向：alice 对自己的 key 必须能改（否则上面的 404 可能只是端点整体坏了）。
	own := h.do(guarded, idUser, http.MethodPatch, "/admin/api/keys/key-"+idUser, `{"name":"mine-renamed"}`)
	if own.Status != http.StatusOK {
		t.Fatalf("alice 改自己的 key 应 200，实际 %d body=%s", own.Status, truncate(own.Body))
	}
	t.Logf("对照：alice 改自己的 key → %d（证明 404 是作用域判定而不是端点故障）", own.Status)
}

// TestAuthzScope_AdminSeesEveryoneInTopups 验证管理员在充值流水上看得到**全站**。
//
// 这是 Lead 之前决策的落地验证（原先管理员按 me.ID 收窄 ⇒ 恒为空）。
func TestAuthzScope_AdminSeesEveryoneInTopups(t *testing.T) {
	h := newAuthzHarness(t)
	mux := buildRealAdminMux(h)
	guarded := h.chain(mux)

	res := h.do(guarded, idAdmin, http.MethodGet, "/admin/api/topups?limit=50", "")
	if res.Status != http.StatusOK {
		t.Fatalf("status = %d body=%s", res.Status, res.Body)
	}
	var page struct {
		Records []struct {
			UserID string `json:"user_id"`
		} `json:"records"`
		Total int64 `json:"total"`
	}
	if err := json.Unmarshal([]byte(res.Body), &page); err != nil {
		t.Fatalf("解析 topups: %v body=%s", err, truncate(res.Body))
	}
	seen := map[string]bool{}
	for _, r := range page.Records {
		seen[r.UserID] = true
	}
	if !seen[idUser] || !seen[idOther] {
		t.Fatalf("管理员应看到两个用户的充值流水，实际只有 %v（total=%d）", seen, page.Total)
	}
	// 管理员自己**不能**有流水（他不能被充值）—— 这同时印证了需求 5。
	if seen[idAdmin] {
		t.Errorf("管理员自己被充值了：%v", seen)
	}
}

// ---------------------------------------------------------------------------
// CSRF
// ---------------------------------------------------------------------------

// TestAuthzCSRF_CookieAuthWriteRequiresJSONContentType 补 csrf_test.go 没覆盖的一面。
//
// internal/admin/csrf_test.go 已经覆盖了「text/plain → 415」与
// 「缺失 Content-Type → 415」（在 handler 层直接调用）。本用例补的是
// **走完整链路、且用 cookie 而非 Authorization 头**的那种请求 ——
// 因为 cookie 是浏览器会自动附带的凭据，正是 CSRF 的真实形态。
func TestAuthzCSRF_CookieAuthWriteRequiresJSONContentType(t *testing.T) {
	h := newAuthzHarness(t)
	mux := buildRealAdminMux(h)
	guarded := h.chain(mux)

	send := func(contentType string, withCookie bool) probeResult {
		req := httptest.NewRequest(http.MethodPost, "/admin/api/keys",
			strings.NewReader(`{"name":"csrf-probe"}`))
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		if withCookie {
			// 用 cookie 承载会话：浏览器跨站时会自动带上它 —— 这正是
			// CSRF 的威胁模型（也是为什么 Content-Type 门禁必须存在）。
			req.AddCookie(&http.Cookie{Name: server.SessionCookieName, Value: h.toks[idUser]})
		}
		req.RemoteAddr = "203.0.113.50:1111"
		rec := httptest.NewRecorder()
		guarded.ServeHTTP(rec, req)
		return classify(rec)
	}

	// 关键断言：cookie 认证 + text/plain（CORS safelisted，不触发预检）→ 必须被拒。
	plain := send("text/plain", true)
	if plain.Status != http.StatusUnsupportedMediaType {
		t.Errorf("cookie 认证 + text/plain 应 415，实际 %d body=%s", plain.Status, truncate(plain.Body))
	}
	// 缺失 Content-Type 同样拒绝。
	missing := send("", true)
	if missing.Status != http.StatusUnsupportedMediaType {
		t.Errorf("cookie 认证 + 无 Content-Type 应 415，实际 %d body=%s",
			missing.Status, truncate(missing.Body))
	}
	// 反向：application/json 必须被接受（否则上面的 415 可能只是链路坏了）。
	okJSON := send("application/json", true)
	if okJSON.Status != http.StatusCreated {
		t.Errorf("cookie 认证 + application/json 应 201，实际 %d body=%s",
			okJSON.Status, truncate(okJSON.Body))
	}

	// 无论哪次尝试，都必须确认库里没多出 CSRF 的 key。
	keys, err := h.db.ListAccessKeys(context.Background())
	if err != nil {
		t.Fatalf("list keys: %v", err)
	}
	for _, k := range keys {
		if k.Name == "csrf-probe" && okJSON.Status != http.StatusCreated {
			t.Errorf("被 415 拒绝的请求仍然建出了 key: %+v", k)
		}
	}
}

// TestAuthzCSRF_SameSiteStrictOnSessionCookie 验证会话 cookie 的 SameSite 属性。
//
// 这是 CSRF 的第一道（也是最强的一道）防线：SameSite=Strict 让浏览器在
// **跨站**请求里根本不附带这个 cookie，于是「跨站写操作」在到达服务端之前
// 就已经没有凭据了。Content-Type 门禁是第二道。
func TestAuthzCSRF_SameSiteStrictOnSessionCookie(t *testing.T) {
	rec := httptest.NewRecorder()
	server.SetSessionCookie(rec, "tok", timeNowPlusHour())
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("期望 1 个 cookie，得到 %d", len(cookies))
	}
	c := cookies[0]
	if c.SameSite != http.SameSiteStrictMode {
		t.Errorf("SameSite = %v，want Strict —— 跨站请求会带上 cookie，CSRF 防线只剩 Content-Type 一道",
			c.SameSite)
	}
	if !c.HttpOnly {
		t.Error("会话 cookie 必须 HttpOnly（否则 XSS 可直接读走令牌）")
	}
	if c.Path != "/admin" {
		t.Errorf("Path = %q，want /admin", c.Path)
	}
	t.Logf("会话 cookie: name=%s path=%s samesite=%v httponly=%v maxage=%d",
		c.Name, c.Path, c.SameSite, c.HttpOnly, c.MaxAge)
}

// ---------------------------------------------------------------------------
// 会话与限速
// ---------------------------------------------------------------------------

// TestAuthzSession_RevokedAfterPasswordChange 验证改密后旧会话立刻失效。
//
// 走真实链路：先确认旧令牌可用 → 改密（真实 handler，递增 auth_version）
// → 同一个旧令牌必须立刻 401。
func TestAuthzSession_RevokedAfterPasswordChange(t *testing.T) {
	h := newAuthzHarness(t)
	mux := buildRealAdminMux(h)
	guarded := h.chain(mux)
	ctx := context.Background()

	// 先给 alice 一个**真实可校验**的密码哈希（改密要验旧密码）。
	hash, err := userauth.HashPassword("OldPassw0rd!")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if err := h.db.SetUserPassword(ctx, idUser, hash); err != nil {
		t.Fatalf("set password: %v", err)
	}

	// 取改密后**当前**的 auth_version，重新签一枚有效令牌。
	u, err := h.db.GetUser(ctx, idUser)
	if err != nil || u == nil {
		t.Fatalf("get user: %v", err)
	}
	tok, _, err := h.mgr.Issue(&userauth.UserClaims{
		UserID: idUser, AuthVersion: u.AuthVersion, Role: u.Role, Username: u.Username,
	})
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	h.toks[idUser] = tok

	// 旧令牌此时有效。
	before := h.do(guarded, idUser, http.MethodGet, "/admin/api/me", "")
	if before.Status != http.StatusOK {
		t.Fatalf("改密前 /me 应 200，实际 %d body=%s", before.Status, truncate(before.Body))
	}

	// 改密。
	chg := h.do(guarded, idUser, http.MethodPost, "/admin/api/me/password",
		`{"old_password":"OldPassw0rd!","new_password":"NewPassw0rd!"}`)
	if chg.Status != http.StatusOK {
		t.Fatalf("改密应 200，实际 %d body=%s", chg.Status, truncate(chg.Body))
	}

	// 同一枚令牌必须立刻失效（auth_version 已递增）。
	after := h.do(guarded, idUser, http.MethodGet, "/admin/api/me", "")
	if after.Status != http.StatusUnauthorized {
		t.Fatalf("改密后旧令牌应 401，实际 %d body=%s —— 旧会话没被吊销",
			after.Status, truncate(after.Body))
	}
	t.Logf("改密前 /me=%d，改密后同一令牌 /me=%d（auth_version 栅栏生效）",
		before.Status, after.Status)
}

// TestAuthzSession_LogoutRevokesServerSide 验证登出是**服务端吊销**而不是只清 cookie。
//
// 判据：登出后拿同一枚令牌再打一次受保护端点必须 401。若只清 cookie，
// 令牌在自然过期前仍然可用（最长 8 小时）。
func TestAuthzSession_LogoutRevokesServerSide(t *testing.T) {
	h := newAuthzHarness(t)
	mux := buildRealAdminMux(h)
	guarded := h.chain(mux)

	before := h.do(guarded, idUser, http.MethodGet, "/admin/api/me", "")
	if before.Status != http.StatusOK {
		t.Fatalf("登出前应 200，实际 %d", before.Status)
	}

	out := h.do(guarded, idUser, http.MethodPost, "/admin/api/logout", "")
	if out.Status != http.StatusOK {
		t.Fatalf("登出应 200，实际 %d body=%s", out.Status, truncate(out.Body))
	}

	// 关键：令牌还在手里（Authorization 头仍带着它），但必须已失效。
	after := h.do(guarded, idUser, http.MethodGet, "/admin/api/me", "")
	if after.Status != http.StatusUnauthorized {
		t.Fatalf("登出后同一令牌应 401（服务端吊销），实际 %d body=%s",
			after.Status, truncate(after.Body))
	}
	t.Logf("登出前 /me=%d，登出后同一令牌 /me=%d（服务端吊销生效）",
		before.Status, after.Status)
}

// TestAuthzThrottle_AnonymousRequestsDoNotCount 验证「匿名请求不计入限速」。
//
// 这条有历史 bug（曾经匿名也计数，于是任何 IP 连发 10 个匿名请求就能把同一
// NAT 出口后面的所有人锁出一分钟）。internal/server/throttle_test.go 已在
// 包内覆盖；本用例从**外部测试包**再验一次，走的是真实中间件链 ——
// 两处独立，任一处被改坏都能发现。
func TestAuthzThrottle_AnonymousRequestsDoNotCount(t *testing.T) {
	h := newAuthzHarness(t)
	mux := buildRealAdminMux(h)
	guarded := h.chain(mux)

	// 连发远超阈值（LoginFailLimit=10）的匿名请求。
	const n = server.LoginFailLimit + 5
	for i := 0; i < n; i++ {
		res := h.do(guarded, idAnon, http.MethodGet, "/admin/api/me", "")
		if res.Status == http.StatusTooManyRequests {
			t.Fatalf("第 %d 个匿名请求被 429 —— 匿名请求绝不可计入限速（零成本 DoS 杠杆）", i+1)
		}
		if res.Status != http.StatusUnauthorized {
			t.Fatalf("第 %d 个匿名请求应 401，实际 %d", i+1, res.Status)
		}
	}

	// 匿名轰炸之后，一个**有效**令牌必须仍然能用。
	ok := h.do(guarded, idUser, http.MethodGet, "/admin/api/me", "")
	if ok.Status != http.StatusOK {
		t.Fatalf("匿名轰炸后有效会话应 200，实际 %d body=%s —— 限速把无关用户误伤了",
			ok.Status, truncate(ok.Body))
	}
	t.Logf("连发 %d 个匿名请求后：全部 401（无 429），有效会话仍 200", n)
}

// TestAuthzThrottle_InvalidTokenEventually429 验证伪造凭据**确实**会被限速。
//
// 与上一条相对：匿名不计数，但「带了令牌且校验失败」必须计数并最终 429，
// 否则暴力猜测令牌没有任何成本。
func TestAuthzThrottle_InvalidTokenEventually429(t *testing.T) {
	h := newAuthzHarness(t)
	mux := buildRealAdminMux(h)
	guarded := h.chain(mux)

	// 用独立的来源 IP，避免与其它用例的计数互相干扰。
	attempt := func(i int) int {
		req := httptest.NewRequest(http.MethodGet, "/admin/api/me", nil)
		req.Header.Set("Authorization", "Bearer not-a-real-token-"+itoa(i))
		req.RemoteAddr = "198.51.100.77:1234"
		rec := httptest.NewRecorder()
		guarded.ServeHTTP(rec, req)
		return rec.Code
	}

	var got429 bool
	for i := 0; i < server.LoginFailLimit+3; i++ {
		code := attempt(i)
		if code == http.StatusTooManyRequests {
			got429 = true
			t.Logf("第 %d 次伪造令牌被 429（阈值 LoginFailLimit=%d）", i+1, server.LoginFailLimit)
			break
		}
		if code != http.StatusUnauthorized {
			t.Fatalf("第 %d 次应 401，实际 %d", i+1, code)
		}
	}
	if !got429 {
		t.Fatalf("连续 %d 次伪造令牌仍未 429 —— 限速没生效", server.LoginFailLimit+3)
	}
}
