# xinghuo-2api 补丁集（飞牛 NAS 实测 2026-11-17）

上游项目：https://github.com/lzA6/xinghuo-2api （Apache-2.0）
部署位置：`/vol1/1000/docker/xinghuo-2api`（不用 Docker，走 NAS 自带 python312 + venv）

## 为什么需要这些补丁

原项目能跑起来但**回复永远是空**，逐层排下去发现是三处独立问题叠在一起：

| 补丁 | 解决的问题 | 症状 |
|---|---|---|
| `patch_xh_fd.py` | `_prepare_payload` 写死了作者自己的 `fd` / `chatId`，改成读 `.env` | 用自己的 Cookie 请求，HTTP 200 但内容空 |
| `patch_xh_parser.py` | **上游 SSE 格式已从 Base64 分片变成明文 JSON**，老解析器 `base64.b64decode` 抛异常被 `except` 吞掉 | 同上，且日志里连错误都没有 |
| `patch_xh_nonstream.py` | `stream=false` 分支原本是空壳（`for` 里只有 `pass`），补成真正的聚合 | 非流式客户端只拿到 `{"content":""}` |

配套脚本：

- `xh_start.sh`：`setsid` + 三路重定向常驻启动（判活用 `grep 8082`，不要写 `grep ':8082 '`）
- `xh_raw.py`：**直连抓上游原始 SSE**，用于区分「上游没回」还是「解析丢了」——排这类问题第一步就该跑它

## 用法

```sh
# 依次打补丁（幂等，重复跑会跳过）→ 重启
python3 patch_xh_fd.py
python3 patch_xh_parser.py
python3 patch_xh_nonstream.py
sh xh_start.sh

# 诊断：先看上游到底回了什么
/vol1/1000/docker/xinghuo-2api/venv/bin/python xh_raw.py
```

## 凭证

`.env` 需要四项，全部从 F12 → Network → `chat` 请求里取：

```
XINGHUO_COOKIE=整段 cookie
GT_TOKEN=Form Data 的 GtToken
XINGHUO_FD=Form Data 的 fd      # 注意：这个值**会随会话变**
XINGHUO_CHAT_ID=Form Data 的 chatId  # 相对稳定
API_MASTER_KEY=自己定的（网关那边当 api_key 用）
```

## 现状（2026-11-17）

- 经网关 `customopenai/spark-*` 调用**正常**（实测返回「通了」「2」）
- 直连 `/v1/chat/completions` 的**非流式仍为空**（流式正常）——上游项目这处有缺陷，
  聚合补丁打上后依然为空，原因未定位；网关侧一律走流式，不受影响
