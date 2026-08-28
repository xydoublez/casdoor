#!/usr/bin/env python3
"""
AD 树形组织架构 + 用户 同步到 Casdoor 的运维脚本。

两种数据源模式（自动选择）：
  1. 直接 LDAP 模式（推荐）：脚本自己用 ldap3 连接 AD，读取 OU 树 + 用户 + memberOf，
     然后调用 Casdoor API 创建群组树并同步用户。
     优点：树形层级精确、可立即执行、不依赖 autoSync 定时器。
  2. Casdoor LDAP 模式：复用 Casdoor 已配置的 LDAP 连接，通过 /api/get-ldap-users
     拉取用户及 memberOf DN 链，脚本据此推导 OU 树并创建群组。
     优点：不需要在脚本里填 AD 凭据；缺点：树推导依赖 memberOf 覆盖度。

用法：
  1. 修改下方 CONFIG 配置块（或设置同名环境变量）
  2. pip install requests ldap3
  3. python3 sync_ad_tree.py

认证：
  使用 Casdoor 管理员账号（ADMIN / PASSWORD）通过 /api/login 获取 session，
  后续所有 API 调用复用该 session。
"""

from __future__ import annotations

import json
import os
import sys
import time
import requests

try:
    from ldap3 import ALL, Connection, NTLM, Server
    HAS_LDAP3 = True
except ImportError:
    HAS_LDAP3 = False

# ============================================================
#  配置区 —— 请根据实际环境修改（也可通过同名环境变量覆盖）
# ============================================================

CASDOOR = os.environ.get("CASDOOR_URL", "https://portal.msuncloud.cn")
ORG = os.environ.get("CASDOOR_ORG", "zymsunsoft")
ADMIN = os.environ.get("CASDOOR_ADMIN", "built-in/admin")
PASSWORD = os.environ.get("CASDOOR_ADMIN_PASSWORD", "")
LOGIN_APP = os.environ.get("CASDOOR_LOGIN_APP", "app-built-in")

# ---- AD 直连模式（可选，优先使用） ----
# 留空则自动回退到 Casdoor LDAP 模式（需要 Casdoor 里已配好 LDAP）
AD_HOST = os.environ.get("AD_HOST", "ad.msunsoft.com")
AD_PORT = int(os.environ.get("AD_PORT", "636"))
AD_USE_SSL = os.environ.get("AD_USE_SSL", "true").lower() in ("1", "true", "yes")
AD_BIND_DN = os.environ.get("AD_BIND_DN", "")
AD_BIND_PASSWORD = os.environ.get("AD_BIND_PASSWORD", "")
AD_BASE_DN = os.environ.get("AD_BASE_DN", "")
# 用户过滤，留空使用 (&(objectClass=user)(objectCategory=person))
AD_USER_FILTER = os.environ.get("AD_USER_FILTER", "")

# ---- 同步策略 ----
# 同步后是否启用 autoSync 持续增量同步用户（分钟，0=不启用）
RESTORE_AUTOSYNC = int(os.environ.get("CASDOOR_AUTOSYNC_MIN", "5"))
# 群组类型标记（便于区分来源）
GROUP_TYPE = "ad-synced"
# 用户默认密码（AD 用户实际走 LDAP 认证，此密码仅占位，满足 Casdoor 必填）
DEFAULT_PASSWORD = os.environ.get("CASDOOR_DEFAULT_PASSWORD", "AdSync@2026")

# ============================================================
#  以下为脚本逻辑，一般不需要修改
# ============================================================

def log(tag, msg):
    print(f"[{tag}] {msg}")


# ---------- Casdoor API 客户端 ----------

class CasdoorClient:
    def __init__(self, base_url, session):
        self.base = base_url.rstrip("/")
        self.s = session

    def _url(self, path):
        return f"{self.base}{path}"

    def _req(self, method, path, **kw):
        kw.setdefault("timeout", 90)
        r = self.s.request(method, self._url(path), **kw)
        if r.status_code >= 500:
            raise RuntimeError(f"HTTP {r.status_code}: {r.text[:200]}")
        try:
            return r.json()
        except ValueError:
            raise RuntimeError(f"非 JSON 响应: {r.text[:200]}")

    def get(self, path, params=None):
        return self._req("GET", path, params=params)

    def post(self, path, json_body=None, params=None):
        return self._req("POST", path, json=json_body, params=params)

    # ---- 认证 ----
    def login(self, admin, password, organization, app):
        org, name = admin.split("/", 1)
        resp = self.post("/api/login", json={
            "type": "login",
            "username": name,
            "password": password,
            "application": app,
            "organization": org,
        })
        if resp.get("status") != "ok":
            raise RuntimeError(f"登录失败: {resp}")
        log("AUTH", f"已登录 {admin} -> session 已建立")

    def get_account(self):
        return self.get("/api/get-account")

    # ---- 用户 ----
    def get_users(self, owner):
        return self.get("/api/get-users", params={"owner": owner, "pageSize": "100000", "p": "1"})["data"]

    def get_user(self, owner, name):
        return self.get("/api/get-user", params={"id": f"{owner}/{name}"})["data"]

    def add_user(self, user):
        return self.post("/api/add-user", json_body=user)

    def update_user(self, owner, name, user, columns=None):
        params = {"id": f"{owner}/{name}"}
        if columns:
            params["columns"] = ",".join(columns)
        return self.post("/api/update-user", json_body=user, params=params)

    # ---- 群组 ----
    def get_groups(self, owner):
        return self.get("/api/get-groups", params={"owner": owner})["data"]

    def add_group(self, group):
        return self.post("/api/add-group", json_body=group)

    def update_group(self, owner, name, group):
        return self.post("/api/update-group", params={"id": f"{owner}/{name}"}, json_body=group)

    def delete_group(self, group):
        return self.post("/api/delete-group", json_body=group)

    def delete_user(self, user):
        return self.post("/api/delete-user", json_body=user)

    # ---- LDAP ----
    def get_ldaps(self, owner):
        return self.get("/api/get-ldaps", params={"owner": owner})["data"]

    def get_ldap_users(self, owner, ldap_id):
        resp = self.get("/api/get-ldap-users", params={"id": f"{owner}/{ldap_id}"})
        if resp.get("status") == "error":
            raise RuntimeError(f"获取 LDAP 用户失败: {resp.get('msg')}")
        data = resp.get("data", {})
        if isinstance(data, dict):
            return data.get("users", [])
        return data

    def sync_ldap_users(self, owner, ldap_id, users):
        resp = self.post("/api/sync-ldap-users",
                         params={"id": f"{owner}/{ldap_id}"},
                         json_body=users)
        if resp.get("status") == "error":
            raise RuntimeError(f"同步用户失败: {resp.get('msg')}")
        d = resp.get("data", {}) or {}
        return len(d.get("exist") or []), len(d.get("failed") or [])

    def update_ldap(self, ldap):
        return self.post("/api/update-ldap", json_body=ldap)


# ---------- AD 直连读取 ----------

def connect_ad():
    if not HAS_LDAP3:
        return None
    if not (AD_HOST and AD_BIND_DN and AD_BASE_DN):
        return None

    server = Server(AD_HOST, port=AD_PORT, use_ssl=AD_USE_SSL, get_info=ALL)
    # 支持 NTLM（AD 常用 user@domain 或 DOMAIN\\user）与简单绑定
    user = AD_BIND_DN
    if "\\" in user or "@" in user:
        conn = Connection(server, user=user, password=AD_BIND_PASSWORD, authentication=NTLM, auto_bind=True)
    else:
        conn = Connection(server, user=user, password=AD_BIND_PASSWORD, auto_bind=True)
    log("LDAP", f"已连接 AD {AD_HOST}:{AD_PORT} base={AD_BASE_DN}")
    return conn


def read_ad_tree(conn):
    """读取 AD OU 树 + 用户（含 memberOf）。返回 (ous, users)"""
    # 1) OU
    ous = []
    conn.search(AD_BASE_DN, "(objectClass=organizationalUnit)",
                attributes=["ou", "description"])
    for entry in conn.entries:
        ous.append({
            "dn": entry.entry_dn,
            "ou": str(entry.ou) if entry.ou else "",
            "description": str(entry.description) if entry.description else "",
        })
    log("LDAP", f"读取到 {len(ous)} 个 OU")

    # 2) 用户
    user_filter = AD_USER_FILTER or "(&(objectClass=user)(objectCategory=person))"
    users = []
    conn.search(AD_BASE_DN, user_filter,
                attributes=["sAMAccountName", "displayName", "mail", "mobile",
                            "telephoneNumber", "objectGUID", "memberOf",
                            "title", "department", "company"])
    for entry in conn.entries:
        def attr(name):
            v = getattr(entry, name, None)
            if v is None:
                return ""
            if hasattr(v, "values"):
                return v.values[0] if v.values else ""
            return str(v)

        member_of = []
        mo = getattr(entry, "memberOf", None)
        if mo and hasattr(mo, "values"):
            member_of = [str(v) for v in mo.values]

        users.append({
            "dn": entry.entry_dn,
            "sAMAccountName": attr("sAMAccountName"),
            "displayName": attr("displayName"),
            "mail": attr("mail"),
            "mobile": attr("mobile"),
            "telephoneNumber": attr("telephoneNumber"),
            "objectGUID": attr("objectGUID"),
            "memberOf": member_of,
            "title": attr("title"),
            "department": attr("department"),
            "company": attr("company"),
        })
    log("LDAP", f"读取到 {len(users)} 个用户")
    return ous, users


# ---------- 树结构推导 ----------

def dn_to_name(dn):
    """取 DN 第一个非 DC 的 RDN 值（与 Go 端 dnToGroupName 一致）"""
    if not dn:
        return ""
    for part in dn.split(","):
        part = part.strip()
        if part.lower().startswith("dc="):
            continue
        if "=" in part:
            return part.split("=", 1)[1].replace("/", "_")
    return ""


def dn_parent(dn):
    """返回父 DN"""
    parts = dn.split(",")
    return ",".join(parts[1:]) if len(parts) > 1 else ""


def build_ou_tree(ous, base_dn):
    """把 OU 列表组织为 {dn: {name, parent_name, description}}"""
    dn_map = {}
    for ou in ous:
        dn_map[ou["dn"]] = ou
    tree = {}
    for ou in ous:
        name = ou["ou"]
        parent_dn = dn_parent(ou["dn"])
        if not parent_dn or parent_dn.lower() == base_dn.lower():
            parent_name = ""
        else:
            p = dn_map.get(parent_dn)
            parent_name = p["ou"] if p else dn_to_name(parent_dn)
        tree[name] = {
            "name": name,
            "parent_name": parent_name,
            "description": ou["description"],
            "dn": ou["dn"],
        }
    return tree


def user_to_group(user_dn, base_dn):
    """根据用户 DN 推导其直接所属的 OU 群组名（第一个非 DC 的 RDN 的父 RDN）"""
    # 例：CN=alice,OU=人力资源部,OU=众阳健康集团,DC=... -> 人力资源部
    parts = [p.strip() for p in user_dn.split(",") if not p.strip().lower().startswith("dc=")]
    if len(parts) < 2:
        return ""
    # 取倒数第二个（即用户的直接父 OU）
    return parts[-2].split("=", 1)[1].replace("/", "_") if "=" in parts[-2] else ""


# ---------- 同步逻辑 ----------

def sync_groups_from_ou_tree(cli, owner, ou_tree, existing_groups):
    """按 OU 树创建/更新群组（父先子后）"""
    by_name = {g["name"]: g for g in existing_groups}
    created = updated = 0

    # 拓扑排序：父先子后
    ordered = []
    visited = set()

    def visit(name):
        if name in visited or name not in ou_tree:
            return
        visited.add(name)
        p = ou_tree[name]["parent_name"]
        if p and p in ou_tree:
            visit(p)
        ordered.append(name)

    for n in ou_tree:
        visit(n)

    for name in ordered:
        info = ou_tree[name]
        parent = info["parent_name"]
        is_top = not parent
        payload = {
            "owner": owner,
            "name": name,
            "displayName": info["description"] or name,
            "parentId": parent,
            "isTopGroup": is_top,
            "type": GROUP_TYPE,
            "isEnabled": True,
        }
        if name in by_name:
            cur = by_name[name]
            if (cur.get("parentId"), cur.get("isTopGroup"), cur.get("type")) != (parent, is_top, GROUP_TYPE):
                r = cli.update_group(owner, name, {**cur, **payload})
                if r.get("status") == "ok":
                    updated += 1
                    cur.update(payload)
            else:
                pass  # 无需更新
        else:
            r = cli.add_group(payload)
            if r.get("status") == "ok":
                created += 1
                by_name[name] = payload
            else:
                log("WARN", f"创建群组 {name} 失败: {r}")

    log("SYNC", f"群组: 新建 {created}, 更新 {updated}, 总计 {len(by_name)}")
    return by_name


def sync_groups_from_member_of(cli, owner, ldap_users, existing_groups):
    """从 memberOf DN 链推导群组并补建缺失的（Casdoor LDAP 模式兜底）"""
    ou_parent, cn_parent, depth = {}, {}, {}

    def rdn_name(comp):
        return comp.split("=", 1)[1].replace("/", "_")

    for u in ldap_users or []:
        for dn in u.get("memberOf") or []:
            comps = [c.strip() for c in dn.split(",")
                     if not c.strip().lower().startswith("dc=") and "=" in c]
            for i, comp in enumerate(comps):
                key = comp.split("=", 1)[0].upper()
                name = rdn_name(comp)
                if key == "OU":
                    parent = rdn_name(comps[i + 1]) if i + 1 < len(comps) else ""
                    ou_parent.setdefault(name, parent)
                    depth.setdefault(name, len(comps) - i)
                elif i == 0:
                    parent = rdn_name(comps[1]) if len(comps) > 1 else ""
                    if name == parent:
                        parent = ou_parent.get(parent, "")
                    cn_parent.setdefault(name, parent)
                    depth.setdefault(name, len(comps))

    by_name = {g["name"]: g for g in existing_groups}
    todo = {n: (p, depth[n]) for n, p in {**ou_parent, **cn_parent}.items() if n not in by_name}
    created = 0
    for name, (parent, _) in sorted(todo.items(), key=lambda kv: kv[1][1]):
        if parent and parent not in by_name:
            parent = ""
        r = cli.add_group({
            "owner": owner, "name": name, "displayName": name,
            "parentId": parent, "isTopGroup": not parent,
            "type": GROUP_TYPE, "isEnabled": True,
        })
        if r.get("status") == "ok":
            by_name[name] = {"name": name, "parentId": parent}
            created += 1
    log("SYNC", f"从 memberOf 推导并补建群组 {created} 个，总计 {len(by_name)}")
    return by_name, ou_parent, cn_parent


def sync_users(cli, owner, ad_users, group_by_name, ou_parent=None, cn_parent=None):
    """同步用户：已存在则更新 groups/ldap 字段，否则新增"""
    existing = {u["name"]: u for u in cli.get_users(owner)}
    added = updated = failed = 0

    for u in ad_users:
        name = u.get("sAMAccountName") or u.get("displayName") or u.get("dn").split("=", 1)[1]
        if not name:
            continue

        # 计算该用户应归属的群组
        groups = []
        direct_ou = user_to_group(u.get("dn", ""))
        if direct_ou and direct_ou in group_by_name:
            groups.append(f"{owner}/{direct_ou}")
        for mo in u.get("memberOf") or []:
            gn = dn_to_name(mo)
            if gn and gn in group_by_name:
                gid = f"{owner}/{gn}"
                if gid not in groups:
                    groups.append(gid)

        ldap_uuid = u.get("objectGUID") or u.get("dn")

        if name in existing:
            cur = existing[name]
            # 更新 groups + ldap 绑定
            cur["groups"] = groups
            cur["ldap"] = ldap_uuid
            if u.get("displayName") and cur.get("displayName") != u["displayName"]:
                cur["displayName"] = u["displayName"]
            if u.get("mail") and cur.get("email") != u["mail"]:
                cur["email"] = u["mail"]
            if u.get("mobile") and cur.get("phone") != u["mobile"]:
                cur["phone"] = u["mobile"]
            cols = ["groups", "ldap", "displayName", "email", "phone"]
            r = cli.update_user(owner, name, cur, columns=cols)
            if r.get("status") == "ok":
                updated += 1
            else:
                failed += 1
                log("WARN", f"更新用户 {name} 失败: {r}")
        else:
            new_user = {
                "owner": owner,
                "name": name,
                "displayName": u.get("displayName") or name,
                "email": u.get("mail") or "",
                "phone": u.get("mobile") or u.get("telephoneNumber") or "",
                "type": "normal-user",
                "password": DEFAULT_PASSWORD,
                "ldap": ldap_uuid,
                "groups": groups,
                "tag": "ad-synced",
            }
            r = cli.add_user(new_user)
            if r.get("status") == "ok":
                added += 1
            else:
                failed += 1
                log("WARN", f"新增用户 {name} 失败: {r}")

    log("SYNC", f"用户: 新增 {added}, 更新 {updated}, 失败 {failed}")


def fix_group_hierarchy(cli, owner, groups, ou_parent, cn_parent):
    """修正群组层级：memberOf 推导优先，消除自引用/环"""
    by_name = {g["name"]: g for g in groups}
    root = next((n for n, p in ou_parent.items() if p == "" and n in by_name), None)
    log("FIX", f"根群组: {root!r}")

    def target_parent(g):
        name = g["name"]
        if name == root:
            return "", True
        p = ou_parent.get(name, cn_parent.get(name))
        if p is None:
            cur = g.get("parentId")
            if cur and cur != name and cur in by_name:
                return cur, False
            p = root or ""
        if p and p not in by_name:
            p = root or ""
        return p, p == ""

    target = {g["name"]: target_parent(g) for g in groups}

    def has_cycle(start):
        seen, cur = {start}, start
        for _ in range(64):
            p = target.get(cur, ("",))[0]
            if not p or p not in by_name:
                return False
            if p in seen:
                return True
            seen.add(p)
            cur = p
        return True

    for _ in range(5):
        bad = [n for n in target if has_cycle(n)]
        if not bad:
            break
        log("FIX", f"环上群组 {len(bad)} 个，改挂根: {bad[:6]}")
        for n in bad:
            target[n] = ("", True) if n == root else (root or "", not root)

    fixed = 0
    for g in groups:
        p, top = target[g["name"]]
        if (g.get("parentId"), bool(g.get("isTopGroup"))) != (p, top):
            r = cli.update_group(owner, g["name"], {**g, "parentId": p, "isTopGroup": top})
            if r.get("status") == "ok":
                fixed += 1
    log("FIX", f"层级修正 {fixed} 个")


# ---------- 主流程 ----------

def main():
    if not PASSWORD:
        sys.exit("请先设置环境变量 CASDOOR_ADMIN_PASSWORD")

    admin_org, admin_name = ADMIN.split("/", 1)
    sess = requests.Session()
    cli = CasdoorClient(CASDOOR, sess)

    log("INIT", f"目标 Casdoor={CASDOOR} 组织={ORG} 管理员={ADMIN}")
    cli.login(ADMIN, PASSWORD, admin_org, LOGIN_APP)

    # 验证登录
    acct = cli.get_account()
    if acct.get("status") != "ok":
        sys.exit(f"登录验证失败: {acct}")
    log("AUTH", f"当前账号: {acct['data'].get('name')} (isAdmin={acct['data'].get('isAdmin')})")

    # ---- 数据源选择 ----
    conn = connect_ad()
    if conn:
        log("MODE", "使用 AD 直连模式")
        ous, ad_users = read_ad_tree(conn)
        ou_tree = build_ou_tree(ous, AD_BASE_DN)
        conn.unbind()

        existing_groups = cli.get_groups(ORG)
        group_by_name = sync_groups_from_ou_tree(cli, ORG, ou_tree, existing_groups)
        sync_users(cli, ORG, ad_users, group_by_name)
    else:
        log("MODE", "使用 Casdoor LDAP 模式（复用已配置的 LDAP 连接）")
        ldaps = cli.get_ldaps(ORG)
        if not ldaps:
            sys.exit(f"组织 {ORG} 下没有 LDAP 配置，且未提供 AD 直连凭据")

        all_users = []
        for ldap in ldaps:
            users = cli.get_ldap_users(ORG, ldap["id"])
            all_users.extend(users)
            log("LDAP", f"{ldap['id']}: {len(users)} 个用户")

        existing_groups = cli.get_groups(ORG)
        group_by_name, ou_parent, cn_parent = sync_groups_from_member_of(
            cli, ORG, all_users, existing_groups)

        # 用 sync-ldap-users 同步用户（服务端会把用户挂到 memberOf 对应的群组）
        for ldap in ldaps:
            exist, failed = cli.sync_ldap_users(ORG, ldap["id"], all_users)
            log("SYNC", f"{ldap['id']}: exist={exist} failed={failed}")

        # 修正层级
        fix_group_hierarchy(cli, ORG, cli.get_groups(ORG), ou_parent, cn_parent)

    # ---- 验证 ----
    log("VERIFY", "回读验证...")
    users = cli.get_users(ORG)
    groups = cli.get_groups(ORG)
    by_name = {g["name"]: g for g in groups}

    self_ref = [g["name"] for g in groups if g.get("parentId") == g["name"]]

    def depth_ok(g):
        p, n = g.get("parentId"), 0
        while p and n < 64:
            pg = by_name.get(p)
            if not pg:
                return True
            p, n = pg.get("parentId"), n + 1
        return n < 64

    cycles = [g["name"] for g in groups if not depth_ok(g)]
    tops = [g["name"] for g in groups if g.get("isTopGroup")]
    with_groups = [u for u in users if u.get("groups")]

    log("VERIFY", f"群组 {len(groups)} 个（顶层 {len(tops)}：{tops[:6]}）")
    log("VERIFY", f"用户 {len(users)} 个，已挂组 {len(with_groups)} 个")
    for g in sorted(tops)[:3]:
        kids = [x["name"] for x in groups if x.get("parentId") == g]
        log("TREE", f"[{g}] -> {kids[:6]}{' ...' if len(kids) > 6 else ''}")

    ok = (bool(groups) and bool(users)
          and not self_ref and not cycles
          and len(with_groups) >= int(len(users) * 0.8))

    log("VERIFY", ("通过 ✔" if ok else "未通过 ")
        + f"（群组 {len(groups)}，挂组 {len(with_groups)}/{len(users)}）")

    if not ok:
        log("WARN", "部分验证未通过，请检查上方输出（可重跑脚本）")
        sys.exit(1)

    log("DONE", "AD 树形组织 + 用户同步完成")


if __name__ == "__main__":
    main()
