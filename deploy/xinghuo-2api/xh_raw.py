#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""直接抓星火上游的原始响应（绕过解析层），判断"200 但空"到底是上游空还是解析丢。

用法：/vol1/1000/docker/xinghuo-2api/venv/bin/python xh_raw.py
"""
import os
import sys

import httpx

ENV = "/vol1/1000/docker/xinghuo-2api/.env"
URL = "https://xinghuo.xfyun.cn/iflygpt-chat/u/chat_message/chat"


def load_env():
    kv = {}
    for line in open(ENV, encoding="utf-8"):
        line = line.strip()
        if not line or line.startswith("#") or "=" not in line:
            continue
        k, _, v = line.partition("=")
        kv[k.strip()] = v.strip()
    return kv


def main():
    e = load_env()
    headers = {
        "Accept": "text/event-stream",
        "Cookie": e.get("XINGHUO_COOKIE", ""),
        "User-Agent": ("Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 "
                       "(KHTML, like Gecko) Chrome/140.0.0.0 Safari/537.36"),
        "Origin": "https://xinghuo.xfyun.cn",
        "Referer": "https://xinghuo.xfyun.cn/desk",
    }
    payload = {
        "fd": e.get("XINGHUO_FD", ""),
        "isBot": "0",
        "clientType": "",
        "text": "只回复两个字：通了",
        "capabilities": "deep_think",
        "chatId": e.get("XINGHUO_CHAT_ID", ""),
        "GtToken": e.get("GT_TOKEN", ""),
    }
    print("fd=", payload["fd"], " chatId=", payload["chatId"],
          " cookie_len=", len(headers["Cookie"]), " gt_len=", len(payload["GtToken"]))
    with httpx.Client(timeout=60, follow_redirects=False) as c:
        with c.stream("POST", URL, headers=headers, data=payload) as r:
            print("HTTP", r.status_code, "content-type=", r.headers.get("content-type"))
            print("set-cookie:", (r.headers.get("set-cookie") or "")[:120])
            n = 0
            for line in r.iter_lines():
                if n >= 25:
                    print("...(截断)")
                    break
                if line.strip():
                    print(repr(line[:220]))
                n += 1


if __name__ == "__main__":
    sys.exit(main())
