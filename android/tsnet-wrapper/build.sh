#!/usr/bin/env bash
# tsnet-wrapper build script (V2 Phase 1)
# Host-agnostic — 不写死 linux-x86_64,靠 uname 判定
# 用法:
#   bash build.sh              # 默认 arm64-only,产 tsnet-wrapper-arm64.aar
#   bash build.sh fat          # 全 4 架构,产 tsnet-wrapper-fat.aar
set -euo pipefail

# ------------------------------------------------------------
# 环境固化(即使调用者没 source gradle-env.sh 也能跑)
# ------------------------------------------------------------
export ANDROID_HOME="${ANDROID_HOME:-$HOME/Library/Android/sdk}"
export ANDROID_NDK_HOME="${ANDROID_NDK_HOME:-$ANDROID_HOME/ndk/android-ndk-r27c}"
export GOPROXY="${GOPROXY:-https://goproxy.cn,https://proxy.golang.org,direct}"
export PATH="$HOME/go/bin:$PATH"

HOST_OS="$(uname -s | tr '[:upper:]' '[:lower:]')"       # darwin / linux
HOST_ARCH="x86_64"                                       # NDK 官方分发仅提供 x86_64 host toolchain
NDK_HOST="${HOST_OS}-${HOST_ARCH}"
TOOLCHAIN_BIN="$ANDROID_NDK_HOME/toolchains/llvm/prebuilt/$NDK_HOST/bin"

# ------------------------------------------------------------
# Preflight
# ------------------------------------------------------------
if [ ! -d "$ANDROID_NDK_HOME" ]; then
    echo "❌ ANDROID_NDK_HOME not found: $ANDROID_NDK_HOME" >&2
    echo "   Install with: sdkmanager 'ndk;27.0.12077973' 或下载 android-ndk-r27c-<host>.zip" >&2
    exit 1
fi

if [ ! -d "$TOOLCHAIN_BIN" ]; then
    echo "❌ NDK toolchain not found: $TOOLCHAIN_BIN" >&2
    echo "   Expected host = $NDK_HOST — 若你在其它 host 上跑,NDK 也要装对应的 host 版本" >&2
    exit 1
fi

if ! command -v gomobile >/dev/null 2>&1; then
    echo "❌ gomobile not on PATH. Install:" >&2
    echo "   go install golang.org/x/mobile/cmd/gomobile@latest" >&2
    echo "   go install golang.org/x/mobile/cmd/gobind@latest" >&2
    echo "   gomobile init" >&2
    exit 1
fi

# ------------------------------------------------------------
# Targets
# ------------------------------------------------------------
MODE="${1:-arm64}"
case "$MODE" in
    arm64)
        TARGET="android/arm64"
        OUT="tsnet-wrapper-arm64.aar"
        ;;
    fat)
        TARGET="android/arm,android/arm64,android/386,android/amd64"
        OUT="tsnet-wrapper-fat.aar"
        ;;
    *)
        echo "❌ unknown mode: $MODE (use arm64 or fat)" >&2
        exit 1
        ;;
esac

# ------------------------------------------------------------
# Build
# ------------------------------------------------------------
cd "$(dirname "$0")"

echo "[1/2] go mod tidy..."
go mod tidy

echo "[2/2] gomobile bind ($MODE)..."
gomobile bind \
    -target="$TARGET" \
    -androidapi 24 \
    -ldflags="-s -w -checklinkname=0" \
    -trimpath \
    -o "$OUT" \
    .

# ------------------------------------------------------------
# Report
# ------------------------------------------------------------
SIZE_HUMAN="$(ls -lh "$OUT" | awk '{print $5}')"
echo
echo "✅ Done: $OUT  ($SIZE_HUMAN)"
echo "   Contents:"
unzip -l "$OUT" | grep -E "classes\.jar|\.so$" | awk '{printf "     %10s  %s\n", $1, $4}'
