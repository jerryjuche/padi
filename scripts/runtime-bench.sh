#!/bin/sh
# M3a runtime evidence: measure a fixed short CPU task under each available
# runtime and write the report outside the repository. No secrets are used.
set -eu

IMAGE=${PADI_SESSION_IMAGE:-sandbox-agent-session:latest}
OUT=${PADI_RUNTIME_REPORT:-/tmp/opencode/runtime-bench.txt}
mkdir -p "$(dirname "$OUT")"
: > "$OUT"

for runtime in runc runsc; do
  if [ "$runtime" = runsc ] && ! command -v runsc >/dev/null 2>&1; then
    echo "$runtime: unavailable (command -v runsc empty)" | tee -a "$OUT"
    continue
  fi
  start=$(date +%s%N)
  if docker run --rm --runtime "$runtime" --security-opt no-new-privileges \
      --cap-drop ALL --read-only --tmpfs /tmp:rw,nosuid,size=16m \
      --memory 256m --cpus 0.5 --pids-limit 64 \
      --entrypoint /bin/sh "$IMAGE" -c \
      'i=0; while [ "$i" -lt 2000 ]; do i=$((i+1)); done; echo bench-ok' >/dev/null 2>&1; then
    end=$(date +%s%N)
    ms=$(( (end - start) / 1000000 ))
    echo "$runtime: bench-ok in ${ms}ms (hardened limits)" | tee -a "$OUT"
  else
    echo "$runtime: FAILED under hardened limits" | tee -a "$OUT"
  fi
done

echo "runtime-bench: report at $OUT"
