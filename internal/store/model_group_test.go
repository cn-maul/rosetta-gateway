package store

import (
	"context"
	"errors"
	"testing"
)

func seedGroup(t *testing.T, st *Store, id, name string) {
	t.Helper()
	if err := st.CreateModelGroup(context.Background(), &ModelGroup{ID: id, DisplayName: name}); err != nil {
		t.Fatalf("seed group %s: %v", id, err)
	}
}

// TestModelGroup_RoundTrip 建组→指派→读回→改名的完整往返。
func TestModelGroup_RoundTrip(t *testing.T) {
	st := testStore(t, t.TempDir()+"/g.db")
	ctx := context.Background()
	seedProvider(t, st, "p1")
	seedCred(t, st, "p1", "c1", true)

	if err := st.CreateUpstreamModel(ctx, &UpstreamModel{ID: "m1", ProviderID: "p1", ModelID: "deepseek-chat"}); err != nil {
		t.Fatalf("create model: %v", err)
	}
	seedGroup(t, st, "g1", "DeepSeek V3")

	// 指派
	if err := st.SetModelGroup(ctx, "m1", "g1"); err != nil {
		t.Fatalf("assign: %v", err)
	}
	m, _ := st.GetUpstreamModel(ctx, "m1")
	if m.GroupID != "g1" {
		t.Fatalf("指派后 group_id=%q 期望 g1", m.GroupID)
	}

	// 改名后读回（模型侧只存 id，组名从组表读）
	if err := st.UpdateModelGroup(ctx, "g1", "DeepSeek V3（新名）"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	g, _ := st.GetModelGroup(ctx, "g1")
	if g.DisplayName != "DeepSeek V3（新名）" {
		t.Fatalf("改名未生效: %+v", g)
	}
	// 改名不动模型的归属指针
	m, _ = st.GetUpstreamModel(ctx, "m1")
	if m.GroupID != "g1" {
		t.Fatalf("改组名不应动模型归属: %q", m.GroupID)
	}
}

// TestModelGroup_SetUnknownGroup 指派到不存在的组必须 ErrNotFound，而不是靠 FK 报错。
func TestModelGroup_SetUnknownGroup(t *testing.T) {
	st := testStore(t, t.TempDir()+"/g.db")
	ctx := context.Background()
	seedProvider(t, st, "p1")
	if err := st.CreateUpstreamModel(ctx, &UpstreamModel{ID: "m1", ProviderID: "p1", ModelID: "x"}); err != nil {
		t.Fatalf("model: %v", err)
	}
	if err := st.SetModelGroup(ctx, "m1", "ghost"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("期望 ErrNotFound，实际 %v", err)
	}
	// 未写入
	m, _ := st.GetUpstreamModel(ctx, "m1")
	if m.GroupID != "" {
		t.Fatalf("失败指派不应写入: %q", m.GroupID)
	}
}

// TestModelGroup_DeleteSetsNull 删组不删模型，成员回到未归组（FK ON DELETE SET NULL）。
func TestModelGroup_DeleteSetsNull(t *testing.T) {
	st := testStore(t, t.TempDir()+"/g.db")
	ctx := context.Background()
	seedProvider(t, st, "p1")
	seedGroup(t, st, "g1", "G")
	if err := st.CreateUpstreamModel(ctx, &UpstreamModel{ID: "m1", ProviderID: "p1", ModelID: "x", GroupID: "g1"}); err != nil {
		t.Fatalf("model: %v", err)
	}
	if err := st.SetModelGroup(ctx, "m1", "g1"); err != nil {
		t.Fatalf("assign: %v", err)
	}

	if err := st.DeleteModelGroup(ctx, "g1"); err != nil {
		t.Fatalf("delete group: %v", err)
	}
	m, _ := st.GetUpstreamModel(ctx, "m1")
	if m == nil {
		t.Fatal("删组不应删除模型")
	}
	if m.GroupID != "" {
		t.Fatalf("删组后模型应回到未归组，实际 group_id=%q", m.GroupID)
	}
}

// TestModelGroup_Unassign SetModelGroup 传空串 = 移出组（落 NULL）。
func TestModelGroup_Unassign(t *testing.T) {
	st := testStore(t, t.TempDir()+"/g.db")
	ctx := context.Background()
	seedProvider(t, st, "p1")
	seedGroup(t, st, "g1", "G")
	if err := st.CreateUpstreamModel(ctx, &UpstreamModel{ID: "m1", ProviderID: "p1", ModelID: "x", GroupID: "g1"}); err != nil {
		t.Fatalf("model: %v", err)
	}
	if err := st.SetModelGroup(ctx, "m1", ""); err != nil {
		t.Fatalf("unassign: %v", err)
	}
	m, _ := st.GetUpstreamModel(ctx, "m1")
	if m.GroupID != "" {
		t.Fatalf("移出后应为空，实际 %q", m.GroupID)
	}
}

// TestModelGroup_GroupNamesByModel 批量映射一次拿全（N+1 守卫）。
func TestModelGroup_GroupNamesByModel(t *testing.T) {
	st := testStore(t, t.TempDir()+"/g.db")
	ctx := context.Background()
	seedProvider(t, st, "p1")
	seedGroup(t, st, "g1", "G1")
	for _, id := range []string{"m1", "m2"} {
		if err := st.CreateUpstreamModel(ctx, &UpstreamModel{ID: id, ProviderID: "p1", ModelID: id, GroupID: "g1"}); err != nil {
			t.Fatalf("model %s: %v", id, err)
		}
	}
	// m3 未归组
	if err := st.CreateUpstreamModel(ctx, &UpstreamModel{ID: "m3", ProviderID: "p1", ModelID: "m3"}); err != nil {
		t.Fatalf("model m3: %v", err)
	}

	names, err := st.GroupNamesByModel(ctx)
	if err != nil {
		t.Fatalf("GroupNamesByModel: %v", err)
	}
	if names["m1"] != "G1" || names["m2"] != "G1" {
		t.Fatalf("归组模型应映射到 G1: %v", names)
	}
	if _, ok := names["m3"]; ok {
		t.Fatalf("未归组模型不应出现在映射里: %v", names)
	}
}

// TestAssignModelsToGroup_AtomicRollback 钉住批量指派的**整笔语义**。
//
// 旧实现是 handler 里逐条 SetModelGroup，每条 UPDATE 独立自动提交：
// 第 N 条失败时前 N-1 条已永久落库，而接口回 500 —— 运维看到报错重试，
// 第二次「成功」把剩下的补上，表面上一切正常，实际是两次不同写入拼出来的
// 结果。这里用「第二个 id 不存在」制造写入中途失败，断言第一个模型**没有**
// 被指派（整笔回滚），并且返回的是可判定的 ErrNotFound 而不是英文 FK 原文。
func TestAssignModelsToGroup_AtomicRollback(t *testing.T) {
	st := testStore(t, t.TempDir()+"/g.db")
	ctx := context.Background()
	seedProvider(t, st, "p1")
	for _, id := range []string{"m1", "m2"} {
		if err := st.CreateUpstreamModel(ctx, &UpstreamModel{
			ID: id, ProviderID: "p1", ModelID: id,
		}); err != nil {
			t.Fatalf("model %s: %v", id, err)
		}
	}
	seedGroup(t, st, "g1", "G1")

	// 正常批量指派：两个都进去。
	n, err := st.AssignModelsToGroup(ctx, []string{"m1", "m2"}, "g1")
	if err != nil {
		t.Fatalf("assign: %v", err)
	}
	if n != 2 {
		t.Fatalf("assigned = %d, want 2", n)
	}
	for _, id := range []string{"m1", "m2"} {
		m, _ := st.GetUpstreamModel(ctx, id)
		if m.GroupID != "g1" {
			t.Fatalf("%s group_id = %q, want g1", id, m.GroupID)
		}
	}

	// 先全部移出，再制造「第二个 id 不存在」的中途失败。
	if _, err := st.UnassignModelsFromGroup(ctx, []string{"m1", "m2"}); err != nil {
		t.Fatalf("unassign: %v", err)
	}
	_, err = st.AssignModelsToGroup(ctx, []string{"m1", "ghost"}, "g1")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound（不能把 SQLite 英文原文漏给运维）", err)
	}
	// 关键断言：m1 必须仍是未归组 —— 否则就是「半套指派」。
	m, _ := st.GetUpstreamModel(ctx, "m1")
	if m.GroupID != "" {
		t.Fatalf("中途失败后 m1 group_id = %q，整笔必须回滚", m.GroupID)
	}
}

// TestUnassignModelsFromGroup_SkipsMissing 移出组对「模型已被删」无害，
// 不该整笔拒绝；但存在的模型仍要在同一个事务里被移出。
func TestUnassignModelsFromGroup_SkipsMissing(t *testing.T) {
	st := testStore(t, t.TempDir()+"/g.db")
	ctx := context.Background()
	seedProvider(t, st, "p1")
	for _, id := range []string{"m1", "m2"} {
		if err := st.CreateUpstreamModel(ctx, &UpstreamModel{
			ID: id, ProviderID: "p1", ModelID: id,
		}); err != nil {
			t.Fatalf("model %s: %v", id, err)
		}
	}
	seedGroup(t, st, "g1", "G1")
	if _, err := st.AssignModelsToGroup(ctx, []string{"m1", "m2"}, "g1"); err != nil {
		t.Fatalf("assign: %v", err)
	}

	n, err := st.UnassignModelsFromGroup(ctx, []string{"m1", "ghost"})
	if err != nil {
		t.Fatalf("unassign: %v", err)
	}
	if n != 1 {
		t.Fatalf("unassigned = %d, want 1（ghost 不存在，跳过）", n)
	}
	if m, _ := st.GetUpstreamModel(ctx, "m1"); m.GroupID != "" {
		t.Fatalf("m1 仍在组 %q", m.GroupID)
	}
	if m, _ := st.GetUpstreamModel(ctx, "m2"); m.GroupID != "g1" {
		t.Fatalf("m2 不在本次名单里，不该被动到：%q", m.GroupID)
	}
}
