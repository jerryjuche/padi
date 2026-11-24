#!/usr/bin/env bash
# M3b smoke: local bare remote, fixed-script checkpoint drill, real verifier,
# stub GitHub API and the three trust gates. No secrets, no live PR.
set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
cd "$root"
tmp=$(mktemp -d /tmp/opencode/padi-m3b-smoke.XXXXXX)
trap 'rm -rf "$tmp"' EXIT

git config --global --add safe.directory "$tmp" >/dev/null 2>&1 || true
g() { git -c user.name=padi-smoke -c user.email=padi-smoke@local "$@"; }

say() { printf 'm3b-smoke: %s\n' "$*"; }

# 1. Local bare remote with an agent/* task branch.
g init --bare -q "$tmp/remote.git"
g init -q -b main "$tmp/source"
printf 'module m3bsmoke\n\ngo 1.21\n' >"$tmp/source/go.mod"
printf 'package m3bsmoke\n\nimport "testing"\n\nfunc TestOK(t *testing.T) {}\n' >"$tmp/source/m3b_test.go"
printf 'baseline\n' >"$tmp/source/work.txt"
g -C "$tmp/source" add -A
g -C "$tmp/source" commit -qm 'baseline'
g -C "$tmp/source" remote add origin "$tmp/remote.git"
g -C "$tmp/source" push -q -u origin main
g -C "$tmp/source" checkout -qb agent/smoke-task
printf 'changed\n' >"$tmp/source/work.txt"
g -C "$tmp/source" add -A
g -C "$tmp/source" commit -qm 'task change'
g -C "$tmp/source" push -q -u origin agent/smoke-task
g --git-dir="$tmp/remote.git" rev-parse --verify refs/heads/agent/smoke-task >/dev/null
say 'local bare remote accepts agent/* and retains main'

# 2. Fixed container-side checkpoint script, including rewind.
cp -a "$tmp/source" "$tmp/checkpoint"
export PADI_CHECKPOINT_DIR="$tmp/checkpoint"
ref0=$("$root/deploy/session/padi-checkpoint" save 0)
printf 'later\n' >"$tmp/checkpoint/work.txt"
ref1=$("$root/deploy/session/padi-checkpoint" save 1)
"$root/deploy/session/padi-checkpoint" rollback 0
test "$(cat "$tmp/checkpoint/work.txt")" = 'changed'
test -n "$ref0" && test -n "$ref1" && test "$ref0" != "$ref1"
unset PADI_CHECKPOINT_DIR
say 'checkpoint save/rollback restores the expected tree'

# 3. Real verifier: pass and fail verdicts.
pass=$(GOPROXY=off go run ./cmd/agent-verify --format json --timeout 2m "$tmp/source")
printf '%s\n' "$pass" | grep -q '"verdict": "pass"'
mkdir -p "$tmp/failing"
cp "$tmp/source/go.mod" "$tmp/failing/go.mod"
printf 'package m3bsmoke\n\nimport "testing"\n\nfunc TestFail(t *testing.T) { t.Fatal("smoke failure") }\n' >"$tmp/failing/m3b_test.go"
set +e
fail=$(GOPROXY=off go run ./cmd/agent-verify --format json --timeout 2m "$tmp/failing" 2>&1)
fail_code=$?
set -e
test "$fail_code" -ne 0
printf '%s\n' "$fail" | grep -q '"verdict": "fail"'
say 'real verification emits pass and fail verdicts'

# 4. Stub GitHub API and push allowlist are covered by hermetic Go tests.
GOPROXY=off go test -count=1 ./internal/gateway/gitops \
  -run 'TestOpenPR|TestPushAgentBranch' >/dev/null
say 'stub GitHub API and branch push gate passed'

# 5. Contract, guard, risk and approval gates block unsafe changes.
GOPROXY=off go test -count=1 ./internal/gateway/pipeline \
  -run 'TestPrepareBlocksUnsafeChanges|TestLockfileWithoutManifestIsApprovalNote' >/dev/null
GOPROXY=off go test -count=1 ./internal/verify/guard ./internal/gateway/risk >/dev/null
GOPROXY=off go test -count=1 ./internal/gateway/approvals \
  -run 'TestLoadStrictPolicy|TestRequiresLockfileApproval|TestDecide|TestGateRequiresApproval|TestTelegramHelpers' >/dev/null
GOPROXY=off go test -count=1 ./internal/gateway/store \
  -run 'TestValidTransition' >/dev/null
say 'contract, guard, risk, approval and expiry gates passed'

# 6. Gateway button/callback contract: task-scoped actions, ap: namespace and
# the audit regression guards for dual approvals, budgets and stale verdicts.
GOPROXY=off go test -count=1 ./cmd/gateway \
  -run 'TestButtonsAreTaskScoped|TestStaleAndUnapprovedCallbacks|TestMarkActionRejectsDuplicateTap|TestContractOverridesAndHardViolation|TestTrackUsageSignalsBudgetAbort|TestReadVerifyVerdictRejectsNonZeroExit|TestMaybePublishKeepsResumeUntilBothGates' >/dev/null
say 'task-scoped buttons, callback dedupe and trust regressions passed'

say 'PASS M3b smoke (no live PR, no secrets used)'
