# Sandbox Agent: Decision Record v2 (Oct 19, 2026)

> Source: the owner's own research, copied into the repository as input. It is **context, not the specification**. Where it differs from `README.md` v0.6 or `BUILD_SPEC.md` v1.2, those win. The reconciliation table at the end says what was adopted, what changed and why.

## Product
Personal coding agent driven from Telegram and web chat. Core loop: I name a repo and describe the issue in plain language. The agent clones it, investigates, fixes, verifies, then opens a PR with a markdown report for my review. A life-assistant mode comes later.

## Infrastructure
- Host: Oracle Always Free, free-only account (no Pay As You Go).
- VM: Ubuntu on VM.Standard.A1.Flex, 2 OCPU (Arm), 12 GB RAM, 2 Gbps, block storage only. This is the entire free Arm allowance after Oracle's 2026 cut, so there is no second free Arm VM.
- Disk: grow the boot volume from 50 GB to about 100 GB (200 GB free pool).
- Docker rootless. Benchmark gVisor, with hardened Docker as fallback.
- Every tool must have an arm64 build. Browser layer still unverified.

## Architecture
- Go controller: Telegram bot (my user ID only), web chat, task queue, one task at a time.
- Engine: OpenCode, run headless inside a disposable per-task container with its own git worktree. LSP and snapshots off, since worktree checkpoints replace undo.
- Browser: on demand only, in its own container, never alongside a build.

## Isolation
- Controller, secrets, and keys stay outside the sandbox.
- Default-deny network with an allowlist. Block the VM metadata address and internal ports.
- Mount only the task workspace. No real secrets inside.
- Scoped repo token plus branch protection on main.
- My approval for pushes, PRs, deletions, and new domains.
- Per-sandbox caps: about 1 core, fixed memory, timeout. /stop kill switch and a full command audit log.

## v1 harness features
Worktree per task with checkpoints, approved task contract (scope, off-limits paths, tests, budget), independent verification (agent cannot self-certify, flags deleted or skipped tests), hard budgets, project rules file.

## Risk and incident tracking
- Approval screen shows git-history risk: last change, frequency, recent commits, co-changing files.
- Incident log kept outside git. Every entry needs my confirmation. Read on fresh clones.
- No logging in a project: agent asks before adding a lightweight setup. With no error surface, it relies on my plain-language report plus git history.

## Free-tier survival
- Oracle deems an instance idle when, over 7 days, CPU (95th percentile), network, and memory are all under 20%.
- Keep memory above about 4 GB resident and run a real daily CPU job, such as a dependency-update or re-index run.
- Keep rebuild scripts, boot-volume backups (5 free), and incident log and agent state copied to free Object Storage.
- Set an alert if the VM stops.

## Build order
1. Go gateway and queue. 2. Sandbox runner with worktree, network lockdown, budgets. 3. Task contract and Telegram approval. 4. Verifier and risk card. 5. Rules-file generation. 6. Incident log with confirmation.

## Added since v1
Hardened per-task containers, rootless Docker, survival plan, disk growth, arm64 checks.

## Removed since v1
- OpenHands: needs the host Docker socket, Python-based, about 4 GB RAM on its own.
- Parallel multi-agent runs.
- Local models and open-gen-AI: no GPU.
- Pay As You Go fallback and a separate sandbox VM: not available on a free-only account.

## Later (not yet approved)
Lessons file from the incident log, post-merge watch, one reviewer pass, replay harness, optional gateway on a free AMD micro VM.

---

# Reconciliation with README and BUILD_SPEC (v0.5 and v1.1; extended by the addendum below for v0.6 and v1.2)

**Status key:** Adopted (as written) · Changed (adopted with a deliberate difference, reason given) · Added (new detail the record did not have) · Open (needs the owner's check).

| Record item | Status | Where it lives | Note |
|---|---|---|---|
| Oracle Always Free, 2 OCPU Arm, 12 GB, no PAYG | Adopted | README header, 11.8, 11.6 | Sized for 2 OCPU and 12 GB. **Open:** Oracle's published docs list 3,000 OCPU-hours and 18,000 GB-hours per month (equal to 4 OCPU and 24 GB) and no cut was found, so check the tenancy's own limits page (V8). If the cut is not real there is unused headroom. |
| Boot volume to about 100 GB | Adopted | BUILD_SPEC M0, `docs/runbooks.md` | Done by hand in the OCI console plus `growpart` and `resize2fs`; documented, not scripted. |
| Docker rootless; benchmark gVisor; hardened Docker fallback | Adopted | README 11.1A, 9 (runtime), D20; M3a | Seven rootless checks are VERIFY items (V9, V10). If rootless cannot meet a requirement, fall back to rootful with user-namespace remapping, never weaken isolation. |
| Every tool needs an arm64 build; browser unverified | Adopted | V16, M11 | Browser is Phase 8 and gated on that check. |
| One task at a time | Adopted | README 11.4, 11.6, D19; BUILD_SPEC queue | `max_sessions` defaults to 1. A second slot only on load-test evidence; three is out of scope. |
| OpenCode headless in a disposable per-task container | Adopted | README 5.2, 11.2 | |
| "Own git worktree" per task | **Changed** | README 5.1, 5.2, 9.2, D21 | An independent clone, not a linked worktree. A worktree's `.git` points outside the mount and shares hooks, config and objects with the cache, so a hostile repo could reach the host. The checkpoint behavior is kept (checkpoint refs, `/undo`). |
| LSP and snapshots off | Adopted, refined | README 6.7 | Off by default; `repos.lsp_enabled` turns LSP on per repository because diagnostics help the agent self-correct. |
| Browser on demand, own container, never alongside a build | Adopted, split in two | README 9.1, 9.3; M3a, M11 | **Research tier** (read-only fetch and search through the egress-proxy) arrives in M3a. **Interactive tier** (click, log in, submit) is Phase 8 with per-domain approval. |
| Controller, secrets and keys outside the sandbox | Adopted | README hard rule 2, 3.4 | Only `ROUTER_TOKEN` (model calls and research) is in the container. |
| Default-deny network with allowlist; block metadata and internal ports | Adopted, designed | README 3.4, 9.1; M3a | Internal network plus an allowlisting `egress-proxy` that resolves DNS itself and checks every address. Proved by `scripts/netcheck.sh`, because rootless networking may bypass host firewall rules. |
| Mount only the task workspace | Adopted | README hard rule 3 | |
| Scoped repo token plus branch protection | Adopted | README 8.8, D13 | |
| Approval for pushes, PRs, deletions, new domains | Adopted | README 7.5 | `new_domain` added as an approval action (5.7). |
| Per-sandbox caps (about 1 core, fixed memory, timeout) | Adopted | README 11.4, 7.2 | 1 CPU, 2.5 GB, watchdogs. |
| `/stop` kill switch | Adopted under another name | README 7.4 | The command is `/cancel`. Add `/stop` as an alias in M1 if you prefer. |
| Full command audit log | **Partly open** | README 10, 14 | Tool calls are stored as events (30-day retention) and gateway actions in `audit_log` (kept longer). Extend event retention if you want every command kept longer. |
| Task contract (scope, off-limits, tests, budget) | Added detail | README 7.7; M3b | Contract fields, defaults, enforcement table, and where each limit is enforced. |
| Independent verification; flags deleted or skipped tests | Adopted, designed | README 12; M3b | Fresh verify container on the quarantined checkout, plus a test-tampering guard (deleted tests, new skips, weakened assertions, config changes, empty pass). |
| Hard budgets | Adopted | README 7.7 | Wall time, tokens (counted by `task_id`), tool calls, files, fix loops. |
| Project rules file | Adopted | README 7.9; M8 | `/onboard` drafts `AGENTS.md` as a PR. |
| Git-history risk on the approval screen | Adopted | README 7.6 (since v0.4) | Now computed on the quarantine repo, not the agent's clone. |
| Incident log outside git, confirmed by me, read on fresh clones | Adopted, designed | README 7.8; M10 | Stored in Postgres keyed by repo; only confirmed rows affect risk. |
| No logging in a project: ask first | Adopted | README 7.8, 7.9 | Onboarding detects the missing error surface and asks. |
| Keep memory above about 4 GB resident | Adopted | README 11.8, D26; M0 | `keepalive` computes the memory it must hold and gives it back to tasks on demand. |
| Run a real daily CPU job | **Changed** (demoted) | README 11.8 | A 95th-percentile CPU measure exceeds 20% only if CPU is above 20% for more than 5% of the week, roughly 70 minutes every day. A short daily job does not change it. Memory is the primary lever; the nightly job is for housekeeping and backups; an optional CPU duty cycle exists and stays off unless OCI metrics show it is needed. |
| Rebuild scripts, boot-volume backups, state to Object Storage | Adopted, extended | README 11.9, D25 | Supabase stays the source of truth; Object Storage holds nightly dumps; secrets are never backed up automatically. |
| Alert if the VM stops | Adopted, designed | README 11.8 | An external uptime monitor plus a dead-man's-switch ping from the nightly job (both live outside the VM). |
| Build order 1 to 6 | Adopted, extended | BUILD_SPEC 2.1 | 1 is M1 and M4, 2 is M3a, 3 and 4 are M3b, 5 is M8, 6 is M10. **Added** around it: survival (M0), router (M2), API, auth and web (M5 to M7), bring-your-own-key (M7). |
| Removed: OpenHands, parallel runs, local models, PAYG, second VM | Adopted | README 1, 18 | |
| Later: lessons file, post-merge watch, reviewer pass, replay harness, AMD micro gateway | Adopted as backlog | README 18 | None approved. The micro-VM gateway would keep the control plane alive if the Arm VM were reclaimed. |

## Open points for the owner

1. **Oracle allowance (V8).** Confirm the tenancy's actual Always Free Arm limits.
2. **Audit retention.** Decide how long tool-call events must be kept.
3. **Kill switch name.** Keep `/cancel`, or add `/stop` as an alias.
4. **Domain.** A domain you own is still required before M5 (cookie login) and is needed at M0 for HTTPS.

---

# Addendum: capability-expansion research (checked 2026-11-23)

**What this is.** A batch of open-source agent tools from social posts was reviewed against what they actually do (documentation, repositories and write-ups), and a second-opinion synthesis of that review was checked line by line. Adopted items became Phase 9 (M12 to M16) in README v0.6 and BUILD_SPEC v1.2.

| Tool | What it is (verified) | Verdict | Where |
|---|---|---|---|
| mattpocock/skills | MIT-licensed collection of `SKILL.md` folders; OpenCode loads this format natively | **Adopt**, vendored, pinned, read-only | README 7.10, M12, D30 |
| Vane (Perplexica 2) and SearxNG | AI search built on SearxNG with embedding rerank and cited answers | **Adopt SearxNG** as the research search provider | README 9.1, M12, D33 |
| Hindsight | Agent memory with retain, recall and reflect over four memory networks; Python server, about 2 GB RAM recommended | **Adopt the model**; Go implementation first; the server only if the memory benchmark wins | README 7.11, M13, D31, D32 |
| Paperclip | Node.js orchestration (org charts, budgets, heartbeats, approvals) for agent companies | **Borrow** heartbeats and budgets as scheduled routines | README 7.12, M14, D34 |
| Orca | Agent IDE; diff-line comments go back to the agent; roughly 2 GB per parallel agent | **Borrow** the comment loop; **skip** parallel agents | README 7.13, M15, D35 |
| Perplexity SPACE | Three layers; credentials and egress handled outside the sandbox; Firecracker; Apache 2.0 | **Validation**, plus a modified credential idea and a runtime interface | README 9.4, M16, D36 |
| Handy | Offline speech-to-text | **Optional** voice notes, only if latency is acceptable | M15, V21 |
| Strix | Dynamic security testing with its own Docker sandbox image | **Backlog** (conflicts with the no-Docker-socket rule) | README 18 |
| Open Notebook, Dyad, Open WebUI | Document chat and podcasts; local app builder; chat UI | **Backlog or idea only** (`/learn`, scaffold catalog) | README 18 |
| Magnitude, AgenticSeek, rea, Meetily | Local inference; local Manus-style agent; reverse engineering; meeting notes | **Skip for now** | README 18 |

## Corrections made while verifying the synthesis

1. **Routines do not protect against reclaim.** "Replaces burn jobs" overstated it. A weekly job does nothing for a 95th-percentile CPU rule, so routines are for value only and the memory floor stays the survival measure (README 7.12, 11.8).
2. **Credential injection cannot rewrite HTTPS.** A CONNECT proxy sees an opaque tunnel; injecting headers would need TLS interception and a certificate authority trusted inside every container. The design uses per-service reverse-proxy routes instead (README 9.4, D36).
3. **osv-scanner sends data out by default.** It queries the public OSV service with package names and versions. Arm64 builds exist and an offline mode exists, so it runs offline against a database the host stages nightly (README 12, D37, V22). gitleaks arm64 is still to be confirmed.
4. **Supabase's free database is 500 MB and turns read-only past it**, and pauses after a week of inactivity. Memory therefore has a row cap and a 300 MB alert (README 7.11, D38).
5. **"Request changes" needs a state-machine edge and its own round cap**, not just a follow-up contract (README 7.13, BUILD_SPEC 5.1).
6. **"Measurable improvement" needs a defined benchmark**: pairs of tasks where the second needs what the first taught (`scripts/bench/memory/`).
7. **A tenth hard rule was needed**: skills and memory are context, never authority.
8. **Firecracker on the free Arm VM is unconfirmed.** No authoritative statement was found about nested virtualization on A1; test `ls /dev/kvm` (V17).
9. **Adopting the Hindsight server has side effects**: a Python service (amend D9 to "no Python in our own code") and data on the VM (add it to the nightly backup, D25).
10. **pgvector HNSW availability on the free plan is unclear.** Supabase's own documentation lists HNSW; one third-party article claims plan restrictions. Fall back to IVFFlat or an exact scan, which is fine at this scale (V20).
11. **Interview-style skills assume a live user.** How the agent's questions surface through `opencode serve` is unverified (V18).

## Open points for the owner

1. **Embeddings provider** for `agent-embed` (384 dimensions), or run memory on full-text search only at first.
2. **Skills approval:** confirm or trim the initial set of eight vendored skills, and review each before it is locked.
3. **Routines:** disabled by default; decide which kinds you want and the nightly window.
4. **Voice notes:** decide whether to build them at all (optional, M15).
