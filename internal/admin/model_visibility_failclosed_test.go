package admin

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cn-maul/rosetta-gateway/internal/snapshot"
)

// 2026-10-31 修（后端接口审计）：管理面的两处模型可见性查询原本把
// 「快照里查不到这个用户」当成「不受限」—— 与该文件自己写的教义
// （空 ID 不当 admin、交集为空必须返回空）方向相反，是一条静默提权：
// 快照重建期 / 用户已被删但会话还在时，普通用户会拿到**全集**。
//
// 下面两条钉住 fail-closed：身份查不到 → 空集 + restricted=true。

func TestModelNames_UnknownUserIsFailClosed(t *testing.T) {
	initSnapshotWithUsers(t, []string{"flash", "pro"}, map[string]*snapshot.UserSnapshot{
		// 快照里**没有** "ghost" 这个 id
		"u1": snapUser("u1", "", snapshot.AllowAll()),
	})
	h := NewGroupHandler(newScopeStore(t))

	got := fetchModelNames(t, h, asUser(httptest.NewRequest(http.MethodGet, "/admin/api/model-names", nil), "ghost"))
	if !got.Restricted {
		t.Error("快照里查不到的用户必须被当成受限（restricted=true），不能当不受限")
	}
	if len(got.Models) != 0 || len(got.Prices) != 0 || len(got.Meta) != 0 {
		t.Fatalf("未知用户应拿到空集，实际 models=%v prices=%v meta=%v", got.Models, got.Prices, got.Meta)
	}
}

func TestProbeModels_UnknownUserIsFailClosed(t *testing.T) {
	st := newScopeStore(t)
	initProbeScopedSnapshot(t, map[string]*snapshot.UserSnapshot{
		"u1": snapUser("u1", "", snapshot.AllowAll()),
	})
	seedProbe(t, st, "up/flash", true)
	h := NewProbeHandler(st, nil)

	// 摘要端点：未知用户不该看到任何模型行。
	rec := httptest.NewRecorder()
	h.Models(rec, asUser(httptest.NewRequest(http.MethodGet, "/admin/api/probes/models", nil), "ghost"))
	if rec.Code != http.StatusOK {
		t.Fatalf("probes/models code = %d body = %s", rec.Code, rec.Body.String())
	}
	var rows []modelHealthResponse
	decodeBody(t, rec, &rows)
	if len(rows) != 0 {
		t.Fatalf("未知用户不该看到任何模型探测行，实际 %d 行: %+v", len(rows), rows)
	}

	// 时间轴端点：同理，一个桶都不该返回（且不是 403 —— 该 id 只是不可见，
	// 不存在的 id 已在更早的分支按「无权查看」处理）。
	code, buckets := fetchTimeline(t, h, asUser(
		httptest.NewRequest(http.MethodGet, "/admin/api/probes/timeline?model_id=up%2Fflash", nil), "ghost"))
	if code == http.StatusForbidden {
		// 允许这种处理：未知用户解析不出任何可见模型，等价于「全都越权」。
		return
	}
	if code != http.StatusOK {
		t.Fatalf("timeline code = %d, want 200 or 403", code)
	}
	if len(buckets) != 0 {
		t.Fatalf("未知用户不该看到任何桶，实际 %+v", buckets)
	}
}
