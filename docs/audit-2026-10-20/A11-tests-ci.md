# A11 测试真实性与 CI 门禁审计

审计日期：2026-10-20  
审计范围：`cmd/`、`internal/`、`web/`、`.github/workflows/`、`Dockerfile`、仓库内测试与现有 A1-A10 报告。  
工作方式：仅读审计。未执行会重写 `internal/webui/dist` 的 `npm run build`、`sync-embed.mjs`，未执行任何 `git` 写操作。唯一写入文件为本报告。

## 方法与结论边界

- 按要求优先使用 `xerj_search(index="rosetta-gateway-docs", ...)`。本次 XERJ 能返回部分文件级片段，但 `xerj_map` 未返回可用索引字段清单，且检索结果不足以替代逐行证据；以下结论均用 `rg` 定位后逐行读源码复核。未查询 `ax-*` 或 `*`。
- 所有 severities 表示本次审计认定的测试真实性或门禁风险：P0=资金/数据完整性/安全/崩溃，P1=确定缺陷，P2=架构或设计一致性，P3=nit。测试存在但不覆盖关键路径，归入相应缺陷严重度的“测试盲区”。
- “测试未覆盖”不等于“生产必然触发”；凡需要额外环境、时序或运营配置才能确认的部分，明确标为“待确认”。凡已由当前源码证明的行为，写明触发条件。
- 既有 A1-A10 报告用于交叉核对，不把报告中的历史实测当作本次重新运行结果；本次实际执行结果见下一节。

## 实际执行结果

以下命令在本次会话中运行并通过：

```text
go build ./...
go vet ./...
go test -count=1 -timeout 300s ./cmd/... ./internal/... ./scripts/...
go test -race -count=1 -timeout 900s ./cmd/... ./internal/... ./scripts/...
npm test
npm run typecheck
```

`npm test` 为 5/5。`go test ./...` 在本地曾因 `work/audit-2026-10-20/probe/` 下的未跟踪审计探针被纳入包发现而失败；这不是仓库生产测试失败，CI 的干净 checkout 不包含该目录。为避免生成或改写 `dist`，本次没有执行 `npm run build` 或 `sync-embed.mjs`，因此“产物与源码一致”仅由 CI 配置和只读检查判断，未由本次重新构建复验。

## 严重度汇总

- P0：2 项。归档滚动窗口的边缘日少算；`cost_pending` 资金生命周期缺少真实生产链测试且现有实现存在未收费用路径。
- P1：6 项。half-open 只有顺序测试；真实生产 mux 未被测试；下游 SSE 写失败被吞；链耗尽测试复制逻辑；崩溃后的 `reserved_tokens` 无恢复测试；跨 Store 的配额竞争未覆盖。
- P2：5 项。Docker 发布绕过 race/前端门禁；前后端版本漂移；前端 API 契约测试缺失；慢测/固定 sleep 进入默认 CI；独立迁移工具无测试。
- P3：1 项。请求预算测试标签与实际覆盖不一致。

## P0 发现

### P0-1 归档测试把“午夜对齐”固化成唯一期望，默认/直接 API 的滚动窗口会少算边缘日

**证据：**

- 测试明确选择本地午夜作为 `from`，并在注释中把“UI 都是午夜对齐”当作真实形状：`internal/store/usage_archive_test.go:148-158`。它只覆盖 `winFrom = midnight - (DefaultRetentionDays+5)`、`winTo = midnight + 2d` 这一种窗口：`internal/store/usage_archive_test.go:155-158`。
- 同一测试的断言语境也全部建立在整日窗口上：`internal/store/usage_archive_test.go:160-223`；另一条计费端点回归同样使用午夜窗口：`internal/store/usage_archive_test.go:388-406`。
- 管理 API 的默认区间不是午夜到午夜，而是 `time.Now()-window` 到 `time.Now()`：`internal/admin/usage_handler.go:76-93`。默认查询 `from` 是滚动毫秒值，`to` 是当前毫秒值。
- 组织账单 API 也默认滚动 30 天并以 `UnixMilli(now)` 收口：`cmd/gateway/billing.go:290-319`；`end_time` 由秒乘 1000，仍有秒级边界：`cmd/gateway/billing.go:305-313`。
- 归档分支的边界规则是“整日包含”，源码自己承认窗口边缘会少算：`internal/store/usage_source.go:83-89`；SQL 条件为 `dayStartMs >= from` 且 `dayEndMs <= to`：`internal/store/usage_source.go:116-121`，`dayStartMs`/`dayEndMs` 定义在 `internal/store/usage_source.go:62-66`。
- 前端并非所有入口都午夜对齐：`rangeStart` 只对齐下界，`rangeBounds` 的 `to` 直接取 `now`：`web/src/range.ts:53-65`；历史查询确实使用该 `rangeBounds`：`web/src/api.ts:435-440`。
- 配置允许保留期 1 天，明确支持低用量部署：`internal/config/config.go:65-67`、`internal/config/config.go:420-426`；因此“默认 30 天不裁剪窗口内数据”并不能覆盖 `usage_retention_days < 查询窗口` 的实际部署。默认值是 30：`internal/config/config.go:312-315`。
- 压力测试的“全部历史”分支用 `from=0`，没有测试非零滚动边界：`internal/store/stress_quota_test.go:297-334`。因此现有套件无法把“明细部分命中、归档整天排除”这条差异压出来。

**结论：** 这是一个由测试选择窗口制造出的假绿。默认 30 天保留期且查询近 30 天时，归档边缘通常尚未裁剪；但当保留期短于查询范围、客户端直接使用非午夜 `from/to`，或组织账单默认滚动窗口跨过已归档日时，同一批数据在剪枝前会计入部分边缘日，剪枝后会被归档分支排除。费用、请求数、token、用户用量和配额 SUM 都可能静默偏小。不能把“默认值恰好 30 天”当作边界正确性证明。

**本次可防住的测试/门禁：**

1. 增加“默认 API 参数”测试：缺省 `from/to`、非午夜毫秒值、`usage_retention_days=1`，剪枝前后逐字段对账。
2. 增加客户端发送 `from=now-30d`、`to=now` 的契约测试，并断言归档边缘日的处理有显式、可解释的误差策略，而不是由测试选午夜绕过。
3. 对 `GetUsageStats`、`SumUserUsedTokens`、`SumTokensByDayForKey`、`SumUsageBuckets`、`SumCostBuckets` 同时断言 `requests/tokens/cost`，覆盖非午夜下界和上界。

### P0-2 `cost_pending` 测试绕过生产入口，资金、累计表和剪枝生命周期没有被任何一条测试串起来

**证据：**

- `seedCostPending` 直接执行 SQL 插入 `cost_pending`，注释明确说明“刻意不走 `CreateUsageRecord`”：`internal/store/cost_pending_test.go:30-49`。生产插入路径见 `internal/store/usage_dao.go:139-152`：查价失败时 `cost=0` 并写 `usage_state=cost_pending`。
- 测试只读取 `usage_state` 与 `cost_total`：`internal/store/cost_pending_test.go:59-91`。它没有断言余额、余数、`balance_charges`、`usage_totals`、日归档或配额累计。
- 生产扣费用 `CreateUsageRecordWithCost` 返回的 `cost`：`cmd/gateway/main.go:3293-3298`；同步降级也使用该值：`cmd/gateway/main.go:3318-3337`。
- `ChargeBalance` 对 `amountYuan<=0` 直接返回 `nil`，不写流水、不动余额：`internal/store/balance_dao.go:298-316`。因此 `cost_pending` 产生的 0 元会走“完全免单”分支。
- 重算只更新 `usage_records.cost_total` 和 `usage_state`：`internal/store/usage_dao.go:489-512`。没有余额补扣，也没有 `usage_totals` 差额更新。
- 后台循环只调用 `RecomputePendingCosts`：`cmd/gateway/main.go:755-790`；`RecomputePendingCosts` 从读池扫描 `cost_pending`，逐条调用重算：`internal/store/usage_dao.go:428-470`。
- `usage_totals` 触发器是 `AFTER INSERT` only：`internal/store/usage_archive.go:227-244`；重算 UPDATE 不会触发累计表差额。归档表 DDL 没有 `usage_state` 列：`internal/store/usage_archive.go:40-83`；归档 SQL 把当前 `cost_total` 复制到日汇总：`internal/store/usage_archive.go:630-672`；随后删除明细：`internal/store/usage_archive.go:678-685`。
- 既有 A8 已记录该资金链的实测结果：`work/audit-2026-10-20/A8-money-concurrency.md:254-302`，并在汇总中给出重算不补表 B、归档销毁待重算标记：`work/audit-2026-10-20/A8-money-concurrency.md:688-690`。

**结论：** 测试通过只证明“手工插入的 `cost_pending` 明细会被改成非零 `cost_total`”，没有证明一次真实查价失败经过插入、异步落库、扣费、重算、归档后仍能收齐钱。生产链确定存在三类后果：首扣为 0 时用户不被收费；重算后 `usage_totals` 仍为 0；若重算前发生剪枝，`cost_pending` 事实随明细删除而消失，日归档与终身累计都固化错误金额。触发需要读池故障或查价失败，属于资金完整性风险；是否必定发生取决于运营是否遇到该故障，但测试门禁确定没有防住。

**本次可防住的测试/门禁：**

1. 用可控读池注入 `usage_state=cost_pending`，必须走真实 `CreateUsageRecordWithCost`，不能用 SQL 造行。
2. 断言真实 `usageRecorder` 的 charge 链：余额、余数、`balance_charges`、`usage_totals`、`usage_daily_rollups` 和报表总额逐一对账。
3. 覆盖“重算前剪枝”与“重算后剪枝”两条时序，并要求待重算事实不丢失；若重算无法补扣款，必须阻止剪枝或保存可修复元数据。

## P1 发现

### P1-1 half-open 互斥测试是顺序测试，不是并发交错测试

**证据：**

- `TestClaimTargetProbe_AllowsOnlyOneAfterCooldown` 的运行顺序是：claim → 拒绝第二次 → success → 再次 claim，全部在同一 goroutine 中：`internal/upstream/pool_test.go:105-129`。
- 生产状态是共享 bool：`internal/upstream/upstream.go:564-578`。
- `RecordTargetFailure` 直接清 `halfOpen`：`internal/upstream/upstream.go:599-606`；`RecordTargetSuccess` 也直接清：`internal/upstream/upstream.go:615-621`；`ReleaseTargetProbe` 同样直接清：`internal/upstream/upstream.go:638-643`。

**结论：** 顺序测试只能证明 API 在一个预定义序列上可用，不能证明旧的在途请求 A 成功后不会清掉新探测请求 B 的名额。由于 `halfOpen` 是共享 bool 而非计数或代际编号，A/B 交错能让 B 被错误释放，重新出现惊群或错误跳过目标。现有测试没有覆盖该风险。

**补什么：** 用可控 channel 制造 A 在 B claim 后完成 success/failure/cancel/non-transferable 的交错；断言 B 的 claim 不被 A 的结果或 `ReleaseTargetProbe` 清掉，并覆盖两条生产收尾路径。

### P1-2 路由矩阵测试从源码正则提取路由，未执行真实生产 mux

**证据：**

- 测试用正则扫描 `cmd/gateway/main.go` 的文本，而不是运行 `main.go`：`internal/server/authz_matrix_test.go:88-130`；正则要求至少 50 条：`internal/server/authz_matrix_test.go:143-148`。如果新路由漏注册，源码里根本没有匹配项，测试集合随之变小，不能失败。
- 网关层鉴权测试自行创建探针 mux：`internal/server/authz_matrix_test.go:428-454`。
- 另一层测试手工重建“与 main.go 同一套注册”的 admin mux，并承认与生产仍有差异：`internal/server/authz_matrix_test.go:656-697`。
- E2E 数据面手工创建新的 `ServeMux` 并只挂一条入口：`cmd/gateway/e2e_datapath_test.go:1111-1115`；failover 直接调用 `handleIngress`：`cmd/gateway/failover_test.go:191-192`。
- 历史证据已经出现“schema/DAO/handler/前端都改了，但 main.go 漏注册，build/test 全绿”：`work/audit-2026-10-20/A1-architecture.md:454-461`。

**结论：** 鉴权矩阵证明的是“测试自己注册的路由”和“源码文本中出现的路由”，不是“生产进程实际暴露的路由”。漏注册、路由被覆盖、路径和前端不一致都能全绿。本次重点问题“为何 main.go 漏注册仍全绿”根因即此。

**补什么：** 将生产 mux/应用装配提取为由生产代码拥有的 `buildMux`/`buildApp`，测试对真实 factory 做路由契约；加入一条端到端“全新数据库 + 启动真实应用装配 + 请求关键 admin/data 路由”的测试。

### P1-3 下游 SSE 写失败被吞掉，现有 deadline 测试只测能力绑定

**证据：**

- `StreamSink` 的 `Event/Finish/Keepalive` 全部没有 error 返回值：`internal/outwire/sink.go:18-36`。
- `SSEWriter.WriteEvent` 调用 `fmt.Fprintf` 后无条件 `return nil`：`internal/outwire/openai_chat.go:114-127`；`WriteComment` 同样忽略错误：`internal/outwire/openai_chat.go:131-140`。
- Anthropic `writeRaw` 忽略 `fmt.Fprintf`，并让 `writeEvent` 无返回：`internal/outwire/anthropic.go:252-268`；其 keepalive 也忽略写错误：`internal/outwire/anthropic.go:271-281`。
- Responses `writeEvent` 忽略 `fmt.Fprintf`：`internal/outwire/openai_responses.go:119-135`。
- 生产流主循环直接调用 `sink.Event(ev)`、`sink.Keepalive()`、`sink.Finish(...)`，没有检查写失败：`cmd/gateway/main.go:2759-2765`、`cmd/gateway/main.go:2787-2801`、`cmd/gateway/main.go:2846-2849`。
- `write_deadline_test.go` 只有 binder 接口断言和 `rw != nil` 断言：`internal/outwire/write_deadline_test.go:22-58`，没有任何 error writer、阻塞 writer、deadline 到期后状态和 usage 记账断言。

**结论：** 测试只能证明“sink 满足方法签名，deadline 会被绑定”，不能证明“下游写失败时停止写、取消上游、正确收尾、只记一次账”。当前实现还会把写失败当成功返回，继续循环，导致 panic/死循环/错误 usage 状态中的至少一种风险；“是否一定 panic”取决于具体 writer 实现，标为待确认，而“错误被吞且无测试”已确定。

**补什么：** 对每个 sink 使用到期 deadline、阻塞 writer、`io.ErrShortWrite`/永久 error writer；断言 handler 不继续消费、上游 ctx 被取消、终止序列不谎报成功、usage 只记录一次且状态为失败/取消。

### P1-4 链耗尽测试复制生产分支，没有通过真实响应写出路径

**证据：**

- `TestChainExhaustedNeverWritesOK` 在测试内手写 `0/200 -> 502` 的逻辑：`cmd/gateway/usage_shutdown_test.go:165-181`。
- 生产侧对应逻辑是 `cmd/gateway/main.go:2246-2262`：同样把 `statusCode==0 || statusCode==200` 改为 502，再调用 `codec.WriteError`。

**结论：** 这条测试的通过只证明“复制出来的 if 分支能改 502”，不证明生产 `handleIngress` 在链耗尽时实际写出正确状态和错误信封。生产分支被删、条件改坏、`clientGone` 判断错或 `codec.WriteError` 未执行，测试仍可能绿。

**补什么：** 通过真实 ingression 路由/真实 `buildMux` 执行链耗尽场景，断言 HTTP 状态、协议错误信封、usage 记录和上游调用次数；测试不得复制条件表达式。

### P1-5 崩溃恢复的 `reserved_tokens` 完全没有测试

**证据：**

- Key 级测试只覆盖“老库缺列后重开能补列”：`internal/store/quota_reserve_test.go:156-210`；用户级同样只覆盖列迁移：`internal/store/user_quota_reserve_test.go:311-363`。
- 预占写入是持久化 `UPDATE ... reserved_tokens + ?`：`internal/store/key_dao.go:483-489`、`internal/store/user_dao.go:599-606`；释放只向下减：`internal/store/key_dao.go:507-513`、`internal/store/user_dao.go:617-623`。
- 全仓搜索 `reserved_tokens` 只找到写入、释放、迁移和断言，没有发现启动时的 in-flight 清空或修复路径；搜索结果覆盖 `internal/store`、`cmd/gateway` 和设计文档。
- 现有 E2E 测试只断言请求正常收尾后归零：`cmd/gateway/e2e_datapath_test.go:1021-1023`，没有模拟进程在 reserve 与 release 之间崩溃。

**结论：** 覆盖缺口确定。一个进程在预占后、收尾前被杀，持久化的 `reserved_tokens` 可能永久占住配额；是否已有未发现的恢复逻辑，标为“待确认”，但测试确定没有证明恢复。该问题会造成配额永久 429，属于确定的可用性风险。

**补什么：** 直接写库制造在途预占，关闭并重开 Store，运行生产启动装配（含后台任务），断言 stale reserve 被清零或安全过期，并断言无法通过重复恢复把并发中的有效预占清掉。

### P1-6 配额竞争只在单个 Store 内测试，双 Store 同文件只测了扣费

**证据：**

- 单 Store 并发测试：`internal/store/stress_quota_test.go:99-143`；混合收尾测试：`internal/store/stress_quota_test.go:156-221`；净记账测试：`internal/store/stress_quota_test.go:228-286`。
- 双 Store 测试确实存在，但只对 `ChargeBalance` 做两个 Store 的并发扣费：`internal/store/stress_billing_test.go:1265-1343`。
- 生产配额异常是 fail-open：key 级 `ReserveQuota` 错误直接放过：`cmd/gateway/main.go:1838-1855`；用户级同样放过：`cmd/gateway/main.go:1890-1901`。而 `Store` 的 DSN 注释明确把多实例/独立连接作为需要 `busy_timeout` 的场景：`internal/store/store.go:19-35`。

**结论：** 现有测试证明单连接写锁下单 Store 的预占不超发，不证明两个 Store 同时读同一文件时不会因 SQLITE_BUSY 或快照升级失败而 fail-open。用户/Key 配额的真实多实例行为没有门禁。

**补什么：** 两个 Store 在同一文件上并发 `ReserveQuota`/`ReserveUserQuota`，断言不超发、没有静默 busy fail-open；同时覆盖 reserve 与 release 交错，验证不会过度释放。

## P2 发现

### P2-1 Docker 发布 job 绕过 race 与前端同步门禁

**证据：**

- Docker workflow 的发布前门禁只有 gofmt/build/vet/test：`.github/workflows/docker.yml:28-57`；随后直接构建并 push：`.github/workflows/docker.yml:113-128`。
- 主 CI 的 race 在独立 job：`.github/workflows/ci.yml:160-161`；前端 typecheck/test/build/sync 检查在 `webui-sync`：`.github/workflows/ci.yml:188-234`。
- Dockerfile 只 `COPY . .` 并编译 Go，不重新构建前端；它嵌入仓库内已有的 `internal/webui/dist`：`Dockerfile:7`、`Dockerfile:22-38`。

**结论：** tag 发布可以绕过 race 和“dist 与源码同步”检查。即使主分支 CI 曾经通过，发布时所打的 tag 也可能指向未经同步或未过 race 的提交。Docker 注释自认这是为了缩短发布流程：`.github/workflows/docker.yml:35-40`。

**补什么：** 发布 job 复用与 `webui-sync` 完全相同的脚本，并执行 `git diff --exit-code -- internal/webui/dist`；在 tag 构建前加 `go test -race`。更稳妥的是让 Docker build 依赖同一套可复用 CI workflow，而不是复制一个较弱的门禁。

### P2-2 前端版本号与 lockfile 漂移，版本测试没有覆盖 lockfile

**证据：**

- `web/package.json:4` 为 `1.6.5`。
- `web/package-lock.json:3` 和 `web/package-lock.json:9` 为 `1.6.0`。
- 内嵌版本测试只比较 `web/package.json` 与嵌入式 JS bundle：`internal/webui/version_test.go:33-71`，没有读取 lockfile 或校验 tag/版本三处一致。

**结论：** 版本源已经漂移，Docker 的版本解析只读 `package.json`：`.github/workflows/docker.yml:59-77`，无法发现 lockfile 仍为旧版本。当前测试可以在版本不一致时全绿。

**补什么：** 将版本唯一来源固定为 package.json，CI 增加 package-lock 与 package.json 的版本一致检查；若发布 tag 存在，同时校验 tag 与两者一致。

### P2-3 前端只有工具函数测试，没有 API 契约测试

**证据：**

- `web/tests/` 只有一个测试文件，导入 `routeTrace.ts` 和 `modelPrice.ts`：`web/tests/routeTrace.test.mjs:1-49`；仓库内测试文件清单也只有该文件。
- 实际 HTTP 调用在 `web/src/api.ts:214-226`，`GET` 会拼 `/admin/api`；usage 多个端点集中在 `web/src/api.ts:754-810`。
- 前端依赖毫秒时间参数：`web/src/api.ts:754-758`；但前端测试没有导入 `api.ts`、没有 mocking fetch，也没有对后端 JSON key/时间单位/错误信封做断言。

**结论：** 前端 typecheck 能发现 TS 类型内部错误，不能发现运行时 API 路径、字段、毫秒 vs 秒、空数组/缺字段和错误 envelope 的契约漂移。后端测试通过也不等于前端调用方兼容。

**补什么：** 增加一层轻量 HTTP 契约测试：用仓库生产后端启动临时 server，或从后端 OpenAPI/路由表生成契约；对 stats、usage 各分组、history、billing、prune 验证 URL、字段和时间单位。前端单测至少覆盖 `fetch` 路径、参数和异常 envelope。

### P2-4 慢测/固定 sleep 默认进入 CI，隔离性和 flakiness 风险未被处理

**证据：**

- 多个性能测试只在 `testing.Short()` 下跳过：`cmd/gateway/perf_test.go:456-459`、`:541-544`、`:669-672`；其他 perf 文件同样用 Short 跳过：`cmd/gateway/perf_batch_test.go:90`、`cmd/gateway/perf_leak_test.go:161` 等。
- 固定 sleep 既有性能环境清理：`cmd/gateway/perf_test.go:397-403`，也有 E2E“给异步扣费留时间”：`cmd/gateway/e2e_datapath_test.go:1413`、`:1754`、`:1865`。
- CI 全量 Go 测试没有 `-short`：`.github/workflows/ci.yml:128-134`；race 同样没有：`.github/workflows/ci.yml:160-161`。

**结论：** 默认 CI 会跑性能/压力测试和固定 sleep，给共享 runner 引入明显时序敏感。不能仅凭本次通过就认定无 flaky；测试隔离、超时和资源泄漏风险仍在，具体哪一条 CI 会偶发红标为“待确认”，但配置层面的混跑已确定。

**补什么：** 将 perf/diag/stress 移入显式 long job；默认 job 用 `-short` 或按 package/tag 选择常规测试。把 sleep 改成事件/轮询/可注入时钟，禁止用固定 sleep 证明“没有发生某事”。

### P2-5 独立迁移工具没有测试，CI 的 `./...` 只证明能编译

**证据：**

- `cmd/migrate-legacy/` 只有 `main.go` 一个 Go 文件；入口和 `migrate` 在 `cmd/migrate-legacy/main.go:39`、`:186`。
- 迁移逻辑明确变换旧/新 master key，并保留 provider/model/routes 资产：`cmd/migrate-legacy/main.go:1-13`、`:186-205`。
- CI 的 `go test ./...` 会覆盖该目录的编译，但没有任何 `_test.go`：`.github/workflows/ci.yml:128-134`。

**结论：** 迁移工具的数据变换、幂等、dry-run、空库检查和失败回滚没有回归门禁。仅编译通过不能证明升级一次旧库不丢配置。

**补什么：** 用最小 v1.4.1 新旧 fixture 文件验证 decrypt/re-encrypt、provider/model/routes/targets/credentials 数量、dry-run 不写、已有目标库拒绝、事务失败回滚。

## P3 发现

### P3-1 请求预算测试的标签与实际覆盖不一致

**证据：**

- `request_budget_test.go:29-91` 只测试 `totalRequestBudget` 的计算、配置跟随和 nil snapshot，不测试请求执行。
- 名字和注释带“总请求预算真的会掐断”的 E2E 用例实际设置 TTFT 超时，测试的是首字超时：`cmd/gateway/e2e_datapath_test.go:1591-1600`。

**结论：** 阅读测试名和日志容易误以为已经覆盖“总预算到期后的完整收尾”。实际覆盖仍偏向计算函数和 TTFT 看门狗；这是可维护性和审计可读性问题，不是独立的生产资金缺陷。

**补什么：** 将该断言明确标为 TTFT；另加可控 clock 的总预算到期测试，断言 `DeadlineExceeded`、无 200 假成功、usage 状态、预占释放和上游取消。

## 测试真实性矩阵

| 区域 | 真实性判定 | 证据与边界 |
|---|---|---|
| 归档核心聚合、幂等、批量节流 | 真生产代码 + 真 SQLite | `usage_archive_test.go:137-223`、`:468-594`；但窗口只取午夜，不能覆盖滚动边缘。 |
| 费用待重算 | 部分假 / 部分真 | `cost_pending_test.go:30-49` 手工 SQL 造状态；`RecomputePendingCosts` 调真实 DAO，但没有穿过插入、扣款、累计表和剪枝。 |
| 协议矩阵 | 真生产 ingress codec | `protocol_matrix_test.go:24-136`、`:172-218` 覆盖 3×3 非流式/流式。 |
| effort 转换 | 真生产转换函数 | `effort_levels_test.go:236-312`、`:346-399`、`:502-557`；没有覆盖预算到期/总预算。 |
| SSE 截止时间 | 只测类型/绑定 | `write_deadline_test.go:31-58`；没有真实写阻塞或写失败。 |
| failover/截断/取消 | 基本真实 | `audit_regression_test.go:24-159`、`failover_test.go:332-530`、`e2e_datapath_test.go:624-784`、`:963-1023`、`:1727-1773`。 |
| 链耗尽写出 | 复制实现 | `usage_shutdown_test.go:165-181` 与生产 `main.go:2246-2262` 重复条件。 |
| usage flush 重试 | 真 SQLite + 注入 batch 失败 | `usage_flush_retry_test.go:15-150`；注入只替代批量边界，成功降级仍落真实库。 |
| 鉴权矩阵 | 部分真实、路由来源不可靠 | `authz_matrix_test.go:97-147` 解析源码；`:428-454` 探针 mux；`:656-696` 手工重组 mux。 |
| 配额并发 | 单 Store 真 SQLite | `stress_quota_test.go:99-286`；双 Store 只有扣费：`stress_billing_test.go:1265-1343`。 |
| 前端 API | 缺失 | `web/tests/routeTrace.test.mjs:1-49`，只测试 routeTrace/modelPrice。 |

## CI 与发布门禁矩阵

| 门禁 | 主 CI | Docker tag 发布 | 结论 |
|---|---|---|---|
| `gofmt` | 有，`ci.yml:106-112` | 有，`docker.yml:47-57` | 通过 |
| `go build` / `go vet` | 有，`ci.yml:114-117` | 有，`docker.yml:55-56` | 通过 |
| 依赖 tidy/verify | 有，`ci.yml:119-126` | 无 | 发布若跳过 tidy 检查，依赖清单可能已脏但仍出镜像。 |
| 全量普通测试 | 有，`ci.yml:128-134` | 有，`docker.yml:57` | 通过，但不含 `-short` 隔离。 |
| race | 有，`ci.yml:160-161` | **无** | Docker tag 可以在 race 未通过时发布。 |
| 前端 typecheck | 有，`ci.yml:191-192` | **无** | 发布路径无前端门禁。 |
| 前端单测 | 有，`ci.yml:194-206` | **无** | 发布路径无前端单测。 |
| 前端 build + sync | 有，`ci.yml:208-234` | **无** | Docker 直接使用仓库 `dist`，可能发布旧 UI。 |
| 迁移工具测试 | 没有 `_test.go` | 不适用 | 只有编译级覆盖。 |

## 按优先级补测试/门禁，能防住哪类缺陷

| 优先级 | 要补的测试或门禁 | 直接防住的缺陷 |
|---|---|---|
| P0 | 默认参数 + 非午夜 + 短保留期剪枝对账 | 归档边缘日少算 requests/tokens/cost，配额和账单静默偏小。 |
| P0 | 真实 `CreateUsageRecordWithCost` 的 pending 资金链 | 查价失败免单、重算不补余额、累计表偏差、归档销毁待重算标记。 |
| P0 | 让发布依赖完整 CI 成功状态 | Docker tag 携带未过 race/前端同步/测试的提交。 |
| P1 | half-open 代际/交错测试 | 旧请求清掉新探测名额，惊群或长期跳过健康目标。 |
| P1 | 生产 `buildMux`/`buildApp` 契约测试 | main.go 漏注册、路径覆盖、前端调用不存在的路由仍全绿。 |
| P1 | sink 写失败/deadline/短写测试 | 下游写失败被吞、handler 泄漏、错误 usage 与重复消费。 |
| P1 | 真实链耗尽响应测试 | 生产兜底分支改坏但复制逻辑测试仍绿。 |
| P1 | crash/reopen stale reserve 测试 | 进程崩溃后 `reserved_tokens` 永久占额度，用户长期 429。 |
| P1 | 双 Store 配额竞争测试 | SQLite busy 触发 fail-open，超额放行。 |
| P2 | 前端 API 契约 + lockfile 版本检查 | 字段/时间单位漂移、tag 与包版本不一致。 |
| P2 | long test 与默认 CI 分离 | 共享 runner 上的固定 sleep/性能测试 flaky。 |
| P2 | migrate-legacy fixture 测试 | 升级时丢 provider/model/routes 或凭据解不开。 |
| P3 | 总预算 execution 测试和标签修正 | 误以为已覆盖总预算收尾。 |

## 已完成核验与待确认边界

- **已确定：** 归档测试的窗口确实是午夜对齐；API 默认确实是滚动窗口；来源确实只包含整天归档。
- **待确认：** 某个生产部署是否会在滚动窗口跨过已归档日；这取决于 `usage_retention_days`、查询窗口、调用客户端和剪枝时点。报告不把“所有部署必然少算”写成事实。
- **已确定：** `cost_pending` 测试不经过生产插入；重算仅更新明细；余额和 `usage_totals` 无补记代码。
- **待确认：** 特定的 `http.ResponseWriter` 在 `fmt.Fprintf` 失败后是否 panic；不同 server/writer 行为可能不同。但“错误被忽略且没有测试”已确定。
- **待确认：** 是否存在本报告检索未发现的 `reserved_tokens` 启动修复逻辑。全仓检索没有发现；缺测试和残余风险确定。
- **已确定：** Docker 发布路径不运行 race、前端构建或 dist 同步检查。
- **已确定：** `web/package.json` 与 `web/package-lock.json` 版本不同。
- **未本次复验：** `npm run build`/`sync-embed.mjs` 的实际差异；为避免改写工作树，未运行。发布前仍应由 CI 做只读 diff 门禁。

## 已验证无问题的区域（含理由）

- **主 CI 的 Go 基础门禁真实有效：** `.github/workflows/ci.yml:106-134` 确实执行 gofmt、build、vet、tidy/verify 和全量测试；`ci.yml:160-161` 确实执行 race。本次 build、vet、普通测试和 race 均通过。
- **3×3 协议矩阵不是 mock 转换器：** `cmd/gateway/protocol_matrix_test.go:24-136` 与 `:172-218` 通过真实 ingress codec 和 HTTP 假上游，证明了 chat/responses/anthropic 入站与三种上游协议的非流式/流式路径，不只是复制转换结果。
- **effort 覆盖到真实转换落点：** `cmd/gateway/effort_levels_test.go:236-312` 断言 Anthropic `BudgetTokens`、`Effort` 和 raw 字段；`:346-399` 覆盖 unsupported/unconfigured/not-requested 三态；`:502-557` 从三个入站 decoder 取原始档位。
- **failover、断流和取消有较强的真实链覆盖：** `cmd/gateway/audit_regression_test.go:24-159` 覆盖上游先 200 后断流触发熔断；`cmd/gateway/failover_test.go:332-530` 覆盖客户端在请求前/中途断开、流式 5xx、TTFT 和探测名额释放；`cmd/gateway/e2e_datapath_test.go:624-784`、`:963-1023` 覆盖链顺序、已提交流不重试、客户端取消传播和配额释放。
- **usage flush 重试/降级不是纯 mock：** `cmd/gateway/usage_flush_retry_test.go:15-150` 只注入批写失败，成功/降级仍写入真实 SQLite，并验证瞬时失败、持续失败、关停未排空和连续失败不丢。
- **归档核心不变量覆盖良好（除滚动边缘 P0-1 外）：** `internal/store/usage_archive_test.go:137-223` 验证剪枝前后 stats、范围过滤和用户 SUM；`:468-496` 验证重复剪枝幂等；`:498-594` 验证 3 万行积压、2 万上限和分批不重复聚合。
- **普通测试与 race 当前可运行：** 本次实际运行的 `go test -count=1 -timeout 300s ./cmd/... ./internal/... ./scripts/...` 与 `go test -race -count=1 -timeout 900s ./cmd/... ./internal/... ./scripts/...` 均通过；这证明当前没有已知的 race 红灯，但不能替代上述缺失场景。
