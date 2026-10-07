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
- **用量记账与归档**：逐请求明细（含固化费用）保留 30 天，
  日归档 + 终身累计单行表保证「明细可删、累计不变」；
  历史费用按落库时单价固化，改价不改历史。
- **Web 管理界面**：Vue 3 + Vite，产物经 `go:embed` 进二进制，无外部静态资源。

## 快速开始

### Docker（推荐）

```bash
docker run -d \
  --name rosetta-gateway \
  -p 8666:8666 \
  -v rosetta-gateway-data:/data \
  --restart unless-stopped \
  ghcr.io/cn-maul/rosetta-gateway:latest
```

打开 `http://<主机>:8666/admin/`，首次进入是**一次性**的「首次设置密码」表单
（设完即永久关闭免鉴权窗口），设完即进入后台。容器部署的目录布局、挂载方式与
升级流程见 [DOCKER.md](DOCKER.md)。

### 源码构建

前置 Go 1.22+。前端产物已入库（`internal/webui/dist`），只改后端无需 Node：

```bash
go build -o gateway ./cmd/gateway
./gateway                      # 默认监听 127.0.0.1:8666
```

配置文件不是必需的（有零配置默认值）；需要引导上游时参考
[config.example.json](config.example.json) 与 [DESIGN.md](DESIGN.md) §12。
所有相对路径锚在状态目录（默认可执行文件同目录，`ROSETTA_GW_HOME` 可重定向）：

```
<home>/
├── config.json        # 可选的引导配置，程序不回写
├── master.key         # 上游凭据加密主密钥（缺失时自动生成）
├── session_secret     # 会话签名密钥（缺失时自动生成）
└── data/gateway.db    # SQLite：配置 + 身份 + 用量
```

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
| [MULTIUSER.md](MULTIUSER.md) | 多用户 / 分组 / 权限模型的设计与实现记录 |
| [DOCKER.md](DOCKER.md) | 容器化部署：镜像、挂载、日志、升级、健康检查 |

## 开发

```bash
go build ./... && go vet ./... && go test ./...   # 后端门禁
cd web && npm ci && npm run typecheck && npm run build && npm run sync
# sync 把前端产物同步进 internal/webui/dist（embed 源）；CI 会校验产物已同步
```

提交前请跑全量测试；改动转发热路径（配额/限速/故障转移/落库）时，先读
[DESIGN.md](DESIGN.md) 对应小节的决策留痕——那里的多数「为什么」都对应一次真实踩坑。
