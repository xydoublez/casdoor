#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""OAuth 2.0 授权码 + PKCE (S256) 端到端验证脚本 —— 智能运维系统对接参考实现

完整流程演示:
  1. 生成 code_verifier / code_challenge (RFC 7636)
  2. 用户登录 Casdoor(此处用 API 模拟, 浏览器场景为跳转登录页)
  3. 请求授权端点拿 authorization code(带 code_challenge)
  4. 用 code + code_verifier 换 access_token(公共客户端免 client_secret)
  5. 用 access_token 获取用户信息
  6. 反向验证: 错误的 code_verifier 必须被拒绝

用法:
  AD_KNOWN_PASSWORD='真实AD密码' python3 oauth_pkce_test.py
"""
import base64
import hashlib
import http.cookiejar
import json
import os
import secrets
import sys
import urllib.error
import urllib.parse
import urllib.request

BASE = os.environ.get("CASDOOR_URL", "http://localhost:8000")
ORG = "zymsunsoft"
APP = "application_ops_platform"
CLIENT_ID = "5754287ce9681f500b78"
REDIRECT_URI = "http://localhost:8080/callback"
SCOPE = "openid profile email"

_JAR = http.cookiejar.CookieJar()
opener = urllib.request.build_opener(
    urllib.request.HTTPCookieProcessor(_JAR))


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, *args, **kwargs):
        return None


def gen_pkce():
    """RFC 7636: verifier 43-128 位随机串, challenge = BASE64URL(SHA256(verifier))"""
    verifier = base64.urlsafe_b64encode(secrets.token_bytes(48)).decode().rstrip("=")
    challenge = base64.urlsafe_b64encode(
        hashlib.sha256(verifier.encode()).digest()).decode().rstrip("=")
    return verifier, challenge


def api_login(username, password):
    """type=code: 登录并直接换取 authorization code(浏览器场景=authorize 端点 302 回调)"""
    qs = urllib.parse.urlencode({
        "clientId": CLIENT_ID, "responseType": "code",
        "redirectUri": REDIRECT_URI, "scope": SCOPE,
        "state": "st-" + secrets.token_hex(8), "nonce": secrets.token_hex(8),
        "code_challenge": api_login.challenge, "code_challenge_method": "S256",
    })
    payload = json.dumps({
        "organization": ORG, "application": APP, "username": username,
        "password": password, "type": "code", "autoSignin": True,
        "signinMethod": "Password",
    }).encode()
    req = urllib.request.Request(BASE + "/api/login?" + qs, data=payload,
                                 headers={"Content-Type": "application/json"})
    with opener.open(req) as r:
        d = json.loads(r.read())
    assert d.get("status") == "ok" and d.get("data"), "登录失败: %s" % d
    return d["data"]  # authorization code


def get_auth_code(code_challenge, username, password):
    """登录并换取 authorization code"""
    api_login.challenge = code_challenge
    return api_login(username, password)


def exchange_token(code, code_verifier, with_secret=False):
    data = urllib.parse.urlencode({
        "grant_type": "authorization_code",
        "client_id": CLIENT_ID,
        "code": code,
        "code_verifier": code_verifier,
        "redirect_uri": REDIRECT_URI,
    }).encode()
    req = urllib.request.Request(BASE + "/api/login/oauth/access_token", data=data,
                                 headers={"Content-Type": "application/x-www-form-urlencoded"})
    try:
        with urllib.request.build_opener(NoRedirect()).open(req) as r:
            return r.status, json.loads(r.read())
    except urllib.error.HTTPError as e:
        return e.code, json.loads(e.read() or b"{}")


def get_userinfo(access_token):
    req = urllib.request.Request(BASE + "/api/userinfo",
                                 headers={"Authorization": "Bearer %s" % access_token})
    with urllib.request.build_opener(NoRedirect()).open(req) as r:
        return json.loads(r.read())


def main():
    password = os.environ.get("AD_KNOWN_PASSWORD", "")
    username = os.environ.get("AD_KNOWN_USER", "lizhiqiang-test")
    if not password:
        print("[FATAL] 请通过环境变量 AD_KNOWN_PASSWORD 提供测试账号密码")
        sys.exit(1)

    print("[1/5] 生成 PKCE 密钥对 (S256)")
    verifier, challenge = gen_pkce()
    print("  code_verifier : %s...(%d字符)" % (verifier[:16], len(verifier)))
    print("  code_challenge: %s..." % challenge[:24])

    print("[2/5] 登录 + 获取授权码 (浏览器场景=authorize 302 到回调地址)")
    code = get_auth_code(challenge, username, password)
    print("  %s?code=%s..." % (REDIRECT_URI, code[:20]))

    print("[3/5] code + code_verifier 换 token (无 client_secret, 公共客户端)")
    status, tok = exchange_token(code, verifier)
    ok = status == 200 and "access_token" in tok
    print("  HTTP %s | access_token: %s... | expires_in: %ss | scope: %s"
          % (status, tok.get("access_token", "")[:20], tok.get("expires_in"), tok.get("scope")))
    if not ok:
        print("  失败详情:", tok)
        sys.exit(1)

    print("[4/5] Bearer token 获取用户信息")
    info = get_userinfo(tok["access_token"])
    print("  sub=%s | preferred_username=%s | name=%s | email=%s"
          % (info.get("sub"), info.get("preferred_username"),
             info.get("name"), info.get("email")))

    print("\n[反向验证] 错误的 code_verifier 必须被拒绝")
    v2 = get_auth_code(challenge, username, password)
    status2, resp2 = exchange_token(v2, verifier.replace(verifier[0], "X" if verifier[0] != "X" else "Y"))
    rejected = status2 != 200 or "access_token" not in resp2
    print("  HTTP %s | error=%s" % (status2, resp2.get("error", "")) if rejected
          else "  *** 未被拒绝, PKCE 失效! ***")
    print("  结果: %s" % ("已正确拒绝 (invalid_grant)" if rejected else "FAIL"))

    print("\n" + "=" * 70)
    print("结论: OAuth2.0 + PKCE(S256) 全链路 %s" % ("通过" if ok and rejected else "失败"))


if __name__ == "__main__":
    main()
