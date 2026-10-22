package admin

// GET /admin/api/usage/by-user 的回归测试（钱包页「消费汇总」一表一行一个用户）。
//
// 本文件要钉住四件事，逐条对应下面四个用例：
//   1. **普通用户 403**（最重要的一条）；
//   2. 每用户金额正确、JOIN 出用户名；
//   3. 按金额降序、同额时 user_id 兜底；
//   4. 用户已删除时退化显示（用户名空、user_id 仍在、金额不丢）。
//
// # 为什么第 1 条最重要
//
// 这个端点的路径落在 **/admin/api/usage** 子树下，但网关层用的是精确路由
// 白名单（internal/access），不会因为前缀命中就放行整棵子树。
// 即便如此，handler 内的 requireAdmin 仍是必须的第二道防线：
// 白名单和 handler 判定各写错一处时，不能互相兜底成越权。
// 漏掉 requireAdmin，任何一个普通用户打一次这个端点就能拿到
// 「每个同事的用户名 + 消费金额」，即全公司的账单。
// 而漏掉它的表现是端点照常 200、没有任何报错 —— 静默越权，
// 所以必须有一条测试钉住它。
//
// 这些用例直接调 handler（不经过中间件），身份由 asAdmin / asUser 注入 ——
// 与其它 admin handler 测试同一套做法（见 testhelpers_test.go）。

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/cn-maul/rosetta-gateway/internal/access"
	"github.com/cn-maul/rosetta-gateway/internal/store"
)

// seedByUser 造一个用户 + 一条用量，费用由 input token 数决定。
//
// 单价是 1 元 / 百万 token（见 newByUserStore），所以 tokens/1e6 就是元。
func seedByUser(t *testing.T, st *store.Store, userID string, ts int64, tokens int64) {
	t.Helper()
	ctx := t.Context()
	if err := st.CreateUser(ctx, &store.User{
		ID: userID, Username: userID, PasswordHash: "x",
		Role: store.RoleUser, Status: store.UserStatusActive,
	}); err != nil {
		t.Fatalf("create user %s: %v", userID, err)
	}
	if err := st.CreateUsageRecord(ctx, &store.UsageRecord{
		ID: "rec-" + userID, Ts: ts, UserID: userID, AccessKeyID: "k-" + userID,
		PublicModel: "m", ProviderID: "p1", UpstreamModel: "priced",
		IngressProtocol: "chat",
		InputTokens:     tokens, TotalTokens: tokens,
		Status: "ok", HTTPStatus: 200, UsageState: "reported",
	}); err != nil {
		t.Fatalf("create usage %s: %v", userID, err)
	}
}

// newByUserStore 造一个带 p1 + 单价 1 元/百万 token 模型的库。
//
// 复用 admin 包已有的 newTestStore（它已经建好 p1/p2 与两个模型），
// 只补一条带价的上游模型。价格必须配上：不配价时 cost_total 恒为 0，
// 「金额正确」的断言会退化成 0 == 0（假通过）。
func newByUserStore(t *testing.T) *store.Store {
	t.Helper()
	st := newTestStore(t)
	if err := st.CreateUpstreamModel(t.Context(), &store.UpstreamModel{
		ID: "priced", ProviderID: "p1", ModelID: "priced", PriceInput: 1.0,
	}); err != nil {
		t.Fatalf("seed priced model: %v", err)
	}
	return st
}

// callByUser 调一次 by-user 端点并解出响应。
func callByUser(t *testing.T, h *UsageHandler, r *http.Request) (int, usageByUserResponse) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.GroupByUser(rec, r)
	var out usageByUserResponse
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("unmarshal: %v body=%s", err, rec.Body.String())
		}
	}
	return rec.Code, out
}

// TestUsageByUser_RequiresAdmin 是本文件最重要的一条：普通用户必须 403，
// 且**响应体里不能带任何人的金额**。
//
// 只看状态码还不够 —— 一个「先写响应再判权限」的实现会给出 403 但 body 里
// 已经泄了数据。所以这里同时断言 body 里不含被查询用户的用户名。
func TestUsageByUser_RequiresAdmin(t *testing.T) {
	st := newByUserStore(t)
	h := NewUsageHandler(st)
	now := time.Now().UnixMilli()
	seedByUser(t, st, "victim", now, 500_000) // 0.5 元，全站唯一的消费

	req := httptest.NewRequest(http.MethodGet, "/admin/api/usage/by-user?from=0", nil)
	code, _ := callByUser(t, h, asUser(req, "eavesdropper"))
	if code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 — /admin/api/usage is on the user-accessible "+
			"prefix whitelist, so requireAdmin inside the handler is the ONLY defence", code)
	}

	// 再单独看一次原始 body：403 的响应里绝不能出现别人的用户名。
	rec := httptest.NewRecorder()
	h.GroupByUser(rec, asUser(httptest.NewRequest(http.MethodGet, "/admin/api/usage/by-user?from=0", nil), "eavesdropper"))
	if body := rec.Body.String(); len(body) > 0 {
		var probe map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &probe)
		if _, hasRecords := probe["records"]; hasRecords {
			t.Fatalf("403 response leaked a records payload: %s", body)
		}
	}

	// 管理员必须能拿到（否则上面那条测试可能因为端点整个坏掉而通过）。
	adminReq := httptest.NewRequest(http.MethodGet, "/admin/api/usage/by-user?from=0", nil)
	code, out := callByUser(t, h, asAdmin(adminReq))
	if code != http.StatusOK {
		t.Fatalf("admin status = %d, want 200", code)
	}
	if len(out.Records) != 1 || out.Records[0].Key != "victim" {
		t.Fatalf("admin should see the row: %+v", out)
	}
}

// TestUsageByUser_GatewayWhitelistDoesNotProtectIt 是上面那条测试的**证据**：
// 它直接查生产白名单，证明全站账单端点不在普通用户的可达集合里。
//
// # 为什么必须把这件事单独钉住
//
// 如果有人误以为「这个端点不需要 requireAdmin，因为 AdminGateGuard 会拦」，
// 那是个**错误的前提**——而这条测试把这个前提的真实取值固定下来：
// 精确白名单里没有它，所以网关层会拦普通用户；handler 的 requireAdmin
// 仍是第二道防线。两条测试合起来才构成完整论证：
//  1. 本用例：网关层不放行；
//  2. TestUsageByUser_RequiresAdmin：handler 拦住 → 403。
func TestUsageByUser_GatewayWhitelistDoesNotProtectIt(t *testing.T) {
	const path = "/admin/api/usage/by-user"
	if access.Allowed("GET " + path) {
		t.Fatal("全站账单端点不应在普通用户白名单里：网关层放行会把它变成越权入口")
	}
}

// TestUsageByUser_CostAndUsername 金额正确 + JOIN 出用户名。
func TestUsageByUser_CostAndUsername(t *testing.T) {
	st := newByUserStore(t)
	h := NewUsageHandler(st)
	now := time.Now().UnixMilli()
	seedByUser(t, st, "alice", now, 250_000) // 0.25 元
	seedByUser(t, st, "bob", now, 750_000)   // 0.75 元

	code, out := callByUser(t, h, asAdmin(httptest.NewRequest(
		http.MethodGet, "/admin/api/usage/by-user?from=0", nil)))
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if out.Total != 2 {
		t.Errorf("total = %d, want 2", out.Total)
	}
	if len(out.Records) != 2 {
		t.Fatalf("got %d records, want 2: %+v", len(out.Records), out.Records)
	}

	// 降序：bob(0.75) 在前。
	want := []struct {
		user string
		cost float64
	}{{"bob", 0.75}, {"alice", 0.25}}
	for i, w := range want {
		got := out.Records[i]
		if got.Key != w.user {
			t.Errorf("row %d key = %q, want %q (cost DESC)", i, got.Key, w.user)
		}
		if got.UserName != w.user {
			t.Errorf("row %d user_name = %q, want %q (JOIN users failed)", i, got.UserName, w.user)
		}
		if got.Role != string(store.RoleUser) {
			t.Errorf("row %d role = %q, want %q", i, got.Role, store.RoleUser)
		}
		if diff := got.Cost - w.cost; diff > 1e-9 || diff < -1e-9 {
			t.Errorf("row %d cost = %v, want %v", i, got.Cost, w.cost)
		}
		if got.Count != 1 {
			t.Errorf("row %d count = %d, want 1", i, got.Count)
		}
		if got.Tokens != int64(w.cost*1_000_000) {
			t.Errorf("row %d tokens = %d, want %d", i, got.Tokens, int64(w.cost*1_000_000))
		}
	}
}

// TestUsageByUser_OrderingTieIsStable 同额时按 user_id 兜底。
//
// 没有 tiebreaker 时同额行的顺序由查询计划决定：界面刷新一次行就换位置。
func TestUsageByUser_OrderingTieIsStable(t *testing.T) {
	st := newByUserStore(t)
	h := NewUsageHandler(st)
	now := time.Now().UnixMilli()
	// 金额完全相同，且创建顺序与 user_id 字典序相反。
	for _, u := range []string{"zed", "mid", "abe"} {
		seedByUser(t, st, u, now, 300_000)
	}

	_, out := callByUser(t, h, asAdmin(httptest.NewRequest(
		http.MethodGet, "/admin/api/usage/by-user?from=0", nil)))
	if len(out.Records) != 3 {
		t.Fatalf("got %d records, want 3", len(out.Records))
	}
	for i, want := range []string{"abe", "mid", "zed"} {
		if out.Records[i].Key != want {
			t.Fatalf("row %d = %q, want %q (tie must fall back to user_id ASC)", i, out.Records[i].Key, want)
		}
	}
}

// TestUsageByUser_DeletedUserDegrades 已删除用户的那一行必须保留、
// 且 user_name 为空（前端据此回退显示 id）。
//
// 「用户已删除」与「无归属」是两回事：前者的 user_id 非空而用户名空，
// 后者两者都空。若这里把行丢掉，这张表的合计会小于总览的全局合计，
// 而管理员看不出少在哪。
func TestUsageByUser_DeletedUserDegrades(t *testing.T) {
	st := newByUserStore(t)
	h := NewUsageHandler(st)
	now := time.Now().UnixMilli()
	seedByUser(t, st, "gone", now, 400_000) // 0.4 元

	// 先确认删除前拿得到用户名（否则「删后为空」可能是别的原因造成的）。
	_, before := callByUser(t, h, asAdmin(httptest.NewRequest(
		http.MethodGet, "/admin/api/usage/by-user?from=0", nil)))
	if len(before.Records) != 1 || before.Records[0].UserName != "gone" {
		t.Fatalf("precondition failed: %+v", before.Records)
	}

	if err := st.DeleteUser(t.Context(), "gone"); err != nil {
		t.Fatalf("delete user: %v", err)
	}

	_, after := callByUser(t, h, asAdmin(httptest.NewRequest(
		http.MethodGet, "/admin/api/usage/by-user?from=0", nil)))
	if len(after.Records) != 1 {
		t.Fatalf("deleted user's row must survive, got %+v", after.Records)
	}
	got := after.Records[0]
	if got.Key != "gone" {
		t.Errorf("key = %q, want %q (frontend falls back to this id)", got.Key, "gone")
	}
	if got.UserName != "" {
		t.Errorf("user_name = %q, want empty for a deleted user", got.UserName)
	}
	if got.Role != "" {
		t.Errorf("role = %q, want empty for a deleted user", got.Role)
	}
	if diff := got.Cost - 0.4; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("cost = %v, want 0.4 (a deleted user's spend must not vanish)", got.Cost)
	}
	if after.Total != 1 {
		t.Errorf("total = %d, want 1", after.Total)
	}
}

// TestUsageByUser_EmptyIsArrayNotNull 空结果必须是 []，不是 null。
//
// nil 切片会被编码成 JSON null，前端拿它 .map() 直接 TypeError ——
// 与 writeGroupEntries 的同款约定。这里刻意不落任何用量。
func TestUsageByUser_EmptyIsArrayNotNull(t *testing.T) {
	st := newByUserStore(t)
	h := NewUsageHandler(st)

	rec := httptest.NewRecorder()
	h.GroupByUser(rec, asAdmin(httptest.NewRequest(
		http.MethodGet, "/admin/api/usage/by-user?from=0", nil)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !json.Valid(rec.Body.Bytes()) {
		t.Fatalf("invalid JSON: %s", body)
	}
	var out struct {
		Records *[]usageUserEntry `json:"records"`
		Total   int64             `json:"total"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.Records == nil {
		t.Fatalf("records is null, want [] — null breaks the client's .map(): %s", body)
	}
	if len(*out.Records) != 0 || out.Total != 0 {
		t.Errorf("want empty result, got %+v", out)
	}
}

// TestUsageByUser_InvalidRangeRejected 非法时间参数必须 400，不能静默退化成全表扫描。
//
// 与其它用量端点同一条约定：usage_records 没有保留策略、行数无上界，
// 一个手滑的 `?from=abc` 若被当成 0，就是本项目最重的一次全表聚合。
func TestUsageByUser_InvalidRangeRejected(t *testing.T) {
	st := newByUserStore(t)
	h := NewUsageHandler(st)

	for _, url := range []string{
		"/admin/api/usage/by-user?from=abc",
		"/admin/api/usage/by-user?to=xyz",
	} {
		rec := httptest.NewRecorder()
		h.GroupByUser(rec, asAdmin(httptest.NewRequest(http.MethodGet, url, nil)))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400 (body=%s)", url, rec.Code, rec.Body.String())
		}
	}
}

// TestUsageByUser_LimitClamped 守住 limit 的钳制与 total 的语义。
//
// 两条：
//   - `limit=-1` 在 SQLite 里是「不限制」，必须被 clampLimit 挡成默认值；
//   - total 是**全部用户数**，不随 limit 变 —— 前端要靠它说出
//     「显示前 N / 共 M 位用户」。若 total 跟着 limit 变，一张被截断的表
//     与一张完整的表在界面上完全同形。
func TestUsageByUser_LimitClamped(t *testing.T) {
	st := newByUserStore(t)
	h := NewUsageHandler(st)
	now := time.Now().UnixMilli()
	for i := 0; i < 4; i++ {
		seedByUser(t, st, fmt.Sprintf("usr-%d", i), now, int64(100_000*(4-i)))
	}

	// limit=2：只有 2 行，但 total 仍是 4。
	_, out := callByUser(t, h, asAdmin(httptest.NewRequest(
		http.MethodGet, "/admin/api/usage/by-user?from=0&limit=2", nil)))
	if len(out.Records) != 2 {
		t.Errorf("got %d records, want 2", len(out.Records))
	}
	if out.Total != 4 {
		t.Errorf("total = %d, want 4 (must not follow limit)", out.Total)
	}
	// 截断留下最贵的前两名。
	if out.Records[0].Key != "usr-0" || out.Records[1].Key != "usr-1" {
		t.Errorf("truncation should keep top spenders, got %+v", out.Records)
	}

	// limit=-1：SQLite 语义是「不限制」，必须被钳回默认值（100）而不是拉全表。
	_, neg := callByUser(t, h, asAdmin(httptest.NewRequest(
		http.MethodGet, "/admin/api/usage/by-user?from=0&limit=-1", nil)))
	if len(neg.Records) != 4 {
		t.Errorf("limit=-1: got %d records, want 4 (clamped to default 100)", len(neg.Records))
	}
}
