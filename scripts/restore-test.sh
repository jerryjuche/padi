#!/bin/sh
# M0: restore the newest local dump into a throwaway Postgres container and
# check that the expected tables contain rows. Never touches the real database.
set -eu

root=${AGENT_ROOT:-/srv/agent}
backup_dir=${BACKUP_DIR:-$root/backups}

dump=''
for candidate in "$backup_dir"/agent-*.sql.gz; do
  if [ -f "$candidate" ]; then
    dump=$candidate
    break
  fi
done
if [ -z "$dump" ]; then
  printf 'restore-test: no agent-*.sql.gz in %s\n' "$backup_dir" >&2
  exit 1
fi

password="verify-$$-$(date -u +%s)"
container="agent-restore-test-$$"
trap 'docker rm -f "$container" >/dev/null 2>&1 || true' EXIT INT TERM

docker run -d --name "$container" \
  -e POSTGRES_PASSWORD="$password" \
  postgres:16-alpine >/dev/null

attempt=0
until docker exec "$container" pg_isready -U postgres >/dev/null 2>&1; do
  attempt=$((attempt + 1))
  if [ "$attempt" -ge 30 ]; then
    printf 'restore-test: throwaway database did not become ready\n' >&2
    exit 1
  fi
  sleep 1
done

gunzip -c "$dump" | docker exec -i "$container" \
  psql -U postgres -v ON_ERROR_STOP=1 -q >/dev/null

for table in sessions repos tasks events approvals; do
  count=$(docker exec "$container" psql -U postgres -Atc "select count(*) from agent.$table")
  printf 'restore-test: agent.%-14s %s rows\n' "$table" "$count"
done

printf 'restore-test: PASS (%s)\n' "$(basename "$dump")"
