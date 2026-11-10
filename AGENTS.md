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
- 全新库首次启动时 `built-in/admin` 无密码，需在 `/init-admin` 欢迎页设置。built-in 组织出厂即带 `PasswordOptions = ["AtLeast6"]`（`object/init.go`），欢迎页与 `SetInitAdminPassword()` 双重校验都会拒绝 `123`；`admin / 123` 只在 demo 模式或启动前预置 `initAdminPassword` 时成立（`getInitAdminPassword()`）

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
- **回归测试**：`object/parent_group_auth_test.go`（三个用例各自创建一次性组织，无需真实数据；早期依赖组织 `test` 真实数据的 `-run "RealData"` 用例已不在仓库中）：

  ```bash
  CASDOOR_TEST_CONFIG=/msun/src/casdoor/conf/app-pg.conf go test ./object/ -run "TestParentGroupAuthWithOUHierarchy|TestNestedGroupAuth|TestMixedHierarchy" -v
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

### 5. 日志页按用户名搜索恒为空（PG 保留字 `user` 未加引号）— 2026-11-04 修复

- **症状**：日志页（`/records`）「用户」列过滤输入用户名（如 `yaozhen2`）恒为「暂无数据」（0/0），其余列搜索正常；令牌、会话等一切经 `GetSession` 按 `user` 列搜索的列表页同样受影响。
- **根因**：`object/ormer_session.go` 的 `GetSession()`/`GetSessionForUser()` 用 `session.And(fmt.Sprintf("%s like ?", ...))` 拼**裸 SQL** 条件；`user` 是 PostgreSQL 保留字，不加引号时被解析为会话用户（`CURRENT_USER`），条件恒假、静默零行。同文件的 `quoteColumn()` 即为此起草且全仓 15+ 处已在用，唯独这两个通用 session 构造器漏掉；xorm 的 `Asc/Desc` 内部会经 Quoter 加引号，故排序不受影响，仅搜索条件中招。
- **修复**：两个函数的 like 条件列名统一过 `quoteColumn()`（带 `a.` 别名前缀的点号形式也安全：xorm Quoter 对点号分段逐段加引号，得 `a."user"`）。
- **回归测试**：`object/record_user_search_test.go`（插入临时 record 后断言 `field=user` 的 count 与分页两路均命中、不存在用户名计 0；未修复代码下 count=0 立即失败）：

  ```bash
  CASDOOR_TEST_CONFIG=/msun/src/casdoor/conf/app-pg.conf go test ./object/ -run "TestRecordSearchByUser" -v
  ```
- **约定（防同类 BUG）**：凡往 `session.And/Where/In` 等**裸 SQL 条件**里拼列名，必须过 `quoteColumn()`；PG 保留字（`user`、`group`、`order` 等）裸拼不报错而是静默错义。排序列交给 xorm `Asc/Desc` 即可（内部已加引号）。

### 6. 删除权限只带 owner/name 时残留 Casbin 策略 — 2026-11-05 修复

- **症状**：`POST /api/delete-permission` 只提交 `{"owner","name"}`（SDK/脚本常见写法）时，权限行被删除，但 `permission_rule` 表里该权限的 `p` 策略仍在（`v5` = 权限 ID）。前端删除不受影响，因为 UI 提交整条记录。
- **根因**：`object/permission.go` `DeletePermission()` 把控制器反序列化出的请求体对象直接交给 `removePolicies()`，而 `getPolicies()`（`object/permission_enforcer.go`）由对象的 `Users/Groups/Roles × Resources × Actions`（含 `Domains`/`Effect`）推导待删策略；字段缺失即推导出空列表，`enforcer.RemovePolicies([])` 一行也不删。策略按 `Model`+`Adapter` 共享 enforcer，残留行会继续参与同 model/adapter 下其他权限的判定。
- **修复**：`DeletePermission()` 先用 `getPermission(owner, name)` 回读库中完整对象（含 `Model`/`Adapter`），以它执行删行与 `removePolicies()`，与 `UpdatePermission()` 使用 `oldPermission` 的写法一致；请求体只需给出 owner/name。
- **回归测试**：`object/permission_enforcer_test.go` `TestDeletePermissionWithPartialObjectRemovesPolicies`（自建一次性组织：`AddPermission` 后断言 `permission_rule` 有行，用部分对象 `DeletePermission` 后断言归零；回退修复即失败）：

  ```bash
  CASDOOR_TEST_CONFIG=/msun/src/casdoor/conf/app-pg.conf go test ./object/ -run "TestDeletePermissionWithPartialObjectRemovesPolicies" -v
  ```
- **验证**：HTTP 层实测（组织/应用/群组/权限齐备后 `POST /api/delete-permission` 仅带 owner+name）：删除前 `permission_rule` 1 行 → 删除后 0 行。
- **约定（防同类 BUG）**：凡是「删主对象 + 按对象字段派生清理」的函数（Casbin p/g 行、enforcer 规则、关联策略等），禁止信任请求体对象的完整性，必须先按 ID 回读库中对象再派生清理。

### 7. 钉钉扫码登录「绑定后」恒报账户不存在且不允许注册 — 2026-11-05 修复

- **症状**：用户经钉钉同步器同步或在个人页绑定后，扫码登录恒报「提供商账户: DingTalk 与用户名: \<unionid\> (姓名) 不存在且 不允许注册新账户, 请联系IT支持」；密码登录正常；用户页「第三方登录」显示已绑定。
- **根因**（两条叠加，任一单独即产生该症状）：
  1. **绑定列 ID 语义错位**：`object/syncer_dingtalk.go` `dingtalkUserToOriginalUser()` 把钉钉企业 **userid** 写入用户 `dingtalk` 列（`object/syncer_user.go` 将 `dingtalk` 列加入同步列，注释即 "enable login binding"）；而扫码 OAuth（`idp/dingtalk.go`）的列查找用 **OpenId**（`userInfo.Id = OpenId`）。两者永不相等 → 列查找恒 miss；其后绑定规则兜底也同时落空（Phone 规则要求 `country_code` 精确匹配，同步用户 country_code 为空；Email 规则在企业邮箱为空时失效）→ 落入「不存在且不允许注册」分支。
  2. **共享应用组织范围缺失**：`getUserByProvider()`/`getExistUserByBindingRule()` 只搜 `application.Organization`（共享应用恒属 built-in，无真实用户），而密码登录有 `GetUserByFieldsForSharedApp()` 的跨组织回退；绑定列写在用户真实组织，共享应用扫码登录永远找不到。
- **修复**：
  - `idp/provider.go` `UserInfo` 新增 `CorpUserId`；`idp/dingtalk.go` `GetUserInfo()` 将内部已解析的企业 userid 存入；`controllers/auth.go` 新增 `providerLoginIds()`：钉钉绑定列查找接受 OpenId + 企业 userid 双候选（UI 绑定的 OpenId 仍走主候选；link 流程重复检查同用双候选，防止绑到已被同步器绑定的账户）。
  - `controllers/auth.go` 新增 `providerLookupOrganizations()`：共享应用把 provider 查找（绑定列/绑定规则/link 重复检查）扩展到「应用自身组织 + 所有以它为 default_application 的组织」，与密码登录对齐；命中后 `SetUserOAuthProperties`/`checkMfaEnable` 改用**用户所属组织**的设置（防止共享应用绕过用户组织的 MFA 策略）。
  - `object/user_util.go` 抽出 `GetSharedApplicationOrganizations()`，`GetUserByFieldsForSharedApp()` 复用。
- **回归测试**：
  - `controllers/auth_dingtalk_binding_test.go` `TestDingTalkSyncerBindingColumnLogin`（同步 userid 列与 UI 绑定 OpenId 列均命中、未知账户不命中；移除 CorpUserId 候选立即失败）。
  - `controllers/auth_shared_app_test.go` `TestSharedAppProviderLoginFindsLinkedUser` / `TestSharedAppBindingRuleFindsUser`（共享应用跨组织命中；非共享应用、非 default 组织不越界；移除共享扩展立即失败）。

  ```bash
  CASDOOR_TEST_CONFIG=/msun/src/casdoor/conf/app-pg.conf go test ./controllers/ -run "TestDingTalkSyncerBindingColumnLogin|TestSharedApp" -v
  ```
- **约定（防同类 BUG）**：API 型同步器（DingTalk/WeCom/Lark）写入绑定列的 ID 种类必须与对应 idp 的 `userInfo.Id` 语义对齐；无法对齐时（如钉钉同步器拿不到 per-app OpenId），登录查找必须接受同步器写入的 ID 作为候选。provider 登录的任何用户查找都必须与密码登录共享同一组织范围（含共享应用扩展）。

### 7b. 统一门户（组织默认应用）登录因用户注册应用绑定而中断 MFA 设置 — 2026-11-10 修复

- **症状**：组织默认应用（统一门户）配置钉钉等提供商登录、组织 MFA 规则 Required 时，`signupApplication` 绑定到其他应用（经其他应用注册/同步/历史数据）的用户扫码后登录流程中断：MFA 设置向导（`/mfa/setup`）按**用户注册应用**而非组织默认应用解析界面与提供商——注册应用已被删除或属组织所有时 `get-application?id=admin/<signupApplication>` 返回 `data:null`，向导第 2 步直接报错卡死，登录永远完不成；注册应用存在但未配 Email/SMS 提供商时，验证码发送/RADIUS/Push 提供商、国家码全部按错误应用解析，无法统一使用门户的提供商。
- **根因**：后端 `/api/login` 全程按**登录应用**（state 携带）驱动且 §7 已保证用户查找与 MFA 策略按用户组织执行，唯独前端 `web/src/pages/auth/MfaSetupPage.tsx` 用 `account.signupApplication`（用户注册应用）优先解析向导所需应用（旧 antd 前端 `MfaSetupPage.js` 同源逻辑，`localStorage.applicationName` 回退在新前端无任何 setItem，恒空）。`RequiredMfa` 回弹链路（AuthCallback/SamlCallback/LoginPage → 控制台根 → `useRequiredMfaRedirect` → `/mfa/setup`）不携带登录应用名，向导无从得知门户应用。
- **修复**：向导改为固定用 `OrganizationBackend.getDefaultApplication("admin", account.owner)` 解析**组织默认应用**（统一门户语义，与 `/login/<org>` 登录页同一解析链），失败再回退 `getUserApplication`（LDAP 同步用户、组织无默认应用兜底）；不再读 `account.signupApplication`。改完须重编前端（`cd web && yarn run build && touch web/build/index.html`）。
- **验证**：mock Custom OAuth 提供商（等价钉钉链路）+ 组织 `torg`（defaultApplication=`app-portal`、MfaItems=[{app,Required}]）+ 用户 `torg/dtuser2`（signupApplication 指向 `app-orgapp`/`app-deleted`）：修复前服务端日志实测向导请求 `GET /api/get-application?id=admin/app-orgapp`（用户注册应用，注册应用缺失时 data:null → 卡死）；修复后向导请求 `get-default-application?id=admin/torg` → `app-portal`，TOTP initiate/verify/enable 全链路通过，再次扫码登录 → NextMfa → 输入 TOTP → `torg/dtuser2` 登录成功。测试数据保留在开发库（`torg` 组织、`app-portal`/`app-other`/`app-orgapp`、`mock-oauth` 提供商指向 127.0.0.1:9100 的本地 mock，mock 服务脚本已删）。
- **约定（防同类 BUG）**：登录延续流程（MFA 设置/验证、验证码发送、prompt 页等）中需要「应用」的地方，**禁止**用 `account.signupApplication`（用户注册应用）解析，必须用登录应用（state/URL 携带）或组织默认应用（`get-default-application`）；`signupApplication` 只属于注册与管理场景。
- **演变链**：旧 antd `MfaSetupPage.js`（signupApplication 优先 + entry 应用回退）→ shadcn 重写（回退项失活，仅剩 signupApplication）→ 本次修复（固定组织默认应用）。

### 8. 其余本地修复清单（2026-10-29 已核对在位，合并后需复查）

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
| 日志/令牌/会话等列表按 `user` 等 PG 保留字列搜索恒空（裸 SQL 条件列名过 `quoteColumn`，见上文 §5） | 2026-11-04 本次修复 | `object/ormer_session.go` `GetSession`/`GetSessionForUser` | 在位 |
| 删除权限先回读库中完整对象再清理 Casbin 策略（请求体只带 owner/name 也不再残留 `permission_rule` 行，见上文 §6） | 2026-11-05 本次修复 | `object/permission.go` `DeletePermission` | 在位 |
| 钉钉扫码登录绑定后恒报账户不存在（同步器写 userid 到绑定列 vs OAuth 按 OpenId 查；provider 登录缺共享应用组织范围，见上文 §7） | 2026-11-05 本次修复 | `idp/dingtalk.go` + `idp/provider.go` `UserInfo.CorpUserId` + `controllers/auth.go` `providerLoginIds`/`providerLookupOrganizations` + `object/user_util.go` `GetSharedApplicationOrganizations` | 在位 |
| 统一门户 MFA 设置向导按用户注册应用解析界面/提供商导致卡死或提供商错位（改用组织默认应用，见上文 §7b） | 2026-11-10 本次修复 | `web/src/pages/auth/MfaSetupPage.tsx` | 在位 |
