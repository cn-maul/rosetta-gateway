package store

import (
	"context"
	"errors"
	"testing"
)

// seedProvider 建一个上游，返回其 id。
func seedProvider(t *testing.T, st *Store, id string) {
	t.Helper()
	if err := st.CreateProvider(context.Background(), &Provider{
		ID: id, Slug: id, Name: id, Endpoint: "http://example.invalid",
		Enabled: true, Protocol: "openai-chat",
	}); err != nil {
		t.Fatalf("seed provider %s: %v", id, err)
	}
}

// seedCred 建一把凭据。
func seedCred(t *testing.T, st *Store, pid, id string, enabled bool) {
	t.Helper()
	if err := st.CreateCredential(context.Background(), &Credential{
		ID: id, ProviderID: pid, Label: id, APIKeyEnc: []byte("sk-" + id),
		Enabled: enabled, Weight: 1, Status: "healthy",
	}); err != nil {
		t.Fatalf("seed credential %s: %v", id, err)
	}
}

// enabledIDs 返回该 provider 下每把凭据的启用态（id → enabled）。
func enabledIDs(t *testing.T, st *Store, pid string) map[string]bool {
	t.Helper()
	creds, err := st.ListCredentials(context.Background(), pid)
	if err != nil {
		t.Fatalf("list credentials of %s: %v", pid, err)
	}
	out := make(map[string]bool, len(creds))
	for _, c := range creds {
		out[c.ID] = c.Enabled
	}
	return out
}

// TestEnableExclusiveCredential_OnlyOneEnabledAtATime 是本文件的核心断言：
// 一次 EnableExclusiveCredential 之后，该 provider 下**恰好**一把凭据启用，
// 且就是指定的那把。
//
// 断言用「计数 == 1 且就是目标」而不是「目标为 true」：只查目标为 true 的话，
// 「把其它全停掉了但目标没开」这种反向故障会假通过 —— 而那正是切 key
// 时最坏的结果（该上游一把都没有，全部请求报 no healthy credentials）。
func TestEnableExclusiveCredential_OnlyOneEnabledAtATime(t *testing.T) {
	st := testStore(t, t.TempDir()+"/x.db")
	ctx := context.Background()
	seedProvider(t, st, "p1")
	seedCred(t, st, "p1", "c1", true)
	seedCred(t, st, "p1", "c2", false)
	seedCred(t, st, "p1", "c3", false)

	if err := st.EnableExclusiveCredential(ctx, "p1", "c2"); err != nil {
		t.Fatalf("EnableExclusiveCredential: %v", err)
	}

	got := enabledIDs(t, st, "p1")
	if !got["c2"] {
		t.Fatalf("目标凭据 c2 未启用: %v", got)
	}
	if got["c1"] {
		t.Fatalf("c1 应已停用: %v", got)
	}
	on := 0
	for _, v := range got {
		if v {
			on++
		}
	}
	if on != 1 {
		t.Fatalf("期望恰好一把启用，实际 %d 把: %v", on, got)
	}
}

// TestEnableExclusiveCredential_Idempotent 反复切到同一把不应改变结果。
//
// 运维会重复点同一个开关（点一次没看到变化再点一次）。如果第二次调用
// 反而把状态弄乱，这条幂等性就是必需的。
func TestEnableExclusiveCredential_Idempotent(t *testing.T) {
	st := testStore(t, t.TempDir()+"/x.db")
	ctx := context.Background()
	seedProvider(t, st, "p1")
	seedCred(t, st, "p1", "c1", true)
	seedCred(t, st, "p1", "c2", false)

	for i := 0; i < 3; i++ {
		if err := st.EnableExclusiveCredential(ctx, "p1", "c1"); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	got := enabledIDs(t, st, "p1")
	if !got["c1"] || got["c2"] {
		t.Fatalf("重复启用后状态漂移: %v", got)
	}
}

// TestEnableExclusiveCredential_ForeignProviderRollsBack 覆盖最容易出事的一条：
// 拿 A 上游的凭据 id 去 B 上游上启用，必须整笔失败且**不动** B 的任何凭据。
//
// 危险点在实现顺序上：先执行的是「把该 provider 下其它凭据全停掉」。
// 若第二步发现 id 不属于本 provider 而直接返回（没有回滚），B 的凭据已经
// 被清空了 —— 而 B 与这次调用毫无关系，它会突然变成 ready=false。
// 所以这里断言的是失败后的**副作用为零**，不只是返回值。
func TestEnableExclusiveCredential_ForeignProviderRollsBack(t *testing.T) {
	st := testStore(t, t.TempDir()+"/x.db")
	ctx := context.Background()
	seedProvider(t, st, "p1")
	seedProvider(t, st, "p2")
	seedCred(t, st, "p1", "c1", true)
	seedCred(t, st, "p2", "b1", true)
	seedCred(t, st, "p2", "b2", false)

	err := st.EnableExclusiveCredential(ctx, "p2", "c1") // c1 属于 p1，不是 p2
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("期望 ErrNotFound，实际 %v", err)
	}

	got := enabledIDs(t, st, "p2")
	if !got["b1"] {
		t.Fatalf("失败的调用仍停用了 p2 的凭据（副作用未回滚）: %v", got)
	}
	if got["b2"] {
		t.Fatalf("p2 的 b2 不应被影响: %v", got)
	}
}

// TestEnableExclusiveCredential_UnknownID 覆盖 id 完全不存在。
func TestEnableExclusiveCredential_UnknownID(t *testing.T) {
	st := testStore(t, t.TempDir()+"/x.db")
	ctx := context.Background()
	seedProvider(t, st, "p1")
	seedCred(t, st, "p1", "c1", true)

	if err := st.EnableExclusiveCredential(ctx, "p1", "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("期望 ErrNotFound，实际 %v", err)
	}
	if !enabledIDs(t, st, "p1")["c1"] {
		t.Fatal("失败的调用把原有凭据停用了")
	}
}

// TestHasEnabledCredential 覆盖新建默认态所依赖的那个判断。
func TestHasEnabledCredential(t *testing.T) {
	st := testStore(t, t.TempDir()+"/x.db")
	ctx := context.Background()
	seedProvider(t, st, "p1")
	seedProvider(t, st, "p2")

	has, err := st.HasEnabledCredential(ctx, "p1")
	if err != nil || has {
		t.Fatalf("空 provider 应报告无启用凭据: has=%v err=%v", has, err)
	}

	seedCred(t, st, "p1", "c1", false)
	if has, err := st.HasEnabledCredential(ctx, "p1"); err != nil || has {
		t.Fatalf("只有停用凭据时应为 false: has=%v err=%v", has, err)
	}

	seedCred(t, st, "p1", "c2", true)
	if has, err := st.HasEnabledCredential(ctx, "p1"); err != nil || !has {
		t.Fatalf("有启用凭据时应为 true: has=%v err=%v", has, err)
	}

	// 另一个 provider 的启用凭据不得影响本 provider 的判定。
	if has, err := st.HasEnabledCredential(ctx, "p2"); err != nil || has {
		t.Fatalf("判定被别的 provider 污染: has=%v err=%v", has, err)
	}
}