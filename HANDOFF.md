# 项目状态交接（2026-10-11）

> **给下一个会话读这个文件就够**，不必重读全部对话。
> 上下文预算：每轮约 685K token。带这份文件（~5K）比带 20 轮对话便宜得多。

## 一、项目

`C:\Users\louis\Desktop\project\rosetta-gateway` — LLM API 网关（Go + Vue3）。
配套 SDK：`C:\Users\louis\Desktop\project\rosetta`（**同一个所有者，v1.1.1 已发**）。

## 二、当前状态

版本 **1.5.3**（`web/package.json`），最新 tag `v1.5.3`，HEAD `cbffbc4`。

**唯一未提交改动**：`go.mod` / `go.sum` —— rosetta `v1.1.0 → v1.1.1`
（`go test ./...` 全绿；升级时需 `GOSUMDB=off`，goproxy.cn 的 sumdb lookup 返回 404）。

### 上次交接（1.5.0）之后的提交

| 提交 | 内容 |
|---|---|
| `cbffbc4` | **思考挡位透传 + 模型能力注入 + 上游余额查询 + 配置项调整**（见 §三） |
| `40e2cd9` | AUDIT：magpie 对照分析与网关侧改进建议 |
| `694e507` | 版本 1.5.2 → 1.5.3 |
| `8045424` | test(outwire)：tool_calls 形状对第三方客户端的兼容性审计 |
| `06a2407` | fix(outwire)：修正 tool_calls 增量形状 —— 对齐 OpenAI 线格式而非填满空串 |
| `c068274` | fix(outwire)：流式 tool_calls 增量缺 `function` 字段，严格客户端判响应非法 |
| `0b524a3` | fix(usage)：按天分桶时区在 Linux 上未生效 —— 补 `TZ` 环境变量 |
| `ce0c204` | 版本 1.5.1 → 1.5.2 |
| `40e1e4e` | fix(usage)：按天分桶日界时区错误 —— 过了 0 点调用仍算进昨天 |
| `ddbe9e9` | fix(web)：Models.vue 的 CRLF 行尾导致 CI webui-sync 红灯 |
| `f6e497d` | ci(webui-sync)：失败时输出产物差异明细 |
| `3c7f7ce` | 版本 1.5.0 → 1.5.1 |
| `82584e0` | feat(wallet)：普通用户消耗记录改为按天列出（含 by-day 费用列） |
| `9234e19` | test(authz)：调用历史跨用户隔离探针（列表 + CSV 导出） |
| `801ee32` | fix(ui)：恢复用户侧「可用模型」与「调用历史」，管理员隐藏新建密钥 |

### 关键决策（别推翻）

- **扣费失败不重试** —— 防重复收费
- **流式解不开不转移** —— 防重放已交付输出、重复计费
- **批量扣费不做** —— 实测只值 12%，折叠幂等键风险高
- **改钱单位不做** —— 实测语句次数不变，性能无收益；用户负载仅 0.049 req/s
- **管理员不调 API** —— 控制面/数据面分离，只负责管理网关

## 三、本轮重点：`cbffbc4`（思考挡位）

在途工作的一次汇总提交（**当时未新增功能，只是提交积累的改动**），但语义重要：

- **新增 `internal/effort/`** —— 思考挡位（reasoning effort）建模与解析 + 测试
- **模型能力注入 SDK 模型档案** —— `buildClient` 统一注入 `ModelInfo`，
  SDK 的 `gateThinking` 才得以生效。此前网关**从未告知 SDK 任何模型**，
  「模型明确不支持思考」的拦截形同虚设。数据面 / 探测 / 连通性测试三条路径均经
  `buildClient`，注入一次全覆盖
- **`internal/upstream/balance.go`**（+390）—— 上游余额查询
- 字段贯通：`inwire` effort 解析（3 处）、`routing` / `snapshot` / `store` 的能力与价格字段
- 前端：Providers 页 + `api.ts` / `types.ts` 跟随模型能力字段
- `go.mod`：rosetta v1.0.1 → v1.1.0（本轮未提交部分又升到 v1.1.1）

## 四、未修的已知缺口

**来自 2026-10-11 全量审计（`AUDIT/audit-2026-10-11.md`，按处理优先级）**：

1. **批量落库整批失败 → 最多 50 条用量与扣费静默丢失**（`main.go:2728-2748`）——
   `flush` 遇错只打一条 ERROR 就 `return`，记录已出队、不重试。一次 SQLITE_BUSY /
   磁盘抖动就让用户**消耗上游 token 却完全免单**，且配额被永久放宽。**无需攻击者**
2. **查价失败即永久记 0，同样免单且无自动修复**（`usage_dao.go:327-348`）——
   `RecomputeCost` 在生产代码中**无任何调用点**，补救纯靠人工
3. **Anthropic `content` 写成单对象时静默变空消息**（`inwire/anthropic.go:473-482`）——
   OpenAI 侧已有单块兜底可对齐（`openai_chat.go:530-541`）
4. **`requireAdmin` 空 ID fail-open**（`user_admin_handler.go:138-154`）——
   全仓唯一一处「空 ID = 管理员」，与全仓 fail-closed 口径相反。需人工确认可达性，
   无论如何建议统一为 403
5. **组白名单悬空回落 `AllowAll()`**（`snapshot/rebuild.go`）—— **唯一可能造成权限放宽的条目**，
   需实测确认 `access_keys.group_id` 的 `ON DELETE SET NULL` 行为
6. 登录限速只有 IP 维度、无账号维度

**本轮已修（2026-10-11）** —— 来自 ``AUDIT/frontend-audit-2026-10-11.md`` 的前端审计：

- **P1** 设置页保存守卫：新增 ``loadedOnce``，不再只看 ``err``。
  原先 401 早退不设 `err`，表单停在兜底常量而守卫放行 → **默认值覆盖生产配置**。
- **H1** 钱包页余额改读 ``stats``（数据本就在手，零额外请求），不再用登录快照。
- **H2** 总览页刷新失败时保留旧数据并加「数据已过期」横幅 + 重试。
- **H3** 充值标题改用 ``topups.length``；截断提示去掉 ``admin &&`` 条件。
- **M3** ``fmtMoney`` 负数不再被吞成 `0.00`。
- **M1/M2/M4/M5** 「全部」档窗口说明、模型空态两种成因、用户级配额告警徽章、
  关闭明文弹窗时抹掉 ``plainKeyBox.key``。
- **L 级与改进** 凭据删除补影响说明、剪枝标 `danger`、History 稳定 key + 重试按钮、
  时间范围下拉键盘可达、表头写单位、密钥有效期负数钳制、首用引导、配额文案统一。

> **收尾（同轮完成）**：上面那条限制也已清空 —— 后端把 `quota_tokens`
> / `used_tokens` 并入了 `/stats`（`
> internal/admin/stats_handler.go` + `store.UserQuotaAndUsed`），
> 前端钱包页改读实时值。
>
> **关键约束**：已用量的口径必须与配额**判定**一致，所以 stats 侧刻意复用
> `store.SumUserUsedTokens`（累加列 `user_usage_totals`），
> 而不是展示列 `users.used_tokens`。两者一旦分叉，用户会看到
> 「明明还有额度却被拒」。该不变量由 `internal/admin/stats_quota_test.go` 中的
> `TestStats_UsedTokens口径与配额判定一致` 钉住 —— **改这里前先读那条测试**。

**本轮已修（2026-10-11）** —— 来自 `AUDIT/perf-2026-10-11.md` §五 的两条建议：

- **保留期可配置**：新增 `usage_retention_days`（1~3650，缺省 30），`config.example.json` 与 `docker/config.default.json` 已同步。
- **配额预检 O(1)**：新增窄表 `user_usage_totals(user_id, total_tokens)` +
  触发器 `trg_update_user_usage_totals`，剪枝事务内同步回收，
  `SumUserUsedTokens` 改读该表。消掉每请求约 63ms 的全量 SUM。

> **为什么不是给 `users` 加 `used_tokens` 列**（第一版就是这么写的，被现有迁移测试打回）：
> SQLite 在 `ALTER TABLE ... RENAME` 时会校验并重写所有引用该表的触发器，
> 而 `ensureColumns` 里有「重建 users 表补列」的逻辑 —— users 上挂了引用它的
> 触发器会把**整个重建操作卡死**（实测 `no such table: main.users`）。
> 同一脆弱性也潜伏在既有的 `trg_update_used_tokens`（引用 access_keys）上，
> 只是目前没有重建 access_keys 的路径所以没暴露。
> 窄表不参与任何 DROP/RENAME，因此彻底避开。详见性能报告「实施记录」。

> **踩过的三个坑**（改这里前先读）：
> 1. `UsageSource` 是 `UNION ALL` 复合 SELECT 且**自带别名 `u`**；外面再套
>    `(...) t` 会让 `u.` 前缀失效，回填静默返回全 0 且**不报错**。
> 2. SQLite 的 `INSERT ... VALUES ... WHERE` / `INSERT ... SELECT ... WHERE` 都是
>    **语法错误**（`WHERE` 只能跟在 `FROM` 后），触发器里要用 `SELECT ... WHERE` 形式。
> 3. 测试中 `Open()` 时 migrate 已跑过回填并写下 marker，要模拟「老库升级」
>    必须先 `DELETE FROM app_settings WHERE key = ...`。


## 五、SDK 侧状态（rosetta 仓库）

最新 tag `v1.1.1`，HEAD `1000fea`，工作区干净。1.1.0 之后的关键提交：

| 提交 | 内容 |
|---|---|
| `1000fea` | ci：钉死 Go 补丁版本以修复 staticcheck 导出格式不兼容 |
| `8c90fe3` | fix：重试保留上游响应、探测用受管 transport、估算与 SSE 修正 |
| `d8b6b71` | feat：错误归因与流超时留痕，含 Responses sealed reasoning |
| `1a354b4` | feat：思考挡位可原样透传，模型能力并入模型档案 |
| `da1d01d` | fix：上游畸形响应可分类化（新增 `ErrUpstreamMalformed`） |

`1a354b4` 正是网关侧 `cbffbc4` 依赖的底座（`gateThinking` 需要模型档案里有能力信息）。

## 六、环境事实（省去重复摸索）

### 测试

```bash
go build ./... && go vet ./... && gofmt -l cmd internal   # 全绿
go test -count=1 ./...        # 全绿（17 包；store ~27s、cmd/gateway ~77s）
CC=C:/mingw64/bin/gcc.exe go test -race -count=1 ./...   # 竞态检测（本机必须带 CC）

# 2026-10-11 实测：-race 17 包全绿，零 DATA RACE
#   （store 259s、cmd/gateway 388s、admin 84s，合计约 13 分钟）
```

**`-race` 必须带 `CC=C:/mingw64/bin/gcc.exe`** —— 默认 PATH 上的 MinGW 在
`C:\Program Files\mingw64`，路径含空格，链接器解析不了。
（CI 是 ubuntu，用默认 gcc 即可。）

本仓库原先有一棵 `.gocache/` 缓存，其中残留**陈旧的 `rosetta@v0.5.1` 源码树**——
已于 2026-10-11 连同整个缓存目录删除。若再出现，读它会得出错误结论。

### 依赖拉取

goproxy.cn 的 sumdb lookup 对 `github.com/cn-maul/*` 返回 **404**，需
`GOSUMDB=off` + `GOPRIVATE=github.com/cn-maul/*` 才能拉新版本。
CI 侧建议同样配置，否则升级会红。

### 前端

pnpm 在此环境会失败。用 node 直接调：

```powershell
$node = "C:\Users\louis\.dsh\dsh-runtimes\dsh-primary-runtime\dependencies\node\bin\node.exe"
cd web
& $node node_modules\vue-tsc\bin\vue-tsc.js --noEmit
& $node node_modules\vite\bin\vite.js build
& $node sync-embed.mjs     # 改前端后必须跑，否则 CI 的 webui-sync 会红
```

**注意行尾**：Windows 上编辑 `.vue` 极易引入 CRLF，`ddbe9e9` 就是因此让 CI 红灯
（产物 hash 不一致）。改完前端用 `git diff --check` 确认无 CRLF。

### 代码风格（重要）

- 注释**中文、高密度、每个非显然决策都解释「为什么」**
- 注释里的数字**引用常量**而非重述（如 `MicrosPerCent`）
- 测试断言要钉**行为**而非猜测值；反例：不要断言「上游没被调用」除非证明它**能**被调用
- 探测（探针）用完即删，但**结论写进注释或报告**

## 七、审计报告位置

`AUDIT/` 下 20+ 份，**先读 `AUDIT/README.md`（索引）**，重点：

- **`audit-2026-10-11.md`** —— **最新全量审计（优先读这个）**：静态分模块审计 16 个包，
  6 项 P1 + 18 项 P2 + 9 项需人工确认风险。结论：**无 P0**，无鉴权绕过/SQL 注入/密钥泄漏/重复扣费
- `test-summary.md` —— 历史动态测试的总审计结论
- `test-datapath.md` / `test-billing.md` / `test-authz.md` / `test-perf.md` —— 四个方向的动态测试
- `fix-p1-perf.md` —— 性能修复（含一条对我给的 38.8× 前提的纠正）
- `magpie-benchmark.md` —— **对照分析，§四 是按收益排序的真实缺口清单**
- `AUDIT-sdk-malformed.md` 在 **rosetta 仓库**内（未提交）

每份都有「未能验证的部分」诚实清单 —— 别当它们是全覆盖。

**历史文档的处理（2026-10-11 定）**：

- `AUDIT/proxy.md` 已删除 —— 全篇锚点 `rosetta v1.0.0`，已严重过时；内容已被
  `audit-2026-10-11.md` 覆盖，**无需恢复**
- `MULTIUSER.md` **已从 git 历史取回并标注为「历史归档」** —— 曾删除，但代码与
  DESIGN.md 中有 **23 处注释以它的章节号为锚点**（`store.go:584` §5.1、
  `main.go:1497` §4.3、`auth.go:113` §5.1 等），删掉会让这些设计决策失去解释来源。
  顶部已加 `> [!WARNING]` 标注「非当前事实来源，判断现状请看源码与 DESIGN.md」
- `AUDIT/data.md` 顶部已加历史基线标注：它锚定旧树 `d529b29`，其中 **3 条结论已被修复**
  （PK 复合键、失败扣费回滚、亚分余数机制），**别当待办**

## 八、用户的实际环境

- 团队生产网关**不在本地**运行
- 约 4200 请求/天、950M token/天（≈0.049 req/s，占实测峰值 0.0019%）
- 缓存命中率 99%，成本 421.22 元/天（其中**输出 token 占约 73%**，单价 9.0 vs 缓存输入 0.1）
- 单价：`price_input=3.0` / `price_cache_hit=0.1` / `price_output=9.0`（元/百万）

## 九、发布剩余步骤

1. ⏭️ 提交 rosetta v1.1.1 升级（`go.mod` / `go.sum`，已在工作区）
2. ⏭️ 推送 + CI 首次运行；CI 绿了再打 tag
3. ⏭️ 给 CI 配 `GOPRIVATE=github.com/cn-maul/*`，否则下次升级会红

### ⚠️ 剩余风险

CI 的 `webui-sync` 与 `-race` 两个门禁在历史提交里都**真的红过**（`ddbe9e9`、`0b524a3`），
但发版链路本身仍缺少一次「推送后 CI 全绿」的确认记录。