# Padi M0–M3b audit

Date: 2026-11-25 (UTC)
Branch: `agent/m0-audit-fixes-b53482d`
Scope: repo, local runtime, live Supabase, Telegram, Docker, and the requested end-to-end Telegram PR gate.
Method: read-only inspection plus the existing gates. No secret values are reproduced here; only env var names, status codes, IDs and timestamps appear.

**Status (2026-11-27):** This is a **historical snapshot** of `agent/m0-audit-fixes-b53482d` at `cb027ca`; findings below are preserved as recorded. M4 and later fixes are committed locally through `1a3fab8` and reported in `update.md` §§23–29. At this snapshot `origin` was `9abe2a4` and no git push had been made; the owner later authorized a branch-only git push, with no PR. Finding 2 is fixed by `update.md` §23 and `TestPrepareWorkspaceReplacesCrossRepoSession`; Finding 1 token-like rows still await the owner's purge from `2026-11-24 10:20–12:44 UTC`. The live E2E gate remains pending genuine owner taps.

## Verdict

M0–M3b acceptance evidence is intact. All 16 `agent` tables have RLS enabled with zero anon REST exposure. The local router and gateway are each running as a single process. Two issues were found:

1. Five historical database rows (2026-11-24, before commit `da8e871`) contain token-like strings in error text. No rows after the M1/M2 redaction fixes contain them.
2. An explicit `clone <other-repo>` reuses the previous repo's live/idle container instead of replacing it.

The live Telegram E2E PR gate is **blocked**: it needs genuine owner button taps. Bot-token `sendMessage` cannot simulate a user-originated callback, and no owner approval for a real target repo has been observed.

At snapshot time, M4 implementation was present in the working tree and uncommitted, and this commit contained `audit.md` only. Current status is in the note above.

## 1. Milestone acceptance matrix

| Milestone | Local status | Live gap |
| --- | --- | --- |
| M0 host & DB survival | Passed: 14 tables at M0, idempotent `001`/`002`, RLS locked to anon, restore drill passed; `keepalive`, swap, sysctl and Caddy config present | ARM64 VM Option B drills not run (keepalive 4 GB floor, 256 MB OOM, rclone OCI upload, Caddy TLS, Healthchecks.io). Role passwords for VM not set from this host. |
| M1 gateway & session spine | Passed: OpenCode v2.4.0 HTTP/SSE client, Telegram poller/renderer, gitops cache/clone, store; live Telegram, Supabase, GitHub App and NVIDIA NIM verified | M1 throwaway NVIDIA key still needs owner revocation in NGC; not verifiable from repo. |
| M2 router v1 | Passed: 10/10 parallel hammer, alias masking, secret-free `/status`, zero provider keys in session containers | Checkbox unticked pending owner revocation above. `llm_usage` persistence was deferred to M4. |
| M3a isolation & quarantine | Passed: `netcheck.sh` 8/8 zones, 256 MB OOM `137` vs control `200`, bundle to quarantine to verify green, gVisor `runsc` not available locally | ARM64 runtime check deferred to owner-provisioned VM. |
| M3b trust & approvals | Passed: `m3b-smoke.sh` green, 16/16 tables RLS locked, two-gate Telegram UI, conversational-first routing, single-use buttons | No live PR opened. Push/PR verified against local bare remote and stub GitHub API only. |

## 2. Tools and integrations

Active and verified:

- Go toolchain: `gofmt`, `GOPROXY=off go vet ./...`, `GOPROXY=off go test -race -count=1 ./...` green.
- Docker 29.4.0: hardened `runc` containers (`CapDrop=ALL`, `no-new-privileges`, read-only rootfs, 256 MB limit) and OOM drill.
- OpenCode v2.4.0: HTTP Basic Auth and SSE; think/plan/execute roles on `agent-think`/`agent-plan`/`agent-build` wired to router aliases.
- Supabase PostgreSQL: 16 tables, all `relrowsecurity = t`; `agent_gateway` and `agent_router` have `BYPASSRLS` and login; anon and authenticated do not.
- Telegram `@Padi_Ai_bot`: live polling, escaped streaming edits, single-use `x:`, `ap:`, `a:` and `mode:` callbacks.
- GitHub App: RS256 JWT to installation token, bare cache clone with `x-access-token` env auth.
- NVIDIA NIM: `nvidia/nemotron-3.5-bolt-e4b` verified through the router.

Stubbed or not executed:

- `cmd/egress-proxy` live daemon (unit tests and `netcheck.sh` stubs only); live gateway uses the `agent-session-net` internal network repaired in `update.md` §25, with the router hostname pinned via Docker `ExtraHosts` (§29).
- `PushAgentBranch` and `OpenPRDraft` against real `github.com` (local bare remote and `httptest` only).
- Non-NVIDIA router providers: `<placeholder>` IDs, empty keys, disabled at startup; still accurate as of 2026-11-27.
- M5–M14: web UI/auth, GC, incidents table, skills verification, gitleaks/OSV, VM-only tools (`rclone`, `caddy`, `ufw`, `runsc`) — not built or not run on the ARM64 host.

## 3. Specific checklist findings

### A. Security and secret hygiene

| Check | Result |
| --- | --- |
| NVIDIA M1 key revoked | Owner action in NGC console; not verifiable here. |
| Masked Supabase key rotated | Owner action; not verifiable here. `agent_router` password was rotated in `.env.local` during M4 setup; `agent_gateway` password not verifiable here. |
| Git history / repo / `/tmp/opencode` logs | 0 tracked env files; 0 history matches; 0 log matches. One worktree match is a synthetic fixture in `internal/verify/secrets/secrets_test.go`. |
| `agent.tasks.fail_reason` / `agent.events` | **Finding 1.** 1 task row and 4 event rows match token-like patterns, all from 2026-11-24 10:20–12:44 UTC, before the redaction fixes in `da8e871` (18:23 UTC). No matching rows after that commit. Recommended owner actions: rotate the affected Telegram/GitHub tokens if still live, then redact or purge those five rows. Values deliberately not reproduced in this report. **Current status:** still awaiting the owner's purge. |
| Git execution boundary | `TestGitexecIsOnlyGitEntryPoint` passes. All git calls in `gitops` route through `internal/gateway/gitops/gitexec.go`; no host git command touches `workspaces/*/repo`. |
| Container mounts and env | Session binds are exactly `workDir:/workspace`; verify binds are `checkout:/workspace:ro` plus an optional gateway-owned output dir. No Docker socket or host path mounts. Env contains `ROUTER_TOKEN` and `OPENCODE_PASSWORD`, plus non-secret task/model/proxy variables (`PADI_TASK_ID`, `TASK_ID`, `SESSION_ID`, `PADI_MODEL`, `PADI_MODEL_BASE_URL`, HTTP proxy vars). No provider keys enter either container. |

### B. Logic and edge cases

| Check | Result |
| --- | --- |
| Single-session concurrency | Poller runs one serial `getUpdates` loop; `chatSession.begin()` is mutex-guarded and rejects a second turn while busy. **Finding 2.** `startTask` reuses an existing `chat` without comparing `chat.repo` to `c.repo` (`cmd/gateway/main.go:1033-1044`), so `clone <other-repo>` can run against the previous repo's idle container. Fix: close the chat when the requested repo differs, before `EnsureRepo`/`CreateSession`. **Current status:** fixed in `update.md` §23 by `prepareWorkspace` and covered by `TestPrepareWorkspaceReplacesCrossRepoSession`. |
| Approval replay and expiry | `Decide` enforces `via = telegram:<chat>:<user>`, rejects cross-chat and non-owner users, rejects expired and already-decided approvals, and the store CAS makes first decision win. `Gate` is per action, so `push_branch` cannot approve `open_pr`. Table-driven tests cover all of these. |
| Quarantine hostile inputs | Bundle is `Lstat`-checked, size-capped, verified, then fetched with `fsckObjects`. `InspectTree` rejects symlinks, nested `.git`, `.git_*` metadata, setuid/setgid and executable hook names. Submodules are not initialized; `.gitmodules` is not explicitly rejected, but a gitlink alone cannot execute code. |
| Egress DNS rebinding | `CheckedAddresses` resolves once and validates every address (`private`, `loopback`, `link_local`, `ULA`, CGNAT, metadata, configured ranges, IPv4-mapped IPv6). `DialChecked` dials the checked IP literal; no hostname re-resolution occurs. |

### C. Operational and repository

| Check | Result |
| --- | --- |
| Git branch hygiene | Working branch `agent/m0-audit-fixes-b53482d`, HEAD `cb027ca` before this audit. M4 edits were already uncommitted and are excluded from this commit. |
| Stale processes / cursor | One `padi-router` (PID 164189) and one `padi-gateway` (PID 176848). `agent.telegram_state.last_update_id` is `374300543` and advances only through the live poller; a second getUpdates client was deliberately not used so owner updates are not consumed. |
| Live E2E gate | **Blocked.** Required sequence: clone test repo → `PROPOSAL:` → Execute → container check → Risk Card → Approve Push → Approve PR → confirm draft PR. No genuine owner taps were observed; no live PR exists. Bot-token messages cannot substitute for owner callbacks. |

## 4. Gates run for this audit

- `GOPROXY=off go vet ./...`: pass
- `GOPROXY=off go test -race -count=1 ./...`: pass
- `go test -race -count=1 ./internal/gateway/gitops -run TestGitexecIsOnlyGitEntryPoint`: pass
- `scripts/dbaccess_test.sh` with `.env.local`: 16/16 tables return HTTP 404 to anon
- `scripts/m3b-smoke.sh`: pass (no live PR, no secrets used)

## 5. Next owner actions

1. Revoke the M1 NVIDIA key and confirm rotation of any exposed Telegram/GitHub token.
2. Redact or purge the five pre-`da8e871` database rows (`2026-11-24 10:20–12:44 UTC`) and set the VM role passwords; this remains open.
3. Finding 2 is already fixed in `update.md` §23 (`TestPrepareWorkspaceReplacesCrossRepoSession`); no further code action is needed.
4. In Telegram, tap Execute, Approve Push and Approve PR on a real target repo to close the live E2E gate; this remains pending genuine owner taps.
