#!/usr/bin/env bash
# =============================================================
#  交叉编译：打成各平台单个可执行文件（无 CGO、无运行时依赖）
#  用法:
#    bash build.sh                     # 编译全部常用平台
#    bash build.sh linux/amd64         # 只编译指定平台
#    PLATFORMS="linux/arm/7" bash build.sh
#  产物输出在 dist/
# =============================================================
set -euo pipefail

NAME="zlion-music-api"
VERSION="${VERSION:-v1.0.0}"
OUT="${OUT:-dist}"
GIT_COMMIT="$(git rev-parse --short HEAD 2>/dev/null || echo unknown)"
BUILD_TIME="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
LDFLAGS="-s -w -X main.version=${VERSION} -X main.commit=${GIT_COMMIT} -X main.buildTime=${BUILD_TIME}"

DEFAULT_PLATFORMS="
linux/amd64
linux/arm64
linux/arm/7
windows/amd64
darwin/arm64
"
if [ $# -gt 0 ]; then
  PLATFORMS="$*"
else
  PLATFORMS="${PLATFORMS:-$DEFAULT_PLATFORMS}"
fi

command -v go >/dev/null 2>&1 || { echo "未检测到 Go，请先安装: https://go.dev/dl/"; exit 1; }
command -v gofmt >/dev/null 2>&1 && gofmt -l . || true

mkdir -p "$OUT"
echo "编译目标: $(echo $PLATFORMS)"
echo

for p in $PLATFORMS; do
  GOOS="${p%%/*}"
  rest="${p#*/}"
  GOARCH="${rest%%/*}"
  variant=""
  [ "$rest" != "$GOARCH" ] && variant="${rest#*/}"

  export CGO_ENABLED=0 GOOS GOARCH
  unset GOARM GOAMD64 2>/dev/null || true
  suffix="${GOOS}_${GOARCH}"
  if [ "$GOARCH" = "arm" ]; then
    if [ -n "$variant" ]; then export GOARM="$variant"; else export GOARM=7; fi
    suffix="${suffix}v${GOARM}"
  fi

  bin="$OUT/${NAME}_${suffix}"
  [ "$GOOS" = "windows" ] && bin="${bin}.exe"

  printf '  -> %-32s' "$(basename "$bin")"
  if go build -trimpath -ldflags "$LDFLAGS" -o "$bin" . 2>/tmp/goerr; then
    printf 'OK  %s\n' "$(du -h "$bin" | cut -f1)"
  else
    printf '失败\n'; cat /tmp/goerr; exit 1
  fi
done

echo
echo "产物目录: $OUT"
ls -lh "$OUT" | tail -n +2 | awk '{printf "  %-34s %s\n", $9, $5}'
echo
echo "部署示例（Linux 服务器）:"
echo "  scp $OUT/${NAME}_linux_amd64 root@server:/usr/local/bin/zlion-music-api"
echo "  scp source.example.js       root@server:/opt/zlion-music-api/source.js"
echo "  ssh root@server 'chmod +x /usr/local/bin/zlion-music-api'"
echo "  然后参考 deploy/zlion-music-api.service 起服务"
