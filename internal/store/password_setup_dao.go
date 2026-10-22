package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// ErrPasswordSetupTokenInvalid deliberately covers every unusable-token case:
// missing, expired, already consumed, user disabled/deleted, or password
// already set. Callers must not turn this into a user-enumeration oracle.
var ErrPasswordSetupTokenInvalid = errors.New("password setup token is invalid")

// ErrPasswordSetupNotAllowed means the target account cannot receive a setup
// token right now (not a normal active user, or it already has a password).
var ErrPasswordSetupNotAllowed = errors.New("password setup is not allowed for this user")

// userPasswordSetupTokensDDL is shared by fresh database initialization and
// the idempotent compatibility pass. One user has at most one active link;
// reissuing replaces the previous row.
const userPasswordSetupTokensDDL = `CREATE TABLE IF NOT EXISTS user_password_setup_tokens (
	user_id    TEXT PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
	token_hash TEXT NOT NULL UNIQUE,
	expires_at INTEGER NOT NULL,
	created_by TEXT NOT NULL,
	created_at INTEGER NOT NULL
)`

// ReplacePasswordSetupToken stores a fresh token hash for an active user who
// still has no password. The conditional INSERT SELECT makes the eligibility
// check and write one atomic action, so an account cannot receive a link after
// a concurrent password reset.
func (s *Store) ReplacePasswordSetupToken(
	ctx context.Context,
	userID, tokenHash string,
	expiresAt int64,
	createdBy string,
) error {
	return s.RunTx(ctx, func(ctx context.Context, ex Tx) error {
		if _, err := ex.ExecContext(ctx,
			`DELETE FROM user_password_setup_tokens WHERE user_id = ?`, userID); err != nil {
			return err
		}
		res, err := ex.ExecContext(ctx,
			`INSERT INTO user_password_setup_tokens
				(user_id, token_hash, expires_at, created_by, created_at)
			 SELECT id, ?, ?, ?, ?
			   FROM users
			  WHERE id = ? AND role = ? AND status = ? AND password_hash = ''`,
			tokenHash, expiresAt, createdBy, time.Now().UnixMilli(),
			userID, RoleUser, UserStatusActive)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return ErrPasswordSetupNotAllowed
		}
		return nil
	})
}

// GetPasswordSetupToken validates a token hash and returns its account.
// Expired and otherwise unusable links all return (nil, nil).
func (s *Store) GetPasswordSetupToken(
	ctx context.Context,
	tokenHash string,
	now int64,
) (*User, int64, error) {
	row := s.read.QueryRowContext(ctx,
		`SELECT u.*, t.expires_at
		   FROM user_password_setup_tokens t
		   JOIN (SELECT `+userColumns+` FROM users) u ON u.id = t.user_id
		  WHERE t.token_hash = ?
		    AND t.expires_at > ?
		    AND u.role = ?
		    AND u.status = ?
		    AND u.password_hash = ''`,
		tokenHash, now, RoleUser, UserStatusActive)
	u, expiresAt, err := scanPasswordSetupUser(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, 0, nil
	}
	return u, expiresAt, err
}

// ConsumePasswordSetupToken sets the password and deletes the token in one
// transaction. The password update also increments auth_version, invalidating
// any session that somehow predates the link.
func (s *Store) ConsumePasswordSetupToken(
	ctx context.Context,
	tokenHash, passwordHash string,
	now int64,
) (string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback() }() //nolint:errcheck // no-op after commit

	var userID string
	err = tx.QueryRowContext(ctx,
		`DELETE FROM user_password_setup_tokens
		  WHERE token_hash = ?
		    AND expires_at > ?
		    AND EXISTS (
		      SELECT 1 FROM users
		       WHERE users.id = user_password_setup_tokens.user_id
		         AND users.role = ?
		         AND users.status = ?
		         AND users.password_hash = ''
		    )
		  RETURNING user_id`,
		tokenHash, now, RoleUser, UserStatusActive).Scan(&userID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrPasswordSetupTokenInvalid
	}
	if err != nil {
		return "", err
	}

	res, err := tx.ExecContext(ctx,
		`UPDATE users
		    SET password_hash = ?, auth_version = auth_version + 1, updated_at = ?
		  WHERE id = ? AND role = ? AND status = ? AND password_hash = ''`,
		passwordHash, time.Now().UnixMilli(), userID, RoleUser, UserStatusActive)
	if err != nil {
		return "", err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return "", err
	}
	if n == 0 {
		return "", ErrPasswordSetupTokenInvalid
	}

	if err := tx.Commit(); err != nil {
		return "", err
	}
	return userID, nil
}

func scanPasswordSetupUser(row interface {
	Scan(dest ...any) error
}) (*User, int64, error) {
	var u User
	var expiresAt int64
	err := row.Scan(
		&u.ID, &u.Username, &u.DisplayName, &u.PasswordHash,
		&u.Role, &u.Status, &u.GroupID, &u.QuotaTokens, &u.UsedTokens, &u.AuthVersion,
		&u.Remark, &u.CreatedAt, &u.UpdatedAt, &u.LastLoginAt,
		&u.BalanceCents, &u.Unlimited, &u.BalanceRemainder,
		&expiresAt,
	)
	if err != nil {
		return nil, 0, err
	}
	return &u, expiresAt, nil
}
