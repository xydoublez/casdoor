#!/bin/bash
# Casdoor 一键编译 + 启动脚本（PostgreSQL 版，PG 实例需自行运行于 127.0.0.1:5432）
#
# 用法:
#   ./start_pg.sh             # 编译并启动（复用已有数据库）
#   ./start_pg.sh fresh       # 删除旧数据库后全新启动（需本机有 psql 客户端）
#
# 说明:
#   - 服务进程以低权限的专用用户 casdoor 运行，不再以 root 或当前登录用户运行
#   - 编译仍以当前用户执行，复用其 ~/go/pkg/mod 模块缓存
#   - 脚本会自动创建 casdoor 用户（不存在时），并使用 sudo 切换身份
#   - 脚本会自动为二进制授予 CAP_NET_BIND_SERVICE，使 casdoor 用户可绑定 389/636/1812 等特权端口
#   - 默认仍使用非特权端口；只有确实需要时才设置为 389/636/1812
#
# 可选环境变量:
#   PORT=8000                 # HTTP 监听端口，默认 8000（非特权）
#   PG_USER=postgres          # 数据库用户，默认 postgres
#   PG_PASSWORD=123456        # 数据库密码
#   PG_HOST=127.0.0.1         # 数据库地址
#   PG_PORT=5432              # 数据库端口
#   DB_NAME=casdoor           # 数据库名
#   LDAP_PORT=1389            # LDAP 端口，0 禁用；默认使用高位端口避免 root
#   LDAPS_PORT=1636           # LDAPS(TLS) 端口，0 禁用；需同时配置 LDAPS_CERT_ID
#   LDAPS_CERT_ID=""          # LDAPS 证书名称（Casdoor 界面「证书」页上传的证书名）
#   RADIUS_PORT=0             # RADIUS 端口(UDP)，0 禁用；默认禁用（1812 为特权端口）
#   RADIUS_SECRET=secret      # RADIUS 共享密钥，与对接设备保持一致
#   RADIUS_ORG=built-in       # RADIUS 请求未带组织时的默认组织
#   SKIP_FRONTEND=0           # 非 0 跳过前端编译（仅编译后端）
#   FORCE_FRONTEND=0          # 非 0 强制重编前端（忽略 mtime 检测）
#   LOG_FILE=logs/casdoor.log # 服务日志落盘文件（终端与文件同时输出）
#   RUN_USER=casdoor          # 服务运行用户，默认 casdoor

set -e
cd "$(dirname "$0")/.."

PORT="${PORT:-8000}"
PG_USER="${PG_USER:-postgres}"
PG_PASSWORD="${PG_PASSWORD:-123456}"
PG_HOST="${PG_HOST:-127.0.0.1}"
PG_PORT="${PG_PORT:-5432}"
DB_NAME="${DB_NAME:-casdoor}"
LDAP_PORT="${LDAP_PORT:-1389}"
LDAPS_PORT="${LDAPS_PORT:-1636}"
LDAPS_CERT_ID="${LDAPS_CERT_ID:-}"
RADIUS_PORT="${RADIUS_PORT:-0}"
RADIUS_SECRET="${RADIUS_SECRET:-secret}"
RADIUS_ORG="${RADIUS_ORG:-built-in}"
SKIP_FRONTEND="${SKIP_FRONTEND:-0}"
FORCE_FRONTEND="${FORCE_FRONTEND:-0}"
LOG_FILE="${LOG_FILE:-logs/casdoor.log}"
RUN_USER="${RUN_USER:-casdoor}"
BINARY="bin/casdoor-server"
CONF_PG="conf/app-pg.conf"

CURRENT_USER="$(id -un)"
CURRENT_UID="$(id -u)"

# 辅助函数：在需要时使用 sudo
_run_as_root() {
    if [ "${CURRENT_UID}" = "0" ]; then
        "$@"
    else
        sudo "$@"
    fi
}

# 检查 PostgreSQL 连通性
if ! (echo > "/dev/tcp/${PG_HOST}/${PG_PORT}") 2>/dev/null; then
    echo "!! 无法连接 PostgreSQL ${PG_HOST}:${PG_PORT}，请确认实例已启动"
    exit 1
fi

# fresh: 删除旧数据库，全新初始化
if [ "${1:-}" = "fresh" ]; then
    if ! command -v psql > /dev/null 2>&1; then
        echo "!! fresh 模式需要 psql 客户端（用于 DROP DATABASE），请先安装 postgresql-client"
        exit 1
    fi
    echo ">> 删除旧数据库: ${DB_NAME}"
    PGPASSWORD="${PG_PASSWORD}" psql -h "${PG_HOST}" -p "${PG_PORT}" -U "${PG_USER}" -d postgres \
        -c "DROP DATABASE IF EXISTS \"${DB_NAME}\" WITH (FORCE);"
fi

# 检测网络，决定是否启用 Go 模块代理（与 build.sh 逻辑一致）
GOPROXY_VALUE="${GOPROXY:-}"
if ! curl www.google.com -o /dev/null --connect-timeout 5 2> /dev/null; then
    echo ">> 无法连接 Google，启用 Go 代理: GOPROXY=https://goproxy.cn,direct"
    GOPROXY_VALUE="https://goproxy.cn,direct"
fi

# 确保服务运行用户存在
if ! id "${RUN_USER}" >/dev/null 2>&1; then
    echo ">> 创建专用运行用户: ${RUN_USER}"
    _run_as_root useradd -r -s /usr/sbin/nologin -d /nonexistent -M "${RUN_USER}"
fi

# 编译（lib/pq 驱动为纯 Go 实现，无需 CGO）
# 先输出到 .tmp 再移动：避免覆盖带有 xattr/CAP 的旧二进制导致 go build 报错
echo ">> 编译 Casdoor ..."
rm -f "${BINARY}.tmp"
CGO_ENABLED=0 GOPROXY="${GOPROXY_VALUE}" go build -o "${BINARY}.tmp" .
mv "${BINARY}.tmp" "${BINARY}"

# 确保运行用户可执行二进制
_run_as_root chown "${RUN_USER}:${RUN_USER}" "${BINARY}"
_run_as_root chmod 750 "${BINARY}"

# 授权二进制绑定特权端口（<1024），使 casdoor 用户能运行 LDAP(389)/LDAPS(636)/RADIUS(1812)
if command -v setcap > /dev/null 2>&1; then
    _run_as_root setcap cap_net_bind_service=+ep "$(pwd)/${BINARY}" || \
        echo ">> 警告: 设置 CAP_NET_BIND_SERVICE 失败，特权端口将不可用（可忽略若未使用 389/636/1812）"
else
    echo ">> 提示: 未安装 setcap(libcap2-bin)，如需绑定特权端口请先安装"
fi

# 基于 conf/app.conf 生成 PostgreSQL 专用配置（不修改原始配置文件）
# PG18 默认 pg_hba.conf 仅允许 hostssl 连接，故使用 sslmode=require
DSN="user=${PG_USER} password=${PG_PASSWORD} host=${PG_HOST} port=${PG_PORT} sslmode=require dbname=${DB_NAME}"
# LDAPS 未配置证书时不监听（Casdoor 要求 ldapsCertId 非空才启动 LDAPS）
if [ -z "${LDAPS_CERT_ID}" ]; then
    LDAPS_PORT=0
fi
# 先生成到临时文件再移动，避免前一次运行后 conf/app-pg.conf 属主为 casdoor 导致写入失败
sed -e "s|^httpport = .*|httpport = ${PORT}|" \
    -e "s|^driverName = .*|driverName = postgres|" \
    -e "s|^dataSourceName = .*|dataSourceName = \"${DSN}\"|" \
    -e "s|^dbName = .*|dbName = ${DB_NAME}|" \
    -e "s|^ldapServerPort = .*|ldapServerPort = ${LDAP_PORT}|" \
    -e "s|^ldapsServerPort = .*|ldapsServerPort = ${LDAPS_PORT}|" \
    -e "s|^ldapsCertId = .*|ldapsCertId = \"${LDAPS_CERT_ID}\"|" \
    -e "s|^radiusServerPort = .*|radiusServerPort = ${RADIUS_PORT}|" \
    -e "s|^radiusSecret = .*|radiusSecret = ${RADIUS_SECRET}|" \
    -e "s|^radiusDefaultOrganization = .*|radiusDefaultOrganization = ${RADIUS_ORG}|" \
    -e "s|^logConfig = .*|logConfig = {\"adapter\":\"console\"}|" \
    conf/app.conf > "${CONF_PG}.tmp"
mv "${CONF_PG}.tmp" "${CONF_PG}"

# 确保日志目录、临时目录、配置文件对运行用户可访问
mkdir -p "$(dirname "${LOG_FILE}")" tmp logs
_run_as_root chown -R "${RUN_USER}:${RUN_USER}" tmp logs "$(dirname "${LOG_FILE}")" "${CONF_PG}"
_run_as_root chmod 755 tmp logs "$(dirname "${LOG_FILE}")"
_run_as_root chmod 640 "${CONF_PG}"

# 如果日志文件已存在，确保运行用户可追加
if [ -f "${LOG_FILE}" ]; then
    _run_as_root chown "${RUN_USER}:${RUN_USER}" "${LOG_FILE}"
    _run_as_root chmod 644 "${LOG_FILE}"
fi

# ---------- 前端：按需编译 web/（staleness 检测） ----------
# 上游已将 web2/（shadcn/ui 版）重命名为 web/，旧的 antd 版前端移至 web-old/。
# 后端仅服务 web/build，因此这里只编译 web/。
# 触发条件（任一为真即编译）:
#   - FORCE_FRONTEND 非 0
#   - <dir>/build/index.html 缺失
#   - <dir>/yarn.lock 或 <dir>/src 任一文件比 <dir>/build/index.html 更新
_need_frontend_build() {
    local dir="$1"
    local target="${dir}/build/index.html"
    [ -z "${FORCE_FRONTEND}" ] || [ "${FORCE_FRONTEND}" = "0" ] || return 0
    [ -f "${target}" ] || return 0
    [ -f "${dir}/yarn.lock" ] && [ "${dir}/yarn.lock" -nt "${target}" ] && return 0
    if [ -d "${dir}/src" ]; then
        [ -z "$(find "${dir}/src" -newer "${target}" -print -quit 2>/dev/null)" ] || return 0
    fi
    return 1
}

if [ "${SKIP_FRONTEND}" = "0" ]; then
    if _need_frontend_build "web"; then
        if ! command -v yarn >/dev/null 2>&1; then
            echo "!! 前端需要编译但 yarn 未安装（请执行: npm install -g yarn）"
            exit 1
        fi
        export NODE_OPTIONS="${NODE_OPTIONS:---max-old-space-size=4096}"

        echo ">> 前端编译: web"
        (cd web && yarn install && yarn run build && touch build/index.html) || { echo "!! web/ 编译失败，详见 web/build.log"; exit 1; }
        echo ">> 前端编译完成"
    else
        echo ">> 前端 build 已是最新，跳过编译（FORCE_FRONTEND=1 可强制重编）"
    fi
fi

# 退出时清理：杀掉服务进程；停止终端日志跟踪
cleanup() {
    if [ -n "${SERVER_PID:-}" ]; then
        _run_as_root kill "${SERVER_PID}" 2>/dev/null || true
    fi
    if [ -n "${TAIL_PID:-}" ]; then
        kill "${TAIL_PID}" 2>/dev/null || true
    fi
}
trap cleanup INT TERM

# 启动服务（以 RUN_USER 身份后台运行，日志直接写入文件）
echo ">> 启动 Casdoor（运行用户: ${RUN_USER}，数据库: postgresql://${PG_HOST}:${PG_PORT}/${DB_NAME}，端口: ${PORT}）..."
if [ "${CURRENT_UID}" = "0" ]; then
    su -s /bin/bash "${RUN_USER}" -c "exec $(pwd)/${BINARY} -config $(pwd)/${CONF_PG} -createDatabase=true > $(pwd)/${LOG_FILE} 2>&1" &
else
    sudo -u "${RUN_USER}" bash -c "exec $(pwd)/${BINARY} -config $(pwd)/${CONF_PG} -createDatabase=true > $(pwd)/${LOG_FILE} 2>&1" &
fi
SERVER_PID=$!

# 当前用户跟踪日志输出到终端
tail -n 0 -f "${LOG_FILE}" &
TAIL_PID=$!

# 等待服务就绪（最长 60 秒）
echo ">> 等待服务启动 ..."
for i in $(seq 1 60); do
    if curl -sf "http://localhost:${PORT}/api/health" > /dev/null 2>&1; then
        echo ""
        echo "=============================================="
        echo " Casdoor 已启动: http://localhost:${PORT}"
        echo " 运行身份: ${RUN_USER}"
        echo " 当前操作身份: ${CURRENT_USER}"
        echo " 日志文件:   ${LOG_FILE}"
        echo " 默认管理员账号: admin / 123（built-in 组织）"
        echo " PostgreSQL: ${PG_USER}@${PG_HOST}:${PG_PORT}/${DB_NAME}"
        if [ "${LDAP_PORT}" != "0" ]; then
            echo " LDAP:       ldap://:${LDAP_PORT}"
        fi
        if [ "${LDAPS_PORT}" != "0" ]; then
            echo " LDAPS:      ldaps://:${LDAPS_PORT}（证书: ${LDAPS_CERT_ID}）"
        fi
        if [ "${RADIUS_PORT}" != "0" ]; then
            echo " RADIUS:     udp/${RADIUS_PORT}（默认组织: ${RADIUS_ORG}）"
        fi
        echo " 按 Ctrl+C 停止服务"
        echo "=============================================="
        break
    fi
    if ! _run_as_root kill -0 "${SERVER_PID}" 2>/dev/null; then
        echo "!! Casdoor 进程异常退出"
        wait "${SERVER_PID}"
    fi
    sleep 1
done

wait "${SERVER_PID}"
