package admin

// 充值流水端点（GET /admin/api/topups）与充值写入口的测试。
//
// 本模块要钉住的不变量（逐条对应下面的用例）：
//   - 充值成功后库里有一条流水，且操作者署名是**当前会话身份**（不是路径里的
//     目标用户，更不是空串）。
//   - 查询对普通用户只返回自己的记录 —— 这是权限边界，漏了就是越权。
//   - limit/offset 被钳制，响应形状稳定（前端按字段名读）。
//   - 充值失败（用户不存在）时不产生流水，且响应是 404。

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/cn-maul/rosetta-gateway/internal/store"
)

// topupPage 是查询端点的响应形状。
//
// 字段名与前端 web/src/types.ts 的 TopupPage 对齐（records / total）——
// 两边各写各的必然漂移，而漂移的表现是前端读到 undefined、白屏且无报错。
type topupPage struct {
	Records []struct {
		store.Topup
		Username string `json:"username"`
	} `json:"records"`
	Total int64 `json:"total"`
}

// doTopUp 调一次充值端点，返回 recorder。
func doTopUp(t *testing.T, h *UserHandler, r *http.Request, id string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.AdjustBalance(rec, r, id)
	return rec
}

// getTopups 调一次查询端点并解出响应。
func getTopups(t *testing.T, h *UserHandler, r *http.Request) (*httptest.ResponseRecorder, topupPage) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ListTopups(rec, r)
	var page topupPage
	if rec.Code == http.StatusOK {
		decodeBody(t, rec, &page)
	}
	return rec, page
}

// ---- 1. 充值写入流水 ----

// 充值成功后必须留下一条流水，且**操作者是发起充值的管理员**。
// 这一条最容易漏的是 operator：留痕的全部意义就在「谁充的」。
func TestTopupHandler_TopUpWritesLedgerRow(t *testing.T) {
	st := newScopeStore(t)
	h := NewUserHandler(st, newTestManager(t))
	mkBalanceUser(t, st, "u1", 0, false)

	rec := doTopUp(t, h, asAdminID(jsonRequest(http.MethodPut, "/admin/api/users/u1/balance",
		strings.NewReader(`{"delta_cents":5000,"remark":"工单 42"}`)), "admin-bob"), "u1")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s, want 200", rec.Code, rec.Body.String())
	}

	list, err := st.ListTopupsByUser(t.Context(), "u1", 50, 0)
	if err != nil {
		t.Fatalf("list topups: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("流水条数 = %d, want 1（充值必须留痕）", len(list))
	}
	got := list[0]
	if got.DeltaCents != 5000 {
		t.Errorf("delta_cents = %d, want 5000", got.DeltaCents)
	}
	if got.BalanceAfter != 5000 {
		t.Errorf("balance_after = %d, want 5000", got.BalanceAfter)
	}
	if got.UserID != "u1" {
		t.Errorf("user_id = %q, want u1（被充值的人）", got.UserID)
	}
	// 关键：操作者是**发起这次充值的管理员**，不是目标用户。
	if got.OperatorID != "admin-bob" {
		t.Errorf("operator_id = %q, want admin-bob（必须是操作者，不是目标用户）", got.OperatorID)
	}
	if got.Remark != "工单 42" {
		t.Errorf("remark = %q, want 工单 42", got.Remark)
	}
}

// 扣减（负 delta）同样留痕：钱的进出两侧都要可查。
func TestTopupHandler_DeductionAlsoRecorded(t *testing.T) {
	st := newScopeStore(t)
	h := NewUserHandler(st, newTestManager(t))
	mkBalanceUser(t, st, "u1", 5000, false)

	rec := doTopUp(t, h, asAdmin(jsonRequest(http.MethodPut, "/admin/api/users/u1/balance",
		strings.NewReader(`{"delta_cents":-2000}`))), "u1")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s, want 200", rec.Code, rec.Body.String())
	}

	list, _ := st.ListTopupsByUser(t.Context(), "u1", 50, 0)
	if len(list) != 1 {
		t.Fatalf("流水条数 = %d, want 1", len(list))
	}
	if list[0].DeltaCents != -2000 || list[0].BalanceAfter != 3000 {
		t.Errorf("流水 = (delta=%d, after=%d), want (-2000, 3000)",
			list[0].DeltaCents, list[0].BalanceAfter)
	}
}

// 用户不存在 → 404，且**不留流水**。
func TestTopupHandler_UnknownUserWritesNoLedgerRow(t *testing.T) {
	st := newScopeStore(t)
	h := NewUserHandler(st, newTestManager(t))

	rec := doTopUp(t, h, asAdmin(jsonRequest(http.MethodPut, "/admin/api/users/ghost/balance",
		strings.NewReader(`{"delta_cents":5000}`))), "ghost")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d body=%s, want 404", rec.Code, rec.Body.String())
	}
	n, _ := st.CountTopupsByUser(t.Context(), "ghost")
	if n != 0 {
		t.Errorf("用户不存在却留下 %d 条流水", n)
	}
}

// ---- 2. 查询：权限收窄（本组是重点）----

// **普通用户查不到别人的充值记录** —— 这是权限边界，漏了就是越权。
func TestTopupHandler_NormalUserSeesOnlyOwn(t *testing.T) {
	st := newScopeStore(t)
	h := NewUserHandler(st, newTestManager(t))
	mkBalanceUser(t, st, "alice", 0, false)
	mkBalanceUser(t, st, "bob", 0, false)

	doTopUp(t, h, asAdmin(jsonRequest(http.MethodPut, "/admin/api/users/alice/balance",
		strings.NewReader(`{"delta_cents":1000}`))), "alice")
	doTopUp(t, h, asAdmin(jsonRequest(http.MethodPut, "/admin/api/users/bob/balance",
		strings.NewReader(`{"delta_cents":2000}`))), "bob")

	rec, page := getTopups(t, h, asUser(jsonRequest(http.MethodGet, "/admin/api/topups", nil), "alice"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s, want 200", rec.Code, rec.Body.String())
	}
	if len(page.Records) != 1 {
		t.Fatalf("alice 看到 %d 条, want 1（不该看到 bob 的）", len(page.Records))
	}
	if page.Records[0].UserID != "alice" || page.Records[0].DeltaCents != 1000 {
		t.Errorf("alice 看到的记录 = (user=%s, delta=%d), want (alice, 1000)",
			page.Records[0].UserID, page.Records[0].DeltaCents)
	}
	// total 也必须收窄 —— 它会渲染成「共 N 条」，泄露别人的条数同样是泄露。
	if page.Total != 1 {
		t.Errorf("total = %d, want 1（total 也必须按作用域收窄）", page.Total)
	}
}

// 试图用 ?user_id= 查别人必须**无效**：作用域只由会话身份决定。
// 这条把「服务端单方面决定查谁」钉成可执行断言 —— 若将来有人图省事
// 改成读参数，这个用例立刻红。
func TestTopupHandler_UserIDQueryParamIsIgnored(t *testing.T) {
	st := newScopeStore(t)
	h := NewUserHandler(st, newTestManager(t))
	mkBalanceUser(t, st, "alice", 0, false)
	mkBalanceUser(t, st, "bob", 0, false)
	doTopUp(t, h, asAdmin(jsonRequest(http.MethodPut, "/admin/api/users/bob/balance",
		strings.NewReader(`{"delta_cents":2000}`))), "bob")

	rec, page := getTopups(t, h, asUser(jsonRequest(http.MethodGet,
		"/admin/api/topups?user_id=bob", nil), "alice"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s, want 200", rec.Code, rec.Body.String())
	}
	if len(page.Records) != 0 {
		t.Fatalf("?user_id=bob 让 alice 看到了 %d 条 bob 的记录：作用域被参数劫持了", len(page.Records))
	}
	if page.Total != 0 {
		t.Errorf("total = %d, want 0（参数不得影响 total）", page.Total)
	}
}

// 管理员看**全站**充值流水（2026-10-10 改，原为「只看自己」）。
//
// # 为什么改了
//
// 原实现的理由是「钱包页是个人账本页，不存在看全站的需求」。但管理员
// **不能被充值**（自充值被 AdjustBalance 拒绝），所以按 me.ID 查得到的一定
// 是空列表 —— 那个视图恒为空，需求「管理员钱包页展示充值记录」根本落不了地。
//
// 需求定下来后，正确的作用域就是全站：管理员钱包页要回答的是
// 「我给所有用户充过多少钱」。
func TestTopupHandler_AdminSeesAllTopups(t *testing.T) {
	st := newScopeStore(t)
	h := NewUserHandler(st, newTestManager(t))
	for _, id := range []string{"u1", "u2"} {
		mkBalanceUser(t, st, id, 0, false)
		doTopUp(t, h, asAdmin(jsonRequest(http.MethodPut, "/admin/api/users/"+id+"/balance",
			strings.NewReader(`{"delta_cents":1000}`))), id)
	}

	rec, page := getTopups(t, h, asAdmin(jsonRequest(http.MethodGet, "/admin/api/topups", nil)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s, want 200", rec.Code, rec.Body.String())
	}
	if len(page.Records) != 2 || page.Total != 2 {
		t.Errorf("admin 看到 %d 条 (total=%d), want 2/2 —— 跨用户的充值台账必须是全站视图",
			len(page.Records), page.Total)
	}
	// 每条要带**各自的**用户名（跨用户视图下逐条不同，不能全填操作者自己）。
	names := map[string]bool{}
	for _, r := range page.Records {
		names[r.Username] = true
	}
	if !names["u1"] || !names["u2"] {
		t.Errorf("用户名集合 = %v, want 同时含 u1 与 u2（跨用户视图必须逐条解析）", names)
	}
}

// 反向钉住：普通用户**永远**只能看自己，哪怕请求带别人的参数。
// 全站视图放宽的只是管理员，不该顺带放宽普通用户。
func TestTopupHandler_NormalUserStillScopedToSelf(t *testing.T) {
	st := newScopeStore(t)
	h := NewUserHandler(st, newTestManager(t))
	mkBalanceUser(t, st, "alice", 0, false)
	mkBalanceUser(t, st, "bob", 0, false)
	for _, id := range []string{"alice", "bob"} {
		doTopUp(t, h, asAdmin(jsonRequest(http.MethodPut, "/admin/api/users/"+id+"/balance",
			strings.NewReader(`{"delta_cents":1000}`))), id)
	}

	// 传 ?user_id=bob 也必须被忽略 —— 作用域只由会话身份决定。
	_, page := getTopups(t, h, asUser(jsonRequest(http.MethodGet,
		"/admin/api/topups?user_id=bob", nil), "alice"))
	if len(page.Records) != 1 {
		t.Fatalf("alice 看到 %d 条, want 1（自己的）", len(page.Records))
	}
	if page.Records[0].UserID != "alice" {
		t.Errorf("记录归属 = %q, want alice —— ?user_id 必须被忽略", page.Records[0].UserID)
	}
}

// 未登录（无身份）必须 401，而不是返回空数组假装成功。
func TestTopupHandler_UnauthenticatedRejected(t *testing.T) {
	st := newScopeStore(t)
	h := NewUserHandler(st, newTestManager(t))

	rec, _ := getTopups(t, h, jsonRequest(http.MethodGet, "/admin/api/topups", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d body=%s, want 401", rec.Code, rec.Body.String())
	}
}

// 引导态合成 user（ID 为空）必须被拒：它没有真实身份，
// 「它的充值记录」无从谈起，放行等于凭空造一份数据。
func TestTopupHandler_EmptyIdentityRejected(t *testing.T) {
	st := newScopeStore(t)
	h := NewUserHandler(st, newTestManager(t))

	rec, _ := getTopups(t, h, asAdminID(jsonRequest(http.MethodGet, "/admin/api/topups", nil), ""))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d body=%s, want 401（空 id 的合成身份不得查流水）",
			rec.Code, rec.Body.String())
	}
}

// 记录里必须带用户名（前端显示「我给谁充的」），不能只有 user_id。
func TestTopupHandler_RecordCarriesUsername(t *testing.T) {
	st := newScopeStore(t)
	h := NewUserHandler(st, newTestManager(t))
	mkBalanceUser(t, st, "alice", 0, false)
	doTopUp(t, h, asAdmin(jsonRequest(http.MethodPut, "/admin/api/users/alice/balance",
		strings.NewReader(`{"delta_cents":1000}`))), "alice")

	_, page := getTopups(t, h, asUser(jsonRequest(http.MethodGet, "/admin/api/topups", nil), "alice"))
	if len(page.Records) != 1 {
		t.Fatalf("条数 = %d, want 1", len(page.Records))
	}
	if page.Records[0].Username != "alice" {
		t.Errorf("username = %q, want alice（前端要显示「我给谁充的」）", page.Records[0].Username)
	}
}

// 没有流水时返回**空数组**而不是 null：前端 `page.records.map(...)` 在
// null 上会直接抛 TypeError，白屏。
func TestTopupHandler_EmptyListIsArrayNotNull(t *testing.T) {
	st := newScopeStore(t)
	h := NewUserHandler(st, newTestManager(t))
	mkBalanceUser(t, st, "alice", 0, false)

	rec, page := getTopups(t, h, asUser(jsonRequest(http.MethodGet, "/admin/api/topups", nil), "alice"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s, want 200", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"records":[]`) {
		t.Errorf("空列表响应体 = %s, 期望 records 为空数组 []（不是 null）", rec.Body.String())
	}
	if page.Total != 0 {
		t.Errorf("total = %d, want 0", page.Total)
	}
}

// ---- 3. 分页参数 ----

// limit 必须被钳制：负数回落默认，超大值钳到上限。
// SQLite 的 `LIMIT -1` 是「不限制」，一个漏钳的入参就是一次全表物化。
func TestTopupHandler_LimitIsClamped(t *testing.T) {
	st := newScopeStore(t)
	h := NewUserHandler(st, newTestManager(t))
	mkBalanceUser(t, st, "alice", 0, false)
	for i := 0; i < 3; i++ {
		doTopUp(t, h, asAdmin(jsonRequest(http.MethodPut, "/admin/api/users/alice/balance",
			strings.NewReader(`{"delta_cents":100}`))), "alice")
	}

	// limit=2：只返回 2 条，total 仍是 3（total 是「共 N 条」的分母）。
	rec := httptest.NewRecorder()
	h.ListTopups(rec, asUser(jsonRequest(http.MethodGet, "/admin/api/topups?limit=2", nil), "alice"))
	var page topupPage
	decodeBody(t, rec, &page)
	if len(page.Records) != 2 || page.Total != 3 {
		t.Errorf("(条数=%d, total=%d), want (2,3)（total 不受 limit 影响）",
			len(page.Records), page.Total)
	}

	// 负数回落默认（不是「不限制」）。
	rec = httptest.NewRecorder()
	h.ListTopups(rec, asUser(jsonRequest(http.MethodGet, "/admin/api/topups?limit=-1", nil), "alice"))
	decodeBody(t, rec, &page)
	if len(page.Records) != 3 {
		t.Errorf("limit=-1 返回 %d 条, want 3（应回落默认 50，不是全表）", len(page.Records))
	}

	// 超大值钳到上限，仍然不会物化整表。
	rec = httptest.NewRecorder()
	h.ListTopups(rec, asUser(jsonRequest(http.MethodGet,
		"/admin/api/topups?limit=999999", nil), "alice"))
	decodeBody(t, rec, &page)
	if len(page.Records) != 3 {
		t.Errorf("超大 limit 返回 %d 条, want 3", len(page.Records))
	}
}

// 分页在不同 offset 下不得重复或漏掉同一条记录 —— 次序键必须稳定
// （store 层用 ts DESC, rowid DESC 保证，这里从 HTTP 侧再钉一次）。
func TestTopupHandler_PaginationDoesNotRepeatRows(t *testing.T) {
	st := newScopeStore(t)
	h := NewUserHandler(st, newTestManager(t))
	mkBalanceUser(t, st, "alice", 0, false)
	const n = 5
	for i := 0; i < n; i++ {
		doTopUp(t, h, asAdmin(jsonRequest(http.MethodPut, "/admin/api/users/alice/balance",
			strings.NewReader(`{"delta_cents":100}`))), "alice")
	}

	seen := map[string]int{}
	for off := 0; off < n; off += 2 {
		rec := httptest.NewRecorder()
		h.ListTopups(rec, asUser(jsonRequest(http.MethodGet,
			"/admin/api/topups?limit=2&offset="+strconv.Itoa(off), nil), "alice"))
		var page topupPage
		decodeBody(t, rec, &page)
		for _, item := range page.Records {
			seen[item.ID]++
		}
	}
	if len(seen) != n {
		t.Errorf("分页共返回 %d 条不同记录, want %d（次序键不稳定会漏或重）", len(seen), n)
	}
	for id, cnt := range seen {
		if cnt != 1 {
			t.Errorf("记录 %s 出现了 %d 次, want 1", id, cnt)
		}
	}
}
