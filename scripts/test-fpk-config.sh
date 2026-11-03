#!/bin/bash
# fpk 启动脚本的 config.json 处理回归测试。
#
# 钉住三件事（都是 v0.1.10 实发事故的直接教训）：
#   1. 脚本里**不再有按行 sed 回写 config.json** 的代码 —— 那是把 JSON 写坏的根源：
#      程序用 MarshalIndent 把 listen（{host,port} 结构体）落成多行对象，按行替换必留
#      孤儿行；而且 sed 替换串里的 & 会展开成整行匹配、值含 | 会静默不生效（均已实测）。
#   2. repair_listen_block 能把旧版写坏的文件自愈回合法 JSON，各种收尾形态都吃得下。
#   3. 自愈绝不碰健康文件、绝不放宽权限（config.json 里有 api_key 与管理面板密码）。
#
# 用法：bash scripts/test-fpk-config.sh
set -u

MAIN="fpk/wb2apifusion/cmd/main"
[ -f "$MAIN" ] || { echo "找不到 $MAIN（请在仓库根目录运行）"; exit 1; }

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT
fails=0

# 从真实脚本里抽出被测函数 —— 避免「测试里另写一份实现」而与实现漂移。
eval "$(awk '/^repair_listen_block\(\)/,/^}/' "$MAIN")"
log_msg() { :; }   # 被测函数会调它，测试里静默

# 函数必须真的抽到，否则调用被静默忽略、用例会「空过假绿」（这个坑实测踩过）。
type repair_listen_block >/dev/null 2>&1 || { echo "  ✗ 在 $MAIN 里找不到 repair_listen_block"; exit 1; }

ok()  { echo "  ✓ $1"; }
bad() { echo "  ✗ $1"; fails=$((fails + 1)); }

# 一律用 argv 传路径，不要把路径插进 python 源码里（反斜杠会被当转义）。
valid_json() { python -c 'import json,sys; json.load(open(sys.argv[1], encoding="utf-8"))' "$1" >/dev/null 2>&1; }
listen_of()  { python -c 'import json,sys; print(json.load(open(sys.argv[1], encoding="utf-8"))["listen"])' "$1" 2>/dev/null; }
mode_of()    { python -c 'import os,stat,sys; print(oct(stat.S_IMODE(os.stat(sys.argv[1]).st_mode)))' "$1" 2>/dev/null; }

# ---------- 0. 结构护栏：不许再有按行回写 ----------
if grep -q 'patch_top_level' "$MAIN"; then
    bad "脚本里仍存在 patch_top_level（按行 sed 回写是事故根源，应彻底移除）"
else
    ok "脚本已无 patch_top_level"
fi
if grep -q 'sed -i .*CFG_FILE' "$MAIN"; then
    bad "脚本里仍有对 config.json 的 sed -i 原地改写"
else
    ok "脚本不再对 config.json 做 sed 原地改写"
fi
grep -q 'chmod 600 "$CFG_FILE"' "$MAIN" \
    && ok "首次创建后显式 chmod 600" || bad "缺少 chmod 600（凭据文件不能默认 0644）"
grep -q 'check_process "$pid"' "$MAIN" \
    && ok "启动后校验进程存活（不再无条件 return 0）" \
       || bad "start_process 未校验进程是否真的起来"

# ---------- 1. 孤儿块：收尾带逗号 ----------
cat > "$TMP/a.json" <<'EOF'
{
  "listen": ":7865",
    "host": "127.0.0.1",
    "port": 7863
  },
  "api_key": "k",
  "region": "cn"
}
EOF
CFG_FILE="$TMP/a.json"; repair_listen_block
if valid_json "$TMP/a.json" && [ "$(listen_of "$TMP/a.json")" = ":7865" ]; then
    ok "收尾带逗号的孤儿块可自愈"
else
    bad "带逗号收尾自愈失败"; cat "$TMP/a.json"
fi

# ---------- 2. 孤儿块：收尾不带逗号（旧实现修不好这条） ----------
printf '{\n  "listen": ":7865",\n    "host": "a",\n    "port": 1\n  }\n}\n' > "$TMP/b.json"
CFG_FILE="$TMP/b.json"; repair_listen_block
if valid_json "$TMP/b.json" && [ "$(listen_of "$TMP/b.json")" = ":7865" ]; then
    ok "收尾无逗号的孤儿块可自愈"
else
    bad "无逗号收尾自愈失败"; cat "$TMP/b.json"
fi

# ---------- 3. CRLF 行尾 ----------
printf '{\r\n  "listen": ":7865",\r\n    "host": "a",\r\n    "port": 1\r\n  },\r\n  "api_key": "k"\r\n}\r\n' > "$TMP/c.json"
CFG_FILE="$TMP/c.json"; repair_listen_block
valid_json "$TMP/c.json" && ok "CRLF 文件可自愈" || { bad "CRLF 自愈失败"; cat "$TMP/c.json"; }

# ---------- 4. 多个孤儿块 ----------
printf '{\n  "listen": ":7865",\n    "host": "a",\n    "port": 1\n  },\n  "api_key": "k",\n  "listen": ":9",\n    "host": "b",\n    "port": 2\n  }\n}\n' > "$TMP/d.json"
CFG_FILE="$TMP/d.json"; repair_listen_block
valid_json "$TMP/d.json" && ok "两处孤儿块都能清理且 JSON 合法" || { bad "多孤儿块自愈失败"; cat "$TMP/d.json"; }

# ---------- 5. 健康文件：对象形态 listen 绝不能被动 ----------
cat > "$TMP/obj.json" <<'EOF'
{
  "listen": {
    "host": "127.0.0.1",
    "port": 7863
  },
  "api_key": "k",
  "region": "cn"
}
EOF
cp "$TMP/obj.json" "$TMP/obj.ref"
CFG_FILE="$TMP/obj.json"; repair_listen_block; repair_listen_block
if diff -q "$TMP/obj.json" "$TMP/obj.ref" >/dev/null; then
    ok "对象形态 listen 逐字节未被改动（不会把正常配置删没）"
else
    bad "对象形态 listen 被误删！"; cat "$TMP/obj.json"
fi

# ---------- 6. 健康文件：标量 listen 后紧跟正常键，也不该动 ----------
printf '{\n  "listen": ":7865",\n  "api_key": "k"\n}\n' > "$TMP/sc.json"
cp "$TMP/sc.json" "$TMP/sc.ref"
CFG_FILE="$TMP/sc.json"; repair_listen_block
if diff -q "$TMP/sc.json" "$TMP/sc.ref" >/dev/null; then
    ok "标量 listen + 正常后继键未被改动"
else
    bad "健康标量文件被改动"; cat "$TMP/sc.json"
fi

# ---------- 7. 权限与备份 ----------
cat > "$TMP/p.json" <<'EOF'
{
  "listen": ":7865",
    "host": "a",
    "port": 1
  },
  "api_key": "k"
}
EOF
chmod 600 "$TMP/p.json"
CFG_FILE="$TMP/p.json"; repair_listen_block
# 权限断言只在真支持 POSIX 位的环境上有效（msys/Windows 上 stat 一律报 0666，
# 而这段脚本实际跑在飞牛 NAS 的 Linux 上）—— 在 Windows 上强行断言会假红。
case "$(uname -s 2>/dev/null)" in
    Linux|Darwin|FreeBSD)
        case "$(mode_of "$TMP/p.json")" in
            *0o600) ok "自愈后权限仍是 0600（未放宽成 0644）" ;;
            *) bad "自愈后权限变成 $(mode_of "$TMP/p.json") —— config.json 含凭据，必须保持 0600" ;;
        esac ;;
    *)
        echo "  - 跳过权限断言（当前环境不保留 POSIX 位；该检查在 NAS/Linux 上生效）" ;;
esac
if [ -f "$TMP/p.json.bak" ]; then ok "自愈前留了 config.json.bak 备份"; else bad "缺少备份文件"; fi

# ---------- 8. 幂等（真的比对内容，而不是无条件报成功） ----------
before=$(cat "$TMP/p.json")
CFG_FILE="$TMP/p.json"; repair_listen_block
if [ "$before" = "$(cat "$TMP/p.json")" ]; then
    ok "重复执行幂等"
else
    bad "第二次执行又改了文件"
fi

# ---------- 9. 语法 ----------
if bash -n "$MAIN" 2>/dev/null; then
    ok "$MAIN 语法检查通过"
else
    bad "$MAIN 语法错误"; bash -n "$MAIN"
fi

echo
if [ "$fails" -eq 0 ]; then
    echo "全部通过"
else
    echo "失败 $fails 项"
    exit 1
fi
