package store

import (
	"context"
	"testing"
	"time"
)

// R3 failure-first coverage for the accounting queue state machine.

func seedR3Usage(t *testing.T, st *Store, id, userID, requestID string,
	ts int64, cost float64, state string) {
	t.Helper()
	if _, err := st.db.ExecContext(context.Background(),
		`INSERT INTO usage_records (id, ts, access_key_id, user_id, public_model,
		   provider_id, upstream_model, ingress_protocol, stream,
		   input_tokens, output_tokens, total_tokens, usage_state, status, http_status,
		   latency_ms, cost_total, request_id)
		 VALUES (?, ?, 'k1', ?, 'priced', 'p1', 'priced', 'chat', 0,
		         100, 0, 100, ?, 'ok', 200, 10, ?, ?)`,
		id, ts, userID, state, cost, requestID); err != nil {
		t.Fatalf("seed usage %s: %v", id, err)
	}
}

func usageRecordExists(t *testing.T, st *Store, id string) bool {
	t.Helper()
	var n int
	if err := st.read.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM usage_records WHERE id = ?`, id).Scan(&n); err != nil {
		t.Fatalf("read usage %s: %v", id, err)
	}
	return n == 1
}

// M07-01: cost_pending is recoverable work, not terminal usage. Retention
// must leave it in the detail table until recomputation resolves it.
func TestR3_M07_01PendingUsageSurvivesRetentionCutoff(t *testing.T) {
	ctx := context.Background()
	st := testStore(t, t.TempDir()+"/gw.db")
	base := time.Now()
	freezeNow(t, base)
	cutoff := time.Date(base.Year(), base.Month(), base.Day(), 0, 0, 0, 0, time.Local).
		AddDate(0, 0, -DefaultRetentionDays)

	seedR3Usage(t, st, "pending-old", "u1", "req-pending-old",
		cutoff.AddDate(0, 0, -3).UnixMilli(), 0, UsageStateCostPending)
	seedR3Usage(t, st, "reported-old", "u1", "req-reported-old",
		cutoff.AddDate(0, 0, -3).UnixMilli(), 0.01, UsageStateReported)
	if _, err := st.db.ExecContext(ctx,
		`INSERT INTO balance_charges (request_id, user_id, amount_cents, ts)
		 VALUES ('req-reported-old', 'u1', 1, ?)`,
		cutoff.AddDate(0, 0, -3).UnixMilli()); err != nil {
		t.Fatalf("seed settled marker: %v", err)
	}

	if _, err := st.PruneOldUsage(ctx, DefaultRetentionDays); err != nil {
		t.Fatalf("prune: %v", err)
	}
	if !usageRecordExists(t, st, "pending-old") {
		t.Fatal("retention archived a recoverable cost_pending record")
	}
	if usageRecordExists(t, st, "reported-old") {
		t.Fatal("retention did not archive the terminal reported record")
	}
}

// A positive successful usage row without a completion marker is still
// recoverable work. It must survive retention just like cost_pending; only
// rows with a marker (or a pending ledger row) may cross the cutoff.
func TestR3_M07_01UnchargedUsageSurvivesRetentionCutoff(t *testing.T) {
	ctx := context.Background()
	st := testStore(t, t.TempDir()+"/gw.db")
	base := time.Now()
	freezeNow(t, base)
	cutoff := time.Date(base.Year(), base.Month(), base.Day(), 0, 0, 0, 0, time.Local).
		AddDate(0, 0, -DefaultRetentionDays)
	old := cutoff.AddDate(0, 0, -3).UnixMilli()

	seedR3Usage(t, st, "uncharged-old", "u1", "req-uncharged-old", old, 3, UsageStateReported)
	seedR3Usage(t, st, "charged-old", "u1", "req-charged-old", old, 3, UsageStateReported)
	if _, err := st.db.ExecContext(ctx,
		`INSERT INTO balance_charges (request_id, user_id, amount_cents, ts)
		 VALUES ('req-charged-old', 'u1', 300, ?)`, old); err != nil {
		t.Fatalf("seed charge marker: %v", err)
	}

	if _, err := st.PruneOldUsage(ctx, DefaultRetentionDays); err != nil {
		t.Fatalf("prune: %v", err)
	}
	if !usageRecordExists(t, st, "uncharged-old") {
		t.Fatal("retention archived a positive successful record with no charge marker")
	}
	if usageRecordExists(t, st, "charged-old") {
		t.Fatal("retention did not archive the settled record")
	}
}

// M14-02: stored costs and precheck must use the same effective-price
// fallback. Input-only models must not price output at zero.
func TestR3_M14_02ActualPricingUsesEffectiveFallback(t *testing.T) {
	ctx := context.Background()
	st := testStore(t, t.TempDir()+"/gw.db")
	if err := st.CreateProvider(ctx, &Provider{
		ID: "p1", Slug: "p1", Name: "P1", Endpoint: "http://x",
		Protocol: "chat", Enabled: true,
	}); err != nil {
		t.Fatalf("provider: %v", err)
	}
	if err := st.CreateUpstreamModel(ctx, &UpstreamModel{
		ID: "m1", ProviderID: "p1", ModelID: "input-only", PriceInput: 3,
	}); err != nil {
		t.Fatalf("model: %v", err)
	}

	cost, err := st.CreateUsageRecordWithCost(ctx, &UsageRecord{
		ID: "input-only-usage", UserID: "u1", AccessKeyID: "k1",
		PublicModel: "input-only", ProviderID: "p1", UpstreamModel: "input-only",
		IngressProtocol: "chat", InputTokens: 1_000_000,
		OutputTokens: 1_000_000, TotalTokens: 2_000_000,
		UsageState: UsageStateReported, Status: "ok", RequestID: "req-input-only",
	})
	if err != nil {
		t.Fatalf("create usage: %v", err)
	}
	if want := 6.0; cost < want-1e-9 || cost > want+1e-9 {
		t.Fatalf("stored cost = %v, want %v from effective fallback", cost, want)
	}
}

// M14-01: a full page of permanently unaffordable rows must not block a
// later chargeable row. The page must expose each settlement result.
func TestR3_M14_01ReconciliationProgressesPastDeferredHead(t *testing.T) {
	ctx := context.Background()
	st := testStore(t, t.TempDir()+"/gw.db")
	seedPricedModel(t, st, 3, 0.1, 9)
	mustUser(t, st, "broke", 0, false)
	mustUser(t, st, "rich", 10_000, false)

	for i := 0; i < 5; i++ {
		seedR3Usage(t, st, "deferred-"+itoa(i), "broke",
			"req-deferred-"+itoa(i), 1_700_000_000_000+int64(i), 3, UsageStateReported)
	}
	seedR3Usage(t, st, "chargeable", "rich", "req-chargeable",
		1_700_000_001_000, 3, UsageStateReported)

	result, err := st.ReconcileUnchargedUsage(ctx, 5)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	// The scan has a fixed page size: the first page classifies the five
	// deferred head rows and records their backoff without double-counting
	// them as collected.
	if result.Deferred != 5 || result.Charged != 0 || result.Failed != 0 {
		t.Fatalf("first result = %+v, want deferred=5 charged=0 failed=0", result)
	}
	second, err := st.ReconcileUnchargedUsage(ctx, 5)
	if err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	if second.Charged != 1 || second.Deferred != 0 || second.Failed != 0 {
		t.Fatalf("second result = %+v, want charged=1 deferred=0 failed=0", second)
	}
	assertBalance(t, st, "rich", 9_700, true)
	assertBalance(t, st, "broke", 0, true)

	third, err := st.ReconcileUnchargedUsage(ctx, 5)
	if err != nil {
		t.Fatalf("third reconcile: %v", err)
	}
	if third.Charged != 0 {
		t.Fatalf("third page charged %d rows; deferred rows were not skipped", third.Charged)
	}
}

// R3 / M08-02: a failed quota release retains a durable lease until retry.
func TestR3_M08_02QuotaReleaseRetriesAfterTransientFailure(t *testing.T) {
	ctx := context.Background()
	st := testStore(t, t.TempDir()+"/gw.db")
	if err := st.CreateUser(ctx, &User{ID: "u1", Username: "u1"}); err != nil {
		t.Fatalf("user: %v", err)
	}
	if err := st.CreateAccessKey(ctx, &AccessKey{
		ID: "k1", UserID: "u1", Name: "k1", KeyHash: "hash", Enabled: true,
		QuotaTokens: 10_000,
	}); err != nil {
		t.Fatalf("key: %v", err)
	}
	reserved, ok, err := st.ReserveQuota(ctx, "k1", 100)
	if err != nil || !ok || reserved != 100 {
		t.Fatalf("reserve = (%d,%v,%v)", reserved, ok, err)
	}

	if err := st.RecordQuotaReservationLease(ctx, "req-lease", "k1", "", reserved, 0); err != nil {
		t.Fatalf("record lease: %v", err)
	}
	// Force the first release transaction to fail after reading the lease.
	// The lease must remain retryable instead of being cleared in memory.
	if _, err := st.db.ExecContext(ctx,
		`CREATE TRIGGER reject_r3_quota_release BEFORE UPDATE OF reserved_tokens ON access_keys
		 BEGIN SELECT RAISE(ABORT, 'transient quota release failure'); END`); err != nil {
		t.Fatalf("create release trigger: %v", err)
	}
	if err := st.ReleaseOwnedQuotaReservations(ctx, "req-lease"); err == nil {
		t.Fatal("release reported success although the quota write failed")
	}
	var pendingLeases int
	if err := st.read.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM quota_reservation_leases
		  WHERE request_id = 'req-lease' AND released = 0`).Scan(&pendingLeases); err != nil {
		t.Fatalf("read lease: %v", err)
	}
	if pendingLeases != 1 {
		t.Fatalf("retryable leases = %d, want 1 after failed release", pendingLeases)
	}
	if _, err := st.db.ExecContext(ctx, `DROP TRIGGER reject_r3_quota_release`); err != nil {
		t.Fatalf("drop trigger: %v", err)
	}
	if err := st.ReleaseOwnedQuotaReservations(ctx, "req-lease"); err != nil {
		t.Fatalf("retry release: %v", err)
	}
	var pending int
	if err := st.read.QueryRowContext(ctx,
		`SELECT reserved_tokens FROM access_keys WHERE id = 'k1'`).Scan(&pending); err != nil {
		t.Fatalf("read reserved: %v", err)
	}
	if pending != 0 {
		t.Fatalf("reserved = %d after successful release, want 0", pending)
	}
}

// The periodic sweep must recover coverage even when the request finalizer can
// no longer retry (for example a crash), without touching a fresh lease.
func TestR3_M08_02ExpiredLeaseIsRecoverableWithoutRestart(t *testing.T) {
	ctx := context.Background()
	st := testStore(t, t.TempDir()+"/gw.db")
	if err := st.CreateUser(ctx, &User{ID: "u1", Username: "u1"}); err != nil {
		t.Fatalf("user: %v", err)
	}
	if err := st.CreateAccessKey(ctx, &AccessKey{
		ID: "k1", UserID: "u1", Name: "k1", KeyHash: "hash", Enabled: true,
		QuotaTokens: 10_000,
	}); err != nil {
		t.Fatalf("key: %v", err)
	}
	_, ok, err := st.ReserveQuota(ctx, "k1", 100)
	if err != nil || !ok {
		t.Fatalf("reserve: ok=%v err=%v", ok, err)
	}
	if err := st.RecordQuotaReservationLease(ctx, "req-expired", "k1", "", 100, 0); err != nil {
		t.Fatalf("record lease: %v", err)
	}
	if _, err := st.db.ExecContext(ctx,
		`UPDATE quota_reservation_leases SET lease_until = 1 WHERE request_id = 'req-expired'`); err != nil {
		t.Fatalf("expire lease: %v", err)
	}
	n, err := st.SweepExpiredQuotaReservations(ctx, time.Now())
	if err != nil || n != 1 {
		t.Fatalf("sweep = (%d,%v), want (1,nil)", n, err)
	}
	var reserved int64
	if err := st.read.QueryRowContext(ctx,
		`SELECT reserved_tokens FROM access_keys WHERE id = 'k1'`).Scan(&reserved); err != nil {
		t.Fatalf("read reserved: %v", err)
	}
	if reserved != 0 {
		t.Fatalf("reserved = %d after sweep, want 0", reserved)
	}
}
