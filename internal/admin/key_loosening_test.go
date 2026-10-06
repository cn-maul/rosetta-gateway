package admin

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cn-maul/rosetta-gateway/internal/store"
)

// 「只能收紧」守卫的测试：管理员对 key 施加的强制措施（禁用/配额/限速/
// 有效期/IP 白名单）不能被归属者用 PATCH 撤销。每条规则同时验证
// 「放宽被拒」与「收紧放行」两个方向，防止守卫退化成一刀切。

func newLooseningStore(t *testing.T) (*store.Store, *KeyHandler) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "gw.db"),
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()
	if err := st.CreateUser(ctx, &store.User{
		ID: "u1", Username: "alice", PasswordHash: "x",
		Role: store.RoleUser, Status: store.UserStatusActive, AuthVersion: 1,
	}); err != nil {
		t.Fatalf("create user: %v", err)
	}
	h := NewKeyHandler(st)
	return st, h
}

// seedRestrictedKey 造一把被管理员全面管束的 key：禁用、配额/限速收紧、
// 设了有效期与 IP 白名单。
func seedRestrictedKey(t *testing.T, st *store.Store) *store.AccessKey {
	t.Helper()
	k := &store.AccessKey{
		ID: "kr", KeyHash: "h-kr", KeyPrefix: "sk-", Name: "restricted",
		Enabled: false, QuotaTokens: 1000, UserID: "u1",
		RPMLimit: 10, TPMLimit: 1000,
		ExpiresAt:  time.Now().Add(48 * time.Hour).UnixMilli(),
		AllowedIPs: "10.0.0.0/8",
	}
	if err := st.CreateAccessKey(context.Background(), k); err != nil {
		t.Fatalf("seed key: %v", err)
	}
	return k
}

func patchKey(h *KeyHandler, as func(*http.Request) *http.Request, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := as(jsonRequest(http.MethodPatch, "/admin/api/keys/kr", strings.NewReader(body)))
	h.Update(rec, req, "kr")
	return rec
}

func TestKeyUpdate_OwnerCannotReenableDisabledKey(t *testing.T) {
	st, h := newLooseningStore(t)
	seedRestrictedKey(t, st)

	rec := patchKey(h, func(r *http.Request) *http.Request { return asUser(r, "u1") }, `{"enabled":true}`)
	if rec.Code != http.StatusForbidden {
		t.Errorf("re-enable disabled key: code=%d body=%s, want 403", rec.Code, rec.Body.String())
	}
	if k, _ := st.GetAccessKey(context.Background(), "kr"); k.Enabled {
		t.Errorf("disabled key was re-enabled: %+v", k)
	}
}

func TestKeyUpdate_OwnerCanSelfDisable(t *testing.T) {
	st, h := newLooseningStore(t)
	seedRestrictedKey(t, st)
	if err := st.UpdateAccessKey(context.Background(), "kr", &store.AccessKey{
		ID: "kr", KeyHash: "h-kr", KeyPrefix: "sk-", Name: "restricted",
		Enabled: true, UserID: "u1",
	}); err != nil {
		t.Fatalf("enable key: %v", err)
	}

	rec := patchKey(h, func(r *http.Request) *http.Request { return asUser(r, "u1") }, `{"enabled":false}`)
	if rec.Code != http.StatusOK {
		t.Errorf("self-disable: code=%d body=%s, want 200", rec.Code, rec.Body.String())
	}
}

func TestKeyUpdate_OwnerCannotLoosenQuotaAndLimits(t *testing.T) {
	st, h := newLooseningStore(t)
	seedRestrictedKey(t, st)
	asU := func(r *http.Request) *http.Request { return asUser(r, "u1") }

	for name, body := range map[string]string{
		"raise quota": `{"quota_tokens":2000}`,
		"clear quota": `{"quota_tokens":0}`,
		"raise rpm":   `{"rpm_limit":100}`,
		"clear rpm":   `{"rpm_limit":0}`,
		"raise tpm":   `{"tpm_limit":5000}`,
		"clear tpm":   `{"tpm_limit":0}`,
	} {
		rec := patchKey(h, asU, body)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s: code=%d body=%s, want 403", name, rec.Code, rec.Body.String())
		}
	}
	// 收紧方向必须放行。
	rec := patchKey(h, asU, `{"quota_tokens":500,"rpm_limit":5,"tpm_limit":500}`)
	if rec.Code != http.StatusOK {
		t.Errorf("tighten limits: code=%d body=%s, want 200", rec.Code, rec.Body.String())
	}
	k, _ := st.GetAccessKey(context.Background(), "kr")
	if k.QuotaTokens != 500 || k.RPMLimit != 5 || k.TPMLimit != 500 {
		t.Errorf("tightened values not applied: %+v", k)
	}
}

func TestKeyUpdate_OwnerCanSetLimitsOnUnrestrictedKey(t *testing.T) {
	st, h := newLooseningStore(t)
	if err := st.CreateAccessKey(context.Background(), &store.AccessKey{
		ID: "kfree", KeyHash: "h-free", KeyPrefix: "sk-", Name: "free", Enabled: true, UserID: "u1",
	}); err != nil {
		t.Fatalf("seed key: %v", err)
	}

	// 现值 0（不限）：给自己加限制是收紧，放行。
	rec := httptest.NewRecorder()
	req := asUser(jsonRequest(http.MethodPatch, "/admin/api/keys/kfree",
		strings.NewReader(`{"quota_tokens":100,"rpm_limit":10,"tpm_limit":1000}`)), "u1")
	h.Update(rec, req, "kfree")
	if rec.Code != http.StatusOK {
		t.Errorf("self-tighten unlimited key: code=%d body=%s, want 200", rec.Code, rec.Body.String())
	}
}

func TestKeyUpdate_OwnerCannotExtendOrClearExpiry(t *testing.T) {
	st, h := newLooseningStore(t)
	seedRestrictedKey(t, st)
	asU := func(r *http.Request) *http.Request { return asUser(r, "u1") }

	extend := time.Now().Add(240 * time.Hour).UnixMilli()
	rec := patchKey(h, asU, `{"expires_at":`+strconv.FormatInt(extend, 10)+`}`)
	if rec.Code != http.StatusForbidden {
		t.Errorf("extend expiry: code=%d body=%s, want 403", rec.Code, rec.Body.String())
	}
	rec = patchKey(h, asU, `{"expires_at":0}`)
	if rec.Code != http.StatusForbidden {
		t.Errorf("clear expiry (revive forever): code=%d body=%s, want 403", rec.Code, rec.Body.String())
	}

	shorten := time.Now().Add(1 * time.Hour).UnixMilli()
	rec = patchKey(h, asU, `{"expires_at":`+strconv.FormatInt(shorten, 10)+`}`)
	if rec.Code != http.StatusOK {
		t.Errorf("shorten expiry: code=%d body=%s, want 200", rec.Code, rec.Body.String())
	}
}

func TestKeyUpdate_OwnerCannotWidenIPAllowlist(t *testing.T) {
	st, h := newLooseningStore(t)
	seedRestrictedKey(t, st)
	asU := func(r *http.Request) *http.Request { return asUser(r, "u1") }

	for name, body := range map[string]string{
		"clear allowlist": `{"allowed_ips":""}`,
		"widen to v4 all": `{"allowed_ips":"0.0.0.0/0"}`,
		"outside net":     `{"allowed_ips":"192.168.0.0/16"}`,
	} {
		rec := patchKey(h, asU, body)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s: code=%d body=%s, want 403", name, rec.Code, rec.Body.String())
		}
	}
	// 收紧：落在现有 /8 内的子网放行。
	rec := patchKey(h, asU, `{"allowed_ips":"10.1.2.0/24,10.3.0.0/16"}`)
	if rec.Code != http.StatusOK {
		t.Errorf("narrow allowlist: code=%d body=%s, want 200", rec.Code, rec.Body.String())
	}
}

func TestKeyUpdate_AdminBypassesLooseningGuard(t *testing.T) {
	st, h := newLooseningStore(t)
	seedRestrictedKey(t, st)

	// 管理员可以做全部「放宽」动作：重新启用、清配额、清限速、清有效期、清 IP。
	rec := patchKey(h, asAdmin, `{"enabled":true,"quota_tokens":0,"rpm_limit":0,"tpm_limit":0,"expires_at":0,"allowed_ips":""}`)
	if rec.Code != http.StatusOK {
		t.Errorf("admin full relax: code=%d body=%s, want 200", rec.Code, rec.Body.String())
	}
	k, _ := st.GetAccessKey(context.Background(), "kr")
	if !k.Enabled || k.QuotaTokens != 0 || k.RPMLimit != 0 || k.TPMLimit != 0 || k.ExpiresAt != 0 || k.AllowedIPs != "" {
		t.Errorf("admin relax not applied: %+v", k)
	}
}
