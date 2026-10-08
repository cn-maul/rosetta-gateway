package admin

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/cn-maul/rosetta-gateway/internal/store"
)

// 用户侧「调用历史」的跨用户隔离（2026-10-10）。
//
// # 为什么需要这个测试
//
// 2026-10-10 把 /history 从 admin-only 放开给普通用户。放开本身**不改后端**：
// usage/history 与 usage/history.csv 早已在 userAccessibleRoutes 白名单里
// （internal/server/user_auth.go），且 usageFilter / usageHistoryFilters 都按
// callerScope 强制收窄。也就是说，本测试断言的**不是新代码**，而是「已有代码
// 在新的暴露面上仍然成立」。
//
// 这恰恰是它值得存在的原因：这两条端点此前只在「管理员看全站」这一种身份下被
// 实际使用过（authz_matrix 只验到网关层放行，不验数据是否按身份收窄）。
// 一旦有人日后动 usageFilter 的 WHERE 构造、或给 callerScope 加分支，
// 泄露的是**别的用户的调用明细**（含模型名、时间、token 数、花费）——
// 这是本系统里最敏感的一类数据，且泄露后界面完全无异常。
//
// # 断言的是「行为」不是「实现」
//
// 刻意同时验三条：只见到自己的、**显式传别人的 key_id 仍然是空集**、
// CSV 导出同样收窄。第一条防的是「忘了加过滤」，第二条防的是「加了过滤但可被
// 查询参数绕过」，第三条防的是「列表收窄了、导出没收窄」—— 导出是最容易漏的
// 那一处，因为它的 handler 与列表**不是同一个函数**。

// seedUsageForScope 造两个用户各一把 key、各一条用量，供隔离测试比对。
func seedUsageForScope(t *testing.T, st *store.Store) {
	t.Helper()
	ctx := context.Background()
	// key.user_id 上有指向 users 的外键，所以必须先把两个用户建出来。
	// 顺序不能反：CreateAccessKey 失败会报 "FOREIGN KEY constraint failed"，
	// 而它与「隔离逻辑写错了」是完全不同的两类失败，不要混在一起排查。
	for _, u := range []*store.User{
		{ID: "u1", Username: "alice"},
		{ID: "u2", Username: "bob"},
	} {
		if err := st.CreateUser(ctx, u); err != nil {
			t.Fatalf("create user %s: %v", u.ID, err)
		}
	}
	for _, k := range []*store.AccessKey{
		{ID: "keyU1", KeyHash: "hU1", KeyPrefix: "sk-gw-u1...", Name: "u1-key", Enabled: true, UserID: "u1"},
		{ID: "keyU2", KeyHash: "hU2", KeyPrefix: "sk-gw-u2...", Name: "u2-key", Enabled: true, UserID: "u2"},
	} {
		if err := st.CreateAccessKey(ctx, k); err != nil {
			t.Fatalf("create key %s: %v", k.ID, err)
		}
	}
	now := time.Now().UnixMilli()
	for _, r := range []*store.UsageRecord{
		{ID: "useU1", Ts: now - 1000, AccessKeyID: "keyU1", UserID: "u1", PublicModel: "m-a", ProviderID: "p1", UpstreamModel: "x", TotalTokens: 111, OutputTokens: 11, Status: "ok", HTTPStatus: 200},
		{ID: "useU2", Ts: now - 1000, AccessKeyID: "keyU2", UserID: "u2", PublicModel: "m-b", ProviderID: "p1", UpstreamModel: "x", TotalTokens: 222, OutputTokens: 22, Status: "ok", HTTPStatus: 200},
	} {
		if err := st.CreateUsageRecord(ctx, r); err != nil {
			t.Fatalf("create usage %s: %v", r.ID, err)
		}
	}
}

type historyPage struct {
	Total   int `json:"total"`
	Records []struct {
		PublicModel string `json:"public_model"`
		TotalTokens int64  `json:"total_tokens"`
	} `json:"records"`
}

// TestUsageHistory_ScopedToCaller 钉住列表端点的跨用户隔离。
func TestUsageHistory_ScopedToCaller(t *testing.T) {
	st := newTestStore(t)
	seedUsageForScope(t, st)
	h := NewUsageHandler(st)

	hit := func(as func(*http.Request) *http.Request, url string) historyPage {
		t.Helper()
		rec := httptest.NewRecorder()
		h.History(rec, as(httptest.NewRequest(http.MethodGet, url, nil)))
		if rec.Code != http.StatusOK {
			t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
		}
		var p historyPage
		if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
			t.Fatalf("unmarshal: %v body=%s", err, rec.Body.String())
		}
		return p
	}

	t.Run("管理员看全站", func(t *testing.T) {
		p := hit(asAdmin, "/admin/api/usage/history?from=0")
		if p.Total != 2 {
			t.Fatalf("管理员应看到 2 条，实际 %d（total=%d）", p.Total, p.Total)
		}
	})

	t.Run("普通用户只看自己", func(t *testing.T) {
		p := hit(func(r *http.Request) *http.Request { return asUser(r, "u1") },
			"/admin/api/usage/history?from=0")
		if p.Total != 1 {
			t.Fatalf("u1 应只看到 1 条，实际 %d —— 跨用户泄露", p.Total)
		}
		if len(p.Records) != 1 || p.Records[0].PublicModel != "m-a" {
			t.Fatalf("u1 看到的应是自己的 m-a，实际 %+v", p.Records)
		}
	})

	// 关键的一条：显式传别人的 key_id 也必须拿不到。
	//
	// 只验「不带参数时收窄」是不够的 —— 实现完全可能靠「按当前用户的 key
	// 列表 JOIN」来收窄，那样 ?key_id=别人的key 就是一条绕过路径。
	t.Run("显式传别人的 key_id 仍是空集", func(t *testing.T) {
		p := hit(func(r *http.Request) *http.Request { return asUser(r, "u1") },
			"/admin/api/usage/history?from=0&key_id=keyU2")
		if p.Total != 0 || len(p.Records) != 0 {
			t.Fatalf("u1 指定 u2 的 key_id 应得空集，实际 total=%d records=%d —— 可被参数绕过",
				p.Total, len(p.Records))
		}
	})
}

// TestExportCSV_ScopedToCaller 钉住 CSV 导出同样收窄。
//
// 单独一个测试而不是并入上面那个：ExportCSV 与 History 是**两个 handler**，
// 各自调用 usageHistoryFilters。一处改对了不代表另一处也改对了，而导出是
// 「把别人的明细整表下载走」，是泄露后果最重的一种形式。
func TestExportCSV_ScopedToCaller(t *testing.T) {
	st := newTestStore(t)
	seedUsageForScope(t, st)
	h := NewUsageHandler(st)

	run := func(as func(*http.Request) *http.Request, url string) [][]string {
		t.Helper()
		rec := httptest.NewRecorder()
		h.ExportCSV(rec, as(httptest.NewRequest(http.MethodGet, url, nil)))
		if rec.Code != http.StatusOK {
			t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
		}
		rows, err := csv.NewReader(rec.Body).ReadAll()
		if err != nil {
			t.Fatalf("csv parse: %v", err)
		}
		return rows
	}

	admin := run(asAdmin, "/admin/api/usage/history.csv?from=0")
	if len(admin) != 3 { // 表头 + 2 条
		t.Fatalf("管理员应导出 3 行（表头+2 条），实际 %d", len(admin))
	}

	user := run(func(r *http.Request) *http.Request { return asUser(r, "u1") },
		"/admin/api/usage/history.csv?from=0")
	if len(user) != 2 { // 表头 + 1 条
		t.Fatalf("u1 应只导出 2 行（表头+1 条），实际 %d —— CSV 泄露", len(user))
	}
	// 逐字节确认另一条记录没混进来（按行数断言不够：万一实现是「少一条但多一条
	// 别人的」，行数照样对得上）。m-b 是 u2 的公开模型名。
	for _, row := range user {
		for _, cell := range row {
			if cell == "m-b" {
				t.Fatalf("u1 的导出里出现了 u2 的模型名 m-b：%v", user)
			}
		}
	}
}
