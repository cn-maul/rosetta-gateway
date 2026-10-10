package upstream

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/cn-maul/rosetta"
	"github.com/cn-maul/rosetta-gateway/internal/effort"
	"github.com/cn-maul/rosetta-gateway/internal/store"
)

// 网关配好的模型能力 → SDK 模型档案。这是「单一真相源」的接线处：
// 能力只在网关存一份，SDK 拿到的是它的一份拷贝（而不是各自维护）。

func thinkingStore(t *testing.T) (*store.Store, string) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	st, err := store.Open(t.TempDir()+"/think.db", logger)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	ctx := context.Background()
	p := &store.Provider{
		ID: "p1", Slug: "p1", Name: "P1", Protocol: "openai-chat",
		Endpoint: "https://example.test", Enabled: true,
	}
	if err := st.CreateProvider(ctx, p); err != nil {
		t.Fatalf("create provider: %v", err)
	}
	return st, p.ID
}

func addModel(t *testing.T, st *store.Store, pid, id, modelID string, sup *bool, levels []string) {
	t.Helper()
	var lv []effort.Level
	for _, s := range levels {
		lv = append(lv, effort.Level(s))
	}
	m := &store.UpstreamModel{
		ID: id, ProviderID: pid, ModelID: modelID, Enabled: true,
		SupportsThinking: sup, EffortLevels: lv,
	}
	if err := st.CreateUpstreamModel(context.Background(), m); err != nil {
		t.Fatalf("create model %s: %v", id, err)
	}
}

// TestModelInfosForProvider_三态映射 钉住「未配置」与「不支持」的分别处理。
//
// 这一格错了就是一次全站影响的能力回退：把未配置当成 false，所有从没用过
// 这个功能的模型会集体失去思考能力，而没人做过任何配置动作。
func TestModelInfosForProvider_三态映射(t *testing.T) {
	st, pid := thinkingStore(t)
	yes, no := true, false
	addModel(t, st, pid, "m1", "supports", &yes, nil)
	addModel(t, st, pid, "m2", "denies", &no, nil)
	addModel(t, st, pid, "m3", "unknown", nil, nil)

	infos := modelInfosForProvider(context.Background(), st, pid, slog.Default())
	byID := map[string]rosetta.ModelInfo{}
	for _, i := range infos {
		byID[i.ID] = i
	}

	// 未配置的模型**不产出条目**：产出一条 Known 且 SupportsThinking=false
	// 的条目等于宣称「它不思考」。
	if _, ok := byID["unknown"]; ok {
		t.Fatalf("未配置思考能力的模型不该产出档案条目：%+v", byID["unknown"])
	}

	sup := byID["supports"]
	if !sup.SupportsThinking || sup.DisableThinking {
		t.Fatalf("支持思考的模型档案不对：%+v", sup)
	}

	den := byID["denies"]
	if den.SupportsThinking || !den.DisableThinking {
		// DisableThinking 必须为真：SDK 的合并语义是 OR，单靠
		// SupportsThinking=false 压不过低优先级层。
		t.Fatalf("不支持思考的模型档案不对（缺 DisableThinking）：%+v", den)
	}
}

func TestModelInfosForProvider_挡位透传(t *testing.T) {
	st, pid := thinkingStore(t)
	yes := true
	addModel(t, st, pid, "m1", "gpt-5", &yes, []string{"low", "xhigh"})

	infos := modelInfosForProvider(context.Background(), st, pid, slog.Default())
	if len(infos) != 1 {
		t.Fatalf("期望 1 条档案，实际 %d", len(infos))
	}
	if len(infos[0].EffortLevels) != 2 || infos[0].EffortLevels[1] != "xhigh" {
		t.Fatalf("挡位未透传：%v", infos[0].EffortLevels)
	}
	// 配了挡位就等于声明能思考，SDK 侧会据此把 SupportsThinking 点亮。
	if !infos[0].SupportsThinking {
		t.Fatal("配了挡位却没声明支持思考")
	}
}

// TestModelInfosForProvider_只带思考能力 守住「不要把网关的能力再抄一份
// 进 SDK」这条纪律：上下文/价格留在网关快照里，抄过去就产生两份可能分叉的
// 真相 —— 而这正是本次收敛要消灭的东西。
func TestModelInfosForProvider_只带思考能力(t *testing.T) {
	st, pid := thinkingStore(t)
	yes := true
	ctx := context.Background()
	m := &store.UpstreamModel{
		ID: "m1", ProviderID: pid, ModelID: "gpt-5", Enabled: true,
		SupportsThinking: &yes, ContextWindow: 200000, MaxOutputTokens: 64000,
	}
	if err := st.CreateUpstreamModel(ctx, m); err != nil {
		t.Fatalf("create: %v", err)
	}
	infos := modelInfosForProvider(ctx, st, pid, slog.Default())
	if len(infos) != 1 {
		t.Fatalf("期望 1 条，实际 %d", len(infos))
	}
	if infos[0].ContextWindow != 0 || infos[0].MaxOutputTokens != 0 {
		t.Fatalf("容量不该被复制进 SDK 档案：%+v", infos[0])
	}
}

// TestBuildClient_思考闸门因注入而生效 是这条链路真正有价值的证据：
// 注入前，SDK 从不知道本网关有哪些模型，gateThinking 恒走「未知模型」分支
// 直接放行 —— 也就是说 supports_thinking 的执行点当时根本不存在。
func TestBuildClient_思考闸门因注入而生效(t *testing.T) {
	st, pid := thinkingStore(t)
	no := false
	yes := true
	addModel(t, st, pid, "m1", "gpt-4o", &no, nil)
	addModel(t, st, pid, "m2", "o3", &yes, nil)

	c, err := buildClient(&ProviderEntry{Protocol: "openai-chat", Endpoint: "https://example.test"},
		"sk-x", nil, modelInfosForProvider(context.Background(), st, pid, slog.Default())...)
	if err != nil {
		t.Fatalf("buildClient: %v", err)
	}

	// 声明不支持思考的模型：思考请求被 SDK 拒绝。
	_, err = c.Chat(context.Background(), &rosetta.ChatRequest{
		Model:    "gpt-4o",
		Messages: []rosetta.Message{rosetta.User("hi")},
		Thinking: &rosetta.ThinkingConfig{Effort: rosetta.EffortHigh},
	})
	if !errors.Is(err, rosetta.ErrThinkingUnsupported) {
		t.Fatalf("不支持思考的模型应被拒绝，实际 err=%v", err)
	}

	// 声明支持思考的模型：正常放行（会真的发请求，但 endpoint 不可达，
	// 所以只要错误不是「不支持思考」就说明闸门放行了）。
	_, err = c.Chat(context.Background(), &rosetta.ChatRequest{
		Model:    "o3",
		Messages: []rosetta.Message{rosetta.User("hi")},
		Thinking: &rosetta.ThinkingConfig{Effort: rosetta.EffortHigh},
	})
	if errors.Is(err, rosetta.ErrThinkingUnsupported) {
		t.Fatal("声明支持思考的模型不该被拒绝")
	}
}
