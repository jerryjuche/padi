# CONTRACT.md — Authoritative Engineering & Security Contract

**Project:** Sandbox Agent (Single-User Remote AI Coding Agent)[cite: 4]
**Authoritative Spec Hierarchy:** `README.md` (v0.6) > `CONTRACT.md` > `BUILD_SPEC.md` (v1.2) > `docs/decision-record-v2.md`[cite: 2, 3, 4]
**Spec Revision:** 2026-11-27 (M0–M4 implemented locally; M4 §2.5 report complete; in-app push/PR flow pending genuine owner Telegram taps; git branch push is owner-authorized; no draft PR).
**Target Environment:** Localhost development first (`127.0.0.1`), deploying to Oracle Always Free `VM.Standard.A1.Flex` (2 OCPU ARM64, 12 GB RAM, ~100 GB boot volume), Supabase Postgres (free tier), Vercel (Next.js UI), and optional Render Free tier (stateless `/healthz` monitor only)[cite: 2, 3, 4].

---

## 1. The 10 Non-Negotiable Hard Rules

1. **No Default-Branch Pushes:** The agent never pushes to `main`, `master`, or any repo default branch[cite: 2, 4]. Only `agent/<slug>-<shortid>` branches are pushed, strictly after independent verification passes and the allowlisted owner approves[cite: 2, 4].
2. **Zero Secrets in Session Containers:** No git tokens, database credentials, or model provider API keys may exist inside a session or verify container[cite: 2, 4]. The **only** secret injected into a session container is `ROUTER_TOKEN` (accepted solely by `router` and `egress-proxy` read-only research)[cite: 2, 4].
3. **Single Read-Write Mount:** Only `/workspace` (`workspaces/<task_id>/repo`) is mounted read-write into a session container[cite: 2, 4]. Never mount the Docker socket, `/srv/agent/secrets`, host `/etc`, or cache repositories[cite: 2, 4].
4. **Explicit Owner Approval:** Destructive or externally visible actions (`task_contract`, `plan_new_app`, `edit_protected_path`, `delete_dir_or_many`, `push_branch`, `open_pr`, `start_preview`, `new_domain`, `log_incident`, `browser_interactive`, `credential_use`) require explicit approval from `TELEGRAM_ALLOWED_USER_ID`[cite: 2, 4].
5. **Router-Enforced Free-Tier Walls:** Session containers only ever see model aliases, never vendor endpoints or keys[cite: 2, 4]. Enabled NVIDIA aliases are `agent-build`, `agent-plan`, `agent-chat`, `agent-small`, `agent-think` and `agent-vision`; `agent-embed` and `agent-eval` are disabled (eval intentionally returns 503 `eval_provider_isolation` rather than reusing the build provider)[cite: update.md §23, §29].
6. **Zero Database Access from Browser:** `web/` (Next.js on Vercel) must never import `@supabase/supabase-js` or hold any DB connection string or key[cite: 2, 4]. All browser data flows through the Go `gateway`[cite: 2, 4].
7. **Hard Resource & Concurrency Caps:** Every container must have hard memory, swap (`MemorySwap == Memory`), CPU, PID, and wall-clock limits[cite: 2, 4]. Default concurrency is **`max_sessions: 1`** on the 2 OCPU / 12 GB VM (raising to 2 requires passing 10-run `scripts/loadtest.sh` evidence)[cite: 2, 4].
8. **Untrusted Workspace & Quarantine Git Boundary:** Host-side `git` never executes inside `workspaces/*/repo`[cite: 2, 4]. Code leaves a container solely as a single `git bundle` (`out.bundle`), verified with `git bundle verify` and fetched with `transfer.fsckObjects=true` into a gateway-owned quarantine repo via `gitexec.Run`[cite: 2, 4].
9. **Default-Deny Egress:** Session containers sit on an internal Docker network (`agent-session-net`) with no direct internet route[cite: 2, 4]. Outbound build traffic goes through `egress-proxy:3128` (allowlist + DNS rebinding guard) and research traffic goes through `egress-proxy:3129`[cite: 2, 4].
10. **Skills & Memory Are Context, Never Authority:** Pinned `SKILL.md` files and recalled `<memory_notes>` are untrusted/contextual data[cite: 2, 4]. They cannot expand task scope, bypass protected paths, or override these 10 rules[cite: 2, 4].

---

## 2. Infrastructure Topology & Resource Split

* **Oracle ARM64 VM (2 OCPU / 12 GB RAM — Single VM):** Runs `gateway`, `router`, `egress-proxy`, `keepalive` (maintaining ~4 GB resident floor and yielding in 64 MB steps), `caddy` (production TLS), and one task/verify container at a time (`2.5 GB` cap)[cite: 2, 3, 4]. Do not split into multiple A1 VMs; 2 OCPU / 12 GB is the full free Arm pool[cite: 3, 4].
* **Supabase Postgres (Free Tier — 500 MB Hard Limit):** Single source of truth[cite: 2, 4]. All tables live in private schema `agent` with RLS enabled, zero policies, and `anon`/`authenticated` privileges revoked[cite: 2, 4]. Dedicated DB roles: `agent_gateway` (CRUD on `agent.*`) and `agent_router` (insert-only on `agent.llm_usage`)[cite: 2, 4]. Alert at 300 MB; prune events >30d and usage >90d[cite: 2, 4].
* **Vercel (Free Tier):** Hosts `web/` (Next.js App Router, strict TypeScript, Tailwind)[cite: 2, 4]. Connects directly to the gateway for REST and SSE (`Last-Event-ID` resume)[cite: 2, 4].
* **Render Free Tier (Optional Stateless Helper Only):** Because Render Free instances have 512 MB RAM and sleep after 15 minutes of inactivity, **never** host `gateway`, `router`, SSE streams, or stateful workers on Render. Use Render Free only as a stateless `/healthz` ping relay or external status check if desired.
* **Localhost Mode (M0–M7 Development):** Services bind to `127.0.0.1` (`gateway :8080`, `router :8090`, `egress-proxy :3128-:3130`, `web :3000`) against your Supabase project and local Docker daemon[cite: 2, 4]. Production cookie domain (`COOKIE_DOMAIN`) and Caddy TLS activate when moving from `localhost` to a custom domain[cite: 2, 4].
* **Session Network & Router Alias:** `agent-session-net` remains `internal=true`; no direct egress is opened. In the local host-bridge runtime, `PADI_ROUTER_HOST_IP=172.20.0.1` makes the gateway add Docker `ExtraHosts` mapping `router:172.20.0.1` to each session container[cite: update.md §29]. Compose deployments instead run a real `router` container on `agent-session-net`, so containers resolve `router:8090` through Docker DNS; no host-IP override is needed there[cite: deploy/docker-compose.yml].

---

## 3. Spec Reconciliation & Mandatory Fixes

1. **Protected Paths (`README.md` §5.3 vs `BUILD_SPEC.md` §4):** `README.md` requires approval when lockfiles (`go.sum`, `package-lock.json`, `pnpm-lock.yaml`, `yarn.lock`, `Cargo.lock`, `poetry.lock`) change without a matching manifest (`go.mod`, `package.json`, `Cargo.toml`, `pyproject.toml`) change[cite: 4]. Implement this check in `internal/gateway/approvals` and `internal/gateway/contract` during M3b[cite: 2].
2. **Concurrency Default:** `max_sessions` is `1` (`BUILD_SPEC.md` §4 & `README.md` D19), superseding earlier D14 (`2`)[cite: 2, 4].
3. **Zero Python on VM:** All backend services, CLIs, and backup scripts use Go 1.26 or POSIX shell + static ARM64 binaries (`pg_dump`, S3-compatible CLI/Go helper, `gitleaks`, `osv-scanner`)—never Python `oci-cli` on the host[cite: 2, 4].
4. **Canonical Filenames:**
   * `docs/decisions.md` is the canonical living decision log and `VERIFY` evidence table[cite: 2, 4]; `docs/decision-record-v2.md` is the historical owner record[cite: 2, 3].
   * `scripts/skills-verify.sh` (wrapping `cmd/skills-verify`) is the canonical skill verification script (built in M12, not M0)[cite: 2].
5. **Implemented M4 and Later Local Fixes:** M0–M4 are implemented locally, including the M4 §2.5 report. Post-M4 work recorded in `update.md` §§23–29 includes trust/router hardening, approval-callback repair, `/options` rolling live trace with durable `agent.events` history, 15 s heartbeat with 6-retry transport-failure stop, same-session think/plan/execute role handoffs with the owner plan→execute gate, and the host-bridge router alias fix. The in-app push/PR gate is still owner-gated: no genuine owner Telegram approval taps for publishing, a git branch push is owner-authorized, and no draft PR exists[cite: update.md §23–29].

---

## 4. Coding, Testing & CI Standards

* **Language & Dependencies:** Go 1.26, standard library first[cite: 2, 4]. Only third-party Go modules allowed by `BUILD_SPEC.md` §0.6: `pgx/v5`, `yaml.v3`, `google/uuid`, `docker/docker`, `oapi-codegen`, `golang-jwt/jwt/v5`, and `golang.org/x/net/html`[cite: 2].
* **Go Conventions:** `context.Context` first parameter on all I/O functions; wrap errors with `fmt.Errorf("...: %w", err)`; zero package-level mutable globals; consumer-defined interfaces; structured `log/slog` JSON logs with secret redaction (`ghs_`, `ghp_`, `sk-`, `AIza`, `Bearer `)[cite: 2].
* **Milestone Gate (Stop & Report):** Build strictly in order (`M0 -> M1 -> M2 -> M3a -> M3b -> M4 -> M5 -> M6 -> M7 -> M8 -> M9 -> M10 -> M12..M16 -> M11`)[cite: 2]. Create only the files listed for the active milestone[cite: 2]. Never start the next milestone until `go vet ./... && go test -race ./...` passes and the human mentor approves the milestone report[cite: 2]. Status at the 2026-11-27 revision: M0–M4 are implemented locally and the M4 §2.5 report is complete; the later fixes above are documented but do not authorize M5. Do not start M5 until the owner clears the pending push/PR gate and gives an explicit go-ahead[cite: update.md §23–29].
