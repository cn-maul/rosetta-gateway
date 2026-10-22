# 核心网络部分审计：代理链路与直连链路

- 审计日期：2026-10-20
- 审计范围：`internal/upstream`（出站传输与代理）、`internal/store`（networks 表与派生读路径）、
  `internal/admin`（网络/上游管理面）、`internal/probe`（后台探测）、`internal/config`（bootstrap）、
  `cmd/gateway`（池重建与数据面装配）、`web/src`（出站网络界面）
- 方法：读代码 + 写临时测试**实跑验证每条结论**（测试已删除，工作区干净；
  `go build ./...` 与 `internal/{upstream,admin,store,config,probe}` 全绿）

---

## 0. 结论摘要

**数据面（/v1 转发、连通性测试、单模型探测、后台探测循环）的代理/直连分流是正确的。**
问题集中在两处：

1. **旁路（side channel）完全绕过 provider 选定的网络** —— 模型发现与余额查询
   （P0-1、P0-2）。其中余额查询**携带上游明文 API Key**。
2. **管理面写入边界不完整** —— bootstrap 配置的 `proxy_url` 不校验（P1-3），
   迁移对带空格的 `proxy_url` 会静默丢代理（P2-4）。

另有一组"文档/界面与实现不一致"的问题（P2-5、P3-7）与死代码（P3-6）。

### 已验证正常（勿重复排查）

| 检查项 | 结论 | 依据 |
|---|---|---|
| 代理 client 构造 | ✅ `http.ProxyURL` + 与直连**逐项一致**的 Transport | upstream.go:1338-1374；实测参数 parity |
| 流量真的拐进代理 | ✅ 真发请求验证 | upstream.go:1338；既有 proxy_test.go:145 |
| socks5 / socks5h | ✅ 真能 CONNECT | Go 1.27 原生支持（net/http/transport.go:120） |
| 池重建跟随网络变更 | ✅ netA→netB 派生 URL 正确切换 | upstream.go:270 PrepareFromStore |
| 网络写操作即时生效 | ✅ POST/PATCH/DELETE → AutoReload → Reload | server/autoreload.go:59 |
| 删网络 → 上游回直连 | ✅ FK `ON DELETE SET NULL` + `_foreign_keys=ON` | store.go:143、store.go:35 |
| 代理配错 → 回落直连 + WARN | ✅ 不阻断建池 | upstream.go:192、upstream.go:389 |
| 直连 client 的环境代理 | ✅ 刻意行为（注释写明"恢复环境代理"） | upstream.go:1264-1299 |

---

## 1. 缺陷清单

### 🔴 P0-1　模型发现完全绕过 provider 选定的网络代理

**位置**：[internal/upstream/upstream.go:1093](internal/upstream/upstream.go#L1093)

```go
client := &http.Client{
    Timeout: 15 * time.Second,
    CheckRedirect: func(req *http.Request, via []*http.Request) error {
        return http.ErrUseLastResponse
    },
}
```

Transport 为 nil → 落到 `http.DefaultTransport`，其 `Proxy` 是 `ProxyFromEnvironment`。
**结果：只认进程环境变量，完全不认 provider 配置的网络。**

**实测**（假代理 + 假上游，各自带计数器）：

```
discover err=<nil> proxyHits=0 directHits=1
```

**后果**

- 给"必须走代理才能访问"的上游点「发现模型」必然失败（症状：超时/连接错误，指向"网络"而非"代理没生效"）。
- 若本机恰好能直连该上游，请求就**从用户没指定的出口发出去了** ——
  而 UI 上该 provider 明写着"经网络 X 转发"（[ProviderDetail.vue:808](web/src/views/ProviderDetail.vue#L808)）。

**修法**：改为按 provider 的派生 `ProxyURL` 取 client，保留"不跟随重定向"的纪律
（该纪律本身是对的：防止探测面扩大到任意主机）。

---

### 🔴 P0-2　余额查询同样绕过代理，且携带上游明文 API Key

**位置**：[internal/upstream/balance.go:242](internal/upstream/balance.go#L242)（包级 `balanceHTTPClient`）
与 [internal/upstream/balance.go:391](internal/upstream/balance.go#L391)（`doBalanceGET` 带 `Authorization: Bearer`）

与 P0-1 同源。实测同样是 `proxyHits=0, directHits=1`。

**严重性高于 P0-1**：请求带着上游的明文 API Key，从一条**未被配置的链路**出去。

**修法**：把包级单例改成按 provider 派生（每个 provider 的网络不同，单例天然做错）；
注意 `balanceHTTPClient` 承载"不跟随重定向"的纪律，改写时必须保留。

---

### 🟠 P1-3　bootstrap 配置的 `proxy_url` 完全没有校验

`validateProxyURL` 只存在于 [internal/admin/network_handler.go:144](internal/admin/network_handler.go#L144)（admin 侧），
[internal/config/config.go:482](internal/config/config.go#L482) 的 bootstrap 校验里**没有 proxy_url**。

实测：`config.validate()` 接受 `"127.0.0.1:7897"`（缺 scheme ——
而 [CONTRIBUTING.md:72](CONTRIBUTING.md#L72) 里正是这个写法）。

该值随后被 [cmd/gateway/main.go:1119](cmd/gateway/main.go#L1119) 原样写进 networks 表 →
运行时判非法 → 回落直连 + 一条 WARN。

**为什么这条是 P1**：共享配置的一个错误会同时打穿**一批**上游，
而界面显示"经网络 X 转发"（[Providers.vue:188](web/src/views/Providers.vue#L188)）。

**顺带**：[internal/config/config.go:159](internal/config/config.go#L159) 注释写"http/https/socks5"，
漏了 admin 侧早已支持的 `socks5h`。

**修法**：把 `validateProxyURL` 提到 `internal/config`（或新建 `internal/netutil`）供两侧共用。

---

### 🟠 P2-4　迁移时带空格的 `proxy_url` 会静默丢代理

**位置**：[internal/store/network_dao.go:185](internal/store/network_dao.go#L185) 与
[internal/store/network_dao.go:232](internal/store/network_dao.go#L232)

```go
// 取值：TRIM 后的
rows, err := s.read.QueryContext(ctx,
    `SELECT DISTINCT TRIM(proxy_url) FROM providers
      WHERE proxy_url IS NOT NULL AND TRIM(proxy_url) <> ''`)
// 回填：拿 trim 后的值去比原值
`UPDATE providers SET network_id = ? WHERE proxy_url = ?`, n.ID, u)
```

原值带空格 → WHERE 不匹配 → `network_id` 保持 NULL → 回落直连。

**实测**（老库两行，同一 URL，一行带空格）：

```
provider p1 network_id=""                 proxy_url=""            ← 丢了
provider p2 network_id="net-8ad4..."       proxy_url="http://127.0.0.1:7897"  ← 正常
```

**可达性**：老 admin 的 PATCH 路径会 `TrimSpace`（旧 provider_handler.go），
但**创建路径不 trim**（`ProxyURL: req.ProxyURL` 原样落库），
所以"从别处复制来的带空格地址"是能进老库的。

**修法**：`WHERE TRIM(proxy_url) = ?`。

---

### 🟡 P2-5　"直连"在有环境变量时并不是真直连，而 UI 说"不经过代理"

- 实现：[internal/upstream/upstream.go:1272](internal/upstream/upstream.go#L1272) → `http.ProxyFromEnvironment`
  （**刻意行为**，注释写明"恢复环境代理"）
- 界面 A：[web/src/views/Providers.vue:232](web/src/views/Providers.vue#L232) → "直连（默认，不经过代理）"
- 界面 B：[web/src/views/ProviderDetail.vue:928](web/src/views/ProviderDetail.vue#L928) → "直连（不经过代理）"
- 界面 C：[web/src/views/Networks.vue:158](web/src/views/Networks.vue#L158) → 准确写了"读进程环境变量里的 HTTP_PROXY / HTTPS_PROXY"

**三处文案自相矛盾**，A/B 在设了 `HTTPS_PROXY` 的部署里是假的。

**附带**：`ProxyFromEnvironment` 的环境变量在进程首次请求时读一次即缓存
（Go 内部 `envProxyOnce`），运行期改环境变量无效。

**修法**：三选一 —— 统一文案 / 直连真正 `Proxy: nil` / 界面上显式显示环境变量状态。
但别让 UI 承诺代码不兑现（本项目的明确禁忌）。

---

### 🟢 P3-6　死代码与无界增长

- [internal/upstream/upstream.go:1327](internal/upstream/upstream.go#L1327)
  `upstreamProxyBuildErr` 声明后从未使用
- [internal/upstream/upstream.go:1326](internal/upstream/upstream.go#L1326)
  `upstreamProxyClients` 只增不减：网络地址每改一次就多一个常驻 Transport，
  旧的没有 `CloseIdleConnections`
- [internal/store/network_dao.go:148](internal/store/network_dao.go#L148) `ProviderProxyURLs`、
  [internal/store/network_dao.go:109](internal/store/network_dao.go#L109) `NetworkProxyURLs`
  全项目无调用点（池构建走的是 provider_dao 的 LEFT JOIN）

---

### 🟢 P3-7　`ProxyConfigError` 注释与实现自相矛盾

- [internal/upstream/upstream.go:88](internal/upstream/upstream.go#L88)："管理面据此把「代理没生效」说出来"
- [internal/upstream/upstream.go:92](internal/upstream/upstream.go#L92)："实际暴露方式是建池循环里的一条 warn 日志"

实际只有日志。代理配错时 UI 上该 provider 一切正常，
而流量其实在直连 —— 又一处"界面在说谎"。

---

## 4. 追加发现：出站头超时（60s）与配置的非流式超时（300s）互相矛盾

> 这一条是审计范围向外扩时发现的，属于**核心网络传输层**，与代理/直连同源
> （它定义的是「一次出站请求最多能挂多久」）。

### 🔴 P0-4　非流式请求在 60s 被 transport 掐断，配置的 300s 从未生效

**位置**

- [internal/upstream/upstream.go:1277](internal/upstream/upstream.go#L1277)
  → `ResponseHeaderTimeout: 60 * time.Second`
- [internal/config/config.go:292](internal/config/config.go#L292)
  → `UpstreamTimeoutMs = 300000`（默认 5 分钟）
- [cmd/gateway/main.go:2917](cmd/gateway/main.go#L2917)
  → `ctx, cancel := context.WithTimeout(r.Context(), nonStreamTimeout(snap, cfg))`

**机制**

非流式上游是「**生成完才发响应头**」。所以一次非流式调用里，
响应头到达的时刻 = 整个生成完成的时刻。此时**先到的是 transport 的
`ResponseHeaderTimeout`（60s），不是网关自己的 ctx（300s）**。

**实跑验证**（用真实的 `upstreamHTTPClient`，上游把响应头拖到 65s，ctx 给 300s）：

```
elapsed=1m0s err=... net/http: timeout awaiting response headers
CONFIRMED: 头超时先到（1m0s），不是 ctx 的 300s
```

同时验证正常路径无恙（上游 8s 延迟 → 成功，耗时 8.002s）。

**后果**

1. **配置的超时是个谎言**：设置页把「上游超时」调成 300s（乃至 600s），
   实际天花板永远是 60s。运维按 300s 推理的容量与故障转移预算全错。
2. **症状是错的**：超时报的是 `net/http: timeout awaiting response headers`，
   经 `outwire.MapUpstreamError` 归类为 `upstream_timeout` →
   HTTP 504 "upstream connection failed"。
   但上游**没有连接失败**——它正在正常生成，只是慢。
3. **放大成重复计费**：`FailoverEligible` 对 `*rosetta.TransportError` 返回 true
   （[internal/outwire/errors.go:306](internal/outwire/errors.go#L306)），
   所以这次失败**可转移** → 故障转移链换下一个目标再发一次。
   SDK 侧的 `NoIdempotencyKey` 只关掉 SDK **自己**的重试
   （[provider_openai_chat.go:399](C:/Users/louis/Desktop/project/rosetta/provider_openai_chat.go#L399)
   把策略降为 `RetryDefault`，POST 不重试），
   **管不住网关自己的故障转移** ——
   于是一个「慢但健康」的上游被掐断后，换目标重发，
   上游侧可能已经算过费了。
4. **注释自己就过期了**：[upstream.go:1266](internal/upstream/upstream.go#L1266) 写
   「网关非流式超时默认 120s」——那是 2026-10-13 之前的值，
   已被 [c18e1be](internal/config/config.go) 改成 300s，注释没跟。

**与总预算的交互**：[cmd/gateway/main.go:2573](cmd/gateway/main.go#L2573) 的
`responseHeaderWait = 60 * time.Second` 是这个值的**镜像**
（注释明说「两处必须同步改」）—— 所以总预算是按 60s 算的，
自洽但同样建立在那个过时的 120s 前提上。

**修法（择一或组合）**

- 把 `ResponseHeaderTimeout` 调成 ≥ 配置的非流式超时（或干脆去掉，
  交给 ctx 与首字看门狗管）；
- 或让「非流式超时」真正成为唯一约束：transport 不设头超时，
  由 `context.WithTimeout` 掐断（这样 60s/300s 的选择权回到运维手里）；
- 若保留 60s，则**必须**同时把「上游超时」配置的语义改成
  「非流式生成的实际天花板是 60s」，并在界面上说清楚。

**顺带**：`max_retries` 的 UI 提示写「仅作用于**幂等**请求」
（[ProviderDetail.vue:920](web/src/views/ProviderDetail.vue#L920)），
而故障转移对 POST 是**可转移**的 —— 两处对「谁负责重试」的口径需要对齐。

### 🟡 P2-8　代理功能在部署文档里没有位置

- [DOCKER.md](DOCKER.md) 全文**没有一处**提到代理 / proxy / PROXY
- [README.md](README.md) 只有一条 CONTRIBUTING.md 的引用提到"代理"（那是 git 走代理，与本功能无关）
- [DESIGN.md:1443](DESIGN.md#L1443) 只有一行表格说明 `/networks` 端点

也就是说：容器部署（[docker/docker-entrypoint.sh](docker/docker-entrypoint.sh)、
[Dockerfile](Dockerfile)）里**没有任何关于"如何在容器里让网关走代理"的说明**，
而这恰恰是代理功能最需要文档的场景（容器里没有系统代理设置，
`HTTP_PROXY` 得显式传 env，而 env 只影响"直连"路径、不影响网络路径）。

**建议**：DOCKER.md 补一节「出站网络与代理」，说明
(a) 容器内怎么配代理（管理面建网络 or `-e HTTPS_PROXY=`）、
(b) 二者的区别（前者是 per-provider、后者是全局 env）、
(c) 网络路径**不读** env 代理这件事。

### 🟠 P2-9　代理地址里的口令会进日志（实测确认）

**位置**：[internal/upstream/upstream.go:1345](internal/upstream/upstream.go#L1345)

```go
return upstreamHTTPClient, fmt.Errorf("网络 %q 的代理地址不合法（支持 http/https/socks5，如 http://127.0.0.1:7897）", proxyURL)
```

整个 URL（含 userinfo）被原样拼进错误信息，而该错误会：

1. 被 [upstream.go:194](internal/upstream/upstream.go#L194) / [upstream.go:391](internal/upstream/upstream.go#L391)
   以 `logger.Warn("provider network proxy_url invalid...", "error", prov.ProxyConfigError)`
   **原样打进网关日志**；
2. 出现在网关的日志采集/告警系统里。

**实跑验证**（输入 `ftp://admin:hunter2@proxy.corp:3128`）：

```
错误信息: 网络 "ftp://admin:hunter2@proxy.corp:3128" 的代理地址不合法（支持 http/https/socks5，如 http://127.0.0.1:7897）
LEAK: 错误信息里含代理口令明文
```

**可达性**：admin 写入路径有 `validateProxyURL` 挡着（[network_handler.go:144](internal/admin/network_handler.go#L144)），
但它**允许 userinfo**（`http://user:pass@host:port` 是合法代理地址），
且**不校验 scheme 之外的内容**。所以「scheme 写错 + 带口令」这一组合完全可达：
管理员把 `socks5://` 打成 `socks5h://` 之外的东西、或从别处粘贴时打错 scheme，
口令就进日志了。

**对照**：本项目对**上游 API Key** 的处理非常克制
（[outwire/errors.go:139](internal/outwire/errors.go#L139) 专门复制了 SDK 的掩码正则做脱敏），
代理口令这条路径是同一类问题里被漏掉的一处。

**修法**：错误信息只带 scheme 与 host，userinfo 换成 `***`；
或复用 SDK 的 `httpx.MaskSecrets`（网关 import 不到，需要复制那份正则 ——
[outwire/errors.go](internal/outwire/errors.go) 里已经有一份）。

**顺带**：`GET /admin/api/networks` 与 `GET /admin/api/providers` 会把
**完整的** `proxy_url`（含 userinfo）回给前端
（[network_handler.go:42](internal/admin/network_handler.go#L42)、
[provider_handler.go:575](internal/admin/provider_handler.go#L575)），
且导出文件也原样带上（[config_transfer_handler.go:136](internal/admin/config_transfer_handler.go#L136)）。
这些都在 admin 权限内，可接受；但**日志**那条不是「谁能看到」的问题 ——
日志的读者范围通常比管理员大得多。

### 🟠 P1-10　导入配置时，同名但不同地址的网络会静默错配（实测确认）

**位置**：[internal/admin/config_transfer_handler.go:263-300](internal/admin/config_transfer_handler.go#L263-L300)

```go
for _, n := range existingNetworks {
    networkByName[n.Name] = n.ID     // 只按名字建映射
}
for _, ne := range in.Networks {
    name := strings.TrimSpace(ne.Name)
    if _, ok := networkByName[name]; ok {
        continue                      // ← 名字已存在就直接复用，不比地址
    }
    ...
}
```

**实跑验证**：本库已有「公司代理」→ `http://old-proxy.corp:3128`；
导入文件里同名网络是 `http://new-proxy.corp:8080`：

```
provider alpha network_id="net-old" proxy_url="http://old-proxy.corp:3128"
IMPORT MISMATCH: 导入后走的是 "http://old-proxy.corp:3128"，文件里写的是 "http://new-proxy.corp:8080"
warnings=[] notes=[]
```

**零告警**。注释里的理由是「导出的地址多半与本库一致」—— 在**同库备份还原**
的场景下成立，但导入的典型用途恰恰是**跨库迁移**（[README/DESIGN](DESIGN.md) 里
导出格式本身就把「跨库 id 无意义、靠名字对齐」写在设计里），
而跨库时"同名不同址"是常态（两台机器的公司代理端口不同、域名不同）。

**后果**：导入成功、界面显示「经网络『公司代理』转发」，
实际走的是**本库那条旧地址**。这与本文件 P1-3（bootstrap 不校验 proxy_url）
是同一类缺陷的另一面：**配置写了 A，运行时用了 B，且没有任何提示**。

**对比**：同一个函数对「引用的网络名在文件里找不到」是**有告警**的
（[config_transfer_handler.go:372](internal/admin/config_transfer_handler.go#L372)），
说明"必须说出来"这条纪律在这里是明确的 —— 只是"同名不同址"这一格漏了。

**修法**：复用前比对地址，不一致时按「新建一条改名的网络」处理
（与 provider 的 slug 冲突处理同一套做法：`uniqueSlug` → 这里用 `uniqueNetworkName`），
并记一条 warning 说明"已改名，请核对地址"。

---

## 2. 修复优先级建议

1. **P0-1 / P0-2**（同一次改动可覆盖）：抽一个按 provider 派生 client 的公共入口，
   保留两个旁路各自的"不跟随重定向"纪律。既是功能缺陷，也是凭据出口问题。
2. **P0-4**（新增）：出站头超时与非流式超时的矛盾。改动小、影响面大，
   但要连带改 `responseHeaderWait` 镜像值与相关注释。
2. **P1-3**：`validateProxyURL` 提到共用包，config 与 admin 共用。
3. **P2-4**：回填改成 `WHERE TRIM(proxy_url) = ?`。
4. **P2-5**：三处文案统一（或让直连真直连）。
5. **P3-6 / P3-7**：删死代码；proxy client 缓存加淘汰；`ProxyConfigError` 要么暴露到管理面，要么把注释改对。

---

## 3. 审计过程中的验证手段（可复用）

- 假代理 + 假上游双计数器，断言"代理收到几次 / 上游直连收到几次" —— 判定"流量是否真的拐弯"的唯一可靠办法
- 最小 SOCKS5 服务器（greeting → CONNECT → relay），用于验证 socks5/socks5h 通路
- 老库裸 SQL 造数据 → `store.Open` 跑完整迁移链 → 断言派生值，用于验证迁移的静默丢失
- 两条 Transport 参数逐项比对（MaxIdleConns / PerHost / IdleConnTimeout / h2 / ResponseHeaderTimeout / TLSHandshakeTimeout / DialContext 有无），
  保证"走代理的"与"直连的"行为差异不会来自池参数
