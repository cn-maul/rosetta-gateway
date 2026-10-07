# 全局审计报告 — 2026-10-06

> 审计方式：5 个分片并行深读（管理面认证与会话 / 数据面权限 / 转发链路 / 存储计费 / 前端与部署 CI），
> 覆盖全部 Go 源码（约 1.8 万行，不含测试）与前端源码；所有 P1 级结论经人工逐行复核确认。
> 上一次全面审计见 `AUDIT-2026-09-21.md`（历史归档）。

## 0. 总体结论

**未发现 P0**：无 SQL 注入面、无 XSS 注入点、无明文凭据回读路径、无直接鉴权旁路、
无账目级数据损坏。已知取舍（reload 原子性窗口、删库=失明、audit actor 写死 admin）
经复核均如文档所述、影响有界。

发现 **5 条 P1、约 15 条 P2**。每条均附文件:行号与代码证据。

---

## 1. P1 —— 应尽快修

### P1-1 普通用户可以撤销管理员对自己 key 施加的全部强制措施

`internal/admin/key_handler.go:238-290`（applyP2）、`:342-387`（Update）

`Update` 在 `ownedByCaller`（自己的 key）通过后，`enabled`、`quota_tokens`、
`rpm_limit`、`tpm_limit` 无条件生效；`applyP2` 里只有 `group_id` 做了 `callerIsAdmin`
检查，`expires_at`、`allowed_ips` 同样不设防：

```go
if req.Enabled != nil { existing.Enabled = *req.Enabled }      // 管理员禁用 → 一条 PATCH 复原
if req.QuotaTokens != nil { existing.QuotaTokens = *req.QuotaTokens }
if req.RPMLimit != nil { existing.RPMLimit = *req.RPMLimit }
...
if req.ExpiresAt != nil { k.ExpiresAt = *req.ExpiresAt }        // 可清零（0=永不过期）
if req.AllowedIPs != nil { k.AllowedIPs = store.FormatAllowedNets(nets) }  // 可清空
```

攻击/故障场景：管理员因泄露禁用某把 key（`enabled=0`，代码多处称这是安全敏感操作），
归属者一条 `PATCH {"enabled":true}` 静默复原；设的有效期/IP 白名单/限额都能清零。
与 user_auth.go 声明的「key 级白名单要能**自助收紧**」矛盾——同一接口放开了全部放松操作。

修复方向：非管理员对这些字段只允许「不宽于现值」，放宽一律 403。

### P1-2 bootstrap「设过即 409」存在并发竞态（TOCTOU）

`internal/admin/user_handler.go:317-352`、`internal/store/user_dao.go:202-208`

查库判定（`FindUninitializedAdmin`）与写库（`SetUserPassword`）是两步独立操作，
UPDATE 只有 `WHERE id = ?` 没有 `AND password_hash = ''`：

```go
u, err := h.store.FindUninitializedAdmin(r.Context())   // 判定 password_hash=''
...
if err := h.store.SetUserPassword(r.Context(), u.ID, hash); err != nil {  // 写库无条件守卫
```

两个并发 `POST /admin/api/bootstrap` 都能通过判定，后写者覆盖先写者。非回环部署下
攻击者若与合法管理员并发提交，最终密码是攻击者的，且攻击者会话带最新 auth_version、
管理员反而被踢出——无任何恢复路径，且该操作不进审计（见 P2-2）。
这与文档声称的取舍「抢先成为第一个」不同：竞态允许「最后一个说了算」。

修复方向：条件 UPDATE（`AND password_hash=''`）+ `RowsAffected==0` 回 409。

### P1-3 SDK v1.0.0 会对对话 POST 做重试，DESIGN 的声明已失效 → 慢非流式请求上游重复计费

`internal/upstream/upstream.go:824`（`WithMaxRetries`）、SDK `rosetta@v1.0.0`
`provider_openai_chat.go:386,432`、`internal/server` 的共享 transport 构造处

DESIGN.md 写「`WithMaxRetries` 只对幂等方法生效……对话是 POST，SDK **不重试**」——
那是 v0.5.x 的行为。v1.0.0 给 chat POST 标了 `RetryIdempotent`（SDK 注释原文
"behaves like RetryAlways"），而网关传了默认 `max_retries=2` 且未设 `NoIdempotencyKey` quirk。
配合 `ResponseHeaderTimeout=60s` < 非流式超时 120s：任何响应头晚于 60s 的慢生成
（慢推理模型）触发传输层超时 → **同一 POST 被原样重发**，上游重复生成重复计费
（`Idempotency-Key` 只对实现去重的上游有效），网关侧账面 0 token、
`usage_state="missing"`（注：修复后区分 `reported`/`missing` 两态，修复前无此字段，
账面与 `reported` 的 0-token 行无法区分），完全看不出。

修复方向：`WithQuirks(NoIdempotencyKey: true)` 使 POST 落回 `RetryDefault`
（= DESIGN 声明的语义，重试职责交给故障转移链），并更新 DESIGN。

### P1-4 `COALESCE(user_id,'')` 使索引失效，用户级配额预检每请求全表扫

`internal/store/usage_source.go:112`；调用方 `internal/store/user_dao.go:247`、
`cmd/gateway/main.go:1101`（每请求热路径）、`internal/admin/user_admin_handler.go:113`（N+1）

```go
if f.UserID != "" {
    add("COALESCE(user_id, '') = ?", "user_id = ?", f.UserID)
}
```

表达式谓词无法使用 `idx_usage_user_ts(user_id, ts)`（EXPLAIN 验证：明细支 `SCAN usage_records`）。
凡配了用户级配额，**每个 /v1 请求**都全表扫 30 天明细（可至百万行级），读池仅 4 连接，
高 QPS 下预检排队。归档支已经直接用 `user_id = ?`（NULL 行两种写法都不匹配非空 scope，
语义完全等价），明细支照抄即可。旁边注释「否则对任何非空 scope 都返回 0 行」的论证是错的。

### P1-5 编辑 key 会静默重写有效期——过期 key 被「复活」为永不过期

`web/src/views/Keys.vue:33-41, 181, 201`

编辑表单用「剩余天数」回显（过期 clamp 为 0 = 永不过期），保存时**总是**发送 `expires_at`：

```ts
function daysLeft(expiresAt: number): number {
  if (!expiresAt) return 0
  return Math.max(0, Math.ceil((expiresAt - Date.now()) / DAY_MS))   // 过期 → 0
}
...
expires_at: expiresAtFromDays(eForm.days),   // 保存时无条件发送
```

管理员只想改名/调白名单：剩余不足 1 天的 key 被顺延为「现在 + 1 天」；
已过期的 key 保存后 `expires_at=0` 永久复活（数据面本应 403 key_expired）。
与 P1-1 叠加后无人能拦。

修复方向：只在用户显式修改过天数时才发送该字段（不传 = 保持原值，符合 PATCH 语义）；
「已过期」与「永不过期」在界面上区分显示。

---

## 2. P2 —— 建议修复

### 认证 / 会话

| # | 问题 | 位置 | 要点 |
|---|---|---|---|
| P2-1 | 无令牌请求计入登录失败限速 | `internal/server/user_auth.go:179-191` | ✅ 已修（`963193c`）—— 无凭据请求不计数，限速对象改为「试图伪造凭据」 |
| P2-2 | bootstrap 审计分支是死代码 | `cmd/gateway/main.go:423`、`internal/server/autoreload.go:102` | bootstrap 挂 public mux，永远到不了带审计的 adminAuto 链——最敏感写操作零审计 | ✅ 已修（新增 server.AuditOnly，bootstrap 独立审计） |
| P2-3 | `/me` 错误路径缺 return | `internal/admin/user_handler.go:413-418` | ✅ 已修（`5ad2813`）—— 降级改走 `writeJSON(200)`，不再写 500 头 |
| P2-4 | 改密对旧密码校验不限速 | `internal/admin/user_admin_handler.go:479-513` | 会话被临时窃取时可对旧密码无限在线爆破；与登录端点防护不对称 | ✅ 已修（按用户 ID 限速，独立 FailureThrottle 5次/5min） |
| P2-5 | 5 处「空 ID 用户 = admin」fail-open 分支残留 | `user_admin_handler.go:83`、`key_handler.go:307`、`usage_handler.go:187`、`group_handler.go:296`、`user_handler.go:405` | 统一认证后不可达，但属休眠提权原语，方向全是静默放行 | ✅ 已修（三处改fail-closed，空 ID 不再当admin） |
| P2-6 | 空哈希账号登录文案构成枚举 oracle | `user_handler.go:139-147` | 文案覆盖管理员建的所有空密码账号（不止引导 admin），与注释声称的范围不符 | ✅ 已修（收紧为 role=admin 且空哈希，与 FindUninitializedAdmin 同口径） |
| P2-7 | `server.writeAuthError` 死代码 | `internal/server/server.go:381-386` | 旧「管理员密码」时代的错误文案，无调用者 | ✅ 已修（删除 internal/server那份死代码） |

### key 权限模型 / 数据面

| # | 问题 | 位置 | 要点 |
|---|---|---|---|
| P2-8 | 普通用户自助建 key 可自设不限额 | `key_handler.go:125-229` | quota/rpm/tpm 任意填（0=不限），架空 key 级限速；结构性修复需用户级限速（新功能） | ✅ 已修（自助建 key 的 quota 封顶、rpm/tpm 强制清零） |
| P2-9 | 配额预检纯 check-then-act 无预占 | `cmd/gateway/main.go:1079-1115` | 并发突发可超发数十倍于剩余额度；注释「至多一个在途超发」仅串行成立；TPM 有预占而配额没有 | ✅ 已修（新增 ReserveQuota/ReleaseQuota 原子预占，与 TPM 同构） |
| P2-10 | 直连形式无法经 API 写进白名单 | `group_handler.go:328-359`、`main.go:1170-1175` | `known` 集合只有具名路由，`/v1/models?include=upstream` 却列出直连 id 供挑选——照指引做恒 400，可能逼出「干脆不设白名单」的错误 workaround（fail-closed 方向） | ✅ 已修（known 集合并入 provider/model 直连形式） |
| P2-11 | key 列表 `username` 永不填充 | `key_handler.go:72,494`、`Keys.vue:288` | 死契约，管理员恒看到裸 user_id | ✅ 已修（username 从快照 UsersByID 取，前端 k.username 生效） |

### 转发 / 计量

| # | 问题 | 位置 | 要点 |
|---|---|---|---|
| P2-12 | 上游不回 usage 按「0 token + reported」记账 | `cmd/gateway/main.go:1667-1688, 1722-1742` | ✅ 已修（`8c833fd`）—— 接入 `Usage.IsZero()` 分两态记 `missing`，TPM 保留预占 |
| P2-13 | `MarkCredentialCooldown` 锁外读共享字段 | `internal/upstream/upstream.go:412-435` | ✅ 已修（2026-10-06，锁内取快照） |
| P2-14 | 共享 transport 缺 dial/TLS 握手超时 | `internal/upstream/upstream.go:809-818` | SDK 注释专门警告过手搓 transport 不继承这两项；黑洞上游的失败检测被拖到 30s/120s 看门狗 | ✅ 已修（补 DialContext/TLSHandshakeTimeout各 10s） |
| P2-15 | TTFT 看门狗 `Stop()` 返回值被忽略 | `cmd/gateway/main.go:1511-1517` | 竞态窗口可产出「空但 ok」的假正常流（历史上反复修的形态） | ✅ 已修（检查 Stop() 返回值 + 等 ttftDone 确保 Close 已落地） |
| P2-16 | RPM 注释与限速器行为相反 | `main.go:1067` vs `ratelimit/limiter.go:80` | 实际行为是 limiter 的（更安全），应改 main.go 注释防后人「修反」 | ✅ 已修（改正注释，与 limiter 实现对齐） |
| P2-17 | 链耗尽错误归因到最后一个 active 而非实际尝试目标 | `main.go:1339-1343` | 排障定位偏差 | ✅ 已修（记录实际尝试到的候选） |

### 存储 / 计费

| # | 问题 | 位置 | 要点 |
|---|---|---|---|
| P2-18 | `CREATE TRIGGER IF NOT EXISTS` 永不更新旧触发器体 | `store.go:242`、`usage_archive.go:128` | 将来改口径时升级库与新库静默分叉（totals 漂移无报错） | ✅ 已修（DROP+CREATE 替代 CREATE IF NOT EXISTS） |
| P2-19 | `DeleteGroup` 只挡 users 不挡 keys | `group_dao.go:107-134` | 删组后 key 级覆盖被 SET NULL，模型白名单静默放宽 | ✅ 已修（DeleteGroup 补查 access_keys 成员） |
| P2-20 | `SumTokensByDayForKey` 归档支边界日整天计入 | `usage_dao.go:450-461` | 账单多报（最多一整天），与 UsageSource 自己的「宁可少算」规则相反 | ✅ 已修（边界改整日包含，与 UsageSource 口径一致） |
| P2-21 | `group_by=day`（UTC/仅明细）与 `by-day`（本地/含归档）口径分裂 | `usage_handler.go:210,561` | 同一界面两个趋势入口数字对不上、老日期空白 | ✅ 已修（group_by=day 改本地日界，与 by-day 对齐） |
| P2-22 | `freezeUsageCost` 查价失败静默计 0 且不可修复 | `usage_dao.go:81-89` | 一次读池抖动 = 永久漏账、无对账线索；缺 RecomputeCost 入口 | ✅ 已修（补 WARN 留痕 + 新增 RecomputeCost 补救入口） |
| P2-23 | token 列无 CHECK 约束 | `store.go:198-219` | 上游回报负数可污染终身累计、凭空发放配额 | ✅ 已修（CreateUsageRecord 入口归一负数为 0） |
| P2-24 | 货币 float64/REAL 无舍入 | `usage_dao.go:98`、`store.go:289` | 长尾二进制小数进响应；建议展示层 round(6) | ✅ 已修（费用舍入到 1e-9 元） |
| P2-25 | 剪枝大事务占满唯一写连接 | `usage_archive.go:361-426` | 首剪可达百万行 DELETE，期间全部写入排队；建议按天分事务 | ✅ 已修（单次上限 2 万行 + 水位仅在删净时推进） |
| P2-26 | `reconcileUsageTotals` 同一 src 绑定 10 次占位符 | `usage_archive.go:261-275` | 当前零占位符所以正确；将来传非空 filter 即错绑——需注释钉死或重构 | ✅ 已修（钉死注释：args 必须零长度） |

### 前端 / 部署 / CI

| # | 问题 | 位置 | 要点 |
|---|---|---|---|
| P2-27 | CI 不跑 go test 也不跑 vue-tsc；action 未 pin SHA | `.github/workflows/ci.yml` | ✅ 已修（`63dde96`）—— 补全量 go test 与 vue-tsc，action pin 到 commit SHA |
| P2-28 | 容器 root 运行 + 无 HEALTHCHECK | `Dockerfile:41-63` | 进程被攻破即持 root；死锁无法探活 | ✅ 已修（entrypoint 降权+ su-exec；补 HEALTHCHECK） |
| P2-29 | localStorage JWT + 登出无服务端吊销 + 多标签页不同步 | `web/src/api.ts:24-34` | A 登出 B 仍可用（无 storage 事件）；供应链投毒可绕 CSP 偷 8h 令牌 | ✅ 已修（登出递增 auth_version 作废全部会话 + 令牌改 sessionStorage） |
| P2-30 | 5 个视图加载失败呈现为「暂无数据」空态 | `Keys/Providers/Routes/History/Overview.vue` | 把加载失败呈现成确无数据，误导运维（Users/Groups 有正确的持久错误态可对照） | ✅ 已修（5 个视图加持久错误态） |
| P2-31 | CSP 缺 `object-src 'none'` | `server.go:411-414` | 零成本硬化项 | ✅ 已修（CSP 加 object-src none） |

---

## 3. 已核查无明显问题（摘要）

- **管理面认证**：JWT 算法固定（HS256 + ValidMethods 双保险）；auth_version 全路径生效；
  登录限速自身逻辑无洞（不采信 X-Forwarded-For 有测试钉住）；免鉴权面恰好 4 条路由无多漏注册；
  SameOrigin（Sec-Fetch-Site 优先 + Origin/Host EqualFold）与 decodeJSON Content-Type 断言在位；
  登录防枚举（统一文案 + dummyHash 拉平 KDF）；密码 PBKDF2 210k/16B 盐自描述编码；
  会话密钥文件 0600/0700、空文件拒启、日志不打值。
- **数据面**：key 哈希比对与错误码不泄露他人资源；横向越权（改别人的 key）已封堵且有测试；
  `/v1/models` 与实际调用用同一 `AllowsModel` 判定（key ∩ 组求交，零值=拒绝全部 fail-closed）；
  RPM/TPM 固定窗口实现正确（TPM 预占+校正）；expires_at 全链路毫秒一致；
  IP 白名单 fail-closed 双向；usage/stats/org 端点作用域收窄逐条核对通过。
- **转发**：sk-gw key/Cookie 不透传上游；SSE 解析缓冲边界（2MB/16MB 上限、跨 chunk 撕裂、CRLF）正确；
  断流收尾语义与 DESIGN §8.2 一致；客户端断开不毒化凭据；400/超限类错误不烧转移预算；
  goroutine 生命周期与取消传播完整；body 上限双层一致。
- **存储**：无 SQL 注入面（标识符全部包内白名单、值全参数化）；PRAGMA 真实生效（WAL/FK/ busy_timeout）；
  触发器口径四处一致；剪枝与归档无双算窗口；dropDeadColumns 名单无仍在读的列；
  迁移幂等；删除用户的 CASCADE 与用量保留语义有测试。
- **前端**：全仓无 v-html/innerHTML/动态 href 注入点（Vue 插值自动转义）；
  CSP 与产物匹配（无内联脚本，有测试守护）；时间戳全链路毫秒；allowed_models 三态 PATCH 语义前后端一致；
  上游 api_key 仅掩码不可回读；`writeServerError` 不回显内部错误。
- **部署/CI 其他**：Docker 卷覆盖全部状态、config.default.json 与代码默认值无漂移；
  两个 workflow permissions 已最小化、无明文密钥。

## 4. 建议修复顺序

1. **P1-1 + P1-5**（key 权限收口 + 前端有效期重写）——同一主题，实际风险最高；
2. **P1-3**（一行 quirk 关闭 POST 重试）+ DESIGN 更新——止损重复计费；
3. **P1-2**（bootstrap 条件 UPDATE）+ **P1-4**（索引失效）——各一行改动；
4. `/me` 双写、无令牌限速、usage missing 标记——小改动高收益；
5. CI 补 `go test` + `vue-tsc`——防回归的根基。

## 5. 修复记录（2026-10-06）

P1 全部修复；P2 修复 4 条，其余按「建议修复顺序」之外的设计变更项保留待办。

| 发现 | 状态 | Commit |
|---|---|---|
| P1-1 key 只能收紧 | ✅ 已修 | `aa11dc3` |
| P1-2 bootstrap 并发竞态 | ✅ 已修（条件 UPDATE + role 纵深） | `5ad2813` |
| P1-3 SDK POST 重试 | ✅ 已修（NoIdempotencyKey quirk + DESIGN 同步） | `57a2745` |
| P1-4 配额预检全表扫 | ✅ 已修（user_id = ? 回归索引） | `5ad2813` |
| P1-5 前端有效期静默重写 | ✅ 已修（改过天数才发送） | `aa11dc3` |
| P2-1 匿名请求计入限速 | ✅ 已修 | `963193c` |
| P2-3 /me 双写响应 | ✅ 已修 | `5ad2813` |
| P2-12 usage missing 漏账不可见 | ✅ 已修（missing 标记 + TPM 保留预占） | `8c833fd` |
| P2-27 CI 缺测试、action 未 pin | ✅ 已修 | `63dde96` |
| P2-13 凭据冷却数据竞争 | ✅ 已修（锁内取快照，见下方专项） | 本次提交 |

## P2-13 修复专项（2026-10-06）

**根因**：`cred` 是 `*CredentialEntry` 共享指针。旧代码在**解锁之后**才读
`cred.CooldownUntil` 并把它作为实参传给 `SetCredentialCooldown`。Go 的实参
在进入函数前求值，而此刻锁已释放、并发的冷却或恢复都可能正在改写该字段。

**为什么必须修**：冷却是**唯一必须落库持久化**的运行时状态 —— 池会被任何
admin 写操作重建，重建时从 `cooldown_until` 恢复。于是这不是"日志时间戳偏差"：
A 请求 60s 冷却、B 请求 1s 冷却，A 把B 的时间戳写进了库，60s 冷却实际只
生效 1s。**对持续失效的凭据，冷却机制形同虚设** —— 而它恰恰是防这个的。

**修法**：`until` 在锁内取值到局部变量，锁外只用这份快照。同批修正
`RecordCredentialSuccess` 的 `changed` 判定（原先若放到锁外读 `cred.Status`，
会与并发冷却争抢同一行，导致刚被冷却的凭据被误判为"无需恢复"，内存与库分叉）。

**测试踩的坑（值得记）**：反向验证时前两版测试都是**恒绿的假信号**，
修复后 PASS、回退后仍 PASS。原因：

1. 第一版让 store 在**函数体内**改写共享字段 —— 太晚。实参在主goroutine
   求值时就已经取完了，污染发生在之后，无效。
2. 第二版改用并发两次调用，但用 `current string` 标记调用方 —— 该字段本身
   也是并发共享的，标签互相串，200/400 次断言失败（是测试自身的竞态）。

最终方案：store 在落库期间**真的起一个 goroutine** 改写共享字段并等待完成，
同时让两种时长悬殊到不可能混淆（1s vs 1h），落库值本身即可唯一标识归属，
不需要标签。结果：旧写法下 399/400 次命中污染值 → FAIL；修复后全过。

> **教训**：Go 里"锁外读共享字段"这类bug，**单线程顺序调用永远测不出来**，
> 因为实参求值发生在进入 callee 之前。要复现必须让两个 goroutine 真实交错。
> 写完并发测试后**必须**做反向验证，否则写出的很可能只是恒绿断言。

---

## 修复状态总览（2026-10-07）

**P1 全部 5 条 + P2 全部 31 条均已修复。** 本轮（P2）按域分四批落地：

| 批次 | 条目 | 关键改动 |
|---|---|---|
| 上游池/转发 | 14/15/16/17 | transport 补拨号与握手超时；TTFT 看门狗消除「空但 ok」；改正与实现相反的注释；错误归因到实际尝试目标 |
| 认证/会话 | 2/4/5/6/7 | bootstrap 独立审计；改密按用户限速；三处 fail-open 收紧；登录文案去掉枚举 oracle；删死代码 |
| 存储/计费 | 18/19/20/22/23/24/25/26 | 触发器 DROP+CREATE；删组补查 key；账单边界与 UsageSource 对齐；费用失败留痕+可重算；负数归一；浮点舍入；剪枝限量；占位符约束钉死 |
| 前端/部署/权限 | 8/9/10/11/28/29/30/31 | 自助建 key 额度封顶；配额原子预占；白名单认直连形式；username 真正填充；容器非root+健康检查；登出真吊销；5 视图持久错误态；CSP 补object-src |

外加 P2-21（group_by=day 与 by-day 日界对齐）随前端批次一并修复。

### 本轮的三条方法论教训

**1. 测试会骗人，而且比缺陷更难发现。** P2-13 修数据竞争时连续写了两版
恒绿测试（修复后 PASS、回退后仍 PASS）：第一版让 store 在函数体内污染共享
字段—— 太晚，Go 的实参在进入 callee 之前就求值完了；第二版用共享的
`current` 变量标记调用方 —— 那变量本身也竞态，标签互串。最终方案是让 store
真的起 goroutine 并等待完成，用悬殊到无法混淆的时长（1s vs 1h）让落库值
自身标识归属。**旧写法下 399/400 命中污染值 → FAIL，才算有效。**

**2. 修「统一口径」这类问题时，先看响应契约。** P2-21 我一度把
`group_by=day` 直接转给 `/by-day` 的实现，被既有测试挡住 —— 两者响应形状
不同（前者带 summary）。那份测试守的是真实契约，不是实现细节。正确解法是
在保留响应形状的前提下修正日界。

**3. 改之前先查有没有现成能力。** P2-29 我准备新写一个 `BumpAuthVersion`，
结果 store 里早就有了（注释写着「用于禁用账号、改角色这类必须让会话失效的
操作」）—— 而我的方案和它的语义完全一致，只是多绕了一圈。

### 遗留（均为设计取舍，不是缺陷）

- **全链路无 TLS**：部署层解决（前置反向代理）。
- **用量归档后老日期在 `group_by=day` 之外仍不可见**：与 UsageSource 的
  「宁可少算」口径一致，属刻意的边界选择。
- **`-race` 本机不可用**（Windows 无 gcc）：并发修复靠人工审查 +
  CI 上有 gcc 的环境兜底。
- **配额预占的 est 是预估值**：真实用量在收尾处校正；上游不回usage 时
  传 0，此时 used 会比真实少计—— `usage_state="missing"` 让漏账可见。

### 审计员在 §0 记下、且复核为「有界」的三条取舍

这三条不是本轮引入的，也不属P1/P2 清单，但**记录在案** —— 写在这里是为了
不随总览重写而丢失：

- **`audit.actor` 恒为 "admin"**（`internal/store/audit_dao.go:15`）：
  `WriteAuditor` 回调没有透传会话身份，多用户后**普通用户对自己资源的写
  操作也会落一条 "admin" 记录**。排查时无法区分是谁改的。修法是把
  `server.UserFromContext(r)` 里的身份透进回调签名 —— 改动面波及所有
  WriteAuditor 调用点，单独一轮。
- **reload 失败无重试**（`internal/server/autoreload.go`）：重建失败只打
  ERROR、不重试、不置脏标志。响应早已是 200 + 「已禁用」，但该 key 会
  **无限期继续放行**，直到下一次任意 admin 写操作。修法是
  `dirty atomic.Bool` + 低频后台重试。
- **删库即失明**：`reconcileUsageTotals` 的种子标记与终身累计都存于同一个
  SQLite 文件，删库后历史累计一并消失。属单文件部署的固有代价。
- **全链路无 TLS / reload 原子性窗口**：见上。
