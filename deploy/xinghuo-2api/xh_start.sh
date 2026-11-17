#!/bin/sh
# 启动星火反代（uvicorn），后台常驻。
# 用 setsid + 三路重定向彻底脱离 SSH 会话，避免 ssh 断开时进程被带走、
# 也避免 SSH 通道因为子进程持有 stdout 而挂住（此前踩过：命令看似无输出、实际超时）。
D=/vol1/1000/docker/xinghuo-2api
pkill -f "uvicorn main:app --host" 2>/dev/null
sleep 1
cd "$D" || exit 1
setsid "$D/venv/bin/uvicorn" main:app --host 0.0.0.0 --port 8082 \
    > "$D/run.log" 2>&1 < /dev/null &
sleep 5
if ss -ltn 2>/dev/null | grep -q 8082; then
    echo "STARTED"
else
    echo "FAILED"
    tail -8 "$D/run.log" 2>/dev/null
fi
