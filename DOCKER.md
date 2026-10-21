# Docker 部署

镜像：`ghcr.io/cn-maul/rosetta-gateway`

**镜像版本号 = 软件版本号**，两者都取自 `web/package.json` 的 `version`（当前 **1.6.5**）。
发布 `v1.6.5` tag 会得到 `:1.6.5` 与 `:latest` 两个标签；CI 会校验 tag 与
`package.json` 一致，不一致直接构建失败（防止打出名不副实的镜像）。

> 下文示例中的 `:1.6.5` 是**编写时的当前版本**。实际部署请先 `docker pull ghcr.io/cn-maul/rosetta-gateway:1.6.5`
> 确认该 tag 存在，或直接用 `:latest`。

架构：`linux/amd64`（`docker.yml` 的 buildx 只构建这一种；需要 arm64 再说）。
非 tag 推送（`main` 分支）会额外打一个 `:edge` 标签。

---

## 快速开始

```bash
docker run -d \
  --name rosetta-gateway \
  -p 127.0.0.1:8666:8666 \
  -v rosetta-gateway-data:/data \
  --restart unless-stopped \
  ghcr.io/cn-maul/rosetta-gateway:1.6.5
```

然后打开 `http://127.0.0.1:8666/admin/` —— 首次进入会显示「首次设置密码」表单，
为自动创建的 `admin` 账号设一个密码，设完直接进入后台。

表单里多了一个**引导令牌**输入框。推荐用环境变量预先指定（值只有部署者持有）：

```bash
docker run -d \
  --name rosetta-gateway \
  -e ROSETTA_GW_BOOTSTRAP_TOKEN="$(openssl rand -hex 32)" \
  -p 127.0.0.1:8666:8666 \
  -v rosetta-gateway-data:/data \
  --restart unless-stopped \
  ghcr.io/cn-maul/rosetta-gateway:1.6.5
```

没设环境变量时网关会生成一枚随机令牌，**不打印到日志**（日志会轮转、会被聚合
到集中式日志平台，等于把「谁还没完成引导」这条信息的攻击面从「能连到端口」
扩大到「能读日志」）。取用方式是从数据库读回：

```bash
docker exec rosetta-gateway sqlite3 /data/data/gateway.db \
  "SELECT value FROM app_settings WHERE key='bootstrap_token'"
```

> **为什么端口映射绑回环**（2026-10-21 修 A10-P0-1）
>
> 网关自身不做 TLS：管理会话 cookie、JWT、上游凭据、下游 `Authorization` 头
> 全部走明文。默认写 `-p 8666:8666` 等于把这条**推荐**部署路径直接放在
> 同网段可被动监听的明文网络上。
>
> 需要跨主机访问时：先在宿主机上装 TLS 反代（nginx / Caddy / Traefik），
> 让反代做 HTTPS 终止并回连 `127.0.0.1:8666`，对外只暴露 443。
> 容器内仍保持 `listen 0.0.0.0:8666`（容器只听回环会让端口映射不可达），
> 边界由**宿主机的端口映射**承担。
>
> **反代部署必须配置 `public_origin`**（审计 A7-13）：
>
> ```json
> { "public_origin": "https://gw.example.com" }
> ```
>
> 网关自身只看到明文 HTTP（`r.TLS=nil`）。配置后，同源校验以这个值为
> 浏览器侧 scheme+host 的基准，会话 Cookie 也会自动带 `Secure`。
> 不配置时同源校验会保守地拒绝浏览器从 HTTPS 页面发来的写请求（403）。
> **不要**依赖 `X-Forwarded-Proto`：该头可被客户端伪造，而同一个校验
> 还保护免鉴权的首次引导入口。
>
> 若你的部署里反代与网关不同机，那么 `-p 127.0.0.1:8666:8666` 就不可用 ——
> 这种情况请在宿主机上用防火墙把 8666 只放行给反代机的地址，
> 并接受「到反代这一跳仍是明文」的现实（跨机的第一跳需要网络层保证）。
>
> 反代还要把真实客户端地址交给网关，否则 key 的 IP 白名单与登录限速会把
> **代理地址**当成所有用户。显式配置可信代理网段后才采信 `Forwarded` /
> `X-Forwarded-For`，未配置时这两个头一律忽略：
>
> ```json
> {
>   "public_origin": "https://gw.example.com",
>   "trusted_proxy_cidrs": ["172.18.0.0/16", "::1/128"]
> }
> ```
>
> 网段要填**容器实际看到的直连对端**，不要为了省事写 `0.0.0.0/0`。
> 反代必须覆盖或追加 `X-Forwarded-For`（nginx：
> `proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;`），不能把
> 客户端自带的同名头原样透传。网关从右向左跳过可信代理，取第一个不可信
> 地址，因此客户端在链首伪造的 IP 不会覆盖代理追加的真实来源。

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
> docker run --rm ghcr.io/cn-maul/rosetta-gateway:1.6.5 \
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
    image: ghcr.io/cn-maul/rosetta-gateway:1.6.5
    container_name: rosetta-gateway
    restart: unless-stopped
    # 优雅关停的总预算是 24s（剪枝 5s + 费用重算 2s + 探测 2s +
    # HTTP 10s + 用量落库 5s），而 docker compose 的默认宽限期只有 **10s**。
    # 不调大的话 `docker compose down` 会在用量队列还在落库时 SIGKILL，
    # 表现为「最后几次调用的用量查不到」（这些请求因此免单且配额被放宽）。
    stop_grace_period: 30s
    ports:
      # 绑回环：网关自身不做 TLS，对外访问请走宿主机上的 TLS 反代
      # （反代回连 127.0.0.1:8666）。详见「快速开始」的说明。
      - "127.0.0.1:8666:8666"
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
| `TZ` | 容器时区。**影响日志时间戳，也决定用量按天统计的日界** | `Asia/Shanghai` |

`ROSETTA_GW_MASTER_KEY` 只在应急时用：它优先于 `master.key`，**两边取值不同会导致
已加密的上游凭据全部解不开**。平时不要设。

会话密钥无需预先配置：没设环境变量时会自动生成 `session_secret` 并落盘。
它**必须随 `/data` 持久化** —— 换了密钥，所有登录会话立即失效。

## 首次启动的安全提示

默认配置是 `listen: 0.0.0.0:8666`，而第一个管理员的密码要等人打开 `/admin/` 来设 ——
**在设好密码之前，能访问到该端口的人都可以尝试抢先完成设置**。网关启动时检测到
这种「未初始化 + 非回环监听」的状态会打一条 ERROR 日志提醒。

同源判定挡不住这条路径 —— `curl` 不发 `Origin`，服务端无从分辨是运维还是攻击者。
所以完成引导**必须带一次性令牌**（见下）。窗口是**一次性**的：首次设密成功即写入
`bootstrap_completed` 标记、永久关闭，令牌同时作废（不存在「空密码 admin 重新打开窗口」
的路径，也不存在「密码设了、令牌还在」的重放窗口）。

### 引导令牌

启动日志里会有一条 WARN 说明令牌**已布置**，但**不打印令牌值**
（2026-10-21 修 A10-P1-1）：

```bash
docker logs rosetta-gateway 2>&1 | grep 引导令牌
```

- 未设置 `ROSETTA_GW_BOOTSTRAP_TOKEN` 时，网关在启动时生成 32 字节随机值（64 位 hex）；
- 令牌**不进日志**，只从数据库读回：

  ```bash
  docker exec rosetta-gateway sqlite3 /data/data/gateway.db \
    "SELECT value FROM app_settings WHERE key='bootstrap_token'"
  ```

  裸机部署把 `/data/data/gateway.db` 换成 `<home>/data/gateway.db` 即可。
- 令牌不会出现在任何 HTTP 响应里 —— `/admin/api/bootstrap` 免鉴权，
  把令牌放进去等于取消令牌；
- 重启不会覆盖已有令牌。令牌若已丢失，删除该部署的
  `app_settings.bootstrap_token` 后重启即可重新生成；
- 令牌在「写密码 + 置完成标记 + 清令牌」**同一个事务**里消耗，不存在中间态。

生产环境建议显式指定它：

```bash
docker run -d \
  --name rosetta-gateway \
  -e ROSETTA_GW_BOOTSTRAP_TOKEN="$(openssl rand -hex 32)" \
  -p 127.0.0.1:8666:8666 \
  -v rosetta-gateway-data:/data \
  --restart unless-stopped \
  ghcr.io/cn-maul/rosetta-gateway:latest
```

理由与自动生成的令牌同类：值只由部署者持有，备份与日志体系都不需要承载它。
指定令牌时长度必须
**至少 32 字节**，过短会被拒绝并在日志里报 ERROR（此时不会写入任何令牌，引导窗口
回到无令牌保护状态）。

> **备份注意**：`/data` 里的数据库若在引导完成前被备份，备份中包含尚未消费的
> 引导令牌明文。备份文件要按凭据级别保护，别丢进公开的对象存储或代码库。

所以：**容器起来后第一时间去 `/admin/` 完成首次设置密码**。
默认的端口映射已经是回环（`-p 127.0.0.1:8666:8666`）；对外访问请走 TLS 反代，
或至少先在本机完成密码设置再放开端口。

## 日志

**没有日志文件 —— 应用日志就是容器的标准输出。**

网关用 `slog` 的 JSON handler 直接写 stdout（`internal/gatewayapp/run.go`），配置里的 `log_level`
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

## 时区：决定「按天统计」的日界

用量按天分桶（总览日趋势、钱包页的按天消耗记录、以及用量归档）用的是 SQLite 的
`'localtime'`，它跟随进程的本地时区 —— 也就是容器里的 `TZ`。

**配错了的症状**：过了 0 点，按天数据不切到新的一天；本地时间 0:00 之后的调用
被算进「昨天」。调用历史里的时间戳始终是对的（那是原始时刻，由浏览器按你的时区
渲染），错的是**按天聚合**。

东八区用户在 UTC 容器里的典型表现：0:00–8:00 的调用全被算进前一天。

设置方式（三种，任选，优先级从高到低）：

```bash
# 1. 容器级（推荐）—— 同时影响日志时间戳
docker run -e TZ=Asia/Shanghai ...

# 2. config.json 里配 —— 只影响按天统计的日界，不依赖容器环境
{ "timezone": "Asia/Shanghai" }

# 3. 裸机部署 —— 设进程环境即可，无需改配置
TZ=Asia/Shanghai ./gateway
```

镜像默认已是 `Asia/Shanghai`（`ENV TZ` + entrypoint 兜底）。换成别的时区：

```bash
docker run -e TZ=Asia/Tokyo ...
```

启动时日志会打印生效的时区，可用于核对：

```
{"level":"INFO","msg":"daily-bucket timezone applied","timezone":"Asia/Shanghai"}
```

**改时区不会损坏数据**：按天的 `day` 是查询时由 `usage_records.ts` 现算的
（`strftime`），`ts` 是绝对时刻、与时区无关。改完重启，历史的按天数字会按新
日界重新分桶。唯一例外是**已经归档**的行（明细保留 30 天后转入日聚合表），
它们的 `day` 是归档那一刻固化的值 —— 那部分历史不会跟着变。

## 数据与升级

```bash
docker run --rm -v rosetta-gateway-data:/data -v "$PWD:/backup" alpine sh -c '
  set -eu
  # 一致性快照：VACUUM INTO 在事务内产出一个完整副本，
  # 不受 -wal / -shm 与主库不同步的影响。
  apk add --no-cache sqlite >/dev/null
  stamp=$(date +%F-%H%M%S)
  out=/backup/rosetta-gateway-$stamp
  mkdir -p "$out"

  # 配置与两把密钥缺一不可：只有数据库无法恢复可用的部署。
  for f in config.json master.key session_secret; do
    [ ! -e "/data/$f" ] || cp -a "/data/$f" "$out/$f"
  done

  # db_path 可能被改过，从当前配置解析；相对路径锚定 /data。
  db=$(sed -n '\''s/.*"db_path"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p'\'' /data/config.json | head -n 1)
  case "$db" in
    /*) db_path=$db ;;
    *)  db_path=/data/${db#./} ;;
  esac
  sqlite3 "$db_path" "VACUUM INTO '\''$out/gateway.db'\''"

  tar czf "$out.tar.gz" -C /backup "$(basename "$out")"
  echo "备份完成：$out.tar.gz"
'
```

备份包必须包含 **`config.json`、`master.key`、`session_secret` 和 SQLite 数据库**。
上面的 `VACUUM INTO` 已把 WAL 中已提交的事务合并进单文件快照；若选择停机后
手工复制，则必须同时复制 `gateway.db`、`gateway.db-wal`、`gateway.db-shm`
三个文件，缺 WAL 会丢掉最近已提交的用量与余额变更。

> `ROSETTA_GW_MASTER_KEY` 或 `ROSETTA_GW_SESSION_SECRET` 用环境变量覆盖时，
> `/data` 里不会有对应文件，备份系统必须另外保存环境变量/密钥管理系统中的值。

> **不要在运行中直接 `tar -C /data .`**。本项目用 WAL（`synchronous=NORMAL`），
> 已提交但尚未 checkpoint 的事务只存在于 `gateway.db-wal` 里。把主库与
> `-wal`/`-shm` 当三个普通文件分别拷走，它们取自不同时刻 —— 恢复出来
> 可能**丢掉最近若干笔已提交用量**（表现为额度、余额与报表对不上）。
> 停机后一次性复制三个文件、或用上面的 `VACUUM INTO` 都安全。

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
docker run -d --name rosetta-gw -p 127.0.0.1:8666:8666 \
  -v /srv/rosetta-gw:/data \
  -e ROSETTA_GW_UID=$(id -u) -e ROSETTA_GW_GID=$(id -g) \
  ghcr.io/cn-maul/rosetta-gateway:latest
```

若属主改不动（NFS、只读根文件系统等），entrypoint **拒绝启动**并在 stderr
说明原因，不再回退到 root。把 UID/GID 配成 0 也会被拒绝。以 root 运行意味着
进程被攻破即持容器 root，部署应修正宿主机目录属主，而不是降级安全边界。

## 健康检查

镜像自带 `HEALTHCHECK`，从 `/data/config.json` 的 `listen` 解析端口后探
`127.0.0.1:<port>/healthz`。改过容器内监听端口时不需要重做镜像：

```
--interval=30s --timeout=5s --start-period=10s --retries=3
```

`/healthz` 会 ping 数据库的**两个**连接池，并检查运行时快照有没有落后于库
（配置热重载失败时返回 503）。它取代了原先探 `/admin/` 的写法 —— 后者只读
embed 的静态产物、不碰数据库，写池被卡住、用量写入持续失败时依然返回 200，
容器既不重启也不告警。同理也不探 `/v1/models`：那个要鉴权、会稳定返回 401，
那种「健康」毫无意义。

该端点**免鉴权**且不返回任何业务数据，只暴露一个状态码。

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
- `127.0.0.1:8666->8666/tcp` → 只绑回环，局域网访问不了（**这是刻意的默认**，
  跨主机访问请走宿主机上的 TLS 反代）
- `0.0.0.0:32768->8666/tcp` → 宿主端口被随机分配了，`-p` 要写 `8666:8666`

映射正确却连不上，再看 `config.json` 里的 `listen`：容器内必须是 `0.0.0.0:8666`，
写 `127.0.0.1` 的话只有容器自己能访问。
