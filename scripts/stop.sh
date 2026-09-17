#!/usr/bin/env bash
set -u
RUNTIME=/data/runtime
for file in "$RUNTIME"/pids/*.pid; do
  [[ -f "$file" ]] || continue
  pid=$(<"$file")
  if kill -0 "$pid" 2>/dev/null; then
    kill "$pid" 2>/dev/null || true
    for _ in {1..50}; do
      kill -0 "$pid" 2>/dev/null || break
      sleep 0.1
    done
    if kill -0 "$pid" 2>/dev/null; then
      kill -KILL "$pid" 2>/dev/null || true
    fi
  fi
  rm -f "$file"
done
echo "PhonArch processes stopped. Redis data remains under /data/runtime/redis."
