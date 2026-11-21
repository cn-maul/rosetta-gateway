package main

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/cn-maul/rosetta-gateway/internal/snapshot"
	"github.com/cn-maul/rosetta-gateway/internal/store"
)

// Admission rejections are ordered so that a caller who is already over quota
// learns that first. Returning 403 for a model the key may not use would hide
// the 429 that actually blocks every request, and the client would retry with
// a different model forever.
func TestAdmission_QuotaOutranksModelWhitelist(t *testing.T) {
	h, db := goodHarness(t)
	reinitAllowance(t, snapshot.AllowOnly([]string{"some-other-model"}), nil)

	if err := db.UpdateAccessKey(context.Background(), "k1", &store.AccessKey{
		ID: "k1", Name: "t", Enabled: true, QuotaTokens: 100, UserID: testUserID,
	}); err != nil {
		t.Fatalf("set quota: %v", err)
	}
	if err := db.CreateUsageRecord(context.Background(), &store.UsageRecord{
		ID: "seed-order", AccessKeyID: "k1", UserID: testUserID, PublicModel: "flash",
		IngressProtocol: "chat", TotalTokens: 100, UsageState: "reported", Status: "ok",
	}); err != nil {
		t.Fatalf("seed usage: %v", err)
	}

	rec := postChat(h, "flash")
	if rec.Code != http.StatusTooManyRequests || !strings.Contains(rec.Body.String(), "insufficient_quota") {
		t.Fatalf("exhausted quota must win over the whitelist: got %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestAdmission_TPMOutranksModelWhitelist(t *testing.T) {
	h, _ := goodHarness(t)
	reinitAllowance(t, snapshot.AllowOnly([]string{"good/good-model"}), nil)

	old := snapshot.Get()
	keys := make(map[string]*snapshot.KeySnapshot, len(old.KeysByHash))
	for hash, ks := range old.KeysByHash {
		copied := *ks
		copied.TPMLimit = 1
		keys[hash] = &copied
	}
	snapshot.Init(&snapshot.Snapshot{
		Routes: old.Routes, Providers: old.Providers, KeysByHash: keys,
		UsersByID: old.UsersByID, Runtime: old.Runtime,
	})

	// Consume the one-token window with an allowed model. The successful
	// response reports 8 tokens, which is then committed against the window.
	if rec := postChat(h, "good/good-model"); rec.Code != http.StatusOK {
		t.Fatalf("priming request: want 200, got %d body=%s", rec.Code, rec.Body.String())
	}

	rec := postChat(h, "flash")
	if rec.Code != http.StatusTooManyRequests || !strings.Contains(rec.Body.String(), "rate_limit_exceeded") {
		t.Fatalf("exhausted TPM must win over the whitelist: got %d body=%s", rec.Code, rec.Body.String())
	}
}
