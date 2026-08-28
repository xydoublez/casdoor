#!/bin/bash
# Casdoor 一键编译 + 启动脚本（SQLite 版，用于快速本地测试，无需安装 MySQL/Postgres）
#
# 用法:
#   ./start_sqlite.sh          # 编译并启动（复用已有数据库）
#   ./start_sqlite.sh fresh    # 删除旧数据库后全新启动
#   sudo ./start_sqlite.sh     # 以 root 运行：可绑定 LDAP(389)/LDAPS(636) 等特权端口
#
# 说明:
#   - sudo 运行时，编译仍以原用户身份执行（复用其 Go 模块缓存），仅服务进程以 root 运行
#   - sudo 运行结束后，自动将数据库/日志文件属主还原给原用户
#
# 可选环境变量:
#   PORT=8000                  # 监听端口，默认 8000
#   DB_FILE=casdoor.db         # SQLite 数据库文件路径，默认 ./casdoor.db

set -e
cd "$(dirname "$0")"

PORT="${PORT:-8000}"
DB_FILE="${DB_FILE:-casdoor.db}"
BINARY="bin/casdoor-server"
CONF_SQLITE="conf/app-sqlite.conf"

# sudo 运行时记录原用户，用于以原用户编译、退出时还原文件属主
SUDO_USER_NAME="${SUDO_USER:-}"

# 非 root 运行时，若上次 sudo 运行遗留 root 属主文件（如脚本被 SIGKILL 未能还原），自动修复
if [ "$(id -u)" != "0" ]; then
    for f in "${DB_FILE}" "${DB_FILE}-wal" "${DB_FILE}-shm" "${CONF_SQLITE}"; do
        if [ -e "$f" ] && [ ! -w "$f" ]; then
            sudo -n chown "$(id -un):$(id -gn)" "$f" 2>/dev/null \
                || echo ">> 警告: ${f} 属主为 root，请手动执行: sudo chown $(id -un) ${f}"
        fi
    done
fi

# fresh: 删除旧数据库，全新初始化
if [ "${1:-}" = "fresh" ]; then
    echo ">> 删除旧数据库: ${DB_FILE}"
    rm -f "${DB_FILE}"
fi

# 检测网络，决定是否启用 Go 模块代理（与 build.sh 逻辑一致）
GOPROXY_VALUE="${GOPROXY:-}"
if ! curl www.google.com -o /dev/null --connect-timeout 5 2> /dev/null; then
    echo ">> 无法连接 Google，启用 Go 代理: GOPROXY=https://goproxy.cn,direct"
    GOPROXY_VALUE="https://goproxy.cn,direct"
fi

# 编译（SQLite 驱动为 modernc.org/sqlite 纯 Go 实现，无需 CGO）
echo ">> 编译 Casdoor ..."
if [ "$(id -u)" = "0" ] && [ -n "${SUDO_USER_NAME}" ]; then
    # root 下仍以原用户编译，复用其 ~/go/pkg/mod 模块缓存
    sudo -u "${SUDO_USER_NAME}" -H env CGO_ENABLED=0 GOPROXY="${GOPROXY_VALUE}" go build -o "${BINARY}" .
else
    CGO_ENABLED=0 GOPROXY="${GOPROXY_VALUE}" go build -o "${BINARY}" .
fi

# 基于 conf/app.conf 生成 SQLite 专用配置（不修改原始配置文件）
sed -e "s|^httpport = .*|httpport = ${PORT}|" \
    -e "s|^driverName = .*|driverName = sqlite3|" \
    -e "s|^dataSourceName = .*|dataSourceName = \"file:${DB_FILE}?_pragma=busy_timeout(10000)\&_pragma=journal_mode(WAL)\"|" \
    -e "s|^dbName = .*|dbName = casdoor|" \
    -e "s|^logConfig = .*|logConfig = {\"adapter\":\"console\"}|" \
    conf/app.conf > "${CONF_SQLITE}"

# 退出时清理：杀掉服务进程；root 运行时将产物文件属主还原给原用户
cleanup() {
    kill "${SERVER_PID}" 2>/dev/null
    if [ "$(id -u)" = "0" ] && [ -n "${SUDO_USER_NAME}" ]; then
        chown -R "${SUDO_USER_NAME}" "${DB_FILE}" "${DB_FILE}-wal" "${DB_FILE}-shm" tmp logs "${CONF_SQLITE}" 2>/dev/null || true
    fi
}
trap cleanup INT TERM

if [ "$(id -u)" = "0" ]; then
    echo ">> 以 root 权限启动 Casdoor（数据库: ${DB_FILE}，端口: ${PORT}）..."
else
    echo ">> 提示: LDAP(389)/LDAPS(636) 为特权端口，非 root 运行时 LDAP 服务将启动失败（不影响其他功能）"
    echo ">>       如需完整功能，请运行: sudo ./start_sqlite.sh"
    echo ">> 启动 Casdoor（数据库: ${DB_FILE}，端口: ${PORT}）..."
fi
"${BINARY}" -config "${CONF_SQLITE}" &
SERVER_PID=$!

# 等待服务就绪（最长 60 秒）
echo ">> 等待服务启动 ..."
for i in $(seq 1 60); do
    if curl -sf "http://localhost:${PORT}/api/health" > /dev/null 2>&1; then
        echo ""
        echo "=============================================="
        echo " Casdoor 已启动: http://localhost:${PORT}"
        echo " 运行身份: $(id -un)"
        echo " 默认管理员账号: admin / 123（built-in 组织）"
        echo " SQLite 数据库:  ${DB_FILE}"
        echo " 按 Ctrl+C 停止服务"
        echo "=============================================="
        break
    fi
    if ! kill -0 "${SERVER_PID}" 2>/dev/null; then
        echo "!! Casdoor 进程异常退出"
        wait "${SERVER_PID}"
    fi
    sleep 1
done

wait "${SERVER_PID}"
