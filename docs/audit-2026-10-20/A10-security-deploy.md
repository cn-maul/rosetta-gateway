# A10 安全与部署面审计

范围：`rosetta-gateway`

审计日期：2026-10-20

边界：只读审计。除本报告外未修改仓库文件，未执行 git 写操作，未启动会写状态目录的服务。

## 0. 结论摘要

本次共确认 **P0 = 1，P1 = 8，P2 = 7，P3 = 6**。

核心判断：鉴权、CSRF、SQL 参数化、静态文件服务和上游重定向控制的基础不差，但“推荐的 Docker 快速开始默认公开监听 HTTP”把整套管理员凭据与 API key 放在明文网络上。这不是单纯少写一句部署提示，而是代码默认值、容器映射、Cookie 属性与文档推荐共同形成的可执行部署路径。

其余 P1 集中在：引导令牌长期留在日志、反代后客户端 IP 契约缺失、SQLite 状态权限、`/healthz` 错误回显、发布镜像不校验 embed 前端、发布镜像绕过 `-race` 门禁、备份不完整、依赖与镜像不可复现。A3 已报告的“上游错误日志未统一脱敏”不在 A10 重复计数，只作为交叉依赖引用。

## 1. 审计方法

1. 使用 `xerj_map` 确认项目索引；只查询 `rosetta-gateway-docs`，未查询 `ax-*` 或 `*`。
2. 使用 `xerj_search` 定位入口、密钥、容器和发布路径，再用 `rg` 核精确字符串与行号。
3. 逐项检查 HTTP 入口、认证中间件、管理 handler、crypto、store、Docker、workflow 和部署文档。
4. 执行只读验证：`go mod verify`、`npm audit --omit=dev --json`、`go doc crypto/rand.Read` 与 GOROOT 源码核对。
5. 使用 `git status --short --branch` 对照初始状态；本报告位于 ignored 的 `work/`，未进入版本控制。

## 2. P0（安全 / 凭据暴露）

### P0-1　推荐 Docker 部署默认跨主机明文 HTTP，管理员会话和上游 key 可被同网段截获

**判定：P0。** 这是默认可执行部署路径上的凭据泄露，不是“用户自行关闭 TLS”的假设场景。

证据链：

- `docker/config.default.json:1-3` 将容器默认监听设为 `"0.0.0.0:8666"`；容器入口在 `docker/docker-entrypoint.sh:40-45` 首次启动时直接复制该配置。
- `README.md:34-50` 的“Docker（推荐）”示例使用 `-p 8666:8666`，随即引导用户打开 `http://<主机>:8666/admin/`；`DOCKER.md:19-35` 给出同样的公开 HTTP 快速开始。
- `cmd/gateway/main.go:668-690` 只调用 `srv.ListenAndServe()`，没有内置 TLS 配置；监听地址直接来自 `cfg.Listen`。
- `internal/server/user_auth.go:399-411` 的会话 cookie 只有 `HttpOnly` 与 `SameSite=Strict`，代码注释明确说“刻意不设 Secure：局域网是 HTTP”。因此凭据不会因 `Secure` 被浏览器限制在 TLS 通道。
- `README.md:93-101` 和 `DESIGN.md:1703` 把 TLS 责任交给反代，但快速开始没有强制要求反代，也没有将公开绑定降为回环。
- `internal/admin/user_handler.go:280-305` 登录响应同时返回 JWT，`internal/server/user_auth.go:400-410` 另写 cookie；同一份会话在 HTTP 下可被同网段观察。

可利用性与影响：

同一广播域或可路由网段中的被动观察者可截获管理登录、Bearer 会话、bootstrap 请求及下游 `Authorization` 头。若上游凭据经管理 API 读取，攻击者还可能获得对上游账户的控制。`DOCKER.md:220-222` 只在后文建议“若要挂公网”时绑回环，不能抵消前面的推荐示例。

修复方向：

- 快速开始改为 `-p 127.0.0.1:8666:8666`，跨主机访问必须先配置 TLS 反代。
- 默认配置或启动检测在非回环监听且未声明“已由可信反代终结 TLS”时拒绝公开管理面，或至少强制显示一次性危险确认。
- 当请求由可信 HTTPS 反代进入时设置 `Secure`；不要把 `Secure` 无条件写死，否则局域网 HTTP 兼容性会破坏。

## 3. P1（确定缺陷）

### P1-1　一次性 bootstrap token 以明文写入长期日志，日志轮转/聚合后仍可恢复管理员初始化权

证据：

- `cmd/gateway/bootstrap_admin.go:240-248` 在启动时用 `logger.Warn` 输出完整 `"token", token`。
- `README.md:45-50` 和 `DOCKER.md:185-220` 把“从 `docker logs` 取引导令牌”作为标准操作；`DOCKER.md:213-218` 还说明日志可能随轮转和聚合平台长期留存。
- `cmd/gateway/bootstrap_admin.go:289-312` 用 32 字节 `crypto/rand` 生成令牌，生成强度正确；`cmd/gateway/main.go:281-295` 在保护布置失败时拒绝启动，令牌消费事务和恒定时间比较见 `internal/store/user_dao.go:331-422`。

问题不在 token 强度，而在“唯一持有证明”被复制进日志系统。能读容器日志、日志归档、集中式日志或备份的人，可能在引导窗口仍开放时抢先设密。即使窗口已关闭，历史日志也应视为敏感凭据。

修复方向：日志只打印 token 的存储位置/一次性取用命令，不回显值；或仅在显式调试模式输出，并把推荐部署改成外部 secret 注入。不要在普通 `INFO/WARN` 中输出完整秘密。

### P1-2　反向代理后没有可信客户端 IP 契约，IP 白名单和登录限速退化为代理地址

证据：

- `internal/server/server.go:187-204` 的 `clientIP` 只解析 `RemoteAddr`，注释明确不读取 `X-Forwarded-For`。
- `internal/auth/auth.go:95-104` 和 `:169-181` 的 key IP allowlist 也只取 `RemoteAddr`。
- `internal/server/server.go:206-219` 对同一 IP 连续失败使用 10 次/1 分钟冷却；`internal/admin/user_handler.go:195-225` 登录按 `server.ClientIPOf(r)` 计入失败。
- `README.md:93-101` 又推荐把网关放在反代后，但配置中没有“可信代理 CIDR/代理层注入真实 IP”的契约。

影响：

反代后所有客户端共享代理的 `RemoteAddr`。IP 白名单无法表达“允许真实客户端 A、不允许 B”；一个恶意用户触发的失败会让同一反代后的所有用户进入登录冷却。相反，直接信任任意 XFF 又会使白名单可伪造，因此问题不是“应该读 XFF”，而是缺少“只从明确配置的可信代理解析”的设计。

修复方向：增加显式 `trusted_proxies`（CIDR）配置，只对来自可信代理的请求解析标准 `Forwarded`/XFF，并定义多级代理的右侧取值规则；未配置时保持当前的 `RemoteAddr` fail-safe。

### P1-3　SQLite 状态目录和数据库文件权限过宽，敏感数据可能被同机其他账号读取

证据：

- `internal/store/store.go:20-35` 用 `os.MkdirAll(dir, 0755)` 创建数据库目录，且没有对数据库、WAL、SHM 文件设置严格权限。
- 数据库中的敏感字段包括：`password_hash`（`internal/store/store.go:267-290`）、`access_keys.key_hash`（`:221-241`）、`provider_credentials.api_key_enc`（`:157-170`）、`app_settings.bootstrap_token`（`internal/store/settings_dao.go:45-52`）和 `audit_log`（`internal/store/store.go:415-426`）。
- 对照：`internal/crypto/crypto.go:109-153` 的 master key 临时文件使用 `0600`；`internal/userauth/session.go:145-159` 的 session secret 使用 `0600`。
- `internal/crypto/crypto.go:57-81` 和 `internal/userauth/session.go:119-159` 对已存在的密钥文件读取后没有检查/修复权限；因此“密钥文件安全”不能覆盖既有部署中的宽权限。

影响：

Linux 常见 umask 下 SQLite 文件可能是 `0644`，同机其他本地账号可复制数据库，离线尝试弱密码或读取尚未消费的明文 bootstrap token。即使上游 key 是 AES-GCM 加密，数据库权限仍是可被离线攻击的边界；如果部署目录被共享或误配置，问题会被放大。

修复方向：数据库目录默认 `0700`，数据库及 WAL/SHM 在创建后 `0600`；启动时检查既有文件权限并在过宽时拒绝启动或明确告警；同时检查 `master.key`/`session_secret` 的既有权限。

### P1-4　免鉴权 `/healthz` 回显底层 DB 错误，泄露路径和驱动内部信息

证据：

- `cmd/gateway/main.go:641-660` 在 `db.Ping` 失败时返回 `{"status":"unhealthy","error": err.Error()}`，状态码 503；该路径没有鉴权保护。
- `DOCKER.md:352-358` 却声称 `/healthz` “不返回任何业务数据，只暴露一个状态码”。
- `internal/store/store.go:91-101` 的 `Ping` 会分别包装写池和读池错误；错误字符串可包含 SQLite/驱动细节、数据库路径或底层 I/O 信息。

影响：外部探针和同网段攻击者可利用错误文本做环境枚举和故障定位，至少违反“健康检查只暴露布尔状态”的文档契约；是否包含明文机密需要取决于驱动错误内容，标为“待确认”，但路径/错误细节本身已足以构成确定缺陷。

修复方向：对外只返回稳定的 `status` 和 `error_code`，将完整错误写入服务端日志；如确需诊断信息，使用仅回环可访问的独立端点。

### P1-5　Docker 发布 workflow 不校验 embed UI，tag 或任意手动触发可发布旧/不同步的前端

证据：

- `.github/workflows/docker.yml:26-57` 的发布前门禁只执行 `gofmt`、`go build`、`go vet` 和 `go test`。
- `.github/workflows/docker.yml:36-40` 注释明确承认未把 `webui-sync` 纳入 tag 流程。
- `.github/workflows/docker.yml:6-10` 允许 `workflow_dispatch`，因此可从非 `main` 分支/提交手动构建并推送 `:edge`。
- `.github/workflows/ci.yml:163-233` 的 `webui-sync` 才执行 `npm ci`、类型检查、前端测试、构建、`node sync-embed.mjs` 和 `git diff --exit-code`。
- 镜像构建 `Dockerfile:37-40` 只 `COPY . .` 并编译 Go，不会自己重建或校验 embed 产物。

影响：tag 流程和手动发布可能把与 `web/src` 不一致的旧 UI 打包进真实镜像。虽然代码测试可能通过，用户看到的管理界面仍可能缺少安全修复（例如登录/权限页面的前端检查），这是确定的发布一致性问题。

修复方向：将 `webui-sync` 作为发布 workflow 的必需 job，或让镜像构建从源码重建并禁止直接使用未校验的 `internal/webui/dist`；手动发布限制到受保护分支，并在运行时输出来源 commit/SHA。

### P1-6　文档备份只备份 SQLite，恢复不了 `master.key`、会话密钥和配置

证据：

- `DOCKER.md:299-319` 的备份示例只用 `VACUUM INTO` 复制 `/data/data/gateway.db` 并打包。
- `README.md:65-77` 明确列出状态包括 `config.json`、`master.key`、`session_secret` 和 `data/gateway.db`；`master.key` 丢失会使全部上游凭据无法解密，`config.json` 决定启动参数。
- `DOCKER.md:123-127` 也要求 `master.key` 与 `session_secret` 随 `/data` 持久化，但恢复章节没有把它们纳入备份包。

影响：按文档片段恢复时，数据库能打开但上游凭据可能全部解不开，旧会话失效，配置也可能回落到默认值。对灾备而言这是确定的数据不可恢复问题，且与“状态目录是关键资产”的文档承诺冲突。

修复方向：备份包必须同时包含 `/data/config.json`、`master.key`、`session_secret` 和数据库；对包含密钥的备份使用加密、访问控制、保留期限和恢复演练；备份前明确停止写入或使用一致性快照。

### P1-7　Docker 发布 workflow 绕过竞态检测，tag 镜像可在 `-race` 未通过时发布

证据：

- `.github/workflows/ci.yml:160-161` 把 `go test -race -count=1 -timeout 900s ./...` 作为独立的竞态门禁；同文件 `:136-159` 明确记录该项目并发密度较高，且历史上曾由 `-race` 抓出真实数据竞争。
- `.github/workflows/docker.yml:28-40` 说明发布 workflow 只“就地复跑最小门禁”，但没有保留主 CI 的 race 检查；`:47-57` 的实际步骤只有 `gofmt`、`go build`、`go vet` 和普通 `go test`。
- `.github/workflows/docker.yml:6-10` 允许手动触发，`:113-128` 在门禁通过后直接 `push: true`。因此 tag 或手动运行指向的提交可以在未执行 race 检查的情况下进入 GHCR。
- `.github/workflows/ci.yml` 与 `.github/workflows/docker.yml` 是两个独立 workflow，docker workflow 没有 `needs` 或对主 CI 结果的显式查询；发布门禁的注释本身解释了这一点。

影响：

数据竞争通常只在并发交错、取消、后台重载或测试替身等路径暴露，普通 `go test` 可能稳定通过。把镜像发布门禁降为普通测试，会让 tag / 手动发布的制品带上未经过项目专项并发验证的提交；对网关这种同时运行请求 goroutine、重载器、探测与用量 worker 的进程，这是确定的发布门禁缺口，而不是对某次构建存在数据竞争的断言。

修复方向：发布前执行与主 CI 相同的 `go test -race -count=1 -timeout 900s ./...`，或者让发布 job 依赖一个可复用、不可绕过的完整质量门禁；手动发布也应受相同门禁约束。

### P1-8　发布依赖与可复现性缺口：lockfile 版本漂移，Docker 基础镜像使用浮动 tag

证据：

- `web/package.json:4` 是 `"version": "1.6.5"`，但 `web/package-lock.json:3,9` 仍写 `"version": "1.6.0"`；`npm ci` 会按 lockfile 安装依赖，但项目元数据与版本声明已经漂移。
- `Dockerfile:9` 使用 `ARG GO_VERSION=1.27`，`go.mod:3` 要求 `1.27.1`；构建阶段 `Dockerfile:22` 拉取 `golang:${GO_VERSION}-alpine` 浮动标签。
- 运行阶段 `Dockerfile:41` 使用 `alpine:3.21` 浮动标签，没有 digest 固定；同一 tag 未来可能指向不同基础层。
- `.github/workflows/docker.yml:83-128` 推送 GHCR 镜像，但没有输出可验证的构建 provenance/SBOM；action 本身已固定 commit SHA，这一正面事实保留。

影响：生产镜像与源码/依赖的对应关系不够强，无法保证几个月后重建得到同一基础环境和依赖树。此条定级为 P1 是因为发布面可复现性直接影响安全修补和事故回溯；它不等同于已知可利用的 CVE。

修复方向：同步 `package-lock.json` 的项目版本并提交；Docker 基础镜像按 digest 固定（至少发布 tag 固定）；生成 SBOM/Provenance 并保存镜像 digest；在发布 job 明确校验 `go.mod` 与构建镜像的 Go 版本。

## 4. P2（架构 / 设计一致性）

### P2-1　master key 只有 SHA-256 派生，没有版本化 KDF/轮换 envelope

`internal/crypto/crypto.go:33-91` 对环境变量和文件内容都执行 `sha256(raw)`；`WeakMaterial` 只在 `:255-263` 做短材料告警，不能防止低熵口令被离线爆破。当前自动生成的 32 字节随机 key 足够强，且改派生方式会锁死存量密文，因此不应直接改成不可逆的新公式；但应设计带 key id/version 的 envelope、逐步迁移工具和轮换流程。

### P2-2　容器降权失败后继续以 root 运行

`docker/docker-entrypoint.sh:49-83` 在无法修复 `/data` 属主时只打印警告，随后 `exec /app/gateway` 以 root 运行。文档 `DOCKER.md:335-341` 也承认这一行为。安全默认应是拒绝启动或要求显式 `ALLOW_ROOT=1`；当前行为使“容器是加固的”依赖运维是否阅读 stderr。

### P2-3　Docker healthcheck 写死 8666，改变监听端口会误判

`Dockerfile:75-84` 固定请求 `http://127.0.0.1:8666/healthz`，但 `docker/config.default.json:1-3` 和 `DOCKER.md:125-131` 允许修改 `listen`。只要运维改端口，健康检查会持续失败或掩盖真实状态。应由入口脚本读取/注入端口，或使用统一环境变量。

### P2-4　审计 actor 写死为 `"admin"`，普通用户写自己的资源也会被误记

`cmd/gateway/main.go:533-551` 的 audit 回调将 `Actor` 固定为 `"admin"`，代码注释也承认普通用户写操作不精确。`internal/store/store.go:415-426` 有 `actor` 字段，说明审计模型本应记录真实主体。当前结果会污染取证和责任追踪；应由认证上下文中传入 user id/role，缺失时明确写 `unknown`。

### P2-5　同源判定只比较 host，不比较 scheme

`internal/server/server.go:161-174` 取 `Origin` 后仅比较 `u.Host` 与 `r.Host`。在 HTTP/HTTPS 混合部署或反代改写 scheme 时，脚本/非浏览器调用可能被误判为同源；当前未发现直接 CSRF 绕过（浏览器 `Sec-Fetch-Site` 分支先做限制），但该契约不完整。应比较规范化 origin（scheme+host+port）并通过受信代理配置声明外部 scheme/host。

### P2-6　TLS/反代配置没有进程级契约

`cmd/gateway/main.go:668-690` 只有明文 `ListenAndServe`；没有 `tls.Config`、受信代理、外部 origin 或 forwarded header 设置。`README.md:93-101` 和 `DESIGN.md:1703` 把安全责任交给部署层，但代码无法判断它是否真的被正确包在 TLS 后。这是 P0 的架构根因之一，修复应和 trusted proxy 契约一起落地。

### P2-7　发布 workflow 的权限和触发面需要与生产分支策略对齐

`.github/workflows/docker.yml:12-15` 给 workflow 授予 `packages: write`，`:6-10` 又允许 `workflow_dispatch`；结合 P1-5 的缺失 embed 门禁，手动运行拥有写包权限的发布任务是扩大面。应对发布 job 使用 environment protection/protected tag，限制手动触发到受保护分支，并输出镜像 digest 供部署固定。

## 5. P3（nit / 防御性改进）

### P3-1　`crypto/rand.Read` 返回值被忽略，当前运行时安全但语义不够防御

`internal/crypto/crypto.go:249-252` 调用 `rand.Read(b)` 不检查返回值；`internal/admin/key_handler.go:179-182` 用它生成下游 key，`cmd/gateway/main.go:3767-3770` 也用于 ID。

当前 Go 1.27 的 `crypto/rand.Read` 实现会填满缓冲区，底层随机源失败时 irrecoverably crash，而不是返回部分全零；我核对 `go doc crypto/rand.Read` 与 GOROOT 源码均确认这一点。因此不能把本条写成“全零 master key/API key”的 P1。建议仍显式检查 error，原因有二：让旧版运行时和已替换的 `rand.Reader` 在编译层失败关闭；让读者无需了解标准库演变就能确认安全语义。

### P3-2　`.dockerignore` 没有显式排除 `.env*`

`.dockerignore:1-45` 排除了 `.git`、`work/`、`data/`、`master.key`、`session_secret`、`config.json` 和数据库，却没有 `.env*` 规则。若开发者把 `.env` 放在仓库根目录并构建镜像，文件会进入构建上下文；是否真的落入镜像取决于 Dockerfile 的 `COPY . .`（`Dockerfile:31-35`）和实际构建层。建议加入 `.env*`，同时保留文档对 secret 的明确指引。

### P3-3　`package-lock.json` 版本漂移

已在 P1-8 记录发布影响；单独的可维护性问题是 `web/package-lock.json:1-10` 的项目版本仍是 `1.6.0`，与 `web/package.json:4` 不一致。它不影响依赖解析本身，但会让 SBOM、镜像 metadata 和人工核对产生歧义。

### P3-4　健康检查文档与实现不一致

实现 `cmd/gateway/main.go:641-660` 会返回错误文本，文档 `DOCKER.md:352-358` 说只返回状态码。P1-4 记录安全影响；这一条提醒应同时修正文档与响应契约。

### P3-5　Docker 示例使用 `:latest`

`README.md:37-42` 和 `DOCKER.md:203-215` 仍使用 `:latest` 或浮动 tag 示例。生产部署应使用明确版本或 digest；这与 P1-8 的可复现性问题相关。

### P3-6　`cmd/migrate-legacy` 的路径 URI 拼接边界

`cmd/migrate-legacy/main.go:63` 直接拼接 `file:` URI，路径包含 `?` 或 `#` 时可能被 SQLite URI 解析器截断或改变语义。该工具是迁移工具，且 A6 已覆盖迁移主缺陷，这里只作为 A10 的文件/运维边界提醒，不与 A6 重复计数。

## 6. 已验证无问题的区域（含理由）

| 区域 | 结论与理由 |
|---|---|
| 管理面越权 | `internal/server/user_auth.go:119-207` 的 `AdminGateGuard` 用真实 mux 路由模式做精确匹配，未匹配路由默认 403；`internal/admin/usage_by_user_handler.go:81-101` 和 `internal/admin/usage_handler.go:175-180` 对普通用户强制按 `callerScope` 收窄，未发现“白名单前缀自动等于权限”的现行越权路径。 |
| CSRF | `internal/server/user_auth.go:323-357` 对 cookie 写请求执行同源判定；`internal/admin/helpers.go:101-148` 强制 JSON Content-Type；`internal/server/server.go:124-143` 对 `/admin` 不设 CORS。三者构成当前管理面写操作防线，未见直接绕过。 |
| CORS | `/v1` 明确放行任意跨域，`/admin` 不返回 `Access-Control-Allow-Origin`；与前端同源 SPA 设计一致。未把数据面 CORS 误用到管理面。 |
| 安全响应头 | `internal/server/server.go:458-494` 设置 CSP、`frame-ancestors 'none'`、`nosniff`、`no-referrer` 与 `no-store`；CSP 无 inline script 放行，管理页静态资源策略合理。 |
| SQL 注入 | `internal/admin/usage_handler.go:133-172` 的用户过滤值走绑定参数；`:204-240` 的 `group_by` 只匹配固定的 `day/key/model/provider` 分支；`:877-932` 的列名、表名和排序字面量只来自内部调用点。`internal/admin/usage_by_user_handler.go:100-119` 及 `internal/store/usage_by_user_dao.go:84-108` 同样使用参数与固定 SQL。A6 已覆盖 store 包的整体 SQL 结论。 |
| API key 存储与鉴权 | `internal/admin/key_handler.go:179-182` 只保存 key 的 SHA-256 哈希；`internal/auth/auth.go:61-181` 检查哈希、启用、过期、归属用户、模型白名单和 IP 白名单；管理员 key 禁止调用数据面。未发现 key 明文落库。 |
| 凭据展示脱敏 | `internal/admin/credential_handler.go:39-60,223-242` 列表/详情只返回 `api_key_mask`，不返回解密后的明文；新建响应仅在调用方主动提供时用于一次性展示，符合凭据管理语义。密码哈希在 `internal/admin/user_admin_handler.go:33-68` 的响应中不存在。 |
| 上游凭据加密 | `internal/crypto/crypto.go:202-239` 使用 AES-256-GCM、随机 12 字节 nonce 并检查 `io.ReadFull`；`internal/admin/export_format.go:159-219` 使用 PBKDF2-HMAC-SHA256 210000 次和 AES-256-GCM，salt/nonce 随机。未把 GCM nonce 写死或复用。 |
| 请求体与超时 | `cmd/gateway/main.go:662-666` 全局使用 `MaxBytesReader`，`internal/config/config.go:300-407` 对 body 上限做边界校验（默认 32 MiB，最大 512 MiB）；`cmd/gateway/main.go:668-683` 设置 Read/Header/Idle 超时。SSE 的 `WriteTimeout=0` 有说明，流由 TTFT/空闲看门狗约束，见 `cmd/gateway/main.go:671-682,1960-1977,2680-2710`。 |
| SSRF / 重定向 | `internal/upstream/upstream.go:1088-1099` 明确禁止跟随重定向；`internal/upstream/balance.go:230-245` 同样使用 `http.ErrUseLastResponse`。管理员可配置上游 endpoint 是产品能力，未把上游响应原样回显给数据面；模型发现非 200 响应只在 server 日志记录 body preview（见 A3 的 P2-3）。 |
| 媒体 URL | `internal/inwire/openai_chat.go:530-590` 和 `internal/inwire/openai_responses.go:138-150` 只把 `image_url/file_url/audio_url` 透传给上游，没有发现网关主动抓取 URL 的下载器或 `http.Get` 路径，故没有新增媒体 SSRF 面。 |
| 命令/模板注入 | 生产 Go 路径未发现 `os/exec`、`exec.Command`、`text/template` 或 `html/template` 执行；命中仅在测试文件。静态资源由 `cmd/gateway/main.go:579-627` 从 embed FS 提供并使用路径白名单，未见目录列表或明显路径穿越。 |
| Docker 构建上下文 | `.dockerignore:8-45` 排除 `.git`、`work/`、数据目录、数据库、`master.key`、`session_secret`、`config.json` 和备份；镜像不会因常规构建上下文直接带入这些文件。`Dockerfile:31-40` 的构建只编译 Go，符合项目归档 embed 产物的设计。 |
| CI 基础门禁 | `.github/workflows/ci.yml:119-161` 有 `go mod tidy -diff`、`go mod verify`、全量测试和 `-race`；`.github/workflows/docker.yml:26-57` 发布前也跑 Go 构建/静态检查/测试。 |
| Actions 供应链 | `.github/workflows/ci.yml` 和 `docker.yml` 的主要第三方 actions 使用完整 commit SHA，而不是浮动 `@v*`；这是可保留的正面控制。 |
| 依赖验证 | 本次 `go mod verify` 输出 `all modules verified`；`npm audit --omit=dev --json` 报告生产依赖 `0` 个漏洞（审计时点）。这不证明所有开发依赖安全，也未代替未来漏洞扫描。 |
| 关闭与资源释放 | `cmd/gateway/main.go:823-886` 先停止后台写入循环，执行 10 秒 HTTP shutdown，再 drain usage；未发现关闭阶段无条件泄露请求 goroutine 的新增安全缺陷。 |

## 7. 与 A5/A6/A7/A11 的交叉契约

- **A5 上游出站网络**：余额查询与模型发现忽略 provider 的出站网络是 A5 已报告的 P2（`A5-upstream-runtime.md:313-335`）。A10 只记录其安全交叉点：模型发现/余额路径应继续禁止重定向，且不要把上游响应体回显给终端用户；不要把 A5 的通用代理连通性缺陷重复算作 A10。
- **A6 存储/迁移**：A6 已覆盖 `migrate-legacy` 的 schema/迁移缺陷（`A6-store-schema-dao.md:182-212`）；A10 只补充状态文件权限与备份契约，不重复计入 A6 的迁移主问题。
- **A7 鉴权**：A7 已报告角色/状态更新与会话失效非原子、Logout 吊销失败仍返回 200、`sameOrigin` 未比较 scheme、bootstrap 失败不审计和 prune 的 `keep_days` 不审计（`A7-admin-auth.md:185-191`）。A10 不重复计这些缺陷；可信客户端 IP 的独立证据在本报告 P1-2，并把 A7 的 `A7-05` 作为部署契约交叉引用。不要把“拒绝任意 XFF”误报为漏洞，缺的是只信可信代理的配置。
- **A11 CI**：A11 已报告发布 job 绕过 race 与前端同步门禁（`A11-tests-ci.md:161-171,250-261`）。A10 的 P1-5 和 P1-7 是从镜像安全与发布制品面的独立确认；最终修复应与 A11 的可复用完整门禁合并，A10 不把具体测试覆盖漏洞重复计数。

## 8. 待确认项

1. **`/healthz` 错误文本是否实际包含绝对路径或数据库名**：错误包装会保留底层 SQLite/OS 错误，具体文本依赖部署与驱动。无论如何回显 `err.Error()` 已违反文档契约，因此 P1-4 不依赖该项。
2. **bootstrap token 的日志保留策略**：不同 Docker、Kubernetes、CI 日志系统轮转/复制策略不同；若平台禁止日志聚合且立即完成初始化，风险窗口会缩小。但把秘密写给普通日志仍是确定的敏感信息扩散。
3. **反代部署的真实客户端 IP**：当前仓库没有 Nginx/Caddy/Traefik 模板，无法验证某一种反代是否已配置 XFF。代码层面确定的是不会解析它，故 P1-2 的修复需要新增明确契约而非猜测现有代理配置。
4. **管理后台错误体中的上游响应**：`internal/admin/model_handler.go:664-667`、`internal/admin/provider_handler.go:430-443` 可能把探测错误文本返回给管理员；管理员本身拥有配置权，未将其单独列为越权，但应结合 A3 的日志/错误脱敏修复统一处理。
5. **依赖漏洞时效**：`npm audit` 与 `go mod verify` 是本次时点的只读结果，不代表未来无漏洞；本报告没有安装 `govulncheck`，也没有执行联网镜像扫描。

## 9. 结论与优先级

先修 P0-1：把推荐部署改成回环+TLS 反代，并让“公开监听没有 TLS 声明”在进程启动时成为显式的危险配置。随后按这个顺序处理 P1：bootstrap token 日志（P1-1）、可信客户端 IP（P1-2）、数据库/密钥文件权限（P1-3）、`/healthz` 错误体（P1-4）、embed 发布门禁（P1-5）、完整备份（P1-6）、race 发布门禁（P1-7）、依赖/镜像可复现性（P1-8）。

本节不把 A3 的上游错误日志未脱敏重复计入 A10 的 P1；它与 A10 的日志面修复应一起做，避免出现“bootstrap 日志脱敏了，但另一条上游错误仍把 key 写进日志”的遗漏。
