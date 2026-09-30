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
  slug          TEXT NOT NULL UNIQUE,      -- 前缀语法用，限定 [a-z0-9]{2,32}（不允许连字符）
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
  UNIQUE (provider_id, model_id)
  -- 曾有一个 default_extra_json 列，但没有任何地方拿它构造请求，2026-09-24 摘除。
  -- 要接线得先定清语义（链上各目标协议可能不同、路由级与模型级谁覆盖谁），
  -- 理由与 routes.priority 同类：留着 = schema 在说谎。见 store.dropDeadColumns 注释。
);

-- 对外虚拟模型名（D2 的轨道一）
-- 一条 route 背后是一条「有序上游链」（见 route_targets）。
-- provider_id / upstream_model_id 是链首（position 0）的向后兼容列：
-- 老代码、config bootstrap、以及「尚未建目标行」的场景仍以它们为准。
CREATE TABLE routes (
  id                TEXT PRIMARY KEY,
  public_name       TEXT NOT NULL UNIQUE,   -- 对外 model 名
  provider_id       TEXT NOT NULL REFERENCES providers(id) ON DELETE RESTRICT,        -- 链首（兼容列）
  upstream_model_id TEXT NOT NULL REFERENCES upstream_models(id) ON DELETE RESTRICT,  -- 链首（兼容列）
  enabled           INTEGER NOT NULL DEFAULT 1,
  -- 没有 priority：曾规划为「同名多路由择优」，但 public_name 是 UNIQUE，
  -- 该语义在数据层不可能成立，界面却写着「按 priority 升序择优」→ 2026-09-24 摘除。
  -- 「一个公开名挂多个上游」由下面的 route_targets 链承担（有序、可故障转移）。
  -- 没有 fallback_route_id：旧单跳兜底已被 route_targets 链取代（DESIGN §10），
  -- 留着会与链形成两套并行机制 → 2026-09-29 摘除。
  -- 没有 max_targets / failure_threshold / stream_first_token_timeout_ms /
  -- nonstream_timeout_ms：故障转移与超时策略已统一收进「设置」页
  -- （app_settings 的 runtime_defaults），不再按路由存 → 2026-09-29 摘除。
  failover_enabled  INTEGER NOT NULL DEFAULT 0,  -- 1 = 这条路由启用自动故障转移
  -- 曾有一个 extra_json 列（自 2026-09-21 起），但没有任何地方拿它构造请求，2026-09-24 摘除。
  created_at        INTEGER NOT NULL
);

-- route 的有序上游链：对外同一个 public_name 挂多个 (provider, model)，按 position 升序尝试。
-- 迁移期幂等回填：为「尚无任何目标行」的老 route 补一条 position 0（取 route 的兼容列）。
CREATE TABLE route_targets (
  id                TEXT PRIMARY KEY,
  route_id          TEXT NOT NULL REFERENCES routes(id) ON DELETE CASCADE,
  provider_id       TEXT NOT NULL REFERENCES providers(id) ON DELETE RESTRICT,
  upstream_model_id TEXT NOT NULL REFERENCES upstream_models(id) ON DELETE RESTRICT,
  position          INTEGER NOT NULL DEFAULT 0,   -- 链顺序，越小越先尝试
  enabled           INTEGER NOT NULL DEFAULT 1,
  created_at        INTEGER NOT NULL
);
CREATE INDEX idx_route_targets_route ON route_targets(route_id, position);

-- 下游访问凭证（D7 的配额载体）
-- 只做「总量配额」这一维。expires_at / rpm_limit / tpm_limit / last_used_at 曾在表里
-- 但没有任何读写路径（能读、无写、无人用），2026-09-24 随 dropDeadColumns() 一并删除——
-- 留着它们只会让「支持过期/RPM/TPM」看起来像是已实现的功能。
CREATE TABLE access_keys (
  id            TEXT PRIMARY KEY,
  key_hash      TEXT NOT NULL UNIQUE,      -- SHA-256(明文)，不存明文
  key_prefix    TEXT NOT NULL,             -- 形如 "sk-gw-a1b2"，用于界面展示
  name          TEXT NOT NULL,
  enabled       INTEGER NOT NULL DEFAULT 1,
  quota_tokens  INTEGER NOT NULL DEFAULT 0,     -- 0 = 不限；累计 input+output token 上限
  used_tokens   INTEGER NOT NULL DEFAULT 0,     -- 累计，只增（由 usage_records 触发器维护）
  created_at    INTEGER NOT NULL
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
  status            TEXT NOT NULL,         -- ok|truncated|overflow|canceled|error（见 §8.2）
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

**命中之后：怎么选上游。** `Resolve` 返回的是 `(Route, []Candidate)`，候选链来自 `route_targets`：

- 按 `position` 升序逐个尝试，过滤掉 provider/model 缺失或禁用的成员。
- **只要这条 route 有目标行，就以链为准**，绝不回落到 `routes` 的主目标列 —— 否则等于
  无视运维对链的显式禁用、把死目标复活。仅当一条目标行都没有（从未配置过链的老 route）
  才用主目标列合成单元素链兜底（零回归）。
- 尝试预算：关掉 `failover_enabled` 时**只打链首**（等价于改造前的单目标行为）；
  开启时按 `failover_max_targets`（**全局设置**，见 §10「参数落点」）裁剪。
- 开启故障转移时跳过正被熔断的目标（`Pool.TargetAvailable`）；**若全被熔断则退回整条链** ——
  宁可打一个刚失败的目标，也不要因为链整体静默就给调用方一个「模型不存在」（模型名明明在
  网关自己的 `/v1/models` 里）。
- 链首与 `routes.provider_id`/`upstream_model_id` 的一致性由写入路径维护：
  `POST /routes` 建 route 时同事务种一条 position 0；`PATCH /routes/{id}` 在**所有字段赋值之后**
  做配对校验并同事务把链首对齐到新的主目标。少了这一步就会留下
  「响应回显新值、界面显示成功、实际流量还打旧目标」的永久分叉。

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

**凭据优先级（唯一事实来源）** —— 两者是**覆盖**关系，不是并存：

| 状态 | 实际生效的凭据 | 判定依据 |
|---|---|---|
| ① `admin_auth.json` 存在且可解析 | **只认其中的用户密码**；`admin_token` **完全失效** | `Store.cred != nil` → 走 `cred.matches()` |
| ② `admin_auth.json` 不存在 | `config.json` 的 `admin_token`；若它为空则用 `ADMIN_TOKEN` 环境变量 | `Store.cred == nil` → 走 `verifyFallback()` |
| ③ 两者都没有 | 无凭据：除 `password/check` 与首次 `password/set` 外一律 401 | `HasCredential() == false` |
| ④ `admin_auth.json` 存在但损坏 | 一律拒绝（锁定态）；`HasCredential()` 仍为真 | `Store.locked != nil` |

要点：
- **一旦在「设置」页设过密码，`admin_token` 就再也不认了**（不是「都能用」）。这是最常见的困惑来源。
- 忘了密码的恢复通道：删掉 `admin_auth.json` 重启 → 回到状态 ②/③（用 `admin_token` 登录，或重新设置密码）。
- 状态 ④ 的 `HasCredential()` 必须为真，否则一个损坏的文件就等于把 `password/set` 引导窗口向所有人敞开。
- 上述四条由 `internal/adminauth/store_test.go` 钉住（`TestVerify_PasswordBeatsConfigToken` 等）。

前端 `GET /admin/api/password/check` 返回 `source` 字段（`none` / `config_token` / `password_file` / `locked`）
与 `first_setup`，登录弹窗据此渲染成三种**明显不同**的形态（首次初始化需二次确认 / 令牌登录 / 密码登录），
避免「创建凭据」与「使用凭据」长得一样。

| 端点 | 放行规则 |
|---|---|
| `GET /admin/api/password/check` | 恒放行（只返回布尔值与来源标签，不含任何可用于登录的信息） |
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
GET    /admin/api/routes/{id}/targets             读有序上游链（含 provider/model 展示名）
PUT    /admin/api/routes/{id}/targets             原子整体替换链；链首回写 routes 主目标列

GET    /admin/api/keys
POST   /admin/api/keys                          返回明文一次
PATCH  /admin/api/keys/{id}                      可改 name / enabled / quota_tokens
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

- **编辑时只发你要改的字段即可**，不必回传完整对象。前端里唯一的正确写法是
  「只发要改的字段」（如启停开关只发 `{ enabled: ... }`）；回传整个对象等于做读-改-写，
  会把列表快照里的陈旧值一起写回去，静默回滚别的标签页刚做的改动。
- `timeout_ms: 0`（=用全局默认）、`quirks_json: ""`（=清空）、
  `context_window: 0`（=未设置）都是可表达的意图，不再是「空值即忽略」。
  （注意：故障转移与超时策略**不是**路由的 PATCH 字段，它们在「设置」页，见 §10。）

管理端登录限速（§11.4）与配额预检（§11.2）都不走 PATCH 语义，别混。

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
| 取消传播 | 上游 ctx 直接取 `r.Context()`。下游断开 → ctx 取消 → Rosetta 中止流 → 上游连接关闭。**这是最直接的止损点，必须做对**。下游断开**不算错误**：`usage_records.status` 记 `canceled`（见 §8.2），不计入错误率 |
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

#### status 取值（五值）

| status | 含义 | 算不算错误 |
|---|---|---|
| `ok` | 拿到终止事件的干净结束 | 否 |
| `truncated` | 内容已下发，但缺收尾事件（上游断连，或我们的看门狗开火） | 否（内容确实上线了） |
| `overflow` | Rosetta 侧内容超限（64 MiB）而截断 | 否（同上） |
| `canceled` | **客户端主动断开**，网关随之取消上游 | **否** |
| `error` | 上游报错、鉴权失败、路由不存在等 | 是 |

`canceled` 单列的理由：客户端断开既不是网关的错，也不是上游的错。记成 `error` 会让
「用户关掉了一个页面」在错误率里和「上游 500」等价 —— 看板上会出现无法解释的错误尖峰，
而真正需要关注的故障被淹没在里面。判定用 `context.Canceled` / `context.DeadlineExceeded`
（`errors.Is`），并在写响应前先检查 `r.Context().Err() != nil`：客户端已经走了，
再往里写只能是徒劳（而且会掩盖真实原因）。

统计口径同步排除它：`GetUsageStats` 的错误计数是 `status NOT IN ('ok','canceled')`，
模型吞吐（`ListModelThroughput`）过滤 `status <> 'canceled'`。前端用**中性灰**标签，
与暖橙的 `truncated`/`overflow`/`error` 区分（见 §13）。


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
| 上游 404 / 410（该目标没有这个模型） | **404** | `model_not_found` | 目标级配置问题（上游退役了那个模型、或链上模型名配错）。**不透传上游原文、也不报 502** —— 调用方给的模型名在网关这边是合法的，报 502 会让人去查错地方；是否可转移见 §10 |
| 未知模型名 | 404 | `model_not_found` | 网关自己产生 |
| 下游 Key 无效 | 401 | `invalid_api_key` | |
| 下游 Key 停用 / 过期 | 403 / 401 | | |
| 配额耗尽（Key 总量） | 429 | `insufficient_quota` | 已实现，见 §11.2；预检读库，OpenAI 计费语义同款 code |
| 限速（RPM/TPM） | 429 | `rate_limit_exceeded` | 尚未实现，§11.4 |
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

**选择策略**（已实现）：

- `weight` 加权的随机选择（默认）、或轮询
- 跳过 `disabled` 或处于冷却（`cooldown_until > now`）的凭证
- 冷却回写（已启用，`outwire.CredentialCooldown`）：401/403 冷却 30 分钟；402 冷却 1 小时；429 / 408 / 5xx / 传输层错误冷却 60s。
  注：Rosetta v0.5.1 未在 `APIError` 上暴露 `Retry-After`，故 429 统一按 60s 上限处理（不读上游响应头）。
- 冷却回写只影响「同一 provider 内换哪把 key」，不决定跨 provider 转移。
- **凭据只有两种失效方式**：`cooling`（冷却到期自动恢复）与 `disabled`（运营显式停用）——
  二者都在 `getHealthyCredentials` 里被真正过滤。旧实现另有一个 `MarkCredentialError`，
  它把内存里的 `status` 置成 `"error"`，但那个值既不参与健康过滤、也不回写库，调用它只是
  打一行 warning：**纯空操作**，2026-09-24 已删（连带删掉热路径里那条调用它的死分支 ——
  因为所有可转移状态都带非零冷却，那条 `else` 分支本来就永远进不去）。
  判废一把 key 的唯一真实手段是冷却。

**自动故障转移**（已实现，取代旧的 `fallback_route_id` 单跳兜底）：

一条 route 背后是 `route_targets` 承载的**有序上游链**——对外同一个 `public_name` 可挂多个 `(provider, model)`，按 `position` 升序尝试。`routes.provider_id / upstream_model_id` 退化为链首兼容列。

请求热路径（`handleChatCompletions` 的转移循环）语义：

- 仅当 `failover_enabled=1` 才进入多目标循环；否则只打链首，上游错误原样透出。
- 单次请求最多尝试 `failover_max_targets` 个候选（**全局设置**，见下方「参数落点」），按链顺序推进。
- **转移只发生在「尚未向下游写出任何字节」时**（pre-commit）：
  - 非流式：整段响应成功即提交；写出前任何可用错误都可换下一目标。
  - 流式：**SSE 头延迟到收到上游首个事件才写**。首字（TTFT）前的建立/超时失败仍可转移；一旦写出 SSE 头即视为已提交，此后断流按 `truncated` 语义处理，**不再转移**（避免把半截回答重复计费/重复输出）。
- 可转移的错误（`outwire.FailoverEligible`）：5xx / 401 / 402 / 403 / **404 / 410** / 408 / 429 / 传输层错误（超时）。不可转移：400 / 422 / 流截断。
  - **为什么 404/410 要转移**（2026-09-24 由 e2e 实测暴露）：上游 404 的语义是「我这个提供商没有这个模型」，属于**目标级**故障，而链正是为吸收目标级故障存在的。最现实的场景是上游退役了链首在用的那个模型 —— 它若不算可转移，整条链会永久硬失败，故障转移在最需要它的场景里恰好失效。反证：`cmd/gateway/failover_test.go` 的 `TestFailover_NonStreamSwitchesOnUpstream404` 与 `TestFailover_SingleTarget404MapsToModelNotFound` 在旧判定下必失败。
  - 404/410 可转移但**不冷却凭据**（`outwire.CredentialCooldown` 返回 0）：key 是好的，错的是目标的模型配置。惩罚凭据会把「目标级故障」放大成「provider 级故障」—— 一个健康 key 陪着一个配错的模型下线。这类失败只累计目标熔断。
- **目标级熔断**：某 `target` 连续失败达 `failover_failure_threshold`（全局设置，>= 1）后，进入 60s 冷却，期间该目标在链上被跳过；若整条链都在冷却，则回退为按序尝试全链（不让运营配出的链因瞬时抖动整体不可用）。
- **每次 attempt 只取该 provider 的一把凭证**：单请求内不会就地换同 provider 的下一把 key，失败即让位链上下一个目标；坏 key 的轮换交给跨请求冷却（下个请求自会选到好 key）。有意如此，避免「一个请求把某 provider 所有 key 各打一遍」放大延迟与配额消耗。
- **客户端断开即收手**：转移循环每轮顶部检查 `r.Context().Err()`，非空则直接返回——不再往链上后续目标打（半路跑掉的客户端不该消耗下游配额），也不记 error 用量（断流是客户端行为，非上游故障）。
  - 同理，**客户端在调用进行中断开时不做任何健康态记账**（`out.eligible && r.Context().Err() == nil`）。SDK 会把 context 取消包成 `TransportError` 落进可转移集合，若照记就会把一把健康凭据冷却 60s 并累计目标熔断 —— 对单 key provider 等于「几次用户点停止 = 该上游 60 秒整体不可用」（冷却期内凭据不再被选中，也就没有任何请求能成功以触发复苏）。判据用 `r.Context()`：它只在客户端断开/服务关停时取消，上游超时用的是派生 ctx，两者不会混淆。
- 命中成功目标：回写清除该凭证冷却 + 复位该目标熔断计数。
- 全链耗尽：透出最后一个上游错误（映射到对应 5xx/4xx），并记一条 error 用量。

**主目标列与链的一致性**：`routes.provider_id / upstream_model_id` 是链首（position 0）的兼容视图，但运行时解析以 `route_targets` 为准（一旦有目标行就不再回看主目标列）。因此 `PATCH /admin/api/routes/{id}` 改这两列时，`SyncHeadTarget` 会把改动落到链首，避免「DB 列变了、响应回显新值、实际流量仍打旧目标」的静默分叉；链为空则补一条 position 0。整体换链走 `PUT .../targets`，其内部再用链首反向同步主目标列。

**健康态与池重建**：`upstream.Pool` 的目标熔断表与凭据冷却在每次重建池（`BuildFromStore` / `BuildFromConfig`，即每个管理写操作触发的 reload）时一并清零——两层语义一致，配置变更本就是重新探测的正当理由（代价：管理员改配置会重置 ≤60s 的冷却/熔断）。这也堵住「每次保存链重生成 `target_id` → 旧熔断条目在表里单调堆积」的泄漏。

**健康态是纯运行时的，不落库、重启即清零**：冷却与熔断只活在 `upstream.Pool` 的内存里。`provider_credentials.cooldown_until / status` 与 `routes` 上的旧策略列**不是**事实来源（后者的策略列已整体删除，见 `store.dropDeadColumns`）。理由：这些状态生命周期极短（熔断 60s、冷却 ≤30min），冷启动一律「全健康」再由真实失败快速收敛，比持久化更简单也更快收敛；反之一旦落库，就要处理「重启后读到一批早已过期的冷却」这种伪状态。运维影响：**重启网关会清空全部健康态**（表现为故障目标立刻又被试一次），这是预期行为，不是故障。

**参数落点**：故障转移与超时策略**统一是全局的**，由「设置」页写入 `app_settings` 的 `runtime_defaults`，经快照（`snapshot.Snapshot.Runtime`）在转发热路径读取；DB 未配置的字段回落 `config.json` 的 `defaults`（这样老部署升级后原配置继续生效，直到在后台显式保存）。

| 设置项 | 含义 | config 兜底键 |
|---|---|---|
| `failover_max_targets` | 一次请求最多尝试链上几个目标 | `defaults.failover_max_targets` |
| `failover_failure_threshold` | 某目标连续失败几次即熔断 | `defaults.failover_failure_threshold` |
| `stream_first_token_timeout_ms` | 流式首字（TTFT）看门狗 | `defaults.stream_first_token_timeout_ms` |
| `stream_idle_timeout_ms` | 流式空闲看门狗 | `defaults.stream_idle_timeout_ms` |
| `upstream_timeout_ms` | 非流式整体超时 | `defaults.upstream_timeout_ms` |

保存设置后前端触发一次 `POST /admin/api/reload` 重建快照，**无需重启即生效**。

> 历史沿革：这些参数曾按 route 存在（`routes.max_targets / failure_threshold /
> stream_first_token_timeout_ms / nonstream_timeout_ms`），界面上每条路由各配一遍。
> 已整体移除：同一份策略散在多条路由上必然漂移，且会出现「改了全局默认、某条路由
> 却被旧覆盖值悄悄盖住」的排查地狱。现在每条路由只需要一个 `failover_enabled` 开关
> 加一条有序链。同理移除的还有 `routes.fallback_route_id`（单跳兜底，已被本链取代）。

管理面用 `PUT /admin/api/routes/{id}/targets` 原子替换整条链，`PATCH /admin/api/routes/{id}` 改 `failover_enabled` 等路由自身字段。

---

## 11. 配额与限速

### 11.1 计量口径

- 单位：**token**。`total = input + output`（reasoning token 若上游给出，计入 output 不重复累加，单独记录）
- 来源：`ChatResponse.Usage` / `EventMessageEnd.Usage`
- **usage 缺失时**：Rosetta 会标记 `IsZero()` 并计入 `usage_missing`。网关的处置是**按估算值扣减**（`rosetta.EstimateTokens` 输入 + 输出按字符数估），并记 `usage_state='estimated'`。理由：记 0 等于放行白嫖。界面里把 `estimated` 单独统计，便于发现是哪个上游不吐 usage。

### 11.2 为什么不可能精确（Key 总量配额 · 已实现）

流式请求的 output token 只有流结束才知道。所以**不存在请求前精确拒绝**。落地实现（`handleChatCompletions` 预检 + 请求后落库）：

```
请求前：GetKeyQuota 从库里读 (quota_tokens, used_tokens)；若 quota>0 且 used>=quota → 429 insufficient_quota
请求中：放行
请求后：usage_records 触发器 trg_update_used_tokens 令 used_tokens += total_tokens
```

- **读库而非读快照**：`used_tokens` 每次请求都在变，而内存快照只在管理写操作后重建，拿它做配额判断会严重滞后——所以预检直查 SQLite（主键单行读，局域网量级可忽略）。
- **超发容忍**：并发下多个在途请求可同时通过预检，最多多放行「一个请求」的量。**这是设计上接受的**（§11.2 前提），界面与文档都明说，不当 bug。
- **查询抖动 fail-open**：预检读库出错时记 error 日志并放行，不因一次读失败拒绝正常流量。
- **语义**：终身累计、不自动重置；`quota_tokens=0` = 不限（默认，向后兼容存量 key）。

### 11.3 并发安全

```sql
-- trg_update_used_tokens：AFTER INSERT ON usage_records
UPDATE access_keys SET used_tokens = used_tokens + NEW.total_tokens WHERE id = NEW.access_key_id;
```

扣减不再由请求路径手写 UPDATE，而是挂在 `usage_records` 插入上的 SQLite 触发器，与 INSERT 同语句原子完成。落库本身走 `recordUsage`（goroutine 异步），故扣减在响应返回后就近实时生效——配合上面的超发容忍，无需同步阻塞。写放大：每条请求一次 INSERT（触发器顺带一次 UPDATE），WAL 下无压力。

### 11.4 限速（P2 · 未实现）

> 注：本节的**下游** RPM/TPM 限速未实现，与 11.2 已实现的**总量配额**是两回事。
> `access_keys` 里原先的 `rpm_limit` / `tpm_limit` 占位列已于 2026-09-24 删除
> （能读、无写、无人用）；要做这一维时再加列 + 加写入口 + 加执行点，三件一起做。

| 维度 | 实现 |
|---|---|
| RPM | 内存固定窗口计数器（每分钟一个桶），Key 维度 |
| TPM | 同上，按请求前估算 + 请求后校正 |
| 实现 | 标准库 `sync.Map` + 每秒清扫；不用 `golang.org/x/time/rate`（避免依赖） |
| 重启 | 计数归零，接受 |

**另有一个已实现、不要与本节混淆的限速**：**管理后台登录失败限速**（`internal/server`）。
按来源 IP 记连续鉴权失败，10 次即进 60 秒冷却，冷却期内连 PBKDF2 都不做（省 CPU）。
存在的理由：管理密码下限只有 6 位，PBKDF2 21 万迭代把单次尝试压到几十毫秒（交互无感），
但**并发下 6 位弱口令依然可爆破** —— 这是唯一的在线防线。

- 来源 IP 取 `r.RemoteAddr`，**刻意不采信 `X-Forwarded-For`**（可伪造，采信等于把限速开关交给攻击者）。
- 判定「是否处于冷却期」必须用 `until.IsZero()`，**不能写 `!time.Now().Before(e.until)`** ——
  未冷却过的条目 `until` 是零值，而任意时刻都「不在零值之前」，那个条件对新条目恒为真，
  于是每次失败都重建条目、计数被清回 1，限速静默失效
  （2026-09-24 实测发现并修复：连打 26 次错误密码全是 401）。
  回归测试见 `internal/server/throttle_test.go`。

### 11.5 配额维度

只做 **Key 总量**（已实现，见 11.2）。per-model 配额、per-provider 配额记入后续路线，不进 P2。

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
    "stream_first_token_timeout_ms": 30000,
    "max_retries": 2,
    "max_request_body_bytes": 33554432,
    "failover_max_targets": 3,
    "failover_failure_threshold": 3
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

**故障转移相关默认**（`defaults` 下，全部可省略、缺省由 `setDefaults` 兜底）：
`stream_first_token_timeout_ms` 是流式首字（TTFT）看门狗——区别于 `stream_idle_timeout_ms`（已出字后的空闲超时），掐的是「一个事件都没等到」的慢上游，触发即在写出 SSE 头前转移下一目标。
`failover_max_targets` 是一次请求最多尝试链上几个目标；`failover_failure_threshold` 是某目标连续失败几次即熔断进 60s 冷却。

这些值同时也是「设置」页可热改项（写入 `app_settings.runtime_defaults`）。**优先级：设置页 > 本文件**：
设置页里显式保存过的值覆盖 config，未配置的字段回落到这里的 `defaults`，见 §10「参数落点」。

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

**做**：`usage_records` 落库；配额检查与同步扣减；RPM/TPM 限速；用量看板；凭证池（多 key、加权、冷却）；**自动故障转移**——最终落地形态是 `route_targets` 有序上游链（取代原设想的 `fallback_route_id` 单跳兜底），每条路由只有一个 `failover_enabled` 开关，策略参数是全局的（「设置」页 → `app_settings.runtime_defaults`），详见 §10。

**验收**：
1. 给 Key 设 10k token 配额，跑超后返回 429
2. 同一 provider 配 2 把 key，其中一把返回 401 后自动切另一把，且该 key 进入冷却
3. 并发 50 个请求，`SUM(usage_records.total_tokens)` 与 `used_tokens` 一致，不漏记
4. 一条 route 挂 2 个上游目标、链首返回 5xx 时，自动在写出任何字节前转移到链上次个目标 ✅（`cmd/gateway/failover_test.go` 覆盖）

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
| 上游重试 / 退避 | ⚠️ | `WithMaxRetries` 只对**幂等**方法生效（GET/HEAD/OPTIONS/PUT/DELETE）。对话是 POST，SDK **不重试**，故 `max_retries` 对 chat 无效——聊天路径的重试由本网关的故障转移链承担（`route_targets`）。含 `Retry-After`（60s 硬上限）。 |
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
| 凭证池 / 冷却 / 故障转移 | ❌ | 网关实现（已落地：加权选凭证 + 冷却回写 + `route_targets` 有序链自动转移，见 §10） |
| per-request endpoint/key 覆盖 | ❌ | 网关用多 Client 实例绕过 |

## 附录 B：需要 Rosetta 后续补的能力（不在本期）

**收集，但不现在提。** 等网关写完再定导出形状，避免过早把协议映射细节冻结成公开契约。

1. **导出统一模型 ⇄ 各协议 wire 的编解码**（含流式事件编码）。这是最大的重复劳动——网关要重写 9 件。合理形状可能是 `rosetta.EncodeOpenAIChat(req) ([]byte, error)` 与 `rosetta.DecodeOpenAIChat(body) (*ChatRequest, error)` 这类对称函数。
2. **流空闲超时选项** `WithStreamIdleTimeout(d)`。目前每个调用方都要自己写看门狗。
3. **per-request endpoint / key 覆盖**，让网关不必为每把凭证维护一个 Client（连接池问题）。
4. **`WithTimeout` 对流式生效**——或明确文档化为「仅 unary」，并给出推荐的看门狗实现范式。
