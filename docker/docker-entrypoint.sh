#!/bin/sh
# rosetta-gateway 容器入口脚本。
#
# 职责：首次启动时把默认配置播种到状态目录，以**非 root** 启动网关本体。
#
# 为什么需要它：网关的一切路径都锚在 ROSETTA_GW_HOME 上（镜像里是 /data），
# 而具名卷首次挂载会继承镜像内容、bind mount 不会。要在两种挂载方式下
# 都得到「listen 0.0.0.0:8666」而不是程序默认的回环监听（127.0.0.1:8666），
# 就必须在启动前把模板配置写进去。
#
# 端口为什么是 8666 而不是 6666：6666 在 Chromium 的保留端口表（IRC 段）里，
# 浏览器会在发出请求前就拒绝，报 ERR_UNSAFE_PORT 且服务端无任何日志。
set -eu

STATE_DIR="${ROSETTA_GW_HOME:-/data}"
CONFIG="${STATE_DIR}/config.json"
RUN_UID="${ROSETTA_GW_UID:-1000}"
RUN_GID="${ROSETTA_GW_GID:-1000}"

# 默认时区（2026-10-09 修复「过了 0 点按天数据不切日」的线上缺陷）。
#
# SQLite 的 'localtime'（store.dayExpr，按天分桶的日界）跟 Go 的 time.Local
# 走；容器里没设 TZ、镜像里也没有 /etc/localtime 链接时它是 UTC，于是
# 北京时间 0~8 点的调用全被算进「昨天」（实测：用户 909 条清晨调用
# 全落进前一天的桶）。
#
# 为什么在这里设 TZ 而不是只依赖 config.json 的 timezone 字段：
#   - 已有部署的 config.json 在持久化卷里，升级镜像**不会**自动长出
#     timezone 字段；而 entrypoint 每次启动都跑，TZ 一定生效 ——
#     这是把修复带给存量部署的唯一路径；
#   - TZ 同时影响日志时间戳等一切 Go time 输出；
#   - 用户可用 -e TZ=xxx 覆盖（docker run -e 的优先级高于 ENV/此处默认）。
#
# 注意：config.json 的 timezone 非空时，main 里的 ApplyTimezone 会再次
# 显式改写 time.Local —— 那是最终口径，优先级高于这里的 TZ。
: "${TZ:=Asia/Shanghai}"
export TZ
echo "[entrypoint] TZ=${TZ}（按天统计的日界跟着此时区走）"

seed_config() {
	if [ ! -f "${CONFIG}" ]; then
		cp /app/config.default.json "${CONFIG}"
		echo "[entrypoint] 已生成默认配置: ${CONFIG} (listen 0.0.0.0:8666)"
		echo "[entrypoint] 首次访问管理后台会显示「首次设置密码」表单，请立即设置；"
		echo "[entrypoint] 会话密钥与主密钥都落在 ${STATE_DIR}，请确保该目录持久化。"
	fi
}

# 降权运行。容器以 root 起（需要 chown 状态目录），但网关本体 exec 成普通
# 用户 —— 进程被攻破时拿到的是非特权 uid，而不是容器 root（AUDIT P2-28）。
#
# /data 是卷挂载点，SQLite 库、master.key、session_secret 全在里面，
# 必须对该用户可写。属主不对时先 chown；chown 失败（NFS、只读根 fs、
# 某些 bind mount）再退到 chmod；两者都不行就**明确告警后**才留在 root 下运行 ——
# 静默以 root 运行会让运维以为容器是加固过的。
if [ "$(id -u)" = "0" ]; then
	mkdir -p "${STATE_DIR}"

	# 只在属主确实不对时才动手：bind mount 到已有目录时 chown 可能是昂贵的
	# 递归操作，而多数情况属主已经对了。
	if [ "$(stat -c '%u:%g' "${STATE_DIR}")" != "${RUN_UID}:${RUN_GID}" ]; then
		chown "${RUN_UID}:${RUN_GID}" "${STATE_DIR}" 2>/dev/null ||
			chmod 0700 "${STATE_DIR}" 2>/dev/null || true
	fi

	if [ "$(stat -c '%u' "${STATE_DIR}")" = "${RUN_UID}" ]; then
		seed_config
		chown "${RUN_UID}:${RUN_GID}" "${CONFIG}" 2>/dev/null || true
		echo "[entrypoint] 以非 root 运行（uid=${RUN_UID} gid=${RUN_GID}）"
		exec su-exec "${RUN_UID}:${RUN_GID}" /app/gateway
	fi

	echo "[entrypoint] 警告：无法把 ${STATE_DIR} 的属主改为 ${RUN_UID}:${RUN_GID}，" >&2
	echo "[entrypoint] 将以 root 运行 —— 进程被攻破即持容器 root。" >&2
	echo "[entrypoint] 若宿主机目录可写，请改用：" >&2
	echo "[entrypoint]   -e ROSETTA_GW_UID=\$(id -u) -e ROSETTA_GW_GID=\$(id -g)" >&2
	seed_config
else
	mkdir -p "${STATE_DIR}" 2>/dev/null || true
	seed_config
fi

exec /app/gateway