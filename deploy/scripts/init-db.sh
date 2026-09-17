#!/usr/bin/env bash
set -euo pipefail
ENV_FILE="${PHONARCH_ENV_FILE:-/data/state/secrets/phonarch-conference.env}"
[[ -f "$ENV_FILE" ]] && set -a && source "$ENV_FILE" && set +a
# When PostgreSQL was staged under /data, make its client libraries discoverable
# without requiring a host-wide package installation.
if [[ -x /data/toolchains/postgres/usr/bin/psql ]]; then
  PATH="/data/toolchains/postgres/usr/bin:$PATH"
  export PATH
  LD_LIBRARY_PATH="/data/toolchains/postgres/usr/lib64:/data/toolchains/postgres/usr/lib:${LD_LIBRARY_PATH:-}"
  export LD_LIBRARY_PATH
fi
command -v psql >/dev/null 2>&1 || { echo "psql is required" >&2; exit 1; }
[[ -n "${DATABASE_URL:-}" ]] || { echo "DATABASE_URL is required" >&2; exit 1; }
MIGRATIONS=/data/components/phonarch-platform-api/migrations
psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -f "$MIGRATIONS/001_initial.sql"
psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -f "$MIGRATIONS/002_room_delete_constraints.sql"
psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -f "$MIGRATIONS/003_multitenant_media_ha.sql"
psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -f "$MIGRATIONS/004_room_lifecycle_roster.sql"
psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -f "$MIGRATIONS/005_room_dialing_region.sql"
psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -f "$MIGRATIONS/006_product_admin_control.sql"
echo "PhonArch schema, tenancy, media HA, room lifecycle, dialing defaults, and product admin controls applied."
