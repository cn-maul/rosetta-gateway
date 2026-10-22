package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func createPasswordlessUser(t *testing.T, db *Store, id, username string) {
	t.Helper()
	if err := db.CreateUser(context.Background(), &User{
		ID: id, Username: username, Role: RoleUser,
		Status: UserStatusActive, AuthVersion: 1,
	}); err != nil {
		t.Fatalf("create user: %v", err)
	}
}

func TestPasswordSetupToken_ConsumeOnce(t *testing.T) {
	db := newStore(t)
	ctx := context.Background()
	createPasswordlessUser(t, db, "u1", "alice")

	expiresAt := time.Now().Add(time.Hour).UnixMilli()
	if err := db.ReplacePasswordSetupToken(ctx, "u1", "hash-1", expiresAt, "admin"); err != nil {
		t.Fatalf("replace token: %v", err)
	}
	u, gotExpiry, err := db.GetPasswordSetupToken(ctx, "hash-1", time.Now().UnixMilli())
	if err != nil {
		t.Fatalf("get token: %v", err)
	}
	if u == nil || u.Username != "alice" || gotExpiry != expiresAt {
		t.Fatalf("token lookup = user=%+v expiry=%d", u, gotExpiry)
	}

	userID, err := db.ConsumePasswordSetupToken(
		ctx, "hash-1", "new-password-hash", time.Now().UnixMilli())
	if err != nil {
		t.Fatalf("consume token: %v", err)
	}
	if userID != "u1" {
		t.Fatalf("user id = %q, want u1", userID)
	}
	got, err := db.GetUser(ctx, "u1")
	if err != nil {
		t.Fatalf("get user: %v", err)
	}
	if got.PasswordHash != "new-password-hash" || got.AuthVersion != 2 {
		t.Fatalf("password=%q auth_version=%d, want new hash and 2", got.PasswordHash, got.AuthVersion)
	}

	_, err = db.ConsumePasswordSetupToken(
		ctx, "hash-1", "second-hash", time.Now().UnixMilli())
	if !errors.Is(err, ErrPasswordSetupTokenInvalid) {
		t.Fatalf("second consume error = %v, want ErrPasswordSetupTokenInvalid", err)
	}
	u, _, err = db.GetPasswordSetupToken(ctx, "hash-1", time.Now().UnixMilli())
	if err != nil || u != nil {
		t.Fatalf("consumed token lookup = user=%+v err=%v, want nil", u, err)
	}
}

func TestPasswordSetupToken_ExpiryReissueAndPasswordReset(t *testing.T) {
	db := newStore(t)
	ctx := context.Background()
	createPasswordlessUser(t, db, "u1", "alice")
	now := time.Now().UnixMilli()

	if err := db.ReplacePasswordSetupToken(ctx, "u1", "expired", now-1, "admin"); err != nil {
		t.Fatalf("replace expired token: %v", err)
	}
	u, _, err := db.GetPasswordSetupToken(ctx, "expired", now)
	if err != nil || u != nil {
		t.Fatalf("expired token lookup = user=%+v err=%v, want nil", u, err)
	}

	if err := db.ReplacePasswordSetupToken(ctx, "u1", "token-1", now+60_000, "admin"); err != nil {
		t.Fatalf("replace token 1: %v", err)
	}
	if err := db.ReplacePasswordSetupToken(ctx, "u1", "token-2", now+120_000, "admin"); err != nil {
		t.Fatalf("replace token 2: %v", err)
	}
	u, _, err = db.GetPasswordSetupToken(ctx, "token-1", now)
	if err != nil || u != nil {
		t.Fatalf("old token remained valid: user=%+v err=%v", u, err)
	}
	u, _, err = db.GetPasswordSetupToken(ctx, "token-2", now)
	if err != nil || u == nil {
		t.Fatalf("new token invalid: user=%+v err=%v", u, err)
	}

	if err := db.SetUserPassword(ctx, "u1", "new-hash"); err != nil {
		t.Fatalf("set password: %v", err)
	}
	u, _, err = db.GetPasswordSetupToken(ctx, "token-2", now)
	if err != nil || u != nil {
		t.Fatalf("token survived password reset: user=%+v err=%v", u, err)
	}
}

func TestPasswordSetupToken_RequiresEligibleUser(t *testing.T) {
	db := newStore(t)
	ctx := context.Background()
	expiresAt := time.Now().Add(time.Hour).UnixMilli()

	if err := db.CreateUser(ctx, &User{
		ID: "admin", Username: "root", PasswordHash: "hash",
		Role: RoleAdmin, Status: UserStatusActive,
	}); err != nil {
		t.Fatalf("create admin: %v", err)
	}
	createPasswordlessUser(t, db, "disabled", "disabled-user")
	if err := db.UpdateUserAndBumpAuthVersion(ctx, &User{
		ID: "disabled", Username: "disabled-user", Role: RoleUser,
		Status: UserStatusDisabled,
	}); err != nil {
		t.Fatalf("disable user: %v", err)
	}

	for _, id := range []string{"admin", "disabled", "missing"} {
		err := db.ReplacePasswordSetupToken(ctx, id, "hash-"+id, expiresAt, "admin")
		if !errors.Is(err, ErrPasswordSetupNotAllowed) {
			t.Errorf("ReplacePasswordSetupToken(%s) error = %v, want ErrPasswordSetupNotAllowed", id, err)
		}
	}
}

func TestPasswordSetupToken_DisableRevokesOutstandingLink(t *testing.T) {
	db := newStore(t)
	ctx := context.Background()
	createPasswordlessUser(t, db, "u1", "alice")
	now := time.Now().UnixMilli()
	if err := db.ReplacePasswordSetupToken(ctx, "u1", "token", now+60_000, "admin"); err != nil {
		t.Fatalf("replace token: %v", err)
	}
	if err := db.UpdateUserAndBumpAuthVersion(ctx, &User{
		ID: "u1", Username: "alice", Role: RoleUser, Status: UserStatusDisabled,
	}); err != nil {
		t.Fatalf("disable user: %v", err)
	}
	u, _, err := db.GetPasswordSetupToken(ctx, "token", now)
	if err != nil || u != nil {
		t.Fatalf("token survived disable: user=%+v err=%v", u, err)
	}
}
