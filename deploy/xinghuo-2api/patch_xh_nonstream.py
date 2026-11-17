#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""补完 xinghuo-2api 的非流式接口。

原代码在 stream=false 分支里只写了注释和 `pass`：
    full_response = ""
    async for chunk in response_generator:
        pass # 实现聚合逻辑
    return JSONResponse({"content": full_response})
于是任何不经流式的客户端（curl、部分 SDK）都只会拿到 {"content":""}，
看起来像"服务坏了"，其实是这个分支从来没实现。
"""
import io
import sys

MAIN = "/vol1/1000/docker/xinghuo-2api/main.py"

OLD = """            # 如果需要支持非流式，需要在这里聚合流式响应
            # If non-streaming support is needed, the streaming response needs to be aggregated here.
            full_response = ""
            async for chunk in response_generator:
                # 解析 chunk 并累加内容
                # Parse the chunk and accumulate the content
                pass # 实现聚合逻辑
            return JSONResponse({"content": full_response}) # 返回聚合后的内容
"""

NEW = """            # 非流式：把 SSE 分片聚合成标准 OpenAI 响应。
            # 原实现是个空壳（for 循环里只有 pass），任何非流式客户端都只能拿到空内容。
            parts: List[str] = []
            async for chunk in response_generator:
                if not isinstance(chunk, str) or not chunk.startswith("data:"):
                    continue
                payload = chunk[len("data:"):].strip()
                if not payload or payload == "[DONE]":
                    continue
                try:
                    obj = json.loads(payload)
                except Exception:
                    continue
                for ch in obj.get("choices") or []:
                    piece = (ch.get("delta") or {}).get("content")
                    if piece:
                        parts.append(piece)
            text = "".join(parts)
            return JSONResponse({
                "id": f"chatcmpl-{uuid.uuid4().hex}",
                "object": "chat.completion",
                "created": int(time.time()),
                "model": request_data.get("model", "spark-3.5-max"),
                "choices": [{
                    "index": 0,
                    "message": {"role": "assistant", "content": text},
                    "finish_reason": "stop",
                }],
                "usage": {"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0},
            })
"""


def main():
    s = io.open(MAIN, encoding="utf-8").read()
    if "parts: List[str] = []" in s:
        print("已打过补丁，跳过")
        return
    if OLD not in s:
        print("ERROR: 未找到待替换片段（项目可能已改动）")
        sys.exit(1)
    s = s.replace(OLD, NEW, 1)
    # 确保 uuid 已导入（原文件用到 time/json，uuid 可能没有）
    if "import uuid" not in s:
        s = s.replace("import time", "import time\nimport uuid", 1)
    io.open(MAIN, "w", encoding="utf-8").write(s)
    print("非流式补丁已应用")


if __name__ == "__main__":
    main()
