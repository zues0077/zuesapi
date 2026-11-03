# workbuddy-api 极简运行镜像。
#
# 设计原则：镜像内**不编译、不拉依赖**，只 COPY 开发机交叉编译好的单个静态二进制 ——
# 彻底规避在 NAS 上 go mod download 访问 proxy.golang.org 超时（i/o timeout）。
# 那是同类项目在容器里现场构建时最常见的失败点。
#
# 二进制 workbuddy-api 由开发机交叉编译（GOOS=linux GOARCH=amd64 CGO_ENABLED=0）：
# 单进程 = OpenAI 兼容 HTTP 服务 + 双调度器 + 静态 Web 管理面板；
# 容器内以 --no-tray 无头运行（容器无桌面环境）。
FROM alpine:3.20

# ★ 零 apk 依赖（全程不做网络拉取，离线也能构建）：
#   ca-certificates → minirootfs 已内置 /etc/ssl/certs/ca-certificates.crt（访问上游 HTTPS 必需）
#   sh / ash / wget → busybox 自带
#   tzdata          → **已内嵌进二进制**（import _ "time/tzdata"），无需系统提供 zoneinfo。
#                     若缺它，TZ=Asia/Shanghai 无法解析、time.Local 静默退化为 UTC，
#                     定时签到会按 UTC 时刻触发（北京时间 17:00/05:00 而非 09:00/21:00）。
#   jq              → 不需要：账号登录走 Web 面板（/api/login/start），容器内无需 CLI 助手。

WORKDIR /app

COPY workbuddy-api /app/workbuddy-api
COPY entrypoint.sh /app/entrypoint.sh
RUN chmod +x /app/workbuddy-api /app/entrypoint.sh

# 只把「数据目录」声明为卷。**绝不声明或挂载单个文件** ——
# 历史上曾把 config.json 作为单文件 bind 挂载，当宿主上该文件不存在时
# Docker 会建出一个同名**空目录**，程序读配置即崩（现象是容器 Exited:0）。
VOLUME ["/data"]
EXPOSE 7863

# WILDWORK_HOME=/data：让 config.json 落进**持久卷**，容器重建后面板改过的
# api_key / 管理密码不再丢（旧值 /app 在容器层，每次重建都退回 entrypoint
# 按环境变量重写的那份 —— 这就是「面板改了密码、重启又变回去」的成因之一）。
# 其余配置仍可由 WILDWORK_* 覆盖；但 entrypoint 在 config.json 已存在时会
# unset 凭据类变量，把 api_key / admin_password 的所有权交还给文件。
ENV WILDWORK_HOME="/data" \
    WILDWORK_LISTEN=":7863" \
    WILDWORK_AUTH_DIR="/data/auths" \
    WILDWORK_STATE_FILE="/data/state.json" \
    WILDWORK_REGION="cn" \
    TZ="Asia/Shanghai"

# /healthz 无需鉴权，探针不带 API Key。
HEALTHCHECK --interval=30s --timeout=5s --retries=3 --start-period=15s \
    CMD wget -q -O - http://127.0.0.1:7863/healthz || exit 1

ENTRYPOINT ["/app/entrypoint.sh"]
