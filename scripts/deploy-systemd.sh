#!/usr/bin/env bash
# =============================================================================
# Casdoor 一键部署脚本（已编译二进制 + systemd + 远程 PostgreSQL）
#
# 功能：
#   1. 预检二进制 / systemd / 远程库地址（配置由脚本直接生成，不依赖外部模板/psql）
#   2. 安装到脚本所在目录（就地部署，二进制/conf/web 同目录；已就地则跳过拷贝）
#   3. 生成 casdoor.service（仅依赖网络在线；数据库不可达时由服务自身启动失败，
#      systemd 按 Restart 策略自动重试，无需客户端工具探测）
#   4. enable + start，并做端口/HTTP 健康检查
#
# 用法：
#   sudo PG_HOST=10.0.0.5 ./deploy-systemd.sh        # 部署（可重复执行，用于升级重启）
#   sudo ./deploy-systemd.sh --upgrade               # 升级（自动查找脚本目录下 casdoor.tar.gz）
#   sudo ./deploy-systemd.sh --upgrade /path/to/casdoor.tar.gz  # 指定升级包路径
#   sudo ./deploy-systemd.sh --uninstall             # 停止并卸载服务（保留数据目录）
#   sudo ./deploy-systemd.sh --uninstall --purge     # 同时删除安装目录
#
# 所有变量均可用环境变量覆盖，例如：
#   sudo PG_HOST=10.0.0.5 PG_PASSWORD='xxx' PG_DB=casdoor HTTP_PORT=8000 ./deploy-systemd.sh
#
# 注意：密码等变量支持 # @ ! 等特殊字符；双引号(")、单引号(')、空白、$、`、\ 被禁止。
# =============================================================================
set -Eeuo pipefail

# ---------- 可配置变量（支持环境变量覆盖） ----------
# 就地部署布局：脚本与二进制/web/conf 同目录（如 /msun/app/casdoor）
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
: "${APP_NAME:=casdoor}"
: "${HTTP_PORT:=8000}"
: "${RUN_MODE:=prod}"
: "${SRC_BIN:=${SCRIPT_DIR}/casdoor}"
: "${SRC_INIT_DATA:=${SCRIPT_DIR}/init_data.json}"
: "${INSTALL_DIR:=${SCRIPT_DIR}}"
: "${RUN_USER:=casdoor}"
: "${RUN_GROUP:=casdoor}"
: "${PG_HOST:=}"                      # 远程 PostgreSQL 地址（必填，部署时通过环境变量传入）
: "${PG_PORT:=5432}"
: "${PG_SSLMODE:=require}"            # 默认要求加密连接；若远程 PG 未启用 SSL 可改为 disable
: "${PG_USER:=casdoor_user}"          # 应用连接用户
: "${PG_PASSWORD:=123456}"
: "${PG_DB:=casdoor}"
: "${PG_SEARCH_PATH:=casdoor}"          # PG schema；空=默认 public。lib/pq 支持在 DSN 直传运行时参数
: "${WAIT_TIMEOUT:=60}"               # 服务健康检查最长等待秒数
: "${LOG_DIR:=/msun/logs/casdoor}"    # casdoor 日志目录（logConfig.filename 所在）；空=回退 console->journald
: "${LOG_MAXSIZE:=100}"               # 单文件轮转阈值 MB（beego file writer maxsize）
: "${LOG_MAXDAYS:=30}"                # 轮转保留天数（maxdays，过期自动清理）
# RADIUS 共享密钥：未显式指定时每次部署生成随机值；重复部署保留 conf 中已生成的密钥（幂等）
: "${RADIUS_SECRET:=}"
SERVICE_NAME="${APP_NAME}"
UNIT_FILE="/etc/systemd/system/${SERVICE_NAME}.service"

PG_DSN="user=${PG_USER} password=${PG_PASSWORD} host=${PG_HOST} port=${PG_PORT} sslmode=${PG_SSLMODE} dbname=${PG_DB}"
# search_path 作为 lib/pq 运行时参数追加（startup packet 携带，整会话生效：建表/查询均落在该 schema）
[[ -n "${PG_SEARCH_PATH}" ]] && PG_DSN="${PG_DSN} search_path=${PG_SEARCH_PATH}"

# ---------- 输出辅助 ----------
c_info='\033[0;32m'; c_warn='\033[0;33m'; c_err='\033[0;31m'; c_off='\033[0m'
info() { echo -e "${c_info}[INFO]${c_off} $*"; }
warn() { echo -e "${c_warn}[WARN]${c_off} $*"; }
die()  { echo -e "${c_err}[ERROR]${c_off} $*" >&2; exit 1; }

# 任何命令触发 errexit 时打印行号，避免脚本"无声中止"难以定位（-E 使 ERR 陷阱穿透函数）
trap 'echo -e "${c_err}[ERROR]${c_off} 第 ${LINENO} 行命令失败，脚本中止（详见上方输出）" >&2' ERR

# ---------- 入口：参数解析 + root 自提升 ----------
ACTION="deploy"
PURGE=0
UPGRADE_PKG=""   # 非空时执行升级流程；值为 tar.gz 路径（空串=自动查找）
for arg in "$@"; do
  case "$arg" in
    --uninstall) ACTION="uninstall" ;;
    --purge)     PURGE=1 ;;
    --upgrade)   ACTION="upgrade" ;;
    -h|--help)   sed -n '2,21p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    -*)          die "未知参数: $arg（使用 -h 查看帮助）" ;;
    # --upgrade 后跟路径参数：非 -- 开头且 ACTION 已设为 upgrade，视为包路径
    *) if [[ "${ACTION}" == "upgrade" && -z "${UPGRADE_PKG}" ]]; then
         UPGRADE_PKG="$arg"
       else
         die "未知参数: $arg（使用 -h 查看帮助）"
       fi ;;
  esac
done
[[ "${ACTION}" == "deploy" && ${PURGE} -eq 1 ]] && die "--purge 需与 --uninstall 一起使用"

if [[ ${EUID} -ne 0 ]]; then
  info "当前非 root，尝试通过 sudo 自提升..."
  # 显式透传可覆盖变量（sudo 默认 env_reset 会丢弃自定义环境变量，-E 在部分系统被禁用）
  SUDO_ENV=()
  for v in APP_NAME HTTP_PORT RUN_MODE SRC_BIN SRC_INIT_DATA INSTALL_DIR \
           RUN_USER RUN_GROUP PG_HOST PG_PORT PG_SSLMODE PG_USER PG_PASSWORD PG_DB PG_SEARCH_PATH \
           WAIT_TIMEOUT RADIUS_SECRET LOG_DIR LOG_MAXSIZE LOG_MAXDAYS UPGRADE_PKG; do
    [[ -n "${!v:-}" ]] && SUDO_ENV+=("${v}=${!v}")
  done
  exec sudo env "${SUDO_ENV[@]}" "$0" "$@"
fi

# ---------- 卸载 ----------
do_uninstall() {
  info "停止并禁用 ${SERVICE_NAME} ..."
  systemctl stop "${SERVICE_NAME}" 2>/dev/null || true
  systemctl disable "${SERVICE_NAME}" 2>/dev/null || true
  rm -f "${UNIT_FILE}"
  systemctl daemon-reload
  systemctl reset-failed 2>/dev/null || true
  if [[ ${PURGE:-0} -eq 1 ]]; then
    # 防误删护栏：INSTALL_DIR 为根/一级系统目录时拒绝
    case "${INSTALL_DIR}" in
      /|/usr|/etc|/var|/opt|/home|/root|/msun|/tmp|/boot|/lib|/bin|/sbin)
        die "拒绝 purge：INSTALL_DIR(${INSTALL_DIR}) 为系统关键目录" ;;
    esac
    warn "将删除整个 ${INSTALL_DIR}（含部署脚本、二进制、conf/app.conf 与 web/），重新部署需重新上传文件"
    warn "数据库数据保留，如需彻底清理请手动执行: DROP DATABASE ${PG_DB};"
    rm -rf "${INSTALL_DIR}"
  else
    warn "已保留安装目录 ${INSTALL_DIR}（含 conf/app.conf，重部署复用其中 radiusSecret）"
    warn "如需彻底删除请加 --purge"
  fi
  info "卸载完成。"
}

# 非法字符匹配模式（双引号/单引号/空白/$/`/\），case 模式中展开使用
PAT_BAD='*["'\''$`\\[:space:]]*'

# ---------- 预检 ----------
preflight() {
  info "预检环境..."
  [[ -x "${SRC_BIN}" ]]  || die "二进制不存在或不可执行: ${SRC_BIN}"
  command -v systemctl   >/dev/null || die "未找到 systemctl"
  [[ -d /run/systemd/system ]] || die "systemd 未运行（当前环境非 systemd 管理）"
  [[ -n "${PG_HOST}" ]] || die "未设置远程数据库地址 PG_HOST，例如: sudo PG_HOST=10.0.0.5 ./deploy-systemd.sh"
  [[ "${PG_PORT}"  =~ ^[0-9]+$ ]] || die "PG_PORT 必须为数字: ${PG_PORT}"
  [[ "${HTTP_PORT}" =~ ^[0-9]+$ ]] || die "HTTP_PORT 必须为数字: ${HTTP_PORT}"
  # 配置为逐行生成，# @ ! 等密码字符安全；以下字符会破坏 ini 引号/libpq DSN 解析，需拦截
  local v
  for v in PG_USER PG_PASSWORD PG_DB PG_HOST PG_SSLMODE RADIUS_SECRET; do
    case "${!v}" in
      ${PAT_BAD})
        die "变量 ${v} 含非法字符（双引号/单引号/空白/美元符/反引号/反斜杠），请修改后重试" ;;
    esac
  done
  # schema 名只允许字母数字下划线：特殊字符需 PG 双引号标识符，且含逗号/空白会破坏 search_path 语义
  if [[ -n "${PG_SEARCH_PATH}" && "${PG_SEARCH_PATH}" =~ ^[A-Za-z0-9_]+$ ]]; then :; else
    [[ -n "${PG_SEARCH_PATH}" ]] && die "PG_SEARCH_PATH 仅允许字母数字下划线: ${PG_SEARCH_PATH}"
  fi
  # 日志目录：路径安全字符 + 数值参数校验（logConfig 为 JSON，非法值会启动 panic）
  [[ -n "${LOG_DIR}" ]] && { [[ "${LOG_DIR}" = /* && "${LOG_DIR}" =~ ^[A-Za-z0-9_/.-]+$ ]] || die "LOG_DIR 须为绝对路径且仅含字母数字 _ / . -: ${LOG_DIR}"; }
  [[ "${LOG_MAXSIZE}" =~ ^[0-9]+$ ]] || die "LOG_MAXSIZE 必须为数字(MB): ${LOG_MAXSIZE}"
  [[ "${LOG_MAXDAYS}"  =~ ^[0-9]+$ ]] || die "LOG_MAXDAYS 必须为数字(天): ${LOG_MAXDAYS}"
  info "预检通过：二进制与配置 OK，systemd OK，远程库 ${PG_HOST}:${PG_PORT}。"
}

# ---------- 安装文件 ----------
install_files() {
  info "安装文件到 ${INSTALL_DIR} ..."
  mkdir -p "${INSTALL_DIR}/conf"
  # 运行用户（幂等创建系统用户）
  if ! id "${RUN_USER}" >/dev/null 2>&1; then
    useradd --system --home-dir "${INSTALL_DIR}" --no-create-home --shell /usr/sbin/nologin "${RUN_USER}"
    info "已创建系统用户 ${RUN_USER}。"
  fi
  # 二进制：源与目标为同一文件（就地部署）时跳过拷贝，避免自覆盖损坏
  local dst_bin="${INSTALL_DIR}/casdoor"
  if [[ "$(readlink -f "${SRC_BIN}")" != "$(readlink -f "${dst_bin}")" ]]; then
    install -m 0755 "${SRC_BIN}" "${dst_bin}"
  else
    chmod 0755 "${dst_bin}"
  fi
  # 直接生成完整配置（不依赖任何模板，避免模板缺失/键缺失/内容异常）
  # 键集与源码 conf/app-pg.conf 一致；echo 逐行输出，密码含特殊字符也安全
  # radiusSecret：未传入时——复用现有 conf 值（重部署不变），否则生成 32 位强随机字符
  # 字符集 A-Za-z0-9!@#$%^&*（约 195 bit 熵）；已实测 beego ini 解析器完整保留特殊字符
  # 注意：字符集勿加 "-_/()" 等 —— "-"/"/" 会破坏 sed 复用与 DSN 语义，"()" 与 "-" 组合会被 tr 当区间
  if [[ -z "${RADIUS_SECRET}" && -r "${INSTALL_DIR}/conf/app.conf" ]]; then
    # awk 单进程取首行即 exit：避免 "sed | head" 在极端情况下（conf 含重复键）head 提前退出触发 SIGPIPE
    RADIUS_SECRET="$(awk '/^radiusSecret = /{sub(/^radiusSecret = /,""); print; exit}' "${INSTALL_DIR}/conf/app.conf")"
  fi
  if [[ -z "${RADIUS_SECRET}" ]]; then
    # 勿写成 "tr ... </dev/urandom | head -c 32"：head 提前退出会令 tr 收到 SIGPIPE(141)，
    # 在 set -o pipefail 下整个赋值失败、脚本静默中止（已实测复现）。
    # 改为有限输入流：head 读 4096 字节后正常退出，tr 消费至 EOF，无下游消费者，不会 SIGPIPE
    RADIUS_SECRET="$(head -c 4096 /dev/urandom | tr -dc 'A-Za-z0-9!@#$%^&*')"
    RADIUS_SECRET="${RADIUS_SECRET:0:32}"
    info "已生成随机 RADIUS 共享密钥（请同步到 NAS/网络设备）: ${RADIUS_SECRET}"
  fi
  {
    echo "appname = ${APP_NAME}"
    echo "httpport = ${HTTP_PORT}"
    echo "runmode = ${RUN_MODE}"
    echo "copyrequestbody = true"
    echo "driverName = postgres"
    echo "dataSourceName = \"${PG_DSN}\""
    echo "dbName = ${PG_DB}"
    echo "tableNamePrefix ="
    echo "showSql = false"
    echo "redisEndpoint ="
    echo "defaultStorageProvider ="
    echo "isCloudIntranet = false"
    echo "authState = \"casdoor\""
    echo "socks5Proxy ="                                   # 清空：生产机无本地开发代理
    echo "verificationCodeTimeout = 10"
    echo "initScore = 0"
    echo "logPostOnly = true"
    echo "isUsernameLowered = false"
    echo "origin ="
    echo "originFrontend ="
    echo "staticBaseUrl = \"https://cdn.casbin.org\""
    echo "isDemoMode = false"
    echo "batchSize = 100"
    echo "showGithubCorner = false"
    echo "forceLanguage = \"\""
    echo "defaultLanguage = \"en\""
    echo "aiAssistantUrl = \"https://ai.casbin.com\""
    echo "defaultApplication = \"app-built-in\""
    echo "maxItemsForFlatMenu = 7"
    echo "enableErrorMask = false"
    echo "enableGzip = true"
    echo "inactiveTimeoutMinutes ="
    echo "ldapServerPort = 389"
    echo "ldapsCertId = \"\""
    echo "ldapsServerPort = 0"
    echo "radiusServerPort = 1812"
    echo "radiusDefaultOrganization = built-in"
    echo "radiusSecret = ${RADIUS_SECRET}"
    echo 'quota = {"organization": -1, "user": -1, "application": -1, "provider": -1}'
    # 日志落文件：main.go 用 SetLogger(logAdapter, logConfig) 初始化，file writer 自动建目录、
    # maxsize(MB)/maxdays 轮转清理（beego/core/logs/file.go）；LOG_DIR 空则回退 console->journald
    if [[ -n "${LOG_DIR}" ]]; then
      echo "logConfig = {\"adapter\":\"file\",\"filename\":\"${LOG_DIR}/casdoor.log\",\"maxsize\":${LOG_MAXSIZE},\"maxdays\":${LOG_MAXDAYS},\"perm\":\"0640\",\"dirperm\":\"0750\"}"
    else
      echo 'logConfig = {"adapter":"console"}'             # console -> journald 接管
    fi
    echo "initDataNewOnly = false"
    echo "initDataFile = \"./init_data.json\""
    echo "frontendBaseDir ="                               # 空 -> 使用工作目录 web/build
  } > "${INSTALL_DIR}/conf/app.conf"
  chmod 0640 "${INSTALL_DIR}/conf/app.conf"
  # init_data.json（如存在则一并部署， initDataFile 为相对工作目录路径；同文件跳过）
  if [[ -r "${SRC_INIT_DATA}" && "$(readlink -f "${SRC_INIT_DATA}")" != "$(readlink -f "${INSTALL_DIR}/init_data.json")" ]]; then
    install -m 0640 "${SRC_INIT_DATA}" "${INSTALL_DIR}/init_data.json"
  fi
  chown -R "${RUN_USER}:${RUN_GROUP}" "${INSTALL_DIR}"
  chmod 0750 "${INSTALL_DIR}"
  # 日志目录：提前创建并授权（file writer 虽会 MkdirAll，但按 dirperm 属 root 时 casdoor 无权建子目录）
  if [[ -n "${LOG_DIR}" ]]; then
    mkdir -p "${LOG_DIR}"
    chown "${RUN_USER}:${RUN_GROUP}" "${LOG_DIR}"
    chmod 0750 "${LOG_DIR}"
  fi
  info "文件安装完成（conf: ${INSTALL_DIR}/conf/app.conf）。"
}

# ---------- systemd 单元 ----------
write_unit() {
  info "写入 systemd 单元 ${UNIT_FILE} ..."
  cat > "${UNIT_FILE}" <<EOF
[Unit]
Description=Casdoor IAM Server
Documentation=https://casdoor.org
# 数据库位于远程服务器：不依赖本地服务，仅依赖网络在线
After=network-online.target
Wants=network-online.target
# 不限重启次数：远程数据库故障导致启动失败时持续自动重试，直至恢复
StartLimitIntervalSec=0

[Service]
Type=simple
User=${RUN_USER}
Group=${RUN_GROUP}
WorkingDirectory=${INSTALL_DIR}
Environment=HOME=${INSTALL_DIR}
ExecStart=${INSTALL_DIR}/casdoor
Restart=on-failure
RestartSec=5s
KillSignal=SIGTERM
TimeoutStopSec=30
LimitNOFILE=65536
# LDAP 389/636 为特权端口(<1024)，非 root 无法绑定；仅授予绑定特权端口能力，无需 root
AmbientCapabilities=CAP_NET_BIND_SERVICE
CapabilityBoundingSet=CAP_NET_BIND_SERVICE
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=full
# read-only 而非 true：lib/pq 连接时按 /etc/passwd 主目录探测 ~/.postgresql/postgresql.crt，
# 完全屏蔽 /home 会让 stat 返回“权限拒绝”导致启动 panic；只读模式下不存在则正常跳过
ProtectHome=read-only

[Install]
WantedBy=multi-user.target
EOF
  systemctl daemon-reload
  systemctl enable "${SERVICE_NAME}" >/dev/null
  info "systemd 单元已写入，已设置开机自启。"
}

# ---------- 启动 + 健康检查 ----------
start_and_verify() {
  info "启动 ${SERVICE_NAME} ..."
  systemctl restart "${SERVICE_NAME}"
  info "等待服务监听 ${HTTP_PORT} 端口（最长 ${WAIT_TIMEOUT}s）..."
  local i=0
  until (exec 3<>"/dev/tcp/127.0.0.1/${HTTP_PORT}") 2>/dev/null; do
    ((i+=2)); [[ ${i} -ge ${WAIT_TIMEOUT} ]] && { systemctl status "${SERVICE_NAME}" --no-pager -l || true; journalctl -u "${SERVICE_NAME}" -n 50 --no-pager || true; die "服务启动超时，请检查上方日志"; }
    sleep 2
  done
  info "端口 ${HTTP_PORT} 已监听。"
  if command -v curl >/dev/null; then
    local code
    code=$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:${HTTP_PORT}/api/health" || true)
    info "健康检查 /api/health -> HTTP ${code:-N/A}"
  fi
  systemctl --no-pager -l status "${SERVICE_NAME}" | head -n 12 || true
  echo
  echo "=============================================================="
  echo -e "${c_info} 部署成功！${c_off}"
  echo "   访问地址   : http://<服务器IP>:${HTTP_PORT}"
  echo "   配置文件   : ${INSTALL_DIR}/conf/app.conf"
  [[ -n "${LOG_DIR}" ]] && echo "   日志文件   : ${LOG_DIR}/casdoor.log（maxsize=${LOG_MAXSIZE}MB, maxdays=${LOG_MAXDAYS}）"
  echo "   数据库     : postgresql://${PG_USER}@${PG_HOST}:${PG_PORT}/${PG_DB} (schema=${PG_SEARCH_PATH:-public}, sslmode=${PG_SSLMODE})"
  echo "   查看状态   : systemctl status ${SERVICE_NAME}"
  echo "   查看日志   : journalctl -u ${SERVICE_NAME} -f"
  echo "   升级       : 将 casdoor.tar.gz 放到 ${SCRIPT_DIR} 后执行 sudo ./deploy-systemd.sh --upgrade"
  echo "=============================================================="
}

# ---------- 升级 ----------
do_upgrade() {
  info "开始升级流程..."

  # ---- 1. 定位升级包 ----
  local pkg="${UPGRADE_PKG}"
  if [[ -z "${pkg}" ]]; then
    # 自动查找脚本目录 / 当前目录下 casdoor.tar.gz
    local candidate
    for candidate in "${SCRIPT_DIR}/casdoor.tar.gz" "$(pwd)/casdoor.tar.gz"; do
      [[ -f "${candidate}" ]] && { pkg="${candidate}"; break; }
    done
  fi
  [[ -f "${pkg}" ]] || die "未找到升级包，请将 casdoor.tar.gz 放到 ${SCRIPT_DIR} 或通过 --upgrade /path/to/pkg.tar.gz 指定"
  info "升级包: ${pkg}"

  # ---- 2. 预检 ----
  command -v systemctl >/dev/null || die "未找到 systemctl"
  [[ -d /run/systemd/system ]] || die "systemd 未运行"
  # 检查升级包中是否包含可执行文件（tar 列出即可，不实际解压到磁盘）
  if ! tar tzf "${pkg}" 2>/dev/null | grep -qiE '(^|/)casdoor$'; then
    die "升级包中未找到 casdoor 可执行文件，请确认包内容正确"
  fi

  # ---- 3. 备份当前版本 ----
  local dst_bin="${INSTALL_DIR}/casdoor"
  local backup_dir="${INSTALL_DIR}/backup"
  mkdir -p "${backup_dir}"
  if [[ -x "${dst_bin}" ]]; then
    local backup_file="${backup_dir}/casdoor.bak.$(date +%Y%m%d_%H%M%S)"
    cp -p "${dst_bin}" "${backup_file}"
    info "已备份当前版本: ${backup_file}"
    # 仅保留最近 5 个备份，防止磁盘占满
    local old_backups
    old_backups="$(ls -1t "${backup_dir}"/casdoor.bak.* 2>/dev/null | tail -n +6)"
    if [[ -n "${old_backups}" ]]; then
      echo "${old_backups}" | xargs rm -f
      info "已清理多余旧备份（保留最近 5 个）"
    fi
  else
    warn "当前无可执行二进制 ${dst_bin}，跳过备份（可能为首次部署）"
    backup_file=""
  fi

  # ---- 4. 停止服务 ----
  info "停止 ${SERVICE_NAME} ..."
  systemctl stop "${SERVICE_NAME}" 2>/dev/null || true

  # ---- 5. 解压新版本并替换 ----
  # 使用临时目录解压，避免直接解压到 INSTALL_DIR 覆盖其他文件
  local tmp_dir
  tmp_dir="$(mktemp -d "${INSTALL_DIR}/upgrade.XXXXXX")"
  # 确保临时目录在脚本退出时被清理
  trap 'rm -rf "${tmp_dir}" 2>/dev/null' RETURN

  info "解压升级包到临时目录..."
  tar xzf "${pkg}" -C "${tmp_dir}"

  # 在解压结果中查找 casdoor 二进制（支持包内有一级子目录的情况）
  local new_bin
  new_bin="$(find "${tmp_dir}" -name casdoor -type f | head -1)"
  if [[ -z "${new_bin}" ]]; then
    # 解压失败 → 回滚
    if [[ -n "${backup_file:-}" && -x "${backup_file}" ]]; then
      cp -p "${backup_file}" "${dst_bin}"
      systemctl start "${SERVICE_NAME}" 2>/dev/null || true
    fi
    die "升级包中未找到 casdoor 二进制文件，已回滚"
  fi

  # 替换二进制
  install -m 0755 "${new_bin}" "${dst_bin}"
  chown "${RUN_USER}:${RUN_GROUP}" "${dst_bin}" 2>/dev/null || true
  info "已替换二进制: ${dst_bin}"

  # 如果包中还有 web/ 目录，一并更新前端静态资源
  local new_web
  new_web="$(find "${tmp_dir}" -maxdepth 2 -type d -name web | head -1)"
  if [[ -n "${new_web}" && -d "${new_web}" ]]; then
    if [[ -d "${INSTALL_DIR}/web" ]]; then
      rm -rf "${INSTALL_DIR}/web.bak"
      mv "${INSTALL_DIR}/web" "${INSTALL_DIR}/web.bak"
      info "已备份旧 web/ 目录 -> web.bak"
    fi
    cp -a "${new_web}" "${INSTALL_DIR}/web"
    chown -R "${RUN_USER}:${RUN_GROUP}" "${INSTALL_DIR}/web"
    info "已更新 web/ 前端静态资源"
  fi

  # 清理临时目录（trap 兜底）
  rm -rf "${tmp_dir}"
  trap - RETURN

  # ---- 6. 启动并健康检查 ----
  info "启动 ${SERVICE_NAME} ..."
  systemctl start "${SERVICE_NAME}"
  info "等待服务监听 ${HTTP_PORT} 端口（最长 ${WAIT_TIMEOUT}s）..."
  local i=0
  until (exec 3<>"/dev/tcp/127.0.0.1/${HTTP_PORT}") 2>/dev/null; do
    ((i+=2)); [[ ${i} -ge ${WAIT_TIMEOUT} ]] && {
      # 启动失败 → 回滚
      if [[ -n "${backup_file:-}" && -x "${backup_file}" ]]; then
        warn "新版本启动失败，正在回滚到旧版本..."
        systemctl stop "${SERVICE_NAME}" 2>/dev/null || true
        cp -p "${backup_file}" "${dst_bin}"
        chown "${RUN_USER}:${RUN_GROUP}" "${dst_bin}" 2>/dev/null || true
        systemctl start "${SERVICE_NAME}" 2>/dev/null || true
        info "已回滚到旧版本，请检查升级包是否正确"
      fi
      systemctl status "${SERVICE_NAME}" --no-pager -l || true
      journalctl -u "${SERVICE_NAME}" -n 50 --no-pager || true
      die "升级后服务启动超时，已回滚（详见上方日志）"
    }
    sleep 2
  done
  info "端口 ${HTTP_PORT} 已监听。"
  if command -v curl >/dev/null; then
    local code
    code=$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:${HTTP_PORT}/api/health" || true)
    info "健康检查 /api/health -> HTTP ${code:-N/A}"
  fi
  systemctl --no-pager -l status "${SERVICE_NAME}" | head -n 12 || true
  echo
  echo "=============================================================="
  echo -e "${c_info} 升级成功！${c_off}"
  echo "   升级包     : ${pkg}"
  [[ -n "${backup_file:-}" ]] && echo "   旧版备份   : ${backup_file}"
  echo "   访问地址   : http://<服务器IP>:${HTTP_PORT}"
  echo "   查看状态   : systemctl status ${SERVICE_NAME}"
  echo "   查看日志   : journalctl -u ${SERVICE_NAME} -f"
  echo "   回滚命令   : cp ${backup_file:-<备份文件>} ${dst_bin} && systemctl restart ${SERVICE_NAME}"
  echo "=============================================================="
}

# ---------- 主流程 ----------
main() {
  if [[ "${ACTION}" == "uninstall" ]]; then
    do_uninstall
    return
  fi
  if [[ "${ACTION}" == "upgrade" ]]; then
    do_upgrade
    return
  fi
  preflight
  install_files
  write_unit
  start_and_verify
}

main
