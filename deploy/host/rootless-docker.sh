#!/bin/sh
# M0: finish a rootless Docker install for the non-root owner user.
# The Docker packages themselves are installed by the operator with a pinned
# version; this script never pipes an installer from the network into a shell.
set -eu

if [ "$(id -u)" -eq 0 ]; then
  printf 'rootless-docker: run as the non-root owner user, not root\n' >&2
  exit 1
fi
if ! command -v dockerd-rootless-setuptool.sh >/dev/null 2>&1; then
  printf 'rootless-docker: dockerd-rootless-setuptool.sh not found.\n' >&2
  printf 'Install docker.io with an apt-pinned version first, then re-run (VERIFY V9).\n' >&2
  exit 1
fi

user=$(id -un)
export XDG_RUNTIME_DIR=/run/user/$(id -u)

loginctl enable-linger "$user" >/dev/null 2>&1 || true
dockerd-rootless-setuptool.sh install

dropin=$HOME/.config/systemd/user/docker.service.d
mkdir -p "$dropin"
cat > "$dropin/override.conf" <<'EOF'
[Service]
Delegate=cpu memory pids
EOF

systemctl --user daemon-reload
systemctl --user restart docker

printf 'rootless-docker: installed for %s\n' "$user"
printf 'Add to your shell profile: export DOCKER_HOST=unix://%s/docker.sock\n' "$XDG_RUNTIME_DIR"
