# 数据面动态端到端测试报告

**任务**：把核心数据面**真的跑起来**，用实测证据（而非代码推理）验证 6 组场景。
**产出**：`cmd/gateway/e2e_datapath_test.go`、`cmd/gateway/e2e_fake_upstream_test.go`
（均为新建测试文件，**未改任何生产代码**）。
**环境**：Go 1.27.0 / `rosetta v1.0.0` / SQLite（真库，`store.Open` 到 `t.TempDir()`）。

---

## 0. 结论速览

| # | 场景 | 结果 |
|---|---|---|
| A | 全链路正例（非流式 + 流式） | ✅ 通过 |
| B | 鉴权拒绝 5 条路径 | ✅ 通过 |
| C | 失败转移链（5xx / 404 / 401 / **已提交后不重试**） | ✅ 通过 |
| D | 配额与限速 + 预占归还 | ✅ 通过 |
| E | 快照生效（改配置不重启） | ✅ 通过 |
| F | 客户端断开 | ✅ 通过 |
| Adv1..10 | 对抗性边界（并发 / 重放 / 二次入口 / 超时 / 畸形体） | 9 通过，**1 发现真实缺陷** |

**发现 1 个真实缺陷（P2）**：上游返回 `200 + 畸形 JSON` 时，网关回 **500 `internal_error`**，
且该错误**不可转移** —— 链不会换到健康上游。详见 §7。

**本轮我自己犯并修掉的 2 个测试错误**（都记在测试注释里，因为它们是最容易导致
假绿/假红的坑）：`sync.Once` 误用（§5.1）、异步副作用断言过早（§5.2）、
余额用例定价与余额不相称致断言空转（§5.3）。

---

## 1. 手法与骨架

### 1.1 复用既有脚手架（不重造）

`failover_test.go` 已有 `buildHarness` / `buildHarnessFull`（装好 pool + 快照 + 临时库 + 测试 key）、
`priceRoute`、`goodHarness`、`postChat*`。本轮的假上游与辅助函数**建立在它们之上**，
不另起一套 —— 两套 harness 并存迟早漂移，而漂移的那一套会让「同一场景两个结论」。

### 1.2 新增的两块能力

**（a）记账假上游**（`e2e_fake_upstream_test.go`）。既有 `fakeGood` 只回响应、不记请求，
而本轮多条断言必须靠**证据**：`fakeRecorder` 记录每次请求的方法/路径/body/凭据，
并暴露 `Count()` / `Calls()` / `LastBody()`。`disconnectingUpstream` 额外提供
「上游自己观察到 ctx 取消」的信号。

**（b）DB 驱动的真运行时**（`e2eDBHarness`，场景 E 专用）。`buildHarness` 手工拼
`routing.RouteIndex`，DB 里没有 routes 行 —— 而 AutoReload 的承诺是
「落库 → 重建 → 快照生效」，要验证它重建就必须**真的从库读**。
所以 `e2eDBHarness` 把 provider/credential/模型/route/用户/key 全写进 DB，
再用生产的 `runtimeReloader.Reload`（含 `PrepareFromStore` + `RebuildFromDB` + `Swap`）建运行时。

### 1.3 断言纪律

- **落库与扣费是异步的**（`usageRecorder` 的 worker goroutine），所有相关断言一律**轮询**：
  `e2eWaitUsageCount` / `e2eWaitBalanceDrop` / `e2eWaitReservedZero`。直接断言会得到一个
  「看时序」的偶发假绿。
- **「没打到上游」必须用计数器证明**，不能只看状态码 —— 一个 403 完全可能在打完上游之后才写出。
- **「没有发生某事」要多等一会儿再断言**（Adv8 在首次查到 1 行后额外 sleep 500ms，
  否则「重复记账」会因「第二行还没写完」而假绿）。

---

## 2. 场景 A：全链路正例

### A1 非流式 —— 合法 key → 命中路由 → 转发 → 200 → 落库 → 扣费

```
$ go test ./cmd/gateway/ -run 'TestE2E_A1' -v -count=1
=== RUN   TestE2E_A1_NonStreamHappyPathChargesAndRecords
    A1 响应: status=200 content-type="application/json" body={"id":"cmpl-1","object":"chat.completion",
      "created":1791450456,"model":"flash","choices":[{"index":0,"message":{"role":"assistant",
      "content":"pong"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":3,
      "total_tokens":8}}
    A1 usage 行: {Status:ok UsageState:reported TotalTokens:8 CostTotal:8 ProviderID:good
      Upstream:good-model Stream:false RequestID:befbeb4215075857}
    A1 余额: 10000分/0微元 → 9200分/0微元，合计扣费≈8.000000 元；cost_total=8.000000 元
--- PASS: TestE2E_A1_NonStreamHappyPathChargesAndRecords (0.32s)
```

**结论：通过。** 逐条对上任务要求：HTTP 200 ✅、响应体形状（`object`/`model`/`choices`/`usage`）✅、
`usage_records` 一行且 `cost_total > 0` ✅、`balance_cents` 减少 ✅、`cost_total` 与实扣**完全一致** ✅。

两个值得记的观测：

1. **响应的 `model` 是公开名 `flash`，而不是上游 `good-model`** —— 我显式断言了这一点。
   路由映射搞反的话，内容上完全看不出来，但客户端会拿到一个它不认识的模型名。
2. **上游收到的是 `Bearer sk-good`**（provider 自己的凭据），不是客户端的 key。也断言了。

### A2 流式 —— SSE 分片 + `[DONE]` + 落库 + 扣费

```
$ go test ./cmd/gateway/ -run 'TestE2E_A2' -v -count=1
    A2 响应: status=200 content-type="text/event-stream"
        data: {"choices":[{"delta":{"role":"assistant"},...}],...,"model":"flash",...}
        data: {"choices":[{"delta":{"content":"pong"},...}],...
        data: {"choices":[{"delta":{},"finish_reason":"stop","index":0}],...
        data: {"choices":[],...,"usage":{"completion_tokens":3,"prompt_tokens":5,"total_tokens":8}}
        data: [DONE]
    A2 usage 行: {Status:ok UsageState:reported TotalTokens:8 CostTotal:8 Stream:true ...}
    A2 余额: 10000分/0微元 → 9200分/0微元，合计扣费≈8.000000 元；cost_total=8.000000
--- PASS: TestE2E_A2_StreamHappyPathRecordsAndCharges (0.23s)
```

**结论：通过。** 断言：SSE 分片顺序（role → content → finish_reason → usage）、
**`[DONE]` 恰好出现 1 次且是最后一行**、`finish_reason` 在 `[DONE]` 之前、
`usage_state=reported`（不是 missing）、落库与扣费都对。

`[DONE]` 的「恰好一次」值得单列：漏了客户端会一直等，多了说明终止序列被写了两次
（流式收尾最常见的错）。

---

## 3. 场景 B：鉴权拒绝路径

五条路径逐条实测，**每条都用计数上游证明「零次触碰」**：

```
$ go test ./cmd/gateway/ -run 'TestE2E_B_' -v -count=1
    [无效key]        status=401 body={"error":{"message":"invalid API key","type":"authentication_error","code":"invalid_api_key"}}
    [禁用key]        status=403 body={"error":{"message":"API key disabled","type":"authentication_error","code":"invalid_api_key"}}
    [过期key]        status=403 body={"error":{"message":"this API key has expired","type":"api_error","code":"key_expired"}}
    [归属用户不存在]  status=401 body={"error":{"message":"authentication failed","type":"authentication_error","code":"invalid_api_key"}}
    [归属管理员的key] status=403 body={"error":{"message":"administrator accounts cannot call models: create a regular user
                     account and use the API key issued to that user","type":"permission_error","code":"admin_cannot_call_model"}}
--- PASS: TestE2E_B_AuthRejectionsNeverReachUpstream (1.21s)
    B2 被拒后账目未变：10000 分 / 0 微元（等待 400ms）
--- PASS: TestE2E_B2_RejectedRequestDoesNotCharge (0.59s)
```

**结论：通过。** 五条都：状态码正确、`code` 正确、**上游计数为 0**、
**没有产生 usage 行**（B）且**账目纹丝不动**（B2）。

管理员的 `code=admin_cannot_call_model` + `type=permission_error` 与任务描述一致。
「归属用户不存在」返回通用 `authentication failed` 是**有意**的（`ErrKeyUnowned`
落到 `writeAuthError` 的 default 分支）—— 不向调用方区分「key 不存在」与「归属用户被删」，
避免泄露内部状态，我认为这个取舍合理。

---

## 4. 场景 C：失败转移链

### C1/C2 5xx / 404 / 401 → 转移并成功

```
    C1 status=200 body=...content":"pong"...    C1 链首收到 1 次；次目标收到 1 次
    [链首404] status=200 ...（链首 1 次 / 次目标 1 次）
    [链首401] status=200 ...（链首 1 次 / 次目标 1 次）
--- PASS: TestE2E_C1_FailoverOn5xxPreservesChainOrder (0.17s)
--- PASS: TestE2E_C2_FailoverOn404And401 (0.40s)
```

**结论：通过。** 顺序证据：链首**被调用过且失败** + 次目标**被调用过且成功**，
两者同时成立才能证明链按 position 推进（若跳过链首，链首计数会是 0）。
另外断言了响应体里 `chat.completion` 出现**恰好 1 次**（不把两次尝试的输出拼给客户端），
且 usage 只 1 行、归因到**真正服务成功**的 `p2`。

### C3（关键边界）流式已写出字节后失败 —— 绝不重试

```
    C3 status=200
        data: {"choices":[{"delta":{"role":"assistant"},...}]}
        data: {"choices":[{"delta":{"content":"PARTIAL"},...}]}
    C3 链首收到 1 次；健康次目标收到 0 次（**必须为 0**）
    C3 usage 行: {Status:truncated UsageState:missing TotalTokens:0 CostTotal:0 ProviderID:p1 Stream:true}
--- PASS: TestE2E_C3_StreamCommittedThenFailedIsNotRetried (0.19s)
```

**结论：通过 —— 这条是整轮最有价值的验证。**

构造要点：次目标必须是**可用**的，否则「没有重试」与「没有可重试的目标」无法区分，
断言会因错误的原因通过。这里链是 `[会截断的上游, 完整健康上游]`，
并断言健康上游计数**为 0** —— 证明确实是「已提交所以不重试」。

四条核心断言全过：① 健康次目标 0 次触碰；② 客户端确实收到了 `PARTIAL`
（证明「已提交」前提成立）；③ **截断流里没有 `[DONE]`**（发了客户端会把半截回答当成功）；
④ 客户端没有收到次目标内容（没发生两段流拼接）。usage 记 `truncated` 而非 `ok`。

---

## 5. 场景 D：配额与限速

```
    D1 status=429 body={"code":"insufficient_quota"}; 上游被碰 0 次
    D2 第3个请求 status=429 retry-after="52" body={"code":"rate_limit_exceeded"}
--- PASS: TestE2E_D1_QuotaExhaustedRejectsWithoutTouchingUpstream (0.17s)
--- PASS: TestE2E_D2_RPMExceededRejectsWithoutTouchingUpstream (0.26s)
--- PASS: TestE2E_D3_ReservationReleasedAfterSuccess (0.25s)
--- PASS: TestE2E_D4_ReservationReleasedAfterChainExhausted (0.27s)
```

**结论：通过。** 配额耗尽 → 429 且上游 0 次触碰；RPM 超限 → 429 + `Retry-After` 头
（实测值 52s，落在合法区间）+ 被拒请求不打上游。

**预占（`reserved_tokens`）三条收尾路径各自验证**（这是任务点名的边界）：

- D3 **成功**收尾 → 归零，且连发 3 个请求都还能通过（泄漏的话第 2 个就 429）；
- D4 **失败**（链耗尽）收尾 → 归零，连发 3 次失败仍是 5xx 而非 429；
- F1 **客户端断开**收尾 → 归零（见 §6）。

三条路径的释放点在不同分支上（`attemptStream`/`attemptNonStream` 内 vs
`handleIngress` 尾部的 `rate.commit(0)`），所以必须分开测 —— 只测一条会漏掉另两条的泄漏。

---

## 6. 场景 E/F

### E：快照生效（改配置不重启）

```
    E1 禁用前: status=200
    E1 管理面 PATCH enabled=false: status=200
    E1 库内 key.Enabled=false
    E1 禁用后（未重启）: status=403 body={"message":"API key disabled","code":"invalid_api_key"}
    E2 重新启用后（未重启）: status=200 ...content":"pong"...
    E3 管理面收紧白名单: status=200
    E3 收紧后（未重启）: status=403 body={"code":"model_not_allowed"}
--- PASS: TestE2E_E1_AdminDisableKeyTakesEffectWithoutRestart (0.31s)
--- PASS: TestE2E_E2_AdminReEnableKeyRestoresWithoutRestart (0.30s)
--- PASS: TestE2E_E3_AdminModelAllowlistTakesEffectWithoutRestart (0.17s)
```

**结论：通过。** 三条链路都真的走通了四段：
`admin handler 落库 → AutoReload 触发 reloader.Reload → RebuildFromDB → Swap → 数据面读新快照`。

- E1 是安全敏感的那条（「禁用 key」不生效 = 界面在说谎）。断言了**库里确实落成
  `Enabled=false`**（响应 200 不等于落库了）+ 数据面 403 + 上游计数未增。
- E2 是反向。只有 E1 的话，一个「重建后一律拒绝」的实现照样绿 —— 那会让所有 key 变废钥。
- E3 走**不同的快照字段**（`AllowedModels` 判定器 vs `enabled` 布尔短路），
  所以单独验证：只重建了 Enabled 的实现会让 E3 红而 E1 绿。

### F：客户端断开

```
    F1 上游已在途并吐出首片，现在模拟客户端断开
    F1 上游观察到的断开（ctx 取消）: true
    F1 usage 行: {Status:canceled UsageState:missing ... Stream:true ProviderID:slow}
--- PASS: TestE2E_F1_ClientDisconnectReleasesUpstreamAndMarksCanceled (0.20s)
```

**结论：通过。** 三个断言：

1. **上游自己观察到 ctx 取消** —— 这是唯一可信的证据。从网关侧完全看不出来
   （连接被归还池里却仍在跑，网关照样安静）。诊断时实测传播延迟 **842µs**。
2. usage 记 `canceled` 而非 `error` —— 客户端断开不是上游故障，记 error 会虚高错误率
   并可能误熔断健康目标。
3. `reserved_tokens` 归零。

另见 **Adv3**：非流式是**另一条**独立路径（`attemptNonStream`），单独测过，同样通过。

---

## 7. 发现：上游返回 `200 + 畸形 JSON` → 500 且**不可转移**（P2）

### 现象

```
$ go test ./cmd/gateway/ -run 'TestZZProbeMalformedClass' -v -count=1   # 临时探针，已删除
    probe status=500 body={"error":{"message":"internal gateway error","type":"api_error","code":"internal_error"}}
    probe 坏上游被调用 1 次；好上游被调用 0 次
    probe 结论：畸形上游响应**不可转移** —— 链没有换到健康上游。
```

### 复现

假上游返回 `200` + `Content-Type: application/json` + body `{"choices": [ this is not json`。
链配成 `[返畸形体的上游, 健康上游]`，故障转移开启。**期望**：转移到健康上游并 200
（或至少把错误归因到上游）。**实际**：500 `internal_error`，健康上游 0 次触碰。

### 根因（已定位到具体行）

SDK 的解码失败被**普通 `fmt.Errorf`** 包装，没有实现任何哨兵/类型：

```
rosetta@v1.0.0/provider_openai_chat.go:773
    return nil, fmt.Errorf("rosetta: decoding openai-chat response: %w", err)
```

于是：

- `outwire.MapUpstreamError`（`internal/outwire/errors.go`）的三段判定
  （`ErrContextTooLong`/`ErrInvalidRequest` → `APIError` → `TransportError`）**全部不命中**，
  落到最后一行的兜底：
  ```go
  internal/outwire/errors.go:89
      return http.StatusInternalServerError, "internal_error", "internal gateway error"
  ```
- `outwire.FailoverEligible` 同样不命中任何一档 → `false` → `handleIngress` 的
  `!out.eligible` 分支直接 break，**不换目标**。

### 为什么重要

1. **链的存在意义被削掉一半。** 一个「接受请求、返回垃圾」的上游（中转网关挂了、
   返回 HTML 错误页、返回半截 JSON —— 都是真实形态）**永远不会被换掉**，
   所有打到它的请求都硬失败。而故障转移链正是为吸收这类目标级故障存在的。
2. **归因指向错误的方向。** `internal_error` / 500 把运维的注意力引到**网关自己**身上，
   而坏的是上游。排障会从错误的一端开始。
3. `500` + `api_error` 对客户端也**不可重试**（SDK 通常只对 5xx/429 退避，
   而这里恰好是 5xx，所以客户端会重试并再次撞上同一个坏上游）。

### 严重度：**P2**

不是 P0/P1：不丢数据、不重复扣费（实测该请求 `cost_total=0`，不扣费）、不崩溃。
但它让一类真实的故障模式**无法被自愈机制吸收**，且排障方向被指错。

### 为什么没修

按任务纪律「发现 bug 写进报告，**不要顺手修**」。而且修法有**至少两种**、
取舍不同，应当由 Lead 决定：

- **方案 A（在网关侧分类）**：`MapUpstreamError` 对「非 APIError 且非 TransportError
  且非已知哨兵」的错误改成 502 `upstream_error`，并让 `FailoverEligible` 对
  「解析上游响应失败」返回 true。改动小，但等于网关在猜 SDK 的错误语义。
- **方案 B（在 SDK 侧分类）**：让 `decodeOpenAIChatResponse` 用
  `fmt.Errorf("%w: ...", ErrUpstreamMalformed)` 之类的哨兵包装（或 `APIError`）。
  更正确（谁产生谁分类），但 `rosetta` 是外部依赖，需要发版或 vendor。

我倾向 **B + A 兜底**：SDK 给出可分类的哨兵，网关侧同时保留「未知错误一律 5xx 且可转移」
的保守兜底 —— 因为「未知的上游错误」几乎不该被当成网关内部错误。
建议 Lead 先与 SDK 属主确认再定。

### 现有测试为何没发现

`internal/outwire/errors_test.go` 覆盖了 `APIError`/`TransportError` 各档映射，
但**没有**「非类型化错误」这一档 —— 而那恰好是兜底分支。这类缺口正是本轮
「动态 E2E」的价值所在：它从真实链路出发，而不是从已建模的错误类型出发。

---

## 8. 对抗性场景（Adv1–Adv10）

| 用例 | 测什么 | 结果 |
|---|---|---|
| Adv1 | 管理员 key 打 **`GET /v1/models`**（独立入口，非 `handleIngress`）也必须 403 | ✅ 403 + `admin_cannot_call_model` + 不泄露模型清单 |
| Adv2 | 同一 `request_id` 串行重放不重复扣费 | ✅ 见下 |
| Adv3 | **非流式**路径客户端断开 → 预占归还、不记 error | ✅ |
| Adv4 | **并发**打限额 key 不超发 | ✅ 见下 |
| Adv5 | 首字看门狗真的掐断「永不出首字」的上游 | ✅ 202ms 返回 504 |
| Adv6 | 畸形上游响应不得 200 放行 | ⚠️ 发现缺陷（§7） |
| Adv7 | **流式**被白名单拒时不得带 SSE 头 | ✅ `application/json` + 无 `data:` |
| Adv8 | 截断流只落**一行** usage、不重复扣费 | ✅ |
| Adv9 | `/v1/messages` 与 `/v1/chat/completions` 共享限速器 | ✅ 见下 |
| Adv10 | **并发**同 `request_id` 不重复扣费 | ✅ 见下 |

### 几条最有说服力的实测数据

**Adv2 幂等（串行重放）**：两条记录的 `cost_total` 合计 **4.00 元**，实际只扣 **2.00 元**
（余额 10000 → 9800 分）。—— 若幂等失效会扣 4.00 元。

**Adv10 幂等（并发重放）**：**8 条**同 `request_id` 记录，`cost_total` 合计 **16.00 元**，
实际只扣 **2.00 元**（余额 10000 → 9800）。
```
    Adv10 并发 8 条同 request_id：落库 8 行，cost_total 合计=16.0000 元；
          余额 10000 → 9800，实扣≈2.0000 元
```
并发版比串行版更有价值：它覆盖的是「两个 goroutine 同时进入 `ChargeBalance`、
同时 `INSERT OR IGNORE`」这个更窄的窗口。

**Adv4 并发配额不超发**：配额 1000、每个请求 `max_tokens=500`，8 个并发 →
放行 2、429 6，**上游只被碰 2 次**，`reserved_tokens` 归零。
```
    Adv4 并发 8 个请求（配额 1000，每个预占 ~500）：放行 2，429 6，
          全部状态码=[429 429 200 200 429 429 429 429]
    Adv4 库内 quota=1000 used=16 reserved=0
```
这条串行永远测不出来 —— 必须真并发才能覆盖「两个请求读到同一个 used、
各自判定够用、双双放行」的 check-then-act 窗口。

**Adv9 跨入口限速共享**：RPM=1，`/v1/chat/completions` 用掉额度后，
`/v1/messages` 返回 429。若两个入口各持一个限速器，换路径就能绕过 RPM。

**Adv5 首字看门狗**：挂死上游 → **202ms** 返回 504 `upstream_timeout`
（而不是等到 `ResponseHeaderTimeout` 的 60s）。

---

## 9. 我在本轮犯并修掉的测试错误（记录下来以免后人重犯）

这些**不是**生产缺陷，但每一个都曾产生过错误结论。写进测试注释是因为它们是最典型的
「测试在说谎」形态。

### 9.1 `sync.Once` 误用导致假红（F1 首轮失败）

`disconnectingUpstream` 最初用**同一个** `sync.Once` 去关两个不同的 channel
（`started` 与 `released`）：
```go
d.releaseOnce.Do(func() { close(d.started) })    // 第一次生效
...
d.releaseOnce.Do(func() { close(d.released) })   // 第二次是 no-op！
```
`released` 永远不闭合 → F1 报「断开没传播到上游」，看起来像个严重缺陷。
我写了个临时诊断去量时序，实测：
```
    diag: 网关 handler 在 cancel 后 842.7µs 返回
    diag: 上游在 cancel 后 842.7µs 观察到 ctx 取消     ← 生产代码完全正常
```
**教训**：一个 `Once` 只能保护一个 channel。以及——**先量时序再下结论**，
否则会把「我的等待姿势错了」写成「生产的 bug」。

### 9.2 异步副作用断言过早导致假红（Adv5 首轮失败）

Adv5 在请求返回后直接查库，得到「挂死请求没有 usage 记录 —— 失败调用漏账」。
加轮询后实测记录是**有**的（`status=error, usage_state=none`）。
**教训**：`usageRecorder` 是异步落库的，「请求返回」≠「副作用已落地」。
所有副作用断言必须轮询。

### 9.3 余额用例的定价与余额不相称导致断言空转（Adv2 首轮）

Adv2 最初把单价写成 `1e6`（每 token 1 元），两条记录 `cost_total` 各 **2000 元**，
而余额只有 **100 元** —— `ChargeBalance` 按余额守卫**正确地**拒绝了扣费。
用例于是报「费用被少扣了」，看起来像幂等 bug，实际是测试素材错误。
改成单价 1000 元/百万 token（单条 2 元）后才有意义。
**教训**：余额用例的定价、token 数、余额必须一起设计；否则「没扣两次」可能只是因为「压根没扣」。

---

## 10. 硬性验证

```
$ gofmt -l cmd internal
（无输出）

$ go test -count=1 ./...
ok  	github.com/cn-maul/rosetta-gateway/cmd/gateway	246.476s
ok  	github.com/cn-maul/rosetta-gateway/internal/admin	36.081s
ok  	github.com/cn-maul/rosetta-gateway/internal/auth	0.041s
ok  	github.com/cn-maul/rosetta-gateway/internal/crypto	0.145s
ok  	github.com/cn-maul/rosetta-gateway/internal/inwire	0.078s
ok  	github.com/cn-maul/rosetta-gateway/internal/outwire	0.087s
ok  	github.com/cn-maul/rosetta-gateway/internal/ratelimit	0.064s
ok  	github.com/cn-maul/rosetta-gateway/internal/routing	0.056s
ok  	github.com/cn-maul/rosetta-gateway/internal/server	4.139s
ok  	github.com/cn-maul/rosetta-gateway/internal/snapshot	1.524s
ok  	github.com/cn-maul/rosetta-gateway/internal/store	111.181s
ok  	github.com/cn-maul/rosetta-gateway/internal/upstream	1.205s
ok  	github.com/cn-maul/rosetta-gateway/internal/userauth	0.424s
ok  	github.com/cn-maul/rosetta-gateway/internal/webui	0.062s
（全绿，未弄红任何别的包）
```

E2E 全部用例（25 个）：
```
$ go test ./cmd/gateway/ -run 'TestE2E' -v -count=1
--- PASS: TestE2E_A1_NonStreamHappyPathChargesAndRecords (0.32s)
--- PASS: TestE2E_A2_StreamHappyPathRecordsAndCharges (0.23s)
--- PASS: TestE2E_B_AuthRejectionsNeverReachUpstream (1.21s)
--- PASS: TestE2E_B2_RejectedRequestDoesNotCharge (0.59s)
--- PASS: TestE2E_C1_FailoverOn5xxPreservesChainOrder (0.17s)
--- PASS: TestE2E_C2_FailoverOn404And401 (0.40s)
--- PASS: TestE2E_C3_StreamCommittedThenFailedIsNotRetried (0.19s)
--- PASS: TestE2E_D1_QuotaExhaustedRejectsWithoutTouchingUpstream (0.17s)
--- PASS: TestE2E_D2_RPMExceededRejectsWithoutTouchingUpstream (0.26s)
--- PASS: TestE2E_D3_ReservationReleasedAfterSuccess (0.25s)
--- PASS: TestE2E_D4_ReservationReleasedAfterChainExhausted (0.27s)
--- PASS: TestE2E_F1_ClientDisconnectReleasesUpstreamAndMarksCanceled (0.20s)
--- PASS: TestE2E_E1_AdminDisableKeyTakesEffectWithoutRestart (0.31s)
--- PASS: TestE2E_E2_AdminReEnableKeyRestoresWithoutRestart (0.30s)
--- PASS: TestE2E_E3_AdminModelAllowlistTakesEffectWithoutRestart (0.17s)
--- PASS: TestE2E_Adv1_AdminKeyRejectedOnListModels (0.28s)
--- PASS: TestE2E_Adv2_DuplicateRequestIDChargesOnce (0.56s)
--- PASS: TestE2E_Adv3_NonStreamClientDisconnectReleasesReservation (3.23s)
--- PASS: TestE2E_Adv4_ConcurrentQuotaReserveDoesNotOverIssue (0.14s)
--- PASS: TestE2E_Adv5_FirstTokenTimeoutCutsHangingUpstream (0.45s)
--- PASS: TestE2E_Adv6_MalformedUpstreamBodyIsNotOK (0.23s)
--- PASS: TestE2E_Adv7_StreamRejectionUsesJSONNotSSE (0.17s)
--- PASS: TestE2E_Adv8_TruncatedStreamRecordsExactlyOnce (0.63s)
--- PASS: TestE2E_Adv9_RateLimitSharedAcrossIngressPaths (0.26s)
--- PASS: TestE2E_Adv10_ConcurrentSameRequestIDChargesOnce (0.62s)
PASS
ok  	github.com/cn-maul/rosetta-gateway/cmd/gateway	11.637s
```

注：`Adv6` 的**用例本身**通过（它只断言「不得 200 放行」，而实际是 500 —— 断言成立）；
它是通过**探针**进一步查证后才发现「500 且不可转移」这个更深的问题（§7）。

---

## 11. 未能验证的部分（诚实清单）

以下路径**没有**真正跑过，或只跑到了部分。列出来是因为「假装全覆盖」比缺口本身更有害。

1. **`/v1/responses`（OpenAI Responses 入口）完全没有 E2E 覆盖。**
   我只测了 `/v1/chat/completions` 与 `/v1/messages`（Adv9）。
   Responses 的编解码路径（`openaiResponsesCodec` + `outwire.ResponsesSSE`）本轮未实测。

2. **`GET /v1/models` 与 `GET /v1/models/{model}` 只测了鉴权分支**（Adv1），
   没有验证正常返回的 JSON 形状、`context_length` 元数据、`?include=upstream` 展开。

3. **多 provider / 多凭据轮换与凭据冷却未做 E2E。**
   所有用例都是单凭据；`MarkCredentialCooldown` 的 30min/1h 冷却、
   权重选择（`selectWeighted`）、熔断器的 half-open 探测名额
   （`ClaimTargetProbe`/`ReleaseTargetProbe`）都**没有**在动态测试里验证过。
   （`failover_test.go` 的 `TestFailover_NonTransferableErrorReleasesProbeSlot` 覆盖了
   探测名额的一部分，但那是既有测试，本轮未扩展。）

4. **TTL/时钟相关的行为未测**：TPM 的分钟窗口翻转、故障转移的 60s 熔断冷却到期后
   的半开探测。这些需要可控时钟或真实等待数十秒，本轮未做。

5. **总请求预算（`totalRequestBudget`，最长 30min）在真实链路上未被实测触发。**
   Adv5 验证的是**首字看门狗**（202ms 生效），不是总预算。要触发总预算需要构造
   一个「每跳都拖满」的链并等待分钟级时间，未做。

6. **客户端断开与上游 `stream.Close()` 的竞态窗口未做压力测试。**
   F1 是单次确定性验证（传播延迟 842µs），没有在高并发下反复断开以暴露时序边角。

7. **`-race` 未跑。** 任务未要求，且全量 `-race` 在 SQLite + 大量异步 goroutine 下
   耗时很长。我在假上游里**主动加了锁**（`fakeRecorder.mu`）以免自己的测试引入竞态，
   但「生产代码在当前 E2E 覆盖面下无竞态」**未被证明**。

8. **场景 E 只覆盖了三个管理写操作**（禁用 key / 启用 key / 收紧模型白名单）。
   provider / route / 链目标的增删改、以及「reload 失败后后台重试收敛」
   （`watchReloadRetry` + `dirty` 标志）都未实测。

9. **畸形响应只测了非流式的 `openai-chat`**（§7）。流式路径下的畸形 SSE 事件
   （SDK 会返回 "malformed event" 错误）是否同样不可转移，**未验证** ——
   按同样的根因（非类型化 `fmt.Errorf`）推测行为一致，但**这是推测，不是实测**。

---

## 12. 给 Lead 的建议

1. **优先决策 §7 的修法**（A/B 或 B+A 兜底）。这是本轮唯一的真实缺陷，
   影响「链能否吸收一类真实故障」与「排障方向是否正确」。
2. **补 `/v1/responses` 的 E2E**（§11.1）—— 它是三条入口之一，目前零动态覆盖。
3. 若时间允许，**补凭据冷却 / 熔断 half-open 的 E2E**（§11.3）——
   那部分逻辑（`upstream.go` 的探测名额与健康状态机）是审计里发现过多个
   微妙问题的地方，但它几乎没有端到端测试，只有单元级覆盖。
