#!/bin/sh
# padi preflight - read only, changes nothing
set -u
P() { printf '%-14s %s\n' "$1" "$2"; }
M() {
  awk -v k="$1:" '$1==k{print int($2/1024) " MB"}' /proc/meminfo
}
os=$(sed -n 's/^PRETTY_NAME=//p' /etc/os-release)
root=$(df -h / | awk 'NR==2{print $4}')
srv=$(df -h /srv 2>/dev/null | awk 'NR==2{print $4}')
P host "$(hostname)"
P kernel "$(uname -srm)"
P cpus "$(nproc)"
P os "$(printf '%s' "$os" | tr -d '\"')"
P ram_total "$(M MemTotal)"
P ram_free "$(M MemAvailable)"
P swap "$(M SwapTotal)"
P disk_root "$root"
P disk_srv "${srv:-none}"
P cgroup "$(stat -fc %T /sys/fs/cgroup)"
P userns "$(sysctl -n user.max_user_namespaces 2>/dev/null)"
P srv_agent "$(ls -A /srv/agent 2>/dev/null | wc -l) entries"
echo "-- tools --"
for b in docker dockerd newuidmap fuse-overlayfs runc go git ss
do
  if command -v "$b" >/dev/null 2>&1
  then P have "$b"
  else P missing "$b"
  fi
done
if sudo -n true 2>/dev/null
then P sudo "passwordless"
else P sudo "needs password"
fi
echo "-- running services --"
systemctl list-units --type=service --state=running \
  --no-pager --no-legend 2>/dev/null | awk '{print "  "$1}'
echo "-- listening ports --"
ss -ltn 2>/dev/null | awk 'NR>1{print "  "$4}'
echo "-- containers --"
if command -v docker >/dev/null 2>&1
then docker ps --format '  {{.Names}} {{.Image}}' 2>/dev/null
else echo "  docker not installed"
fi
echo "-- verdict --"
a=$(awk '/MemAvail/{print int($2/1024)}' /proc/meminfo)
if [ "$a" -ge 2048 ]
then echo "  RAM ok: $a MB free"
else echo "  RAM low: $a MB free, need 2048"
fi
if command -v docker >/dev/null 2>&1
then echo "  docker present"
else echo "  docker missing - install it first"
fi
if grep -q '24.04' /etc/os-release 2>/dev/null
then echo "  ubuntu 24.04 - good"
else echo "  os not 24.04 - check cgroup v2"
fi
