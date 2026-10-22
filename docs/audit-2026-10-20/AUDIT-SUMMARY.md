# Rosetta Gateway 全面系统设计审计汇总

- 审计日期：2026-10-20
- 仓库：`rosetta-gateway`
- 基线：`main` = `ea19ce1`（领先 `origin/main` 2 个提交）
- 审计性质：只读审计，不修改生产代码，不执行 Git 写操作
- 组织方式：主协调者拆分 11 个互有交叉引用、但尽量避免重复的子域，全部取证与结论由子 agent 完成
- 明细报告：`work/audit-2026-10-20/A1-*.md` 至 `A11-*.md`

---

## 0. 执行结论

这不是“再修几个函数”就能恢复稳定的项目。

11 个子域合计给出 **200 条分级问题**（含测试透镜中的重复项）。按统一的生产影响口径去重后，确认 **12 个生产 P0、50 个生产 P1**；另有 **6 个测试/CI P1 缺口**和若干关键 P2 边界问题。问题集中在资金事务原子性、协议状态机、路由并发、前端/后端契约、部署安全和测试门禁。

当前最重要的判断有五条：

1. **资金正确性已经失去一致性边界。** `balance_pending` 的结算不是单事务，存在重复结转、重复扣款、漏收和把“无限额”改成“有限额”的路径。
2. **“上游已计费、用户应收”之间没有可靠闭环。** 重算不补扣余额、累计表不更新、归档销毁待重算标记、charge 吞错，且没有对账任务。
3. **配额预占没有崩溃恢复契约。** `reserved_tokens` 是终身预占；一次崩溃或强杀可永久冻结 key/user 的终身额度。TPM 还有一条早退路径漏退预占。
4. **协议层和路由层仍有静默语义漂移。** `reasoning_effort=max` 被静默降成“不思考”；健康上游目标被 half-open 全局互斥位串行化；跨协议编号空间和流式状态机存在串味。
5. **“测试全绿”不能证明系统正确。** 多条测试把错误行为固化成期望，或复制生产分支、正则提取路由、只测单 Store。现有 CI 没有真实 mux、前端契约、崩溃恢复和跨 Store 资金并发门禁。

---

## 1. 覆盖范围与原始统计

| 报告       | 子域                            |     P0 |     P1 |     P2 |     P3 | 明细                                                   |
| -------- | ----------------------------- | -----: | -----: | -----: | -----: | ---------------------------------------------------- |
| A1       | 系统架构、模块边界、文档一致性、改动根因          |      0 |      3 |      9 |      8 | [A1-architecture.md](./A1-architecture.md)           |
| A2       | 入站协议 `internal/inwire`、effort |      1 |      6 |     10 |      4 | [A2-inwire.md](./A2-inwire.md)                       |
| A3       | 出站协议 `internal/outwire`       |      0 |      6 |      8 |      4 | [A3-outwire.md](./A3-outwire.md)                     |
| A4       | 请求热路径、预占/释放、usage、计费          |      3 |      6 |      7 |      5 | [A4-hotpath-billing.md](./A4-hotpath-billing.md)     |
| A5       | 上游池、路由、快照、探测、限速               |      1 |      3 |      6 |      3 | [A5-upstream-runtime.md](./A5-upstream-runtime.md)   |
| A6       | SQLite schema、迁移、归档、DAO       |      2 |      5 |      6 |      3 | [A6-store-schema-dao.md](./A6-store-schema-dao.md)   |
| A7       | 管理面 API、鉴权、会话、权限边界            |      0 |      5 |     12 |      3 | [A7-admin-auth.md](./A7-admin-auth.md)               |
| A8       | 资金 DAO 并发、事务、原子性              |      4 |      7 |      3 |      1 | [A8-money-concurrency.md](./A8-money-concurrency.md) |
| A9       | 前端工程、视图完整性与 API 契约            |      0 |      4 |      9 |      7 | [A9-frontend.md](./A9-frontend.md)                   |
| A10      | 安全、Docker/部署、密钥、供应链           |      1 |      8 |      7 |      6 | [A10-security-deploy.md](./A10-security-deploy.md)   |
| A11      | 测试真实性、CI/发布门禁                 |      2 |      6 |      5 |      1 | [A11-tests-ci.md](./A11-tests-ci.md)                 |
| **原始合计** |                               | **14** | **59** | **82** | **45** | **200**                                              |

说明：

- A11 是“测试透镜”，两条 P0 分别复证 A6 的归档边界与 A4/A6/A8 的费用生命周期；不能把 A1-A11 原始数量直接当成 200 个互不相关的生产缺陷。
- 原始 59 个 P1 去重后是 **50 个生产 P1**。A6-P1-2、A8-P1-2、A8-P1-3 被提升并并入生产 P0；A11 的 6 个 P1 是测试/CI 缺口，单独列在 §3.10。
- A7/A9/A10/A11 还交叉覆盖了 Origin、可信代理 IP、审计 actor、Docker race 门禁、前端产物这些问题。总表已合并同源项。
- 去重后的生产 P0 为 **12 条**。P1/P2/P3 保留子域编号，便于从汇总跳回证据。

---

## 2. 去重后的生产 P0

排序原则：资金/数据一致性优先，其后是安全、可用性、协议语义。

| #     | 问题                                                                                      | 关键证据                                                                                                                    | 来源 / 交叉          |
| ----- | --------------------------------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------- | ---------------- |
| P0-1  | 并发充值把同一笔 `balance_pending` 重复结转，多收用户钱；实测复现率 59/60                                       | `internal/store/balance_dao.go:564-589`、`internal/store/topup_dao.go:225`                                               | A8-P0-1          |
| P0-2  | 结算 UPDATE 与整表 DELETE 之间存在崩溃窗口，同一笔欠款可被扣两次                                                | `internal/store/balance_dao.go:577-587`                                                                                 | A8-P0-2          |
| P0-3  | `COALESCE(balance_cents,0)` 把 NULL（不限额）当 0，并发扣费会把无限额用户静默切成有限额                           | `internal/store/balance_dao.go:578-582`                                                                                 | A8-P0-3          |
| P0-4  | `DELETE ... WHERE user_id=?` 抹掉结算期间并发写入的新欠款，实测复现率 60/60                                 | `internal/store/balance_dao.go:585`                                                                                     | A8-P0-4、A6-P1-2  |
| P0-5  | `cost_pending` 重算只更新 `cost_total`，不调用 `ChargeBalance`；一次读池抖动即永久免单                       | `internal/store/usage_dao.go:510-512`、`cmd/gateway/main.go:3408`                                                        | A4-P0-2、A11-P0-2 |
| P0-6  | `RecomputeCost` 只改明细表，不更新 `usage_totals`；累计费用永久吞掉                                       | `internal/store/usage_dao.go:489-521`、`internal/store/store.go` 的 AFTER INSERT 触发器                                      | A6-P0-2、A8-P1-3  |
| P0-7  | 归档支“整日包含”边界遇到非午夜 `To` 会丢掉窗口边缘整日用量；真实数据实测丢失 87% token                                    | `internal/store/usage_source.go:117-120`、`cmd/gateway/billing.go:129`、`internal/admin/usage_handler.go:76-107`          | A6-P0-1、A11-P0-1 |
| P0-8  | key 级配额超限早退漏调 `rate.commit(0)`，TPM 预占在窗口内永久不退还                                          | `cmd/gateway/main.go:1857`                                                                                              | A4-P0-1          |
| P0-9  | `reserved_tokens` 是终身预占，进程崩溃/强杀后无复位路径；一次崩溃永久冻结 key/user 终身额度                            | `internal/store/key_dao.go:507-513`、`internal/store/user_dao.go:617-623`、`internal/store/store.go:234,285`              | A4-P0-3、A8-P1-2  |
| P0-10 | Docker“推荐部署”公开监听明文 HTTP，管理员会话、JWT、bootstrap token 与上游 key 可被同网段截获                       | `docker/config.default.json:1-3`、`README.md:34-50`、`internal/server/user_auth.go:399-411`、`cmd/gateway/main.go:668-690` | A10-P0-1         |
| P0-11 | `ClaimTargetProbe` 的 half-open 互斥位作用于健康目标，开启 failover 后稳态并发被压成 1，其余请求静默改投第二目标           | `internal/upstream/upstream.go:564-578`                                                                                 | A5-P0-1、A11-P1-1 |
| P0-12 | `reasoning_effort:"max"` 被私有解析器判成 unset，上游 payload 完全不带 effort；`very_high`/`min` 等也静默降级 | `internal/inwire/openai_chat.go:632`、`internal/effort/effort.go`                                                        | A2-P0-1          |

---

## 3. P1 主清单与关键边界问题

本节列出 **50 个去重生产 P1**：

`架构 3 + 资金链 6 + 余额配额 5 + 存储完整性 4 + 入站 6 + 出站 6 + 上游运行时 3 + 管理面 5 + 前端 4 + 安全部署 8 = 50`。

会直接放大资金、可用性或数据正确性问题的关键 P2 另列于 §3.10。测试/CI 缺口另列于 §3.11；两者都不计入 50 个生产 P1。

### 3.0 架构与规格真源

| 编号 | 问题                                                                   | 证据                                                                                                          |
| -- | -------------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------- |
| G1 | DESIGN §10 声称健康态不落库、重启清零，实际凭据冷却持久化到 SQLite 并重建时读回                    | `DESIGN.md:1034`、`internal/upstream/upstream.go:463-495,354-366`、`internal/store/credential_dao.go:115-123` |
| G2 | DESIGN §11.4 声称 RPM 被拒同样计数，实现明确不计数；代码注释已纠正，规格未改                      | `DESIGN.md:1129`、`internal/ratelimit/limiter.go:85-98`、`cmd/gateway/main.go:1750-1756`                      |
| G3 | `internal/webui/dist` 当前新旧资产并存、embed 产物与源码不同步；可直接构建但产物体依赖未跟踪文件，CI 会红 | `internal/webui/dist/index.html:26`、`.github/workflows/ci.yml:208-234`                                      |

### 3.1 请求热路径与资金链

| 编号 | 问题                                                                                         | 证据                                                                     |
| -- | ------------------------------------------------------------------------------------------ | ---------------------------------------------------------------------- |
| F1 | 客户端中断流式请求时 usage 恒为零值：用户可免费消耗已生成 token，同时绕过终身 token 配额                                     | `cmd/gateway/main.go:2795-2800,2853-2878`                              |
| F2 | TPM 校正用 `TotalTokens`，费用用 `Input+Output`；Anthropic cache creation 无独立价格且 cached token 归档漏项 | `cmd/gateway/main.go:2889,2956`、`internal/store/usage_dao.go:332-352`  |
| F3 | 余额预检与实际扣费使用两份计价公式，缓存价模型可 fail-open 放行                                                      | `cmd/gateway/billing.go:492-505`、`internal/store/usage_dao.go:337-340` |
| F4 | 落库成功、扣费前崩溃时没有字段标记“未扣费”，全仓无对账任务                                                             | `cmd/gateway/main.go:3293-3299,3408-3422`                              |
| F5 | `flushOneByOne` 把“主键冲突”等同于“已扣费”，Commit 返回错误但实际已提交时会跳过 charge                               | `cmd/gateway/main.go:3323-3328`                                        |
| F6 | `charge` 吞掉 `ChargeBalance` 的所有错误，DB 抖动即永久漏收，记录仍看似正常                                       | `cmd/gateway/main.go:3408-3422`                                        |

### 3.2 余额、配额与聚合

| 编号 | 问题                                                                                     | 证据                                                                                                              |
| -- | -------------------------------------------------------------------------------------- | --------------------------------------------------------------------------------------------------------------- |
| M0 | 配额“先退后记账”：预占同步退回、usage 落库异步，终身配额可超发（quota 1000 可放行 2000 token）                         | `cmd/gateway/main.go:2853,2889`                                                                                 |
| M1 | 多实例/双 Store 打开同一 SQLite 时，`ReserveQuota` 的读先行触发 `SQLITE_BUSY_SNAPSHOT`，fail-open 使配额失效 | `internal/store/key_dao.go:452-489`                                                                             |
| M2 | 报表计入失败请求费用，扣费只算 `status='ok'`；余额与报表口径不一致                                               | `internal/store/usage_by_user_dao.go:101`、`cmd/gateway/main.go:3384-3386`、`internal/store/balance_dao.go:12-15` |
| M3 | `UsageByUser` 的总用户数和行数来自两条独立读池查询，不同快照下互相矛盾                                             | `internal/store/usage_by_user_dao.go:83-85,114`                                                                 |
| M5 | `cost_pending` 标记在归档表无对应列，剪枝后永久丢失，待重算费用归零                                              | `internal/store/usage_archive.go:630-672`                                                                       |

### 3.3 存储、迁移与数据完整性

| 编号 | 问题                                                  | 证据           |
| -- | --------------------------------------------------- | ------------ |
| S1 | `backfillProxyNetworks` 用未 trim 原文回填，带首尾空白的代理配置静默丢失 | 详见 `A6-P1-1` |
| S2 | `usage_records` 无外键，删 key/user 后留下无法解释的孤儿用量         | `A6-P1-3`    |
| S3 | `cmd/migrate-legacy` 与运行时迁移是两套逻辑，且都漏 `network_id`   | `A6-P1-4`    |
| S4 | `route_targets` 单条增删改 API 无人调用，并违反自身不变量             | `A6-P1-5`    |

### 3.4 入站协议与 effort

| 编号 | 问题                                                                 | 证据                                                               |
| -- | ------------------------------------------------------------------ | ---------------------------------------------------------------- |
| I1 | effort 有 `internal/effort`、`inwire.parseEffort`、Anthropic 预算折算三套口径 | `internal/inwire/openai_chat.go:632`、`internal/effort/effort.go` |
| I2 | inwire 归一后的 effort 被数据面丢弃，Clamp 在多数情况下不生效                          | `cmd/gateway/main.go` effort 装配链                                 |
| I3 | Anthropic 入站 thinking 预算被静默改写，幅度可达 12 倍                            | `internal/inwire/anthropic.go` 预算折算                              |
| I4 | Anthropic `tool_result` 内媒体块被静默丢弃                                  | `A2-P1-4`                                                        |
| I5 | Responses `input_file` 只带 `file_id` 时整个请求被判死                       | `A2-P1-5`                                                        |
| I6 | Responses `tool_choice` 指向 namespace 时约束被静默丢弃                      | `A2-P1-6`                                                        |

### 3.5 出站协议

| 编号 | 问题                                                                                | 证据                                                                  |
| -- | --------------------------------------------------------------------------------- | ------------------------------------------------------------------- |
| O1 | Anthropic content-block index 被直接当 OpenAI `tool_call.index`，首个工具可下发 `index:1`     | `internal/outwire/openai_chat.go:234`、`internal/outwire/sink.go:96` |
| O2 | Responses reasoning 状态机不支持多个 reasoning item，文本和 sealed 载荷串到同一 item                | `internal/outwire/openai_responses.go:70-75`                        |
| O3 | Responses 流式 message 文本 builder 跨 item 不 Reset，第二段文本被污染为累计值                       | `internal/outwire/openai_responses.go:411`                          |
| O4 | Responses sink 忽略 `Event.OutputIndex`，下游 `output_index` 按到达顺序自分配，可能错位/不连续         | `internal/outwire/openai_responses.go` 全篇                           |
| O5 | Anthropic `tool_use.input` 非合法 JSON 时编码错误被丢弃，客户端收到 200 + 零字节                      | `internal/outwire/anthropic.go:150`                                 |
| O6 | Responses usage 缺 `output_tokens_details.reasoning_tokens`，思考 token 无法在 API 响应中查询 | `internal/outwire/openai_responses.go:491`                          |

### 3.6 上游池、路由和探测

| 编号 | 问题                                                      | 证据                              |
| -- | ------------------------------------------------------- | ------------------------------- |
| U1 | `ParseBalanceSpec` 只查 `/` 前缀，未做路径规范化，强度低于注释承诺           | `internal/upstream/balance.go`  |
| U2 | `liveTargetIDs` 读失败返回空集，`Install` 会清空全站熔断状态，Reload 仍报成功 | `internal/upstream/upstream.go` |
| U3 | 运维显式关闭目标级熔断（threshold=0）后，half-open 全局互斥仍生效             | `internal/upstream/upstream.go` |

### 3.7 管理面鉴权、会话与审计

| 编号 | 问题                                                  | 证据                                                                                          |
| -- | --------------------------------------------------- | ------------------------------------------------------------------------------------------- |
| A1 | 角色/状态更新已提交，但 auth_version 吊销失败时只返回 500；旧 token 继续有效 | `internal/admin/user_admin_handler.go:491-506`、`internal/store/user_dao.go:259-278,455-465` |
| A2 | Logout 服务端吊销失败仍返回 200，客户端认为已退出但复制 token 仍可用         | `internal/admin/user_handler.go:630-650`、`web/src/api.ts:191-198`                           |
| A3 | `sameOrigin` 只比较 host，不比较 scheme；Cookie 又未设 Secure  | `internal/server/server.go:146-174`、`internal/server/user_auth.go:323-357`                  |
| A4 | Bootstrap 失败不写审计，错误令牌和重复初始化攻击信号丢失                   | `internal/server/autoreload.go:187-195`                                                     |
| A5 | 不可逆 usage prune 的 `keep_days` 不进入审计，且与 handler 注释矛盾 | `internal/admin/usage_handler.go:944-982`                                                   |

### 3.8 前端

| 编号 | 问题                                                                                  | 证据                                                                                        |
| -- | ----------------------------------------------------------------------------------- | ----------------------------------------------------------------------------------------- |
| W1 | `/usage/by-day` 用 `ORDER BY key ASC + LIMIT 100`，截断最早 100 天；近 1 年/全部/钱包 90 天静默丢最近数据 | `internal/admin/usage_handler.go:889,896-899`、`web/src/api.ts:774`                        |
| W2 | by-* 四个端点有两套默认窗口语义，`usageByDay` 与另外三个签名形状不同                                         | `internal/admin/usage_handler.go:884,286`、`web/src/api.ts:774,779-784`                    |
| W3 | 搜索框每次击键改写 URL，与“草稿 + 应用”模式并存，`clearFilters` 又复制清 query 规则                           | `web/src/composables/useQueryText.ts:6-14`                                                |
| W4 | ProviderDetail 拉全表再 find；注释称后端没有单条 GET，实际有但缺 `ready`/`ready_reason`                 | `web/src/views/ProviderDetail.vue:54-77`、`internal/admin/provider_handler.go:173-181,292` |

### 3.9 安全与部署

| 编号 | 问题                                        | 证据                                                                                  |
| -- | ----------------------------------------- | ----------------------------------------------------------------------------------- |
| D1 | bootstrap token 明文进入长期日志，轮转/聚合后可恢复管理员初始化权 | A10-P1-1                                                                            |
| D2 | SQLite 状态目录和 DB 文件权限过宽，同机其他账号可读敏感数据       | A10-P1-3                                                                            |
| D3 | 免鉴权 `/healthz` 回显底层 DB 错误，泄露路径和驱动内部信息     | A10-P1-4                                                                            |
| D4 | Docker 发布不校验 embed UI，tag/手动触发可发布旧前端      | A10-P1-5、A11-P2-1                                                                   |
| D5 | 发布 workflow 绕过 `-race`，镜像可在 race 未通过时发布   | A10-P1-7、A11-P2-1                                                                   |
| D6 | 文档备份只备份 SQLite，恢复不了 `master.key`、会话密钥和配置  | A10-P1-6                                                                            |
| D7 | lockfile 版本漂移、基础镜像浮动 tag，发布制品不可复现         | A10-P1-8、A11-P2-2                                                                   |
| D8 | 反向代理后没有可信客户端 IP 契约，IP 白名单与登录限速都退化为代理地址    | `internal/server/server.go:187-204`、`internal/server/user_auth.go:270-295`、A10-P1-2 |


### 3.10 关键 P2 边界问题

这些不是原始统计中的 P1，但会直接放大资金、可用性或数据正确性风险，修复阶段应随对应 P0/P1 一起处理：

| 编号   | 问题                                                                    | 证据                                                                                                 |
| ---- | --------------------------------------------------------------------- | -------------------------------------------------------------------------------------------------- |
| *X0  | 退出 drain 超时后 `stillPending` 记录永久丢失；后台 worker 仍用不可取消 context 与 DB 关闭并发 | `cmd/gateway/main.go:876-886,3170-3184`                                                            |
| *X1  | 探测与熔断是两套健康真相；admin 层复制 `Classify` 且阈值硬编码，配置会静默分叉                      | `internal/probe/runner.go`、`internal/admin`                                                        |
| *X2  | 余额查询与模型发现 client 不读 provider 的出站网络，与数据面网络口径相反                         | `internal/upstream/balance.go`、模型发现路径                                                              |
| *X3  | `balance_topups.balance_after` 记录结转前的过期值，充值流水不可直接用于对账                 | `internal/store/topup_dao.go:201-213,225`                                                          |
| *X4  | `GetRecentPerformance` 绕过 `UsageSource`，剪枝后近期表现样本归零                   | `internal/store/recent_performance_dao.go:24-38`                                                   |
| *X5  | 管理路由授权契约分散，新增端点容易漏掉对象级收窄                                              | A7-P2-01                                                                                           |
| *X6  | Group PATCH 会清空 description；Network/ModelGroup PATCH 有错误的字段必填耦合       | A7-P2-07、A7-P2-10/11                                                                               |
| *X7  | 管理 handler 的 PATCH 合并靠注释；Key 认领有部分提交窗口                                | A7-P2-08                                                                                           |
| *X8  | 前端 Go struct、`types.ts`、`api.ts` 三处手写、零机器校验；`as T` 不校验运行时形状           | `web/src/api.ts:248`                                                                               |
| *X9  | `api.ts` 的 `mutate()` 是空函数，20 个写操作仍包着它，抹掉了 reload 知识                  | A9-P2-5                                                                                            |
| *X10 | Docker 降权失败后继续以 root 运行；healthcheck 写死 8666                           | A10-P2-2/3                                                                                         |
| *X11 | TTFT 超时被优先判成上游流错误并冷却 60s；网关自身总预算超时也记目标失败                              | `cmd/gateway/main.go:2202-2203,2678-2685,2824-2828`                                                |
| *X12 | SSE 七个写点全部丢弃 `fmt.Fprintf` 错误，残缺流被判成功，handler/上游流可泄漏                  | `internal/outwire/openai_chat.go:120,122,136`、`anthropic.go:255,277`、`openai_responses.go:131,504` |
| *X13 | `route_targets` 无唯一约束且 Replace 不查重，同一上游可被配置成两个 position，重复计费          | `internal/store/route_target_dao.go:161-177`、`internal/admin/route_target_handler.go:104-120`      |
| *X14 | “该 provider 无健康凭据”被记成目标级失败，冷却与熔断两套惩罚叠加                                | `cmd/gateway/main.go:2136-2142`                                                                    |
| *X15 | `routing.RouteIndex` 无并发保护，已发布快照仍可被导出方法原地修改                           | `internal/routing/routing.go`                                                                      |
| *X16 | 路由链顺序只由单条 SQL 的 `ORDER BY` 保证，`Candidate.Position` 从未读取               | `internal/routing/routing.go`、`internal/store/route_target_dao.go`                                 |
| *X17 | 欠费路径三步非原子；空 `request_id` 时余额已扣，流水和欠款都未记                               | `internal/store/balance_dao.go:497-527`                                                            |

### 3.11 测试与 CI 缺口

以下 6 项对应 A11 的 P1。它们不重复计入 50 个生产 P1，但正是本次大量缺陷未被现有测试发现的原因：

| 编号 | 缺口                                    | 证据 / 影响                                                                                            |
| -- | ------------------------------------- | -------------------------------------------------------------------------------------------------- |
| T1 | half-open 互斥只有单 goroutine 顺序测试，没有并发交错 | `internal/upstream/pool_test.go:105-129`；无法发现旧请求释放新探测名额                                            |
| T2 | 路由矩阵从 `main.go` 源码正则提取路由，没有执行真实生产 mux | `internal/server/authz_matrix_test.go:88-148`；端点漏注册仍可全绿                                            |
| T3 | 下游 SSE 写失败没有测试；deadline 测试只断言能力绑定     | `internal/outwire/write_deadline_test.go:22-58`；残缺流/泄漏无门禁                                          |
| T4 | 链耗尽测试在测试里复制生产 `0/200 -> 502` 分支       | `cmd/gateway/usage_shutdown_test.go:165-181`；生产分支改坏仍绿                                              |
| T5 | `reserved_tokens` 完全没有崩溃/重开恢复测试       | `internal/store/quota_reserve_test.go:156-210`、`internal/store/user_quota_reserve_test.go:311-363` |
| T6 | 双 Store 测试只覆盖扣费，没有覆盖配额预占竞争            | `internal/store/stress_billing_test.go:1265-1343`、`internal/store/stress_quota_test.go:99-143`     |

---

## 4. 为什么“修一点不管用”

### 4.1 根因一：规格来源已经开始说谎

A1 已确认 `DESIGN.md` 的关键章节与实现相反：

- §10 声称健康态“不落库、重启即清零”，代码却把凭据冷却持久化到 `provider_credentials`。
- §11.4 声称 RPM 被拒绝也计数，代码明确不计数。
- §13.2 路由清单与 `web/src/router.ts` 大面积不符。
- §15 目录结构、§3 架构图（`quota` 包）、§6.3 端点表都已漂移。

这使后续 agent 的起点是错误规格。局部修复越努力，越可能围绕错误模型做补丁。

### 4.2 根因二：`main.go` 是单点真相，缺少可测试边界

`cmd/gateway/main.go` 约 3532 行，承担路由、鉴权、预占、限速、failover、编解码注册、usage 记账、计费预检、优雅关闭和后台循环等 12 类职责。

典型后果：某次改动同时改了 schema、DAO、handler、前端，却漏了 `main.go` 的端点注册；build、vet、测试全绿，实际接口不可达。路由矩阵测试又从源码正则提取路由，而不是启动真实生产 mux，因此没有发现。

### 4.3 根因三：事务边界靠注释保证，不靠数据库和状态机

资金代码大量写着“原子”“免疫 check-then-act”，但成立前提是“那条 UPDATE 是事务的第一条写语句”。一旦先 SELECT，SQLite 的 `SQLITE_BUSY_SNAPSHOT` 和并发窗口就重新出现。

仓库没有统一的资金状态机，也没有对账任务：

```text
usage 生成 -> 落库 -> 计价 -> 扣余额 -> 累计表 -> 归档 -> 重算
```

每一跳都可能失败，失败只记日志。结果包括免单、少收、多收、重复扣、永久冻结额度和累计表偏差。

### 4.4 根因四：跨层契约全部手工复制

以下契约没有机器校验：

- Go response struct ↔ `web/src/types.ts`
- Go handler 路由 ↔ `web/src/api.ts` 路径
- inwire 统一事件 ↔ outwire 三套协议字段
- Anthropic content index ↔ OpenAI tool index ↔ Responses output index
- `web/src` ↔ `internal/webui/dist`
- `package.json` ↔ `package-lock.json`

每次改动都要人工同时记住 2 到 6 个位置，这正是多 agent 改动后“补一处、坏一处”的机制来源。

### 4.5 根因五：测试把现状固化成正确性

典型例子：

- `internal/upstream/pool_test.go:106` 把 half-open 全局互斥导致的“并发只放行 1”当作期望。
- `internal/store/usage_archive_test.go:157` 把窗口刻意对齐午夜，掩盖真实滚动窗口的边界丢数。
- 路由矩阵从源码正则提取路由，没有执行真实 mux。
- failover 链耗尽测试复制生产分支，而不是走真实响应写出路径。
- `go test -race ./internal/store/` 全绿，但 race 检不出 SQL 事务语义和 check-then-act。

---

## 5. 架构级 P2/P3 债务

以下是“不立即造成资金/安全 P0，但会持续产 bug”的结构债务：

| 主题         | 结论                                                                         | 主要证据                      |
| ---------- | -------------------------------------------------------------------------- | ------------------------- |
| 模块边界       | 存在 3532 行 `main.go`；`internal/admin` 反向依赖 `internal/server`                | A1 §2、A7-P2-18            |
| 鉴权口径       | 路由白名单、handler `requireAdmin`、对象归属、usage scope 四套判定并存                       | A1 §2、A7-P2-01            |
| 配置真相       | runtime config、snapshot、store、admin DTO 多份定义；部分字段从未读取                      | A1 §3/§5                  |
| PATCH 语义   | 字段合并靠注释；Group PATCH 会清空 description，Network/ModelGroup 必填耦合                | A7-P2-07、A7-P2-10/11      |
| 钱包/计价      | 余额预检与扣费公式分叉；NULL 限额、缓存 token、Anthropic cache creation 语义不统一                | A1-P2-2、A4-P1-2/3、A8-P0-3 |
| usage 生命周期 | `usage_state` 词表混维；missing、cost_pending、归档、累计表没有统一状态机                      | A4 §4、A6、A8               |
| 上游运行时      | snapshot 可变、健康真相分裂、探测 stagger 硬编码、余额查询绕过代理                                 | A5-P2                     |
| 前端契约       | struct/types/api 三处手写；`as T` 只是 TypeScript 断言；`mutate()` 是空函数且 20 个写操作仍包着它 | A9-P2                     |
| 测试门禁       | 仅 5 个前端测试；无组件/API 契约测试；慢测和固定 sleep 进默认 CI                                  | A9-P2-8、A11-P2            |
| 部署         | 降权失败仍以 root 运行；healthcheck 端口写死；备份不完整；配置/TLS 无进程级契约                        | A10-P2                    |
| 文档         | 版本号、架构图、路由表、端点表、目录树和“已解决”项大面积漂移                                            | A1-P3                     |

---

## 6. 已验证无问题或基础较好的区域

这不是一份“全部否定”的报告。以下区域经过专门核验，暂未发现同级问题：

- SQL 注入面：store 内 SQL 拼接仅限包内白名单常量、占位符和字面量排序方向。
- `balance_charges` 复合主键迁移：单事务重建、失败回滚、迁移幂等已正确实现。
- usage 剪枝的聚合与删除：同一事务、同一 rowid 临时表谓词，剪枝前后总量守恒（按天序列仍需修 P0-7）。
- `usage_daily_rollups` 主键重建：非空表拒绝启动而非静默丢历史；索引与 `ON CONFLICT` 对齐。
- 管理面现有 81 条路由的匿名/普通用户/管理员矩阵，当前未发现匿名可达受保护 handler 或直接跨用户 key 越权。
- 密码重置、改密和 bootstrap 设密通过 `auth_version + 1` 原子失效旧会话，成功路径正确。
- 出站 `ResponseController.SetWriteDeadline` 的用法未复发历史“包装 writer 丢帧”；SSE writer 有并发锁。
- Anthropic 块状态机、缓存 token 三桶拆分的主要路径已验证。
- 入站协议解码的重复顶层键拒绝、非对象顶层处理、结构化解码基础检查已验证。
- 安全基础面：SQL 参数化、静态文件服务、上游重定向控制、密码哈希与 JWT 算法固定没有发现直接 P0/P1 缺口。

各报告末尾都另有更细的“已验证无问题的区域”。

---

## 7. 修复路线

### 阶段 0：冻结数据风险，先建立可观测性

1. 对 `balance_pending`、`balance_charges`、`usage_records.cost_total`、`usage_totals`、`reserved_tokens` 建立只读对账脚本，先量化现存偏差。
2. 记录每次预占、结算、扣费、重算的 request_id 和状态，不只看日志。
3. 在修复前禁止执行会触发大规模结算/归档的运维操作。
4. 将推荐 Docker 部署改为回环绑定 + TLS 反代；公开监听且无 TLS 声明时启动告警或拒绝启动。

### 阶段 1：资金与数据正确性 P0

1. 把 `settlePendingForUser` 改成单事务，采用“先锁定/快照，再 UPDATE，再按已结算 ID 精确 DELETE”，禁止整表 DELETE。
2. 明确 NULL 余额语义，禁止 `COALESCE(NULL, 0)` 参与限额判定。
3. 统一费用状态机：`reported`、`cost_pending`、`charged`、`charge_failed`、`archived`；扣费失败进入可重试队列。
4. `RecomputeCost` 在同一事务内同时写明细、累计表与余额调整；补记逻辑必须幂等。
5. 修复归档边界：对边缘日采用明细 + 归档拆分区间，不再要求“整日包含”。
6. 建立 `reserved_tokens` 启动恢复/过期租约；所有预占必须有 lease、终态和清理路径。
7. 修复 key 配额早退漏退 TPM，并补并发/崩溃测试。

### 阶段 2：安全与可用性 P0/P1

1. bootstrap token 只在一次性安全通道展示，日志永久脱敏。
2. SQLite、`master.key`、会话密钥目录权限收紧到进程用户；备份包含全部恢复材料。
3. `/healthz` 只返回稳定状态码和通用错误码，不返回底层错误字符串。
4. `ClaimTargetProbe` 拆分“健康目标正常并发”和“熔断恢复 half-open 单探测”两个状态。
5. SSE 所有写点检查错误；写失败立即终止状态机并记录真实 outcome。

### 阶段 3：拆边界、统一契约

1. 从 `main.go` 抽出 route manifest、auth policy、billing service、failover coordinator、usage recorder、shutdown coordinator。
2. 路由 manifest 成为唯一路由真相；启动真实 mux，做端到端端点契约测试。
3. 统一 effort 解析和映射；删掉 inwire 私有 `parseEffort` 与 Anthropic 预算折算中的重复口径。
4. 从 Go response struct 生成 TypeScript 类型；为 `api.ts` 每个端点建立路径/方法快照测试。
5. 明确 `Event.ToolIndex`、`OutputIndex`、Anthropic block index 三个编号空间，禁止跨语义复用。

### 阶段 4：测试与发布门禁

1. 每个资金不变量都有真实 Store + 真实事务测试：重复结算、崩溃窗口、NULL 限额、跨 Store 竞争。
2. 真实生产 mux 契约测试：路由漏注册、路径缺失、前后端调用不存在端点必须失败。
3. 协议矩阵测试走真实 inwire/outwire 生产路径，测试中禁止复制实现。
4. CI 增加 `-race`、前端 typecheck/test、API 契约、embed 同步、lockfile 版本、迁移 fixture 检查。
5. Docker tag 发布必须先通过完整 CI；禁止手动绕过 race/前端/embed 门禁。

---

## 8. 当前仓库卫生

审计前后均未由审计 agent 修改跟踪文件，也未执行 Git 写操作。当前工作树原本就处于中间态：

- `main` 为 `ea19ce1`，领先 `origin/main` 2 个提交。
- 已跟踪的 `internal/webui/dist/assets/*.js` 有 17 个删除、17 个新 hash 未跟踪；`internal/webui/dist/index.html` 已修改。
- `web/src/views/UsageDetail.vue` 有未提交改动。
- `AGENTS.md` 与 `docs/` 下 5 份文档未跟踪。
- `work/` 是审计 scratch 区，不能加入提交。

在完成汇总时不要执行 `git add -A`：当前会把 dist 中间态和源改动以不对应方式纳入历史。

---

## 9. 最终判断

当前系统的主要风险不是“还有几个未修 bug”，而是：

- 资金路径没有可证明的事务和状态机；
- 关键契约没有机器校验；
- 路由/鉴权/热路径集中在一个不可测试的大文件；
- 测试在若干关键处验证的是错误现状；
- 部署推荐默认把凭据放在明文网络上。

建议先完成阶段 0 的对账与风险冻结，再按阶段 1 修资金 P0。不要从 UI 小问题、文档文字或单个协议字段开始修，否则仍会重复“修一点、坏另一处”的循环。

---

## 10. 覆盖矩阵

用于证明本次审计没有漏掉子系统。每一格对应一份带证据的明细报告。

| 子系统    | 覆盖内容                                                                        | 报告  |
| ------ | --------------------------------------------------------------------------- | --- |
| 架构/边界  | 模块依赖图、单点真相、配置唯一事实来源、死代码与孤儿端点、近期改动根因归类                                       | A1  |
| 入站协议   | OpenAI Chat / Responses / Anthropic 三套解码、effort 归一、媒体块、tool_choice、跨协议字段对称性 | A2  |
| 出站协议   | 三套 sink 的流式/非流式写出、状态机、编号空间、usage 字段、SSE 写错误处理                               | A3  |
| 热路径与计费 | handleIngress 状态机、预占释放完备性、usageRecorder、charge 链、流式记账、优雅关闭、replay gate      | A4  |
| 上游运行时  | 上游池、冷却/熔断、快照与路由索引、探测、限速、余额查询、模型发现                                           | A5  |
| 存储层    | 17 张表 schema、16 条索引、3 个触发器、迁移、归档/剪枝、单条 SQL 正确性                              | A6  |
| 管理面    | 81 条路由矩阵、认证与限速、会话/token 生命周期、对象级授权、PATCH 合并、CSRF/CORS、审计日志                  | A7  |
| 资金并发   | 余额结算、配额预占、扣费与欠费路径、聚合 DAO、事务边界与崩溃恢复                                          | A8  |
| 前端     | 17 个 view × router × 导航、API 契约、竞态与失败态、token/401、测试覆盖、dist 同步状态              | A9  |
| 安全与部署  | 监听/TLS/反代、凭据加密、注入与 SSRF、容器与权限、备份、发布与供应链                                     | A10 |
| 测试与 CI | 测试真实性、协议/并发/崩溃覆盖、CI 门禁矩阵、发布制品、flaky 风险                                      | A11 |

每份报告末尾均有「已验证无问题的区域」，避免只呈现负面结论。
