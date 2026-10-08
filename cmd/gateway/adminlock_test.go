package main

// 管理员数据面锁定（2026-10 控制面/数据面分离）在网关层的端到端用例。
//
// 业务要求原话：「管理员账号不能调用模型，不能创建key，管理员需要新建普通用户
// 账户来调用API，这样的逻辑，管理员账号只负责管理网关。」
//
// 鉴权层的用例在 internal/auth/adminlock_test.go；这里覆盖两件它够不到的事：
//  1) 端到端确认管理员请求**不触碰上游**、且错误体形状对 SDK 可用；
//  2) 管理面「管理员不能建 key」这条**路由层**策略。

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cn-maul/rosetta-gateway/internal/admin"
	"github.com/cn-maul/rosetta-gateway/internal/server"
	"github.com/cn-maul/rosetta-gateway/internal/snapshot"
	"github.com/cn-maul/rosetta-gateway/internal/store"
)

// ---------------------------------------------------------------------
// 数据面：管理员不得调用 /v1
// ---------------------------------------------------------------------

// adminSnapRole 把测试用户的角色改成 admin（鉴权只读快照）。
func adminSnapRole(t *testing.T, role string) {
	t.Helper()
	u := snapshot.Get().UsersByID[testUserID]
	if u == nil {
		t.Fatalf("快照里没有测试用户 %q —— harness 没装好", testUserID)
	}
	u.Role = role
}

// 管理员调用 /v1 → 403 + admin_cannot_call_model + **上游一次都没被碰**。
//
// 「上游没被碰」是这条用例的核心断言，不能只看状态码：403 完全可能在打完
// 上游之后才写出来（那样管理员照样消耗了上游额度，恰好是这条需求要消除的
// 敞口），而状态码断言照样绿。
func TestAdminLockout_AdminCannotCallV1(t *testing.T) {
	var hits int64
	up := fakeCountingGood(&hits)
	defer up.Close()

	// 自己搭 harness，让「被计数的上游」真的挂在链上 —— 否则 hits 恒为 0，
	// 断言会因**错误的原因**通过（那个计数服务器压根没被调用过）。
	h, db := buildHarness(t, []struct {
		slug, url string
		fail      bool
	}{{slug: "good", url: up.URL}}, false)
	priceRoute(t, db, "good", "good-model", 1_000_000, 1_000_000)
	adminSnapRole(t, store.RoleAdmin)

	rec := postChatWithID(h, "flash")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("管理员调用 /v1 应 403，实际 %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"code":"admin_cannot_call_model"`) {
		t.Fatalf("错误 code 应为 admin_cannot_call_model（SDK 据此分流），body=%s", rec.Body.String())
	}
	if hits != 0 {
		t.Fatalf("上游被触碰了 %d 次：管理员必须在触碰上游之前被拒", hits)
	}
}

// 错误体的 **type** 必须是 permission_error，而不是 authentication_error。
//
// 这条直接对应需求里的「type 字段可区分的分类，SDK 不要当成普通认证失败无限
// 重试」：authentication_error 的标准处置是换 key，而管理员的 key 换多少把
// 都没用，只会无限重试刷日志。
func TestAdminLockout_ErrorTypeIsTerminalNotAuth(t *testing.T) {
	h, _ := pricedHarness(t, store.RoleAdmin, 1_000_000, 1_000_000)

	rec := postChatWithID(h, "flash")
	var body struct {
		Error struct {
			Type string `json:"type"`
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("解析错误体: %v body=%s", err, rec.Body.String())
	}
	if body.Error.Type != "permission_error" {
		t.Fatalf("type = %q，want permission_error —— "+
			"authentication_error 会让 SDK 反复换 key 重试（管理员换 key 无解）", body.Error.Type)
	}
	if body.Error.Code == "invalid_api_key" {
		t.Fatalf("code 复用了 invalid_api_key，客户端无法把「角色不允许」与「凭据坏了」分开：%s",
			rec.Body.String())
	}
}

// 三个协议端点**同样**被拒：拦截收敛在 auth 层，不能只覆盖 chat。
//
// 漏掉任一协议的症状很隐蔽：客户端在 Anthropic 协议下照常能调，于是
// 「管理员不能用模型」这条要求实际没成立，只是没被发现。
//
// 每个 codec 单独建一套 harness：handleIngress 把 codec 编进了闭包，
// 复用同一个 handler 换个 codec 是测不到东西的。
func TestAdminLockout_AllIngressProtocolsRejectAdmin(t *testing.T) {
	for _, codec := range []ingressCodec{openaiChatCodec{}, anthropicMessagesCodec{}, openaiResponsesCodec{}} {
		t.Run(codec.Name(), func(t *testing.T) {
			var hits int64
			up := fakeCountingGood(&hits)
			defer up.Close()

			h := buildHarnessProtocols(t, []harnessChain{{slug: "good", url: up.URL, protocol: "openai-chat"}}, false, codec)
			adminSnapRole(t, store.RoleAdmin)

			body := `{"model":"flash","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`
			req := httptest.NewRequest(http.MethodPost, "/v1/x", strings.NewReader(body))
			req.Header.Set("Authorization", "Bearer "+testAccessKey)
			rec := httptest.NewRecorder()
			h(rec, req)

			if rec.Code != http.StatusForbidden {
				t.Fatalf("管理员应被拒，实际 %d body=%s", rec.Code, rec.Body.String())
			}
			if hits != 0 {
				t.Fatalf("上游被触碰 %d 次：必须在触碰上游之前被拒", hits)
			}
		})
	}
}

// 反向：普通用户**完全不受影响**。
//
// 这是回归的真实风险面 —— 鉴权层一旦写得太宽（例如误判成「Role 非空即拒绝」），
// 症状是全体用户 403，看起来像部署坏了。
//
// 这里不额外计上游命中数：pricedHarness 内部自带 fakeGood，200 + 余额按实收
// 扣减已经完整证明「请求走完了鉴权→上游→落库→扣费」整条链。
func TestAdminLockout_NormalUserUnaffected(t *testing.T) {
	h, db := pricedHarness(t, "", 1_000_000, 1_000_000) // 角色为普通用户
	const start = 10_000_000
	setBalance(t, db, start)

	rec := postChatWithID(h, "flash")
	if rec.Code != http.StatusOK {
		t.Fatalf("普通用户应照常调用，实际 %d body=%s", rec.Code, rec.Body.String())
	}
	// 顺带钉住：普通用户**照常扣费**（拦截没有误伤计费链路）。
	waitBalanceSettled(t, db, start-800, "普通用户应照常按实收扣费")
}

// 管理员的数据面查询端点（/v1/models、billing、org）同样被拒 —— 口径统一。
//
// 理由：这些端点都只做 auth.Authenticate，拦截收敛在鉴权层就自动覆盖。
// 若将来有人绕过鉴权直连某端点，这条会先红。
func TestAdminLockout_QueryEndpointsAlsoRejectAdmin(t *testing.T) {
	up := fakeGood()
	defer up.Close()
	_, db, _ := buildHarnessFull(t, []struct {
		slug, url string
		fail      bool
	}{{slug: "good", url: up.URL}}, false, openaiChatCodec{})
	adminSnapRole(t, store.RoleAdmin)

	for _, tc := range []struct {
		name string
		h    http.HandlerFunc
	}{
		{"billing/subscription", billingSubscription(db)},
		{"billing/usage", billingUsage(db)},
		{"organization/costs", orgCosts(db)},
		{"organization/usage", orgUsageCompletions(db)},
	} {
		rec := httptest.NewRecorder()
		tc.h(rec, authedGet("/v1/x"))
		if rec.Code != http.StatusUnauthorized && rec.Code != http.StatusForbidden {
			t.Errorf("%s：管理员 key 应被拒，实际 %d body=%s", tc.name, rec.Code, rec.Body.String())
		}
	}
}

// ---------------------------------------------------------------------
// 控制面：管理员不能建 key
// ---------------------------------------------------------------------

// asAdminReq / asNormalReq 给**管理面**请求注入身份。
//
// 与 internal/admin 的同名 helper 刻意分开：那边是 package admin 的测试，
// 这边是 package main，两者不共享同一个测试二进制，不能互相引用。
// 身份走 context（server.UserCtxKey）而不是 Authorization 头 —— 管理面读的是
// context，头只在数据面 auth.Authenticate 里被读。
func asAdminReq(r *http.Request) *http.Request {
	return withRole(r, store.RoleAdmin, "admin-test")
}

func asNormalReq(r *http.Request) *http.Request {
	return withRole(r, store.RoleUser, testUserID)
}

func withRole(r *http.Request, role, id string) *http.Request {
	ctx := context.WithValue(r.Context(), server.UserCtxKey(), &store.User{
		ID: id, Username: id,
		Role: role, Status: store.UserStatusActive, AuthVersion: 1,
	})
	return r.WithContext(ctx)
}

// 管理员 POST /admin/api/keys → 403，且**库里一把新 key 都没多**。
//
// 断言落库结果而不只看状态码：只断言 403 时，一个「先建了再回 403」的
// 实现照样绿，而那正是本条策略要消除的敞口。
func TestAdminLockout_AdminCannotCreateKey(t *testing.T) {
	up := fakeGood()
	defer up.Close()
	_, db, _ := buildHarnessFull(t, []struct {
		slug, url string
		fail      bool
	}{{slug: "good", url: up.URL}}, false, openaiChatCodec{})

	before, err := db.ListAccessKeys(context.Background())
	if err != nil {
		t.Fatalf("list keys: %v", err)
	}

	h := denyAdminKeyCreate(admin.NewKeyHandler(db).Create)
	req := httptest.NewRequest(http.MethodPost, "/admin/api/keys",
		strings.NewReader(`{"name":"admin-self-key"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h(rec, asAdminReq(req))

	if rec.Code != http.StatusForbidden {
		t.Fatalf("管理员建 key 应 403，实际 %d body=%s", rec.Code, rec.Body.String())
	}
	// 消息必须指向正确出路，否则管理员会以为 key 功能坏了。
	if !strings.Contains(rec.Body.String(), "普通用户") {
		t.Fatalf("错误消息应提示「建普通用户」，实际 body=%s", rec.Body.String())
	}

	after, err := db.ListAccessKeys(context.Background())
	if err != nil {
		t.Fatalf("list keys after: %v", err)
	}
	if len(after) != len(before) {
		t.Fatalf("管理员建 key 后库里多了 %d 把（拒绝必须是干净的）", len(after)-len(before))
	}
}

// 反向：普通用户自助建 key **照常成功**。
func TestAdminLockout_NormalUserCanStillCreateKey(t *testing.T) {
	up := fakeGood()
	defer up.Close()
	_, db, _ := buildHarnessFull(t, []struct {
		slug, url string
		fail      bool
	}{{slug: "good", url: up.URL}}, false, openaiChatCodec{})

	h := denyAdminKeyCreate(admin.NewKeyHandler(db).Create)
	req := httptest.NewRequest(http.MethodPost, "/admin/api/keys",
		strings.NewReader(`{"name":"self-service"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h(rec, asNormalReq(req))

	if rec.Code != http.StatusCreated {
		t.Fatalf("普通用户自助建 key 应 201，实际 %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"plaintext_key"`) {
		t.Fatalf("创建响应应含 plaintext_key，body=%s", rec.Body.String())
	}
}

// 身份缺失一律 401（fail-closed），绝不等于「当作普通用户放行」。
//
// 这是本条策略最危险的一种写错方向：拿不到身份被当成非管理员，就等于给匿名
// 请求开了一条建 key 的路（key_handler.Create 自己也会 401，但那依赖 handler
// 内部的另一处判定；本中间件必须自己独立守住）。
func TestAdminLockout_MissingIdentityIsRejected(t *testing.T) {
	up := fakeGood()
	defer up.Close()
	_, db, _ := buildHarnessFull(t, []struct {
		slug, url string
		fail      bool
	}{{slug: "good", url: up.URL}}, false, openaiChatCodec{})

	h := denyAdminKeyCreate(admin.NewKeyHandler(db).Create)
	req := httptest.NewRequest(http.MethodPost, "/admin/api/keys",
		strings.NewReader(`{"name":"anon"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h(rec, req) // 不注入任何身份

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("无身份建 key 应 401，实际 %d body=%s", rec.Code, rec.Body.String())
	}
}

// 空 ID 的身份同样拒绝（fail-closed）。
//
// key_handler 里已有「me.ID == \"\" 即 401」的同向判据；这条钉住中间件
// 不与它漂移。空 ID 在统一认证后已无构造路径，但它一旦可达就是静默提权。
func TestAdminLockout_EmptyIDIdentityIsRejected(t *testing.T) {
	// 不需要库：下游 handler 是个 stub，只要证明请求**没被透传**下去即可。
	called := false
	h := denyAdminKeyCreate(func(w http.ResponseWriter, r *http.Request) { called = true })
	req := httptest.NewRequest(http.MethodPost, "/admin/api/keys", nil)
	rec := httptest.NewRecorder()
	h(rec, withRole(req, store.RoleAdmin, "")) // 空 ID

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("空 ID 应 401，实际 %d body=%s", rec.Code, rec.Body.String())
	}
	if called {
		t.Fatal("空 ID 的请求透传到了下游 handler —— 空 ID 等于 admin，方向全错")
	}
}
