#!/usr/bin/env python3
# 纯脚本方案（不改动、不依赖重新部署远程 Go 代码），编排远程 Casdoor 已有能力：
#
# 关键事实（来自对远程实例的实测与本地源码核对）：
# 1. /api/sync-ldap-users 只同步用户：需要 query 参数 id=<org>/<ldapId>，请求体是
#    get-ldap-users 返回的用户数组（前端即是这么调用的）。
# 2. AD 树形群组（group + organizationalUnit 层级）同步只存在于 autoSync 定时任务
#    （object/ldap_autosync.go syncRoutine，enableGroups=true 时每周期先群组后用户）。
#    注意：远程构建缺少本地未提交的 selfParent 修复（cn=X inside ou=X 会把 parentId
#    写成自身，形成自引用），因此群组树同步完成后需要第 D 阶段按 memberOf DN 链修正层级。
# 3. update-ldap 在服务端会同步执行 StopAutoSync/StartAutoSync：若 goroutine 正在同步，
#    调用会阻塞到当前周期结束，网关可能 504 —— 但 DB 写入与定时器操作通常已生效，
#    必须以 get-ldaps 回读配置为准，不能只信 HTTP 响应。
#
# 流程：
#   A. 停止 autoSync 并【确认】已停止（配置归零 + 数据计数连续稳定）后才进入删除
#   B. 清空组织用户与群组（群组叶子先行，自引用残留留给 D 阶段修正）
#   C. 临时 autoSync=1 触发服务端群组树同步 -> 手动端点同步用户 -> 停回 autoSync=0
#   D. 用 get-ldap-users 返回的 memberOf DN 链重建 OU 父子关系，update-group 修正 parentId
#   E. 回读验证（无自引用、无环、用户挂组正确）并恢复 autoSync 原值
import sys, time, os, requests

CASDOOR   = "https://portal.msuncloud.cn"
ORG       = "zymsunsoft"          # 目标组织名
ADMIN     = "built-in/admin"      # 管理员（组织管理员则填 org/admin 形式）
PASSWORD  = os.environ.get("CASDOOR_ADMIN_PASSWORD", "")
if not PASSWORD:
    sys.exit("请先设置环境变量 CASDOOR_ADMIN_PASSWORD")
LOGIN_APP = "app-built-in"        # built-in 组织的登录应用；组织管理员需改为该组织应用
LDAP_NAME = None                  # 留 None = 该组织下所有 LDAP；或填 serverName/id 精确匹配

RESTORE_AUTOSYNC  = 5             # 结束时恢复的 autoSync（分钟，任务开始前的原值）
FAST_SYNC_MIN     = 1             # 临时 autoSync（分钟）——触发服务端群组树同步
EXPECTED_GROUPS   = 590           # 服务端同步目标（AD 全量约 601）
MIN_GROUPS        = 545           # 脚本自建模式下的最低可接受数（memberOf 链覆盖 ~555）
FALLBACK_STALL_S  = 300           # 服务端群组停滞超过该秒数 -> 切换脚本自建模式
POLL_S            = 15            # 常规轮询间隔
STABLE_POLLS      = 3             # 「确认已停止」所需的连续稳定次数
STABLE_GAP_S      = 20            # 稳定性轮询间隔
VERIFY_TIMEOUT_S  = 1500          # 等待服务端群组同步的总超时（AD 搜索可能多轮重试）
RETRIES          = 4              # 普通瞬时故障重试次数

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

def get_ldap_conf():
    return api("GET", "/api/get-ldaps", params={"owner": ORG})["data"]

def counts():
    us = api("GET", "/api/get-users",  params={"owner": ORG})["data"]
    gs = api("GET", "/api/get-groups", params={"owner": ORG})["data"]
    return us, gs

def set_autosync(ldap, value):
    """update-ldap 可能因服务端 Stop/StartAutoSync 阻塞而 504/超时，
    但 DB 写入通常已生效 —— 以 get-ldaps 回读到的值为准，最多补发 2 次。"""
    for attempt in range(3):
        try:
            r = s.post(f"{CASDOOR}/api/update-ldap", json={**ldap, "autoSync": value}, timeout=300)
            print(f"[{ldap['id']}] update-ldap autoSync={value} -> HTTP {r.status_code}")
        except Exception as e:
            print(f"[{ldap['id']}] update-ldap autoSync={value} 请求异常（可能已生效）: {e}")
        for _ in range(6):
            time.sleep(5)
            cur = next((l for l in get_ldap_conf() if l["id"] == ldap["id"]), None)
            if cur and cur.get("autoSync", 0) == value:
                print(f"[{ldap['id']}] 已确认 autoSync={value}")
                return cur
        print(f"[{ldap['id']}] 配置回读仍非 {value}，重试 ...")
    sys.exit(f"无法把 {ldap['id']} 的 autoSync 置为 {value}")

def wait_stable(tag, polls=STABLE_POLLS, gap=STABLE_GAP_S, timeout=900):
    """等待 (用户数, 群组数, 最新 updatedTime) 连续 polls 次不变 ——
    计数不变但时间戳在动，说明服务端仍有同步周期在逐条写库，必须继续等"""
    deadline = time.time() + timeout
    last = None
    stable = 0
    while time.time() < deadline:
        us, gs = counts()
        stamp = max([x.get("updatedTime") or "" for x in gs + us])
        cur = (len(us), len(gs), stamp)
        if cur == last:
            stable += 1
            if stable >= polls:
                print(f"[{tag}] 数据已连续 {polls} 次稳定 {cur}，同步确认停止/完成")
                return cur
        else:
            stable = 0
            last = cur
            print(f"[{tag}] 计数 {cur}，等待稳定 ...")
        time.sleep(gap)
    sys.exit(f"[{tag}] 等待数据稳定超时")

def derive_parents(ldap_users):
    """从 get-ldap-users 的 memberOf DN 链推导群组父子关系。
    与服务端 dnToGroupName 一致：取 RDN 值，'/' -> '_'；cn=X inside ou=X 的自引用
    沿用 OU 自身的父级。同时记录链上深度（根侧小、叶子大）供建组排序用。"""
    def rdn_name(comp):
        return comp.split("=", 1)[1].replace("/", "_")

    ou_parent, cn_parent, depth = {}, {}, {}
    for u in ldap_users or []:
        for dn in u.get("memberOf") or []:
            comps = [c.strip() for c in dn.split(",")
                     if not c.strip().lower().startswith("dc=") and "=" in c]
            for i, comp in enumerate(comps):
                key, name = comp.split("=", 1)[0].upper(), rdn_name(comp)
                if key == "OU":
                    parent = rdn_name(comps[i + 1]) if i + 1 < len(comps) else ""
                    ou_parent.setdefault(name, parent)  # 最右 OU 的父级是 baseDn -> 顶层
                    depth.setdefault(name, len(comps) - i)
                elif i == 0:  # CN 叶子群组
                    parent = rdn_name(comps[1]) if len(comps) > 1 else ""
                    if name == parent:  # cn=X inside ou=X：自引用 -> 沿用 OU 自身的父级
                        parent = ou_parent.get(parent, "")
                    cn_parent.setdefault(name, parent)
                    depth.setdefault(name, len(comps))
    return ou_parent, cn_parent, depth

def build_missing_groups(ou_parent, cn_parent, depth):
    """服务端 AD 群组搜索不稳定时的兜底：用 memberOf 链直接 add-group 补建缺失群组，
    按 DN 深度从根到叶创建（父先子后），完全绕开服务端的不稳定搜索。"""
    _, gs = counts()
    have = {g["name"] for g in gs}
    todo = {n: (p, depth[n]) for n, p in {**ou_parent, **cn_parent}.items() if n not in have}
    created = 0
    for name, (parent, _) in sorted(todo.items(), key=lambda kv: kv[1][1]):
        if parent and parent not in have:
            parent = ""  # 父级本轮未被建出（异常）-> 顶层，避免悬空引用
        res = api("POST", "/api/add-group", json={
            "owner": ORG, "name": name, "displayName": name,
            "parentId": parent, "isTopGroup": not parent,
            "type": "ldap-synced", "isEnabled": True,
        })
        if res.get("status") == "ok":
            have.add(name)
            created += 1
        else:
            print(f"[build] {name} 失败: {res}")
    print(f"[build] 脚本自建群组 {created} 个，群组总数 {len(have)}")
    return created

# ---------------- 登录 ----------------
admin_org, admin_name = ADMIN.split("/")
j = api("POST", "/api/login", json={
    "type": "login", "username": admin_name, "password": PASSWORD,
    "application": LOGIN_APP, "organization": admin_org,
})
if j.get("status") != "ok":
    sys.exit(f"登录失败: {j}")

targets = [l for l in get_ldap_conf()
           if not LDAP_NAME or LDAP_NAME in (l.get("id"), l.get("serverName"))]
if not targets:
    sys.exit(f"组织 {ORG} 下没有匹配 LDAP_NAME={LDAP_NAME} 的 LDAP 配置")

# ---------------- A. 停止 autoSync 并确认 ----------------
print("== 阶段 A：停止组织同步并确认 ==")
for ldap in targets:
    if ldap.get("autoSync", 0) != 0:
        print(f"[{ldap['id']}] 当前 autoSync={ldap['autoSync']}，先停止定时任务")
        set_autosync(ldap, 0)
    else:
        print(f"[{ldap['id']}] autoSync 已为 0")
wait_stable("A")

# ---------------- B. 清空用户与群组 ----------------
print("== 阶段 B：清空组织用户与群组 ==")
users, groups = counts()
for u in users:
    res = api("POST", "/api/delete-user", json=u)
    print(f"[del-user] {u['name']} -> {res.get('status')}")
print(f"共删除用户 {len(users)} 个")

pending = list(groups)
deleted = 0
while pending:
    names = {g["name"] for g in pending}
    removable = [g for g in pending if not g.get("parentId") or g["parentId"] not in names]
    if not removable:
        print(f"[del-group] 存在自引用/循环依赖，{len(pending)} 个残留待 D 阶段修正: "
              f"{[g['name'] for g in pending][:8]} ...")
        break
    for g in removable:
        res = api("POST", "/api/delete-group", json=g)
        if res.get("status") != "ok":
            print(f"[del-group] {g['name']} 删除失败: {res}")
            pending.remove(g)
            continue
        pending.remove(g)
        deleted += 1
print(f"共删除群组 {deleted} 个，残留 {len(pending)} 个")

# ---------------- C. 同步群组树与用户 ----------------
print("== 阶段 C：同步群组树与用户 ==")

# 先取 LDAP 用户与 memberOf DN 链（get-ldap-users）——它同时是用户同步的请求体、
# 群组树推导（自建 fallback / D 阶段修正）的数据源
ldap_users = None
for ldap in targets:
    lid = f"{ORG}/{ldap['id']}"
    resp = api("GET", "/api/get-ldap-users", params={"id": lid})
    ldap_users = resp["data"]["users"]
    print(f"[C] LDAP 用户 {len(ldap_users)} 个")
ou_parent, cn_parent, g_depth = derive_parents(ldap_users)

# 服务端 autosync（enableGroups=True）每周期会同步群组树；但远程对 AD 的大搜索
# （objectClass=group 全子树）间歇性超时，甚至令 goroutine 长时间挂起（goldap 无
# 读超时），群组数可能长期停滞 —— 停滞超 FALLBACK_STALL_S 即切换脚本自建模式
for ldap in targets:
    cur = set_autosync({**ldap, "enableGroups": True}, FAST_SYNC_MIN)
    print(f"[{ldap['id']}] enableGroups=True autoSync={FAST_SYNC_MIN}min，等待服务端同步群组树")

fallback = False
deadline = time.time() + VERIFY_TIMEOUT_S
best, since = len(counts()[1]), time.time()
while time.time() < deadline:
    time.sleep(POLL_S)
    _, gs = counts()
    if len(gs) > best:
        best, since = len(gs), time.time()
        print(f"[C] 群组已同步 {best} 个 ...")
    if best >= EXPECTED_GROUPS:
        print(f"[C] 群组已达到预期规模 {best} >= {EXPECTED_GROUPS}")
        break
    if time.time() - since > FALLBACK_STALL_S:
        fallback = True
        print(f"[C] 服务端群组同步停滞在 {best} 个超过 {FALLBACK_STALL_S}s，切换脚本自建模式")
        break
if best < EXPECTED_GROUPS and not fallback:
    sys.exit(f"超时：服务端群组同步仅 {best} 个 < 预期 {EXPECTED_GROUPS}")

# 停定时任务，防止后续周期把脚本写入改回去（挂死的 goroutine 停不掉也无碍：
# 它不再写库；update-ldap 服务端先写 DB 再 Stop，配置以回读为准）
for ldap in targets:
    set_autosync(ldap, 0)
wait_stable("C-stop")

if fallback:
    build_missing_groups(ou_parent, cn_parent, g_depth)

# 手动同步用户（前端方式：sync-ldap-users?id=org/ldapId，body 为用户数组），
# 服务端会把用户挂到 memberOf 对应的已存在群组上
for ldap in targets:
    lid = f"{ORG}/{ldap['id']}"
    res = api("POST", "/api/sync-ldap-users", params={"id": lid}, json=ldap_users)
    print(f"[C] sync-ldap-users -> {res.get('status')}, "
          f"exist={len(res.get('data', {}).get('exist') or [])}, "
          f"failed={len(res.get('data', {}).get('failed') or [])}")
wait_stable("C-end")

# ---------------- D. 依据 memberOf DN 链修正群组层级 ----------------
print("== 阶段 D：修正群组树层级 ==")
users, groups = counts()
by_name = {g["name"]: g for g in groups}

# 根群组：memberOf 推导中父级为 ""（baseDn 之下）的最右 OU，如「众阳健康集团」
root = next((n for n, p in ou_parent.items() if p == "" and n in by_name), None)
print(f"[D] 根群组: {root!r}")

def target_parent(g):
    """三级策略：memberOf 推导 > 服务端现有有效 parentId > 挂根/顶层。
    memberOf 只含用户直接挂靠的群组链，无用户的群组推导不到；此时保留服务端
    同步写的 parentId（远程 selfParent bug 只会产生『指向自身』的无效值，
    其余非自引用值即为服务端正确构建的结果），无效则挂到根群组下。"""
    name = g["name"]
    if name == root:
        return "", True
    p = ou_parent.get(name, cn_parent.get(name))
    if p is None:
        cur = g.get("parentId")
        if cur and cur != name and cur in by_name:
            return cur, False   # 保留服务端现有（有效）层级
        p = root or ""           # 现有无效（自引用/悬空）-> 挂根或顶层
    if p and p not in by_name:
        p = root or ""
    return p, p == ""

target = {g["name"]: target_parent(g) for g in groups}

def chain_has_cycle(start):  # 沿 parentId 链检测环/自引用
    seen, cur = {start}, start
    while True:
        p = target.get(cur, ("",))[0]
        if not p or p not in by_name:
            return False
        if p in seen:
            return True
        seen.add(p)
        cur = p

for _ in range(5):  # 迭代消除环：环上节点统一挂根（root 不在任何环上）
    bad = [n for n in target if chain_has_cycle(n)]
    if not bad:
        break
    print(f"[D] 环上群组 {len(bad)} 个，改挂根: {bad[:6]} ...")
    for n in bad:
        target[n] = ("", True) if n == root else (root or "", not root)

fixed = 0
for g in groups:
    p, top = target[g["name"]]
    if (g.get("parentId"), bool(g.get("isTopGroup"))) != (p, top):
        res = api("POST", "/api/update-group", params={"id": f"{ORG}/{g['name']}"},
                  json={**g, "parentId": p, "isTopGroup": top})
        if res.get("status") == "ok":
            fixed += 1
        else:
            print(f"[fix] {g['name']} -> {p!r} 失败: {res}")
print(f"层级修正 {fixed} 个（memberOf 推导 + 无用户群组保留/挂根 + 环消除）")

# ---------------- E. 验证并恢复 ----------------
print("== 阶段 E：回读验证 ==")
users, groups = counts()
by_name = {g["name"]: g for g in groups}
self_ref = [g["name"] for g in groups if g.get("parentId") == g["name"]]

def depth_ok(g):
    p, n = g.get("parentId"), 0
    while p and n < 64:
        pg = by_name.get(p)
        if not pg:
            return True  # 指向不存在（历史残留）不视为环
        p, n = pg.get("parentId"), n + 1
    return n < 64

cycles = [g["name"] for g in groups if not depth_ok(g)]
tops = [g["name"] for g in groups if g.get("isTopGroup")]
with_groups = [u for u in users if u.get("groups")]
print(f"群组 {len(groups)} 个（顶层 {len(tops)}：{tops[:6]} ...；自引用 {len(self_ref)}；环 {len(cycles)}）")
print(f"用户 {len(users)} 个，其中已挂接群组 {len(with_groups)} 个")
for u in with_groups[:5]:
    print(f"  - user {u['name']} groups={u['groups']}")
for g in sorted(tops)[:3]:
    kids = [x['name'] for x in groups if x.get('parentId') == g]
    print(f"  - tree [{g}] -> {kids[:6]}{' ...' if len(kids) > 6 else ''}")

min_groups = MIN_GROUPS if fallback else EXPECTED_GROUPS
ok = (bool(groups) and bool(users) and not self_ref and not cycles
      and len(groups) >= min_groups                     # 群组树必须同步完整
      and len(with_groups) >= int(len(users) * 0.9))    # 至少 90% 用户挂上群组
print("验证结果: " + ("通过 ✔" if ok else "未通过 ✘")
      + f"（群组 {len(groups)}/{min_groups}+，挂组 {len(with_groups)}/{len(users)}"
      + ("，脚本自建模式）" if fallback else "）"))
for ldap in targets:
    body = {**ldap, "enableGroups": not ok, "autoSync": RESTORE_AUTOSYNC}
    # 远程构建缺少 selfParent 修复：enableGroups=true 的每个同步周期都会把
    # cn=X/ou=X 群组的 parentId 改回自身（重新破坏层级）。因此验证通过时保持
    # enableGroups=false（树保持正确，autosync 仅同步用户并挂接既有群组）；
    # 部署含修复的构建后可改回 true。
    r = s.post(f"{CASDOOR}/api/update-ldap", json=body, timeout=300)
    print(f"[{ldap['id']}] 恢复 autoSync={RESTORE_AUTOSYNC} enableGroups={body['enableGroups']} "
          f"-> HTTP {r.status_code}")

if not ok:
    sys.exit("未达到预期效果：请检查上方输出（可重跑本脚本，会先清空再同步）")
print("完成：AD 树形组织群组与用户均已同步并通过验证")
