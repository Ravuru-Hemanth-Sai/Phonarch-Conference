#!/usr/bin/env bash
set -euo pipefail

DATA_ROOT="/data"
COMPONENTS_ROOT="$DATA_ROOT/components"
UI_ROOT="$COMPONENTS_ROOT/phonarch-platform-web"
ENV_FILE="${PHONARCH_ENV_FILE:-$DATA_ROOT/state/secrets/phonarch-conference.env}"
[[ -f "$ENV_FILE" ]] && set -a && source "$ENV_FILE" && set +a
# Keep the lab/build toolchain inside /data when the host does not provide one.
# Host-installed tools still win when these directories are absent.
for tool_dir in "$DATA_ROOT/toolchain/go/bin" "$DATA_ROOT/toolchain/node/bin" "$DATA_ROOT/toolchains/redis/usr/bin"; do
  [[ -d "$tool_dir" ]] && PATH="$tool_dir:$PATH"
done
export PATH
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
(cd "$COMPONENTS_ROOT/phonarch-sip-gateway" && go mod download && mkdir -p bin && go build -trimpath -ldflags='-s -w' -o "bin/sipgo-lb-$BIN_SUFFIX" ./cmd/sipgo-lb)
(cd "$COMPONENTS_ROOT/phonarch-pbx-sidecar" && go mod download && mkdir -p bin && go build -trimpath -ldflags='-s -w' -o "bin/pbx-sidecar-$BIN_SUFFIX" ./cmd/pbx-sidecar)
(cd "$COMPONENTS_ROOT/phonarch-platform-api" && go mod download && mkdir -p bin && go build -trimpath -ldflags='-s -w' -o "bin/control-api-$BIN_SUFFIX" ./cmd/control-api)
(cd "$COMPONENTS_ROOT/phonarch-rustpbx/heartbeat" && mkdir -p bin && go build -trimpath -ldflags='-s -w' -o "bin/rustpbx-heartbeat-$BIN_SUFFIX" .)
(cd "$COMPONENTS_ROOT/phonarch-rustpbx-lab" && mkdir -p bin && go build -trimpath -ldflags='-s -w' -o "bin/mock-rustpbx-$BIN_SUFFIX" .)

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

echo "Build complete under $COMPONENTS_ROOT/*/bin"
