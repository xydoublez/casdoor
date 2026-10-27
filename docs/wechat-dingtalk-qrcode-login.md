# Casdoor 钉钉 / 微信 扫码登录实现原理与流程

> 版本：v1.0 ｜ 适用代码：本仓库（Go + Beego 后端，`web/` Ant Design 前端）
> 目标：说明 Casdoor 如何对接钉钉、微信实现"扫码登录"，覆盖授权 URL 构造、回调解析、令牌换取、用户身份归一、以及三种微信扫码形态的差异。

## 1. 总览：一套 Provider 抽象，两种"扫码"实现

Casdoor 把第三方登录统一抽象为 **OAuth 类 Provider**。所谓"扫码"，在实现上分为两类截然不同的机制：

| 类别 | 二维码由谁生成/托管 | 代表 Provider | 二维码内容 |
|---|---|---|---|
| **托管页扫码**（重定向型） | 第三方站点页面 | 钉钉、微信公众平台扫码（qrconnect）、企业微信 | 第三方页面自己渲染，用户用手机 App 扫屏上码并确认，**浏览器整页跳转**到第三方 → 带 `code` 回跳 |
| **自建码轮询**（反向通道型） | **Casdoor 自己**渲染二维码图片 | 微信公众号带参二维码（`signName=media`） | Casdoor 后端调微信接口拿 `ticket`+`url`，前端渲染 `url`；扫码事件由**微信服务器推给 Casdoor** `/api/webhook`，前端轮询 `ticket` 状态后再"伪造"一次本地回调 |

两者最终都汇聚到同一个入口：前端 `/callback` 路由 → `POST /api/login`（携带 `provider` + `code`）。

```
                    ┌──────────────── 托管页扫码（钉钉 / 微信 qrconnect）────────────────┐
 浏览器 ──点按钮──► 第三方授权页(展示二维码) ──手机App扫码确认──► 302 /callback?code&state
                    └───────────────────────────────────────────────────────────────┘
                    ┌──────────────── 自建码轮询（微信公众号 media）──────────────────┐
 前端 ──GET /api/get-qrcode──► Casdoor ──► 微信 cgi-bin/qrcode/create ──► (url, ticket)
 前端 ◄── 渲染二维码(url) ────┘
 手机微信扫码 ──► 微信服务器 ──POST /api/webhook(SCAN 事件)──► Casdoor 内存表[ticket]=unionId
 前端 每秒轮询 GET /api/get-webhook-event?ticket ──► "SCAN"
 前端 ──本地跳转 /callback?state&code=wechat_oa:{ticket}──┐
                                                        ▼
                                     POST /api/login {provider, code, state, ...}
                                                        ▼
                            idp.GetToken(code) ──► idp.GetUserInfo(token) ──► UserInfo
                                                        ▼
                            按 UserInfo.Id 匹配 user.dingtalk / user.wechat 列
                                                        ▼
                                  HandleLoggedIn → 下发 OAuth code / token / 会话
```

---

## 2. 公共骨架（钉钉与微信共用）

### 2.1 后端 Provider 接口与工厂

- 接口定义：`idp/provider.go:59`

  ```go
  type IdProvider interface {
      SetHttpClient(client *http.Client)
      GetToken(code string) (*oauth2.Token, error)
      GetUserInfo(token *oauth2.Token) (*UserInfo, error)
  }
  ```

- 统一身份结构 `idp.UserInfo`（`idp/provider.go:25`）：`Id / Username / DisplayName / UnionId / Email / Phone / CountryCode / AvatarUrl / Extra`。**`Id` 是账号归一的唯一键**。
- 工厂分发：`idp.GetIdProvider()`（`idp/provider.go:65`）
  - `"DingTalk"` → `NewDingTalkIdProvider(clientId, clientSecret, redirectUrl)`（`:82`）
  - `"WeChat"` → `SubType=="Mobile"` 时 `NewWeChatMobileIdProvider`，否则默认 PC 扫码的 `NewWeChatIdProvider`（`:73-79`）
- Provider 数据库记录 → `idp.ProviderInfo` 转换：`object.FromProviderToIdpInfo()`（`object/provider.go:615`）。此处有一条 WeChat 专属逻辑：若请求 UA 含 `MicroMessenger`，把 `ClientId/ClientSecret` 替换为 `ClientId2/ClientSecret2`（公众号凭证），见 `object/provider.go:632-636`。
- HTTP 客户端选择：`controllers/auth.go:426 setHttpClient()`，`provider.EnableProxy` 为真时用 `proxy.ProxyHttpClient`，否则 `proxy.DefaultHttpClient`。

### 2.2 前端授权 URL 构造

`web/src/auth/Provider.js:432 getAuthUrl(application, provider, method, code)`

- 端点/作用域集中配置在 `authInfo` 表（`Provider.js:59`）：
  - `DingTalk`: `https://login.dingtalk.com/oauth2/auth`，scope `openid`
  - `WeChat`: `https://open.weixin.qq.com/connect/qrconnect`（PC 扫码），scope `snsapi_login`；
    以及 `mpEndpoint` `https://open.weixin.qq.com/connect/oauth2/authorize`（内置浏览器），`mpScope` `snsapi_userinfo`
- `redirectUri` 固定为 `{origin}/callback`（`Provider.js:439`），`application.forcedRedirectOrigin` 可覆盖。
- `scope` 可被 `provider.scopes` 字段覆盖（`Provider.js:442`）。

### 2.3 state：把上下文穿过第三方带回来

`web/src/auth/Util.js:180 getStateFromQueryParams(applicationName, providerName, method, isShortState)`

```
query = 当前页 location.search + &application=X&provider=Y&method=signin[&userCode=...]
state = isShortState ? providerName(真值存 sessionStorage) : btoa(query)
```

- `isShortState` 仅在以下场景启用（`Provider.js:446`）：微信内置浏览器且配了 `clientId2`、Twitter、Telegram。原因是这类通道对 `state` 长度敏感。
- 解回：`Util.getQueryParamsFromState(state)`（`Util.js:203`）——先查 `sessionStorage`，否则 `atob(state)`。
- **已知缺陷**：钉钉分支（`Provider.js:491`）与微信 `qrconnect` 分支（`Provider.js:507`）拼接 URL 时，`redirect_uri` 与 `state` **未做 `encodeURIComponent`**。base64 的 `state` 含 `+ / =`，`+` 经第三方回跳后会被解析成空格，导致 `atob` 失败 → `application` 解析为空 → 后端报"应用不存在"。修复方式：对 `state`（建议连同 `redirect_uri`）做 `encodeURIComponent` 后重编前端。

### 2.4 前端回调页 → 后端 Login

`web/src/auth/AuthCallback.js:211`（路由 `/callback`）

1. 取授权码，做多 Provider 兼容（`AuthCallback.js:215-233`）：`code` → `auth_code`（企业微信）→ **`authCode`（钉钉新版）** → `web3AuthTokenKey` → Steam 全参数。
2. 解 state 得 `application / provider / method`，`getResponseType()`（`AuthCallback.js:164`）判定回给 Casdoor 的是什么：
   - 无 `redirect_uri` → `login`（Casdoor 自身登录页）
   - 有外部 `redirect_uri` → 沿用 authorize 的 `response_type`（一般为 `code`）
   - 带 `userCode` → `device`；带 `SAMLRequest` → `saml`；带 `service` → `cas`
3. `AuthBackend.login(body, oAuthParams)` → `POST /api/login`。

### 2.5 后端 Login 的 Provider 分支

`controllers/auth.go:845`（`authForm.Provider != ""`）

```
GetApplication("admin/{application}")  或  GetApplicationByClientId(clientId)   // :847-864
GetOrganization(...)                                                            // :867
GetProvider("admin/{provider}")                                                 // :873
providerItem = application.GetProviderItem(provider.Name)
  └─ IsProviderVisible() 为假 → 拒绝                                            // :884
[OAuth/Web3] FromProviderToIdpInfo → GetIdProvider → setHttpClient              // :899-916
state 校验：authState 或 application 名匹配                                       // :918-922
token    = idProvider.GetToken(authForm.Code)                                   // :925
userInfo = idProvider.GetUserInfo(token)                                        // :936
provider.EmailRegex 校验（可选）                                                 // :942
```

### 2.6 身份归一：`UserInfo.Id` ↔ user 表列

`controllers/auth.go:542 getUserByProvider()`：

```go
object.GetUserByField(organization, provider.Type, userInfo.Id)
// field 会被 strings.ToLower()（object/user_util.go:45）
// → provider.Type="DingTalk" → SQL 列 dingtalk
// → provider.Type="WeChat"   → SQL 列 wechat
```

对应字段声明在 `object/user.go:126/128`：

```go
WeChat   string `xorm:"wechat varchar(100)" json:"wechat"`
DingTalk string `xorm:"dingtalk varchar(100)" json:"dingtalk"`
```

命中/未命中的三种走向（`controllers/auth.go:954-1185`）：

| 情况 | 行为 |
|---|---|
| 查到未删除用户 | `SetUserOAuthProperties` 同步资料 → `HandleLoggedIn` 直接登录 |
| 未查到，`method=signup/signin` | 先按 **BindingRule**（默认 `["Email","Phone","Name"]`）在组织内找已有账号（`auth.go:493 getExistUserByBindingRule`）；仍无则校验 `application.EnableSignUp` + `providerItem.CanSignUp` + 邀请码 → 新建 `User` → `SetUserOAuthProperties` → `linkUserByProvider(user, provider, userInfo.Id)` 写入 `user.dingtalk / user.wechat` |
| `method=link`（账号设置页主动绑定） | 必须已有会话；若该第三方 ID 已绑他人则拒绝；否则写绑定字段 |

`object.SetUserOAuthProperties`（`object/user_util.go:262`）会把第三方资料落到 `user.properties`：
`oauth_{Type}_id`、`oauth_{Type}_username`、`oauth_{Type}_displayName`、`oauth_{Type}_email`、`oauth_{Type}_phone`、`oauth_{Type}_avatar`、`oauth_{Type}_extra`（`Extra` 的 JSON 合并），以及 `{Type} accessToken / refreshToken` 与兼容字段 `user.OriginalToken`。若 Provider 配了 `UserMapping`，还会按映射写用户属性（`user_util.go:332`）。

### 2.7 登录后处理

`controllers/auth.go:60 HandleLoggedIn()` 依次执行：`IsForbidden` / `IsDeleted` 检查 → `CheckEntryIp` 入口 IP 白/黑名单 → 应用与组织的 `DisableSignin` → `CheckLoginPermission`（Casbin RBAC）→ 普通用户的 `application.Tags` 标签限制 → `paid-user` 订阅校验 → 独占登录(`EnableExclusiveSignin`)踢下线 → 按 `form.Type` 产出 `code` / `token` / `id_token` / SAML / CAS ST → 设置会话过期（`CookieExpireInHours`，未勾选自动登录则最多 24h）→ `RecordUserSignin` 审计 + 写 `Session`。

---

## 3. 钉钉扫码登录详解

### 3.1 特点

**Casdoor 不生成二维码。** 钉钉的二维码由 `login.dingtalk.com/oauth2/auth` 托管页渲染，用户在页面上选择"扫码登录"标签页，用手机钉钉 App 扫屏确认。对 Casdoor 而言，这是一个标准的 **OAuth2 授权码模式（跳转式）**。

### 3.2 时序

```
① 前端 Provider.js:491
   https://login.dingtalk.com/oauth2/auth
     ?client_id={provider.clientId}
     &redirect_uri={origin}/callback          ← 未 encode（见 2.3 缺陷）
     &scope=openid
     &response_type=code
     &prompt=login%20consent
     &state={btoa(...&application&provider&method)}
② 用户在钉钉托管页扫码 → 手机确认 → 钉钉 302 回跳
   {origin}/callback?authCode=xxxx&state=xxxx      ← 注意参数名是 authCode
③ AuthCallback.js:222 识别 authCode → POST /api/login
④ controllers/auth.go:899 → idp.NewDingTalkIdProvider
⑤ GetToken(code)   → 钉钉用户 accessToken
⑥ GetUserInfo(...) → openId / unionId / nick / mobile / email
⑦ 匹配 user.dingtalk == openId → 登录或注册/绑定
⑧ HandleLoggedIn → 回到业务应用 redirect_uri?code=...
```

### 3.3 后端实现要点（`idp/dingtalk.go`）

| 步骤 | 接口 | 代码位置 | 说明 |
|---|---|---|---|
| OAuth 配置 | — | `:51 getConfig` | `AuthURL` 被复用为**用户信息端点** `https://api.dingtalk.com/v1.0/contact/users/me`；`TokenURL` = `https://api.dingtalk.com/v1.0/oauth2/userAccessToken`。`Scopes` 为占位空值（钉钉不允许设置 scope） |
| 换用户令牌 | `POST /v1.0/oauth2/userAccessToken` | `:80 GetToken` | JSON body：`{"clientId","clientSecret","code","grantType":"authorization_code"}`。**字段名必须是 `clientId`**，写成 `clientKey` 会被钉钉以 `MissingclientId` 拒绝 |
| 取用户资料 | `GET /v1.0/contact/users/me` | `:135 GetUserInfo` | 令牌放在**请求头** `x-acs-dingtalk-access-token`（不是 query）。返回 `nick / avatarUrl / mobile / openId / unionId / email / stateCode`；`openId` 或 `unionId` 为空即报错 |
| 取企业凭证 | `POST /v1.0/oauth2/accessToken` | `:243 getInnerAppAccessToken` | body `{"appKey","appSecret"}` → 企业内部应用 access_token |
| unionId→userId | `POST /topapi/user/getbyunionid` | `:274 getUserId` | `errcode==60121` → "该应用只允许本企业内部用户登录" |
| 取通讯录详情 | `POST /topapi/v2/user/get` | `:308 getUserCorpInfo` | 用企业内资料**覆盖**手机号/邮箱；`title`（职位）写入 `UserInfo.Extra["title"]`；若拿到 `unionid` 则把 `Username` 改成 unionId |

最终返回：`UserInfo.Id = openId`（落到 `user.dingtalk`），`UnionId = unionId`，`Username` 可能是昵称或企业 unionId。

### 3.4 排错速查

| 现象 | 根因 |
|---|---|
| 回调页报"应用不存在"（`%s` 为空） | `state` 未 URL 编码被破坏（见 2.3）。验证：F12 看 `/api/login` 请求体 `application` 是否为空串 |
| 钉钉返回 `MissingclientId` | 请求体字段名误写为 `clientKey` |
| 钉钉返回 `invalidParameter.idOrSecret.notFound` | clientId/clientSecret 值不匹配；且钉钉侧改凭证**必须在"版本管理与发布"发布新版本**后才生效 |
| `code been used` / `invalid code` | 授权码一次性，重复回调或刷新 `/callback` 页会失败 |
| 60121 只能企业内部用户 | 应用可见范围限制，需在钉钉后台扩大范围 |
| Casdoor 侧改 Provider 凭证不生效 | Provider 是每次登录实时读库，**无需重启**；若无效请检查是否落在正确 owner/name 的 Provider 上 |

---

## 4. 微信扫码登录详解（三种形态）

微信的配置复杂度显著高于钉钉，因为同一个 `WeChat` 类型 Provider 承载了三条通道，靠 `clientId/clientId2` + `disableSsl` + `signName` + User-Agent 组合切换。

### 4.1 形态 A：开放平台 PC 网站应用扫码（qrconnect，托管页）

**触发条件**：非微信内置浏览器，且 `signName !== "media"`（或 `disableSsl` 为假）。

```
Provider.js:507
https://open.weixin.qq.com/connect/qrconnect
  ?appid={provider.clientId}            ← 开放平台"网站应用" AppID
  &redirect_uri={origin}/callback
  &scope=snsapi_login
  &response_type=code
  &state={btoa(...)}
  #wechat_redirect
```

微信托管页展示二维码 → 手机微信扫码确认 → 302 回 `/callback?code=...&state=...`。

后端 `idp/wechat.go`：

1. `GetToken`（`:91`）：`GET https://api.weixin.qq.com/sns/oauth2/access_token?grant_type=authorization_code&appid&secret&code`
   - 响应若含 `errcode` 直接报错（`:127`，如 `40163 code been used`）
   - 解析 `access_token / refresh_token / openid / unionid`；`openid` 通过 `token.WithExtra({"Openid": ...})` 传递（`:143-145`）
2. `GetUserInfo`（`:178`）：`GET https://api.weixin.qq.com/sns/userinfo?access_token&openid`
   - `id = unionid ?: openid`（`:228-231`）——**同一微信开放平台账号下 unionId 才跨应用稳定**
   - `Extra["wechat_unionid"] = openid`（注意：本仓库此处赋的是 `Openid`，与键名字面不符，若依赖此键需留意）、`Extra[BuildWechatOpenIdKey(appId)] = openid`（`:234-236`，因不同 appId 的 openid 不同，按 appId 分键存储）
3. 归一：`user.wechat == id`（unionId 优先，否则 openId）。

### 4.2 形态 B：微信内置浏览器免扫（公众号网页授权）

在微信里打开登录页时，`qrconnect` 需要"另一台设备扫码"，毫无意义，因此：

```js
// Provider.js:493-502
if (navigator.userAgent.includes("MicroMessenger")) {
  if (!provider.clientId2) { 提示"请在外部浏览器打开/请管理员配置公众号"; return "#"; }
  → https://open.weixin.qq.com/connect/oauth2/authorize
     ?appid={clientId2} &scope=snsapi_userinfo ... #wechat_redirect
}
```

- 走 `clientId2/clientSecret2`（**公众号**凭证），后端在 `FromProviderToIdpInfo`（`object/provider.go:632`）按 UA 自动切换凭证，前端无需传密钥。
- 该分支 `isShortState = true`：`state` 只放 `providerName`，真实参数存 `sessionStorage`。**副作用**：`sessionStorage` 不跨设备/跨浏览器，因此授权发起页与回调页必须是同一个浏览器标签会话。
- 若 Provider 的 `SubType == "Mobile"`，后端使用 `idp/wechat_mobile.go` 的 `WeChatMobileIdProvider`（`snsapi_userinfo` + `lang=zh_CN`），逻辑与 A 基本一致。

### 4.3 形态 C：公众号带参二维码（Casdoor 自建码 + 事件轮询）★ 真正的"自绘二维码扫码登录"

**触发条件**：`provider.clientId2 && provider.disableSsl && provider.signName === "media"`
（UI 上对应 Provider 编辑页 "Use WeChat Media Platform in PC" 开关 = `disableSsl`，"Follow-up action" 选 `media`；见 `web/src/provider/OAuthProviderFields.js:61-98`）

#### 前端两种呈现

| 位置 | 代码 | 行为 |
|---|---|---|
| 登录页 OAuth 图标（小图标模式） | `web/src/auth/ProviderButton.js:150` | 点击图标 → `Util.WechatOfficialAccountModal()`（`Util.js:222`）弹出 Modal 展示二维码 + 每 1s 轮询 |
| 应用 SigninMethods 配置 `WeChat` + rule `Login page` | `web/src/auth/LoginPage.js:1707-1717`、`:730` | 登录页侧栏/面板直接内嵌 `WeChatLoginPanel`（`web/src/auth/WeChatLoginPanel.js:36`），组件挂载/切换 tab 时自动取码并轮询，带"刷新"链接 |

#### 完整时序

```
① 前端 GET /api/get-qrcode?id={owner}/{name}
     routes: routers/router.go:65 → controllers.GetQRCode (auth.go:1476)
     → idp.GetWechatOfficialAccountQRCode(provider.ClientId2, ClientSecret2, providerId)
        a) GET https://api.weixin.qq.com/cgi-bin/token?grant_type=client_credential&appid={clientId2}&secret={secret2}
        b) POST https://api.weixin.qq.com/cgi-bin/qrcode/create?access_token=...
           body {"expire_seconds":3600,"action_name":"QR_STR_SCENE",
                 "action_info":{"scene":{"scene_str":"{owner/name}"}}}      ← scene_str = providerId
     响应 ResponseOk(url, ticket)   → data=二维码URL, data2=ticket
② 前端 <QRCode value={url}/> 渲染；setInterval 每 1000ms 调
     GET /api/get-webhook-event?ticket={ticket}   (routers/router.go:66 → auth.go:1456)
     后端仅判断内存表 idp.WechatCacheMap 是否存在该 ticket
       命中 → ResponseOk("SCAN", ticket)；否则 → ResponseError("ticket not found")
③ 用户手机微信"扫一扫" → 关注/扫码 → 微信服务器 POST 事件到 Casdoor
     /api/webhook   (routers/router.go:64 → controllers.HandleOfficialAccountEvent, auth.go:1384)
       · 只处理 Event ∈ {SCAN, SUBSCRIBE}，其余直接写空串返回
       · EventKey == scene_str == providerId → GetProvider(providerId)
       · 签名校验：idp.VerifyWechatSignature(provider.Content, nonce, timestamp, signature)
         （SHA1(sort{Token, timestamp, nonce}) 与 signature 比对，idp/wechat.go:329）
       · 写入 idp.WechatCacheMap[Ticket] = {IsScanned:true, WechatUnionId:FromUserName}
       · 返回空字符串（微信要求，否则会重试）
     注：GET /api/webhook 用于微信服务器接入验证，原样回显 echostr（auth.go:1385-1391）
④ 下一次轮询命中 → Util.getEvent (Util.js:212)
     code = ticket
     → Provider.getAuthUrl(application, provider, "signup", code)
        media 分支（Provider.js:504-506）**不再跳微信**，直接本地：
        {origin}/callback?state={state}&code=wechat_oa:{ticket}
⑤ AuthCallback → POST /api/login {provider, code:"wechat_oa:{ticket}", ...}
⑥ idp/wechat.go 特判 "wechat_oa:" 前缀：
     GetToken (:92)  → 不发网络请求，把 code 原样当 AccessToken 返回
     GetUserInfo(:182) → 取 accessToken[10:] 作为 ticket 查 WechatCacheMap
                         取到后 **delete 该 ticket（一次性消费）**
                         UserInfo{Id: unionId, Username/DisplayName: "wx_user_"+unionId, AvatarUrl:""}
⑦ user.wechat == unionId → 登录；否则按 BindingRule 匹配或注册
⑧ HandleLoggedIn → 回业务应用
```

#### 形态 C 的关键约束与风险

- **回调地址必须公网 HTTPS 可达**：`/api/webhook` 需登记在公众号"服务器配置"里，URL 为 `https://{你的域名}/api/webhook`，Token 与 `provider.content` 必须完全一致，否则签名校验失败（`invalid signature`）。
- **`WechatCacheMap` 是进程内内存 map**（`idp/wechat.go:35-38`，配 `sync.RWMutex`）：
  - 多副本部署时，`/api/webhook` 落在实例 A、前端轮询打到实例 B → 永远 `ticket not found`。**必须单实例，或做会话/回调同实例亲和**。
  - 服务重启后未消费的 ticket 丢失，需用户重新取码。
  - 只被"消费"删除，无 TTL 清理，长期运行有少量内存增长。
- **二维码有效期 3600s**（`expire_seconds`），过期后前端轮询一直失败，需点"刷新"重新取码。
- **UnionID 前提**：`FromUserName` 推的是 openid；只有公众号与网站应用**绑定到同一微信开放平台账号**时才能拿到一致的 unionId，否则该通道产生的 `user.wechat` 值与形态 A 不互通，会出现"同一个人两个账号"。
- **配置一致性**：`ProviderButton.js:150` 判定弹窗只看 `clientId2 + secret2 + disableSsl + 非 MicroMessenger`，而 `Provider.js:504` 判定 media 分支还额外要求 `signName === "media"`。若 `disableSsl=true` 但 `signName` 仍是 `open`，会出现"弹了公众号二维码，但最终跳去微信 qrconnect 页"的不一致。`ProviderEditPage.js:229-241` 有一段自动纠正 `signName` 的联动逻辑（无 clientId 时置 `media`、无 clientId2 或 `disableSsl=false` 时置 `open`）。

### 4.4 形态 D：微信小程序（不扫码，作为对照）

小程序不走浏览器授权码，而是独立通道：`object/token_oauth.go:63` 判断 `tag == "wechat_miniprogram"` → `GetWechatMiniProgramToken()`（`:494`）→ `idp/wechat_miniprogram.go` 的 `code2session` 拿 openid/unionId → `getUserByWechatId` → 建/取用户并发 Casdoor token。本文不展开。

---

## 5. 配置清单

### 5.1 Casdoor 侧

| 项 | 钉钉 | 微信（qrconnect） | 微信（公众号 media） |
|---|---|---|---|
| Provider.Category | `OAuth` | `OAuth` | `OAuth` |
| Provider.Type | `DingTalk` | `WeChat` | `WeChat` |
| clientId / clientSecret | 钉钉应用 AppKey / AppSecret | 开放平台**网站应用** AppID / AppSecret | 同左（可留空则自动置 `signName=media`） |
| clientId2 / clientSecret2 | — | 备用（公众号，用于内置浏览器） | **公众号 AppID / AppSecret（必填）** |
| disableSsl | — | — | **必须 true**（UI："Use WeChat Media Platform in PC"） |
| signName | — | — | **必须 `"media"`**（UI："Follow-up action"） |
| content | — | — | **微信服务器配置 Token**（签名校验用） |
| SubType | — | `Mobile` 可选（内置浏览器专用实现） | — |
| EnableProxy | 视网络 | 视网络 | 建议 false（需能直连 api.weixin.qq.com） |
| 挂到 Application.Providers | ✅ | ✅ | ✅（并设 `CanSignUp` / `BindingRule` / `SignupGroup`） |
| Application.SigninMethods | — | — | 可加 `WeChat` + rule `Login page` 让二维码内嵌在登录页 |

### 5.2 第三方平台侧

| 平台 | 必填配置 |
|---|---|
| 钉钉开放平台 | 应用类型（企业内部 H5 微应用 / 第三方个人应用，**旧版 open.dingtalk.com "扫码登录"应用不支持新 token 接口**）；回调域名 `https://{casdoor}/callback`；权限 `Contact.User.Read`；**修改后需在"版本管理与发布"发版** |
| 微信开放平台 | 网站应用授权回调域 `https://{casdoor}`（不带协议路径）；获取 UnionID 机制需账号已认证 |
| 微信公众平台 | 网页授权域名 `{casdoor}`；JS 接口安全域名；**服务器配置** URL `https://{casdoor}/api/webhook`、Token 与 `provider.content` 一致、消息格式 JSON/XML 需与解析匹配；"带参数二维码"权限 |

---

## 6. 关键文件索引

| 层 | 文件 | 作用 |
|---|---|---|
| 前端-授权 URL | `web/src/auth/Provider.js` | `authInfo` 端点表、`getAuthUrl()` 各 Provider 分支 |
| 前端-按钮 | `web/src/auth/ProviderButton.js`、`DingTalkLoginButton.js`、`WechatLoginButton.js` | 渲染图标/按钮，media 模式改弹窗 |
| 前端-二维码面板 | `web/src/auth/WeChatLoginPanel.js`、`web/src/auth/Util.js` | 取码、1s 轮询、`WechatOfficialAccountModal` |
| 前端-回调 | `web/src/auth/AuthCallback.js`、`web/public/AuthCallbackHandler.js`、`web/public/ProviderHintRedirect.js` | 解析 code/state → `POST /api/login` |
| 前端-后端调用 | `web/src/auth/AuthBackend.js` | `login` / `getWechatQRCode` / `getWechatMessageEvent` |
| 路由注册 | `routers/router.go:64-66` | `/api/webhook`、`/api/get-qrcode`、`/api/get-webhook-event` |
| 控制器 | `controllers/auth.go` | `Login`、`HandleLoggedIn`、`GetQRCode`、`GetWebhookEventType`、`HandleOfficialAccountEvent`、`Callback` |
| Provider 抽象 | `idp/provider.go` | `IdProvider` 接口、`UserInfo`、`GetIdProvider` 工厂 |
| 钉钉实现 | `idp/dingtalk.go` | 用户令牌 + 通讯录两级换取 |
| 微信实现 | `idp/wechat.go`、`idp/wechat_mobile.go`、`idp/wechat_miniprogram.go` | qrconnect、内置浏览器、带参二维码、`wechat_oa:` 伪码、签名校验、`WechatCacheMap` |
| 身份落库 | `object/user_util.go`（`GetUserByField`、`SetUserOAuthProperties`）、`object/user.go`（`LinkUserAccount`、社交字段） | 第三方 ID ↔ user 列 |
| 后台配置页 | `web/src/provider/OAuthProviderFields.js`、`web/src/ProviderEditPage.js` | `disableSsl`/`signName`/`content` 录入与联动 |

> `web2/`（shadcn 版）在 `web2/src/auth/Provider.ts`、`web2/src/backend/AuthBackend.ts` 有对应实现，逻辑与 `web/` 一致。

---

## 7. 安全性评估

> 本节结论均来自本仓库代码静态审读（含 `authz/authz.go` 公开接口清单），未做线上渗透验证。

### 7.0 公开（免鉴权）攻击面

`authz/authz.go:49-65` 将以下端点列入全局放行策略，任何人无需登录即可访问，这是扫码登录所有问题的放大器：

| 端点 | 用途 | 未认证可触发 |
|---|---|---|
| `POST /api/login` | 所有登录（含第三方 code 换令牌） | ✅ |
| `*  /api/webhook` | 微信服务器事件回调（写 `WechatCacheMap`） | ✅ |
| `GET /api/get-qrcode` | 用 Provider 的公众号凭证向微信申请带参二维码 | ✅ |
| `GET /api/get-webhook-event` | ticket 状态轮询 | ✅ |

### 7.1 高危

**H1 · 第三方身份自动关联（auto-link）默认按「昵称」匹配 → 账号接管**

- 证据：`controllers/auth.go:494-496` `BindingRule == nil` 时默认 `["Email","Phone","Name"]`；`Name` 规则走 `object.GetUserByFields(org, userInfo.Username)`（`object/user_util.go:77-89`，先精确匹配 `name` 列）；而微信 `userInfo.Username = nickname`（`idp/wechat.go:239`）**完全由用户自行设置**。
- 关键点：命中后直接 `SetUserOAuthProperties` → `linkUserByProvider` → `HandleLoggedIn`（`auth.go:1119-1133`），这段**位于 `EnableSignUp` / `CanSignUp` 校验之外**（那两个判断只在 `user == nil` 分支里，`auth.go:995-1004`）。因此「关闭自助注册」并不能阻断该路径。
- 攻击：新注册一个微信号 → 昵称改成组织内已有账号名（`admin`、工号、真实姓名）→ 点微信登录 → 该昵称对应的 Casdoor 账号被自动绑定并直接登录，绕过口令与 MFA。
- 修复：① 默认规则移除 `Name`；② auto-link 成功后必须再做一次强证明（口令/MFA）才落绑定；③ `Email` 规则要求 `user.EmailVerified`；④ 运营侧立即把应用 Providers 的 bindingRule 显式清空（空数组会走 `auth.go:497-499` 直接返回 nil，auto-link 完全关闭，此时仅能靠已绑定字段或管理员回填登录）。

**H2 · `EnableProxy` 打开即关闭 TLS 证书校验**

- 证据：`controllers/auth.go:426-432` → `proxy.ProxyHttpClient`；`proxy/proxy.go:95` `TLSClientConfig: &tls.Config{InsecureSkipVerify: true}`。
- 影响：勾选「启用代理」后，钉钉/微信的 `clientSecret`、授权 `code`、用户手机号/邮箱在链路上可被中间人读取与替换。UI 上该开关无任何风险提示（`web/src/provider/OAuthProviderFields.js:52`）。
- 修复：代理与跳过校验解耦，改为可配 RootCA；`InsecureSkipVerify` 默认 false 并在 UI 二次确认。

**H3 · 公众号 webhook 签名可绕过、可重放，缓存无 TTL/上限**

- 证据：`idp/wechat.go:329-342` `VerifyWechatSignature` = `SHA1(sort{token,timestamp,nonce})` 且用 `==` 比较；`controllers/auth.go:1432` 传入 `provider.Content`。**`Content` 为空时**，`timestamp`/`nonce` 均来自请求方，攻击者可自行算出合法签名；且对 `EventKey` 指向的 Provider 未校验 `Type==WeChat`/`signName==media`，任何 `content` 为空的 Provider 都能被借用。
- 无时间窗口校验 → 一次抓包可无限重放；`idp.WechatCacheMap`（`idp/wechat.go:35-38`）只在被消费时删除，无过期、无容量上限。
- 影响：未认证投毒/内存增长；若某 ticket 经日志、反向代理、崩溃栈等途径泄露，可在任意时刻以对应 unionId 完成登录。
- 修复：拒绝空 `Content`；`|now - timestamp| > 5min` 拒绝；`hmac.Equal` 常量时间比较；限制 `EventKey` 仅接受 WeChat-media 类型 Provider；给缓存加 TTL（对齐二维码 3600s）+ LRU 上限；对 `EventKey`/`Ticket` 做格式校验。

**H4 · `state` 未做任何 CSRF 校验（形同虚设）**

- 证据：前端把 `body.state` 设为 `applicationName`（`web/src/auth/AuthCallback.js:284`），后端 `auth.go:918-922` 比较的 `application` 恰是由同一个 `applicationName` 解析而来（`auth.go:854`）→ `stateApplicationName != application.Name` 恒假；`authState` 配置是全局常量 `"casdoor"`（`conf/app.conf:13`），不能当 CSRF token。
- 影响：**登录 CSRF**。攻击者自己走完钉钉/微信授权拿到 `authCode`，诱导受害者访问 `/callback?authCode=...&state=<base64>`，受害者浏览器即被种上「攻击者 Casdoor 账号」的会话；配合工单/远程协助类社工可造成操作归属混淆与数据投递到攻击者账号。同时，「谁扫了码」与「哪个浏览器发起」之间没有任何绑定，media 形态的二维码转发（把码截图发给别人扫）也无法被检测。
- 修复：`state` 改为服务端签发的一次性随机 nonce（HttpOnly + SameSite=Lax Cookie 绑定），`/api/login` 校验并消费；不依赖客户端 base64。

### 7.2 中危

| 编号 | 问题 | 证据 | 修复 |
|---|---|---|---|
| M1 | `state`/`redirect_uri` 未 `encodeURIComponent`，base64 中 `+` 被解成空格 → `application` 解析为空 → 报「应用不存在」；也破坏 state 完整性 | `Provider.js:491`（钉钉）、`:507`（微信 qrconnect）、`:513/516`（企业微信） | 统一 encode 后重编前端 |
| M2 | 出站 HTTP 全无超时：`DefaultHttpClient = http.DefaultClient`（Timeout=0）、`idp/wechat.go:258,294` 用 `new(http.Client)`。上游变慢即拖垮登录；且 `/api/get-qrcode` 免鉴权，可被无限触发（每次新取公众号 access_token，可能挤掉有效 token 并耗尽配额 → 登录通道自瘫） | `proxy/proxy.go:37`、`authz.go:64` | 统一 `Timeout: 10s`；`/api/get-qrcode` 限流 + access_token 缓存 |
| M3 | 第三方 access/refresh token 明文写入 `user.properties`，掩码仅在 `!isAdminOrSelf` 生效 → 组织管理员可读他人原文 | `object/user_util.go:268-279`、`object/user.go:686-702` | 静态加密；对管理员一并掩码（无回显需求） |
| M4 | 上游错误原文回显到前端，成为凭证有效性 oracle（`/api/get-qrcode` 的 `Fail to fetch WeChat QRcode: %s` 同理） | `idp/dingtalk.go:100`、`idp/wechat.go:128`、`auth.go:938,1491` | 对外泛化文案，详情进服务端日志 |
| M5 | Casdoor 侧不记录已消费 `code`，重放保护完全依赖上游；`wechat_oa:` ticket 反而是三者中实现最严谨的（消费即删） | `auth.go:925`、`idp/wechat.go:191-193` | 对第三方 code 做短 TTL 去重（LRU/Redis） |
| M6 | `GET /api/get-webhook-event?ticket=` 是免鉴权布尔 oracle，ticket 在前端 1s 轮询中可能进入访问日志 | `auth.go:1456-1468` | 轮询接口要求会话绑定；反代剥离该 query |
| M7 | `RequestBodyFilter` 在 Beego `MaxMemory` 之前 `io.ReadAll` 且无大小上限 → 免鉴权 POST 大 body 造成内存压力 | `routers/request_body_filter.go:39` | `io.ReadAll(io.LimitReader(...))` |
| M8 | 应用未挂载该 Provider 时 `providerItem` 为 nil 却直接调用方法，触发 panic（net/http 每连接 recover 兜为 500，进程不崩，但未认证可稳定制造 panic 日志与断连） | `auth.go:883-884` + `object/provider_item.go:32-39` | `if providerItem == nil { ResponseError }` |

### 7.3 设计固有 / 低危

- **L1 二维码转发**：media 形态由 Casdoor 自绘二维码，手机上只看到「关注/扫码」，不展示发起端信息。建议在公众号回复消息中回显发起浏览器的 IP/城市/UA，PC 端扫码后二次确认。
- **L2 单实例内存态**：`WechatCacheMap` 为进程内 map，多副本部署时 webhook 与轮询落到不同实例会永久 `ticket not found`；重启即丢失。该通道不具备水平扩展性。
- **L3 键名与值不符**：`idp/wechat.go:234` `extra["wechat_unionid"] = wechatUserInfo.Openid`，下游若按名取用会拿到 openid。
- **L4 字段语义复用**：`disableSsl` 被复用为「PC 使用公众号」开关、`signName` 控制通道分流，误配会同时改变多条安全分支；`ProviderButton.js:150`（弹窗条件）与 `Provider.js:504`（media 分支条件）判定不一致。
- **L5 静默失败**：`idp/dingtalk.go:190-209` 企业通讯录详情获取失败时不告警，资料缺失会悄悄影响后续 BindingRule 匹配。

### 7.4 实现中值得保留的正确做法

- AppSecret 只在服务端使用，前端授权 URL 仅携带 clientId；Provider 读取接口有掩码。
- `GetUserByField` 用 `util.FilterSQLIdentifier(field)` 白名单校验列名（`object/user_util.go:40`），防止把 `provider.Type` 拼成 SQL 注入点——这类由外部输入决定的列名很容易踩坑，此处防护到位。
- ticket 单次消费（`idp/wechat.go:191-193`）。
- 所有扫码登录都必须过 `HandleLoggedIn`（`auth.go:60-160`）的统一闸门：`IsForbidden`/`IsDeleted`、`CheckEntryIp`、应用与组织 `DisableSignin`、Casbin `CheckLoginPermission`、用户 Tag 限制、`paid-user` 订阅、独占登录踢下线——扫码与口令登录共享同一授权判定，没有旁路。
- 会话过期收敛：未勾选自动登录时会话上限 24h（`auth.go:299-310`）。
- 审计统一：成功 `RecordUserSignin`、失败 `recordDetail`（含 MFA 失败等原因码），与口令登录同链路。

### 7.5 上线前最小检查清单（不改代码即可做）

1. 目标应用的 Providers 行，**bindingRule 显式清空**（或至少删除 `Name`）。
2. `application.EnableSignUp = false` 且微信/钉钉 Provider 项 `CanSignUp = false`。
3. 钉钉/微信 Provider **不要勾选 EnableProxy**；确需出网请走网络层代理而非该开关。
4. 微信 media 形态：确认 `provider.content` 非空、与公众号服务器配置 Token 一致；`/api/webhook` 在反代日志中关闭 query 记录。
5. 反代对 `/api/get-qrcode`、`/api/get-webhook-event`、`/api/webhook`、`/api/login` 配置速率限制（Casdoor 自身无内置限流，`routers/` 无 rate-limit 过滤器，CasWAF 仅作用于 Site 代理链路）。
