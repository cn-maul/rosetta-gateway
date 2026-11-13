package admin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cn-maul/rosetta-gateway/internal/store"
)

// 这三个用例守的是同一类缺陷：PATCH 里的「未提供」被当成「显式置空」。
//
// helpers.go 写明了契约（nil = 保持原值、非 nil = 显式赋值、空串合法），
// 但契约只是注释：group 的 name/description 曾是普通 string、
// network 与 modelGroup 用 derefStr 后判空，三个端点因此都只支持
// 「整条重发」。它们没有在线上炸，是因为前端恰好每次都发全字段 ——
// 那种「恰好」不会一直成立，所以这里把行为钉住。

// TestGroupPatch_OmittingDescriptionKeepsIt 修 A7-07。
//
// 只有 name 的 PATCH 不该清空 description。此前的实现里 description 是
// 无条件赋值，于是这次请求把描述写成了空串，而前端下一次拉列表时
// 只会显示「描述没了」，没法看出是哪次请求弄丢的。
func TestGroupPatch_OmittingDescriptionKeepsIt(t *testing.T) {
	st := newScopeStore(t)
	h := NewGroupHandler(st)

	rec := httptest.NewRecorder()
	h.Create(rec, asAdmin(jsonRequest(http.MethodPost, "/admin/api/groups",
		strings.NewReader(`{"name":"研发","description":"内部使用"}`))))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: code = %d body = %s", rec.Code, rec.Body.String())
	}
	var created groupResponse
	decodeBody(t, rec, &created)

	rec2 := httptest.NewRecorder()
	h.Update(rec2, asAdmin(jsonRequest(http.MethodPatch, "/admin/api/groups/"+created.ID,
		strings.NewReader(`{"name":"研发二组"}`))), created.ID)
	if rec2.Code != http.StatusOK {
		t.Fatalf("update: code = %d body = %s", rec2.Code, rec2.Body.String())
	}

	got, err := st.GetGroup(context.Background(), created.ID)
	if err != nil || got == nil {
		t.Fatalf("get group: %v", err)
	}
	if got.Name != "研发二组" {
		t.Errorf("name = %q, want 研发二组", got.Name)
	}
	if got.Description != "内部使用" {
		t.Errorf("description = %q, want 内部使用 —— 只改 name 的 PATCH 把描述清空了", got.Description)
	}
}

// TestGroupPatch_EmptyDescriptionClearsIt 是上一条的反面：显式传空串
// 必须真的清空。少了这条，「未提供」和「提供空串」会被一起误判成同一个
// 状态，于是清空描述永远做不到。
func TestGroupPatch_EmptyDescriptionClearsIt(t *testing.T) {
	st := newScopeStore(t)
	h := NewGroupHandler(st)

	rec := httptest.NewRecorder()
	h.Create(rec, asAdmin(jsonRequest(http.MethodPost, "/admin/api/groups",
		strings.NewReader(`{"name":"研发","description":"内部使用"}`))))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: code = %d", rec.Code)
	}
	var created groupResponse
	decodeBody(t, rec, &created)

	rec2 := httptest.NewRecorder()
	h.Update(rec2, asAdmin(jsonRequest(http.MethodPatch, "/admin/api/groups/"+created.ID,
		strings.NewReader(`{"description":""}`))), created.ID)
	if rec2.Code != http.StatusOK {
		t.Fatalf("update: code = %d body = %s", rec2.Code, rec2.Body.String())
	}

	got, err := st.GetGroup(context.Background(), created.ID)
	if err != nil || got == nil {
		t.Fatalf("get group: %v", err)
	}
	if got.Description != "" {
		t.Errorf("description = %q, want 空串", got.Description)
	}
	if got.Name != "研发" {
		t.Errorf("name = %q, want 研发（不该被只改 description 的 PATCH 动到）", got.Name)
	}
}

// TestNetworkPatch_OnlyOneFieldKeepsOther 修 A7-10。
//
// 只改 name 时 proxy_url 必须原样留着。这里要同时守住两层：
// 一是 handler 不能把缺失字段判成非法（那就是 400），二是它不能把零值
// 写进去把另一列清掉（UpdateNetwork 是无条件覆盖两列）。
func TestNetworkPatch_OnlyOneFieldKeepsOther(t *testing.T) {
	st := newScopeStore(t)
	n := &store.Network{ID: "n1", Name: "直连机房", ProxyURL: "socks5://10.0.0.2:1080"}
	if err := st.CreateNetwork(context.Background(), n); err != nil {
		t.Fatalf("create network: %v", err)
	}
	h := NewNetworkHandler(st)

	rec := httptest.NewRecorder()
	h.Update(rec, asAdmin(jsonRequest(http.MethodPatch, "/admin/api/networks/n1",
		strings.NewReader(`{"name":"机房B"}`))), "n1")
	if rec.Code != http.StatusOK {
		t.Fatalf("update: code = %d body = %s", rec.Code, rec.Body.String())
	}

	got, err := st.GetNetwork(context.Background(), "n1")
	if err != nil || got == nil {
		t.Fatalf("get network: %v", err)
	}
	if got.Name != "机房B" {
		t.Errorf("name = %q, want 机房B", got.Name)
	}
	if got.ProxyURL != "socks5://10.0.0.2:1080" {
		t.Errorf("proxy_url = %q, want socks5://10.0.0.2:1080 —— 只改 name 的 PATCH 把代理清空了", got.ProxyURL)
	}

	// 反方向：只改 proxy_url，name 必须留着。
	rec2 := httptest.NewRecorder()
	h.Update(rec2, asAdmin(jsonRequest(http.MethodPatch, "/admin/api/networks/n1",
		strings.NewReader(`{"proxy_url":"http://127.0.0.1:7890"}`))), "n1")
	if rec2.Code != http.StatusOK {
		t.Fatalf("update2: code = %d body = %s", rec2.Code, rec2.Body.String())
	}
	got2, err := st.GetNetwork(context.Background(), "n1")
	if err != nil || got2 == nil {
		t.Fatalf("get network2: %v", err)
	}
	if got2.Name != "机房B" {
		t.Errorf("name = %q, want 机房B", got2.Name)
	}
	if got2.ProxyURL != "http://127.0.0.1:7890" {
		t.Errorf("proxy_url = %q, want http://127.0.0.1:7890", got2.ProxyURL)
	}
}

// TestNetworkPatch_NoFieldsRejected 守的是「空 PATCH 不算成功」。
// 两个字段都没提供时返回 400：静默 200 会让调用方以为改成功了，
// 而实际什么都没发生。
func TestNetworkPatch_NoFieldsRejected(t *testing.T) {
	st := newScopeStore(t)
	if err := st.CreateNetwork(context.Background(),
		&store.Network{ID: "n1", Name: "机房", ProxyURL: "socks5://127.0.0.1:1080"}); err != nil {
		t.Fatalf("create network: %v", err)
	}
	h := NewNetworkHandler(st)

	rec := httptest.NewRecorder()
	h.Update(rec, asAdmin(jsonRequest(http.MethodPatch, "/admin/api/networks/n1",
		strings.NewReader(`{}`))), "n1")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code = %d, want 400 —— 空 PATCH 不该被当成成功", rec.Code)
	}
}

// 空 PATCH（两个字段都没提供）与显式空值必须能区分开 —— 这正是
// 本文件三个用例共同守的那个分界。
var _ = http.StatusBadRequest
