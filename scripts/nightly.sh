#!/bin/sh
# M0: nightly housekeeping. One failure never aborts the rest; each step's
# outcome is recorded in state/nightly.json for the gateway staleness watcher.
set -u

root=${AGENT_ROOT:-/srv/agent}
src=${AGENT_SRC:-/srv/agent/src}
state_dir=$root/state
mkdir -p "$state_dir"

started=$(date -u +%Y-%m-%dT%H:%M:%SZ)
results=""

record() {
  name=$1
  status=$2
  at=$3
  finished=$(date -u +%Y-%m-%dT%H:%M:%SZ)
  results="$results {\"step\":\"$name\",\"status\":\"$status\",\"started\":\"$at\",\"finished\":\"$finished\"},"
}

run_step() {
  name=$1
  shift
  at=$(date -u +%Y-%m-%dT%H:%M:%SZ)
  if "$@" >/dev/null 2>&1; then
    record "$name" ok "$at"
  else
    printf 'nightly: step %s failed (continuing)\n' "$name" >&2
    record "$name" failed "$at"
  fi
}

skip_step() {
  name=$1
  at=$(date -u +%Y-%m-%dT%H:%M:%SZ)
  printf 'nightly: step %s skipped\n' "$name" >&2
  record "$name" skipped "$at"
}

db_touch() {
  if [ -z "${DATABASE_URL:-}" ]; then
    return 1
  fi
  psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -q \
    -c 'select 1' \
    -c "insert into agent.system_flags (key, value) values ('nightly_heartbeat', now()::text) on conflict (key) do update set value = excluded.value, updated_at = now()"
}

git_cache_gc() {
  found=0
  for dir in "$root"/cache/repos/*; do
    if [ -d "$dir/.git" ]; then
      found=1
      git -C "$dir" gc --quiet --prune=now >/dev/null 2>&1 || true
    fi
  done
  [ "$found" -eq 1 ]
}

deadman_ping() {
  if [ -z "${HEALTHCHECK_PING_URL:-}" ]; then
    return 1
  fi
  curl -fsS "$HEALTHCHECK_PING_URL" >/dev/null
}

run_step db_touch db_touch
run_step backup sh "$src/scripts/backup.sh"

if [ -f "$src/scripts/gc.sh" ]; then
  run_step gc sh "$src/scripts/gc.sh"
else
  skip_step gc
fi

if command -v docker >/dev/null 2>&1; then
  run_step docker_image_prune docker image prune -f
else
  skip_step docker_image_prune
fi

run_step git_cache_gc git_cache_gc

# osv-scanner offline database refresh arrives with M14.
skip_step osv_refresh

if [ -n "${HEALTHCHECK_PING_URL:-}" ]; then
  run_step deadman_ping deadman_ping
else
  skip_step deadman_ping
fi

tmp=$state_dir/nightly.json.tmp
printf '{"started_at":"%s","finished_at":"%s","steps":[%s]}\n' \
  "$started" "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "${results%,}" > "$tmp"
mv "$tmp" "$state_dir/nightly.json"
printf 'nightly: state written to %s/nightly.json\n' "$state_dir"
