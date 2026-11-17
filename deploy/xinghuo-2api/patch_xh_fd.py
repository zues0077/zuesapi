#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""把 xinghuo-2api 里写死的 fd / chatId 改成可配置（读 .env）。

背景（2026-11-17 实测）：
  原代码 `_prepare_payload` 里硬编码了作者自己账号的
    "fd": "775510"、"chatId": "985818485"
  用自己的 Cookie + GtToken 去请求，星火返回 **HTTP 200 但流内容为空**，
  表现成「鉴权通过却回复空」。这两个值都是账号/会话级的，必须换成自己的。
"""
import io
import re
import sys

CONF = "/vol1/1000/docker/xinghuo-2api/app/core/config.py"
PROV = "/vol1/1000/docker/xinghuo-2api/app/providers/xinghuo_provider.py"


def patch_config():
    s = io.open(CONF, encoding="utf-8").read()
    if "XINGHUO_FD" in s:
        print("config.py 已包含 XINGHUO_FD，跳过")
        return
    anchor = '    API_MASTER_KEY: Optional[str] = None\n'
    if anchor not in s:
        print("ERROR: config.py 锚点未找到")
        sys.exit(1)
    add = anchor + (
        '\n'
        '    # --- 星火账号级参数（从 F12 → chat 请求的 Form Data 里取，见 README）---\n'
        '    XINGHUO_FD: Optional[str] = None\n'
        '    XINGHUO_CHAT_ID: Optional[str] = None\n'
    )
    s = s.replace(anchor, add, 1)
    io.open(CONF, "w", encoding="utf-8").write(s)
    print("config.py 已加 XINGHUO_FD / XINGHUO_CHAT_ID")


def patch_provider():
    s = io.open(PROV, encoding="utf-8").read()
    if 'settings.XINGHUO_FD' in s:
        print("provider 已使用 settings.XINGHUO_FD，跳过")
        return
    # 替换硬编码的 fd / chatId
    s2 = s.replace('"fd": "775510",', '"fd": settings.XINGHUO_FD or "775510",', 1)
    s2 = s2.replace('"chatId": "985818485", # 使用抓包获取的 chatId',
                    '"chatId": settings.XINGHUO_CHAT_ID or "985818485",', 1)
    if s2 == s:
        print("ERROR: provider 里没找到硬编码行，未改动")
        sys.exit(1)
    io.open(PROV, "w", encoding="utf-8").write(s2)
    print("provider 已改为读配置")


if __name__ == "__main__":
    patch_config()
    patch_provider()
