# syntax=docker/dockerfile:1

# rosetta-gateway 容器镜像（Linux）。
#
# 产物是 CGO_ENABLED=0 的静态链接 Linux 可执行文件 —— modernc.org/sqlite 是纯 Go
# 实现，所以运行层直接用 alpine，没有 glibc/musl 依赖问题。
# 前端产物 internal/webui/dist 已随仓库入库并由 go:embed 打包，构建阶段只需编译 Go。

ARG GO_VERSION=1.27

# ---- 构建阶段 -------------------------------------------------------------
# 固定跑在构建机架构上（$BUILDPLATFORM），靠 Go 自身的交叉编译产出目标架构二进制。
# 这样多架构构建时只有 COPY 在换架构，不必把整个 go build 丢进 QEMU 模拟 ——
# 后者会让 arm64 构建慢一个数量级。
FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine AS build
WORKDIR /src

# 依赖单独成层：go.mod / go.sum 没变就命中缓存
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev

RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
      go build -trimpath \
      -ldflags "-s -w -X main.buildVersion=${VERSION}" \
      -o /out/gateway ./cmd/gateway

# ---- 运行阶段 -------------------------------------------------------------
FROM alpine:3.21

# ca-certificates：访问上游 HTTPS 必需；tzdata：日志时间戳按容器时区显示
RUN apk add --no-cache ca-certificates tzdata

COPY --from=build /out/gateway /app/gateway
COPY docker/config.default.json /app/config.default.json
COPY docker/docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh
RUN chmod 0755 /app/gateway /usr/local/bin/docker-entrypoint.sh

# 全部可变状态（config.json / master.key / admin_auth.json / db）都锚在这个目录上。
# 镜像层因此完全无状态：升级镜像不会碰配置与数据。
ENV ROSETTA_GW_HOME=/data
VOLUME ["/data"]

EXPOSE 6666

WORKDIR /app
ENTRYPOINT ["/usr/local/bin/docker-entrypoint.sh"]
