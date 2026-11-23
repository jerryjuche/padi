#!/bin/sh
# M0: dump schema `agent`, archive host config/state, keep 7 local days,
# upload to Object Storage via rclone and keep 14 remote days. No Python on the VM.
set -eu

: "${DATABASE_URL:?set DATABASE_URL to the gateway connection string}"

root=${AGENT_ROOT:-/srv/agent}
backup_dir=${BACKUP_DIR:-$root/backups}
src=${AGENT_SRC:-/srv/agent/src}
mkdir -p "$backup_dir"

ts=$(date -u +%Y%m%dT%H%M%SZ)
dump="$backup_dir/agent-$ts.sql.gz"

# No pipefail in /bin/sh: a failed pg_dump through a pipe would leave a valid
# but empty gzip. Write the raw dump first so set -e sees pg_dump's status.
raw="$dump.raw"
pg_dump -n agent --no-owner --no-acl "$DATABASE_URL" > "$raw"
if ! grep -q 'CREATE TABLE agent\.' "$raw"; then
  printf 'backup: dump contains no agent tables\n' >&2
  rm -f "$raw"
  exit 1
fi
gzip -c "$raw" > "$dump.tmp"
rm -f "$raw"
mv "$dump.tmp" "$dump"

archive="$backup_dir/config-state-$ts.tar.gz"
tar -czf "$archive" -C / etc/agent srv/agent/state 2>/dev/null || true

if [ -n "${RCLONE_REMOTE:-}" ]; then
  rclone copy "$dump" "$RCLONE_REMOTE/agent/" --no-traverse
  if [ -s "$archive" ]; then
    rclone copy "$archive" "$RCLONE_REMOTE/agent/" --no-traverse
  fi
  rclone delete "$RCLONE_REMOTE/agent/" --min-age 14d
  printf 'backup: uploaded %s to %s\n' "$(basename "$dump")" "$RCLONE_REMOTE"
else
  printf 'backup: RCLONE_REMOTE is unset; Object Storage upload SKIPPED (VERIFY V14)\n' >&2
fi

find "$backup_dir" -name 'agent-*.sql.gz' -mtime +7 -delete
find "$backup_dir" -name 'config-state-*.tar.gz' -mtime +7 -delete
printf 'backup: local dump %s ready\n' "$dump"
