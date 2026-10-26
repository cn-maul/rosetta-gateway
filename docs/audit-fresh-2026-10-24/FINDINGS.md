# Findings

Only confirmed findings are listed here. Unproven observations belong in
`Open Questions` in the module section of `PROGRESS.md`.

## M00 - Baseline, Build, CI, Release

### M00-01 - CI hygiene is weaker than the repository hygiene script

- Severity: P2
- Status: fixed in R12; CI runs the repository hygiene script and ignore files are LF-locked
- Evidence: `scripts/check-repo-hygiene.ps1:147-252`,
  `.github/workflows/ci.yml:51-90`, `.gitattributes:1-31`
- Trigger: `.gitignore`, `.dockerignore`, or `.zcodeignore` is rewritten with
  CRLF, or its ignore probes stop matching, while the tracked index remains
  clean.
- Impact: the CI job reports success after checking only tracked files. The
  local script's ignore-rule probes and blank-line wildcard trap check never
  run in CI. On Windows, a CRLF rewrite can make blank lines behave like
  match-any-directory ignore rules, hiding untracked runtime data or secrets
  from ordinary `git status` checks.
- Direction: run the existing read-only hygiene script from CI, or port its
  probe and newline checks to the CI job. Lock the ignore files to LF in
  `.gitattributes`.

### M00-02 - Release workflow omits generated-contract and module-manifest checks

- Severity: P3
- Status: fixed in R12; release workflow runs generated contract and module checks
- Evidence: `.github/workflows/ci.yml:116-152`,
  `.github/workflows/docker.yml:95-110`
- Trigger: a tag points at a commit with stale `defaults.ts`, `routes.ts`,
  `dto-contract.ts`, or an untidy module manifest.
- Impact: the release workflow can publish an image without the same
  generated-contract and dependency checks used by CI. The tag is the
  production artifact, so a green release job does not prove these files match
  the tagged source.
- Direction: run the three generation diffs plus `go mod tidy -diff` and
  `go mod verify` in the release workflow, or invoke one shared check target.

### M00-03 - Image metadata declares MIT without a license file

- Severity: P3
- Status: fixed in R12; the unsupported OCI license claim was removed
- Evidence: `.github/workflows/docker.yml:163`
- Trigger: the image is redistributed based on its OCI metadata.
- Impact: the image advertises `MIT`, but the repository has no tracked
  `LICENSE`, `COPYING`, or `NOTICE` text for a distributor to verify.
- Direction: add the intended license text, or remove the metadata claim.

## M01 - Startup and Lifecycle

### M01-01 - Initial runtime rebuild failure is neither retried nor reported by health

- Severity: P1
- Status: fixed in R1; initial rebuild failure feeds typed readiness and retry
- Evidence: `internal/gatewayapp/run.go:127-137`,
  `internal/runtime/reloader.go:63-120`, `internal/api/healthz.go:24-40`
- Trigger: the first `reloader.Reload` fails, for example because a runtime
  settings row is unreadable or a provider/snapshot query errors, and the
  config bootstrap fallback cannot build a usable runtime.
- Impact: the failed reload does not call `MarkDirty`. `WatchRetry` therefore
  sleeps forever in its clean-state branch, so the database may become
  reachable again without any automatic rebuild. `/healthz` checks only the
  dirty flag and database ping, so it can return `200 {"status":"ok"}` while
  the runtime has no routes or is serving an empty/fallback snapshot.
- Direction: mark the runtime dirty when the initial rebuild fails, or make the
  initial load enter the same retry state as a failed admin reload. Health must
  represent both database reachability and runtime convergence.
- R0-D implementation: every failed `Reloader.Reload` now enters the existing
  dirty/retry state before `gatewayapp.Run` attempts the configuration
  fallback. `TestR0AC10` proves `/healthz` returns
  `runtime_lags_behind_database` after a failed rebuild and returns to healthy
  after a successful retry. R1 now exposes the result through
  `lifecycle.Manager`, and the production `/healthz` path consumes it.

### M01-02 - First-run admin bootstrap failure is swallowed and startup continues

- Severity: P1
- Status: fixed in R1; bootstrap outcomes are typed and startup failures are fatal
- Evidence: `cmd/gateway/bootstrap_admin.go:20-22,60-92`,
  `internal/gatewayapp/run.go:146-154`,
  `internal/admin/user_handler.go:485-528`,
  `internal/store/user_dao.go:185-195,348-390`
- Trigger: on a fresh database, `CountUsers` fails, `CreateUser` fails for a
  non-unique reason, or the initial admin insert otherwise does not happen.
- Impact: `initializeAdminBootstrap` always returns `nil`, so the startup guard
  that is designed to refuse service on bootstrap failure never fires.
  `FindUninitializedAdmin` then returns no account, and the public bootstrap
  endpoint has no admin identity to initialize. The operator can be left with
  a running gateway that has no usable administrator and no startup failure.
- Direction: propagate bootstrap read/write errors. A concurrent unique
  violation can remain a successful no-op; other failures must either stop
  startup or produce an explicit degraded state that prevents normal serving.
- R0-C implementation: `ensureBootstrapAdmin` now returns explicit
  `setup_required` / `ready` / `failed` outcomes and errors for unreadable
  bootstrap state or failed writes. A unique conflict is accepted only after a
  re-read proves the administrator exists. `gatewayapp.Run` also treats a
  post-bootstrap `FindUninitializedAdmin` failure as fatal. R1 projects the
  returned `BootstrapState` into readiness instead of re-inferring health.

### M01-03 - Probe shutdown cannot interrupt stagger sleep and may continue after the wait budget

- Severity: P2
- Status: fixed in R1; stagger is context-aware and post-cancel writes are refused
- Evidence: `internal/probe/runner.go:124-145`,
  `internal/gatewayapp/serve.go:79-89`, `internal/gatewayapp/run.go:115-120`
- Trigger: termination arrives while one or more probe workers are in the
  random pre-probe delay, or while the rest of a probe round is still running.
- Impact: cancellation is observed only at the semaphore acquisition and in
  the probe context. The worker then executes a bare
  `time.Sleep(rand.Int63n(...))` and can continue into `probeOne` after
  `Serve` has stopped waiting for `probeDone`. `Run` returns and its deferred
  `db.Close` can overlap later probe reads/writes; at minimum shutdown reports
  success while a probe goroutine is still active.
- Direction: replace the sleep with a context-aware timer/select and make
  `probeOne` check cancellation before writing the result. Treat probe
  termination as a dependency of store close, not a best-effort wait.
- R1 implementation: the stagger uses `time.NewTimer` plus `ctx.Done()`;
  `probeOne` returns before `RecordProbe` or pruning when canceled. The probe
  worker is registered with the lifecycle manager and must join before
  `Run` may close storage.

### M01-04 - Usage drain timeout does not stop synchronous fallback writes or guarantee workers have exited

- Severity: P2
- Status: fixed in R1; admission closes and every drain requires worker exit
- Evidence: `internal/gatewayapp/serve.go:102-107`,
  `internal/usage/recorder.go:360-362,788-820,860-878`,
  `internal/gatewayapp/run.go:139-144`
- Trigger: a request finishes after `Recorder.Wait` marks the queue closed, or
  the five-second drain deadline expires while a usage write/retry is active.
- Impact: `RecordWithCallback` then performs a synchronous
  `CreateUsageRecordWithCost` with `context.Background()`. `Wait` returns after
  its deadline but does not wait for the worker to exit; `Abort` only closes
  the retry backoff channel and cannot interrupt an in-progress DB call. The
  returned `Serve` can then let `Run` close the store while that write is still
  using it. The code explicitly logs lost records, but the store-close overlap
  and ongoing work are not bounded.
- Direction: make the post-deadline path reject or queue writes behind a
  lifecycle barrier, and join the recorder/worker goroutines (or explicitly
  keep the store open until they exit) before `db.Close`.
- R1 implementation: post-close records are rejected without a synchronous
  DB write; the cleanup callback is invoked exactly once. `Wait` requires the
  worker `done` channel, and a failed usage/runtime join prevents `Run` from
  closing storage.

## M02 - Config, Snapshot, Routing, Catalog

### M02-01 - `api_key_env` overrides `api_key` in bootstrap credentials

- Severity: P2
- Status: fixed in R2; shared precedence resolver used by bootstrap and runtime fallback
- Evidence: `internal/gatewayapp/bootstrap.go:64-71`,
  `internal/upstream/upstream.go:226-234`,
  `internal/config/config.go:572-576`, `DESIGN.md:1438-1441`
- Trigger: a bootstrap credential sets both `api_key` and `api_key_env`, and
  the named environment variable is empty or unset.
- Impact: both the database bootstrap path and the config fallback pool replace
  the non-empty `api_key` with the empty environment value, then skip the
  credential. The documented precedence is the opposite: `api_key` wins when
  both forms are present. A credential that should authenticate instead
  disappears from the pool.
- Direction: resolve `api_key` first and consult `api_key_env` only when
  `api_key` is empty. Apply the same precedence in both bootstrap paths, or
  centralize credential resolution so they cannot drift.

### M02-02 - Corrupt model defaults are silently replaced with zero in the runtime snapshot

- Severity: P2
- Status: fixed in R2; corrupt setting keeps positive fallback and fails rebuild visibly
- Evidence: `internal/snapshot/rebuild.go:210-225`,
  `internal/store/settings_dao.go:120-136`,
  `internal/catalog/models.go:143-151,176-192`, `DESIGN.md:407-409`
- Trigger: the stored `app_settings.model_defaults` JSON is corrupt or the
  setting read otherwise fails, while an upstream model has no capacity values
  of its own.
- Impact: `GetModelDefaults` returns the built-in 128K/64K defaults together
  with the error, but the snapshot handles only the `err == nil` branch. The
  runtime keeps zero values and the rebuild still succeeds. `/v1/models` then
  omits `context_length`, `max_input_tokens`, and `max_output_tokens` instead
  of using the documented setting-page defaults. Other settings callers surface
  the error, so this is a snapshot-layer omission rather than an intentional
  global fallback policy.
- Direction: use the returned defaults even when `GetModelDefaults` reports an
  error, or initialize the snapshot's model defaults from the same constants
  before reading the setting. Decide explicitly whether a corrupt setting
  should keep the last valid snapshot, fail the rebuild, or fall back with a
  warning.

### M02-03 - A partial `probes` object silently disables probing

- Severity: P2
- Status: fixed in R2; partial probes object defaults enabled true by key presence
- Evidence: `internal/config/config.go:145-183,384-395`,
  `internal/gatewayapp/serve.go:296-305`, `DESIGN.md:1477-1482`
- Trigger: the configuration contains a `probes` object with any field but no
  explicit `enabled`, for example
  `{"probes":{"interval_sec":300}}`.
- Impact: `configured` becomes true when any child key exists, so `Enabled`
  remains its zero value. The probe loop then exits without starting.
  The contract says `enabled` defaults to true and only an explicit `false`
  turns probing off. Existing tests cover an explicit false and a missing
  block, but not a partial object.
- Direction: track presence of the `enabled` key separately, or represent it as
  `*bool` during decoding and default only a missing value to true.

### M02-04 - Bootstrap writes are non-atomic and are not repaired on restart

- Severity: P2
- Status: fixed in R7; one transaction, explicit completion marker, legacy graph verification
- Evidence: `internal/gatewayapp/bootstrap.go:18-25,35-123`,
  `internal/gatewayapp/run.go:122`, `internal/store/tx.go:96-116`,
  `DESIGN.md:1307,1309`
- Trigger: bootstrap creates one or more providers, then a network, credential,
  model, or route write fails; or the initial provider-list read fails while
  the database is otherwise writable.
- Impact: each object is committed independently and most errors merely log and
  continue. A failed run can leave a provider without credentials or models, or
  a model without a route; the subsequent reload can publish that partial
  configuration. On the next restart, any existing provider causes the whole
  bootstrap section to be ignored, so the missing pieces are never repaired.
  A failed `ListProviders` read is also treated as an empty database, allowing
  bootstrap writes against an unknown existing state.
- Direction: read providers with error propagation, and create the complete
  bootstrap graph in one transaction using the existing `Store.RunTx`. Treat a
  failed read or failed write as a startup error, or persist an explicit
  bootstrap completion marker rather than inferring completion from any
  provider row.

## M03 - Inbound Protocols

### M03-01 - OpenAI Chat inbound silently discards `input_audio` and `file`

- Severity: P2
- Status: fixed in R4; Chat audio/file parts are normalized or rejected explicitly
- Evidence: `internal/inwire/openai_chat.go:581-590`,
  `internal/inwire/openai_chat.go:605-643`,
  `internal/inwire/openai_chat.go:653-670`
- Trigger: an OpenAI Chat request sends a user content part with
  `{"type":"input_audio",...}` or `{"type":"file",...}`, either alongside text
  or as the only content.
- Impact: `openAIContentPart` models only `text` and `image_url`, and
  `extractUserContent` switches only on those two types. `encoding/json`
  ignores the remaining fields. With accompanying text, the request can return
  a normal 200 while the model never receives the audio or file. Without text,
  the user message becomes empty and rosetta rejects it as an internal
  "message has no content blocks" 400. This is the same silent-content-loss
  failure mode that the Responses path already handles for
  `input_file`/`input_audio`.
- Direction: translate Chat `input_audio` and `file` into
  `rosetta.BlockAudio` and `rosetta.BlockFile`, matching the Responses
  handling, or reject these parts explicitly with a 400. Do not continue
  ignoring them.

### M03-02 - Anthropic `document.source.type=text` is silently discarded

- Severity: P2
- Status: fixed in R4; text-source documents become normalized text
- Evidence: `internal/inwire/anthropic.go:87-96`,
  `internal/inwire/anthropic.go:495-567`,
  `internal/inwire/anthropic.go:662-688`
- Trigger: an Anthropic Messages request contains a legal document block whose
  source is `{"type":"text","media_type":"text/plain","data":"..."}`, either
  alongside text or as the only content.
- Impact: the source type comment explicitly includes `text`, but `fileBlock`
  handles only `base64` and `url` and returns `false` otherwise.
  `convertUserMessage` then skips the block without an error. An attachment
  beside ordinary text disappears while the request succeeds; an
  attachment-only message can become empty and surface as a generic internal
  400. The request's own declaration and its conversion disagree.
- Direction: convert a text source to a file block or a text block according
  to the pinned rosetta contract, or reject it explicitly before conversion.
  Add a regression test for text-source documents and document-only messages.

### M03-03 - TPM and lifetime quota reservations omit the model output ceiling

- Severity: P2
- Status: fixed in R4 and R5; admission and balance preflight share the
  reachable attempt set
- Evidence: `internal/ingress/estimate.go:15-16`,
  `internal/dataplane/handler.go:307-322`,
  `internal/dataplane/handler.go:351-355`,
  `internal/dataplane/handler.go:413-417`,
  `internal/billing/billing.go:69-82`, `DESIGN.md:1842`
- Trigger: a Chat request omits both `max_tokens` and
  `max_completion_tokens`, or a Responses request omits
  `max_output_tokens`, while TPM, key quota, or user quota admission is
  enabled.
- Impact: `EstimateRequestTokens` adds only `req.MaxOutputTokens`, which is
  zero when the client supplied no output limit. TPM and quota reservations
  happen before route resolution, so no target model is available. Meanwhile
  the balance precheck falls back to the routed model's `MaxOutputTokens`.
  Final usage corrects the records, but concurrent requests can all pass
  admission against near-zero output reservations. The documented contract
  says reservations include the full max-token ceiling and deliberately
  reserve high before refunding.
- Direction: after route resolution, reserve output against the maximum
  configured output limit in the candidate chain; alternatively define and
  share a conservative default output rule with the balance precheck.

### M03-04 - Ingress token estimator ignores all media input

- Severity: P2
- Status: fixed in R4; image/audio/file payloads participate in estimation
- Evidence: `internal/ingress/estimate.go:25-50`,
  `internal/dataplane/handler.go:307-322,351-355,413-417,1431-1445`,
  `internal/billing/billing.go:65-108`
- Trigger: a request sends images, audio, files, or large inline document data
  while TPM or lifetime quota admission uses the ingress estimator, or
  upstream usage is missing and the fallback estimate is used.
- Impact: the estimator counts only text, thinking, tool-result text, tool
  arguments, and tool definitions. It does not count `ImageURL`,
  `AudioData`, `FileData`, file names, or MIME types. Media-only requests can
  therefore reserve approximately zero input tokens, and media-heavy
  requests can significantly under-reserve TPM and quota. Final usage may
  correct the record, but it does not prevent simultaneous admission beyond
  the configured limit.
- Direction: mirror the pinned rosetta media estimate (or a documented
  conservative approximation) for image/audio/file blocks, and use the same
  estimator for both admission and usage fallback.

### M03-05 - Responses tool errors omit supported `custom` and `local_shell`

- Severity: P3
- Status: fixed in R4; the error names every accepted tool type
- Evidence: `internal/inwire/openai_responses.go:239-265`,
  `internal/inwire/openai_responses.go:286-290`
- Trigger: a Responses request contains an unsupported tool type, or a tool
  type that is rejected by a sub-branch, and the client reads the error text.
- Impact: the default error says only `function` and `namespace` can be
  proxied, but `custom` and `local_shell` are also accepted. This is a
  diagnostic/contract inconsistency, not a data-loss defect, because the
  accepted paths still work.
- Direction: update the error text to list all accepted types, or derive the
  list from the validation table.

## M04 - Outbound Protocols

### M04-01 - OpenAI Chat non-stream output discards text when tool calls are present

- Severity: P2
- Status: fixed in R4; non-empty text is preserved beside tool calls
- Evidence: `internal/outwire/openai_chat.go:465-492`,
  `internal/outwire/openai_chat.go:525-537`
- Trigger: a non-streaming Chat response contains both assistant text and at
  least one tool call.
- Impact: `WriteNonStreamResponse` first writes `resp.Text()` into
  `message.content`, then unconditionally replaces it with the JSON literal
  `null` whenever tool calls exist. OpenAI's wire format permits
  `content:null`, but it also permits and commonly carries text alongside
  `tool_calls`. The model's visible explanation or partial answer is silently
  removed from the client response even though it was received from upstream.
  The streaming path preserves both text and tool-call deltas, so the same
  request changes meaning based only on `stream`.
- Direction: keep the encoded text when it is non-empty and use `null` only
  when there is no text. Add a non-stream test containing text plus tool calls
  and compare it with the streaming semantics.

### M04-02 - Anthropic and Responses streaming omit protocol start for terminal-only empty replies

- Severity: P2
- Status: fixed in R4; `Finish("ok")` ensures the protocol opening event
- Evidence: `internal/outwire/anthropic.go:370-389`,
  `internal/outwire/anthropic.go:439-516`,
  `internal/outwire/openai_responses.go:236-245`,
  `internal/outwire/openai_responses.go:504-676`
- Trigger: an upstream stream emits `EventMessageEnd` as its first/only event,
  with no `EventMessageStart`, text, thinking, or tool delta, and the request
  finishes with status `ok` (for example a legitimate empty completion or a
  terminal event delivered before any content event).
- Impact: both sinks mark `sawTerminal` in `EventMessageEnd` without calling
  `ensureStarted` or opening a content item. `Finish("ok", ...)` then writes
  Anthropic `message_delta`/`message_stop` or Responses `response.completed`
  without the required opening `message_start`/`response.created` and output
  lifecycle. A strict client can reject the event sequence even though the
  gateway has already sent HTTP 200 and will record the request as successful.
  The existing text/tool tests always include content or a start event, so this
  terminal-only path is untested.
- Direction: in `Finish` for `ok`, ensure the protocol start event exists before
  emitting terminal events; for Responses also emit a valid empty output
  sequence or explicitly document and test the chosen empty-response shape.
  Add terminal-only tests for both sinks.

## M05 - Dataplane and Failover

### M05-01 - Non-stream downstream write failures are recorded as successful and charged

- Severity: P2
- Status: fixed in R5; typed downstream-write outcomes own terminal accounting
- Evidence: `internal/dataplane/handler.go:1577-1617`,
  `internal/ingress/codec.go:27,72-74,106-108,141-143`,
  `internal/outwire/openai_chat.go:530-537`,
  `internal/outwire/openai_responses.go:826-846`,
  `internal/outwire/anthropic.go:187-205`,
  `internal/usage/recorder.go:483`
- Trigger: the client disconnects or the connection fails while a successful
  non-stream response is being encoded or written, after the upstream call has
  returned usage.
- Impact: `Codec.WriteNonStream` has no error result. Chat and Responses ignore
  `json.Encoder.Encode` errors; Anthropic logs the error but still returns
  normally. The handler then unconditionally records `UsageStatusOK`,
  `HTTPStatus: 200`, and `Outcome{Committed: true, Success: true}`, and commits
  the reservation. The usage recorder charges balance only when the status is
  `ok`, so a response the client never received can still consume the user's
  balance and appear as a healthy upstream request. The streaming path already
  records the equivalent failure as `canceled` /
  `downstream_write_failed`.
- Direction: make non-stream writers surface encode/write failures through
  `Codec.WriteNonStream`, record them as `canceled` /
  `downstream_write_failed`, and avoid charging them. Do not count this
  downstream failure against the upstream target.
- R0-E implementation: all three writers return
  `outwire.NonStreamWriteResult`; payloads are fully marshalled before headers
  are committed and body write errors (including short writes) are propagated.
  R5 gives write failure its own `OutcomeDownstreamWriteFailed` kind and derives
  `canceled` / `downstream_write_failed`, zero billability, no target or
  credential penalty, and a terminal route trace from that one projection.
  Pre-commit encode failures use `OutcomeEncodeFailed`, render a
  protocol-native 500, and remain distinct from client cancellation.

## M06 - Upstream Pool, Probing, Networking

### M06-01 - OpenAI balance reports the hard limit as available when usage lookup fails

- Severity: P2
- Status: fixed in R6; partial balance is reported as `unknown` with no amount
- Evidence: `internal/upstream/balance.go:444-486`,
  `internal/upstream/balance_test.go:86-129`,
  `internal/upstream/r6_transport_test.go`
- Trigger: the OpenAI subscription request succeeds, but the subsequent
  `/dashboard/billing/usage` request fails, returns a non-200 status, or
  returns JSON without a usable `total_usage` value.
- Impact: `fetchOpenAIBalance` initializes `used` to zero and ignores both the
  usage request error and the JSON error. It then reports `hard_limit - 0` as
  a successful `ok` balance, with a detail string claiming "本月已用 0.00 USD".
  The provider balance endpoint and UI therefore present the full hard limit
  as an exact available balance even though no usage reading was obtained.
  This can lead to incorrect purchasing or account decisions. The function's
  own comment explicitly says not to return an apparently precise available
  balance with this accounting ambiguity.
- Direction: treat a failed or unparseable usage lookup as an error or an
  explicitly partial/unknown result. Do not substitute zero for an unknown
  used amount. Add a test where only the usage request fails.

### M06-02 - Model discovery sends Bearer auth for an auto-detected Anthropic provider

- Severity: P2
- Status: fixed in R6; discovery resolves `auto` before auth selection
- Evidence: `internal/upstream/upstream.go:945-968`,
  `internal/upstream/upstream.go:1329-1353`,
  `internal/upstream/upstream.go:1424-1434`,
  `internal/upstream/upstream.go:1674-1690`,
  `internal/protocol/protocol.go:6-31`
- Trigger: an administrator configures a provider with `protocol: auto`, the
  upstream is actually Anthropic, and the administrator uses the provider
  model-discovery action.
- Impact: data-plane client construction resolves `auto` to `anthropic` and
  writes that resolved value only into the in-memory `ProviderEntry`.
  `DiscoverUpstreamModels` independently reloads the provider from the store,
  so it still sees `auto`. `fetchModels` checks only for the literal
  `anthropic` value and therefore sends `Authorization: Bearer`. An
  Anthropic-compatible endpoint expects `x-api-key` (plus
  `anthropic-version`), so discovery can return 401 even though ordinary
  inference through the same provider works. The admin detail page exposes
  model discovery for configured providers, so the path is reachable.
- Direction: reuse the resolved protocol from the runtime pool for discovery,
  persist the detected protocol, or perform the same protocol detection in
  the discovery path before choosing authentication headers. Add a test with
  stored `protocol=auto` and an Anthropic-shaped upstream.
- Evidence: `internal/upstream/upstream.go:1326-1365`,
  `internal/upstream/upstream.go:1421-1431`,
  `internal/upstream/r6_transport_test.go`

## M07 - Storage, Schema, DAO, Transactions

### M07-01 - Pending costs are archived as permanent zero-cost usage after retention

- Severity: P2
- Status: fixed in R3; recoverable pending and uncharged usage is retention-protected
- Evidence: `internal/store/usage_archive.go:75-91`,
  `internal/store/usage_archive.go:144-391`,
  `internal/store/usage_dao.go:423-470`,
  `internal/gatewayapp/serve.go:130-294`,
  `internal/store/usage_archive_boundary_test.go:144-200`
- Trigger: a usage record is marked `cost_pending`; recomputation
  obtains a price but remains pending because the user cannot pay, or the
  price lookup keeps failing, and the record remains unresolved past
  `usage_retention_days` (default 30 days).
- Impact: pruning aggregates the row into `usage_daily_rollups`, copies only
  its zero cost and a `cost_pending_count` marker, then deletes the per-record
  token, model, and request data. `RecomputePendingCosts` scans only
  `usage_records`, so the archived record can never be priced or charged.
  Its cost remains permanently absent from reports and
  `usage_totals.cost_total`; the UI retains only an unfixable pending count.
  The balance reconciliation path cannot recover the charge either.
- Direction: exclude pending records from pruning, move them to a durable
  work ledger containing the pricing and charging fields, or complete
  recomputation before archiving. Add a regression test where insufficient
  balance keeps a record pending across the retention boundary.

## M08 - Money, Quota, Balance

### M08-01 - Deferred and recomputed costs can be charged twice

- Severity: P1
- Status: fixed in R0-A containment and R3 accounting state machine
- Evidence: `internal/store/balance_dao.go:312-380`,
  `internal/store/balance_dao.go:688-727`,
  `internal/store/balance_dao.go:782-923`,
  `internal/store/topup_dao.go:236-245`,
  `internal/store/usage_dao.go:506-553`,
  `internal/store/usage_dao.go:588-727`,
  `internal/gatewayapp/serve.go:208-294`
- Trigger: either of these normal recovery paths occurs:
  1. A charge returns `ErrInsufficientBalance`, records the amount in
     `balance_pending`, and the user later tops up.
  2. The initial price lookup fails, so usage is stored with zero cost and
     `usage_state='cost_pending'`; recomputation later succeeds.
- Impact: both recovery paths debit the wallet without creating the canonical
  `balance_charges` marker keyed by the original `(request_id, user_id)`.
  Balance reconciliation joins usage only to that original marker and
  therefore treats the same usage as never charged, deducting the full cost a
  second time. In the first path, top-up settlement removes the pending debt
  but leaves no marker; in the second, recomputation creates only a
  `recompute:<record_id>:<cost_micros>` marker. The first reconciliation runs
  after one minute and the recomputation queue after 45 seconds, so the second
  path is reachable on the next normal maintenance cycle. The wallet and
  charge ledger then disagree with the amount actually collected.
- Direction: make every successful usage-related debit, including pending
  settlement and recomputation, create or update one authoritative completion
  marker for the original usage request. Reconciliation should consult that
  settled-state record rather than infer settlement from the absence of the
  original charge key.
- R0-A implementation: `ensureUsageCompletionTx` writes the existing original
  `(request_id, user_id)` marker in the same transaction as fully settled
  pending rows, recomputation, and their wallet debits. Reconciliation excludes
  requests that still have a `balance_pending` row, so the settlement race
  cannot charge twice. No schema or new ledger was introduced. Acceptance
  fixtures `R0-AC-01` through `R0-AC-04` pass, and the full `internal/store`
  suite passes. R0-A containment is verified. This does not close R3's final
  accounting state machine, schema decision, or durable recovery work.

### M08-02 - Quota release failure strands reservations until restart

- Severity: P2
- Status: fixed in R3; request-owned leases and periodic expired-lease recovery
- Evidence: `internal/dataplane/rate.go:60-85`,
  `internal/store/key_dao.go:580-590`,
  `internal/store/user_dao.go:551-565`,
  `internal/store/reservation_recovery.go:47-90`
- Trigger: a transient SQLite busy, lock, or I/O error occurs while releasing a
  key or user lifetime-quota reservation after a request.
- Impact: `RateCommit.ReleaseQuota` clears the in-memory reservation fields
  before attempting the database writes and only logs a WARN when either write
  fails. There is no retry or durable cleanup marker. The persisted
  `reserved_tokens` value remains consumed, so quota admission can return
  false `429 insufficient_quota` responses until the process restarts and
  startup recovery clears all stale reservations. The failure therefore lasts
  until an unrelated restart, not merely for the duration of the transient
  error.
- Direction: preserve a retryable cleanup state until release succeeds, use
  durable expiring reservations, or add a recovery sweep that can distinguish
  live reservations from abandoned entries without requiring a restart.

### M08-03 - Malformed organization time parameters silently use the default window

- Severity: P2
- Status: fixed in R9; explicit invalid values return 400
- Evidence: `internal/billinghttp/billinghttp.go:177-181`,
  `internal/billinghttp/billinghttp.go:221-225`,
  `internal/billinghttp/billinghttp.go:259-287`,
  `internal/billinghttp/billinghttp.go:290-312`,
  `cmd/gateway/billing_scope_test.go:81-110`
- Trigger: call `/v1/organization/costs` or
  `/v1/organization/usage/completions` with an explicit malformed, zero, or
  negative `start_time` or `end_time`.
- Impact: `orgQueryWindow` silently ignores any unparsable or non-positive
  value and keeps that side of the default 30-day window. Both handlers then
  return 200 and data for a different interval than the caller requested. The
  same package's date-based billing endpoint correctly returns 400 for an
  explicit malformed range, and its comment records the exact hazard of a
  silent fallback masquerading as a successful filter. The organization
  endpoints do not follow that contract.
- Direction: distinguish an absent parameter from a present invalid one and
  return 400 whenever an explicit time value is malformed or non-positive.
  Keep the 30-day fallback only for parameters that are genuinely absent.
- R9 implementation: `orgQueryWindow` validates each supplied value before
  applying it. The regression covers malformed, zero, and negative values for
  both organization endpoints while preserving the absent-parameter fallback.

## M09 - Usage and Audit Persistence

### M09-01 - Abandoned usage batches release only the first ten quota reservations

- Severity: P2
- Status: fixed in R3; every abandoned job executes its callback once
- Evidence: `internal/usage/recorder.go:144-153`,
  `internal/usage/recorder.go:176`, `internal/usage/recorder.go:280-291`,
  `internal/usage/recorder.go:331-339`,
  `internal/usage/recorder.go:377-395`,
  `internal/gatewayapp/serve.go:102-107`
- Trigger: usage persistence remains failed through batch retry and the
  per-record fallback, leaving more than ten jobs in the worker's
  `stillPending` slice, and shutdown reaches `Recorder.Abort()`.
- Impact: `logPendingAbandoned` uses the ten-entry log-sample loop to call
  `job.finish()`. Its `break` therefore skips the quota-reservation release
  callback for jobs 11 through 50 even though those records will never be
  persisted. The persisted key/user `reserved_tokens` remain consumed until
  startup reservation recovery runs, so a normal application shutdown during
  a storage failure can leave stranded quota until restart recovery.
- Direction: execute `finish()` once for every job regardless of log-sample
  limits, and keep sample truncation separate. Add a shutdown test with more
  than ten callback-bearing jobs that asserts exactly one callback per job.

### M09-02 - `audit_log_keep_rows=0` does not disable pruning as documented

- Severity: P3
- Status: fixed in R2; explicit zero disables audit pruning
- Evidence: `internal/config/config.go:116-126`,
  `internal/config/config.go:378-382`,
  `internal/config/config.go:503-506`, `DESIGN.md:1456-1462`
- Trigger: an operator follows the `Config.AuditLogKeepRows` comment and sets
  `audit_log_keep_rows` to `0` to retain all audit rows.
- Impact: `setDefaults` rewrites zero to the default 20,000 before validation,
  while validation and `DESIGN.md` define `-1` as the permanent-retention
  value. The configured zero therefore prunes older audit rows instead of
  disabling pruning. This is an operator-visible contract mismatch in a
  compliance-sensitive retention setting.
- Direction: document `-1` consistently in the field comment, or preserve an
  explicit zero-presence signal if zero is intended to mean permanent
  retention. Add a configuration test for the documented value.

## M10 - Identity and Authorization

### M10-01 - Login account throttling is bypassed by username case variants

- Severity: P1
- Status: fixed in R8; store and login share `CanonicalUsername`, and the R0
  case/whitespace budget tests remain green
- Evidence: `internal/admin/user_handler.go:69-74`,
  `internal/admin/user_handler.go:208-225`,
  `internal/admin/user_handler.go:229-237`,
  `internal/admin/user_handler.go:302-307`,
  `internal/admin/user_handler.go:341-357`,
  `internal/store/user_dao.go:158-163`, `internal/store/schema.go:304-311`,
  `DESIGN.md:1277-1286`
- Trigger: an attacker sends failed password attempts for one account from
  multiple source IPs and changes only the username letter case between
  attempts.
- Impact: the account-dimension throttle uses the trimmed username verbatim
  as its key. The database uniqueness constraint and lookup are both
  `COLLATE NOCASE`, so `alice`, `Alice`, and `ALICE` resolve to the same
  account while creating separate throttle entries. Each casing receives the
  five-attempt account budget. The account dimension was added specifically to
  prevent many-IP password guessing against one account; the bypass restores
  the IP-only exposure, multiplied by the number of case variants. The
  existing cross-IP regression test uses one spelling and cannot detect this.
- Direction: canonicalize the throttle key with the same case semantics as the
  database before every `Allow`, `Fail`, `Success`, and `Release` call.
  Add a cross-IP test that alternates case variants of one existing account.
- R0-B containment used a login-local helper. R8 moved the rule to
  `store.CanonicalUsername`, used by persisted user writes, store lookup, admin
  account creation, and every login `Allow`/`Release`/`Fail`/`Success` call.
  `TestR0AC05`, `TestR0AC06`, and
  `TestR8_UsernameCanonicalAtStoreBoundary` prove shared semantics.

### M10-02 - Successful session authentication leaves idle throttle entries behind

- Severity: P2
- Status: fixed in R8; idle entries are removed on Release and the cap applies
  to success traffic
- Evidence: `internal/guard/throttle.go:95-116`,
  `internal/guard/throttle.go:119-128`,
  `internal/guard/throttle.go:176-191`,
  `internal/guard/throttle.go:193-244`,
  `internal/server/user_auth.go:81-135`,
  `internal/admin/user_admin_handler.go:887-904`
- Trigger: valid sessions authenticate from many source addresses; a
  successful lookup failure path also leaves an entry after `Release`.
- Impact: `Allow` inserts a new entry with `inflight=1`. On success,
  `Success` clears the failure state but does not delete the entry while
  `inflight` is positive. The caller's deferred `Release` then decrements it
  to zero without deleting it. `sweepLocked` is called only from `Fail`, so
  all-success traffic never reclaims these entries. The map can exceed its
  documented 4096-entry cap. A legitimate session making requests through a
  rotating set of addresses can therefore grow process memory without any
  failed authentication, contrary to the throttle's bounded-size design.
  This is not an authorization bypass, but it is a reachable resource-growth
  defect in the authentication boundary.
- Direction: delete an entry in `Release` when `inflight` reaches zero and the
  entry has no active failure/cooldown state. Run the same bounded sweep from
  `Release` or another success-path point if the map is still above the cap.
  Add a test that authenticates successfully from many addresses and asserts
  the map does not retain idle entries.

## M11 - HTTP Middleware and Routing

### M11-01 - Admin request-body overflow is reported as malformed JSON

- Severity: P3
- Status: fixed in R9; shared admin decoder emits 413
- Evidence: `internal/server/server.go:128-135`,
  `internal/admin/helpers.go:127-136`,
  `internal/admin/config_transfer_handler.go:222-228`,
  `internal/dataplane/handler.go:239-252`,
  `internal/ingress/codec.go:21-23`
- Trigger: an authenticated administrator sends a JSON mutation whose body
  exceeds `defaults.max_request_body_bytes`.
- Impact: `RequestSizeLimit` wraps the body with `http.MaxBytesReader`, but
  `decodeJSON` returns the resulting `*http.MaxBytesError` unchanged. The
  located admin callers classify every non-415 decode failure as
  `400 invalid JSON`; the response therefore says the payload is malformed
  and can include `http: request body too large` instead of returning the
  canonical 413. The data-plane decoder already distinguishes the same error
  and returns 413 `request_too_large`, and the ingress codec explicitly leaves
  that mapping to its caller. Admin clients receive misleading remediation
  guidance, especially for configuration import and other large mutations.
  `DESIGN.md` does not separately prescribe an admin 413 response, which is
  why this is recorded as a consistency defect rather than a higher-severity
  availability failure.
- Direction: map `*http.MaxBytesError` in the shared admin decode layer to
  status 413 with a stable error code/message. Keep 400 for JSON syntax and
  type errors, and add an admin request-size regression test.
- R9 implementation: `decodeJSON` maps `*http.MaxBytesError` to
  `413 request body exceeds <limit> bytes` and returns
  `errRequestTooLarge`. `TestR9_AdminDecodeMapsBodyOverflowTo413` exercises a
  real MaxBytesReader.

## M12 - Admin Mutation APIs

### M12-01 - Group PATCH accepts and persists an empty name

- Severity: P3
- Status: fixed in R9; create and update share the required-name check
- Evidence: `internal/admin/group_handler.go:133-138`,
  `internal/admin/group_handler.go:161-188`,
  `internal/admin/group_handler_test.go:123-132`
- Trigger: an administrator sends
  `PATCH /admin/api/groups/{id}` with `{"name":""}` or a whitespace-only
  name.
- Impact: creation trims the supplied name and rejects an empty result, but
  update only enforces the 64-character maximum before assigning the trimmed
  value. The update succeeds and stores an empty group name, leaving an
  unnamed group that is difficult to distinguish in management views. The
  name column remains non-null, so this is a validation inconsistency rather
  than a database-integrity failure. The create test covers empty names, but
  no located test covers the same input through update.
- Direction: apply the same required-name check to create and update, and add
  a PATCH regression case for both `""` and whitespace-only names.
- R9 implementation: update rejects blank names before assigning to the
  loaded group. `TestR9_GroupPatchRejectsBlankName` covers both inputs and
  verifies the stored name is unchanged.

### M12-02 - Configuration dry-run undercounts duplicate models across new providers

- Severity: P3
- Status: fixed in R7; dry-run and import share provider-scoped model cardinality
- Evidence: `internal/admin/config_transfer_handler.go:387-397`,
  `internal/admin/config_transfer_handler.go:536-566`,
  `internal/admin/config_transfer_handler.go:615-625`, `DESIGN.md:1649`
- Trigger: dry-run a configuration import containing two new providers that
  each contain the same `model_id`.
- Impact: every new provider is represented by the same literal dry-run ID,
  `"dry-run-placeholder"`. The model deduplication set is keyed by provider ID
  plus model ID, so the second occurrence is treated as already present even
  though the real import assigns each provider a unique ID and imports both.
  The preview therefore reports one fewer created model, contradicting the
  code's explicit requirement that preview and actual import agree. Actual
  writes are not corrupted by this defect.
- Direction: give each pending provider a distinct synthetic dry-run
  identity, or key the preview deduplication by original provider slug plus
  model ID. Add a dry-run/import parity test with same-named models under two
  new providers.

### M12-03 - Key owner can remove an admin-applied key model allowlist

- Severity: P2
- Status: fixed in R8; owner PATCH is monotonic for model allowlists
- Evidence: `internal/admin/key_handler.go:406-408`,
  `internal/admin/key_handler.go:411-456`,
  `internal/admin/key_handler.go:614-621`, `DESIGN.md:600-604`,
  `DESIGN.md:717-718`, `internal/admin/group_handler_test.go:424-431`
- Trigger: after an administrator restricts a key to specific models, the
  ordinary user who owns that key sends
  `PATCH /admin/api/keys/{id}` with `{"allowed_models":[]}`.
- Impact: `allowed_models` is deliberately omitted from
  `guardNoLoosening`. The update validates the empty array, normalizes it to
  nil, and persists the removal of the explicit key-level restriction. The
  owner's effective models remain bounded by the user's group allowlist when
  one exists, so this does not by itself prove access beyond a group ceiling.
  It does contradict the documented rule that ordinary users may only tighten
  key settings and that admin-applied restrictions on a specific key cannot be
  undone by PATCH. Existing coverage verifies that an owner can clear a
  self-created allowlist, but does not distinguish an admin-applied one.
- Direction: preserve an admin-applied key allowlist against owner PATCHes, or
  narrow the contract to state that key-level allowlists are owner-managed and
  that group allowlists are the enforced admin boundary. Add the chosen
  behavior to tests.
- R8 decision: owner PATCH may preserve or narrow the existing key-level
  allowlist but cannot clear, replace, or widen it. Admin PATCH is the explicit
  widening command. `TestR8_OwnerCannotRemoveAdminModelAllowlist` covers both
  directions.

## M13 - Admin Query APIs

### M13-01 - Audit query silently accepts a malformed limit

- Severity: P3
- Status: fixed in R9; explicit malformed limits return 400
- Evidence: `internal/admin/audit_handler.go:33-47`,
  `internal/admin/stats_handler.go:369-381`,
  `internal/admin/usage_handler.go:44-60`
- Trigger: an administrator sends
  `GET /admin/api/audit?limit=abc`.
- Impact: the parse failure is silently ignored, the handler retains its
  default of 50, and the endpoint returns 200 with an ordinary result page.
  Other admin query parameters reject malformed numeric input with a 400:
  `from`/`to` use `parseOptionalUnixMilli`, and usage pagination has an
  explicit clamping contract. Audit has no corresponding documented
  "invalid values fall back to default" rule. A typo or misconfigured caller
  therefore looks like a successful request while returning a different
  number of rows than requested, making a bounded audit investigation
  silently incomplete. The value is also clamped to 500, but that bounded
  behavior is separately documented by the handler comment.
- Direction: return 400 with a stable message when an explicitly supplied
  `limit` is not an integer, or document and test a deliberate
  malformed-to-default policy for audit alongside the existing clamp rules.
  Add handler coverage for `limit=abc`.
- R9 implementation: `AuditHandler.List` rejects an explicitly supplied
  non-integer with 400 `limit must be an integer`; valid values retain the
  documented 1..500 clamp. `TestR9_AuditLimitRejectsMalformedValue` covers the
  malformed case.

## M14 - Billing and Usage Pipeline

### M14-01 - Uncharged-usage reconciliation can starve behind permanently unaffordable rows

- Severity: P2
- Status: fixed in R3; typed outcomes, durable retry backoff, and charged-only counts
- Evidence: `internal/store/usage_dao.go:506-555`,
  `internal/gatewayapp/serve.go:249-290`
- Trigger: at least 500 successful, positive-cost usage rows are uncharged,
  and the oldest of them belong to users whose balance remains below the
  charge amount.
- Impact: `FindUnchargedUsage` always selects the oldest 500 rows with no
  original `balance_charges` marker. `ChargeUnchargedUsage` treats
  `ErrInsufficientBalance` as success and returns nil, but the rolled-back
  charge leaves no marker, so those rows remain at the front of the next scan.
  The ten-minute reconciliation worker therefore processes the same page
  forever and never reaches newer recoverable rows. It also increments its
  `charged` count for each unaffordable row, making the summary log claim
  successful collection when the rows were only moved to `balance_pending`.
  The pending-cost recomputation queue has the same ordering problem for
  successive rows that remain unaffordable after recomputation.
- Direction: give reconciliation a durable per-record outcome cursor or a
  settlement marker that distinguishes charged, deferred-as-debt, and
  retryable failure; exclude or page past terminally deferred rows. Count only
  actual wallet debits as charged. Add a backlog test with more than one page
  of insufficient-balance records followed by a chargeable row.

### M14-02 - Actual usage pricing bypasses the effective-price fallback used by precheck

- Severity: P2
- Status: fixed in R3 and R5; pricing and reachable-chain preflight share one
  effective policy
- Evidence: `internal/store/pricing.go:64-84`,
  `internal/store/pricing.go:147-160`, `internal/store/usage_dao.go:386-408`,
  `internal/store/pricing.go:199-225`, `cmd/gateway/balance_test.go:456-497`
- Trigger: a model has only `price_input` configured and produces output
  tokens, or has only `price_output` configured and consumes uncached input.
- Impact: precheck resolves raw prices through `Prices.EffectivePrices`, which
  fills a missing output price from the effective input price and a missing
  input price from output/cache prices. Actual usage charging calls
  `PriceUsage` directly with the raw columns; it only falls back for cache-hit
  and cache-creation prices. An input-only model therefore charges output at
  zero, and an output-only model charges uncached input at zero. The cost
  frozen into `usage_records.cost_total` and the balance debit derived from it
  use a different pricing policy from precheck. The existing upper-bound test
  compares the estimator with `PriceUsageWithPrices`, which also receives raw
  prices, so it cannot expose the divergence.
- Direction: make actual pricing use `EffectivePrices` too. Add a regression
  test that persists a usage record for each partial-price configuration and
  compares stored `cost_total` with the precheck contract. Keep an explicitly
  unpriced model free.

## M15 - Frontend Foundation

### M15-01 - Login failure is rewritten as an expired session

- Severity: P3
- Status: fixed in R9; login uses the public request path
- Evidence: `web/src/api.ts:154-158`, `web/src/api.ts:282-287`,
  `web/src/views/Login.vue:93-108`, `internal/admin/user_handler.go:271-284`,
  `web/tests/apiContract.test.mjs:136-150`
- Trigger: a user submits an unknown username or a wrong password from the
  login form.
- Impact: the backend returns `401` with `用户名或密码错误`, but the generic
  request wrapper handles every `401` before parsing the JSON body, clears the
  token, and replaces the error with `登录已失效，请重新登录`. `Login.vue`
  displays that rewritten message. A first-time user is therefore told that an
  existing session expired when authentication failed, which sends them toward
  a session diagnosis instead of checking their credentials. The endpoint is
  public and `publicPost()` already provides the no-session-side-effects path
  needed by login, but `login()` does not use it. The API test locks in the
  generic `401` behavior without covering login itself, so this regression is
  currently green.
- Direction: send login through a public request path that parses the error
  envelope and does not clear or mutate session state, or make the generic
  wrapper special-case the login endpoint. Add a test asserting that an
  invalid-login `401` preserves the backend message and does not clear an
  unrelated local session.
- R9 implementation: `login()` calls `publicPost` and preserves the backend
  credential message without clearing the existing token.
  `apiContract.test.mjs` covers both message and session preservation.

### M15-02 - Password setup reports failure after the password has been changed

- Severity: P3
- Status: fixed in R9; password completion is separate from profile refresh
- Evidence: `web/src/api.ts:165-177`, `web/src/views/Login.vue:117-139`,
  `internal/admin/user_handler.go:463-506`,
  `internal/admin/user_handler.go:562-592`
- Trigger: the one-time bootstrap request succeeds and closes the password
  setup window, then the immediately following `GET /me` fails with a network
  error, timeout, rate limit, or server error.
- Impact: `bootstrapSetup()` saves the new token and then awaits `/me`; any
  failure from `/me` rejects the whole function. `Login.vue` renders the
  generic `设置密码失败`, although `BootstrapSetup` has already persisted the
  password and consumed the one-time window. The user is told the irreversible
  setup failed even though their new password is active. On retry the setup
  form may be gone or the request may return a conflict, leaving the user
  unsure which password to use. `login()` has the same “authentication
  succeeded, profile refresh failed” ambiguity, but bootstrap is the
  irreversible case.
- Direction: separate the completed credential change from the profile refresh.
  Keep the returned login result and saved token, surface a distinct
  “password changed; profile refresh failed” state, and retry or reload the
  profile without asking the user to repeat the setup. Add a test for a
  successful setup response followed by a failed `/me`.
- R9 implementation: `bootstrapSetup` saves the returned token and closes the
  setup UI before refreshing `/me`; a refresh failure reports that the
  password was set. The contract test verifies the token remains current.

### M15-03 - Bootstrap status errors are presented as an ordinary login form

- Severity: P3
- Status: fixed in R9; bootstrap read failure is a distinct degraded state
- Evidence: `web/src/api.ts:122-134`, `web/src/views/Login.vue:37-41`,
  `internal/admin/user_handler.go:412-461`,
  `internal/admin/helpers.go:57-61`
- Trigger: `GET /session` succeeds, but `GET /bootstrap` fails with `500` or
  another non-network error because the store cannot read the bootstrap
  marker or pending administrator.
- Impact: `loadSession()` catches every bootstrap error and sets
  `needsSetup=false`, leaving `backendReady=true`. The login page consequently
  renders the ordinary username/password form. The handler explicitly
  documents that a failed bootstrap read must not be reported as “setup not
  needed”, because that sends the user to a login box that cannot work; a 500
  is emitted for that case. The frontend discards the distinction and creates
  exactly that misleading state.
- Direction: preserve the bootstrap request failure as a startup error state
  distinct from “not needed”, and render an unavailable/degraded message with
  a retry instead of the login form. Add a session-bootstrap test for `/session`
  success plus `/bootstrap` 500.
- R9 implementation: `loadSession` records `bootstrapCheckFailed`; `Login.vue`
  renders a retryable startup-error mode. The contract test covers
  `/session` success plus `/bootstrap` 500.

## M16 - Frontend Pages

### M16-01 - Provider navigation can display one provider's balance under another

- Severity: P2
- Status: fixed in R10
- Evidence: `web/src/views/ProviderDetail.vue:417-437`,
  `web/src/views/ProviderDetail.vue:686-693`,
  `web/src/components/CredentialPanel.vue:150-166`,
  `web/src/types.ts:344-353`,
  `internal/upstream/balance.go:87-154`
- Trigger: query the balance on provider A, then navigate within the same
  component instance to provider B without a full page reload.
- Impact: the `watch(providerId)` branch clears `provider`, credentials, and
  models, but leaves `balance` intact. `CredentialPanel.vue` renders the
  balance by matching only `credential_id`; it never checks the response's
  `provider_id`. Consequently, provider B's page can show the retained
  provider-A balance state and mismatched rows. The API contract includes
  `provider_id`, so the data needed to reject a stale response is already
  present but unused. A similar race exists if a balance request started for
  A returns after navigation to B: `queryBalance()` writes the response
  without comparing the captured provider id to the current route.
- Direction: clear `balance` whenever `providerId` changes, bind the panel or
  filter to `balance.provider_id === provider.id`, and discard responses whose
  captured provider id no longer matches `providerId`. Add a navigation test
  that queries A's balance, switches to B, and asserts that no A balance is
  visible.
- R10 implementation: `RequestGeneration` scopes balance requests; provider
  navigation invalidates the generation and clears the balance. The page
  validates `provider_id`, and `CredentialPanel` independently filters to the
  current provider before rendering any credential result.

### M16-02 - Probe timeline responses can overwrite a newer selection

- Severity: P3
- Status: fixed in R10
- Evidence: `web/src/views/Models.vue:163-181`,
  `web/src/views/Models.vue:197-207`,
  `web/src/views/Models.vue:347-353`,
  `web/src/components/HeartbeatBar.vue:93-110`
- Trigger: change `days`, `granularity`, or the set of drawn models while a
  previous probe-timeline request is still in flight, with the older request
  completing last.
- Impact: `loadTimeline()` has no request sequence or abort guard. The last
  response to arrive replaces `bucketsByUpstream` regardless of which
  selection produced it. `HB_HOURS` and `bucketMinutes` are read from the
  current controls, so an old hourly or seven-day response can be rendered on
  the newly selected daily or thirty-day axis. The charts can therefore show
  temporally mismatched buckets while the page labels them as the new range,
  and `timelineLoading` can also be cleared by the older request while a newer
  one is still running.
- Direction: capture a monotonically increasing request sequence (or abort the
  previous request) and apply results only when both the sequence and the
  requested ids/range/granularity still match. Keep the loading flag scoped
  to the active request. Add a test with two out-of-order responses.
- R10 implementation: timeline requests capture ids, days, and granularity in
  a selection key plus a monotonically increasing generation. Only the current
  generation and current key may publish data, errors, or loading completion.

### M16-03 - Direct price-tab navigation is rendered as an empty configuration

- Severity: P3
- Status: fixed in R10
- Evidence: `web/src/views/Settings.vue:44-47`,
  `web/src/views/Settings.vue:400-487`,
  `web/src/views/Settings.vue:816-823`,
  `web/src/views/Settings.vue:1250-1275`
- Trigger: open `/settings?tab=price` directly, refresh that URL, or navigate
  to it from a bookmark.
- Impact: the computed tab is already `'price'` on mount, so the
  `watch(tab, ...)` lazy-load callback does not run. `onMounted()` loads only
  the general settings, not model prices. With `pricesLoading=false`,
  `providers=[]`, and `totalModelCount=0`, the price panel renders “还没有任何
  模型 —— 请先在「上游」页添加模型” and disables “新增价格”. The page states a
  definitive absence of models before making either request, and users are
  directed to fix a configuration that is not missing.
- Direction: call `loadPrices()` when mounting on the price tab (or make the
  lazy-load watcher immediate and keyed to the current effective tab), and
  distinguish “not loaded” from “zero models.” Add a direct-navigation test
  for `/settings?tab=price`.
- R10 implementation: the price-tab watcher is immediate, the panel has an
  explicit `not-loaded/loading/error/ready` state, and only a confirmed
  successful load with zero models renders the empty catalogue message.

## M17 - Frontend Components

### M17-01 - Three Tabs callers emit `aria-controls` without matching tab panels

- Severity: P3
- Status: fixed in R11
- Evidence: `web/src/components/Tabs.vue:53-61`,
  `web/src/components/Tabs.vue:121-135`,
  `web/src/views/Upstream.vue:90-92`,
  `web/src/views/Settings.vue:841-844`,
  `web/src/views/Settings.vue:891`,
  `web/src/views/Settings.vue:1095`,
  `web/src/views/Settings.vue:1237`,
  `web/src/views/Wallet.vue:639-651`,
  `web/src/views/Wallet.vue:840`,
  `web/src/views/AccessControl.vue:78-83`,
  `web/src/views/UsageStats.vue:57-62`
- Trigger: a user focuses or selects a tab in `/providers`, `/settings`, or
  `/wallet`.
- Impact: every tab button receives
  `aria-controls="<tabs-id>-panel-<key>"`, but those three pages render the
  active content as ordinary components or `<div class="panel">` elements
  without the matching panel id, `role="tabpanel"`, or
  `aria-labelledby`. The accessibility relation therefore points at an
  element that does not exist. Screen-reader users cannot associate the
  selected tab with its content, even though `AccessControl.vue` and
  `UsageStats.vue` already demonstrate the correct binding through the
  exposed `panelIdFor()` and `tabIdFor()` helpers.
- Direction: either let `Tabs` render a standard panel wrapper, or bind the
  three missing callers the same way as `AccessControl.vue` and
  `UsageStats.vue`. Add a component test that asserts every emitted
  `aria-controls` target exists and has a reciprocal `aria-labelledby`.
- R11 implementation: `Upstream.vue`, `Settings.vue`, and `Wallet.vue` hold a
  Tabs ref and bind every active panel to `panelIdFor(tab)`,
  `role="tabpanel"`, and `tabIdFor(tab)`.

### M17-02 - “Select all visible” silently discards selections outside the search

- Severity: P2
- Status: fixed in R10
- Evidence: `web/src/components/ModelPicker.vue:35-51`,
  `web/src/components/ModelPicker.vue:66-81`,
  `web/src/components/ModelPicker.vue:97-103`,
  `web/src/components/ModelPicker.vue:134-136`,
  `web/src/views/Keys.vue:734`,
  `web/src/views/Keys.vue:819`,
  `web/src/views/UsersGroups.vue:950`
- Trigger: a picker has more than 12 options, the existing selection contains
  models A and B, the user searches for C, then clicks “全选当前”.
- Impact: `selectAll()` replaces the complete `modelValue` with
  `[...visible.value]` whenever a query is present. A, B, and every other
  selected model outside the filter are removed without being displayed.
  The same component explicitly promises that filtering is presentation-only
  and that the selected set is unaffected, while the button label naturally
  reads as “select all currently shown,” not “replace the selection with the
  currently shown set.” If the user saves the key or group after this action,
  the submitted whitelist has silently revoked access to the hidden models.
- Direction: when a query is active, emit the deduplicated union of the
  existing `modelValue` and `visible`; if replacement is intentional, rename
  the action to make that destructive scope explicit and require
  confirmation. Add a test that starts with hidden selections, filters, and
  clicks “全选当前”.
- R10 implementation: filtered select-all emits the deduplicated union of the
  prior selection and visible options, so hidden selections survive.

### M17-03 - Heartbeat bucket details are available only through pointer hover

- Severity: P3
- Status: fixed in R11
- Evidence: `web/src/components/HeartbeatBar.vue:193-201`,
  `web/src/components/HeartbeatBar.vue:303-320`,
  `web/src/components/HeartbeatBar.vue:416-443`
- Trigger: a keyboard-only or screen-reader user attempts to inspect a
  specific connectivity bucket.
- Impact: the canvas is exposed only as a single `role="img"` whose label
  summarizes how many buckets are shown and how many contain data. Bucket
  time, success rate, `ok/count`, and latency are rendered only when
  `mousemove` calls `onMove()` and populate the tooltip. The canvas is not
  focusable, has no keyboard navigation, and no focusable element references
  the tooltip through `aria-describedby`. Users who cannot operate a pointer
  cannot find which interval failed or inspect its measurements.
- Direction: provide a keyboard-focusable per-bucket summary or an alternate
  table/list of the visible intervals. At minimum, include the failed or
  worst buckets and their counts in the text alternative. Add an
  accessibility test that reaches bucket details using only the keyboard.
- R11 implementation: HeartbeatBar renders a visually hidden but focusable
  bucket list. Arrow/Home/End navigation moves among the same visible
  intervals as the canvas, and each item's text includes time, success rate,
  `ok/count`, and latency.

## M18 - WebUI Embedding, Deployment, CI

### M18-01 - A manual Docker workflow dispatch can overwrite a published version tag

- Severity: P2
- Status: fixed in R12; raw version tags require a tag ref, manual dispatch publishes `edge`
- Evidence: `.github/workflows/docker.yml:6-10`,
  `.github/workflows/docker.yml:120-121`,
  `.github/workflows/docker.yml:155-158`,
  `.github/workflows/docker.yml:175-176`
- Trigger: after `v1.6.8` has been released, advance `main` without changing
  `web/package.json`, then run the Docker workflow manually on `main`.
- Impact: the tag-to-version guard runs only when the ref is a tag. On a manual
  branch dispatch it is skipped, while the raw `type=raw,value=<version>` tag
  remains enabled for every event and the build always uses `push: true`.
  The workflow therefore rebuilds `1.6.8` from unreleased `main` bytes and
  replaces the registry tag that users pin for the released version. The
  `latest` tag is not overwritten in this path, so the damage is easy to miss:
  `latest` and `1.6.8` can point to different code despite carrying the same
  advertised version. A later rollback or digest audit can no longer identify
  the original release from its version tag.
- Direction: push the raw version tag only for tag refs. Restrict manual
  dispatches to `edge`/an explicitly entered non-release tag, or require the
  selected ref to match an existing release tag before allowing the version
  tag. Keep release tags immutable and document the intended manual-rerun
  contract.

### M18-02 - The detached launcher reports `/admin/` availability as a health check

- Severity: P3
- Status: fixed in R12; the detached launcher requires HTTP 200 from `/healthz`
- Evidence: `gateway.ps1:244-251`, `Dockerfile:80-91`,
  `DOCKER.md:391-406`
- Trigger: start `gateway.ps1 -Detached` while the database is unhealthy or the
  runtime snapshot cannot be rebuilt, but the embedded admin page is still
  served.
- Impact: the launcher probes `/admin/` and labels any successful response
  `[OK] 健康检查通过`. `/admin/` serves only the embedded static UI and does
  not check either database pool or runtime freshness. A process that cannot
  forward requests can therefore be reported as healthy. The Docker
  documentation explicitly identifies `/admin/` as the obsolete false-positive
  probe and `/healthz` as the correct endpoint.
- Direction: probe `/healthz` and base the printed result on its status code.
  Treat a 503 as a failed readiness result rather than a static-page success.

### M18-03 - The Windows launcher force-terminates whichever process owns the port or stale PID

- Severity: P2
- Status: fixed in R12; stop paths verify executable path or wrapper command line
- Evidence: `gateway.ps1:70-78`, `gateway.ps1:94-105`,
  `gateway.ps1:201-205`
- Trigger: launch or stop the gateway while an unrelated application is
  listening on the configured port, or leave `bin\gateway.pid` behind until
  Windows reuses its PID for another process.
- Impact: startup calls `Stop-Process -Force` on the listener without checking
  that its image path or identity is this project's gateway. `-Stop` repeats
  the same port-based kill and also terminates any process whose PID is stored
  in the stale pid file. A mistaken port in `config.json`, another local
  service already using 8666, or PID reuse can therefore terminate unrelated
  work silently.
- Direction: only force-stop after verifying the executable path and, for the
  wrapper PID, the expected command line. Otherwise fail with the occupying
  process details and let the operator choose a port or stop it explicitly.
  Delete stale pid files after validating their process identity.

### M18-04 - The fine-grained Compose example does not create the key files it mounts

- Severity: P3
- Status: fixed in R12; Compose mounts the whole `/data` state directory
- Evidence: `DOCKER.md:158-165`, `DOCKER.md:169-173`,
  `DOCKER.md:178-202`, `DOCKER.md:419-421`,
  `docker/docker-entrypoint.sh:29-40`
- Trigger: follow the fine-grained Compose example on a clean host where
  `rosetta/master.key` and `rosetta/session_secret` do not yet exist.
- Impact: the example bind-mounts both files, and the preceding warning says a
  missing single-file bind mount is created as a directory. The only provided
  initialization command creates `config.json` and the database directory; it
  never creates valid key files. Following the documented sequence therefore
  makes the gateway fail on a directory where a secret file is expected, or
  start with an invalid empty/non-existent secret. This contradicts the
  documented promise that the keys are generated automatically when absent.
- Direction: remove the two key-file mounts from the default fine-grained
  example, or add a preparation step that generates valid keys before Compose
  starts the service. Do not recommend a plain `touch`: an empty secret is
  explicitly rejected by the runtime.

## M19 - Test Quality

### M19-01 - The `-short` gate still runs the load and leak tests it claims to exclude

- Severity: P2
- Status: fixed in R12; every load, timing, leak, and billing-stress test honors `-short`
- Evidence: `.github/workflows/ci.yml:160-164`,
  `.github/workflows/ci.yml:190-193`, `.github/workflows/ci.yml:197-207`,
  `.github/workflows/ci.yml:226-229`,
  `.github/workflows/docker.yml:105-110`,
  `cmd/gateway/perf_test.go:456-457`, `:541-542`, `:669-670`, `:730`,
  `:778`, `:819`, `:869`, `:937`, `:1010`, `:1104`, `:1196-1197`,
  `:1298`, `:1356-1357`,
  `internal/store/stress_billing_test.go:348-353`, `:387-392`,
  `:563-571`, `:832-840`
- Trigger: run the documented default gate,
  `go test -short -count=1 -timeout 300s ./...`, on a normal pull request or
  push. A race or release gate triggered by the same workflows follows the
  same test-selection path.
- Impact: CI comments and the `nightly-long` job say `-short` skips
  performance, leak, and stress tests, and that those tests belong only to the
  long-running job. In reality, `testing.Short()` is present in only 5 of the
  13 `TestPerf*` functions in `cmd/gateway/perf_test.go`, and only once in
  `internal/store/stress_billing_test.go`. The default job therefore still
  executes 64- and 100-way concurrency workloads, roughly 1,500-4,000 request
  scenarios, 100 disconnect rounds, fixed-TTFT/sleep assertions, 2,000-event
  stream measurements, and multi-10,000-transaction billing stress. The race
  and release gates repeat that workload, so a shared-runner slowdown can
  delay, time out, or fail the default gate for reasons unrelated to the
  change. This contradicts the documented fast/slow split and makes a red
  result less attributable to a real regression.
- Direction: add `testing.Short()` guards to the tests that are genuinely
  load-, timing-, or resource-dependent, while moving correctness assertions
  that need lightweight coverage into ordinary tests. Make the default, race,
  release, and nightly jobs use the same explicitly documented test boundary.
  Do not claim the slow suite is isolated until every slow test honors that
  boundary.

## M20 - Documentation and Final Deduplication

### M20-01 - The documented shutdown budget omits one two-second wait

- Severity: P3
- Status: fixed in R1; R12 retains the final delivery check
- Evidence: `DOCKER.md:186-190`, `internal/gatewayapp/serve.go:82-89`
- Trigger: an operator sizes the container grace period from the documented
  nominal shutdown budget, or uses that budget to reason about how much time
  remains for the database close.
- Impact: `DOCKER.md` states a total budget of 24 seconds and lists prune 5s,
  recompute 2s, probe 2s, HTTP 10s, and usage 5s. `Serve` also waits up to two
  seconds for charge reconciliation before the probe wait. The nominal
  sequential total is therefore 26 seconds, not 24. The documented 30-second
  Compose grace period still covers that nominal sum, so this is not a claim
  that the example immediately kills the process. It is an incorrect
  operational budget that can mislead future grace-period and timeout changes.
  The separately confirmed probe and usage overruns are counted under M01 and
  are not duplicated here.
- Direction: include the reconciliation wait in the documented budget, or
  derive the budget and the Compose grace period from one named set of
  lifecycle limits. State whether the budget is a nominal sum or an upper
  bound.
- R1 implementation: `lifecycle.DefaultShutdownBudget()` is the single source
  (26s nominal, 30s upper bound), `DOCKER.md` documents both plus the 31s
  Compose grace, and `budget_doc_test.go` compares the document to the
  constants.

### M20-02 - Several documents describe usage retention as fixed at 30 days

- Severity: P3
- Status: fixed in R2; documentation now describes configurable retention
- Evidence: `README.md:26`, `DOCKER.md:308`, `DESIGN.md:345`,
  `DESIGN.md:1900`, `internal/config/config.go:95-114,375-376,495-501`,
  `internal/gatewayapp/serve.go:158`
- Trigger: an operator sets `usage_retention_days` to a value other than 30
  and relies on the README, deployment guide, or design summary for the
  retention and archive boundary.
- Impact: the prose says request details and request-id visibility are fixed
  at 30 days, while the configuration accepts 1 through 3650 days, defaults
  to 30, and passes the configured value to `PruneOldUsage`. An operator can
  therefore size storage, plan compliance retention, or diagnose missing
  details against the wrong cutoff. `DESIGN.md:1445-1454` correctly documents
  the setting, so this is stale repeated prose rather than a disagreement
  about the intended feature.
- Direction: describe 30 days as the default, not a fixed retention period,
  in every retention and archive statement. Keep the fixed 30-day throughput
  window separate from the configurable detail-retention cutoff.

### M20-03 - DOCKER claims `TZ` overrides the configured timezone, but config wins

- Severity: P3
- Status: fixed in R2; documentation now states config timezone wins over TZ
- Evidence: `DOCKER.md:281-291`, `internal/config/timezone.go:60-93`,
  `internal/gatewayapp/run.go:75-81`, `docker/docker-entrypoint.sh:34-35`
- Trigger: a container is started with a non-empty `config.json.timezone`
  that differs from the `TZ` environment variable.
- Impact: the deployment guide presents `TZ` as the highest-precedence option
  and the config field as a lower-precedence alternative. `ApplyTimezone`
  loads a non-empty configured timezone and then overwrites both `TZ` and
  `time.Local`, and startup always calls it. The configured value is therefore
  final. The entrypoint already states the correct precedence, so the guide
  and the implementation's own container script teach opposite rules. Logs
  and daily usage buckets can follow a different timezone than the operator
  selected through `-e TZ`.
- Direction: document config `timezone` as the final override when non-empty,
  with `TZ` used only when config is empty. If container `TZ` is intended to
  win, change `ApplyTimezone` and all three deployment examples together.

### M20-04 - DESIGN says local race testing is unavailable despite the documented CC setup

- Severity: P3
- Status: fixed in R12; DESIGN points to the CONTRIBUTING `CC` prerequisite
- Evidence: `DESIGN.md:1901`, `CONTRIBUTING.md:14-37`,
  `.github/workflows/ci.yml:177-188`
- Trigger: a Windows contributor follows the design document's risk summary
  and concludes that `go test -race` cannot be run locally.
- Impact: `DESIGN.md` says the Windows development machine has no gcc and that
  race testing depends on CI. `CONTRIBUTING.md` documents an existing
  space-free MinGW path and the required `CC` variable, including a second
  invocation form that can handle the space-containing installation. CI
  confirms the expected local Windows setup and runs the race gate. The stale
  claim can cause contributors to skip local race coverage for concurrency
  changes.
- Direction: replace the design-document claim with a pointer to
  `CONTRIBUTING.md`, including the explicit `CC` requirement and the note
  that the path is a machine-local prerequisite rather than a repository
  guarantee.

## Post-Remediation Smoke Finding

This finding was discovered after the original 54-finding remediation ledger
was complete. It is listed separately and does not change that historical
count.

### SMOKE-01 - Authenticated management writes are audited as anonymous

- Severity: P2
- Status: fixed in the working tree and verified against an isolated binary
- Evidence: `internal/server/autoreload.go`, `internal/server/user_auth.go`,
  `internal/server/authz_matrix_test.go`,
  `TestAuthzAuditActor_UsesAuthenticatedIdentity`
- Trigger: a logged-in user or administrator mutates a management resource,
  including creating a key, group, user, route, provider, or import/export
  operation.
- Failure-first result:

```text
audit actors = ["anonymous" "anonymous" "anonymous"],
want ["alice" "admin1" "admin1"]
```

- Impact: the authenticated request succeeded, but the audit row lost the
  actor. IP and method remained available, yet accountability degraded to
  `anonymous`; ordinary authenticated writes could not be attributed to a
  specific account.
- Root cause: `AutoReload` wraps `UserAuth` and records the audit after the
  inner handler returns. `UserAuth` authenticated against a derived request
  context, so the outer audit callback saw only the original request context.
- Direction: carry the authenticated actor through the request boundary
  without changing reload semantics. The fix uses a per-request actor slot
  created by `AutoReload` and populated by `UserAuth` only after successful
  authentication. Audit reads that slot first and retains the existing
  context fallback. Failed authentication returns before the slot is written,
  preserving the rule that failed auth does not reload.
- Verification: the targeted test, full short suite, race short suite, and
  frontend tests/typecheck pass. Real isolated HTTP evidence records
  `smokeuser` for a user key PATCH while old `anonymous` rows remain unchanged.

### DELIVERY-01 - Streamed downstream write failures were folded into client aborts

- Status: fixed after the first v1.7.0 CI run exposed the failure on Linux
- Evidence: `internal/dataplane/outcome.go`,
  `internal/dataplane/outcome_lane_test.go`,
  `TestR5_DownstreamWriteFailureKeepsItsCommittedKind`,
  `TestE2E_F1c_DownstreamWriteFailureMarksCanceled`
- Trigger: a streamed response committed its headers and first chunk, then the
  downstream connection rejected a later write.
- Failure-first result from CI:

```text
--- FAIL: TestE2E_F1c_DownstreamWriteFailureMarksCanceled
    e2e_datapath_test.go:1293: F1c 写失败后 usage 行数 = 2，期望恰好 1 行（重复记账）
```

- Root cause: `attemptStream` persisted one terminal row and returned
  `OutcomeClientAborted` with code `downstream_write_failed`.
  `normalizedKind` did not preserve the more specific downstream-write kind,
  so the outer orchestration treated the committed result as an ordinary
  client abort and appended a second terminal row.
- Direction: preserve `OutcomeDownstreamWriteFailed` when a committed client
  abort carries `downstream_write_failed`; ordinary client cancellation keeps
  its neutral completion behavior.
- Verification: the targeted regression, the Linux-failing E2E repeated 20
  times, and dataplane/usage/outwire repeated 10 times pass. Full gates and a
  fresh CI run are required before tagging.

- Follow-up: the first write can fail after a committed SSE 200 response but
  before the event loop sees a second write. `attemptStream` now checks the
  first sink write immediately and `LaneDecision` treats the explicit
  `downstream_write_failed` code as committed, terminal, and ineligible for
  failover. `TestE2E_F1d_FirstDownstreamWriteFailureIsSingleTerminal` pins the
  single-row result. The F1b/F1c/F1d group then passed 50 mixed runs (20 with
  `-race`), and dataplane/usage/outwire passed 20 runs.

### DELIVERY-02 - A1/A2 read the balance before asynchronous charging

- Status: fixed after the second v1.7.0 CI run exposed a race-only failure
- Evidence: `cmd/gateway/e2e_datapath_test.go`,
  `TestE2E_A1_NonStreamHappyPathChargesAndRecords`,
  `TestE2E_A2_StreamHappyPathRecordsAndCharges`
- Trigger: the full non-short suite ran A2 under `-race` on Linux.
- Failure-first result from CI:

```text
--- FAIL: TestE2E_A2_StreamHappyPathRecordsAndCharges
    e2e_datapath_test.go:442: A2 成功流式请求没被扣费
```

- Root cause: the usage row and its charge are both written by the asynchronous
  recorder worker, in that order. A1/A2 waited with a zero-cent threshold, so
  they could observe the visible usage row and read the unchanged balance before
  the worker reached the charge step. The slower race build made that existing
  test window fail intermittently.
- Direction: wait for the observed balance movement to reach the persisted
  `cost_total`, including remainder-only sub-cent charges. Product charging and
  recorder ordering are unchanged.
- Verification: the A1/A2 race pair passed 200 runs; the complete non-short Go
  race suite, short suite, and frontend tests/typecheck passed locally. A fresh
  complete CI run remains required before tagging.

### DELIVERY-03 - F1b canceled between WriteHeader(200) and the first body write

- Status: fixed after a fresh v1.7.0 CI run exposed the low-frequency failure
- Evidence: `cmd/gateway/e2e_datapath_test.go`,
  `TestE2E_F1b_ClientDisconnectStillConsumesTokenQuota`
- Trigger: the full non-race Linux suite canceled F1b after the first
  `Write` returned, without waiting for a recorded `WriteHeader(200)`.
- Failure-first result from CI:

```text
--- FAIL: TestE2E_F1b_ClientDisconnectStillConsumesTokenQuota
    e2e_datapath_test.go:1246: F1b usage 行:
      {Status:canceled UsageState:none TotalTokens:0 HTTPStatus:502}
    e2e_datapath_test.go:1250: 客户端中断后 total_tokens=0
```

- Root cause: `signalingRecorder` closed its signal inside `Write`, while
  `WriteHeader` is a separate call. The cancel could land before the recorder
  showed the committed 200, so the test exercised the uncommitted cancel path
  (`usage_state=none`, HTTP 502) instead of the committed partial-stream path.
- Direction: signal only after an HTTP 200 has been recorded and a body byte
  has been written. Product stream and quota logic are unchanged.
- Verification: F1b/F1c/F1d passed 300 race runs after the synchronization
  change. The complete non-short Go race suite, short suite, and frontend
  tests/typecheck remain required before tagging.

- v1.7.1 CI follow-up: the recorder synchronization was not the only defect.
  The next full CI run still observed F1b's zero-token 502 row while
  `used_tokens` increased by six. Repeating F1b 200 times locally reproduced
  two rows for every request: an already-persisted committed
  `canceled/200/reported/6` stream row and a later uncommitted
  `canceled/502/none/0` summary with the same request ID.
- Root cause: `attemptStream` set the diagnostic attempt's `Committed` bit but
  constructed its return value without it. `OutcomeClientAborted` therefore
  normalized as uncommitted, and the outer lane executed the final-error path
  after the stream had already recorded its terminal usage.
- Direction: construct the post-WriteHeader outcome with `Committed: true`.
  The test now requires exactly one usage row after a short drain window; its
  recorder also follows `net/http` semantics by treating `Header()` as
  non-committing and an implicit first `Write` as HTTP 200.
- Verification: F1b passed 200 consecutive runs; F1b/F1d passed 100 non-race
  and 100 race runs. The complete short non-race suite and `go vet` passed for
  all non-scratch packages; frontend tests, typecheck, build, and embedded-asset
  sync passed.

### DELIVERY-04 - v1.6.8 database upgrade creates the retry index before its column

- Status: fixed after a real v1.7.0 container startup failed; shipped in v1.7.1
- Evidence: `internal/store/schema.go`,
  `TestSchema_AddsR3AccountingObjectsToExistingV168Database`
- Trigger: starting v1.7.0 against a v1.6.8 `/data` directory.
- Failure-first result:

```text
failed to open database: initialize database:
ensure usage charge retry index: SQL logic error:
no such column: charge_next_at (1)
```

- Root cause: both releases stamp `PRAGMA user_version = 2`, so the schema gate
  correctly treats v1.6.8 as an in-place-compatible database. However,
  `ensureCompatibleSchema` created `idx_usage_charge_retry` before the additive
  column pass added `usage_records.charge_next_at`. The earlier upgrade test
  removed the index and lease table but not the R3 columns, so it did not model
  a real v1.6.8 database.
- Direction: create the retry index only after the additive columns exist. The
  test now drops `charge_attempts`, `charge_next_at`, and `charge_last_error`
  as well as the v1.7.0-only table and index before reopening the store.
- Verification: the failure-first test reproduced the container error, then
  passed after the ordering change. `go test ./internal/store -count=1`,
  `go test ./internal/gatewayapp -count=1`, and `go build ./cmd/gateway` pass.
  The v1.7.1 release rebuild repeats the store, gatewayapp, cmd/gateway,
  webui, frontend test, and typecheck gates.
