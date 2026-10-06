# 多用户改造设计文档

| 项    | 值                                                                       |
| ---- | ----------------------------------------------------------------------- |
| 项目   | **rosetta-gateway**                                                     |
| 文档性质 | 多用户能力改造设计（v1 草案）                                                        |
| 日期   | 2026-10-05                                                              |
| 触发场景 | 已部署到公司局域网，**已有外部用户接入**                                                  |
| 状态   | **主体已实现**（users/分组/配额/归档/会话均落地）。其中与旧双通道鉴权相关的段落（§1 现状、§3.5、§4.1 去留、§5 引导）**已被 2026-10-06 的统一认证改造推翻**——admin_token / admin_auth.json 两条旁路整体删除，详见 §3.5 顶部的废弃说明与 `DESIGN.md` §6.3 |
| 参考   | new-api（`QuantumNous/new-api`，AGPLv3）、cc-switch（`farion1231/cc-switch`） |
| 参考方式 | **源码通读**，非文档推测。关键结论均标注源文件                                               |

---

## 0. 结论先行

**必须做的四件事**，按优先级：

1. **加 `users` 表**，把 `access_keys` 挂到用户名下（`user_id` 外键）。  
   这是唯一的结构性改动 —— 其余全是它的推论。
2. **鉴权链从「单密码」升级为「用户身份 + 角色」**，  
   引入用户级会话（JWT），让每个用户能在**自己的账号里**看用量、管自己的 key。
3. **引入 `groups` 分组表 + `user_group_models` 关联**，  
   让管理员能批量把「一组模型权限」发给一批人，而不用一把 key 一把key 手点。
4. **用量归档：明细保留 30 天，累计总额永久正确**（§4.8）。  
   这条是**容量刚需**，不是功能需求：现有 8 个统计查询全部打在明细表上，
   用户量上去后最先崩的是它，**表现是网关整体卡死而不是看板变慢**。  
   实现上是**两张表**：`usage_daily_rollups`（按日归档）+ `usage_totals`
   （终身累计单行表，触发器实时累加，**与剪枝完全解耦**）——
   剪枝只动明细与归档表，累计值永不回退。

**明确不要做的**：不要上 Casbin。new-api 自己都已经从 Casbin 收敛回  
「角色 + 静态授权矩阵」（见 §2.3），局域网网关的权限复杂度远低于它，  
引入 Casbin 只会增加依赖与调试成本。也不要上 Redis —— 理由见 §6.1。

**对现有代码的冲击面**：约 14 个文件、1600 行改动。热路径（`auth.Authenticate`）  
改动不超过 25 行 —— 因为快照（`snapshot`）机制已经把「管理写 → 热路径生效」  
这件事做好了，users 只是往里多加一个 map。

**参考来源占比**：new-api 约 90%（身份/权限/配额），  
cc-switch 约 10%（数据工程：明细+归档、reload 互斥、SSOT）。详见 §2.7。

---

## 1. 现状盘点（改造的地基）

### 1.1 当前是什么形态

| 维度    | 现状                                          | 证据                                                                 |
| ----- | ------------------------------------------- | ------------------------------------------------------------------ |
| 身份    | **无用户概念**。管理面只有一个全局密码                       | `internal/adminauth/store.go`：单文件 `admin_auth.json` + 单个 PBKDF2 哈希 |
| 访问凭证  | `access_keys` 表**没有归属字段**，是「事实上的全局资源」       | `store.go` 建表语句无 `user_id`                                         |
| 配额    | **每 key 独立**：`quota_tokens` / `used_tokens` | `key_dao.go` + `trg_update_used_tokens` 触发器                        |
| 限速    | **每 key 独立**：rpm / tpm                      | `KeySnapshot.RPMLimit/TPMLimit`                                    |
| 用量归属  | `usage_records.access_key_id`，**无 user_id** | `store.go` 建表语句                                                    |
| 热路径鉴权 | 快照 O(1) 查 SHA-256 哈希表                       | `internal/auth/auth.go` + `snapshot.KeysByHash`                    |
| 热路径配额 | **每请求查库** `GetKeyQuota`（刻意不读快照，见代码注释）       | `cmd/gateway/main.go:893-902`                                      |
| 路由权限  | **无**。`public_name` 全局唯一，任何 key 可见任何模型      | `routes.public_name UNIQUE`                                        |

### 1.2 三个必须正视的既有约束

改造方案不能违反这三条，否则会破坏现有能力：

**约束 A：`used_tokens` 是数据库触发器维护的派生值。**  
`trg_update_used_tokens` 在每条 `usage_records` 插入后累加。  
加了用户配额层之后，如果用户级配额也想用触发器维护，  
就会出现「一把 key 的用量同时触发 key 级和 user 级两次累加」——  
**必须在设计时明确用户级配额是「查表实时 SUM」还是「独立计数器」**（见 §4.3）。

**约束 B：配额预检刻意 fail-open，且每请求查库。**  
`main.go:896` 的注释写得很清楚：预检失败只记日志不拒绝。  
这在单 key 场景是可接受的降级；**在多用户场景下会变成「一个用户的  
数据库抖动可以让他无限超发」**。多用户改造必须重新审视这条。

**约束 C：`adminauth` 刻意与数据库分离。**  
`adminauth/store.go` 的包注释明确写着：管理员密码是身份凭据，  
放进可被删库的 `gateway.db` 等于「删库 = 把自己锁在门外」。  
**用户表不能重蹈这条覆辙吗？** 不 —— 用户表与配额、路由数据强相关，  
删库重建时会一并消失。解决方案见 §3.5「bootstrap 管理员」。

---

## 2. new-api 的多用户设计（源码实证）

以下全部来自本地克隆的源码，位于 `C:/Users/louis/temp/ref/new-api`。

### 2.1 二级实体：User → Token

new-api 有**两层**凭证（`model/user.go:78`、`model/token.go:16`）：

```go
// model/user.go:78
type User struct {
    Username     string  `gorm:"unique;index" validate:"max=20"`
    Password     string  `validate:"min=8,max=128"`
    Role         int     `gorm:"type:int;default:1"`   // admin, common
    Status       int     `gorm:"type:int;default:1"`   // enabled, disabled
    Quota        int     // 总配额
    UsedQuota    int     // 已用
    RequestCount int
    Group        string  `gorm:"type:varchar(64);default:'default'"`  // ← 分组
    DisplayName  string
    AuthVersion  int64   // ← 见 §2.4
    DeletedAt    gorm.DeletedAt
}

// model/token.go:16
type Token struct {
    UserId             int
    Key                string `gorm:"type:varchar(128);uniqueIndex"`
    Status             int
    Name               string
    ExpiredTime        int64  // -1 = 永不过期
    RemainQuota        int
    UnlimitedQuota     bool
    ModelLimitsEnabled bool    // ← 单key 模型白名单
    ModelLimits        string  `gorm:"type:text"`
    AllowIps           *string // ← CIDR 白名单
    Group              string  // ← 覆盖用户分组
    AutoGroups         string
    DeletedAt          gorm.DeletedAt
}
```

**对我们的启示：**

| new-api 字段          | 我们的对应                             | 优先级           |
| ------------------- | --------------------------------- | ------------- |
| `User` + `Token` 二级 | `users` + `access_keys`           | P0            |
| `Token.Group` 覆盖    | `access_keys.group`               | P1            |
| `Token.ModelLimits` | `access_keys.allowed_models_json` | P1            |
| `Token.AllowIps`    | `access_keys.allowed_ips`         | P2            |
| `Token.ExpiredTime` | **已有但被摘除**（`dropDeadColumns`）     | P2            |
| `User.AuthVersion`  | `users.auth_version`              | **P0，见 §2.4** |

### 2.2 权限模型：Ability 表 = (group, model, channel) 三元组

这是 new-api 最值得抄的一点（`model/ability.go:20`）：

```go
type Ability struct {
    Group     string `gorm:"type:varchar(64);primaryKey;autoIncrement:false"`
    Model     string `gorm:"type:varchar(255);primaryKey;autoIncrement:false"`
    ChannelId int    `gorm:"primaryKey;autoIncrement:false;index"`
    Enabled   bool
    Priority  *int64
    Weight    uint
    Tag       *string
}
```

**权限判定链路**（`controller/model.go:156-178`，由 deepwiki 索引与源码交叉确认）：

```
用户分组（token.Group 为空则取 User.Group）
    ↓
abilities 表里筛 (group, model) → 可见模型集合
    ↓
命中 channel_id → 选上游
```

Token 级模型白名单**优先级高于分组**：`if ContextKeyTokenModelLimitEnabled
则忽略 group 权限，只返回 limit map 里的模型`。  
这个优先级顺序要抄——**限制必须能收得更紧，授予不能更松**。

**与我们的差异**：new-api 的 `Ability` 把「可见性」和「路由」合成了一张表，  
因此它的 group 天然绑定 channel。我们没有这个约束——  
我们的 `routes` 是管理员配置的全局资源，**不该被 group 绑定**。  
所以我们应该拆成两张表（见 §3.3），而不是照抄 Ability。

### 2.3 权限判定：new-api 已经从 Casbin 退回到静态矩阵

`service/authz/` 的目录结构暴露了一个重要的**负面经验**：

```
service/authz/
├── adapter.go      casbin GORM adapter
├── assignment.go
├── enforcer.go
├── override.go
├── permission.go   ← Permission{Resource, Action}
├── registry.go     ← 静态资源注册表
├── resolver.go
├── role.go         ← 只有 root / admin 两个内置角色
└── seed.go
```

关键在 `service/authz/adapter.go` 的 `LoadPolicy`：

```go
if rule.V5 != "" || (rule.V4 != "" && rule.V4 != "all") {
    // 当前模型无法强制 own/other 作用域。保留存储的规则供审查，
    // 并在内存中把该权限**降级为拒绝**。
    effect = EffectDeny
    common.SysLog(fmt.Sprintf(
        "authorization policy %d has an unsupported legacy scope; "+
        "loaded as deny; ...", rule.Id))
}
```

**翻译成人话**：Casbin 规则里残留的 `own`/`other` 作用域它已经**不支持了**，  
于是一条条日志刷出来、全部降级为 deny。角色只剩 `root`/`admin` 两个，  
`root.Superuser = true` 直接放行一切（`role.go:22`）。

**这是从 Casbin 撤退的痕迹。** 我们的权限复杂度远低于 new-api  
（只有「管理员」和「普通用户」两类），引入 Casbin 是净负债。  
**决策：静态角色 + 简单资源操作矩阵，不引外部 RBAC 库。**

### 2.4 AuthVersion：最值得抄的一个机制

`model/user_auth_cache.go` 的文件头注释写得极清楚：

> User auth cache fencing uses three Redis keys per user: the cached user hash,  
> a short-lived **pending fence** published before a restrictive database  
> transaction, and a monotonic **committed version floor** published after  
> commit. Cache writes below either floor are rejected, readers below the  
> effective floor fall back to the database, and the pending fence outlives  
> every user-hash TTL so a **rolled-back transaction heals** without allowing a  
> stale snapshot to re-authorize the user.

翻译：**缓存与数据库之间用版本号栅栏**，解决「禁用了用户但缓存里还能用 60 秒」的经典问题。

三把钥匙：

- `user:{id}` — 用户缓存 hash，TTL 60s（`user_cache.go:54`）
- `auth:user:fence:{id}` — **预写栅栏**，在数据库事务*之前*发布
- `auth:user:version:{id}` — **已提交版本下限**，事务提交*之后*发布

读者发现自己的版本低于下限时，直接回落查库。

**我们的场景更简单**：我们**不用 Redis**，有现成的 `snapshot` 原子指针  
（`snapshot.Swap`，`internal/snapshot/snapshot.go:87`）。  
但问题一模一样：**禁用用户后，当前在途请求用的还是旧快照**。  
方案见 §4.4。

### 2.5 令牌校验：状态机而非布尔

`model/token.go:220` 的 `ValidateUserToken`：

```go
if token.Status == TokenStatusExhausted ||
   token.Status == TokenStatusExpired ||
   token.Status != TokenStatusEnabled {   // 注意：这行让前两行冗余了
   return token, ErrTokenInvalid
}
if token.ExpiredTime != -1 && token.ExpiredTime < common.GetTimestamp() {
   // 顺手把状态落库改成 expired
   return token, ErrTokenInvalid
}
if !token.UnlimitedQuota && token.RemainQuota <= 0 {
   token.Status = TokenStatusExhausted   // 顺手落库
   return token, ErrTokenInvalid
}
```

**值得学的两点**：

1. **拒绝时顺手更新状态落库** —— 惰性状态机，省掉定时任务。
2. `token.UnlimitedQuota` 是独立布尔位，而不是 `RemainQuota = -1` 之类的魔数。

**它的 bug**（`token.Status == X || token.Status == Y || token.Status != Enabled`  
在 X=Y=非 Enabled 时前两项恒真、最后一项恒真，整段等价于一句废话）——  
我们别抄这个。这正是我们的 `dropDeadColumns` 注释里说的「schema 与界面在说谎」的同类。

### 2.6 用户缓存 TTL 只有 60 秒，且**不带 Redis 直接失效**

```go
// model/user_cache.go:54
func userCacheTTLSeconds() int {
    ttl := common.RedisKeyCacheSeconds()
    if ttl <= 0 { return 60 }
    return ttl
}
```

而 `invalidateUserCache`：

```go
func invalidateUserCache(userId int) error {
    if !common.RedisEnabled { return nil }   // ← 没 Redis 就什么都不做
    return common.RedisDelKey(getUserCacheKey(userId))
}
```

**没开 Redis 时，禁用用户最长 60 秒才生效。** 它靠 TTL 兜底。  
我们没有这个问题（快照是进程内的，改完立刻 Swap），但反过来——  
**我们的问题比它更难**：进程重启后快照从库里重建，这个恰好是对的。

### 2.7 cc-switch 的参考价值：数据工程与并发一致性

cc-switch（`farion1231/cc-switch`）是**桌面端单用户**应用，
**身份与权限维度确实没有可借鉴的东西**——我通读了它的
`database/schema.rs` 全部 13 张表与整个 `proxy/` 模块，
确认没有任何 `users` 表、`user_id` 字段或认证中间件。

但它在**数据工程**上有三处直接命中我们的长期瓶颈，值得抄。

#### 2.7.1 明细 + 日聚合两层表（最重要）

`proxy_request_logs`（明细）→ `usage_daily_rollups`（按日聚合），
配合定期剪枝（`database/dao/usage_rollup.rs`）。

聚合 SQL 的核心是**幂等合并**，而非简单 INSERT：

```sql
INSERT OR REPLACE INTO usage_daily_rollups (date, ..., request_count, total_cost_usd, avg_latency_ms)
SELECT ...
    COALESCE(old.request_count, 0) + new_req,      -- 累加
    CASE WHEN COALESCE(old.request_count,0) + new_req > 0
         THEN (COALESCE(old.avg_latency_ms,0) * COALESCE(old.request_count,0)
               + new_lat * new_req)
              / (COALESCE(old.request_count,0) + new_req)   -- 延迟按请求数加权
         ELSE 0 END
FROM ( ...聚合待处理明细... ) agg
LEFT JOIN usage_daily_rollups old
    ON old.date = agg.d AND old.app_type = agg.a AND old.provider_id = agg.p
    AND old.model = agg.m AND old.request_model = agg.rm AND old.pricing_model = agg.pm
```

**`avg_latency_ms` 用请求数加权平均而非算术平均**——这是我们
`usage_dao.go` 里 `GetRecentThroughput` 已经在用但没推广的做法
（那里注释就说「用聚合口径…比逐条速率平均更稳」）。
聚合层要保持同一口径，否则长窗口延迟会被算错。

**三个必须抄的工程细节**：

1. **剪枝不可逆，先尽力回填再删**
   ```rust
   // 剪枝是不可逆的：明细一旦汇总删除，0 成本行就永远失去按 pricing_model
   // 补价重算的机会…所以剪枝前先尽力回填一次。失败仅告警不阻断——
   // 否则一行损坏的定价数据会永久卡死日志清理。
   ```
   我们的 `upstream_models.price_*` 是**可改的**，历史费用按「当前单价实时计算」
   （`usage_dao.go:GetUsageStats` 注释明确说了这点）。
   照此逻辑：**剪枝前必须先固化历史单价**，否则明细一删，
   改价后的历史费用永久算错。这点我们比 cc-switch 更脆弱——
   它有 `pricing_model` 维度专门固化「计价基准」，我们没有。

2. **切分点对齐到本地午夜，不能落在半天中间**
   ```rust
   /// Aligning to the next local midnight after `(now - retain_days)` guarantees
   /// that the youngest rollup row always represents a *complete* local day.
   /// Without this alignment the cutoff falls mid-day, leaving the day half-rolled-up
   /// and half-pruned — which would silently under-count any range query that touches that day.
   ```
   我们现有按日统计的 `date(ts/1000,'unixepoch')` 用的是 **UTC**
   （`usage_dao.go:SumTokensByDayForKey` 注释写明 `YYYY-MM-DD（UTC）`）。
   引入按日聚合时必须先定死**日界用本地还是 UTC**，两套口径混用会静默少算。

3. **`SAVEPOINT` 包住「聚合 + 剪枝」两步**（`usage_rollup.rs:89-112`）——
   聚合成功但剪枝失败（或反之）不留半成品状态。

#### 2.7.2 并发一致性：per-app 互斥锁

`proxy/switch_lock.rs`：

```rust
//! 确保同一应用同时只有一个切换、进入/退出代理的操作在执行，
//! 防止并发操作导致指针、代理路由和客户端文件不一致。

/// 每个应用类型一把互斥锁，保证同一应用的切换操作串行执行。
/// 不同应用之间可以并行切换。
```

**映射到我们**：现有 reload 路径是「先重建上游池 → 再 Swap 快照」
（`snapshot/rebuild.go` 包注释写明了这个分工）。
`Swap` 本身原子，但**「池已重建、快照未 Swap」的中间窗口里，
快照指向的 provider ID 可能已在池中消失**。
多用户阶段配置改动会变频繁（用户自己改 key 白名单），这个窗口需要收口。见 §4.7。

#### 2.7.3 SSOT 与「配置不撒谎」

**1）SSOT + 双向同步**（`src-tauri/src/provider.rs`）：

```rust
/// SSOT 模式：不再写供应商副本文件
pub struct Provider {
    pub id: String,
    pub settings_config: Value,    // ← 一切配置都在这里
    pub meta: Option<ProviderMeta>, // ← 元数据不写入 live 配置
}
```

切换时：把目标 provider 写进 live 文件，**再立刻读回来**更新 SSOT
（backfill），防止用户手改被覆盖丢失。对我们的启示见 §3.6。

**2）原子写 + 事务快照**：每次修改先做快照，失败回滚。

---

## 3. 目标数据模型

### 3.1 全景 ER

```
┌──────────────┐
│    users     │  身份主体
│──────────────││
│ id        PK │
│ username  UQ ││
│ password_hash││  ← 不存明文
│ role         ││  admin / user
│ status       ││  active / disabled
│ group_id  FK ││─→ groups
│ quota_tokens ││  用户级总配额（token 计量，与 access_keys 同口径）
│ used_tokens  ││  ← 触发器维护，见 §4.3
│ auth_version ││  ← 缓存栅栏，见 §4.4
│ created_at   ││
└──────┬───────┘│
       │        │
       │ 1:N    │
┌──────▼───────┐│┌──────────────┐
│ access_keys  │││   groups     │
│──────────────│││──────────────││
│ id       PK  │││ id       PK  ││
│ user_id   FK │││ name    UQ  ││
│ key_hash  UQ │││ description ││
│ key_prefix   │││ quota_tokens││ ← 组级共享总额度（可选）
│ name         │││ rpm_limit   ││ ← 组级限速兜底
│ enabled      │││ tpm_limit   ││
│ group_id FK ─┼─→│ created_at  ││
│ quota_tokens ││└──────┬───────┘│
│ used_tokens  ││       │        │
│ rpm/tpm_limit││       │        │
│ allowed_models_json (P1)│        │
│ allowed_ips       (P2)│        │
│ expires_at     (P2)  │        │
└──────┬───────┘│       │        │
       │        │       │        │
       │ 1:N    │       │        │
┌──────▼────────▼───────▼────────▼┐
│         user_group_models        │  (P1) 组 → 模型白名单
│──────────────────────────────────│
│ group_id    FK                   ││
│ public_model                      ││
└──────────────────────────────────┘

┌──────────────────────────────────┐
│       usage_records              │  加 user_id 列
│──────────────────────────────────│
│ id, ts, access_key_id,           │
│ user_id       ← 新增（冗余）      │
│ group_id      ← 新增（冗余）      │
│ public_model, provider_id, ...   │
└──────────────────────────────────┘
```

### 3.2 `users` 表 DDL


```sql
CREATE TABLE IF NOT EXISTS users (
    id            TEXT PRIMARY KEY,
    username      TEXT NOT NULL UNIQUE,
    display_name  TEXT,
    -- 密码哈希：PBKDF2-HMAC-SHA256，与 adminauth 参数一致（21万次迭代）
    password_hash TEXT NOT NULL,
    role          TEXT NOT NULL DEFAULT 'user',      -- admin / user
    status        TEXT NOT NULL DEFAULT 'active',   -- active / disabled
    group_id      TEXT REFERENCES groups(id) ON DELETE SET NULL,
    -- 用户级总额度（token 计量）。0 = 不限
    quota_tokens  INTEGER NOT NULL DEFAULT 0,
    used_tokens   INTEGER NOT NULL DEFAULT 0,
    -- 会话失效栅栏：改密/禁用/改角色时自增，见 §4.4
    auth_version  INTEGER NOT NULL DEFAULT 1,
    -- 备注，管理员可见
    remark        TEXT,
    created_at    INTEGER NOT NULL,
    updated_at    INTEGER NOT NULL,
    last_login_at INTEGER
);
CREATE INDEX IF NOT EXISTS idx_users_group ON users(group_id);
```

**为什么 `password_hash` 不是明文**：
沿用 `adminauth` 已有的 PBKDF2 参数（`kdfIterations = 210_000`）。
**不引入 bcrypt/argon2** —— 理由见 §6.1。

### 3.3 权限表：拆开，不抄 Ability

new-api 的 `Ability` 把可见性与路由绑在一张表（§2.2）。我们不能这么抄，
因为我们的 `routes` 是管理员显式配的资源，**把它绑到 group 会让
「改一个 group 的模型权限」意外影响路由拓扑**。

拆成两张：

```sql
-- 分组表
CREATE TABLE IF NOT EXISTS groups (
    id           TEXT PRIMARY KEY,
    name         TEXT NOT NULL UNIQUE,
    description  TEXT,
    -- 组级共享额度：0 = 不限。P2，先留字段不接线也可以（对齐 dropDeadColumns 的教训，
    -- 若要加就同时加执行点，否则不放）
    quota_tokens INTEGER NOT NULL DEFAULT 0,
    rpm_limit    INTEGER NOT NULL DEFAULT 0,
    tpm_limit    INTEGER NOT NULL DEFAULT 0,
    created_at   INTEGER NOT NULL
);

-- 组 → 模型白名单
CREATE TABLE IF NOT EXISTS user_group_models (
    group_id     TEXT NOT NULL REFERENCES groups(id) ON DELETE CASCADE,
    public_model TEXT NOT NULL,
    PRIMARY KEY (group_id, public_model)
);
```

**权限求交规则**（写死，不可配置，写进代码注释与文档）：

```
可访问模型 = ∩(
    group 的模型白名单（为空 = 不限制）,
    access_key.allowed_models（为空 = 不限制）
)
```

且 **key 级限制只能更紧** —— 与 new-api 的优先级一致（§2.2）。

**谁来判定**：`groups` 为空（未启用分组功能）时，全部 key 可见全部模型，
与当前行为完全一致 —— **这是 P1 的向后兼容保证**。

> ✅ **实现记录（2026-10-05，P1 已完成）**
>
> 设计之外还需要拍板的地方，全部记在这里，避免后人按「看起来很自然」的方式改回去。
>
> **1. `groups` 表刻意只有 id/name/description/时间戳，不带 `quota_tokens` /
> `rpm_limit` / `tpm_limit`。** 设计稿的建表语句里有这三列并注了「P2，先留字段」。
> 实现时把它们**去掉了**：本仓库已经为「留了列但没有执行点」付过两次代价
> （`dropDeadColumns` 摘掉的 `expires_at` / `routes.priority` 等），
> 界面上能配、文档里写着、实际完全不生效。要加组级额度就与
> 「列 + 管理写入口 + 热路径执行点」三件套一起加。
>
> **2. 白名单判定器 `snapshot.ModelAllow` 的零值是「拒绝全部」。**
> 快照重建漏填字段 → 该 key 什么都调不了（有界、立刻被发现）；
> 零值若是「不限制」→ 本该受限的 key 静默拿到全部模型。
> 要表达「不限制」必须显式写 `AllowAll()`。这是 P1 唯一一处
> 「故意让默认值偏向收紧」的设计。
>
> **3. `provider/model` 直连形式同样受白名单约束。**
> `routing.Resolve` 对直连形式会**合成**一条 `PublicName` 恰为请求原文的
> route，所以按解析后的 `PublicName` 判定天然覆盖它。若只对具名路由校验，
> 受限用户把请求改写成 `provider/model` 就能拿到白名单之外的上游模型 ——
> 整条权限链被一次改写绕过。端到端脚本第 21b 步锁这条。
>
> **4. 白名单之外的模型返回 `403 model_not_allowed`，而不是 404。**
> 与 §4.5「改他人的 key 回 404」不同：那里保护的是**他人的数据**
> （404 是为了不可枚举他人资源 id），这里保护的是**共享的配置边界**
> —— 说清「你的 key/组不允许这个模型」不泄露任何属于别人的东西，
> 却能把「客户端配置过期」这个最常见的真实原因一次讲明白。
> 但 `/v1/models` 列表**不会**列出白名单之外的名字（列表是「你能用什么」的
> 权威答案，与转发用同一套判定）。
>
> **5. 组里还有成员时拒绝删除（409 `ErrGroupNotEmpty`），不靠外键 SET NULL。**
> 白名单非空的组被删掉后，`ON DELETE SET NULL` 会让那批成员**从「受限」变成
> 「不受限」**—— 一次删除操作悄悄给一组人扩权，且没有任何人会发现。
> 外键上的 SET NULL 保留为直接改库时的安全网，但正常路径走「显式拒绝 +
> 带上人数与处置办法的错误信息」。
>
> **6. 库里 `allowed_models_json` 解析失败 → 拒绝全部模型（fail closed）。**
> 返回 error 会让 `RebuildFromDB` 整体失败 —— 而首建快照就在启动路径上，
> 等于「一个字段格式错误让整个网关起不来」。当成「不限制」则是静默放权。
> 拒绝全部的损失面只有那一把 key，且管理员在管理 API 上重新保存一次即可修好。
>
> **7. 白名单写入时校验模型名真实存在**（读快照里的公开模型名，与数据面同源）。
> 白名单是精确匹配，拼错一个字符的结果是「这个模型谁都看不到」而界面上毫无异常；
> 整组都拼错时表现为「组内用户全部 403」，排查要从权限链一路查到拼写。
> 在写入点挡成一条 400，把静默故障变成可读的错误。
>
> **8. 顺带接上了用户级配额**（§4.3 三级配额的最外层）。`users.quota_tokens`
> 在此之前只有展示、没有执行点 —— 又一处「界面能配但不生效」。
> 位置在 key 级预检之后；**只在额度 >0 时才查库**（不限额是常态）。

### 3.4 `access_keys` 的增量列

```sql
ALTER TABLE access_keys ADD COLUMN user_id       TEXT REFERENCES users(id) ON DELETE CASCADE;
ALTER TABLE access_keys ADD COLUMN group_id     TEXT REFERENCES groups(id) ON DELETE SET NULL;
-- P1
ALTER TABLE access_keys ADD COLUMN allowed_models_json TEXT;
-- P2
ALTER TABLE access_keys ADD COLUMN allowed_ips  TEXT;
```

**`user_id` 的可空性决策**：见 §5.1，这是整个迁移里最需要小心的一个点。

### 3.5 bootstrap 管理员：破解约束 C

> **❌ 本节方案已被推翻（2026-10-06 统一认证改造）。**
> 旧方案的核心是「保留 `admin_token` / `ADMIN_TOKEN` / `admin_auth.json` 作为
> 永远绕过数据库的应急旁路」。这条决策最终反向收敛：**旁路全部删除**。
> 理由：双通道的判定污染没有干净的解法（「这串是明文还是哈希」式的启发式
> 就是这么来的），且任何绕开 users 表的通道都同时绕开了用户状态、角色与改密审计。
>
> **最终方案（唯一通道）**：
> - 启动时 users 表没有**已设密码**的 admin → 自动建一个空密码 admin
>   （`cmd/gateway/bootstrap_admin.go`）；
> - 登录页探测到未初始化 → 渲染「首次设置密码」表单，
>   `POST /admin/api/bootstrap` 一步完成设密码 + 登录；
> - 忘记密码：由管理员重置；唯一的 admin 忘了 → 删库重建（重新走首次初始化）。
>   **「删库 = 失明」是明确接受的取舍**——需要永久保全的身份状态只有
>   `master.key` 与 `session_secret` 两把密钥，它们独立于库存放；
> - 免鉴权初始化窗口的暴露面与旧实现「无凭据时 `password/set` 免鉴权」完全相同：
>   默认回环监听 + 启动时对「非回环 + 未初始化」打 ERROR 告警。
>
> 以下原文仅作设计演进的历史记录保留；「实现警示」里主张的
> `WithAdminCredentials` 并行通道也已随统一认证删除。

约束 C 说「身份凭据不能放进可删的库」（§1.2）。但用户表必须进库。

**解法：分离「唯一不可删的那一份」与「可重建的那一份」。**

1. **bootstrap token 独立于数据库**。
   保留现有 `config.json` 的 `admin_token` 与 `ADMIN_TOKEN` 环境变量作为
   **唯一**的应急入口，永远绕过数据库。
   与 `adminauth.Store.fallback` 的现有语义完全一致，零新增概念。

2. **启动时若 `users` 表为空，自动从 `admin_auth.json` 的现有密码引导出一个 admin 用户**。
   这样现有的单管理员部署**升级后不需要任何操作**就能登录，
   `admin_auth.json` 保留为身份锚点，不删除。

3. **用户表可删**。删库重建后：重新 bootstrap，或用 `admin_token` 应急登录后重建用户。
   风险可接受——因为「忘了删库重建 + 忘了 bootstrap」这个组合，
   比现有「删库 = 失明」已经好得多。

> ⚠️ **实现警示（2026-10-05 实测踩坑）**
>
> 第 1 条的关键词是「**永远**绕过数据库」。实现时如果把它做成
> 「仅在 users 表为空时放行」的一次性引导窗口，会造成**死锁**：
>
> - 启动时 `ensureBootstrapAdmin` 已经建出 admin 账号（密码为空）
> - → users 表非空 → 引导窗口关闭
> - → `admin_token` 用不了、bootstrap admin 又登不进去
> - → **没有任何途径设置第一个密码**
>
> 实测时启动日志还在自相矛盾地提示「用 config 的 admin_token 登录后台」。
>
> 正确实现：`admin_token` / `admin_auth.json` 密码是**始终有效**的并行通道
> （见 `server.UserAuthMiddleware.WithAdminCredentials`），
> 它注入一个 ID 为空、Role=admin 的「运维身份」——
> 有管理权限但不属于任何 users 行。
>
> 另外：**不要**用「users 表为空」作为免鉴权条件。那不看任何凭据，
> 意味着全新部署时局域网里任何人能抢先建 admin 账号
> （默认 listen 是回环，但运维改成 0.0.0.0 很常见）。

### 3.6 借鉴 cc-switch：SSOT 与「不撒谎」

**SSOT 落点**：`users` 表是身份的唯一事实来源。
（旧方案的「`adminauth.json` 只存 bootstrap 单一密码」已随统一认证删除——
现在连 bootstrap 也走 users 表。）

**backfill 思想的应用**：cc-switch 切换后立刻读回 live 文件更新 SSOT。
我们的对应场景是——**用量记录不固化 user_id，靠 JOIN 回溯**。
但这是错的（§4.2），所以我们反向借鉴：**user_id 冗余固化**，
宁可数据冗余也不让「归属」依赖运行时可变的链接。

**另一处「不撒谎」的 SSOT 应用在费用口径上**：
现有 `GetUsageStats` 每次按当前单价重算历史费用，
这本质上是「**没有 SSOT**」——账随时会变。归档表必须固化 `cost_*`（§4.8），
让已归档区间有一个稳定的事实来源。

---

## 4. 改造点清单

### 4.1 鉴权链：双轨

当前 `AdminAuth` 是「一个密码通吃整个 `/admin`」。多用户后必须拆成：

```
/admin/api/*     → AdminSession（用户身份 + role==admin）
/admin/api/me/*  → UserSession（任意登录用户，只能碰自己的资源）
/v1/*            → AccessKey（不变，加 user_id 归属校验）
```

**`internal/adminauth` 的去留**：（已落定，2026-10-06：**整个包删除**，
不收窄、不保留——见 §3.5 废弃说明。会话由新包 `internal/userauth` 承担。）

### 4.2 热路径改动（`internal/auth`）

这是**最需要小心**的部分。要求：**鉴权 O(1) 不变，不引入 DB 查询。**

```go
// internal/auth/auth.go现状
type Context struct {
    KeyID, Name string
    RPMLimit, TPMLimit int
}

// 改造后
type Context struct {
    KeyID, Name string
    UserID      string   // 新增：空 = 无归属（迁移期旧 key）
    GroupID     string   // 新增
    RPMLimit, TPMLimit int
    // 以下为快照下发，不查库
    UserStatus  string   // 新增：disabled 时直接拒绝，无需查库
    UserQuota   int64
    UserUsed    int64
    AllowedModels map[string]bool  // P1：nil = 不限制
}
```

**`snapshot.Snapshot` 新增两个索引**（`internal/snapshot/snapshot.go`）：

```go
UsersByID map[string]*UserSnapshot
KeysByHash map[string]*KeySnapshot  // KeySnapshot 内嵌 UserID/GroupID/限额
// P1: GroupsByID / GroupModels
```

`RebuildFromDB`（`internal/snapshot/rebuild.go`）同步扩展。
**并发契约不变**：仍然「新建 → 填充 → Swap」，绝不原地改已发布快照。
这条契约在 `snapshot.go` 的注释里写得很重，必须守住。

**`usage_records` 加 `user_id` 冗余**：
理由——归属是**历史事实**。用户改名、key 被删、组被调整，
都不应改变「这条消耗是谁的」。若靠 JOIN 回溯，
用户删 key 后用量记录就失去归属，审计链断裂。

### 4.3 配额：三级 + 一个必须回答的问题

```
用户级 quota_tokens（总闸）
  └─ key 级 quota_tokens（子闸）
       └─ key 级 rpm/tpm（速率）
```

**预检顺序**：`key rpm/tpm`（快照，零成本）→ `key quota`（查库）→
`user quota`（查库）。任一不过即拒。

**无归属 key 不走配额链（2026-10-05 修订）**：
`user_id` 为空的 key 已被 §5.1 的退役迁移禁用，
`Authenticate` 查不到归属用户即返回 `ErrKeyUnowned`（401），
**根本到不了配额预检这一步**。这里记录它是为了说明「为什么配额链不需要
为无归属 key 写分支」——不是需要兼容它。

**必须回答的问题：用户级 `used_tokens` 怎么算？**

| 方案 | 做法 | 优点 | 缺点 |
|---|---|---|---|
| **A. 触发器累加**（仿现状） | 加第二个触发器累加 `users.used_tokens` | O(1) 读，与现状同构 | **一把 key 的用量会同时触发 key 级和 user 级两次写入**；且 key 删了怎么办？触发器按 id 累加，删 key 不回退 |
| **B. 实时 SUM** | `SELECT SUM(total_tokens) FROM usage_records WHERE user_id=?` | 永远正确，删 key 自动生效 | 每请求扫索引。已有 `idx_usage_key_ts`，需加 `idx_usage_user_ts` |
| **C. 独立计数器** | 用量落库时在 Go 里同步 UPDATE users | 可控 | 异步 worker 崩溃时计数器漂移 |

**决策：B（实时 SUM），但只在 `user.quota_tokens > 0` 时才查。**

理由：
1. 方案 A 的双触发器问题很实在——且**触发器无法感知「key 被删除」**，
   会永久留下一个偏高的 `users.used_tokens`，用户永远撞 quota 上限。
   这正是 `key_dao.go` 的 `RecomputeUsedTokens` 想解决的问题，
   不该复制到用户层。
2. 方案 C 引入漂移且要额外补偿任务。
3. 方案 B 的成本可控：`user.quota_tokens = 0`（不限）时**一次查询都不发**。
   局域网网关里「不限额」是常态，为常态付查询成本不合理。
4. 即使用了 B，`RecomputeUsedTokens` 那样的显式重算入口也要补一个给用户层。

**索引**：`CREATE INDEX idx_usage_user_ts ON usage_records(user_id, ts)`。

> ✅ **实现记录（2026-10-05）**：三级配额已全部接上。
> `cmd/gateway/main.go` 的转发热路径顺序为：
> `key rpm/tpm`（快照，零成本）→ `key quota`（查库）→ `user quota`（仅在
> `quota_tokens > 0` 时查库）→ 解码请求体。
>
> 用户级预检**排在解码之前**是刻意的：解码要读取并解析整个请求体，
> 而超额度的请求不该为此付出成本；同时错误码优先级也更合理 ——
> 超额度就该报 429，而不是先花代价解出一个随后被丢弃的请求。
>
> 读失败与 key 级同口径 **fail-open**（记 ERROR 日志后放行）：
> 一次查询抖动不该让正常流量全部 429。

**前瞻说明**：`SUM` 方案在「单用户几十万条用量」时会变慢，
与 §4.8 想解决的全局累计慢是同一类问题、只是推迟到了用户维度。
届时可升级为 `user_usage_totals(user_id, ...)` 分用户累计表——
注意那时**触发器是可用的**（分用户累计没有「key 删除」语义问题，
§4.3 否定触发器仅针对 key 级）。当前 SUM 够用，保留这条升级路径即可。

### 4.4 会话与 AuthVersion 栅栏

**问题**：用户 A 被管理员禁用，但当前快照里 A 的 `status` 还是 `active`，
在途请求与**快照重建前的所有请求**都会放行。
现有 `FailureThrottle` 只保护登录爆破，不管这个。

new-api 用 Redis 三钥匙解决（§2.4）。我们更简单：

```
禁用用户 → UPDATE users SET status='disabled', auth_version=auth_version+1
        → db: invalidateUserCache
        → snapshot.RebuildFromDB + Swap   ← 关键：立刻生效
```

我们本来就有 `snapshot.Swap` 这个原语（`autoreload.go` 已经在用），
**版本栅栏在这里其实是多余的**——因为：

- 快照是**进程内**的，`Swap` 之后立刻全量生效，没有 TTL 窗口。
- 唯一的窗口是「DB 已改、Swap 未执行」的几十毫秒。
  这个窗口用 `auth_version` 也关不上（读的就是快照里的旧值）。

**结论：不做 Redis 式三钥匙栅栏，直接复用现有 rebuild + Swap 路径。**

`auth_version` 字段仍然保留，但用途收窄为**会话失效**：

```
改密码 / 禁用账号 / 改角色 → auth_version++
JWT 里带 av → 每个请求比对快照里的当前 av → 不一致即 401
```

这解决的是另一个问题：**「用户的旧 JWT 在密码已改后仍然有效」**。
`adminauth` 现有的单密码状态做不到这件事，多用户必须做。

**JWT 的依赖决策**：`go.mod` 当前只有 `rosetta` 与 `modernc.org/sqlite`
两个直接依赖，没有任何 JWT 库。**引入 `github.com/golang-jwt/jwt/v5`**——
成熟、审计充分，是本方案唯一新增的运行时依赖。不要手写 HMAC 签名 token：
JWT 的 alg 混淆、过期、吊销等坑不值得自己踩。这也与 §6.1「不引入依赖」
不冲突——那里排除的是 Redis/Casbin 这类重依赖，不是绝对零依赖。

### 4.5 管理 API 的作用域收窄

这是**最容易被忽略、但对多用户最要命**的一类漏洞：

| 现状 | 风险 | 改造 |
|---|---|---|
| `GET /admin/api/keys` 返回**所有** key | 普通用户看到别人的 key_prefix、已用量 | 用户只看自己的；管理员看全部（`?scope=all`） |
| `PATCH /admin/api/keys/{id}` 不校验归属 | 改别人的 key | 先校验 `key.user_id == session.user_id \|\| role==admin` |
| `GET /admin/api/usage` 全局聚合 | 普通用户看全局用量 | 按 user_id 过滤 |
| `GET /admin/api/stats` | 同上 | 管理员专属 |
| `GET /admin/api/providers` `/routes` `/settings` | 普通用户能改上游 | **保持 admin-only**，多用户阶段不放开 |

**建议实现方式**：不要在每个 handler 里手写 `if isAdmin`。
在 `internal/admin` 加一层统一的：

```go
// 从 context 取身份，注入 SQL 过滤条件
func (h *Handler) scopeFilter(r *http.Request) store.Scope
type Scope struct {
    UserID string  // 非空 = 限制到此用户
    IsAdmin bool   // true = 不限制
}
```

然后所有 DAO 方法多接一个 `scope` 参数。**漏加一个的后果是数据泄露**，
所以要把「scope 参数」做成强制——即所有 DAO 方法签名都带它，漏了编译不过。

> ⚠️ **实现修正（2026-10-05）：必须做两层，只做数据层不够**
>
> 上面只说了数据层（DAO 带 scope）。实测发现**光做数据层会漏掉整类端点**：
> 只对 key 与 usage 做了收窄，而 `/stats`、`/settings`、`/providers`、
> `/routes` 没做任何权限校验 —— 普通用户全部能读，全部返回 200。
>
> 正确做法是两层都要：
>
> **第 1 层（端点层）**：`server.AdminGateGuard` —— **白名单**模式。
> 明确列出普通用户可访问的前缀，其余默认要求管理员：
>
> ```go
> var userAccessiblePrefixes = []string{
>     "/admin/api/keys", "/admin/api/usage", "/admin/api/stats",
>     "/admin/api/me", "/admin/api/logout",
>     "/admin/api/password/", "/admin/api/auth/",
> }
> ```
>
> **第 2 层（数据层）**：白名单端点内部按 `callerScope(r)` 收窄。
>
> **为什么用白名单而不是逐个标 admin-only**：51 个路由靠人手标必然漏
> （实测第一版漏了 4 个）。而漏标的后果：
> - 黑名单漏标 = **静默越权**（接口照常 200，没人发现）
> - 白名单漏标 = **显式不可用**（403，立刻发现并修掉）
>
> 安全默认值应该朝「显式失败」的方向，不是「静默放行」。
>
> `callerScope` 还有一个容易写错的细节：**没有身份时必须返回一个
> 不可能匹配任何行的哨兵值，而不是空串** —— 空串是 admin 语义，
> 返回空串会让未鉴权请求看到全量数据。

### 4.6 前端改造

`web/src/router.ts` 现在是**单一布局、单一密码门禁**。改造后：

```
/login          登录（拿 JWT）
/setup          首次设置密码（保留现有逻辑，仅在无任何用户时可达）
Layout
├── /dashboard   概览（用户：自己的；admin：全局）
├── /keys        Key 管理（新增「归属」列）
├── /usage       用量（新增按用户筛选）
├── /groups      分组管理（admin）
├── /providers   （admin）
├── /routes      （admin）
└── /settings    （admin）
```

新增一个 `/admin/api/me` 维度：**用户只能改自己的密码、看到自己的 key 与用量**。

前端现有的 `theme-init.js` 那个坑不要重犯——
CSP 是 `script-src 'self'`，任何内联脚本会被**静默拦掉**，
功能坏掉但构建与测试全绿。改造时新增的任何内联脚本都要走外部文件。

### 4.7 reload 的原子性缺口

现状（`snapshot/rebuild.go` 包注释写明的分工）：

```
ReloadHandler:  先重建上游池（BuildFromStore，解密 api_key）
                → 再 RebuildFromDB + snapshot.Swap
```

`Swap` 是原子的，但**两步之间有窗口**：池已换新、快照还是旧的。
若这期间快照被Swap 到新值，而新快照引用的某个 provider 在池重建时
因故未建成（密钥解密失败、DB 读错），请求就会路由到一个不存在的客户端。

单管理员时这个窗口几乎撞不上（配置改动少）。
**多用户阶段用户能自己改 key 白名单与分组，配置改动频率上升一个量级，
撞上的概率显著提高。**

**改造方案（三选一，推荐 b）**：

| 方案 | 做法 | 评价 |
|---|---|---|
| a. 全局互斥锁 | 仿 cc-switch `switch_lock`，reload 期间串行化所有配置写 | 有效但粗暴：阻塞所有管理写，包括与池无关的改备注 |
| **b. 池失败则拒绝 Swap**（推荐） | 重建池失败时**不 Swap**，保留旧快照，并让 `BuildFromStore` 返回「哪些 provider 失败」；对失败项在快照里标记不可用 | 失败可观测，且不阻塞无关写入 |
| c. 快照引用池的句柄而非 ID | 快照直接持有 client 指针，池变成不可变对象整体替换 | 最干净，但要重写 `routing.RouteIndex` 的查找逻辑，改动面过大 |

**决策：b。** 关键是要**让失败可见**——现在 `BuildFromStore` 若部分失败
大概率只记 WARN（参考现有 provider_credentials 解密路径的写法），
运维看到的是「路由配了但请求莫名 500」，查不到根因。
改造时要让它进管理面：哪些 provider 因密钥问题未就绪，在 Providers 页面显式标出。

**实现注意（接口改造，不是顺手小改）**：方案 b 依赖 `BuildFromStore`
返回「部分失败列表」，但现有 `RebuildFromDB(ctx, st) (*Snapshot, error)`
的错误是**全有或全无**的，没有 partial-failure 通道（已核实
`snapshot/rebuild.go` 签名）。因此本项需要**改 `RebuildFromDB` 的返回结构**，
引入一个可观测的「失败 provider 列表」，而不是只加个布尔开关。

### 4.8 用量归档：明细保留 30 天，累计总额永久正确

**这是长期发展里最容易被低估的一项。**

现状：全部统计查询（`stats_handler.go` 的 `Get`、
`usage_handler.go` 的 `Query`/`summarize`/`ExportCSV`/`History`/
`GroupByKey`/`GroupByModel`/`GroupByProvider`/`GroupByDay` 共 8 个入口）
**全部打在 `usage_records` 明细表上**。

现有代码已经在为此打补丁——`usage_dao.go:ListModelThroughput` 被迫加了
30 天窗口，注释写得很直白：

> 调用量大的 provider 积累几十万行后…会直接阻塞配额预检与用量写入

用户量从 1 到 20，调用量可能涨 20 倍。**这是最先崩的地方**：
`usage_records` 没有保留策略，行数无上界，而写池是 `SetMaxOpenConns(1)`——
任何聚合查询误配到写池就会**阻塞全部用量落库**，表现为「网关整体卡死」。

#### 需求：明细可删，累计不能断

**明确的目标口径**：

| 数据 | 保留策略 | 需求来源 |
|---|---|---|
| **明细**（逐次调用） | **30 天**，超期清理 | 排障只需回溯近期；长期留着无价值且占容量 |
| **累计总额**（总 token、总次数、总费用） | **永久，不能因清理而改变** | 总览页「全部」档 |

**这是硬需求，不是优化项。** 剪枝后总览页的数字如果从 3 亿掉到 5 百万，
比「看板变慢」严重得多——那是**数据丢失**，且用户不会立刻发现。

#### 方案：日归档表 + 累计表双层

与 cc-switch 的单一 rollup 不同，这里要**两张表**——
因为「按天/按模型/按 key 看趋势」和「看历史累计」是两种不同粒度的查询：

```sql
-- 表 A：日归档（按维度分组，支撑趋势与分项统计）
CREATE TABLE IF NOT EXISTS usage_daily_rollups (
    day              TEXT NOT NULL,      -- 'YYYY-MM-DD'
    user_id          TEXT,               -- 分用户（P0 已加）
    access_key_id    TEXT,
    public_model     TEXT,
    provider_id      TEXT,
    ingress_protocol TEXT,
    stream           INTEGER,
    request_count    INTEGER NOT NULL DEFAULT 0,
    success_count    INTEGER NOT NULL DEFAULT 0,
    input_tokens     INTEGER NOT NULL DEFAULT 0,
    output_tokens    INTEGER NOT NULL DEFAULT 0,
    cached_tokens    INTEGER NOT NULL DEFAULT 0,
    reasoning_tokens INTEGER NOT NULL DEFAULT 0,
    -- 固化计价基准。不固化的话，改价后历史费用永久算错（§2.7.1-1）
    cost_total       REAL NOT NULL DEFAULT 0,
    -- 延迟按请求数加权平均，不能算术平均（§2.7.1）
    latency_sum_ms   INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (day, user_id, access_key_id, public_model, provider_id,
                 ingress_protocol, stream)
);

-- 表 B：终身累计（单行表，支撑「全部」档的大数字）
CREATE TABLE IF NOT EXISTS usage_totals (
    id               INTEGER PRIMARY KEY CHECK (id = 1),   -- 恒定单行
    request_count    INTEGER NOT NULL DEFAULT 0,
    success_count    INTEGER NOT NULL DEFAULT 0,
    error_count      INTEGER NOT NULL DEFAULT 0,
    input_tokens     INTEGER NOT NULL DEFAULT 0,
    output_tokens    INTEGER NOT NULL DEFAULT 0,
    cached_tokens    INTEGER NOT NULL DEFAULT 0,
    reasoning_tokens INTEGER NOT NULL DEFAULT 0,
    cost_total       REAL NOT NULL DEFAULT 0,
    latency_sum_ms   INTEGER NOT NULL DEFAULT 0,
    first_record_at  INTEGER NOT NULL DEFAULT 0  -- 见下方「趋势起点」
);
```

**表 B 为什么要单独一张**：表 A 是按维度分组的，
「历史总 token」需要 `SUM(...) OVER ()` 全表聚合——而 rollup 行数会随
「天数 × 用户数 × key 数 × 模型数」增长，比明细小但也不小。
一个恒定单行的表，O(1) 读，永远不删。这是「累计不受影响」的最直接保证。

#### 写入时机：与用量落库同事务

**关键决策：累计值在用量落库时同步累加，而不是靠扫明细重算。**

现有 `trg_update_used_tokens` 已经在 `usage_records` INSERT 后触发器里
累加 `access_keys.used_tokens`。表 B 的累加用同样手法：

```sql
CREATE TRIGGER IF NOT EXISTS trg_update_usage_totals
    AFTER INSERT ON usage_records
    BEGIN
      UPDATE usage_totals SET
        request_count  = request_count  + 1,
        success_count  = success_count  + CASE WHEN NEW.status = 'ok' THEN 1 ELSE 0 END,
        error_count    = error_count    + CASE WHEN NEW.status NOT IN ('ok','canceled') THEN 1 ELSE 0 END,
        input_tokens   = input_tokens   + NEW.input_tokens,
        output_tokens  = output_tokens  + NEW.output_tokens,
        cached_tokens  = cached_tokens  + NEW.cached_tokens,
        reasoning_tokens = reasoning_tokens + NEW.reasoning_tokens,
        -- ⚠ 这里不能用「当前单价」算，见下方「单价固化」的坑
        cost_total     = cost_total + NEW.cost_total,  -- NEW.cost_total 是 Go 侧落库前算好的
        latency_sum_ms = latency_sum_ms + NEW.latency_ms,
        -- 首次写入时置为 NEW.ts；已非 0 则取更早者。
        -- ⚠ 不能写 MIN(first_record_at, NEW.ts)：初始值 0 会让 MIN 恒为 0，永远置不进去。
        first_record_at = CASE WHEN first_record_at = 0 THEN NEW.ts
                               ELSE MIN(first_record_at, NEW.ts) END
      WHERE id = 1;
    END;
```

> **注意**：表 B 单行必须**先插入一行 `(id=1)` 占位**（`INSERT OR IGNORE`），
> 否则 `UPDATE ... WHERE id=1` 命中 0 行，触发器变成空操作、累计静默丢失。

**为什么 `cost_total` 不能在触发器里算**：
触发器拿不到「这条记录对应哪个模型、当时单价多少」。
两个可选解法：

| 解法 | 做法 | 评价 |
|---|---|---|
| **a. 落库时在 Go 侧算好成本列** | `usage_records` 加 `cost_total` 列，写入时按当时单价算好 | **推荐**。触发器直接累加，O(1)，且顺带把费用固化在明细上 |
| b. 归档时回填 | 只在 rollup 阶段算 | 明细存续期间的费用算不出来（总览「近 14 天」的费用就没法算） |

**决策 a**。这意味着 `usage_records` 要加一列 `cost_total`——
**这是对现有费用语义的一次性修正**，好处是彻底消除
「改价后历史费用变」这个已经在代码注释里被承认的怪异语义（§6.3 讨论过）。

```sql
-- 触发器引用 NEW.cost_total 的前提：usage_records 必须先有这个列
ALTER TABLE usage_records ADD COLUMN cost_total REAL NOT NULL DEFAULT 0;
```

**落库时的赋值**：`cost_total` 在 Go 侧按「该模型当时单价」算好后再 INSERT。
算价的复用现有 `usage_dao.go` 的费用口径（缓存未命中输入 × price_input +
缓存命中 × price_cache_hit + 输出 × price_output，单位换算同现状）。
注意：**历史数据回填**——存量行 `cost_total=0` 会在升级后把历史费用算成 0，
需要一次一次性回填（用当时的 `upstream_models` 单价算，虽不精确但比 0 强），
或者接受「升级前历史费用不可追溯」。这个回填范围要明确写进迁移。

#### 单价固化的坑（必须写清楚，否则一定踩）

加了 `cost_total` 列后，**所有费用查询都必须改成读 `SUM(cost_total)`，
不能再 JOIN `upstream_models` 拿当前单价重算**。

现有 `GetUsageStats`、`SumCostBuckets` 都是 JOIN 重算的
（`usage_dao.go` 注释：「按**当前** `upstream_models` 里的单价实时计算，
改价后历史区间统计随之变化，usage_records 不固化金额」）。

**改价后历史费用不再变化** —— 这是**语义变更**，必须在 P1.5 与归档表
作为一个整体上线，且要在界面上说明（否则又是一处「界面在说谎」）。

好消息：这个变更**消除了一个长期存在的怪异行为**。
现在的语义下，管理员改个价，昨天报表里的历史费用会跟着变，
这是对账时最容易吵架的地方。

#### 剪枝流程

```
每日一次（或手动触发）：
  1. SAVEPOINT begin
  2. 选出 cutoff 之前、尚无 rollup 行的明细 → 聚合写入表 A（幂等合并）
  3. DELETE 同一批明细行
  4. RELEASE savepoint        （失败则 ROLLBACK TO，两步都不留半成品）
```

**切分点必须对齐本地午夜**（cc-switch 的 `compute_local_midnight_cutoff`，
它还专门处理了 DST gap/ambiguous）。理由同 §2.7.1-2：
切在半天中间会导致那一天被切成两半，区间查询静默少算。

**表 A 的聚合必须幂等**（`INSERT OR REPLACE ... LEFT JOIN old` 累加，
见 §2.7.1），否则重跑同一天会算成两倍。

**剪枝不动表 B**：表 B 靠触发器实时累加，与剪枝完全解耦。
剪枝只影响表 A 和明细表，不影响累计值——这正是需求的保证。

#### 趋势起点：全部档的「最早一天」

`Overview.vue:60-63` 的逻辑：

```js
// 全部历史：以数据里最早一天为起点
if (days === 0) {
  const keys = byDay.value.map((e) => e.key).sort()
  const earliest = keys[0]
  start = earliest ? new Date(earliest + 'T00:00:00') : new Date(now..., -30)
}
```

`byDay` 来自 `usageByDay(0, now)` → `GroupByDay` → **查明细表**。
明细剪掉 30 天后，`keys[0]` 会变成 30 天前那天，
**趋势图会凭空丢掉 30 天以前的历史**。

**修复方案**：让 `GroupByDay` 在「全部」档时**同时查表 A**，
起始日以 `usage_totals.first_record_at` 为准（表 B 里记了终身最早一条记录的时间，
永久保留，不受剪枝影响）。

这个字段在设计上是免费的——反正表 B 是恒定单行，多存一个时间戳没成本。

#### 日界口径：现有代码已经不一致了

这是个**已存在的隐患**，归档会把它放大。实测：

| 位置 | 表达式 | 日界 |
|---|---|---|
| `usage_handler.go:467` `GroupByDay` | `strftime('%Y-%m-%d', ts/1000,'unixepoch','localtime')` | **本地时区** |
| `usage_dao.go:328` `SumTokensByDayForKey` | `date(ts/1000, 'unixepoch')` | **UTC** |
| `store.go` `usage_records` 索引 | 无 | — |

**两套口径已经并存**，只是没人注意（差异在跨零点的请求上，通常一天几行）。

**归档必须处理，否则会出现「归档切点与查询日界差几个小时」的不一致**：
表 A 的 `day` 列统一用**本地时区**（与 `GroupByDay` 一致，
这是用户界面上看到的那一套），切分点也用本地午夜。
`SumTokensByDayForKey`（UTC）**保持不变**——
它是 `/v1/organization/usage` 的数据源，与界面无关，改它属于超出本次范围。

**在表 A 的注释里写死这条约定**，否则半年后没人记得为什么两套并存。

### 4.9 30 天后如何追查单次调用

需求：**明细剪枝后，是否还能追查「某月某日那次调用」的细节？**

结论：**能，但靠日志，不靠数据库。** 这一节说明为什么，
以及顺带发现的一个现存缺陷。

#### 先澄清一个想当然的方案：request_id 映射表

第一反应是建一张窄表保住 `request_id → 明细行`的指向：

```
request_id         保留期
a1b2c3d4          → 指向 usage_records 的某一行
```

**这个方案有三个问题，因此不采用**：

1. **它和日志重复。** `usage_records` 本来就有 `request_id` 列，
   转发路径每次都带 `server.RequestIDFromContext` 落日志
   （`main.go:886/900/935/994/1060/1086`），日志里同时有
   request_id、时间、模型、provider、状态码、延迟、token。
   **排障需要的信息比映射表更全**，且日志有独立的保留策略（运维在轮转）。
   在数据库里再建一份等于把日志已有的能力重复实现一遍。

2. **保留映射等于没剪枝。** 映射表要能追查出完整信息，
   意味着那些数据**一条都没删**，只是换了张表存。
   剪枝的意义是控制总行数，不是换个表名。

3. **成本不低。** 多一张永久增长的表（行数≈被剪掉的明细数），
   换来一个日志已经能做的事。

**正确做法**：需要更长的追查窗口时，**延长日志保留期**。
日志本来就是为这件事准备的，且它不参与数据库的写锁竞争。

#### 顺带发现：`request_id` 列一直没写入值

排查过程中发现的独立缺陷，**与归档无关，值得单独修**：

- `store.go` 的 `usage_records` 有 `request_id TEXT` 列；
- `usage_dao.go:37` `CreateUsageRecord` 的 INSERT 语句里有 `r.RequestID`；
- 但 `usage_dao.go:9-30` 的 `UsageRecord` 结构体字段里，
  **全仓搜不到任何 `RequestID:` 的赋值**——转发路径构造记录时压根没传。

**即：这列大概率恒为 NULL。**

**影响**：30 天内想用数据库查某次调用（比翻日志方便）也做不到。

**修法**（P0 顺手做，成本极低）：

```go
// cmd/gateway/main.go 的用量记录构造处
rec.RequestID = server.RequestIDFromContext(r.Context())
```

**这个缺陷正好是 `dropDeadColumns` 注释警告的「schema 在说谎」的活例**：
列在、查询能写、界面没有入口——比注释里列的那些占位列更隐蔽，
因为它看起来「像是实现了」，实际从未接线。**列存在 ≠ 数据存在。**

#### 分工写进文档

| 需求 | 查哪里 | 保留期 |
|---|---|---|
| 统计、趋势、账单 | 数据库（归档表 + 累计表） | 永久 |
| 30 天内追查某次调用 | 数据库（`usage_records`，修好 request_id 后） | 30 天 |
| 30 天后追查某次调用 | **日志**（按 request_id 检索） | 随日志策略 |

---


## 5. 迁移方案

### 5.1 存量数据归谁

这是最需要拍板的一点。三种选择：

| 方案 | 做法 | 代价 |
|---|---|---|
| **A. 全部归一个 admin** | 建 admin 用户，所有存量 `access_keys.user_id` 指向它 | 简单。但**普通用户拿到的 key 也会归属 admin**——用户自己管不了自己的 key |
| **B. 存量 user_id 置 NULL + 退役** | 允许 `user_id` 可空；迁移时把无归属 key **禁用** | 语义清晰。**代价：升级后存量 key 全部失效，使用者必须换新 key** |
| **C. 建虚拟「默认组」** | 存量归 `default` 组下的系统用户 | 折中，多一层概念 |

**决策：B（2026-10-05 修订）。** `user_id` 可空 = 无归属 key；
**升级后不发新 key 就不给用**。

**为什么改成「退役」**（原方案是「照常可用」）：
多用户改造的前提是「每把 key 都能追溯到人」。让无归属 key 继续工作，
就等于在系统里留一块「谁的 key 说不清」的黑区 —— 那正是多用户要消除的东西。
既然是内网网关、key 重新发放的成本只是让同事改一次环境变量，
不值得为省这点事留一个归属不清的口子。

**实现方式是「禁用」而非物理删除**（`store.retireOrphanKeys`）：

```sql
UPDATE access_keys SET enabled = 0 WHERE user_id IS NULL AND enabled = 1
```

三条理由：
1. **可逆**。误操作（迁移逻辑写错、误在生产库上跑）可以改回来。
2. **用量历史不断**。`usage_records` 按 key_id 归集，物理删除会让那些记录
   变成悬空数字，对账时无法解释「这把 key 上个月用掉的 token 是谁花的」。
3. **责任可追溯**。谁在什么时候用过这把 key，依然查得到。

**热路径行为**：`Authenticate` 查不到归属用户即返回 `ErrKeyUnowned` → **401**。
明确失败好过静默放行。

**升级后必须做的事**（否则所有人直接失联）：
1. 为每个使用者建 `users` 账号；
2. 逐个发新 key（管理端创建时指定 `user_id`）；
3. 通知使用者更新本地配置。

**新建的 key 必须带 `user_id`**：用户自助创建时自动填自己，
管理端创建时显式指定。

### 5.2 迁移必须幂等

沿用现有 `store.go` 的模式（`ensureColumns` / `backfillRouteTargets`）：

```go
// migrate() 新增调用
s.ensureUserColumns()    // ALTER TABLE ADD COLUMN，先查 table_info
s.backfillRouteTargets() // 已有
s.ensureBootstrapAdmin() // users 表没有已设密码的 admin 时，建空密码 admin（最终实现）
```

**全部幂等**，每次 `Open` 都执行，老库升级与新库首建都覆盖到。
这是 `store.go` 里既有的成熟模式，直接沿用。

### 5.3 分阶段实施

| 阶段 | 内容 | 可上生产 | 交付物 |
|---|---|---|---|
| **P0** | `users` 表 + `access_keys.user_id`（可空）+ bootstrap 管理员 + 迁移 + **补 `request_id` 写入缺失**（§4.9） | ✅ | 后端可用，UI 不变 |
| **P0.5** | `userauth` 会话包 + JWT + `/admin/api/me` | ✅ | 用户能登录看自己的数据 |
| **P1** | `groups` + `user_group_models` + key 级 `allowed_models` + `/v1/models` 按身份过滤 | ✅ | 分组权限生效（含用户级配额执行点） |
| **P1.5** | **用量归档**：`usage_records.cost_total` 列 + `usage_daily_rollups` + `usage_totals` 累计表 + 30 天剪枝 + 查询改写（§4.8） | ✅ | 明细可删、累计不变 |
| **P2** | `allowed_ips` / `expires_at` / key 级 `group` 覆盖 / 组级共享额度 | ✅ | 功能完整 |
| **P3** | 前端完整改造（用户视图、admin 视图分离） | ✅ | 界面就绪 |
| **P4** | reload 原子性收口（§4.7）+ 失败 provider 可见化 | ⚠️ 可延后 | 运维可观测性 |

**关于 P1.5 的排序**：它被放在 P1 之后而非之前，理由是**它不阻塞功能上线，
但必须在用户量上来之前完成**。若当前外部用户已超过十来人，
建议把 P1.5 提到与 P1 并行——它改的是 DAO 查询层，与分组功能无耦合。

**关于 P4 的排序**：reload 的池/快照窗口是**已存在的隐患**，不是多用户引入的。
多用户只是把配置改动频率提高，让撞上的概率上升。
如果当前管理操作频繁，可以提前做；如果不频繁，放 P4 完全合理。

**建议节奏**：P0 与 P0.5 一起上，中间不要停——只有 users 表没有会话，
用户拿不到登录入口，等于半成品。

### 5.4 回滚

**迁移前先备份**（这一步不能省——虽然迁移是加列，但退役是写操作）：

```bash
cp gateway.db gateway.db.bak-$(date +%Y%m%d-%H%M%S)
```

**回滚只需回滚二进制**：新增的 `users` 表与 `access_keys.user_id` 列
被旧二进制完全忽略（SQLite 允许多余列/表）。

**但要注意 `retireOrphanKeys` 是写操作**：它把无归属 key 置为 `enabled=0`。
旧二进制**不认识 user_id**，会把这些 key 照常放行 —— 也就是说回滚二进制后，
那些被禁用的 key 会重新可用。这通常是可接受的（回到改造前状态），
但如果你在退役后又给某把 key 补了 user_id，回滚会让它在旧版下
以「无归属但启用」的状态运行。要彻底回到退役前，只能从备份恢复。

### 5.5 升级操作清单

**P0 已实现，可以按下面这份清单上线。** 顺序很重要——第 3 步不做，
第 4 步之后所有人都会失联。

```bash
# 1. 备份（可回滚的前提）
cp gateway.db gateway.db.bak-$(date +%Y%m%d-%H%M%S)

# 2. 停服，换新二进制，启动
#    启动日志应出现：
#      database migrations completed
#      retired unowned access keys ... (count=N)
#      bootstrapped admin user for multi-user migration

# 3. 建账号（管理端或直接写库），至少覆盖所有在用存量 key 的人

# 4. 逐人发新 key（管理端创建时指定 user_id）

# 5. 通知各人更新本地配置（旧 key 已失效，会返回 401）

# 6. 验证：随便取一把新 key 打一次真实请求
```

**验证退役是否符合预期**：

```sql
-- 应为 0：有启用且无归属的 key
SELECT COUNT(*) FROM access_keys WHERE user_id IS NULL AND enabled = 1;

-- 存量 key 的历史用量仍在（对账依据没丢）
SELECT key_hash, COUNT(*), SUM(total_tokens) FROM usage_records
 GROUP BY key_hash ORDER BY SUM(total_tokens) DESC LIMIT 10;
```

第 2 条查出来的是「旧 key 花了多少」——退役不影响它，这是选禁用而非删除的收益。

---

## 6. 风险与取舍

### 6.1 明确不做的事

| 不做 | 理由 |
|---|---|
| **Casbin / 任何外部 RBAC 库** | new-api 自己已经收敛回静态矩阵（§2.3）。依赖成本 > 收益 |
| **Redis** | 单进程局域网网关，`snapshot` 原生就是内存快照。为分布式一致性引入外部依赖是负债 |
| **bcrypt/argon2** | 沿用 `adminauth` 的 PBKDF2 21 万次。多一个密码库就多一处不一致的风险 |
| **用户自注册** | 局域网场景，管理员开户足够。加注册 = 加邮箱/验证码/密码找回全套 |
| **充值 / 余额 / 结算** | `DESIGN.md` §1 已明确「只做用量记账，不做钱」。保持 |
| **放开 provider/route 的用户编辑权** | 多用户阶段一律 admin-only。这是最敏感的运维面 |

### 6.2 真实风险清单

| 风险 | 影响 | 缓解 |
|---|---|---|
| **漏加 scope 过滤泄露他人数据** | 严重 | DAO 签名强制带 scope（§4.5），漏了编译不过 |
| **剪枝导致累计数字变小** | **严重** | 累计走 `usage_totals` 单行表，触发器实时累加，**与剪枝完全解耦**（§4.8）。上线前必须验「剪枝前后 `request_count`/`total_tokens` 完全相等」 |
| **明细表膨胀拖垮数据面** | **严重** | P1.5 归档。写池仅 1 条连接，聚合查询误配会阻塞全部用量落库 |
| **归档后费用口径变化** | 中 | `usage_records.cost_total` 在落库时固化单价，费用查询全部改读该列。**改价不再影响历史**——这是语义变更，需在界面与文档说明（§4.8） |
| **趋势图「全部」档丢掉 30 天前历史** | 中 | 起始日改用 `usage_totals.first_record_at`（永久保留，不受剪枝影响） |
| **日界口径混用（UTC vs 本地）** | 中 | 表 A 的 `day` 与切分点统一本地时区（与 `GroupByDay` 一致）；`SumTokensByDayForKey`(UTC) 保持不变。**已实证两套口径目前并存**（§4.8 末） |
| **用户级 quota SUM 拖慢热路径** | 中 | `quota_tokens=0` 时完全不发查询；加索引；压测验证 |
| **快照重建在用户数大时变慢** | 中 | 现有 `ListAccessKeys` 已全量拉取，用户表加入后量级相近。超百用户再考虑差分 |
| **reload 的池/快照中间窗口** | 中 | 池失败则拒绝 Swap + 失败 provider 在界面可见化（§4.7）。配置改动频繁时可提前做 P4 |
| **`used_tokens` 双触发器** | 中 | 已定：用户层用 SUM，不用触发器（§4.3） |
| **禁用用户有几十毫秒窗口** | 低 | 复用 rebuild+Swap；窗口内请求已经在途，无法回收，接受 |
| **升级瞬间全员 401** | **高** | 这是「存量 key 全部退役」的既定后果（§5.1）。**上线前必须先建好 users 账号并把新 key 发到每个人手上**，否则升级即全面失联。退役用禁用而非删除，误判可回滚 |
| **`request_id` 列恒为 NULL** | 低 | 已定位：`usage_records` 有该列但全仓无赋值。P0 补一行赋值（§4.9）。影响「30 天内按 request_id 查库」，日志不受影响 |

### 6.3 与既有 dead column 教训的对照

`store.go` 的 `dropDeadColumns` 长注释是本项目最有价值的工程遗产之一：

> 留着它们的代价不是磁盘，而是 **schema 与界面在说谎**：
> 运维照着「0 = 不过期」「按优先级择优」去配，配完发现毫无效果，
> 只能翻源码才发现没接线。

**据此，本方案强制两条纪律**：
1. `groups.quota_tokens` 等列**要么同时有执行点，要么不放进建表语句**。
   宁可少一列，不要一个「界面上能配但不生效」的字段。
2. 前端新增的任何字段（分组、用户归属），
   **必须有后端执行点后才允许出现在界面上**。

**但本方案自己也踩在这个坑的边缘，有一处必须诚实标注**：

`usage_records.cost_total`（§4.8）会**改变现有的费用语义**——
现在「改价影响历史区间」，改造后「落库即固化，改价不影响任何历史」。

若归档功能上线而费用口径未同步调整，
就会出现「**明细区间的账随改价变、归档区间不变**」——
这正是我们警告的「schema 与界面在说谎」的另一种形态。

**因此 P1.5 必须把费用口径改造与归档功能作为一个整体上线，不可拆分。**
界面上也必须显式标注归档区间的只读属性。

**但要说清楚：这个变更本身是收益，不是代价。**
现在的语义下，管理员改个价，昨天报表里的历史费用会跟着变——
这是对账时最容易吵架的地方。固化单价消除的正是这个怪异行为。

**另一个已存在的同类问题：`usage_records.request_id` 恒为 NULL。**
列在、INSERT 语句里有、全仓却没有赋值点（§4.9）。
它比注释里列的那些占位列更隐蔽——**看起来「像是实现了」，实际从未接线**。

**由此补一条纪律**：
3. **新增列必须验证「真的有值」**，而不是只看 INSERT 语句里出现了列名。
   最简单的验证：写完接入点后跑一次真实请求，
   `SELECT request_id FROM usage_records ORDER BY ts DESC LIMIT 1` 看是否非空。

---

## 7. 参考实现索引

| 结论 | 源文件（`C:/Users/louis/temp/ref/`） |
|---|---|
| User/Token 二级模型 | `new-api/model/user.go:78`、`model/token.go:16` |
| Ability(group,model,channel) | `new-api/model/ability.go:20` |
| 模型可见性判定顺序 | `new-api/controller/model.go:112-201` |
| Casbin 降级为 deny 的实测痕迹 | `new-api/service/authz/adapter.go` LoadPolicy |
| 静态角色矩阵 | `new-api/service/authz/role.go` |
| AuthVersion 三钥匙栅栏 | `new-api/model/user_auth_cache.go:1-40` |
| 用户缓存 TTL=60s | `new-api/model/user_cache.go:54` |
| 令牌状态机校验 | `new-api/model/token.go:220` |
| 分组鉴权（含 token 覆盖） | `new-api/middleware/auth.go` TokenAuth 末段 |
| IP 白名单 CIDR | `new-api/middleware/auth.go` GetIpLimits + IsIpInCIDRList |
| **明细+日聚合两层表** | `cc-switch-main/src-tauri/src/database/dao/usage_rollup.rs` |
| **幂等合并（LEFT JOIN old 累加）** | 同上 `do_rollup_and_prune` |
| **剪枝前先回填费用** | 同上 `rollup_and_prune` 的 backfill 段 |
| **切分点对齐本地午夜（含 DST 处理）** | 同上 `compute_local_midnight_cutoff` |
| **SAVEPOINT 包住聚合+剪枝** | 同上 89-112 行 |
| **reload per-app 互斥锁** | `cc-switch-main/src-tauri/src/proxy/switch_lock.rs` |
| SSOT + backfill | `cc-switch-main/src-tauri/src/provider.rs` |
| **cc-switch 无多用户模型的确认** | `cc-switch-main/src-tauri/src/database/schema.rs`（13 张表全文核对） |

**本项目自身的实证点**（§4.8 引用）：

| 结论 | 源文件 |
|---|---|
| 「全部」档 = `from=0` 扫全表 | `internal/admin/stats_handler.go:37`、`web/src/api.ts:190` |
| 默认档是「近 14 天」不是「全部」 | `web/src/views/Overview.vue:24-26` |
| 趋势图起点取自 byDay 的最早一天 | `web/src/views/Overview.vue:60-63` |
| 8 个统计查询全打明细表 | `internal/admin/usage_handler.go`（470/509 等）、`stats_handler.go:59` |
| `GroupByDay` 用 **localtime** | `internal/admin/usage_handler.go:467` |
| `SumTokensByDayForKey` 用 **UTC**（口径已不一致） | `internal/store/usage_dao.go:328` |
| 触发器累加可作为累计实现范式 | `internal/store/store.go` 的 `trg_update_used_tokens` |
| 写池单连接 | `internal/store/store.go:42` `SetMaxOpenConns(1)` |
| **`request_id` 列恒为 NULL**（无赋值点） | `internal/store/usage_dao.go:37` 有 INSERT 列名、`:9-30` 结构体无对应赋值、全仓无 `RequestID:` |
| 排障信息日志已覆盖 | `cmd/gateway/main.go:886/900/935/994/1060/1086`（每次都带 request_id） |

---

## 8. 待拍板事项

1. **是否需要「用户自助改密码」？** 影响 P0.5 是否要做 `password/change` 端点。
   默认做——`auth_version` 机制已就位，成本很低。
2. **是否需要组级共享额度？** 即「一组人共用一个总额度」。
   new-api 支持（分组 quota 共享），但会引入**组内竞争的并发扣减**复杂度。
   默认不做，先只用 key 级 + 用户级。
3. **普通用户能否看到自己的费用金额？** 当前 `GetUsageStats` 只算管理员口径。
   默认不做——`DESIGN.md` 明确「不做计费结算」，金额展示容易引发对账争议。
4. **JWT还是服务端会话？** JWT 无状态、易水平扩展，但**无法服务端强制撤销**
   （只能靠 `auth_version` 栅栏）。服务端会话可立即撤销但需要存储。
   局域网单进程场景两者都可以，**默认 JWT + `auth_version`**（零外部依赖）。

**已确认的（2026-10-05）**：
- **明细保留 30 天**，超期剪枝，累计总额永久正确（§4.8）。

5. **30 天后如何追查单次调用？** 答：**查日志，不查数据库**。详见下方新增的 §4.9。
6. **费用口径变更是否接受？** §4.8 会把费用从「按当前单价重算」
   改为「落库时固化」，**改价不再影响任何历史数据**。
   这个变更消除了「管理员改个价，昨天报表跟着变」的对账争议，
   建议接受。但它确实改变了现有行为，需在界面与文档说明。

---

## 9. 方案审计（2026-10-05）

对这份方案本身做一次独立审计。结论：**整体架构成立，方向正确；
但有 3 处会直接导致实现错误的问题已在本轮修正，另有 4 处论证薄弱需要补强。**

### 9.1 已修正的硬伤（本轮发现并改掉）

| # | 问题 | 严重度 | 修正 |
|---|---|---|---|
| 1 | `cost_total = cost_total + 0` 占位符——触发器里 NEW 引用的新列是 `NEW.cost_total`，写成 `+0` 会让累计费用恒为 0 | **高** | 已改为 `+ NEW.cost_total`，并注明该列由 Go 侧落库前算好 |
| 2 | `first_record_at = MIN(first_record_at, NEW.ts)`——初始默认 0 时 `MIN(0, ts)` 恒为 0，时间戳永远置不进去 | **高** | 改为 `CASE WHEN first_record_at = 0 THEN NEW.ts ELSE MIN(...) END` |
| 3 | 表 B（`usage_totals`）单行没有初始化说明——`UPDATE WHERE id=1` 命中 0 行时触发器变空操作，累计静默丢失 | **高** | 补注：需 `INSERT OR IGNORE (id=1)` 占位 |

这三处都是「看起来对、跑起来静默错」的类型——正是本文 §6.3 反复警告的那一类。

### 9.2 论证薄弱、需要补强的四点

**1. JWT 没有依赖，文档没提这个决策的成本。**

`go.mod` 当前只有 `rosetta` 和 `modernc.org/sqlite` 两个直接依赖，
**没有任何 JWT 库**。§4.4 说「默认 JWT」却没说这个默认要付出什么：

- 要么引入 `github.com/golang-jwt/jwt/v5`（成熟、审计充分）；
- 要么自己用 `crypto/hmac` 手写签名的 token（无依赖，但要自己处理
  alg 混淆、过期、吊销等一堆 JWT 的坑——不值得）。

**补强结论**：引入 `golang-jwt` 是正确选择，但它**违背了 §6.1
「不引入外部依赖」的精神**（那里说的"不引入"是指 Redis/Casbin 这种重依赖，
不是绝对零依赖）。文档应明确：**JWT 库是本次改造唯一新增的运行时依赖**，
并说明理由，避免实现者误以为要手写 token。

**2. 孤儿 key 的用户级配额行为没有定义。**

> **已解决（2026-10-05）**：本条审计意见提出时，方案 §5.1 选的是
> 「无归属 key 照常可用」，因此配额链需要为它写「跳过」分支。
> 用户随后决策改为**升级后不发新 key 就不给用**（见 §5.1 修订），
> 无归属 key 在迁移时即被禁用、鉴权时返回 401，
> 配额链不需要为它写任何分支。

**3. `users.used_tokens` 用 SUM 与 §4.8 的 `usage_totals` 单行表功能重叠。**

§4.3 说用户级 `used_tokens` 用实时 `SUM(total_tokens) WHERE user_id=?`；
§4.8 又建了一张 `usage_totals` 单行表存**全局**累计。
两者不冲突（一个是分用户、一个是全局），
但**分用户的累计在用户数 × 天数增长后，SUM 会越来越慢**——
和 §4.8 想解决的「全局累计慢」是同一个问题，只是推迟到了用户维度。

**补强结论**：若未来出现「单用户几十万条用量」，用户级 SUM 会重蹈全局的覆辙。
届时要么给 `usage_totals` 拆成「分用户」的多行表，要么加
`users.used_tokens` 的触发器维护（§4.3 明确否定了触发器，但那是在
「key 删除」语义下的否定；分用户累计没有 key 删除问题，触发器其实可用）。
**建议在 §4.3 补一句前瞻说明**：当前 SUM 够用，但保留了升级到
`user_usage_totals(user_id, ...)` 分用户累计表的路径。

**4. 「reload 原子性」的方案 b 依赖 `BuildFromStore` 返回失败项，
但现有签名不支持。**

§4.7 方案 b 说「让 `BuildFromStore` 返回哪些 provider 失败」。
我核对了 `snapshot/rebuild.go`——它现在的签名是 `RebuildFromDB(ctx, st) (*Snapshot, error)`，
**错误是「全有或全无」的**，没有「部分失败、部分成功」的通道。
池的 `BuildFromStore`（在 `internal/upstream`）需要确认是否能部分失败。

**补强结论**：方案 b 是对的，但它是一个**接口改造**，不只是加个开关。
`RebuildFromDB` 需要新增「失败 provider 列表」的返回，或引入一个可观测的
partial-failure 结构。文档应把这一点标为「需要改接口签名」，而非暗示是顺手的小改。

### 9.3 经得起推敲、无需改动的部分（审计确认）

以下是我重新核对后**站得住**的设计，列出来是为了让你知道哪些地方我验证过、不是拍脑袋：

1. **不引 Casbin**——new-api 源码 `service/authz/adapter.go` 确实在把 legacy
   scope 规则降级为 deny，`role.go` 只剩 root/admin 两个角色。证据充分。
2. **不引 Redis**——`snapshot` 的 `atomic.Pointer` + `Swap` 确实是进程内原子替换，
   `autoreload.go` 已经在用「管理写成功 → 自动重建」这条链。§4.4 说"版本栅栏多余"
   的逻辑成立。
3. **配额用触发器维护 `access_keys.used_tokens`**——这是现状（`store.go:194`
   `trg_update_used_tokens`），§4.8 复用同一范式是对的，不是发明。
4. **明细+归档两层 + 累计单行**——cc-switch 的 `usage_rollup.rs` 实证了这个方向，
   我补的 `usage_totals` 单行表是对「累计不受剪枝影响」这个需求的正确回应。
5. **`request_id` 恒 NULL**——已逐行核实（`usage_dao.go:37` INSERT 有列名、
   `:9-30` 结构体无赋值、全仓无 `RequestID:` 赋值点），结论可靠。
6. **默认档是「近 14 天」**——`Overview.vue:24-26` 实锤，剪枝不影响默认视图。

### 9.4 审计结论

方案**可以进入实施**，但必须先把 9.2 的四点补进正文（尤其是第 2 点，
它决定存量 key 升级后会不会全部 429）。

**优先级排序**：9.1 的三处笔误已在正文修正；9.2 的 1/2 是实施前必须补的
（依赖决策 + 迁移兼容规则），3/4 是实施中遇到再补的前瞻说明。

这份方案最大的价值不是列了什么，而是**列清楚"哪些是已验证的事实、
哪些是待验证的假设"**。审计后我确认：已验证的部分证据充分，
待验证的部分集中在接口改造（9.2-4）和依赖引入（9.2-1）这两处。