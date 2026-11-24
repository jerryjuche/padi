#!/bin/sh
# M0 acceptance: the Supabase anon key must get no data from schema `agent`.
# Usage: SUPABASE_URL=https://<ref>.supabase.co SUPABASE_ANON_KEY=... sh scripts/dbaccess_test.sh
set -eu

: "${SUPABASE_URL:?set SUPABASE_URL to https://<ref>.supabase.co}"
: "${SUPABASE_ANON_KEY:?set SUPABASE_ANON_KEY to the project anon key}"

base="${SUPABASE_URL%/}/rest/v1"
tables="sessions repos tasks events approvals login_tokens web_sessions telegram_state telegram_messages system_flags llm_usage audit_log provider_keys risk_cache egress_allow session_checkpoints"
failed=0

for table in $tables; do
  body_file=$(mktemp)
  code=$(curl -sS -o "$body_file" -w '%{http_code}' \
    -H "apikey: $SUPABASE_ANON_KEY" \
    -H "Authorization: Bearer $SUPABASE_ANON_KEY" \
    "$base/$table?select=*&limit=1" 2>/dev/null || true)

  case "$code" in
    401|403|404)
      printf 'PASS  %-22s HTTP %s (no exposure)\n' "$table" "$code"
      ;;
    200)
      # Any 200 is a failure: an exposed schema with empty tables today leaks
      # rows as soon as the first one is inserted.
      printf 'FAIL  %-22s HTTP 200 (schema exposed): %s\n' "$table" "$(head -c 120 "$body_file")"
      failed=$((failed + 1))
      ;;
    *)
      printf 'FAIL  %-22s unexpected HTTP %s: %s\n' "$table" "$code" "$(head -c 120 "$body_file")"
      failed=$((failed + 1))
      ;;
  esac
  rm -f "$body_file"
done

if [ "$failed" -ne 0 ]; then
  printf 'dbaccess_test: %d table(s) exposed to the anon key\n' "$failed" >&2
  exit 1
fi
printf 'dbaccess_test: anon key cannot read any agent table (%d checked)\n' "$(printf '%s\n' $tables | wc -l | tr -d ' ')"
