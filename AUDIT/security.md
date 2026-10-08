# Security Audit — Authentication, Session, Multi-user RBAC & Permission Boundaries

**Scope:** `internal/server/user_auth.go`, `internal/server/server.go` (auth sections), `internal/server/autoreload.go`, `internal/userauth/*`, `internal/admin/user_handler.go`, `internal/admin/user_admin_handler.go`, `internal/store/user_dao.go`, and the auth/route-wiring section of `cmd/gateway/main.go` (≈L260–460). IDOR/ownership tracing followed into `internal/admin/key_handler.go`, `usage_handler.go`, `stats_handler.go`, `group_handler.go` because those handlers are reachable through the normal-user prefix whitelist.

---

### [P1] Change-password throttle leaks a concurrency slot on every success → permanent 429 lockout on password rotation

**Location:** `internal/admin/user_admin_handler.go:664-681` (missing release) + `internal/server/server.go:338-353` (`Success` does not decrement `inflight`)

**What's wrong**

`FailureThrottle.Allow` increments a per-key `inflight` counter (`server.go:261,271`) that represents "requests currently running a KDF", and the cap `maxConcurrentAttempts = 4` (`server.go:277`) rejects the 5th concurrent attempt with a 429. Two of the three paths that call `Allow` balance the counter with `defer Release`:

- `Login` → `internal/admin/user_handler.go:159` (`defer th.Release(ip)`)
- `BootstrapSetup` → `internal/admin/user_handler.go:434` (`defer th.Release(ip)`)
- session middleware → `internal/server/user_auth.go:184` (`defer a.throttle.Release(ip)`)

`ChangePassword` calls `thr.Allow(u.ID)` (`user_admin_handler.go:665`) but **never** calls `thr.Release(u.ID)` and has no `defer`. Only `Fail` (`server.go:326-328`) and `Release` (`server.go:284-285`) decrement `inflight`. On the old-password-verified path the handler calls `thr.Success(u.ID)` (`user_admin_handler.go:676`), and `Success` only resets `count` and `until` — it explicitly does **not** touch `inflight`, and it keeps the map entry alive when `inflight > 0` (`server.go:350-352`). Net effect: **each successful old-password check permanently increments `inflight` by one and never decrements it.**

The easiest trigger needs no password change at all. If the old password verifies and the *new* password is weak, `HashPassword` fails at line 677-681 and the handler returns 400 — but `Success` already ran at line 676, so the slot is already leaked. Do that four times and `inflight == 4`.

Because this throttle is keyed by **user ID** (`user_admin_handler.go:665`) rather than IP or session, the leaked counter survives logout, re-login, and even the `auth_version` bump that `SetUserPassword` causes. At that point `Allow` hits `if e.inflight >= maxConcurrentAttempts { return false }` (`server.go:268-270`) and `ChangePassword` returns 429 (`user_admin_handler.go:667`) **permanently** for that account. `sweepLocked` (`server.go:358-368`) only prunes when the map holds ≥4096 entries and only entries whose `until` has passed; `until` is zero for this entry, so under any realistic load the poisoned entry is never reclaimed.

Note the asymmetry is easy to miss because `Success` keeping the entry (rather than deleting it) is itself a deliberate concurrency fix — the comment at `server.go:345-347` explains exactly why the entry is retained when other attempts are in flight. That reasoning is correct for `Release`-paired callers; it silently breaks for the one caller that never releases.

**Why it matters**

Password rotation is the self-service recovery path for a user whose password was changed by an admin, and it is the endpoint the "stolen session" defense (`user_admin_handler.go:656-663`) is protecting. A permanently-429 endpoint means a user who fat-fingers the new-password strength rule four times can no longer change their own password through the admin panel at all, with a message that says only "原密码尝试过于频繁" — misleading, since no failed old-password attempt ever occurred. The only recovery is another admin resetting the password out-of-band.

**Concrete fix**

Make the release unconditional so every exit path balances the counter, exactly as the sibling endpoints do:

```go
	thr := h.throttleForPassword()
	if !thr.Allow(u.ID) {
		w.Header().Set("Retry-After", strconv.Itoa(int(thr.RetryAfter(u.ID).Seconds())+1))
		writeError(w, http.StatusTooManyRequests, "原密码尝试过于频繁，请稍后再试")
		return
	}
	// 归还 Allow 占用的并发额度。Success **不**递减 inflight（见 server.go 的注释），
	// 漏了这一行会让每次「旧密码正确」的校验永久泄漏一个并发槽：4 次之后
	// inflight 达到 maxConcurrentAttempts，本端点对该用户永久 429，
	// 且按 user ID 归键 —— 跨会话、跨登出都不会复位。
	defer thr.Release(u.ID)

	if !userauth.VerifyPassword(u.PasswordHash, req.OldPassword) {
		thr.Fail(u.ID)
		writeError(w, http.StatusUnauthorized, "原密码不正确")
		return
	}
	thr.Success(u.ID)
```

(`Release` is safe to pair with `Fail`/`Success`: both floor at zero via `if e.inflight > 0`, and `Release` explicitly documents that a late call after `Success` deletes the entry is a no-op — `server.go:279-287`.)

Regression test worth adding in `internal/admin/user_admin_handler_test.go`, exercising four verify-succeeds-then-weak-new-password requests followed by a fifth valid request, asserting the fifth is not 429.

**Confidence:** high — traced the full increment/decrement accounting; `Release(` appears in exactly three call sites repo-wide (confirmed by grep) and none of them is `ChangePassword`.

---

## Sub-areas checked and found clean

- **Privilege escalation / whitelist prefix matching** — `userAccessiblePrefixes` (`internal/server/user_auth.go:132-142`) is `/admin/api/{keys,usage,stats,me,logout,model-names}`. I enumerated every route registered in `cmd/gateway/main.go:319-401` and checked each against those prefixes. No admin-only route is shadowed by a whitelist prefix. The two in-prefix admin-only endpoints are both guarded inside their handlers, not by the prefix: `POST /admin/api/usage/prune` (`usage_handler.go:682 requireAdmin`) and the config-export routes, which live under `/admin/api/config-export/` and therefore match no whitelist prefix at all. The whitelist direction is genuinely fail-safe: a future route under e.g. `/admin/api/stats-admin/...` would be *more* restrictive, and a *new* sub-route is explicitly denied to normal users until whitelisted.
- **IDOR on `/admin/api/keys/*`** — all four mutating paths re-check ownership, not just GET: `Update` (`key_handler.go:551`), `Delete` (`key_handler.go:679`), `RecomputeUsage` (`key_handler.go:706`) each call `ownedByCaller` before any write. `Create` forces `owner := me.ID` and silently ignores `req.UserID` (`key_handler.go:269-280`). The `user_id` reassignment path (`key_handler.go:629-646`) is gated on `me.IsAdmin()` and validated against a real user. Privilege-sensitive fields are covered: `group_id` override is admin-only on **both** set and clear (`applyP2`, `key_handler.go:351-354`), `guardNoLoosening` (`key_handler.go:403-434`) blocks loosening `enabled`/`quota`/`rpm`/`tpm`/`expires_at`/`allowed_ips`, and `Create` caps quota at the user-level quota and force-zeroes rpm/tpm for non-admins (`key_handler.go:236-254`). `ownedByCaller` fails closed on nil and on empty ID (`key_handler.go:516-528`) and returns 404 rather than 403 for another user's key, avoiding an existence oracle.
- **IDOR on `/admin/api/usage/*` and `/admin/api/stats`** — all seven read paths (`Query`, `by-key`, `by-model`, `by-provider`, `by-day`, `history`, `history.csv`) funnel through `callerScope` (`usage_handler.go:180-197`) into the single WHERE-construction points `usageFilter` (`usage_handler.go:132`) and `usageHistoryFilters` (`usage_handler.go:422`). `callerScope` fails closed: nil identity and empty ID both return the `"\x00none"` sentinel rather than `""` (admin = unrestricted), so an un-injected context cannot become a full-data pass. `stats` uses the same scope and additionally zeroes `cost` for non-admins (`stats_handler.go:109,138`) and gates lifetime totals on `scope == ""` (`stats_handler.go:145`). Explicit `?key_id=` of someone else's key yields an empty set, not their rows (AND-composed scoping).
- **JWT / alg confusion** — `Issue` hardcodes `jwt.SigningMethodHS256` (`session.go:198`); both `Verify` and `Peek` pass `jwt.WithValidMethods(["HS256"])` **and** assert `*jwt.SigningMethodHMAC` in the keyfunc (`session.go:232-241`, `295-300`). No `none`, no RS256→HMAC confusion. `subject != uid` is rejected (`session.go:252-255`). Session secret is HMAC-SHA256 with a 32-character floor, loaded from env or an atomically-written 0600 file, and `NewManager` returns `nil` rather than degrading to an empty key (`session.go:71-97`, `119-160`); `main.go:297-306` refuses to start if sessions are unavailable. No secret material in logs.
- **auth_version enforcement / logout revocation** — the `Peek → GetUser → Verify(token, u.AuthVersion)` ordering is correct and is the right way round (`user_auth.go:197-228`); `Verify` rejects `currentVersion < 0` and any mismatch (`session.go:259-261`). `store.UpdateUser` deliberately never writes `auth_version`, so the PATCH path cannot resurrect a stolen token from a stale snapshot, and role/status changes go through the atomic relative `BumpAuthVersion` (`store/user_dao.go:246-255,325-330`) — the read-old-write-old race the comments warn about is genuinely closed. Logout now bumps `auth_version`, so it revokes server-side, not just the local cookie (`user_handler.go:510-530`). Cookie `MaxAge` is derived from the JWT `exp` (`user_auth.go:260-272`), matching the `SessionTTL`/8h invariant.
- **Password hashing** — PBKDF2-HMAC-SHA256 at 210k iterations, 16-byte random salt, self-describing encoding, constant-time compare via `subtle.ConstantTimeCompare` (`userauth/password.go:87-142`). No dictionary/entropy check, but that is a deliberate and reasonable scope decision for a deployer-configured system secret.
- **User enumeration** — `Login` returns an identical status and message for "no such user" and "wrong password" and runs a real KDF on the miss path via `dummyHash` so the two are timing-equivalent (`user_handler.go:196-212`). The one differentiated message is narrowed to `role=admin && password_hash==""` only (`user_handler.go:186-194`). Username lookup and the table's unique constraint are both `COLLATE NOCASE` (`store/user_dao.go:149-157`, `store/store.go:187`), so case cannot be used to slip past uniqueness or to shadow another login name.
- **CSRF** — no exploitable gap found. The session cookie is `HttpOnly` + `SameSite=Strict` + `Path=/admin` (`user_auth.go:261-271`), which alone blocks the cookie from being attached to any cross-site request. Independently, every state-changing handler with a body routes through `decodeJSON`, which rejects anything that is not `application/json`/`application/*+json` and treats a missing `Content-Type` as a rejection (`admin/helpers.go:117-148`) — so a `text/plain` simple request cannot be used even if `SameSite` were `Lax`. `/admin` responses carry no CORS headers at all (`server.go:124-144`), so a preflight is never satisfiable. The unauthenticated write endpoint `POST /admin/api/bootstrap` adds an explicit `SameOrigin` check as its very first statement (`user_handler.go:384-387`). `POST /admin/api/logout` is bodyless and takes the cookie, but `SameSite=Strict` covers it.
- **`sameOrigin` semantics** — prefers the unforgeable `Sec-Fetch-Site` header, falls back to `Origin`, and treats "neither header present" as same-origin for non-browser clients (`server.go:161-174`). Rejecting-by-default on a present-but-unparseable `Origin` is the right conservative choice.
- **Secret / PII leakage** — `userResponse` deliberately omits `password_hash` and exposes only `HasPassword` (`user_admin_handler.go:35-64,84`). The audit trail records only top-level **field names**, never values (`server/autoreload.go:145-156`), so `api_key`/`password` plaintext never reaches `audit_log`; the `AutoReload` doc comment states this explicitly (`autoreload.go:29`). `writeServerError` no longer echoes raw SQL errors, constraint names, or credential-file paths to the client (`admin/helpers.go:41-50`). `bootstrap` gets audit-without-reload via `AuditOnly` (`autoreload.go:169-197`, wired at `main.go:451`), which was otherwise a zero-audit hole. `clientIP` deliberately ignores `X-Forwarded-For` so the throttle key cannot be forged (`server.go:198-204`).
- **Throttle accounting (otherwise)** — the `until`-is-zero-value bug class is correctly handled (`Fail` at `server.go:319-324` resets after cooldown; the comment documents the exact historical regression), `Success` correctly refuses to delete an entry with in-flight attempts (`server.go:345-352`), and anonymous requests are deliberately not counted (`user_auth.go:186-195`) to avoid a NAT-wide self-DoS. The concurrency cap `maxConcurrentAttempts = 4` is a sane amplifier guard against N-fold PBKDF2 blowup, and it is not itself attacker-triggerable against a *different* victim: the key is `RemoteAddr`, not a forwarded header. The one genuine accounting hole is the P1 above.
- **Error paths failing open** — the notable candidate, `Me` degrading a failed balance read to "unlimited" (`user_handler.go:571-577`), is display-only by construction and the real enforcement point (`store.BalanceOf` on the charge path) is unaffected; it is documented and correct. `callerScope`, `ownedByCaller`, `requireAdmin` (`user_admin_handler.go:93-109`), `ModelNames` (`group_handler.go:311-314`), and the two `requireAdmin` blocks in `List`/`Create` all fail closed. `Userauth.Verify`/`Peek` and `Enabled()` return errors rather than permissive defaults.

## Accepted risk (documented, not re-reported)

The fresh-deployment bootstrap window on a non-loopback listener is pre-admitted. Reviewing it for anything worse than documented: the window is now gated by a one-shot `bootstrap_completed` marker written in the **same transaction** as the password write (`store/user_dao.go:287-317`), the write is a conditional `UPDATE ... AND password_hash='' AND role='admin'` so concurrent double-setup yields 409 rather than last-writer-wins, `CreateUser` refuses empty-password admins at the source (`user_admin_handler.go:246-249`), and `UpdateUser` refuses to promote a passwordless account to admin (`user_admin_handler.go:402-406`). I found no path that reopens the window after completion. Severity matches what is documented — no finding raised.