# syntax=docker/dockerfile:1

# rosetta-gateway 容器镜像（Linux）。
#
# 产物是 CGO_ENABLED=0 的静态链接 Linux 可执行文件 —— modernc.org/sqlite 是纯 Go
# 实现，所以运行层直接用 alpine，没有 glibc/musl 依赖问题。
# 前端产物 internal/webui/dist 已随仓库入库并由 go:embed 打包，构建阶段只需编译 Go。

# ---- 构建阶段 -------------------------------------------------------------
# 只构建 linux/amd64，**不使用多架构交叉编译**。
#
# 原先这里写的是 `--platform=$BUILDPLATFORM` + `GOARCH=${TARGETARCH}`，
# 为的是「只有 COPY 在换架构，不必把 go build 丢进 QEMU 模拟」。
# 但既然部署目标只有 amd64，这套机制就是纯复杂度：
#   - $BUILDPLATFORM / TARGETOS / TARGETARCH 三个变量全为单架构服务；
#   - 交叉编译产出的 arm64 二进制没人部署；
#   - 每次构建还要额外解析一次目标平台。
# 直接让 FROM / RUN 用运行器自身架构即可，语义更直白，也不给
# 「误以为在构建 arm64」留下空间。
# 基础镜像固定到补丁版本与多架构 manifest digest，避免 tag 漂移导致同一
# 源码在两次发布中实际使用不同工具链/运行库。升级时两者必须一起评估。
FROM golang:1.27.1-alpine3.24@sha256:8a5910f31396cd4d89662f56c68b3ae31d374308270a1c3bd96672ee5ed43414 AS build
WORKDIR /src

# 依赖单独成层：go.mod / go.sum 没变就命中缓存
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# VERSION 由 CI 经 build-args 传入（值取自 web/package.json），
# 直接 ldflags 注入 main.buildVersion，启动日志与镜像标签同源。
ARG VERSION=dev

RUN CGO_ENABLED=0 GOOS=linux \
      go build -trimpath \
      -ldflags "-s -w -X main.buildVersion=${VERSION}" \
      -o /out/gateway ./cmd/gateway

# ---- 运行阶段 -------------------------------------------------------------
FROM alpine:3.24.2@sha256:294b683cb724975bec92580e1e685676bd4b50bda910ddb8c51d4cabeaec77e6

# ca-certificates：访问上游 HTTPS 必需；tzdata：日志时间戳按容器时区显示；
# su-exec：以非 root 身份降权启动（entrypoint 用它 exec 掉 root 外壳）。
RUN apk add --no-cache ca-certificates tzdata su-exec

COPY --from=build /out/gateway /app/gateway
COPY docker/config.default.json /app/config.default.json
COPY docker/docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh
RUN chmod 0755 /app/gateway /usr/local/bin/docker-entrypoint.sh

# 全部可变状态（config.json / master.key / session_secret / db）都锚在这个目录上。
# 镜像层因此完全无状态：升级镜像不会碰配置与数据。
# session_secret 必须随卷持久化：丢了它所有登录会话立即失效（用户需重新登录）。
ENV ROSETTA_GW_HOME=/data

# 默认时区（2026-10-09 修复「过了 0 点按天数据不切日」的线上缺陷）。
#
# SQLite 的 'localtime'（store.dayExpr，按天分桶的日界）跟 Go 的 time.Local
# 走；容器里没设 TZ 时它是 UTC，于是北京时间 0~8 点的调用全被算进
# 「昨天」。默认东八区（本项目当前用户所在时区），要改用 -e TZ=Asia/Tokyo
# 覆盖。tzdata 已在上方装上，IANA 名都能解析。
#
# entrypoint 里还有一道 :${TZ:=Asia/Shanghai} 兜底（覆盖「直接 exec 二进制」
# 而不经 entrypoint 的用法），两处必须保持同一个默认值。
ENV TZ=Asia/Shanghai

VOLUME ["/data"]

# 端口用 8666，**不能用 6666** —— 6666 在 Chromium 的保留端口表
# （net/base/port_util.cc 的 kRestrictedPorts，IRC 段）里，浏览器会在
# 发起请求前就拒绝，报 ERR_UNSAFE_PORT，且服务端看不到任何连接日志。
EXPOSE 8666

# HEALTHCHECK：死锁时容器不会自动重启，编排系统也探不到活性。
# 探 /healthz：它 ping 数据库两个连接池，并检查运行时快照有没有落后于库。
# 此前探 /admin/（静态产物），而那条路径只读 embed FS、不碰数据库 ——
# 写池被卡住、用量写入持续失败时容器仍然报 healthy，既不重启也不告警。
# 不探 /v1/models：那个要鉴权、会稳定返回 401，那种「健康」毫无意义。
#
# 间隔 30s / 超时 5s / 宽限 10s：SQLite 启动剪枝偶发慢，宽限期别太短，
# 否则滚动更新时新容器会被反复判死并重启。
# 端口从 /data/config.json 的 listen 解析，不能写死 8666：模板与文档虽然
# 默认 8666，但存量部署可以改端口，固定探针会把健康容器永久标成 unhealthy。
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
	CMD /bin/sh -c 'config="${ROSETTA_GW_HOME:-/data}/config.json"; listen=$(sed -n '\''s/.*"listen"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p'\'' "$config" | head -n 1); port="${listen##*:}"; case "$port" in ""|*[!0-9]*) exit 1 ;; esac; wget -q -O /dev/null "http://127.0.0.1:${port}/healthz" || exit 1'

WORKDIR /app
ENTRYPOINT ["/usr/local/bin/docker-entrypoint.sh"]
