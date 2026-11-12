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
