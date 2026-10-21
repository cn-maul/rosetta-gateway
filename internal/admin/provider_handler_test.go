package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cn-maul/rosetta-gateway/internal/snapshot"
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
