package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/cn-maul/rosetta-gateway/internal/snapshot"
	"github.com/cn-maul/rosetta-gateway/internal/store"
)

// TestProviderHandler_GetIncludesRuntimeState 钉住单条 GET 与列表接口
// 返回同一份运行时就绪状态。
//
// ProviderDetail 依赖这个接口直接取一条记录。若 Get 只回数据库字段，
// 详情页会显示 ready=true 的零值，把「凭据解密失败、实际不可用」的 provider
// 展示成正常；这正是审计 W4 指出的静默误导。
func TestProviderHandler_GetIncludesRuntimeState(t *testing.T) {
	st := newTestStore(t)
	h := NewProviderHandler(st, nil, nil)

	old := snapshot.Get()
	t.Cleanup(func() { snapshot.Init(old) })
	snapshot.Init(&snapshot.Snapshot{
		Providers: map[string]*snapshot.ProviderSnapshot{
			"p1": {ID: "p1", Slug: "p1", Name: "Alpha", Ready: false, Reason: "凭据解密失败"},
		},
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/api/providers/p1", nil)
	h.Get(rec, req, "p1")
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var got providerResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, rec.Body.String())
	}
	if got.Ready {
		t.Fatal("Get 应返回快照中的 ready=false，实际为零值 true")
	}
	if got.ReadyReason != "凭据解密失败" {
		t.Fatalf("ready_reason=%q，未带上快照原因", got.ReadyReason)
	}
}

// TestProviderHandler_ListIncludesProbeLatency 钉住列表接口下发探针延迟。
//
// 界面把「配置状态」列换成延迟之后，这一列的值只能来自这里：漏接就整列
// 显示「—」，而接口 200、字段存在 —— 单看响应看不出来，只能靠断言数值。
func TestProviderHandler_ListIncludesProbeLatency(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UnixMilli()
	for i, latency := range []int64{100, 200, 300} {
		if err := st.RecordProbe(ctx, store.ProbeRecord{
			ModelID: "m1", ProviderID: "p1", Ts: now - int64(i)*1000, OK: true, LatencyMs: latency,
		}); err != nil {
			t.Fatalf("record probe: %v", err)
		}
	}
	h := NewProviderHandler(st, nil, nil)

	rec := httptest.NewRecorder()
	h.List(rec, httptest.NewRequest(http.MethodGet, "/admin/api/providers", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var rows []providerResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, rec.Body.String())
	}

	var p1, p2 *providerResponse
	for i := range rows {
		switch rows[i].ID {
		case "p1":
			p1 = &rows[i]
		case "p2":
			p2 = &rows[i]
		}
	}
	if p1 == nil || p2 == nil {
		t.Fatalf("列表缺少 p1/p2：%+v", rows)
	}
	if p1.LatencySamples != 3 || p1.LatencyMs != 200 {
		t.Fatalf("p1 延迟应是 3 样本 / 200ms，实际 %d 样本 / %dms",
			p1.LatencySamples, p1.LatencyMs)
	}
	// p2 一条探测记录都没有 → 三个字段保持零值，前端据此显示「—」。
	// 若这里被写成 0ms + 非零样本数，「从未探测」会被读成「快到 0 毫秒」。
	if p2.LatencySamples != 0 || p2.LatencyMs != 0 {
		t.Fatalf("p2 没有探测记录，延迟字段应为零值，实际 %d 样本 / %dms",
			p2.LatencySamples, p2.LatencyMs)
	}
}
