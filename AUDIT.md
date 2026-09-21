# 审计与修复记录 — 2026-09-21

本轮针对「多阶段开发累积的不一致」做了一次全面审计。触发点是局长报的一个具体故障：
**访问管理后台，输入密码 test123 后又要求输入，一直重复。**

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
| 11 | 凭据冷却机制未接线，失效凭据永不被摘除 | **P1** | 未修复（§4.1） |
| 12 | 路由 `priority` 在解析时完全没被使用，同名路由互相覆盖 | **P1** | 未修复（§4.2） |
| 13 | 两处 `extra_json` 与 `routes.fallback_route_id` 存了但从不使用 | **P2** | 未修复（§4.3） |
| 14 | `access_keys.quota_tokens` 既无写路径也无校验（假配额字段） | **P2** | 未修复（§4.4） |
| 15 | 若干死代码与契约瑕疵 | **P2** | 部分修复（§4.5） |
| 16 | 主密钥只认环境变量：**双击 exe 启动即无密钥，API Key 明文落库** | **P1** | 已修复（§3.9） |

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

---

## 4. 未修复项（需要局长决策或后续排期）

以下问题都已核实，但**改动面大或涉及产品语义**，一次全改会引入回归且难以验证，
因此明确列出而不是偷偷绕过。

### 4.1 凭据冷却未接线（P1）

`upstream.MarkCredentialCooldown` / `MarkCredentialError` 全仓库**无调用者**。
也就是说：某条凭据被上游拒绝（401/429）后不会被摘除，也不会进入冷却，
每次请求仍然可能选到它。`CredentialEntry.Status` / `CooldownUntil` 字段形同虚设。

**建议**：在 `handleChatCompletions` 的错误分支接线 —— 上游返回鉴权/限流类错误时调用对应方法，
并让 `selectWeighted` 跳过冷却期内的凭据。

### 4.2 `priority` 在路由解析时完全没被使用（P1）

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

### 4.3 三处配置字段存了但从不使用（P2）

- `routes.fallback_route_id`：库里存、后台能选、`snapshot/rebuild.go` 搬进快照，
  但 `routing.Resolve()` 从不读取 —— **兜底路由是个假开关**。
  （顺带说明：因为功能不存在，我**没有**给它加「不允许指向自己」的校验，
  给一个不工作的功能加校验会让人误以为它能用。）
- `routes.extra_json`：同上。
- `upstream_models.default_extra_json`：同上。

**现象**：后台那个「透传参数」输入框填了不生效，且没有任何提示。
**建议**：要么接线（在 `ToRosetta()` 之后合并进 `Extra`），要么从界面和 schema 里摘掉 ——
现在这样最糟：给了用户一个假的开关。

### 4.4 `access_keys.quota_tokens` 是假配额（P2）

`quota_tokens` 列在 schema 里（`NOT NULL DEFAULT 0`）、被 `ListAccessKeys` / `GetAccessKey` 读出、
出现在 `keyResponse.quota_tokens` 和前端 `types.ts` 的 `AccessKey` 里 —— 但：

- **没有任何 INSERT / UPDATE 语句写它**（`UpdateAccessKey` 只更新 `name` 和 `enabled`）；
- **没有任何地方校验它**（`auth` 包只校验 key 哈希与 enabled）。

也就是说这是个「能读、不能写、不生效」的三无字段。

**我刻意没有给它加写入口**：加上去会让它看起来像个能用的配额功能，
而实际上没有任何强制逻辑。要么把校验也补上，要么把这个字段和前端类型一起摘掉。

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

## 6. 排障手册

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
