# 充值流水（balance_topups）实现报告

## 背景：为什么需要这张表

充值此前**完全没有留痕**。`AdjustBalance` 只做一句
`UPDATE users SET balance_cents = …` 就返回，于是「谁给谁充了多少钱」在原理上无法回答：

- `balance_charges` 记的是**扣费**（方向相反的减法），不是充值；
- `audit_log` 刻意**只存字段名不存值**（`audit_dao.go` 的注释说明了理由：
  body 里可能有 api_key 与密码明文），所以它能证明「有人调过充值接口」，
  却说不出「充了多少、给了谁」。

余额可以凭空增加（管理员手误、误操作，或将来任何一条写错 `user_id` 的代码），
而事后没有任何东西能与它对账。本表把「钱进来」这一侧补齐。

---

## 一、表结构与每字段理由

DDL 位于 `internal/store/store.go` 的 `migrations` 列表（`CREATE TABLE IF NOT EXISTS`）。

```sql
CREATE TABLE IF NOT EXISTS balance_topups (
    id                TEXT PRIMARY KEY,
    user_id           TEXT NOT NULL,
    delta_cents       INTEGER NOT NULL,
    balance_after     INTEGER NOT NULL,
    was_unlimited     INTEGER NOT NULL DEFAULT 0,
    operator_id       TEXT NOT NULL,
    operator_username TEXT,
    ts                INTEGER NOT NULL,
    remark            TEXT
);
CREATE INDEX IF NOT EXISTS idx_balance_topups_user ON balance_topups(user_id, ts);
```

| 字段 | 类型 | 理由 |
|---|---|---|
| `id` | TEXT PK | 32 位十六进制，与 `generateTargetID` / `generateTopupID` 同构。前端要拿它做 `:key`，缺了就只能按索引渲染。 |
| `user_id` | TEXT NOT NULL | **被调整余额的人**，不是操作者。两者混在一列会让「这个人的充值记录」查询答非所问（那是「谁充过钱」）。 |
| `delta_cents` | INTEGER NOT NULL | 相对调整量，**保留符号**：正=充值，负=扣减。不用绝对值，因为充值与扣减走同一个端点，存增量才能让「充了多少」和「扣了多少」在同一条列表里读得通。 |
| `balance_after` | INTEGER NOT NULL | 调整**之后**的余额。只有这一列能回答「这笔钱现在还在不在」；`users.balance_cents` 会变，而流水必须是当时的事实。 |
| `was_unlimited` | INTEGER NOT NULL DEFAULT 0 | 见下节，单独说明。 |
| `operator_id` | TEXT NOT NULL | 余额是钱，「谁给的」必须可追溯到**具体账号**。没有操作者的改动等于无法追责，不该允许落库。 |
| `operator_username` | TEXT（可空） | 冗余固化一份：账号可能被改名或删除，流水不该因主体消失而失去署名（与 `usage_records` 冗余固化 `user_id` 同一理由）。可空是因为引导态合成管理员没有真实用户名。 |
| `ts` | INTEGER NOT NULL | 毫秒时间戳，与全仓其它时间字段同口径。 |
| `remark` | TEXT（可空） | 充值原因 / 工单号。不参与任何计算，纯留痕。 |

**金额一律整数分**（与 `users.balance_cents` 同口径）。对账要求「流水求和 == 余额变化」
严格成立，掺浮点就永远不成立（`0.1+0.2 != 0.3`）。

**索引 `(user_id, ts)`**：钱包页永远是「某一个人的、按时间倒序」这一种查法，
这个复合索引同时服务过滤与排序。

### `was_unlimited` 为什么必须有

`AdjustBalance` 对 NULL 余额用 `COALESCE(balance_cents, 0) + ?` 起算，于是
「给不限额用户充值 100 元」会**把「无限」变成「100 元」** —— 这是**语义突变**
（对用户是实打实的限制收紧），不是普通的加钱。

不记这一列，事后对账只看到「+100.00 元」，无法区分：

- 给一个本来就有额度的人充值；
- 把一个无限额度的人**降级**成了有限额度。

后者性质完全不同，且往往正是用户投诉「我原来无限额度怎么变成 100 元了」时
管理员需要回答的问题。

### 迁移策略：刻意不写数据迁移脚本

存量充值已丢失。补记等于**凭空造账** —— 那比「查不到历史充值」糟得多：
查不到是已知缺口，造账是假数据。本表从启用之日起才可信。

---

## 二、事务边界取舍（最重要的一处）

### 结论：**不**把「改余额」与「记流水」放进同一事务

一眼看去，包进 `BeginTx` 显然更整齐：要么都成、要么都不成，不会出现
「钱加了但没流水」。**但本实现刻意不做**，三条理由按重要性排：

**1. 不能改变 `AdjustBalance` 的并发语义。**

`AdjustBalance` 用 `balance_cents + ?` 做原子自增，天然免疫丢失更新，
`balance_dao_test.go` 的 `TestAdjustBalance_ConcurrentTopUpsDoNotLoseUpdates`
钉住了这一点。包进事务本身**不改变这条语句的原子性**（仍是单条 UPDATE），
但会让整个调用持有写事务直到 Commit —— 而写池是单连接
（`SetMaxOpenConns(1)`），持锁期间所有用量落库、扣费、管理写全部排队。
充值虽是低频操作，但把它做成一个可能长时间持锁的事务没有收益。

**2. 流水写失败不该让充值看起来失败。**

余额已经改了 —— 那是既成事实。管理员看到「充值失败」却发现余额其实变了，
比「充值成功但流水缺一笔」**更糟**：前者会诱导他**再充一次**，
那才是真的多充了钱。

所以 `AdjustBalanceWithLedger` 在流水写失败时返回 `(Topup, nil)` —— 调用方
看到成功。错误以 **ERROR** 记进日志（含 `user_id` / `delta_cents` /
`balance_after` / `operator_id`），运维能发现并手工补记。

**3. 流水是事后账，不是准入闸门。**

它不参与任何判定（余额预检读 `users.balance_cents`，不看本表），因此它的
短暂缺失不会造成错误放行或错误拒绝 —— 与 quota/balance 这类**判定依据**的
原子性要求不在一个量级。

### 代价（明确接受）

**余额改了而流水没写是一个可达状态。** 这是本方案的真实成本，报告它而不是
藏起来。若将来要求「绝不允许缺流水」，正确做法是加一个对账任务
（找出 `status='ok'` 却没有对应 `balance_charges` 行的 usage），而不是把
低频管理写绑进长事务。

### `balance_after` 为什么用 `UPDATE ... RETURNING`

「先 SELECT 旧值 → UPDATE → SELECT 新值」在并发下会读到他人的中间态：
另一个管理员的充值可能正好插在两次读之间，于是回读到的新值不是本次的结果。
`RETURNING` 把「改」与「读回改后值」压进**同一条语句**，由 SQLite 写锁保证
两者之间没有别的写入 —— 与 `balance_dao` 里 `balance_remainder` 的累加读回
是同一个理由（该驱动已验证支持 `RETURNING`）。

### `was_unlimited` 为什么用前置读

`RETURNING` 只能给出**新值**，而这一列判定的是**旧值**为 NULL。事后反推
（「新值 == delta 就是突变」）是**错的** —— 一个本来 0 分的用户被充值到恰好
等于 delta 的值时同样满足该等式，却并不是语义突变。

`TestTopup_WasUnlimitedNotInferredFromEqualValues` 专门把两种情形放在**同一个
数值**上，确保判据是「旧值是不是 NULL」。

这条前置读与 UPDATE 之间可能有并发，那种情况下 `was_unlimited` 可能记错方向。
但它记错的**后果**被严格限制：这一列只用于事后审计的分类说明，
**不参与任何判定、不影响余额、不影响预检**。宁可分类标签偶尔不准，
也不为此把低频管理写绑进长事务。

---

## 三、接口设计

### 写入：`PUT /admin/api/users/{id}/balance`（既有端点，行为增强）

请求体新增可选字段 `remark`：

```json
{ "delta_cents": 5000, "remark": "工单 42" }
```

内部从 `AdjustBalance` 换成 `AdjustBalanceWithLedger`，把 `self.ID` /
`self.Username` 作为操作者传下去。**响应体完全不变**（仍是 `userResponse`），
前端无需改动。

保留了另一条任务新加的「管理员不能给自己充值」防护，未做改动。

### 查询：`GET /admin/api/topups`（**唯一一个**查询端点）

```
GET /admin/api/topups?limit=50&offset=0
```

**为什么是「一个」而不是「两个」端点：**

任务描述里让我在 `GET /admin/api/me/topups`（普通用户查自己）与
`GET /admin/api/users/{id}/topups`（管理员查指定用户）之间选择或两者都要。
我先按「两者都要」实现，随后发现前端**已经写好了 Wallet.vue**，且
`web/src/api.ts` 调的是 `get('/topups')` —— 即 `GET /admin/api/topups`，
注释里明确写着「接口形状尚未最终确认，联调时以服务端实际返回为准」。

既然我是被对齐的一方，就把接口收敛成**前端已经在调的那一个**，而不是要求
前端改代码。代价是失去了「管理员按 id 查指定用户」的能力 —— 但钱包页是
**个人账本页**，本来就没有「看全站充值」的用例；真要查某个人，用用户详情页
（另一个任务的范围）。

**作用域完全由会话身份决定，不接受任何请求参数指定查谁：**

- 普通用户 → 只看到自己的；
- 管理员 → 也只看自己的那一份。

管理员也只看自己是有意的：一旦 admin 默认看到全部，任何一次误操作
（比如把 `limit` 调大后忘了改回来）都会把全站用户的余额变动摊到一个人眼前。

`TestTopupHandler_UserIDQueryParamIsIgnored` 把这条钉成可执行断言 ——
若将来有人图省事改成读 `?user_id=`，这个用例立刻红。
`TestTopupHandler_AdminAlsoSeesOnlyOwn` 钉住「不因为是 admin 就放宽作用域」。

### 返回 JSON 形状（前端要用）

与 `web/src/types.ts` 的 `TopupPage` 对齐：

```json
{
  "records": [
    {
      "id": "9f2c1a8b0e7d4c3b5a6f8e1d2c3b4a59",
      "user_id": "u-alice",
      "username": "alice",
      "delta_cents": 5000,
      "balance_after": 6000,
      "was_unlimited": false,
      "operator_id": "admin-bob",
      "operator_username": "bob",
      "ts": 1791419309010,
      "remark": "工单 42"
    }
  ],
  "total": 1
}
```

| 字段 | 类型 | 说明 |
|---|---|---|
| `id` | string | 32 位 hex，用作列表 `:key`。 |
| `user_id` | string | 被充值的人。 |
| `username` | string | **服务端额外补的**：被充值者的用户名。管理员问的是「我给谁充的」，让他拿 id 去用户表比对是这一页独有的额外一步。查询失败时退化为显示 `user_id`，不阻断列表。 |
| `delta_cents` | number | 分，**带符号**。正=充值，负=扣减。 |
| `balance_after` | number | 分，本次调整**之后**的余额。 |
| `was_unlimited` | boolean | 本次是否把「不限额」切成了有限额。**前端必须对它做特殊提示** —— 它意味着用户的使用权限被收紧了。 |
| `operator_id` | string | 操作者 id。 |
| `operator_username` | string | 操作者用户名（可能为空串）。 |
| `ts` | number | 毫秒时间戳。 |
| `remark` | string | 备注，空时为 `""`。 |
| `total` | number | **不受 limit 影响的总条数**，供「共 N 条」与翻页。 |

排序：**新→旧**（`ts DESC, rowid DESC`）。钱包页展示的是「最近发生过什么」。

> 次序键用 `rowid` 而不是 `id`：`ts` 是毫秒精度，同一毫秒内的多笔充值完全可能
> 发生（批量充值、或测试里连续调用）。用随机十六进制 `id` 做次序键，
> 同一毫秒内顺序就是随机的 —— 翻页时同一条记录可能在第 1 页和第 2 页各出现一次，
> 还可能漏掉某条。`rowid` 随插入单调递增，正是分页需要的稳定次序键。
> 这条是被测试逼出来的：初版用 `id DESC`，`TestTopup_ListIsNewestFirstAndPaginates`
> 直接失败。

分页钳制：`limit` 默认 50、上限 200；`offset` 非正数归零。
沿用 `clampLimit`（SQLite 的 `LIMIT -1` 是「不限制」，漏钳就是一次全表物化）。

无流水时 `records` 是 `[]` 而**不是 `null`** —— 前端 `records.map(...)`
在 null 上会直接抛 TypeError 白屏（`TestTopupHandler_EmptyListIsArrayNotNull`）。

---

## 四、权限模型

| 端点 | 普通用户 | 管理员 |
|---|---|---|
| `GET /admin/api/topups` | ✅ 只看自己 | ✅ 只看自己 |
| `PUT /admin/api/users/{id}/balance` | ❌ 403 | ✅ |

`/admin/api/topups` 加进了 `server.userAccessiblePrefixes` 白名单
（`internal/server/user_auth.go`）—— 否则 `AdminGateGuard` 会要求管理员，
用户打开钱包页看到的是 403 而不是自己的记录。

职责分配与 `key_handler` / `usage_handler` 一致：**白名单只管「谁能进这个端点」，
进来之后看谁由 handler 判。**

新增 `internal/server/admin_gate_topup_test.go` 钉住白名单的实际可达性 ——
此前该白名单**零测试覆盖**，而它的失效是静默的（加端点忘加白名单 = 用户看到
403；误把 admin 端点加进去 = 越权）。用例同时覆盖正反两面：

- 普通用户可达：`/keys`、`/usage`、`/stats`、`/me`、`/topups`、`/logout`、`/model-names`；
- 普通用户**必须**被 403 挡下：`/users`、`/users/{id}/balance`、`/users/{id}/password`、
  `/groups`、`/routes`、`/providers`、`/settings`、`/audit`；
- 无身份 → 403；引导态合成 admin → 放行；管理员 → 全部放行。

「拿不到身份」返回 **401 而不是空数组**：返回「空列表」会被前端渲染成
「你没有充值记录」，把一次鉴权故障伪装成正常状态。

---

## 五、测试

新增 **29** 个用例，全绿。

`internal/store/topup_dao_test.go`（16 个）
: 字段正确性 · `was_unlimited` 三种情形（真突变 / 碰巧相等非突变 / 只在转换那一次为真）·
负 delta 留痕 · 扣减夹到 0 时 delta 与 balance_after 的差异 · 用户不存在不留痕 ·
delta=0 被拒 · **并发**（条数、delta 求和、id 唯一、balance_after 最大值）·
作用域收窄 · 空 userID 返回空 · 倒序与分页 · limit 钳制 · 可空列读回 ·
老 `AdjustBalance` 语义不变 · 读池在写事务占用期间仍可读。

`internal/admin/topup_handler_test.go`（11 个）
: 充值写流水且**操作者是会话身份** · 扣减留痕 · 用户不存在 404 且无流水 ·
普通用户只见自己 · **`?user_id=` 参数被忽略** · 管理员也只见自己 ·
未登录 401 · 空身份 401 · 记录带 username · 空列表是 `[]` · limit 钳制 ·
分页不重不漏。

`internal/server/admin_gate_topup_test.go`（4 组）· 见上节。

### 并发用例（重点要求）

`TestTopup_ConcurrentTopUpsLedgerCountAndSumAreExact`：10 个 goroutine 并发充值，
断言 **条数 == 10**、**delta 求和 == 总充值额**、**id 互不相同**、
**balance_after 最大值 == 真实余额**。这条专门盯住「余额自增是原子的、
流水写入是另一次语句」这个结构 —— 每个 worker 做「预读 → UPDATE…RETURNING →
INSERT 流水」，三者之间没有任何互斥，漏写或重写必然在此暴露。

`-count=40` 压测稳定。

> 压测过程中发现并修掉了一个**我自己写错的断言**：初版断言「`list[0]` 就是最新那条」。
> 并发下「执行最后一次 UPDATE 的 worker」与「rowid 最大的那条流水」未必是同一个
> （余额自增与流水 INSERT 是两条独立语句，中间夹得进别的 worker 的 UPDATE），
> 该断言 flaky。改为按值取最大值 —— 这也正是 DAO 注释里写明
> 「并发下 balance_after 只保证本次自己的结果是权威值，不保证与相邻那条首尾相接，
> 逐笔核对应以 delta_cents 求和」的原因。

---

## 六、验证结果

```
gofmt -l cmd internal   → 仅 internal/admin/user_handler.go（非本次改动，见下）
go build ./...          → 通过
go vet ./...            → 通过
go test ./...           → 全绿
```

- `TestAdjustBalance_*` 共 10 个用例（store 2 个 + admin 8 个）**全部仍通过**，
  包括任务点名不许红的 `TestAdjustBalance_ConcurrentTopUpsDoNotLoneUpdates`。
- `go test -race` **未能执行**：本机无 C 编译器（`gcc not found`，cgo 被禁用）。
  作为替代，对并发路径做了 `-count=40` 的重复压测。
- `gofmt -l` 剩余的 `internal/admin/user_handler.go` **不是本次改动**：
  经 `git stash` 验证，该文件在 HEAD 上格式合规，是**另一条任务的未提交改动**
  导致的格式问题。按任务要求「不要回退任何东西」，未触碰该文件。

## 七、改动文件清单

| 文件 | 改动 |
|---|---|
| `internal/store/topup_dao.go` | **新增** — `Topup` 结构、`AdjustBalanceWithLedger`、`ListTopupsByUser`、`CountTopupsByUser` |
| `internal/store/store.go` | migrations 列表追加 `balance_topups` DDL + 索引（无数据迁移） |
| `internal/admin/user_admin_handler.go` | `AdjustBalance` 改调 `AdjustBalanceWithLedger` 并传操作者；请求体加 `remark`；新增 `ListTopups` handler |
| `cmd/gateway/main.go` | 注册 `GET /admin/api/topups` |
| `internal/server/user_auth.go` | `userAccessiblePrefixes` 追加 `/admin/api/topups` |
| `internal/store/topup_dao_test.go` | **新增** 16 个用例 |
| `internal/admin/topup_handler_test.go` | **新增** 11 个用例 |
| `internal/server/admin_gate_topup_test.go` | **新增** 4 组用例 |

未改动：`web/`、`internal/store/balance_dao.go`、`internal/webui/dist`，
以及 `internal/admin/` 下的其它 handler。未 `git commit`。

## 八、给前端的三点提醒

1. **`records` 不是 `topups`**，且 `total` 是**不受 limit 影响**的分页总数。
   （前端 `api.ts`/`types.ts` 已按 `{records, total}` 写，与本实现一致。）
2. **`was_unlimited === true` 必须给用户显著提示。** 它意味着「你原来无限额度，
   这次被设成了具体额度」—— 是权限收紧，不是普通充值。普通充值提示会误导用户。
3. **`delta_cents` 带符号**，列表里充值与扣减混排。展示金额时不要取绝对值，
   扣减应当用另一种视觉标记（颜色/前缀），否则用户会以为被扣了钱。