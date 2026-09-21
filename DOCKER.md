# Docker 部署

镜像：`ghcr.io/cn-maul/rosetta-gateway`

**镜像版本号 = 软件版本号**，两者都取自 `web/package.json` 的 `version`。
发布 `v1.1.0` tag 会得到 `:1.1.0` 与 `:latest` 两个标签；CI 会校验 tag 与
`package.json` 一致，不一致直接构建失败（防止打出名不副实的镜像）。

架构：`linux/amd64`、`linux/arm64`。

---

## 快速开始

```bash
docker run -d \
  --name rosetta-gateway \
  -p 6666:6666 \
  -v rosetta-gateway-data:/data \
  --restart unless-stopped \
  ghcr.io/cn-maul/rosetta-gateway:1.1.0
```

然后打开 `http://<主机>:6666/admin/` —— 首次进入会要求设置管理员密码。

## 容器内的目录布局

```
/app/gateway              网关本体（Linux 静态二进制，随镜像更新）
/app/config.default.json  首次启动用的配置模板（listen 0.0.0.0:6666）
/data                     唯一需要持久化的目录（VOLUME）
├── config.json           生效中的配置
├── master.key            上游凭据加密主密钥，自动生成
├── admin_auth.json       管理员密码（PBKDF2-SHA256 加盐，无明文）
└── db/
    └── gateway.db        SQLite：上游 / 模型 / 路由 / 访问密钥 / 用量记录
```

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
-v /srv/rosetta/db:/data/db
```

> **注意**：bind mount 单个**文件**时，宿主机上的该文件必须**先存在**，
> 否则 Docker 会把它当成目录创建，程序会读到「is a directory」而启动失败。
> 第一次可以这样初始化：
> ```bash
> mkdir -p /srv/rosetta/db
> docker run --rm ghcr.io/cn-maul/rosetta-gateway:1.1.0 \
>   cat /app/config.default.json > /srv/rosetta/config.json
> ```
> （`mkdir` 了 `db`，因为 `db_path` 指向 `/data/db/gateway.db`，父目录必须存在。）

主密钥 `master.key` 与管理员密码 `admin_auth.json` 也都在 `/data` 下，
**跟着 `/data` 一起持久化。** 若只单独挂了 `config.json` 和 `db/` 而没挂 `/data`，
这两者会落在容器可写层、随容器重建丢失 —— 上游凭据将无法解密，管理员密码会回到未设置。
要精细挂载就把它们也一起挂上。

## docker compose

```yaml
services:
  rosetta-gateway:
    image: ghcr.io/cn-maul/rosetta-gateway:1.1.0
    container_name: rosetta-gateway
    restart: unless-stopped
    ports:
      - "6666:6666"
    volumes:
      - ./rosetta/config.json:/data/config.json   # 配置文件
      - ./rosetta/db:/data/db                     # 数据库目录
      - ./rosetta/master.key:/data/master.key     # 凭据加密主密钥
      - ./rosetta/admin_auth.json:/data/admin_auth.json
```

（若不需要精细控制，把上面四条换成一个 `- ./rosetta:/data` 即可。）

## 环境变量

| 变量 | 作用 | 默认 |
|---|---|---|
| `ROSETTA_GW_HOME` | 状态根目录（配置 / 主密钥 / 凭据 / 数据库） | 镜像内已设为 `/data` |
| `ROSETTA_GW_MASTER_KEY` | 覆盖主密钥。**设了就不用 `master.key` 文件** | 空（走 `/data/master.key`） |
| `ADMIN_TOKEN` | 管理员兜底令牌，用户设过密码后失效 | 空 |
| `TZ` | 容器时区，影响日志时间戳 | `UTC` |

`ROSETTA_GW_MASTER_KEY` 只在应急时用：它优先于 `master.key`，**两边取值不同会导致
已加密的上游凭据全部解不开**。平时不要设。

## 首次启动的安全提示

默认配置是 `listen: 0.0.0.0:6666` 且 `admin_token` 为空 —— 这是「端口已发布、
但还没有任何管理凭据」的状态，**在设置密码之前，能访问到该端口的人可以先设密码**。

所以：**容器起来后第一时间去 `/admin/` 设置管理员密码**。
若要挂在公网，建议先设 `-e ADMIN_TOKEN=<随机串>` 再启动，或者只绑回环
（`-p 127.0.0.1:6666:6666`）。

## 数据与升级

```bash
docker run --rm -v rosetta-gateway-data:/data -v "$PWD:/backup" alpine \
  tar czf /backup/rosetta-gateway-$(date +%F).tar.gz -C /data .
```

升级就是换镜像标签后重建容器 —— `/data` 原样保留：

```bash
docker compose pull && docker compose up -d
```

## 常见问题

**起来就退出，日志报 `credentials file ... 内容无效`**
`admin_auth.json` 被写坏了（比如挂载了一个空文件）。删掉它重启即可，
届时改用 `ADMIN_TOKEN` 或重新设置密码。

**上游请求 502，日志里有 `cipher: message authentication failed`**
`master.key` 变了或丢了，库里已加密的凭据解不开。恢复原来的 `master.key`；
找不回来就删掉对应上游凭据重建。

**端口 6666 没监听**
`config.json` 里的 `listen` 被改过。容器内请用 `0.0.0.0:6666`，
`127.0.0.1` 只能被容器自己访问。
