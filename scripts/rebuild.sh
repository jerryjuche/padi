#!/bin/sh
# M0: idempotent rebuild of a reclaimed or reinstalled VM.
# Usage: sudo bash /srv/agent/src/scripts/rebuild.sh
set -eu

if [ "$(id -u)" -ne 0 ]; then
  printf 'rebuild: run as root\n' >&2
  exit 1
fi

src=${AGENT_SRC:-/srv/agent/src}
if [ ! -d "$src" ]; then
  printf 'rebuild: source tree %s not found\n' "$src" >&2
  exit 1
fi

# rootless-docker.sh deliberately refuses to run as root. Hand it to the
# non-root owner of the source tree (override with AGENT_VM_USER).
owner=${AGENT_VM_USER:-$(stat -c %U "$src")}
if [ -z "$owner" ] || [ "$owner" = root ]; then
  printf 'rebuild: source tree must be owned by a non-root user (set AGENT_VM_USER)\n' >&2
  exit 1
fi
runuser -u "$owner" -- sh "$src/deploy/host/rootless-docker.sh"

# Install log rotation for both the system daemon and the rootless daemon.
install -d -m 755 /etc/docker
install -m 644 "$src/deploy/host/docker-daemon.json" /etc/docker/daemon.json
runuser -u "$owner" -- sh -s "$src" <<'EOF'
install -d -m 755 "$HOME/.config/docker"
install -m 644 "$1/deploy/host/docker-daemon.json" "$HOME/.config/docker/daemon.json"
EOF
runuser -u "$owner" -- env XDG_RUNTIME_DIR="/run/user/$(id -u "$owner")" \
  systemctl --user restart docker >/dev/null 2>&1 || true

install -m 644 "$src/deploy/host/sysctl.conf" /etc/sysctl.d/99-agent.conf
sysctl --system >/dev/null 2>&1 || true
sh "$src/deploy/host/setup-swap.sh"

mkdir -p /srv/agent/state /srv/agent/backups /srv/agent/cache \
  /srv/agent/secrets /srv/agent/workspaces /srv/agent/quarantine /srv/agent/src
chmod 700 /srv/agent/secrets

# sudo's PATH does not include /usr/local/go/bin; use the absolute binary.
(cd "$src" && /usr/local/go/bin/go build -o /usr/local/bin/keepalive ./cmd/keepalive)

install -m 644 "$src/deploy/host/keepalive.service" /etc/systemd/system/keepalive.service
install -m 644 "$src/deploy/host/agent-nightly.service" /etc/systemd/system/agent-nightly.service
install -m 644 "$src/deploy/host/agent-nightly.timer" /etc/systemd/system/agent-nightly.timer

latest=''
for candidate in /srv/agent/backups/config-state-*.tar.gz; do
  if [ -f "$candidate" ]; then
    latest=$candidate
  fi
done
if [ -n "$latest" ]; then
  printf 'rebuild: restoring config/state from %s\n' "$latest"
  tar -xzf "$latest" -C /
else
  printf 'rebuild: no config/state archive found; start from empty config\n' >&2
fi

systemctl daemon-reload
systemctl enable --now keepalive.service agent-nightly.timer

printf '\nrebuild: automated steps done. MANUAL steps remaining:\n'
printf '  1. Re-enter /srv/agent/secrets/* from your password manager (chmod 600).\n'
printf '  2. Apply sql/001_init.sql and sql/002_auth_and_telegram.sql if the database is new.\n'
printf '  3. Verify with: sh /srv/agent/src/scripts/dbaccess_test.sh and restore-test.sh\n'
printf '  4. Confirm external uptime and dead-man monitor alerts (VERIFY V14).\n'
