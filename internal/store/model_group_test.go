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

// TestModelGroup_SurvivesProviderDeletion 是 2026-11-03 的核心回归：
// 把两个模型归进一组，删掉其中一个 provider，归属必须留在独立表里，
// 否则 by-model 会把该上游的历史用量重新裂成单独一行。
func TestModelGroup_SurvivesProviderDeletion(t *testing.T) {
	st := testStore(t, t.TempDir()+"/g.db")
	ctx := context.Background()
	seedProvider(t, st, "p1")
	seedProvider(t, st, "p2")
	seedGroup(t, st, "g1", "DeepSeek-V4.1-Flash")

	models := []*UpstreamModel{
		{ID: "m1", ProviderID: "p1", ModelID: "deepseek-v4.1-flash"},
		{ID: "m2", ProviderID: "p2", ModelID: "cn:deepseek-v4.1-flash"},
	}
	for _, m := range models {
		if err := st.CreateUpstreamModel(ctx, m); err != nil {
			t.Fatalf("create model %s: %v", m.ID, err)
		}
	}
	if _, err := st.AssignModelsToGroup(ctx, []string{"m1", "m2"}, "g1"); err != nil {
		t.Fatalf("assign: %v", err)
	}
	// 两条历史用量分别指向两个上游 —— 删上游之后它们仍按组合并。
	for i, m := range models {
		if err := st.CreateUsageRecord(ctx, &UsageRecord{
			ID: "u" + m.ID, Ts: 1_700_000_000_000 + int64(i), AccessKeyID: "k1",
			PublicModel: "any", ProviderID: m.ProviderID, UpstreamModel: m.ModelID,
			IngressProtocol: "chat", TotalTokens: 100, Status: "ok", HTTPStatus: 200,
		}); err != nil {
			t.Fatalf("usage %s: %v", m.ID, err)
		}
	}
	if err := st.DeleteProvider(ctx, "p2"); err != nil {
		t.Fatalf("delete provider: %v", err)
	}

	// 独立表里的归属必须还在；模型行已被级联删除。
	var groupID string
	if err := st.DB().QueryRowContext(ctx,
		`SELECT group_id FROM model_group_members WHERE provider_id = ? AND model_id = ?`,
		"p2", "cn:deepseek-v4.1-flash").Scan(&groupID); err != nil {
		t.Fatalf("归属表丢失了已删上游的成员: %v", err)
	}
	if groupID != "g1" {
		t.Fatalf("归属变成 %q, want g1", groupID)
	}
	var modelRows int
	if err := st.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM upstream_models WHERE provider_id = 'p2'`).Scan(&modelRows); err != nil {
		t.Fatalf("count models: %v", err)
	}
	if modelRows != 0 {
		t.Fatalf("删 provider 后仍有 %d 个模型行", modelRows)
	}
}

// TestModelGroup_DeleteProviderDropsUnusedMembership 从没产生过量用的归属
// 在删上游时一并清掉：留下它只会让成员数多出一个既无模型行、又无统计的
// 不可见空壳。
func TestModelGroup_DeleteProviderDropsUnusedMembership(t *testing.T) {
	st := testStore(t, t.TempDir()+"/g.db")
	ctx := context.Background()
	seedProvider(t, st, "p1")
	seedGroup(t, st, "g1", "G")
	if err := st.CreateUpstreamModel(ctx, &UpstreamModel{
		ID: "m1", ProviderID: "p1", ModelID: "unused",
	}); err != nil {
		t.Fatalf("model: %v", err)
	}
	if _, err := st.AssignModelsToGroup(ctx, []string{"m1"}, "g1"); err != nil {
		t.Fatalf("assign: %v", err)
	}
	if err := st.DeleteProvider(ctx, "p1"); err != nil {
		t.Fatalf("delete provider: %v", err)
	}
	var n int
	if err := st.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM model_group_members WHERE provider_id = 'p1'`).Scan(&n); err != nil {
		t.Fatalf("count memberships: %v", err)
	}
	if n != 0 {
		t.Fatalf("无用量归属应随上游删除清掉，剩 %d 行", n)
	}
}

// TestModelGroup_ListHistoryMembersAndReassign 历史成员要能被列出，并能按
// (provider_id, model_id) 重新挂回组 —— 这正是「删上游后统计裂行」的补救入口。
func TestModelGroup_ListHistoryMembersAndReassign(t *testing.T) {
	st := testStore(t, t.TempDir()+"/g.db")
	ctx := context.Background()
	seedProvider(t, st, "p1")
	seedGroup(t, st, "g1", "G")

	if err := st.CreateUsageRecord(ctx, &UsageRecord{
		ID: "u1", Ts: 1_700_000_000_000, AccessKeyID: "k1", PublicModel: "any",
		ProviderID: "p1", ProviderName: "腾讯WorkBuddy", UpstreamModel: "cn:deepseek-v4.1-flash",
		IngressProtocol: "chat", TotalTokens: 500, Status: "ok", HTTPStatus: 200,
	}); err != nil {
		t.Fatalf("usage: %v", err)
	}
	// 删掉上游：模型行与 provider 行都没了，只剩用量里固化的名字。
	if err := st.DeleteProvider(ctx, "p1"); err != nil {
		t.Fatalf("delete provider: %v", err)
	}
	orphans, err := st.ListUngroupedHistoryMembers(ctx)
	if err != nil {
		t.Fatalf("list orphans: %v", err)
	}
	if len(orphans) != 1 || orphans[0].ProviderID != "p1" ||
		orphans[0].ModelID != "cn:deepseek-v4.1-flash" ||
		orphans[0].ProviderName != "腾讯WorkBuddy" || orphans[0].Tokens != 500 {
		t.Fatalf("孤儿历史成员不对: %+v", orphans)
	}
	if _, err := st.AssignHistoryMembersToGroup(ctx, "g1", []GroupMemberRef{
		{ProviderID: "p1", ModelID: "cn:deepseek-v4.1-flash"},
	}); err != nil {
		t.Fatalf("assign history: %v", err)
	}

	history, err := st.ListGroupHistoryMembers(ctx, "g1")
	if err != nil {
		t.Fatalf("list history: %v", err)
	}
	if len(history) != 1 || history[0].ModelID != "cn:deepseek-v4.1-flash" || history[0].Tokens != 500 {
		t.Fatalf("组的历史成员不对: %+v", history)
	}
	// 再列孤儿时应为空（已经归组了）。
	orphans, err = st.ListUngroupedHistoryMembers(ctx)
	if err != nil {
		t.Fatalf("list orphans again: %v", err)
	}
	if len(orphans) != 0 {
		t.Fatalf("归组后仍列为孤儿: %+v", orphans)
	}
}

// TestModelGroup_RepairOrphanMembershipOnce 钉住一次性修补的两个边界：
//   - 真模型对得上（cn:deepseek-v4.1-flash ↔ deepseek-v4.1-flash）时补回组；
//   - 只是共用 public_model 的其它模型（cn:space-bunny）不能被误并。
func TestModelGroup_RepairOrphanMembershipOnce(t *testing.T) {
	path := t.TempDir() + "/repair.db"
	st := testStore(t, path)
	ctx := context.Background()
	seedProvider(t, st, "p-live")
	seedGroup(t, st, "g1", "DeepSeek-V4.1-Flash")
	if err := st.CreateUpstreamModel(ctx, &UpstreamModel{
		ID: "m-live", ProviderID: "p-live", ModelID: "deepseek-v4.1-flash",
	}); err != nil {
		t.Fatalf("model: %v", err)
	}
	if _, err := st.AssignModelsToGroup(ctx, []string{"m-live"}, "g1"); err != nil {
		t.Fatalf("assign: %v", err)
	}
	// 现存成员必须有历史用量，修补才有「该组的模型叫法」可对照。
	if err := st.CreateUsageRecord(ctx, &UsageRecord{
		ID: "u-live", Ts: 1_700_000_000_000, AccessKeyID: "k1",
		PublicModel: "DeepSeek-V4.1-Flash", ProviderID: "p-live", UpstreamModel: "deepseek-v4.1-flash",
		IngressProtocol: "chat", TotalTokens: 100, Status: "ok", HTTPStatus: 200,
	}); err != nil {
		t.Fatalf("live usage: %v", err)
	}

	// 删掉的腾讯上游留下的两条历史用量：一个是同模型的中转叫法，
	// 另一个只是恰好服务同一个公开模型名。
	for _, r := range []struct{ id, model string }{
		{"u-real", "cn:deepseek-v4.1-flash"},
		{"u-other", "cn:space-bunny"},
	} {
		if err := st.CreateUsageRecord(ctx, &UsageRecord{
			ID: r.id, Ts: 1_700_000_000_000, AccessKeyID: "k1",
			PublicModel: "DeepSeek-V4.1-Flash", ProviderID: "p-gone", UpstreamModel: r.model,
			IngressProtocol: "chat", TotalTokens: 100, Status: "ok", HTTPStatus: 200,
		}); err != nil {
			t.Fatalf("usage %s: %v", r.id, err)
		}
	}

	// 让修补在重新打开时跑：这模拟升级到本版本后的首次启动。
	if err := st.repairOrphanGroupMembershipsOnce(ctx); err != nil {
		t.Fatalf("repair: %v", err)
	}
	memberships := map[string]string{}
	rows, err := st.db.Query(
		`SELECT model_id, group_id FROM model_group_members WHERE provider_id = 'p-gone'`)
	if err != nil {
		t.Fatalf("read memberships: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var modelID, groupID string
		if err := rows.Scan(&modelID, &groupID); err != nil {
			t.Fatalf("scan: %v", err)
		}
		memberships[modelID] = groupID
	}
	if memberships["cn:deepseek-v4.1-flash"] != "g1" {
		t.Fatalf("真模型没补回组: %v", memberships)
	}
	if _, ok := memberships["cn:space-bunny"]; ok {
		t.Fatalf("只是共用公开模型名的其它模型被误并: %v", memberships)
	}

	// 幂等 + 尊重用户：手动移出后再次启动不能补回来。
	if err := st.repairOrphanGroupMembershipsOnce(ctx); err != nil {
		t.Fatalf("second repair: %v", err)
	}
	if err := st.setSetting(ctx, settingModelGroupOrphanRepairKey, "1"); err != nil {
		t.Fatalf("mark: %v", err)
	}
	if _, err := st.db.ExecContext(ctx,
		`DELETE FROM model_group_members WHERE provider_id = 'p-gone'`); err != nil {
		t.Fatalf("manual unassign: %v", err)
	}
	if err := st.repairOrphanGroupMembershipsOnce(ctx); err != nil {
		t.Fatalf("repair after manual unassign: %v", err)
	}
	var n int
	if err := st.db.QueryRow(
		`SELECT COUNT(*) FROM model_group_members WHERE provider_id = 'p-gone'`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatal("修补已经跑过，手动移出的成员不该再被加回")
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
