#!/usr/bin/env python3
# 基于 sfx.py 的风格创建：配置「基于角色访问应用」（Role -> Application）。
# 纯脚本方案（不改动、不依赖重新部署远程 Go 代码），编排远程 Casdoor 已有能力：
#
# 关键事实（来自本地源码核对 object/check.go CheckLoginPermission）：
# 1. 应用访问权限的生效条件：permission.IsEnabled == true、State == "Approved"、
#    ResourceType == "Application"、Resources 命中应用名（支持 "*"）。
# 2. 命中主体为 Users/Groups/Roles 任一；登录检查的动作是 "Read"
#    （enforcer.Enforce(主体, 应用名, "Read")），因此 Actions 至少要含 "Read"。
# 3. Deny 优先（Effect=Deny 命中即拒绝）；且只要该组织存在 Allow 型 Application
#    权限，未命中任何权限的用户默认被拒绝（Deny-by-default）——
#    即「只有拥有角色的用户才能访问应用」。built-in 组织用户与 IsAdmin 用户
#    始终放行，不受影响。
# 4. 角色成员（Role.Users）以 owner/name 存储；内置模型
#    built-in/user-model-built-in 含 [role_definition]，permission.Roles 直接生效。
# 5. 服务端 wrapActionResponse（controllers/base.go）对 affected=false 也返回
#    status="ok"，仅 data 字段为 "Unaffected"（如删除不存在的对象、update 内容
#    相同）—— status 不可信！所有写操作必须暴露 data 字段，且最终以回读为准。
# 6. permission.model 存全 id（GetModel 会拆分 owner/name），但 permission.adapter
#    存【纯 name】：setEnforcerAdapter 用 getAdapter(p.Owner, p.Adapter) 按 name
#    原样查表，存全 id 会查不到而静默落回默认 permission_rule 表。
# 7. 模型 token 数双轨制（实测结论）：checkPermissionModel 要求 Application 权限的
#    model p 定义恰为 3 元素；而 enforcer 路径（NewModelFromString）不补垫，
#    3 元素 p 解析不了 6 列 policy（p.eft 缺失 -> deny）。因此权限关联 3 元素模型，
#    enforcer 关联 6 元素模型（p = sub, obj, act, eft, "", permissionId）。
#
# 流程（幂等，可重复执行；每次以脚本配置为准覆盖角色成员与权限字段）：
#   A. 登录 + 前置校验（应用存在性、用户存在性、规则命名合法性）
#   B. upsert 角色（users/嵌套 roles 以配置为准，isEnabled=true）
#      并幂等确保 Casbin 三件套：model（含 role_definition）/ adapter（同库独立
#      policy 表）/ enforcer（model+adapter 组合）——权限关联自建 model+adapter，
#      不依赖默认为空的内置配置
#   C. upsert 应用访问权限（roles=[角色]、resources=[应用]、State=Approved）
#   D. 回读验证授权链（角色 -> 权限 -> 应用）+ enforcer 实测（role 主体 Read 命中）
#   --delete：删除本脚本维护的权限与角色（先权限后角色，回读确认）；
#             三件套属基础设施，--delete 不清理
#
# 规则可选字段（对接已有生产对象时使用）：
#   "manage_role": False  -> 角色已存在，不创建/更新/删除，仅引用（不动其成员）
#   "permission": "名称"  -> 不新建权限，把角色【合并】进该已有权限的包含角色
#                           （保留其原 users/groups/resources 等字段）；
#                           --delete 时仅把角色从其中移除，不删权限本体
import sys, os, re, time, argparse, requests
from datetime import datetime

CASDOOR   = "https://portal.msuncloud.cn"
ORG       = "zymsunsoft"          # 目标组织名
ADMIN     = "built-in/admin"      # 管理员（组织管理员则填 org/admin 形式）
PASSWORD  = os.environ.get("CASDOOR_ADMIN_PASSWORD", "")
if not PASSWORD:
    sys.exit("请先设置环境变量 CASDOOR_ADMIN_PASSWORD")
LOGIN_APP = "app-built-in"        # built-in 组织的登录应用；组织管理员需改为该组织应用

PERM_PREFIX = "perm-"                          # 本脚本维护的权限名前缀（perm-<角色名>）

# Casbin 三件套（脚本幂等创建，权限关联自建 model/adapter，enforcer 供运行时 enforce）
MODEL_NAME    = "model-app-access"          # 3 元素 p：供 permission 关联（checkPermissionModel 要求）
ENFORCER_MODEL_NAME = "model-app-access-enforcer"  # 6 元素 p：供 enforcer 关联（enforce 实测可用）
ADAPTER_NAME  = "adapter-app-access"
ADAPTER_TABLE = "casbin_app_access_rule"       # 同库独立 policy 表
ENFORCER_NAME = "enforcer-app-access"
# 与内置 user-model-built-in 同构：p 三元素（checkPermissionModel 要求）、含 role_definition
MODEL_TEXT = """[request_definition]
r = sub, obj, act

[policy_definition]
p = sub, obj, act

[role_definition]
g = _, _

[policy_effect]
e = some(where (p.eft == allow))

[matchers]
m = g(r.sub, p.sub) && r.obj == p.obj && r.act == p.act"""
# 与 GetBuiltInModel 默认模型同构：p 六元素，p.eft 可解析，enforcer 路径 enforce 才为 True
ENFORCER_MODEL_TEXT = """[request_definition]
r = sub, obj, act

[policy_definition]
p = sub, obj, act, eft, "", permissionId

[role_definition]
g = _, _

[policy_effect]
e = some(where (p.eft == allow))

[matchers]
m = g(r.sub, p.sub) && r.obj == p.obj && r.act == p.act"""

# 规则以脚本为准（幂等）：重复运行会把角色成员与权限字段覆盖为下述配置。
# users 填 ORG 内用户名（不支持通配，不存在的用户会被警告并跳过）；
# apps 填应用名（ORG 下的应用）；actions 登录检查用 "Read"，
# 需要管理操作可加 "Write"/"Admin"；effect 为 "Allow" 或 "Deny"。
ROLE_APP_RULES = [
    {
        # 生产配置：把既有角色 role_ops 并入既有权限 permission_ops-platform 的
        # 包含角色（保留原 users/groups），并关联自建 model/adapter；
        # manage_role=False：角色成员不归本脚本管，仅引用。
        "role": "role_ops",
        "manage_role": False,
        "permission": "permission_ops-platform",
        "apps": ["ops_platform"],           # 授权应用名列表
        "actions": ["Read"],                # 登录检查用 Read
    },
]

RETRIES = 4  # 普通瞬时故障重试次数

s = requests.Session()

def api(method, path, **kw):
    """带重试的普通 API 调用：瞬时故障（网关超时空响应、5xx）自动重试"""
    last = None
    for i in range(RETRIES):
        try:
            r = s.request(method, f"{CASDOOR}{path}", timeout=90, **kw)
            if r.status_code >= 500:
                raise RuntimeError(f"HTTP {r.status_code} {r.text[:80]!r}")
            return r.json()
        except Exception as e:
            last = e
            print(f"[retry] {method} {path} 第 {i + 1}/{RETRIES} 次失败: {e}")
            if i + 1 < RETRIES:
                time.sleep(5 * (i + 1))
    sys.exit(f"{method} {path} 连续 {RETRIES} 次失败: {last}")

def check_write(res, what):
    """写操作校验：status 仅代表服务端无报错；affected=0 时服务端仍返回 ok
    （data="Unaffected"，如主键已存在/内容相同/删除不存在的对象）。
    因此 Unaffected 只告警不判死（update 幂等场景内容相同属正常），
    最终正确性一律以阶段 D / 删除后回读为准。"""
    if res.get("status") != "ok":
        sys.exit(f"{what} 失败: {res}")
    if res.get("data") == "Unaffected":
        print(f"{what} 注意: 服务端影响 0 行（data=Unaffected），以回读验证为准")
    return res

def now_ts():
    return datetime.now().astimezone().isoformat(timespec="seconds")  # RFC3339

def perm_name(rule):
    return f"{PERM_PREFIX}{rule['role']}"

def perm_id(rule):
    return f"{ORG}/{perm_name(rule)}"

def role_id(rule):
    return f"{ORG}/{rule['role']}"

# ---------------- A. 前置校验 ----------------
def fetch_org_user_names():
    us = api("GET", "/api/get-users", params={"owner": ORG})["data"]
    return {u["name"] for u in us}

def fetch_app_names():
    """owner=ORG 取本组织应用，organization=ORG 兜底取组织可见应用，合并去重"""
    apps = {}
    for params in ({"owner": ORG}, {"organization": ORG}):
        for a in api("GET", "/api/get-applications", params=params)["data"] or []:
            apps[a["name"]] = a
    return set(apps)

def phase_a_check(rules, user_names, app_names):
    """应用不存在 -> 硬失败；用户不存在 -> 警告并剔除（其余照常生效，最终验证体现）"""
    name_re = re.compile(r"^[A-Za-z0-9][A-Za-z0-9_-]*$")
    ok = True
    for rule in rules:
        if not name_re.match(rule["role"]):
            sys.exit(f"[A] 角色名非法（仅字母数字与 -/_）: {rule['role']}")
        missing_apps = [a for a in rule["apps"] if a not in app_names]
        if missing_apps:
            sys.exit(f"[A] 角色 {rule['role']} 的应用不存在: {missing_apps}（现有应用 {sorted(app_names)[:10]} ...）")
        missing_users = [u for u in rule.get("users") or [] if u not in user_names]
        if missing_users:
            ok = False
            print(f"[A] 警告: 角色 {rule['role']} 的用户不存在，将跳过: {missing_users}")
            rule["users"] = [u for u in rule.get("users") or [] if u in user_names]
    print(f"[A] 校验完成：应用 {len(app_names)} 个，组织用户 {len(user_names)} 个")
    return ok

# ---------------- B. Casbin 三件套 + upsert 角色 ----------------
def ensure_casbin_objects():
    """幂等确保 models/adapter/enforcer 存在（缺失则建；enforcer 模型引用不符则纠正）"""
    emid, aid = f"{ORG}/{ENFORCER_MODEL_NAME}", f"{ORG}/{ADAPTER_NAME}"
    models = {m["name"] for m in api("GET", "/api/get-models", params={"owner": ORG})["data"] or []}
    for name, text, disp in (
            (MODEL_NAME, MODEL_TEXT, "应用访问控制模型（3 元素 p，供 permission）"),
            (ENFORCER_MODEL_NAME, ENFORCER_MODEL_TEXT, "应用访问控制执行器模型（6 元素 p，供 enforcer）")):
        if name not in models:
            res = api("POST", "/api/add-model", json={
                "owner": ORG, "name": name, "createdTime": now_ts(),
                "displayName": disp, "modelText": text,
            })
            check_write(res, f"[B] model {name}")
            print(f"[B] model {name} -> {res.get('status')}/{res.get('data') or ''}")
        else:
            print(f"[B] model {name} 已存在，跳过")

    adapters = {a["name"] for a in api("GET", "/api/get-adapters", params={"owner": ORG})["data"] or []}
    if ADAPTER_NAME not in adapters:
        res = api("POST", "/api/add-adapter", json={
            "owner": ORG, "name": ADAPTER_NAME, "createdTime": now_ts(),
            "table": ADAPTER_TABLE, "useSameDb": True,
        })
        check_write(res, f"[B] adapter {ADAPTER_NAME}")
        print(f"[B] adapter {ADAPTER_NAME}(table={ADAPTER_TABLE}) -> {res.get('status')}/{res.get('data') or ''}")
    else:
        print(f"[B] adapter {ADAPTER_NAME} 已存在，跳过")

    enforcers = {e["name"]: e for e in api("GET", "/api/get-enforcers", params={"owner": ORG})["data"] or []}
    if ENFORCER_NAME not in enforcers:
        res = api("POST", "/api/add-enforcer", json={
            "owner": ORG, "name": ENFORCER_NAME, "createdTime": now_ts(),
            "displayName": "应用访问控制执行器", "model": emid, "adapter": aid,
        })
        check_write(res, f"[B] enforcer {ENFORCER_NAME}")
        print(f"[B] enforcer {ENFORCER_NAME} -> {res.get('status')}/{res.get('data') or ''}")
    elif enforcers[ENFORCER_NAME].get("model") != emid:
        # 纠正模型引用：enforcer 必须用 6 元素 p 模型，enforce 才为 True
        body = dict(enforcers[ENFORCER_NAME])
        body["model"], body["adapter"] = emid, aid
        res = api("POST", "/api/update-enforcer", params={"id": f"{ORG}/{ENFORCER_NAME}"}, json=body)
        check_write(res, f"[B] enforcer {ENFORCER_NAME} 模型纠正")
        print(f"[B] enforcer {ENFORCER_NAME} model -> {emid} ({res.get('status')}/{res.get('data') or ''})")
    else:
        print(f"[B] enforcer {ENFORCER_NAME} 已存在，跳过")

# ---------------- B. upsert 角色 ----------------
def upsert_role(rule, existing_role_ids):
    rid = role_id(rule)
    body = {
        "owner": ORG, "name": rule["role"], "createdTime": now_ts(),
        "displayName": rule.get("displayName") or rule["role"],
        "description": rule.get("description", ""),
        "users": [f"{ORG}/{u}" for u in rule.get("users") or []],  # Casdoor 角色成员以 owner/name 存储
        "roles": rule.get("roles") or [],
        "groups": [], "domains": [],
        "isEnabled": True,
    }
    if rid in existing_role_ids:
        return api("POST", "/api/update-role", params={"id": rid}, json=body)
    return api("POST", "/api/add-role", json=body)

# ---------------- C. upsert 应用访问权限 ----------------
def upsert_permission(rule, existing_perm_ids):
    target = rule.get("permission") or perm_name(rule)
    pid = f"{ORG}/{target}"
    cur = None
    if rule.get("permission"):
        # 合并模式：读已有权限全量字段，仅补 roles/resources/actions 与启用状态，
        # 保留原 users/groups/effect/model 等，避免破坏生产配置
        cur = api("GET", "/api/get-permission", params={"id": pid}).get("data")
        if not cur:
            sys.exit(f"[C] 待合并的权限不存在: {pid}")
    if cur:
        body = dict(cur)
        body["roles"] = sorted(set(cur.get("roles") or []) | {role_id(rule)})
        body["resources"] = sorted(set(cur.get("resources") or []) | set(rule["apps"]))
        body["actions"] = sorted(set(cur.get("actions") or []) | set(rule.get("actions") or ["Read"]))
        body["isEnabled"] = True
        body["state"] = "Approved"   # 必需：非 Approved 的权限不参与登录判定
        body["model"] = f"{ORG}/{MODEL_NAME}"      # 关联自建 model（全 id，GetModel 拆分）
        body["adapter"] = ADAPTER_NAME             # 关联自建 adapter（纯 name！getAdapter 不拆分）
    else:
        body = {
            "owner": ORG, "name": target, "createdTime": now_ts(),
            "displayName": f"{rule['role']} -> {'/'.join(rule['apps'])}",
            "description": rule.get("description", ""),
            "users": [], "groups": [], "domains": [],   # 授权完全走角色
            "roles": [role_id(rule)],
            "model": f"{ORG}/{MODEL_NAME}", "adapter": ADAPTER_NAME,
            "resourceType": "Application",              # 登录判定只认 Application 类型
            "resources": sorted(set(rule["apps"])),     # 命中应用名（不带 owner 前缀）
            "actions": rule.get("actions") or ["Read"], # 登录检查用 Read
            "effect": rule.get("effect", "Allow"),
            "isEnabled": True,
            "submitter": ADMIN, "approver": ADMIN, "approveTime": now_ts(),
            "state": "Approved",                        # 必需：非 Approved 的权限不参与登录判定
        }
    if pid in existing_perm_ids:
        return api("POST", "/api/update-permission", params={"id": pid}, json=body)
    return api("POST", "/api/add-permission", json=body)

# ---------------- D. 回读验证 ----------------
def perm_target(rule):
    """权限目标名：合并模式用 rule["permission"]，新建模式用 perm-<角色名>"""
    return rule.get("permission") or perm_name(rule)

def phase_d_verify(rules):
    roles = {r["name"]: r for r in api("GET", "/api/get-roles", params={"owner": ORG})["data"]}
    perms = {p["name"]: p for p in api("GET", "/api/get-permissions", params={"owner": ORG})["data"]}
    ok = True
    for rule in rules:
        rid, target = role_id(rule), perm_target(rule)
        role, perm = roles.get(rule["role"]), perms.get(target)
        if not role:
            print(f"[D] {rid} ✘ 角色不存在")
            ok = False
            continue
        if not role.get("isEnabled"):
            print(f"[D] {rid} ✘ 角色未启用")
            ok = False
        if rule.get("manage_role", True):  # 引用既有角色时不校验成员（不归脚本管）
            members = set(role.get("users") or [])
            missing = [u for u in rule.get("users") or [] if f"{ORG}/{u}" not in members]
            if missing:
                print(f"[D] {rid} ✘ 角色缺少成员: {missing}")
                ok = False

        if not perm:
            print(f"[D] {ORG}/{target} ✘ 权限不存在")
            ok = False
            continue
        bad = []
        if not perm.get("isEnabled"):
            bad.append("isEnabled=false")
        if perm.get("state") != "Approved":
            bad.append(f"state={perm.get('state')!r}")
        if perm.get("resourceType") != "Application":
            bad.append(f"resourceType={perm.get('resourceType')!r}")
        if not rule.get("permission") and perm.get("effect") != rule.get("effect", "Allow"):
            bad.append(f"effect={perm.get('effect')!r}")  # 合并模式保留原 effect，不校验
        if not set(rule["apps"]) <= set(perm.get("resources") or []):
            bad.append(f"resources={perm.get('resources')}")
        if rid not in (perm.get("roles") or []):
            bad.append(f"roles={perm.get('roles')}")
        if "Read" not in (perm.get("actions") or []):
            bad.append(f"actions={perm.get('actions')}")
        if perm.get("model") != f"{ORG}/{MODEL_NAME}" or perm.get("adapter") != ADAPTER_NAME:
            bad.append(f"model/adapter={perm.get('model')}/{perm.get('adapter')}")
        if bad:
            print(f"[D] {ORG}/{target} ✘ 权限异常: {', '.join(bad)}")
            ok = False
        else:
            print(f"[D] {rule['role']} -> {rule['apps']} ✔ "
                  f"（权限 {target} Approved/{perm.get('effect')}，model={perm.get('model')}）")
    return ok

def verify_enforce(rules):
    """实测两条 enforce 路径：
    - enforcerId（自建 enforcer + 6 元素模型）：role 主体 Read 命中
    - permissionId（CheckLoginPermission 真实运行时路径，运行时重建
      user->role 的 g 行）：user 主体 Read 命中"""
    eid = f"{ORG}/{ENFORCER_NAME}"
    ok = True
    for rule in rules:
        for app in rule["apps"]:
            res = api("POST", "/api/enforce", params={"enforcerId": eid},
                      json=[role_id(rule), app, "Read"])
            allowed = bool((res.get("data") or [False])[0])
            print(f"[D] enforcer   {role_id(rule)} -> {app} Read = {allowed}")
            ok = ok and allowed
        if rule.get("manage_role", True) and rule["users"]:
            uid, app0 = f"{ORG}/{rule['users'][0]}", rule["apps"][0]
            res = api("POST", "/api/enforce",
                      params={"permissionId": f"{ORG}/{perm_target(rule)}"},
                      json=[uid, app0, "Read"])
            allowed = bool((res.get("data") or [False])[0])
            print(f"[D] permission {uid} -> {app0} Read = {allowed}")
            ok = ok and allowed
    return ok

# ---------------- --delete：清理 ----------------
def delete_all(rules):
    print("== 删除脚本维护的权限与角色（三件套不清理）==")
    # 权限侧：合并模式 -> 仅把角色从 roles 移除；新建模式 -> 删整个权限
    for rule in rules:
        if rule.get("permission"):
            pid = f"{ORG}/{rule['permission']}"
            cur = api("GET", "/api/get-permission", params={"id": pid}).get("data")
            if cur and role_id(rule) in (cur.get("roles") or []):
                body = dict(cur)
                body["roles"] = [r for r in body["roles"] if r != role_id(rule)]
                res = api("POST", "/api/update-permission", params={"id": pid}, json=body)
                print(f"[del-perm] 从 {rule['permission']} 移除 {role_id(rule)} -> {res.get('status')}/{res.get('data') or ''}")
            else:
                print(f"[del-perm] {rule['permission']} 不含 {role_id(rule)}，跳过")
        else:
            res = api("POST", "/api/delete-permission", json={"owner": ORG, "name": perm_name(rule)})
            print(f"[del-perm] {perm_name(rule)} -> {res.get('status')}/{res.get('data') or ''} {res.get('msg', '')}")
    # 角色侧：manage_role=False -> 仅引用既有角色，不删除
    for rule in rules:
        if not rule.get("manage_role", True):
            print(f"[del-role] {rule['role']} 为既有引用角色，不删除")
            continue
        res = api("POST", "/api/delete-role", json={"owner": ORG, "name": rule["role"]})
        print(f"[del-role] {rule['role']} -> {res.get('status')}/{res.get('data') or ''} {res.get('msg', '')}")

    roles = {r["name"] for r in api("GET", "/api/get-roles", params={"owner": ORG})["data"]}
    perms = {p["name"] for p in api("GET", "/api/get-permissions", params={"owner": ORG})["data"]}
    left_r = [rule["role"] for rule in rules
              if rule.get("manage_role", True) and rule["role"] in roles]
    left_p = [perm_name(rule) for rule in rules
              if not rule.get("permission") and perm_name(rule) in perms]
    bad_merge = []
    for rule in rules:  # 合并模式回读：角色应已从目标权限移除
        if rule.get("permission"):
            cur = api("GET", "/api/get-permission", params={"id": f"{ORG}/{rule['permission']}"}).get("data")
            if cur and role_id(rule) in (cur.get("roles") or []):
                bad_merge.append(rule["permission"])
    if left_r or left_p or bad_merge:
        sys.exit(f"删除后仍存在 角色{left_r} 权限{left_p} 合并未清理{bad_merge}，请检查上方输出")
    print("删除完成并通过回读确认")

# ---------------- 主流程 ----------------
def main():
    parser = argparse.ArgumentParser(description="配置基于角色访问应用（Role -> Application）")
    parser.add_argument("--delete", action="store_true", help="删除本脚本维护的权限与角色")
    args = parser.parse_args()

    admin_org, admin_name = ADMIN.split("/")
    j = api("POST", "/api/login", json={
        "type": "login", "username": admin_name, "password": PASSWORD,
        "application": LOGIN_APP, "organization": admin_org,
    })
    if j.get("status") != "ok":
        sys.exit(f"登录失败: {j}")

    if args.delete:
        delete_all(ROLE_APP_RULES)
        return

    print("== 阶段 A：前置校验 ==")
    user_names, app_names = fetch_org_user_names(), fetch_app_names()
    users_ok = phase_a_check(ROLE_APP_RULES, user_names, app_names)

    print("== 阶段 B：Casbin 三件套 + upsert 角色 ==")
    ensure_casbin_objects()
    existing_role_ids = {f"{ORG}/{r['name']}"
                         for r in api("GET", "/api/get-roles", params={"owner": ORG})["data"]}
    for rule in ROLE_APP_RULES:
        if not rule.get("manage_role", True):
            if role_id(rule) not in existing_role_ids:
                sys.exit(f"[B] 引用的角色不存在（manage_role=False）: {role_id(rule)}")
            print(f"[B] 角色 {rule['role']} 引用既有，不改动其成员")
            continue
        res = upsert_role(rule, existing_role_ids)
        check_write(res, f"[B] 角色 {rule['role']}")
        print(f"[B] 角色 {rule['role']}（成员 {len(rule.get('users') or [])} 个）-> {res.get('status')}/{res.get('data') or ''}")

    print("== 阶段 C：upsert 应用访问权限 ==")
    existing_perm_ids = {p.get("id") or f"{p['owner']}/{p['name']}"
                         for p in api("GET", "/api/get-permissions", params={"owner": ORG})["data"]}
    for rule in ROLE_APP_RULES:
        target = perm_target(rule)
        res = upsert_permission(rule, existing_perm_ids)
        check_write(res, f"[C] 权限 {target}")
        print(f"[C] 权限 {target}（{rule['role']} -> {rule['apps']}）-> {res.get('status')}/{res.get('data') or ''}")

    print("== 阶段 D：回读验证 + enforcer 实测 ==")
    ok = phase_d_verify(ROLE_APP_RULES)
    ok = verify_enforce(ROLE_APP_RULES) and ok
    ok = ok and users_ok
    print("验证结果: " + ("通过 ✔" if ok else "未通过 ✘"))
    print("提示: Allow 型 Application 权限存在即 Deny-by-default —— "
          f"组织 {ORG} 下未命中任何权限的用户将无法登录这些应用"
          "（built-in 组织用户与 IsAdmin 用户不受影响）；Deny 优先于 Allow。")
    if not ok:
        sys.exit("未达到预期效果：请修正上方告警后重跑本脚本（幂等）")
    print("完成：角色 -> 应用 授权链已配置并通过验证")

if __name__ == "__main__":
    main()
