# Rosetta Gateway 设计文档

| 项 | 值 |
|---|---|
| 项目名 | **rosetta-gateway**（2026-09-15 定） |
| 项目性质 | **独立新项目**，不在 Rosetta 上改造（2026-09-15 定） |
| 目录 | `C:\Users\louis\Desktop\project\rosetta-gateway` |
| module path | `github.com/cn-maul/rosetta-gateway`（建议值，仓库创建后确认） |
| 依赖 | `github.com/cn-maul/rosetta` v0.4.0+ |
| 版本 | v0.1 草案 |
| 日期 | 2026-09-15 |
| 状态 | 待评审 |

---

## 1. 定位

**做什么**：一个跑在局域网的 LLM 网关。上游接若干 AI 服务商（provider），下游对外只暴露一个接入地址。网关把 `(provider, 上游模型)` 这层二维复杂性挡在后面，对外给出干净的模型名。

**核心诉求**（来自需求确认）：

1. 多个 provider，每个 provider 挂自己的模型列表，**不同 provider 的同名模型互不干扰**
2. 对外可以走 OpenAI Chat / OpenAI Responses / Anthropic Messages 三种协议
3. 局域网可用，单点接入
4. 通过 **Web 管理界面**切换上游
5. 下游 **Key 管理与配额限速**，配额**按 token 计量**
6. 二维命名空间对外用 **「虚拟模型名 + `provider/model` 前缀」双轨**暴露

**明确不做**：

- 不做 prompt 编排 / Agent 框架 / 会话托管
- 不做 RAG、向量库
- 不做计费结算与账务（只做用量记账，不做钱）
- 不做上游 Responses 的会话状态托管（`previous_response_id` 走 `Extra` 透传）
- 不做 Embeddings / Rerank 的对外入口（Rosetta 已具备上游能力，P3 之后再评估）
- 不做多节点集群（单进程，SQLite 本地文件）

---

## 2. 关键决策速览

| # | 决策 | 选择 | 理由 |
|---|---|---|---|
| D1 | 是否并入 Rosetta | **独立新项目、独立 module，Rosetta 仅作 `require`**（2026-09-15 已定） | 依赖方向、SemVer 冻结节奏、质量门禁、部署形态（见 §2.1） |
| D2 | 二维命名空间怎么对外 | **双轨**：虚拟模型名（主）+ `provider/model` 前缀（快捷） | 生态零改动 + 零配置快捷路径；OpenRouter / LiteLLM 同款 |
| D3 | 配额计量 | **token 数**（input + output） | 直接取上游 usage，无需维护单价表 |
| D4 | 配置的事实来源 | **数据库唯一**，配置文件只管进程级参数 + 首次 bootstrap | 避免「界面改了、重启被配置文件覆盖」 |
| D5 | 热更新机制 | 内存快照 + `atomic.Pointer` 原子替换，请求路径无锁 | 路由热改不影响在途请求 |
| D6 | 上游凭证 | 每凭证一个 `rosetta.Client` 实例，由网关池化管理 | Rosetta 的 endpoint/key 是 per-client 的（见 §10） |
| D7 | 配额检查时机 | **请求前粗检 + 请求后按真实 usage 扣减**，容忍超发 | 流式下 token 只能在流结束后得知，事前精确拒绝不存在 |
| D8 | 流空闲超时 | **网关自己实现看门狗** | Rosetta 无此能力（已核实，全仓无 idle 相关实现） |
| D9 | `/v1/models` 形状冲突 | 按认证头分流 + 显式别名路径 | OpenAI 与 Anthropic 的该路径完全相同、响应形状不同 |
| D10 | 前端形态 | `go:embed` + 原生 HTML/fetch，不上框架 | 内网管理页不超过 8 个，构建链收益不成比例 |
| D11 | 配置格式 | JSON | 零依赖，与 Rosetta 的 models 文件一致 |
| D12 | SQLite 驱动 | `modernc.org/sqlite`（纯 Go） | 交叉编译无 cgo，保持单二进制干净 |

### 2.1 为什么独立项目（D1 展开）

| 约束 | 说明 |
|---|---|
| 依赖方向 | Rosetta 的招牌是零第三方依赖。网关必然引入 SQLite 驱动等依赖，并进同一 module 会污染消费者的 `go.sum` |
| 发布节奏 | Rosetta `PLAN.md` §3 承诺 v0.1.0 后按 SemVer 冻结公开 API。网关是快速迭代的运营代码，混入会反复撬动 SDK API |
| 质量门禁 | Rosetta 现有 86% 覆盖率、`-race` 全量、审计回归测试、benchmark 基线。网关的 IO/配置/调度代码混入会稀释这些门禁 |
| 部署形态 | 库没有进程；网关是单二进制 + `cmd/` + 静态资源，与 `examples/` 语义冲突 |

**已定（2026-09-15）：走独立新项目，不在 Rosetta 上改造。**

具体含义：

- 不在 Rosetta 的 module 里新增任何网关代码，Rosetta 的公开 API、go.mod、CI 门禁全部不动
- 网关自己的依赖（SQLite 驱动等）只进网关的 `go.mod`
- 两边独立发布：Rosetta 继续按 SemVer 冻结；网关按自己的节奏迭代
- Rosetta 在网关的 `go.mod` 里只以 `require github.com/cn-maul/rosetta` 出现
- Rosetta 那边唯一可能要动的是附录 B 列出的能力（导出层、流空闲超时、per-request 覆盖），且**等网关写完再提**，不占用网关的排期

---

## 3. 架构

```
                     局域网客户端
        Cherry Studio / OpenWebUI / Dify / Claude Code / 脚本
                              │
                              │  POST /v1/chat/completions
                              │  POST /v1/responses
                              │  POST /v1/messages
                              ▼
        ┌──────────── Rosetta Gateway（单进程） ────────────┐
        │                                                   │
        │  server        路由装配 · 中间件 · 错误映射         │
        │  auth          下游 Key 校验（SHA-256 索引）        │
        │  quota         配额检查 · 用量扣减 · 限速          │
        │  inwire        [下游 → 统一模型] 请求解码           │
        │  routing       模型名解析 → (provider, model)      │
        │  upstream      凭证池 · 冷却 · 故障转移             │
        │  outwire       [统一模型 → 下游] 响应/SSE 编码      │
        │  snapshot      运行时配置快照（atomic.Pointer）     │
        │  store         SQLite：配置 + 用量 + 日志           │
        │  admin         管理 API                             │
        │  webui         embed 静态页面                       │
        │                                                   │
        │         ┌──────── rosetta.Client 池 ────────┐      │
        │         │ 每 (provider, credential) 一个实例  │      │
        └─────────┴──────────────────┬───────────────┴──────┘
                                     │
              ┌──────────┬───────────┼───────────┬──────────┐
              ▼          ▼           ▼           ▼          ▼
           DeepSeek   Anthropic    OpenAI      Ollama      vLLM
```

**请求生命周期（流式）**：

```
下游 HTTP 请求
  → auth       校验 sk-gw-... → access_key 记录
  → quota      读 used_tokens，超配额即拒（429）
  → inwire     按入口协议解码 body → *rosetta.ChatRequest
  → routing    解析 model → 确定 (provider, upstream_model)
  → upstream   选凭证（池 + 冷却状态），构造/取出 rosetta.Client
  → client.ChatStream(ctx, req)          ← ctx 来自 r.Context()
      ├─ 返回 error  → 映射状态码，写 JSON 错误响应，结束
      └─ 返回 stream → 写 200 + text/event-stream
                      启动看门狗 + 心跳
                      loop: stream.Next() → outwire 编码 → Flush
                      结束：EventMessageEnd → usage 入账 + 下游结束事件
  → store      异步写入 usage_records
```

---

## 4. 数据模型

SQLite，WAL 模式，`foreign_keys=ON`。

```sql
-- 上游服务商
CREATE TABLE providers (
  id            TEXT PRIMARY KEY,          -- uuid
  slug          TEXT NOT NULL UNIQUE,      -- 前缀语法用，限定 [a-z0-9-]{2,32}
  name          TEXT NOT NULL,             -- 显示名
  protocol      TEXT NOT NULL DEFAULT 'auto',
                                           -- auto|openai-chat|openai-responses|anthropic
  endpoint      TEXT NOT NULL,
  enabled       INTEGER NOT NULL DEFAULT 1,
  timeout_ms    INTEGER NOT NULL DEFAULT 0,    -- 0 = 用全局默认
  max_retries   INTEGER NOT NULL DEFAULT 2,
  quirks_json   TEXT,                      -- 透传 rosetta.Quirks
  created_at    INTEGER NOT NULL,
  updated_at    INTEGER NOT NULL
);

-- 一个 provider 下的多把上游 key
CREATE TABLE provider_credentials (
  id            TEXT PRIMARY KEY,
  provider_id   TEXT NOT NULL REFERENCES providers(id) ON DELETE CASCADE,
  label         TEXT,
  api_key_enc   BLOB NOT NULL,             -- AES-GCM 密文，主密钥来自环境/文件
  enabled       INTEGER NOT NULL DEFAULT 1,
  weight        INTEGER NOT NULL DEFAULT 1,
  status        TEXT NOT NULL DEFAULT 'healthy',  -- healthy|cooling|disabled
  cooldown_until INTEGER NOT NULL DEFAULT 0,
  last_error    TEXT,
  created_at    INTEGER NOT NULL
);

-- provider 下声明的上游模型
CREATE TABLE upstream_models (
  id                TEXT PRIMARY KEY,
  provider_id       TEXT NOT NULL REFERENCES providers(id) ON DELETE CASCADE,
  model_id          TEXT NOT NULL,          -- 上游真实模型名，可含 "/"
  display_name      TEXT,
  enabled           INTEGER NOT NULL DEFAULT 1,
  context_window    INTEGER,                -- 覆盖 rosetta 注册表
  max_output_tokens INTEGER,
  supports_thinking INTEGER,                -- NULL = 不覆盖
  default_extra_json TEXT,                  -- 该模型固定附加的上游字段
  UNIQUE (provider_id, model_id)
);

-- 对外虚拟模型名（D2 的轨道一）
CREATE TABLE routes (
  id                TEXT PRIMARY KEY,
  public_name       TEXT NOT NULL UNIQUE,   -- 对外 model 名
  provider_id       TEXT NOT NULL REFERENCES providers(id) ON DELETE RESTRICT,
  upstream_model_id TEXT NOT NULL REFERENCES upstream_models(id) ON DELETE RESTRICT,
  enabled           INTEGER NOT NULL DEFAULT 1,
  priority          INTEGER NOT NULL DEFAULT 0,   -- 预留：故障转移候选顺序
  fallback_route_id TEXT REFERENCES routes(id) ON DELETE SET NULL,
  extra_json        TEXT,
  created_at        INTEGER NOT NULL
);

-- 下游访问凭证（D7 的配额载体）
CREATE TABLE access_keys (
  id            TEXT PRIMARY KEY,
  key_hash      TEXT NOT NULL UNIQUE,      -- SHA-256(明文)，不存明文
  key_prefix    TEXT NOT NULL,             -- 形如 "sk-gw-a1b2"，用于界面展示
  name          TEXT NOT NULL,
  enabled       INTEGER NOT NULL DEFAULT 1,
  expires_at    INTEGER NOT NULL DEFAULT 0,     -- 0 = 不过期
  quota_tokens  INTEGER NOT NULL DEFAULT 0,     -- 0 = 不限
  used_tokens   INTEGER NOT NULL DEFAULT 0,     -- 累计，只增
  rpm_limit     INTEGER NOT NULL DEFAULT 0,     -- 0 = 不限
  tpm_limit     INTEGER NOT NULL DEFAULT 0,     -- 0 = 不限
  created_at    INTEGER NOT NULL,
  last_used_at  INTEGER NOT NULL DEFAULT 0
);

-- 逐请求用量
CREATE TABLE usage_records (
  id                TEXT PRIMARY KEY,
  ts                INTEGER NOT NULL,
  access_key_id     TEXT NOT NULL,
  public_model      TEXT NOT NULL,         -- 下游看到的名字
  provider_id       TEXT NOT NULL,
  upstream_model    TEXT NOT NULL,
  ingress_protocol  TEXT NOT NULL,         -- openai-chat|openai-responses|anthropic
  stream            INTEGER NOT NULL,
  input_tokens      INTEGER NOT NULL DEFAULT 0,
  output_tokens     INTEGER NOT NULL DEFAULT 0,
  total_tokens      INTEGER NOT NULL DEFAULT 0,
  reasoning_tokens  INTEGER NOT NULL DEFAULT 0,
  cached_tokens     INTEGER NOT NULL DEFAULT 0,
  usage_state       TEXT NOT NULL,         -- reported|estimated|missing
  status            TEXT NOT NULL,         -- ok|error|truncated|canceled
  http_status       INTEGER NOT NULL,
  error_code        TEXT,
  latency_ms        INTEGER NOT NULL,
  ttfb_ms           INTEGER NOT NULL DEFAULT 0,
  request_id        TEXT
);

CREATE INDEX idx_usage_ts        ON usage_records(ts);
CREATE INDEX idx_usage_key_ts    ON usage_records(access_key_id, ts);
CREATE INDEX idx_usage_model_ts  ON usage_records(public_model, ts);
CREATE INDEX idx_usage_prov_ts   ON usage_records(provider_id, ts);
```

**不做的事**：`usage_records` 不做分区与归档策略（单机局域网量级，一年也到不了千万行）；`used_tokens` 不做对账重算（如果真要，可以用 `SUM(usage_records)` 定期校正——记入 P2 可选）。

---

## 5. 路由解析（D2）

### 5.1 解析规则

输入：下游请求里的 `model` 字符串。输出：`(provider, upstream_model, route)` 或错误。

```
resolve(model):
  1. 查内存快照的 routes 索引:
       若 public_name == model 且 enabled → 命中（轨道一）
  2. 否则若 model 含 "/":
       slug = model 到第一个 "/" 之前的部分
       若存在 enabled 的 provider 且 slug 匹配:
           rest = model 中第一个 "/" 之后的部分
           若该 provider 下存在 (model_id == rest) 且 enabled 的 upstream_model → 命中（轨道二）
       未命中则继续
  3. 返回 404 model_not_found
```

**关键细节**：

- **轨道一优先于轨道二。** 若虚拟名恰好含 `/`，仍然优先按虚拟名精确匹配，保证可控性。
- **只切第一个 `/`。** 上游模型名本身可能带斜杠（OpenRouter 风格 `anthropic/claude-3.5-sonnet`），写法是 `openrouter/anthropic/claude-3.5-sonnet`。
- **轨道二零配置。** 只要 provider 和模型在库里 enabled，就能直接用 `slug/model` 调用，不需要建 route。
- **撞名校验。** 管理界面在创建 `public_name` 时，若其与任一 provider 的 `slug` 相同则给出警告（不阻断，但明确提示该名字的无斜杠请求会走轨道一）。
- 解析结果进快照，O(1) 查表。

### 5.2 对外模型列表

`GET /v1/models` 默认**只返回轨道一的虚拟名**。轨道二的形式不列出（可能非常长，且上游模型数会随 provider 增长）。可用 `?include=upstream` 展开。

---

## 6. 对外接口

### 6.1 下游入口

| 路径 | 协议 | 备注 |
|---|---|---|
| `POST /v1/chat/completions` | OpenAI Chat | P0 |
| `POST /v1/responses` | OpenAI Responses | P3 |
| `POST /v1/messages` | Anthropic Messages | P3 |
| `GET /v1/models` | 形状按认证头分流（D9） | P0 只出 OpenAI 形状 |
| `GET /openai/v1/models` | 强制 OpenAI 形状 | 别名 |
| `GET /anthropic/v1/models` | 强制 Anthropic 形状 | 别名 |

**D9 冲突说明**：OpenAI 与 Anthropic 的模型列表**路径完全相同**（都是 `GET /v1/models`），但响应结构不同：

```json
// OpenAI
{ "object": "list", "data": [ { "id": "gpt-4o", "object": "model", "created": 0, "owned_by": "gateway" } ] }

// Anthropic
{ "data": [ { "id": "claude-sonnet-4-5", "type": "model", "display_name": "...", "created_at": "..." } ],
  "has_more": false, "first_id": "...", "last_id": "..." }
```

分流规则，按序：

1. 显式别名路径（`/openai/...`、`/anthropic/...`）永远优先
2. 请求带 `x-api-key` 且不带 `Authorization: Bearer` → Anthropic 形状
3. 其余 → OpenAI 形状

### 6.2 下游鉴权

接受三种承载方式，任一命中即通过：

| 方式 | 头 / 参数 |
|---|---|
| Bearer | `Authorization: Bearer sk-gw-...` |
| Anthropic 风格 | `x-api-key: sk-gw-...` |
| Query（兼容兜底） | `?key=sk-gw-...` |

校验：`SHA-256(明文)` 查 `access_keys.key_hash` 索引。上游 Key 是高熵随机串，无需 bcrypt 类慢哈希。

失败返回 401；Key 被停用返回 403；过期返回 401；配额耗尽返回 429。

### 6.3 管理接口

统一挂在 `/admin/api/*`。管理鉴权独立于下游 Key：凭据存放在**可执行文件同级的 `admin_auth.json`**
（PBKDF2-HMAC-SHA256 + 随机盐，见 `internal/adminauth`），`config.json` 的 `admin_token`
仅作为「尚未设置密码」时的兜底与应急恢复通道。**凭证是运行时状态，设置后立即生效、无需重启。**

| 端点 | 放行规则 |
|---|---|
| `GET /admin/api/password/check` | 恒放行（只返回布尔值） |
| `POST /admin/api/password/set` | 仅当系统尚无任何凭据时放行；已有凭据则必须带正确的旧凭据 |
| `GET /admin/api/auth/verify` | 需鉴权（给前端「先验证再保存」用） |
| 其余 | `Authorization: Bearer <密码或 admin_token>` |

```
GET    /admin/api/providers                     列表
POST   /admin/api/providers                     新建
PATCH  /admin/api/providers/{id}
DELETE /admin/api/providers/{id}
POST   /admin/api/providers/{id}/test           连通性测试（调 ListModels）

GET    /admin/api/providers/{id}/credentials
POST   /admin/api/providers/{id}/credentials
PATCH  /admin/api/credentials/{id}
DELETE /admin/api/credentials/{id}

GET    /admin/api/providers/{id}/models
POST   /admin/api/providers/{id}/models
POST   /admin/api/providers/{id}/models/discover  从上游 /models 拉取并批量导入
PATCH  /admin/api/models/{id}
DELETE /admin/api/models/{id}

GET    /admin/api/routes
POST   /admin/api/routes
PATCH  /admin/api/routes/{id}
DELETE /admin/api/routes/{id}

GET    /admin/api/keys
POST   /admin/api/keys                          返回明文一次
PATCH  /admin/api/keys/{id}
DELETE /admin/api/keys/{id}

GET    /admin/api/usage?from=&to=&group_by=key|model|provider|day
GET    /admin/api/stats                         当前快照：总请求/总 token/错误率/各 provider 健康
POST   /admin/api/reload                        从 DB 重建内存快照
```

所有写操作的事务边界：**先写 DB，提交成功后再重建快照**。DB 写失败则快照不动。

#### PATCH 语义：字段级部分更新

PATCH 端点一律「只看请求体里出现了哪些字段」：

| 请求体 | 行为 |
|---|---|
| 字段不出现（或 `null`） | 保持数据库原值 |
| 字段出现 | 显式赋新值。**空串 / `0` 都是合法值**，会真正落库（空串落 `NULL`） |
| 必填字段显式传空串 | `400`，而不是静默忽略 |

必填字段指 `name` / `endpoint` / `protocol` / `model_id` / `public_name` / `provider_id` /
`upstream_model_id` / `api_key`。**静默忽略是最坏的选项** —— 用户会以为改成功了。

因此两个直接结论：

- **编辑时只发你要改的字段即可**，不必回传完整对象。
- `priority: 0`、`fallback_route_id: ""`、`timeout_ms: 0`（=用全局默认）、
  `context_window: 0`（=未设置）都是可表达的意图，不再是「空值即忽略」。

实现约束：结构体的标量字段必须是指针，合并处写 `if req.X != nil { ... }`。
用 `if *req.X != ""` 或解引用后判断零值的写法会把「显式置空」重新变回「未提供」，
等于回到旧行为 —— `internal/admin/helpers.go` 的 `derefStr` / `derefInt`
**只准用于 Create 这类「缺省即零值」的场合**。

例外：`providers` 的 `slug` 创建后不可修改（它是对外引用的稳定标识），
PATCH 结构体里刻意不含该字段，传了也会被忽略。

---

## 7. 协议转换边界（网关 vs Rosetta）

这是本项目最容易低估工作量的地方。一次请求要经过 4 个方向的转换：

| # | 方向 | 由谁做 | 现状 |
|---|---|---|---|
| 1 | 下游协议 JSON → 统一模型（含 SSE 输入解析） | **网关** | Rosetta 无 |
| 2 | 统一模型 → 下游协议 JSON（含 SSE 编码） | **网关** | Rosetta 无 |
| 3 | 统一模型 → 上游协议 JSON | Rosetta | 有（`buildPayload`），未导出但网关不需要 |
| 4 | 上游响应 → 统一模型 | Rosetta | 有（`Client.Chat` / `ChatStream` 返回值） |

**下游侧要写 3 套 × 3 件 = 9 件**（每套协议各一份请求解码、响应编码、SSE 编码）。上游侧 2 件 Rosetta 全包。

**已核实的关键事实**：

- 网关**不需要** Rosetta 导出任何内部函数。第 3、4 方向通过 `Client.Chat` / `ChatStream` 的公开 API 完全覆盖。
- 因此 **P0 阶段零阻塞，不必修改 Rosetta**。
- `Client.ChatStream` 返回 `nil` error 意味着上游已返回 HTTP 200 且响应头已读（`provider_openai_chat.go:387` 仅在 `StatusCode == 200` 时退出重试循环）。**结论：网关可以「失败返回 JSON 错误、成功再写 SSE 头」，无需先写 200 再报错。**
- `WithTimeout` 对**流式无效**（`client.go:147` 注释：仅约束 unary 调用），流只受传入 `ctx` 约束。
- **Rosetta 没有流空闲超时**（全仓检索 `idle` 仅命中一句注释），看门狗是网关职责。

**P0 只需实现 OpenAI Chat 那一套**，是 9 件里的 3 件。其余 6 件留到 P3。

---

## 8. 流式转发

```
下游 SSE  ←  outwire 编码  ←  rosetta.Event  ←  Stream.Next()
```

### 8.1 要点

| 项 | 做法 |
|---|---|
| 取消传播 | 上游 ctx 直接取 `r.Context()`。下游断开 → ctx 取消 → Rosetta 中止流 → 上游连接关闭。**这是最直接的止损点，必须做对** |
| 空闲看门狗 | 独立 `time.AfterFunc`，默认 60s 无事件则 `stream.Close()`。每收到一个事件重置定时器。超时视为 `status=truncated`。**注意 `Stream.Close()` 不写 `stream.Err()`**（Rosetta 只在真的读失败时才置 err），所以看门狗必须自己用 `atomic.Bool` 留痕；否则 `Err()==nil` → 状态保持 `ok` → 下游收到 `finish_reason:"stop"` + `[DONE]`，卡死的上游被伪装成正常收尾（详见 §8.2） |
| 心跳 | 空闲超过 `idle/2` 时下发 `: keepalive\n\n`，防中间代理超时断连 |
| Flush | 用 `http.NewResponseController(w).Flush()`（Go 1.20+），不用 `http.Flusher` 类型断言 |
| 首字节时机 | `ChatStream` 成功后才写 `200 + Content-Type: text/event-stream` + `Cache-Control: no-cache` + `X-Accel-Buffering: no` |
| 结束事件 | OpenAI 系发 `data: [DONE]`；Anthropic 发 `event: message_stop`。**只有 `status=="ok"` 才发** |
| 思考增量 | `EventThinkingDelta` 编码为 `delta.reasoning_content`（DeepSeek / Qwen / vLLM 的既成约定，Rosetta 的 openai-chat 适配器也按这个键回读）。**不能丢**：只吐思考的流丢了它就是个零内容的流 |
| 空文本事件 | `EventThinkingDelta` 的 `Text==""` 是 Anthropic thinking signature 的载体，OpenAI 下游无对应字段，跳过即可 |
| usage 合成 | OpenAI 下游要 usage 需客户端传 `stream_options.include_usage`；网关在 `EventMessageEnd` 处合成仅含 usage 的 chunk，且仅当客户端要求时下发 |
| 断流处理 | 见 §8.2 |
| 缓冲 | 逐事件 Flush，不做批量聚合（延迟优先） |

### 8.2 断流语义

Rosetta 用 `ErrStreamTruncated` 区分「干净结束」与「连接被掐断」。下游已经收到了部分内容，**无法回滚**。约定：

- **OpenAI / Responses 下游**：发完已收到的增量与结束事件，但**不下发 `[DONE]` / `response.completed`**，直接关闭连接。这是 OpenAI 自身中断时的表现，客户端会据此判定异常。
- **Anthropic 下游**：发 `event: error`，载荷 `{"type":"error","error":{"type":"api_error","message":"upstream stream truncated"}}`，然后关闭。
- 无论哪种，`usage_records.status` 记 `truncated`，已知的 usage 照常入账（Rosetta 在截断时会交付带 usage 的结束事件）。

**`status=truncated` 有两个来源，都要覆盖**：

1. `stream.Err()` 命中 `rosetta.ErrStreamTruncated`（上游断连、缺 `[DONE]`）。
2. **空闲看门狗开火**。这类超时在 Rosetta 的流上不留痕迹（`Close()` 不置 `err`），
   必须由网关自己判定；否则就是第 1 类漏网、且是以「正常收尾」的形态漏网。
   终态判定：`idleTimedOut && !sawTerminal` → `truncated`。
   其中 `sawTerminal`（收到过 `EventMessageEnd`）是防御性条件 —— 实测 Rosetta v0.5.1
   在 `[DONE]`/EOF 之后会短路 `next()`，适配器不会在吐出 `message_end` 后继续阻塞，
   所以当前打不到；留着守「适配器将来在终止事件之后仍等待更多数据」的情形。

两者都无法回滚已下发的内容，下游看到的都是「有增量、无收尾」。
另外补一条留痕规则：上游给了终止事件却一个内容增量都没写（`wroteContent==false`）时，
协议行为保持不变（上游可能因内容过滤合法地返回空回复，网关不该替它改语义），
但记一条 `WARN stream finished with no content` —— 日志是唯一能区分
「上游确实回了空」与「网关把内容吃掉了」的地方。


### 8.3 非流式

直接 `client.Chat()`。上游失败时把 `*rosetta.APIError` 映射为下游状态码（§9），无部分结果问题。

---

## 9. 错误映射

统一入口：所有上游错误都是 `*rosetta.APIError`（或 `TransportError`）。

| 上游情况 | 网关对下游 | error code | 说明 |
|---|---|---|---|
| 上游 401 / 403 | **502** | `upstream_auth_error` | **绝不透传 401。** 上游凭证问题是网关的锅，透传会让调用方以为自己 key 错了 |
| 上游 402（余额不足） | 502 | `upstream_quota_exhausted` | 同上，且触发凭证冷却 |
| 上游 429 | 429 | `rate_limit_exceeded` | 先尝试换凭证 / 换 provider，全部失败才返回 |
| 上游 5xx | 502 | `upstream_error` | |
| 上游超时 / 连接失败 | 504 / 502 | `upstream_timeout` | 出站侧 |
| 上游 400（请求非法） | 400 | 透传上游 code | 这类是调用方的问题，原样透传并保留 message |
| 上游 404（模型不存在） | 502 | `upstream_model_not_found` | 是网关路由配置错了，不是调用方写错模型名 |
| 未知模型名 | 404 | `model_not_found` | 网关自己产生 |
| 下游 Key 无效 | 401 | `invalid_api_key` | |
| 下游 Key 停用 / 过期 | 403 / 401 | | |
| 配额耗尽 / 限速 | 429 | `rate_limit_exceeded` | |
| 上下文超长（`ErrContextTooLong`） | 400 | `context_length_exceeded` | Rosetta strict 模式产生 |
| 请求体非法（`ErrInvalidRequest`） | 400 | `invalid_request_error` | 含 Rosetta 的结构校验失败 |

错误响应形状按入口协议输出：

```json
// OpenAI
{ "error": { "message": "...", "type": "invalid_request_error", "code": "model_not_found", "param": "model" } }

// Anthropic
{ "type": "error", "error": { "type": "invalid_request_error", "message": "..." } }
```

**出站错误体也要脱敏**：上游错误里的疑似密钥材料在写日志与返回前掩码（Rosetta 已对 `APIError.Raw` 做了脱敏，网关在拼装下游错误时不要重新引入 `Raw` 原文）。

---

## 10. 上游凭证池与故障转移

**约束**：Rosetta 的 endpoint / api_key 是**构建期**配置（`WithEndpoint` / `WithAPIKey`），没有 per-request 覆盖。因此凭证池的实现方式是：

```
每个 (provider, credential) 对应一个 rosetta.Client 实例，由网关持有并复用。
Client 是 goroutine 安全的，进程内单例复用（内含连接池）。
```

一个 provider 有 N 把 key 就有 N 个 Client。这带来两点代价，需接受：

1. N 个 `http.Client` ⇒ N 个连接池。key 数量在几十把以内可忽略；上百把需要合并 `WithHTTPClient` 共享传输层。
2. 改 key 后要重建对应 Client（管理接口里封装，热更新时替换快照中的指针）。

**选择策略**（P2）：

- `weight` 加权的随机选择（默认）、或轮询
- 跳过 `status != healthy` 或 `cooldown_until > now` 的凭证
- 冷却规则：429 按 `Retry-After`（上限 60s，Rosetta 已有硬上限）；401/403 冷却 30 分钟；402 冷却 1 小时；5xx 冷却 60s
- 单请求重试：用另一把凭证重发，最多 2 次（**仅在未向下游写出任何字节时可以重试**，即非流式请求、或流式的 `ChatStream` 建立阶段失败）

**故障转移**：`routes.fallback_route_id` 指向备用 route。仅当当前 provider 的所有凭证都不可用时触发。P2 实现。

**P0 阶段**：单凭证、无池、无冷却、无故障转移。够跑通链路。

---

## 11. 配额与限速

### 11.1 计量口径

- 单位：**token**。`total = input + output`（reasoning token 若上游给出，计入 output 不重复累加，单独记录）
- 来源：`ChatResponse.Usage` / `EventMessageEnd.Usage`
- **usage 缺失时**：Rosetta 会标记 `IsZero()` 并计入 `usage_missing`。网关的处置是**按估算值扣减**（`rosetta.EstimateTokens` 输入 + 输出按字符数估），并记 `usage_state='estimated'`。理由：记 0 等于放行白嫖。界面里把 `estimated` 单独统计，便于发现是哪个上游不吐 usage。

### 11.2 为什么不可能精确

流式请求的 output token 只有流结束才知道。所以**不存在请求前精确拒绝**。设计成：

```
请求前：读 used_tokens（快照缓存），若 >= quota_tokens → 429
请求中：放行
请求后：used_tokens += 真实用量（原子 UPDATE）
```

并发下必然短暂超发（N 个并发请求可同时通过检查）。**这是设计上接受的**，界面与文档都要明说，不要把它当 bug。

### 11.3 并发安全

```sql
UPDATE access_keys SET used_tokens = used_tokens + ?, last_used_at = ? WHERE id = ?;
```

单条 UPDATE 天然原子，无需显式事务。写放大：每条请求一次 UPDATE + 一条 INSERT，SQLite WAL 下局域网量级无压力。`usage_records` 可以异步批量写（缓冲 ≤1s 或 ≤64 条 flush），但 `used_tokens` 扣减**同步写**，否则崩溃会丢额度。

### 11.4 限速（P2）

| 维度 | 实现 |
|---|---|
| RPM | 内存固定窗口计数器（每分钟一个桶），Key 维度 |
| TPM | 同上，按请求前估算 + 请求后校正 |
| 实现 | 标准库 `sync.Map` + 每秒清扫；不用 `golang.org/x/time/rate`（避免依赖） |
| 重启 | 计数归零，接受 |

### 11.5 配额维度

只做 **Key 总量**。per-model 配额、per-provider 配额记入后续路线，不进 P2。

---

## 12. 配置

### 12.1 唯一事实来源（D4）

| 配置类别 | 存放位置 | 可否运行时改 |
|---|---|---|
| Provider / 模型 / Route / Key | **数据库** | 是（管理 API / Web 界面） |
| **管理后台密码** | **`<exeDir>/admin_auth.json`** | **是（「设置」页，改完立即生效）** |
| 监听地址、DB 路径、日志级别、加密主密钥、全局默认超时与重试、body 大小上限 | **配置文件** | 否（改后重启） |
| 首次 bootstrap 的 provider/route | 配置文件（仅当 DB 为空时生效） | 否 |

**关键纪律**：DB 非空时，配置文件里的 `providers` / `routes` 段落被**忽略并打印 warning**。这防止出现「界面改完、重启被配置文件覆盖」这类经典事故。

**为什么管理密码不进数据库**：`gateway.db` 在本项目里是「可丢弃的运行时数据」—— 加密主密钥丢失、库损坏、想重来一遍时，标准动作就是删库重建。管理员密码是**身份凭据**，放进一个会被随手删掉的文件里，等于「删库 = 把自己锁在门外」。这与 `master.key` 独立于库的理由完全一致：**身份状态必须独立于业务数据**。

**为什么也不塞进 `config.json`**：`config.json` 是运维手写的引导配置，程序回写它会丢掉注释与字段顺序；而且 `admin_token` 的语义是「运维引导用的静态令牌」，与「用户在界面上设置的管理密码」是两回事。混用会让判定逻辑互相污染 —— 早期实现因此被迫用 `len(token) == 64` 去猜「这串到底是明文还是哈希」，一个恰好 64 字符的明文令牌就会被误判成哈希而**永久锁死**。

两者关系：用户设置的密码**优先**；未设置时回退到 `config.json` 的 `admin_token`（或 `ADMIN_TOKEN` 环境变量）。后者是兜底与应急恢复通道 —— 忘了密码时删掉 `admin_auth.json` 重启，就退回用 `admin_token` 登录。

### 12.1.1 数据目录布局

所有相对路径都按**可执行文件所在目录**解析（与进程 CWD 无关），因此部署形态是「一个目录装下全部状态」：

```
<部署目录>/
├── gateway.exe          # 单个二进制（前端已 embed）
├── config.json          # 手写引导配置，程序不回写
├── master.key           # 凭据加密主密钥（缺失时程序自动生成，见下）
├── admin_auth.json      # 管理后台密码（PBKDF2-SHA256，加盐，无明文）
└── data/
    └── gateway.db       # SQLite：provider/模型/路由/key/用量记录
```

删掉 `data/` 等于重置全部业务数据；删掉 `admin_auth.json` 等于重置管理密码。两者互不影响。

**主密钥的解析顺序**（`crypto.LoadMasterKey`）：

1. 环境变量 `master_key_env`（缺省 `ROSETTA_GW_MASTER_KEY`）
2. `<exeDir>/master.key` 文件
3. 都没有 → **生成一个写入 `<exeDir>/master.key`** 并复用（启动日志给出路径）

之所以必须有第 3 条：早期实现只认环境变量，而 `bin/master.key` 那套自动生成活在
`gateway.ps1` 里 —— 结果**用脚本启动有密钥、双击 exe 启动没有**，后者会把上游 API Key
**明文**写进 `data/gateway.db`。同一份库在两种启动方式下还会互相解不开。
密钥落盘后两条路共用一把。

⚠️ **选定启动方式后不要来回换。** 环境变量优先级高于文件：先双击（密钥落在 `master.key`）、
后来改成设环境变量启动，两把密钥不同 → 先前加密的凭据解不开。
`gateway.ps1` 写的正是 `bin/master.key`，与 Go 侧路径一致，所以脚本与双击天然对齐。

**历史明文数据的兼容**：解密走 `crypto.DecryptWithFallback` —— 主密钥存在但解不开时，
只有数据看起来是可打印文本才按明文返回，密文形态仍报错。
这样老库（明文）升级后立刻可用，不会被新密钥打死；但明文依旧躺在库里，
要彻底消除得把每条凭据**重新保存一次**。

### 12.2 配置文件示例

```json
{
  "listen": "127.0.0.1:8080",
  "db_path": "./data/gateway.db",
  "log_level": "info",
  "admin_token": "",
  "master_key_env": "ROSETTA_GW_MASTER_KEY",
  "defaults": {
    "upstream_timeout_ms": 120000,
    "stream_idle_timeout_ms": 60000,
    "max_retries": 2,
    "max_request_body_bytes": 33554432
  },
  "bootstrap": {
    "providers": [
      {
        "slug": "deepseek",
        "name": "DeepSeek 官方",
        "protocol": "auto",
        "endpoint": "https://api.deepseek.com/v1",
        "credentials": [{ "label": "主号", "api_key_env": "DEEPSEEK_API_KEY" }],
        "models": ["deepseek-chat", "deepseek-reasoner"]
      }
    ],
    "routes": [
      { "public_name": "gpt-4o", "provider": "openai", "model": "gpt-4o" }
    ]
  }
}
```

`api_key_env` 支持从环境变量读，避免密钥落盘到配置文件。

**关于 `listen`**：默认（含程序自动生成的配置）是 `127.0.0.1:8080`。
网关对外提供 `/v1` 是常态，但**首次启动时后台还没有任何凭据**，
此时绑 `0.0.0.0` 等于把「抢先设置管理员密码」的权利交给局域网里第一个访问 `/admin/` 的人。
要对外服务就显式改成 `0.0.0.0:<port>` —— 启动日志会打印实际监听地址，改完记得回头核对。

**关于 `admin_token`**：留空**不等于**免鉴权。真实凭据在 `<exeDir>/admin_auth.json`。
留空且该文件不存在时，后台处于「等待首次设置密码」状态，
此时除 `password/check` 与首次 `password/set` 外的接口一律 401。

---

## 13. Web 管理界面（D10）

### 13.1 形态

`go:embed` 打包静态资源（`internal/webui/dist`），`web/` 下是 **Vue 3 + Vite + TS** 工程，`fetch` 调 `/admin/api/*`。

> 历史沿革：早期是单个 `index.html` 内联全部逻辑，理由写的是「内网管理页不超过 8 个，引入框架收益不成比例」，并预设了升级边界「一旦出现多页 + 复杂表单联动 + 图表就换框架」。边界随后真的被触发了（六页 + 表单弹窗 + 图表），于是按当初的约定迁到 Vue 3。构建链：`cd web && npm run build && npm run sync`（`sync` 把产物同步进 `internal/webui/dist` 供 embed），`gateway.ps1` 已编排。

**鉴权**：管理 API 由 `server.AdminAuth` 中间件保护，凭据逻辑在 `internal/adminauth`。三个端点例外/半例外：
- `GET /admin/api/password/check` —— 恒免鉴权，前端靠它决定弹「设置密码」还是「输入密码」；
- `POST /admin/api/password/set` —— **仅在系统尚无任何凭据时免鉴权**（一次性引导窗口），已有凭据后必须带上正确的旧凭据；
- `GET /admin/api/auth/verify` —— 需鉴权，专门给前端做「先验证再保存」。

前端有一条硬规则：**绝不「把输入存进 localStorage 就刷新」**。必须先用 `/admin/api/auth/verify` 验证通过再落盘，否则密码一错就会被 401 弹回同一个对话框，而该对话框是 `dismissable=false` 的 —— 用户会被永久困在「输入密码 → 又要求输入」的循环里。


### 13.2 页面清单

| 页面 | 期 | 内容 |
|---|---|---|
| Providers | P1 | 列表（协议/端点/凭证健康）、新建/编辑、连通性测试 |
| 模型 | P1 | 某 provider 下的模型列表、手动添加、从上游 `/models` 批量导入、能力覆盖 |
| Routes | P1 | 虚拟名 ↔ (provider, model) 映射表、启停 |
| Keys | P1 | 列表、新建（明文只显示一次）、启停、配额编辑 |
| 用量总览 | P1 | 今日请求数 / token / 错误率 / 各 provider 健康灯 |
| 用量看板 | P2 | 按天、按 Key、按模型、按 provider 的 token 趋势 |
| 系统 | P1 | 版本、DB 大小、重建快照、日志级别 |

### 13.3 安全

- 管理界面仅监听内网，但**默认要求 `ADMIN_TOKEN`**，不做「内网免鉴权」的假设
- 上游 key 在界面只显示掩码（`sk-...abcd`），明文不可回读
- 所有写操作记审计日志（谁、什么时候、改了什么）

---

## 14. 技术选型

| 组件 | 选择 | 理由 |
|---|---|---|
| HTTP 路由 | 标准库 `net/http`（Go 1.22+ 方法路由 + 通配 `{id}`） | 零依赖，本项目路由形态简单，`ServeMux` 够用 |
| SQLite | `modernc.org/sqlite`（纯 Go） | 无 cgo，`GOOS=linux/darwin/windows` 交叉编译无痛，保持单二进制 |
| 日志 | `log/slog`（标准库） | JSON handler，带请求 ID |
| 配置解析 | `encoding/json`（标准库） | 与 Rosetta 一致，零依赖 |
| 密钥加密 | `crypto/aes` + GCM（标准库） | 主密钥来自环境变量或 0600 权限文件 |
| Key 哈希 | `crypto/sha256`（标准库） | 高熵随机串，不需要慢哈希 |
| UUID | `crypto/rand` 自造 16 字节 hex | 避免引入依赖 |
| 前端 | 原生 HTML + JS + `go:embed` | 见 §13.1 |
| 上游调用 | `github.com/cn-maul/rosetta` | 本项目存在的理由 |
| 部署 | 单个二进制 + 一个 SQLite 文件 + 一个配置文件 | `scp` 过去就能跑 |

---

## 15. 目录结构

```
rosetta-gateway/
├── go.mod
├── DESIGN.md                       本文件
├── config.example.json
├── cmd/
│   └── gateway/
│       └── main.go                 装配与启动
├── internal/
│   ├── config/                     启动配置加载与校验
│   ├── store/                      SQLite 连接、迁移、各表 DAO
│   ├── snapshot/                   运行时快照：路由索引、Provider 池、Key 索引
│   ├── routing/                    模型名解析（§5）
│   ├── upstream/                   凭证池、健康与冷却、Client 生命周期、故障转移
│   ├── inwire/                     下游 → 统一模型
│   │   ├── openai_chat.go
│   │   ├── openai_responses.go     P3
│   │   └── anthropic.go            P3
│   ├── outwire/                    统一模型 → 下游（响应 + SSE）
│   │   ├── openai_chat.go
│   │   ├── openai_responses.go     P3
│   │   ├── anthropic.go            P3
│   │   └── errors.go               错误形状映射（§9）
│   ├── auth/                       下游 Key 校验
│   ├── quota/                      配额检查、扣减、限速
│   ├── admin/                      管理 API handlers
│   ├── webui/                      embed 静态资源
│   ├── crypto/                     上游 key 加解密
│   └── server/                     HTTP 装配、中间件、请求 ID、访问日志
└── examples/
    └── curl.md                     各客户端的接入手册
```

---

## 16. 里程碑

### P0 — 打通链路

**做**：`cmd/gateway` 骨架；启动配置（JSON）；Provider 抽象与 `rosetta.Client` 构建；路由解析（§5 轨道一 + 二）；OpenAI Chat 入口的解码/编码/SSE；错误映射（§9）；看门狗、心跳、取消传播；静态 bootstrap 配置（暂不建 DB）。

**不做**：数据库、管理 API、Web 界面、配额、限速、凭证池、故障转移、Anthropic/Responses 入口。

**验收**：
1. Cherry Studio 把 base_url 指向 `http://<局域网IP>:8080/v1`，流式输出正常、非流式正常
2. `model` 同时支持虚拟名与 `slug/model` 两种写法
3. 下游 Ctrl+C 中断后，**网关日志显示上游请求已取消，上游连接确实断开**（用一个记录 ctx 的假上游验证）
4. 上游返回 401 时，下游收到的是 502 `upstream_auth_error`，不是 401
5. 上游卡住不吐字节，60s 后看门狗关闭流，下游收到断流

### P1 — 管起来

**做**：SQLite + 迁移；Provider / 模型 / Route / Key 的 CRUD 管理 API；管理鉴权；内存快照热更新；Web 界面（Providers、模型、Routes、Keys、用量总览、系统页）；连通性测试。

**验收**：
1. 浏览器里加一个 provider、加模型、建 route、发一把 key，**全程不改配置文件、不重启**
2. 改完立刻生效（新建的 route 立即能被下游调用）
3. DB 非空时，配置文件里的 bootstrap 段落被忽略并打 warning
4. 上游 key 在界面不可回读明文

### P2 — 管住量

**做**：`usage_records` 落库；配额检查与同步扣减；RPM/TPM 限速；用量看板；凭证池（多 key、加权、冷却）；`fallback_route_id` 故障转移。

**验收**：
1. 给 Key 设 10k token 配额，跑超后返回 429
2. 同一 provider 配 2 把 key，其中一把返回 401 后自动切另一把，且该 key 进入冷却
3. 并发 50 个请求，`SUM(usage_records.total_tokens)` 与 `used_tokens` 一致，不漏记
4. 主 route 的 provider 全部凭证失效时，自动走 fallback route

### P3 — 补协议

**做**：Anthropic Messages 入口；Responses 入口；`/v1/models` 形状分流与别名路径；Anthropic thinking signature 的跨协议处理（§17 R4）。

**验收**：
1. Claude Code 把 `ANTHROPIC_BASE_URL` 指向网关，能正常多轮对话含工具调用
2. 三个入口对同一虚拟模型名都能工作，且互相之间的会话切换不报错
3. `/anthropic/v1/models` 返回 Anthropic 形状，`/openai/v1/models` 返回 OpenAI 形状

---

## 17. 待验证假设与风险

| # | 假设 / 风险 | 影响 | 处置 |
|---|---|---|---|
| R1 | Rosetta 不导出协议映射层，下游侧 9 件转换全靠网关自己写 | 工作量集中在 P0 与 P3 | 已确认 P0 只需 3 件；P3 再评估是否向 Rosetta 提导出需求 |
| R2 | 一维模型名承载二维命名空间 | 可能歧义 | 解析优先级封死（§5），管理界面做撞名提示 |
| R3 | `/v1/models` 在两种协议下路径相同、形状不同 | 客户端拿错格式 | 按认证头分流 + 显式别名路径（§6.1） |
| R4 | **Anthropic thinking block 带 `signature`，跨协议转换会失效** | 下游 Anthropic + 上游非 Anthropic 时，多轮回传 thinking 会 400 | 该组合下默认剥掉历史 thinking 块（可配开关）。P3 处理 |
| R5 | 流式断流无法回滚 | 下游可能收到半截回答 | 约定：不发终止事件，直接断连（§8.2） |
| R6 | 流式配额必然可能超发 | 需接受 | 设计明示，界面明示（§11.2） |
| R7 | 多模态 base64 让请求体很大 | 内存与 body 限制 | `max_request_body_bytes` 默认 32 MiB；注意 Rosetta chat 的 1 MiB 限制是**响应**侧，不冲突 |
| R8 | 每凭证一个 `rosetta.Client` ⇒ 连接池随 key 数增长 | 上百把 key 时资源偏高 | 共享 `WithHTTPClient` 的 Transport，或后续向 Rosetta 提 per-request 覆盖 |
| R9 | 上游 usage 缺失时按估算扣减 | 配额不完全准确 | 记 `usage_state='estimated'`，界面单列统计 |
| R10 | 同类成熟产品（one-api / new-api / LiteLLM / Portkey / Higress）功能面重合 | 自研投入产出比 | 自研的唯一正当理由是「Rosetta 作内核」与「深度定制」。已确认走自研 |

---

## 附录 A：Rosetta 能力矩阵（网关视角）

| 网关需求 | Rosetta | 说明 |
|---|---|---|
| 三协议上游调用 | ✅ | `Client.Chat` / `ChatStream` |
| Embed / Rerank 上游 | ✅ | `Client.Embed` / `Rerank` |
| 统一 usage | ✅ | `Usage` + `UsageTracker` 接口（网关实现它落库） |
| 上游重试 / 退避 | ✅ | `WithMaxRetries`，含 `Retry-After`（60s 硬上限） |
| 上游凭证 | ✅ | 每 Client 一套，网关按 provider × credential 建实例 |
| 流式拉取 | ✅ | `Stream.Next()`，逐事件转下游 SSE 很顺 |
| 流截断识别 | ✅ | `ErrStreamTruncated` |
| 脏数据容错 | ✅ | `internal/jsonx` 宽松解码 |
| 兼容服务降级 | ✅ | quirks + sticky probe |
| 模型元数据 | ✅ | `ModelInfo` / 手动配置注入（可覆盖 context window、输出上限、thinking 支持） |
| 上下文预估 | ✅ | `EstimateTokens`（网关用于 usage 缺失兜底） |
| 流空闲超时 | ❌ | 网关实现看门狗 |
| 下游协议解码 | ❌ | 网关实现 |
| 下游协议编码 | ❌ | 网关实现 |
| 凭证池 / 冷却 / 故障转移 | ❌ | 网关实现 |
| per-request endpoint/key 覆盖 | ❌ | 网关用多 Client 实例绕过 |

## 附录 B：需要 Rosetta 后续补的能力（不在本期）

**收集，但不现在提。** 等网关写完再定导出形状，避免过早把协议映射细节冻结成公开契约。

1. **导出统一模型 ⇄ 各协议 wire 的编解码**（含流式事件编码）。这是最大的重复劳动——网关要重写 9 件。合理形状可能是 `rosetta.EncodeOpenAIChat(req) ([]byte, error)` 与 `rosetta.DecodeOpenAIChat(body) (*ChatRequest, error)` 这类对称函数。
2. **流空闲超时选项** `WithStreamIdleTimeout(d)`。目前每个调用方都要自己写看门狗。
3. **per-request endpoint / key 覆盖**，让网关不必为每把凭证维护一个 Client（连接池问题）。
4. **`WithTimeout` 对流式生效**——或明确文档化为「仅 unary」，并给出推荐的看门狗实现范式。
