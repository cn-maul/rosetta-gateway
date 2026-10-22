# A5 上游池与运行时状态审计

- 仓库：`rosetta-gateway`
- 版本：`main` = `ea19ce1`
- 范围：`internal/upstream/`（upstream.go / balance.go 及其测试）、`internal/routing/routing.go`、`internal/ratelimit/limiter.go`、`internal/snapshot/`、`internal/probe/runner.go`、`internal/config/ports.go` 与 `timezone.go`
- 日期：2026-10-20
- 方法：逐行阅读上述文件 + `rg` 精确检索 + 两处一次性实证复现（临时测试置于 `work/` 下，已 gitignore；**仓库内未修改任何 git 跟踪文件**）

## 结论摘要

| 级别 | 数量 |
|---|---|
| P0 | 1 |
| P1 | 3 |
| P2 | 6 |
| P3 | 3 |

最重要的发现是一个**已实测确认的数据面并发缺陷**：`ClaimTargetProbe` 把
「half-open 探测名额」的开销施加到了**从未熔断过的健康目标**上，于是任何开启
故障转移的路由在稳态下只能通过 1 个并发请求，其余全部被静默转移到链上第二目标。
这不是「冷却/探测两套判定打架」的边缘情况，而是熔断器的主路径本身。

---

## P0

### P0-1　`ClaimTargetProbe` 把 half-open 名额施加于健康目标，健康路由在故障转移开启时被串行化（吞吐塌成 1，并静默改投第二目标）

**位置**：`internal/upstream/upstream.go:564`（`ClaimTargetProbe`）、`:574-578`

```go
if time.Now().Before(h.until) {
    return false
}
if h.halfOpen {
    return false // 探测请求已在途，不放第二个
}
h.halfOpen = true
return true
```

**问题**：`h.halfOpen` 的语义被写成「有探测请求在途」，但实现是**全局互斥位**，
与「该目标是否处于冷却后的恢复期」无关。健康目标的 `until` 是零值
（`time.Now().Before(零值)` 为 false），`until` 检查通过后**直接撞上 `halfOpen`
检查**。而 `halfOpen` 只在 `RecordTargetSuccess` / `RecordTargetFailure` /
`ReleaseTargetProbe` / `Install` 四处被清（`upstream.go:600`、`:620`、`:642`、
`:424`）—— 也就是说，任何时刻一个健康目标最多只允许 **1 个在途请求**。

熔断器**不需要**这一点：熔断的存在意义就是「冷却期满后放一个探测进去，避免惊群」。
但惊群只在**刚从冷却中恢复**的那个瞬间才有意义，健康稳态下把它也一并限制，
是把熔断器的语义从「故障期间限流」误植成「常态限流」。

`Install`（`upstream.go:408`）对存活 target 初始化 `&targetHealth{}`
（`until` 零值、`halfOpen` false），所以**每次 admin 写操作触发重建后，健康目标
只获得 1 个请求的配额**，直到某个请求走完记账路径。这是稳态行为，不是边缘竞态。

**调用侧放大**：`cmd/gateway/main.go:2113-2129` 在故障转移开启时对每个候选**先**
`ClaimTargetProbe`，失败即 `continue` 转到链上下一个目标，且只在「是最后一个候选」
时才回 502。因此第 2..N 个并发请求不会报错 —— 它们被**静默送到次目标**，
route_trace 记为 `skip/probe_busy_or_cooling`（`main.go:2117`），
只有一条 WARN 日志（`main.go:2119`）。次目标若配置为另一个真实供应商，
这就是**在健康状态下把流量送往另一家供应商并按另一家计费**，运维从界面上
看不出来（熔断状态显示「正常」）。

**实测证据**（临时测试 `work/audit-2026-10-20/probe/probe_concurrency_test.go`）：

```
=== RUN   TestHealthyTargetAdmitsConcurrentRequests
    第二个并发 claim 被拒：ClaimTargetProbe 返回 false，尽管目标未处于冷却
--- FAIL: TestHealthyTargetAdmitsConcurrentRequests (0.00s)
=== RUN   TestHealthyTargetThroughput
    健康目标放行 1/64 个并发 claim；期望 64
--- FAIL: TestHealthyTargetThroughput (0.00s)
```

测试前置状态为 `Install(..., map[string]bool{tid: true})`，即 target 存活、
从未失败 —— 与生产稳态完全一致。

**为什么既有测试没抓到**：`internal/upstream/pool_test.go:106-130`
（`TestClaimTargetProbe_AllowsOnlyOneAfterCooldown`）明确把「只放一个」断言为
**期望行为**，但它是在**先把 until 拨到过去**（`pool_test.go:114-116`）之后才断言，
即测试把「恢复期只放一个」与「健康期只放一个」当成同一件事。
`cmd/gateway/failover_test.go` 的用例全部是串行 `postChat`，天然测不出并发塌陷。

**前置条件**：`routes.failover_enabled = 1`（`internal/store/store.go:218`
默认 0，需管理员显式开启）。开启后该路由上的并发能力即降为 1。

**修复方向**：`halfOpen` 只应在 `until` **曾经非零且已过期**的恢复期参与判定
（例如加 `probed bool`，或仅在 `!h.until.IsZero() && expired` 时才检查 `halfOpen`）；
健康目标应当无条件放行。同时 `main.go:2113` 的 `ClaimTargetProbe` 失败必须能
区分「仍在冷却」与「健康目标被误拒」两种原因，否则这类退化仍然静默。

---

## P1

### P1-1　`ParseBalanceSpec` 的「相对路径」校验只检查前缀 `/`，强度与其注释承诺不符，且未做路径规范化

**位置**：`internal/upstream/balance.go:308-310`（校验）、`balance.go:332`（拼接）

```go
if !strings.HasPrefix(spec.Path, "/") {
    return nil, fmt.Errorf("path 必须以 / 开头（只允许相对路径，避免把请求打到别的域名）")
}
...
body, err := doBalanceGET(ctx, base+spec.Path, apiKey, spec.Auth == "anthropic")
```

`base + spec.Path` 是**字符串拼接**，不是 `url.ResolveReference`。校验只挡住了
`https://` / `http://` 前缀（不满足 `HasPrefix("/")`），却宣称「避免把请求打到
别的域名」。

实测（`work/audit-2026-10-20/probe/spec_test.go`）：

```
path="//evil.example/x"        ACCEPTED -> 最终 host="good.example"
path="/\evil.example/x"       ACCEPTED -> 最终 host="good.example"
path="https://evil.example/x" REJECTED: path 必须以 / 开头
path="/a/../b"                ACCEPTED
```

即 **主机逃逸在当前 `base+path` 拼接下不可达**（详见「已验证无问题」第 9 条），
真正成立的问题是：`spec.Path` 未做 `url.Path` 规范化（`path.Clean`）也未拒绝
`..` 与 `\`，使「相对路径」这一约束形同虚设。`/a/../b` 被放行意味着在某些上游
的 `/admin/../` 语义下可打到未授权路径；`/\` 在不同代理/框架下解析不一致。

定 P1 的理由：写入校验与查询校验共用同一函数
（`internal/admin/provider_handler.go:227` 与 `:393` 都调 `ParseBalanceSpec`），
这一点是对的；但校验强度不足以支撑它的注释承诺，而这条路径**携带 API key**。

### P1-2　`liveTargetIDs` 读失败时返回空集，`Install` 因此清空全站熔断状态，且 `Reload` 仍返回成功

**位置**：`internal/upstream/upstream.go:241-251`、`cmd/gateway/main.go:1075-1079`、
`cmd/gateway/main.go:1063`、`cmd/gateway/main.go:1068`

```go
targets, err := st.ListAllRouteTargets(ctx)
if err != nil {
    return map[string]bool{}
}
...
rr.pool.Install(providers, liveTargetIDs(ctx, rr.db))
snapshot.Swap(snap)
rr.markClean()   // ← 仍被清除
```

`Install` 的语义是「集合外的条目一律丢弃」（`upstream.go:414-430`）。所以
`liveTargetIDs` 读失败时，**全站熔断计数与 `until` 被一次性清零**，而
`Reload` 返回 nil、`markClean()` 把 dirty 标志清掉 —— 失败被彻底吞掉，
没有任何日志。

该失败是**可达**的：`Reload` 的 ctx 自带 30s 预算
（`cmd/gateway/main.go:935`），`RebuildFromDB`（`main.go:1059`）已经成功读过
同一张表（`snapshot/rebuild.go:112`），紧接着的第二次独立查询
（`liveTargetIDs`，`main.go:1063`）仍可能因 ctx 刚好到期而失败。

后果：正在熔断中的坏上游立即复活并被打满，而这恰好发生在数据库压力大
（最可能有故障）的时刻。`until` 是熔断器唯一的自愈机制，被静默清零后
要重新攒满 threshold 次失败。

注释（`upstream.go:238-240`）承认了取舍动机（「清掉全部熔断只是让坏上游短暂复活，
保留已删目标的状态则会单调堆积」），但**漏了「不记录、不置 dirty」这一半**：
正确形态是保留上一次成功的集合，或至少 `MarkDirty()` + 打 ERROR。

### P1-3　运维显式关闭目标级熔断（threshold=0）后，`ClaimTargetProbe` 的全局互斥依然生效

**位置**：`internal/upstream/upstream.go:583-590`、`internal/upstream/upstream.go:564-579`、
`cmd/gateway/main.go:2074`、`cmd/gateway/main.go:2113`

`RecordTargetFailure` 已正确处理「关了熔断也要归还名额」
（`upstream.go:585-589`）。但 threshold<=0 时，`main.go:2113` 仍然对每个候选调
`ClaimTargetProbe`（判定条件是 `route.FailoverEnabled`，与 threshold 无关），
于是 P0-1 的并发塌陷**在熔断已被显式关闭时依然存在**。

即：运维在设置页把「熔断阈值」设为 0（本意「不要熔断，也别拦我」），
得到的却是「仍被限成 1 并发，且被静默转移到次目标」。这是「运维以为已关闭、
实际仍在限流」的独立可观测后果，修复 P0-1 时必须一并处理
（threshold<=0 时应完全跳过 `ClaimTargetProbe`）。

---
## P2

### P2-1　`routing.RouteIndex` 无任何并发保护，不可变性只靠注释约定，且导出方法允许事后原地修改已发布快照

**位置**：`internal/routing/routing.go:142-190`、`internal/snapshot/snapshot.go:13-22`

`RouteIndex` 的 6 张 map 全是裸 map，`Add*` 无锁。契约写在
`internal/snapshot/snapshot.go:15-19` 的注释里：「必须保持不可变，绝不允许原地修改
已发布的 Snapshot」。这是**纯约定**，无编译期或运行期强制。

当前生产写路径确实遵守：`RebuildFromDB`（`rebuild.go:34`）与
`buildSnapshotFromConfig`（`cmd/gateway/main.go:1211`）都是「新建 → 填充 →
Swap」，填完即冻结；`Resolve` / `buildCandidates` 无锁读取安全。

但 `AddRoute` / `AddRouteTarget` / `AddProvider` / `AddUpstreamModel` 是**导出**的，
任何包（含未来的 admin handler 或测试辅助）都能在 Swap 之后调用。一旦有人为
「让新配置立即生效」而这么做，热路径的 map 读写立即变成并发 map 读写 ——
**Go 运行时 fatal error（不可恢复的 crash，不是 panic）**。

同理，`ListRoutes()`（`routing.go:293`）返回的切片元素是索引内部的 `*Route` 指针；
调用方修改 `*Route.Enabled` 就是原地改已发布快照。
`cmd/gateway/main.go:3550`、`internal/admin/group_handler.go:307` 与 `:464`
都在遍历这些返回值。

我的 `-race` 全部通过，**未观察到实际竞争**，故定 P2。

### P2-2　链的顺序由单条 SQL 的 `ORDER BY` 单点承担，`Candidate.Position` 存在但从未被读取

**位置**：`internal/routing/routing.go:149-151`、`internal/snapshot/rebuild.go:111-125`、
`internal/store/route_target_dao.go:38`、`cmd/gateway/main.go:2087`

```go
// AddRouteTarget 追加一个链成员。快照重建按 route_id, position 顺序喂进来，
// 故这里 append 即为有序；调用方保证 position 递增。
func (ri *RouteIndex) AddRouteTarget(t *Target) {
    ri.targetsByRoute[t.RouteID] = append(ri.targetsByRoute[t.RouteID], t)
}
```

唯一的排序来源是 `route_target_dao.go:38` 的 `ORDER BY route_id, position`，
注释里的「调用方保证 position 递增」**没有断言**。而链顺序**直接决定钱花给谁**：
position 0 是主目标。

更值得注意：`Candidate.Position`（`routing.go:120`）被填充了却**从未被读** ——
`main.go:2087` 的候选循环用的是切片下标 `i`（`isLast := i == len(active)-1`），
不是 `cand.Position`。于是「顺序」有两种表示（切片序 vs 字段值）并存，
无一致性校验。若哪天有人为「稳健」改成按 `Position` 排序或跳号处理，
当前下标逻辑会与之分叉。

`main.go:2070` 的 `active[:limit]` 预算截断让顺序错误的后果被放大：
顺序错了就会稳定地把真正的主目标砍掉、每次都打次目标，
而界面上的 position 列显示的仍是 DB 里的正确值。

### P2-3　`AddProvider` 把 ID 与 Slug 放进同一张 map，两个查找函数共享命名空间

**位置**：`internal/routing/routing.go:161-170`、`:265`、`:429-435`

```go
ri.providers[p.ID] = p
ri.providers[p.Slug] = p
```

注释（`routing.go:155-160`）已诚实承认取舍并给出「slug 由 slugify 从显示名生成」
作为不可达理由。但 `rebuild.go:63` 传的 `ID`/`Slug` 都是 DB 真实值，
而 provider 的 slug 可经 admin PATCH 写成任意字符串（含另一个 provider 的 32 位 hex ID）。
一旦发生，后写的条目覆盖先写的，`buildCandidates`（`routing.go:265`）用
`findProviderByID` 解析链成员会把链上的目标解析到**另一个供应商**。
`ListUpstreamModels`（`routing.go:409-427`）的 `seen` 去重也依赖两个 key
指向同一指针才正确。

标「待确认」：需确认 admin 的 slug 写入是否限制为 slugify 产物（我未审 A 组的
admin 校验路径）。

### P2-4　探测的 stagger 只在抢到信号量之后生效，且 `intervalStagger` 是硬编码 2500ms，与配置的探测间隔无关

**位置**：`internal/probe/runner.go:133-151`、`:164-166`、`:84-87`

```go
select {
case sem <- struct{}{}:
case <-ctx.Done():
    return
}
defer func() { <-sem }()
time.Sleep(time.Duration(rand.Int63n(int64(intervalStagger))))  // 占着槽位在睡
r.probeOne(ctx, m, timeout)
```

两个问题：

1. **注释与实现不符**：`:164-166` 说「取间隔的 1/4」，但 `intervalStagger` 是独立
   的包级 `var = 2500ms`，**与 `cfg.Probes.IntervalSec` 毫无关系**。
   当运维把 `interval_sec` 配小（如 5）时，stagger 仍为 2.5s，一轮探测跨度
   超过间隔；`runOnce` 末尾的 `wg.Wait()`（`runner.go:152`）会让下一轮
   ticker 事件被丢弃（`time.NewTicker` 的 channel 容量为 1），
   探测周期被静默拉长。「1/4」这个意图没有任何测试锚定
   （`internal/probe/runner_test.go` 仅 2.6KB）。
2. **stagger 占用并发槽**：sleep 期间仍持有信号量令牌。真正打散需要把 sleep
   放在抢令牌**之前**，否则 4 个令牌意味着最多 4 个在途、2.5s 后几乎同时发出。

### P2-5　探测与熔断是两套互不知情的健康真相；`Classify` 在 admin 层被复制一份且阈值硬编码，与配置静默分叉

**位置**：`internal/probe/runner.go:1-8`、`internal/admin/probe_handler.go:141-160`、
`internal/upstream/upstream.go:583-622`

包注释把「探测失败绝不写 cooldown」定为**红线**（`runner.go:5-8`）。这个纪律本身
是对的（探测用 provider 自身凭据、max_tokens=1，回答的是「这个 model_id 现在
能不能推理」，与「这个目标此刻能否服务生产流量」不是同一问题）。但后果是系统
里有两套互不知情的健康真相：`pool.targets` 驱动**实际路由**，
`model_probes` + `Classify` 驱动**界面红绿**。

更具体的一致性问题：`Classify` 在 admin 层被**复制了一份**
（`probe_handler.go:141-145` 明确说明是为了避免 import probe 包），
阈值**硬编码为 3 与 0.8**：

```go
if h.ConsecutiveFail >= 3 {           // probe_handler.go:150
    return "down"
}
if float64(h.WindowOK)/float64(h.WindowCount) >= 0.8 {   // :154
```

而 `probe.Classify`（`runner.go:48`）从 `cfg.Probes.DownThreshold` /
`HealthyRate` 读。默认值恰好都是 3 / 0.8，所以**当前不显形**；运维一旦在
设置页改健康阈值，`probe` 包遵守、`admin` 界面不遵守，界面 down 判定与配置值
静默分叉。

方向性后果：探测判 down 的模型数据面照打（直到熔断计数攒够）；
熔断中的目标界面可能显示 healthy（探测用不同凭据、不同参数，结论不同也合理）。
这不是 bug，但**没有任何对齐机制**，包注释也只说了「红线」、没说
「因此两套状态会长期不一致，界面不承诺等于路由行为」。

### P2-6　余额查询与模型发现的 HTTP client 不读 provider 挂的出站网络，与数据面口径相反

**位置**：`internal/upstream/balance.go:242-247`、`internal/upstream/upstream.go:1093-1098`、
`internal/upstream/upstream.go:1270-1272`、`:1338`

数据面明确恢复了环境代理（`upstream.go:1272`），并额外支持**按 provider 的
networks 代理**（`proxyHTTPClientFor`，`upstream.go:1338`）。但余额查询
（`balance.go:242`）与模型发现（`upstream.go:1093`）各自 `&http.Client{...}`，
**Transport 为 nil → 走 `http.DefaultTransport`**，即只认
`HTTP_PROXY`/`HTTPS_PROXY`/`NO_PROXY`，**完全忽略 `ProviderEntry.ProxyURL`**。

后果：一家上游在配置上必须走代理（`networks.proxy_url`，如内网出口）才能连通，
数据面正常，但**「获取模型」与「查询余额」必然失败** —— 而这两个正是管理员
配置上游时最先点的两个按钮。失败信息是连接超时 / 502，与「代理没生效」
毫无关联，正是本项目反复消除的那类误导。

附带：这两个 client 都是**每次调用新建**（无连接池复用），而
`DiscoverUpstreamModels` 会对每个候选 URL 试 1~2 次（`upstream.go:1104-1112`），
批量发现模型时建连成本不低。

---

## P3

### P3-1　`upstreamProxyBuildErr` map 是死代码：声明并初始化后从未被写入或读取

**位置**：`internal/upstream/upstream.go:1324-1328`

```go
var (
    upstreamProxyMu       sync.Mutex
    upstreamProxyClients  = make(map[string]*http.Client)
    upstreamProxyBuildErr = make(map[string]error)   // 全仓库仅此一处出现
)
```

`rg upstreamProxyBuildErr` 在整个仓库只有这一行命中。`proxyHTTPClientFor`
（`upstream.go:1338`）把构造错误直接**返回**给调用方（记进
`ProviderEntry.ProxyConfigError`），从不缓存。

即：代理地址非法时，每次 `PrepareFromStore` 都会为**每把凭据**重新 `url.Parse`
+ 重新报错一次（`upstream.go:1342-1346`），错误日志按凭据数放大
（`upstream.go:389-392`）。50 把凭据的 provider 配错代理 → 50 条同样的 WARN。
这个 map 看起来是某次重构的残留。

### P3-2　限速窗口键用 UTC 对齐，`Retry-After` 用本地时区对齐，两者口径不一致

**位置**：`internal/ratelimit/limiter.go:71`、`:152-155`

```go
if start := now.Unix() / 60; b.windowStart != start {   // UTC 对齐
    ...
}
func windowRemaining(now time.Time) time.Duration {
    next := now.Truncate(windowLen).Add(windowLen)      // 按 Location 取整
```

`time.Truncate` 按 wall clock 取整，`Unix()/60` 是 UTC 分钟。分钟级对齐下
两者对**任何非整小时时区偏移都相差一个常数分钟数**，后果是 `Retry-After`
与实际窗口翻转点错开至多 1 分钟。

`internal/config/timezone.go:92` 会把 `time.Local` 改写成配置时区，所以这个
偏移是**真实可触发**的（非 UTC 部署）。影响面仅是 429 响应的 `Retry-After`
头偏差与窗口边界观感，不影响限速本身的正确性（窗口仍精确 60s）。
`limiter_test.go` 全在默认本地时区跑，覆盖不到。

### P3-3　`CheckListenPort` 只查浏览器保留端口表，不做占用检查，命名暗示后者

**位置**：`internal/config/ports.go:105-127`、`cmd/gateway/main.go:156`

函数只查一张 Chromium 保留端口表（`ports.go:22-103`），**不检查端口是否被占用**，
调用点（`main.go:156`）也只发一条 WARN。注释（`ports.go:111-112`）已明说
「端口无法解析或越界时返回 blocked=false，那是另一类错误，交给 net.Listen」。

功能上没问题（这是刻意的、有注释的设计），但命名 `CheckListenPort` 在 Go 里
强烈暗示 `net.Listen` 前置检查。审计点 (9) 问「端口占用检查的竞态」——
答案：**本包不做占用检查，因此不存在竞态**；真实占用竞态存在于
`net.Listen` 之后，属 A4/A6 范畴。

---
## 已验证无问题的区域

1. **冷却状态的数据竞争（历史缺陷 be18edd / `cooldown_race_test.go`）—— 已确认无新的竞争面。**
   `MarkCredentialCooldown`（`upstream.go:463-495`）与 `RecordCredentialSuccess`
   （`:500-528`）都严格遵守「锁内判断并快照、锁外只用自己的局部值」：
   `until` 在 `p.mu` 内取值后才落库（`:474-476`），`changed` 判定在锁内
   （`:511-515`）。落库调 `store.SetCredentialCooldown(ctx, id, "cooling", until)`
   传的是局部快照值，**不再触碰共享 `cred` 指针**（`credential_dao.go:122-124`
   只读入参、不回写）。

2. **「测试用私有 mu 写 cred 而生产在 p.mu 下写同一字段」这一模式已不存在。**
   `cooldown_race_test.go:183-186` 现在用 `p.mu.Lock()` 而非测试私有锁；
   `pool_test.go:114-116` 与 `:140-142` 也都走 `p.mu`。注释
   （`cooldown_race_test.go:92-108`、`:176-179`）明确记录了修正原因与
   `-race` 实测输出。

3. **竞态检测实测通过**：`CC=C:/mingw64/bin/gcc.exe go test -race -count=1`
   跑 `internal/upstream`（5.216s）、`internal/ratelimit`（1.053s）、
   `internal/routing`（1.053s）、`internal/snapshot`（2.435s）——
   **全部 ok，无 DATA RACE**。与 P0-1 不矛盾：P0-1 是**逻辑**缺陷
   （无竞争，只是语义错）。

4. **`RecordTargetSuccess` 刻意不动 `until`**（`upstream.go:609-622`）—— 判断正确。
   熔断期内的成功可能来自熔断前已发出的在途请求（非流式默认超时 300s，
   `config.go:292`，远长于 60s 熔断），拿陈旧响应解除熔断会让坏目标提前复活。

5. **冷却的持久化闭环完整**：`MarkCredentialCooldown` 落库（`upstream.go:490-494`）
   + `PrepareFromStore` 从库读回（`:354-357`）+ `store.SetCredentialCooldown`
   写 `status`/`cooldown_until`（`credential_dao.go:122-124`）构成闭环。
   仅改内存会让「刚判 401 无效的 key 在任意 admin 写操作后立刻回到轮换」，
   这个坑已避开。

6. **快照写路径的原子性**：所有写路径都是「新建 → 填充 → Swap」
   （`snapshot.go:240`、`:259`；写方 `rebuild.go:40` 与 `main.go:1212`），
   失败时**不执行 Swap、旧快照继续生效**（`rebuild.go` 全部读失败路径
   `return nil, err`；`main.go:1060-1062` 在 `Install` 之前就 return）。
   权限相关读（users / group_models / keys）**一律致命**（`rebuild.go:132-147`）
   而非降级为空表 —— 方向正确（零值偏向收紧，见 `snapshot.go:76-86`）。

7. **热替换的失败重试闭环**：`runtimeReloader` 的 `dirty` 标志 + 指数退避
   （`main.go:942-1010`）确保「响应已 200『已禁用』但重建失败」不会无限期失准。
   `markClean` 只在 `Install` 与 `Swap` 都成功后调用（`main.go:1068`）。
   （P1-2 是这条闭环的一个漏洞：`liveTargetIDs` 的内部失败被吞掉，
   `Reload` 仍走 `markClean`。）

8. **下游 target 参与路由的正确性**：`Install` 丢弃非存活 target 条目
   （`upstream.go:414-430`），而 `buildCandidates`（`routing.go:258-274`）
   逐个过滤 `!t.Enabled` / provider 缺失或禁用 / model 缺失或禁用。
   「已下线 target 仍参与路由」**未发现** —— 唯一残留窗口是 P1-2 的
   `liveTargetIDs` 空集路径，但那影响的是熔断状态而非路由成员资格。

9. **余额查询的三个安全纪律仍然成立**：
   （a）不跟随重定向（`balance.go:242-247`，`ErrUseLastResponse`）；
   （b）非 200 响应体**只进日志不进 error**（`balance.go:407-415`，
   `upstream.go:1196-1212` 同一纪律），不把上游内部细节回显给终端用户；
   （c）`hostIs`（`balance.go:260-266`）用 `EqualFold(u.Hostname(), want)`
   **精确相等**，旧的 `strings.Contains` 已被修掉，
   `api.openai.com.evil.example` 不会被误判成 OpenAI 官方
   （`Contains` 版的漏洞正是基线里提到的那个）。

10. **`BalanceSpec` 的 path 主机逃逸不可达（实测）**：`base+spec.Path` 是字符串
    拼接，`//evil.example/x` 拼进 `https://good.example//evil.example/x` 后
    `url.Parse` 解析出的主机仍是 `good.example`（见 P1-1 实测输出）。
    即当前实现**不会**因 `path` 把带 API key 的请求发到别的主机。
    P1-1 定 P1 是因为校验强度与注释承诺不符 + 路径穿越未规范化，
    而非因为主机逃逸。

11. **重定向禁令在模型发现路径同样成立**：`upstream.go:1093-1098` 的
    `CheckRedirect` 返回 `ErrUseLastResponse`，注释（`:1087-1092`）明确
    「3xx 原样作为响应返回」，不会被 302 带到 `169.254.169.254` 一类地址。

12. **bootstrap 的代理归并与清单口径一致**：`cmd/gateway/main.go:1106-1126`
    用 `networkByURL map[string]string` 按 proxy URL 归并成**一条** network
    （而非每 provider 一条），避免 `UNIQUE(name)` 拒或各写一份。
    网络先于 provider 建（`:1112-1113` 注释说明 FK 顺序），
    `NetworkID` 写入 `store.Provider`（`:1135`），
    池侧由 `ProviderProxyURLs` 在读路径 JOIN 出 `ProxyURL`
    （`provider_dao.go:26-28`、`:104`）。口径一致。

13. **限速器本身的正确性**：`AllowRPM` 被拒不计数（`limiter.go:93-97`）、
    `ReserveTPM` 单请求超额度时记满而不累加到 `est`（`:118-121`，
    防永久饿死）、`CommitTPM` 夹下界 0（`:145-149`，防跨窗负值放宽限速）、
    `tpmLimit<=0` 提前返回不建桶（`:107-109`，防 map 随 key 增删无界增长）。
    `limiter_audit_test.go` 对这四条都有断言；我的 `-race` 运行覆盖了
    `TestLimiter_ConcurrentReserveCommit`（8×200 次
    Reserve+Commit+AllowRPM），通过。sweep 以 `touched` 为判据
    （`:47-61`）而非 `windowStart`，活跃 key 不会被误回收。

14. **`selectWeighted` 的权重全零保护**（`upstream.go:686-689`）：
    `rand.IntN(0)` 会 panic（把网关打挂），退化为均匀随机。
    这是可达状态（后台把所有凭据权重改成 0），已防住。

15. **`BuildFromStore` 的失败纪律**：`PrepareFromStore`（`upstream.go:270`）
    **不触碰池的现有状态**，整体失败（`ListProviders` 失败）返回 error 且
    providers 为 nil，调用方不得 `Install`（`main.go:1022-1025`）。
    「先清空 `p.providers` 再查库」的旧 bug 已消除，
    `pool_test.go:157`（`TestBuildFromStore_ErrorKeepsOldPool`）守住。

16. **时区应用的时机与全局副作用**：`ApplyTimezone`（`timezone.go:60`）在
    `cmd/gateway/main.go:145` 调用，早于 `store.Open`（`main.go:195`）、
    早于第一个 `go func()`（`main.go:685`）。`os.Setenv("TZ")` +
    `time.Local = loc` 两处都设、顺序正确，且注释（`timezone.go:25-36`）
    明确记录了第一版修复只设 `time.Local` 导致 Linux CI 红的实测
    （modernc.org/sqlite 走 C tzset、不看 Go 的 time.Local）。
    对 `time.Local` 的写发生在任何 goroutine 启动前，**不构成数据竞争**。
    空串语义为「沿用进程现有时区」，且会**报告实际生效值**
    （`timezone.go:66`），启动日志把它打出来（`main.go:150`）——
    这让「容器里是 UTC 而用户在东八区」这个问题可被发现。

17. **流式连接回收与 SDK 超时语义**：已核对 `rosetta@v1.2.0`：
    `WithTimeout` 只作用于**非流式**调用（`client.go:186`、`:245`、`:314`），
    已建立的流**不带 SDK 侧超时**（`stream.go:82-84` 的注释与实现一致），
    由网关的 TTFT / 流空闲看门狗接管。因此 `upstream.go:1412` 的
    `WithTimeout(prov.Timeout)` **不会掐断长流式请求**。
    Transport 侧 `MaxIdleConnsPerHost: 64` + `IdleConnTimeout: 90s`
    （`upstream.go:1274-1275`）对长流合理：流结束、body 读完即归还空闲池。
    `client.Timeout` 刻意不设（`upstream.go:1269`），超时一律走 ctx。

18. **`upstreamHTTPClient` 补齐了手搓 transport 缺失的两个默认值**
    （`upstream.go:1278-1297`）：`DialContext`（10s）与 `TLSHandshakeTimeout`
    （10s）。注释解释了「手搓 Transport 不继承 `http.DefaultTransport` 的预置值」，
    以及缺失时黑洞上游要把「已死」判定拖到看门狗。代理版 client
    （`:1354-1370`）逐字抄了同一组参数，口径统一。

19. **代理地址非法时不让整个 provider 建池失败**（`proxyHTTPClientFor`，
    `upstream.go:1338-1374` + `buildClient:1393-1395`）：回落直连 client、
    错误记进 `ProviderEntry.ProxyConfigError`，并明确**不**计入
    `ProviderFailure`（`upstream.go:382-388`）—— 因为该清单语义是「未就绪」，
    而配错代理的 provider 是就绪的。这个区分是对的。

20. **`flexBool` 对 OpenRouter 双形态的解析**（`upstream.go:990-1013`）：
    布尔/对象/null 三形态显式分派，不认识的形状**返回 error 而非静默忽略**
    （`:1010-1012` 的取舍理由正确：静默会让「配置成功但没填上」）。
    `isObj` 标志避免把布尔 `true` 读成 `obj.Mandatory=false`。
    `effortLevelsFor`（`:1042-1058`）取第一个**非空**集合而非第一个字段。
    `hasVersionSuffix`（`:1159`）用 path 而非字符串后缀匹配，
    正确排除 `/v1beta`。

21. **模型名解析规则的边界**：`Resolve`（`routing.go:201-247`）命中**具名路由**
    且该路由被显式禁用时，立即返回 `ErrModelNotFound`（`:203-217`），
    **不**落到 `provider/model` 直连兜底。注释（`:204-215`）记录了这个 bug
    的形态：`public_name` 只校验非空、不限含 `/`，所以「acme/gpt-4」这种
    名字合法，禁用后旧实现会走兜底继续请求成功、禁用形同虚设。
    修正在位。通配符 / 别名 / `model_groups` 在本仓不存在（白名单走
    `ModelAllow`，`snapshot.go:87-152`），故无对应面。

---

## 附：本次审计产生的临时文件

为验证 P0-1 与 P1-1，在 `work/audit-2026-10-20/probe/` 下创建了两个临时
`_test.go`（`probe_concurrency_test.go`、`spec_test.go`）。
删除操作被环境策略拦截，因此该目录仍然存在。它位于已 gitignore 的 `work/` 下，
**不影响仓库状态**（`git status` 中无任何由本次审计引入的跟踪文件变更）。
