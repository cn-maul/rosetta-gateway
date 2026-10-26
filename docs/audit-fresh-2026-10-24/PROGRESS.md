# Audit Progress

Updated: 2026-10-26

## Current Position

- Audit completed: `M00`, `M01`, `M02`, `M03`, `M04`, `M05`, `M06`, `M07`,
  `M08`, `M09`, `M10`, `M11`, `M12`, `M13`, `M14`, `M15`, `M16`, `M17`,
  `M18`, `M19`, `M20`
- Remediation implementation: R0 containment, R1 lifecycle ownership, R2
  effective configuration, R3 accounting state machine, R4 protocol model,
  R5 requestlane ownership, R6 transport contracts, R7 persistence command
  boundaries, R8 identity/authorization invariants, R9 HTTP/admin edges,
  R10 frontend async ownership, R11 accessibility contracts, and R12 delivery
  contracts complete and verified
- Next: none; final ledger reviewed after R12
- Findings: 54 confirmed, comprising 4 P1, 29 P2, and 21 P3
- Open questions: 83
- Post-remediation smoke test: 1 additional confirmed finding, fixed in the
  working tree; isolated local browser and HTTP verification complete

## Completed Modules

### M00 - Baseline, Build, CI, Release

- Baseline verified: `e43c266c98cb8121be020fdac95dfe15fcdae2d9`.
- Branch/tag context recorded: `main`; `v1.6.8` points at the baseline.
- Worktree was clean when the fresh audit started.
- CI, release workflow, Docker build, repository hygiene, generated
  frontend contracts, architecture dependency gate, and version sources were
  reviewed.
- No build or Docker command was executed. The review guard rejected those
  commands as not proven read-only.

### M01 - Startup and Lifecycle

- Traced process startup order from executable/home resolution through config,
  timezone, master key, store, bootstrap, first runtime reload, usage recorder,
  admin bootstrap, TLS contract, sessions, handler assembly, and `Serve`.
- Traced the shutdown order: stop maintenance loops and probes, wait for
  background workers, stop HTTP, drain usage, then return to `Run` and close
  the store.
- Reviewed `internal/gatewayapp/run.go`, `internal/gatewayapp/serve.go`,
  `internal/gatewayapp/healthz.go`, `internal/gatewayapp/bootstrap.go`,
  `cmd/gateway/main.go`, `cmd/gateway/bootstrap_admin.go`,
  `internal/runtime/reloader.go`, `internal/probe/runner.go`,
  `internal/usage/recorder.go`, `internal/api/healthz.go`, relevant store
  shutdown/DAOs, and existing lifecycle tests.
- Confirmed four lifecycle findings: initial reload failure is not retried,
  admin bootstrap failure is swallowed, probe shutdown can outlive the deferred
  wait, and usage drain can continue writing after its deadline.

#### M01 Open Questions

- `WatchRetry` is not joined after `stopReloadRetry`. A retry already inside
  `Reload` may still be using the store when `Run` returns and defers
  `db.Close()`. The same pattern applies to SQL statements already in flight:
  cancellation does not guarantee that `database/sql` has returned. Static
  inspection cannot establish whether the process exits before the race causes
  user-visible damage, so this is not a separate confirmed finding.
- The documented shutdown budget in `DOCKER.md` is lower than the maximum
  waits visible in `Serve`. The probe and usage findings cover the substantive
  consequences; the numeric documentation mismatch belongs to M18/M20.
- Existing lifecycle tests cover the TLS contract and parts of usage shutdown,
  but there are no tests for initial-reload fallback/health reporting, admin
  bootstrap error propagation, probe cancellation latency, or the deferred
  close/writer ordering.

### M02 - Config, Snapshot, Routing, Catalog

- Reviewed configuration decoding and validation, defaults, timezone/port
  checks, trusted-proxy/public-origin handling, generated field contracts,
  snapshot construction and atomic publication, routing resolution, bootstrap
  configuration, and catalog model metadata.
- Traced the bootstrap credential path through both database bootstrap and the
  config fallback pool. Confirmed that environment lookup incorrectly
  overrides an explicitly configured key.
- Traced model-capacity fallback from the stored setting through snapshot
  rebuild to `/v1/models`. Confirmed that a corrupt setting leaves zero
  capacity in the runtime snapshot.
- Traced probe configuration presence handling into the probe startup gate.
  Confirmed that a partial `probes` object disables probing instead of applying
  the default.
- Traced bootstrap writes across networks, providers, credentials, models, and
  routes. Confirmed non-atomic ordering, swallowed writes, and the restart
  condition that prevents repair.
- Reviewed routing index construction, explicit disabled-route behavior,
  provider/model direct resolution, model allowlists, and snapshot publication.
  No additional confirmed finding was established in those paths.

#### M02 Open Questions

- `internal/config/config.go:246` restricts bootstrap provider slugs to
  `^[a-z0-9]{2,32}$`, while `internal/admin/provider_handler.go:515-542`
  generates slugs containing hyphens. A round-trip through the documented
  bootstrap format would be rejected. The concrete user workflow that writes
  such a generated slug into bootstrap configuration was not established, so
  this remains a contract inconsistency rather than a confirmed runtime
  finding.
- Bootstrap endpoints and generated routes validate non-empty text but do not
  validate URL scheme, and bootstrap routes do not apply all admin-route
  constraints. `DESIGN.md` does not state a strict validation requirement for
  bootstrap input, so impact is unresolved.
- `internal/routing/routing.go:254-286` takes a nested `RLock` while `Resolve`
  already holds one. Go's `RWMutex` can block a nested reader once a writer is
  queued. Current production paths build a new index and swap it rather than
  mutating a published index, so a reachable deadlock was not demonstrated.
- `internal/routing/routing.go:230-240` indexes provider models by both primary
  ID and provider/model name. Bootstrap provider IDs equal slugs while admin
  provider IDs are generated identifiers. No collision path was found in a
  normal workflow.
- `internal/gatewayapp` has no bootstrap unit tests, and `internal/catalog` has
  no package-local tests. Capacity metadata is covered only through
  `cmd/gateway/features_test.go`.
- The routing benchmark and concurrency tests were inspected for intended
  behavior but not executed. The current audit remains static.

### M03 - Inbound Protocols

- Traced all three inbound codecs from HTTP body reading through protocol
  validation, `ToRosetta` conversion, protocol-private extras, and the ingress
  codec boundary.
- Reviewed OpenAI Chat content-part decoding, Anthropic content/tool-result
  conversion, Responses item/part conversion, namespace/custom/local-shell
  handling, Responses-to-Chat replay, malformed shape handling, and duplicate
  top-level key rejection.
- Confirmed Chat `input_audio`/`file` silent loss, Anthropic text-source
  document silent loss, the missing output-ceiling reservation for TPM/quota,
  and media-blind input estimation.
- Recorded the remaining Responses audio/reasoning/malformed-input concerns as
  open questions where the external contract or desired malformed-input policy
  is not established.

#### M03 Open Questions

- Responses `input_audio` uses gateway-specific flat `audio_url` /
  `audio_data` / `format` fields. The pinned rosetta contract considers
  Responses input to be text/image/file, so a Responses upstream may reject
  this shape while a Chat upstream may translate it. This is a documented
  contract inconsistency; the exact intended compatibility target is not
  established.
- `responsesItemReasoningText` reads only `summary`. The design relies on
  `encrypted_content` for continuation, so reading `content`/`reasoning_text`
  is not confirmed as required.
- Malformed or unknown Responses content parts can decode to an empty
  message and be dropped. Whether this should be an explicit 400 is a policy
  decision, not yet a confirmed defect.
- `internal/ingress` has no package-local tests, so Chat media translation,
  media-aware estimation, and output-ceiling reservations lack direct
  regression coverage.

### M04 - Outbound Protocols

- Traced the `StreamSink` contract into all three encoders, including event
  ordering, item/block boundaries, tool-index remapping, reasoning sealed
  payloads, usage details, terminal sequences, and first-write error capture.
- Reviewed non-stream response encoding for Chat, Responses, and Anthropic,
  including usage bucket translation, tool argument normalization, and the
  downgraded custom/local-shell shapes.
- Reviewed the error classification and error envelopes, including failover
  eligibility, credential cooldown, client-facing redaction, and diagnostic
  detail truncation.
- Confirmed that Chat non-stream encoding drops text when tool calls exist and
  that Anthropic/Responses omit the protocol start for terminal-only empty
  streams.

#### M04 Open Questions

- OpenAI error envelopes never populate `param`. The documented shape allows
  it but does not require a value, and no client workflow was established.
- Every `TransportError` maps to 504 although the design allows 504 or 502 for
  timeout and connection failures. The intended split is not specified.
- Non-ok stream errors always say "upstream stream truncated", even for
  overflow or idle timeout. Usage metadata still distinguishes the causes, so
  this is a diagnostic-fidelity question.
- Non-stream encoders ignore post-header encode errors. Their payloads are
  ordinary serializable types, so a concrete failure was not demonstrated.
- No outwire test covers `WriteOpenAIError`, the complete error status table,
  or terminal-only streams.

### M05 - Dataplane and Failover

- Traced request admission, authentication-derived limits, RPM/TPM checks, key
  and user quota reservations, balance precheck, route and candidate
  resolution, credential selection, target-circuit filtering, attempt budgets,
  streaming and non-streaming attempts, timeouts, failover eligibility,
  reservation cleanup, usage recording, and final error mapping.
- Reviewed `outcome.go`, `policy.go`, `replay.go`, `rate.go`, `balance.go`,
  `cache_affinity.go`, `handler.go`, the package tests, the `cmd/gateway`
  failover/audit/request-budget tests, quota DAO paths, the rate limiter, and
  usage recorder settlement.
- Excluded several candidates after tracing their cleanup: model and
  authorization early returns call `rate.Commit(0)`; pre-upstream client
  cancellation releases reservations; rejected user quota after a successful
  key reservation refunds both levels; probe claims are returned on
  non-transferable and client-cancelled paths; stream write failures are
  classified without penalizing the upstream; gateway-budget expiry does not
  punish the upstream.
- Confirmed one dataplane finding: non-stream downstream write failures are
  recorded as successful and remain eligible for balance charging.

#### M05 Open Questions

- Balance precheck uses the complete candidate list before failover
  enablement, attempt budget, target circuit, and structured-output filtering.
  A costlier target that can never be attempted may cause a 402. The billing
  code documents a conservative estimate, but the intended product behavior is
  not established.
- A request that finds every target's probe slot busy or cooling receives 502.
  Whether the contract should distinguish this capacity condition from an
  upstream failure is unresolved.
- `RateCommit.ReleaseQuota` uses `context.Background()` during cleanup. This
  overlaps the M01 shutdown/store-close risk and is not counted separately.
- `internal/dataplane` lacks a package-local integration test for the complete
  handler early-return and reservation-release matrix. Existing coverage is
  primarily in `cmd/gateway`.

### M06 - Upstream Pool, Probing, Networking

- Traced provider-pool construction and publication, credential selection and
  weighting, cooldown persistence, target circuit state, probe-claim
  handoff, live-target installation, proxy client construction, model
  discovery, provider balance lookup, and the probe runner.
- Reviewed `internal/upstream/upstream.go`, `balance.go`, `proxy_probe.go`,
  `opencode.go`, `trace.go`, the upstream pool/probe/protocol/proxy/cooldown
  tests, `internal/probe/runner.go` and its tests, credential cooldown DAO
  paths, runtime reload inputs, snapshot failure projection, dataplane probe
  claim/release paths, and the relevant design sections.
- Excluded candidates whose cleanup or safety property was verified: local
  pool build before install, live-target state retention, stale half-open
  token cleanup, single half-open probe admission, proxy validation and
  caching, redirect refusal, disabled-provider credential filtering,
  zero-weight fallback, balance-spec path validation, and probe runner
  classification/concurrency bounds.
- Confirmed two findings: an unknown OpenAI used amount is reported as zero,
  and model discovery mishandles the authentication scheme for an
  auto-detected Anthropic provider.

#### M06 Open Questions

- Successful model discovery decodes the response without a body-size limit.
  The known OpenRouter 462-model response is not large enough to establish a
  defect; the intended maximum auxiliary-response size is undefined.
- Proxy connectivity testing uses a fixed Google probe URL. Whether it should
  be configurable, or use a provider-owned endpoint, is not established.
- The probe runner writes its result with the parent context and performs an
  immediate first run before checking cancellation. Both overlap the M01
  shutdown finding; no distinct user-visible consequence was confirmed.

### M07 - Storage, Schema, DAO, Transactions

- Reviewed canonical schema creation, migration/compatibility gates, store
  opening and close behavior, transaction helpers, reservation recovery, and
  every material DAO family: users, access keys, quotas, balances, top-ups,
  usage, usage source and archive, pricing, models, model groups, providers,
  credentials, groups, networks, route targets, probes, audit logs, settings,
  and recent performance.
- Traversed usage insertion, price freezing, pending-cost recomputation,
  balance-charge idempotency, archive pruning, lifetime totals, and
  reservation recovery as complete state machines rather than isolated DAO
  methods.
- Reviewed the corresponding schema, archive boundary, model deletion,
  balance settlement, cost pending, quota reservation, recent performance,
  and stress tests. No build, test, or race command was executed; verification
  remains static.
- Confirmed one finding: a `cost_pending` row that crosses the usage retention
  boundary is archived as a permanently zero-cost record and can no longer be
  recomputed or charged.
- Excluded or downgraded several candidates after tracing them: the archive
  and delete batch intentionally share one transaction; lifetime totals are
  deliberately not decremented during pruning; duplicate balance charges are
  guarded by the `balance_charges` key; the tests cover the main balance
  settlement invariants; and the route-target, random-ID, model-upsert, and
  context candidates have no demonstrated production trigger.

#### M07 Open Questions

- `RecomputeUserUsage` writes the display column `users.used_tokens`, while
  quota admission reads `user_usage_totals.total_tokens`. It has no located
  production caller, so this is not yet a reachable defect.
- `UpsertUpstreamModel` omits `price_cache_creation` and `group_id` from its
  INSERT. Production import currently does not populate those fields.
- Batched pricing uses `context.Background()`, overlapping the M01 shutdown
  and store-close finding.
- Route-target pairing validation is enforced by current admin handlers but
  not by the transactional DAO methods themselves.
- Several generated-ID helpers ignore entropy errors; the observed failure
  mode is an operation failure, not a successful corrupt write.
- `RecomputeCost` reads the prior row and price before opening its write
  transaction. The only current consumer is a single periodic goroutine.
- `FindUnchargedUsage` would also lose recovery data if a positive-cost
  uncharged row crossed the retention boundary. A realistic persistent
  trigger was not established in M07; continue this question in M08/M09.
- Financial recovery tests do not yet cover pending records across pruning,
  authoritative quota-table repair, or the full uncharged/archived lifecycle.

### M08 - Money, Quota, Balance

- Reviewed balance admission and charging, the insufficient-balance pending
  ledger, top-up settlement, charging idempotency, cost recomputation,
  uncharged-usage reconciliation, key/user lifetime-quota reservation and
  release, pricing conversion, provider balance queries, organization billing
  queries, and the related admin balance paths.
- Traced the two financial recovery state machines end to end, including the
  marker keys written during initial charging, pending settlement, and cost
  recomputation, then compared them with the reconciliation join.
- Traced quota reservation release through the dataplane finalizer, store
  updates, startup recovery, and the admission predicate.
- Reviewed the maintainer schedules for recomputation, reconciliation, and
  pruning, plus the balance pending, balance settlement, cost pending, stress
  billing, quota, and reconciliation tests. Verification remains static.
- Confirmed three findings: deferred/recomputed costs can be charged twice,
  failed quota release strands reservations until restart, and malformed
  organization time parameters silently select the default window.
- Excluded or downgraded candidates after tracing the contract: balance
  admission failure is intentionally fail-closed while quota admission failure
  is intentionally fail-open; non-positive charge amounts are documented
  no-ops; duplicate charges for the same marker are intentionally idempotent;
  and the pricing helpers intentionally use the documented conservative
  rounding policy.

#### M08 Open Questions

- `AdjustBalanceWithLedger` can return a successful `Topup` after the top-up
  ledger insert or the subsequent commit fails
  (`internal/store/topup_dao.go:270-295`). This is documented as intentional to
  avoid duplicate top-ups, but there is no durable outbox or automated repair;
  whether the product instead requires a persisted pending-ledger state is a
  policy decision.
- `YuanToMicros` rounds every positive cost below one micro-unit up to one
  micro-unit (`internal/store/balance_dao.go:81-116`). The conservative bias is
  documented and tested; whether the billing contract permits that systematic
  overcharge at sub-micro granularity is unresolved.
- `settlePendingForUser` writes off a residual pending total below one cent
  and deliberately drops debt for unlimited or deleted users
  (`internal/store/balance_dao.go:782-923`). This is documented as the
  settlement policy; the product has not specified whether sub-cent debt must
  be carried indefinitely.
- Balance display in `/me` and admin stats degrades a failed lookup to
  "unlimited" (`internal/admin/user_handler.go:702-717`,
  `internal/admin/stats_handler.go:283-310`). Authoritative admission remains
  fail-closed, and the degradation is logged, so this is not counted as a
  correctness finding without a display-specific contract.
- The admin negative-balance guard reads the current balance and then writes
  a relative adjustment (`internal/admin/user_admin_handler.go:657-689`). The
  code acknowledges the check is non-atomic; a concurrent charge can make the
  requested deduction partially succeed by clamping at zero. No normal-path
  user impact or financial contract violation was established.
- `FindUnchargedUsage` still inherits the retention exposure recorded in M07:
  a positive-cost row that remains uncharged across the usage cutoff is
  pruned before reconciliation can recover it. M08 confirms the marker join but
  did not establish a realistic persistent failure that keeps a single normal
  successful request uncharged for the entire retention period.
- Financial recovery tests do not combine pending settlement with a later
  reconciliation scan, do not reconcile immediately after successful cost
  recomputation, and do not inject a key/user reservation-release failure.

### M09 - Usage and Audit Persistence

- Traced asynchronous batch persistence, retry and per-record fallback,
  synchronous queue-full/shutdown fallback, quota-release callbacks, balance
  charging, usage source/archive reads, audit DAO retention, and AutoReload
  request-body capture.
- Confirmed the source/archive query boundaries: history and CSV deliberately
  use per-request details, while statistics, grouping, and by-user paths merge
  details and daily rollups. CSV truncation reports the total separately.
- Confirmed that audit DAO scanning preserves nullable fields and that audit
  pruning keeps the configured newest rows.
- Confirmed two findings: abandoned batches release only the first ten quota
  reservations, and `audit_log_keep_rows=0` contradicts the documented
  permanent-retention behavior.
- Excluded candidates after tracing them: archive and delete share one
  transaction; audit body capture is bounded and preserves oversized request
  input via `MultiReader`.

#### M09 Open Questions

- The synchronous fallback write-failure log does not include
  `request_id`, `user_id`, or the model. The failure is real, but the
  product's required reconciliation metadata is not specified.
- AutoReload audits successful ordinary writes, while the design's wording can
  be read as requiring failed and bootstrap writes too. Continue this in
  M12/M20 rather than treating it as a confirmed security defect here.
- `internal/usage` has no package-local tests; most behavioral coverage lives
  in `cmd/gateway`. The existing abort tests do not exercise callback-bearing
  batches, and the retry tests do not assert all jobs finish.

### M10 - Identity and Authorization

- Traced the identity chain from data-plane API-key extraction and SHA-256
  lookup through key/user status checks, IP allowlisting, admin lockout, and
  management-session authentication.
- Reviewed session issuance, HS256 algorithm pinning, expiry, `auth_version`
  revocation, cookie properties, logout revocation, bootstrap setup, password
  setup invitations, password changes, and user/admin role checks.
- Reviewed the exact-route management whitelist and the `AdminGateGuard`
  ordering after session authentication. Checked the no-user and empty-ID
  fail-closed paths, cross-user IDOR tests, and the real route/permission
  matrix.
- Confirmed two findings: account-dimension login throttling is bypassable by
  changing username case, and successful session authentication leaves idle
  throttle map entries without a cleanup path.
- Excluded or downgraded several candidates after tracing their contract:
  session tokens do not trust token-supplied roles for authorization; role and
  status are re-read from the store; `auth_version` changes invalidate old
  tokens; the whitelist uses exact method-plus-route matching; empty IDs are
  rejected; cookies include `HttpOnly`, `SameSite=Strict`, scoped paths, and
  conditional `Secure`; and password setup tokens are hashed, expiring,
  one-use, and consumed in the same transaction as the password write.

#### M10 Open Questions

- `authhttp.WriteAuthError` does not explicitly map `auth.ErrUserDisabled` or
  `auth.ErrKeyUnowned`; both fall through to the default 401
  `"authentication failed"`. The required client-visible distinction is not
  specified.
- `auth.Authenticate` calls `snapshot.Get()` separately for the key and its
  owner. A snapshot swap between those reads can combine two snapshot
  generations; the reachable security consequence is unproven.
- `BootstrapSetup` writes the password and closes the bootstrap window before
  checking whether the session manager can issue a token. Normal startup
  generates a secret automatically, so a reachable disabled state was not
  established.
- `SessionTTL` is a hard-coded 8-hour constant while `Manager.TTL()` is the
  actual source of token lifetime. No current behavior depends on the constant;
  it is a future drift point.
- `CheckPasswordSetupLink` is a public endpoint without request throttling.
  Only a SHA-256 and one indexed lookup occur, so a practical denial-of-service
  impact was not established.
- Session-secret ACL tightening is skipped on Windows. The file is created
  with mode `0600`; the required Windows ACL deployment contract is not
  established.
- Authentication tests cover failure accumulation, cooldown, cookie/CSRF
  behavior, cross-user access, route authorization, session revocation, and
  admin lockout. They do not cover case-normalized login throttling or removal
  of idle throttle entries after successful authentication.

### M11 - HTTP Middleware and Routing

- Traced the complete production middleware order from trusted-proxy
  rewriting through body limiting, CORS, security headers, request IDs,
  access logging, public-origin handling, recovery, and route dispatch.
- Reviewed root/public/admin mux registration, 108-route contract coverage,
  the real authorization matrix, static-asset allowlisting, AutoReload
  capture, CSRF/media-type checks, and response-wrapper behavior.
- Confirmed one finding: an oversized admin JSON body is reported as 400
  malformed JSON rather than 413, diverging from the data-plane decoder.
- Excluded or downgraded candidates after tracing them: protected endpoints
  are not exposed through the public mux; recovery does not append JSON after
  a normal committed response; streaming encoders write before flushing;
  public bootstrap/setup routes are intentionally present on both public and
  root muxes; and static serving is restricted to an explicit asset
  allowlist.

#### M11 Open Questions

- `statusResponseWriter.Flush` does not mark the response committed. A
  flush-before-write panic could bypass the committed-response guard, but no
  production path with that order was located.
- The real authorization matrix intentionally omits `/admin/api/` prefix
  fallbacks, leaving trailing-slash redirects, unsupported methods, and
  `HEAD`/`OPTIONS` routing outside the per-endpoint matrix. No bypass was
  established.
- CORS answers `OPTIONS` for unknown non-admin paths before routing. This is
  conventional preflight handling; no sensitive capability exposure was
  demonstrated.

### M12 - Admin Mutation APIs

- Reviewed provider, network, credential, route, route-target, model,
  model-group, group, user, settings, access-key, usage-prune, and
  configuration-transfer mutation workflows, including their relevant DAOs,
  schema constraints, and handler tests.
- Confirmed that provider plus optional credential creation is atomic; route
  creation and head-target updates are transactional; route-target replacement
  is transactional; user role/status updates revoke sessions transactionally;
  access-key field and owner updates are transactional; settings save is
  transactional; password reset increments `auth_version` atomically; usage
  pruning is transactional and rejects non-positive retention; credential
  exclusive switching is transactional; batch price update is one SQL
  statement after validation; and ordinary batch model import is one
  all-or-nothing transaction.
- Verified the model-import idempotency claim directly. The DAO uses
  `ON CONFLICT(provider_id, model_id) DO UPDATE` inside a single transaction,
  disproving the candidate that it only executes plain inserts.
- Confirmed three findings: group PATCH persists an empty name, configuration
  dry-run undercounts same-named models across multiple new providers, and a
  key owner can remove an admin-applied key model allowlist despite the
  documented no-loosening rule.

#### M12 Open Questions

- Model-group assignment and unassignment execute one autocommit update per
  model. A mid-loop database failure can leave a partial result, but no
  deterministic failure path or mid-loop failure test was established.
- Credential creation/update followed by a separate exclusive-enable write can
  leave a disabled or partially updated credential if the enable fails. A
  normal user-reachable trigger was not established.
- Configuration import treats row-level write failures as warnings and
  continues within the outer transaction. The documented bad-row isolation
  contract does not settle whether database write failures should abort the
  complete import.

### M13 - Admin Query APIs

- Reviewed the admin stats, usage summary/grouping/history/CSV, usage-by-user,
  audit, probe summary/timeline, and route-catalog query APIs, including their
  DAOs, tests, authorization boundaries, and frontend callers.
- Confirmed admin-only query endpoints are protected by the exact-route
  authorization matrix or handler checks; `callerScope` consistently binds
  ordinary users to their own data.
- Verified parameterized usage filters, fixed grouping identifiers, stable
  history pagination, non-null empty grouped responses, CSV truncation
  headers, route-catalog field filtering, and probe group scoping.
- Confirmed one finding: the audit endpoint silently ignores a malformed
  `limit` and returns a default-sized 200 response instead of rejecting the
  explicit invalid parameter.

#### M13 Open Questions

- Unknown probe granularity values other than `day` silently use hour
  granularity.
- Probe summary window counts and last-record metadata use different time
  boundaries, so an old last error can be displayed beside a zero window
  count. Health classification itself remains `unknown` in that case.
- Streamed grouped usage responses can encounter a scan error after the JSON
  status has already been committed, but no deterministic normal-data trigger
  was established.
- Route catalog fields are collected with five independent reads and can
  represent slightly different snapshots.
- Archived-day overlap counting is conservative and intentional; its boundary
  behavior is documented and tested.

### M14 - Billing and Usage Pipeline

- Reviewed balance admission and charging, top-up settlement, pricing
  conversion, usage batch persistence and per-record fallback, quota
  reservation/release, pending-cost recomputation, uncharged-usage
  reconciliation, and the startup maintenance workers.
- Confirmed two findings: reconciliation can remain permanently occupied by
  insufficient-balance rows and misreport them as charged; actual usage
  pricing bypasses the effective-price fallback used by precheck.
- Excluded the chain-wide preflight maximum as an intentional conservative
  policy, while recording the failover-disabled cost as an open question.
- Noted that batch charging preserves idempotency and rollback semantics, and
  that no package-local reconciliation-worker test exists.

#### M14 Open Questions

- Balance preflight estimates every resolved candidate before failover flags,
  circuit filtering, structured-output filtering, and target caps are applied.
  A disabled-failover route with an expensive unused backup can therefore be
  rejected as unaffordable. The current contract favors a conservative chain
  maximum; the product decision on restricting the estimate is unresolved.
- `RecomputePendingCosts` has no stable ordering and can remain occupied by
  rows whose recomputed charge is permanently insufficient. This is folded
  into M14-01 rather than duplicated.
- `AdjustBalanceWithLedger` commits a successful balance adjustment when the
  ledger insert fails and returns success with an ERROR log. There is no
  durable outbox or automatic repair, so the documented manual-reconciliation
  policy remains an operational question.
- `settlePendingForUser` writes off sub-cent debt and drops debt for unlimited
  or deleted users by design; whether sub-cent debt must be carried
  indefinitely is not specified.
- The usage recorder has no package-local tests for the reconciliation
  worker's retry, ordering, or accounting summary. Financial recovery tests
  cover individual settlement and idempotency cases but not a multi-page
  backlog with terminally deferred rows.

### M15 - Frontend Foundation

- Reviewed `web/src/api.ts`, `main.ts`, `router.ts`, `App.vue`, `types.ts`,
  `ui.ts`, `fmt.ts`, `range.ts`, `password.ts`, `protocols.ts`,
  `modelPrice.ts`, `routeTrace.ts`, the shared composables, and generated
  route, DTO, and default contracts.
- Traced startup session probing, login and logout, bootstrap setup,
  one-time password setup, authenticated request construction, error parsing,
  token storage, redirect validation, route guards, and generated-contract
  coverage.
- Confirmed three findings: login authentication failures are rewritten as
  expired sessions, bootstrap can report failure after committing the
  password, and bootstrap-status read failures are presented as an ordinary
  login form.
- Verified the route and DTO contracts are generated and tested, redirects
  reject protocol-relative targets, and the frontend password-strength
  classifier matches the backend Unicode rules.

#### M15 Open Questions

- `range.ts` documents a half-open interval while the backend usage SQL uses
  an inclusive `ts <= to`; the frontend supplies the current millisecond, so
  duplicate inclusion was not shown to affect displayed totals.
- A `/me` failure caused by rate limiting or a server error is collapsed into
  “not logged in”; whether it should instead show a retryable degraded state
  remains a product decision.
- Shared password/range helpers have no direct tests, and there is no
  component-level coverage of login, bootstrap, password setup, or route
  guards.

### M16 - Frontend Pages

- Reviewed every route-level page in `web/src/views`, including the
  permission-specific views and the nested provider detail page, plus the
  shared credential, heartbeat, modal, table, picker, and tab components used
  by those pages.
- Traced page loading, empty/error separation, mutation payloads, role-visible
  controls, credential and secret handling, navigation-driven reloads, and
  the backend contracts those pages depend on.
- Confirmed three findings: a provider balance can survive navigation and
  appear under another provider, probe timeline responses can overwrite a
  newer selection, and direct navigation to the price tab can render an
  unloaded catalogue as an empty configuration.
- Verified the key, user/group, route-chain, provider-detail, and usage-export
  paths against their handlers and contracts; the earlier export-stale-filter
  candidate was cleared because navigation is awaited before reading filters.

#### M16 Open Questions

- Network deletion and saving have no per-row pending guard; duplicate clicks
  can produce redundant requests, with the backend returning normal
  not-found/conflict outcomes. No incorrect state was established.
- The model-group member dialog keeps a group snapshot; concurrent deletion
  can leave stale modal data after a refresh, but the normal flow was not
  shown to render incorrect membership.
- The probe timeline loading flag is global across cards, so toggling one
  model makes every drawn card show a loading state. This is a presentation
  granularity issue, not a data-integrity defect.

### M17 - Frontend Components

- Reviewed every reusable component under `web/src/components` and traced the
  material callers of Tabs, ModelPicker, HeartbeatBar, AppModal, DataTable,
  DetailDrawer, UpstreamModelPanel, UsageByGroup, and CredentialPanel across
  the route-level pages.
- Checked controlled props and emitted updates, keyboard/focus behavior,
  accessibility relationships, loading and error states, stale-response
  guards, and mutation semantics at the component/page boundary.
- Confirmed three findings: three Tabs callers emit dangling
  `aria-controls` relationships, “select all visible” removes hidden model
  selections, and HeartbeatBar bucket details are pointer-only.
- Verified that the modal focus/locking model, table state separation,
  UsageByGroup request sequencing, Tabs indicator mechanics, and
  UpstreamModelPanel route-head protection are consistent in the inspected
  paths. The CredentialPanel balance mismatch remains counted once under
  M16-01.

#### M17 Open Questions

- `UsageByGroup` counts canceled requests in the success-rate denominator by
  documented choice; the desired metric remains unconfirmed.
- Its dynamic error block is not a live region, so reader announcement is
  uncertain.
- `UsageDetail` does not clear the selected row when filters or pages reload;
  a normal navigation path that leaves a misleading drawer open was not
  established.
- AppModal focus restoration for a simultaneously unmounted nested close
  control is unverified.
- Shared components have no component-level tests for ARIA, selection
  merging, modal focus, or keyboard chart access.
- A zero-count probe bucket would produce a `NaN` rate; no such backend
  response path was confirmed.

### M18 - WebUI Embedding, Deployment, CI

- Reviewed the embedded frontend contract from `web/` build output through
  `web/sync-embed.mjs`, `internal/webui`, static serving, CSP and version
  tests, then the Docker image, runtime entrypoint, Compose documentation,
  local PowerShell launcher, and both CI/publish workflows.
- Confirmed four findings: manual Docker dispatch can replace a published
  version tag, the detached launcher probes the static admin page instead of
  `/healthz`, the launcher force-kills any port owner or stale PID, and the
  fine-grained Compose example mounts key files it never creates.
- Verified that the embed sync, asset allowlist/cache policy, CSP, version
  checks, non-root container path, Docker build gates, and documented
  `/healthz` implementation agree with the inspected contracts.

#### M18 Open Questions

- The container health check assumes loopback reachability after parsing the
  configured port. The documented container configuration uses
  `0.0.0.0:8666`; support for a concrete non-loopback bind address is not
  established.
- The documented 24-second shutdown budget can exceed the maximum waits
  described by M01. The underlying shutdown defects are already counted under
  M01; the documentation drift belongs to M20.

### M19 - Test Quality

- Reviewed the CI and release workflow test gates, the `testing.Short()`
  boundary in the performance and billing-stress suites, and the existing
  frontend contract tests.
- Traced the default, race, nightly, and Docker release commands and compared
  them with the actual set of tests that honor `-short`.
- Confirmed one finding: the fast gate still runs many unguarded performance,
  leak, concurrency, and multi-transaction stress tests that the CI comments
  and nightly split claim are excluded.
- Verified that the nightly timeout is longer than the default timeout and
  that the existing frontend test files contain real assertions; neither
  produced a separate finding.

#### M19 Open Questions

- Package-local coverage remains thin in `internal/gatewayapp`,
  `internal/ingress`, `internal/usage`, and `internal/catalog`; related gaps
  are already noted in M02, M03, and M09.
- Frontend coverage remains limited to API, DTO, and route-trace contract
  tests; page and component behavior still has no direct test layer.
- The nested `go test` in `authz_matrix_test.go` does not inherit the outer
  race flag, but no reachable race was established in that path.
- Fixed timing tolerances in performance tests are an operational policy
  question; this audit did not execute them on the supported runners.

### M20 - Documentation and Final Deduplication

- Reviewed `README.md`, `DESIGN.md`, `DOCKER.md`, and `CONTRIBUTING.md`
  against the implementation paths inspected in M00-M19.
- Confirmed four documentation defects: the nominal shutdown budget omits
  the reconciliation wait, several statements still call usage retention
  fixed at 30 days, the deployment guide reverses the effective timezone
  precedence, and the design document says local race testing is unavailable
  despite the documented `CC` setup.
- Cross-checked overlap with earlier findings. The lifecycle consequences of
  shutdown are counted once under M01, partial bootstrap once under M02,
  API-key precedence once under M02, and the release/tag risk once under M18.
  M20 records only distinct documentation drift.
- Completed a severity review and a cross-module deduplication pass. The
  consolidated result is in `AUDIT-REPORT.md`.

#### M20 Open Questions

- Actual shutdown duration was not measured because no process, signal, or
  container command was executed. M20-01 is a static sum of visible waits;
  the unbounded paths remain M01 findings.
- Whether an old local compiler prerequisite deserves a historical note
  rather than replacement is an editorial decision.
- The documentation review covered material contract and operator statements,
  not every historical example or release note.
- No documentation build or link checker was run, so rendered formatting and
  anchors were not verified.

## Next Entry Point

The audit is complete and the remediation plan has been revised. Use
`REMEDIATION-PLAN.md` as the design entry point:

- 54 findings map once each to eight primary root causes (`RC1`-`RC8`).
- `requestlane` is a `control` module, not a `data` module, because it invokes
  billing and accounting commands.
- R0 safety containment comes before structural refactors.
- R2 effective configuration precedes R1 lifecycle ownership.
- R7 persistence primitives precede R3 accounting state migration.
- R11 accessibility follows R10 component/state contracts.
- R12 delivery closeout waits for the runtime contracts it must verify.
- Each phase now has explicit deliverables, acceptance evidence, risk
  controls, rollback/containment, and an expected change scope.
- Lifecycle and persistence transactions are separate phase gates because
  they have different failure evidence and rollback boundaries.
- The earlier README-only plan is still the audit plan; changes in this
  revision are limited to `REMEDIATION-PLAN.md` and this progress note.
- Target package names are architectural ownership labels, not a mandate for
  rename-only churn. Existing packages move only when the responsibility and
  dependency contract changes.

## Latest Planning Consistency Pass

Completed on 2026-10-25:

- Verified 13 phase gates (`Phase 0` through `Phase 12`) and 13 workstreams
  (`R0` through `R12`).
- Verified the per-finding ownership table contains 54 distinct rows, matching
  the 54 confirmed findings.
- Removed stale workstream ownership: `M08-03` is now only under R9, `M12-02`
  is under R7, and `M20-04` is under R12.
- The per-finding table is now the sole ownership authority. Workstream finding
  lists are explicitly implementation touchpoints, so staged containment and
  structural work cannot be mistaken for duplicate ownership.
- Corrected the dependency graph so accounting and transport are prerequisites
  or parallel contracts for requestlane, not its downstream children.
- Added the missing R0, R4, and R7 dependency rows, made R10 depend on R9, and
  made R12 depend on all runtime contracts plus every workstream's closure
  evidence.
- Added explicit Phase 4 acceptance for configuration cardinality and Phase 9
  acceptance for malformed organization time parameters and service-owned
  validation results.

Current remediation status: Phase 0 design baseline is next. This pass changed
only `REMEDIATION-PLAN.md` and `PROGRESS.md`. No source, configuration, test,
deployment, or database change has been made. A future implementation task must
use a new, explicit file boundary rather than this planning lock.

## R0 Detailed Design Gate

Completed on 2026-10-25:

- Added the R0 detailed technical design to `REMEDIATION-PLAN.md`, covering
  all five R0 findings: `M01-01`, `M01-02`, `M05-01`, `M08-01`, and
  `M10-01`.
- Defined five independently reviewable implementation slices:
  - R0-A: canonical accounting completion marker.
  - R0-B: canonical login throttle identity.
  - R0-C: typed bootstrap outcomes and fatal startup failures.
  - R0-D: dirty/retry state for failed initial runtime rebuilds.
  - R0-E: typed non-stream encoding/write results across all three protocols.
- Fixed the ownership boundary for each slice:
  - M08-01 containment moves to R3's accounting state machine.
  - M10-01 containment moves to R8's principal identity model.
  - M01-02 containment moves to R1's lifecycle readiness.
  - M01-01 containment moves to R1's typed lifecycle state.
  - M05-01 containment moves to R5's requestlane commitment model.
- Added the `R0-AC-01` through `R0-AC-15` acceptance matrix. It covers
  pending settlement, recomputation, marker rollback, concurrency, mixed-case
  login budgets and reset, bootstrap read/write/concurrent-create outcomes,
  initial reload retry and health, all three non-stream codecs, non-billing,
  no upstream penalty, and successful-response compatibility.
- Added explicit schema/compatibility rules: R0 performs no migration, uses
  the existing original `(request_id, user_id)` marker, and keeps the existing
  recomputation key for delta idempotency.
- Added the R0 handoff/deletion-point table so every temporary containment
  adapter has a final owner and closure evidence.
- Linked Phase 1 deliverables, acceptance, and risk controls to the detailed
  design and acceptance IDs.

R0 implementation has not started. Phase 0 must first capture each failure as
a reproducible failing or explicitly synthetic fixture and record the baseline
metrics required by the phase gate. No source, test, configuration, generated
artifact, deployment, or database command was executed in this pass. Only
`REMEDIATION-PLAN.md` and `PROGRESS.md` were updated.

## R0-A Containment Verified

Updated on 2026-10-25:

- Added the four R0-A acceptance fixtures for `R0-AC-01` through `R0-AC-04`
  in `internal/store/balance_settle_test.go`.
- Implemented the minimal no-schema containment design:
  - `ensureUsageCompletionTx` writes the existing original
    `(request_id, user_id)` marker.
  - Pending settlement marks fully settled rows inside the top-up transaction
    before deleting them; partial rows retain their remaining debt.
  - Reconciliation excludes requests that still have a `balance_pending` row,
    preventing the settlement/reconciliation race from charging twice.
  - Successful recomputation writes the original marker in the same
    transaction as the recompute-specific marker and `usage_records` update.
- Preserved existing compatibility: no migration, no new table, and the
  recompute-specific idempotency key remains in use.
- Verified with the user-provided PowerShell output:
  - targeted `TestR0AC01` through `TestR0AC04`: `ok ... 0.214s`.
  - full `internal/store` package: `ok ... 17.069s`.
- Recorded the exact test evidence in `EVIDENCE.md`.

Status:

- `M08-01` R0-A containment: verified.
- `R0-A`: complete at the containment scope.
- R3 final accounting state machine: not started and not claimed complete.
- Other R0 slices (`R0-B` through `R0-E`): see below.

## R0-B Containment Verified

Updated on 2026-10-25:

- Added `TestR0AC05_LoginAccountThrottleUsesCanonicalCaseAcrossIPs` and
  `TestR0AC06_SuccessfulLoginClearsCanonicalAccountBudget` in
  `internal/admin/scope_test.go`.
- The pre-fix run reproduced the bypass: the sixth mixed-case attempt from a
  new IP returned 401 instead of 429.
- Added the login-local `canonicalLoginThrottleKey`: trim, then fold ASCII
  `A-Z` only, matching SQLite `NOCASE`. The same key is now used for database
  lookup and all account-throttle operations.
- Verification:
  - targeted `TestR0AC05` / `TestR0AC06`: pass.
  - existing account-throttle and login regression tests: pass.
  - full `go test -count=1 ./internal/admin`: pass (`7.333s`).

Status:

- `M10-01` R0-B containment: verified.
- `R0-B`: complete at the containment scope.
- R8 principal identity model: not started and not claimed complete.
- R0-C through R0-E: see below.

## R0-C Containment Verified

Updated on 2026-10-25:

- Added `TestR0AC07_BootstrapReadFailureIsFatal`,
  `TestR0AC08_NonUniqueCreateFailureIsFatal`,
  `TestR0AC09_ConcurrentCreateIsVerifiedByReRead`,
  `TestR0AC09_UnverifiedUniqueConflictIsFatal`, and
  `TestInitializeAdminBootstrap_PropagatesFailure` in
  `cmd/gateway/bootstrap_admin_test.go`.
- Captured the pre-fix behavior in a temporary worktree at baseline
  `e43c266`: all three failure classes returned nil from
  `initializeAdminBootstrap`.
- Changed bootstrap to return explicit `setup_required`, `ready`, and `failed`
  outcomes. `CountUsers`, initial `CreateUser`, and post-bootstrap
  `FindUninitializedAdmin` failures now propagate as startup errors. A unique
  create conflict is accepted only when a re-read proves that the expected
  administrator exists.
- Changed startup to refuse service when the post-bootstrap
  `FindUninitializedAdmin` read fails.
- Verification:
  - targeted R0-C fixtures: pass.
  - full `go test -count=1 ./cmd/gateway`: pass (`72.346s`).
  - full `go test -count=1 ./internal/gatewayapp`: pass.

Status:

- `M01-02` R0-C containment: verified.
- `R0-C`: complete at the containment scope.
- R1 lifecycle readiness model: not started and not claimed complete.
- R0-D through R0-E: see below.

## R0-D Containment Verified

Updated on 2026-10-25:

- Added `TestR0AC10_RebuildFailureMarksDirtyUntilSuccessfulRetry` in
  `internal/runtime/reloader_test.go`.
- Captured the pre-fix failure in a temporary worktree at baseline
  `e43c266`: a failed rebuild left `IsDirty()` false.
- Changed `Reload` so both the pool-preparation and snapshot-rebuild failure
  paths call the existing `MarkDirty` before returning. The configuration
  fallback in `gatewayapp.Run` remains unchanged.
- Verification:
  - targeted `TestR0AC10`: pass, covering 503 degraded then 200 healthy after
    a successful retry.
  - `go test -count=1 ./internal/runtime ./internal/gatewayapp ./internal/api`:
    pass.

Status:

- `M01-01` R0-D containment: verified.
- `R0-D`: complete at the containment scope.
- R1 typed lifecycle readiness: not started and not claimed complete.
- R0-E: see below.

## R0-E Containment Verified

Updated on 2026-10-25:

- Added `TestR0AC12_NonStreamWritersReportWriteFailure`,
  `TestR0AC12_NonStreamWritersDoNotReportSuccessBeforeBodyIsWritten`, and
  `TestR0AC12_EncodeFailureIsNotReportedAsCommitted` in
  `internal/outwire/nonstream_write_result_test.go`.
- Added `TestR0AC13_NonStreamDownstreamWriteFailureIsTerminalAndNotCharged`
  for all three ingress protocols and
  `TestR0AC14_NonStreamWriteFailureDoesNotPenalizeTargetOrCredential` in
  `cmd/gateway/nonstream_write_failure_test.go`.
- Captured the pre-fix failure: all three non-stream codecs hid the write
  error and recorded `UsageStatusOK`; the Chat path charged 3 yuan and wrote a
  `balance_charges` row.
- Introduced `outwire.NonStreamWriteResult`. Each non-stream writer now
  marshals the complete payload before committing headers, propagates the
  first body write error, and rejects short writes.
- `ingress.Codec.WriteNonStream` now returns the typed result.
  `attemptNonStream` treats a committed write error as terminal:
  `canceled` / `downstream_write_failed`, no charge, and no target or
  credential penalty. A pre-commit encode failure renders the protocol-native
  500 envelope and is also terminal and non-billable.
- Verification:
  - targeted R0-AC-12 through R0-AC-14: pass.
  - 3x3 non-stream protocol matrix: pass.
  - full `go test -count=1 ./internal/outwire ./internal/dataplane
    ./cmd/gateway`: pass.

Status:

- `M05-01` R0-E containment: verified.
- `R0-E`: complete at the containment scope.
- R5 requestlane commitment model: not started and not claimed complete.
- R0: complete at the containment scope. Structural lifecycle, accounting,
  requestlane, and identity work remains assigned to R1, R3, R5, and R8.

## R2 Effective Configuration Verified

Updated on 2026-10-25:

- `M02-01`: added one shared `BootstrapCredential.ResolveAPIKey` precedence
  function. Both database bootstrap and the config-only runtime pool now use
  it; a non-empty `api_key` wins and `api_key_env` is consulted only when
  `api_key` is empty.
- `M02-02`: a corrupt `model_defaults` setting still selects the store's
  documented 128K/64K fallback, but `snapshot.RebuildFromDB` now returns the
  read error instead of publishing an implicit zero-capacity snapshot. The
  existing dirty/retry lifecycle makes the degraded state visible.
- `M02-03`: probe defaults now track presence of the `enabled` key separately.
  A partial `probes` object still defaults probing on; only explicit `false`
  disables it.
- `M09-02`: `audit_log_keep_rows=0` now survives defaulting and, together with
  `-1`, disables audit pruning. This matches the field comment and makes the
  compliance setting enforceable.
- `M20-02` / `M20-03`: README, DESIGN, and DOCKER now describe configurable
  usage retention rather than a fixed 30-day detail lifetime, and document
  non-empty config `timezone` as the final override with `TZ` as fallback.

Verification:

```text
go test -count=1 ./internal/config ./internal/upstream ./internal/snapshot ./internal/gatewayapp ./internal/store ./cmd/gateway
ok internal/config
ok internal/upstream
ok internal/snapshot
ok internal/gatewayapp
ok internal/store
ok cmd/gateway
```

Status:

- `M02-01`, `M02-02`, `M02-03`, `M09-02`, `M20-02`, and `M20-03`: verified.
- R2 effective configuration: complete.
- R1 lifecycle ownership: verified (see R1 section below).

## R1 Lifecycle Ownership Verified

Recorded on 2026-10-25. R1 supersedes the R0 lifecycle adapters while keeping
their public safety behavior.

Implemented:

- `internal/lifecycle/readiness.go` owns process phase, bootstrap state,
  database/runtime/bootstrap readiness dimensions, registered-worker counts,
  and the explicit join barrier.
- `/healthz` is projected through `lifecycle.Checker`; the legacy wrapper only
  adapts existing command-local tests and is no longer the production path.
- `gatewayapp.Run` returns a typed `lifecycle.BootstrapState` from the
  bootstrap hook, records the reload-retry and usage owners, and closes storage
  only after all joins are proven.
- `gatewayapp.Serve` binds the listener before starting maintenance workers,
  never publishes `ready` after a startup failure, registers each maintenance
  worker that actually starts, decrements the count on exit, and reports
  `ErrWorkersNotJoined` when a deadline expires without exit evidence.
- `internal/probe/runner.go` uses a cancelable stagger timer and refuses to
  write or prune after cancellation.
- `internal/usage/recorder.go` rejects admission after close, exposes
  `CloseQueue`/`Join`/`Accepting`, and `Wait` only succeeds when the worker
  exit is observed and no pending batch remains.
- Shutdown limits have one source: `lifecycle.DefaultShutdownBudget` (26s
  nominal, 30s upper bound, 31s Compose grace). `DOCKER.md` is compared to the
  constants by `budget_doc_test.go`.

Verification:

```text
PS> go test -count=1 ./internal/lifecycle ./internal/api ./internal/gatewayapp ./internal/probe ./internal/usage
PASS

PS> go test -race -count=1 ./internal/gatewayapp ./internal/lifecycle ./internal/usage ./internal/probe
PASS

PS> go test -count=1 ./cmd/gateway -run 'TestUsageShutdown|TestRecordAfterWait|TestRecordConcurrentWithWait|TestWaitDoesNotDeadlock'
PASS
```

- Startup failure injection covers an occupied listener before workers start.
- Slow-writer tests prove a timed-out join does not become storage-close
  permission.
- Probe cancellation is bounded to the cancelable stagger and performs no
  post-cancel writes.
- `M01-01`, `M01-02`, `M01-03`, `M01-04`, and `M20-01` are fixed at the R1
  scope. M20-01 remains a final delivery check in R12.

## R7 Persistence Command Boundaries Verified

Recorded on 2026-10-25.

Implemented:

- `bootstrapDB` reads existing provider state with error propagation and
  creates the complete network/provider/credential/model/route graph in one
  `Store.RunTx` transaction.
- Failure at any child write rolls back every earlier child and never writes
  the completion marker.
- Added the independent `config_bootstrap_completed` setting. It is distinct
  from `bootstrap_completed`, which belongs to first-administrator password
  setup.
- A complete graph from an older binary is accepted only after
  `verifyLegacyBootstrapGraph` validates every configured network, provider,
  credential, model, and route; the new marker is then backfilled.
- A non-empty but incomplete or unverifiable graph is refused instead of
  published as ready.
- Dry-run and real configuration import count model cardinality through the
  same provider-scoped model identity.

Regression coverage:

```text
TestR7_BootstrapReadFailureDoesNotWritePartialGraph
TestR7_BootstrapChildFailureLeavesNoPartialGraph
TestR7_BootstrapCommitsCompleteGraphAndCompletionMarker
TestR7_ExistingGraphWithoutMarkerIsNotReportedComplete
TestR7_CompleteLegacyGraphIsVerifiedAndMarked
TestR7_BootstrapEncryptFailureRollsBackEarlierWrites
TestR7_DryRunAndImportAgreeOnDuplicateModelsAcrossNewProviders
TestR7_ModelCardinalityIsProviderScoped
```

Status: `M02-04` and `M12-02` fixed at the R7 scope.

## R3 Accounting State Machine Verified

Recorded on 2026-10-25.

Implemented:

- Retention treats pending and unsettled positive successful usage as
  recoverable work. `cost_pending` and successful positive-cost rows without
  either a canonical `balance_charges` marker or a `balance_pending` row stay
  in the detail table; the prune watermark uses the same eligibility
  predicate.
- Actual pricing and preflight pricing both resolve through
  `Prices.EffectivePrices`; partial input/output price configuration no longer
  charges the missing side at zero.
- Reconciliation records typed `charged`/`deferred`/`failed` outcomes,
  persists retry state, and cannot let a full page of unaffordable rows block
  newer chargeable rows. Only an actual wallet debit increments `charged`.
- Added `quota_reservation_leases` with request-level ownership and an explicit
  lease deadline. Key and user reservations for one request are released in
  one transaction; failed releases retain the lease.
- Startup and periodic recovery only clear expired leases; a failed release
  no longer waits for a process restart.
- If a lease row is missing because its write was lost, the dataplane
  finalizer falls back to the checked direct release and keeps its in-memory
  reservation when that write also fails.
- Abandonment of a usage batch executes every quota callback; only the logged
  request-ID sample is capped at ten.
- Compatibility setup creates `quota_reservation_leases` and
  `idx_usage_charge_retry` on databases already stamped with the current
  schema version.

Verification:

```text
PS> go test -count=1 ./internal/store ./internal/dataplane -timeout 240s
ok github.com/cn-maul/rosetta-gateway/internal/store 18.741s
ok github.com/cn-maul/rosetta-gateway/internal/dataplane 0.155s

PS> go test -count=1 ./internal/usage ./internal/gatewayapp ./cmd/gateway -timeout 300s
ok github.com/cn-maul/rosetta-gateway/internal/usage
ok github.com/cn-maul/rosetta-gateway/internal/gatewayapp
ok github.com/cn-maul/rosetta-gateway/cmd/gateway
```

Status: `M07-01`, `M08-01`, `M08-02`, `M09-01`, `M14-01`, and `M14-02`
fixed at the R3 scope.

## R4 Protocol Model and Stream Lifecycle Verified

Recorded on 2026-10-26.

Implemented:

- Chat inbound `input_audio` and `file` parts now map to normalized audio/file
  blocks, including mutually exclusive `file_data`/`file_id` handling. A
  malformed media part is no longer silently reduced to absent content.
- Anthropic text-source documents now become normalized text instead of being
  skipped by the document converter.
- The ingress estimator counts images, audio, files, inline audio/file payload
  size, filenames, MIME types, signatures, and sealed reasoning. Admission and
  usage fallback share this estimator.
- Route resolution precedes admission estimation. An omitted client output cap
  reserves against the largest configured `MaxOutputTokens` in the resolved
  candidate chain; an explicit client cap wins.
- TPM is reserved after key/user quota checks using the same estimate, so a
  quota rejection is not masked by a TPM rejection and cannot consume TPM.
- Responses unsupported-tool errors enumerate every accepted tool type.
- Chat non-stream responses retain non-empty text alongside `tool_calls`.
- Anthropic and Responses `Finish("ok")` ensure protocol start events for
  terminal-only empty streams.

Regression coverage:

```text
TestR4_ChatInputAudioAndFileArePreserved
TestR4_ChatFileIDUsesOnlyFileID
TestR4_AnthropicTextDocumentIsPreserved
TestR4_UnsupportedResponsesToolErrorListsEveryAcceptedType
TestR4_InputEstimateCountsMedia
TestR4_InlineFileEstimateScalesWithPayload
TestR4_OutputCeilingUsesResolvedChainWhenClientOmitsCap
TestR4_ClientOutputCapWinsOverChainCeiling
TestR4_AnthropicTerminalOnlyStartsMessage
TestR4_ResponsesTerminalOnlyStartsResponse
TestR4_ChatNonStreamKeepsTextWithToolCalls
```

Verification:

```text
PS> go test -count=1 ./internal/ingress ./internal/inwire ./internal/outwire ./internal/dataplane ./internal/ratelimit
PASS

PS> go test -count=1 ./internal/config ./internal/upstream ./internal/usage ./internal/gatewayapp ./cmd/gateway -timeout 420s
PASS
```

Status: `M03-01`, `M03-02`, `M03-03`, `M03-04`, `M03-05`, `M04-01`, and
`M04-02` fixed at the R4 scope. R5 retains final request-lane ownership of
admission/commit accounting.

## R5 Request Lane and Commitment Ownership Verified

Recorded on 2026-10-26.

Implemented:

- `Outcome.Kind` is now the authoritative attempt terminal classification.
  Compatibility booleans are projections filled from that kind; stream and
  non-stream paths no longer independently reinterpret flag combinations.
- One `LaneDecision` maps committed success, partial completion, gateway
  overflow/timeout, client abort, downstream write failure, and encode failure
  to the same usage state, target/credential penalty, diagnostic status, and
  route-trace vocabulary.
- A request-context cancellation is authoritative over a simultaneously wrapped
  upstream error, so a disconnected non-stream caller is persisted as
  `canceled`, not `error`.
- Balance preflight now receives the exact candidate set the request lane may
  attempt: disabled failover and the configured attempt budget are applied
  before preflight, matching the chain actually reachable by the request.

Regression coverage:

```text
TestR5_AttemptOutcomeProjectsOneLaneDecision
TestR5_OutcomeConstructorsCannotExpressConflictingBooleans
TestR5_ClientCancellationWinsOverWrappedUpstreamError
TestR5_BalancePreflightUsesAttemptSet
```

Verification:

```text
PS> go test -count=1 ./internal/dataplane ./internal/ingress ./internal/inwire ./internal/outwire ./internal/usage ./internal/ratelimit -timeout 180s
PASS

PS> go test -count=1 ./internal/config ./internal/upstream ./internal/usage ./internal/gatewayapp ./cmd/gateway -timeout 420s
PASS (cmd/gateway 73.495s)
```

Status: `M03-03`, `M05-01`, and `M14-02` fixed at the R5 scope. Next: R6
transport discovery and balance contracts.

## R6 Transport Discovery and Balance Contracts Verified

Recorded on 2026-10-26.

Implemented:

- OpenAI balance calculation now requires a usable usage reading. When the
  subscription limit is known but usage is unavailable, malformed, missing
  `total_usage`, or times out, the credential and provider are reported as
  `unknown` with no amount instead of a precise `hard_limit - 0`.
- Model discovery resolves the effective protocol before selecting auth
  headers. A stored `auto` provider now shares the same Anthropic detection
  outcome used by data-plane client construction, then sends `x-api-key` plus
  `anthropic-version` rather than Bearer.
- The public balance DTO and frontend now carry the `unknown` state and render
  it separately from success, unsupported, and error.

Failure-first evidence:

- Before the fix, all four usage-failure cases in
  `TestR6_OpenAIBalanceDoesNotInventZeroUsed` returned `status=ok` with an
  invented `100.00 USD` amount.
- Before the fix, `TestR6_AnthropicDiscoveryUsesResolvedProtocolAuth/auto`
  returned HTTP 401 while the explicit-anthropic case passed.

Regression coverage:

```text
TestR6_OpenAIBalanceDoesNotInventZeroUsed
TestR6_AnthropicDiscoveryUsesResolvedProtocolAuth
```

Verification:

```text
PS> go test -count=1 ./internal/upstream -timeout 180s
ok github.com/cn-maul/rosetta-gateway/internal/upstream

PS> go generate ./internal/admin
PS> cd web; npm test
PASS 25/25

PS> cd web; npm run typecheck
PASS
```

Status: `M06-01` and `M06-02` fixed at the R6 scope. `M02-01` remains verified
by R2 and receives no duplicate implementation. Next: R8 identity and
authorization invariants.

## R8 Identity and Authorization Invariants Verified

Recorded on 2026-10-26.

Implemented:

- `store.CanonicalUsername` is now the single ASCII/NOCASE identity
  implementation. It trims surrounding whitespace and folds ASCII A-Z exactly
  like SQLite `COLLATE NOCASE`. Store reads, store writes, admin account
  creation, and login throttle keying all use it.
- `FailureThrottle.Release` deletes entries that become idle after success and
  invokes the same bounded sweep used by `Fail`. The hard cap now applies to
  successful authentication traffic as well as failed traffic.
- Owner key PATCHes treat an existing key-level model allowlist as a
  restriction: the owner may preserve or narrow it, but cannot clear,
  replace, or widen it. An explicit admin PATCH remains the widening command.

Failure-first evidence:

- Before the fix, `TestR8_SuccessfulAttemptsLeaveNoIdleEntries` retained all
  6000 entries after `Allow -> Success -> Release`; the bounded-map case
  retained 5000 entries above the documented 4096 cap.
- Before the fix, `TestR8_UsernameCanonicalAtStoreBoundary` stored
  `"  Alice  "` verbatim instead of the canonical `alice`.
- Before the fix, owner PATCHes clearing or replacing an admin-applied
  `allowed_models` list returned HTTP 200. The new admin/owner test now
  requires 403 for both and 200 only for preserve/narrow or an admin command.

Verification:

```text
PS> go test -count=1 ./internal/guard ./internal/store ./internal/server -timeout 180s
ok github.com/cn-maul/rosetta-gateway/internal/guard
ok github.com/cn-maul/rosetta-gateway/internal/store
ok github.com/cn-maul/rosetta-gateway/internal/server

PS> go test -count=1 ./internal/admin -timeout 300s
ok github.com/cn-maul/rosetta-gateway/internal/admin

PS> go test -race -count=1 ./internal/guard ./internal/server -timeout 240s
ok github.com/cn-maul/rosetta-gateway/internal/guard
ok github.com/cn-maul/rosetta-gateway/internal/server

PS> go vet ./internal/admin ./internal/guard ./internal/store ./internal/server
PASS
```

Status: `M10-01`, `M10-02`, and `M12-03` fixed at the R8 scope. Next: R9
HTTP/admin edges.

## R9 HTTP and Admin Edge Contracts Verified

Recorded on 2026-10-26.

Implemented:

- Organization cost/usage endpoints now distinguish absent time parameters
  from explicit malformed, zero, or negative values. Only an absent value uses
  the 30-day fallback; an explicit invalid value returns 400.
- The shared admin JSON decoder maps `http.MaxBytesError` to 413 before any
  handler can label the body as malformed JSON.
- Audit `limit` and group PATCH names are validated explicitly instead of
  silently falling back or persisting a blank name.
- Login and bootstrap setup use the public request path: credential 401s do
  not clear an unrelated session, and a successful bootstrap password change
  remains observable if the following `/me` refresh fails.
- `/bootstrap` read failure is a distinct degraded startup state with retry,
  not an ordinary login form.

Failure-first evidence:

- Explicit `start_time` / `end_time` values such as `abc`, `0`, and `-1`
  previously returned 200 with the default window.
- Admin request-body overflow previously reached handler-level error mapping
  as malformed JSON.
- Audit `?limit=abc` previously returned 200 with the default audit page.
- Group PATCH with `""` or whitespace previously persisted a blank name.
- Frontend contract tests now cover login 401 preservation, bootstrap profile
  refresh separation, and degraded startup state.

Verification:

```text
PS> go test -count=1 ./cmd/gateway -run TestR9_ -timeout 180s
PASS

PS> go test -count=1 ./internal/admin -run TestR9_ -timeout 180s
PASS

PS> go test -count=1 ./internal/admin ./internal/server ./cmd/gateway -timeout 420s
ok github.com/cn-maul/rosetta-gateway/internal/admin
ok github.com/cn-maul/rosetta-gateway/internal/server
ok github.com/cn-maul/rosetta-gateway/cmd/gateway

PS> go test -race -count=1 ./internal/admin ./internal/server -timeout 360s
ok github.com/cn-maul/rosetta-gateway/internal/admin
ok github.com/cn-maul/rosetta-gateway/internal/server

PS> go vet ./internal/admin ./internal/billinghttp ./internal/server ./cmd/gateway
PASS

PS> cd web; npm test
pass 28
fail 0

PS> cd web; npm run typecheck
PASS
```

Status: `M08-03`, `M11-01`, `M12-01`, `M13-01`, `M15-01`, `M15-02`, and
`M15-03` fixed at the R9 scope. Next: R10 frontend async ownership.

## R10 Frontend Async Ownership Verified

Recorded on 2026-10-26.

Implemented:

- Added a pure `RequestGeneration` and selection-key contract in
  `web/src/asyncState.ts`. Async views can reject stale responses and stale
  loading completions without depending on a browser component harness.
- Provider detail now clears and invalidates balance state on provider
  navigation, validates the response's `provider_id`, and `CredentialPanel`
  independently filters cross-provider results.
- Probe timelines capture ids, days, and granularity. Data, errors, and the
  loading flag are applied only for the current generation and still-current
  selection.
- Direct `/settings?tab=price` navigation loads immediately; the panel
  distinguishes not-loaded/loading/error/ready before claiming that no models
  exist.
- Filtered ModelPicker “全选当前” uses a deduplicated union and preserves
  hidden selections.

Failure-first evidence:

- Before the fix, `web/tests/asyncIntegration.test.mjs` failed all four view
  contracts: no provider generation, no timeline generation/selection key,
  no immediate price load, and visible-array replacement in ModelPicker.
- The pure ownership tests cover out-of-order request publication,
  invalidation on entity navigation, selection-key changes, and hidden-set
  preservation.

Verification:

```text
PS> cd web; npm test
pass 36
fail 0

PS> cd web; npm run typecheck
PASS
```

Status: `M16-01`, `M16-02`, `M16-03`, and `M17-02` fixed at the R10 scope.
Next: R11 accessibility component contracts.

## R11 Accessibility Component Contracts Verified

Recorded on 2026-10-26.

Implemented:

- `Upstream.vue`, `Settings.vue`, and `Wallet.vue` now pair every active Tabs
  caller with a matching tabpanel id, reciprocal `aria-labelledby`, and a
  focusable panel container.
- HeartbeatBar exposes a visually hidden but keyboard-reachable list of the
  visible probe intervals. Arrow keys, Home, and End move between buckets;
  each row includes time, success rate, `ok/count`, and latency, while the
  existing canvas and pointer tooltip remain unchanged.

Failure-first evidence:

- Before the fix, `web/tests/accessibilityContract.test.mjs` failed all three
  Tabs callers for missing panel bindings and failed HeartbeatBar for missing
  focus/keyboard/text contracts.

Verification:

```text
PS> cd web; npm test
pass 41
fail 0

PS> cd web; npm run typecheck
PASS
```

Status: `M17-01` and `M17-03` fixed at the R11 scope. Next: R12 delivery and
operator contract closeout.

## R12 Delivery Contracts Verified

Recorded on 2026-10-26.

Implemented:

- CI now runs `scripts/check-repo-hygiene.ps1`, and `.gitattributes` locks
  `.gitignore`, `.dockerignore`, and `.zcodeignore` to LF.
- The release workflow runs all three generated-contract diffs plus
  `go mod tidy -diff` and `go mod verify`. Raw version tags are limited to tag
  refs; manual branch dispatches publish only `edge`.
- The OCI license claim was removed because the repository has no matching
  tracked license text.
- The detached launcher probes `/healthz` and requires HTTP 200. All stop paths
  verify the executable path or wrapper command line through
  `RosettaGatewayLauncher.ProcessOwnership`; an unrelated port owner or stale
  PID is reported and left running.
- The fine-grained Compose example mounts the complete `/data` state directory,
  so the entrypoint can create valid key material on a clean host.
- Slow performance, leak, timing, and billing-stress tests honor
  `testing.Short()`. R12 added guards to the remaining eight `TestPerf*`
  functions and three billing-stress functions.
- `DESIGN.md` points contributors to `CONTRIBUTING.md` for the local `CC`
  prerequisite instead of claiming that race testing is unavailable.
- `internal/delivery/contract_test.go` holds the final delivery contract for
  hygiene, release generation/module checks, image metadata, tag selection,
  launcher ownership and health, Compose persistence, the slow-test boundary,
  the shutdown budget, and the race prerequisite.

Failure-first evidence:

- The new delivery contract initially failed on the weaker CI grep, `/admin/`
  launcher probe, unverified launcher stops, file-level Compose mounts, the
  unguarded slow tests, and the stale `DESIGN.md` race statement. All of those
  contracts now pass.
- A real unrelated listener on the configured port and a stale PID naming an
  unrelated PowerShell process both survived `gateway.ps1 -Stop`; the launcher
  reported each identity and did not terminate either process.

Verification:

```text
PS> go test -count=1 ./internal/delivery
PASS

PS> go test -short -count=1 -timeout 300s ./cmd/... ./internal/...
PASS

PS> $env:CC="C:/mingw64/bin/gcc.exe"
PS> go test -race -short -count=1 -timeout 900s ./cmd/... ./internal/...
PASS

PS> go vet ./cmd/... ./internal/...
PS> go build ./cmd/... ./internal/...
PS> go mod tidy -diff
PS> go mod verify
PS> pwsh -NoProfile -File scripts/check-repo-hygiene.ps1
PASS

PS> cd web; npm test
pass 41
fail 0

PS> cd web; npm run typecheck
PASS
```

Docker was not available on this machine, so the local image build and Compose
startup were not executed. The clean-state behavior is covered by the Compose
contract and the entrypoint's generated-key path; live image verification
remains an explicit delivery residual for the operator.

`gateway.ps1` was also exercised under Windows PowerShell 5.1 with
`-ExecutionPolicy Bypass -File ... -Stop`; it parses and exits successfully.
The working copy remains CRLF and carries a UTF-8 BOM so Windows PowerShell 5.1
reads its Chinese string literals correctly. The index remains LF; the diff is
limited to line-ending representation plus the 91 substantive changed lines
recorded by `git diff --ignore-space-at-eol`.

Status: `M00-01`, `M00-02`, `M00-03`, `M18-01`, `M18-02`, `M18-03`,
`M18-04`, `M19-01`, and `M20-04` fixed at the R12 scope. `M20-01`, `M20-02`,
and `M20-03` retain their R1/R2 fixes and pass the R12 delivery contract.
All 54 confirmed findings now have fixed/closure evidence. Remediation is
complete in the working tree; no commit, push, tag, deployment, or production
database operation was performed.

## Post-Remediation Isolated Smoke Test

Recorded on 2026-10-26 after the R12 completion ledger.

The smoke instance used a temporary `ROSETTA_GW_HOME`, local SQLite state, a
fake upstream on `127.0.0.1:18701`, and the gateway on `127.0.0.1:18666`.
No repository state, production database, deployment, or external credential
was used.

### Additional finding

`SMOKE-01` is a post-remediation regression, not one of the original 54
findings: authenticated management writes were persisted with audit actor
`anonymous`. `AutoReload` wraps `UserAuth`; `UserAuth` injected identity into a
derived inner request, so the outer audit callback could only see the original
context. A failing first test reproduced all three writes as
`anonymous`. The working tree fix adds a per-request actor slot created by
`AutoReload`, written by `UserAuth` only after successful authentication, and
read by the auditor. Failed authentication still does not reload.

Verification:

```text
FAIL before fix:
audit actors = ["anonymous" "anonymous" "anonymous"],
want ["alice" "admin1" "admin1"]

PASS after fix:
go test -count=1 ./internal/server -run
  'TestAuthzAuditActor_UsesAuthenticatedIdentity|TestAutoReload|TestAuditOnly' -v

PASS:
go test -short -count=1 -timeout 300s ./cmd/... ./internal/...
go test -race -short -count=1 -timeout 900s ./cmd/... ./internal/...
cd web; npm test          # 41 passed
cd web; npm run typecheck
```

Real isolated HTTP evidence: a user PATCH changed the key name and the new
audit row recorded `smokeuser`; historical rows remained unchanged.

### Common-operation matrix

- Browser: first-run setup, automatic login, admin overview, encrypted
  credential provider creation, model discovery/import, route and ordered
  target creation, ordinary user creation, user login, one-time key display,
  history, models, wallet, settings, account, transfer tab, and a 390 px mobile
  provider page. Browser console errors, page errors, and failed requests were
  empty; the mobile page had no horizontal overflow.
- API: `GET /v1/models`, non-stream Chat, Chat SSE, Responses, Anthropic
  Messages, invalid-key 401, unknown-model 404, malformed JSON 400, route
  disable/re-enable, history and wallet accounting.
- Key lifecycle: user disable returned 403 to the API; the owner could not
  self-re-enable the administrator-disabled key; an administrator restored it
  and the API returned 200.
- Provider connectivity: `POST /admin/api/providers/{id}/test` returned
  `status:"ok"` with one model.
- Config transfer: encrypted export returned an attachment, wrong passphrase
  returned 400, and dry-run import reported one provider, one model, and one
  credential while provider and model counts remained unchanged.
- Failover: an unreachable first target followed by the fake upstream returned
  `pong`; history recorded the exact ordered trace
  `1:Broken Primary/broken-chat -> fail_switch:upstream_timeout; 2:Smoke OpenAI/fake-chat -> ok`.
  The single-target configuration was then restored and verified.

Status: the post-remediation smoke finding is fixed and verified in the working
tree. The original 54 findings remain closed. Browser and API smoke coverage
found no additional defect. No commit, push, tag, deployment, or production
database operation was performed.

## v1.7.0 Release Verification

Release commit `01b0b9c` was pushed to `main`. The first CI run (`#51`) failed
the full short suite on Linux because a committed streamed downstream-write
failure was normalized as an ordinary client abort, causing a second usage row.
`DELIVERY-01` records the failure-first test, the minimal outcome-kind fix, and
its verification. The tag and GHCR publication are intentionally blocked until
the fix passes a fresh complete CI run.
