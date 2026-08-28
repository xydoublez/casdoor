#!/bin/bash
# Casdoor 构建脚本
#   1. 静态编译后端（CGO_ENABLED=0，-s -w 去除符号表与调试信息，-trimpath 去构建机路径）
#   2. 打包生产发布包 casdoor.tar.gz（二进制 + conf + web/build + swagger）
#
# 用法:
#   ./build.sh                    # 编译双架构二进制并打包（默认打包本机架构）
#   PKG_ARCH=arm64 ./build.sh     # 打包 arm64 版本的发布包
#
# 说明:
#   - server_linux_amd64 / server_linux_arm64 两个文件名被 Dockerfile 引用，勿改名
#   - Docker 构建阶段（无 node/yarn）会自动跳过打包，前端由 Dockerfile FRONT 阶段提供
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
VERSION="dev"
COMMIT_ID="unknown"
if git rev-parse HEAD >/dev/null 2>&1; then
    COMMIT_ID="$(git rev-parse HEAD)"
    VERSION="$(git describe --tags --always --dirty 2>/dev/null || echo dev)"
fi

LDFLAGS="-s -w -X github.com/casdoor/casdoor/util.Version=${VERSION} -X github.com/casdoor/casdoor/util.CommitId=${COMMIT_ID}"

# ---------- 后端：静态编译（无 CGO 依赖，可在裸机直接运行） ----------
echo ">> 编译后端: version=${VERSION}, commitId=${COMMIT_ID}"
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

install -m 0755 "${BIN}" "${PKG}/casdoor"

# 生产配置：runmode 改为 prod；清空仅本地开发使用的 socks5 代理与前端目录覆盖
# （源文件可能为 CRLF 行尾，先归一化为 LF 再替换，避免 $ 锚点失配）
# （deploy-systemd.sh 部署时会重新生成完整配置，此处保证手动运行开箱即用）
sed -e $'s/\r$//' \
    -e 's/^runmode = .*/runmode = prod/' \
    -e 's/^socks5Proxy = .*/socks5Proxy =/' \
    -e 's/^frontendBaseDir = .*/frontendBaseDir =/' \
    conf/app.conf > "${PKG}/conf/app.conf"
chmod 0640 "${PKG}/conf/app.conf"

cp -a web/build "${PKG}/web/build"
cp -a swagger "${PKG}/swagger"
install -m 0644 init_data.json.template "${PKG}/init_data.json.template"

# 一键部署脚本（源码目录存在则随包分发，解压后即可 sudo PG_HOST=... ./deploy-systemd.sh）
if [ -f deploy-systemd.sh ]; then
    install -m 0755 deploy-systemd.sh "${PKG}/deploy-systemd.sh"
fi

cat > "${PKG}/README.txt" <<EOF
Casdoor 生产发布包 (linux/${PKG_ARCH})
==================================

目录结构（运行时以本目录为工作目录，按相对路径加载 conf/ 与 web/）:
  casdoor                  服务端二进制（静态编译，无 CGO/系统库依赖）
  conf/app.conf            生产配置（runmode=prod，数据库连接需按环境修改）
  web/build/               前端静态资源
  swagger/                 API 文档静态资源（/swagger）
  init_data.json.template  初始数据模板（可选：复制为 init_data.json 启用初始数据导入）
  deploy-systemd.sh        一键部署/升级脚本（如包含）
  logs/                    运行日志目录（conf 中 logConfig 指定）

快速部署（Systemd + 远程 PostgreSQL，推荐）:
  tar xzf casdoor.tar.gz
  cd casdoor
  sudo PG_HOST=<数据库地址> PG_USER=<用户> PG_PASSWORD=<密码> ./deploy-systemd.sh

手动运行:
  1. 编辑 conf/app.conf（driverName / dataSourceName / dbName 等数据库配置）
  2. 启动: ./casdoor

默认监听 http://0.0.0.0:8000，默认管理员 admin / 123（built-in 组织，首次登录后请立即修改）。
升级: 将新的 casdoor.tar.gz 放到部署目录，执行 sudo ./deploy-systemd.sh --upgrade
EOF

tar -C "${STAGE}" --owner=0 --group=0 -czf casdoor.tar.gz casdoor
echo ">> 发布包已生成: casdoor.tar.gz（linux/${PKG_ARCH}，$(du -h casdoor.tar.gz | cut -f1)）"
tar tzf casdoor.tar.gz | head -5
