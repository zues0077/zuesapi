#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""给 web2api 管理面板开局域网访问（自用补丁）。

原因：飞牛无图形界面 → 内置浏览器登录不可用；面板又被限制为「仅本机」，
同网段设备一律 403，导致根本没法手动粘贴凭证。
本补丁只放宽到 RFC1918 / 回环 / 链路本地，公网 IP 仍然 403。
"""
import io
import shutil
import sys

P = sys.argv[1] if len(sys.argv) > 1 else "/vol2/@appcenter/web2api/pysite/webai2api/app.py"

HELPER = '''    def _is_private_client(host: str) -> bool:
        """局域网客户端放行（自用补丁）：飞牛无图形界面，内置浏览器登录不可用，
        面板必须能从同网段设备打开才能手动粘凭证。仅放行私有地址。"""
        import ipaddress

        try:
            ip = ipaddress.ip_address(host)
        except ValueError:
            return False
        if isinstance(ip, ipaddress.IPv6Address) and ip.ipv4_mapped:
            ip = ip.ipv4_mapped
        return ip.is_private or ip.is_loopback or ip.is_link_local

    def _is_private_host(host: str) -> bool:
        """Host 头为局域网地址/本机名时放行，否则 192.168.x.x 面板照样 403。"""
        host = (host or "").strip()
        if host in ("localhost", "127.0.0.1", "::1", "[::1]"):
            return True
        try:
            ip = ipaddress.ip_address(host.strip("[]"))
        except ValueError:
            return host.endswith(".local") or ("." not in host and host != "")
        return ip.is_private or ip.is_loopback or ip.is_link_local

'''

def main():
    s = io.open(P, encoding="utf-8").read()
    if "_is_private_client" in s:
        print("already patched")
        return 0

    old = """                or not _is_local_client(client)
                or not local_host
            ):"""
    new = """                or not (_is_local_client(client) or _is_private_client(client))
                or not (local_host or _is_private_host(host_name))
            ):"""
    if old not in s:
        print("ERROR: guard anchor not found")
        return 1

    shutil.copy2(P, P + ".bak-lan")
    s = s.replace(old, new, 1)

    anchor = "    def _is_local_client(host: str) -> bool:"
    if anchor not in s:
        print("ERROR: helper anchor not found")
        return 1
    s = s.replace(anchor, HELPER + anchor, 1)

    # 需要 ipaddress 在 _is_private_host 所在作用域可用（函数内已 import，这里给模块级兜底）
    if "\nimport ipaddress\n" not in s.split("def create_app")[0]:
        s = s.replace("import json\n", "import ipaddress\nimport json\n", 1)

    io.open(P, "w", encoding="utf-8").write(s)
    print("patched ok")
    return 0


if __name__ == "__main__":
    sys.exit(main())
