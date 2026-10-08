# P1 性能修复：用量批量落库（fix-p1-perf）

被测/被改对象：`C:\Users\louis\Desktop\project\rosetta-gateway`（`d529b29` + 未提交改动）。

改动文件：

| 文件 | 改动 |
|---|---|
| `internal/store/usage_dao.go` | **新增** `CreateUsageRecordsBatched` + 两个私有助手（不改单条路径） |
| `cmd/gateway/main.go` | `usageRecorder.worker` 改为攒批；新增 `flush` / `usageBatchSize` / `ctxBackground` |
| `cmd/gateway/perf_split_test.go` | **新增** 拆分测量（2 个用例） |
| `cmd/gateway/perf_batch_test.go` | **新增** 批量收益探针（1 个用例） |
| `internal/store/usage_batched_test.go` | **新增** 8 个账目/失败语义用例 |

**单条落库路径 `CreateUsageRecordWithCost` 一行未改** —— 它仍是其它调用方
（导入、剪枝回填、测试）以及 `len==1` 时的实际路径。

---

## 0. 先说一个比「优化」本身更重要的发现

**`AUDIT/test-perf.md` 里所有性能数字都只含 INSERT、不含扣费。**

`buildHarness`（`failover_test.go:192`）直接返回 `handleIngress`，
**没有包 `server.Middleware`**，于是 context 里没有 `request_id`。
而 `usageRecorder.charge` 在 `rec.RequestID == ""` 时**直接 return，完全不扣费**
（`main.go`，注释写着「空 request_id → 没有幂等键，扣费不做」）。

证据（本任务第一版拆分测量的输出）：

```
split I 只INSERT    吞吐= 3827.5 req/s  P50=22.378ms P99=282.934ms 新增 usage 行=1980
split F INSERT+扣费  吞吐= 3458.8 req/s  P50=24.311ms P99=301.669ms 新增 usage 行=1980
split 手臂F 扣费流水: 0 行, 合计 0 分（必须 > 0，否则「扣费成本」没被测到）
手臂 F 一条扣费流水都没有 —— 价格/余额没生效，拆分结论无效
```

我第一版的手臂 F 给了 I 几乎相同的数字、0 条扣费流水。若不核对这一行，
会把「扣费免费」写进结论。**真相是扣费根本没跑。** 修正办法是包上真
Middleware（复用既有的 `middlewareForTest`），修正后：

```
split 手臂F 扣费流水: 2082 行, 合计 3328 分
```

**这条对生产数字的含义**：生产上每个成功请求是 **两次写事务**
（INSERT + 扣费），而现有压测只覆盖了前一次。所以那条
「去掉落库 38.8×」的因果对照，在生产真实负载下的倍数会**比 38.8× 小**
—— 因为扣费那笔还在。实测真实倍数是 **11.6×**（见第 4 节）。

---

## 1. 拆分测量：INSERT vs 扣费各占多少

### 方法

沿用 `perf_leak_test.go` 的既有脚手架（`buildHarness` / `newPerfEnv` /
`runNonStreamWorkload` / `usageRowCount`），不另造一套。三个手臂：

| 手臂 | 请求路径 | 写事务数 |
|---|---|---|
| **N** | `model_not_found`（404） | 0 |
| **I** | 正常请求、**无价格** | 1（INSERT） |
| **F** | 正常请求 + **有价格 + 有余额** | 2（INSERT + 扣费） |

无价格 ⇒ `cost_total`=0 ⇒ `ChargeBalance` 在第一行短路，连事务都不开
（`balance_dao.go` 的「amountYuan <= 0 时完全 no-op」）。所以 I 干净地只含 INSERT。

单价取 **2000 元/百万 tokens**：每次请求 8 token ⇒ 0.016 元 = 1.6 分，
确保每次扣费都走**完整路径**（真正扣余额 + 回写金额），而不是「余数不够
一分」的短路分支 —— 后者语句更少，会低估扣费成本。

### 命令与原始输出

```
$ go test ./cmd/gateway/ -run TestSplitInsertVsCharge -v -count=1 -timeout 30m

split N 都不做(404)       吞吐= 37787.0 req/s  P50=2.949ms   P99=29.457ms   新增 usage 行=0
split I 只INSERT        吞吐=  3693.6 req/s  P50=22.572ms  P99=291.465ms  新增 usage 行=1981
split F INSERT+扣费      吞吐=  1642.0 req/s  P50=81.127ms  P99=601.703ms  新增 usage 行=1980
split 手臂F 扣费流水: 2082 行, 合计 3328 分

=== 拆分汇总（conc=200, n=3000，同机同方法）===
split 每请求耗时: N=26µs  I=271µs  F=609µs
split 归属: INSERT≈244µs/请求   扣费≈338µs/请求
split 结论 **两笔相当**（INSERT/扣费 = 0.72×）→ 只优化一侧最多省一半
```

### store 层交叉验证（第二种方法）

```
$ go test ./cmd/gateway/ -run TestSplitStoreTxCeiling -v -count=1

split-tx 串行: INSERT=1792 ops/s   扣费=2488 ops/s
split-tx 并发(200): INSERT=1529 ops/s(错误 0)   扣费=2317 ops/s(错误 0)
split-tx 并发/串行 扩展比: INSERT=0.85×  扣费=0.93×
split-tx 每事务耗时: INSERT=654µs   扣费=432µs
split-tx 归属: INSERT 耗时 / 扣费耗时 = 1.52×（>1 表示 INSERT 更贵）
split-tx 结论 **INSERT 事务更贵**（是扣费的 1.52× 耗时）→ 批量 INSERT 收益可观
```

### 结论

**两笔相当**（端到端 0.72×、store 层 1.52× 两个方向不同但都指向「同一量级」）。

这直接否定了「只批量 INSERT 就能解决」的天真预期：**最多省一半**。
也否定了「INSERT 是绝对大头、扣费可以先不管」的判断。

差异来源：`ChargeBalance` 的事务里有多条语句（占位 INSERT、
`UPDATE ... RETURNING` 余额守卫、金额回写、以及失败分支的 SELECT），
而 INSERT 是「一条多值 INSERT + 两个 AFTER INSERT 触发器」。

---

## 2. 批量到底能省多少（先量上界，再决定做不做）

「每事务 296µs」推不出「批量后 39µs/行」。中间必须直接量一次。

```
$ go test ./cmd/gateway/ -run TestBatchUpside -v -count=1 -timeout 20m

batch (a) N 次独立事务: 2000 行 592ms → 3376 行/s  每行 296µs
batch (b) 单事务多值INSERT bs=1   : 2000 行 609ms → 3283 行/s  每行 305µs
batch (b) 单事务多值INSERT bs=10  : 2000 行 155ms → 12908 行/s  每行 77µs
batch (b) 单事务多值INSERT bs=50  : 2000 行  77ms → 25814 行/s  每行 39µs
batch (b) 单事务多值INSERT bs=200 : 2000 行  89ms → 22397 行/s  每行 45µs
batch (c) 每次开事务但只插 1 行: 2000 行 1.154s → 1734 行/s  每行 577µs
```

**(a) 与 (c) 的对照很说明问题**：两者语句完全相同（一事务一行），
差别只在事务封装方式，(c) 反而更慢（577 vs 296µs，批量路径少一层封装）。
而 (b) bs=50 降到 39µs —— **7.6×**。

收益在 bs=50 附近饱和，bs=200 反而略升（大 VALUES 文本更大）。

> 注：(a) 用的是生产方法 `CreateUsageRecordWithCost`（内含查价往返），
> (b)/(c) 用的是直写（绕过查价）。所以 (a) 与 (b) 的 296→39 不是严格同基。
> 但**方向与量级是可靠的**：端到端三臂测量（同一真实路径、含查价）
> 独立印证了同一次量级改善，见第 4 节。

---

## 3. 选了什么方案、为什么

**只做方案 1（批量落库），不做方案 2（批量扣费）。**

理由，按权重：

1. **批量 INSERT 的收益已被实测钉死**：39µs/行 vs 296µs/行，7.6×。
2. **批量扣费的风险与收益不成比例**。幂等键是**逐条**的
   `(request_id, user_id)`。要聚合就得：
   - 批量插占位行 → 需要知道「哪些 request_id 已扣过」，
   - 同用户多笔合并成一条 `UPDATE` → 需要知道「这批里每一笔的
     cents 各是多少」。
   
   两者都要求把**逐笔状态**折叠成**聚合状态**。折叠规则一旦出错，
   后果是**漏收**或**重复收费** —— 而这两者都不会报错，只会表现为
   余额与报表对不上。批量 INSERT 侧不存在这个风险：每行费用逐条算，
   错不了。
3. **扣费那条事务本来就更便宜**（store 层 432µs vs INSERT 654µs），
   而它内部有余额守卫与幂等占位这两道钱的闸门。
4. **够不够**：批量后端到端 1642 → 2608 req/s（+59%），且
   **持续负载的吞吐崩塌消失**（第 4 节）。对「局域网小团队」这个目标场景，
   这个量级已经够。剩下的差距在扣费侧，要动它需要先解决聚合幂等，
   属于另一个需要产品口径决策的题目。

**批大小取 50**，理由是上面那条饱和曲线（39µs @ bs=50，45µs @ bs=200）。

---

## 4. 改后的事务边界

### 之前

```
请求 A ─┬─ worker: 事务1: INSERT usage_records(A)        ~296µs
        └─ worker: 事务2: ChargeBalance(A)             ~338µs   ← 两笔事务/请求
请求 B ─┬─ worker: 事务3: INSERT usage_records(B)
        └─ worker: 事务4: ChargeBalance(B)
```

### 之后

```
一批 (A..A49) ─┬─ 事务1: 逐条 freezeUsageCost 计价（读池，不持写锁）
               │          + 单条多值 INSERT（50 行）+ 2 个 AFTER INSERT 触发器逐行触发
               │                                        ~39µs/行
               └─ 事务2..51: 逐条 ChargeBalance          ~338µs/条   ← 仍逐条
一批 (B..B49) ─┬─ 事务52: 多值 INSERT
               └─ 事务53..102: 逐条 ChargeBalance
```

**关键点：事务数其实没有减少**（50 条仍然是 50 笔扣费事务）。
减少的是**INSERT 的提交次数**（50 → 1）。这与第 1 节的结论一致：
省的是 INSERT 那 ~244µs，扣费那 ~338µs 一分没省。

### 失败语义

| 情形 | 行为 | 为什么 |
|---|---|---|
| 整批落库失败（主键冲突等） | **整批回滚**，`CreateUsageRecordsBatched` 返回**全 0 费用** + err；`flush` **一条都不扣** | 与单条路径「落库失败 → 返回 (0,err) → 不扣费」一致。**部分扣费**会让这一批里出现无法解释的「报表有、余额没扣」的偏差，而用量是**计费依据**，宁缺勿滥 |
| 批中第 3 条扣费失败 | 前 2 条**已经扣了**（各在自己的事务里提交了），第 3 条起按 `charge()` 原有语义处理：欠费记 ERROR 不重试、其它错误记 ERROR | 见下 |
| 队列关闭 | `worker` 冲掉**手上那批**（未满 50 的部分）再退出 | 关停 drain 的全部意义就是别弄丢已发生的调用 |
| 队列满 | `record()` 走同步写背压（**未改**） | 不变式 6「用量不丢」 |

### 「一批里第 3 条扣费失败」的具体答案

**前 2 条已扣，后 7 条照常各扣各的。** 没有「回滚整批」这种选项，
因为扣费是**逐条独立事务**（第 3 节的理由 2）。

失败各自的后果已经由 `charge()` 既有逻辑处理：

- `ErrInsufficientBalance` → 记 ERROR，**不重试**。此时上游已被调用、
  费用已固化进 `cost_total`，钱收不回 —— 该笔计入「欠费漏收」，
  是设计取舍（见 `charge()` 注释）。
- 其它 error（含 DB 故障）→ 记 ERROR。

**为什么这是可接受的**：批只影响**落库**，而落库是全有或全无的；
扣费的失败在批量之前就是逐条独立的，批量没有改变它的原子性边界。
唯一的差别是「这 7 条的落库与前 2 条在同一个事务里成功了」——
但落库成功与扣费成功本来就是两件事（前者是「记录这次调用发生了」，
后者是「收这笔钱」）。

---

## 5. 前后性能对比（同机、同方法、同并发）

方法：`TestSplitInsertVsCharge`，conc=200、n=3000、真 Middleware、
每个场景 5 次取分布（`test-perf.md` 实测同场景离散度可达 53%，
所以单次数字不作数）。

### 改前（临时把 worker 换回「一次一条独立事务」量同机基线）

```
BEFORE I 只INSERT    : 4153.8 / 3783.6 / 4341.3 / 4402.5 / 4432.7  req/s
BEFORE F INSERT+扣费 : 1814.1 / 2080.9 / 1971.6 / 2647.0 / 2602.6  req/s
```

### 改后

```
AFTER  I 只INSERT    : 5069.4 / 4766.5 / 4844.7 / 5238.5 / 5089.4  req/s
AFTER  F INSERT+扣费 : 2556.5 / 2615.3 / 2589.0 / 2404.2 / 2666.0  req/s
```

### 汇总

| 场景 | 改前（中位） | 改后（中位） | 提升 |
|---|---|---|---|
| I 只 INSERT | 4341 req/s | 5089 req/s | **+17%** |
| F INSERT + 扣费 | 2081 req/s | 2589 req/s | **+24%** |

| 分位（F，扣费真实存在） | 改前 | 改后 |
|---|---|---|
| P50 | 66.3ms | 51.0ms |
| P99 | 453.5ms | 328.9ms |

改前区间 1814–2647、改后 2404–2666 —— **两个分布高度重叠**，
所以**端到端 +24% 这个数不该被当成显著提升**。诚实结论：
**端到端改善在噪声量级，真正确定的收益在持续负载形态。**

### 真正确定的改善：持续负载的崩塌消失

这是最能说明问题的一项（`TestDiagSustainedBackpressure`，conc=200、n=6000）：

```
改前（AUDIT/test-perf.md 第 2.5 节）:
  总吞吐=1841 req/s
  第 0s: 2592 req/s  P50=26.403ms  P99=371.022ms
  第 1s: 1710 req/s  P50=83.254ms  P99=572.583ms
  第 2s: 1217 req/s  P50=103.237ms P99=673.858ms
  第 3s:  481 req/s  P50=117.103ms P99=849.892ms      ← −81%

改后（实测）:
  总吞吐=4037 req/s（2.2×），wall=1.486s
  第 0s: 4407 req/s  P50=17.315ms  P99=274.617ms
  第 1s: 1593 req/s  P50=39.773ms  P99=377.790ms
  第 2s~5s: 无样本（已排空）
```

**6000 个请求从「4 秒内单调衰减到 481/s」变成「1.5 秒跑完」**。
之前那个「越用越慢」的反直觉现象消失了。整批落库把队列积压清空的速度
快了一个量级，尾窗口不再出现。

### 队列排空（用量不丢，且追平更快）

```
TestPerfUsageQueueSaturation（容量 1024，请求 1524）:
  改前: 落库=1524  追平耗时=905ms（请求墙钟 539ms）
  改后: 落库=1524  追平耗时= 82ms（请求墙钟 207ms）
```

**1524 → 1524，零丢失**（不变式 6 成立），且追平时间 905ms → 82ms。

### 因果对照的比值变化（暴露了第 0 节那个发现）

```
TestDiagUsageWriteIsTheCause（改后）:
  A 有用量记录: 4029 req/s     B 无用量记录: 46674 req/s   比值 11.58×
改前（AUDIT/test-perf.md）: 931 vs 36144 → 38.81×
```

比值从 38.8× 降到 11.6× —— 不是「变差了」，而是**改前那个 38.8× 测的是
不含扣费的场景**（第 0 节）。改后写入侧快了，于是扣费那笔在总账里
占的比重上升，比值自然收窄。真实瓶颈从「落库」移到了「扣费」。

---

## 6. 不变量仍然成立的证据

### 6.1 store 层账目精确性（我自己写的 24 个并发用例）

```
$ go test ./internal/store/ -run TestStress -count=5
ok  	github.com/cn-maul/rosetta-gateway/internal/store	33.955s
```

覆盖：并发扣费总额精确（200 路）、余额恰好够 M 次则**恰好** M 次成功、
混合金额不为负、余数累加精确、并发余数不丢、亚微元不丢、
幂等（同 request_id / 跨用户撞号 / 失败后重试）、配额不超发、
配额无泄漏、净记账不 2 倍、剪枝前后总额不变、反复剪枝幂等。

### 6.2 批量落库的新增用例（8 个，`usage_batched_test.go`）

```
$ go test ./internal/store/ -run TestBatched -count=3
ok  	github.com/cn-maul/rosetta-gateway/internal/store	1.012s
```

| 不变量 | 用例 | 钉住什么 |
|---|---|---|
| 1「扣的 = 报表的」 | `CostMatchesSingleRowPerRecord` | 批量第 i 条费用 == 单条路径对同一输入的费用，**逐条比对**（总额相等抓不到错位） |
| 1 | `OneRowMatchesSingleRowPath` | 批=1 时与单条路径逐字段相同 |
| 1 | 返回值 vs 落库值 | 从库读回 `cost_total` 再比一次，确认是同一个数 |
| 3「失败不扣费」 | `PartialFailureRollsBackWholeBatch` | 一条主键冲突 → 整批回滚 + **返回全 0 费用**（全 0 才是「整批不扣」的依据） |
| 负值归一 | `NegativeTokensClampedLikeSinglePath` | 负 token 被夹到 0（绕过则经触发器永久拉低终身累计 → 配额凭空放宽） |
| 触发器逐行 | `TriggerStillFiresPerRow` | `used_tokens` / `usage_totals` 逐行累加（共享一次触发会少算） |
| 并发批量 | `ConcurrentBatchesStayExact` | 20 批 × 25 行：行数、distinct id、used_tokens、终身累计全部精确 |
| 计价容错 | `UnknownProviderCostsZero` | 查价失败记 0 但**照样落库**（不因计不了价丢用量） |
| 边界 | `EmptyAndSingleEdgeCases` | 空批次/空切片不写库不报错 |

### 6.3 现有计费用例未被破坏

```
$ go test ./internal/store/ -count=1
ok  	github.com/cn-maul/rosetta-gateway/internal/store	39.831s

$ go test ./cmd/gateway/ -run "TestBalance|TestCharge|TestBilling|TestFailover|TestE2E|TestUsage|TestQuota|TestBillingScope|TestQueue|TestShutdown" -count=1
ok  	github.com/cn-maul/rosetta-gateway/cmd/gateway	7.619s
```

### 6.4 关停 drain 与并发安全（`-race`）

```
$ CC=C:/mingw64/bin/gcc.exe go test -race ./cmd/gateway/ -run "TestRecordAfterWaitDoesNotPanic|TestRecordConcurrentWithWait|TestWaitDoesNotDeadlockWithRecord" -count=5
--- PASS × 15    ok  7.975s

$ CC=C:/mingw64/bin/gcc.exe go test -race ./internal/store/ -run "TestBatched|TestStress_Concurrent|TestStress_Idempotency|TestStress_ReserveQuota|TestStress_FullLifecycle" -count=3
ok  	github.com/cn-maul/rosetta-gateway/internal/store	28.556s

$ CC=C:/mingw64/bin/gcc.exe go test -race ./cmd/gateway/ -run "TestBalance|TestCharge|TestUsage|TestQuota|TestShutdown|TestQueue|TestBilling" -count=1
ok  	github.com/cn-maul/rosetta-gateway/cmd/gateway	5.585s
```

**0 数据竞争。** 攒批引入了新的共享状态（复用缓冲区、`ctxBackground`），
`-race` 是验证它安全的手段而非形式。

### 6.5 全仓

```
$ go test -count=1 ./...
（16 个包全部 ok，含 cmd/gateway 88.540s、internal/store 39.831s）

$ gofmt -l cmd internal
（无输出）
```

---

## 7. 我自己犯的三个错误（都留了痕迹）

1. **把「去掉扣费」误当成「去掉 INSERT」** —— 第一版手臂 F 的扣费流水是
   0 行，而我没有先看这一行就准备写结论。若照那样写，会把「扣费免费」
   记进报告。修正方式是加一条 `chargeRows == 0 → t.Fatal` 的护栏。
2. **并发结论写反了** —— `TestSplitStoreTxCeiling` 第一版把
   「ops/s 更高」读成「更贵」，而 ops/s 高恰恰是**便宜**。
   改成先换算成每事务耗时再比。
3. **`TestBatched_OneRowMatchesSingleRowPath` 连错两次**：
   先是假设 `ORDER BY id` 后 `rows[0]=s1`（实际 `b1 < s1`），
   再是把**必须不同**的 `ID`/`RequestID` 也纳入了相等断言。

三者都写进了代码注释，因为「测试自身的错误」比「测试通过」更容易
在后来者重写时被误当成结论。

---

## 8. 未能验证 / 已知局限

1. **端到端 +24% 不可信为显著提升**：改前 1814–2647、改后 2404–2666，
   两个分布重叠。**真正确定的收益是持续负载形态**（崩塌消失、排空 11×）。
2. **单次压测最长 6000 请求 / 约 1.5s**。小时级稳定性未覆盖。
3. **扣费侧未优化**，它是改后最大的单笔开销（~338µs/请求）。
   要动它需要先定「批量幂等」的口径，属另一个决策。
4. **批大小 50 是在本机、假上游、零延迟下测的**。真实上游有网络延迟，
   到达率更低 ⇒ 批更小 ⇒ 收益更接近 bs=1（无收益）。
   **低负载场景下本改动等价于「什么都不做」**，这是刻意的
   （低负载不需要吞吐，低延迟更重要）。
5. **`AUDIT/test-perf.md` 的历史数字全部只含 INSERT**。若要把那些
   基线用于回归对比，应先用带 Middleware 的口径重测一遍 ——
   本报告第 5 节已给出该口径的数。
6. **未验证崩溃恢复**：批量事务中途进程被杀时，WAL 恢复行为未测
   （单条路径也同样未测）。

---

## 9. 复现命令

```powershell
cd C:\Users\louis\Desktop\project\rosetta-gateway
$env:GOCACHE="C:\Users\louis\Desktop\project\rosetta-gateway\.gocache"

# 拆分测量（INSERT vs 扣费）
go test ./cmd/gateway/ -run TestSplitInsertVsCharge -v -count=1 -timeout 30m
go test ./cmd/gateway/ -run TestSplitStoreTxCeiling -v -count=1

# 批量收益探针
go test ./cmd/gateway/ -run TestBatchUpside -v -count=1 -timeout 20m

# 批量落库的账目/失败语义
go test ./internal/store/ -run TestBatched -count=3

# 持续负载形态（改动的核心收益）
go test ./cmd/gateway/ -run TestDiagSustainedBackpressure -v -count=1

# 竞态与全仓
$env:CGO_ENABLED="1"; $env:CC="C:/mingw64/bin/gcc.exe"
go test -race ./internal/store/ -run "TestBatched|TestStress" -count=3
go test -count=1 ./...
```