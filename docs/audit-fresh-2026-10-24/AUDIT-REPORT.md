# Rosetta Gateway Fresh Code Audit

Date: 2026-10-25

Baseline: `e43c266c98cb8121be020fdac95dfe15fcdae2d9`

Branch: `main`

## Executive Summary

This audit covered the repository in 21 modules, from build and release
through backend lifecycle, protocol conversion, persistence, authorization,
frontend behavior, deployment, tests, and documentation. The result is:

- 54 confirmed findings: 4 P1, 29 P2, and 21 P3.
- No P0 finding was established.
- 83 unresolved questions are recorded separately from confirmed defects.
- All planned modules `M00` through `M20` are complete.
- Every confirmed finding has a concrete trigger, impact, and `file:line`
  evidence in `FINDINGS.md` and `EVIDENCE.md`.
- The workspace was not built, tested, run, migrated, or deployed during this
  audit. The result is a static source and contract audit, not runtime proof.

The four P1 findings are concentrated in four different control planes:
initial startup convergence, first-run administrator initialization, financial
idempotency, and login throttling. The P2 population is dominated by
accounting recovery, protocol/content preservation, configuration bootstrap,
and operational safety. The P3 population is mostly bounded compatibility,
frontend state, accessibility, and documentation drift.

## Severity Summary

| Severity | Count | Meaning in this audit |
|---|---:|---|
| P0 | 0 | Immediate loss of confidentiality, integrity, or availability in a normal deployment path |
| P1 | 4 | Serious production failure or security exposure with a realistic trigger |
| P2 | 29 | Correctness, recovery, compatibility, or operational defect with a bounded workaround |
| P3 | 21 | Documentation, metadata, consistency, or low-impact maintainability defect |
| Total | 54 | Confirmed findings only |

## Module Coverage

| Module | Area | Status | Findings | P1 | P2 | P3 |
|---|---|---|---:|---:|---:|---:|
| M00 | Baseline, build, CI, release | complete | 3 | 0 | 1 | 2 |
| M01 | Startup and lifecycle | complete | 4 | 2 | 2 | 0 |
| M02 | Config, snapshot, routing, catalog | complete | 4 | 0 | 4 | 0 |
| M03 | Inbound protocols | complete | 5 | 0 | 4 | 1 |
| M04 | Outbound protocols | complete | 2 | 0 | 2 | 0 |
| M05 | Dataplane and failover | complete | 1 | 0 | 1 | 0 |
| M06 | Upstream pool, probing, networking | complete | 2 | 0 | 2 | 0 |
| M07 | Storage, schema, DAO, transactions | complete | 1 | 0 | 1 | 0 |
| M08 | Money, quota, balance | complete | 3 | 1 | 2 | 0 |
| M09 | Usage and audit persistence | complete | 2 | 0 | 1 | 1 |
| M10 | Identity and authorization | complete | 2 | 1 | 1 | 0 |
| M11 | HTTP middleware and routing | complete | 1 | 0 | 0 | 1 |
| M12 | Admin mutation APIs | complete | 3 | 0 | 1 | 2 |
| M13 | Admin query APIs | complete | 1 | 0 | 0 | 1 |
| M14 | Billing and usage pipeline | complete | 2 | 0 | 2 | 0 |
| M15 | Frontend foundation | complete | 3 | 0 | 0 | 3 |
| M16 | Frontend pages | complete | 3 | 0 | 1 | 2 |
| M17 | Frontend components | complete | 3 | 0 | 1 | 2 |
| M18 | WebUI embedding, deployment, CI | complete | 4 | 0 | 2 | 2 |
| M19 | Test quality | complete | 1 | 0 | 1 | 0 |
| M20 | Documentation and final deduplication | complete | 4 | 0 | 0 | 4 |
| Total |  |  | 54 | 4 | 29 | 21 |

Detailed reviewed-file lists are in `MODULES.md`. Per-module reasoning,
cleared candidates, and open questions are in `PROGRESS.md`. The finding
register is in `FINDINGS.md`; the evidence index is in `EVIDENCE.md`.

## Scope and Method

The audit used only the repository at the recorded baseline. The old audit
reports under `docs/audit-2026-10-20` and `docs/audit-2026-10-22.md` were not
used as evidence. No sub-agents were used.

For each module, the process was:

1. Enumerate the production files, entry points, and exported contracts.
2. Trace callers, state transitions, failure paths, and cleanup paths.
3. Compare the behavior with `DESIGN.md` and `README.md`.
4. Reproduce the logical trigger from source and record exact evidence.
5. Count only reachable defects as findings; put unresolved policy or
   environmental questions in the open-question section.
6. Update progress before moving to the next module.
7. Recheck cross-module overlap before assigning the final finding count.

The mode was read-only. Only this audit directory was created or updated.
No source, configuration, generated output, test, or unrelated documentation
was modified.

## P1 Findings

### M01-01 - Initial runtime rebuild failure is not retried or health-reported

The first database-backed runtime rebuild can fail, leave the process on a
fallback or empty snapshot, and never mark the runtime dirty. The retry loop
then sleeps in its clean branch while `/healthz` can report success if the
database ping succeeds.

Primary evidence: `internal/gatewayapp/run.go:127-137`,
`internal/runtime/reloader.go:63-120`, `internal/api/healthz.go:24-40`.

### M01-02 - First-run admin bootstrap failure is swallowed

The admin bootstrap helper logs `CountUsers` and `CreateUser` failures but
always returns success. The startup guard therefore cannot stop a gateway
that has no usable administrator. A fresh deployment can appear to start
normally while the public bootstrap workflow has no pending administrator to
initialize.

Primary evidence: `cmd/gateway/bootstrap_admin.go:20-22,60-92`,
`internal/gatewayapp/run.go:146-154`,
`internal/admin/user_handler.go:485-528`.

### M08-01 - Deferred and recomputed costs can be charged twice

The original pending balance charge and the later reconciliation/recompute
path do not share one complete idempotency key across both settlement paths.
A successful cost recomputation can therefore make a usage record chargeable
again after the user has already paid the pending amount. This is a
financial-integrity defect, not merely a display discrepancy.

Primary evidence: `internal/store/balance_dao.go:312-380,688-727,782-923`,
`internal/store/topup_dao.go:236-245`,
`internal/store/usage_dao.go:506-553,588-727`,
`internal/gatewayapp/serve.go:208-294`.

### M10-01 - Login throttling is bypassed by username case variants

The account-dimension failure counter does not use one canonical identity
before applying the throttle key. Changing username case creates distinct
buckets and defeats the intended account lockout across those variants.

Primary evidence: `internal/admin/user_handler.go:69-74,208-225,229-237,302-307,341-357`,
`internal/store/user_dao.go:158-163`, `internal/store/schema.go:304-311`.

## P2 Findings

| ID | Finding | Primary area |
|---|---|---|
| M00-01 | CI hygiene checks are weaker than the repository hygiene script | Build/CI |
| M01-03 | Probe shutdown cannot interrupt stagger sleep and can outlive the wait budget | Lifecycle |
| M01-04 | Usage drain timeout does not stop synchronous fallback writes or join workers | Lifecycle |
| M02-01 | `api_key_env` incorrectly overrides a configured `api_key` | Bootstrap credentials |
| M02-02 | Corrupt model defaults become zero capacity in the runtime snapshot | Catalog/snapshot |
| M02-03 | A partial `probes` object silently disables probing | Configuration |
| M02-04 | Bootstrap writes are non-atomic and are not repaired on restart | Bootstrap |
| M03-01 | OpenAI Chat inbound silently drops `input_audio` and `file` | Inbound compatibility |
| M03-02 | Anthropic text-source documents are silently discarded | Inbound compatibility |
| M03-03 | TPM and lifetime quota reservations omit the routed output ceiling | Admission |
| M03-04 | Ingress token estimation ignores all media input | Admission/accounting |
| M04-01 | Chat non-stream output drops text when tool calls are present | Outbound compatibility |
| M04-02 | Empty terminal streams omit Anthropic/Responses protocol starts | Outbound streaming |
| M05-01 | Non-stream downstream write failures are recorded successful and charged | Dataplane accounting |
| M06-01 | Unknown OpenAI usage is reported as zero, inflating available balance | Upstream balance |
| M06-02 | Auto-detected Anthropic discovery uses Bearer instead of x-api-key | Model discovery |
| M07-01 | Pending costs can be archived as permanent zero-cost usage | Retention/archive |
| M08-02 | Failed quota release strands reservations until restart | Quota recovery |
| M08-03 | Malformed organization time parameters silently select the default window | Reporting |
| M09-01 | Abandoned usage batches release only the first ten quota reservations | Usage recovery |
| M10-02 | Successful session authentication leaves idle throttle entries behind | Identity |
| M12-03 | Key owner can remove an admin-applied model allowlist | Authorization |
| M14-01 | Uncharged reconciliation can starve behind permanently unaffordable rows | Financial recovery |
| M14-02 | Actual pricing bypasses the effective-price fallback used by precheck | Billing |
| M16-01 | Provider navigation can display one provider's balance under another | Frontend state |
| M17-02 | "Select all visible" removes hidden selected models | Frontend authorization input |
| M18-01 | Manual Docker dispatch can overwrite a published version tag | Release safety |
| M18-03 | Launcher force-terminates any port owner or stale PID | Host safety |
| M19-01 | The `-short` CI gate still runs claimed-excluded load and stress tests | Test selection |

## P3 Findings

| ID | Finding | Primary area |
|---|---|---|
| M00-02 | Release workflow omits generated-contract and module-manifest checks | Release gate |
| M00-03 | OCI metadata declares MIT without a license file | Metadata |
| M03-05 | Responses tool errors omit supported `custom` and `local_shell` types | Diagnostics |
| M09-02 | `audit_log_keep_rows=0` does not disable pruning as documented | Audit retention |
| M11-01 | Oversized admin JSON bodies are reported as malformed JSON, not 413 | HTTP compatibility |
| M12-01 | Group PATCH accepts and persists an empty name | Admin validation |
| M12-02 | Configuration dry-run undercounts same-named models across new providers | Configuration transfer |
| M13-01 | Audit query silently accepts a malformed `limit` | Query validation |
| M15-01 | Login failure is rewritten as an expired session | Login UX |
| M15-02 | Password setup reports failure after the password was committed | Setup UX |
| M15-03 | Bootstrap status errors become an ordinary login form | Startup UX |
| M16-02 | Probe timeline responses can overwrite a newer selection | Frontend state |
| M16-03 | Direct price-tab navigation appears as an empty model catalogue | Frontend loading |
| M17-01 | Tabs emit `aria-controls` targets that do not exist on three pages | Accessibility |
| M17-03 | Heartbeat bucket details are pointer-only | Accessibility |
| M18-02 | Detached launcher uses the static `/admin/` page as its health check | Operations |
| M18-04 | Fine-grained Compose example mounts key files it never creates | Deployment |
| M20-01 | Documented shutdown budget omits the reconciliation wait | Documentation |
| M20-02 | Several documents still describe usage retention as fixed at 30 days | Documentation |
| M20-03 | DOCKER says `TZ` overrides config, but config is final | Documentation |
| M20-04 | DESIGN says local race testing is unavailable despite the documented CC setup | Documentation |

## Cross-Module Deduplication

The final count counts a root cause once unless a different module has a
separately reachable consequence and a separate remedy:

- `M01-03` and `M01-04` own the probe and usage shutdown defects.
  `M20-01` records only the independent arithmetic/documentation error in the
  advertised budget and does not count the underlying shutdown bugs again.
- `M01-02` covers failure to create the initial administrator. `M02-04`
  covers partial provider/network/credential/model/route bootstrap. They are
  different state machines and have different recovery consequences.
- `M03-03` is the missing output-side reservation. `M03-04` is the missing
  media input estimate. They affect different portions of the same estimator
  contract and require separate fixes.
- `M07-01` is a retention/archive ordering defect for pending cost rows.
  `M08-01` is the balance-charge idempotency defect. `M14-01` is a
  reconciliation starvation defect. Fixing one does not imply the other two.
- `M08-02` is a failed quota-release cleanup path. `M09-01` is the
  abandoned-batch release limit. They have different triggers and different
  lost/stranded state.
- `M08-03` and `M13-01` both involve malformed query values, but one changes
  the selected reporting window and the other accepts a default-sized audit
  result. They are separate endpoint contracts.
- `M11-01` is body-size/status behavior. `M12-02` is configuration dry-run
  cardinality. `M13-01` is query-parameter validation. There is no shared
  defect despite the common HTTP surface.
- `M15-01`, `M15-02`, and `M15-03` are distinct states: invalid credentials,
  a committed password followed by a failed profile refresh, and a failed
  bootstrap-status read rendered as login.
- `M16-01` owns the stale cross-provider balance root cause.
  `CredentialPanel` is the display site, so M17 does not count the same
  defect again.
- `M18-02` is a false-positive health message. `M18-03` is an unsafe process
  kill. They affect different launcher actions.
- `M18-01` covers Docker tag mutability. `M18-04` covers missing key-file
  initialization in the Compose example. They require separate release and
  deployment fixes.
- `M20-02` is stale documentation for an already-correct configurable feature.
  It is not a duplicate of any missing configuration implementation.

## Open Questions

The 83 open questions are not rejected findings. They are cases where the
source shows a possible inconsistency but the product contract, environment,
or runtime-threshold evidence was not sufficient to call a defect. They are
recorded by module in `PROGRESS.md` so later remediation does not silently
turn them into assumptions.

The main unresolved groups are:

- Whether balance precheck should estimate the full resolved chain before
  failover and target filters, or only the targets that can actually be
  attempted.
- Whether all-busy probe claims should be distinguished from upstream failure
  with a different error status.
- How sub-cent debt, deleted/unlimited-user debt, and ledger-write failures
  should be represented durably.
- Whether pending and uncharged-usage rows must survive a retention cutover,
  and what recovery guarantee the accounting contract requires.
- Whether missing upstream usage and malformed historical records should be
  estimated, failed, or surfaced as unknown.
- Which Responses media and reasoning shapes are part of the pinned public
  contract rather than gateway extensions.
- Whether transient `/me` failures should produce a retryable degraded UI
  instead of a login state.
- Which performance and timing assertions are valid on all supported CI
  runners.
- Whether documentation should preserve historical notes or replace obsolete
  claims.

## Verification Limits

This audit is deliberately narrower than a release qualification:

- No `go build`, `go vet`, `go test`, race, frontend, Docker, HTTP, browser,
  database, or deployment command was executed.
- No live request was sent, no server was started, no container was created,
  and no source file was edited during the audit.
- Findings based on streams, shutdown, recovery, and frontend races are
  source-proven logical paths. Their wall-clock frequency and production
  blast radius were not measured.
- The exact line numbers refer to the recorded baseline. They can drift after
  remediation.
- The audit covered material route, DTO, generated contract, test, and
  documentation paths. It did not exhaustively verify every historical
  example, comment, or release note.
- The workspace state check is limited to `git status --short`; no staged
  index, commit, tag, or remote state was changed.

## Prioritized Remediation

### Gate 1: P1 correctness and security

1. `M08-01`: make balance charging idempotent across pending settlement,
   cost recomputation, and reconciliation.
2. `M10-01`: canonicalize the login identity before throttle keying and add a
   case-variation regression test.
3. `M01-02`: propagate admin bootstrap failures and stop or explicitly
   degrade startup.
4. `M01-01`: mark failed initial runtime rebuilds dirty and make health reflect
   runtime convergence.

### Gate 2: Financial and recovery invariants

1. `M07-01`, `M08-02`, `M09-01`, `M14-01`, and `M14-02`: repair archive,
   quota release, batch abandonment, reconciliation ordering, and actual-price
   fallback together, because each one changes the evidence used by the next.
2. `M06-01`: distinguish unknown provider usage from zero usage.
3. Add end-to-end recovery tests for pending charge, recomputation, archive
   boundary, quota release failure, and multi-page reconciliation.

### Gate 3: Protocol and accounting correctness

1. `M03-01`, `M03-02`, `M04-01`: preserve media, text-source documents, and
   text alongside tool calls.
2. `M04-02`: guarantee valid protocol starts for empty terminal streams.
3. `M05-01`: propagate non-stream write failure into usage status and charging.
4. `M03-03`, `M03-04`, `M06-02`: align reservations, media estimation, and
   discovery authentication.

### Gate 4: Lifecycle and configuration

1. `M01-03`, `M01-04`: make shutdown cancellation-aware and join all writers
   before store close.
2. `M02-01` through `M02-04`: centralize credential precedence, default
   handling, bootstrap reads, and atomic bootstrap writes.
3. Re-run startup and shutdown verification with explicit failure injection.

### Gate 5: Identity, frontend, and authorization

1. `M10-02`, `M12-03`, `M16-01`, `M17-02`: throttle cleanup, allowlist
   immutability, provider-scoped balance state, and hidden-selection merge.
2. `M15-01` through `M15-03` and `M16-02` through `M16-03`: preserve login
   errors, committed setup state, bootstrap failure state, timeline request
   generation, and price-tab loading state.
3. `M17-01`, `M17-03`: bind ARIA panels and provide keyboard-accessible
   heartbeat details.

### Gate 6: Delivery and documentation

1. `M18-01`, `M18-03`, `M19-01`: make release tags immutable from manual
   dispatch, verify process identity before termination, and align the fast
   test boundary with the actual workload.
2. `M00-01` through `M00-03`, `M18-02`, `M18-04`: close CI hygiene,
   release-gate, license, launcher-health, and Compose initialization gaps.
3. `M20-01` through `M20-04`: correct the shutdown budget, configurable
   retention wording, timezone precedence, and race-test instructions.

Do not combine Gate 2 with unrelated protocol refactors: financial recovery
must have a clean before/after state. Do not treat documentation fixes as
proof that the corresponding runtime defect is fixed.

## Completion State

- Plan: `PLAN.md`
- Module coverage: `MODULES.md`
- Progress and open questions: `PROGRESS.md`
- Confirmed findings: `FINDINGS.md`
- Evidence index: `EVIDENCE.md`
- Consolidated report: this file

The audit is complete within the static, read-only scope. Any implementation,
test, build, deployment, or runtime validation is a separate task and was not
performed here.
