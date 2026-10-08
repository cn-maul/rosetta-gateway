# 并发压测与账目完整性验证（billing）

被测对象：`C:\Users\louis\Desktop\project\rosetta-gateway`（LLM API 网关），
工作区 `d529b29` + 大量未提交改动。

新增测试文件（**未改任何生产代码**）：

- `internal/store/stress_billing_test.go` —— 场景 A / B / C / F
- `internal/store/stress_quota_test.go` —— 场景 D / E

共 **24** 个用例。每个场景一节，均附**实际执行的命令 + 原始输出**。

> **先说结论：没有发现金额对不上的缺陷。** 所有并发形态下账目都精确闭合，
> 「差一分」级别的问题一个也没有出现。发现 2 个真实缺陷，都不在「算错钱」
> 这一类，而是**记账边缘语义**问题（B4）与**单向舍入偏差**（B5），详见第 8 节。

---

## 0. 环境：`-race` 居然能跑（本报告的加分项）

任务说明里写「`-race` 在本机跑不了：MinGW 链接器处理不了
`C:\Program Files\mingw64\bin` 里的空格」。这一点**可以绕过**。

`PATH` 里确实有一个带空格的 MinGW，但**同一台机器上还有一个不带空格路径的
gcc**：

```
C:\mingw64\bin\gcc.exe
```

把 `CC` 显式指到它即可：

```powershell
$env:CGO_ENABLED="1"; $env:CC="C:/mingw64/bin/gcc.exe"
go test -race ./internal/store/ ...
```

### 证明 race 运行时**真的生效**（不是静默跳过）

仅仅「命令没报错」不足以证明 `-race` 在工作 —— 它可能只是被忽略了。做了两道验证：

**（1）二进制里链入了 TSan 运行时符号**

```
$ go test -race -c -o probe.exe ./internal/store/
$ go tool nm probe.exe | Select-String "__tsan"
141008dec R .pdata$_ZN11__sanitizer24InternalMmapVectorNoCtorIN6__tsan16FiredSuppressionEE7ReallocEy
141008b88 R .pdata$_ZN11__sanitizer24InternalMmapVectorNoCtorIN6__tsan16FiredSuppressionEE9push_backERKS2_
141008c30 R .pdata$_ZN11__sanitizer6VectorIN6__tsan10RacyStacksEE8PushBackERKS2_
141008af8 R .pdata$_ZN6__tsan12MemoryAccessEPNS_11ThreadStateEyyyy
```

**（2）阳性对照：故意制造的数据竞争必须被抓到**

临时写了一个对共享 `int` 无锁自增的用例（验证后已删除），
`go test -race` 如实报告：

```
==================
WARNING: DATA RACE
Read at 0x00c00008e558 by goroutine 11:
  github.com/cn-maul/rosetta-gateway/internal/store.TestRaceDetectorPositiveControl.func1()
      .../zz_race_positive_control_test.go:19 +0x99

Previous write at 0x00c00008e558 by goroutine 10:
  .../zz_race_positive_control_test.go:19 +0xab
```

两道验证都通过 ⇒ `-race` 的真实性已确认，下文所有 `-race` 结论可信。

### `-race` 下的实测结果（全部并发用例）

```
$ CGO_ENABLED=1 CC=C:/mingw64/bin/gcc.exe go test -race ./internal/store/ `
    -run "TestStress_Concurrent|TestStress_Idempotency|TestStress_ReserveQuota|TestStress_FullLifecycle|TestStress_Persistence_NoDatabaseLocked|TestStress_Persistence_TwoStores|TestStress_Persistence_PruneWhileCharging" -count=5

ok  	github.com/cn-maul/rosetta-gateway/internal/store	71.621s
```

**0 个数据竞争。** 另有一次覆盖全部 `TestStress` 的 `-count=3`：

```
ok  	github.com/cn-maul/rosetta-gateway/internal/store	205.762s
```

---

## A. 并发扣费

### A1 · N goroutine 同扣一个用户，总额精确

**测什么**：200 个 goroutine 同时扣同一个用户、各 1 分、`request_id` 互不相同，
余额给足（1000 元）。断言三条**同时**成立：成功次数 == 200、余额 == 初始 − 200 分、
流水累计 == 200 分。另加账目恒等式 `初始 − 累计扣费 == 当前余额`。

**怎么构造**：`sync.WaitGroup` + `start` 门闩让 200 个 goroutine 尽量同时开跑
（不用门闩的话第一个可能已经跑完，测试退化成串行）。

**命令与输出**

```
$ go test ./internal/store/ -run TestStress_ConcurrentCharge_TotalIsExact -count=1 -v
=== RUN   TestStress_ConcurrentCharge_TotalIsExact
--- PASS: TestStress_ConcurrentCharge_TotalIsExact (0.12s)
```

**结论**：精确。余额、流水、恒等式三者一致，无超扣、无为负。

### A2 · 余额刚好够 M 次 → **恰好** M 次成功

**测什么**：余额恰好 50 分，150 个 goroutine 各扣 1 分。断言**恰好** 50 次成功
（不是 `<= 50`）、余额**正好归零**、`amount_cents > 0` 的流水行数**恰好 50**。

这是「不得超扣」最锋利的形态：M 与 N 有明确差值，任何超扣都会让成功数 > 50。

```
$ go test ./internal/store/ -run TestStress_ConcurrentCharge_ExactlyMBecauseBalanceIsM -count=1 -v
=== RUN   TestStress_ConcurrentCharge_ExactlyMBecauseBalanceIsM
--- PASS: TestStress_ConcurrentCharge_ExactlyMBecauseBalanceIsM (0.10s)
```

**结论**：**恰好 50 次**，余额正好 0，无负值。`ChargeBalance` 里
`WHERE ... balance_cents >= ?` 的单语句守卫在 150 路并发下没有漏过一次。

### A3 · 混合金额（1/2/3 分）不为负且闭环

余额 100 分，200 个 goroutine 轮扣 1/2/3 分。三方差闭环
`初始 − 成功金额之和 == 余额 == 初始 − 流水累计`。

```
$ go test ./internal/store/ -run TestStress_ConcurrentCharge_MixedAmountsStayExactAndNonNegative -count=1 -v
--- PASS: TestStress_ConcurrentCharge_MixedAmountsStayExactAndNonNegative (0.12s)
```

### A4 · **完整计费生命周期**端到端并发（本组最重要）

**测什么**：100 个并发请求跑真实链路：落 usage → 触发器累加 `used_tokens` →
读回固化 `cost` → 按该 cost 扣余额。逐个请求的 token 数**故意不同**。

**这条用例的初版断言是错的，而失败本身就是一条发现**，值得记下来：

初版按「逐条 `YuanToCents(cost)` 求和 == 实际扣的分」断言，实测**直接失败**：

```
    stress_billing_test.go:881: 余额减少 = 7 分, want 0（差 7）
    stress_billing_test.go:886: 流水累计 = 7 分, want 0
```

**原因**：该 fixture 下每次费用只有约 600 **微元**（0.006 分），逐条
`YuanToCents` 全部舍成 0；而实际收上了 7 分。这**反证**了真实链路走的是
**微元累加 + 满一分才扣**（`ChargeBalance` 的余数机制），而不是
「每条各自舍入到分再相加」—— 后者在低价模型下会把**每一笔**都舍成 0，
正是余数机制要解决的那个 2026-10-10 缺陷的原形。

修正后的正确恒等式在**微元**层面成立（更锋利：差 1 微元 = 1e-6 元即失败）：

```
$ go test ./internal/store/ -run TestStress_FullLifecycle -count=5 -v
--- PASS: TestStress_FullLifecycle_ChargeMatchesFrozenCostExactly (0.08s)
--- PASS: TestStress_FullLifecycle_ChargeMatchesFrozenCostExactly (0.07s)
--- PASS: TestStress_FullLifecycle_ChargeMatchesFrozenCostExactly (0.07s)
--- PASS: TestStress_FullLifecycle_ChargeMatchesFrozenCostExactly (0.07s)
--- PASS: TestStress_FullLifecycle_ChargeMatchesFrozenCostExactly (0.07s)
```

5 条闭包全部成立：
1. 余额减少 == `floor(Σ微元 / 10000)`；
2. 流水累计 == 同一数字；
3. **余数 == `Σ微元 mod 10000`**（精确到微元）；
4. **从库里读回** `cost_total` 重算的微元总和 == 扣费侧用的总数；
5. 触发器累加的 `used_tokens` == 各请求 token 数之和。

**结论**：真实链路（计价 → 固化 → 换算 → 扣费 → 触发器）在并发下**完全闭合**。

---

## B. 余数机制精确性

### B1 · 大量不足一分的扣费累加精确

10 000 次 × 0.0001 元（100 微元），总额恰为 1 元 = 100 分（整数结果，
任何精度损失都会让它不精确）。

```
$ go test ./internal/store/ -run TestStress_RemainderAccumulationIsExact -count=1 -v
--- PASS: TestStress_RemainderAccumulationIsExact (2.93s)
```

余额恰好 −100 分，余数恰好 0，流水累计恰好 100 分。

### B2 · 极端小金额（0.0000001 元，亚微元）不丢

10 000 次 × 0.1 微元 = 1000 微元，本应只攒不扣。

```
$ go test ./internal/store/ -run TestStress_TinyAmountsAreNotLost -count=1 -v
--- PASS: TestStress_TinyAmountsAreNotLost (2.85s)
```

### B3 · **并发下余数不丢**（单向 UPDATE 的关键验证）

200 个 goroutine 各扣 0.0003 元（300 微元），总 60 000 微元 = 6 分。
若 `balance_remainder` 的累加被写成「SELECT 再 UPDATE」，并发就会互相覆盖
（丢失更新），表现是**少收钱**。

```
$ go test ./internal/store/ -run TestStress_ConcurrentRemainder_NoLostUpdates -count=1 -v
--- PASS: TestStress_ConcurrentRemainder_NoLostUpdates (0.11s)
```

余额恰好 = 初始 − 6 分，余数恰好 == `60000 mod 10000` = 0。
`UPDATE ... SET balance_remainder = balance_remainder + ? ... RETURNING` 的
单语句写法确实免疫丢失更新。

### B4 · 【缺陷 1】余额不足时，本次已累加的余数被回滚丢弃，而真实调用方不重试

**现象**（实测日志）

```
$ go test ./internal/store/ -run TestStress_RemainderDiscardedOnInsufficientBalance -count=1 -v
=== RUN   TestStress_RemainderDiscardedOnInsufficientBalance
    stress_billing_test.go:520: 缺陷确认: 本次费用 0.02 元（20000 微元）随
    ErrInsufficientBalance 的回滚被**永久丢弃**；真实调用方不重试，
    故这笔钱收不回来（上限 < 1 分/次）
--- PASS: TestStress_RemainderDiscardedOnInsufficientBalance (0.16s)
```

**复现**：余额 0 的用户，调用 `ChargeBalance(ctx, "u1", 0.02, "req")`。
该调用会先把 20000 微元累加进 `balance_remainder`，跨过一分后尝试扣 2 分，
被余额守卫拦下 → 返回 `ErrInsufficientBalance` → 事务**整体回滚** →
那 20000 微元随回滚消失（余数读到 0）。

**根因**：`balance_dao.go` 里失败路径改为 `return ErrInsufficientBalance`
（这是 2026-10-10 修 P0「失败路径误提交占位行」的**正确**改动），
但回滚连带撤销了同一事务里前面那次 `balance_remainder` 累加。
代码注释对此**知情**，它给出的理由是：

> 「回滚会一并撤销上面那条『余数累加』，所以注释里说的『余数照留』
> 由**调用方重试时重新累加**实现」

**但这个前提在代码里不成立。** 真实调用方是
`cmd/gateway/main.go` 的 `usageRecorder.charge`，它收到
`ErrInsufficientBalance` 时**只记一条 ERROR 日志然后放弃**，不重试。
其自身的注释写的是「预检在前，正常路径走不到这里……钱收不回，
『报欠费』比『悄悄放过』更诚实」。

也就是说「调用方重试时重新累加」这条路径**不存在** —— 余额不足时那一笔
费用中留在余数里的部分**永久漏收，且无人对账发现**。

**严重度**：P2。金额上限明确为 **< 1 分/次**（真正 ≥ 1 分的部分属于**设计
意图**：`balance_dao.go` 明确选择「不扣 + 报欠费」而非「扣一部分 + 报欠费」，
那个取舍本身是合理的）。所以这不是「差一分」的资金 bug，而是
**设计注释与实现不一致**：注释声称余数会保住，实际没保住。
触发条件是「余额不足」，即用户已经欠费 —— 影响面小，但它是**静默**的
（既不报错也不进流水）。

**为什么没修**：按任务要求「发现 bug 写进报告，不要顺手修」。
修法有两条且需产品决策：(a) 失败时把余数单独提交（但需处理「重试时二次累加」）；
(b) 修正注释与文档，承认这部分漏收并把它计入「欠费时的漏收上限」。
(a) 会改变事务语义、可能引入重复累加（正是 P0-2 那类风险），不应由压测任务擅自决定。

### B5 · 【缺陷 2】`YuanToMicros` 的亚微元兜底使误差**单向偏大**，随调用量线性累积

**现象**（实测日志）

```
$ go test ./internal/store/ -run TestStress_SubMicroFeesRoundUpSystematically -count=1 -v
=== RUN   TestStress_SubMicroFeesRoundUpSystematically
    stress_billing_test.go:595: 偏差确认: 10000 次 × 1e-07 元 —— 真实费用
    1000 微元（0.1 分），实扣 1 分（= 10000 微元），**放大 10 倍**
    （误差单向偏大，随调用次数线性累积）
--- PASS: TestStress_SubMicroFeesRoundUpSystematically (2.90s)
```

**复现**：连续 10 000 次 `ChargeBalance(ctx, u, 0.0000001, ...)`。
每次真实费用 0.1 微元，被计入 1 微元 → 实扣 10 000 微元 = 1 分；
真实应付仅 1000 微元 = 0.1 分。**多收 10 倍**。

**根因**：`YuanToMicros` 对 `micros < 1` 的正费用一律向上记 1 微元：

```go
if micros < 1 {
    // 极端小的正费用（低于 0.5 微元）也至少记 1 微元 ——
    // 记 0 等于把这次消费丢掉，而余数机制的意义正是「不丢」。
    return 1
}
```

**与 `YuanToCents` 的关键差别**：`YuanToCents` 的四舍五入误差是**零均值**的
（其注释正是以此论证「四舍五入优于截断」）。`YuanToMicros` **不是** ——
它单向向上，偏差随调用次数**线性累积**，不会相互抵消。

**严重度**：P2。方向是「宁可多记不可丢」，作为单次决策无可指摘；
但要量化：**在费用普遍低于 0.5 微元的形态下（极低价模型 / 极短请求），
用户被多收的倍数等于 `1 / 真实微元数`**，本用例是 10 倍。
它不是「差一分」的噪声，而是一个与调用量成正比的**倍数级系统偏差**。

**为什么没修**：修法需要产品口径决策（是接受向上兜底、还是把
`balance_remainder` 的粒度再细化、还是对亚微元费用做上限保护），
且 `YuanToMicros` 是被 `ChargeBalance` 直接调用的核心换算，
擅自改动会波及全部计费路径。

**注**：这两个用例的断言刻意写成「实际行为」（因此永远绿），
把缺陷固化成可执行的证据 + 日志里的确切数字，而不是写成会红的断言 ——
因为任务要求「发现 bug 只报告、不修」，红了会让 CI 失败却无人能修。

---

## C. 幂等

### C1 · 同 `request_id` 并发重试 50 次 → 流水 1 行、只扣 1 次

```
$ go test ./internal/store/ -run TestStress_Idempotency_SameRequestIDConcurrent -count=1 -v
--- PASS: TestStress_Idempotency_SameRequestIDConcurrent (0.06s)
```

流水行数**恰好 1**，余额恰好 −10 分，流水累计恰好 10 分。
（串行重试只验证「第二次读到已存在的占位」；并发才暴露
「两个事务同时 SELECT 到没有占位、都 INSERT」的竞态，复合主键挡住了。）

### C2 · **不同用户撞同一 `request_id` → 两人都扣**（修过的 P0 回归）

```
$ go test ./internal/store/ -run TestStress_Idempotency_DifferentUsersSameRequestIDBothCharged -count=1 -v
--- PASS: TestStress_Idempotency_DifferentUsersSameRequestIDBothCharged (0.05s)
```

alice 扣 7 分、bob 扣 13 分，两人余额各自精确减少、各自 1 行流水。
复合主键 `(request_id, user_id)` 生效，**跨租户漏收的 P0 未回归**。

### C3 · **扣费失败后重试 → 钱要能扣到**（修过的 P0 回归）

分三阶段：余额不足 → 并发重试 20 次全部 `ErrInsufficientBalance` →
充值 → 用**同一 `request_id`** 重试。

```
$ go test ./internal/store/ -run TestStress_Idempotency_RetryAfterInsufficientEventuallyCharges -count=1 -v
--- PASS: TestStress_Idempotency_RetryAfterInsufficientEventuallyCharges (0.09s)
```

关键断言（修复前会红的正是这条）：
- 阶段 1 失败后，`amount_cents > 0` 的流水累计为 **0**（占位未被误提交）；
- 阶段 3 重试**真的扣到了钱**（余额精确减少 500 分）。

**「失败路径误提交占位行」的 P0 在并发形态下依然被修复。**

---

## D. 配额预占

### D1 · 并发 `ReserveQuota` 不超总额

quota = 1000，200 个 goroutine 各预占 100。断言**恰好 10 次**放行、
放行预占总量 == 1000、`reserved_tokens` == 1000，且 `used_tokens` 仍为 0
（预占**绝不能**写 used —— 那正是 P0-2）。

```
$ go test ./internal/store/ -run TestStress_ReserveQuota_NeverExceedsTotal -count=1 -v
--- PASS: TestStress_ReserveQuota_NeverExceedsTotal (0.04s)
```

### D2 · **泄漏检测**：混合收尾后 `reserved_tokens` 归零

三种形态：reserve→release 配对 50 轮、超额预占被拒、100 路并发 reserve/release 配对。

```
$ go test ./internal/store/ -run TestStress_ReserveQuota_NoLeakAfterMixedOutcomes -count=1 -v
--- PASS: TestStress_ReserveQuota_NoLeakAfterMixedOutcomes (0.06s)
```

`reserved_tokens` 最终 **0** —— 无泄漏。（泄漏的后果是这把 key 永久 429，
且界面上看不出异常，所以这条必须精确。）

### D3 · 净记账 == 真实用量（**不是 2 倍**，P0-2 回归）

100 个请求各自 reserve(500) → 落 usage(total=300) → release。
断言 `used_tokens` **恰好** == 100 × 300 = 30000（若是 2 倍记账则为 60000），
且 `reserved_tokens` 归零。

```
$ go test ./internal/store/ -run TestStress_ReserveQuota_NetAccountingEqualsActualUsage -count=1 -v
--- PASS: TestStress_ReserveQuota_NetAccountingEqualsActualUsage (0.16s)
```

---

## E. 归档剪枝的账目一致性

### E1 · 跨多天用量 → 剪枝 → 总额**不变**

`archiveFixture` 造跨 3 组维度 × 2 provider × 3 状态 × 多天的数据（维度差异被
刻意设计成「漏一维就能看出来」）。

```
$ go test ./internal/store/ -run TestStress_Prune_StatsIdenticalBeforeAndAfter -count=1 -v
--- PASS: TestStress_Prune_StatsIdenticalBeforeAndAfter (0.16s)
```

剪枝删除行数 == fixture 声称的窗口外行数，且 `GetUsageStats`（全部历史口径）
**逐字段相等**（复用 `usage_archive_test.go` 的 `assertStatsEqual`，
覆盖 TotalRequests/TotalTokens/Input/Output/Cached/ErrorCount/CacheHitRate/Cost）。

### E2 · 反复剪枝**幂等**

```
$ go test ./internal/store/ -run TestStress_Prune_RepeatedPruneIsIdempotent -count=1 -v
--- PASS: TestStress_Prune_RepeatedPruneIsIdempotent (0.27s)
```

第 2、3 次剪枝 `DeletedRows == 0`、`RollupRows == 0`，统计与第 1 次**完全相等**，
且表 B 的 `total_tokens`/`request_count` 与统计一致。

### E3 · 剪枝**不得**污染终身累计（表 B）

显式核对表 B 的 10 个整数字段 + `cost_total` 在剪枝前后**逐字段相同**，
并断言 `pruned_through_day` 确实推进（否则「不变」只是因为没剪）。

```
$ go test ./internal/store/ -run TestStress_Prune_LifetimeTotalsSurvivePrune -count=1 -v
--- PASS: TestStress_Prune_LifetimeTotalsSurvivePrune (0.19s)
```

---

## F. 持久化健壮性

### F1 · 不 Close 直接重开 → 已提交数据完整

旧句柄**仍打开着**的同时重开同一个库（等价于「进程被杀，文件锁还挂着」）。
50 次扣费后重开，余额与流水累计精确，且重开后仍能继续扣费。

```
$ go test ./internal/store/ -run TestStress_Persistence_ReopenWithoutCloseKeepsCommittedData -count=3 -v
--- PASS: TestStress_Persistence_ReopenWithoutCloseKeepsCommittedData (0.07s)
```

> 测试自身的坑（已在代码注释里记下）：Windows 上未释放的文件句柄会让
> `t.TempDir()` 清理失败（实测 `The process cannot access the file because
> it is being used by another process`）。解法是用 `t.Cleanup` 在**验证之后**
> 关闭句柄 —— 这不削弱用例，因为重开与全部校验都发生在旧句柄仍打开时。

### F2 · 20 写 + 20 读并发，**0 次** `database is locked`

```
$ go test ./internal/store/ -run TestStress_Persistence_NoDatabaseLockedUnderConcurrentReadWrite -count=1 -v
--- PASS: TestStress_Persistence_NoDatabaseLockedUnderConcurrentReadWrite (0.26s)
```

500 次写 + 1000 次读（读池）并发，**0 次** locked/busy，且账目精确。

### F3 · **长写事务**（剪枝）与扣费共存 → 0 次 locked

剪枝全程持写锁（写池单连接 ⇒ 期间所有写入排队）。2000 行明细 +
10 路并发扣费 + 读余额。

```
$ go test ./internal/store/ -run TestStress_Persistence_PruneWhileCharging -count=3 -v
--- PASS: TestStress_Persistence_PruneWhileChargingNoLocked (0.71s)
    stress_billing_test.go:1243: 剪枝删除 2000 行（长写事务确实发生），期间 200 次扣费、0 次 locked
```

用例里有一条**防空转**断言（`prunedRows == 0` 直接 `t.Fatal`），
确保「没报 locked」不是因为压根没发生长写事务。

### F4 · **两个 Store 打开同一文件**并发扣费，账目不丢

覆盖 `SetMaxOpenConns(1)` 挡不住的场景（备份脚本 / CLI / 误起的第二实例）。
两个独立连接池各扣 50 次，总额精确。

```
$ go test ./internal/store/ -run TestStress_Persistence_TwoStores -count=3 -v
--- PASS: TestStress_Persistence_TwoStoresSameFileNoLostMoney (0.06s)
```

100 次扣费全部成功，两个 Store 各自读到的余额与流水累计**一致且精确**，
0 次 locked ⇒ `_busy_timeout=5000` 生效。

---

## 7. 硬性验证

### `go test ./internal/store/ -count=1`

```
$ go test ./internal/store/ -count=1
ok  	github.com/cn-maul/rosetta-gateway/internal/store	100.495s
```

### 关键并发用例 `-count=20`

```
$ go test ./internal/store/ -run TestStress -count=20
ok  	github.com/cn-maul/rosetta-gateway/internal/store	298.304s
```

（另有更早一次针对并发子集的 `-count=20`：`ok ... 12.850s`。）

### 全仓 `go test -count=1 ./...`

```
$ go test -count=1 ./...
ok  	github.com/cn-maul/rosetta-gateway/cmd/gateway	112.275s
ok  	github.com/cn-maul/rosetta-gateway/internal/admin	25.537s
ok  	github.com/cn-maul/rosetta-gateway/internal/auth	0.063s
ok  	github.com/cn-maul/rosetta-gateway/internal/crypto	0.220s
ok  	github.com/cn-maul/rosetta-gateway/internal/inwire	0.123s
ok  	github.com/cn-maul/rosetta-gateway/internal/outwire	0.094s
ok  	github.com/cn-maul/rosetta-gateway/internal/ratelimit	0.051s
ok  	github.com/cn-maul/rosetta-gateway/internal/routing	0.045s
ok  	github.com/cn-maul/rosetta-gateway/internal/server	4.021s
ok  	github.com/cn-maul/rosetta-gateway/internal/snapshot	1.151s
ok  	github.com/cn-maul/rosetta-gateway/internal/store	66.572s
ok  	github.com/cn-maul/rosetta-gateway/internal/upstream	0.634s
ok  	github.com/cn-maul/rosetta-gateway/internal/userauth	0.376s
ok  	github.com/cn-maul/rosetta-gateway/internal/webui	0.022s
```

**没有弄红任何别的包。**

> ⚠️ **诚实记录一次未复现的全仓失败**：最早的一次 `go test -count=1 ./...`
> 报 `FAIL github.com/cn-maul/rosetta-gateway/cmd/gateway 125.237s`，
> 但被截断的输出里没留下失败用例名。此后我连跑 **9 次**
> （全仓 5 次 + `cmd/gateway` 单独 4 次，含 `-json` 留档）**全部通过**，
> 也模拟了「store 压测与 gateway 并行争 CPU」的场景，仍全绿。
> 因此判断为**未复现的间歇性失败**，位置在 `cmd/gateway`（**不是**我的
> `internal/store` 压测，且我没有改动 `cmd/gateway` 的任何生产代码）。
> 未能定位到具体用例 —— 见第 9 节。**这条对本报告的结论无影响**
> （store 侧 24 个用例在 `-count=20` 与 `-race` 下均稳定通过），
> 但如实记录，因为它可能是 `cmd/gateway` 里某条时间敏感用例的 flake。

### `gofmt -l cmd internal`

```
$ gofmt -l cmd internal
cmd\gateway\e2e_datapath_test.go
```

`cmd/gateway/e2e_datapath_test.go` **不是我的文件** —— 它是另一位 teammate
新增的未跟踪文件（`git status` 显示 `?? cmd/gateway/e2e_datapath_test.go`）。
按「不要动别的 teammate 的文件」，我未触碰它。

**我的两个文件 gofmt 干净**：

```
$ gofmt -l internal/store/stress_billing_test.go internal/store/stress_quota_test.go
（无输出）
```

---

## 8. 发现的缺陷汇总

| # | 严重度 | 位置 | 一句话 |
|---|---|---|---|
| B4 | **P2** | `internal/store/balance_dao.go`（失败路径 `return ErrInsufficientBalance`）+ `cmd/gateway/main.go`（`usageRecorder.charge` 不重试） | 余额不足时本次已累加的余数随回滚丢弃；注释声称「调用方重试时重新累加」，但**该重试路径不存在**，故 < 1 分/次静默漏收 |
| B5 | **P2** | `internal/store/balance_dao.go` 的 `YuanToMicros` | 亚微元费用一律向上记 1 微元，误差**单向偏大**、随调用量线性累积（实测放大 10 倍） |

**没有 P0/P1，没有发现任何金额对不上的缺陷。** 两个 P2 都属于「记账边缘语义」，
不是「算错钱」：B4 影响面是已欠费用户且上限 < 1 分/次；B5 只在费用普遍
低于 0.5 微元的极端形态下显著。两者都**未修**（按要求只报告）。

### 值得记录的「反向发现」

- **两处 P0 修复在并发下依然成立**：失败路径不再误提交占位行（C3）、
  主键已含 `user_id`（C2）。这两条是本次重点验证的对象，均通过。
- **A4 的初版断言失败反证了真实链路走微元累加**：若实现按「逐条舍入到分」
  计费，低价模型下将完全收不到钱。实测收上了，说明余数机制在真实链路上生效。

---

## 9. 未能验证的部分（诚实清单）

1. **未复现的全仓间歇性失败**：见第 7 节。`cmd/gateway` 有一次 `FAIL`
   但输出被截断、失败用例名未捕获；随后 9 次运行全部通过，未能定位。
   需要后续用 `-json` 长期重复（例如 `-count=50`）才能抓到 —— 本次未做，
   因为单次 `cmd/gateway` 就要 ~110 秒。

2. **剪枝的「长写事务」规模有限**：F3 只用 2000 行明细（`PruneOldUsage` 单轮
   上限是 `maxPruneDelete = 20000`）。真正的百万行首剪会持锁数十秒
   （`usage_archive.go` 的注释自己承认会「表现为网关整体卡顿」）。
   未构造那种规模 —— 跑一次要很久，且**结论不会变**：持锁更久只会让排队更长，
   而 `busy_timeout=5000` 是否被撞破取决于「单轮剪枝是否超过 5 秒」，
   这一点由数据量与磁盘决定，本机无法代表生产。**该场景的真实风险未被排除。**

3. **未做进程级 kill（SIGKILL）下的持久性验证**：F1 是「不 Close 重开」，
   属于同一进程内的句柄泄漏，**不等价于**进程被强杀时 WAL 的恢复行为。
   真正的验证需要起一个子进程写库后 `taskkill /F`，再重开校验。
   未做（需要额外的测试脚手架与进程管理）。这是本报告最明显的覆盖缺口。

4. **`SetMaxOpenConns(1)` 之外的多进程写竞争**：F4 覆盖了同进程两个连接池，
   但没有覆盖**真正两个进程**（各自有独立 SQLite 连接与锁）的写竞争。
   多进程下 `busy_timeout` 的行为可能不同。

5. **未验证 `-race` 下的 `cmd/gateway` 全链路**：`-race` 只在
   `internal/store` 上跑了（store 是账目主战场）。`cmd/gateway` 的
   `-race` 会更慢，未尝试。

6. **金额与真实上游用量的对账**：本报告全部在 `store` 层验证
   「cost_total ←→ 扣费」闭合，但**没有**验证「上游真实 token 数 ←→
   `usage_records`」这一段（那需要 mock 上游或真实调用，属于 `cmd/gateway`
   的 E2E 范围，另一位 teammate 在做）。

---

## 10. 复现命令速查

```powershell
cd C:\Users\louis\Desktop\project\rosetta-gateway
$env:GOCACHE="C:\Users\louis\Desktop\project\rosetta-gateway\.gocache"

# 全部压测用例（单次）
go test ./internal/store/ -run TestStress -count=1 -v

# 并发关键用例 20 次（抓 flaky）
go test ./internal/store/ -run TestStress -count=20

# 全仓
go test -count=1 ./...

# -race（本机需显式指定无空格的 CC）
$env:CGO_ENABLED="1"; $env:CC="C:/mingw64/bin/gcc.exe"
go test -race ./internal/store/ -run TestStress -count=3

# 查看两条缺陷的实测数字
go test ./internal/store/ -run "TestStress_RemainderDiscarded|TestStress_SubMicro" -count=1 -v
```
