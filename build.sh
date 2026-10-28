#!/bin/bash
# Casdoor 构建脚本（sfx 定制版）
#   1. 静态编译后端（CGO_ENABLED=0，-s -w 去除符号表与调试信息，-trimpath 去构建机路径）
#   2. 打包生产发布包（二进制 + conf + web/build + swagger + VERSION.txt + deploy-systemd.sh）
#
# 用法:
#   ./build.sh                    # 编译双架构二进制并打包（默认打包本机架构）
#   PKG_ARCH=arm64 ./build.sh     # 打包 arm64 版本的发布包
#   SFX_VERSION=xxx ./build.sh    # 覆盖自动生成的定制版本号
#   BUILD_DATE=20261028 ./build.sh # 覆盖版本尾部的日期戳（默认取当天）
#
# 产物:
#   sfx_release+<git describe>+<YYYYMMDD>-linux-<arch>.tar.gz   定制发布包（主产物）
#   casdoor.tar.gz                                             同内容副本，供 deploy-systemd.sh --upgrade 自动发现
#
# 说明:
#   - server_linux_amd64 / server_linux_arm64 两个文件名被 Dockerfile 引用，勿改名
#   - Docker 构建阶段（无 node/yarn）会自动跳过打包，前端由 Dockerfile FRONT 阶段提供
#   - 升级生产环境（含 v3 -> v4）统一使用包内 deploy-systemd.sh --upgrade
set -e

#try to connect to google to determine whether user need to use proxy
if curl www.google.com -o /dev/null --connect-timeout 5 2> /dev/null; then
    echo "Successfully connected to Google, no need to use Go proxy"
else
    echo "Google is blocked, Go proxy is enabled: GOPROXY=https://goproxy.cn,direct"
    export GOPROXY="https://goproxy.cn,direct"
fi

# ---------- 版本信息（ldflags 注入，见 /api/get-version-info） ----------
# 注意: -X 仅支持 string 变量，util.CommitOffset 为 int 无法注入（保持默认 0）
#
# 定制版本号规则: sfx_release+<git describe>+<当天日期 YYYYMMDD>
#   例: sfx_release+v4.24.0-37-gcb05f10b+20261028
#   - 前缀 sfx_release+ 用于和官方 casdoor 发行版区分（生产环境为 v3，本仓库已合并到 v4）
#   - 尾部日期戳使同一天内多次构建、以及不同日期的同代码构建都能一眼区分
#   - 该字符串带非 vX.Y.Z 形态，因此 upgrade.parseVersion() 无法解析，
#     HasNewVersion 恒为 false，应用内“自升级到官方 release”不会触发，
#     定制内容不会被官方包覆盖；升级一律走 deploy-systemd.sh --upgrade
#   - 可用环境变量 SFX_VERSION 覆盖
BASE_VERSION="$(git describe --tags --always --dirty 2>/dev/null || echo unknown)"
COMMIT_ID="$(git rev-parse HEAD 2>/dev/null || echo unknown)"
# 日期戳取构建机本地日期（“当天日期”），BUILD_TIME 保留 UTC 精确时刻
BUILD_DATE="${BUILD_DATE:-$(date '+%Y%m%d')}"
SFX_VERSION="${SFX_VERSION:-sfx_release+${BASE_VERSION}+${BUILD_DATE}}"
BUILD_TIME="$(date -u '+%Y-%m-%dT%H:%M:%SZ')"

case "${BASE_VERSION}" in
    *-dirty) echo "!! 警告: 工作区存在未提交改动（${BASE_VERSION}），正式发布请先提交或 stash" ;;
esac

LDFLAGS="-s -w -X github.com/casdoor/casdoor/util.Version=${SFX_VERSION} -X github.com/casdoor/casdoor/util.CommitId=${COMMIT_ID}"

# ---------- 后端：静态编译（无 CGO 依赖，可在裸机直接运行） ----------
echo ">> 编译后端: version=${SFX_VERSION}, commitId=${COMMIT_ID}, buildTime=${BUILD_TIME}"
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="${LDFLAGS}" -o server_linux_amd64 .
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags="${LDFLAGS}" -o server_linux_arm64 .

# 静态链接校验（file 不存在时跳过，CGO_ENABLED=0 本身已保证）
for bin in server_linux_amd64 server_linux_arm64; do
    if command -v file >/dev/null 2>&1; then
        file "${bin}" | grep -q "statically linked" || { echo "!! 校验失败: ${bin} 不是静态链接"; exit 1; }
    fi
done
echo ">> 后端编译完成，静态链接校验通过"

# ---------- 前端：缺少构建产物时自动编译 ----------
if [ ! -f web/build/index.html ] && command -v yarn >/dev/null 2>&1; then
    echo ">> web/build 不存在，编译前端 ..."
    (cd web && yarn install --frozen-lockfile --network-timeout 1000000 && NODE_OPTIONS="--max-old-space-size=4096" yarn run build)
fi

if [ ! -f web/build/index.html ]; then
    if [ -f /.dockerenv ]; then
        # Dockerfile BACK 阶段无 node，前端由 FRONT 阶段单独构建，无需发布包
        echo ">> Docker 构建阶段无 web/build，跳过发布包打包"
        exit 0
    fi
    echo "!! web/build 缺失且无 yarn 可编译前端，无法生成发布包"
    exit 1
fi

# ---------- 组装发布包 casdoor.tar.gz ----------
PKG_ARCH="${PKG_ARCH:-$(go env GOARCH)}"
case "${PKG_ARCH}" in
    amd64) BIN="server_linux_amd64" ;;
    arm64) BIN="server_linux_arm64" ;;
    *) echo "!! 不支持的打包架构: ${PKG_ARCH}（仅支持 amd64/arm64）"; exit 1 ;;
esac

STAGE="$(mktemp -d)"
trap 'rm -rf "${STAGE}"' EXIT
PKG="${STAGE}/casdoor"
mkdir -p "${PKG}/conf" "${PKG}/web" "${PKG}/logs"

# 发布包文件名带定制版本号，便于与官方包及其他构建区分
SFX_PKG_NAME="${SFX_VERSION}-linux-${PKG_ARCH}.tar.gz"

install -m 0755 "${BIN}" "${PKG}/casdoor"

# 生产配置：runmode 改为 prod；清空仅本地开发使用的 socks5 代理与前端目录覆盖
# （源文件可能为 CRLF 行尾，先归一化为 LF 再替换，避免 $ 锚点失配）
# （deploy-systemd.sh 部署时会重新生成完整配置，此处保证手动运行开箱即用）
# 追加 disableUpgrade = true：定制版禁止应用内自升级到官方 release，
# 否则会用官方二进制覆盖掉本地定制内容；升级统一走 deploy-systemd.sh --upgrade
sed -e $'s/\r$//' \
    -e 's/^runmode = .*/runmode = prod/' \
    -e 's/^socks5Proxy = .*/socks5Proxy =/' \
    -e 's/^frontendBaseDir = .*/frontendBaseDir =/' \
    conf/app.conf > "${PKG}/conf/app.conf"
printf '\n# sfx 定制发布：禁止应用内自升级到官方 release（防止覆盖定制内容）\ndisableUpgrade = true\n' >> "${PKG}/conf/app.conf"
chmod 0640 "${PKG}/conf/app.conf"

cp -a web/build "${PKG}/web/build"
cp -a swagger "${PKG}/swagger"
install -m 0644 init_data.json.template "${PKG}/init_data.json.template"

# 一键部署脚本（源码目录存在则随包分发，解压后即可 sudo PG_HOST=... ./deploy-systemd.sh）
# 本仓库该脚本位于 scripts/ 下，历史上也可能直接放在根目录，两处都兼容
for deploy_script in deploy-systemd.sh scripts/deploy-systemd.sh; do
    if [ -f "${deploy_script}" ]; then
        install -m 0755 "${deploy_script}" "${PKG}/deploy-systemd.sh"
        break
    fi
done
if [ ! -f "${PKG}/deploy-systemd.sh" ]; then
    echo "!! 警告: 未找到 deploy-systemd.sh，发布包将不含一键部署/升级脚本"
fi

# 版本清单：便于生产环境核对当前部署的到底是哪个定制版本
cat > "${PKG}/VERSION.txt" <<EOF
version     = ${SFX_VERSION}
baseVersion = ${BASE_VERSION}
buildDate   = ${BUILD_DATE}
commitId    = ${COMMIT_ID}
buildTime   = ${BUILD_TIME}
arch        = linux/${PKG_ARCH}
EOF
chmod 0644 "${PKG}/VERSION.txt"

cat > "${PKG}/README.txt" <<EOF
Casdoor 生产发布包 - sfx 定制版 (linux/${PKG_ARCH})
==================================================

版本: ${SFX_VERSION}
提交: ${COMMIT_ID}
构建: ${BUILD_TIME}

目录结构（运行时以本目录为工作目录，按相对路径加载 conf/ 与 web/）:
  casdoor                  服务端二进制（静态编译，无 CGO/系统库依赖）
  VERSION.txt              版本清单（version / commitId / buildTime / arch）
  conf/app.conf            生产配置（runmode=prod，disableUpgrade=true，数据库连接需按环境修改）
  web/build/               前端静态资源
  swagger/                 API 文档静态资源（/swagger）
  init_data.json.template  初始数据模板（可选：复制为 init_data.json 启用初始数据导入）
  deploy-systemd.sh        一键部署/升级脚本（如包含）
  logs/                    运行日志目录（conf 中 logConfig 指定）

快速部署（Systemd + 远程 PostgreSQL，推荐）:
  tar xzf ${SFX_PKG_NAME}
  cd casdoor
  sudo PG_HOST=<数据库地址> PG_USER=<用户> PG_PASSWORD=<密码> ./deploy-systemd.sh

手动运行:
  1. 编辑 conf/app.conf（driverName / dataSourceName / dbName 等数据库配置）
  2. 启动: ./casdoor

默认监听 http://0.0.0.0:8000，默认管理员 admin / 123（built-in 组织，首次登录后请立即修改）。

升级（v3 生产环境升级到本 v4 定制版同样走此路径）:
  1. 将新的发布包放到部署目录（脚本会自动查找同目录下的 casdoor.tar.gz）
  2. 执行 sudo ./deploy-systemd.sh --upgrade
     或指定包路径 sudo ./deploy-systemd.sh --upgrade ${SFX_PKG_NAME}
  升级会先备份旧二进制（保留最近 5 份），替换失败或启动超时会自动回滚。
  数据库表结构由服务启动时的 xorm sync2 自动增量迁移，无需手工执行 SQL。
  注意: conf/app.conf 为 prod 配置且 disableUpgrade=true，应用内“自升级到官方 release”
        已关闭，避免官方二进制覆盖定制内容。
EOF

tar -C "${STAGE}" --owner=0 --group=0 -czf "${SFX_PKG_NAME}" casdoor
# casdoor.tar.gz 为 deploy-systemd.sh --upgrade 的默认自动查找名，同步生成一份，
# 保证“把包丢到部署目录直接 --upgrade”的既有习惯不被破坏
cp -f "${SFX_PKG_NAME}" casdoor.tar.gz

echo ">> 发布包已生成:"
echo "   - ${SFX_PKG_NAME}（linux/${PKG_ARCH}，$(du -h "${SFX_PKG_NAME}" | cut -f1)）"
echo "   - casdoor.tar.gz（同内容副本，供 deploy-systemd.sh --upgrade 自动发现）"
echo ">> 版本: ${SFX_VERSION}"
tar tzf "${SFX_PKG_NAME}" | head -8
