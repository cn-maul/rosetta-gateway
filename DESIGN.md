# Rosetta Gateway 设计文档

| 项 | 值 |
|---|---|
| 项目名 | **rosetta-gateway** |
| 项目性质 | 独立项目，不在 Rosetta 上改造（2026-09-15 定，理由见 §2.1） |
| module path | `github.com/cn-maul/rosetta-gateway` |
| 运行时依赖 | `github.com/cn-maul/rosetta` v1.0.1（上游 SDK）、`modernc.org/sqlite`（纯 Go SQLite）、`github.com/golang-jwt/jwt/v5`（管理会话） |
| 版本 | 与 `web/package.json` 同源（当前 1.5.0），镜像与二进制共用 |
| 状态 | **已实现**（P0–P3 全部落地，多用户、用量归档、配置导入导出均已上线） |
| 更新 | 2026-10-10 |

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
- **不做对下游的计费结算与账务**（不对客户开账单、不做充值发票/支付网关；余额只是
  网关内部的配额闸门）。**2026-10-08 修订**：此前写的是「不做计费结算与账务（只做
  用量记账，不做钱）」，而 v1.4.3 已引入用户余额、按 `cost_total` 自动扣费、余额耗尽
  拒绝调用（§11.5）。口径未变的是「不做钱」那一半：余额不可提现、不与任何支付
  渠道对接、费用按上游单价估算而非账单。
- 不做上游 Responses 的会话状态托管：`previous_response_id` / `store` 在上游协议为
  `openai-responses` 时经 `Extra` 透传给上游承接；chat / anthropic 上游无此概念，
  这两个字段被丢弃，客户端须自带全量历史
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
| D7 | 配额检查时机 | **请求前原子预占（used+reserved vs quota）+ 请求后按真实 usage 记账、退预占** | 纯 check-then-act 在并发下会整体放行；预占把「检查+占用」合并进同一条写事务（§11.2） |
| D8 | 流空闲超时 | **网关自己实现看门狗** | Rosetta 无此能力（已核实，全仓无 idle 相关实现） |
| D9 | `/v1/models` 形状冲突 | 按认证头分流 + 显式别名路径 | OpenAI 与 Anthropic 的该路径完全相同、响应形状不同 |
| D10 | 前端形态 | `go:embed` + **Vue 3 + Vite + TS**（原定原生 HTML/fetch，预设的升级边界触发后迁移，见 §13.1） | 产物仍进二进制单文件交付 |
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
  → auth       校验 sk-gw-... → access_key 记录（快照 O(1)，含归属用户/组/白名单）
  → quota      RPM 限速（快照）→ 解码 → TPM 预占 + 终身配额原子预占（§11），
               超额即拒（429）
  → routing    解析 model → 候选链（route_targets，故障转移见 §10）；
               候选按「能否满足请求硬约束」预过滤（如结构化输出 vs anthropic 上游）
  → upstream   逐目标尝试：选凭证（池 + 冷却状态），构造/取出 rosetta.Client
  → client.ChatStream(ctx, req)          ← ctx 来自 r.Context()
      ├─ 返回 error  → 映射状态码，可转移则换链上下一个目标，否则写 JSON 错误响应
      └─ 返回 stream → 写 200 + text/event-stream
                      启动 TTFT/空闲看门狗 + 心跳
                      loop: stream.Next() → outwire 编码 → Flush
                      结束：EventMessageEnd → usage 入账 + 下游结束事件
  → store      异步写入 usage_records（触发器同步累加 used_tokens 与终身累计）
```

---

## 4. 数据模型

SQLite，WAL 模式，`foreign_keys=ON`，`synchronous=NORMAL`。
**读写分池**（2026-10-04 起）：写池单连接（管理写 + 用量落库 + 审计写），
读池 4 连接承载一切只读查询 —— WAL 下读者与单写者并行，用量看板的重查询
不再阻塞热路径上的配额预检。

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

-- 下游访问凭证（归属主体见 users；完整列清单以 internal/store/store.go 为准）
CREATE TABLE access_keys (
  id              TEXT PRIMARY KEY,
  user_id         TEXT REFERENCES users(id) ON DELETE CASCADE,
                  -- NULL = 无归属：迁移期存量 key 已被 retireOrphanKeys 禁用，
                  -- 鉴权直接 401（ErrKeyUnowned），见 MULTIUSER.md §5.1
  key_hash        TEXT NOT NULL UNIQUE,      -- SHA-256(明文)，不存明文
  key_prefix      TEXT NOT NULL,             -- 形如 "sk-gw-a1b2"，用于界面展示
  name            TEXT NOT NULL,
  enabled         INTEGER NOT NULL DEFAULT 1,
  quota_tokens    INTEGER NOT NULL DEFAULT 0,     -- 0 = 不限；终身累计 input+output 上限
  used_tokens     INTEGER NOT NULL DEFAULT 0,     -- 累计真实用量（usage_records 触发器维护）
  reserved_tokens INTEGER NOT NULL DEFAULT 0,     -- 在途预占（2026-10-07 计费修复，§11.2）
  rpm_limit       INTEGER NOT NULL DEFAULT 0,     -- 每分钟请求数上限，0 = 不限（§11.4）
  tpm_limit       INTEGER NOT NULL DEFAULT 0,     -- 每分钟 token 上限，0 = 不限（§11.4）
  expires_at      INTEGER NOT NULL DEFAULT 0,     -- 毫秒时间戳，0 = 永不过期
  allowed_models_json TEXT,                       -- key 级模型白名单，NULL/空 = 不限制
  allowed_ips     TEXT,                           -- CIDR 逗号分隔原文，空 = 不限制
  group_id        TEXT REFERENCES groups(id) ON DELETE SET NULL,
                  -- key 级分组覆盖，空 = 沿用归属用户的分组；仅管理员可设/清
  created_at      INTEGER NOT NULL
);

-- 身份主体（管理员也是 role='admin' 的普通用户，users 表是唯一身份来源）
CREATE TABLE users (
  id            TEXT PRIMARY KEY,
  username      TEXT NOT NULL COLLATE NOCASE UNIQUE,
  display_name  TEXT,
  password_hash TEXT NOT NULL,           -- PBKDF2-HMAC-SHA256，21 万次迭代
  role          TEXT NOT NULL DEFAULT 'user',   -- admin | user
  status        TEXT NOT NULL DEFAULT 'active', -- active | disabled
  group_id      TEXT REFERENCES groups(id) ON DELETE SET NULL,
  quota_tokens  INTEGER NOT NULL DEFAULT 0,      -- 用户级总配额（三级配额最外层）
  balance_cents INTEGER,                         -- 账户余额（分，人民币）。**NULL = 不限额**，
                   -- 0 = 真的一分钱都没有（触发 402）。与 quota_tokens 的「0 = 不限」
                   -- 刻意相反：0 在余额语义下是一个有意义的实数状态，必须能表达。
  used_tokens   INTEGER NOT NULL DEFAULT 0,      -- 展示值；执行走实时 SUM（MULTIUSER.md §4.3）
  auth_version  INTEGER NOT NULL DEFAULT 1,      -- 会话失效栅栏（改密/禁用/改角色 +1）
  remark        TEXT,
  created_at    INTEGER NOT NULL,
  updated_at    INTEGER NOT NULL,
  last_login_at INTEGER NOT NULL DEFAULT 0
);

-- 分组与组级模型白名单（groups 刻意不带 quota/rpm/tpm 列 —— 没有执行点的列不建）
CREATE TABLE groups (
  id          TEXT PRIMARY KEY,
  name        TEXT NOT NULL COLLATE NOCASE UNIQUE,
  description TEXT,
  created_at  INTEGER NOT NULL,
  updated_at  INTEGER NOT NULL
);
CREATE TABLE user_group_models (
  group_id     TEXT NOT NULL REFERENCES groups(id) ON DELETE CASCADE,
  public_model TEXT NOT NULL,
  PRIMARY KEY (group_id, public_model)
);

-- 逐请求用量（user_id / cost_total 由迁移补列；归档与累计见表后说明）
CREATE TABLE usage_records (
  id                TEXT PRIMARY KEY,
  ts                INTEGER NOT NULL,
  access_key_id     TEXT NOT NULL,
  user_id           TEXT,                  -- 冗余固化归属：历史事实不随改名/删 key 变
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
  cost_total        REAL NOT NULL DEFAULT 0,  -- 落库时按当时单价固化；费用查询只 SUM 本列
  usage_state       TEXT NOT NULL,         -- reported|missing
  status            TEXT NOT NULL,         -- ok|truncated|overflow|canceled|error（见 §8.2）
  http_status       INTEGER NOT NULL,
  error_code        TEXT,
  latency_ms        INTEGER NOT NULL,
  ttfb_ms           INTEGER NOT NULL DEFAULT 0,
  request_id        TEXT                   -- 与访问日志同 ID，30 天内按库追查单次调用
);

CREATE INDEX idx_usage_ts        ON usage_records(ts);
CREATE INDEX idx_usage_key_ts    ON usage_records(access_key_id, ts);
CREATE INDEX idx_usage_model_ts  ON usage_records(public_model, ts);
CREATE INDEX idx_usage_prov_ts   ON usage_records(provider_id, ts);
```

**保留与归档（已实现，MULTIUSER.md §4.8）**：`usage_records` 明细保留 30 天，
超期由每日剪枝聚合进 `usage_daily_rollups`（按维度按天的表 A）后删除；
`usage_totals`（恒定单行的表 B）由触发器在落库时实时累加，**与剪枝完全解耦**，
保证「明细可删、终身累计不变」。聚合与删除在同一事务内对**同一批行**执行
（先按 `id` 圈批，聚合与删除共用该集合），水位只在明细清空到切点时推进。
对账入口：`POST /admin/api/keys/{id}/recompute-usage`（key 级）与
`POST /admin/api/usage/prune`（手动剪枝）。

**查询侧的防线**：「近期表现」类统计（模型速度/成功率/TTFB，`ListModelThroughput`）固定只回看近 30 天（`throughputWindow`）——窗口函数要对全历史排序，调用量大的 provider 积累几十万行后，挂它的 `GET /providers/{id}/models` 会变成重查询并阻塞唯一的 DB 连接。总览页「全部」档的全区间聚合保留（只在打开总览页时触发），缓存命中率与总统计合并在同一条 SELECT 里完成。

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

### 5.3 模型容量元数据（2026-10-04）

外部工具（Cherry Studio / LobeChat / 各类网关面板）需要按模型读取上下文与输出上限，
约定字段：`context_length`（OpenRouter 约定）、`max_input_tokens` / `max_output_tokens`
（LiteLLM 约定）。取值链：链首上游模型的 `context_window` / `max_output_tokens`
→ 设置页「模型默认」→ 两级都为 0 则**不加字段**（不编造数字）。
单模型详情走 `GET /v1/models/{model}`（轨道一虚拟名与轨道二 slug/model 皆可查）。

---

## 6. 对外接口

### 6.1 下游入口

| 路径 | 协议 | 备注 |
|---|---|---|
| `POST /v1/chat/completions` | OpenAI Chat | P0，已实现 |
| `POST /v1/messages` | Anthropic Messages | **已实现（2026-10-02）**——Claude Code 把 `ANTHROPIC_BASE_URL` 指向网关即可用；thinking 回放（含签名）见 §17 R4 |
| `POST /v1/responses` | OpenAI Responses | **已实现（2026-10-04）**——Codex CLI 等客户端接入点；`previous_response_id`/`store` 被忽略（网关不托管会话状态，store:false 的全量历史客户端天然兼容）；`text.format` 跨协议映射为 openai-chat 的 response_format |
| `GET /v1/models` | 形状按认证头分流（D9） | 已实现，`?include=upstream` 展开轨道二；**条目带容量元数据**（见 §5.3） |
| `GET /v1/models/{model}` | 单模型详情 | 已实现（OpenAI/Anthropic 形状 + 容量元数据；`/openai/v1/models/{m}`、`/anthropic/v1/models/{m}` 别名） |
| `GET /dashboard/billing/*`、`GET /v1/organization/usage|costs` | 额度/费用查询 | 已实现（2026-10-04），见 §6.4 |

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

接受两种承载方式，任一命中即通过：

| 方式 | 头 |
|---|---|
| Bearer | `Authorization: Bearer sk-gw-...` |
| Anthropic 风格 | `x-api-key: sk-gw-...` |

校验：`SHA-256(明文)` 查 `access_keys.key_hash` 索引。上游 Key 是高熵随机串，无需 bcrypt 类慢哈希。

**刻意不支持 `?key=sk-gw-...`**（早期文档里有，现已移除）：query 串会被写进访问日志、
反向代理日志与 `Referer`，等于把凭据复制到系统里每一个会记录 URL 的地方
（见 `internal/auth/auth.go` 的 `extractKey` 注释）。SDK 与脚本改用 Bearer 头即可。

失败返回 401；Key 被停用、过期、来源 IP 不在白名单均返回 **403**
（`invalid_api_key` / `key_expired` / `ip_not_allowed`）；配额耗尽返回
429 `insufficient_quota`；模型不在该 key 的白名单内返回 403 `model_not_allowed`。
无归属的 key 返回 401（`ErrKeyUnowned`）—— 见 §6.4「先建后认领」。

### 6.3 管理接口

统一挂在 `/admin/api/*`。管理鉴权只有一条通道：**users 表 + JWT 会话**
（设计与演进史见 `MULTIUSER.md`），不存在任何旁路 —— 旧版「`admin_auth.json` 密码文件 +
`config.json` 的 `admin_token` / `ADMIN_TOKEN` 环境变量」双通道已整体删除
（`internal/adminauth` 包不复存在，`Config.AdminToken` 字段已删）。
**凭据是运行时状态，改密码后旧会话立即失效、无需重启。**

首次初始化由 `GET/POST /admin/api/bootstrap` 完成：

- 系统启动时若 users 表没有任何**已设密码**的 admin，自动建出一个空密码 admin 账号
  （`cmd/gateway/bootstrap_admin.go`）；
- 登录页探测到这种状态就渲染「首次设置密码」表单（不是登录表单），
  提交后一步完成设密码 + 登录（`internal/admin/user_handler.go` 的 `BootstrapSetup`）；
- **窗口是一次性的**（2026-10-07 加固）：`app_settings` 里的 `bootstrap_completed`
  标记与首次设密在同一事务写入 —— 设过一次后，无论日后是否再出现空密码 admin
  （该形态本身已被下一条堵死），免鉴权窗口永不重开；
- 防 线：只对 `role='admin' AND password_hash=''` 的账号生效（条件 UPDATE，
  设过即 409）、创建 admin 必须带初始密码、空密码账号禁止升级为 admin、
  同源校验 + Content-Type 断言（防 CSRF）；
- 暴露面边界：全新部署且监听非回环时，任何能连到端口的人都能抢先完成首次设置
  —— 窗口只开这一次，抢到的人成为第一个管理员；缓解是默认回环监听 +
  启动时的 ERROR 告警（`FindUninitializedAdmin != nil && !isLoopbackListen`）。

| 端点 | 放行规则 |
|---|---|
| `GET /admin/api/bootstrap` | 恒放行（返回是否需要初始化与待初始化账号名） |
| `POST /admin/api/bootstrap` | 仅当 `bootstrap_completed` 标记未写入且存在未初始化 admin 时放行（同源 + JSON Content-Type；标记与首次设密同事务写入，窗口一次性） |
| `POST /admin/api/login` / `POST /admin/api/logout` | 免鉴权（登录 / 登出本体） |
| 其余 | `Authorization: Bearer <会话令牌>`，查 users 表比对 `auth_version` |

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
POST   /admin/api/providers/{id}/models/import    批量导入（单事务，要么全进要么全不进）
PATCH  /admin/api/models/{id}
DELETE /admin/api/models/{id}
GET    /admin/api/upstream-models                 全部上游模型扁平列表（含 provider_id，
                                                  供 Routes/Settings 一次拉全，替代逐 provider 的 N+1；
                                                  不挂吞吐重查询）

GET    /admin/api/routes
POST   /admin/api/routes
PATCH  /admin/api/routes/{id}
DELETE /admin/api/routes/{id}
GET    /admin/api/routes/{id}/targets             读有序上游链（含 provider/model 展示名）
PUT    /admin/api/routes/{id}/targets             原子整体替换链；链首回写 routes 主目标列

GET    /admin/api/users                         用户列表（admin）
POST   /admin/api/users                         新建用户；role=admin 必须带初始密码
PATCH  /admin/api/users/{id}                    角色/状态/配额/分组/备注；改角色或状态
                                                 自动递增 auth_version 使旧会话失效
DELETE /admin/api/users/{id}
POST   /admin/api/users/{id}/reset-password     管理员重置密码

GET    /admin/api/groups                        分组与组级模型白名单（admin）

GET    /admin/api/keys                          列表（普通用户只看到自己的）
POST   /admin/api/keys                          **所有登录用户**（2026-10-07 重开）：
                                                  归属恒为创建者自己，请求里的
                                                  `user_id` 被忽略（含管理员）；
                                                  普通用户额度封顶到用户级
                                                  quota_tokens、rpm/tpm 清零
PATCH  /admin/api/keys/{id}                     可改 name / enabled / quota_tokens /
                                                 rpm_limit / tpm_limit / expires_at /
                                                 allowed_models / allowed_ips / group_id；
                                                 普通用户只能收紧不能放宽（guardNoLoosening）
DELETE /admin/api/keys/{id}
POST   /admin/api/keys/{id}/recompute-usage     从用量记录重算 used_tokens（配额漂移自愈）

GET    /admin/api/audit?limit=                  管理写操作审计（新→旧，只记字段名不记值）
GET    /admin/api/usage?from=&to=&group_by=key|model|provider|day
                                                 from/to 为毫秒时间戳；**from=0 一律表示
                                                 「全部历史」**（本端点与 by-* 系列语义统一，
                                                 未传 from 时 Query 默认近 24h）
GET    /admin/api/usage/by-provider|by-day|by-model|by-key    分维度用量（按身份收窄）
GET    /admin/api/usage/history.csv             调用明细 CSV 导出（与 history 同一套
                                                 status/model/key_id 过滤，无分页）
POST   /admin/api/usage/prune                   手动触发归档剪枝（每日定时之外的人口）
GET    /admin/api/stats                         当前快照：总请求/总 token/错误率/各 provider 健康
GET/PUT /admin/api/settings                     运行时全局默认（超时/故障转移策略，§10）
POST   /admin/api/config-export/export|import   配置导出/导入（§13.3，requireAdmin）
GET    /admin/api/model-names                   公开模型名清单（按身份收窄，供白名单选择）；附带
                                                  `prices[]`（与 `models` **平行**的数组，prices[i]
                                                  对应 models[i]，三项单价元/百万 tokens，0 = 未配置）
PUT    /admin/api/users/{id}/balance             管理员充值/扣减余额，body `{delta_cents}`（分，正充负扣）
GET    /admin/api/me / POST /admin/api/logout   当前用户信息 / 登出（递增 auth_version）
POST   /admin/api/reload                        从 DB 重建内存快照
```

所有写操作的事务边界：**先写 DB，提交成功后再重建快照**。DB 写失败则快照不动。

**重建由服务端自动执行**（2026-10-01 起）：管理写请求成功（2xx）后，`server.AutoReload` 中间件就地调用 `runtimeReloader.Reload`（池重建 + 快照重建，`sync.Mutex` 串行化，两边都构建成功才原子替换，任一步失败运行时保持旧状态）。**重建失败有后台兜底**（2026-10-07 起）：失败即置 `dirty` 标志，后台循环按 30s 起步、指数退避（封顶 10 分钟）反复重试直至收敛，避免「响应已 200 +『已禁用』而数据面仍在放行」的分叉无限期存在（健康检查可读 `IsDirty`）。此前生效路径完全依赖前端写完自觉调 `POST /admin/api/reload`——任何绕过前端的调用方（curl/脚本）写完不调 reload 就是静默分叉，最敏感的是**禁用下游 Key 后 auth 读旧快照照常放行**。前端 `mutate()` 里的 reload 调用保留为兜底。

### 6.4 额度/费用查询（2026-10-04）

两套形状（实现见 `cmd/gateway/billing.go`），均需有效 sk-gw key：

| 端点 | 形状来源 | 数据口径 |
|---|---|---|
| `GET /v1/organization/costs` | OpenAI 官方 Usage/Costs API（page + bucket + `organization.costs.result`） | **按调用者身份收窄**（管理员/运维凭据=全组织，普通用户=自己名下）；费用按 upstream_models 单价实时估算（口径同 §11.1 统计），`amount.currency` 诚实标 `cny` |
| `GET /v1/organization/usage/completions` | 同上 | 同上；input/cached/output/请求数按 1h/1d 桶聚合 |
| `GET /dashboard/billing/subscription`（含 `/v1/` 前缀别名） | one-api/new-api 时代起客户端通用 | **per-key**：`hard_limit_usd` 等承载 key 的 token 配额 |
| `GET /dashboard/billing/usage`（含别名） | 同上 | **per-key**：区间内已用 token 折算 `total_usage`（美分） |

「组织」= 本网关整个部署，因此上面两个 `/v1/organization/*` 端点**按调用者身份收窄**
（实现见 `cmd/gateway/billing.go` 的 `orgCostsScope` + `store.SumUsageBucketsScoped` /
`SumCostBucketsScoped`）。作用域只由服务端根据会话身份判定，绝不从 query 参数读取。
修复前它们只做 `auth.Authenticate`，任何一把 `sk-gw` key 都能读到**别的租户**的
模型名、用量与费用（实测普通用户的 `/admin/api/stats` 显示 `cost:0`，而同一把 key
打 `/v1/organization/costs` 直接拿到 `value:0.000072 cny` —— 恰好是 stats 不给的那笔）。

`/dashboard/billing/usage` 的 `start_date` / `end_date`（`YYYY-MM-DD`）**给了但解析不了、
或区间反向，一律 400**：静默回退到「最近 30 天」等于把另一个窗口的数据返回给调用方
却让它以为生效了。参数**缺省**才回退近 30 天。

**单位映射（重要）**：dashboard billing 系把 **百万 tokens 记作 1 美元等价单位（PTM）**
—— 客户端 UI 只消费两个数的比值（余额/进度条），同量纲保证比例正确；绝对值是
token 量纲而非美元。key 未设配额时 `hard_limit_usd` 返回 1e9（避免 UI 把 0 显示成
「零余额」），响应带 `total_tokens` 扩展字段供按 token 展示的工具使用。

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

三处刻意偏离上面这套通用规则，各有理由：

- **`POST /admin/api/keys` 的归属恒为创建者自己**：`user_id` 被**静默忽略**（不 403）。
  任何人都不能代建 —— 包括管理员。这样 key 的来源永远是「谁建的」，
  归属变更永远是「谁改的」，审计里两件事不会混在一条记录里。忽略而不是拒绝，
  是因为旧版本前端仍会带这个字段，让一个本该成功的建 key 操作硬失败没有意义；
  越权没有发生，就不该为它中断。拿不到身份 → 401（fail-closed：绝不放行空 ID）。
- **自助发 key 的封顶**（仅普通用户，管理员不受限）：`quota_tokens` 不得超过其
  **用户级** `users.quota_tokens`（0 = 不限时才允许 key 不限）；`rpm_limit` /
  `tpm_limit` **强制清零** —— users 表没有 rpm/tpm 列，没有用户级上限可比，
  保留任意填的能力就是留一个不限速的口子。`expires_at` / `allowed_ips` 允许自设：
  这两个字段没有「更宽」的方向，自设是**给自己加限制**（收紧），与
  `group_id` 相反 —— 后者能指向一个更宽松的组，是**放宽**，故仍限管理员。
  管理员对**具体某把** key 施加的收紧，用户既不能 PATCH 撤销（`guardNoLoosening`），
  重建一把也不会「更宽松」，只会失去该 key 上已有的全部限制性配置。
  部署侧仍应给普通用户配 `users.quota_tokens`：它不设时自助建 key 就是不限额凭证。
- **`PATCH /admin/api/keys/{id}` 的 `user_id`**（仅管理员有效）：这是**归属变更的
  唯一入口** —— 管理员先给自己建一把，再用这个字段认领给某个**已存在**的用户。
  落库走独立的 `store.ReassignAccessKey`；`UpdateAccessKey` 刻意不写 `user_id`
  （归属不该由一个 PATCH 随手改写，见 `user_dao_test.go` 的断言）。指向不存在的
  用户 → 400。历史数据里仍可能存在 `user_id:""` 的无归属 key：鉴权时得到
  `ErrKeyUnowned` → 401，且会被 `store.retireOrphanKeys` 在迁移时退役。
- **模型白名单 `allowed_models`（key 与组）**：出现**空白条目**（trim 后为空）→ 400。
  静默丢弃会把「收紧到某模型」变成「完全不限制」（归一后为空 → 落库 NULL →
  读回 `nil` → `AllowAll()`），而界面看不出任何区别。清空白名单用**显式的空数组 `[]`**。
  响应回显一律是**归一后**的值（去空白、去重、排序），与随后的 `GET` 一致。
- **`allowed_ips`**：逗号分隔，落库前逐段 trim 并丢掉空段（`"10.0.0.0/8,"` 与
  `"10.0.0.0/8"` 语义完全相同），回显的也是归一后的原文。无法解析的段 → 400。

`providers.timeout_ms` 除「非负」外还有**上界 86 400 000 ms（24h）**
（`config.MaxDurationMillis`）：`time.Duration(ms) * time.Millisecond` 在
ms > 9.223e12 时会回绕成**负数**，负 Duration 让 `context.WithTimeout` 立即过期。
`config.json`、设置页、provider PATCH 三处共用同一个常量。

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

**已实现（2026-10-02 起分批落地）**：三套入口全部完成——OpenAI Chat（P0）、
Anthropic Messages（2026-10-02）、OpenAI Responses（2026-10-04）。转发骨架
（鉴权/配额/路由/故障转移/看门狗/落库）通过 `ingressCodec` 接口复用，
各协议只实现「解码请求 + 渲染错误/响应/SSE」。跨协议的私有字段翻译
（tool_choice 三方互译、response_format ↔ text.format 等）与硬约束过滤
（结构化输出请求不打 anthropic 上游）见 §10 与 `internal/inwire`。

---

## 8. 流式转发

```
下游 SSE  ←  outwire 编码  ←  rosetta.Event  ←  Stream.Next()
```

### 8.1 要点

| 项 | 做法 |
|---|---|
| 取消传播 | 上游 ctx 直接取 `r.Context()`。下游断开 → ctx 取消 → Rosetta 中止流 → 上游连接关闭。**这是最直接的止损点，必须做对**。下游断开**不算错误**：`usage_records.status` 记 `canceled`（见 §8.2），不计入错误率 |
| 空闲看门狗 | 独立 `time.AfterFunc`，默认 60s 无事件则 `stream.Close()`。每收到一个事件重置定时器，**回调执行时按「距最近事件的间隔」复核**——`Timer.Reset` 追不回已派发的回调，超时边界上事件与回调竞速时不复核会误杀健康流（2026-10-07 修复）。超时视为 `status=truncated`。**注意 `Stream.Close()` 不写 `stream.Err()`**（Rosetta 只在真的读失败时才置 err），所以看门狗必须自己用 `atomic.Bool` 留痕；否则 `Err()==nil` → 状态保持 `ok` → 下游收到 `finish_reason:"stop"` + `[DONE]`，卡死的上游被伪装成正常收尾（详见 §8.2） |
| 心跳 | 空闲超过 `idle/2` 时下发 `: keepalive\n\n`，防中间代理超时断连 |
| Flush | 用 `http.NewResponseController(w).Flush()`（Go 1.20+），不用 `http.Flusher` 类型断言 |
| 首字节时机 | `ChatStream` 成功后才写 `200 + Content-Type: text/event-stream` + `Cache-Control: no-cache` + `X-Accel-Buffering: no` |
| 结束事件 | OpenAI 系发 `data: [DONE]`；Anthropic 发 `event: message_stop`。**只有 `status=="ok"` 才发** |
| 思考增量 | `EventThinkingDelta` 编码为 `delta.reasoning_content`（DeepSeek / Qwen / vLLM 的既成约定，Rosetta 的 openai-chat 适配器也按这个键回读）。**不能丢**：只吐思考的流丢了它就是个零内容的流 |
| 空文本事件 | `EventThinkingDelta` 的 `Text==""` 是 Anthropic thinking signature 的载体，OpenAI 下游无对应字段，跳过即可 |
| usage 合成 | OpenAI 下游要 usage 需客户端传 `stream_options.include_usage`；网关在 `EventMessageEnd` 处合成仅含 usage 的 chunk，且仅当客户端要求时下发 |
| 断流处理 | 见 §8.2 |
| 缓冲 | 逐事件 Flush，不做批量聚合（延迟优先） |
| Anthropic 下游 | 事件序列 `message_start → (content_block_start → delta* → stop)* → message_delta(stop_reason, usage) → message_stop`，块 index 严格递增（文本/思考/工具各一块，`outwire.AnthropicSSE` 状态机）。message_start 的 `input_tokens` 发 0 —— 上游在结束时才报 usage，权威值随 message_delta 的累计 usage 补齐；断流发 `event: error` 且不发 message_stop（§8.2）。协议骨架与 OpenAI 共用一个 `StreamSink` 接口（`ingressCodec`），看门狗/心跳/断流归类只写一份 |
| 全局 WriteTimeout | **必须为 0**：net/http 的写超时从「开始写响应」起算、覆盖整个响应时长，定时值一到会把仍在正常吐字的长流硬切（大输出的慢推理模型恰好会撞上），下游只看到来历不明的 truncated。流的生命周期由 TTFT/空闲看门狗约束，非流式由 `upstream_timeout` 限定 handler 时长；全局写超时在这里只会误伤，不多保护任何东西（2026-10-01 修正，此前 5 分钟） |

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
| **余额耗尽（用户）** | **402** | `insufficient_balance` | 与上面的 token 配额**刻意分开**：那是终身 token 额度（换 key / 换账号），这一条是人民币余额（**充值**）。两者塌成一类会让客户端给出错误建议。`error.type` 沿用 `insufficient_quota`（OpenAI SDK 只认得有限几种 type，而「额度不足、去充值」的语义一致），所以**分流要按 code + 状态码，不能只看 type**。管理员豁免；只对成功请求扣费，扣费额 = usage 落库的 `cost_total` |
| 限速（RPM/TPM） | 429 | `rate_limit_exceeded` | 已实现，§11.4；被拒请求不计数，避免客户端重试把窗口锁死 |
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

请求热路径（`handleIngress` 的转移循环）语义：

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
  - **没走到记账的探测名额必须显式归还**（2026-10-07 修复）：熔断 half-open 名额由 `ClaimTargetProbe` 领取，正常由成功/失败记账释放；不可转移错误与客户端断开两种形态不会走到记账，循环尾部用 `ReleaseTargetProbe` 归还 —— 否则一次 400 就把目标占死到下一次池重建。
- 命中成功目标：回写清除该凭证冷却 + 复位该目标熔断计数。
- 全链耗尽：透出最后一个上游错误（映射到对应 5xx/4xx），并记一条 error 用量，归因到**实际尝试到**的目标（而非链上最后一个候选）。

**主目标列与链的一致性**：`routes.provider_id / upstream_model_id` 是链首（position 0）的兼容视图，但运行时解析以 `route_targets` 为准（一旦有目标行就不再回看主目标列）。因此 `PATCH /admin/api/routes/{id}` 改这两列时，`SyncHeadTarget` 会把改动落到链首，避免「DB 列变了、响应回显新值、实际流量仍打旧目标」的静默分叉；链为空则补一条 position 0。整体换链走 `PUT .../targets`，其内部再用链首反向同步主目标列。

**健康态与池重建**：`upstream.Pool` 的目标熔断表与凭据冷却在每次重建池（`BuildFromStore` / `BuildFromConfig`，即每个管理写操作触发的 reload）时一并清零——两层语义一致，配置变更本就是重新探测的正当理由（代价：管理员改配置会重置 ≤60s 的冷却/熔断）。这也堵住「每次保存链重生成 `target_id` → 旧熔断条目在表里单调堆积」的泄漏。重建是**先完整构建、后原子替换**（`PrepareFromStore` 构建局部表 → `Install` 锁内换入）：构建失败时旧池/旧快照原样保留，绝不出现「清空后查库失败」留下的空池——那会让全站 /v1 选不到上游，直到下一次成功的 reload。

**健康态是纯运行时的，不落库、重启即清零**：冷却与熔断只活在 `upstream.Pool` 的内存里。`provider_credentials.cooldown_until / status` 与 `routes` 上的旧策略列**不是**事实来源（后者的策略列已整体删除，见 `store.dropDeadColumns`）。理由：这些状态生命周期极短（熔断 60s、冷却 ≤30min），冷启动一律「全健康」再由真实失败快速收敛，比持久化更简单也更快收敛；反之一旦落库，就要处理「重启后读到一批早已过期的冷却」这种伪状态。运维影响：**重启网关会清空全部健康态**（表现为故障目标立刻又被试一次），这是预期行为，不是故障。

**参数落点**：故障转移与超时策略**统一是全局的**，由「设置」页写入 `app_settings` 的 `runtime_defaults`，经快照（`snapshot.Snapshot.Runtime`）在转发热路径读取；DB 未配置的字段回落 `config.json` 的 `defaults`（这样老部署升级后原配置继续生效，直到在后台显式保存）。

| 设置项 | 含义 | config 兜底键 |
|---|---|---|
| `failover_max_targets` | 一次请求最多尝试链上几个目标 | `defaults.failover_max_targets` |
| `failover_failure_threshold` | 某目标连续失败几次即熔断 | `defaults.failover_failure_threshold` |
| `stream_first_token_timeout_ms` | 流式首字（TTFT）看门狗 | `defaults.stream_first_token_timeout_ms` |
| `stream_idle_timeout_ms` | 流式空闲看门狗 | `defaults.stream_idle_timeout_ms` |
| `upstream_timeout_ms` | 非流式整体超时 | `defaults.upstream_timeout_ms` |

保存设置后由服务端自动重建快照（`server.AutoReload`，见 §6.3），**无需重启即生效**，也不依赖前端调 reload。

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
- **usage 缺失时**：SDK 的 `Usage.IsZero()` 为真即视为「上游一个 token 数都没给」，记
  `usage_state='missing'`（**不是** `reported`，理由见 §17 R9）。此状态不做估算扣减 ——
  估算值是编的，拿它当账单只会把漏账伪装成正常数据。处置是：TPM **保留预占量不回滚**
  （否则客户端可用「让上游不吐 usage」把限速绕过），终身配额侧**退回预占**
  （净计入 0），界面单列统计 `missing`，让「哪个上游不吐 usage」这件事暴露出来而不是静默。

### 11.2 终身配额：原子预占 + 触发器记账（已实现）

流式请求的 output token 只有流结束才知道，**不存在请求前精确拒绝**。最早的实现是纯查询
（`GetKeyQuota` 读 used → 比较 → 放行），两个致命问题：check-then-act 在并发下全部通过
（剩余 1000 token、50 个并发各预估 200 会全部放行，超发数十倍）；且曾因返回值解构错误
让「全新 key 的配额完全不生效」（2026-10-07 P0-1）。现行实现是三段式：

```
请求前（ReserveQuota，单条写事务内「检查 + 占用」）：
  读 (quota_tokens, used_tokens, reserved_tokens)
  quota<=0            → 不限额，直接放行
  used+reserved+est > quota → 429 insufficient_quota
  否则 reserved_tokens += est          ← 预占进**独立列**，不碰 used_tokens

请求中：放行

请求后（rateCommit 收尾）：
  usage_records INSERT → 触发器 used_tokens += total_tokens   （真实用量，唯一写 used 的路径）
  ReleaseQuota：reserved_tokens -= reserved                       （退预占）
```

- **预占为什么独立成列**（2026-10-07 P0-2）：最初预占直接加进 `used_tokens`，与
  「触发器累加真实用量」叠加后每请求净记账 2×真实值 —— 差值校正语义与触发器语义
  在同一列上纠缠。分列后 `used_tokens` 只归触发器写、`reserved_tokens` 只归
  预占/释放写，两列各只有一个写入者，口径不再打架。
- **预占口径**：`est` = 请求字符估算 + `max_tokens` 全额（输出侧按上限预占，
  宁可先多占后退也不先少占再超发）；预占为 0 时按 1 计，让并发请求数本身成为约束。
- **usage missing**：收尾仍必须退预占（2026-10-07 P1-3 修复——此前整段收尾被跳过，
  一次请求可永久吃掉数万 token 额度）；TPM 窗口预占保留（窗口自愈）。
- **读库而非读快照**：`used_tokens` 每次请求都在变，内存快照只在管理写操作后重建；
  预检直查 SQLite，写锁天然把并发请求串行化，「检查」与「占用」之间不存在窗口。
- **查询抖动 fail-open**：预检读库出错时记 error 日志并放行，不因一次读失败拒绝正常流量。
- **自愈入口**：`POST /admin/api/keys/{id}/recompute-usage`（`RecomputeUsedTokens`）从
  用量来源重算 `used_tokens`，是派生值漂移的唯一修正手段。
- **语义**：终身累计、不自动重置；`quota_tokens=0` = 不限（默认，向后兼容存量 key）。

### 11.3 并发安全

```sql
-- trg_update_used_tokens：AFTER INSERT ON usage_records
UPDATE access_keys SET used_tokens = used_tokens + NEW.total_tokens WHERE id = NEW.access_key_id;
```

真实用量的扣减不由请求路径手写 UPDATE，而是挂在 `usage_records` 插入上的 SQLite
触发器，与 INSERT 同语句原子完成。`used_tokens` 的写入者**只有这一个触发器**，
`reserved_tokens` 的写入者只有 `ReserveQuota` / `ReleaseQuota`（各自单条 UPDATE）——
三个写入点互不重叠，配额判定 `used+reserved+est <= quota` 在写锁串行化下无竞态窗口。
落库本身走 `recordUsage`（goroutine 异步），故扣减在响应返回后就近实时生效。
写放大：每条请求一次 INSERT（触发器顺带两次 UPDATE：key 级 used_tokens 与
终身累计表 `usage_totals`），WAL 下无压力。

### 11.4 限速（已实现，2026-10-04）

> 本节的**下游** RPM/TPM 限速与 11.2 已实现的**总量配额**是两回事。
> `access_keys` 的 `rpm_limit` / `tpm_limit` 列曾作为占位在 2026-09-24 摘除
> （当时无执行点），现按当初承诺的「加列 + 加写入口 + 加执行点」三件一起落地。

| 维度 | 实现 |
|---|---|
| RPM | 内存固定窗口计数器（每分钟一个桶，`internal/ratelimit`），Key 维度；**被拒的请求同样计数** |
| TPM | 同桶按 token 计数：请求前按 `estimateRequestTokens` 预占（输入估算 + max_tokens 全额），请求终结时按真实 usage 校正（`CommitTPM`）；被拒的请求不预占 |
| 执行点 | `handleIngress` 骨架层（鉴权后 RPM、解码后 TPM），两个入口共享；额度随 `KeySnapshot` 从快照下发（0 = 不限），热路径不查库 |
| 拒绝响应 | 429 `rate_limit_exceeded` + `Retry-After`（窗口剩余秒数，两个协议同语义、各按自己的错误形状） |
| 窗口实现 | 单锁守护的 map（key 数量由管理员管理、有界，不需清扫）；不用 `golang.org/x/time/rate`（避免依赖） |
| 重启 | 计数归零，接受（DESIGN 既定取舍） |
| 校正跨窗 | 请求跨窗口边界时，TPM 校正差值落进新窗口 —— 误差为单个请求量级，接受 |

**另有一个已实现、不要与本节混淆的限速**：**管理后台登录失败限速**（`internal/server`）。
按来源 IP 记连续鉴权失败，10 次即进 60 秒冷却，冷却期内连 PBKDF2 都不做（省 CPU）。
存在的理由：管理密码下限为 8 字符 + 两类字符（2026-10-04 从 6 位上调，只约束新设
密码），PBKDF2 21 万迭代把单次尝试压到几十毫秒（交互无感），
但**并发下的弱口令依然可爆破** —— 这是唯一的在线防线。

- 来源 IP 取 `r.RemoteAddr`，**刻意不采信 `X-Forwarded-For`**（可伪造，采信等于把限速开关交给攻击者）。
- 判定「是否处于冷却期」必须用 `until.IsZero()`，**不能写 `!time.Now().Before(e.until)`** ——
  未冷却过的条目 `until` 是零值，而任意时刻都「不在零值之前」，那个条件对新条目恒为真，
  于是每次失败都重建条目、计数被清回 1，限速静默失效
  （2026-09-24 实测发现并修复：连打 26 次错误密码全是 401）。
  回归测试见 `internal/server/throttle_test.go`。

### 11.5 配额维度

两级终身配额均已实现：**key 级**（§11.2，原子预占）与**用户级**（`users.quota_tokens`，
总闸，热路径按实时 SUM 校验、仅额度 >0 时查库，设计论证见 MULTIUSER.md §4.3）。
per-model 配额、per-provider 配额仍不做，记入后续路线。

---

## 12. 配置

### 12.1 唯一事实来源（D4）

| 配置类别 | 存放位置 | 可否运行时改 |
|---|---|---|
| Provider / 模型 / Route / Key / **用户与管理员账号** | **数据库** | 是（管理 API / Web 界面） |
| 监听地址、DB 路径、日志级别、加密主密钥、全局默认超时与重试、body 大小上限 | **配置文件** | 否（改后重启） |
| 首次 bootstrap 的 provider/route | 配置文件（仅当 DB 为空时生效） | 否 |

**关键纪律**：DB 非空时，配置文件里的 `providers` / `routes` 段落被**忽略并打印 warning**。这防止出现「界面改完、重启被配置文件覆盖」这类经典事故。

**管理员账号为什么在数据库里**（2026-10-06 统一认证改造的决策）：管理面收敛到
users 表 + 会话一条通道后，管理员只是一个 `role='admin'` 的普通用户，
密码哈希（PBKDF2）自然随账号落库。**代价是「删库 = 失明」**：库没了，
管理员账号与密码一起没了 —— 但这也意味着首次初始化窗口重新打开，
重启后登录页会再次出现「首次设置密码」表单，重建的第一个管理员就是新的你。
这一取舍是明确接受的；真正不可重建的身份状态只有两把密钥（见 §12.1.1），
它们仍然独立于数据库存放。

**为什么管理凭据不再有 `admin_auth.json` / `admin_token` 旁路**：双通道时代
「界面密码」与「运维令牌」并存，判定逻辑互相污染 —— 早期实现被迫用
`len(token) == 64` 去猜「这串到底是明文还是哈希」，一个恰好 64 字符的明文令牌
就会被误判成哈希而**永久锁死**；而且旁路意味着绕开 users 表的状态、角色与改密审计。
统一后只有一种凭据、一种失效路径，401 只有一个含义。

### 12.1.1 数据目录布局

所有相对路径默认按**可执行文件所在目录**解析（与进程 CWD 无关）；设了
`ROSETTA_GW_HOME` 则整体重定向到该目录（容器部署就是这么把状态指到挂载卷的）。
部署形态是「一个目录装下全部状态」：

```
<home>/
├── gateway.exe          # 单个二进制（前端已 embed）
├── config.json          # 手写引导配置，程序不回写
├── master.key           # 上游凭据加密主密钥（缺失时自动生成）
├── session_secret       # 会话签名密钥（缺失时自动生成并原子落盘）
└── data/
    └── gateway.db       # SQLite：provider/模型/路由/key/用户/用量记录
```

删掉 `data/` 等于重置全部业务数据（管理员账号随之消失，重启后重新走首次初始化）；
删掉 `master.key` 等于上游凭据全部作废；删掉 `session_secret` 只影响登录态
（所有人被登出，数据无损）。

**主密钥的解析顺序**（`crypto.LoadMasterKey`）：

1. 环境变量 `master_key_env`（缺省 `ROSETTA_GW_MASTER_KEY`）
2. `<home>/master.key` 文件
3. 都没有 → **生成一个写入 `<home>/master.key`** 并复用（启动日志给出路径）

**会话密钥的解析顺序**（`userauth.NewManager`，与主密钥同构）：

1. 环境变量 `ROSETTA_GW_SESSION_SECRET`
2. `<home>/session_secret` 文件
3. 都没有 → **生成一个并原子落盘**（先写 `.tmp` 再 rename；启动日志说明来源）

两个「都没有」分支是刻意设计：早期实现只认环境变量 —— 主密钥那边，`bin/master.key`
的自动生成活在 `gateway.ps1` 里，结果**用脚本启动有密钥、双击 exe 启动没有**，后者会把
上游 API Key **明文**写进 `data/gateway.db`；会话密钥这边，没配就起不来，或者更糟 ——
静默用临时密钥，重启后所有人被登出。自动落盘让两条密钥都「零配置可用」，
代价是运维必须把 `<home>` 当状态目录持久化（容器要挂载卷）。
注意：**空文件视为故障**（长度不足会被拒），不会静默重新生成 ——
静默换密钥等于悄悄作废所有会话/解不开旧凭据，宁可让网关起不来并报错。

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
  "listen": "127.0.0.1:8666",
  "db_path": "./data/gateway.db",
  "log_level": "info",
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

**关于 `listen`**：默认（含程序自动生成的配置）是 `127.0.0.1:8666` —— 端口与
Docker 侧对齐，且避开浏览器的保留端口表（6666 是 IRC 段，浏览器会直接
`ERR_UNSAFE_PORT`，黑名单见 `internal/config/ports.go`，完整讨论见 DOCKER.md）。
网关对外提供 `/v1` 是常态，但**首次启动时第一个管理员的密码还没设**，
此时绑 `0.0.0.0` 等于把「抢先完成首次设置密码」的权利交给局域网里第一个访问 `/admin/` 的人。
要对外服务就显式改成 `0.0.0.0:<port>` —— 启动日志会打印实际监听地址，
并在这种「未初始化 + 非回环」状态下打 ERROR 告警。

**关于管理员凭据**：配置文件里没有任何管理员字段 —— 管理面走 users 表 + 会话
（§6.3）。首次打开 `/admin/` 会出现「首次设置密码」表单，设完即进入后台；
此后再无免鉴权窗口。

---

## 13. Web 管理界面（D10）

### 13.1 形态

`go:embed` 打包静态资源（`internal/webui/dist`），`web/` 下是 **Vue 3 + Vite + TS** 工程，`fetch` 调 `/admin/api/*`。

> 历史沿革：早期是单个 `index.html` 内联全部逻辑，理由写的是「内网管理页不超过 8 个，引入框架收益不成比例」，并预设了升级边界「一旦出现多页 + 复杂表单联动 + 图表就换框架」。边界随后真的被触发了（六页 + 表单弹窗 + 图表），于是按当初的约定迁到 Vue 3。构建链：`cd web && npm run build && npm run sync`（`sync` 把产物同步进 `internal/webui/dist` 供 embed），`gateway.ps1` 已编排。
>
> **CI 会校验产物同步**（`.github/workflows/ci.yml`，2026-10-02 起）：push/PR 时重新
> `npm build + sync` 一遍，与入库的 `internal/webui/dist` 做 `git diff --exit-code`，
> 不一致即红灯 —— 把「改了 web/src 忘 sync 就打 tag、旧 UI 被静默发出去」从线上事故
> 变成一次 CI 失败。本地二进制版本号由 `build.ps1` 从 `web/package.json` 读取并经
> `-ldflags` 注入 `main.buildVersion`，与 CI/前端页脚同源。

**鉴权**：管理 API 由 `adminGuarded` 鉴权链保护，凭据逻辑在 `internal/userauth` +
`internal/admin`（users 表 + JWT 会话，见 §6.3）。**恰好 4 条路由免鉴权**，且必须显式
注册到根 mux（`cmd/gateway/main.go`）—— 它们不进 `adminAuto`（那会走鉴权链），但也不能
因为「没注册」而落到 `/admin/api/` 前缀上被鉴权拦掉，那样会得到 401 而非功能缺失，
症状是「登录页一直转圈」：

| 路由 | 用途 |
|---|---|
| `POST /admin/api/login` | 提交账号密码换 JWT |
| `GET /admin/api/session` | 探测是否已登录（前端启动时决定去登录页还是主页） |
| `GET /admin/api/bootstrap` | 查询是否需要引导（未初始化 → 前端弹「设置密码」） |
| `POST /admin/api/bootstrap` | 设初始管理员密码，**仅在系统尚无任何凭据时开放** |

> **历史沿革（2026-10-06 已整体删除）**：统一认证之前是「`admin_auth.json` 密码文件 +
> `config.json` 的 `admin_token`」双通道，前端存的是**管理密码**而不是令牌，靠三个
> 端点驱动：`GET /admin/api/password/check`（决定弹「设置密码」还是「输入密码」）、
> `POST /admin/api/password/set`（一次性引导窗口）、`GET /admin/api/auth/verify`
>（保存前先验证）。**这三条路由现已不存在**，职责分别由上表的 `/session`、`/bootstrap`、
> `/me` 接管。前端那条「绝不把输入存进 localStorage 就刷新」的硬规则也随通道一起作废 ——
> 旧规则存在的原因是密码一错就会被 401 弹回同一个 `dismissable=false` 对话框，
> 用户被永久困在「输入密码 → 又要求输入」循环里（全局审计时代记录过的真实故障：密码一错就被困在循环里）；
> 现在 localStorage 里是 JWT（`rosetta_gw_admin_token`），验证由服务端完成，
> 登录失败走正常的错误提示而非困住对话框。


### 13.2 页面清单

实际路由（`web/src/router.ts`，2026-10-06 核实）共 **11 页**，其中 5 页
`adminOnly` —— 未登录或非 admin 看不到也进不去：

| 路由 | 页签 | 可见 | 内容 |
|---|---|---|---|
| `/login` | 登录 | 公开 | 账号密码换 JWT；未初始化时改走引导设密 |
| `/` | 总览 | 全体 | 今日请求数 / token / 错误率 / 各 provider 健康灯 |
| `/keys` | 访问密钥 | 全体 | 列表（按身份收窄）、启停、配额/有效期/IP/模型白名单编辑（普通用户只能收紧）、重算用量；**新建对全体开放**（明文只显示一次，归属恒为创建者自己；普通用户额度封顶到用户级配额、限速清零，分组覆盖仍限管理员） |
| `/history` | 调用历史 | 全体 | 按时间倒序的调用明细，含 usage_state 与耗时 |
| `/profile` | 我的账号 | 全体 | 自改密码、看自己的配额、**余额**（`balance_unlimited` 时显示「不限」而非 0.00 元）与用量；余额为 0 时显式提示「余额已用尽，请联系管理员充值」，对应数据面真实的 402 |
| `/models` | 可用模型 | 全体 | **只读**清单：`GET /admin/api/model-names` 按身份收窄后的模型名（普通用户 = 所属组白名单 ∩ 全部路由名）+ 输入/输出/缓存命中三项单价；`restricted=true` 时显式提示「已被分组收窄」。刻意不给勾选控件 —— 白名单的写入口在 `/keys`（只能收紧）与 `/groups`（管理员按组收紧），且**空清单一律解释为「一个模型都调不了」而非 ModelPicker 的「一个都不选 = 不限制」**（后者只属于配置态）。未配置的单价显示「未配置」并弱化，**不显示 0.00**（会被读成「确认免费」） |
| `/users` | 用户 | admin | 用户 CRUD、角色、启停、密码重置、**余额充值**（`delta_cents` 相对调整：填 100 是「加 100 元」而非「设为 100 元」，误操作不会清零；给不限额用户充值会把它切成有限额，界面上必须写明）；admin 账号必须带初始密码。余额列同样区分「不限」与「0.00 元」 |
| `/groups` | 分组 | admin | 分组配额与模型白名单；key 级覆盖被 SET NULL 后白名单会静默放宽（P2-19） |
| `/providers` | 上游与模型 | admin | 列表（协议/端点/凭证健康）、新建/编辑、连通性测试、模型管理、导入导出 |
| `/routes` | 路由 | admin | 虚拟名 ↔ (provider, model) 映射表、启停、故障转移链 |
| `/settings` | 设置 | admin | 版本、DB 大小、重建快照、日志级别、审计日志、配置导入导出 |

「用量看板」不是独立页 —— 按天 / 按 Key / 按模型 / 按 provider 的 token 趋势以
分组切换的形式内嵌在 `/history`。设置页的导入导出即 §13.3 描述的功能。

> **`/model-names` 加价格为什么是「平行数组」而不是把 `models` 改成对象数组**
>
> `/models` 页要显示单价，于是该端点的响应加了 `prices`。但它有**三个**调用方，
> 其中两个（`/keys` 的 ModelPicker、`/groups`）只要模型名：ModelPicker 的选项是
> `string[]`（`v-model` + `Set` 去重 + `includes` 判定），Groups.vue 同样。改成
> `[{name, …}]` 会同时打破这两处，而它们并不需要价格 —— 为一个页面的展示去改
> 两个组件的数据契约，收益与风险不成比例。
>
> 所以 `models` **保持 `string[]` 不变**（旧调用方零改动），价格以
> `prices[i]` 对应 `models[i]` 的方式并行给出，顺序与长度由后端保证
> （`group_handler_test.go` 有测试钉住，含「交集为空时 prices 也必须为空」）。
> 风险是平行数组的长度必须对齐，因此它在同一个循环里构造、排序时同步重排。

> 布局在 2026-10-06 改为**侧边栏**（`329a7ca`，参考 hirezo），此前是顶部标签页。

### 13.3 配置导入导出（2026-10-06 新增，2026-10-07 补数值校验）

`POST /admin/api/config-export/{export,import}`，入口在设置页。**只做供应商 +
模型**两项：路由与故障转移链是本部署的组织结构（公开名是给调用方看的契约，
跨环境照抄会撞名），访问密钥与分组根本不该离开这个库。

- **导出必加密、必带凭据**（2026-10-07 收敛，此前是「明文默认不带凭据 + 勾选带凭据 +
  口令可选」的四种组合）：凭据是这份文件里唯一「泄了就能花钱」的东西，明文带凭据的
  文件又恰恰是最容易顺手导出的那种；且单一产物格式让导入侧不用兼容两套形态。
  无口令请求一律 400，产物统一是 `.json.enc`；导入对**旧版明文文件**仍兼容。
- **导入语义是「只增不改」**：已存在的 slug 原样保留，新来的改名加 `-2`/`-3` 后缀。
  覆盖要先删、删错了就没了；加后缀最坏只是库里多一个重复供应商，用户自己能看出来。
- 导入**必须先干跑**（`dry_run`），界面先展示预览再确认写入。
- 导出响应是文件下载（`Content-Disposition`），前端走独立的 `downloadFile()` 而非
  通用 `request<T>` —— 后者会 `JSON.parse` 一整个配置清单。
- **导入与手工创建同一套校验**（2026-10-07）：超时/重试/上下文窗口/单价等数值字段
  不再绕过 provider/model handler 的非负与上界校验，违例跳过并进 warnings；
  凭据的 `enabled` 跟随导出文件（旧版文件缺该字段按启用处理），
  不再硬编码复活为启用。
- 两个端点都在 handler 内 `requireAdmin`：路径虽在 `/admin/api/` 前缀下（白名单管不到），
  但导出体可能含全部上游凭据，绝不能落到普通用户手里。

配套的**跨版本数据迁移**是一次性工具 `cmd/migrate-legacy`（把 v1.4.1 旧库的
provider / 模型 / 路由搬进当前版本空库），不能简单复制 db 文件 —— master.key 不同，
凭据是 AES-256-GCM 加密的（密钥 = sha256(master.key 文件内容)），直接搬密文会导致
全部 provider 解不开凭据、表现为 `/v1` 全站 404 且无任何告警；且 v1.4.1 → 当前版之间
`routes`/`access_keys` 有过 ALTER，列顺序不同，故按列名显式 INSERT。

### 13.4 安全

- 管理面只有 users 表 + 会话一条通道（§6.3），不做「内网免鉴权」的假设；
  首次初始化窗口只对未设密码的 admin 开启，且启动时对「非回环监听 + 未初始化」打 ERROR
- 上游 key 在界面只显示掩码（`sk-...abcd`），明文不可回读
- 所有写操作记审计日志（**已实现，2026-10-04**）：`server.AutoReload` 在每个管理写
  操作上落 `audit_log`（谁=admin+来源 IP、何时、哪个资源、**哪些字段名**）。
  字段名刻意不记值 —— 请求体里有上游 api_key 与管理密码明文。查询走
  `GET /admin/api/audit`，「设置 → 审计日志」页签展示

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
| 前端 | **Vue 3 + Vite + TS**，`go:embed` 打包 | 原定原生 HTML + JS，理由是「内网管理页不超过 8 个，引入框架收益不成比例」，并预设升级边界「一旦出现多页 + 复杂表单联动 + 图表就换框架」。**该边界后来真的被触发了**（六页 + 表单弹窗 + 图表），于是按当初约定迁到 Vue 3（见 §13.1） |
| 上游调用 | `github.com/cn-maul/rosetta` | 本项目存在的理由 |
| 部署 | 单个二进制 + 一个 SQLite 文件 + 一个配置文件 | `scp` 过去就能跑 |

---

## 15. 目录结构

> **2026-10-06 按实际仓库核实重写。** 原版本列的是 P1 时期的规划结构，与代码已严重脱节：
> `quota/` 包从未存在（配额逻辑最终落在 `cmd/gateway/main.go` 的请求热路径里）、
> `examples/curl.md` 从未创建、`inwire`/`outwire` 下标注「P3」的三个文件早已实现、
> 缺了后来新增的 `userauth` 与 `ratelimit`。

```
rosetta-gateway/
├── README.md                       项目门面：简介、快速开始、文档索引
├── DESIGN.md                       本文件
├── MULTIUSER.md                    多用户 / 分组 / 权限模型（§6.3 的展开）
├── DOCKER.md                       容器化部署
├── config.example.json             配置样例（config.json 本身不入库）
├── cmd/
│   ├── gateway/                    装配与启动；**配额预占、限速、故障转移链、usage 记账
│   │                               都在这里**（请求热路径，非独立包）
│   └── migrate-legacy/             一次性工具：v1.4.1 旧库 → 当前版本（见 §13.3）
├── internal/
│   ├── config/                     启动配置加载、校验、端口占用检查
│   ├── store/                      SQLite 连接、迁移、各表 DAO、归档与对账
│   ├── snapshot/                   运行时快照：路由索引、Provider 池，atomic.Pointer 热替换
│   ├── routing/                    模型名解析（§5）
│   ├── upstream/                   凭证池、健康与冷却、Client 生命周期、故障转移
│   ├── ratelimit/                  RPM / TPM 固定窗口限速器
│   ├── auth/                       下游 Key 校验
│   ├── userauth/                   登录、PBKDF2、会话密钥、JWT 签发
│   ├── inwire/                     下游 → 统一模型（openai_chat / openai_responses / anthropic
│   │                               + 跨协议字段翻译与硬约束判定）
│   ├── outwire/                    统一模型 → 下游（响应 + SSE + 错误映射 §9）
│   ├── admin/                      管理 API handlers（含配置导入导出）
│   ├── crypto/                     上游 key 加解密（AES-256-GCM）
│   ├── webui/                      embed 静态资源（dist 由 web/ 构建同步而来）
│   └── server/                     HTTP 装配、中间件、请求 ID、访问日志
└── web/                            Vue 3 + Vite + TS 工程（npm run build && npm run sync）
```

---

## 16. 里程碑

> **P0–P3 已全部交付**（2026-10 上旬完成）。以下保留原始验收标准作为回归参照。

### P0 — 打通链路 ✅

**做**：`cmd/gateway` 骨架；启动配置（JSON）；Provider 抽象与 `rosetta.Client` 构建；路由解析（§5 轨道一 + 二）；OpenAI Chat 入口的解码/编码/SSE；错误映射（§9）；看门狗、心跳、取消传播；静态 bootstrap 配置（暂不建 DB）。

**不做**：数据库、管理 API、Web 界面、配额、限速、凭证池、故障转移、Anthropic/Responses 入口。

**验收**：
1. Cherry Studio 把 base_url 指向 `http://<局域网IP>:8080/v1`，流式输出正常、非流式正常
2. `model` 同时支持虚拟名与 `slug/model` 两种写法
3. 下游 Ctrl+C 中断后，**网关日志显示上游请求已取消，上游连接确实断开**（用一个记录 ctx 的假上游验证）
4. 上游返回 401 时，下游收到的是 502 `upstream_auth_error`，不是 401
5. 上游卡住不吐字节，60s 后看门狗关闭流，下游收到断流

### P1 — 管起来 ✅

**做**：SQLite + 迁移；Provider / 模型 / Route / Key 的 CRUD 管理 API；管理鉴权；内存快照热更新；Web 界面（Providers、模型、Routes、Keys、用量总览、系统页）；连通性测试。

**验收**：
1. 浏览器里加一个 provider、加模型、建 route、发一把 key，**全程不改配置文件、不重启**
2. 改完立刻生效（新建的 route 立即能被下游调用）
3. DB 非空时，配置文件里的 bootstrap 段落被忽略并打 warning
4. 上游 key 在界面不可回读明文

### P2 — 管住量 ✅

**做**：`usage_records` 落库；配额检查与同步扣减；RPM/TPM 限速；用量看板；凭证池（多 key、加权、冷却）；**自动故障转移**——最终落地形态是 `route_targets` 有序上游链（取代原设想的 `fallback_route_id` 单跳兜底），每条路由只有一个 `failover_enabled` 开关，策略参数是全局的（「设置」页 → `app_settings.runtime_defaults`），详见 §10。

**验收**：
1. 给 Key 设 10k token 配额，跑超后返回 429
2. 同一 provider 配 2 把 key，其中一把返回 401 后自动切另一把，且该 key 进入冷却
3. 并发 50 个请求，`SUM(usage_records.total_tokens)` 与 `used_tokens` 一致，不漏记
4. 一条 route 挂 2 个上游目标、链首返回 5xx 时，自动在写出任何字节前转移到链上次个目标 ✅（`cmd/gateway/failover_test.go` 覆盖）

### P3 — 补协议 ✅

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
| R4 | **Anthropic thinking block 带 `signature`，跨协议转换会失效** | 下游 Anthropic + 上游非 Anthropic 时，多轮回传 thinking 会 400 | **已解决（2026-10-02）**：thinking（含签名）在统一模型里原生表达（`Block.Thinking/Signature`），Anthropic 上游完整回放；OpenAI 系上游由 SDK 适配器剥除历史 thinking 块。无需开关 |
| R5 | 流式断流无法回滚 | 下游可能收到半截回答 | 约定：不发终止事件，直接断连（§8.2） |
| R6 | 流式配额只能事后记账 | 预占 + 退预占把并发超发封死（§11.2）；残余误差仅剩 est 与真实用量的估算差 | 预占含 max_tokens 全额，宁可先多占后退 |
| R7 | 多模态 base64 让请求体很大 | 内存与 body 限制 | `max_request_body_bytes` 默认 32 MiB；注意 Rosetta chat 的 1 MiB 限制是**响应**侧，不冲突 |
| R8 | 每凭证一个 `rosetta.Client` ⇒ 连接池随 key 数增长 | 上百把 key 时资源偏高 | 共享 `WithHTTPClient` 的 Transport，或后续向 Rosetta 提 per-request 覆盖 |
| R9 | 上游 usage 缺失时无法区分「真报 0」与「没报」 | 配额失效、账单漏账且不可见 | **已改为两态显式区分**（2026-10-06）：`usageStateFor()` 按 SDK `Usage.IsZero()` 判「上游一个 token 数都没给」→ 记 `missing`，否则 `reported`。关键在于旧实现按「0 token + reported」记账，接不回 usage 的第三方兼容服务等于整 provider 静默漏账、配额形同虚设，且事后无法从库里分辨。`missing` 在界面单列统计，TPM 保留预占量不回滚。注意 `IsZero` 把 cached/reasoning 也计入 —— 只报缓存命中或思考 token 仍算「报了」，否则会丢掉真实数字 |
| R10 | 同类成熟产品（one-api / new-api / LiteLLM / Portkey / Higress）功能面重合 | 自研投入产出比 | 自研的唯一正当理由是「Rosetta 作内核」与「深度定制」。已确认走自研 |

---

## 附录 A：Rosetta 能力矩阵（网关视角）

| 网关需求 | Rosetta | 说明 |
|---|---|---|
| 三协议上游调用 | ✅ | `Client.Chat` / `ChatStream` |
| Embed / Rerank 上游 | ✅ | `Client.Embed` / `Rerank` |
| 统一 usage | ✅ | `Usage` + `UsageTracker` 接口（网关实现它落库） |
| 上游重试 / 退避 | ⚠️ | SDK v1.0.0 起对话 POST 默认带传输层重试（标 `RetryIdempotent`，429/503 与传输错误都会重发同一请求）。网关**必须**传 `WithQuirks(NoIdempotencyKey: true)` 把它关掉（`buildClient` 已传）——否则非流式慢生成在 `ResponseHeaderTimeout` 处被掐后原地重发，上游重复生成重复计费。关闭后 POST 的失败一律立即上抛，重试职责由本网关的故障转移链承担（`route_targets`）。含 `Retry-After`（60s 硬上限）。 |
| 上游凭证 | ✅ | 每 Client 一套，网关按 provider × credential 建实例 |
| 流式拉取 | ✅ | `Stream.Next()`，逐事件转下游 SSE 很顺 |
| 流截断识别 | ✅ | `ErrStreamTruncated` |
| 脏数据容错 | ✅ | Rosetta 侧宽松解码；网关管理 API 另用 `internal/admin` 的 `decodeJSON`（断言 Content-Type） |
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

## 附录 C：已知取舍与边界（有意接受，非遗留缺陷）

> 本清单是**正式的、当前有效的**取舍记录（2026-10-07 收编自历轮审计的「遗留」小节）。
> 收录标准：复核确认影响有界、修复代价大于收益、或有明确的其他兜底。
> 新一轮审计复核后应更新本表，而不是让取舍散落在历史报告里。

| 取舍 | 说明 | 影响与兜底 |
|---|---|---|
| 全链路无 TLS | 定位是局域网网关，设计上不做传输加密 | 对外暴露由部署层解决（反向代理终结 TLS，见 DOCKER.md） |
| 删库 = 失明 | 身份、配额、路由、key 全部绑在同一个 SQLite 文件上（§12.1） | 不可重建的身份状态只有 `master.key` 与 `session_secret` 两把密钥，独立于库存放；删库后重新走首次初始化 |
| `audit.actor` 恒为 "admin" | `WriteAuditor` 回调未透传会话身份，普通用户对自己资源的写操作也落 "admin" 记录 | 审计无法区分操作者；修法需改回调签名波及全部调用点，单独立项 |
| 配额预占 est 是预估值 | 预占含 `max_tokens` 全额，请求收尾按真实 usage 退预占；上游不回 usage 时退预占、漏账以 `usage_state="missing"` 显形（§11.1/§11.2） | 在途瞬间不精确，终态正确；漏账可从界面察觉 |
| 归档明细的可见性边界 | 明细只保留 30 天，更老区间的分组统计从日归档表取，按「宁可少算」口径（§4） | 极老日期的部分维度不可查；30 天内可按 `request_id` 查明细 |
| `-race` 本机不可用 | Windows 开发机无 gcc | 并发修复靠人工审查 + 反向验证测试，CI 环境兜底 |
