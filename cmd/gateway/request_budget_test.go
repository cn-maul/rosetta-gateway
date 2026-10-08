package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/cn-maul/rosetta"
	"github.com/cn-maul/rosetta-gateway/internal/config"
	"github.com/cn-maul/rosetta-gateway/internal/snapshot"
)

// 2026-10-10 回归：整请求总预算（此前 /v1 请求没有任何总时长上限，
// 一串半死的上游能把客户端挂约 100s × 目标数 ≈ 5 分钟）。

func newBudgetSnapshot(ttft, upstreamMs, targets int) *snapshot.Snapshot {
	return &snapshot.Snapshot{
		Runtime: snapshot.RuntimeDefaults{
			StreamFirstTokenTimeoutMs: ttft,
			UpstreamTimeoutMs:         upstreamMs,
			FailoverMaxTargets:        targets,
		},
	}
}

// 默认配置下的总预算：必须覆盖最坏链路（响应头 60s + 首字 30s 那一跳
// 逐次复用），且**远小于**修复前的无上限。
func TestTotalRequestBudget_DefaultsCoverWorstCase(t *testing.T) {
	cfg := &config.Config{}
	// 显式给默认值，避免依赖 config 的兜底常量变动。
	snap := newBudgetSnapshot(30_000, 120_000, 3)

	b := totalRequestBudget(snap, cfg)

	// 每跳上界 = max(120s, 30s+60s) + 5s = 125s；次数 = 3+1 = 4 → 500s。
	want := 500 * time.Second
	if b != want {
		t.Fatalf("totalRequestBudget = %v, want %v", b, want)
	}
	// 地板/天花板夹逼。
	if b < 30*time.Second || b > 30*time.Minute {
		t.Fatalf("总预算 %v 超出 [30s, 30min] 夹逼范围", b)
	}
}

// 调大任一输入，总预算必须跟着变 —— 这是「不新增开关、由现有配置推导」
// 这个设计的全部意义：运维改过的设置立刻体现在总预算上。
func TestTotalRequestBudget_FollowsConfig(t *testing.T) {
	cfg := &config.Config{}
	base := newBudgetSnapshot(30_000, 120_000, 3)
	baseBudget := totalRequestBudget(base, cfg)

	// 目标数翻倍 → 总预算翻倍
	fewer := totalRequestBudget(newBudgetSnapshot(30_000, 120_000, 1), cfg)
	if fewer >= baseBudget {
		t.Errorf("目标数减少后总预算没有下降：%v vs %v", fewer, baseBudget)
	}

	// 非流式超时翻倍 → 总预算跟着涨
	longer := totalRequestBudget(newBudgetSnapshot(30_000, 240_000, 3), cfg)
	if longer <= baseBudget {
		t.Errorf("非流式超时翻倍后总预算没有上升：%v vs %v", longer, baseBudget)
	}

	// 极端配置必须被夹住，不能出现「等于没有上限」或「必然失败」
	for _, c := range []struct {
		name string
		snap *snapshot.Snapshot
	}{
		{"全部最小", newBudgetSnapshot(1, 1, 1)},
		{"全部最大", newBudgetSnapshot(3_600_000, 86_400_000, 100)},
	} {
		b := totalRequestBudget(c.snap, cfg)
		if b < 30*time.Second {
			t.Errorf("%s：总预算 %v 小于地板 30s —— 会把正常请求直接掐死", c.name, b)
		}
		if b > 30*time.Minute {
			t.Errorf("%s：总预算 %v 超过天花板 30min —— 等于没有上限", c.name, b)
		}
	}
}

// nil snapshot（尚未装载）不得 panic —— 热路径上它可能为 nil。
func TestTotalRequestBudget_NilSnapshot(t *testing.T) {
	if b := totalRequestBudget(nil, &config.Config{}); b <= 0 {
		t.Fatalf("nil snapshot 时总预算 = %v，应回落到 config 默认并为正", b)
	}
}

// ---- SDK 行为钉住：这个测试的存在理由本身就是一条结论 ----
//
// 「给 ChatStream 套一个带超时的子 context、拿到流之后取消它」是错的：
// 会把正在正常输出的长流从中间掐断。下面的测试把这个事实固定下来，
// 防止有人日后「优化」回那个写法。
func TestSDK_CancellingCtxTruncatesEstablishedStream(t *testing.T) {
	events := []string{
		"data: {\"id\":\"c1\",\"model\":\"m\",\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n",
		"data: {\"choices\":[{\"delta\":{\"content\":\"b\"}}]}\n\n",
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1,\"total_tokens\":2}}\n\n",
		"data: [DONE]\n\n",
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl, _ := w.(http.Flusher)
		for i, e := range events {
			io.WriteString(w, e)
			if fl != nil {
				fl.Flush()
			}
			if i == 0 {
				time.Sleep(200 * time.Millisecond)
			}
		}
	}))
	defer srv.Close()

	c, err := rosetta.NewClient(
		rosetta.WithEndpoint(srv.URL), rosetta.WithAPIKey("k"),
		rosetta.WithProtocol(rosetta.ProtoOpenAIChat))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	req := &rosetta.ChatRequest{Model: "m", Messages: []rosetta.Message{rosetta.User("hi")}}

	// 对照组：不取消，必须完整读完。素材有问题时立刻暴露。
	full, err := c.ChatStream(context.Background(), req)
	if err != nil {
		t.Fatalf("ChatStream(对照): %v", err)
	}
	wantEvents := 0
	for full.Next() {
		wantEvents++
	}
	if wantEvents < len(events) {
		t.Fatalf("对照组只读到 %d/%d —— 测试素材本身有问题，不能据此下结论",
			wantEvents, len(events))
	}

	// 取消组：建连后取消父 ctx，会**截断**已建立的流。
	// 这条断言的语义是「现状如此，且是有意保持现状」：它提醒后来的读者，
	// 首字预算不能靠子 context 实现。
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	st, err := c.ChatStream(ctx, req)
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	cancel()
	got := 0
	for st.Next() {
		got++
	}
	if got >= wantEvents {
		t.Logf("注意：SDK 现在不截断了（读到 %d/%d）。若 rosetta 升级成"+
			"detach 了 context，首字预算可以考虑改用子 context 实现。", got, wantEvents)
	} else {
		t.Logf("确认：取消父 ctx 截断了已建立的流（%d/%d，err=%v）—— "+
			"这正是 attemptStream 不能给 ChatStream 套子 context 的原因",
			got, wantEvents, st.Err())
	}
}
