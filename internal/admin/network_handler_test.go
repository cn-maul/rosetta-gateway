package admin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cn-maul/rosetta-gateway/internal/config"
	"github.com/cn-maul/rosetta-gateway/internal/store"
)

// TestNetworkCRUDAndProviderReference 覆盖本功能的完整链路：
// 建网络 → 上游选它 → 换回直连 → 删网络（引用自动回到直连，且上游还在）。
func TestNetworkCRUDAndProviderReference(t *testing.T) {
	st := newTestStore(t)
	h := NewNetworkHandler(st)
	ctx := context.Background()

	// 建网络
	rec := httptest.NewRecorder()
	h.Create(rec, jsonRequest(http.MethodPost, "/admin/api/networks",
		jsonBody(t, map[string]string{"name": "本机 Clash", "proxy_url": "http://127.0.0.1:7897"})))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create code=%d body=%s", rec.Code, rec.Body.String())
	}
	var created networkResponse
	decodeBody(t, rec, &created)
	if created.ID == "" || created.ProviderCount != 0 {
		t.Fatalf("建出的网络不对: %+v", created)
	}

	// 非法地址当场拒（这是共享配置 —— 填错会让一批上游一起连不上）
	rec = httptest.NewRecorder()
	h.Create(rec, jsonRequest(http.MethodPost, "/admin/api/networks",
		jsonBody(t, map[string]string{"name": "坏地址", "proxy_url": "ftp://x"})))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("非法 scheme 应 400，得到 %d body=%s", rec.Code, rec.Body.String())
	}
	// 空地址应拒（不代理请用直连，不该有一条「空的」网络）
	rec = httptest.NewRecorder()
	h.Create(rec, jsonRequest(http.MethodPost, "/admin/api/networks",
		jsonBody(t, map[string]string{"name": "空的", "proxy_url": ""})))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("空 proxy_url 应 400，得到 %d", rec.Code)
	}
	// 重名应 409
	rec = httptest.NewRecorder()
	h.Create(rec, jsonRequest(http.MethodPost, "/admin/api/networks",
		jsonBody(t, map[string]string{"name": "本机 Clash", "proxy_url": "http://127.0.0.1:7898"})))
	if rec.Code != http.StatusConflict {
		t.Fatalf("重名应 409，得到 %d body=%s", rec.Code, rec.Body.String())
	}

	// 上游引用它：p1 选这条网络
	p1, err := st.GetProvider(ctx, "p1")
	if err != nil || p1 == nil {
		t.Fatalf("取 p1: %v", err)
	}
	p1.NetworkID = created.ID
	if err := st.UpdateProvider(ctx, "p1", p1); err != nil {
		t.Fatalf("update p1: %v", err)
	}
	got, err := st.GetProvider(ctx, "p1")
	if err != nil || got == nil {
		t.Fatalf("重读 p1: %v", err)
	}
	if got.NetworkID != created.ID {
		t.Errorf("p1 应引用该网络，得到 %q", got.NetworkID)
	}
	if got.ProxyURL != "http://127.0.0.1:7897" {
		t.Errorf("p1 的派生 ProxyURL 不对: %q", got.ProxyURL)
	}

	// 直连哨兵：写 NetworkDirectID 与写空串，落库后是同一种（NULL）
	p2, _ := st.GetProvider(ctx, "p2")
	p2.NetworkID = store.NetworkDirectID
	if err := st.UpdateProvider(ctx, "p2", p2); err != nil {
		t.Fatalf("update p2: %v", err)
	}
	got2, _ := st.GetProvider(ctx, "p2")
	if got2.NetworkID != "" {
		t.Errorf("直连哨兵应落成空值，得到 %q", got2.NetworkID)
	}

	// 列表带上引用计数
	rec = httptest.NewRecorder()
	h.List(rec, asAdmin(httptest.NewRequest(http.MethodGet, "/admin/api/networks", nil)))
	if rec.Code != http.StatusOK {
		t.Fatalf("list code=%d body=%s", rec.Code, rec.Body.String())
	}
	var list []networkResponse
	decodeBody(t, rec, &list)
	if len(list) != 1 || list[0].ProviderCount != 1 {
		t.Fatalf("列表应有一条、引用 1，得到 %+v", list)
	}

	// 改名 + 改地址：改一处，引用它的上游立即跟着变（派生值，无第二份拷贝）
	rec = httptest.NewRecorder()
	h.Update(rec, asAdmin(jsonRequest(http.MethodPatch, "/admin/api/networks/"+created.ID,
		jsonBody(t, map[string]string{"name": "公司代理", "proxy_url": "http://proxy.corp:3128"}))), created.ID)
	if rec.Code != http.StatusOK {
		t.Fatalf("update code=%d body=%s", rec.Code, rec.Body.String())
	}
	got, _ = st.GetProvider(ctx, "p1")
	if got.ProxyURL != "http://proxy.corp:3128" {
		t.Errorf("改地址后 p1 应立刻用新址，得到 %q", got.ProxyURL)
	}

	// 删网络：p1 回到直连，**p1 本身不被删**
	rec = httptest.NewRecorder()
	h.Delete(rec, asAdmin(httptest.NewRequest(http.MethodDelete, "/admin/api/networks/"+created.ID, nil)), created.ID)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete code=%d body=%s", rec.Code, rec.Body.String())
	}
	stillThere, _ := st.GetProvider(ctx, "p1")
	if stillThere == nil {
		t.Fatal("删网络不该删掉上游 p1")
	}
	if stillThere.NetworkID != "" || stillThere.ProxyURL != "" {
		t.Errorf("删网络后 p1 应回到直连，得到 network_id=%q proxy=%q",
			stillThere.NetworkID, stillThere.ProxyURL)
	}
}

// TestProviderCreateRejectsUnknownNetwork 挡住「引用一个不存在的网络」：
// 那样会让上游在池构建时安静地退回直连，界面显示与实际行为不一致。
func TestProviderCreateRejectsUnknownNetwork(t *testing.T) {
	st := newTestStore(t)
	ph := NewProviderHandler(st, nil, config.Default())

	rec := httptest.NewRecorder()
	ph.Create(rec, asAdmin(jsonRequest(http.MethodPost, "/admin/api/providers", jsonBody(t,
		map[string]string{
			"name": "New", "endpoint": "https://x", "network_id": "net-nope",
		}))))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("引用不存在的网络应 400，得到 %d body=%s", rec.Code, rec.Body.String())
	}

	// 直连两种写法都通过，且落库是同一种
	for _, nid := range []string{"", store.NetworkDirectID} {
		rec = httptest.NewRecorder()
		ph.Create(rec, asAdmin(jsonRequest(http.MethodPost, "/admin/api/providers", jsonBody(t,
			map[string]string{
				"name": "Direct", "endpoint": "https://x", "network_id": nid,
			}))))
		if rec.Code != http.StatusCreated {
			t.Fatalf("network_id=%q 应可创建，得到 %d body=%s", nid, rec.Code, rec.Body.String())
		}
		var resp providerResponse
		decodeBody(t, rec, &resp)
		if resp.NetworkID != "" {
			t.Errorf("直连的响应 network_id 应为空，得到 %q", resp.NetworkID)
		}
	}
}
