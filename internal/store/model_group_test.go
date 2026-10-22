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
