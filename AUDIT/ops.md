# ops audit — build/CI, deploy/config, observability, docs drift, cross-cutting code quality

Scope: `.github/workflows/*`, `Dockerfile`, `docker/*`, `.dockerignore`, `DOCKER.md`,
`build.ps1`, `gateway.ps1`, `go.mod`, `config.example.json`, `README.md`,
`cmd/gateway/main.go` (non-auth), `.gitignore`, `.gitattributes`, plus a sampled
sweep of cross-cutting Go quality across `internal/**` and `cmd/**`.

Severity: **P0** ships broken / exposed / leaks secrets · **P1** serious operational
bug · **P2** real defect · **P3** minor.

No source files were modified during this audit. `internal/webui/dist` was
regenerated once to run the CI staleness check and left byte-identical (verified
with `git status`); `build.ps1`, Docker builds, and anything else mutating the repo
were deliberately not run.

---

### [P1] `gateway.ps1` writes a master key whose format the gateway rejects → gateway refuses to start the moment the key is used from the file

**Location** — `gateway.ps1:179` (generation), `gateway.ps1:181` (write), consumed by
`internal/crypto/crypto.go:72-79` (`validKeyFormat`, `crypto.go:101-107`).

**What's wrong** — When no master key is supplied, the script generates one as
64 hex characters and saves it to `bin\master.key`:

```powershell
$MasterKey = -join ((1..32) | ForEach-Object { '{0:x2}' -f (Get-Random -Maximum 256) })
Set-Content -Path $KeyFile -Value $MasterKey -Encoding ASCII
```

The Go loader validates any key read from that file: `validKeyFormat` requires
`len(raw) == 44` and a successful `base64.URLEncoding.DecodeString` yielding 32
bytes, because `crypto.GenerateKey` always produces base64url(32B) = 44 chars
(`crypto.go:249-253`). A 64-char hex string fails the length test, so
`LoadMasterKey` returns:

> `master.key 内容格式非法（长度 64，自动生成的密钥应为 44 字符 base64url）：文件很可能已损坏…`

**Verified locally**: the script's generator produces length 64; `validKeyFormat`
requires 44. (The `bin\master.key` currently on this machine is 44 chars because it
was written by the Go code path, not the script — which is why the defect is latent
and not yet on fire.)

**Why it matters** — the failure is asymmetric and confusing. While launched via
`gateway.ps1` the key arrives via `$env:ROSETTA_GW_MASTER_KEY` (`gateway.ps1:190`),
and `LoadMasterKey` checks the env var *first* (`crypto.go:49`) and never validates
its format — so the script's own run works fine. The moment the same state directory
is used by any other launcher — double-clicking `bin\gateway.exe`, `docker run`
with a mounted home, a service unit, a second operator's shell — there is no env var,
the file path is taken, and **the gateway exits 1 at startup**. That is precisely the
split-brain that `crypto.go:36-40` says this code exists to prevent, inverted: the
script creates the two-path inconsistency rather than fixing it.

Secondary issue on the same line: `Get-Random` is **not** a CSPRNG. The material
protects every upstream provider API key in the database (AES-256-GCM key derived
via SHA-256), so PRNG-grade entropy here is a real weakness even though the 32-byte
length makes brute force from the *length* infeasible.

**Concrete fix** — delete the generation block entirely (lines 178-184) and let the
Go side do it. The gateway already generates and atomically persists a correctly
formatted key on first start (`crypto.go:87-90`), and the script then reuses it
via `bin\master.key`… except that path also hits `validKeyFormat`. So the correct
minimal fix is:

1. Remove the `Get-Random` generation from `gateway.ps1` (lines 178-184) and drop
   the `bin\master.key` read/write (lines 174-177), letting `ROSETTA_GW_MASTER_KEY`
   stay empty so the gateway uses its own file. This also removes the weak RNG.
2. Keep `-MasterKey` / `ROSETTA_GW_MASTER_KEY` as explicit overrides only.
3. Add a `TestKeyFileFormat` unit test in `internal/crypto` asserting that the key
   `gateway.ps1` would have written is rejected — or, better, a test asserting the
   launcher contract "whatever writes `master.key` must write 44-char base64url", so
   the two entry points cannot drift again.

**Confidence** — high (both the 64-char generation and the 44-char validation were
executed/confirmed, not inferred).

---

### [P1] Graceful shutdown can panic the process: `usage.queue` is closed while in-flight handlers can still send to it

**Location** — `cmd/gateway/main.go:604-614` (shutdown order), `cmd/gateway/main.go:2333`
(`close(u.queue)` inside `usageRecorder.wait`), `cmd/gateway/main.go:2317-2319`
(`u.queue <- rec` in `record`).

**What's wrong** — the shutdown sequence assumes `srv.Shutdown` returning means
"all handlers have returned":

```go
ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
if err := srv.Shutdown(ctx); err != nil {          // main.go:606
    logger.Error("shutdown error", "error", err)    // ← error is logged, NOT fatal
}
drainCtx, drainCancel := context.WithTimeout(context.Background(), 5*time.Second)
usage.wait(drainCtx)                                 // main.go:614 → close(u.queue)
```

When the 10s deadline expires, `Shutdown` returns `context.DeadlineExceeded` and
active handlers are **still running**. The code only logs and continues. Two seconds
later `usage.wait` calls `close(u.queue)` (`main.go:2333`) while those handlers are
still running; when such a handler finishes and calls `usage.record()`, the send at
`main.go:2318` hits a closed channel and panics with *send on closed channel*.

**Why it matters** — the window is real, not theoretical: a streaming request has
no write timeout (`WriteTimeout: 0`, deliberately, `main.go:531`) and its lifetime
is bounded only by the TTFT / idle watchdogs, so a slow upstream can legitimately
outlive a 10s shutdown drain. On that path the process panics during shutdown, so
the `defer db.Close()` (`main.go:170`) never runs and every usage record still in
the queue is lost — exactly the loss the surrounding comment says the drain exists to
prevent. `server.Recovery` does catch the panic *for that one request* (it converts
it to a 500), so the failure is not always fatal — but if the panic lands outside a
served request, or a second handler panics on the same already-closed channel, the
process aborts. The observable symptom is "the gateway dies ugly on every shutdown
that has a slow request in flight", which operators read as a crash, not as a drain.

**Concrete fix** — make the queue close conditional on the drain actually having
completed, and make `record` tolerate a closed queue:

```go
if err := srv.Shutdown(ctx); err != nil {
    logger.Error("graceful shutdown incomplete; forcing close", "error", err)
    _ = srv.Close()          // terminate in-flight conns BEFORE closing the queue
}
```

`http.Server.Close()` (unlike `Shutdown`) force-closes listeners and connections and
returns once handlers have been interrupted, so after it returns no handler can still
call `record`. Additionally, make the recorder itself defensive so the ordering bug
cannot become a panic even if the sequence changes later:

```go
func (u *usageRecorder) record(rec *store.UsageRecord) {
    defer func() {
        if r := recover(); r != nil {   // queue closed during shutdown
            u.logger.Warn("usage queue closed during record; dropping record", "key_id", rec.AccessKeyID)
        }
    }()
    ...
}
```

and guard the send with a `closed atomic.Bool` set by `wait` before `close`, so the
common case is a logged drop rather than a recovered panic.

**Confidence** — high for the code path (ordering is exactly as quoted); medium for
how often it fires in production, since it needs a >10s in-flight request at
shutdown.

---

### [P2] CI never runs the race detector on a codebase built around shared mutable state

**Location** — `.github/workflows/ci.yml:57`.

**What's wrong** — the test gate is:

```yaml
run: go test -count=1 -timeout 300s ./internal/... ./cmd/gateway/
```

There is no `-race` anywhere in either workflow (verified by grepping
`.github/workflows` for `-race`: no matches).

**Why it matters** — this repo is unusually concurrency-dense for its size, and it
has *already* been bitten by a data race in exactly this area: the commit
`be18edd fix(upstream): P2-13 凭据冷却数据竞争—— 锁内取快照，锁外不再读共享字段`
and the file `internal/upstream/cooldown_race_test.go` exist solely because of it.
That test is a *hand-constructed* interleaving using a `concurrencyStore` double —
it asserts a specific ordering, but `go test` without `-race` cannot detect any
race the author did not think to model. The concurrency surfaces that remain
unverified are exactly the ones that are easy to get wrong:

- `runtimeReloader.dirty` / `dirtySince` are `atomic.Bool` / `atomic.Int64`
  (`main.go:648-652`) accessed from both the request goroutines (`MarkDirty`) and the
  retry goroutine — correct today, but nothing keeps it correct.
- `statusResponseWriter.mu` guards `statusCode`/`written` shared between the
  streaming heartbeat goroutine and the main loop (`internal/server/server.go:60-65`)
  — a classic place for a race to hide.
- `usageRecorder.queue` closed by `wait` while `record` sends (see the P1 above) —
  a race detector run under a shutdown test would have surfaced the ordering hazard
  far earlier than reasoning did.

**Concrete fix** — add a second, race-enabled pass. Keep it cheap by scoping it:

```yaml
      - name: 全量测试（竞态检测）
        run: go test -race -count=1 -timeout 600s ./internal/upstream/ ./internal/server/ ./internal/userauth/ ./internal/store/ ./internal/ratelimit/
```

or, simpler and more honest, change line 57 to `go test -race -count=1 -timeout 600s
./internal/... ./cmd/gateway/` and let it run every time. Note this requires CGO on the
runner (it works on `ubuntu-latest`; it does **not** work in this Windows checkout —
`go test -race` there fails at link time with
`cannot find C:/Program Files/mingw64/.../default-manifest.o`, i.e. an environment
issue, not a code one).

**Confidence** — high for the gap; the downstream claim ("a race exists") is *not*
made — this finding is about the missing guardrail, not a proven bug.

---

### [P2] Release images are built and pushed without any test gate

**Location** — `.github/workflows/docker.yml:20-97` (whole job), trigger at
`docker.yml:6-10`.

**What's wrong** — `docker.yml` triggers on `push: tags: ['v*.*.*']` and on
`workflow_dispatch`. It checks out, resolves the version from `web/package.json`,
verifies tag/version agreement, and runs `docker/build-push-action` with
`push: true`. It never calls `needs:`, never runs `go test`, `go vet`, `gofmt`, or
the frontend typecheck, and does not depend on the `ci` workflow.

The tag/version check (`docker.yml:36-46`) is a genuine and useful gate — it is the
one thing this workflow actually enforces.

**Why it matters** — the images that operators actually deploy are produced by a
workflow that is independent of the one that verifies the code. Nothing in the repo
prevents tagging `v1.4.4` on a commit where tests are red, or where
`internal/webui/dist` is stale, and having that image published to GHCR under the
`:latest` and `1.4.4` tags. The `webui-sync` job in `ci.yml` is exactly the guard
that would catch a stale-UI release, and it has no path to block this one.

**Concrete fix** — the two workflows are in separate files so they cannot share a
`needs:`; merge the release into `ci.yml` as a job, or at minimum make `docker.yml`
run the Go gate before building:

```yaml
      - name: Set up Go
        uses: actions/setup-go@7b8cf10d4e4a01d4992d18a89f4d7dc5a3e6d6f4
        with:
          go-version-file: go.mod
          cache: true
      - run: go build ./... && go vet ./...
      - run: go test -count=1 -timeout 300s ./internal/... ./cmd/gateway/
```

placed before the build-and-push step. Optionally require the tag to exist on a commit
that has a green `ci` run (via the GitHub API) for true release gating.

**Confidence** — high (read both workflows end to end; no `needs:` anywhere).

---

### [P2] `.zcodeignore` still describes the deleted admin credential channels and lacks the anchoring that `.gitignore` learned the hard way

**Location** — `.zcodeignore:20` (`admin_auth.json`), `.zcodeignore:29` (comment
naming `admin_token`), `.zcodeignore:2-3, 11-12, 23, 34` (unanchored patterns).

**What's wrong** — the security fix that removed `admin_token` and
`admin_auth.json` updated `.gitignore`'s reasoning but left this sibling file
describing the old world:

```
# 管理后台密码（含盐哈希，不应入库）
admin_auth.json
...
# 本地实际配置（含 admin_token / providers api_key / routes）。
config.json
```

Two concrete problems:

1. `admin_auth.json` is listed as a live artifact to ignore. It can no longer be
   produced by this codebase, so the rule is dead weight that implies the feature
   still exists.
2. Unlike `.gitignore`, these directory patterns are **unanchored**: `bin/`,
   `web/node_modules/`, `gateway/`, `data/`. `.gitignore:2-5` carries an explicit
   scar-tissue comment about exactly this bug — *"`gateway/` 曾把 `cmd/gateway/`
   整个排除在版本控制之外，导致 billing.go 等 7 个文件从未入库"* — and fixed it by
   writing `/gateway/`. `.zcodeignore` never received the same fix and still has the
   bare `gateway/` on line 23.

**Why it matters** — `.zcodeignore` is not git, so the `gateway/` rule cannot cause
the "files never committed" failure mode directly; it makes the file inconsistent
with its sibling and with reality, and it misleads any tooling or reviewer that reads
it as a description of what the project still contains. The genuine defect is drift:
a file that enumerates `admin_auth.json` and `admin_token` as current concepts,
three weeks after both were deleted.

**Concrete fix** — delete the `admin_auth.json` block (lines 19-20), reword the
`config.json` comment to drop `admin_token`, and add leading slashes to the directory
rules so the two ignore files agree:

```
/bin/
/web/node_modules/
/web/dist/
/gateway/
/data/
```

**Confidence** — high for the drift and the unanchored patterns; I did not verify
which tool consumes `.zcodeignore`, so I am not claiming a concrete exploit path
through it.

---

### [P3] README tells developers they need Go 1.22; `go.mod` requires 1.27

**Location** — `README.md:47` vs `go.mod:3` (and `Dockerfile:9`).

**What's wrong** — the build instructions open with:

> 前置 Go 1.22+。前端产物已入库（`internal/webui/dist`），只改后端无需 Node：

but `go.mod:3` declares `go 1.27.0`, and `Dockerfile:9` pins `ARG GO_VERSION=1.27`.
A developer on Go 1.22 following the README will hit the toolchain's refusal to
build (or, with `GOTOOLCHAIN=auto`, an unprompted 1.27 download).

Note that CI is *not* affected: `ci.yml:38` uses `go-version-file: go.mod`, so CI
correctly derives 1.27 from the single source of truth. The README is the only place
that hard-codes a stale minimum.

**Why it matters** — minor, but it is exactly the class of drift where the "single
source of truth" discipline (which this repo applies carefully everywhere else —
`build.ps1:93-100` reads the version from `web/package.json`, `vite.config.ts:17-22`
injects `__APP_VERSION__`/`__ROSETTA_VERSION__`) is broken in the one place a new
developer reads first.

**Concrete fix** — either delete the version and let `go.mod` speak (preferred):

> 前置 Go 工具链（版本见 `go.mod` 的 `go` 指令）。前端产物已入库…

or state the real minimum: `前置 Go 1.27+`.

**Confidence** — high.

---

### [P3] `internal/webui/embed.go` is checked out with CRLF, which is exactly what CI's `gofmt` gate rejects

**Location** — `internal/webui/embed.go` (whole file, 10 lines / 284 bytes), gate at
`.github/workflows/ci.yml:41-47`.

**What's wrong** — running the CI's own command locally:

```
$ gofmt -l cmd/ internal/
internal\webui\embed.go
```

The diff is a whole-file rewrite, which is the signature of line-ending
normalization, not a formatting change. Byte-level inspection confirms all 10 line
endings are CRLF (10 CRLF, 0 bare LF) while the other 100+ `.go` files in the repo
are pure LF — this file is the sole outlier.

`.gitattributes:33` already declares `*.go text eol=lf`, and `git check-attr`
confirms `internal/webui/embed.go: text: set, eol: lf`. So **the blob in the git
index is LF** and CI, which checks out on `ubuntu-latest`, will see an LF file and
pass. This is a local-checkout-only condition.

**Why it matters** — low in CI, but it is a live trap for anyone running
`.\build.ps1 -Vet` or a local `gofmt -l` on Windows: the file is the single thing
that makes the local format check disagree with CI, which is the exact class of
"works locally, red in CI" confusion the repo's `.gitattributes:30-32` comment warns
about. The usual cause is an editor writing CRLF over a file that predates the
attribute, or `core.autocrlf=true` interacting with a file added before the rule.

**Concrete fix** — renormalize once, which is what the attribute exists for:

```
git add --renormalize internal/webui/embed.go
git commit -m "chore: renormalize embed.go line endings to LF"
```

then confirm with `gofmt -l cmd/ internal/` returning nothing. Worth pairing with a
pre-commit hook or `git config core.autocrlf input` on Windows checkouts.

**Confidence** — high that the local file is CRLF and CI would see LF; I verified the
index blob is LF, which is why I am rating this P3 rather than a CI break.

---

### [P3] CI test scope omits `cmd/migrate-legacy`

**Location** — `.github/workflows/ci.yml:57`.

**What's wrong** — `go list ./...` returns 17 packages including
`github.com/cn-maul/rosetta-gateway/cmd/migrate-legacy`, but the test step is scoped
to `./internal/... ./cmd/gateway/`. The migration tool is therefore never `go test`ed.

**Why it matters** — low today, because `cmd/migrate-legacy/` contains exactly one
file (`main.go`, 11928 bytes) and **zero** `_test.go` files, so there is currently
nothing to run. But `go build ./...` on `ci.yml:51` does compile it, so it is not
silently broken — it just gets no future test coverage without anyone noticing. The
scope was almost certainly written before this package existed; a hand-maintained
package list drifting from `go list ./...` is the kind of thing that quietly rots.

**Concrete fix** — drop the package list entirely so new packages are covered
automatically:

```yaml
run: go test -count=1 -timeout 300s ./...
```

`./...` covers every test-bearing package today (verified: all 13 directories
containing `*_test.go` are under `./internal/...` or `./cmd/gateway/`) and keeps
covering them as packages are added. This also removes the need for the `-count=1`
rationale comment's implicit assumption that the list is complete.

**Confidence** — high (verified zero test files in `cmd/migrate-legacy`, and that all
test-bearing dirs are inside the current scope).

---

### [P3] `gateway.ps1` health check and `-Stop` port fallback assume loopback and can act on an unrelated process

**Location** — `gateway.ps1:242` (health check), `gateway.ps1:100-106` and
`gateway.ps1:195-200` (port-based kill).

**What's wrong** — two related assumptions:

1. The health check is hardcoded to loopback while the port is read from config:

```powershell
$null = Invoke-WebRequest -Uri "http://127.0.0.1:$Port/admin/" -UseBasicParsing -TimeoutSec 5
```

`$Port` is correctly parsed from `listen` including IPv6 (`gateway.ps1:57-63`), but
the *host* is hardcoded. A config with `listen: "192.168.1.50:8666"` (a single
non-loopback address, a normal thing to write on a multi-homed host) means the
gateway binds only that address and `127.0.0.1:8666` never answers — the script then
prints `[!] 健康检查未通过` and tells the operator the gateway failed to start,
when it is running correctly.

2. `-Stop` and the pre-launch cleanup both fall back to *"kill whatever holds the
   port"*:

```powershell
$lp = Get-ListenerPid $Port
if ($lp -and $lp -ne $PID) {
  Write-Host "-> 端口 $Port 被占用（PID $lp），先停止旧进程"
  Stop-Process -Id $lp -Force -ErrorAction SilentlyContinue
```

There is no verification that the PID actually is this project's gateway — no
command-line or path check — so pointing the config at a port that some *other*
service occupies causes `.\gateway.ps1` to kill that unrelated process, silently and
with `-Force`. The same happens on a deliberate `-Stop` (`gateway.ps1:100-106`).

**Why it matters** — the loopback assumption produces a false alarm on a legitimate
configuration; the port fallback is a genuine "kills something you didn't mean to"
foot-gun, and it is the kind of thing that gets discovered the hard way when someone
points a dev config at a shared port.

**Concrete fix** — derive the health-check host from the parsed `listen` instead of
hardcoding it (parse the host portion the same way the port is parsed, defaulting to
`127.0.0.1`), and gate the port fallback on identity rather than port alone:

```powershell
$proc = Get-Process -Id $lp -ErrorAction SilentlyContinue
if ($proc -and $proc.Path -and ($proc.Path -eq $Bin -or $proc.ProcessName -eq 'gateway')) {
  Stop-Process -Id $lp -Force
} else {
  Write-Host "[X] 端口 $Port 被非网关进程（PID $lp, $($proc.ProcessName)）占用，请手工处理。" -ForegroundColor Red
  exit 1
}
```

**Confidence** — high for the hardcoded loopback (read directly); medium for the
unintended-kill scenario, since it depends on the operator pointing the config at a
foreign port.

---

## Verified clean (no issues found)

- **Embedded `internal/webui/dist` is NOT stale.** This was the highest-risk item in
  scope, so it was checked the way CI checks it rather than by inspection: ran
  `npm run build && node sync-embed.mjs` and then `git diff --exit-code --
  internal/webui/dist` → exit 0. The rebuild is byte-identical to the committed
  artifact, with matching content hashes (`index-ITvrlvTg.js`, `Keys-C6DhrOms.js`,
  `Users-1w0tbks7.js`, `Models-DAkpWwBB.js`, …). Spot-checked that the newest
  features (可用模型 page, 余额充值 modal, 新建密钥 flow) are present in the
  committed bundles. `git log` shows `web/src` and `internal/webui/dist` last touched
  by the same commit `d529b29`. All 31 dist files are tracked and `git check-ignore`
  confirms they are not ignored. Working tree left clean afterwards.
- **CI's dist-drift guard is real and correctly written.** `ci.yml:89-97` uses
  `set -euo pipefail`, `git diff --exit-code -- internal/webui/dist`, and a
  non-zero `exit 1` — it cannot silently pass. `go.mod`/`package.json` are LF-pinned
  by `.gitattributes:34-35`, so the two `define` values that `vite.config.ts`
  injects (`__APP_VERSION__` from `package.json`, `__ROSETTA_VERSION__` parsed from
  `../go.mod`) are identical on Windows and ubuntu — the cross-platform
  false-diff trap described at `.gitattributes:15-24` is genuinely closed.
- **Dockerfile is sound.** Multi-stage with a separate runtime layer, `CGO_ENABLED=0`
  static build (`Dockerfile:35-38`), dependency layer cached separately
  (`:26-27`), version injected by `-ldflags` from a build-arg, non-root via
  `su-exec` in the entrypoint, `EXPOSE 8666` with a documented rationale, and a
  `HEALTHCHECK` probing `/admin/` (embed-only, no DB, no auth → deterministic 200).
- **Signal handling is correct for PID 1.** Both branches of
  `docker/docker-entrypoint.sh` `exec` the gateway (`:50` via `su-exec`, `:63`
  directly) rather than leaving a shell in the foreground, so SIGTERM from
  `docker stop` reaches the Go process, which handles it at `main.go:587-589`.
- **`main.go` reload-retry loop is well built.** `watchReloadRetry`
  (`main.go:698-740`) polls a dirty flag rather than a connection, bounds each
  attempt with `context.WithTimeout(ctx, reloadTimeout)` (`:719`), derives
  backoff from the *observed* failure count with a cap (`:727-730`), resets on
  success (`:723`), and returns on `ctx.Done()`. `Reload` (`:747-799`) holds `mu`
  for the whole two-phase build and only calls `markClean()` after both the pool and
  the snapshot are installed (`:797`) — so every `return err` path correctly leaves
  the dirty flag set for the retry loop. Map iteration into `failures` is
  explicitly sorted (`:779`) to avoid order-dependence. Logging is nil-guarded
  (`:783`) precisely because that branch is the one that must never panic.
- **Graceful shutdown ordering (apart from the P1 above) is deliberate and
  correct.** `close(shutdown)` before `db.Close()` stops the prune loop first
  (`main.go:596`), the wait is bounded at 5s (`:597-601`), and `srv.Shutdown` has a
  10s budget with in-flight usage drained afterwards (`:604-614`).
- **No SQL injection.** All dynamic SQL in `internal/store` concatenates
  *compile-time constants* only — column lists (`providerColumns`, `keyColumns`, …),
  fixed fragments, and table names (`tx.go:30` takes a literal, documented as such at
  `:28`). Every user-supplied value goes through a `?` placeholder, including the
  scope-narrowing fragment (`usage_dao.go:384-389`). `fmt.Sprintf` is not used to
  build statements.
- **Default bind address is safe.** `config.setDefaults` sets
  `listen: "127.0.0.1:8666"` (`config.go:145`) with a comment explaining that the
  pre-password bootstrap window makes `0.0.0.0` dangerous. The container template
  (`docker/config.default.json:2`) is `0.0.0.0:8666`, which is the intended and
  documented exception — and `main.go:256-265` raises a loud `SECURITY:` ERROR when
  it finds *uninitialised admin + non-loopback listen*.
- **No default credentials or secrets in shipped config.** `config.example.json` and
  `docker/config.default.json` both carry empty `bootstrap.providers`/`routes` and
  reference keys only by *env var name*. The master key is refused-if-missing rather
  than defaulted (`main.go:147-156`, deliberately refusing to start so credentials
  are never written in plaintext).
- **Environment-variable naming is consistent** between code and docs:
  `ROSETTA_GW_HOME` (`main.go:46`), `ROSETTA_GW_MASTER_KEY`
  (`crypto.go:26`, `config.go:93`), `ROSETTA_GW_SESSION_SECRET`
  (`userauth/session.go:26`) — all three match the DOCKER.md table at `:146-151`,
  and no `os.Getenv` call in the repo references a name absent from the docs.
- **Crypto primitives are sound.** AES-256-GCM with a fresh `io.ReadFull(rand.Reader)`
  nonce per encryption (`crypto.go:213-218`), 0600 perms and atomic tmp→fsync→
  rename→dir-fsync writes for both key files (`crypto.go:114-153`,
  `userauth/session.go`), explicit guards against silently adopting a truncated key
  (`crypto.go:62-79`), and a throttled WARN when a credential is accepted as
  plaintext by the legacy fallback (`crypto.go:177-184`).
- **Panics in request paths are handled.** `server.Recovery` wraps the mux
  (`main.go:516`), and `writeInternalError` correctly checks `Committed()` before
  writing so a post-header panic cannot inject JSON into an SSE stream
  (`server.go:456-462`). Config validation hard-rejects negative/overflowing
  durations precisely to avoid `time.NewTicker` panics at request time
  (`config.go:181-226`).
- **Duration math uses monotonic time.** `time.Since(start)` (`server.go:51`),
  `time.Since(time.UnixMilli(since))` (`main.go:713`), and `time.Until(e.until)`
  (`server.go:297`) all subtract `time.Time` values, so the monotonic component is
  used and wall-clock adjustments cannot produce negative or absurd durations.
- **Docs already updated for the admin-credential removal.** `DOCKER.md` and
  `README.md` contain **zero** references to `admin_token` / `admin_auth.json`.
  `DESIGN.md:439-440, 976, 1116-1125` and `MULTIUSER.md:9, 589-614` each carry an
  explicit "已整体删除 / 已被推翻" note at the point of mention. `MULTIUSER.md:613`
  even says the retained original text "仅作设计演进的历史记录保留" — the stale-looking
  §1 table (`:51`) and §3.5 body (`:620-648`) are inside clearly-labelled historical
  sections that the doc's own status line at `:9` flags as overturned. I checked for
  the case where a doc would *actively mislead* an operator and did not find one;
  the only file that still describes these as live is `.zcodeignore` (reported above).
