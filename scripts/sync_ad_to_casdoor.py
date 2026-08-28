#!/usr/bin/env python3
"""
AD -> Casdoor 同步脚本

功能：
  1. 在 Casdoor 中创建/更新 LDAP 服务器配置（指向 AD）
  2. 开启组织树（OU/安全组）自动同步
  3. 通过 API 立即拉取 AD 用户并同步到 Casdoor

用法：
  1. 修改下方 CONFIG 配置块
  2. pip install requests
  3. python3 sync_ad_to_casdoor.py

认证方式：
  使用 Casdoor Application 的 clientId / clientSecret 进行 API 认证。
  需要在 Casdoor 中创建一个 Application，并赋予管理员权限（所属组织为 built-in）。
"""

import json
import sys
import time
import requests

# ============================================================
#  配置区 —— 请根据实际环境修改
# ============================================================

CONFIG = {
    # Casdoor 服务地址（不要加末尾斜杠）
    "casdoor_endpoint": "http://localhost:8000",
    # Casdoor Application 的 clientId / clientSecret
    # 在 Casdoor 管理界面 -> Applications 中创建或使用已有 Application
    "client_id": "YOUR_CLIENT_ID",
    "client_secret": "YOUR_CLIENT_SECRET",
    # Casdoor 中目标组织名称（如 built-in 或自建组织名）
    "organization": "built-in",
    # ---- AD 服务器连接信息 ----
    "ldap_id": "ad-sync-default",  # LDAP 配置的唯一 ID（自定义）
    "ldap_server_name": "AD Sync",  # 显示名称
    "ad_host": "192.168.1.100",  # AD 服务器 IP 或域名
    "ad_port": 389,  # LDAP 端口（389 或 636 for LDAPS）
    "ad_enable_ssl": False,  # 是否启用 SSL
    "ad_allow_self_signed_cert": False,  # 是否允许自签名证书
    "ad_bind_dn": "CN=svc_ldap,OU=ServiceAccounts,DC=example,DC=com",  # 绑定 DN
    "ad_bind_password": "YOUR_AD_PASSWORD",  # 绑定密码
    "ad_base_dn": "DC=example,DC=com",  # 搜索基准 DN
    # 用户过滤条件，留空则使用 (objectClass=*)
    # 对于 AD，建议使用：(&(objectClass=user)(objectCategory=person))
    "ad_filter": "(&(objectClass=user)(objectCategory=person))",
    # 用户搜索字段（用于登录时匹配），如 ["sAMAccountName", "mail"]
    "filter_fields": ["sAMAccountName"],
    # 新同步用户的默认组（Casdoor 中已存在的组名，格式: owner/group_name）
    "default_groups": [],
    # 密码类型：SSHA / MD5 / Plain
    "password_type": "Plain",
    # ---- 组织树同步 ----
    "enable_groups": True,  # 设为 True 以同步 AD 的 OU 和安全组为 Casdoor 组
    "auto_sync_minutes": 5,  # 自动同步间隔（分钟），0 表示不自动同步
    # ---- 自定义属性映射 ----
    # key = AD 属性名, value = Casdoor 用户属性名
    "custom_attributes": {},
}

# ============================================================
#  以下是脚本逻辑，一般不需要修改
# ============================================================


class CasdoorSyncClient:
    def __init__(self, config):
        self.config = config
        self.base_url = config["casdoor_endpoint"]
        self.auth_params = {
            "clientId": config["client_id"],
            "clientSecret": config["client_secret"],
        }

    def _get(self, path, params=None):
        url = f"{self.base_url}{path}"
        merged_params = {**self.auth_params, **(params or {})}
        resp = requests.get(url, params=merged_params, timeout=30)
        resp.raise_for_status()
        try:
            return resp.json()
        except ValueError:
            raise RuntimeError(f"服务器返回非 JSON 响应: {resp.text[:200]}")

    def _post(self, path, params=None, json_body=None):
        url = f"{self.base_url}{path}"
        merged_params = {**self.auth_params, **(params or {})}
        resp = requests.post(url, params=merged_params, json=json_body, timeout=60)
        resp.raise_for_status()
        try:
            return resp.json()
        except ValueError:
            raise RuntimeError(f"服务器返回非 JSON 响应: {resp.text[:200]}")

    def get_ldaps(self):
        """获取当前组织已有的 LDAP 配置列表"""
        result = self._get("/api/get-ldaps", {"owner": self.config["organization"]})
        return result.get("data", []) if isinstance(result, dict) else result

    def find_ldap(self, ldap_id):
        """查找指定 ID 的 LDAP 配置"""
        ldaps = self.get_ldaps()
        for ldap in ldaps:
            if ldap.get("id") == ldap_id:
                return ldap
        return None

    def add_ldap(self):
        """在 Casdoor 中添加 LDAP 服务器配置"""
        ldap_config = self._build_ldap_config()
        result = self._post("/api/add-ldap", json_body=ldap_config)

        if result.get("status") == "ok":
            print(f"[OK] LDAP 配置创建成功: {ldap_config['id']}")
            # 启动自动同步（如果配置了）
            if self.config["auto_sync_minutes"] > 0:
                self._start_auto_sync(ldap_config["id"])
            return True
        else:
            print(f"[ERROR] LDAP 配置创建失败: {result.get('msg', '未知错误')}")
            return False

    def update_ldap(self, existing):
        """更新 Casdoor 中已有的 LDAP 服务器配置"""
        ldap_config = self._build_ldap_config()
        # 更新时需要保留 id
        ldap_config["id"] = existing.get("id", self.config["ldap_id"])
        # 后端 UpdateLdap 在 password == "***" 时自动使用原密码，
        # 因此直接发送明文密码即可（如果配置中提供了的话）。
        # 同时保留已有的 defaultGroup（单数）字段，避免被清空。
        if existing.get("defaultGroup"):
            ldap_config["defaultGroup"] = existing["defaultGroup"]

        result = self._post("/api/update-ldap", json_body=ldap_config)

        if result.get("status") == "ok":
            print(f"[OK] LDAP 配置更新成功: {ldap_config['id']}")
            # 重启自动同步
            if self.config["auto_sync_minutes"] > 0:
                self._start_auto_sync(ldap_config["id"])
            return True
        else:
            print(f"[ERROR] LDAP 配置更新失败: {result.get('msg', '未知错误')}")
            return False

    def _build_ldap_config(self):
        return {
            "owner": self.config["organization"],
            "id": self.config["ldap_id"],
            "serverName": self.config["ldap_server_name"],
            "host": self.config["ad_host"],
            "port": self.config["ad_port"],
            "enableSsl": self.config["ad_enable_ssl"],
            "allowSelfSignedCert": self.config["ad_allow_self_signed_cert"],
            "username": self.config["ad_bind_dn"],
            "password": self.config["ad_bind_password"],
            "baseDn": self.config["ad_base_dn"],
            "filter": self.config["ad_filter"],
            "filterFields": self.config["filter_fields"],
            "defaultGroups": self.config["default_groups"],
            "passwordType": self.config["password_type"],
            "customAttributes": self.config["custom_attributes"],
            "enableGroups": self.config["enable_groups"],
            "autoSync": self.config["auto_sync_minutes"],
        }

    def _start_auto_sync(self, ldap_id):
        """启动自动同步 — 通过 UpdateLdap API 间接触发

        Casdoor 的 AddLdap/UpdateLdap 在 autoSync != 0 时会自动启动定时同步。
        此函数已在上面的 add/update 中通过设置 autoSync 字段完成，
        这里仅做日志输出。
        """
        print(f"[INFO] 自动同步已配置，间隔 {self.config['auto_sync_minutes']} 分钟")
        print(f"[INFO] 组织树同步（OU + 安全组）将在首次定时任务时执行")

    def test_connection(self):
        """测试 AD 连接是否正常"""
        print("[INFO] 正在测试 AD 连接...")
        ldap_id = self.config["ldap_id"]
        owner = self.config["organization"]
        full_id = f"{owner}/{ldap_id}"

        try:
            result = self._get("/api/get-ldap-users", {"id": full_id})
            if isinstance(result, dict) and result.get("status") == "error":
                print(f"[ERROR] AD 连接失败: {result.get('msg', '未知错误')}")
                return False

            users = []
            if isinstance(result, dict):
                users = result.get("data", {}).get("users", [])
            elif isinstance(result, list):
                users = result

            print(f"[OK] AD 连接成功，共发现 {len(users)} 个用户")
            return True
        except requests.exceptions.RequestException as e:
            print(f"[ERROR] 请求失败: {e}")
            return False

    def get_ldap_users(self):
        """从 AD 拉取用户列表"""
        ldap_id = self.config["ldap_id"]
        owner = self.config["organization"]
        full_id = f"{owner}/{ldap_id}"

        result = self._get("/api/get-ldap-users", {"id": full_id})

        if isinstance(result, dict) and result.get("status") == "error":
            raise RuntimeError(f"获取 AD 用户失败: {result.get('msg')}")

        data = result.get("data", result) if isinstance(result, dict) else result
        if isinstance(data, dict):
            users = data.get("users", [])
            exist_uuids = data.get("existUuids", [])
        else:
            users = data
            exist_uuids = []

        return users, exist_uuids

    def sync_users(self, users):
        """将用户同步到 Casdoor"""
        ldap_id = self.config["ldap_id"]
        owner = self.config["organization"]
        full_id = f"{owner}/{ldap_id}"

        print(f"[INFO] 正在同步 {len(users)} 个用户到 Casdoor...")
        result = self._post(
            "/api/sync-ldap-users", params={"id": full_id}, json_body=users
        )

        if isinstance(result, dict) and result.get("status") == "error":
            raise RuntimeError(f"用户同步失败: {result.get('msg')}")

        data = result.get("data", result) if isinstance(result, dict) else result
        if isinstance(data, dict):
            exist_count = len(data.get("exist", []))
            failed_count = len(data.get("failed", []))
        else:
            exist_count = 0
            failed_count = 0

        new_count = len(users) - exist_count - failed_count
        print(f"[OK] 用户同步完成:")
        print(f"      新增用户: {new_count}")
        print(f"      已有用户: {exist_count}")
        print(f"      失败用户: {failed_count}")
        return new_count, exist_count, failed_count

    def run(self):
        """执行完整同步流程"""
        print("=" * 60)
        print("  AD -> Casdoor 同步脚本")
        print("=" * 60)
        print()

        # Step 1: 创建或更新 LDAP 配置
        print("[Step 1] 检查 LDAP 配置...")
        existing = self.find_ldap(self.config["ldap_id"])
        if existing:
            print(f"[INFO] LDAP 配置已存在: {self.config['ldap_id']}，执行更新...")
            if not self.update_ldap(existing):
                print("[ERROR] LDAP 配置更新失败，终止同步")
                return False
        else:
            print(f"[INFO] LDAP 配置不存在，创建新配置: {self.config['ldap_id']}")
            if not self.add_ldap():
                print("[ERROR] LDAP 配置创建失败，终止同步")
                return False
        print()

        # Step 2: 测试 AD 连接
        print("[Step 2] 测试 AD 连接...")
        if not self.test_connection():
            print("[ERROR] AD 连接测试失败，终止同步")
            return False
        print()

        # Step 3: 拉取 AD 用户
        print("[Step 3] 从 AD 拉取用户...")
        users, exist_uuids = self.get_ldap_users()
        print(f"[OK] 共拉取 {len(users)} 个用户")
        print(f"      其中 {len(exist_uuids)} 个用户已存在于 Casdoor")
        print()

        if len(users) == 0:
            print("[WARN] AD 中未找到任何用户，请检查过滤条件和 Base DN")
            return False

        # Step 4: 同步用户
        print("[Step 4] 同步用户到 Casdoor...")
        self.sync_users(users)
        print()

        # Step 5: 组织树同步状态
        print("[Step 5] 组织树同步状态...")
        if self.config["enable_groups"]:
            print("[INFO] 组织树同步已启用 (enableGroups=True)")
            print("[INFO] 同步内容: AD 安全组 (objectClass=group) + 组织单元 (OU)")
            print("[INFO] 层级结构: 父子关系通过 DN 自动保持")
            print()
            if self.config["auto_sync_minutes"] > 0:
                print(
                    f"[INFO] 自动同步已开启，间隔 {self.config['auto_sync_minutes']} 分钟"
                )
                print("[INFO] 组织树将在首次自动同步时完整同步")
                print("[INFO] 同步顺序: 先同步组 -> 再同步用户")
            else:
                print("[WARN] 自动同步未开启 (autoSync=0)")
                print("[WARN] 组织树同步仅在 LDAP 配置页面手动触发自动同步时执行")
        else:
            print("[WARN] 组织树同步未启用 (enableGroups=False)")
        print()

        print("=" * 60)
        print("  同步流程完成!")
        print("=" * 60)
        return True


def main():
    config = CONFIG

    # 简单的参数校验
    required_fields = [
        "casdoor_endpoint",
        "client_id",
        "client_secret",
        "organization",
        "ad_host",
        "ad_bind_dn",
        "ad_bind_password",
        "ad_base_dn",
    ]
    for field in required_fields:
        val = config.get(field, "")
        if not val or val.startswith("YOUR_"):
            print(f"[ERROR] 配置项 '{field}' 未设置，请修改脚本顶部的 CONFIG")
            sys.exit(1)

    client = CasdoorSyncClient(config)
    success = client.run()
    sys.exit(0 if success else 1)


if __name__ == "__main__":
    main()
