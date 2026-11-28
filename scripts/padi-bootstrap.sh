#!/bin/sh
# padi bootstrap on a host that already runs another service.
# Run as root:   sudo sh padi-bootstrap.sh
# Safe to re-run: it updates the code, rebuilds, and starts only if
# deploy/.env has real values. It never touches the other service.
set -eu

REPO=${REPO:-https://github.com/jerryjuche/padi.git}
BRANCH=${BRANCH:-agent/vm-bootstrap-b53482d}
PU=${PADI_USER:-padi}
SRC=${AGENT_SRC:-/srv/agent/src}

if [ "$(id -u)" -ne 0 ]; then
  echo "run me with sudo" >&2
  exit 1
fi

# 1. dedicated user + state tree
if ! id -u "$PU" >/dev/null 2>&1; then
  useradd -m -s /bin/bash "$PU"
fi
for d in state backups cache secrets workspaces quarantine verify
do
  mkdir -p "/srv/agent/$d"
done
chmod 700 /srv/agent/secrets
chown -R "$PU:$PU" /srv/agent
loginctl enable-linger "$PU" >/dev/null 2>&1 || true
UP=$(id -u "$PU")
GP=$(id -g "$PU")
SOCK="/run/user/$UP/docker.sock"

# 2. code
if [ -d "$SRC/.git" ]; then
  runuser -u "$PU" -- git -C "$SRC" fetch -q origin "$BRANCH"
  runuser -u "$PU" -- git -C "$SRC" checkout -q "$BRANCH"
  runuser -u "$PU" -- git -C "$SRC" merge -q --ff-only "origin/$BRANCH"
else
  runuser -u "$PU" -- git clone -q -b "$BRANCH" "$REPO" "$SRC"
fi
chown -R "$PU:$PU" "$SRC"
echo "code:    $SRC at $(runuser -u "$PU" -- git -C "$SRC" rev-parse --short HEAD)"

# 3. rootless daemon that only padi uses
if [ ! -S "$SOCK" ]; then
  runuser -u "$PU" -- sh -c \
    "cd $SRC && sh deploy/host/rootless-docker.sh" || true
fi
i=0
while [ ! -S "$SOCK" ] && [ "$i" -lt 30 ]
do
  i=$((i + 1))
  sleep 1
done
if [ ! -S "$SOCK" ]; then
  echo "rootless docker did not start; see:" >&2
  echo "  journalctl --user-unit docker -n 30" >&2
  exit 1
fi
export DOCKER_HOST="unix://$SOCK"
echo "docker:  $DOCKER_HOST"

# 4. session image (built inside padi's own daemon)
runuser -u "$PU" -- env DOCKER_HOST="$DOCKER_HOST" sh -c \
  "cd $SRC && docker build -q -f deploy/session/Dockerfile \
   -t sandbox-agent-session:latest ."
echo "image:   sandbox-agent-session:latest"

# 5. secrets file (never printed, owner-only)
ENVF="$SRC/deploy/.env"
if [ ! -f "$ENVF" ]; then
  cat > "$ENVF" <<'EOF'
DATABASE_URL=
TELEGRAM_BOT_TOKEN=
TELEGRAM_OWNER_ID=
ROUTER_TOKEN=
NVIDIA_API_KEY=
EOF
  chown "$PU:$PU" "$ENVF"
  chmod 600 "$ENVF"
  echo "secrets: created $ENVF (empty)"
fi

if ! grep -q '^DATABASE_URL=.' "$ENVF" ||
   ! grep -q '^TELEGRAM_BOT_TOKEN=.' "$ENVF" ||
   ! grep -q '^ROUTER_TOKEN=.' "$ENVF"; then
  echo ""
  echo "next: fill these three (plus your model key) then re-run:"
  echo "  sudo -u $PU nano $ENVF"
  echo "  sudo sh $0"
  exit 0
fi

# 6. start padi only (no host ports, no caddy)
runuser -u "$PU" -- env DOCKER_HOST="$DOCKER_HOST" \
  PADI_UID="$UP" PADI_GID="$GP" DOCKER_SOCK="$SOCK" \
  sh -c "cd $SRC && docker compose -f deploy/docker-compose.yml \
         up -d --build router egress-proxy gateway"
echo ""
runuser -u "$PU" -- env DOCKER_HOST="$DOCKER_HOST" sh -c \
  "cd $SRC && docker compose -f deploy/docker-compose.yml ps"

echo ""
echo "isolation check:"
runuser -u "$PU" -- env DOCKER_HOST="$DOCKER_HOST" sh -c \
  "cd $SRC && sh scripts/netcheck.sh" || true

echo ""
echo "logs:  sudo -u $PU env DOCKER_HOST=$DOCKER_HOST \\"
echo "         docker compose -f deploy/docker-compose.yml logs gateway"
