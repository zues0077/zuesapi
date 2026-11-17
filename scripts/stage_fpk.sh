#!/usr/bin/env bash
# 本地暂存 fpk 目录树（对应 CI 的 Stage fpk tree + Fill manifest 两步）。
# 之后把 wb2apifusion/ 传到一个 Linux 机器上跑 `fnpack build` 即可 —— fnpack 只有 linux 版。
set -euo pipefail
cd "$(dirname "$0")/.."

PLATFORM="${1:-x86}"     # x86 | arm
GOARCH="${2:-amd64}"
VERSION=$(grep -oE 'const Version = "[0-9.]+"' internal/app/app.go | grep -oE '[0-9.]+')

echo "版本 $VERSION / 平台 $PLATFORM / 架构 $GOARCH"

GOOS=linux GOARCH="$GOARCH" CGO_ENABLED=0 \
  go build -trimpath -ldflags "-s -w" -o "dist/workbuddy-api-linux-$GOARCH" ./cmd/workbuddy-api

rm -rf "stage-$PLATFORM" "stage-$PLATFORM/wb2apifusion"
mkdir -p "stage-$PLATFORM"
cp -r fpk/wb2apifusion "stage-$PLATFORM/wb2apifusion"
APP="stage-$PLATFORM/wb2apifusion"
mkdir -p "$APP/app/bin"
cp "dist/workbuddy-api-linux-$GOARCH" "$APP/app/bin/workbuddy-api"
chmod 755 "$APP/app/bin/workbuddy-api"
cp config.example.json "$APP/app/config.example.json"
chmod 755 "$APP"/cmd/*

python3 - "$APP/manifest" "$VERSION" "$PLATFORM" <<'PY'
import sys
path, version, platform = sys.argv[1:4]
s = open(path, encoding="utf-8").read()
s = s.replace("${VERSION}", version).replace("${PLATFORM}", platform)
s = s.replace("${CHANGELOG}", f"zuesapi {version}（WebAPI 凭证接入集中修复：导入 404、"
                           f"多实例模型缺席、热生效、按钮语义；移除 gemini/通义不可用源）")
open(path, "w", encoding="utf-8").write(s)
print("manifest 已填充")
PY

echo "暂存完成：$APP"
du -sh "$APP"
