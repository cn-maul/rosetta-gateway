# A6 存储层审计：schema 与 DAO 正确性

审计对象：`rosetta-gateway`，`main` = `ea19ce1`
范围：`internal/store/`（schema / migration / DAO / SQL）、`cmd/migrate-legacy/`
只读审计。未修改任何 git 跟踪文件。

结论统计：**P0 = 2，P1 = 5，P2 = 6，P3 = 3**

---

## P0

### P0-1 归档（剪枝）会永久丢失「窗口边缘那一天」的全部用量

`internal/store/usage_source.go:117-120` 把归档支的边界规则定成「**整日包含**」：
归档行只在 `dayStartMs >= From` 且 `dayEndMs <= To` 时才被计入。

而 `internal/store/usage_dao.go:883-884`（`SumTokensByDayForKey`）用的是同一套规则。

问题在于 `To` 通常**不是本地午夜**。`cmd/gateway/billing.go:129` 的
`billingRangeMs` 把 `end_date`（含端日）换算成 `end_date + 1 天` 的 UTC 零点，
而 `internal/admin/usage_handler.go:100-107`（`queryRangeExplicit`）用
「现在」作上界 —— 两者都不是本地午夜。于是窗口上界总是切在某天的正中间，
而那一天的数据在剪枝后已进归档表，**却不满足整日包含**，被整体排除。

实测（真实 `bin/data/gateway.db`，8333 行明细，TZ=Asia/Shanghai）：
窗口 `[2026-10-07 12:00, 2026-10-09 12:00]`：

- 剪枝前（全部在明细）：`detail=215338903 rollup=0 total=215338903`
- 剪枝后（明细已删、归档已写）：`detail=0 rollup=27882697 total=27882697`
- **丢失 187456206 token（87%）**

这与「剪枝前后同窗口数字相等」这条验收（`internal/store/usage_archive_test.go:137`
`TestPrune_StatsUnchangedAcrossPrune`）直接冲突。该测试之所以绿，是因为它
**刻意把窗口对齐到本地午夜**（`usage_archive_test.go:157-158` 的注释
「对齐午夜才是真实查询的形状」）—— 而真实查询（`billing.go` / `queryRangeExplicit`）
并不对齐。测试的假设与生产调用点不一致，掩盖了这条缺陷。

影响面：`/v1/organization/*` 的 `SumTokensForKey`（`usage_dao.go:834`）、
`SumCostBucketsScoped`（`usage_dao.go:798`）、`SumUsageBucketsScoped`
（`usage_dao.go:753`）、`GetUsageStats`（`usage_dao.go:567`）全部走
`UsageSource`，因此**所有**以「非午夜对齐」为上界的区间聚合在剪枝后都会少算
窗口边缘那一天。

**修法方向**（不在本次范围）：归档支的边界规则不能是「整日包含」，必须与明细支
逐字对齐 —— 或者反过来，把 `To` 统一上取整到本地午夜（并明确记录这是
「多算一天」的取舍）。当前两套口径不可能同时成立。

### P0-2 `RecomputeCost` 改写 `cost_total` 时不更新任何累计表 → 费用被永久吞掉

`internal/store/usage_dao.go:489-517`（`RecomputeCost`）在重算一条
`usage_state='cost_pending'` 的记录时执行：

```sql
UPDATE usage_records SET cost_total = ?, usage_state = ? WHERE id = ?
```

但 `usage_totals` 的累加只由 **AFTER INSERT** 触发器
（`internal/store/usage_archive.go:227-249`）负责，而
`usage_daily_rollups` 的归档只发生在剪枝时（`usage_archive.go:640-700`）。
**没有任何 AFTER UPDATE 触发器**，也没有把 UPDATE 后的差额补记进表 B 的逻辑。

实测（最小复现，`:memory:`）：

```
after insert (cost_pending, cost=0): (request_count=1, cost_total=0.0)
after RecomputeCost UPDATE:          (request_count=1, cost_total=0.0)
detail row: (1.5, 'reported')
```

即：明细行说「这笔花了 1.5 元」，终身累计表说「0 元」。

推论（比这更糟）：

1. 剪枝发生时，这笔被补正过的 `cost_total` 会被写进 `usage_daily_rollups`，
   于是**归档表有 1.5、终身累计表（表 B）有 0** —— 两个归档层互相矛盾，
   且表 B 是「剪枝后累计不变」的保证（`usage_archive.go:20-25`）。
2. `internal/store/balance_dao.go` 的余额扣费走的是
   `CreateUsageRecordWithCost` 返回的 `cost`（`cmd/gateway/main.go:3408`），
   落库时是 0，所以**余额那一侧没漏**；漏的是**报表/对账侧**。
3. `RecomputePendingCosts`（`usage_dao.go:428`）每 5 分钟跑一次
   （`cmd/gateway/main.go:754-780`），所以这不是罕见路径。

**影响**：`usage_totals`（总览「全部」档的大数字，`usage_archive.go:769`）
的 `cost_total` 会**永久低于**真实应收，任何一次读池抖动导致的查价失败
（`usage_dao.go:403-406`）都会造成一笔不可见的费用漏记，且没有任何对账入口
能发现它 —— 表 B 没有重算入口（`reconcileUsageTotals` 是一次性的、
被 marker 挡住，`usage_archive.go:402-460`）。

**修法方向**：要么给 `usage_records` 加 AFTER UPDATE 触发器（把
`cost_total` 差额补进表 B），要么在 `RecomputeCost` 里显式补记差额；
要么让 `reconcileUsageTotals` 变成可重入的对账任务。

---

## P1

### P1-1 `backfillProxyNetworks` 用未 trim 的原文回填 → 带首尾空白的代理配置静默丢失

`internal/store/network_dao.go:185-187` 取 URL 时做了 `TRIM`：

```sql
SELECT DISTINCT TRIM(proxy_url) FROM providers
 WHERE proxy_url IS NOT NULL AND TRIM(proxy_url) <> ''
```

但回填那一步（`network_dao.go:232`）用的是**未 trim 的原文**：

```sql
UPDATE providers SET network_id = ? WHERE proxy_url = ?
```

`?` 绑的是 trim 后的 URL。于是 `proxy_url = '  http://127.0.0.1:7897  '`
这一行**匹配不上**，network_id 保持 NULL。紧接着 `dropDeadColumns`
（`store.go:717`）把 `proxy_url` 列摘掉 —— **代理配置就此永久丢失**，
该上游静默回落成直连（表现为 504 / 连不上，没有任何报错）。

实测（最小复现）：

```
urls: ['http://127.0.0.1:7897']
providers: [('p1', '  http://127.0.0.1:7897 ', None)]   ← network_id 是 None
```

`network_migration_smoke_test.go` 的四个 fixture 用例（`'http://127.0.0.1:7897'`、
NULL、`''`）**没有一个带首尾空白**，所以这条路径没有被测到。
管理面写入路径（`internal/admin/network_handler.go:139-156`）会校验 URL
合法性但**不 trim**（`network_dao.go:81` 的 `CreateNetwork` 才 trim），
而 `providers.proxy_url` 是历史列、由更早的版本直接写入，空白是完全可能的。

### P1-2 `balance_pending` 的「结转」把欠款余额抹成 0，且丢弃未收足的零头

`internal/store/balance_dao.go:564-590`（`settlePendingForUser`）在充值时：

```go
cents := totalMicros.Int64 / MicrosPerCent
... UPDATE users SET balance_cents = MAX(0, COALESCE(balance_cents,0) - ?)
... DELETE FROM balance_pending WHERE user_id = ?
```

两个缺陷：

1. **`MAX(0, …)` 吞掉超出余额的欠款**。用户欠 100 分、余额只有 50 分时，
   扣 100 分后余额被夹成 0，**50 分的欠款凭空消失**，而
   `balance_pending` 已被整表 DELETE —— 这 50 分永远收不回来，且账上
   （`balance_topups` 的 `delta_cents`）看不出它。
   实测：balance=50、pending=100 分 → 结算后 balance=0、pending 行数=0。
2. **不足一分的零头被直接核销**（`cents` 整除后余数不写回任何地方）。
   注释（`balance_dao.go:576-581`）说这是刻意的「宁可多扣一次也不漏收」，
   但零头既没有写回 `users.balance_remainder`、也没有留在 `balance_pending`，
   它是被**删除**了 —— 方向恰好是「漏收」。

这两条与 `balance_dao.go` 文件头声明的「金额是整数分」「流水求和 == 余额变化
严格成立」相矛盾：`balance_topups` 只记充值 delta，`balance_charges` 只记
实扣分数，被 `MAX(0,…)` 夹掉的那部分欠款**在两张流水里都不存在**。

### P1-3 `usage_records` 无外键，删 key / 删 user 会留下不可解释的孤儿行

`usage_records` 的 DDL（`store.go:333-358`）里
`access_key_id TEXT NOT NULL`、`user_id TEXT` **都没有 REFERENCES**。
`internal/store/key_dao.go:361`（`DeleteAccessKey`）与
`internal/store/user_dao.go:480`（`DeleteUser`）直接 `DELETE FROM`，
而 `access_keys.user_id` 是 `ON DELETE CASCADE`（`store.go:781`）。

结果：

- 删 user → 其名下 key 级联删除 → `usage_records` 里那些行的
  `access_key_id` 指向已不存在的 key，`user_id` 指向已不存在的 user。
- `internal/store/usage_by_user_dao.go:20-30` 的注释承认这一点
  （「UserID 非空而 Username 空 → 用户已删除」），并用 LEFT JOIN 兜住。
- 但 `trg_update_used_tokens`（`store.go:410-414`）是
  `UPDATE access_keys SET used_tokens = used_tokens + NEW.total_tokens
  WHERE id = NEW.access_key_id` —— key 不存在时命中 0 行，**静默丢一次累加**。
  这本身无害（行也不存在了），但它说明「孤儿用量」这条路是设计内的，
  而 schema 里没有任何东西记录「这条用量属于谁」。

这不是崩溃级缺陷，但它是**审计链断裂**：用量是历史事实，key 被删后
`usage_records` 里的 `access_key_id` 成了无主外键，
`internal/admin/usage_handler.go:588` 的调用历史页靠
`LEFT JOIN access_keys a` 取名字，key 删了名字就空了。

### P1-4 `cmd/migrate-legacy` 与运行时迁移是两套完全不同的逻辑，且都漏了 `network_id`

`cmd/migrate-legacy/main.go` 是一份**独立手写**的迁移实现，与
`internal/store/store.go` 的 `migrate()` 无任何共享代码。口径不一致的地方：

1. **不迁 `network_id`**：`main.go:195` 的 `pcols` 是
   `"id, slug, name, protocol, endpoint, enabled, timeout_ms, max_retries,
   quirks_json, created_at, updated_at"` —— 没有 `network_id`，
   也没有 `balance_spec_json`。v1.4.1 的库本来就没有这两列，所以
   这一次不算漏，但**从 v1.6.3 起**（`git tag --contains e070909` 显示
   `proxy_url` 是 v1.6.3 引入的）用这个工具搬库会**丢掉全部代理配置**。
2. **key 格式校验比运行时宽松**：`main.go:115` 只查
   `len(s) != 44`，而 `internal/crypto/crypto.go:72,101` 的
   `validKeyFormat` 还要求能 base64url 解出 32 字节。工具会把一个
   损坏的 master.key 当成合法密钥使用。
3. **目标库不建表**：`main.go:88` 只 `PRAGMA foreign_keys=ON`，
   之后直接 INSERT。目标库若不是由当前版本 `store.Open` 建出来的
   （即缺 `networks` / `model_groups` / `route_targets` 等表），
   迁移会中途失败或写出坏数据。`mustNonEmpty`（`main.go:150`）只检查
   四张表有没有行，不检查表是否存在。
4. **只读打开旧库但没有 `?mode=ro` 之外的保护**：`main.go:66` 用了
   `mode=ro`，这点是对的。

**根因**：仓库里存在**两套 schema 真相**。`cmd/migrate-legacy` 的表清单与
`store.go` 的 `migrations` 列表各自演化，后者改了前者不会跟着改，
也没有任何测试把两者钉在一起（`cmd/migrate-legacy` 下**没有 `_test.go`**）。

### P1-5 `route_targets` 的单条增删改 API 存在但无人调用，且违反自身不变量

`internal/store/route_target_dao.go:156`（`CreateRouteTarget`）、
`:179`（`UpdateRouteTarget`）、`:190`（`DeleteRouteTarget`）三个方法：

- 全仓**没有任何非测试调用点**（`rg` 结果只有定义本身）。
- `UpdateRouteTarget`（`:184-187`）**不看 RowsAffected**，删不存在的行
  也返回 nil —— 与 `tx.go:41-56` 里 `deleteByID` / `checkAffected`
  建立的「删不存在的行必须报 ErrNotFound」约定直接矛盾。
- `UpdateRouteTarget` **允许把 position 改成任意值**，而
  `internal/store/model_dao.go:365-390`（`compactTargetPositions`）
  维护的「position 连续 0..n-1」不变量靠的是「只有
  `ReplaceRouteTargets` / `DeleteUpstreamModelDetaching` 会写它」。
  任何第三方调用 `UpdateRouteTarget` 都能把链序写坏，而运行时
  （`internal/routing/routing.go`）以 position 升序遍历。

这些是**死代码**，但它们是公开 API（导出方法），一旦有人用就破坏不变量。
应删除或加上与 `ReplaceRouteTargets` 同等的约束。

---

## P2

### P2-1 「配额预占」在 key 级与 user 级是两套几乎相同的实现，只差一个列名

`internal/store/key_dao.go:446-500`（`ReserveQuota`）与
`internal/store/user_dao.go:545-609`（`ReserveUserQuota`）：

- 同样的「读 quota / used / reserved → 比较 → UPDATE 累加 reserved → Commit」五步；
- 同样的 `est <= 0 → est = 1` 兜底；
- 同样的 `quota <= 0 → 不限额` 短路；
- 同样的 `MAX(0, reserved_tokens - ?)` 释放（`key_dao.go:512` /
  `user_dao.go:622`）。

差别只在：key 级读 `used_tokens`（触发器维护的列），
user 级读 `user_usage_totals`（也是触发器维护的列，但**缺行时按 0 处理**
—— `user_dao.go:583-586`）。

**可合并点**：抽一个 `reserveQuota(ctx, table, id, usedExpr, quotaExpr, reservedCol, est)`，
两处共用。风险点是「user 级用表的累加列、key 级用本表列」这个不对称 ——
合并时必须显式参数化，不能想当然地共用一个模板。

### P2-2 「重算已用量」在 key 级与 user 级各写一份，且数据源/事务边界不同

`internal/store/key_dao.go:375-412`（`RecomputeUsedTokens`）与
`internal/store/user_dao.go:667-693`（`RecomputeUserUsage`）逐字同构：
同样走 `UsageSource`、同样 `UPDATE ... SET used_tokens = ?`、
同样 `RowsAffected()==0 → ErrNotFound`。

差别：**key 级有管理端点**（`cmd/gateway/main.go:452`
`POST /admin/api/keys/{id}/recompute-usage`），**user 级没有**。
`internal/store/user_dao.go:488` 的注释明说「而管理界面里没有任何修正入口」
—— 也就是说 `users.used_tokens` 偏高时**没有任何恢复路径**。
这个字段虽然只是展示值（`usage_archive.go:404` 说是展示字段），
但 `internal/admin/user_admin_handler.go:190-194` 在
`SumUserUsedTokens` 失败时会回退读它 —— 于是「读失败时显示一个偏高的数」
而「读成功时显示真数」，同一账号两个答案。

### P2-3 `settings_dao.go` 的设置项没有版本号 / 结构版本，改结构后旧值静默丢弃

`internal/store/settings_dao.go` 的三个设置键
（`settingModelDefaultsKey` / `settingRuntimeDefaultsKey` /
`settingBootstrapCompletedKey`）都是「JSON 塞进 `app_settings.value`」。

`GetRuntimeDefaults`（`settings_dao.go:200-215`）在 JSON 解析失败时
**静默返回全 0**：

```go
if err := json.Unmarshal([]byte(raw), &d); err != nil {
    // 存储损坏时退回「全部未配置」而不是让整个启动失败
    return RuntimeDefaults{}, nil
}
```

这与 `GetModelDefaults`（`settings_dao.go:154-172`）的容错口径**相反**：
后者在解析失败时返回 `err`（`:159-161`）。两个相邻函数对同一类故障
给出两种处置 —— 且两者都把错误**丢掉了**（`GetRuntimeDefaults` 连
err 都不返回），运维无从知道「设置读不出来」。

**根因**：`app_settings` 是一个无 schema 的 KV 表（`store.go:363-367`
只有 `key/value/updated_at`），所有结构靠约定。字段一改名，旧值
`json.Unmarshal` 失败 → 静默全 0 → 界面显示「未配置」，
而用户以为自己配过。应当加 `schema_version` 或至少把解析失败上报。

### P2-4 `credential_dao.go` 与 `internal/crypto` 的契约有一条未在 DAO 层强制的分支

`internal/store/credential_dao.go` 只负责存取 `api_key_enc BLOB`，
**完全不知道它是密文**。解密发生在数据面
（`internal/upstream/upstream.go:1496` → `crypto.DecryptWithFallback`）。

`internal/crypto/crypto.go:169-187`（`DecryptWithFallback`）有一条
**明文回落**分支：解密失败且字节「看起来像文本」时，
**把密文字节当明文直接用**（只打一条 WARN）。`looksLikeText`
（`crypto.go:190-200`）的判据是「全是可打印字符」。

后果：任何一次 master.key 更换/损坏，都会让**所有**凭据被当成明文
传给上游 → 全站 401，同时日志里只有一条 WARN。
「明文凭据」与「密文凭据」在库里**同形**（都是 BLOB），
`credential_dao.go` 层面无法区分，也没有一个「这行是不是密文」的标记列。

**建议**（不改行为）：`provider_credentials` 增加一列标记密文格式
（如 `enc_version INTEGER`），或在 DAO 层加 `IsEncrypted(blob)` 判定，
让「明文回落」成为一个可查询、可告警的状态，而不是只靠一条 WARN。

### P2-5 `usage_dao.go` 的「近 N 次」查询用 `LIMIT` + `ORDER BY ts DESC`，无 keyset

`internal/store/usage_dao.go:611-630`（`GetRecentThroughput`）、
`:632-652`（`GetRecentTtfbMs`）、`internal/store/recent_performance_dao.go:28-32`
都是 `ORDER BY ts DESC LIMIT ?`。

在同毫秒内多条记录时，相对顺序不确定 —— 与
`internal/store/topup_dao.go:277-283` 里 topup 分页刻意用
`ts DESC, rowid DESC` 的理由完全相同。`topup_dao.go` 的注释
（`:277-280`）明确说「用随机十六进制 id 做次序键，同一毫秒内的顺序
就是随机的 —— 翻页时同一条记录可能在第 1 页和第 2 页各出现一次」。

`usage_records.id` 是 `crypto/rand` 的纯 hex（`cmd/gateway/main.go`
的 `generateID()`），与 `topup_dao.go:280-282` 描述的 `id` 形态一致。
`internal/admin/usage_handler.go:676,705` 的调用历史页**已经**用了
`ORDER BY u.ts DESC, u.id DESC` 来避免这个问题 ——
说明这个模式在仓库里是已知的，但 `usage_dao.go` 的三个「近 N 次」
查询没有跟上。

影响面小（这些是「最近 N 次的平均值」而非分页列表），
但 `GetRecentThroughput` 的结果会因 tie 抖动。

### P2-6 `internal/store` 包内同一常量的多处定义

同一语义的值在多个文件里各写一份字面量：

| 语义 | 出现位置 |
|---|---|
| `status = 'ok'` | `usage_source.go:36-37`、`usage_archive.go:236-237`、`usage_archive.go:652`、`usage_dao.go:34`（`usageStateOK` 在 `cmd/gateway/main.go:2990`） |
| `'canceled'` | `usage_source.go:37`、`usage_archive.go:237`、`usage_archive.go:652` |
| `'reported'` / `'missing'` | `cmd/gateway/main.go:2974,2976`、`usage_dao.go:506` |
| `'healthy'` / `'cooling'` / `'disabled'` | `credential_dao.go`（建表默认 `'healthy'`）、`cmd/gateway/main.go:1165`、`internal/admin/credential_handler.go:126`、`internal/upstream/upstream.go:203,475,512,665` |
| 日格式 `'2006-01-02'` | `usage_archive.go:530,532`、`cmd/gateway/billing.go:332,339` |
| `86400000`（一天毫秒） | `usage_source.go:64`、`usage_dao.go:884` |
| 限流上限 `1000` | `topup_dao.go:294,352` |

`usage_state` 的三个取值（`reported` / `missing` / `cost_pending`）目前分散在
`cmd/gateway/main.go:2972-2977`（`usageStateFor`）与
`internal/store/usage_dao.go:373`（`usageStateCostPending`）——
**store 包自己定义了一个，但另外两个在 cmd 包里**。若 `usage_state`
的取值集合再变（例如加一个 `estimated`），没有任何编译期或运行期的
一致性检查能发现「某个写入点漏了新状态」。

**可合并点**：在 `internal/store` 里定义一组 `const`（`UsageStatusOK`、
`UsageStatusCanceled`、`UsageStateReported` / `Missing` / `CostPending`），
`cmd/gateway` 引用它们。`cmd/gateway/main.go:2987-2991` 已经这么做过一次
（`usageStatusOK`），但只覆盖了 `ok` 一个。

---

## P3

### P3-1 `timeRangeClause` 是死代码

`internal/store/usage_dao.go:589-606`（`timeRangeClause`）在全仓**没有任何
调用点**（含测试）。它是 `UsageSource` 之前的旧实现残留 ——
`UsageSource`（`usage_source.go:105-165`）已经接管了时间过滤。
留着会误导后人以为时间过滤有两条路径。

### P3-2 `columnExists` 用字符串拼接 PRAGMA

`internal/store/store.go:950-976`：

```go
rows, err := s.db.Query(`PRAGMA table_info(` + table + `)`)
```

表名来自包内的常量白名单（`store.go:903-906` 的 `additions`、
`store.go:698-715` 的 `dropDeadColumns`），**没有注入面**
（注释也这么写）。但 `PRAGMA` 不支持参数绑定，只能拼接 ——
这意味着「表名必须来自白名单」这条约束是**纯靠代码评审维持**的，
没有任何机制阻止后来者把外部输入塞进来。建议加一个
`validIdentifier()` 白名单校验。

### P3-3 `usage_dao.go` 的批量落库路径有两处 `usageInsertColumns` 之类的魔数

`internal/store/usage_dao.go:266` 的 `const usageInsertColumns = 24`
注释说是「仅用于容量预估」，但 `:288` 的 `usageInsertArgs` 实际返回
24 个参数，而 `:151` 的单条 INSERT 也是 24 个 `?`。
三处（列清单字面量、占位符串、魔数）需要手工同步 ——
少一处就是「参数个数与占位符不匹配」的运行时错误
（`usage_source.go:105-113` 的注释描述过这类错误有多难查）。

---

## 附：schema 真相（从代码整理）

### 表清单（17 张）

`networks` / `providers` / `provider_credentials` / `model_groups` /
`upstream_models` / `routes` / `access_keys` / `groups` / `user_group_models` /
`users` / `usage_records` / `app_settings` / `route_targets` /
`user_usage_totals` / `audit_log` / `usage_daily_rollups` / `usage_totals` /
`balance_charges` / `balance_pending` / `balance_topups` / `model_probes`
（21 张，含归档三张）。

与实际库（`bin/data/gateway.db`）核对结果：**结构一致**。
实际库的表是在多次 `ALTER TABLE ADD COLUMN` 之后的样子，例如
`access_keys` 的建表语句末尾是 `, rpm_limit INTEGER NOT NULL DEFAULT 0,
tpm_limit INTEGER NOT NULL DEFAULT 0, user_id TEXT REFERENCES users(id)
ON DELETE CASCADE, allowed_models_json TEXT, expires_at INTEGER NOT NULL
DEFAULT 0, allowed_ips TEXT, group_id TEXT REFERENCES groups(id) ON DELETE SET NULL`。
`providers` 同样是「原 CREATE TABLE + 追加 `balance_spec_json` 与 `network_id`」。
这是 `ensureColumns` 的正常结果，**不是**缺陷，但说明「新库」与「老库」
的**物理表定义文本不同**（列顺序不同）——
`cmd/migrate-legacy` 用「按列名显式 INSERT」规避了这个问题（`main.go:196`），
但它自己的 `pcols` 是**按位置**拼的字符串，与当前 `providers` 的 13 列
不匹配（当前有 13 列，`pcols` 只有 11 列）。

### 索引清单（16 条显式索引）

`idx_usage_ts` / `idx_usage_key_ts` / `idx_usage_model_ts` /
`idx_usage_prov_ts` / `idx_usage_user_ts` / `idx_access_keys_user` /
`idx_users_group` / `idx_rollup_day` / `idx_rollup_user` / `idx_rollup_key` /
`idx_route_targets_route` / `idx_audit_ts` / `idx_balance_charges_user` /
`idx_balance_pending_user` / `idx_balance_topups_user` /
`idx_probes_model_ts` / `idx_probes_provider_ts`（17 条）。

**无索引的热查询列**：

| 表.列 | 热查询 | 状态 |
|---|---|---|
| `usage_records.status` | `RecomputePendingCosts`（`usage_dao.go:433`）`WHERE usage_state = ?` | ❌ **无索引**（`usage_state` 也没有）—— 每次 5 分钟全表扫 `usage_records`（30 天明细，百万级） |
| `usage_daily_rollups.day` | 已有 `idx_rollup_day` | ✅ |
| `networks.proxy_url` | `findNetworkByProxyURL`（`network_dao.go:241`） | ❌ 无索引 —— 但只在迁移期跑，可接受 |
| `provider_credentials.provider_id` | `ListCredentials`（`credential_dao.go:45`）、`HasEnabledCredential`（`:139`） | ❌ 无索引（表小，可接受） |
| `route_targets.route_id` | 已有 `idx_route_targets_route` | ✅ |
| `model_probes.ts` | `PruneOldProbes`（`probe_dao.go:213`）`WHERE ts < ?` | ❌ 无索引（但有 `(model_id, ts)` / `(provider_id, ts)`，全表剪枝会全扫） |

`usage_records.usage_state` 无索引是**最值得修**的一条：
`RecomputePendingCosts`（`usage_dao.go:432-434`）每 5 分钟
`SELECT id FROM usage_records WHERE usage_state = ? LIMIT 500`，
在 30 天明细（百万行）上是一次全表扫。`internal/store/usage_dao.go:421-425`
的注释说这是为了「避免无界扫描」，但**没有索引的 LIMIT 也是全表扫** ——
LIMIT 只限制返回行数，不限制扫描量。

### 触发器（3 个，全部 AFTER INSERT ON usage_records）

1. `trg_update_used_tokens`（`store.go:410-414`）：
   `UPDATE access_keys SET used_tokens = used_tokens + NEW.total_tokens WHERE id = NEW.access_key_id`
2. `trg_update_user_usage_totals`（`usage_archive.go:196-206`）：
   `INSERT INTO user_usage_totals ... ON CONFLICT DO UPDATE`（空 user_id 守卫）
3. `trg_update_usage_totals`（`usage_archive.go:227-249`）：
   `UPDATE usage_totals SET ... WHERE id = 1`

**共同缺陷**：三者都**只挂在 INSERT 上**。任何对 `usage_records` 的 UPDATE
（目前只有 `RecomputeCost`，`usage_dao.go:511`）都不会反映到这三张表 ——
见 P0-2。

**「配额原子预占」不在触发器里**，而在 Go 代码的显式事务里：
`ReserveQuota`（`key_dao.go:446`）/ `ReserveUserQuota`（`user_dao.go:545`）。
预占**不写** `used_tokens`，只写 `reserved_tokens`；`used_tokens` 由触发器写。
两列各只有一个写入者 —— 这条纪律在代码里是**成立**的
（`rg` 确认 `reserved_tokens` 的写入点只有 4 处：
`key_dao.go:484,512`、`user_dao.go:600,622`）。

### NOT NULL / 默认值

- `usage_records.usage_state` / `status` 是 `NOT NULL` 但**没有 CHECK 约束**，
  取值集合（`ok` / `canceled` / `truncated` / `overflow` / `error`；
  `reported` / `missing` / `cost_pending`）在 schema 层无约束。
- `users.balance_cents` 刻意可空（NULL = 不限额），且 `ALTER TABLE ADD COLUMN`
  **不带 DEFAULT**（`store.go:872-878`）—— 语义正确。
- `providers.balance_spec_json` 可空不带 DEFAULT（NULL = 从没配过），
  与 `''`（配过又清空）可区分 —— 语义正确，但读路径
  （`provider_dao.go:104` 的 `nullIfEmpty(p.BalanceSpecJSON)`）把 `''` 与
  NULL **压成同一个值**（都变 NULL）。也就是说「配过又清空」这个
  精心设计的第三态在**写入边界就丢了**。P2-3 同类问题。

### 类型匹配

- 金额一律 INTEGER 分（`users.balance_cents` / `balance_charges.amount_cents` /
  `balance_topups.delta_cents`）—— 无浮点混入。
- `users.balance_remainder` INTEGER 微元。
- `usage_records.cost_total` REAL（`store.go:812`）—— 与
  `usage_daily_rollups.cost_total` REAL 一致。**唯一的浮点金额**，
  但它单位是「元」且是有意为之（单价本身是小数）。
- `usage_daily_rollups.day` TEXT `YYYY-MM-DD`（本地时区），
  `usage_records.ts` INTEGER 毫秒。**没有 TEXT 存数字再 CAST** 的情况。
- `CAST` 只出现在 `strftime(...)` 的秒↔毫秒换算处
  （`usage_source.go:65`、`usage_dao.go:879-884`），无类型滥用。

---

## 已验证无问题的区域（附理由）

1. **SQL 注入面**：全包内的 SQL 拼接只有三类，逐个核对后**没有用户可控输入**：
   - 列名/表名常量拼接：`providerColumns`（`provider_dao.go:88`）、
     `modelColumns`（`model_dao.go:52`）、`keyColumns`（`key_dao.go:72`）、
     `routeColumns`（`route_dao.go:39`）、`routeTargetColumns`
     （`route_target_dao.go:26`）、`groupColumns`（`group_dao.go:47`）、
     `userColumns`（`user_dao.go:131`）、`topupColumns`（`topup_dao.go:104`）、
     `credentialColumns`（`credential_dao.go:22`）、
     `modelGroupColumns`（`model_group_dao.go:22`）—— 全部是包内常量。
   - `IN (...)` 列表：`model_dao.go:425-430`（`BatchUpdateModelPrices`）与
     `probe_dao.go:186-190`（`ProbeTimeline`）用
     `strings.Repeat("?,", n)` 拼**占位符**（不是值），值逐个绑定。
   - 排序方向/列名：`internal/admin/usage_handler.go:903,932` 的
     `u.` + column 与 order —— column 来自四个**字面量**调用点
     （`usage_handler.go:786,806,842,864`），不含用户输入。
   - `store.go:722,903` 的 `fmt.Sprintf("ALTER TABLE %s ...")` —— 表名/列名
     来自包内白名单常量。
   唯一「有注入面但被约束住」的是 `store.go:952` 的 `PRAGMA table_info(...)`
   拼接（P3-2）。

2. **主键 / 唯一约束**：`routes.public_name` UNIQUE、
   `users.username` COLLATE NOCASE UNIQUE、`groups.name` COLLATE NOCASE UNIQUE、
   `model_groups.display_name` UNIQUE、`upstream_models UNIQUE(provider_id, model_id)`、
   `access_keys.key_hash` UNIQUE、`providers.slug` UNIQUE、
   `networks.name` UNIQUE、`user_group_models PRIMARY KEY(group_id, public_model)`、
   `balance_charges PRIMARY KEY(request_id, user_id)`、
   `balance_pending PRIMARY KEY(request_id, user_id)`、
   `usage_totals CHECK(id = 1)` 且 migrations 里有
   `INSERT OR IGNORE INTO usage_totals (id) VALUES (1)`（`store.go:455`）
   —— 占位行一定存在（这是 `usage_archive.go` 注释里记的三个陷阱之一，
   已正确处理）。

3. **`balance_charges` 主键迁移**（`internal/store/balance_pk_migration.go`）：
   判定方式（`PRAGMA table_info` 数 pk 列，`:36-68`）可靠；
   整表重建在**单个事务**里（`:78-107`），失败整体回滚；
   `DROP TABLE` 后逐条重造索引（`:94`）—— 这是 SQLite 的必要步骤，已做。
   数据搬运不会丢行（旧单列主键 ⟹ 新复合主键只放宽不收紧）。
   **幂等**：迁过之后 pk 列数 ≥ 2，直接返回（`:60-62`）。

4. **`usage_daily_rollups` 的 7→8 列主键重建**（`usage_archive.go:96-142`）：
   判定用 `columnExists`；**表非空时拒绝启动**（`:107-111`）而不是悄悄丢历史；
   `DROP TABLE` 后重造全部 3 条索引（`:120-131`）。
   `ON CONFLICT` 目标列与主键逐字一致（8 列）。

5. **剪枝的「聚合与删除同事务、同谓词」**（`usage_archive.go:590-700`）：
   `prune_batch` 临时表圈定严格同一批 rowid，聚合与删除都以
   `rowid IN (SELECT rid FROM prune_batch)` 为谓词，同事务提交。
   实测剪枝前后的**总量**守恒（912567265 → 912567265），
   说明「进了表 A 的必然被删掉」这条不变式成立。
   （但**按天序列**会位移，见 P0-1。）

6. **触发器的 DROP+CREATE 幂等**（`store.go:409`、
   `usage_archive.go:196-206`、`usage_archive.go:220-249`）：
   全部用 `DROP TRIGGER IF EXISTS` + `CREATE`，不是
   `CREATE IF NOT EXISTS`（后者会让改过触发器体的老库永远保留旧体）。
   两个重建触发器的路径都把 DROP+CREATE 放在**同一条写事务**里
   （`usage_archive.go:199-206`、`:220-249`）——
   分两次 Exec 会留下「触发器已删没重建」的窗口。

7. **`user_usage_totals` 放在独立窄表**（`store.go:396-409`）：
   避免了 `users` 表被重建时触发器失效的问题（`usage_archive.go:151-165`
   记了实测报错 `error in trigger trg_update_user_usage_totals: no such
   table: main.users`）。这是对的架构决策。

8. **`priceUsage` 公式唯一**（`usage_dao.go:332-360`）：
   费用公式只有一份，余额扣费复用 `CreateUsageRecordWithCost` 返回的
   同一个变量（`cmd/gateway/main.go:3408`），
   「报表显示花了 X、余额扣了 Y」在结构上不可能漂移。
   `backfillUsageCost`（`usage_archive.go:262-320`）的 SQL 版公式与 Go 版
   逐字一致（口径核对通过）。

9. **写池/读池分离与事务边界**（`store.go:41-63`）：
   写池 `SetMaxOpenConns(1)`、读池 4 连接、WAL、`_busy_timeout=5000`。
   所有「检查与占用」都在写池事务里（`ReserveQuota` / `ReserveUserQuota` /
   `ChargeBalance` 的余数累加用 `UPDATE ... RETURNING`）。

10. **外键的级联语义**：`providers` → `provider_credentials` CASCADE、
    `providers` → `upstream_models` CASCADE、`routes` → `route_targets` CASCADE、
    `routes`/`route_targets` → `providers`/`upstream_models` RESTRICT、
    `users` → `access_keys` CASCADE、`groups` → `user_group_models` CASCADE、
    `access_keys`/`users` → `groups` SET NULL、`networks` → `providers` SET NULL、
    `model_groups` → `upstream_models` SET NULL、
    `users` → `balance_pending` CASCADE。删除 provider 撞 RESTRICT 时
    `internal/admin/helpers.go:94-95` 转成 409 + 人话。语义自洽。

11. **`retireOrphanKeys` 的幂等**（`store.go:1013-1029`）：
    `WHERE user_id IS NULL AND enabled = 1`，每次启动跑但只影响
    「仍启用且无归属」的行，管理员补 user_id 后不再被碰。

12. **`network_dao.go` 的「直连只有一种表示」**：
    `normalizeNetworkID`（`network_dao.go:135-140`）在写边界把
    `NetworkDirectID` 归一成空串（落 NULL），读路径只认 NULL ——
    「库里两个都指直连」的状态不可能出现。

13. **`usage_dao.go` 的负 token 归一**（`usage_dao.go:107-125`、
    `batchComputeCosts` `:251-278`）：单条与批量两条写入口都归一，
    触发器累加的终身值不会被拉低。

14. **归档层的 `COALESCE(user_id, '')`**（`usage_archive.go:660`）：
    明细的 `user_id` 可空而表 A 的维度列 NOT NULL，不 COALESCE 的话
    NULL 进主键让 `ON CONFLICT` 永不匹配 → 每次归档多插一行、数字翻倍。
    已正确处理。

15. **`migrations` 列表的顺序**（`store.go:102-596`）：
    `networks` 在 `providers` 之前（`providers.network_id` 的 FK 目标必须先存在）、
    `model_groups` 在 `upstream_models` 之前、`routes` 在 `route_targets` 之前、
    `users` 在 `access_keys` 之前 —— 全部满足 SQLite 的
    「ALTER TABLE ADD COLUMN ... REFERENCES 要求目标表已存在」约束。
    依赖新列的索引统一放在 `ensureUserIndexes`（`store.go:921-948`）里，
    在 `ensureColumns` 之后跑。

---

## 复现方式

本报告的所有「实测」结论均来自对 `bin/data/gateway.db` 的**只读**复制件
（复制到 `%TEMP%` 后写入），以及若干 `:memory:` 最小复现。仓库内
`bin/data/gateway.db` **未被修改**。
