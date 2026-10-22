package store

import (
	"context"
	"testing"

	"github.com/cn-maul/rosetta-gateway/internal/effort"
)

// newModelTestProvider 建一个 provider，供上游模型的往返测试使用。
func newModelTestProvider(t *testing.T, st *Store) string {
	t.Helper()
	p := &Provider{
		ID: "p1", Slug: "p1", Name: "P1", Protocol: "chat",
		Endpoint: "https://example.test", Enabled: true,
	}
	if err := st.CreateProvider(context.Background(), p); err != nil {
		t.Fatalf("create provider: %v", err)
	}
	return p.ID
}

// TestEffortLevels_RoundTrip 钉住「档位写进去能原样读回来」这条链路。
//
// 它守住的是 modelColumns / scanModel / effortColumnValue 三处必须同步：
// 任何一处漏改，症状都是**静默丢数据** —— 配好的挡位读回来是空，于是
// /v1/models 不下发 supported_reasoning、数据面不夹取，而界面毫无异样。
func TestEffortLevels_RoundTrip(t *testing.T) {
	ctx := context.Background()
	st := testStore(t, t.TempDir()+"/effort.db")
	pid := newModelTestProvider(t, st)

	m := &UpstreamModel{
		ID: "m1", ProviderID: pid, ModelID: "gpt-5", EffortLevels: []effort.Level{effort.Low, effort.High, effort.XHigh},
	}
	if err := st.CreateUpstreamModel(ctx, m); err != nil {
		t.Fatalf("create model: %v", err)
	}

	got, err := st.GetUpstreamModel(ctx, "m1")
	if err != nil || got == nil {
		t.Fatalf("get model: %v", err)
	}
	want := []effort.Level{effort.Low, effort.High, effort.XHigh}
	if len(got.EffortLevels) != len(want) {
		t.Fatalf("挡位 = %v，期望 %v", got.EffortLevels, want)
	}
	for i := range want {
		if got.EffortLevels[i] != want[i] {
			t.Fatalf("挡位[%d] = %q，期望 %q", i, got.EffortLevels[i], want[i])
		}
	}

	list, err := st.ListUpstreamModels(ctx, pid)
	if err != nil || len(list) != 1 {
		t.Fatalf("list models: %v（%d 条）", err, len(list))
	}
	if len(list[0].EffortLevels) != len(want) {
		t.Fatalf("列表路径没带出挡位：%+v", list[0])
	}
}

// TestEffortLevels_未配置与未知档位 守住两个容易被合并掉的语义：
//   - 「未配置」必须落 NULL、读回空切片（数据面据此**不干预**）；
//   - 网关不认识的厂商私有档必须**原样保留**，而不是被吞掉。
func TestEffortLevels_未配置与未知档位(t *testing.T) {
	ctx := context.Background()
	st := testStore(t, t.TempDir()+"/effort2.db")
	pid := newModelTestProvider(t, st)

	plain := &UpstreamModel{ID: "m1", ProviderID: pid, ModelID: "gpt-4o"}
	if err := st.CreateUpstreamModel(ctx, plain); err != nil {
		t.Fatalf("create: %v", err)
	}
	got, _ := st.GetUpstreamModel(ctx, "m1")
	if len(got.EffortLevels) != 0 {
		t.Fatalf("未配置的模型不该有挡位，实际 %v", got.EffortLevels)
	}

	weird := &UpstreamModel{
		ID: "m2", ProviderID: pid, ModelID: "custom-thinker", EffortLevels: []effort.Level{effort.High}, UnknownLevels: []string{"ultra_deep"},
	}
	if err := st.CreateUpstreamModel(ctx, weird); err != nil {
		t.Fatalf("create: %v", err)
	}
	got, _ = st.GetUpstreamModel(ctx, "m2")
	if len(got.EffortLevels) != 1 || got.EffortLevels[0] != effort.High {
		t.Fatalf("已知档位丢失：%+v", got)
	}
	if len(got.UnknownLevels) != 1 || got.UnknownLevels[0] != "ultra_deep" {
		t.Fatalf("未知档位被吞掉：%+v", got)
	}
}

// TestEffortLevels_清除守住 PATCH 语义里最容易被漏掉的一格：
// 空切片必须落 NULL（回到未配置），而不是落空串 —— 后者与「配了但一个
// 有效档都没有」在数据面上无法区分。
func TestEffortLevels_清除(t *testing.T) {
	ctx := context.Background()
	st := testStore(t, t.TempDir()+"/effort3.db")
	pid := newModelTestProvider(t, st)

	m := &UpstreamModel{
		ID: "m1", ProviderID: pid, ModelID: "gpt-5", EffortLevels: []effort.Level{effort.High},
	}
	if err := st.CreateUpstreamModel(ctx, m); err != nil {
		t.Fatalf("create: %v", err)
	}

	// PATCH 语义：把挡位清空。
	m.EffortLevels = nil
	m.ModelID = "gpt-5"
	if err := st.UpdateUpstreamModel(ctx, "m1", m); err != nil {
		t.Fatalf("update: %v", err)
	}
	got, _ := st.GetUpstreamModel(ctx, "m1")
	if len(got.EffortLevels) != 0 || len(got.UnknownLevels) != 0 {
		t.Fatalf("清除后仍有挡位：%+v", got)
	}

	// 再存回去，确认「清除」不是一次不可逆的破坏。
	m.EffortLevels = []effort.Level{effort.Medium}
	if err := st.UpdateUpstreamModel(ctx, "m1", m); err != nil {
		t.Fatalf("re-update: %v", err)
	}
	got, _ = st.GetUpstreamModel(ctx, "m1")
	if len(got.EffortLevels) != 1 || got.EffortLevels[0] != effort.Medium {
		t.Fatalf("重新写入失败：%+v", got)
	}
}

// TestUpsertModel_不覆盖已有挡位 但**会在空时回填**（2026-10-14 改）。
//
// 价格仍是「一律不覆盖」：上游返回的模型列表永远不带价格，覆盖它等于
// 「点一次同步就抹掉手工配置」。
//
// effort_levels 的规则不同，因为它多了一个来源 —— 上游 /models 的
// reasoning_supported_efforts。原实现假定导入侧拿不到挡位信息所以不覆盖，
// 那个前提已经不成立，于是「不覆盖」从保护措施变成了缺陷：**已有模型
// 永远填不上**，管理员只能逐个手勾。
//
// 现在是「只在空时回填」：能自动填上，同时不顶掉手工挑过的组合。
func TestUpsertModel_不覆盖已有挡位但空时回填(t *testing.T) {
	ctx := context.Background()
	st := testStore(t, t.TempDir()+"/effort4.db")
	pid := newModelTestProvider(t, st)

	orig := &UpstreamModel{
		ID: "m1", ProviderID: pid, ModelID: "gpt-5", EffortLevels: []effort.Level{effort.High, effort.XHigh},
		PriceInput: 3.0,
	}
	if err := st.UpsertUpstreamModel(ctx, orig); err != nil {
		t.Fatalf("upsert 1: %v", err)
	}

	// 重新导入，且这次**带了**挡位（上游报了）：已有配置必须保留。
	if err := st.UpsertUpstreamModel(ctx, &UpstreamModel{
		ID: "m2", ProviderID: pid, ModelID: "gpt-5", EffortLevels: []effort.Level{effort.Low, effort.Medium},
	}); err != nil {
		t.Fatalf("upsert 2: %v", err)
	}
	got, _ := st.GetUpstreamModel(ctx, "m1")
	if len(got.EffortLevels) != 2 {
		t.Fatalf("重新导入顶掉了手工挡位：%+v", got)
	}
	if got.EffortLevels[0] != effort.High || got.EffortLevels[1] != effort.XHigh {
		t.Errorf("手工挡位内容变了：%v", got.EffortLevels)
	}
	if got.PriceInput != 3.0 {
		t.Fatalf("重新导入把价格抹掉了：%v", got.PriceInput)
	}

	// 未配置过的模型：上游报了就应该被回填 —— 这正是本次要修的能力。
	if err := st.UpsertUpstreamModel(ctx, &UpstreamModel{
		ID: "m3", ProviderID: pid, ModelID: "fresh"}); err != nil {
		t.Fatalf("upsert 3: %v", err)
	}
	// 模拟「首次导入时上游就报了」：清空后用带挡位的 payload 写入。
	if err := st.UpsertUpstreamModel(ctx, &UpstreamModel{
		ID: "m4", ProviderID: pid, ModelID: "fresh", EffortLevels: []effort.Level{effort.Low, effort.Medium, effort.Max},
	}); err != nil {
		t.Fatalf("upsert 4: %v", err)
	}
	got, _ = st.GetUpstreamModel(ctx, "m3")
	if len(got.EffortLevels) != 3 {
		t.Fatalf("空配置未被回填：%+v", got)
	}
	// 再来一次不带挡位的导入：已回填的值必须还在（不回退）。
	if err := st.UpsertUpstreamModel(ctx, &UpstreamModel{
		ID: "m5", ProviderID: pid, ModelID: "fresh"}); err != nil {
		t.Fatalf("upsert 5: %v", err)
	}
	got, _ = st.GetUpstreamModel(ctx, "m3")
	if len(got.EffortLevels) != 3 {
		t.Fatalf("后续导入把已回填的挡位清掉了：%+v", got)
	}
}

// TestUpsertModel_新模型可写入挡位 与上一条互为镜像：upsert 不覆盖**已有**
// 值，但首次 INSERT 必须把发现来的挡位写进去 —— 否则「自动预填」在
// 真实的导入路径（全是 upsert）上一条都落不了地。
func TestUpsertModel_新模型可写入挡位(t *testing.T) {
	ctx := context.Background()
	st := testStore(t, t.TempDir()+"/effort5.db")
	pid := newModelTestProvider(t, st)

	if err := st.UpsertUpstreamModel(ctx, &UpstreamModel{
		ID: "m1", ProviderID: pid, ModelID: "cn:space-bunny", EffortLevels: []effort.Level{effort.Low, effort.Medium, effort.High, effort.XHigh, effort.Max},
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	got, _ := st.GetUpstreamModel(ctx, "m1")
	if len(got.EffortLevels) != 5 {
		t.Fatalf("首次导入没写进挡位：%+v", got)
	}
	// max 必须以 max 存下来，而不是被压成 xhigh —— 上游的默认值就是它，
	// 存成 xhigh 会让「上游默认档」这一信息在库这一层就丢了。
	last := got.EffortLevels[len(got.EffortLevels)-1]
	if last != effort.Max {
		t.Errorf("最高档存成了 %q，期望 max（压成 xhigh 会与上游对不上账）", last)
	}
}

// TestUpsertModel_supports_thinking_三态守住 nil 的含义。
//
// 「上游没报」（nil）绝不能把已有配置抹成 NULL：那会把「已明确支持」变成
// 「不知道」，而管理界面上这两种状态长得一模一样，丢得无声无息。
// 反过来，上游**明确表态**时必须能覆盖 —— 否则上游改了能力网关也看不见。
func TestUpsertModel_supports_thinking_三态(t *testing.T) {
	ctx := context.Background()
	st := testStore(t, t.TempDir()+"/effort6.db")
	pid := newModelTestProvider(t, st)

	yes := true
	if err := st.UpsertUpstreamModel(ctx, &UpstreamModel{
		ID: "m1", ProviderID: pid, ModelID: "cn:space-bunny", SupportsThinking: &yes,
	}); err != nil {
		t.Fatalf("upsert 1: %v", err)
	}

	// 上游没报（nil）→ 保留 true。
	if err := st.UpsertUpstreamModel(ctx, &UpstreamModel{
		ID: "m2", ProviderID: pid, ModelID: "cn:space-bunny"}); err != nil {
		t.Fatalf("upsert 2: %v", err)
	}
	got, _ := st.GetUpstreamModel(ctx, "m1")
	if got.SupportsThinking == nil || !*got.SupportsThinking {
		t.Fatalf("上游没报就把已配的 true 抹成了 %v", got.SupportsThinking)
	}

	// 上游明确说 false → 必须覆盖（上游能力变了要看得见）。
	no := false
	if err := st.UpsertUpstreamModel(ctx, &UpstreamModel{
		ID: "m3", ProviderID: pid, ModelID: "cn:space-bunny", SupportsThinking: &no,
	}); err != nil {
		t.Fatalf("upsert 3: %v", err)
	}
	got, _ = st.GetUpstreamModel(ctx, "m1")
	if got.SupportsThinking == nil || *got.SupportsThinking {
		t.Fatalf("上游明确说 false 却没覆盖：%v", got.SupportsThinking)
	}
}
