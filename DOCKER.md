# Docker 部署

镜像：`ghcr.io/cn-maul/rosetta-gateway`

**镜像版本号 = 软件版本号**，两者都取自 `web/package.json` 的 `version`。
发布 `v1.1.1` tag 会得到 `:1.1.1` 与 `:latest` 两个标签；CI 会校验 tag 与
`package.json` 一致，不一致直接构建失败（防止打出名不副实的镜像）。

架构：`linux/amd64`（CI 只构建这一种；需要 arm64 再说）。

---

## 快速开始

```bash
docker run -d \
  --name rosetta-gateway \
  -p 8666:8666 \
  -v rosetta-gateway-data:/data \
  --restart unless-stopped \
  ghcr.io/cn-maul/rosetta-gateway:1.1.1
```

然后打开 `http://<主机>:8666/admin/` —— 首次进入会显示「首次设置密码」表单，
为自动创建的 `admin` 账号设一个密码，设完直接进入后台。

> 局域网里别的机器访问，要用宿主机的**局域网 IP**，不是 `localhost`。

## 端口为什么是 8666，不是 6666

**因为浏览器不许用 6666。** Chromium 把 6666 列在保留端口表里
（`net/base/port_util.cc` 的 `kRestrictedPorts`，IRC 段：6665–6669、6697），
Chrome / Edge / Brave / Opera 一律硬拦；Firefox 也拦，只是报错文案不同。

症状非常容易误判：

| 测法 | 结果 |
|---|---|
| 容器内 `wget 127.0.0.1:8666` | 通（busybox wget 不管黑名单） |
| 宿主机 `curl 127.0.0.1:8666` | 通（curl 也不管） |
| **浏览器打开** | **`ERR_UNSAFE_PORT`** |

关键在于拦截发生在**浏览器内部**——请求根本不会发出去，所以**服务端没有任何连接日志**。
于是人会一路去查防火墙、端口映射、容器网络，唯独想不到是端口号的问题。

**`--explicitly-allowed-ports` 和组策略 `ExplicitlyAllowedNetworkPorts` 不是解药**：
它们只放开**你这一台**浏览器，同事、手机、别的机器照样打不开。

黑名单里还有一些看着很正常的端口，挑端口时一并避开：
`6000`(X11)、`6566`(sane)、`10080`(Amanda)、`2049`(nfs)、`5060`/`5061`(sip)、
`1719`–`1723`、`3659`、`4045`、`6697`。完整表见 `internal/config/ports.go`——
网关启动时会自动比对，命中就打一条显眼警告。

要在 6666 上跑（比如已有约定），**只能改宿主端口映射**，容器内保持 8666 即可：

```bash
-p 6666:8666      # 宿主 6666 → 容器 8666，浏览器访问 6666 依然会被拦，没意义
```

正确做法是**把宿主端口也换掉**：

```bash
-p 8666:8666
```

## 容器内的目录布局

```
/app/gateway              网关本体（Linux 静态二进制，随镜像更新）
/app/config.default.json  首次启动用的配置模板（listen 0.0.0.0:8666）
/data                     唯一需要持久化的目录（VOLUME），即 ROSETTA_GW_HOME
├── config.json           生效中的配置
├── master.key            上游凭据加密主密钥，自动生成
├── session_secret        会话签名密钥，自动生成；丢失 = 所有登录会话失效
└── data/
    └── gateway.db        SQLite：上游 / 模型 / 路由 / 访问密钥 / 用户 / 用量记录
```

> `db_path` 在模板里是相对 `ROSETTA_GW_HOME` 的 `./data/gateway.db`（2026-10-07 起，
> 与裸机部署的代码默认值对齐；此前模板曾写 `./db/gateway.db`）。**已有部署不受影响**：
> 旧模板生成的 `config.json` 里仍是老路径，库照常落在 `/data/db/`，升级镜像不会挪库。

镜像层本身是无状态的：`/data` 之外没有任何东西需要保存，升级镜像不会碰配置与数据。

## 挂载

**配置文件和数据目录都在 `/data` 下，所以可以整体挂，也可以分开挂。**

整体挂一个卷（省事，推荐）：

```bash
-v rosetta-gateway-data:/data
```

或者按需精细挂载 —— 把配置和数据库分别落到宿主机目录：

```bash
-v /srv/rosetta/config.json:/data/config.json \
-v /srv/rosetta/data:/data/data
```

> **注意**：bind mount 单个**文件**时，宿主机上的该文件必须**先存在**，
> 否则 Docker 会把它当成目录创建，程序会读到「is a directory」而启动失败。
> 第一次可以这样初始化：
> ```bash
> mkdir -p /srv/rosetta/data
> docker run --rm ghcr.io/cn-maul/rosetta-gateway:1.1.1 \
>   cat /app/config.default.json > /srv/rosetta/config.json
> ```
> （`mkdir` 了 `data`，因为 `db_path` 指向 `/data/data/gateway.db`，父目录必须存在；
> 若你的 `config.json` 是旧模板生成的、`db_path` 仍为 `./db/gateway.db`，则挂 `/data/db`。）

主密钥 `master.key` 与会话密钥 `session_secret` 也都在 `/data` 下，
**跟着 `/data` 一起持久化。** 若只单独挂了 `config.json` 和 `db/` 而没挂 `/data`，
这两者会落在容器可写层、随容器重建丢失 —— `master.key` 丢了上游凭据将无法解密；
`session_secret` 丢了所有登录会话立即失效（用户被登出，重新登录即可，数据无损）。
要精细挂载就把它们也一起挂上。

> **`config.json` 一旦生成就不会跟镜像更新。** 升级镜像后它仍然是老内容
> （包括 `listen` 端口）。换过默认端口的话，要么手工改这一行，要么删掉它让入口脚本重新播种。

## docker compose

```yaml
services:
  rosetta-gateway:
    image: ghcr.io/cn-maul/rosetta-gateway:1.1.1
    container_name: rosetta-gateway
    restart: unless-stopped
    ports:
      - "8666:8666"
    volumes:
      - ./rosetta/config.json:/data/config.json   # 配置文件
      - ./rosetta/data:/data/data                 # 数据库目录（旧模板路径为 /data/db）
      - ./rosetta/master.key:/data/master.key     # 凭据加密主密钥
      - ./rosetta/session_secret:/data/session_secret  # 会话签名密钥
```

（若不需要精细控制，把上面四条换成一个 `- ./rosetta:/data` 即可。）

> `ports:` 才是发布端口；写成 `expose:` 只在容器网络内可见，**宿主机访问不到**。
> `"8666"`（只写一个数）会随机分配宿主端口，别这么写。

## 环境变量

| 变量 | 作用 | 默认 |
|---|---|---|
| `ROSETTA_GW_HOME` | 状态根目录（配置 / 密钥 / 数据库） | 镜像内已设为 `/data` |
| `ROSETTA_GW_MASTER_KEY` | 覆盖主密钥。**设了就不用 `master.key` 文件** | 空（走 `/data/master.key`） |
| `ROSETTA_GW_SESSION_SECRET` | 覆盖会话签名密钥。**设了就不用 `session_secret` 文件** | 空（走 `/data/session_secret`，没有就自动生成） |
| `TZ` | 容器时区，影响日志时间戳 | `UTC` |

`ROSETTA_GW_MASTER_KEY` 只在应急时用：它优先于 `master.key`，**两边取值不同会导致
已加密的上游凭据全部解不开**。平时不要设。

会话密钥无需预先配置：没设环境变量时会自动生成 `session_secret` 并落盘。
它**必须随 `/data` 持久化** —— 换了密钥，所有登录会话立即失效。

## 首次启动的安全提示

默认配置是 `listen: 0.0.0.0:8666`，而第一个管理员的密码要等人打开 `/admin/` 来设 ——
**在设好密码之前，能访问到该端口的人都可以抢先完成设置**。网关启动时检测到这种
「未初始化 + 非回环监听」的状态会打一条 ERROR 日志提醒。窗口是**一次性**的：
首次设密成功即写入 `bootstrap_completed` 标记、永久关闭，且创建 admin 必须带初始密码
（不存在「空密码 admin 重新打开窗口」的路径）。

所以：**容器起来后第一时间去 `/admin/` 完成首次设置密码**。
若要挂在公网，建议只绑回环（`-p 127.0.0.1:8666:8666`）再加反代鉴权，
或至少先在本机完成密码设置再放开端口。

## 日志

**没有日志文件 —— 应用日志就是容器的标准输出。**

网关用 `slog` 的 JSON handler 直接写 stdout（`cmd/gateway/main.go`），配置里的 `log_level`
只调级别、不改去向。所以 `docker logs` 就是全部日志，`/data` 里不会出现任何 `.log`。

```bash
docker logs -f --tail 200 rosetta-gateway        # 实时跟随
docker logs rosetta-gateway > gateway.log        # 导出留档（推荐，见下）
```

Docker 默认的 `json-file` driver 会把它们落在
`/var/lib/docker/containers/<容器ID>/<容器ID>-json.log`。**别去那儿找**：Docker Desktop 跑在
WSL 的 docker VM 里，宿主机文件系统上翻不到；而且 `docker rm` 容器就带走了。

每行是完整的 JSON（`time` / `level` / `msg` + 字段），要人读可转一下：

```bash
docker logs rosetta-gateway 2>&1 | jq -r '"\(.time) \(.level) \(.msg)"'
```

想让 Docker 自己控制大小与轮转（compose）：

```yaml
    logging:
      driver: json-file        # 或 local
      options:
        max-size: "20m"
        max-file: "5"
```

`TZ` 环境变量只影响时间戳的读法，不影响日志去向。

## 数据与升级

```bash
docker run --rm -v rosetta-gateway-data:/data -v "$PWD:/backup" alpine \
  tar czf /backup/rosetta-gateway-$(date +%F).tar.gz -C /data .
```

升级就是换镜像标签后重建容器 —— `/data` 原样保留：

```bash
docker compose pull && docker compose up -d
```

## 以非 root 运行（2026-10-07 起）

进程不再以 root 运行。容器仍以 root 启动（需要 `chown` 状态目录），
但 entrypoint 会把 `/data` 的属主改成 `ROSETTA_GW_UID:GID`（默认 1000:1000），
然后用 `su-exec` 把网关 exec 成那个用户。

宿主机目录属主与容器内 uid 不一致时（bind mount 很常见），显式传：

```bash
docker run -d --name rosetta-gw -p 8666:8666 \
  -v /srv/rosetta-gw:/data \
  -e ROSETTA_GW_UID=$(id -u) -e ROSETTA_GW_GID=$(id -g) \
  ghcr.io/cn-maul/rosetta-gateway:latest
```

若属主改不动（NFS、只读根文件系统等），entrypoint **不会放开目录权限**（回退是
`chmod 0700` 收紧，而不是曾经的世界可写），会在 **stderr 明确告警**后以 root 继续运行
—— 不静默降级，否则运维会以为容器已加固。以 root 运行意味着进程被攻破即持容器 root，
请优先修复属主问题。

## 健康检查

镜像自带 `HEALTHCHECK`，探 `127.0.0.1:8666/admin/`：

```
--interval=30s --timeout=5s --start-period=10s --retries=3
```

探 `/admin/` 而不是 `/v1/models` —— 后者要鉴权、稳定返回 401，那种「健康」
毫无意义。`/admin/` 只依赖 embed 的静态产物、不碰数据库，能返回 200 就证明
进程还活着且路由正常。

```
docker inspect --format '{{.State.Health.Status}}' rosetta-gw
```

## 常见问题

**浏览器报 `ERR_UNSAFE_PORT`，但容器日志一切正常**
端口落在浏览器的保留端口表里（典型是 6666）。这是**客户端**拦截，请求没发出去，
所以服务端无日志。换成黑名单外的端口，宿主机和容器都用同一个，例如 8666。
判断方法：容器内 `wget` 和宿主机 `curl` 都通、只有浏览器不通，就是这个。

**起来就退出，日志报会话密钥相关错误（secret 无效 / 太短 / 为空）**
`session_secret` 被写坏了（比如挂载了一个空文件 —— 网关把它当故障，不会静默重新生成）。
删掉它重启即可：会自动生成新密钥，代价是所有用户需要重新登录。

**上游请求 502，日志里有 `cipher: message authentication failed`**
`master.key` 变了或丢了，库里已加密的凭据解不开。恢复原来的 `master.key`；
找不回来就删掉对应上游凭据重建。

**端口没监听 / 浏览器连不上**
先看 `docker ps` 的 `PORTS` 列：
- 空的 → 没发布端口（漏了 `-p`，或写成了 `expose:`）
- `127.0.0.1:8666->8666/tcp` → 只绑回环，局域网访问不了
- `0.0.0.0:32768->8666/tcp` → 宿主端口被随机分配了，`-p` 要写 `8666:8666`

映射正确却连不上，再看 `config.json` 里的 `listen`：容器内必须是 `0.0.0.0:8666`，
写 `127.0.0.1` 的话只有容器自己能访问。
