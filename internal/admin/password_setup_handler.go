package admin

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/cn-maul/rosetta-gateway/internal/crypto"
	"github.com/cn-maul/rosetta-gateway/internal/httpx"
	"github.com/cn-maul/rosetta-gateway/internal/identity"
	"github.com/cn-maul/rosetta-gateway/internal/store"
	"github.com/cn-maul/rosetta-gateway/internal/userauth"
)

const passwordSetupTokenTTL = 24 * time.Hour

// @ts-type PasswordSetupLink
type passwordSetupLinkResponse struct {
	// URLPath is relative and deliberately carries the token in the hash
	// fragment. Fragments are not sent in the initial HTTP request, so reverse
	// proxy and access logs do not capture the secret.
	URLPath   string `json:"url_path"`
	ExpiresAt int64  `json:"expires_at"`
}

// @ts-type PasswordSetupStatus
type passwordSetupStatusResponse struct {
	Valid     bool   `json:"valid"`
	Username  string `json:"username,omitempty"`
	ExpiresAt int64  `json:"expires_at,omitempty"`
}

type passwordSetupCheckRequest struct {
	Token string `json:"token"`
}

type passwordSetupCompleteRequest struct {
	Token    string `json:"token"`
	Password string `json:"password"`
}

// CreatePasswordSetupLink issues a one-time invitation for an active ordinary
// user who has not set a password yet. The plaintext token is returned exactly
// once; only its SHA-256 hash is persisted.
func (h *UserHandler) CreatePasswordSetupLink(w http.ResponseWriter, r *http.Request, id string) {
	self, ok := requireAdmin(w, r)
	if !ok {
		return
	}

	u, err := h.store.GetUser(r.Context(), id)
	if err != nil {
		writeServerError(w, "password setup: get user", err)
		return
	}
	if u == nil {
		writeError(w, http.StatusNotFound, "用户不存在")
		return
	}
	if u.ID == self.ID || u.IsAdmin() {
		writeError(w, http.StatusBadRequest, "管理员账号不能生成用户设密链接")
		return
	}
	if !u.IsActive() {
		writeError(w, http.StatusConflict, "请先启用该用户，再生成设密链接")
		return
	}
	if u.PasswordHash != "" {
		writeError(w, http.StatusConflict, "该用户已有密码，请使用“改密”或让用户自行修改密码")
		return
	}

	rawToken := crypto.GenerateKey()
	tokenHash := passwordSetupTokenHash(rawToken)
	expiresAt := time.Now().Add(passwordSetupTokenTTL).UnixMilli()
	if err := h.store.ReplacePasswordSetupToken(
		r.Context(), u.ID, tokenHash, expiresAt, self.ID,
	); err != nil {
		if errors.Is(err, store.ErrPasswordSetupNotAllowed) {
			writeError(w, http.StatusConflict, "该用户当前不能设置密码，请刷新后重试")
			return
		}
		writeServerError(w, "password setup: issue token", err)
		return
	}

	query := url.Values{"token": {rawToken}}.Encode()
	writeJSON(w, http.StatusCreated, passwordSetupLinkResponse{
		URLPath:   "/admin/#/set-password?" + query,
		ExpiresAt: expiresAt,
	})
}

// CheckPasswordSetupLink lets the public setup page render the target account
// name without authenticating. Invalid, expired, used and disabled-account
// tokens all return the same valid=false shape.
func (h *UserHandler) CheckPasswordSetupLink(w http.ResponseWriter, r *http.Request) {
	if !httpx.SameOrigin(r) {
		httpx.WriteForbidden(w)
		return
	}
	var req passwordSetupCheckRequest
	if err := decodeJSON(w, r, &req); err != nil {
		if errors.Is(err, errUnsupportedMediaType) {
			return
		}
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	token := strings.TrimSpace(req.Token)
	if !validPasswordSetupToken(token) {
		writeJSON(w, http.StatusOK, passwordSetupStatusResponse{Valid: false})
		return
	}

	u, expiresAt, err := h.store.GetPasswordSetupToken(
		r.Context(), passwordSetupTokenHash(token), time.Now().UnixMilli())
	if err != nil {
		writeServerError(w, "password setup: check token", err)
		return
	}
	if u == nil {
		writeJSON(w, http.StatusOK, passwordSetupStatusResponse{Valid: false})
		return
	}
	writeJSON(w, http.StatusOK, passwordSetupStatusResponse{
		Valid: true, Username: u.Username, ExpiresAt: expiresAt,
	})
}

// CompletePasswordSetup consumes the invitation, stores the new password and
// issues a session in one flow. The token row is deleted in the same DB
// transaction as the password update.
func (h *UserHandler) CompletePasswordSetup(w http.ResponseWriter, r *http.Request) {
	if !httpx.SameOrigin(r) {
		httpx.WriteForbidden(w)
		return
	}
	if h.mgr == nil || !h.mgr.Enabled() {
		writeError(w, http.StatusServiceUnavailable,
			"用户登录未启用：请设置环境变量 "+userauth.SecretEnvName+"（至少 32 个字符）后重启")
		return
	}

	var req passwordSetupCompleteRequest
	if err := decodeJSON(w, r, &req); err != nil {
		if errors.Is(err, errUnsupportedMediaType) {
			return
		}
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	token := strings.TrimSpace(req.Token)
	if !validPasswordSetupToken(token) {
		writeError(w, http.StatusGone, "设密链接无效或已过期，请联系管理员重新生成")
		return
	}

	th := h.passwordSetupThrottle()
	ip := httpx.ClientIPOf(r)
	if th != nil && !th.Allow(ip) {
		writeTooMany(w, th.RetryAfter(ip))
		return
	}
	if th != nil {
		defer th.Release(ip)
	}

	hash, err := userauth.HashPassword(req.Password)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	userID, err := h.store.ConsumePasswordSetupToken(
		r.Context(), passwordSetupTokenHash(token), hash, time.Now().UnixMilli())
	if err != nil {
		switch {
		case errors.Is(err, store.ErrPasswordSetupTokenInvalid):
			if th != nil {
				th.Fail(ip)
			}
			writeError(w, http.StatusGone, "设密链接无效或已过期，请联系管理员重新生成")
		default:
			writeServerError(w, "password setup: consume token", err)
		}
		return
	}

	fresh, err := h.store.GetUser(r.Context(), userID)
	if err != nil || fresh == nil {
		writeServerError(w, "password setup: reload user", err)
		return
	}
	sessionToken, exp, err := h.mgr.Issue(&userauth.UserClaims{
		UserID: fresh.ID, AuthVersion: fresh.AuthVersion,
		Role: fresh.Role, Username: fresh.Username,
	})
	if err != nil {
		writeServerError(w, "password setup: issue session", err)
		return
	}
	if th != nil {
		th.Success(ip)
	}
	identity.TouchLogin(h.store, r.Context(), fresh.ID)
	identity.SetSessionCookie(w, sessionToken, exp, httpx.SessionCookieSecure(r))
	writeJSON(w, http.StatusOK, loginResponse{
		Token: sessionToken, ExpiresAt: exp.UnixMilli(),
		Username: fresh.Username, Role: fresh.Role,
		MustSetPassword: false,
	})
}

func passwordSetupTokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func validPasswordSetupToken(token string) bool {
	return len(token) >= 32 && len(token) <= 128
}
