# syntax=docker/dockerfile:1

# rosetta-gateway 容器镜像（Linux）。
#
# 产物是 CGO_ENABLED=0 的静态链接 Linux 可执行文件 —— modernc.org/sqlite 是纯 Go
# 实现，所以运行层直接用 alpine，没有 glibc/musl 依赖问题。
# 前端产物 internal/webui/dist 已随仓库入库并由 go:embed 打包，构建阶段只需编译 Go。

ARG GO_VERSION=1.27

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
FROM golang:${GO_VERSION}-alpine AS build
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
FROM alpine:3.21

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
VOLUME ["/data"]

# 端口用 8666，**不能用 6666** —— 6666 在 Chromium 的保留端口表
# （net/base/port_util.cc 的 kRestrictedPorts，IRC 段）里，浏览器会在
# 发起请求前就拒绝，报 ERR_UNSAFE_PORT，且服务端看不到任何连接日志。
EXPOSE 8666

# HEALTHCHECK：死锁时容器不会自动重启，编排系统也探不到活性。
# 探 /admin/（静态产物）而不是 /v1/models —— 后者要鉴权，会稳定返回 401，
# 那种「健康」毫无意义。前者只依赖 embed FS，不碰数据库，
# 所以「能返回 200」证明的是**进程还在跑且路由活着**，这正是要探的东西。
#
# 间隔 30s / 超时 5s / 宽限 10s：SQLite 启动剪枝偶发慢，宽限期别太短，
# 否则滚动更新时新容器会被反复判死并重启。
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
	CMD wget -q -O /dev/null http://127.0.0.1:8666/admin/ || exit 1

WORKDIR /app
ENTRYPOINT ["/usr/local/bin/docker-entrypoint.sh"]
