#!/bin/sh
# M3a network-zone check. Host mode builds throwaway internal networks with
# fake services and runs the same script INSIDE a session container; "inside"
# mode performs the actual probes. No secrets are used.
set -eu

IMAGE=${PADI_SESSION_IMAGE:-sandbox-agent-session:latest}
IN=${PADI_NETCHECK_INTERNAL:-padi-netcheck-internal}
OTHER_IN=${PADI_NETCHECK_OTHER_NET:-padi-netcheck-other}
SESSION=padi-netcheck-session

probe() {
  label=$1
  url=$2
  expect=$3
  code=$(curl -sS -o /dev/null -w '%{http_code}' --max-time 5 "$url" 2>/dev/null) || code=000
  printf '%s %s -> %s (want %s)\n' "$label" "$url" "$code" "$expect"
  [ "$code" = "$expect" ]
}

inside_check() {
  ok=0
  gw=${PADI_HOST_GW:-172.17.0.1}
  other=${PADI_OTHER_HOST:-127.0.0.2}
  probe allowed http://router:8090/ 200 && ok=$((ok+1)) || true
  probe allowed http://egress-proxy:3129/ 200 && ok=$((ok+1)) || true
  probe blocked https://api.telegram.org/ 000 && ok=$((ok+1)) || true
  probe blocked http://169.254.169.254/latest/meta-data/ 000 && ok=$((ok+1)) || true
  probe blocked "http://$gw:8080/" 000 && ok=$((ok+1)) || true
  probe blocked "http://$gw:5432/" 000 && ok=$((ok+1)) || true
  probe blocked "http://$other:8091/" 000 && ok=$((ok+1)) || true
  if [ ! -S /var/run/docker.sock ]; then
    echo "blocked /var/run/docker.sock -> absent"
    ok=$((ok+1))
  else
    echo "blocked /var/run/docker.sock -> PRESENT"
  fi
  echo "netcheck: $ok/8 zones correct"
  [ "$ok" -eq 8 ]
}

if [ "${1:-}" = inside ]; then
  inside_check
  exit 0
fi

cleanup() {
  docker rm -f "$SESSION" padi-netcheck-router padi-netcheck-egress padi-netcheck-other >/dev/null 2>&1 || true
  docker network rm "$IN" "$OTHER_IN" >/dev/null 2>&1 || true
}
trap cleanup EXIT
cleanup
docker network create --internal "$IN" >/dev/null
docker network create --internal "$OTHER_IN" >/dev/null

for svc in router egress-proxy; do
  if [ "$svc" = router ]; then port=8090; name=padi-netcheck-router; else port=3129; name=padi-netcheck-egress; fi
  docker run -d --name "$name" --network "$IN" --network-alias "$svc" \
    --entrypoint python3 "$IMAGE" -m http.server "$port" >/dev/null
done
docker run -d --name padi-netcheck-other --network "$OTHER_IN" \
  --entrypoint python3 "$IMAGE" -m http.server 8091 >/dev/null

HOST_GW=$(docker network inspect bridge -f '{{(index .IPAM.Config 0).Gateway}}')
OTHER_HOST=$(docker inspect padi-netcheck-other -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}')

# Give the fake services a moment to bind their ports.
sleep 3

docker run --rm --name "$SESSION" --network "$IN" \
  -e "PADI_HOST_GW=$HOST_GW" -e "PADI_OTHER_HOST=$OTHER_HOST" \
  -v "$(cd "$(dirname "$0")" && pwd)/netcheck.sh:/netcheck.sh:ro" \
  --entrypoint /bin/sh "$IMAGE" /netcheck.sh inside
echo 'netcheck: ok (session container cannot reach public internet, metadata, host services, Docker socket or other networks)'
