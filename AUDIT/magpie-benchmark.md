# rosetta-gateway × magpie 对照分析与借鉴建议

> 基线：`rosetta-gateway` @ 当前工作区（DESIGN.md / MULTIUSER.md / internal/**）
> 参考：`magpie 0.1.1157`（`C:\Users\louis\Downloads\magpie-0.1.1157`）
> 结论一句话：magpie 是**单机个人订阅聚合器**，rosetta-gateway 是**多用户计费网关**。
> 二者重叠面只在「路由 + 故障转移 + 上游协议适配」。magpie 在**调度策略**上远比
> rosetta 精细，rosetta 在**多租户/计费/审计**上远超 magpie。值得抄的是调度、
> 上游适配韧性、以及「决策留痕」工程方法；不该抄的是它的单机假设和插件体系
> （会与 rosetta 的强管控定位冲突）。

---

## 一、先说清两者的定位差异（避免误抄）

| 维度 | magpie | rosetta-gateway |
|---|---|---|
| 部署形态 | 单机桌面/终端，服务本机 agent | 局域网服务器，供多用户多客户端 |
| 状态 | `providers.json` + `affinity.json` + 内存态 | SQLite 单文件 + 不可变快照（`internal/snapshot`） |
| 上游身份 | provider / 订阅账号（OAuth 登录态）/ 插件账号 | provider + 多条 API Key（AES-GCM 加密） |
| 路由单位 | 账号级（key/account 级候选） | 目标级（provider+model 级 `route_targets`） |
| 计量 | 本地 quota 观察，不扣费 | 余额扣费 + 终身配额 + 固化单价 |
| 扩展性 | JS 插件（provider plugin + middleware） | 无插件，Go 编译期内建 |

---

## 二、最值得抄的 9 点（按性价比排序；含两条「确认不要重做」的对照项）

### 1. 账号级并发车道 + 排队（`internal/gateway/concurrency.go`）—— 最高优先

magpie 给**每一条 key / 每一个账号**一个「车道」：最多 N 个在途，超出的进 FIFO 队列
等释放，队列有上界（`QueueLimit`）和最长等待（`QueueWait`），超时返 429 + `Retry-After`。

> 关键语义（magpie 反复强调的）：
> - **排队 ≠ 失败**：在队列里等的请求不会被误判为「该 key 不可用」而触发故障转移；
> - 客户端中途断开时立刻把位置还回去（`Agent gone while it waits gives its place back`）；
> - **让位而非丢弃（`laneMate`，concurrency.go:159）**：某 key/账号满载时，同 provider
>   或同组内**有空闲槽的兄弟候选先上**，满者原地让位、不丢排队次序——避免「排队等待」
>   被 failover 逻辑误读为「该 key 不可用」。
>
> 另有一个配套技巧：**transport 层统一 RPM 计量**（`rpm.go:248` 的 `rpmTransport.RoundTrip`
> + context 携带 `rpmTicket`）——count_tokens、分类器、重试等**中间请求**自动计入同一
> 每分钟配额，业务循环零侵入。rosetta 的 `TestUpstreamModel` / `DiscoverUpstreamModels`
> 目前打上游的探活/发现请求**不占任何配额**，同样有「测试把生产 key 打爆」的风险。

rosetta 现状：`internal/ratelimit/limiter.go` 只做 **RPM/TPM 固定窗口**，**没有任何上游并发上限**。
后果是：多用户打同一个 provider 时，该 provider 的凭据池会被同时打满，上游返回 429
时 rosetta 只能靠 `MarkCredentialCooldown` 事后冷却，属于**事后补救**而非事前整形。

**建议**：`internal/upstream` 增加 `lanes`（keyID → {busy, queue}），在
`GetAnyClient` 之后、发请求之前 acquire。接口形状照抄 magpie：

```go
type lane struct { limit int; busy int; queue []chan struct{} }
func (p *Pool) AcquireLane(ctx context.Context, credID string) (release func(), waitMs int, err error)
```

`Concurrency` / `QueueSize` / `QueueWait` 挂在 `CredentialEntry` 上，快照读取，热路径无库查。
**注意**：限流是按用户维度（防滥用），并发车道是按上游凭据维度（防打爆上游），
两者语义正交，不要合并成一个东西。

---

### 2. 亲和性 Affinity —— 保住上游 prompt cache（`internal/gateway/affinity.go`）

magpie 让**一个会话固定落在回答过它的 key 上**，因为上游（Anthropic/OpenAI）的
prompt cache 是按 key+会话前缀走的；换 key 意味着整段历史重新计费。

判定非常克制，且是**有数据才动**：
- `cacheWorth = 1024` tokens：上一轮从缓存读到少于这个数 → 不值得固定；
- `cacheCold = 5 * time.Minute`：缓存 5 分钟不用就失效 → 直接放开；
- 亲和只决定「**谁先试**」，永远不做硬绑定；对方在冷却中 / 额度用尽 → 立刻让位。

**rosetta 可直接借**：rosetta 已有 `X-Request-ID` 与 usage 落库，加一张
`conversation_affinity(key_hash, upstream_target_id, last_cache_tokens, last_at)`，
或者更省事：按 `user_id + 虚拟模型 + 最近 30 分钟` 聚合出主力 target，优先它。

成本很低，收益是**缓存命中直接体现在 PriceCacheHit 与实际扣费上**，是可量化的。

---

### 3. 加权平滑轮询（`internal/gateway/weighted.go`）—— rosetta 有 Weight 但用得粗糙

rosetta `CredentialEntry.Weight` 已存在，`Pool.selectWeighted` 已在用；但 magpie 用的是
NGINX 式 **smooth weighted round robin**（权重 3:1 的出车序列是 a,a,b,a,a,a,b,a…），
比例在**最近若干次请求**内就成立，而不是只在长期平均上成立。

对多用户网关的意义：用户抱怨「我有时打到慢通道」本质就是随机轮询的短窗口不均匀。
**改造成本 ~20 行**（每个 provider 一个 `{curWeight map[credID]int; total int}`，
`total = 2*sum(weights)`，`current += weight`，选最大者，选中后 `-= total`）。

顺带抄一条：**冷却中的 key 直接从轮次里摘掉**，其余 key 按自己的权重分担它的份额，
恢复后再插回——rosetta 现在是把它排到最后（`getHealthyCredentials`），那会让
剩余 key 的实际权重虚高。

---

### 4. 熔断的 half-open 惊群抑制 —— rosetta 已做对，但可再进一步

rosetta `targetHealth.halfOpen` + `ClaimTargetProbe` 已经有**惊群保护**
（注释写得很清楚），这点比 magpie 的固定 1 分钟冷却更成熟。**不要改。**

rosetta **已经做了分类**（`cmd/gateway/main.go:1870-1888` 的 `out.credCooldown`
按 401/403 → 30min、402 → 1h、429/408/5xx/传输错 → 60s 分类），**不要重做**。
这里真正可补的是 magpie 的两条延伸：

| magpie 分类 | rosetta 现状 |
|---|---|
| 被限速 → 按上游 `Retry-After` 精确睡 | **缺**：`DESIGN.md:797` 明确写「Rosetta v0.5.1 未在 `APIError` 上暴露 `Retry-After`，故 429 统一按 60s 上限处理」 |
| 其他失败 1 分钟且**重复失败指数退避** | **缺**：定长 60s，无退避 |
| 欠费 30min / 额度等到 reset | 已有（401/403→30min、402→1h） |

所以这条的落地形态是：**向 Rosetta 上游提一个需求**（DESIGN 附录 B 已列待提需求）——
让 `APIError` 带上 `Retry-After`，然后把 429 的固定 60s 换成「按上游给的时长，上限 60s」。
上游说 5 秒就不该睡 60 秒（白扔 55 秒容量），上游说 300 秒则当前会**提前**叫醒它继续吃 429。

核心引擎报告补充了两个**比「分类」更深一层**的机制，一并记录：

- **三键分层 rest**（fallback.go:83-129）：`restKey()`（provider@账号）管账号级休息、
  `restID()`（restKey/model）管模型级休息、`who()`（provider#keyID）管计量。
  「该 key 的套餐不含此模型」只 rest 模型级键，**账号的其他模型照常服务**。
- **熔断的旁路唤醒**（routing.go:277 `renewed`，经 `provider.OnRenewed` 回调）：
  quota 读数发现**窗口已恢复**时立即解除 rest，不必干等到期。rest 只被「答案成功」
  或「读数恢复」关闭。rosetta 的 `cooldown_until` 落库但没有等价的主动唤醒路径——
  一个 429 冷却 60s 的 key，如果厂商侧其实 5 秒就恢复了，rosetta 仍要睡满。

另有一条**已经比 magpie 好、不要动**：`out.eligible && r.Context().Err() == nil`
——客户端中断时跳过全部记账，避免「几次用户中断把单 key provider 搞成整体不可用」。
magpie 是靠 `Agent gone while it waits gives its place back` 覆盖的，粒度更粗。

再补一条 magpie 的 **key×模型粒度降级**：上游说「这个模型不在你的套餐里」
（SenseNova 403）或「该模型已下线」（OpenCode Zen 410）时，**只把这个 key 上的这个模型**
标记不可用，该 key 的**其他模型仍正常服务**。rosetta 粒度只到 credential / target，
一个只有部分模型权限的 key 目前无法榨干剩余价值。

---

### 5. 「决策留痕」Trace —— 直接嫁接到 rosetta 的 usage 流水（magpie `internal/gateway/trace.go`）

rosetta 的 usage 明细是**计费账本**（30 天 + 日归档），magpie 的 trace 是**排障账本**
（Routing 页面上能看到：候选列表、谁被休息、休息多久、哪一次真的答了、为什么选它）。

两者互补。rosetta 的明细表现在缺三样运维最需要的东西：
1. **本次实际落到哪个上游凭据**（现在只有 provider，没有 credential 标签）；
2. **尝试过几次、每次的错误分类**（用户报「很慢」时无从判断是重试了 3 次还是一次成功）；
3. **本次请求的上下文构成**（system / tools / 历史各占多少 token，magpie 的
   `internal/gateway/prompt.go` + GUI `/api/context` 做的这件事极有价值）。

建议扩 usage 表三列：`target_id, credential_label, attempt_trace (JSON)`，
并在管理端加一列「展开」即可。**改动小、排障收益大，优先级应排在并发车道之后。**

---

### 6. 429 沉底 Sink（magpie `sink.go`）

`Retry-After` 部分已并入 §二.4（受 Rosetta SDK 限制，需先推上游）。

「沉底」本身值得抄，且核心引擎报告给出了它的**真实动机**：某账号**明明还有配额**却被 429
（厂商风控，非配额原因）时，把它沉到候选末尾、按时间戳排序——负载在账号间轮转，
**避免厂商风控持续盯着同一账号打**（magpie 的 01huadalang 案例）。这与 rosetta 的
`MarkCredentialCooldown`（直接移出健康集）是两种语义：前者是**降序**（仍可用，只是排后），
后者是**禁用**（60s 内完全不用）。rosetta 现在的做法在单 key 场景下等价于禁用，
在多 key 场景下则过于激进——一个被风控盯上的 key 应该「少用」而不是「不用」，
把流量匀给兄弟 key 反而能让它脱敏。

### 7. holdWriter 首字节缓冲（magpie `fallback.go:1006`）—— rosetta 已有等价物

magpie 用 `holdWriter` 缓冲流式响应，**首个内容字节写出之前的一切失败**（含 SSE 流中的
错误事件）都可撤回、无缝换下一个 provider——agent 永远只收到一份完整答案。

rosetta 的等价物：`attemptStream` 的「SSE 头延迟到收到首个上游事件才写」。
**已覆盖，不要重做**。这里列出只为对照确认 rosetta 语义完整：写出任何字节后绝不转移。

### 8. 插件化网关中间件（magpie `internal/middleware`）

magpie 用一个 Go 内嵌 JS 引擎（moejs）让用户在**不重编译**的前提下改请求：
`onRequest` / `onEvent`（SSE 逐事件）/ `onResponse` 三个钩子，**fail-open**（钩子抛异常
则原样放行），每钩子有毫秒级超时（250ms/50ms/1s），runtime 用 `sync.Pool` 复用，
实测 ~1µs/event。

**是否要抄取决于你的用户**：如果 rosetta-gateway 是自用/小团队，不必引入一个 JS 引擎
（安全面、依赖面、体积都是长期负债）。但**三个语义值得无条件抄**：
- **fail-open**：扩展失败不能拖垮主链路；
- **逐事件钩子**：能看到 SSE 每个 event，这是做「响应注入/审计」的唯一正确位置；
- **硬超时 + 计数 + 错误上限频（60 行/分钟）**：防止扩展自己刷爆日志。

如果确实需要可扩展性，先做**配置驱动的转换规则**（如「剥掉/改写请求里的某字段」、
「为某模型强制加 system prompt」），比整个插件系统便宜两个数量级。

### 9. 文档方法论：`LESSONS.md` + 每子系统一页「责任/事实源表」

magpie 每个子系统文档开头都是一张三列表：`Part | Responsibility | Source(带 file:link)`，
然后 Runtime path（编号步骤）、Constraints and failure behavior、Verification（可直接粘贴的
`go test -run` 命令）。

**这比 rosetta 的 DESIGN.md 更适合长期演进**。rosetta 的 DESIGN.md 已有 §2「关键决策速览」
和大量「为什么」注释（质量其实很高，注释里甚至写了「为什么索引不是可选优化」这种），
但**缺一张「哪个子系统/文件是某个行为的唯一事实源」的索引**——新维护者只能全文搜索 100KB。
建议给 rosetta 也补一层 `docs/subsystems/*.md` 索引页，每页就一张表。

magpie 的 **LESSONS.md 机制**值得单独一提：每晚对当日合并的提交做评审，把「改了又改的」
提炼成「规则 + 为什么 + 证据（commit/issue）」，**同一教训出现 3 天自动升格进
code-standards.md**。这不是普通的 changelog，而是一个**会自我提纯的规则沉淀管道**。
rosetta 的 AUDIT/ 目录已有类似雏形（test-summary、回归记录），但缺「教训 → 规则 →
标准」的升格机制。如果本项目后续也是 agent 主导开发，这一条的价值不亚于任何代码层借鉴。

magpie 教训库里与本仓库直接相关的代表性规则（来自 LESSONS.md，均带证据）：
- **修复要覆盖整类而非样本**（#834 回归 4 次的根因：每轮只修截图里那个 agent）；
- **隐私/安全豁免按协议位置判定，绝不按 key 名**（曾因 key 叫 `signature` 就豁免，
  导致 tool arguments 未掩码外泄）——rosetta 的审计日志「只记字段名不记值」与之同构；
- **失败读取 = 未知而非空**（providers.json 读不出来时 `ErrUnreadable` ≠ 空列表）；
- **fixture 用报告者的原始字节，不要自己造相似的**（自造的 `<tool_call>{json}` 样本
  「证明」了错误的修复）。

---

## 三、明确**不该**抄的部分

| magpie 的东西 | 为什么不抄 |
|---|---|
| 桌面 GUI / TUI / 托盘 / Wails | rosetta 是服务端 Web，形态不同；`internal/webui` 的 Vue 方案更合适 |
| OAuth 订阅账号（读 Codex/Claude 登录态） | 局域网网关读客户端本机凭据是**越权**且不可运维；rosetta 用管理员配置的 API Key 是对的 |
| 从环境变量读 Key（magpie 明确不读） | 同上，rosetta 保持加密落库 |
| 全局状态散在多个 JSON（providers.json / affinity.json / retired.json / logins.json） | rosetta 的**单一 SQLite + 不可变快照**是明显更优的架构，不要倒退 |
| DNS rebinding / CORS 白名单那一套浏览器防护 | rosetta 已有 `sameOrigin` + CSRF；但**外网暴露时应补一个 host 白名单**——见下节风险 |
| 每 provider 一套 `MaxConcurrency` GUI 调参 | rosetta 已有全局 `app_settings` 策略参数，且刻意不做 per-route 覆盖（避免隐形配置）。这个决定是对的，别改 |
| **完整插件生态**（npm 包 + Bun host + 市场 + built-in→插件迁移） | rosetta 是内网受管服务，扩展靠发版；插件生态的供应链面（npm 安装、市场分发）对多用户计费网关是**负资产**。§二.8 已单独提取三个值得抄的语义 |
| **Session/library 层**（读 60+ agent 原生会话文件、MCP/skills 下发） | magpie 的核心产品面是「管本机 agent」；rosetta 的边界刻意不含 Agent 编排（DESIGN §1），不应跨界 |
| **WebDAV/S3 多机同步、自动更新、autostart** | 单二进制 + Docker + `config-export` 已覆盖 rosetta 的部署形态；多机状态同步反而会破坏「DB 是唯一事实来源」（D4） |

### 已核对为「rosetta 已有等价物」的项（不要重做）

- **加密导出**：magpie 的 PBKDF2 600k + AES-GCM 备份口令包裹 ↔ rosetta `internal/admin/export_format.go`
  的 `sealExport`/`openExport`（PBKDF2 + AES-256-GCM，口令 ≥8 位、无口令一律 400、
  错口令与坏密文不可区分）。**rosetta 做得同样完整，且多了「导出必含凭据+必加密」的强约束。**
- **Docker healthcheck**：rosetta `Dockerfile:82` 已有（`/admin/` 无鉴权确定性 200）。
- **`--json` CLI 面**：rosetta 是 Web-first，管理面在浏览器；magpie 的 CLI 体系是其桌面形态的产物。

---

## 四、rosetta 现存的真实缺口（按收益排序）

1. **上游并发整形缺失** —— 见 §二.1，多用户共享凭据时必然互相拖累，且 429 是事后冷却。
2. **无亲和性** —— 每个 provider 多 key 时缓存命中率无保障，实际账单比理论高。
3. **429 冷却定长 60s + 无指数退避** —— 且受限于 Rosetta SDK 未暴露 `Retry-After`
   （`DESIGN.md:797`），需先推上游 SDK 才能精确化。冷却分类本身**已有**，勿重做。
4. **usage 缺少 attempt/target/credential 维度** —— 线上问题不可复盘。
5. **key×model 粒度不可用** —— 部分套餐的 key 无法榨干。
6. **429 是禁用而非降序**（`MarkCredentialCooldown` 移出健康集 vs magpie 的 sink 沉底）
   —— 多 key provider 下一个被限速的 key 会连带损失容量，且风控盯防无法靠分流脱敏。
7. **冷却无旁路唤醒** —— `cooldown_until` 落库但没有任何「发现上游已恢复就提前解除」
   的路径；magpie 靠 `renewed()`（quota 读数恢复即解 rest）避免白睡。
8. **测试/发现类请求不占上游配额** —— `TestUpstreamModel` / `DiscoverUpstreamModels`
   打的是真实 key，但不在任何计量内；magpie 用 `rpmTransport` 把一切中间请求统一计数。
9. **`internal/ratelimit` 单锁 + 全 map 扫描**（当前 5 分钟 sweep 一次，注释已承认）。
   key 数量上千后会有争用；`sweep` 逻辑本身没问题，但 map 分片（`hash(keyID) % N`）
   可以作为后续优化项，非紧急。
10. **外网暴露缺 host 校验** —— `sameOrigin` 已挡跨站，但 DNS rebinding（攻击者域名
    解析到 127.0.0.1）仍可绕过。magpie 专门为此写了 `lanGuard.rebound`。若 rosetta 要
    前置反向代理暴露公网，这一条应补。

> 注：测绘另发现三处**与 magpie 无关但更要紧**的既有问题，此处仅记录以免遗漏——
> 写池单连接把吞吐钉在落库速率上（实测瓶颈 38.8×）、落库与扣费是两个事务
> （SIGKILL 会丢一次扣费）、`audit.actor` 恒为 `"admin"`（审计无法区分操作者）。
> 这三条的建议优先级**高于**本文所有 magpie 借鉴项。

---

## 五、建议的落地顺序（两个 sprint）

**Sprint A（低风险、高收益，不动架构）**
1. 推 Rosetta SDK 暴露 `Retry-After`，429 冷却改为「按上游时长、上限 60s」+ 指数退避
   （§二.4；**注意这是跨仓库改动，先确认 Rosetta 侧排期**）
2. usage 表加 `target_id / credential_label / attempt_trace`（§二.5）
3. 亲和性表 + 优先命中缓存的 target（§二.2）
4. 测试/发现请求计入上游 RPM 计量（§二.1 的 rpmTransport 思路；改动小、防自伤）

**Sprint B（需要设计评审）**
5. 上游并发车道 `lanes`（§二.1）—— 需定义「排队等待算不算超时」「客户端断连如何归还位置」
6. key×model 降级粒度（§二.4 后半）
7. smooth WRR 替换随机加权（§二.3）+ 冷却中 key 从轮次摘除
8. 429 沉底语义（§二.6）—— 与 6 有交互，需一并评审

**Sprint C（可选）**
9. `docs/subsystems/` 索引层（§二.9）
10. 配置驱动的请求改写钩子，而非完整插件系统（§二.8）