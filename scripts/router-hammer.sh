#!/bin/sh
# M2 acceptance hammer: send parallel chat requests through the router and
# verify that none fail. The router token is read from the environment and is
# never printed or written to disk.
set -eu

URL=${PADI_ROUTER_URL:-http://127.0.0.1:8090/v1/chat/completions}
MODEL=${PADI_ROUTER_MODEL:-agent-chat}
N=${PADI_HAMMER_N:-10}
PARALLEL=${PADI_HAMMER_PARALLEL:-3}

if [ -z "${ROUTER_TOKEN:-}" ]; then
  echo "router-hammer: ROUTER_TOKEN is required" >&2
  exit 2
fi

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT HUP INT TERM
: > "$tmp/codes"

# Variables are exported rather than passed as arguments so the token never
# appears in a process argument list.
export URL MODEL tmp ROUTER_TOKEN
seq "$N" | xargs -P "$PARALLEL" -I{} sh -c '
  code=$(curl -sS --max-time 45 -o "$tmp/body.{}" -w "%{http_code}" \
    -H "Authorization: Bearer $ROUTER_TOKEN" \
    -H "Content-Type: application/json" \
    -H "X-Task-Id: hammer-{}" \
    -d "{\"model\":\"$MODEL\",\"stream\":false,\"messages\":[{\"role\":\"user\",\"content\":\"Reply with OK.\"}]}" \
    "$URL" 2>/dev/null) || code=000
  printf "%s\n" "$code" >> "$tmp/codes"
' sh

sort "$tmp/codes" | uniq -c
if grep -qv '^2' "$tmp/codes"; then
  echo "router-hammer: at least one request failed" >&2
  exit 1
fi
echo "router-hammer: ok ($N requests, all 2xx)"
