#!/bin/sh
# rosetta-gateway 容器入口脚本。
#
# 唯一职责：首次启动时把默认配置播种到状态目录，然后启动网关本体。
#
# 为什么需要它：网关的一切路径都锚在 ROSETTA_GW_HOME 上（镜像里是 /data），
# 而具名卷首次挂载会继承镜像内容、bind mount 不会。要在两种挂载方式下
# 都得到「listen 0.0.0.0:8666」而不是程序自动生成的 127.0.0.1:8080，
# 就必须在启动前把模板配置写进去。
#
# 端口为什么是 8666 而不是 6666：6666 在 Chromium 的保留端口表（IRC 段）里，
# 浏览器会在发出请求前就拒绝，报 ERR_UNSAFE_PORT 且服务端无任何日志。
set -eu

STATE_DIR="${ROSETTA_GW_HOME:-/data}"
CONFIG="${STATE_DIR}/config.json"

mkdir -p "${STATE_DIR}"

if [ ! -f "${CONFIG}" ]; then
  cp /app/config.default.json "${CONFIG}"
  echo "[entrypoint] 已生成默认配置: ${CONFIG} (listen 0.0.0.0:8666)"
  echo "[entrypoint] 首次访问管理后台会要求设置密码，请立即设置；"
  echo "[entrypoint] 或用 -e ADMIN_TOKEN=xxx 先设一个兜底令牌。"
fi

exec /app/gateway
