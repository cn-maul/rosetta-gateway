package store

import (
	"context"
	"errors"
	"testing"
)

// 删除上游模型 + 自动摘链（2026-10-14）。四组用例各钉一个语义。

func seedChainModel(t *testing.T, ctx context.Context, st *Store, pid, id, modelID string) {
	t.Helper()
	if err := st.CreateUpstreamModel(ctx, &UpstreamModel{
		ID: id, ProviderID: pid, ModelID: modelID, Enabled: true,
	}); err != nil {
		t.Fatalf("create model %s: %v", id, err)
	}
}

func chainPositions(t *testing.T, ctx context.Context, st *Store, routeID string) []int {
	t.Helper()
	rows, err := st.ListRouteTargets(ctx, routeID)
	if err != nil {
		t.Fatalf("list targets: %v", err)
	}
	out := make([]int, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Position)
	}
	return out
}

func chainModels(t *testing.T, ctx context.Context, st *Store, routeID string) []string {
	t.Helper()
	rows, err := st.ListRouteTargets(ctx, routeID)
	if err != nil {
		t.Fatalf("list targets: %v", err)
	}
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.UpstreamModelID)
	}
	return out
}

// 链首拒绝删除，且**不留任何副作用**。
//
// 「不留副作用」是要点：链首检查与删除在同一事务里，若检查通过后事务回滚、
// 或先删了链行再判链首，链会被搞坏而用户以为删除失败了。
func TestDeleteUpstreamModelDetaching_链首拒绝(t *testing.T) {
	ctx := context.Background()
	st := testStore(t, t.TempDir()+"/del1.db")
	pid := newModelTestProvider(t, st)

	seedChainModel(t, ctx, st, pid, "m1", "chat-a")
	seedChainModel(t, ctx, st, pid, "m2", "chat-b")
	if err := st.CreateRoute(ctx, &Route{
		ID: "r1", PublicName: "flash", ProviderID: pid, UpstreamModelID: "m1", Enabled: true,
	}); err != nil {
		t.Fatalf("create route: %v", err)
	}
	if err := st.ReplaceRouteTargets(ctx, "r1", []RouteTarget{
		{ProviderID: pid, UpstreamModelID: "m1", Enabled: true},
		{ProviderID: pid, UpstreamModelID: "m2", Enabled: true},
	}); err != nil {
		t.Fatalf("set chain: %v", err)
	}

	if _, err := st.DeleteUpstreamModelDetaching(ctx, "m1"); !errors.Is(err, ErrModelIsRouteHead) {
		t.Fatalf("删除链首 err = %v，期望 ErrModelIsRouteHead", err)
	}

	// 模型还在。
	if m, _ := st.GetUpstreamModel(ctx, "m1"); m == nil {
		t.Fatal("链首拒绝后模型被删掉了")
	}
	// 链完好。
	if got := chainModels(t, ctx, st, "r1"); len(got) != 2 {
		t.Fatalf("链首拒绝后链被破坏：%v", got)
	}
}

// 中间成员可以删，且后续成员**相对顺序不变**（position 收缩到 0..n-1）。
func TestDeleteUpstreamModelDetaching_中间成员(t *testing.T) {
	ctx := context.Background()
	st := testStore(t, t.TempDir()+"/del2.db")
	pid := newModelTestProvider(t, st)

	for _, id := range []string{"m1", "m2", "m3"} {
		seedChainModel(t, ctx, st, pid, id, "chat-"+id)
	}
	if err := st.CreateRoute(ctx, &Route{
		ID: "r1", PublicName: "flash", ProviderID: pid, UpstreamModelID: "m1", Enabled: true,
	}); err != nil {
		t.Fatalf("create route: %v", err)
	}
	if err := st.ReplaceRouteTargets(ctx, "r1", []RouteTarget{
		{ProviderID: pid, UpstreamModelID: "m1", Enabled: true},
		{ProviderID: pid, UpstreamModelID: "m2", Enabled: true},
		{ProviderID: pid, UpstreamModelID: "m3", Enabled: true},
	}); err != nil {
		t.Fatalf("set chain: %v", err)
	}

	affected, err := st.DeleteUpstreamModelDetaching(ctx, "m2")
	if err != nil {
		t.Fatalf("删除中间成员失败：%v", err)
	}
	if len(affected) != 1 || affected[0] != "r1" {
		t.Errorf("affected = %v，期望 [r1]", affected)
	}

	if m, _ := st.GetUpstreamModel(ctx, "m2"); m != nil {
		t.Error("m2 没被删掉")
	}
	got := chainModels(t, ctx, st, "r1")
	if len(got) != 2 || got[0] != "m1" || got[1] != "m3" {
		t.Fatalf("链成员 = %v，期望 [m1 m3]（顺序不能变）", got)
	}
	// position 必须重排，否则界面上显示「0, 2」这种跳号，像数据坏了。
	if pos := chainPositions(t, ctx, st, "r1"); pos[0] != 0 || pos[1] != 1 {
		t.Errorf("position = %v，期望 [0 1]", pos)
	}
	// 链首没被动过：routes 的主目标列必须仍指向 m1。
	var head string
	if err := st.DB().QueryRowContext(ctx,
		`SELECT upstream_model_id FROM routes WHERE id='r1'`).Scan(&head); err != nil {
		t.Fatalf("read route head: %v", err)
	}
	if head != "m1" {
		t.Errorf("routes 主目标列变成了 %q，期望 m1", head)
	}
}

// 跨路由共享：同一个模型在多条链的中间，删一次全部摘干净。
func TestDeleteUpstreamModelDetaching_跨路由共享(t *testing.T) {
	ctx := context.Background()
	st := testStore(t, t.TempDir()+"/del3.db")
	pid := newModelTestProvider(t, st)

	seedChainModel(t, ctx, st, pid, "m1", "a")
	seedChainModel(t, ctx, st, pid, "m2", "b")
	seedChainModel(t, ctx, st, pid, "m3", "c")

	for i, head := range []string{"m1", "m3"} {
		rid := "r" + string(rune('1'+i))
		if err := st.CreateRoute(ctx, &Route{
			ID: rid, PublicName: "flash" + rid, ProviderID: pid, UpstreamModelID: head, Enabled: true,
		}); err != nil {
			t.Fatalf("create route %s: %v", rid, err)
		}
		// m2 都是中间成员。
		if err := st.ReplaceRouteTargets(ctx, rid, []RouteTarget{
			{ProviderID: pid, UpstreamModelID: head, Enabled: true},
			{ProviderID: pid, UpstreamModelID: "m2", Enabled: true},
		}); err != nil {
			t.Fatalf("set chain %s: %v", rid, err)
		}
	}

	affected, err := st.DeleteUpstreamModelDetaching(ctx, "m2")
	if err != nil {
		t.Fatalf("删除失败：%v", err)
	}
	if len(affected) != 2 {
		t.Errorf("affected = %v，期望两条路由都被摘", affected)
	}
	for _, rid := range []string{"r1", "r2"} {
		if got := chainModels(t, ctx, st, rid); len(got) != 1 {
			t.Errorf("%s 的链 = %v，期望只剩链首", rid, got)
		}
	}
}

// 探测记录级联清理。
//
// 探测记录是纯派生数据，对已删模型没有意义；留着会在 /probes/models 里
// 变成一堆取不到模型行的孤儿记录。
func TestDeleteUpstreamModelDetaching_清探测记录(t *testing.T) {
	ctx := context.Background()
	st := testStore(t, t.TempDir()+"/del4.db")
	pid := newModelTestProvider(t, st)

	seedChainModel(t, ctx, st, pid, "m1", "orphan")
	seedChainModel(t, ctx, st, pid, "m2", "keep")
	if err := st.CreateRoute(ctx, &Route{
		ID: "r1", PublicName: "flash", ProviderID: pid, UpstreamModelID: "m2", Enabled: true,
	}); err != nil {
		t.Fatalf("create route: %v", err)
	}
	if err := st.ReplaceRouteTargets(ctx, "r1", []RouteTarget{
		{ProviderID: pid, UpstreamModelID: "m2", Enabled: true},
		{ProviderID: pid, UpstreamModelID: "m1", Enabled: true},
	}); err != nil {
		t.Fatalf("set chain: %v", err)
	}

	for _, id := range []string{"m1", "m2"} {
		if err := st.RecordProbe(ctx, ProbeRecord{
			ModelID: id, ProviderID: pid, Ts: 1, OK: true, LatencyMs: 5,
		}); err != nil {
			t.Fatalf("record probe %s: %v", id, err)
		}
	}

	if _, err := st.DeleteUpstreamModelDetaching(ctx, "m1"); err != nil {
		t.Fatalf("删除失败：%v", err)
	}

	var n int
	if err := st.DB().QueryRowContext(ctx,
		`SELECT count(*) FROM model_probes WHERE model_id='m1'`).Scan(&n); err != nil {
		t.Fatalf("count probes: %v", err)
	}
	if n != 0 {
		t.Errorf("已删模型还留着 %d 条探测记录（应级联清理）", n)
	}
	// 别人的探测记录必须留着。
	if err := st.DB().QueryRowContext(ctx,
		`SELECT count(*) FROM model_probes WHERE model_id='m2'`).Scan(&n); err != nil {
		t.Fatalf("count probes m2: %v", err)
	}
	if n != 1 {
		t.Errorf("m2 的探测记录被误删（剩 %d 条）", n)
	}
}

// 不存在的 id 返回 ErrNotFound，而不是「删除成功」。
func TestDeleteUpstreamModelDetaching_不存在(t *testing.T) {
	ctx := context.Background()
	st := testStore(t, t.TempDir()+"/del5.db")

	if _, err := st.DeleteUpstreamModelDetaching(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v，期望 ErrNotFound", err)
	}
}