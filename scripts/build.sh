#!/usr/bin/env bash
set -euo pipefail

DATA_ROOT="/data"
BUILD_ROOT="$DATA_ROOT/build"
UI_ROOT="$DATA_ROOT/ui"
ENV_FILE="$DATA_ROOT/config/phonarch.env"
[[ -f "$ENV_FILE" ]] && set -a && source "$ENV_FILE" && set +a
# Keep the lab/build toolchain inside /data when the host does not provide one.
# Host-installed tools still win when these directories are absent.
for tool_dir in "$DATA_ROOT/toolchain/go/bin" "$DATA_ROOT/toolchain/node/bin" "$DATA_ROOT/toolchains/redis/usr/bin"; do
  [[ -d "$tool_dir" ]] && PATH="$tool_dir:$PATH"
done
export PATH
mkdir -p "$BUILD_ROOT"

need() { command -v "$1" >/dev/null 2>&1 || { echo "missing required tool: $1" >&2; exit 1; }; }
need go
need node
need npm

export GOOS="${GOOS:-linux}"
case "$(uname -m)" in
  x86_64|amd64) NATIVE_GOARCH="amd64" ;;
  aarch64|arm64) NATIVE_GOARCH="arm64" ;;
  *) echo "unsupported host architecture: $(uname -m); set GOARCH explicitly" >&2; exit 1 ;;
esac
export GOARCH="${GOARCH:-$NATIVE_GOARCH}"
export CGO_ENABLED="${CGO_ENABLED:-0}"
BIN_SUFFIX="${GOOS}-${GOARCH}"

echo "Building Go services for $GOOS/$GOARCH"
(cd "$DATA_ROOT/sipgo-lb" && go mod download && go build -trimpath -ldflags='-s -w' -o "$BUILD_ROOT/sipgo-lb-$BIN_SUFFIX" ./cmd/sipgo-lb)
(cd "$DATA_ROOT/sidecar" && go mod download && go build -trimpath -ldflags='-s -w' -o "$BUILD_ROOT/pbx-sidecar-$BIN_SUFFIX" ./cmd/pbx-sidecar)
(cd "$DATA_ROOT/control-api" && go mod download && go build -trimpath -ldflags='-s -w' -o "$BUILD_ROOT/control-api-$BIN_SUFFIX" ./cmd/control-api)
(cd "$DATA_ROOT/rustpbx/heartbeat" && go build -trimpath -ldflags='-s -w' -o "$BUILD_ROOT/rustpbx-heartbeat-$BIN_SUFFIX" .)
(cd "$DATA_ROOT/mock-rustpbx" && go build -trimpath -ldflags='-s -w' -o "$BUILD_ROOT/mock-rustpbx-$BIN_SUFFIX" .)

echo "Building Next.js production bundle"
cd "$UI_ROOT"
if [[ -f package-lock.json ]]; then npm ci; else npm install; fi
npm run build
if [[ -d .next/standalone ]]; then
  mkdir -p .next/standalone/.next
  mkdir -p .next/standalone/.next/static
  cp -a .next/static/. .next/standalone/.next/static/
  if [[ -d public ]]; then
    mkdir -p .next/standalone/public
    cp -a public/. .next/standalone/public/
  fi
fi

echo "Build complete under $BUILD_ROOT"
