#!/usr/bin/env python3
"""生产(portal.msuncloud.cn) zymsunsoft 组织群组授权部署前检查与脏数据修复。

用途（配合群组层级鉴权修复版部署）：
  1. 只读扫描：列出绑"父群组"的 Application 权限 —— 新版部署后其子群组成员
     将自动获得授权（范围扩大项，需人工确认）
  2. 扫描并修复 roles/groups/users 中的 ID 空格脏数据（如 "org/ role"），
     --fix 时仅对目标字段做清洗，其余字段原样回写

用法：
  CASDOOR_ADMIN_PASSWORD=... python3 scripts/scan_parent_group_perms.py          # 只读扫描
  CASDOOR_ADMIN_PASSWORD=... python3 scripts/scan_parent_group_perms.py --fix    # 扫描+修复脏数据
"""
import sys, os
import requests

CASDOOR = os.environ.get("CASDOOR_URL", "https://portal.msuncloud.cn")
ORG = os.environ.get("CASDOOR_ORG", "zymsunsoft")
PASSWORD = os.environ.get("CASDOOR_ADMIN_PASSWORD", "")
FIX = "--fix" in sys.argv

if not PASSWORD:
    sys.exit("请先设置环境变量 CASDOOR_ADMIN_PASSWORD")

s = requests.Session()

def api(method, path, **kw):
    for i in range(4):
        try:
            r = s.request(method, f"{CASDOOR}{path}", timeout=60, **kw)
            if r.status_code >= 500:
                raise RuntimeError(f"HTTP {r.status_code} {r.text[:80]!r}")
            return r.json()
        except Exception as e:
            print(f"[retry] {method} {path} 第{i + 1}次失败: {e}")
            if i == 3:
                sys.exit(f"{method} {path} 连续失败")
    return None

j = api("POST", "/api/login", json={"type": "login", "username": "admin", "password": PASSWORD,
                                    "application": "app-built-in", "organization": "built-in"})
if j.get("status") != "ok":
    sys.exit(f"登录失败: {j}")
print("登录 OK")

# 群组层级：parentName -> [子群组名]
groups = api("GET", "/api/get-groups", params={"owner": ORG})["data"] or []
children_of = {}
for g in groups:
    if not g.get("isTopGroup") and g.get("parentId"):
        children_of.setdefault(g["parentId"], []).append(g["name"])

def descendants(name):
    out, stack = [], [name]
    while stack:
        for ch in children_of.get(stack.pop(), []):
            out.append(ch)
            stack.append(ch)
    return out

print(f"\n群组总数 {len(groups)}，父群组 {len(children_of)} 个")

perms = api("GET", "/api/get-permissions", params={"owner": ORG})["data"] or []
app_perms = [p for p in perms if p.get("resourceType") == "Application"]
print(f"权限总数 {len(perms)}，Application 类 {len(app_perms)}\n")

print("== 一、绑父群组的 Application 权限（新版部署后子群组成员将获得授权） ==")
hit = False
for p in app_perms:
    for gid in p.get("groups") or []:
        gname = gid.split("/", 1)[-1] if "/" in gid else gid
        desc = descendants(gname)
        if desc:
            hit = True
            print(f"  [{p['name']}] 应用 {p.get('resources')} <- 父群组「{gname}」"
                  f" effect={p.get('effect')} state={p.get('state')} enabled={p.get('isEnabled')}")
            print(f"      子群组: {desc}")
if not hit:
    print("  （无，部署新版不会扩大任何授权范围）")

print("\n== 二、roles/groups/users ID 空格脏数据 ==")
dirty = []
for p in perms:
    for field in ("roles", "groups", "users"):
        raw = p.get(field) or []
        cleaned = [x.replace("/ ", "/").strip() for x in raw]
        if raw != cleaned:
            dirty.append((p, field, raw, cleaned))
            print(f"  [{p['name']}] {field}: {raw} -> {cleaned}")
if not dirty:
    print("  （无）")

if not FIX:
    print("\n只读扫描完成。加 --fix 修复脏数据。")
    sys.exit(0)

print("\n== 三、修复脏数据（仅清洗目标字段，其余原样回写） ==")
for p, field, raw, cleaned in dirty:
    body = dict(p)
    body[field] = cleaned
    res = api("POST", "/api/update-permission", params={"id": f"{p['owner']}/{p['name']}"}, json=body)
    print(f"  [{p['name']}] {field}: {res.get('status')}/{res.get('data')}")

print("\n== 四、回读验证 ==")
perms2 = api("GET", "/api/get-permissions", params={"owner": ORG})["data"] or []
bad = 0
for p in perms2:
    for field in ("roles", "groups", "users"):
        ids = p.get(field) or []
        if any(x != x.replace("/ ", "/").strip() for x in ids):
            bad += 1
            print(f"  仍存在: [{p['name']}] {field}: {ids}")
print("  通过：无脏数据" if bad == 0 else f"  失败：{bad} 处未修复")
sys.exit(1 if bad else 0)
