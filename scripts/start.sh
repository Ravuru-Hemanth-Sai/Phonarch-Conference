#!/usr/bin/env bash
set -euo pipefail

DATA_ROOT="/data"
RUNTIME="$DATA_ROOT/runtime"
BUILD="$DATA_ROOT/build"
ENV_FILE="$DATA_ROOT/config/phonarch.env"
[[ -f "$ENV_FILE" ]] && set -a && source "$ENV_FILE" && set +a
for tool_dir in "$DATA_ROOT/toolchain/node/bin" "$DATA_ROOT/toolchain/go/bin" "$DATA_ROOT/toolchains/redis/usr/bin"; do
  [[ -d "$tool_dir" ]] && PATH="$tool_dir:$PATH"
done
export PATH

case "${PHONARCH_BIN_SUFFIX:-$(uname -m)}" in
  x86_64|amd64) BIN_SUFFIX="linux-amd64" ;;
  aarch64|arm64) BIN_SUFFIX="linux-arm64" ;;
  linux-*) BIN_SUFFIX="${PHONARCH_BIN_SUFFIX}" ;;
  *) echo "unsupported host architecture: $(uname -m)" >&2; exit 1 ;;
esac

mkdir -p "$RUNTIME/logs" "$RUNTIME/pids" "$RUNTIME/redis" "$RUNTIME/audit"
log() { echo "[$(date -u +%FT%TZ)] $*"; }
require_file() { [[ -x "$1" ]] || { echo "missing executable: $1; run /data/scripts/build.sh" >&2; exit 1; }; }
start_bg() { local name="$1"; shift; log "starting $name"; nohup "$@" >>"$RUNTIME/logs/$name.log" 2>&1 & echo $! >"$RUNTIME/pids/$name.pid"; }
sanitize_id() { printf '%s' "$1" | tr '.:/' '---' | tr -cd '[:alnum:]-'; }
host_part() { case "$1" in \[*\]:*) printf '%s' "${1#\[}" | cut -d']' -f1 ;; *:*) printf '%s' "${1%:*}" ;; *) printf '%s' "$1" ;; esac; }
http_url() { case "$1" in http://*|https://*) printf '%s' "$1" ;; *) printf 'http://%s' "$1" ;; esac; }

command -v redis-server >/dev/null 2>&1 || { echo "redis-server is required as a native host service" >&2; exit 1; }
command -v redis-cli >/dev/null 2>&1 || { echo "redis-cli is required as a native host service" >&2; exit 1; }
command -v node >/dev/null 2>&1 || { echo "node is required to run the Next.js UI" >&2; exit 1; }
require_file "$BUILD/sipgo-lb-$BIN_SUFFIX"
require_file "$BUILD/pbx-sidecar-$BIN_SUFFIX"
require_file "$BUILD/control-api-$BIN_SUFFIX"

redis_ready=0
for _ in {1..4}; do
  if redis-cli -h "${REDIS_HOST:-127.0.0.1}" -p "${REDIS_PORT:-6379}" ping >/dev/null 2>&1; then
    redis_ready=1
    break
  fi
  sleep 0.25
done
if [[ "$redis_ready" -eq 0 ]]; then
  start_bg redis redis-server --bind "${REDIS_HOST:-127.0.0.1}" --port "${REDIS_PORT:-6379}" --dir "$RUNTIME/redis" --dbfilename dump.rdb --appendonly yes
  for _ in {1..20}; do
    if redis-cli -h "${REDIS_HOST:-127.0.0.1}" -p "${REDIS_PORT:-6379}" ping >/dev/null 2>&1; then
      redis_ready=1
      break
    fi
    sleep 0.25
  done
fi
[[ "$redis_ready" -eq 1 ]] || { echo "Redis did not become ready" >&2; exit 1; }

export REDIS_ADDR="${REDIS_ADDR:-${REDIS_HOST:-127.0.0.1}:${REDIS_PORT:-6379}}"

# The number of edge processes is the number of configured bind addresses.
# IDs are derived from private addresses unless the deployment supplies IDs.
IFS=',' read -r -a SIPGO_BINDS <<< "${SIPGO_BIND_ADDRESSES:-${SIPGO_LISTEN:-0.0.0.0:5060}}"
IFS=',' read -r -a SIPGO_TCP_BINDS <<< "${SIPGO_TCP_BIND_ADDRESSES:-${SIPGO_TCP_LISTEN:-${SIPGO_LISTEN:-0.0.0.0:5060}}}"
IFS=',' read -r -a SIPGO_HTTP_BINDS <<< "${SIPGO_HTTP_ADDRESSES:-${SIPGO_HTTP:-127.0.0.1:8080}}"
IFS=',' read -r -a SIPGO_ADVERTISE_IPS <<< "${SIPGO_ADVERTISE_IPS:-}"
IFS=',' read -r -a SIPGO_PUBLIC_IPS <<< "${SIPGO_PUBLIC_IPS:-}"
IFS=',' read -r -a SIPGO_NODE_IDS <<< "${SIPGO_NODE_IDS:-}"
edge_count=${#SIPGO_BINDS[@]}
[[ "$edge_count" -gt 0 ]] || { echo "SIPGO_BIND_ADDRESSES must contain at least one address" >&2; exit 1; }
[[ "${#SIPGO_TCP_BINDS[@]}" -eq "$edge_count" && "${#SIPGO_HTTP_BINDS[@]}" -eq "$edge_count" ]] || { echo "SIPGo bind, TCP, and HTTP address lists must have equal lengths" >&2; exit 1; }

edge_http_targets=$(IFS=,; echo "${SIPGO_HTTP_BINDS[*]}")
export SIPGO_HTTP="${SIPGO_HTTP_BINDS[0]}"
export SIPGO_HTTP_TARGETS="${SIPGO_HTTP_TARGETS:-$edge_http_targets}"
for index in "${!SIPGO_BINDS[@]}"; do
  bind=${SIPGO_BINDS[$index]}
  tcp_bind=${SIPGO_TCP_BINDS[$index]}
  http_bind=${SIPGO_HTTP_BINDS[$index]}
  advertise_ip=${SIPGO_ADVERTISE_IPS[$index]-$(host_part "$bind")}
  public_ip=${SIPGO_PUBLIC_IPS[$index]-}
  edge_id=${SIPGO_NODE_IDS[$index]-edge-$(sanitize_id "$advertise_ip")}
  label="sipgo-$(sanitize_id "$advertise_ip")"
  start_bg "$label" env REDIS_ADDR="$REDIS_ADDR" SIPGO_NODE_ID="$edge_id" SIPGO_ADVERTISE_IP="$advertise_ip" SIPGO_PUBLIC_IP="$public_ip" SIPGO_LISTEN="$bind" SIPGO_TCP_LISTEN="$tcp_bind" SIPGO_HTTP="$http_bind" SIPGO_ADVERTISE_HTTP="$(http_url "$http_bind")" "$BUILD/sipgo-lb-$BIN_SUFFIX"
done

export CONTROL_API_LISTEN="${CONTROL_API_LISTEN:-127.0.0.1:8081}"
[[ -n "${DATABASE_URL:-}" ]] || { echo "DATABASE_URL must be configured" >&2; exit 1; }
start_bg control-api "$BUILD/control-api-$BIN_SUFFIX"

# The number of core servers is the number of configured private addresses.
# The sidecar discovers active SipGo addresses from Redis; no edge address is
# copied into each core command line.
IFS=',' read -r -a CORE_PRIVATE_IPS <<< "${CORE_PRIVATE_IPS:-127.0.0.1}"
IFS=',' read -r -a CORE_MEDIA_PUBLIC_IPS <<< "${CORE_MEDIA_PUBLIC_IPS:-}"
IFS=',' read -r -a CORE_SIP_PORTS <<< "${CORE_SIP_PORTS:-5071}"
IFS=',' read -r -a CORE_RUSTPBX_CONTROLS <<< "${CORE_RUSTPBX_CONTROL_ADDRESSES:-127.0.0.1:9091}"
IFS=',' read -r -a CORE_SIDECAR_BINDS <<< "${CORE_SIDECAR_ADDRESSES:-127.0.0.1:9441}"
IFS=',' read -r -a CORE_ROLES <<< "${CORE_ROLES:-listener}"
IFS=',' read -r -a RUSTPBX_CONFIGS <<< "${RUSTPBX_CONFIG_PATHS:-}"
core_count=${#CORE_PRIVATE_IPS[@]}
[[ "$core_count" -gt 0 ]] || { echo "CORE_PRIVATE_IPS must contain at least one address" >&2; exit 1; }
[[ "${#CORE_MEDIA_PUBLIC_IPS[@]}" -eq "$core_count" && "${#CORE_SIP_PORTS[@]}" -eq "$core_count" && "${#CORE_RUSTPBX_CONTROLS[@]}" -eq "$core_count" && "${#CORE_SIDECAR_BINDS[@]}" -eq "$core_count" ]] || { echo "core IP, media IP, SIP port, control, and sidecar lists must have equal lengths" >&2; exit 1; }

RUSTPBX_BIN="${RUSTPBX_BIN:-}"
if [[ -n "$RUSTPBX_BIN" && -x "$RUSTPBX_BIN" ]]; then
  [[ "${#RUSTPBX_CONFIGS[@]}" -eq "$core_count" && -n "${RUSTPBX_CONFIGS[0]-}" ]] || { echo "RUSTPBX_CONFIG_PATHS must contain one config path per core server" >&2; exit 1; }
  for index in "${!CORE_PRIVATE_IPS[@]}"; do
    private_ip=${CORE_PRIVATE_IPS[$index]}
    core_id="core-$(sanitize_id "$private_ip")"
    start_bg "rustpbx-$(sanitize_id "$private_ip")" env NODE_ID="$core_id" PRIVATE_IP="$private_ip" MEDIA_PUBLIC_IP="${CORE_MEDIA_PUBLIC_IPS[$index]}" "$RUSTPBX_BIN" --config "${RUSTPBX_CONFIGS[$index]}"
  done
elif [[ "${PHONARCH_USE_MOCK_RUSTPBX:-0}" == "1" && -x "$BUILD/mock-rustpbx-$BIN_SUFFIX" ]]; then
  for index in "${!CORE_PRIVATE_IPS[@]}"; do
    private_ip=${CORE_PRIVATE_IPS[$index]}
    start_bg "rustpbx-$(sanitize_id "$private_ip")" env MOCK_LISTEN="${CORE_RUSTPBX_CONTROLS[$index]}" MOCK_SIP_LISTEN="${private_ip}:${CORE_SIP_PORTS[$index]}" "$BUILD/mock-rustpbx-$BIN_SUFFIX"
  done
else
  log "RUSTPBX_BIN is not executable; skipping engine processes and starting sidecars only"
fi

for index in "${!CORE_PRIVATE_IPS[@]}"; do
  private_ip=${CORE_PRIVATE_IPS[$index]}
  core_id="core-$(sanitize_id "$private_ip")"
  role=${CORE_ROLES[$index]-listener}
  sidecar_bind=${CORE_SIDECAR_BINDS[$index]}
  start_bg "sidecar-$(sanitize_id "$private_ip")" env REDIS_ADDR="$REDIS_ADDR" NODE_ID="$core_id" PRIVATE_IP="$private_ip" MEDIA_PUBLIC_IP="${CORE_MEDIA_PUBLIC_IPS[$index]}" NODE_ROLE="$role" SIP_UDP_PORT="${CORE_SIP_PORTS[$index]}" SIP_TCP_PORT="${CORE_SIP_PORTS[$index]}" SIDECAR_LISTEN="$sidecar_bind" SIDECAR_CONTROL_URL="$(http_url "$sidecar_bind")" RUSTPBX_CONTROL_URL="$(http_url "${CORE_RUSTPBX_CONTROLS[$index]}")" SIDECAR_EVENT_URL="http://${CONTROL_API_LISTEN}/internal/v1/events" "$BUILD/pbx-sidecar-$BIN_SUFFIX"
done

export NEXT_PUBLIC_CONTROL_API_URL="${NEXT_PUBLIC_CONTROL_API_URL-}"
start_bg ui bash -lc "cd '$DATA_ROOT/ui' && PORT='${UI_PORT:-3000}' HOSTNAME='0.0.0.0' node .next/standalone/server.js"
log "PhonArch services started; inspect $RUNTIME/logs and run /data/scripts/status.sh"
