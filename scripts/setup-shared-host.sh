#!/bin/sh
# Prepare a host that ALREADY runs another service to also run padi, isolated:
# a dedicated non-root user with its own rootless Docker daemon, and padi's
# state tree under /srv/agent. Idempotent; changes nothing that is not ours.
# Run as root from the repository root:
#   sudo sh scripts/setup-shared-host.sh
set -eu

if [ "$(id -u)" -ne 0 ]; then
  printf 'setup-shared-host: run as root (sudo)\n' >&2
  exit 1
fi

padi_user=${PADI_USER:-padi}
src=${AGENT_SRC:-/srv/agent/src}

if ! id -u "$padi_user" >/dev/null 2>&1; then
  useradd -m -s /bin/bash "$padi_user"
  printf 'setup-shared-host: created user %s\n' "$padi_user"
else
  printf 'setup-shared-host: user %s already exists\n' "$padi_user"
fi

# subuid/subgid entries are what make rootless Docker possible.
if ! grep -q "^${padi_user}:" /etc/subuid 2>/dev/null; then
  printf 'setup-shared-host: WARNING no /etc/subuid entry for %s; rootless Docker will fail\n' "$padi_user" >&2
fi

for d in state backups cache secrets workspaces quarantine verify src; do
  mkdir -p "/srv/agent/$d"
done
chown -R "$padi_user:$padi_user" /srv/agent
chmod 700 /srv/agent/secrets
chmod 755 /srv/agent

# Keep the user manager (and therefore the rootless daemon) alive over SSH logout.
loginctl enable-linger "$padi_user" >/dev/null 2>&1 || true

uid=$(id -u "$padi_user")
gid=$(id -g "$padi_user")

printf '\nsetup-shared-host: done\n'
printf '  user          %s (uid %s, gid %s)\n' "$padi_user" "$uid" "$gid"
printf '  source tree   %s\n' "$src"
printf '  rootless sock /run/user/%s/docker.sock\n' "$uid"
printf '\nNext, as %s:\n' "$padi_user"
printf '  1. put the repo in %s (extract the tarball there)\n' "$src"
printf '  2. sh deploy/host/rootless-docker.sh\n'
printf '  3. export DOCKER_HOST=unix:///run/user/%s/docker.sock\n' "$uid"
printf '  4. docker build -f deploy/session/Dockerfile -t sandbox-agent-session:latest .\n'
printf '  5. write deploy/.env (chmod 600) with DATABASE_URL, TELEGRAM_BOT_TOKEN,\n'
printf '     TELEGRAM_OWNER_ID, ROUTER_TOKEN and your model provider key\n'
printf '  6. export PADI_UID=%s PADI_GID=%s\n' "$uid" "$gid"
printf '     export DOCKER_SOCK=/run/user/%s/docker.sock\n' "$uid"
printf '     docker compose -f deploy/docker-compose.yml up -d router egress-proxy gateway\n'
printf '     (never start the caddy service here: ports 80/443 belong to the other app)\n'
