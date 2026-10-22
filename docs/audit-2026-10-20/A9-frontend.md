# A9 前端工程与「前端 ↔ 后端 API 契约」审计

- 审计日期：2026-10-20
- 范围：`web/src/**`（17 views + 8 components + 2 composables + api.ts / types.ts / router.ts / ui.ts / fmt.ts / range.ts / routeTrace.ts / modelPrice.ts / password.ts / App.vue / main.ts / styles.css）
- 交叉范围：前端与 `internal/admin/**`、`internal/server/**`、`cmd/gateway/main.go` 的请求契约
- 方法：逐文件读源码 + 逐端点比对 Go handler + 用真实库（`bin/data/gateway.db`）与合成数据实跑验证关键结论
- 边界：鉴权/会话/JWT/CSRF/scope 的**正确性**引用 A7，不在本报告展开；CSP/embed 链路引用 A1；代理链路引用 docs/network-proxy-audit-2026-10-20.md

---

## 0. 结论摘要

**前端工程本身的质量是这轮审计里少见的「好」：类型系统干净、无 `any` 泛滥、状态管理模式在多数页面上一致、危险操作都有确认、错误态与空态在多数页面被刻意区分。真正的系统性缺陷集中在两处：**

1. **一个确定的数据正确性缺陷（P1-1）**：`GET /admin/api/usage/by-day` 的排序是 `ORDER BY key ASC` + `LIMIT 100`，前端却把它当「最近 N 天」用。总览页的「近 1 年」「全部」两档、以及钱包页的 90 天窗口，在历史超过 100 天后**图表会静默丢掉最近的数据**，且界面上没有任何提示。已用合成数据实跑复现。
2. **契约与实现的「重复造轮子」是根因（P2）**：`api.ts` 里每个端点都手写路径字符串，`types.ts` 里每个响应都手写一遍 interface，而 Go 侧也有手写 struct。**三处手写、没有任何一处由机器校验**，这正是「每次都被改坏」的机制来源。

其余为若干 P1/P2 级别的局部缺陷与一致性问题。**未发现 P0**（无资金丢失、无越权、无崩溃路径）。

### 数量

| 级别 | 数量 |
|---|---|
| P0 | 0 |
| P1 | 4 |
| P2 | 9 |
| P3 | 7 |

### 已实跑的验证

```
cd web && npm run typecheck   → 通过（vue-tsc --noEmit，0 错误）
cd web && npm test            → 5 passed / 0 failed
                                   （但只覆盖 routeTrace.ts 与 modelPrice.ts 两个纯函数，
                                     见 P2-8）
```

**没有**运行 `node sync-embed.mjs`，**没有**触碰 `internal/webui/dist`（该目录正处于新旧资产并存的中间态，由 A1 记录）。下面所有关于 dist 的陈述均为**只读**比对。

---

## 1. P1 缺陷（确定的功能缺陷）

### P1-1　`/usage/by-day` 的 `ORDER BY key ASC` + `LIMIT 100` 让总览「近 1 年 / 全部」与钱包 90 天窗口静默丢掉最近的数据

**位置**：
- 后端：`internal/admin/usage_handler.go:889`（`limit := clampLimit(q.Get("limit"), defaultUsageLimit, maxUsageLimit)`，默认 100）
- 后端：`internal/admin/usage_handler.go:896-899`
  ```go
  order := "ORDER BY tokens DESC"
  if column == "day" {
      order = "ORDER BY key ASC"
  }
  ```
- 前端：`web/src/api.ts:774`
  ```ts
  usageByDay: (from: number, to: number) => get<UsageGroupEntry[]>(`/usage/by-day?from=${from}&to=${to}`),
  ```
- 前端调用点：`web/src/views/Overview.vue:250`、`web/src/views/Wallet.vue:190`

**机制**：`by-day` 是 `groupBy` 家族里唯一用 `ORDER BY key ASC`（日期升序）的维度，其余三个（by-model / by-key / by-provider）都是 `ORDER BY tokens DESC`。升序 + `LIMIT 100` 的组合意味着**截断的是最早的那 100 天，而不是最近的那 100 天**。

前端两个调用点都把 `from` 设成「区间起点」、`to` 设成「现在」，并假设返回的是**整个区间**的每日序列：

- `web/src/views/Overview.vue:250`：`api.usageByDay(from, to)`，`from` 来自 `rangeBounds(requested)`。`RANGES` 里 `1y` 的 days=365、`all` 的 days=0（全部历史）。
- `web/src/views/Wallet.vue:183,190`：`dayFrom = rangeStart(USAGE_DAYS /* =90 */, now)`，取最近 90 天。

**实跑验证**（合成数据，SQL 与后端 `groupBy` 逐字一致，含 `dayExpr = strftime('%Y-%m-%d', ts/1000,'unixepoch','localtime')`）：

```
# 场景一：400 天连续历史，总览「全部」档
ASC LIMIT 100 -> n=100  first=2025-01-01  last=2025-04-10
today present? False
完整区间 400 天，其中 300 天的数据根本不在响应里

# 场景二：365 天历史，总览「近 1 年」档
ASC LIMIT 100 -> n=100  first=2025-10-26  last=2026-02-02
today (2026-10-20) present? False
→ 所选年份里最近 265 天的数据全部缺失

# 场景三：90 天窗口（钱包页）
若窗口内超过 100 个有记录日，同样从最早的开始截
```

**后果**：

`web/src/views/Overview.vue:86-160` 的 `daySeries` 拿 `byDay` 建 Map 后**逐日补零**：

```ts
const map = new Map(byDay.value.map((e) => [e.key, e]))
...
while (cursor <= end) {
  const date = timeRange.localDayKey(cursor.getTime())
  const e = map.get(date)
  daily.push({ date, count: e?.count ?? 0, tokens: e?.tokens ?? 0 })
  cursor.setDate(cursor.getDate() + 1)
}
```

缺失的日期被补成 `count: 0, tokens: 0`。于是：

- **总览「近 1 年」档**：图上是一段真实数据 + 一大片零 + 尾部（最近 265 天）全零。读图得到的结论是「最近半年没有流量」，而实际是「接口没返回那些天」。
- **总览「全部」档**：同理，且 `lifetimeTruncated` 的兜底逻辑（`web/src/views/Overview.vue:79-84`）在管理员身份下**永远为 false**（管理员能拿到 `stats.lifetime.first_record_at`），所以那条「趋势仅覆盖当前返回的最近 N 个有记录日期」的提示**不会显示**。管理员看到的是一张看起来完整、实际缺了 3/4 历史的图。
- **钱包页 90 天消耗记录**：同样从最早的开始截断，最近的消耗记录整段消失。**这一条最危险** —— 钱包页的「按天消耗」是用户对账「钱扣了没有」的唯一入口，而图上最近的日子全是 0。

**为什么「全部」档那条兜底提示救不了**：`web/src/views/Overview.vue:79-84` 的 `lifetimeTruncated` 判据是「`stats.lifetime.first_record_at` 拿不到 **且** `byDay.length > 0`」。管理员在「全部」档会请求 `include_lifetime=true`（`web/src/views/Overview.vue:250` 的 `requested === 'all'`），后端 `internal/admin/stats_handler.go:221` 对管理员返回 lifetime —— 于是 `first_record_at` 有值，`lifetimeTruncated` 恒为 false，提示不出现。**这正是最需要提示的那个身份。**

**根因归属**：这是**后端**的缺陷（by-day 是唯一升序的维度），但前端把「区间起点 → 终点」当作该端点的契约，且没有对返回天数做校验。两边都该改。

**建议修法**：
1. 后端：`by-day` 改为 `ORDER BY key DESC`（或新增 `ORDER BY key DESC LIMIT`），并在响应里回一个 `truncated` / `first_day` 字段；或者给 `by-day` 单独走一个「按天升序返回、最多 N 天、可指定窗口」的语义。
2. 前端：`api.usageByDay` 传 `limit`；`Overview.vue` 与 `Wallet.vue` 在拿到响应后**校验返回的天数与请求区间是否吻合**，不吻合就显示明确的「部分覆盖」提示（复用现有的 `lifetimeTruncated` 模式），而不是把缺失日补成 0。
3. 前端补一条单元测试：给定「后端只返回最早 100 天」的响应，断言总览页**显示截断提示**而不是画出一张缺尾的图。

---

### P1-2　by-\* 家族的四个端点有两套不同的默认窗口语义，漏传就静默变成「近 7 天」

**位置**：
- 后端：`internal/admin/usage_handler.go:884`（`queryRangeExplicit(q, 7*24*time.Hour)`）—— by-\* 聚合端点 `from` **不传**时回落 7 天
- 后端：`internal/admin/usage_handler.go:286`（`/usage` 明细端点，**同一族但不同默认值**）
- 前端：`web/src/api.ts:774`（`usageByDay` 显式传 `from`/`to`，且**无 limit 参数**）
- 前端：`web/src/api.ts:779-784`（`usageByModel` / `usageByKey` / `usageByProvider` 的 `from`/`to` 有默认值 `0`）

**说明**：这条本身**当前是正确的** —— 全部四个封装都显式传了 `from`/`to`。列在这里是因为它是 P1-1 的**同类结构**：by-\* 家族有 4 个端点、2 套不同的默认窗口语义（`/usage` 明细 vs by-\* 聚合）、加上 by-day 独有的排序方向，全靠每个调用点自觉传对参数。`api.ts:774` 的 `usageByDay` 与 `api.ts:779-784` 的其他三个**签名形状不同**（前两个参数必填、后两个有默认值），这个不一致本身就是信号。

**建议**：把 by-\* 家族的四个封装统一成同一个签名（`from`/`to` 必填，无默认值），让漏传变成编译错误而不是运行时行为变化。

---

### P1-3　`Providers.vue` / `Routes.vue` 的筛选与搜索**每次击键都改写 URL**，与 `UsageDetail.vue` 的「草稿 + 应用」模式并存

**位置**：
- `web/src/composables/useQueryText.ts:6-14`：`set` 直接 `router.replace({ query: { ...route.query, [key]: value === fallback ? undefined : value } })`
- `web/src/views/Providers.vue:15-27`：
  ```ts
  const status = useQueryText('status')
  const filtered = computed(() => providers.value.filter(p => {
    const state = !p.enabled ? 'disabled' : p.ready === false ? 'unready' : p.ready === true ? 'ready' : 'unknown'
    return matches && (!status.value || status.value === state)
  }))
  ```
- `web/src/views/Routes.vue:33-34`（`status = useQueryText('status')`）
- 模板绑定：`web/src/views/Providers.vue:177-178`、`web/src/views/Routes.vue:404-405`

**机制**：`search` 用 `v-model="search"` 绑在 `<input type="search">` 上，而 `search` 是 `useQueryText('q')` —— **每一次击键都会 push 一次 URL**。这意味着：

1. 输入「deepseek」= 8 次路由跳转，浏览器前进/后退栈被搜索词填满；
2. 用户按一次「后退」期望离开页面，实际只是回到上一个字符；
3. `web/src/views/Providers.vue:29` 的 `clearFilters()` 又是另一套 `router.replace` 写法（手写 `query` 展开），与 `useQueryText` 的 set 逻辑**重复实现** —— 两处各写一份「清掉 query 参数」的规则，改一处忘一处就漂移。

**对比正确做法**：`web/src/views/UsageDetail.vue:32-40` 就是对的 —— 筛选条件拆成「URL 中已应用的值」与「输入框里的草稿」两份，只有点「应用」/ 回车才写 URL：

```ts
const fStatus = ref(''); const fModel = ref(''); const fKey = ref('')
watch(() => [request.value.status, request.value.model, request.value.keyId], () => { ... }, { immediate: true })
```

**影响面**：这是本仓库里**最容易被下一个 agent 改坏**的地方 —— 两个页面用「击键即改 URL」，一个页面用「草稿 + 应用」。两种模式并存。

**建议**：统一到 `UsageDetail.vue` 的草稿模式；`useQueryText` 增加一个 `useQueryDraft`（本地 ref + `apply()`），或者至少让 `useQueryText` 的 set 支持 `replace` 与防抖。

---

### P1-4　`ProviderDetail.vue` 拉单个 provider 用「拉全表再 find」，且它的注释「后端没有 GET /providers/{id}」是错的

**位置**：`web/src/views/ProviderDetail.vue:54-77`
```ts
const all = await api.providers()
// 列表接口是唯一按 id 取单个 provider 的入口（后端没有 GET /providers/{id}）。
```

**这段注释是错的**。后端**有**这个端点：

`cmd/gateway/main.go:381`
```go
adminMux.HandleFunc("GET /admin/api/providers/{id}", func(w http.ResponseWriter, r *http.Request) { providerHandler.Get(w, r, r.PathValue("id")) })
```
`internal/admin/provider_handler.go:282-293` 的 `Get` 实现完整。

**后果**：
1. 详情页为了显示**一个**上游，拉了**全部**上游（含每个的 endpoint、ready_reason、模型数、凭据数）。上游数量增长时这是一个 O(N) 的页面加载。
2. 后端的 `Get` 走 `store.GetProvider`，**不带** `ready` / `ready_reason`（`toProviderResponse` 不填这两个字段，只有 `List` 填，见 `internal/admin/provider_handler.go:173-181` vs `:292`）。所以前端**不能**直接换成 `GET /providers/{id}` —— 那会丢掉 `ready` 状态。这是一个**后端契约缺口**，不是前端笔误。
3. 注释「后端没有 GET /providers/{id}」会持续误导后续维护者，让这个 O(N) 拉法被当成既定事实保留下来。

**建议**：后端给 `Get` 补上 `ready` / `ready_reason`（与 `List` 同一份来源），前端改用单条端点；或前端加一个 `api.provider(id)` 并同步修正注释。

---
## 2. P2 缺陷（架构 / 设计 / 一致性问题，会持续产 bug）

### P2-1　三处手写同一份契约，**没有任何机器校验** —— 这是「每次都被改坏」的根因

**证据链**（以 Provider 为例）：

| 层 | 位置 | 形式 |
|---|---|---|
| Go handler | `internal/admin/provider_handler.go:69-108` | `type providerResponse struct { ... json:"..." }` |
| TS interface | `web/src/types.ts:3-56` | `export interface Provider { ... }` |
| TS 客户端 | `web/src/api.ts:450-452` | `get<Provider[]>('/providers')` |

同样的三元组在 `Credential` / `UpstreamModel` / `Route` / `AccessKey` / `User` / `Group` / `ModelGroup` / `Network` / `Settings` / `Stats` / `UsageHistoryEntry` / `UsageGroupEntry` 上各有一份。**共 12 组以上。**

已经**实际发生过**的漂移（本次审计在阅读中发现的）：

1. `web/src/types.ts:1021-1022` 把 `warnings?: string[]` / `notes?: string[]` 声明在 **`UpstreamModelUpdate`**（模型 PATCH 的载荷）里。实测这两个字段属于 `ConfigImportResult`（配置导入的**响应**），`internal/admin/config_transfer_handler.go:204` 有 `DryRun bool json:"dry_run"` + warnings/notes，而模型 PATCH 的 `modelRequest`（`internal/admin/model_handler.go:34-67`）**根本没有** warnings/notes 字段。位置放错了类型，读代码时会以为「PATCH 模型能写 warnings」。
2. `web/src/types.ts:995-1022` 的 `ConfigExportFile` / `ConfigExportProvider` / `ConfigExportModel` 等是**手抄**的导出文件结构，而 `internal/admin/export_format.go` 里有真正的定义。
3. `web/src/types.ts:673` 的 `KeyCreateResponse extends AccessKey` 加 `plaintext_key` —— 正确，但这个「后端在 Create 响应里额外加一个字段」的约定没有在任何类型层面被表达，只能靠人记住。

**为什么这是根因**：多 agent 反复修改时，「后端加了字段」→ 「人肉去 types.ts 加一行」→ 「忘了加，TypeScript 不报错（因为后端返回的是 `any`）」→ 「运行时不报错（因为前端没读那个字段）」→ **契约漂移静默存在**。`web/src/api.ts:248` 的 `let data: any = null` 就是这个洞的入口：

```ts
let data: any = null
try { data = text ? JSON.parse(text) : null } catch { }
...
return data as T
```

`as T` 是一个**断言**，不是一次**校验**。后端改了字段名，TypeScript 一声不吭。

**建议**（见第 6 节「最小机制建议」第 1 条）：从 Go 的 struct 生成 TS interface，作为构建步骤。

---

### P2-2　`types.ts` 与后端 JSON 的**可空性**不匹配：前端大量 `?? 0` / `?? ''` 兜底，掩盖了后端契约的真实形状

**证据**：
- `web/src/types.ts:2` 的文件头注释写「与 internal/admin/*.go 的 response 结构一一对应」—— 但**没有**说清哪些字段是 `omitempty`。
- 后端 `internal/admin/provider_handler.go:101`：`ReadyReason string json:"ready_reason,omitempty"` → **可能整个字段缺席**。
- 后端 `internal/admin/model_handler.go:140-166`：`tokens_per_sec` / `ttfb_ms` / `call_count` / `route_count` / `group_id` / `unknown_effort_levels` 全部 `omitempty`；但 `success_rate`（:142）与 `is_route_head`（:167）**没有** omitempty。
- 前端 `web/src/types.ts:153-213` 的 `UpstreamModel` 把 `tokens_per_sec?` / `ttfb_ms?` / `success_rate?` / `call_count?` / `route_count?` / `is_route_head?` 全部声明成**可选**。

**为什么这有实际后果**：`success_rate` 后端**总是**下发（0~1），前端声明成 `number | undefined`，于是 `web/src/components/UpstreamModelPanel.vue:120` 写成：
```html
<span v-if="m.call_count" class="sr" :class="{ warn: (m.success_rate ?? 1) < 1 }">
```
`?? 1` 的含义是「字段缺席 = 100% 成功」。而后端在「从未调用过」时下发的是 `success_rate: 0` + `call_count` 缺席 —— 这时 `v-if="m.call_count"` 为 false，不渲染，所以当前**没有出错**。但 `?? 1` 这个兜底一旦被复制到别处（例如不再被 `call_count` 门控），就会把「无样本」读成「100% 成功」—— 这正是本仓库反复写注释要防的那类误读。

**建议**：在 `types.ts` 里对每个 `omitempty` 字段写一行注释标明「可能缺席」，并把 `success_rate` 改成必填（后端确实总是下发）。这一条不需要生成代码，靠一次人工对表即可，但**必须做一次**。

---

### P2-3　「请求序号」防竞态的模式在 11 处各自实现，形状一致但**不覆盖组件卸载**

**证据**（`let xxxSeq = 0` + `if (seq !== xxxSeq) return`）：

| 文件 | 行 | 卸载时作废？ |
|---|---|---|
| `web/src/composables/useNetworks.ts` | 23, 38, 42, 47, 50 | 否 |
| `web/src/components/UsageByGroup.vue` | 34-52 | 否 |
| `web/src/views/Providers.vue` | 30-51 | 否 |
| `web/src/views/Routes.vue` | 61-101, 167 | 否 |
| `web/src/views/Overview.vue` | 242, 263 | 否 |
| `web/src/views/Wallet.vue` | 133-253 | 否 |
| `web/src/views/ProviderDetail.vue` | 101-109 | 否（用 `id !== providerId.value` 代替） |
| `web/src/views/Models.vue` | — | **完全没有** |
| `web/src/views/UsersGroups.vue` | — | **完全没有** |
| `web/src/views/ModelGroups.vue` | — | **完全没有** |
| `web/src/views/Settings.vue` | — | **完全没有** |
| `web/src/views/UsageDetail.vue` | 56-57 | **是**（`onUnmounted(() => { ++reqSeq })`） |

**只有 `UsageDetail.vue` 一个**做了卸载作废。它是唯一有 `onUnmounted` 的视图 —— 说明**有人意识到过这个问题，但只在一个地方修了**。

**为什么这在 Vue 3 里不是纯理论问题**：Vue 3 的 `setup` 在组件卸载后仍会被正在 await 的 async 函数持有引用。异步 `await` 之后写 `rows.value = result`（`web/src/components/UsageByGroup.vue:44`）在组件已卸载时是**写一个已销毁组件的 ref** —— Vue 3 不会报错（不像 Vue 2 会 warning），但这个 ref 仍被闭包持有，直到 promise settle。这是内存泄漏的短窗口，且如果未来引入 KeepAlive（`web/src/views/AccessControl.vue` 的 `<component :is>` 已经用 `:key` 强制重建，接近这个语义），行为会更难预测。

**更实质的问题**：`Models.vue` / `UsersGroups.vue` / `ModelGroups.vue` / `Settings.vue` **完全没有序号保护**。`web/src/views/UsersGroups.vue:131-146` 的 `load()` 是 `Promise.all` 三个请求：

```ts
const [u, g, names] = await Promise.all([api.users(), api.groups(), api.modelNames()])
```

用户点「刷新」两次 → 两个 `load()` 并发 → 后到的响应可能覆盖先到的。虽然这三个是全量替换（不会「旧数据盖新数据」），但**错误的空/失败状态会互相覆盖**：`web/src/views/UsersGroups.vue:143-145` 的 catch 把 `err.value` 设成失败消息，而并发的另一次成功请求已经把 `users.value` 更新了 —— 结果是「有数据 + 显示错误」的矛盾态。

`web/src/views/Models.vue:236-249` 的 `load()` 同样：`modelNames` 成功后调 `loadProbes()`，后者失败会设 `probeErr`；两次并发 load 时，**旧的那次 `loadProbes` 失败会覆盖新一次的成功**（`web/src/views/Models.vue:216-226`）。

**建议**：抽一个 `useAsyncSeq()` composable（`const { run } = useAsyncSeq()`），统一提供「序号 + 卸载作废」；在所有 `load()` 上套用。这条改完，竞态与卸载问题一起消失。

---

### P2-4　失败态的**呈现方式**在 12 个视图里有 4 种不同实现

**证据**（同样是「请求失败」）：

| 模式 | 位置 | 表现 |
|---|---|---|
| A. DataTable 的 `error` prop | `web/src/views/Providers.vue:181`、`web/src/views/UsageDetail.vue:139` | 表格区显示错误 + 保留旧数据 + 重试按钮 |
| B. 独立的 `err` ref + 自己的 `v-else-if="err"` | `web/src/views/UsersGroups.vue:143-145, 578`、`web/src/views/ModelGroups.vue:33-40` | 整个面板替换成错误块 |
| C. `err` ref + `stale-bar` | `web/src/views/Overview.vue:316` | 顶部一条横条 + 保留上次结果 |
| D. 只弹 toast，无持久错误态 | `web/src/views/ModelGroups.vue:38`、`web/src/views/Networks.vue:53-56` | toast 消失后无痕 |

模式 D 出现在 `web/src/views/ModelGroups.vue:36-40`：
```ts
} catch (e) {
  if ((e as { status?: number }).status === 401) return
  err.value = '加载模型组失败：' + (e as Error).message
  toast('加载模型组失败：' + (e as Error).message, 'err')
}
```
`err.value` 被设了，但 `ModelGroups.vue` 的模板里**没有一处读 `err`** —— 它是一个写了但没接线的死变量。所以模式 D 实际上是「只弹 toast」：加载失败后页面呈现为「还没有模型组」，与「真的没有」完全同形。

**同一批文件的注释明确要求相反的做法**（`web/src/views/UsersGroups.vue:130-140`）：

> 失败必须留下可见的错误态：只弹 toast 的话，页面保留空列表/空表格，呈现成「暂无数据」—— 而真实原因是请求失败了。运维会据此判断「今天没有流量」，进而排除掉网关/上游故障这个真正的方向。

**`ModelGroups.vue` 违反了它自己引用着的那条规范。**

**建议**：统一到模式 A（`DataTable` 的 error prop，它已经同时实现了「保留旧数据 + 重试 + 区分空态」三件事，是本仓库里最完整的一套）。

---

### P2-5　`api.ts` 的 `mutate()` 是个**空函数**，但 20 个写操作仍然包着它 —— 命名在撒谎，且抹掉了「哪些写操作需要 reload」的知识

**位置**：`web/src/api.ts:377-383`
```ts
export async function mutate<T>(fn: () => Promise<T>): Promise<T> {
  return await fn()
}
```

**它上面 60 行的注释**（`web/src/api.ts:320-376`）详述了「为什么不再调 POST /admin/api/reload」，讲得非常清楚 —— 这是一段**高质量的注释**，记录了一个真实的 P1 事故（普通用户建 key 后 reload 403，被报成「创建失败」）。但修完之后，`mutate()` 变成了空壳，而**20 个写操作仍然包着它**：

```ts
createProvider: (b) => mutate(() => post<Provider>('/providers', b)),
updateProvider: (id, b) => mutate(() => patch<Provider>(`/providers/${id}`, b)),
deleteProvider: (id) => mutate(() => del(`/providers/${id}`)),
... （共 20 处）
```

**问题**：
1. **命名撒谎**：`mutate` 承诺「这次写操作有额外语义」，实际没有。下一个 agent 读到 `mutate()` 的实现会困惑。
2. **「哪些写操作需要 reload」的知识被抹掉**。注释里明确说了三类：
   - 走 `mutate()` 的：改了运行时快照（providers / networks / credentials / models / routes / route-targets / model-groups / groups / settings）
   - **刻意不走** `mutate()` 的：`createUser` / `updateUser` / `deleteUser` / `adjustUserBalance` / `recomputeKeyUsage` / `pruneUsage` / `providerBalance`
   - `api.ts:698-712` 的 `createUser` / `updateUser` / `deleteUser` / `adjustUserBalance` 确实不包；`api.ts:730` 的 `createGroup` 包了，`api.ts:749` 的 `setGroupModels` 包了 —— 理由是「模型可见性随内存快照下发」。**这个区分是对的，但它只存在于注释里。**
3. **不一致已经存在**：`api.ts:715-716` 的 `resetUserPassword` / `changeMyPassword` 也不包 —— 这两类（改密 → auth_version 递增 → 会话失效）**确实不该 rebuild**，是对的。但既然 `mutate()` 是空的，代码里就**没有任何东西**在说这件事。

**建议**：把 `mutate()` 改名为 `write()`（或直接删掉包裹、改用注释标记）；把「需要 reload / 不需要 reload」的区分显式化 —— 例如用两个命名空间 `api.writes` / `api.dbWrites`，或给每个写操作加一行 `// reload: yes/no`。

---

### P2-6　`api.ts` 的 `api` 对象与 `auth`/`session`/`mutate` 是**两套导出风格**；`auth.token` 是 `reactive` 的可变全局，且「清 token + 清 me」这个二元组被三处各自重复

**位置**：`web/src/api.ts:48-50`
```ts
export const auth = reactive({ token: sessionStorage.getItem(TOKEN_KEY) ?? '' })
```

`auth.token` 是**可写**的，导出给全项目。`saveToken()`（`web/src/api.ts:52-56`）是官方的写入口，但没有任何机制阻止别处直接 `auth.token = 'xxx'`。

**实际影响**：`web/src/api.ts:236-241`（401 处理）、`web/src/api.ts:294-299`（download 的 401 处理）、`web/src/views/AccountSettings.vue:29-31`（改密后）三处都各自写「清 token + 清 me」。**它必须同时发生**（少任何一个，界面就会停在「有外壳但无身份」或「有身份但发请求必 401」的空壳态）。

`web/src/App.vue:295-303` 的 watch 依赖 `session.me` 变空来跳登录页。所以「三处必须同时清」是一条**没有类型保护、没有任何断言**的隐式契约。

**建议**：提供一个 `signOutLocally()` 函数，封装「清 token + 清 me」，三处统一调用；`auth.token` 改为只读（`readonly` + 一个 `setToken`）。

---

### P2-7　`web/src/views/Upstream.vue` 是**完全的死代码**，但仍在 `web/src/views/` 下、且被 `ModelGroups.vue` 的注释引用

**证据**：
- `web/src/views/Upstream.vue`（3698 字节）定义了三个页签（上游与模型 / 路由 / 模型归组），并 `import Providers from './Providers.vue'` / `Routes` / `ModelGroups`。
- `web/src/router.ts` 的**任何一条路由**都不引用它（`rg "views/Upstream" web/src` 在 import 位置零命中）。
- 唯一提到它的地方是一句注释：`web/src/views/ModelGroups.vue:197`：「面板形态（2026-10-14 页面合并）：并进 Upstream.vue 的「模型归组」页签。」

**这是一个「已被正确重构、但旧壳没删」的残留**。2026-10-14 的页面合并把 Providers / Routes / ModelGroups 拆成三个独立路由（`web/src/router.ts:22-27`），`Upstream.vue` 随之失去最后一个使用者。

**为什么它有害**：
1. 它是**同名概念的第二个定义** —— 「上游与模型」页在 `Upstream.vue` 里是「一个页面三个页签」，在 `router.ts:22` 里是「三个独立路由」。下一个 agent 读 `Upstream.vue` 会以为「上游」是一个页面。
2. `ModelGroups.vue:197` 的注释**仍然指向它**，会把人引向死代码。
3. 它内部 `import` 了三个重量级 view，虽然没人 import 它所以不进 bundle，但源码搜索时它是噪音。

**同类**：`web/src/views/UsersGroups.vue:50` 的 `import Tabs from '../components/Tabs.vue'` —— `UsersGroups.vue` **不使用** `<Tabs>`（它用宿主页 `web/src/views/AccessControl.vue:78` 的 Tabs），这是一个未使用的 import。同类还有 `web/src/views/Settings.vue:8` 未使用的 `ApiFail`、`web/src/components/Tabs.vue:33` 未使用的 `computed`。TypeScript 的 `noUnusedLocals` 没有开（`web/tsconfig.json` 只开了 `strict`）。

**建议**：删 `Upstream.vue`；修 `ModelGroups.vue:197` 的注释指向 `AccessControl.vue` / `UsageStats.vue`；开 `noUnusedLocals`（会让上面 3 个未使用 import 立刻暴露）。

---

### P2-8　前端测试只覆盖 2 个纯函数，**零组件测试、零 API 契约测试**

**证据**：`web/package.json:14` → `"test": "node --experimental-strip-types --test tests/*.test.mjs"`；`npm test` 输出 5 个用例，全部是 `routeTrace.ts`（4 个）与 `modelPrice.ts`（1 个）的纯函数。

`web/src/routeTrace.ts`（3649 字节）与 `web/src/modelPrice.ts`（796 字节）是**两个**有真实逻辑的纯函数模块，恰好被测了。而：

- `web/src/range.ts`（4186 字节）—— 时间区间口径（本地自然日 0 点、夏令时、跨月），**是全仓库最容易出错的一段逻辑**（`web/src/range.ts:8-14` 的注释列举了 UTC 偏移、跨夏令时、跨月三种坑），**没有测试**。
- `web/src/api.ts`（40031 字节）—— 全部端点封装，**没有测试**。
- `web/src/types.ts`（40479 字节）—— 全部契约，**没有测试**（也不可能靠单测保证）。
- `web/src/password.ts`（3241 字节）—— 密码强度门槛，`web/src/views/Login.vue:60` 与 `web/src/views/AccountSettings.vue:25` 都用它，**没有测试**。而 `web/src/views/UsersGroups.vue:367` 的注释明确说这个门槛必须与后端 `userauth.ValidatePassword` 一致 —— **两边各写一份，没有任何东西钉住它们一致**。

**为什么这是 P2 而不是 P3**：P1-1（by-day 截断）**如果有一个 API 契约测试**（断言「请求 365 天 → 响应覆盖 365 天」）就会立刻暴露。`range.ts` 的三种时区坑同理。

**建议**：至少补 3 组测试 —— `range.ts` 的区间口径（固定 `now`、断言 from/to 落在本地 0 点）、`password.ts` 的门槛（与后端 Go 实现同表驱动）、以及一个「api.ts 的 URL 构造」快照测试（每个封装调用一次，断言拼出的路径与 query）。

---

### P2-9　`UsageDetail.vue` 的未提交改动是**正确且完整的**，但 `internal/webui/dist` 处于「已同步新 CSS、旧 JS 已删新 JS 未跟踪」的**不一致中间态**

**审计点 6 的结论**。

**未提交改动本身**（`git diff -- web/src/views/UsageDetail.vue`，50 增 13 删）：
1. 新增 `.logs-table .model-name.mono, .logs-table .cell-meta.mono { white-space: nowrap; }` —— 把 `.cell-meta` 的特异性从 (0,1,0)+data-v 抬到 (0,4,0)，压过全局 `web/src/styles.css:2093` 的 `.table-panel .tbl .mono { white-space: normal; overflow-wrap: anywhere; }`。
2. `.model-column` 的 `max-width` 从 360px 改为 300px。

**这两处都是对的**，注释里的推理（`web/src/views/UsageDetail.vue:215-241`）也基本站得住：

- 全局那条 `white-space: normal` 的特异性是 `.table-panel .tbl .mono` = (0,3,0)。scoped 样式会编译成 `.logs-table .model-name.mono[data-v-x]` = (0,4,0)，**本来就更高**；而 `web/src/views/UsageDetail.vue:243` 的 `.cell-meta` 只有 1 个 class（+data-v 后是 (0,2,0)），**确实低于 (0,3,0)** —— 所以**真正需要抬特异性的是 `.cell-meta`，不是 `.model-name`**。改动给两者都加了规则，是安全的超集。
- 300px 的依据（`web/src/views/UsageDetail.vue:236-241`）是 1280px 视口下各定宽列之和 ≈742px、留给模型列 ≈255px —— 300px 略超 255px，但 `min-width: 0` 会让它被压缩。**这个推理没有实测支撑**（没有 1280px 的实际渲染测量），属**待确认**。

**dist 中间态**（只读比对，未做任何修改）：

```
工作区（未提交，含新改动）:
  internal/webui/dist/assets/UsageStats-BaFrZ4Kw.css   ← 含 .model-name.mono / .cell-meta.mono 的 nowrap 规则
  internal/webui/dist/assets/UsageStats-mKQs0NDv.js
  internal/webui/dist/index.html → assets/index-m5WBl62f.js

HEAD (ea19ce1，已提交):
  internal/webui/dist/assets/UsageStats-yQCFwsvU.css   ← 无那两条 nowrap 规则
  internal/webui/dist/assets/UsageStats-BYUqPI-D.js
  internal/webui/dist/index.html → assets/index-CqXLBNJx.js
```

**关键观察**：工作区的 CSS **已经**包含未提交改动里新增的那两条规则：

```
UsageStats-BaFrZ4Kw.css:
  .logs-table th[data-v-8869632e],.logs-table td[data-v-8869632e],
  .logs-table .model-name.mono[data-v-8869632e],
  .logs-table .cell-meta.mono[data-v-8869632e]{white-space:nowrap}
  .logs-table .model-column[data-v-8869632e]{min-width:0;max-width:300px}
```

也就是说 —— **「改 CSS」这一步已经做过了，但 `git status` 仍显示 `M web/src/views/UsageDetail.vue`**。因为 `internal/webui/dist` 是 gitignore 的（`git status` 里那些 `?? internal/webui/dist/assets/*` 是未跟踪的新文件、`D` 是被删的旧文件），**dist 的状态不在 git 的视野里**。

**结论**：
1. `web/src/views/UsageDetail.vue` 的未提交改动**是完整且自洽的**（CSS 规则 + 宽度调整 + 解释性注释三者配套），不像是半途而废。
2. `internal/webui/dist` 目前**包含**这份改动（CSS 已新），但因为工作区的 dist 是「旧 JS 删掉 + 新 JS 未跟踪」的中间态，**如果此刻 `git add -A && git commit`，新 JS 会被提交、旧 JS 被删除，而 `web/src` 的源改动不会**（除非显式 `git add web/src`）—— 这会让「dist 与源不同步」的状态进入历史。
3. `docs/frontend-shared-components-2026-10-18.md` 记录的流程是：`build` → `sync`（写 dist）→ 提交。**当前工作区停在 build 之后、commit 之前。**

**建议**：把 `web/src/views/UsageDetail.vue` 与重新构建的 dist **一起**提交（顺序：`npm run build` → `node sync-embed.mjs` → 一起 add），不要让 dist 与源分两次进历史。（`sync-embed.mjs` 的运行**不在本审计范围内** —— 规则禁止，且 dist 归 A1。）

---
## 3. P3（nit / 样式）

### P3-1　`web/src/components/Tabs.vue:33` 未使用的 `computed` import

`import { computed, nextTick, onBeforeUnmount, ref, useId, watch } from 'vue'` —— `computed` 在本文件里没有使用（`tabs` prop 用 `withDefaults(defineProps(...))` 接收）。`web/tsconfig.json` 没开 `noUnusedLocals`，所以不报错。

### P3-2　`web/src/views/Settings.vue:8` 未使用的 `ApiFail` import，且 401 判定方式与 `UsersGroups.vue` 不一致

该文件用 `(e as { status?: number }).status` 判断 401（`web/src/views/Settings.vue:416`、`:502`、`:602`），而**没有**用 `ApiFail` 类。同一份代码在 `web/src/views/UsersGroups.vue:143` 用的是 `e instanceof ApiFail && e.status === 401` —— **两种 401 判定方式并存**。

### P3-3　`web/src/views/UsersGroups.vue:50` 未使用的 `Tabs` import

`UsersGroups.vue` 曾有自己的页签条，合并进 `AccessControl.vue` 后删掉了（`web/src/views/UsersGroups.vue:559-563` 的注释：「原来这里自己也有页签条…删掉比留着好」），但 import 没删。

### P3-4　`web/src/composables/useQueryText.ts` 的 `set` 没有区分「值没变」与「值变了」

```ts
set: value => {
  void router.replace({ query: { ...route.query, [key]: value === fallback ? undefined : value } })
},
```
每次 set 都发起一次 `router.replace`，即使值与当前相同。Vue Router 会去重相同的导航，所以不构成 bug，但每次击键都走一遍导航钩子（`web/src/router.ts:114` 的 `afterEach(updateDocumentTitle)` 每次都会跑）。与 P1-3 同源。

### P3-5　`web/src/api.ts:774` 的 `usageByDay` 与 `web/src/api.ts:779-784` 的其他三个 by-\* 封装**签名形状不一致**（前两个参数必填 vs 有默认值）

已作为 P1-2 的证据列出，此处仅记为一致性 nit。

### P3-6　「直连（不经过代理）」的文案在三处自相矛盾

`web/src/views/Providers.vue:232` 与 `web/src/views/ProviderDetail.vue:928` 写「直连（不经过代理）」，而 `web/src/views/Networks.vue:158` 写「读进程环境变量里的 HTTP_PROXY / HTTPS_PROXY」。

这与 `docs/network-proxy-audit-2026-10-20.md` 的 **P2-5** 是同一条（直连实际走 `http.ProxyFromEnvironment`，`internal/upstream/upstream.go:1272`）。此处只记「前端侧同一问题在三处文案中两说一」这一事实，不重复展开修法。

### P3-7　`web/src/types.ts:1021-1022` 的 `warnings` / `notes` 声明在错误的类型里

见 P2-1 的证据 1。这两个字段属于 `ConfigImportResult`（`internal/admin/config_transfer_handler.go:204`），被误放进 `UpstreamModelUpdate`（模型 PATCH 的**请求体**类型）。

---

## 4. 「前端如何使用 token / 401 如何处理」（与 A7 交叉）

**结论：这一段是本仓库做得最好的部分之一，没有发现问题。**

**令牌存储**：`web/src/api.ts:48-50` 用 `sessionStorage`（**不是** localStorage），且 `web/src/api.ts:30-46` 有 17 行注释解释了为什么（localStorage 会让 8 小时有效期的凭证在用户关页后长期驻留）。这个决定是对的。

**全局 401 处理**：`web/src/api.ts:233-241`（`request()` 内）与 `web/src/api.ts:294-299`（`download()` 内）是**两份几乎相同的代码**：
```ts
if (res.status === 401) {
  saveToken('')
  session.me = null
  throw new ApiFail(401, '登录已失效，请重新登录')
}
```
两处必须一致（漏一处 → 导出按钮停在永远失败的态）。**它们目前一致** —— 但这是靠复制维持的，见 P2-6。

**401 语义单一化**：`web/src/api.ts:231-236` 的注释明确「401 只有一个含义：会话无效（未登录 / 过期 / 密码已改 / 账号被禁用）」。与 A7 的会话模型一致。

**改密后重新登录闭环**：`web/src/views/AccountSettings.vue:26-33`：
```ts
await api.changeMyPassword(oldPwd.value, newPwd.value)
saveToken('')
session.me = null
toast('密码已更新，请用新密码重新登录')
oldPwd.value = newPwd.value = confirmPwd.value = ''
await router.replace({ name: 'login', query: { redirect: '/account' } })
```
**闭环是完整的**：后端 `POST /me/password` 递增 auth_version 使会话立即失效（`internal/admin/user_handler.go` 的 `ChangePassword`），前端同步清 token + 清 me + 跳登录页并带上 `redirect: '/account'`。

**`redirect: '/account'` 的后续动作**：`web/src/router.ts:88-92` 的 `guardDecision` 对 `to.meta.public && session.me` 的分支会读 `to.query.redirect` 并回跳：
```ts
if (typeof r === 'string' && r.startsWith('/') && !r.startsWith('//')) return r
```
这里有**开放重定向防护**（排除 `//`）。重新登录后回到账号设置页，**合理**。

**`web/src/router.ts:52-56` 的 `/settings?tab=password` → `/account` 映射**：
```ts
if (to.name === 'settings' && to.query.tab === 'password') return { name: 'account' }
```
这条在 `guardDecision` 里，**位置在 `if (!session.me) return {name:'login'}` 之后**（`web/src/router.ts:80-88`）。未登录时访问 `/settings?tab=password` 会先去登录页，登录后 `redirect` 带着 `?tab=password` 回来 → 命中这条映射 → 转 `/account`。**闭环成立**。

**`session.checked` 相关的边界**：`web/src/router.ts:63` 的 `if (!session.checked) return true` 与 `web/src/App.vue:225-231` 的 `showShell` 组合起来，导致**硬刷新时受保护页不挂载**（App.vue 的注释详述了这个修复）。`web/src/router.ts:126-141` 的 `reapplyGuard()` 在 `loadSession()` 之后补做一次纠正。**这条链是完整的**。

---

## 5. 视图完整性：17 view × router × App 导航

### 5.1 路由 → 视图对应表

| 路由 (`web/src/router.ts`) | 视图文件 | App.vue 导航项 | 一致？ |
|---|---|---|---|
| `/login` | `views/Login.vue` | —（独立分支） | 是 |
| `/` overview | `views/Overview.vue` | 管理员/普通用户都有 | 是 |
| `/history` | `views/UsageStats.vue` | 有 | 是 |
| `/usage` | `views/UsageStats.vue` | 有 | 是 |
| `/models` | `views/Models.vue` | 仅普通用户 | 是 |
| `/wallet` | `views/Wallet.vue` | 有 | 是 |
| `/profile` | → redirect `wallet` | — | 是 |
| `/keys` | `views/AccessControl.vue` | 有 | 是 |
| `/users` | `views/AccessControl.vue` | 管理员 | 是 |
| `/providers` | `views/Providers.vue` | 管理员 | 是 |
| `/routes` | `views/Routes.vue` | 管理员 | 是 |
| `/model-groups` | → redirect `usage?tab=groups` | — | 是 |
| `/networks` | `views/Networks.vue` | 管理员 | 是 |
| `/providers/:id` | `views/ProviderDetail.vue` | 由列表页进入 | 是 |
| `/account` | `views/AccountSettings.vue` | 侧栏底部 | 是 |
| `/settings` | `views/Settings.vue` | 管理员 | 是 |
| `/:pathMatch(.*)*` | → redirect `/` | — | 是 |

**17 个 view 全部有路由或被路由引用**（`views/Upstream.vue` 除外 —— 见 P2-7，它是死代码）。`web/src/App.vue:104-135` 的导航项与 `web/src/router.ts` 的 adminOnly 标记**逐条一致**。

**标题/高亮三重一致性**：`web/src/router.ts:96-113`（浏览器标题）、`web/src/App.vue:139-147`（`activeNavTo`）、`web/src/views/AccessControl.vue:42`（h1）三处都按当前页签取值，`/users?tab=keys` 的情况被专门处理（`web/src/router.ts:100-104` 的注释与 `web/src/App.vue:139-147` 的实现）。**这个坑已经被前人发现并修好了**。

### 5.2 危险操作的确认与错误反馈

**全部到位**。逐条核对：

| 操作 | 确认框 | 错误反馈 | 位置 |
|---|---|---|---|
| 删上游 | 是（`danger: true`） | toast | `web/src/views/Providers.vue:132-150` |
| 删网络 | 是，且文案说明「上游会退回直连」 | 是 | `web/src/views/Networks.vue:119-136` |
| 删凭据 | 是 | 是 | `web/src/views/ProviderDetail.vue:364` |
| 删模型 | 是，且链首置灰 | 是 | `web/src/views/ProviderDetail.vue:643-659` |
| 删路由 | 是 | 是 | `web/src/views/Routes.vue:380` |
| 删密钥 | 是 | 是 | `web/src/views/Keys.vue:500-512` |
| 删用户 | 是 | 是 | `web/src/views/UsersGroups.vue:353` |
| 删分组 | 是 | 是 | `web/src/views/UsersGroups.vue:547` |
| 删模型组 | 是 | 是 | `web/src/views/ModelGroups.vue:121` |
| 充值 / 扣减 | 弹窗 `dismissable: false`，且提示「相对调整」语义 | 是 | `web/src/views/Wallet.vue:448` |
| 导入配置 | 预览 + dry-run | 有 warnings/notes 区 | `web/src/views/Settings.vue:120-140` |
| 改密码 | — | 是 | `web/src/views/AccountSettings.vue:14-38` |

**`web/src/ui.ts:40-52` 的 `confirmBox` 处理了「上一个确认框未结算」的正确做法**（`confirmState.resolve?.(false)` 先结算旧的），注释说明了「表现为『点了删除，界面卡住，什么也没发生』」这个具体症状。这是个**容易被 agent 改坏**的地方，当前是对的。

### 5.3 分页 / 筛选 / 排序与后端参数的匹配

| 页面 | 前端参数 | 后端 | 一致？ |
|---|---|---|---|
| 请求日志 | `?from&to&status&model&key_id&limit&offset` | `usageHistoryQuery` + `clampLimit(200, maxUsageLimit=1000)` | 是 |
| 请求日志 CSV | 同上（不含 limit） | `clampLimit(maxUsageLimit, maxUsageLimit)` = 1000 | 是，且截断有 `X-Export-Truncated` |
| 用量分组 | `?from&to&limit=200` | `clampLimit(defaultUsageLimit=100, maxUsageLimit=1000)` | 是 |
| 按天 | `?from&to`（**无 limit**） | 默认 100 | **否 —— P1-1** |
| 按用户 | `?from=0&to&limit=100` | `clampLimit(100, 1000)` + `total` | 是 |
| 充值流水 | `?limit=50&offset=0` | `defaultTopupLimit` / `maxTopupLimit` | 是 |
| 模型价格 | 本地分页（`PRICE_PAGE_SIZE`） | 全量 `allModels` | 是 |

**`web/src/components/UsageByGroup.vue:169`** 的截断提示：
```html
<span class="dim">{{ rows.length >= 200 ? '前 200 项' : '共 ' + rows.length + ' 项' }} · 按用量降序</span>
```
这个判断是**对的**（200 = 前端传的 limit），且 by-model / by-key / by-provider 三个维度都是 `ORDER BY tokens DESC`（`internal/admin/usage_handler.go:896`），**截断的是用量最小的** —— 与 by-day 的升序正好相反，**再次证明 by-day 是异常的那个**。

**但 `usageByDay` 的调用方（`web/src/views/Wallet.vue:190`）用的是 `api.usageByDay(dayFrom, now)`，它没有 limit 参数** —— 这正是 P1-1 的前端侧。

---

## 6. 「前端如何不再每次都被改坏」的最小机制建议

按投入产出排序。**第 1 条是根因，其余是防复发。**

### 建议 1（根因）：从 Go 的 response struct **生成** TS interface

现在 `web/src/types.ts` 的 40KB 是手抄的，任何人改后端字段都会漂移。最小可行做法：

1. 写一个 `web/scripts/gen-types.mjs`，或用 `//go:generate` 在 `internal/admin/` 加一个生成器；
2. 用 `go/ast`（或 `go doc`/`gopls` 的 JSON 标签导出）解析 `internal/admin/*.go` 里所有 `type xxxResponse struct`，读 `json:"..."` tag 与 Go 类型；
3. 输出 `web/src/types.gen.ts`；
4. `web/src/types.ts` 里手写的地方只保留**注释与语义说明**（那些是有价值的），字段本身引用生成物。

**为什么这条最值**：它一次性消灭 P2-1、P2-2、P3-7（部分），并且让 P1-4 这类「后端有但前端没用 / 前端注释说没有」的问题在生成时暴露。

**替代方案**（如果不想引入生成器）：在 CI 里加一步「契约快照测试」—— 后端每个 handler 写一个 httptest，断言响应 JSON 的**键集合**；前端写一个 `.test.mjs` 断言 `types.ts` 的 interface 键集合与之相同。两边任何一侧改字段而另一侧没改，测试红。

### 建议 2：给 `api.ts` 的每个封装加一条**构建期可查**的端点清单测试

现在 `web/src/api.ts` 有 60+ 个端点封装，**没有一个被测试覆盖**。加一个 `web/tests/api-paths.test.mjs`：

- 用一个假 `fetch` 拦截器，依次调用 `api` 的每个封装；
- 断言拼出的 `method + path + query` 与一份**手写的期望表**一致；
- 期望表由 `cmd/gateway/main.go` 的注册表生成或人工维护。

**这能抓住**：前端调用但后端没有的端点（拼错的路径）、后端加了但前端没用的端点（对比清单）、query 参数名漂移。**这是 P1-1 那一族问题的直接防线。**

### 建议 3：把 `types.ts` 与 Go struct 的**可空性**一次性对表，并写进 `types.ts` 头部

具体做法：给 `types.ts` 每个 interface 加一段注释，标明哪些字段后端是 `omitempty`（可能缺席）。或者更省事：生成时直接把 `omitempty` 映射成 `?:`，非 omitempty 映射成必填 —— 这样 P2-2 那种「`?? 1` 把无样本读成 100% 成功」的兜底会被编译器逼出来。

### 建议 4：引入 `useAsyncSeq()`，统一所有视图的竞态与卸载处理

一次实现，全部套用（见 P2-3 的对照表）。这是**最容易在下一个 agent 的改动里被改坏**的一类代码 —— 因为每个视图自己写一遍，形状还略有差异（有的用 `seq`、有的用 `id !== providerId.value`、有的完全没有）。

### 建议 5：把「三处必须同时清 token + 清 me」收敛成一个函数

见 P2-6。`signOutLocally()` 一个函数，三处调用，`auth.token` 改只读。**改动量 10 行，消除一条隐式契约。**

### 建议 6：把 `npm run sync` 纳入**强制**的提交前检查

当前 `web/package.json:15` 有 `"sync": "node sync-embed.mjs"`，但 `internal/webui/dist` 是 gitignore 的，所以「源改了、dist 没同步」在 git 层面**完全不可见**。当前工作区就停在这个中间态（P2-9）。

最小做法：加一个 `web/scripts/check-sync.mjs`，比对 `web/dist` 与 `internal/webui/dist` 的文件清单与内容哈希，不一致就 `exit 1`；接到 `npm run build` 之后或 CI 里。**这不需要改 embed 链路（A1 的范围），只是在构建后加一道比对。**

---
## 7. 已验证无问题的区域（及理由）

| 区域 | 结论 | 依据 |
|---|---|---|
| **类型系统强度** | 无 `any` 泛滥 | 全仓库仅 `web/src/api.ts:248` 一处 `let data: any = null`（JSON.parse 的中间值，紧接着被 `as T` 断言）；`rg 'as any\|: any\|<any>'` 在 `web/src` 下**零命中**（排除 CSS 的 `overflow-wrap: anywhere` 误匹配） |
| **TypeScript 严格度** | `strict: true` | `web/tsconfig.json:6`；`npm run typecheck` 0 错误通过 |
| **PATCH 语义（字段级部分更新）** | 前后端一致 | 后端 `internal/admin/key_handler.go:570-611`、`internal/admin/provider_handler.go:316-388`、`internal/admin/model_handler.go:343-403` 全部用 `*T` 指针区分「缺席 = 不改」与「传 0/空串 = 显式清空」；前端 `web/src/types.ts:81-96`（`ProviderUpdatePayload`）、`:347-368`（`UpstreamModelUpdate`）、`web/src/api.ts:577-620`（`updateKey`）的注释逐条对应 |
| **`supports_thinking` 三态** | 完整往返 | 后端 `internal/admin/model_handler.go:66-79` 的 `nullableBool`（缺席 = 不改 / `null` = 清除 / true\|false = 显式）；前端 `web/src/types.ts:361`（`supports_thinking?: boolean \| null`）+ `web/src/views/ProviderDetail.vue:578`（`'unset' ? null : ...`） |
| **模型名 / 价格 / 探测的同源对齐** | 一致 | 后端 `internal/admin/group_handler.go:313-314`（`priceForModel` 与 `metaForModel` 走同一条 `Resolve` / 同一个 `Candidates[0]`）；前端 `web/src/views/Models.vue:127-131`（`priceOf` / `upstreamIdOf` / `providerOf` 三个都从 `metaMap`/`priceMap` 取） |
| **`model-names` 的平行数组契约** | 一致 | 后端 `internal/admin/group_handler.go:284-324`（`all` / `prices` / `meta` 在同一循环构造，且三者按 `Name` 同步排序）；前端 `web/src/api.ts:706-724` 逐位对应的说明 + `web/src/views/Models.vue:225-227` 建成 Map（不按下标） |
| **`effort_levels` 的字符串 ↔ 数组** | 一致 | 响应是数组（`internal/admin/model_handler.go:147`），写入是逗号分隔字符串（`:53`）；前端 `web/src/types.ts:347`（`UpstreamModelUpdate.effort_levels?: string`）与 `:174`（`UpstreamModel.effort_levels: string[]`）**刻意用两个类型**，注释说明了理由 |
| **空集合语义（不限制 vs 什么都看不到）** | 一致 | 后端 `internal/admin/group_handler.go:346-361`（交集为空返回空数组而非全集）+ `internal/admin/probe_handler.go:96-104`（`(nil, true)` = 不限制 / `(空 map, true)` = 空集，两个语义分开）；前端 `web/src/views/Models.vue:38-46`（空清单**绝不**写成「不限制」）+ `web/src/views/Keys.vue:130-140`（`anyLimit` 兜底显示「不限」） |
| **401 全局处理** | 一致 | `web/src/api.ts:233-241`（request）与 `:294-299`（download）两处逻辑**完全一致**：清 token + 清 me + 抛 ApiFail(401) |
| **令牌存储** | sessionStorage | `web/src/api.ts:48-50` + `:30-46` 的 17 行理由注释；不用 localStorage |
| **改密后重新登录闭环** | 闭环 | `web/src/views/AccountSettings.vue:26-33`（清 token + 清 me + 跳登录页带 redirect）+ `web/src/router.ts:52-56`（`/settings?tab=password` → `/account`）+ `web/src/router.ts:88-92`（redirect 开放重定向防护，排除 `//`） |
| **硬刷新时的路由纠正** | 闭环 | `web/src/router.ts:126-141`（`reapplyGuard`）+ `web/src/App.vue:249-262`（`loadSession` 后调用）+ `web/src/App.vue:225-231`（`showShell` 在 `checked` 前不挂载任何页面，避免白跑请求）—— 三处配套 |
| **危险操作的确认** | 12 类全部有 | 见 5.2 的表；`web/src/ui.ts:40-52` 的 `confirmBox` 正确处理了「未结算的上一个确认框」 |
| **确认框的默认 dismissable** | 已修 | `docs/frontend-shared-components-2026-10-18.md` 记录「修复未传 dismissable 时被 Vue 转为 false 的默认值问题」；`web/src/components/AppModal.vue:54` 现为 `{ dismissable: true }` |
| **`DataTable` 的加载/错误/空/旧数据四态** | 完整 | `web/src/components/DataTable.vue` 提供 `loading` / `error` / `has-data` / `empty` 四个 prop + `@retry`，是本仓库最完整的失败态实现（模式 A） |
| **`useNetworks` 的竞态保护** | 完整 | `web/src/composables/useNetworks.ts:38-50` 的 `loadSeq` 三处判定齐全（数据、错误、loading 都判 `seq === loadSeq`） |
| **空串 vs 未配置 vs 免费的三分** | 一致 | 贯穿 `web/src/views/Models.vue:139-146`（`priceCell`）、`web/src/components/UsageByGroup.vue:130-134`（`costTitle`）、`web/src/views/Settings.vue:474-478`（`priceCell`）、`web/src/views/UsageDetail.vue:112-116`（`speedText`）—— 0 一律显示「未配置」/「—」，不显示 0.00 |
| **截断必须可见** | 除 by-day 外一致 | `web/src/api.ts:332`（`download` 读 `X-Export-Truncated`/`X-Export-Total`）+ `web/src/views/UsageDetail.vue:130-135`（CSV 截断提示）+ `web/src/components/UsageByGroup.vue:169`（前 200 项）+ `web/src/views/Wallet.vue:920-923`（充值流水截断）+ `web/src/views/Wallet.vue:705-712`（by-user 截断）—— **by-day 是唯一漏网的**（P1-1） |
| **时间区间口径统一** | 一致 | `web/src/range.ts` 是唯一实现（`localMidnight` / `rangeStart` / `rangeBounds` / `localDayKey`），`web/src/views/Overview.vue:114-118` 明确「不能在这里另算一份」；后端 `internal/store/usage_archive.go:465` 的 `dayExpr` 同为 localtime |
| **`now` 只取一次** | 一致 | `web/src/views/Wallet.vue:180` 的注释详述「各调一次 Date.now() 会让两个 to 差几毫秒…那是我自己造出来的假不一致」—— 正确的纪律 |
| **路由与视图一一对应** | 16/17 | 见 5.1 的表；唯一例外是 `views/Upstream.vue`（死代码，P2-7） |
| **导航 / 标题 / h1 三重一致** | 一致 | `web/src/router.ts:96-113` + `web/src/App.vue:139-147` + `web/src/views/AccessControl.vue:42` |
| **`api.ts` 的端点与后端路由对应** | 无「前端调后端没有」 | 逐一比对 `web/src/api.ts` 的 60+ 个封装与 `cmd/gateway/main.go:374-517` 的注册表：**全部存在**。后端有而前端未用的只有 3 个：`GET /admin/api/usage`（明细列表，前端用 `/usage/history` 代替）、`GET /admin/api/providers/{id}`（见 P1-4）、`POST /admin/api/reload`（2026-10-10 刻意废弃，见 `web/src/api.ts:320-376`） |
| **`/usage/by-user` 与 `/usage/prune` 的 admin-only** | 一致 | 后端 `internal/admin/usage_by_user_handler.go:85-88` 与 `internal/admin/usage_handler.go:959-962` 都有 `requireAdmin`；前端 `web/src/views/Wallet.vue:197-205` 管理员才发 by-user |
| **审计端点保留但界面移除** | 有意为之 | `web/src/views/Settings.vue:21-23` 明确说明「删掉的是界面上的那一块，不是能力本身」，`api.audit()` 仍在 `web/src/api.ts:682` |
| **「余额是分、费用是元」的单位口径** | 一致 | `web/src/views/Wallet.vue:903-905` 用 `fmtMoney(Math.abs(t.delta_cents) / 100)`，与后端「delta_cents 单位是分」一致；`web/src/types.ts:696-704` 注释说明 `balance_cents` 语义 |
| **登录页的三形态互斥** | 一致 | `web/src/views/Login.vue:41-46` 的 `mode` computed 保证「无法连接 / 首次设密码 / 登录」三张表单**永不同时出现**；免鉴权窗口关闭后 `session.needsSetup = false`（`web/src/api.ts:216`） |

---

## 8. 附：本次审计用到的验证手段（可复用）

1. **端点比对**：`web/src/api.ts` 的 60+ 个路径用正则抽出，与 `cmd/gateway/main.go:374-517` 的 `mux.HandleFunc` 注册表逐一对照。
2. **类型比对**：`web/src/types.ts` 的 12+ 个 interface 与 `internal/admin/*.go` 的 `type xxxResponse struct` 对照（含 `omitempty` 与可空性）。
3. **by-day 截断实跑**（合成 SQLite）：
   ```sql
   SELECT strftime('%Y-%m-%d', ts/1000,'unixepoch','localtime') AS key, COUNT(*)
   FROM usage_records GROUP BY <dayExpr> ORDER BY key ASC LIMIT 100
   ```
   分别模拟 400 天（「全部」）、365 天（「近 1 年」）、90 天（钱包页）三个窗口，确认被截掉的是**最早**的数据。
4. **真实库只读检查**：`bin/data/gateway.db`（8333 条 usage_records，9 个有记录日，32 个上游模型，8 条路由）—— 确认 by-day 在当前数据量下**尚未**触发（9 < 100），所以这个 bug 要等历史超过 100 天才显形。**这是它能一直藏着的原因。**
5. **未使用 import 扫描**：对 `web/src/**` 的每个 `.vue` 提取 import 名，统计其在 `<script>` 主体中的出现次数。
6. **dist 只读比对**：`git show HEAD:internal/webui/dist/assets/UsageStats-yQCFwsvU.css` vs 工作区的 `UsageStats-BaFrZ4Kw.css`，比对 `.model-column` 与 nowrap 规则。

---

## 9. 未覆盖 / 待确认

| 项 | 原因 |
|---|---|
| 浏览器实测（1280/390px 渲染、CSS 特异性、横向溢出） | 本轮为只读审计，未跑 dev server；P1-4 里「300px 是否真的不溢出」属**待确认**（注释里的算术没有实测支撑） |
| `sync-embed.mjs` 的行为 | 规则明确禁止运行；P2-9 只做了 dist 的只读比对 |
| 鉴权 / CSRF / JWT / scope 的正确性 | 归 A7；本报告只从「前端如何使用 token、401 如何处理」角度核对（第 4 节） |
| CSP / embed 链路 | 归 A1 |
| 代理 / 直连链路 | 已在 `docs/network-proxy-audit-2026-10-20.md` 记录；本报告只在 P3-6 记前端文案侧的事实 |
| 4 份 docs 里的「待办清单」 | 逐条读过后：IA 评审的 P1（主路径与密度）与 P2（公共组件 + 聚合接口）**已落实**（见 `docs/frontend-shared-components-2026-10-18.md` 的验收表）；「路由独立页面式编辑」文档明确记为「按用户要求暂缓」；正式验收文档记的 3 处修复（probe JSON 标签 / 总览口径 / 推荐值单位）**已在代码中**。**未发现文档与代码矛盾之处** —— 唯一与文档不符的是 `web/src/views/ModelGroups.vue:197` 仍把旧的宿主指向死代码 `Upstream.vue`（P2-7） |
