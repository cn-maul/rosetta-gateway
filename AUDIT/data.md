# Data / Persistence / Billing Audit — rosetta-gateway

Scope: `internal/store/**`, `internal/snapshot/**`, `cmd/gateway/billing.go`,
`internal/routing/**`, and the charge path in `cmd/gateway/main.go`.

All findings below were read from source and, where marked **Verified**, reproduced
against the real store package with a throwaway harness
(`AUDIT/.scratch-data/`, `go test ./AUDIT/.scratch-data/`). Line numbers refer to
the tree as of `d529b29`.

Severity: **P0** money loss / corruption / auth bypass / crash · **P1** serious
bug · **P2** real defect, limited impact · **P3** minor.

---

## Money correctness

### [P0] A failed charge COMMITS its idempotency placeholder, so the retry is silently swallowed forever

**Location** `internal/store/balance_dao.go:194-280` (specifically the two
`tx.Commit()` calls at `:265` and `:274`, versus the placeholder `INSERT OR IGNORE`
at `:211-220`).

**What's wrong**

`ChargeBalance` is documented (`:146-159`) as "placeholder first, then deduct, in
one transaction", so that "placeholder succeeded but the deduction failed" cannot
leave a window for double-charging. The placeholder insert and the `UPDATE` do
share a transaction — but on the two **error return paths** the code explicitly
**commits** the transaction before returning:

```go
// :263-278
switch {
case errors.Is(err, sql.ErrNoRows):
    if err := tx.Commit(); err != nil {   // :265  <- commits the placeholder!
        return err
    }
    return ErrNotFound
...
case balance.Valid && balance.Int64 < amountCents && charged == 0:
    if err := tx.Commit(); err != nil {   // :274  <- commits the placeholder!
        return err
    }
    return ErrInsufficientBalance
}
```

Both commits persist the `balance_charges` row that was inserted at `:212`. The
idempotency contract is therefore inverted on exactly the paths that report
failure: the one row that proves "this request was never actually charged" is the
row that gets written.

Because `:218-220` treats `RowsAffected()==0` on the placeholder as "already
charged, return nil", **every subsequent retry of that `request_id` returns
`nil` and deducts nothing**. The charge is unrecoverable: there is no admin entry
point that clears `balance_charges`, and no repair path anywhere in the package.

**Verified** (`AUDIT/.scratch-data/placeholder_test.go`):

```
attempt1 (insufficient): err=insufficient balance
attempt2 after topup:    err=<nil>
balance before=100100 after=100100 delta=0   (0 => silently swallowed)
control R2 (never failed): err=<nil> balance=99600   (normal charge works)
```

Same for the `ErrNotFound` path:

```
ghost charge err=record not found
ghost retry err=<nil> balance=100000   (99500 expected; 100000 => swallowed)
```

**Why it matters**

Direct, permanent money loss with **zero** error signal on the retry — `nil` is
logged as success or ignored. It is reachable from the normal retry path, because
`ChargeBalance` is called on a *background worker goroutine*
(`cmd/gateway/main.go:2287`) with `context.Background()`, and the store's whole
purpose here is to survive retry. Any transient condition that makes the
`UPDATE` miss on the first attempt — user deleted mid-request, key/user
topology change, or a genuine concurrent overdraft from the precheck race
(observed and acknowledged at `balance_dao.go:26-28`) — converts that user's
charge into a permanent no-op. The user consumed upstream tokens, `cost_total`
records the expense, the report shows the spend, and the balance never moves.
This is exactly the drift the module was written to make "structurally
impossible".

Note also the interaction with the documented intent at `:171-181`, which argues
that on insufficient balance it is better to "not deduct + report" than to
partially deduct — but that reasoning only holds if the *deduction* is the thing
being protected. Committing the placeholder additionally destroys the retry.

**Concrete fix**

Roll back (or skip the placeholder write) on both failure paths so a failed charge
leaves no trace and the retry can succeed. Simplest correct form — replace the two
commits with a bare return and let the deferred `tx.Rollback()` (`:207`) undo the
placeholder:

```go
case errors.Is(err, sql.ErrNoRows):
    return ErrNotFound            // rollback: no placeholder, retry is safe
...
case balance.Valid && balance.Int64 < amountCents && charged == 0:
    return ErrInsufficientBalance // rollback: no placeholder, retry is safe
```

If the `ErrNotFound` case is meant to stay a committed no-op for auditability,
then at minimum the insufficient-balance case must roll back, and
`ErrInsufficientBalance` needs to be excluded from the idempotency-hit fast path
(e.g. store the outcome alongside the placeholder and let a failed row be retried).
Add a regression test asserting: charge → insufficient → top up → retry same
`request_id` ⇒ balance decreases by `amountCents`.

**Confidence** high (read + reproduced).

---

### [P0] `balance_charges` PK is `request_id` alone, so an id collision silently cancels a *different* user's charge

**Location** `internal/store/store.go:350-356` (DDL) and
`internal/store/balance_dao.go:211-220` (the idempotency fast path).

**What's wrong**

```sql
CREATE TABLE IF NOT EXISTS balance_charges (
    request_id   TEXT PRIMARY KEY,
    user_id      TEXT NOT NULL,
    amount_cents INTEGER NOT NULL,
    ts           INTEGER NOT NULL
)
```

The dedup key is the request id alone, with `user_id` a plain (non-key) column.
Two charges from **different users** sharing a `request_id` collide on the PK;
the loser's `INSERT OR IGNORE` affects 0 rows, so `ChargeBalance` returns `nil`
having deducted nothing at all.

**Verified** (`AUDIT/.scratch-data/money_test.go`):

```
charge ub with SAME request_id -> err=<nil>
ua balance=99500  (expected, charged)
ub balance=100000 (expected 99300 if charged — NOT charged)
```

**Why it matters**

This is a cross-tenant money leak: one tenant's traffic can suppress another's
charge, and the suppressed charge reports success. Note that the failover path
makes same-`request_id` reuse *within* one user genuinely common (several
`usage_records` rows share one `request_id`, one per attempt) — the design
correctly relies on only one of those succeeding. Extending that same key across
users is not a safe generalization.

Exploitability today is bounded: `server.generateRequestID()`
(`internal/server/server.go:29-33`) mints a random 8-byte (64-bit) hex per
request, so natural collision needs ~2^32 requests. That is why this is rated
alongside the placeholder bug rather than above it — but the schema invariant is
wrong, and it becomes directly exploitable the moment a client-supplied
`X-Request-Id` is honoured, or the ID space is shortened. It is also a latent
tenant-isolation hole that a future "trust the client's request id for tracing"
feature would turn into a real one.

**Concrete fix**

Make the dedup key match the thing being protected:

```sql
CREATE TABLE IF NOT EXISTS balance_charges (
    user_id      TEXT NOT NULL,
    request_id   TEXT NOT NULL,
    amount_cents INTEGER NOT NULL,
    ts           INTEGER NOT NULL,
    PRIMARY KEY (user_id, request_id)
);
```

This needs a table rebuild (SQLite cannot alter a PK in place) — do it the same
way `ensureRollupDimensions` does it (`usage_archive.go:79-118`): check, rebuild
only when empty, recreate indexes. Then use `(user_id, request_id)` in the
`INSERT OR IGNORE` at `balance_dao.go:211-214`.

**Confidence** high (read + reproduced).

---

### [P2] A user with 1 cent of balance can make unbounded sub-cent requests for free

**Location** `cmd/gateway/billing.go:529-543` (`balancePreflight`) combined with
`internal/store/balance_dao.go:199-201` (`amountCents <= 0` → no-op).

**What's wrong**

The preflight has a deliberate, well-reasoned branch for the "priced model but
this request rounds to 0 cents" case:

```go
// :529-543
if estCents <= 0 {
    if !priced { return balanceAllow }
    if balanceCents > 0 { return balanceAllow }   // <-- 1 cent passes
    return balanceReject
}
```

The intent (stated at `:533-535`) is "a user who owes money must not ride free by
sending tiny requests". The guard `balanceCents > 0` enforces *"has at least one
cent"*, not *"can pay for this request"*. And the actual charge is then a no-op,
because `ChargeBalance` returns early on `amountCents <= 0` (`:199-201`).

`YuanToCents` rounds at the half-cent, so any request whose true cost is under
half a cent charges 0. With cheap models (e.g. `price_input = 0.1` 元/百万 tokens)
a 1-cent balance admits an effectively unlimited number of such calls, each of
which still consumes real upstream tokens and still writes a `cost_total` row.

**Why it matters**

Small but a genuine hole in the one gate that is supposed to be fail-closed.
It is bounded by the price configuration (a model priced at ≥ 1 元/百万 tokens
would need ~500 input tokens to reach half a cent, so ordinary-sized requests are
unaffected), which is why this is P2 rather than P0. It is most reachable on a
gateway with low-priced models and cheap prompts — exactly the multi-tenant,
low-price-per-token deployments this balance system targets.

**Concrete fix**

Tie the zero-estimate allowance to actual consumption rather than to "balance
non-zero": either (a) treat `balanceCents >= 1` as the floor only for requests
whose *estimated* cost rounds to 0 but whose **token estimate** is non-trivial —
i.e. gate on a token count as well as cents; or (b) accumulate sub-cent
remainders per user so they eventually cross the half-cent threshold and are
collected; or (c) simply reject when `estCents <= 0 && priced && balanceCents <= 0`
**and** additionally require the user's cumulative uncollected remainder to stay
under one cent. Option (b) is the only one that preserves the "小请求不误拒"
intent without a free ride.

**Confidence** medium (logic is unambiguous in the code; the magnitude depends on
the deployed price table, which I cannot see).

---

## Transactions, atomicity and concurrency

### Clean: the concurrent balance guard holds; no negative balances

**Location** `internal/store/balance_dao.go:239-244`.

The single-statement `UPDATE ... WHERE balance_cents IS NOT NULL AND
balance_cents >= ?` with the `MAX(0, …)` clamp is genuinely atomic under the
single-writer pool, and the precheck→charge race is **bounded by available
balance**, not unbounded overshoot.

**Verified** (`AUDIT/.scratch-data/money_test.go`, 20 goroutines × 100¢ against a
1000¢ balance):

```
charged_ok=10 insufficient=10 final_balance=0   (no negative balance)
```

**Verified** (`AUDIT/.scratch-data/rounding_test.go`, 20 concurrent
precheck-then-charge at 200¢ against 1000¢):

```
charged=5 rejected=15 final_balance=0
```

Exactly 5 charges succeeded — i.e. the number that fits, no more. The
documented "concurrency may overdraw slightly" note at `balance_dao.go:26-28`
overstates the risk: in practice the atomic guard caps the damage at zero. No
finding here.

### [P2] Usage INSERT and balance charge are two separate transactions — a crash between them loses the charge

**Location** `cmd/gateway/main.go:2217-2229` (worker) and `:2316-2328` (sync
fallback).

**What's wrong**

```go
// :2220-2228
cost, err := u.db.CreateUsageRecordWithCost(context.Background(), rec)
if err != nil { ...; continue }
u.charge(rec, cost)      // separate tx inside ChargeBalance
```

The usage row commits in its own transaction; the balance deduction commits in a
second, independent one. `ChargeBalance` has no foreign key, no existence check,
and no idempotency tie to the usage row — **Verified**: charging an id with no
corresponding usage record succeeds and deducts (`orphan charge err=<nil>
balance=9500`).

**Why it matters**

If the process is killed (SIGKILL, container eviction, OOM) between the two
commits, the request is billed to `cost_total` and to the reports but the balance
is never deducted, and nothing in the DB records that a charge is outstanding —
the failure is invisible. The window is small (microseconds) but the class of
event is exactly the one that kills processes. Note the `wait(ctx)` shutdown path
(`main.go:2332-2343`) drains the queue gracefully, so this only bites on hard
kills.

**Concrete fix**

Either wrap both in one transaction (needs a `Store.ChargeForUsage(ctx, rec,
cost)` that inserts the usage row and deducts in a single `BeginTx`), or record
the intended charge as pending before the async hop and reconcile on startup
(e.g. `SELECT ... FROM usage_records WHERE status='ok' AND <no matching
balance_charges row>`). The second is simpler and also repairs historical gaps
from the P0 above.

**Confidence** high for the mechanism; medium for real-world frequency.

---

## Schema / migrations

### [P2] `PRAGMA foreign_keys=ON` is applied, but several DDL references assume constraints the schema does not actually enforce

**Location** `internal/store/store.go:34` (DSN sets `_foreign_keys=ON` — correct),
versus the DDL for `usage_records` at `:222-243` and `balance_charges` at
`:350-355`.

**What's what**

Foreign keys **are** enabled (good — `_foreign_keys=ON` in the DSN, applied to
both the write and read pools). WAL is on, `busy_timeout=5000`, `synchronous=NORMAL`
— all appropriate and deliberately commented.

The gap is `usage_records`: it declares `access_key_id TEXT NOT NULL` and
`provider_id`/`upstream_model` as bare `NOT NULL` text with **no** `REFERENCES`
clause. This is intentional (`user_dao.go:342-343`: usage must outlive the key so
history survives key deletion) and the comments are honest about it. But it means
the row-level invariant the charge path depends on — "every usage row belongs to a
real user" — is not enforced anywhere. `balance_charges.user_id` is likewise
unenforced. Since the P0 placeholder bug above leaves `balance_charges` rows
pointing at users that were never charged, the absence of any referential
integrity is what makes the resulting state unrecoverable.

**Why it matters**

Not a bug in itself — the deliberate looseness is correct for usage history — but
it means data-integrity problems in the charge path have no safety net at all, and
no reconciliation query can distinguish "charged" from "placeholder only".

**Concrete fix**

Keep the usage history FK-free by design, but give the charge path a reconcilable
state machine: add an explicit `charged_at`/`settled` marker to
`balance_charges`, and add a startup-or-admin "reconcile charges" routine that
finds `usage_records` with `status='ok'` and no settled `balance_charges` row.
That single routine would have surfaced both P0s above automatically.

**Confidence** high.

### Clean: migration ordering, idempotent DDL and trigger rebuilds

`internal/store/store.go:86-403` is unusually careful and I found no defect in it:

- Trigger DDL uses `DROP` + `CREATE` rather than `CREATE IF NOT EXISTS`
  (`:279-284`) — correctly avoids the silent old-body/new-body divergence the
  comment describes.
- `usage_totals` placeholder row is inserted at `:335`, before the trigger that
  depends on it — the documented trap is genuinely avoided.
- `ensureColumns` → `ensureUserIndexes` → `ensureRollupDimensions` →
  `backfillUsageCost` → `ensureUsageTotalsTrigger` → `reconcileUsageTotals`
  ordering (`:365-393`) matches every dependency stated in the comments.
- `ensureRollupDimensions` (`usage_archive.go:79-118`) refuses to rebuild a
  non-empty stale table rather than dropping archived history.
- `backfillUsageCost` uses a rowid cursor (`:195-224`) instead of
  `WHERE cost_total = 0`, which is the correct way to avoid non-convergence on
  unpriced models.
- One-time migrations are gated by `app_settings` markers (`usage_archive.go:24-27`).
- `reconcileUsageTotals` correctly skips when `pruned_through_day` is set
  (`:263-268`) so it cannot roll back an archived total.
- `dropDeadColumns` checks `table_info` first and logs-and-continues on failure
  (`:447-460`) — correct, since failing startup over a redundant column would be
  disproportionate.

**Verified**: quota reservation accounting is exactly single-counted —
reserve 300 → usage insert of 250 (trigger) → release 300 ⇒
`used=250` (not 550), and the over-reserve guard rejects a second 900-token
reservation against a 1000 quota.

### [P3] `usage_records.user_id` can silently drift from `access_keys.user_id` after a key reassignment, with no reconciliation

**Location** `internal/store/key_dao.go:339-343` (`ReassignAccessKey`) and
`internal/store/usage_dao.go:16-22`.

**What's wrong**

`ReassignAccessKey` updates only `access_keys.user_id`. Historical
`usage_records.user_id` is deliberately frozen ("归属是历史事实",
`usage_dao.go:18-22`) — which is the right call for auditing. But nothing
records *when* the reassignment happened, so there is no way to tell an intended
historical freeze from an accidental one, and no admin tool to review or reverse
a transfer's effect on quota/billing attribution.

**Why it matters**

Minor, and the design choice itself is defensible. But the user-level quota
precheck (`main.go:1277-1291`, `SumUserUsedTokens`) reads the frozen
`usage_records.user_id`. So after a key transfer, the **new** owner is not
charged for the old owner's historical tokens (correct), yet both owners' quota
numbers silently shift with no record. An operator investigating "why did this
user's usage jump" has no signal.

**Concrete fix**

Record reassignment as an audit-log entry (`audit_dao.CreateAuditEntry` already
exists and is wired to the admin surface) carrying old/new `user_id`, and note the
semantics in the audit trail so the quota shift is explainable.

**Confidence** high for the mechanism, low impact.

---

## Usage aggregation, day boundaries, retention

### Clean: day-boundary handling is deliberate and internally consistent

This area had a documented past incident (the 69300 → 44550 client-billing drop,
`usage_dao.go:576-587`). I checked it and the fix is sound:

- `UsageSource` (`usage_source.go:83-149`) takes archive days only when the **whole
  day** falls inside the window (`:102-107`, `dayStartMs >= from AND
  dayEndMs <= to`), deliberately erring toward under-count rather than
  over-count — correct for a money-adjacent report.
- `SumTokensByDayForKey` (`usage_dao.go:589-634`) converts the archive's *local*
  `day` back to its UTC date via
  `date(CAST(strftime('%s', day,'utc') AS INTEGER),'unixepoch')` so the detail and
  archive branches share one day boundary. This is genuinely subtle and it is
  correct; the divergence from the UI's local-day grouping is documented at
  `:588` as intentional.
- `timeRangeClause` (`:319-334`) uses `>=` / `<=` on both bounds — inclusive on
  both ends, no off-by-one, and `to` of "now" cannot exclude same-millisecond rows
  (the `orgQueryWindow` note at `billing.go:270-272` records that this was
  actually hit and fixed).
- `dayExpr` (`usage_archive.go:313`) is a single shared constant used by both the
  archive writer and the day-bucketed readers, so writer/reader cannot drift.

The 86400000 ms-per-day assumption under DST is documented at `usage_source.go:77-78`
and `usage_dao.go:607-608`. Genuine, but bounded to window edges and honest.

### [P2] The prune watermark is one-way: pruning with a shorter window after a longer one silently advances it backwards' counterpart, and the retention setting is not monotonic

**Location** `internal/store/usage_archive.go:375-392`.

**What's wrong**

```go
cut := today.AddDate(0, 0, -keepDays)
res.PrunedThrough = cut.AddDate(0, 0, -1).Format("2006-01-02")
...
if cur != "" && cur >= res.PrunedThrough {   // :389
    res.Skipped = true; return res, nil
}
```

The watermark stores "pruned through (inclusive) = cut-1", and the skip test is a
lexicographic date comparison — fine. But the *result* depends entirely on the
`keepDays` the caller happens to pass, and `/admin/api/usage/prune`
(`internal/admin/usage_handler.go:694`) accepts an operator-supplied `keepDays`
while the daily ticker always passes `DefaultRetentionDays`.

**Verified** (`AUDIT/.scratch-data/probe_test.go`), seed one 40-day-old row:

```
prune30: RollupRows:1 DeletedRows:1 PrunedThrough:2026-09-07   (archived, correct)
prune5:  RollupRows:0 DeletedRows:0 PrunedThrough:2026-10-02   (ran, advanced watermark forward)
prune90: Skipped:true Reason:已剪到该水位，无事可做
```

**Why it matters**

No data is lost or double-counted in this sequence — the batch/aggregate/delete
invariance at `usage_archive.go:412-421` is correct, and I confirmed it. The
defect is that the watermark is **not a function of retention policy**, only of
call order. Two concrete consequences:

1. An operator who prunes with a small `keepDays` and then with a large one gets a
   `Skipped:true / "无事可做"` response while the response's own `CutoffDay`
   (`2026-07-10`) says the run should have covered 90 days. The result object
   reports `PrunedThrough:2026-10-02` — a date *later* than the cutoff it claims —
   which is incoherent to anyone reading the admin output.
2. If retention is later raised, previously-pruned details are gone and there is
   no way to restore them, and no warning that this happened.

Neither loses money — `usage_totals` (table B) is maintained by trigger and is
prune-independent, and `GetUsageLifetime` reads only that — so this is a reporting
and operability defect, not a billing one.

**Concrete fix**

Track the retention setting that produced the watermark (e.g. a
`prune_keep_days` column on `usage_totals`) and, when the operator asks for a
window *older* than the watermark, respond with an explicit
`skipped: true, reason: "retention previously ran at N days; details before
<date> are archived and not restorable"` rather than the generic
"已剪到该水位，无事可做". Separately, reject or warn on a `keep_days` that is
larger than the one already applied.

**Confidence** high (read + reproduced).

### Clean: lifetime totals survive pruning; `first_record_at` moves backwards correctly

**Verified** (`AUDIT/.scratch-data/race_test.go`): inserting a record one hour
*older* correctly lowers `usage_totals.first_record_at`
(`1791419309010` → `1791415709010`), so the `MIN(...)` in the trigger at
`usage_archive.go:157-158` behaves as documented and the "全部" range start point
is not lost.

The two-layer archive design is correct in the ways that matter:

- Table B (`usage_totals`) is trigger-maintained and prune-independent, and the
  placeholder row is seeded at `store.go:335` before the trigger exists.
- `GetUsageLifetime` (`usage_archive.go:563`) deliberately has **no**
  `scopeUserID` parameter, which structurally prevents the global row from being
  served to a non-admin.
- `reconcileUsageTotals` uses `SET = (subquery)` (overwrite) rather than
  accumulate, so a re-run after marker loss cannot double totals — and it is
  correctly skipped once a prune watermark exists.

### Clean: hot-path index coverage

`ensureUserIndexes` (`store.go:594-619`) creates `idx_usage_user_ts(user_id, ts)`,
`idx_access_keys_user(user_id)`, `idx_users_group(group_id)`,
`idx_rollup_user(user_id)`, `idx_rollup_key(access_key_id)`; the migration list
adds `idx_usage_ts`, `idx_usage_key_ts`, `idx_usage_model_ts`,
`idx_usage_prov_ts`, `idx_rollup_day`, `idx_balance_charges_user`. That covers
every hot query I traced:

- user quota precheck → `idx_usage_user_ts` (and the bare `user_id = ?` predicate
  at `usage_source.go:113-115` is deliberately not COALESCE-wrapped specifically so
  the index is usable — a genuinely correct micro-decision).
- key quota / reserve → PK lookup.
- balance precheck → PK lookup on `users.id`.
- charge idempotency → PK on `balance_charges`.
- per-key day sums → `idx_usage_key_ts` / `idx_rollup_key`.

No missing index found.

---

## Snapshot rebuild

### Clean: rebuild does NOT degrade to an empty permission table — it fails the rebuild

The known context flagged `rebuild.go` for degrading to an empty group list and
silently making all group users unlimited. **I verified this is not what the code
does**, and the comments match the implementation:

- `RebuildFromDB` returns `nil, err` on every read failure — providers
  (`:47-50`), models (`:72-75`), routes (`:92-95`), targets (`:108-111`),
  **group models (`:128-131`)**, **users (`:140-143`)**, keys (`:165-168`),
  runtime defaults (`:204-214`). There is no `if err != nil { degrade }` branch
  anywhere; every failure propagates and the caller skips the `Swap`, leaving the
  previous snapshot in force. `rebuild.go:133-139` states exactly this intent.
- The comment at `rebuild.go:203` ("读失败不致命：留 0 即全部回落 config") is
  **stale and wrong for `GetRuntimeDefaults`** — the `else` at `:212-214`
  returns `nil, err`, so it is in fact fatal. The comment is harmless (the code is
  the stricter one) but misleading to the next reader. Minor.

The deeper question — whether a *successful* rebuild can widen access — is also
answered defensively:

- `groupModelAllow` (`rebuild.go:246-255`) returns `AllowAll` only when the group
  id is empty (no group), or the group has no rows in `user_group_models` (no
  whitelist configured = unrestricted, the documented product semantics), or a
  dangling group id. Each is a deliberate case with a stated rationale; none is
  reachable via a read error, because read errors abort the whole rebuild.
- `ModelAllow`'s zero value is **restrictive** (`snapshot.go:74-96`): an
  unfilled field denies all models rather than allowing all. This is the correct
  bias and it is enforced consistently by `keyModelAllow` (`:228-233`).
- `KeySnapshot.GroupModelAllow` falls back to the owning user's resolved allow
  when `usersByID[k.UserID]` misses (`:177-179`) — commented as unreachable
  because `auth.Authenticate` returns `ErrKeyUnowned` first
  (`internal/auth/auth.go:93-102`). That reasoning is correct and I confirmed the
  auth path enforces it.

One residual note: `ListGroupModels` returns a map keyed by group id; a group with
*zero* configured models produces **no map entry**, so `ok == false` →
`AllowAll()`. This is the documented "empty whitelist = unrestricted" product
decision (`group_dao.go:18-22`), and the UI is expected to explain it. It is a
real footgun — creating a group and forgetting to tick any models *widens*
rather than restricts — but it is a consciously chosen semantic, consistently
implemented and documented at three separate call sites, not a defect.

**No fail-open in snapshot rebuild.**

---

## Resource handling

### Clean: no leaks found

Every `Query`/`QueryContext` in the DAOs is paired with `defer rows.Close()` —
verified across `key_dao.go`, `user_dao.go`, `usage_dao.go`, `group_dao.go`,
`usage_archive.go`, `model_dao.go`, `audit_dao.go`. `columnExists`
(`store.go:623-646`) closes its `PRAGMA table_info` rows and returns `rows.Err()`.

Every `BeginTx` has a `defer tx.Rollback()`, which is a no-op after `Commit` —
including the error-return paths where it *matters* (`balance_dao.go:207`, and
the two `ChargeBalance` commits discussed in the P0 above). `usageRecorder`
(`main.go:2217`) runs a single worker draining a closed channel, and `wait`
(`:2332-2343`) bounds shutdown with a context so a stuck write cannot hang
shutdown. No goroutine or handle leak identified.

---

## Notes on areas reviewed with no issues found

- **`ReserveQuota` / `ReleaseQuota` accounting** — the 2026-10-07 double-count fix
  holds. Reserve writes only `reserved_tokens`; the trigger owns `used_tokens`;
  release returns the reservation in full without an `actual-est` correction.
  Verified `used == 250` exactly, never 2×. The `MAX(0, …)` clamp on release
  prevents a negative reservation from silently widening quota. The
  `usage_state="missing"` path correctly releases the *quota* reservation while
  deliberately keeping the *TPM* one (`main.go:2062-2066`).
- **`priceUsage` is the single pricing formula** — one implementation, no copies;
  negative-token clamping happens once at the write entry point
  (`usage_dao.go:97-111`) so the trigger cannot be fed a negative that would
  permanently lower lifetime totals or make the quota check vacuously true.
  `cost_total` is frozen at insert so a price change cannot retroactively alter
  history. `charge` uses the exact variable returned by
  `CreateUsageRecordWithCost`, so "the amount deducted" and "the amount reported"
  cannot diverge.
- **`YuanToCents`** — round-half-up, negatives and zero clamped to 0. No
  systematic truncation loss (which the comment at `balance_dao.go:49-52`
  correctly identifies as the failure mode to avoid). Verified across the
  boundary cases.
- **`adjustBalance`** — `balance_cents + ?` in SQL is genuinely immune to
  lost-update; verified under concurrency. The documented NULL→finite behaviour on
  top-up is intentional and consistent.
- **NULL-vs-zero semantics** — the two are kept strictly apart everywhere I
  looked: `quota_tokens` 0 = unlimited (`key_dao.go:469`), `balance_cents` NULL =
  unlimited (`balance_dao.go:113-115`, `user_dao.go:112-116` deriving an explicit
  boolean). `BalanceOf` returns `limited=true, err` on read failure so a DB blip
  can never be misread as "unlimited". **No path conflates them**; the risk the
  brief warned about is handled.
- **fail-open vs fail-closed** — quota prechecks fail-open (logged at ERROR),
  balance precheck fails-closed (logged at ERROR), and `billing.go:570-589`
  explains at length why the asymmetry is intentional and warns against
  "unifying" it. The reasoning is sound; I found no path where a balance read
  failure results in an allow.
- **`DeleteGroup` members check** — users *and* access keys are both counted
  inside the transaction (`group_dao.go:108-150`), closing the check-then-delete
  race that would otherwise silently widen access via `ON DELETE SET NULL`.
- **`SetInitialAdminPassword`** — the bootstrap condition guard plus the
  `bootstrap_completed` marker are written in one transaction
  (`user_dao.go:287-317`), so there is no window where the password is set but
  the unauthenticated bootstrap window remains open.
- **`auth_version` fencing** — `SetUserPassword` / `BumpAuthVersion` increment in
  SQL (`user_dao.go:261-267`, `:325-330`) and `UpdateUser` deliberately never
  writes the column (`:236-255`), so a stale read-modify-write cannot roll back a
  revocation.
- **`routing.Resolve` / `buildCandidates`** — an explicitly disabled named route
  is not resurrected by the `provider/model` direct-connect fallback
  (`routing.go:180-202`), and a route with configured-but-all-disabled targets
  does not fall back to its head target (`routing.go:254-259`). Both are the
  "explicit disable always wins" rule and both hold.

---

## Summary

| Sev | Finding | Location |
| --- | --- | --- |
| P0 | Failed charge commits its idempotency placeholder; the retry is permanently swallowed | `balance_dao.go:265`, `:274` |
| P0 | `balance_charges` PK is `request_id` alone — cross-user id collision cancels another user's charge | `store.go:351`, `balance_dao.go:211` |
| P2 | 1-cent balance admits unbounded sub-cent requests (preflight + no-op charge) | `billing.go:539`, `balance_dao.go:199` |
| P2 | Usage insert and balance charge are separate transactions; a hard kill between them loses the charge | `main.go:2220-2228` |
| P2 | Prune watermark is call-order dependent, not retention-policy dependent; incoherent admin response | `usage_archive.go:375-392` |
| P2 | No referential integrity or reconciliation over the charge path amplifies every charge-path defect | `store.go:222-243`, `:350-355` |
| P3 | Key reassignment silently shifts quota attribution with no audit record | `key_dao.go:339-343` |
| P3 | Stale comment: `GetRuntimeDefaults` failure is described as non-fatal but is fatal | `rebuild.go:203` vs `:212-214` |

The two P0s share one root cause and one fix: the charge path's idempotency record
is committed independently of whether the charge succeeded. Rolling back the
placeholder on the error paths fixes the first; scoping the primary key to
`(user_id, request_id)` fixes the second. A `settled` marker on
`balance_charges` plus a reconciliation routine would prevent both from recurring
and would make any future instance detectable rather than silent.

The rest of the billing machinery is in good shape — the reservation accounting,
the atomic charge guard, the single pricing formula, the NULL/zero separation, and
the deliberate fail-closed balance gate are all correct and well-tested, and the
snapshot rebuild does **not** fail open.