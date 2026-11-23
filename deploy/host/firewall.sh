#!/bin/sh
# M0: deny inbound by default; allow SSH and web only. Run as root.
# Rootless Docker networking can bypass host rules, so isolation is proved by
# scripts/netcheck.sh in M3a rather than assumed from this firewall.
set -eu

if [ "$(id -u)" -ne 0 ]; then
  printf 'firewall: run as root\n' >&2
  exit 1
fi
if ! command -v ufw >/dev/null 2>&1; then
  printf 'firewall: ufw is not installed\n' >&2
  exit 1
fi

ufw default deny incoming
ufw default allow outgoing
for port in 22 80 443; do
  ufw allow "$port/tcp" >/dev/null
done
ufw --force enable >/dev/null
ufw status verbose
