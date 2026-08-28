#!/usr/bin/env python3
"""
zymsunsoft 组织从 AD (LDAP) 认证切换到 Casdoor 本地认证的迁移脚本。

背景：
  AD 即将废弃，需要将已通过 LDAP 同步到 Casdoor 的用户和群组
  切换为 Casdoor 本地认证模式。

功能：
  1. 停止 LDAP 自动同步（autoSync 置 0）
  2. 将所有用户的 ldap 字段清空（解除 AD 绑定）
  3. 为每个用户设置本地密码（用于 Casdoor 本地认证）
  4. 将所有群组类型从 ldap-synced/ad-synced 改为本地
  5. 删除 LDAP 配置（可选）
  6. 回读验证

用法：
  1. 修改下方 CONFIG 配置块（或设置同名环境变量）
  2. pip install requests
  3. python3 migrate_ad_to_local.py

认证方式：
  使用 Casdoor Application 的 clientId / clientSecret 进行 API 认证。
"""

from __future__ import annotations

import json
import os
import sys
import time
import requests

# ============================================================
#  配置区 —— 请根据实际环境修改（也可通过同名环境变量覆盖）
# ============================================================

CONFIG = {
    # Casdoor 服务地址
    "casdoor_endpoint": os.environ.get("CASDOOR_URL", "https://portal.msuncloud.cn"),
    # Application 的 clientId / clientSecret（需属于 built-in 组织以获得全权限）
    "client_id": os.environ.get("CASDOOR_CLIENT_ID", ""),
    "client_secret": os.environ.get("CASDOOR_CLIENT_SECRET", ""),
    # 目标组织
    "organization": os.environ.get("CASDOOR_ORG", "zymsunsoft"),
    # 迁移后用户的默认密码（所有从 AD 迁移的用户将使用此密码登录）
    "default_password": os.environ.get("MIGRATE_DEFAULT_PASSWORD", "ZymSunsoft@2026"),
    # 是否删除 LDAP 配置（True=删除，False=仅停止 autoSync 保留配置）
    "delete_ldap": os.environ.get("MIGRATE_DELETE_LDAP", "true").lower() in ("1", "true", "yes"),
    # 是否强制重置密码（False=仅给没有密码的用户设密码，True=所有用户统一重置）
    "force_reset_password": False,
    # dry-run 模式：仅检查不修改
    "dry_run": os.environ.get("MIGRATE_DRY_RUN", "false").lower() in ("1", "true", "yes"),
}

# ============================================================
#  以下为脚本逻辑，一般不需要修改
# ============================================================


def log(tag, msg):
    print(f"[{tag}] {msg}")


class CasdoorClient:
    def __init__(self, config):
        self.base = config["casdoor_endpoint"].rstrip("/")
        self.auth = {
            "clientId": config["client_id"],
            "clientSecret": config["client_secret"],
        }

    def _url(self, path):
        return f"{self.base}{path}"

    def get(self, path, params=None):
        merged = {**self.auth, **(params or {})}
        r = requests.get(self._url(path), params=merged, timeout=90)
        r.raise_for_status()
        return r.json()

    def post(self, path, json_body=None, params=None):
        merged = {**self.auth, **(params or {})}
        r = requests.post(self._url(path), params=merged, json=json_body, timeout=90)
        r.raise_for_status()
        return r.json()

    def get_organization(self, name):
        resp = self.get("/api/get-organization", {"id": f"admin/{name}"})
        return resp.get("data")

    def get_users(self, owner):
        resp = self.get("/api/get-users", {"owner": owner, "pageSize": "100000", "p": "1"})
        return resp.get("data") or []

    def update_user(self, owner, name, body, columns):
        params = {"id": f"{owner}/{name}", "columns": ",".join(columns)}
        return self.post("/api/update-user", json_body=body, params=params)

    def get_groups(self, owner):
        resp = self.get("/api/get-groups", {"owner": owner})
        return resp.get("data") or []

    def update_group(self, owner, name, body):
        return self.post("/api/update-group",
                         json_body=body, params={"id": f"{owner}/{name}"})

    def get_ldaps(self, owner):
        resp = self.get("/api/get-ldaps", {"owner": owner})
        return resp.get("data") or []

    def update_ldap(self, ldap_body):
        return self.post("/api/update-ldap", json_body=ldap_body)

    def delete_ldap(self, ldap_body):
        return self.post("/api/delete-ldap", json_body=ldap_body)


def main():
    cfg = CONFIG

    if not cfg["client_id"] or not cfg["client_secret"]:
        sys.exit("[ERROR] 请设置 CASDOOR_CLIENT_ID 和 CASDOOR_CLIENT_SECRET 环境变量，"
                 "或在脚本 CONFIG 中填写 client_id / client_secret")

    org = cfg["organization"]
    cli = CasdoorClient(cfg)

    log("INIT", f"Casdoor={cfg['casdoor_endpoint']}  组织={org}  dry_run={cfg['dry_run']}")

    # ---- 预检：组织存在 ----
    org_data = cli.get_organization(org)
    if not org_data:
        sys.exit(f"[ERROR] 组织 {org} 不存在")
    log("INIT", f"组织 {org} 确认存在")

    # ================================================================
    #  阶段 1：停止 LDAP 自动同步
    # ================================================================
    log("STEP-1", "停止 LDAP 自动同步 ...")
    ldaps = cli.get_ldaps(org)
    if not ldaps:
        log("STEP-1", "该组织下没有 LDAP 配置，跳过")
    else:
        for ldap in ldaps:
            cur_auto = ldap.get("autoSync", 0)
            log("STEP-1", f"  LDAP [{ldap.get('id')}] serverName={ldap.get('serverName')} "
                          f"autoSync={cur_auto}")
            if cur_auto != 0:
                if cfg["dry_run"]:
                    log("DRY", f"  将 autoSync 置 0: {ldap.get('id')}")
                else:
                    body = {**ldap, "autoSync": 0}
                    # update-ldap 可能因 StopAutoSync 阻塞而超时
                    try:
                        r = cli.update_ldap(body)
                        log("STEP-1", f"  -> {r.get('status')}")
                    except Exception as e:
                        log("WARN", f"  update-ldap 可能超时（通常已生效）: {e}")
                    # 回读确认
                    time.sleep(3)
                    refreshed = cli.get_ldaps(org)
                    cur = next((l for l in refreshed if l["id"] == ldap["id"]), None)
                    if cur:
                        log("STEP-1", f"  回读 autoSync={cur.get('autoSync', 0)}")

    # ================================================================
    #  阶段 2：迁移用户 —— 清除 ldap 字段，设置本地密码
    # ================================================================
    log("STEP-2", "获取用户列表 ...")
    users = cli.get_users(org)
    log("STEP-2", f"共 {len(users)} 个用户")

    ldap_users = [u for u in users if u.get("ldap")]
    local_users = [u for u in users if not u.get("ldap")]
    log("STEP-2", f"  LDAP 绑定用户: {len(ldap_users)} 个")
    log("STEP-2", f"  本地用户: {len(local_users)} 个")

    migrated = skipped = failed = 0
    for u in users:
        name = u.get("name")
        if not name:
            continue

        need_clear_ldap = bool(u.get("ldap"))
        need_set_password = cfg["force_reset_password"] or not u.get("password")
        need_fix_type = u.get("type") not in ("normal-user", "")

        if not need_clear_ldap and not need_set_password and not need_fix_type:
            skipped += 1
            continue

        columns = []
        body = dict(u)

        if need_clear_ldap:
            body["ldap"] = ""
            columns.append("ldap")

        if need_set_password:
            body["password"] = cfg["default_password"]
            body["passwordType"] = "plain"
            columns.extend(["password", "passwordType"])

        if need_fix_type:
            body["type"] = "normal-user"
            columns.append("type")

        if cfg["dry_run"]:
            log("DRY", f"  用户 {name}: columns={columns}")
            migrated += 1
            continue

        r = cli.update_user(org, name, body, columns)
        if r.get("status") == "ok":
            migrated += 1
        else:
            failed += 1
            log("WARN", f"  用户 {name} 更新失败: {r.get('msg', r)}")

    log("STEP-2", f"用户迁移完成: 修改 {migrated}, 跳过 {skipped}, 失败 {failed}")

    # ================================================================
    #  阶段 3：迁移群组 —— 清除 ldap-synced / ad-synced 类型标记
    # ================================================================
    log("STEP-3", "获取群组列表 ...")
    groups = cli.get_groups(org)
    log("STEP-3", f"共 {len(groups)} 个群组")

    synced_types = {"ldap-synced", "ad-synced"}
    g_migrated = g_skipped = g_failed = 0

    for g in groups:
        name = g.get("name")
        if not name:
            continue

        cur_type = g.get("type", "")
        if cur_type not in synced_types:
            g_skipped += 1
            continue

        body = dict(g)
        body["type"] = ""

        if cfg["dry_run"]:
            log("DRY", f"  群组 {name}: type {cur_type!r} -> ''")
            g_migrated += 1
            continue

        r = cli.update_group(org, name, body)
        if r.get("status") == "ok":
            g_migrated += 1
        else:
            g_failed += 1
            log("WARN", f"  群组 {name} 更新失败: {r.get('msg', r)}")

    log("STEP-3", f"群组迁移完成: 修改 {g_migrated}, 跳过 {g_skipped}, 失败 {g_failed}")

    # ================================================================
    #  阶段 4：处理 LDAP 配置（删除或保留）
    # ================================================================
    if cfg["delete_ldap"] and ldaps:
        log("STEP-4", "删除 LDAP 配置 ...")
        for ldap in ldaps:
            lid = ldap.get("id")
            if cfg["dry_run"]:
                log("DRY", f"  将删除 LDAP 配置: {lid}")
                continue
            r = cli.delete_ldap(ldap)
            log("STEP-4", f"  删除 {lid} -> {r.get('status')} ({r.get('msg', '')})")
    else:
        log("STEP-4", "保留 LDAP 配置（已停止 autoSync）")

    # ================================================================
    #  阶段 5：回读验证
    # ================================================================
    if cfg["dry_run"]:
        log("VERIFY", "dry-run 模式，跳过回读验证")
        log("DONE", "预检完成，去掉 dry_run 后执行实际迁移")
        return

    log("VERIFY", "回读验证 ...")
    users_after = cli.get_users(org)
    groups_after = cli.get_groups(org)

    # 用户验证
    still_ldap = [u for u in users_after if u.get("ldap")]
    no_password = [u for u in users_after if not u.get("password")]
    log("VERIFY", f"用户总数: {len(users_after)}")
    log("VERIFY", f"  仍绑定 LDAP: {len(still_ldap)}" +
        (f" -> {[u['name'] for u in still_ldap[:5]]}" if still_ldap else ""))
    log("VERIFY", f"  无密码: {len(no_password)}")

    # 群组验证
    still_synced = [g for g in groups_after if g.get("type") in synced_types]
    log("VERIFY", f"群组总数: {len(groups_after)}")
    log("VERIFY", f"  仍标记为 synced: {len(still_synced)}")

    # 群组层级验证
    by_name = {g["name"]: g for g in groups_after}
    self_ref = [g["name"] for g in groups_after if g.get("parentId") == g["name"]]

    def depth_ok(g):
        p, n = g.get("parentId"), 0
        while p and n < 64:
            pg = by_name.get(p)
            if not pg:
                return True
            p, n = pg.get("parentId"), n + 1
        return n < 64

    cycles = [g["name"] for g in groups_after if not depth_ok(g)]
    log("VERIFY", f"  自引用: {len(self_ref)}  环: {len(cycles)}")

    # LDAP 配置验证
    ldaps_after = cli.get_ldaps(org)
    log("VERIFY", f"LDAP 配置: {len(ldaps_after)} 个" +
        (f" (ids={[l['id'] for l in ldaps_after]})" if ldaps_after else ""))

    ok = len(still_ldap) == 0 and len(still_synced) == 0 and not self_ref and not cycles
    log("VERIFY", "通过 ✔" if ok else "部分未通过 ✘")

    if not ok:
        if still_ldap:
            log("WARN", f"以下用户仍绑定 LDAP: {[u['name'] for u in still_ldap[:10]]}")
        if still_synced:
            log("WARN", f"以下群组仍标记 synced: {[g['name'] for g in still_synced[:10]]}")
        if self_ref:
            log("WARN", f"自引用群组: {self_ref[:10]}")
        if cycles:
            log("WARN", f"循环依赖群组: {cycles[:10]}")
        log("WARN", "可重跑脚本修复")
        sys.exit(1)

    log("DONE", "迁移完成！zymsunsoft 组织已切换为 Casdoor 本地认证")
    log("DONE", f"用户默认密码: {cfg['default_password']}")
    log("DONE", "请通知用户使用新密码登录，并建议尽快修改密码")


if __name__ == "__main__":
    main()
