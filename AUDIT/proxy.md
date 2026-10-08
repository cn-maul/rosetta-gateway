# Proxy / Data-Plane Audit — rosetta-gateway

**Scope:** `internal/upstream/upstream.go`, `internal/inwire/*`, `internal/outwire/*`,
`internal/routing/routing.go`, `internal/server/*`, and the `/v1` data-plane request path in
`cmd/gateway/main.go` (+ `cmd/gateway/billing.go`).

**Dependency verified against:** `github.com/cn-maul/rosetta v1.0.0`
(`go.mod`; note the repo also contains a stale `rosetta@v0.5.1` tree under `.gocache/` which is
**not** what the binary links — all SDK claims below were checked against
`C:\Users\louis\go\pkg\mod\github.com\cn-maul\rosetta@v1.0.0`).

**Runtime verification:** two findings were proven with runnable probes under
`AUDIT/.scratch-proxy/` (both compiled and executed against the real packages).

---

### [P1] Pre-first-event stream failure is reported to the client as HTTP 200 with an empty error envelope

**Location**
- `internal/outwire/errors.go:35-44` (the `(200, "", "")` mapping)
- `cmd/gateway/main.go:1865-1868` (the `outcomeFromErr(cerr)` call)
- `cmd/gateway/main.go:1660-1666` (the final write, which trusts `out.statusCode`)

**What's wrong**

`MapUpstreamError` maps rosetta's two stream sentinels to *"nothing to report"*:

```go
// internal/outwire/errors.go:35
if errors.Is(err, rosetta.ErrStreamTruncated) {
    // Partial content is already on the wire; there is no error body to
    // add on top of it.
    return http.StatusOK, "", ""
}
if errors.Is(err, rosetta.ErrStreamOverflow) {
    return http.StatusOK, "", ""
}
```

That reasoning is correct **only when the stream has already been committed**. But
`attemptStream` calls `outcomeFromErr` at `main.go:1867` *before* the SSE headers are written:

```go
// main.go:1865 — nothing has been written to the client yet
if !gotFirst || ttftTimedOut.Load() {
    if cerr := stream.Err(); cerr != nil {
        return outcomeFromErr(cerr)      // eligible=false, statusCode=200, code="", message=""
    }
```

The result then flows to the end of the failover loop, where `statusCode` is non-zero so the
502 fallback does **not** apply, and the gateway writes the sentinel's empty mapping verbatim:

```go
// main.go:1660
statusCode, code, message := out.statusCode, out.code, out.message   // 200, "", ""
if statusCode == 0 { ... }                                            // not taken
codec.WriteError(w, statusCode, code, message)                        // WriteError(w, 200, "", "")
```

Probe output (`go run ./AUDIT/.scratch-proxy/probe_sentinel`):

```
rosetta: stream truncated    -> status=200 code="" msg="" failoverEligible=false
rosetta: stream accumulation exceeded safety limits -> status=200 code="" msg="" failoverEligible=false

OpenAI   client sees: status=200 body={"error":{"message":"","type":"api_error"}}
Anthropic client sees: status=200 body={"error":{"message":"","type":"api_error"},"type":"error"}
```

**Why it matters**

A hard upstream failure reaches the downstream client as **`200 OK`**. Concretely:

* Any SDK that decides success from the HTTP status treats the call as successful with an empty
  message. OpenAI/Anthropic SDKs raise on `>= 400`, so they will hand the caller an empty string
  and **no error**.
* Billing/telemetry that key off status code record this as a success. `usage_records.http_status`
  is separately set to 200 at the end of the chain-exhausted path (`main.go:1698`), so dashboards show
  a green row for a call that produced nothing.
* It is also **un-transferable by construction**: `FailoverEligible` returns `false` for both
  sentinels (`errors.go:178-202`), so `main.go:1645` breaks out of the failover loop and the other
  chain targets are never tried. A failure that could have been absorbed by failover is instead
  surfaced as a fake success.

**Reachability.** The common shapes (`openai-chat` EOF-before-`[DONE]`, `anthropic` EOF-before-
`message_stop`, `responses` EOF-before-`response.completed`) all synthesize an `EventMessageEnd`
*first*, so `gotFirst` is true and they correctly take the committed path. The `!gotFirst`
precondition is met when:

1. `streamCore.Next()` trips the accumulator guard **on the first event**
   (`rosetta@v1.0.0/stream.go:261-272`: `s.cur = ev` is set but `Next()` returns `false` with
   `s.err = ErrStreamOverflow`). The 10 000-block cap (`maxStreamBlocks`) is reachable by a
   provider that emits many distinct `ToolIndex` values, and `maxStreamAccumBytes` (64 MiB) is
   reachable by accumulating across deltas.
2. The TTFT timer fires and the `!ttftTimer.Stop()` boundary at `main.go:1860` races with a stream
   that terminated with truncation — the code's own comments at `main.go:1855-1859` and
   `main.go:1897-1904` identify this exact boundary as real for the idle watchdog.

Confidence that the mapping itself is wrong: **high** (proven by execution). Confidence that case 1
needs a large accumulation before the first yielded event: **medium**.

**Concrete fix**

Make the sentinel mapping conditional on "already committed" — it is the only situation in which
"add nothing" is right. The cleanest place is `outcomeFromErr`, which is only ever called on the
uncommitted path:

```go
// cmd/gateway/main.go
func outcomeFromErr(err error) attemptOutcome {
	// ErrStreamTruncated / ErrStreamOverflow mean "content is already on the wire".
	// Every call site of outcomeFromErr is the *pre-commit* path, so that premise
	// is false here and the (200,"","") mapping would ship a fake success.
	if errors.Is(err, rosetta.ErrStreamTruncated) || errors.Is(err, rosetta.ErrStreamOverflow) {
		return attemptOutcome{
			eligible:   true,
			statusCode: http.StatusBadGateway,
			code:       "upstream_error",
			message:    "upstream stream failed before the first event",
		}
	}
	eligible := outwire.FailoverEligible(err)
	statusCode, code, message := outwire.MapUpstreamError(err)
	return attemptOutcome{
		eligible:     eligible,
		credCooldown: outwire.CredentialCooldown(err),
		statusCode:   statusCode,
		code:         code,
		message:      message,
	}
}
```

Marking these `eligible: true` also lets the failover chain try the next target, which is the whole
point of the chain. As a belt-and-braces guard, reject the degenerate triple at the write site too:

```go
// main.go:1661
if statusCode == 0 || statusCode == http.StatusOK {
    statusCode, code, message = http.StatusBadGateway, "upstream_error", "no available upstream provider"
}
```

(`statusCode == http.StatusOK` is unreachable from every other uncommitted outcome, so this cannot
mask a legitimate case.) Finally, add the missing regression test next to
`internal/outwire/errors_test.go:35`, which already pins `FailoverEligible(truncated) == false` but
never asserts the status mapping:

```go
func TestMapUpstreamError_StreamSentinelHasNoRealStatus(t *testing.T) {
	for _, e := range []error{rosetta.ErrStreamTruncated, rosetta.ErrStreamOverflow} {
		if st, code, _ := MapUpstreamError(e); st == http.StatusOK || code == "" {
			t.Fatalf("%v maps to %d/%q — an empty 200 is only valid post-commit", e, st, code)
		}
	}
}
```

---

### [P1] `usageRecorder.record` panics with "send on closed channel" when a request outlives the shutdown drain

**Location**
- `cmd/gateway/main.go:2316-2328` (`record`'s enqueue)
- `cmd/gateway/main.go:2333` (`wait` closes the queue)
- `cmd/gateway/main.go:606-614` (`srv.Shutdown` with a 10s deadline, then `usage.wait`)

**What's wrong**

`record` uses the classic "queue full → fall back to a synchronous write" shape:

```go
// main.go:2316
func (u *usageRecorder) record(rec *store.UsageRecord) {
	select {
	case u.queue <- rec:
	default:
		cost, err := u.db.CreateUsageRecordWithCost(context.Background(), rec)
		...
	}
}
```

and `wait` closes the channel:

```go
// main.go:2332
func (u *usageRecorder) wait(ctx context.Context) {
	close(u.queue)
	...
}
```

**A `select` with a `default` does not protect against a send on a closed channel** — that is a
runtime panic, not a "channel not ready" case. Probe (`go run ./AUDIT/.scratch-proxy/probe_closed_channel`):

```
recovered = send on closed channel
```

The ordering that makes this reachable is in `main`:

```go
// main.go:604-614
ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
if err := srv.Shutdown(ctx); err != nil {          // <-- returns ctx.Err() on timeout, handlers still running
    logger.Error("shutdown error", "error", err)   //     and this error is only LOGGED
}
...
usage.wait(drainCtx)                                 // closes u.queue anyway
```

`http.Server.Shutdown` **does not** forcibly terminate handlers when its context expires; it returns
`context.DeadlineExceeded` and leaves them running. Every streaming request is exactly the kind that
survives 10s (the stream lifecycle is bounded only by `stream_idle_timeout`, default 60s, and
`WriteTimeout: 0` at `main.go:531`). So the normal SIGTERM-during-traffic sequence is: a long stream
is still in `attemptStream` → `wait` closes the queue → the stream finishes → `usage.record(...)` at
`main.go:2031` or `main.go:1686` → **panic**.

**Why it matters**

* The panic aborts the handler *after* the response was (partly) written, so the in-flight usage
  record is lost — precisely the accounting the drain at `main.go:610-611` exists to protect.
* `Recovery` (`server.go:430`) sits *inside* `Middleware`, so the process does not die, but the
  request dies, and the log shows a stack trace rather than an accounting gap.
* It fires during shutdown, the one moment when an operator is already looking for anomalies, and
  it is time-dependent (only on streams longer than the shutdown budget), so it will not reproduce
  in a casual test.

**Concrete fix**

Give the recorder a closed flag guarded by a mutex so `record` degrades to the synchronous write
instead of panicking:

```go
// cmd/gateway/main.go
type usageRecorder struct {
	db     *store.Store
	logger *slog.Logger

	mu     sync.Mutex
	queue  chan *store.UsageRecord
	closed bool
	wg     sync.WaitGroup
}

func (u *usageRecorder) record(rec *store.UsageRecord) {
	u.mu.Lock()
	if !u.closed {
		select {
		case u.queue <- rec:
			u.mu.Unlock()
			return
		default:
		}
	}
	u.mu.Unlock() // queue full OR already closed → synchronous write (back-pressure, never a panic)

	cost, err := u.db.CreateUsageRecordWithCost(context.Background(), rec)
	if err != nil {
		u.logger.Error("failed to record usage (sync fallback)", "error", err, "key_id", rec.AccessKeyID)
		return
	}
	u.charge(rec, cost)
}

func (u *usageRecorder) wait(ctx context.Context) {
	u.mu.Lock()
	u.closed = true
	close(u.queue)
	u.mu.Unlock()

	done := make(chan struct{})
	go func() { u.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
	}
}
```

The `Shutdown` error should also stop pretending the drain is meaningful:

```go
// main.go:606
if err := srv.Shutdown(ctx); err != nil {
    logger.Error("shutdown error; handlers still in flight, usage drain is best-effort",
        "error", err, "inflight_note", "streams longer than the 10s shutdown budget")
}
```

---

### [P2] No overall request deadline: a single `/v1` call can be held for minutes, and the TTFT watchdog does not cover the pre-header phase

**Location**
- `cmd/gateway/main.go:1822` (`ctx := r.Context()` — the streaming path has no deadline)
- `cmd/gateway/main.go:1848` (TTFT timer is created *after* `ChatStream` returns)
- `internal/upstream/upstream.go:855` (`ResponseHeaderTimeout: 60 * time.Second`)
- `cmd/gateway/main.go:531` (`WriteTimeout: 0`)
- `cmd/gateway/main.go:1529-1652` (the failover loop has no cross-attempt budget)

**What's wrong**

`attemptStream` binds the stream to the bare request context:

```go
// main.go:1822
ctx := r.Context()
...
// main.go:1834
stream, err := client.ChatStream(ctx, upstreamReq)
```

The SDK applies `WithTimeout` to unary calls only (`rosetta@v1.0.0/client.go:199-202`: *"the
caller's ctx bounds the whole stream, while WithTimeout applies only to unary calls"*), so the only
bounds on this path are the two watchdogs — and the **TTFT watchdog starts too late**:

```go
// main.go:1834  <-- up to ResponseHeaderTimeout (60s) elapsed here, unbounded by TTFT
stream, err := client.ChatStream(ctx, upstreamReq)
...
// main.go:1848
ttftTimer := time.AfterFunc(ttftTimeout, func() { ... })   // only now
gotFirst := stream.Next()
```

So the pre-header phase is bounded only by `ResponseHeaderTimeout = 60s`, and `upstream_timeout_ms`
(120s by default) — the operator's "upstream timeout" knob — **does not apply to streams at all**.

Worse, the failover loop applies that budget **per attempt**, and there is no aggregate cap:

| phase | bound | source |
|---|---|---|
| dial + TLS | 10s each | `upstream.go:871-875` |
| response headers | 60s | `upstream.go:855` |
| TTFT (after headers) | 30s | `firstTokenTimeout` |
| per attempt, streaming | ≤ ~100s | sum of the above |
| × `failover_max_targets` (default 3) | **≤ ~300s** | `main.go:1456-1461` |
| non-stream, per attempt | 120s × 3 | `main.go:2084` |

With `WriteTimeout: 0` (correctly — the comment at `main.go:526-531` explains why a global write
timeout would kill legitimate long streams) and no `http.TimeoutHandler` or handler-level
`context.WithTimeout`, **nothing** bounds the total. Every intermediate socket also stays open for
the whole duration.

**Why it matters**

* One slow or half-dead upstream pins a client connection, an upstream connection, a usage-reservation
  row (`reserved_tokens`) and a failover slot for up to five minutes. Enough of these and the gateway
  is effectively down for new work while looking healthy.
* The operator-facing `upstream_timeout_ms` setting is silently not applied to streaming — the
  setting page implies a global guarantee it does not provide.
* `IdleTimeout: 2 * time.Minute` (`main.go:536`) does not help: it governs *idle keep-alive*
  connections, not in-flight requests.

**Concrete fix**

Wrap the whole `handleIngress` body in a total-deadline context, leaving the configured budgets as
the per-hop limits, and document the relationship:

```go
// cmd/gateway/main.go, inside handleIngress's returned closure
// Total request budget. It must exceed any single hop's budget
// (upstream_timeout_ms / ResponseHeaderTimeout) because failover reuses the
// budget per attempt, but it must still be finite: without it a chain of
// half-dead upstreams holds the client for minutes (≈100s × failover_max_targets).
total := time.Duration(cfg.FailoverMaxTargets()+1) * (nonStreamTimeout(snap, cfg) + 2*time.Second)
ctx, cancel := context.WithTimeout(r.Context(), total)
defer cancel()
r = r.WithContext(ctx)
```

Apply the same derived deadline to the streaming pre-header phase by arming the TTFT watchdog
**before** `ChatStream` — `rosetta` exposes no pre-`StreamChat` hook, so the transport is the
correct lever:

```go
// internal/upstream/upstream.go — make the header wait part of the TTFT budget
ResponseHeaderTimeout: 60 * time.Second,   // keep as the standalone ceiling
```

and in `attemptStream`, bound the connect phase with the first-token budget:

```go
// cmd/gateway/main.go:1834
dialCtx, dialCancel := context.WithTimeout(ctx, ttftTimeout)
defer dialCancel()
stream, err := client.ChatStream(dialCtx, upstreamReq)
```

(`defer dialCancel()` is safe: it only frees the timer; the SDK derives its own cancellable child
context for the stream body, so cancelling here after `ChatStream` returns would be wrong — cancel
it explicitly once the first event lands instead.)

Whichever shape you pick, state the composition explicitly in the settings page copy so
`upstream_timeout_ms` is not read as a total-request guarantee.

---

### [P3] `TargetAvailable` takes the pool **write** lock on the per-request failover path

**Location** `internal/upstream/upstream.go:489-497`

```go
func (p *Pool) TargetAvailable(targetID string) bool {
	p.mu.Lock()          // <-- write lock; nothing is mutated
	defer p.mu.Unlock()
	h, ok := p.targets[targetID]
	...
}
```

**What's wrong**

This is a pure query, and its own doc comment says so ("它是**纯查询**"). It only reads
`h.until`, yet takes the exclusive lock. Every failover-enabled request calls it once per candidate
(`main.go:1473`). Meanwhile `MarkCredentialCooldown`, `RecordCredentialSuccess`,
`RecordTargetFailure`, `RecordTargetSuccess`, `ReleaseTargetProbe` and `ClaimTargetProbe` all take the
same write lock on every request completion. The whole `sync.RWMutex` collapses to a mutex.

**Why it matters**

Throughput, not correctness. Under the lock all write-lock holders queue behind it, so the read path
of a hot failover chain serializes against all the per-request bookkeeping the `credIndex`
optimization at `upstream.go:34-47` was explicitly added to avoid. It is self-contradicting: the
comment justifying `credIndex` says the write locks would otherwise "把整个数据面串行化" — and this
one reintroduces exactly that for every failover request.

**Concrete fix**

```go
// internal/upstream/upstream.go
func (p *Pool) TargetAvailable(targetID string) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	h, ok := p.targets[targetID]
	if !ok {
		return true
	}
	return !time.Now().Before(h.until)
}
```

`h.until` is only ever written under the write lock (`RecordTargetFailure`, `Install`), both of which
exclude readers — so a read lock is sufficient and race-free.

---

### [P3] `ReleaseTargetProbe` clears `halfOpen` without an ownership token — a pool rebuild in between lets two probes into the same target

**Location**
- `internal/upstream/upstream.go:580-586` (`ReleaseTargetProbe`)
- `internal/upstream/upstream.go:374` (`Install` resets `h.halfOpen = false`)
- `cmd/gateway/main.go:1641-1643` (the call site)

**What's wrong**

`halfOpen` is a bare `bool` shared across all in-flight requests for a target, and
`ReleaseTargetProbe` clears it unconditionally:

```go
func (p *Pool) ReleaseTargetProbe(targetID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if h, ok := p.targets[targetID]; ok {
		h.halfOpen = false        // clears whatever claim is there, not necessarily its own
	}
}
```

The doc comment at `upstream.go:578-579` names exactly this hazard but only guards against one
trigger ("不得在已调用 Record* 之后再调"). A second trigger exists:

1. Request A claims the probe (`halfOpen = true`) and is in flight.
2. An admin write triggers `runtimeReloader.Reload` → `Pool.Install` → `h.halfOpen = false`
   (`upstream.go:374`, deliberate: it also *keeps the existing `targetHealth` pointer*, line 375).
3. Request B now claims successfully (`halfOpen = true`).
4. Request A receives a non-transferable upstream 400, so it reaches `main.go:1641` with
   `probeClaimed && !probeRecorded` and calls `ReleaseTargetProbe`.
5. B's claim is wiped while B is still in flight → request C also claims → **two (then three)
   concurrent probes against a target that is exactly the one that just failed.**

The stampede protection the half-open mechanism exists for (documented at `upstream.go:60-63` and
`499-509`) is defeated for the duration of those requests.

**Why it matters**

Low blast radius (needs an admin reload to land inside the window) but it produces the exact
symptom the design calls out: a burst of concurrent requests to a target that is still unhealthy,
each counting its own failure. Worst case is a few wasted upstream calls plus inflated breaker
counts — no correctness or data-loss impact.

**Concrete fix**

Make the claim a generation counter rather than a bool, and release only if the generation is still
yours:

```go
// internal/upstream/upstream.go
type targetHealth struct {
	consecutiveFails int
	until            time.Time
	// probeGen increments on every ClaimTargetProbe; a releaser must present
	// the generation it was handed, so a pool rebuild (which resets halfOpen)
	// plus a later claim cannot be clobbered by a stale release.
	probeGen uint64
}

func (p *Pool) ClaimTargetProbe(targetID string) (bool, uint64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	h, ok := p.targets[targetID]
	if !ok {
		return true, 0
	}
	if time.Now().Before(h.until) || h.probeGen%2 == 1 {
		return false, 0
	}
	h.probeGen++
	return true, h.probeGen
}

func (p *Pool) ReleaseTargetProbe(targetID string, gen uint64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if h, ok := p.targets[targetID]; ok && h.probeGen == gen {
		h.probeGen = 0
	}
}
```

If a signature change is too invasive for now, the cheap mitigation is to have `Install` delete the
`targetHealth` entry instead of reusing the pointer when `halfOpen` is set, so a stale releaser
finds a fresh entry and (with a generation check) becomes a no-op.

---

### [P3] `AdminGateGuard` whitelist is prefix-matched

**Location** `internal/server/user_auth.go:147-163`, whitelist at `user_auth.go:132-142`

```go
var userAccessiblePrefixes = []string{
	"/admin/api/keys", "/admin/api/usage", "/admin/api/stats", "/admin/api/me", "/admin/api/logout",
	"/admin/api/model-names",
}
...
if strings.HasPrefix(p, pre) { next.ServeHTTP(w, r); return }
```

**What's wrong**

Prefix matching means `/admin/api/me-anything`, `/admin/api/logoutx` and `/admin/api/usage/../users`
all bypass the admin gate. Today this is **not exploitable**: the only route registered under those
prefixes for a non-admin is `/admin/api/me` (exact), and Go's `ServeMux` cleans `..` segments
before dispatch and 404s unregistered paths. It is a latent trap, not a live bypass — reporting it
as low severity deliberately, since the admin plane is owned by another auditor and I did not trace
every handler's own scope-narrowing.

**Why it matters**

Only on a future route addition. The whitelist's own comment argues that the failure direction
should be "explicitly unavailable" over "silently over-permissive"; a prefix match quietly undoes
that guarantee for the next endpoint someone registers.

**Concrete fix**

Match on the exact path or a genuine path-segment boundary:

```go
func pathAllowed(p, pre string) bool {
	return p == pre || strings.HasPrefix(p, pre+"/")
}
```

---

## Areas checked and found clean

* **The previously-fixed cooldown data race** (`internal/upstream/upstream.go:412-478`).
  `MarkCredentialCooldown` and `RecordCredentialSuccess` both snapshot `cred.CooldownUntil`,
  `cred.Status` and `p.cooldownSto` **inside** the write lock and touch nothing shared afterwards
  (lines 418-430, 456-467). This is fully fixed. I searched for siblings of that pattern and found
  **none**: every other `Pool` method that reads or writes `*CredentialEntry` / `*targetHealth` fields
  (`GetAnyClient` 394, `TargetAvailable` 489, `ClaimTargetProbe` 510, `RecordTargetFailure` 529,
  `RecordTargetSuccess` 557, `ReleaseTargetProbe` 580, `Install` 358, `reindexLocked` 126) holds the
  lock for the whole access, and `getHealthyCredentials` (597) / `selectWeighted` (615) are only ever
  called from under the `RLock` at `GetAnyClient`.
* **Unbounded request bodies** — no issue. `RequestSizeLimit` wraps every route with
  `http.MaxBytesReader` (`main.go:520`, `server.go:115-122`), and each decoder independently re-reads
  at `maxBytes+1` and raises `*http.MaxBytesError` so the 413 branch is reachable
  (`inwire/openai_chat.go:136-146`, `inwire/anthropic.go:105-115`, `inwire/openai_responses.go:82-92`).
  `handleIngress` maps it to 413 with the limit (`main.go:1298-1303`).
* **Unbounded response buffering** — no issue. Non-stream bodies are capped at 8 MiB by the SDK
  (`rosetta@v1.0.0/errors.go:134`), stream accumulation at 64 MiB / 10 000 blocks
  (`stream.go:390-391`), and individual SSE events at 16 MiB / 2 MiB per line
  (`internal/sse/sse.go:17-20`).
* **Goroutine leaks on client disconnect** — no issue. `attemptStream` binds the SDK stream to
  `r.Context()` (`main.go:1822`), and the SDK attaches both a `context.CancelFunc` and the response
  body to the stream (`client.go:228`, `provider_openai_chat.go:479-480`), so a client disconnect
  unblocks the in-flight read via the transport. The heartbeat goroutine is joined by
  `close(quit); hbWG.Wait()` in a `defer` (`main.go:1955-1958`) — `defer`, not fire-and-forget,
  which is what keeps it from writing into a recycled connection.
* **Client disconnect mid-stream is correctly not charged or blamed on the upstream** — no issue.
  `main.go:1628` skips all breaker/cooldown bookkeeping when `r.Context().Err() != nil`, the
  mid-stream classification sets `status = "canceled"` rather than `error` (`main.go:1998-2002`), and
  `usageRecorder.charge` refuses anything whose `Status != "ok"` (`main.go:2269-2271`). It also
  cannot loop forever: the next iteration's disconnect check at `main.go:1536` returns immediately.
* **No retry after a partial response has been written** — no issue, and this is carefully handled.
  `attemptStream` deliberately defers the SSE header until the first upstream event
  (`main.go:1881-1888`), so "connection failed" and "slow first token" both happen pre-commit and
  remain transferable; once bytes are out, `attemptOutcome.committed` is set
  (`main.go:2071`) and `handleIngress` returns immediately at `main.go:1603-1614` instead of trying
  the next target. Non-stream sets the same flag (`main.go:2129`) after `WriteNonStream`.
* **Retry on non-retryable status** — no issue. `FailoverEligible` (`errors.go:178-202`) admits only
  401/403/402/404/410/408/429/5xx plus transport errors; 400 and other 4xx fall through to `false`,
  and `main.go:1645` breaks out of the loop on `!out.eligible`.
* **Retry ignoring context cancellation** — no issue. `r.Context()` is threaded into both
  `attemptNonStream` (`main.go:2084`) and `attemptStream` (`main.go:1822`), and the loop re-checks it
  at the top of every iteration (`main.go:1536`).
* **Retry budget / infinite loops** — no issue. The loop is a bounded `for i, cand := range active`
  over a slice already truncated to `min(len(cands), failover_max_targets)` (`main.go:1455-1518`),
  with `failover_max_targets` validated to `1..100` at both entry points
  (`config/config.go:217`, `admin/settings_handler.go:114`).
* **Circuit breaker state transitions under concurrency** — no issue in the state machine itself.
  Every transition is under `p.mu.Lock()`, and the `halfOpen` slot is correctly released on all three
  exit paths (bookkeeping at `main.go:1612`/`1630`, explicit release at `main.go:1641-1643`). The
  residual ownership gap is reported above as P3. Cooldown is correctly persisted so a pool rebuild
  cannot resurrect a just-banned key (`upstream.go:437-444`).
* **Cooldown key correctness** — no issue. Keyed by `cred.ID` via `credIndex`
  (`upstream.go:422`), not by provider, so one bad key does not cool down the others;
  `getHealthyCredentials` (597-613) filters per credential.
* **Data-plane access-key auth** — no issue. `internal/auth/auth.go:45-120` rejects missing
  (`ErrNoKey`), unknown, disabled, expired (`now >= ExpiresAt`, line 73) and IP-blocked keys, and
  additionally refuses keys whose owning user is missing or not active (lines 92-105), so a disabled
  user takes effect on the next `snapshot.Swap` with no per-request DB read. The hash-indexed lookup
  (line 60) is the documented and correct design here: keys are gateway-generated high-entropy
  strings, so SHA-256 index lookup leaks nothing exploitable (the comment at lines 54-59 argues this
  correctly, and I agree).
* **Charged exactly once** — no issue. `rateCommit.commit`/`releaseQuota` is invoked on exactly one
  terminal path per request: a committed attempt returns immediately (`main.go:1603-1614`), and the
  chain-exhausted path commits at `main.go:1667`. `releaseQuota` zeroes `quotaReserved` before
  releasing (lines 1087-1088) so it is idempotent. `charge` runs once per persisted record, after
  the write, and uses `cost_total` as frozen at insert time so billing cannot drift from reporting
  (`main.go:2217-2230`). Balance charging is idempotent on `request_id` and skipped entirely when
  `request_id` is empty rather than risking a double charge (`main.go:2279-2284`).
* **A failed request does not consume balance** — correct by design and correctly implemented:
  `charge` requires `rec.Status == "ok"` (`main.go:2269`). Note that `cost_total` (gateway cost) is
  still recorded for failures — that is the documented two-ledger distinction at `main.go:2236-2251`,
  and it is correct.
* **Hop-by-hop header forwarding** — no issue, because there is none. The gateway never copies
  client headers upstream; the SDK builds a fresh header set per call
  (`rosetta@v1.0.0/provider_openai_chat.go:74-76`, `openAIHeaders`), so `Connection`,
  `Transfer-Encoding`, `Content-Length`, `Host` and `Accept-Encoding` are entirely under
  `http.Transport`'s control. Go's transport handles gzip transparently, so there is no
  content-encoding confusion. Same for `Content-Length` on translated bodies: bodies are re-marshalled
  by the SDK and length is recomputed per attempt.
* **`[DONE]` sentinel / terminal sequence** — correct. The SDK normalizes all three upstream dialects
  to a single `EventMessageEnd`; the sinks emit the downstream terminal exactly once in
  `Finish` (`openaiSink.Finish` writes `finish_reason` → optional usage → `[DONE]` only when
  `status == "ok"`; `AnthropicSSE.Finish` writes `message_delta` + `message_stop`, or an `error` event
  without `message_stop` on truncation; `ResponsesSSE.Finish` writes `response.completed`, or
  `response.failed`). No double-close of the response body: `streamCore.releaseLocked` is guarded by
  `s.released` (`rosetta@v1.0.0/stream.go:359-373`), and the gateway's `defer stream.Close()`
  (`main.go:1838`) is therefore a safe no-op after a clean end.
* **Flushing** — correct. `statusResponseWriter.Flush` forwards to the underlying `http.Flusher`
  (`server.go:104-108`) without the mutex, which is safe only because every data-plane sink serializes
  `Fprintf` **and** `Flush` under its own mutex (`SSEWriter.mu`, `AnthropicSSE.mu`,
  `ResponsesSSE.mu`) — so heartbeat and event-loop writes can never interleave into a half-written
  `data:` line, and no two flushes race. `WriteHeader` at `server.go:74` also runs outside the mutex,
  but `w.WriteHeader(http.StatusOK)` (`main.go:1888`) strictly precedes the heartbeat goroutine's
  creation (`main.go:1942`), so there is no concurrent-header-write window either.
* **Missing/extra flush stalls** — none found. Each sink flushes per event and per keepalive, and the
  gateway adds an SSE comment heartbeat at `idleTimeout/2` only when genuinely idle (the ticker is
  reset on every event at `main.go:1973`). `heartbeatInterval` is forced positive
  (`main.go:1922-1925`) precisely because a 1 ms idle timeout would otherwise make `NewTicker` panic
  *after* the SSE headers were already written.
* **Watchdog races** — well handled. Both watchdogs close the window the `Stop()` return value leaves
  open: the TTFT path waits on `ttftDone` so `stream.Close()` is known to have landed before deciding
  (`main.go:1860-1863`), and the idle path re-reads `lastEventAt` inside the callback rather than
  trusting `Reset` (`main.go:1905-1916`), with `lastEventAt` updated *before* `Reset` as its comment
  requires. `safeMillis` (line 1766) clamps ms→Duration so a dirty DB value cannot wrap to negative
  and make `context.WithTimeout`/`time.AfterFunc` fire immediately; the same clamp constant is shared
  with `config.MaxDurationMillis` and both admin write paths.
* **Protocol translation: model mapping** — correct. `upstreamReq.Model = cand.UpstreamModel.ModelID`
  is applied per attempt (`main.go:1831`, `2079`), and the request is rebuilt per attempt
  (`ing.buildRosetta`) precisely so `Extra` from one target's protocol cannot leak to the next —
  the comment at `main.go:1026-1028` correctly identifies why.
* **Protocol translation: unknown-field handling** — no silent drops where it matters. Structured
  output is treated as a hard constraint and enforced (anthropic candidates filtered at
  `main.go:1496-1515`, 400 when none remain) rather than degraded; `tool_choice` is translated for
  every protocol pair; `response_format` is passed as `json.RawMessage` so unmodelled subfields
  (`strict`, schema name) survive (`inwire/openai_chat.go:287-292`); `text` passthrough keys beyond
  `format` are forwarded (`inwire/openai_responses.go:382-393`); unknown content blocks become typed
  blocks that rosetta rejects loudly instead of vanishing (`inwire/anthropic.go:431-435`,
  `inwire/openai_responses.go:195-202`); and `reasoning_effort` is deliberately kept out of `Extra`
  because it is a reserved SDK key and would 400 every request (`inwire/openai_chat.go:302-313`).
* **Protocol translation: multimodal + tool passthrough** — correct. `image_url` is collected into
  `BlockImage` instead of being dropped (`inwire/openai_chat.go:504-543`); Anthropic base64 sources
  normalize to `data:` URLs (`inwire/anthropic.go:464-482`); documents become `BlockFile`
  (485-506); the malformed-JSON fallback returns empty rather than shipping a raw JSON literal as
  prose (the comment at 541-543 documents the old bug). Both streaming sinks key tool-call block
  identity on `ToolIndex`, not `ToolID`, which is the correct call given the SDK's contract that
  continuation fragments carry neither — and this is regression-tested
  (`outwire/toolcall_stream_test.go:22-120`).
* **Protocol translation: stop reasons and usage** — correct. Anthropic usage is split into the
  mutually-exclusive billing buckets with a floor at 0 (`outwire/anthropic.go:110-126`), which
  prevents double-counting cached tokens; cached tokens are reported on the non-stream OpenAI-chat
  path too, matching the streaming path (`outwire/openai_chat.go:289-295`); `usage_state` distinguishes
  "reported" from "missing" so missing-usage upstreams cannot be mistaken for zero-token calls
  (`main.go:2138-2143`); and `responses` non-stream message items get a unique per-index id
  (`outwire/openai_responses.go:291-299`).
* **Protocol translation: error mapping upstream↔downstream** — good, with the one P1 above.
  Upstream bodies are never echoed back (only logged) — `MapUpstreamError` is explicit about this at
  `errors.go:64-69` and `83-86` — and the `type` translation table
  (`errorTypeFromCode`, `anthropicErrorType`) maps the categories clients actually branch on
  (auth / quota / timeout / overloaded) rather than collapsing everything to `api_error`.
* **Routing / model whitelist** — no bypass found. `Resolve` refuses a *disabled* named route before
  falling through to the `provider/model` direct form (`routing/routing.go:181-202`), which closes a
  real bypass for route names containing `/` (documented at 185-195). The whitelist is checked
  against the **resolved** `route.PublicName`, not the request string, so the direct form cannot
  escape it (`main.go:1418`); `handleListModels` and `handleGetModel` use the same resolved-name
  judgement, so "listed but 403" and "usable but unlisted" cannot diverge. `ModelAllow`'s zero value
  is deny-all rather than allow-all (`snapshot/snapshot.go:74-101`), the right direction for a
  permission boundary. `buildCandidates` never resurrects an explicitly disabled target
  (`routing.go:254-259`).
* **Per-request rate limiting** — correct. RPM is counted only for admitted requests (deliberate and
  documented at `main.go:1250-1257`), TPM reserves before the call and corrects after with a
  zero floor (`ratelimit/limiter.go:145-149`), and the single-request-too-large case fills the window
  instead of starving the key forever (`limiter.go:114-121`). Bucket growth is bounded by sweep.
* **Balance precheck ordering** — correct. `balanceExempt` short-circuits before any DB read, the
  unpriced-model case returns before touching the DB so an unpriced deployment is never blocked by
  balance-lookup flakiness, and the `est == 0 but priced` case still requires a positive balance
  (`billing.go:529-547`) — otherwise a near-zero-balance user could send arbitrarily small requests
  forever.

## Notes for the next reader

* The `.gocache/github.com/cn-maul/rosetta@v0.5.1/` tree in this repo is **not** the linked
  dependency (`go.mod` requires `v1.0.0`). Auditing against it produces wrong conclusions — in
  particular v0.5.1 does *not* retry chat POSTs, while v1.0.0 does unless
  `Quirks.NoIdempotencyKey` is set. The gateway does set it (`upstream.go:891`), so the SDK adds no
  retries on top of the failover chain — which is the intended design and is correct today. That
  comment in `upstream.go:884-890` is accurate for v1.0.0.
* `internal/server/autoreload.go:190-195` has a comment/code mismatch (`AuditOnly` says it records
  `>= 200` "2xx 与 4xx 都记" but the code is `sw.StatusCode() < 400`). That is in the admin plane and
  belongs to the admin auditor's domain, so I flag it only as a pointer.
* Runtime probes used for this audit are kept under `AUDIT/.scratch-proxy/` and are throwaway; no
  source file was modified.
