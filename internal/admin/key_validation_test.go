package admin

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cn-maul/rosetta-gateway/internal/config"
	"github.com/cn-maul/rosetta-gateway/internal/routing"
	"github.com/cn-maul/rosetta-gateway/internal/snapshot"
	"github.com/cn-maul/rosetta-gateway/internal/store"
)

// ---- F4：无归属 key 的「先建后认领」必须真的存在 ----

// TestKeyHandler_ClaimOwnerOnPatch 钉住 F4 的修复。
//
// Update 过去**从不读** req.UserID（keyRequest 里声明了它），于是
// Create 注释里承诺的「先建后认领」流程不存在：
//
//	POST  {"name":"k"}                      -> 201 user_id:""（那把 key 永远 401）
//	PATCH {"user_id":"<真实用户>"}            -> 200，但 user_id 仍是 ""
//
// 无归属的 key 走 auth.Authenticate 会得到 ErrKeyUnowned（401），
// 所以认领不上就等于「建出一把谁都用不了的死物」。
func TestKeyHandler_ClaimOwnerOnPatch(t *testing.T) {
	st := newTestStore(t)
	h := NewKeyHandler(st)
	ctx := t.Context()

	if err := st.CreateUser(ctx, &store.User{
		ID: "u-claim", Username: "claimable", PasswordHash: "x",
		Role: store.RoleUser, Status: store.UserStatusActive, AuthVersion: 1,
	}); err != nil {
		t.Fatalf("seed user: %v", err)
	}

	rec := httptest.NewRecorder()
	h.Create(rec, jsonRequest(http.MethodPost, "/admin/api/keys",
		bytes.NewBufferString(`{"name":"orphan"}`)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create code=%d body=%s", rec.Code, rec.Body.String())
	}
	var created keyCreateResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if created.UserID != "" {
		t.Fatalf("前置条件不成立：期望无归属，实际 %q", created.UserID)
	}

	rec2 := httptest.NewRecorder()
	h.Update(rec2, asAdmin(jsonRequest(http.MethodPatch, "/admin/api/keys/"+created.ID,
		bytes.NewBufferString(`{"user_id":"u-claim"}`))), created.ID)
	if rec2.Code != http.StatusOK {
		t.Fatalf("claim code=%d body=%s", rec2.Code, rec2.Body.String())
	}
	var upd keyResponse
	if err := json.Unmarshal(rec2.Body.Bytes(), &upd); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if upd.UserID != "u-claim" {
		t.Fatalf("认领未生效：user_id=%q（修复前这里恒为空串，且仍返回 200）", upd.UserID)
	}
	// 落库也必须同步 —— 否则下一次重建快照时又变回无归属。
	dbKey, _ := st.GetAccessKey(ctx, created.ID)
	if dbKey == nil || dbKey.UserID != "u-claim" {
		t.Fatalf("归属未落库：%+v", dbKey)
	}

	// 指向不存在的用户 → 400（否则又造出一把死 key）
	rec3 := httptest.NewRecorder()
	h.Update(rec3, asAdmin(jsonRequest(http.MethodPatch, "/admin/api/keys/"+created.ID,
		bytes.NewBufferString(`{"user_id":"does-not-exist"}`))), created.ID)
	if rec3.Code != http.StatusBadRequest {
		t.Fatalf("认领到不存在的用户应 400，实际 %d body=%s", rec3.Code, rec3.Body.String())
	}
}

// ---- F9：空白模型条目不能静默变成「不限制」----

// snapshotWithRoute 给 unknownModels 准备一个已知的公开模型名。
func snapshotWithRoute(t *testing.T, publicName string) {
	t.Helper()
	snap := &snapshot.Snapshot{
		Routes:     routing.NewRouteIndex(),
		Providers:  map[string]*snapshot.ProviderSnapshot{},
		KeysByHash: map[string]*snapshot.KeySnapshot{},
		UsersByID:  map[string]*snapshot.UserSnapshot{},
	}
	ri := routing.NewRouteIndex()
	ri.AddProvider(&routing.ProviderRef{ID: "p1", Slug: "p1", Protocol: "openai-chat", Enabled: true})
	ri.AddUpstreamModel(&routing.UpstreamModel{ID: "m1", ProviderID: "p1", ModelID: "m1", Enabled: true})
	ri.AddRoute(&routing.Route{ID: "r1", PublicName: publicName, ProviderID: "p1", UpstreamModelID: "m1", Enabled: true})
	snap.Routes = ri
	snapshot.Init(snap)
	t.Cleanup(func() {
		snapshot.Init(&snapshot.Snapshot{
			Routes:     routing.NewRouteIndex(),
			Providers:  map[string]*snapshot.ProviderSnapshot{},
			KeysByHash: map[string]*snapshot.KeySnapshot{},
			UsersByID:  map[string]*snapshot.UserSnapshot{},
		})
	})
}

// TestKeyHandler_BlankModelEntryRejected 钉住 F9 的修复。
//
// 修复前 unknownModels 对 trim 后为空白的条目 continue，于是
// {"allowed_models":["  "]} 校验通过 → 落库归一成空 → 写 NULL →
// 读回 nil → keyModelAllow(nil)=AllowAll()：**一把不限制的 key**。
// 本意「收紧」的结果是「完全放开」，且界面无任何提示。
func TestKeyHandler_BlankModelEntryRejected(t *testing.T) {
	st := newTestStore(t)
	h := NewKeyHandler(st)
	snapshotWithRoute(t, "flash")

	for _, body := range []string{
		`{"name":"k","allowed_models":["  "]}`,
		`{"name":"k","allowed_models":[""]}`,
		`{"name":"k","allowed_models":["flash"," "]}`, // 混一个有效名也一样：白名单会被静默改写
	} {
		rec := httptest.NewRecorder()
		h.Create(rec, jsonRequest(http.MethodPost, "/admin/api/keys", bytes.NewBufferString(body)))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s -> %d，期望 400（空白条目会静默变成「不限制」）；body=%s",
				body, rec.Code, rec.Body.String())
		}
	}

	// 清空白名单仍然只能用显式的 []。
	rec := httptest.NewRecorder()
	h.Create(rec, jsonRequest(http.MethodPost, "/admin/api/keys",
		bytes.NewBufferString(`{"name":"k","allowed_models":[]}`)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("空数组应 201（显式「不限制」），实际 %d body=%s", rec.Code, rec.Body.String())
	}
}

// TestKeyHandler_AllowedModelsResponseMatchesStore 钉住 F9 的另一半：
// 响应的回显必须与落库后的读取结果一致。
//
// 修复前 allowedModelsForResponse 直接返回请求原文，于是
// POST ["  "," pub-chat ","pub-chat"] 立刻回显三个条目（界面「限 3 个模型」），
// 而 600ms 后 GET 读回来是 ["pub-chat"]（限 1 个）。
func TestKeyHandler_AllowedModelsResponseMatchesStore(t *testing.T) {
	st := newTestStore(t)
	h := NewKeyHandler(st)
	snapshotWithRoute(t, "flash")

	rec := httptest.NewRecorder()
	h.Create(rec, jsonRequest(http.MethodPost, "/admin/api/keys",
		bytes.NewBufferString(`{"name":"k","allowed_models":[" flash "," flash "]} `)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create code=%d body=%s", rec.Code, rec.Body.String())
	}
	var created keyCreateResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(created.AllowedModels) != 1 || created.AllowedModels[0] != "flash" {
		t.Fatalf("回显未归一：%v（应去空白、去重成 [\"flash\"]）", created.AllowedModels)
	}
	// 从库里读回来必须与响应一致
	dbKey, _ := st.GetAccessKey(t.Context(), created.ID)
	if dbKey == nil {
		t.Fatal("key not found")
	}
	got := allowedModelsForResponse(dbKey.AllowedModels)
	if len(got) != 1 || got[0] != "flash" {
		t.Fatalf("落库后读取不一致：%v", got)
	}
}

// ---- F11：allowed_ips 存规范原文 ----

// TestKeyHandler_AllowedIPsNormalized 钉住 F11 的修复。
//
// ParseAllowedNets 逐段 trim、丢空段（"10.0.0.0/8," 与 "10.0.0.0/8"
// 语义完全相同），但修复前存的是**请求原文**，于是回显与实际生效的规则
// 长得不一样，管理员无法分辨自己是不是写了个边缘输入。
func TestKeyHandler_AllowedIPsNormalized(t *testing.T) {
	st := newTestStore(t)
	h := NewKeyHandler(st)

	cases := map[string]string{
		`{"name":"a","allowed_ips":"127.0.0.0/8,"}`:               "127.0.0.0/8",
		`{"name":"b","allowed_ips":" 127.0.0.0 "}`:                "127.0.0.0/32",
		`{"name":"c","allowed_ips":"  "}`:                         "",
		`{"name":"d","allowed_ips":"10.0.0.0/8,,192.168.0.0/16"}`: "10.0.0.0/8,192.168.0.0/16",
	}
	for body, want := range cases {
		rec := httptest.NewRecorder()
		h.Create(rec, jsonRequest(http.MethodPost, "/admin/api/keys", bytes.NewBufferString(body)))
		if rec.Code != http.StatusCreated {
			t.Fatalf("%s -> %d body=%s", body, rec.Code, rec.Body.String())
		}
		var got keyResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if got.AllowedIPs != want {
			t.Errorf("%s -> allowed_ips=%q，期望规范形式 %q", body, got.AllowedIPs, want)
		}
	}
	// 坏值仍然 400
	rec := httptest.NewRecorder()
	h.Create(rec, jsonRequest(http.MethodPost, "/admin/api/keys",
		bytes.NewBufferString(`{"name":"e","allowed_ips":"127.0.0.0/8,hello"}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("不可解析的 CIDR 应 400，实际 %d body=%s", rec.Code, rec.Body.String())
	}
}

// ---- F13：provider timeout_ms 需要上界 ----

// TestProviderHandler_TimeoutMsUpperBound 钉住 F13 的修复。
//
// 修复前 PATCH 只拒负数，实测 {"timeout_ms":9223372036854} 照单全收（200），
// 而 internal/upstream 里的 `time.Duration(ms) * time.Millisecond`
// 在 ms > 9.223e12 时回绕成负数 —— 负 Duration 让 context.WithTimeout
// 立即过期、time.AfterFunc 立即开火。
func TestProviderHandler_TimeoutMsUpperBound(t *testing.T) {
	st := newTestStore(t)
	ctx := t.Context()
	if err := st.CreateProvider(ctx, &store.Provider{
		ID: "prov-timeout", Slug: "prov-timeout", Name: "prov-timeout", Protocol: "openai-chat",
		Endpoint: "https://example.invalid/v1", Enabled: true,
	}); err != nil {
		t.Fatalf("seed provider: %v", err)
	}
	h := NewProviderHandler(st, nil, config.Default())

	for _, body := range []string{
		`{"timeout_ms":9223372036854}`,
		`{"timeout_ms":9223372036855}`,
		`{"max_retries":-1}`,
	} {
		rec := httptest.NewRecorder()
		h.Update(rec, asAdmin(jsonRequest(http.MethodPatch, "/admin/api/providers/prov-timeout",
			bytes.NewBufferString(body))), "prov-timeout")
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s -> %d，期望 400（会回绕成负 Duration）；body=%s",
				body, rec.Code, rec.Body.String())
		}
	}

	// 边界内的值仍然放行
	rec := httptest.NewRecorder()
	h.Update(rec, asAdmin(jsonRequest(http.MethodPatch, "/admin/api/providers/prov-timeout",
		bytes.NewBufferString(`{"timeout_ms":60000}`))), "prov-timeout")
	if rec.Code != http.StatusOK {
		t.Fatalf("合法 timeout 应 200，实际 %d body=%s", rec.Code, rec.Body.String())
	}
	got, _ := st.GetProvider(ctx, "prov-timeout")
	if got == nil || got.TimeoutMs != 60000 {
		t.Fatalf("timeout 未落库：%+v", got)
	}
	_ = config.MaxDurationMillis // 上界常量与 config/settings 共用同一个
}
