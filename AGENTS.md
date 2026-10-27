# Casdoor Agent Guide

## 项目说明

Casdoor 是一个基于 Go + React 的身份认证与授权平台。后端使用 Beego 框架，前端位于 `web/`（shadcn/ui，主前端）和 `web-old/`（旧版 Ant Design）。

## 启动方式

### 调试运行（推荐）

以当前用户 `sfx` 直接运行，使用 `conf/app-pg.conf` 配置，适合开发调试：

```bash
RUN_USER=sfx ./scripts/start_pg.sh
```

或手动编译后直接启动（完整流程）：

```bash
# 1. 编译后端
rm -f bin/casdoor-server
CGO_ENABLED=0 go build -o bin/casdoor-server .

# 2. 编译前端（仅在源文件有变更时执行，否则跳过）
# 后端仅服务 web/build（shadcn/ui 主前端），web-old/ 为旧版前端，一般无需编译。
# 检查 web/ 是否需要重编：
#   find web/src -newer web/build/index.html -print -quit
# 若有输出则执行：
#   cd web && yarn install && yarn run build && touch web/build/index.html && cd ..

# 3. 启动服务
mkdir -p logs tmp
./bin/casdoor-server -config conf/app-pg.conf -createDatabase=true
```

**要点：**
- 配置文件固定使用 `conf/app-pg.conf`（PostgreSQL，`runmode = dev`）
- 不使用 `app.conf`，不修改 `app.conf`
- `RUN_USER=sfx` 表示以当前用户运行，无需 sudo 创建专用用户
- 日志输出到控制台（`logConfig = {"adapter":"console"}`）

### 环境变量（可选覆盖）

| 变量 | 默认值 | 说明 |
|---|---|---|
| `PORT` | `8000` | HTTP 监听端口 |
| `PG_USER` | `postgres` | 数据库用户 |
| `PG_PASSWORD` | `123456` | 数据库密码 |
| `PG_HOST` | `127.0.0.1` | 数据库地址 |
| `PG_PORT` | `5432` | 数据库端口 |
| `DB_NAME` | `casdoor` | 数据库名 |
| `RUN_USER` | `sfx` | 服务运行用户 |
| `SKIP_FRONTEND` | `0` | 非 0 跳过前端编译 |

### 前端开发

```bash
cd web && yarn install && yarn start      # shadcn/ui 主前端（端口 3000）
cd web-old && yarn install && yarn start  # 旧版 Ant Design 前端
```

## 配置文件

| 文件 | 用途 |
|---|---|
| `conf/app-pg.conf` | **主配置**，PostgreSQL，开发模式，LDAP 1389，日志输出控制台 |
| `conf/app-sqlite.conf` | SQLite 轻量配置（备用） |
| `conf/app.conf` | 不使用，勿修改 |

## 数据库

PostgreSQL，默认连接：`postgres@127.0.0.1:5432/casdoor`，SSL (`sslmode=require`)。

重置数据库：

```bash
./scripts/start_pg.sh fresh
```

## 代码结构

```
controllers/   # Beego 控制器（HTTP handlers）
object/        # 业务逻辑层
routers/       # 路由注册
conf/          # 配置文件
web/           # 主前端（React + shadcn/ui，后端服务此目录的 build）
web-old/       # 旧版前端（React + Ant Design）
scripts/       # 启停脚本
```

## 注意事项

- 后端修改无需重启前端，直接重新编译运行 `bin/casdoor-server` 即可。
- `conf/app-pg.conf` 属主可能因上次以 casdoor 用户运行而变为 root/casdoor，若写入失败请执行：
  ```bash
  sudo chown sfx conf/app-pg.conf
  ```
- LDAP 端口默认 `1389`（非特权），无需 `setcap`。
