package admin

// 2026-10-14 上线前审计加固（引导抢注窗口）的回归测试。
//
// # 问题
//
// 首次部署时 ensureBootstrapAdmin 建出一个密码为空的 admin，
// 而完成引导的 `POST /admin/api/bootstrap` 是**免鉴权**的。容器默认
// listen 0.0.0.0（否则 `-p 8666:8666` 映射不出去），于是「谁先连到端口，
// 谁就是管理员」。
//
// 同源判定挡不住这条路径：curl 不发 Origin，服务端无从分辨是运维还是
// 攻击者 —— 那不是浏览器发起的请求，没有 Origin 可校验。
//
// # 修法：一次性引导令牌
//
// 启动时（引导未完成的情况下）生成/接受一个随机令牌，只打进启动日志；
// 完成引导必须带上它。比对与「写密码」「置 completed 标记」「清令牌」
// 在**同一个事务**里，于是不存在「设过密但令牌还在」（可重放）也不存在
// 「令牌清了但密码没设上」（管理员被永久锁在门外）这两种中间态。
//
// 本文件钉住的关键不变量：
//   - 令牌不对 → 403，且**密码绝不能被设上**（窗口仍开着，可重试）；
//   - 令牌对 → 200，且令牌立刻作废（重放拿不到第二份会话）；
//   - 窗口已关 → 409（不是 403，也不是静默成功）；
//   - 未配置令牌的安装（老库 / 本地直跑）行为与修复前逐字一致；
//   - BootstrapStatus 只暴露「需不需要令牌」，**绝不**把令牌本身发出去。

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cn-maul/rosetta-gateway/internal/store"
	"github.com/cn-maul/rosetta-gateway/internal/userauth"
)

const testBootstrapToken = "b1a7f3c92d4e6081af25c7e93b0d4816fe2a95c8d730b14e6f9a2c5d8b0e3714"

// setupWithToken 造出一个「配了引导令牌」的待初始化安装。
func setupWithToken(t *testing.T) (*store.Store, *UserHandler, *store.User) {
	t.Helper()
	st := newScopeStore(t)
	if err := st.SetBootstrapToken(t.Context(), testBootstrapToken); err != nil {
		t.Fatalf("set bootstrap token: %v", err)
	}
	u := seedUninitializedAdmin(t, st)
	return st, NewUserHandler(st, newTestManager(t)), u
}

func postBootstrap(h *UserHandler, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.BootstrapSetup(rec, jsonRequest(http.MethodPost, "/admin/api/bootstrap",
		strings.NewReader(body)))
	return rec
}

// 核心用例：不带/带错令牌一律 403，且**密码绝不能被设上**。
//
// 「密码没被设上」这一条比 403 本身更重要：攻击者若能靠猜令牌抢到 admin，
// 状态码是 403 还是 200 都无关紧要了。而窗口必须仍开着 ——
// 运维拿正确令牌还能完成引导。
func TestBootstrapSetup_WrongTokenRejectedAndPasswordUntouched(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"完全不带令牌", `{"password":"Attacker#2026pw"}`},
		{"空令牌", `{"password":"Attacker#2026pw","token":""}`},
		{"错令牌", `{"password":"Attacker#2026pw","token":"deadbeef"}`},
		// 只对一半：前缀相同、后缀不同。
		{"令牌前缀正确", `{"password":"Attacker#2026pw","token":"` + testBootstrapToken[:32] + `"}`},
		{"令牌大小写不同", `{"password":"Attacker#2026pw","token":"` + strings.ToUpper(testBootstrapToken) + `"}`},
		{"多一个字符", `{"password":"Attacker#2026pw","token":"` + testBootstrapToken + `0"}`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st, h, u := setupWithToken(t)

			rec := postBootstrap(h, tc.body)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d body=%s, want 403", rec.Code, rec.Body.String())
			}
			if userauth.VerifyPassword(mustHash(t, st, u.ID), "Attacker#2026pw") {
				t.Fatal("攻击者的密码被设上了 —— 引导窗口被抢注")
			}
			if got := mustHash(t, st, u.ID); got != "" {
				t.Errorf("password_hash = %q, want 空（窗口仍开着）", got)
			}
			// 窗口必须仍然开着：运维拿正确令牌还能完成。
			if done, err := st.BootstrapCompleted(t.Context()); err != nil || done {
				t.Errorf("bootstrap_completed = %v (err %v), want false —— 窗口被误关了",
					done, err)
			}
			// 令牌也不能被消耗掉，否则运维永远完不成引导。
			if tok, _ := st.BootstrapToken(t.Context()); tok != testBootstrapToken {
				t.Errorf("token = %q, want 保持不变", tok)
			}
		})
	}
}

// 令牌正确 → 200，且**令牌立刻作废**。
//
// 作废是这套机制的收尾：令牌留在库里就是一个「谁拿到日志谁就能重放
// 抢注」的过期凭据。它必须与密码、completed 标记同事务清空。
func TestBootstrapSetup_CorrectTokenSucceedsAndConsumesToken(t *testing.T) {
	st, h, u := setupWithToken(t)

	rec := postBootstrap(h, `{"password":"Legit#2026pw","token":"`+testBootstrapToken+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s, want 200", rec.Code, rec.Body.String())
	}
	var got loginResponse
	decodeBody(t, rec, &got)
	if got.Token == "" {
		t.Fatal("no session issued; setup must log the user in directly")
	}
	if !userauth.VerifyPassword(mustHash(t, st, u.ID), "Legit#2026pw") {
		t.Fatal("password was not actually set")
	}
	if done, err := st.BootstrapCompleted(t.Context()); err != nil || !done {
		t.Errorf("bootstrap_completed = %v (err %v), want true", done, err)
	}
	if tok, _ := st.BootstrapToken(t.Context()); tok != "" {
		t.Errorf("token = %q, want 空（已随设密消费）", tok)
	}
}

// 重放：令牌用掉之后再用同一个令牌设密码，必须失败且不改密码。
func TestBootstrapSetup_TokenIsNotReplayable(t *testing.T) {
	st, h, _ := setupWithToken(t)

	if rec := postBootstrap(h, `{"password":"Legit#2026pw","token":"`+
		testBootstrapToken+`"}`); rec.Code != http.StatusOK {
		t.Fatalf("first setup = %d body=%s", rec.Code, rec.Body.String())
	}
	rec := postBootstrap(h, `{"password":"Replay#2026pw","token":"`+testBootstrapToken+`"}`)
	// 窗口已关，走的是「重复初始化」那条 409 —— 不管走哪条，
	// 关键是密码没被改成第二个。
	if rec.Code == http.StatusOK {
		t.Fatal("token was replayable after being consumed")
	}
	if userauth.VerifyPassword(mustHash(t, st, "boot-admin"), "Replay#2026pw") {
		t.Fatal("重放的密码被设上了")
	}
}

// 未配置令牌的安装（老库 / 本地直跑）行为与修复前**逐字一致**：
// 不带 token 也能完成引导。
//
// 这条兼容路径必须留着：库里有 app_settings 表但没有 bootstrap_token
// 记录的，就是「本次升级之前建的库」。给它们凭空要求一个没人知道的
// 令牌，等于把一批存量部署的管理员永久锁在门外。
func TestBootstrapSetup_LegacyInstallWithoutTokenUnchanged(t *testing.T) {
	st := newScopeStore(t)
	h := NewUserHandler(st, newTestManager(t))
	u := seedUninitializedAdmin(t, st)

	rec := postBootstrap(h, `{"password":"Legacy#2026pw"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("legacy setup rejected: %d body=%s", rec.Code, rec.Body.String())
	}
	if !userauth.VerifyPassword(mustHash(t, st, u.ID), "Legacy#2026pw") {
		t.Fatal("legacy install could not complete bootstrap")
	}
}

// BootstrapStatus 只说「需不需要令牌」，**绝不**把令牌本身发出去。
//
// 这个端点免鉴权：把令牌放进响应等于取消令牌（谁都能拿到）。
// 断言响应体里不出现令牌串 —— 这是防「顺手加个字段方便前端」的哨兵。
func TestBootstrapStatus_ReportsRequirementButNeverTheToken(t *testing.T) {
	_, h, _ := setupWithToken(t)

	rec := httptest.NewRecorder()
	h.BootstrapStatus(rec, jsonRequest(http.MethodGet, "/admin/api/bootstrap", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var got bootstrapStatusResponse
	decodeBody(t, rec, &got)
	if !got.NeedsSetup {
		t.Error("needs_setup = false")
	}
	if !got.RequiresToken {
		t.Error("requires_token = false —— 前端不会显示令牌输入框，运维拿正确令牌也提交不了")
	}
	if strings.Contains(rec.Body.String(), testBootstrapToken) {
		t.Fatal("引导令牌出现在免鉴权响应里 —— 这个令牌等于没设")
	}
}

// 没有配置令牌时 requires_token 必须为 false（老库不该被多问一句）。
func TestBootstrapStatus_NoTokenConfiguredMeansNoRequirement(t *testing.T) {
	st := newScopeStore(t)
	h := NewUserHandler(st, newTestManager(t))
	seedUninitializedAdmin(t, st)

	rec := httptest.NewRecorder()
	h.BootstrapStatus(rec, jsonRequest(http.MethodGet, "/admin/api/bootstrap", nil))
	var got bootstrapStatusResponse
	decodeBody(t, rec, &got)
	if got.RequiresToken {
		t.Error("requires_token = true on an install with no token")
	}
}

// store 层：哨兵错误必须把「令牌不匹配」与「窗口已关」区分开。
//
// 合成一个 bool 之后两者返回同一句话，运维分不清「窗口还开着、换个令牌
// 还能继续」与「怎么重试都没用」—— 处置完全相反。
func TestSetInitialAdminPasswordWithToken_DistinguishesMismatchFromClosed(t *testing.T) {
	st := newScopeStore(t)
	if err := st.SetBootstrapToken(t.Context(), testBootstrapToken); err != nil {
		t.Fatalf("set token: %v", err)
	}
	u := seedUninitializedAdmin(t, st)

	// 令牌不对 → 哨兵错误（HTTP 层据此回 403）。
	ok, err := st.SetInitialAdminPasswordWithToken(t.Context(), u.ID, "hash-x", "wrong")
	if !errors.Is(err, store.ErrBootstrapTokenMismatch) {
		t.Errorf("wrong token: err = %v, want ErrBootstrapTokenMismatch", err)
	}
	if ok {
		t.Error("wrong token reported success")
	}

	// 令牌对 → 写入成功。
	ok, err = st.SetInitialAdminPasswordWithToken(t.Context(), u.ID, "hash-ok", testBootstrapToken)
	if err != nil || !ok {
		t.Fatalf("correct token: ok=%v err=%v", ok, err)
	}

	// 窗口已关（已设过密码）→ (false, nil)，**不是**哨兵错误。
	ok, err = st.SetInitialAdminPasswordWithToken(t.Context(), u.ID, "hash-y", testBootstrapToken)
	if ok || err != nil {
		t.Errorf("closed window: ok=%v err=%v, want false/nil", ok, err)
	}
}

// store 层：令牌已被消费但引导未完成（写密码与置标记之间被打断，
// 事务已回滚）—— 那一态下**绝不能**放行无凭据的引导。
//
// 手工构造：写一个空的 bootstrap_token 记录，但不置 completed。
// 若放行，攻击者就能在运维以为「令牌还在」的时候无声无息完成引导。
func TestSetInitialAdminPasswordWithToken_ConsumedTokenDoesNotOpenWindow(t *testing.T) {
	st := newScopeStore(t)
	// 令牌被消费后的残留：key 在、值为空。
	if err := st.SetBootstrapToken(t.Context(), ""); err != nil {
		t.Fatalf("seed empty token: %v", err)
	}
	u := seedUninitializedAdmin(t, st)

	ok, err := st.SetInitialAdminPasswordWithToken(t.Context(), u.ID, "hash-x", "")
	if !errors.Is(err, store.ErrBootstrapTokenMismatch) {
		t.Errorf("err = %v, want ErrBootstrapTokenMismatch（空令牌残留不得放行）", err)
	}
	if ok {
		t.Error("空令牌残留状态放行了无凭据引导")
	}
	if got := mustHash(t, st, u.ID); got != "" {
		t.Errorf("password_hash = %q, want 空", got)
	}
}
