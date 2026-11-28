#!/bin/sh
# Read-only preflight for running padi on a host that already runs another
# service. Changes nothing: no installs, no containers, no writes.
# Usage: sh vm-preflight.sh            (no root needed)
set -eu

say() { printf '%s\n' "$*"; }
kv() { printf '  %-28s %s\n' "$1" "$2"; }

say "== padi preflight (read-only) =="
kv "date" "$(date -u '+%Y-%m-%dT%H:%M:%SZ')"
kv "host" "$(hostname 2>/dev/null || echo unknown)"
kv "kernel" "$(uname -srm)"
if [ -r /etc/os-release ]; then
  kv "os" "$(. /etc/os-release && printf '%s' "${PRETTY_NAME:-unknown}")"
fi

say ""
say "== capacity =="
kv "cpus" "$(nproc 2>/dev/null || echo '?')"
mem_total_kb=$(awk '/^MemTotal:/{print $2}' /proc/meminfo)
mem_avail_kb=$(awk '/^MemAvailable:/{print $2}' /proc/meminfo)
kv "ram total" "$((mem_total_kb / 1024)) MB"
kv "ram available" "$((mem_avail_kb / 1024)) MB"
swap_kb=$(awk '/^SwapTotal:/{print $2}' /proc/meminfo)
kv "swap total" "$((swap_kb / 1024)) MB"
kv "load (1m/5m)" "$(cut -d' ' -f1-2 /proc/loadavg)"
df -Ph / 2>/dev/null | awk 'NR==2{printf "  %-28s %s free of %s (%s used)\n", "disk /", $4, $2, $5}'
if [ -d /srv ]; then
  df -Ph /srv 2>/dev/null | awk 'NR==2{printf "  %-28s %s free of %s (%s used)\n", "disk /srv", $4, $2, $5}'
fi

say ""
say "== isolation primitives =="
kv "cgroup" "$(stat -fc %T /sys/fs/cgroup 2>/dev/null || echo unknown)"
kv "user namespaces" "$(sysctl -n kernel.unprivileged_userns_clone 2>/dev/null || sysctl -n user.max_user_namespaces 2>/dev/null || echo unknown)"
for bin in dockerd docker newuidmap fuse-overlayfs slirp4netns rootlesskit runc runsc git go; do
  if command -v "$bin" >/dev/null 2>&1; then
    kv "found" "$bin ($(command -v "$bin"))"
  else
    kv "missing" "$bin"
  fi
done
if command -v sudo >/dev/null 2>&1; then
  if sudo -n true 2>/dev/null; then kv "sudo" "passwordless"; else kv "sudo" "not passwordless"; fi
fi

say ""
say "== existing workload (what we must not disturb) =="
if command -v systemctl >/dev/null 2>&1; then
  units=$(systemctl list-units --type=service --state=running --no-legend --no-pager 2>/dev/null \
    | awk '{print $1}' | grep -viE '^(systemd-|dbus|cron|cronie|rsyslog|ssh|sshd|polkit|multipathd|udisks2|snapd|unattended|networkd|chrony|chronyd|apparmor|user@|getty|irqbalance|packagekit|ModemManager|wpa_supplicant|acpid|accounts-daemon|atd|blk-|switcheroo|thermald|uuidd|upower|NetworkManager)' || true)
  if [ -n "$units" ]; then
    for u in $units; do kv "service" "$u"; done
  else
    kv "service" "(none beyond base system)"
  fi
  padi_units=$(systemctl list-unit-files --no-legend --no-pager 2>/dev/null | awk '{print $1}' \
    | grep -iE '(padi|keepalive|agent-nightly)' || true)
  kv "padi units present" "${padi_units:-none}"
fi

ports=$( (ss -ltnp 2>/dev/null || ss -ltn 2>/dev/null || netstat -ltn 2>/dev/null) | awk 'NR>1{print $4}' | sort -u || true)
if [ -n "$ports" ]; then
  kv "listening" "$(printf '%s' "$ports" | tr '\n' ' ')"
  for p in 80 443 8080 8090 3128; do
    if printf '%s\n' "$ports" | grep -qE "[:.]${p}$"; then kv "port ${p}" "IN USE"; else kv "port ${p}" "free"; fi
  done
fi

if command -v docker >/dev/null 2>&1; then
  kv "docker version" "$(docker version --format '{{.Server.Version}}' 2>/dev/null || echo 'daemon unreachable')"
  kv "docker rootless" "$(docker info -f '{{join .SecurityOptions \",\"}}' 2>/dev/null | grep -q rootless && echo yes || echo no)"
  kv "docker context" "$(docker context show 2>/dev/null || echo '?')"
  containers=$(docker ps --format '{{.Names}}\t{{.Image}}\t{{.Status}}' 2>/dev/null || true)
  if [ -n "$containers" ]; then
    printf '%s\n' "$containers" | while IFS="$(printf '\t')" read -r n i s; do kv "container" "$n ($i) $s"; done
  else
    kv "container" "(none running)"
  fi
fi
if [ -d /srv/agent ]; then
  kv "/srv/agent" "$(ls -A /srv/agent 2>/dev/null | tr '\n' ' ')"
else
  kv "/srv/agent" "absent"
fi

say ""
say "== verdict =="
min_kb=$((2 * 1024 * 1024))
ok=1
if [ "$mem_avail_kb" -lt "$min_kb" ]; then
  say "  RAM: FAIL — $((mem_avail_kb / 1024)) MB available, need at least 2048 MB for router + gateway + one build session."
  ok=0
else
  say "  RAM: ok — $((mem_avail_kb / 1024)) MB available."
fi
if [ ! -d /srv/agent ]; then say "  DISK: ok — /srv/agent absent, nothing of ours to collide with."; fi
if command -v docker >/dev/null 2>&1; then
  say "  DOCKER: present — padi's gateway needs the daemon; prefer a rootless daemon owned by a dedicated 'padi' user so the other service's containers stay out of reach."
else
  say "  DOCKER: missing — install Docker (rootless daemon support) before running the stack."
fi
if [ "$ok" -eq 1 ]; then
  say "  RESULT: host looks able to run padi alongside the existing service."
else
  say "  RESULT: host is too small right now; stop/grow the other service or use a separate VM."
fi
