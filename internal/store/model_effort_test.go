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
		ID: "p1", Slug: "p1", Name: "P1", Protocol: "openai-chat",
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
		ID: "m1", ProviderID: pid, ModelID: "gpt-5", Enabled: true,
		EffortLevels: []effort.Level{effort.Low, effort.High, effort.XHigh},
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

	plain := &UpstreamModel{ID: "m1", ProviderID: pid, ModelID: "gpt-4o", Enabled: true}
	if err := st.CreateUpstreamModel(ctx, plain); err != nil {
		t.Fatalf("create: %v", err)
	}
	got, _ := st.GetUpstreamModel(ctx, "m1")
	if len(got.EffortLevels) != 0 {
		t.Fatalf("未配置的模型不该有挡位，实际 %v", got.EffortLevels)
	}

	weird := &UpstreamModel{
		ID: "m2", ProviderID: pid, ModelID: "custom-thinker", Enabled: true,
		EffortLevels: []effort.Level{effort.High}, UnknownLevels: []string{"ultra_deep"},
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
		ID: "m1", ProviderID: pid, ModelID: "gpt-5", Enabled: true,
		EffortLevels: []effort.Level{effort.High},
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

// TestUpsertModel_不覆盖挡位 与既有价格纪律同源：批量导入/重新发现的
// 调用方拿不到挡位信息，若 upsert 把它覆盖成空，等于「点一次同步就抹掉
// 手工配置」。
func TestUpsertModel_不覆盖挡位(t *testing.T) {
	ctx := context.Background()
	st := testStore(t, t.TempDir()+"/effort4.db")
	pid := newModelTestProvider(t, st)

	orig := &UpstreamModel{
		ID: "m1", ProviderID: pid, ModelID: "gpt-5", Enabled: true,
		EffortLevels: []effort.Level{effort.High, effort.XHigh},
		PriceInput:   3.0,
	}
	if err := st.UpsertUpstreamModel(ctx, orig); err != nil {
		t.Fatalf("upsert 1: %v", err)
	}

	// 重新导入：同 provider + model_id，携带零值（探测侧拿不到这些信息）。
	if err := st.UpsertUpstreamModel(ctx, &UpstreamModel{
		ID: "m2", ProviderID: pid, ModelID: "gpt-5", Enabled: true,
	}); err != nil {
		t.Fatalf("upsert 2: %v", err)
	}

	got, _ := st.GetUpstreamModel(ctx, "m1")
	if len(got.EffortLevels) != 2 {
		t.Fatalf("重新导入把挡位抹掉了：%+v", got)
	}
	if got.PriceInput != 3.0 {
		t.Fatalf("重新导入把价格抹掉了：%v", got.PriceInput)
	}
}
