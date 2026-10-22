# A8 · 资金相关 DAO 的并发与事务原子性审计

- 仓库：`rosetta-gateway`（module `github.com/cn-maul/rosetta-gateway`）
- 基线：`main` = `ea19ce1`
- 子域：`internal/store/` 中的余额（balance）、预占（quota reserve）、归档（usage archive）与聚合 DAO，以及 `DESIGN.md §11`
- 性质：**只读审计**。本报告未修改/创建/删除任何 git 跟踪文件（工作区内既有的 `internal/webui/dist/*` 与 `web/src/views/UsageDetail.vue` 改动在审计开始前就存在，与本次审计无关）。
- 日期：2026-10-20

---

## 0. 结论摘要

本子域的核心问题不是「某条 SQL 写错了」（A6 已覆盖），而是**资金路径上的「读-判-写」没有被收进同一条事务边界**。仓库里到处写着「原子」「天然免疫 check-then-act」的注释，但这些注释成立的前提是「那条 UPDATE 是本事务的第一条写语句」。一旦某个事务在写之前先做了 SELECT（`ReserveQuota` / `ReserveUserQuota` / `settlePendingForUser` / `RecomputeUsedTokens`），原子性就消失了。

共 15 条问题：**P0 × 4、P1 × 7、P2 × 3、P3 × 1**。

| 级别 | 数量 | 一句话概括 |
|---|---|---|
| P0 | 4 | 并发充值把同一笔欠款**重复结转**（多收用户钱）；结算与删账本之间崩溃导致**同一笔债被扣两次**；并发扣费把「无限额」用户**静默切成有限额**；`settlePendingForUser` 的整表 DELETE 抹掉并发写入的欠款（漏收） |
| P1 | 7 | 配额预占**先退后记账**造成超发；`reserved_tokens` 崩溃后**永久冻结**；`RecomputeCost` 改价后**不补记表 B**；`cost_pending` 标记被**剪枝销毁**；多实例下预占**直接失败**（`SQLITE_BUSY`）；报表费用与余额扣费**口径不一致**；聚合 DAO 的**读-读 TOCTOU** |
| P2 | 3 | 欠费路径的三条**非原子提交**；`balance_topups.balance_after` **记的是过期值**；`GetRecentPerformance` **绕过 UsageSource**，剪枝后指标归零 |
| P3 | 1 | `store.go:473-479` 关于 request_id 来源的注释与实现不符 |

### 验证方式

1. **实跑 `-race`**：`CC=C:/mingw64/bin/gcc.exe go test -race -count=1 -timeout 30m ./internal/store/` → **ok，240.419s**。仓库自带的并发/压力测试在 race 下全绿。**这一点本身就是结论的一部分**：下面每一条 P0/P1 都是在 race 检不出来的路径上（它们是 SQL 语义级 / 事务边界级缺陷，不是 Go 内存数据竞争）。
2. **树外探针**：用 `go test -overlay` 把一份探针测试文件**虚拟映射**进 `internal/store` 包（不落盘到仓库），共 16 组用例，逐条验证下面的假设。探针文件存放在 `a8probe\`（仓库外），运行方式：

   ```
   go test -overlay a8probe\overlay.json -count=1 -v -run "TestA8_" ./internal/store/
   ```

   复现率见每条问题的「实测」小节。

---

## 1. P0

### P0-1 并发充值把同一笔 `balance_pending` 欠款**重复结转**（多收用户钱）

**位置**：`internal/store/balance_dao.go:564-589`（`settlePendingForUser`）、`internal/store/topup_dao.go:225`（调用点）

```go
// balance_dao.go:564
func (s *Store) settlePendingForUser(ctx context.Context, dbtx dbtx, userID string) error {
	var totalMicros sql.NullInt64
	if err := dbtx.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(amount_micros), 0) FROM balance_pending WHERE user_id = ?`,
		userID).Scan(&totalMicros); err != nil { ... }
	...
	cents := totalMicros.Int64 / MicrosPerCent
	if cents > 0 {
		if _, err := dbtx.ExecContext(ctx,
			`UPDATE users SET balance_cents = MAX(0, COALESCE(balance_cents,0) - ?), updated_at = ?
			  WHERE id = ?`, cents, time.Now().UnixMilli(), userID); err != nil { ... }
	}
	if _, err := dbtx.ExecContext(ctx,
		`DELETE FROM balance_pending WHERE user_id = ?`, userID); err != nil { ... }
```

**为什么是 P0**：这是**多收用户的钱**。调用点 `topup_dao.go:225` 传的是 `s.db`（不是 `*sql.Tx`），所以 SUM / UPDATE / DELETE 是**三条独立的自动提交语句**，中间没有任何互斥：

- 两个管理员同时给同一用户充值 → 两个事务各自 SUM 到同一笔 `balance_pending` → 各自扣一次 `cents` → **同一笔欠款被扣两次**。
- 更糟：`DELETE FROM balance_pending WHERE user_id = ?` 是**整表按 user_id 删**（不是「删掉我 SUM 到的那些行」），所以第二个事务的 DELETE 会把第一个事务刚插入的新欠款行也一并删掉。

`topup_dao.go:220-224` 的注释明确说「本函数刻意不绑事务」，理由是「不为此把低频管理写绑进长事务」。但 `settlePendingForUser` **不是低频的写** —— 它是一笔**扣款**，而且与「充值」共用一条不互斥的路径。注释里说「方向的选择原则：宁可多扣一次（可发现、可纠正）也不漏收」—— 这个取舍本身可以接受，**但它没有意识到并发充值下会多扣 N 次**，而 N 由并发度决定，且每一次多扣都记在同一张 `balance_topups` 流水里（`delta_cents` 一样、`balance_after` 不同），事后无法区分哪笔是重复结转。

**实测**（`TestA8_ConcurrentTopupDoubleSettlesPending` / `TestA8_DoubleSettleFrequency`）：

```
pending micros=3000000 (=300 cents)
balance=78801 pending_rem=0 ; single-settle expectation = 8*10000+1-300 = 79701
DOUBLE SETTLE: balance=78801, want >= 79701 (pending deducted more than once)

double-settle in 59/60 iterations      ← 复现率 98%
iter 0: balance=58201 want>=59701 (extra deduction 1500 cents)
iter 1: balance=59101 want>=59701 (extra deduction 600 cents)
iter 2: balance=58801 want>=59701 (extra deduction 900 cents)
```

多实例（两个 `Store` 打开同一文件）同样复现（`TestA8_TwoStoresConcurrentTopupDoubleSettle`：`balance=59401`，期望 `59701`，差 300 分 = 一次重复结转）。

**根因归纳**：`settlePendingForUser` 是典型的「读-判-写 + 批量删」三段式，却运行在自动提交模式下。它需要的是**一条事务 + 条件删除**：

```sql
-- 正确形态（示意）：同一个事务里
BEGIN IMMEDIATE;
  SELECT request_id, amount_micros FROM balance_pending WHERE user_id=?;
  UPDATE users SET balance_cents = ... WHERE id=?;
  DELETE FROM balance_pending WHERE user_id=? AND request_id IN (SELECT request_id FROM balance_pending WHERE user_id=?);
COMMIT;
```

且 `DELETE` 必须是「删我 SUM 到的那些行」而不是「删该用户全部行」，否则并发写入的新欠款会被无辜抹掉（见 P0-4）。

**修法方向**：
- 把 `settlePendingForUser` 改成接受 `*sql.Tx`，在 `AdjustBalanceWithLedger` 里用一条 `BEGIN IMMEDIATE` 事务包住「加余额 → 结转 → 删账本」三步；
- `DELETE` 带上 `request_id IN (本次 SUM 到的行)` 的限定；
- 顶层加一个「同一用户同一时刻只允许一笔充值/结转」的进程内互斥（key = user_id），作为单进程内的第二道防线。

---

### P0-2 结算与删账本之间的崩溃窗口 → 同一笔欠款被**扣两次**

**位置**：`internal/store/balance_dao.go:577-587`（UPDATE 与 DELETE 之间）

`settlePendingForUser` 的扣款 UPDATE 与账本 DELETE 是**两条自动提交语句**。进程若在两者之间被杀：

1. 余额已经被扣了 `cents`；
2. `balance_pending` 里的行**还在**（欠款仍然挂着）；
3. 下一次充值（或管理员手动调一次）会**再扣一次**。

这与 P0-1 是**同一个根因的两面**：P0-1 是并发导致重复结转，P0-2 是崩溃导致重复结转。后者尤其恶劣，因为它是**确定性的**（不是概率性的），且没有任何日志表明发生了什么。

**实测**（`TestA8_CrashBetweenSettleUpdateAndDelete`）：

```
after crash: balance=99700 pending still=3000000 micros (=3.00 yuan)
after next topup: balance=109400 pending=0 ; the 3-yuan debt was deducted TWICE
                   (100000 -> 100000+10000-300 = 109700)
OVER-DEDUCTION: balance=109400, want 109700 (the same debt settled twice)
```

**修法方向**：同 P0-1 —— UPDATE 与 DELETE 必须在同一事务内。理想形态是「UPDATE 的影响行数与 DELETE 的影响行数必须一致」的自校验：若 `DELETE` 的行数 ≠ SUM 时读到的行数，说明有并发插入，事务必须回滚并重试。

---

### P0-3 并发扣费把「无限额」用户**静默切成有限额**

**位置**：`internal/store/balance_dao.go:578-582`（`settlePendingForUser` 的 UPDATE）

```sql
UPDATE users SET balance_cents = MAX(0, COALESCE(balance_cents,0) - ?), updated_at = ?
  WHERE id = ?
```

`balance_cents` 的 **NULL = 不限额**（`balance_dao.go:9-11` 明确写「『不限额』= 列值 NULL，不是 0」）。而这条 UPDATE 里的 `COALESCE(balance_cents,0)` 把 NULL 当成 0，于是：

- 用户原本是**不限额**（NULL），因为之前欠费被记进了 `balance_pending`；
- 管理员给他充值 → 结转执行 `COALESCE(NULL, 0) - cents` → 写回一个**非 NULL 的整数**；
- 用户从「无限额度」变成「有限额（余额 9700 分）」。

这是**语义突变**，且没有任何告警。更严重的是方向：它可能把用户的余额**设成 0**（若 `cents` 足够大且 `MAX(0, …)` 生效），让一个原本不受限的用户**立刻被余额预检拦住**（`BalanceOf` 返回 `limited=true, cents=0` → `precheckBalance` 判 `balanceReject` → 402）。

**实测**（`TestA8_SettleConvertsUnlimitedToZeroBalance`）：

```
after SetBalance(unlimited): cents=0 limited=false err=<nil>
after topup: cents=9700 limited=true err=<nil>
UNLIMITED LOST: the user was converted from unlimited to a 9700-cent limited balance
```

**修法方向**：`settlePendingForUser` 的 UPDATE 必须与 `AdjustBalance`（`balance_dao.go:627`）一样处理「NULL = 不限额」的语义：若 `balance_cents IS NULL`，结转应当**不写这一列**（无限额用户不存在「余额不足」，欠款无从谈起）—— 实际上无限额用户根本不会进欠费分支（`ChargeBalance` 的 `balance_cents IS NOT NULL` 守卫，`balance_dao.go:355`），但 `SetBalance` 可以在欠费发生**之后**把用户切成不限额，于是 pending 行就变成了「给一个无限额用户记账的债务」—— 这本身就该被拒绝或清理。

---

### P0-4 `settlePendingForUser` 的整表 DELETE 抹掉并发写入的欠款（漏收）

**位置**：`internal/store/balance_dao.go:585`（`DELETE FROM balance_pending WHERE user_id = ?`）

这条 DELETE 按 `user_id` 删**全部**行，而不是「删我 SUM 到的那些行」。于是：

- 事务 A（充值结转）SUM 到 100 分，执行 UPDATE 扣 1 元；
- 事务 B（另一笔欠费）在这期间 INSERT 了一条新的 `balance_pending`（5 元）；
- 事务 A 的 DELETE 把 B 那条**新欠款**也删了 —— 而 B 的 INSERT 记的是「这笔钱用户欠着」，**从未被扣**。

结果是**漏收**（与 P0-1 的多收方向相反，但同样破坏账目）。而且这条 DELETE 还带着「删掉了自己没读过的行」这个性质：结转的 SUM 与 DELETE 的作用集合**不一致**。

**实测**（`TestA8_SettleDropsPendingInsertedConcurrently` / `TestA8_PendingLossFrequency`）：

```
pending remaining=0 micros (=0.00 yuan), balance=1000400
PENDING LOST: the concurrently inserted 5-yuan pending row was deleted by
               settlePendingForUser's blanket DELETE without being deducted

pending row silently dropped in 60/60 iterations      ← 复现率 100%
```

**修法方向**：DELETE 必须限定 `request_id IN (本次 SUM 覆盖的行)`。若采用「事务内 SELECT 出 request_id 列表 → UPDATE → DELETE ... WHERE request_id IN (...)」，则并发 INSERT 的新行自然不被触及。

---

## 2. P1

### P1-1 配额预占**先退后记账** → 终身配额被超发

**位置**：`cmd/gateway/main.go:2888-2892`（流式收尾）、`main.go:2955-2959`（非流式收尾）、`internal/store/key_dao.go:445-491`（`ReserveQuota`）、`internal/store/user_dao.go:545-607`（`ReserveUserQuota`）

`DESIGN.md:1086-1089` 描述的三段式是：

```
请求后（rateCommit 收尾）：
  usage_records INSERT → 触发器 used_tokens += total_tokens
  ReleaseQuota：reserved_tokens -= reserved
```

但实现里**这两步的顺序是反的**：

- `attemptStream`（`main.go:2853`）先 `usage.record(...)` —— 这是**入队**，不是落库；
- 落库由 `usageRecorder.worker`（`main.go:3153`）异步完成（队列容量 1024，攒批 50）；
- 而 `rate.commit(lastUsage.TotalTokens)`（`main.go:2889`）**在 `usage.record` 之后同步执行**，它调用 `releaseQuota`（`main.go:1553`）→ `ReleaseQuota`（`key_dao.go:507`）→ `reserved_tokens` **立刻**减掉。

于是从「预占被退」到「usage 落库、触发器累加 `used_tokens`」之间存在一个**无对账窗口**：这段时间里该请求**既不在 `reserved_tokens` 里、也不在 `used_tokens` 里**。并发进来的请求看到的是「额度还剩那么多」，被放行。

`DESIGN.md:1113-1116` 断言「三个写入点互不重叠，配额判定 `used+reserved+est <= quota` 在写锁串行化下无竞态窗口」—— 这个断言的前提是「`used_tokens` 的累加与预占的释放**在时间上相邻**」。异步落库把这个前提打破了。

**实测**（`TestA8_QuotaGapBetweenReleaseAndUsageInsert` / `TestA8_UserQuotaGapBetweenReleaseAndUsageInsert`）：

```
request B admitted while request A's usage is not yet in the DB: ok=true
OVERSHOOT: quota 1000 admitted 2000 tokens of requests (release precedes the usage INSERT)

user-level request B admitted: ok=true
OVERSHOOT: user quota 1000 admitted 2000 tokens
```

**量级**：窗口 = 队列里排队的记录数 / 落库速率。队列满（1024）时窗口可达秒级。在高并发下，这意味着**终身配额的超发量与窗口内的并发请求数成正比**，而 `est` 本身按 `max_tokens` 上限估算（`main.go:1610-1626`），超发的 token 数被进一步放大。

**修法方向**：
- 让「usage 落库」与「退预占」在**同一条写事务**里完成：把 `ReleaseQuota` 挪到 `usageRecorder` 的落库事务内（与 INSERT 同事务），或
- 引入「已用未记账」的第三列（如 `inflight_tokens`），在退预占的同时把 est 挪进它，usage 落库时再由触发器对账冲销；或
- 至少把退预占**延后**到落库之后（把 `releaseQuota` 交给 `usageRecorder` 在 charge 之后调用）。

---

### P1-2 `reserved_tokens` 崩溃后永久冻结（终身额度）

**位置**：`internal/store/key_dao.go:507-513`、`internal/store/user_dao.go:617-623`、`internal/store/store.go:234` / `285`

A4 已从热路径角度报过这条（`A4-hotpath-billing.md` P0-3），本节只补**存储层视角**的证据与量化：

`reserved_tokens` 是**在途预占**（语义上属于「进程内的在途状态」），但它被持久化在库里。进程被 SIGKILL / OOM / 容器驱逐后，所有在途预占**永久留在库里**：

- `ReserveQuota` 的判定是 `used + reservedNow + est > quota`（`key_dao.go:476`）；
- `reserved_now` 单调偏高 → 判定恒成立 → 这把 key **永久 429**；
- 界面上 `used` 与 `quota` 都是对的，**看不出任何异常**。

全仓 `rg "reserved_tokens"` 的写入点只有 4 处（2 个 Reserve 的 `+`、2 个 Release 的 `-`），**没有任何启动复位语句**。`UpdateAccessKey`（`key_dao.go:317-323`）也不碰这一列。

**实测**（`TestA8_ReservedTokensNeverResetOnRestart`）：

```
reserved=900
after reopen reserved_tokens=900
LEAK: reserved_tokens=900 survives restart -> key permanently 429
```

**修法方向**：`store.Open` / `migrate()` 之后执行 `UPDATE access_keys SET reserved_tokens = 0` 与 `UPDATE users SET reserved_tokens = 0`（进程启动时不存在在途请求）。更稳妥的是给预占加 TTL（记录预占时间戳，超过 N 分钟视为失效）。注意这必须与 P1-1 一起改：若 P1-1 采用「inflight 列」方案，则该列同样需要复位。

---

### P1-3 `RecomputeCost` 改价后不补记表 B —— A6 根因在归档/并发下的放大形态

**位置**：`internal/store/usage_dao.go:489-521`（`RecomputeCost`）、`internal/store/usage_archive.go:227-249`（`trg_update_usage_totals`）

A6 已报根因（`A6-store-schema-dao.md` P0-2：触发器只挂 AFTER INSERT，`RecomputeCost` 的 UPDATE 不触发任何累计表）。本节补两点 A6 未覆盖的：

1. **归档后表 A 与表 B 互相矛盾**：A6 的推论 1 已提到这点，但未给实测。实测（`TestA8_RollupVsLifetimeAfterRecompute`）：

   ```
   detail=0.000000 rollup=0.020000 lifetime=0.000000
   A/B DISAGREE: rollup=0.020000 lifetime=0.000000
   ```

   即**表 A（`usage_daily_rollups`）有 0.02 元、表 B（`usage_totals`）有 0 元**。表 B 存在的唯一理由是「剪枝后累计不变」（`usage_archive.go:20-25`），而它现在与表 A 矛盾。`GetUsageLifetime`（`usage_archive.go:769`）读的是表 B → 总览「全部」档**永久少算**。

2. **并发放大**：`RecomputePendingCosts`（`usage_dao.go:428-471`）先在**读池**捞出一批 id（`usage_dao.go:432-434`），再**逐条**在写池 UPDATE（`usage_dao.go:510-512`）。这期间：
   - 剪枝可能把某条记录搬进表 A（`PruneOldUsage`）→ 随后的 UPDATE 命中 0 行 → `n > 0` 为 false → 该条**既没被重算、也没被记录**，静默丢失（`usage_dao.go:516-520`）。
   - 多个 `RecomputePendingCosts` 并发（启动时一次 + 5 分钟 ticker，`main.go:790-796`）会捞到**同一批 id** → 两条同时 UPDATE 同一行 → 后者覆盖前者的结果（这里因为是幂等的赋值，无害，但配合剪枝就是上面那个丢失）。

   实测（`TestA8_RecomputeCostRealDivergence`）：

   ```
   fixed=1 detail SUM(cost_total)=0.020000 usage_totals.cost_total=0.000000
   DIVERGENCE: usage_totals.cost_total=0.000000 != SUM(details)=0.020000 (trigger is INSERT-only)
   ```

**修法方向**：给 `usage_records` 加 AFTER UPDATE 触发器（把 `cost_total` 差额补进表 B），或让 `reconcileUsageTotals`（`usage_archive.go:402`）从「一次性 + marker 挡住」改成「可重入的对账任务」。**注意**：`RecomputeCost` 改成补记表 B 之后，A4 提的「重算后要不要补扣余额」就成了下一个必须回答的问题 —— 补记表 B 只修报表，余额那一侧仍然漏收（见 P1-4）。

---

### P1-4 `cost_pending` 标记被剪枝销毁 → 待重算的费用永久归零

**位置**：`internal/store/usage_archive.go:630-672`（归档 INSERT 走 `ON CONFLICT DO UPDATE`，**不写 `usage_state`**）、`internal/store/usage_archive.go:681`（DELETE 明细）

`usage_daily_rollups` 的表结构里**没有 `usage_state` 列**（`usage_archive.go:39-83`）。于是：

- 一条 `usage_state='cost_pending'`、`cost_total=0` 的记录被剪枝时，归档行记的是 `cost_total=0`，**`cost_pending` 这个「待重算」的事实被彻底销毁**；
- 后台的 `RecomputePendingCosts`（`usage_dao.go:432`）按 `usage_state = 'cost_pending'` 扫描明细表 —— 明细行已被删，**再也捞不到它**；
- 这笔费用永久是 0：报表里 0 元，表 B 里 0 元，余额也 0（从未扣过）。

`RecomputePendingCosts` 每 5 分钟跑一次（`main.go:791`），`PruneOldUsage` 每天跑一次（`main.go:743`）。`cost_pending` 出现在「读池抖动导致查价失败」时（`usage_dao.go:403-406`）—— 也就是说**读池恰好在那 30 天窗口里抖动过**的记录，会在剪枝那一刻被静默归零。

**实测**（`TestA8_PruneDestroysCostPendingMarker`）：

```
pending rows left=0 rollup cost=0.000000 lifetime cost=0.000000 (true cost = 0.02)
```

注意 `rollup cost=0.000000` 而 `lifetime cost=0.000000` —— 这次两边一致（因为重算没跑），但**真实费用是 0.02 元**，两个数字都是错的，且**永久无法恢复**。

**修法方向**：归档表加 `pending_cost_count`（或 `pending_cost_micros`）列，把「这批里有几条待重算」这个事实一并归档；或让 `RecomputePendingCosts` 在剪枝**之前**跑一轮（但这不解决「长期读池故障 + 剪枝」的组合）；或让剪枝**跳过** `usage_state='cost_pending'` 的行（最简单，但会让待重算的记录长期滞留）。

---

### P1-5 多实例（两个 `Store` 打开同一文件）下配额预占**直接失败**（`SQLITE_BUSY`）

**位置**：`internal/store/key_dao.go:445-491`（`ReserveQuota`）、`internal/store/user_dao.go:545-607`（`ReserveUserQuota`）

`ReserveQuota` 的事务形态是「**先 SELECT，后 UPDATE**」：

```go
tx, err := s.db.BeginTx(ctx, nil)          // 延迟事务（DEFERRED）
...
tx.QueryRowContext(ctx, `SELECT quota_tokens, used_tokens, reserved_tokens FROM access_keys WHERE id = ?`)   // ← 读，拿到读快照
...
if used+reservedNow+est > quota { ... }
tx.ExecContext(ctx, `UPDATE access_keys SET reserved_tokens = reserved_tokens + ? WHERE id = ?`)             // ← 写，要升级为写事务
```

SQLite 的 DEFERRED 事务在「先读后写」时，若在读快照与首次写之间有**其它连接提交**，则升级写事务时返回 `SQLITE_BUSY_SNAPSHOT`（错误码 517），**且 `_busy_timeout` 对它不生效**（busy handler 只对「等锁」有效，对「快照已失效」无效）。

DSN 里 `_busy_timeout=5000`（`store.go:35`）的注释明确说「备份脚本、CLI 工具、误起的第二个实例都会以独立连接打开同一个文件 —— 那种情况下没有它，一次写撞锁就直接失败，而**配额预检是 fail-open 的，会静默放行超额请求**」。但这条注释只考虑了「等锁」类的 BUSY，没考虑 BUSY_SNAPSHOT。

**实测**（`TestA8_TwoStoresReserveQuotaBusy`）：

```
ok=12 lockErrs=8 otherErrs=0
first=[database is locked (5) (SQLITE_BUSY) database is locked (517) ...]
RESERVE FAILS under multi-instance: 8 SQLITE_BUSY errors
```

对照组（`TestA8_TwoStoresWriteFirstVsReadThenWrite`）—— 同样两个 Store，`ChargeBalance` **零** busy 错误，因为它的第一条语句就是写（`UPDATE ... RETURNING`，`balance_dao.go:353-357`）：

```
ChargeBalance (write-first): ok=20 busy=0
ReserveQuota  (read-then-write): ok=13 busy=7
```

这说明**「写在前」不是风格问题，是 SQLite 下的正确性要求**：写开头的 DEFERRED 事务会立刻拿到写锁，不会遇到 BUSY_SNAPSHOT。

**影响**：`ReserveQuota` 失败 → `main.go:1848-1850` 的 fail-open 分支**记 ERROR 后放行**（`main.go:1849`）→ 多实例部署下终身配额**静默失效**。`ReserveUserQuota` 同理（`main.go:1896-1901`）。

**修法方向**：
- 把 `ReserveQuota` / `ReserveUserQuota` 的事务改成「**写先行**」：第一条语句就是一条带条件的 UPDATE（把「检查 + 占用」压进一条语句，正如 `ChargeBalance` 的 `balance_cents >= ?` 守卫那样），或在 BeginTx 时用 `BEGIN IMMEDIATE`。
- 顺带：`_busy_timeout` 对 BUSY_SNAPSHOT 无效这一点应写进 `store.go:28-31` 的注释。

---

### P1-6 报表费用与余额扣费的口径不一致（`status != 'ok'` 的用量进报表不扣费）

**位置**：`internal/store/usage_by_user_dao.go:101`（`SUM(u.cost_total)`，不筛 status）、`cmd/gateway/main.go:3384-3386`（`charge` 只对 `status == "ok"` 扣费）

- 报表侧：`UsageByUser` 对**所有** status 求和 `cost_total`（`usage_by_user_dao.go:101`），`UsageSource` 的明细支也不筛 status（`usage_source.go:28-41`）。
- 扣费侧：`usageRecorder.charge` 明确「只对成功的请求扣费」（`main.go:3384-3386`）。

`main.go:3357-3359` 的注释说这是**有意的口径区分**：

> 与 cost_total 的差别要记牢：cost_total **无论成败都记**（那是网关的上游成本），扣费**只看成功**（那是用户的应收）。

这个区分本身是合理的产品决策。**但它带来一个后果**：`balance_dao.go:12-15` 声称「本包**不重算费用**：调用方拿的是 `CreateUsageRecordWithCost` 返回的那个 cost 值……所以『报表显示花了 X、余额扣了 Y』在结构上不可能发生」。这个「结构上不可能」的说法**不成立** —— 结构上保证的是「同一条记录的费用值一致」，不是「报表总额 = 余额扣款总额」。

管理员在「谁花得多」页面（`usage_by_user_handler.go:102`）看到的数字，与该用户余额实际减少的数字，**结构性地不相等**。差额 = 所有失败/取消/截断请求的费用。

**实测**（`TestA8_ReportCostVsBalanceCharged`）：

```
report SUM(cost_total)=0.0400 yuan ; balance charged=0.0200 yuan (delta 0.0200)
UsageByUser total users=1 rows=[{u1 u1 user 2 4000 0.04}]
```

（一条 ok + 一条 error，token 量相同：报表 0.04 元，余额只扣 0.02 元。）

**影响**：这不是「算错钱」，是「两个口径的差没有在任何地方被声明为正常」。当一个用户投诉「报表说我花了 100 元，余额只少了 60 元」时，运维没有依据解释这个差额。

**修法方向**：不需要改扣费逻辑，需要**改口径声明**：
- 在 `usage_by_user_dao.go` 的注释与 `UsageByUserStat.Cost` 的字段注释里明确「这是网关的上游成本，含失败请求；用户应收见 `balance_charges`」；
- 或在 `UsageByUser` 里加一列 `charged_cents`（来自 `balance_charges`），让管理员能直接对账；
- 或在 `balance_dao.go:12-15` 修正那句「结构上不可能发生」的表述。

**待确认**：`usage_records` 里 `status != 'ok'` 的记录到底会不会产生 `cost_total > 0`？实测证明会（`freezeUsageCost` 不筛 status，`usage_dao.go:392-414`）。

---

### P1-7 聚合 DAO 的**读-读 TOCTOU**：`UsageByUser` 的「总用户数」与「行数」来自不同快照

**位置**：`internal/store/usage_by_user_dao.go:83-85`（总数查询）、`usage_by_user_dao.go:114`（聚合查询）

```go
if err := s.read.QueryRowContext(ctx,
    `SELECT COUNT(DISTINCT u.user_id) FROM `+src, args...).Scan(&total); err != nil { ... }
...
rows, err := s.read.QueryContext(ctx, query, rowArgs...)
```

两条**独立的读池查询**，中间没有事务。WAL 下每条语句各取一个快照，于是「有消费的用户总数」与「实际返回的行数」可以不一致。

这个数字的用途被明确写出来了（`usage_by_user_dao.go:75-81`）：

> 为什么必须有它：列表被 limit 截断时，调用方**只有**拿到总数才能说出「显示前 N / 共 M 位用户」。

也就是说，这个数字是**给管理员看的一致性声明**（「前 100 / 共 M 位」）。它在并发写入下会说谎。

**实测**（`TestA8_UsageByUserCountVsRowsInconsistent`）：

```
iteration 7: total=3 but len(rows)=2
iteration 9: total=3 but len(rows)=2
iteration 11: total=2 but len(rows)=3
inconsistent count-vs-rows in 16/50 iterations
```

**修法方向**：把两条查询合并进一条（同一条 SQL 里用窗口函数 `COUNT(*) OVER ()`），或把两条查询放进同一个读事务（`s.read.BeginTx` + `BEGIN DEFERRED` 保持同一快照）。前者更好 —— 一条语句天然同快照。

---

## 3. P2

### P2-1 欠费路径的三条**非原子提交**

**位置**：`internal/store/balance_dao.go:497-527`（欠费分支）

欠费分支的时序是：

1. `tx.Rollback()`（`balance_dao.go:511`）—— 撤销主事务（含余数累加、占位行）；
2. `recordPendingRemainder`（`balance_dao.go:519`）—— **独立事务**写 `balance_pending`；
3. 返回 `ErrInsufficientBalance`（`balance_dao.go:527`）。

这三步之间没有原子性。崩溃点分析：

| 崩在 | 后果 |
|---|---|
| 1 之后、2 之前 | 这笔欠费**彻底消失**（无流水、无账本、余额未动） |
| 2 之后、3 之前 | 账本记了欠费，但调用方以为失败 → 若将来加重试，重复记账（有 `INSERT OR IGNORE` 保护，但只对同 `request_id` 有效） |

`balance_dao.go:281-295` 的注释已经承认了「余数随回滚丢失」这个缺口并用 `balance_pending` 修补，但**修补本身没有事务边界**。

**实测**（`TestA8_EmptyRequestIDPendingNotRecorded`）—— 空 `request_id` 时（`recordPendingRemainder` 因 `requestID == ""` 直接 return，`balance_dao.go:544`）欠款**完全蒸发**：

```
err=insufficient balance
pending=0 micros (3 yuan debt not recorded)
PENDING LOST: 3 yuan of debt vanished with an empty request_id
```

生产里 `request_id` 恒非空（`server.Middleware` 生成，`internal/server/server.go:38-42`），但 `ChargeBalance` 是**公共 API**，`requestID == ""` 时既不记占位（`balance_dao.go:324`）也不记欠款（`balance_dao.go:518`）—— 余额**扣了但流水里查不到**：

```
balance=99700 (dropped 300 cents) but balance_charges sums to 0
LEDGER GAP: balance dropped 300 cents but the ledger records 0
```

**修法方向**：把「写 `balance_pending`」与「返回 `ErrInsufficientBalance`」放进同一个提交单元；或者把欠费记录改成**先写后判**（先在主事务里写 pending，再决定扣费成败）。

### P2-2 `balance_topups.balance_after` 记的是**过期值**

**位置**：`internal/store/topup_dao.go:201-213`（RETURNING 读回）、`topup_dao.go:225`（结转）、`topup_dao.go:229-241`（构造 Topup）

`balance_after` 的值来自 `UPDATE ... RETURNING balance_cents`（`topup_dao.go:202-207`），读的是**结转之前**的余额。而 `settlePendingForUser`（`topup_dao.go:225`）在**这之后**又把余额减掉了结转额。所以流水里记的 `balance_after` 与实际余额**差一个结转额**。

**实测**（`TestA8_TopupLedgerBalanceAfterIsStale` / `TestA8_TopupLedgerBalanceAfterMismatch`）：

```
ledger balance_after=10001 real balance=9701 diff=300
```

`topup_dao.go:60-67` 的注释说「流水里的 `balance_after` 是链式相减还原每笔的锚点」—— 锚点错了，链式相减就还原不出真实余额。

**影响**：这是**审计账**的缺陷，不是余额本身的缺陷（余额是对的）。但 `balance_topups` 是「钱进来」这一侧的唯一对账依据（`store.go:524-537`），它的 `balance_after` 与 `users.balance_cents` 对不上，管理员/审计就无法用它核对。

**修法方向**：把结转移到 `RETURNING` **之前**（先结转、再加余额、再 RETURNING），或在结转后重新读一次余额再构造 `Topup`。

### P2-3 `GetRecentPerformance` 绕过 `UsageSource`，剪枝后指标归零

**位置**：`internal/store/recent_performance_dao.go:24-38`

```go
err := s.read.QueryRowContext(ctx, `
	WITH throughput AS (
	 SELECT output_tokens, latency_ms FROM usage_records
	 WHERE status = 'ok' AND output_tokens > 0 AND latency_ms > 0`+cond+`
	 ORDER BY ts DESC, id DESC LIMIT ?
	), ...
```

它**直接读 `usage_records`**，而不是走 `UsageSource`（明细 ∪ 日归档）。这与仓库里其它所有聚合入口的口径相反 —— `usage_dao.go` 里所有聚合都走 `UsageSource`（`usage_source.go:69-96` 明确说明「剪枝一旦真的跑起来，这些入口的求和就会随时间自动变小」是**要避免的**）。

后果：剪枝后，这个「近期表现」卡片的**速度与首字延迟样本全部消失**（显示 0），而同一页面上的其它数字（总额、请求数）因为走了 `UsageSource` 仍在。

**实测**（`TestA8_RecentPerformanceDivergesAfterPrune`）：

```
before prune: tps=100.0000 samples=2 ttfb=0.0 n=0
after prune:  tps=0.0000 samples=0 ttfb=0.0 n=0
AFTER PRUNE the recent-performance samples vanished (2 -> 0)
```

（注意：`ttfb` 的样本数在剪枝**前**就是 0，因为 `ttfb_ms > 0` 的过滤（`recent_performance_dao.go:31`）在本 fixture 里没满足。）

另外还有一处口径细节：这里的速度分子分母是 `output_tokens / latency_ms`（`recent_performance_dao.go:34`），而归档侧的口径是 `ok_output_tokens / ok_latency_sum_ms`（`usage_source.go:38-39`、`usage_archive.go:653-654`）—— **分母的样本集合不同**（这里要求 `output_tokens > 0`，归档侧只要求 `status='ok'`）。即使改成走 `UsageSource`，两套口径也需要对齐。

**修法方向**：改走 `UsageSource`（用 `u.out_ok / u.lat_ok` 与 `UsageRatioExpr`），对齐归档侧口径。

---

## 4. P3

### P3-1 `store.go:473-479` 关于 request_id 来源的注释与实现不符

`store.go:473-479` 附近与 `balance_charges` 主键相关的注释写着「request_id 来源于**客户端可控的请求头**，碰撞并不遥远」。实际实现是 `server.Middleware` 用 `crypto/rand` 生成 8 字节（16 hex 字符）的 request_id（`internal/server/server.go:29-33`、`38-42`），**不读客户端头**。A4 已从热路径角度指出过同一条注释（`A4-hotpath-billing.md` §8.5）。

本子域补充一条**该注释没提到的风险**：`request_id` 只保证**单次请求内**唯一。若将来支持「客户端自带 request_id 用于重试去重」（这是幂等扣费的自然演进方向），碰撞概率会从 2^-64 变成 2^-16（8 字节）甚至更低 —— 而 `balance_charges` 的复合主键正是靠它防重复扣费。

**修法方向**：修正注释；若将来允许客户端指定 request_id，需提高其熵并加长度/格式校验。

---

## 5. 事务边界与 SQLite 并发行为的系统梳理

### 5.1 写池 / 读池配置

`internal/store/store.go:20-66`：

```go
dsn := dbPath + "?_journal_mode=WAL&_synchronous=NORMAL&_foreign_keys=ON&_busy_timeout=5000"
db.SetMaxOpenConns(1)     // 写池：单连接
read.SetMaxOpenConns(4)   // 读池：4 连接
```

单连接写池意味着**单进程内不存在写-写竞态**（所有写排队）。这掩盖了大量问题：上面 P0-1/P0-2/P0-4/P1-5 的全部复现都是**跨连接**的（两个 `Store` 实例，或「读池 + 写池」混合）。

**关键结论**：单进程单连接下，`Store` 的**写路径**是安全的（SQL 语句要么是单条原子 UPDATE，要么在一条 `*sql.Tx` 里）；但**跨进程/多实例/外部工具**打开同一个库文件时，所有「读-判-写」路径都会退化。仓库里 `stress_billing_test.go` 的 F4 用例（`stress_billing_test.go:1265`）只测了两个 Store **各跑各的**（`request_id` 带前缀避免撞号，`stress_billing_test.go:1299-1308`），**没有测两个 Store 竞争同一行**。

### 5.2 「写先行」原则

实测给出明确规律（`TestA8_TwoStoresWriteFirstVsReadThenWrite`）：

| 形态 | 例子 | 跨连接并发结果 |
|---|---|---|
| 写先行（第一条语句就是写） | `ChargeBalance`（`balance_dao.go:353-357` 的 `UPDATE ... RETURNING`） | 0 次 busy |
| 读先行（先 SELECT 再 UPDATE） | `ReserveQuota`（`key_dao.go:459-461` → `483-485`） | 7/20 次 busy，含 BUSY_SNAPSHOT(517) |

**建议**：把「事务的第一条语句必须是写」作为 `internal/store` 的**硬性约定**，并写进 `tx.go` 的包级注释（`tx.go:12-30` 现有的注释只讲了「同一份 SQL 既能在自动提交下跑，也能被包进事务」，没提这条）。

### 5.3 `tx.go` 的事务使用模式

`internal/store/tx.go`：

- `RunTx`（`tx.go:98-111`）：通用事务包装，**只被「批量导入」用**（`tx.go:88-97` 的注释）。
- `CreateProviderWithCredential`（`tx.go:126-140`）、`CreateRouteWithHeadTarget`（`tx.go:151-166`）、`UpdateRouteWithHeadTarget`（`tx.go:178-195`）：配置写入，**正确地把「多张表一起改」放进一条事务**。

**缺失的是资金路径上的事务**：

| 操作 | 当前 | 应在同事务的步骤 |
|---|---|---|
| 充值 + 结转 | 3 条自动提交（`topup_dao.go:202`/`225`/`243`） | 加余额、结转扣款、删账本、写流水 |
| 欠费记账 | 2 条（`balance_dao.go:511` rollback + `519` pending） | 写 pending 与返回错误 |
| 预占 | 1 条事务，但读先行（`key_dao.go:452-489`） | 改成写先行 |
| 落库 + 退预占 | 完全分离（`main.go:2853` 异步入队 + `main.go:2889` 同步退） | 见 P1-1 |

### 5.4 崩溃恢复视角

| 崩点 | 最终状态 | 可否对账 |
|---|---|---|
| 预占后、落库前 | `reserved_tokens` 永久偏高 → key 永久 429 | **不可**（P1-2） |
| 落库后、退预占前 | 预占泄漏（同上） | 不可 |
| 退预占后、落库前 | 额度被超发，缺口随时间自愈（used 最终会追上） | 事后可对账（但额度已被超发） |
| 落库后、扣费前 | 报表有、余额无 → 永久漏收 | **不可**（A4 P1-4 已报） |
| 扣费中（事务内） | 事务回滚，无残留 | 可 |
| 扣费后、流水写前 | 余额少了、流水没记 → 漏记 | **不可**（P2-1 实测） |
| 结算 UPDATE 后、DELETE 前 | 同一笔债被扣两次 | **不可**（P0-2） |
| 归档聚合后、删除前 | 同事务，无残留 | 可 |
| 归档删除后 | 表 A 有、表 B 由触发器已有 | 可 |
| `RecomputeCost` 后 | 表 A/B/余额三方不一致 | **不可**（P1-3） |

**没有一个崩溃点能保证「最终状态可对账」**，因为全仓**没有对账任务**（A4 已指出：`balance_charges` 只出现在 `balance_dao.go`/`store.go`/`topup_dao.go`/`balance_pk_migration.go`，无对账逻辑）。这是本子域的**根因级问题**：

> 没有对账任务 ⇒ 任何「两条语句之间崩溃」或「并发下读-判-写交错」产生的偏差都**永久存在且无人知晓**。

---

## 6. 压测/测试的真实性评估（测试绿但漏洞仍在）

### 6.1 实跑结果

```
$ CC=C:/mingw64/bin/gcc.exe go test -race -count=1 -timeout 30m ./internal/store/
ok  	github.com/cn-maul/rosetta-gateway/internal/store	240.419s
```

**全绿**。下面的每一条 P0/P1 都是在 race 下检不出来的。

### 6.2 逐条说明「为什么绿」

| 缺陷 | 为什么现有测试测不到 |
|---|---|
| **P0-1 重复结转** | `balance_pending_test.go` 的全部用例（`balance_pending_test.go:48`/`:67`/`:84`/`:105`/`:137`/`:154`）都是**单线程**的。没有一个用例并发调用 `AdjustBalanceWithLedger`。`stress_billing_test.go` 里有并发扣费（`stress_billing_test.go:176`）与并发充值，但**没有一个用例把「充值」与「欠费」放进同一个并发场景**。 |
| **P0-2 崩溃重复结转** | 需要「UPDATE 成功、DELETE 未执行」的中间态。测试无法注入这个状态（`settlePendingForUser` 走 `s.db` 自动提交，没有可注入的 hook）。 |
| **P0-3 不限额被切成有限额** | `balance_dao_test.go` / `balance_pending_test.go` 里没有「先欠费、再 SetBalance(unlimited)、再充值」的组合。`balance_dao.go:303-308` 的注释讨论了「不限额用户不扣」，但只覆盖了扣费侧，没覆盖结算侧。 |
| **P0-4 整表 DELETE 抹掉并发欠款** | 同 P0-1，需要并发。 |
| **P1-1 退预占早于落库** | `stress_quota_test.go` 的 D3 用例（`stress_quota_test.go:228`）在**同一个 goroutine 内**做 reserve → 落 usage → release，**刻意把顺序串行化**了，因此永远测不到「先 release 后落库」。真实网关的顺序恰好相反（`main.go:2889` 先 release，`main.go:2853` 的落库是异步的）。 |
| **P1-2 预占泄漏** | `stress_quota_test.go` 的 D2（`stress_quota_test.go:155`）测的是「配对释放不泄漏」，它**总是成对**调 reserve/release，从不模拟「进程死了」。`stress_billing_test.go` 的 F1（`stress_billing_test.go:976`）测的是「重开后已提交数据还在」—— 它**没有预留任何在途预占**。 |
| **P1-3 RecomputeCost 不补记表 B** | A6 已报。测试层面：`cost_pending_test.go` 只断言「明细行的 `cost_total` 被改正了」（`cost_pending_test.go:71-73`），**没有断言 `usage_totals` / `usage_daily_rollups` 也被同步**。 |
| **P1-4 cost_pending 被剪枝销毁** | `cost_pending_test.go` 的用例都在**剪枝之前**重算（`cost_pending_test.go:71`/`:126`/`:138`/`:162`），**没有「先剪枝再重算」的组合**。 |
| **P1-5 跨实例 busy** | `stress_billing_test.go` 的 F4（`stress_billing_test.go:1265`）用**两个 Store 并发扣费**，而扣费是**写先行**的 → 不会 BUSY。**没有任何用例让两个 Store 竞争同一行**（`ReserveQuota` 就是这种形态）。 |
| **P1-6 报表/余额口径差** | 所有扣费测试都只造 `status='ok'` 的记录，没有造 `status='error'` 的记录并与报表对比。 |
| **P1-7 UsageByUser TOCTOU** | `usage_by_user_dao_test.go` 的用例都是静态数据，没有并发写入。 |

### 6.3 压力测试的真实覆盖边界

- `stress_billing_test.go:95-108` 的 `runConcurrent` 用**门闩 + WaitGroup** 制造真实并发，不是串行 —— 这部分做得对。
- 但所有并发用例都**只测单进程单连接**（`testStore` 单实例）。`stress_billing_test.go:1265` 的 F4 虽是双 Store，却是「各写各的」。
- **失败注入**：全仓**没有** crash-point 注入（没有在两条语句之间 kill 进程的用例）。`stress_billing_test.go:976` 的 F1 只测「关闭再打开后已提交数据还在」—— 这测的是**持久性**，不是**崩溃一致性**。
- `-race` 覆盖：`go test -race` 在 CI 里是**全仓**跑的（`.github/workflows/ci.yml:161`），所以 race 层面没有漏。但 race 检测的是 Go 内存数据竞争，与本子域的缺陷类型正交。

**建议补的用例**（按性价比排序）：

1. 并发 `AdjustBalanceWithLedger` × N + 存在 `balance_pending` → 断言「余额 == 期望值」（钉住 P0-1/P0-4）。
2. 「先 release 预占、再落 usage」的两步序列 → 断言额度不会被超发（钉住 P1-1）。
3. 「预占后不释放，直接 Close + 重开」→ 断言 `reserved_tokens == 0`（钉住 P1-2）。
4. 双 Store 竞争**同一行**的 `ReserveQuota` → 断言 0 次 busy（钉住 P1-5）。
5. 「剪枝前把记录标成 `cost_pending`」→ 断言归档表记下了「待重算」（钉住 P1-4）。

---

## 7. 已验证无问题的区域（含理由）

1. **`ChargeBalance` 的扣费原子性**（`balance_dao.go:414-435`）
   扣费是**一条 UPDATE**，同时带余额守卫（`balance_cents >= ?`）与下界夹住（`MAX(0, …)`），判余额与扣减在同一条语句里。`balance_dao.go:414-429` 的论证成立。实测：跨连接并发下 0 次 busy、20/20 全部成功（`TestA8_TwoStoresWriteFirstVsReadThenWrite`），且 `stress_billing_test.go:220`（恰好 M 次成功）、`:283`（混合金额三方闭环）在 race 下全绿。

2. **扣费的幂等顺序**（`balance_dao.go:324-341`）
   「先占位、再累加余数、再扣费」在同一事务内，失败时 `defer tx.Rollback()` 把占位行一起撤销。实测（`balance_charge_retry_test.go:33`/`:52`/`:88` 全绿）确认：失败路径不留占位行、用户不存在不留占位行、同用户重试只扣一次、跨用户同 request_id 各自都扣。**我另行验证的并发版本**（`TestA8_ConcurrentSameRequestIDInsufficientLosesDebt`，40 轮并发同 request_id 欠费）**没有**出现欠款丢失（0/40）。

3. **欠费分支的回滚顺序**（`balance_dao.go:502-517`）
   「必须先显式 `tx.Rollback()` 再调 `recordPendingRemainder`」的论证正确：写池 `SetMaxOpenConns(1)`，本事务独占唯一连接，不先回滚就是自己等自己 → 死锁。这条注释救了一个真实的挂死。

4. **余数机制的精度**（`balance_dao.go:352-401`）
   用 `UPDATE ... RETURNING balance_remainder` 把「累加 + 读回」压进一条语句，避免 check-then-act；不足一分时提交（`balance_dao.go:391-393`），够一分时整除扣减、余数留账（`balance_dao.go:400-401`）。实测：`TestStress_RemainderAccumulationIsExact`（10000 × 0.0001 元 → 恰好 10 分、余数 0）、`TestStress_TinyAmountsAreNotLost`（10000 × 1e-7 元 → 恰好 1 分）在 race 下全绿。`YuanToMicros` 的「亚微元至少记 1」的单向偏差（`balance_dao.go:86-109`）已被 `stress_billing_test.go:563` 明确记录并量化（10 倍放大），**属于已知取舍，不是缺陷**。

5. **归档的「圈批 → 聚合 → 删除」同事务不变式**（`usage_archive.go:546-740`）
   `prune_batch` 临时表（`usage_archive.go:585-618`）保证聚合（`:656`）与删除（`:681`）谓词严格相同，同事务（`:546`）提交，水位只在「cutoff 前已无残留」时推进（`:725-736`）。实测（`TestA8_PruneConcurrentInsertRace`）：并发插入一条老明细 + 并发剪枝，`detail + rollup == 797` **恰好**（无丢失、无重复）。这条不变式是本仓库里做得最扎实的一块。

6. **归档不回收累加列**（`usage_archive.go:687-710`）
   刻意不按被剪量扣回 `user_usage_totals` / `access_keys.used_tokens`，理由（终身累计 vs 保留期用量）正确，且注释明确记录了 2026-10-11 那两条空跑 UPDATE 的历史，防止后人「顺手修好」。

7. **`trg_update_user_usage_totals` 的空 user_id 守卫**（`usage_archive.go:188-204`）
   `SELECT ... WHERE NEW.user_id <> ''` + `ON CONFLICT DO UPDATE` 避免了 NULL 进主键导致「ON CONFLICT 永不匹配 → 每次归档多插一行」的静默双计。正确。

8. **两个触发器的 DROP+CREATE 在同一事务**（`usage_archive.go:178-211`、`:218-251`）
   避免了「DROP 成功、CREATE 失败 → 触发器消失 → 累计列停止累加 → 配额被低估 → 用户可超额使用」的窗口。这个窗口的论证是本仓库里对事务边界最清醒的一处。

9. **`GetUsageLifetime` 不接受 `scopeUserID`**（`usage_archive.go:764-768`）
   刻意少一个可传错的入参，避免普通用户拿到全局数字。正确。

10. **`RecomputeUsedTokens` 的并发安全**（`key_dao.go:375-412`）
    SELECT 与 UPDATE 在**同一条写池事务**里（`key_dao.go:376`、`409`），不存在跨事务读。实测（`TestA8_RecomputeUsedTokensLostUpdate`，40 轮「重算 + 并发插入」）：`used_tokens == SUM(details) == 82000`，**没有丢失更新**。因为写池单连接 + 事务，插入要么在事务前（被 SUM 看到）、要么在事务后（触发器在 UPDATE 之后累加）—— 两种情况都正确。

11. **`users.used_tokens` 只是展示字段**（`store.go:404-405`、`user_dao.go:634-637`）
    配额判定读 `user_usage_totals`（`user_dao.go:499`、`:579`），展示读 `SumUserUsedTokens`（`user_dao.go:496`）—— 口径一致。`internal/admin/user_admin_handler.go:190-194` 也用 `SumUserUsedTokens` 并在出错时回退。**但** `toUserResponse`（`user_admin_handler.go:96`、`:117`）在 `AdjustBalance` 路径直接用 `u.UsedTokens`（`store.User` 的展示列）—— 该列只在 `backfillUserUsedTokens`（一次性）与 `RecomputeUserUsage`（**无生产调用点**，`rg` 确认只有 `user_dao_test.go` 调用）里更新，因此**充值响应里的 `used_tokens` 可能是陈旧的**。这与 A6 的 P1-3（孤儿 user_id）不同，属**待确认**的展示层问题，不影响配额判定。

12. **`probe_dao.go` 与资金完全隔离**
    `model_probes` 是独立表（`store.go:581`），`RecordProbe`（`probe_dao.go:26-41`）不写任何用量/余额表，`UsageSource` 不包含它。探测**绝不污染计费**这条红线成立。（`ProbeHealthSummary` 的几个相关子查询有 N+1 形态（`probe_dao.go:82-93`），但那是性能问题，不是资金问题。）

13. **余额预检的 fail-closed 方向**（`cmd/gateway/billing.go:698-707`）
    余额读失败 → 拒绝（500），而不是放行。理由（`billing.go:651-656`）成立：fail-closed 的代价是可用性损失（自愈），fail-open 的代价是资金损失（不自愈）。**与配额预检的 fail-open 方向相反**，这是一个有意的、论证充分的不对称。

14. **单进程单连接下写路径的串行性**
    `db.SetMaxOpenConns(1)`（`store.go:43`）使所有写排队，本进程内不存在写-写竞态。上面所有 P0/P1 的复现都**需要跨连接**（多实例 / 外部工具 / 读写池混用），因此它们在「标准单实例部署」下不触发 —— 但 `store.go:28-31` 的注释明确说这套 DSN 就是为「误起的第二个实例」准备的，所以这不是「不会发生」，是「会发生但没被处理」。

---

## 8. 完整问题清单

### P0

| 编号 | 位置 | 摘要 |
|---|---|---|
| **P0-1** | `internal/store/balance_dao.go:564-589`、`internal/store/topup_dao.go:225` | `settlePendingForUser` 在自动提交模式下做「SUM → UPDATE → 整表 DELETE」三段式；并发充值时同一笔 `balance_pending` 被**重复结转**（多收用户钱）。实测复现率 59/60。 |
| **P0-2** | `internal/store/balance_dao.go:577-587` | 结算的 UPDATE 与删账本的 DELETE 之间是两条自动提交语句；进程在中间被杀 → 下一笔充值把**同一笔债再扣一次**。实测：余额 109400 vs 期望 109700。 |
| **P0-3** | `internal/store/balance_dao.go:578-582` | 结算的 `COALESCE(balance_cents,0)` 把 NULL（= 不限额）当成 0，**把无限额用户静默切成有限额**。实测：`limited` 由 false 变 true。 |
| **P0-4** | `internal/store/balance_dao.go:585` | `DELETE FROM balance_pending WHERE user_id = ?` 删的是该用户**全部**行而非本次 SUM 到的行 → 并发写入的新欠款被**静默抹掉**（漏收）。实测复现率 60/60。 |

### P1

| 编号 | 位置 | 摘要 |
|---|---|---|
| **P1-1** | `cmd/gateway/main.go:2853`+`:2889`、`internal/store/key_dao.go:445-491` | 配额**先退后记账**（退预占同步、usage 落库异步）→ 窗口内该请求既不在 `reserved` 也不在 `used` → 终身配额超发。实测：quota 1000 放行 2000 token。 |
| **P1-2** | `internal/store/key_dao.go:507-513`、`internal/store/user_dao.go:617-623` | `reserved_tokens` 是持久化的**在途**状态，进程崩溃后**永久冻结**（全仓无复位语句）→ key/用户永久 429。实测：重开后 `reserved_tokens=900`。 |
| **P1-3** | `internal/store/usage_dao.go:489-521`、`internal/store/usage_archive.go:227-249` | `RecomputeCost` 改价后**不补记表 B**（触发器只挂 AFTER INSERT）→ 归档后**表 A 有 0.02 元、表 B 有 0 元**。实测：`rollup=0.020000 lifetime=0.000000`。 |
| **P1-4** | `internal/store/usage_archive.go:630-672`、`:681` | 归档表**无 `usage_state` 列** → `cost_pending` 标记随明细删除被销毁 → 待重算费用**永久归零**。实测：`pending rows left=0`。 |
| **P1-5** | `internal/store/key_dao.go:452-489`、`internal/store/user_dao.go:552-604` | 预占事务「**读先行**」→ 跨连接时升级写事务返回 `SQLITE_BUSY_SNAPSHOT`(517)，`_busy_timeout` **对它无效** → 预占失败 → `main.go:1849` fail-open → 多实例下配额静默失效。实测：8/20 次 busy。 |
| **P1-6** | `internal/store/usage_by_user_dao.go:101` vs `cmd/gateway/main.go:3384-3386` | 报表对**所有** status 求和 `cost_total`，扣费**只对 `status='ok'`** → 「报表显示花了 X、余额扣了 Y」在结构上**可以**发生（`balance_dao.go:12-15` 的断言不成立）。实测：报表 0.04 元 vs 扣费 0.02 元。 |
| **P1-7** | `internal/store/usage_by_user_dao.go:83-85`、`:114` | 「总用户数」与「聚合行数」是**两条独立的读池查询** → 不同快照 → 「前 N / 共 M 位用户」在并发写入下会说谎。实测：16/50 轮不一致。 |

### P2

| 编号 | 位置 | 摘要 |
|---|---|---|
| **P2-1** | `internal/store/balance_dao.go:497-527`、`:544` | 欠费路径是「rollback → 独立事务写 pending → 返回错误」三步非原子；空 `request_id` 时**既不记占位也不记欠款**（余额扣了但流水查不到 / 欠款蒸发）。 |
| **P2-2** | `internal/store/topup_dao.go:201-213`、`:225` | `balance_topups.balance_after` 读的是**结转之前**的余额 → 与 `users.balance_cents` 差一个结转额 → 审计链的锚点错了。实测：10001 vs 9701。 |
| **P2-3** | `internal/store/recent_performance_dao.go:24-38` | 直接读 `usage_records` 而非 `UsageSource` → 剪枝后「近期表现」样本**全部归零**（同页面其它数字仍在）。实测：samples 2 → 0。 |

### P3

| 编号 | 位置 | 摘要 |
|---|---|---|
| **P3-1** | `internal/store/store.go:473-479` | 「request_id 来源于客户端可控的请求头」的注释与实现（服务端 `crypto/rand` 生成，`internal/server/server.go:29-33`）不符；若将来允许客户端指定，该 8 字节 request_id 的碰撞概率不足以支撑幂等扣费。 |

---

## 9. 建议的修复顺序（按「资金正确性 × 复现概率」排序）

1. **P0-1 + P0-2 + P0-4**（同一个根因）→ 把 `settlePendingForUser` 改成「一条 `BEGIN IMMEDIATE` 事务 + 条件 DELETE（限定 request_id）+ 影响行数自校验」。
2. **P0-3** → 结算路径必须尊重 `balance_cents IS NULL = 不限额` 语义。
3. **P1-5** → 预占改成「写先行」（`UPDATE ... WHERE used+reserved+est <= quota` 一条语句），或用 `BEGIN IMMEDIATE`。
4. **P1-1 + P1-2**（同一个根因：预占是「在途状态」却按「已发生事实」管理）→ 引入「落库与退预占同事务」或第三列 `inflight_tokens`；并在启动时复位所有在途列。
5. **P1-3 + P1-4** → 归档表增加 `pending_cost_count`；给 `usage_records` 加 AFTER UPDATE 触发器补记表 B。
6. **P1-6 + P1-7** → 明确报表口径并把「总额 + 扣款额」并列展示；聚合 DAO 的两条查询合并成一条。
7. **P2-1 + P2-2 + P2-3** → 欠费记账进同一事务；结转移到 `RETURNING` 之前；`GetRecentPerformance` 改走 `UsageSource`。
8. **架构级**：引入**对账任务**（扫 `status='ok' AND cost_total>0` 但 `balance_charges` 无对应行的记录；扫 `balance_pending` 与 `balance_charges` 的对账；扫 `usage_totals` 与 `SUM(明细+归档)` 的对账）。没有对账，上面任何一条残留偏差都**永久存在且无人知晓**。

---

*本报告为只读审计产出。探针测试文件存放在仓库外的 `a8probe\`，通过 `go test -overlay` 虚拟映射进 `internal/store` 包运行，未在仓库内留下任何文件。*
