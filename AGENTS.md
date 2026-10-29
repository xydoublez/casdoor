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
cd web && yarn install && yarn start      # shadcn/ui 主前端（vite，端口 7001）
cd web-old && yarn install && yarn start  # 旧版 Ant Design 前端（craco，端口 7001）
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

## BUG 修复历史（防止上游合并回退）

> 组层级（`Group.ParentId`）不会物化到 `user.Groups` 或 Casbin g 行，所有按组匹配的代码路径都必须显式展开祖先/后代组。本地修复与上游同文件冲突时，合并可能使其丢失或语义不完整。**每次合并 `upstream/master` 后，按下表逐项核对修复是否仍在位，并运行对应回归测试。**

### 1. 父组权限覆盖子组成员（组层级授权）— 2026-10-29 回退修复

- **症状**：权限/角色绑定在父组（如 `AI中台部`）时，只属于其子组（如 `AI医疗专家组`）的用户（如 `test/liuyangyang`）拿不到授权；`getPermissionsAndRolesByUser()` 不返回父组权限，导致用户页权限列表与 `GetAllObjects/GetAllActions`（前端菜单/资源可见性）全部缺失。
- **根因**：上游合并 `0d1df1f8`（2026-10-27）重写了 `object/permission.go` 的 `getPermissionsAndRolesByUser()`（引入 SourceGroups/SourceRoles 跟踪），重写后的按组收集循环只遍历 `user.Groups`（直接所属组），未继承本地提交 `d89cd51f`（2026-10-27，"enforce group hierarchy in permission checks"）建立的父组覆盖语义——该提交修复的 `isGroupHit`、`getRolesByUserInternal`、运行时分组策略三处在合并中幸存，但此函数被上游版本取代后症状重新暴露。
- **修复**：在 `getPermissionsAndRolesByUser()` 收集组绑定权限前，先用 `GetGroupIdsWithAncestors()`（`object/group.go`，`d89cd51f` 引入，带 per-owner 缓存 + 写失效 + 10s TTL）展开祖先组。
- **回归测试**：`object/parent_group_auth_test.go`（真实数据：组织 `test`，父组 `AI中台部` → 子组 `AI医疗专家组`，用户 `liuyangyang`/`chenyipeng`）：

  ```bash
  CASDOOR_TEST_CONFIG=/msun/src/casdoor/conf/app-pg.conf go test ./object/ -run "RealData" -v
  ```

- **演变链**：`13cd8e59`（组层级初版）→ `d89cd51f`（完整修复）→ 合并 `0d1df1f8`（语义被上游重写覆盖）→ 本次回退修复。

### 2. 其余本地修复清单（2026-10-29 已核对在位，合并后需复查）

| 修复 | 引入提交 | 位置 | 状态 |
|---|---|---|---|
| LDAP 组同步层级硬化：OU 先于安全组排序、环检测、BaseDn 为组时的根处理、selfParent 保留 OU 层级 | `f13a408d` | `object/ldap_conn.go` `SyncLdapGroups` | 在位 |
| LDAP 组织无应用时自动创建默认应用（避免 AddUser 报 "should have one application at least"） | `f13a408d` | `object/ldap_conn.go` `ensureOrganizationApplication` | 在位（`c4591a5f` 适配 lang 参数） |
| LDAP `title`/`o`/`company` 属性同步 + CustomAttributes 映射（如 department→affiliation） | `13cd8e59` | `object/ldap_conn.go` | 在位 |
| 用户 `SignupApplication` 应用回退查找（应用可能属于用户组织而非 admin） | `13cd8e59` | `object/application.go` `GetApplicationByUser` | 在位 |
| SQLite driverName `sqlite3`→`sqlite` 映射 | `13cd8e59`（上游 `b3d9ca63` 亦内置） | `object/adapter.go` | 在位 |
| Syncer AffiliationTable 的 Ormer 空指针守卫 | `13cd8e59` | `object/syncer_sync.go` | 在位 |
| 前端构建目录解析：`frontendBaseDir` 失效/未设时回退 `web/build` | `95f7bed2` | `routers/static_filter.go` `getWebBuildFolder` | 在位 |
| TagsInput 接受 string/null/undefined 并归一化为 string[] | `95f7bed2` | `web/src/components/common/TagsInput.tsx`（原 web2，上游重命名 web2→web） | 在位 |
| goroutine panic 打印调用堆栈 | `13cd8e59` | `util/routine.go` `SafeGoroutine` | 在位 |
| 上游 web/web2 目录重命名适配（后端仅服务 `web/build`） | `c4591a5f` | `scripts/start_pg.sh` 等 | 在位 |
