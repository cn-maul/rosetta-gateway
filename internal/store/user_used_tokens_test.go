package store

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"
)

// newArchiveStore 开一个临时库。与 usage_archive_test.go 里各测试内联的
// Open 调用等价，这里抽出来是因为本组三个测试都要用。
func newArchiveStore(t *testing.T) *Store {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	st, err := Open(filepath.Join(t.TempDir(), "u.db"), logger)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// users.used_tokens 从「展示快照」升级为配额判定数据源（2026-10-11，
// 见 internal/config 的 UsageRetentionDays 注释）之后，它由 trg_update_user_used_tokens
// 触发器累加、并在剪枝事务里回收。
//
// 这一组测试钉的就是那条**配对不变量**：触发器只加不減，剪枝负责減；
// 两者只要有一个漏掉，这个列就会单向虚高，配额因此低估已用量 ——
// 用户可以超额使用，而界面上的数字看起来完全正常。
//
// 这类缺陷不会自己暴露：虚高的值永远「看起来像用过了」，没有任何症状，
// 直到某天有人超额消耗了本该用完的额度才发现。所以必须用测试钉住。

// userTokens 读 users.used_tokens 原始列（绕开 SumUserUsedTokens，
// 因为这里要检查的正是它下面的数据源本身）。
func userTokens(t *testing.T, st *Store, userID string) int64 {
	t.Helper()
	var v int64
	if err := st.read.QueryRowContext(context.Background(),
		`SELECT total_tokens FROM user_usage_totals WHERE user_id = ?`, userID).Scan(&v); err != nil {
		t.Fatalf("read users.used_tokens(%s): %v", userID, err)
	}
	return v
}

// TestUserUsedTokens_累加与剪枝回收配对 是本组的核心一格。
//
// 断言三件事，缺一不可：
//  1. 插入明细后累加列正确（触发器生效）；
//  2. 剪枝后累加列**只减去被剪掉那部分**（回收生效）；
//  3. 剪枝后累加列仍等于「明细 + 归档」的真实合计（口径没跑偏）。
//
// 第 3 条最容易被漏：只断言 1 和 2 的话，回收减错对象也能过 ——
// 而那会让配额在剪枝后永久错位。
func TestUserUsedTokens_累加与剪枝回收配对(t *testing.T) {
	st := newArchiveStore(t)
	ctx := context.Background()
	base := time.Now()

	// archiveFixture 只造明细、不建用户行；触发器要有行可累加，这里补上。
	mustUser := func(id string) {
		t.Helper()
		if err := st.CreateUser(ctx, &User{ID: id, Username: id, Role: "user", Status: "active"}); err != nil {
			t.Fatalf("CreateUser(%s): %v", id, err)
		}
	}
	mustUser("u1")
	mustUser("u2")

	old, kept := archiveFixture(t, st, base, 30)

	// 1. 触发器累加：u1 / u2 应各自等于其全部明细的 token 合计。
	wantBefore := map[string]int64{}
	for _, uid := range []string{"u1", "u2"} {
		var sum int64
		src, args := UsageSource(UsageFilter{UserID: uid})
		if err := st.read.QueryRowContext(ctx,
			`SELECT `+UsageSumExpr("total_tokens", "u")+` FROM `+src, args...).Scan(&sum); err != nil {
			t.Fatalf("fixture sum(%s): %v", uid, err)
		}
		wantBefore[uid] = sum
	}
	for uid, want := range wantBefore {
		if got := userTokens(t, st, uid); got != want {
			t.Fatalf("剪枝前 users[%s].used_tokens = %d，期望 %d（触发器未累加明细）", uid, got, want)
		}
	}

	// 2. 剪枝
	if _, err := st.PruneOldUsage(ctx, DefaultRetentionDays); err != nil {
		t.Fatalf("PruneOldUsage: %v", err)
	}
	if old == 0 {
		t.Fatalf("fixture 造不出可剪枝的老记录，测试失去意义")
	}

	// 3. 剪枝后：累加列必须仍等于「明细 + 归档」的合计。
	//
	//    这一条同时覆盖「回收生效」与「口径没跑偏」：
	//    若回收漏了，累加列会**大于**这个真实值（虚高）；
	//    若回收减错了对象，会**不等于**（错位）。
	for uid := range wantBefore {
		var real int64
		src, args := UsageSource(UsageFilter{UserID: uid})
		if err := st.read.QueryRowContext(ctx,
			`SELECT `+UsageSumExpr("total_tokens", "u")+` FROM `+src, args...).Scan(&real); err != nil {
			t.Fatalf("post-prune sum(%s): %v", uid, err)
		}
		got := userTokens(t, st, uid)
		if got != real {
			t.Fatalf("剪枝后 users[%s].used_tokens = %d，真实合计（明细+归档）= %d\n"+
				"  差值 %d —— 回收未生效（正数=虚高，配额会被低估）",
				uid, got, real, got-real)
		}
	}

	// 无归属记录（user_id 为空串）不该累加到任何用户头上。
	//
	// 注意这里**不能**去读 id 为空串的 user 行 —— fixture 从未建过它，
	// 读到的是 sql.ErrNoRows 而非一个「错误的非零值」。真正要断言的是：
	// 孤儿明细没有凭空造出一个用户，或把量记到某个真实用户头上。
	var orphanSum int64
	if err := st.read.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(used_tokens), 0) FROM users`).Scan(&orphanSum); err != nil {
		t.Fatalf("sum all users: %v", err)
	}
	var u1Sum int64
	src1, args1 := UsageSource(UsageFilter{UserID: "u1"})
	if err := st.read.QueryRowContext(ctx,
		`SELECT `+UsageSumExpr("total_tokens", "u")+` FROM `+src1, args1...).Scan(&u1Sum); err != nil {
		t.Fatalf("u1 real sum: %v", err)
	}
	if got := userTokens(t, st, "u1"); got != u1Sum {
		t.Fatalf("无归属明细污染了 u1 的计数：累加列 %d，真实 %d", got, u1Sum)
	}
	var count int64
	if err := st.read.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM users WHERE id = ''`).Scan(&count); err != nil {
		t.Fatalf("count empty-id users: %v", err)
	}
	if count != 0 {
		t.Fatalf("孤儿明细不应造出 id 为空串的用户，实际有 %d 行", count)
	}
	_ = orphanSum

	t.Logf("配对验证通过：剪掉 %d 条、保留 %d 条后累加列仍等于真实合计", old, kept)
}

// TestUserUsedTokens_空串用户不被累加 单独守住触发器里的空串守卫。
//
// usage_records.user_id 可为空串（孤儿 key 的历史行，见 store.go 该列的
// 迁移注释）。若守卫失效，这些行会累加到 id 为空串的那个用户身上 ——
// 而"空串用户"在语义上根本不存在。
func TestUserUsedTokens_空串用户不被累加(t *testing.T) {
	st := newArchiveStore(t)
	ctx := context.Background()

	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("fixture: %v", err)
		}
	}
	must(st.CreateProvider(ctx, &Provider{
		ID: "p1", Slug: "p1", Name: "P1", Endpoint: "http://x", Protocol: "openai-chat", Enabled: true}))
	if err := st.CreateUsageRecord(ctx, &UsageRecord{
		ID: "r1", Ts: time.Now().UnixMilli(),
		UserID: "", AccessKeyID: "k1",
		PublicModel: "m", ProviderID: "p1", UpstreamModel: "m",
		IngressProtocol: "openai-chat", Stream: false,
		InputTokens: 100, OutputTokens: 100, TotalTokens: 200,
		UsageState: "reported", Status: "ok", HTTPStatus: 200,
	}); err != nil {
		t.Fatalf("CreateUsageRecord: %v", err)
	}

	// 守卫失效的症状是「凭空造出一个 id 为空串的用户」或「把量记到真实用户上」。
	var count int64
	if err := st.read.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM users`).Scan(&count); err != nil {
		t.Fatalf("count users: %v", err)
	}
	if count != 0 {
		t.Fatalf("user_id 为空串的明细不该造出任何用户行，实际 %d 行", count)
	}
}

// TestSumUserUsedTokens_用户不存在返回零不报错 钉住改读列时的语义保持。
//
// 旧实现是「对明细做 SUM」，用户不存在时无匹配行 → 返回 0 + nil。
// 改成 SELECT users.used_tokens 后自然语言上是 sql.ErrNoRows。
//
// 调用方（main.go 的用户级配额检查）对错误 **fail-open**：只记 ERROR、
// 不拒绝请求。若这里把 ErrNoRows 当错误返回，每次请求都会刷一条误导性
// 的 ERROR 日志（"user quota lookup failed"），而实际上什么都没失败。
func TestSumUserUsedTokens_用户不存在返回零不报错(t *testing.T) {
	st := newArchiveStore(t)

	got, err := st.SumUserUsedTokens(context.Background(), "no-such-user")
	if err != nil {
		t.Fatalf("用户不存在时不应返回错误（调用方会误判为查询失败并刷 ERROR）：%v", err)
	}
	if got != 0 {
		t.Fatalf("用户不存在时返回 %d，期望 0", got)
	}
}
