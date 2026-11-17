#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""修 xinghuo-2api 的 SSE 解析：同时支持"新版明文 JSON"与"老版 Base64 分片"。

背景（2026-11-17 实测）：
  星火上游现在返回的是
      event:message
      data:{"type":"text","content":"通了", ...}
  而项目里还在 `base64.b64decode(raw_data)`，解码抛异常后被 `except: continue` 吞掉，
  于是所有内容丢失 —— 表现为「HTTP 200 但回复为空」，极易误判成 cookie/鉴权问题。
  （直连抓包确认：上游确实返回了 "通了" 两个字。）
"""
import io
import sys

PROV = "/vol1/1000/docker/xinghuo-2api/app/providers/xinghuo_provider.py"

OLD = """                        try:
                            # 解码 Base64 内容块
                            decoded_bytes = base64.b64decode(raw_data)
                            delta_content = decoded_bytes.decode('utf-8')
"""

NEW = """                        try:
                            # 上游有两种格式，必须都支持：
                            #   · 新版（2026-11 实测）：明文 JSON —
                            #     event:message / data:{"type":"text","content":"通了",...}
                            #   · 老版：Base64 分片
                            # 只按老版解码会抛异常并被下面的 except 吞掉，表现为
                            # 「HTTP 200 但回复为空」（曾误判为 cookie 失效）。
                            _raw = raw_data.lstrip()
                            if _raw.startswith("{"):
                                _obj = json.loads(raw_data)
                                _type = _obj.get("type") or ""
                                # 只取 assistant 的最终文本；reasoning 事件不进正文
                                delta_content = _obj.get("content") or "" if _type == "text" else ""
                            else:
                                decoded_bytes = base64.b64decode(raw_data)
                                delta_content = decoded_bytes.decode('utf-8')
"""


def main():
    s = io.open(PROV, encoding="utf-8").read()
    if "_raw.startswith" in s:
        print("已打过补丁，跳过")
        return
    if OLD not in s:
        print("ERROR: 未找到待替换片段（项目可能已改动）")
        sys.exit(1)
    s = s.replace(OLD, NEW, 1)
    io.open(PROV, "w", encoding="utf-8").write(s)
    print("解析补丁已应用")


if __name__ == "__main__":
    main()
