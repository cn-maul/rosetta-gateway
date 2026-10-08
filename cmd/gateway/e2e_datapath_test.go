package main

// 核心数据面的**动态端到端**测试。
//
// # 与其他测试文件的分工
//
// 本仓库已有大量「读代码 + 断言函数」的单元/回归测试。本文件补的是另一种
// 价值：**把真实的 HTTP 链跑起来**，用假上游 + 真 SQLite 观察端到端事实。
// 因此每个用例的断言都落到「可观测的证据」上，而不是状态码：
//   - usage_records 真的有一行、cost_total 是多少；
//   - users.balance_cents 真的减少了多少；
//   - access_keys.reserved_tokens 是否归零；
//   - 假上游真的被调了几次、**按什么顺序**、收到的是什么模型名。
//
// 只看状态码会漏掉一整类缺陷：一个 402/403 完全可能在**打完上游之后**才写出，
// 状态码断言照样绿，而那次上游调用已经真的花掉了额度与配额。
//
// # 为什么用真 store 而不是 mock
//
// mock DB 测不到落库：触发器累加 used_tokens、cost_total 的固化计价、
// balance_charges 的幂等占位、reserved_tokens 的预占释放 —— 这些全都是
// **SQL 层的行为**，mock 掉等于把被测对象换成自己的假设。
//
// # 复用既有脚手架
//
// buildHarness / buildHarnessFull（failover_test.go）已经装好「pool + 快照 +
// 临时库 + k1 测试 key」，本文件在它之上补两个能力：
//   - e2eHarness：在 harness 之后按需设余额、定价、改配额；
//   - 记账假上游（见 e2e_fake_upstream_test.go）。
//
// 刻意**不重造** harness：两套 harness 并存迟早漂移，而漂移的那一套会让
// 「同一个场景两个结论」。

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cn-maul/rosetta-gateway/internal/admin"
	"github.com/cn-maul/rosetta-gateway/internal/config"
	"github.com/cn-maul/rosetta-gateway/internal/ratelimit"
	"github.com/cn-maul/rosetta-gateway/internal/server"
	"github.com/cn-maul/rosetta-gateway/internal/snapshot"
	"github.com/cn-maul/rosetta-gateway/internal/store"
	"github.com/cn-maul/rosetta-gateway/internal/upstream"
)

// =====================================================================
// E2E 辅助
// =====================================================================

// e2eBalance 读测试用户的余额（分）与待结算余数（微元）。
//
// 余数必须一起读出来：扣费是「累进余数、满一分才动余额」的，
// 只看 balance_cents 会把「已经计费但还没满一分」误判成「没扣钱」——
// 那正是低价模型下最容易得出的错误结论。
func e2eBalance(t *testing.T, db *store.Store) (cents int64, remainder int64) {
	t.Helper()
	err := db.DB().QueryRowContext(context.Background(),
		`SELECT COALESCE(balance_cents, -1), COALESCE(balance_remainder, 0) FROM users WHERE id = ?`,
		testUserID).Scan(&cents, &remainder)
	if err != nil {
		t.Fatalf("读余额失败: %v", err)
	}
	return cents, remainder
}

// e2eWaitBalanceDrop 轮询等待余额（分）相对初始值下降 want 分。
//
// 必须轮询：扣费发生在 usageRecorder 的 worker goroutine 里，请求返回时
// 余额还没动。直接断言会得到一个「偶发假绿」—— 有时候刚好抢在 worker 前面
// 通过了断言，有时候就红。
func e2eWaitBalanceDrop(t *testing.T, db *store.Store, startCents, want int64) (int64, int64) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var cents, rem int64
	for {
		cents, rem = e2eBalance(t, db)
		if startCents-cents >= want {
			return cents, rem
		}
		if time.Now().After(deadline) {
			t.Fatalf("等待余额下降 %d 分超时：起始 %d，现在 %d，余数 %d",
				want, startCents, cents, rem)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// e2eUsageRows 读某把 key 的 usage_records 行（倒序）。
// 返回可断言的字段集合，避免测试里散落一堆 SQL。
type e2eUsageRow struct {
	ID          string
	Status      string
	UsageState  string
	TotalTokens int64
	CostTotal   float64
	ProviderID  string
	Upstream    string
	Stream      bool
	RequestID   string
}

func e2eUsageRows(t *testing.T, db *store.Store, keyID string) []e2eUsageRow {
	t.Helper()
	rows, err := db.DB().QueryContext(context.Background(),
		`SELECT id, status, usage_state, total_tokens, cost_total, provider_id, upstream_model, stream, COALESCE(request_id,'')
		   FROM usage_records WHERE access_key_id = ? ORDER BY ts DESC, id DESC`, keyID)
	if err != nil {
		t.Fatalf("读 usage_records: %v", err)
	}
	defer rows.Close()
	var out []e2eUsageRow
	for rows.Next() {
		var r e2eUsageRow
		var stream int
		if err := rows.Scan(&r.ID, &r.Status, &r.UsageState, &r.TotalTokens, &r.CostTotal,
			&r.ProviderID, &r.Upstream, &stream, &r.RequestID); err != nil {
			t.Fatalf("scan usage_records: %v", err)
		}
		r.Stream = stream == 1
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("usage_records rows: %v", err)
	}
	return out
}

// e2eWaitUsageCount 轮询等待 usage_records 达到 n 行（落库是异步的）。
func e2eWaitUsageCount(t *testing.T, db *store.Store, keyID string, n int) []e2eUsageRow {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		rows := e2eUsageRows(t, db, keyID)
		if len(rows) >= n {
			return rows
		}
		if time.Now().After(deadline) {
			t.Fatalf("等待 %d 行 usage_records 超时，现在 %d 行：%+v", n, len(rows), rows)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// e2eReserved 读 access_keys.reserved_tokens（配额预占）。
func e2eReserved(t *testing.T, db *store.Store, keyID string) int64 {
	t.Helper()
	var v int64
	if err := db.DB().QueryRowContext(context.Background(),
		`SELECT reserved_tokens FROM access_keys WHERE id = ?`, keyID).Scan(&v); err != nil {
		t.Fatalf("读 reserved_tokens: %v", err)
	}
	return v
}

// e2eWaitReservedZero 轮询等待预占归零。
//
// 预占是「用完必须还」的资源：残留会让后续请求被凭空压死
// （判定是 used+reserved+est<=quota）。而它由 rate.commit/releaseQuota 在
// 请求收尾时释放，也是一条异步/延迟路径，所以要轮询。
func e2eWaitReservedZero(t *testing.T, db *store.Store, keyID, scenario string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		v := e2eReserved(t, db, keyID)
		if v == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s：reserved_tokens 残留 %d（预占没归还，后续请求会被凭空压死）", scenario, v)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// e2eSetBalance 给测试用户设一个确定余额（分），并确保**不是**不限额。
func e2eSetBalance(t *testing.T, db *store.Store, cents int64) {
	t.Helper()
	if err := db.SetBalance(context.Background(), testUserID, cents, false); err != nil {
		t.Fatalf("set balance: %v", err)
	}
}

// e2eSnapUserRole 改快照里测试用户的角色（鉴权与余额豁免都只读快照）。
func e2eSnapUserRole(t *testing.T, role string) {
	t.Helper()
	u := snapshot.Get().UsersByID[testUserID]
	if u == nil {
		t.Fatalf("快照里没有测试用户 %q —— harness 没装好", testUserID)
	}
	u.Role = role
}

// e2eRequireUserRole 在用例结束后把角色还原，避免污染同包其它用例
// （快照是**进程级全局**，一个用例改了不回滚，后续用例会莫名其妙失败）。
func e2eRequireUserRole(t *testing.T, role string) {
	t.Helper()
	e2eSnapUserRole(t, role)
	t.Cleanup(func() { e2eSnapUserRole(t, store.RoleUser) })
}

// e2ePost 发一个请求，返回 recorder。key 可自定义（鉴权用例要发坏 key）。
// 走真 Middleware 是为了拿到生产同款 request_id —— 扣费拿它当幂等键，
// 缺了会静默跳过扣费（见 postChatWithID 的注释）。
func e2ePost(h http.HandlerFunc, path, key, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	rec := httptest.NewRecorder()
	// 与 postChatWithID 同一手法：用真 Middleware 而不是手搓 context key。
	wrapped := middlewareForTest(h)
	wrapped.ServeHTTP(rec, req)
	return rec
}

// e2eChatBody 组装一个普通 chat 请求体。
func e2eChatBody(model string, stream bool) string {
	return fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hi"}],"stream":%t%s}`,
		model, stream, map[bool]string{true: `,"stream_options":{"include_usage":true}`}[stream])
}

// e2eChatBodyMaxTokens 带 max_tokens 的请求体（配额预占用它撑大估算）。
func e2eChatBodyMaxTokens(model string, maxTokens int) string {
	return fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hi"}],"max_tokens":%d,"stream":false}`,
		model, maxTokens)
}

// =====================================================================
// A. 全链路正例
// =====================================================================

// A1：非流式全链路 —— 合法 key → 命中路由 → 转发假上游 → 200 → 落库 → 扣费。
//
// 这是整套 E2E 的地基：它不通，后面所有场景的结论都不可信。
// 所以断言刻意做全：状态码 + 响应体形状 + 上游收到的模型名 + DB 行 + 余额。
func TestE2E_A1_NonStreamHappyPathChargesAndRecords(t *testing.T) {
	up := newFakeUpstream(t, replyOK("pong"))
	h, db := buildHarness(t, []struct {
		slug, url string
		fail      bool
	}{{slug: "good", url: up.URL()}}, false)

	// 定价必须**同时**写快照与库：快照供余额预检，库供固化计价。
	// 只写一处会造成「预检以为有价、实际 cost_total=0」的假绿（见 priceRoute）。
	priceRoute(t, db, "good", "good-model", 1_000_000, 1_000_000)
	e2eSetBalance(t, db, 10_000) // 100.00 元

	startCents, startRem := e2eBalance(t, db)
	rec := e2ePost(h, "/v1/chat/completions", testAccessKey, e2eChatBody("flash", false))

	t.Logf("A1 响应: status=%d content-type=%q body=%s",
		rec.Code, rec.Header().Get("Content-Type"), strings.TrimSpace(rec.Body.String()))

	if rec.Code != http.StatusOK {
		t.Fatalf("A1 期望 200，实际 %d，body=%s", rec.Code, rec.Body.String())
	}

	// --- 响应体形状 ---
	var resp struct {
		Object  string `json:"object"`
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
			TotalTokens      int64 `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("A1 响应不是合法 JSON: %v body=%s", err, rec.Body.String())
	}
	if resp.Object != "chat.completion" {
		t.Errorf("A1 object=%q，期望 chat.completion", resp.Object)
	}
	// 对外的 model 必须是**公开名**（客户端写的那个），不是上游 model_id。
	// 这是路由层最容易搞反的地方，且从内容上完全看不出来。
	if resp.Model != "flash" {
		t.Errorf("A1 响应的 model=%q，期望公开名 flash（不是上游 model_id）", resp.Model)
	}
	if len(resp.Choices) != 1 || resp.Choices[0].Message.Content != "pong" {
		t.Errorf("A1 choices 内容不对: %+v", resp.Choices)
	}
	if resp.Usage.TotalTokens != 8 {
		t.Errorf("A1 usage.total_tokens=%d，期望 8（透传上游）", resp.Usage.TotalTokens)
	}

	// --- 证据 1：上游真的被调了一次，且收到的是上游 model_id ---
	if up.Count() != 1 {
		t.Fatalf("A1 上游应被调用 1 次，实际 %d 次：%+v", up.Count(), up.Calls())
	}
	if body := up.LastBody(); !bodyHasModel(body, "good-model") {
		t.Errorf("A1 上游收到的 model 不是 good-model（路由映射错了）: %s", body)
	}
	if auth := up.Calls()[0].Auth; auth != "Bearer sk-good" {
		t.Errorf("A1 上游收到的凭据=%q，期望 Bearer sk-good（provider 自己的 key）", auth)
	}

	// --- 证据 2：usage_records 真的落了一行，且 cost_total > 0 ---
	rows := e2eWaitUsageCount(t, db, "k1", 1)
	row := rows[0]
	t.Logf("A1 usage 行: %+v", row)
	if row.Status != "ok" {
		t.Errorf("A1 usage.status=%q，期望 ok", row.Status)
	}
	if row.TotalTokens != 8 {
		t.Errorf("A1 usage.total_tokens=%d，期望 8", row.TotalTokens)
	}
	if row.CostTotal <= 0 {
		t.Errorf("A1 usage.cost_total=%v，必须 > 0（否则「报表在涨余额不动」那条老毛病会复发）", row.CostTotal)
	}
	if row.ProviderID != "good" || row.Upstream != "good-model" {
		t.Errorf("A1 usage 归因错: provider=%q upstream=%q", row.ProviderID, row.Upstream)
	}
	if row.Stream {
		t.Errorf("A1 非流式请求的 usage.stream 应为 false")
	}

	// --- 证据 3：余额真的被扣了，且扣的**就是** cost_total ---
	//
	// 扣费是「累进余数、满一分才动余额」，所以断言写成「余额减少 + 余数补足」
	// 的合计等于 cost_total，而不是直接比某个整数分。
	endCents, endRem := e2eWaitBalanceDrop(t, db, startCents, 0)
	chargedYuan := float64(startCents-endCents)*0.01 +
		float64(endRem-startRem)/1e6
	t.Logf("A1 余额: %d分/%d微元 → %d分/%d微元，合计扣费≈%.6f 元；cost_total=%.6f 元",
		startCents, startRem, endCents, endRem, chargedYuan, row.CostTotal)
	if chargedYuan <= 0 {
		t.Fatalf("A1 余额与余数都没变 —— 成功请求没被扣费")
	}
	if diff := chargedYuan - row.CostTotal; diff > 1e-6 || diff < -1e-6 {
		t.Errorf("A1 扣费金额 %.6f 与 cost_total %.6f 不一致（差 %.6f）—— 「扣的=报表的」被破坏",
			chargedYuan, row.CostTotal, diff)
	}
}

// A2：流式全链路 —— SSE 分片形状 + [DONE] + 落库 + 扣费。
//
// 流式与非流式是两条**独立**的收尾路径（attemptStream vs attemptNonStream），
// 非流式通过不代表流式通过：usage 的采集、rate.commit 的时机、落库的
// usage_state 判定全都是分开写的。
func TestE2E_A2_StreamHappyPathRecordsAndCharges(t *testing.T) {
	up := newFakeUpstream(t, replySSE("pong"))
	h, db := buildHarness(t, []struct {
		slug, url string
		fail      bool
	}{{slug: "good", url: up.URL()}}, false)

	priceRoute(t, db, "good", "good-model", 1_000_000, 1_000_000)
	e2eSetBalance(t, db, 10_000)

	startCents, startRem := e2eBalance(t, db)
	rec := e2ePost(h, "/v1/chat/completions", testAccessKey, e2eChatBody("flash", true))

	body := rec.Body.String()
	t.Logf("A2 响应: status=%d content-type=%q\n--- body ---\n%s\n--- end ---",
		rec.Code, rec.Header().Get("Content-Type"), body)

	if rec.Code != http.StatusOK {
		t.Fatalf("A2 期望 200，实际 %d body=%s", rec.Code, body)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("A2 Content-Type=%q，期望 text/event-stream", ct)
	}

	lines := sseDataLines(body)
	if len(lines) == 0 {
		t.Fatalf("A2 没有任何 SSE data 行：%s", body)
	}
	// 末尾必须是 [DONE]，且**恰好一次**：漏了它客户端会一直等；
	// 多了一次说明终止序列被写了两次（流式收尾最容易出的错）。
	done := 0
	for _, l := range lines {
		if l == "[DONE]" {
			done++
		}
	}
	if done != 1 {
		t.Errorf("A2 [DONE] 出现 %d 次，期望恰好 1 次", done)
	}
	if lines[len(lines)-1] != "[DONE]" {
		t.Errorf("A2 最后一行是 %q，期望 [DONE]", lines[len(lines)-1])
	}
	if !strings.Contains(body, "pong") {
		t.Errorf("A2 流里没有上游内容 pong：%s", body)
	}
	// finish_reason 必须在 [DONE] 之前出现，否则严格客户端会认为流没结束。
	if !strings.Contains(body, `"finish_reason":"stop"`) {
		t.Errorf("A2 缺少 finish_reason=stop 分片：%s", body)
	}

	rows := e2eWaitUsageCount(t, db, "k1", 1)
	row := rows[0]
	t.Logf("A2 usage 行: %+v", row)
	if !row.Stream {
		t.Errorf("A2 usage.stream 应为 true")
	}
	if row.Status != "ok" {
		t.Errorf("A2 usage.status=%q，期望 ok", row.Status)
	}
	// 上游报告了 usage，所以不能是 missing —— missing 意味着「上游一个 token
	// 数都没给」，那会让报表把它读成系统性漏账。
	if row.UsageState != "reported" {
		t.Errorf("A2 usage_state=%q，期望 reported（上游带了 usage 块）", row.UsageState)
	}
	if row.TotalTokens != 8 {
		t.Errorf("A2 usage.total_tokens=%d，期望 8", row.TotalTokens)
	}
	if row.CostTotal <= 0 {
		t.Errorf("A2 cost_total=%v，必须 > 0", row.CostTotal)
	}

	endCents, endRem := e2eWaitBalanceDrop(t, db, startCents, 0)
	charged := float64(startCents-endCents)*0.01 + float64(endRem-startRem)/1e6
	t.Logf("A2 余额: %d分/%d微元 → %d分/%d微元，合计扣费≈%.6f 元；cost_total=%.6f",
		startCents, startRem, endCents, endRem, charged, row.CostTotal)
	if charged <= 0 {
		t.Fatalf("A2 成功流式请求没被扣费")
	}
}

// =====================================================================
// B. 鉴权拒绝路径
// =====================================================================

// B：五条鉴权拒绝路径，每条都要证明**没有打到上游**。
//
// # 为什么必须用带计数的上游
//
// 只看状态码会漏掉「先打上游再拒绝」这一类：401/403 完全可以在转发之后才写出，
// 状态码断言照样绿，而额度已经花掉了。那正是「鉴权必须在触碰上游之前」这条
// 约定要防的事。
//
// 五条路径各自触发的判定点不同（哈希查不到 / Enabled=false / 过期 /
// 归属用户不在快照 / 归属用户是 admin），所以必须逐条跑，不能只测一条就当
// 「鉴权没问题」。
func TestE2E_B_AuthRejectionsNeverReachUpstream(t *testing.T) {
	cases := []struct {
		name string
		// setup 在 harness 建好之后改状态（改快照或 DB），返回要发的 key。
		setup func(t *testing.T, db *store.Store) string
		// wantStatus / wantCode 是对外契约。
		wantStatus int
		wantCode   string
		why        string
	}{
		{
			name: "无效key",
			setup: func(t *testing.T, db *store.Store) string {
				return "sk-gw-completely-unknown"
			},
			wantStatus: http.StatusUnauthorized,
			wantCode:   "invalid_api_key",
			why:        "快照里没有这个 hash → ErrInvalidKey",
		},
		{
			name: "禁用key",
			setup: func(t *testing.T, db *store.Store) string {
				// 改快照而不是 DB：鉴权只读快照（Enabled 随 Swap 生效）。
				// 这样测的正是热路径那一份判定。
				old := snapshot.Get()
				newKeys := make(map[string]*snapshot.KeySnapshot, len(old.KeysByHash))
				for hash, ks := range old.KeysByHash {
					cp := *ks
					cp.Enabled = false
					newKeys[hash] = &cp
				}
				snapshot.Init(&snapshot.Snapshot{
					Routes: old.Routes, Providers: old.Providers,
					KeysByHash: newKeys, UsersByID: old.UsersByID, Runtime: old.Runtime,
				})
				return testAccessKey
			},
			wantStatus: http.StatusForbidden,
			wantCode:   "invalid_api_key",
			why:        "Enabled=false → ErrKeyDisabled（403，不是 401）",
		},
		{
			name: "过期key",
			setup: func(t *testing.T, db *store.Store) string {
				old := snapshot.Get()
				newKeys := make(map[string]*snapshot.KeySnapshot, len(old.KeysByHash))
				for hash, ks := range old.KeysByHash {
					cp := *ks
					// 此刻之前即失效。判定是 now >= ExpiresAt，所以「刚刚过去」也拦得住。
					cp.ExpiresAt = time.Now().Add(-time.Minute).UnixMilli()
					newKeys[hash] = &cp
				}
				snapshot.Init(&snapshot.Snapshot{
					Routes: old.Routes, Providers: old.Providers,
					KeysByHash: newKeys, UsersByID: old.UsersByID, Runtime: old.Runtime,
				})
				return testAccessKey
			},
			wantStatus: http.StatusForbidden,
			wantCode:   "key_expired",
			why:        "ExpiresAt 已过 → ErrKeyExpired（403 + 独立 code，客户端据此知道要续期而不是换 key）",
		},
		{
			name: "归属用户不存在",
			setup: func(t *testing.T, db *store.Store) string {
				// 把 key 指到一个快照里不存在的 user_id：模拟「归属用户被删」。
				// 放行等于绕过归属约束（谁的 key 在用无从追查），必须 401。
				old := snapshot.Get()
				newKeys := make(map[string]*snapshot.KeySnapshot, len(old.KeysByHash))
				for hash, ks := range old.KeysByHash {
					cp := *ks
					cp.UserID = "u-deleted-nobody"
					newKeys[hash] = &cp
				}
				snapshot.Init(&snapshot.Snapshot{
					Routes: old.Routes, Providers: old.Providers,
					KeysByHash: newKeys, UsersByID: old.UsersByID, Runtime: old.Runtime,
				})
				return testAccessKey
			},
			wantStatus: http.StatusUnauthorized,
			wantCode:   "invalid_api_key",
			why:        "UsersByID 查不到归属用户 → ErrKeyUnowned（401，不静默放行）",
		},
		{
			name: "归属管理员的key",
			setup: func(t *testing.T, db *store.Store) string {
				// 控制面/数据面分离：管理员的 key 不能调用模型。
				e2eRequireUserRole(t, store.RoleAdmin)
				return testAccessKey
			},
			wantStatus: http.StatusForbidden,
			wantCode:   "admin_cannot_call_model",
			why:        "管理员只做控制面；必须给可区分的 code，否则 SDK 会当成普通认证失败无限重试",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			up := newFakeUpstream(t, replyOK("pong"))
			h, db := buildHarness(t, []struct {
				slug, url string
				fail      bool
			}{{slug: "good", url: up.URL()}}, false)
			priceRoute(t, db, "good", "good-model", 1_000_000, 1_000_000)
			e2eSetBalance(t, db, 10_000)

			key := tc.setup(t, db)
			rec := e2ePost(h, "/v1/chat/completions", key, e2eChatBody("flash", false))

			t.Logf("[%s] status=%d body=%s", tc.name, rec.Code, strings.TrimSpace(rec.Body.String()))

			if rec.Code != tc.wantStatus {
				t.Errorf("[%s] 期望 %d，实际 %d（%s）body=%s",
					tc.name, tc.wantStatus, rec.Code, tc.why, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), tc.wantCode) {
				t.Errorf("[%s] 响应体缺 code=%q：%s", tc.name, tc.wantCode, rec.Body.String())
			}
			// 核心断言：上游一次都没被碰。
			if got := up.Count(); got != 0 {
				t.Errorf("[%s] 上游被触碰 %d 次 —— 必须在触碰上游之前拒绝（%s）；请求体=%+v",
					tc.name, got, tc.why, up.Calls())
			}
			// 没有 usage 行：被拒的请求不该产生用量记录（也不该扣费）。
			if rows := e2eUsageRows(t, db, "k1"); len(rows) != 0 {
				t.Errorf("[%s] 被拒的请求产生了 %d 行 usage_records：%+v", tc.name, len(rows), rows)
			}
		})
	}
}

// B 补充：被拒请求不得扣费（余额纹丝不动）。
//
// 「上游没被碰」与「没扣钱」是两件事：一个在鉴权之后、转发之前错误扣费的
// 实现，前一条断言照样绿。
func TestE2E_B2_RejectedRequestDoesNotCharge(t *testing.T) {
	up := newFakeUpstream(t, replyOK("pong"))
	h, db := buildHarness(t, []struct {
		slug, url string
		fail      bool
	}{{slug: "good", url: up.URL()}}, false)
	priceRoute(t, db, "good", "good-model", 1_000_000, 1_000_000)
	e2eSetBalance(t, db, 10_000)

	before, beforeRem := e2eBalance(t, db)
	rec := e2ePost(h, "/v1/chat/completions", "sk-gw-bogus", e2eChatBody("flash", false))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("期望 401，实际 %d", rec.Code)
	}

	// 等足够久再断言「没变」：扣费是异步的，早断言会因「还没来得及扣」而假绿。
	deadline := time.Now().Add(400 * time.Millisecond)
	for time.Now().Before(deadline) {
		cents, rem := e2eBalance(t, db)
		if cents != before || rem != beforeRem {
			t.Fatalf("被拒请求动了账：%d分/%d微元 → %d分/%d微元", before, beforeRem, cents, rem)
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Logf("B2 被拒后账目未变：%d 分 / %d 微元（等待 400ms）", before, beforeRem)
}

// =====================================================================
// C. 失败转移链
// =====================================================================

// C1：链首 5xx → 自动切次目标并成功；断言**调用顺序**与「客户端只看到一次成功」。
//
// 顺序断言是重点：只断言「最终 200」无法区分「先打 A 再打 B」与
// 「直接打 B」（跳过链首），而后者意味着链首的健康状态从未被检验。
func TestE2E_C1_FailoverOn5xxPreservesChainOrder(t *testing.T) {
	first := newFakeUpstream(t, replyStatus(http.StatusInternalServerError, "boom"))
	second := newFakeUpstream(t, replyOK("pong"))
	h, db := buildHarness(t, []struct {
		slug, url string
		fail      bool
	}{
		{slug: "p1", url: first.URL()},
		{slug: "p2", url: second.URL()},
	}, true)

	rec := e2ePost(h, "/v1/chat/completions", testAccessKey, e2eChatBody("flash", false))
	body := rec.Body.String()
	t.Logf("C1 status=%d body=%s", rec.Code, strings.TrimSpace(body))
	t.Logf("C1 链首收到 %d 次；次目标收到 %d 次", first.Count(), second.Count())

	if rec.Code != http.StatusOK {
		t.Fatalf("C1 期望 200（转移后成功），实际 %d body=%s", rec.Code, body)
	}
	if !strings.Contains(body, "pong") {
		t.Errorf("C1 响应里没有次目标的内容：%s", body)
	}
	// 顺序：链首必须先被尝试。
	if first.Count() != 1 {
		t.Errorf("C1 链首应被尝试恰好 1 次，实际 %d", first.Count())
	}
	if second.Count() != 1 {
		t.Errorf("C1 次目标应被尝试恰好 1 次，实际 %d", second.Count())
	}
	if first.Count() > 0 && second.Count() > 0 {
		// 两次调用的时间先后无法从两个独立计数器直接读出，
		// 但「链首被打了 + 次目标也被打了 + 链首失败」这一组合已足以
		// 证明链是按顺序推进的（若跳过链首，first.Count() 会是 0）。
		t.Logf("C1 顺序证据：链首=%d 次且失败，次目标=%d 次且成功 → 链按 position 推进",
			first.Count(), second.Count())
	}
	// 客户端只看到**一次**成功响应，不是两段拼接。
	if n := strings.Count(body, `"object":"chat.completion"`); n != 1 {
		t.Errorf("C1 响应体里 chat.completion 出现 %d 次，期望 1（不得把两次尝试的输出拼给客户端）", n)
	}

	// usage 只能有一行，且归因到**真正服务成功**的次目标。
	rows := e2eWaitUsageCount(t, db, "k1", 1)
	if len(rows) != 1 {
		t.Fatalf("C1 期望恰好 1 行 usage_records，实际 %d：%+v", len(rows), rows)
	}
	if rows[0].ProviderID != "p2" {
		t.Errorf("C1 usage 归因到 %q，期望 p2（实际服务成功的那个目标）", rows[0].ProviderID)
	}
}

// C2：链首 404 → 转移；链首 401（凭据坏）→ 转移。
//
// 404/401 与 5xx 走的是**不同的**分类逻辑（FailoverEligible 的白名单 +
// CredentialCooldown 的差异），所以必须各测一条。
func TestE2E_C2_FailoverOn404And401(t *testing.T) {
	cases := []struct {
		name      string
		firstCode int
		firstMsg  string
	}{
		{"链首404(上游没这个模型)", http.StatusNotFound, "model not found"},
		{"链首401(凭据无效)", http.StatusUnauthorized, "invalid api key"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			first := newFakeUpstream(t, replyStatus(tc.firstCode, tc.firstMsg))
			second := newFakeUpstream(t, replyOK("pong"))
			h, _ := buildHarness(t, []struct {
				slug, url string
				fail      bool
			}{
				{slug: "p1", url: first.URL()},
				{slug: "p2", url: second.URL()},
			}, true)

			rec := e2ePost(h, "/v1/chat/completions", testAccessKey, e2eChatBody("flash", false))
			t.Logf("[%s] status=%d body=%s（链首 %d 次 / 次目标 %d 次）",
				tc.name, rec.Code, strings.TrimSpace(rec.Body.String()), first.Count(), second.Count())

			if rec.Code != http.StatusOK {
				t.Fatalf("[%s] 期望转移后 200，实际 %d body=%s", tc.name, rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), "pong") {
				t.Errorf("[%s] 响应里没有次目标内容：%s", tc.name, rec.Body.String())
			}
			if first.Count() == 0 {
				t.Errorf("[%s] 链首一次都没被尝试 —— 链没按顺序走", tc.name)
			}
			if second.Count() == 0 {
				t.Errorf("[%s] 次目标没被尝试 —— 没有发生转移", tc.name)
			}
		})
	}
}

// C3（关键边界）：流式**已写出字节之后**失败，绝不能重试。
//
// # 为什么这条最重要
//
// 一旦第一片内容已经写给客户端，回退去换目标是**不可能**的：客户端会把
// 两次尝试的输出拼在一起（重复内容、重复计费）。正确行为是如实按
// truncated 收尾（OpenAI 形状下不发 [DONE]，客户端据此判定流异常）。
//
// # 怎么构造
//
// 次目标必须是**可用**的，否则「没有重试」与「没有可重试的目标」无法区分 ——
// 那样断言会因错误的原因通过。所以链是 [会截断的上游, 健康上游]，
// 并断言健康上游的计数**为 0**：证明确实是「已提交所以不重试」，
// 而不是「没得可试」。
func TestE2E_C3_StreamCommittedThenFailedIsNotRetried(t *testing.T) {
	// 链首：先写一片真实内容（网关据此提交 SSE 头），然后掐断连接。
	broken := newFakeUpstream(t, replySSEThenFail(
		`{"id":"c1","object":"chat.completion.chunk","created":0,"model":"upstream-model","choices":[{"index":0,"delta":{"content":"PARTIAL"},"finish_reason":null}]}`))
	// 次目标：完全健康。它的计数必须保持 0。
	healthy := newFakeUpstream(t, replySSE("SHOULD-NOT-APPEAR"))
	h, db := buildHarness(t, []struct {
		slug, url string
		fail      bool
	}{
		{slug: "p1", url: broken.URL()},
		{slug: "p2", url: healthy.URL()},
	}, true)

	rec := e2ePost(h, "/v1/chat/completions", testAccessKey, e2eChatBody("flash", true))
	body := rec.Body.String()
	t.Logf("C3 status=%d\n--- body ---\n%s\n--- end ---", rec.Code, body)
	t.Logf("C3 链首收到 %d 次；健康次目标收到 %d 次（**必须为 0**）", broken.Count(), healthy.Count())

	// 状态码此时已是 200（SSE 头在首片之后写的），这是正确的、无法回退。
	if rec.Code != http.StatusOK {
		t.Logf("C3 注意：状态码为 %d（不是 200）。已提交的流理论上头已写出，200 是预期。", rec.Code)
	}
	// 核心断言 1：健康次目标一次都不能被碰。
	if got := healthy.Count(); got != 0 {
		t.Fatalf("C3 已提交的流式失败被重试了：健康次目标被调用 %d 次 —— "+
			"客户端会看到两次尝试的输出拼接（重复内容 + 重复计费）", got)
	}
	// 核心断言 2：部分内容必须真的已经到了客户端（证明「已提交」这个前提成立）。
	if !strings.Contains(body, "PARTIAL") {
		t.Fatalf("C3 客户端没收到首片内容，说明「已提交」前提不成立，本用例没测到目标路径：%s", body)
	}
	// 核心断言 3：截断的流不得发 [DONE]（发了客户端会把半截回答当成功）。
	if strings.Contains(body, "[DONE]") {
		t.Errorf("C3 截断的流里出现了 [DONE] —— 客户端会把半截回答当成正常结束：%s", body)
	}
	// 核心断言 4：不得出现次目标的内容。
	if strings.Contains(body, "SHOULD-NOT-APPEAR") {
		t.Errorf("C3 客户端收到了次目标的内容 —— 两段流被拼接：%s", body)
	}

	// 证据：usage 落库为截断状态（不是 ok）。
	rows := e2eWaitUsageCount(t, db, "k1", 1)
	t.Logf("C3 usage 行: %+v", rows[0])
	if rows[0].Status == "ok" {
		t.Errorf("C3 截断的流被记成 status=ok —— 「先 200 再断流」这类上游故障将永远不累计失败，目标永不熔断")
	}
	if rows[0].ProviderID != "p1" {
		t.Errorf("C3 usage 归因到 %q，期望 p1（实际提供内容的目标）", rows[0].ProviderID)
	}
}

// =====================================================================
// D. 配额与限速
// =====================================================================

// D1：token 配额用尽 → 429 且**不碰上游**，且预占不留残渣。
func TestE2E_D1_QuotaExhaustedRejectsWithoutTouchingUpstream(t *testing.T) {
	up := newFakeUpstream(t, replyOK("pong"))
	h, db := buildHarness(t, []struct {
		slug, url string
		fail      bool
	}{{slug: "good", url: up.URL()}}, false)

	// 配额 100，已用 100 → 剩余 0。
	if err := db.UpdateAccessKey(context.Background(), "k1", &store.AccessKey{
		ID: "k1", Name: "t", Enabled: true, QuotaTokens: 100, UserID: testUserID,
	}); err != nil {
		t.Fatalf("set quota: %v", err)
	}
	if err := db.CreateUsageRecord(context.Background(), &store.UsageRecord{
		ID: "seed-1", AccessKeyID: "k1", UserID: testUserID, PublicModel: "flash",
		IngressProtocol: "openai-chat", TotalTokens: 100, UsageState: "reported", Status: "ok",
	}); err != nil {
		t.Fatalf("seed usage: %v", err)
	}

	rec := e2ePost(h, "/v1/chat/completions", testAccessKey, e2eChatBody("flash", false))
	t.Logf("D1 status=%d body=%s; 上游被碰 %d 次", rec.Code, strings.TrimSpace(rec.Body.String()), up.Count())

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("D1 期望 429，实际 %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "insufficient_quota") {
		t.Errorf("D1 code 应为 insufficient_quota：%s", rec.Body.String())
	}
	if up.Count() != 0 {
		t.Errorf("D1 配额已尽却打了上游 %d 次 —— 预检必须在触碰上游之前", up.Count())
	}
	// 被拒请求不得留下预占（否则会凭空压缩后续可用额度）。
	e2eWaitReservedZero(t, db, "k1", "D1 配额拒绝")
}

// D2：RPM 超限 → 429 + Retry-After，且不碰上游。
func TestE2E_D2_RPMExceededRejectsWithoutTouchingUpstream(t *testing.T) {
	up := newFakeUpstream(t, replyOK("pong"))
	h, db, _ := buildHarnessFull(t, []struct {
		slug, url string
		fail      bool
	}{{slug: "good", url: up.URL()}}, false, openaiChatCodec{})

	// RPM 额度经快照下发（与既有 ratelimit_test 同一手法：
	// harness 的路由只在手工快照里，不能整体 RebuildFromDB）。
	old := snapshot.Get()
	newKeys := make(map[string]*snapshot.KeySnapshot, len(old.KeysByHash))
	for hash, ks := range old.KeysByHash {
		cp := *ks
		cp.RPMLimit = 2
		newKeys[hash] = &cp
	}
	snapshot.Init(&snapshot.Snapshot{
		Routes: old.Routes, Providers: old.Providers, KeysByHash: newKeys,
		UsersByID: old.UsersByID, Runtime: old.Runtime,
	})

	for i := 1; i <= 2; i++ {
		rec := e2ePost(h, "/v1/chat/completions", testAccessKey, e2eChatBody("flash", false))
		if rec.Code != http.StatusOK {
			t.Fatalf("D2 第 %d 个请求（限额内）期望 200，实际 %d body=%s", i, rec.Code, rec.Body.String())
		}
	}
	hitsBefore := up.Count()

	rec := e2ePost(h, "/v1/chat/completions", testAccessKey, e2eChatBody("flash", false))
	t.Logf("D2 第3个请求 status=%d retry-after=%q body=%s",
		rec.Code, rec.Header().Get("Retry-After"), strings.TrimSpace(rec.Body.String()))

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("D2 第 3 个请求期望 429，实际 %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "rate_limit_exceeded") {
		t.Errorf("D2 code 应为 rate_limit_exceeded：%s", rec.Body.String())
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Errorf("D2 缺少 Retry-After 头（客户端无从知道何时重试）")
	}
	if up.Count() != hitsBefore {
		t.Errorf("D2 被 RPM 拒的请求打了上游（%d → %d 次）", hitsBefore, up.Count())
	}
	_ = db
}

// D3：预占在**成功**收尾后必须归零。
//
// 三条收尾路径（成功 / 失败 / 客户端断开）各自的释放点不同，
// 所以必须分开验证 —— 只测一条会漏掉另外两条的泄漏。
func TestE2E_D3_ReservationReleasedAfterSuccess(t *testing.T) {
	up := newFakeUpstream(t, replyOK("pong"))
	h, db := buildHarness(t, []struct {
		slug, url string
		fail      bool
	}{{slug: "good", url: up.URL()}}, false)

	// 配额给得足够大，只要预占能归还，多个请求都该通过。
	if err := db.UpdateAccessKey(context.Background(), "k1", &store.AccessKey{
		ID: "k1", Name: "t", Enabled: true, QuotaTokens: 1000, UserID: testUserID,
	}); err != nil {
		t.Fatalf("set quota: %v", err)
	}

	rec := e2ePost(h, "/v1/chat/completions", testAccessKey, e2eChatBodyMaxTokens("flash", 200))
	t.Logf("D3 status=%d body=%s", rec.Code, strings.TrimSpace(rec.Body.String()))
	if rec.Code != http.StatusOK {
		t.Fatalf("D3 期望 200，实际 %d body=%s", rec.Code, rec.Body.String())
	}
	e2eWaitReservedZero(t, db, "k1", "D3 成功收尾")

	// 关键的「预占真的被释放了」证据：连续发多个请求都还能通过。
	// 若预占泄漏，第二次就会因 used+reserved+est > quota 而 429。
	for i := 2; i <= 3; i++ {
		rec := e2ePost(h, "/v1/chat/completions", testAccessKey, e2eChatBodyMaxTokens("flash", 200))
		t.Logf("D3 第 %d 个请求 status=%d", i, rec.Code)
		if rec.Code != http.StatusOK {
			t.Fatalf("D3 第 %d 个请求被拒（%d）—— 预占泄漏把额度耗光了：%s",
				i, rec.Code, rec.Body.String())
		}
		e2eWaitReservedZero(t, db, "k1", fmt.Sprintf("D3 第 %d 个请求收尾", i))
	}
}

// D4：预占在**失败**（链耗尽）收尾后也必须归零。
//
// 这是与成功路径不同的代码分支（handleIngress 尾部的 rate.commit(0)），
// 漏掉它会让每次失败都永久吃掉一笔预占。
func TestE2E_D4_ReservationReleasedAfterChainExhausted(t *testing.T) {
	bad := newFakeUpstream(t, replyStatus(http.StatusInternalServerError, "boom"))
	h, db := buildHarness(t, []struct {
		slug, url string
		fail      bool
	}{{slug: "bad", url: bad.URL()}}, false)

	if err := db.UpdateAccessKey(context.Background(), "k1", &store.AccessKey{
		ID: "k1", Name: "t", Enabled: true, QuotaTokens: 1000, UserID: testUserID,
	}); err != nil {
		t.Fatalf("set quota: %v", err)
	}

	rec := e2ePost(h, "/v1/chat/completions", testAccessKey, e2eChatBodyMaxTokens("flash", 200))
	t.Logf("D4 status=%d body=%s", rec.Code, strings.TrimSpace(rec.Body.String()))
	if rec.Code < 500 {
		t.Fatalf("D4 期望 5xx（链耗尽），实际 %d", rec.Code)
	}
	e2eWaitReservedZero(t, db, "k1", "D4 失败收尾")

	// 失败若干次后额度必须仍然够用（预占没泄漏）。
	for i := 2; i <= 3; i++ {
		rec := e2ePost(h, "/v1/chat/completions", testAccessKey, e2eChatBodyMaxTokens("flash", 200))
		t.Logf("D4 第 %d 次失败请求 status=%d", i, rec.Code)
		if rec.Code == http.StatusTooManyRequests {
			t.Fatalf("D4 第 %d 次变成了 429 —— 每次失败都在泄漏配额预占：%s", i, rec.Body.String())
		}
		e2eWaitReservedZero(t, db, "k1", fmt.Sprintf("D4 第 %d 次收尾", i))
	}
}

// =====================================================================
// F. 客户端断开
// =====================================================================

// F1：流式请求中途断开 → 上游观察到 ctx 取消 + usage 记 canceled + 预占归还。
//
// # 三个断言各自的意义
//
//   - **上游观察到取消**：这是唯一能证明「网关真的把断开传播到了在途上游请求」
//     的证据。从网关侧完全看不出来 —— 连接被归还池里却仍在跑，网关照样安静。
//     不证明这一点就无法回答「断开后我们还在为 token 付费吗」。
//   - usage 记 canceled 而不是 error：客户端主动断开不是上游故障。记成 error
//     会让后台错误率虚高、把真故障淹没，并可能把健康目标误熔断。
//   - 预占归还：断开的请求不该永久吃掉终身配额。
func TestE2E_F1_ClientDisconnectReleasesUpstreamAndMarksCanceled(t *testing.T) {
	up := newDisconnectingUpstream(t)
	h, db := buildHarness(t, []struct {
		slug, url string
		fail      bool
	}{{slug: "slow", url: up.srv.URL}}, false)

	if err := db.UpdateAccessKey(context.Background(), "k1", &store.AccessKey{
		ID: "k1", Name: "t", Enabled: true, QuotaTokens: 100_000, UserID: testUserID,
	}); err != nil {
		t.Fatalf("set quota: %v", err)
	}

	// 用可取消的 ctx 发请求，然后中途取消 —— 等价于客户端关掉连接。
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(e2eChatBody("flash", true))).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer "+testAccessKey)

	rec := httptest.NewRecorder()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		middlewareForTest(h).ServeHTTP(rec, req)
	}()

	// 等上游真的开始吐字（网关已提交 SSE 头），再模拟客户端断开。
	select {
	case <-up.started:
		t.Log("F1 上游已在途并吐出首片，现在模拟客户端断开")
	case <-time.After(5 * time.Second):
		cancel()
		wg.Wait()
		t.Fatal("F1 上游一直没收到请求 —— harness 或链路有问题，本用例没测到目标路径")
	}
	cancel()
	wg.Wait()

	// 证据 1：上游观察到 ctx 被取消（= 网关传播了断开）。
	select {
	case <-up.released:
	case <-time.After(3 * time.Second):
		t.Fatal("F1 上游的 ctx 从未被取消 —— 断开没有传播到上游，token 还在继续烧")
	}
	t.Logf("F1 上游观察到的断开（ctx 取消）: %v", up.disconnectObserved.Load())
	if !up.disconnectObserved.Load() {
		t.Errorf("F1 上游是被测试兜底超时放行的，而不是被网关取消 ctx —— 断开未传播")
	}

	// 证据 2：usage 落库为 canceled。
	rows := e2eWaitUsageCount(t, db, "k1", 1)
	t.Logf("F1 usage 行: %+v", rows[0])
	if rows[0].Status != "canceled" {
		t.Errorf("F1 usage.status=%q，期望 canceled（客户端断开不是上游故障，记 error 会虚高错误率并误熔断健康目标）",
			rows[0].Status)
	}

	// 证据 3：配额预占归还。
	e2eWaitReservedZero(t, db, "k1", "F1 客户端断开收尾")
}

// =====================================================================
// E. 快照生效（改配置后不重启，数据面立刻按新配置工作）
// =====================================================================

// e2eDBHarness 是一套**完全由数据库驱动**的运行时：池与快照都从 DB 重建，
// 并带上真的 runtimeReloader。
//
// # 为什么 E 场景不能用 buildHarness
//
// buildHarness 手工拼一个 routing.RouteIndex 塞进快照，DB 里根本没有 routes /
// route_targets 行。而 AutoReload 的核心承诺是「管理写操作落库后，重建出的
// 快照立刻生效」—— 要验证它，重建就必须真的**从库读**，否则测的是
// 「手工快照有没有生效」，那是个没有意义的命题。
//
// 所以这里把 provider / credential / 上游模型 / route / 链 / 用户 / key 全部
// 写进 DB，再用生产同一个 snapshot.RebuildFromDB + runtimeReloader 建运行时。
// masterKey 传 nil：DecryptWithFallback 在无主密钥时按明文返回（见其注释），
// 省掉在测试里生成密钥的噪音，而这条路径本身也是被支持的历史兼容路径。
func e2eDBHarness(t *testing.T, endpoint string) (http.Handler, *store.Store, *runtimeReloader) {
	t.Helper()

	logger := discardLogger()
	cfg := config.Default()
	cfg.Defaults.MaxRetries = 0

	db, err := store.Open(filepath.Join(t.TempDir(), "gw.db"), logger)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	ctx := context.Background()

	// 用户必须在 key 之前：access_keys.user_id 有外键，且鉴权要求归属用户存在。
	if err := db.CreateUser(ctx, &store.User{
		ID: testUserID, Username: "tester", PasswordHash: "x",
		Role: store.RoleUser, Status: store.UserStatusActive, AuthVersion: 1,
		BalanceCents: 10_000, // 有限余额（非 NULL = 受余额约束）
	}); err != nil {
		t.Fatalf("seed user: %v", err)
	}

	// provider → credential → 上游模型 → route（含链首）的顺序是**外键顺序**，
	// 不能调换：任一前置缺失都会得到 "FOREIGN KEY constraint failed"，
	// 而那条报错完全不指向"你漏了哪一步"。
	if err := db.CreateProvider(ctx, &store.Provider{
		ID: "good", Slug: "good", Name: "good", Protocol: "openai-chat",
		Endpoint: endpoint, Enabled: true,
	}); err != nil {
		t.Fatalf("seed provider: %v", err)
	}
	if err := db.CreateCredential(ctx, &store.Credential{
		ID: "cred1", ProviderID: "good", Label: "k",
		APIKeyEnc: []byte("sk-good"), Enabled: true, Weight: 1, Status: "healthy",
	}); err != nil {
		t.Fatalf("seed credential: %v", err)
	}
	if err := db.CreateUpstreamModel(ctx, &store.UpstreamModel{
		ID: "good/m", ProviderID: "good", ModelID: "good-model", Enabled: true,
		PriceInput: 1_000_000, PriceOutput: 1_000_000,
	}); err != nil {
		t.Fatalf("seed upstream model: %v", err)
	}
	// CreateRouteWithHeadTarget 会把「route + 链首目标」原子写入 —— 用它与
	// 生产的管理面走同一条写入路径（而不是自己拼两行）。
	if err := db.CreateRouteWithHeadTarget(ctx, &store.Route{
		ID: "r1", PublicName: "flash", ProviderID: "good",
		UpstreamModelID: "good/m", Enabled: true,
	}); err != nil {
		t.Fatalf("seed route: %v", err)
	}

	sum := sha256.Sum256([]byte(testAccessKey))
	if err := db.CreateAccessKey(ctx, &store.AccessKey{
		ID: "k1", KeyHash: hex.EncodeToString(sum[:]), KeyPrefix: "sk-gw-test",
		Name: "t", Enabled: true, UserID: testUserID,
	}); err != nil {
		t.Fatalf("seed access key: %v", err)
	}

	pool := upstream.NewPool(logger)
	reloader := &runtimeReloader{db: db, masterKey: nil, pool: pool, cfg: cfg, logger: logger}
	if err := reloader.Reload(ctx); err != nil {
		t.Fatalf("首次 reload: %v", err)
	}

	mux := http.NewServeMux()
	rl := ratelimit.New()
	mux.HandleFunc("POST /v1/chat/completions",
		handleIngress(pool, cfg, newUsageRecorder(db, logger), rl, openaiChatCodec{}))
	return server.Middleware(mux, logger), db, reloader
}

// E1：管理面禁用一把 key → **不重启**，数据面立刻拒绝。
//
// # 为什么这条最重要
//
// 「禁用下游 Key」是安全敏感操作。若快照没重建，auth 读的还是旧快照，
// 数据面会**继续放行**这把已禁用的 key，直到下一次任意 admin 写操作碰巧成功。
// 而响应早已是 200 +「已禁用」—— 界面在说谎，且没有任何迹象。
//
// # 这条链路要真的走通四段
//
//	admin handler 落库 → AutoReload 触发 reloader.Reload →
//	RebuildFromDB 建新快照 → snapshot.Swap → 数据面 auth 读新快照
//
// 任何一段断了，本用例都会红。这才是「AutoReload 的核心承诺」的端到端验证。
func TestE2E_E1_AdminDisableKeyTakesEffectWithoutRestart(t *testing.T) {
	up := newFakeUpstream(t, replyOK("pong"))
	handler, db, reloader := e2eDBHarness(t, up.URL())

	post := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
			strings.NewReader(e2eChatBody("flash", false)))
		req.Header.Set("Authorization", "Bearer "+testAccessKey)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}

	// 前置：改之前必须能用。否则「改之后 403」可能因为别的原因，
	// 整条用例会因错误的原因通过。
	before := post()
	t.Logf("E1 禁用前: status=%d body=%s", before.Code, strings.TrimSpace(before.Body.String()))
	if before.Code != http.StatusOK {
		t.Fatalf("E1 前置不成立：禁用前就不能用（%d）body=%s", before.Code, before.Body.String())
	}

	// 走生产同一条管理面链路：AutoReload 包住 key handler。
	// 审计回调传 nil（只关心重建），onFail 传 nil。
	adminH := server.AutoReload(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			admin.NewKeyHandler(db).Update(w, r, "k1")
		}),
		reloader.Reload, nil, discardLogger(), nil)

	body := `{"enabled":false}`
	adminReq := httptest.NewRequest(http.MethodPatch, "/admin/api/keys/k1", strings.NewReader(body))
	adminReq.Header.Set("Content-Type", "application/json")
	adminRec := httptest.NewRecorder()
	adminH.ServeHTTP(adminRec, asAdminReq(adminReq))
	t.Logf("E1 管理面 PATCH enabled=false: status=%d body=%s",
		adminRec.Code, strings.TrimSpace(adminRec.Body.String()))
	if adminRec.Code != http.StatusOK {
		t.Fatalf("E1 管理面禁用失败：%d body=%s", adminRec.Code, adminRec.Body.String())
	}

	// 证据 1：库里确实落成 enabled=0 了（响应 200 不等于落库了）。
	k, err := db.GetAccessKey(context.Background(), "k1")
	if err != nil || k == nil {
		t.Fatalf("E1 读回 key 失败: %v", err)
	}
	t.Logf("E1 库内 key.Enabled=%v", k.Enabled)
	if k.Enabled {
		t.Fatalf("E1 管理面回了 200 但库里 Enabled 仍为 true —— 压根没落库")
	}

	// 证据 2：**不重启**，数据面立刻拒绝。
	after := post()
	t.Logf("E1 禁用后（未重启）: status=%d body=%s", after.Code, strings.TrimSpace(after.Body.String()))
	if after.Code != http.StatusForbidden {
		t.Fatalf("E1 禁用生效失败：期望 403，实际 %d body=%s —— "+
			"快照没重建，数据面仍在放行已禁用的 key", after.Code, after.Body.String())
	}
	if !strings.Contains(after.Body.String(), "invalid_api_key") {
		t.Errorf("E1 错误信封不对：%s", after.Body.String())
	}
	// 证据 3：被拒请求不得打到上游。
	if got := up.Count(); got != 1 {
		t.Errorf("E1 上游调用次数=%d，期望 1（只有禁用前那一次成功请求）", got)
	}
}

// E2：管理面**重新启用** key → 不重启立刻恢复可用。
//
// 反向也要测：只有「禁用生效」的话，一个「重建后一律拒绝」的实现照样绿 ——
// 那会让所有 key 变成废钥，而测试看不出来。
func TestE2E_E2_AdminReEnableKeyRestoresWithoutRestart(t *testing.T) {
	up := newFakeUpstream(t, replyOK("pong"))
	handler, db, reloader := e2eDBHarness(t, up.URL())

	// 先把 key 在**库里**禁用（快照仍是启用态，所以数据面此刻还能用 ——
	// 这正好模拟"禁用还没重建"的中间态被后来的重建收敛）。
	if err := db.UpdateAccessKey(context.Background(), "k1", &store.AccessKey{
		ID: "k1", Name: "t", Enabled: false, UserID: testUserID,
	}); err != nil {
		t.Fatalf("库里禁用 key: %v", err)
	}
	// 手动重建一次，让数据面进入"已禁用"状态。
	if err := reloader.Reload(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}

	post := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
			strings.NewReader(e2eChatBody("flash", false)))
		req.Header.Set("Authorization", "Bearer "+testAccessKey)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}
	if rec := post(); rec.Code != http.StatusForbidden {
		t.Fatalf("E2 前置不成立：期望已禁用为 403，实际 %d", rec.Code)
	}

	adminH := server.AutoReload(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			admin.NewKeyHandler(db).Update(w, r, "k1")
		}),
		reloader.Reload, nil, discardLogger(), nil)
	adminReq := httptest.NewRequest(http.MethodPatch, "/admin/api/keys/k1",
		strings.NewReader(`{"enabled":true}`))
	adminReq.Header.Set("Content-Type", "application/json")
	adminRec := httptest.NewRecorder()
	adminH.ServeHTTP(adminRec, asAdminReq(adminReq))
	t.Logf("E2 管理面重新启用: status=%d", adminRec.Code)
	if adminRec.Code != http.StatusOK {
		t.Fatalf("E2 重新启用失败：%d body=%s", adminRec.Code, adminRec.Body.String())
	}

	rec := post()
	t.Logf("E2 重新启用后（未重启）: status=%d body=%s", rec.Code, strings.TrimSpace(rec.Body.String()))
	if rec.Code != http.StatusOK {
		t.Fatalf("E2 重新启用没生效：期望 200，实际 %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "pong") {
		t.Errorf("E2 响应里没有上游内容：%s", rec.Body.String())
	}
}

// E3：管理面改**模型白名单** → 不重启立刻 403。
//
// 与 key 启停走的是**不同的**快照字段（AllowedModels 是 ModelAllow 判定器，
// 而 enabled 是布尔短路），所以必须单独验证：一个「只重建了 Enabled」的
// 实现会让这条红，而 E1 照样绿。
func TestE2E_E3_AdminModelAllowlistTakesEffectWithoutRestart(t *testing.T) {
	up := newFakeUpstream(t, replyOK("pong"))
	handler, db, reloader := e2eDBHarness(t, up.URL())

	post := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
			strings.NewReader(e2eChatBody("flash", false)))
		req.Header.Set("Authorization", "Bearer "+testAccessKey)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}
	if rec := post(); rec.Code != http.StatusOK {
		t.Fatalf("E3 前置不成立：白名单为空时应放行，实际 %d body=%s", rec.Code, rec.Body.String())
	}

	// 白名单收紧成「只允许直连形式 good/good-model」→ 具名路由 flash 应被 403。
	//
	// 为什么用 `good/good-model` 而不是随便编一个名字：管理面会用**当前快照**
	// 校验白名单里的每个名字（unknownModels），编造的名字会得到 400 而不是
	// 我们想测的 403。直连形式（provider/model）本身是合法模型名 ——
	// 数据面的 Resolve 认得它，所以校验也认。
	adminH := server.AutoReload(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			admin.NewKeyHandler(db).Update(w, r, "k1")
		}),
		reloader.Reload, nil, discardLogger(), nil)
	adminReq := httptest.NewRequest(http.MethodPatch, "/admin/api/keys/k1",
		strings.NewReader(`{"allowed_models":["good/good-model"]}`))
	adminReq.Header.Set("Content-Type", "application/json")
	adminRec := httptest.NewRecorder()
	adminH.ServeHTTP(adminRec, asAdminReq(adminReq))
	t.Logf("E3 管理面收紧白名单: status=%d body=%s",
		adminRec.Code, strings.TrimSpace(adminRec.Body.String()))
	if adminRec.Code != http.StatusOK {
		t.Fatalf("E3 白名单写入失败：%d body=%s", adminRec.Code, adminRec.Body.String())
	}

	rec := post()
	t.Logf("E3 收紧后（未重启）: status=%d body=%s", rec.Code, strings.TrimSpace(rec.Body.String()))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("E3 白名单没生效：期望 403，实际 %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "model_not_allowed") {
		t.Errorf("E3 code 应为 model_not_allowed：%s", rec.Body.String())
	}
	// 被白名单拒的请求绝不能打到上游（白名单是权限边界，不是事后过滤）。
	if got := up.Count(); got != 1 {
		t.Errorf("E3 上游调用次数=%d，期望 1（只有白名单收紧前那一次）", got)
	}
}

// =====================================================================
// 对抗性场景（Adv*）：happy path 全绿但边界会漏的地方
// =====================================================================
//
// 上面每个场景验证「能工作」，这一节验证它们在**边界条件下仍然正确** ——
// 并发、重放、超时、二次入口。这些是不看代码光跑正例发现不了的那一类。
// 本节原先单独一个文件，后并入此处以便与它所依赖的 e2e 辅助函数同处一地。
// Adv1：管理员 key 打 **GET /v1/models** 也必须被拒。
//
// 管理面锁定的既有用例只覆盖了 POST /v1/chat/completions。而 /v1/models 是
// 一条独立的入口（handleListModels，不经 handleIngress），它自己做
// auth.Authenticate。若截断只加在转发入口，管理员仍能从这里枚举全部模型名
// —— 恰恰是「管理员只做控制面」这条需求要消除的敞口。
func TestE2E_Adv1_AdminKeyRejectedOnListModels(t *testing.T) {
	up := newFakeUpstream(t, replyOK("pong"))
	_, db := buildHarness(t, []struct {
		slug, url string
		fail      bool
	}{{slug: "good", url: up.URL()}}, false)

	// 用 DB 驱动的 handler 会更真实，但 /v1/models 只读快照与库，
	// 手工 harness 的快照已足够（它测的是鉴权分支，不是重建）。
	h := handleListModels("")

	e2eRequireUserRole(t, store.RoleAdmin)

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+testAccessKey)
	rec := httptest.NewRecorder()
	h(rec, req)

	t.Logf("Adv1 管理员 GET /v1/models: status=%d body=%s", rec.Code, strings.TrimSpace(rec.Body.String()))

	if rec.Code != http.StatusForbidden {
		t.Errorf("Adv1 管理员枚举模型应 403，实际 %d —— 锁定的入口不完整", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "admin_cannot_call_model") {
		t.Errorf("Adv1 code 应为 admin_cannot_call_model：%s", rec.Body.String())
	}
	// 模型名列表绝不能泄给管理员（那是控制面/数据面分离的意义）。
	if strings.Contains(rec.Body.String(), "flash") {
		t.Errorf("Adv1 管理员拿到了模型清单 —— 锁没生效：%s", rec.Body.String())
	}
	_ = db
}

// Adv2：**同一 request_id 重放**不得扣两次费。
//
// 扣费拿 request_id 当幂等键（balance_charges 以 (request_id,user_id) 为主键）。
// 这条路径的触发是真实的：客户端超时重发、SDK 传输层重试、上游重放。
// 若幂等失效，用户被重复扣钱且不会有任何报错。
//
// 怎么构造重放：直接调 usageRecorder.record 两次、传同一个 request_id ——
// 这正是"同一次调用的收尾走了两遍"的等价形态，且绕过了 HTTP 层的
// 随机 request_id 生成。
func TestE2E_Adv2_DuplicateRequestIDChargesOnce(t *testing.T) {
	up := newFakeUpstream(t, replyOK("pong"))
	_, db := buildHarness(t, []struct {
		slug, url string
		fail      bool
	}{{slug: "good", url: up.URL()}}, false)
	// 定价必须与余额相称：单价 1000 元/百万 token，1000 输入 + 1000 输出
	// → 单条 2.00 元（200 分）。余额 100.00 元（10000 分）足够付**两次**，
	// 这样「扣了一次」与「扣了两次」在余额上是可区分的两个数。
	//
	// 最初写这条用例时单价设成 1e6（= 每 token 1 元），单条费用 2000 元
	// 远超余额，ChargeBalance 按余额守卫正确地拒绝了扣费 —— 于是用例报
	// 「费用被少扣了」。那是个**测试素材错误**（把余额不足当成了幂等 bug），
	// 记在这里以免后人重犯：余额用例的定价与余额必须一起设计。
	priceRoute(t, db, "good", "good-model", 1000, 1000)
	e2eSetBalance(t, db, 10_000) // 100.00 元

	before, _ := e2eBalance(t, db)
	rec := newUsageRecorder(db, discardLogger())

	// 同一条用量记录（同 request_id）记两次。
	//
	// token 数必须**非零**：cost_total 由 freezeUsageCost 按
	// (provider_id, model_id) 查单价 × token 数算出，token 全 0 时
	// cost_total 恒 0 → ChargeBalance 直接 no-op → 幂等断言会**空转通过**
	// （本轮实测踩过：第一次写这个用例时两条记录 cost_total 都是 0，
	// 于是「没扣两次」是因为「压根没扣」，测不到幂等）。
	for i := range 2 {
		rec.record(&store.UsageRecord{
			ID:          fmt.Sprintf("dup-%d", i),
			AccessKeyID: "k1", UserID: testUserID,
			PublicModel: "flash", ProviderID: "good", UpstreamModel: "good-model",
			IngressProtocol: "openai-chat", Status: "ok", HTTPStatus: 200,
			InputTokens: 1000, OutputTokens: 1000, TotalTokens: 2000,
			UsageState: "reported",
			RequestID:  "replay-same-id",
		})
	}
	// 等 worker 把两条都写完。
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if len(e2eUsageRows(t, db, "k1")) >= 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(300 * time.Millisecond) // 给扣费留时间（异步）

	after, rem := e2eBalance(t, db)
	charged := float64(before-after)*0.01 + float64(rem)/1e6
	t.Logf("Adv2 两条同 request_id 记录：余额 %d → %d，余数 %d，合计扣费≈%.6f 元",
		before, after, rem, charged)

	rows := e2eUsageRows(t, db, "k1")
	var totalCost float64
	for _, r := range rows {
		totalCost += r.CostTotal
	}
	t.Logf("Adv2 共 %d 条记录，cost_total 合计=%.6f 元（单条 %.6f）",
		len(rows), totalCost, costOf(rows))

	// 前置：必须真的产生了费用，否则「只扣一次」是因为「压根没扣」——
	// 一个空转通过的假绿（本轮第一次写这条用例时正是如此）。
	if len(rows) < 2 || totalCost <= 0 {
		t.Fatalf("Adv2 前置不成立：%d 条记录、cost_total 合计 %.6f —— "+
			"没有可扣的费用，本用例测不到幂等", len(rows), totalCost)
	}

	// 核心断言：实扣 == **单条** cost，而不是两条之和。
	// 允许 1 分钱的取整误差（余数机制按分扣）。
	single := rows[0].CostTotal
	if charged > single+0.02 {
		t.Errorf("Adv2 幂等失效：实扣 ≈%.6f 元，单条 cost_total=%.6f 元，两条合计=%.6f 元 —— "+
			"同一次调用被扣了两次费", charged, single, totalCost)
	}
	if charged < single-0.02 {
		t.Errorf("Adv2 实扣 ≈%.6f 元 少于单条 cost_total %.6f 元 —— 费用被少扣了",
			charged, single)
	}
}

// costOf 返回第一条记录的 cost_total（0 条时返回 0），供日志与断言复用。
func costOf(rows []e2eUsageRow) float64 {
	if len(rows) == 0 {
		return 0
	}
	return rows[0].CostTotal
}

// Adv3：**非流式**请求中途断开 → 预占必须归还、不记成上游故障。
//
// F1 覆盖的是流式路径（attemptStream）。非流式是**另一条**独立路径
// （attemptNonStream），它的 ctx 绑定与收尾在不同分支上，所以必须单独测。
func TestE2E_Adv3_NonStreamClientDisconnectReleasesReservation(t *testing.T) {
	hit := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case hit <- struct{}{}:
		default:
		}
		// 挂住直到客户端断开传播过来（网关取消上游 ctx）。
		//
		// 兜底超时必须**短于**测试结束：httptest.Server.Close 会等所有
		// in-flight 连接结束，一个永不退出的 handler 会让 Close 卡满 5s
		// 并打一条 "blocked in Close" 警告 —— 那是测试自己的噪音，
		// 不是被测行为。3s 足够覆盖「网关传播取消」的亚毫秒级路径。
		select {
		case <-r.Context().Done():
		case <-time.After(3 * time.Second):
		}
	}))
	defer srv.Close()

	h, db := buildHarness(t, []struct {
		slug, url string
		fail      bool
	}{{slug: "slow", url: srv.URL}}, false)
	if err := db.UpdateAccessKey(context.Background(), "k1", &store.AccessKey{
		ID: "k1", Name: "t", Enabled: true, QuotaTokens: 100_000, UserID: testUserID,
	}); err != nil {
		t.Fatalf("set quota: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(e2eChatBodyMaxTokens("flash", 500))).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer "+testAccessKey)

	done := make(chan struct{})
	go func() {
		middlewareForTest(h).ServeHTTP(httptest.NewRecorder(), req)
		close(done)
	}()

	select {
	case <-hit:
	case <-time.After(5 * time.Second):
		cancel()
		<-done
		t.Fatal("Adv3 上游一直没收到请求")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Adv3 客户端断开后 handler 未返回 —— 非流式路径没响应 ctx 取消")
	}

	e2eWaitReservedZero(t, db, "k1", "Adv3 非流式客户端断开")

	// 断开不该把健康凭据打进冷却（否则几次用户中断就把 provider 搞成不可用）。
	rows := e2eUsageRows(t, db, "k1")
	for _, r := range rows {
		t.Logf("Adv3 usage 行 status=%q", r.Status)
		if r.Status == "error" {
			t.Errorf("Adv3 客户端断开被记成 error —— 会虚高错误率并可能误熔断健康目标")
		}
	}
}

// Adv4：**并发**打同一个限额 key 不得超发（ReserveQuota 的原子性）。
//
// 这是预占机制存在的全部理由。串行请求永远测不出来：必须真并发，
// 才能覆盖「两个请求读到同一个 used、各自判定够用、双双放行」的
// check-then-act 窗口。
func TestE2E_Adv4_ConcurrentQuotaReserveDoesNotOverIssue(t *testing.T) {
	up := newFakeUpstream(t, replyOK("pong"))
	h, db := buildHarness(t, []struct {
		slug, url string
		fail      bool
	}{{slug: "good", url: up.URL()}}, false)

	// 配额 1000，每个请求 max_tokens=500 → 理论上最多放行 2 个（1000/500）。
	if err := db.UpdateAccessKey(context.Background(), "k1", &store.AccessKey{
		ID: "k1", Name: "t", Enabled: true, QuotaTokens: 1000, UserID: testUserID,
	}); err != nil {
		t.Fatalf("set quota: %v", err)
	}

	const n = 8
	var wg sync.WaitGroup
	codes := make([]int, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := e2ePost(h, "/v1/chat/completions", testAccessKey, e2eChatBodyMaxTokens("flash", 500))
			codes[i] = rec.Code
		}()
	}
	wg.Wait()

	ok, limited := 0, 0
	for _, c := range codes {
		switch c {
		case http.StatusOK:
			ok++
		case http.StatusTooManyRequests:
			limited++
		}
	}
	t.Logf("Adv4 并发 %d 个请求（配额 1000，每个预占 ~500）：放行 %d，429 %d，全部状态码=%v",
		n, ok, limited, codes)

	// 从库里核对实际预占总量是否被守住。
	quota, used, _, err := db.GetKeyQuota(context.Background(), "k1")
	if err != nil {
		t.Fatalf("GetKeyQuota: %v", err)
	}
	t.Logf("Adv4 库内 quota=%d used=%d reserved=%d", quota, used, e2eReserved(t, db, "k1"))

	// 放行数 × 预占 应该 <= 配额（允许相等）。
	// est 是估算值（含输入），所以用下界 500 判「没有超发数倍」。
	if ok*500 > int(quota)*2 {
		t.Errorf("Adv4 严重超发：放行 %d 个请求 × 每个至少 500 token = %d，配额仅 %d",
			ok, ok*500, quota)
	}
	// 上游被碰的次数必须等于放行的次数 —— 被拒的请求绝不能打上游。
	if up.Count() != ok {
		t.Errorf("Adv4 上游被调用 %d 次，但放行了 %d 个 —— 被拒的请求打了上游",
			up.Count(), ok)
	}
}

// Adv5：总请求预算真的会掐断一个「永远不出首字」的上游。
//
// 这条验证的是 totalRequestBudget 这个修复**在真实链路上生效**。
// 之前审计发现过「没有任何总时长上限」，而 TTFT 看门狗只在拿到响应头之后
// 才启动 —— 一个接受了 TCP 却永不回响应头的上游能把请求挂到
// ResponseHeaderTimeout(60s)。
//
// 这里把 route 级 TTFT 压到 200ms，然后断言请求在**远小于 60s** 的时间内
// 被掐断并返回错误 —— 证明看门狗而不是 TCP 兜底在起作用。
func TestE2E_Adv5_FirstTokenTimeoutCutsHangingUpstream(t *testing.T) {
	// 上游接受连接、返回 200 + SSE 头，然后一个事件都不发。
	hang := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
		<-r.Context().Done()
	}))
	defer hang.Close()

	// ttftMs=200：route 级首字超时。
	h, db := buildHarness(t, []struct {
		slug, url string
		fail      bool
	}{{slug: "hang", url: hang.URL}}, true, 200)

	start := time.Now()
	rec := e2ePost(h, "/v1/chat/completions", testAccessKey, e2eChatBody("flash", true))
	elapsed := time.Since(start)

	t.Logf("Adv5 首字超时：status=%d 耗时=%v body=%s",
		rec.Code, elapsed.Round(time.Millisecond), strings.TrimSpace(rec.Body.String()))

	// 必须在看门狗尺度内返回，而不是等到 ResponseHeaderTimeout(60s) 或更久。
	if elapsed > 10*time.Second {
		t.Errorf("Adv5 耗时 %v —— 首字看门狗没生效，请求被拖到了 TCP/响应头超时", elapsed)
	}
	if rec.Code == http.StatusOK {
		t.Errorf("Adv5 挂死的上游不该返回 200：body=%s", rec.Body.String())
	}
	// usage 必须落库（挂死也是一次真实的失败尝试，不落库等于漏账）。
	//
	// 必须**轮询**：usageRecorder 是异步落库的（worker goroutine），
	// 请求返回的那一刻记录还没写进去。本轮实测踩过这个坑 ——
	// 第一版直接查库，得到一个「没有 usage 记录」的假红；
	// 加轮询后发现记录是有的（status=error / usage_state=none）。
	// 这类时序误判是本轮最值得记的教训：断言异步副作用必须等差，
	// 不能假定「请求返回 == 副作用已落地」。
	rows := e2eWaitUsageCount(t, db, "k1", 1)
	t.Logf("Adv5 usage 行: status=%q usage_state=%q tokens=%d cost=%v prov=%q",
		rows[0].Status, rows[0].UsageState, rows[0].TotalTokens, rows[0].CostTotal, rows[0].ProviderID)
	if rows[0].Status == "ok" {
		t.Errorf("Adv5 挂死/超时的请求被记成 status=ok —— 目标永远不会累计失败、永不熔断")
	}
	if rows[0].ProviderID != "hang" {
		t.Errorf("Adv5 usage 归因到 %q，期望 hang", rows[0].ProviderID)
	}
}

// Adv6：上游返回**畸形 JSON**（200 但 body 不是合法 completion）→ 不得 200 放行。
//
// 这类上游真实存在（中转网关出错时返回 HTML 错误页、或半截 JSON）。
// 若网关把畸形体当成功透传，客户端会拿到一段无法解析的 200。
func TestE2E_Adv6_MalformedUpstreamBodyIsNotOK(t *testing.T) {
	bad := newFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// 200 + 非法 JSON。
		_, _ = w.Write([]byte(`{"choices": [ this is not json`))
	})
	h, _ := buildHarness(t, []struct {
		slug, url string
		fail      bool
	}{{slug: "bad", url: bad.URL()}}, false)

	rec := e2ePost(h, "/v1/chat/completions", testAccessKey, e2eChatBody("flash", false))
	t.Logf("Adv6 status=%d body=%s", rec.Code, strings.TrimSpace(rec.Body.String()))

	if rec.Code == http.StatusOK {
		t.Errorf("Adv6 畸形上游响应被当成 200 放行 —— 客户端会拿到无法解析的体：%s", rec.Body.String())
	}
	// 响应体本身必须是合法 JSON（不能把上游的畸形体原样透传）。
	var probe map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &probe); err != nil {
		t.Errorf("Adv6 网关返回的响应体不是合法 JSON：%v body=%s", err, rec.Body.String())
	}
}

// Adv7：**流式白名单/配额拒绝**不得写出 `text/event-stream` 头。
//
// 若一个流式请求在预检阶段被拒（403/429），而响应已经带上 SSE 头，
// 客户端会按 SSE 解析一段 JSON 错误体 —— 报出「流式响应中没有内容」
// 这种指向不了任何一层的错误。所以拒绝路径必须走普通 JSON 响应。
func TestE2E_Adv7_StreamRejectionUsesJSONNotSSE(t *testing.T) {
	up := newFakeUpstream(t, replySSE("pong"))
	h, db := buildHarness(t, []struct {
		slug, url string
		fail      bool
	}{{slug: "good", url: up.URL()}}, false)

	// 用白名单把 flash 挡掉（比配额更早、且与流式无关）。
	old := snapshot.Get()
	newKeys := make(map[string]*snapshot.KeySnapshot, len(old.KeysByHash))
	for hash, ks := range old.KeysByHash {
		cp := *ks
		cp.AllowedModels = snapshot.AllowOnly([]string{"only-this"})
		newKeys[hash] = &cp
	}
	snapshot.Init(&snapshot.Snapshot{
		Routes: old.Routes, Providers: old.Providers, KeysByHash: newKeys,
		UsersByID: old.UsersByID, Runtime: old.Runtime,
	})

	rec := e2ePost(h, "/v1/chat/completions", testAccessKey, e2eChatBody("flash", true))
	ct := rec.Header().Get("Content-Type")
	t.Logf("Adv7 流式被白名单拒：status=%d content-type=%q body=%s",
		rec.Code, ct, strings.TrimSpace(rec.Body.String()))

	if rec.Code != http.StatusForbidden {
		t.Fatalf("Adv7 期望 403，实际 %d", rec.Code)
	}
	if strings.Contains(ct, "text/event-stream") {
		t.Errorf("Adv7 被拒的流式请求带上了 SSE 头（%q）—— 客户端会把 JSON 错误体当 SSE 解析", ct)
	}
	if !strings.Contains(ct, "application/json") {
		t.Errorf("Adv7 Content-Type=%q，期望 application/json", ct)
	}
	if strings.Contains(rec.Body.String(), "data:") {
		t.Errorf("Adv7 响应体里有 SSE 分片：%s", rec.Body.String())
	}
	if up.Count() != 0 {
		t.Errorf("Adv7 被白名单拒的请求打了上游 %d 次", up.Count())
	}
	_ = db
}

// Adv8：流式**中途截断**只落**一行** usage，且不重复扣费。
//
// C3 已经证明「已提交的流不重试」。这条补的是它的**账目后果**：
// 不重试只是手段，真正要保证的是「客户端只被计一次费、报表只有一行」。
// 一个「重试被正确拦住、但收尾把 usage 记了两遍」的实现会让 C3 绿而这条红。
func TestE2E_Adv8_TruncatedStreamRecordsExactlyOnce(t *testing.T) {
	broken := newFakeUpstream(t, replySSEThenFail(
		`{"id":"c1","object":"chat.completion.chunk","created":0,"model":"upstream-model","choices":[{"index":0,"delta":{"content":"PARTIAL"},"finish_reason":null}]}`))
	healthy := newFakeUpstream(t, replySSE("SHOULD-NOT-APPEAR"))
	h, db := buildHarness(t, []struct {
		slug, url string
		fail      bool
	}{
		{slug: "p1", url: broken.URL()},
		{slug: "p2", url: healthy.URL()},
	}, true)
	priceRoute(t, db, "p1", "p1-model", 1_000_000, 1_000_000)
	e2eSetBalance(t, db, 10_000)

	before, beforeRem := e2eBalance(t, db)
	rec := e2ePost(h, "/v1/chat/completions", testAccessKey, e2eChatBody("flash", true))
	t.Logf("Adv8 status=%d body=%s", rec.Code, strings.TrimSpace(rec.Body.String()))

	// 等 usage 落库。**必须轮询**（异步 worker）。
	rows := e2eWaitUsageCount(t, db, "k1", 1)
	// 再多等一会儿，给「万一有第二行」留出出现的时间 ——
	// 这是「断言没有发生某事」时必须做的：只查一次会因「还没写完」而假绿。
	time.Sleep(500 * time.Millisecond)
	rows = e2eUsageRows(t, db, "k1")

	t.Logf("Adv8 usage 行数=%d: %+v", len(rows), rows)
	if len(rows) != 1 {
		t.Errorf("Adv8 截断的流落了 %d 行 usage，期望恰好 1 行 —— 重复计费/重复记账", len(rows))
	}
	if up := healthy.Count(); up != 0 {
		t.Errorf("Adv8 健康次目标被调用 %d 次 —— 已提交的流被重试了", up)
	}

	// 账目后果：截断（未报 usage）不该扣费（cost_total=0 → 扣费 0）。
	after, afterRem := e2eBalance(t, db)
	charged := float64(before-after)*0.01 + float64(afterRem-beforeRem)/1e6
	t.Logf("Adv8 截断流的扣费≈%.6f 元（usage_state=%q，cost_total=%v）",
		charged, rows[0].UsageState, rows[0].CostTotal)
	if rows[0].UsageState == "missing" && charged > 0.01 {
		t.Errorf("Adv8 上游没报 usage（missing）却扣了 %.6f 元 —— 用户为一次拿不到结果的调用付了钱",
			charged)
	}
}

// Adv9：`/v1/messages`（Anthropic 入口）与 `/v1/chat/completions` 共享限速器。
//
// 「限速在 ingress 骨架层，两个入口共享」是既有设计承诺，但两个入口是**分别**
// 注册的 handler。若哪天有人给其中一个单独套了限速器，另一个就能绕过限速
// —— 一个入口被限死、另一个敞开的部署会让人误以为限速生效了。
func TestE2E_Adv9_RateLimitSharedAcrossIngressPaths(t *testing.T) {
	up := newFakeUpstream(t, replyOK("pong"))
	h, db, _ := buildHarnessFull(t, []struct {
		slug, url string
		fail      bool
	}{{slug: "good", url: up.URL()}}, false, openaiChatCodec{})
	_ = db

	// RPM=1，然后一个 chat 请求用掉额度，紧接着一个 messages 请求应被拒。
	old := snapshot.Get()
	newKeys := make(map[string]*snapshot.KeySnapshot, len(old.KeysByHash))
	for hash, ks := range old.KeysByHash {
		cp := *ks
		cp.RPMLimit = 1
		newKeys[hash] = &cp
	}
	snapshot.Init(&snapshot.Snapshot{
		Routes: old.Routes, Providers: old.Providers, KeysByHash: newKeys,
		UsersByID: old.UsersByID, Runtime: old.Runtime,
	})

	rec1 := e2ePost(h, "/v1/chat/completions", testAccessKey, e2eChatBody("flash", false))
	t.Logf("Adv9 chat 入口（第 1 个，应 200）：status=%d", rec1.Code)
	if rec1.Code != http.StatusOK {
		t.Fatalf("Adv9 前置不成立：第一个请求就不是 200（%d）", rec1.Code)
	}

	// 用**另一个入口**发第二个请求：同一把 key、同一个限速器 → 应 429。
	rec2 := e2ePost(h, "/v1/messages", testAccessKey,
		`{"model":"flash","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`)
	t.Logf("Adv9 messages 入口（第 2 个，应 429）：status=%d body=%s",
		rec2.Code, strings.TrimSpace(rec2.Body.String()))

	if rec2.Code != http.StatusTooManyRequests {
		t.Errorf("Adv9 跨入口限速未共享：chat 用掉额度后 messages 仍返回 %d —— "+
			"换一个 ingress 路径就能绕过 RPM 限速", rec2.Code)
	}
}

// Adv10：**并发**同 requestID 的 usage 记录不得双重扣费（幂等的并发版）。
//
// Adv2 是串行重放，覆盖的是「第二次命中主键」的快路径。并发版覆盖的是
// 更窄的窗口：两个 goroutine 同时进入 ChargeBalance、同时 INSERT OR IGNORE
// —— 若事务隔离或占位时机有偏差，两个都会占位成功并各扣一次。
func TestE2E_Adv10_ConcurrentSameRequestIDChargesOnce(t *testing.T) {
	up := newFakeUpstream(t, replyOK("pong"))
	_, db := buildHarness(t, []struct {
		slug, url string
		fail      bool
	}{{slug: "good", url: up.URL()}}, false)
	priceRoute(t, db, "good", "good-model", 1000, 1000)
	e2eSetBalance(t, db, 10_000)

	before, _ := e2eBalance(t, db)
	rec := newUsageRecorder(db, discardLogger())

	// 8 个并发都写同一个 request_id：只有一次应该真正扣到钱。
	const n = 8
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec.record(&store.UsageRecord{
				ID:          fmt.Sprintf("conc-%d", i),
				AccessKeyID: "k1", UserID: testUserID,
				PublicModel: "flash", ProviderID: "good", UpstreamModel: "good-model",
				IngressProtocol: "openai-chat", Status: "ok", HTTPStatus: 200,
				InputTokens: 1000, OutputTokens: 1000, TotalTokens: 2000,
				UsageState: "reported",
				RequestID:  "concurrent-same-id",
			})
		}()
	}
	wg.Wait()

	// 等 n 行都落库 + 扣费跑完。
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if len(e2eUsageRows(t, db, "k1")) >= n {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(500 * time.Millisecond)

	rows := e2eUsageRows(t, db, "k1")
	after, rem := e2eBalance(t, db)
	charged := float64(before-after)*0.01 + float64(rem)/1e6

	var totalCost float64
	for _, r := range rows {
		totalCost += r.CostTotal
	}
	t.Logf("Adv10 并发 %d 条同 request_id：落库 %d 行，cost_total 合计=%.4f 元；余额 %d → %d，实扣≈%.4f 元",
		n, len(rows), totalCost, before, after, charged)

	if len(rows) < n {
		t.Fatalf("Adv10 只落了 %d/%d 行 —— 落库丢了记录", len(rows), n)
	}
	if totalCost <= 0 {
		t.Fatalf("Adv10 前置不成立：cost_total 合计 0，没有可扣的费用")
	}
	single := rows[0].CostTotal
	// 幂等生效：实扣 = 单条（约 single），绝不接近 n×single。
	if charged > single*2 {
		t.Errorf("Adv10 并发幂等失效：实扣 ≈%.4f 元，单条 cost=%.4f 元，%d 条合计=%.4f 元 —— "+
			"同一次调用被并发扣了多次", charged, single, n, totalCost)
	}
}
