package admin

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cn-maul/rosetta-gateway/internal/store"
)

// credEnabledMap 列出该 provider 下每把凭据的启用态（id → enabled）。
func credEnabledMap(t *testing.T, st *store.Store, providerID string) map[string]bool {
	t.Helper()
	creds, err := st.ListCredentials(t.Context(), providerID)
	if err != nil {
		t.Fatalf("list credentials: %v", err)
	}
	out := make(map[string]bool, len(creds))
	for _, c := range creds {
		out[c.ID] = c.Enabled
	}
	return out
}

// createCred 走真实 HTTP 入口建一把凭据，返回响应体。
func createCred(t *testing.T, h *CredentialHandler, providerID, body string) credentialResponse {
	t.Helper()
	rec := httptest.NewRecorder()
	req := asAdmin(jsonRequest(http.MethodPost, "/admin/api/providers/"+providerID+"/credentials",
		bytes.NewBufferString(body)))
	h.Create(rec, req, providerID)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create code=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp credentialResponse
	decodeBody(t, rec, &resp)
	return resp
}

// patchCred 走真实 HTTP 入口 PATCH 一把凭据。
func patchCred(t *testing.T, h *CredentialHandler, id, body string) credentialResponse {
	t.Helper()
	rec := httptest.NewRecorder()
	req := asAdmin(jsonRequest(http.MethodPatch, "/admin/api/credentials/"+id,
		bytes.NewBufferString(body)))
	h.Update(rec, req, id)
	if rec.Code != http.StatusOK {
		t.Fatalf("patch code=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp credentialResponse
	decodeBody(t, rec, &resp)
	return resp
}

// TestCredentialHandler_FirstCredentialAutoEnabled 第一把 key 必须自动可用：
// 「新建上游 + 填一个 key」是配网的常规动作，若默认停用，用户还得
// 回列表点一次开关才能开始跑 —— 而那一步没有任何提示为什么不能用。
func TestCredentialHandler_FirstCredentialAutoEnabled(t *testing.T) {
	st := newTestStore(t)
	h := NewCredentialHandler(st, testCredentialKey)

	got := createCred(t, h, "p1", `{"label":"主号","api_key":"sk-1"}`)
	if !got.Enabled {
		t.Fatalf("第一把凭据应自动启用: %+v", got)
	}
}

// TestCredentialHandler_SecondCredentialDefaultsDisabled 覆盖「加一把备用 key」
// 不得顺手把正在跑的流量切走。
//
// 这条与上一条构成一对：默认值不是恒定常量，而是「当前一把都没有才启用」。
// 只测其中一条的话，另一条恒会漏掉。
func TestCredentialHandler_SecondCredentialDefaultsDisabled(t *testing.T) {
	st := newTestStore(t)
	h := NewCredentialHandler(st, testCredentialKey)

	first := createCred(t, h, "p1", `{"label":"主号","api_key":"sk-1"}`)
	second := createCred(t, h, "p1", `{"label":"备用","api_key":"sk-2"}`)

	if second.Enabled {
		t.Fatalf("第二把凭据应默认停用: %+v", second)
	}
	if !enabledIDsOf(st, t, "p1")[first.ID] {
		t.Fatal("新建第二把把第一把也带停了")
	}
}

// enabledIDsOf 是 credEnabledMap 的别名式包装，保持测试可读。
func enabledIDsOf(st *store.Store, t *testing.T, providerID string) map[string]bool {
	t.Helper()
	return credEnabledMap(t, st, providerID)
}

// TestCredentialHandler_EnableSwitchesOthersOff 是本文件的主断言：
// 「启用 B」是一次**切换** —— A 必须同时停用，最终恰好一把启用。
func TestCredentialHandler_EnableSwitchesOthersOff(t *testing.T) {
	st := newTestStore(t)
	h := NewCredentialHandler(st, testCredentialKey)

	a := createCred(t, h, "p1", `{"label":"A","api_key":"sk-a"}`)
	b := createCred(t, h, "p1", `{"label":"B","api_key":"sk-b"}`)

	resp := patchCred(t, h, b.ID, `{"enabled":true}`)
	if !resp.Enabled {
		t.Fatalf("响应应报告 B 已启用: %+v", resp)
	}

	states := enabledIDsOf(st, t, "p1")
	if !states[b.ID] {
		t.Fatalf("B 未启用: %v", states)
	}
	if states[a.ID] {
		t.Fatalf("A 应随切换一并停用: %v", states)
	}
	on := 0
	for _, v := range states {
		if v {
			on++
		}
	}
	if on != 1 {
		t.Fatalf("期望恰好一把启用，实际 %d 把: %v", on, states)
	}
}

// TestCredentialHandler_DisableAffectsOnlyItself 停用不跨行：关掉 B 不能顺带
// 影响 A（或其它 provider 的凭据）。
func TestCredentialHandler_DisableAffectsOnlyItself(t *testing.T) {
	st := newTestStore(t)
	h := NewCredentialHandler(st, testCredentialKey)

	a := createCred(t, h, "p1", `{"label":"A","api_key":"sk-a"}`)
	b := createCred(t, h, "p1", `{"label":"B","api_key":"sk-b"}`)
	patchCred(t, h, b.ID, `{"enabled":true}`) // 此刻启用的是 B

	patchCred(t, h, b.ID, `{"enabled":false}`)

	states := enabledIDsOf(st, t, "p1")
	if states[b.ID] {
		t.Fatalf("B 应已停用: %v", states)
	}
	// A 保持它自己原来的状态（停用），而不是被这次操作改写
	if states[a.ID] {
		t.Fatalf("停用 B 不该启用 A: %v", states)
	}
}

// TestCredentialHandler_LabelPatchKeepsEnabled 改标签不能顺带改启用态。
//
// 这是 PATCH 语义最容易破的地方：enabled 字段缺席 = 「保持原值」。
// 若实现里把它写成「没传就当成 false」，用户改个备注就会把正在用的
// key 悄悄停掉 —— 而界面上那把 key 的徽章是在下一次整表重载后才翻转的，
// 中间那段时间请求全打不到上游。
func TestCredentialHandler_LabelPatchKeepsEnabled(t *testing.T) {
	st := newTestStore(t)
	h := NewCredentialHandler(st, testCredentialKey)

	a := createCred(t, h, "p1", `{"label":"A","api_key":"sk-a"}`)
	resp := patchCred(t, h, a.ID, `{"label":"A 改名"}`)

	if !resp.Enabled {
		t.Fatalf("只改标签不应改变启用态: %+v", resp)
	}
	if !enabledIDsOf(st, t, "p1")[a.ID] {
		t.Fatal("只改标签把凭据停用了")
	}
}

// TestCredentialHandler_EnableOnOtherProviderIsolation 覆盖跨 provider：
// 在 p2 上启用一把，绝不能动 p1 的启用态。
//
// 这是「同时只启用一把」的正确作用域 —— 约束是**每个上游各一把**，
// 不是全站只有一把。若实现误把条件写成全局的，p2 的切换会把 p1 的
// 正在使用的 key 停掉，表现为另一个上游毫无征兆地不可用。
func TestCredentialHandler_EnableOnOtherProviderIsolation(t *testing.T) {
	st := newTestStore(t)
	h := NewCredentialHandler(st, testCredentialKey)

	p1a := createCred(t, h, "p1", `{"label":"A","api_key":"sk-a"}`)
	p2a := createCred(t, h, "p2", `{"label":"P2A","api_key":"sk-p2a"}`)
	p2b := createCred(t, h, "p2", `{"label":"P2B","api_key":"sk-p2b"}`)

	patchCred(t, h, p2b.ID, `{"enabled":true}`)

	if !enabledIDsOf(st, t, "p1")[p1a.ID] {
		t.Fatal("在 p2 上切换凭据把 p1 的凭据也停用了")
	}
	if enabledIDsOf(st, t, "p2")[p2a.ID] {
		t.Fatal("p2a 应随切换停用")
	}
	if !enabledIDsOf(st, t, "p2")[p2b.ID] {
		t.Fatal("p2b 应启用")
	}
}

// TestCredentialHandler_ExplicitEnabledTrueOnCreate 覆盖显式传 true 的创建：
// 必须同样走「切换」语义（关掉原有那把），而不是插出第二把启用的。
func TestCredentialHandler_ExplicitEnabledTrueOnCreate(t *testing.T) {
	st := newTestStore(t)
	h := NewCredentialHandler(st, testCredentialKey)

	first := createCred(t, h, "p1", `{"label":"A","api_key":"sk-a"}`)
	second := createCred(t, h, "p1", `{"label":"B","api_key":"sk-b","enabled":true}`)

	if !second.Enabled {
		t.Fatalf("显式 enabled=true 应生效: %+v", second)
	}
	states := enabledIDsOf(st, t, "p1")
	if states[first.ID] {
		t.Fatalf("显式启用新凭据后，原有那把应停用: %v", states)
	}
}

// TestCredentialHandler_UpdateKeepsOtherFields 与 EnabledExclusive 组合时的
// 回归护栏：一次「启用 + 改标签」的 PATCH 必须两个字段都写进去。
//
// 实现上启用被拆成了 UpdateCredential + EnableExclusiveCredential 两步，
// 这类拆分最容易出的错是「第一步没带上另一个字段」，而症状是静默丢失。
func TestCredentialHandler_UpdateKeepsOtherFields(t *testing.T) {
	st := newTestStore(t)
	h := NewCredentialHandler(st, testCredentialKey)

	createCred(t, h, "p1", `{"label":"A","api_key":"sk-a"}`)
	b := createCred(t, h, "p1", `{"label":"B","api_key":"sk-b"}`)

	resp := patchCred(t, h, b.ID, `{"enabled":true,"label":"B-最终"}`)
	if !resp.Enabled {
		t.Fatalf("启用未生效: %+v", resp)
	}
	if resp.Label != "B-最终" {
		t.Fatalf("同一次 PATCH 里的 label 丢失: %q", resp.Label)
	}
}

// TestCredentialHandler_MissingCredential 启停一个已不存在的 id 必须 404，
// 不能报 200 —— 那会让界面以为切换成功了。
func TestCredentialHandler_MissingCredential(t *testing.T) {
	st := newTestStore(t)
	h := NewCredentialHandler(st, testCredentialKey)

	rec := httptest.NewRecorder()
	req := asAdmin(jsonRequest(http.MethodPatch, "/admin/api/credentials/nope",
		bytes.NewBufferString(`{"enabled":true}`)))
	h.Update(rec, req, "nope")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("期望 404，实际 %d body=%s", rec.Code, rec.Body.String())
	}
}
