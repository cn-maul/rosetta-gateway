# Evidence Index

## M00

### M00-01

- `scripts/check-repo-hygiene.ps1:147-252` contains index checks, ignore
  probes, and the blank-line/CRLF trap check.
- `.github/workflows/ci.yml:51-90` performs only `git ls-files` matching.
- `.gitattributes:1-31` locks many source and build paths to LF but does not
  cover `.gitignore`, `.dockerignore`, or `.zcodeignore`.
- `.gitignore:11-31` states that CRLF turns blank lines into match-any-directory
  rules.

### M00-02

- `.github/workflows/ci.yml:116-152` regenerates defaults, routes, and DTO
  contracts, then checks `go mod tidy -diff` and `go mod verify`.
- `.github/workflows/docker.yml:95-110` runs gofmt, build, vet, tests, and
  race tests, but none of those generated-contract or module-manifest checks.

### M00-03

- `.github/workflows/docker.yml:163` declares
  `org.opencontainers.image.licenses=MIT`.
- The tracked repository listing contains no `LICENSE`, `COPYING`, or
  `NOTICE` file.

## M01

### M01-01

- `internal/gatewayapp/run.go:127-137` logs a failed initial
  `reloader.Reload`, optionally calls `pool.BuildFromConfig`, initializes a
  config snapshot, and then starts `WatchRetry` without `MarkDirty`.
- `internal/runtime/reloader.go:63-67` shows `MarkDirty` is the only path that
  records a failed rebuild; `:83-89` shows `WatchRetry` only reloads when
  `dirty` is true.
- `internal/api/healthz.go:24-40` returns success when DB ping succeeds and
  `IsDirty()` is false; it does not inspect route availability or the result
  of the initial fallback.
- `internal/snapshot/snapshot.go:238-260` stores the fallback snapshot as a
  normal current snapshot, so later readers cannot distinguish it from a
  successful database rebuild.

### M01-02

- `cmd/gateway/bootstrap_admin.go:20-22` calls `ensureBootstrapAdmin` and then
  unconditionally returns `nil`.
- `cmd/gateway/bootstrap_admin.go:60-92` only logs `CountUsers`/`CreateUser`
  failures; it has no return value.
- `internal/gatewayapp/run.go:146-154` rejects startup only when the injected
  `Bootstrap` function returns an error.
- `internal/admin/user_handler.go:485-528` makes `BootstrapSetup` depend on
  `FindUninitializedAdmin`; `internal/store/user_dao.go:185-195` returns no
  row when the initial admin was not created.

### M01-03

- `internal/probe/runner.go:124-143` acquires the semaphore with a
  cancellation-aware select, then executes bare `time.Sleep` before calling
  `probeOne`.
- `internal/probe/runner.go:181-183` records a probe result with the runner
  context, but a canceled context does not prevent the preceding sleep from
  completing.
- `internal/gatewayapp/serve.go:79-89` cancels probes, waits only two seconds,
  and continues with HTTP shutdown/usage drain; `internal/gatewayapp/run.go:120`
  defers `db.Close`.

### M01-04

- `internal/gatewayapp/serve.go:102-107` calls `Usage.Wait` with a five-second
  deadline and then `Usage.Abort`.
- `internal/usage/recorder.go:860-878` closes the queue, waits for workers only
  until the context expires, and returns without a second join.
- `internal/usage/recorder.go:788-820` sends to the queue only while `closed`
  is false; otherwise it writes synchronously with `context.Background()`.
- `internal/usage/recorder.go:360-362` closes only the retry backoff channel;
  it does not cancel an in-progress SQL call.

## M02

### M02-01

- `internal/config/config.go:572-576` validates only that at least one of
  `api_key_env` and `api_key` is non-empty.
- `internal/gatewayapp/bootstrap.go:64-71` starts with `bc.APIKey`, then
  unconditionally replaces it with `os.Getenv(bc.APIKeyEnv)` whenever
  `api_key_env` is non-empty.
- `internal/upstream/upstream.go:226-234` has the same replacement rule in the
  config-only fallback pool builder and skips the empty result.
- `DESIGN.md:1438-1441` explicitly states that `api_key` wins when both forms
  are configured.

### M02-02

- `internal/store/settings_dao.go:120-136` initializes `ModelDefaults` with the
  built-in fallback and returns that value together with an error when the
  setting read or JSON decode fails.
- `internal/snapshot/rebuild.go:222-225` copies model defaults only inside the
  `err == nil` branch and does not return the error, so both snapshot fields
  remain zero.
- `internal/catalog/models.go:176-192` falls back from an upstream model's zero
  capacity to the snapshot runtime defaults.
- `internal/catalog/models.go:143-151` adds the three capacity fields only when
  the resulting values are positive.
- `DESIGN.md:407-409` defines the fallback chain as upstream model values, then
  the settings-page defaults, then omitted fields only when both are zero.

### M02-03

- `internal/config/config.go:170-182` records `configured = len(keys) > 0` for
  any non-empty `probes` object.
- `internal/config/config.go:393-395` sets `Probes.Enabled = true` only when
  the whole object was not configured.
- Therefore a partial object leaves `Enabled=false`; the enabled default is not
  applied to fields omitted from an otherwise present object.
- `internal/gatewayapp/serve.go:296-305` closes the probe completion channel
  and returns without starting a runner whenever `Probes.Enabled` is false.
- `DESIGN.md:1477-1482` states that probing is enabled by default and
  `enabled` defaults to true.

### M02-04

- `internal/gatewayapp/bootstrap.go:20-25` discards the `ListProviders` error
  and returns early when any provider exists, regardless of whether the
  bootstrap graph is complete.
- `internal/gatewayapp/bootstrap.go:35-123` commits networks, providers,
  credentials, models, and routes through separate DAO calls; most write
  failures log and continue, and route creation is a separate loop after
  models.
- `internal/gatewayapp/run.go:122` calls `bootstrapDB` without an error result,
  so startup cannot distinguish a complete bootstrap from a partial one.
- `internal/store/tx.go:96-116` already provides `RunTx` for all-or-nothing
  batches, and its comment describes the same partial-write failure mode.
- `DESIGN.md:1307,1309` limits bootstrap to the database-empty case and says a
  non-empty database causes the configured provider/route section to be
  ignored with a warning.

## M03

### M03-01

- `internal/inwire/openai_chat.go:581-590` defines `openAIContentPart` with
  only `type`, `text`, and `image_url`; its own comment says `input_audio` and
  `file` are ignored by `encoding/json`.
- `internal/inwire/openai_chat.go:605-643` unmarshals content parts and handles
  only `case "text"` and `case "image_url"`; every other part type falls
  through without an error.
- `internal/inwire/openai_chat.go:653-670` builds a user message from the
  returned text and image URLs. If no text or images survive, it returns
  `rosetta.User("")`, which later fails rosetta validation as an internal
  400.
- The Responses-side comparison is visible at
  `internal/inwire/openai_responses.go:907-925,1022-1097`: that path already
  creates `rosetta.BlockFile` and `rosetta.BlockAudio`, demonstrating the
  intended cross-protocol behavior.

### M03-02

- `internal/inwire/anthropic.go:87-96` declares the source type comment as
  `base64 | url | text`, keeping `MediaType` and `Data` as the payload fields.
- `internal/inwire/anthropic.go:662-688` handles only `base64` and `url` in
  `fileBlock`; other source types return `rosetta.Block{}, false`.
- `internal/inwire/anthropic.go:540-544` converts a document only when
  `fileBlock` returns true; otherwise the block is skipped.
- `internal/inwire/anthropic.go:553-558` emits an empty user message if every
  content block was skipped, leaving the generic rosetta validation error.
- `internal/inwire/anthropic.go:198-232` validates content shape and assistant
  block types, but does not validate user document source types, so a
  text-source document reaches the silent skip.

### M03-03

- `internal/ingress/estimate.go:15-16` defines the TPM reservation as
  `EstimateInputTokensOnly(req) + req.MaxOutputTokens`.
- `internal/dataplane/handler.go:307-322,351-355,413-417` performs TPM,
  key-quota, and user-quota admission with the request before candidate
  resolution; the estimates in all three paths use `EstimateRequestTokens`.
- `internal/billing/billing.go:69-82` uses the client's output limit when
  present and otherwise falls back to `m.MaxOutputTokens`, so the balance
  precheck has a different output-ceiling policy from ingress admission.
- `DESIGN.md:1842` states that reservations contain the full `max_tokens`
  amount and that the gateway prefers to reserve high and refund later.

### M03-04

- `internal/ingress/estimate.go:25-50` adds only `blk.Text`, `blk.Thinking`,
  `blk.Content`, `blk.Arguments`, and tool name/description/parameter bytes.
  It does not add any media payload or media metadata.
- `internal/dataplane/handler.go:1431-1445` calls
  `EstimateInputTokensOnly` for the missing-usage fallback, so the same
  omission affects accounting when upstream usage is unavailable.
- `internal/billing/billing.go:65-108` also calls
  `EstimateInputTokensOnly` for balance estimation.
- `DESIGN.md:1859` describes `EstimateTokens` as the available Rosetta
  estimation facility; the gateway's standalone estimator does not cover the
  media blocks that the ingress codecs now support.

### M03-05

- `internal/inwire/openai_responses.go:239-265` accepts `function`, `custom`,
  `local_shell`, and `namespace` in `validateResponsesTool`.
- `internal/inwire/openai_responses.go:286-290` emits a default error claiming
  only `"function"` and `"namespace"` can be proxied, contradicting the
  accepted branches.

### M03 Open Questions

- Responses `input_audio`: the gateway's `responsesContentPart` reads flat
  `audio_url` / `audio_data` / `format` fields
  (`internal/inwire/openai_responses.go:149-165`), and
  `responsesAudioBlock` consumes them. The pinned rosetta contract and
  `DESIGN.md` differ on whether this is an official Responses input shape.
  A request to a Responses upstream may be rejected, while a Chat upstream
  may translate it. This is a contract inconsistency, not enough to claim
  silent data loss.
- `responsesItemReasoningText` reads only the `summary` field
  (`internal/inwire/openai_responses.go:1119-1135`), not a reasoning item's
  `content` or `reasoning_text`. The existing design relies on
  `encrypted_content` for continuation, so the missing field is not confirmed
  as a defect.
- `decodeResponsesParts` returns nil for malformed part arrays or unknown
  part shapes (`internal/inwire/openai_responses.go:962-970`), after which an
  entire message can be dropped by `userMessageFromContent`. Whether this
  should be an explicit 400 depends on the desired malformed-input policy.
- There are no `internal/ingress` package tests. Chat media translation,
  media-aware estimation, and the output-ceiling reservation have no direct
  regression test.

## M04

### M04-01

- `internal/outwire/openai_chat.go:475-481` initializes the assistant message
  with `Content: jsonString(resp.Text())`.
- `internal/outwire/openai_chat.go:484-491` enters the tool-call branch when
  `len(resp.ToolCalls()) > 0` and unconditionally assigns
  `msg.Content = json.RawMessage("null")`, overwriting any text.
- `internal/outwire/openai_chat.go:525-537` serializes that message directly,
  so the loss reaches the wire.
- `internal/outwire/openai_chat.go:465-466` is the non-stream entry used by
  `internal/ingress/codec.go:72-74`; the streaming path in
  `internal/outwire/openai_chat.go:321-393` emits text and tool deltas
  independently.
- The current outwire tests cover text-only and tool-only non-stream cases,
  but not a response containing both. No test would catch this overwrite.

### M04-02

- `internal/outwire/anthropic.go:370-389` emits `message_start` only from
  `ensureStarted`.
- `internal/outwire/anthropic.go:439-482` routes `EventMessageEnd` directly to
  `sawTerminal = true`; it does not call `ensureStarted` or open a block.
- `internal/outwire/anthropic.go:487-516` then enters the `status == "ok"`
  branch and writes `message_delta` followed by `message_stop`.
- `internal/outwire/openai_responses.go:236-245` emits `response.created` only
  from `ensureStarted`.
- `internal/outwire/openai_responses.go:606-608` handles `EventMessageEnd`
  without starting the response.
- `internal/outwire/openai_responses.go:611-676` then writes
  `response.completed` for a successful terminal-only stream.
- `internal/outwire/anthropic_test.go:42-54` and
  `internal/outwire/openai_responses_test.go:14-30` both include content
  before finishing; neither exercises a terminal-only stream.

### M04 Open Questions

- `WriteOpenAIError` never emits `param` even for invalid request/model
  fields. `DESIGN.md:1030-1034` shows `param` in the OpenAI error shape but
  does not require a non-empty value; no client workflow proved the omission
  changes behavior.
- `MapUpstreamError` maps every `TransportError` to 504
  (`internal/outwire/errors.go:103-107`), while `DESIGN.md:1011-1013` says
  timeout/connect failures may be 504 or 502. The code is internally
  consistent but coarser than the documented split; the intended distinction
  for DNS/TCP/TLS failures is not established.
- The error text for non-ok streams is always "upstream stream truncated"
  even for overflow, error, and idle-timeout terminal states
  (`internal/outwire/anthropic.go:497-503`,
  `internal/outwire/openai_responses.go:627-630`). This is a diagnostics
  fidelity question; the status/error code recorded in usage still distinguishes
  the causes.
- `WriteNonStreamResponse` and `WriteResponsesResponseWithTools` ignore
  encoding errors after writing headers (`internal/outwire/openai_chat.go:530-536`,
  `internal/outwire/openai_responses.go:854-866`). Their payloads are built
  from ordinary maps/strings, so a real encode failure was not demonstrated.
- Outwire has no test for `WriteOpenAIError`, `MapUpstreamError`'s full status
  table, or terminal-only streams. The existing tests are strong for tool-call
  shape, usage details, write failures, and reasoning item boundaries.

## M05

### M05-01

- `internal/dataplane/handler.go:1577` calls `codec.WriteNonStream` without
  receiving a return value.
- `internal/ingress/codec.go:27` declares `WriteNonStream` as returning
  nothing. Its Chat, Anthropic, and Responses implementations at `:72-74`,
  `:106-108`, and `:141-143` only delegate to outwire.
- `internal/outwire/openai_chat.go:530-537` calls `json.NewEncoder(w).Encode`
  and discards the error. `internal/outwire/openai_responses.go:826-846` does
  the same for the Responses object.
- `internal/outwire/anthropic.go:187-205` logs an encode error but cannot
  propagate it to the dataplane caller.
- After that call, `internal/dataplane/handler.go:1580-1617` unconditionally
  writes a usage record with `Status: store.UsageStatusOK` and
  `HTTPStatus: 200`, commits the rate reservation, and returns
  `Outcome{Committed: true, Success: true}`.
- `internal/usage/recorder.go:483` skips balance charging only when the status
  is not `ok`, so the false-success record is eligible for charging.
- The streaming path establishes the intended attribution: detection at
  `internal/dataplane/handler.go:1271,1293-1307` records `canceled` with
  `downstream_write_failed` and does not punish the upstream.
- `DESIGN.md:1053` classifies `downstream_write` as downstream responsibility;
  `DESIGN.md:1093` states that balance charges apply only to successful
  requests; `DESIGN.md:1152` defines non-stream success as committing the
  complete response.
- Existing write-failure tests cover the three streaming sinks
  (`internal/outwire/write_failure_test.go`) but not non-stream writers or the
  handler's usage outcome.

### M05 Open Questions

- `internal/dataplane/handler.go:527-534` performs balance precheck over the
  full `res.Candidates` before `failover_enabled`, `failover_max_targets`,
  target-circuit, and structured-output filtering at `:546-619`. A disabled
  failover route with a costlier second target can therefore receive a 402
  based on a target it will never try. `internal/billing/billing.go` documents
  the conservative "most expensive target" policy, but the product contract
  does not establish whether conservative rejection is intended here.
- When all probe slots are busy or cooling, the handler returns 502. Whether
  this should be distinguished from a real upstream failure as a
  capacity/availability response is not defined by the current contract.
- `RateCommit.ReleaseQuota` uses `context.Background()` for cleanup
  (`internal/dataplane/rate.go`). This overlaps the M01 shutdown/store-close
  issue and is not counted as a separate finding.
- `internal/dataplane` has no package-local integration test that drives the
  complete `Handler.Handle` early-return and reservation-release matrix.
  Existing end-to-end coverage lives in `cmd/gateway`.

## M06

### M06-01

- `internal/upstream/balance.go:447-456` fetches
  `/dashboard/billing/subscription` and returns early only when that request
  fails or its JSON cannot be parsed.
- `internal/upstream/balance.go:475-484` sets `used := 0.0`, then performs the
  usage request inside `if b, err := ...; err == nil`. Both a request error and
  a JSON decode error leave `used` at zero; the error is never returned or
  reported.
- `internal/upstream/balance.go:485-486` constructs a success detail and
  returns `*limit - used` without any partial/unknown marker.
- `internal/upstream/balance.go:140-147` maps that return to
  `CredentialBalance.Status = "ok"`, `Amount`, and `Currency`, which the
  provider balance response exposes as a normal reading.
- `internal/upstream/balance.go:444-446` documents the requirement to report
  what can still be spent and warns against an apparently precise available
  balance; the fallback contradicts that requirement.
- `internal/upstream/balance_test.go:86-129` covers only the case where both
  the subscription and usage requests succeed. There is no test for a failed
  or malformed usage response.

### M06-02

- `internal/upstream/upstream.go:945-968` obtains the provider via
  `st.GetProvider`; this is the stored row, not the runtime pool entry.
- `internal/upstream/upstream.go:1350` passes that stored `p.Protocol` into
  `fetchModels`.
- `internal/upstream/upstream.go:1429-1434` selects `x-api-key` plus
  `anthropic-version` only when the supplied protocol is exactly `anthropic`;
  every other value, including `auto`, uses Bearer.
- `internal/upstream/upstream.go:1674-1690` resolves `auto` during client
  construction and mutates only the in-memory `ProviderEntry.Protocol`.
- `internal/upstream/protocol_test.go:15-44` verifies that `buildClient`
  resolves an Anthropic-shaped auto provider to `anthropic`; it does not
  exercise discovery.
- `internal/upstream/balance_test.go:344-373` verifies discovery uses the
  provider proxy, but the fixture does not assert protocol-specific auth
  headers and does not use `auto`.
- `internal/admin/model_handler.go:731-744` is the reachable admin discovery
  entry, and `web/src/views/ProviderDetail.vue:845-850` exposes it in the
  provider detail UI.

### M06 Open Questions

- `internal/upstream/upstream.go:1476` decodes a successful model-list
  response without an `io.LimitReader`, while the non-200 preview is bounded
  and balance reads are bounded to 64 KiB. The largest known real response is
  OpenRouter's 462-model list, which is not itself a demonstrated defect; the
  acceptable maximum auxiliary-response size is not established.
- `internal/upstream/proxy_probe.go:16-50` probes a fixed
  `https://www.gstatic.com/generate_204`. A network that blocks that host can
  report a valid proxy as failed; whether the probe target must be
  configurable is not specified.
- `internal/probe/runner.go:162-185` writes the probe record with the parent
  context while the model call uses `pctx`. This overlaps the M01 shutdown
  finding; a distinct user-visible failure from the context choice was not
  established.
- `internal/probe/runner.go:83-105` performs an immediate `runOnce` before
  checking cancellation. The operational consequence overlaps M01-03 and is
  not counted separately.

## M07

### M07-01

- `internal/store/usage_dao.go:348-409` defines the `cost_pending` state as a
  deferred price-lookup failure, and `freezeUsageCost` deliberately records
  zero cost while returning the pending flag.
- `internal/store/usage_dao.go:423-470` implements the only production
  recomputation queue. Its source query is
  `SELECT id FROM usage_records WHERE usage_state = 'cost_pending'`.
- `internal/store/usage_dao.go:620-709` keeps a record pending when a
  recomputed price still cannot be obtained or the user has insufficient
  balance. The insufficient-balance path rolls back the cost update and warns
  that the record remains pending.
- `internal/store/usage_archive.go:75-91` explicitly says archiving only
  retains the number of pending rows because the aggregate has no per-record
  token data and their cost is permanently zero.
- `internal/store/usage_archive.go:144-391` selects every row older than the
  cutoff, including pending rows, aggregates it into `usage_daily_rollups`,
  and deletes the same rowids from `usage_records`. The aggregation copies
  `cost_total` and increments `cost_pending_count`; it does not retain the
  fields needed to recompute or charge the row.
- `internal/gatewayapp/serve.go:130-235` schedules pruning every 24 hours, or
  every 15 minutes while backlog remains, and recomputation every five
  minutes. A continuously pending record therefore has no protection from
  the retention boundary.
- `internal/gatewayapp/serve.go:249-294` reconciles only `FindUnchargedUsage`
  from `usage_records`; `internal/store/usage_dao.go:506-534` explicitly
  excludes archived rows.
- `internal/store/usage_archive_boundary_test.go:144-200` asserts only that
  `CostPendingCount` survives the prune. It does not assert that the record
  can subsequently be priced or charged, so the current test treats the
  permanent-zero outcome as success.

### M07 Open Questions

- `internal/store/user_dao.go:601-630` makes `RecomputeUserUsage` write
  `users.used_tokens`, while quota admission reads
  `user_usage_totals.total_tokens` at `:488-558`. No production caller of
  `RecomputeUserUsage` was found; only `internal/store/user_dao_test.go`
  exercises it. The function's name and comment imply that it repairs the
  authoritative quota number, but the test does not assert that table.
- `internal/store/model_dao.go:510-533` omits `price_cache_creation` and
  `group_id` from the `UpsertUpstreamModel` INSERT columns. Production model
  import does not currently supply either field, and the conflict branch
  intentionally preserves price configuration, so no reachable user impact
  was established.
- `internal/store/usage_dao.go:281-310` uses `context.Background()` for the
  batched pricing lookup. This overlaps the M01 shutdown/store-close risk and
  has no independently demonstrated user outcome.
- `internal/store/tx.go:129-157` exposes route-target reference validation,
  but `CreateRouteWithHeadTarget`, `UpdateRouteWithHeadTarget`, and
  `ReplaceRouteTargets` do not call it themselves. All located admin entry
  points validate first, so this remains a DAO misuse and future-bootstrap
  risk rather than a reachable defect.
- `internal/store/route_target_dao.go:181-185`,
  `internal/store/topup_dao.go:108-111`, and
  `internal/store/network_dao.go:189-192` ignore errors from random-ID
  generation. A failure normally becomes a primary-key collision or a failed
  write; no successful operation with a corrupt ID was demonstrated.
- `internal/store/usage_dao.go:620-709` reads the prior record and price
  before opening its write transaction. The current production consumer is a
  single periodic goroutine, so concurrent double charging was not
  demonstrated. The idempotency and stale-read assumptions must be revisited
  if this API becomes reachable from concurrent workers or an admin action.
- `FindUnchargedUsage` has the same retention exposure for a successful,
  positive-cost row that remains uncharged past the cutoff: it scans only
  `usage_records`, while pruning removes those details. The normal reconciler
  runs every ten minutes and a temporary failure alone is unlikely to last 30
  days, but a persistent failure mode was not established. Keep this as an
  open question pending M08/M09 rather than a second M07 finding.
- Test coverage remains incomplete for the actual financial recovery
  invariant: the archive test accepts a surviving counter instead of a
  recoverable charge, `RecomputeUserUsage` does not assert the authoritative
  table, `UpsertUpstreamModel` does not cover new-model price/group columns,
  and no test drives a pending record across the prune boundary.

## M08

### M08-01

- `internal/store/balance_dao.go:312-380` implements the insufficient-balance
  branch. `chargeBalanceTx` rolls back the `balance_charges` insertion inside
  the `charge_attempt` savepoint, then `recordPendingRemainderTx` commits only
  a `balance_pending` row. No `(request_id, user_id)` completion marker remains
  in `balance_charges`.
- `internal/store/balance_dao.go:688-727` implements
  `recordPendingRemainderTx` as
  `INSERT OR IGNORE INTO balance_pending (request_id, user_id, amount_micros,
  ts)`. The schema at `internal/store/schema.go:468-481` shows these are two
  separate ledgers; the pending table has no corresponding charge marker.
- `internal/store/topup_dao.go:236-245` calls `settlePendingForUser` in the
  top-up transaction.
- `internal/store/balance_dao.go:782-923` reads the pending rows, deducts the
  affordable cents from `users.balance_cents`, partially updates or deletes
  pending rows, and returns. It does not insert into `balance_charges`.
  The successful settlement can therefore erase the only visible debt state
  without marking the usage request as charged.
- `internal/store/usage_dao.go:506-534` builds `FindUnchargedUsage` from
  `usage_records LEFT JOIN balance_charges` on the original
  `request_id`/`user_id`. A legacy settled request that has no original charge
  marker still satisfies `c.request_id IS NULL`; a recomputed request is the
  same because its marker uses another key.
- `internal/store/usage_dao.go:536-556` calls `ChargeBalance` with the original
  request key, which creates the missing marker and debits the full cost.
- `internal/store/usage_dao.go:588-597` defines the recomputation key as
  `recompute:<record_id>:<cost_micros>`. The call site at `:690-693` uses
  that key with `chargeBalanceTx`, so recomputation never creates the original
  request marker.
- `internal/store/usage_dao.go:641-727` updates `cost_total`, charges the
  positive delta, and commits. On the next reconcile scan the updated row is
  still uncharged according to the original-key join.
- `internal/gatewayapp/serve.go:208-245` starts recomputation after 45 seconds
  and repeats every five minutes.
  `internal/gatewayapp/serve.go:249-293` starts reconciliation after one
  minute and repeats every ten minutes. Some recompute paths can therefore be
  followed by a reconcile pass in the normal schedule.
- `internal/store/balance_settle_test.go:254-340` verifies that two
  recomputations of the same target cost are idempotent. It does not call
  `FindUnchargedUsage` after the recomputation.
- `internal/store/balance_pending_test.go:14-123` verifies pending amount and
  atomicity, and `internal/store/balance_settle_test.go:223-244` verifies that
  the debt enters the pending ledger. Neither test combines pending settlement
  with a later uncharged-usage scan.
- No located test covers `insufficient balance -> balance_pending -> top-up
  settlement -> FindUnchargedUsage -> ChargeUnchargedUsage`.

### M08-02

- `internal/dataplane/rate.go:60-85` reads `quotaReserved` and
  `userQuotaReserved`, immediately sets the in-memory field to zero, then calls
  the corresponding store release. On error it only emits a WARN.
- `internal/store/key_dao.go:580-590` and
  `internal/store/user_dao.go:551-565` are single `UPDATE ... reserved_tokens =
  MAX(0, reserved_tokens - ?)` statements. They return the SQL error to the
  caller.
- The request cleanup contract in `DESIGN.md:1237-1241` requires lifetime
  reservations to be returned at the end of the request. A failed release
  contradicts that contract.
- `internal/store/reservation_recovery.go:47-90` clears every positive
  reservation during startup, before traffic begins. That is the only located
  recovery path; no periodic release retry or durable failed-release record
  exists.
- `internal/store/key_dao.go:530-535` and
  `internal/store/user_dao.go:507-513` count the persisted reservation in the
  admission predicate. A stranded value therefore directly reduces available
  quota until restart.
- No located test injects a failure from `ReleaseQuota` or
  `ReleaseUserQuota` and retries the request lifecycle.

### M08-03

- `internal/billinghttp/billinghttp.go:259-287` initializes a 30-day default
  and then parses each explicit timestamp only if
  `strconv.ParseInt(v, 10, 64)` succeeds and the value is positive. Parse
  errors and non-positive values leave the default unchanged.
- `internal/billinghttp/billinghttp.go:177-181` and `:221-225` treat only
  `bad == true` as a client error. The malformed-value case never sets `bad`
  unless the resulting default range happens to be reversed, so the handlers
  return 200.
- `internal/billinghttp/billinghttp.go:290-312` implements the contrasting
  date-range parser: an explicitly supplied start or end date that fails to
  parse returns `bad=true`, and the caller at `:94-103` returns 400. Its
  comments at `:292-295` explicitly reject silent fallback because it gives
  the caller another window while appearing to honor the filter.
- `cmd/gateway/billing_scope_test.go:81-110` covers malformed and reversed
  `start_date`/`end_date` for the legacy billing usage endpoint. No test
  exercises malformed `start_time`/`end_time` for the organization usage or
  costs endpoints.

## M09

### M09-01

- `internal/usage/recorder.go:144-153` defines `persistJob.finish()` as the
  safe execution point for the quota-release callback.
- `internal/usage/recorder.go:176` caps a worker batch at 50 jobs.
- `internal/usage/recorder.go:280-291` and `:331-339` place the entire batch
  back into `stillPending` after persistent flush failure; if `retryPause`
  observes shutdown, they call `logPendingAbandoned(stillPending)` and return.
- `internal/usage/recorder.go:377-395` constructs a ten-entry sample and calls
  `job.finish()` inside that loop. The `break` at ten entries prevents every
  later job's callback from running.
- `internal/gatewayapp/serve.go:102-107` calls `Abort()` after the drain
  deadline, making this abandonment path reachable during normal shutdown.
- `internal/dataplane/handler.go:1451-1498` and `:1579-1610` attach quota
  settlement to this callback for streaming and non-streaming requests.
- `internal/usage/recorder.go:820` wraps callbacks in `sync.Once`, so a
  corrected loop can call `finish()` without double release if a retry later
  reaches the same job.
- `cmd/gateway/usage_abort_test.go` records one job without a callback;
  `cmd/gateway/usage_flush_retry_test.go` tests persistence recovery but never
  asserts callback execution or a batch larger than ten.

### M09-02

- `internal/config/config.go:116-126` states that
  `audit_log_keep_rows=0` disables pruning and retains every row.
- `internal/config/config.go:378-382` replaces zero with 20,000 during
  defaulting.
- `internal/config/config.go:503-506` rejects values below `-1` and documents
  `-1` as the permanent-retention value.
- `DESIGN.md:1456-1462` also defines `-1` as permanent retention, leaving the
  code comment as the inconsistent contract.

### M09 Open Questions

- The synchronous fallback failure log at
  `internal/usage/recorder.go:813-818` includes the error and access-key ID but
  omits `request_id`, `user_id`, and model. It is reachable when the queue is
  full or closed and the synchronous write also fails. Whether the product's
  reconciliation contract requires a request-level identifier at this
  diagnostic point is not established.
- `internal/server/autoreload.go:136-141` audits a successful ordinary write
  but not failed writes or bootstrap writes, while `DESIGN.md:1676` says all
  write operations are audited. The security/audit requirement for failed and
  bootstrap mutations is not established in this module; defer to M12/M20.

## M10

### M10-01

- `internal/admin/user_handler.go:208-225` derives the account throttle key
  directly from `username := strings.TrimSpace(req.Username)` and calls
  `Allow(username)` without any case normalization.
- `internal/admin/user_handler.go:229-237` calls `accThr.Fail(username)` on
  every failed login, and `:302-307` calls `accThr.Success(username)` after a
  successful login. These use the same case-sensitive key.
- `internal/admin/user_handler.go:341-357` initializes the account throttle
  with a five-failure limit. `internal/guard/throttle.go:95-116` uses the
  supplied string as the exact map key.
- `internal/store/schema.go:304-311` declares
  `username TEXT NOT NULL COLLATE NOCASE UNIQUE`, and
  `internal/store/user_dao.go:158-163` queries with
  `WHERE username = ? COLLATE NOCASE`. Different case spellings therefore
  resolve to one account while producing different throttle buckets.
- `internal/admin/scope_test.go:641-688` verifies cross-IP brute-force
  blocking with one spelling (`alice`), but does not alternate case. No
  located test covers case variants.
- `DESIGN.md:1277-1286` documents the IP-based login throttle, while the
  account-dimension code at `internal/admin/user_handler.go:69-74` states that
  it exists specifically to stop many-IP attacks on one account.

### M10-02

- `internal/guard/throttle.go:95-116` creates a new `failEntry{inflight: 1}`
  on the first `Allow` for a key.
- `internal/guard/throttle.go:119-128` decrements `inflight` on `Release` but
  never deletes the entry when it reaches zero.
- `internal/guard/throttle.go:176-191` clears `count` and `until` on
  `Success`, but deletes only when `inflight <= 0`. The caller has not yet
  released its in-flight token, so a successful single request leaves the
  entry behind; the later `Release` only makes it idle.
- `internal/server/user_auth.go:81-135` calls `Allow` for every authenticated
  management request, then `Success` and a deferred `Release` on the success
  path. A store lookup error at `:102-105` returns through `Release` without
  `Fail`, producing the same idle-entry outcome.
- `internal/guard/throttle.go:193-244` runs `sweepLocked` only from `Fail`
  (`:151`). A stream of successful authentications never invokes it, and the
  map can grow beyond `maxEntries = 4096`.
- `internal/admin/user_admin_handler.go:887-904` has the same
  `Success`-then-deferred-`Release` shape for password changes.
- `internal/server/throttle_test.go` and
  `internal/server/session_throttle_concurrency_test.go` cover failure
  accumulation, success reset, cooldown expiry, and concurrency caps, but do
  not assert entry removal after a successful authentication.

### M10 Open Questions

- `internal/authhttp/auth.go:16-25` does not explicitly map
  `auth.ErrUserDisabled` or `auth.ErrKeyUnowned`; both fall through to the
  default 401 `"authentication failed"`. The data-plane error contract does
  not state that these two conditions need distinct client-visible codes.
- `internal/auth/auth.go:79-121` calls `snapshot.Get()` first for the key and
  again for the owning user. A snapshot swap between those reads can combine
  a key from one snapshot with a user from another. The window and any
  security consequence were not established.
- `internal/admin/user_handler.go:485-593` does not check `h.mgr.Enabled()`
  before `BootstrapSetup` writes the password and closes the bootstrap window.
  If the manager were unavailable, the write could succeed and then `Issue`
  fail. Normal startup generates a session secret, so a reachable disabled
  state in production is not established.
- `internal/server/user_auth.go:22-23` hard-codes `SessionTTL = 8h`, while
  `userauth.Manager.TTL()` is the actual token lifetime source. No located
  production caller relies on `SessionTTL`, so this is a potential drift
  point rather than an active behavior defect.
- `internal/admin/password_setup_handler.go:98-136` exposes
  `CheckPasswordSetupLink` without a throttle. It performs only a SHA-256 and
  one indexed lookup, so a meaningful denial-of-service impact was not
  established.
- `internal/userauth/session.go:146-181` skips ACL tightening on Windows;
  the file is still created with mode `0600`, and the cross-platform
  deployment contract for Windows ACLs was not established in M10.

## M11

### M11-01

- `internal/api/api.go:498-506` installs the production middleware from outer
  to inner as `WithTrustedProxies`, `RequestSizeLimit`, `CORS`,
  `SecurityHeaders`, `Middleware`, `WithPublicOrigin`, `Recovery`, then the
  root mux.
- `internal/server/server.go:128-135` replaces `r.Body` with
  `http.MaxBytesReader(w, r.Body, maxBytes)` for every request. It does not
  inspect the resulting decode error.
- `internal/admin/helpers.go:127-136` checks `Content-Type`, writes 415 for a
  non-JSON media type, and otherwise returns `json.Decoder.Decode` directly.
- `internal/admin/config_transfer_handler.go:222-228`,
  `internal/admin/credential_handler.go:74-80`,
  `internal/admin/group_handler.go:126-131`,
  `internal/admin/key_handler.go:173-178`,
  `internal/admin/model_handler.go:257-262`,
  `internal/admin/provider_handler.go:179-184`,
  `internal/admin/route_handler.go:86-91`,
  `internal/admin/user_handler.go:184-189`, and
  `internal/admin/user_admin_handler.go:269-274` are representative callers.
  Every located `decodeJSON` call uses the same branch: write 415 for
  `errUnsupportedMediaType`, otherwise write 400 with `invalid JSON: ...`.
- `internal/dataplane/handler.go:245-250` uses `errors.As` for
  `*http.MaxBytesError` and writes 413 `request_too_large` with the configured
  byte limit. `internal/ingress/codec.go:21-23` documents that the caller owns
  the malformed-JSON versus oversized-body distinction.
- `internal/config/config.go:356-357` defaults the limit to 32 MiB, and
  `internal/config/config.go:475` permits a minimum of one byte and a maximum
  of 512 MiB. The trigger therefore covers both ordinary admin mutations and
  configuration import.

### M11 Routing and Middleware Evidence

- `internal/server/server.go:48-76` generates the request ID, injects it into
  the context, sets `X-Request-Id`, wraps the response, and logs method, path,
  final status, duration, request ID, and resolved client IP.
- `internal/server/server.go:82-122` records `WriteHeader` and `Write` while
  holding a mutex. `Committed` and `StatusCode` expose that state to recovery.
- `internal/server/server.go:181-216` forwards `Flush` and `Unwrap`.
  `internal/outwire/openai_chat.go:204-224`,
  `internal/outwire/openai_responses.go:214-217`, and
  `internal/outwire/anthropic.go:319-345` write an encoded event before
  flushing. No located production path flushes before the first response
  write.
- `internal/server/server.go:218-260` checks `Committed` before writing the
  recovery JSON. An uncommitted panic receives a 500; a committed response is
  left to terminate as a broken stream.
- `internal/server/server.go:137-160` applies CORS only outside the `/admin`
  prefix and short-circuits every non-admin `OPTIONS` request with 204.
  `internal/api/api.go:429-474` serves only `/admin/`,
  `/admin/theme-init.js`, and `/admin/assets/*`.
- `internal/api/api.go:418-425` registers five method-specific
  `/admin/api/` fallbacks to `adminAuto`. Concrete protected endpoints are
  registered only on `adminMux`; public login/bootstrap/setup endpoints are
  registered on `publicAdminMux` and the root mux.
- `cmd/gateway/routes_test.go:23-33` pins 108 route records.
  `internal/server/authz_matrix_test.go:91-126` deliberately removes trailing
  slash prefix fallbacks before building the per-endpoint authorization
  matrix.
- `internal/server/autoreload.go:83-270` captures at most 64 KiB of
  POST/PATCH/PUT bodies, records successful write audits, and skips runtime
  reload for failed writes and the reload/test/discover endpoints.
- `internal/server/server_test.go` does not exist. No located
  package-local test directly covers recovery before/after commit,
  request-body overflow on an admin route, trailing-slash routing, unsupported
  methods, or `HEAD`/`OPTIONS` boundary behavior.

### M11 Open Questions

- `internal/server/server.go:117-121` forwards `Flush` without marking
  `written=true`. Net/http commits a 200 response on a flush before headers.
  A handler that flushes an empty response and later panics could therefore
  make `writeInternalError` append JSON to an already committed stream. No
  located production path flushes before its first write, so the defect is
  not confirmed.
- Root-mux method-specific `/admin/api/` fallbacks route trailing-slash and
  unmatched-method requests through authentication and `adminMux`.
  `authz_matrix_test.go` omits those patterns. No bypass was found, but the
  redirect/405/HEAD boundary is not covered by the real-route matrix.
- `internal/server/server.go:137-160` answers `OPTIONS` for every non-admin
  path, including unregistered paths, before routing. The measured behavior
  is intentional CORS preflight; no sensitive method or header exposure was
  established.

## M12

### M12-01

- `internal/admin/group_handler.go:133-138` trims the create request's name,
  rejects an empty result, and then checks the 64-character maximum.
- `internal/admin/group_handler.go:161-188` performs the update path. It
  trims a supplied name and checks only `len(name) > 64` before assigning it
  to the existing group and calling `UpdateGroup`.
- `internal/admin/group_handler_test.go:123-132` covers empty and
  whitespace-only names for create only. The update test uses a non-empty
  replacement and does not exercise blank input.
- The group name column is non-null and case-insensitively unique at
  `internal/store/schema.go:333-337`, but nothing rejects an empty string, so
  the handler's update path can persist it.

### M12-02

- `internal/admin/config_transfer_handler.go:387-390` states that preview must
  match actual import.
- `internal/admin/config_transfer_handler.go:391-397` assigns the literal
  `"dry-run-placeholder"` to every new provider in the preview attachment.
- `internal/admin/config_transfer_handler.go:536-540` keys existing-model
  deduplication by `providerID + "\x00" + modelID`.
- `internal/admin/config_transfer_handler.go:556-566` skips and marks a model
  as existing after the first occurrence. With one shared provider ID in the
  preview, the second new provider's same-named model is skipped.
- `internal/admin/config_transfer_handler.go:615-625` increments the dry-run
  model count only for models that pass that deduplication step. The real
  write path assigns a generated provider ID per provider before the same
  attach loop, so two occurrences become two database rows.
- `DESIGN.md:1649` defines the preview/actual consistency requirement.

### M12-03

- `internal/admin/key_handler.go:392-410` documents the no-loosening rule and
  explicitly excludes `allowed_models`, arguing that a group allowlist is the
  effective administrator-controlled ceiling.
- `internal/admin/key_handler.go:411-456` checks enabled, quota, RPM, TPM,
  expiry, and IP restrictions, but performs no model-allowlist comparison.
- `internal/admin/key_handler.go:614-621` converts an explicitly supplied
  empty model list to nil and persists it, which removes the key-level
  restriction.
- `DESIGN.md:600-604` says ordinary users may only tighten key settings;
  `DESIGN.md:717-718` says admin-applied restrictions on a specific key cannot
  be undone by PATCH.
- `internal/admin/group_handler_test.go:424-431` confirms the owner-initiated
  clear succeeds. It uses a self-created key and does not establish an
  admin-applied restriction, so the test validates the implementation while
  also confirming the contract gap.

### M12 Cleared Candidates

- `internal/admin/model_handler.go:792` describes model import as an upsert,
  and `internal/store/model_dao.go:531-553` confirms that
  `ImportUpstreamModels` performs the full loop in one transaction.
  `internal/store/model_dao.go:516-528` uses
  `ON CONFLICT(provider_id, model_id) DO UPDATE`, so repeated imports are
  idempotent at the model key and the earlier plain-INSERT suspicion is
  disproved.
- Provider plus optional credential creation, route head-target creation,
  route-target replacement, user role/status updates with session revocation,
  access-key field/owner updates, settings save, password reset, usage
  pruning, credential exclusive switching, batch price update, and ordinary
  batch model import were traced to their transaction boundaries. No
  additional confirmed M12 finding was established in those paths.
- Network deletion returning providers to direct connection is intentional,
  matching the network deletion contract.

### M12 Open Questions

- `internal/admin/model_group_handler.go:229-236` and `:272-280` call
  `SetModelGroup` once per model, while
  `internal/store/model_group_dao.go:116-132` executes each update in
  autocommit. A database error or concurrent deletion after earlier updates
  can return 500 with a partially changed assignment. Missing-ID validation
  occurs before the loop and is covered, but no deterministic mid-loop
  failure test exists, so this is not counted as a confirmed finding.
- `internal/admin/credential_handler.go:130-139` creates a disabled credential
  and then enables it separately; `:200-210` updates fields and then performs
  exclusive enable separately. Enable itself is transactional at
  `internal/store/credential_dao.go:170-195`, but a failure between the two
  operations leaves a disabled or partially updated credential. A deterministic
  user-reachable trigger was not established.
- `internal/admin/config_transfer_handler.go:488-511`, `:611-623`, and
  `:665-677` append row-level write warnings and continue inside the outer
  transaction, allowing later rows to commit after an earlier row-level write
  failure. `DESIGN.md:1652-1655` explicitly describes invalid imported rows as
  skipped and reported, so whether database write errors should abort the
  batch is a contract decision rather than a proven defect.

## M13

### M13-01

- `internal/admin/audit_handler.go:33-47` reads `limit` with
  `r.URL.Query().Get`, attempts `strconv.Atoi`, and leaves the default 50 in
  place when parsing fails. A successfully parsed value below 1 becomes 1 and
  one above 500 becomes 500.
- `internal/admin/helpers.go:13-17` writes the JSON status immediately and
  encodes directly to the response writer; there is no buffering or query
  validation layer that could reject the malformed value later.
- `internal/api/api.go:346-350` registers `GET /admin/api/audit` directly to
  `auditHandler.List`. The admin guard wraps the route, but no middleware
  parses or normalizes endpoint-specific query parameters.
- `internal/admin/stats_handler.go:369-381` rejects a non-numeric `from` or
  `to` with an error, and the usage handlers convert that error to 400 via
  `queryRangeExplicit`.
- `internal/admin/usage_handler.go:44-60` intentionally treats malformed
  usage `limit`/`offset` values as defaults, and
  `internal/admin/usage_handler_test.go:496-524` explicitly pins
  `limit=abc&offset=xyz` to 200. That tested clamping contract does not cover
  the audit endpoint.
- `internal/store/audit_dao_test.go:15-33` covers nullable `fields` and newest
  ordering; no located test exercises audit query-parameter parsing or the
  1/500 clamps.

### M13 Cleared Candidates

- `internal/api/api.go:257-350` places `/admin/api/audit` and
  `/admin/api/stats/overview` behind the guarded admin mux. `/audit` lacks a
  handler-local `requireAdmin`, but its exact route is not in the
  user-accessible prefix allowlist, so no reachable privilege issue was
  established.
- `callerScope` in the usage handlers returns a non-admin sentinel for
  anonymous/empty identities and the user ID for ordinary users. No fallback
  to global scope was found, and the real-route authorization matrix covers
  ordinary-user isolation for usage, history, CSV, grouping, and topups.
- Usage filters use parameters for user-controlled values. Grouping SQL uses
  fixed literal column/table names selected by the server, not query-string
  identifiers.
- `writeGroupEntries` initializes an empty slice, so an empty grouped result
  encodes as `[]` rather than `null`.
- History pagination uses `ORDER BY u.ts DESC, u.id DESC`; the paging test
  verifies no duplicate or skipped row across pages.
- CSV export emits `X-Export-Truncated` and `X-Export-Total` when the result
  is capped.
- The route catalog is admin-only, returns aliased empty arrays, and its test
  checks that sensitive fields are absent.
- Probe summary and timeline enforce group-allowlist scoping; mixed
  allowed/denied IDs are filtered and an all-denied request returns 403 rather
  than 400.

### M13 Open Questions

- `internal/admin/probe_handler.go:235-238` treats every granularity other
  than exactly `day` as hour. No contract requiring 400 for an unknown value
  was located, so the fallback is recorded as a behavior question rather than
  a defect.
- `internal/store/probe_dao.go:67-121` computes window counts from the
  configured window while `last latency`/`last error` fields come from the
  latest record even when it is outside that window. `Classify` returns
  `unknown` when `WindowCount` is zero, so current health is not
  misclassified; whether the UI intends stale last-error metadata to be shown
  is unresolved.
- `writeGroupEntries` streams through `writeJSON`, whose helper writes the
  status before encoding. A mid-iteration `rows.Scan` or `rows.Err` failure
  could therefore attempt a 500 after partial output. No deterministic
  normal-data trigger was established.
- `routeCatalog` performs five independent reads, so counts and relationship
  data can come from slightly different snapshots. No consistency contract
  for this diagnostic view was established.
- `internal/store/usage_source.go:155-246` intentionally counts an archived
  day when a queried interval only partially overlaps it. The conservative
  boundary behavior is documented and covered by
  `internal/store/usage_archive_boundary_test.go:3-39` and
  `internal/store/usage_source_index_test.go:91-165`, so it is not a finding.

## M14

### M14-01

- `internal/store/usage_dao.go:506-520` selects successful, positive-cost,
  non-empty-request rows with no corresponding `balance_charges` marker,
  ordered oldest first and capped by `limit`.
- `internal/store/usage_dao.go:547-555` treats `ErrInsufficientBalance` as a
  successful reconciliation result and returns nil. `ChargeBalance` rolls
  back the attempted charge and records the amount in `balance_pending`
  (`internal/store/balance_dao.go:312-380`), so no marker is created.
- `internal/gatewayapp/serve.go:255-278` scans at most 500 rows and increments
  `charged` for every nil result. It repeats the same ordered page every ten
  minutes (`:283-289`), so the oldest blocked rows can occupy every cycle.
- `internal/store/usage_dao.go:423-457` scans `cost_pending` rows with no
  stable order; `RecomputeCost` keeps insufficient-balance rows pending
  (`:632-704`). Both recovery paths have the same starvation shape.
- Existing tests cover a single reconciliation and its idempotency
  (`internal/store/balance_settle_test.go:381-443`) and a pending-cost limit
  (`internal/store/cost_pending_test.go:151-170`). No test builds more than one
  full page of deferred rows ahead of a recoverable row.

### M14-02

- `internal/store/pricing.go:64-84` documents and implements
  `EffectivePrices`: missing input falls back through output, cache-hit, and
  cache-creation; missing cache prices fall back to input; missing output falls
  back to effective input.
- `internal/store/pricing.go:147-160` implements `PriceUsage` with raw
  arguments and only substitutes input for cache-hit/cache-creation. It does
  not fill a missing output or input price.
- `internal/store/usage_dao.go:386-408` reads raw `upstream_models` prices with
  `COALESCE(..., 0)` and passes them directly to `priceUsage`, which forwards
  to `PriceUsage`.
- `internal/store/pricing.go:199-225` uses `EffectivePrices` for precheck.
  `internal/billing/billing.go:65-99` states that precheck and actual charging
  share the fallback policy.
- `cmd/gateway/balance_test.go:456-497` compares the estimator with
  `PriceUsageWithPrices`; `internal/store/pricing.go:174-176` forwards raw
  prices from that helper, so the test does not exercise a persisted record's
  `cost_total`.

### M14 Cleared Candidates

- `internal/store/balance_dao.go:312-380` rolls back the charge placeholder and
  remainder on insufficient balance before recording pending debt.
- `internal/store/balance_dao.go:688-923` and the top-up path settle pending
  debt in one transaction, read exact rows, deduct only the affordable amount,
  and delete only rows covered by that deduction.
- `internal/store/usage_dao.go:202-340` makes batch insertion all-or-nothing
  and returns costs aligned with inserted records.
- Balance preflight intentionally uses the maximum across the full resolved
  candidate chain (`internal/dataplane/handler.go:527-537`,
  `internal/billing/billing.go:96-108`) before failover filtering. The
  conservative direction matches the stated admission policy.

### M14 Open Questions

- Because the preflight covers every candidate before `failover_enabled`,
  circuit availability, structured-output filtering, and `failover_max_targets`
  are applied, a primary-only route with an expensive unused backup can be
  rejected as unaffordable. The contract favors a conservative estimate, so
  whether to restrict it to the attemptable prefix is a policy decision.
- `RecomputePendingCosts` has no stable ordering. This is folded into M14-01
  rather than counted as a separate root cause.
- `internal/store/topup_dao.go:266-295` commits a balance adjustment when the
  top-up ledger insert fails and returns success with an ERROR log. There is
  no durable outbox; whether the documented manual-reconciliation policy is
  sufficient remains open.
- Sub-cent pending debt is written off at settlement, and debt for unlimited
  or deleted users is dropped (`internal/store/balance_dao.go:738-923`). The
  product has not specified whether sub-cent debt must be carried.
- The reconciliation worker has no package-local test for backlog ordering,
  deferred-recording accounting, or its `charged` summary.

## M15

### M15-01

- `web/src/api.ts:154-158` sends login through `post()`, which is
  `request('POST', path, ...)`.
- `web/src/api.ts:282-287` handles status `401` before reading the response
  body, clears the token and `session.me`, and throws a fixed
  `ApiFail(401, '登录已失效，请重新登录')`.
- `internal/admin/user_handler.go:271-284` returns
  `401 {"error":{"message":"用户名或密码错误"}}` for both an unknown user and
  a wrong password.
- `web/src/views/Login.vue:93-108` displays `ApiFail.message` unchanged. Its
  comment says the backend's anti-enumeration message should be shown as-is,
  but the request wrapper replaces it first.
- `web/src/api.ts:185-214` already implements `publicPost()`, which parses the
  error envelope without applying the authenticated-session `401` side
  effects. Password setup uses it; login does not.
- `web/tests/apiContract.test.mjs:136-150` verifies the generic `401` token
  clearing behavior but has no invalid-login case, so the current suite does
  not distinguish these two meanings.

### M15-02

- `web/src/api.ts:165-177` awaits `post('/bootstrap', ...)`, saves the returned
  token, then awaits `get('/me')` before returning success.
- `internal/admin/user_handler.go:463-506` describes the one-time bootstrap
  window. `internal/admin/user_handler.go:562-592` writes the password, closes
  the marker, issues a token, and returns `200`.
- If the subsequent `/me` call fails, the rejection propagates through
  `bootstrapSetup()` to `web/src/views/Login.vue:117-139`, which displays the
  generic `设置密码失败`. No code distinguishes “bootstrap rejected” from
  “password committed, profile lookup failed”.
- `login()` at `web/src/api.ts:154-158` has the same sequencing. It is less
  severe because the subsequent login attempt is repeatable.

### M15-03

- `web/src/api.ts:122-134` catches every failure from `GET /bootstrap`,
  explicitly maps it to `needsSetup=false`, and leaves `backendReady=true`
  from the successful `/session` request.
- `web/src/views/Login.vue:37-41` chooses `login` whenever the backend is ready
  and setup is not pending.
- `internal/admin/user_handler.go:423-446` returns `500` if either bootstrap
  state read fails and states that reporting “no setup needed” would send the
  user to a login box that cannot work.
- `internal/admin/helpers.go:57-61` emits a generic server error for those
  failures. The frontend catch discards the status and message.

### M15 Cleared Candidates

- `web/src/generated/routes.ts` is generated from the production mux and the
  frontend test asserts the exact route count and public endpoint set.
- `web/src/generated/dto-contract.ts` covers 55 response/request types, and
  `web/tests/dtoContract.test.mjs` compares every listed JSON field with
  `web/src/types.ts`.
- `web/src/password.ts` uses Unicode property classes in the same
  lower/upper/digit/other order as `internal/userauth/password.go:50-73`.
- `web/src/router.ts:64-92` rejects protocol-relative redirect targets and
  keeps the admin-only redirect client-side while backend authorization
  remains authoritative.
- Credential-bearing downloads and ordinary API requests both clear the local
  session on `401`; the generic behavior is correct for authenticated
  endpoints. The defect is applying it to the public login endpoint.

### M15 Open Questions

- `web/src/range.ts:58-65` describes `[from, to)` but the backend usage filters
  use `ts <= to` (`internal/store/usage_dao.go:839-848`,
  `internal/admin/usage_handler.go:143-146`). The frontend normally passes
  `Date.now()` as `to`, so a duplicate is limited to records sharing that exact
  millisecond. Whether the comments should say closed or the API should use a
  half-open upper bound is not settled by `DESIGN.md`.
- `loadSession()` treats a `/me` rate-limit or server failure as “not logged
  in” (`web/src/api.ts:135-142`). The route guard then shows the login page.
  Whether a transient profile failure should render a retryable degraded state
  rather than an authentication prompt is a product decision.
- Password strength and range helpers have no direct unit tests. Their current
  implementations match the inspected contracts, but drift would only be
  caught by the generated DTO/API tests if a field or route changed.
- The frontend test suite has no component-level coverage for route guards,
  login rendering, session bootstrap, or one-time password setup. The static
  findings above are therefore not contradicted by a passing runtime test.

## M16

### M16-01

- `web/src/views/ProviderDetail.vue:417-437` keeps the balance response and
  busy flag in page-level state. `queryBalance()` writes the response to
  `balance` without capturing or comparing the provider id that initiated the
  request.
- `web/src/views/ProviderDetail.vue:686-693` resets the provider, credentials,
  models, and all editor forms when `providerId` changes, but does not reset
  `balance` or `balanceBusy`.
- `web/src/components/CredentialPanel.vue:150-166` filters each credential's
  balance only by `credential_id`; it never verifies
  `balance.provider_id === props.providerId`.
- `web/src/types.ts:344-353` includes `provider_id` in `ProviderBalance`, and
  `internal/upstream/balance.go:87-154` populates it from the queried provider.
  The contract therefore supports rejecting stale results; the page and panel
  currently do not use it.
- R10 failure-first evidence: the integration contract initially failed because
  `ProviderDetail.vue` had no request generation, no provider-id response
  check, and no invalidate-on-navigation. `CredentialPanel.vue` rendered
  results without comparing `provider_id`.
- R10 implementation evidence: `web/src/asyncState.ts` owns monotonic request
  generations. `ProviderDetail.vue` captures the provider id and generation,
  rejects a mismatched/current response, clears balance on navigation, and
  scopes its busy completion. `CredentialPanel.vue` derives `currentBalance`
  only when `balance.provider_id === providerId`.

### M16-02

- `web/src/views/Models.vue:163-181` assigns the result of `probeTimeline()`
  directly to `bucketsByUpstream` and clears `timelineLoading` in `finally`,
  with no sequence number or cancellation.
- `web/src/views/Models.vue:197-207` launches timeline loads without awaiting
  or tracking earlier calls. `web/src/views/Models.vue:347-353` also launches
  loads on range and granularity changes.
- `web/src/components/HeartbeatBar.vue:93-110` derives the time axis from the
  current `hours` and `bucketMinutes` props, while bucket values come from the
  last response to arrive. An older response can therefore be rendered on a
  newer axis.
- R10 failure-first evidence: the integration contract initially failed
  because `loadTimeline()` wrote the response and loading completion directly
  without a generation or selection key.
- R10 implementation evidence: each timeline load captures ids, days, and
  granularity as `selectionKey(...)` plus a `RequestGeneration`. Data, errors,
  and loading completion are published only when both remain current.

### M16-03

- `web/src/views/Settings.vue:44-47` initializes the tab from the route query,
  so a direct `/settings?tab=price` starts with `tab.value === 'price'`.
- `web/src/views/Settings.vue:486-488` loads prices only inside
  `watch(tab, ...)` when the tab changes and `pricesLoaded` is false.
  `onMounted()` at `web/src/views/Settings.vue:821-823` calls only `load()`.
- `web/src/views/Settings.vue:400-487` leaves `pricesLoading=false`,
  `providers=[]`, and `totalModelCount=0` until `loadPrices()` runs.
- `web/src/views/Settings.vue:1250-1275` disables “新增价格” when
  `providers.length === 0` and renders the “还没有任何模型” empty state when
  `totalModelCount === 0`, making the unloaded state indistinguishable from a
  genuinely empty model catalogue.
- R10 failure-first evidence: the integration contract initially failed
  because the price watcher was not immediate and the empty-state branch did
  not require `pricesLoaded`.
- R10 implementation evidence: the watcher is immediate, `pricesState`
  distinguishes `not-loaded`, `loading`, `error`, and `ready`, and the empty
  catalogue message is gated by `pricesLoaded && totalModelCount === 0`.

### M16 Cleared Candidates

- `web/src/views/Keys.vue` sends quota and rate limits only for administrators,
  preserves untouched expiry values, sends explicit model arrays for clearing,
  and guards creation, toggling, and plaintext-key cleanup. These behaviors
  match the inspected key handler contracts.
- `web/src/views/UsersGroups.vue` blocks self-demotion/self-deletion and
  passwordless admin promotion, confirms destructive user actions, guards
  per-row mutations, and explains group bindings that prevent deletion.
- `web/src/views/Routes.vue` sequences target-chain loads, blocks whole-chain
  PUT saves until the chain is ready, and reports partial create/save failures.
- `web/src/views/ProviderDetail.vue` sequences provider loads, isolates
  credential/model subresource failures, and guards credential toggles.
- `web/src/views/UsageDetail.vue:85-92` and `:266-273` await route navigation
  before exporting, so the earlier “export uses old filters” candidate does
  not reproduce from the inspected code.

### M16 Open Questions

- `web/src/views/Networks.vue` has no per-row pending guard for delete and no
  delete/save busy state; repeated clicks after the confirmation dialog can
  generate duplicate DELETE requests. The backend returns a normal 404 for the
  second request, and no incorrect state was established, so this is recorded
  as an interaction robustness question rather than a confirmed finding.
- `web/src/views/ModelGroups.vue` keeps `mForm.group` as a snapshot. After
  `removeMember()` calls `load()`, it tries to rebind that snapshot by id; if
  the group disappeared concurrently, the modal can retain stale data. No
  normal-flow stale rendering was established.
- `web/src/views/Models.vue`'s `timelineLoading` is shared by every card. While
  one timeline request is active, all drawn cards show “加载中…” even if only
  one model was toggled; this is a loading-state granularity question, not a
  data-integrity finding.

## M17

### M17-01

- `web/src/components/Tabs.vue:57` creates one id prefix per Tabs instance.
- `web/src/components/Tabs.vue:61` exposes `panelIdFor(key)` and
  `tabIdFor(key)` so a caller can pair its content container with the tab.
- `web/src/components/Tabs.vue:121-135` gives every tab an id, a tab role,
  `aria-selected`, `aria-controls`, and roving `tabindex`.
- `web/src/views/AccessControl.vue:78-83` correctly uses a template ref and
  binds the panel id, `role="tabpanel"`, and `aria-labelledby`.
- `web/src/views/UsageStats.vue:57-62` performs the same correct binding and
  adds a focusable panel.
- `web/src/views/Upstream.vue:90-92` mounts Tabs and then renders the current
  page component directly, with no panel binding.
- `web/src/views/Settings.vue:841-844`, `:891`, `:1095`, and `:1237` render
  four ordinary `.panel` branches with no id, tabpanel role, or labelling.
- `web/src/views/Wallet.vue:639-651` and `:840` render its tab panels the same
  way.
- Static inspection therefore establishes dangling `aria-controls` targets on
  all three pages. No browser or accessibility tree was run.
- R11 failure-first evidence: the source contract initially failed all three
  pages because none held a Tabs ref or rendered a matching tabpanel.
- R11 implementation evidence: all three callers now bind the active panel's
  id and `aria-labelledby` through the Tabs instance helpers, with
  `role="tabpanel"` and a focusable wrapper.

### M17-02

- `web/src/components/ModelPicker.vue:35-39` specifies that filtering is
  presentation-only and must not alter the selection.
- `web/src/components/ModelPicker.vue:48-52` derives `visible` solely from the
  current query.
- `web/src/components/ModelPicker.vue:79-81` performs
  `q ? [...visible.value] : [...sortedOptions.value]`, replacing the entire
  selection instead of combining it with the hidden selected models.
- `web/src/components/ModelPicker.vue:103` labels the filtered action
  “全选当前”, while `:134-136` states that checked state is unaffected by
  filtering.
- `web/src/views/Keys.vue:734` and `:819` bind the component directly to key
  create/edit selections; `web/src/views/UsersGroups.vue:950` binds it to
  group membership. A changed component value is therefore submitted as the
  key/group model whitelist.
- The backend treats a shorter or empty whitelist as a tighter permission
  scope (`DESIGN.md:728-731`), so the silent removal changes authorization.
- R10 failure-first evidence: the integration contract initially failed
  because `selectAll()` still emitted only `[...visible.value]` under a query.
- R10 implementation evidence: `web/src/modelSelection.ts` exposes a pure
  deduplicated union helper, and filtered select-all now combines existing
  selections with visible models.

### M17-03

- `web/src/components/HeartbeatBar.vue:193-201` builds a text alternative
  containing only the visible bucket count, the number with data, and the
  truncated count.
- `web/src/components/HeartbeatBar.vue:303-308` can produce a detailed string
  with timestamp, success rate, `ok/count`, and latency.
- `web/src/components/HeartbeatBar.vue:311-320` obtains the selected bucket
  from pointer coordinates in a mouse event.
- `web/src/components/HeartbeatBar.vue:416-419` exposes the canvas as
  `role="img"` with only `mousemove` and `mouseleave` handlers; it has no
  `tabindex` and no keyboard handlers.
- `web/src/components/HeartbeatBar.vue:436-443` renders the detailed tooltip
  only when `tip.show` is true; no focusable element owns or describes it.
- R11 failure-first evidence: the accessibility contract initially failed
  because HeartbeatBar had no focusable bucket rows, no keyboard handler, and
  no per-bucket text list.
- R11 implementation evidence: `heartbeatText.ts` builds the time/rate/count/
  latency summary, and HeartbeatBar renders those summaries as a focusable
  `role="list"` with roving Arrow/Home/End navigation.

### M17 Cleared Candidates

- `web/src/components/AppModal.vue` has a reference-counted body lock,
  inert/non-interactive background handling, Escape close, Tab trapping,
  nested-modal ordering, and focus restoration. The inspected path is
  internally consistent.
- `web/src/components/DataTable.vue` distinguishes loading, error, empty, and
  populated states; the inspected callers do not expose stale slot content
  during an initial error.
- `web/src/components/UsageByGroup.vue` increments a request sequence before
  every load and applies a response only when it is still current. Error
  handling clears rows and displays an explicit error.
- `web/src/components/UpstreamModelPanel.vue` correctly disables deletion for
  the route head and explains `route_count`; this matches the route contract.
- `web/src/components/Tabs.vue` measures the selected indicator, observes
  resizing, supports ArrowLeft/ArrowRight/Home/End, and does not crash on an
  empty tab list.
- `web/src/components/CredentialPanel.vue` can display a stale cross-provider
  balance, but that root cause is already recorded as M16-01 and is not
  duplicated here.

### M17 Open Questions

- `web/src/components/UsageByGroup.vue:68-75` intentionally uses `count` as
  the success-rate denominator, so canceled requests reduce the displayed
  rate. The rationale is documented, but the product metric is not otherwise
  specified.
- `web/src/components/UsageByGroup.vue:181-184` renders its dynamic error in
  an ordinary `<div>` rather than a live region or `role="alert"`. Whether
  screen-reader announcement is required is not established.
- `web/src/views/UsageDetail.vue:22` keeps the selected history row when
  `load()` reloads a different filter, page, or day range; route changes on
  this page are not confirmed to leave a misleading open drawer in normal
  navigation.
- If an AppModal close control is unmounted simultaneously with a nested
  confirmation close, the saved focus-restoration target may no longer exist.
  Browser behavior for that edge case was not established.
- None of the inspected shared components has component-level test coverage,
  especially Tabs ARIA pairing, ModelPicker selection merging, Modal focus
  management, and HeartbeatBar keyboard access.
- If the probe backend can return a bucket with `count=0`,
  `web/src/components/HeartbeatBar.vue:110` computes `ok / count` as `NaN`.
  No backend path producing such a bucket was confirmed.

## M18

### M18-01

- `.github/workflows/docker.yml:6-10` enables both version-tag pushes and
  `workflow_dispatch`.
- `.github/workflows/docker.yml:120-121` checks the version tag only when
  `github.ref_type == 'tag'`, so a manual branch dispatch has no version guard.
- `.github/workflows/docker.yml:155-158` always enables
  `type=raw,value=${{ steps.ver.outputs.version }}`; only `latest` and `edge`
  are conditional on the ref type.
- `.github/workflows/docker.yml:175-176` sets `push: true` and pushes the
  generated tags. A branch dispatch can therefore replace the version tag that
  points at the released commit.
- The release documentation treats `web/package.json` as the version source of
  truth and documents pinning image versions. It does not define manual
  dispatch as permission to reuse a released version for a different commit.

### M18-02

- `gateway.ps1:244-251` probes `http://127.0.0.1:$Port/admin/` and prints
  `[OK] 健康检查通过` for any HTTP response.
- `DOCKER.md:400-404` states that `/admin/` only reads embedded static assets
  and can return 200 while the database write pool is stuck; it documents
  `/healthz` as the endpoint that checks both DB pools and runtime freshness.
- `Dockerfile:80-91` uses `/healthz` for the container health check, so the
  local launcher and the container health contract disagree.

### M18-03

- `gateway.ps1:70-78` resolves a listener by port and returns its owning PID
  with no executable-name or path check.
- `gateway.ps1:94-105` stops processes by the stale pid file and by the port
  fallback using `Stop-Process -Force`.
- `gateway.ps1:201-205` performs the same unconditional port-owner kill before
  the normal startup path.
- The script's own main stop path at `gateway.ps1:84-92` does verify that the
  process path equals `$Bin`; that check is absent from both fallback paths.

### M18-04

- `DOCKER.md:158-159` warns that a missing single-file bind mount becomes a
  directory.
- `DOCKER.md:160-165` provides a first-run command that creates only
  `config.json`; it does not create valid `master.key` or `session_secret`
  files.
- `DOCKER.md:169-173` says both keys live under `/data` and should be mounted
  for fine-grained persistence.
- `DOCKER.md:195-199` mounts `config.json`, the database directory,
  `master.key`, and `session_secret` separately.
- `DOCKER.md:419-421` states that an empty or invalid session secret is
  rejected rather than regenerated, confirming that an empty placeholder is
  not a valid initialization step.
- `docker/docker-entrypoint.sh:29-40` seeds only `config.json`; key creation is
  left to the gateway after the invalid mount has already been created.

### M18 Cleared Candidates

- `web/sync-embed.mjs` replaces `internal/webui/dist` by deleting the old tree
  before copying the new build, so stale hashed assets are not retained.
- `internal/webui/embed.go`, `internal/api/api.go`, and
  `internal/webui/dist/index.html` agree on the embedded asset allowlist,
  content types, cache-control policy, and SPA fallback behavior.
- `internal/webui/csp_test.go` and the external `theme-init.js` keep the CSP
  free of inline-script exceptions.
- `internal/webui/version_test.go` and `lockfile_version_test.go` verify the
  version contract; `internal/api/healthz.go` pings the database pools and
  reports runtime dirtiness without exposing underlying errors.
- `Dockerfile:22-50` pins the Go and Alpine base images and matches the Go
  module/toolchain version. The entrypoint's non-root `su-exec` path and
  `/data` ownership checks are internally consistent.
- `ci.yml` rebuilds the frontend, checks the embedded-dist diff, runs the Go
  and race gates, and the Docker workflow repeats the release-critical gates.
- `.dockerignore` excludes local build, test, and secret artifacts from the
  image context.

### M18 Open Questions

- The container health check always probes `127.0.0.1`. `DOCKER.md` tells
  container operators to keep `listen` on `0.0.0.0`, so this is correct for
  the documented deployment; whether a supported concrete non-loopback bind
  address should also be considered healthy is not established.
- `DOCKER.md:186-190` describes a 24-second graceful-shutdown budget, while
  `Serve` can exceed that under the M01 probe/usage shutdown paths. The
  substantive lifecycle defects are already recorded in M01; the numeric
  documentation drift is left for M20 rather than counted again here.

## M19

### M19-01

- `.github/workflows/ci.yml:160-164` says `-short` skips the perf, leak, and
  stress tests that have `testing.Short()` guards, and runs
  `go test -short -count=1 -timeout 300s ./...`.
- `.github/workflows/ci.yml:190-193` repeats the same selection under `-race`
  with a 900-second timeout; `.github/workflows/ci.yml:197-207` creates the
  `nightly-long` job and asserts that the default job has excluded the slow
  tests through `-short`.
- `.github/workflows/docker.yml:105-110` deliberately uses the same `-short`
  commands for the release gate; both the 300-second default job and the
  900-second race job therefore repeat the same slow workloads.
- `cmd/gateway/perf_test.go` contains 13 `TestPerf*` functions. The guards are
  at `:456-457`, `:541-542`, `:669-670`, `:1196-1197`, and `:1356-1357`.
- `cmd/gateway/perf_test.go:730-743` has no short guard and runs 64 concurrent
  data-plane requests. `:778-786` and `:819-827` measure goroutine leaks;
  `:869-888` runs 20 concurrent requests over 300 total connections;
  `:937-951` performs 100 disconnect rounds; `:1010-1051` runs 1,000 and then
  3,000 requests with repeated GC settling; `:1104-1133` waits on a fixed
  150 ms TTFT budget and then runs 50 concurrent requests;
  `:1298-1320` deliberately sends 1,524 requests through a 1,024-entry usage
  queue; `:1298` and `:1010` add fixed sleep/GC behavior through `settle()`.
- `internal/store/stress_billing_test.go` contains 16 tests but only the
  `:564` guard. `:348-353`, `:387-392`, and `:571-577` execute 10,000
  transactions each; `:832-840` runs 100 concurrent workers through the full
  charge lifecycle. These are included by the same `./...` command.
- The exact impact proven here is test selection, not an observed CI failure.
  No test command was executed during this audit; the finding is that the
  documented fast/slow boundary and the actual CI workload differ.

### M19 Cleared Candidates

- The default and race jobs intentionally use the same test set; their timeout
  difference is consistent with the added cost of race instrumentation.
- The nightly job's `1800s` timeout is longer than the default `300s`, so the
  timeout values themselves do not contradict the intended separation.
- `internal/store/stress_billing_test.go:563-566` is a valid example of a
  load test that is actually excluded by `-short`; the defect is the large
  unguarded remainder, not the absence of the short mechanism.
- The three frontend test files are real contract checks, not placeholder
  tests: `web/tests/apiContract.test.mjs`, `web/tests/dtoContract.test.mjs`,
  and `web/tests/routeTrace.test.mjs` contain assertions over generated
  contracts and trace behavior.

### M19 Open Questions

- `internal/gatewayapp`, `internal/ingress`, `internal/usage`, and
  `internal/catalog` have little or no package-local coverage. Related gaps
  were recorded under M02, M03, and M09; they are not counted again here
  because this module did not establish a new distinct bug.
- The frontend still has only `apiContract`, `dtoContract`, and `routeTrace`
  test files and no page- or component-level tests. The absence of tests is
  recorded in M15 and M17 rather than duplicated as a new finding.
- `internal/server/authz_matrix_test.go:96` invokes a nested `go test` to
  export routes. The outer `-race` flag does not automatically make the nested
  command use `-race`. No reachable race in the export path was established.
- `cmd/gateway/perf_test.go` has several fixed timing assertions with explicit
  slack. Whether those assertions are acceptable on the project's supported
  CI runners is an operational policy decision, not provable from source
  alone.
- The `-short` behavior of tests outside the inspected perf/stress files was
  not exhaustively mapped. The finding is limited to the specific unguarded
  test functions and workflow commands cited above.

## M20

### M20-01

- `DOCKER.md:186-190` says the graceful-shutdown total budget is 24 seconds
  and enumerates prune 5s, recompute 2s, probe 2s, HTTP 10s, and usage 5s.
- `internal/gatewayapp/serve.go:82-89` calls `waitLoop` for prune 5s,
  recompute 2s, reconciliation 2s, and probes 2s before the HTTP and usage
  waits.
- The code therefore permits a nominal sequential wait of 26 seconds before
  the M01 overrun paths are considered. Those overrun paths are counted in
  M01-03 and M01-04, not again here.
- The Compose example at `DOCKER.md:190` supplies a 30-second
  `stop_grace_period`, so the documented example currently leaves four
  seconds of nominal margin. The defect is the incorrect repeated budget,
  not an observed production timeout.

### M20-02

- `README.md:26` says per-request usage details, including frozen cost, are
  retained for 30 days.
- `DOCKER.md:308` says already-archived rows came from details retained for
  30 days.
- `DESIGN.md:345` and `:1900` describe the detail/archive boundary and
  request-id visibility as 30 days.
- `internal/config/config.go:95-114` defines `usage_retention_days`; `:375-376`
  defaults it to 30; `:495-501` validates the range 1 through 3650.
- `internal/gatewayapp/serve.go:158` passes `cfg.UsageRetentionDays` to
  `PruneOldUsage`, proving the configured value controls the cutoff.
- `DESIGN.md:1445-1454` correctly calls 30 days the default and documents
  the configurable range, so the stale statements are internally
  inconsistent with the detailed contract.

### M20-03

- `DOCKER.md:281-291` lists three timezone methods in descending precedence:
  container `TZ`, `config.json.timezone`, then a host process `TZ`.
- `internal/config/timezone.go:60-67` returns the process location when the
  configured name is empty, so `TZ` remains effective only in that case.
- `internal/config/timezone.go:68-93` loads a non-empty configured location
  and assigns it to both `TZ` and `time.Local`.
- `internal/gatewayapp/run.go:75-81` applies that configuration on every
  startup.
- `docker/docker-entrypoint.sh:34-35` explicitly says a non-empty config
  timezone is applied again and is the final source of truth, contradicting
  the deployment guide.

### M20-04

- `DESIGN.md:1901` states as a design fact that `-race` is unavailable
  locally because the Windows machine has no gcc.
- `CONTRIBUTING.md:14-37` documents that the issue is the space in the
  default MinGW path, provides a space-free `CC` path, and explains the
  quoted alternative when that path is absent.
- `.github/workflows/ci.yml:177-188` contains the corresponding race-test
  step and local Windows path setup.
- The exact current syntax and invocation are already covered in
  `CONTRIBUTING.md`; this finding is limited to the stale design-document
  claim, not a claim that every machine necessarily has a working compiler.

### M20 Cleared Candidates

- The DOCKER container-health description now agrees with
  `internal/api/healthz.go`: `/admin/` is not a health endpoint and `/healthz`
  checks the database and runtime state.
- The documented default timezone `Asia/Shanghai` agrees with
  `docker/docker-entrypoint.sh:36-38`; the confirmed defect is precedence,
  not the default value.
- The documented 30-day recent-performance window is a separate fixed query
  window from the configurable usage-detail retention period. It was not
  counted as stale documentation.
- `DESIGN.md` correctly states that bootstrap provider/route inputs apply only
  when the database is empty; the implementation defect of partial writes is
  counted once under M02-04.
- `DESIGN.md` correctly documents the intended `api_key`-before-`api_key_env`
  precedence; the implementation defect is counted once under M02-01.
- The Compose example's 30-second grace period itself is consistent with the
  documented intent to provide more than the nominal shutdown waits. The
  counting mistake is the separate M20-01 finding.
- The repository contains no tracked license file; the OCI metadata
  contradiction is counted once under M00-03.
- The Docker version-tag overwrite risk is counted once under M18-01; the
  release documentation's general version-pinning guidance does not add a
  second defect.

### M20 Open Questions

- The audit did not execute the documented shutdown sequence, so it did not
  measure actual elapsed shutdown time or whether a signal arrives during a
  specific maintenance wait. M20-01 is a static arithmetic mismatch, while
  the unbounded customization paths under M01 remain the substantive
  lifecycle findings.
- Whether `DESIGN.md` should retain a historical note about the pre-fix
  Windows gcc setup, rather than replace it, is an editorial decision.
  M20-04 only establishes that the current statement is false as a present
  capability claim.
- The audit compared the material README, DESIGN, DOCKER, and CONTRIBUTING
  statements against the inspected implementation and did not exhaustively
  check every historical date, example, or release note in those documents.
- No documentation build or link checker was available in the proven
  read-only command set, so anchor validity and rendered formatting were not
  tested.

## R0-A / M08-01 Implementation Evidence

Recorded on 2026-10-25. R0-A containment is verified at the store layer. This
does not close the final accounting state-machine work assigned to R3.

### Baseline audit evidence

- `internal/store/balance_dao.go:782-901` is the single top-up settlement
  transaction. It reads `balance_pending`, debits the wallet, and deletes or
  reduces pending rows. Before R0-A it did not insert the original
  `(request_id, user_id)` row into `balance_charges`.
- `internal/store/usage_dao.go:669-714` keeps the recompute-specific
  `recompute:<record_id>:<cost_micros>` key for delta idempotency. Before
  R0-A it did not insert the original request marker either.
- `internal/store/usage_dao.go:480-543` makes reconciliation depend on the
  original `(request_id, user_id)` marker. Consequently, either recovery path
  could be charged again on the next maintenance pass.
- `internal/store/schema.go:468-474` already defines the canonical marker
  table and primary key. R0-A requires no schema change.

### Fixtures added

- `internal/store/balance_settle_test.go`
  - `TestR0AC01_PendingSettlementCreatesCanonicalCompletionMarker`
    exercises insufficient charge -> pending -> top-up settlement ->
    reconciliation.
  - `TestR0AC02_RecomputeCreatesCanonicalCompletionMarker`
    exercises cost-pending -> successful recompute -> reconciliation while
    retaining the recompute-specific marker.
  - `TestR0AC03_CompletionMarkerFailureRollsBackAssociatedDebit`
    injects a marker-insert failure for both settlement and recomputation.
  - `TestR0AC04_SettlementAndReconciliationRemainIdempotent`
    races settlement against reconciliation, then races two reconciliation
    passes, and checks one debit and one original marker.

### Implementation under review

- `internal/store/balance_dao.go`: `settlePendingForUser` now calls the new
  `ensureUsageCompletionTx` for every fully settled pending row before deleting
  it. Partially settled rows keep their remaining `balance_pending` amount and
  no completion marker. The full write-off branch also marks rows before
  deleting them. Marker insertion, wallet update, pending changes, and top-up
  ledger write share the existing `AdjustBalanceWithLedger` transaction.
- `internal/store/usage_dao.go`: `RecomputeCost` inserts the original marker
  after a successful delta debit and before commit, while retaining the
  recompute-specific idempotency key.
- `internal/store/usage_dao.go`: reconciliation excludes any usage with an
  existing `balance_pending` row. This closes the reachable race where
  reconciliation debits first and top-up settlement would otherwise debit the
  same pending amount again.
- No schema, migration, new ledger, or additional accounting table was added.

### Verification status

Verified on 2026-10-25 with the following commands and outputs:

```text
PS C:\Users\louis\Desktop\project\rosetta-gateway> go test ./internal/store -run 'TestR0AC0[1-4]' -count=1
ok      github.com/cn-maul/rosetta-gateway/internal/store       0.214s

PS C:\Users\louis\Desktop\project\rosetta-gateway> go test ./internal/store -count=1
ok      github.com/cn-maul/rosetta-gateway/internal/store       17.069s
```

- `R0-AC-01` through `R0-AC-04` pass.
- The complete `internal/store` package suite passes.
- Result: R0-A containment verified.
- Remaining owner: R3 still owns the final accounting state machine, durable
  recovery model, and any schema migration. R0-A is a no-schema compatibility
  containment and must not be reported as completion of R3.

## R0-B / M10-01 Implementation Evidence

Recorded on 2026-10-25. R0-B is a login-local identity containment; R8 still
owns the authoritative principal identity model.

### Baseline reproduction

`TestR0AC05_LoginAccountThrottleUsesCanonicalCaseAcrossIPs` sends five failed
logins for one account while alternating `alice` / `Alice` / `ALICE` and
changing the source IP. Before the fix:

```text
scope_test.go:753: attempt 6 ("Alice") status = 401, want 429:
case variants must share one account failure budget
```

`TestR0AC06_SuccessfulLoginClearsCanonicalAccountBudget` also failed before the
fix: a successful `DAVE` login did not clear failures recorded under `dave`,
and the following attempt returned 429.

### Implementation

- `internal/admin/user_handler.go` computes
  `canonicalLoginThrottleKey(strings.TrimSpace(...))` once after JSON decode.
- The helper folds only ASCII `A-Z` to `a-z`, matching SQLite `NOCASE`; it does
  not use Unicode case folding.
- `GetUserByUsername`, account-throttle `Allow`/`RetryAfter`/`Release`,
  `Fail`, and `Success` all receive that one canonical key.
- The IP-dimension throttle remains keyed by `ClientIPOf(r)` and is unchanged.

### Verification

```text
PS> go test -count=1 ./internal/admin -run 'TestR0AC0[56]' -v
PASS
ok github.com/cn-maul/rosetta-gateway/internal/admin 0.497s

PS> go test -count=1 ./internal/admin -run 'TestLogin_AccountDimensionThrottle|TestLogin_NoUserEnumeration|TestLogin_BootstrapAccountCannotLogin' -v
PASS
ok github.com/cn-maul/rosetta-gateway/internal/admin 0.490s

PS> go test -count=1 ./internal/admin
ok github.com/cn-maul/rosetta-gateway/internal/admin 7.333s
```

- `R0-AC-05` and `R0-AC-06` pass.
- Remaining owner: R8 replaces this helper with the principal canonical identity
  contract and must retain the cross-IP regression coverage.

## R0-C / M01-02 Implementation Evidence

Recorded on 2026-10-25. R0-C is a startup-safety containment; R1 still owns
the complete lifecycle readiness model.

### Baseline reproduction

A throwaway worktree checked out at baseline `e43c266` used SQLite triggers and
a closed store to inject bootstrap failures. All three cases failed the new
expectation because `initializeAdminBootstrap` returned nil:

```text
TestR0CBaseline_UnreadableStateIsNotFatal
  ERROR bootstrap admin: cannot count users; skipping
  baseline returned success after bootstrap state became unreadable

TestR0CBaseline_NonUniqueCreateFailureIsNotFatal
  ERROR bootstrap admin: create failed ... simulated disk failure
  baseline returned success after non-unique initial admin creation failed

TestR0CBaseline_UnverifiedUniqueConflictIsNotFatal
  INFO bootstrap admin skipped: created concurrently by another instance
  baseline accepted an unverified unique conflict as successful bootstrap
```

The throwaway worktree was removed with `git worktree remove` after capturing
the output; no baseline files remain in the working tree.

### Implementation

- `cmd/gateway/bootstrap_admin.go`
  - Introduces explicit `setup_required`, `ready`, and `failed` outcomes.
  - `CountUsers` failure is fatal.
  - Non-unique `CreateUser` failure is fatal.
  - Unique conflict triggers a re-read of the count and administrator; the
    conflict is accepted only when the expected admin exists.
  - A post-create `FindUninitializedAdmin` failure is fatal.
  - `initializeAdminBootstrap` propagates the error and logs a derived outcome.
- `internal/gatewayapp/run.go`
  - A post-bootstrap `FindUninitializedAdmin` read failure now returns an error
    before HTTP serving can begin.

### Verification

```text
PS> go test -count=1 ./cmd/gateway -run 'TestEnsureBootstrapAdmin|TestR0AC0[789]|TestInitializeAdminBootstrap' -v
PASS
ok github.com/cn-maul/rosetta-gateway/cmd/gateway 0.170s

PS> go test -count=1 ./cmd/gateway
ok github.com/cn-maul/rosetta-gateway/cmd/gateway 72.346s

PS> go test -count=1 ./internal/gatewayapp
ok github.com/cn-maul/rosetta-gateway/internal/gatewayapp 0.035s
```

- `R0-AC-07`, `R0-AC-08`, and `R0-AC-09` pass, including the unverified
  unique-conflict counterexample.
- Remaining owner: R1 must project the typed bootstrap outcome into lifecycle
  readiness rather than treating this startup guard as the final model.

## R0-D / M01-01 Implementation Evidence

Recorded on 2026-10-25. R0-D reuses the existing dirty/retry and health
projection; R1 still owns the final typed lifecycle state.

### Baseline reproduction

The same test copied into a throwaway worktree at baseline `e43c266` failed:

```text
reloader_test.go:49: failed initial-style reload did not mark runtime dirty
```

The fixture renames `app_settings` to force the snapshot stage to fail after
the upstream-pool stage succeeds, then restores the table for the retry.

### Implementation

- `internal/runtime/reloader.go`
  - A `PrepareFromStore` failure calls `MarkDirty` before returning.
  - A `RebuildFromDB` failure calls `MarkDirty` before returning.
  - A successful `Reload` continues to call `markClean`.
- No second health model was introduced. `WatchRetry`, `IsDirty`, and
  `/healthz` retain their existing contract.

### Verification

```text
PS> go test -count=1 ./internal/runtime -run TestR0AC10 -v
PASS
ok github.com/cn-maul/rosetta-gateway/internal/runtime 0.057s

PS> go test -count=1 ./internal/runtime ./internal/gatewayapp ./internal/api
ok github.com/cn-maul/rosetta-gateway/internal/runtime 0.065s
ok github.com/cn-maul/rosetta-gateway/internal/gatewayapp 0.035s
?  github.com/cn-maul/rosetta-gateway/internal/api [no test files]
```

- `R0-AC-10`: after a failed rebuild, `/healthz` returns 503 with
  `runtime_lags_behind_database`.
- `R0-AC-11`: after a successful retry, the dirty bit clears and `/healthz`
  returns 200 with `status: ok`.
- Remaining owner: R1 replaces the dirty-bit projection with typed lifecycle
  readiness.

## R0-E / M05-01 Implementation Evidence

Recorded on 2026-10-25. R0-E is a non-stream commitment containment; R5 still
owns the final requestlane commitment and accounting handoff contract.

### Baseline reproduction

The new outwire fixtures did not compile against the baseline writer API
because `NonStreamWriteResult` did not exist and the three writers returned no
result. The end-to-end Chat fixture reproduced the full false-success:

```text
usage.status = "ok", want canceled
usage.error_code = "", want downstream_write_failed
usage.cost_total = 3, want 0
charged = 3, want 0
balance_charges rows = 1, want 0
```

The Responses and Anthropic subcases also recorded `ok` instead of
`canceled`; Anthropic logged the encode failure and returned as if complete.

### Implementation under review

- `internal/outwire/nonstream_write.go`
  - Defines `NonStreamWriteResult{Committed, Err}` and `EncodeError()`.
  - Marshals the complete payload before setting headers or calling
    `WriteHeader`.
  - Converts a zero-error short body write into a non-nil error.
- `internal/outwire/openai_chat.go`,
  `internal/outwire/openai_responses.go`, and
  `internal/outwire/anthropic.go`
  - Return the typed result for every non-stream response path.
- `internal/ingress/codec.go`
  - Carries the typed result across the protocol-independent boundary.
- `internal/dataplane/handler.go`
  - A committed write error records `canceled` /
    `downstream_write_failed`, keeps upstream token facts, commits only the
    known token/quota facts, and returns a terminal outcome with no upstream
    penalty.
  - A pre-commit encode failure renders the protocol-native HTTP 500 envelope
    with `response_encode_failed`; it is also terminal and non-billable.

### Fixtures added

- `internal/outwire/nonstream_write_result_test.go`
  - `TestR0AC12_NonStreamWritersReportWriteFailure`
  - `TestR0AC12_NonStreamWritersDoNotReportSuccessBeforeBodyIsWritten`
  - `TestR0AC12_EncodeFailureIsNotReportedAsCommitted`
- `cmd/gateway/nonstream_write_failure_test.go`
  - `TestR0AC13_NonStreamDownstreamWriteFailureIsTerminalAndNotCharged`
    covers Chat, Responses, and Anthropic ingress.
  - `TestR0AC14_NonStreamWriteFailureDoesNotPenalizeTargetOrCredential`
    checks three terminal write failures with threshold 1, then verifies the
    target remains available and the credential remains selectable.

### Verification

```text
PS> go test -count=1 ./internal/outwire -run 'TestR0AC12' -v
PASS
ok github.com/cn-maul/rosetta-gateway/internal/outwire 0.018s

PS> go test -count=1 ./cmd/gateway -run 'TestR0AC1[34]|TestProtocolMatrix_AnyIngressToAnyUpstream' -v
PASS
ok github.com/cn-maul/rosetta-gateway/cmd/gateway 0.297s

PS> go test -count=1 ./internal/outwire ./internal/dataplane ./cmd/gateway
ok github.com/cn-maul/rosetta-gateway/internal/outwire 0.052s
ok github.com/cn-maul/rosetta-gateway/internal/dataplane 0.120s
ok github.com/cn-maul/rosetta-gateway/cmd/gateway 72.847s
```

- `R0-AC-12` through `R0-AC-14` pass.
- `R0-AC-15` passes via the unchanged 3x3 non-stream protocol matrix.
- Remaining owner: R5 replaces this local result/classifier with the
  requestlane commitment model. R3 remains responsible for the final
  accounting state machine and retention-aware settlement behavior.

## R2 / Effective Configuration Implementation Evidence

Recorded on 2026-10-25.

### Baseline reproductions

- `M02-01`: both bootstrap paths assigned `api_key = os.Getenv(api_key_env)`
  whenever the environment field was non-empty, erasing a configured key when
  the named variable was unset.
- `M02-02`: `RebuildFromDB` consumed model defaults only in the `err == nil`
  branch, leaving both snapshot capacity fields at zero on corrupt JSON.
- `M02-03`: a `probes` object containing only `interval_sec` left
  `enabled=false`, so the probe loop exited despite the documented default.
- `M09-02`: `audit_log_keep_rows=0` was normalized to 20000 during defaulting
  even though the field contract says zero disables pruning.

### Fixes and fixtures

- `internal/config/config.go`
  - Added `BootstrapCredential.ResolveAPIKey`: explicit `api_key` wins;
    `api_key_env` is consulted only when the explicit key is empty.
  - Added presence tracking for `audit_log_keep_rows`; only an absent field is
    defaulted to 20000, while explicit 0 and -1 disable pruning.
  - Added presence tracking for `probes.enabled`; a partial probes object now
    still defaults probing on.
- `internal/gatewayapp/bootstrap.go`, `internal/upstream/upstream.go`
  - Use the shared credential resolver in both bootstrap and runtime fallback.
- `internal/snapshot/rebuild.go`
  - Keeps the documented positive fallback values from `GetModelDefaults` and
    returns the decode error so convergence is visibly degraded rather than
    publishing zero capacity.
- `internal/gatewayapp/serve.go`
  - Explicitly skips audit pruning for both 0 and -1.
- Fixtures:
  - `internal/config/config_test.go` covers all credential precedence
    combinations and checks the deployment-document contracts.
  - `internal/config/defaults_consistency_test.go` covers partial probes and
    explicit zero retention.
  - `internal/gatewayapp/bootstrap_test.go` verifies the database bootstrap
    stores an explicit key when its environment variable is unset.
  - `internal/upstream/pool_test.go` verifies the same precedence in the
    config-only runtime pool.
  - `internal/snapshot/rebuild_test.go` verifies corrupt model defaults fail
    the rebuild while the store fallback remains positive.

### Verification

```text
PS> go test -count=1 ./internal/config ./internal/upstream ./internal/snapshot ./internal/gatewayapp
ok github.com/cn-maul/rosetta-gateway/internal/config
ok github.com/cn-maul/rosetta-gateway/internal/upstream
ok github.com/cn-maul/rosetta-gateway/internal/snapshot
ok github.com/cn-maul/rosetta-gateway/internal/gatewayapp

PS> go test -count=1 ./internal/config ./internal/upstream ./internal/snapshot ./internal/gatewayapp ./internal/store ./cmd/gateway
ok github.com/cn-maul/rosetta-gateway/internal/config
ok github.com/cn-maul/rosetta-gateway/internal/upstream
ok github.com/cn-maul/rosetta-gateway/internal/snapshot
ok github.com/cn-maul/rosetta-gateway/internal/gatewayapp
ok github.com/cn-maul/rosetta-gateway/internal/store
ok github.com/cn-maul/rosetta-gateway/cmd/gateway
```

### Documentation alignment

- `README.md`: 30 days is now described as the default detail-retention period
  controlled by `usage_retention_days`.
- `DESIGN.md`: detail and archive boundaries use configurable retention; audit
  pruning documents both 0 and -1 as permanent retention; partial probes are
  described as defaulting to enabled.
- `DOCKER.md`: non-empty config `timezone` is documented as the final override
  and container/process `TZ` as the fallback. Archive wording no longer implies
  a fixed 30-day cutoff.


## R1 / Lifecycle Ownership Implementation Evidence

Recorded on 2026-10-25. R1 replaces the R0 lifecycle containment adapters.

### Failure-first evidence

- `internal/gatewayapp/serve_test.go`
  - `TestR1_ListenFailureDoesNotStartWorkersOrPublishReady` occupies the
    configured listener and proves `Serve` returns before phase `ready` or
    worker registration.
  - `TestR1_NormalShutdownJoinsEveryRegisteredWorker` proves each started
    worker decrements the lifecycle count and the join barrier is reachable.
  - `TestR1_SlowUsageWriterKeepsJoinBarrierClosed` blocks the writer inside a
    DB-like call and proves both `Wait` and `Join` reject a premature drain.
- `internal/lifecycle/readiness_test.go`
  - The join-barrier test prevents `MarkWorkersJoined` from masking a live
    worker count.
- `internal/probe/runner_cancel_test.go`
  - Cancellation during the stagger exits within 500ms and leaves zero probe
    rows.

### Implementation

- `internal/lifecycle/readiness.go`
  - Typed phases and bootstrap states.
  - Independent database/runtime/bootstrap readiness dimensions.
  - `AddWorkers`/`WorkerExited`/`PendingWorkers`/`WorkersJoined`.
- `internal/gatewayapp/serve.go`
  - Listener bind precedes any maintenance goroutine or `ready` publication.
  - Only enabled probe workers register.
  - Abort bound is real; `ErrWorkersNotJoined` propagates when exit is not
    observed.
- `internal/gatewayapp/run.go`
  - The reload retry and recorder are lifecycle-owned workers.
  - After `Serve`, `Run` joins the reload retry and usage queue; store close is
    conditional on a proven barrier.
- `internal/usage/recorder.go`
  - Closed admission never starts a new synchronous write.
  - Every abandoned callback executes; only log sampling is capped.
  - `Wait` and `Join` expose exit evidence to lifecycle callers.
- `internal/probe/runner.go`
  - Context-aware stagger and cancellation checks before probe persistence.

### Verification

```text
PS> go test -count=1 ./internal/lifecycle ./internal/api ./internal/gatewayapp ./internal/probe ./internal/usage
PASS

PS> go test -race -count=1 ./internal/gatewayapp ./internal/lifecycle ./internal/usage ./internal/probe
PASS

PS> go test -count=1 ./cmd/gateway -run 'TestUsageShutdown|TestRecordAfterWait|TestRecordConcurrentWithWait|TestWaitDoesNotDeadlock'
PASS
```

- R1 acceptance: startup failure injection, registered-worker closure,
  slow-writer race, probe cancellation, usage join, and budget/document
  agreement are covered.

## R7

### M02-04

- `internal/gatewayapp/bootstrap.go` reads provider state with error
  propagation, validates the whole configured graph, and calls `Store.RunTx`
  for network, provider, credential, model, and route writes.
- The same transaction writes `config_bootstrap_completed`; no child failure
  or encryption failure can leave that marker committed.
- `verifyLegacyBootstrapGraph` accepts a complete graph from an older binary
  and refuses an incomplete one.
- Regression fixtures: `TestR7_BootstrapReadFailureDoesNotWritePartialGraph`,
  `TestR7_BootstrapChildFailureLeavesNoPartialGraph`,
  `TestR7_BootstrapCommitsCompleteGraphAndCompletionMarker`,
  `TestR7_ExistingGraphWithoutMarkerIsNotReportedComplete`,
  `TestR7_CompleteLegacyGraphIsVerifiedAndMarked`, and
  `TestR7_BootstrapEncryptFailureRollsBackEarlierWrites`.

### M12-02

- `config_transfer_cardinality_test.go` proves dry-run and import use the same
  provider-scoped model key when two new providers expose the same model ID.
- `TestR7_ModelCardinalityIsProviderScoped` covers provider rename/identity
  boundaries.

## R3

### M07-01

- `PruneOldUsage` excludes `cost_pending` rows and successful positive-cost
  rows that have neither a canonical charge marker nor a pending ledger row.
- The watermark check uses the same predicate, so excluded work cannot be
  declared pruned.
- Regression fixtures: `TestR3_M07_01PendingUsageSurvivesRetentionCutoff` and
  `TestR3_M07_01UnchargedUsageSurvivesRetentionCutoff`.

### M08-01

- Existing R0-A fixtures cover marker creation and transaction rollback:
  `TestR0AC01` through `TestR0AC04`.
- R3 retains that single marker contract while reconciliation reports typed
  outcomes and does not treat a deferred debt as a successful debit.

### M08-02

- `quota_reservation_leases` stores request-scoped key/user reservations,
  success state, lease deadline, and release error.
- `ReleaseOwnedQuotaReservations` releases the complete set in one
  transaction; `SweepExpiredQuotaReservations` recovers only provably expired
  owners.
- Missing durable lease writes fall back to a checked direct release.
- Regression fixtures:
  `TestR3_M08_02QuotaReleaseRetriesAfterTransientFailure`,
  `TestR3_M08_02ExpiredLeaseIsRecoverableWithoutRestart`, and
  `TestR3_M08_02ReleaseWithoutLeaseFallsBackToCheckedRelease`.

### M09-01

- `TestR3_M09_01AbandonedBatchReleasesEveryQuotaCallback` queues 25 jobs,
  forces persistent persistence failure, closes the queue, aborts the worker,
  and asserts exactly 25 callbacks.

### M14-01

- `usage_records.charge_attempts`, `charge_next_at`, and `charge_last_error`
  persist retry state. The scan orders by retry deadline and time, while
  `ReconcileUnchargedUsage` counts only successful debits as `Charged`.
- `TestR3_M14_01ReconciliationProgressesPastDeferredHead` proves five
  unaffordable head rows do not prevent a later chargeable row from being
  collected.

### M14-02

- `TestR3_M14_02ActualPricingUsesEffectiveFallback` stores a usage record for
  an input-only model and asserts the same effective output price used by
  preflight.

### Compatibility

- `TestSchema_AddsR3AccountingObjectsToExistingCurrentDatabase` removes the
  lease table and retry index from a current-version database, reopens it, and
  proves `ensureCompatibleSchema` restores both.
## R4 Protocol Model and Stream Lifecycle Evidence

Recorded on 2026-10-26.

- `internal/inwire/openai_chat.go` normalizes Chat text, image, `input_audio`,
  and `file` parts. `chatFileBlock` sets exactly one of `FileData` or `FileID`;
  `input_audio.data` and `file.file_data` lose a data-URL prefix before entering
  the normalized model.
- `internal/inwire/anthropic.go` converts a legal `document.source.type=text`
  source into a normalized text block. Unconvertible document sources remain an
  explicit invalid file block, so validation fails rather than dropping content.
- `internal/ingress/estimate.go` charges flat image/audio/file estimates plus
  inline audio/file payload size and checks signatures, sealed reasoning, file
  name, and MIME type. `RequestOutputCeiling` selects the client cap or the
  largest configured chain ceiling.
- `internal/dataplane/handler.go` resolves the route before admission estimates,
  shares one estimate across TPM and both lifetime quotas, and reserves TPM only
  after quota admission so quota rejection remains visible and non-consuming.
- `internal/inwire/openai_responses.go` derives the accepted tool list in the
  unsupported-tool diagnostic: function, custom, local_shell, namespace.
- `internal/outwire/openai_chat.go` only writes `content:null` when no text is
  present; non-empty text survives alongside tool calls.
- `internal/outwire/anthropic.go` and `internal/outwire/openai_responses.go`
  call `ensureStarted` before a successful terminal sequence.

Regression evidence:

```text
internal/inwire/r4_protocol_test.go
internal/ingress/estimate_test.go
internal/outwire/r4_stream_lifecycle_test.go
```

Relevant package tests and the complete `cmd/gateway` suite pass. The isolated
`TestE2E_D1b_QuotaRejectedRequestsDoNotConsumeTPMBudget` failure found during
the admission reorder was resolved by reserving TPM after quota checks; with an
omitted output cap, the actual configured chain ceiling (zero in that fixture)
is authoritative rather than an invented default.

## R5 Request Lane and Commitment Ownership Evidence

Recorded on 2026-10-26.

### Failure-first evidence

- `internal/dataplane/outcome_lane_test.go` initially failed to compile because
  `OutcomeKind`, typed terminal kinds, and `LaneDecision` did not exist.
- `TestR5_ClientCancellationWinsOverWrappedUpstreamError` failed before the
  final context-precedence fix: the persisted status remained `error` when the
  request context was canceled but the SDK surfaced an API error first.
- `TestE2E_Adv3_NonStreamClientDisconnectReleasesReservation` reproduced the
  same defect end to end. It now observes `canceled`, reservation release, and
  no target penalty.

### Implementation

- `internal/dataplane/outcome.go` defines the terminal kinds and the single
  `LaneDecision` projection. It derives usage status, billability, upstream
  penalty, diagnostic status, and route trace from the same outcome.
- `internal/dataplane/attempt_set.go` owns the reachable candidate slice and
  committed target/credential health update. Balance preflight and the attempt
  loop use the same slice.
- `internal/dataplane/handler.go` no longer branches on independent
  `Committed`/`Success`/`ClientGone` combinations for outer failover and final
  accounting. Stream terminal status is converted once to a typed kind.
- `Outcome.WithRequestContext` gives a proven client cancellation precedence
  over a wrapped upstream/transport error while leaving committed outcomes
  immutable.

### Verification

```text
PS> go test -count=1 ./internal/dataplane ./internal/ingress ./internal/inwire ./internal/outwire ./internal/usage ./internal/ratelimit -timeout 180s
PASS

PS> go test -count=1 ./internal/config ./internal/upstream ./internal/usage ./internal/gatewayapp ./cmd/gateway -timeout 420s
ok github.com/cn-maul/rosetta-gateway/internal/config
ok github.com/cn-maul/rosetta-gateway/internal/upstream
ok github.com/cn-maul/rosetta-gateway/internal/usage
ok github.com/cn-maul/rosetta-gateway/internal/gatewayapp
ok github.com/cn-maul/rosetta-gateway/cmd/gateway 73.495s
```

## R6 Transport Discovery and Balance Contracts Evidence

Recorded on 2026-10-26.

### Failure-first evidence

- `TestR6_OpenAIBalanceDoesNotInventZeroUsed` reproduced four independent
  `ok` responses with a precise `100.00 USD` amount when the usage request
  returned 503, malformed JSON, omitted `total_usage`, or timed out.
- `TestR6_AnthropicDiscoveryUsesResolvedProtocolAuth/auto` reproduced HTTP 401
  because discovery sent Bearer to an Anthropic-shaped endpoint. The explicit
  `anthropic` subtest used `x-api-key` and passed.

### Implementation

- `internal/upstream/balance.go` now distinguishes an unknown usage reading
  from a successful balance. The `balanceUnknownErr` path preserves the
  reason in `message`, sets `fetched_at`, and leaves `amount` empty.
- OpenAI only returns an amount after the subscription limit and a parsed,
  present `total_usage` value are both available.
- `internal/upstream/upstream.go` resolves the discovery protocol through the
  same `rosetta.DetectClient` path used for `auto` client construction before
  `fetchModels` chooses auth headers.
- `web/src/types.ts`, `web/src/components/CredentialPanel.vue`, and
  `web/src/styles.css` expose and style the `unknown` state without treating
  it as a successful number.

### Verification

```text
PS> go test -count=1 ./internal/upstream -timeout 180s
ok github.com/cn-maul/rosetta-gateway/internal/upstream

PS> go generate ./internal/admin
PS> cd web; npm test
pass 25
fail 0

PS> cd web; npm run typecheck
PASS
```

## R12 Delivery Contract Evidence

Recorded on 2026-10-26.

### Failure-first evidence

- `internal/delivery/contract_test.go` failed before the fixes across its ten
  delivery contracts: the CI job still contained its weaker inline grep, the
  release workflow omitted generated-contract/module checks, image metadata
  still claimed MIT, manual dispatch still enabled the raw version tag,
  `gateway.ps1` still probed `/admin/`, launcher stop paths still trusted a
  port owner or PID without proving process identity, the Compose example
  still mounted individual key files, eight `TestPerf*` and three stress tests
  still ran under `-short`, and `DESIGN.md` still said local race testing was
  unavailable.
- A live ownership probe started an unrelated PowerShell HTTP listener on port
  18670 and wrote its PID to the launcher PID file. `gateway.ps1 -Stop`
  reported the stale PID as "不是本项目启动器" and the port as "被无关进程占用";
  both the process and listener survived. This demonstrates that ownership is
  based on identity, not merely the port or PID number.

### Implementation

- `.github/workflows/ci.yml` invokes the existing repository hygiene script.
  `.gitattributes` locks the three ignore files to LF.
- `.github/workflows/docker.yml` regenerates defaults, routes, and DTO
  contracts, fails on their diff, runs `go mod tidy -diff` and
  `go mod verify`, restricts raw version/latest tags to tag refs, uses `edge`
  for manual branch dispatches, and no longer claims MIT.
- `gateway.ps1` probes `/healthz`, treats non-200 as failed readiness, and
  verifies executable paths and wrapper command lines before stopping
  anything. `gateway-launcher.ps1` owns the compiled
  `RosettaGatewayLauncher.ProcessOwnership` helper.
- `DOCKER.md` mounts `./rosetta:/data` as a whole, documents automatic key
  generation, and explicitly rejects empty key placeholders.
- `cmd/gateway/perf_test.go` and `internal/store/stress_billing_test.go` declare
  their load/timing workloads as outside the `-short` class.
- `DESIGN.md` points to `CONTRIBUTING.md` for the Windows `CC` setup and no
  longer asserts that race testing is unavailable.
- `internal/delivery/contract_test.go` makes the delivery decisions executable
  contracts rather than prose-only closure.

### Verification

```text
PS> go test -count=1 ./internal/delivery
ok github.com/cn-maul/rosetta-gateway/internal/delivery

PS> go test -short -count=1 -timeout 300s ./cmd/... ./internal/...
PASS

PS> $env:CC="C:/mingw64/bin/gcc.exe"
PS> go test -race -short -count=1 -timeout 900s ./cmd/... ./internal/...
ok github.com/cn-maul/rosetta-gateway/cmd/gateway
ok github.com/cn-maul/rosetta-gateway/internal/admin
ok github.com/cn-maul/rosetta-gateway/internal/store
PASS (all packages)

PS> go vet ./cmd/... ./internal/...
PASS

PS> go build ./cmd/... ./internal/...
PASS

PS> go generate ./internal/config; go generate ./cmd/gateway; go generate ./internal/admin
PS> git diff --exit-code -- web/src/generated/defaults.ts web/src/generated/routes.ts web/src/generated/dto-contract.ts
PS> go mod tidy -diff
PS> go mod verify
all modules verified

PS> pwsh -NoProfile -File scripts/check-repo-hygiene.ps1
PASS

PS> pwsh -NoProfile -File scripts/gateway-launcher-contract.ps1 -LauncherPath gateway-launcher.ps1
{"exactBinary":true,"differentPath":false,"wrapper":true,"unrelated":false}

PS> cd web; npm test
pass 41
fail 0

PS> cd web; npm run typecheck
PASS
```

The focused M19 verification showed `SKIP` for all eight newly guarded
`TestPerf*` functions and all three billing-stress functions under `-short`.
Docker is not installed in this environment, so the local image build and a
clean Compose startup could not be executed; that remains an external delivery
verification item, not an unrecorded code change.

### Ledger closure

`M00-01`, `M00-02`, `M00-03`, `M18-01`, `M18-02`, `M18-03`, `M18-04`,
`M19-01`, and `M20-04` are fixed by R12. `M20-01` (R1), `M20-02` (R2), and
`M20-03` (R2) retain their existing fixes and pass the R12 delivery contract.
The 54 confirmed findings are all fixed and carry evidence.

## R8 Identity and Authorization Evidence

Recorded on 2026-10-26.

### Failure-first evidence

- `TestR8_SuccessfulAttemptsLeaveNoIdleEntries` reproduced 6000 retained
  entries after successful `Allow -> Success -> Release` traffic.
- `TestR8_ReleaseBoundsIdleMap` reproduced 5000 retained entries, above the
  documented 4096-entry cap.
- `TestR8_UsernameCanonicalAtStoreBoundary` read back `"  Alice  "` from the
  store instead of the canonical `alice`.
- `TestR8_OwnerCannotRemoveAdminModelAllowlist` reproduced HTTP 200 when the
  key owner cleared or replaced an admin-applied allowlist.

### Implementation

- `internal/store/user_dao.go` owns `CanonicalUsername`; `CreateUser`,
  `GetUserByUsername`, admin account creation, and login throttle keying use
  that same function.
- `internal/guard/throttle.go` removes idle entries in `Release` and runs the
  bounded sweep when the map reaches its cap.
- `internal/admin/key_handler.go` applies `modelAllowlistCanTighten` in the
  owner no-loosening guard. Existing self-created restrictions stay
  narrowable; admins retain the explicit widening path.

### Verification

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

## R9 HTTP and Admin Edge Contract Evidence

Recorded on 2026-10-26.

### Failure-first evidence

- Organization endpoints returned 200 with the default 30-day window for
  explicit `start_time` / `end_time` values of `abc`, `0`, or `-1`.
- Admin body overflow reached handler-level JSON error mapping instead of the
  shared decoder emitting 413.
- `GET /admin/api/audit?limit=abc` returned 200 with the default page.
- Group PATCH with `""` or whitespace persisted a blank name.
- Frontend contract coverage demonstrated the prior login 401 rewrite,
  bootstrap-follow-up ambiguity, and missing bootstrap degraded state.

### Implementation

- `internal/billinghttp/billinghttp.go` validates explicit organization time
  values before applying the 30-day fallback.
- `internal/admin/helpers.go` maps `http.MaxBytesError` to 413;
  `audit_handler.go` rejects malformed limits; `group_handler.go` rejects blank
  PATCH names.
- `web/src/api.ts` separates public authentication requests from protected
  session handling and exposes `bootstrapCheckFailed`; `Login.vue` renders the
  degraded state with retry.

### Verification

```text
PS> go test -count=1 ./cmd/gateway -run TestR9_ -timeout 180s
ok github.com/cn-maul/rosetta-gateway/cmd/gateway

PS> go test -count=1 ./internal/admin -run TestR9_ -timeout 180s
ok github.com/cn-maul/rosetta-gateway/internal/admin

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

## Post-Remediation Isolated Smoke Evidence

Recorded on 2026-10-26. This section records new smoke evidence only; it does
not renumber or reopen the original audit findings.

### SMOKE-01 failure-first evidence

- The first run of `TestAuthzAuditActor_UsesAuthenticatedIdentity` sent three
  management writes through the production middleware order
  `AutoReload -> UserAuth -> AdminGateGuard -> handler` using real signed JWT
  sessions and real handlers.
- Observed failure:

```text
audit actors = ["anonymous" "anonymous" "anonymous"],
want ["alice" "admin1" "admin1"]
```

- Cause: `AutoReload` creates the outer request, while `UserAuth` injects
  identity into a derived inner request. The outer audit callback therefore
  could not read the authenticated user.
- Fix: `AutoReload` adds a per-request actor slot; `UserAuth` writes the
  authenticated username into it only after session verification; the auditor
  prefers the slot and retains the identity-context fallback for direct
  callers. Authentication failures still return before any slot write, so they
  do not trigger a reload.
- Real binary evidence: the user key PATCH produced
  `smokeuser|PATCH|/admin/api/keys/...`, while prior rows remained
  `anonymous`. Admin writes produced actor `admin`; bootstrap remains
  legitimately anonymous.

### Verification

```text
PASS:
go test -count=1 ./internal/server -run
  'TestAuthzAuditActor_UsesAuthenticatedIdentity|TestAutoReload|TestAuditOnly' -v

PASS:
go test -short -count=1 -timeout 300s ./cmd/... ./internal/...

PASS:
$env:CC="C:/mingw64/bin/gcc.exe"
go test -race -short -count=1 -timeout 900s ./cmd/... ./internal/...

PASS:
cd web; npm test          # 41 passed, 0 failed
cd web; npm run typecheck
```

### Isolated real-operation evidence

```text
Browser pages:
  bootstrap, overview, provider/credential, discovery/import, route/targets,
  user creation, user login/key creation, history, models, wallet, settings,
  account, transfer tab, mobile providers

Browser correctness:
  consoleErrors=[] pageErrors=[] failedRequests=[]
  mobile scrollWidth=390 innerWidth=390

API protocols:
  chat=200 pong
  chat SSE=200 [DONE]
  responses=200 object
  anthropic=200 message
  invalid key=401
  malformed JSON=400

Key lifecycle:
  enabled call=200
  user disable=200, subsequent API call=403
  owner re-enable=403 (administrator-only restore)
  administrator restore=200, subsequent API call=200

Provider connectivity:
  status=ok, model_count=1

Config transfer:
  encrypted export=200 attachment, plaintext credential absent
  wrong passphrase=400
  dry-run=providers_created:1 models_created:1 credentials_added:1
  row counts before/after dry-run unchanged (providers=1, models=1)

Failover:
  response=200 pong
  route_trace="1:Broken Primary/broken-chat -> fail_switch:upstream_timeout; 2:Smoke OpenAI/fake-chat -> ok"
  restored single target=200 pong
```

Residual: Docker could not be executed on this host, so this smoke test does
not replace the already documented live image/Compose verification residual.
No commit, push, tag, deployment, or production database operation was
performed.

## v1.7.0 Release CI Finding

The first CI run for release commit `01b0b9c` exposed one Linux-only failure in
the full short suite:

```text
--- FAIL: TestE2E_F1c_DownstreamWriteFailureMarksCanceled
    e2e_datapath_test.go:1293: F1c 写失败后 usage 行数 = 2，期望恰好 1 行（重复记账）
```

### Failure-first reproduction

The regression test calls `normalize` on the exact production shape: a
committed client-abort outcome carrying the more specific
`downstream_write_failed` code.

```text
--- FAIL: TestR5_DownstreamWriteFailureKeepsItsCommittedKind
    outcome_lane_test.go:143: downstream write failure normalized to client_aborted,
    want downstream_write_failed
```

### Implementation

`Outcome.normalizedKind` now upgrades a committed client abort with code
`downstream_write_failed` to `OutcomeDownstreamWriteFailed`. The outer lane
then recognizes the already-persisted terminal result instead of adding a
second uncommitted-failure usage row.

### Verification

```text
go test ./internal/dataplane -run
  'TestR5_(AttemptOutcomeProjectsOneLaneDecision|DownstreamWriteFailureKeepsItsCommittedKind)' -count=1
go test ./cmd/gateway -run
  'TestE2E_F1c_DownstreamWriteFailureMarksCanceled|TestR0AC13_NonStream|TestR0AC14_NonStream' -count=20
go test ./internal/dataplane ./internal/usage ./internal/outwire -count=10
```

All passed. Tagging remains gated on a new green CI run and the GHCR workflow.
