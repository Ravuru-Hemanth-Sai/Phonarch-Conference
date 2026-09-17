#!/usr/bin/env bash
set -u
RUNTIME=/data/state/runtime
ENV_FILE="${PHONARCH_ENV_FILE:-/data/state/secrets/phonarch-conference.env}"
[[ -f "$ENV_FILE" ]] && set -a && source "$ENV_FILE" && set +a
for file in "$RUNTIME"/pids/*.pid; do
  [[ -f "$file" ]] || continue
  name=$(basename "$file" .pid)
  pid=$(<"$file")
  if kill -0 "$pid" 2>/dev/null; then echo "$name: running ($pid)"; else echo "$name: stale pid ($pid)"; fi
done
IFS=',' read -r -a sipgo_http_addresses <<< "${SIPGO_HTTP_ADDRESSES:-${SIPGO_HTTP:-127.0.0.1:8080}}"
for address in "${sipgo_http_addresses[@]}"; do
  address=${address%/}
  case "$address" in http://*|https://*) endpoint="$address/healthz" ;; *) endpoint="http://$address/healthz" ;; esac
  name="sipgo-$(printf '%s' "$address" | tr '.:/' '---' | tr -cd '[:alnum:]-')"
  curl -fsS "$endpoint" 2>/dev/null && echo "$name-http: healthy" || echo "$name-http: unavailable"
done
curl -fsS http://127.0.0.1:8081/healthz 2>/dev/null && echo "control-api: healthy" || echo "control-api: unavailable"
