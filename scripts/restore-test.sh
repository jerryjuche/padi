#!/bin/sh
# M0: restore the newest local dump into a throwaway Postgres container and
# check that the expected tables contain rows. Never touches the real database.
set -eu

root=${AGENT_ROOT:-/srv/agent}
backup_dir=${BACKUP_DIR:-$root/backups}

# Rootless Docker listens in the owner user's runtime dir, not /var/run.
if [ -z "${DOCKER_HOST:-}" ]; then
  owner_uid=$(stat -c %u "$root/src" 2>/dev/null || printf '')
  if [ -n "$owner_uid" ] && [ -S "/run/user/$owner_uid/docker.sock" ]; then
    DOCKER_HOST="unix:///run/user/$owner_uid/docker.sock"
    export DOCKER_HOST
  fi
fi

dump=''
# Do not break: lexicographic order means the LAST match is the newest timestamp.
for candidate in "$backup_dir"/agent-*.sql.gz; do
  if [ -f "$candidate" ]; then
    dump=$candidate
  fi
done
if [ -z "$dump" ]; then
  printf 'restore-test: no agent-*.sql.gz in %s\n' "$backup_dir" >&2
  exit 1
fi

password="verify-$$-$(date -u +%s)"
container="agent-restore-test-$$"
sql_file=$(mktemp)
trap 'rm -f "$sql_file"; docker rm -f "$container" >/dev/null 2>&1 || true' EXIT INT TERM

docker run -d --name "$container" \
  --memory=512m --memory-swap=512m --cpus=1 --pids-limit=128 \
  -e POSTGRES_PASSWORD="$password" \
  postgres:17-alpine >/dev/null

attempt=0
until docker exec "$container" pg_isready -U postgres >/dev/null 2>&1; do
  attempt=$((attempt + 1))
  if [ "$attempt" -ge 30 ]; then
    printf 'restore-test: throwaway database did not become ready\n' >&2
    exit 1
  fi
  sleep 1
done

# Decompress first: a pipe would hide a corrupt gzip from set -e.
gunzip -c "$dump" > "$sql_file"
if ! grep -q 'CREATE TABLE agent\.' "$sql_file"; then
  printf 'restore-test: dump contains no agent tables\n' >&2
  exit 1
fi
docker exec -i "$container" \
  psql -U postgres -v ON_ERROR_STOP=1 -q < "$sql_file" >/dev/null

tables=$(docker exec "$container" psql -U postgres -Atc \
  "select count(*) from information_schema.tables where table_schema='agent'")
rls_tables=$(docker exec "$container" psql -U postgres -Atc \
  "select count(*) from pg_tables where schemaname='agent' and rowsecurity")
seed=$(docker exec "$container" psql -U postgres -Atc \
  'select count(*) from agent.telegram_state where id=1')
printf 'restore-test: agent tables=%s rls_tables=%s telegram_state_seed=%s\n' \
  "$tables" "$rls_tables" "$seed"
if [ "$tables" -lt 14 ] || [ "$rls_tables" -lt 14 ] || [ "$seed" -ne 1 ]; then
  printf 'restore-test: dump failed structural verification\n' >&2
  exit 1
fi

# Row counts are evidence, not pass/fail: a fresh database legitimately has
# empty business tables while telegram_state is seeded.
for table in sessions repos tasks events approvals llm_usage; do
  count=$(docker exec "$container" psql -U postgres -Atc "select count(*) from agent.$table")
  printf 'restore-test: agent.%-14s %s rows\n' "$table" "$count"
done

printf 'restore-test: PASS (%s)\n' "$(basename "$dump")"
