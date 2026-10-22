package admin

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/cn-maul/rosetta-gateway/internal/config"
	"github.com/cn-maul/rosetta-gateway/internal/store"
)

// jsonBody 把任意值编码成请求体（本文件测试专用的薄封装）。
func jsonBody(t *testing.T, v any) *bytes.Buffer {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return bytes.NewBuffer(raw)
}

// seedUsage 落一条最小用量记录（by-model 聚合的燃料）。
func seedUsage(t *testing.T, st *store.Store, id, providerID, upstreamModel string, tokens int64) {
	t.Helper()
	if err := st.CreateUsageRecord(t.Context(), &store.UsageRecord{
		ID: id, Ts: time.Now().UnixMilli(), AccessKeyID: "k1", UserID: "admin-test",
		PublicModel: "any", ProviderID: providerID, UpstreamModel: upstreamModel,
		IngressProtocol: "chat", InputTokens: tokens, TotalTokens: tokens,
		Status: "ok", HTTPStatus: 200, UsageState: "reported",
	}); err != nil {
		t.Fatalf("seed usage %s: %v", id, err)
	}
}

// callByModel 请求 /usage/by-model 并解析成分组行。
func callByModel(t *testing.T, h *UsageHandler) []usageGroupEntry {
	t.Helper()
	rec := httptest.NewRecorder()
	h.GroupByModel(rec, asAdmin(httptest.NewRequest(http.MethodGet, "/admin/api/usage/by-model", nil)))
	if rec.Code != http.StatusOK {
		t.Fatalf("by-model code=%d body=%s", rec.Code, rec.Body.String())
	}
	var entries []usageGroupEntry
	if err := json.Unmarshal(rec.Body.Bytes(), &entries); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, rec.Body.String())
	}
	return entries
}

// TestGroupByModel_MergesAcrossProviders 是特性②的核心断言：
// 两个不同 provider、不同 model_id 的模型归到同一组后，
// by-model 必须把它们合并成**一行**，键 = 组名。
func TestGroupByModel_MergesAcrossProviders(t *testing.T) {
	st := newTestStore(t) // p1/alpha-chat、p2/beta-chat 已就位
	h := NewUsageHandler(st)
	ctx := t.Context()

	// 两个上游模型归为同一组
	if err := st.CreateModelGroup(ctx, &store.ModelGroup{ID: "g1", DisplayName: "DeepSeek V3"}); err != nil {
		t.Fatalf("create group: %v", err)
	}
	if err := st.SetModelGroup(ctx, "m1", "g1"); err != nil {
		t.Fatalf("assign m1: %v", err)
	}
	if err := st.SetModelGroup(ctx, "m2", "g1"); err != nil {
		t.Fatalf("assign m2: %v", err)
	}

	seedUsage(t, st, "u-a", "p1", "alpha-chat", 100)
	seedUsage(t, st, "u-b", "p2", "beta-chat", 200)

	entries := callByModel(t, h)
	if len(entries) != 1 {
		t.Fatalf("归组后应合并成 1 行，实际 %d 行: %+v", len(entries), entries)
	}
	e := entries[0]
	if e.Key != "DeepSeek V3" {
		t.Fatalf("分组键应为组名，实际 %q", e.Key)
	}
	if e.Tokens != 300 || e.Count != 2 {
		t.Fatalf("合并后 tokens=300/count=2，实际 %d/%d", e.Tokens, e.Count)
	}
	if e.Name != "DeepSeek V3" {
		t.Fatalf("name 应回显组名，实际 %q", e.Name)
	}
}

// TestGroupByModel_UngroupedKeepsLegacyKey 守住向后兼容的边界：
// **没归组的模型必须逐字保持旧行为**（键 = upstream_model，不合并、不为空）。
//
// 这条最容易写反：COALESCE(组名, upstream_model) 如果 JOIN 键对不上，
// 所有模型都会静默退化成「各自成行」—— 与没做归组完全同形，测不出组功能坏没坏；
// 反过来如果未归组行键变成空串，统计页会出现一个没有名字的桶。
func TestGroupByModel_UngroupedKeepsLegacyKey(t *testing.T) {
	st := newTestStore(t)
	h := NewUsageHandler(st)

	// m1 归组、m2 不归
	if err := st.CreateModelGroup(t.Context(), &store.ModelGroup{ID: "g1", DisplayName: "G1"}); err != nil {
		t.Fatalf("group: %v", err)
	}
	if err := st.SetModelGroup(t.Context(), "m1", "g1"); err != nil {
		t.Fatalf("assign: %v", err)
	}

	seedUsage(t, st, "u-a", "p1", "alpha-chat", 100)
	seedUsage(t, st, "u-b", "p2", "beta-chat", 50)

	entries := callByModel(t, h)
	if len(entries) != 2 {
		t.Fatalf("一个归组一个没归，应 2 行: %+v", entries)
	}
	byKey := map[string]usageGroupEntry{}
	for _, e := range entries {
		byKey[e.Key] = e
	}
	if _, ok := byKey["G1"]; !ok {
		t.Fatalf("组名行缺失: %+v", entries)
	}
	if e, ok := byKey["beta-chat"]; !ok || e.Tokens != 50 {
		t.Fatalf("未归组模型应保持 upstream_model 键: %+v", entries)
	}
	// 未归组的行 name 必须是空串（前端据此区分合并行与普通行）
	if e := byKey["beta-chat"]; e.Name != "" {
		t.Fatalf("未归组行 name 应为空，实际 %q", e.Name)
	}
}

// TestGroupByModel_DeletedModelStillCounts 用量指向的模型行已不在 upstream_models：
// LEFT JOIN 落空 → 回退到 upstream_model 键，用量不能丢。
//
// 为什么用「用量指向一个不存在的模型行」而不是「先建再删模型」：模型被 route
// 引用时是 ON DELETE RESTRICT，删不掉（newTestStore 的 m1 就挂在 r1 上）。
// 而 usage_records 不强制 FK 到 upstream_models —— 用量是历史事实，模型行
// 后来被改名/删除是完全可能的场景，这才是本用例要覆盖的真实退化路径。
func TestGroupByModel_DeletedModelStillCounts(t *testing.T) {
	st := newTestStore(t)
	h := NewUsageHandler(st)

	// provider_id=p1 存在，但 p1 下没有 "gone-model" 这个 upstream_models 行。
	seedUsage(t, st, "u-a", "p1", "gone-model", 100)

	entries := callByModel(t, h)
	if len(entries) != 1 || entries[0].Key != "gone-model" || entries[0].Tokens != 100 {
		t.Fatalf("模型行缺失时用量应回退到 upstream_model 键: %+v", entries)
	}
}

// TestModelGroupHandler_AssignAndList 走真实 HTTP：建组→指派→成员→改名→删除。
func TestModelGroupHandler_AssignAndList(t *testing.T) {
	st := newTestStore(t)
	h := NewModelGroupHandler(st)

	rec := httptest.NewRecorder()
	req := asAdmin(jsonRequest(http.MethodPost, "/admin/api/model-groups",
		jsonBody(t, map[string]any{"display_name": "DeepSeek V3"})))
	h.Create(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create code=%d body=%s", rec.Code, rec.Body.String())
	}
	var g modelGroupResponse
	decodeBody(t, rec, &g)

	// 指派两个模型
	rec = httptest.NewRecorder()
	req = asAdmin(jsonRequest(http.MethodPost, "/admin/api/model-groups/"+g.ID+"/assign",
		jsonBody(t, map[string]any{"model_ids": []string{"m1", "m2"}})))
	h.AssignModels(rec, req, g.ID)
	if rec.Code != http.StatusOK {
		t.Fatalf("assign code=%d body=%s", rec.Code, rec.Body.String())
	}

	// List 的 member_count 必须反映指派结果
	rec = httptest.NewRecorder()
	h.List(rec, asAdmin(httptest.NewRequest(http.MethodGet, "/admin/api/model-groups", nil)))
	var groups []modelGroupResponse
	decodeBody(t, rec, &groups)
	if len(groups) != 1 || groups[0].MemberCount != 2 {
		t.Fatalf("List 成员数错误: %+v", groups)
	}

	// 改名后统计的组名跟着变（派生不回填 —— 特性②的立身之本）
	rec = httptest.NewRecorder()
	req = asAdmin(jsonRequest(http.MethodPatch, "/admin/api/model-groups/"+g.ID,
		jsonBody(t, map[string]any{"display_name": "DeepSeek V3 改名"})))
	h.Update(rec, req, g.ID)
	if rec.Code != http.StatusOK {
		t.Fatalf("rename code=%d body=%s", rec.Code, rec.Body.String())
	}

	// 删除组：模型不删、回到未归组
	rec = httptest.NewRecorder()
	h.Delete(rec, asAdmin(jsonRequest(http.MethodDelete, "/admin/api/model-groups/"+g.ID, nil)), g.ID)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete code=%d body=%s", rec.Code, rec.Body.String())
	}
	m, _ := st.GetUpstreamModel(t.Context(), "m1")
	if m == nil || m.GroupID != "" {
		t.Fatalf("删组后 m1 应仍存在且未归组: %+v", m)
	}
}

// TestModelGroupHandler_AssignRejects 指派含不存在的模型 id 必须整笔拒绝。
func TestModelGroupHandler_AssignRejects(t *testing.T) {
	st := newTestStore(t)
	h := NewModelGroupHandler(st)

	rec := httptest.NewRecorder()
	req := asAdmin(jsonRequest(http.MethodPost, "/admin/api/model-groups",
		jsonBody(t, map[string]any{"display_name": "G"})))
	h.Create(rec, req)
	var g modelGroupResponse
	decodeBody(t, rec, &g)

	rec = httptest.NewRecorder()
	req = asAdmin(jsonRequest(http.MethodPost, "/admin/api/model-groups/"+g.ID+"/assign",
		jsonBody(t, map[string]any{"model_ids": []string{"m1", "ghost"}})))
	h.AssignModels(rec, req, g.ID)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("含幽灵 id 应 404，实际 %d body=%s", rec.Code, rec.Body.String())
	}
	// 整笔拒绝：m1 不应被写
	m, _ := st.GetUpstreamModel(t.Context(), "m1")
	if m.GroupID != "" {
		t.Fatalf("部分失败时不应有模型被指派: %q", m.GroupID)
	}
}

// TestModelGroupHandler_DuplicateNameRejected 同名组必须 409。
//
// 归组靠名字对齐（导入导出也按名字匹配），重名会让两组合并成一个而无人察觉。
func TestModelGroupHandler_DuplicateNameRejected(t *testing.T) {
	st := newTestStore(t)
	h := NewModelGroupHandler(st)

	for _, want := range []int{http.StatusCreated, http.StatusConflict} {
		rec := httptest.NewRecorder()
		req := asAdmin(jsonRequest(http.MethodPost, "/admin/api/model-groups",
			jsonBody(t, map[string]any{"display_name": "Same"})))
		h.Create(rec, req)
		if rec.Code != want {
			t.Fatalf("第二次建同名组 code=%d 期望 %d body=%s", rec.Code, want, rec.Body.String())
		}
	}
}

// TestModelGroupExportRoundTrip 导出→导入的归组往返。
func TestModelGroupExportRoundTrip(t *testing.T) {
	st := newTestStore(t)
	ctx := t.Context()

	// 建组 + 指派
	if err := st.CreateModelGroup(ctx, &store.ModelGroup{ID: "g1", DisplayName: "合并名"}); err != nil {
		t.Fatalf("group: %v", err)
	}
	if err := st.SetModelGroup(ctx, "m1", "g1"); err != nil {
		t.Fatalf("assign: %v", err)
	}

	// 导出
	th := NewConfigTransferHandler(st, nil, &config.Config{})
	file, err := th.buildExport(ctx)
	if err != nil {
		t.Fatalf("buildExport: %v", err)
	}
	if file.Version != exportFormatVersion {
		t.Fatalf("导出版本应为 %d，实际 %d", exportFormatVersion, file.Version)
	}
	if len(file.ModelGroups) != 1 || file.ModelGroups[0].DisplayName != "合并名" {
		t.Fatalf("导出应含组: %+v", file.ModelGroups)
	}
	var found bool
	for _, me := range file.Models {
		if me.ModelID == "alpha-chat" && me.GroupName == "合并名" {
			found = true
		}
	}
	if !found {
		t.Fatalf("导出模型应带 group_name: %+v", file.Models)
	}

	// 导入到一个全新的空库（admin 包没有 testStore，这里就地开一个）
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	st2, err := store.Open(t.TempDir()+"/fresh.db", logger)
	if err != nil {
		t.Fatalf("open fresh: %v", err)
	}
	t.Cleanup(func() { st2.Close() })
	th2 := NewConfigTransferHandler(st2, nil, &config.Config{})
	const pass = "group-roundtrip-pass"
	enc, err := sealExport(file, pass)
	if err != nil {
		t.Fatalf("seal export: %v", err)
	}
	rec := httptest.NewRecorder()
	req := asAdmin(jsonRequest(http.MethodPost, "/admin/api/config-export/import",
		jsonBody(t, importRequest{Data: enc, Passphrase: pass})))
	th2.Import(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("import code=%d body=%s", rec.Code, rec.Body.String())
	}

	groups2, err := st2.ListModelGroups(ctx)
	if err != nil || len(groups2) != 1 || groups2[0].DisplayName != "合并名" {
		t.Fatalf("导入后应有组: %+v err=%v", groups2, err)
	}
	models2, err := st2.ListAllUpstreamModels(ctx)
	if err != nil || len(models2) != 2 {
		t.Fatalf("导入后应有 2 个模型: %+v err=%v", models2, err)
	}
	var g2 *store.ModelGroup
	g2 = &groups2[0]
	var assigned int
	for _, m := range models2 {
		if m.ModelID == "alpha-chat" {
			if m.GroupID != g2.ID {
				t.Fatalf("alpha-chat 应挂到新库的组上: group_id=%q want %q", m.GroupID, g2.ID)
			}
			assigned++
		}
	}
	if assigned != 1 {
		t.Fatal("未找到导入的 alpha-chat")
	}
}
