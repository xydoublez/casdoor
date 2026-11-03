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

### 2. 面包屑显示 URL 编码组名（BreadcrumbBar 解码）— 2026-10-29 回退修复

- **症状**：群组页面包屑最后一级显示百分号编码原文（如 `%E4%BC%97%E9%98%B3%E5%81%A5%E5%BA%B7%E9%9B%86%E5%9B%A2`），而非解码后的中文组名（众阳健康集团）。
- **根因**：`f13a408d`（2026-08-31）的 `safeDecodeURIComponent` 修复加在旧 antd 前端 `web/src/common/BreadcrumbBar.js`；上游合并 `0d1df1f8` 将 web2→web 重命名后，主前端变为 shadcn 版，其 `web/src/components/layout/BreadcrumbBar.tsx` 虽注明 "ported from web/src/common/BreadcrumbBar.js" 但未移植解码逻辑；旧文件随 antd 前端移入 `web-old/`，修复只留在不再被服务的旧前端里。
- **修复**：`web/src/components/layout/BreadcrumbBar.tsx` 增加 `safeDecodeURIComponent`；`RESOURCE_LABELS` 查找键用原始段、显示标签用解码段；改完必须重编前端（`cd web && yarn run build && touch web/build/index.html`），否则后端仍服务旧 bundle。
- **验证**：`npx tsc --noEmit` 通过；浏览器实测 `/groups/test/%E4%BC%97...` 面包屑显示「众阳健康集团」。
- **演变链**：`f13a408d`（旧前端修复）→ 合并 `0d1df1f8`（前端换壳，修复未移植）→ 本次回退修复（新前端）。

### 3. 用户下拉搜不到第一页之外的用户（服务端搜索 + 滚动翻页）— 2026-10-30 重写回退修复

- **症状**：角色/权限等编辑页的「包含用户」多选搜索 `lizhiqiang` 显示「暂无数据」，但该用户真实存在（用户列表页同名服务端搜索可命中）；组织用户数超过 1000 时，第一页之外的任何用户都搜不到、选不中。
- **根因**：shadcn 重写后 `web/src/hooks/use-options.ts` 的 `useUserOptions`/`useUserNameOptions` 只取 `get-users` 第一页（`PAGE_SIZE=1000`），`MultiSelect`/`SearchableSelect` 用 cmdk `filter` 纯客户端过滤；按 `created_time` 倒序排在第一页之外的用户永远进不了选项列表。旧 antd 前端此处是 `web-old/src/common/PaginateSelect.js` 的服务端搜索（`field=name`）+ `onPopupScroll` 滚动翻页，重写时语义丢失——与 §2 同属「前端换壳后旧前端能力/修复未移植」。
- **修复**：hook 返回 `RemoteOptions {options, onSearch, onLoadMore, hasMore}`：输入防抖 300ms 走后端 `name`/`displayName` 双路 LIKE 搜索合并去重；下拉滚到底追加下一页（基础 1000/页、搜索 100/页）；ticket/seq 作废在途旧响应防竞态；关闭下拉重置回基础页；`MultiSelect`/`SearchableSelect` 新增 `onSearch`/`onLoadMore`/`hasMore`（远程模式跳过本地过滤）并以标签缓存保证已选徽章换页后不丢显示名；`SimpleEditPage` 的 select/multiselect 透传三 prop，接入 Role、Permission、Order、Pricing、Ticket、Transaction、Coupon、Subscription、Payment、Key 共 10 个编辑页；后端 `object/ormer_session.go` `GetSessionForUser` 分页分支在排序列后追加 `owner,name` 确定性 tiebreaker——LDAP 批量同步会给成千用户写同一秒 `created_time`，无 tiebreaker 时并列行在两次分页查询间顺序不保证，滚动翻页可能漏行（万人组织必现级风险）。
- **验证**：`npx tsc --noEmit` 通过并重编前端；开发库注入 1008 用户（`lizhiqiang` 排在 1000 名外）浏览器实测：下拉打开 1000 项 → 滚到底自动增至 1008 项、搜索 `lizhiqiang` 命中 1 项可勾选；测试数据已清理。
- **回归测试**：`object/user_pagination_tie_test.go`（同 `created_time` 120 用户倒序插入：分页无遗漏/无重叠、遍历序等于 tiebreaker 序、同页重复查询不漂移、搜索路径分页完整；移除 tiebreaker 后该测试立即失败）：

  ```bash
  CASDOOR_TEST_CONFIG=/msun/src/casdoor/conf/app-pg.conf go test ./object/ -run "TestPaginationUsersTieBreaker" -v
  ```
- **约定（防同类 BUG）**：新前端任何可能超过一页的长列表下拉（用户、群组、角色等）**禁止**「只加载单页 + 客户端过滤」模式，必须接服务端搜索/分页（`RemoteOptions` 模式）；移植旧 antd 页面时须逐项核对 `PaginateSelect`、`showSearch`、`onPopupScroll` 的服务端搜索语义，防止换壳丢失。
- **演变链**：旧前端 `PaginateSelect`（服务端搜索 + 翻页）→ shadcn 重写（单页 + 客户端过滤，能力丢失）→ 本次修复（恢复服务端搜索 + 滚动翻页）。

### 4. 联合登录（钉钉扫码等）验证 MFA 后弹回登录页 — 2026-11-03 重写回退修复

- **症状**：普通用户钉钉扫码登录，TOTP 验证通过后弹出「登录成功」toast，但页面仍停在登录界面，必须手动刷新（F5）才进入应用。每次必现；密码登录完全正常。
- **根因**：社交/联合登录回跳 `/callback`，`web/src/pages/auth/AuthCallback.tsx` 的 `handleOAuth` 在 `type === "login"`（及 `link`）分支直接 `navigate(Setting.getFromLink())`。SPA 跳转不重载页面，`AccountProvider`（`web/src/hooks/use-account.tsx`）里的 `account` 仍是页面加载时的匿名 `null`；`App.tsx` 的 `RequireAuth` 见 `account === null` 立即 `<Navigate to="/login">` 把人弹回登录页——而 toast 已经弹出，观感即「提示登录成功却还在登录界面」。手动刷新会重新拉 `get-account`，账号就绪后 `LoginPage` 的 effect 再跳 `getFromLink()`，于是能进应用。密码登录不受影响：`LoginPage.handleLoginResult` 写的是 `reload().then(() => navigate(...))`，先刷新账号再跳转。旧 antd 前端 `web-old/src/App.js` 的 `onLoginSuccess()` → `this.getAccount()` 同样有这一步，shadcn 重写时丢失——与 §2/§3 同属「前端换壳后旧前端能力/修复未移植」。
- **影响面**：不止钉钉。全仓仅 6 处调用登录 API，其中 `AuthCallback.tsx`（**所有**第三方扫码/社交登录：钉钉、微信、企业微信、飞书、GitHub、Google、Telegram 等，以及账户页绑定第三方的 `link` 流程）与 `SamlCallback.tsx`（外部 SAML IdP）都缺 `reload()`。触发条件是 `responseType === "login"`：从 Casdoor 自身登录页发起社交登录（state 里没有 `redirect_uri`），或 OAuth 应用的 `redirect_uri` 指回 Casdoor 自身（`isSelfRedirectUri`）。给外部应用发授权码的 code/token 流程走 `goToLink`（整页加载）不受影响——这解释了为何只有部分场景暴露。
- **修复**：两个回调页在 SPA 跳转前先 `reload()` 刷新账号上下文，与 `LoginPage` 对齐；`goToLink`/`sendOAuthResponse`/`RedirectForm` 等整页跳转分支不动。`link` 分支同时修掉绑定成功后账号未刷新、页面看不到新绑定身份的问题。附带修复 `web/src/pages/auth/CasLogout.tsx`：`finish()` 原先 fire-and-forget 调 `reload()` 后就 `navigate()`，退出瞬间 CAS 登录页会闪现「使用当前账号登录」按钮；改为仅在 `navigate` 分支 await `reload()`（两个 `goToLink` 分支整页加载会自己拉账号，那次请求在导航中被丢弃，一并去掉）。
- **验证**：`npx tsc --noEmit` 通过并重编前端。用本地 mock OAuth2 服务（`Custom` 类型 Provider 指向 `127.0.0.1:9100` 的 authorize/token/userinfo，userinfo 按 email 命中已绑 TOTP 的测试用户）驱动与钉钉**完全相同**的链路：`/login/torg` → 点 Provider 按钮 → `/callback?code=...` → 轻量静态页（`routers/lightweight_auth_filter.go` + `web/public/AuthCallbackHandler.js`）判定 `NextMfa` 并移交 React（URL 带 `__casdoor_callback_react=1`）→ MFA 界面 → 输入 TOTP。对照结果：**未修复**构建最终 URL `/login`、页面为登录表单；**修复后**最终 URL `/apps`、控制台侧栏显示已登录用户。服务端日志同步印证：未修复时 `API: [torg/dingtest] signed in` 之后**没有** `get-account` 请求；修复后紧跟一条 `GET /api/get-account`（即新增的 `reload()`）。mock 服务与 mock Provider 已删除，测试用户 `torg/dingtest`（已绑 TOTP）保留。
- **约定（防同类 BUG）**：登录/登出/绑定等**改变会话状态**后要进入受 `RequireAuth` 保护的路由，禁止裸 `navigate()`，必须 `reload().then(() => navigate(...))`（或先 `setAccount()`），否则账号上下文仍是旧值、会被立刻弹回登录页。整页跳转（`Setting.goToLink`、`window.location.*`、表单 POST）不受此约束，因为它们本身会重新拉账号。移植旧 antd 页面时须逐项核对 `onLoginSuccess`/`getAccount` 的账号刷新语义。
- **演变链**：旧前端 `App.onLoginSuccess()` → `getAccount()`（刷新账号后再进应用）→ shadcn 重写（`AuthCallback`/`SamlCallback` 直接 `navigate`，刷新语义丢失）→ 本次修复（跳转前 `reload()`）。

### 5. 其余本地修复清单（2026-10-29 已核对在位，合并后需复查）

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
| 面包屑 safeDecodeURIComponent 解码（CJK 组名不显示百分号编码） | `f13a408d`（2026-10-29 重新应用到新前端，见上文 §2） | `web/src/components/layout/BreadcrumbBar.tsx` + `web-old/src/common/BreadcrumbBar.js` | 在位 |
| goroutine panic 打印调用堆栈 | `13cd8e59` | `util/routine.go` `SafeGoroutine` | 在位 |
| 上游 web/web2 目录重命名适配（后端仅服务 `web/build`） | `c4591a5f` | `scripts/start_pg.sh` 等 | 在位 |
| 用户下拉服务端搜索 + 滚动翻页（`RemoteOptions`，恢复 antd PaginateSelect 语义，见上文 §3） | 2026-10-30 本次修复 | `web/src/hooks/use-options.ts` + `web/src/components/common/MultiSelect.tsx`/`SearchableSelect.tsx` + `web/src/components/crud/SimpleEditPage.tsx` | 在位 |
| 联合登录/社交扫码（钉钉等）+ MFA 后跳转前刷新账号（恢复 antd `onLoginSuccess`→`getAccount` 语义，见上文 §4） | 2026-11-03 本次修复 | `web/src/pages/auth/AuthCallback.tsx` + `web/src/pages/auth/SamlCallback.tsx` | 在位 |
| CAS 退出后先清空账号上下文再跳 CAS 登录页（避免闪现「使用当前账号登录」） | 2026-11-03 本次修复 | `web/src/pages/auth/CasLogout.tsx` `finish()` | 在位 |
