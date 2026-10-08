# 任务 11：消费汇总改成「一行一个用户」（wallet-by-user）

日期：2026-10-11

## 需求原文

> 钱包与充值页面，管理员余额不适用「管理员不参与余额计费：不建密钥、不调用接口，
> 也没有充值入口」这部分去掉，消费汇总列表一行一个用户，写一个用户的总消费。

第 1 条（去掉「管理员余额：不适用」那一格）由 Lead 自己完成，我未触碰。
本报告只覆盖第 2 条：**消费汇总从「一行全局合计」改成「一行一个用户」**。

## 结论速览

| 项 | 结论 |
| --- | --- |
| 新端点 | `GET /admin/api/usage/by-user`（**admin-only**，handler 内 `requireAdmin`） |
| 权限 | 403 已验证；且**用变异测试证明**该用例不是假通过（见第 5 节） |
| 归档合并 | 走 `store.UsageSource`（明细 ∪ 日归档）—— 剪枝后合计不缩水，有剪枝用例钉住 |
| 管理员历史用量 | **照实显示 + 标注「管理员·历史」**，不静默过滤（理由见第 3 节） |
| 分页 | 后端 `limit` 默认 100 / 上限 1000；前端取 100，**截断时显式提示** |
| 文件 | 新增 4 个文件，改动 4 个；`usage_handler.go` 我**没有**改动 |

---

## 1. 接口形状

```
GET /admin/api/usage/by-user?from=<ms>&to=<ms>&limit=<n>
```

响应（对象，**不是裸数组**）：

```json
{
  "records": [
    { "key": "u_abc", "user_name": "alice", "role": "user",
      "count": 12, "tokens": 34567, "cost": 0.1234 },
    { "key": "u_del", "user_name": "",      "role": "",
      "count": 3,  "tokens": 8000,  "cost": 0.05 },
    { "key": "",      "user_name": "",      "role": "",
      "count": 1,  "tokens": 500,   "cost": 0.001 }
  ],
  "total": 3
}
```

| 字段 | 说明 |
| --- | --- |
| `key` | `usage_records.user_id`（冗余固化的归属）。空串 = **无归属** |
| `user_name` | JOIN `users` 得到；空串 = **用户已删除**（靠 `key` 是否为空区分于无归属） |
| `role` | `admin` / `user`；已删除时为空。用于标注管理员的历史用量 |
| `count` | `SUM(u.n)` —— 归档行的 `request_count` 已聚合，**不能用 `COUNT(*)`** |
| `tokens` | `SUM(total_tokens)` |
| `cost` | `SUM(cost_total)`（元）—— **这张表的主角** |
| `total` | 有消费的用户总数（去重），**不受 `limit` 影响** |

### 为什么这个端点返回对象，而三个兄弟返回裸数组

`by-key` / `by-model` / `by-provider` 由总览页调用、固定 `limit=10` 画「Top 10」
条形图 —— 截断是它们的**既定语义**（标题就写着 Top 10），所以不需要总数。

而这张表是**按金额读账**的：管理员会把它当成「所有人各花了多少」。用户数一旦超过
`limit`，一张被截断的表与一张完整的表在界面上**完全同形** —— 把「前 100 名」读成
「全部用户」会直接得出错误结论（典型症状：「合计怎么对不上上面那张汇总卡」）。
所以必须下发 `total`，让前端能说出「显示前 N / 共 M 位用户」。
与 CSV 导出的 `X-Export-Truncated` 是同一个原则：**截断必须可见**。

## 2. 归档合并（`UsageSource`）

`store.UsageByUser` 的数据源是 `UsageSource(UsageFilter{From, To})`，
与 `GetUsageStats` / `SumCostBuckets` 同构 —— UNION 明细分支与 `usage_daily_rollups`
归档支：

```sql
SELECT ... FROM (  <明细> UNION ALL <归档> ) u
LEFT JOIN users usr ON usr.id = u.user_id
GROUP BY u.user_id
ORDER BY cost DESC, u.user_id ASC
```

**为什么必须合并**：只查明细的话，30 天剪枝一跑，按用户的汇总就会**逐日缩水** ——
管理员会看到「他上个月没花钱」。而漏掉归档支在**还没剪过枝的库上完全测不出来**
（那一支恒为空），所以 `TestUsageByUser_SurvivesPrune` 与
`TestUsageByUser_SumEqualsGlobalStats/mixed-detail-and-archive` 都是**先真剪枝再断言**。

两个细节：

- **`SUM(u.n)` 而不是 `COUNT(*)`**：归档一行代表「一天 × 一维度下的一组请求」，
  `COUNT(*)` 会把它当成 1 次请求，直接少算。这是 `UsageSource` 的 `n` 列存在的理由。
- **JOIN `users` 放在归一化来源之外**：用户名是展示属性、不参与聚合；塞进 UNION 支里
  会随「一个用户对应多条来源行」把计数复制多份。`users.id` 是主键（至多匹配一行），
  放外层是 1:1 的。（与 `usage_handler.groupBy` JOIN `access_keys` 同一手法。）

**LEFT JOIN 而不是 INNER**：见第 3 节。

### 一个容易踩的坑（我踩了）

`store.UsageSumExpr(col, alias)` 返回的是 `COALESCE(SUM(alias.col), 0)`，
**不带 `AS 别名`** —— 别名的拼接是调用方的责任。我第一版漏了 `AS cost`，
`ORDER BY cost` 直接报 `no such column: cost`。
这个错误**编译期看不出来**，只有真跑一次查询才暴露（好在冒烟测试当场抓到了）。
已在代码里加注释钉住。

## 3. 管理员的历史用量：照实显示 + 标注

**决定：不过滤，照实显示，并在界面上标注「管理员·历史」。**

理由：

1. 管理员现在**确实不能**调用模型（`internal/auth.ErrAdminCannotCallModel` 拦在
   `/v1` 之前），所以理论上他不该出现在这张表里。但**升级前**可能有他的历史用量。
2. 那是**真实发生过的消费**。静默过滤会让这张表的合计**小于**总览的全局合计 ——
   而「两张表对不上账」是最难查的一类问题（两个数字各看都合理，只有放一起才知道错）。
3. 不标注的话，那一行看起来像一条脏数据，管理员会去找一个「怎么管理员也在花钱」的
   原因。标注成「管理员·历史」就直接回答了这个问题。

`role` 字段就是为此下发的。前端 `rowTag()` 的三档：

| 情况 | 判定 | 显示 |
| --- | --- | --- |
| 无归属 | `key === ''` | 「无归属」（灰） |
| 用户已删除 | `key` 非空而 `user_name` 为空 | **回退显示 `key`** + 「已删除」（灰） |
| 管理员的历史用量 | `role === 'admin'` | 用户名 + 「管理员·历史」（accent） |

**「已删除」与「无归属」必须分开**：两者在数据形状上都是「`user_name` 为空」，
但一个是真实存在过、现在被删掉的账号（消费是历史事实，要能对账），另一个是从来没有
归属的迁移遗留。留白会让管理员把两者混为一谈，而它们的处置方式不一样。
所以前端**绝不**渲染成空白。

## 4. 分页与「截断必须可见」

- 后端：`clampLimit(q.Get("limit"), defaultUsageLimit /*100*/, maxUsageLimit /*1000*/)` ——
  与其它 by-* 同一个钳制函数。它同时堵死了 `limit=-1`（SQLite 语义是「不限制」，
  会静默拉全表）。有用例 `TestUsageByUser_LimitClamped` 钉住。
- 前端：`BY_USER_LIMIT = 100`。**不取 1000**：这张表是给人读的，一屏能读完的账号数
  远小于 100，而 1000 行既没人翻完，又让「截断」在实际使用中几乎不出现。
- 截断时表尾显示常驻告警（不是 toast —— toast 5 秒就没了，而管理员是拿着这张表
  在做金额判断的）：
  > 共 N 位用户有消费，这里只显示消费最高的 100 位。要看某位用户的逐条明细，
  > 请到「调用历史」页按密钥或模型过滤。

### 顺带加了一道对账

前端在口径说明里同时给出**本表合计**，并写明「未截断时与上方『用户消费金额』应当
一致；不一致说明有一侧读取失败」。

这句话只有在两者**恒等**时才敢写，所以我把它变成了测试
（`TestUsageByUser_SumEqualsGlobalStats`）：同一 `from/to` 下，
`Σ(per-user cost)` 必须等于 `stats.cost`，三处口径（请求数 / token / 金额）全比。
明细-only 与 剪枝后混合 两种场景各跑一遍。

**并且验证了这条用例不是假通过**：我把 `GROUP BY` 前面插了一个
`WHERE u.user_id <> ''`（一个很现实的「顺手过滤掉空用户」bug），用例立刻失败并指出
差额 `-0.0699`、行数 3≠4。恢复后重新通过。

> 这也是我在前端把两个请求的 `Date.now()` **合并成一个共享 `now`** 的原因：
> 各取一次会让两个 `to` 差几毫秒，恰好落在这几毫秒里的用量会造出一个
> **我自己制造出来的**假不一致，而管理员会照着那条提示去排查不存在的问题。

## 5. 权限验证（本任务最关键的一条）

### 风险

`/admin/api/usage/by-user` 落在 **`/admin/api/usage`** 前缀下，而该前缀在
`internal/server/user_auth.go` 的 `userAccessiblePrefixes` 白名单里（普通用户要能读
自己的用量）。`AdminGateGuard` 是**前缀**匹配 —— 只要前缀在名单里就**整体放行**。

也就是说：**网关层不会保护这个端点**，唯一防线是 handler 内的 `requireAdmin`。
漏掉它的表现是端点照常 200、没有任何报错 —— **静默越权**：任何普通用户打一次就能拿到
「每个同事的用户名 + 消费金额」，即全公司的账单。

### 两条措施

1. **handler 内显式 `requireAdmin`**（照 `usageHandler.Prune` 的同款做法 ——
   它就是同一个坑）。同时在路由注册处写了注释说明为什么不能只靠白名单。
2. **测试 `TestUsageByUser_RequiresAdmin`**：普通用户必须 403，且**额外断言 403 的
   响应体里不含 `records`**（防「先写响应再判权限」那类实现）。然后再断言管理员能拿到
   数据 —— 否则这条用例可能因为端点整个坏掉而通过。

### 变异测试：证明 403 用例不是假通过

我把 `requireAdmin` 那 3 行临时删掉重跑：

```
--- FAIL: TestUsageByUser_RequiresAdmin (0.07s)
    status = 200, want 403 — /admin/api/usage is on the user-accessible
    prefix whitelist, so requireAdmin inside the handler is the ONLY defence
```

**普通用户拿到了 200**。恢复后重新通过。这条用例确实钉住了那道防线。

另加 `TestUsageByUser_PrefixWhitelistDoesNotProtectIt`：断言该路径确实以
`/admin/api/usage` 开头，把「网关层挡不住」这个**前提**固定下来 ——
将来若有人把该前缀移出白名单、并据此认为 handler 里的 `requireAdmin` 多余，
这条会失败提醒他重新评估。两条测试合起来才是完整论证。

## 6. 实现与改动清单

**新增**

| 文件 | 内容 |
| --- | --- |
| `internal/store/usage_by_user_dao.go` | `Store.UsageByUser` + `UserUsageStat` |
| `internal/admin/usage_by_user_handler.go` | `GroupByUser` + `usageUserEntry` / `usageByUserResponse` |
| `internal/store/usage_by_user_dao_test.go` | 7 个用例 |
| `internal/admin/usage_by_user_test.go` | 8 个用例 |

**改动（均为追加式）**

| 文件 | 改动 |
| --- | --- |
| `cmd/gateway/main.go` | 仅新增 1 行路由注册 + 注释（改前已重读） |
| `web/src/types.ts` | 追加 `UsageByUserEntry` / `UsageByUserPage` |
| `web/src/api.ts` | 追加 `api.usageByUser`（含 import） |
| `web/src/views/Wallet.vue` | 管理员「消费汇总」格改为按用户表 |

**未改动**：`internal/admin/usage_handler.go`。
该文件当时正被其他任务编辑（我两次编辑都撞上 `FS_STALE_VERSION`），
所以我把 handler 放进**同包的新文件** —— 效果相同（同 package、同 `UsageHandler`
类型），但零冲突。这比反复重试抢占共享文件更稳。

## 7. 前端行为

- 「消费汇总」页签（管理员）现在渲染：**用户 / 总消费 / 调用次数 / Token**。
  金额紧跟用户名放第二列 —— 用户要的就是「谁花了多少钱」，金额是主角，
  推到最右边会让人先读一堆次数/token 才能找到要看的数。
- **顶部汇总卡保留**（Lead 已去掉「管理员余额：不适用」那一格）：它是「全站总共花了
  多少」，与下面「每人各花了多少」是两个层次的问题。
- 「查看逐条调用明细 →」入口保留（明细的唯一入口）。
- 三态分开，且**这张表失败不替换整块**：顶部汇总卡可能已经渲染出来了，
  整块替换会把那个数字一起收走，而失败只影响这一张表。空态里额外说明
  「若总览有流量，说明是加载问题而不是真的没数据」—— 避免空表被读成
  「没有人消费过」。
- 普通用户**不请求**这个端点（必然 403），否则会给每个普通用户制造一条假错误态。

`vue-tsc --noEmit` 与 `vite build` 均 **exit 0**。

## 8. 验证

```
go build ./...            → exit 0
go vet ./...              → exit 0
gofmt -l cmd internal     → 无输出
go test -count=1 ./...    → 全部 ok（14 个包）
vue-tsc --noEmit          → exit 0
vite build                → exit 0
```

`-count=1` 是刻意的：共享文件期间被改过，缓存结果不能作为证据。

**测试清单（15 条）**

store（7）：金额与降序、同额稳定兜底、已删除用户退化、无归属成行、
剪枝不缩水、limit 与 total、时间窗口、**Σper-user == stats 全局**（2 子场景）。

admin（8）：**普通用户 403（含 body 不泄露）**、白名单前提、
金额与用户名、同额排序、已删除退化、空数组非 null、非法时间参数 400、limit 钳制。

未跑 `sync-embed.mjs`（Lead 统一做），未 `git commit`。

## 9. 遗留与建议

1. **管理员历史用量的处置由我判断为「照实显示 + 标注」**（第 3 节）。若 Lead 更希望
   收敛成「只在有历史用量时才多一个折叠区」，那是一个纯前端改动，
   数据已经够了（`role` 字段）。
2. `by-user` 的默认窗口是近 7 天（与三个兄弟端点一致）。钱包页显式发 `from=0` 取全部。
   我没有给这个端点单独设一个更宽的默认值 —— 那会让「同一个 `from` 参数在四个
   `by-*` 端点上含义不同」，正是上一轮刚修掉的 `queryRange`/`queryRangeExplicit` 分叉。
3. 「消耗记录」那一格（普通用户）仍是最近 30 天 15 条。**这不受本次影响**，
   但我顺手更新了它上面那段已经过期的注释：它此前写着「`/usage/history` 把 `from=0`
   当成没传」—— 那个 `queryRange` 已被 Lead 删除，现在 `days=0` 确实等于全部历史。
   保留 30 天现在是**产品选择**（速览），不是兼容性妥协，注释已改成这个说法。
