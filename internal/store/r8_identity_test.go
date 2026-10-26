package store

import (
	"context"
	"path/filepath"
	"testing"
)

// Every stored account must use the same ASCII/NOCASE identity used by lookup.
// This keeps direct DAO callers from creating rows that login can only find by
// the exact spelling and surrounding whitespace.
func TestR8_UsernameCanonicalAtStoreBoundary(t *testing.T) {
	st := openSchemaTestStore(t, filepath.Join(t.TempDir(), "r8.db"))
	ctx := context.Background()

	if err := st.CreateUser(ctx, &User{ID: "u1", Username: "  Alice  "}); err != nil {
		t.Fatalf("create canonical user: %v", err)
	}
	got, err := st.GetUser(ctx, "u1")
	if err != nil {
		t.Fatalf("get user: %v", err)
	}
	if got.Username != "alice" {
		t.Fatalf("stored username = %q, want canonical %q", got.Username, "alice")
	}

	probe, err := st.GetUserByUsername(ctx, "  ALICE\t")
	if err != nil {
		t.Fatalf("lookup canonical variant: %v", err)
	}
	if probe == nil || probe.ID != "u1" {
		t.Fatalf("canonical lookup = %+v, want user u1", probe)
	}

	if err := st.CreateUser(ctx, &User{ID: "u2", Username: "ALICE"}); !IsUniqueViolation(err) {
		t.Fatalf("duplicate canonical username error = %v, want unique violation", err)
	}
}
