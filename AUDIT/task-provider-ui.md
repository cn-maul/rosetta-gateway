# task-provider-ui — 上游与模型页面：凭据字段精简 + 单模型可用性测试

需求四条（用户原文）：

> 上游与模型页面，凭据里面，权重和状态，创建时间都去掉。右侧上游模型列表在操作一栏再加一个测试按钮，快速测试模型是否可用，provider的测试按钮去掉。

拆成四条的实现落点：

| # | 需求 | 状态 | 落点 |
|---|---|---|---|
| 1 | 凭据区域去掉「权重」「状态」「创建时间」三个字段（**仅展示**） | 完成 | `web/src/views/Providers.vue` 凭据子表 + 凭据表单 |
| 2 | 上游模型列表操作列新增「测试」按钮 | 完成 | `Providers.vue` 模型行 + `api.testModel` + `POST /admin/api/models/{id}/test` |
| 3 | provider 行上的「测试」按钮去掉 | 完成 | `Providers.vue` provider 行 |
| 4 | 权重字段后端仍在用，**不删列、不删后端字段** | 遵守 | 只改模板与前端表单；`provider_credentials.weight`、`credential_handler.go`、`upstream.selectWeighted` 全部未动 |

---

## 一、核心设计问题：单模型「可用」到底怎么测

### 结论：方案 A（真实发一次最小推理），`max_tokens=1`、非流式、单次

### 为什么不能复用现有的 provider 测试

现有 `ProviderHandler.Test`（`internal/admin/provider_handler.go:331`）调的是
`client.ListModels(ctx)`，即拉一次上游 `GET /models`。它证明的是：

> 「这个 endpoint + 这把凭据能拉到模型列表」

而需求问的是：

> 「这个具体的 model_id 现在能不能用」

两者是**不同的命题**，且差别恰好落在管理员最需要答案的几种情形上：

| 故障形态 | `/models` 能通？ | 真推理能通？ |
|---|---|---|
| 模型已下架 / 改名，但配置还留着 | ✅（列表里可能仍缓存着，或压根没查这一项） | ❌ |
| 账号对该模型无权限（常见于中转站按模型授权） | ✅ 看得见 | ❌ 403/404 |
| 名字写错 / 大小写不符 | ✅ | ❌ `model_not_found` |
| 该模型要求特定参数（如 reasoning 模型拒绝 temperature） | ✅ | ❌ |
| 凭据本身无效 | ❌ | ❌（这个才是两者都能测的） |

也就是说：**唯一只有真推理能回答的问题，正是需求要问的那个。** 这也是我没有选方案 B 的理由 —— B 在「模型下架 / 无权限 / 名字写错」这三类上会给出假绿，而这三类恰恰是「测试」按钮存在的意义。

### 为什么方案 A 的成本是可接受的

四道压低成本的措施，缺一不可：

1. **`max_tokens=1`**（`upstream.go` 的 `probeMaxTokens`）。
   回答「能不能推理」只需要上游接受请求并产出 1 个 token；生成 1 个和生成 1000 个对这个问题给出的答案完全相同，成本却差三个数量级。
2. **不取 0**。`max_tokens=0` 在 OpenAI 协议里语义是「由上游决定」，部分上游会因此生成到 stop 或模型上限 —— 一次探测就变成一次完整且计费可观的推理。这是把「最小」落实成常量的理由，不是随手写 1。
3. **非流式**。流式会在同一件事上额外引入 SSE 解析、首字（TTFT）看门狗、断流判定三条失败路径，而探测要回答的问题不需要其中任何一条。走非流式也让「耗时」这个数字干净可比。
4. **不请求思考**（`Thinking: nil`，显式）。推理模型的 thinking 预算会**绕过 max_tokens 下限**：SDK 的 `anthropicPlan`（`provider_anthropic.go:184-188`）在 `budget >= maxTokens` 时会把 `max_tokens` 顶到 `budget+4096`。不显式关掉思考，一次「最小」探测在 Anthropic 协议上可能变成几千 token。这一条不写就静默失效，所以在代码里显式置 nil 并注明。

**一次探测的真实成本量级**：输入约 1~2 token（提示词就是 `"hi"`），输出 1 token。按主流单价（如 1 元/百万输入 token、2 元/百万输出 token）计，单次约 **0.0001 元以下**，即万分之一分。所以「测试」按钮可以放心点。

### 一次探测最多只发生一次计费调用（不会因重试翻倍）

`buildClient`（`upstream.go:891`）已经设了 `rosetta.WithQuirks(rosetta.Quirks{NoIdempotencyKey: true})`，SDK 因此把对话 POST 标为 `RetryDefault` 而不是 `RetryIdempotent`。`httpx.canRetry`（`internal/httpx/httpx.go:188-201`）对 `RetryDefault` 只放行 GET/HEAD/OPTIONS/PUT/DELETE —— **POST 不重试**。所以一次探测在最坏情况下也只对上游产生一次计费调用。

这条对成本安全性是必要的：若 POST 会被 SDK 重试，`max_tokens=1` 的「最小」承诺就在传输层错误 / 429 / 5xx 时失效（每次重试都是一次真实生成）。

### 失败算谁的账 —— 明确不属于任何用户

**确认：不涉及用户余额，也不产生 `usage_records` 行。** 理由在调用链上：

1. 探测走的是 `upstream.TestUpstreamModel` → `NewProviderClient` → `resolveCredential`，用的是 **provider 自带凭据**（`provider_credentials.api_key_enc` 里解密出来的那把上游 key），不是网关签发的 `sk-gw-` 访问密钥。
2. 计费与用量落库的唯一入口是 `cmd/gateway` 里的 `usage.record(...)`（`main.go` 的 3 处调用点，全在 `attemptStream` / `attemptNonStream` 里）与 `usageRecorder.charge`。探测完全不经过 `handleIngress` / 认证 / 配额 / 余额预检这条链，因此既不写 `usage_records`，也不调 `ChargeBalance`。
3. 结果：**花掉的是 provider 的钱，网关侧无账可记**。这正是可以把探测做成零余额门槛的管理员工具的原因 —— 与「管理员余额豁免」（`billing.go` 的 `balanceExempt`）无关，不是靠豁免绕过的。

### 我考虑过但没选的方案：C（让用户选 A/B）

不选的理由：需求原话是「**快速测试模型是否可用**」——「可用」指向真实推理，「快速」由 `max_tokens=1` + 非流式满足。给两个按钮会让管理员在「快但可能假绿」和「真但花钱」之间做选择，而 B 的假绿恰好是最容易误导人的形态（一次绿色让人以为模型没问题，直到真实流量 404）。用一个按钮 + 一条诚实的成本说明（探测会真实计费、量级万分之一分以下）比把判断责任推给运维更合适。如果将来确实需要零成本校验，「获取模型列表」按钮（`runDiscover`）已经在那里了 —— 它本来就是 B。

---

## 二、接口形状

### 新增：`POST /admin/api/models/{id}/test`

- `{id}` 是 **`upstream_models.id`**（主键），不是 `model_id` 字符串。
  理由：`model_id` 只在单个 provider 内唯一，跨 provider 会重名，做路径参数无法唯一定位；且它可能含 `/` 等需要转义的字符。
- 注册在 `adminMux`（`cmd/gateway/main.go`），因此自动获得：
  - **管理员鉴权**：`AdminGateGuard` 是**前缀白名单**，`/admin/api/models/` 不在白名单里 → 自动要求 admin。这里没有额外写 `requireAdmin`，因为白名单机制就是为此设计的（见 `server/user_auth.go:120-157` 的说明：漏标 = 显式 403，而不是静默越权）。
  - **写操作审计**：`server.AutoReload` 对 POST 记审计（字段名）。它**不会**触发快照重建 —— `AutoReload` 的跳过规则里有 `strings.HasSuffix(p, "/test")`（`internal/server/autoreload.go:97`），与 provider 的 Test 同一条规则。这是对的：探测只读上游，不改任何配置，重建纯属开销。

### 响应体

```jsonc
{
  "status": "ok",            // "ok" | "error"
  "model_id": "deepseek-chat", // 被探测的上游模型名，原样回显
  "provider_id": "p1",
  "message": "模型可用，842ms",
  "latency_ms": 842,
  "input_tokens": 3,          // omitempty
  "output_tokens": 1,         // omitempty
  "in_band": false            // omitempty
}
```

### HTTP 状态码的取舍：失败也回 200

**除「id 不存在」回 404 外，一律 200**，成功/失败放在 `status` 字段。理由与 provider 的 Test 同口径：

探测失败的原因有十几种（凭据错、模型不存在、无权限、上游 5xx、超时、网络不通），**每一种都是探测的结论，而不是「这个管理接口调用失败」**。用 4xx/5xx 表达它们，前端只能拿到 `ApiFail` 里那句统一的错误文案，反而把真正的诊断信息丢掉了 —— 而那正是管理员唯一能据以行动的东西（401 换 key、404 改模型名、403 开权限）。

只有「你调的 id 不存在」用 404：那是调用方用错了接口，不是模型不可用。混在一起会让前端把拼错的 id 显示成「模型测试失败」，运维于是去查上游，而真正的问题是路径。

### 新增字段 `in_band` 的必要性

部分中转网关用 **HTTP 200 + 错误体**回绝请求（如额度耗尽）。只看状态码会把「200 但没推理」判成可用。SDK 已经把这种形态标成 `APIError.InBand`（`provider_openai_chat.go:775-780`），这里把它透出来，前端加一句「（上游以 HTTP 200 返回错误）」—— 否则「HTTP 成功却报错」看起来像网关自己坏了。

### 保留：`POST /admin/api/providers/{id}/test` 与 `api.testProvider`

**只去掉按钮，后端与前端 api 封装都保留。** 理由：

1. **零成本**。不删除就不产生任何运行时开销，删了反而多一次「将来要用再写回来」的机会成本。
2. **仍然有独占用例**。它测的是「endpoint + 凭据」这一层：换完 key 想确认凭据本身有效、或者 provider 下列表拉不到但想确认网络通路 —— 这时它比模型级探测更直接（不打推理、不花钱、`GET` 可安全重试）。
3. **对脚本/curl 调用方仍然有价值**。它是有文档、有测试的既有契约（`provider_handler.go:329`），删掉会让任何既有运维脚本 500。
4. 界面上不提供入口，就不会产生「比模型级测试更弱的绿色信号」这个误导（见模板里的注释）。

---

## 三、安全与正确性要点

- **不回显内网响应体**。上游错误经 rosetta 的 `APIError.Error()` 透出，而它已对所有字段做凭据掩码（`httpx.MaskSecrets`，见 `errors.go:86-103`），并以 `safeURL` 处理 URL。注意这与 `fetchModels` 的**刻意不同**：那里（`upstream.go:780-796`）明确把响应体**只进日志不进 error**，因为 discover 允许管理员把 endpoint 指向内网，回显响应体会把盲打变成读取原语。这里我选择透传上游错误消息，判断依据是：探测打的是**推理端点**，返回的是模型语义错误（`model_not_found` / `insufficient_quota`），而这正是管理员必须看到才能行动的信息 —— 一个不含原因的红点等于没测。掩码已由 SDK 保证。若将来要更保守，改法是只回 `StatusCode` + 归类文案，但那会显著降低这个功能的可用性。

- **`cfg` 为 nil 不 panic**。`ModelHandler` 的既有端点（List/Create/Update/Delete）都不碰 `h.cfg`，所以 `NewModelHandler(st, nil, nil)` 是既有测试与调用点的合法用法（`model_handler_test.go:20` 就是这么写的）。`Test` 是第一个真正依赖 cfg 的路径（算超时 + 交给 `NewProviderClient`），请求路径上 deref nil 会把一个 500 变成 panic。已加兜底（回落 `config.Default()`）并配测试 `TestModelTest_NilConfigDoesNotPanic`。

- **停用的模型也能测**（有测试固定这个行为）。管理员在「启用」之前最需要知道的就是它能不能用；加 `enabled` 前置判定只会把探测推后到启用之后，那时坏模型已进候选池。

- **防连点复用既有的 Set 机制**，没有新造一套。`testing` 是 `ref(new Set<string>())` + 整体替换赋值 + 按 id 清除 —— 这正是 2026-10-10 修掉的那个 P1（单字符串会让跨行状态互相冲掉）的正确形态。现在它服务模型行；provider 行不再有按钮，但机制本身保持不变，将来加回任何一行测试都直接可用。

- **管理接口失败 ≠ 模型不可用**。前端把两条路径分开：`status=error`（HTTP 200，模型/凭据/权限问题）显示上游原因；`catch`（网络/超时/5xx）显示「管理接口调用失败：…」。混在一起会把网关自己的问题读成上游模型的问题。

- **探测结果留在行内，不只弹 toast**。探测会真实计费，失败原因是唯一可据以行动的信息，而 toast 5.2 秒后消失、并发时单例 toast 还会被后来的顶掉（只剩最后一条）。结果按 `upstream_models.id` 索引存在 `modelTestResults` 里，每行独立显示。

- **超时**：后端取 `min(cfg.UpstreamTimeout(), 60s)`（≤0 时回落 60s）。一次 `max_tokens=1` 的调用不该更久，真卡住要报出来而不是让管理请求跟着挂住。前端沿用既有的 `testTimeoutMs`（`upstream_timeout_ms + 5s`，读设置失败时 150s+5s），让后端先一步超时、把真实原因带回来。

---

## 四、去掉凭据「权重 / 状态 / 创建时间」展示的副作用（必读）

**副作用 1：界面上再也无法调整凭据权重。** 这是用户明确要求的结果，但影响要写清楚：

- `provider_credentials.weight` 列**还在**，`upstream.Pool.selectWeighted`（`upstream.go:615`）照常按它做加权轮询 —— 也就是说**已有凭据的权重仍然生效**，只是看不见也改不了。
- 新建凭据的权重由后端兜底为 **1**（`credential_handler.go:92-95` 的 `weight <= 0 → 1`；表单不再发这个字段，`derefInt(nil) == 0`）。
- 编辑已有凭据时前端**不发** `weight`（PATCH 语义里 `nil` = 保持原值），所以**已有凭据的权重不会被静默重置成 1**。这一点是刻意保证的：表单里删掉输入框时最容易犯的错就是顺手发一个 `weight: 1`，那会把所有已调过的权重抹平，且没有任何提示。
- 后果：若某个 provider 有多把凭据且**权重不同**（例如主账号 10、备用 1），现在只能通过 API/直接改库调整。多凭据场景下这会让「主备分流比例」变成不可运维项。需求如此，照做。

**副作用 2：失去凭据冷却状态的可见性。** 被去掉的「状态」列显示的是 `provider_credentials.status`（`healthy` / `cooling`），它随上游冷却态变化（由数据面在 401/5xx 时写入，见 `store.SetCredentialCooldown`）。现在运维无法从界面上看出「这把 key 正在冷却」，只能从 provider 级「未就绪」徽标或日志反推。

**副作用 3：失去创建时间。** `created_at` 只用于展示，去掉无功能影响。但排障时「这把 key 是什么时候加的」不再可见。

**没有做的事**（刻意）：不删数据库列、不删 `credentialResponse.Weight/Status/CreatedAt`、不改 `Credential` 类型、不改 PATCH 语义。接口仍然下发这三个字段，前端只是不渲染 —— 任何既有 API 调用方零影响。

---

## 五、改动清单

| 文件 | 改动 |
|---|---|
| `internal/upstream/upstream.go` | 新增 `probeMaxTokens` / `probeInput` / `ModelProbeResult` / `TestUpstreamModel`；仅为 `errors.As` 加 `errors` import |
| `internal/admin/model_handler.go` | 新增 `modelTestResponse` / `ModelHandler.Test`；`fmt` import |
| `cmd/gateway/main.go` | **仅新增 1 行路由注册**（`POST /admin/api/models/{id}/test`）+ 注释；写前重读，未触碰其它任务的行 |
| `internal/admin/model_test_handler_test.go` | 新增 7 条测试 |
| `web/src/views/Providers.vue` | 凭据子表删 3 列、凭据表单删权重输入、provider 行删测试按钮、模型行加测试按钮 + 结果行、`testModel`/`testResultLine`/`modelTestResults`、样式 |
| `web/src/api.ts` | 新增 `api.testModel`；`ModelTestResult` import |
| `web/src/types.ts` | 新增 `ModelTestResult` |

**未改**：`provider_handler.go`（provider Test 保留）、`credential_handler.go`、`store` 层、任何 DB schema、`internal/webui/dist`（按指示**没有**跑 `sync-embed.mjs`，也没有 commit）。

---

## 六、测试

`internal/admin/model_test_handler_test.go`，7 条全绿。**全部用 `httptest` 假上游，绝不碰外网**；每条断言都指向一个具体的回归形态：

| 测试 | 守的是什么 |
|---|---|
| `TestModelTest_UnknownIDIs404` | 不存在的 id 是 404，不是 200+error。混在一起会让拼错的路径显示成「模型测试失败」 |
| `TestModelTest_NoCredentialFailsWithoutTouchingUpstream` | 缺凭据在建客户端阶段就失败；用假上游**命中计数器断言 0 次请求** —— 只断言「返回了错误」的话，错误可能来自上游，那就完全没测到 `resolveCredential` |
| `TestModelTest_SendsMinimalRealInferenceToTheRightModel` | 核心断言：打的是 `/chat/completions`（是推理，不是 `/models`）、`model` == 该行配置的 `model_id`、输出上限**恰为 1**、没有 `stream=true` |
| `TestModelTest_UpstreamModelNotFoundSurfacesReason` | 上游 404 的原因必须透传（管理员唯一能据以行动的信息），且 `InBand=false` |
| `TestModelTest_InBandErrorIsNotSuccess` | HTTP 200 + 错误体必须判成 error 且 `InBand=true`；只看状态码会把「200 但没推理」误判为可用 |
| `TestModelTest_DisabledModelIsStillProbed` | 停用模型仍可探测，且恰好 1 次请求 |
| `TestModelTest_NilConfigDoesNotPanic` | `cfg == nil` 不 panic（既有调用点就传 nil） |

**为什么第 3 条要断言请求体而不只断言响应**：一个「压根没打上游、直接回 ok」的实现也能让纯响应断言全绿。这正是本仓库记过的假信号形态（`cmd/gateway/upstream_mapping_test.go:60-66` 的注释：fake 上游不断言收到的 model，测试全绿放行）。所以 fake 上游必须记录并断言实际发出的 body。

---

## 七、验证结果（全部实际执行）

```
gofmt -l cmd internal      → 无输出（exit 0）
go build ./...             → exit 0
go vet ./...               → exit 0
go test -count=1 ./...     → exit 0（16 个包全 ok；admin 20.4s，store 30.7s）
vue-tsc --noEmit           → exit 0
vite build                 → exit 0（built in 1.59s）
```

用指定的 node 执行：
`C:\Users\louis\.dsh\dsh-runtimes\dsh-primary-runtime\dependencies\node\bin\node.exe`（v24.21.0）。

按指示**没有**跑 `sync-embed.mjs`、**没有** `git commit`。`vite build` 只写入了 gitignore 的 `web/dist`。工作区里其它任务的未提交改动一律未触碰。

---

## 八、留给人类决策的一点

`max_tokens=1` 在极少数上游上可能被拒（例如某些服务要求 `max_tokens >= 2`，或对 reasoning 模型有最小值）。当前实现**不**做参数降级重试 —— 若那个上游真的拒绝，探测会报出它的原话（如 `max_tokens must be at least 2`），管理员据此知道「这个上游的最小输出限制」，这本身就是有用信息。

若实测发现某个目标上游稳定因此误报，最小改动是让 `probeMaxTokens` 可配（或对该 provider 用一个稍大的值）—— 但那会把「成本可控」的论证从「常量 1」变成「取决于配置」，所以我没有预先引入。这个取舍留在这里，等你确认目标上游后再定。
