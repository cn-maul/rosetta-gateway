# Rosetta Gateway Remediation Plan

Status: proposed, not executed

Source audit: `AUDIT-REPORT.md` at baseline
`e43c266c98cb8121be020fdac95dfe15fcdae2d9`

Scope: architecture-led remediation of all 54 confirmed findings and the
systemic causes behind them. This file is a plan. It does not claim that any
finding is fixed.

## Decision

Do not repair 54 symptoms one by one.

The audit findings are concentrated in eight primary root causes. Each
finding has exactly one primary cause in the ownership table below; secondary
effects are handled by the listed workstreams.

- **RC1 - State has no authoritative owner.** Request commitment, settlement,
  runtime readiness, operation results, and UI entity state are inferred from
  scattered flags or local state.
- **RC2 - Failure becomes success by omission.** A read, write, encode,
  discovery, or bootstrap error is logged, ignored, defaulted, or reported as
  empty/zero. Cleanup and worker-liveness failures that cross a lifecycle or
  transaction boundary belong to RC3.
- **RC3 - A business operation has no closed transaction or recovery
  boundary.** Bootstrap, charge settlement, cost recomputation, quota release,
  worker shutdown, and archival update related state in separate steps.
- **RC4 - The same semantic rule has multiple implementations.** Pricing
  fallback, credential precedence, timezone precedence, input estimation,
  and retention/documentation wording differ by caller or document.
- **RC5 - The protocol domain model is incomplete.** Media, text plus tool
  calls, terminal-only streams, malformed choices, and diagnostic capability
  lists are handled per adapter, creating cross-protocol gaps.
- **RC6 - Validation or authorization is not enforced at the authoritative
  layer.** Presence-sensitive defaults, explicit query parameters, command
  validation, identity canonicalization, and allowlist narrowing leak through
  a handler, DAO, or caller.
- **RC7 - Component interaction contracts are incomplete.** Callers can
  produce dangling ARIA relationships, and pointer-only interaction has no
  equivalent keyboard or textual path.
- **RC8 - Delivery and operator contracts are not verified as one boundary.**
  CI, release tags, launchers, test classes, lifecycle budgets, and
  documentation can disagree with the artifact or runtime.

These are not eight independent rewrites. They share one architectural rule:
the module that owns a decision must also own the state and result that make
that decision observable and enforceable.

The remediation therefore changes ownership and contracts first, then updates
features inside the new boundaries. A feature fix that does not fit the target
boundary is temporary containment, not the final design.

## Success Criteria

The remediation is complete only when all of the following are true:

- The workstream dependency graph is respected, and no phase starts before
  its hard prerequisites have acceptance evidence.
- All 54 confirmed findings are either fixed or have an explicit, accepted
  disposition with evidence of why they are no longer applicable.
- Every externally visible operation has one explicit success, failure, and
  terminal state. No error path reports success or "unknown" as zero/empty.
- Every multi-table financial or bootstrap operation has one transaction
  boundary or one durable, idempotent resumable state machine.
- Each business rule has one implementation and one contract test.
- The architecture gate enforces the target dependency directions, not only
  the current package bans.
- Shutdown joins every background worker and writer before closing the store.
- Protocol adapters preserve the complete supported request/response model,
  and terminal streams have protocol-valid event sequences.
- Frontend async views reject stale responses and distinguish loading,
  empty, error, and ready states.
- CI, release, launcher, and operator documentation use the same lifecycle and
  test-classification contracts as the code.
- The final state passes build, vet, unit, race, frontend, embedding,
  architectural, and targeted failure-injection checks on the supported
  environments.
- The rollback/risk register has a concrete verification or operational
  containment for every phase.

## Target Architecture

The target is a small set of deep modules with explicit contracts. Names are
proposed; the responsibilities are the decision.

| Module | Owns | Must not own |
|---|---|---|
| `platform/lifecycle` | startup phases, readiness, shutdown budget, worker ownership, join barriers | HTTP handlers, business rules |
| `platform/config` | decoding, defaults, cross-field validation, effective precedence | runtime snapshots, admin HTTP |
| `control/identity` | canonical principal identity, session claims, throttle keys, revocation | admin page state, provider selection |
| `control/catalog` | provider/model/route commands, effective configuration, bootstrap transaction | request transport, usage records |
| `control/billing` | price resolution, preflight estimate, reservation contract, charge commands | HTTP encoding, SQL transactions |
| `control/accounting` | usage facts, pending settlement, idempotency, reconciliation, retention eligibility | protocol conversion, frontend formatting |
| `control/requestlane` | admission, candidate policy, attempt lifecycle, commit classification, accounting handoff | persistence internals, protocol wire encoding, frontend state |
| `data/protocol` | one complete Rosetta model boundary, stream lifecycle, error taxonomy | billing, persistence, admin UI |
| `data/transport` | upstream client, credential resolution, discovery, balance, circuit/probe state | usage charging, HTTP response encoding |
| `data/persistence` | DAO primitives, schemas, transaction helpers, query services | HTTP, lifecycle, business orchestration, multi-entity command decisions |
| `surface/http` | middleware, route binding, status mapping, request limits | domain decisions duplicated from control modules |
| `surface/admin` | command/query adapters, validation, authorization binding | direct cross-entity transactions |
| `surface/web` | API client, route state, view models, user-visible error states | business-rule reimplementation |
| `delivery` | image, launcher, embedding, workflows, release invariants | runtime business logic |

### Dependency rule

Dependencies flow inward:

```text
surface -> control -> data contracts
control/requestlane -> control/billing + control/accounting + data contracts
delivery -> build artifacts + public health/version contracts
platform -> no business or HTTP dependency
```

`control` modules may depend on narrow interfaces implemented by `data`.
They may also depend on other `control` contracts, but not on another control
module's persistence or HTTP adapter. `data` may not call upward into
`control`, `surface`, or `delivery`. A module that needs to notify another
after a state change emits a typed result/event; it does not reach into the
other module's storage or HTTP layer.

`control/requestlane` belongs above `billing` and `accounting` because it must
invoke their commands. It is not a data-layer package merely because it
handles request traffic.

`control/catalog` is an administrative control module, not a prerequisite for
the request data path. It owns provider/model/route commands and bootstrap
graph assembly; transport and requestlane consume immutable runtime
projections and must not call catalog commands during request handling.

The projected public package layout is a target shape, not a mandatory
filesystem rewrite in every phase. A current package keeps its path until its
owned responsibility and dependencies match the target. This avoids a large
rename-only change and preserves working call sites during the migration.

### Ownership rules

1. The request lane owns attempt commitment, but the accounting module owns
   the final billable outcome.
2. A settlement command owns all wallet, pending, usage, and marker writes in
   one transaction. Reconciliation only invokes that command and projects its
   result.
3. The lifecycle manager owns the order and deadline of every worker and the
   store-close barrier. It does not infer shutdown completion from a timeout.
4. The configuration loader resolves precedence once and publishes an
   immutable effective configuration. Runtime and admin code do not re-read
   environment/config precedence independently.
5. The protocol layer translates a complete supported model or returns an
   explicit unsupported/invalid error. It never silently drops a supported
   part.
6. Frontend request state is keyed by the entity and request generation that
   produced it. A response that no longer matches is discarded.

## Finding-to-Root-Cause Map

This map is the contract for remediation. A finding may appear under more than
one contributing cause, but its **primary fix owner** is listed once.

| Root cause | Findings | Primary owner |
|---|---|---|
| RC1 - State has no authoritative owner | M01-01, M01-02, M05-01, M10-02, M15-02, M15-03, M16-01, M16-02, M16-03, M17-02 | lifecycle, requestlane, identity, web |
| RC2 - Failure becomes success by omission | M02-02, M06-01, M11-01, M15-01 | config, transport, HTTP, API client |
| RC3 - No closed transaction/recovery boundary | M01-03, M01-04, M02-04, M07-01, M08-01, M08-02, M09-01, M14-01 | persistence, accounting, lifecycle |
| RC4 - Same semantic rule has multiple implementations | M02-01, M03-03, M03-04, M06-02, M14-02, M20-02, M20-03 | config, billing, protocol, transport, surface |
| RC5 - Incomplete protocol domain model | M03-01, M03-02, M03-05, M04-01, M04-02 | protocol |
| RC6 - Validation/authorization not at authoritative layer | M02-03, M08-03, M09-02, M10-01, M12-01, M12-02, M12-03, M13-01 | config, identity, command services, query services |
| RC7 - Incomplete component interaction contract | M17-01, M17-03 | web components |
| RC8 - Delivery/operator contract not verified | M00-01, M00-02, M00-03, M18-01, M18-02, M18-03, M18-04, M19-01, M20-01, M20-04 | delivery, lifecycle |

### Per-finding ownership

| Finding | Primary `RC` | Workstream |
|---|---|---|
| `M00-01` | RC8 | R12 |
| `M00-02` | RC8 | R12 |
| `M00-03` | RC8 | R12 |
| `M01-01` | RC1 | R0, R1 |
| `M01-02` | RC1 | R0, R1 |
| `M01-03` | RC3 | R1 |
| `M01-04` | RC3 | R1 |
| `M02-01` | RC4 | R2, R6 |
| `M02-02` | RC2 | R2 |
| `M02-03` | RC6 | R2 |
| `M02-04` | RC3 | R7 |
| `M03-01` | RC5 | R4 |
| `M03-02` | RC5 | R4 |
| `M03-03` | RC4 | R4, R5 |
| `M03-04` | RC4 | R4 |
| `M03-05` | RC5 | R4 |
| `M04-01` | RC5 | R4 |
| `M04-02` | RC5 | R4 |
| `M05-01` | RC1 | R0, R5 |
| `M06-01` | RC2 | R6 |
| `M06-02` | RC4 | R6 |
| `M07-01` | RC3 | R3, R7 |
| `M08-01` | RC3 | R0, R3 |
| `M08-02` | RC3 | R3 |
| `M08-03` | RC6 | R9 |
| `M09-01` | RC3 | R3 |
| `M09-02` | RC6 | R2 |
| `M10-01` | RC6 | R0, R8 |
| `M10-02` | RC1 | R8 |
| `M11-01` | RC2 | R9 |
| `M12-01` | RC6 | R9 |
| `M12-02` | RC6 | R7 |
| `M12-03` | RC6 | R8 |
| `M13-01` | RC6 | R9 |
| `M14-01` | RC3 | R3 |
| `M14-02` | RC4 | R3 |
| `M15-01` | RC2 | R9, R10 |
| `M15-02` | RC1 | R9, R10 |
| `M15-03` | RC1 | R9, R10 |
| `M16-01` | RC1 | R10 |
| `M16-02` | RC1 | R10 |
| `M16-03` | RC1 | R10 |
| `M17-01` | RC7 | R11 |
| `M17-02` | RC1 | R10 |
| `M17-03` | RC7 | R11 |
| `M18-01` | RC8 | R12 |
| `M18-02` | RC8 | R12 |
| `M18-03` | RC8 | R12 |
| `M18-04` | RC8 | R12 |
| `M19-01` | RC8 | R12 |
| `M20-01` | RC8 | R1, R12 |
| `M20-02` | RC4 | R2, R12 |
| `M20-03` | RC4 | R2, R12 |
| `M20-04` | RC8 | R12 |

### Mapping checks

- All 54 confirmed findings appear exactly once as a primary owner in the
  per-finding table.
- The eight primary root causes cover all 54 findings.
- The per-finding table is the sole ownership authority. Workstream findings
  lists are implementation touchpoints; a finding may appear in an early
  containment phase and a later structural phase without creating a second
  owner.
- Documentation-only findings stay attached to the implementation contract
  they describe; they do not create a documentation subsystem.

## Workstreams

### R0 - Safety stop and baseline truth

Purpose: stop ongoing integrity/security loss immediately while introducing
the shared result vocabulary that later phases will extend. R0 does not
implement the full lifecycle manager or accounting state machine.

Findings: M01-01, M01-02, M05-01, M08-01, M10-01.

Work:

1. Freeze independent debit writes behind one minimal settlement entry point.
   Give it a request-scoped idempotency key and route pending settlement and
   recomputation through it. R3 later moves this command to its final
   accounting state machine; the temporary entry point must share the same
   marker contract, not create a second one.
2. Canonicalize account identity before both password lookup and throttle
   keying. Store and query throttle state under the same normalized identity.
3. Make first-run admin bootstrap return an error on non-unique read/write
   failures. Persist an explicit bootstrap state so startup can distinguish
   "not needed", "incomplete", and "ready".
4. Make the initial runtime rebuild produce the same typed convergence state
   as a failed admin reload. R0 exposes the state; R1 owns the full lifecycle
   and readiness projection.
5. Give the non-stream write result a typed commit outcome. Only a complete
   downstream write may become billable success.

Exit checks:

- A pending charge followed by recomputation produces one debit and one
  completion marker.
- Case variants of one account share one failure budget.
- A failed initial admin/bootstrap and a failed initial rebuild both prevent a
  healthy readiness result.
- A downstream write failure is recorded as a client-side failure and is not
  charged as a successful response.
- No broad refactor is mixed into this safety tranche. R0 adapters must have a
  named deletion point in R1/R3.

#### R0 Detailed Technical Design

**Delivery position.** R0 is a containment gate, not a partial implementation
of R1, R3, R5, or R8. It must stop the five integrity/security failures using
the smallest authoritative contract available in the current code. Every R0
adapter has a named final owner and deletion point below.

**Schema and compatibility rule.** R0 introduces no schema migration and no
new accounting table. It uses the existing `balance_charges` row keyed by the
original `(request_id, user_id)` as the compatibility completion marker. The
marker's presence means the request has passed through a committed charge or
settlement path. `amount_cents` remains informational for R0 and must not be
used to infer whether a different usage row was charged. R3 replaces this
presence-based compatibility contract with the authoritative accounting state
machine.

**Ordered implementation slices.** The slices are independently reviewable
and revertible, but R0 is not complete until all five are present.

| Slice | Findings | Contract introduced | Merge dependency |
|---|---|---|---|
| R0-A | M08-01 | canonical completion marker for pending settlement and recomputation | none |
| R0-B | M10-01 | one ASCII-compatible canonical login throttle key | none |
| R0-C | M01-02 | typed bootstrap outcome with fatal infrastructure failures | none |
| R0-D | M01-01 | failed initial rebuild enters the existing dirty-retry state | none |
| R0-E | M05-01 | non-stream encoding/write failure is observable and terminal | R0-D shared outcome vocabulary |

R0-A is first because it stops silent monetary duplication. R0-B and R0-C
are independent. R0-D establishes the bootstrap/runtime truth needed by the
Phase 1 readiness evidence. R0-E is last because it changes the ingress codec
interface and must be verified across all three protocols.

##### M08-01 - One canonical completion marker for every usage debit

**Root cause and control objective.** Pending settlement and recomputation
perform a wallet debit but do not create the marker on which reconciliation
relies. The control objective is that every committed debit attributable to a
usage request has one canonical `(request_id, user_id)` marker in the same
transaction as the debit.

**Current broken invariant.** Normal charging creates a `balance_charges`
reservation before debit. Pending settlement deducts a lump sum from
`balance_pending` and deletes those pending rows without creating the original
marker. `RecomputeCost` uses a `recompute:<record_id>:<cost_micros>` key for
delta idempotency but does not create the original marker.
`FindUnchargedUsage` joins only the original marker, so either recovery path
can be selected again and charged a second time.

**Proposed contract.** Add one store-private transaction helper:

```text
ensureUsageCompletionTx(ctx, tx, requestID, userID) error
```

The helper inserts the canonical `(requestID, userID)` row into
`balance_charges` with `INSERT OR IGNORE`. It is intentionally not a new debit
primitive and does not replace the existing recomputation idempotency key. It
must:

- reject empty canonical identifiers only when a caller is trying to mark a
  usage-linked debit; non-usage internal pending keys keep their existing
  behavior;
- propagate every SQL error, including marker insertion and affected-row
  readback failures;
- execute on the same transaction as the wallet debit and the pending-row or
  usage-row mutation;
- be idempotent when the marker already exists.

**Pending settlement rules.** `settlePendingForUser` remains the single
settlement transaction. For every pending row whose debit is included, fully
or partially, it must:

1. determine the amount that can actually be absorbed by the available
   balance;
2. call `ensureUsageCompletionTx` with that row's original request ID and user
   ID;
3. delete the fully settled row or reduce the partially settled row only after
   the marker operation succeeds;
4. commit all three effects together.

If a contradictory canonical marker already exists for a still-present pending
row, R0 must not debit that row again. It preserves the pending anomaly for
R3/operator inspection rather than silently deleting evidence. This is a
containment preference for under-collection over double collection, not the
final accounting policy.

**Recomputation rules.** `RecomputeCost` keeps
`recompute:<record_id>:<cost_micros>` for delta idempotency. After a
recomputation debit succeeds, it calls `ensureUsageCompletionTx` with the
record's original request ID and user ID in the same transaction that updates
`usage_records`. A marker failure aborts the usage update, the recompute
charge, and the cumulative update together.

**Compatibility and failure semantics.** Existing historical
`balance_charges` rows remain valid. The reconciliation query continues to
join on the original marker and requires no migration. Insufficient balance
still rolls back the recomputation; the row remains `cost_pending`. No
settlement path may report success after a marker failure. R3 owns the final
state machine, amount-per-line semantics, and repair of contradictory legacy
rows.

##### M10-01 - Canonical login throttle identity

**Root cause and control objective.** Account lookup and uniqueness are
case-insensitive under SQLite `NOCASE`, while the account throttle uses the
trimmed username verbatim. The control objective is one throttle key for every
spelling that resolves to the same database account.

**Proposed contract.** Add one login-boundary helper:

```text
canonicalLoginThrottleKey(rawUsername string) string
```

It trims surrounding whitespace and folds ASCII `A-Z` to `a-z` only. It must
not use general Unicode case folding because SQLite `NOCASE` does not implement
Unicode case semantics. The canonical key is computed once at the start of the
login request and reused for:

- `GetUserByUsername`;
- account-throttle `Allow`;
- deferred `Release`;
- every `Fail`;
- `Success`.

The IP throttle remains separate and continues to use the source address.
The throttle implementation does not change. No Unicode normalization,
username alias merging, or new identity store is introduced in R0.

**Failure semantics.** A failed login under any ASCII case spelling increments
the same account budget. A successful login under any spelling resets the same
key. R8 replaces this login-local helper with the authoritative principal
identity contract and must preserve the cross-IP regression case.

##### M01-02 - Bootstrap failures must become startup truth

**Root cause and control objective.** `initializeAdminBootstrap` always
returns `nil`, even when the user count cannot be read or the initial admin
cannot be created. The control objective is that unknown or failed bootstrap
state cannot be reported as a successful start.

**Proposed outcome contract.** The bootstrap implementation returns one of
these derived states; no new table is required because the existing users,
pending-admin state, and `bootstrap_completed` marker are already durable:

- `setup_required`: the database is readable and either has no users or has the
  expected uninitialized administrator;
- `ready`: the database is readable and no bootstrap setup is pending;
- `failed`: the state cannot be determined, or a required write failed for any
  reason other than a verified concurrent creation.

`CountUsers` failure is `failed` and is returned to `gatewayapp.Run`.
After a zero count, `CreateUser` failure is `failed` unless it is a unique
conflict followed by a successful re-read proving that another starter created
the account. A unique conflict with a failed or still-zero re-read is
`failed`; it is not accepted on the strength of the original error code.

`initializeAdminBootstrap` maps `failed` to a non-nil error so the existing
"refuse to start" path becomes reachable. A first-run `setup_required` result
is not an infrastructure error: startup continues and the first-run password
endpoint remains available. A failure to read
`FindUninitializedAdmin` after bootstrap is also fatal for R0 because the
required identity state is unknown.

**Failure semantics.** No service start after an unverified bootstrap write.
No service start after an unreadable bootstrap state. No silent success for a
concurrent unique conflict until a re-read confirms the account. R1 consumes
the typed outcome as a readiness dimension.

##### M01-01 - Initial runtime rebuild failure must enter retry and degrade health

**Root cause and control objective.** The initial `Reload` failure falls back
to configuration without calling `MarkDirty`. `WatchRetry` therefore remains
in its clean branch and `/healthz` can report healthy while runtime state has
not converged. The control objective is that initial and later rebuild
failures use the same convergence state.

**Proposed contract.** On any non-nil initial `Reload` result, call
`MarkDirty` before attempting the configuration fallback. The fallback remains
available so a previously working deployment can continue serving, but it is
explicitly degraded state. `WatchRetry` then retries through the existing
backoff path. A successful retry calls `markClean`, which restores health.

If the configuration fallback also fails, startup may still choose its current
serving policy, but the reloader remains dirty and health remains degraded.
R0 does not add a second health model or claim that the fallback equals
database convergence.

**Compatibility and failure semantics.** `/healthz` behavior is unchanged
except that it now sees the correct dirty state: HTTP 503 with
`runtime_lags_behind_database` until convergence. R1 replaces the dirty-bit
projection with typed lifecycle readiness.

##### M05-01 - Non-stream response write failure must be terminal and non-billable

**Root cause and control objective.** `Codec.WriteNonStream` returns no
result. The three outwire writers ignore or only log write errors, and
`attemptNonStream` unconditionally records HTTP 200, `UsageStatusOK`, success,
and a chargeable outcome. The control objective is that only a complete
non-stream downstream write can become billable success.

**Proposed contract.** Change the ingress boundary to return a typed non-stream
write result:

```text
NonStreamWriteResult {
    Committed bool
    Err       error
}
```

All three outwire encoders return that result. Each writer first builds and
marshals its complete response payload, then writes the protocol headers and
payload. This separates:

- encode failure before any downstream bytes: `Committed=false`;
- a write failure after headers or partial bytes: `Committed=true`, with a
  non-nil error that preserves the underlying write error.

`attemptNonStream` treats any error as terminal: it must not fail over after an
upstream response has returned, because retrying would risk duplicate upstream
work and an inconsistent client-visible result. It reuses the stream path's
downstream-write classifier and records:

- `UsageStatusCanceled`;
- error code `downstream_write_failed`;
- the safely formatted write detail;
- no monetary charge;
- no credential or target failure penalty;
- the same quota/token finalization rule as a committed stream cancellation
  with known usage.

If encoding fails before commit, R0 may render the protocol's error envelope
with HTTP 500 where no headers have been written. That refinement does not
change the non-billable, non-penalized outcome. Successful writes keep their
existing HTTP status, body shape, and usage accounting.

**Compatibility and failure semantics.** Stream behavior is unchanged.
Non-stream clients still receive the protocol-native success body on success.
A downstream failure after upstream completion is recorded as a client-side
terminal failure rather than an upstream failure. R5 replaces the local
classifier with the requestlane commitment model.

#### R0 Acceptance Matrix

These are implementation acceptance cases, not tests executed by this
planning pass. Every case must be captured first as a failing Phase 0
regression fixture. R0 implementation is accepted only when all 15 cases pass
on a test database.

| ID | Finding | Fixture and action | Required observations |
|---|---|---|---|
| `R0-AC-01` | M08-01 | Charge an expensive usage while the balance is insufficient, verify a `balance_pending` row, top up enough to settle it, then run reconciliation. | The wallet is debited once for the original debt, the pending row is cleared, exactly one original `(request_id,user_id)` marker exists, and reconciliation selects no row. |
| `R0-AC-02` | M08-01 | Store a usage with `cost_pending`, make pricing available, run `RecomputeCost`, then run reconciliation. | The usage cost is updated, the delta is debited once, the recompute key remains idempotent, the original marker exists, and reconciliation selects no row. |
| `R0-AC-03` | M08-01 | Make canonical marker insertion fail inside the settlement/recompute transaction. | The associated wallet debit, pending deletion or reduction, usage update, and cumulative update all roll back; no success is returned. |
| `R0-AC-04` | M08-01 | Run pending settlement and reconciliation concurrently for the same request, then repeat both. | The final wallet delta equals one charge, the marker count is one, and neither operation reports a second successful debit. |
| `R0-AC-05` | M10-01 | Send account-password failures for `alice`, `Alice`, and `ALICE` across different source IPs. | The sixth logical attempt is rejected by the shared account budget even though the IP budgets have room. |
| `R0-AC-06` | M10-01 | Accumulate four failures under mixed case, log in successfully with another case variant, then fail again. | The successful login resets the canonical account key; the next failure starts a fresh budget rather than immediately returning 429. |
| `R0-AC-07` | M01-02 | Inject a `CountUsers` or post-bootstrap `FindUninitializedAdmin` read failure during startup. | Startup returns an error and does not listen; the failure identifies bootstrap state as unknown. |
| `R0-AC-08` | M01-02 | Inject a non-unique admin `CreateUser` failure on an empty users table. | Bootstrap returns an error, startup refuses to serve, and no false ready state is recorded. |
| `R0-AC-09` | M01-02 | Race two starters so one `CreateUser` returns a unique conflict while the other creates the admin. | The losing starter re-reads successfully, observes the created admin, and returns verified `ready`/no-op rather than an unverified success or a false failure. |
| `R0-AC-10` | M01-01 | Fail the initial runtime reload and successfully build the configuration fallback. | The reloader is dirty, the retry loop is active, and `/healthz` returns 503 `runtime_lags_behind_database`. |
| `R0-AC-11` | M01-01 | After `R0-AC-10`, allow the retrying reload to succeed. | The dirty state clears and `/healthz` returns 200 with the existing healthy shape. |
| `R0-AC-12` | M05-01 | For Chat, Responses, and Anthropic, use a `ResponseWriter` that fails on write and a failing payload writer where applicable. | Each codec returns a non-nil typed result; no encoder logs and returns as if the response completed. |
| `R0-AC-13` | M05-01 | Complete the upstream call, then fail the non-stream downstream write for each protocol. | The attempt is terminal, usage is `canceled` with `downstream_write_failed`, no billable charge/marker is created, and the client-visible status is truthful for whether headers were sent. |
| `R0-AC-14` | M05-01 | Repeat `R0-AC-13` while observing target circuit and credential health. | The target and credential receive no failure penalty, and the route trace records a committed client-side failure rather than an upstream failure. |
| `R0-AC-15` | M05-01 | Run the existing successful non-stream matrix for all three protocols. | Status, headers, body shape, usage status, token fields, and billing behavior remain unchanged from the baseline. |

#### R0 Handoff and Deletion Points

| R0 artifact | Final owner | Deletion or replacement point | Closure evidence |
|---|---|---|---|
| Canonical `ensureUsageCompletionTx` compatibility helper | R3 accounting state machine | Delete when R3's single settlement command owns usage, wallet, pending, and canonical completion state; retain historical markers for reconciliation. | `R0-AC-01` through `R0-AC-04` plus R3 migration tests. |
| Login-local `canonicalLoginThrottleKey` | R8 identity model | Replace with the principal canonical identity API; keep the ASCII/`NOCASE` compatibility test until the database contract changes. | `R0-AC-05`, `R0-AC-06`, and R8 cross-IP tests. |
| Bootstrap outcome projection | R1 lifecycle manager | Consume as a readiness dimension; remove any duplicate readiness inference from startup and HTTP. | `R0-AC-07` through `R0-AC-09` plus R1 startup failure injection. |
| Dirty-on-initial-reload adapter | R1 lifecycle manager | Replace the dirty-bit projection with typed lifecycle readiness after all startup dimensions are represented. | `R0-AC-10`, `R0-AC-11`, and R1 readiness tests. |
| `NonStreamWriteResult` and local classifier | R5 requestlane | Replace with the requestlane commitment/outcome contract; keep protocol writers returning errors. | `R0-AC-12` through `R0-AC-15` plus R5 protocol matrix. |

R0 must not be marked complete on the strength of code presence. Phase 1
acceptance requires the failure-first fixtures, the 15 observations above,
and a successful existing happy-path matrix to prove that containment did not
change the public success contract.

### R1 - Establish lifecycle and worker ownership

Purpose: make startup, readiness, and shutdown one state machine.

Findings: M01-01, M01-02, M01-03, M01-04, M20-01.

Work:

1. Introduce or extend a lifecycle manager with explicit phases: load
   configuration, open storage, prepare runtime, start workers, accept
   traffic, stop admission, stop workers, drain writers, close storage.
2. Register every long-lived worker with the manager. Cancellation must be
   context-aware, and the manager must join each worker before allowing the
   next destructive phase.
3. Replace timeout-only waits with a two-part contract: a deadline and an
   observable "not joined" state. A timed-out shutdown must not silently close
   the store under a live writer.
4. Represent readiness as a typed result with database, runtime, and
   bootstrap dimensions. `/healthz` projects that result; it does not infer
   health from a dirty bit.
5. Define one shutdown-budget source in code and use it for the launcher,
   Compose grace period, logs, and documentation. The budget must state which
   waits are nominal and which can overrun.

Exit checks:

- Startup failure injection covers initial rebuild, admin insert, bootstrap
  read, and worker start failure.
- Shutdown tests prove probe and usage workers cannot access a closed store.
- Race tests pass with an intentionally slow writer.
- The documented budget and the actual configured limits are compared by a
  test or generated check.

### R2 - Centralize configuration and effective policy

Purpose: one resolved configuration and one precedence rule.

Findings: M02-01, M02-02, M02-03, M09-02, M20-02, M20-03; R12 verification:
M20-02, M20-03.

Work:

1. Split configuration into raw decoding, presence-aware defaults, normalized
   effective values, and validation. `*bool` or explicit presence tracking is
   allowed at the raw boundary; ambiguity must not escape it.
2. Resolve credential precedence (`api_key` versus environment) in one
   function used by bootstrap and runtime fallback.
3. Resolve timezone precedence in one function: non-empty config timezone is
   final; `TZ` is the fallback. Generate or test the corresponding deployment
   documentation text.
4. Treat corrupt optional settings as a typed degraded configuration, never as
   a silent zero. Consumers either use a documented default or reject the
   rebuild with a visible reason.
5. Make retention and audit pruning policy explicit: usage detail retention
   is configurable; throughput window is fixed; `audit_log_keep_rows=0`
   means disable pruning and must not be normalized to a retention default.

Exit checks:

- Table-driven tests cover missing, partial, explicit zero, explicit false,
  corrupt JSON, and precedence combinations.
- Effective config is immutable after publication.
- Deployment docs are checked against the same precedence/retention constants
  where practical.

### R3 - Make accounting a state machine, not scattered writes

Purpose: settle money and quota once, recoverably, and without starvation.

Findings: M07-01, M08-01, M08-02, M09-01, M14-01, M14-02.

Work:

1. Define an accounting record with explicit states: reserved, usage-known,
   priced, charged, pending, recomputed, archived, failed-terminal. Define
   legal transitions and the idempotency key for each transaction.
2. Put wallet debit, pending ledger update, usage marker, and charge marker in
   one settlement transaction. Every settlement path uses the same command.
3. Reserve and release all quota tokens in one owned object. Batch abandonment
   and shutdown release iterate the complete reservation set.
4. Make retention operate only on terminal records. Pending, uncharged, or
   recomputable records remain eligible for recovery or have a durable
   quarantine state.
5. Make reconciliation page over terminaable work without a permanently
   unaffordable head blocking later rows. Each failure has a retry class and a
   terminal/manual-review class.
6. Resolve actual and preflight prices through one effective-price function.
   Preserve an explicitly unpriced-model state instead of treating it as zero.

Exit checks:

- Property/invariant tests prove no record can be charged twice and no debit
  lacks a completion marker.
- A full lifecycle test covers price missing, pending, top-up, recompute,
  archive cutoff, uncharged recovery, and shutdown.
- Quota reservation tests cover partial batch failure and all-item release.
- Reconciliation progress is measurable with a permanently failing row in the
  queue.

### R4 - Establish one protocol model and stream lifecycle

Purpose: preserve supported content and make every stream protocol-valid.

Findings: M03-01, M03-02, M03-03, M03-04, M03-05, M04-01, M04-02.

Work:

1. Define the gateway's complete normalized request/response contract:
   text, image, audio, file/document, tool call, tool result, thinking,
   reasoning, refusal, usage, finish reason, and unsupported-part policy.
2. Make every inbound codec either map each supported part or return an
   explicit 4xx. Remove silent ignores and empty-message fallbacks.
3. Make estimation consume the same normalized media/text model as encoding.
   Reserve output after route resolution using one chain-level ceiling rule.
4. Make every outbound sink own a small state machine with `not started`,
   `started`, `content`, `terminal`, and `failed`. `Finish` must ensure a valid
   empty stream when no content arrived.
5. Preserve all valid Chat response content; never replace non-empty text with
   `null` merely because tool calls exist.
6. Centralize error taxonomy and status mapping so HTTP, protocol, failover,
   credential cooldown, and user-visible details share one classification.
7. Derive supported-tool error text from the validation table, not a manually
   duplicated list.

Exit checks:

- Cross-product tests: each inbound protocol to each outbound protocol for
  text+tool, media, document, empty, terminal-only, malformed, and usage-missing
  cases.
- Every normalized part has one explicit mapping or rejection test.
- No protocol test can pass by relying on a zero value that means "dropped".

### R5 - Separate request orchestration from accounting

Purpose: one request lane and one explicit ownership handoff.

Findings: M03-03, M05-01, M14-02.

Work:

1. Split request handling into admission, candidate selection, attempt,
   downstream commit, and accounting handoff.
2. Return typed attempt results (`not committed`, `committed-success`,
   `committed-partial`, `client-aborted`, `upstream-failed`) instead of
   booleans whose combinations are interpreted differently by callers.
3. Let the accounting module decide billability from the committed outcome
   and policy. The request lane must not synthesize `OK/200` before encoding
   has completed.
4. Resolve balance preflight against the exact attempt set policy. If the
   product intentionally estimates the full chain, encode that policy in one
   function and test disabled failover and target filtering explicitly.
5. Make stream and non-stream paths share the same commitment rules and
   failure attribution.

Exit checks:

- A table test drives every early return and proves reservation release,
  usage state, and billability.
- Non-stream write failure, stream disconnect, terminal-only empty, malformed
  upstream, and upstream timeout produce distinct, stable outcomes.
- Route trace and usage records are projections of the same attempt events.

### R6 - Make transport discovery/balance contracts truthful

Purpose: no invented zero, no protocol guess leaking into authentication.

Findings: M02-01, M06-01, M06-02.

Work:

1. Model balance as `known`, `unknown`, or `error` with a source and timestamp.
   An unavailable usage lookup must not become a zero used amount.
2. Resolve the effective provider protocol before discovery and pass the
   resolved protocol into authentication selection.
3. Resolve credentials through the shared effective-credential function.
4. Keep discovery and auxiliary response size limits adjacent to the client
   contract; do not silently accept unbounded auxiliary data.

Exit checks:

- Balance tests cover success, partial failure, malformed JSON, timeout, and
  unsupported provider without reporting a precise false balance.
- Auto-detected Anthropic discovery and explicit Anthropic discovery send the
  same required headers.

### R7 - Consolidate persistence commands and transactions

Purpose: DAO primitives stay low-level; business operations move up.

Findings: M02-04, M07-01, M08-01, M08-02, M09-01, M12-02.

Work:

1. Keep `data/persistence` responsible for schema, typed rows, transaction
   helpers, and single-entity DAO operations.
2. Move multi-entity workflows into command services: bootstrap graph,
   settlement, quota lifecycle, configuration import/dry-run, and usage
   archival.
3. Make bootstrap a single transaction or a durable idempotent bootstrap job
   with an explicit completion marker. A non-empty provider table is not proof
   of complete bootstrap.
4. Make all command services return typed results with committed/not
   committed status and machine-readable error classes.
5. Add optimistic or compare-and-set semantics where an admin update reads
   then writes a related table.

Exit checks:

- No command service performs a separate transaction per child when failure
  requires all-or-nothing behavior.
- Bootstrap tests inject failure at every child write and prove no partial
  published graph.
- Configuration dry-run and real import share one model cardinality function.

### R8 - Unify identity and authorization invariants

Purpose: one canonical principal and one authorization decision path.

Findings: M10-01, M10-02, M12-03.

Work:

1. Normalize usernames before credential lookup, throttle keying, and stored
   uniqueness checks.
2. Define session/throttle state ownership. Successful authentication,
   logout, revocation, and idle expiry must all release or expire the same
   state.
3. Represent model allowlists as an authorization capability with an owner and
   a monotonic narrowing rule. The key-owner path cannot remove an
   administrator-applied restriction by omission.
4. Keep the real method-plus-route authorization matrix authoritative and add
   a test for every allowlist mutation path.

Exit checks:

- Case/whitespace variants of one identity share lockout and permission
  behavior.
- Idle throttle maps have a bounded removal path.
- An owner update can only narrow a key restriction unless an explicit
  admin-authorized widening command is used.

### R9 - Make HTTP/admin edges contract-driven

Purpose: one request-size policy, one error envelope, one validation layer.

Findings: M08-03, M11-01, M12-01, M13-01, M15-01, M15-02, M15-03.

Work:

1. Centralize body decompression/limit/JSON decoding so data-plane and admin
   paths distinguish 413, 400, and 415 consistently.
2. Make explicit query parameters either valid or a typed 4xx; never silently
   substitute a default for a malformed value.
3. Put entity validation in the command service, not only in one handler.
4. Separate authentication result from profile/bootstrap refresh. A successful
   password change or login must remain observable even if `/me` subsequently
   fails.
5. Make the frontend API client parse the public error envelope without
   clearing unrelated session state.

Exit checks:

- A shared HTTP-edge table test covers body limit, malformed JSON, unsupported
  media type, invalid query, and authorization failure.
- Login, bootstrap setup, and login-follow-up profile failure produce distinct
  UI states.

### R10 - Make frontend async ownership explicit

Purpose: no stale response may be rendered as current state.

Findings: M15-01, M15-02, M15-03, M16-01, M16-02, M16-03, M17-02.

Work:

1. Introduce one request-state helper keyed by entity id and a monotonically
   increasing generation. Stale responses and stale loading completions are
   discarded.
2. Clear or scope all entity state on route-key change, especially balance,
   credentials, models, and detail drawers.
3. Distinguish `not loaded`, `loading`, `empty`, `error`, and `ready` in view
   models. Do not render "no models" before loading.
4. Make ModelPicker selection operations set semantics, not visible-array
   replacement. Filtering changes visibility only.
5. Make the API client own session transport, and make view components own
   user-facing state transitions.

Exit checks:

- Two out-of-order responses cannot overwrite current entity state.
- Direct navigation to each lazy tab triggers its required load exactly once.
- Hidden selections survive search-and-select-all.
- Login and setup failure states have stable component tests.

### R11 - Accessibility as component contract

Purpose: components provide complete interaction semantics to all callers.

Findings: M17-01, M17-03.

Work:

1. Make Tabs expose or render matching tab/tabpanel ids and require callers to
   bind the panel contract.
2. Make HeartbeatBar expose keyboard-focusable bucket details or a textual
   alternative containing time, count, rate, and latency.
3. Add component-level checks for ARIA target existence, reciprocal labels,
   keyboard reachability, and focus restoration.

Exit checks:

- Every `aria-controls` target exists and every panel has `tabpanel` semantics.
- Bucket facts are reachable without pointer input.

### R12 - Delivery as a verifiable contract

Purpose: one release tag, one health probe, one process identity, one test
class.

Findings: M00-01, M00-02, M00-03, M18-01, M18-02, M18-03, M18-04, M19-01,
M20-01, M20-02, M20-03, M20-04.

Work:

1. Make tag publication immutable: version tags only from verified release
   refs; manual dispatch cannot write the version tag.
2. Make launcher health use `/healthz`, verify process executable/command
   identity before stopping anything, and remove stale PID files only after
   verification.
3. Make the Compose example and entrypoint agree: create valid key files or do
   not mount files that the example does not create.
4. Make tests declare a class (`fast`, `race`, `stress`, `live`) and generate
   CI selection from that class. `-short` comments cannot substitute for
   enforcement.
5. Run the existing repository hygiene script in CI and lock ignore-file
   newlines.
6. Decide the license metadata and include the matching license file, or
   remove the OCI claim.
7. Generate or verify operator documentation against the effective shutdown
   budget, retention constants, timezone precedence, and race prerequisites.

Exit checks:

- A manual branch dispatch cannot write a released version tag.
- Launcher tests inject an unrelated port owner and prove no kill.
- A clean Compose run creates valid key material.
- Fast, race, and stress jobs select disjoint documented test classes.
- Repository hygiene, generated contracts, and module manifests run in both
  CI and release gates.

## Workstream Dependency Graph

The arrows below mean "must be established before the dependent workstream can
use the contract". R0 is the exception: its safety fixes must happen before
structural refactoring even though its diagnostic contracts may mature later.

```text
R0 safety containment
├──> R1 lifecycle (containment vocabulary)
├──> R3 accounting (one settlement entry point)
├──> R4 protocol (typed commit outcome)
├──> R7 persistence (typed result vocabulary)
└──> R8 identity (canonical-principal containment)

R2 effective configuration
├──> R1 lifecycle
├──> R6 transport
├──> R9 HTTP/admin
└──> R12 operator documentation

R7 persistence commands
├──> R3 accounting
└──> R8 identity

R3 accounting
└──> R5 requestlane

R4 protocol model + stream lifecycle
├──> R5 requestlane
├──> R9 HTTP error/protocol contracts
└──> R11 displayed-data contract

R5 requestlane
└──> R9 HTTP/admin

R8 identity
└──> R9 HTTP/admin

R1 lifecycle
└──> R12 health/shutdown contract

R6 transport
└──> R12 release evidence

R9 HTTP/admin
├──> R10 frontend async
├──> R11 accessibility data
└──> R12 HTTP/runtime contract

R10 frontend async
└──> R11 component accessibility

R12 delivery closeout waits for R1, R3, R5, R6, and R9.
R12 also requires closure evidence from R0, R2, R4, R7, R8, R10, and R11.
```

### Hard dependencies

| Workstream | Must wait for | Why |
|---|---|---|
| R0 | none | emergency containment is the first gate and establishes the shared typed-result vocabulary |
| R1 | R0, R2 effective config | worker budgets, readiness dimensions, and shutdown limits must come from one resolved config; R0 supplies the safety/result vocabulary |
| R2 | R0 | containment must land before structural configuration work; R2 is then the base contract for lifecycle budgets, credential resolution, retention, and admin defaults |
| R3 | R0, R2, R7 transaction primitives | R0 owns emergency settlement containment; R2 supplies effective retention and pricing inputs; R7 provides the final transaction/command contract |
| R4 | R0 | R0 supplies the typed commit outcome and failure vocabulary that the protocol sinks must preserve |
| R5 | R3, R4, R7 | attempt outcome, pricing/reservation, and persistence semantics must be typed first |
| R6 | R2 effective config | credential and protocol precedence must be resolved once |
| R7 | R0 | persistence commands must use the containment contracts rather than creating a parallel result model |
| R8 | R7 command boundaries | allowlist and throttle mutations need command-level atomicity |
| R9 | R2, R4, R5, R8 | HTTP errors and validation must project domain results, not reconstruct them |
| R10 | R9 public API semantics | UI states must consume stable login/error/loading contracts |
| R11 | R10 for shared state helpers; R4/R9 for displayed data contracts | accessibility must use final data/state boundaries, but can proceed independently of unrelated page work |
| R12 | R1, R3, R5, R6, R9; evidence from R0, R2, R4, R7, R8, R10, R11 | health, release, test classes, and operator docs must describe the final runtime and close every workstream |

### Parallel-safe groups

- After R0: R2, R4, and R7 can proceed in parallel.
- After R0, R2, and R7: R1, R3, and R8 can proceed in parallel once their
  individual prerequisites are met; R6 can also start after R2.
- After R3 and R4: R5 can proceed while R6 and R8 continue.
- After R5 and R8: R9 starts. R10 starts only after R9 publishes the public API
  semantics; it must not invent backend states.
- R11 can start after R10 and the R4/R9 displayed-data contracts are fixed. It
  does not need every unrelated page migrated.
- R12 implementation waits for R1, R3, R5, R6, and R9, but its read-only
  inventory and failing gate tests can start immediately.

### Dependency violations to reject

- `requestlane` appearing under `data` and importing `control/billing`.
- `control/accounting` importing `surface/http` to report settlement.
- `surface/admin` reaching directly into multiple DAOs for a cross-entity
  command.
- `surface/web` deciding whether a 500 means "not logged in", "empty", or
  "not loaded".
- `delivery` documenting a shutdown budget independently of lifecycle.
- `test` classification being inferred from a comment instead of a named
  test class.
- `R5 -> R3` or `R5 -> R6`: requestlane consumes accounting and transport
  contracts; those ownership boundaries do not move into the request lane.
- `R10` projecting backend login or error states before `R9` has published the
  public contract.

## Phase Gates

Each phase has four blocks: deliverables, acceptance evidence, risk controls,
and expected change scope. A phase is not complete if any acceptance item is
unknown. "Expected scope" is not a permission boundary for implementation; it
is a forecast for review and sequencing.

### Phase 0 - Measurement and design baseline

**Deliverables**

- A finding disposition ledger with one row per audit finding and one primary
  RC/workstream assignment.
- A call-path inventory for startup/shutdown, settlement, bootstrap, request
  commit, and frontend request ownership.
- Failing or skipped regression tests that demonstrate each P1 trigger.
- Baseline metrics on a test database: settlement paths, pending rows,
  uncharged rows, bootstrap child writes, worker shutdown durations, and
  frontend stale-response cases.

**Acceptance**

- Every one of the 54 findings appears exactly once as a primary entry in the
  ledger; secondary implementation dependencies are cross-references, not
  duplicate findings.
- Every P1 has a reproducible test or a clearly specified synthetic fixture
  that fails on the baseline.
- The inventory identifies the current owner and proposed owner for each
  boundary; no boundary is left as "the handler will decide".
- Existing public routes, generated contracts, and database schema are captured
  as baseline artifacts for compatibility comparison.

**Risk controls**

- Do not start with a package rename.
- Do not widen a test only to make a baseline pass.
- Do not use production data or a live deployment.
- Store baseline artifacts under the audit/remediation workspace, not in the
  production tree unless they are intentional test fixtures.

**Expected change scope**

- Planning files and test-design notes only.
- No production behavior change.

### Phase 1 - R0 safety containment

**Deliverables**

- One authoritative settlement command and idempotency marker path.
- Canonical identity normalization shared by lookup, authentication, and
  throttle state.
- Explicit first-run/bootstrap failure states and initial-readiness state.
- Typed non-stream commit result; unsuccessful writes cannot become billable
  success.
- Failure instrumentation around startup, settlement, quota release, and
  downstream commit.
- The R0 detailed design and acceptance matrix above, implemented in the
  ordered R0-A through R0-E slices rather than as independent symptom patches.

**Acceptance**

- `R0-AC-01` through `R0-AC-04` prove one canonical marker and one wallet
  debit for pending settlement and recomputation, including rollback and
  concurrency.
- `R0-AC-05` and `R0-AC-06` prove mixed-case login variants share one account
  budget and one successful reset.
- `R0-AC-07` through `R0-AC-09` prove bootstrap read/write failures stop
  startup, while a verified concurrent unique conflict is a no-op.
- `R0-AC-10` and `R0-AC-11` prove initial reload failure is retried and
  reflected in degraded health.
- `R0-AC-12` through `R0-AC-15` prove all three non-stream protocols surface
  write failures as terminal, non-billable, non-penalized outcomes without
  changing successful responses.

**Risk controls**

- Do not combine this phase with accounting schema cleanup.
- Do not introduce a new accounting table or a second completion key in R0.
- Keep a short-lived compatibility adapter if callers cannot move in one
  change; tag it with a deletion condition.
- If the settlement change requires a migration, isolate the migration and
  verify forward/rollback behavior before the command migration.
- Do not use a retry as a substitute for a missing idempotency marker.
- Do not fail over after an upstream response has returned merely because
  the downstream response write failed.

**Expected change scope**

- `internal/store` settlement/usage paths, `internal/usage`,
  `internal/dataplane` commit result, `internal/gatewayapp` startup
  readiness/bootstrap, identity/throttle code, and targeted unit/integration
  tests.
- No protocol model or frontend redesign.

### Phase 2 - R2 effective configuration

**Deliverables**

- Raw config with presence fidelity.
- One effective-config type that owns defaults, precedence, normalization, and
  validation.
- One credential resolver and one timezone resolver.
- Typed degraded-config outcomes for optional or corrupt settings.
- Retention/audit pruning policy exposed as explicit effective values.

**Acceptance**

- Table tests cover missing, partial, explicit zero/false, corrupt JSON, and
  precedence combinations.
- Bootstrap and fallback pool receive identical credential resolution.
- Non-empty configured timezone overrides `TZ`; empty config preserves `TZ`.
- `audit_log_keep_rows=0` retains the documented "disable pruning" behavior.
- Corrupt optional settings cannot silently become zero capacity or disabled
  probing without an explicit status.

**Risk controls**

- Do not change config field names or public JSON shapes in this phase.
- Keep generated default/field inventories in sync.
- Treat admin runtime defaults as a separate source from static config; do not
  collapse both into one mutable object.
- Do not use a zero value to mean both "absent" and "explicitly disabled".

**Expected change scope**

- `internal/config`, bootstrap/config fallback construction,
  `internal/snapshot` consumption, `internal/gatewayapp` startup, config
  generation/tests, and documentation statements governed by these constants.

### Phase 3 - R1 lifecycle ownership

**Deliverables**

- Lifecycle phases and worker registration/join API.
- Readiness projection with database, runtime, and bootstrap dimensions.
- One shutdown-budget source and consistent deadline observations.

**Acceptance**

- Every long-lived worker is registered and joined before storage close.
- Probe stagger sleep and usage retry/synchronous fallback observe
  cancellation and cannot write after store close.
- Health tests distinguish DB failure, runtime not converged, bootstrap
  incomplete, and ready.

**Risk controls**

- Do not make lifecycle manager own business decisions.
- A timeout log is not proof of worker exit; require join evidence.

**Expected change scope**

- `internal/gatewayapp`, `internal/runtime`, worker packages
  (`internal/probe`, `internal/usage`), `internal/api` readiness projection,
  lifecycle tests, and deployment budget constants.

### Phase 4 - R7 persistence command boundaries

**Deliverables**

- Persistence command/transaction primitives for cross-entity mutations.
- Bootstrap graph command with complete graph validation and explicit
  completion state.
- Transaction result types that distinguish committed, not committed, and
  recoverable/incomplete state.

**Acceptance**

- Bootstrapping a DB with any provider but an incomplete graph is detected and
  can be repaired or refused explicitly.
- A failure injected at each bootstrap child write leaves either the complete
  graph or a verified resumable state.
- Configuration dry-run and real import share one model-cardinality function,
  including the same `model_id` appearing under multiple new providers.
- Admin handlers no longer compose cross-entity mutations from independent
  DAO calls.
- Existing data remains readable and forward-compatible.

**Risk controls**

- Migrate bootstrap and settlement separately; they share transaction
  primitives but not one mega-command.
- Preserve current data and migration compatibility.
- Do not let admin handlers call transaction helpers directly.
- Do not mix readiness/lifecycle behavior into transaction tests.

**Expected change scope**

- `internal/store` transaction/DAO primitives and bootstrap-related commands,
  admin command adapters, schema/migration tests, and persistence command
  tests.

### Phase 5 - R3 accounting state machine

**Deliverables**

- Accounting state and transition model.
- One settlement transaction covering wallet, pending, usage, and canonical
  completion marker.
- Owned quota reservation set with complete release.
- Terminal-state-aware retention and archive eligibility.
- Reconciliation cursor/queue that cannot be starved by one retryable or
  permanent row.
- One effective-price path for preflight and actual charging.

**Acceptance**

- No record can be charged twice under pending, recompute, and reconciliation
  paths.
- Retention cannot archive non-terminal or still-recoverable financial state.
- Every reservation in a failed batch is released.
- A permanently failing reconciliation row does not prevent later rows from
  making progress; the failure has an explicit terminal/manual state.
- Actual usage cost equals the effective-price contract used by preflight for
  partial price configurations.

**Risk controls**

- Use an invariant/property test over state transitions, not only happy-path
  examples.
- Test with realistic archive boundaries and old pending rows.
- Keep money amounts and idempotency keys stable across retries.
- Do not silently drop debt or write off residuals without an explicit policy.
- Do not change charging semantics and retention SQL in the same commit if a
  migration can be separated.

**Expected change scope**

- `internal/billing`, `internal/usage`, `internal/store/balance_dao.go`,
  `topup_dao.go`, `usage_dao.go`, `usage_archive.go`, `pricing.go`,
  maintenance workers in `internal/gatewayapp`, and financial stress tests.

### Phase 6 - R4 protocol model and stream lifecycle

**Deliverables**

- Complete normalized request/response part model and unsupported-part policy.
- Inbound codecs that map or reject every supported part explicitly.
- Shared media-aware estimation and route-aware output ceiling.
- Outbound sink state machines with guaranteed protocol-valid start/terminal
  sequences.
- One error-classification and supported-capability source.

**Acceptance**

- Chat `input_audio`/`file`, Anthropic text-source documents, text plus tool
  calls, and terminal-only empty streams have explicit passing tests.
- Every inbound/outbound pair either round-trips a supported part or returns a
  typed rejection; no silent drop remains.
- TPM/quota reservations include the resolved output ceiling and media input
  estimate.
- Stream state tests cover no-event, terminal-only, text, tool, failure, and
  client-cancel paths.

**Risk controls**

- Treat public wire compatibility as a contract; do not opportunistically
  reshape existing JSON fields.
- Keep reasoning/encrypted payload behavior scoped to the existing contract.
- Do not change request limits, error status mappings, and protocol shapes in
  one undifferentiated patch.
- Test malformed and duplicate input separately from supported-content
  translation.

**Expected change scope**

- `internal/inwire`, `internal/ingress`, `internal/outwire`, protocol
  contract tests, and the relevant dataplane call sites.

### Phase 7 - R5 requestlane and R6 transport

**Deliverables**

- Requestlane under `control`, with typed admission, candidate, attempt, and
  commit outcomes.
- Accounting handoff consumes the commit result and owns billability.
- Transport results distinguish known, unknown, and error balance/discovery
  states.
- Effective protocol/credential resolution used by runtime and discovery.

**Acceptance**

- Every early return releases the correct reservations and produces one
  accounting outcome.
- Non-stream write failure, stream disconnect, terminal-only empty, malformed
  upstream, and timeout are distinct and stable.
- Auto-detected Anthropic discovery uses the same auth contract as explicit
  Anthropic.
- Balance failures never display a precise available amount without a known
  usage value.

**Risk controls**

- Keep the old request path available only behind a tested adapter during the
  transition.
- Do not move pricing decisions into the request lane.
- Do not let transport call accounting directly.
- Preserve route trace semantics as a projection of attempt events.
- Test cancellation and simultaneous stream/non-stream paths under race.

**Expected change scope**

- `internal/dataplane` split into request orchestration and protocol-facing
  adapters, `internal/upstream`, `internal/probe` only where ownership touches
  transport, route trace, and the relevant store command interfaces.

### Phase 8 - R8 identity and authorization

**Deliverables**

- Canonical principal identity type.
- Session/throttle lifecycle with bounded cleanup.
- Model allowlist as a narrowing capability.
- Authorization matrix coverage for every key/group/admin mutation path.

**Acceptance**

- Case and whitespace variants canonicalize identically for lookup and
  throttling.
- Successful authentication, logout, revocation, and expiry release the
  correct state.
- A key owner cannot remove an administrator-applied allowlist by omission.
- Cross-user and cross-role tests cover the new command paths.

**Risk controls**

- Do not weaken the exact route/method authorization matrix.
- Do not trust token claims for current role/status.
- Avoid a global mutable identity cache; use the snapshot/session owner.
- Test concurrent login and revocation, not only serial requests.

**Expected change scope**

- `internal/auth`, `internal/authhttp`, `internal/identity`,
  `internal/userauth`, `internal/guard`, `internal/admin` identity/key/group
  handlers, and authorization tests.

### Phase 9 - R9 HTTP/admin edges

**Deliverables**

- Shared decoding/limit/error policy for data-plane and admin JSON.
- Typed validation failures at command/query services.
- Login, setup, bootstrap, and profile-refresh result separation.
- Stable public error envelopes for the frontend.

**Acceptance**

- Oversized, malformed, unsupported-media, and invalid-query requests map
  consistently without silently substituting defaults.
- Malformed organization time parameters return a typed 4xx and never select
  the default reporting window.
- Group/name validation and query/configuration contract errors are produced
  by the owning command or query service; the HTTP edge only projects them.
- Invalid login preserves authentication error semantics; successful setup
  followed by `/me` failure remains a distinct state.
- Real route authorization and validation tests pass.

**Risk controls**

- Keep response wrappers and CSRF behavior unchanged unless explicitly
  reviewed.
- Do not duplicate domain validation in both handler and command service.
- Do not turn all errors into 500 or all invalid inputs into 400.
- Maintain generated DTO/route contracts.

**Expected change scope**

- `internal/httpx`, `internal/api`, `internal/server`,
  `internal/admin` helpers/handlers, `internal/billinghttp`, command/query
  services, frontend API client contracts, and HTTP/admin contract tests.

### Phase 10 - R10 frontend async ownership

**Deliverables**

- Request generation/entity-key helper.
- View states for not-loaded, loading, empty, error, and ready.
- Entity-scoped state reset on route changes.
- Set semantics for model selection.
- Login/setup/bootstrap states driven by backend results.

**Acceptance**

- Out-of-order responses cannot overwrite a newer provider, timeline, or
  catalogue selection.
- Direct lazy-tab navigation loads the required data exactly once.
- Hidden ModelPicker selections survive a filtered "select all visible".
- Login/setup/profile failures and loading/empty states are distinguishable.

**Risk controls**

- Do not hide an error as an empty state.
- Do not introduce a global store for convenience if a scoped helper suffices.
- Do not change public API shapes to simplify the UI.
- Test with deferred promises/out-of-order completion, not only happy paths.

**Expected change scope**

- `web/src/api.ts`, shared composables/request state, `web/src/views`, shared
  picker/table/tab call sites, and frontend contract/component tests.

### Phase 11 - R11 accessibility

**Deliverables**

- Tabs/tabpanel contract with reciprocal ids and roles.
- Keyboard-focusable or textual heartbeat bucket details.
- Component-level accessibility tests.

**Acceptance**

- Every generated `aria-controls` target exists.
- Every tab panel has `tabpanel` and reciprocal `aria-labelledby`.
- Bucket time, success rate, counts, and latency are reachable without a
  pointer.
- Focus restoration and modal/tab keyboard behavior remain intact.

**Risk controls**

- Prefer native elements and existing component APIs.
- Do not add ARIA roles that conflict with actual behavior.
- Test semantics in the accessibility tree or DOM, not only screenshot
  appearance.

**Expected change scope**

- `web/src/components/Tabs.vue`, `HeartbeatBar.vue`, their callers, and
  component/accessibility tests.

### Phase 12 - R12 delivery and closeout

**Deliverables**

- Immutable release-tag policy.
- Process-identity-safe launcher and `/healthz` probe.
- Valid clean Compose/secret initialization.
- Explicit fast/race/stress test classes.
- CI and release visibility for hygiene, generated contracts, and module
  manifests.
- Documentation synchronized to effective config/lifecycle constants.
- Final finding disposition ledger with evidence.

**Acceptance**

- A manual branch dispatch cannot replace a released version tag.
- Launcher refuses an unrelated process on the configured port/PID.
- A clean Compose run creates valid key material and reaches readiness.
- Fast, race, stress, and live tests have enforceable selection.
- The release workflow runs the same required contract checks as CI.
- All 54 findings have closure evidence or an explicitly accepted residual
  risk.

**Risk controls**

- Do not make version tags mutable.
- Do not use a static admin page as readiness.
- Do not rely on a developer machine path without documenting and testing a
  fallback.
- Do not close a finding because its documentation changed; verify the runtime
  trigger.
- Re-run the final phase on the exact artifact that would be published.

**Expected change scope**

- `.github/workflows`, release metadata, `Dockerfile`, Compose docs,
  `docker/docker-entrypoint.sh`, `gateway.ps1`, test-class configuration,
  affected docs, and final audit/remediation ledgers.

## Rollback and Risk Register

Rollback is part of the plan, not an afterthought. The remediation must be
deployable and reversible one phase at a time.

| Risk | Trigger | Prevention | Containment / rollback |
|---|---|---|---|
| Double charging during R0/R3 transition | both old and new settlement paths remain callable | one entry point and one marker; no second write path | disable the new path, retain the ledger and replay pending records through the old path only after reconciliation |
| Store close while a worker is alive | lifecycle wait times out | worker registry, join evidence, no store close on incomplete join | keep process alive in a drain/fail-fast state; do not report clean shutdown |
| Bootstrap leaves a mixed graph | child write fails after an earlier commit | one command/transaction or durable completion marker | block publish/readiness and repair from the recorded graph state |
| Protocol content regression | codec rejects a previously accepted part | compatibility matrix and explicit rejection tests | keep the previous adapter behind a feature switch only if it preserves the same normalized contract; remove after one release |
| Pricing changes historical display | recomputation or archival changes effective price | freeze price at usage time; retain explicit unpriced state | stop recomputation, restore old read path, investigate from frozen records |
| Config precedence changes existing deployments | effective config resolver replaces caller-local rules | migration test with old/new config examples; one resolver | keep old config JSON accepted, restore prior resolver, and publish the effective values in logs |
| Frontend hides a real error as empty | new loading/error state default is wrong | explicit five-state rendering and component tests | revert the helper or render a retryable error; never show an empty catalogue as a substitute |
| Release tag is overwritten | manual dispatch still writes version tag | immutable tag policy and workflow guard | delete/restore the tag only under an explicit operator-controlled recovery; publish a new version, do not move the old one silently |
| CI fast gate becomes unreliable | stress tests remain in the fast class | named test classes, measured runtime budget, failing workflow gate | run the stress class separately; block release until it passes |
| Documentation drifts again | constants or precedence change without docs | generated/checked constant references and review gate | correct docs in the same change; do not close the runtime finding from a docs edit |

### Data safety rules

- Snapshot the database before any accounting migration.
- Migrations must be forward-compatible with the current binary until the
  migration phase is complete.
- Never delete pending, uncharged, or non-terminal accounting rows merely to
  make a migration pass.
- Record a stable remediation marker or migration version so retries are
  idempotent.
- Verify a restore from backup on a copied database before declaring a
  destructive step safe.
- Do not run a repair command on live data without a dry-run report and a
  reviewed list of affected records.

### Compatibility rules

- Public request/response shapes remain stable.
- Existing config keys remain accepted; new presence semantics must not reject
  valid old files.
- Database rows written by the baseline binary must remain readable by the
  migration path.
- Generated routes/DTOs/defaults are regenerated before handwritten adapters
  change.
- A temporary adapter must name its owner, deletion phase, and test that
  proves both paths agree.

## Required Tests and Gates

### Architecture gates

- No `data` or `platform` package imports `control`, `surface`, `delivery`, or
  `cmd`.
- No `surface` module performs direct cross-entity SQL transactions.
- No protocol module imports billing, persistence, snapshot, or admin.
- No frontend component directly mutates global API/session state outside the
  client and route-state contracts.

### Domain invariant tests

- Settlement idempotency across pending, top-up, recompute, and reconciliation.
- Retention never archives a non-terminal accounting record.
- Quota reservation release covers every reservation in a failed batch.
- Bootstrap either commits the complete graph or publishes none of it.
- Model allowlist narrowing is monotonic for non-admin callers.
- Balance reports unknown rather than a fabricated value.

### Protocol matrix tests

- Each inbound/outbound pair for text, tool, image, audio, file/document,
  thinking, refusal, empty terminal, malformed, and missing usage.
- Every supported part has either a round-trip assertion or an explicit
  rejection assertion.
- Every stream has a valid start, content or empty-output, terminal, and
  failure sequence.

### Lifecycle and race tests

- Cancellation during probe stagger, usage retry, reconcile wait, and HTTP
  drain.
- Shutdown under active writer and active reader.
- Store close forbidden until every registered worker has joined.
- No stale writer after readiness flips to not-ready.

### Frontend tests

- Request generation/out-of-order response.
- Route/entity change clears the correct state.
- Lazy tab direct navigation.
- ModelPicker hidden selection preservation.
- Login, setup, bootstrap error, modal focus, tabs ARIA, and heartbeat keyboard
  access.

### Delivery tests

- Tag guard rejects branch dispatch for version tags.
- Launcher refuses unrelated PID/port owner.
- Clean Compose initialization produces valid files.
- Fast/race/stress class selection is asserted from workflow configuration.
- Hygiene and generated-contract checks exist in both CI and release.

## Regression and Compatibility Rules

- Preserve public HTTP request/response shapes unless a compatibility change is
  explicitly approved and documented.
- Preserve existing database data through forward migrations. No destructive
  reset is part of this plan.
- Keep the generated route/DTO/default contracts authoritative; update
  generators before handwritten clients.
- Do not introduce a new third-party dependency for a problem already solved
  by the standard library or an existing dependency.
- Do not keep two permanently maintained copies of pricing, credential,
  timezone, retention, or protocol rules "for safety". Transitional adapters
  must have a deletion point.
- Avoid speculative abstractions with no current caller. The listed modules
  each own an already observed responsibility.

## Verification Policy

Verification follows the affected guarantee, not the number of files changed.

1. Run focused package tests for a changed module.
2. Run architecture and generated-contract checks when boundaries or contracts
   change.
3. Run the full fast test class for every phase merge.
4. Run race tests for lifecycle, worker, accounting, authorization, and
   frontend async changes.
5. Run stress/live tests only in their explicit class; do not let a green fast
   gate imply they ran.
6. Validate migrations on a copy of realistic data and on a clean database.
7. Validate frontend behavior with typecheck, build, and targeted browser tests
   when UI state or accessibility changes.
8. Build and inspect the image for delivery changes. Do not treat a source test
   as proof that the published artifact or tag is safe.

## Stop Conditions

Pause and resolve the design before continuing if any of these occur:

- A proposed fix creates a second authoritative settlement, pricing, config,
  protocol, or authorization path.
- A new package dependency creates an upward edge or a cycle.
- A migration cannot preserve existing pending, uncharged, or archived data.
- A protocol change would silently alter a public response shape without an
  explicit compatibility decision.
- A lifecycle wait cannot be joined and the only proposed remedy is a longer
  timeout.
- A test passes only because a zero value or empty collection is interpreted
  as success.
- A frontend fix hides an error state instead of representing it.
- A delivery or documentation fix is used to mask an unresolved runtime defect.

## Finding Disposition Template

Each fix, when implemented, must append a closeout entry:

```text
Finding: Mxx-yy
Root cause: Rxx
Decision: fixed / superseded / accepted risk
Change: <module and contract changed>
Proof: <test, migration, artifact, or operator evidence>
Compatibility: <public behavior, data, or deployment impact>
Residual risk: <none or explicit>
```

The audit is not complete merely because a finding is marked "fixed". The
entry must show evidence that the old trigger no longer reproduces.

## Execution Order Summary

1. Phase 0: measurement, baseline, and finding ledger.
2. R0: safety containment.
3. R2: effective configuration.
4. R1: lifecycle ownership.
5. R7: persistence command boundaries.
6. R3: accounting state machine.
7. R4: protocol model and stream lifecycle.
8. R5 and R6: requestlane and truthful transport.
9. R8: identity and authorization.
10. R9: HTTP/admin edges.
11. R10: frontend async ownership.
12. R11: accessibility.
13. R12: delivery and closeout.

The sequence is ordered by integrity risk and by how many later workstreams
depend on the contract. It is not a promise that every finding disappears in
the same pull request. It is a promise that each pull request moves the
architecture toward the target rather than adding a permanent special case.

The order differs from the workstream numbering because R2 supplies the
effective values used by R1, and R7 supplies the transaction primitives used
by R3. Workstream IDs are stable labels, not an execution sequence.

## Explicit Non-Goals

- No full rewrite.
- No new framework or speculative service split.
- No live deployment or database migration in this planning step.
- No claim that the current test suite covers the target invariants.
- No package rename merely to look cleaner; move code only when ownership or
  dependency direction changes.
- No weakening of authentication, authorization, retention, recovery, or
  accessibility to simplify the refactor.
