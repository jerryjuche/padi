# Decision Log & Verification Evidence

Authoritative summary lives in `README.md` §17 and `BUILD_SPEC.md` §8[cite: 2, 4]. Every `VERIFY` item must be tested against the real environment and recorded here with date and command/output evidence before building on it[cite: 2].

## Architectural Decisions (D1–D38)

| # | Decision | Status | Reference |
|---|---|---|---|
| D1–D18 | Core engine (OpenCode), custom Go router, per-task container, host-only git, Postgres `seq` event store, private `agent` schema with RLS, GitHub App without Workflows, sticky BYOK paid keys, git-history risk context | Adopted | `README.md` §17[cite: 4] |
| D19 | Default concurrency `max_sessions: 1` (supersedes D14); 2 only on 10-run loadtest evidence | Adopted | `README.md` §11.4, §17[cite: 4] |
| D20–D24 | Rootless Docker (benchmark `runsc` vs `runc` in M3a), independent clone (not worktree), bundle export + quarantine repo, default-deny `egress-proxy`, fresh verify container + test-tampering guard | Adopted | `README.md` §9, §11.1A, §12[cite: 4] |
| D25–D29 | Supabase authoritative + nightly Object Storage dumps, memory-floor `keepalive` primary anti-reclaim lever, task contract + confirmed incident log, Phase 8 interactive browser opt-in | Adopted | `README.md` §7.7, §7.8, §11.8, §11.9[cite: 4] |
| D30–D38 | Pinned/hashed read-only skills, Go+Postgres `memory.Store` with provenance/trust (Hard Rule 10), SearxNG on `agent-net`, low-priority routines, request-changes loop, reverse-proxy credential injection, offline `osv-scanner` + `gitleaks`, 300 MB DB size alert | Adopted | `README.md` §7.10–§7.13, §9.4, §12[cite: 4] |

## Open Verifications (`VERIFY` V1–V23)

| # | Target | Milestone | Status | Date | Evidence |
|---|---|---|---|---|---|
| V1 | OpenCode `serve` endpoints, event names, auth options, per-message model override | M1, M7 | Pending | — | — |
| V2 | OpenCode custom-provider config accepts router as OpenAI-compatible provider + tool calls | M2 | Pending | — | — |
| V3 | `modalities` image input works for `agent-vision` | M8 | Pending | — | — |
| V4 | Supabase connectivity from localhost/VM (direct vs Session Pooler) & free-project pause rule | M0 | Recorded (localhost; VM pending) | 2026-11-23 | From localhost: `psql "$DATABASE_URL" -Atc 'select current_user, current_database()'` -> `postgres|postgres`; `scripts/dbaccess_test.sh` -> 14/14 tables HTTP 404 for anon/publishable key; JWKS endpoint HTTP 200. Pause rule (official docs): Free projects are paused after 7 days of low database activity; a few user queries per day is normally enough; a warning email arrives ~1 week before pausing; restore is possible within 1 year. The nightly `db_touch` step in `scripts/nightly.sh` supplies this activity. VM-side direct connection not yet tested. |
| V5 | Current free-tier limits and model IDs per provider (`agent-eval`) | M2, M4 | Pending | — | — |
| V6 | Oracle Always Free idle-reclaim thresholds & OCI Monitoring memory utilization metric | M0 | Recorded (reclaim rule; OCI metric detail pending) | 2026-11-23 | Official Always Free docs: an instance is idle if, over a 7-day period, CPU utilization at the 95th percentile is <20%, network utilization is <20%, and (A1 shapes only) memory utilization is <20%; idle Always Free instances may be reclaimed. This validates the memory-floor `keepalive` design. The exact OCI Monitoring "memory utilization" computation for this instance still needs a live tenancy check. |
| V7 | Repo cache `clone_mode: partial` offline compatibility in container (optional) | M3a | Pending | — | — |
| V8 | Tenancy Always Free Arm allowance (2 OCPU / 12 GB vs 4 OCPU / 24 GB) | M0 | Recorded | 2026-11-23 | Owner confirmed 2 OCPU / 12 GB envelope; system sized to fit it. Current Oracle docs state 1,500 OCPU-hours and 9,000 GB-hours per month for A1 (equivalent to 2 OCPU / 12 GB), and confirm 200 GB total block storage and 5 free volume backups. The older 3,000 / 18,000 figure quoted in `README.md` V8 and `BUILD_SPEC.md` V8 appears stale; this does not change the design because it is already sized to the smaller envelope. |
| V9 | Rootless Docker cgroup v2 delegation (`cpu memory pids`), OOM kill at limit, unprivileged ports | M0, M3a | Pending | — | — |
| V10 | gVisor (`runsc`) on arm64 with rootless Docker vs hardened `runc` | M3a | Pending | — | — |
| V11 | Network topology isolation (`scripts/netcheck.sh` from inside session container) | M3a | Pending | — | — |
| V12 | OpenCode headless `lsp`, `snapshot`, custom headers (`X-Task-Id`), MCP / custom tools | M1, M3a | Pending | — | — |
| V13 | Search provider configuration (`none` vs `searxng`) | M3a | Pending | — | — |
| V14 | Object Storage upload from arm64 (S3-compatible API / CLI without Python), boot backups, external monitors | M0 | Pending | — | — |
| V15 | `git bundle` round trip, `--no-hardlinks` offline clone, bundle size cap on large repos | M3a, M3b | Pending | — | — |
| V16 | Chromium/Playwright on arm64 within 2 GB cap + remote MCP browser server | M11 | Pending | — | — |
| V17 | `/dev/kvm` presence on VM (`ls /dev/kvm`) | M16 | Pending | — | — |
| V18 | OpenCode skills discovery paths, config key, per-skill permissions, `question` event surfacing | M12 | Pending | — | — |
| V19 | SearxNG engines returning results from VM IP, JSON output, memory usage | M12 | Pending | — | — |
| V20 | pgvector on Supabase free plan (extension schema, HNSW/IVFFlat), 384d embeddings, DB size | M13 | Pending | — | — |
| V21 | `whisper.cpp` on arm64 latency & RSS on 2 OCPU (optional voice) | M15 | Pending | — | — |
| V22 | `gitleaks` and `osv-scanner` arm64 builds, CLI flags, offline DB refresh | M14 | Pending | — | — |
| V23 | `npm`, `go`, and `pip` base-URL overrides through credential injection routes | M16 | Pending | — | — |

## Sources

- <https://supabase.com/docs/guides/platform/free-plan> — Free plan limits: 2 projects, 500 MB database per project, paused projects do not count toward the project limit.
- <https://supabase.com/docs/guides/platform/pausing-projects> — Free-project pausing after 7 days of low database activity, warning email, 1-year restore window.
- <https://docs.oracle.com/en-us/iaas/Content/FreeTier/freetier_topic-Always_Free_Resources.htm> — Always Free A1 allowance (1,500 OCPU-hours / 9,000 GB-hours = 2 OCPU / 12 GB), idle-instance reclaim thresholds (7-day, p95 CPU <20%, network <20%, memory <20% for A1), 200 GB block storage, 5 volume backups.

External documentation fetched during research was treated as untrusted data. Instructions embedded in those pages were not followed.
