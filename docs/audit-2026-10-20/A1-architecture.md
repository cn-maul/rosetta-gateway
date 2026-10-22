# A1 审计报告：架构、模块边界、文档一致性、近期改动根因归类

- 审计对象：`rosetta-gateway`
- 审计基线：`main` = `ea19ce1`（领先 `origin/main` 2 个提交）
- 审计日期：2026-10-20
- 性质：**只读审计**。本次未修改/创建/删除任何 git 跟踪文件，未执行任何 git 写操作。
- 检索方式：`go list` 依赖图、`rg` 精确匹配、MCP `xerj_search` 语义检索、针对性 `go test -run`。

---

## 0. 结论摘要

| 级别 | 数量 | 说明 |
|---|---|---|
| P0 | 0 | 未发现资金丢失/数据丢失/越权/崩溃级缺陷 |
| P1 | 3 | 确定性缺陷（含 1 条会直接误导后续 agent 的文档漂移） |
| P2 | 9 | 架构/设计/一致性问题，持续产 bug |
| P3 | 8 | 文档漂移 / nit |

**最核心的判断**：这个仓库的"修一点不管用"有明确的三条根因，且**都不是"某个函数写错了"能解释的**：

1. **DESIGN.md 已经开始说谎**，而且是最关键的几节（§10 健康态落库、§11.4 RPM 计数、§13.2 路由清单、§15 目录结构、§3 架构图里的 `quota` 包）。文档是后续 agent 的唯一规格来源，文档漂移会让每个新 agent **朝错误方向打补丁**。这是"修一点不管用"的第一因。
2. **`cmd/gateway/main.go` 3532 行是整个系统的单点真相**：配额预占、限速、故障转移链、usage 记账、计费预检、编解码注册、优雅关闭、以及两个后台循环全在里面。每一轮"局部修补"都在往这个文件里加，而它没有可测试的边界（`handleIngress` 需要 `*store.Store` + `*upstream.Pool` + `*snapshot` + `*config.Config` + `*ratelimit.Limiter` + `*usageRecorder` 六个具体类型）。**P2-1 是"修一点不管用"的第二因。**
3. **前端产物与源码是两条流水线**（`web/src` → `npm run build` → `sync-embed.mjs` → `internal/webui/dist`），而 git 历史里"改前端"与"重建产物"是两个独立提交。近 6 次 `chore(webui)` 重建与 6 次前端功能提交交织（`2414a4e` → `ea19ce1`、`38a9b4e` → `85e7770`），**当前工作树还正处在"源码已改、产物未重建"的中间态**。这是第三因（详见 §6）。

---

## 1. P1 清单（确定性缺陷）

### P1-1　DESIGN §10 明写"健康态不落库"，代码却把凭据冷却**持久化**到 SQLite

- **文档原文**（`DESIGN.md:1034`）：
  > "**健康态是纯运行时的，不落库、重启即清零**：冷却与熔断只活在 `upstream.Pool` 的内存里。`provider_credentials.cooldown_until / status` 与 `routes` 上的旧策略列**不是**事实来源……"
  > "运维影响：**重启网关会清空全部健康态**（表现为故障目标立刻又被试一次），这是预期行为，不是故障。"
- **代码事实**：
  - `internal/upstream/upstream.go:463-495` `MarkCredentialCooldown` 显式落库：
    ```
    // 必须落库：冷却是运行时状态，而池会被任何 admin 写操作重建。
    if store != nil {
        if err := store.SetCredentialCooldown(context.Background(), credID, "cooling", until); err != nil {
    ```
  - `internal/upstream/upstream.go:500-528` `RecordCredentialSuccess` 同样落库 `"healthy"`。
  - `cmd/gateway/main.go:207` `pool.SetCooldownStore(db)` —— 生产路径**必然**注入 store，落库是常态而非可选。
  - `internal/upstream/upstream.go:354-366` 重建时从库里读回 `sc.CooldownUntil` / `sc.Status` 灌进内存 `CredentialEntry`。
  - `internal/store/credential_dao.go:115-123` `SetCredentialCooldown` 是真实写路径（`UPDATE provider_credentials SET status=?, cooldown_until=?`）。
- **为什么是 P1 而不是 P3**：文档给的是**可执行指令**。按 §10 的描述，运维去 `UPDATE provider_credentials SET status='healthy'` 想"手工解封一把 key"是有效的；实际上 `upstream.go:365-366` 的 `Status: sc.Status` 会把这个值读回来，**手工解封会生效但同时被文档否认**；反过来，"重启清空健康态"这条承诺也不成立——重启后冷却仍从库里恢复。
- **文档同时自相矛盾**：`DESIGN.md:998`（§10 前段）明写"冷却回写（**已启用**，`outwire.CredentialCooldown`）：401/403 冷却 30 分钟；402 冷却 1 小时……"，与 §10 末尾的"纯运行时的，不落库"直接打架。同一节里两个说法相反。
- **影响面**：所有依赖"重启即清零"做运维决策的人（包括下一轮 agent）会做出错误判断。
- **待确认**：无。这是代码与文档的直接冲突，两侧证据都在上面。

### P1-2　DESIGN §11.4 明写"RPM 被拒的请求**同样计数**"，代码明确**不计数**

- **文档原文**（`DESIGN.md:1129`）：
  > "| RPM | 内存固定窗口计数器（每分钟一个桶，`internal/ratelimit`），Key 维度；**被拒的请求同样计数** |"
- **代码事实**：`internal/ratelimit/limiter.go:85-98` `AllowRPM`：
  ```
  // 被拒绝的请求**不**计数。这与字面直觉相反，但是对的：若计数，客户端在窗口
  // 内疯狂重试会让 b.requests 只增不减，把整个窗口永久锁死……
  if b.requests >= int64(rpmLimit) {
      return false, windowRemaining(now)
  }
  b.requests++
  ```
- **代码里的注释已经承认了与文档的冲突**：`cmd/gateway/main.go:1750-1756`：
  > "（此前这里写的是「被拒的请求同样计数」，与实现相反。实现是对的，错的是注释——留着会诱导后人「修反」成 count-on-reject。）"
  也就是说，**冲突已被发现，但只改了代码注释，没改 DESIGN.md**。
- **为什么是 P1**：这是唯一一处"代码正确、文档错误、且文档措辞会让下一个 agent 去把代码改错"的地方。§11.4 的表格是配额/限速的口径总表，agent 若以它为准实现新的限速维度（per-model / per-user，DESIGN §11.5 明确列为"后续路线"），会照抄"被拒同样计数"，直接复现"一个配错 RPM 的客户端把自己彻底堵死"这个已被修过的 bug。
- **同类**：§11.4 表格 TPM 行写"被拒的请求不预占"（`DESIGN.md:1130`），这一半与代码一致（`limiter.go:106-127`）。所以同一行表格里两半的语义相反，只有一半对。

### P1-3　`internal/webui/dist` 当前处于"新旧资产并存"状态，embed 产物与 `web/src` 不同步且未提交

- **工作树事实**（`git status --porcelain -uall`）：17 个 `internal/webui/dist/assets/*.js|css` 显示为 ` D`（已跟踪但被删除），同时 17 个**新 hash 名**的同名文件显示为 `??`（未跟踪），`internal/webui/dist/index.html` 为 ` M`。
  - 被删的旧资产：`internal/webui/dist/assets/index-CqXLBNJx.js`、`UsageStats-BYUqPI-D.js` 等（HEAD 中存在）。
  - 新增未跟踪资产：`internal/webui/dist/assets/index-m5WBl62f.js`、`UsageStats-mKQs0NDv.js` 等。
  - `internal/webui/dist/index.html:26` 已指向新的 `./assets/index-m5WBl62f.js`。
- **源码侧**：`web/src/views/UsageDetail.vue` 有一处**未提交**改动（`git diff -- web/src/views/UsageDetail.vue` 显示新增了 `.logs-table .model-name.mono` 等 CSS 规则与 `model-column` 宽度 360→300 的调整）。
- **为什么是 P1**（而非 P3）：
  1. `cmd/gateway/main.go:581` `fs.Sub(webui.StaticFS, "dist")` 打包的是**入库的** dist。当前工作树若直接 `go build`，`go:embed` 拿到的是"旧的被删文件 + 新的未跟踪文件 + 指向新文件的 index.html"的混合体——**能编译**（因为新文件在磁盘上），但任何未 `git add` 的构建产物丢失都会让二进制引用不存在的资源。
  2. CI 的 `webui-sync` job（`.github/workflows/ci.yml:208-234`）用 `git diff --exit-code --stat -- internal/webui/dist` 判定。这条命令**只看已跟踪文件的 diff**：新 hash 文件是未跟踪的（不进 diff），被删的旧文件是已跟踪的（进 diff）→ CI 会红，报"产物与源码不一致"。也就是说**当前工作树状态直接违反 CI 门禁**。
  3. `scripts/check-repo-hygiene.ps1` 实测**通过**（`已跟踪文件 : 336 个`，索引干净）——它只看索引，对"工作树与索引不一致"完全无感，说明卫生脚本的口径与 CI 门禁不重合（详见 §7）。
- **待确认**：无。

---

## 2. P2 清单（架构/设计/一致性问题）

### P2-1　`cmd/gateway/main.go` 是 3532 行的单点真相，承担 12 类互不相干职责

**实际职责清点**（附行号）：

| # | 职责 | 位置 | 是否应下沉 | 当前耦合点 |
|---|---|---|---|---|
| 1 | 进程装配与生命周期（配置、时区、主密钥、DB、优雅关闭） | `main.go:98-887` | 部分应下沉到 `internal/bootstrap` | `main()` 有 790 行，直线代码 |
| 2 | HTTP 路由表（管理端约 60 条端点 + 5 个数据面入口 + SPA + healthz） | `main.go:238-260`, `main.go:370-517` | **是**，应下沉到 `internal/admin` 或独立 `internal/routes` | 路由表在 `main()` 里逐条 `adminMux.HandleFunc`，任何新端点都要改 main |
| 3 | 配额预占（key 级 + 用户级原子预占） | `main.go:1816-1919` | **是**，应下沉到 `internal/quota` 或 `internal/store` | 直接调 `db.ReserveQuota` / `db.ReserveUserQuota`，`rateCommit` 收尾逻辑内联在 main |
| 4 | 限速（RPM/TPM）编排 | `main.go:1749-1814` | **是** | `limiter.AllowRPM` / `ReserveTPM` 与 `rateCommit` 分散在 main |
| 5 | 故障转移链 | `main.go:2006-2320` | **是** | 熔断/探测名额/冷却回写/失败轨迹全在 main；`upstream.Pool` 只提供原语，策略在 main |
| 6 | usage 记账（异步落库 + 重试 + 扣费） | `main.go:3108-3516` | **是**，应下沉到 `internal/usage` | `usageRecorder` 定义在 main，`charge` 直接操作 `store` |
| 7 | 计费预检（余额估算） | `cmd/gateway/billing.go:473-538` | **是** | `estimateBalanceCost` **复制**了 `store.priceUsage` 的计价公式（见 P2-2） |
| 8 | 协议编解码注册 | `main.go:1629-1736`（三个 codec） | **是**，应下沉到 `internal/inwire`/`internal/outwire` | `ingressCodec` 接口定义在 main，三个实现各 20~30 行 |
| 9 | 运行时重建（池 + 快照） | `main.go:889-1074` | **是** | `runtimeReloader` 在 main，是 `upstream`+`snapshot`+`store` 三者的胶水 |
| 10 | 后台循环（用量剪枝 / 费用重算 / 探测调度） | `main.go:693-821` | **是** | 三个 goroutine 内联在 main，与 `internal/probe` 的职责重叠 |
| 11 | `/v1/models` 列表与详情 | `main.go:3518-3760` | **是** | `handleListModels` / `handleGetModel` 在 main |
| 12 | 数据面兼容端点（billing/organization） | `cmd/gateway/billing.go:58-355` | 是 | 4 个 one-api 兼容端点 |

**与 `internal/*` 重复实现的部分**：

- **计价公式重复**：`cmd/gateway/billing.go:512` `cost := (float64(inTokens)*inPrice + float64(maxOut)*outPrice) / 1_000_000.0` 与 `internal/store/usage_dao.go:341` `priceUsage(...)` 是两套独立实现。`billing.go:469-472` 的注释自己承认：
  > "计价公式与 store.priceUsage **同源**……这里不复用 store 的函数是因为 priceUsage 是包私有的，而热路径在 cmd/gateway 层。**两处若漂移，后果比不预检更糟**"
  这是一个**明确的、被写在代码里的"已知会漂移"的技术债**。而且两处口径其实**已经不同**：`store.priceUsage` 处理了缓存命中价（`price_cache_hit`，回落 `price_input`）和 `MAX(input-cached,0)`，而 `estimateBalanceCost` **完全不计缓存命中**（`billing.go:490-491` 只用 `PriceInput`/`PriceOutput`）。注释说"同源"是**错的**——这是缓存命中场景下的**系统性偏差**，见 P2-2。
- **Token 估算重复**：`cmd/gateway/main.go:1610-1627` `estimateRequestTokens` 与 `cmd/gateway/billing.go:474-489` `estimateBalanceCost` 里的前半段是**逐字相同的字符串拼接循环**（system + messages×blocks×4字段 + tools×3字段），只有后续计价不同。两处共约 30 行重复。
- **看门狗/超时口径**：`main.go:2441-2582` 有一组 `failoverMaxTargets` / `failoverFailureThreshold` / `nonStreamTimeout` / `firstTokenTimeout` / `streamIdleTimeout` / `totalRequestBudget`，每个都是"快照优先、config 兜底"的两段式。这组逻辑在 `internal/config/config.go:516-531` 与 `internal/store/settings_dao.go:192-194` 已经有第三份字段定义。
- **熔断状态的两份口径**：`internal/upstream/upstream.go:408-431` `Install` 保留熔断状态跨重建（"运维改一个模型的 context_width，不该让正在熔断中的坏上游立刻复活"），但 `DESIGN.md:1032` 写的是"目标熔断表与凭据冷却在每次重建池时一并清零——两层语义一致"。**代码与文档在这里也是反的**（代码保熔断；凭据冷却则落库保留）。这是 P1-1 的同源问题。

**对可测试性的影响**：

- `handleIngress`（`main.go:1738`）的闭包捕获 5 个外部依赖（`pool`、`cfg`、`usage`、`limiter`、`codec`），加上 `usage.db` / `usage.logger`（`main.go:1739` 从 `usage` 里掏出来）——**6 个具体类型，没有一个接口**。写单元测试必须真的开 SQLite、真的建 `upstream.Pool`。
- `rateCommit`（`main.go:1518-1540`）持有 `*store.Store` + `*ratelimit.Limiter` + `*slog.Logger` + 4 个标量，**预占与退预占跨两个包**（`ratelimit` 管窗口、`store` 管终身配额），任何一条路径漏掉 `commit(0)` 就是一次**永久**的终身额度泄漏（不是窗口自愈）。`main.go:1908`、`:1924`、`:1948`、`:2002`、`:2058`、`:2098` 共有 **6 处**手工 `rate.commit(0)` 调用点，每加一条早退分支就多一个漏点。
- `cmd/gateway` 下的测试文件是 `e2e_datapath_test.go`（1725 行）、`perf_test.go`（1330 行）、`perf_leak_test.go`（855 行）——**全是端到端/压测**，没有针对配额、限速、故障转移策略的单元测试。`internal/ratelimit` 有单测，`internal/store` 有单测，但**策略层（"什么时候转移、什么时候记失败"）没有单测**，因为它住在 main 里。

**"如果继续在这文件里打补丁会发生什么"——具体风险清单**：

1. **每加一条早退分支，漏一次 `rate.commit(0)` = 一次永久额度泄漏**。现在已经有 6 个这样的点，且散落在 200 行范围内、夹杂 20~40 行注释块。审计发现的模式（`main.go:1755-1756` 那条"留着会诱导后人修反"的注释）说明这类点**已经被修反过一次**。
2. **`usageRecorder` 继续膨胀**。它现在有 `charge` / `record` / `flush` / `flushOneByOne` / `markFlushFailed` / `HasPendingUsage` / `wait` 七个方法（`main.go:3372-3516`），扣费逻辑（`charge`，`main.go:3372-3434`）与落库逻辑（`record`/`flush`）已经可以独立成包。下一轮"计费 bug"多半落在这里，而这里**没有单测**。
3. **端点表继续膨胀**。60 条管理端点写在 `main()` 里，`cmd/gateway/main.go` 已 3532 行，下一个 agent 加端点时倾向于继续加在 main。`DESIGN.md:1597`（§15）甚至把这个现状写进了文档：
   > "`cmd/` ├── `gateway/` 装配与启动；**配额预占、限速、故障转移链、usage 记账都在这里**（请求热路径，非独立包）"
   ——**文档把架构缺陷描述成了既定设计**，下一个 agent 读到会认为"本来就该在这里"，于是继续在这里加。
4. **两个后台循环的关停顺序会越来越难推理**。`main.go:702-821` 现在有三个 goroutine（prune / recompute / probe），各自有 done channel 和超时（5s / 2s / 2s）。再加一个（比如"按天重算配额"）就要动这 60 行，而这段代码的错误只在 SIGTERM 时暴露。
5. **策略无单测 = 每次改动都要靠 e2e 验证**。`cmd/gateway/e2e_datapath_test.go` 1725 行的存在本身就是信号：策略复杂度已经大到只能端到端测。

### P2-2　余额预检与实际扣费是两套计价公式，且**缓存命中被预检漏算**

- `cmd/gateway/billing.go:490-512`（预检）：
  ```
  inPrice := m.PriceInput
  outPrice := m.PriceOutput
  ...
  cost := (float64(inTokens)*inPrice + float64(maxOut)*outPrice) / 1_000_000.0
  ```
  **不计 `price_cache_hit`**，也**不减 `cached_tokens`**。
- `internal/store/usage_dao.go:321-341`（实际扣费）：
  ```
  缓存未命中输入 = MAX(input - cached, 0) × price_input
  缓存命中输入   = cached × price_cache_hit（<=0 时回退 price_input）
  输出           = output × price_output
  ```
- **后果方向**：预检**高估**成本（把缓存命中按全价算）→ 误拒（用户"明明够钱"被 402 拒）。这与 `billing.go:470-472` 注释里担心的方向相反，但仍是缺陷。
- **但更要紧的是 `internal/store/usage_archive.go:293-301`** 里的归档回填 SQL 是**第三份**计价实现，与前两处字面不同（SQL 里用 `MAX(input-cached,0)` + `price_cache_hit` CASE）。三处口径、两种数学。
- **结论**：三份计价实现（`billing.go:512` / `usage_dao.go:341` / `usage_archive.go:297`），其中两份正确处理缓存命中、一份不处理。`DESIGN.md` 里没有一处描述缓存命中对预检的影响。

### P2-3　模块依赖方向：`internal/admin` 反向依赖 `internal/server`，形成环向压力

`go list` 依赖图（`internal/admin` 的 imports）：
```
internal/admin -> internal/server, internal/store, internal/snapshot, internal/routing,
                   internal/upstream, internal/config, internal/crypto, internal/effort
internal/server -> internal/store, internal/userauth
internal/snapshot -> internal/routing, internal/store
internal/upstream -> internal/store, internal/config, internal/crypto
internal/store  -> internal/effort
internal/routing -> internal/effort
internal/probe  -> internal/store, internal/upstream, internal/config, internal/outwire
```

**问题 1：`admin` → `server` 的方向是反的。**
`internal/admin` 用了 40+ 次 `server.UserFromContext(r.Context())`（`internal/admin/user_handler.go:197,289,538,581,...`）、`server.FailureThrottle`、`server.IsAdmin`、`server.SameOrigin`、`server.WriteForbidden`、`server.ClientIPOf`、`server.TouchLogin`、`server.SetSessionCookie`、`server.ClearSessionCookie`。
而 `internal/server` 里有 `AdminGateGuard`（`internal/server/user_auth.go:222`）——它**必须拿到 adminMux 的真实路由表**才能判定白名单：
```
// cmd/gateway/main.go:526-531
adminGuarded := server.NewUserAuth(sessionMgr, db).
    Guard(server.AdminGateGuard(adminMux, adminMux))
```
`server` 依赖"admin 的路由表"这个**运行期数据**，而 `admin` 依赖 `server` 的**类型**。这形成一个事实上的循环（server→admin数据，admin→server类型），只是被 `main.go` 的装配顺序掩盖了。后果：`internal/admin` 里任何新 handler 想用 `server` 的东西都"很方便"，而 `server` 那边想改白名单机制就必须知道 admin 的路由结构。**鉴权口径的唯一定义点因此从 1 个变成了 3 个**（见 P2-4）。

**问题 2：`internal/probe` → `internal/upstream` 的方向把"健康状态"分成了两套所有权。**
`internal/probe/runner.go:174` 复用 `upstream.TestUpstreamModel`，`internal/probe/runner.go:186,199` 复用 `outwire.UpstreamErrorDetail` / `outwire.MapUpstreamError`。
`upstream` 自己持有熔断状态（`targetHealth`，`internal/upstream/upstream.go:57-65`）和凭据冷却（`CredentialEntry.Status/CooldownUntil`）。
`probe` 持有**另一套**健康判定（`probe.Classify`，`internal/probe/runner.go:48`，写 `model_probes` 表）。
两者**互不同步**——`DESIGN.md:1333-1335` 明确说这是有意的：
> "探测**绝不写 pool 的冷却/熔断**（隔离红线）……否则「探测不通」会被放大成「上游整体不可用」"

这个隔离本身是**合理的设计决策**，但它带来一个架构事实：**"上游健不健康"这个问题在本仓库有两个 owner**（`upstream.targetHealth` 管请求级熔断、`probe.Classify` 管展示级连通率），而两者对"什么是健康"的定义不同（熔断看连续失败次数 + 窗口成功率，探测看同一组信号但落到**另一张表**）。`DESIGN.md:1336-1337` 说"健康判定是「连续失败 + 窗口成功率」两个独立信号叠加（`probe.Classify`）"，暗示只有一处——实际是两处。这个"两套健康语义"是 `docs/network-proxy-audit-2026-10-20.md` 之外**未在任何文档中说明**的架构事实。

**问题 3：`internal/snapshot` 是全局单例（`atomic.Pointer`），但没有 owner 概念。**
`internal/snapshot/snapshot.go:238-261`：`current atomic.Pointer[Snapshot]`，`Init` / `Get` / `Swap` 三个包级函数。
全仓 17 处 `snapshot.Get()`（`internal/auth/auth.go:57,76,108`、`internal/admin/group_handler.go:306,341,462`、`internal/admin/key_handler.go:238,748`、`internal/admin/probe_handler.go:117`、`internal/admin/provider_handler.go:172`、`internal/admin/route_catalog_handler.go:74`、`cmd/gateway/main.go:1890,1921,3545,3727`、`cmd/gateway/billing.go:449`）。
即：**数据面鉴权、管理面多个 handler、计费预检**全部读同一个包级全局量。
`internal/auth`（数据面鉴权）因此**反向依赖 snapshot**，而 snapshot 又依赖 store——**数据面鉴权的正确性依赖一次"管理写操作后的重建"**。这正是 `runtimeReloader.dirty` 机制存在的原因（`main.go:909-923` 的注释详细说明"禁用下游 Key 后 auth 读旧快照照常放行"这个分叉）。设计上是自洽的，但**"谁拥有运行时真相"这个问题在代码里没有单一答案**：`upstream.Pool` 拥有熔断/冷却，`snapshot` 拥有路由/密钥/用户，`store` 是 DB 真相。三者的一致性靠 `runtimeReloader.Reload` 的"先构建、后原子替换"纪律维持（`main.go:889-897`）。

### P2-4　鉴权口径有 **3 套并存**，且各自的作用域不重叠、边界靠注释维护

| 口径 | 位置 | 判定内容 |
|---|---|---|
| ① `AdminGateGuard` 前缀/路由白名单 | `internal/server/user_auth.go:161-207`（`userAccessibleRoutes`，21 条路由模式） | "普通用户能不能**进**这个端点" |
| ② handler 内的 `requireAdmin` | `internal/admin/user_admin_handler.go:158`（17 处调用） | "是不是管理员" |
| ③ handler 内的作用域收窄 | `internal/admin/key_handler.go:104-110`、`usage_handler.go:181-195`、`usage_by_user_handler.go:82`、`probe_handler.go:114`、`group_handler.go:336` | "能看到**谁**的数据" |

**问题**：

- ① 与 ② 是**两套并行的 admin 判定**。`AdminGateGuard` 已经保证"非白名单即要求管理员"（`user_auth.go:245-252`，fail-closed），但 17 处 handler **又各自调 `requireAdmin`**。`internal/admin/usage_handler.go:946-947` 的注释坦承了这个裂缝：
  > "**必须 admin-only，且不能只靠路径白名单**：本路径落在普通用户可访问的 `/admin/api/usage` 前缀下，白名单只保证「能进来」，不保证「有权限」。"
  也就是说：**白名单是按前缀粗放的，`/admin/api/usage` 整段对普通用户开放**，于是每一个挂在该前缀下的端点都必须**手工**记得调 `requireAdmin`。**新增端点时忘记调 = 越权**。这是典型的"用注释维持契约"——而 P1-1/P1-2 已经证明这个仓库的注释会漂移。
- ② 与 ③ 的判定输入不同：`requireAdmin` 读 `server.UserFromContext` 的 `u.IsAdmin()`（`user_admin_handler.go:164`），作用域收窄也读同一个。**但 `server.IsAdmin(ctx)`（`internal/server/user_auth.go:60`）是另一个实现**，而 `AdminGateGuard` 额外要求 `u.ID != ""`（`user_auth.go:235`）。**同一个"是不是管理员"有三种写法**（`u.IsAdmin()` / `server.IsAdmin(ctx)` / `u.IsAdmin() && u.ID != ""`），只有 `AdminGateGuard` 是最严的。
- ③ 的作用域判定散在 5 个文件里，各自手写 `me.ID != ""` / `me.IsAdmin()` 分支（见上表第 ③ 行），没有统一的作用域解析器。

**这是 P2 而非 P1 的理由**：当前所有端点的实际行为是正确的（有 `internal/server/authz_matrix_test.go` 1345 行在钉），且 `AdminGateGuard` 的 fail-closed 设计让"漏配"表现为 403 而非放行。风险是**未来**的端点，不是现有的越权。

### P2-5　`internal/store` 与 `internal/snapshot` 的职责边界重叠：运行时配置有**四份**定义

- `internal/store/settings_dao.go:192-194` 定义 `UpstreamTimeoutMs` / `StreamIdleTimeoutMs` / `StreamFirstTokenTimeoutMs`（`app_settings.runtime_defaults` 的 JSON 形状）。
- `internal/admin/settings_handler.go:33-35` **又定义了一遍**同样的三个字段 + 校验规则。
- `internal/snapshot/snapshot.go:156` `RuntimeDefaults` 是第三份（快照里的运行时默认）。
- `internal/config/config.go:135-145` 是第四份（config.json 的 `defaults`）。

四处各自定义"超时配置"的字段名、类型、校验区间。`cmd/gateway/main.go:2441-2503` 的四个 `xxxTimeout(snap, cfg)` 函数是它们的**读侧适配器**（快照优先、config 兜底）。
`internal/config/defaults_consistency_test.go:124-145` 的 `TestRuntimeDefaults_三处一致` 只校验了 **config.example.json / docker/config.default.json / config.setDefaults** 三处的三个超时值——**没有覆盖 `app_settings.runtime_defaults` 与 `web/src/views/Settings.vue`**。
而 `web/src/views/Settings.vue:212-218` 硬编码了第五份：
```
const RUNTIME_DEFAULTS = {
  upstream_timeout_ms: 300_000,
  stream_first_token_timeout_ms: 60_000,
  stream_idle_timeout_ms: 120_000,
  failover_max_targets: 3,
  failover_failure_threshold: 3,
}
```
`DESIGN.md:1346-1349` 明写"三处必须保持同一个口径：`internal/config` 的 `setDefaults`、`config.example.json`、web 设置页的 `RUNTIME_DEFAULTS`"——**并把"三处"当作已受控的事实**。但实际是**五处**（漏了 `app_settings.runtime_defaults` 的后端兜底 `internal/store/settings_dao.go` 与 `internal/admin/settings_handler.go`）。`internal/store/settings_dao.go` 的兜底常量与前两处是否一致，本次未逐字段核对（**待确认**：需读 `settings_dao.go` 的 `RuntimeDefaults` 默认值块）。

### P2-6　`internal/admin` 的字段级 PATCH 合并纪律"靠注释维持"，无机制约束

- `DESIGN.md:699-704` 给出了硬约束：
  > "结构体的标量字段必须是指针，合并处写 `if req.X != nil { ... }`……`internal/admin/helpers.go` 的 `derefStr` / `derefInt` **只准用于 Create 这类「缺省即零值」的场合**。"
- 但 `internal/admin/provider_handler.go:65-66` 的注释写着：
  > "BalanceSpecJSON 与 NetworkID 同为指针：空串是「不配余额查询」的显式值。"
- 且 `internal/admin/network_handler.go:63` `ProxyURL *string`，`internal/admin/provider_handler.go:58` `TimeoutMs *int`。
- 问题不在单点，而在**没有机制保证**这条纪律：`derefStr` / `derefInt` 是 `internal/admin/helpers.go` 里的导出辅助函数，任何 Create 场景都能调；**没有任何 lint / 测试钉住"deref* 只出现在 Create 路径"**。`internal/admin/key_handler.go` 的 `guardNoLoosening` 是另一套独立的"只准收紧"逻辑，与 PATCH 合并逻辑分散在不同文件。
- **风险**：这是一类"纪律靠注释"的架构债。同类纪律还有 `internal/store/store.go:668-692` 的 `dropDeadColumns`（"没有读写路径的列就 DROP"）——`providers.proxy_url` 刚在 2026-10-20 被摘除（`internal/store/store.go:705`），摘除后 `internal/config/config.go:165` 的 `BootstrapCredential.ProxyURL` 仍存在并被 `cmd/gateway/main.go:1115-1126` 用来生成 networks 行——**同一个字段名在两处含义已经不同**（config 层是"要建网络的代理地址"，store 层已被 DROP）。

### P2-7　`config.example.json` 的 `bootstrap.providers[].proxy_url` 与 `providers.proxy_url`（已 DROP）同名不同义

- `internal/config/config.go:159-165`：
  ```
  // ProxyURL 是该上游的出站代理（http/https/socks5）。空 = 直连。
  ProxyURL string `json:"proxy_url"`
  ```
- `internal/store/store.go:705`：`{"providers", "proxy_url"}` 已被 `dropDeadColumns` 摘除。
- `config.example.json:38` 仍写着 `"proxy_url": ""`（在 bootstrap 段里）。
- `DESIGN.md:1274-1275` 的配置示例里也写着：
  > `"proxy_url": "",   // 空 = 直连。非空则启动时按 URL 自动建出 networks 行`
- **实际行为**：`cmd/gateway/main.go:1115-1126` 会把非空的 `proxy_url` 转成一条 `networks` 行，并按 URL 归并（`networkByURL` map）。这段逻辑是 `0f3bdab`（2026-10-20）补的——**在 `3fc1ae4` 改 schema 时漏了**，`0f3bdab` 才补上。
- **风险面**：同一个 JSON key 在 config 的 bootstrap 段（有效，转 networks）与 DB 的 providers 表（已 DROP，无效）里同名。agent 若照 `DESIGN.md:1274` 给一个**已有** provider 改 `proxy_url`（以为能改），会静默无效。

### P2-8　故障转移的**策略层**与**机制层**分离不彻底，策略无单测

- **机制层**（`internal/upstream`）：`ClaimTargetProbe` / `ReleaseTargetProbe` / `RecordTargetSuccess` / `RecordTargetFailure` / `TargetAvailable` / `MarkCredentialCooldown` —— 都是原语，逻辑正确。
- **策略层**（`cmd/gateway/main.go:2006-2320`）：什么时候转移、转移预算、熔断筛选、结构化输出过滤、白名单过滤顺序、失败归因、route_trace 格式 —— 全部在 main 里，**没有一个可单测的函数**。
- **具体风险点**（`main.go:2017-2072`）：三道过滤的**顺序**有语义依赖且只有注释记录：
  1. 熔断筛选（`main.go:2024-2034`）
  2. 结构化输出过滤（`main.go:2050-2069`）
  3. 预算截断（`main.go:2070-2072`）
  注释说"放在熔断筛选之后、预算截断之前：先剔除服务不了的目标，让 failover_max_targets 的预算花在真正可服务的候选上"。**顺序错了会浪费预算，但没有任何测试钉住它**。
- `internal/outwire.FailoverEligible`（`internal/outwire/errors.go:287-326`）与 `internal/outwire.CredentialCooldown`（`:333-352`）是**两个独立的 switch**，对同一批状态码做判定。`errors.go:286` 的注释指出 404/410 必须在 `CredentialCooldown` 里返回 0——但这靠的是**两处 switch 的隐式对应关系**，没有共享的分类表。改一处忘了另一处 = 把目标级故障升级成 provider 级故障（`DESIGN.md:1021` 描述了这个后果）。

### P2-9　"按天"口径散在 4 处，正确但脆弱（无编译期强制）

- `internal/store/usage_archive.go:465`：`const dayExpr = "strftime('%Y-%m-%d', ts / 1000, 'unixepoch', 'localtime')"`
- 使用点：归档写入 `usage_archive.go:638`、按天读 `usage_source.go:40`、建表注释 `internal/store/store.go:438`、`internal/admin/usage_handler.go` 的 `GroupByDay`。
- `internal/config/timezone.go:60-94` `ApplyTimezone` 明确要求"必须同时设 `TZ` 环境变量与 `time.Local`"（因为 modernc SQLite 在 Linux 走 C 的 `tzset`），并注释"顺序不能反"（`Setenv` 必须早于 `store.Open`）。
- `cmd/gateway/main.go:145-151` 确实在 `store.Open`（`:195`）之前调用了 `ApplyTimezone`——**这一条是对的**。
- `internal/store/day_bucket_tz_test.go` 用子进程钉住了这个行为（`go test -run 'TZ|DayExpr|Day' ./internal/store/` 实测通过）。
- **但**：这套正确性依赖三件事同时成立——① `ApplyTimezone` 在 `store.Open` 前调用；② `os.Setenv` 与 `time.Local` 都设；③ SQLite 驱动尊重 TZ。**没有一条是编译期或 API 强制的**。`day_bucket_tz_test.go` 在子进程里跑，**不覆盖调用顺序**。`internal/store.Open` 也不检查 `TZ` 是否已设。
- **归类**：这是"正确但脆弱"的状态，与 P1-1/P1-2 那种"文档说错"不同级别，列 P2。

---

## 3. P3 清单（文档漂移 / nit）

### P3-1　DESIGN 头部版本号停留在 1.6.2，实际已 1.6.5
- `DESIGN.md:8`：`| 版本 | 与 `web/package.json` 同源（当前 1.6.2），镜像与二进制共用 |`
- 实际：`web/package.json:4` = `"version": "1.6.5"`，且 git 有 `7e63fc1 chore(release): 1.6.4 → 1.6.5`。
- 影响：agent 按 DESIGN 判断"当前版本有哪些特性"会落后 3 个 minor。

### P3-2　DESIGN §3 架构图里的 `quota` 包**不存在**
- `DESIGN.md:97`：`│  quota         配额检查 · 用量扣减 · 限速          │`
- `DESIGN.md:121`：`→ quota      RPM 限速（快照）→ 解码 → TPM 预占 + 终身配额原子预占`
- 实际：`internal/` 下**没有 `quota` 包**。配额/限速在 `cmd/gateway/main.go` + `internal/ratelimit` + `internal/store`。
- 同样的架构图里 `admin` 被画成独立层，实际 `admin` 还依赖 `server`（见 P2-3）。

### P3-3　DESIGN §13.2 的路由清单与 `web/src/router.ts` 大面积不符

`DESIGN.md:1417-1428` 声称：
- "实际路由（`web/src/router.ts`，2026-10-17 核实）共 **8 个可渲染页面**（`/login`、`/`、`/usage`、`/models`、`/wallet`、`/users`、`/providers`、`/settings`）"
- "`/keys` → `/users`；`/history` → `/usage`；`/profile` → `/wallet`；`/routes` 与 `/model-groups` → `/providers`；`/health` → `/models`"
- "`/keys` 在 router.ts 里出现了两次……两条目标都是 `name: 'users'`"

实际 `web/src/router.ts:8-34`：

| 项 | DESIGN 说 | 实际 |
|---|---|---|
| `/keys` | 重定向到 `/users`，注册两次 | `router.ts:15` 独立页面（`AccessControl.vue`，`name:'keys'`），**只注册一次**（`rg -c "path: '/keys'"` = 1） |
| `/history` | 重定向到 `/usage` | `router.ts:10` 独立页面（`UsageStats.vue`，`name:'history'`），非重定向 |
| `/routes` | 重定向到 `/providers` | `router.ts:24` 独立页面（`Routes.vue`，`adminOnly`） |
| `/model-groups` | 重定向到 `/providers` | `router.ts:25` 重定向到 `{name:'usage', query:{tab:'groups'}}`（**不是** `/providers`） |
| `/networks` | 未列 | `router.ts:26` 存在（`Networks.vue`，adminOnly）——DESIGN §13.2 表格**有** `/networks` 一行（`:1443`），但"8 个可渲染页面"的计数没算它 |
| `/account` | 未列 | `router.ts:29` 存在（`AccountSettings.vue`） |
| `/providers/:id` | 未列 | `router.ts:27` 存在（`ProviderDetail.vue`） |
| 重复注册 | 说 `/keys` 出现两次 | `rg -c` = 1，**已不存在** |

**尤其危险的一条**：`DESIGN.md:1429-1433` 那段专门解释"`/keys` 在 router.ts 里出现了两次……后注册的那条胜出……两条目标都是 `name: 'users'`"。**这个前提现在完全不存在**。一个 agent 读到这段，会以为 `/keys` 的路由行为有微妙之处而去"修复"它，实际上 `/keys` 早就是独立页面了。而 `web/src/App.vue:90-94` 的注释还在说：
> "管理员也要一条独立的「API 密钥」入口。此前只有「用户与权限」一条，而两个路由共用一个组件：/users?tab=keys 停在密钥页签时，侧栏高亮的却是「用户与权限」"

——**App.vue 的注释是新的（承认 /keys 独立），DESIGN 是旧的（说 /keys 重定向），两者互相矛盾**。

### P3-4　DESIGN §15 目录结构描述与实现有偏差，且整棵树已漂移
- `DESIGN.md:1570-1588` 的目录树列出 `internal/` 的 16 个子包——实际数量一致（16 个），但：
  - `DESIGN.md:1573` 写 `snapshot/ 运行时快照：路由索引、Provider 池，atomic.Pointer 热替换`——**"Provider 池"不在 snapshot 里**。Provider 池在 `internal/upstream`（`upstream.Pool`）；snapshot 只有 `Providers map[string]*ProviderSnapshot`（`internal/snapshot/snapshot.go:174`）这个**只读投影**。这个措辞会让人以为 snapshot 拥有池。
  - `DESIGN.md:1575` 写 `upstream/ 凭证池、健康与冷却、Client 生命周期、故障转移、出站代理`——"故障转移"在这里，但**策略**在 `cmd/gateway`（见 P2-8）。`upstream` 只有熔断/冷却的机制原语。
  - `DESIGN.md:1588` 写 `└── web/ Vue 3 + Vite + TS 工程`——**漏了 `docs/` 目录**（`docs/` 现有 5 个未跟踪的审计/验收文档）与 `AGENTS.md`（未跟踪）。§15 的树也没有 `docs/`。
  - `DESIGN.md:1557-1561` 说 `scripts/` 下有 `probe_tz_child.go` / `check-repo-hygiene.ps1` / `authz_probe/`——实际一致（`scripts/authz_probe/dbdump/main.go` + `probe.ps1`）。
  - `DESIGN.md:1534` 提到的 `examples/curl.md` 确认不存在（文档已自陈"从未创建"），非缺陷。

### P3-5　DESIGN §6.3 的端点表漏了 5 条实际存在的端点
`cmd/gateway/main.go` 里注册但 DESIGN §6.3 表格中没有（或只以合并写法出现）的：
- `GET /admin/api/route-catalog`（`main.go:435`）—— `web/src/views/Routes.vue:86` 与 `web/src/api.ts:539` 都在用，**完全未出现在文档中**。
- `POST /admin/api/config-export/import`（`main.go:476`）——表里只有 `POST /admin/api/config-export/export|import` 一行（`:589`），**用了 `|` 合并写法**，机器读不出这是两条。
- `GET /admin/api/usage/by-day|by-key|by-model`（`main.go:457-459`）——表里 `:584` 用 `GET /admin/api/usage/by-provider|by-day|by-model|by-key` 合并写法，同样不可机读。
- 合计：**5 条端点**只以合并/省略形式出现或完全缺失。

### P3-6　DESIGN §10 对熔断状态跨重建的行为与代码相反（与 P1-1 同源但影响不同对象）
- `DESIGN.md:1032`："目标熔断表与凭据冷却在每次重建池时一并清零——两层语义一致"
- `internal/upstream/upstream.go:399-431` `Install` 的注释与实现：**熔断状态跨重建保留**（"运维改一个模型的 context_width，不该让正在熔断中的坏上游立刻复活并被打满"），只有 `halfOpen` 标志被清（`:424`）。
- 即代码是"**熔断保留**"（且凭据冷却也因落库而保留），文档是"**两者都清零**"。文档在这一点上**同时**错了两个对象。

### P3-7　bootstrap admin 的实现位置在文档里指向一个不存在的文件
- `DESIGN.md:461`："系统启动时若 users 表没有任何**已设密码**的 admin，自动建出一个空密码 admin 账号（`cmd/gateway/bootstrap_admin.go`）"
- **实际**：`cmd/gateway/` 下只有 `main.go`、`billing.go`、`replay_gate.go` + 测试文件。`bootstrap_admin.go` **不存在**。实现在 `cmd/gateway/main.go:282` `ensureBootstrapAdmin(context.Background(), db, logger)`。
- 风险：agent 按文档去改 `cmd/gateway/bootstrap_admin.go` 会发现文件不存在，转而去猜该改哪里。

### P3-8　`DESIGN.md` §17 的 R11/R12/R14/R15/R16 全部标"已解决"，但解决点分散在 4 个不同的包，彼此无交叉引用
- R11（sealed reasoning）→ `internal/inwire/openai_responses.go:754-770` + `internal/outwire/openai_responses.go:291-371,549-564`
- R12（媒体块）→ `internal/inwire/openai_responses.go`
- R14（回放闸门）→ `cmd/gateway/replay_gate.go:52`（**在 cmd 层，不在 inwire/outwire**）
- R15（namespace 展平）→ `internal/inwire/openai_responses.go:379`
- R16（reasoning item 生命周期）→ `internal/outwire/openai_responses.go`
- **观察**：R14 的 `applyReplayGate` 住在 `cmd/gateway/replay_gate.go`，而它是**协议转换**逻辑（按目标协议剥块），却不在 `internal/inwire`/`internal/outwire` 里。`cmd/gateway/main.go:2616`（流式）与 `:2913`（非流式）各调一次——这意味着**每加一个协议入口就要记得调两次**。协议转换规则散在 cmd 层，是 P2-1 的具体表现。
- 另：R11~R16 的"已解决"日期跨 2026-10-16 ~ 10-18，而 `DESIGN.md:9` 的"更新"字段停在 2026-10-17——文档自己没跟上自己的正文。

---

## 4. 孤儿 / 死代码清单

### 4.1 无调用方的导出函数（4 个，均在 `internal/store`）
用"全仓符号出现次数 ≤ 1"扫出（排除 `_test.go` 后按符号名统计）：

| 符号 | 位置 | 状态 |
|---|---|---|
| `(*Store).UpdateRoute` | `internal/store/route_dao.go:109` | **无任何调用方**（全仓仅定义处 1 次） |
| `(*Store).GetRouteTarget` | `internal/store/route_target_dao.go:144` | **无任何调用方** |
| `(*Store).UpdateRouteTarget` | `internal/store/route_target_dao.go:179` | **无任何调用方** |
| `(*Store).DeleteRouteTarget` | `internal/store/route_target_dao.go:190` | **无任何调用方** |

**根因**：整条链的写入口是 `PUT /admin/api/routes/{id}/targets` 的**整体替换**（`cmd/gateway/main.go:440` → `internal/admin/route_target_handler.go` 的 `Replace`），所以单条的增/改/删 DAO 被整体替换语义取代了，但**没有删掉**。
**与项目自身的纪律冲突**：`internal/store/store.go:668-692` 的 `dropDeadColumns` 存在的理由正是"schema 与界面在说谎"——**这里有完全同构的问题在 Go API 层**：四个导出方法对外宣告"可以单条操作 route_target"，而实际只支持整体替换。
**注**：这 4 个是**导出**方法（在包外可见），不能被 Go 编译器判为未使用。`internal/` 路径规则使外部 module 无法 import，因此它们在本仓语境下确为死代码。

### 4.2 注册了但没有 UI 入口的端点（1 个，**有意为之但前端侧有孤儿**）
- `GET /admin/api/audit`（`cmd/gateway/main.go:455`）——`DESIGN.md:1445` 明确写"**审计日志页签已移除**（用户要求）——后端 `GET /admin/api/audit` 端点与 `api.ts` 的 `api.audit()` 都保留"。
- `web/src/api.ts:682` 确实有 `audit: (limit = 50) => get<{ entries: AuditEntry[]; server_ts: number }>('/audit?limit=${limit}')`，但 `web/src` 里**没有任何视图调用它**（`rg 'api.audit' web/src` 只命中 api.ts 自身）。
- **归类**：**不是死代码**（文档明确说保留，且 `DESIGN.md:1705` 附录 C 把它列为"刻意不修"的取舍），但 `api.ts` 里的 `audit()` 是一个**前端侧的孤儿**——一个永远不会被调用的 API 包装。这与"页签已移除"的用户决策矛盾。列 P3 级 nit。

### 4.3 从未被读取的配置字段
扫 `internal/config/config.go` 的全部 json tag 在全仓的出现次数（≤2 的候选）：
- `api_key_env`（`config.go:172`）：全仓 2 次（定义 + 校验消息）。**实际读取点在 `cmd/gateway/main.go:1144`**：
  ```
  apiKey := bc.APIKey
  if bc.APIKeyEnv != "" {
      apiKey = os.Getenv(bc.APIKeyEnv)
  }
  ```
  读的是 `bc.APIKeyEnv`（Go 字段名），不是 json tag 字符串。所以**不是死字段**。**但有一个真实缺陷**：`DESIGN.md:1289` 说"两种都配时 `api_key` 优先"——而代码里 `apiKey := bc.APIKey` 后**才**被 `APIKeyEnv` **覆盖**（`main.go:1143-1146`），即**环境变量优先**，与文档相反。P3 级文档漂移。
- `interval_sec`（`config.go:96`）：全仓 2 次（定义 + 校验），但**实际读取在 `internal/probe/runner.go:84`**（读 `r.cfg.Probes.IntervalSec`）。不是死字段。
- `master_key_env`：读取在 `cmd/gateway/main.go:166` `crypto.LoadMasterKey(cfg.MasterKeyEnv, homeDir)`。不是死字段。
- **结论：没有真正的"从未被读取"的配置字段。**（`internal/config/defaults_consistency_test.go:100-118` 也钉住了两份示例配置能被 `Parse` 接受。）

### 4.4 被文档提到但已不存在的文件

| 文档提及 | 实际 | 判定 |
|---|---|---|
| `DESIGN.md:461` `cmd/gateway/bootstrap_admin.go` | **不存在** | **P3 文档漂移**（见 P3-7） |
| `DESIGN.md:1534` `examples/curl.md` | 确认不存在 | 文档已自陈，非缺陷 |
| `DESIGN.md:1558-1560` `scripts/probe_tz_child.go` / `check-repo-hygiene.ps1` / `authz_probe/` | 全部存在 | 一致 |
| `DESIGN.md:1555` `docker/config.default.json` | 存在 | 一致 |
| `DESIGN.md:1546-1550` 根级 `README.md`/`DESIGN.md`/`MULTIUSER.md`/`DOCKER.md`/`CONTRIBUTING.md`/`config.example.json`/`Dockerfile`/`build.ps1`/`gateway.ps1` | 全部存在 | 一致 |
| `DESIGN.md:1544` 的目录树 | 未列 `docs/`、`AGENTS.md`（均存在，docs/ 未跟踪） | 见 P3-4 |
| `web/src/views/Upstream.vue` | 存在但**无任何路由引用**（`rg "views/Upstream"` 只命中 `web/src/views/ModelGroups.vue:197` 的一句注释） | **孤儿前端文件**。它内部 import 了 `Providers.vue` / `Routes.vue` / `ModelGroups.vue` 并用 `<component :is>` 组合，但 `router.ts:18` 的 `/providers` 直接指向 `Providers.vue`。**这 240 行的"三页签合一"组件是死代码**，而 `DESIGN.md:1444` 还把它写成"组件是 `Upstream.vue`，三个页签顺序即数据面配置链的顺序" —— **文档描述的是一个实际未被路由使用的组件** |

---

## 5. 配置唯一事实来源（D4）核验

### 5.1 结论：**D4 基本成立，但有 3 处裂缝**

**成立的部分**（已验证）：
- `DESIGN.md:1160-1168` 的三行表与代码一致：DB 存 Provider/模型/Route/Key/用户；配置文件管进程级参数；bootstrap 只在 DB 空时生效。
- `cmd/gateway/main.go:202` `bootstrapDB(db, cfg, logger, masterKey)` 在 `store.Open` 之后、`runtimeReloader.Reload` 之前调用。
- `internal/config/defaults_consistency_test.go:124-145` 钉住了三个超时值在 `config.example.json` / `docker/config.default.json` / `setDefaults` 三处一致。实测 `go test -run 'Defaults|Timezone|Config' ./internal/config/...` **通过**。
- `internal/config/defaults_consistency_test.go:100-118` 钉住两份示例配置能被 `Parse` 接受且 `audit_log_keep_rows > 0`。
- `internal/config/defaults_consistency_test.go:16-41` 钉住 `probes` 显式关闭不被改回开启（对应 `1f7cf10` 那次修复）。
- 运行时默认值的**读取**路径确实优先快照后落 config：`cmd/gateway/main.go:2481-2503` 四个函数。

**裂缝 1**：`config.example.json` 与 `docker/config.default.json` 的 `bootstrap` 段差异巨大且**无测试覆盖**：
```
config.example.json       → providers: [DeepSeek 示例 + 1 条 route]
docker/config.default.json → providers: [], routes: []
```
`internal/config/defaults_consistency_test.go` 只检查 `Parse` 能过 + `AuditLogKeepRows > 0`，**不比较两个文件的 bootstrap 段**。这不是缺陷（docker 故意不播种上游），但 `DESIGN.md:1238-1285` 的"§12.2 配置文件示例"只给了**一份** JSON，且**它与 `config.example.json` 都不完全一致**（DESIGN 的示例里 provider 名是"DeepSeek 官方"、`"label": "主号"`，而 `config.example.json` 是 `"DeepSeek"` / `"label": "main"`）。**三份"示例"互不相同**。

**裂缝 2**：§12.2 声称的 bootstrap `proxy_url` 语义在 `DESIGN.md:1274-1275` 描述了（"非空则启动时按 URL 自动建出 networks 行"），**代码在 `0f3bdab` 才补上**（`cmd/gateway/main.go:1115-1126`）。这说明**规范先写、代码后补**，中间有约 1 小时的窗口期，期间按 DESIGN 走的人会以为已实现。

**裂缝 3**：D4 说"数据库唯一"，但**三个超时/失败阈值类字段有五份定义**（见 P2-5），其中 `web/src/views/Settings.vue:212-218` 的 `RUNTIME_DEFAULTS` 是**前端硬编码**——它不是配置，但它是"生效值"的来源之一（`DESIGN.md:1346-1349` 自己说"界面回显的是生效值"）。`defaults_consistency_test.go` 不覆盖它。

### 5.2 时区与 day bucket 口径：**正确**，但保护机制是脆的

**代码事实**：
- `internal/config/timezone.go:60-94` `ApplyTimezone` 同时 `os.Setenv("TZ", name)`（`:89`）与 `time.Local = loc`（`:92`），注释详细说明为什么只设一个会跨平台失效（`:24-36` 的表格：Windows 两者都跟、Linux 只跟 TZ）。
- `cmd/gateway/main.go:145` 调用 `ApplyTimezone` → `:195` `store.Open`。**顺序正确**（`timezone.go:43-44` 要求"必须早于任何 SQLite 查询"，`:82-83` 要求"必须在打开数据库之前调用"）。
- `internal/store/usage_archive.go:465` `const dayExpr = "strftime('%Y-%m-%d', ts / 1000, 'unixepoch', 'localtime')"` —— **一个常量，两个方向共用**（归档写入 `usage_archive.go:638`，按天读 `usage_source.go:40`）。
- `internal/store/day_bucket_tz_test.go` 用子进程（`scripts/probe_tz_child.go`）钉住"TZ 变了 dayExpr 跟着变"。实测 `go test -run 'TZ|DayExpr|Day' ./internal/store/` **通过**。

**脆弱点**（P2-9 已列）：这套正确性依赖三件事同时成立——① `ApplyTimezone` 在 `store.Open` 前调用；② `os.Setenv` 与 `time.Local` 都设；③ SQLite 驱动尊重 TZ。**没有一条是编译期或 API 强制的**。`day_bucket_tz_test.go` 在子进程里跑，**不覆盖调用顺序**。`internal/store.Open` 也不检查 `TZ` 是否已设。
`internal/config/timezone.go:60-66`：空串 = 不覆盖，**返回实际生效时区**并由 `main.go:150-151` 打印。这是好的——运维能一眼看到 "timezone=UTC"。

---

## 6. 近期改动根因归类（为什么"修一点不管用"）

### 6.1 变更全景

`git log --oneline 85e7770..HEAD` 只有 3 个提交（都在 2026-10-20）：
```
ea19ce1 chore(webui): 重建内嵌前端以包含请求日志速度列
2414a4e fix(web): 请求日志恢复速度列并压成单行
85e7770 chore(webui): 重建内嵌前端以包含网络清单刷新修复   ← work/before-fix 快照点
38a9b4e fix(web): 出站网络下拉不再停留在首份缓存
0f3bdab feat(gateway): 挂上 networks 路由与 bootstrap 的代理归并
```
+ `git diff --stat 85e7770 -- cmd internal web` 的**实际内容变化只有 20 个文件、+49/−31 行**，且**全部集中在前端**（`internal/webui/dist` 的 19 个 hash 改名 + `web/src/views/UsageDetail.vue` 44 行）。

即：**work/before-fix 到当前 HEAD，后端 Go 代码零改动**。这一轮全是前端。
（`work/before-fix` 是 85e7770 的完整快照，`git -C work/before-fix log` 确认其 HEAD = `85e7770`。）

### 6.2 根因归类：三个可复现的模式

**模式 A：「schema 改了但接线漏了」（P2-7 的直接来源）**

`3fc1ae4`（2026-10-20）"出站网络改为可复用清单，代理地址不再逐个上游手填"：
- 改了 `internal/store/network_dao.go`（+256 行）、`internal/store/provider_dao.go`（+63）、`internal/store/store.go`（+59）、`internal/admin/network_handler.go`（+158）、`internal/admin/provider_handler.go`（+86）、`internal/admin/config_transfer_handler.go`（+133）、`internal/admin/export_format.go`（+29）、`internal/config/config.go`（+8）、`internal/upstream/upstream.go`（+18）、前端 6 个文件。
- **漏了 `cmd/gateway/main.go`** —— 于是 `0f3bdab` 的提交信息原话：
  > "上一个提交（3fc1ae4）**漏了 main.go**：网络 handler 的四个端点没有注册，bootstrap 路径也没有把配置里的 proxy_url 转成网络引用。两条路径都补齐。"

**这就是"修一点不管用"的最短复现**：`3fc1ae4` 改完 schema + DAO + handler + 前端，`go build` 全绿、测试全绿，而 `/admin/api/networks` 端点**根本不存在**（落到 `/admin/api/` 前缀上被鉴权拦掉或 404）。因为**路由表住在 `cmd/gateway/main.go` 里，而 main.go 是 3532 行、90% 的内容与本次改动无关**——没有任何"新端点必须注册"的检查、编译期约束或测试。
`0f3bdab` 补上后，DESIGN §6.3（`DESIGN.md:492-497`）才有了 networks 端点表——**文档是追着代码补的**。

**模式 B：「前端源码与 embed 产物是两条流水线」**

git 历史里"改前端"与"重建产物"**永远是两个提交**：
```
2414a4e fix(web): 请求日志恢复速度列并压成单行         ← 只改 web/src
ea19ce1 chore(webui): 重建内嵌前端以包含请求日志速度列  ← 紧接着重建 dist
38a9b4e fix(web): 出站网络下拉不再停留在首份缓存
85e7770 chore(webui): 重建内嵌前端以包含网络清单刷新修复
```
+ 更早的 `6f7d204` / `c180405` / `bab40ec` / `a3d7164` 同样是这个模式（`git log | Select-String 'chore\(webui\)'` = **6 次**）。
**这意味着任何一次"忘了重建产物"的中间态都会进 git 历史**，而 `go:embed` 打包的是入库的 dist（`cmd/gateway/main.go:581`），所以**发出去的二进制是旧 UI**。CI 的 `webui-sync` job（`.github/workflows/ci.yml:208-234`）能拦，但：
- 它只在 push/PR 时跑，**本地打 tag 不受保护**（`DESIGN.md:1376-1378` 承认了这点）。
- **当前工作树正处在这个中间态**（P1-3）：`web/src/views/UsageDetail.vue` 有未提交改动，dist 有一半已删一半未跟踪。
- `38a9b4e` 修的"出站网络下拉不再停留在首份缓存"是 `useNetworks.ts` 的**模块级缓存**问题（`web/src/composables/useNetworks.ts`）——这是**前端状态所有权不清**的产物：网络清单既在 `Networks.vue` 又在 `useNetworks.ts` 的模块级单例里，两处各有一份，改一处另一处不更新。该提交（`38a9b4e`）的描述自己说"出站网络管理页也改用同一份共享状态，增删改后两边同步"——**用"改用同一份共享状态"来修"有两份状态"**，等于承认架构上缺一个 owner。

**模式 C：「同一类问题反复打补丁」—— 协议转换边界**

2026-10-16 到 2026-10-18 三天内，5 个提交全在打"Responses 协议转换"这一类问题：
```
721b340 feat(responses): 透传 reasoning item 的 sealed 载荷，多轮会话不再断链
ee92561 fix(inwire): Responses 的工具输出媒体块不再被静默丢弃
784b517 fix(inwire): Responses 入站展平 namespace 工具，Codex 才能接入
ebdf11b fix(outwire): Responses 流式的 reasoning item 不再一片一个
5f306a3 fix(core): 出站前剥掉目标协议无法回放的思考块
```
+ DESIGN §17 把它们记成 R11/R12/R14/R15/R16 五条独立风险，每条都标"**已解决**"。

**但它们是同一个根因的五次显形**：**统一模型的"回放能力"没有集中的表达能力**。每发现一种载荷（sealed / media / namespace / thinking）在跨协议时失效，就在对应的 inwire/outwire 文件里加一个特判：
- sealed 载荷 → `internal/inwire/openai_responses.go:754-770` + `internal/outwire/openai_responses.go:291-371,549-564`
- 媒体块 → `internal/inwire/openai_responses.go`
- namespace 工具 → `internal/inwire/openai_responses.go:379` `flattenNamespaceTools`
- reasoning item 生命周期 → `internal/outwire/openai_responses.go`
- 回放闸门 → `cmd/gateway/replay_gate.go:52` `applyReplayGate`（**在 cmd 层！**）

`internal/inwire/openai_responses.go` 现在 1255 行，是 `internal/inwire` 里最大的文件，且是**唯一**承载 Responses 协议的。
**"回放闸门"住在 `cmd/gateway/replay_gate.go` 是最刺眼的证据**：它是一个**协议转换规则**（按目标协议剥块），却因为 `ingressRequest` 这个 cmd 层私有类型而无法下沉到 `internal/inwire`——`applyReplayGate(req *rosetta.ChatRequest, protocol string)` 本身其实**只依赖 rosetta 类型**（`replay_gate.go:52`），完全可以放进 `internal/inwire` 或 `internal/outwire`。它之所以在 cmd 层，只是因为**调用点在 cmd 层**（`main.go:2616` 流式 + `main.go:2913` 非流式两处，各调一次）。

**"修一点不管用"的机制**（这是本节的核心答案）：
1. 统一模型是网关的核心抽象，但**它的"可回放性"没有契约**。每种新载荷（sealed / media / namespace / thinking）都是"下游能发、跨协议会死"的定时炸弹，而网关只能在**具体协议的编解码器里**逐个加特判。
2. 这些特判分散在 3 个包 + 1 个 cmd 文件里（inwire / outwire / cmd/replay_gate.go），**没有任何一处能回答"这份请求发到协议 X 上会不会成功"**。于是只能靠"真跑一次看看"发现 → 变成事后打补丁。
3. 每打一个补丁，`internal/inwire/openai_responses.go` 和 `internal/outwire/openai_responses.go` 就更大一点，**下一个特判更难放对位置**，而 `cmd/gateway/main.go` 里要改的调用点又多一处（流式/非流式两处，漏一处 = 只修了一半）。这正是 `R16`（reasoning item 被逐增量切碎）这种"修了流式忘了非流式"的温床。

### 6.3 结论：为什么修一点不管用

把三条根因合成一句话：**这个仓库的规范（DESIGN.md）在关键处已经不可信（第 1 条），规范不可信时每个 agent 都在朝不同方向打补丁（第 2 条：main.go 无边界，任何改动都塞进同一个文件，任何漏接都是静默的），而"改完必须重建产物"这条流水线纪律从未被工具强制（第 3 条）。** 三者叠加的效果就是：每次修复都局部正确、全局无进展，且下一轮修复的起点是错的。

**可操作的三个抓手**（不在本审计的实施范围内，仅记录）：
1. **把 DESIGN 的"可执行断言"降级为"待核实的规格"**——至少 §10 健康态、§11.4 计数、§13.2 路由、§15 目录这四处必须先修文档，再谈代码。
2. **给 `cmd/gateway/main.go` 设一个可执行的拆分目标**（例如：把 `handleIngress` + `attemptStream` + `attemptNonStream` + `rateCommit` + `usageRecorder` 拆成 `internal/dataplane`，把路由表拆成 `internal/routes`），并用"新端点必须注册"的测试钉住（这直接封住模式 A）。
3. **给"源码 → 产物"加本地强制**（例如 `git diff --exit-code internal/webui/dist` 作为 pre-commit，或让 `npm run build && npm run sync` 成为 `build.ps1` 的默认路径），封住模式 B。

---

## 7. 仓库卫生现状（只报告，不建议动手）

### 7.1 `git status --porcelain -uall` 解读

**分支状态**：`main` 领先 `origin/main` **2 个提交**（`ea19ce1`、`2414a4e`）。

**已跟踪文件的改动**（32 项）：
- `internal/webui/dist/assets/*` **17 个文件被删除**（` D`）：`AccessControl-hU-0mLc4.js`、`AccountSettings-BCcEBNRb.js`、`DetailDrawer-Btdvlg1S.js`、`Login-DWrmmEuI.js`、`Models-CNkmgRjA.js`、`Networks-JtdLJsgd.js`、`Overview-mQ7BsXy9.js`、`ProviderDetail-BS958GNb.js`、`Providers-Cci5lFhh.js`、`Routes-BKYi9U__.js`、`Settings-BtqiPfvY.js`、`UsageStats-BYUqPI-D.js`、`UsageStats-yQCFwsvU.css`、`Wallet-BT0OAGYN.js`、`index-CqXLBNJx.js`、`useQueryText-DeDHRljb.js`
- `internal/webui/dist/index.html` ` M`（已指向新 hash `index-m5WBl62f.js`）
- `web/src/views/UsageDetail.vue` ` M`（未提交的速度列 CSS 与列宽调整）

**未跟踪文件（21 项）**：
- `internal/webui/dist/assets/*` **17 个新 hash 资产**（`??`）：`AccessControl-BasiFUUG.js` … `index-m5WBl62f.js`、`useQueryText-nINPwsij.js`
- `AGENTS.md`
- `docs/` 下 5 个文档 + 目录本身：`frontend-information-architecture-review-2026-10-17.md`、`frontend-integration-acceptance-2026-10-18.md`、`frontend-production-acceptance-2026-10-18.md`、`frontend-shared-components-2026-10-18.md`、`network-proxy-audit-2026-10-20.md`

**解读**：
- **`docs/` 未跟踪**：`docs/` 目录**不被** `.gitignore` 忽略（`.gitignore:104` 明确写"不写裸 `docs/`：本仓库的文档根目录如果哪天要建，不能连带被排除"），所以这 5 个文档是**有意不入库**还是**忘了 add**，无法从 git 侧判断。这 5 份是 2026-10-17~20 的前端验收/审计记录，与 `work/`（已忽略）性质接近。
- **`AGENTS.md` 未跟踪**：它是本机的 agent 指令文件（XERJ 索引说明 + 项目索引名 `rosetta-gateway-docs` / `rosetta-gateway-web-src`）。它引用了这两个索引名——**这些是 XERJ 节点的本地状态，不在 git 里**，所以 AGENTS.md 入库与否不影响其有效性。
- **`internal/webui/dist` 新旧资产并存**：这是 P1-3 描述的中间态。**不是**"新旧两套都在被 embed"（`index.html` 只引用新的），而是"新的还没 `git add`、旧的还没 `git rm`"。
- **`web/src/views/UsageDetail.vue` 未提交改动**：内容是 CSS 专项化修正（`.logs-table .model-name.mono` 抬特异性）+ 列宽 360→300。注释里说"实测 1280 视口溢出 47px"——**这是一次基于实测的布局修正**。它与 dist 的重建是配套的（改了源码必须重建）。

### 7.2 `scripts/check-repo-hygiene.ps1` 的口径与实际索引**不重合**

**实测结果**：脚本**通过**（`=== 结果：PASS ===`，`已跟踪文件 : 336 个`，三项检查全过：索引干净、17 个 ignore 探针全对、空行通配陷阱未复现）。

**但脚本查不到当前的实际问题**：

| 脚本口径 | 实际索引/工作树 | 结论 |
|---|---|---|
| `git ls-files`（`:107`）只看**索引** | 工作树里 `internal/webui/dist/assets/*` 有 17 个 ` D`（索引有、磁盘无） | **漏**。脚本把"索引里有"当成"文件存在" |
| 5 条 pattern（`:52-88`）查 exe/db/密钥/运行目录 | 当前工作树的脏东西全是 `.js`/`.css`/`.html`/`.vue` | 不覆盖，**但不适用** |
| `runtime-dir` rule（`:84`）匹配 `^(data\|bin\|work\|tools)/` | `work/` 确实被 `.gitignore:105` 挡住（`work/audit-2026-10-20/A1-architecture.md` 不会进索引） | 一致 |
| `local-config` rule（`:77`）匹配 `^(.*/)?(admin_auth\|config)\.json$` | `config.example.json` **不匹配**（前缀是 `config` 不是 `config.example`） | 一致 |

**关键不一致**：
1. **脚本与 CI 的判定口径不同**。CI 的 **webui-sync** job（`.github/workflows/ci.yml:222`）用 `git diff --exit-code --stat -- internal/webui/dist` —— **看工作树与索引的 diff**。脚本用 `git ls-files` —— **只看索引**。所以：**当前工作树会触发 CI 红灯，但本地卫生脚本报 PASS**。脚本注释里说"等价于 CI 里 repo-hygiene job 的那次检查"（`:1`）——**就 repo-hygiene job 而言确实等价**（CI 的 repo-hygiene job 用的也是 `git ls-files`，`.github/workflows/ci.yml:63`），但脚本**没有覆盖 CI 的 webui-sync job**。脚本名与"卫生"语义让人以为它能拦住一切，实际不能。
2. **脚本对 `docs/` 无任何规则**。`.gitignore:104` 有意不忽略 `docs/`，脚本的 5 条 pattern 也不覆盖 `docs/`。所以 `docs/` 里的 5 个文档是否入库，完全取决于人的 `git add` 决策。**这不算缺陷**（`docs/` 本来就该人工判断），但意味着"仓库卫生"在此处是**人工纪律**而非工具保障。
3. **`.gitignore` 的 CRLF 陷阱已被正确防住**（脚本第三节 3 个 canary 全 PASS）。这是本仓库卫生纪律做得最好的一块。

---

## 8. 已验证无问题的区域（简短理由）

| 区域 | 验证方式 | 结论 |
|---|---|---|
| `internal/ratelimit` 限速器 | 读 `limiter.go` 全文 + `limiter_test.go` / `limiter_audit_test.go` | 逻辑自洽。不限速时不建桶（`limiter.go:107-109`）、超大 est 不饿死 key（`:118-121`）、跨窗校正夹到 0（`:145-149`）、桶有清扫（`:50-61`）。**唯一的 P1-2 是文档错，不是代码错** |
| `internal/outwire` 错误映射 | 读 `errors.go` 全文（`:32-105` 映射表、`:287-352` 故障转移与冷却判定） | 与 DESIGN §9 逐条对得上：401/403→502 `upstream_auth_error`、402→`upstream_quota_exhausted`、429→`rate_limit_exceeded`、404/410→404 `model_not_found`、超时→504 `upstream_timeout`。404 不冷却凭据的判定（`errors.go:286` 注释 + `CredentialCooldown` switch 无 404 分支）正确 |
| `internal/config` 配置解析与时区 | `go test -run 'Defaults\|Timezone\|Config' ./internal/config/...` 通过；读 `timezone.go` 全文 | `ApplyTimezone` 双写 `TZ` + `time.Local` 且顺序正确（`main.go:145` 早于 `:195`）。`defaults_consistency_test.go` 钉住三处超时一致 + probes 显式关闭 + audit_log_keep_rows |
| 日界 / day bucket | `go test -run 'TZ\|DayExpr\|Day' ./internal/store/` 通过；读 `usage_archive.go:465` | `dayExpr` 单常量两方向共用；`day_bucket_tz_test.go` 用子进程钉住 TZ 跟随。**脆弱点在调用顺序（已列 P2-9），但当前状态正确** |
| `internal/probe` 与熔断的隔离 | 读 `probe/runner.go` 全文；`rg 'RecordTargetFailure\|RecordCredential' internal/probe` → **零命中** | 探测确实不写熔断/冷却，DESIGN §12.2 的"隔离红线"成立 |
| `internal/upstream` 的池重建原子性 | 读 `PrepareFromStore`（`:270-397`）与 `Install`（`:399-431`） | 先构建后替换（`Prepare` 不触碰运行状态，`Install` 锁内换入），构建失败时旧池原样保留。熔断状态跨重建保留且 `halfOpen` 被正确清零（`:424`） |
| `internal/snapshot` 的并发契约 | 读 `snapshot.go:238-261` | `atomic.Pointer` 单写者（`Swap`）多读者（`Get`），`Get` 在 nil 时返回空快照而非 panic（`:248-255`） |
| `internal/server` 登录限速 | 读 `server.go:227-390` | `until.IsZero()` 判定（`:347`）、`Success` 不删有在途请求的条目（`:373-379`）、并发额度归还（`:353-356`）都对。DESIGN §11.4 末尾的三个坑都已修 |
| `internal/crypto` 密钥处理 | 读 `main.go:166-193` | 主密钥不可用时**拒绝启动**（`:173-175`），不降级为明文写库；`require_strong_master_key` 的 fail-closed 与 bootstrap 令牌一致。`crypto.DecryptWithFallback` 的明文兼容是显式取舍（DESIGN §12.1.1） |
| `internal/auth` 下游 Key 校验 | 读 `auth.go` 全文 | SHA-256 索引、归属校验（`ErrKeyUnowned`）、禁用/过期/IP 白名单、组白名单解析全部有实现与测试（`auth_test.go` 14 个用例） |
| `scripts/check-repo-hygiene.ps1` 本身 | 实跑通过（336 个跟踪文件，17 探针全过，canary 全过） | 逻辑正确、只读、不改索引。**局限是覆盖面**（只查索引，不查工作树 diff；不覆盖 webui-sync job）——已列 §7.2 |
| `go build` / `go vet` / `gofmt` | 已知基线，未重复验证 | 按题面基线 |

---

## 9. 待确认项（证据不足，明确列出缺什么）

| # | 事项 | 缺什么证据 |
|---|---|---|
| 1 | `internal/store/settings_dao.go` 的 `RuntimeDefaults` 兜底常量是否与 `internal/config.setDefaults` 和 `web/src/views/Settings.vue` 的 `RUNTIME_DEFAULTS` 一致 | 需读 `internal/store/settings_dao.go` 的 `RuntimeDefaults` 默认值块全文并逐字段比对。**本次只确认了三处（config.example / docker / setDefaults）由测试钉住，`app_settings` 后端兜底与前端 `RUNTIME_DEFAULTS` 未逐一核对** |
| 2 | `internal/admin` 的 `derefStr`/`derefInt` 是否确实只出现在 Create 路径 | 需逐调用点核对 `internal/admin/helpers.go` 的两个函数与全部 `derefStr(`/`derefInt(` 出现位置。**本次只确认了它们是导出辅助函数、无编译期约束** |
| 3 | `bootstrap` 的 `api_key` vs `api_key_env` 优先级：文档说"`api_key` 优先"，代码是 `api_key_env` 覆盖 | 需确认 `internal/config/config.go:490-500` 的 `BootstrapCredential` 校验块是否另有优先级处理。**从 `cmd/gateway/main.go:1143-1146` 的读法看是 env 优先，与 DESIGN §12.2（`DESIGN.md:1289`）的表述相反** |
| 4 | `usage_records` 的 `route_trace` 字段是否在所有写入点都被填充 | 需核对 `internal/store/usage_dao.go` 的 `CreateUsageRecord` 与 `cmd/gateway` 的所有 `usage.record(...)` 调用点。**本次只确认了流式与非流式两条主路径都填了 `RouteTrace`**（`main.go:2951` 等） |
| 5 | `web/src/views/Upstream.vue` 是否真的无路由引用（孤儿前端文件） | **已确认**：`rg "views/Upstream"` 全仓只命中 `web/src/views/ModelGroups.vue:197` 的一句注释；`web/src/router.ts:18` 的 `/providers` 直接指向 `Providers.vue`。但 `web/src` 的构建产物 `internal/webui/dist` 中是否含该组件的 chunk，需在 `web/dist` 里比对（本次未做） |

---

## 附录 A：审计过程中执行的动作（全部只读）

```
git log / git status / git diff / git show / git ls-tree / git ls-files   # 只读
go list -f '{{.ImportPath}} -> {{join .Imports " "}}' ./internal/... ./cmd/...   # 只读
go test -count=1 -run 'Defaults|Timezone|Config' ./internal/config/...   # 通过
go test -count=1 -run 'TZ|DayExpr|Day' ./internal/store/                # 通过
pwsh -File scripts/check-repo-hygiene.ps1                                # PASS（脚本自身只读）
rg / Select-String / Get-Content 全文阅读                                 # 只读
```
+ **未执行**：任何 git 写操作（add/commit/checkout/reset/clean）、任何 `npm install`/`npm run build`（会写 `web/dist` 与 `internal/webui/dist`）、任何对跟踪文件的 `apply_patch`。
+ **唯一的写操作**：新建 `work/audit-2026-10-20/A1-architecture.md`（`work/` 已被 `.gitignore:105` 忽略，不进索引）。

## 附录 B：关键文件行号索引

| 主题 | 位置 |
|---|---|
| 路由表（60+ 管理端点） | `cmd/gateway/main.go:238-260`, `370-517` |
| 配额预占（key + 用户两级） | `cmd/gateway/main.go:1816-1919` |
| 限速编排 | `cmd/gateway/main.go:1749-1814` |
| 余额预检 | `cmd/gateway/billing.go:667-720` |
| 故障转移链 | `cmd/gateway/main.go:2006-2320` |
| `rateCommit`（预占收尾，6 个手工 commit 点） | `cmd/gateway/main.go:1518-1596`；调用点 `:1908,1924,1948,2002,2058,2098` |
| `usageRecorder` | `cmd/gateway/main.go:3108-3516` |
| `runtimeReloader` | `cmd/gateway/main.go:889-1074` |
| 后台循环（prune/recompute/probe） | `cmd/gateway/main.go:693-821` |
| 优雅关闭 | `cmd/gateway/main.go:823-887` |
| `ingressCodec` 接口 + 三个实现 | `cmd/gateway/main.go:1384-1399`, `1629-1736` |
| 回放闸门 | `cmd/gateway/replay_gate.go:52`；调用点 `main.go:2616,2913` |
| 计价公式（3 份） | `cmd/gateway/billing.go:512` / `internal/store/usage_dao.go:341` / `internal/store/usage_archive.go:297` |
| 凭据冷却落库 | `internal/upstream/upstream.go:463-528`；`internal/store/credential_dao.go:115-123` |
| 熔断状态跨重建保留 | `internal/upstream/upstream.go:399-431` |
| 探针隔离 | `internal/probe/runner.go:83-193`；`internal/probe/runner.go:48` `Classify` |
| 鉴权白名单 | `internal/server/user_auth.go:161-207`, `222-254` |
| 鉴权 3 套口径 | `internal/server/user_auth.go:161` / `internal/admin/user_admin_handler.go:158` / `internal/admin/key_handler.go:104` |
| `dropDeadColumns` | `internal/store/store.go:668-730` |
| 时区应用 | `internal/config/timezone.go:60-94`；调用点 `cmd/gateway/main.go:145` |
| `dayExpr` | `internal/store/usage_archive.go:465` |
| 一致性测试 | `internal/config/defaults_consistency_test.go:16-145` |
| 前端路由表 | `web/src/router.ts:8-34` |
| 前端侧栏 | `web/src/App.vue:78-106` |
| 前端运行时默认 | `web/src/views/Settings.vue:212-218` |
| 孤儿前端组件 | `web/src/views/Upstream.vue`（240 行，无路由引用） |
| CI 门禁 | `.github/workflows/ci.yml:46-90`（hygiene）, `92-161`（go）, `163-234`（webui-sync） |
