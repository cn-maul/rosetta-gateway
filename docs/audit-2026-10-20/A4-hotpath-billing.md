# A4 审计报告：请求热路径与计费记账

- 仓库：`rosetta-gateway`
- 基线：`main = ea19ce1`，module `github.com/cn-maul/rosetta-gateway`
- 审计范围：`cmd/gateway/main.go`（handleIngress / attemptStream / attemptNonStream / usageRecorder / rateCommit）、`cmd/gateway/billing.go`、`cmd/gateway/replay_gate.go`，以及 `internal/store` 中的计费/配额 DAO。
- 性质：**只读审计**。本报告未修改仓库内任何 git 跟踪文件。
- 证据口径：每条结论给出 `相对路径:行号` 与代码证据。不能确认者标注「待确认」。

## 0. 结论摘要

| 级别 | 数量 |
|---|---|
| P0 | 3 |
| P1 | 6 |
| P2 | 7 |
| P3 | 5 |

**总体判断**：预占/释放的**配对结构**是完备的（我逐条核对了 handleIngress 的 7 个 early-return 与 attempt 层的 3 个收尾点，只发现一处遗漏）。但**「记账 → 扣费」这一段有系统性断裂**：三处独立的路径会让用户永久免单，且**全仓没有任何对账任务**能发现它们。核心根因是同一句话的三个变体：

> 一次请求的**成本**（上游已计费）与**应收**（用户该付）之间，只有一条单向管道（`usage.record` → `flush` → `charge`），管道上**每一处失败都只记一条日志**，没有任何重试、回放或对账。

这三处断裂分别是：
1. `charge` 吞掉所有错误 → 永久少记账（§3.6）；
2. `cost_pending` 重算只修报表不修余额 → 永久少记账（§3.2）；
3. 客户端中断/无终止事件的流 `total_tokens=0` → 上游已计费但用户免单**且绕过终身 token 配额**（§4.4）。

第四个系统性问题是**配额的永久冻结**：`reserved_tokens` 是终身预占，进程异常终止后没有任何复位路径（§3.7 / P1-2）。

---
## 1. handleIngress 状态机（鉴权 → 预占 → 尝试 → 记账 → 释放）

### 1.1 状态机全貌

```
handleIngress (main.go:1738)
 │
 ├─[S1] auth.Authenticate(r)                    main.go:1743
 │      fail → writeAuthError → return          main.go:1745  （未预占，无泄漏）
 │
 ├─[S2] limiter.AllowRPM                        main.go:1757
 │      fail → 429 → return                     main.go:1764  （未预占）
 │
 ├─[S3] codec.Decode                            main.go:1767
 │      fail → 400/413 → return                 main.go:1778  （未预占）
 │
 ├─[S4] est = estimateRequestTokens             main.go:1800
 │      limiter.ReserveTPM                      main.go:1801
 │      fail → 429 → return                     main.go:1808  （TPM 未预占）
 │      ok  → rate = &rateCommit{reserved:est}  main.go:1810
 │
 ├─[S5] db.GetKeyQuota → quota>0                main.go:1838
 │      db.ReserveQuota                         main.go:1846
 │      err  → fail-open（继续）                main.go:1849
 │      !rok → 429 → return                     main.go:1857  ★★ P0-1 漏退 TPM
 │      ok  → rate.quotaReserved = reserved     main.go:1862
 │
 ├─[S6] 用户级 ReserveUserQuota                 main.go:1894
 │      !rok → rate.commit(0) → 429 → return    main.go:1908  ✅ 已释放
 │      ok  → rate.userQuotaReserved = reserved main.go:1916
 │
 ├─[S7] snap.Routes.Resolve                     main.go:1922
 │      fail → rate.commit(0) → 404 → return    main.go:1924  ✅
 │
 ├─[S8] authCtx.AllowsModel                     main.go:1947
 │      fail → rate.commit(0) → 403 → return    main.go:1948  ✅
 │
 ├─[S9] totalRequestBudget / ctx 绑定           main.go:1973-1976
 │
 ├─[S10] precheckBalance                        main.go:1997
 │      fail → rate.commit(0) → return          main.go:2002  ✅
 │
 ├─[S11] 候选筛选（熔断/结构化输出/预算截断）   main.go:2024-2072
 │      全不可服务 → rate.commit(0) → 400       main.go:2058  ✅
 │
 └─[S12] 故障转移循环                            main.go:2087
        ├─ 客户端断开 → rate.commit(0) → return  main.go:2098  ✅
        ├─ ClaimTargetProbe 失败 → continue/break（未领名额）main.go:2117
        ├─ GetAnyClient 失败 → RecordTargetFailure → continue/break  main.go:2138
        └─ attemptStream / attemptNonStream     main.go:2152/2154
             ├─ committed → return（usage 已在 attempt 内落库）main.go:2183
             └─ 未提交 → 沿链继续

  链耗尽 → codec.WriteError → rate.commit(0) → usage.record(error)  main.go:2260-2301
```

### 1.2 预占释放完备性核对

`rateCommit` 的两个预占字段都有「先取后清零」的自幂等保护（`main.go:1570-1572` 的 `quotaReserved`、`main.go:1580-1582` 的 `userQuotaReserved`），因此**重复调用 `releaseQuota` 不会双释放**；`commit` 内部也只调一次 `releaseQuota`（`main.go:1553`）。`commit` 与 `releaseQuota` 的分工清晰：`commit(actual)` = TPM 校正 + 退配额；`releaseQuota()` = 只退配额（usage missing 专用，理由见 `main.go:1556-1563`）。

**逐条核对通过的路径**（每条 early-return 都有 `rate.commit(0)`）：

| 位置 | 触发条件 | 释放调用 |
|---|---|---|
| main.go:1908 | 用户级配额满 | ✅ `rate.commit(0)` |
| main.go:1924 | 模型不存在 | ✅ `rate.commit(0)` |
| main.go:1948 | 模型不在白名单 | ✅ `rate.commit(0)` |
| main.go:2002 | 余额预检失败 | ✅ `rate.commit(0)` |
| main.go:2058 | 无上游能服务结构化输出 | ✅ `rate.commit(0)` |
| main.go:2098 | 循环开始前客户端断开 | ✅ `rate.commit(0)` |
| main.go:2263 | 链耗尽终态 | ✅ `rate.commit(0)` |
| main.go:2888-2892 | 流式 attempt 收尾 | ✅ `commit` / `releaseQuota` 二选一 |
| main.go:2955-2959 | 非流式 attempt 收尾 | ✅ `commit` / `releaseQuota` 二选一 |

**唯一例外是 S5**（见 P0-1）。

### 1.3 P0-1：S5 分支漏退 TPM 预占（key 级配额超限）

`cmd/gateway/main.go:1846-1857`：

```go
reserved, rok, rerr := db.ReserveQuota(r.Context(), authCtx.KeyID, est)
switch {
case rerr != nil:
    logger.Error("quota reserve failed (fail-open)",
        "error", rerr, "key_id", authCtx.KeyID)
case !rok:
    logger.Warn("quota exceeded", "key_id", authCtx.KeyID,
        "estimated_tokens", est,
        "request_id", server.RequestIDFromContext(r.Context()))
    codec.WriteError(w, http.StatusTooManyRequests, "insufficient_quota",
        "this API key has exhausted its token quota")
    return          // ← 直接 return，没有 rate.commit(0)
```

与紧邻的 S6 分支对比，S6 在 return 前显式调用了 `rate.commit(0)` 并有注释说明为什么（`main.go:1906-1908`：「拒绝前必须退回 key 级预占：它已经在同一请求里立起来了，直接 return 会让这把 key 的额度被这次被拒的请求永久吃掉一块。」），**S5 却漏了**。

后果分解：

- `rate.quotaReserved` 在 `!rok` 分支**不赋值**（`main.go:1862` 只在 `default` 分支执行），所以**终身配额预占本身没有泄漏**。
- 但 `rate.reserved`（TPM 窗口预占，值为 `est`，`main.go:1812`）**永远不会被 `CommitTPM` 校正** —— 既不退还也不校正。

`internal/ratelimit/limiter.go:106-127` 的 `ReserveTPM` 在放行时执行 `b.tokens += est`（`limiter.go:125`）。这笔 est 被永久计入当前分钟的 TPM 窗口，没有任何路径把它减回去。

影响：TPM 限速启用时，一个已耗尽 token 配额的 key 可以**反复触发 S5 拒绝**，每次都在 TPM 窗口里留下一笔永不退还的 `est`。由于 `est = estimateRequestTokens(...)` 包含 `max_tokens`（`main.go:1626`：`EstimateTokens(文本) + MaxOutputTokens`），单笔可达数千 token。**几十次配额拒绝就能吃光该 key 的整分钟 TPM 额度**，此后即便管理员调大配额，该 key 在本分钟内仍会被 TPM 层 429 拒绝。

这不是「少记一次账」，而是**一个本应零成本的拒绝请求反过来消耗了该 key 的限速额度**。因为配额是终身累计而 TPM 窗口每分钟翻转，攻击面是「每分钟可重复触发」，可稳定复现。

严重度 **P0**：越权放大（拒绝本应零成本却消耗了别的配额），且无任何报错（只记一条 WARN）。

修法：`main.go:1857` 的 `return` 前加 `rate.commit(0)`（与 `main.go:1908` 同款）。

**已验证**：`rate.commit(0)` 会调 `CommitTPM(keyID, limit, reserved, 0)`，其语义是 `b.tokens + 0 - reserved`（`limiter.go:145`），即把 est 全额退回；下界夹 0 不会吞掉这笔（est 本来就是刚加进去的）。

---
## 2. 计费口径

### 2.1 usage 的提取路径

三条协议各自从**流式终止事件**取 usage，SDK 层各自实现，网关侧统一在 `main.go:2795-2800` 收取：

```go
if ev.Type == rosetta.EventMessageEnd {
    if ev.Usage != nil {
        lastUsage = *ev.Usage
    }
    stopReason = ev.StopReason
}
```

| 协议 | SDK 取点 | 网关取点 |
|---|---|---|
| openai-chat | `provider_openai_chat.go:605-608`（`ch.Usage != nil` → `usage = &u`），随 `endEvent()` 发出（`provider_openai_chat.go:513-524`） | `main.go:2795-2800` |
| anthropic | `provider_anthropic.go:877-898`（`message_delta` 折叠进 `usage`），`endEvent()` 在 `hasUsage` 时挂 `ev.Usage`（`provider_anthropic.go:742-745`） | 同上 |
| openai-responses | `provider_openai_responses.go:608-612`（`response.completed` 折叠） | 同上 |

**关键观察**：SDK 在**截断流**上仍会发出 `endEvent()`（`provider_openai_chat.go:549`：`return endEvent(), nil`），因此 `truncated` / `overflow` 状态**能拿到真实 usage**。只有**没有终止事件的流**（客户端中断、idle 看门狗掐断）拿不到 —— 这正是 P1-1 的根因。

非流式：`resp.Usage` 直接取（`main.go:2941-2946`）。

**已验证无问题**：usage 不会从流中间取、不会被取两次、不会因为 `sink.Event(ev)` 改写（sink 只读不改 `ev`）。

### 2.2 usage 缺失时的兜底

`usageStateFor`（`main.go:2972-2977`）区分 `missing` / `reported`，判据是 `rosetta.Usage.IsZero()`。SDK 的 `IsZero`（`rosetta@v1.2.0/usage.go:32-35`）把 `CachedInputTokens` / `CachedCreationTokens` / `ReasoningTokens` 都计入，所以「只报了缓存命中或思考 token」算 reported，不会丢真实数字（`main.go:2970-2971` 的注释说明了这个设计意图）。

**P2-1**：usage missing 时 TPM 预占按 `max_tokens` 上界**永久保留**在窗口里。

`main.go:2888-2892`：

```go
if !lastUsage.IsZero() {
    rate.commit(lastUsage.TotalTokens)
} else {
    rate.releaseQuota()
}
```

`releaseQuota()` 故意保留 TPM 预占（理由见 `main.go:2882-2887`：「commit(0) 会全额退还 TPM 预占，等于不报用量的上游完全绕过 TPM」）。这个**方向是对的**（不报 usage 的上游不能绕过 TPM）。

但 `est` 是按 `max_tokens` 上界估算的（`main.go:1626`），所以缺失 usage 的请求会让 TPM 窗口**按上界全额占住**。上游不报 usage 的第三方兼容服务在主流部署里是常态（SDK 自己的 `Quirks.NoStreamUsage` 注释就写明「otherwise stream usage is missing」，`rosetta@v1.2.0/options.go:20-23`；SDK 默认 `streamOptions: !NoStreamUsage` 为 true，`provider_openai_chat.go:50`），这类流量会**系统性压低该 key 的 TPM 有效额度**。量级取决于 `max_tokens` 配置，可达实际用量的 10 倍以上。

严重度 **P2**（方向保守、不超发，但有效额度被低估）。

### 2.3 thinking / cached token 是否计入成本

`priceUsage`（`internal/store/usage_dao.go:332-352`）只用了 `inputTokens / cachedTokens / outputTokens` 三个量：

```go
func priceUsage(priceInput, priceCacheHit, priceOutput, inputTokens, cachedTokens, outputTokens float64) float64 {
    uncached := inputTokens - cachedTokens
    if uncached < 0 { uncached = 0 }
    hit := priceCacheHit
    if hit <= 0 { hit = priceInput }
    total := (uncached*priceInput + cachedTokens*hit + outputTokens*priceOutput) / 1_000_000.0
    ...
}
```

**thinking token（已验证无问题）**：

- OpenAI 系：reasoning token 是 `output_tokens` 的**子集**（`provider_openai_chat.go:717-720`：`CompletionTokensDetails.ReasoningTokens` ⊆ `CompletionTokens`），所以 `priceUsage` 不含 reasoning 项是**正确**的，不重复计费。
- Anthropic：`anthroUsage`（`provider_anthropic.go:1005-1010`）**根本没有 reasoning 字段** —— Anthropic 的思考 token 计入 `output_tokens`，同样不重复。

reasoning token 落库进了 `usage_records.reasoning_tokens`（`main.go:2866`）、`usage_totals.reasoning_tokens`（`usage_archive.go:237`）、报表，但**不进入成本公式** —— 这是正确的。

**cached token**：计入成本，且 `price_cache_hit <= 0` 时回退到 `price_input`（`usage_dao.go:337-340`），保证「只填输入价的模型不会把命中部分算成免费」。

**★ P1-2：Anthropic 的 `cache_creation`（缓存写入）token 被按输入价全额计费**

`rosetta@v1.2.0/provider_anthropic.go:1012-1027`：

```go
func (u *anthroUsage) toUsage() Usage {
    cached := u.CacheReadInputTokens.Value
    creation := u.CacheCreationInputTokens.Value
    input := u.InputTokens.Value + cached + creation
    return Usage{
        InputTokens:          input,
        OutputTokens:         u.OutputTokens.Value,
        TotalTokens:          input + u.OutputTokens.Value,
        CachedInputTokens:    cached,
        CachedCreationTokens: creation,
    }
}
```

Anthropic 的 `input_tokens` **只含未缓存部分**，cache_read / cache_creation 与它**互斥**。SDK 把三者相加得到统一的 `InputTokens`（因为统一模型里 `CachedInputTokens ⊆ InputTokens`）。

代入 `priceUsage`：

- `uncached = input - cached = (origInput + cached + creation) - cached = origInput + creation`
- `hit` 项 = `cached × price_cache_hit`

→ **`creation` token 落在 `uncached` 里，被按 `price_input` 全额计费**，而**没有享受任何缓存写入价**。

同时，`UsageRecord.CachedTokens` 落库取的是 `lastUsage.CachedInputTokens`（`main.go:2867`、`main.go:2945`），**只含 cache_read，不含 cache_creation**。所以归档表 `usage_daily_rollups.cached_tokens`（`usage_archive.go:644`）也漏掉 creation。

**这里有一个需要精确区分的地方**（避免夸大）：

- `creation` token **只被计费一次**（在 `uncached` 里），**没有被计费两次**。我最初怀疑的「双倍计费」不成立。
- 真正的问题是：缓存写入 token 被按**缓存未命中的输入价**收费。Anthropic 官方口径里 `cache_creation` 价通常**高于**普通输入价（写入要占存储），所以这里**低估**了成本 → **少收网关的钱**。
- 但从**用户**角度看：用户被按普通输入价付费，而这份 token 实际上是缓存写入产生的。**这不构成「多收用户钱」** —— 用户拿到的服务就是这次生成，收费口径是网关的单价表。

所以这条的严重度取决于单价表怎么配：

- 若 `price_cache_hit < price_input`（缓存命中更便宜，这是常规配置），那么 cache_creation 被按 `price_input` 收费**高于**其实际应收费价（缓存写入价通常介于两者之间或更低）→ **多收用户钱**。
- 若单价表没区分（`price_cache_hit == 0` → 回退 `price_input`），则完全无影响。

**归类调整**：这不是「口径漂移导致的多收」，而是「**Anthropic 的 cache_creation 没有独立单价入口，被迫按输入价计**」—— 属于**数据模型缺字段**（`upstream_models` 只有 `price_input / price_cache_hit / price_output` 三列，`internal/store/store.go:207-209`，没有 cache_creation 价）+ 归档列口径漏项。

严重度 **P1**（Anthropic 缓存场景下计费不精确，且 `cached_tokens` 归档口径不完整，影响所有按缓存命中率做的报表与对账）。

**待确认**：缓存写入价在当前运营口径下是否应当独立计价？若产品上不区分，severity 可降为 P2（`cached_tokens` 归档漏项仍需修）。

### 2.4 模型价格查找失败的兜底

`internal/store/usage_dao.go:392-414` `freezeUsageCost`：

```go
err := s.read.QueryRowContext(ctx,
    `SELECT COALESCE(price_input, 0), COALESCE(price_cache_hit, 0), COALESCE(price_output, 0)
       FROM upstream_models WHERE provider_id = ? AND model_id = ?`,
    r.ProviderID, r.UpstreamModel).Scan(&pin, &phit, &pout)
if err != nil {
    if !errors.Is(err, sql.ErrNoRows) {
        s.logger.Warn("freeze usage cost: price lookup failed, marked cost_pending",
            "provider_id", r.ProviderID, "upstream_model", r.UpstreamModel,
            "request_id", r.RequestID, "error", err)
        return 0, true
    }
    return 0, false
}
```

- `sql.ErrNoRows`（模型不存在 / 从没配过价）→ `return 0, false` → 费用 0，`usage_state` 保持原值（`reported`）。这是**正确的**（未配价 = 0，与 `usage_dao.go:327-328` 的口径一致）。
- 其他错误（读池抖动）→ `return 0, true`（pending）→ `usage_state` 改写为 `cost_pending` **随 INSERT 落库**（`usage_dao.go:146-152`、`usageInsertArgs` 的 `usage_dao.go:291-293`）。

「待重算标记与记录同时落库」这一点是**对的**，避免了「INSERT 后 UPDATE 命中 0 行」的老坑（注释在 `usage_dao.go:140-145`）。

但见 **P0-2**：`cost_pending` 的重算只修 `cost_total`，不修余额。

### 2.5 浮点误差累积

**费用侧无累积**：`priceUsage` 在 `usage_dao.go:348-351` 把结果舍入到 1e-9 元：

```go
if total != 0 {
    total = math.Round(total*1e9) / 1e9
}
```

每次独立计算后舍入，不存在跨次累加的浮点漂移。

**余额侧无累积**：全部走整数。

- `balance_remainder`：微元（1e-6 元）整数列，累加用 `UPDATE ... RETURNING` 原子读回（`balance_dao.go:352-357`）。
- `balance_cents`：整数分列（`store.go:303` 的建表注释解释了为什么用 INTEGER 而非 REAL）。

`YuanToMicros`（`balance_dao.go:79-99`）用 `math.Round(yuan*1e6)`，并对极小正费用向上取整到 1 微元。注释明确说明这是**故意的单向偏差**（`balance_dao.go:89-99`：「永远向上取整，所以偏差是单向的、随调用次数线性累积，不会自愈」），实测放大约 10 倍（10000 次 × 0.0000001 元 → 真实 0.1 分、实扣 1 分）。

**已验证无问题**：浮点误差不会造成累积漂移。`YuanToMicros` 的向上取整是**已知的、被文档化的**单向偏差，不在本次审计的缺陷范围。

### 2.6 billing.go 与 internal/store 的职责边界

`cmd/gateway/billing.go` 只做**预检**（`precheckBalance`，`billing.go:667-722`），`internal/store` 做**实际扣费**（`ChargeBalance`，`balance_dao.go:309`）。职责边界本身清晰。

但**计价公式在两处各写了一份**：

- 预检侧 `cmd/gateway/billing.go:512`：
  ```go
  cost := (float64(inTokens)*inPrice + float64(maxOut)*outPrice) / 1_000_000.0
  ```
- 实际侧 `internal/store/usage_dao.go:341`：
  ```go
  total := (uncached*priceInput + cachedTokens*hit + outputTokens*priceOutput) / 1_000_000.0
  ```

`billing.go:469-472` 的注释自己承认了这个风险：「计价公式与 store.priceUsage **同源**…两处若漂移，后果比不预检更糟：用户会在「明明够钱」时被拒。故改任一侧口径都必须同步另一侧。」

**实际差异**：

| 维度 | `estimateBalanceCost`（预检） | `priceUsage`（实际） |
|---|---|---|
| 缓存项 | **完全没有**（按「全部未命中」计上界） | 有 `price_cache_hit` 项 |
| `price_input <= 0` 兜底 | 回退到 `price_output`（`billing.go:500-502`） | 无兜底（未命中部分免费） |
| `price_output <= 0` 兜底 | 回退到 `price_input`（`billing.go:503-505`） | 无兜底（输出免费） |
| `price_cache_hit <= 0` 兜底 | 不涉及 | 回退到 `price_input`（`usage_dao.go:337-340`） |
| 双零提前返回 | 有（`billing.go:492-496` 判 `inPrice<=0 && outPrice<=0` → 返回 0） | 无 |

缓存项的差异是**有意的保守**（`billing.go:460-463` 明说「按「全部未命中」的最高价计…真实扣费用的是落库时那个含缓存命中的 cost_total，两者差额就是「预检保守、扣费精确」的体现，不是偏差」），**不是缺陷**。

**★ P1-3：单价兜底方向的差异，在「只配了 `price_cache_hit`」时导致预检 fail-open**

逐条代入「`price_input = 0`、`price_output = 0`、`price_cache_hit = 3.0`」的模型：

**预检侧**（`estimateBalanceCost`）：
1. `inPrice = 0`、`outPrice = 0` → 命中 `billing.go:492` 的 `if inPrice <= 0 && outPrice <= 0` → **提前 return 0**。
2. 回到 `precheckBalance`（`billing.go:692-696`）：`est = 0`，`chainPriced = chainHasPricedModel(cands)` —— 而 `chainHasPricedModel`（`billing.go:631-633`）判定条件是 `PriceInput > 0 || PriceOutput > 0 || PriceCacheHit > 0` → **price_cache_hit > 0 → chainPriced = true**。
3. `est <= 0 && !chainPriced` 为 false → **不提前放行**，继续读余额。
4. `balancePreflight(est=0, cents, unlimited, false, priced=true)`（`billing.go:595-619`）→ `estCents <= 0` 且 `priced == true` → 走地板线判定 `balanceCents >= minSubcentSpendableCents`（100 分 = 1 元）→ **余额 ≥ 1 元就放行**。

**实际侧**（`priceUsage`）：
- `priceInput = 0` → 未命中部分 `uncached × 0 = 0`（免费）
- `priceCacheHit = 3.0` → 命中部分 `cached × 3.0`
- `priceOutput = 0` → 输出部分免费

所以**只要请求的输入全部命中缓存，实际扣费就非零**，而预检按「0 分」处理。

**净效果**：预检只要求「余额 ≥ 1 元」这一道地板线，而实际扣费可以远超这个数。余额 1.00 元的用户可以持续发大量请求直到余额耗尽 —— 预检本应在这里拦住。

**这确实是「预检与实际扣费口径漂移」的实例**，且方向是**漏放行**（钱的方向）。

**待确认**（影响严重度）：管理面是否允许「只配 `price_cache_hit`、不配 `price_input` / `price_output`」？

- `internal/admin/model_handler.go:274-297` 的 `Create` 路径用 `derefFloat(req.PriceInput)` 等，**没有做「至少一个 > 0」的校验**。
- `internal/admin/model_handler.go:379-381` 的 `Update` 路径同理（逐字段覆盖）。
- `BatchUpdateModelPrices`（`model_handler.go:534`）也逐字段应用。

→ 管理面**允许**只配 `price_cache_hit`。该路径**可达**。

严重度 **P1**（预检 fail-open，且是「钱」的口径）。

**修法方向**：把 `estimateBalanceCost` 与 `priceUsage` 收敛到**同一个**可导出的计价函数（`store.priceUsage` 目前是包私有，`billing.go:469-472` 的注释正是因为这个才复制了一份），并在两边使用**完全相同**的兜底规则。

### 2.7 `precheckBalance` 用的是未过滤的候选链

`main.go:1997`：`precheckBalance(w, r, db, logger, authCtx, ing, res.Candidates, codec)`

传的是 `res.Candidates`（**整条链**），而实际会打的候选是 `active`（经熔断筛选 `main.go:2024-2034` + 结构化输出筛选 `main.go:2050-2069` + 预算截断 `main.go:2070-2072` 之后的子集）。

`estimateBalanceCostChain`（`billing.go:527-538`）取整条链的**最大值**，注释说明这是有意的保守（`billing.go:516-524`：「按链首估会在「转移到了贵目标」时低估…取上界的代价是可能误拒」）。

**已验证无问题**：预检只会更严格，不会更松。

---
## 3. usageRecorder

### 3.1 架构

- 队列：`chan *store.UsageRecord`，容量 1024（`main.go:3067`、`main.go:3112`）。
- worker：1 个（`main.go:3070`），攒够 50 条或队列排空就发（`main.go:3085`、`main.go:3207-3218` 的 `drain` 循环用 `default` 非阻塞）。
- 落库：`CreateUsageRecordsBatched`（`main.go:3268`）→ **一个事务一条多值 INSERT**（`usage_dao.go:188-239`）。
- 扣费：**逐条**（`main.go:3293-3299`），每条一个独立事务。

批量与逐条的职责切分有充分论证（`main.go:3130-3152`）：批量只省提交开销，**计价仍逐条**（`usage_dao.go:274` 逐条调 `freezeUsageCost`），所以「扣的 = 报表的」这条不变量在批量下逐字不变。

### 3.2 ★ P0-2：`cost_pending` 重算只修报表，余额永久免单

这是本子域最严重的发现。

**链路**：

1. `freezeUsageCost` 查价失败 → 费用 0，`usage_state = 'cost_pending'` 随记录落库（`usage_dao.go:139-152`）。
2. `CreateUsageRecordWithCost` 返回 `cost = 0`（`usage_dao.go:139`、`usage_dao.go:157`）。
3. `usageRecorder.charge(rec, 0)` → `ChargeBalance(userID, 0, requestID)` → `amountYuan <= 0` → **完全 no-op**（`balance_dao.go:313-315`）。
   → **这一次调用用户没被扣钱。**
4. 5 分钟一轮的 `RecomputePendingCosts`（`main.go:766-801`）→ `RecomputeCost`（`usage_dao.go:489-521`）→ **只 UPDATE 两列**：

   ```go
   res, err := s.db.ExecContext(ctx,
       `UPDATE usage_records SET cost_total = ?, usage_state = ? WHERE id = ?`,
       cost, state, recordID)
   ```

   （`usage_dao.go:510-512`）

   **它不调用 `ChargeBalance`，不写 `balance_charges`，不碰 `users.balance_remainder`。**

5. 幂等键**没有**把这条路径锁死：`ChargeBalance` 的 `amountYuan <= 0` 提前 return 发生在占位 INSERT **之前**（`balance_dao.go:313-315` 在 `if requestID != ""` 占位块 `balance_dao.go:324-341` 之前），所以 `balance_charges` **没有行**。**存在一条补救路径**（用同 `request_id` 调 `ChargeBalance` 不会命中幂等快路径）。

   但 —— **代码库里没有任何地方做这个补救**。`rg -n "ChargeBalance" -g "*.go" .`（排除测试）只命中 `cmd/gateway/main.go:3408` 一处生产调用点。`RecomputeCost` 是唯一消费 `cost_pending` 的地方，而它不扣费。

**净效果**：**一次读池抖动（SQLITE_BUSY / 磁盘瞬时故障）= 该请求永久免单**。用户白嫖了上游 token，报表上显示 0 元，余额没动，没有任何 ERROR（`freezeUsageCost` 只打一条 WARN，`usage_dao.go:405-407`）。

**这与设计意图直接冲突**。`usage_dao.go:356-372` 的注释白纸黑字：

> 旧实现…main.go 的 charge 用同一个 cost 扣费 → 费用 0 → **该请求完全免单**
> …
> 加上这个状态后，「哪些记录待重算」变成一条**可查询**的事实：后台任务能周期性地把它们捞出来重算（见 RecomputePendingCosts），而不必依赖人工记得去调某个接口。

**只实现了「重算报表」，没实现「重算余额」** —— 这是典型的「机制建了一半」。和 `main.go:1868-1884` 记录的那个「key 级原子、用户级裸读」的历史缺陷是同一种形态。

严重度 **P0**：直接资金损失（用户免单），且无声无息。触发条件是 DB 抖动，不需要攻击者。

**修法方向**：`RecomputeCost` 在把 `cost_total` 从 0 改成非 0 时，必须走一次 `ChargeBalance`（用同一条记录的 `request_id` / `user_id`）。需额外确认：重算出的费用可能超过预检时的余额 → 走 `ChargeBalance` 的余额守卫即可，欠费时它会记 `balance_pending` 零头（`balance_dao.go:518-526`）并报 `ErrInsufficientBalance`。

**待确认**：`RecomputeCost` 的生产调用点只有 `main.go:773`（`rg` 确认），且 `RecomputeCost` 也会把「重算后仍是 0」的记录复位为 `reported`（`usage_dao.go:504-509`）—— 这类记录（未配价模型）确实不该扣费，与本条无冲突。修的时候需要区分这两种情况。

### 3.3 charge 与 record 的顺序

`main.go:3293-3299`：

```go
for i, rec := range batch {
    var cost float64
    if i < len(costs) {
        cost = costs[i]
    }
    u.charge(rec, cost)
}
```

**先落库、后扣费**，且「扣的 = 报表的」在结构上成立：`costs[i]` 就是 `usageInsertArgs` 里写进 `cost_total` 列的**同一个变量**（`usage_dao.go:228` → `usage_dao.go:301`），不是重算一次。

单条路径同理（`main.go:3454-3459` 的同步回退、`main.go:3321-3336` 的逐条降级）。

**★ P1-4：落库与扣费之间存在无对账的窗口**

- 落库成功、扣费前崩溃 → 记录在库、`cost_total` 非 0、**余额未扣**。差额永久存在。
- 落库失败 → 不扣费（`usage_dao.go:153-158` 返回 0），口径一致。

全仓**没有对账任务**比对 `usage_records.cost_total > 0 AND status='ok'` 与 `balance_charges` 的缺失项（`rg` 确认：`balance_charges` 只出现在 `balance_dao.go`、`store.go`、`topup_dao.go`、`balance_pk_migration.go`，无对账逻辑）。

单次窗口极小（一次事务），但在高并发 + DB 抖动下会累积。严重度 **P1**（与 P0-2 共享「无对账」这个根因；两者叠加就是「用量与余额系统性分叉且无人知晓」）。

**修法方向（与 P0-2 合并）**：加一个对账/补扣任务，扫 `status='ok' AND cost_total > 0` 但 `balance_charges` 无对应 `(request_id, user_id)` 的记录，补扣。

### 3.4 flush 失败重试与 markFlushFailed

`flush`（`main.go:3260-3301`）三级降级：

1. 整批插入 + `usageFlushMaxAttempts=3` 次重试，固定 20ms 退避（`main.go:3096-3098`）。
2. 仍失败 → `flushOneByOne` 逐条（`main.go:3318-3339`）。
3. 单条失败 → 记带 `request_id` 的 ERROR（`main.go:3331-3333`），返回 false。

`stillPending` 留在 worker 手里而非插回有界队列（`main.go:3160-3168` 的详细论证：插回会在队列满时被 `default` 静默丢弃，那正是要修的缺陷本身）。

**★ P1-5：`flushOneByOne` 的主键冲突分支会漏扣费**

`main.go:3323-3328`：

```go
if store.IsUniqueViolation(err) {
    // 这一条上一轮已经落库成功。不再 charge：余额扣费本身以
    // (request_id, user_id) 幂等，再调一次是空操作，但跳过它
    // 让「已落库」与「已扣费」这对关系保持单一发生点。
    continue
}
```

这个推理是：**「主键冲突」⇒「上一轮已落库」⇒「已扣过费」**。第二步不成立。

`CreateUsageRecordsBatched`（`usage_dao.go:188-239`）在事务失败时**整批回滚**（`usage_dao.go:231-234`），返回全 0 费用，**不扣任何一条**。此时库里**没有这批记录**。

`flush` 的批路径（`usage_dao.go:231-238`）：

```go
if _, err := tx.ExecContext(ctx, sb.String(), args...); err != nil {
    return make([]float64, len(recs)), err   // 整批回滚
}
if err := tx.Commit(); err != nil {
    return make([]float64, len(recs)), err   // ← Commit 返回错误，但事务可能已提交
}
return costs, nil
```

`tx.Commit()` 在 SQLite commit 阶段返回 I/O 错误时，事务**可能已经提交**。此时 `flush` 认为失败 → 下一轮批路径会因主键冲突而**整批回滚**（`usage_dao.go:231`）→ 降级逐条 → 每条**都命中主键冲突** → 全部 `continue` → **一条都不扣费**。

净效果：**这批记录全部落库了（报表上有费用）、余额一分没扣**。

另一个更简单的可达路径：批路径因 `rec.ID` 重复而失败（`usage_records.id` 是主键，`store.go:334`），而这批记录从未成功落库 → 逐条写时每条都冲突 → 全部 `continue` → 全部不扣费。`rec.ID` 重复在 `usage.record` 的同步回退与 worker 路径处理**同一个 `*UsageRecord` 指针**时是可达的（`main.go:3225` 的 `stillPending` 保留原指针，ID 不变 —— 这正是 `main.go:3311-3313` 描述的幂等机制）。

严重度 **P1**（确定的漏扣费路径，与 P0-2 共享「报表与余额分叉」的根因）。

**修法方向**：`flushOneByOne` 的唯一约束冲突分支不应无条件跳过 charge。幂等应该由 `balance_charges` 的 `(request_id, user_id)` 承担 —— **`ChargeBalance` 本身已经是幂等的**（`balance_dao.go:331-341`：`INSERT OR IGNORE` 占位，命中即 `return tx.Commit()`，不碰余额）。所以可以直接去掉这个 `continue`，让 `ChargeBalance` 自己判幂等。

**待确认**：`main.go:3324-3326` 的注释说「让『已落库』与『已扣费』这对关系保持单一发生点」。这个设计意图是好的，但**实现上把幂等责任放错了层** —— `ChargeBalance` 已经幂等，外层再跳过一次就成了双重保护，代价正是上面这条漏扣路径。

### 3.5 ★ P0-3：`reserved_tokens` 是终身预占，进程崩溃后永久冻结

`access_keys.reserved_tokens`（`store.go:234`）与 `users.reserved_tokens`（`store.go:285`）是**在途预占**列，判定时与 `used` 相加（`key_dao.go:476`、`user_dao.go:594`）。

预占由 `ReserveQuota` / `ReserveUserQuota` 累加（`key_dao.go:484`、`user_dao.go:600`），由 `ReleaseQuota` / `ReleaseUserQuota` 退回（`key_dao.go:512`、`user_dao.go:622`）。两者都是 `MAX(0, ...)` 夹下界。

**问题：没有任何复位路径。**

`rg -n "reserved_tokens" -g "*.go" -g "*.sql" .`（排除测试）的全部命中：

- `key_dao.go:484`（+）、`key_dao.go:512`（-）
- `user_dao.go:600`（+）、`user_dao.go:622`（-）
- `store.go:234`、`store.go:285`（建表）、`store.go:753`、`store.go:759`（迁移补列）
- `main.go:1816-1827`、`main.go:1866-1918`（注释与调用）
- `key_dao.go:426-434`、`user_dao.go:543`（注释）

**没有任何一条「启动时把 `reserved_tokens` 归零」的语句。** `UpdateAccessKey`（`key_dao.go:318`）只更新 `name / enabled / quota_tokens / rpm_limit / tpm_limit / ...`，不碰 `reserved_tokens`。

后果：

- 网关进程被 SIGKILL、OOM、容器驱逐、崩溃（panic 未被 `Recovery` 捕获的 goroutine）时，所有在途请求的 `reserved_tokens` **永久留在库里**。
- 由于预占是**终身累计**口径的一部分（判定式 `used + reservedNow + est > quota`，`key_dao.go:476`），这些幽灵预占会**永久占用**该 key / 该用户的终身额度。
- 例如：key 配额 100000，一个大请求预占 50000 后进程被强杀 → 该 key 的可用额度**永久减少 50000**，直到人工改库。

而 `used_tokens` / `user_usage_totals` 是由 `usage_records` 的触发器累加的（`store.go:410-414`、`usage_archive.go:197-204`），它们是**已发生**的事实，保留是对的。`reserved_tokens` 是**在途**状态，进程重启后所有在途请求必然已不存在 —— 归零才是唯一正确的语义。

这是本子域**唯一**的系统性 P0 之外的资金类 P0。

严重度 **P0**：一次崩溃 = 永久额度损失（需人工改库才能恢复），且完全无信号。

**修法方向**：在 `store.Open` / `migrate()` 完成后，把 `access_keys.reserved_tokens` 与 `users.reserved_tokens` 归零。理由：进程启动时不存在任何在途请求。更稳妥的方案是给预占加 TTL（记录预占时间戳，超过 N 分钟视为失效）。

**待确认**：`migrate()` 的调用点与是否适合放这个归零（它需要一条写事务，且 `migrate()` 已经在跑多条 DDL）。

### 3.6 ★ P1-6：`charge` 吞掉所有错误 → 永久少记账

`cmd/gateway/main.go:3408-3422`：

```go
err := u.db.ChargeBalance(context.Background(), rec.UserID, costYuan, rec.RequestID)
switch {
case err == nil:
case errors.Is(err, store.ErrInsufficientBalance):
    u.logger.Error("charge failed: insufficient balance (precheck should have prevented this)", ...)
default:
    u.logger.Error("failed to charge balance", ...)
}
```

**两个分支都只是记一条 ERROR，然后函数返回 —— 没有任何重试、补偿或落盘待扣队列。**

结合 §3.3 的「先落库后扣费」，这构成一条完整的漏账链：

1. `usage.record` 把记录排队（或同步写库）。
2. worker 落库成功（`cost_total` 已固化，报表上**有**这笔钱）。
3. `charge` 调 `ChargeBalance` 失败 —— 可能是 `SQLITE_BUSY`、磁盘瞬时故障、连接被 `db.Close()` 关掉。
4. 记一条 ERROR，**放弃**。余额永远没扣。

**这条路径比 P0-2 严重的地方在于**：P0-2 至少还有 `cost_pending` 标记 + 5 分钟一轮的重算任务（虽然重算不扣钱）；而这里**连一个待重试的标记都没有**。`usage_records` 里那条记录看起来完全正常（`usage_state='reported'`、`cost_total` 非 0、`status='ok'`），**没有任何字段表明它没扣费**。事后对账都无法从数据本身发现。

唯一的线索是日志里一条 `failed to charge balance` ERROR —— 而日志会轮转。

严重度 **P1**（确定的钱款漏收路径；DB 抖动即可触发，不需要攻击者。之所以不是 P0，是因为它**至少记了 ERROR**，与 P0-2 的「静默」有别，且触发需要 DB 故障）。

**修法方向**：`charge` 失败时把这条记录写进一张待扣费表（类似 `balance_pending` 的设计），由后台任务重试。`ChargeBalance` 已经幂等，重试安全。

**已验证无问题**：`charge` 的三道前置判据是合理的 —— 管理员短路（`main.go:3380-3382`）、非成功不扣（`main.go:3384-3386`）、空 `request_id` 不扣并记 ERROR（`main.go:3394-3399`）。「宁可漏扣不可重复扣」的选择有明确论证（`main.go:3387-3393`），且 `request_id` 恒非空（`server.Middleware` 每个请求生成，`internal/server/server.go:38-42`）。

### 3.7 shutdown drain

`main.go:856-886`：

1. `srv.Shutdown(10s)`（`main.go:856-858`）。超时 → `srv.Close()` 强制掐断在途连接（`main.go:869`）。
2. `usage.wait(drainCtx)`，drain 预算 5s（`main.go:876-877`）。
3. `wait` 关闭队列 → worker 冲完手上和队列里的 → 返回 `!HasPendingUsage()`（`main.go:3498-3516`）。
4. 返回 false → 记 ERROR「records were lost during shutdown」（`main.go:882-885`）。

**已验证无问题**：

- 队列关闭与 `record` 的竞争由 `mu` 保护（`main.go:3439-3449`），不会 `send on closed channel` panic。`usage_shutdown_test.go:50-103` 有回归测试钉住。
- 关停后到达的 `record` 走同步写（`main.go:3454`），不丢。
- `wait` 的返回值语义正确（`main.go:3515`：`return !u.HasPendingUsage()`）。
- `main.go:864-866` 的注释说明了「先 close 队列再掐 handler」的顺序意图与 `mu` 兜底的双保险。

**P2-2：5s drain 预算到期后，那批 `stillPending` 记录永久丢失。**

`wait` 的 `ctx.Done()` 分支（`main.go:3511-3514`）**只是放弃等待**，不阻止 worker 继续跑。随后 `main` 返回 → `defer db.Close()`（`main.go:200`）执行 → 手上那批记录写不进去 → **用户免单，且 `usage_records` 里没有对应行**。

同时，worker 的重试循环用的是 `ctxBackground()`（`main.go:3175`、`main.go:3195`）—— **永不取消**。它会继续 `time.Sleep(20ms)` + 重试写，拿到 "database is closed" 错误，形成直到进程退出的 goroutine 泄漏 + 日志噪声。

严重度 **P2**（关停场景，有 ERROR 日志）。

**待确认**：`db.Close()` 与 worker 并发的具体表现需实际跑一次关停压测确认日志噪声程度。

### 3.8 `HasPendingUsage` 的语义

`HasPendingUsage`（`main.go:3483-3487`）只反映 `flushFailed` 标志，**不反映队列长度**。若 worker 正在处理一批、尚未 flush 完成（`flushFailed` 仍为 false），`wait` 超时路径下可能返回假阴性（报告为「已排空」但实际未落库）。

不过 `wait` 的实现是「关闭队列 → `wg.Wait()` → 检查标志」（`main.go:3499-3515`），`wg.Wait()` 覆盖了 worker 退出，**只有 ctx 超时**才可能假阴性。

**P3**。

---
## 4. 流式路径 attemptStream

### 4.1 三个超时的关系

| 超时 | 取值来源 | 覆盖范围 | 位置 |
|---|---|---|---|
| `firstTokenTimeout` (TTFT) | 设置页 → config（默认 60s） | 拿到流对象之后、首个事件之前 | `main.go:2646`、`main.go:2661` |
| `streamIdleTimeout` | 设置页 → config（默认 120s） | 两个上游事件之间 | `main.go:2710`、`main.go:2724` |
| `totalRequestBudget` | 由前两者 + `failoverMaxTargets` 推导 | 整个请求（跨所有 attempt） | `main.go:1973-1976`、`main.go:2539-2565` |
| `responseHeaderWait` | 硬编码 60s | 拨号 + 等响应头 | `main.go:2573` |
| `nonStreamTimeout` | 设置页 → config（默认 300s） | 非流式单跳 | `main.go:2482` |

关系式（`main.go:2542-2564`）：

```
perAttempt = max(nonStreamTimeout, firstTokenTimeout + responseHeaderWait) + perAttemptSlack
budget     = perAttempt × (failoverMaxTargets + 1)
budget     = clamp(budget, 30s, 30min)
```

所以**总预算恒大于单跳预算**，且不存在「总超时配得比单跳还短」这种自相矛盾的配置（`main.go:2519-2526` 的论证）。

**已验证无问题**：三者层级关系正确，无配置冲突可能。

SDK 层还有一个独立的 stream idle watchdog（`rosetta@v1.2.0/stream.go:318-338`），但网关**没有启用**它（`internal/upstream` 内 `rg "streamIdle|WithStreamIdle"` 无命中），所以实际只有网关自己的 `idleTimer`。**已验证无问题**（不会双重超时）。

### 4.2 TTFT 看门狗

`main.go:2646-2692`：

```go
ttftTimeout := firstTokenTimeout(snap, cfg)
stream, err := client.ChatStream(ctx, upstreamReq)
if err != nil { return outcomeFromErr(err) }
defer stream.Close()

var ttftTimedOut atomic.Bool
ttftDone := make(chan struct{})
ttftTimer := time.AfterFunc(ttftTimeout, func() {
    ttftTimedOut.Store(true)
    _ = stream.Close()
    close(ttftDone)
})
gotFirst := stream.Next()

ttftFired := !ttftTimer.Stop()
if ttftFired { <-ttftDone }

if !gotFirst || ttftTimedOut.Load() {
    if cerr := stream.Err(); cerr != nil { return outcomeFromErr(cerr) }
    if ttftTimedOut.Load() {
        return attemptOutcome{eligible: true, credCooldown: 60 * time.Second,
            statusCode: http.StatusGatewayTimeout, code: "upstream_timeout",
            message: "upstream first-token timeout"}
    }
    return attemptOutcome{eligible: true, statusCode: http.StatusBadGateway,
        code: "upstream_error", message: "upstream returned empty response"}
}
```

**已验证无问题**：

- `ttftTimer.Stop()` 的返回值被检查（`main.go:2673`），`ttftDone` channel 消除了「回调已起跑但 `stream.Close()` 未落地」的窗口（`main.go:2657-2660` 的注释与 `main.go:2675` 的 `<-ttftDone`）。这避免了在流刚建立就被关掉的情况下写出「空但 ok」的假正常流。
- TTFT 超时发生在**写任何字节之前** → 未提交 → 外层可转移（`main.go:2694` 的注释）。
- **没有给 `ChatStream` 套带超时的子 context** —— `main.go:2621-2637` 用实测数据论证了那样做会掐断正常的长流（SDK 内部 `WithCancel(ctx)` 后把 ctx 交给 `http.Do`，ctx 一取消就关响应体）。这个决定是对的。

**P2-3：TTFT 超时时 `credCooldown` 无条件 60s，且 `stream.Err()` 优先于超时判定。**

`main.go:2678-2681` 的 `if cerr := stream.Err(); cerr != nil { return outcomeFromErr(cerr) }` 在 `ttftTimedOut.Load()` 判定**之前**。

TTFT 超时时 `stream.Close()` 已经把流标记为 `abortedByCaller` 并 `releaseLocked`（`rosetta@v1.2.0/stream.go:444-458`），此时 `stream.Err()` 很可能返回一个传输错误 → `outcomeFromErr(cerr)` → `outwire.FailoverEligible` 对 `TransportError` 返回 true（`internal/outwire/errors.go:306-309`），`CredentialCooldown` 返回 60s（`errors.go:337-339`）。

结果：TTFT 超时**确实**被当成可转移失败处理（`eligible: true`），这与设计意图一致。唯一的偏差是 `statusCode` 变成了 `MapUpstreamError` 对 `TransportError` 的映射（`errors.go:102-106` → `http.StatusGatewayTimeout, "upstream_timeout"`）—— **恰好与 TTFT 超时自己的映射相同**，所以客户端看到的错误码不变。

**已验证无问题**（这条撤回自最初的怀疑：状态码映射一致，无实际偏差）。

**P2-3（保留部分）**：`credCooldown: 60 * time.Second` 意味着**任何一次 TTFT 超时都会把这把上游凭据踢出健康轮换 60 秒**（`main.go:2205-2207` → `pool.MarkCredentialCooldown`）。TTFT 超时可能是上游偶发慢（而非坏了），一次就冷却 60s 对单 key provider 而言是相当重的惩罚。**待确认**：这是否是产品期望（对比 `main.go:2685` 显式写了 `credCooldown: 60 * time.Second`，看起来是刻意的）。

### 4.3 ★ P1-1：客户端中断时，usage 恒为零值 → 用户免单**且绕过终身 token 配额**

`main.go:2809-2829` 的流式错误分类：

```go
if err := stream.Err(); err != nil {
    switch {
    case errors.Is(err, rosetta.ErrStreamTruncated):
        status = "truncated"
        ...
    case errors.Is(err, rosetta.ErrStreamOverflow):
        status = "overflow"
        ...
    case isClientGone(ctx, err):
        status = "canceled"
        logger.Info("client disconnected mid-stream", ...)
    default:
        status = "error"
        errorCode = "upstream_error"
        ...
    }
}
```

随后 `main.go:2853-2878` 落库，`Status: "canceled"`。

**问题在 `lastUsage` 的来源**。`lastUsage` 只在 `ev.Type == rosetta.EventMessageEnd` 时赋值（`main.go:2795-2800`）：

```go
consume := func(ev *rosetta.Event) {
    lastEventAt.Store(time.Now().UnixNano())
    idleTimer.Reset(idleTimeout)
    heartbeatTicker.Reset(heartbeatInterval)
    if ev.Type == rosetta.EventMessageEnd {
        if ev.Usage != nil { lastUsage = *ev.Usage }
        stopReason = ev.StopReason
    }
    sink.Event(ev)
}
```

客户端中断时，流被取消，**上游不会发出 `EventMessageEnd`** → `lastUsage` 保持**零值**。

于是：

- `usageStateFor(lastUsage)` = `"missing"`（`main.go:2972-2977`）
- `cost_total` = 0（`freezeUsageCost` 用 input=cached=output=0 算出 0，`usage_dao.go:412-413`）
- `charge` 因 `rec.Status != "ok"` 提前 return（`main.go:3384-3386`）

**但上游在这段时间里已经生成了 token 并且已经计费了。**

对比：`truncated` / `overflow` 状态在 SDK 层**能**拿到 usage（截断流仍发 `endEvent()`，`provider_openai_chat.go:549`），所以那些路径 `cost_total` 是对的。**只有 `canceled` 这条路径彻底丢失**。

更严重的是**可被主动利用**：

1. 客户端发一个请求，收到第一个 token 后立刻断开（`r.Context()` 取消 → 上游调用被取消）。
2. 上游已经生成了 N 个 token 并计费。
3. 网关记 `status="canceled"`、`usage_state="missing"`、`cost_total=0`、余额不动。
4. 重复 N 次。

同时，**这条路径还绕过了 key 级的终身 token 配额**：

- `trg_update_used_tokens` 触发器累加 `NEW.total_tokens`（`store.go:410-414`），这里是 0 → **`access_keys.used_tokens` 不涨**。
- `rate.releaseQuota()`（`main.go:2891`）把 `est` 预占**全额退回**（`rateCommit.releaseQuota`，`main.go:1564-1590`）→ 终身配额预占也归零。

→ **用户可以用「点停止」无限消耗上游 token，既不扣钱、也不占配额。**

这与 `main.go:3344-3360` 里明确写的口径直接冲突：

> 明确**不扣**的形态（它们都不该收用户钱）：上游错误与不可转移错误、流式截断/溢出/中断、客户端主动断开（canceled）、以及根本没碰上游就被拒的请求
> 与 cost_total 的差别要记牢：cost_total **无论成败都记**（那是网关的上游成本），扣费**只看成功**（那是用户的应收）。

「扣费只看成功」是既定口径（客户端主动断开确实不该收用户钱，这是合理的）。**但「cost_total 无论成败都记」这一半没有实现** —— canceled 路径的 `cost_total` 是 0 而不是真实值。**网关的上游成本在这条路径上被完全漏记**，报表上的「上游成本」低于真实值。

所以这条有两个层面：
- **报表层面**：网关上游成本被系统性低估（每一条 canceled 记录都是 0）。
- **配额层面**：中断的请求**不占用**终身 token 配额（`used_tokens` 不涨 + 预占全额退回）→ 配额被绕过。

严重度 **P1**（确定的少记用户钱 + 绕过配额，且可主动利用；不是 P0 因为它不涉及崩溃/越权/资金直接损失到他人）。

**待确认**：SDK 侧在 abort 时能否给出部分 usage？`rosetta@v1.2.0/stream.go:532` 的 `usage, err = s.usage, s.err` 表明 `s.usage` 只从 `EventMessageEnd` 派生（`stream.go:606`：`s.usage = *ev.Usage`），abort 路径不产生该事件 → 零值。**结论成立**。网关读的 `lastUsage` 与 SDK 的 `stream.Usage()` 同为零。

**修法方向**：至少要让 canceled 路径**占用配额**（不退 `est` 预占，或让 `total_tokens` 落一个保守下界），使「点停止」不能成为绕过配额的通道。费用是否收是产品口径，但 `cost_total`（网关成本）**必须**记。

### 4.4 `isClientGone` 的判据与预算到期的归因

`main.go:3002-3008`：

```go
func isClientGone(ctx context.Context, err error) bool {
    if errors.Is(err, context.Canceled) { return true }
    return errors.Is(ctx.Err(), context.Canceled)
}
```

只认 `context.Canceled`，不认 `DeadlineExceeded`（注释在 `main.go:2998-3001`）。

**★ P2-4：总预算到期被记成「上游故障」，并累计目标熔断计数**

预算到期时 `ctx.Err() == context.DeadlineExceeded`：

- `isClientGone` 返回 false → 流式路径落 `status="error"`, `errorCode="upstream_error"`（`main.go:2824-2828`）。
- 外层 `main.go:2202`：`if out.eligible && !isClientGone(r.Context(), nil)` → 为真 → **`pool.RecordTargetFailure(cand.TargetID, threshold)`**（`main.go:2203`）→ 累计目标熔断计数。

也就是说：**网关自己的总预算到期会被算成「这个上游坏了」**。连续 3 次预算到期（`failoverFailureThreshold` 默认 3，`internal/config/config.go:537-542`）就把健康目标熔断 60s（`upstream.go:601-606`）。

`main.go:2196-2201` 的注释说：

> 判据只认 Canceled：请求 ctx 上还挂着 totalRequestBudget 的超时，用 ctx.Err()==nil 会把「预算到期」也一起放过去 —— 那是网关侧的事件，但请求确实已经打到了这个目标，用它当「上游健康」的证据同样不成立，所以这里只排除真正的客户端取消。

这个推理有个逻辑缺口：「预算到期不能当作**上游健康**的证据」（所以不该 `RecordTargetSuccess`）是对的；但当前实现同时「把预算到期当作**上游不健康**的证据」（所以 `RecordTargetFailure`）。两个方向都用了。

同样地，`main.go:2244` 的 `clientGone := isClientGone(r.Context(), r.Context().Err())` 在预算到期时为 false → 链耗尽时记 `status = "error"`（`main.go:2272`）而非 `canceled`，`error_code` 保留为 `upstream_error` —— 网关自身的超时在报表里与上游故障混为一谈。

严重度 **P2**（不是资金损失，但会把网关自身的问题归因到上游，导致无谓的熔断与误导性的报表）。

**修法方向**：把 `DeadlineExceeded` 与 `Canceled` 分开处理 —— 预算到期应记为独立的 `status`（如 `"timeout"`），且**不**计入 `RecordTargetFailure`。

### 4.5 心跳

`main.go:2736-2775`：

- `heartbeatInterval = idleTimeout / 2`，`<= 0` 时兜底 1s（`main.go:2739-2742`）—— 防止 `time.NewTicker(0)` panic（`main.go:2736-2738` 说明了原因：此刻 SSE 响应头已写出，收不回来）。
- 心跳 goroutine 由 `quit` channel + `WaitGroup` 保证在 handler 返回前退出（`main.go:2756-2775`）—— 避免写到已结束的响应（`main.go:2751-2755` 的注释说明了这个具体危害）。
- 只有真的空闲时才发（`consume` 里 `heartbeatTicker.Reset`，`main.go:2794`）。

**已验证无问题**：

- 心跳的并发写由各 sink 内部的锁串行化：`SSEWriter.mu`（`openai_chat.go:84`、`openai_chat.go:115`）、`AnthropicSSE.mu`（`anthropic.go:217`、`anthropic.go:273`）、`ResponsesSSE.mu`（`openai_responses.go:39`）。`internal/outwire/sink.go:16-17` 的接口契约明确要求「实现必须自行串行化写底层 writer」。
- `lastEventAt` 在 `idleTimer.Reset` **之前**更新（`main.go:2792-2793`），符合 `main.go:2720-2721` 的注释要求。

**P3**：`main.go:2794` 的 `heartbeatTicker.Reset(heartbeatInterval)` 在**每个事件**上都执行。高频事件下 `time.Ticker.Reset` 的开销虽小但不为零，且语义上等价于「永不到期」。**行为正确**，只是略显浪费。

### 4.6 ★ P2-5：SSE 写错误被完全丢弃 → 残缺流被记为成功 / handler 泄漏

`internal/outwire/write_deadline.go:10-17` 的注释说明了要解决的问题：

> 已有 idle 看门狗管的是「上游多久没给数据」。它看不见另一类卡死：上游正常吐字，但下游客户端挂死不读（休眠、断网、被防火墙黑洞）。此时内核发送缓冲填满后 Write 会一直阻塞，handler 与它占用的上游流一起泄漏 —— 上游还在按 token 计费，网关却连看门狗都跑不到（主循环卡在写里）。

`internal/outwire/openai_chat.go:114-127` 的 `WriteEvent`：

```go
func (sw *SSEWriter) WriteEvent(event, data string) error {
    sw.mu.Lock()
    defer sw.mu.Unlock()
    setWriteDeadline(sw.rw)
    defer clearWriteDeadline(sw.rw)
    if event != "" {
        fmt.Fprintf(sw.w, "event: %s\n", event)
    }
    fmt.Fprintf(sw.w, "data: %s\n\n", data)
    if sw.flusher != nil {
        sw.flusher.Flush()
    }
    return nil
}
```

**`fmt.Fprintf` 的返回错误被完全忽略**，`WriteEvent` 永远返回 nil。全部七个写点都是这样：

- `openai_chat.go:120`、`openai_chat.go:122`（WriteEvent）
- `openai_chat.go:136`（WriteComment）
- `anthropic.go:255`（writeRaw）
- `anthropic.go:277`（Keepalive）
- `openai_responses.go:131`（writeEvent）
- `openai_responses.go:504`（Keepalive）

**两个后果**：

**(a) 写超时后流被静默截断，却仍记 `status="ok"`。**

写 deadline 生效时，`Fprintf` 在 30s 后返回超时错误 → **错误被丢弃** → `WriteEvent` 返回 nil → `consume`（`main.go:2801`）认为写成功 → 继续跑 → 下一次 `Fprintf` **立即**返回错误（deadline 还在）→ 事件被静默丢弃 → 流继续「正常」跑完 → `status` 保持 `"ok"`（`main.go:2779` 初始化，`main.go:2899` 的 `success: status == "ok"`）→ **`usage_records` 记一条 `status="ok"` 的成功记录，客户端却收到一个残缺的流**。

对下游是「假成功」；对上游是「网关以为成功」；对报表是「成功率虚高」。

**(b) 写 deadline 不支持时 handler 可永久阻塞在写里。**

`write_deadline.go:35-46`：

```go
rc := http.NewResponseController(w)
if err := rc.SetWriteDeadline(time.Now().Add(writePerFrameTimeout)); err != nil {
    if !errors.Is(err, http.ErrNotSupported) {
        slog.Debug("stream write deadline not supported", "error", err)
    }
}
```

若 `SetWriteDeadline` 返回 `ErrNotSupported`（或在 `setWriteDeadline` 里失败），`Fprintf` 永久阻塞 → handler 永久卡在 `sink.Event(ev)` → `consume` 里的 `idleTimer.Reset` 永远跑不到 → **idle 看门狗形同虚设** → 上游流与 token 一起泄漏。这正是那段注释想消灭的场景，但只解决了「写超时」，没解决「写错误不被处理」。

严重度 **P2**（不是资金损失，但导致「残缺流被记为成功」+「handler 与上游 token 泄漏」；写 deadline 在生产环境通常支持，所以 (b) 多为理论问题，(a) 是现实的）。

**修法方向**：把写错误一路传回 `consume` / `attemptStream` —— `StreamSink.Event` 返回 error，`attemptStream` 在收到写错误时把流按 `status="error"` 收尾并正常落库。

### 4.7 截断 / 溢出时的记账

`main.go:2811-2818`：`ErrStreamTruncated` → `status="truncated"`；`ErrStreamOverflow` → `status="overflow"`。

这两条路径**能拿到 usage**（SDK 在截断时仍发 `endEvent()`，`provider_openai_chat.go:549`），所以 `cost_total` 正确。`charge` 因 `rec.Status != "ok"` 不扣用户钱（`main.go:3384-3386`），但 `cost_total` 记录了网关的上游成本。

**已验证无问题**（与 `main.go:3357-3359` 的口径完全一致）。

### 4.8 usage_state 词表不一致

落库侧有三种取值：

- `usageStateFor` 返回 `"missing"` / `"reported"`（`main.go:2972-2977`）
- `cost_pending`（`usage_dao.go:373`）
- 链耗尽路径硬编码 `"none"`（`main.go:2294`）

**P3**：`"none"` 只出现在链耗尽（根本没碰上游或未提交）的路径上，与 `usage_state` 文档里的含义（`reported`/`missing`/`cost_pending` 描述的是「上游有没有报 token 数」）不在同一维度上。读报表的人需要额外知道 `"none"` 的特殊含义。`main.go:2979-2991` 的注释自己承认了这个词表外溢（「散成裸字面量时改一处忘一处的后果很隐蔽」），但 `usageStateFor` 的引入只统一了新增的判读方。

---
## 5. 失败转移链

### 5.1 参数

- `failoverMaxTargets`：设置页 → config（默认 3）（`main.go:2441-2446`）。
- `failoverFailureThreshold`：设置页 → config（默认 3）（`main.go:2449-2454`）。

### 5.2 候选筛选的顺序与名额时机

1. 熔断筛选：`pool.TargetAvailable(cand.TargetID)`（`main.go:2027`）—— **纯查询，不占名额**（`upstream.go:539-551` 的注释详细论证了为什么副作用不能留在这里）。
2. 结构化输出筛选（`main.go:2050-2069`）。
3. 预算截断 `active = active[:limit]`（`main.go:2070-2072`）。
4. 循环内 `pool.ClaimTargetProbe(cand.TargetID)`（`main.go:2114`）—— **此时才占名额**。

`main.go:2017-2022` 的注释明确了这个时序设计：「这一步只做**查询**…名额统一在循环内、真的要发请求前由 ClaimTargetProbe 领取。」

**★ P2-6：链上可以出现「同一个上游被打了两次」**

`internal/store/route_target_dao.go:161-177` 的 `createRouteTarget`：

```go
_, err := ex.ExecContext(ctx,
    `INSERT INTO route_targets (id, route_id, provider_id, upstream_model_id, position, enabled, created_at)
     VALUES (?, ?, ?, ?, ?, ?, ?)`,
    t.ID, t.RouteID, t.ProviderID, t.UpstreamModelID, t.Position, enabled, t.CreatedAt)
```

`route_targets` 表（`store.go:371-380`）的唯一约束**只有 `id` 主键**。没有 `(route_id, position)` 唯一约束，也没有 `(route_id, provider_id, upstream_model_id)` 唯一约束。

**管理面可达性**：`internal/admin/route_target_handler.go:69-133` 的 `Replace` 端点接收整个 `targets` 数组，`built` 的 `Position: i` 由数组下标决定（`route_target_handler.go:117`），**没有任何重复检查** —— `validateTarget`（`route_target_handler.go:141-143`）只校验「provider 存在、上游模型存在且确属该 provider」（`ValidateRouteTargetRef`），**不检查链内是否重复**。

所以运维在管理面把同一个 `provider_id + upstream_model_id` 配到两个 position 是**完全可达**的。

后果链：

- `buildCandidates`（`internal/routing/routing.go:258-274`）产出两个候选，**TargetID 不同**（`t.ID` 是各自生成的主键，`route_target_dao.go:196-200`）但**上游相同**。
- 第一个失败 → `RecordTargetFailure(targetA)`，熔断计数记在 targetA 上（`upstream.go:583-607`）。
- 第二个候选的 TargetID 是 targetB，`TargetAvailable(targetB)` 为真（targetB 从未失败过）→ **同一个上游被打了第二次**。
- 且 `pool.GetAnyClient(cand.Provider.Slug)`（`main.go:2135`）每次都从健康凭据里加权随机选（`selectWeighted`，`upstream.go:673`），所以两次可能选到同一把 key。

上游因此被**重复计费两次**（第一次虽未向下游提交，但上游可能已生成并计费）。

严重度 **P2**（需要运维误配置才触发；不构成越权或资金直接损失，但重复计费 = 用户重复付费）。

**修法方向**：`Replace` 端点或 `ReplaceRouteTargets` 里加链内重复检查；或 `buildCandidates` 按 `(provider_id, upstream_model_id)` 去重。

### 5.3 探测名额的释放

`main.go:2223-2225`：

```go
if probeClaimed && !probeRecorded {
    pool.ReleaseTargetProbe(cand.TargetID)
}
```

`probeRecorded` 在三处置 true：`main.go:2139`（无健康凭据）、`main.go:2182`（已提交）、`main.go:2204`（未提交但 eligible）。未置 true 的只有「不可转移错误」与「客户端中断」两种形态（`main.go:2218-2222` 的注释）。

**已验证无问题**：

- 已记账的不重复归还（`halfOpen` 是共享 bool 而非计数，重复归还会错清并发请求刚领到的名额 —— `upstream.go:636-637` 的注释）。
- `RecordTargetFailure` 在 `threshold <= 0` 时**也会**归还（`upstream.go:585-589`），否则关了目标级熔断就会把目标永久占死。
- `RecordTargetSuccess` 也会清 `halfOpen`（`upstream.go:615-622`）。
- `TargetAvailable` 与 `ClaimTargetProbe` 之间的 TOCTOU 窗口（筛选与领名额之间隔着两次筛选）是**安全的** —— `ClaimTargetProbe` 内部原子判断（`upstream.go:564-579`），冷却中的目标不会真的被打。

**已验证无问题**：不存在「冷却中仍被选中」的窗口。

### 5.4 ★ P2-7：「该 provider 无健康凭据」被记成目标级失败

`main.go:2135-2149`：

```go
client, credID, cerr := pool.GetAnyClient(cand.Provider.Slug)
if cerr != nil {
    // 该目标当前无健康凭据：累计目标失败，换链上下一个。
    pool.RecordTargetFailure(cand.TargetID, threshold)
    probeRecorded = true // 名额由记账释放
    out = attemptOutcome{eligible: true, statusCode: http.StatusBadGateway,
        code: "upstream_error", message: "no available upstream provider"}
    routeTrace = appendRouteTrace(routeTrace, i+1, cand, "fail", "no_healthy_credential")
    ...
}
```

「该 provider 的所有凭据都在冷却」是**上游健康的表现**（之前的失败已经被正确惩罚过了，凭据冷却正是那个惩罚的结果），却被记成**目标级失败**，累计熔断计数。

连续 3 次这样的请求 → 目标熔断 60s（`upstream.go:601-606`）。而**凭据的冷却惩罚与目标的熔断惩罚叠加**了 —— 凭据本来就在冷却，又被记一次目标失败。

对比：客户端中断的路径**已经**处理对了（`main.go:2202` 的 `!isClientGone` 判据让中断不罚目标）。但「无健康凭据」这条在 `main.go:2136` 走的是**另一条分支**，在 `isClientGone` 判据**之前**，没有那个保护。

严重度 **P2**（可用性影响，非资金；单 key provider 会被自己的冷却惩罚连带熔断）。

### 5.5 `appendRouteTrace` 与 `attemptOutcome` 归类

`appendRouteTrace`（`main.go:2339-2352`）格式：`"%d:%s/%s -> %s"` + 可选 `":" + reason`。

**轨迹的落库点**：

| 位置 | 场景 |
|---|---|
| main.go:2117 | 熔断 skip |
| main.go:2142 | 无健康凭据 |
| main.go:2169 | 成功（committed + success） |
| main.go:2175 | 已提交 + 客户端取消 |
| main.go:2180 | 已提交 + 失败 |
| main.go:2213 | 未提交 + 可转移（`fail_switch`） |
| main.go:2215 | 未提交 + 不可转移（`fail_terminal`） |
| main.go:2877 | 流式已提交（`priorTrace` + 当前候选） |
| main.go:2951 | 非流式已提交 |
| main.go:2300 | 链耗尽 |

**已验证无问题（编号一致性）**：外层用 `i+1`（`i` 是候选在 `active` 中的下标），attempt 内用 `len(priorTrace)+1`。两者同步增长 —— 每发生一次 skip，`i` 递增且 `priorTrace` 也追加一条（`main.go:2117`），所以编号始终一致。

**★ P2-8：`appendRouteTrace` 的 nil 解引用不对称**

`main.go:2339-2346`：

```go
func appendRouteTrace(trace []string, attempt int, cand routing.Candidate, outcome, reason string) []string {
    provider, model := "?", "?"
    if cand.Provider != nil {
        provider = cand.Provider.Slug
    }
    if cand.UpstreamModel.ModelID != "" {   // ← cand.UpstreamModel 未判空
        model = cand.UpstreamModel.ModelID
    }
```

`cand.Provider` 判了空（`main.go:2341`），`cand.UpstreamModel`（`*routing.UpstreamModel`，`routing.go:121`）**没判**。

**当前不可达**：`buildCandidates` 保证 `UpstreamModel != nil`（`routing.go:269-271`：`m == nil || !m.Enabled { continue }`）；`Resolve` 的 `provider/model` 直连分支也保证非 nil（`routing.go:231-241`）。

**P3**（防御性缺口）：同一函数里两个同类型的指针字段，一个判空一个不判。

### 5.6 `attemptOutcome` 的分类语义

`main.go:2370-2393`：

- `committed`：已向下游写出内容（流式已发首字、或非流式写完响应）→ **绝不能回退去换目标**。
- `success`：本次 attempt 真的成功抵达上游（流式仅 `status=="ok"` 为真）。
- `clientGone`：客户端自己走了。
- `eligible`：这次失败值不值得转移。
- `credCooldown`：凭据该被踢出轮换多久。

**已验证无问题**：`committed` 与 `success` 的区分是正确的，且被三个调用点一致使用（`main.go:2166`、`main.go:2170`、`main.go:2178`；`main.go:2899` 的 `success: status == "ok"`）。

`main.go:2163-2165` 的注释说明了这个区分的价值：「只有真成功才记成功 —— 断流/溢出/上游错误虽已提交，却是失败，必须让目标熔断计数与凭据健康照常累计，否则故障永不转移。」

**已验证无问题**。

---
## 6. 优雅关闭

### 6.1 时序

```
SIGINT/SIGTERM                                    main.go:824-825
  ↓
close(shutdown) → 等剪枝 5s → 等费用重算 2s → 等探测 2s   main.go:832-853
  ↓
srv.Shutdown(10s)                                  main.go:856-858
  ├─ 成功 → 所有 handler 已返回
  └─ 超时 → srv.Close() 强制掐断在途连接            main.go:869
  ↓
usage.wait(5s)                                     main.go:876-882
  ↓
main 返回 → defer db.Close()                       main.go:200
```

`main.go:864-866` 的注释说明了顺序意图：「先 close 队列再掐 handler 的话，那条 handler 收尾时恰好撞上「队列已关」。现在 record 侧也有 mu 兜底（双保险），但把顺序修正过来才是它本来的意图。」

### 6.2 长流式请求被强制关闭时的费用处理

`srv.Shutdown` 10s 到期 → `srv.Close()` 掐断连接 → 在途 handler 被中断。

此时该流式请求的 `attemptStream`：

- `ctx` 被取消 → `stream.Next()` 返回 false → `stream.Err()` 返回 `context.Canceled` → `isClientGone(ctx, err)` 为 true（`main.go:3003`）→ `status = "canceled"`。
- 落库一条 `status="canceled"`、`usage_state="missing"`、`cost_total=0` 的记录（`main.go:2853-2878`）。
- `rate.releaseQuota()`（`main.go:2891`）退配额预占，**保留 TPM 预占**。

即：**上游已消耗的 token 完全没有记账，用户免单，且不占用终身 token 配额** —— 与 §4.3 是同一个根因（canceled 路径丢 usage）。

**已验证无问题**：`usage.record` 在关停后不会 panic（`mu` 保护，`main.go:3439`；测试 `usage_shutdown_test.go:50-103` 钉住）。

### 6.3 关停期间已预占配额的处理

`srv.Close()` 掐断在途 handler → handler 的 defer 链执行 → `attemptStream` 正常返回（不是 panic）→ `rate.releaseQuota()` 会被调用。

若 handler 卡在 `client.Chat`（`main.go:2920`），`ctx` 取消 → `client.Chat` 返回错误 → `outcomeFromErr` → 未提交 → 外层 `main.go:2202` 判定 → 然后 `main.go:2223` 归还探测名额 → `main.go:2227` break → 链耗尽路径 `main.go:2263` `rate.commit(0)` 释放预占。

**已验证无问题**：预占会被释放（前提是 handler 正常返回而非 panic —— 见 P0-3 关于进程级异常终止的情形，那里预占会永久残留）。

**待确认**：`srv.Close()` 是否会取消请求 ctx？`http.Server.Close` 会关闭连接，但请求 ctx 的取消语义与 `Shutdown` 不同。这影响 `isClientGone` 在强关路径上的判定，进而影响是否 `RecordTargetFailure`。**建议实测**。

---

## 7. replay_gate.go

### 7.1 目的

`cmd/gateway/replay_gate.go` 的 `applyReplayGate`（`replay_gate.go:52-113`）在**出站前**剥掉目标协议无法回放的思考块。

要防的是 SDK 的两条硬报错：

- openai-chat：`provider_openai_chat.go:206-211` —— 「assistant turn has only thinking/redacted-thinking blocks, which the OpenAI chat protocol cannot replay」（`ErrInvalidRequest`）。
- anthropic：`provider_anthropic.go:442-447` —— 「assistant turn has only unsigned thinking blocks, which anthropic cannot replay (missing signature)」（`ErrInvalidRequest`）。

`replay_gate.go:25-27` 的说明是准确的：这两条报错是 SDK **故意**写清楚的（宁可报错也不发一条畸形空消息），但没有退路可选 —— 客户端无法为别人的协议重造签名，剥掉是唯一的走法。网关必须在 attempt 内做这一刀，因为故障转移链上换一个目标就换一个协议。

### 7.2 是否仍有效

**有效，且与 SDK 行为逐条一致**：

| 协议 | `keepThinking` | `keepRedacted` | SDK 实际行为 | 一致？ |
|---|---|---|---|---|
| `""` / `openai-chat` / `auto` | false | false | chat 一律跳过 thinking/redacted（`provider_openai_chat.go:187-189`） | ✅ |
| `anthropic` | true | true | anthropic 保留有签名的 thinking（`provider_anthropic.go:415-419`）；redacted **不要求签名**（`provider_anthropic.go:423`） | ✅ |
| `openai-responses` | — | — | `default: return`（`replay_gate.go:65`），由 SDK 的 `SealedBy` 门闩处理（`provider_openai_responses.go:372-374`） | ✅ |

`replay_gate.go:84-89` 的 `BlockRedactedThinking` 分支只按 `keepRedacted` 判、不查签名 —— 与 SDK 的 anthropic provider 一致（redacted_thinking 是 opaque payload，原样回放，不校验签名）。

调用点：`main.go:2616`（流式）、`main.go:2913`（非流式），**每个 attempt 内部各调一次**，符合 `replay_gate.go:49-51` 的要求（「必须**在每个 attempt 内部**调（协议是「发给谁」这一层的属性）」）。

### 7.3 与 inwire / outwire 的回放闸门是否重复

**不重复，三者分层**：

1. `internal/inwire/anthropic.go:442-443`（**入站**）：把 `redacted_thinking` 原样搬进 `rosetta.BlockRedactedThinking`，**保留 data**。
2. `internal/outwire/openai_responses.go:291-295`、`openai_responses.go:549-551`（**出站**）：`SealedBy` 门闩 —— 只有 `SealedBy == "openai-responses"` 的**密封载荷**才写回。
3. `cmd/gateway/replay_gate.go`（**网关层**）：出站前按**目标协议**剥掉不可回放的块。

第 2 条管的是 `Block.Sealed`（opaque reasoning item），第 3 条管的是 `Block.Thinking` / `Block.Signature`。**管的对象不同，不重叠。**

`internal/inwire/openai_responses.go:754-755` 的注释也确认了这个分工：「现在原样搬进统一模型的 Sealed/SealedBy，由 rosetta 的 SealedBy 门闩决定写回哪个协议族」。

### 7.4 幂等性

`replay_gate.go:52` 的注释说「幂等、可重复调用」。

核对：`applyReplayGate` 只做**删除**（`replay_gate.go:79-89` 的 `removed++; continue`），不做转换。第二次调用时已剥过的块不存在了 → `droppedBlocks == 0` → 提前 return（`replay_gate.go:107-109`）。**幂等成立**。

**已验证无问题**：当前架构下 `ing.buildRosetta()` 每次 attempt 都造**新**请求（`main.go:2610`、`main.go:2908`），幂等性在这里不必要；但如果将来 `buildRosetta` 改成缓存复用，幂等性就会成为必需的保险。**这层保险是有的**。

### 7.5 P3：`default` 分支静默放行未知协议

`replay_gate.go:63-66`：

```go
default:
    // openai-responses 由 SDK 的 SealedBy 门闩自行处理，不干预。
    return
```

协议取值由 `validateProtocol`（`internal/admin/provider_handler.go:118-125`）限制在 `validProtocols` 之内，所以 `default` 分支当前只能被 `openai-responses` 命中。

**P3**：`default` 分支**静默放行**任何未知协议。若将来新增第四种协议而忘了在这里加分支，会走到 SDK 的行为 —— 而 SDK 对未知协议可能报错（`rosetta` 的 `request.go:222` 有 `openai-chat` 的保留 payload key 列表，暗示 SDK 侧对协议有明确假设）。改成显式列举 + 未知协议按 chat 规则兜底更安全。

**已验证无问题**：`case "", "openai-chat", "auto":` 里的 `""` 不会持久化 —— `provider_handler.go:210` 把空串默认成 `"openai-chat"`，`provider_handler.go:212` 校验通过后才落库。

---

## 8. 权限 / 额度门在热路径上的行为

（本节只从**热路径正确性**角度谈。鉴权矩阵本身归 A8/A9。）

### 8.1 denyAdminKeyCreate

`main.go:1301-1322`。作用在**管理面**的 key 创建端点上，**不在数据面热路径上**。

**已验证无问题**：

- 无身份一律 401（fail-closed，`main.go:1304-1310`）。
- 管理员 403 + 明确文案（`main.go:1311-1319`），且文案与 `writeAuthError` 里 `ErrAdminCannotCallModel` 的口径一致（`main.go:1355-1358` 说的是同一件事）。
- 它读 `server.UserFromContext(r.Context())`（`main.go:1303`），与数据面 `auth.Authenticate` 是两套体系，不冲突。

### 8.2 ★ P3：writeAuthError 遗漏两个鉴权哨兵

`main.go:1327-1383`。数据面鉴权失败的统一出口，在 `handleIngress` 的第一步（`main.go:1745`）调用。

`main.go:1324-1326` 的注释说明了这个函数存在的理由：「/v1 下的每个端点都走这一处，免得口径漂移（例如某个端点把「密钥被禁用」也当成 401 而非 403）。」

**正因为集中，口径漂移会一致地发生。** switch 里**没有**列举的两个哨兵：

- `auth.ErrKeyUnowned`（`internal/auth/auth.go:117`）
- `auth.ErrUserDisabled`（`internal/auth/auth.go:120`）

两者都落进 `default`（`main.go:1379-1381`）：

```go
default:
    writeErr(w, http.StatusUnauthorized, "invalid_api_key", "authentication failed")
```

**→ 401 + "invalid_api_key"**。

按 `writeAuthError` 自己的分类逻辑，这应该是 **403**：

- `ErrKeyUnowned`（`internal/auth/auth.go:108-118`）：key 有效、用户有效，只是**没有归属**。回 401 让客户端以为 key 坏了而反复换 key —— 而换多少把新 key 都没用（它们同样无归属）。该处的注释说「明确 401 好过静默放行」，但**静默放行是错的，401 并不比 403 好**；403 + 「这把 key 没有归属，请联系管理员认领」才是可执行的指引。
- `ErrUserDisabled`（`internal/auth/auth.go:120`）：账号被禁用。回 401 同样误导 —— key 是好的，该做的是联系管理员启用账号。

对比同文件里已正确处理的：`ErrKeyDisabled` → 403（`main.go:1335-1336`）、`ErrKeyExpired` → 403（`main.go:1338-1341`，含详尽注释说明为什么不用 401）、`ErrIPNotAllowed` → 403（`main.go:1343-1345`）、`ErrAdminCannotCallModel` → 403（`main.go:1355-1358`）。

严重度 **P3**（可诊断性问题，不涉及资金与越权 —— 两个哨兵都是 fail-closed 拒绝，只是错误码/文案误导）。

**待确认**：`writeAuthError` 之外是否有别处处理这两个哨兵（`rg "ErrKeyUnowned|ErrUserDisabled"` 在 `internal/auth/auth.go` 之外未见处理分支）。

### 8.3 鉴权与配额预占的顺序

`handleIngress` 的顺序：鉴权（`main.go:1743`）→ RPM（`main.go:1757`）→ 解码（`main.go:1767`）→ TPM 预占（`main.go:1801`）→ key 级配额预占（`main.go:1846`）→ 用户级配额预占（`main.go:1894`）→ 路由解析（`main.go:1922`）→ 白名单（`main.go:1947`）→ 余额预检（`main.go:1997`）。

**已验证无问题**：

- 鉴权在最前，未鉴权的请求不可能触达任何预占 —— 不会消耗任何额度。
- 白名单在预占**之后**是有意的（`main.go:1943-1946`：「白名单判定只是内存里的线性扫描…放前面会让「超额度的请求」拿到 403 而不是 429，掩盖更该先解决的配额问题。两者都不打上游，顺序只影响错误码优先级。」）。
- 余额预检在白名单之后也是有意且论证充分的（`main.go:1982-1996`）。

### 8.4 `balanceExempt` 的一致性

`balanceExempt`（`billing.go:448-451`）判 `u.Role == store.RoleAdmin`。数据面在 `auth.Authenticate`（`internal/auth/auth.go:135-137`）就已经拒绝管理员。

**已验证无问题**：`charge` 里的 `balanceExempt` 短路（`main.go:3380-3382`）与 `precheckBalance` 里的短路（`billing.go:671-673`）口径一致，都是纵深防御。

### 8.5 客户端可控的 request_id 与扣费幂等

`internal/server/server.go:35-43`：

```go
func Middleware(next http.Handler, logger *slog.Logger) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        reqID := generateRequestID()
        ctx := context.WithValue(r.Context(), requestIDKey, reqID)
        r = r.WithContext(ctx)
        w.Header().Set("X-Request-Id", reqID)
```

`generateRequestID`（`server.go:29-33`）是 8 字节随机 → 16 个 hex 字符。**服务端生成，不读客户端头**。

**已验证无问题**：`balance_charges` 的 `(request_id, user_id)` 复合主键（`store.go:486-492`）在这个前提下是安全的 —— 碰撞概率 2^-64，且复合主键带了 user_id。

**这与 `store.go:473-479` 的注释形成对照**：

> request_id 来源于**客户端可控的请求头**，碰撞并不遥远

该注释描述的是一个**已经不成立的前提**（现在的 request_id 由服务端生成）。注释本身无害（复合主键的结论仍然正确），但它记录的是旧实现。**P3**。

---
## 9. 已验证无问题的区域（含理由）

| 区域 | 结论与理由 |
|---|---|
| **rateCommit 的幂等性** | `releaseQuota` 的两级预占各自「先取后清零」（`main.go:1570-1572`、`main.go:1580-1582`），任何路径重复收尾都是空操作，不会误退别的请求的预占。`commit` 只调一次 `releaseQuota`（`main.go:1553`）。 |
| **handleIngress 6 个 early-return 的配额释放** | S6/S7/S8/S10/S11 均有 `rate.commit(0)`（`main.go:1908`/`1924`/`1948`/`2002`/`2058`），链耗尽终态有（`main.go:2263`），客户端提前断开有（`main.go:2098`）。**唯一例外是 S5（P0-1）**。 |
| **usage 记录的完整性** | 全仓只有 3 个 `usage.record` 调用点（`main.go:2284`/`2853`/`2931`），覆盖「链耗尽未提交」「流式已提交」「非流式已提交」三种终态，无遗漏分支。 |
| **「扣的 = 报表的」不变量** | `costs[i]` 就是写进 `cost_total` 列的**同一个变量**（`usage_dao.go:228` → `usage_dao.go:301`），不是重算。批量路径下逐条 `freezeUsageCost`（`usage_dao.go:274`），语义与单条路径一致。 |
| **浮点误差累积** | 费用在 `priceUsage` 舍入到 1e-9 元（`usage_dao.go:348-351`），无累积；余额走 `balance_remainder`（微元整数）与 `balance_cents`（分整数），全整数运算（`balance_dao.go:352-357`、`balance_dao.go:430-435`）。 |
| **thinking token 不重复计费** | OpenAI 的 reasoning ⊆ output（`provider_openai_chat.go:717-720`），Anthropic 的思考计入 output 且无独立 reasoning 字段（`provider_anthropic.go:1005-1010`）。`priceUsage` 不含 reasoning 项是**正确**的。 |
| **usage 缺失的判定** | `usageStateFor`（`main.go:2972-2977`）用 SDK 的 `IsZero`（`usage.go:32-35`），把 cached/reasoning 计入，「只报缓存命中」不会被误判为 missing。 |
| **cost_pending 标记与记录同生共死** | `cost_pending` 随 INSERT 落库（`usage_dao.go:146-152`、`usageInsertArgs` `usage_dao.go:291-293`），避免了「INSERT 后 UPDATE 命中 0 行」的老坑（`usage_dao.go:140-145` 的注释记录了那个坑）。 |
| **三个流式超时的层级关系** | `totalRequestBudget` = `max(nonStream, ttft + headerWait) + slack` × `(maxTargets+1)`，钳制 [30s, 30min]（`main.go:2542-2564`），恒大于单跳预算，配置上不自相矛盾。 |
| **SDK 的 stream idle watchdog 未双重启用** | 网关没设 `WithStreamIdle`（`internal/upstream` 内 `rg` 无命中），实际只有网关自己的 `idleTimer`（`main.go:2724`）。 |
| **TTFT 看门狗的竞态** | `ttftTimer.Stop()` 的返回值被检查（`main.go:2673`），`ttftDone` channel 消除「回调已起跑但 Close 未落地」的窗口（`main.go:2660`、`main.go:2675`）。 |
| **idle 看门狗的边界竞态** | `lastEventAt` 在 `Reset` **之前**更新（`main.go:2792-2793`），回调里复核（`main.go:2725-2727`），健康流不会被误杀成 `stream_idle_timeout`。 |
| **心跳 goroutine 的生命周期** | `quit` + `WaitGroup` 保证在 handler 返回前退出（`main.go:2756-2775`），不会写到已结束响应（`main.go:2751-2755` 说明了这个具体危害）。并发写由各 sink 内部锁串行化（`openai_chat.go:84`、`anthropic.go:217`、`openai_responses.go:39`；契约见 `internal/outwire/sink.go:16-17`）。 |
| **写 deadline 的能力探测** | 三个 sink 都实现了 `WithResponseWriter`（`openaiSink` `sink.go:124`、`AnthropicSSE` `anthropic.go:248`、`ResponsesSSE` `openai_responses.go:110`）；`statusResponseWriter` 实现了 `Unwrap`（`internal/server/server.go:111-113`），`ResponseController` 能穿透。 |
| **探测名额的释放** | `probeClaimed && !probeRecorded` 才归还（`main.go:2223-2225`），已记账的不重复归还（`halfOpen` 是共享 bool，`upstream.go:636-637`）。`RecordTargetFailure` 在 `threshold<=0` 时也会归还（`upstream.go:585-589`）。 |
| **「冷却中仍被选中」不存在** | `TargetAvailable` 是纯查询（`upstream.go:539-551`），真正拦截在 `ClaimTargetProbe`（`upstream.go:564-579`）的原子判断里。筛选与领名额之间的 TOCTOU 窗口是安全的。 |
| **失败结果不写成 200** | `outcomeFromErr` 把流哨兵改判为 502（`main.go:2417-2426`），`main.go:2257-2259` 在写出关口再兜底一次。测试 `usage_shutdown_test.go:134-180` 钉住。 |
| **usage.record 的关停竞态** | `mu` 把「判 closed + 发送」变成原子操作（`main.go:3439-3449`），关停后走同步写不丢。测试 `usage_shutdown_test.go:50-125` 覆盖。 |
| **replay_gate 的正确性** | 三个协议的剥离规则与 SDK 的实际行为逐条一致（见 §7.2 对照表）；幂等成立；与 inwire/outwire 的 `SealedBy` 门闩管的是不同对象，不重复。 |
| **路由解析不会复活死目标** | `Resolve` 命中禁用路由立即否决（`routing.go:203-217`），`buildCandidates` 一旦见到目标行就以它为准（`routing.go:275-280`），不回落主目标列。 |
| **用量归档不回收终身累计** | 剪枝不回收 `user_usage_totals` / `access_keys.used_tokens`（`usage_archive.go:687-710` 有充分论证，并记录了 2026-10-11 那次「空跑语句」的历史），所以「明细被剪掉」不等于「配额被放宽」。 |
| **`users.used_tokens` 与判定源不混用** | 配额判定读 `user_usage_totals`（`user_dao.go:578-579`），展示列 `users.used_tokens` 只由 `RecomputeUserUsage`（`user_dao.go:681`）/ 回填（`usage_archive.go:389-395`）维护。 |
| **SDK 重试已被关闭** | `buildClient` 设 `WithQuirks{NoIdempotencyKey: true}`（`internal/upstream/upstream.go:1388`），注释（`upstream.go:1381-1387`）说明 SDK v1.0.0 起把 chat POST 标为 RetryIdempotent 且 MaxRetries 默认 2，会导致「上游重复生成重复计费」。**这条已处理，重要。** |
| **探测请求不扣用户钱** | `TestUpstreamModel` 的开销走 provider 自带凭据、不经 usage_recorder（`internal/upstream/upstream.go:820-821` 的注释）。 |
| **管理面 key 创建的管理员短路** | `denyAdminKeyCreate` 无身份 401、管理员 403（`main.go:1301-1322`），fail-closed。 |
| **数据面管理员的一致拦截** | 收敛在 `auth.Authenticate`（`internal/auth/auth.go:135-137`），所有数据面入口同时生效；`balanceExempt` 只是纵深防御（`main.go:3380-3382`）。 |
| **鉴权在预占之前** | 未鉴权的请求不可能触达任何预占，不会消耗额度。 |
| **request_id 由服务端生成** | `generateRequestID`（`internal/server/server.go:29-33`）是 8 字节随机，服务端生成、不读客户端头 → `balance_charges` 的复合主键在这个前提下安全。 |

---

## 10. 完整问题清单

### P0

| # | 位置 | 一句话 |
|---|---|---|
| **P0-1** | `cmd/gateway/main.go:1857` | key 级配额超限的 `return` **漏调 `rate.commit(0)`** —— TPM 窗口预占永不退还。配额已耗尽的 key 可反复触发拒绝，每次都在 TPM 窗口留下一笔永不退还的 `est`（含 `max_tokens` 上界），几十次就能吃光该 key 的整分钟 TPM 额度。 |
| **P0-2** | `internal/store/usage_dao.go:510-512` + `cmd/gateway/main.go:3408` | `cost_pending` 记录的重算**只 UPDATE `cost_total`，不调用 `ChargeBalance`** —— 一次读池抖动造成该请求**永久免单**（报表 0 元、余额不动、无 ERROR）。设计与 `usage_dao.go:356-372` 的意图直接冲突：机制只建了一半。 |
| **P0-3** | `internal/store/key_dao.go:507-513`、`internal/store/user_dao.go:617-623` + `internal/store/store.go:234`/`285` | `reserved_tokens` 是**终身**预占，但**进程崩溃/强杀后没有任何复位路径**（全仓 `rg` 无 reset 语句，`UpdateAccessKey` 也不碰它）—— 一次崩溃把该 key / 该用户的终身额度永久冻结，需人工改库才能恢复。 |

### P1

| # | 位置 | 一句话 |
|---|---|---|
| **P1-1** | `cmd/gateway/main.go:2795-2800` + `2853-2878`（流式 canceled 路径） | 客户端中断时 `lastUsage` 恒为零值（只在 `EventMessageEnd` 上赋值，而中断时上游不会发该事件）→ `usage_state="missing"`、`cost_total=0` → **用户点「停止」即可免费消耗已生成的 token**，且 `trg_update_used_tokens` 累加 0 + 预占全额退回 → **完全绕过 key 级终身 token 配额**。可主动利用。 |
| **P1-2** | `cmd/gateway/main.go:2889`/`2956` vs `internal/store/usage_dao.go:332-352`；`rosetta@v1.2.0/provider_anthropic.go:1012-1027` | TPM 校正用 `TotalTokens`、费用用 `Input+Output`，两套口径不同源；且 Anthropic 的 `cache_creation` token 被按 `price_input` 计费（无缓存写入价入口，`store.go:207-209` 只有三列单价），`cached_tokens` 归档也漏掉它（`main.go:2867`）。 |
| **P1-3** | `cmd/gateway/billing.go:492-496` + `500-505` vs `internal/store/usage_dao.go:337-340` | 预检与实际扣费的**单价兜底方向不同**：只配了 `price_cache_hit` 的模型，预检按「未配价」提前返回 0（`billing.go:492`），而实际扣费中命中部分按缓存价收费 → 预检只要求「余额 ≥ 1 元」地板线 → **fail-open 漏放行**。管理面**允许**该配置（`internal/admin/model_handler.go:274-297`、`379-381` 无「至少一价 > 0」校验）。 |
| **P1-4** | `cmd/gateway/main.go:3293-3299` + `internal/store/usage_dao.go:510-512` | 落库与扣费之间**无对账**：落库成功、扣费前崩溃 → 记录在库、报表有费用、**余额未扣**，且记录本身没有任何字段表明它没扣费。全仓无对账任务。 |
| **P1-5** | `cmd/gateway/main.go:3323-3328` | `flushOneByOne` 的主键冲突分支无条件 `continue`（跳过 charge）—— 「已落库」不足以推出「已扣费」：`tx.Commit()` 返回错误但事务已提交时，下一轮会走进「落库了但没扣费」的状态。幂等责任放错了层（`ChargeBalance` 本身已幂等，`balance_dao.go:331-341`）。 |
| **P1-6** | `cmd/gateway/main.go:3408-3422` | `charge` 吞掉 `ChargeBalance` 的**所有**错误（两个分支都只记一条 ERROR），无重试、无补偿、无待扣队列 —— 一次 DB 抖动 = 钱款永久漏收，且记录看起来完全正常（`usage_state='reported'`、`cost_total` 非 0），事后对账都无法从数据本身发现。 |

### P2

| # | 位置 | 一句话 |
|---|---|---|
| **P2-1** | `cmd/gateway/main.go:2888-2892` | usage missing 时 TPM 预占按 `max_tokens` 上界**永久保留**在窗口里（`est` 含 `max_tokens`，`main.go:1626`），不报 usage 的第三方服务（SDK `Quirks.NoStreamUsage` 的注释，`options.go:20-23`）会**系统性压低 TPM 有效额度**，量级可达实际用量的 10 倍。 |
| **P2-2** | `cmd/gateway/main.go:876-886` + `3170-3184` | 关停 5s drain 超时后那批 `stillPending` 记录**永久丢失**（进程随即退出、`db.Close()` 执行）；worker 还在用 `ctxBackground()`（永不取消）无限重试，与 `db.Close()` 并发产生噪声。 |
| **P2-3** | `cmd/gateway/main.go:2678-2681` + `2685` | TTFT 超时时 `stream.Err()` 的判定**优先于** `ttftTimedOut` 判定，且无条件 `credCooldown: 60s` —— 一次偶发慢就把单 key provider 踢出健康轮换 60s。 |
| **P2-4** | `cmd/gateway/main.go:2202-2203` + `2824-2828` | `totalRequestBudget` 到期（`DeadlineExceeded`）被当作**上游故障**记 `status="error"` 并 `RecordTargetFailure` → 网关自身超时会把健康目标熔断 60s，且在报表里与上游故障混为一谈。 |
| **P2-5** | `internal/outwire/openai_chat.go:120`/`122`/`136`、`anthropic.go:255`/`277`、`openai_responses.go:131`/`504` | 全部七个 SSE 写点的 `fmt.Fprintf` **返回错误被完全丢弃** → 写超时后流被静默截断却仍记 `status="ok"`（假成功）；写 deadline 不支持时 handler 可永久阻塞在写里，**上游流与 token 一起泄漏**，idle 看门狗形同虚设。 |
| **P2-6** | `internal/store/route_target_dao.go:161-177` + `internal/store/store.go:371-380` + `internal/admin/route_target_handler.go:104-120` | `route_targets` 无 `(route_id, provider_id, upstream_model_id)` 唯一约束，管理面 `Replace` 端点**不做链内重复检查** → 可配出两个 position 指向同一上游 → 熔断计数记在不同 TargetID 上，**同一上游被打了两次**（重复计费）。 |
| **P2-7** | `cmd/gateway/main.go:2136-2142` | 「该 provider 无健康凭据」（凭据全在冷却）被记成**目标级失败**并累计熔断计数 —— 上游本应被冷却，结果是冷却 + 熔断两套惩罚叠加，连续 3 次即熔断 60s。 |

### P3

| # | 位置 | 一句话 |
|---|---|---|
| **P3-1** | `cmd/gateway/main.go:2344` | `appendRouteTrace` 判空了 `cand.Provider` 却没判 `cand.UpstreamModel`（当前不可达，但同类型字段处理不对称）。 |
| **P3-2** | `cmd/gateway/replay_gate.go:63-66` | `default` 分支**静默放行**未知协议；将来新增协议忘记加分支会走到 SDK 的报错路径。 |
| **P3-3** | `cmd/gateway/main.go:1379-1381` | `writeAuthError` 遗漏 `auth.ErrKeyUnowned` 与 `auth.ErrUserDisabled`，两者都落进 `default` → 回 **401**（而同类错误一律 403），误导客户端「换一把 key」。 |
| **P3-4** | `cmd/gateway/main.go:3483-3487` | `HasPendingUsage` 只反映 `flushFailed` 标志，不反映队列长度 —— `wait` 超时路径下可能假阴性（报告为「已排空」但实际未落库）。 |
| **P3-5** | `internal/store/store.go:473-479` | 注释称「request_id 来源于**客户端可控的请求头**」—— 该前提已不成立（`internal/server/server.go:38` 现在服务端生成）。结论（复合主键）仍正确，但记录的是旧实现，容易误导。 |

**另有两处不单列的词表/一致性问题**：
- `usage_state` 的 `"none"`（`main.go:2294`）与 `reported`/`missing`/`cost_pending` 不在同一维度上（见 §4.8）。
- `estimateBalanceCost`（`cmd/gateway/billing.go:473-514`）与 `store.priceUsage`（`internal/store/usage_dao.go:332-352`）是**两份**计价公式。缓存项的差异是有意的保守（`billing.go:460-463`），但**兜底规则的差异是缺陷**（P1-3）。根治办法是把 `priceUsage` 导出并在两侧共用 —— `billing.go:469-472` 的注释正是因为它包私有才复制了一份。

---

## 11. 待确认清单

1. **P1-2 的产品口径**：Anthropic 的 `cache_creation`（缓存写入）token 是否应独立计价？`upstream_models` 表只有 `price_input / price_cache_hit / price_output` 三列（`store.go:207-209`），没有缓存写入价入口。若产品上不区分，该条降为 P2（`cached_tokens` 归档漏项仍需修）。
2. **P2-3 的产品期望**：TTFT 超时是否真的要无条件冷却凭据 60s？`main.go:2685` 显式写了 `credCooldown: 60 * time.Second`，看起来是刻意的，但单 key provider 的一次偶发慢就不可用 60s。
3. **P0-3 的复位位置**：`migrate()` 的调用点与是否适合放 `reserved_tokens` 归零（它需要一条写事务，且 `migrate()` 已在跑多条 DDL）。
4. **§6.3 的 `srv.Close()` 语义**：`http.Server.Close` 是否取消请求 ctx？影响 `isClientGone` 在强关路径上的判定，进而影响是否 `RecordTargetFailure`。建议实测。
5. **§2.6 的 `srv.Close()` 与 worker 并发**：需实际跑一次关停压测确认日志噪声程度。
6. **P3-3 的其他处理**：`writeAuthError` 之外是否有别处处理 `ErrKeyUnowned` / `ErrUserDisabled`（`rg` 只在 `internal/auth/auth.go` 见定义处）。

---

## 12. 建议的修复顺序

按「改一行就能修 → 需要设计 → 需要口径统一」排列：

1. **P0-1**（一行）：`main.go:1857` 的 `return` 前加 `rate.commit(0)`。零风险。
2. **P0-3**（启动时一行 UPDATE）：`migrate()` 之后把 `access_keys.reserved_tokens` 与 `users.reserved_tokens` 归零（进程启动时不存在在途请求）。
3. **P1-5**（删几行）：去掉 `main.go:3323-3328` 的 `continue`，让 `ChargeBalance` 自己判幂等（它已经幂等）。
4. **P0-2 + P1-4**（设计级，但可合并）：引入「待扣费」表。`charge` 失败 → 写待扣费表；`RecomputeCost` 把 `cost_total` 从 0 改为非 0 → 走 `ChargeBalance`；后台任务扫「`status='ok' AND cost_total>0` 但 `balance_charges` 无对应行」的记录补扣。**这一条同时解决三个问题，是本子域最值得优先做的架构改动。**
5. **P1-1**（设计级）：取消/中断路径必须**占用配额**（不退 `est`，或让 `total_tokens` 落一个保守下界），使「点停止」不能成为绕过配额的通道。费用是否收是产品口径，但 `cost_total`（网关成本）**必须**记。
6. **P1-3 + P1-2**（口径统一）：把 `priceUsage` 导出并在 `billing.go` 共用，消除两份公式；给 `upstream_models` 增加缓存写入价列（或明确记录「不区分」并把 `cached_tokens` 的归档口径与 `priceUsage` 对齐）。
7. **P2-4**（归因）：把 `DeadlineExceeded` 与 `Canceled` 分开 —— 预算到期记独立 `status` 且不计入 `RecordTargetFailure`。
8. **P2-5**（写错误）：让 `StreamSink.Event` 返回 error，一路传回 `attemptStream`，写失败按 `status="error"` 收尾并正常落库。
9. **P2-6**（配置校验）：`Replace` 端点加链内重复检查，或 `buildCandidates` 按 `(provider_id, upstream_model_id)` 去重。
10. **P2-7**（归因）：「无健康凭据」不应计入目标熔断计数（应与客户端中断一样跳过 `RecordTargetFailure`）。
11. **P2-1 / P2-2 / P2-3 / P3-***：按上述清单逐条处理。

---

## 附：审计方法说明

- **检索**：XERJ（`xerj_search`，index `rosetta-gateway-docs`）定位「哪段代码在做 X」；`rg` 定位精确字面量与行号。未查询 `ax-*` 或 `*`。
- **SDK 侧证据**：来自 `$(go env GOMODCACHE)/github.com/cn-maul/rosetta@v1.2.0`（版本由 `go.mod:6` 确认）。所有 SDK 引用都给了 `文件:行号`。
- **交叉核对**：`cmd/gateway` 的相关测试文件（`failover_test.go`、`quota_test.go`、`usage_flush_retry_test.go`、`usage_shutdown_test.go`、`billing_scope_test.go`、`e2e_datapath_test.go`）—— 用来确认哪些分支已被钉住、哪些是测试盲区。**本报告的多数发现正落在测试盲区**（例如 `quota_test.go` 未覆盖「key 级配额超限」这条 return；`usage_flush_retry_test.go` 未覆盖「`tx.Commit()` 成功但返回错误」）。
- **未运行全量 `go test`**（基线已知全绿，cmd/gateway 67.7s）。未修改仓库内任何文件。
- **自我修正记录**：初稿曾把「Anthropic cache_creation 双倍计费」列为 P1，复核 `provider_anthropic.go:1012-1027` 后**撤回**（creation 只被计费一次，不是两次），并把该条改写为「无独立缓存写入价入口 + 归档列漏项」。初稿曾怀疑 TTFT 超时会写出 200 假成功，复核 `outcomeFromErr` + `main.go:2257` 的兜底后**撤回**。初稿曾把 P1-3 的可达性标为待确认，复核 `internal/admin/model_handler.go:274-297` 后**确认为可达**。
