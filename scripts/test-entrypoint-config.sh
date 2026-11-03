#!/bin/bash
# docker/entrypoint.sh 的 config.json 归属权回归测试。
#
# 钉住三条（对应「面板改的 API-Key / 管理密码重启后被回滚」这个用户可见缺陷）：
#   1. 首次启动才生成 config.json（且 0600）；
#   2. 已存在时**绝不重写**——面板改过的值必须活过重启；
#   3. 已存在时把凭据类环境变量 unset 掉，让 applyEnv 不再覆盖文件（applyEnv 只在
#      环境变量非空时才覆盖，所以 unset 即让位）；同时「对外暴露且没密码」仍必须拒绝启动。
#
# 用 WILDWORK_BIN 把最后的 exec 换成桩，脚本才能被测到（否则 exec 会把测试进程换掉）。
# 用法：bash scripts/test-entrypoint-config.sh
set -u

EP="docker/entrypoint.sh"
[ -f "$EP" ] || { echo "找不到 $EP（请在仓库根目录运行）"; exit 1; }

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT
fails=0
ok()  { echo "  ✓ $1"; }
bad() { echo "  ✗ $1"; fails=$((fails + 1)); }

# 桩：把收到的环境变量存下来，然后正常退出（替代真实二进制）
cat > "$TMP/bin.sh" <<'EOF'
#!/bin/sh
env | grep '^WILDWORK_' | sort > "$STUB_OUT"
exit 0
EOF
chmod +x "$TMP/bin.sh"

run_ep() { # run_ep <workdir> [ENV=VAL ...]
    local dir="$1"; shift
    env -i PATH="/usr/bin:/bin" \
        WILDWORK_BIN="$TMP/bin.sh" STUB_OUT="$TMP/env.txt" \
        WILDWORK_HOME="$dir" \
        WILDWORK_AUTH_DIR="$dir/auths" \
        WILDWORK_STATE_FILE="$dir/state.json" \
        WILDWORK_REGION="cn" \
        "$@" \
        bash "$EP" > "$TMP/out.txt" 2>&1
}

# ---------- 1. 首次启动：生成配置 ----------
D1="$TMP/d1"; mkdir -p "$D1"
run_ep "$D1" WILDWORK_LISTEN=":7863" WILDWORK_ADMIN_PASSWORD="firstpass"
rc=$?
if [ "$rc" = 0 ] && [ -f "$D1/config.json" ]; then
    ok "首次启动生成 config.json（rc=0）"
else
    bad "首次启动未生成配置（rc=$rc）"; cat "$TMP/out.txt"
fi
grep -q '"admin_password": "firstpass"' "$D1/config.json" \
    && ok "首次启动把密码写进配置" || bad "首次启动没写密码"
case "$(uname -s 2>/dev/null)" in
    Linux|Darwin) [ "$(stat -c '%a' "$D1/config.json" 2>/dev/null || stat -f '%Lp' "$D1/config.json")" = "600" ] \
        && ok "首次生成的文件权限是 600" || bad "首次生成的文件权限不是 600" ;;
    *) echo "  - 跳过权限断言（当前环境不保留 POSIX 位）" ;;
esac

# ---------- 2. 二次启动：不重写，面板改过的值保留 ----------
python - "$D1/config.json" <<'PY'
import json, sys
p = sys.argv[1]
d = json.load(open(p, encoding="utf-8"))
d["api_key"] = "panel-key-2222"
d["admin_password"] = "panel-pass-2222"
json.dump(d, open(p, "w", encoding="utf-8"), ensure_ascii=False, indent=2)
PY
before=$(cat "$D1/config.json")
run_ep "$D1" WILDWORK_LISTEN=":7863"   # 注意：这次**不给**密码环境变量
rc=$?
if [ "$rc" = 0 ] && [ "$before" = "$(cat "$D1/config.json")" ]; then
    ok "二次启动未改动 config.json（面板改的 api_key/密码保住了）"
else
    bad "二次启动重写了配置或被拒（rc=$rc）"; cat "$TMP/out.txt"
fi
# 凭据类环境变量必须已被 unset，否则 applyEnv 会再覆盖一次
if grep -q '^WILDWORK_ADMIN_PASSWORD=' "$TMP/env.txt"; then
    bad "已存在配置时仍把 WILDWORK_ADMIN_PASSWORD 传给了程序（会覆盖面板值）"
else
    ok "已存在配置时凭据类环境变量已 unset"
fi
grep -q '^WILDWORK_LISTEN=' "$TMP/env.txt" \
    && ok "LISTEN 仍由环境变量控制（容器内必须绑 0.0.0.0）" \
    || bad "LISTEN 环境变量丢了（容器内可能绑回环导致访问不到）"

# ---------- 3. 对外监听 + 环境变量与文件都没有密码 → 必须拒绝启动 ----------
D3="$TMP/d3"; mkdir -p "$D3"
cat > "$D3/config.json" <<'EOF'
{
  "listen": ":7863",
  "api_key": "k",
  "admin_password": "",
  "auth_dir": "/x/auths"
}
EOF
run_ep "$D3" WILDWORK_LISTEN=":7863"
rc=$?
if [ "$rc" != 0 ]; then
    ok "对外监听且无密码时拒绝启动（rc=$rc）"
else
    bad "对外监听且无密码竟然启动了（安全闸失效）"; cat "$TMP/out.txt"
fi

# ---------- 4. 语法 ----------
if bash -n "$EP" 2>/dev/null; then ok "$EP 语法检查通过"; else bad "$EP 语法错误"; bash -n "$EP"; fi

echo
if [ "$fails" -eq 0 ]; then echo "全部通过"; else echo "失败 $fails 项"; exit 1; fi
