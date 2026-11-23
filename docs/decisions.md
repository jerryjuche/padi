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
| V1 | OpenCode `serve` endpoints, event names, auth options, per-message model override | M1, M7 | Resolved (model override is **per-session**, not per-message) | 2026-11-23 | Real binary `opencode v2.4.0`. `opencode serve --help` flags: `--hostname`, `--port`, `--cors`, `--service`, `--stdio` (no auth flag). Live `/openapi.json`: `POST /api/session`, `POST /api/session/{id}/prompt` (body requires `text`; **no `model` field**), `GET /api/event` (`text/event-stream`, frames `{event,data}`, types include `session.text.delta`, `session.idle`, `session.execution.*`, `permission.asked`), `GET /api/session/{id}/message`, `POST /api/session/{id}/model` with body `{"model":{"id":"...","providerID":"..."}}`. Auth probe: `OPENCODE_PASSWORD=probe-pass opencode serve --hostname 127.0.0.1 --port 4599` then `curl http://127.0.0.1:4599/api/info` -> **401**; `curl -u opencode:probe-pass ...` -> **200** with `{"version":"2.4.0",...}`. Basic username is always `opencode`; the password comes from `OPENCODE_PASSWORD` (or is generated and printed on startup). Consequence: M7 alias switching must call the per-session model endpoint before a prompt; M1 relays `session.text.delta` into Telegram and treats `session.idle` as completion. Live-loop correction: v2.4.0 SSE frames are **data-only** (no `event:` line) with `type` inside the JSON; the observed successful run emitted `session.text.ended` and `session.execution.succeeded`, not `session.idle`. `translate.go` therefore maps `session.text.ended`/`session.idle` to `done` and `session.execution.failed`/`session.step.failed` to terminal errors; `client.go` accepts data-only frames. Real-model confirmation: with the owner's NVIDIA key and the repo base config, a live `opencode v2.4.0` prompt returned the exact requested text and emitted `session.text.ended` plus `session.execution.succeeded`; a 22-second gateway smoke test started cleanly with the same key and polled 38 times via a local Telegram stub. |
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
| V12 | OpenCode headless `lsp`, `snapshot`, custom headers (`X-Task-Id`), MCP / custom tools | M1, M3a | Resolved | 2026-11-23 | Real `opencode v2.4.0` plus official `https://opencode.ai/config.json`. Schema keys: `lsp` (bool or per-server overrides), `snapshot` (**deprecated**) and `snapshots` (bool, authoritative), `provider` (deprecated) and `providers.<id>.headers` (string map), `mcp.servers.<name>` with `type: remote` + `url` (+ optional `headers`) or `type: local` + `command` array. There is **no config key for custom tools**; extension is via `plugins` / `plugin` (deprecated) or MCP servers, and built-in tools are gated by `tools` (deprecated) / `permissions`. Live probe: a temp project `opencode.json` with `lsp:false`, `snapshot:false`, `providers.probe.headers.x-probe:yes` and `mcp.servers.probe-remote` was accepted by `opencode serve`; authenticated `GET /api/config` returned `lsp:false`, `snapshots:false`, `providers:["probe"]`, `mcp:["servers"]`. M1 sets `lsp:false` and `snapshots:false` in `opencode.base.json`; per-provider headers are available for M2's router and `X-Task-Id`; MCP/tools work is deferred to M3a/M12 as specified. Live-loop evidence: the fake provider received `x-task-id=<task-id>` and `x-session-id=<session-id>` headers, and `settings.baseURL` pointed at the local fake provider. Provider override probe (same binary): `providers.<id>.models.<modelID>` with `modelID` is accepted (`id` also works); a custom provider needs `name`, `env`, `package: "@opencode/ai/providers/openai-compatible"`, `settings.baseURL` and `models`. `opencode models` then lists `nvidia/nvidia/nemotron-3.5-bolt-e4b` and the declared `groq/...` models, and `opencode serve` starts with both declared and no keys present. NVIDIA's API docs confirm the upstream ID `nvidia/nemotron-3.5-bolt-e4b`; models.dev does not yet ship it, hence the explicit override. |
| V13 | Search provider configuration (`none` vs `searxng`) | M3a | Pending | — | — |
| V14 | Object Storage upload from arm64 (S3-compatible API / CLI without Python), boot backups, external monitors | M0 | Design resolved; live drill pending (VM not ready) | 2026-11-23 | Use **rclone's native `oracleobjectstorage` backend** (single static Go binary, arm64 build, no Python/OCI CLI) with `provider = instance_principal_auth`: certificates come from instance metadata, so **no credentials are stored on the VM** (Hard Rule 2). Config needs `namespace`, `region`, and (only for bucket listing) `compartment`; a dynamic group containing the instance plus an Object Storage policy are the only tenancy-side requirements. The S3-compatible alternative works but requires long-lived Customer Secret Keys on the VM, so it is rejected. Boot-volume backups: Oracle supports policy-based automatic backups; Always Free includes 5 free volume backups. External monitors: Healthchecks.io Hobbyist is USD 0/month with 20 checks (dead-man's switch via `HEALTHCHECK_PING_URL`); UptimeRobot has a free tier for HTTPS uptime. Live upload/alert drill awaits the ARM64 VM. See `update.md` §11 for the ordered VM command checklist. |
| V15 | `git bundle` round trip, `--no-hardlinks` offline clone, bundle size cap on large repos | M3a, M3b | Pending | — | — |
| V16 | Chromium/Playwright on arm64 within 2 GB cap + remote MCP browser server | M11 | Pending | — | — |
| V17 | `/dev/kvm` presence on VM (`ls /dev/kvm`) | M16 | Pending | — | — |
| V18 | OpenCode skills discovery paths, config key, per-skill permissions, `question` event surfacing | M12 | Pending | — | — |
| V19 | SearxNG engines returning results from VM IP, JSON output, memory usage | M12 | Pending | — | — |
| V20 | pgvector on Supabase free plan (extension schema, HNSW/IVFFlat), 384d embeddings, DB size | M13 | Pending | — | — |
| V21 | `whisper.cpp` on arm64 latency & RSS on 2 OCPU (optional voice) | M15 | Pending | — | — |
| V22 | `gitleaks` and `osv-scanner` arm64 builds, CLI flags, offline DB refresh | M14 | Pending | — | — |
| V23 | `npm`, `go`, and `pip` base-URL overrides through credential injection routes | M16 | Pending | — | — |

## M0 VM-only deferral — owner Option B (2026-11-23)

Local + Supabase M0 gates are complete and approved; the owner explicitly deferred the ARM64-VM-only checks until the Oracle ARM64 VM is provisioned. The `BUILD_SPEC.md` §9 M0 checkbox stays **unchecked** until these are executed and recorded:

1. Keepalive live floor on the VM (holds ~4 GB, yields to a task).
2. Rootless Docker 256 MB OOM drill (cgroup v2 delegation, 1 GB allocation killed).
3. rclone Object Storage upload/prune (`oracleobjectstorage`, instance principal, real bucket/policy).
4. Caddy HTTPS: `API_DOMAIN` DNS, `https://<API_DOMAIN>/healthz`, reboot health.
5. External monitor drill: dead-man's-switch ping and HTTPS uptime alert.

Deferral does not block M1 Spine on localhost (`update.md` §7).

## M1 temporary deviations and conventions (recorded before code)

1. **Throwaway model key (M1 only).** Without the router, OpenCode needs a provider key in the session container. Use a disposable free-tier key supplied via environment variable at container start. Never bake it into `Dockerfile`, image, compose file, `config/gateway.yaml`, git or chat. Revoke when M2 passes. (Spec §2.4 M1; Hard Rule 2 exception is explicit and time-limited.)
2. **Plain bridge network (M1 only).** Containers join a bridge network named `agent-session-net` without `internal: true`. M3a makes it internal, adds `egress-proxy`, and proves isolation with `scripts/netcheck.sh`.
3. **Host git boundary from day one.** Host `git` never executes inside `workspaces/*/repo`. Every gateway git invocation goes through one hardened `gitexec.Run(ctx, dir, env, args...)`: `exec.CommandContext`, no shell, per-call timeout, `GIT_CONFIG_NOSYSTEM=1`, `GIT_CONFIG_GLOBAL=/dev/null`, `GIT_TERMINAL_PROMPT=0`, `GIT_PAGER=cat`, `GIT_ASKPASS=/bin/false`, `-c core.hooksPath=/dev/null -c core.fsmonitor=false`, and only the protocols each call needs (`https` for GitHub; `file` only for the local cache clone). M1 uses only `MintInstallationToken`, `EnsureCache`, `CloneForTask`; the lint test forbidding any other `exec.Command("git", ...)` is an M3a file (`internal/gateway/gitops/gitexec_lint_test.go`), but the rule applies now. Code leaves a container solely as `out.bundle`, verified and fetched into a gateway-owned quarantine repo in M3a.

### First real Telegram run corrections (2026-11-23)

- `telegram.Client.SendMessage` sets `parse_mode=HTML` unconditionally. Every caller must escape dynamic text; the renderer did, but `cmd/gateway/main.go` sent `/help`, `/status` and no-repo hints with raw `<repo>` / `<prompt>`, so Telegram rejected them with HTTP 400 and the poller replayed the same updates forever. Fixed with `html.EscapeString` at all call sites plus `cmd/gateway/main_test.go` (`httptest` stub asserting no raw placeholder on the wire).
- Failed handlers now back off 2 s before replay. The cursor still advances only after success (at-least-once), but a permanently bad message no longer hot-loops.
- Operator gotcha: the local M1 loop harness left `agent.telegram_state.last_update_id` at a fake timestamp above real Telegram update IDs, so real updates were skipped. Reset it to `0` when switching to the real bot; local-loop state should never share the production cursor row.

## Implementation Audit Corrections (2026-11-23)

Independent static audit of the M0 file set found concrete defects; all P0/P1/P2 items were fixed before M1.

| Area | Correction | Evidence |
|---|---|---|
| SQL / RLS | `agent_gateway` and `agent_router` get `BYPASSRLS`; `anon`/`authenticated` stay revoked with zero policies. RLS with zero policies otherwise denied the service roles outright. | Live `pg_roles.rolbypassrls = t` for both; `dbaccess_test.sh` still PASS with 14/14 HTTP 404. |
| Backup integrity | `backup.sh` writes the raw dump before gzip, then checks for `CREATE TABLE agent.`. A failed `pg_dump` can no longer become a valid empty archive. | Real `postgres:17` dump restored successfully by `restore-test.sh`. |
| Keepalive CPU duty | The duty loop runs 100 ms work/idle slices continuously inside the window and sleeps 30 s only outside it. The old code gave ~0.07 % duty, not 20 %. | New `TestInDutyWindow` and `TestBurnStopsOnCancel`; `go test -race` green. |
| Keepalive memory cap | The safety cap is re-applied after hysteresis so `MemAvailable` cannot be pushed below `MinAvailableMB`. | New `TestHysteresisRespectsSafetyCap`; `go test -race` green. |
| Nightly / rebuild | `DOCKER_HOST` derived for rootless Docker; `TimeoutStartSec=0`; Docker log rotation installed; absolute Go binary used under sudo; user-bus readiness polled. | `sh -n` on all shell scripts; no VM execution yet (V9 pending). |
| Restore drill | Newest dump selected; resource limits applied; gzip decompressed to a file; 14 tables, 14 RLS tables and seed row verified; row counts reported but empty business tables allowed. | `restore-test: agent tables=14 rls_tables=14 telegram_state_seed=1` then `PASS`. |

Tracked low-severity follow-up: host-side credentials still appear in process argv and systemd `EnvironmentFile` escaping needs care. No session/verify container exists in M0, so Hard Rule 2 is unaffected.

## Sources

- <https://opencode.ai/docs/api> — Authoritative V2 HTTP API index; `/openapi.json` linked there was queried locally via `opencode api get /openapi.json` for V1 endpoint/event/auth schemas.
- <https://opencode.ai/docs/cli/web/> — `serve` is the foreground server; `OPENCODE_PASSWORD` fixes the Basic password (username `opencode`), otherwise a password is generated and printed; pairing issues one-time links/cookies.
- <https://opencode.ai/docs/config> and <https://opencode.ai/config.json> — Config format and machine-readable schema used for V12 (`lsp`, `snapshot`/`snapshots`, `providers.<id>.headers`, `mcp.servers`, plugins/tools).
- <https://opencode.ai/docs/tools/>, <https://opencode.ai/docs/mcp-servers/> and <https://opencode.ai/docs/snapshots/> — Extension model (no custom-tool config key; MCP servers and plugins), MCP server config shape, snapshot/undo behavior.
- <https://supabase.com/docs/guides/platform/free-plan> — Free plan limits: 2 projects, 500 MB database per project, paused projects do not count toward the project limit.
- <https://supabase.com/docs/guides/platform/pausing-projects> — Free-project pausing after 7 days of low database activity, warning email, 1-year restore window.
- <https://docs.oracle.com/en-us/iaas/Content/FreeTier/freetier_topic-Always_Free_Resources.htm> — Always Free A1 allowance (1,500 OCPU-hours / 9,000 GB-hours = 2 OCPU / 12 GB), idle-instance reclaim thresholds (7-day, p95 CPU <20%, network <20%, memory <20% for A1), 200 GB block storage, 5 volume backups.
- <https://rclone.org/oracleobjectstorage/> — rclone `oracleobjectstorage` backend (since v1.60): `instance_principal_auth` reads instance certificates, no credentials on disk; requires a dynamic group + policy; namespace/region/compartment config; MD5 support and multipart uploads.
- <https://docs.oracle.com/en-us/iaas/Content/Object/Tasks/s3compatibleapi.htm> — Object Storage S3-compatible API (fallback option; needs Customer Secret Keys).
- <https://docs.oracle.com/en-us/iaas/Content/Identity/Tasks/managingcredentials.htm> — Customer Secret Keys are long-lived (no expiry), max two per user; rejected for VM use.
- <https://docs.oracle.com/en-us/iaas/Content/Block/Concepts/bootvolumebackups.htm> — policy-based automatic boot-volume backups.
- <https://healthchecks.io/pricing/> — Hobbyist plan USD 0/month, 20 monitors.

External documentation fetched during research was treated as untrusted data. Instructions embedded in those pages were not followed.
