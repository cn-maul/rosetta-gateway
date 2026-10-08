# 任务 10：普通用户「我的账号」→「钱包与充值」（wallet-ui）

日期：2026-10-10

## 需求原文

> 普通用户我的账户页面改为钱包与充值，里面强调显示余额，消耗记录以及充值记录，
> 显示一个总消费值。

## 结论速览（给 Lead）

| 项 | 结论 |
| --- | --- |
| 组件路径 | **新建 `web/src/views/Wallet.vue`**。`Profile.vue` 保留为一行转发壳。 |
| 路由 `name` / `path` | **保持 `profile` / `/profile` 不变**。你已经这么配了，正确。理由见下。 |
| 导航文字 | 「钱包与充值」——你已改（`App.vue` 第 251/257 行的 `to="/profile"` 不用动）。 |
| 余额 / 总消费 / 消耗记录 | **已实现**，走现有接口。 |
| 充值记录 | **后端无接口，页面给不出数据**。已在页面上以「说明 + 指引」占位，接口设计见第 4 节。 |
| `api.ts` / `types.ts` | **未改动**。现有接口够用（详见第 2 节「为什么没加接口」）。 |
| 验证 | `vue-tsc --noEmit` **exit 0**；`vite build` 在本组件上 **exit 0**（见第 6 节的诚实说明）。 |

---

## 1. 页面结构

单列，顶部一张余额卡 + 三个胶囊页签（形态对齐 `Settings.vue` 的 `.set-tabs`，
刻意不用下划线式 tab——那会被读成「可切换的独立页面」，而这三格同属钱包、
共享同一个标题与刷新按钮）。

```
钱包与充值                          [刷新]
┌───────────────────────────────────────────────────────┐
│  当前余额          待结算            总消费            │
│  ¥12.34           ¥0.0032           ¥45.67            │
│  可用余额。耗尽…   已消费但不足一分…  全部历史累计，    │
│                   攒够一分后自动扣除。共 12.3K 次调用，│
│                                    均次 ¥0.0037。      │
└───────────────────────────────────────────────────────┘
[消耗记录] [充值记录] [修改密码]
─────────────────────────────────────────────────────────
消耗记录
  最近消耗            近 30 天，最多 15 条
  时间   调用模型   Tokens   状态   费用
  ─────────────────────────────────────────
  …最近 15 条调用…
                          [查看全部调用记录 →]   ← 跳 /history
  ─────────────────────────────────────────
  Token 额度  已用 1.2M/5M   额度按 token 计量，耗尽后更换密钥即可；
                             余额按人民币计量，耗尽需充值。
```

### 为什么余额占这么大一块

改造前这一页是「我的账号」，头号信息是身份与 token 额度——那是**管理员视角**
的东西。余额计费落地后普通用户来这一页只有一个诉求，所以整页重排成
「余额 → 消费 → 流水」，改密降级成最末一个页签。

余额区的列宽是 `2fr : 1fr : 1fr`，**刻意不对等**：另外两项是解释性信息，
与主位同权重会让「不限」或一个 4 位小数的余数抢走注意力——而 0 元余额才是本页
唯一需要立刻被看见的东西。

### 三个余额态必须分开（与 `fmt.ts` 的既有约定一致）

| 状态 | 显示 | 依据 |
| --- | --- | --- |
| 管理员 / 不限额 | 「不限」+ 说明「不受余额限制」 | `balance_unlimited` |
| 有限额 > 0 | `¥xx.xx` | `balance_cents` |
| 有限额 = 0 | `¥0.00`，**危险色** + 「会被拒绝（402），请联系管理员充值」 | `balance_cents === 0` |

「不限」与「0.00 元」含义相反：一个永远放行，一个会被 402 拒绝。所以判定
一律走 `fmtBalance(cents, unlimited)`，**没有**自己写 `cents / 100` 绕开它。

### 额度为什么没和余额并排

`quota_tokens` 与 `balance_cents` 是两件事，耗尽后的处置动作**相反**：额度耗尽
换个 key 就行，余额耗尽必须充值。塌成一行会被读成「同一个东西的两种单位」，
于是用户按前者的经验处理后者——换完 key 发现还是 402，只能回头找管理员。
所以额度降级成消耗记录页签底部的弱化单行，并配一句「额度按 token 计量，余额按
人民币计量」。

### 待结算为什么单独成列而不是塞进括号

改造前 `Profile.vue` 的写法是 `¥12.34（另有 0.0032 待结算）`。它现在单独成列，
因为括号里的东西多数用户根本不会去读——而「余额为什么没动」是余额计费落地后
最常见的疑问。`fmtRemainder(0)` 返回空串，所以余数为 0 时这一列显示「—」，
不制造噪音。

---

## 2. 数据来源

一次 `load()`，`Promise.all` 并发两个请求 + `reqSeq` 序号守卫（写法对齐
`Overview.vue` / `History.vue`）。

| 展示项 | 来源 | 口径 |
| --- | --- | --- |
| 当前余额 / 待结算 | `session.me`（`/admin/api/me`） | `balance_cents` / `balance_remainder` / `balance_unlimited` |
| 总消费 | `api.stats(0, Date.now())` → `cost` | **全部历史**区间合计 |
| 累计调用次数 / 均次消费 | 同上 → `total_requests` | 同上 |
| 最近消耗记录 | `api.usageHistory(30, 15, 0)` | 近 30 天，15 条 |
| 额度 | `session.me` → `quota_tokens` / `used_tokens` | — |

### 总消费为什么不能用「本页 15 条求和」

`rows` 是最近 15 条，把它 reduce 出来当「总消费」有两个问题：少算一大截；且这个
数每刷新一次就变一次，用户会读成「总消费在变」。真正的合计只能来自 `stats` 的
区间汇总——而且因为费用是**落库当时**按单价算好后固化的 `cost_total` 再求和
（改价不回溯历史），「全部」档的口径是准确的。

`stats(0, …)` 传 `from=0` 是安全的：stats 端点区分「显式传 0」与「没传」
（`stats_handler.go` + `queryRangeExplicit`），且数据源是 `UsageSource`
（明细 ∪ 日归档），30 天明细被剪掉后费用不会缩水。

顺带给「总消费」配了**均次消费**作参照物——单看一个总额用户无法判断量级。
0 次调用时不计算，避免除零显示 `NaN 元`。

### ⚠️ 一处必须记下的后端陷阱：`/usage/history` 不认 `from=0`

我最初写的是 `api.usageHistory(0, 15, 0)`（照 `range.ts` 的「days=0 = 全部历史」）。
**这是错的**，已修。`range.ts` 的 `days<=0 → from=0` 对 **stats** 成立，对
**usage history 不成立**：

- `/admin/api/stats` 用 `queryRangeExplicit`，`explicit = rawFrom != ""`，
  显式 `from=0` 被正确理解为「全部历史」；
- `/admin/api/usage/history` 用 `queryRange`，里面有
  `if from == 0 { from = time.Now().Add(-window) }`——它把 `from == 0`
  解释为「请求里没带 from」，回落到默认窗口（近 7 天）。

于是传 0 的真实后果是：一张标注「最近消耗」的表里混进 7 天前的记录，而且它
排在最前面——用户会认为「我刚花了钱」，实际那是一周前的事。**静默的错数据比
报错难查**，所以改用显式的 `RECENT_DAYS = 30`，并把时间窗口写进面板标题
（「近 30 天，最多 15 条」），不说的话用户会把它读成「我的全部调用」。

> **给后端/Lead 的建议（我没有改 Go）**：`queryRange` 与 `queryRangeExplicit`
> 的这个差异是个真实的坑，`History.vue` 传 `days=0` 时同样会中招。修法是让
> history 端点也用 `queryRangeExplicit`，或在 `queryRange` 里区分
> 「未传」与「显式 0」。

### 为什么没有加 api.ts / types.ts 的接口

现有 `api.stats` + `api.usageHistory` + `session.me` 已经覆盖全部需求，
`Stats` / `UsageHistoryEntry` / `Me` 的字段上一轮都补齐了。两个文件这次
**一行未动**。唯一缺的是充值流水（后端没有数据源，见下），而那属于「调后端
新接口」，不是前端能补的。

---

## 3. 消耗记录

精简列表（时间 / 模型 / tokens / 状态 / 费用）+ 「查看全部调用记录 →」跳
`/history`。**没有**复制 `History.vue` 的分页、时间范围、状态/模型/密钥过滤
和 CSV 导出：

- 那是排障工具，一页塞满控件会淹没余额区这个主角；
- 两份筛选器会各自演化，用户拿本页的结论去调用历史页核对会对不上。

保留了流式/非流式徽章（首字时间的含义随它而变，不标注则耗时数字无法解读），
费用用 `fmtMoney` 的高精度档——单次费用可能远低于一分，两位小数会让整列显示成
「0.00 元」，恰恰掩盖了「确实花了钱」并解释不了余额为何没动。

---

## 4. 充值记录：**确认无接口，需后端补**

### 查证结论：后端确实没有任何充值留痕

逐条看过：

1. **`internal/admin/user_admin_handler.go` 的 `AdjustBalance`**（第 559 行）——
   校验 `delta_cents` 非 0、负数不超额（避免 SQL 侧 `MAX(0, …)` 夹到 0 造成
   「部分成功」），然后调 `store.AdjustBalance`，**回读余额拼响应，全程不写任何表**。
2. **`internal/store/balance_dao.go` 的 `AdjustBalance`**（第 465 行）——只有一句：
   ```sql
   UPDATE users SET balance_cents = MAX(0, COALESCE(balance_cents, 0) + ?),
                    updated_at = ? WHERE id = ?
   ```
   没有 INSERT，没有审计表。
3. **`balance_charges` 表**——记的是**扣费**（`amount_cents` 为实际扣掉的分数，
   主键 `(request_id, user_id)` 做幂等），方向与充值相反，也不含充值金额。
4. **`audit_log` 表**——`server.AutoReload` 确实会为这次写操作落一条审计，但
   `extractFieldNames` **只存请求体的顶层字段名**（`fields="delta_cents"`），
   刻意不存值（注释理由：body 里会出现 api_key 明文，值的泄露风险远大于排查收益）。
   **金额从未进入任何表。**

所以「充值记录」在当前后端上**在原理上不可查**，不是缺个查询端点的问题。

### 页面上为什么给说明而不是空表

一张空表会被读成「我从来没被充过值」——而真实情况是他上周刚充了 100 元，
只是系统没记。那是比「没有这个功能」更糟的误导。所以这一格给的是：

- 明确的「暂无充值记录 / 暂不可用」；
- **成因说明**（余额变更只更新账户数字，不留记录）；
- **操作指引**（充值需联系管理员在「用户」页操作）；
- 一句「这不是加载失败——账户当前的余额与总消费是准确的」，避免与真正的
  请求失败混淆。

### 建议的接口设计（**我没有写 Go 代码**）

**建表**（放在 `store.go` 的 migrations，紧邻 `balance_charges`）：

```sql
CREATE TABLE IF NOT EXISTS balance_topups (
  id           TEXT PRIMARY KEY,          -- 短哈希，与 users/keys 同风格
  user_id      TEXT NOT NULL,
  delta_cents  INTEGER NOT NULL,         -- 正=充值，负=管理员扣减（同 AdjustBalance 口径）
  balance_after INTEGER NOT NULL,        -- 调整后余额（分）；不限额用户记实际值
  was_unlimited INTEGER NOT NULL DEFAULT 0, -- 充值前是否不限额（充值会把 NULL 切成有限额）
  actor        TEXT NOT NULL,            -- 操作者用户名，管理员充值必填
  remark       TEXT NOT NULL DEFAULT '', -- 管理员可填：「微信转账 100」
  ts           INTEGER NOT NULL          -- 毫秒
);
CREATE INDEX IF NOT EXISTS idx_balance_topups_user ON balance_topups(user_id, ts);
```

为什么要有 `balance_after` / `was_unlimited`：充值**前**是否不限额决定了这次
充值的语义（`COALESCE` 把 NULL 当 0 起算，所以给不限额用户充 +100 元会**把
「不限」切成 100 元**，见 `balance_dao.go` 的注释）。用户看到「我充值后怎么
就不能无限用了」时，只有 `was_unlimited` 能回答。

**写入点**：唯一的写入方是 `UserHandler.AdjustBalance` 成功之后。**不要**依赖
`AutoReload` 的通用审计——那条路径拿不到调用者身份（`audit_dao.go` 的注释说
明 `actor` 目前恒为 `"admin"`），也不该把资金流水寄存在通用审计里。

**读取端点**（新增，走 `userAccessiblePrefixes` 白名单）：

```
GET /admin/api/me/topups?limit=20&offset=0
```

- **作用域**：普通用户只能看自己的（照 `usage_handler.callerScope` 的做法，
  从会话身份取 `user_id`，**绝不**从 query 读 `?user_id=`）。要在
  `internal/server/user_auth.go` 的 `userAccessiblePrefixes` 加
  `"/admin/api/me"` 前缀——**已经有了**（第 140 行），所以这个端点默认就对普通
  用户开放，前端不用改白名单。
- 响应：`{ "records": [...], "total": N }`，与 `UsageHistoryPage` 同形，
  前端可以直接复用 `api.usageHistory` 那套分页写法。

每条记录建议的字段：

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| `ts` | number | 毫秒，前端用 `fmtDateTime` |
| `delta_cents` | number | 有符号，前端按正负决定「充值/扣减」与颜色 |
| `balance_after` | number | 调整后余额，分 |
| `was_unlimited` | boolean | 充值前是否不限额 |
| `actor` | string | 操作的管理员（**普通用户能看到**：「谁给我充的」本身就是对账依据） |
| `remark` | string | 管理员备注 |

前端侧接入成本很小：`api.ts` 加一个 `topups()`，`types.ts` 加一个接口，
`Wallet.vue` 的「充值记录」页签把说明换成表格即可。

---

## 5. 改名建议：保持 `name`/`path` 为 `profile`（你已经这么做了）

**建议：不改 `name`/`path`，只改展示标题。** 你现在的配置正是这个形态
（`router.ts:15`：`path: '/profile'`、`name: 'profile'`、`component: Wallet.vue`、
`meta.title: '钱包与充值'`），`App.vue` 的 `to="/profile"` 也不用动。

理由：

1. **hash 路由的路径是外部分发物**。`/admin/#/profile` 会被贴进聊天、工单、
   收藏夹。改 path 等于让这些已分发的链接全部 404——而这一页恰恰是
   「账号出问题时用户被送去的地方」，它失效的代价最高。
2. **守卫里硬写着 `{ name: 'profile' }`**（`router.ts:83`，`guardDecision` 里把
   撞 admin-only 页的普通用户送回这里）。改 `name` 会在守卫里留下悬空引用，
   而这个悬空引用的表现是「点了用户管理 → 跳到一个空路由」，不报错。
3. `meta.title` 已经驱动了浏览器标题（`router.afterEach`），展示层改名是完整的。

**如果以后确实想要 `/wallet` 这个路径**，正确做法是加 alias 而不是换 path：

```ts
{
  path: '/profile',
  name: 'profile',
  component: () => import('./views/Wallet.vue'),
  alias: '/wallet',                       // 老书签继续有效
  meta: { title: '钱包与充值' },
}
```

### Profile.vue 的处置

我把它改成了**一行转发壳**（`<template><Wallet /></template>`）。

- 保留的理由同改名建议：`/profile` 这个路径仍然有效，任何人（包括导航守卫）
  都可能引用它，删掉文件会让那条路由变成悬空 import。
- 壳里**不**放任何状态与数据加载——壳里再抄一份 `session.me` 就会与 Wallet
  的那份漂移（两份余额可能不一样）。壳的职责只有「让 `/profile` 渲染出钱包页」。

> ⚠️ **注意**：你的 `router.ts` 已经指向 `Wallet.vue`，所以 `Profile.vue` 现在
> **没有路由引用它**。两个选择都行，我保留了转发壳（防止 `Profile.vue` 被
> 别的引用或后续合并时静默丢失）。如果你更希望它彻底消失，删掉这个文件是安全的
> —— 我会顺手确认没有别处 import 它。**这条请你定，我没有替你删。**

---

## 6. 验证

```
vue-tsc --noEmit   →  exit 0
vite build         →  exit 0
```

**关于 `vite build` 的诚实说明**：仓库里直接跑 `vite build` 目前会失败，但
**失败点不在本组件**：

```
Could not resolve "./views/UsersGroups.vue" from "src/router.ts"
```

`router.ts`（你正在改、我被禁止触碰）已经指向 `./views/UsersGroups.vue`，而那个
文件还没建出来。这是你在做的「用户与分组」合并页。`vue-tsc --noEmit` 能过是因为
它不解析动态 import 的目标。

为了证明**本组件本身**构建正常，我在系统临时目录（**不在仓库内**）建了一个
带桩的 vite 配置，只对缺失的 `UsersGroups.vue` 打桩，其余完全走仓库的真实配置：

```
✓ 64 modules transformed
dist/assets/Wallet-DReB7_BC.js    7.32 kB │ gzip: 3.79 kB
dist/assets/Wallet-BvJwGbP7.css   2.52 kB │ gzip: 0.84 kB
✓ built in 1.40s
vite exit: 0
```

`Wallet.vue` 正常产出独立 chunk（路由懒加载生效）。**你把 `UsersGroups.vue`
建好之后，仓库里的 `vite build` 应该就能直接过。**

另外：按要求**没有**跑 `sync-embed.mjs`，也没有 `git commit`。
`internal/webui/dist` 未被我的验证改动（临时构建输出到了 temp 目录，已清理）。

---

## 7. 其它发现

1. **`queryRange` 的 `from=0` 陷阱**（详见 2 节）。`History.vue` 传 `days=0`
   时同样中招。建议让 history 端点也用 `queryRangeExplicit`。
2. **`/admin/api/me` 已在普通用户白名单里**（`user_auth.go:140`），所以将来加
   `/me/topups` 不需要改白名单——只要前缀是 `/admin/api/me` 就自动放行。
3. **`Stats` 里也有 `balance_cents` / `balance_remainder`**（`stats_handler.go`
   会 `BalanceOf` + `BalanceRemainderOf` 回库读）。本页的余额**刻意用
   `session.me` 而不是 `stats`** 的同名字段：两者同源，但 `/me` 是身份的权威
   快照（`loadSession` 已拉过，零额外成本），而 stats 的余额是「读失败时
   fail-open 成不限额」的展示字段——拿它当余额会在读库失败时显示成「不限」，
   那是**危险方向的错误**（用户会以为能随便用）。本页宁可显示错误态。
4. **`fmt.ts` 的 `fmtBalanceWithRemainder` 本页没用**。它把余额与余数合成
   「¥12.34（另有 0.0032 待结算）」一行，正是我认为在钱包页**不够醒目**的
   那种形态（见 1 节）。该函数在 `Users.vue` 里仍然合适（那是 admin 的密集
   表格），不需要删。
5. **审计的 `actor` 恒为 `"admin"`**（`audit_dao.go:15` 的注释已自认不精确）。
   充值流水表设计里的 `actor` 字段应当从会话身份显式取，不要沿用这条路径。
6. **次数别用 `fmtTokens`**。`fmtTokens` 是 K/M/B 紧凑档，用在「次调用」上会
   显示成「12.3K 次调用」——那个 K 会被读成 token 数。本页的调用次数用
   `fmtNum`（与 `Overview.vue` 的「总请求」卡片同口径），tokens 列才用
   `fmtTokens`。这是我做这一页时自己踩到并修掉的，供后续页面参考。
