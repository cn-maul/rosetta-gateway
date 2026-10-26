package dataplane

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/cn-maul/rosetta-gateway/internal/store"
)

// M08-02: if the durable lease write is lost, the finalizer must not clear its
// in-memory reservation and assume the database was updated. It falls back to
// a checked direct release.
func TestR3_M08_02ReleaseWithoutLeaseFallsBackToCheckedRelease(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(filepath.Join(t.TempDir(), "gw.db"),
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.CreateUser(ctx, &store.User{ID: "u1", Username: "u1"}); err != nil {
		t.Fatalf("user: %v", err)
	}
	if err := db.CreateAccessKey(ctx, &store.AccessKey{
		ID: "k1", UserID: "u1", Name: "k1", KeyHash: "hash", Enabled: true,
		QuotaTokens: 10_000,
	}); err != nil {
		t.Fatalf("key: %v", err)
	}
	reserved, ok, err := db.ReserveQuota(ctx, "k1", 100)
	if err != nil || !ok || reserved != 100 {
		t.Fatalf("reserve = (%d,%v,%v)", reserved, ok, err)
	}

	rc := NewRateCommit(nil, "k1", 0, 0, db,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	rc.SetRequestID("req-without-lease")
	rc.SetKeyQuotaReserved(reserved) // deliberately no RecordQuotaReservationLease row
	rc.ReleaseQuota()

	var remaining int64
	if err := db.DB().QueryRowContext(ctx,
		`SELECT reserved_tokens FROM access_keys WHERE id = 'k1'`).Scan(&remaining); err != nil {
		t.Fatalf("read reservation: %v", err)
	}
	if remaining != 0 {
		t.Fatalf("reserved_tokens = %d after missing-lease fallback, want 0", remaining)
	}
}
