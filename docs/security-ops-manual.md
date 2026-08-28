# Casdoor 生产环境安全运维手册

| 项目 | 内容 |
| --- | --- |
| 文档版本 | v1.0 |
| 适用系统 | Casdoor IAM Server（systemd 二进制部署） |
| 部署方式 | `deploy-systemd.sh`（编译二进制 + systemd + 远程 PostgreSQL） |
| 默认安装目录 | `/msun/app/casdoor`（`INSTALL_DIR`，与脚本/二进制同目录） |
| 默认日志目录 | `/msun/logs/casdoor`（100MB 轮转，保留 30 天） |
| 密级 | 内部 — 限制分发 |

---

## 目录

1. [系统架构与资产清单](#1-系统架构与资产清单)
2. [账号与权限管理](#2-账号与权限管理)
3. [网络安全](#3-网络安全)
4. [主机与 systemd 加固](#4-主机与-systemd-加固)
5. [应用层安全配置](#5-应用层安全配置)
6. [密钥与凭据管理](#6-密钥与凭据管理)
7. [日志与审计](#7-日志与审计)
8. [备份与恢复](#8-备份与恢复)
9. [监控与告警](#9-监控与告警)
10. [补丁与漏洞管理](#10-补丁与漏洞管理)
11. [应急响应](#11-应急响应)
12. [周期性安全巡检清单](#12-周期性安全巡检清单)

---

## 1. 系统架构与资产清单

### 1.1 部署拓扑

```
                    ┌─────────────────────────────┐
   用户/浏览器 ────► │  Nginx/SLB (TLS 终结, 建议)   │
                    └──────────────┬──────────────┘
                                   │ http://127.0.0.1:8000
                    ┌──────────────▼──────────────┐
                    │  casdoor.service (非 root)   │
                    │  · Web/API    :8000/tcp      │
                    │  · LDAP       :389/tcp       │
                    │  · RADIUS     :1812/udp      │
                    │  · WAF: Coraza(DetectionOnly)│
                    └───────┬──────────────┬───────┘
                            │ TLS (sslmode=require)
                 ┌──────────▼───┐   ┌──────▼────────┐
                 │ 远程 PostgreSQL│   │ 日志 /msun/logs│
                 │ schema=casdoor│   │ 100MB×30d 轮转 │
                 └───────────────┘   └───────────────┘
```

### 1.2 端口与资产清单

| 端口 | 协议 | 用途 | 暴露范围 | 风险等级 |
| --- | --- | --- | --- | --- |
| 8000/tcp | HTTP | Web 控制台 / API / 健康检查 | 仅本机/内网，经反代对外 | 高 |
| 389/tcp | LDAP | 内置 LDAP Server（AD 同步、认证） | 仅内网，按客户端 IP 白名单 | 高 |
| 1812/udp | RADIUS | NAS/网络设备认证 | 仅设备网段 | 高 |
| 5432/tcp | PostgreSQL | 远程数据库（出方向） | 仅应用服务器可访问 | 高 |

### 1.3 关键文件资产

| 路径 | 权限 | 说明 |
| --- | --- | --- |
| `${INSTALL_DIR}/casdoor` | 0755 root:root（部署时 casdoor:casdoor） | 主二进制 |
| `${INSTALL_DIR}/conf/app.conf` | 0640 casdoor:casdoor | **含 DB 密码、radiusSecret，敏感** |
| `${INSTALL_DIR}/init_data.json` | 0640 casdoor:casdoor | 初始数据（含内置账号，敏感） |
| `/etc/systemd/system/casdoor.service` | 0644 root:root | 服务单元（含沙箱策略） |
| `/msun/logs/casdoor/casdoor.log` | 0640 casdoor:casdoor | 应用日志（目录 0750） |
| `${INSTALL_DIR}/web/build` | — | 前端静态资源 |

---

## 2. 账号与权限管理

### 2.1 系统账号

- 服务以专用系统账号 `casdoor` 运行：`nologin` shell、无 home、无密码登录权限。
- **禁止**将该账号添加 sudoers 或设置可登录密码。

验证（巡检项 A-1）：

```bash
id casdoor
# 期望: uid=...(casdoor) gid=...(casdoor) groups=...(casdoor)
grep casdoor /etc/passwd
# 期望 shell 为 /usr/sbin/nologin
sudo passwd -S casdoor   # 期望 L (locked)
```

### 2.2 Casdoor 管理账号

| 要求 | 说明 |
| --- | --- |
| 修改内置 admin 密码 | 首次上线立即在控制台修改 `built-in/admin`，长度 ≥16，含大小写/数字/符号 |
| 启用 MFA | 管理员账号强制开启 MFA（TOTP/WebAuthn） |
| 最小权限 | 普通运维只授予对应 Organization 的权限，不共用全局 admin |
| 禁用演示账号 | 删除/禁用 `isDemoMode = false` 下的一切演示用户与示例 Application |
| 会话超时 | 设置 `inactiveTimeoutMinutes`（建议 ≤ 30） |

### 2.3 数据库账号

- 应用使用最小权限账号 `casdoor_user`（非超级用户 `postgres`），仅授权 `casdoor` schema：

```sql
-- 巡检项 A-2：确认无超级用户权限
SELECT rolname, rolsuper, rolcreaterole, rolcreatedb
FROM pg_roles WHERE rolname = 'casdoor_user';
-- 期望 rolsuper = f, rolcreaterole = f, rolcreatedb = f

-- 权限收敛（首次上线执行）
REVOKE ALL ON DATABASE casdoor FROM PUBLIC;
GRANT USAGE ON SCHEMA casdoor TO casdoor_user;
ALTER DEFAULT PRIVILEGES IN SCHEMA casdoor
  GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO casdoor_user;
```

- 数据库侧限制来源 IP（`pg_hba.conf` 仅允许应用服务器 IP，`sslmode=require`）：

```
# pg_hba.conf 示例
hostssl casdoor casdoor_user 10.0.0.10/32 scram-sha-256
# 其余 host 条目一律拒绝
```

### 2.4 密码策略（Casdoor 组织级）

对每个 Organization 设置：最小长度 ≥ 12、密码强度正则开启、登录失败锁定（如 5 次失败锁 15 分钟）、禁止密码包含用户名。

---

## 3. 网络安全

### 3.1 防火墙基线（应用服务器）

仅放行业务必需端口；管理端口（SSH 22）限跳板机 IP。

```bash
# nftables 示例（按实际网段替换）
sudo nft add rule inet filter input ip saddr 10.0.0.0/24 tcp dport 8000 accept   # 仅反代/内网
sudo nft add rule inet filter input ip saddr 10.1.0.0/24 tcp dport 389  accept   # LDAP 客户端
sudo nft add rule inet filter input ip saddr 10.2.0.0/24 udp dport 1812 accept   # RADIUS 设备
sudo nft add rule inet filter input ip saddr 10.9.9.9 tcp dport 22 accept        # 跳板机
sudo nft add rule inet filter input drop
```

巡检项 B-1：`sudo ss -tlnp | grep -E '8000|389|1812'` 确认监听地址符合预期（8000 应仅 127.0.0.1 或内网网卡）。

### 3.2 TLS 强制

- 对外统一由 Nginx/SLB 终结 TLS 1.2+，Casdoor 本体 8000 端口**不直接暴露公网**。
- 数据库连接保持 `PG_SSLMODE=require`（默认）。若远程 PG 已部署可信 CA，建议升级为 `verify-full`：

```bash
sudo PG_SSLMODE=verify-full ./deploy-systemd.sh   # 需 PG 侧证书与 lib/pq sslrootcert 支持
```

- LDAPS（636）：如需加密 LDAP，在 Casdoor 上传证书后将 `ldapsServerPort` 设为 636 并开启 `ldapsCertId`。

### 3.3 RADIUS 共享密钥

- `radiusSecret` 由部署脚本自动生成 32 位强随机值（约 195 bit 熵），**禁止**手工改为弱值。
- 密钥变更流程：`sudo RADIUS_SECRET='<新密钥>' ./deploy-systemd.sh` 重启后，**同步更新所有 NAS/网络设备**，否则认证全部失败。

---

## 4. 主机与 systemd 加固

### 4.1 已内置的沙箱（deploy-systemd.sh 生成，勿削弱）

| 指令 | 作用 |
| --- | --- |
| `User=casdoor` / 非 root | 最小权限运行 |
| `NoNewPrivileges=true` | 阻止子进程提权（setuid/sgid 失效） |
| `PrivateTmp=true` | 私有 /tmp，防本地临时文件攻击 |
| `ProtectSystem=full` | /usr /boot /etc 只读 |
| `ProtectHome=read-only` | /home 只读（保留 lib/pq 客户端证书探测能力） |
| `CapabilityBoundingSet=CAP_NET_BIND_SERVICE` | 仅保留绑定 389 特权端口的能力，其余 capability 全部丢弃 |
| `LimitNOFILE=65536` | 文件描述符上限 |

巡检项 C-1（确认单元未被篡改、与脚本一致）：

```bash
systemctl cat casdoor | grep -E 'NoNewPrivileges|ProtectSystem|CapabilityBounding'
# 运行时实际生效情况：
sudo systemd-run --property=... 无需执行；直接:
systemctl show casdoor -p NoNewPrivileges -p PrivateTmp -p ProtectSystem -p CapabilityBoundingSet
```

### 4.2 文件完整性

- 上线后对关键文件建立基线哈希，巡检时比对：

```bash
sudo sha256sum \
  /etc/systemd/system/casdoor.service \
  ${INSTALL_DIR}/casdoor \
  ${INSTALL_DIR}/conf/app.conf > /root/casdoor.baseline.sha256

# 巡检比对（预期仅升级后变化）
sudo sha256sum -c /root/casdoor.baseline.sha256
```

- 严格权限复核（巡检项 C-2）：

```bash
stat -c '%a %U:%G %n' ${INSTALL_DIR} ${INSTALL_DIR}/conf/app.conf /msun/logs/casdoor
# 期望: 750/640 且属主 casdoor:casdoor（安装目录 0750、app.conf 0640）
```

### 4.3 内核与主机基线

```bash
# SYN flood / 连接层防护（按需）
sudo sysctl -w net.ipv4.tcp_syncookies=1
# fail2ban 监控 SSH 与 Nginx 暴力破解（建议）
```

---

## 5. 应用层安全配置

### 5.1 conf/app.conf 安全相关键

| 键 | 生产建议值 | 说明 |
| --- | --- | --- |
| `runmode` | `prod` | 脚本默认已设，禁用 dev |
| `showSql` | `false` | 防 SQL 语句泄漏到日志 |
| `enableErrorMask` | `false`（如无合规要求可 true） | 错误信息脱敏 |
| `isDemoMode` | `false` | 关闭演示模式 |
| `logPostOnly` | `true` | 日志仅记录 POST（含安全事件） |
| `verificationCodeTimeout` | `10`（分钟） | 验证码有效期，不建议调大 |
| `socks5Proxy` | 空 | 脚本已清空，勿填公网代理 |
| `initDataNewOnly` | 视需要 `true` | 防止重启时重放 init_data.json 覆盖现网数据 |
| `quota` | 按组织设上限 | 防资源滥用（-1 为不限制，内网可控） |

### 5.2 WAF（Coraza）

- 规则文件：`${INSTALL_DIR}/conf/waf.conf`（若部署目录含）或源码 `conf/waf.conf`。
- 当前 `SecRuleEngine DetectionOnly`：**只检测不阻断**。上线流程：
  1. 观察期 ≥ 2 周，通过日志确认无误报；
  2. 切换 `SecRuleEngine On` 进入阻断模式；
  3. 请求体上限 `SecRequestBodyLimit 13107200`（12.5MB），如无大文件上传可收紧至 2MB。
- 巡检项 D-1：确认规则引擎模式未被回退为 Off。

### 5.3 OAuth/OIDC 应用侧

- 所有接入 Application：开启 PKCE（public client 必须）、redirectURI 精确白名单（禁通配）、Access Token 有效期 ≤ 1h、Refresh Token 按需轮换。
- ClientSecret 泄漏处置：立即在控制台重置 secret，并排查 token 使用日志。
- 管理 API（`/api/...`）仅允许持有 client credentials/管理会话调用，网关层屏蔽 `/api/setup` 等初始化端点的公网访问。

### 5.4 LDAP/RADIUS 服务面

- LDAP 389 明文：仅限内网可信网段 + 防火墙白名单；跨不可信网络必须走 LDAPS。
- RADIUS：PAP 经共享密钥隧道，密钥轮换周期 ≤ 90 天（见 §6.2）。

---

## 6. 密钥与凭据管理

### 6.1 凭据清单与存放

| 凭据 | 位置 | 管理要求 |
| --- | --- | --- |
| PG 密码 | `conf/app.conf` (0640) + 部署时环境变量 | 环境变量勿落入 shell history（`export HISTCONTROL=ignorespace`，命令前加空格） |
| radiusSecret | `conf/app.conf` (0640) | 90 天轮换，与 NAS 设备同步 |
| Casdoor 管理员密码 | 数据库（bcrypt） | 90 天轮换 + MFA |
| ClientSecrets | 数据库 | 泄漏即重置 |
| TLS 证书/私钥 | Nginx / Casdoor 证书库 | 见 §6.3 |

### 6.2 轮换操作手册

**数据库密码轮换（PG 侧先改，应用侧后改）**

```bash
# 1. PG 侧
ALTER ROLE casdoor_user WITH PASSWORD '<新密码>';
# 2. 应用侧重新部署（脚本幂等，仅重写配置并重启）
sudo PG_HOST=10.0.0.5 PG_PASSWORD='<新密码>' ./deploy-systemd.sh
# 3. 验证
curl -s http://127.0.0.1:8000/api/health
```

**RADIUS 密钥轮换**

```bash
sudo RADIUS_SECRET='<新密钥>' ./deploy-systemd.sh
# 立即同步所有 NAS 设备；窗口期可能出现认证失败，安排维护窗口执行
```

### 6.3 证书管理

- 建立证书台账（域名、颁发者、到期日），到期前 30 天告警。
- 到期检查脚本（可入 cron）：

```bash
echo | openssl s_client -connect sso.example.com:443 2>/dev/null \
  | openssl x509 -noout -enddate
```

### 6.4 通用红线

- 禁止将密码/密钥写入 Git 仓库、聊天工具、工单。
- 部署脚本调用示例中的密码用占位符，真实值仅通过密码管理系统/环境变量传递。
- 源码目录中的 `conf/app-pg.conf`、`app-sqlite.conf` 含开发默认口令（123456/secret），**严禁**直接用于生产。

---

## 7. 日志与审计

### 7.1 日志布局

| 来源 | 位置 | 轮转 |
| --- | --- | --- |
| 应用日志 | `/msun/logs/casdoor/casdoor.log`（perm 0640） | 100MB/文件，30 天自动清理（beego maxsize/maxdays） |
| systemd stdout/stderr | journald | 按 journald 全局策略 |
| Nginx 访问日志 | 反代服务器 | logrotate |
| PostgreSQL 日志 | DB 服务器 | logrotate |

### 7.2 常用排查命令

```bash
# 服务状态与最近日志
systemctl status casdoor --no-pager -l
journalctl -u casdoor -n 100 --no-pager
journalctl -u casdoor -f                        # 实时跟踪

# 应用日志：登录失败 / 权限异常 / 5xx
grep -iE 'fail|denied|error|panic' /msun/logs/casdoor/casdoor.log | tail -50

# 登录审计（logPostOnly=true 下 POST 记录）
grep '/api/login' /msun/logs/casdoor/casdoor.log
```

### 7.3 审计要点

- **登录审计**：管理员登录时间、来源 IP、成功/失败（发现非跳板机 IP 的 admin 登录即触发 IR）。
- **配置变更审计**：`app.conf` 与 `casdoor.service` 的 mtime/哈希变化（§4.2 基线比对）。
- **对象变更审计**：Organization/User/Application/Provider 的创建删除（Casdoor 操作日志 + 数据库触发器或同步对比）。
- **RADIUS/LDAP 认证异常**：短时间大量失败 = 撞库或设备密钥失配。

### 7.4 日志保护

- 日志目录属主 `casdoor:casdoor`、0750；其他账号不可读。
- 若有日志外送（syslog/vector），传输走 TLS；本地保留 ≥ 90 天（合规要求更高时外置归档）。
- journald 防篡改：`SystemMaxFiles`、`ForwardToSyslog=yes` 双写。

---

## 8. 备份与恢复

### 8.1 备份对象与策略

| 对象 | 方式 | 频率 | 保留 | 异地 |
| --- | --- | --- | --- | --- |
| PostgreSQL（casdoor schema） | `pg_dump -Fc` | 每日全量（凌晨低峰） | 30 天 + 每周归档 | 是 |
| `conf/app.conf` + `init_data.json` + systemd 单元 | tar 加密归档 | 每次变更后 + 每周 | 90 天 | 是 |
| 证书/密钥备份 | 密码库 | 变更即备 | — | — |

### 8.2 备份脚本示例（应用服务器上仅备配置；DB 备份在 DB 侧执行）

```bash
#!/usr/bin/env bash
set -Eeuo pipefail
BAK=/msun/backup/casdoor/$(date +%F)
mkdir -p "$BAK"
tar -C /msun/app/casdoor -czf "$BAK/conf-$(date +%F%H%M).tgz" conf init_data.json
cp /etc/systemd/system/casdoor.service "$BAK/"
find /msun/backup/casdoor -maxdepth 1 -mtime +30 -exec rm -rf {} +
```

DB 侧（远程 PostgreSQL 服务器）：

```bash
pg_dump -h 127.0.0.1 -U backup_user -Fc -f /backup/casdoor-$(date +%F).dump casdoor
# 每季度至少一次恢复演练：恢复到临时库并抽查登录/同步功能
pg_restore -d casdoor_verify -c /backup/casdoor-2026-08-26.dump
```

### 8.3 恢复目标

| 指标 | 目标 |
| --- | --- |
| RPO（数据丢失容忍） | ≤ 24h（有 WAL 归档可到分钟级） |
| RTO（服务恢复时间） | ≤ 30 分钟（重跑部署脚本 + 恢复 DB） |

### 8.4 灾难重建（全损场景）

```bash
# 1. 恢复操作系统基线与防火墙
# 2. 上传二进制与部署脚本到 /msun/app/casdoor
# 3. 恢复数据库（见 8.2）
# 4. 重新部署（复用原 radiusSecret 以免重配 NAS，见 §3.3）
sudo PG_HOST=10.0.0.5 RADIUS_SECRET='<原密钥>' ./deploy-systemd.sh
# 5. 验证：健康检查 + 管理台登录 + LDAP/RADIUS 各认证一次
```

---

## 9. 监控与告警

### 9.1 探活

```bash
# 健康端点（部署脚本同款）
curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:8000/api/health
# 期望 200；连续 3 次非 200 触发告警
```

systemd 层（Prometheus node_exporter systemd collector 或脚本）：

```bash
systemctl is-active casdoor          # active
systemctl show casdoor -p NRestarts  # 重启次数突增 = 数据库/网络异常
```

### 9.2 建议指标与阈值

| 指标 | 阈值 | 动作 |
| --- | --- | --- |
| `/api/health` 可用性 | < 99.9%/月 | 告警 |
| 服务重启次数（NRestarts） | 1 小时 > 5 | 告警（多为 DB 不可达） |
| 登录失败次数 | 5 分钟 > 50 同源 IP | 告警 + 自动封禁（fail2ban/网关） |
| 8000/389/1812 端口连接数 | 突增 3σ | 告警（扫描/爆破） |
| 磁盘（日志/DB 卷） | > 80% | 告警；> 90% 自动清理旧轮转 |
| PG 连接数 / 慢查询 | 连接 > 80%、慢查询突增 | 告警 |
| 证书有效期 | < 30 天 | 告警 |

### 9.3 日志告警关键字

`panic`、`password`（异常频率）、`permission denied`、`sql error`、`radius` 失败风暴、非白名单 IP 的 admin 登录。

---

## 10. 补丁与漏洞管理

### 10.1 升级流程（幂等重部署）

```bash
# 1. 变更窗口公告；快照/备份（§8）
# 2. 替换二进制（保留原文件用于回退）
cp /msun/app/casdoor/casdoor /msun/app/casdoor/casdoor.bak-$(date +%F)
cp server_linux_amd64 /msun/app/casdoor/casdoor
# 3. 重跑部署脚本（幂等：复用 conf 中 radiusSecret，仅重写配置并重启）
sudo PG_HOST=10.0.0.5 ./deploy-systemd.sh
# 4. 验证：health 200 + 核心登录/LDAP/RADIUS 冒烟
# 5. 回退：cp casdoor.bak-xxx casdoor && sudo ./deploy-systemd.sh
```

### 10.2 漏洞跟踪

- 订阅 Casdoor 上游安全公告（GitHub advisories / casdoor.org）；漏洞报告渠道见仓库 `SECURITY.md`（admin@casdoor.org）。
- 依赖扫描：CI 阶段 `govulncheck ./...`、前端 `npm audit`；镜像/二进制定期 Trivy 扫描。
- SLA：严重漏洞 ≤ 7 天修复，高危 ≤ 30 天，中危 ≤ 90 天。

### 10.3 变更管理

- 任何 `app.conf`、systemd 单元、防火墙变更走变更单：变更内容、回退方案、验证步骤、审批人。
- 生产变更须两人复核（一人执行一人验证）。

---

## 11. 应急响应

### 11.1 应急联系人（上线时填写）

| 角色 | 姓名 | 电话 | 备份联系人 |
| --- | --- | --- | --- |
| 一线运维 | | | |
| 应用负责人 | | | |
| DBA | | | |
| 安全负责人 | | | |

### 11.2 事件分级

| 级别 | 定义 | 响应时限 |
| --- | --- | --- |
| P0 | 疑似数据泄漏/入侵/管理台沦陷 | 立即，15 分钟内响应 |
| P1 | 服务不可用 > 15 分钟 / DB 不可达 | 15 分钟 |
| P2 | 单功能异常（LDAP 或 RADIUS 失败）、性能劣化 | 1 小时 |
| P3 | 一般缺陷 | 下个工作日 |

### 11.3 常见故障 Runbook

**服务启动失败/持续重启（多为远程 DB 故障）**

```bash
systemctl status casdoor -l --no-pager
journalctl -u casdoor -n 100 --no-pager | grep -iE 'panic|postgres|connect'
# 网络通断
timeout 3 bash -c '</dev/tcp/10.0.0.5/5432' && echo PG可达 || echo PG不可达
# DB 恢复后服务自愈（Restart=on-failure + StartLimitIntervalSec=0 不限重试）
```

**端口未监听**

```bash
sudo ss -tlnp | grep -E ':8000|:389'; sudo ss -ulnp | grep ':1812'
curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:8000/api/health
```

**RADIUS 全部认证失败**：多为共享密钥失配 → 比对 `conf/app.conf` 的 `radiusSecret` 与 NAS 配置；或 1812/udp 被防火墙拦截。

**配置损坏恢复**：重跑部署脚本会完整重建 `app.conf`（幂等），无需模板。

### 11.4 安全事件处置（P0）

1. **隔离**：`sudo systemctl stop casdoor`；防火墙阻断攻击源 IP；必要时摘除 SLB。
2. **保全**：立即快照磁盘 + 导出日志（`journalctl -u casdoor --since ... > incident.log`；打包 `/msun/logs/casdoor`）。
3. **取证**：检查 admin 登录记录、新增 Application/OAuth client、异常 token 签发；比对 §4.2 文件基线。
4. **清除与恢复**：重置全部管理员密码与 ClientSecret、轮换 DB 密码与 radiusSecret（§6.2）、修复入侵路径后按 §8.4 重建。
5. **复盘**：5 个工作日内出事件报告（时间线、根因、整改项）。

---

## 12. 周期性安全巡检清单

### 每日（5 分钟，可脚本化）

- [ ] `curl /api/health` = 200
- [ ] `systemctl is-active casdoor` = active，NRestarts 无突增
- [ ] 登录失败日志无爆破特征（§7.2 命令）
- [ ] 磁盘使用率 < 80%
- [ ] 昨日数据库备份存在且大小正常

### 每周

- [ ] 巡检项 A-1/A-2：系统账号状态、DB 账号权限
- [ ] 巡检项 B-1：监听端口与防火墙规则无未授权变更
- [ ] 巡检项 C-1/C-2：systemd 沙箱指令、文件权限与属主
- [ ] `sha256sum -c` 文件基线比对（升级除外）
- [ ] WAF 观察日志回顾（误报/攻击趋势）
- [ ] journald 磁盘占用 `journalctl --disk-usage`

### 每月

- [ ] 管理员账号清点：删除离职/冗余账号，确认全员 MFA
- [ ] OAuth Application 清点：redirectURI、token 有效期、无主 client 下线
- [ ] 证书台账核对与到期预警（< 30 天处理）
- [ ] 恢复演练：恢复一份备份到临时库并验证
- [ ] 依赖漏洞扫描（govulncheck / npm audit / Trivy）
- [ ] PostgreSQL 安全更新与 `pg_hba.conf` 复核

### 每季度

- [ ] 轮换：DB 密码、radiusSecret、管理员密码（§6.2）
- [ ] 全量灾备重建演练（§8.4）
- [ ] 本手册评审更新（联系人、架构、阈值）
- [ ] 权限再评审（组织/应用/数据库三层）

---

## 附录 A：上线安全基线核查表（一次性）

- [ ] 内置 admin 密码已修改且开启 MFA；演示账号已清理
- [ ] `runmode=prod`、`showSql=false`、`isDemoMode=false`
- [ ] 8000 端口未直接暴露公网，经 TLS 反代
- [ ] 防火墙仅放行 8000/389/1812/22 白名单
- [ ] DB 使用 `casdoor_user` 最小权限账号，`pg_hba.conf` 限源 IP + scram-sha-256
- [ ] `conf/app.conf` 权限 0640、属主 casdoor；radiusSecret 为随机值非默认
- [ ] systemd 沙箱指令齐全（§4.1 表全部在位）
- [ ] 日志轮转生效（首日观察 casdoor.log 生成与权限 0640）
- [ ] 备份任务上线并完成一次恢复验证
- [ ] 监控告警（health/重启/登录失败/磁盘/证书）全部接入
- [ ] 文件基线哈希已归档（§4.2）
- [ ] 应急联系人表已填写

## 附录 B：敏感文件位置速查

| 内容 | 命令 |
| --- | --- |
| 当前 DSN/密钥 | `sudo grep -E 'dataSourceName\|radiusSecret' /msun/app/casdoor/conf/app.conf` |
| 服务单元 | `systemctl cat casdoor` |
| 健康检查 | `curl -s http://127.0.0.1:8000/api/health` |
| 实时日志 | `journalctl -u casdoor -f` / `tail -f /msun/logs/casdoor/casdoor.log` |
| 卸载（保留数据） | `sudo ./deploy-systemd.sh --uninstall` |
| 卸载（含安装目录） | `sudo ./deploy-systemd.sh --uninstall --purge` |

---

*本手册随架构变更同步修订；修订记录：v1.0 首次发布（2026-08-26）。*
