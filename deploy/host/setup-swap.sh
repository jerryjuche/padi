#!/bin/sh
# M0: 4 GB swapfile with low swappiness. Idempotent. Run as root.
set -eu

if [ "$(id -u)" -ne 0 ]; then
  printf 'setup-swap: run as root\n' >&2
  exit 1
fi

swapfile=/swapfile

if [ ! -e "$swapfile" ]; then
  if ! fallocate -l 4G "$swapfile" 2>/dev/null; then
    dd if=/dev/zero of="$swapfile" bs=1M count=4096 status=none
  fi
  chmod 600 "$swapfile"
  mkswap "$swapfile" >/dev/null
fi

if ! swapon --show | grep -q "^$swapfile[[:space:]]"; then
  # A pre-existing file may be unformatted (e.g. a partial earlier run).
  if ! swaplabel "$swapfile" >/dev/null 2>&1; then
    mkswap "$swapfile" >/dev/null
  fi
  swapon "$swapfile"
fi
if ! grep -q "^$swapfile[[:space:]]" /etc/fstab; then
  printf '%s none swap sw 0 0\n' "$swapfile" >> /etc/fstab
fi
sysctl -w vm.swappiness=10 >/dev/null
printf 'setup-swap: %s is active (swappiness=10)\n' "$swapfile"
