package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/cn-maul/rosetta-gateway/internal/store"
)

func createInviteUser(t *testing.T, h *UserHandler, username, password string) userResponse {
	t.Helper()
	body := `{"username":"` + username + `"`
	if password != "" {
		body += `,"password":"` + password + `"`
	}
	body += `}`
	rec := httptest.NewRecorder()
	h.CreateUser(rec, asAdmin(jsonRequest(http.MethodPost, "/admin/api/users", strings.NewReader(body))))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create user: code=%d body=%s", rec.Code, rec.Body.String())
	}
	var u userResponse
	decodeBody(t, rec, &u)
	return u
}

func tokenFromSetupPath(t *testing.T, path string) string {
	t.Helper()
	parsed, err := url.Parse(path)
	if err != nil {
		t.Fatalf("parse setup path: %v", err)
	}
	fragment, err := url.Parse(strings.TrimPrefix(parsed.Fragment, "/"))
	if err != nil {
		t.Fatalf("parse setup fragment: %v", err)
	}
	token := fragment.Query().Get("token")
	if token == "" {
		t.Fatalf("setup path has no token: %s", path)
	}
	return token
}

func TestCreatePasswordSetupLink_AdminOnlyAndEligibility(t *testing.T) {
	st := newScopeStore(t)
	h := NewUserHandler(st, newTestManager(t))
	u := createInviteUser(t, h, "alice", "")

	rec := httptest.NewRecorder()
	h.CreatePasswordSetupLink(rec,
		asUser(httptest.NewRequest(http.MethodPost, "/admin/api/users/"+u.ID+"/password-setup", nil), "u1"),
		u.ID)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin code=%d, want 403", rec.Code)
	}

	admin := &store.User{
		ID: "admin1", Username: "adminuser", PasswordHash: "existing-hash",
		Role: store.RoleAdmin, Status: store.UserStatusActive, AuthVersion: 1,
	}
	if err := st.CreateUser(t.Context(), admin); err != nil {
		t.Fatalf("create admin: %v", err)
	}
	rec = httptest.NewRecorder()
	h.CreatePasswordSetupLink(rec, asAdminID(httptest.NewRequest(
		http.MethodPost, "/admin/api/users/"+admin.ID+"/password-setup", nil), admin.ID), admin.ID)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("admin target code=%d, want 400", rec.Code)
	}

	rec = httptest.NewRecorder()
	h.CreatePasswordSetupLink(rec, asAdmin(httptest.NewRequest(http.MethodPost, "/admin/api/users/missing/password-setup", nil)), "missing")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing user code=%d, want 404", rec.Code)
	}

	withPassword := createInviteUser(t, h, "already", "Str0ngPassw0rd!")
	rec = httptest.NewRecorder()
	h.CreatePasswordSetupLink(rec, asAdmin(httptest.NewRequest(
		http.MethodPost, "/admin/api/users/"+withPassword.ID+"/password-setup", nil)), withPassword.ID)
	if rec.Code != http.StatusConflict {
		t.Fatalf("user with password code=%d, want 409", rec.Code)
	}

	rec = httptest.NewRecorder()
	h.CreatePasswordSetupLink(rec, asAdmin(httptest.NewRequest(
		http.MethodPost, "/admin/api/users/"+u.ID+"/password-setup", nil)), u.ID)
	if rec.Code != http.StatusCreated {
		t.Fatalf("issue link: code=%d body=%s", rec.Code, rec.Body.String())
	}
	var link passwordSetupLinkResponse
	decodeBody(t, rec, &link)
	if !strings.HasPrefix(link.URLPath, "/admin/#/set-password?token=") {
		t.Fatalf("url_path = %q", link.URLPath)
	}
	if link.ExpiresAt <= time.Now().UnixMilli() {
		t.Fatalf("expires_at = %d, want future", link.ExpiresAt)
	}
	token := tokenFromSetupPath(t, link.URLPath)
	got, _, err := st.GetPasswordSetupToken(t.Context(), passwordSetupTokenHash(token), time.Now().UnixMilli())
	if err != nil || got == nil || got.ID != u.ID {
		t.Fatalf("stored token lookup = user=%+v err=%v", got, err)
	}
}

func TestCheckPasswordSetupLink_ValidAndInvalidUniform(t *testing.T) {
	st := newScopeStore(t)
	h := NewUserHandler(st, newTestManager(t))
	u := createInviteUser(t, h, "alice", "")

	raw := "0123456789abcdef0123456789abcdef"
	if err := st.ReplacePasswordSetupToken(
		t.Context(), u.ID, passwordSetupTokenHash(raw),
		time.Now().Add(time.Hour).UnixMilli(), "admin",
	); err != nil {
		t.Fatalf("seed token: %v", err)
	}

	for _, tc := range []struct {
		token string
		valid bool
	}{
		{raw, true},
		{"wrong-token-wrong-token-wrong-token", false},
	} {
		rec := httptest.NewRecorder()
		h.CheckPasswordSetupLink(rec, jsonRequest(http.MethodPost, "/admin/api/password-setup/check",
			strings.NewReader(`{"token":"`+tc.token+`"}`)))
		if rec.Code != http.StatusOK {
			t.Fatalf("check %q code=%d body=%s", tc.token, rec.Code, rec.Body.String())
		}
		var got passwordSetupStatusResponse
		decodeBody(t, rec, &got)
		if got.Valid != tc.valid {
			t.Fatalf("token %q valid=%v, want %v", tc.token, got.Valid, tc.valid)
		}
		if tc.valid && got.Username != "alice" {
			t.Fatalf("username = %q, want alice", got.Username)
		}
		if !tc.valid && got.Username != "" {
			t.Fatalf("invalid token leaked username %q", got.Username)
		}
	}
}

func TestCompletePasswordSetup_ConsumesOnceAndLogsIn(t *testing.T) {
	st := newScopeStore(t)
	mgr := newTestManager(t)
	h := NewUserHandler(st, mgr)
	u := createInviteUser(t, h, "alice", "")

	raw := "fedcba9876543210fedcba9876543210"
	if err := st.ReplacePasswordSetupToken(
		t.Context(), u.ID, passwordSetupTokenHash(raw),
		time.Now().Add(time.Hour).UnixMilli(), "admin",
	); err != nil {
		t.Fatalf("seed token: %v", err)
	}
	body := `{"token":"` + raw + `","password":"Str0ngPassw0rd!"}`
	rec := httptest.NewRecorder()
	h.CompletePasswordSetup(rec, jsonRequest(http.MethodPost, "/admin/api/password-setup", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("complete setup code=%d body=%s", rec.Code, rec.Body.String())
	}
	var login loginResponse
	decodeBody(t, rec, &login)
	if login.Token == "" || login.Username != "alice" {
		t.Fatalf("login response = %+v", login)
	}
	got, err := st.GetUser(t.Context(), u.ID)
	if err != nil {
		t.Fatalf("get user: %v", err)
	}
	if got.PasswordHash == "" || got.AuthVersion != 2 {
		t.Fatalf("password not persisted or auth_version not bumped: %+v", got)
	}

	rec = httptest.NewRecorder()
	h.CompletePasswordSetup(rec, jsonRequest(http.MethodPost, "/admin/api/password-setup", strings.NewReader(body)))
	if rec.Code != http.StatusGone {
		t.Fatalf("second use code=%d body=%s, want 410", rec.Code, rec.Body.String())
	}
	var apiErr apiErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &apiErr); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if strings.Contains(apiErr.Error.Message, "alice") {
		t.Fatalf("invalid-link error leaks account: %s", apiErr.Error.Message)
	}
}
