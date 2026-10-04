# 审计与修复记录 — 2026-09-21

本轮针对「多阶段开发累积的不一致」做了一次全面审计。触发点是局长报的一个具体故障：
**访问管理后台，输入密码 test123 后又要求输入，一直重复。**

§3.11 是后半天追加的：局长另一个项目（leans）通过本网关调模型时报
「AI 调用失败: 流式响应中没有内容」，定位下来根因在本网关，见该节。

---

## 0. 结论速览

| # | 问题 | 严重度 | 状态 |
|---|---|---|---|
| 1 | 后台设完密码必须重启才生效，表现为「输入密码 → 又要求输入」死循环 | **P0** | 已修复 |
| 2 | `POST /admin/api/password/set` 完全免鉴权，任何人可劫持管理员密码 | **P0** | 已修复 |
| 3 | 前端「输入密码」从不校验，存进 localStorage 就刷新 | **P0** | 已修复 |
| 4 | 改完密码不更新本地令牌，等于把自己锁出门 | **P1** | 已修复 |
| 5 | 密码用无盐 SHA-256，且靠 `len()==64` 猜明文/哈希 | **P1** | 已修复 |
| 6 | 全部凭据权重为 0 时 `rand.Intn(0)` panic，可打挂网关 | **P1** | 已修复 |
| 7 | 重建上游池时不清空 map，已删除的上游永久残留 | **P1** | 已修复 |
| 8 | `config.go` 注释与 `server.go` 实现语义相反 | **P1** | 已修复 |
| 9 | PATCH 一律「非空才覆盖」，`0` / 空串永远写不进库 | **P1** | 已修复（§3.7） |
| 10 | 自动生成的默认配置绑 `0.0.0.0` + 无凭据 → 局域网可抢先设管理员密码 | **P1** | 已修复（§3.8） |
| 11 | 凭据冷却机制未接线，失效凭据永不被摘除 | **P1** | 已修复（v1.2.0，§4.1） |
| 12 | 路由 `priority` 在解析时完全没被使用，同名路由互相覆盖 | **P1** | 已修复（v1.2.0，§4.2）：**摘掉字段**（`public_name` 是 UNIQUE，「同名择优」在数据层不可能成立；多上游由 `route_targets` 链承担） |
| 13 | 两处 `extra_json` 与 `routes.fallback_route_id` 存了但从不使用 | **P2** | 已修复（v1.2.0）：兜底改由 `route_targets` 有序链实现、UI 不再暴露假开关；两处 `extra_json` 连同列一并摘除（§4.3） |
| 14 | `access_keys.quota_tokens` 既无写路径也无校验（假配额字段） | **P2** | 已修复（v1.2.0，§4.4）：写路径 + 请求前预检强制 |
| 15 | 若干死代码与契约瑕疵 | **P2** | 部分修复（§4.5） |
| 16 | 主密钥只认环境变量：**双击 exe 启动即无密钥，API Key 明文落库** | **P1** | 已修复（§3.9） |
| 17 | 容器默认端口 6666 是**浏览器保留端口**，部署后管理界面永远打不开 | **P1** | 已修复（§3.10） |
| 18 | 空闲看门狗超时被当成正常收尾：下游收到假的 `finish_reason:stop` + `[DONE]` | **P1** | 已修复（§3.11） |
| 19 | `EventThinkingDelta` 被静默丢弃：只吐思考的上游在下游变成**零内容**的流 | **P1** | 已修复（§3.11） |

---

## 1. 环境事实（先厘清，否则会查错对象）

排查中发现：**本机同时存在两份网关，端口都是 18080。**

| 位置 | 用途 | 状态 |
|---|---|---|
| `C:\Users\louis\Desktop\gateway\gateway.exe` | 局长实际访问的部署副本 | 已换成本轮新构建并复验 |
| `C:\Users\louis\Desktop\project\rosetta-gateway\bin\gateway.exe` | 开发仓库构建产物 | 未运行 |

桌面副本的 `config.json` 里 `admin_token` 是
`ecd71870d1963316a97e3ac3408c9835ad8cf0f3c1bc703527c30265534f75ae`，
经核对**恰好等于 `sha256("test123")`** —— 也就是说 **test123 确实是当时有效的密码，后端校验没有问题**。

这一点很重要：**故障不在「密码是什么」，而在「设置密码的流程本身是断的」**。
（用 curl 实测也确认：桌面副本对 `Bearer test123` 返回 200，对 `Bearer devtoken` 返回 401。
仓库 `bin/config.json` 里写的 `devtoken` 属于另一个环境，与局长看到的现象无关。）

> 两处都监听同一端口，同时启动只有一个能抢到。排障时**先确认进程路径**再对照配置文件，
> 否则会像我一开始那样对着错误的配置查半天。

---

## 2. 根因链：为什么会「输入密码后又让输入」

旧实现的完整因果链，每一环都已核对代码：

1. **语义自相矛盾。** `internal/config/config.go` 的注释写「admin_token 留空 = 后台免鉴权，首次启动即可进面板」，
   而 `internal/server/server.go` 的实现是 `if adminToken == "" { 401 }` —— **注释与代码完全相反**。
2. **新部署落在夹缝里。** 配置文件不存在时程序自动生成默认配置，其 `admin_token` 为空。
   于是系统进入一个「既没有任何凭据可用、又拒绝所有请求」的状态。
3. **前端据此显示「设置管理密码」**（`/password/check` 返回 `has_password=false`），这是合理的第一步。
4. **用户输入密码 → `POST /admin/api/password/set`。** 该端点被中间件**无条件白名单放行**，于是写盘成功。
5. **但内存不更新。** `cmd/gateway/main.go` 里 `adminToken := cfg.AdminToken` 是**启动期的值拷贝**，
   作为 `string` 传给中间件闭包。`password_handler.go` 只改 `config.json` 文件，**进程内的值永不变化**。
6. **于是新密码在重启前永远无效。** 前端把密码存进 localStorage 后 reload，
   新请求仍被 401 拒绝 → 弹框再次出现。
7. **对话框是 `dismissable=false` 的**（无法关闭），用户被永久困在循环里。

叠加一个独立缺陷：**前端 `submitToken` 根本不校验输入** ——
`saveToken(pwd)` → `needToken=false` → `window.location.reload()`，
「存下来就刷新」。哪怕用户输错了，也会被 401 弹回同一个框，且错误的 token 已经写进了 localStorage。

**一句话总结**：写盘与内存各行其是，前端又把「输入」当成了「凭据」。

---

## 3. 已修复内容

### 3.1 新增 `internal/adminauth` 包（P0）

凭据的加载、校验、更新集中到此包，**带读写锁，是唯一事实来源**。
`Set()` 成功即生效，无需重启 —— 这一条直接掐断死循环的第 5~6 环。

**存储位置：可执行文件同级的 `admin_auth.json`。**

为什么不进数据库：`gateway.db` 在本项目里是「可丢弃的运行时数据」，删库重建是常规操作
（凭据加密密钥丢失时就是这么处理的）。管理员密码是身份凭据，放进会被随手删掉的文件里，
等于**删库 = 把自己锁在门外**。与 `master.key` 独立于库的理由一致。

为什么不塞进 `config.json`：那是运维手写的引导配置，程序回写会丢注释与字段顺序；
而且 `admin_token` 是「运维引导令牌」，与「用户设置的管理密码」语义不同。
混用正是旧实现被迫用 `len(token)==64` 猜类型的根源。

**哈希**：PBKDF2-HMAC-SHA256，16 字节随机盐，21 万次迭代，32 字节输出，
存 base64。替代原来的裸 `sha256(password)`（无盐、可彩虹表、可 GPU 秒破）。

**原子落盘**：先写 `.tmp` 再 `rename`。直接覆写若中途崩溃会留下半截 JSON，
下次启动直接拒绝服务 —— 那是把「改密码失败」升级成「彻底进不去」。

**兼容与恢复**：用户密码优先；未设置时回退到 `config.json` 的 `admin_token`
（同时兼容历史写入的裸 sha256 hex 形态）。忘了密码时删掉 `admin_auth.json` 重启即可恢复。

### 3.2 鉴权中间件重写（P0）

`server.AdminAuth` 改为接受接口 `AdminCredentials`（由 `adminauth.Store` 实现），
不再接收一个启动期固定的字符串。

放行规则：

| 端点 | 规则 |
|---|---|
| `GET /admin/api/password/check` | 恒放行（只返回布尔值，不含可用于登录的信息） |
| `POST /admin/api/password/set` | **仅当系统尚无任何凭据时**放行；已有凭据则必须带正确的旧凭据 |
| `GET /admin/api/auth/verify` | 需鉴权；专门给前端「先验证再保存」用 |
| 其余 | 一律要求 `Authorization: Bearer <密码或 admin_token>` |

**这是本轮最重要的安全修复**：旧实现把 `password/set` 无条件白名单放行，
意味着**局域网内任何人都能直接覆盖管理员密码，把局长锁在门外**。
修复后无凭据调用返回 401（已验证）。

比较一律走 `crypto/subtle.ConstantTimeCompare`，避免通过响应时间逐字节爆破。

### 3.3 前端：先验证，再保存（P0）

`web/src/App.vue` 重写凭据流程：

- `login()` 先 `GET /admin/api/auth/verify`，**通过才写 localStorage**；
- 失败时显示「密码错误，请重新输入」，**不再静默刷新**；
- 首次设置走 `setupPassword()`，成功后立即用新密码作为令牌（后端已即时生效）；
- 新增 `authError` 状态与 `.auth-error` 样式（用 `--heat` 暖橙，红只留给破坏性操作）；
- 提交期间按钮进入 `验证中…` 禁用态，避免重复提交。

`web/src/views/Settings.vue` 的改密码流程同步修正：
旧代码只在「本地没有 token」时才写入新密码，
存在 token 时**改完密码旧令牌立刻失效而本地没换** → 下次请求 401，等于自己把自己锁出去。
现在无条件 `saveToken(newPassword)`。

### 3.4 上游池重建（P1）

`BuildFromConfig` / `BuildFromStore` 在重建前**不再复用旧的 map**。
原来只赋值不清空，导致从管理界面删除上游后，**池里仍然留着它**，
`/v1` 还能把请求转发到已删除的 provider（快照已删、池里还在）。而 admin 的每个写操作都会触发 reload。

### 3.5 加权选择 panic（P1）

`selectWeighted` 在总权重为 0 时执行 `rand.Intn(0)` → **直接 panic**。
这是可达状态：后台把每条凭据的权重都改成 0 即可。现退化为均匀随机。

### 3.6 数据目录（局长要求）

`db_path` 相对路径按**可执行文件所在目录**解析（`main.go` 已有 `filepath.Join(exeDir, cfg.DBPath)`），
默认值为 `./data/gateway.db`，即 **`<exeDir>/data/gateway.db`**。
`config.example.json` 与 `DESIGN.md` 同步为这一约定，完整目录布局见 `DESIGN.md` §12.1.1。

### 3.7 PATCH 改为字段级部分更新（P1，本轮第二个重点）

**旧语义**：`if req.X != ""` / `if req.X != 0` 判断是否更新 → **空串和 0 永远写不进库**。
前端为绕开它，被迫在编辑表单里回传完整字段（`api.ts` 里专门写了注释警告这件事）。

**新语义**：请求结构体的标量字段一律改成指针。

| 传法 | 含义 |
|---|---|
| 字段不出现（或 `null`） | 未提供 → 保持数据库原值 |
| 字段出现 | 显式赋新值，**空串 / 0 都是合法值**，会真正落库（空串落 NULL） |
| 必填字段显式传空串 | `400`，**不再静默忽略**（静默忽略会让人以为改成功了） |

**改动范围**

| 文件 | 关键变化 |
|---|---|
| `admin/helpers.go` | 新增 `derefStr` / `derefInt`，并写明「Create 用 deref，Update 必须用 `!= nil`」 |
| `admin/key_handler.go` | `Name` 改指针；显式空串 → 400 |
| `admin/provider_handler.go` | **拆成 `providerCreateRequest` / `providerUpdateRequest`**：create 路径的 slug/enabled/timeout/retries 由后端决定，与 PATCH 字段集本就不同；update 结构体**刻意不含 slug**，因为 slug 是对外引用的稳定标识，创建后不可改 |
| `admin/route_handler.go` | 全部字段改指针；`priority=0`、`fallback_route_id=""` 现在能真正写入 |
| `admin/model_handler.go` | `context_window` / `max_output_tokens` 传 0 → 落 NULL（「未设置」）；`display_name` 可清空 |
| `admin/credential_handler.go` | `label` 可清空；`api_key` 显式空串 → 400（空密钥的凭据毫无意义，要弃用请删除或置 `enabled=false`）；`weight < 1` → 400 |
| `web/src/api.ts` | 契约注释重写；新增 `ProviderUpdatePayload`（不含 slug） |
| `web/src/views/Providers.vue` | 停止回传完整字段；切换启用状态时只发 `{enabled}` |

**顺带修掉的三个隐性缺陷**

1. 所有字符串入参补齐 `TrimSpace`。原来上游/模型/密钥名带首尾空格会被原样存库，
   之后按名字匹配（如路由解析）就永远对不上。凭据 `api_key` 也裁空白 ——
   粘贴时极易带上换行，在 HTTP 头里非法。
2. 负数入参统一拒绝（`timeout_ms` / `max_retries` / `priority` / `context_window` / `max_output_tokens`）。
   原来负数会被直接落库，等于把「不可能的值」写进配置。
3. DB 层的 `nullIfEmpty` / `nullIfZeroInt` 本来就在，但**因为 handler 根本不肯传 0/空串，永远走不到**
   —— 这也解释了为什么「清空」在界面上从来没生效过。

**注意**：前端不再需要「必须发送完整字段」这条纪律了，只发要改的字段即可，
`api.ts` 顶部注释已同步。`web/src/views/Providers.vue` 里编辑上游的分支仍回传多数字段，
那是因为 UI 上这些字段确实可编辑，不是契约要求。

### 3.8 默认配置只监听回环（P1）

**发现过程**：本地起测试实例时我误删了测试配置文件，程序自动生成了默认配置并监听
`0.0.0.0:8080` —— 日志里同时打出
`no admin credential configured; the admin UI will ask you to set a password on first visit`。

组合起来就是一个真实风险：**默认绑定全部网卡 + 首次启动无任何凭据
= 局域网里第一个访问 `/admin/` 的人可以抢先设置管理员密码，把部署者锁在门外。**

修复：`setDefaults()` 的 `Listen` 默认值改为 `127.0.0.1:8080`。
要对外提供服务就显式改成 `0.0.0.0:<port>`（启动日志会打印实际监听地址）。

`config.example.json` 与 `DESIGN.md` §12.2 的示例同步改为 `127.0.0.1:8080`，
并在文档里写明「要对外服务就显式改成 `0.0.0.0:<port>`」。
示例是用户会直接照抄的东西，让示例与默认值保持一致、都站在安全那一侧。

同时修掉 `Config.Default()` 上方那句错误注释（原文「admin_token 留空 = 后台免鉴权」）。
空 `admin_token` 的正确语义是「等待首次设置密码」，不是免鉴权。

### 3.9 主密钥落盘：让「双击启动」和「脚本启动」共用一把密钥（P1）

**现场证据（局长机器实测）**：桌面副本的 `config.json` 是 `"master_key_env": "ROSETTA_GW_MASTER_KEY"`，
而进程是**从资源管理器双击启动**的（`Win32_Process` 的父进程 = `explorer.exe`），
环境里没有这个变量 → 日志打 `master key unavailable, credential encryption disabled`。
查库直接坐实后果：

```
provider_credentials.api_key_enc
  default   51 字节  可打印文本=True  前缀=b'sk-jBBPM'
  default   35 字节  可打印文本=True  前缀=b'sk-eAMo8'
```

**API Key 以明文躺在 SQLite 里。**任何拿到 `gateway.db` 的人直接读走全部上游密钥。

根因不是「忘了设环境变量」，是**密钥来源本身分裂**：
`internal/crypto` 只认环境变量；`bin/master.key` 那套（自动生成 + 复用）**完全活在 `gateway.ps1` 里**。
于是同一个部署，脚本启动有密钥、双击启动没密钥 —— 两把不同状态，
既泄露明文，又会让「脚本加密、双击解密」互相解不开。

修复：把密钥解析下沉到 Go 侧，优先级 **环境变量 → `<exeDir>/master.key` → 自动生成并写入**。
与 `admin_auth.json` 同一个道理：**加密身份独立于业务数据，且跟着 exe 走**。
生成时打日志给出路径，避免「悄悄换了一把钥匙」这种更难查的故障。

配套加了解密兜底 `crypto.DecryptWithFallback`：**主密钥存在但解不开时，
只有数据看起来是可打印文本才按明文返回**（兼容本轮之前落的明文数据），
密文形态仍然报错 —— 否则一旦补上密钥，历史明文凭据会永久失效，
还会把乱码当 API Key 发给上游，错误信息变成难以定位的 401。

顺带清掉 `snapshot.RebuildFromDB` 的两个死参数（`masterKey` / `logger`，
函数体内只有 `_ = masterKey` / `_ = logger`）。签名改为 `RebuildFromDB(ctx, st, pool)`，
调用点 2 处同步。它本来就不该持有主密钥 —— 解密发生在上游池的 `BuildFromStore` 里。

**注意（风险提示，已写入 DESIGN.md）**：密钥来源一旦确定就不要中途改。
若先用双击（密钥落在 `<exeDir>/master.key`）、后来改成设环境变量启动，
两把密钥不同 → 先前加密的凭据解不开。要么一直双击，要么一直用脚本。
`gateway.ps1` 写的正是 `bin/master.key`，与 Go 侧路径一致，所以两条路天然对齐。

### 3.10 容器默认端口 6666 换成 8666（P1，v1.1.1）

**症状**：镜像部署到局域网后浏览器打不开，容器日志却完全正常
（`listen 0.0.0.0:6666`、`server starting`）。

**根因**：`6666` 在 Chromium 的保留端口表里（`net/base/port_util.cc` 的
`kRestrictedPorts`，IRC 段 6665–6669 / 6697）。Chrome / Edge / Brave / Opera
一律硬拦，Firefox 也拦。**拦截发生在浏览器内部，请求根本不会发出去**，
所以服务端没有任何连接日志。

这就解释了为什么分层自测会得到「很矛盾」的结果：

| 测法 | 结果 |
|---|---|
| 容器内 `wget 127.0.0.1:6666` | 通 |
| 宿主机 `curl.exe 127.0.0.1:6666` | 通 |
| 浏览器 | `ERR_UNSAFE_PORT` |

**`--explicitly-allowed-ports` / 组策略 `ExplicitlyAllowedNetworkPorts` 不是解药** ——
只放开本机浏览器，其他访问者照样打不开。

**修复**：

- `Dockerfile` 的 `EXPOSE`、`docker/config.default.json` 的 `listen`、
  入口脚本的提示语、`DOCKER.md` 全部改为 **8666**（在黑名单内最接近 6666 的取值）。
- **新增 `internal/config/ports.go`**：内置一份完整的 Chromium 保留端口表 +
  `CheckListenPort(listen)`。启动时在 `main.go` 里比对，命中就 `logger.Warn`
  并打出 `reserved_for` / `browser_error` / 建议值。
  放在 Go 里而不是入口脚本里，是为了**裸机部署（`bin\config.json`）也吃得到这个保护**。
- `DOCKER.md` 新增「端口为什么是 8666」整节 + FAQ 条目；
  并补上「`config.json` 生成后不随镜像更新」的提醒（换端口后老配置不会自动改）。

**注意**：这是警告不是致命错误 —— 端口在非浏览器场景（纯 API 调用、反向代理前置）
仍然可用，没必要拒绝启动。

已验证（隔离环境 `.workbuddy/tmp/e2e-port`，见 §5.6）。

---

### 3.11 流式空响应：看门狗超时被伪装成正常收尾 + 思考增量被丢弃（P1，v1.1.2）

**症状**：局长另一个项目（leans）经本网关（部署在 `192.168.4.162:8666`，v1.1.1）调模型时
报 **「AI 调用失败: 流式响应中没有内容」**，随机出现、无法复现。直连上游各模型
（6+3+8+3 次）全部正常，所以最初怀疑是模型 API 提供商。

**定位结论**：两处都在本网关，且都会让下游收到一个**看起来成功**的空流。

#### (a) 空闲看门狗超时不留痕 → 状态仍是 `ok`

```go
idleTimer := time.AfterFunc(idleTimeout, func() {
    logger.Warn("stream idle timeout", ...)
    stream.Close()          // ← 只关流，没置任何状态
})
...
if err := stream.Err(); err != nil { ... }   // ← Close() 不写 s.err，恒 nil
if status == "ok" { sse.WriteFinish(...) }   // ← 于是照常写 finish_reason:"stop"
if status == "ok" { sse.WriteDone() }        // ← 并且照常写 [DONE]
```

**根因是跨包契约误判**：`streamCore.Close()` 只置 `done` 并释放连接，
**不写 `s.err`**（Rosetta 的设计是「干净结束必须 `Err()==nil`，哪怕关连接失败」）。
所以看门狗掐断的流在 SDK 视角里跟正常结束**完全一样**，网关不自己记一笔就无从区分。

这直接违反本项目自己的设计文档：
`DESIGN.md` §8.1「超时视为 `status=truncated`」与 §16-P0 验收第 5 条
「上游卡住不吐字节，60s 后看门狗关闭流，下游收到断流」—— 两条都没做到。

**隐蔽伤害**：`usage_records.status` 也记成 `ok`，所以事后从网关后台的调用历史里
**也查不出来**（实测记录是 `status=ok, latency_ms=3001, ttfb_ms=0, 0 tokens`）。

#### (b) `EventThinkingDelta` 是空实现

```go
case rosetta.EventThinkingDelta:     // ← 直接丢掉
```

上游只发 `reasoning_content` 的流（思考型模型把 `max_tokens` 耗在思考期时正是这种形态）
到下游就变成**完全空**的流，且照样以 `finish_reason:"stop"` + `[DONE]` 收尾。
旁证：`qwen` 首字延迟 15.3s，`agens` 1.5s / `dsv41` 1.3s —— `qwen` 是思考型，
思考阶段对下游完全不可见。

#### 为什么两处都要修，而不是「下游少报点错就行」

两类问题有一个共同的病理：**网关把一个异常包装成了正常收尾**。
下游（leans 侧 `backend/service/ai.go` 只统计 `EventTextDelta`）拿到
「HTTP 200 + 零文本增量 + `finish_reason:stop` + `[DONE]`」，
在协议层面这是一次**成功的空回复**，它除了报「没有内容」无话可说。
错误信息模糊是症状，网关撒谎才是病因。

#### 修复

`cmd/gateway/main.go`：

- 看门狗回调加 `var idleTimedOut atomic.Bool`，开火时置位（`Close()` 释放的局部变量
  不能直接写，必须走 atomic）。
- 循环后补第二类终态：`else if idleTimedOut.Load() && !sawTerminal` → `status="truncated"`，
  从而落进既有的「非 `ok` 不写 `finish_reason`、不写 `[DONE]`」分支。
- `sawTerminal`（收到过 `EventMessageEnd`）是**防御性条件**：实测 Rosetta v0.5.1 在
  `[DONE]`/EOF 之后就短路 `next()`，适配器不会在吐出 `message_end` 后继续阻塞，
  所以这条判定当前**打不到**；留着守「适配器将来在终止事件之后仍等待更多数据」。
- `EventThinkingDelta` 改为透传；同时记 `wroteContent`，在「上游给了终止事件却
  一个增量都没写」时补一条 `WARN stream finished with no content`
  —— 不替上游改协议语义（合法的空回复确实存在），但日志必须能区分
  「上游真空」与「网关吃掉」。

`internal/outwire/openai_chat.go`：新增 `WriteThinkingDelta` → `delta.reasoning_content`；
非流式的 `message.reasoning_content` 同步补上（取自 `ChatResponse.ThinkingText()`）。
键名不是自创的：DeepSeek / Qwen / vLLM / OpenRouter 一致用它，Rosetta 的 openai-chat
适配器也按这个键回读（`provider_openai_chat.go`），所以**整链路自洽可往返**。

#### 验证（隔离环境 `.workbuddy/tmp/e2e`，18099 + 19090 假上游，空闲超时调成 3000ms）

`zzfake` 新增三种模式：`silent`（只发 role 块后挂住）、`no_done_hold`
（发了 finish_reason + usage 但既不发 `[DONE]` 也不关连接）、`thinking`（只发 `reasoning_content`）。
判定看**下游实收字节**（`raw_*.sse` 留档），不看代码推断：

| 场景 | 下游收到的流 | `usage_records.status` |
|---|---|---|
| A 正常上游 | `content` + `finish_reason:"stop"` + usage + `[DONE]` | `ok` |
| **B 上游静默** | 只有 `: keepalive`，**无 finish_reason、无 `[DONE]`** | **`truncated`**（`latency_ms=3001`） |
| **C 有内容但缺 `[DONE]` 且挂住** | 内容已下发，**无 finish_reason、无 `[DONE]`** | **`truncated`**（`latency_ms=3001`） |
| **D 上游只发思考（流式）** | `reasoning_content` ×3 + `finish_reason:"stop"` + usage + `[DONE]` | `ok` |
| **E 上游只发思考（非流式）** | `message.content=""` + `message.reasoning_content="想完了，但没写正文"` | `ok` |

修前 B/C 两条的 `status` 都是 `ok`、下游都带假 `finish_reason:"stop"` + `[DONE]`；
D 的 `reasoning_content` 一行为空。日志侧同步可见两条新告警，
且 `content_written` 字段能区分「掐断时有内容」（C=true）与「一个字都没吐」（B=false）：

```
WARN stream idle timeout                           idle_timeout_ms=3000
WARN stream cut by idle watchdog without terminal event  content_written=false
WARN stream cut by idle watchdog without terminal event  content_written=true
```

`go build ./... && go vet ./...` 空输出。

**给下游（leans）的残余项**：网关现在最多只能做到「如实暴露」。`leans` 若要更精确的
报错，得把 `finish_reason` / `usage` / 是否收到过 thinking 带进错误文案
（该项目的 §5.2 已记录，不在本仓库范围）。

---

## 4. 未修复项（需要局长决策或后续排期）

以下问题都已核实，但**改动面大或涉及产品语义**，一次全改会引入回归且难以验证，
因此明确列出而不是偷偷绕过。

### 4.1 凭据冷却未接线（P1）—— 已修复（v1.2.0）

~~`upstream.MarkCredentialCooldown` / `MarkCredentialError` 全仓库无调用者。~~
**已接线**：故障转移热路径（`handleChatCompletions`，见 DESIGN §10）在写回处直接依错误分类回写凭据健康态——
`out.credCooldown>0` 时 `pool.MarkCredentialCooldown(credID, d)`（时长来自 `outwire.CredentialCooldown`：
401/403→30min、402→1h、429/408/5xx/传输层→60s）；成功命中则 `pool.RecordCredentialSuccess(credID)` 清除冷却。
`getHealthyCredentials` 会跳过 `CooldownUntil > now` 的凭据，故被冷却的失效 key 在窗口内不再被选中。
> 注（v1.2.x 后续清理）：原封装 `pool.RecordCredentialFailure` 因热路径按 attempt 结果内联回写而成了死代码，已删除；行为不变。

**修订（2026-09-24，详见 §2.5）**：原结论里「失效凭据永不摘除」只对了一半 ——
`MarkCredentialError` **就算接上也是空操作**：它只改内存里的 `status`，既不回写库（管理 API 读的是库），
也不参与健康过滤（`getHealthyCredentials` 只看 `cooling` 与 `disabled`）。而且它当时所在的那条 `else`
分支**本来就不可达**（所有可转移状态都带非零冷却，`credCooldown>0` 恒成立）。
已**删除该函数与那条死分支**：判废一把 key 的唯一真实手段是冷却；「不该罚凭据的失败」（当前只有 404/410）
的正确记账位置是目标熔断，而不是凭据状态。

### 4.2 `priority` 在路由解析时完全没被使用（P1）—— 已修复（v1.2.0）

**这是本轮新发现的**，性质比 §4.3 更严重。

管理界面写着「公开模型名 → 上游模型的映射，**按 priority 升序择优**」，
路由表单也提示「数字越小越优先」。但 `routing.RouteIndex` 里：

```go
byPublicName map[string]*Route   // 普通 map，同名直接覆盖
func (ri *RouteIndex) AddRoute(r *Route) {
    ri.byPublicName[r.PublicName] = r   // ← priority 从未参与
}
```

`Resolve()` 只做 `byPublicName[model]` 一次查表，**没有任何按 priority 排序或择优的逻辑**。

**后果**：同一个 `public_name` 配多条路由时，谁生效取决于 `AddRoute` 的**最后写入者**，
而调用方是 `for _, r := range routes`（`st.ListRoutes` 返回的切片虽有序，
但真正的不确定性在于这条路径**没有任何设计意图**）——
换句话说，**同名的多条路由谁能服务，是没有定义的行为**。

**当前为什么没爆**：两个库实测都**没有重名的 `public_name`**（各 3 条，互不相同），
所以这是潜伏问题，不是正在发生的故障。

**建议**：把 `byPublicName` 改成 `map[string][]*Route`，按 `(priority, created_at)` 排序，
`Resolve` 依次尝试，跳过 provider/模型被停用的候选。
**没有直接做**：这会改变「谁在服务流量」的行为，属于功能变更而非纯 bug 修复，需要局长确认。

**最终处置（2026-09-24，全修轮）—— 摘掉字段，而不是接上语义。**

局长选择「全修」后重新评估了上面那条建议，结论是**不采纳**，理由：

1. `routes.public_name` 带 **`UNIQUE` 约束**。也就是说「同一个 public_name 配多条路由」
   这件事在**数据层根本不可能发生** —— 想让它发生，得先拆掉 UNIQUE。
   而拆掉 UNIQUE 之后，`Resolve` 就必须处理「同名多路由」的全部歧义（哪条优先？
   部分可用怎么办？`/v1/models` 列表怎么去重？），换来的能力却只是
   「同一个名字背后有多个上游」—— 这件事已经由 `route_targets` 有序链做了，
   而且做得更对：链是**有序、显式、可故障转移、可在界面上编排**的。
2. 两个机制语义重叠，保留 `priority` 的净效果是**误导**：界面上有个可编辑、
   写着「数字越小越优先」的输入框，而它什么也不控制。这正是本次审计要消灭的那类问题。

因此：`routes.priority` 列由 `dropDeadColumns()` **真删**（不只是从建表语句里去掉，
老库启动时也会 DROP），`routing.Route` 结构体、`routeRequest`/`routeResponse`、
`web/src/types.ts` 的 `Route`、以及 `Routes.vue` 的优先级徽章与输入框一并移除。

真正的「多上游」需求请用 `route_targets`：`POST /admin/api/routes` 建 route 时同事务
种一条 position 0，之后 `PUT /admin/api/routes/{id}/targets` 整体替换链。

### 4.3 三处配置字段存了但从不使用（P2）

- ~~`routes.fallback_route_id`：兜底路由是个假开关。~~ **已修复（v1.2.0）——故障转移已落地，但换了一种形态**：
  自动兜底不再走 `fallback_route_id` 单跳，而是由新表 `route_targets` 承载的**有序上游链**实现
  （对外同一个 `public_name` 挂多个 `(provider, model)`，按 `position` 升序、满足 pre-commit 条件时自动转移，详见 DESIGN §10）。
  `fallback_route_id` 就此退化为**历史兼容列**：`Routes.vue` 已移除「兜底路由」下拉，不再暴露给用户；
  列本身保留在 schema 中（避免破坏性迁移），但无写路径、`Resolve()` 亦不读取。
  即「假开关」的问题根因（给了用户一个不工作的入口）已消除——界面不再呈现它。
- ~~`routes.extra_json`：仍未接线。~~ **已修（v1.2.0，全修轮）——摘掉。**
- ~~`upstream_models.default_extra_json`：仍未接线。~~ **已修（v1.2.0，全修轮）——摘掉。**

**先更正本条审计的一处事实**：「后台那个『透传参数』输入框填了不生效」在当前版本**已不存在** ——
逐字 `grep` 过 `web/src`，`extra_json` 只出现在 `types.ts` 的类型声明里，没有任何 view 渲染它。
所以假开关的**可见**症状早在前几轮就被消掉了，剩下的纯粹是 API 层「接受写入、写入无效果」。

**最终处置：摘掉，而不是接线。** 理由（与 §4.2 的 `priority` 同一逻辑）：

1. 两个字段都没有消费者。要接线，必须先在 `ToRosetta()` 之后把值合并进 `ChatRequest.Extra`，
   而这一步的前提是**语义先定清楚**，以下几件事目前都是未决的：
   - **链可跨协议**：一条 route 的 `route_targets` 可以同时挂 `openai-chat` 与 `anthropic` 的上游，
     所以「路由级 extra 的保留键校验」在**写入时没法一次做完** —— 而 SDK 对保留键的处理是
     `ErrInvalidRequest`，校验漏一次就是让整条路由的**每个请求**都 400。
   - **优先级未定**：路由级与模型级同时存在时谁覆盖谁？合并顺序写反了会静默改行为。
   - **热路径成本**：每请求解一次 JSON，还是解析后缓进快照？缓存就要处理「改了配置但快照没 reload」。
2. 在没有测试覆盖的前提下塞进一个「修复轮」，产出的是一个语义不明、只有作者知道怎么用的开关 ——
   那不叫接线，叫把「静默不生效」换成「静默生效但没人说得清规则」。

**留下的钩子**：真要这个能力（例如给某个 vLLM 模型固定加 `top_k`），它是个小而清楚的功能轮，
加起来是一个 `ALTER TABLE ADD COLUMN` + 快照字段 + 合并点 + 保留键门控 + 用例，
目录和命名已由 `ApplyProtocolPrivateExtra` 铺好。届时优先做**模型级**（协议唯一、可在写入时校验），
路由级留到有跨协议方案之后。

### 4.4 `access_keys.quota_tokens` 是假配额（P2）—— 已修复（v1.2.0）

~~「能读、不能写、不生效」的三无字段。~~ 本轮按 §4.4 的建议**把写入口和强制逻辑一并补上**（而非摘掉字段）：

- **写路径**：`CreateAccessKey` / `UpdateAccessKey` 均落 `quota_tokens`（PATCH 语义 `*int64`，可显式改回 0=不限；负数在 handler 层挡 400）；
  前端 `Keys.vue` 新建/编辑表单加了「Token 配额」数字输入，列表行显示 `用量 used/quota`（超限打「配额已用尽」徽章）。
- **强制**：`handleChatCompletions` 在鉴权后、解析前调 `store.GetKeyQuota` 读库预检，`quota>0 且 used>=quota` → `429 insufficient_quota`。
  `used_tokens` 由 `usage_records` 触发器实时累加，故读库即权威值（不读滞后快照）。
- **语义**：终身累计、不自动重置、0=不限；并发容忍至多一个在途请求超发（pre-check + post-deduct，DESIGN §11.2）；读库出错 fail-open。

**测试**：`store` 写/读+触发器、`admin` 写路径+负数 400、`cmd/gateway` 超限 429 与 quota=0 放行——均已覆盖并通过。

### 4.5 死代码与契约瑕疵（P2）

| 位置 | 问题 |
|---|---|
| `snapshot/rebuild.go` | `decryptKey` 从未被调用；`RebuildFromDB` 收下 `pool`/`masterKey`/`logger` 却不用（靠 `_ =` 消音），池的重建散落在 `ReloadHandler` 里 —— 职责割裂，建议把池重建收进该函数 |
| `upstream/upstream.go` | `GetClientForCredential` / `GetProvider` / `ListProviders` 无调用者；且 `GetProvider` 返回锁内指针，调用方读 `Credentials` 会与冷却写入并发竞争 |
| `inwire/openai_chat.go` | `OpenAIChatRequest.Extra` 从无引用；`Stream` / `StreamOptions` / `N` 解码后弃用；`extractText` 只取 `type=="text"`，`image_url` 等非文本内容被**静默丢弃**（图像请求会变成空文本，建议显式拒绝或透传） |
| `auth/auth.go` | `KeyHash` 用 `==` 比较而非恒定时间；`extractKey` 支持 `?key=` 传参，密钥易落入访问日志 / Referer |
| `store/usage_dao.go` | `UsageRecord.Ts` 被 `CreateUsageRecord` 忽略、恒写 now（死字段）；`rows.Scan` 出错静默 `continue`，应改用 `rows.Err()` 上抛 |
| `admin/credential_handler.go` | `Update` 把 DB 错误也当 404 返回；`Delete` 不校验存在，删不存在的 ID 也返回 200 deleted |
| `admin/provider_handler.go` | `protocol` 没有取值校验，任意字符串都能落库；UI 是固定下拉所以碰不到，但 API 可以。**没加**是因为不确定历史数据里有没有非标准值，加白名单会让那些记录的 PATCH 直接失败 |
| `web/src/ui.ts` | `authState.callback` 从未被赋值或读取 |
| `web/src/types.ts` | `UpstreamModel.created_at` 后端并不返回，`Providers.vue` 的 `fmtDate(m.created_at)` 恒显示「—」 |

---

## 5. 验证证据

### 5.1 鉴权（隔离环境 `.workbuddy/tmp/e2e-auth`，18098，空 `admin_token`，模拟全新部署）

```
 1) check（全新建库）            → {"has_password":false,"first_setup":true,"source":"none"}
 2) 无凭据访问 stats             → 401   ✓
 3) 首次设置 test123             → 200   ✓
 4) ★不重启即用新密码访问 stats   → 200   ✓  ← 死循环根因已消除
 5) check                        → {"has_password":true,"first_setup":false,"source":"password_file"}
 6) verify 正确密码              → 200   ✓
 7) verify 错误密码              → 401   ✓
 8) ★无凭据改密码                → 401   ✓  ← 旧版此处为 200（安全漏洞已堵）
 9) 带凭据改密码                 → 200   ✓
10) 旧密码                       → 401   ✓
11) 新密码                       → 200   ✓
12) 弱密码（3 位）               → 400   ✓
```

凭据文件落盘于 exe 同级，`grep` 确认**不含任何明文**：

```json
{
  "version": 1,
  "algo": "pbkdf2-sha256",
  "iter": 210000,
  "salt": "zaJbLjZGQzbTnpj8y2+04Q==",
  "hash": "vSnKYwcUIe0a9lybuwN+EhI/vek/y+KMTE/ALUFyFzk="
}
```

### 5.2 PATCH 语义（隔离环境 `.workbuddy/tmp/e2e-patch`，18097，全新库）

```
################ 上游 provider ################
T1  新建默认值      : name='TestUp' slug='testup' protocol='openai-chat' enabled=True timeout_ms=120000 max_retries=2
T2  PATCH 仅 name   : name='Renamed' slug='testup' timeout_ms=120000 max_retries=2 endpoint='https://example.com/v1'
T3* PATCH timeout=0 : timeout_ms=0 max_retries=0            ← 旧版此处不变，现在真正落 0
T4  PATCH 传 slug   : slug='testup'                          ← slug 不可改
T5  PATCH name=空串 : 400
T6  PATCH endpoint空: 400
T7  PATCH timeout负 : 400

################ 上游模型 ################
M1  新建           : model_id='deepseek-chat' display_name='DS' context_window=128000 max_output_tokens=8192
M2* ctx=0 清空     : context_window=0 display_name='DS'      ← 0 落 NULL，且未误伤 display_name
M3* display_name空 : display_name=''
M4  PATCH model_id空: 400
M5  PATCH max_out负 : 400

################ 上游凭据 ################
C1  新建           : label='main' api_key_mask='sk-a...6789' weight=3 enabled=True
C2  PATCH api_key空 : 400
C3  PATCH weight=0 : 400
C4* label空+weight5: label='' weight=5
C5  PATCH 仅enabled: enabled=False weight=5 api_key_mask='sk-a...6789'   ← 未误伤其他字段

################ 路由 ################
R1  新建 priority=5 : public_name='pub-a' priority=5 fallback_route_id='' enabled=True
R2  新建带兜底      : fallback_route_id='6769f341ab8820ce2b28d37706bf35c1'
R3* priority=0      : priority=0                             ← 旧版此处不变
R4* 兜底清空        : fallback_route_id=''                    ← 旧版此处清不掉
R5  PATCH public空  : 400
R6  PATCH priority负: 400
R7  PATCH 仅enabled : enabled=False public_name='pub-a' priority=0

################ 访问密钥 ################
K1  新建           : name='k1' enabled=True
K2  PATCH 仅enabled: name='k1' enabled=False
K3  PATCH name空    : 400
K4  PATCH 双字段    : name='k1-renamed' enabled=True

################ 边界 ################
E1  不存在 provider : 404
E2  不存在 model    : 404
E3  不存在 route    : 404
E4  不存在 key      : 404
E5  空 body {}      : 200   （什么都不改）
E6  无凭据 PATCH    : 401

################ 库内实际落值（NULL 语义）################
model : {'model_id': 'deepseek-chat', 'display_name': None, 'context_window': None}
routeB: {'public_name': 'pub-b', 'priority': 0, 'fallback_route_id': None}
cred  : {'label': None, 'weight': 5, 'enabled': 0}
prov  : {'name': 'Renamed', 'slug': 'testup', 'timeout_ms': 0, 'max_retries': 0}
```

库内落值确认了 NULL 语义：被清空的字段落的确实是 `NULL` 而不是空串
（`fallback_route_id` 带外键，落空串会直接触发 FK 失败）。

### 5.3 默认监听（隔离环境 `.workbuddy/tmp/e2e-default`，无配置文件）

```
{"level":"INFO","msg":"no config found, generated default","path":"...\\config.json"}
{"level":"INFO","msg":"config loaded","listen":"127.0.0.1:8080",...}
{"level":"WARN","msg":"no admin credential configured; the admin UI will ask you to set a password on first visit"}
{"level":"INFO","msg":"server starting","addr":"127.0.0.1:8080"}
```

### 5.4 静态检查

`go build ./...`、`go vet ./...`、`npm run typecheck`（`vue-tsc --noEmit`）均通过。

> 注意：`npm run build` 只跑 `vite build`，**不含类型检查**。
> 改过前端后必须单独跑一次 `npm run typecheck`，否则类型错误会被静默带进产物。

### 5.5 主密钥落盘（隔离环境 `.workbuddy/tmp/e2e-mk`，18096，**不设任何环境变量**）

模拟局长的启动方式：纯双击、无环境变量、目录里没有 `master.key`。

首次启动：

```
1. master.key 是否自动生成        → 生成，45 字节（base64 44 + 换行），权限 0600
2. 新建凭据 "sk-probe-SECRET-1234567890"
   库内 api_key_enc               → 54 字节 blob，前 16 字节 b',;\xa5\x86rvi$\xebi...'
                                    含明文 "SECRET" ? False   ← 已加密
   接口回显掩码                   → sk-p...7890            ← 能解回来
```

指纹 `sha256(master.key)` = `28686c4a487a578d…`。**不重启进程、只重启网关**，二次启动：

```
1. master.key 指纹                 → 28686c4a487a578d…（**未变**，密钥被复用而非重新生成）
2. 全部凭据掩码
   历史明文（手工注入的明文 blob） → sk-l...3456   OK   ← 兜底生效，老数据没被打死
   default / 主号（密文）          → sk-p...7890   OK   ← 跨启动解密一致
3. POST /admin/api/providers/{id}/test
   → rosetta: transport error during GET https://mk.invalid/v1/models
     （说明已成功解析出凭据并建好客户端，只是假域名连不上；
       若凭据不可用会返回「该上游没有可用凭据」）
```

第 2 条的「历史明文」是**手工往库里插的明文 blob**，
专门验证 §3.9 的兜底逻辑：老数据（明文）与新数据（密文）在同一把密钥下都能读出来。

---

### 5.6 端口自检（隔离环境 `.workbuddy/tmp/e2e-port`）

同一个二进制，只改 `listen`，跑两次：

```
用例 1  listen=127.0.0.1:6666  (期望有警告)
  结果: 命中警告 ✓
{"level":"WARN","msg":"监听端口被浏览器保留，管理界面将无法在浏览器中打开",
 "listen":"127.0.0.1:6666","port":6666,"reserved_for":"alternate IRC",
 "browser_error":"ERR_UNSAFE_PORT","hint":"改用黑名单外的端口（如 8666）；容器里也可把宿主端口映射成 8666"}
  (服务是否起来: 1)
用例 2  listen=127.0.0.1:8666  (期望无警告)
  结果: 无警告
  (服务是否起来: 1)
```

要点：命中保留端口**只警告、不拒绝启动**（反向代理前置或纯 API 调用场景下端口仍然可用），
两种情况服务都正常起来 —— 这正是期望行为。

---

## 6. 排障手册

**浏览器报 `ERR_UNSAFE_PORT`，但容器日志/服务端一切正常？**
端口落在浏览器的保留端口表里（典型是 6666）。这是**客户端**拦截，请求根本没发出去，
所以服务端看不到任何连接。换成黑名单外的端口，宿主和容器用同一个（例如 8666）。
快速自证：容器内 `wget` 与宿主机 `curl` 都通、**只有浏览器不通** —— 就是这个。
网关启动时会自动比对并在日志里警告，搜 `ERR_UNSAFE_PORT` 即可。

**忘了管理密码怎么办？**
删掉部署目录下的 `admin_auth.json` 并重启。系统退回使用 `config.json` 的 `admin_token` 登录。
进去后在「设置」页重新设置密码即可。

**登录时提示「密码错误」怎么办？**
先确认 `admin_auth.json` 是否存在：
- 存在 → 用你设置的那个密码；忘了就按上一条处理。
- 不存在 → 用 `config.json` 里的 `admin_token`（若是 64 位 hex，说明它是历史实现写入的
  `sha256(原密码)`，用原密码登录）。

**看到「设置管理密码」而不是「输入密码」？**
说明系统里没有任何凭据（`first_setup=true`），这是全新部署的正常状态。
注意：这个对话框**只在无凭据时出现** —— 如果它在你已经设过密码后反复出现，说明
`admin_auth.json` 没有成功落盘，检查该目录是否可写。

**改配置端口不生效 / 访问不到？**
先确认你启动的是哪一份 exe —— 本机有两份部署（见 §1），且都默认 18080。
`config.json` 必须与 exe **同级**，`db_path` 的相对路径也按 exe 所在目录解析。

**后台改了东西但 `/v1` 看不到？**
任何写操作都需要 reload 内存快照（前端已自动调用）。若前端 reload 失败会明确提示，
此时可手动 `POST /admin/api/reload`。

**某字段清不掉 / 改不成 0？**
本轮的 §3.7 已修。确认你部署的是本轮之后的构建：
旧版对空串和 0 一律视为「未提供」，前端即使发出去也写不进库。

**想确认上游 API Key 在库里是明文还是密文？**
看部署目录下有没有 `master.key`：
- 有 → 新写入的凭据是密文；文件被人删掉，这些凭据就废了。
- 没有 → 早期构建的产物，**凭据是明文存的**，任何拿到 `gateway.db` 的人都能读走。
  升级到本轮之后的构建、重启一次即可自动生成 `master.key`，之后新增/修改的凭据即加密；
  已存在的明文凭据仍可正常使用（有兜底），但要彻底消除历史明文，
  需要把每条凭据**重新保存一次**（或删了重建）。

**换了启动方式之后凭据解不开（上游返回 401 / 提示没有可用凭据）？**
密钥来源变过。优先级是**环境变量 → `<exeDir>/master.key` → 自动生成**。
如果你先双击启动（密钥落在 `master.key`），后来又设置了 `ROSETTA_GW_MASTER_KEY`
（或改用 `gateway.ps1`），两者内容不同 → 先前加密的凭据解不开。
选定一种启动方式后不要来回换；必须换时，把旧密钥内容原样写进新来源。

---

# 审计与修复记录 — 2026-09-23（故障转移 + 配额复审）

对象：v1.2.0 新落地的**自动故障转移**（`route_targets` 有序链）与**密钥 token 配额**。
全量审阅后端热路径、DAO、快照、admin handler 与前端，构建链与 `go test ./...` 均绿。

## 结论速览

设计扎实：迁移幂等（`ensureColumns` + `backfillRouteTargets`）、零回归意识（未配链的老 route 回落单目标）、
哨兵错误分级、pre-commit 才转移、`committed` 后绝不回退。测试覆盖 failover/quota/routing/store/admin。
复审查出 6 项，**已全部处置**；另留 3 项低优先注记（未改，见文末）。

## 已修复

| # | 严重度 | 问题 | 处置 |
|---|---|---|---|
| 1 | 中 | **主目标列与链分叉**：运行时以 `route_targets` 为准，但 `PATCH /routes/{id}` 改 `provider_id/upstream_model_id` 只动 `routes` 行、不回写链首 → 裸 API 改主目标被静默忽略，响应还回显新值 | 新增 `store.SyncHeadTarget`（空链补 position-0 / 有链只改链首 / 幂等），`RouteHandler.Update` 成功后调用。UI 流程本就一致（随后 `PUT .../targets` 覆盖），此修复覆盖直连 API 的调用方。测试 `TestSyncHeadTarget` |
| 2 | 中 | **客户端断开仍打完整条链**：非流式用 `context.Background()` 无视取消；流式取消可能被判为可转移 → 给跑掉的客户端逐目标重试，烧下游配额 | 转移循环每轮顶部检查 `r.Context().Err()`，非空即 return（不再打下个目标、不误记 error 用量）。测试 `TestFailover_ClientGoneAbortsBeforeAnyUpstream` |
| A | 中 | **`pool.targets` 只增不删 → 慢性泄漏**：每次 `PUT .../targets` 重生成 `target_id`，旧熔断条目永久驻留；重建池只清 `providers` 不清 `targets` | `BuildFromStore` / `BuildFromConfig` 重建时一并清空 `p.targets`。测试 `TestPoolBuildResetsTargetHealth` |
| B | 低 | **死代码**：`upstream.RecordCredentialFailure` 无调用者（热路径按 attempt 结果内联回写），且它使 `outwire` 成为该包唯一依赖 | 删除函数 + 去掉 `outwire` import；同步修正本文档 §4.1 对它的旧引用 |
| C | 低 | **两层健康态语义相反**：重建池会重置凭据冷却，却保留目标熔断（A 的反面） | 随 A 一并解决：两者都在重建时清零，一致。代价（管理员改配置重置 ≤60s 冷却/熔断）已在 DESIGN §10 记为有意取舍 |
| D | — | **单请求内不重试同 provider 的其他 key**：一次 attempt 只用一把 key，坏 key 交给跨请求冷却轮换 | 确认为有意设计，未改行为；在循环取凭据处 + DESIGN §10 补注说明，免后人误判为 bug |

## 文档同步

- DESIGN §6.3：补 `GET/PUT /admin/api/routes/{id}/targets` 端点、`keys` PATCH 的 `quota_tokens`。
- DESIGN §10：补「主目标列与链的一致性」「每次 attempt 只取一把凭证」「客户端断开即收手」「健康态随池重建清零」四段。
- DESIGN §12.2 + `config.example.json` + `docker/config.default.json`：补 `stream_first_token_timeout_ms` / `failover_max_targets` / `failover_failure_threshold` 三个默认及其说明。

## 仍存（低优先注记，未改）

1. **截断/溢出的流被记为目标「成功」**：`attemptStream` 对 `truncated`/`overflow` 也返回 `committed`，调用方随即 `RecordTargetSuccess` 清零熔断。回退不了这点没错，但「持续截断的目标永远开不了熔断」偏乐观——可考虑 truncated 不重置计数。
2. **TTFT 定时器与首事件的窄竞态**：首个事件恰在 `ttftTimer` 关流的同一刻到达时，`gotFirst` 仍为 true，可能把半条流按 `ok` 收尾。概率极低。
3. **配额预检是唯一同步落库的热路径读**：鉴权走内存快照，quota 每请求一次 `SELECT`（为拿权威 `used_tokens`，设计如此）。高并发下值得盯一眼。

---

# 全面审计 — 2026-09-23（全代码库）

范围：`cmd/` + `internal/` 全部 41 个 Go 文件（7.9k 行）+ `web/src` 全部 17 个前端文件。
基线：`go build ./...` / `go vet ./...` 空输出，`go test ./...` 全绿。
方法：通读热路径 → 分模块深审（store / admin+鉴权 / 前端）→ **对可疑结论一律实测**，不靠读码推断。

## 0. 结论速览

**无 P0。** P1 三条（均已实测复现或代码确凿），P2 一批（分六组）。
上一轮（本节之上 2026-09-23 故障转移/配额复审）的 6 项处置仍然有效，未发现回归。

| # | 严重度 | 问题 | 状态 |
|---|---|---|---|
| 1 | **P1** | `defaults` 里负数超时 → `time.NewTicker` panic → 流式响应变成「200 + `text/event-stream` + 一坨 JSON 错误体」，且**漏记用量** | 实测复现 |
| 2 | **P1** | 流式请求被**客户端断开**被记成 `status=error` + `ERROR stream error`，污染用量统计与成功率 | 实测复现 |
| 3 | **P1** | route 的 `(provider_id, upstream_model_id)` **无配对校验** → 建出恒 404 的路由，接口却回 `201` | 代码确凿 |
| 4 | P2 | 一批契约/死字段/并发/前端缺陷，见 §3（A–F 六组） | 已核实 |

---

## 1. P1 详述

### 1.1 负数 `defaults` → `NewTicker` panic → 畸形响应 + 漏记用量（实测）

`internal/config/config.go:141-161` 的 `setDefaults` **只在 `== 0` 时填默认值，负值原样通过**，
`validate()`（164-193）也只检查 `Listen` 与 bootstrap 段，**从不检查 `defaults` 任何字段**。
于是 `stream_idle_timeout_ms: -1` 一路到达 `cmd/gateway/main.go:742`：

```go
heartbeatTicker := time.NewTicker(idleTimeout / 2)   // idleTimeout = -1ms → panic
```

**实测**（隔离环境 `.workbuddy/tmp/e2e`，18099 + 假上游 19090，只改这一行）：

```json
{"level":"ERROR","msg":"panic recovered","error":"non-positive interval for NewTicker",
 "path":"/v1/chat/completions","request_id":"125e757edd0a81ad"}
```

下游实收：

```
HTTP/1.1 200 OK
Content-Type: text/event-stream
X-Accel-Buffering: no
Content-Length: 63

{"error":{"message":"internal error","type":"internal_error"}}
```

三个后果，每个都独立成立：

1. **协议层说谎**：状态码 200 + `Content-Type: text/event-stream`（SSE 头在 `main.go:723-726`
   已写过，`WriteHeader` 不可撤），body 却是一个没有 `data:` 前缀的 JSON 对象。
   任何 SSE 客户端都只会报「解析失败 / 流式响应中没有内容」—— 正是 2026-09-21 那次
   流量事故的症状形态，排查成本极高。
2. **用量漏记**：panic 发生在 `recordUsage` 之前，这次调用在 `usage_records` 里
   **完全不存在**（实测 `usage/history` 里没有记录）。配额与统计同时漏账。
3. **触发门槛极低**：运维用 `-1` 表达「禁用超时」是很自然的直觉。
   同一路径上 `upstream_timeout_ms: -1` 会让 `context.WithTimeout` 立即超时、
   `stream_first_token_timeout_ms: -1` 会让 `AfterFunc` 立即开火 —— 都是「全量请求失败」级。

**建议**：`validate()` 补 `defaults` 区间校验（各超时 > 0、`max_*` >= 0），
并在 `main.go` 里对 `idleTimeout/2` 做下界钳制（`NewTicker` 的入参必须 > 0）。

### 1.2 客户端断开被记成上游错误（实测）

`cmd/gateway/main.go:802-813` 的归类里，`stream.Err()` 只要不是 truncated/overflow 就
`status = "error"`，**不区分 `context.Canceled`**：

```go
default:
    status = "error"
    logger.Error("stream error", "error", err, ...)
```

转移循环顶部那处 `r.Context().Err()` 前置检查（`main.go:534-539`）**只覆盖「尚未写头」**；
一旦 committed（SSE 头已发），客户端断开必然落到这里。

**实测**（同一环境，curl 在约 1.8s 后主动断开，三次）：

```json
{"level":"ERROR","msg":"stream error","error":"context canceled","model":"test-model",
 "key_id":"5b4c1096f05cdc153d3c1566b56deac3"}
```

`GET /admin/api/usage/history` 对应三条：

```
{"total_tokens":0,"ttfb_ms":1,"latency_ms":1806,"status":"error"}
{"total_tokens":0,"ttfb_ms":1,"latency_ms":2102,"status":"error"}
{"total_tokens":0,"ttfb_ms":1,"latency_ms":2201,"status":"error"}
```

**这三条的真实原因是客户端主动断开**（用户点「停止生成」、客户端自带超时、网络切换），
不是上游故障。代价：后台调用历史的错误率虚高、`stats` 的成功率虚低、
日志里 `ERROR` 噪音掩盖真正的上游故障。

**建议**：新增 `status="canceled"`（或复用 `truncated` 语义但单独打 INFO 日志），
判定用 `errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)`。
注意这会改 `usage_records.status` 的取值集合 —— **前端标签中文化**（`History.vue`）
与 `DESIGN.md` §8 的四值定义需同步，故列为待决策而非直接改。

### 1.3 route 的主目标列不校验「配对」，可造出恒 404 的路由

两条写入路径对同一件事的严格程度**不一致**：

| 端点 | 校验 |
|---|---|
| `PUT /admin/api/routes/{id}/targets` | `route_target_handler.go:122-143`：provider 存在、model 存在、**且 `m.ProviderID == in.ProviderID`**，并有测试守护 |
| `POST /admin/api/routes`、`PATCH /admin/api/routes/{id}` | `route_handler.go:74-141` / `143-232`：**只校验非空串**，无配对校验 |

而 `route_handler.Create` 会在 126-138 行**立刻种一条 position 0 的目标**（用同一对
`route.ProviderID` / `route.UpstreamModelID`）。FK 只做单列存在性检查
（`store.go:93-94`、`151-153`），不拦跨 provider 的配对。

运行时后果：`routing.buildCandidates`（`routing.go:195-228`）用
`findUpstreamModel(t.ProviderID, t.UpstreamModelID)` 查不到 → 候选为空；
由于该 route 有目标行（`len(targets) > 0`），**不回落主目标列**，直接
`return out`（空）→ `Resolve` 报 `ErrModelNotFound`。

于是：**接口返回 `201 Created` 并回显这条路由，界面显示保存成功，`/v1` 上该公开模型名恒 404。**
唯一的差异是 `route_targets` 那条链，而链是谁种的？正是 `Create` 自己 —— 与 targets 端点
的严格校验自相矛盾。

**建议**：把 `validateTarget` 的配对校验抽成 `store` 层可复用的函数，
在 `route_handler.Create/Update` 落库前调用，不合法返 `400`。

---

## 2. 实测环境说明（复现路径）

隔离环境 `.workbuddy/tmp/e2e`（18099 + 假上游 19090），沿用 `rosetta-gateway-verify` skill 的
脚手架，本轮新增：

- `zzfake` 增加 **`slow` 模式**：分 100 批 × 200 块慢速吐字（每批后停 50ms），
  用来让长流跨越网关 keepalive 的触发周期。
- `check_sse.py`：按 SSE 帧规范逐行校验下游实收字节（`data:` 载荷必须能 `json.loads`，
  非空行只允许 `data:` / `event:` / `: ` 三种形态）。

**踩到的坑（已回写 skill）**：`gateway-e2e.exe` 若用 `ROSETTA_GW_MASTER_KEY=<固定值>` 启动，
而库里的凭据是用同目录 `master.key` 加密的，两者派生的密钥不同 → 凭据全部解不开 →
`/v1` 恒 `no available upstream provider`（**不是**路由问题，也不是上游问题）。
本轮改为**不设该环境变量**、并删库重新 bootstrap，才跑通。

---

## 3. P2 分组（已核实，未改）

### A. 死字段 / 静默失效（「存了但不用」，与 §4.3 的 extra_json 同类）

| 位置 | 问题 |
|---|---|
| `internal/config/config.go:16,138-140` + `main.go:65-67` | **`log_level` 是死配置**：被解析、被写进启动日志的 `log_level` 字段，但 logger 硬编码 `slog.LevelInfo` —— 改成 `debug`/`warn` 完全没有效果 |
| `internal/snapshot/rebuild.go:14` | **`RebuildFromDB` 的 `pool *upstream.Pool` 参数从未被使用**（函数体 15-108 行无引用），该 import 仅为它存在。池的重建实际发生在 `ReloadHandler` 里（分工明确，但这个参数是纯噪音） |
| `internal/routing/routing.go:18,96-99,157-164` | `Route.Priority` 被 `rebuild.go:67` 搬进快照，`Resolve` 却**只做一次 `byPublicName[model]` 查表**。§4.2 记的「同名路由互相覆盖」因 `store.go:92` 的 `public_name TEXT NOT NULL UNIQUE` 而**不可达**（DB 层挡住），但字段本身仍是假语义 —— 界面写着「按 priority 升序择优」，实际没有任何择优 |
| `internal/upstream/upstream.go:227,244,251` | `GetClientForCredential` / `GetProvider` / `ListProviders` **仍无调用者**（§4.5 记录过，未清理）；其中 `GetProvider` 返回**锁内裸指针**，调用方读 `Credentials` 会与冷却写入并发竞争 |
| `internal/upstream/upstream.go:148-151,460-463` | **`provider.max_retries = 0` 被静默替换为全局默认**（`if sp.MaxRetries > 0`）。用户显式设 0 表达「不重试」，实际走 `cfg.Defaults.MaxRetries`（默认 2）。与 §3.7 确立的「0 是合法显式值」契约直接冲突。（`timeout_ms: 0 = 用全局默认` 是文档明示的，`max_retries` 不是） |
| `internal/upstream/upstream.go:601-610` | `buildClient` 的 `switch prov.Protocol` **无 `default`** → 拼错的 protocol（如 `openai`）不加任何 `WithProtocol`，静默退化为 SDK 自动探测。与 `admin/provider_handler.go` 不校验 protocol（§4.5 记录）组合成**静默错配** |
| `internal/store/store.go:107-113` | `access_keys` 的 `expires_at` / `rpm_limit` / `tpm_limit` / `last_used_at` 是**死列**：建表有、全仓无读写路径。未实现的「密钥过期 / 限速」看起来像已支持 |

### B. 管理 API 契约

| 位置 | 问题 |
|---|---|
| `internal/admin/usage_handler.go:56-70,105-106` | `GET /admin/api/usage` 的 `limit` **无上限、负数不拒**。SQLite 里 `LIMIT -1` 语义是「不限制」（`strconv.ParseInt("-1")` 得到 -1，`if limit == 0` 不触发）→ `?limit=-1` 拉全表；`?limit=999999999` 全量物化。同文件 `History`（166-171）已有 `<=0→200 / >1000→1000` 的 clamp，此处是**同一不变量漏了一处**。另：`group_by` 分支（88-103）用 `args = []any{from, to}` **整体覆盖**前面已追加的 `key_id`/`model`/`provider_id` 过滤条件，且 SELECT 里含非聚合裸列（SQLite 取组内任一行）—— 当前前端不传 `group_by`，属潜伏 |
| `internal/admin/route_handler.go:74-141` | 同名 `public_name` 的 Create 撞 `store.go:92` 的 `UNIQUE` → 返回 **500 + 裸 `UNIQUE constraint failed: routes.public_name`**，而非 409 + 可读文案。前端 `Routes.vue` 的两步创建（create → saveTargets）在这种失败下提示「保存失败」，用户重试必然是同一个 500 |
| `model_handler.go:120-124`、`route_handler.go:144-148`、`key_handler.go:112-116`、`provider_handler.go:249-253` | **DB 错误被当成 404**（`if err != nil \|\| existing == nil { 404 }`）。DB 故障时前端看到「资源不存在」，会误导运维去删库/重建配置。credential_handler 的同款问题 §4.5 已记，这四处是**同模式扩散** |
| `provider_handler.go:238-244`、`model_handler.go:174-180`、`route_handler.go:234-240`、`key_handler.go:151-157` | **Delete 不存在的 ID 返回 200 deleted**：DAO 层（`provider_dao.go:106-109` 等）是裸 `ExecContext` + `return err`，不看 `RowsAffected` |
| `model_handler.go:75-117`、`credential_handler.go:56-101` | **子资源端点不校验父资源存在**：`POST /admin/api/providers/{乱填}/models\|credentials` 直接 INSERT → 撞 FK → **500 `FOREIGN KEY constraint failed`**（应 400/404）。`GET .../{不存在}/models` 则返回 200 + `[]`，无从区分「provider 不存在」与「没有模型」 |
| `provider_handler.go:123-147`、`route_handler.go:121-138,218-229` | **复合写无事务**，第二步失败留下半成品却返 500：provider 已建但 credential 建失败；route 已建但 seed target 失败（作者在 136 行的文案里已自认这点）；route 已改但 `SyncHeadTarget` 失败 → **`routes` 主目标列与 `route_targets` 链永久分叉**。`route_target_dao.go:156-185` 的 `ReplaceRouteTargets` 已是正确范式（`BeginTx` + `defer Rollback`），照抄即可 |
| `internal/store/route_target_dao.go:129-151` | `SyncHeadTarget` 是「读-改-写」跨两条语句、**无事务**；并发两个 PATCH 会丢更新。`MaxOpenConns(1)` 只保证单条语句串行，不覆盖这次组合 |
| `provider_handler.go:124,131,144,231,240,257`、`model_handler.go:112,167,176`、`key_handler.go:101,144`、`usage_handler.go:110`、`password_handler.go:67` | **500 响应直接回显 `err.Error()`**：把 SQLite 约束名/语句片段、乃至 `密码_handler` 的凭据文件**绝对路径**吐给调用方。管理端点虽已鉴权，仍是内部信息泄露面 |
| `usage_handler.go:203-247` | `/admin/api/usage/by-model\|by-key\|by-provider\|by-day` **静默忽略 `limit`**：`web/src/api.ts:174-175` 明确请求 `?limit=10`，`groupBy`/`groupByNamed` 全文不读该参数 → 全量返回 |
| `usage_handler.go:119-139` | `summary` 是**从被 `LIMIT ?` 截断的页内行累加**的：字段名 `total_requests` 暗示总量，实际是「本页条数」；`avg_latency_ms` 同理是页内均值 |
| `usage_handler.go:121-123,187-189,254-256,279-281` | 四处 `rows.Scan` 出错 `continue` **静默丢行**，循环后**均未检查 `rows.Err()`**。（`store` 包内已全部上抛，此处是包外漏网） |

### C. 鉴权 / 安全

| 位置 | 问题 |
|---|---|
| `cmd/gateway/main.go:142,910-938` | **`GET /v1/models` 完全不鉴权**：`handleListModels` 不调 `auth.Authenticate`，任何人可枚举全部公开模型名（等于暴露路由与供应商结构），也不受 key 的 enabled/配额约束。同一文件的 `/v1/chat/completions` 是鉴权的（450 行）。官方 OpenAI 的 `/v1/models` **需要**鉴权 —— 但「模型目录公开」也可能是产品决策，**需局长定** |
| `internal/adminauth/store.go:94-101` + `main.go:155-159` | 凭据文件**格式损坏/内容非法 → `os.Exit(1)`，整个进程拒绝启动**。`/v1` 数据面根本不读管理凭据，却被一起拖死：一次磁盘写坏或手工编辑失误 = 全部转发服务中断。`store.go:96` 的注释只论证了「不该静默放行」，没论证「该拒绝启动」 |
| `internal/adminauth/store.go:57,150` + `server.go:158` | 管理登录**无失败计数/限速/锁定**；口令下限仅 6 位。PBKDF2 21 万迭代把单次尝试压到几十毫秒（对交互无感），但并发下 6 位弱口令仍可爆破，且失败路径无任何痕迹 |
| `internal/adminauth/store.go:244-253` | `verifyFallback` 保留 `len(expected)==64 && isHex(expected)` 的「猜明文还是摘要」启发式 —— 正是包注释 21-23 行声称已消除的失败模式。运维把 `admin_token` 设成 64 位 hex 明文字符串时，**同一串作 Bearer 会恒 401**（被当作 sha256 摘要比对） |
| `internal/auth/auth.go:28,49` | `ks.KeyHash == hashHex` 非恒定时间比较；`extractKey` 仍支持 `?key=` 传参（§4.5 记录，未变）。补充证据：`server.Middleware` 只记 `r.URL.Path` **不记 query**，故本网关日志不落密钥；泄露面在 Referer / 上游代理日志 |

### D. 流式与 HTTP 细节

| 位置 | 问题 |
|---|---|
| `main.go:742,768` | keepalive 的 `time.NewTicker(idleTimeout/2)` **创建后从不 Reset**（只有 `idleTimer` 在 `handleEvent` 里 Reset）→ **上游持续吐字时心跳照发**，实测 1.8s 的流里发了 **18 次** `: keepalive`（raw_slow_*.sse 可见其与 `data:` 事件交错）。对标准 SSE 客户端无害（注释行），对朴素按行解析的下游（如 leans 的 `backend/service/ai.go`）是纯噪声 |
| `main.go:745-757` | **heartbeat goroutine 与主 SSE 循环并发写同一个 `http.ResponseWriter`**（主循环经 `SSEWriter`，心跳直接 `fmt.Fprintf(w, ...)`）。Go 明确不支持并发使用 ResponseWriter；`statusResponseWriter.written`/`statusCode`（`server.go:57,62,70`）也是无锁写。**实测 3 轮、6814~8594 个事件、18~22 次并发写窗口，未观测到字节损坏**（`check_sse.py` 报 `corrupt_lines=0`）—— 定 P2：违反契约但触发窗口极小，随时间与并发累积。**注意本机 `go test -race` 不可用**（链接器故障：`cannot find default-manifest.o`），无法用 race detector 硬证。修法：`SSEWriter` 内加 mutex，心跳改走 `sse.WriteComment()` |
| `server.go:188` | `Recovery` 用 `http.Error` 写 JSON **字符串** → `Content-Type: text/plain; charset=utf-8` 且内容多一个换行；若响应头已发出（如上面的 §1.1 场景），只能把这段文本追加进 SSE 流里 |
| `main.go:902-908` | `recordUsage` 是 **fire-and-forget goroutine**，`srv.Shutdown` 只等 handler 返回、不等它 → 进程退出时**尾部若干条用量可能丢**。（顺带：§4.4 配额预检注释声称「并发下容忍至多一个在途请求超发」，实际是无上限的 fire-and-forget，突发 N 个并发请求会在第一条 usage 落库前**全部**通过预检） |
| `inwire/openai_chat.go:71` | 硬编码 `io.LimitReader(r.Body, 32*1024*1024)`，与 `cfg.Defaults.MaxRequestBodyBytes`（可配，默认同为 32MB）**两处独立**：配大了内层仍截断（`json` 报「unexpected end of JSON input」）；配小了外层 `MaxBytesReader` 触发，但错误被包成 `read body: ...` → 返回 **400 而非 413** |
| `outwire/openai_chat.go:173-188` | `WriteUsage` 只发 `prompt_tokens`/`completion_tokens`/`total_tokens`，**不含 `prompt_tokens_details.cached_tokens`** —— 网关内部明明记了 `cached_tokens`（§5.2 口径），下游却拿不到 |

### E. 前端（`web/src`）

| 位置 | 问题 |
|---|---|
| `views/Routes.vue:88-97,169` | **目标链加载失败被静默吞掉并伪造单行链**：`catch { form.targets = [{主目标}] }` 无 toast、无 return。用户下一次保存 → `PUT .../targets` 后端先 `DELETE` 再按数组 INSERT（`route_target_dao.go:163`）→ **position 1..N 的目标被静默删光**。这是本轮前端最严重的一条（静默数据丢失） |
| `api.ts:84-96` | `mutate()` 的 reload 失败分支把 **401 抹成 `status: 0`**（`new ApiFail(0, ...)`）→ 绕过所有 view 的 `status !== 401` 守卫，令牌过期时在令牌弹窗之上再叠一条错误 toast，真实 401 语义丢失 |
| `App.vue:183-195` | 确认弹窗 `<AppModal>` **缺 `@close`**（对比 `Keys.vue:168` 有）：`AppModal.vue:14` 的遮罩点击会 `emit('close')` 但无人监听 → 遮罩点击无效；且 `ui.ts:53` 的 `confirmState.resolve = resolve` 直接覆盖，**上一个 Promise 永久 pending** |
| `styles.css:791` | `@media (prefers-transparency: reduced)` —— **特性名非法**（正确是 `prefers-reduced-transparency`），整块降级**永不生效**，与文件内注释「三个必须齐备」矛盾 |
| `views/Settings.vue:28-34,85-101` | `password/check` 的失败被并入 `load()` 的同一个 `try` → settings 已成功加载也会报「加载设置失败」；改密码**无提交锁**（按钮无 `:disabled`、函数无守卫）→ 双击时第二次带已失效的旧密码 → 401 toast，而第一次其实已成功 |
| `ui.ts:68`、`api.ts:24-27`、`Overview.vue:56`、`History.vue:30` | 死代码：`authState.callback`（§4.5 已记）、**`auth.ready` 从未被写入或读取**（新增）、两处 `defineExpose({ load })` 无人引用 |
| `views/Routes.vue:181-192` | `toggleRoute` **仍回传完整字段集**（`public_name`/`provider_id`/`upstream_model_id`/…），构成读-改-写，与「只发要改的字段」的新契约不符（`Providers.vue:109`、`Keys.vue:95` 已是正确写法） |
| `views/Providers.vue:406` | 成功率展示自相矛盾：`0.995` 被 `Math.round` 显示成 `100%`，同时因 `< 1` 而带 warn 橙 —— 99.5% 与 100% 在界面上完全等价 |

### F. 小瑕疵

| 位置 | 问题 |
|---|---|
| `usage_handler.go:302` | `var _ = sql.ErrNoRows`：为了让 `database/sql` 这个**实际只被这一行使用**的 import 不报错而写的死代码 |
| `outwire/openai_chat.go:59,87` | `SSEWriter.flushed` 被写但**从未被读** |
| `outwire/openai_chat.go:214,231` | `WriteNonStreamResponse` 手工拼 JSON（`"` + `escapeJSON` + `"`）；且有 tool_calls 时 `msg.Content = nil` + `omitempty` → **`content` 字段整体消失**（OpenAI 语义应为 `null`） |
| `inwire/openai_chat.go:218` | `extractText` 解析失败时 `return string(raw)` —— **畸形 content 会被原样当正文发给上游**；数组里的 `image_url` 等非文本块仍被静默丢弃（§4.5 已记） |
| `store/store.go:26` | DSN `?_journal_mode=WAL&_foreign_keys=ON` **未设 `_busy_timeout`**：多进程打开同一 db（备份/CLI/双实例）时写锁竞争直接返回 `SQLITE_BUSY`，配合 `main.go:468` 的 fail-open 会让配额预检静默失守。单实例进程内因 `SetMaxOpenConns(1)` 无此问题 |
| `store/usage_dao.go:37-39` | `error_code` / `request_id` 是可空列却直接落空串，未走项目约定的 `nullIfEmpty`（同文件其他列与 `provider_dao`/`credential_dao` 都已走） |

---

## 4. 已核对、确认无问题（避免重复排查）

- **SQL 注入**：全仓唯一的字符串拼 SQL 在 `store.ensureColumns`（`fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s")`，三处全部取自**常量白名单**）与 `usage_handler.groupBy(column)`（column 只用字面量调用）。用户输入一律走 `?` 占位符，`ORDER BY`/`LIMIT` 无拼接。
- **事务**：`route_target_dao.go:157` 的 `BeginTx` 写法正确（失败即 return、`defer Rollback`、`Commit` 错误上抛）。**除此之外全仓无事务** —— 已列进 §3.B。
- **NULL / 外键**：`fallback_route_id`（唯一带 FK 的可空列）在 `route_dao.go:109,126` 走 `nullIfEmpty`，不会触发 FK 787；所有 `scan*` 的可空列都用 `sql.Null*` 承接；8 处 list 函数全部 `make([]T,0)`（空结果不会序列化成 `null`）。
- **行扫描（store 包内）**：所有 `rows.Scan` 出错均 `return nil, err`，所有 `defer rows.Close()` 齐全，所有 `return result, rows.Err()` 都检查了。无 `*sql.Rows`/`*sql.Stmt` 跨 goroutine，无 Prepared Statement。
- **SQLite 算术**：`usage_dao.go` 的 `cache_hit_rate` / tps / 成功率全部写成 `SUM(x) * 1.0 / NULLIF(SUM(y), 0)`，无整数除法截断、均有除零保护。
- **触发器与冗余状态**：`usage_records` 只有 `AFTER INSERT` 触发器；全仓**无** `UPDATE/DELETE usage_records`；`UpdateAccessKey` 不碰 `used_tokens`；失败路径落的 error 记录 `TotalTokens=0`（加 0 不虚增）；usage 只有一处 INSERT → **无重复计数**。
- **迁移幂等**：`CREATE TABLE/INDEX/TRIGGER` 全带 `IF NOT EXISTS`；`ensureColumns` 先 `columnExists` 再 `ALTER`；`backfillRouteTargets` 用 `WHERE NOT EXISTS`。`store_test.go` 已实测三次重开不重复。
- **删除级联**：providers→credentials/upstream_models 为 CASCADE；routes/route_targets→providers 与 routes→upstream_models 为 RESTRICT；route_targets→routes 为 CASCADE → 无孤儿配置行。（代价是删除被拒不友好，见 §3.B）
- **CORS / CSRF**：`Access-Control-Allow-Origin: *` 但鉴权走 `Authorization` 头（非 Cookie），浏览器不自动携带，跨源页面也读不到 `localStorage` 中的令牌 → 无 CSRF。
- **路径穿越**：`AdminAuth` 对白名单用 `==` 精确匹配；ServeMux 先做 `cleanPath` 归一并对脏路径发 301。逐一核对 `main.go:181-224` 注册表，**未被白名单放行的敏感端点无一旁路**；`/admin/api/auth/verify` 确实需鉴权。
- **恒定时间比较（管理端）**：`adminauth` 的 PBKDF2 结果与 fallback 两条分支全部 `subtle.ConstantTimeCompare`。例外只有 §3.C 的 `auth.KeyHash`。
- **PATCH 指针语义**：逐个 handler 核对，**全部 `if req.X != nil`**，无一处 Update 残留 `deref*` 或 `!= ""` / `!= 0`；必填字段显式空串一律 400；`deref*` 只出现在 Create 分支 —— 与 `helpers.go:39-40` 的约定一致。
- **reload 契约**：全仓无 view 绕过 `mutate()`；`ReloadHandler` 先重建 pool 再 `snapshot.Swap`，顺序正确。
- **前端 XSS**：`v-html|innerHTML|dangerously|eval(|new Function` **零命中**，所有用户可控字符串经 `{{ }}` 转义。
- **前端凭据流程**：`App.vue:70-92` 严格「先 `/auth/verify` 成功才 `saveToken`」，失败只写 `authError` 不落库；`Settings.vue` 改密码的 `saveToken` 在 `if (res.ok)` 内 —— **2026-09-21 修的死循环未回退**。
- **API 契约对齐**：`api.ts` 全部 path/method 与 `main.go:181-224` 一致，字段名与 Go json tag 一致。
- **ID 随机性**：`generateID()` = `crypto/rand` 16 字节（128-bit），不可枚举。
- **前端 typecheck**：`npm run typecheck` 通过（`vite.config.ts` 不参与类型检查是已知的 tsconfig 限制，§6 已记）。

---

## 5. 建议处置顺序

**建议立即修（低风险、无产品语义变更）**

1. §1.1 `config.validate` 补 `defaults` 区间校验 + `main.go` 对 `NewTicker` 入参钳制下界。
2. §1.3 `route_handler.Create/Update` 复用 `validateTarget` 的配对校验。
3. §3.B `usage_handler` 的 `limit` clamp（照抄同文件 `History` 的写法）。
4. §3.A `RebuildFromDB` 删掉死参数、`config.log_level` 要么接线要么删字段。
5. §3.D `Recovery` 改用 `outwire.WriteOpenAIError` 风格的自定义 writer（别用 `http.Error`）。

**需局长决策后再动**

6. §1.2 客户端断开是否新增 `status="canceled"`（会改 `usage_records.status` 取值集合，
   连带前端标签与 `DESIGN.md` §8）。
7. §3.C `/v1/models` 是否加鉴权（产品决策）。
8. §3.A `route.priority`：接上「按 priority 择优」（改 `byPublicName` 为多值 + 排序），
   还是摘掉字段与界面提示（DB 层 `public_name` 已 UNIQUE，择优目前不可能生效）。
9. §3.E `Routes.vue` 的链加载失败处理（是禁止保存并提示，还是二次确认）。

---

# 全修 — 2026-09-24（v1.2.0）

局长指令：「全修」。即上面列出的**全部**问题 —— 含「建议立即修」5 项、4 项「需决策」、
以及 §3 的六组 P2（A 死字段 / B 管理 API 契约 / C 鉴权安全 / D 流式与 HTTP / E 前端 / F 小瑕疵）。

## 1. 四项待决策的落地

| 项 | 决定 | 理由 |
|---|---|---|
| §1.2 客户端断开 | **新增 `status="canceled"`** | 客户端主动断开既不是网关的错也不是上游的错；记成 `error` 会污染错误率，让「用户关了个页面」看起来像线上故障。统计口径（`GetUsageStats` / `ListModelThroughput`）同步排除该值；前端用中性灰、与暖橙的 `失败/截断` 区分。 |
| §3.C `/v1/models` | **加鉴权** | 它暴露的是完整的公开模型名清单。未鉴权时等于把「这台网关上配了哪些模型」免费送给任意扫描者，而 `/v1/chat/completions` 本身就要鉴权 —— 这是个不一致的缺口。 |
| §3.A `route.priority` | **摘掉字段**（而非接上语义） | 「同名路由择优」在数据层不可能成立（`routes.public_name` 是 `UNIQUE`），而「一个公开名挂多个上游」已由 `route_targets` 链（有序 + 可故障转移）承担。两个机制重叠，保留 `priority` 只会让人以为它在起作用。 |
| §3.E 链加载失败 | **禁止保存并提示** | 保存走 `PUT /routes/{id}/targets` 的**整体替换**。加载失败时伪造一条单目标链再提交，等于把 position 1..N 静默删光 —— 界面显示「保存成功」，故障转移链已经没了，且无撤销路径。宁可挡着不让存。 |

## 2. 本轮实测新发现（原审计未列出，均已修）

三条都是「编译通过、看代码像对的、只有实测才暴露」的类型。

### 2.1 管理端登录限速**完全失效**（P1，功能性）

`server.go` 的 `FailureThrottle.Fail` 原文：

```go
e, ok := t.fails[ip]
if !ok || !time.Now().Before(e.until) {   // ← 对新条目恒为真
    e = &failEntry{}
    t.fails[ip] = e
}
e.count++
```

新条目的 `until` 是**零值**，而任意时刻都「不在零值之前」，所以这个条件对新条目恒为真：
每失败一次就新建条目、`count` 被清回 1，`count >= limit` 永远不成立。

实测：连打 26 次错误密码，**全是 401，一条 429 都没有**。管理密码下限只有 6 位，
PBKDF2 21 万迭代把单次尝试压到几十毫秒 —— 这道唯一的在线防线此前形同虚设。

修法：用 `e.until.IsZero()` 区分「从未冷却」与「冷却已过」，并在冷却重启时把 `until` 清回零值
（不清就会在下一次 `Fail` 又命中重置分支，行为退化成同样的失效）。
`internal/server/throttle_test.go` 六个用例钉死；**已用旧实现反证**：旧代码下 3 个用例 FAIL，
其中一条报的正是实测看到的 `超过阈值应 429，得到 401`。

### 2.2 所有 Create 响应的时间戳恒为 0（P2，契约）

`createProvider` / `createCredential` / `createRoute` / `CreateAccessKey` 都把
`time.Now().UnixMilli()` 存进**局部变量** `now` 落库，却从不回写到结构体 ——
于是创建响应里的 `created_at`（provider 还有 `updated_at`）是 0，与随后 GET 到的同一条记录不一致。
前端目前恰好只用创建响应里的 `plaintext_key`，所以没暴露出可见症状，但契约是错的。
`route_target_dao.go` 一直是正确写法（`if t.CreatedAt == 0 { t.CreatedAt = now }`），本轮把其余四处对齐。

### 2.3 `upstream_models` 根本没有 `created_at` 列，前端却在渲染它（P2，死字段）

`types.ts` 的 `UpstreamModel` 声明了 `created_at: number`，`Providers.vue` 用
`fmtDate(m.created_at)` 渲染 —— 该列在表里不存在、接口也不下发，于是模型列表里永远挂着一个
只显示「—」的日期列。已从类型与模板删除。

### 2.4 上游 404 不在可转移集合里 → 一条链首模型被上游退役的链会**永久硬失败**（P1，功能性）

**怎么发现的**：验证「故障转移到底会不会发生」时，把链配成
`[fakeanth/fake-claude , fake/fake-model]`（链首打的 anthropic 路径在本机假上游上必 404），
开启 `failover_enabled` + `max_targets=2`，然后发一次请求：

```
HTTP 502  {"error":{"message":"not found","type":"api_error","code":"upstream_error"}}
假上游 /__last 显示实收 model = "fake-claude"   ← 只打了链首，根本没有转移
```

**根因**：`outwire.FailoverEligible` 的判定集合是 5xx / 401 / 402 / 403 / 408 / 429 / 传输层，
**404 不在其中**（还有一条 `errors_test.go` 用例显式断言 404 → false）。于是 `out.eligible=false`
立刻 `break`，`max_targets=2` 形同虚设。

**为什么这是真 bug 而不是设计取舍**：上游 404 的语义是「我这个提供商没有这个模型」，
属于**目标级**配置问题 —— 而链正是为吸收目标级故障存在的。最现实的触发场景是
**上游退役模型**（OpenAI / Anthropic 定期下架旧模型）：链首那个模型一旦被退役，
整条链会永久硬失败，故障转移在最需要它的场景里恰好是失效的。运维加链的动机通常就是
「主上游不稳/在换模型」，此时链首 404 若不能落到链尾，链等于白配。

**附带**：`MapUpstreamError` 对 404 落到 `default` 分支，把上游那句裸 `"not found"`
塞进 **502 `upstream_error`** 返回。这既掩盖了病因（调用方会去查自己的 model 名，
而模型名在网关自己的 `/v1/models` 里是合法的），又泄漏上游原文。改为 404 `model_not_found`。

**修法**：404 / 410 纳入可转移集合；`CredentialCooldown(404) = 0`（**不罚凭据** ——
key 是好的，错的是目标的模型配置，罚它会把目标级故障放大成 provider 级故障），
这类失败只累计目标熔断。

**反证**：临时把 404 从两个判定里摘掉，`TestFailover_NonStreamSwitchesOnUpstream404` 与
`TestFailover_SingleTarget404MapsToModelNotFound` 双双 FAIL，报的正是实测看到的那个形状：

```
want 200 after failover on 404, got 502 body={"error":{"message":"model not found","type":"api_error","code":"upstream_error"}}
```

### 2.5 `MarkCredentialError` 是个纯日志空操作（P2，死代码 / 假机制）

原审计 §4.1 记的是「`MarkCredentialCooldown/Error` 无调用者，失效凭据永不摘除」。
本轮把调用者接上之后才发现后半句不是「没接上」而是**接上也没用**：

```go
func (p *Pool) MarkCredentialError(credID string, err error) {
	...
			cred.Status = "error"        // ← 只改内存，不回写库
			p.logger.Warn("credential error", ...)
```

而健康过滤 `getHealthyCredentials` 只跳过 `cooling`（且 `cooldown_until > now`）与 `disabled`，
**从不看 `"error"`**；管理 API 读的又是库里的值（内存里这次改也没落库）。所以这个函数
唯一的效果是打一行 warning。

更彻底的是：它所在的 `else` 分支**本来就永远进不去**。热路径只在 `out.eligible` 时回写，
而当时所有可转移状态（401/403/402/408/429/5xx/传输层）都带非零冷却，`credCooldown > 0`
恒成立 —— 那个 `else` 是死分支，只是没人注意。

**处置：删掉函数与那条死分支，而不是给它补行为。** 判废一把 key 的唯一真实手段是冷却；
对「404 这类不该罚凭据的失败」，正确的记账位置是目标熔断而不是凭据状态。
（把 `"error"` 接成「永久摘除」反而有害：一个健康 key 会陪着一个配错的模型一起下线。）

## 3. 已修复清单

- **P1**：配置 `defaults` 区间校验 + `NewTicker` 入参钳制（负数超时改为**启动期拒绝**，不再 panic）；
  客户端断开 → `canceled`；route 主目标列**配对校验**（复用 `validateTarget` 的同一份判定）；
  **上游 404/410 纳入可转移集合**（见 2.4）+ 404 对外映射改 `404 model_not_found`。
- **P2.A 死字段**：`config.log_level` 接线（`slog.LevelVar` + `slog.SetDefault`）；
  `RebuildFromDB` 删死参数；删 3 个无调用者的 pool 方法；`max_retries` 显式 0 不再回落默认；
  `buildClient` 补 `default` 分支；**删 `Pool.MarkCredentialError` 及其永不可达的调用分支**（见 2.5，
  原 §4.1 的「失效凭据永不摘除」实为「接上也无效」）；`access_keys` 删 4 死列（`expires_at`/`rpm_limit`/`tpm_limit`/`last_used_at`）+
  `routes` 删 `priority`（`dropDeadColumns()` 真删，不只是建表语句里去掉）；
  `access_keys.quota_tokens` 补上管理端写入口（运行时的配额预检本来就在，缺的只是写入路径）。
- **P2.B 管理 API 契约**：`limit` clamp；DB 故障与 404 分离；Delete 未找到 → 404；
  父资源校验；三处复合写改**单事务**（provider+credential、route+链首目标、route 更新+链首对齐）；
  500 不回显底层错误；UNIQUE → 409；外键冲突 → 409；`usage` 明细与汇总共用同一份 WHERE
  （修 `group_by` 覆盖 args）、`summary` 改**全量聚合**（不再页内累加）、`rows.Err()` 检查、
  非聚合裸列给中性值；补 `routes/{id}/targets` 子资源端点。
- **P2.C 鉴权安全**：`auth.KeyHash` 改**恒定时间比较**并遍历全部 key（原实现提前 return，
  可按响应时序区分「键存在但停用」与「键不存在」）；删 `?key=` 查询参数支持（会进 access log / Referer）；
  `admin_auth.json` 损坏不再 `os.Exit(1)`（改锁定态，转发服务不受影响）；登录失败限速（本节 2.1）；
  删 `verifyFallback` 的 `len==64 && isHex` 启发式（应为明文比较）；`/v1/models` 加鉴权。
- **P2.D 流式与 HTTP**：keepalive ticker 补 `Reset` + 入参钳制下界；`SSEWriter` 加锁 + 新增
  `WriteComment`（并发写同一个 `http.ResponseWriter` 是数据竞争）；`Recovery` 换掉 `http.Error`
  （SSE 头已发时不能再写 JSON 错误体，先查 `Committed()`）；`recordUsage` 改为可等待的
  `usageRecorder` + 关停时 drain（原来进程退出会丢最后几条用量）；
  `inwire` 请求体上限单源化 + `*http.MaxBytesError` → 413；`WriteUsage` 补
  `prompt_tokens_details.cached_tokens`；`statusResponseWriter` 加锁。
- **P2.E 前端**：`Routes.vue` 链加载失败禁止保存（§1 决策）+ `toggleRoute` 只发增量字段；
  `api.ts` `mutate()` 的 401 原样上抛；`App.vue` 确认弹窗补 `@close` + 接住锁定态；
  `ui.ts` 覆盖前结算旧 Promise；`styles.css` 的 `prefers-transparency` → `prefers-reduced-transparency`
  （原特性名非法，整块降级样式从未生效）；删死代码 `auth.ready` / `authState.callback` / 两处 `defineExpose`；
  `Settings.vue` 的 `password/check` 从 `load()` 的 try 拆出 + 改密码加提交锁；
  `Providers.vue` 成功率改用 `fmtPercent`（`0.995` 不再被显示成 `100%` 却挂着 warn 橙）；
  `fmt.ts` 补 `canceled` 标签与中性徽章、`fmtPercent` 不再向上进位到 100%。
- **P2.F 小瑕疵**：`var _ = sql.ErrNoRows` 死代码；`SSEWriter.flushed` 死字段；
  `WriteNonStreamResponse` 手工拼 JSON → `json.Marshal`，且有 tool_calls 时 `content` 下发 `null`
  而非整体消失；`extractText` 畸形 JSON 不再原样当正文（改返回空串 → 由 SDK 校验拒绝为 400）；
  DSN 补 `_busy_timeout=5000`；`error_code`/`request_id` 走 `nullIfEmpty`。

### 附带完成：多模态输入透传（原「已知缺口」）

`inwire.extractText` 把 `image_url` 静默丢弃：客户端发多模态请求，网关照单全收并回 200，
上游只看到文字 —— 用户以为「模型看不懂图」，实际是网关半路把图删了且不留痕迹。

rosetta v0.5.1 原生有 `BlockImage`/`ImageURL`，跨协议翻译（OpenAI `image_url` ↔
Anthropic `image source` ↔ Responses `input_image`）正是这个网关存在的意义，没有理由丢。
现 `user` 角色按块透传文本 + 图像；`system`/`assistant`/`tool` 保持纯文本（与 OpenAI 语义一致）。
无图像时仍走 `rosetta.User(text)`，与旧实现逐字节一致，不做行为变更。

**非法 URL 不需要网关再写白名单**：rosetta 的 `validate` 会拦下并返回 `ErrInvalidRequest`，
网关已有的错误映射把它变成 400（实测 `file:///etc/passwd` → 400，未开 SSRF 口子）。

## 4. 验证证据（全部在 `.workbuddy/tmp/e2e`，18099 网关 + 19090 假上游）

- **迁移**：拿 **9-23 建的旧库**（`routes` 带 `priority`、`access_keys` 带 4 个死列）直接起新二进制。
  实测 `PRAGMA table_info` 确认 5 个死列**真被 DROP**，`route_targets` 为两条已有路由各回填 position 0。
- **多模态透传**：`zzfake` 的 `/__last` 回看上游实收 body ——
  `{"content":[{"text":"描述这张图","type":"text"},{"image_url":{"url":"https://example.com/cat.png"},"type":"image_url"}],"role":"user"}`
  图像块完整保留。回归：纯字符串 → `'hello'`；纯文本块数组 → `'ABCD'`（拼接无分隔符，与旧行为一致）；
  畸形 `content: 123` → **400**（不再把 `123` 当正文发给上游）。
- **客户端断开**：`slow` 模式 + `--max-time 1` 主动断开 → `usage_records.status = canceled`
  （改为 `error` 之前的行为已不复现）。
- **负数超时**：`stream_idle_timeout_ms: -1` → 启动**退出码 1**，
  `failed to load config: defaults.stream_idle_timeout_ms: must be >= 1, got -1`（不再 panic）。
- **配对校验**：provider 与模型不互属 → 400；provider 不存在 → 400；重名 → **409**（原先 500 + 裸 SQL 错误）。
- **`/v1/models` 鉴权**：无头 401，带有效 key 200。
- **登录限速**：前 10 次 401，第 11 次起 **429 + `Retry-After: 60`**；
  冷却期内即使口令正确也 429，且耗时 1.1ms（未做 PBKDF2）。
- **Create 时间戳**：`POST /keys` → `created_at = 1790206267000`；`POST /providers` → `created_at`/`updated_at` 均非 0。
- **流式矩阵** `bash .workbuddy/tmp/e2e/run-matrix.sh` 四种模式全部符合预期：
  `ok` → `[DONE]` + `finish_reason` + status `ok`；`silent` → 仅 `: keepalive`、status `truncated`（latency 3001ms）；
  `no_done_hold` → 有内容但无收尾、status `truncated`；`thinking` → 流式 `delta.reasoning_content`
  与非流式 `message.reasoning_content` 均有值。`check_sse.py` 对四个 raw 文件判定全部 `VERDICT=clean`
  （`corrupt_lines=0`，含 keepalive 与数据帧交错的 `no_done_hold`）。
  用量尾块已带 `prompt_tokens_details.cached_tokens: 4`。
- **单元测试反证**：`internal/server/throttle_test.go` 在旧实现下 3 个用例 FAIL（见 2.1）；
  `TestFailover_NonStreamSwitchesOnUpstream404` / `TestFailover_SingleTarget404MapsToModelNotFound`
  在旧的 404 判定下双双 FAIL（见 2.4）。
- **故障转移**（`python .workbuddy/tmp/e2e/failover-test.py`，13 项断言全过）：
  链 `[fakeanth(必 404) , fake(ok)]`、`failover=on max_targets=2` → **200 且假上游 `/__last` =
  `fake-model`**（证明真的转移了；修复前是 502 且 `/__last` = `fake-claude`）；
  `failover=off` → 只打链首、对外 **404 `model_not_found`**；按模型给链首注入 `http500` → 同样转移；
  **流式路径同样转移**（下游收到链尾的完整流并带 `[DONE]`）。单目标链下六种注入状态码映射
  全部符合 DESIGN §9 表格：`http429`→429 `rate_limit_exceeded`、`http500`→502 `upstream_error`、
  `http404`→404 `model_not_found`、`http402`→502 `upstream_quota_exhausted`、
  `http401`→502 `upstream_auth_error`、`http400`→400 `invalid_request_error`。
  每条用例前 reload（清掉上一条留下的凭据冷却/目标熔断）保证结论不被前置状态污染。
- **目标熔断**（`bash .workbuddy/tmp/e2e/breaker-test.sh`）：`failure_threshold=2` 连打 4 次，
  网关日志出现 `target circuit opened target=0b67e213… threshold=2 until=+60s`，
  且 4 次请求对外都是 200 —— 熔断器不是死配置，404 确实走了 `RecordTargetFailure` 记账。
- **假上游能力扩展**（`.workbuddy/tools/zstream/zzfake`）：新增 `httpNNN` 错误注入与
  `?model=<上游模型名>` 按模型覆盖模式。后者是验证链式故障转移的**必要条件** ——
  链上各目标发来的 model 名不同，只改全局模式会把整条链一起打掉，根本测不出转移。
- **静态检查**：`go build ./...` / `go vet ./...` 均无输出；`go test ./... -count=1` 全过；
  `npm run typecheck` 通过；管理界面四页截图核对（`overflowX: 0`，无窄屏溢出）。



---

# 全局复审 + 高优先级修复 — 2026-10-01

触发点：对全项目做一次整体全局审计（性能 / 功能 / 正确性），随后修复其中高优先级的 5 项。
审计范围覆盖 Go 全部 50 个源文件 + Vue 前端 + 构建链路；此处只记录本轮修掉的高优先级项，
性能优化与功能增强建议（synchronous=NORMAL、鉴权 O(N)、Anthropic 入口、response_format 透传、
RPM/TPM 限速、审计日志、CI 校验 dist 同步等）待后续排期。

## 0. 结论速览

| # | 问题 | 严重度 | 状态 |
|---|---|---|---|
| 1 | /v1 请求体超限被内层 LimitReader 静默截断，413 永远变 400 | **P1** | 已修复 |
| 2 | 池重建「先清空再查库」，ListProviders 失败留下空池 → 全站 /v1 选不到上游；并发 reload 交错 | **P1** | 已修复 |
| 3 | 配置生效完全依赖前端调 reload：绕过前端的调用方写完不生效，禁用 Key 后 auth 读旧快照照常放行（安全窗口） | **P1** | 已修复 |
| 4 | `ListModelThroughput` 全历史窗口函数挂在 GET models 上，重查询阻塞唯一 DB 连接；缓存命中率对同一区间重复全扫 | **P1** | 已修复 |
| 5 | `http.Server.WriteTimeout=5min` 从响应起算，会硬切超过 5 分钟的正常流式（来历不明的 truncated）；无 ReadHeaderTimeout/IdleTimeout | **P1** | 已修复 |

## 1. 修复详述

### 1.1 请求体超限报 413（inwire）

`DecodeOpenAIChatRequest` 原来读 `LimitReader(r.Body, maxBytes)`——LimitReader 到点即停不报错，
外层 MaxBytesReader 永远等不到越界读，超限 body 被截断后 json 报 `unexpected end of JSON input`（400），
`handleChatCompletions` 里 `errors.As(*http.MaxBytesError)` 的 413 分支是死代码。
现改为读 `maxBytes+1` 后显式判长度，超限直接返回 `&http.MaxBytesError{Limit: maxBytes}`；
外层中间件先行报错的路也汇到同一个 413。回归测试：`internal/inwire/openai_chat_test.go`
（超限报 MaxBytesError / 恰好满额正常解码）。

### 1.2 池重建原子化（upstream）

`BuildFromStore` / `BuildFromConfig` 原来第一步就 `p.providers = make(...)` 清空，之后任何失败
都留下空池。现拆为 `PrepareFromStore`（构建完整局部表，不触碰运行状态；单 provider 级失败仍只跳过并记日志）
+ `Install`（锁内一次性换入新表并清零目标熔断表）。构建失败 → 旧池原样保留，照常服务。
回归测试：`internal/upstream/pool_test.go` 的 `TestBuildFromStore_ErrorKeepsOldPool`
（用已关闭的 store 制造失败，断言旧池仍可用、熔断表未被误清）。

### 1.3 配置生效服务端化：AutoReload + runtimeReloader（server / cmd/gateway）

新增 `server.AutoReload` 中间件：管理写方法（POST/PATCH/PUT/DELETE）响应 2xx 后就地触发重建；
GET、4xx/5xx、`/admin/api/reload`（自身会重建）、`password/set`（不涉运行时）、
`/test` 与 `/discover`（只读探测）均跳过。重建与请求生命周期解耦
（`context.Background()` + 30s 超时），失败只 ERROR 留痕（响应已发出无法改写），
运行时与库的分叉由下一次写操作或手动 reload 收敛。

新增 `runtimeReloader`（cmd/gateway）：`sync.Mutex` 串行化「池重建 + 快照重建」，
先 `PrepareFromStore` 与 `RebuildFromDB` 各自完整构建，两边都成功才依次原子替换——
任一步失败运行时保持旧状态，消除「池新快照旧」的分叉。启动流程、`POST /admin/api/reload`、
AutoReload 三者共用同一把锁。前端 `mutate()` 的 reload 调用保留为兜底。
回归测试：`internal/server/autoreload_test.go`（触发矩阵 + reload 失败不影响响应）。

### 1.4 用量重查询加界（store）

- `ListModelThroughput` 统计范围限定近 30 天（`throughputWindow`）：窗口函数要对全历史排序，
  调用量大的 provider 积累几十万行后，每次打开模型页都是一次重查询，且持着唯一的
  DB 连接（`SetMaxOpenConns(1)`）阻塞配额预检与用量写入。`idx_usage_prov_ts` 可直接服务该范围。
- 缓存命中率并入 `GetUsageStats` 同一条 SELECT（JOIN 至多 1:1，不会因行复制失真），
  独立的 `CacheHitRate` 方法（唯一调用方是 stats 页）随之删除；语义注释迁入 `UsageStats.CacheHitRate`。
- 设计口径不变：`usage_records` 仍不做保留/归档策略（DESIGN §4），本轮只约束查询侧。

### 1.5 HTTP 超时（cmd/gateway）

`WriteTimeout: 5min` → `0`：net/http 的写超时从「开始写响应」起算、覆盖整个响应时长，
长输出的慢推理流（>5 分钟）会被硬切成来历不明的 truncated。流已有 TTFT/空闲看门狗兜底，
非流式由 `upstream_timeout` 限定。补上 `ReadHeaderTimeout: 30s` 与 `IdleTimeout: 2min`
（不设 IdleTimeout 会回落 ReadTimeout 的 30s，聊天客户端思考间隙的 keep-alive 连接被反复重建）。

## 2. 验证证据

- `go build ./...` / `go vet ./...` 无输出；`go test ./... -count=1` 全过
  （含本轮新增的 inwire 2 例、upstream 1 例、server 2 例）。
- 既有回归全部保持绿：`failover_test.go` / `quota_test.go` / `throttle_test.go` /
  admin 各 handler 测试未改动、原样通过。
- 文档同步：DESIGN §4（查询侧防线）、§6.3（服务端自动 reload）、§8.1（WriteTimeout=0）、
  §10（池重建原子化）已更新。

---

# 审计跟进 — 2026-10-02（v1.4 三批：快赢 / 中等 / 收尾）

背景：2026-10-01 全局复审修掉 5 个高优先级后，剩余优化项按投入分三批落地
（`5899834` 快赢批 / `086cc5e` 中等批 / `bb79480` 收尾批）。本节补记三批的文档同步。

## 快赢批（5899834）

- SQLite DSN 增 `_synchronous=NORMAL`：WAL 模式的官方推荐搭配，用量 INSERT
  不再逐条 fsync（FULL 在 WAL 下只多保护「掉电丢最近几个已提交事务」，不涉及损坏）。
  钉住参数生效的回归测试：`TestStore_Pragmas`（驱动对 DSN 参数静默解析，写错不报错）。
- 鉴权 O(1)：`snapshot.Keys`（ID 键、无读者）改为 `KeysByHash` 哈希索引，
  `auth.Authenticate` 从全量遍历常量时间比较改为 O(1) 查表。map 查找的计时差异
  只泄露「与存储哈希的前缀匹配度」，而存储的是高熵 key 的 SHA-256 —— 前缀信息
  无法反推原像，不构成可用侧信道；DESIGN §6.2「SHA-256 索引」口径回归一致。
- config bootstrap `protocol` 白名单校验（与 admin 端 / upstream.buildClient 同一份
  名单，空串 = auto 放行）：拼错在启动时即报错，不再静默退化成 SDK 自动探测。
- usage 明细 `ORDER BY ts DESC` 补 `, id DESC` 兜底（同毫秒并发写入不再随机重排；
  分组行无单一 id，不套用）；`/admin/api/usage` 与 by-* 系列的 `from=0` 语义统一为
  「全部历史」（此前一个当「未传」一个当「全部」）。
- StatsHandler 对 tps/ttfb 查询失败补 WARN 留痕（此前 `_` 丢弃、指标静默显示 0）。

## 中等批（086cc5e）

- inwire 透传 `response_format`（json.RawMessage 原样直传，不结构往返，schema 里
  未建模字段不被吃掉）/ `seed` / `user` / `parallel_tool_calls`，经
  `ApplyProtocolPrivateExtra` 走 openai-chat 的 Extra 通道；anthropic / responses
  上游维持既有门控跳过。
- `ImportModels` 改单事务批量 upsert（`ImportUpstreamModels`）——逐条自动提交会在
  第 N 条失败时永久落下前 N-1 条；Settings 的 model_defaults + runtime_defaults
  合并为 `SaveSettings` 单事务。
- 新增 `GET /admin/api/upstream-models` 扁平聚合端点（含 provider_id，不挂吞吐
  重查询），Routes / Settings 从「1+N 个请求」降为「1 请求 + 本地分组」。
- web：`api.ts` 请求加 `AbortSignal.timeout(30s)`（网关挂起不再永久「加载中」）；
  History / Overview 加请求序号守卫（快速翻页/切范围旧响应不再覆盖新响应）。
- CI：新增 `.github/workflows/ci.yml` —— push/PR 时重建前端 + sync，与入库的
  `internal/webui/dist` 做 `git diff --exit-code`，「忘 sync 就打 tag 发旧 UI」从
  线上事故变成一次红灯。

## 收尾批（bb79480）

- Update 类 DAO 命中 0 行返回 ErrNotFound（新增 `tx.checkAffected`），各写 handler
  映射 404 —— 堵住 Get→Update 之间被并发删除仍回 200 的 TOCTOU 假成功。
- `uniqueSlug` 不再把 `GetProviderBySlug` 的 DB 错误当「slug 可用」，上抛 500。
- 删死代码：`adminauth.Clear()`（全仓无调用点）、入站 `OpenAIChatRequest.Extra`
  （自述 always nil 的死字段；透传走的是 rosetta.ChatRequest.Extra，不受影响）。
- `extractUserContent` 改 `strings.Builder`，去掉循环内拼接的 O(n²)。
- `build.ps1` 从 web/package.json 读版本并 `-ldflags` 注入 `main.buildVersion`
  （本地二进制不再恒为 "dev"）；`gateway.ps1` 端口解析兼容 IPv6 监听地址。
- 前端：index.html 内联脚本首帧前打 `data-theme`（消暗色闪屏）；顶栏退出登录；
  同步内嵌产物。

## 验证证据

- `go build ./...` / `go vet ./...` 无输出；`go test ./... -count=1` 全绿
  （含 `TestStore_Pragmas`）。
- 文档同步：DESIGN §6.3（upstream-models 端点、from=0 口径）、§13.1（CI 产物校验、
  build.ps1 版本注入）。

## 仍未做（大件，待排期）

| 项 | 说明 |
|---|---|
| Anthropic `/v1/messages` + `/v1/responses` 入口 | 最大的功能缺口：Claude Code 尚无法直连；`/v1/models` 形状分流与别名路径、`?include=upstream` 同属此批 |
| RPM/TPM 限速 | DESIGN §11.4 方案已写好（内存固定窗口 + Key 维度），纯实现活 |
| 写操作审计日志 | DESIGN §13.3 承诺（谁/何时/改了什么），未实现 |
| SQLite 读写连接分离 | 消统计重查询与热路径互斥；查询加界后紧迫性下降 |
| 安全硬化 | 管理密码仅 6 位下限；主密钥 env 口令场景无 KDF（自动生成的 master.key 不受影响） |
| History 过滤/导出 | 按状态/模型/Key 过滤 + CSV 导出 |
