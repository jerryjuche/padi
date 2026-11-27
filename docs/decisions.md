# Decision Log & Verification Evidence

Authoritative summary lives in `README.md` §17 and `BUILD_SPEC.md` §8[cite: 2, 4]. Every `VERIFY` item must be tested against the real environment and recorded here with date and command/output evidence before building on it[cite: 2].

## Architectural Decisions (D1–D43)

| # | Decision | Status | Reference |
|---|---|---|---|
| D1–D18 | Core engine (OpenCode), custom Go router, per-task container, host-only git, Postgres `seq` event store, private `agent` schema with RLS, GitHub App without Workflows, sticky BYOK paid keys, git-history risk context | Adopted | `README.md` §17[cite: 4] |
| D19 | Default concurrency `max_sessions: 1` (supersedes D14); 2 only on 10-run loadtest evidence | Adopted | `README.md` §11.4, §17[cite: 4] |
| D20–D24 | Rootless Docker (benchmark `runsc` vs `runc` in M3a), independent clone (not worktree), bundle export + quarantine repo, default-deny `egress-proxy`, fresh verify container + test-tampering guard | Adopted | `README.md` §9, §11.1A, §12[cite: 4] |
| D25–D29 | Supabase authoritative + nightly Object Storage dumps, memory-floor `keepalive` primary anti-reclaim lever, task contract + confirmed incident log, Phase 8 interactive browser opt-in | Adopted | `README.md` §7.7, §7.8, §11.8, §11.9[cite: 4] |
| D30–D38 | Pinned/hashed read-only skills, Go+Postgres `memory.Store` with provenance/trust (Hard Rule 10), SearxNG on `agent-net`, low-priority routines, request-changes loop, reverse-proxy credential injection, offline `osv-scanner` + `gitleaks`, 300 MB DB size alert | Adopted | `README.md` §7.10–§7.13, §9.4, §12[cite: 4] |
| D39 | Conversational-first Telegram broker: normal messages use the router directly with per-chat memory and no container/repo; a repo is requested only when the model marks real workspace work; every build turn is read-only plan + `PROPOSAL` until the owner taps Execute; `default_repo` is compatibility-only | Adopted | `cmd/gateway/directchat.go`, `cmd/gateway/main.go`, `deploy/session/AGENTS.target.md`, `update.md` §21 |
| D40 | `/options` is a bounded, escaped rolling live trace (phase track, tools, files, errors, elapsed time, tokens) that edits one Telegram card and rebuilds the last 18 events from durable `agent.events` for finished/failed tasks after the container closes | Adopted | `internal/gateway/telegram`, `cmd/gateway/main.go`, `update.md` §28 |
| D41 | A 15 s OpenCode activity probe makes silent model waits observable (`♥ alive`, retry count, last error); after 6 retries the gateway fails the task instead of waiting forever | Adopted | `cmd/gateway/main.go`, `internal/gateway/telegram`, `update.md` §29 |
| D42 | One OpenCode session carries explicit `think → plan → execute` role handoffs; unapproved work runs `think → plan`, and only the owner's Execute tap gates `plan → execute` with the approved PROPOSAL passed as a named handoff. Session image rebuilt with 8 aliases and a read-only custom `think` agent | Adopted | `deploy/session/AGENTS.target.md`, `deploy/session/opencode.base.json`, `update.md` §29 |
| D43 | Host-bridge runtime sets `PADI_ROUTER_HOST_IP=172.20.0.1`; the gateway adds Docker `ExtraHosts` `router:172.20.0.1` to session containers while `agent-session-net` stays `internal=true`. Compose deployments use a real `router` container and Docker DNS instead | Adopted | `config/gateway.yaml`, `internal/gateway/runner`, `deploy/docker-compose.yml`, `update.md` §29 |

## Open Verifications (`VERIFY` V1–V24)

| # | Target | Milestone | Status | Date | Evidence |
|---|---|---|---|---|---|
| V1 | OpenCode `serve` endpoints, event names, auth options, per-message model override | M1, M7 | Resolved (model override is **per-session**, not per-message) | 2026-11-23 | Real binary `opencode v2.4.0`. `opencode serve --help` flags: `--hostname`, `--port`, `--cors`, `--service`, `--stdio` (no auth flag). Live `/openapi.json`: `POST /api/session`, `POST /api/session/{id}/prompt` (body requires `text`; **no `model` field**), `GET /api/event` (`text/event-stream`, frames `{event,data}`, types include `session.text.delta`, `session.idle`, `session.execution.*`, `permission.asked`), `GET /api/session/{id}/message`, `POST /api/session/{id}/model` with body `{"model":{"id":"...","providerID":"..."}}`. Auth probe: `OPENCODE_PASSWORD=probe-pass opencode serve --hostname 127.0.0.1 --port 4599` then `curl http://127.0.0.1:4599/api/info` -> **401**; `curl -u opencode:probe-pass ...` -> **200** with `{"version":"2.4.0",...}`. Basic username is always `opencode`; the password comes from `OPENCODE_PASSWORD` (or is generated and printed on startup). Consequence: M7 alias switching must call the per-session model endpoint before a prompt; M1 relays `session.text.delta` into Telegram and treats `session.idle` as completion. Live-loop correction: v2.4.0 SSE frames are **data-only** (no `event:` line) with `type` inside the JSON; the observed successful run emitted `session.text.ended` and `session.execution.succeeded`, not `session.idle`. `translate.go` therefore maps `session.text.ended`/`session.idle` to `done` and `session.execution.failed`/`session.step.failed` to terminal errors; `client.go` accepts data-only frames. Real-model confirmation: with the owner's NVIDIA key and the repo base config, a live `opencode v2.4.0` prompt returned the exact requested text and emitted `session.text.ended` plus `session.execution.succeeded`; a 22-second gateway smoke test started cleanly with the same key and polled 38 times via a local Telegram stub. |
| V2 | OpenCode custom-provider config accepts router as OpenAI-compatible provider + tool calls | M2 | Resolved | 2026-11-24 | Real `opencode v2.4.0`, `providers.padi` package `@opencode/ai/providers/openai-compatible`, `baseURL=http://127.0.0.1:18082/v1`, `ROUTER_TOKEN`, `X-Task-Id`/`X-Session-Id`. Fake upstream saw `stream=true`, `model=agent-chat`, `tools=0` then `tools=14`, then `roles=[system,user,assistant,tool]`; assistant finished with `V2_OK tool result received.`. See M2 pre-code verification below. |
| V3 | `modalities` image input works for `agent-vision` | M8 | Pending | — | — |
| V4 | Supabase connectivity from localhost/VM (direct vs Session Pooler) & free-project pause rule | M0 | Recorded (localhost; VM pending) | 2026-11-23 | From localhost: `psql "$DATABASE_URL" -Atc 'select current_user, current_database()'` -> `postgres|postgres`; `scripts/dbaccess_test.sh` -> 14/14 tables HTTP 404 for anon/publishable key; JWKS endpoint HTTP 200. Pause rule (official docs): Free projects are paused after 7 days of low database activity; a few user queries per day is normally enough; a warning email arrives ~1 week before pausing; restore is possible within 1 year. The nightly `db_touch` step in `scripts/nightly.sh` supplies this activity. VM-side direct connection not yet tested. |
| V5 | Current free-tier limits and model IDs per provider (`agent-eval`) | M2, M4 | Recorded (live NVIDIA only; others official docs) | 2026-11-24 | Live NVIDIA chat completion with `nvidia/nemotron-3.5-bolt-e4b` returned HTTP 200 and `V5_OK`. Groq/OpenRouter/DeepSeek limits and IDs are recorded from official docs only; no keys for those providers are present on this host, so their `router.yaml` IDs stay `<placeholder>` and disabled. See M2 pre-code verification below. |
| V6 | Oracle Always Free idle-reclaim thresholds & OCI Monitoring memory utilization metric | M0 | Recorded (reclaim rule; OCI metric detail pending) | 2026-11-23 | Official Always Free docs: an instance is idle if, over a 7-day period, CPU utilization at the 95th percentile is <20%, network utilization is <20%, and (A1 shapes only) memory utilization is <20%; idle Always Free instances may be reclaimed. This validates the memory-floor `keepalive` design. The exact OCI Monitoring "memory utilization" computation for this instance still needs a live tenancy check. |
| V7 | Repo cache `clone_mode: partial` offline compatibility in container (optional) | M3a | Pending | — | — |
| V8 | Tenancy Always Free Arm allowance (2 OCPU / 12 GB vs 4 OCPU / 24 GB) | M0 | Recorded | 2026-11-23 | Owner confirmed 2 OCPU / 12 GB envelope; system sized to fit it. Current Oracle docs state 1,500 OCPU-hours and 9,000 GB-hours per month for A1 (equivalent to 2 OCPU / 12 GB), and confirm 200 GB total block storage and 5 free volume backups. The older 3,000 / 18,000 figure quoted in `README.md` V8 and `BUILD_SPEC.md` V8 appears stale; this does not change the design because it is already sized to the smaller envelope. |
| V9 | Rootless Docker cgroup v2 delegation (`cpu memory pids`), OOM kill at limit, unprivileged ports | M0, M3a | Partial (host OOM proven; VM/rootless pending) | 2026-11-24 | On this host: `memory.max=268435456`, hog `exit 137`, `OOMKilled=true`, control HTTP stayed `200` before and after. Hardened flags were `--runtime runc --security-opt no-new-privileges --cap-drop ALL --read-only --tmpfs /tmp:rw,nosuid,nodev,exec,size=512m --memory 128m --cpus 0.5 --pids-limit 64`. Rootless cgroup delegation on the ARM VM still pending under owner Option B. |
| V10 | gVisor (`runsc`) on arm64 with rootless Docker vs hardened `runc` | M3a | Partial (host only) | 2026-11-24 | `command -v runsc` is empty; Docker 29.4.0 uses `runc`, cgroup v2 with `systemd` driver, seccomp+AppArmor enabled. Hardened probe returned `CapEff: 0000000000000000`, rootfs `ro`, `memory.max=134217728`, `cpu.max=50000 100000`, `pids.max=64`. `scripts/runtime-bench.sh`: `runc: bench-ok in 634ms (hardened limits)`, `runsc: unavailable (command -v runsc empty)`. This host uses `/var/run/docker.sock` (not rootless) and amd64; gVisor on arm64/rootless stays unverified until the VM. Select hardened `runc` for M3a and re-test `runsc` on the VM. |
| V11 | Network topology isolation (`scripts/netcheck.sh` from inside session container) | M3a | Resolved | 2026-11-24 | `./scripts/netcheck.sh` built two throwaway internal networks and ran inside a real session container: allowed `router:8090 -> 200`, `egress-proxy:3129 -> 200`; blocked public internet, cloud metadata, host bridge `:8080`/`:5432`, a container on another internal network and the Docker socket. Final output: `netcheck: 8/8 zones correct`. |
| V12 | OpenCode headless `lsp`, `snapshot`, custom headers (`X-Task-Id`), MCP / custom tools | M1, M3a | Resolved | 2026-11-23 | Real `opencode v2.4.0` plus official `https://opencode.ai/config.json`. Schema keys: `lsp` (bool or per-server overrides), `snapshot` (**deprecated**) and `snapshots` (bool, authoritative), `provider` (deprecated) and `providers.<id>.headers` (string map), `mcp.servers.<name>` with `type: remote` + `url` (+ optional `headers`) or `type: local` + `command` array. There is **no config key for custom tools**; extension is via `plugins` / `plugin` (deprecated) or MCP servers, and built-in tools are gated by `tools` (deprecated) / `permissions`. Live probe: a temp project `opencode.json` with `lsp:false`, `snapshot:false`, `providers.probe.headers.x-probe:yes` and `mcp.servers.probe-remote` was accepted by `opencode serve`; authenticated `GET /api/config` returned `lsp:false`, `snapshots:false`, `providers:["probe"]`, `mcp:["servers"]`. M1 sets `lsp:false` and `snapshots:false` in `opencode.base.json`; per-provider headers are available for M2's router and `X-Task-Id`; MCP/tools work is deferred to M3a/M12 as specified. Live-loop evidence: the fake provider received `x-task-id=<task-id>` and `x-session-id=<session-id>` headers, and `settings.baseURL` pointed at the local fake provider. Provider override probe (same binary): `providers.<id>.models.<modelID>` with `modelID` is accepted (`id` also works); a custom provider needs `name`, `env`, `package: "@opencode/ai/providers/openai-compatible"`, `settings.baseURL` and `models`. `opencode models` then lists `nvidia/nvidia/nemotron-3.5-bolt-e4b` and the declared `groq/...` models, and `opencode serve` starts with both declared and no keys present. NVIDIA's API docs confirm the upstream ID `nvidia/nemotron-3.5-bolt-e4b`; models.dev does not yet ship it, hence the explicit override. |
| V13 | Search provider configuration (`none` vs `searxng`) | M3a | Resolved (`none`) | 2026-11-24 | No searxng config, service, URL or credential exists in `config/`, `deploy/`, `scripts/` or host (`grep -rn searxng` returned only this VERIFY row). Choose `provider: none`; research search returns `search_unavailable` and tells the agent to fetch known documentation URLs. Revisit when M12 has an acceptable free-tier provider. |
| V14 | Object Storage upload from arm64 (S3-compatible API / CLI without Python), boot backups, external monitors | M0 | Design resolved; live drill pending (VM not ready) | 2026-11-23 | Use **rclone's native `oracleobjectstorage` backend** (single static Go binary, arm64 build, no Python/OCI CLI) with `provider = instance_principal_auth`: certificates come from instance metadata, so **no credentials are stored on the VM** (Hard Rule 2). Config needs `namespace`, `region`, and (only for bucket listing) `compartment`; a dynamic group containing the instance plus an Object Storage policy are the only tenancy-side requirements. The S3-compatible alternative works but requires long-lived Customer Secret Keys on the VM, so it is rejected. Boot-volume backups: Oracle supports policy-based automatic backups; Always Free includes 5 free volume backups. External monitors: Healthchecks.io Hobbyist is USD 0/month with 20 checks (dead-man's switch via `HEALTHCHECK_PING_URL`); UptimeRobot has a free tier for HTTPS uptime. Live upload/alert drill awaits the ARM64 VM. See `update.md` §11 for the ordered VM command checklist. |
| V15 | `git bundle` round trip, `--no-hardlinks` offline clone, bundle size cap on large repos | M3a, M3b | Resolved | 2026-11-24 | Pre-code probe: 447-byte bundle round trip. M3a real drill: container-side `padi-export-bundle`, host copy-out, `gitops.Quarantine`, `CheckoutForVerify` and a fresh verify container returned `verdict: pass` with `verify_exit=0` (466-byte bundle). M3b large-repo drill: 100,663,319-byte fixture (96 MiB incompressible blobs), bundle 100,694,505 bytes in 10,496 ms, `git clone --no-hardlinks` in 2,910 ms, tree SHA-256 identical; cap `size-1` rejected and cap `size+1MiB` accepted. `ExportBundle`/`Quarantine` enforce the same cap through `BundleMaxMB`; the 1 MB over-cap case is unit-tested. |
| V16 | Chromium/Playwright on arm64 within 2 GB cap + remote MCP browser server | M11 | Pending | — | — |
| V17 | `/dev/kvm` presence on VM (`ls /dev/kvm`) | M16 | Pending | — | — |
| V18 | OpenCode skills discovery paths, config key, per-skill permissions, `question` event surfacing | M12 | Pending | — | — |
| V19 | SearxNG engines returning results from VM IP, JSON output, memory usage | M12 | Pending | — | — |
| V20 | pgvector on Supabase free plan (extension schema, HNSW/IVFFlat), 384d embeddings, DB size | M13 | Pending | — | — |
| V21 | `whisper.cpp` on arm64 latency & RSS on 2 OCPU (optional voice) | M15 | Pending | — | — |
| V22 | `gitleaks` and `osv-scanner` arm64 builds, CLI flags, offline DB refresh | M14 | Pending | — | — |
| V23 | `npm`, `go`, and `pip` base-URL overrides through credential injection routes | M16 | Pending | — | — |
| V24 | Telegram owner chat can run a conversational plan/research/build agent with inline buttons using existing credentials | M1 | Resolved | 2026-11-24 | `opencode v2.4.0` exposes `POST /api/session/{id}/agent` and emits `session.agent.selected`; built-in primary agents are `build` and `plan` (`general`/`explore` are subagents). Ad-hoc custom agents in `opencode.base.json` were ignored in this build, so the config overrides `build` and `plan`, disables `edit`/`write`/`patch`/`bash` tools for `plan`, and the gateway prefixes research/plan instructions to the prompt. Telegram `sendMessage`/`editMessageText` accept `reply_markup.inline_keyboard`; `callback_query` is allowed only from the configured owner's private chat. A per-chat long-lived container + OpenCode session preserves context across follow-ups and is reaped after 30 minutes idle. No new secrets: existing `TELEGRAM_BOT_TOKEN` and `PADI_MODEL_API_KEY`/`NVIDIA_API_KEY` are reused; Groq switch remains unchanged. |

## M2 pre-code verification (2026-11-24)

### V2 — OpenCode through an OpenAI-compatible router

A temporary project config declared `padi/agent-chat` as an OpenAI-compatible provider:

```json
{
  "model": "padi/agent-chat",
  "providers": {
    "padi": {
      "name": "Padi Router",
      "env": ["ROUTER_TOKEN"],
      "package": "@opencode/ai/providers/openai-compatible",
      "settings": {"baseURL": "http://127.0.0.1:18082/v1"},
      "headers": {"X-Task-Id": "v2-task", "X-Session-Id": "v2-session"},
      "models": {"agent-chat": {"modelID": "agent-chat", "name": "Agent Chat"}}
    }
  }
}
```

`opencode v2.4.0 serve` accepted the config; authenticated `GET /api/config` showed `model={"providerID":"padi","model":"agent-chat"}` and the `padi` provider. The prompt `Use your tools to list markdown files, then tell me what you found.` produced three requests to the fake upstream:

1. `stream=true`, `model=agent-chat`, `tools=0`, headers `x-task-id=v2-task` and `x-session-id=ses_...`.
2. `stream=true`, `model=agent-chat`, `tools=14` — the tool definitions were passed through unchanged.
3. `roles=[system,user,assistant,tool]` — the `glob` tool result returned to the provider; upstream ID `call_v2_glob` was preserved.

The final assistant content was `V2_OK tool result received.` and the session ended with `idle/outcome=succeeded`. Conclusion: the router can be a single OpenAI-compatible custom provider, tool calls pass through, and per-task/session headers are sent. No real provider key was used in this probe.

### V5 — free-tier facts and verified IDs

Live on this host with the existing M1 key: `POST https://integrate.api.nvidia.com/v1/chat/completions` for `nvidia/nemotron-3.5-bolt-e4b` returned HTTP 200 and exactly `V5_OK`. This is the only model ID enabled in M2's initial `router.yaml`.

Official documentation checked on 2026-11-24:

- **Groq** free limits: `openai/gpt-oss-120b`, `openai/gpt-oss-20b`, `qwen/qwen3.8-27b` are 30 RPM / 1K RPD / 8K TPM / 200K TPD; `openai/gpt-oss-safeguard-20b` is 3 RPM / 1K RPD / 2K TPM / 200K TPD. `llama-3.1-8b-instant` and `llama-3.3-70b-versatile` are now enterprise, not free. Sources: Groq rate-limits, models and OpenAI-compatibility docs.
- **OpenRouter**: IDs ending `:free` get 20 RPM / 50 RPD below 10 purchased credits and 20 RPM / 1000 RPD at or above 10. `openrouter/free` routes only to free models unless paid fallbacks are added. Sources: OpenRouter limits, free-variant and free-router docs.
- **DeepSeek**: no standing free tier; `deepseek-flash` and `deepseek-v4-pro` are priced per million tokens with account concurrency limits. Source: DeepSeek pricing and rate-limit docs.

No Groq, OpenRouter or DeepSeek key exists on this host, so those model IDs were not verified live and their M2 targets remain `<placeholder>` / disabled. The NVIDIA numeric free cap is not published officially (NVIDIA staff describe it as variable by model, use case and traffic), so the router's conservative RPM comes from configuration, not an assumed quota.

## M2 implementation decisions and acceptance (2026-11-24)

- **Router-only routing.** `config/gateway.yaml` names `padi/agent-chat` (plus the later per-role aliases) and `http://router:8090/v1`. `deploy/session/opencode.base.json` defines one provider family, `padi`, using `@opencode/ai/providers/openai-compatible`, `ROUTER_TOKEN` and the router base URL. NVIDIA/Groq providers and their keys were removed from session containers; the gateway injects only `ROUTER_TOKEN` and the router base URL. The current session image has 8 aliases: `agent-build`, `agent-plan`, `agent-chat`, `agent-small`, `agent-think` and `agent-vision` enabled on NVIDIA, with `agent-embed` and `agent-eval` disabled[cite: update.md §23, §29].
- **Concurrency queues instead of rejecting.** `limiter.Sem.Acquire(ctx)` blocks until a slot is free (bounded by `queue_max_wait`). The first hammer run with concurrency 1 produced 9×503 because `TryAcquire` rejected parallel requests; after this fix the same 10-request hammer returned 10×200. Delays, never failures, as the spec requires.
- **429 cooldown escalation.** No `Retry-After` → configured steps `1s, 2s, 5s, 15s, 1m`, one step per 429 response. With `Retry-After` present, the upstream value wins. Total exhaustion returns 503 with the soonest cooldown end in `Retry-After`.
- **Breaker isolated.** Circuit-breaker state lives in `internal/router/breaker` with its own `-race` table tests; only retryable failures count. 429 uses the cooldown map, 4xx never opens the breaker.
- **Chat style.** Owner chose concise collaborator, one natural clarifying question, contextual buttons, and "say the M1 limit, then do the local part". This is enforced in `AGENTS.target.md`, both agent prompts, `modePrompt` and a renderer filter that drops tool-parameter jargon from final replies.
- **Router URL must be container-reachable.** A local override of `http://127.0.0.1:8090/v1` made session tasks fail with `ECONNREFUSED`; inside a container that address is itself. Production uses the compose service name `router:8090`; local runs use the bridge gateway `172.20.0.1:8090`.
- **Telegram tokens never in logs.** `net/http` transport errors include the full `api.telegram.org/bot<token>/...` URL. A `redactedError` wrapper scrubs the token while keeping `errors.Is` working.
- **Placeholder discipline.** The starting `agent-chat` target was NVIDIA `nvidia/nemotron-3.5-bolt-e4b`; current enabled aliases are `agent-build`, `agent-plan`, `agent-chat`, `agent-small`, `agent-think` and `agent-vision`. `agent-embed` stays `<placeholder>`/disabled (the probed model was 2048d, not the required 384d); `agent-eval` is disabled on purpose and returns 503 `eval_provider_isolation` rather than judging NVIDIA work with NVIDIA. Groq, OpenRouter and DeepSeek targets remain `<placeholder>` and disabled at startup with warnings. `/status` exposes counters only; no token, key or base URL[cite: update.md §23].

## M3a pre-code verification (2026-11-24)

Real commands run on this host before any M3a code was written. No secrets appear in the output.

```
$ command -v runc; command -v runsc
/usr/bin/runc
(no runsc)

$ docker info --format '{{json .SecurityOptions}} {{.CgroupDriver}} {{.CgroupVersion}} {{.DefaultRuntime}}'
["name=apparmor","name=seccomp,profile=builtin","name=cgroupns"] systemd 2 runc

$ docker run --rm --runtime runc --security-opt no-new-privileges --cap-drop ALL \
    --read-only --tmpfs /tmp:rw,nosuid,size=16m --memory 128m --cpus 0.5 --pids-limit 64 \
    --entrypoint /bin/sh sandbox-agent-session:latest -c '<cgroup/caps probe>'
CapEff: 0000000000000000
overlay / overlay ro,...
134217728
50000 100000
64
```

Network topology (V11), internal network with fake services, session container attached only to it:

```
allowed http://router:8090/ -> 200
allowed http://egress-proxy:3129/ -> 200
blocked https://api.telegram.org/ -> 000
blocked http://172.20.0.1:8090/healthz -> 000
```

Bundle round trip (V15):

```
$ git -C source bundle create /tmp/opencode/m3a-v15/repo.bundle --all
$ git clone --no-hardlinks /tmp/opencode/m3a-v15/repo.bundle offline-clone
identical
447
cap_ok 447 1
```

Limits: this host is amd64 with a root Docker socket, so V10's arm64/rootless/gVisor portion remains open for the VM. V15's large-repository half was resolved on this host before M3b code; the ARM VM does not change the result.

## M3a implementation acceptance (2026-11-24)

```
$ gofmt -l cmd internal
internal/gateway/runner/runner.go
internal/gateway/runner/runner_test.go
$ gofmt -w internal/gateway/runner/runner.go internal/gateway/runner/runner_test.go
$ GOPROXY=off go vet ./...
$ GOPROXY=off go test -race -count=1 ./...
ok  github.com/jerryjuche/padi/... (all packages)
```

Real network drill (8/8):

```
allowed http://router:8090/ -> 200 (want 200)
allowed http://egress-proxy:3129/ -> 200 (want 200)
blocked https://api.telegram.org/ -> 000 (want 000)
blocked http://169.254.169.254/latest/meta-data/ -> 000 (want 000)
blocked http://172.17.0.1:8080/ -> 000 (want 000)
blocked http://172.17.0.1:5432/ -> 000 (want 000)
blocked http://172.22.0.2:8091/ -> 000 (want 000)
blocked /var/run/docker.sock -> absent
netcheck: 8/8 zones correct
```

Bundle → quarantine → verify-container drill (local fixture, real containers):

```
session_container=14bbe83816db7c57d499d8e258e0e138af9c368c5bfd5f16cf2ef04aabda368a
exported_bundle=/tmp/opencode/m3a-live/exported.bundle
quarantine_head=d2c1c6d9bcb24b4729f352e9a5dd33b862be7b3f
verify_checkout=/tmp/opencode/m3a-live/verify/checkout
verify_container=bcb8f6da674adef014ca24ec1dcb990ea850b402269ae389ddeb8ba9c02eee35
verify_exit=0 oom=false
{
  "verdict": "pass",
  "steps": [
    {"name":"go test","ok":true,"duration_ms":119225,"output_tail":"ok  \tfixture\t0.065s\n"}
  ]
}
```

Egress research drill with a local dummy router token (no real secret used):

```
status=ok title='Example Domain' chars=156
private fetch: http=403 {"code":"research_refused",...}
audit reason=port_not_allowed
search: http=501 {"code":"search_unavailable",...}
build proxy allowlisted registry.npmjs.org -> 200
build proxy api.telegram.org -> 403 CONNECT tunnel failed
```

OOM isolation:

```
control_before=200
hog_exit=137 hog_oomkilled=true
control_after=200
```

Decisions from defects found in the real drill:

- Docker `exec` must inspect `/exec/{id}/json`; a nonzero exit is now an error instead of a silent success.
- `/tmp` tmpfs needs explicit `exec` for compiled test binaries, `nosuid,nodev` retained; size raised to 512 MB for Go caches.
- The writable tmpfs is `/home/agent/.config`, not `/home/agent`, so the installed `opencode` binary stays visible.
- Explicit `PATH` and `HOME` are injected because the container API replaces the image environment.
- The fixed export script writes its bundle to `/workspace/.padi-task.bundle` because Docker cannot copy out of a runtime tmpfs; it deletes that file before every export so it is never committed.
- Image rebuilt locally after these changes; verify the current digest with `docker image inspect sandbox-agent-session:latest --format '{{.Id}}'` (the image has been rebuilt again since M3a[cite: update.md §27, §29]).

## V15 large-repository resolution (2026-11-24, before M3b code)

```text
$ git init -q -b main source
$ for n in 1 2 3; do dd if=/dev/urandom of=blob-$n.bin bs=1M count=32; done
$ git bundle create /tmp/opencode/m3b-v15/repo.bundle --all
$ git clone --no-hardlinks /tmp/opencode/m3b-v15/repo.bundle /tmp/opencode/m3b-v15/offline-clone

fixture_files=4 fixture_bytes=100663319
bundle_bytes=100694505 bundle_ms=10496 clone_ms=2910
tree_match=yes
src_sha=f0ed936eb296a309a332f72f1a5aea2ab9a490674f06d443e33088452aef59f3
clone_sha=f0ed936eb296a309a332f72f1a5aea2ab9a490674f06d443e33088452aef59f3
cap_under=100694504 result=rejected
cap_over=101743081 result=accepted
```

Conclusion: a ~96 MiB repo round-trips with byte-identical tree contents; the cap boundary is enforced by the same size check used in `ExportBundle` and `Quarantine`. V15 is fully resolved; no VM dependency remains.

## M3b Trust implementation (2026-11-24)

Approved deviations recorded before code:

1. `sql/004_checkpoints.sql` is applied now and also adds `tasks.contract` and `tasks.budget_used`. The spec's `003_harness.sql` was already applied in M3a, so a second migration is the safe idempotent path.
2. M10's confirmed incident log moves to `sql/005_incidents.sql`; M3b risk reads history-only signals from the quarantine repository and leaves incident input empty until M10.
3. Trust composition lives in `internal/gateway/pipeline/` so the individual `contract`, `approvals`, `risk`, `guard` and `gitops` packages remain independently testable.
4. `deploy/session/padi-checkpoint` plus an explicit `Dockerfile` install are part of accepted M3b scope because `contract.Checkpoint` invokes `/usr/local/bin/padi-checkpoint` inside the session container.
5. `scripts/m3b-smoke.sh` is the hermetic end-to-end drill: local bare remote, fixed-script checkpoint rewind, real `agent-verify`, stub GitHub API, push/branch gates, contract/guard/risk/approval gates, and Telegram button/callback contracts. No live PR and no secrets are used.
6. Telegram approval decisions use the `ap:` callback namespace; `undo:<task>:<n>` and `undo:<task>:latest` are task-scoped. This avoids collision with the M1 `a:<task>:<action>` quick actions and the legacy `act:` buttons.

Verification evidence:

- `psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -f sql/004_checkpoints.sql` is idempotent and leaves `agent.session_checkpoints` with RLS enabled, zero policies and `REVOKE ALL` from `anon, authenticated`.
- `scripts/dbaccess_test.sh` passes for all 16 `agent` tables, including `egress_allow` and `session_checkpoints`.
- `scripts/m3b-smoke.sh` ends with `PASS M3b smoke (no live PR, no secrets used)`.
- `GOPROXY=off go vet ./...` and `GOPROXY=off go test -race -count=1 ./...` are green.
- Session image rebuilt locally with both fixed scripts and `agent-verify`; verify the current digest with `docker image inspect sandbox-agent-session:latest --format '{{.Id}}'` (subsequent M4-era rebuilds supersede the M3b image[cite: update.md §27, §29]).
- An independent audit found five trust gaps before acceptance; all five were fixed with regression tests: dual approvals no longer deadlock and no extra message is sent while the second gate is pending, a 100% budget aborts with a checkpoint, owner off-limits paths stop a task on `file_change`, contract approval uses `AWAITING_PLAN_APPROVAL`, and verifier output is cleared so a stale verdict or non-zero wait is `unverifiable`.

Live PR remains deliberately unopened: the owner has not supplied a genuine Telegram decision for a target repository installation, and no approval is forged in code or tests.

Post-M3b/M4 evidence is in `update.md` §§23–29: M4 router/trust hardening and the §2.5 report, contract-approval callback repair, visual status card and internal-network repair, container-startup crash fix, stale-image/workspace-ownership/orphan-container fixes, `/options` rolling live trace with durable events, and silent-retry/role-handoff repairs. Status: M0–M4 implemented locally; the in-app push/PR flow is pending genuine owner Telegram taps, a git push to the audit branch is owner-authorized, and no draft PR exists. Do not start M5 without the owner's go-ahead.

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

## One-time contextual Telegram buttons (2026-11-25)

- Task-scoped keyboards are rendered from the task state, not a fixed menu. The final keyboard is built from the cleaned assistant reply, so a `PROPOSAL:` immediately yields Execute/Revise on the same card.
- Every actionable button carries a fresh random `x:` token stored with its chat, task, action and state. The first callback consumes the token and edits the tapped card to a spent state; a replay shows current buttons instead of starting another run. Legacy task-scoped tokens are also single-use.
- The session prompt no longer emits the retired "M1 cannot push or open a PR" disclaimer. Replies must answer the current request, state verification, and offer one concrete next step; publishing is an approval-gated gateway action, not a canned agent message.

Evidence is in `update.md` §20.

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
