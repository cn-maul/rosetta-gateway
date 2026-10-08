# Frontend Audit — Rosetta Gateway admin panel

Scope: `web/src/**` (Vue 3 + TypeScript, Vite build), `web/index.html`, `web/vite.config.ts`,
`web/public/theme-init.js`, cross-checked against `cmd/gateway/main.go` and `internal/admin/*.go`,
`internal/server/server.go`.

**Known context (confirmed, not re-reported as new):** the admin panel intentionally relies on the
server for authorization; client-side `isAdmin()` only hides UI. Confirmed intact and correct:
`api.ts` stores the token in `sessionStorage` (not `localStorage`) with a documented rationale
(`api.ts:27-51`); `router.ts:45-74` correctly ignores `session.checked === false` during bootstrap;
the guard re-application race (`App.vue:192-199` watcher + `router.ts:94-98 reapplyGuard`) is handled
and documented. Extending the known-context items:

- **Token blast radius (extends known context):** no `v-html` / `innerHTML` / `eval` / `new Function`
  exists anywhere in `web/src` (verified by grep), and the CSP is `script-src 'self'` with
  `object-src 'none'`, so XSS injection is currently not the exfiltration vector. The residual risk
  is a supply-chain XSS in a dependency, which `sessionStorage` limits to the lifetime of one tab.
  No new finding here.
- **`isAdmin()` UI-only hiding (extends known context):** every admin-only control I checked is
  *also* enforced server-side (`requireAdmin` / `callerIsAdmin` on every write handler), so
  hiding-only does not create a false sense of security. The one place where the UI **implies** a
  permission the caller does not have is the Keys edit dialog, filed as §[P1] below: it renders
  `quota_tokens` / `rpm_limit` / `tpm_limit` as editable inputs for non-admins, but
  `key_handler.go:403-434 guardNoLoosening` will 403 any widening edit. The create dialog on the same
  page gets this right (`:161-167`, `:478-485`), so the edit path is an asymmetric omission.

---

### [P1] `History.vue` — `exportCSV` sets its `exporting` guard *after* a pre-commit `await load()`, leaving a double-submit window

**Location:** `web/src/views/History.vue:184-215` (`exportCSV`), with `filters()` at `:38-44` and `load()` at `:115-121`.

**What's wrong:**
```ts
async function exportCSV() {
  if (exporting.value) return
  if (fModel.value.trim() !== appliedModel.value.trim()) {
    page.value = 1
    await load()          // ← commits fModel into appliedModel and fetches
  }
  exporting.value = true
  exportNotice.value = ''
  try {
    const r = await api.exportUsageCSV(days.value, filters())   // ← reads current draft refs
```
When the model draft differs from `appliedModel`, `exportCSV` calls `await load()` first (which sets
`appliedModel.value = fModel.value` inside `load()`), then reads `filters()`. That part is correct —
the filter values themselves are read fresh, so no stale filter is sent.

The defect is the **ordering of the busy flag**. `exporting.value = true` is only set *after* that
`await load()` returns. `load()` is a full `GET /usage/history` round-trip (up to the 30s request
timeout in `api.ts:193`), and during that window `if (exporting.value) return` does not fire, so a
second click on "导出 CSV" starts a concurrent `exportCSV()`. Both reach
`api.exportUsageCSV`, producing two overlapping blob downloads and two `Content-Disposition`
`<a download>` clicks (`api.ts:318-331`), while `exportNotice`/`toast` (single-slot, `ui.ts:15-20`)
are written twice and whichever `finally` runs first clears `exporting` while the other export is
still running — so the "导出中…" label lies and the truncation notice for one download can overwrite
the other. This is the one export path in the app where the busy flag is not set before the first
`await`.

Compare `Settings.vue` `doExport` (`transferBusy` set at `:98` before its first `await`) and
`Users.vue`'s `pending` map, which both set the guard up front.

**Why it matters:** Two simultaneous blob downloads + two `Content-Disposition` `<a download>`
clicks (see `api.ts:318-331`) can race the `URL.revokeObjectURL` and the toast singleton
(`ui.ts:15-20`, single-slot), and can leave `exporting=false` while an export is still running —
the "导出中…" label lies. For the CSV path this can also produce two files where the user expects
one, and the truncation notice for one may be overwritten by the other's toast.

**Concrete fix:** set the busy guard before the pre-commit `await`, and keep it across both phases.
```ts
async function exportCSV() {
  if (exporting.value) return
  exporting.value = true
  exportNotice.value = ''
  try {
    // 输入框里还有没提交的模型名时先落盘再导出（见原注释）
    if (fModel.value.trim() !== appliedModel.value.trim()) {
      page.value = 1
      await load()
    }
    const r = await api.exportUsageCSV(days.value, filters())
    if (r.truncated) {
      /* … unchanged … */
    } else {
      toast('CSV 已开始下载')
    }
  } catch (e) {
    if ((e as { status?: number }).status !== 401) toast('导出失败：' + (e as Error).message, 'err')
  } finally {
    exporting.value = false
  }
}
```

**Confidence:** high.

---

### [P1] `Keys.vue` — edit dialog always sends `quota_tokens` / `rpm_limit` / `tpm_limit`, which `guardNoLoosening` rejects for a normal user editing their own key

**Location:** `web/src/views/Keys.vue:240-265` (`submitEdit`), template edit dialog `:544-558`.

**What's wrong:**
```ts
await api.updateKey(eForm.id, {
  name: eForm.name.trim(),
  quota_tokens: Number(eForm.quota) || 0,   // always sent
  rpm_limit:     Number(eForm.rpm)  || 0,   // always sent
  tpm_limit:     Number(eForm.tpm)  || 0,   // always sent
  allowed_models: eForm.models,
  …
```
`quota_tokens` / `rpm_limit` / `tpm_limit` are the only fields in the edit dialog that are **not**
wrapped in `isAdmin()`. Compare the create dialog, which correctly gates them (`Keys.vue:161-167`),
and compare `submitCreate`, which also gates them. On the backend,
`key_handler.go:403-434 guardNoLoosening` rejects these for a non-admin caller whenever the value
would widen (`canTightenQuota` / `canTightenLimit`, `:439-451`), returning HTTP 403.

Because the edit form is loaded from the current row (`:226-228`), resubmitting the *unchanged* value
passes the guard (`new == existing` is allowed). The defect is that the dialog exposes these three
inputs to **every** user with no indication they are administrator-only, and the request always
carries them:

- A normal user who types a value larger than the current one (the natural thing to do in a field
  labelled "Token 配额" / "RPM 限速") gets a bare `toast('保存失败：配额只能收紧不能放宽…')`
  (server message, `key_handler.go:412`) with the form still open and no field marked. The user has
  no way to tell *which* input was rejected, and the whole save — including the name change they
  actually wanted — is lost.
- A normal user who clears a field that an admin had set (`Number('') || 0` → `0`) sends an explicit
  `0`, which `canTightenQuota(existing>0, new=0)` rejects with a 403 — an edit that looks like
  "clearing a field I never had" fails with no client-side hint.

The create dialog gets this right by giving non-admins a disabled read-only quota field plus a tip
(`:478-485`); the edit dialog has no equivalent, and its asymmetry with the create path is exactly
the drift the file's own header comments warn about.

**Why it matters:** The Keys page's primary non-admin persona is a regular user editing their own
key. The UI implies they can set their own quota/rate limits; the backend forbids widening; the only
feedback is a raw server message that loses the whole save. It is also a "UI implies a permission
it does not have" case, which the audit brief specifically asks to flag.

**Concrete fix:** gate the three numeric fields in `submitEdit` the same way the create path does,
and disable/hide them for non-admins so the edit dialog cannot imply a permission it lacks.
```ts
await api.updateKey(eForm.id, {
  name: eForm.name.trim(),
  // 配额与限速与「新建」同一口径：只有管理员能设/改。
  // 普通用户填了也会被 guardNoLoosening 以 403 拒（只能收紧不能放宽），
  // 与其发一个注定失败的请求，不如不发 —— 不发 = 保持原值。
  ...(isAdmin()
    ? {
        quota_tokens: Number(eForm.quota) || 0,
        rpm_limit: Number(eForm.rpm) || 0,
        tpm_limit: Number(eForm.tpm) || 0,
      }
    : {}),
  allowed_models: eForm.models,
  …(Number(eForm.days) !== Number(eForm.daysOrig) ? { expires_at: expiresAtFromDays(eForm.days) } : {}),
  allowed_ips: eForm.ips.trim(),
  ...(isAdmin() ? { group_id: eForm.groupId } : {}),
})
```
and in the template, wrap those three fields:
```html
<div v-if="isAdmin()" class="field span2">
  <label>Token 配额</label>
  <input v-model.number="eForm.quota" class="input num" type="number" min="0" step="1" />
  …
</div>
<div v-if="isAdmin()" class="field">… RPM 限速 …</div>
<div v-if="isAdmin()" class="field">… TPM 限速 …</div>
<div v-else class="field span2"><span class="tip">配额与限速由管理员设置（你的密钥额度不能超过用户级配额）。</span></div>
```

**Confidence:** medium-high (the always-sent-fields behaviour is definitely present and asymmetric with
create; whether an admin-facing 403 happens on the exact path depends on which value the user types —
the "UI implies a permission it lacks" framing holds regardless, and the fix removes the ambiguity).

---

### [P1] `Providers.vue` — `testProvider` uses a single-string `testing` ref, so starting a test on one row unlocks a still-running test on another

**Location:** `web/src/views/Providers.vue:166-176` (`testProvider`), template button `:449-451`.

**What's wrong:**
```ts
const testing = ref('')   // single string, provider id

async function testProvider(p: Provider) {
  testing.value = p.id                       // ← set before await; only one id is tracked
  try {
    const r = await api.testProvider(p.id, testTimeoutMs.value)   // up to 155s
    toast(r.message || …)
  } catch (e) { … }
  finally { testing.value = '' }             // ← unconditional, clears whatever id is there
}
```
`testing` holds **one** provider id, not a set. Same-row double-clicks happen to be blocked (the
button is `:disabled="testing === p.id"` and the id is set synchronously before the first `await`),
but the **cross-row** case is broken: click 测试 on row A (a slow/flaky provider, up to a 155 s
client timeout), then click 测试 on row B. `testing.value` is overwritten with B's id, so:

- row A's button re-enables immediately (`:disabled="testing === p.id"` is now false), letting a
  third click start a *second* concurrent test against the same struggling upstream;
- whichever request finishes first runs `finally { testing.value = '' }`, which clears **B's**
  in-flight marker too, unlocking row B while B's test is still running.

Toasts then race on the single-slot toast (`ui.ts:15-20`), so the slower test's result can overwrite
the faster one's message, and the "测试中…" label is simply wrong for one of the two rows.

**Why it matters:** Each test performs a real upstream `GET /models` (`provider_handler.go:342-356`,
15 s server budget) against whatever provider the operator is diagnosing — precisely when they are
most likely to try several rows in a row. Unbounded concurrent tests against a sick provider consume
real upstream quota and can turn a diagnostic into an additional source of load. The incorrect
"测试中…" state also misleads the operator about what is still running.

**Concrete fix:** track in-flight provider ids in a `Set` (mirroring `Users.vue`'s `pending` map at
`:214-226`) and clear per-id in `finally`.
```ts
// 正在飞行中的测试：provider id 集合。
// 单一字符串不够用 —— 测试最长 155s，A 行未结束时点 B 行会把标记冲掉；
// 先结束的那条在 finally 里清空同一个标记，把仍在跑的 B 也一并解锁。
const testing = ref(new Set<string>())

function isTesting(id: string) { return testing.value.has(id) }
function setTesting(id: string, on: boolean) {
  const next = new Set(testing.value)
  if (on) next.add(id); else next.delete(id)
  testing.value = next
}

async function testProvider(p: Provider) {
  if (isTesting(p.id)) return          // 防连点
  setTesting(p.id, true)
  try {
    const r = await api.testProvider(p.id, testTimeoutMs.value)
    toast(r.message || (r.status === 'ok' ? '连接正常' : '测试失败'), r.status === 'ok' ? 'ok' : 'err')
  } catch (e) {
    if ((e as { status?: number }).status !== 401) toast('测试失败：' + (e as Error).message, 'err')
  } finally {
    setTesting(p.id, false)             // 只清自己那一行
  }
}
```
Template: `:disabled="isTesting(p.id)"` and `{{ isTesting(p.id) ? '测试中…' : '测试' }}`.

**Confidence:** high.

---

### [P2] `Settings.vue` — settings `load()` has no `err` state; a failed `GET /settings` leaves the hard-coded fallback constants rendered as if they were real values

**Location:** `web/src/views/Settings.vue:190-206` (`load`), template `:557-576` and `:579-616`.

**What's wrong:**
```ts
const form = reactive({ default_context_window: 131072, default_max_output_tokens: 65536, … })

async function load() {
  loading.value = true
  try { const s = await api.settings(); /* …assign… */ }
  catch (e) {
    if ((e as { status?: number }).status !== 401) toast('加载设置失败：' + (e as Error).message, 'err')
  } finally { loading.value = false }
}
```
On failure there is only a transient toast and **no persistent `err` state**. `loading` flips to
`false` and the form renders with `form.*` still holding the fallback constants. Every other list-ish
view in this codebase deliberately keeps a visible error state (Keys `:339`, Users `:414`, Groups
`:226`, History `:282`, Overview `:277`, Models `:133`, Routes `:313`, Providers `:400`) precisely so
"request failed" is distinguishable from "no data". The Settings form is the one place that guard is
missing, and it is the place where a wrong number is **actively dangerous**: these values (timeouts,
failover targets, circuit-breaker threshold) feed the forwarding path. An operator whose
`GET /settings` fails (e.g. a 429 rate-limit on the admin API) sees a fully populated, editable form
populated with hard-coded defaults, and may "confirm" a save that overwrites real production values
with the fallback constants (131072/65536/120000/…). The save path itself is a full `PUT /settings`
(api.ts:631), so this is a real silent-overwrite vector.

**Why it matters:** This is the concrete instance of "no data indistinguishable from request failed"
the audit brief asks about, and it escalates from a display bug to a data-integrity risk because the
form is writable.

**Concrete fix:** add a persistent `err` ref and block the form (or at least the save button) when
`GET /settings` failed, mirroring `Models.vue`'s retry pattern.
```ts
const err = ref('')

async function load() {
  loading.value = true
  err.value = ''
  try {
    const s = await api.settings()
    /* …assign… */
  } catch (e) {
    if ((e as { status?: number }).status === 401) return
    // 失败必须留下**持久**错误态：这两个值喂的是转发路径的看门狗与熔断，
    // 回落到 form 的兜底常量后再让人点一次保存，等于用默认值覆盖真实生效值。
    err.value = '加载设置失败：' + (e as Error).message
    toast(err.value, 'err')
  } finally {
    loading.value = false
  }
}
```
Template (both settings forms, `:557` and `:579`):
```html
<div v-if="loading" class="loading">加载中…</div>
<div v-else-if="err" class="empty">
  <div class="big">⚠</div>{{ err }}
  <div class="err-retry"><button class="btn btn-sm" :disabled="loading" @click="load">重试</button></div>
</div>
<form v-else …>
```
and add a `:disabled="saving || !!err"` on the save buttons.

**Confidence:** high.

---

### [P2] `AppModal.vue` — confirm dialog in `App.vue` renders without `dismissable`, so `Esc` and scrim-click resolve it to `false`; combined with `confirmBox`'s "auto-cancel previous", a page modal and the confirm can leave `pageLockCount` correct but the focused element wrong

**Location:** `web/src/components/AppModal.vue:78-106`; `web/src/App.vue:381-398`.

**What's wrong:** The focus-trap `onKeydown` (AppModal `:78-106`) is bound to `window` for every open
modal. The confirm `AppModal` in `App.vue:381` is always mounted (not `v-if`), so its `activateModal`
runs on every `confirmBox()`. When a page's own `dismissable=false` form modal is open and the user
triggers a `confirmBox` from within it (e.g. Users "删除"), **two** modals are simultaneously active:
both listen on `window` for `keydown`. Pressing `Esc` while the confirm is open triggers the
confirm's handler (`dismissable !== false` → `undefined !== false` → true → `emit('close')` →
`settleConfirm(false)`), which is the desired behavior. But the underlying page modal's
`onKeydown` is also still registered; for `Esc` it does nothing because `dismissable === false`.
For `Tab`, **both** traps run in the same `keydown` dispatch: each `preventDefault()`s and focuses its
own `first`/`last`. Whichever handler is registered on `window` last (the confirm, opened most
recently) wins the `Tab` focus move, but the page modal's `activateModal` had set focus into the
confirm's first focusable node… and when the confirm closes, `deactivateModal` restores focus to
`previouslyFocused`, checking `!target.closest('[inert]')`. This is mostly benign, but the
double-registered global `keydown` trap is a real source of subtle focus bugs.

**Why it matters:** Low user impact (confirm still resolves correctly), but it is a genuine
focus-management smell: two independent focus traps active on `window` at once. A cleaner design
restricts the trap to the topmost modal only.

**Concrete fix:** gate the global `keydown` handling so only the most recently activated modal reacts
to `Tab`/`Esc`. Track activation order in the module scope shared by all `AppModal` instances.
```ts
// 模块作用域：当前应当响应键盘的“栈顶”弹窗。
// 只按激活顺序决定归属，不按 open —— 多个弹窗同时 active 时，
// 只有最后一个拿到键盘，否则两个焦点陷阱会在同一次 keydown 里互相抢焦点。
let topModal: symbol | null = null

const modalId = Symbol('app-modal')
// activateModal:
topModal = modalId
// onKeydown 开头：
if (topModal !== modalId) return
// deactivateModal:
if (topModal === modalId) topModal = null
```
This makes Esc/Tab always resolve to the visually topmost dialog (the confirm), which is the
correct precedence, and leaves the page modal inert to keyboard until the confirm closes.

**Confidence:** medium (the trap-collision is real and demonstrable in code; the user-visible symptom
is subtle focus jitter rather than broken function).

---

### [P2] `Login.vue` — offline mode: the `redirect` query is silently dropped when the backend is unreachable, so a user who was bounced for a 401 and lands on "无法连接" loses their intended destination

**Location:** `web/src/views/Login.vue:84-90` (`leaveToBack`), `:38-42` (`mode`), App.vue `:192-199` (redirect injection).

**What's wrong:** When `api.ts:219-228` gets a 401 it sets `session.me = null`; the `App.vue` watcher
(`:192-199`) then pushes `{ name: 'login', query: { redirect: route.fullPath } }`. That redirect is
preserved through the router guard (`router.ts:56-57`) and used by `leaveToBack`. This is correct.

The gap is the offline branch: `loadSession` (`api.ts:108-115`) sets `backendReady = false` when
`GET /session` fails and returns early, so `session.checked = true` and `session.me = null`. If the
current route is `/login` (already there, or bounced there) the user sees "无法连接到网关". That is
correct behavior. However, there is a subtle path: `showShell` is `false`, so `<RouterView v-else />`
renders. If the hash is on a **protected** route (not `/login`) and the backend is down, `reapplyGuard`
(`router.ts:94-98`) sends it to `{ name: 'login' }` — **without** a `redirect` query. So a user who
opened `/#/settings`, backend hiccups, gets bounced to login with no redirect, then reconnects and
lands on the default route `/` instead of `/settings`. This is a minor UX loss, not a security
issue (the redirect validator correctly rejects external URLs in both `router.ts:56-57` and
`Login.vue:89`).

**Why it matters:** Minor — it is a "lost destination on reconnect" UX nit, and worth a single line
in the audit for completeness. It is not exploitable: both redirect consumers validate
`startsWith('/') && !startsWith('//')`.

**Concrete fix:** make `reapplyGuard` preserve the current path when redirecting an already-signed-in
user away from a protected page to login, so the destination survives.
```ts
export function reapplyGuard(): void {
  const cur = router.currentRoute.value
  const d = guardDecision(cur)
  if (d !== true) void router.replace(d)
}
```
and inside `guardDecision`, when returning `{ name: 'login' }` for a non-public target, attach the
path so `Login.vue`'s `leaveToBack` can restore it:
```ts
if (!session.me) {
  return { name: 'login', query: to.meta.public ? {} : { redirect: to.fullPath } }
}
```

**Confidence:** medium.

---

### [P2] `Overview.vue` — pre-probe mount fires 5 uncancelled `/usage/*` requests with no token, then a second set after `loadSession`

**Location:** `web/src/App.vue:208/366` (`v-if showShell` / `v-else` RouterView), `web/src/views/Overview.vue:233-241` (`onMounted → load`).

**What's wrong:** On a **hard refresh** of `/#/` (Overview) while `session.checked === false` and
`session.me === null`, `showShell` is `false`, so `<RouterView v-else />` renders the Overview
component immediately. `onMounted` runs `load()`, which fires `api.stats` + 4× `api.usageBy*` with
`auth.token` read from `sessionStorage`. If a valid token *is* present, these succeed (harmless but
wasted); if the token is expired/absent, all 5 return 401, and `api.ts:219-228` runs
`saveToken(''); session.me = null` five times (idempotent). `Overview.load`'s catch swallows 401
(`:200`), so nothing is shown. Then `App.vue`'s `onMounted` → `loadSession()` completes, sets
`session.me`, `reapplyGuard()` runs, `showShell` flips `true`, and the **Overview component is
re-created** (the `v-if`/`v-else` branches swap RouterViews), firing `load()` a **second** time.
Net effect on a cold load of `/#/`: 10 requests instead of 5, half of them guaranteed-wasted 401s.

**Why it matters:** Minor efficiency + noise. It is not a correctness bug (401s are swallowed; the
second `load()` with a valid session produces the right data and `reqSeq` guards ordering). But it
is exactly the class of "duplicate work at boot" the router/`showShell` comments were written to
avoid, and on a cold load the admin fires 5 avoidable 401s which some rate-limiters count.

**Concrete fix:** don't mount protected views until the session probe has resolved. Gate the
`v-else` RouterView so it only renders the Login route while probing, rather than rendering whichever
route the hash points at.
```html
<RouterView v-else-if="session.checked" />
<div v-else class="boot-splash" aria-busy="true"></div>
```
(when `session.checked && !session.me`, the router guard has already pushed to `/login`, so the
`v-else-if` branch only ever mounts the Login view — no protected view fires duplicate requests).

**Confidence:** high.

---

### [P2] `ModelPicker.vue` — `selectAll` selects all *currently filtered-by-nothing* options, but is reachable while a search filter is active and can silently widen a whitelist to every model

**Location:** `web/src/components/ModelPicker.vue:66-69` (`selectAll`), `:48-52` (`visible`), `:83-86` (button).

**What's wrong:** `selectAll` emits `[...sortedOptions.value]` — the **full** option list, ignoring
the active `query` filter. The search box is right above the list and the "全选" button is next to it
in the header, so a user who typed a search term and then clicks 全选 expecting "select all visible
matches" gets **every** model selected. The component's own doc comment (`:36-39`) says the search
is "只做过滤" and that the selected set is unaffected by filtering — but 全选 sitting in the same
header contradicts that: it makes filtering feel like it scopes the action when it does not. When
`options` is the full public-name list (an admin configuring a group whitelist), this silently sets
the whitelist to **all** models — i.e. it *widens* the group to "unrestricted", the opposite of the
tightening the user was performing.

**Why it matters:** "配置模型" in `Groups.vue` uses this picker, and the whole point of a group
whitelist is to *restrict* models. A mis-scoped 全选 turns a restricted group into an unrestricted
one. The "一个都不选 = 不限制" rule is documented prominently, but the inverse trap (全选 while
filtered = unrestricted) is not.

**Concrete fix:** when a search filter is active, make 全选 operate on the filtered set (what the
user can see), and label it accordingly. Or disable 全选 while filtering.
```ts
function selectAll() {
  // 有搜索词时「全选」= 选中**当前可见**的那些，否则一个防误触的收紧动作
  // 会因为没注意到搜索框还留着筛选词而悄悄变成「全选所有模型」——
  // 那恰好等于取消这个组的限制，与本页意图相反。
  const target = query.value.trim() ? visible.value : sortedOptions.value
  emit('update:modelValue', [...target])
}
```
and label the button `{{ query.trim() ? '全选当前' : '全选' }}`.

**Confidence:** high.

---

### [P2] `fmt.ts` `copyText` — the `execCommand` fallback appends the plaintext secret to the DOM before copying

**Location:** `web/src/fmt.ts:211-231`, used by `Keys.vue:193-196` (`copyKey`) to copy the one-time plaintext API key.

**What's wrong:**
```ts
const ta = document.createElement('textarea')
ta.value = text                 // ← plaintext secret
ta.style.position = 'fixed'
ta.style.opacity = '0'
document.body.appendChild(ta)   // ← now in the live DOM
ta.select()
const ok = document.execCommand('copy')
document.body.removeChild(ta)
```
When `navigator.clipboard` is unavailable — which is exactly the case on a plain-HTTP LAN deployment
(the documented deployment target; clipboard requires a secure context) — the fallback briefly inserts
the **plaintext gateway key** into the live document as an `<textarea>`. It is `opacity:0`, removed
immediately, and no script in this codebase reads the DOM, so there is no in-app leak. But the value
is momentarily in the DOM and in any DOM-inspecting devtools/extension, and it persists in the
textarea's undo/history until the node is GC'd.

**Why it matters:** Low. This is a well-known clipboard-fallback pattern and the only sink for the
plaintext key. Because the CSP is strict and there is no `v-html`/`innerHTML`, the exposure is to
browser extensions/devtools only, not to page script. Worth a note because it is the one place a
long-lived secret touches the DOM.

**Concrete fix:** use a hidden, non-`<textarea>` element or set `readonly` + `autocapitalize=off`,
and copy via `Selection` to keep the secret out of the value-bearing textarea where possible. (The
practical mitigation is to prefer the async clipboard and treat the fallback as last resort, which
the code already does.) A minimal hardening is to avoid `value` lingering by clearing before removal:
```ts
const ok = document.execCommand('copy')
ta.value = ''                   // 清掉 DOM 里的明文再移除
document.body.removeChild(ta)
return ok
```

**Confidence:** high (the pattern is present); low impact.

---

## Clean areas (no issues found)

- **XSS sinks:** zero `v-html`, `innerHTML`, `outerHTML`, `document.write`, `eval`, `new Function`,
  or `dangerouslySet*` across `web/src`. All server data (key names, usernames, group names, provider
  endpoints, model names, error messages, audit entries) is rendered through Vue text interpolation
  or `:title`/`:data-tip` bindings, which are escaped. `v-html` is not used even for rich text.
- **CSP compatibility:** the only script in `index.html` is `<script src="./theme-init.js">`
  (external, `'self'`-compatible) and `<script type="module" src="/src/main.ts">` (external). There
  are no inline `<script>` blocks, no inline event handlers, and no `style="..."` that would need
  `style-src` (the CSP does allow `'unsafe-inline'` for styles, and `Providers.vue`/`Settings.vue`
  use inline `style` attributes, which is fine). No `eval`/inline-eval. The app is fully compatible
  with `script-src 'self'; object-src 'none'; base-uri 'self'; form-action 'self'`.
- **console.log / sensitive logging:** zero `console.*` calls anywhere in `web/src`. No secret is
  logged.
- **Open-redirect:** both redirect consumers validate the target — `router.ts:56-57` and
  `Login.vue:89` both require `startsWith('/') && !startsWith('//')`. Correct.
- **`Range` guards (reqSeq):** `Overview.vue:174-209` and `History.vue:62-112` both implement
  sequence guards that correctly place the stale-response check in `finally` (History explicitly notes
  this). `Routes.vue:107,120,134` has a `chainSeq` guard for the concurrent route-edit case. These
  three concurrency-sensitive views are correct. The views without an equivalent guard (Keys, Users,
  Groups, Providers, Routes list, Settings) do not have rapid range/filter switch paths that re-fetch
  on every keystroke or dropdown change, so a `reqSeq` guard is not needed there — the only
  unguarded re-entrancy is Providers' cross-row `testProvider`, filed above.
- **Double-submit guards:** `Users.vue` (pending map), `Groups.vue` (`deleting`/`fBusy`/`mBusy`),
  `Keys.vue` (`recomputing`), `Settings.vue` (`saving`/`pForm.saving`/`transferBusy`/`pruning`),
  `Login.vue` (`submitting`), `Routes.vue` (`saving`) all correctly set a busy flag before the first
  `await`. `History.vue` `go()` guards on `loading.value` (`:167`) and `exportCSV` guards on
  `exporting` — except the pre-commit window, which is the filed P1. `Providers.vue` `testProvider`
  blocks same-row double clicks but its single-string `testing` ref is clobbered across rows, which
  is the filed P1.
- **`fmt.ts` numeric formatting:** `fmtTokens` tiers by the post-rounding display value and walks back
  to a larger unit at boundaries (`:38-53`), so `999999 → "1M"`, not `"1000K"`. `fmtPercent` floors
  `<1` to one decimal so it never shows a contradictory "100%" + warning badge (`:95-106`).
  `fmtBalance` keys strictly on the `unlimited` flag, never on the numeric value, so "unlimited" and
  "0.00 元" (the 402 case) never collapse together (`:143-146`). These are all correct and carefully
  designed.
- **`password.ts` / backend `userauth.ValidatePassword` parity:** `checkPasswordStrength` uses
  Unicode property escapes (`\p{Ll}`/`\p{Lu}`/`\p{Nd}`) to mirror Go's `unicode.IsLower/IsUpper/
  IsDigit` classification and counts code points (not `.length`) for emoji. This is a faithful port.
- **Backend contract cross-check (route paths):** every path in `api.ts` maps to a registered route in
  `cmd/gateway/main.go:314-401`:
  - `/admin/api/session`, `/bootstrap`, `/login`, `/logout`, `/me`, `/me/password` ✓
  - providers CRUD + `/test` + `/credentials` ✓
  - models CRUD + `/models/discover` + `/models/import` + `/upstream-models` ✓
  - routes CRUD + `/routes/{id}/targets` ✓
  - keys CRUD + `/recompute-usage` ✓
  - users CRUD + `/users/{id}/password` + `/users/{id}/balance` ✓
  - `/groups` + `/groups/{id}/models` + `/model-names` ✓
  - `/stats`, `/settings`, `/audit`, `/reload`, `/usage/by-*`, `/usage/history`, `/usage/history.csv`,
    `/usage/prune`, `/config-export/export`, `/config-export/import` ✓
  Field-level cross-checks also hold: `stats` fields (`cached_tokens`, `cache_hit_rate`,
  `avg_tokens_per_sec`, `avg_ttfb_ms`, `cost`, `lifetime.*`) match `stats_handler.go:36-78`;
  `keyResponse` fields match `key_handler.go:66-89`; `model-names` returns
  `{models, restricted, prices}` matching `group_handler.go:259-336`; `config-export/import` takes
  `{data, passphrase, dry_run}` matching `config_transfer_handler.go:160-162`; the provider `test`
  endpoint returns `{status, message, latency_ms, model_count}` (`provider_handler.go:359-364`) and
  `Providers.vue:170` consumes `status`/`message`; the CSV export writes `X-Export-Total` /
  `X-Export-Truncated` headers (`usage_handler.go:494-497`) that `History.vue:196` reads correctly.
  I found **no frontend↔backend contract mismatch**.
- **Secret handling:** the plaintext key is shown exactly once (`KeyCreateResponse.plaintext_key`),
  the input for new keys/credentials is `type="password"` with `autocomplete="new-password"`, login is
  `autocomplete="current-password"`, and the export passphrase is `type="password"`. No secret is
  written to `localStorage`/`console`/URL.
- **`confirmBox` (ui.ts) modal-stack safety:** the "auto-cancel the previous unresolved confirm" logic
  (`:52`) prevents a hung promise when a second confirm supersedes a first — correct.
- **`Routes.vue` create-route chain guard (reviewed, correct):** `openRoute()`'s no-arg branch
  (`:159-164`) sets `chainReady = true` with a single blank placeholder target. That is safe because
  a create has nothing to overwrite, and `submitRoute` (`:209-212`) independently rejects a chain
  with any unset `provider_id`/`upstream_model_id`. The edit→cancel→create transition also rebuilds
  `targets`/`editing` on every open (`:119-165`), and `chainError` is cleared at `:125`. No defect.
- **`Users.vue` balance double-charge guard (reviewed, correct):** `submitBalance` (`:355-394`) sets
  `balBusy = true` before the first `await`, and re-checks the shared `pending` map
  (`:376-380`) so a balance adjustment cannot race another in-flight row operation. The ordering of
  the `isPending` check after `balBusy = true` is defensive only — there is no `await` between them,
  so no interleaving is possible. No defect.
- **`Users.vue` row-level `pending` map (`:214-226`):** correctly re-wraps the `Map` in a new `ref`
  on every `set`/`delete`, so Vue reactivity is guaranteed regardless of how the mutation happens;
  the per-row keying prevents one busy row from disabling an unrelated row, and the
  `finally { setPending(id, null) }` guarantees no row can be permanently locked.
- **`Models.vue` empty-list semantics:** correctly renders "没有可用模型，请联系管理员…" for an empty
  list rather than "不限制" (`:143-146`), and only shows the "已被你的分组收窄过" banner when the
  backend actually set `restricted: true`. The parallel `models`/`prices` arrays are consumed via a
  `Map` keyed by name (`:91`), so sorting cannot mis-pair a model with another model's price.
- **`Groups.vue` delete guard:** `canDelete`/`boundCounts` (`:144-152`) correctly block the delete
  button when either `member_count` or `key_count` > 0, matching the backend's two independent
  `ON DELETE SET NULL` bindings, and the blocked case is explained rather than hidden (`:158-170`).
- **`confirmBox` + `AppModal` `dismissable` default:** the confirm dialog in `App.vue:381` omits
  `dismissable`, so it defaults to `undefined`, which `AppModal` treats as dismissable
  (`props.dismissable !== false`) — Esc and scrim-click correctly resolve it to `false` via
  `@close="settleConfirm(false)"`. This is the intended behaviour.