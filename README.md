# Rosetta Gateway

局域网 LLM 网关：把多个 AI 服务商挡在身后，对外只暴露干净的虚拟模型名与统一的接入点。

上游内核是 [rosetta](https://github.com/cn-maul/rosetta)（三协议上游调用、统一 usage、
传输层重试与脏数据容错）；网关在其上补齐「管起来」的部分——多协议下游入口、
多用户与权限、配额限速、故障转移、用量记账与归档、Web 管理界面，
交付形态是**单个二进制 + 一个 SQLite 文件**。

## 功能总览

- **三协议下游入口**：OpenAI Chat（`/v1/chat/completions`）、OpenAI Responses
  （`/v1/responses`）、Anthropic Messages（`/v1/messages`），客户端零改造接入
  （Cherry Studio / OpenWebUI / Claude Code / Codex CLI 等）。
- **双轨模型命名**：虚拟模型名（主）+ `provider/model` 直连（快捷），
  不同 provider 的同名模型互不干扰。
- **链式故障转移**：一条路由挂一条有序上游链，目标熔断 + 凭据冷却 +
  half-open 探测，写出任何字节前自动转移。
- **多用户与权限**：users 表是唯一身份来源（JWT 会话 + `auth_version` 栅栏），
  分组模型白名单，key 级收紧不放宽，管理写操作全量审计。
- **配额与限速**：用户级 / key 级两级终身 token 配额（原子预占）+
  RPM/TPM 固定窗口限速。
- **余额闸门**：用户级人民币余额（亚分精度累积、待结算账本），
  耗尽即 402 拒绝调用；与 token 配额是两条独立的闸，处置动作不同。
- **用量记账与归档**：逐请求明细（含固化费用）保留 30 天，
  日归档 + 终身累计单行表保证「明细可删、累计不变」；
  历史费用按落库时单价固化，改价不改历史。
- **上游模型探测**：定时对每个上游模型发最小真实推理，得出连通率曲线，
  画在「可用模型」页的模型卡上（探测不扣网关用户的钱，也不写熔断状态）。
- **Web 管理界面**：Vue 3 + Vite，产物经 `go:embed` 进二进制，无外部静态资源。

## 快速开始

### Docker（推荐）

```bash
docker run -d \
  --name rosetta-gateway \
  -p 127.0.0.1:8666:8666 \
  -v rosetta-gateway-data:/data \
  --restart unless-stopped \
  ghcr.io/cn-maul/rosetta-gateway:latest
```

打开 `http://127.0.0.1:8666/admin/`，首次进入是**一次性**的「首次设置密码」表单
（设完即永久关闭免鉴权窗口），设完即进入后台。

表单需要填**引导令牌**。推荐用 `ROSETTA_GW_BOOTSTRAP_TOKEN` 预先指定
（≥ 32 字节，值由部署者自己持有）；未指定时网关会生成一枚随机令牌，
**只从数据库读回**（不打印到日志）—— 见 [DOCKER.md](DOCKER.md) 的取用命令。
原因：容器内监听 `0.0.0.0`，而完成引导的端点是免鉴权的 —— 没有令牌时
「谁先连到端口谁就是管理员」。
详见 [DOCKER.md](DOCKER.md)（含令牌生命周期与备份注意事项）；容器部署的目录布局、
挂载方式与升级流程也在那里。

> **端口映射默认绑回环（`127.0.0.1:8666:8666`）是刻意的**（2026-10-21 修 A10-P0-1）。
> 网关自身不做 TLS：管理员会话 cookie、JWT、上游凭据与下游 `Authorization` 头
> 在明文 HTTP 下可被同网段被动观察者完整截获。之前这里写的是 `-p 8666:8666`，
> 等于把这条默认部署路径直接放在明文网络上。
>
> 需要跨主机访问时，**先**在宿主机上装一个 TLS 反代（nginx / Caddy / Traefik），
> 只让反代连回环端口，并让反代做 HTTPS 终止；此时把映射改成
> `-p 127.0.0.1:8666:8666`（保持回环）再由反代接管对外的 443。反代放行到
> 容器直连端口的写法是 `-p 8666:8666`，**只在反代与网关同机、且宿主机防火墙
> 已挡住 8666 的对外暴露时**才可用。
>
> TLS 反代部署还必须在 `config.json` 配 `"public_origin": "https://gw.example.com"`
> （审计 A7-13）。网关自身只看到明文 HTTP，没有这个显式声明时无法安全地
> 区分「浏览器从 HTTPS 页面发来的同源请求」与「跨协议伪造」，会保守返回 403；
> 配置后会话 Cookie 也会自动带 `Secure`。不要用 `X-Forwarded-Proto` 代替它。
>
> 反代部署还要配置 `"trusted_proxy_cidrs": ["172.18.0.0/16"]`（填实际直连
> 对端的网段），网关才会采信 `Forwarded` / `X-Forwarded-For` 并恢复真实客户端
> IP；不配置时这两个头一律忽略，key 的 IP 白名单和登录限速会退化成按代理 IP
> 计数。反代必须覆盖或追加转发头，不能把客户端自带值原样透传。

### 源码构建

前置 Go 1.27+（与 `go.mod` 的 `go` 指令一致，CI 由 `go-version-file` 单一来源读取）。前端产物已入库（`internal/webui/dist`），只改后端无需 Node：

```bash
go build -o gateway ./cmd/gateway
./gateway                      # 默认监听 127.0.0.1:8666
```

配置文件不是必需的（有零配置默认值）；需要引导上游时参考
[config.example.json](config.example.json) 与 [DESIGN.md](DESIGN.md) §12。
所有相对路径锚在状态目录（默认可执行文件同目录，`ROSETTA_GW_HOME` 可重定向）：

```
<home>/
├── config.json        # 可选的引导配置；不存在时程序自动生成一份，**不回写已存在的**
├── master.key         # 上游凭据加密主密钥（缺失时自动生成并落盘）
├── session_secret     # 会话签名密钥（缺失时自动生成并落盘）
└── data/gateway.db    # SQLite：配置 + 身份 + 用量
```

三把「状态」的代价不对称，备份时按这个优先级：`master.key` 丢了上游凭据全部作废；
`session_secret` 丢了只是所有人重新登录；`data/` 删了要重走首次初始化。

### 下游接入

在管理界面建一把 `sk-gw-` 访问密钥后：

```bash
curl http://<主机>:8666/v1/chat/completions \
  -H "Authorization: Bearer sk-gw-..." \
  -H "Content-Type: application/json" \
  -d '{"model":"<虚拟名或 provider/model>","messages":[{"role":"user","content":"hi"}]}'
```

把 `ANTHROPIC_BASE_URL` 指向网关即可接 Claude Code，`base_url` 指向网关即可接
OpenAI 系客户端；额度查询端点（`/dashboard/billing/*`、`/v1/organization/*`）
与 one-api 系工具兼容。

## 安全模型（摘要）

- **管理面**：users 表 + JWT 会话一条通道，无旁路凭据；首次初始化窗口一次性，
  全新部署且监听非回环时启动日志会打 ERROR 提醒抢先设置的风险。
- **数据面**：访问密钥只存 SHA-256 哈希；禁用/过期/IP 白名单/模型白名单/归属校验
  全链路生效。
- **上游凭据**：AES-256-GCM 加密落库，主密钥独立于数据库存放——
  **`master.key` 丢失 = 上游凭据全部作废，请把状态目录当关键资产持久化**。
- **传输**：网关自身不做 TLS，对外暴露请前置反向代理（见 [DESIGN.md](DESIGN.md) 附录 C）。

## 文档

| 文档 | 内容 |
|---|---|
| [DESIGN.md](DESIGN.md) | 设计文档：架构、数据模型、路由、配额、故障转移、协议转换、配置 |
| [MULTIUSER.md](MULTIUSER.md) | ⚠️ **历史归档**（冻结于 2026-10-07）：多用户 / 分组 / 权限模型的设计记录。保留是因为代码与 DESIGN.md 中有多处注释以它的章节号为锚点（`§5.1` / `§4.3` / `§4.8` 等） |
| [DOCKER.md](DOCKER.md) | 容器化部署：镜像、挂载、日志、升级、健康检查 |
| [CONTRIBUTING.md](CONTRIBUTING.md) | 开发环境与协作约定：本机 `-race` 的 CC 陷阱、纯 Go 交叉编译、代理与 SSH、`sync-embed`、TS 版本上界、代码风格 |

仓库里还有两个本机脚本（不进文档表格，Windows 开发用）：`build.ps1`
（纯编译，产物 `bin\gateway.exe`）与 `gateway.ps1`（编译 + 启动/停止，
前台 / `-Detached` / `-Stop`）。

## 开发

```bash
go build ./... && go vet ./... && go test ./...   # 后端门禁
cd web && npm ci && npm run typecheck && npm run build && npm run sync
# sync（= node sync-embed.mjs）把 web/dist 同步进 internal/webui/dist（embed 源）；
# CI 的 webui-sync job 会重新构建一遍并与入库产物做 git diff，不一致即红灯
```

Windows 上一键做同样的事：`.\build.ps1`（`-NoWeb` 跳过前端、`-Typecheck` 加类型检查）；
启动用 `.\gateway.ps1`（前台）或 `.\gateway.ps1 -Detached`（后台，日志落 `bin\logs\`），
`-Stop` 停止。产物是 `bin\gateway.exe`，配置自动落在它同级的 `bin\config.json`。

提交前请跑全量测试；改动转发热路径（配额/限速/故障转移/落库）时，先读
[DESIGN.md](DESIGN.md) 对应小节的决策留痕——那里的多数「为什么」都对应一次真实踩坑。
本机特有的环境坑（`-race` 需要显式 CC 等）见 [CONTRIBUTING.md](CONTRIBUTING.md)。
