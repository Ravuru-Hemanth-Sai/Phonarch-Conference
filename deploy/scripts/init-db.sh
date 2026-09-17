#!/usr/bin/env bash
set -euo pipefail
ENV_FILE="${PHONARCH_ENV_FILE:-/data/state/secrets/phonarch-conference.env}"
[[ -f "$ENV_FILE" ]] && set -a && source "$ENV_FILE" && set +a
command -v psql >/dev/null 2>&1 || { echo "psql is required" >&2; exit 1; }
[[ -n "${DATABASE_URL:-}" ]] || { echo "DATABASE_URL is required" >&2; exit 1; }
MIGRATIONS=/data/components/phonarch-platform-api/migrations
psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -f "$MIGRATIONS/001_initial.sql"
psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -f "$MIGRATIONS/002_room_delete_constraints.sql"
psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -f "$MIGRATIONS/003_multitenant_media_ha.sql"
echo "PhonArch schema, tenancy, media HA, and room-delete constraints applied."
